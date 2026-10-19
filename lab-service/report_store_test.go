package main

import (
	"testing"
	"time"
)

func TestReportIsFresh(t *testing.T) {
	gen := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	before := gen.Add(-time.Hour)
	after := gen.Add(time.Hour)
	if !reportIsFresh(gen, "ok", before) {
		t.Error("fresh report must be reused")
	}
	if reportIsFresh(gen, "ok", after) {
		t.Error("stale report must be regenerated")
	}
	if reportIsFresh(gen, "failed", before) {
		t.Error("failed AI state must not be treated as complete")
	}
	if !reportIsFresh(gen, "none", before) {
		t.Error("method without AI instruction is complete")
	}
	if reportIsFresh(time.Time{}, "ok", before) {
		t.Error("missing generated_at is not fresh")
	}
	if !reportIsFresh(gen, "ok", gen) {
		t.Error("report generated exactly at last change is fresh")
	}
}

func TestReportAIState(t *testing.T) {
	cases := []struct {
		name                  string
		hasInstruction, hasAI bool
		want                  string
	}{
		{"no instruction", false, false, "none"},
		{"ai ok", true, true, "ok"},
		{"ai empty or failed", true, false, "failed"},
	}
	for _, c := range cases {
		if got := reportAIState(c.hasInstruction, c.hasAI); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
