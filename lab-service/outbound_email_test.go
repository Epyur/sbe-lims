package main

import (
	"strings"
	"testing"
)

// shouldAutoSend — единственная часть outbound_email.go без обращения к БД/SMTP (в
// проекте нет мок-инфраструктуры для этого, см. AGENTS.md) — резолв лабы/журнала и сама
// отправка проверяются живым E2E, не юнит-тестами.

func TestShouldAutoSendLabDisabled(t *testing.T) {
	sendCustomer, sendLpitrack := shouldAutoSend(false, true, false, false)
	if sendCustomer || sendLpitrack {
		t.Fatalf("lab auto-send disabled: expected nothing sent, got customer=%v lpitrack=%v", sendCustomer, sendLpitrack)
	}
}

func TestShouldAutoSendFreshCompletionWithExternalID(t *testing.T) {
	sendCustomer, sendLpitrack := shouldAutoSend(true, true, false, false)
	if !sendCustomer || !sendLpitrack {
		t.Fatalf("expected both sent, got customer=%v lpitrack=%v", sendCustomer, sendLpitrack)
	}
}

func TestShouldAutoSendFreshCompletionWithoutExternalID(t *testing.T) {
	sendCustomer, sendLpitrack := shouldAutoSend(true, false, false, false)
	if !sendCustomer {
		t.Fatal("expected customer email to be sent even without external_id")
	}
	if sendLpitrack {
		t.Fatal("expected no lpitrack dup when external_id is empty")
	}
}

func TestShouldAutoSendAlreadySent(t *testing.T) {
	sendCustomer, sendLpitrack := shouldAutoSend(true, true, true, true)
	if sendCustomer || sendLpitrack {
		t.Fatalf("expected no re-send when already logged as sent, got customer=%v lpitrack=%v", sendCustomer, sendLpitrack)
	}
}

// customerEmailContent — живая жалоба пользователя (2026-08-28): заказчик узнаёт
// заявку переходного периода по external_id, не по внутреннему номеру ЛИМС — письмо
// должно называть оба, external_id первым.
func TestCustomerEmailContentWithExternalID(t *testing.T) {
	req := Request{CustomerNumber: "1/2026-РП", ExternalID: "999999"}
	subject, body := customerEmailContent(req)
	if !strings.Contains(subject, "999999") {
		t.Errorf("subject должен содержать external_id, got %q", subject)
	}
	if !strings.Contains(body, "999999") {
		t.Errorf("body должен содержать external_id, got %q", body)
	}
	if !strings.Contains(body, "1/2026-РП") {
		t.Errorf("body должен содержать учётный номер ЛИМС (CustomerNumber) даже при наличии external_id, got %q", body)
	}
}

func TestCustomerEmailContentWithoutExternalID(t *testing.T) {
	req := Request{CustomerNumber: "1/2026-РП", ExternalID: ""}
	subject, body := customerEmailContent(req)
	if !strings.Contains(subject, "1/2026-РП") {
		t.Errorf("subject должен содержать CustomerNumber, got %q", subject)
	}
	if !strings.Contains(body, "1/2026-РП") {
		t.Errorf("body должен содержать CustomerNumber, got %q", body)
	}
}

func TestShouldAutoSendPartiallyAlreadySent(t *testing.T) {
	// заказчику уже ушло (напр. предыдущий авто-триггер), lpitrack ещё нет (напр.
	// external_id появился только сейчас, задним числом) — должно долать только его.
	sendCustomer, sendLpitrack := shouldAutoSend(true, true, true, false)
	if sendCustomer {
		t.Fatal("expected no re-send to customer")
	}
	if !sendLpitrack {
		t.Fatal("expected lpitrack dup to still be sent")
	}
}

// shouldAutoSendProcessing (2026-08-29, WP2 продолжение — уведомление в LPITrack
// при переходе заявки в processing, тот же переключатель labs.auto_send_email).

func TestShouldAutoSendProcessingLabDisabled(t *testing.T) {
	if shouldAutoSendProcessing(false, true, false) {
		t.Fatal("lab auto-send disabled: expected no processing email")
	}
}

func TestShouldAutoSendProcessingNoExternalID(t *testing.T) {
	if shouldAutoSendProcessing(true, false, false) {
		t.Fatal("no external_id: expected no processing email (nothing to match in LPITrack)")
	}
}

func TestShouldAutoSendProcessingFresh(t *testing.T) {
	if !shouldAutoSendProcessing(true, true, false) {
		t.Fatal("expected processing email to be sent on first transition")
	}
}

func TestShouldAutoSendProcessingAlreadySent(t *testing.T) {
	if shouldAutoSendProcessing(true, true, true) {
		t.Fatal("expected no re-send when already logged as sent (e.g. received<->processing toggling)")
	}
}

// legacyExternalKey (2026-08-29) — тема служебного письма должна совпадать с полем
// «Внешний идентификатор», показанным пользователю (LPIZAYAVKINAPRO-<N>), а не
// голым external_id.
func TestLegacyExternalKey(t *testing.T) {
	if got := legacyExternalKey("775"); got != "LPIZAYAVKINAPRO-775" {
		t.Fatalf("got %q, want LPIZAYAVKINAPRO-775", got)
	}
	if got := legacyExternalKey(""); got != "" {
		t.Fatalf("empty external_id should stay empty, got %q", got)
	}
}
