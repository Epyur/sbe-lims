package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func testCtx() *placeholderCtx {
	return &placeholderCtx{
		req: &Request{Title: "Заявка Х", EKN: "644713", OwnerEmail: "customer@tn.ru",
			NumberSeq: 5, NumberYear: 2026, CustomerNumber: "0-5/2026-ЛПИ-ГГ",
			ReportDate: "07.05.2025", SamplesInDate: "2025-04-29", ExpDate: "2025-05-06",
			AmbTemp: "22", AmbPres: "77", AmbMoist: "22"},
		objectName:      "LOGICPIR PROF",
		objectChars:     map[string]any{"batch_number": 9571.0, "sample_id": "S-1"},
		targetIndicator: "Г1",
		inventorName:    "В.С. Шоя",
		attrsByID: map[string]MethodAttribute{
			"mass_loss": {ID: "mass_loss", Name: "Потеря массы"},
			"grade":     {ID: "grade", Name: "Группа горючести", Level: "aggregated"},
			"photo1":    {ID: "photo1", Name: "Фото", DataType: "photo"},
		},
		series: []map[string]any{
			{"mass_loss": 10.0, "photo1": "http://x/1.jpg"},
			{"mass_loss": 20.0, "photo1": "http://x/2.jpg"},
			{"mass_loss": 30.0},
		},
		agg: map[string]any{"grade": "Г2"},
	}
}

func TestResolveSystemPlaceholder(t *testing.T) {
	ctx := testCtx()
	cases := map[string]string{
		"title":            "Заявка Х",
		"number":           "0-5/2026-ЛПИ-ГГ",
		"object_name":      "LOGICPIR PROF",
		"ekn":              "644713",
		"owner_email":      "customer@tn.ru",
		"target_indicator": "Г1",
		"batch_number":     "9571",
		"sample_id":        "S-1",
		"unknown_field":    "",
		// Системные атрибуты (2026-08-23) — испытатель/даты/условия среды, общие для
		// любого метода (см. email_ingest_test.go, TestSystemRequestFieldsCoversUniversalConcepts).
		"inventor":        "В.С. Шоя",
		"report_date":     "07.05.2025",
		"samples_in_date": "2025-04-29",
		"exp_date":        "2025-05-06",
		"amb_temp":        "22",
		"amb_pres":        "77",
		"amb_moist":       "22",
	}
	for id, want := range cases {
		got := resolveSystemPlaceholder(ctx, id)
		if got != want {
			t.Errorf("resolveSystemPlaceholder(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestResolvePlaceholderAggregatedAttribute(t *testing.T) {
	ctx := testCtx()
	got := resolvePlaceholder(ctx, InlineNode{Type: "placeholder", Source: "attribute", AttributeID: "grade"})
	if got != "Г2" {
		t.Errorf("aggregated attribute placeholder = %q, want %q", got, "Г2")
	}
}

func TestResolvePlaceholderExperimentAttributeAggregation(t *testing.T) {
	ctx := testCtx()
	cases := map[string]string{"avg": "20", "min": "10", "max": "30", "first": "10", "last": "30"}
	for agg, want := range cases {
		got := resolvePlaceholder(ctx, InlineNode{Type: "placeholder", Source: "attribute", AttributeID: "mass_loss", Agg: agg})
		if got != want {
			t.Errorf("agg=%s: got %q, want %q", agg, got, want)
		}
	}
}

func TestAggregateSeriesSkipsMissingKeys(t *testing.T) {
	series := []map[string]any{{"a": 1.0}, {}, {"a": 3.0}}
	if v := aggregateSeries(series, "a", "avg"); v != 2.0 {
		t.Errorf("avg with a missing key skipped = %v, want 2.0", v)
	}
}

func TestRenderInlineHTMLBoldItalicAndPlaceholder(t *testing.T) {
	ctx := testCtx()
	nodes := []InlineNode{
		{Type: "text", Text: "Материал: ", Bold: true},
		{Type: "placeholder", Source: "system", AttributeID: "object_name"},
		{Type: "text", Text: " (проверка)", Italic: true},
	}
	got := renderInlineHTML(ctx, nodes)
	want := "<b>Материал: </b>LOGICPIR PROF<i> (проверка)</i>"
	if got != want {
		t.Errorf("renderInlineHTML = %q, want %q", got, want)
	}
}

// Верхний/нижний индекс (2026-08-24, по прямому запросу пользователя — "во
// всех элементах... добавить возможность вставки верхних/нижних индексов, как
// это сделано в настройках атрибутов" — там юникод-символ на plain input, тут
// настоящее форматирование на contenteditable). Sup/Sub взаимоисключающие —
// проверяем оба варианта отдельно, а не одновременно на одном узле.
func TestRenderInlineHTMLSupSub(t *testing.T) {
	ctx := testCtx()
	sup := renderInlineHTML(ctx, []InlineNode{{Type: "text", Text: "CO2", Sup: true}})
	if sup != "<sup>CO2</sup>" {
		t.Errorf("sup = %q, want <sup>CO2</sup>", sup)
	}
	sub := renderInlineHTML(ctx, []InlineNode{{Type: "text", Text: "2", Sub: true}})
	if sub != "<sub>2</sub>" {
		t.Errorf("sub = %q, want <sub>2</sub>", sub)
	}
}

func TestRenderNodeDocxSupSub(t *testing.T) {
	ctx := testCtx()
	sup := renderNodeDocx(ctx, RichNode{Type: "paragraph", Children: []InlineNode{{Type: "text", Text: "x", Sup: true}}})
	if !strings.Contains(sup, `<w:vertAlign w:val="superscript"/>`) {
		t.Errorf("docx missing superscript vertAlign: %s", sup)
	}
	sub := renderNodeDocx(ctx, RichNode{Type: "paragraph", Children: []InlineNode{{Type: "text", Text: "x", Sub: true}}})
	if !strings.Contains(sub, `<w:vertAlign w:val="subscript"/>`) {
		t.Errorf("docx missing subscript vertAlign: %s", sub)
	}
}

// Выравнивание абзаца/заголовка (2026-08-24, по запросу пользователя — "в
// абзаце настройки выравнивания (по ширине, центр, право, лево)"). "" (слева)
// не должно выводить style/w:pPr вовсе — левый край без атрибута — дефолт
// HTML/Word, добавлять его явно было бы избыточно.
func TestRenderNodeAlign(t *testing.T) {
	ctx := testCtx()
	para := RichNode{Type: "paragraph", Align: "center", Children: []InlineNode{{Type: "text", Text: "x"}}}
	if got := renderNodeHTML(ctx, para); got != `<p style="text-align:center">x</p>` {
		t.Errorf("centered paragraph html = %q", got)
	}
	if got := renderNodeDocx(ctx, para); !strings.Contains(got, `<w:jc w:val="center"/>`) {
		t.Errorf("centered paragraph docx missing w:jc: %s", got)
	}

	justify := RichNode{Type: "heading", Level: 2, Align: "justify", Children: []InlineNode{{Type: "text", Text: "x"}}}
	if got := renderNodeHTML(ctx, justify); !strings.Contains(got, `style="text-align:justify"`) {
		t.Errorf("justify heading html = %q", got)
	}
	if got := renderNodeDocx(ctx, justify); !strings.Contains(got, `<w:jc w:val="both"/>`) {
		t.Errorf(`justify heading docx should use w:val="both" (OOXML), got: %s`, got)
	}

	left := RichNode{Type: "paragraph", Children: []InlineNode{{Type: "text", Text: "x"}}}
	if got := renderNodeHTML(ctx, left); got != "<p>x</p>" {
		t.Errorf("default-align paragraph should have no style attr: %q", got)
	}
	if got := renderNodeDocx(ctx, left); strings.Contains(got, "w:pPr") {
		t.Errorf("default-align paragraph should have no w:pPr: %s", got)
	}
}

// Статическая таблица (2026-08-24, визуальный конструктор) — ячейки вводятся
// вручную, ничего не подставляется из результатов эксперимента кроме
// плейсхолдеров внутри самой ячейки; не переиспользует renderTableHTML/Docx
// (те привязаны к сериям/TableColumn).
func TestRenderStaticTable(t *testing.T) {
	ctx := testCtx()
	rows := [][][]InlineNode{
		{{{Type: "text", Text: "A", Bold: true}}, {{Type: "text", Text: "B"}}},
		{{{Type: "placeholder", Source: "system", AttributeID: "object_name"}}, {{Type: "text", Text: "D"}}},
	}
	html := renderNodeHTML(ctx, RichNode{Type: "static_table", Rows: rows})
	want := "<table><tr><td><b>A</b></td><td>B</td></tr><tr><td>LOGICPIR PROF</td><td>D</td></tr></table>"
	if html != want {
		t.Errorf("static_table html = %q, want %q", html, want)
	}
	docx := renderNodeDocx(ctx, RichNode{Type: "static_table", Rows: rows})
	if !strings.Contains(docx, "<w:tblBorders>") {
		t.Errorf("static_table docx missing borders: %s", docx)
	}
	if !strings.Contains(docx, "LOGICPIR PROF") {
		t.Errorf("static_table docx missing resolved placeholder: %s", docx)
	}
}

func TestRenderNodeHTMLHeadingAndBulletList(t *testing.T) {
	ctx := testCtx()
	heading := renderNodeHTML(ctx, RichNode{Type: "heading", Level: 3, Children: []InlineNode{{Type: "text", Text: "Заголовок"}}})
	if heading != "<h3>Заголовок</h3>" {
		t.Errorf("heading = %q", heading)
	}
	list := renderNodeHTML(ctx, RichNode{Type: "bullet_list", Items: [][]InlineNode{
		{{Type: "text", Text: "Пункт 1"}},
		{{Type: "text", Text: "Пункт 2"}},
	}})
	if list != "<ul><li>Пункт 1</li><li>Пункт 2</li></ul>" {
		t.Errorf("bullet_list = %q", list)
	}
}

func TestRenderTableHTMLPhotoColumn(t *testing.T) {
	ctx := testCtx()
	got := renderNodeHTML(ctx, RichNode{Type: "table", Columns: []TableColumn{
		{AttributeID: "mass_loss"}, {AttributeID: "photo1"},
	}})
	if !strings.Contains(got, "<th>Потеря массы</th>") {
		t.Errorf("table missing column label: %s", got)
	}
	if !strings.Contains(got, `<img src="http://x/1.jpg"`) {
		t.Errorf("table missing photo <img>: %s", got)
	}
	if strings.Count(got, "<tr>") != 4 { // header + 3 series rows
		t.Errorf("expected 4 <tr> (header+3 series), got: %s", got)
	}
}

func TestRenderTableSeriesNoColumn(t *testing.T) {
	ctx := testCtx()
	columns := []TableColumn{{Kind: "series_no"}, {AttributeID: "mass_loss"}}
	html := renderNodeHTML(ctx, RichNode{Type: "table", Columns: columns})
	if !strings.Contains(html, "<th>Серия</th>") {
		t.Errorf("html missing default series_no header: %s", html)
	}
	if !strings.Contains(html, "<td>1</td>") || !strings.Contains(html, "<td>3</td>") {
		t.Errorf("html missing 1-based series numbers: %s", html)
	}
	docx := renderNodeDocx(ctx, RichNode{Type: "table", Columns: columns})
	if !strings.Contains(docx, "<w:t>Серия</w:t>") {
		t.Errorf("docx missing default series_no header: %s", docx)
	}
	if !strings.Contains(docx, "<w:t>1</w:t>") || !strings.Contains(docx, "<w:t>3</w:t>") {
		t.Errorf("docx missing 1-based series numbers: %s", docx)
	}

	custom := renderNodeHTML(ctx, RichNode{Type: "table", Columns: []TableColumn{{Kind: "series_no", Label: "№ п/п"}}})
	if !strings.Contains(custom, "<th>№ п/п</th>") {
		t.Errorf("html should respect explicit label override: %s", custom)
	}
}

// Центрирование таблиц результатов (2026-08-24, по прямому запросу пользователя
// — "таблица результатов... выравнивание по центру и границы"). Границы были
// уже и в HTML (td,th{border:...}), и в DOCX (<w:tblBorders>) — не хватало
// только выравнивания.
// Пакет OPC/OOXML .docx (2026-08-24) — реальный баг, найденный пользователем:
// "файлы ворд... не читаемые, ошибка открытия". Предыдущие тесты проверяли
// только СОДЕРЖИМОЕ word/document.xml подстроками — ни один не открывал
// результат как ZIP/OOXML-пакет, поэтому отсутствие обязательной части
// _rels/.rels (без неё Word не может найти точку входа — какой part главный
// документ — и отказывается открывать файл) не было замечено. Этот тест
// действительно распаковывает zip и парсит XML каждой части, а не ищет
// подстроки — именно такая проверка поймала бы баг раньше.
func TestProtocolDocxIsValidOOXMLPackage(t *testing.T) {
	req := &Request{Title: "Заявка Х", CustomerNumber: "0-5/2026-ЛПИ-ГГ"}
	p := &protocolData{Number: req.CustomerNumber, Title: "Заявка Х", Methods: []protocolMethod{{
		MethodID: 1, MethodName: "ГГ",
		Blocks: []DocumentBlock{{
			ID: "b1", ShowInProtocol: true,
			Content: []RichNode{
				{Type: "heading", Level: 2, Align: "center", Children: []InlineNode{{Type: "text", Text: "Заголовок"}}},
				{Type: "paragraph", Children: []InlineNode{{Type: "text", Text: "CO2", Sup: true}}},
				{Type: "table", Columns: []TableColumn{{Kind: "series_no"}}},
			},
		}},
		Ctx: &placeholderCtx{req: req, series: []map[string]any{{}}},
	}}}
	data, err := protocolDocx(p, "protocol")
	if err != nil {
		t.Fatalf("protocolDocx: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("результат protocolDocx — не валидный zip: %v", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		files[f.Name] = b
	}

	for _, required := range []string{"_rels/.rels", "[Content_Types].xml", "word/document.xml"} {
		content, ok := files[required]
		if !ok {
			t.Fatalf("пакет .docx не содержит обязательную часть %q (найдены: %v)", required, keysOf(files))
		}
		if err := xml.Unmarshal(content, new(any)); err != nil {
			t.Errorf("%s — невалидный XML: %v\n%s", required, err, content)
		}
	}

	// _rels/.rels обязана ссылаться на word/document.xml как на главный документ
	// (Type=officeDocument) — без этой связи Word не находит точку входа.
	if !strings.Contains(string(files["_rels/.rels"]), `Target="word/document.xml"`) {
		t.Errorf("_rels/.rels не ссылается на word/document.xml: %s", files["_rels/.rels"])
	}
	if !strings.Contains(string(files["_rels/.rels"]), "relationships/officeDocument") {
		t.Errorf("_rels/.rels: связь на document.xml должна быть типа officeDocument: %s", files["_rels/.rels"])
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestProtocolHTMLTableCentered(t *testing.T) {
	req := &Request{Title: "Заявка Х", CustomerNumber: "0-5/2026-ЛПИ-ГГ"}
	p := &protocolData{Number: req.CustomerNumber, Methods: []protocolMethod{{
		MethodID: 1, MethodName: "ГГ",
		Blocks: []DocumentBlock{{
			ID: "b1", ShowInProtocol: true,
			Content: []RichNode{{Type: "table", Columns: []TableColumn{{Kind: "series_no"}}}},
		}},
		Ctx: &placeholderCtx{req: req, series: []map[string]any{{}}},
	}}}
	got := protocolHTML(p, "protocol")
	if !strings.Contains(got, "text-align:center") {
		t.Errorf("style block missing text-align:center: %s", got)
	}
	if !strings.Contains(got, "border:1px solid") {
		t.Errorf("style block missing cell border (regression): %s", got)
	}
}

func TestRenderTableDocxCellsCentered(t *testing.T) {
	ctx := testCtx()
	docx := renderTableDocx(ctx, []TableColumn{{Kind: "series_no"}, {AttributeID: "mass_loss"}})
	if strings.Count(docx, `<w:jc w:val="center"/>`) < 4 { // header x2 + at least one data row x2
		t.Errorf("expected centered paragraphs in header and data cells: %s", docx)
	}
	if !strings.Contains(docx, "<w:tblBorders>") {
		t.Errorf("table borders missing (regression): %s", docx)
	}
}

func TestRenderNodeDocxHeadingForcesBoldAndSize(t *testing.T) {
	ctx := testCtx()
	got := renderNodeDocx(ctx, RichNode{Type: "heading", Level: 2, Children: []InlineNode{{Type: "text", Text: "Заголовок"}}})
	if !strings.Contains(got, "<w:b/>") || !strings.Contains(got, `<w:sz w:val="32"/>`) {
		t.Errorf("heading docx missing bold/size: %s", got)
	}
}

func TestRenderNodeDocxParagraphRespectsPerRunFormatting(t *testing.T) {
	ctx := testCtx()
	got := renderNodeDocx(ctx, RichNode{Type: "paragraph", Children: []InlineNode{
		{Type: "text", Text: "обычный "},
		{Type: "text", Text: "жирный", Bold: true},
	}})
	runs := strings.Split(got, "</w:r>")
	if len(runs) < 2 {
		t.Fatalf("expected at least 2 runs: %s", got)
	}
	if strings.Contains(runs[0], "<w:b/>") {
		t.Errorf("first (plain) run should not be bold: %s", runs[0])
	}
	if !strings.Contains(runs[1], "<w:b/>") {
		t.Errorf("second (bold) run missing <w:b/>: %s", runs[1])
	}
}

func TestFilterBlocksForKind(t *testing.T) {
	blocks := []DocumentBlock{
		{ID: "a", ShowInUI: true, ShowInExcerpt: false, ShowInProtocol: true},
		{ID: "b", ShowInUI: false, ShowInExcerpt: true, ShowInProtocol: true},
	}
	ui := filterBlocksForKind(blocks, "ui")
	if len(ui) != 1 || ui[0].ID != "a" {
		t.Errorf("kind=ui: got %+v, want only block a", ui)
	}
	excerpt := filterBlocksForKind(blocks, "excerpt")
	if len(excerpt) != 1 || excerpt[0].ID != "b" {
		t.Errorf("kind=excerpt: got %+v, want only block b", excerpt)
	}
	protocol := filterBlocksForKind(blocks, "protocol")
	if len(protocol) != 2 {
		t.Errorf("kind=protocol: got %d blocks, want 2", len(protocol))
	}
}

// parseMethodPresentation — легаси-фолбэк на ДВА шага назад (v1 плоские поля
// 2026-08-21, v2 секции 2026-08-22) в блоки (v3, 2026-08-23) — без потери
// уже настроенных методов.

func TestParseMethodPresentationV3BlocksAsIs(t *testing.T) {
	raw := []byte(`{"blocks":[{"id":"b1","title":"Блок","content":[{"type":"paragraph","children":[{"type":"text","text":"привет"}]}],"show_in_ui":true}]}`)
	got := parseMethodPresentation(raw)
	if len(got.Blocks) != 1 || got.Blocks[0].Title != "Блок" {
		t.Fatalf("got %+v, want one block titled 'Блок'", got.Blocks)
	}
}

func TestParseMethodPresentationV1LegacyFlatFields(t *testing.T) {
	raw := []byte(`{"fields":[{"attribute_id":"mass_before","label":"Масса до","show_in_ui":true,"show_in_protocol":true}]}`)
	got := parseMethodPresentation(raw)
	if len(got.Blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(got.Blocks))
	}
	blk := got.Blocks[0]
	// колонки: series_no (сохранение старого implicit-"Серия" поведения) + mass_before.
	if len(blk.Content) != 1 || blk.Content[0].Type != "table" || len(blk.Content[0].Columns) != 2 {
		t.Fatalf("got content %+v, want single table node with 2 columns", blk.Content)
	}
	if blk.Content[0].Columns[0].Kind != "series_no" {
		t.Errorf("column[0].kind = %q, want series_no", blk.Content[0].Columns[0].Kind)
	}
	if blk.Content[0].Columns[1].AttributeID != "mass_before" {
		t.Errorf("column[1] attribute_id = %q, want mass_before", blk.Content[0].Columns[1].AttributeID)
	}
	if !blk.ShowInUI || !blk.ShowInProtocol {
		t.Errorf("block visibility = %+v, want ShowInUI/ShowInProtocol true", blk)
	}
}

func TestParseMethodPresentationV2LegacySections(t *testing.T) {
	raw := []byte(`{"sections":[{"id":"s1","title":"Температура","fields":[
		{"attribute_id":"temp_max","role":"table","show_in_ui":true,"show_in_protocol":true},
		{"attribute_id":"grade","label":"Группа","role":"summary","show_in_excerpt":true}
	]}]}`)
	got := parseMethodPresentation(raw)
	if len(got.Blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(got.Blocks))
	}
	blk := got.Blocks[0]
	if blk.Title != "Температура" {
		t.Errorf("title = %q, want Температура", blk.Title)
	}
	if len(blk.Content) != 2 {
		t.Fatalf("got content %+v, want table + bullet_list", blk.Content)
	}
	// колонки: series_no (сохранение старого implicit-"Серия" поведения) + temp_max.
	if blk.Content[0].Type != "table" || blk.Content[0].Columns[0].Kind != "series_no" || blk.Content[0].Columns[1].AttributeID != "temp_max" {
		t.Errorf("first node should be table with series_no + temp_max columns: %+v", blk.Content[0])
	}
	if blk.Content[1].Type != "bullet_list" || len(blk.Content[1].Items) != 1 {
		t.Errorf("second node should be bullet_list with 1 item: %+v", blk.Content[1])
	}
	// видимость блока — OR по полям: show_in_ui у temp_max, show_in_excerpt у grade
	if !blk.ShowInUI || !blk.ShowInExcerpt || !blk.ShowInProtocol {
		t.Errorf("block visibility should OR across fields: %+v", blk)
	}
}

func TestParseMethodPresentationEmpty(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("{}"), []byte("null")} {
		got := parseMethodPresentation(raw)
		if got.Blocks == nil || len(got.Blocks) != 0 {
			t.Errorf("parseMethodPresentation(%q) = %+v, want empty non-nil slice", raw, got.Blocks)
		}
	}
}
