package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"
)

// ---- Экспорт заявки в Excel (2026-08-24, по прямому запросу пользователя:
// "скачивать таблицу всех данных по заявке в формате excel" — по решению
// пользователя, серии + агрегаты/статистика, не только сырые серии). Метод
// заявки — ровно один (1 заявка = 1 метод), поэтому один файл = одна заявка. ----

// methodAttributesOrdered — cfg.InputParams как []MethodAttribute, СОХРАНЯЯ
// порядок JSON-массива (methodAttributesByID в protocol.go даёт map — порядок
// не гарантирован, для колонок таблицы Excel порядок важен).
func methodAttributesOrdered(cfg *MethodConfig) []MethodAttribute {
	var attrs []MethodAttribute
	if b, err := json.Marshal(cfg.InputParams); err == nil {
		_ = json.Unmarshal(b, &attrs)
	}
	return attrs
}

// buildExportXlsx строит книгу из двух листов: "Серии" (одна строка — одна
// серия эксперимента, столбцы — атрибуты уровня "experiment" в порядке
// конфигуратора) и "Агрегаты и статистика" (агрегированные результаты +
// авто-статистика — плоские key-value, т.к. их набор ключей гетерогенен и
// разреженный, в отличие от регулярной таблицы серий).
func buildExportXlsx(cfg *MethodConfig, series []map[string]any, agg, stats map[string]any) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	attrs := methodAttributesOrdered(cfg)
	var expCols []MethodAttribute
	for _, a := range attrs {
		if a.Level == "experiment" {
			expCols = append(expCols, a)
		}
	}

	const seriesSheet = "Серии"
	if err := f.SetSheetName("Sheet1", seriesSheet); err != nil {
		return nil, err
	}
	header := make([]any, 0, len(expCols)+1)
	header = append(header, "Серия №")
	for _, a := range expCols {
		label := a.Name
		if label == "" {
			label = a.ID
		}
		header = append(header, label)
	}
	if err := f.SetSheetRow(seriesSheet, "A1", &header); err != nil {
		return nil, err
	}
	for i, sv := range series {
		row := make([]any, 0, len(expCols)+1)
		row = append(row, i+1)
		for _, a := range expCols {
			row = append(row, fmtVal(sv[a.ID]))
		}
		cell, err := excelize.CoordinatesToCellName(1, i+2)
		if err != nil {
			continue
		}
		if err := f.SetSheetRow(seriesSheet, cell, &row); err != nil {
			return nil, err
		}
	}

	const aggSheet = "Агрегаты и статистика"
	if _, err := f.NewSheet(aggSheet); err != nil {
		return nil, err
	}
	aggHeader := []any{"Показатель", "Значение"}
	if err := f.SetSheetRow(aggSheet, "A1", &aggHeader); err != nil {
		return nil, err
	}
	attrsByID := methodAttributesByID(cfg)
	rowIdx := 2
	writeKV := func(m map[string]any) error {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			label := k
			if a, ok := attrsByID[k]; ok && a.Name != "" {
				label = a.Name
			}
			cell, err := excelize.CoordinatesToCellName(1, rowIdx)
			if err != nil {
				continue
			}
			row := []any{label, fmtVal(m[k])}
			if err := f.SetSheetRow(aggSheet, cell, &row); err != nil {
				return err
			}
			rowIdx++
		}
		return nil
	}
	if err := writeKV(agg); err != nil {
		return nil, err
	}
	if err := writeKV(stats); err != nil {
		return nil, err
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleExportExcel — GET /requests/{id}/export.xlsx. Роль/видимость — тот же
// паттерн, что handleProtocol (protocol.go): requirePerm("editor") на роуте +
// requireLabRead внутри хендлера.
func (s *Server) handleExportExcel(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	req, err := s.loadRequest(r.Context(), requestID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	if req.MethodID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "request has no method"})
		return
	}
	cfg, err := s.loadMethodConfig(r.Context(), req.MethodID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	series, err := s.loadSeriesValues(r.Context(), requestID, req.MethodID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	agg, _ := s.loadAggregatedRow(r.Context(), requestID, req.MethodID)
	stats, _ := s.loadStatsRow(r.Context(), requestID, req.MethodID)

	data, err := buildExportXlsx(cfg, series, agg, stats)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "xlsx: " + err.Error()})
		return
	}
	// base64-в-JSON, а не сырой бинарный ответ (2026-08-24) — тот же паттерн,
	// что docx_base64 у handleProtocol: клиентский тонкий HTTP-хелпер в
	// sbe-lims/sbe-requests уже собран вокруг текстовых ответов (res.text),
	// сырой бинарный поток рисковал бы повреждением при таком транспорте.
	writeJSON(w, http.StatusOK, map[string]any{
		"xlsx_base64":  toBase64(data),
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}
