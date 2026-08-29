package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"image"
	_ "image/jpeg" // регистрирует декодер для image.DecodeConfig (пропорции фото в DOCX)
	_ "image/png"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- Протокол / выписка / короткий вид: HTML + DOCX по блокам форматированного
// текста с плейсхолдерами (2026-08-23 — блочный редактор заменил секции полей:
// пользователь явно отверг модель "показать/скрыть атрибут" как не подходящую
// для документа с реквизитами/описаниями/юридическим футером, см. AGENTS.md) ----

type protocolData struct {
	RequestID   int64
	Number      string
	Title       string
	Owner       string
	Object      string
	EKN         string
	Priority    string
	TestPurpose string
	Status      string
	CreatedAt   string
	Methods     []protocolMethod
}

type protocolMethod struct {
	MethodID     int64
	MethodName   string
	Blocks       []DocumentBlock
	ChartConfigs []map[string]any
	Ctx          *placeholderCtx
}

// placeholderCtx — всё, что нужно для резолва плейсхолдеров ОДНОГО метода в
// рамках одного рендера: системные данные заявки/объекта + серии/статистика/
// агрегаты этого метода.
type placeholderCtx struct {
	req             *Request
	objectName      string
	objectChars     map[string]any
	targetIndicator string
	inventorName    string // req.InventorID резолвится в имя один раз в buildProtocol
	methodName      string // 2026-08-27: плейсхолдер method_name — админ сам решает,
	// показывать ли название метода (жёсткого "Метод: ..." заголовка больше нет,
	// см. AGENTS.md "Метод: ... — теперь просто плейсхолдер").
	attrsByID map[string]MethodAttribute
	series    []map[string]any
	stats     map[string]any
	agg       map[string]any
	// photoBefore/photoAfter — top-level measurement_results.photo_before/photo_after
	// каждой серии (2026-08-28), параллельно series (тот же индекс = та же серия) —
	// см. loadSeriesPhotos. Не часть values, чтобы не течь в DSL-формулы/агрегацию.
	photoBefore []string
	photoAfter  []string
	// photoRegistry — только при рендере DOCX (2026-08-24, см. docxPhotoRegistry): при
	// рендере HTML остаётся nil, фото-атрибуты рендерятся прямой ссылкой (renderTableHTML/
	// renderInlineHTML), сеть не нужна. Общий на ВЕСЬ протокол объект (передаётся всем
	// методам одного документа), а не по одному на метод — иначе relationship id/имена
	// media-файлов пересеклись бы между методами.
	photoRegistry *docxPhotoRegistry
	// headingNumbers/currentBlockID (2026-08-29, WP6, путь б) — "№ п/п"-плейсхолдер
	// внутри вручную набранного текста блока. headingNumbers считается ОДИН раз на
	// метод (computeHeadingNumbers, по уже отфильтрованному для текущего kind
	// списку блоков) — блокID → его порядковый номер среди блоков, СОДЕРЖАЩИХ этот
	// плейсхолдер где-то в себе (не среди всех видимых блоков — админ сам решает,
	// для каких блоков нужна нумерация, просто вставляя/не вставляя плейсхолдер).
	// currentBlockID — мутируется перед рендером содержимого каждого блока (см.
	// protocolHTML/protocolDocx), чтобы resolvePlaceholder знал, какой блок сейчас
	// рендерится (ctx — указатель, общий на весь рендер метода).
	headingNumbers map[string]int
	currentBlockID string
}

// showInKind — какой из трёх флагов блока проверять для запрошенного вида
// вывода. "protocol" (полный) — поведение по умолчанию для неизвестного
// значения kind, чтобы старые клиенты без ?template= получали прежнее поведение.
func showInKind(showInUI, showInExcerpt, showInProtocol bool, kind string) bool {
	switch kind {
	case "ui":
		return showInUI
	case "excerpt":
		return showInExcerpt
	default:
		return showInProtocol
	}
}

func filterBlocksForKind(blocks []DocumentBlock, kind string) []DocumentBlock {
	out := make([]DocumentBlock, 0, len(blocks))
	for _, b := range blocks {
		if showInKind(b.ShowInUI, b.ShowInExcerpt, b.ShowInProtocol, kind) {
			out = append(out, b)
		}
	}
	return out
}

// computeHeadingNumbers — WP6 (путь б, 2026-08-29): "№ п/п"-плейсхолдер внутри
// вручную набранного текста блока (см. placeholderCtx.headingNumbers). Считает
// по УЖЕ отфильтрованному для текущего вида вывода (kind) списку блоков —
// нумерует только блоки, СОДЕРЖАЩИЕ этот плейсхолдер где-то в своём содержимом
// (не все видимые блоки подряд) — блок без плейсхолдера просто пропускается,
// не сбивая счёт остальных.
func computeHeadingNumbers(blocks []DocumentBlock) map[string]int {
	out := make(map[string]int, len(blocks))
	n := 0
	for _, b := range blocks {
		if blockHasHeadingNumberPlaceholder(b) {
			n++
			out[b.ID] = n
		}
	}
	return out
}

func blockHasHeadingNumberPlaceholder(b DocumentBlock) bool {
	for _, node := range b.Content {
		if richNodeHasHeadingNumberPlaceholder(node) {
			return true
		}
	}
	return false
}

// richNodeHasHeadingNumberPlaceholder ищет source="heading_number" в любом из
// мест, где вообще может быть InlineNode: paragraph/heading (Children), bullet_list
// (Items), static_table (Rows) — table ("table", данные серий) плейсхолдер
// вставить нельзя (ячейки не редактируются текстом, см. renderTableNodeEditor).
func richNodeHasHeadingNumberPlaceholder(node RichNode) bool {
	for _, c := range node.Children {
		if c.Type == "placeholder" && c.Source == "heading_number" {
			return true
		}
	}
	for _, item := range node.Items {
		for _, c := range item {
			if c.Type == "placeholder" && c.Source == "heading_number" {
				return true
			}
		}
	}
	for _, row := range node.Rows {
		for _, cell := range row {
			for _, c := range cell {
				if c.Type == "placeholder" && c.Source == "heading_number" {
					return true
				}
			}
		}
	}
	return false
}

// columnLabel — явная подпись, иначе имя атрибута, иначе сам id.
func columnLabel(key, explicitLabel string, attrsByID map[string]MethodAttribute) string {
	if explicitLabel != "" {
		return explicitLabel
	}
	if a, ok := attrsByID[key]; ok && a.Name != "" {
		return a.Name
	}
	return key
}

// tableColumnHeader — подпись колонки таблицы; "series_no" — обычная колонка
// (2026-08-23, раньше жёстко prepend-илась как "Серия" без права пользователя
// её убрать/переместить/переименовать, см. TableColumn.Kind в results.go).
func tableColumnHeader(c TableColumn, attrsByID map[string]MethodAttribute) string {
	if c.Kind == "series_no" {
		if c.Label != "" {
			return c.Label
		}
		return "Серия"
	}
	// "sequential" (2026-08-29, WP5) — та же нумерация i+1, что у "series_no"
	// (см. renderTableHTML/renderTableDocx ниже), только другая дефолтная подпись —
	// для таблиц, где "Серия" семантически не подходит (напр. таблица оборудования,
	// прямой запрос MVP-документа "заменять № серии на № п/п").
	if c.Kind == "sequential" {
		if c.Label != "" {
			return c.Label
		}
		return "№ п/п"
	}
	// photo_before/photo_after (2026-08-28) — top-level измерение measurement_results,
	// не атрибут метода, поэтому columnLabel(AttributeID, ...) тут не резолвит: свои
	// дефолтные подписи, как у series_no выше.
	if c.Kind == "photo_before" {
		if c.Label != "" {
			return c.Label
		}
		return "Фото до испытания"
	}
	if c.Kind == "photo_after" {
		if c.Label != "" {
			return c.Label
		}
		return "Фото после испытания"
	}
	return columnLabel(c.AttributeID, c.Label, attrsByID)
}

// methodAttributesByID парсит cfg.InputParams (map[string]any) в MethodAttribute,
// индексированные по id.
func methodAttributesByID(cfg *MethodConfig) map[string]MethodAttribute {
	var attrs []MethodAttribute
	if b, err := json.Marshal(cfg.InputParams); err == nil {
		_ = json.Unmarshal(b, &attrs)
	}
	out := make(map[string]MethodAttribute, len(attrs))
	for _, a := range attrs {
		out[a.ID] = a
	}
	return out
}

func findChartConfigByID(charts []map[string]any, id string) map[string]any {
	for _, c := range charts {
		if cid, _ := c["id"].(string); cid == id {
			return c
		}
	}
	return nil
}

// ---- Резолв плейсхолдеров ----

// resolveSystemPlaceholder — заявка/объект: партия/материал/ЕКН/заказчик и
// т.п. (см. AGENTS.md — каталог "system.*", резолвится из уже загруженных в
// buildProtocol данных, без дополнительных запросов на каждый плейсхолдер).
func resolveSystemPlaceholder(ctx *placeholderCtx, id string) string {
	switch id {
	case "title":
		return ctx.req.Title
	case "method_name":
		return ctx.methodName
	case "number":
		// Полный номер (2026-08-24, по прямому запросу пользователя — "везде...
		// всегда должен фигурировать полный номер заявки (с кодом проекта,
		// лаборатории, метода)"). CustomerNumber ({projectCode}-{NNN}/{yyyy}-
		// {labCode}-{methodCode}) — единственная форма с ВСЕМИ тремя кодами;
		// LabNumber ({NNN}/{yyyy}-{methodCode}) не содержит код проекта.
		// customer_number/lab_number остаются отдельными плейсхолдерами без
		// изменений — это дефолт "номер заявки", не единственный источник.
		return ctx.req.CustomerNumber
	case "object_name":
		return ctx.objectName
	case "ekn":
		return ctx.req.EKN
	case "owner_email":
		return ctx.req.OwnerEmail
	case "priority":
		return ctx.req.Priority
	case "test_purpose":
		return ctx.req.TestPurpose
	case "status":
		return ctx.req.Status
	case "created_at":
		return ctx.req.CreatedAt
	case "customer_number":
		return ctx.req.CustomerNumber
	case "lab_number":
		return ctx.req.LabNumber
	case "target_indicator":
		return ctx.targetIndicator
	case "batch_number":
		return fmtVal(ctx.objectChars["batch_number"])
	case "sample_id":
		return fmtVal(ctx.objectChars["sample_id"])
	case "thickness":
		return fmtVal(ctx.objectChars["thickness"])
	// Системные атрибуты (2026-08-23) — общие для ЛЮБОГО метода (испытатель/даты/
	// условия окружающей среды), заполняются автоматически из письма-результата
	// (email_ingest.go), НЕ настраиваются как MethodAttribute per-method (см.
	// sbe-lims/AGENTS.md, "Системные атрибуты").
	case "inventor":
		return ctx.inventorName
	// report_date/samples_in_date/exp_date/amb_temp/amb_pres/amb_moist (2026-08-28,
	// WP3b) — теперь хранятся per-series в values (испытания одной заявки могут
	// идти в разные дни с разными условиями среды), а не одним значением на
	// requests.*. Одиночный плейсхолдер вне таблицы результатов обязан свернуться
	// до ОДНОГО значения — берём ближайшую с конца серию, где поле реально
	// заполнено (см. seriesSystemField), не обязательно последнюю: у последней
	// серии поле могло быть скрыто формой как "тот же день, ничего не изменилось"
	// (см. sbe-lims-mobile). ctx.req.* — фоллбэк для домиграционных данных/заявок
	// без единой серии.
	case "report_date":
		return seriesSystemField(ctx.series, "report_date", ctx.req.ReportDate)
	case "samples_in_date":
		return seriesSystemField(ctx.series, "samples_in_date", ctx.req.SamplesInDate)
	case "exp_date":
		return seriesSystemField(ctx.series, "exp_date", ctx.req.ExpDate)
	case "amb_temp":
		return seriesSystemField(ctx.series, "amb_temp", ctx.req.AmbTemp)
	case "amb_pres":
		return seriesSystemField(ctx.series, "amb_pres", ctx.req.AmbPres)
	case "amb_moist":
		return seriesSystemField(ctx.series, "amb_moist", ctx.req.AmbMoist)
	}
	return ""
}

// seriesSystemField — значение системного поля (2026-08-28, WP3b) из ближайшей
// К КОНЦУ серии, где оно реально заполнено (сериям позже — приоритет), фоллбэк —
// requests-колонка (домиграционные заявки/заявки без единой серии, см. план
// docs/superpowers/plans/2026-08-28-sbe-lims-system-fields-per-series-plan.md).
func seriesSystemField(series []map[string]any, key, fallback string) string {
	for i := len(series) - 1; i >= 0; i-- {
		if v, ok := series[i][key]; ok {
			if s := fmtVal(v); s != "" {
				return s
			}
		}
	}
	return fallback
}

// aggregateSeries — ОДНО значение experiment-атрибута из серий метода, вне
// таблицы (см. InlineNode.Agg) — таблица показывает все серии как есть,
// одиночный плейсхолдер обязан свернуться до одного числа (прямое требование
// пользователя, 2026-08-23).
func aggregateSeries(series []map[string]any, key, agg string) any {
	var nums []float64
	var first, last any
	haveFirst := false
	for _, sv := range series {
		v, ok := sv[key]
		if !ok {
			continue
		}
		if !haveFirst {
			first = v
			haveFirst = true
		}
		last = v
		if f, ok := toFloatOK(v); ok {
			nums = append(nums, f)
		}
	}
	switch agg {
	case "first":
		return first
	case "last":
		return last
	case "min":
		if len(nums) == 0 {
			return nil
		}
		m := nums[0]
		for _, n := range nums {
			if n < m {
				m = n
			}
		}
		return m
	case "max":
		if len(nums) == 0 {
			return nil
		}
		m := nums[0]
		for _, n := range nums {
			if n > m {
				m = n
			}
		}
		return m
	default: // "avg"
		if len(nums) == 0 {
			return nil
		}
		var sum float64
		for _, n := range nums {
			sum += n
		}
		return sum / float64(len(nums))
	}
}

func toFloatOK(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// resolvePlaceholder — значение одного плейсхолдера ВНЕ таблицы (внутри
// таблицы колонки резолвятся построчно по сериям, см. renderTableHTML/Docx).
func resolvePlaceholder(ctx *placeholderCtx, n InlineNode) string {
	if n.Source == "heading_number" {
		if num, ok := ctx.headingNumbers[ctx.currentBlockID]; ok {
			return strconv.Itoa(num)
		}
		return ""
	}
	if n.Source == "system" {
		return resolveSystemPlaceholder(ctx, n.AttributeID)
	}
	attr := ctx.attrsByID[n.AttributeID]
	if attr.Level == "aggregated" {
		if v, ok := ctx.agg[n.AttributeID]; ok {
			return fmtVal(v)
		}
		if v, ok := ctx.stats[n.AttributeID]; ok {
			return fmtVal(v)
		}
		return ""
	}
	return fmtVal(aggregateSeries(ctx.series, n.AttributeID, n.Agg))
}

// seriesPhotoAt — photo_before/photo_after серии с индексом i (2026-08-28) — общий
// bounds-checked доступ для renderTableHTML/renderTableDocx; kind — "photo_before" |
// "photo_after". Индекс может выйти за границы, если photoBefore/photoAfter не
// удалось загрузить (loadSeriesPhotos best-effort, ошибка проглатывается в
// buildProtocol) — тогда просто пустая ячейка, как для незаполненного фото.
func seriesPhotoAt(ctx *placeholderCtx, kind string, i int) string {
	var arr []string
	if kind == "photo_before" {
		arr = ctx.photoBefore
	} else {
		arr = ctx.photoAfter
	}
	if i < 0 || i >= len(arr) {
		return ""
	}
	return arr[i]
}

// formatEventLog — event_log-атрибут (2026-08-28, WP3c ч.2: таймер/лог
// наблюдений) в читаемый текст "150 сек - label1; 155 сек - label2" (формат
// зафиксирован по прямому примеру пользователя 2026-08-29 — время ПЕРЕД
// названием события, не после) — без этого fmtVal() дал бы нечитаемый дамп
// Go-структуры (та же причина, что была у "[object Object]" для curve-массивов
// на десктопе до отдельного фикса). Значение — []any записей
// {"label":string,"seconds":number}, как пишут клиентские кнопки лога (см.
// renderTimerWidget); не JSON-массив/не запись без "label" — пропускается, не
// паникует.
func formatEventLog(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		label := fmtVal(m["label"])
		if label == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s сек - %s", fmtVal(m["seconds"]), label))
	}
	return strings.Join(parts, "; ")
}

// ---- HTML ----

func renderInlineHTML(ctx *placeholderCtx, nodes []InlineNode) string {
	var b strings.Builder
	for _, n := range nodes {
		// Плейсхолдер атрибута data_type="photo" вне таблицы (2026-08-24) — та же
		// логика, что уже была в renderTableHTML для ячеек: значение — URL фото,
		// показываем как <img>, а не как убегающий escaped-текст ссылки (иначе
		// вместо фото в отчёте печатается голый URL — путали пользователя).
		if n.Type == "placeholder" && n.Source == "attribute" && ctx.attrsByID[n.AttributeID].DataType == "photo" {
			if url := resolvePlaceholder(ctx, n); url != "" {
				fmt.Fprintf(&b, `<img src="%s" style="max-width:200px;max-height:200px">`, html.EscapeString(url))
			}
			continue
		}
		// event_log (2026-08-28, WP3c ч.2) — та же логика, что у photo выше: сырое
		// значение нужно ДО того, как resolvePlaceholder/fmtVal превратит его в
		// нечитаемый дамп — берём его через aggregateSeries напрямую.
		if n.Type == "placeholder" && n.Source == "attribute" && ctx.attrsByID[n.AttributeID].DataType == "event_log" {
			if s := formatEventLog(aggregateSeries(ctx.series, n.AttributeID, n.Agg)); s != "" {
				b.WriteString(html.EscapeString(s))
			}
			continue
		}
		text := n.Text
		if n.Type == "placeholder" {
			text = resolvePlaceholder(ctx, n)
		}
		escaped := html.EscapeString(text)
		if n.Bold {
			escaped = "<b>" + escaped + "</b>"
		}
		if n.Italic {
			escaped = "<i>" + escaped + "</i>"
		}
		if n.Sup {
			escaped = "<sup>" + escaped + "</sup>"
		} else if n.Sub {
			escaped = "<sub>" + escaped + "</sub>"
		}
		b.WriteString(escaped)
	}
	return b.String()
}

func renderTableHTML(ctx *placeholderCtx, columns []TableColumn) string {
	var b strings.Builder
	b.WriteString("<table><tr>")
	for _, c := range columns {
		fmt.Fprintf(&b, "<th>%s</th>", html.EscapeString(tableColumnHeader(c, ctx.attrsByID)))
	}
	b.WriteString("</tr>")
	for i, sv := range ctx.series {
		b.WriteString("<tr>")
		for _, c := range columns {
			if c.Kind == "series_no" || c.Kind == "sequential" {
				fmt.Fprintf(&b, "<td>%d</td>", i+1)
				continue
			}
			if c.Kind == "photo_before" || c.Kind == "photo_after" {
				url := seriesPhotoAt(ctx, c.Kind, i)
				if url != "" {
					fmt.Fprintf(&b, "<td><img src=\"%s\" style=\"max-width:160px;max-height:160px\"></td>", html.EscapeString(url))
				} else {
					b.WriteString("<td></td>")
				}
				continue
			}
			attr := ctx.attrsByID[c.AttributeID]
			val := sv[c.AttributeID]
			if attr.DataType == "photo" {
				if url, ok := val.(string); ok && url != "" {
					fmt.Fprintf(&b, "<td><img src=\"%s\" style=\"max-width:160px;max-height:160px\"></td>", html.EscapeString(url))
				} else {
					b.WriteString("<td></td>")
				}
				continue
			}
			// event_log (2026-08-28, WP3c ч.2) — та же логика, что photo выше:
			// сырое значение строки таблицы уже под рукой (val), fmtVal() дал бы
			// нечитаемый дамп.
			if attr.DataType == "event_log" {
				fmt.Fprintf(&b, "<td>%s</td>", html.EscapeString(formatEventLog(val)))
				continue
			}
			fmt.Fprintf(&b, "<td>%s</td>", html.EscapeString(fmtVal(val)))
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
	return b.String()
}

// htmlAlignAttr — style="text-align:X" для paragraph/heading (2026-08-24);
// "" (слева, браузерный дефолт) не выводит атрибут вовсе.
func htmlAlignAttr(align string) string {
	if align == "" {
		return ""
	}
	return fmt.Sprintf(` style="text-align:%s"`, align)
}

// renderStaticTableHTML — таблица, введённая пользователем вручную (2026-08-24,
// визуальный конструктор) — в отличие от renderTableHTML (данные серий,
// авто-заполняемые ячейки), ячейки — готовый rich-text, ничего не резолвится
// из результатов эксперимента, кроме плейсхолдеров внутри самой ячейки.
func renderStaticTableHTML(ctx *placeholderCtx, rows [][][]InlineNode) string {
	var b strings.Builder
	b.WriteString("<table>")
	for _, row := range rows {
		b.WriteString("<tr>")
		for _, cell := range row {
			fmt.Fprintf(&b, "<td>%s</td>", renderInlineHTML(ctx, cell))
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
	return b.String()
}

func renderNodeHTML(ctx *placeholderCtx, n RichNode) string {
	switch n.Type {
	case "heading":
		lvl := n.Level
		if lvl < 2 || lvl > 4 {
			lvl = 3
		}
		return fmt.Sprintf("<h%d%s>%s</h%d>", lvl, htmlAlignAttr(n.Align), renderInlineHTML(ctx, n.Children), lvl)
	case "bullet_list":
		var b strings.Builder
		b.WriteString("<ul>")
		for _, item := range n.Items {
			fmt.Fprintf(&b, "<li>%s</li>", renderInlineHTML(ctx, item))
		}
		b.WriteString("</ul>")
		return b.String()
	case "table":
		return renderTableHTML(ctx, n.Columns)
	case "static_table":
		return renderStaticTableHTML(ctx, n.Rows)
	default: // "paragraph"
		return fmt.Sprintf("<p%s>%s</p>", htmlAlignAttr(n.Align), renderInlineHTML(ctx, n.Children))
	}
}

func renderBlockChartHTML(b *strings.Builder, m protocolMethod) func(chartID string) {
	return func(chartID string) {
		if chartID == "" {
			return
		}
		chartCfg := findChartConfigByID(m.ChartConfigs, chartID)
		if chartCfg == nil {
			return
		}
		png, err := renderChartConfigPNG(chartCfg, m.Ctx.series)
		if err != nil || png == nil {
			return
		}
		title, _ := chartCfg["title"].(string)
		if title == "" {
			title, _ = chartCfg["id"].(string)
		}
		if title != "" {
			fmt.Fprintf(b, "<h4>%s</h4>", html.EscapeString(title))
		}
		fmt.Fprintf(b, "<img src=\"data:image/png;base64,%s\" alt=\"%s\" style=\"max-width:100%%\">", png2base64(png), html.EscapeString(title))
	}
}

func png2base64(png []byte) string { return toBase64(png) }

func protocolHTML(p *protocolData, kind string) string {
	var b strings.Builder
	b.WriteString("<html><head><meta charset='utf-8'><title>" + docTitle(kind) + "</title>")
	b.WriteString("<style>body{font-family:Segoe UI,Arial,sans-serif;font-size:14px;margin:24px}")
	b.WriteString("h1{font-size:20px}h2{font-size:16px;margin-top:16px}h3{font-size:14px;margin-top:12px}h4{font-size:13px;margin:8px 0 4px}")
	b.WriteString("table{border-collapse:collapse;margin:8px 0}")
	b.WriteString("td,th{border:1px solid #999;padding:4px 8px;font-size:13px;text-align:center}th{background:#f0f0f0}</style></head><body>")
	// Жёсткий заголовок (номер/наименование/объект/ЕКН/заказчик/статус/дата +
	// "Метод: ...") убран (2026-08-27, прямое требование пользователя: "в
	// протоколы/выписки/UI не должно ничего попадать, что не может быть
	// настроено пользователем в конфигураторе"). Всё это уже доступно как
	// обычные плейсхолдеры ({number}/{title}/{object_name}/{ekn}/{owner_email}/
	// {status}/{created_at}/{method_name}) — админ сам решает, какой блок с
	// каким текстом добавить в presentation.blocks, без миграции старых
	// методов (см. AGENTS.md).
	for _, m := range p.Methods {
		renderChart := renderBlockChartHTML(&b, m)
		for _, blk := range m.Blocks {
			m.Ctx.currentBlockID = blk.ID
			for _, node := range blk.Content {
				b.WriteString(renderNodeHTML(m.Ctx, node))
			}
			renderChart(blk.ChartID)
		}
	}
	b.WriteString("</body></html>")
	return b.String()
}

func docTitle(kind string) string {
	switch kind {
	case "ui":
		return "Краткий вид"
	case "excerpt":
		return "Выписка из протокола"
	default:
		return "Протокол испытаний"
	}
}

func fmtVal(v any) string {
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', 2, 64)
	}
	return fmt.Sprintf("%v", v)
}

// ---- DOCX (zip + document.xml) ----

// headingSizeHalfPoints — размер шрифта заголовка в полупунктах (w:sz),
// прямое форматирование прогона, а не ссылка на встроенный стиль Word
// ("Heading2" и т.п.) — у нашего минимального docx нет styles.xml, прямые
// свойства надёжнее, чем ссылка на стиль, которого может не быть.
func headingSizeHalfPoints(level int) string {
	switch level {
	case 2:
		return "32" // 16pt
	case 4:
		return "24" // 12pt
	default:
		return "28" // 14pt (level 3)
	}
}

// renderInlineDocxRuns — прогоны (w:r) инлайн-узлов; headingLevel>0 — форсирует
// bold+размер на ВСЕХ прогонах (это заголовок), иначе — по своим Bold/Italic.
func renderInlineDocxRuns(ctx *placeholderCtx, nodes []InlineNode, headingLevel int) string {
	var b strings.Builder
	for _, n := range nodes {
		// Плейсхолдер атрибута data_type="photo" вне таблицы (2026-08-24) — та же логика,
		// что уже в renderInlineHTML: значение — ссылка на фото, вставляем как реальную
		// картинку (<w:drawing>), а не как текст ссылки.
		if n.Type == "placeholder" && n.Source == "attribute" && ctx.attrsByID[n.AttributeID].DataType == "photo" {
			if drawing := ctx.photoRegistry.register(resolvePlaceholder(ctx, n)); drawing != "" {
				b.WriteString(drawing)
			}
			continue
		}
		text := n.Text
		if n.Type == "placeholder" {
			// event_log (2026-08-28, WP3c ч.2) — сырое значение через aggregateSeries
			// напрямую, ДО того, как resolvePlaceholder/fmtVal превратит его в
			// нечитаемый дамп — та же причина, что у photo выше.
			if n.Source == "attribute" && ctx.attrsByID[n.AttributeID].DataType == "event_log" {
				text = formatEventLog(aggregateSeries(ctx.series, n.AttributeID, n.Agg))
			} else {
				text = resolvePlaceholder(ctx, n)
			}
		}
		bold := n.Bold || headingLevel > 0
		italic := n.Italic
		var rPr strings.Builder
		if bold {
			rPr.WriteString("<w:b/>")
		}
		if italic {
			rPr.WriteString("<w:i/>")
		}
		if n.Sup {
			rPr.WriteString(`<w:vertAlign w:val="superscript"/>`)
		} else if n.Sub {
			rPr.WriteString(`<w:vertAlign w:val="subscript"/>`)
		}
		if headingLevel > 0 {
			fmt.Fprintf(&rPr, `<w:sz w:val="%s"/>`, headingSizeHalfPoints(headingLevel))
		}
		rPrStr := ""
		if rPr.Len() > 0 {
			rPrStr = "<w:rPr>" + rPr.String() + "</w:rPr>"
		}
		fmt.Fprintf(&b, `<w:r>%s<w:t xml:space="preserve">%s</w:t></w:r>`, rPrStr, xmlEscape(text))
	}
	return b.String()
}

// docxCenteredParagraph — <w:p> с <w:jc w:val="center"/> (2026-08-24, по прямому
// запросу пользователя — таблицы результатов выравниваются по центру и в HTML,
// и в документах). Общий хелпер header/data-строк renderTableDocx.
func docxCenteredParagraph(runProps, text string) string {
	return fmt.Sprintf(`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r>%s<w:t>%s</w:t></w:r></w:p>`,
		runProps, xmlEscape(text))
}

func renderTableDocx(ctx *placeholderCtx, columns []TableColumn) string {
	var b strings.Builder
	b.WriteString(`<w:tbl><w:tblPr><w:tblBorders><w:top w:val="single"/><w:left w:val="single"/><w:bottom w:val="single"/><w:right w:val="single"/><w:insideH w:val="single"/><w:insideV w:val="single"/></w:tblBorders></w:tblPr>`)
	b.WriteString(`<w:tr>`)
	for _, c := range columns {
		fmt.Fprintf(&b, `<w:tc>%s</w:tc>`,
			docxCenteredParagraph(`<w:rPr><w:b/></w:rPr>`, tableColumnHeader(c, ctx.attrsByID)))
	}
	b.WriteString(`</w:tr>`)
	for i, sv := range ctx.series {
		b.WriteString(`<w:tr>`)
		for _, c := range columns {
			if c.Kind == "series_no" || c.Kind == "sequential" {
				fmt.Fprintf(&b, `<w:tc>%s</w:tc>`, docxCenteredParagraph("", strconv.Itoa(i+1)))
				continue
			}
			if c.Kind == "photo_before" || c.Kind == "photo_after" {
				if url := seriesPhotoAt(ctx, c.Kind, i); url != "" {
					if drawing := ctx.photoRegistry.register(url); drawing != "" {
						fmt.Fprintf(&b, `<w:tc><w:p><w:pPr><w:jc w:val="center"/></w:pPr>%s</w:p></w:tc>`, drawing)
						continue
					}
				}
				b.WriteString(`<w:tc><w:p/></w:tc>`)
				continue
			}
			if ctx.attrsByID[c.AttributeID].DataType == "photo" {
				if url, ok := sv[c.AttributeID].(string); ok {
					if drawing := ctx.photoRegistry.register(url); drawing != "" {
						fmt.Fprintf(&b, `<w:tc><w:p><w:pPr><w:jc w:val="center"/></w:pPr>%s</w:p></w:tc>`, drawing)
						continue
					}
				}
				b.WriteString(`<w:tc><w:p/></w:tc>`)
				continue
			}
			// event_log (2026-08-28, WP3c ч.2) — та же логика, что photo выше.
			if ctx.attrsByID[c.AttributeID].DataType == "event_log" {
				fmt.Fprintf(&b, `<w:tc>%s</w:tc>`, docxCenteredParagraph("", formatEventLog(sv[c.AttributeID])))
				continue
			}
			fmt.Fprintf(&b, `<w:tc>%s</w:tc>`, docxCenteredParagraph("", fmtVal(sv[c.AttributeID])))
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl>`)
	return b.String()
}

// docxAlignPPr — <w:pPr><w:jc .../></w:pPr> для paragraph/heading (2026-08-24);
// "" (слева) не выводит <w:pPr> вовсе. OOXML называет "по ширине" не "justify",
// а "both".
func docxAlignPPr(align string) string {
	if align == "" {
		return ""
	}
	if align == "justify" {
		align = "both"
	}
	return fmt.Sprintf(`<w:pPr><w:jc w:val="%s"/></w:pPr>`, align)
}

// renderStaticTableDocx — аналог renderStaticTableHTML для DOCX (2026-08-24).
func renderStaticTableDocx(ctx *placeholderCtx, rows [][][]InlineNode) string {
	var b strings.Builder
	b.WriteString(`<w:tbl><w:tblPr><w:tblBorders><w:top w:val="single"/><w:left w:val="single"/><w:bottom w:val="single"/><w:right w:val="single"/><w:insideH w:val="single"/><w:insideV w:val="single"/></w:tblBorders></w:tblPr>`)
	for _, row := range rows {
		b.WriteString(`<w:tr>`)
		for _, cell := range row {
			fmt.Fprintf(&b, `<w:tc><w:p>%s</w:p></w:tc>`, renderInlineDocxRuns(ctx, cell, 0))
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl>`)
	return b.String()
}

func renderNodeDocx(ctx *placeholderCtx, n RichNode) string {
	switch n.Type {
	case "heading":
		lvl := n.Level
		if lvl < 2 || lvl > 4 {
			lvl = 3
		}
		return `<w:p>` + docxAlignPPr(n.Align) + renderInlineDocxRuns(ctx, n.Children, lvl) + `</w:p>`
	case "bullet_list":
		var b strings.Builder
		for _, item := range n.Items {
			b.WriteString(`<w:p><w:r><w:t xml:space="preserve">• </w:t></w:r>`)
			b.WriteString(renderInlineDocxRuns(ctx, item, 0))
			b.WriteString(`</w:p>`)
		}
		return b.String()
	case "table":
		return renderTableDocx(ctx, n.Columns)
	case "static_table":
		return renderStaticTableDocx(ctx, n.Rows)
	default: // "paragraph"
		return `<w:p>` + docxAlignPPr(n.Align) + renderInlineDocxRuns(ctx, n.Children, 0) + `</w:p>`
	}
}

// docxImage — одно изображение, которое войдёт в итоговый .docx как word/media/*
// (2026-08-24, см. AGENTS.md "фото в протоколе" — до этого фикса DOCX фото не
// встраивал вовсе, только текстовую пометку).
type docxImage struct {
	relID       string
	mediaName   string
	contentType string
	data        []byte
}

const emuPerInch = 914400

// docxPhotoRegistry собирает фото-вложения, встречающиеся при рендере DOCX одного
// протокола (общий на все методы документа — relationship id/имена media-файлов не
// должны пересекаться между методами). register вызывается из renderTableDocx/
// renderInlineDocxRuns при встрече атрибута data_type="photo".
type docxPhotoRegistry struct {
	ctx    context.Context
	s3     *S3Store
	images []docxImage
}

// register скачивает фото по значению атрибута (ссылка вида .../api/lab/file-redirect?
// key=... — см. files.go uploadFileBytes) и возвращает готовый <w:drawing> XML для
// вставки в run. "" — если значение пустое/не удалось разобрать ключ/не удалось
// скачать (фото просто не появится в этом месте документа, остальной рендер не рвётся).
func (reg *docxPhotoRegistry) register(rawURL string) string {
	if reg == nil || strings.TrimSpace(rawURL) == "" {
		return ""
	}
	key, ok := fileRedirectKey(rawURL)
	if !ok {
		return ""
	}
	data, err := reg.s3.Get(reg.ctx, key)
	if err != nil {
		log.Printf("docx photo: s3 get %q: %v", key, err)
		return ""
	}
	ext, contentType := "jpg", "image/jpeg"
	if strings.HasSuffix(strings.ToLower(key), ".png") {
		ext, contentType = "png", "image/png"
	}
	idx := len(reg.images) + 1
	relID := fmt.Sprintf("rIdPhoto%d", idx)
	img := docxImage{
		relID:       relID,
		mediaName:   fmt.Sprintf("image%d.%s", idx, ext),
		contentType: contentType,
		data:        data,
	}
	reg.images = append(reg.images, img)
	cx, cy := docxImageExtent(data)
	return docxDrawingXML(relID, cx, cy)
}

// fileRedirectKey достаёт S3-ключ из ссылки вида ".../api/lab/file-redirect?key=..." —
// та же ссылка, что хранится в values/measurement_results (uploadFileBytes, files.go).
func fileRedirectKey(rawURL string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", false
	}
	key := u.Query().Get("key")
	if key == "" {
		return "", false
	}
	return key, true
}

// docxImageExtent — размер картинки в EMU (1 дюйм = 914400 EMU) в квадратной рамке
// максимум 2х2 дюйма, с сохранением пропорций (тот же принцип, что max-width/max-height
// в HTML-рендере той же фото-ячейки, см. renderTableHTML) — если размеры прочитать не
// удалось, отдаём квадрат по умолчанию, не рвём документ.
func docxImageExtent(data []byte) (cx, cy int64) {
	const maxBox = int64(emuPerInch * 2)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return maxBox, maxBox
	}
	w, h := float64(cfg.Width), float64(cfg.Height)
	scale := float64(maxBox) / w
	if hScale := float64(maxBox) / h; hScale < scale {
		scale = hScale
	}
	return int64(w * scale), int64(h * scale)
}

// docxDrawingXML — <w:r><w:drawing>...</w:drawing></w:r> с встроенным изображением по
// relationship id. Пространства имён wp/a/pic/r объявлены прямо на использующих их
// элементах (не в корневом <w:document>, чтобы не трогать остальной, уже стабильный,
// generator) — валидно для OOXML, в отличие от HTML там разрешено объявлять namespace
// на любом элементе, не только на корне.
func docxDrawingXML(relID string, cx, cy int64) string {
	return fmt.Sprintf(
		`<w:r><w:drawing>`+
			`<wp:inline distT="0" distB="0" distL="0" distR="0" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing">`+
			`<wp:extent cx="%d" cy="%d"/>`+
			`<wp:docPr id="0" name="Photo"/>`+
			`<a:graphic xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">`+
			`<a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture">`+
			`<pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">`+
			`<pic:nvPicPr><pic:cNvPr id="0" name="Photo"/><pic:cNvPicPr/></pic:nvPicPr>`+
			`<pic:blipFill><a:blip xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>`+
			`<pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr>`+
			`</pic:pic></a:graphicData></a:graphic>`+
			`</wp:inline></w:drawing></w:r>`,
		cx, cy, relID, cx, cy)
}

// DOCX-writer раньше не встраивал изображения вовсе (нет media-частей/relationships) —
// график секции по-прежнему только текстовой пометкой (не запрошено — фото, не графики,
// см. AGENTS.md "фото в протоколе", 2026-08-24); фото внутри таблиц/инлайн — теперь
// реальные <w:drawing>, см. docxPhotoRegistry.
func renderBlockChartDocx(doc *strings.Builder, m protocolMethod, chartID string) {
	if chartID == "" {
		return
	}
	chartCfg := findChartConfigByID(m.ChartConfigs, chartID)
	if chartCfg == nil {
		return
	}
	title, _ := chartCfg["title"].(string)
	if title == "" {
		title, _ = chartCfg["id"].(string)
	}
	fmt.Fprintf(doc, `<w:p><w:r><w:rPr><w:i/></w:rPr><w:t>%s</w:t></w:r></w:p>`,
		xmlEscape(fmt.Sprintf("[График: %s — см. HTML-версию]", title)))
}

func (s *Server) protocolDocx(ctx context.Context, p *protocolData, kind string) ([]byte, error) {
	// photoRegistry общий на весь документ (2026-08-24) — назначается КАЖДОМУ методу
	// протокола ДО рендера, иначе фото первого метода получили бы rIdPhoto1/image1, а
	// фото второго метода — те же id, если бы у каждого была своя нумерация.
	photoRegistry := &docxPhotoRegistry{ctx: ctx, s3: s.s3}
	for i := range p.Methods {
		if p.Methods[i].Ctx != nil {
			p.Methods[i].Ctx.photoRegistry = photoRegistry
		}
	}

	var doc strings.Builder
	doc.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	doc.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	doc.WriteString(`<w:body>`)
	// Жёсткий заголовок убран (2026-08-27) — см. тот же комментарий в protocolHTML.
	for _, m := range p.Methods {
		for _, blk := range m.Blocks {
			m.Ctx.currentBlockID = blk.ID
			for _, node := range blk.Content {
				doc.WriteString(renderNodeDocx(m.Ctx, node))
			}
			renderBlockChartDocx(&doc, m, blk.ChartID)
		}
	}
	doc.WriteString(`<w:sectPr/></w:body></w:document>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(doc.String())); err != nil {
		return nil, err
	}
	ctFile, err := zw.Create("[Content_Types].xml")
	if err != nil {
		return nil, err
	}
	// Extension-дефолты для встреченных типов фото (2026-08-24) — без этого Word не
	// знает, как открывать части word/media/*.jpg|png (см. docxPhotoRegistry).
	var extraContentTypes strings.Builder
	seenExt := map[string]bool{}
	for _, img := range photoRegistry.images {
		ext := strings.TrimPrefix(img.mediaName[strings.LastIndex(img.mediaName, "."):], ".")
		if seenExt[ext] {
			continue
		}
		seenExt[ext] = true
		fmt.Fprintf(&extraContentTypes, `<Default Extension="%s" ContentType="%s"/>`, ext, img.contentType)
	}
	ctFile.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		extraContentTypes.String() +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`</Types>`))
	// _rels/.rels (2026-08-24) — ОБЯЗАТЕЛЬНАЯ часть пакета OPC/OOXML: без неё
	// Word не может найти точку входа (какой part — главный документ) и
	// отказывается открывать файл как повреждённый — именно баг, о котором
	// сообщил пользователь ("файлы ворд... не читаемые, ошибка открытия").
	// [Content_Types].xml/word/document.xml одних было НЕДОСТАТОЧНО — этот
	// файл раньше просто отсутствовал в пакете.
	rels, err := zw.Create("_rels/.rels")
	if err != nil {
		return nil, err
	}
	if _, err := rels.Write([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
		`</Relationships>`)); err != nil {
		return nil, err
	}
	// word/_rels/document.xml.rels — раньше всегда пустой (styles.xml/numbering.xml
	// сознательно не реализованы), теперь — по одной связи на каждое встреченное фото
	// (см. docxPhotoRegistry.register, r:embed в docxDrawingXML ссылается именно на
	// эти Id). Пустой rels-файл части — общепринятая практика, читалки его ждут даже
	// при отсутствии реальных связей, поэтому пишем его всегда, фото или нет.
	var photoRels strings.Builder
	for _, img := range photoRegistry.images {
		fmt.Fprintf(&photoRels, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/%s"/>`,
			img.relID, img.mediaName)
	}
	docRels, err := zw.Create("word/_rels/document.xml.rels")
	if err != nil {
		return nil, err
	}
	if _, err := docRels.Write([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		photoRels.String() +
		`</Relationships>`)); err != nil {
		return nil, err
	}
	// word/media/* — сами байты фото (2026-08-24) — без этой части relationship выше
	// ссылался бы в никуда, Word отказался бы открыть документ как повреждённый (тот
	// же класс ошибки, что уже был описан для _rels/.rels выше).
	for _, img := range photoRegistry.images {
		mediaPart, err := zw.Create("word/media/" + img.mediaName)
		if err != nil {
			return nil, err
		}
		if _, err := mediaPart.Write(img.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// ---- Сбор данных ----

func (s *Server) loadStatsRow(ctx context.Context, requestID, methodID int64) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
SELECT values FROM measurement_results
WHERE request_id = $1 AND method_id = $2 AND is_statistical_row = true ORDER BY id DESC LIMIT 1`,
		requestID, methodID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(raw) > 0 && string(raw) != "{}" {
		_ = json.Unmarshal(raw, &out)
	}
	return out, nil
}

func (s *Server) loadAggregatedRow(ctx context.Context, requestID, methodID int64) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
SELECT result_data FROM aggregated_results
WHERE request_id = $1 AND method_id = $2 ORDER BY id DESC LIMIT 1`, requestID, methodID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(raw) > 0 && string(raw) != "{}" {
		_ = json.Unmarshal(raw, &out)
	}
	return out, nil
}

// buildProtocol собирает данные заявки для документа (kind: "ui"|"excerpt"|"protocol").
func (s *Server) buildProtocol(ctx context.Context, requestID int64, kind string) (*protocolData, error) {
	req, err := s.loadRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	var objectName string
	var objectChars map[string]any
	if req.ObjectID > 0 {
		var on string
		var charsRaw []byte
		err := s.pool.QueryRow(ctx, `SELECT name, characteristics FROM objects WHERE id = $1`, req.ObjectID).Scan(&on, &charsRaw)
		if err == nil {
			objectName = on
			objectChars = map[string]any{}
			if len(charsRaw) > 0 && string(charsRaw) != "{}" {
				_ = json.Unmarshal(charsRaw, &objectChars)
			}
		}
	}
	// Системный плейсхолдер "inventor" (2026-08-23) — req.InventorID резолвится в
	// имя один раз здесь, не на каждый плейсхолдер (см. sbe-lims/AGENTS.md).
	var inventorName string
	if req.InventorID > 0 {
		_ = s.pool.QueryRow(ctx, `SELECT name FROM inventors WHERE id = $1`, req.InventorID).Scan(&inventorName)
	}
	p := &protocolData{
		RequestID: requestID,
		// Полный номер в заголовке документа (2026-08-24) — см. комментарий у
		// resolveSystemPlaceholder("number") выше про CustomerNumber vs LabNumber.
		Number:      req.CustomerNumber,
		Title:       req.Title,
		Owner:       req.OwnerEmail,
		Object:      objectName,
		EKN:         req.EKN,
		Priority:    req.Priority,
		TestPurpose: req.TestPurpose,
		Status:      req.Status,
		CreatedAt:   req.CreatedAt,
	}
	// метод заявки (1 заявка = 1 метод)
	mRows, err := s.pool.Query(ctx, `
SELECT r.method_id, m.name FROM requests r
JOIN methods m ON m.id = r.method_id WHERE r.id = $1 ORDER BY m.id`, requestID)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()
	type mid struct {
		id   int64
		name string
	}
	var mids []mid
	for mRows.Next() {
		var m mid
		if err := mRows.Scan(&m.id, &m.name); err != nil {
			continue
		}
		mids = append(mids, m)
	}
	for _, m := range mids {
		series, err := s.loadSeriesValues(ctx, requestID, m.id)
		if err != nil {
			continue
		}
		stats, _ := s.loadStatsRow(ctx, requestID, m.id)
		agg, _ := s.loadAggregatedRow(ctx, requestID, m.id)
		photoBefore, photoAfter, _ := s.loadSeriesPhotos(ctx, requestID, m.id)
		cfg, cfgErr := s.loadMethodConfig(ctx, m.id)
		if cfgErr != nil {
			cfg = &MethodConfig{}
		}
		targetIndicator, _ := s.loadTargetIndicator(ctx, requestID, m.id)
		blocks := filterBlocksForKind(cfg.Presentation.Blocks, kind)
		pctx := &placeholderCtx{
			req: req, objectName: objectName, objectChars: objectChars,
			targetIndicator: targetIndicator, inventorName: inventorName, methodName: m.name,
			attrsByID: methodAttributesByID(cfg),
			series:    series, stats: stats, agg: agg,
			photoBefore: photoBefore, photoAfter: photoAfter,
			headingNumbers: computeHeadingNumbers(blocks),
		}
		p.Methods = append(p.Methods, protocolMethod{
			MethodID:     m.id,
			MethodName:   m.name,
			Blocks:       blocks,
			ChartConfigs: cfg.ChartConfigs,
			Ctx:          pctx,
		})
	}
	return p, nil
}

// ---- Handler ----

// templateKindFromQuery — ?template=ui|excerpt|protocol, по умолчанию "protocol"
// (старое поведение для клиентов без выбора вида).
func templateKindFromQuery(r *http.Request) string {
	switch r.URL.Query().Get("template") {
	case "ui":
		return "ui"
	case "excerpt":
		return "excerpt"
	default:
		return "protocol"
	}
}

// formatFromQuery — ?format=html (только HTML, без сборки DOCX — для карточки
// результатов/предпросмотра, экономит время) | "full" (по умолчанию, оба формата).
func formatFromQuery(r *http.Request) string {
	if r.URL.Query().Get("format") == "html" {
		return "html"
	}
	return "full"
}

func (s *Server) handleProtocol(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	kind := templateKindFromQuery(r)
	p, err := s.buildProtocol(r.Context(), requestID, kind)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	resp := map[string]any{
		"html":         protocolHTML(p, kind),
		"docx_base64":  "",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if formatFromQuery(r) != "html" {
		docxBytes, err := s.protocolDocx(r.Context(), p, kind)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "docx: " + err.Error()})
			return
		}
		resp["docx_base64"] = toBase64(docxBytes)
	}
	writeJSON(w, http.StatusOK, resp)
}

func toBase64(b []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n int
		rem := len(b) - i
		switch {
		case rem >= 3:
			n = int(b[i])<<16 | int(b[i+1])<<8 | int(b[i+2])
			out.WriteByte(chars[(n>>18)&63])
			out.WriteByte(chars[(n>>12)&63])
			out.WriteByte(chars[(n>>6)&63])
			out.WriteByte(chars[n&63])
		case rem == 2:
			n = int(b[i])<<16 | int(b[i+1])<<8
			out.WriteByte(chars[(n>>18)&63])
			out.WriteByte(chars[(n>>12)&63])
			out.WriteByte(chars[(n>>6)&63])
			out.WriteByte('=')
		case rem == 1:
			n = int(b[i]) << 16
			out.WriteByte(chars[(n>>18)&63])
			out.WriteByte(chars[(n>>12)&63])
			out.WriteByte('=')
			out.WriteByte('=')
		}
	}
	return out.String()
}
