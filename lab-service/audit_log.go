package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

// ---- WP8 (2026-08-29): журнал изменений заявки/результатов ----
// См. docs/superpowers/specs/2026-08-29-sbe-lims-audit-log-design.md.
//
// Узкий охват (решение пользователя): смена статуса заявки (requests.status) +
// создание/правка серий результатов (measurement_results) — полный снимок values
// до/после, не per-field diff. Только уровень приложения: каждая точка записи
// (handleSetRequestStatus/handleKanbanMove/sync push, saveResultSeries) явно
// вставляет строку журнала СРАЗУ ПОСЛЕ своего успешного UPDATE/INSERT — прямые
// SQL-скрипты в журнал не попадают (см. границы спеки). Best-effort — ошибка
// записи в журнал не должна ломать основное действие, только логируется.

// AuditLogEntry — одна строка журнала.
type AuditLogEntry struct {
	ID           int64  `json:"id"`
	RequestID    int64  `json:"request_id"`
	Kind         string `json:"kind"` // "status" | "result_created" | "result_updated"
	Who          string `json:"who"`
	CreatedAt    string `json:"created_at"`
	OldStatus    string `json:"old_status,omitempty"`
	NewStatus    string `json:"new_status,omitempty"`
	MethodID     int64  `json:"method_id,omitempty"`
	SeriesNum    int    `json:"series_num,omitempty"`
	ValuesBefore any    `json:"values_before,omitempty"`
	ValuesAfter  any    `json:"values_after,omitempty"`
}

// shouldLogStatusChange — чистая функция (юнит-тестируется без БД, см. shouldAutoSend
// в outbound_email.go — тот же принцип): пишем строку журнала, только если это
// РЕАЛЬНЫЙ переход, не повторное сохранение того же значения статуса.
func shouldLogStatusChange(oldStatus, newStatus string) bool {
	return oldStatus != newStatus
}

// logStatusChange — вызывается из handleSetRequestStatus/handleKanbanMove/sync push
// СРАЗУ ПОСЛЕ успешного UPDATE requests SET status=... .
func (s *Server) logStatusChange(ctx context.Context, requestID int64, who, oldStatus, newStatus string) {
	if !shouldLogStatusChange(oldStatus, newStatus) {
		return
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO request_audit_log (request_id, kind, who, old_status, new_status)
VALUES ($1, 'status', $2, $3, $4)`,
		requestID, who, oldStatus, newStatus); err != nil {
		log.Printf("logStatusChange: request=%d: %v", requestID, err)
	}
}

// resultSaveKind — чистая функция: before == nil → новая серия, иначе — правка
// существующей. Юнит-тестируется без БД.
func resultSaveKind(before map[string]any) string {
	if before == nil {
		return "result_created"
	}
	return "result_updated"
}

// logResultSave — вызывается из saveResultSeries/recalcRequestMethod СРАЗУ ПОСЛЕ
// успешного апсерта серии. who == "" → не логировать вовсе (recalc-all, CLI без
// HTTP-сессии/пользователя — системный пересчёт, не действие человека, см. границы
// спеки WP8).
func (s *Server) logResultSave(ctx context.Context, requestID, methodID int64, seriesNum int, who string, before, after map[string]any) {
	if who == "" {
		return
	}
	kind := resultSaveKind(before)
	var beforeJSON any
	if before == nil {
		beforeJSON = nil
	} else {
		b, err := json.Marshal(before)
		if err != nil {
			log.Printf("logResultSave: marshal before request=%d series=%d: %v", requestID, seriesNum, err)
			return
		}
		beforeJSON = string(b)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		log.Printf("logResultSave: marshal after request=%d series=%d: %v", requestID, seriesNum, err)
		return
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO request_audit_log (request_id, kind, who, method_id, series_num, values_before, values_after)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb)`,
		requestID, kind, who, methodID, seriesNum, beforeJSON, string(afterJSON)); err != nil {
		log.Printf("logResultSave: request=%d series=%d: %v", requestID, seriesNum, err)
	}
}

// handleListAuditLog — GET /requests/{id}/audit-log: та же видимость, что у журнала
// отправленных писем (handleListSentEmails) — участник/владелец/admin/lab_auditor.
// Без пагинации/фильтров (см. границы спеки) — newest-first.
func (s *Server) handleListAuditLog(w http.ResponseWriter, r *http.Request) {
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
SELECT id, request_id, kind, who, created_at,
       COALESCE(old_status, ''), COALESCE(new_status, ''),
       COALESCE(method_id, 0), COALESCE(series_num, 0),
       values_before, values_after
FROM request_audit_log WHERE request_id = $1 ORDER BY created_at DESC`, requestID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]AuditLogEntry, 0, 16)
	for rows.Next() {
		var it AuditLogEntry
		var createdAt time.Time
		var beforeRaw, afterRaw []byte
		if err := rows.Scan(&it.ID, &it.RequestID, &it.Kind, &it.Who, &createdAt,
			&it.OldStatus, &it.NewStatus, &it.MethodID, &it.SeriesNum, &beforeRaw, &afterRaw); err != nil {
			continue
		}
		it.CreatedAt = createdAt.Format(time.RFC3339)
		if len(beforeRaw) > 0 {
			_ = json.Unmarshal(beforeRaw, &it.ValuesBefore)
		}
		if len(afterRaw) > 0 {
			_ = json.Unmarshal(afterRaw, &it.ValuesAfter)
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}
