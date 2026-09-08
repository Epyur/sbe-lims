package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- Свободный поиск заявок по любому атрибуту (2026-09-07) ----
// См. docs/superpowers/specs/2026-09-07-lims-attribute-search-design.md. GET
// /api/lab/requests раньше не принимал вообще никаких параметров поиска — вся
// "фильтрация" происходила на клиенте после полной выгрузки, а материал/изготовитель/
// характеристики/наблюдения физически недостижимы клиенту (JSONB в objects/
// measurement_results/aggregated_results, не отдаются списком заявок). Данных сейчас
// сотни/низкие тысячи — вместо материализованного FTS-индекса (как в photo-service)
// сервер "расплющивает" нужные таблицы в память на каждый запрос поиска (несколько
// батч-запросов, не N+1) и ищет вхождение слов прямо там.

// searchField — одно поле-источник заявки для поиска (подпись + текст).
type searchField struct {
	Label string
	Text  string
}

// matchedField — поле, в котором нашлось совпадение (для честного объяснения агенту).
type matchedField struct {
	Field   string `json:"field"`
	Snippet string `json:"snippet"`
}

// searchRequestResult — одна заявка в ответе поиска.
type searchRequestResult struct {
	ID             int64          `json:"id"`
	Title          string         `json:"title"`
	CustomerNumber string         `json:"customer_number"`
	LabNumber      string         `json:"lab_number"`
	Status         string         `json:"status"`
	Result         string         `json:"result"`
	Compliance     string         `json:"compliance"`
	ProjectID      int64          `json:"project_id"`
	InventorID     int64          `json:"inventor_id"`
	MethodID       int64          `json:"method_id"`
	CreatedAt      string         `json:"created_at"`
	CompletedAt    string         `json:"completed_at"`
	MatchedIn      []matchedField `json:"matched_in"`
}

// scoredRequest — заявка, совпавшая с поисковым запросом, вместе с полями, где
// нашлось совпадение (для matched_in в выдаче без group_by).
type scoredRequest struct {
	req       Request
	matchedIn []matchedField
}

// searchGroupPoint — одна точка агрегированной статистики по совпавшим заявкам
// (group_by вместе с q) — {label: "...", count: N} плюс period-специфичные поля,
// когда group_by временной (day/week/month).
type searchGroupPoint struct {
	Label     string `json:"label"`
	Count     int    `json:"count"`
	Period    string `json:"period,omitempty"`
	Arrived   int    `json:"arrived,omitempty"`
	Completed int    `json:"completed,omitempty"`
}

// stopWords — короткие предлоги/союзы, отбрасываются при разборе запроса: сами по
// себе слишком часто встречаются в любом тексте, чтобы нести смысл поиска.
var stopWords = map[string]bool{
	"и": true, "с": true, "из": true, "на": true, "по": true, "для": true,
	"от": true, "за": true, "к": true, "о": true, "в": true, "у": true,
	"со": true, "во": true, "об": true, "то": true, "не": true,
}

var cyrillicToLatin = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o",
	'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "c",
	'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu",
	'я': "ya",
}

// translit — та же транслитерация кириллица→латиница, что уже используется в
// веб-/Obsidian-агенте для поиска по письмам/документам (баг ПИР/PIR) — здесь
// нужна по той же причине: технические термины в характеристиках/материалах
// нередко записаны латиницей даже в русском тексте.
func translit(s string) string {
	var b strings.Builder
	for _, r := range s {
		if v, ok := cyrillicToLatin[r]; ok {
			b.WriteString(v)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// searchWords разбивает запрос на значимые слова: короче 2 символов и стоп-слова
// отбрасываются.
func searchWords(q string) []string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(q)))
	words := make([]string, 0, len(fields))
	for _, w := range fields {
		if len([]rune(w)) < 2 || stopWords[w] {
			continue
		}
		words = append(words, w)
	}
	return words
}

// stringifyJSONValue — печатное представление значения JSONB-поля для поиска/сниппета.
func stringifyJSONValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s := stringifyJSONValue(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	case map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// flattenJSONMap — "ключ: значение" по одной строке на ключ, для полей вида
// characteristics/values/result_data.
func flattenJSONMap(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		val := stringifyJSONValue(m[k])
		if val == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", k, val))
	}
	return strings.Join(lines, "\n")
}

// buildSearchFields собирает поля-источники одной заявки для поиска — см. таблицу
// в дизайн-документе. objectByID/projectByID/inventorByID/measurementsByRequest/
// aggregatedByRequest — уже загруженные батчем справочники (не по одной заявке).
func buildSearchFields(
	req Request,
	projectByID map[int64]Project,
	inventorByID map[int64]Inventor,
	objectByID map[int64]Object,
	measurementsByRequest map[int64][]MeasurementResult,
	aggregatedByRequest map[int64][]AggregatedResult,
) []searchField {
	fields := make([]searchField, 0, 8)
	add := func(label, text string) {
		if strings.TrimSpace(text) != "" {
			fields = append(fields, searchField{Label: label, Text: text})
		}
	}

	add("Название", req.Title)
	add("Описание", req.Description)
	if p, ok := projectByID[req.ProjectID]; ok {
		add("Заказчик", p.Name)
	}
	if inv, ok := inventorByID[req.InventorID]; ok {
		add("Испытатель", inv.Name)
	}
	add("Номера", strings.Join([]string{req.CustomerNumber, req.LabNumber, req.ExternalID}, " "))
	if obj, ok := objectByID[req.ObjectID]; ok {
		add("Объект", obj.Name)
		add("Характеристики объекта", flattenJSONMap(obj.Characteristics))
	}
	if results := measurementsByRequest[req.ID]; len(results) > 0 {
		var b strings.Builder
		for _, mr := range results {
			if s := flattenJSONMap(mr.Values); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		}
		add("Результаты измерений", b.String())
	}
	if aggs := aggregatedByRequest[req.ID]; len(aggs) > 0 {
		var b strings.Builder
		for _, ar := range aggs {
			if s := flattenJSONMap(ar.ResultData); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		}
		add("Итоговые результаты", b.String())
	}
	return fields
}

// matchSearchWords проверяет, что КАЖДОЕ слово запроса нашлось хотя бы в одном поле
// (слова могут быть в разных полях), и возвращает поля, где что-то нашлось —
// честная база для matched_in в ответе агенту. Сравнение регистронезависимое +
// транслитерация кириллица↔латиница в обе стороны.
func matchSearchWords(fields []searchField, words []string) (bool, []matchedField) {
	if len(words) == 0 {
		return false, nil
	}
	matchedFieldSet := map[string]bool{}
	matched := make([]matchedField, 0, len(fields))
	for _, f := range fields {
		lower := strings.ToLower(f.Text)
		lowerTranslit := translit(lower)
		hit := false
		for _, w := range words {
			wt := translit(w)
			if strings.Contains(lower, w) || strings.Contains(lowerTranslit, wt) {
				hit = true
				break
			}
		}
		if hit && !matchedFieldSet[f.Label] {
			matchedFieldSet[f.Label] = true
			matched = append(matched, matchedField{Field: f.Label, Snippet: snippetFor(f.Text)})
		}
	}
	if len(matchedFieldSet) == 0 {
		return false, nil
	}
	// Заявка совпадает, только если КАЖДОЕ слово запроса нашлось хоть где-то —
	// проверяем по объединённому тексту всех полей разом (слова могут быть в разных полях).
	var all strings.Builder
	for _, f := range fields {
		all.WriteString(strings.ToLower(f.Text))
		all.WriteString("\n")
	}
	allText := all.String()
	allTranslit := translit(allText)
	for _, w := range words {
		wt := translit(w)
		if !strings.Contains(allText, w) && !strings.Contains(allTranslit, wt) {
			return false, nil
		}
	}
	return true, matched
}

const snippetMaxLen = 160

// snippetFor обрезает длинное поле до короткого читаемого фрагмента для ответа агенту.
func snippetFor(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", "; "))
	r := []rune(text)
	if len(r) <= snippetMaxLen {
		return text
	}
	return string(r[:snippetMaxLen]) + "…"
}

// handleSearchRequests — GET /api/lab/requests/search?q=&limit=&lab=&status=&
// compliance=&date_from=&date_to=&group_by= (viewer+, та же видимость, что у
// обычного списка). group_by (day/week/month/project/inventor/method) считает
// агрегированную статистику ПО ВСЕМ совпавшим заявкам (не только по показанным
// limit) — так «распределение по методам среди найденных по запросу» не требует
// вытягивать все сырые заявки, чтобы посчитать вручную.
func (s *Server) handleSearchRequests(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "q is required"})
		return
	}
	words := searchWords(q)
	if len(words) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "q has no significant words"})
		return
	}
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 50 {
		limit = 50
	}
	groupBy := strings.TrimSpace(r.URL.Query().Get("group_by"))
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	complianceFilter := strings.TrimSpace(r.URL.Query().Get("compliance"))
	dateFrom := strings.TrimSpace(r.URL.Query().Get("date_from"))
	dateTo := strings.TrimSpace(r.URL.Query().Get("date_to"))
	labQuery := strings.TrimSpace(r.URL.Query().Get("lab"))

	ctx := r.Context()
	email := currentEmail(r)
	requests, err := s.loadVisibleRequests(ctx, email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	labResolvedName := ""
	if labQuery != "" {
		matched, names, err := s.matchingLabIDs(ctx, labQuery)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if len(matched) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("lab %q not found", labQuery)})
			return
		}
		labResolvedName = strings.Join(names, ", ")
		filtered := requests[:0:0]
		for _, req := range requests {
			if matched[req.LabID] {
				filtered = append(filtered, req)
			}
		}
		requests = filtered
	}
	requests = filterRequestsForSearch(requests, r.URL.Query())
	totalVisible := len(requests)
	filtersApplied := map[string]any{
		"lab": labResolvedName, "status": statusFilter, "compliance": complianceFilter,
		"date_from": dateFrom, "date_to": dateTo,
	}
	if totalVisible == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"requests": []searchRequestResult{}, "total_matched": 0, "total_visible": 0, "filters_applied": filtersApplied,
		})
		return
	}

	projectByID, inventorByID, methodByID, objectByID, measurementsByRequest, aggregatedByRequest, err := s.loadSearchLookups(ctx, requests)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	scoredList := make([]scoredRequest, 0, len(requests))
	for _, req := range requests {
		fields := buildSearchFields(req, projectByID, inventorByID, objectByID, measurementsByRequest, aggregatedByRequest)
		ok, matchedIn := matchSearchWords(fields, words)
		if ok {
			scoredList = append(scoredList, scoredRequest{req: req, matchedIn: matchedIn})
		}
	}
	totalMatched := len(scoredList)

	if groupBy == "day" || groupBy == "week" || groupBy == "month" {
		series := groupByPeriod(scoredList, groupBy, dateFrom, dateTo)
		writeJSON(w, http.StatusOK, map[string]any{
			"series": series, "total_matched": totalMatched, "total_visible": totalVisible, "filters_applied": filtersApplied,
		})
		return
	}
	if groupBy == "project" || groupBy == "inventor" || groupBy == "method" {
		series := groupByDimension(scoredList, groupBy, projectByID, inventorByID, methodByID)
		writeJSON(w, http.StatusOK, map[string]any{
			"series": series, "total_matched": totalMatched, "total_visible": totalVisible, "filters_applied": filtersApplied,
		})
		return
	}

	sort.SliceStable(scoredList, func(i, j int) bool {
		if len(scoredList[i].matchedIn) != len(scoredList[j].matchedIn) {
			return len(scoredList[i].matchedIn) > len(scoredList[j].matchedIn)
		}
		return scoredList[i].req.UpdatedAt > scoredList[j].req.UpdatedAt
	})
	if len(scoredList) > limit {
		scoredList = scoredList[:limit]
	}
	out := make([]searchRequestResult, 0, len(scoredList))
	for _, sc := range scoredList {
		out = append(out, searchRequestResult{
			ID: sc.req.ID, Title: sc.req.Title, CustomerNumber: sc.req.CustomerNumber,
			LabNumber: sc.req.LabNumber, Status: sc.req.Status, Result: sc.req.Result, Compliance: sc.req.Compliance,
			ProjectID: sc.req.ProjectID, InventorID: sc.req.InventorID, MethodID: sc.req.MethodID,
			CreatedAt: sc.req.CreatedAt, CompletedAt: sc.req.CompletedAt, MatchedIn: sc.matchedIn,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests": out, "total_matched": totalMatched, "total_visible": totalVisible, "filters_applied": filtersApplied,
	})
}

// groupByPeriod — распределение совпавших заявок по дате регистрации/завершения
// (та же bucket-логика, что группировка без query в клиентских тулах, только
// здесь — над результатом поиска, а не над всей БД).
func groupByPeriod(scoredList []scoredRequest, granularity, dateFrom, dateTo string) []searchGroupPoint {
	type bucket struct{ arrived, completed int }
	buckets := map[string]*bucket{}
	bump := func(dateStr, field string) {
		if !dateInRange(dateStr, dateFrom, dateTo) {
			return
		}
		key := periodBucketKey(dateStr, granularity)
		if key == "" {
			return
		}
		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
		}
		if field == "arrived" {
			b.arrived++
		} else {
			b.completed++
		}
	}
	for _, sc := range scoredList {
		bump(sc.req.CreatedAt, "arrived")
		bump(sc.req.CompletedAt, "completed")
	}
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]searchGroupPoint, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		out = append(out, searchGroupPoint{Period: k, Arrived: b.arrived, Completed: b.completed, Count: b.arrived + b.completed})
	}
	return out
}

// periodBucketKey — начало периода (день/неделя-с-понедельника/месяц) для даты.
func periodBucketKey(dateStr, granularity string) string {
	if len(dateStr) < 10 {
		return ""
	}
	d := dateStr[:10]
	if granularity == "day" {
		return d
	}
	if granularity == "month" {
		return d[:7]
	}
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return ""
	}
	isoDow := int(t.Weekday())
	if isoDow == 0 {
		isoDow = 7
	}
	return t.AddDate(0, 0, -(isoDow - 1)).Format("2006-01-02")
}

// groupByDimension — распределение совпавших заявок по проекту/испытателю/методу.
func groupByDimension(scoredList []scoredRequest, dimension string, projectByID map[int64]Project, inventorByID map[int64]Inventor, methodByID map[int64]string) []searchGroupPoint {
	counts := map[int64]int{}
	for _, sc := range scoredList {
		var id int64
		switch dimension {
		case "project":
			id = sc.req.ProjectID
		case "inventor":
			id = sc.req.InventorID
		case "method":
			id = sc.req.MethodID
		}
		counts[id]++
	}
	labelFor := func(id int64) string {
		if id <= 0 {
			return "(не указано)"
		}
		switch dimension {
		case "project":
			if p, ok := projectByID[id]; ok && p.Name != "" {
				return p.Name
			}
		case "inventor":
			if inv, ok := inventorByID[id]; ok && inv.Name != "" {
				return inv.Name
			}
		case "method":
			if name, ok := methodByID[id]; ok && name != "" {
				return name
			}
		}
		return fmt.Sprintf("#%d", id)
	}
	out := make([]searchGroupPoint, 0, len(counts))
	for id, count := range counts {
		out = append(out, searchGroupPoint{Label: labelFor(id), Count: count})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// matchingLabIDs — id лабораторий, чьё имя или код содержит query (регистронезависимо).
func (s *Server) matchingLabIDs(ctx context.Context, query string) (map[int64]bool, []string, error) {
	q := strings.ToLower(query)
	rows, err := s.pool.Query(ctx, `SELECT id, code, name FROM labs`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	names := make([]string, 0, 2)
	for rows.Next() {
		var id int64
		var code, name string
		if err := rows.Scan(&id, &code, &name); err != nil {
			return nil, nil, err
		}
		if strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(code), q) {
			out[id] = true
			names = append(names, name)
		}
	}
	return out, names, rows.Err()
}

// filterRequestsForSearch применяет те же фильтры lab/status/compliance/date_from/
// date_to, что уже есть у клиентов get_lims_requests — здесь на сервере, ДО поиска,
// чтобы поиск можно было сузить (например «прочность алюминия в лаборатории
// пожарных испытаний за март»). lab резолвится по имени/коду через labs.
func filterRequestsForSearch(requests []Request, params interface{ Get(string) string }) []Request {
	status := strings.ToLower(strings.TrimSpace(params.Get("status")))
	compliance := strings.ToLower(strings.TrimSpace(params.Get("compliance")))
	dateFrom := strings.TrimSpace(params.Get("date_from"))
	dateTo := strings.TrimSpace(params.Get("date_to"))

	out := requests[:0:0]
	for _, req := range requests {
		if status != "" && strings.ToLower(req.Status) != status {
			continue
		}
		if compliance != "" && strings.ToLower(req.Compliance) != compliance {
			continue
		}
		if (dateFrom != "" || dateTo != "") && !dateInRange(req.CreatedAt, dateFrom, dateTo) && !dateInRange(req.CompletedAt, dateFrom, dateTo) {
			continue
		}
		out = append(out, req)
	}
	return out
}

func dateInRange(dateStr, from, to string) bool {
	if len(dateStr) < 10 {
		return false
	}
	d := dateStr[:10]
	if from != "" && d < from {
		return false
	}
	if to != "" && d > to {
		return false
	}
	return true
}

// loadSearchLookups батчем догружает справочники, нужные buildSearchFields — по
// одному запросу на таблицу (не по одной заявке на запрос).
func (s *Server) loadSearchLookups(ctx context.Context, requests []Request) (
	map[int64]Project, map[int64]Inventor, map[int64]string, map[int64]Object,
	map[int64][]MeasurementResult, map[int64][]AggregatedResult, error,
) {
	fail := func(err error) (map[int64]Project, map[int64]Inventor, map[int64]string, map[int64]Object,
		map[int64][]MeasurementResult, map[int64][]AggregatedResult, error) {
		return nil, nil, nil, nil, nil, nil, err
	}

	ids := make([]int64, 0, len(requests))
	objectIDs := make(map[int64]bool, len(requests))
	for _, req := range requests {
		ids = append(ids, req.ID)
		if req.ObjectID > 0 {
			objectIDs[req.ObjectID] = true
		}
	}

	projectByID := map[int64]Project{}
	prows, err := s.pool.Query(ctx, `SELECT id, name FROM projects`)
	if err != nil {
		return fail(err)
	}
	for prows.Next() {
		var id int64
		var name string
		if err := prows.Scan(&id, &name); err != nil {
			prows.Close()
			return fail(err)
		}
		projectByID[id] = Project{ID: id, Name: name}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return fail(err)
	}

	inventorByID := map[int64]Inventor{}
	irows, err := s.pool.Query(ctx, `SELECT id, name FROM inventors`)
	if err != nil {
		return fail(err)
	}
	for irows.Next() {
		var id int64
		var name string
		if err := irows.Scan(&id, &name); err != nil {
			irows.Close()
			return fail(err)
		}
		inventorByID[id] = Inventor{ID: id, Name: name}
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return fail(err)
	}

	methodByID := map[int64]string{}
	metrows, err := s.pool.Query(ctx, `SELECT id, name FROM methods`)
	if err != nil {
		return fail(err)
	}
	for metrows.Next() {
		var id int64
		var name string
		if err := metrows.Scan(&id, &name); err != nil {
			metrows.Close()
			return fail(err)
		}
		methodByID[id] = name
	}
	metrows.Close()
	if err := metrows.Err(); err != nil {
		return fail(err)
	}

	objectByID := map[int64]Object{}
	if len(objectIDs) > 0 {
		oIDs := make([]int64, 0, len(objectIDs))
		for id := range objectIDs {
			oIDs = append(oIDs, id)
		}
		orows, err := s.pool.Query(ctx, `SELECT id, name, characteristics FROM objects WHERE id = ANY($1)`, oIDs)
		if err != nil {
			return fail(err)
		}
		for orows.Next() {
			var id int64
			var name string
			var charsRaw []byte
			if err := orows.Scan(&id, &name, &charsRaw); err != nil {
				orows.Close()
				return fail(err)
			}
			chars := map[string]any{}
			if len(charsRaw) > 0 {
				_ = json.Unmarshal(charsRaw, &chars)
			}
			objectByID[id] = Object{ID: id, Name: name, Characteristics: chars}
		}
		orows.Close()
		if err := orows.Err(); err != nil {
			return fail(err)
		}
	}

	measurementsByRequest := map[int64][]MeasurementResult{}
	mrows, err := s.pool.Query(ctx, `SELECT request_id, values FROM measurement_results WHERE request_id = ANY($1)`, ids)
	if err != nil {
		return fail(err)
	}
	for mrows.Next() {
		var requestID int64
		var valuesRaw []byte
		if err := mrows.Scan(&requestID, &valuesRaw); err != nil {
			mrows.Close()
			return fail(err)
		}
		values := map[string]any{}
		if len(valuesRaw) > 0 {
			_ = json.Unmarshal(valuesRaw, &values)
		}
		measurementsByRequest[requestID] = append(measurementsByRequest[requestID], MeasurementResult{RequestID: requestID, Values: values})
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return fail(err)
	}

	aggregatedByRequest := map[int64][]AggregatedResult{}
	arows, err := s.pool.Query(ctx, `SELECT request_id, result_data FROM aggregated_results WHERE request_id = ANY($1)`, ids)
	if err != nil {
		return fail(err)
	}
	for arows.Next() {
		var requestID int64
		var dataRaw []byte
		if err := arows.Scan(&requestID, &dataRaw); err != nil {
			arows.Close()
			return fail(err)
		}
		data := map[string]any{}
		if len(dataRaw) > 0 {
			_ = json.Unmarshal(dataRaw, &data)
		}
		aggregatedByRequest[requestID] = append(aggregatedByRequest[requestID], AggregatedResult{RequestID: requestID, ResultData: data})
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return fail(err)
	}

	return projectByID, inventorByID, methodByID, objectByID, measurementsByRequest, aggregatedByRequest, nil
}
