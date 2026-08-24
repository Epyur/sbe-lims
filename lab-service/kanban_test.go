package main

import "testing"

const (
	headEmail    = "head@lab.ru"
	testerAEmail = "testerA@lab.ru"
	testerBEmail = "testerB@lab.ru"
)

func TestCanApplyKanbanMoveLabHeadAlwaysAllowed(t *testing.T) {
	cases := []struct {
		oldStatus, newStatus, oldAssignedTo, newAssignedTo string
	}{
		{"new", "received", "", testerAEmail},
		{"received", "processing", testerAEmail, testerBEmail},
		{"processing", "new", testerAEmail, ""},
		{"completed", "processing", testerAEmail, testerAEmail},
	}
	for _, c := range cases {
		ok, reason := canApplyKanbanMove(headEmail, "admin", "", c.oldStatus, c.newStatus, c.oldAssignedTo, c.newAssignedTo)
		if !ok {
			t.Errorf("head move %+v: got forbidden (%s), want allowed", c, reason)
		}
	}
}

func TestCanApplyKanbanMoveLabAdminActsAsLabHead(t *testing.T) {
	// lab_admin ИМЕННО этой лабы (без глобальной admin-роли) — свободно, как
	// руководитель: переназначение, движение чужих карточек, переоткрытие
	// завершённых (2026-08-24, делегированные полномочия).
	cases := []struct {
		oldStatus, newStatus, oldAssignedTo, newAssignedTo string
	}{
		{"new", "received", "", testerAEmail},
		{"received", "processing", testerAEmail, testerBEmail},
		{"completed", "processing", testerAEmail, testerAEmail},
	}
	for _, c := range cases {
		ok, reason := canApplyKanbanMove(headEmail, "", "lab_admin", c.oldStatus, c.newStatus, c.oldAssignedTo, c.newAssignedTo)
		if !ok {
			t.Errorf("lab_admin move %+v: got forbidden (%s), want allowed", c, reason)
		}
	}
}

func TestCanApplyKanbanMoveTesterCannotAssign(t *testing.T) {
	// Заявка уже назначена testerA; testerA пытается переназначить её testerB.
	ok, reason := canApplyKanbanMove(testerAEmail, "", "lab_operator", "received", "received", testerAEmail, testerBEmail)
	if ok {
		t.Fatal("tester reassigning to someone else must be forbidden")
	}
	if reason == "" {
		t.Error("expected a reason for the forbidden move")
	}
}

func TestCanApplyKanbanMoveTesterMovesOwnCardBetweenCells(t *testing.T) {
	cases := []struct{ oldStatus, newStatus string }{
		{"received", "processing"},
		{"processing", "received"},
		{"received", "completed"},
		{"processing", "completed"},
	}
	for _, c := range cases {
		ok, reason := canApplyKanbanMove(testerAEmail, "", "lab_operator", c.oldStatus, c.newStatus, testerAEmail, testerAEmail)
		if !ok {
			t.Errorf("tester own-card move %s->%s: got forbidden (%s), want allowed", c.oldStatus, c.newStatus, reason)
		}
	}
}

func TestCanApplyKanbanMoveTesterCannotTouchUnassignedOrForeignCard(t *testing.T) {
	// Назначена testerB, testerA пытается подвинуть статус.
	if ok, _ := canApplyKanbanMove(testerAEmail, "", "lab_operator", "received", "processing", testerBEmail, testerBEmail); ok {
		t.Error("tester must not move a card assigned to someone else")
	}
}

func TestCanApplyKanbanMoveTesterCannotPullFromNewOrReopenCompleted(t *testing.T) {
	// Уже назначена testerA, но статус "new" (гипотетическое несогласованное состояние) —
	// не разрешаем менять статус без прохождения через самозабор-ветку.
	if ok, _ := canApplyKanbanMove(testerAEmail, "", "lab_operator", "new", "received", testerAEmail, testerAEmail); ok {
		t.Error("tester must not move out of 'new' via the own-card path (only self-pickup, with oldAssignedTo=='')")
	}
	if ok, _ := canApplyKanbanMove(testerAEmail, "", "lab_operator", "completed", "processing", testerAEmail, testerAEmail); ok {
		t.Error("tester must not reopen a completed request")
	}
}

func TestCanApplyKanbanMoveTesterSelfPickupFromNew(t *testing.T) {
	ok, reason := canApplyKanbanMove(testerAEmail, "", "lab_operator", "new", "received", "", testerAEmail)
	if !ok {
		t.Fatalf("tester self-pickup from new must be allowed, got forbidden: %s", reason)
	}
}

func TestCanApplyKanbanMoveTesterCannotPickUpForSomeoneElse(t *testing.T) {
	ok, _ := canApplyKanbanMove(testerAEmail, "", "lab_operator", "new", "received", "", testerBEmail)
	if ok {
		t.Fatal("tester must not be able to assign an unclaimed new request to someone else")
	}
}

func TestCanApplyKanbanMoveNonMemberDenied(t *testing.T) {
	// Глобальная роль editor, но не lab_operator/lab_admin этой лабы.
	ok, _ := canApplyKanbanMove(testerAEmail, "editor", "", "received", "processing", testerAEmail, testerAEmail)
	if ok {
		t.Fatal("a non lab_operator/lab_admin must not be able to move cards, regardless of global role")
	}
}

func TestNormalizeKanbanTargetClearsAssignedToOnNew(t *testing.T) {
	status := "new"
	newStatus, newAssignedTo := normalizeKanbanTarget("received", testerAEmail, kanbanMoveRequest{Status: &status})
	if newStatus != "new" || newAssignedTo != "" {
		t.Fatalf("got status=%q assigned_to=%q, want new/''", newStatus, newAssignedTo)
	}
}

func TestNormalizeKanbanTargetKeepsExistingWhenFieldOmitted(t *testing.T) {
	assignedTo := testerBEmail
	newStatus, newAssignedTo := normalizeKanbanTarget("processing", testerAEmail, kanbanMoveRequest{AssignedTo: &assignedTo})
	if newStatus != "processing" {
		t.Fatalf("omitted status must be kept as-is, got %q", newStatus)
	}
	if newAssignedTo != testerBEmail {
		t.Fatalf("got assigned_to=%q, want %q", newAssignedTo, testerBEmail)
	}
}
