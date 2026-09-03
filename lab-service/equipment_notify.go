package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- Оповещения о приближении срока поверки оборудования (2026-09-03) ----
//
// По образцу documents_notify_settings/documents_notifications в
// sbe-documents/documents-service/notify.go: единые настройки на весь модуль
// (получатели+пороги в днях, не по каждой единице оборудования — у Equipment нет
// поля-email, а responsible — свободный текст), тикер раз в 6ч, дедуп по
// (equipment_id, day, email), письмо через уже готовый sendMailWithAttachment
// (outbound_email.go, те же LAB_MAIL_*/LAB_SMTP_* креды, что и остальная почта ЛИМС).

type EquipmentNotifySettings struct {
	Enabled    bool   `json:"enabled"`
	Days       []int  `json:"days"`
	Recipients string `json:"recipients"`
}

func parseEquipmentNotifyDays(s string) []int {
	parts := strings.Split(s, ",")
	seen := map[int]bool{}
	var out []int
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func joinEquipmentNotifyDays(days []int) string {
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

func (s *Server) handleGetEquipmentNotifySettings(w http.ResponseWriter, r *http.Request) {
	var enabled bool
	var days, recipients string
	err := s.pool.QueryRow(r.Context(),
		`SELECT enabled, days, recipients FROM equipment_notify_settings WHERE id = 1`).Scan(&enabled, &days, &recipients)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "days": parseEquipmentNotifyDays(days), "recipients": recipients})
}

func (s *Server) handleSetEquipmentNotifySettings(w http.ResponseWriter, r *http.Request) {
	var req EquipmentNotifySettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	days := joinEquipmentNotifyDays(parseEquipmentNotifyDays(joinEquipmentNotifyDays(req.Days)))
	_, err := s.pool.Exec(r.Context(),
		`UPDATE equipment_notify_settings SET enabled = $1, days = $2, recipients = $3 WHERE id = 1`,
		req.Enabled, days, strings.TrimSpace(req.Recipients))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// startEquipmentNotifyJob запускает фоновую проверку приближающегося срока
// поверки оборудования (на старте и далее раз в 6 ч).
func (s *Server) startEquipmentNotifyJob() {
	go func() {
		s.checkEquipmentNotifications()
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			s.checkEquipmentNotifications()
		}
	}()
}

// checkEquipmentNotifications отправляет письма настроенным получателям об
// оборудовании, у которого verification_expiry_date приближается — по каждому
// настроенному порогу в днях, не чаще одного письма на (оборудование, порог, адрес).
func (s *Server) checkEquipmentNotifications() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var enabled bool
	var daysStr, recipientsStr string
	if err := s.pool.QueryRow(ctx,
		`SELECT enabled, days, recipients FROM equipment_notify_settings WHERE id = 1`).Scan(&enabled, &daysStr, &recipientsStr); err != nil {
		log.Printf("equipment notify: settings read: %v", err)
		return
	}
	if !enabled {
		return
	}
	days := parseEquipmentNotifyDays(daysStr)
	var recipients []string
	for _, r := range strings.Split(recipientsStr, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			recipients = append(recipients, r)
		}
	}
	if len(days) == 0 || len(recipients) == 0 {
		return
	}

	now := time.Now().UTC()
	notified := 0
	for _, day := range days {
		nowDate := now.Format("2006-01-02")
		windowEnd := now.AddDate(0, 0, day).Format("2006-01-02")
		rows, err := s.pool.Query(ctx, `
SELECT id, code, name, verification_expiry_date
FROM equipment
WHERE verification_expiry_date IS NOT NULL
	AND verification_expiry_date > $1::date AND verification_expiry_date <= $2::date
ORDER BY id`, nowDate, windowEnd)
		if err != nil {
			log.Printf("equipment notify: query day=%d: %v", day, err)
			continue
		}
		type row struct {
			id         int64
			code, name string
			expiry     time.Time
		}
		var found []row
		for rows.Next() {
			var it row
			if err := rows.Scan(&it.id, &it.code, &it.name, &it.expiry); err != nil {
				log.Printf("equipment notify: scan: %v", err)
				continue
			}
			found = append(found, it)
		}
		rows.Close()

		for _, it := range found {
			daysLeft := int(time.Until(it.expiry).Hours() / 24)
			if daysLeft < 0 {
				daysLeft = 0
			}
			for _, email := range recipients {
				var exists bool
				if err := s.pool.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM equipment_notifications WHERE equipment_id = $1 AND day = $2 AND email = $3)`,
					it.id, day, email).Scan(&exists); err != nil {
					log.Printf("equipment notify: check: %v", err)
					continue
				}
				if exists {
					continue
				}
				subject := "Приближается срок поверки оборудования"
				body := fmt.Sprintf(
					"Здравствуйте!\n\n"+
						"Срок действия поверки оборудования скоро истекает:\n\n"+
						"Оборудование: %s — %s\n"+
						"Действует до: %s\n"+
						"Осталось дней: %d\n\n"+
						"Пожалуйста, проверьте оборудование и при необходимости проведите поверку.\n\n"+
						"— Служебные уведомления, отвечать не нужно.",
					it.code, it.name, it.expiry.Format("02.01.2006"), daysLeft)
				if err := sendMailWithAttachment(email, subject, body, nil); err != nil {
					log.Printf("equipment notify: email to %s (equipment %d, day %d): %v", email, it.id, day, err)
					continue
				}
				if _, err := s.pool.Exec(ctx, `
INSERT INTO equipment_notifications (equipment_id, day, email)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, it.id, day, email); err != nil {
					log.Printf("equipment notify: record: %v", err)
				}
				notified++
			}
		}
	}
	if notified > 0 {
		log.Printf("equipment notify: отправлено уведомлений: %d", notified)
	}
}
