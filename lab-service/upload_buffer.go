package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Фоновая дозаливка буфера в S3 (2026-09-24), см. docs/superpowers/specs/
// 2026-09-24-s3-upload-buffer-design.md. Тот же приём, что у pending_deletes в
// photo-service: таблица-очередь в Postgres + тикер + канал-«толчок» для
// мгновенной реакции (см. S3Store.nudge в s3.go), лимит попыток вместо
// бесконечного ретрая заведомо непроходимого ключа.

const (
	uploadFlushBatchSize   = 50
	uploadFlushInterval    = 2 * time.Minute
	uploadFlushMaxAttempts = 50
)

// startUploadFlushWorker запускает фоновый обход очереди pending_uploads. Один
// заход сразу при старте (после перезапуска в очереди могло остаться
// недозалитое — буфер на диске и строки в БД переживают рестарт контейнера),
// дальше — по тикеру и по толчку.
func (s *S3Store) startUploadFlushWorker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(uploadFlushInterval)
		defer ticker.Stop()
		s.runUploadFlushOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runUploadFlushOnce(ctx)
			case <-s.wake:
				s.runUploadFlushOnce(ctx)
			}
		}
	}()
}

// runUploadFlushOnce выгружает одну пачку файлов из буфера в S3. Возвращает,
// сколько выгружено и сколько не удалось — для журнала и для живой проверки.
func (s *S3Store) runUploadFlushOnce(ctx context.Context) (uploaded, failed int) {
	rows, err := s.pool.Query(ctx, `
SELECT key FROM pending_uploads
WHERE attempts < $1
ORDER BY attempts, queued_at
LIMIT $2`, uploadFlushMaxAttempts, uploadFlushBatchSize)
	if err != nil {
		log.Printf("upload buffer: выборка очереди: %v", err)
		return 0, 0
	}
	keys := make([]string, 0, uploadFlushBatchSize)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) == 0 {
		return 0, 0
	}

	for _, key := range keys {
		path := s.bufferPath(key)
		rcCtx, cancel := context.WithTimeout(ctx, rcloneTimeout)
		cmd := s.rcloneArgs(rcCtx, "copyto", "--log-level", "ERROR", path, s.remote(key))
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			failed++
			if _, uerr := s.pool.Exec(ctx, `
UPDATE pending_uploads SET attempts = attempts + 1, last_error = $2 WHERE key = $1`,
				key, strings.TrimSpace(string(out))); uerr != nil {
				log.Printf("upload buffer: отметка неудачи для %s: %v", key, uerr)
			}
			continue
		}
		if _, derr := s.pool.Exec(ctx, `DELETE FROM pending_uploads WHERE key = $1`, key); derr != nil {
			// Объект уже в S3, а строка осталась: следующий заход попробует
			// выгрузить его снова, rclone copyto поверх существующего — не
			// ошибка, файл просто перезапишется тем же содержимым.
			log.Printf("upload buffer: строка очереди для %s осталась: %v", key, derr)
		}
		if rerr := os.Remove(path); rerr != nil {
			log.Printf("upload buffer: локальный файл %s не удалён: %v", key, rerr)
		}
		uploaded++
	}
	if uploaded > 0 {
		s.clearCooldown()
	}
	log.Printf("upload buffer: выгружено %d, не удалось %d (в пачке %d)", uploaded, failed, len(keys))
	return uploaded, failed
}

// BufferStatus — сводка для GET /api/lab/buffer/status. Stuck — сколько строк
// уже уткнулись в uploadFlushMaxAttempts и не участвуют в автоматических
// попытках (файл при этом никуда не делся, читается из буфера как обычно —
// см. S3Store.Get/handleFileRedirect — просто дозаливка нуждается в разборе).
type BufferStatus struct {
	Count          int        `json:"count"`
	TotalBytes     int64      `json:"total_bytes"`
	OldestQueuedAt *time.Time `json:"oldest_queued_at,omitempty"`
	Stuck          int        `json:"stuck"`
}

func (s *S3Store) status(ctx context.Context) (BufferStatus, error) {
	var out BufferStatus
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*), COALESCE(SUM(file_size), 0), MIN(queued_at) FROM pending_uploads`,
	).Scan(&out.Count, &out.TotalBytes, &out.OldestQueuedAt); err != nil {
		return out, err
	}
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM pending_uploads WHERE attempts >= $1`, uploadFlushMaxAttempts,
	).Scan(&out.Stuck); err != nil {
		return out, err
	}
	return out, nil
}

// handleBufferStatus — GET /api/lab/buffer/status (admin) — виден ли сейчас
// авральный буфер, чтобы не нужно было идти в docker logs/psql вручную.
func (s *Server) handleBufferStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.s3.status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}
