package main

import "testing"

// Живой сценарий из дизайна: слово из запроса может найтись в РАЗНЫХ полях
// (характеристики объекта + заказчик), а не в одном — заявка всё равно должна
// совпасть, если КАЖДОЕ слово запроса нашлось хоть где-то.
func TestMatchSearchWordsAcrossFields(t *testing.T) {
	req := Request{ID: 1, ProjectID: 8, ObjectID: 5}
	projectByID := map[int64]Project{8: {ID: 8, Name: "ООО «Ромашка»"}}
	inventorByID := map[int64]Inventor{}
	objectByID := map[int64]Object{5: {ID: 5, Name: "Панель фасадная", Characteristics: map[string]any{
		"Материал": "анодированный алюминий",
	}}}
	measurements := map[int64][]MeasurementResult{}
	aggregated := map[int64][]AggregatedResult{}

	fields := buildSearchFields(req, projectByID, inventorByID, objectByID, measurements, aggregated)
	ok, matched := matchSearchWords(fields, searchWords("алюминий ромашка"))
	if !ok {
		t.Fatalf("expected match across fields, got no match")
	}
	if len(matched) != 2 {
		t.Fatalf("expected 2 matched fields (характеристики + заказчик), got %d: %+v", len(matched), matched)
	}
}

// Слово запроса, которого нет НИ В ОДНОМ поле, — заявка не должна совпасть, даже
// если остальные слова нашлись (AND по словам, не OR).
func TestMatchSearchWordsRequiresAllWords(t *testing.T) {
	req := Request{ID: 1}
	objectByID := map[int64]Object{}
	fields := buildSearchFields(req, nil, nil, objectByID, nil, nil)
	fields = append(fields, searchField{Label: "Название", Text: "Панель фасадная"})
	ok, _ := matchSearchWords(fields, searchWords("панель титан"))
	if ok {
		t.Fatalf("expected no match: 'титан' is not present in any field")
	}
}

// Транслитерация: технический термин латиницей в данных должен находиться
// запросом на кириллице и наоборот (тот же класс бага, что уже чинили для
// писем/документов — ПИР/PIR).
func TestMatchSearchWordsTranslit(t *testing.T) {
	fields := []searchField{{Label: "Характеристики объекта", Text: "Марка: LOGICPIR PROF"}}
	ok, matched := matchSearchWords(fields, searchWords("пир"))
	if !ok {
		t.Fatalf("expected 'пир' (cyrillic) to match 'PIR' (latin) via translit")
	}
	if len(matched) != 1 || matched[0].Field != "Характеристики объекта" {
		t.Fatalf("unexpected matched_in: %+v", matched)
	}
}

// Наблюдения/измерения (event_log-подобные значения) — measurement_results.values
// тоже должны участвовать в поиске, не только object.characteristics.
func TestMatchSearchWordsMeasurementResults(t *testing.T) {
	req := Request{ID: 42}
	measurements := map[int64][]MeasurementResult{
		42: {{RequestID: 42, Values: map[string]any{"Наблюдение": "Появление трещины на образце №2"}}},
	}
	fields := buildSearchFields(req, nil, nil, nil, measurements, nil)
	// "трещин" — короткая основа без падежного окончания: буквальный поиск
	// подстроки (как и везде в этом проекте) не учитывает падежи, "трещина"
	// не найдёт "трещины" — см. AGENTS.md про этот класс поведения.
	ok, matched := matchSearchWords(fields, searchWords("трещин"))
	if !ok {
		t.Fatalf("expected match in measurement results")
	}
	if len(matched) != 1 || matched[0].Field != "Результаты измерений" {
		t.Fatalf("unexpected matched_in: %+v", matched)
	}
}

// Короткие предлоги/союзы не должны требоваться как отдельные обязательные слова —
// иначе почти ни один многословный запрос не совпал бы буквально.
func TestSearchWordsDropsStopWords(t *testing.T) {
	words := searchWords("панель из анодированного алюминия")
	want := []string{"панель", "анодированного", "алюминия"}
	if len(words) != len(want) {
		t.Fatalf("got %v, want %v", words, want)
	}
	for i, w := range want {
		if words[i] != w {
			t.Fatalf("got %v, want %v", words, want)
		}
	}
}

// Пустой/бессмысленный запрос (только предлоги) — не должен давать ложных
// совпадений "по умолчанию".
func TestMatchSearchWordsEmptyQuery(t *testing.T) {
	fields := []searchField{{Label: "Название", Text: "Панель фасадная"}}
	ok, _ := matchSearchWords(fields, nil)
	if ok {
		t.Fatalf("expected no match for empty word list")
	}
}
