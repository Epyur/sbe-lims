package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// POST /api/lab/requests/{id}/kanban-move — единая точка входа и для drag-and-
// drop на Kanban-доске sbe-lims ("Очередь лаборатории"), и для контролов в
// детали заявки — обе идут через canApplyKanbanMove, чтобы правило "руководитель
// лабы — свободно, испытатель — только свою уже назначенную карточку (плюс
// самозабор неназначенной из 'new' себе)" исполнялось идентично независимо от
// способа взаимодействия.
//
// Старый POST /requests/{id}/status (requests.go) НЕ трогаем — его использует
// также sbe-requests для смены статуса владельцем заявки; ужесточение его
// авторизации сломало бы этот сценарий. Инварианты assigned_to/completed_at
// поддерживаются во всех местах записи requests.status (см. handleSetRequestStatus,
// pushUpdate), не только здесь — иначе 10-рабочедневное окно колонки
// "Завершённые" работало бы непредсказуемо в зависимости от того, кто менял статус.

type kanbanMoveRequest struct {
	Status     *string `json:"status"`
	AssignedTo *string `json:"assigned_to"`
}

var validRequestStatuses = map[string]bool{"new": true, "received": true, "processing": true, "completed": true}

// normalizeKanbanTarget — nil-поле патча = "не меняем"; переход в status="new"
// безусловно чистит assigned_to (у "новых" заявок ячеек-исполнителей нет).
func normalizeKanbanTarget(oldStatus, oldAssignedTo string, patch kanbanMoveRequest) (newStatus, newAssignedTo string) {
	newStatus = oldStatus
	if patch.Status != nil {
		newStatus = strings.TrimSpace(*patch.Status)
	}
	newAssignedTo = oldAssignedTo
	if patch.AssignedTo != nil {
		newAssignedTo = strings.TrimSpace(*patch.AssignedTo)
	}
	if newStatus == "new" {
		newAssignedTo = ""
	}
	return newStatus, newAssignedTo
}

// canApplyKanbanMove — без БД (роли резолвит вызывающая сторона), юнит-тестируется
// напрямую:
//  1. Руководитель лабы — глобальная роль admin/superadmin, ЛИБО lab_admin
//     ИМЕННО этой лабы (2026-08-24, делегированные полномочия: lab_admin теперь
//     полноценный руководитель своей лабы в канбане, не синоним lab_operator) —
//     разрешено всё.
//  2. Испытатель (lab_operator ИМЕННО этой лабы):
//     a. Самозабор: неназначенную заявку из "новых" (oldStatus=="new",
//        oldAssignedTo=="") может забрать СЕБЕ (newAssignedTo==actorEmail) в
//        статус "received" — и только себе, не кому-то другому.
//     b. Иначе не может менять assigned_to вовсе (переназначение — только
//        руководитель).
//     c. Может менять status, только если заявка уже назначена ему
//        (oldAssignedTo==actorEmail), старый статус — received/processing,
//        новый — received/processing/completed (не может переоткрыть
//        завершённую).
//  3. Остальные — запрещено.
func canApplyKanbanMove(actorEmail, actorGlobalRole, actorLabRole, oldStatus, newStatus, oldAssignedTo, newAssignedTo string) (bool, string) {
	if roleRank(actorGlobalRole) >= roleRank("admin") || actorLabRole == "lab_admin" {
		return true, ""
	}
	if actorLabRole != "lab_operator" {
		return false, "forbidden: not a lab_operator/lab_admin of this lab"
	}
	if oldStatus == "new" && oldAssignedTo == "" {
		if newStatus == "received" && newAssignedTo == actorEmail {
			return true, ""
		}
		return false, "forbidden: a tester may only self-assign a new request into their own cell"
	}
	if newAssignedTo != oldAssignedTo {
		return false, "forbidden: only the lab head can assign/reassign a request"
	}
	if oldAssignedTo != actorEmail {
		return false, "forbidden: this request is not assigned to you"
	}
	if oldStatus != "received" && oldStatus != "processing" {
		return false, "forbidden: cannot move a card out of new/completed as a tester"
	}
	if newStatus != "received" && newStatus != "processing" && newStatus != "completed" {
		return false, "forbidden: invalid target status for a tester"
	}
	return true, ""
}

func (s *Server) handleKanbanMove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var patch kanbanMoveRequest
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if patch.Status == nil && patch.AssignedTo == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one of status or assigned_to is required"})
		return
	}
	if patch.Status != nil && !validRequestStatuses[strings.TrimSpace(*patch.Status)] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status must be new, received, processing or completed"})
		return
	}

	existing, err := s.loadRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	newStatus, newAssignedTo := normalizeKanbanTarget(existing.Status, existing.AssignedTo, patch)

	email := currentEmail(r)
	actorGlobalRole, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	labID, err := s.requestLabID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	actorLabRole, err := s.labMemberRole(r.Context(), email, labID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if ok, reason := canApplyKanbanMove(email, actorGlobalRole, actorLabRole, existing.Status, newStatus, existing.AssignedTo, newAssignedTo); !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": reason})
		return
	}

	if newAssignedTo != "" && newAssignedTo != existing.AssignedTo {
		targetRole, err := s.labMemberRole(r.Context(), newAssignedTo, labID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if targetRole != "lab_operator" && targetRole != "lab_admin" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "assigned_to must be a lab_operator/lab_admin of this request's lab"})
			return
		}
	}

	_, err = s.pool.Exec(r.Context(), `
UPDATE requests SET
	status = $2,
	assigned_to = $3,
	completed_at = CASE
		WHEN $2 = 'completed' AND status <> 'completed' THEN now()
		WHEN $2 <> 'completed' THEN NULL
		ELSE completed_at
	END,
	updated_at = now()
WHERE id = $1`, id, newStatus, newAssignedTo)
	if err != nil {
		log.Printf("kanban move: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	full, err := s.loadRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": full})
}
