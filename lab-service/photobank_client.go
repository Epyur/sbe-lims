package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Зеркалирование фото серии испытания в sbe-photobank (2026-09-01), см.
// docs/superpowers/specs/2026-09-01-sbe-lims-photobank-mirror-design.md. Переиспользует
// готовый межсервисный механизм serviceToken/auth-service (ekn_client.go) — тот же
// принцип, что уже работает для lab-service → ekn-service.

const photobankRootFolderName = "Испытания"

func photoServiceURL() string {
	if v := strings.TrimSpace(os.Getenv("PHOTO_SERVICE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://photo:3000"
}

// changedPhotoFields — фото-поля серии, которые появились или изменились между
// прежним и новым values (пустая строка/отсутствие в after — не считается).
func changedPhotoFields(before, after map[string]any) map[string]string {
	changed := map[string]string{}
	for _, key := range []string{"photo_before", "photo_after", "photo_before_test", "photo_after_test"} {
		newVal, ok := after[key].(string)
		if !ok || newVal == "" {
			continue
		}
		oldVal, hadOld := before[key]
		if !hadOld || oldVal != newVal {
			changed[key] = newVal
		}
	}
	return changed
}

func photoFieldKind(field string) string {
	switch field {
	case "photo_after", "photo_after_test":
		return "после испытания"
	default:
		return "до испытания"
	}
}

// requestLabAndNumber — имя лаборатории заявки (для подпапки Фотобанка) и её
// customer_number (для названия карточки). Пустая лаба (NULL lab_id) — допустимо,
// зовущий код подставляет заглушку.
func (s *Server) requestLabAndNumber(ctx context.Context, requestID int64) (labName, customerNumber string) {
	_ = s.pool.QueryRow(ctx, `
SELECT COALESCE(l.name, ''), r.customer_number
FROM requests r
LEFT JOIN labs l ON l.id = r.lab_id
WHERE r.id = $1`, requestID).Scan(&labName, &customerNumber)
	return labName, customerNumber
}

// fetchPhotoBytes достаёт байты фото по значению, сохранённому в measurement_results.values:
// собственная ссылка (`.../api/lab/file-redirect?key=...`) читается напрямую из S3 этого
// сервиса, внешняя (например forms.yandex.ru) — скачивается по HTTP.
func fetchPhotoBytes(ctx context.Context, s *Server, sourceURL string) ([]byte, string, error) {
	prefix := publicBaseURL() + "/api/lab/file-redirect?key="
	if strings.HasPrefix(sourceURL, prefix) {
		key, err := url.QueryUnescape(strings.TrimPrefix(sourceURL, prefix))
		if err != nil {
			return nil, "", fmt.Errorf("decode key: %w", err)
		}
		data, err := s.s3.Get(ctx, key)
		if err != nil {
			return nil, "", err
		}
		return data, path.Base(key), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, "", err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch photo: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	name := "photo.jpg"
	if parsed, err := url.Parse(sourceURL); err == nil {
		if base := path.Base(parsed.Path); base != "" && base != "." && base != "/" {
			name = base
		}
	}
	return data, name, nil
}

type photoFolder struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID int64  `json:"parent_id"`
}

// findOrCreatePhotoFolder ищет папку по (parentID, name) среди видимых сервисному
// токену папок, создаёт при отсутствии. Идемпотентно с точностью до редкой гонки при
// одновременном первом вызове для одной лабы (см. design — риск принят намеренно).
func findOrCreatePhotoFolder(ctx context.Context, token, name string, parentID int64) (int64, error) {
	endpoint := photoServiceURL() + "/api/photo/folders"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("list folders: status %d", resp.StatusCode)
	}
	var out struct {
		Folders []photoFolder `json:"folders"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	for _, f := range out.Folders {
		if f.ParentID == parentID && f.Name == name {
			return f.ID, nil
		}
	}

	body, err := json.Marshal(map[string]any{"name": name, "parent_id": parentID})
	if err != nil {
		return 0, err
	}
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil {
		return 0, err
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("create folder %q: status %d", name, createResp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		return 0, err
	}
	return created.ID, nil
}

// uploadPhotoFile грузит байты оригинала в photo-service (S3 `sbe-photo`), возвращает
// ключи оригинала и авто-миниатюры.
func uploadPhotoFile(ctx context.Context, token string, folderID int64, filename string, data []byte) (fileKey, thumbKey string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", "", err
	}
	_ = w.WriteField("folder_id", strconv.FormatInt(folderID, 10))
	_ = w.WriteField("kind", "image")
	if err := w.Close(); err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, photoServiceURL()+"/api/photo/file", &buf)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("upload photo: status %d", resp.StatusCode)
	}
	var out struct {
		FileKey  string `json:"file_key"`
		ThumbKey string `json:"thumb_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.FileKey, out.ThumbKey, nil
}

// pushPhotoCard создаёт карточку фото (id=0 — сервер сам назначит id и author_email
// из токена).
func pushPhotoCard(ctx context.Context, token string, folderID int64, title, fileKey, thumbKey, fileName string, fileSize int64) error {
	card := map[string]any{
		"id":           0,
		"folder_id":    folderID,
		"title":        title,
		"file_key":     fileKey,
		"thumb_key":    thumbKey,
		"thumb_author": "auto",
		"file_name":    fileName,
		"file_size":    fileSize,
		"kind":         "image",
	}
	body, err := json.Marshal(map[string]any{"photos": []map[string]any{card}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, photoServiceURL()+"/api/photo/sync/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push photo card: status %d", resp.StatusCode)
	}
	return nil
}

// mirrorPhotoToPhotobank — best-effort: любая ошибка на любом шаге только логируется,
// вызывается из горутины (см. saveResultSeries), не должна влиять на сохранение
// результата испытания.
func mirrorPhotoToPhotobank(ctx context.Context, s *Server, labName, title, sourceURL string) {
	data, filename, err := fetchPhotoBytes(ctx, s, sourceURL)
	if err != nil {
		log.Printf("photobank mirror: fetch %q: %v", sourceURL, err)
		return
	}
	token, err := serviceToken(ctx, "photo")
	if err != nil {
		log.Printf("photobank mirror: service token: %v", err)
		return
	}
	if labName == "" {
		labName = "Без лаборатории"
	}
	rootID, err := findOrCreatePhotoFolder(ctx, token, photobankRootFolderName, 0)
	if err != nil {
		log.Printf("photobank mirror: root folder: %v", err)
		return
	}
	folderID, err := findOrCreatePhotoFolder(ctx, token, labName, rootID)
	if err != nil {
		log.Printf("photobank mirror: lab folder %q: %v", labName, err)
		return
	}
	fileKey, thumbKey, err := uploadPhotoFile(ctx, token, folderID, filename, data)
	if err != nil {
		log.Printf("photobank mirror: upload %q: %v", filename, err)
		return
	}
	if err := pushPhotoCard(ctx, token, folderID, title, fileKey, thumbKey, filename, int64(len(data))); err != nil {
		log.Printf("photobank mirror: push card %q: %v", title, err)
		return
	}
	log.Printf("photobank mirror: ok title=%q folder_id=%d", title, folderID)
}
