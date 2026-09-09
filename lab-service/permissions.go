package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
)

func ownerEmailFromEnv() string {
	return os.Getenv("LAB_OWNER_EMAIL")
}

// handleMyPermission возвращает роль текущего пользователя (по JWT email).
// real_role — реальная роль БЕЗ учёта активного «просмотра от лица роли»:
// клиенту нужна она, чтобы решить, показывать ли переключатель ролей — если
// брать «role» (уже возможно подменённую), суперадмин, включивший просмотр
// как viewer, потерял бы сам переключатель.
func (s *Server) handleMyPermission(w http.ResponseWriter, r *http.Request) {
	email := currentEmail(r)
	if email == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	rawRole, err := s.rawRole(r.Context(), appIDFromEnv(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	role, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if role == "" {
		writeJSON(w, http.StatusOK, map[string]any{"email": email, "role": "", "real_role": rawRole, "hasAccess": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"email": email, "role": role, "real_role": rawRole, "hasAccess": true})
}

// handleListPermissions возвращает все глобальные роли (для admin).
// С 2026-09-09 источник правды — auth-service: читаем карту прав приложения,
// локальная таблица lab_permissions больше не используется (см. authperms.go).
func (s *Server) handleListPermissions(w http.ResponseWriter, r *http.Request) {
	snap := s.permissionsSnapshot(r.Context())
	type perm struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	perms := make([]perm, 0, len(snap.Roles))
	for email, role := range snap.Roles {
		perms = append(perms, perm{Email: email, Role: role})
	}
	sort.Slice(perms, func(i, j int) bool { return perms[i].Email < perms[j].Email })
	writeJSON(w, http.StatusOK, map[string]any{"permissions": perms})
}

// handleSetPermission устанавливает роль ({email, role}); role="" — удаляет доступ.
func (s *Server) handleSetPermission(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	// ToLower (2026-09-03, живой баг): auth-service всегда приводит email к
	// нижнему регистру при входе (magic_link.go/main.go) — если здесь сохранить
	// как ввёл админ (с заглавной буквой), назначенная роль никогда не совпадёт
	// с currentEmail(r) при проверке и молча не подействует. См. AGENTS.md.
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Role = strings.TrimSpace(req.Role)
	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email is required"})
		return
	}
	if req.Role != "" && req.Role != "viewer" && req.Role != "editor" && req.Role != "admin" && req.Role != "superadmin" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role must be viewer, editor, admin or superadmin"})
		return
	}
	// Назначать/снимать superadmin может только действующий superadmin (иначе admin
	// мог бы повысить себя или кого угодно выше своего уровня через тот же admin-only роут).
	if req.Role == "superadmin" {
		actorRole, err := s.effectiveRole(r.Context(), appIDFromEnv(), currentEmail(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if roleRank(actorRole) < roleRank("superadmin") {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: superadmin required"})
			return
		}
	}

	if req.Role == "" || (req.Role != "admin" && req.Role != "superadmin") {
		owner := ownerEmailFromEnv()
		if req.Email == owner {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "нельзя отозвать доступ владельца"})
			return
		}
	}

	// Запись уходит в auth-service — единственный источник правды с 2026-09-09;
	// право «админ приложения» проверяет он же, по центральной таблице.
	if err := setPermissionUpstream(r.Context(), currentEmail(r), req.Email, req.Role); err != nil {
		log.Printf("set permission upstream: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "не удалось сохранить права: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGetCommonAccess возвращает общий уровень доступа.
// Пустой уровень отдаётся как есть — это «общий доступ закрыт», а не
// «настройка не задана» (на подмене пустого значения на viewer погорел
// Фотобанк, см. его AGENTS.md за 2026-09-09).
func (s *Server) handleGetCommonAccess(w http.ResponseWriter, r *http.Request) {
	snap := s.permissionsSnapshot(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"level": snap.CommonAccess})
}

// handleSetCommonAccess устанавливает общий уровень доступа ({level}).
// Запись уходит в auth-service — единственный источник правды; право «админ
// приложения» проверяет он же.
func (s *Server) handleSetCommonAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if err := setCommonAccessUpstream(r.Context(), currentEmail(r), strings.TrimSpace(req.Level)); err != nil {
		log.Printf("set common access upstream: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "не удалось сохранить общий доступ: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
