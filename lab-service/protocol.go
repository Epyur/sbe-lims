package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
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
	attrsByID       map[string]MethodAttribute
	series          []map[string]any
	stats           map[string]any
	agg             map[string]any
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
	case "report_date":
		return ctx.req.ReportDate
	case "samples_in_date":
		return ctx.req.SamplesInDate
	case "exp_date":
		return ctx.req.ExpDate
	case "amb_temp":
		return ctx.req.AmbTemp
	case "amb_pres":
		return ctx.req.AmbPres
	case "amb_moist":
		return ctx.req.AmbMoist
	}
	return ""
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
			if c.Kind == "series_no" {
				fmt.Fprintf(&b, "<td>%d</td>", i+1)
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
	fmt.Fprintf(&b, "<h1>%s № %s</h1>", docTitle(kind), html.EscapeString(p.Number))
	b.WriteString("<table>")
	fieldRow(&b, "Наименование", p.Title)
	fieldRow(&b, "Объект", p.Object)
	fieldRow(&b, "ЕКН", p.EKN)
	fieldRow(&b, "Заказчик", p.Owner)
	fieldRow(&b, "Статус", p.Status)
	fieldRow(&b, "Создана", p.CreatedAt)
	b.WriteString("</table>")
	for _, m := range p.Methods {
		fmt.Fprintf(&b, "<h2>Метод: %s</h2>", html.EscapeString(m.MethodName))
		renderChart := renderBlockChartHTML(&b, m)
		for _, blk := range m.Blocks {
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

func fieldRow(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, "<tr><td><b>%s</b></td><td>%s</td></tr>", html.EscapeString(label), html.EscapeString(value))
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
		text := n.Text
		if n.Type == "placeholder" {
			text = resolvePlaceholder(ctx, n)
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
			val := fmtVal(sv[c.AttributeID])
			if c.Kind == "series_no" {
				val = strconv.Itoa(i + 1)
			}
			fmt.Fprintf(&b, `<w:tc>%s</w:tc>`, docxCenteredParagraph("", val))
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

// DOCX-writer не встраивает изображения (нет media-частей/relationships) —
// график секции, как и фото внутри таблиц, в DOCX только текстовой пометкой.
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

func protocolDocx(p *protocolData, kind string) ([]byte, error) {
	var doc strings.Builder
	doc.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	doc.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	doc.WriteString(`<w:body>`)
	fmt.Fprintf(&doc, `<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>%s</w:t></w:r></w:p>`,
		xmlEscape(docTitle(kind)+" № "+p.Number))
	docxTableRow(&doc, "Наименование", p.Title)
	docxTableRow(&doc, "Объект", p.Object)
	docxTableRow(&doc, "ЕКН", p.EKN)
	docxTableRow(&doc, "Заказчик", p.Owner)
	docxTableRow(&doc, "Статус", p.Status)
	for _, m := range p.Methods {
		fmt.Fprintf(&doc, `<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>%s</w:t></w:r></w:p>`,
			xmlEscape("Метод: "+m.MethodName))
		for _, blk := range m.Blocks {
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
	ct, err := zw.Create("[Content_Types].xml")
	if err != nil {
		return nil, err
	}
	ct.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
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
	// word/_rels/document.xml.rels — у document.xml нет внешних ссылок
	// (изображений/styles.xml/numbering.xml — сознательно не реализованы), но
	// пустой rels-файл части — общепринятая практика, некоторые читалки его
	// ожидают даже при отсутствии реальных связей.
	docRels, err := zw.Create("word/_rels/document.xml.rels")
	if err != nil {
		return nil, err
	}
	if _, err := docRels.Write([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func docxTableRow(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, `<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>%s: </w:t></w:r><w:r><w:t>%s</w:t></w:r></w:p>`,
		xmlEscape(label), xmlEscape(value))
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
		cfg, cfgErr := s.loadMethodConfig(ctx, m.id)
		if cfgErr != nil {
			cfg = &MethodConfig{}
		}
		targetIndicator, _ := s.loadTargetIndicator(ctx, requestID, m.id)
		pctx := &placeholderCtx{
			req: req, objectName: objectName, objectChars: objectChars,
			targetIndicator: targetIndicator, inventorName: inventorName,
			attrsByID: methodAttributesByID(cfg),
			series:    series, stats: stats, agg: agg,
		}
		p.Methods = append(p.Methods, protocolMethod{
			MethodID:     m.id,
			MethodName:   m.name,
			Blocks:       filterBlocksForKind(cfg.Presentation.Blocks, kind),
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
		docxBytes, err := protocolDocx(p, kind)
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
