package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// validObjectKey проверяет ключ S3: разрешён только собственный префикс сервиса,
// без выхода за пределы («..») и служебных символов (path traversal — ревью B3).
func validObjectKey(key, prefix string) bool {
	if key == "" || len(key) > 512 {
		return false
	}
	if strings.Contains(key, "..") || strings.ContainsAny(key, "\\\x00\r\n") {
		return false
	}
	return strings.HasPrefix(key, prefix+"/")
}

// publicBaseURL — базовый адрес ЭТОГО сервиса, каким его видит браузер/клиент
// (2026-08-24, нужен для handleFileRedirect — ссылка на файл, встраиваемая в HTML
// протокола, должна быть абсолютной: значение уходит внутрь sandboxed iframe/srcdoc,
// относительный путь там резолвится не туда). По умолчанию — тот же адрес, что клиенты
// используют для API (см. sbe-lims/AGENTS.md, sync.service.ts apiUrl).
func publicBaseURL() string {
	if v := os.Getenv("LAB_PUBLIC_BASE_URL"); v != "" {
		return v
	}
	return "https://epyur.fvds.ru"
}

func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file is required"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read file"})
		return
	}

	var requestID int64
	if v := r.FormValue("request_id"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			requestID = parsed
		}
	}

	url, err := s.uploadFileBytes(r.Context(), requestID, header.Filename, data, currentEmail(r))
	if err != nil {
		log.Printf("s3 put: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "s3 error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"file_name":  header.Filename,
		"file_size":  len(data),
		"file_url":   url,
		"request_id": requestID,
	})
}

// uploadFileBytes грузит байты в объектное хранилище (тот же S3Store, что
// handleUploadFile) и, если requestID > 0 и заявка существует, регистрирует файл в общей
// таблице files заявки — переиспользуется HTTP-хендлером выше и email_ingest.go
// (resolvePhotoAttachments, 2026-08-24) напрямую, без HTTP-раунд-трипа.
//
// Возвращает НЕ прямой адрес объекта в S3 (s.s3.Put вернул бы именно его) — бакет
// sbe-doc НЕ публичный, прямой адрес отдаёт 403 (подтверждено прямым HTTP-тестом,
// 2026-08-24, см. AGENTS.md "фото в протоколе"). Возвращает стабильную, никогда не
// протухающую ссылку на handleFileRedirect (этот же сервис), которая при каждом заходе
// выпускает свежую presigned-ссылку (см. S3Store.Link) — так значение, один раз
// сохранённое в values/measurement_results, остаётся рабочим сколько угодно, а не только
// пока не истёк срок какой-то одной подписанной ссылки.
func (s *Server) uploadFileBytes(ctx context.Context, requestID int64, filename string,
	data []byte, uploadedBy string) (string, error) {
	// Дедуп по (request_id, file_name, file_size) — 2026-08-25, реальный инцидент:
	// заявка 287/2026 набрала 10 файлов вместо 2 (по 5 копий каждого) из-за ручных
	// повторных запусков одноразового CLI fetch-mail-photos во время отладки
	// фото-фикса (mail_fetch_by_id.go) — у этой функции не было НИКАКОЙ проверки на
	// уже загруженный файл: каждый вызов грузил в S3 и писал в files НОВУЮ строку с
	// новым случайным ключом, даже для байт-в-байт того же вложения того же письма
	// (saveResultSeries идемпотентен через ON CONFLICT, uploadFileBytes — не был).
	// Совпадение имени+размера — достаточно надёжный признак "тот же файл" для этого
	// сценария (files не хранит хэш содержимого; случайная коллизия имя+размер у
	// РАЗНЫХ файлов не стоит отдельной миграции под хэш). requestID<=0 — до этого
	// момента заявка ещё не найдена (см. resolvePhotoAttachments pending-путь) —
	// дедуп не имеет смысла, files ничего не знает про такую заявку.
	if requestID > 0 {
		var existingURL string
		err := s.pool.QueryRow(ctx, `
SELECT file_url FROM files WHERE request_id = $1 AND file_name = $2 AND file_size = $3
ORDER BY id LIMIT 1`, requestID, filename, len(data)).Scan(&existingURL)
		if err == nil {
			return existingURL, nil
		}
	}
	key := s3Key(filename)
	size, _, err := s.s3.Put(ctx, key, data)
	if err != nil {
		return "", err
	}
	fileURL := fmt.Sprintf("%s/api/lab/file-redirect?key=%s", publicBaseURL(), url.QueryEscape(key))
	if requestID > 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM requests WHERE id = $1)`, requestID).Scan(&exists); err == nil && exists {
			_, _ = s.pool.Exec(ctx, `
INSERT INTO files (request_id, file_key, file_name, file_size, file_url, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6)`,
				requestID, key, filename, size, fileURL, uploadedBy)
		}
	}
	return fileURL, nil
}

// handleFileRedirect — GET /api/lab/file-redirect?key=... — БЕЗ requirePerm (2026-08-24):
// ссылка встраивается как <img src> в HTML протокола/выписки, который открывается в
// sandboxed iframe и не может приложить Authorization-заголовок к запросу картинки; сам
// объектный ключ — случайный (см. s3Key), доступ по нему аналогичен "неопубликованной"
// облачной ссылке (тот же уровень защиты, что был у изначальной ссылки Яндекс.Форм — с
// той разницей, что эта реально открывается). Выпускает свежую presigned-ссылку
// (S3Store.Link) на каждый заход и 302-редиректит — сама ссылка нигде не хранится, значит
// не может протухнуть на хранении.
func (s *Server) handleFileRedirect(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" || !validObjectKey(key, "lab") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid or missing key"})
		return
	}
	link, err := s.s3.Link(r.Context(), key, 7*24*time.Hour)
	if err != nil {
		log.Printf("file redirect: s3 link: %v", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "file not found"})
		return
	}
	http.Redirect(w, r, link, http.StatusFound)
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" || !validObjectKey(key, "lab") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid or missing key"})
		return
	}
	data, err := s.s3.Get(r.Context(), key)
	if err != nil {
		log.Printf("s3 get: %v", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "file not found"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		log.Printf("download write: %v", err)
	}
}
