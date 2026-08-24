package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Типы ----

type MeasurementResult struct {
	ID                int64          `json:"id"`
	RequestID         int64          `json:"request_id"`
	MethodID          int64          `json:"method_id"`
	InventorID        int64          `json:"inventor_id"`
	SeriesNum         int            `json:"series_num"`
	Values            map[string]any `json:"values"`
	FileLinks         map[string]any `json:"file_links"`
	PhotoBefore       string         `json:"photo_before"`
	PhotoAfter        string         `json:"photo_after"`
	IsStatisticalRow  bool           `json:"is_statistical_row"`
	CalculationType   string         `json:"calculation_type"`
	SourceSeriesCount int            `json:"source_series_count"`
	SourceSeriesRange string         `json:"source_series_range"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
}

type AggregatedResult struct {
	ID                int64          `json:"id"`
	RequestID         int64          `json:"request_id"`
	MethodID          int64          `json:"method_id"`
	CalculationType   string         `json:"calculation_type"`
	ResultData        map[string]any `json:"result_data"`
	SourceSeriesCount int            `json:"source_series_count"`
	SourceSeriesRange string         `json:"source_series_range"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
}

// MethodConfig — конфиги метода (formulas/classification/chart_configs/input_parameters/presentation/operator_form).
type MethodConfig struct {
	Formulas       []map[string]any   `json:"formulas"`
	Classification []map[string]any   `json:"classification"`
	ChartConfigs   []map[string]any   `json:"chart_configs"`
	InputParams    []map[string]any   `json:"input_parameters"`
	Presentation   MethodPresentation `json:"presentation"`
	OperatorForm   MethodOperatorForm `json:"operator_form"`
}

// InlineNode — текстовый узел форматированного текста ИЛИ плейсхолдер-чип
// (2026-08-23, блочный редактор — заменил "секции с полями/ролями": документ
// теперь состоит из реального форматированного текста с вставленными
// плейсхолдерами, а не из списка атрибутов с видимостью). Source/AttributeID/
// Agg заполнены только когда Type=="placeholder": Source "system" — заявка/
// объект (партия/материал/ЕКН/заказчик и т.п., см. resolveSystemPlaceholder),
// "attribute" — показатель метода; Agg ("avg"|"min"|"max"|"first"|"last") —
// ТОЛЬКО для experiment-уровня атрибута ВНЕ таблицы (внутри RichNode "table"
// агрегация не нужна — там одна строка на серию).
type InlineNode struct {
	Type   string `json:"type"` // "text" | "placeholder"
	Text   string `json:"text,omitempty"`
	Bold   bool   `json:"bold,omitempty"`
	Italic bool   `json:"italic,omitempty"`
	// Sup/Sub — верхний/нижний индекс (2026-08-24, по прямому запросу
	// пользователя: "во всех элементах... добавить возможность вставки
	// верхних/нижних индексов, как это сделано в настройках атрибутов" —
	// там, из-за plain <input>, индекс — юникод-символ; здесь, в настоящем
	// contenteditable, — реальное форматирование, взаимоисключающее (клиент
	// не должен ставить оба одновременно, сервер это не проверяет).
	Sup bool `json:"sup,omitempty"`
	Sub bool `json:"sub,omitempty"`

	Source      string `json:"source,omitempty"` // "system" | "attribute"
	AttributeID string `json:"attribute_id,omitempty"`
	Agg         string `json:"agg,omitempty"`
}

// TableColumn — один столбец динамической таблицы (RichNode Type="table"):
// одна строка на серию, без агрегации (агрегация — только для одиночных
// плейсхолдеров вне таблицы, см. InlineNode.Agg).
type TableColumn struct {
	// Kind — "" (или "attribute", по умолчанию) — обычная колонка атрибута;
	// "series_no" — номер серии (1,2,3...), раньше жёстко прописывался первой
	// колонкой без права пользователя её убрать/переместить/переименовать —
	// теперь обычная колонка в списке, пользователь сам решает, включать ли её
	// и куда поставить (2026-08-23, по жалобе "отсутствует опция создания
	// колонки с номером серии").
	Kind        string `json:"kind,omitempty"`
	AttributeID string `json:"attribute_id,omitempty"`
	Label       string `json:"label,omitempty"`
}

// RichNode — один блочный узел форматированного текста. Align (2026-08-24,
// по запросу пользователя) — "" (слева, по умолчанию) | "center" | "right" |
// "justify"; применимо к paragraph/heading. Rows (2026-08-24) — static_table:
// строка → колонка → inline-содержимое ячейки, введённое пользователем вручную
// (в отличие от Columns/"table" — данные серий, авто-заполняемые ячейки).
type RichNode struct {
	Type     string           `json:"type"`               // "paragraph" | "heading" | "bullet_list" | "table" | "static_table"
	Level    int              `json:"level,omitempty"`    // heading: 2..4
	Align    string           `json:"align,omitempty"`    // paragraph/heading: "" | "center" | "right" | "justify"
	Children []InlineNode     `json:"children,omitempty"` // paragraph/heading
	Items    [][]InlineNode   `json:"items,omitempty"`    // bullet_list: одна строка = один InlineNode[]
	Columns  []TableColumn    `json:"columns,omitempty"`  // table
	Rows     [][][]InlineNode `json:"rows,omitempty"`     // static_table: строка -> колонка -> ячейка
}

// DocumentBlock — один блок документа (напр. "Общая информация", "Результаты
// измерения температуры") — своя видимость в каждом из 3 фиксированных видов
// вывода (2026-08-22: "ровно 3, простые галочки", по решению пользователя —
// НЕ расширяемый список именованных шаблонов) + опциональный привязанный
// график. Title — только для списка блоков в редакторе, не печатается в
// документе (в отличие от заголовков RichNode "heading", которые пользователь
// сам вставляет как содержимое, если нужен видимый заголовок раздела).
type DocumentBlock struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Content        []RichNode `json:"content"`
	ChartID        string     `json:"chart_id,omitempty"`
	ShowInUI       bool       `json:"show_in_ui"`
	ShowInExcerpt  bool       `json:"show_in_excerpt"`
	ShowInProtocol bool       `json:"show_in_protocol"`
}

// MethodPresentation — methods.presentation: блоки форматированного текста.
// Ровно 3 фиксированных вида вывода читают один и тот же набор блоков,
// отличаясь только фильтром ShowInUI/ShowInExcerpt/ShowInProtocol на блоке —
// состав "выписки" не зафиксирован программно, админ метода решает сам, что
// туда включить.
type MethodPresentation struct {
	Blocks []DocumentBlock `json:"blocks"`
}

// OperatorFormField — один вводимый испытателем показатель эксперимента.
// Только описание схемы (конструктор) — сам ввод данных лаборантом (мобильный/
// веб-фронт) пока не разрабатывается (2026-08-22).
type OperatorFormField struct {
	AttributeID string `json:"attribute_id"`
	Label       string `json:"label,omitempty"`
	Required    bool   `json:"required"`
	HelpText    string `json:"help_text,omitempty"`
}

// MethodOperatorForm — methods.operator_form: схема формы для испытателя.
type MethodOperatorForm struct {
	Fields []OperatorFormField `json:"fields"`
}

// ---- Вспомогательные ----

// loadMethodConfig читает конфиги метода.
func (s *Server) loadMethodConfig(ctx context.Context, methodID int64) (*MethodConfig, error) {
	var formulas, classification, charts, inputs, presentation, operatorForm []byte
	err := s.pool.QueryRow(ctx, `
SELECT formulas, classification, chart_configs, input_parameters, presentation, operator_form
FROM methods WHERE id = $1`, methodID).Scan(&formulas, &classification, &charts, &inputs, &presentation, &operatorForm)
	if err != nil {
		return nil, err
	}
	cfg := &MethodConfig{
		Formulas:       []map[string]any{},
		Classification: []map[string]any{},
		ChartConfigs:   []map[string]any{},
		InputParams:    []map[string]any{},
		Presentation:   parseMethodPresentation(presentation),
		OperatorForm:   parseMethodOperatorForm(operatorForm),
	}
	if len(formulas) > 0 && string(formulas) != "[]" {
		_ = json.Unmarshal(formulas, &cfg.Formulas)
	}
	if len(classification) > 0 && string(classification) != "[]" {
		_ = json.Unmarshal(classification, &cfg.Classification)
	}
	if len(charts) > 0 && string(charts) != "[]" {
		_ = json.Unmarshal(charts, &cfg.ChartConfigs)
	}
	if len(inputs) > 0 && string(inputs) != "[]" {
		_ = json.Unmarshal(inputs, &cfg.InputParams)
	}
	return cfg, nil
}

// legacyPresentationV1Field — плоский список полей блока 3 от 2026-08-21
// (до секций и до блоков).
type legacyPresentationV1Field struct {
	AttributeID    string `json:"attribute_id"`
	Label          string `json:"label,omitempty"`
	ShowInUI       bool   `json:"show_in_ui"`
	ShowInProtocol bool   `json:"show_in_protocol"`
}

// legacyPresentationV2Section — секции с полями/ролями от 2026-08-22 (жила
// меньше суток — заменена блочным редактором в тот же день, по прямому
// отказу пользователя от этого подхода).
type legacyPresentationV2Section struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Fields []struct {
		AttributeID    string `json:"attribute_id"`
		Label          string `json:"label,omitempty"`
		Role           string `json:"role"`
		ShowInUI       bool   `json:"show_in_ui"`
		ShowInExcerpt  bool   `json:"show_in_excerpt"`
		ShowInProtocol bool   `json:"show_in_protocol"`
	} `json:"fields"`
	Charts []struct {
		ChartID        string `json:"chart_id"`
		ShowInUI       bool   `json:"show_in_ui"`
		ShowInExcerpt  bool   `json:"show_in_excerpt"`
		ShowInProtocol bool   `json:"show_in_protocol"`
	} `json:"charts,omitempty"`
}

// parseMethodPresentation — единая точка разбора сырого JSONB methods.presentation,
// используется ВЕЗДЕ, где он читается (loadMethodConfig в этом файле,
// unmarshalPresentation/handleListMethods в references.go, sync.go pull-хендлер)
// — раньше это были 3 независимых места разбора, разошедшихся при первом же
// изменении формы (найдено при проектировании секций, 2026-08-22).
//
// Легаси-фолбэк — ДВА шага назад, без SQL-миграции данных:
//   - v1 (плоский `{"fields":[...]}`, 2026-08-21) → один блок "Показатели" с
//     одной таблицей из всех полей.
//   - v2 (`{"sections":[...]}` с role/3 галочками, 2026-08-22, жила меньше
//     суток) → один блок на секцию: table-узел из полей с role="table",
//     bullet_list из полей с role="summary" (каждый пункт — готовый плейсхолдер
//     с Agg="avg", т.к. у v2 не было понятия агрегации — лучшее приближение).
//     Видимость блока — OR по видимости его полей/графиков (v2 позволяла
//     разную видимость на каждом поле секции, блок такого не может — в v2
//     это была только что введённая форма, релевантных данных не потеряно).
func parseMethodPresentation(raw []byte) MethodPresentation {
	out := MethodPresentation{Blocks: []DocumentBlock{}}
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return out
	}
	var probe struct {
		Blocks   []DocumentBlock               `json:"blocks"`
		Sections []legacyPresentationV2Section `json:"sections"`
		Fields   []legacyPresentationV1Field   `json:"fields"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return out
	}
	if probe.Blocks != nil {
		out.Blocks = probe.Blocks
		return out
	}
	if probe.Sections != nil {
		out.Blocks = make([]DocumentBlock, 0, len(probe.Sections))
		for _, sec := range probe.Sections {
			var cols []TableColumn
			var summaryItems [][]InlineNode
			var showUI, showExcerpt, showProtocol bool
			for _, f := range sec.Fields {
				showUI = showUI || f.ShowInUI
				showExcerpt = showExcerpt || f.ShowInExcerpt
				showProtocol = showProtocol || f.ShowInProtocol
				if f.Role == "summary" {
					label := f.Label
					if label == "" {
						label = f.AttributeID
					}
					summaryItems = append(summaryItems, []InlineNode{
						{Type: "text", Text: label + ": "},
						{Type: "placeholder", Source: "attribute", AttributeID: f.AttributeID, Agg: "avg"},
					})
					continue
				}
				cols = append(cols, TableColumn{AttributeID: f.AttributeID, Label: f.Label})
			}
			var chartID string
			for _, c := range sec.Charts {
				showUI = showUI || c.ShowInUI
				showExcerpt = showExcerpt || c.ShowInExcerpt
				showProtocol = showProtocol || c.ShowInProtocol
				if chartID == "" {
					chartID = c.ChartID
				}
			}
			var content []RichNode
			if len(cols) > 0 {
				// раньше "Серия" рендерилась implicit-первой колонкой всегда —
				// сохраняем это поведение для легаси-данных явной колонкой.
				content = append(content, RichNode{Type: "table", Columns: append([]TableColumn{{Kind: "series_no"}}, cols...)})
			}
			if len(summaryItems) > 0 {
				content = append(content, RichNode{Type: "bullet_list", Items: summaryItems})
			}
			out.Blocks = append(out.Blocks, DocumentBlock{
				ID: sec.ID, Title: sec.Title, Content: content, ChartID: chartID,
				ShowInUI: showUI, ShowInExcerpt: showExcerpt, ShowInProtocol: showProtocol,
			})
		}
		return out
	}
	if len(probe.Fields) > 0 {
		cols := make([]TableColumn, 0, len(probe.Fields)+1)
		cols = append(cols, TableColumn{Kind: "series_no"})
		for _, f := range probe.Fields {
			cols = append(cols, TableColumn{AttributeID: f.AttributeID, Label: f.Label})
		}
		out.Blocks = []DocumentBlock{{
			ID:             "legacy",
			Title:          "Показатели",
			Content:        []RichNode{{Type: "table", Columns: cols}},
			ShowInUI:       true,
			ShowInExcerpt:  false,
			ShowInProtocol: true,
		}}
	}
	return out
}

// parseMethodOperatorForm — разбор сырого JSONB methods.operator_form.
func parseMethodOperatorForm(raw []byte) MethodOperatorForm {
	out := MethodOperatorForm{Fields: []OperatorFormField{}}
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out.Fields == nil {
		out.Fields = []OperatorFormField{}
	}
	return out
}

// loadSeriesValues собирает values всех серий (не статистических) заявки+метода.
func (s *Server) loadSeriesValues(ctx context.Context, requestID, methodID int64) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
SELECT values FROM measurement_results
WHERE request_id = $1 AND method_id = $2 AND is_statistical_row = false
ORDER BY series_num`, requestID, methodID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		m := map[string]any{}
		if len(raw) > 0 && string(raw) != "{}" {
			_ = json.Unmarshal(raw, &m)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// buildFormulaEnv строит среду для DSL: Params = values одной серии (или агрегата),
// SeriesParams = собранные по сериям значения для агрегаций, rankOrder =
// determinable_indicators метода (для min_grade/max_grade).
func buildFormulaEnv(seriesValues []map[string]any, current map[string]any, rankOrder []string) *FormulaEnv {
	params := map[string]any{}
	for k, v := range current {
		params[k] = v
	}
	seriesParams := map[string][]any{}
	keys := map[string]bool{}
	for _, sv := range seriesValues {
		for k := range sv {
			keys[k] = true
		}
	}
	for k := range keys {
		for _, sv := range seriesValues {
			if v, ok := sv[k]; ok {
				seriesParams[k] = append(seriesParams[k], v)
			}
		}
	}
	return &FormulaEnv{Params: params, SeriesParams: seriesParams, RankOrder: rankOrder}
}

// applyFormulas выполняет формулы уровня "series" (по умолчанию, если apply_level не
// задан) над values текущей серии; результат вписывается в values. Формулы уровня
// "aggregated" здесь пропускаются — см. applyAggregatedFormulas (считаются раз на
// заявку+метод, пишутся в aggregated_results, а не в values серии).
func (s *Server) applyFormulas(ctx context.Context, methodID int64, seriesValues []map[string]any, values map[string]any) error {
	cfg, err := s.loadMethodConfig(ctx, methodID)
	if err != nil {
		return err
	}
	rankOrder, err := s.loadMethodRankOrder(ctx, methodID)
	if err != nil {
		rankOrder = nil
	}
	env := buildFormulaEnv(seriesValues, values, rankOrder)
	for _, f := range cfg.Formulas {
		if lvl, _ := f["apply_level"].(string); lvl == "aggregated" {
			continue
		}
		expr, _ := f["expression"].(string)
		target, _ := f["target_parameter"].(string)
		if strings.TrimSpace(expr) == "" {
			continue
		}
		res, err := runFormula(expr, env)
		if err != nil {
			return err
		}
		if target != "" {
			values[target] = res
			env.Params[target] = res
		}
	}
	return nil
}

// applyAggregatedFormulas выполняет формулы уровня "aggregated" (например, итоговая
// оценка по всем сериям заявки+метода) И классификацию, чьи subjects читают/пишут
// aggregated-атрибуты (applyAggregatedClassification, 2026-08-23 — см. там; нужно
// делать это в ОДНОЙ функции/транзакции результата, чтобы classification видела
// уже посчитанные formula-aggregated значения того же прохода, напр.
// agg_flam_flow_density -> flammability_group у метода ГВ). Результат пишется в
// aggregated_results (delete+insert по (request_id, method_id,
// calculation_type='formula_aggregated'), та же схема, что recomputeStatistics
// использует для стат-строки).
func (s *Server) applyAggregatedFormulas(ctx context.Context, requestID, methodID int64) error {
	cfg, err := s.loadMethodConfig(ctx, methodID)
	if err != nil {
		return err
	}
	hasAggregatedFormula := false
	for _, f := range cfg.Formulas {
		if lvl, _ := f["apply_level"].(string); lvl == "aggregated" {
			hasAggregatedFormula = true
			break
		}
	}
	hasAggregatedClassification := false
	aggregatedIDs := aggregatedAttributeIDs(cfg)
ruleLoop:
	for _, rule := range cfg.Classification {
		subjects, _ := rule["subjects"].([]any)
		for _, sj := range subjects {
			subject, ok := sj.(map[string]any)
			if !ok {
				continue
			}
			outputID, _ := subject["output_attribute_id"].(string)
			if aggregatedIDs[outputID] {
				hasAggregatedClassification = true
				break ruleLoop
			}
		}
	}
	if !hasAggregatedFormula && !hasAggregatedClassification {
		return nil
	}
	seriesValues, err := s.loadSeriesValues(ctx, requestID, methodID)
	if err != nil {
		return err
	}
	if len(seriesValues) == 0 {
		return nil
	}
	rankOrder, err := s.loadMethodRankOrder(ctx, methodID)
	if err != nil {
		rankOrder = nil
	}
	env := buildFormulaEnv(seriesValues, map[string]any{}, rankOrder)
	result := map[string]any{}
	for _, f := range cfg.Formulas {
		lvl, _ := f["apply_level"].(string)
		if lvl != "aggregated" {
			continue
		}
		expr, _ := f["expression"].(string)
		target, _ := f["target_parameter"].(string)
		if strings.TrimSpace(expr) == "" || target == "" {
			continue
		}
		res, err := runFormula(expr, env)
		if err != nil {
			return err
		}
		result[target] = res
		env.Params[target] = res
	}
	if hasAggregatedClassification {
		if err := s.applyAggregatedClassification(ctx, requestID, methodID, cfg, result); err != nil {
			return err
		}
	}
	if len(result) == 0 {
		return nil
	}
	dataJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
DELETE FROM aggregated_results
WHERE request_id = $1 AND method_id = $2 AND calculation_type = 'formula_aggregated'`,
		requestID, methodID); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO aggregated_results (request_id, method_id, calculation_type, result_data,
	source_series_count, source_series_range, updated_at)
VALUES ($1, $2, 'formula_aggregated', $3::jsonb, $4, $5, now())`,
		requestID, methodID, string(dataJSON), len(seriesValues), "1-"+strconv.Itoa(len(seriesValues)))
	return err
}

// applyClassification выполняет правила классификации над values (по конфигу
// метода). 2026-08-22v3, по прямой правке пользователя: убрана агрегация между
// сериями (всегда берётся значение текущей записи как есть — свод по сериям
// оказался ненужной сложностью) И убрано упоминание конкретных атрибутов в
// самой схеме условий "если [ветка], то ...": ветки/clauses теперь сравнивают
// НЕЯВНЫЙ "текущий оцениваемый атрибут" (subjectValue) с правой частью
// (литерал/другой атрибут/целевой показатель) — своей для условия, но общей
// для ВСЕХ subjects правила. Правило = ОДНА схема условий (branches),
// применяемая по отдельности к каждой строке таблицы subjects — "оцениваемый
// атрибут" → "куда записать результат" (может быть несколько строк:
// пользователь явно описал это как обход двух параллельных списков).
// requestID нужен только для операндов kind="target_indicator".
func (s *Server) applyClassification(ctx context.Context, requestID, methodID int64, values map[string]any) error {
	cfg, err := s.loadMethodConfig(ctx, methodID)
	if err != nil {
		return err
	}
	cctx, err := s.newClassifyCtx(ctx, requestID, methodID, values)
	if err != nil {
		return err
	}
	aggregatedIDs := aggregatedAttributeIDs(cfg)
	for _, rule := range cfg.Classification {
		// wantAggregated=false: субъекты, чей output — обычный (experiment-level)
		// атрибут. Субъекты с aggregated-output здесь всегда пропускаются (не
		// найдут aggregated-only input, вроде agg_flam_flow_density, в per-series
		// values) — их считает applyAggregatedClassification.
		applyRuleToSubjects(cctx, rule, aggregatedIDs, false)
	}
	return nil
}

// newClassifyCtx собирает общий контекст резолвинга операндов — rankOrder и
// лениво загружаемый целевой показатель (kind="target_indicator") — общий код
// applyClassification/applyAggregatedClassification.
func (s *Server) newClassifyCtx(ctx context.Context, requestID, methodID int64, values map[string]any) (classifyCtx, error) {
	rankOrder, _ := s.loadMethodRankOrder(ctx, methodID)
	var targetGrade string
	targetLoaded := false
	loadTarget := func() (any, bool) {
		if !targetLoaded {
			targetLoaded = true
			targetGrade, _ = s.loadTargetIndicator(ctx, requestID, methodID)
		}
		if targetGrade == "" {
			return nil, false
		}
		return targetGrade, true
	}
	return classifyCtx{values: values, rankOrder: rankOrder, loadTarget: loadTarget}, nil
}

// aggregatedAttributeIDs — id атрибутов метода уровня "aggregated" (одно
// значение на заявку+метод, а не на серию) — используется, чтобы отличить
// subjects классификации, которые пишут/читают агрегированные значения
// (agg_flam_flow_density -> flammability_group), от обычных per-series.
func aggregatedAttributeIDs(cfg *MethodConfig) map[string]bool {
	out := map[string]bool{}
	for id, a := range methodAttributesByID(cfg) {
		if a.Level == "aggregated" {
			out[id] = true
		}
	}
	return out
}

// applyAggregatedClassification — аналог applyClassification, но для subjects,
// чей output_attribute_id — атрибут уровня "aggregated" (2026-08-23; найдено на
// методе ГВ: flammability_group читает agg_flam_flow_density — обе aggregated,
// но applyClassification вызывается только с per-series values, где
// agg_flam_flow_density никогда не появляется, поэтому такой subject раньше
// НИКОГДА не совпадал). aggValues — уже посчitанные формулы уровня "aggregated"
// (см. applyAggregatedFormulas) — мутируется на месте, чтобы результат
// классификации попал в тот же aggregated_results, откуда resolvePlaceholder
// (protocol.go) читает aggregated-плейсхолдеры.
func (s *Server) applyAggregatedClassification(ctx context.Context, requestID, methodID int64, cfg *MethodConfig, aggValues map[string]any) error {
	cctx, err := s.newClassifyCtx(ctx, requestID, methodID, aggValues)
	if err != nil {
		return err
	}
	aggregatedIDs := aggregatedAttributeIDs(cfg)
	for _, rule := range cfg.Classification {
		applyRuleToSubjects(cctx, rule, aggregatedIDs, true)
	}
	return nil
}

// applyRuleToSubjects применяет ОДНУ схему условий (rule["branches"]) по
// отдельности к каждой строке rule["subjects"] — "оцениваемый атрибут"
// (input_attribute_id) → "куда записать результат" (output_attribute_id).
// Строка без значения input_attribute_id в values (атрибут не заполнен) —
// пропускается, а не ошибка; так же, как ветка без совпадения ничего не пишет.
// aggregatedIDs/wantAggregated (2026-08-23) — subject обрабатывается только если
// его output — атрибут уровня "aggregated" (wantAggregated=true, вызов из
// applyAggregatedClassification) или обычный (wantAggregated=false, обычный
// applyClassification); иначе subject пропускается этим проходом (его обработает
// другой проход с подходящим values).
func applyRuleToSubjects(ctx classifyCtx, rule map[string]any, aggregatedIDs map[string]bool, wantAggregated bool) {
	branches, _ := rule["branches"].([]any)
	subjects, _ := rule["subjects"].([]any)
	for _, s := range subjects {
		subject, ok := s.(map[string]any)
		if !ok {
			continue
		}
		inputID, _ := subject["input_attribute_id"].(string)
		outputID, _ := subject["output_attribute_id"].(string)
		if inputID == "" || outputID == "" {
			continue
		}
		if aggregatedIDs[outputID] != wantAggregated {
			continue
		}
		subjectValue, found := ctx.values[inputID]
		if !found {
			continue
		}
		if grade, matched := evaluateBranches(ctx, branches, subjectValue); matched {
			ctx.values[outputID] = grade
		}
	}
}

// evaluateBranches перебирает branches ПО ПОРЯДКУ (без пересортировки): первая
// совпавшая — grade. Ветка без clauses — безусловное совпадение (см. evalBranch).
func evaluateBranches(ctx classifyCtx, branches []any, subjectValue any) (string, bool) {
	for _, b := range branches {
		branch, ok := b.(map[string]any)
		if !ok {
			continue
		}
		grade, _ := branch["grade"].(string)
		if grade == "" {
			continue
		}
		if evalBranch(ctx, branch, subjectValue) {
			return grade, true
		}
	}
	return "", false
}

// classifyCtx — общий контекст резолвинга операндов при оценке одного правила.
type classifyCtx struct {
	values     map[string]any
	rankOrder  []string
	loadTarget func() (any, bool)
}

// resolveOperand — операнд правой части атомарного сравнения (см. Operand на
// фронтенде): литерал, другой атрибут текущей записи (без агрегации по
// сериям — берётся как есть) или целевой показатель заявки.
func (ctx classifyCtx) resolveOperand(op map[string]any) (any, bool) {
	switch kind, _ := op["kind"].(string); kind {
	case "literal":
		v := op["value"]
		return v, v != nil
	case "attribute":
		id, _ := op["id"].(string)
		if id == "" {
			return nil, false
		}
		v, ok := ctx.values[id]
		return v, ok
	case "target_indicator":
		return ctx.loadTarget()
	}
	return nil, false
}

// evalClause — один атомарный тест "[оцениваемый атрибут] [знак] [сравнить с]".
// Левая часть — НЕЯВНАЯ (subjectValue, общий текущий предмет оценки для всех
// clauses/branches правила — см. applyRuleToSubjects); в самой схеме условий
// конкретный атрибут не упоминается. Если правая часть не резолвится (атрибут
// не заполнен, целевой показатель не задан) — clause считается невыполненным,
// а не ошибкой.
func evalClause(ctx classifyCtx, clause map[string]any, subjectValue any) bool {
	op, _ := clause["operator"].(string)
	compareToOp, _ := clause["compare_to"].(map[string]any)
	compareTo, ok := ctx.resolveOperand(compareToOp)
	if !ok {
		return false
	}
	return classificationCompare(op, subjectValue, compareTo, ctx.rankOrder)
}

// evalBranch — ветка правила: пустой/отсутствующий clauses — безусловная ветка
// ("Иначе", без явного "Если" — всегда верна); иначе — clauses объединяются через
// join ("and" по умолчанию, соответствует базовому Excel AND()/OR() без вложенных
// групп — умышленное упрощение, см. ClassificationBranch на фронтенде).
func evalBranch(ctx classifyCtx, branch map[string]any, subjectValue any) bool {
	clauses, _ := branch["clauses"].([]any)
	if len(clauses) == 0 {
		return true
	}
	or := branch["join"] == "or"
	for _, c := range clauses {
		clause, ok := c.(map[string]any)
		if !ok {
			continue
		}
		matched := evalClause(ctx, clause, subjectValue)
		if or && matched {
			return true
		}
		if !or && !matched {
			return false
		}
	}
	return !or
}

// classificationCompare — сравнение для единой модели. "=="/"!=" — как раньше,
// строковое через JSON (evalBooleanCond, годится для чисел/Да-Нет/показателей
// одинаково). "<"/"<="/">"/">=" — если ОБЕ стороны найдены в rankOrder (оба —
// показатели метода determinable_indicators) — сравнение ПО ИНДЕКСУ, но в обратную
// сторону: первый введённый показатель считается "большим" (по явному подтверждению
// пользователя — "если ввёл Г1,Г2,Г3,Г4, это Г1>Г2>Г3>Г4"), у него МЕНЬШИЙ индекс,
// поэтому `A > B` эквивалентно `index(A) < index(B)` — inverted operator на индексах.
// Это тот же порядок, что уже подтверждён по legacy для compliance (valueIdx <=
// targetIdx → "не ниже цели"), выражается здесь как `A >= B` (achieved >= target).
// Если хотя бы одна сторона не найдена в rankOrder — обычное числовое сравнение
// (evalBooleanCond уже это умеет, тот же путь, что у порогового правила раньше).
func classificationCompare(op string, left, right any, rankOrder []string) bool {
	if op == "==" || op == "!=" {
		return evalBooleanCond(op, left, right)
	}
	if leftStr, ok1 := left.(string); ok1 {
		if rightStr, ok2 := right.(string); ok2 {
			li, ri := indexOfString(rankOrder, leftStr), indexOfString(rankOrder, rightStr)
			if li >= 0 && ri >= 0 {
				return evalBooleanCond(invertCompareOp(op), float64(li), float64(ri))
			}
		}
	}
	return evalBooleanCond(op, left, right)
}

func invertCompareOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

func indexOfString(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// loadMethodRankOrder возвращает determinable_indicators метода — то же самое, что в
// legacy classification_ranks: массив показателей по убыванию, первый — максимальный/
// больше остальных (проверено на реальных данных, см. AGENTS.md).
func (s *Server) loadMethodRankOrder(ctx context.Context, methodID int64) ([]string, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT determinable_indicators FROM methods WHERE id = $1`, methodID).Scan(&raw); err != nil {
		return nil, err
	}
	out := []string{}
	if len(raw) > 0 && string(raw) != "[]" {
		_ = json.Unmarshal(raw, &out)
	}
	return out, nil
}

// loadTargetIndicator возвращает целевой показатель заявки для метода —
// objects.characteristics.target_indicators[methodID] объекта, привязанного к заявке
// (поле «Целевой показатель», sbe-requests). Пустая строка, если объект/показатель не
// заданы — вызывающий код трактует это как "не оценивается".
func (s *Server) loadTargetIndicator(ctx context.Context, requestID, methodID int64) (string, error) {
	var target *string
	err := s.pool.QueryRow(ctx, `
SELECT o.characteristics->'target_indicators'->>$2
FROM requests r JOIN objects o ON o.id = r.object_id
WHERE r.id = $1`, requestID, strconv.FormatInt(methodID, 10)).Scan(&target)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if target == nil {
		return "", nil
	}
	return *target, nil
}

func evalBooleanCond(op string, a, b any) bool {
	switch op {
	case "==":
		return fmtAny(a) == fmtAny(b)
	case "!=":
		return fmtAny(a) != fmtAny(b)
	case "<":
		af, err1 := toFloat(a)
		bf, err2 := toFloat(b)
		if err1 == nil && err2 == nil {
			return af < bf
		}
		return false
	case "<=":
		af, err1 := toFloat(a)
		bf, err2 := toFloat(b)
		if err1 == nil && err2 == nil {
			return af <= bf
		}
		return false
	case ">":
		af, err1 := toFloat(a)
		bf, err2 := toFloat(b)
		if err1 == nil && err2 == nil {
			return af > bf
		}
		return false
	case ">=":
		af, err1 := toFloat(a)
		bf, err2 := toFloat(b)
		if err1 == nil && err2 == nil {
			return af >= bf
		}
		return false
	}
	return false
}

func fmtAny(v any) string {
	return strings.TrimSpace(strings.ToLower(strings.TrimSpace(strings.TrimSpace(fmtS(v)))))
}

func fmtS(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- Статистика ----

// computeStats считает статистики по числовым параметрам серий. Возвращает map параметр -> среднее.
func computeStats(series []map[string]any) map[string]any {
	out := map[string]any{}
	// собрать числовые параметры
	keys := map[string][]float64{}
	for _, sv := range series {
		for k, v := range sv {
			if f, err := toFloat(v); err == nil {
				keys[k] = append(keys[k], f)
			}
		}
	}
	for k, vals := range keys {
		if len(vals) == 0 {
			continue
		}
		s := 0.0
		for _, v := range vals {
			s += v
		}
		out[k+"_avg"] = round2(s / float64(len(vals)))
		out[k+"_count"] = float64(len(vals))
	}
	return out
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

// ---- Handlers ----

// handleListResults возвращает все результаты (серии) заявки.
func (s *Server) handleListResults(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(r.Context(), currentEmail(r), id); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
SELECT id, request_id, method_id, COALESCE(inventor_id, 0), series_num, values,
	file_links, photo_before, photo_after, is_statistical_row, calculation_type,
	source_series_count, source_series_range, created_at, updated_at
FROM measurement_results WHERE request_id = $1 ORDER BY series_num, id`, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	res := make([]MeasurementResult, 0, 16)
	for rows.Next() {
		var m MeasurementResult
		var valsRaw, linksRaw []byte
		var ca, ua time.Time
		if err := rows.Scan(&m.ID, &m.RequestID, &m.MethodID, &m.InventorID, &m.SeriesNum,
			&valsRaw, &linksRaw, &m.PhotoBefore, &m.PhotoAfter, &m.IsStatisticalRow,
			&m.CalculationType, &m.SourceSeriesCount, &m.SourceSeriesRange, &ca, &ua); err != nil {
			log.Printf("results scan: %v", err)
			continue
		}
		m.Values = map[string]any{}
		if len(valsRaw) > 0 && string(valsRaw) != "{}" {
			_ = json.Unmarshal(valsRaw, &m.Values)
		}
		m.FileLinks = map[string]any{}
		if len(linksRaw) > 0 && string(linksRaw) != "{}" {
			_ = json.Unmarshal(linksRaw, &m.FileLinks)
		}
		m.CreatedAt = ca.Format(time.RFC3339)
		m.UpdatedAt = ua.Format(time.RFC3339)
		res = append(res, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// handleCreateResult создаёт/обновляет серию и запускает авто-расчёт.
func (s *Server) handleCreateResult(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabAccess(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	var req struct {
		MethodID    int64          `json:"method_id"`
		InventorID  int64          `json:"inventor_id"`
		SeriesNum   int            `json:"series_num"`
		Values      map[string]any `json:"values"`
		PhotoBefore string         `json:"photo_before"`
		PhotoAfter  string         `json:"photo_after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.MethodID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "method_id is required"})
		return
	}
	if req.Values == nil {
		req.Values = map[string]any{}
	}

	id, seriesNum, err := s.saveResultSeries(r.Context(), requestID, req.MethodID, req.InventorID,
		req.SeriesNum, req.Values, req.PhotoBefore, req.PhotoAfter)
	if err != nil {
		var fe *formulaApplyError
		if errors.As(err, &fe) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fe.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "series_num": seriesNum, "values": req.Values})
}

// formulaApplyError оборачивает ошибку DSL-формулы, чтобы HTTP-хендлер мог отличить
// её (400, ошибка входных данных) от прочих ошибок сохранения (500, db error).
type formulaApplyError struct{ err error }

func (e *formulaApplyError) Error() string { return "formula: " + e.err.Error() }
func (e *formulaApplyError) Unwrap() error { return e.err }

// saveResultSeries сохраняет серию измерений (upsert по request_id+method_id+series_num),
// применяет формулы/классификацию (мутирует values), пересчитывает статистику и формулы
// уровня "aggregated". Переиспользуется HTTP-хендлером (handleCreateResult) и
// email-ingestion воркером (email_ingest.go, applyResultPayload) — без HTTP-обёртки.
// seriesNum <= 0 — авто-выбор следующего свободного номера серии.
func (s *Server) saveResultSeries(ctx context.Context, requestID, methodID, inventorID int64,
	seriesNum int, values map[string]any, photoBefore, photoAfter string) (int64, int, error) {
	var err error
	if seriesNum <= 0 {
		seriesNum, err = s.nextSeriesNum(ctx, requestID, methodID)
		if err != nil {
			return 0, 0, err
		}
	}

	// собрать все серии (для формул/статистики) с уже сохранёнными
	allSeries, err := s.loadSeriesValues(ctx, requestID, methodID)
	if err != nil {
		return 0, 0, err
	}
	allSeries = append(allSeries, values)

	// формулы + классификация
	if err := s.applyFormulas(ctx, methodID, allSeries, values); err != nil {
		return 0, 0, &formulaApplyError{err}
	}
	if err := s.applyClassification(ctx, requestID, methodID, values); err != nil {
		return 0, 0, err
	}

	valsJSON, err := json.Marshal(values)
	if err != nil {
		return 0, 0, err
	}

	// upsert серии
	var id int64
	err = s.pool.QueryRow(ctx, `
INSERT INTO measurement_results (request_id, method_id, inventor_id, series_num, values,
	photo_before, photo_after, updated_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, now())
ON CONFLICT (request_id, method_id, series_num) DO UPDATE SET
	values = EXCLUDED.values, inventor_id = EXCLUDED.inventor_id,
	photo_before = EXCLUDED.photo_before, photo_after = EXCLUDED.photo_after, updated_at = now()
RETURNING id`,
		requestID, methodID, nullableID(inventorID), seriesNum, string(valsJSON),
		photoBefore, photoAfter).Scan(&id)
	if err != nil {
		log.Printf("save result series: %v", err)
		return 0, 0, err
	}

	// статистика: пересчитать стат-строку
	if err := s.recomputeStatistics(ctx, requestID, methodID); err != nil {
		log.Printf("statistics: %v", err)
	}
	// формулы уровня "aggregated" — пересчитать по всем сериям заявки+метода
	if err := s.applyAggregatedFormulas(ctx, requestID, methodID); err != nil {
		log.Printf("aggregated formulas: %v", err)
	}

	return id, seriesNum, nil
}

// nextSeriesNum — следующий свободный номер РЕАЛЬНОЙ серии (is_statistical_row
// исключены, 2026-08-24): иначе стат-строка (см. recomputeStatistics) временно
// занимает "следующий" слот, следующая настоящая серия получает номер ЕЩЁ
// дальше, и при третьей+ серии recomputeStatistics повторно пытается занять
// тот же слот, что уже забрала настоящая серия — "duplicate key value violates
// unique constraint uq_meas_req_method_series" (обнаружено на реальном письме,
// request 1352/671 — 3 письма-результата подряд).
func (s *Server) nextSeriesNum(ctx context.Context, requestID, methodID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(MAX(series_num), 0) + 1 FROM measurement_results
WHERE request_id = $1 AND method_id = $2 AND is_statistical_row = false`, requestID, methodID).Scan(&n)
	return n, err
}

// recomputeStatistics пересчитывает и пишет стат-строку заявки+метода.
func (s *Server) recomputeStatistics(ctx context.Context, requestID, methodID int64) error {
	series, err := s.loadSeriesValues(ctx, requestID, methodID)
	if err != nil {
		return err
	}
	if len(series) == 0 {
		return nil
	}
	stats := computeStats(series)
	if len(stats) == 0 {
		return nil
	}
	statsJSON, _ := json.Marshal(stats)
	// удалить старую стат-строку и вставить новую
	if _, err := s.pool.Exec(ctx, `
DELETE FROM measurement_results WHERE request_id = $1 AND method_id = $2 AND is_statistical_row = true`,
		requestID, methodID); err != nil {
		return err
	}
	// nextSeriesNum (не len(series)+1) — гарантированно свободный слот ПОСЛЕ
	// максимального номера настоящей серии, даже если она не начинается с 1
	// или имеет разрывы (2026-08-24, см. комментарий у nextSeriesNum).
	seriesNum, err := s.nextSeriesNum(ctx, requestID, methodID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO measurement_results (request_id, method_id, series_num, values, is_statistical_row,
	calculation_type, source_series_count, source_series_range)
VALUES ($1, $2, $3, $4::jsonb, true, 'auto_statistics', $5, $6)`,
		requestID, methodID, seriesNum, string(statsJSON), len(series), "1-"+strconv.Itoa(len(series)))
	return err
}

// handleListAggregated возвращает агрегаты заявки.
func (s *Server) handleListAggregated(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
SELECT id, request_id, method_id, calculation_type, result_data, source_series_count,
	source_series_range, created_at, updated_at
FROM aggregated_results WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	res := make([]AggregatedResult, 0, 8)
	for rows.Next() {
		var a AggregatedResult
		var dataRaw []byte
		var ca, ua time.Time
		if err := rows.Scan(&a.ID, &a.RequestID, &a.MethodID, &a.CalculationType,
			&dataRaw, &a.SourceSeriesCount, &a.SourceSeriesRange, &ca, &ua); err != nil {
			continue
		}
		a.ResultData = map[string]any{}
		if len(dataRaw) > 0 && string(dataRaw) != "{}" {
			_ = json.Unmarshal(dataRaw, &a.ResultData)
		}
		a.CreatedAt = ca.Format(time.RFC3339)
		a.UpdatedAt = ua.Format(time.RFC3339)
		res = append(res, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"aggregated": res})
}

// handleCalculateSeries пересчитывает формулы/классификацию для всех серий заявки.
func (s *Server) handleCalculateSeries(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabAccess(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	seriesNum, err := strconv.Atoi(r.PathValue("series"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid series"})
		return
	}
	// найти метод по серии
	var methodID int64
	err = s.pool.QueryRow(r.Context(), `
SELECT method_id FROM measurement_results WHERE request_id = $1 AND series_num = $2`,
		requestID, seriesNum).Scan(&methodID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "series not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	// пересчитать все серии: для каждой серии применить формулы заново
	rows, err := s.pool.Query(r.Context(), `
SELECT id, series_num, values FROM measurement_results
WHERE request_id = $1 AND method_id = $2 AND is_statistical_row = false ORDER BY series_num`,
		requestID, methodID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	allValues := []map[string]any{}
	type rowT struct {
		id        int64
		seriesNum int
		values    map[string]any
	}
	var rowsList []rowT
	for rows.Next() {
		var rw rowT
		var raw []byte
		if err := rows.Scan(&rw.id, &rw.seriesNum, &raw); err != nil {
			continue
		}
		rw.values = map[string]any{}
		if len(raw) > 0 && string(raw) != "{}" {
			_ = json.Unmarshal(raw, &rw.values)
		}
		rowsList = append(rowsList, rw)
		allValues = append(allValues, rw.values)
	}
	for _, rw := range rowsList {
		if err := s.applyFormulas(r.Context(), methodID, allValues, rw.values); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "formula: " + err.Error()})
			return
		}
		if err := s.applyClassification(r.Context(), requestID, methodID, rw.values); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		j, _ := json.Marshal(rw.values)
		if _, err := s.pool.Exec(r.Context(), `
UPDATE measurement_results SET values = $2::jsonb, updated_at = now() WHERE id = $1`,
			rw.id, string(j)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}
	// статистика
	if err := s.recomputeStatistics(r.Context(), requestID, methodID); err != nil {
		log.Printf("statistics: %v", err)
	}
	// формулы уровня "aggregated" — пересчитать по всем сериям заявки+метода
	if err := s.applyAggregatedFormulas(r.Context(), requestID, methodID); err != nil {
		log.Printf("aggregated formulas: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
