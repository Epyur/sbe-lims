package main

import "testing"

func TestComputeIndicatorStatsNumericAndAggregated(t *testing.T) {
	attrs := []MethodAttribute{
		{ID: "length", Name: "Длина", Level: "experiment"},
		{ID: "group", Name: "Группа", Level: "aggregated"},
		{ID: "missing", Name: "Пусто", Level: "experiment"},
	}
	seriesByReq := map[int64][]map[string]any{
		1: {{"length": 10.0}, {"length": 20.0}},
		2: {{"length": 30.0}},
	}
	aggByReq := map[int64]map[string]any{
		1: {"group": "Г4"},
		2: {"group": "Г2"},
	}

	stats := computeIndicatorStats(attrs, nil, seriesByReq, aggByReq)
	if len(stats) != 2 {
		t.Fatalf("want 2 stats, got %d (%+v)", len(stats), stats)
	}

	var length, group *indicatorStat
	for i := range stats {
		switch stats[i].ID {
		case "length":
			length = &stats[i]
		case "group":
			group = &stats[i]
		}
	}
	if length == nil || group == nil {
		t.Fatalf("missing stats: %+v", stats)
	}
	if !length.HasNumeric || length.Count != 3 || length.Avg != 20 || length.Min != 10 || length.Max != 30 {
		t.Fatalf("length stats wrong: %+v", *length)
	}
	if group.HasNumeric || group.Count != 2 || group.Distribution["Г4"] != 1 || group.Distribution["Г2"] != 1 {
		t.Fatalf("group stats wrong: %+v", *group)
	}
}

func TestComputeIndicatorStatsSkipsEmpty(t *testing.T) {
	attrs := []MethodAttribute{{ID: "a", Name: "A", Level: "experiment"}}
	if got := computeIndicatorStats(attrs, nil, map[int64][]map[string]any{}, map[int64]map[string]any{}); len(got) != 0 {
		t.Fatalf("want no stats, got %+v", got)
	}
}

func TestBuildAnalogues(t *testing.T) {
	rows := []analogueRow{{RequestID: 1}, {RequestID: 2}, {RequestID: 3}}
	agg := map[int64]map[string]any{
		1: {"res": "Г4", "comp": "Не соответствует"},
		2: {"res": "Г2", "comp": "Соответствует"},
	}
	got := buildAnalogues(rows, agg, "res", "comp")
	if got[0].Result != "Г4" || got[0].Compliance != "Не соответствует" {
		t.Fatalf("row1: %+v", got[0])
	}
	if got[2].Result != "" || got[2].Compliance != "" {
		t.Fatalf("row3 should be empty: %+v", got[2])
	}
}

func TestOrderedMethodAttributesIncludesClassificationOutputs(t *testing.T) {
	cfg := &MethodConfig{
		InputParams: []map[string]any{
			{"id": "length", "name": "Длина", "level": "experiment"},
			{"id": "group", "name": "Группа", "level": "aggregated"},
		},
		Classification: []map[string]any{
			{"subjects": []any{
				map[string]any{"input_attribute_id": "length", "output_attribute_id": "grade"},
			}},
		},
	}
	got := orderedMethodAttributes(cfg)
	if len(got) != 3 {
		t.Fatalf("want 3 attributes, got %d (%+v)", len(got), got)
	}
	if got[0].ID != "length" || got[1].ID != "group" || got[2].ID != "grade" {
		t.Fatalf("order wrong: %+v", got)
	}
}
