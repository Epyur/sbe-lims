package main

import "testing"

// Многоаргументные agg-функции — legacy frm00001 (ГГ): среднее по 4 замерам одной
// серии, а не по сериям одного атрибута.
func TestDSLMultiArgAggregate(t *testing.T) {
	env := &FormulaEnv{Params: map[string]any{"a": 100.0, "b": 100.0, "c": 100.0, "d": 90.0}}
	cases := []struct {
		expr string
		want float64
	}{
		{"avg(a, b, c, d)", 97.5},
		{"min(a, b, c, d)", 90.0},
		{"max(a, b, c, d)", 100.0},
		{"sum(a, b, c, d)", 390.0},
	}
	for _, c := range cases {
		res, err := runFormula(c.expr, env)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if res.(float64) != c.want {
			t.Errorf("%s = %v, want %v", c.expr, res, c.want)
		}
	}
}

// Двойные кавычки для строк (2026-08-23) — метод ГВ (target_group_compliance)
// хранил формулу со строковыми литералами в двойных кавычках ("Не оценивается"
// и т.п., привычка из большинства языков) — раньше лексер понимал только
// одинарные, любая '"' сразу давала "неожиданный символ" при каждом расчёте.
func TestDSLDoubleQuotedStrings(t *testing.T) {
	env := &FormulaEnv{Params: map[string]any{"flag": true}}
	res, err := runFormula(`if flag then "Да, двойные" else "Нет"`, env)
	if err != nil {
		t.Fatalf("double-quoted string: %v", err)
	}
	if res != "Да, двойные" {
		t.Errorf("got %v, want %q", res, "Да, двойные")
	}
	// Смешивать типы кавычек в одном выражении — можно, каждая строка закрывается
	// СВОИМ типом кавычки (одинарная не обязана заканчиваться на двойную).
	res2, err := runFormula(`if flag then 'одинарная' else "двойная"`, env)
	if err != nil {
		t.Fatalf("mixed quotes: %v", err)
	}
	if res2 != "одинарная" {
		t.Errorf("got %v, want %q", res2, "одинарная")
	}
}

// Реальная формула метода ГВ (target_group_compliance), ранее падавшая с
// "неожиданный символ '\"'" на каждом расчёте серии (см. TestDSLDoubleQuotedStrings
// выше — сама причина; здесь — конкретно эта формула, из живой БД, не падает).
func TestDSLRealGVTargetGroupComplianceFormula(t *testing.T) {
	env := &FormulaEnv{Params: map[string]any{
		"target_group_compliance": "",
		"flammability_group":      "В1",
	}}
	expr := `if target_group_compliance == '' then "Не оценивается" else if flammability_group >= target_group_compliance then "Соответствует" else "Не соответствует"`
	res, err := runFormula(expr, env)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	if res != "Не оценивается" {
		t.Errorf("got %v, want %q", res, "Не оценивается")
	}
}

// Реальный пример ap00022 (ГГ, десктопная ЛИМС): comb_lenth_1..4=100,100,100,100,
// mass_before=1036, mass_after=828 → avg_comb_length=100.0, delta_by_mass≈20.0772.
func TestDSLRealGGExample(t *testing.T) {
	env := &FormulaEnv{Params: map[string]any{
		"comb_length_1": 100.0, "comb_length_2": 100.0, "comb_length_3": 100.0, "comb_length_4": 100.0,
		"mass_before": 1036.0, "mass_after": 828.0,
	}}
	avg, err := runFormula("avg(comb_length_1, comb_length_2, comb_length_3, comb_length_4)", env)
	if err != nil {
		t.Fatal(err)
	}
	if avg.(float64) != 100.0 {
		t.Errorf("avg_comb_length = %v, want 100.0", avg)
	}
	delta, err := runFormula("100 - (mass_after * 100) / mass_before", env)
	if err != nil {
		t.Fatal(err)
	}
	got := delta.(float64)
	want := 20.0772
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("delta_by_mass = %v, want ~%v", got, want)
	}
}

// min_grade/max_grade — замена legacy find_rank_extreme: сравнение по позиции в
// RankOrder (индекс 0 — максимальный/больше остальных показателей).
func TestDSLMinMaxGrade(t *testing.T) {
	env := &FormulaEnv{
		Params:    map[string]any{"a": "Г1", "b": "Г3", "c": "Г2"},
		RankOrder: []string{"Г1", "Г2", "Г3", "Г4"},
	}
	min, err := runFormula("min_grade(a, b, c)", env)
	if err != nil {
		t.Fatal(err)
	}
	if min.(string) != "Г3" {
		t.Errorf("min_grade = %v, want Г3", min)
	}
	max, err := runFormula("max_grade(a, b, c)", env)
	if err != nil {
		t.Fatal(err)
	}
	if max.(string) != "Г1" {
		t.Errorf("max_grade = %v, want Г1", max)
	}
}

func TestDSLGradeExtremeWithoutRankOrder(t *testing.T) {
	env := &FormulaEnv{Params: map[string]any{"a": "Г1"}}
	if _, err := runFormula("min_grade(a)", env); err == nil {
		t.Error("expected error when RankOrder is empty")
	}
}

// interpolate — калибровочная таблица (замена legacy frm00020): внутри диапазона —
// линейно между соседними точками; за пределами — продолжение по касательной
// крайнего отрезка.
func TestDSLInterpolate(t *testing.T) {
	env := &FormulaEnv{Params: map[string]any{
		"x":  5.0,
		"xs": []any{0.0, 10.0, 20.0},
		"ys": []any{0.0, 5.0, 20.0},
	}}
	cases := []struct {
		x    float64
		want float64
	}{
		{5.0, 2.5},   // внутри первого отрезка (0..10, 0..5)
		{15.0, 12.5}, // внутри второго отрезка (10..20, 5..20)
		{-5.0, -2.5}, // ниже диапазона — касательная первого отрезка
		{25.0, 27.5}, // выше диапазона — касательная последнего отрезка
	}
	for _, c := range cases {
		env.Params["x"] = c.x
		res, err := runFormula("interpolate(x, xs, ys)", env)
		if err != nil {
			t.Fatalf("x=%v: %v", c.x, err)
		}
		got := res.(float64)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("interpolate(%v) = %v, want %v", c.x, got, c.want)
		}
	}
}

// agg_where — замена legacy формул вида "min(get_all_values(x) where условие)"
// (frm00015: критическая плотность потока — минимум среди серий с зажиганием).
func TestDSLAggWhere(t *testing.T) {
	env := &FormulaEnv{SeriesParams: map[string][]any{
		"flam_flow_density": {11.2, 8.5, 15.0},
		"flam_ignition":     {"Да", "Нет", "Да"},
	}}
	res, err := runFormula("agg_where('min', flam_flow_density, flam_ignition, 'Да')", env)
	if err != nil {
		t.Fatal(err)
	}
	if res.(float64) != 11.2 {
		t.Errorf("agg_where min = %v, want 11.2", res)
	}
	res, err = runFormula("agg_where('avg', flam_flow_density, flam_ignition, 'Да')", env)
	if err != nil {
		t.Fatal(err)
	}
	if res.(float64) != 13.1 {
		t.Errorf("agg_where avg = %v, want 13.1", res)
	}
}

func TestDSLAggWhereNoMatch(t *testing.T) {
	env := &FormulaEnv{SeriesParams: map[string][]any{
		"v": {1.0, 2.0},
		"c": {"Нет", "Нет"},
	}}
	if _, err := runFormula("agg_where('min', v, c, 'Да')", env); err == nil {
		t.Error("expected error when no series match the condition")
	}
}
