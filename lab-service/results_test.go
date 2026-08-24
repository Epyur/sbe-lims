package main

import "testing"

// ---- Хелперы построения операндов/clauses/веток/subjects единой модели
// (2026-08-22v3 — левая часть сравнения НЕЯВНАЯ: "текущий оцениваемый атрибут",
// см. applyRuleToSubjects; в самих условиях конкретные атрибуты не упоминаются). ----

func lit(v any) map[string]any        { return map[string]any{"kind": "literal", "value": v} }
func attrOp(id string) map[string]any { return map[string]any{"kind": "attribute", "id": id} }
func targetOp() map[string]any        { return map[string]any{"kind": "target_indicator"} }

func clause(op string, compareTo map[string]any) map[string]any {
	return map[string]any{"operator": op, "compare_to": compareTo}
}

func toAnySlice(clauses []map[string]any) []any {
	out := make([]any, len(clauses))
	for i, c := range clauses {
		out[i] = c
	}
	return out
}

func branch(grade string, clauses ...map[string]any) map[string]any {
	return map[string]any{"clauses": toAnySlice(clauses), "grade": grade}
}

func branchOr(grade string, clauses ...map[string]any) map[string]any {
	return map[string]any{"clauses": toAnySlice(clauses), "join": "or", "grade": grade}
}

func elseBranch(grade string) map[string]any { return map[string]any{"grade": grade} }

func subject(inputID, outputID string) map[string]any {
	return map[string]any{"input_attribute_id": inputID, "output_attribute_id": outputID}
}

// noTarget — loadTarget, который никогда ничего не резолвит (для веток без
// target_indicator — не должен даже вызываться).
func noTarget() (any, bool) { return nil, false }

// evaluateBranches — единая модель: subjectValue передаётся явно (неявная левая
// часть), сама схема условий не знает, какой атрибут оценивается. Порядок как
// задан, без пересортировки, первая совпавшая ветка — результат, иначе — явный
// catch-all вместо неявного "последнего условия".
func TestEvalBranchOrderedLiteral(t *testing.T) {
	branches := []any{
		branch("A", clause("<=", lit(135.0))),
		branch("B", clause("<=", lit(235.0))),
		branch("C", clause("<=", lit(450.0))),
		branch("D", clause(">", lit(450.0))),
	}
	cases := []struct {
		value float64
		want  string
	}{
		{100.0, "A"},
		{135.0, "A"}, // граница включена ("<=")
		{136.0, "B"},
		{450.0, "C"},
		{451.0, "D"},
	}
	ctx := classifyCtx{loadTarget: noTarget}
	for _, c := range cases {
		grade, matched := evaluateBranches(ctx, branches, c.value)
		if !matched || grade != c.want {
			t.Errorf("value=%v: got (%q,%v), want (%q,true)", c.value, grade, matched, c.want)
		}
	}
}

// Явный catch-all (elseBranch, без clauses) — заменяет прежний неявный "фолбэк на
// последнее условие".
func TestEvaluateBranchesElseFallback(t *testing.T) {
	branches := []any{branch("low", clause("<=", lit(50.0))), elseBranch("иначе")}
	ctx := classifyCtx{loadTarget: noTarget}
	grade, matched := evaluateBranches(ctx, branches, 999.0)
	if !matched || grade != "иначе" {
		t.Errorf("got (%q,%v), want (\"иначе\",true)", grade, matched)
	}
}

// Без совпавшей ветки и без catch-all — ничего не результат (matched=false),
// вызывающий код (applyRuleToSubjects) в этом случае ничего не пишет в values.
func TestEvaluateBranchesNoMatchNoElse(t *testing.T) {
	branches := []any{branch("low", clause("<=", lit(50.0)))}
	ctx := classifyCtx{loadTarget: noTarget}
	_, matched := evaluateBranches(ctx, branches, 999.0)
	if matched {
		t.Error("не должно быть совпадения без catch-all")
	}
}

// Порядок веток — как задал пользователь, БЕЗ пересортировки: те же две ветки в
// разном порядке классифицируют одно и то же значение по-разному.
func TestEvaluateBranchesRespectsGivenOrder(t *testing.T) {
	ctx := classifyCtx{loadTarget: noTarget}
	orderA := []any{branch("first", clause("<=", lit(100.0))), branch("second", clause("<=", lit(200.0)))}
	if grade, _ := evaluateBranches(ctx, orderA, 50.0); grade != "first" {
		t.Errorf("orderA: got %q, want first", grade)
	}
	orderB := []any{branch("second", clause("<=", lit(200.0))), branch("first", clause("<=", lit(100.0)))}
	if grade, _ := evaluateBranches(ctx, orderB, 50.0); grade != "second" {
		t.Errorf("orderB: got %q, want second (сервер не пересортировывает ветки)", grade)
	}
}

// classificationCompare — ключевой инвариант, подтверждённый пользователем: первый
// введённый в determinable_indicators показатель считается "большим" (Г1,Г2,Г3,Г4 ->
// Г1>Г2>Г3>Г4), т.е. сравнение по индексу ИНВЕРТИРОВАНО.
func TestClassificationCompareRankInverted(t *testing.T) {
	rankOrder := []string{"Г1", "Г2", "Г3", "Г4"}
	if !classificationCompare(">", "Г1", "Г2", rankOrder) {
		t.Error("Г1 > Г2 должно быть true (первый введённый — больше)")
	}
	if classificationCompare(">", "Г4", "Г1", rankOrder) {
		t.Error("Г4 > Г1 должно быть false")
	}
	if !classificationCompare(">=", "Г2", "Г2", rankOrder) {
		t.Error("Г2 >= Г2 должно быть true (равенство)")
	}
	if !classificationCompare("<", "Г4", "Г3", rankOrder) {
		t.Error("Г4 < Г3 должно быть true (Г4 ниже/меньше Г3)")
	}
}

// Обратная индексация ⇒ "не ниже цели" (старая подтверждённая семантика compliance:
// valueIdx <= targetIdx → соответствует) выражается через >= в новой модели —
// проверяем эквивалентность на тех же случаях, что раньше покрывал classifyCompliance.
func TestClassificationCompareComplianceEquivalence(t *testing.T) {
	rankOrder := []string{"Г1", "Г2", "Г3", "Г4"}
	cases := []struct {
		achieved, target string
		wantComply       bool
	}{
		{"Г1", "Г2", true},  // выше цели
		{"Г2", "Г2", true},  // равно цели
		{"Г3", "Г2", false}, // ниже цели
	}
	for _, c := range cases {
		got := classificationCompare(">=", c.achieved, c.target, rankOrder)
		if got != c.wantComply {
			t.Errorf("achieved=%q target=%q: got %v, want %v", c.achieved, c.target, got, c.wantComply)
		}
	}
}

// Числовое сравнение (обе стороны — не показатели метода) — обычная арифметика.
func TestClassificationCompareNumericFallback(t *testing.T) {
	if !classificationCompare("<=", 50.0, 100.0, nil) {
		t.Error("50 <= 100 должно быть true")
	}
	if classificationCompare("<=", 150.0, 100.0, nil) {
		t.Error("150 <= 100 должно быть false")
	}
}

// Интеграционный сценарий: "соответствие целевому показателю" через единую модель —
// 3 явные ветки (соответствует/не соответствует/не оценивается) вместо трёх скрытых
// текстовых полей прежнего compliance-правила. subjectValue = "достигнутый"
// показатель, правая часть каждого clause — целевой показатель заявки.
func TestEvaluateBranchesComplianceViaTargetIndicator(t *testing.T) {
	rankOrder := []string{"Г1", "Г2", "Г3", "Г4"}
	branches := []any{
		branch("Соответствует", clause(">=", targetOp())),
		branch("Не соответствует", clause("<", targetOp())),
		elseBranch("Не оценивается"),
	}
	cases := []struct {
		name      string
		achieved  any
		target    string
		hasTarget bool
		want      string
	}{
		{"выше цели", "Г1", "Г2", true, "Соответствует"},
		{"равно цели", "Г2", "Г2", true, "Соответствует"},
		{"ниже цели", "Г3", "Г2", true, "Не соответствует"},
		{"нет цели", "Г1", "", false, "Не оценивается"},
	}
	for _, c := range cases {
		loadTarget := func() (any, bool) {
			if c.hasTarget {
				return c.target, true
			}
			return nil, false
		}
		ctx := classifyCtx{rankOrder: rankOrder, loadTarget: loadTarget}
		grade, matched := evaluateBranches(ctx, branches, c.achieved)
		if !matched || grade != c.want {
			t.Errorf("%s: got (%q,%v), want (%q,true)", c.name, grade, matched, c.want)
		}
	}
}

// Правая часть сравнивается с другим атрибутом текущей записи (не только с
// литералом/целевым показателем) — subjectValue при этом передаётся отдельно,
// в самом clause атрибут-предмет оценки не упоминается.
func TestEvalClauseCompareToAttribute(t *testing.T) {
	ctx := classifyCtx{values: map[string]any{"b": 5.0}, loadTarget: noTarget}
	if !evalClause(ctx, clause(">", attrOp("b")), 10.0) {
		t.Error("10 > b(5) должно быть true")
	}
	if evalClause(ctx, clause("<", attrOp("b")), 10.0) {
		t.Error("10 < b(5) должно быть false")
	}
}

// Несколько clauses в одной ветке, объединённых "И" (join по умолчанию) — все
// должны совпасть, как в Excel AND(...).
func TestEvalBranchAndJoin(t *testing.T) {
	b := branch("grade", clause(">", lit(5.0)), clause("<", lit(20.0)))
	ctx := classifyCtx{loadTarget: noTarget}
	if !evalBranch(ctx, b, 10.0) {
		t.Error("10 в (5;20) — оба условия верны, должно быть true")
	}
	if evalBranch(ctx, b, 30.0) {
		t.Error("30 > 20 — второе условие не верно, должно быть false")
	}
}

// Несколько clauses, объединённых "ИЛИ" — достаточно одного совпадения, как в
// Excel OR(...).
func TestEvalBranchOrJoin(t *testing.T) {
	b := branchOr("grade", clause(">", lit(100.0)), clause("<", lit(5.0)))
	ctx := classifyCtx{loadTarget: noTarget}
	if !evalBranch(ctx, b, 1.0) {
		t.Error("1 < 5 верно, должно быть true")
	}
	if evalBranch(ctx, b, 10.0) {
		t.Error("ни одно условие не верно, должно быть false")
	}
}

// Правая часть не резолвится (атрибут не заполнен) — clause просто не
// совпадает (не ошибка, не паника).
func TestEvalClauseUnresolvedCompareToNoMatch(t *testing.T) {
	ctx := classifyCtx{values: map[string]any{}, loadTarget: noTarget}
	if evalClause(ctx, clause(">", attrOp("missing")), 10.0) {
		t.Error("неразрешённая правая часть не должна совпадать ни с каким условием")
	}
}

// applyRuleToSubjects — ключевой новый сценарий по прямой правке пользователя:
// ОДНА схема условий (branches) применяется по отдельности к КАЖДОЙ строке
// subjects — "оцениваемый атрибут" → "куда записать результат". Разные
// subjects с разными значениями input-атрибута должны получить разные grade
// в СВОИХ output-атрибутах, при этом порядок subjects не пересортировывается,
// а subject без значения input-атрибута просто пропускается.
func TestApplyRuleToSubjectsMultiple(t *testing.T) {
	rule := map[string]any{
		"branches": []any{
			branch("A", clause("<=", lit(135.0))),
			branch("B", clause("<=", lit(235.0))),
			elseBranch("C"),
		},
		"subjects": []any{
			subject("comb_length_1", "comb_length_1_grade"),
			subject("comb_length_2", "comb_length_2_grade"),
			subject("comb_length_3", "comb_length_3_grade"), // нет значения — пропускается
		},
	}
	values := map[string]any{
		"comb_length_1": 100.0, // A
		"comb_length_2": 200.0, // B
	}
	ctx := classifyCtx{values: values, loadTarget: noTarget}
	applyRuleToSubjects(ctx, rule, nil, false)

	if got := values["comb_length_1_grade"]; got != "A" {
		t.Errorf("comb_length_1_grade: got %v, want A", got)
	}
	if got := values["comb_length_2_grade"]; got != "B" {
		t.Errorf("comb_length_2_grade: got %v, want B", got)
	}
	if _, ok := values["comb_length_3_grade"]; ok {
		t.Error("comb_length_3_grade не должен быть записан — у subject нет значения input-атрибута")
	}
}

// wantAggregated (2026-08-23) — subject с aggregated-output должен сработать
// ТОЛЬКО при wantAggregated=true, и не сработать (не найти input, который никогда
// не появляется в per-series values) при wantAggregated=false — реальный сценарий
// метода ГВ: agg_flam_flow_density -> flammability_group, обе aggregated. Без этого
// разделения такой subject не совпадал НИКОГДА, ни при каком проходе (см.
// aggregatedAttributeIDs/applyAggregatedClassification).
func TestApplyRuleToSubjectsAggregatedOutputRequiresAggregatedPass(t *testing.T) {
	rule := map[string]any{
		"branches": []any{
			branch("В3", clause("<", lit(20.0))),
			elseBranch("В2"),
		},
		"subjects": []any{
			subject("agg_flam_flow_density", "flammability_group"),
		},
	}
	aggregatedIDs := map[string]bool{"flammability_group": true}

	// per-series проход (wantAggregated=false) — input есть в values, но output
	// aggregated: subject должен быть пропущен этим проходом.
	perSeriesValues := map[string]any{"agg_flam_flow_density": 15.0}
	ctx := classifyCtx{values: perSeriesValues, loadTarget: noTarget}
	applyRuleToSubjects(ctx, rule, aggregatedIDs, false)
	if _, ok := perSeriesValues["flammability_group"]; ok {
		t.Error("flammability_group не должен писаться проходом wantAggregated=false")
	}

	// aggregated-проход (wantAggregated=true) — тот же subject должен сработать.
	aggValues := map[string]any{"agg_flam_flow_density": 15.0}
	actx := classifyCtx{values: aggValues, loadTarget: noTarget}
	applyRuleToSubjects(actx, rule, aggregatedIDs, true)
	if got := aggValues["flammability_group"]; got != "В3" {
		t.Errorf("flammability_group: got %v, want В3", got)
	}
}
