package main

import "testing"

// Заголовок таблицы аналогов после 2026-10-17 — «Номер» (колонки расширены
// толщиной), таблицы идут по ступеням. Числовые таблицы показателя и таблицы
// по толщинам начинаются с «Показатель»/«Толщина, мм» — различаем их по
// заголовку первой колонки.
func TestComposeAppendixNodes(t *testing.T) {
	rc := &reportContext{
		MethodName: "ГГ",
		EKN:        "644712",
		Material:   MaterialKey{Type: materialTypeInsul, Line: "logicpir", Modifier: "f/f"},
		Thickness:  40,
		IndicatorStats: []indicatorStat{
			{ID: "length", Name: "Длина", Count: 3, HasNumeric: true, Avg: 20, Min: 10, Max: 30},
			{ID: "group", Name: "Группа", Count: 2, Distribution: map[string]int{"Г4": 1, "Г2": 1}},
		},
		Analogues: []analogueRow{{
			Number: "1/2025", Object: "Образец", Thickness: "50",
			Result: "Г4", Compliance: "Не соответствует", Tier: analogueTierSameEKN,
		}},
	}
	nodes := composeAppendixNodes(rc)
	if len(nodes) == 0 {
		t.Fatal("no nodes")
	}
	if nodes[0].Type != "heading" {
		t.Fatalf("first node must be heading, got %q", nodes[0].Type)
	}
	var hasNumeric, hasDist, hasAnalogues bool
	for _, n := range nodes {
		if n.Type != "static_table" {
			continue
		}
		header := n.Rows[0][0][0].Text
		switch header {
		case "Показатель":
			if len(n.Rows[0]) == 5 {
				if len(n.Rows) < 2 || n.Rows[1][2][0].Text != "20" {
					t.Fatalf("numeric table wrong: %+v", n.Rows)
				}
				hasNumeric = true
			} else if len(n.Rows[0]) == 3 {
				hasDist = true
			}
		case "Номер":
			hasAnalogues = true
		}
	}
	if !hasNumeric || !hasDist || !hasAnalogues {
		t.Fatalf("missing tables: numeric=%v dist=%v analogues=%v", hasNumeric, hasDist, hasAnalogues)
	}
}

// Мини-таблицы по толщинам: по одной на показатель, с выводом о тренде или
// честной пометкой «данных недостаточно».
func TestComposeThicknessNodes(t *testing.T) {
	rc := &reportContext{
		MethodName: "РП",
		Material:   MaterialKey{Type: materialTypeMembrane, Line: "logicroof v-rp"},
		IndicatorStats: []indicatorStat{
			{
				ID: "strength", Name: "Прочность", HasNumeric: true, Count: 12,
				ByThickness: []thicknessBucket{
					{Thickness: "1,2", Tests: 3, Count: 3, HasNumeric: true, Avg: 10, Min: 9, Max: 11},
					{Thickness: "1,5", Tests: 3, Count: 4, HasNumeric: true, Avg: 15, Min: 14, Max: 16},
					{Thickness: "2,0", Tests: 3, Count: 5, HasNumeric: true, Avg: 20, Min: 19, Max: 21},
				},
				Trend: &trendNote{IndicatorID: "strength", Direction: "растёт", From: "1,2", To: "2,0", Delta: "+100%"},
			},
			{
				ID: "group", Name: "Группа", Count: 4,
				ByThickness: []thicknessBucket{
					{Thickness: "1,5", Tests: 2, Count: 2, Distribution: map[string]int{"Г2": 2}},
					{Thickness: "2,0", Tests: 2, Count: 2, Distribution: map[string]int{"Г3": 2}},
				},
			},
		},
	}
	nodes := composeThicknessNodes(rc)
	if len(nodes) == 0 {
		t.Fatal("ожидался раздел по толщинам")
	}

	var tables, trendText, insufficient int
	for _, n := range nodes {
		if n.Type == "static_table" {
			tables++
			if len(n.Rows[0]) != 6 && len(n.Rows[0]) != 3 {
				t.Errorf("неожиданная таблица по толщинам: %+v", n.Rows[0])
			}
		}
		if n.Type == "paragraph" && len(n.Children) == 1 {
			text := n.Children[0].Text
			if containsAll(text, "растёт", "Прочность") {
				trendText++
			}
			if containsAll(text, "данных недостаточно") {
				insufficient++
			}
		}
	}
	if tables != 2 {
		t.Errorf("ожидалось по таблице на каждый показатель (2), получено %d", tables)
	}
	if trendText == 0 {
		t.Error("не найден текст вывода о тренде")
	}
	if insufficient == 0 {
		t.Error("не найдена пометка «данных недостаточно» для второй толщины")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestComposeAppendixNodesNoData(t *testing.T) {
	nodes := composeAppendixNodes(&reportContext{MethodName: "X"})
	var hasPlain bool
	for _, n := range nodes {
		if n.Type == "paragraph" && len(n.Children) == 1 && n.Children[0].Text == "Числовых данных по аналогичным объектам недостаточно для статистической обработки." {
			hasPlain = true
		}
		if n.Type == "static_table" {
			t.Fatal("must not build tables without data")
		}
	}
	if !hasPlain {
		t.Fatal("expected 'no data' note")
	}
}

func TestComposeReportBlocksAddsAIAndAppendix(t *testing.T) {
	p := &protocolData{Methods: []protocolMethod{{}}}
	rc := &reportContext{MethodName: "M"}
	ai := &aiSections{Sections: []aiSection{{Heading: "Вывод", Paragraphs: []string{"текст"}, Bullets: []string{"пункт"}}}}

	composeReportBlocks(p, rc, ai)
	if len(p.Methods[0].Blocks) != 2 {
		t.Fatalf("want protocol-less body to get AI + appendix blocks, got %d", len(p.Methods[0].Blocks))
	}
	if p.Methods[0].Blocks[0].ID != "report_ai" || p.Methods[0].Blocks[1].ID != "report_appendix" {
		t.Fatalf("blocks wrong: %+v", p.Methods[0].Blocks)
	}

	// Без ИИ - только приложение.
	p2 := &protocolData{Methods: []protocolMethod{{}}}
	composeReportBlocks(p2, rc, nil)
	if len(p2.Methods[0].Blocks) != 1 || p2.Methods[0].Blocks[0].ID != "report_appendix" {
		t.Fatalf("without AI want only appendix: %+v", p2.Methods[0].Blocks)
	}
}
