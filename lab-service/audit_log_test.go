package main

import "testing"

// shouldLogStatusChange/resultSaveKind — единственные части audit_log.go без
// обращения к БД (в проекте нет мок-инфраструктуры для этого, см. AGENTS.md) —
// сама запись/чтение журнала проверяется живым E2E, не юнит-тестами.

func TestShouldLogStatusChangeRealTransition(t *testing.T) {
	if !shouldLogStatusChange("new", "processing") {
		t.Fatal("expected a real status transition to be logged")
	}
}

func TestShouldLogStatusChangeNoOp(t *testing.T) {
	if shouldLogStatusChange("processing", "processing") {
		t.Fatal("expected no log entry when old and new status are the same")
	}
}

func TestResultSaveKindCreated(t *testing.T) {
	if got := resultSaveKind(nil); got != "result_created" {
		t.Fatalf("got %q, want result_created", got)
	}
}

func TestResultSaveKindUpdated(t *testing.T) {
	if got := resultSaveKind(map[string]any{}); got != "result_updated" {
		t.Fatalf("got %q, want result_updated", got)
	}
	if got := resultSaveKind(map[string]any{"foo": "bar"}); got != "result_updated" {
		t.Fatalf("got %q, want result_updated", got)
	}
}

func TestShouldAutoTransitionToProcessing(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"new", true},
		{"received", true},
		{"processing", false},
		{"completed", false},
	} {
		if got := shouldAutoTransitionToProcessing(tc.status); got != tc.want {
			t.Errorf("shouldAutoTransitionToProcessing(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
