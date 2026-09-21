package main

import "testing"

// TestBuildNumbersFormat закрепляет формат пары номеров. Формула продублирована
// в TypeScript для предпросмотра при смене проекта (sbe-core/src/utils/
// request-number.ts и копия в web/sbe-web) — этот тест ловит расхождение со
// стороны Go: поменяли формат здесь, тест покраснел, значит надо править и обе
// копии (их между собой сверяет scripts/check_request_number.mjs).
func TestBuildNumbersFormat(t *testing.T) {
	cases := []struct {
		name                        string
		seq                         int64
		year                        int
		projectCode                 string
		labCode                     string
		methodCode                  string
		wantCustomer, wantLabNumber string
	}{
		{
			name: "обычная заявка",
			seq:  5, year: 2026, projectCode: "ПР1", labCode: "ЛГ", methodCode: "30244",
			wantCustomer: "ПР1-5/2026-ЛГ-30244", wantLabNumber: "5/2026-30244",
		},
		{
			name: "без проекта — код 0, как в loadProjectInfo",
			seq:  12, year: 2026, projectCode: "0", labCode: "ЛГ", methodCode: "30244",
			wantCustomer: "0-12/2026-ЛГ-30244", wantLabNumber: "12/2026-30244",
		},
		{
			name: "без лаборатории — пустой кусок, но номер собирается",
			seq:  7, year: 2025, projectCode: "ПР2", labCode: "", methodCode: "30403",
			wantCustomer: "ПР2-7/2025--30403", wantLabNumber: "7/2025-30403",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			customer, lab := buildNumbers(c.seq, c.year, c.projectCode, c.labCode, c.methodCode)
			if customer != c.wantCustomer {
				t.Errorf("customer = %q, want %q", customer, c.wantCustomer)
			}
			if lab != c.wantLabNumber {
				t.Errorf("lab = %q, want %q", lab, c.wantLabNumber)
			}
		})
	}
}

// TestBuildNumbersProjectCodeOnlyAffectsCustomer — смысл пересчёта при смене
// проекта: меняется ТОЛЬКО номер заказчику, номер лаборатории остаётся прежним
// (кода проекта в нём нет). Из-за этого rebuildCustomerNumber не трогает
// lab_number.
func TestBuildNumbersProjectCodeOnlyAffectsCustomer(t *testing.T) {
	before, labBefore := buildNumbers(5, 2026, "ПР1", "ЛГ", "30244")
	after, labAfter := buildNumbers(5, 2026, "ПР2", "ЛГ", "30244")

	if before == after {
		t.Fatalf("номер заказчику не изменился при смене проекта: %q", after)
	}
	if after != "ПР2-5/2026-ЛГ-30244" {
		t.Errorf("после смены проекта customer = %q, want %q", after, "ПР2-5/2026-ЛГ-30244")
	}
	if labBefore != labAfter {
		t.Errorf("номер лаборатории изменился при смене проекта: %q → %q", labBefore, labAfter)
	}
}

// TestMapEmailPriority — слова легаси-трекера и новые подписи интерфейса дают
// одни и те же значения. Старые убирать нельзя: шаблоны писем трекера мы не
// меняем (см. комментарий у mapEmailPriority).
func TestMapEmailPriority(t *testing.T) {
	cases := map[string]string{
		"":              "normal",
		"  ":            "normal",
		"Обычный":       "normal",
		"Средний":       "normal",
		"Критичный":     "critical",
		"Критический":   "critical",
		"Блокер":        "blocker",
		"Блокирующий":   "blocker",
		" Критический ": "critical",
		"неведомое":     "normal",
	}
	for raw, want := range cases {
		if got := mapEmailPriority(raw); got != want {
			t.Errorf("mapEmailPriority(%q) = %q, want %q", raw, got, want)
		}
	}
}
