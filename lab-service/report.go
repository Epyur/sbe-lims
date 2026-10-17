package main

import (
	"fmt"
	"sort"
	"strings"
)

// ---- Композиция тела отчёта (2026-10-16) ----
//
// Тело = блоки протокола метода (как настроено, filterBlocksForKind "protocol")
// + ИИ-разделы (по report_instruction метода) + приложение со статистикой по
// аналогичным объектам. Всё — те же DocumentBlock/RichNode, что рендерят
// protocolDocx/reportDocx, отдельного формата нет.

// aiSection/aiSections — разобранный ответ ИИ (см. report_ai.go). Текст только
// (заголовок/абзацы/маркеры) — таблицы/графики ИИ не формирует.
type aiSection struct {
	Heading    string   `json:"heading"`
	Paragraphs []string `json:"paragraphs"`
	Bullets    []string `json:"bullets"`
}

type aiSections struct {
	Sections []aiSection `json:"sections"`
}

func textNode(text string) InlineNode { return InlineNode{Type: "text", Text: text} }

func paragraphNode(text string) RichNode {
	return RichNode{Type: "paragraph", Children: []InlineNode{textNode(text)}}
}

func headingNode(text string, level int) RichNode {
	return RichNode{Type: "heading", Level: level, Children: []InlineNode{textNode(text)}}
}

func bulletListNode(items []string) RichNode {
	rows := make([][]InlineNode, 0, len(items))
	for _, it := range items {
		rows = append(rows, []InlineNode{textNode(it)})
	}
	return RichNode{Type: "bullet_list", Items: rows}
}

func textCell(text string) []InlineNode { return []InlineNode{textNode(text)} }

// composeReportBlocks дополняет блоки метода (p.Methods[0]) ИИ-разделами и
// приложением. 1 заявка = 1 метод, поэтому метод один; при пустом ai ИИ-блок
// не добавляется (отчёт = протокол + приложение).
func composeReportBlocks(p *protocolData, rc *reportContext, ai *aiSections) {
	if p == nil || len(p.Methods) == 0 {
		return
	}
	m := &p.Methods[0]

	if ai != nil && len(ai.Sections) > 0 {
		block := DocumentBlock{ID: "report_ai", Title: "Анализ испытания"}
		block.Content = append(block.Content, headingNode("Анализ результатов испытания", 2))
		for _, sec := range ai.Sections {
			if sec.Heading != "" {
				block.Content = append(block.Content, headingNode(sec.Heading, 3))
			}
			for _, par := range sec.Paragraphs {
				if par != "" {
					block.Content = append(block.Content, paragraphNode(par))
				}
			}
			if len(sec.Bullets) > 0 {
				block.Content = append(block.Content, bulletListNode(sec.Bullets))
			}
		}
		if len(block.Content) > 1 {
			m.Blocks = append(m.Blocks, block)
		}
	}

	appendix := DocumentBlock{ID: "report_appendix", Title: "Приложение: статистика"}
	appendix.Content = composeAppendixNodes(rc)
	m.Blocks = append(m.Blocks, appendix)
}

// composeAppendixNodes — узлы приложения: материал выборки, свод по показателям,
// ступени аналогичности, мини-таблицы по толщинам и таблица аналогичных испытаний.
//
// Ступени (решения пользователя 2026-10-17): 1 — тот же материал с ЕКН,
// 2 — тот же материал, ЕКН не указан, 3 — модификация линейки. Статистика по
// толщинам строится только по ступени 1 (ступени не смешиваются).
func composeAppendixNodes(rc *reportContext) []RichNode {
	nodes := []RichNode{headingNode("Приложение. Статистика по аналогичным объектам", 2)}

	// Шапка: по какому материалу собраны аналоги. Раньше здесь был ЕКН — теперь
	// аналогичность определяется материалом, а ЕКН оставлен пометкой заявки.
	materialLine := materialKeyLabel(rc.Material)
	if materialLine == "не определён" {
		materialLine = "материал не определён"
	}
	eknNote := "ЕКН заявки не указан"
	if rc.EKN != "" {
		eknNote = "ЕКН заявки " + rc.EKN
	}
	thicknessNote := ""
	if t := formatThickness(rc.Thickness); t != "" {
		thicknessNote = fmt.Sprintf(", толщина %s мм", t)
	}
	intro := fmt.Sprintf("Выборка: завершённые испытания методом «%s» для того же материала (%s%s; %s). Найдено аналогов: %d.",
		rc.MethodName, materialLine, thicknessNote, eknNote, len(rc.Analogues))
	nodes = append(nodes, paragraphNode(intro))

	if len(rc.IndicatorStats) == 0 {
		nodes = append(nodes, paragraphNode("Числовых данных по аналогичным объектам недостаточно для статистической обработки."))
		return nodes
	}

	// Свод по показателям (числовые) — по всем ступеням, как было.
	numRows := [][][]InlineNode{{textCell("Показатель"), textCell("Кол-во"), textCell("Среднее"), textCell("Минимум"), textCell("Максимум")}}
	distRows := [][][]InlineNode{{textCell("Показатель"), textCell("Значение"), textCell("Кол-во")}}
	hasDist := false
	for _, st := range rc.IndicatorStats {
		if st.HasNumeric {
			numRows = append(numRows, [][]InlineNode{
				textCell(st.Name),
				textCell(fmt.Sprintf("%d", st.Count)),
				textCell(fmtVal(st.Avg)),
				textCell(fmtVal(st.Min)),
				textCell(fmtVal(st.Max)),
			})
		}
		for val, n := range st.Distribution {
			hasDist = true
			distRows = append(distRows, [][]InlineNode{textCell(st.Name), textCell(val), textCell(fmt.Sprintf("%d", n))})
		}
	}
	if len(numRows) > 1 {
		nodes = append(nodes, headingNode("Числовые показатели", 3), RichNode{Type: "static_table", Rows: numRows})
	}
	if hasDist {
		nodes = append(nodes, headingNode("Распределение оценок", 3), RichNode{Type: "static_table", Rows: distRows})
	}

	// Зависимость от толщины: отдельная мини-таблица на КАЖДЫЙ показатель
	// (решение пользователя 2026-10-17: одна широкая таблица отвергнута — при
	// 6+ показателях в DOCX она нечитаема).
	if thicknessNodes := composeThicknessNodes(rc); len(thicknessNodes) > 0 {
		nodes = append(nodes, thicknessNodes...)
	}

	// Таблицы аналогичных испытаний — по ступеням, отдельно.
	nodes = append(nodes, composeAnalogueTables(rc.Analogues)...)
	return nodes
}

// composeThicknessNodes — раздел «Зависимость от толщины»: по мини-таблице на
// каждый показатель, у которого есть разрез, плюс строка вывода о тренде.
func composeThicknessNodes(rc *reportContext) []RichNode {
	var withSplit []indicatorStat
	for _, st := range rc.IndicatorStats {
		if len(st.ByThickness) > 0 {
			withSplit = append(withSplit, st)
		}
	}
	if len(withSplit) == 0 {
		return nil
	}

	nodes := []RichNode{headingNode("Зависимость от толщины", 3)}
	nodes = append(nodes, paragraphNode(
		"Все толщины одной линейки рассматриваются как один материал; ниже — как меняются показатели при изменении толщины. "+
			"Вывод о закономерности приводится только при наличии не менее трёх испытаний на каждую толщину."))

	for _, st := range withSplit {
		nodes = append(nodes, paragraphNode(st.Name))
		nodes = append(nodes, RichNode{Type: "static_table", Rows: thicknessTableRows(st)})
		if st.Trend != nil {
			nodes = append(nodes, paragraphNode(trendSentence(st)))
		} else {
			nodes = append(nodes, paragraphNode(
				"Вывод о зависимости показателя от толщины не приводится: данных недостаточно (менее трёх испытаний на толщину)."))
		}
	}
	return nodes
}

// thicknessTableRows — строки мини-таблицы одного показателя по толщинам.
func thicknessTableRows(st indicatorStat) [][][]InlineNode {
	numeric := st.HasNumeric
	if numeric {
		rows := [][][]InlineNode{{
			textCell("Толщина, мм"), textCell("Испытаний"), textCell("Значений"),
			textCell("Среднее"), textCell("Минимум"), textCell("Максимум"),
		}}
		for _, b := range st.ByThickness {
			thickness := b.Thickness
			if thickness == "" {
				thickness = "не указана"
			}
			rows = append(rows, [][]InlineNode{
				textCell(thickness), textCell(fmt.Sprintf("%d", b.Tests)),
				textCell(fmt.Sprintf("%d", b.Count)), textCell(fmtVal(b.Avg)),
				textCell(fmtVal(b.Min)), textCell(fmtVal(b.Max)),
			})
		}
		return rows
	}
	rows := [][][]InlineNode{{textCell("Толщина, мм"), textCell("Испытаний"), textCell("Значения")}}
	for _, b := range st.ByThickness {
		thickness := b.Thickness
		if thickness == "" {
			thickness = "не указана"
		}
		rows = append(rows, [][]InlineNode{
			textCell(thickness), textCell(fmt.Sprintf("%d", b.Tests)), textCell(distributionText(b.Distribution)),
		})
	}
	return rows
}

// trendSentence — детерминированный вывод о зависимости показателя от толщины.
func trendSentence(st indicatorStat) string {
	trend := st.Trend
	if trend == nil {
		return ""
	}
	switch trend.Direction {
	case "немонотонно":
		return fmt.Sprintf("Зависимость %s от толщины немонотонна: при переходе от %s к %s мм среднее значение меняет направление.",
			st.Name, trend.From, trend.To)
	case "не меняется":
		return fmt.Sprintf("При изменении толщины от %s до %s мм среднее значение «%s» практически не меняется.",
			trend.From, trend.To, st.Name)
	}
	delta := ""
	if trend.Delta != "" {
		delta = fmt.Sprintf(" на %s", trend.Delta)
	}
	return fmt.Sprintf("При увеличении толщины с %s до %s мм среднее значение «%s» %s%s.",
		trend.From, trend.To, st.Name, trend.Direction, delta)
}

// distributionText — «Г2 — 5; Г3 — 1» для нечисловых показателей.
func distributionText(dist map[string]int) string {
	if len(dist) == 0 {
		return ""
	}
	keys := make([]string, 0, len(dist))
	for k := range dist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s — %d", k, dist[k]))
	}
	return strings.Join(parts, "; ")
}

// composeAnalogueTables — таблицы аналогичных испытаний по ступеням.
func composeAnalogueTables(analogues []analogueRow) []RichNode {
	if len(analogues) == 0 {
		return nil
	}
	groups := []struct {
		tier  int
		title string
		note  string
	}{
		{analogueTierSameEKN, "Аналоги: тот же материал (разная фасовка и толщина)",
			"ЕКН у аналогов указан, материал определён по карточке ЕКН."},
		{analogueTierSameName, "Аналоги: тот же материал, ЕКН не указан",
			"Материал определён по названию объекта — в статистику по толщинам не входит."},
		{analogueTierModifier, "Модификации линейки",
			"Та же линейка с другим модификатором (FR / Pro / Arctic / Middle и т. п.) — показаны отдельно, в статистику не входят."},
	}
	var nodes []RichNode
	for _, g := range groups {
		rows := make([]analogueRow, 0, len(analogues))
		for _, a := range analogues {
			if a.Tier == g.tier {
				rows = append(rows, a)
			}
		}
		if len(rows) == 0 {
			continue
		}
		nodes = append(nodes, headingNode(fmt.Sprintf("%s (%d)", g.title, len(rows)), 3))
		nodes = append(nodes, paragraphNode(g.note))
		table := [][][]InlineNode{{
			textCell("Номер"), textCell("Объект"), textCell("Толщина"), textCell("Партия"),
			textCell("Завершена"), textCell("Результат"), textCell("Соответствие"),
		}}
		for _, a := range rows {
			table = append(table, [][]InlineNode{
				textCell(a.Number), textCell(a.Object), textCell(a.Thickness),
				textCell(a.Batch), textCell(a.CompletedAt), textCell(a.Result), textCell(a.Compliance),
			})
		}
		nodes = append(nodes, RichNode{Type: "static_table", Rows: table})
	}
	return nodes
}
