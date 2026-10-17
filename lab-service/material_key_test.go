package main

import "testing"

// Названия — реальные, снятые с lab.objects 2026-10-17.
func TestParseMaterialFromName(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantType  string
		wantLine  string
		wantMod   string
		thickness float64
	}{
		{"простая мембрана", "Logicroof V-RP", materialTypeMembrane, "logicroof v-rp", "", 0},
		{"Middle перед линейкой", "Logicroof Middle V-RP 1,5mm", materialTypeMembrane, "logicroof v-rp", "middle", 1.5},
		{"Middle после линейки", "Logicroof V-RP middle 1.5mm", materialTypeMembrane, "logicroof v-rp", "middle", 1.5},
		{"подчёркивание в модели", "Logicroof V_RP  темно-серая", materialTypeMembrane, "logicroof v-rp", "", 0},
		{"опечатка Lodgicroof", "Lodgicroof V-RP Middle", materialTypeMembrane, "logicroof v-rp", "middle", 0},
		{"Arctic с кириллической с", "ПВХ Logicroof V-RP Arctiс 2,0 мембрана серая 2,10х15 м", materialTypeMembrane, "logicroof v-rp", "arctic", 2.0},
		{"FR", "Logicroof V-RP FR 1,2 мм", materialTypeMembrane, "logicroof v-rp", "fr", 1.2},
		{"Pro", "Logicroof V-RP Pro", materialTypeMembrane, "logicroof v-rp", "pro", 0},
		{"Ecoplast — другой материал", "ПВХ Ecoplast V-RP 1,2 мм мембрана серая 2,10х25 м", materialTypeMembrane, "ecoplast v-rp", "", 1.2},
		{"TSTN — другой материал", "TSTN ПВХ-МЕМБРАНА V-RP 1,2 мм (2,10х25 м)", materialTypeMembrane, "tstn", "", 1.2},
		{"мусор в конце", "ПВХ Logicroof V-RP 1,5 мм мембрана синяя (RAL 5005) 2,10x20 м. сурьма RG-69", materialTypeMembrane, "logicroof v-rp", "", 1.5},
		{"LOGICPIR PROF", "LOGICPIR PROF Ф/Ф Г1 L-2385х1185х40", materialTypeInsul, "logicpir", "f/f", 40},
		{"LOGICPIR полы", "LOGICPIR Полы ФЛ/ФЛ L-1190х590х40", materialTypeInsul, "logicpir", "floors", 40},
		{"LOGICPIR простой", "LOGICPIR Ф/Ф L-1190х590х40", materialTypeInsul, "logicpir", "f/f", 40},
		{"герметик", "Герметик ПУ ТехноНИКОЛЬ Logicflex для плоских кровель, шт.", materialTypeSealant, "logicflex", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, thickness := ParseMaterialFromName(c.input)
			if key.Type != c.wantType {
				t.Errorf("type = %q, want %q", key.Type, c.wantType)
			}
			if key.Line != c.wantLine {
				t.Errorf("line = %q, want %q", key.Line, c.wantLine)
			}
			if key.Modifier != c.wantMod {
				t.Errorf("modifier = %q, want %q", key.Modifier, c.wantMod)
			}
			if thickness != c.thickness {
				t.Errorf("thickness = %v, want %v", thickness, c.thickness)
			}
		})
	}
}

// Разные материалы не должны схлопываться в один ключ.
func TestParseMaterialDistinctMaterials(t *testing.T) {
	keys := map[string]MaterialKey{}
	for _, name := range []string{
		"ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м",
		"ПВХ Ecoplast V-RP 1,5 мм мембрана серая 2,10х20 м",
		"ПВХ ECOROOF V-RP 1,5 мм мембрана серая 2,10х20 м",
		"TSTN ПВХ-МЕМБРАНА V-RP 1,5 мм (2,10х20 м)",
		"ПВХ Logicroof V-RP FR 1,5 мм мембрана серая 2,10x20 м",
		"Logicroof V-RP Pro 1,5",
		"Logicroof V-RP Middle 1,5 мм",
	} {
		k, _ := ParseMaterialFromName(name)
		keys[name] = k
	}
	seen := map[MaterialKey]string{}
	for name, k := range keys {
		if other, ok := seen[k]; ok {
			t.Errorf("%q и %q дали одинаковый ключ %+v", name, other, k)
		}
		seen[k] = name
	}
	if len(seen) != len(keys) {
		t.Fatalf("различных ключей %d, ожидалось %d", len(seen), len(keys))
	}
}

// Одна и та же линейка в разных записях — один материал.
func TestParseMaterialSameMaterialVariants(t *testing.T) {
	variants := []string{
		"Logicroof V-RP 1,2 мм",
		"Logicroof V-RP 1.2 мм",
		"ПВХ Logicroof V-RP 1,2 мм мембрана серая 2,10x25 м",
		"Logicroof V-RP  №п.27519",
		"Logicroof V-RP белая (RAL 9003) 1,5 мм",
	}
	want, _ := ParseMaterialFromName("Logicroof V-RP")
	for _, v := range variants {
		got, _ := ParseMaterialFromName(v)
		if !SameMaterial(got, want) {
			t.Errorf("%q → %+v, ожидался %+v", v, got, want)
		}
	}
}

func TestNormalizeMaterialText(t *testing.T) {
	cases := map[string]string{
		"Logicroof V_RP":                "logicroof v-rp",
		"LOGICPIR  PROF":                "logicpir prof",
		"LOGICPIR L-2385х1185х40":       "logicpir l-2385x1185x40",
		"ПВХ Logicroof V-RP Arctiс":     "пвх logicroof v-rp arctic",
		"  TSTN   ПВХ-МЕМБРАНА  V-RP  ": "tstn пвх-мембрана v-rp",
	}
	for in, want := range cases {
		if got := normalizeMaterialText(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanEkn(t *testing.T) {
	cases := map[string]string{
		"591278": "591278",
		"001517": "001517",
		"ПВХ Ecoplast V-RP 1.2 мм мембрана серая 2.10х25, 591278": "591278",
		"ЕКН 644866, партия 9245":                                 "644866",
		"ПВХ мембрана":                                            "",
		"":                                                        "",
	}
	for in, want := range cases {
		if got := cleanEkn(in); got != want {
			t.Errorf("cleanEkn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaterialKeyHelpers(t *testing.T) {
	mem, _ := ParseMaterialFromName("ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м")
	memFR, _ := ParseMaterialFromName("Logicroof V-RP FR 1,5 мм")
	eco, _ := ParseMaterialFromName("ПВХ Ecoplast V-RP 1,5 мм мембрана серая 2,10x20 м")

	if !MaterialKeyFilled(mem) {
		t.Errorf("ключ мембраны должен быть заполнен: %+v", mem)
	}
	if !SameMaterial(mem, mem) {
		t.Error("ключ не равен сам себе")
	}
	if SameMaterial(mem, memFR) {
		t.Error("FR не должен считаться тем же материалом")
	}
	if !SameMaterialLine(mem, memFR) {
		t.Error("FR — та же линейка (ступень 3)")
	}
	if SameMaterialLine(mem, eco) {
		t.Error("Ecoplast — другая линейка, не ступень 3")
	}
	if SameMaterial(mem, eco) {
		t.Error("Ecoplast — другой материал")
	}
	if label := materialKeyLabel(mem); label == "" || label == "не определён" {
		t.Errorf("подпись материала пустая: %q", label)
	}
}

// Кросс-проверка: разбор названия и разбор структурных признаков QRC
// (карточка 500464 — мембрана Logicroof V-RP 1,5 мм 2,1x20 м) дают один ключ.
func TestParseMaterialFromFeaturesMatchesName(t *testing.T) {
	fromName, thName := ParseMaterialFromName("ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м")
	fromFeatures, thFeatures := ParseMaterialFromFeatures(materialFeatures{
		NameWeb:   "ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м",
		Type:      "roofing_polym_membr",
		Thickness: 1.5,
		Width:     2100,
		Length:    20000,
		Color:     "silk_gray_ral7047",
		Armature:  "polyester_setka",
	})
	if !SameMaterial(fromName, fromFeatures) {
		t.Errorf("ключи расходятся: из названия %+v, из признаков %+v", fromName, fromFeatures)
	}
	if thName != 1.5 || thFeatures != 1.5 {
		t.Errorf("толщина: из названия %v, из признаков %v", thName, thFeatures)
	}
}

// Структурный тип из QRC перебивает угаданный по названию.
func TestParseMaterialFromFeaturesTypeOverrides(t *testing.T) {
	key, thickness := ParseMaterialFromFeatures(materialFeatures{
		NameWeb:   "LOGICPIR PROF Ф/Ф Г1 L-2385х1185х40",
		Type:      "thermal_insulation",
		Thickness: 40,
	})
	if key.Type != materialTypeInsul {
		t.Errorf("type = %q", key.Type)
	}
	if key.Modifier != "f/f" {
		t.Errorf("modifier = %q", key.Modifier)
	}
	if thickness != 40 {
		t.Errorf("thickness = %v", thickness)
	}
}
