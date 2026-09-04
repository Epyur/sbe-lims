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
// sbe-documents/documents-service/notify.go: тикер раз в 6ч, дедуп по
// (equipment_id, day, email), письмо через уже готовый sendMailWithAttachment
// (outbound_email.go, те же LAB_MAIL_*/LAB_SMTP_* креды, что и остальная почта ЛИМС).
//
// Per-lab (2026-09-04, по прямому запросу пользователя — «у каждой лаборатории
// могут быть свои сроки и свои получатели уведомлений»): equipment_notify_settings
// теперь одна строка НА ЛАБУ (lab_id), не единая строка на весь модуль. lab_id=0 —
// зарезервированный "общий" ряд — fallback для (а) оборудования без
// equipment.lab_id и (б) любой лабы, не задавшей свой override. См. миграцию в
// main.go.

type EquipmentNotifySettings struct {
	// LabID — лаба, которой принадлежит этот ряд (0 — общий/дефолтный).
	LabID int64 `json:"lab_id"`
	// Configured — true, если для запрошенного lab_id есть СВОЙ ряд (не fallback на
	// общий). Только для GET-ответа, см. handleGetEquipmentNotifySettings.
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	Days       []int  `json:"days"`
	Recipients string `json:"recipients"`
	// Reset (2026-09-04, POST-запрос only) — вернуть лабу к общим настройкам вместо
	// upsert'а: удаляет её override-ряд. См. handleSetEquipmentNotifySettings.
	Reset bool `json:"reset"`
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

// handleGetEquipmentNotifySettings — GET ?lab_id=N (отсутствует/некорректно → 0,
// общий ряд). Авторизация — только вопрос ПРОСМОТРА: app-admin+ либо lab_admin
// ХОТЯ БЫ ОДНОЙ лабы вообще (requireAnyLabAdmin, не обязательно именно lab_id) —
// реальная граница на ЗАПИСЬ проверяется в handleSetEquipmentNotifySettings.
// Маршрут заведён на "viewer" (main.go), эта проверка — внутренняя.
func (s *Server) handleGetEquipmentNotifySettings(w http.ResponseWriter, r *http.Request) {
	if ok, err := s.requireAnyLabAdmin(r.Context(), currentEmail(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	} else if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer a lab"})
		return
	}
	labID, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("lab_id")), 10, 64)
	if labID < 0 {
		labID = 0
	}
	var enabled bool
	var days, recipients string
	err := s.pool.QueryRow(r.Context(),
		`SELECT enabled, days, recipients FROM equipment_notify_settings WHERE lab_id = $1`, labID).Scan(&enabled, &days, &recipients)
	configured := true
	if err != nil {
		if labID == 0 {
			// Не должно происходить — общий ряд (lab_id=0) всегда есть, см. seed-insert
			// в main.go, но на всякий случай не падаем молча.
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		// Эта лаба ещё не задавала свои настройки — fallback на общий ряд (2026-09-04).
		configured = false
		if fbErr := s.pool.QueryRow(r.Context(),
			`SELECT enabled, days, recipients FROM equipment_notify_settings WHERE lab_id = 0`).Scan(&enabled, &days, &recipients); fbErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lab_id": labID, "configured": configured,
		"enabled": enabled, "days": parseEquipmentNotifyDays(days), "recipients": recipients,
	})
}

// handleSetEquipmentNotifySettings — POST {lab_id, enabled, days, recipients,
// reset}. Граница доступа на ЗАПИСЬ (2026-09-04, в отличие от GET) зависит от
// lab_id: 0 (общий ряд) — только app-admin+ (не lab_admin — общий/дефолтный
// конфиг не принадлежит никакой конкретной лабе); >0 — requireLabAdminOf(lab_id)
// (app-admin+ либо lab_admin ИМЕННО этой лабы, тот же паттерн, что
// handleUpdateLab в references.go) — lab_admin ЧУЖОЙ лабы получает 403.
func (s *Server) handleSetEquipmentNotifySettings(w http.ResponseWriter, r *http.Request) {
	var req EquipmentNotifySettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	email := currentEmail(r)
	if req.LabID == 0 {
		role, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if roleRank(role) < roleRank("admin") {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: общие настройки меняет только администратор"})
			return
		}
	} else {
		if ok, err := s.requireLabAdminOf(r.Context(), email, req.LabID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		} else if !ok {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer this lab"})
			return
		}
	}
	// Reset (2026-09-04) — вернуть лабу к общим настройкам (удалить её override).
	// Для lab_id=0 бессмысленно (общий ряд удалить нельзя — на нём и держится
	// fallback остальных) — молча игнорируем, а не 400: клиент никогда не должен
	// присылать reset вместе с lab_id=0, но не стоит на этом ломаться.
	if req.Reset && req.LabID > 0 {
		if _, err := s.pool.Exec(r.Context(),
			`DELETE FROM equipment_notify_settings WHERE lab_id = $1`, req.LabID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	days := joinEquipmentNotifyDays(parseEquipmentNotifyDays(joinEquipmentNotifyDays(req.Days)))
	_, err := s.pool.Exec(r.Context(), `
INSERT INTO equipment_notify_settings (lab_id, enabled, days, recipients) VALUES ($1, $2, $3, $4)
ON CONFLICT (lab_id) DO UPDATE SET enabled = EXCLUDED.enabled, days = EXCLUDED.days, recipients = EXCLUDED.recipients`,
		req.LabID, req.Enabled, days, strings.TrimSpace(req.Recipients))
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
// настроенному ДЛЯ ЭТОГО ОБОРУДОВАНИЯ порогу в днях, не чаще одного письма на
// (оборудование, порог, адрес). Per-lab (2026-09-04): у каждого оборудования свой
// эффективный набор days/recipients/enabled, резолвится через его lab_id (см.
// resolve ниже) — вместо единого набора на весь модуль, как было раньше.
func (s *Server) checkEquipmentNotifications() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. Все настройки одним запросом — не по одной строке на день/лабу.
	settingsRows, err := s.pool.Query(ctx,
		`SELECT lab_id, enabled, days, recipients FROM equipment_notify_settings`)
	if err != nil {
		log.Printf("equipment notify: settings read: %v", err)
		return
	}
	settingsByLab := map[int64]EquipmentNotifySettings{}
	for settingsRows.Next() {
		var it EquipmentNotifySettings
		var daysStr string
		if err := settingsRows.Scan(&it.LabID, &it.Enabled, &daysStr, &it.Recipients); err != nil {
			log.Printf("equipment notify: settings scan: %v", err)
			continue
		}
		it.Days = parseEquipmentNotifyDays(daysStr)
		settingsByLab[it.LabID] = it
	}
	settingsRows.Close()

	// 2. Общий (fallback) ряд — должен существовать всегда (seed-insert в main.go).
	general, ok := settingsByLab[0]
	if !ok {
		log.Printf("equipment notify: общий ряд (lab_id=0) не найден — пропуск проверки")
		return
	}

	// 3. resolve — эффективные настройки конкретного оборудования: свой override
	// лабы, если задан, иначе общий ряд.
	resolve := func(labID *int64) EquipmentNotifySettings {
		if labID != nil {
			if v, ok := settingsByLab[*labID]; ok {
				return v
			}
		}
		return general
	}

	// 4. Оборудование одним запросом (не по одному на каждый день порога, как раньше).
	rows, err := s.pool.Query(ctx, `
SELECT id, code, name, lab_id, verification_expiry_date
FROM equipment
WHERE verification_expiry_date IS NOT NULL AND verification_expiry_date > CURRENT_DATE
ORDER BY id`)
	if err != nil {
		log.Printf("equipment notify: query equipment: %v", err)
		return
	}
	type row struct {
		id         int64
		code, name string
		labID      *int64
		expiry     time.Time
	}
	var found []row
	for rows.Next() {
		var it row
		if err := rows.Scan(&it.id, &it.code, &it.name, &it.labID, &it.expiry); err != nil {
			log.Printf("equipment notify: scan: %v", err)
			continue
		}
		found = append(found, it)
	}
	rows.Close()

	notified := 0
	for _, it := range found {
		settings := resolve(it.labID)
		if !settings.Enabled {
			continue
		}
		var recipients []string
		for _, r := range strings.Split(settings.Recipients, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				recipients = append(recipients, r)
			}
		}
		if len(settings.Days) == 0 || len(recipients) == 0 {
			continue
		}
		daysLeft := int(time.Until(it.expiry).Hours() / 24)
		if daysLeft < 0 {
			daysLeft = 0
		}
		// Каскад по порогам сохранён как раньше (2026-09-03): оборудование может
		// попасть сразу в НЕСКОЛЬКО настроенных порогов (напр. days=[30,14,7],
		// daysLeft=10 → совпадает и с 30, и с 14, не только с 7) — эквивалент
		// прежнего SQL-окна "> now AND <= now+day", т.е. daysLeft <= day. Дедуп по
		// (equipment_id, day, email) в equipment_notifications не даёт повторно
		// отправить один и тот же порог одному адресату.
		for _, day := range settings.Days {
			if daysLeft > day {
				continue
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
