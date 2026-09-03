package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type GroupMember struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type Group struct {
	ID         int64         `json:"id"`
	Name       string        `json:"name"`
	OwnerEmail string        `json:"owner_email"`
	Members    []GroupMember `json:"members"`
	CreatedAt  string        `json:"created_at"`
	UpdatedAt  string        `json:"updated_at"`
}

// loadGroupMembers загружает участников группы.
func (s *Server) loadGroupMembers(ctx context.Context, groupID int64) ([]GroupMember, error) {
	rows, err := s.pool.Query(ctx, `
SELECT email, role FROM group_members WHERE group_id = $1 ORDER BY email`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members := make([]GroupMember, 0, 16)
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.Email, &m.Role); err != nil {
			log.Printf("group members scan: %v", err)
			continue
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return members, nil
}

// handleListGroups — мои группы + группы, где я участник.
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	email := currentEmail(r)
	rows, err := s.pool.Query(r.Context(), `
SELECT id, name, owner_email, created_at, updated_at
FROM groups
WHERE owner_email = $1 OR id IN (SELECT group_id FROM group_members WHERE email = $1)
ORDER BY id`, email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()

	groups := make([]Group, 0, 16)
	for rows.Next() {
		var g Group
		var ca, ua time.Time
		if err := rows.Scan(&g.ID, &g.Name, &g.OwnerEmail, &ca, &ua); err != nil {
			log.Printf("groups scan: %v", err)
			continue
		}
		g.CreatedAt = ca.Format(time.RFC3339)
		g.UpdatedAt = ua.Format(time.RFC3339)
		if g.Members, err = s.loadGroupMembers(r.Context(), g.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	var id int64
	err := s.pool.QueryRow(r.Context(), `
INSERT INTO groups (name, owner_email) VALUES ($1, $2) RETURNING id`,
		req.Name, currentEmail(r)).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// groupOwnerChecks — проверка владельца группы (владелец ИЛИ admin) и загрузка owner.
func (s *Server) groupOwnerChecks(ctx context.Context, r *http.Request, groupID int64) (string, bool) {
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT owner_email FROM groups WHERE id = $1`, groupID).Scan(&owner)
	if err != nil {
		return "", false
	}
	email := currentEmail(r)
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return "", false
	}
	if owner == email || roleRank(role) >= roleRank("admin") {
		return owner, true
	}
	return owner, false
}

func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	groupID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	// ToLower (2026-09-03, живой баг): auth-service всегда приводит email к
	// нижнему регистру при входе — участник, добавленный с заглавной буквой,
	// никогда не проходил регистрозависимую проверку членства (loadVisibleProjects,
	// visibleRequestsQuery, handleListGroups и т.д.) и молча не видел ни группу,
	// ни её проекты/заявки/лаборатории. См. AGENTS.md.
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Role = strings.TrimSpace(req.Role)
	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email is required"})
		return
	}
	if req.Role != "" && req.Role != "viewer" && req.Role != "editor" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role must be viewer or editor"})
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}
	if _, ok := s.groupOwnerChecks(r.Context(), r, groupID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not group owner"})
		return
	}
	_, err = s.pool.Exec(r.Context(), `
INSERT INTO group_members (group_id, email, role) VALUES ($1, $2, $3)
ON CONFLICT (group_id, email) DO UPDATE SET role = EXCLUDED.role`,
		groupID, req.Email, req.Role)
	if err != nil {
		log.Printf("add member: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	groupID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	// ToLower — см. handleAddGroupMember; email всегда хранится в нижнем регистре.
	email := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	if email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email is required"})
		return
	}
	owner, ok := s.groupOwnerChecks(r.Context(), r, groupID)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not group owner"})
		return
	}
	if email == owner {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "нельзя удалить владельца группы"})
		return
	}
	_, err = s.pool.Exec(r.Context(), `
DELETE FROM group_members WHERE group_id = $1 AND email = $2`, groupID, email)
	if err != nil {
		log.Printf("remove member: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
