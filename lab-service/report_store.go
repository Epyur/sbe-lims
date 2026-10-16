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
func (s *Server) ensureReport(ctx context.Context, requestID int64) (*reportResult, error) {
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
	if strings.TrimSpace(rc.Instruction) != "" {
		secs, aiGenErr := s.generateReportSections(ctx, rc)
		if aiGenErr != nil {
			aiErr = aiGenErr.Error()
			log.Printf("report: AI sections failed for request %d: %v", requestID, aiGenErr)
		} else if secs != nil {
			ai = secs
			hasAI = true
		}
	}

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

	if err := s.upsertReportRow(ctx, requestID, key, hasAI, aiErr); err != nil {
		log.Printf("report: upsert row request %d: %v", requestID, err)
	}
	if oldKey != "" && oldKey != key {
		if err := s.s3.Delete(ctx, oldKey); err != nil {
			log.Printf("report: delete old key %q: %v", oldKey, err)
		}
	}
	return &reportResult{DOCX: docx, HasAI: hasAI, FileKey: key}, nil
}

func (s *Server) upsertReportRow(ctx context.Context, requestID int64, fileKey string, hasAI bool, aiErr string) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO request_reports (request_id, file_key, has_ai, ai_error, generated_at, updated_at)
VALUES ($1, $2, $3, $4, now(), now())
ON CONFLICT (request_id) DO UPDATE SET
	file_key = EXCLUDED.file_key, has_ai = EXCLUDED.has_ai, ai_error = EXCLUDED.ai_error,
	generated_at = EXCLUDED.generated_at, updated_at = now()`,
		requestID, fileKey, hasAI, aiErr)
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

	var key string
	_ = s.pool.QueryRow(r.Context(), `SELECT file_key FROM request_reports WHERE request_id = $1`, requestID).Scan(&key)

	var data []byte
	if key != "" {
		if b, getErr := s.s3.Get(r.Context(), key); getErr == nil {
			data = b
		} else {
			log.Printf("report: s3 get %q: %v", key, getErr)
		}
	}
	if data == nil {
		res, genErr := s.ensureReport(r.Context(), requestID)
		if genErr != nil {
			log.Printf("report: generate request %d: %v", requestID, genErr)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "report generation failed"})
			return
		}
		data = res.DOCX
	}

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
	res, genErr := s.ensureReport(r.Context(), requestID)
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
