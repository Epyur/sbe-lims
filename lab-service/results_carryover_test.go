package main

import (
	"reflect"
	"testing"
)

// Разбор, из-за которого правило появилось: 2026-09-21 первый боевой прогон
// приборного буфера. Сервер подмешал данные прибора в серию при сохранении, а
// модель формы у клиента их не получила — следующее сохранение из той же формы
// пришло без них и стёрло кривую дыма, температуры по термопарам и параметры
// опроса (заявка 1578, серия 1). Восстановлено из журнала изменений.
func TestCarryOverUnownedValues(t *testing.T) {
	// Поля, которыми распоряжается форма испытателя метода ГГ.
	owned := map[string]bool{
		"mass_before": true, "mass_after": true, "amb_temp": true,
		"comb_length_1": true, "burning_drops": true,
	}

	t.Run("приборные поля переносятся, ручные остаются от формы", func(t *testing.T) {
		before := map[string]any{
			"mass_before":      1634,
			"amb_temp":         "22",
			"smoke_temp_curve": map[string]any{"time": []any{4.9, 14.9}},
			"tp1_smog":         99.2,
			"num_channels":     4,
		}
		values := map[string]any{
			"mass_before":   1634,
			"amb_temp":      "22",
			"mass_after":    1208,
			"comb_length_1": 63,
		}

		carried := carryOverUnownedValues(before, values, owned)

		want := []string{"num_channels", "smoke_temp_curve", "tp1_smog"}
		if !reflect.DeepEqual(carried, want) {
			t.Errorf("перенесены %v, ожидалось %v", carried, want)
		}
		if values["tp1_smog"] != 99.2 || values["num_channels"] != 4 {
			t.Error("приборные значения не доехали в values")
		}
		if values["mass_after"] != 1208 || values["comb_length_1"] != 63 {
			t.Error("ручной ввод испытателя пострадал")
		}
	})

	t.Run("очистка поля формы доезжает", func(t *testing.T) {
		// Испытатель стёр примечание: поле объявлено в форме, во входящем
		// наборе его нет — значит очищено, восстанавливать нельзя.
		before := map[string]any{"amb_temp": "22", "mass_after": 1208}
		values := map[string]any{"amb_temp": "23"}

		carried := carryOverUnownedValues(before, values, owned)

		if len(carried) != 0 {
			t.Errorf("ничего переносить не следовало, перенесено %v", carried)
		}
		if _, exists := values["mass_after"]; exists {
			t.Error("поле формы, которое испытатель очистил, вернулось из старой записи")
		}
		if values["amb_temp"] != "23" {
			t.Error("новое значение поля формы затёрто старым")
		}
	})

	t.Run("состав формы неизвестен — переносим всё недостающее", func(t *testing.T) {
		before := map[string]any{"tp1_smog": 99.2, "mass_after": 1208}
		values := map[string]any{"comb_length_1": 63}

		carried := carryOverUnownedValues(before, values, nil)

		want := []string{"mass_after", "tp1_smog"}
		if !reflect.DeepEqual(carried, want) {
			t.Errorf("перенесены %v, ожидалось %v", carried, want)
		}
	})

	t.Run("присланное значение всегда сильнее сохранённого", func(t *testing.T) {
		before := map[string]any{"tp1_smog": 99.2}
		values := map[string]any{"tp1_smog": 101.5}

		carried := carryOverUnownedValues(before, values, owned)

		if len(carried) != 0 {
			t.Errorf("переносить было нечего, перенесено %v", carried)
		}
		if values["tp1_smog"] != 101.5 {
			t.Error("присланное значение затёрто сохранённым")
		}
	})

	t.Run("пустое значение поля вне формы не воскрешает старое", func(t *testing.T) {
		// Ключ прислан, пусть и с пустым значением, — это осознанная запись.
		before := map[string]any{"tp1_smog": 99.2}
		values := map[string]any{"tp1_smog": nil}

		carryOverUnownedValues(before, values, owned)

		if values["tp1_smog"] != nil {
			t.Error("присланный null затёрт прежним значением")
		}
	})
}
