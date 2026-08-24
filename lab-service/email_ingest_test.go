package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// buildMimeMessage — собирает сырое RFC822-сообщение из строк (CRLF, как в реальной
// почте) для тестов extractMailTextAndAttachments — без похода в сеть/IMAP.
func buildMimeMessage(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n"))
}

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

// 2026-08-24: раньше письмо-вложение (реальное фото) читалось и тихо отбрасывалось на
// этом же проходе MIME (никакого default-case не было) — email_ingest.go хранил только
// yandex-ссылку из JSON, которая, как выяснилось, недоступна анонимно. Проверяем, что
// вложение теперь действительно извлекается, а разбор текста/JSON не пострадал.
func TestExtractMailTextAndAttachmentsCapturesAttachment(t *testing.T) {
	photoBytes := []byte("fake-jpeg-bytes")
	raw := buildMimeMessage(
		`From: lab@example.com`,
		`To: lab@example.com`,
		`Content-Type: multipart/mixed; boundary="B1"`,
		``,
		`--B1`,
		`Content-Type: text/plain; charset=utf-8`,
		``,
		`{"ID":"1","type":"result","photo_before":""}`,
		`--B1`,
		`Content-Type: image/jpeg; name="photo.jpg"`,
		`Content-Disposition: attachment; filename="photo.jpg"`,
		`Content-Transfer-Encoding: base64`,
		``,
		base64.StdEncoding.EncodeToString(photoBytes),
		`--B1--`,
		``,
	)

	text, attachments, err := extractMailTextAndAttachments(raw)
	if err != nil {
		t.Fatalf("extractMailTextAndAttachments: %v", err)
	}
	if !strings.Contains(text, `"ID":"1"`) {
		t.Errorf("text extraction regressed: got %q", text)
	}
	if len(attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(attachments))
	}
	a := attachments[0]
	if a.Filename != "photo.jpg" {
		t.Errorf("Filename = %q, want %q", a.Filename, "photo.jpg")
	}
	if a.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want %q", a.ContentType, "image/jpeg")
	}
	if string(a.Data) != string(photoBytes) {
		t.Errorf("Data = %q, want %q (base64 decode of attachment failed)", a.Data, photoBytes)
	}
}

// Вложение внутри вложенного multipart (multipart/mixed > multipart/related для текста,
// сестринская часть — вложение на верхнем уровне) должно пробрасываться наверх, а не
// теряться на рекурсии.
func TestExtractMailTextAndAttachmentsNestedMultipart(t *testing.T) {
	raw := buildMimeMessage(
		`Content-Type: multipart/mixed; boundary="OUTER"`,
		``,
		`--OUTER`,
		`Content-Type: multipart/related; boundary="INNER"`,
		``,
		`--INNER`,
		`Content-Type: text/plain; charset=utf-8`,
		``,
		`{"ID":"2"}`,
		`--INNER--`,
		`--OUTER`,
		`Content-Type: application/octet-stream; name="report.jpg"`,
		`Content-Disposition: attachment; filename="report.jpg"`,
		``,
		`raw-bytes-no-encoding`,
		`--OUTER--`,
		``,
	)

	text, attachments, err := extractMailTextAndAttachments(raw)
	if err != nil {
		t.Fatalf("extractMailTextAndAttachments: %v", err)
	}
	if !strings.Contains(text, `"ID":"2"`) {
		t.Errorf("nested text extraction regressed: got %q", text)
	}
	if len(attachments) != 1 || attachments[0].Filename != "report.jpg" {
		t.Fatalf("got %+v, want exactly one attachment named report.jpg", attachments)
	}
}

// Часть без имени файла (ни Content-Disposition, ни Content-Type name=) — не текст,
// значит не JSON-полезная нагрузка, но и не опознаваемое вложение — не сохраняем (не
// гадаем об имени).
func TestExtractMailTextAndAttachmentsUnnamedPartIgnored(t *testing.T) {
	raw := buildMimeMessage(
		`Content-Type: multipart/mixed; boundary="B1"`,
		``,
		`--B1`,
		`Content-Type: text/plain`,
		``,
		`{"ID":"3"}`,
		`--B1`,
		`Content-Type: application/octet-stream`,
		``,
		`no-name-no-disposition`,
		`--B1--`,
		``,
	)

	_, attachments, err := extractMailTextAndAttachments(raw)
	if err != nil {
		t.Fatalf("extractMailTextAndAttachments: %v", err)
	}
	if len(attachments) != 0 {
		t.Errorf("got %d attachments, want 0 (unnamed part must be ignored)", len(attachments))
	}
}

// Реальная ссылка из письма Comb этой сессии (см. AGENTS.md "фото в протоколе") — конечный
// сегмент пути должен совпасть с тем, что реально пришло вложением.
func TestExtractYandexFormsFilename(t *testing.T) {
	cases := []struct {
		url      string
		wantName string
		wantOK   bool
	}{
		{
			"https://forms.yandex.ru/cloud/files?path=%2F4488571%2F6a8c1a2f902902b1f7c9d9a4_17875666329476692133998677536992.jpg",
			"6a8c1a2f902902b1f7c9d9a4_17875666329476692133998677536992.jpg", true,
		},
		{"", "", false},
		{"не-url", "", false},
		{"https://forms.yandex.ru/cloud/files?other=1", "", false},
	}
	for _, c := range cases {
		name, ok := extractYandexFormsFilename(c.url)
		if name != c.wantName || ok != c.wantOK {
			t.Errorf("extractYandexFormsFilename(%q) = (%q, %v), want (%q, %v)", c.url, name, ok, c.wantName, c.wantOK)
		}
	}
}

func TestMatchAttachmentByFilename(t *testing.T) {
	attachments := []mailAttachment{
		{Filename: "a.jpg", Data: []byte("A")},
		{Filename: "b.jpg", Data: []byte("B")},
		{Filename: "dup.jpg", Data: []byte("C1")},
		{Filename: "dup.jpg", Data: []byte("C2")},
	}
	if got, ok := matchAttachmentByFilename("a.jpg", attachments); !ok || string(got.Data) != "A" {
		t.Errorf("exact match: got (%+v, %v), want (a.jpg/A, true)", got, ok)
	}
	if _, ok := matchAttachmentByFilename("missing.jpg", attachments); ok {
		t.Errorf("no match should return false")
	}
	if _, ok := matchAttachmentByFilename("dup.jpg", attachments); ok {
		t.Errorf("ambiguous match (2 attachments, same name) should return false, not guess")
	}
}

// 2026-08-24: письмо прибора (payload содержит "mesure_data") раньше пропускалось
// целиком (решение 2026-08-19), из-за чего statistics/experiment_params — реальные
// показатели метода ГГ — никогда не заполнялись, хотя не являются сырыми массивами по
// каналам (те и сейчас не заводим, см. фикстуру ниже — mesure_data сюда не передаётся
// вовсе, только statistics/experiment_params, ровно то, что реально извлекается).
// Фикстура — по мотивам реального письма external_id=698 (найдено пользователем).
func TestExtractInstrumentFields(t *testing.T) {
	payload := map[string]any{
		"statistics": map[string]any{
			"by_channel": map[string]any{
				"1": map[string]any{"max_temp": 905.3, "max_time": 140.8},
				"2": map[string]any{"max_temp": 926.7, "max_time": 130.8},
				"3": map[string]any{"max_temp": 895.7, "max_time": 130.8},
				"4": map[string]any{"max_temp": 926.5, "max_time": 140.8},
			},
			"average_max_temp": 913.55,
		},
		"experiment_params": map[string]any{
			"pre_time_seconds":      15.0,
			"main_time_seconds":     600.0,
			"post_time_seconds":     30.0,
			"num_channels":          4.0,
			"poll_interval_seconds": nil, // null в реальном письме — не должен попасть в результат
		},
	}
	got := extractInstrumentFields(payload)

	wantChannels := map[string]any{"tp1_smog": 905.3, "tp2_smog": 926.7, "tp3_smog": 895.7, "tp4_smog": 926.5}
	for k, want := range wantChannels {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if got["temp_of_smog"] != 913.55 {
		t.Errorf("temp_of_smog = %v, want 913.55 (statistics.average_max_temp напрямую)", got["temp_of_smog"])
	}
	// (140.8+130.8+130.8+140.8)/4 = 135.8 — среднее по каналам (решение пользователя).
	if timeOfMax, ok := got["time_of_max_temp"].(float64); !ok || timeOfMax != 135.8 {
		t.Errorf("time_of_max_temp = %v, want 135.8 (среднее max_time по 4 каналам)", got["time_of_max_temp"])
	}
	for k, want := range map[string]any{"pre_time_seconds": 15.0, "main_time_seconds": 600.0, "post_time_seconds": 30.0, "num_channels": 4.0} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if _, ok := got["poll_interval_seconds"]; ok {
		t.Errorf("poll_interval_seconds был null в payload — не должен попасть в результат, got %v", got["poll_interval_seconds"])
	}
	// mesure_data (сырые массивы) не передавались в payload вовсе — но даже если бы
	// были, extractInstrumentFields их не читает и не может случайно вернуть.
	if _, ok := got["mesure_data"]; ok {
		t.Errorf("extractInstrumentFields не должен возвращать сырые данные прибора")
	}
}

// Письмо-форма (без statistics/experiment_params) — extractInstrumentFields должен
// молча вернуть пустую карту, не паниковать на отсутствующих полях.
func TestExtractInstrumentFieldsNoInstrumentData(t *testing.T) {
	payload := map[string]any{"ID": "698", "type": "result", "mass_before": "2462"}
	got := extractInstrumentFields(payload)
	if len(got) != 0 {
		t.Errorf("got %+v, want empty map (письмо-форма не несёт statistics/experiment_params)", got)
	}
}

// Регресс на реальных данных (external_id=698, 2026-08-24): письмо-форма несёт
// "tp1_smog":"" как заглушку "заполнит прибор" — если письмо прибора обработано РАНЬШЕ
// (порядок обработки писем одной серии не гарантирован) и уже записало реальное число,
// повторная обработка формы не должна затирать его пустой строкой.
func TestSetIfMeaningfulDoesNotOverwriteWithBlank(t *testing.T) {
	values := map[string]any{"tp1_smog": 905.3}
	setIfMeaningful(values, "tp1_smog", "") // письмо-форма приходит после письма прибора
	if values["tp1_smog"] != 905.3 {
		t.Errorf("tp1_smog = %v, want 905.3 (пустая строка не должна затирать уже сохранённое число)", values["tp1_smog"])
	}

	// Пусто -> пусто: ключа ещё не было, пустое значение — законный первый результат
	// (напр. поле реально не заполнено ни в одном письме).
	values2 := map[string]any{}
	setIfMeaningful(values2, "flam_time", "")
	if v, ok := values2["flam_time"]; !ok || v != "" {
		t.Errorf("got %v (ok=%v), want (\"\", true) — не было предыдущего значения для сохранения", v, ok)
	}

	// Настоящее новое значение поверх настоящего старого — обычная перезапись, не блокируется.
	values3 := map[string]any{"mass_before": "2400"}
	setIfMeaningful(values3, "mass_before", "2462")
	if values3["mass_before"] != "2462" {
		t.Errorf("mass_before = %v, want \"2462\" (непустое поверх непустого — обычное обновление)", values3["mass_before"])
	}
}

// Регресс на реальных данных (external_id=698, живой прогон fetch-mail-photos,
// 2026-08-24): вложение письма несёт ОРИГИНАЛЬНОЕ имя файла, а ссылка в JSON — то же имя с
// добавленным Яндексом хеш-префиксом "<hex>_<имя>". Точное совпадение здесь ложно
// провалилось бы — нужен хвостовой матч.
func TestMatchAttachmentByFilenameYandexHashPrefix(t *testing.T) {
	attachments := []mailAttachment{
		{Filename: "17875666329476692133998677536992.jpg", Data: []byte("BEFORE")},
		{Filename: "178756959825993379636321324362.jpg", Data: []byte("AFTER")},
	}
	got, ok := matchAttachmentByFilename("6a8c1a2f902902b1f7c9d9a4_17875666329476692133998677536992.jpg", attachments)
	if !ok || string(got.Data) != "BEFORE" {
		t.Fatalf("got (%+v, %v), want the BEFORE attachment matched via suffix", got, ok)
	}
	got, ok = matchAttachmentByFilename("6a8c25c3493639ac32d46748_178756959825993379636321324362.jpg", attachments)
	if !ok || string(got.Data) != "AFTER" {
		t.Fatalf("got (%+v, %v), want the AFTER attachment matched via suffix", got, ok)
	}
}

func TestMatchPhotoFields(t *testing.T) {
	beforeURL := "https://forms.yandex.ru/cloud/files?path=%2Fx%2Fbefore.jpg"
	afterURL := "https://forms.yandex.ru/cloud/files?path=%2Fx%2Fafter.jpg"
	attachments := []mailAttachment{
		{Filename: "before.jpg", Data: []byte("BEFORE")},
		{Filename: "after.jpg", Data: []byte("AFTER")},
	}

	t.Run("both fields match", func(t *testing.T) {
		payload := map[string]any{"photo_before": beforeURL, "photo_after": afterURL}
		got := matchPhotoFields(payload, attachments)
		if len(got) != 2 || string(got["photo_before"].Data) != "BEFORE" || string(got["photo_after"].Data) != "AFTER" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("empty field absent from result", func(t *testing.T) {
		payload := map[string]any{"photo_before": beforeURL, "photo_after": ""}
		got := matchPhotoFields(payload, attachments)
		if _, ok := got["photo_after"]; ok {
			t.Errorf("photo_after was empty in payload, must not appear in result")
		}
		if _, ok := got["photo_before"]; !ok {
			t.Errorf("photo_before should have matched")
		}
	})

	t.Run("no attachments at all, no panic", func(t *testing.T) {
		payload := map[string]any{"photo_before": beforeURL, "photo_after": afterURL}
		got := matchPhotoFields(payload, nil)
		if len(got) != 0 {
			t.Errorf("got %+v, want empty (no attachments to match against)", got)
		}
	})
}
