package main

import "fmt"

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

// composeAppendixNodes — узлы приложения: свод по показателям, распределение
// оценок и таблица аналогов.
func composeAppendixNodes(rc *reportContext) []RichNode {
	nodes := []RichNode{headingNode("Приложение. Статистика по аналогичным объектам", 2)}

	ekn := rc.EKN
	if ekn == "" {
		ekn = "—"
	}
	intro := fmt.Sprintf("Выборка: завершённые испытания по тому же методу «%s» для объектов с ЕКН %s. Найдено аналогов: %d.",
		rc.MethodName, ekn, len(rc.Analogues))
	nodes = append(nodes, paragraphNode(intro))

	if len(rc.IndicatorStats) == 0 {
		nodes = append(nodes, paragraphNode("Числовых данных по аналогичным объектам недостаточно для статистической обработки."))
		return nodes
	}

	// Свод по показателям (числовые).
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

	if len(rc.Analogues) > 0 {
		rows := [][][]InlineNode{{textCell("Номер"), textCell("Объект"), textCell("Партия"), textCell("Завершена"), textCell("Результат"), textCell("Соответствие")}}
		for _, a := range rc.Analogues {
			rows = append(rows, [][]InlineNode{
				textCell(a.Number), textCell(a.Object), textCell(a.Batch),
				textCell(a.CompletedAt), textCell(a.Result), textCell(a.Compliance),
			})
		}
		nodes = append(nodes, headingNode("Испытания аналогичных объектов", 3), RichNode{Type: "static_table", Rows: rows})
	}
	return nodes
}
