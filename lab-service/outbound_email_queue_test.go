package main

import (
	"strings"
	"testing"
)

func TestCompletionTargets(t *testing.T) {
	req := Request{OwnerEmail: "c@tn.ru", ExternalID: "777", CustomerNumber: "1/2026-ГГ"}
	got := completionTargets(req, "s@tn.ru")
	if len(got) != 2 || got[0].RecipientType != "customer" || got[1].RecipientType != "lpitrack" {
		t.Fatalf("targets: %+v", got)
	}
	if !got[0].WithAttachment || !got[1].WithAttachment {
		t.Fatalf("completion targets must carry attachment")
	}
	if only := completionTargets(Request{OwnerEmail: "c@tn.ru"}, "s@tn.ru"); len(only) != 1 {
		t.Fatalf("without external_id: %+v", only)
	}
	if none := completionTargets(Request{}, ""); len(none) != 0 {
		t.Fatalf("empty request: %+v", none)
	}
	if noService := completionTargets(Request{OwnerEmail: "c@tn.ru", ExternalID: "1"}, ""); len(noService) != 1 {
		t.Fatalf("without service email: %+v", noService)
	}
}

func TestProcessingTarget(t *testing.T) {
	req := Request{ExternalID: "777", CustomerNumber: "1/2026-ГГ"}
	got := processingTarget(req, "s@tn.ru")
	if len(got) != 1 || got[0].RecipientType != "lpitrack_processing" || got[0].WithAttachment {
		t.Fatalf("processing: %+v", got)
	}
	if len(processingTarget(Request{}, "s@tn.ru")) != 0 {
		t.Fatalf("without external_id must be empty")
	}
	if len(processingTarget(Request{ExternalID: "1"}, "")) != 0 {
		t.Fatalf("without service email must be empty")
	}
}

func TestFilterCompletionTargets(t *testing.T) {
	all := []outboundTarget{
		{RecipientType: "customer", Address: "c@tn.ru", WithAttachment: true},
		{RecipientType: "lpitrack", Address: "s@tn.ru", WithAttachment: true},
	}
	if got := filterCompletionTargets(all, false, true); len(got) != 1 || got[0].RecipientType != "lpitrack" {
		t.Fatalf("lpitrack only: %+v", got)
	}
	if got := filterCompletionTargets(all, true, false); len(got) != 1 || got[0].RecipientType != "customer" {
		t.Fatalf("customer only: %+v", got)
	}
	if got := filterCompletionTargets(all, false, false); len(got) != 0 {
		t.Fatalf("none: %+v", got)
	}
}

func TestNextOutboundStatus(t *testing.T) {
	cases := []struct {
		attempts int
		want     string
	}{
		{0, "pending"}, {3, "pending"}, {4, "failed"}, {9, "failed"},
	}
	for _, c := range cases {
		if got := nextOutboundStatus(c.attempts); got != c.want {
			t.Errorf("attempts=%d: got %q want %q", c.attempts, got, c.want)
		}
	}
}

func TestOutboundPayload(t *testing.T) {
	req := Request{OwnerEmail: "c@tn.ru", ExternalID: "777", CustomerNumber: "1/2026-ГГ", AssignedTo: "i@tn.ru"}
	subj, body := outboundPayload(req, "customer")
	if subj != "Результаты испытания №777" || body == "" {
		t.Fatalf("customer: %q / %q", subj, body)
	}
	if subj, _ := outboundPayload(req, "lpitrack"); subj != "LPIZAYAVKINAPRO-777" {
		t.Fatalf("lpitrack subject: %q", subj)
	}
	_, body = outboundPayload(req, "lpitrack_processing")
	if !containsAll(body, "взята в работу", "i@tn.ru") {
		t.Fatalf("processing body: %q", body)
	}
	req.AssignedTo = ""
	_, body = outboundPayload(req, "lpitrack_processing")
	if !strings.Contains(body, "не назначен") {
		t.Fatalf("processing body without assignee: %q", body)
	}
}
