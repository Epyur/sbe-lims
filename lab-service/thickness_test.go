package main

import "testing"

// Срез по толщинам: все толщины одного материала попадают в свою строку,
// значения одного испытания (заявки) по нескольким сериям считаются вместе,
// а Tests — это РАЗНЫЕ испытания, а не число значений.
func TestGroupByThicknessSplits(t *testing.T) {
	rows := []analogueRow{
		{RequestID: 1, Thickness: "1,2"},
		{RequestID: 2, Thickness: "1,2"},
		{RequestID: 3, Thickness: "1,5"},
		{RequestID: 4, Thickness: "1,5"},
		{RequestID: 5, Thickness: "1,5"},
		{RequestID: 6, Thickness: ""},
	}
	values := []analogueValue{
		{row: rows[0], value: 10.0},
		{row: rows[1], value: 12.0},
		// у испытания 3 две серии — Tests должен остаться 1
		{row: rows[2], value: 20.0},
		{row: rows[2], value: 22.0},
		{row: rows[3], value: 24.0},
		{row: rows[4], value: 26.0},
		{row: rows[5], value: 30.0},
	}

	buckets := groupByThickness(values)
	if len(buckets) != 3 {
		t.Fatalf("ожидалось 3 среза (1,2 / 1,5 / не указана), получено %d: %+v", len(buckets), buckets)
	}

	// Порядок: по возрастанию толщины, «не указана» последней.
	if buckets[0].Thickness != "1,2" || buckets[1].Thickness != "1,5" || buckets[2].Thickness != "" {
		t.Errorf("порядок срезов неверный: %q, %q, %q",
			buckets[0].Thickness, buckets[1].Thickness, buckets[2].Thickness)
	}
	if buckets[0].Tests != 2 || buckets[0].Count != 2 {
		t.Errorf("1,2: tests=%d count=%d, ожидалось 2/2", buckets[0].Tests, buckets[0].Count)
	}
	if buckets[1].Tests != 3 {
		t.Errorf("1,5: tests=%d, ожидалось 3 (серии одного испытания — одно)", buckets[1].Tests)
	}
	if buckets[1].Count != 4 {
		t.Errorf("1,5: count=%d, ожидалось 4 значения (2+1+1)", buckets[1].Count)
	}
	if buckets[1].Min != 20 || buckets[1].Max != 26 {
		t.Errorf("1,5: min=%v max=%v, ожидалось 20/26", buckets[1].Min, buckets[1].Max)
	}
	wantAvg := (20 + 22 + 24 + 26) / 4.0
	if buckets[1].Avg != wantAvg {
		t.Errorf("1,5: avg=%v, ожидалось %v", buckets[1].Avg, wantAvg)
	}
}

// Тренд: порог — 3 испытания на КАЖДУЮ толщину (решение пользователя).
func TestComputeTrendThreshold(t *testing.T) {
	mk := func(pairs ...[2]any) []thicknessBucket {
		out := make([]thicknessBucket, 0, len(pairs))
		for _, p := range pairs {
			out = append(out, thicknessBucket{
				Thickness: p[0].(string), Tests: p[1].(int),
				HasNumeric: true, Avg: float64(p[1].(int)) * 10,
			})
		}
		return out
	}

	// 3+ на каждой — тренд есть.
	buckets := mk(
		[2]any{"1,2", 3},
		[2]any{"1,5", 4},
		[2]any{"2,0", 3},
	)
	for i := range buckets {
		buckets[i].Avg = float64(i+1) * 10
	}
	if note := computeTrend("x", buckets); note == nil || note.Direction != "растёт" {
		t.Errorf("ожидался тренд «растёт», получено %+v", note)
	} else if note.Delta == "" {
		t.Errorf("ожидался процент изменения, получено пусто")
	}

	// У одной толщины 2 испытания — текста нет.
	if note := computeTrend("x", mk([2]any{"1,2", 2}, [2]any{"1,5", 5})); note != nil {
		t.Errorf("при 2 испытаниях тренд должен отсутствовать, получено %+v", note)
	}

	// Одна толщина — тренда не бывает.
	if note := computeTrend("x", mk([2]any{"1,5", 5})); note != nil {
		t.Errorf("одна толщина — тренда нет, получено %+v", note)
	}

	// Нечисловые срезы не участвуют.
	nn := mk([2]any{"1,2", 3}, [2]any{"1,5", 3})
	nn[1].HasNumeric = false
	if note := computeTrend("x", nn); note != nil {
		t.Errorf("срез без чисел не участвует в тренде, получено %+v", note)
	}
}

// Смена знака приращения — «немонотонно» (решение пользователя: пересечение
// диапазонов тренда не отменяет, отменяет только смена знака).
func TestComputeTrendNonMonotonic(t *testing.T) {
	buckets := []thicknessBucket{
		{Thickness: "1,2", Tests: 3, HasNumeric: true, Avg: 10},
		{Thickness: "1,5", Tests: 3, HasNumeric: true, Avg: 20},
		{Thickness: "2,0", Tests: 3, HasNumeric: true, Avg: 15},
	}
	note := computeTrend("x", buckets)
	if note == nil || note.Direction != "немонотонно" {
		t.Fatalf("ожидалось «немонотонно», получено %+v", note)
	}
}

// Пересечение диапазонов min..max при постоянном знаке прироста среднего
// тренда НЕ отменяет (решение пользователя 2026-10-17).
func TestComputeTrendIgnoresRangeOverlap(t *testing.T) {
	buckets := []thicknessBucket{
		{Thickness: "1,2", Tests: 3, HasNumeric: true, Avg: 10, Min: 5, Max: 40},
		{Thickness: "1,5", Tests: 3, HasNumeric: true, Avg: 20, Min: 12, Max: 45},
	}
	note := computeTrend("x", buckets)
	if note == nil || note.Direction != "растёт" {
		t.Errorf("пересечение диапазонов не должно отменять тренд, получено %+v", note)
	}
}

// Ступени 2 и 3 в разрез по толщинам не попадают — проверяем на уровне
// computeIndicatorStats.
func TestComputeIndicatorStatsThicknessOnlyStrictTier(t *testing.T) {
	attrs := []MethodAttribute{{ID: "strength", Name: "Прочность"}}
	analogues := []analogueRow{
		{RequestID: 1, Tier: analogueTierSameEKN, Thickness: "1,2"},
		{RequestID: 2, Tier: analogueTierSameEKN, Thickness: "1,2"},
		{RequestID: 3, Tier: analogueTierSameEKN, Thickness: "1,5"},
		{RequestID: 4, Tier: analogueTierSameName, Thickness: "1,5"},
		{RequestID: 5, Tier: analogueTierModifier, Thickness: "2,0"},
	}
	series := map[int64][]map[string]any{
		1: {{"strength": 10.0}},
		2: {{"strength": 12.0}},
		3: {{"strength": 20.0}},
		4: {{"strength": 99.0}}, // ступень 2 — в разрез не идёт
		5: {{"strength": 88.0}}, // ступень 3 — в разрез не идёт
	}
	stats := computeIndicatorStats(attrs, analogues, series, map[int64]map[string]any{})
	if len(stats) != 1 {
		t.Fatalf("ожидался 1 показатель, получено %d", len(stats))
	}
	st := stats[0]
	if st.Count != 5 {
		t.Errorf("общий count=%d, ожидалось 5 (все ступени)", st.Count)
	}
	if len(st.ByThickness) != 2 {
		t.Fatalf("в разрезе ожидалось 2 толщины (1,2 и 1,5), получено %d: %+v",
			len(st.ByThickness), st.ByThickness)
	}
	for _, b := range st.ByThickness {
		if b.Thickness == "2,0" {
			t.Errorf("ступень 3 попала в разрез по толщинам: %+v", b)
		}
		if b.Thickness == "1,5" && b.Max > 50 {
			t.Errorf("ступень 2 попала в разрез: %+v", b)
		}
	}
}
