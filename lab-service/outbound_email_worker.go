package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ---- Воркер очереди писем заявки (2026-10-19) ----
// Дизайн: docs/superpowers/specs/2026-10-19-sbe-lims-outbound-email-queue-design.md.
// Приём тот же, что у startTelegramNotifyJob/upload_buffer.go: тикер + канал-
// «толчок» от постановки в очередь; строки pending переживают перезапуск
// контейнера. Отличие от Telegram: вложение (отчёт) собирается ОДИН раз на
// заявку, даже если писем два (заказчику + дубль в трекер).

const (
	outboundEmailInterval = 30 * time.Second
	outboundEmailBatch    = 20
)

type outboundEmailRow struct {
	id             int64
	requestID      int64
	recipientType  string
	address        string
	withAttachment bool
}

// startOutboundEmailJob — фоновая отправка очереди. Один заход сразу при старте:
// после рестарта в очереди могли остаться неотправленные строки.
func (s *Server) startOutboundEmailJob() {
	go func() {
		ticker := time.NewTicker(outboundEmailInterval)
		defer ticker.Stop()
		s.runOutboundEmailOnce(context.Background())
		for {
			select {
			case <-ticker.C:
				s.runOutboundEmailOnce(context.Background())
			case <-s.outboundWake:
				s.runOutboundEmailOnce(context.Background())
			}
		}
	}()
}

// runOutboundEmailOnce — один заход: берём пачку pending, группируем по заявке,
// собираем вложение один раз, шлём.
func (s *Server) runOutboundEmailOnce(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `
SELECT id, request_id, recipient_type, recipient_address, with_attachment
FROM sent_emails WHERE status = 'pending'
ORDER BY id LIMIT $1`, outboundEmailBatch)
	if err != nil {
		log.Printf("outbound email worker: select: %v", err)
		return
	}
	items := make([]outboundEmailRow, 0, outboundEmailBatch)
	order := make([]int64, 0, outboundEmailBatch)
	byRequest := map[int64][]outboundEmailRow{}
	for rows.Next() {
		var it outboundEmailRow
		if err := rows.Scan(&it.id, &it.requestID, &it.recipientType, &it.address, &it.withAttachment); err != nil {
			continue
		}
		if _, seen := byRequest[it.requestID]; !seen {
			order = append(order, it.requestID)
		}
		byRequest[it.requestID] = append(byRequest[it.requestID], it)
		items = append(items, it)
	}
	rows.Close()
	if len(items) == 0 {
		return
	}

	for _, requestID := range order {
		group := byRequest[requestID]
		req, err := s.loadRequest(ctx, requestID)
		if err != nil {
			log.Printf("outbound email worker: load request %d: %v", requestID, err)
			for _, it := range group {
				s.markOutboundAttemptFailed(ctx, it.id, fmt.Sprintf("заявка не найдена: %v", err))
			}
			continue
		}
		needAttachment := false
		for _, it := range group {
			if it.withAttachment {
				needAttachment = true
				break
			}
		}
		var attachment *emailAttachment
		var buildErr error
		if needAttachment {
			attachment = s.completionAttachment(ctx, requestID)
			if attachment == nil {
				buildErr = fmt.Errorf("не удалось собрать вложение")
			}
		}
		for _, it := range group {
			s.sendOutboundEmailRow(ctx, it, req, attachment, buildErr)
		}
	}
}

// sendOutboundEmailRow — одна строка очереди. Ошибка сборки вложения считается
// попыткой (иначе строка висела бы в очереди вечно) и видна в журнале.
func (s *Server) sendOutboundEmailRow(ctx context.Context, it outboundEmailRow, req *Request, attachment *emailAttachment, buildErr error) {
	if it.withAttachment && buildErr != nil {
		s.markOutboundAttemptFailed(ctx, it.id, buildErr.Error())
		log.Printf("outbound email worker: request %d: %v", it.requestID, buildErr)
		return
	}
	var sendAttachment *emailAttachment
	if it.withAttachment {
		sendAttachment = attachment
	}
	subject, body := outboundPayload(*req, it.recipientType)
	err := sendMailWithAttachment(it.address, subject, body, sendAttachment)
	if err != nil {
		s.markOutboundAttemptFailed(ctx, it.id, err.Error())
		log.Printf("outbound email worker: request %d type %s to %s: %v", it.requestID, it.recipientType, it.address, err)
		return
	}
	if _, dbErr := s.pool.Exec(ctx, `
UPDATE sent_emails
SET status = 'sent', sent_at = now(), success = true, error = '', attempts = attempts + 1
WHERE id = $1`, it.id); dbErr != nil {
		log.Printf("outbound email worker: mark sent %d: %v", it.id, dbErr)
	}
}

// markOutboundAttemptFailed — attempts++, last_error; на пятой попытке строка
// становится failed (см. nextOutboundStatus).
func (s *Server) markOutboundAttemptFailed(ctx context.Context, id int64, errMsg string) {
	var attempts int
	if err := s.pool.QueryRow(ctx, `SELECT attempts FROM sent_emails WHERE id = $1`, id).Scan(&attempts); err != nil {
		log.Printf("outbound email worker: load attempts %d: %v", id, err)
		return
	}
	if _, err := s.pool.Exec(ctx, `
UPDATE sent_emails
SET attempts = attempts + 1, status = $2, success = false, last_error = $3, error = $3
WHERE id = $1`, id, nextOutboundStatus(attempts), errMsg); err != nil {
		log.Printf("outbound email worker: mark error %d: %v", id, err)
	}
}
