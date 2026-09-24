package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBufferFull — буфер аварийного хранения (bufferMaxBytes) уже занят под
// завязку, новый файл принять некуда. Отдельная переменная, а не просто
// fmt.Errorf, — чтобы handleUploadFile мог отличить эту причину от прочих и
// ответить тестировщику понятным текстом вместо общего "s3 error".
var ErrBufferFull = errors.New("буфер аварийного хранения переполнен, файл не сохранён")

// S3Store — загрузка/скачивание файлов через rclone CLI (remote firstvds_doc -> бакет sbe-doc).
// Используем rclone вместо aws-sdk-go-v2: он стабильно работает с этим Ceph (проверено),
// а SDK внутри HTTP-обработчика зависал и дестабилизировал сервер (см. documents-service).
//
// Буфер загрузки при недоступности S3 (2026-09-24, docs/superpowers/specs/
// 2026-09-24-s3-upload-buffer-design.md): при ошибке rclone на Put файл вместо
// потери попытки пишется на локальный диск (bufferDir) и регистрируется в
// pending_uploads — фоновый воркер (upload_buffer.go) дозаливает его в S3, как
// только связь восстановится. Put/Get/Delete прозрачны для вызывающего кода:
// ключ существует с точки зрения БД сразу же, независимо от того, где физически
// лежат байты в данный момент.
type S3Store struct {
	bucket     string
	configPath string
	publicBase string
	pool       *pgxpool.Pool

	bufferDir      string
	bufferMaxBytes int64
	wake           chan struct{}

	mu        sync.Mutex
	coolUntil time.Time
}

const (
	// rcloneTimeout ограничивает каждый вызов rclone — раньше context.Context
	// внутрь exec.Command не пробрасывался вовсе, и зависшее (не мгновенно
	// отказавшее) соединение с S3 держало HTTP-обработчик неограниченно долго.
	rcloneTimeout = 25 * time.Second
	// s3CooldownAfterFailure — после неудачи Put следующие попытки этот срок
	// сразу уходят в буфер, минуя реальный вызов rclone: при затяжной аварии
	// иначе каждая загрузка ждала бы полный rcloneTimeout ради того же исхода.
	// Сбрасывается первой же успешной выгрузкой из буфера (см. upload_buffer.go).
	s3CooldownAfterFailure = 30 * time.Second
)

// NewS3Store создаёт конфиг rclone из env и возвращает S3Store.
func NewS3Store(pool *pgxpool.Pool) (*S3Store, error) {
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		return nil, fmt.Errorf("S3_ENDPOINT is required")
	}
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("S3_ACCESS_KEY and S3_SECRET_KEY are required")
	}
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		bucket = "sbe-doc"
	}

	configDir := "/root/.config/rclone"
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, err
	}
	configPath := filepath.Join(configDir, "rclone.conf")
	conf := fmt.Sprintf(`[firstvds_doc]
type = s3
provider = Other
access_key_id = %s
secret_access_key = %s
endpoint = %s
`, accessKey, secretKey, endpoint)
	if err := os.WriteFile(configPath, []byte(conf), 0o600); err != nil {
		return nil, err
	}

	bufferDir := os.Getenv("UPLOAD_BUFFER_DIR")
	if bufferDir == "" {
		bufferDir = "/data/upload-buffer"
	}
	if err := os.MkdirAll(bufferDir, 0o755); err != nil {
		return nil, fmt.Errorf("upload buffer dir: %w", err)
	}
	bufferMaxBytes := int64(10) * 1024 * 1024 * 1024
	if v := os.Getenv("UPLOAD_BUFFER_MAX_BYTES"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			bufferMaxBytes = parsed
		}
	}

	return &S3Store{
		bucket:         bucket,
		configPath:     configPath,
		publicBase:     fmt.Sprintf("%s/%s", strings.TrimSuffix(endpoint, "/"), bucket),
		pool:           pool,
		bufferDir:      bufferDir,
		bufferMaxBytes: bufferMaxBytes,
		wake:           make(chan struct{}, 1),
	}, nil
}

func (s *S3Store) publicBaseURL() string {
	return s.publicBase
}

// remote возвращает полный адрес объекта: remote:bucket/key.
func (s *S3Store) remote(key string) string {
	return fmt.Sprintf("firstvds_doc:%s/%s", s.bucket, key)
}

func (s *S3Store) rcloneArgs(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+s.configPath)
	return cmd
}

// Put загружает файл в S3. data — содержимое целиком; во временный файл, затем rclone copyto.
//
// При недоступности S3 (2026-09-24) файл не теряется: вместо ошибки байты
// уходят в локальный буфер (bufferPut), а вызывающему коду возвращается тот же
// успешный результат, что и при обычной загрузке — ключ и публичный URL уже
// "существуют" для БД, физическое расположение байт для неё не имеет значения.
// Фоновый воркер (upload_buffer.go) дозаливает буфер в S3 сам.
func (s *S3Store) Put(ctx context.Context, key string, data []byte) (int64, string, error) {
	url := fmt.Sprintf("%s/%s", s.publicBase, key)

	if s.underCooldown() {
		if err := s.bufferPut(ctx, key, data); err != nil {
			return 0, "", err
		}
		return int64(len(data)), url, nil
	}

	tmp, err := os.CreateTemp("", "rclone-upload-*")
	if err != nil {
		return 0, "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return 0, "", err
	}

	rcCtx, cancel := context.WithTimeout(ctx, rcloneTimeout)
	defer cancel()
	start := time.Now()
	cmd := s.rcloneArgs(rcCtx, "copyto", "--log-level", "ERROR", tmp.Name(), s.remote(key))
	out, err := cmd.CombinedOutput()
	if err == nil {
		log.Printf("rclone copyto OK: %s (elapsed %s, %d bytes)", key, time.Since(start), len(data))
		return int64(len(data)), url, nil
	}
	log.Printf("rclone copyto failed: %v (%s) out=%s", err, time.Since(start), strings.TrimSpace(string(out)))
	s.startCooldown()
	if bufErr := s.bufferPut(ctx, key, data); bufErr != nil {
		return 0, "", bufErr
	}
	return int64(len(data)), url, nil
}

// bufferPath — путь буферизованной копии на диске: та же структура подкаталогов,
// что и ключ в S3. Ключ — всегда с прямым слэшем (как в S3), поэтому "path", а
// не "path/filepath" — не зависим от разделителя платформы сборки.
func (s *S3Store) bufferPath(key string) string {
	return pathpkg.Join(s.bufferDir, key)
}

// bufferedBytes — суммарный размер того, что сейчас лежит в очереди на дозаливку.
func (s *S3Store) bufferedBytes(ctx context.Context) (int64, error) {
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(file_size), 0) FROM pending_uploads`).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// bufferPut пишет файл в локальный буфер и ставит его в очередь на дозаливку.
// Лимит проверяется ДО записи на диск — переполнение возвращает ErrBufferFull,
// не тратя место под файл, который всё равно придётся отклонить.
func (s *S3Store) bufferPut(ctx context.Context, key string, data []byte) error {
	used, err := s.bufferedBytes(ctx)
	if err != nil {
		return fmt.Errorf("buffer size check: %w", err)
	}
	if used+int64(len(data)) > s.bufferMaxBytes {
		return ErrBufferFull
	}
	bufPath := s.bufferPath(key)
	if err := os.MkdirAll(pathpkg.Dir(bufPath), 0o755); err != nil {
		return fmt.Errorf("buffer mkdir: %w", err)
	}
	if err := os.WriteFile(bufPath, data, 0o644); err != nil {
		return fmt.Errorf("buffer write: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO pending_uploads (key, file_size) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET file_size = EXCLUDED.file_size, queued_at = now(), attempts = 0, last_error = NULL`,
		key, len(data)); err != nil {
		_ = os.Remove(bufPath)
		return fmt.Errorf("buffer queue insert: %w", err)
	}
	log.Printf("upload buffer: заняло очередь %s (%d байт)", key, len(data))
	s.nudge()
	return nil
}

// nudge будит фоновый воркер выгрузки, не дожидаясь тикера. Не блокирует: если
// воркер уже разбужен или занят, второй сигнал не нужен (тот же приём, что у
// nudgeCleanup в photo-service).
func (s *S3Store) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// underCooldown/startCooldown/clearCooldown — короткое "остывание" после
// неудачи (см. s3CooldownAfterFailure): следующие Put этот срок сразу уходят
// в буфер, не тратя время на заведомо обречённую попытку через rclone.
func (s *S3Store) underCooldown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.coolUntil)
}

func (s *S3Store) startCooldown() {
	s.mu.Lock()
	s.coolUntil = time.Now().Add(s3CooldownAfterFailure)
	s.mu.Unlock()
}

func (s *S3Store) clearCooldown() {
	s.mu.Lock()
	s.coolUntil = time.Time{}
	s.mu.Unlock()
}

// Link выпускает временную подписанную (presigned) ссылку на объект — бакет sbe-doc НЕ
// публичный (прямой s.publicBase+key отдаёт 403, подтверждено прямым HTTP-тестом,
// 2026-08-24, см. AGENTS.md "фото в протоколе"), но presigned-ссылка на GET (НЕ на HEAD —
// у этого Ceph-гейтвея presigned HEAD почему-то 403, GET подтверждённо отдаёт 200 с
// верным Content-Type) работает. rclone у этого бэкенда ограничивает срок максимум 1
// неделей ("Reducing expiry to 1w") — вызывающая сторона (handleFileRedirect) выпускает
// свежую ссылку на КАЖДЫЙ запрос, поэтому сама ссылка никогда не хранится дольше своего
// использования.
func (s *S3Store) Link(ctx context.Context, key string, expiry time.Duration) (string, error) {
	rcCtx, cancel := context.WithTimeout(ctx, rcloneTimeout)
	defer cancel()
	cmd := s.rcloneArgs(rcCtx, "link", "--expire", expiry.String(), s.remote(key))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rclone link: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// rclone печатает NOTICE-строки (лог) вперемешку с самой ссылкой в
	// CombinedOutput — ищем строку, которая реально начинается с "http", а не
	// полагаемся на то, что ссылка обязательно последняя.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http") {
			return line, nil
		}
	}
	return "", fmt.Errorf("rclone link: no URL in output: %s", strings.TrimSpace(string(out)))
}

// Get скачивает файл — сначала проверяет локальный буфер (2026-09-24: файл,
// который ещё не доехал до S3, должен читаться сразу же, а не отдавать 404
// до завершения фоновой дозаливки), и только если там пусто — идёт в S3.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	if data, err := os.ReadFile(s.bufferPath(key)); err == nil {
		return data, nil
	}

	tmp, err := os.CreateTemp("", "rclone-download-*")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	rcCtx, cancel := context.WithTimeout(ctx, rcloneTimeout)
	defer cancel()
	start := time.Now()
	cmd := s.rcloneArgs(rcCtx, "copyto", "--log-level", "ERROR", s.remote(key), tmp.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("rclone copyto(download) failed: %v (%s) out=%s", err, time.Since(start), strings.TrimSpace(string(out)))
		return nil, err
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	log.Printf("rclone copyto(download) OK: %s (elapsed %s, %d bytes)", key, time.Since(start), len(data))
	return data, nil
}

// s3Key формирует уникальный ключ для файла заявки.
func s3Key(fileName string) string {
	return fmt.Sprintf("lab/%s/main-%s", randomID(), sanitizeKey(fileName))
}

func sanitizeKey(s string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_", "{", "", "}", "")
	s = replacer.Replace(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "file"
	}
	return s
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Printf("rand.Read: %v", err)
	}
	return fmt.Sprintf("%x", b)
}
