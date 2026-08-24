package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// S3Store — загрузка/скачивание файлов через rclone CLI (remote firstvds_doc -> бакет sbe-doc).
// Используем rclone вместо aws-sdk-go-v2: он стабильно работает с этим Ceph (проверено),
// а SDK внутри HTTP-обработчика зависал и дестабилизировал сервер (см. documents-service).
type S3Store struct {
	bucket     string
	configPath string
	publicBase string
}

// NewS3Store создаёт конфиг rclone из env и возвращает S3Store.
func NewS3Store() (*S3Store, error) {
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

	return &S3Store{
		bucket:     bucket,
		configPath: configPath,
		publicBase: fmt.Sprintf("%s/%s", strings.TrimSuffix(endpoint, "/"), bucket),
	}, nil
}

func (s *S3Store) publicBaseURL() string {
	return s.publicBase
}

// remote возвращает полный адрес объекта: remote:bucket/key.
func (s *S3Store) remote(key string) string {
	return fmt.Sprintf("firstvds_doc:%s/%s", s.bucket, key)
}

func (s *S3Store) rcloneArgs(args ...string) *exec.Cmd {
	cmd := exec.Command("rclone", args...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+s.configPath)
	return cmd
}

// Put загружает файл в S3. data — содержимое целиком; во временный файл, затем rclone copyto.
func (s *S3Store) Put(ctx context.Context, key string, data []byte) (int64, string, error) {
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

	start := time.Now()
	cmd := s.rcloneArgs("copyto", "--log-level", "ERROR", tmp.Name(), s.remote(key))
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("rclone copyto failed: %v (%s) out=%s", err, time.Since(start), strings.TrimSpace(string(out)))
		return 0, "", err
	}
	log.Printf("rclone copyto OK: %s (elapsed %s, %d bytes)", key, time.Since(start), len(data))
	return int64(len(data)), fmt.Sprintf("%s/%s", s.publicBase, key), nil
}

// Get скачивает файл из S3 и возвращает содержимое.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "rclone-download-*")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	start := time.Now()
	cmd := s.rcloneArgs("copyto", "--log-level", "ERROR", s.remote(key), tmp.Name())
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
