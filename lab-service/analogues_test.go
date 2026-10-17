package main

import "testing"

// Ступени аналогичности (решения пользователя 2026-10-17):
// 1 — тот же материал, у аналога есть ЕКН;
// 2 — тот же материал, ЕКН не указан (определено по названию);
// 3 — та же линейка, другой модификатор;
// 0 — не аналог.
func TestClassifyAnalogueTier(t *testing.T) {
	current, _ := ParseMaterialFromName("ПВХ Logicroof V-RP 1,5 мм мембрана серая 2,10x20 м")

	cases := []struct {
		name      string
		candidate string
		hasEKN    bool
		wantTier  int
		comment   string
	}{
		{
			name:      "та же линейка, другая фасовка и толщина",
			candidate: "ПВХ Logicroof V-RP 2,0 мм мембрана серая 2,10x15 м", hasEKN: true,
			wantTier: analogueTierSameEKN,
			comment:  "толщина и фасовка не входят в ключ",
		},
		{
			name:      "та же линейка, другой цвет и габарит",
			candidate: "ПВХ Logicroof V-RP 1,5 мм мембрана белая (RAL 9003) 2,10x33 м", hasEKN: true,
			wantTier: analogueTierSameEKN,
			comment:  "цвет не входит в ключ",
		},
		{
			name:      "тот же материал, ЕКН не указан",
			candidate: "Logicroof V-RP 1,2 мм", hasEKN: false,
			wantTier: analogueTierSameName,
		},
		{
			name:      "FR — модификация линейки",
			candidate: "Logicroof V-RP FR 1,2 мм", hasEKN: true,
			wantTier: analogueTierModifier,
		},
		{
			name:      "Middle — модификация линейки",
			candidate: "Logicroof V-RP Middle 1,5 мм", hasEKN: true,
			wantTier: analogueTierModifier,
		},
		{
			name:      "Pro — модификация линейки",
			candidate: "Logicroof V-RP Pro 1,5 мм", hasEKN: true,
			wantTier: analogueTierModifier,
		},
		{
			name:      "другой бренд — не аналог",
			candidate: "ПВХ Ecoplast V-RP 1,5 мм мембрана серая 2,10x20 м", hasEKN: true,
			wantTier: 0,
			comment:  "торговые двойники — разные материалы (решение пользователя)",
		},
		{
			name:      "другой тип материала — не аналог",
			candidate: "LOGICPIR PROF Ф/Ф Г1 L-2385х1185х40", hasEKN: true,
			wantTier: 0,
		},
		{
			name:      "герметик — не аналог мембраны",
			candidate: "Герметик ПУ ТехноНИКОЛЬ Logicflex для плоских кровель, шт.", hasEKN: true,
			wantTier: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			candidate, _ := ParseMaterialFromName(c.candidate)
			got := classifyAnalogueTier(current, candidate, c.hasEKN)
			if got != c.wantTier {
				t.Errorf("tier = %d, want %d (кандидат %q → %+v) %s",
					got, c.wantTier, c.candidate, candidate, c.comment)
			}
		})
	}
}

// Плиты: габарит и толщина не в ключе, модификатор — в ключе.
func TestClassifyAnalogueTierInsulation(t *testing.T) {
	current, _ := ParseMaterialFromName("LOGICPIR PROF Ф/Ф Г1 L-2385х1185х40")

	cases := []struct {
		candidate string
		hasEKN    bool
		want      int
	}{
		{"LOGICPIR PROF Ф/Ф Г1 2400х1200х50", true, analogueTierSameEKN},
		{"LOGICPIR PROF Ф/Ф Г1 L-2385х1185х50", true, analogueTierSameEKN},
		{"LOGICPIR PROF Ф/Ф Г1 L-2385х1185х30", false, analogueTierSameName},
		{"LOGICPIR Полы ФЛ/ФЛ L-1190х590х40", true, analogueTierModifier},
		{"LOGICPIR Ф/Ф L-1190х590х40", true, analogueTierSameEKN},
	}
	for _, c := range cases {
		candidate, _ := ParseMaterialFromName(c.candidate)
		if got := classifyAnalogueTier(current, candidate, c.hasEKN); got != c.want {
			t.Errorf("%q → tier %d, want %d (%+v)", c.candidate, got, c.want, candidate)
		}
	}
}

func TestFormatThickness(t *testing.T) {
	cases := map[float64]string{0: "", 1.5: "1,5", 2: "2", 40: "40", 1.2: "1,2"}
	for in, want := range cases {
		if got := formatThickness(in); got != want {
			t.Errorf("formatThickness(%v) = %q, want %q", in, got, want)
		}
	}
}
