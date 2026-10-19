package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- Хранение и ручки отчёта (2026-10-16) ----
//
// ensureReport собирает отчёт (протокол + ИИ-разделы + приложение), рендерит DOCX
// на бланке, кладёт в S3 и запоминает в request_reports. Готовый файл отдаётся
// скачиванием и вкладывается в письмо заказчику при завершении (outbound_email.go).

type reportResult struct {
	DOCX    []byte
	HasAI   bool
	FileKey string
}

// ensureReport — идемпотентно собирает/пересобирает отчёт заявки. Ошибка ИИ не
// прерывает отчёт (has_ai=false), ошибка рендера/чтения данных — прерывает.
//
// С 2026-10-19 (см. docs/superpowers/specs/2026-10-19-sbe-lims-outbound-email-queue-design.md)
// готовый файл переиспользуется: при force=false отчёт берётся из S3, если он
// собран после последней правки заявки/результатов и ИИ в нём удался (или у
// метода нет инструкции). Отчёт с провалившимся ИИ (ai_state='failed') готовым
// не считается — письмо с ним уходит, но при следующем обращении ИИ пробуется
// снова. force=true — принудительная пересборка (кнопка «Пересобрать отчёт»).
func (s *Server) ensureReport(ctx context.Context, requestID int64, force bool) (*reportResult, error) {
	if !force {
		if res := s.reuseReport(ctx, requestID); res != nil {
			return res, nil
		}
	}

	p, err := s.buildProtocol(ctx, requestID, "protocol")
	if err != nil {
		return nil, fmt.Errorf("build protocol: %w", err)
	}
	rc, err := s.loadReportContext(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("report context: %w", err)
	}

	var ai *aiSections
	hasAI := false
	aiErr := ""
	hasInstruction := strings.TrimSpace(rc.Instruction) != ""
	if hasInstruction {
		secs, aiGenErr := s.generateReportSections(ctx, rc)
		if aiGenErr != nil {
			aiErr = aiGenErr.Error()
			log.Printf("report: AI sections failed for request %d: %v", requestID, aiGenErr)
		} else if secs != nil {
			ai = secs
			hasAI = true
		}
	}
	aiState := reportAIState(hasInstruction, hasAI)

	composeReportBlocks(p, rc, ai)

	docx, err := s.reportDocx(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("render report docx: %w", err)
	}

	key := fmt.Sprintf("lab/%d/report-%d.docx", requestID, time.Now().Unix())

	var oldKey string
	_ = s.pool.QueryRow(ctx, `SELECT file_key FROM request_reports WHERE request_id = $1`, requestID).Scan(&oldKey)

	if _, _, putErr := s.s3.Put(ctx, key, docx); putErr != nil {
		// S3 недоступен и буфер переполнен — отчёт всё равно возвращаем (письмо/
		// скачивание отдадут байты), в БД ключ не пишем.
		log.Printf("report: s3 put request %d: %v", requestID, putErr)
		key = ""
	}

	if err := s.upsertReportRow(ctx, requestID, key, hasAI, aiErr, aiState); err != nil {
		log.Printf("report: upsert row request %d: %v", requestID, err)
	}
	if oldKey != "" && oldKey != key {
		if err := s.s3.Delete(ctx, oldKey); err != nil {
			log.Printf("report: delete old key %q: %v", oldKey, err)
		}
	}
	return &reportResult{DOCX: docx, HasAI: hasAI, FileKey: key}, nil
}

// reportIsFresh — можно ли отдать сохранённый отчёт, не пересобирая: он собран
// не раньше последней правки заявки/результатов, и ИИ в нём либо удался, либо
// не требовался.
func reportIsFresh(generatedAt time.Time, aiState string, lastChange time.Time) bool {
	return !generatedAt.IsZero() && !generatedAt.Before(lastChange) &&
		(aiState == "ok" || aiState == "none")
}

// reportAIState — состояние ИИ-разделов сохранённого отчёта (см. ai_state).
func reportAIState(hasInstruction, hasAI bool) string {
	if !hasInstruction {
		return "none"
	}
	if hasAI {
		return "ok"
	}
	return "failed"
}

// reuseReport — готовый отчёт из S3, если он свежий и полный; nil — надо
// пересобирать.
func (s *Server) reuseReport(ctx context.Context, requestID int64) *reportResult {
	var key, aiState string
	var generatedAt *time.Time
	var hasAI bool
	if err := s.pool.QueryRow(ctx, `
SELECT file_key, ai_state, generated_at, has_ai FROM request_reports WHERE request_id = $1`,
		requestID).Scan(&key, &aiState, &generatedAt, &hasAI); err != nil {
		return nil
	}
	if key == "" || generatedAt == nil {
		return nil
	}
	lastChange, err := s.latestRequestChange(ctx, requestID)
	if err != nil {
		log.Printf("report: latest change request %d: %v", requestID, err)
		return nil
	}
	if !reportIsFresh(*generatedAt, aiState, lastChange) {
		return nil
	}
	data, err := s.s3.Get(ctx, key)
	if err != nil {
		log.Printf("report: s3 get %q: %v", key, err)
		return nil
	}
	return &reportResult{DOCX: data, HasAI: hasAI, FileKey: key}
}

// latestRequestChange — момент последней правки, от которой отчёт считается
// устаревшим: сама заявка или любая её серия/агрегат. Нет серий — ограничение
// задаёт только updated_at заявки.
//
// Опора — именно updated_at: правка результатов через API его двигает, а
// точечный прямой SQL (как разведение номеров/массовые скрипты) — нет. Если
// данные поправлены мимо API, отчёт может считаться устаревшим до следующей
// правки через приложение; это осознанное упрощение, а не забытый случай
// (см. корневой AGENTS.md, «правка данных обязана двигать updated_at»).
func (s *Server) latestRequestChange(ctx context.Context, requestID int64) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `
SELECT GREATEST(
	r.updated_at,
	COALESCE((SELECT MAX(updated_at) FROM measurement_results WHERE request_id = r.id), 'epoch'::timestamptz),
	COALESCE((SELECT MAX(updated_at) FROM aggregated_results WHERE request_id = r.id), 'epoch'::timestamptz)
) FROM requests r WHERE r.id = $1`, requestID).Scan(&at)
	return at, err
}

func (s *Server) upsertReportRow(ctx context.Context, requestID int64, fileKey string, hasAI bool, aiErr, aiState string) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO request_reports (request_id, file_key, has_ai, ai_error, ai_state, generated_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now(), now())
ON CONFLICT (request_id) DO UPDATE SET
	file_key = EXCLUDED.file_key, has_ai = EXCLUDED.has_ai, ai_error = EXCLUDED.ai_error,
	ai_state = EXCLUDED.ai_state,
	generated_at = EXCLUDED.generated_at, updated_at = now()`,
		requestID, fileKey, hasAI, aiErr, aiState)
	return err
}

// handleGetReport — GET /api/lab/requests/{id}/report.docx: если готового нет —
// собрать; отдать DOCX. Чтение (requireLabRead), как протокол.
func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}

	var data []byte
	res, genErr := s.ensureReport(r.Context(), requestID, false)
	if genErr != nil {
		log.Printf("report: generate request %d: %v", requestID, genErr)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "report generation failed"})
		return
	}
	data = res.DOCX

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="report-%d.docx"`, requestID))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		log.Printf("report: write response: %v", err)
	}
}

// handleRegenerateReport — POST /api/lab/requests/{id}/report: пересобрать отчёт
// (после правок результатов). Запись (requireLabAccess) — тратит токены ИИ.
func (s *Server) handleRegenerateReport(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabAccess(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	res, genErr := s.ensureReport(r.Context(), requestID, true)
	if genErr != nil {
		log.Printf("report: regenerate request %d: %v", requestID, genErr)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "report generation failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "has_ai": res.HasAI})
}

// reportFilename — имя вложения отчёта в письме (чистая функция, для теста).
func reportFilename(requestID int64) string {
	return fmt.Sprintf("report-%d.docx", requestID)
}

// protocolFilename — имя вложения фолбэка (обычный протокол, без ИИ/бланка).
func protocolFilename(requestID int64) string {
	return fmt.Sprintf("protocol-%d.docx", requestID)
}
