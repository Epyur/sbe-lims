package main

import "testing"

func TestComposeAppendixNodes(t *testing.T) {
	rc := &reportContext{
		MethodName: "ГГ",
		EKN:        "644712",
		IndicatorStats: []indicatorStat{
			{ID: "length", Name: "Длина", Count: 3, HasNumeric: true, Avg: 20, Min: 10, Max: 30},
			{ID: "group", Name: "Группа", Count: 2, Distribution: map[string]int{"Г4": 1, "Г2": 1}},
		},
		Analogues: []analogueRow{{Number: "1/2025", Object: "Образец", Result: "Г4", Compliance: "Не соответствует"}},
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
			// различаем две таблицы по числу колонок
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
	ai := &aiSections{Sections: []aiSection{{Heading: "Вывод", Paragraphs: []string{"Всё хорошо"}, Bullets: []string{"пункт"}}}}

	composeReportBlocks(p, rc, ai)
	if len(p.Methods[0].Blocks) != 2 {
		t.Fatalf("want protocol-less body to get AI + appendix blocks, got %d", len(p.Methods[0].Blocks))
	}
	if p.Methods[0].Blocks[0].ID != "report_ai" || p.Methods[0].Blocks[1].ID != "report_appendix" {
		t.Fatalf("blocks wrong: %+v", p.Methods[0].Blocks)
	}

	// Без ИИ — только приложение.
	p2 := &protocolData{Methods: []protocolMethod{{}}}
	composeReportBlocks(p2, rc, nil)
	if len(p2.Methods[0].Blocks) != 1 || p2.Methods[0].Blocks[0].ID != "report_appendix" {
		t.Fatalf("without AI want only appendix: %+v", p2.Methods[0].Blocks)
	}
}
