package main

import (
	"testing"
)

// Фрагменты — реальные ответы QRC (сняты из кэша ekn_products 2026-10-17),
// урезанные до нужных групп. Проверяют, что разбор структурных признаков
// совпадает с тем, как те же карточки разбираются по названию.
func TestMaterialFeatureFromGroups(t *testing.T) {
	cases := []struct {
		name          string
		groups        eknGroupsData
		wantLine      string
		wantModifier  string
		wantType      string
		wantThickness float64
		wantWidthMM   float64
		wantLengthMM  float64
		wantColor     string
	}{
		{
			name: "мембрана 500464 (Logicroof V-RP 1,5, 2,1x20 м)",
			groups: eknGroupsData{
				"Web": map[string]any{
					"name_web": map[string]any{"value": "ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м"},
				},
				"Modif": map[string]any{
					"atr_type_material": map[string]any{"value": []any{"roofing_polym_membr"}},
					"type_ofgidro":      map[string]any{"data": "pvc"},
				},
				"geometric_characteristics": map[string]any{
					"Thikness": map[string]any{"value": map[string]any{"amount": "1.5", "unit": "MILLIMETER"}},
					"Width":    map[string]any{"value": map[string]any{"amount": "2.1", "unit": "METER"}},
					"Length":   map[string]any{"value": map[string]any{"amount": "20", "unit": "METER"}},
				},
				"Spec": map[string]any{
					"atr_color_material": map[string]any{"data": "silk_gray_ral7047"},
					"armirovanie":        map[string]any{"data": "polyester_setka"},
				},
			},
			wantLine: "logicroof v-rp", wantModifier: "", wantType: materialTypeMembrane,
			wantThickness: 1.5, wantWidthMM: 2100, wantLengthMM: 20000, wantColor: "silk_gray_ral7047",
		},
		{
			name: "плита 644712 (LOGICPIR PROF 2385x1185x40)",
			groups: eknGroupsData{
				"Web": map[string]any{
					"name_web": map[string]any{"value": "LOGICPIR PROF Ф/Ф Г1 L-2385х1185х40"},
				},
				"Modif": map[string]any{
					"atr_type_material": map[string]any{"value": []any{"thermal_insulation"}},
				},
				"geometric_characteristics": map[string]any{
					"Thikness": map[string]any{"value": map[string]any{"amount": "40", "unit": "MILLIMETER"}},
					"Width":    map[string]any{"value": map[string]any{"amount": "1185", "unit": "MILLIMETER"}},
					"Length":   map[string]any{"value": map[string]any{"amount": "2385", "unit": "MILLIMETER"}},
				},
			},
			wantLine: "logicpir", wantModifier: "f/f", wantType: materialTypeInsul,
			wantThickness: 40, wantWidthMM: 1185, wantLengthMM: 2385,
		},
		{
			name: "герметик 40415 (без толщины и габарита)",
			groups: eknGroupsData{
				"Web": map[string]any{
					"name_web": map[string]any{"value": "Герметик ПУ ТехноНИКОЛЬ Logicflex для плоских кровель, шт."},
				},
				"Spec": map[string]any{
					"atr_color_material": map[string]any{"data": "gray"},
				},
			},
			wantLine: "logicflex", wantModifier: "", wantType: materialTypeSealant,
			wantThickness: 0, wantColor: "gray",
		},
		{
			name: "геомембрана 149759 (толщина есть, габарит только в названии)",
			groups: eknGroupsData{
				"Web": map[string]any{
					"name_web": map[string]any{"value": "Геомембрана ГЕОПРУФ ПЭВП (1,5) 6х100"},
				},
				"geometric_characteristics": map[string]any{
					"Thikness": map[string]any{"value": map[string]any{"amount": "1.5", "unit": "MILLIMETER"}},
				},
				"Modif": map[string]any{
					"atr_type_material": map[string]any{"value": []any{"geomembrane"}},
				},
			},
			wantType: materialTypeGeomembr, wantThickness: 1.5,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			feat := materialFeatureFromEkn(c.groups)
			if c.wantColor != "" && feat.Color() != c.wantColor {
				t.Errorf("color = %q, want %q", feat.Color(), c.wantColor)
			}
			if c.wantWidthMM > 0 && feat.WidthMM() != c.wantWidthMM {
				t.Errorf("width = %v, want %v", feat.WidthMM(), c.wantWidthMM)
			}
			if c.wantLengthMM > 0 && feat.LengthMM() != c.wantLengthMM {
				t.Errorf("length = %v, want %v", feat.LengthMM(), c.wantLengthMM)
			}
			if feat.Thickness() != c.wantThickness {
				t.Errorf("thickness = %v, want %v", feat.Thickness(), c.wantThickness)
			}

			key, thickness := ParseMaterialFromFeatures(materialFeatures{
				NameWeb:   feat.NameWeb(),
				Type:      feat.RawType(),
				Thickness: feat.Thickness(),
				Width:     feat.WidthMM(),
				Length:    feat.LengthMM(),
				Color:     feat.Color(),
				Armature:  feat.Armature(),
			})
			if c.wantLine != "" && key.Line != c.wantLine {
				t.Errorf("line = %q, want %q", key.Line, c.wantLine)
			}
			if key.Modifier != c.wantModifier {
				t.Errorf("modifier = %q, want %q", key.Modifier, c.wantModifier)
			}
			if c.wantType != "" && key.Type != c.wantType {
				t.Errorf("type = %q, want %q", key.Type, c.wantType)
			}
			if thickness != c.wantThickness {
				t.Errorf("parsed thickness = %v, want %v", thickness, c.wantThickness)
			}
		})
	}
}

// Метры в QRC приходят в метрах — в кэш и отчёт идут миллиметры, чтобы
// толщина (мм) и габарит (мм) сравнивались в одних единицах.
func TestMetricToMM(t *testing.T) {
	cases := []struct {
		v, want float64
		unit    string
	}{
		{2.1, 2100, "METER"},
		{20, 20000, "METER"},
		{1.5, 1.5, "MILLIMETER"},
		{40, 40, ""},
	}
	for _, c := range cases {
		if got := metricToMM(c.v, c.unit); got != c.want {
			t.Errorf("metricToMM(%v, %q) = %v, want %v", c.v, c.unit, got, c.want)
		}
	}
}

// Отсутствие групп не должно паниковать и даёт пустые признаки.
func TestMaterialFeatureEmptyGroups(t *testing.T) {
	feat := materialFeatureFromEkn(eknGroupsData{})
	if feat.NameWeb() != "" || feat.Thickness() != 0 || feat.Color() != "" {
		t.Errorf("пустые группы должны давать пустые признаки, получено %+v", feat)
	}
}

// Ключ материала одинаков независимо от того, пришёл он из карточки QRC или
// из названия — иначе заявки с ЕКН и без него разъехались бы по разным группам.
func TestMaterialKeySameFromQRCAndName(t *testing.T) {
	name := "ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м"
	fromName, _ := ParseMaterialFromName(name)
	fromQRC, _ := ParseMaterialFromFeatures(materialFeatures{
		NameWeb: name, Type: "roofing_polym_membr", Thickness: 1.5,
	})
	if !SameMaterial(fromName, fromQRC) {
		t.Errorf("ключи расходятся: name=%+v qrc=%+v", fromName, fromQRC)
	}
}
