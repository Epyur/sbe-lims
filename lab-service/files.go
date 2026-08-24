package main

import (
	"io"
	"log"
	"net/http"
	"strconv"
)

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

	key := s3Key(header.Filename)
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read file"})
		return
	}
	size, url, err := s.s3.Put(r.Context(), key, data)
	if err != nil {
		log.Printf("s3 put: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "s3 error"})
		return
	}

	// Связываем файл с заявкой, если передан request_id.
	var requestID int64
	if v := r.FormValue("request_id"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			requestID = parsed
		}
	}
	if requestID > 0 {
		var exists bool
		if err := s.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM requests WHERE id = $1)`, requestID).Scan(&exists); err == nil && exists {
			_, _ = s.pool.Exec(r.Context(), `
INSERT INTO files (request_id, file_key, file_name, file_size, file_url, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6)`,
				requestID, key, header.Filename, size, url, currentEmail(r))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"file_key":   key,
		"file_name":  header.Filename,
		"file_size":  size,
		"file_url":   url,
		"request_id": requestID,
	})
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key is required"})
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
