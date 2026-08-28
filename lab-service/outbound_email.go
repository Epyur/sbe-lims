package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---- WP2 (2026-08-28): исходящая почта — результаты заказчику + дубль в LPITrack ----
// См. docs/superpowers/specs/2026-08-28-sbe-lims-outbound-email-design.md.
//
// Отправка использует тот же exim-relay (localhost:25, без пароля, STARTTLS с
// самоподписанным сертификатом), что и sbe-core/auth-service/email.go sendMail — но
// расширена под MIME multipart/mixed (вложение docx-протокола) и под собственные
// LAB_SMTP_* переменные (не LAB_MAIL_* — те читают IMAP-ящик LPITrack, к отправке
// отношения не имеют).

type emailAttachment struct {
	Name string
	Data []byte
}

// SentEmail — одна строка журнала sent_emails.
type SentEmail struct {
	ID               int64  `json:"id"`
	RequestID        int64  `json:"request_id"`
	RecipientType    string `json:"recipient_type"` // "customer" | "lpitrack"
	RecipientAddress string `json:"recipient_address"`
	SentAt           string `json:"sent_at"`
	Success          bool   `json:"success"`
	Error            string `json:"error"`
	TriggeredBy      string `json:"triggered_by"` // "auto" | "manual"
}

// shouldAutoSend — чистая функция (без БД/сети, юнит-тестируется отдельно в
// outbound_email_test.go): решает, какие письма слать при АВТОТРИГГЕРЕ (переход заявки
// в completed). Ручная кнопка (handleSendRequestEmail) эту функцию не использует — шлёт
// оба применимых письма безусловно, это явное действие пользователя.
func shouldAutoSend(labAutoSend, hasExternalID, alreadySentCustomer, alreadySentLpitrack bool) (sendCustomer, sendLpitrack bool) {
	if !labAutoSend {
		return false, false
	}
	sendCustomer = !alreadySentCustomer
	sendLpitrack = hasExternalID && !alreadySentLpitrack
	return sendCustomer, sendLpitrack
}

// triggerCompletionEmails — вызывается из handleSetRequestStatus/handleKanbanMove СРАЗУ
// ПОСЛЕ успешного перехода заявки в status="completed" (вызывающий код уже проверяет,
// что это настоящий переход, не повторное сохранение уже completed-заявки). Best-effort:
// ошибка отправки не должна ломать сам переход статуса — только пишется в лог/журнал.
func (s *Server) triggerCompletionEmails(ctx context.Context, requestID int64) {
	req, err := s.loadRequest(ctx, requestID)
	if err != nil {
		log.Printf("triggerCompletionEmails: load request %d: %v", requestID, err)
		return
	}
	if req.LabID <= 0 {
		return
	}
	var labAutoSend bool
	if err := s.pool.QueryRow(ctx, `SELECT auto_send_email FROM labs WHERE id = $1`, req.LabID).Scan(&labAutoSend); err != nil {
		log.Printf("triggerCompletionEmails: load lab %d: %v", req.LabID, err)
		return
	}
	if !labAutoSend {
		return
	}
	sendCustomer, sendLpitrack := shouldAutoSend(labAutoSend, req.ExternalID != "",
		s.hasSentEmail(ctx, requestID, "customer"), s.hasSentEmail(ctx, requestID, "lpitrack"))
	s.sendCompletionEmails(ctx, *req, sendCustomer, sendLpitrack, "auto")
}

func (s *Server) hasSentEmail(ctx context.Context, requestID int64, recipientType string) bool {
	var exists bool
	_ = s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM sent_emails WHERE request_id = $1 AND recipient_type = $2 AND success = true)`,
		requestID, recipientType).Scan(&exists)
	return exists
}

// sendCompletionEmails строит протокол ОДИН раз (даже если шлём оба письма), затем шлёт
// то, что запрошено, и пишет каждую попытку в журнал (успех и неудачу одинаково —
// видимость важнее, см. спеку).
// customerEmailContent — тема/тело письма заказчику. Живая жалоба пользователя
// (2026-08-28, после первого реального теста): если у заявки есть external_id
// (заявка переходного периода, пришедшая через легаси LPITrack), заказчик узнаёт её
// именно по этому номеру — письмо должно явно называть его, а не только внутренний
// номер ЛИМС (CustomerNumber), который заказчику ни о чём не говорит. Оба номера
// показываются вместе, чтобы не терялась прослеживаемость внутри ЛИМС.
func customerEmailContent(req Request) (subject, body string) {
	if req.ExternalID != "" {
		subject = fmt.Sprintf("Результаты испытания №%s", req.ExternalID)
		body = fmt.Sprintf(
			"Результаты испытания по заявке №%s (учётный номер в ЛИМС системе — %s) во вложении.",
			req.ExternalID, req.CustomerNumber)
		return subject, body
	}
	subject = fmt.Sprintf("Результаты испытания №%s", req.CustomerNumber)
	body = fmt.Sprintf("Результаты испытания по заявке №%s во вложении.", req.CustomerNumber)
	return subject, body
}

func (s *Server) sendCompletionEmails(ctx context.Context, req Request, sendCustomer, sendLpitrack bool, triggeredBy string) {
	if !sendCustomer && !sendLpitrack {
		return
	}
	p, err := s.buildProtocol(ctx, req.ID, "protocol")
	if err != nil {
		log.Printf("sendCompletionEmails: build protocol %d: %v", req.ID, err)
		return
	}
	docxBytes, err := s.protocolDocx(ctx, p, "protocol")
	if err != nil {
		log.Printf("sendCompletionEmails: protocol docx %d: %v", req.ID, err)
		return
	}
	attachment := &emailAttachment{Name: fmt.Sprintf("protocol-%d.docx", req.ID), Data: docxBytes}

	if sendCustomer {
		if req.OwnerEmail == "" {
			log.Printf("sendCompletionEmails: request %d has no owner_email, skip customer", req.ID)
		} else {
			subject, body := customerEmailContent(req)
			s.sendAndLog(ctx, req.ID, "customer", req.OwnerEmail, triggeredBy, attachment, subject, body)
		}
	}
	if sendLpitrack {
		lpitrackAddr := strings.TrimSpace(os.Getenv("LAB_LPITRACK_EMAIL"))
		if lpitrackAddr == "" {
			log.Printf("sendCompletionEmails: LAB_LPITRACK_EMAIL not set, skip lpitrack dup for request %d", req.ID)
		} else {
			s.sendAndLog(ctx, req.ID, "lpitrack", lpitrackAddr, triggeredBy, attachment,
				fmt.Sprintf("external_id=%s", req.ExternalID),
				fmt.Sprintf("Заявка №%s (external_id=%s) завершена.", req.CustomerNumber, req.ExternalID))
		}
	}
}

func (s *Server) sendAndLog(ctx context.Context, requestID int64, recipientType, to, triggeredBy string,
	attachment *emailAttachment, subject, body string) {
	err := sendMailWithAttachment(to, subject, body, attachment)
	success := err == nil
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
		log.Printf("sendAndLog: request=%d type=%s to=%s: %v", requestID, recipientType, to, err)
	}
	if _, dbErr := s.pool.Exec(ctx, `
INSERT INTO sent_emails (request_id, recipient_type, recipient_address, success, error, triggered_by)
VALUES ($1, $2, $3, $4, $5, $6)`,
		requestID, recipientType, to, success, errMsg, triggeredBy); dbErr != nil {
		log.Printf("sendAndLog: journal insert: %v", dbErr)
	}
}

// handleSendRequestEmail — POST /requests/{id}/send-email (ручная кнопка в карточке
// заявки). В отличие от triggerCompletionEmails — БЕЗ проверки labs.auto_send_email и
// БЕЗ проверки журнала на дубли (явное действие пользователя): шлёт заказчику всегда,
// дубль в LPITrack — если есть external_id.
func (s *Server) handleSendRequestEmail(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabAccess(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	req, err := s.loadRequest(r.Context(), requestID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	s.sendCompletionEmails(r.Context(), *req, true, req.ExternalID != "", "manual")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListSentEmails — GET /requests/{id}/sent-emails: журнал заявки для отображения
// в её карточке (кому/когда/авто или вручную/успех).
func (s *Server) handleListSentEmails(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
SELECT id, request_id, recipient_type, recipient_address, sent_at, success, error, triggered_by
FROM sent_emails WHERE request_id = $1 ORDER BY sent_at DESC`, requestID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]SentEmail, 0, 8)
	for rows.Next() {
		var it SentEmail
		var sentAt time.Time
		if err := rows.Scan(&it.ID, &it.RequestID, &it.RecipientType, &it.RecipientAddress,
			&sentAt, &it.Success, &it.Error, &it.TriggeredBy); err != nil {
			continue
		}
		it.SentAt = sentAt.Format(time.RFC3339)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"emails": out})
}

// ---- SMTP-транспорт ----

// sendMailWithAttachment — тот же паттерн, что sbe-core/auth-service/email.go sendMail
// (net/smtp, локальный exim-релей, STARTTLS с самоподписанным сертификатом), расширенный
// под MIME multipart/mixed (текстовое тело + опциональное бинарное вложение). Тема письма
// кодируется по RFC 2047 (Q-encoding) — кириллица в Subject без этого не гарантированно
// корректно отображается почтовыми клиентами (в отличие от тела, где charset=utf-8 в
// Content-Type достаточно).
func sendMailWithAttachment(to, subject, body string, attachment *emailAttachment) error {
	host := os.Getenv("LAB_SMTP_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("LAB_SMTP_PORT")
	if port == "" {
		port = "25"
	}
	from := os.Getenv("LAB_SMTP_FROM")
	if from == "" {
		from = "noreply@epyur.fvds.ru"
	}

	encodedSubject := mime.QEncoding.Encode("UTF-8", subject)

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n", from, to, encodedSubject)
	if attachment == nil {
		fmt.Fprintf(&msg, "Content-Type: text/plain; charset=utf-8\r\n\r\n%s", body)
	} else {
		boundary := fmt.Sprintf("sbe-lab-%x", time.Now().UnixNano())
		fmt.Fprintf(&msg, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
		fmt.Fprintf(&msg, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n\r\n", boundary, body)
		fmt.Fprintf(&msg, "--%s\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			boundary, attachment.Name, base64WrapLines(attachment.Data))
		fmt.Fprintf(&msg, "--%s--\r\n", boundary)
	}

	addr := fmt.Sprintf("%s:%s", host, port)
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Hello("lab-service"); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		skipVerify := os.Getenv("LAB_SMTP_SKIP_VERIFY") == "1"
		if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: skipVerify}); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// base64WrapLines — MIME требует переносить base64-тело вложения по 76 символов в строке.
func base64WrapLines(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		b.WriteString(encoded[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}
