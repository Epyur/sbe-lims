package main

import (
	"bytes"
	"testing"

	"github.com/xuri/excelize/v2"
)

// buildExportXlsx (2026-08-24, по прямому запросу пользователя — "скачивать
// таблицу всех данных по заявке в формате excel"; по решению пользователя —
// серии + агрегаты/статистика). "Серии" — только атрибуты уровня "experiment"
// (aggregated не имеют значения на серию); "Агрегаты и статистика" — плоский
// key-value список из agg+stats, названия атрибутов там, где есть.
func TestBuildExportXlsx(t *testing.T) {
	cfg := &MethodConfig{
		InputParams: []map[string]any{
			{"id": "mass_loss", "name": "Потеря массы", "level": "experiment"},
			{"id": "grade", "name": "Группа горючести", "level": "aggregated"},
		},
	}
	series := []map[string]any{
		{"mass_loss": 10.0},
		{"mass_loss": 20.0},
	}
	agg := map[string]any{"grade": "Г2"}
	stats := map[string]any{"mass_loss_avg": 15.0, "mass_loss_count": 2.0}

	data, err := buildExportXlsx(cfg, series, agg, stats)
	if err != nil {
		t.Fatalf("buildExportXlsx: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("re-open generated xlsx: %v", err)
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) != 2 || sheets[0] != "Серии" || sheets[1] != "Агрегаты и статистика" {
		t.Fatalf("unexpected sheets: %v", sheets)
	}

	header, err := f.GetRows("Серии")
	if err != nil {
		t.Fatal(err)
	}
	if len(header) != 3 { // header + 2 series rows
		t.Fatalf("got %d rows in Серии, want 3: %v", len(header), header)
	}
	if header[0][0] != "Серия №" || header[0][1] != "Потеря массы" {
		t.Errorf("Серии header = %v", header[0])
	}
	if header[1][0] != "1" || header[1][1] != "10" {
		t.Errorf("Серии row 1 = %v", header[1])
	}
	if header[2][0] != "2" || header[2][1] != "20" {
		t.Errorf("Серии row 2 = %v", header[2])
	}
	// aggregated-level attribute (grade) НЕ должен попасть колонкой в "Серии".
	if len(header[0]) > 2 {
		t.Errorf("Серии header should have exactly 2 columns (no aggregated attrs), got: %v", header[0])
	}

	aggRows, err := f.GetRows("Агрегаты и статистика")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, r := range aggRows[1:] {
		if len(r) >= 2 {
			found[r[0]] = r[1]
		}
	}
	if found["Группа горючести"] != "Г2" {
		t.Errorf("expected 'Группа горючести' = 'Г2' (resolved attribute name), got: %v", found)
	}
	if found["mass_loss_avg"] != "15" {
		t.Errorf("expected raw stats key 'mass_loss_avg' = '15' (no attribute name for this key), got: %v", found)
	}
}
