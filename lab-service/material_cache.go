package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// ---- Кэш признаков материала по ЕКН (2026-10-17) ----
//
// Отбор аналогов для приложения отчёта идёт по ключу материала (тип, линейка,
// модификатор), а ЕКН — это артикул конкретной фасовки (см. material_key.go).
// Чтобы посчитать ключ для каждой заявки, нужны признаки карточки ЕКН: они берутся
// из ekn-service (QRC) и кэшируются здесь.
//
// Почему свой кэш, а не кэш ekn-базы: там лежит сырой QRC-дамп с TTL 7 дней и
// разбор делается на месте, здесь — уже готовый результат. Сборка одного отчёта
// проходит по всем заявкам метода (сотни), и сетевой запрос на каждую недопустим
// (у QRC лимит 2 запроса/сек). Второй проход идёт целиком из кэша.

// materialFeature — разобранные признаки материала по одному ЕКН.
type materialFeature struct {
	EKN       string  `json:"ekn"`
	Type      string  `json:"type"`
	Line      string  `json:"line"`
	Modifier  string  `json:"modifier"`
	Thickness float64 `json:"thickness"` // мм, 0 — не определена
	WidthMM   float64 `json:"width_mm"`  // мм
	LengthMM  float64 `json:"length_mm"` // мм
	Color     string  `json:"color"`
	Source    string  `json:"source"` // qrc | name | manual
	Note      string  `json:"note"`
}

// materialSources — происхождение разбора. Порядок отражает достоверность.
const (
	materialSourceQRC    = "qrc"
	materialSourceName   = "name"
	materialSourceManual = "manual"
)

// loadMaterialFeature читает кэш. Второе значение — найдена ли строка.
func (s *Server) loadMaterialFeature(ctx context.Context, ekn string) (materialFeature, bool) {
	ekn = cleanEkn(ekn)
	if ekn == "" {
		return materialFeature{}, false
	}
	var f materialFeature
	var thickness, width, length *float64
	err := s.pool.QueryRow(ctx, `
SELECT ekn, type, line, modifier, thickness, width_mm, length_mm, color, source, note
FROM ekn_material_features WHERE ekn = $1`, ekn).
		Scan(&f.EKN, &f.Type, &f.Line, &f.Modifier, &thickness, &width, &length,
			&f.Color, &f.Source, &f.Note)
	if err != nil {
		return materialFeature{}, false
	}
	if thickness != nil {
		f.Thickness = *thickness
	}
	if width != nil {
		f.WidthMM = *width
	}
	if length != nil {
		f.LengthMM = *length
	}
	return f, true
}

// storeMaterialFeature пишет разбор в кэш. Строки, заведённые вручную
// (source='manual'), автоматика не перетирает — они считаются эталоном.
func (s *Server) storeMaterialFeature(ctx context.Context, f materialFeature) {
	if f.EKN == "" {
		return
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO ekn_material_features
	(ekn, type, line, modifier, thickness, width_mm, length_mm, color, source, note, fetched_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
ON CONFLICT (ekn) DO UPDATE SET
	type = EXCLUDED.type,
	line = EXCLUDED.line,
	modifier = EXCLUDED.modifier,
	thickness = EXCLUDED.thickness,
	width_mm = EXCLUDED.width_mm,
	length_mm = EXCLUDED.length_mm,
	color = EXCLUDED.color,
	source = EXCLUDED.source,
	note = EXCLUDED.note,
	fetched_at = now()
WHERE ekn_material_features.source <> 'manual'`,
		f.EKN, f.Type, f.Line, f.Modifier,
		nullableFloat(f.Thickness), nullableFloat(f.WidthMM), nullableFloat(f.LengthMM),
		f.Color, f.Source, f.Note); err != nil {
		log.Printf("material feature: store %s: %v", f.EKN, err)
	}
}

// nullableFloat — 0 трактуется как «не определена» (NULL), чтобы отчёт не
// показывал толщину 0 у материалов, где её нет вовсе.
func nullableFloat(v float64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// resolveMaterialKey — ключ материала и толщина по ЕКН и запасному названию.
// Порядок: кэш → карточка QRC (через ekn-service) → разбор названия. Сетевой
// запрос делается только для ЕКН, которых ещё нет в кэше; ошибка сети не
// роняет отчёт — материал определяется по названию.
func (s *Server) resolveMaterialKey(ctx context.Context, ekn, fallbackName string) (MaterialKey, float64, string) {
	if cached, ok := s.loadMaterialFeature(ctx, ekn); ok {
		return MaterialKey{Type: cached.Type, Line: cached.Line, Modifier: cached.Modifier},
			cached.Thickness, cached.Source
	}

	ekn = cleanEkn(ekn)
	if ekn != "" {
		if groups, err := lookupEknGroups(ctx, ekn); err == nil && len(groups) > 0 {
			feat := materialFeatureFromEkn(groups)
			key, thickness := ParseMaterialFromFeatures(materialFeatures{
				NameWeb:   feat.NameWeb(),
				Type:      feat.RawType(),
				Thickness: feat.Thickness(),
				Width:     feat.WidthMM(),
				Length:    feat.LengthMM(),
				Color:     feat.Color(),
				Armature:  feat.Armature(),
			})
			out := materialFeature{
				EKN: ekn, Type: key.Type, Line: key.Line, Modifier: key.Modifier,
				Thickness: thickness, WidthMM: feat.WidthMM(), LengthMM: feat.LengthMM(),
				Color: feat.Color(), Source: materialSourceQRC,
			}
			s.storeMaterialFeature(ctx, out)
			return key, thickness, materialSourceQRC
		}
	}

	key, thickness := ParseMaterialFromName(fallbackName)
	if ekn != "" && MaterialKeyFilled(key) {
		s.storeMaterialFeature(ctx, materialFeature{
			EKN: ekn, Type: key.Type, Line: key.Line, Modifier: key.Modifier,
			Thickness: thickness, Source: materialSourceName,
			Note: "qrc unavailable, parsed from name",
		})
	}
	return key, thickness, materialSourceName
}

// eknGroupsData — сырые группы QRC одной карточки (data.groups).
type eknGroupsData map[string]any

func (g eknGroupsData) NameWeb() string {
	return qrcString(g, "Web", "name_web", "value")
}

func (g eknGroupsData) RawType() string {
	if v := qrcFirstValue(g, "Modif", "atr_type_material"); v != "" {
		return v
	}
	return qrcString(g, "Modif", "type_ofgidro", "data")
}

func (g eknGroupsData) Thickness() float64 {
	v, unit := qrcMetric(g, "geometric_characteristics", "Thikness")
	return metricToMM(v, unit)
}

func (g eknGroupsData) WidthMM() float64 {
	if v, unit := qrcMetric(g, "geometric_characteristics", "Width"); v > 0 {
		return metricToMM(v, unit)
	}
	return 0
}

func (g eknGroupsData) LengthMM() float64 {
	if v, unit := qrcMetric(g, "geometric_characteristics", "Length"); v > 0 {
		return metricToMM(v, unit)
	}
	return 0
}

func (g eknGroupsData) Color() string {
	return qrcString(g, "Spec", "atr_color_material", "data")
}

func (g eknGroupsData) Armature() string {
	return qrcString(g, "Spec", "armirovanie", "data")
}

// materialFeatureFromGroups — вычисляемое представление карточки QRC для разбора.
type materialFeatureFromGroups struct {
	groups eknGroupsData
}

func materialFeatureFromEkn(groups eknGroupsData) materialFeatureFromGroups {
	return materialFeatureFromGroups{groups: groups}
}

func (f materialFeatureFromGroups) NameWeb() string    { return f.groups.NameWeb() }
func (f materialFeatureFromGroups) RawType() string    { return f.groups.RawType() }
func (f materialFeatureFromGroups) Thickness() float64 { return f.groups.Thickness() }
func (f materialFeatureFromGroups) WidthMM() float64   { return f.groups.WidthMM() }
func (f materialFeatureFromGroups) LengthMM() float64  { return f.groups.LengthMM() }
func (f materialFeatureFromGroups) Color() string      { return f.groups.Color() }
func (f materialFeatureFromGroups) Armature() string   { return f.groups.Armature() }

// metricToMM — приведение значения QRC-метрики к миллиметрам.
func metricToMM(v float64, unit string) float64 {
	switch strings.ToUpper(strings.TrimSpace(unit)) {
	case "METER", "METERS":
		return v * 1000
	case "MILLIMETER", "MILLIMETERS", "":
		return v
	}
	return v
}

// qrcMap — вложенный объект groups по пути ключей.
func qrcMap(g map[string]any, keys ...string) map[string]any {
	var cur any = g
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	m, _ := cur.(map[string]any)
	return m
}

// qrcString — строковое поле attributes[group][attr][field].
func qrcString(g map[string]any, group, attr, field string) string {
	m := qrcMap(g, group, attr)
	if m == nil {
		return ""
	}
	v, _ := m[field].(string)
	return strings.TrimSpace(v)
}

// qrcFirstValue — первое значение списочного атрибута (atr_type_material —
// массив кодов в QRC).
func qrcFirstValue(g map[string]any, group, attr string) string {
	m := qrcMap(g, group, attr)
	if m == nil {
		return ""
	}
	arr, ok := m["value"].([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	v, _ := arr[0].(string)
	return strings.TrimSpace(v)
}

// qrcMetric — числовое значение метрики и её единица (Thikness/Width/Length
// приходят как {unit, amount}).
func qrcMetric(g map[string]any, group, attr string) (float64, string) {
	m := qrcMap(g, group, attr, "value")
	if m == nil {
		return 0, ""
	}
	amount, _ := m["amount"].(string)
	unit, _ := m["unit"].(string)
	return parseThicknessToken(amount), unit
}

// materialFeatureResult — ответ диагностической ручки GET /materials/{ekn}.
type materialFeatureResult struct {
	EKN       string  `json:"ekn"`
	Type      string  `json:"type"`
	Line      string  `json:"line"`
	Modifier  string  `json:"modifier"`
	Thickness float64 `json:"thickness"`
	WidthMM   float64 `json:"width_mm"`
	LengthMM  float64 `json:"length_mm"`
	Color     string  `json:"color"`
	Source    string  `json:"source"`
	NameWeb   string  `json:"name_web"`
}

// handleGetMaterial — GET /api/lab/materials/{ekn}: диагностика разбора материала.
// Возвращает то, что сервис понял про карточку ЕКН, и откуда взял признаки
// (qrc / name / manual). Нужна, чтобы проверить отбор аналогов на конкретном
// материале, не собирая отчёт. Кэш заполняется этим же вызовом.
func (s *Server) handleGetMaterial(w http.ResponseWriter, r *http.Request) {
	ekn := strings.TrimSpace(r.PathValue("ekn"))
	if ekn == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ekn is required"})
		return
	}
	key, thickness, source := s.resolveMaterialKey(r.Context(), ekn, "")
	if !MaterialKeyFilled(key) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "material not determined", "ekn": cleanEkn(ekn),
		})
		return
	}
	// Габарит и цвет берём из кэша — он уже сохранён при разборе; второй сетевой
	// запрос к ekn-service нужен только ради name_web.
	result := materialFeatureResult{
		EKN: cleanEkn(ekn), Type: key.Type, Line: key.Line, Modifier: key.Modifier,
		Thickness: thickness, Source: source,
	}
	if cached, ok := s.loadMaterialFeature(r.Context(), ekn); ok {
		result.WidthMM = cached.WidthMM
		result.LengthMM = cached.LengthMM
		result.Color = cached.Color
	}
	if groups, err := lookupEknGroups(r.Context(), cleanEkn(ekn)); err == nil {
		result.NameWeb = groups.NameWeb()
	}
	writeJSON(w, http.StatusOK, result)
}

// jsonGroups — разбор JSON-ответа QRC в map (для тестов и ручки).
func jsonGroups(raw []byte) eknGroupsData {
	var full struct {
		Groups map[string]any `json:"groups"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil
	}
	return eknGroupsData(full.Groups)
}
