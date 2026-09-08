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

// Живой сценарий: пользователь ищет по материалу («Logicroof V-RP»), затем просит
// «дай распределение по методам» — group_by=method должен посчитать это НАД
// результатами поиска (все совпавшие заявки), не только над показанной страницей.
func TestGroupByDimensionMethod(t *testing.T) {
	scored := []scoredRequest{
		{req: Request{ID: 1, MethodID: 10}},
		{req: Request{ID: 2, MethodID: 10}},
		{req: Request{ID: 3, MethodID: 20}},
		{req: Request{ID: 4, MethodID: 0}}, // метод не указан
	}
	methodByID := map[int64]string{10: "ГГ (горючесть)", 20: "РП (распространение пламени)"}
	series := groupByDimension(scored, "method", nil, nil, methodByID)
	if len(series) != 3 {
		t.Fatalf("expected 3 groups (10, 20, 0), got %d: %+v", len(series), series)
	}
	// Отсортировано по убыванию count — метод 10 (2 заявки) должен быть первым.
	if series[0].Label != "ГГ (горючесть)" || series[0].Count != 2 {
		t.Fatalf("expected top group ГГ with count 2, got %+v", series[0])
	}
	found0 := false
	for _, p := range series {
		if p.Label == "(не указано)" {
			found0 = true
			if p.Count != 1 {
				t.Fatalf("expected 1 request without method, got %d", p.Count)
			}
		}
	}
	if !found0 {
		t.Fatal("expected a '(не указано)' group for MethodID=0")
	}
}

// Неизвестный id (нет в справочнике) — не должен ломаться, отдаёт "#id" как ярлык.
func TestGroupByDimensionUnknownID(t *testing.T) {
	scored := []scoredRequest{{req: Request{ID: 1, ProjectID: 999}}}
	series := groupByDimension(scored, "project", map[int64]Project{}, nil, nil)
	if len(series) != 1 || series[0].Label != "#999" {
		t.Fatalf("expected fallback label #999, got %+v", series)
	}
}

// group_by=day/week/month над результатом поиска — та же bucket-логика, что и
// group_by без query, только считает по совпавшим заявкам, а не по всей БД.
func TestGroupByPeriod(t *testing.T) {
	scored := []scoredRequest{
		{req: Request{ID: 1, CreatedAt: "2026-08-10T10:00:00Z"}},
		{req: Request{ID: 2, CreatedAt: "2026-08-10T12:00:00Z"}},
		{req: Request{ID: 3, CompletedAt: "2026-08-11T09:00:00Z"}},
	}
	series := groupByPeriod(scored, "day", "", "")
	byPeriod := map[string]searchGroupPoint{}
	for _, p := range series {
		byPeriod[p.Period] = p
	}
	if byPeriod["2026-08-10"].Arrived != 2 {
		t.Fatalf("expected 2 arrived on 2026-08-10, got %+v", byPeriod["2026-08-10"])
	}
	if byPeriod["2026-08-11"].Completed != 1 {
		t.Fatalf("expected 1 completed on 2026-08-11, got %+v", byPeriod["2026-08-11"])
	}
}

// dateFrom/dateTo сужают group_by=day/week/month так же, как обычный список.
func TestGroupByPeriodRespectsDateRange(t *testing.T) {
	scored := []scoredRequest{
		{req: Request{ID: 1, CreatedAt: "2026-07-01T10:00:00Z"}}, // вне диапазона
		{req: Request{ID: 2, CreatedAt: "2026-08-10T10:00:00Z"}}, // в диапазоне
	}
	series := groupByPeriod(scored, "day", "2026-08-01", "2026-08-31")
	total := 0
	for _, p := range series {
		total += p.Arrived + p.Completed
	}
	if total != 1 {
		t.Fatalf("expected only 1 request within date range, got total=%d series=%+v", total, series)
	}
}
