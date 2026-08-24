package main

import "testing"

// Проверяет canonicalFieldNames/knownRawFields на реальных наборах raw-ключей писем
// (Comb/Flam/FlamProp, см. json_attr.md и scripts/check_keys.js) — не полную логику
// applyResultPayload (та требует БД), а сами карты: каждый нераспознанный ключ должен
// быть либо в canonicalFieldNames (переименование), либо в knownRawFields (оставлен
// как есть осознанно), иначе он попадёт в лог как "неизвестное поле".
func TestCanonicalFieldNamesAgainstRealKeys(t *testing.T) {
	cases := []struct {
		folder string
		keys   []string
	}{
		{"Comb", []string{
			"Comb_lenth_1", "Comb_lenth_2", "Comb_lenth_3", "Comb_lenth_4",
			"ID", "additional_inf", "aim_indicator", "amb_moist", "amb_pres", "amb_temp",
			"burning_drops", "combustion_time", "exp_date", "inventor", "mass_after",
			"mass_before", "mounting_method", "photo_after", "photo_before", "place",
			"report_date", "sampels_in_date", "series_num", "start_time", "substrate",
			"temp_of_smog", "time_of_max_temp", "tp1_smog", "tp2_smog", "tp3_smog", "tp4_smog",
		}},
		{"Flam", []string{
			"ID", "aim_indicator", "flam_additional_inf", "flam_date_material_in",
			"flam_exp_date", "flam_fixation", "flam_flow_density", "flam_ignition",
			"flam_inventor", "flam_rep_date", "flam_subst", "flam_time", "series_num",
		}},
		{"FlamProp", []string{
			"ID", "additional_inf", "aim_indicator", "amb_moist", "amb_pres", "amb_temp",
			"computing", "exp_date", "exp_time", "flam_time", "inventor",
			"length_of_distraction", "method", "photo_after", "photo_before", "place",
			"report_date", "sampels_in_date", "series_num", "type",
			"calibration_flux_csi", "calibration_flux_firelab", "calibration_flux_lpi",
			"calibration_flux_vniipo", "calibration_length",
		}},
	}

	wantCanon := map[string]string{
		"Comb_lenth_1": "comb_length_1", "flam_fixation": "mounting_method",
		"flam_subst": "substrate", "flam_inventor": "inventor",
		"additional_inf": "additional_info", "flam_additional_inf": "additional_info",
		"flam_exp_date": "exp_date", "flam_rep_date": "report_date",
		"sampels_in_date": "samples_in_date", "flam_date_material_in": "samples_in_date",
	}
	wantUntouched := []string{
		"flam_flow_density", "calibration_flux_csi", "calibration_flux_firelab",
		"calibration_flux_lpi", "calibration_flux_vniipo", "calibration_length",
		"burning_drops", "amb_temp",
	}

	for _, c := range cases {
		for _, k := range c.keys {
			if resultMetaFields[k] {
				continue // маршрутизация, не параметр
			}
			canon, mapped := canonicalFieldNames[k]
			known := knownRawFields[k]
			if !mapped && !known {
				t.Errorf("%s: raw-ключ %q не покрыт ни canonicalFieldNames, ни knownRawFields — попадёт в лог как неизвестное поле", c.folder, k)
			}
			if want, ok := wantCanon[k]; ok && canon != want {
				t.Errorf("%s: %q -> %q, ожидалось %q", c.folder, k, canon, want)
			}
		}
	}

	for _, k := range wantUntouched {
		if _, mapped := canonicalFieldNames[k]; mapped {
			t.Errorf("%q должно остаться без переименования (явное решение пользователя/отдельная величина), но найдено в canonicalFieldNames", k)
		}
	}

	// mounting_method/substrate/inventor/exp_date/report_date уже канонические
	// сами по себе (со стороны Comb) — не должны маппиться на что-то другое.
	for _, k := range []string{"mounting_method", "substrate", "inventor", "exp_date", "report_date"} {
		if canon, mapped := canonicalFieldNames[k]; mapped {
			t.Errorf("%q уже каноническое имя, не должно быть ключом в canonicalFieldNames (нашли -> %q)", k, canon)
		}
	}
}

// resolveResultKey: synonyms конкретного атрибута метода (настроены пользователем в
// конфигураторе, 2026-08-21) должны иметь приоритет над глобальным
// canonicalFieldNames — иначе атрибут, названный не так, как ждёт legacy-письмо
// (напр. "flame_duration" вместо уже устоявшегося "flam_time"), не получал бы
// значения из email-импорта, несмотря на явно настроенный синоним.
func TestResolveResultKeySynonymPriority(t *testing.T) {
	synonyms := map[string]string{"flam_time": "flame_duration"}
	cases := []struct {
		raw       string
		wantKey   string
		wantKnown bool
	}{
		{"flam_time", "flame_duration", true},      // синоним побеждает canonicalFieldNames-отсутствие
		{"flam_fixation", "mounting_method", true}, // нет синонима — canonicalFieldNames как раньше
		{"burning_drops", "burning_drops", true},   // knownRawFields — оставлен как есть
		{"совершенно_неизвестное", "совершенно_неизвестное", false},
	}
	for _, c := range cases {
		key, known := resolveResultKey(c.raw, synonyms)
		if key != c.wantKey || known != c.wantKnown {
			t.Errorf("resolveResultKey(%q) = (%q, %v), want (%q, %v)", c.raw, key, known, c.wantKey, c.wantKnown)
		}
	}
}

// Без synonyms (метод ещё не сконфигурирован, nil map) — поведение как раньше
// (canonicalFieldNames/knownRawFields), без паники на nil map lookup.
func TestResolveResultKeyNilSynonyms(t *testing.T) {
	key, known := resolveResultKey("flam_fixation", nil)
	if key != "mounting_method" || !known {
		t.Errorf("got (%q, %v), want (\"mounting_method\", true)", key, known)
	}
}

// systemRequestFields (2026-08-23) — испытатель/даты/условия среды общие для ЛЮБОГО
// метода (см. TestCanonicalFieldNamesAgainstRealKeys выше — одни и те же поля несут
// Comb/Flam/FlamProp, независимо от метода), по решению пользователя не MethodAttribute,
// а requests.*. Каждый raw-ключ, который canonicalFieldNames/knownRawFields сводит к
// одному из этих понятий, должен попасть в systemRequestFields — иначе applyResultPayload
// молча запишет его в values как обычный атрибут метода (регресс к состоянию до правки).
func TestSystemRequestFieldsCoversUniversalConcepts(t *testing.T) {
	rawToCanonical := map[string]string{
		"flam_inventor": "inventor", "inventor": "inventor",
		"flam_rep_date": "report_date", "report_date": "report_date",
		"flam_date_material_in": "samples_in_date", "sampels_in_date": "samples_in_date",
		"flam_exp_date": "exp_date", "exp_date": "exp_date",
		"amb_temp": "amb_temp", "amb_pres": "amb_pres", "amb_moist": "amb_moist",
	}
	for raw, wantCanon := range rawToCanonical {
		key, known := resolveResultKey(raw, nil)
		if !known || key != wantCanon {
			t.Errorf("resolveResultKey(%q) = (%q, %v), want (%q, true)", raw, key, known, wantCanon)
		}
		if !systemRequestFields[key] {
			t.Errorf("канонический ключ %q (из raw-имени %q) должен быть в systemRequestFields", key, raw)
		}
	}
	// additional_info/substrate/mounting_method — тоже общие ИМЕНА полей у разных
	// методов, но содержание метод-специфичное (описание подложки/крепления/заметки
	// разное для Comb/Flam) — НЕ должны стать системными (в отличие от инвентора/дат/
	// условий среды, которые объективно идентичны независимо от метода).
	for _, canon := range []string{"additional_info", "substrate", "mounting_method"} {
		if systemRequestFields[canon] {
			t.Errorf("%q — метод-специфичное содержание, не должно быть в systemRequestFields", canon)
		}
	}
}
