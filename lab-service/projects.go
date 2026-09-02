package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Project struct {
	ID          int64  `json:"id"`
	ParentID    int64  `json:"parent_id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsEkn       bool   `json:"is_ekn"`
	GroupID     int64  `json:"group_id"`
	OwnerEmail  string `json:"owner_email"`
	// Триггеры почтового приёма (2026-09-02, прямой запрос пользователя): заявка
	// без своего ЕКН-автопроекта, поступившая по почте, попадает в этот проект,
	// если её ЕКН/отправитель совпал с триггером — см. email_ingest.go
	// findProjectByMailTrigger. Пусто — проект не участвует в авто-маршрутизации
	// (обычное ручное создание через плагин/веб, как и раньше).
	MailTriggerEkn    string `json:"mail_trigger_ekn"`
	MailTriggerSender string `json:"mail_trigger_sender"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// loadVisibleProjects возвращает проекты, видимые пользователю: публичные, его собственные,
// доступные по группе (член группы) или админу. Включены предки видимых проектов,
// чтобы дерево не рвалось.
func (s *Server) loadVisibleProjects(ctx context.Context, email string) ([]Project, error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return nil, err
	}
	isAdmin := roleRank(role) >= roleRank("admin")

	all := make([]Project, 0, 64)
	rows, err := s.pool.Query(ctx, `
SELECT id, COALESCE(parent_id, 0), code, name, description, is_ekn,
       COALESCE(group_id, 0), owner_email, mail_trigger_ekn, mail_trigger_sender, created_at, updated_at
FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Project
		var ca, ua time.Time
		if err := rows.Scan(&p.ID, &p.ParentID, &p.Code, &p.Name, &p.Description,
			&p.IsEkn, &p.GroupID, &p.OwnerEmail, &p.MailTriggerEkn, &p.MailTriggerSender, &ca, &ua); err != nil {
			log.Printf("loadVisibleProjects scan: %v", err)
			continue
		}
		p.CreatedAt = ca.Format(time.RFC3339)
		p.UpdatedAt = ua.Format(time.RFC3339)
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if isAdmin {
		return all, nil
	}

	visible := make([]Project, 0, len(all))
	byID := make(map[int64]Project, len(all))
	for _, p := range all {
		byID[p.ID] = p
	}
	isVisible := func(p Project) bool {
		if p.GroupID == 0 || p.OwnerEmail == email {
			return true
		}
		var member bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM group_members WHERE group_id = $1 AND email = $2)`,
			p.GroupID, email).Scan(&member); err != nil {
			log.Printf("loadVisibleProjects member check: %v", err)
			return false
		}
		return member
	}
	for _, p := range all {
		if isVisible(p) {
			visible = append(visible, p)
		}
	}

	// Добавляем предков видимых проектов (рекурсивно до корня).
	seen := make(map[int64]bool, len(visible))
	for _, p := range visible {
		seen[p.ID] = true
	}
	for _, p := range visible {
		parent := p.ParentID
		for parent > 0 && !seen[parent] {
			if ancestor, ok := byID[parent]; ok {
				visible = append(visible, ancestor)
				seen[parent] = true
				parent = ancestor.ParentID
			} else {
				break
			}
		}
	}

	// Сортировка по id, стабильная к порядку добавления предков.
	sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
	return visible, nil
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.loadVisibleProjects(r.Context(), currentEmail(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParentID          int64  `json:"parent_id"`
		Code              string `json:"code"`
		Name              string `json:"name"`
		Description       string `json:"description"`
		IsEkn             bool   `json:"is_ekn"`
		GroupID           int64  `json:"group_id"`
		MailTriggerEkn    string `json:"mail_trigger_ekn"`
		MailTriggerSender string `json:"mail_trigger_sender"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	req.Code = strings.ToUpper(strings.TrimSpace(req.Code))
	req.MailTriggerEkn = strings.TrimSpace(req.MailTriggerEkn)
	req.MailTriggerSender = strings.ToLower(strings.TrimSpace(req.MailTriggerSender))
	if req.Code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code is required"})
		return
	}
	if req.ParentID > 0 {
		var parentExists bool
		if err := s.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)`, req.ParentID).Scan(&parentExists); err != nil || !parentExists {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent project not found"})
			return
		}
	}
	if req.GroupID > 0 {
		var groupExists bool
		if err := s.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1)`, req.GroupID).Scan(&groupExists); err != nil || !groupExists {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "group not found"})
			return
		}
	}
	email := currentEmail(r)
	var id int64
	var parentID any
	if req.ParentID > 0 {
		parentID = req.ParentID
	}
	err := s.pool.QueryRow(r.Context(), `
INSERT INTO projects (parent_id, code, name, description, is_ekn, group_id, owner_email, mail_trigger_ekn, mail_trigger_sender)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (code) DO NOTHING RETURNING id`,
		parentID, req.Code, req.Name, req.Description, req.IsEkn, nullableID(req.GroupID), email,
		req.MailTriggerEkn, req.MailTriggerSender).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
		return
	}
	if err != nil {
		log.Printf("create project: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		ParentID          *int64  `json:"parent_id"`
		Code              string  `json:"code"`
		Name              string  `json:"name"`
		Description       string  `json:"description"`
		IsEkn             *bool   `json:"is_ekn"`
		GroupID           *int64  `json:"group_id"`
		MailTriggerEkn    *string `json:"mail_trigger_ekn"`
		MailTriggerSender *string `json:"mail_trigger_sender"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.MailTriggerEkn != nil {
		trimmed := strings.TrimSpace(*req.MailTriggerEkn)
		req.MailTriggerEkn = &trimmed
	}
	if req.MailTriggerSender != nil {
		trimmed := strings.ToLower(strings.TrimSpace(*req.MailTriggerSender))
		req.MailTriggerSender = &trimmed
	}

	if req.GroupID != nil && *req.GroupID > 0 {
		var groupExists bool
		if err := s.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1)`, *req.GroupID).Scan(&groupExists); err != nil || !groupExists {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "group not found"})
			return
		}
	}

	var ownerEmail string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT owner_email FROM projects WHERE id = $1`, id).Scan(&ownerEmail); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	email := currentEmail(r)
	role, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if ownerEmail != email && roleRank(role) < roleRank("admin") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not owner"})
		return
	}

	// Смена родительского проекта (2026-09-02, «ЦУП Веб» — первый клиент, вообще
	// позволяющий менять parent_id ПОСЛЕ создания; плагин задаёт его только при
	// создании, поэтому раньше эта проверка была не нужна). Без неё проект можно
	// сделать потомком самого себя или своего же потомка — цикл в дереве.
	if req.ParentID != nil && *req.ParentID > 0 {
		if *req.ParentID == id {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "project cannot be its own parent"})
			return
		}
		descendants, err := s.projectAndDescendants(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		for _, d := range descendants {
			if d == *req.ParentID {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot move project into itself or its descendant"})
				return
			}
		}
	}

	req.Code = strings.ToUpper(strings.TrimSpace(req.Code))
	if req.Code != "" {
		var taken bool
		if err := s.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM projects WHERE code = $1 AND id <> $2)`, req.Code, id).Scan(&taken); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if taken {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
			return
		}
	}

	_, err = s.pool.Exec(r.Context(), `
UPDATE projects SET
	parent_id = CASE WHEN $2 = 0 THEN NULL ELSE COALESCE($2, parent_id) END,
	code = CASE WHEN $3 = '' THEN code ELSE $3 END,
	name = CASE WHEN $4 = '' THEN name ELSE $4 END,
	description = $5,
	is_ekn = COALESCE($6, is_ekn),
	group_id = CASE WHEN $7 = 0 THEN NULL ELSE COALESCE($7, group_id) END,
	mail_trigger_ekn = COALESCE($8, mail_trigger_ekn),
	mail_trigger_sender = COALESCE($9, mail_trigger_sender),
	updated_at = now()
WHERE id = $1`, id, req.ParentID, req.Code, req.Name, req.Description, req.IsEkn, req.GroupID,
		req.MailTriggerEkn, req.MailTriggerSender)
	if err != nil {
		log.Printf("update project: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// projectAndDescendants возвращает id проекта и всех его потомков (для защиты
// от цикла при смене parent_id — зеркало photo-service folderAndDescendants).
func (s *Server) projectAndDescendants(ctx context.Context, rootID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, parent_id FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := map[int64][]int64{}
	for rows.Next() {
		var id int64
		var parent *int64
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		if parent != nil {
			children[*parent] = append(children[*parent], id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := []int64{rootID}
	queue := []int64{rootID}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			ids = append(ids, c)
			queue = append(queue, c)
		}
	}
	return ids, nil
}
