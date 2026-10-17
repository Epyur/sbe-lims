package main

import (
	"context"
	"encoding/json"
)

// ---- Сбор контекста отчёта и статистика по аналогичным объектам (2026-10-16) ----
//
// Отчёт по испытанию (см. report.go) строится по данным заявки/метода/результатов
// (те же загрузчики, что у протокола — не дублируются запросы) плюс «приложение»:
// статистика по результатам испытаний АНАЛОГИЧНЫХ объектов. Аналогичный объект —
// заявка с ТЕМ ЖЕ ЕКН объекта и ТЕМ ЖЕ методом (согласовано 2026-10-16).

// indicatorStat — свод по одному показателю метода по всем аналогам.
type indicatorStat struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Count        int            `json:"count"`
	HasNumeric   bool           `json:"has_numeric"`
	Avg          float64        `json:"avg"`
	Min          float64        `json:"min"`
	Max          float64        `json:"max"`
	Distribution map[string]int `json:"distribution,omitempty"`

	// Разрез по толщинам (2026-10-17): все толщины одной линейки — один материал,
	// но различие толщин показывается отдельно (см. analogues.go, groupByThickness).
	ByThickness []thicknessBucket `json:"by_thickness,omitempty"`
	Trend       *trendNote        `json:"trend,omitempty"`
}

// analogueRow — одна строка таблицы «Аналоги» (испытание аналогичного объекта).
type analogueRow struct {
	RequestID    int64  `json:"request_id"`
	Number       string `json:"number"`
	Object       string `json:"object"`
	Batch        string `json:"batch"`
	CompletedAt  string `json:"completed_at"`
	Result       string `json:"result"`
	Compliance   string `json:"compliance"`
	Tier         int    `json:"tier"`                    // 1 — тот же материал (с ЕКН), 2 — по названию, 3 — модификация
	Line         string `json:"line"`                    // линейка материала
	Modifier     string `json:"modifier"`                // модификатор (fr/pro/arctic/middle/…)
	Thickness    string `json:"thickness"`               // "1,5" или "" (не определена)
	MaterialNote string `json:"material_note,omitempty"` // пояснение, почему попал в ступень
}

// reportContext — всё, что нужно для отчёта: данные заявки/метода/результатов +
// статистика по аналогам. Собирается loadReportContext.
type reportContext struct {
	Request         *Request
	MethodID        int64
	MethodName      string
	Instruction     string
	ObjectName      string
	ObjectChars     map[string]any
	EKN             string
	TargetIndicator string
	Series          []map[string]any
	Stats           map[string]any
	Agg             map[string]any
	IndicatorStats  []indicatorStat
	Analogues       []analogueRow

	// Материал текущей заявки (2026-10-17): по какому ключу собраны аналоги —
	// выводится в шапке приложения, чтобы было видно, что попало в статистику.
	Material  MaterialKey
	Thickness float64 // толщина текущей заявки, мм (0 — не определена)
}

const maxAnalogues = 300

// loadReportContext собирает контекст отчёта по заявке.
func (s *Server) loadReportContext(ctx context.Context, requestID int64) (*reportContext, error) {
	req, err := s.loadRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	rc := &reportContext{
		Request:  req,
		MethodID: req.MethodID,
		EKN:      req.EKN,
		Stats:    map[string]any{},
		Agg:      map[string]any{},
	}
	_ = s.pool.QueryRow(ctx, `SELECT name FROM methods WHERE id = $1`, req.MethodID).Scan(&rc.MethodName)

	cfg, err := s.loadMethodConfig(ctx, req.MethodID)
	if err != nil || cfg == nil {
		cfg = &MethodConfig{}
	}
	rc.Instruction = cfg.ReportInstruction

	if req.ObjectID > 0 {
		var charsRaw []byte
		_ = s.pool.QueryRow(ctx,
			`SELECT name, characteristics FROM objects WHERE id = $1`, req.ObjectID).
			Scan(&rc.ObjectName, &charsRaw)
		rc.ObjectChars = map[string]any{}
		if len(charsRaw) > 0 && string(charsRaw) != "{}" {
			_ = json.Unmarshal(charsRaw, &rc.ObjectChars)
		}
	}
	rc.TargetIndicator, _ = s.loadTargetIndicator(ctx, requestID, req.MethodID)

	if seriesRows, serr := s.loadSeriesValuesWithSeriesNum(ctx, requestID, req.MethodID); serr == nil {
		for _, row := range seriesRows {
			rc.Series = append(rc.Series, row.Values)
		}
	}
	if st, e := s.loadStatsRow(ctx, requestID, req.MethodID); e == nil && st != nil {
		rc.Stats = st
	}
	if ag, e := s.loadAggregatedRow(ctx, requestID, req.MethodID); e == nil && ag != nil {
		rc.Agg = ag
	}

	// Материал текущей заявки — по нему отобраны аналоги (см. analogues.go).
	rc.Material, rc.Thickness, _ = s.resolveRequestMaterial(ctx, req)

	analogues, ids := s.loadAnalogueRequests(ctx, req)
	seriesByReq := s.loadSeriesBatch(ctx, ids, req.MethodID)
	aggByReq := s.loadAggBatch(ctx, ids, req.MethodID)

	rc.IndicatorStats = computeIndicatorStats(orderedMethodAttributes(cfg), analogues, seriesByReq, aggByReq)

	resultAttr, complianceAttr := resolveResultCompliance(cfg)
	rc.Analogues = buildAnalogues(analogues, aggByReq, resultAttr, complianceAttr)
	return rc, nil
}

// loadAnalogueRequests — заявки-аналоги текущей: материал определяется ключом
// (тип, линейка, модификатор), а не равенством ЕКН (см. analogues.go и
// material_key.go). Возвращает строки для таблицы и список id для батч-загрузки;
// текущая заявка исключена. Ступени не смешиваются — каждая строка помечена Tier.
func (s *Server) loadAnalogueRequests(ctx context.Context, req *Request) ([]analogueRow, []int64) {
	out := []analogueRow{}
	ids := []int64{}
	current, _, _ := s.resolveRequestMaterial(ctx, req)
	if !MaterialKeyFilled(current) {
		return out, ids
	}
	candidates := s.loadAnalogueCandidates(ctx, req.MethodID, req.ID)
	out = s.classifyCandidates(ctx, current, candidates)
	for _, a := range out {
		ids = append(ids, a.RequestID)
	}
	return out, ids
}

// loadSeriesBatch — values всех настоящих серий аналогов (без стат-строк), одним запросом.
func (s *Server) loadSeriesBatch(ctx context.Context, ids []int64, methodID int64) map[int64][]map[string]any {
	out := map[int64][]map[string]any{}
	if len(ids) == 0 {
		return out
	}
	rows, err := s.pool.Query(ctx, `
SELECT request_id, values FROM measurement_results
WHERE request_id = ANY($1) AND method_id = $2 AND is_statistical_row = false`, ids, methodID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		var raw []byte
		if err := rows.Scan(&rid, &raw); err != nil {
			continue
		}
		vals := map[string]any{}
		if len(raw) > 0 && string(raw) != "{}" {
			_ = json.Unmarshal(raw, &vals)
		}
		out[rid] = append(out[rid], vals)
	}
	return out
}

// loadAggBatch — агрегированные значения (одно на заявку+метод) аналогов, одним запросом.
func (s *Server) loadAggBatch(ctx context.Context, ids []int64, methodID int64) map[int64]map[string]any {
	out := map[int64]map[string]any{}
	if len(ids) == 0 {
		return out
	}
	rows, err := s.pool.Query(ctx, `
SELECT request_id, result_data FROM aggregated_results
WHERE request_id = ANY($1) AND method_id = $2`, ids, methodID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		var raw []byte
		if err := rows.Scan(&rid, &raw); err != nil {
			continue
		}
		vals := map[string]any{}
		if len(raw) > 0 && string(raw) != "{}" {
			_ = json.Unmarshal(raw, &vals)
		}
		out[rid] = vals
	}
	return out
}

// orderedMethodAttributes — все показатели метода в порядке input_parameters,
// плюс выходы классификации, которых нет среди атрибутов (на случай расхождения).
func orderedMethodAttributes(cfg *MethodConfig) []MethodAttribute {
	byID := methodAttributesByID(cfg)
	out := []MethodAttribute{}
	seen := map[string]bool{}
	for _, raw := range cfg.InputParams {
		id, _ := raw["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if a, ok := byID[id]; ok {
			out = append(out, a)
		}
	}
	for _, rule := range cfg.Classification {
		subs, _ := rule["subjects"].([]any)
		for _, sj := range subs {
			m, _ := sj.(map[string]any)
			id, _ := m["output_attribute_id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			if a, ok := byID[id]; ok {
				out = append(out, a)
			} else {
				out = append(out, MethodAttribute{ID: id, Name: id})
			}
		}
	}
	return out
}

// computeIndicatorStats — ЧИСТАЯ функция (тестируется без БД): по каждому
// показателю метода собирает значения по аналогам (experiment-уровень — все
// серии всех аналогов; aggregated-уровень — по одному значению на аналог) и
// считает count/avg/min/max, а для нечисловых значений — распределение.
// Для ступени 1 дополнительно строится разрез по толщинам и вывод о тренде
// (см. analogues.go): ступени 2/3 в тренд не попадают (решение пользователя).
func computeIndicatorStats(attrs []MethodAttribute, analogues []analogueRow, seriesByReq map[int64][]map[string]any, aggByReq map[int64]map[string]any) []indicatorStat {
	// Индекс аналогов по заявке — нужен для раскладки значений по толщинам.
	rowsByReq := make(map[int64]analogueRow, len(analogues))
	for _, a := range analogues {
		rowsByReq[a.RequestID] = a
	}

	out := make([]indicatorStat, 0, len(attrs))
	for _, a := range attrs {
		if a.ID == "" {
			continue
		}
		var vals []any
		var split []analogueValue
		if a.Level == "aggregated" {
			for rid, agg := range aggByReq {
				if v, ok := agg[a.ID]; ok && v != nil && fmtVal(v) != "" {
					vals = append(vals, v)
					if row, ok := rowsByReq[rid]; ok && row.Tier == analogueTierSameEKN {
						split = append(split, analogueValue{row: row, value: v})
					}
				}
			}
		} else {
			for rid, series := range seriesByReq {
				row, hasRow := rowsByReq[rid]
				for _, s := range series {
					if v, ok := s[a.ID]; ok && v != nil && fmtVal(v) != "" {
						vals = append(vals, v)
						if hasRow && row.Tier == analogueTierSameEKN {
							split = append(split, analogueValue{row: row, value: v})
						}
					}
				}
			}
		}
		if len(vals) == 0 {
			continue
		}
		st := indicatorStat{ID: a.ID, Name: a.Name, Count: len(vals), Distribution: map[string]int{}}
		var nums []float64
		for _, v := range vals {
			if f, ok := toFloatOK(v); ok {
				nums = append(nums, f)
			} else {
				st.Distribution[fmtVal(v)]++
			}
		}
		if len(nums) > 0 {
			st.HasNumeric = true
			sum := 0.0
			mn, mx := nums[0], nums[0]
			for _, f := range nums {
				sum += f
				if f < mn {
					mn = f
				}
				if f > mx {
					mx = f
				}
			}
			st.Avg = sum / float64(len(nums))
			st.Min = mn
			st.Max = mx
		}
		// Разрез по толщинам — только по ступени 1 (тот же материал + ЕКН).
		if len(split) > 0 {
			st.ByThickness = groupByThickness(split)
			st.Trend = computeTrend(a.ID, st.ByThickness)
		}
		// Распределение ценно для оценок/классификаций (мало различимых значений);
		// для свободного текста с сотнями уникальных — не раздуваем документ.
		if len(st.Distribution) > 50 {
			st.Distribution = nil
		}
		out = append(out, st)
	}
	return out
}

// buildAnalogues проставляет каждой строке таблицы «Аналоги» её результат и
// соответствие (из агрегированных значений методом resolveResultCompliance —
// та же пара «Результат/Соответствие», что показывает веб-портал).
func buildAnalogues(rows []analogueRow, aggByReq map[int64]map[string]any, resultAttr, complianceAttr string) []analogueRow {
	for i := range rows {
		agg := aggByReq[rows[i].RequestID]
		if resultAttr != "" {
			rows[i].Result = fmtVal(agg[resultAttr])
		}
		if complianceAttr != "" {
			rows[i].Compliance = fmtVal(agg[complianceAttr])
		}
	}
	return rows
}
