package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const missingNamePlaceholder = "Без названия"

// eknLookupEmail — служебная учётная запись для вызовов lab-service → ekn-service
// (2026-08-22, по согласованию с пользователем): переиспользуем уже
// существующего владельца приложения ekn (admin в ekn_permissions), а не
// заводим отдельную служебную запись — только для чтения карточки товара по
// номеру ЕКН, ничего не меняет на стороне ekn-service.
const eknLookupEmail = "polishchuk@tn.ru"

// eknProduct — только поля, нужные для имени продукта (полная карточка шире,
// см. sbe-ekn/src/types/ekn.ts EknProduct).
type eknProduct struct {
	Name string `json:"name"`
}

func eknServiceURL() string {
	if v := strings.TrimSpace(os.Getenv("EKN_SERVICE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://ekn:3000"
}

// lookupEknProductName — имя продукта из справочника ЕКН (ekn-service) по
// номеру. Используется, когда письмо email-импорта не содержит читаемого
// названия продукта (email_ingest.go), чтобы не создавать заявки/объекты с
// заглушкой "Без названия", хотя продукт по факту есть в справочнике.
// Возвращает "" при недоступности ekn-service/номер не найден — это НЕ должно
// блокировать создание заявки (тот же принцип отказоустойчивости, что у
// клиентского sync.service.getEknProduct).
func lookupEknProductName(ctx context.Context, ekn string) string {
	ekn = strings.TrimSpace(ekn)
	if ekn == "" {
		return ""
	}
	token, err := mintServiceJWT("ekn", eknLookupEmail, 2*time.Minute)
	if err != nil {
		log.Printf("ekn lookup: mint token: %v", err)
		return ""
	}
	endpoint := fmt.Sprintf("%s/api/ekn/product/%s", eknServiceURL(), url.PathEscape(ekn))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ekn lookup: request %s: %v", ekn, err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var product eknProduct
	if err := json.Unmarshal(body, &product); err != nil {
		return ""
	}
	return strings.TrimSpace(product.Name)
}

// handleBackfillEknNames — разовое исправление уже импортированных объектов/
// заявок, у которых имя застряло на заглушке missingNamePlaceholder, хотя ЕКН
// задан и реально есть в справочнике (тот же случай, что lookupEknProductName
// теперь предотвращает для НОВЫХ писем). Идемпотентна: повторный вызов не
// трогает уже исправленные строки (у них имя больше не заглушка). Вызывается
// вручную админом, не автоматически при старте — не хотим сетевых вызовов к
// ekn-service в цепочке запуска сервиса.
func (s *Server) handleBackfillEknNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
SELECT id, characteristics->>'ekn' FROM objects
WHERE (name = '' OR name = $1) AND COALESCE(characteristics->>'ekn', '') != ''`, missingNamePlaceholder)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	type candidate struct {
		id  int64
		ekn string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.ekn); err != nil {
			continue
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	fixed := 0
	for _, c := range candidates {
		name := lookupEknProductName(r.Context(), c.ekn)
		if name == "" {
			continue
		}
		if _, err := s.pool.Exec(r.Context(),
			`UPDATE objects SET name = $2, updated_at = now() WHERE id = $1`, c.id, name); err != nil {
			log.Printf("backfill ekn names: update object %d: %v", c.id, err)
			continue
		}
		if _, err := s.pool.Exec(r.Context(), `
UPDATE requests SET title = $2, updated_at = now()
WHERE object_id = $1 AND (title = '' OR title = $3)`, c.id, name, missingNamePlaceholder); err != nil {
			log.Printf("backfill ekn names: update requests for object %d: %v", c.id, err)
		}
		fixed++
	}
	writeJSON(w, http.StatusOK, map[string]any{"checked": len(candidates), "fixed": fixed})
}
