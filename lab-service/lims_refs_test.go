package main

import (
	"encoding/json"
	"testing"
)

// deriveFormulasFromAttributes — единственный источник formulas для конфигуратора
// методов (2026-08-21): расчётные атрибуты дают formula как есть, агрегированные без
// своей формулы — автоформулу "{method}({source})"; атрибуты без формулы/агрегации
// (ручной ввод, данные прибора) в formulas не попадают.
func TestDeriveFormulasFromAttributes(t *testing.T) {
	input := `[
		{"id":"mass_before","name":"Масса до","data_type":"float","fill_method":"manual","level":"experiment"},
		{"id":"delta_by_mass","name":"Потеря массы","data_type":"float","fill_method":"calculated","level":"experiment","formula":"100 - (mass_after * 100) / mass_before"},
		{"id":"avg_delta","name":"Среднее","data_type":"float","fill_method":"calculated","level":"aggregated","aggregation":{"source":"delta_by_mass","method":"avg"}}
	]`
	out, err := deriveFormulasFromAttributes(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	var formulas []map[string]any
	if err := json.Unmarshal(out, &formulas); err != nil {
		t.Fatal(err)
	}
	if len(formulas) != 2 {
		t.Fatalf("got %d formulas, want 2: %v", len(formulas), formulas)
	}
	if formulas[0]["target_parameter"] != "delta_by_mass" || formulas[0]["apply_level"] != "series" {
		t.Errorf("unexpected formula[0]: %v", formulas[0])
	}
	if formulas[0]["expression"] != "100 - (mass_after * 100) / mass_before" {
		t.Errorf("formula[0] expression not passed through as-is: %v", formulas[0])
	}
	if formulas[1]["target_parameter"] != "avg_delta" || formulas[1]["apply_level"] != "aggregated" {
		t.Errorf("unexpected formula[1]: %v", formulas[1])
	}
	if formulas[1]["expression"] != "avg(delta_by_mass)" {
		t.Errorf("formula[1] expression = %v, want avg(delta_by_mass)", formulas[1]["expression"])
	}
}

// Реальный баг метода ГВ (2026-08-23): target_group_compliance изначально был
// fill_method="calculated" с формулой; пользователь переключил его в конфигураторе
// на fill_method="classification" (level="aggregated") и добавил правило
// классификации — но конфигуратор не чистит .formula при смене fill_method, и
// deriveFormulasFromAttributes раньше ориентировался только на "Formula не
// пусто", игнорируя fill_method. Старая формула продолжала молча попадать в
// cfg.Formulas и падать при расчёте (двойные кавычки — отдельный баг DSL,
// исправлен в dsl.go), обрывая ВЕСЬ проход applyAggregatedFormulas — включая
// другие, исправные aggregated-формулы того же метода (agg_flam_flow_density).
func TestDeriveFormulasFromAttributesIgnoresStaleFormulaAfterFillMethodSwitch(t *testing.T) {
	input := `[
		{"id":"agg_flam_flow_density","level":"aggregated","fill_method":"calculated","formula":"agg_where('min', flam_flow_density, flam_ignition, 'Да')"},
		{"id":"target_group_compliance","level":"aggregated","fill_method":"classification","formula":"if target_group_compliance == '' then \"Не оценивается\" else \"Соответствует\""}
	]`
	out, err := deriveFormulasFromAttributes(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	var formulas []map[string]any
	if err := json.Unmarshal(out, &formulas); err != nil {
		t.Fatal(err)
	}
	if len(formulas) != 1 {
		t.Fatalf("got %d formulas, want 1 (stale target_group_compliance.formula must be ignored): %v", len(formulas), formulas)
	}
	if formulas[0]["target_parameter"] != "agg_flam_flow_density" {
		t.Errorf("unexpected surviving formula: %v", formulas[0])
	}
}

// Тот же класс бага, что выше, но для .aggregation вместо .formula: атрибут был
// уровня "aggregated" с авто-формулой агрегации (fill_method="instrument", без
// собственной .formula), пользователь переключил его на "Классификация" —
// конфигуратор не чистит .aggregation, оставшееся значение не должно молча
// породить авто-формулу поверх правила классификации.
func TestDeriveFormulasFromAttributesIgnoresStaleAggregationAfterClassificationSwitch(t *testing.T) {
	input := `[{"id":"flammability_group","level":"aggregated","fill_method":"classification","aggregation":{"source":"agg_flam_flow_density","method":"min"}}]`
	out, err := deriveFormulasFromAttributes(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	var formulas []map[string]any
	if err := json.Unmarshal(out, &formulas); err != nil {
		t.Fatal(err)
	}
	if len(formulas) != 0 {
		t.Fatalf("got %d formulas, want 0 (stale .aggregation on a classification attribute must be ignored): %v", len(formulas), formulas)
	}
}

func TestDeriveFormulasFromAttributesInvalidAggregationMethod(t *testing.T) {
	input := `[{"id":"x","level":"aggregated","aggregation":{"source":"y","method":"sum"}}]`
	if _, err := deriveFormulasFromAttributes(json.RawMessage(input)); err == nil {
		t.Error("expected error for unsupported aggregation method")
	}
}

// any/all (2026-08-25, реальный сценарий заявки 287/2026 — метод ГГ): агрегация
// текстового Да/Нет-поля должна давать логическую DSL-формулу (any(...)/all(...),
// см. dsl.go), не числовую max/min/avg, которая падает на "Да"/"Нет".
func TestDeriveFormulasFromAttributesAnyAllAggregation(t *testing.T) {
	input := `[
		{"id":"agg_burning_drops","level":"aggregated","fill_method":"instrument","aggregation":{"source":"burning_drops","method":"any"}},
		{"id":"agg_all_smog","level":"aggregated","fill_method":"instrument","aggregation":{"source":"has_smog","method":"all"}}
	]`
	out, err := deriveFormulasFromAttributes(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	var formulas []map[string]any
	if err := json.Unmarshal(out, &formulas); err != nil {
		t.Fatal(err)
	}
	if len(formulas) != 2 {
		t.Fatalf("got %d formulas, want 2: %v", len(formulas), formulas)
	}
	if formulas[0]["expression"] != "any(burning_drops)" {
		t.Errorf("formula[0] expression = %v, want any(burning_drops)", formulas[0]["expression"])
	}
	if formulas[1]["expression"] != "all(has_smog)" {
		t.Errorf("formula[1] expression = %v, want all(has_smog)", formulas[1]["expression"])
	}
}

func TestDeriveFormulasFromAttributesEmpty(t *testing.T) {
	out, err := deriveFormulasFromAttributes(json.RawMessage(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "[]" {
		t.Errorf("got %s, want []", out)
	}
}

// parseOptionalDate (2026-08-26, PATCH-поля дат оборудования) — три исхода должны
// различаться: поле не пришло в запросе (nil, колонку не трогаем), пришло пустой
// строкой (present=true, value=nil — явно очистить колонку), пришло валидной датой.
func TestParseOptionalDate(t *testing.T) {
	notProvided := func() *string { return nil }
	empty := ""
	valid := "2026-08-26"
	invalid := "26-08-2026"

	if v, present, ok := parseOptionalDate(notProvided()); present || !ok || v != nil {
		t.Errorf("nil: got value=%v present=%v ok=%v, want nil/false/true", v, present, ok)
	}
	if v, present, ok := parseOptionalDate(&empty); !present || !ok || v != nil {
		t.Errorf("empty: got value=%v present=%v ok=%v, want nil/true/true", v, present, ok)
	}
	v, present, ok := parseOptionalDate(&valid)
	if !present || !ok || v == nil || v.Format("2006-01-02") != valid {
		t.Errorf("valid: got value=%v present=%v ok=%v, want 2026-08-26/true/true", v, present, ok)
	}
	if _, _, ok := parseOptionalDate(&invalid); ok {
		t.Error("invalid date must return ok=false")
	}
}
