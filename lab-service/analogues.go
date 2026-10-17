package main

import (
	"context"
	"log"
	"strconv"
	"strings"
)

// ---- Отбор аналогичных объектов по материалу (2026-10-17) ----
//
// Было: аналоги = точное равенство ЕКН (ЕКН — артикул фасовки, см. material_key.go),
// из-за чего статистика собиралась внутри одной упаковки, а у 613 заявок из 682
// ЕКН не заполнен вовсе.
//
// Стало: материал определяется ключом (тип, линейка, модификатор). Толщина в ключ
// не входит — все толщины линейки анализируются вместе, а различие толщин
// показывается в приложении отчёта (см. report_context.go, разрез по толщинам).
// Решения пользователя 2026-10-17:
//   - ступень 1 — тот же материал, у аналога есть ЕКН;
//   - ступень 2 — тот же материал, ЕКН не указан (материал определён по названию);
//   - ступень 3 — та же линейка, другой модификатор (FR / Pro / Arctic / Middle /
//     Полы ФЛ/ФЛ). Ступени в статистике НЕ смешиваются.

// Ступени аналогичности.
const (
	analogueTierSameEKN   = 1
	analogueTierSameName  = 2
	analogueTierModifier  = 3
	maxAnalogueCandidates = 2000
)

// analogueCandidate — заявка-кандидат до разбора материала (минимум полей).
type analogueCandidate struct {
	RequestID   int64
	Number      string
	ObjectName  string
	Title       string
	EKN         string
	ObjectEKN   string
	Batch       string
	CompletedAt string
}

// classifyAnalogueTier — чистая функция: к какой ступени относится кандидат.
// Возвращает 0, если материал не совпал.
func classifyAnalogueTier(current, candidate MaterialKey, candidateHasEKN bool) int {
	switch {
	case SameMaterial(current, candidate):
		if candidateHasEKN {
			return analogueTierSameEKN
		}
		return analogueTierSameName
	case SameMaterialLine(current, candidate):
		return analogueTierModifier
	}
	return 0
}

// resolveRequestMaterial — материал самой заявки. Приоритет: requests.ekn (с
// лечением мусора) → objects.characteristics.ekn → название объекта → заголовок
// заявки. Последние два нужны, потому что ЕКН заполнен у меньшинства заявок.
func (s *Server) resolveRequestMaterial(ctx context.Context, req *Request) (MaterialKey, float64, string) {
	ekn := cleanEkn(req.EKN)
	name := ""
	if req.ObjectID > 0 {
		var objName, objEKN string
		if err := s.pool.QueryRow(ctx,
			`SELECT COALESCE(name, ''), COALESCE(characteristics->>'ekn', '') FROM objects WHERE id = $1`,
			req.ObjectID).Scan(&objName, &objEKN); err == nil {
			name = objName
			if ekn == "" {
				ekn = cleanEkn(objEKN)
			}
		}
	}
	if name == "" {
		name = req.Title
	}
	return s.resolveMaterialKey(ctx, ekn, name)
}

// loadAnalogueCandidates — все заявки того же метода, кроме текущей. Материал
// каждой определяется в памяти (ключ строится по ЕКН из кэша либо по названию),
// поэтому SQL не фильтрует по ЕКН. LIMIT с запасом: 2000 заявок — предельный
// объём метода на текущих данных (максимум ~600), фильтр по материалу режет
// выборку уже после загрузки.
func (s *Server) loadAnalogueCandidates(ctx context.Context, methodID, excludeID int64) []analogueCandidate {
	rows, err := s.pool.Query(ctx, `
SELECT r.id,
	COALESCE(NULLIF(r.customer_number, ''), r.lab_number),
	COALESCE(o.name, ''),
	COALESCE(r.title, ''),
	COALESCE(r.ekn, ''),
	COALESCE(o.characteristics->>'ekn', ''),
	COALESCE(o.characteristics->>'batch_number', ''),
	COALESCE(r.completed_at::text, '')
FROM requests r
LEFT JOIN objects o ON o.id = r.object_id
WHERE r.method_id = $1 AND r.id <> $2
ORDER BY r.completed_at DESC NULLS LAST, r.id DESC
LIMIT $3`, methodID, excludeID, maxAnalogueCandidates)
	if err != nil {
		log.Printf("analogues: candidates query: %v", err)
		return nil
	}
	defer rows.Close()
	out := make([]analogueCandidate, 0, 64)
	for rows.Next() {
		var c analogueCandidate
		if err := rows.Scan(&c.RequestID, &c.Number, &c.ObjectName, &c.Title,
			&c.EKN, &c.ObjectEKN, &c.Batch, &c.CompletedAt); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// classifyCandidates — раскладывает кандидатов по ступеням относительно материала
// текущей заявки. Возвращает заявки с метаданными материала.
func (s *Server) classifyCandidates(ctx context.Context, current MaterialKey, candidates []analogueCandidate) []analogueRow {
	out := make([]analogueRow, 0, len(candidates))
	for _, c := range candidates {
		ekn := cleanEkn(c.EKN)
		if ekn == "" {
			ekn = cleanEkn(c.ObjectEKN)
		}
		name := c.ObjectName
		if name == "" {
			name = c.Title
		}
		key, thickness, _ := s.resolveMaterialKey(ctx, ekn, name)
		tier := classifyAnalogueTier(current, key, ekn != "")
		if tier == 0 {
			continue
		}
		row := analogueRow{
			RequestID: c.RequestID,
			Number:    c.Number,
			Object:    c.ObjectName,
			Batch:     c.Batch,
			Tier:      tier,
			Line:      key.Line,
			Modifier:  key.Modifier,
			Thickness: formatThickness(thickness),
		}
		if ekn == "" {
			row.MaterialNote = "ЕКН не указан — определено по названию"
		}
		out = append(out, row)
	}
	return out
}

// ---- Разрез по толщинам (этап D, 2026-10-17) ----
//
// Все толщины одной линейки — один материал (решение пользователя), но в отчёте
// различие толщин показывается отдельно: именно оно даёт «закономерности при
// изменении толщины». Порог текстового вывода о тренде — 3+ испытания на толщину
// (решение пользователя 2026-10-17).

// minTestsForTrend — сколько испытаний должно быть на толщину, чтобы текст
// о закономерности имел право на существование.
const minTestsForTrend = 3

// thicknessBucket — срез значений одного показателя по одной толщине.
type thicknessBucket struct {
	Thickness    string         `json:"thickness"` // "1,5" / "" — не указана
	Tests        int            `json:"tests"`     // сколько разных испытаний (заявок) дали значения
	Count        int            `json:"count"`     // сколько значений (может быть > Tests: по сериям)
	HasNumeric   bool           `json:"has_numeric"`
	Avg          float64        `json:"avg"`
	Min          float64        `json:"min"`
	Max          float64        `json:"max"`
	Distribution map[string]int `json:"distribution,omitempty"`

	avgSum       float64
	numericCount int
}

// trendNote — детерминированный вывод о зависимости показателя от толщины.
type trendNote struct {
	IndicatorID string `json:"indicator_id"`
	Direction   string `json:"direction"` // "растёт" | "падает" | "немонотонно"
	From        string `json:"from"`
	To          string `json:"to"`
	Delta       string `json:"delta"` // "+18%" / "−8%"
}

// bucketKey — ключ среза для значения аналога: толщина в мм с одним знаком
// либо "none", если толщина не определена.
func bucketThicknessKey(thickness string) string {
	if thickness == "" {
		return "none"
	}
	return thickness
}

// computeTrend — чистая функция: вывод о зависимости среднего от толщины.
// Тренд возможен только если у КАЖДОЙ числовой толщины не меньше minTestsForTrend
// испытаний (решение пользователя). Направление — знак приращения среднего между
// соседними толщинами на всей оси; смена знака хотя бы на одном шаге —
// «немонотонно» (и текстового вывода нет).
func computeTrend(indicatorID string, buckets []thicknessBucket) *trendNote {
	numeric := make([]thicknessBucket, 0, len(buckets))
	for _, b := range buckets {
		if !b.HasNumeric || b.Thickness == "" {
			continue
		}
		numeric = append(numeric, b)
	}
	if len(numeric) < 2 {
		return nil
	}
	for _, b := range numeric {
		if b.Tests < minTestsForTrend {
			return nil
		}
	}

	var deltas []float64
	for i := 1; i < len(numeric); i++ {
		deltas = append(deltas, numeric[i].Avg-numeric[i-1].Avg)
	}
	positive, negative := 0, 0
	for _, d := range deltas {
		switch {
		case d > 0:
			positive++
		case d < 0:
			negative++
		}
	}

	first, last := numeric[0], numeric[len(numeric)-1]
	note := &trendNote{
		IndicatorID: indicatorID,
		From:        first.Thickness,
		To:          last.Thickness,
	}
	if positive > 0 && negative > 0 {
		note.Direction = "немонотонно"
		return note
	}
	switch {
	case positive > 0:
		note.Direction = "растёт"
	case negative > 0:
		note.Direction = "падает"
	default:
		note.Direction = "не меняется"
	}
	if first.Avg != 0 {
		pct := (last.Avg - first.Avg) / first.Avg * 100
		sign := "+"
		if pct < 0 {
			sign = "−"
			pct = -pct
		}
		note.Delta = sign + strconv.FormatFloat(pct, 'f', 0, 64) + "%"
	}
	return note
}

// bucketOf — срез, к которому относится испытание данного аналога.
func bucketOf(row analogueRow) string {
	return bucketThicknessKey(row.Thickness)
}

// groupByThickness — чистая функция: раскладывает значения показателя по
// толщинам аналогов. values — пары (строка аналога, значение показателя);
// значения одного испытания (заявки) по нескольким сериям считаются вместе,
// а Tests считает РАЗНЫЕ испытания.
func groupByThickness(values []analogueValue) []thicknessBucket {
	byKey := map[string]*thicknessBucket{}
	seenTests := map[string]map[int64]bool{}
	order := []string{}

	for _, v := range values {
		key := bucketOf(v.row)
		b, ok := byKey[key]
		if !ok {
			b = &thicknessBucket{Thickness: v.row.Thickness, Distribution: map[string]int{}}
			byKey[key] = b
			seenTests[key] = map[int64]bool{}
			order = append(order, key)
		}
		if !seenTests[key][v.row.RequestID] {
			seenTests[key][v.row.RequestID] = true
			b.Tests++
		}
		if v.value == nil || fmtVal(v.value) == "" {
			continue
		}
		b.Count++
		if f, ok := toFloatOK(v.value); ok {
			if !b.HasNumeric {
				b.Avg, b.Min, b.Max = f, f, f
				b.HasNumeric = true
			} else {
				// Накопление суммы делается отдельно — здесь только min/max.
				if f < b.Min {
					b.Min = f
				}
				if f > b.Max {
					b.Max = f
				}
			}
			b.avgSum += f
			b.numericCount++
		} else {
			b.Distribution[fmtVal(v.value)]++
		}
	}

	out := make([]thicknessBucket, 0, len(order))
	for _, key := range order {
		b := byKey[key]
		if b.numericCount > 0 {
			b.Avg = b.avgSum / float64(b.numericCount)
		}
		if len(b.Distribution) == 0 {
			b.Distribution = nil
		}
		out = append(out, *b)
	}
	sortThicknessBuckets(out)
	return out
}

// sortThicknessBuckets — толщины по возрастанию, «не указана» в конце.
func sortThicknessBuckets(buckets []thicknessBucket) {
	for i := 1; i < len(buckets); i++ {
		for j := i; j > 0; j-- {
			a, b := buckets[j-1], buckets[j]
			if thicknessLess(b, a) {
				buckets[j-1], buckets[j] = b, a
				continue
			}
			break
		}
	}
}

func thicknessLess(a, b thicknessBucket) bool {
	if a.Thickness == "" {
		return false
	}
	if b.Thickness == "" {
		return true
	}
	fa := parseThicknessToken(strings.ReplaceAll(a.Thickness, ",", "."))
	fb := parseThicknessToken(strings.ReplaceAll(b.Thickness, ",", "."))
	if fa == 0 || fb == 0 {
		return a.Thickness < b.Thickness
	}
	return fa < fb
}

// analogueValue — значение показателя у конкретного аналога.
type analogueValue struct {
	row   analogueRow
	value any
}

// formatThickness — толщина для документа: "1,5" (запятая — русская запись),
// пустая строка, если толщина не определена.
func formatThickness(v float64) string {
	if v <= 0 {
		return ""
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	return strings.Replace(s, ".", ",", 1)
}
