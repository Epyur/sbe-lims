package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ---- Свой PNG-рендер простых графиков (line/scatter/bar) ----
// Без внешних зависимостей: image/png + image/draw.

const (
	chartW      = 800
	chartH      = 480
	chartMargin = 60
)

var (
	chartBg     = color.RGBA{255, 255, 255, 255}
	chartGrid   = color.RGBA{220, 220, 220, 255}
	chartAxis   = color.RGBA{40, 40, 40, 255}
	chartText   = color.RGBA{40, 40, 40, 255}
	chartRed    = color.RGBA{200, 30, 30, 255}
	chartBlue   = color.RGBA{30, 60, 180, 255}
	chartGreen  = color.RGBA{30, 140, 60, 255}
	chartOrange = color.RGBA{220, 130, 20, 255}
)

var chartPalette = []color.RGBA{chartRed, chartBlue, chartGreen, chartOrange}

type chartSeries struct {
	Name  string
	Color color.RGBA
	X     []float64
	Y     []float64
}

// renderChart строит PNG-график из серий. chartType: line|scatter|bar.
func renderChart(chartType, title, xLabel, yLabel string, series []chartSeries) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, chartW, chartH))
	draw.Draw(img, img.Bounds(), &image.Uniform{chartBg}, image.Point{}, draw.Src)

	plotW := chartW - 2*chartMargin
	plotH := chartH - 2*chartMargin

	// границы
	minX, maxX, minY, maxY := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	anyPts := false
	for _, s := range series {
		for i := range s.X {
			if s.X[i] < minX {
				minX = s.X[i]
			}
			if s.X[i] > maxX {
				maxX = s.X[i]
			}
			if s.Y[i] < minY {
				minY = s.Y[i]
			}
			if s.Y[i] > maxY {
				maxY = s.Y[i]
			}
			anyPts = true
		}
	}
	if !anyPts {
		return nil, errors.New("нет данных для графика")
	}
	if minX == maxX {
		maxX = minX + 1
	}
	if minY == maxY {
		maxY = minY + 1
	}
	// небольшой отступ
	padY := (maxY - minY) * 0.1
	minY -= padY
	maxY += padY

	toX := func(x float64) int {
		return chartMargin + int((x-minX)/(maxX-minX)*float64(plotW))
	}
	toY := func(y float64) int {
		return chartMargin + plotH - int((y-minY)/(maxY-minY)*float64(plotH))
	}

	// сетка
	for i := 0; i <= 5; i++ {
		gy := minY + (maxY-minY)*float64(i)/5
		y := toY(gy)
		drawHLine(img, chartMargin, chartMargin+plotW, y, chartGrid)
	}
	for i := 0; i <= 5; i++ {
		gx := minX + (maxX-minX)*float64(i)/5
		x := toX(gx)
		drawVLine(img, x, chartMargin, chartMargin+plotH, chartGrid)
	}

	// рамка
	drawRect(img, chartMargin, chartMargin, chartMargin+plotW, chartMargin+plotH, chartAxis)

	// серии
	for idx, s := range series {
		if len(s.X) == 0 || len(s.Y) == 0 {
			continue
		}
		col := s.Color
		if col == (color.RGBA{}) {
			col = chartPalette[idx%len(chartPalette)]
		}
		switch chartType {
		case "bar":
			step := float64(plotW) / float64(len(s.X)+1)
			for i := range s.X {
				x0 := chartMargin + int(float64(i+1)*step) - int(step*0.3)
				x1 := chartMargin + int(float64(i+1)*step) + int(step*0.3)
				y := toY(s.Y[i])
				drawRectFilled(img, x0, y, x1, chartMargin+plotH, col)
			}
		case "scatter":
			for i := range s.X {
				x, y := toX(s.X[i]), toY(s.Y[i])
				drawDot(img, x, y, 3, col)
			}
		default: // line
			for i := 0; i < len(s.X)-1; i++ {
				x0, y0 := toX(s.X[i]), toY(s.Y[i])
				x1, y1 := toX(s.X[i+1]), toY(s.Y[i+1])
				drawLine(img, x0, y0, x1, y1, col)
			}
		}
	}

	// заголовок и подписи осей (минимально — текст не рисуем сложными шрифтами,
	// выводим значения по краям упрощённо)
	_ = title
	_ = xLabel
	_ = yLabel

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawHLine(img *image.RGBA, x0, x1, y int, c color.RGBA) {
	for x := x0; x <= x1; x++ {
		img.Set(x, y, c)
	}
}

func drawVLine(img *image.RGBA, x, y0, y1 int, c color.RGBA) {
	for y := y0; y <= y1; y++ {
		img.Set(x, y, c)
	}
}

func drawRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	drawHLine(img, x0, x1, y0, c)
	drawHLine(img, x0, x1, y1, c)
	drawVLine(img, x0, y0, y1, c)
	drawVLine(img, x1, y0, y1, c)
}

func drawRectFilled(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			img.Set(x, y, c)
		}
	}
}

func drawDot(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				img.Set(x, y, c)
			}
		}
	}
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)
	sx := 1
	if x0 > x1 {
		sx = -1
	}
	sy := 1
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- Handler ----

func (s *Server) handleChart(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(r.Context(), currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	cfgID := r.PathValue("cfg_id")

	// найти конфиг чарта по всем методам заявки
	cfg, methodID, err := s.findChartConfig(r.Context(), requestID, cfgID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "chart config not found"})
		return
	}

	// собрать данные: X из x_column, Y из series_config[].source_param
	seriesValues, err := s.loadSeriesValues(r.Context(), requestID, methodID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	pngBytes, err := renderChartConfigPNG(cfg, seriesValues)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "chart: " + err.Error()})
		return
	}
	if pngBytes == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no data for chart"})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pngBytes)
}

// buildChartSeries строит серии графика (chartSeries) из конфига и values серий —
// общая логика для HTTP-хендлера (handleChart) и встраивания в протокол
// (protocol.go, 2026-08-22 — по просьбе пользователя графики теперь можно вставлять
// в протокол, как и фото).
func buildChartSeries(cfg map[string]any, seriesValues []map[string]any) []chartSeries {
	xCol, _ := cfg["x_column"].(string)
	seriesCfg, _ := cfg["series_config"].([]any)
	type sdata struct {
		name string
		x, y []float64
	}
	series := make([]chartSeries, 0, 4)
	for _, sc := range seriesCfg {
		m, ok := sc.(map[string]any)
		if !ok {
			continue
		}
		src, _ := m["source_param"].(string)
		name, _ := m["label"].(string)
		if name == "" {
			name = src
		}
		var sd sdata
		for _, sv := range seriesValues {
			xv := 0.0
			if xCol != "" {
				if f, err := toFloat(sv[xCol]); err == nil {
					xv = f
				} else if n, ok := sv[xCol].(float64); ok {
					xv = n
				} else {
					xv = float64(len(sd.x) + 1)
				}
			} else {
				xv = float64(len(sd.x) + 1)
			}
			if yv, ok := sv[src]; ok {
				if f, err := toFloat(yv); err == nil {
					sd.x = append(sd.x, xv)
					sd.y = append(sd.y, f)
				}
			}
		}
		if len(sd.y) > 0 {
			series = append(series, chartSeries{Name: name, X: sd.x, Y: sd.y})
		}
	}
	return series
}

// renderChartConfigPNG рендерит один chart_config в PNG — тот же путь, что
// GET /requests/{id}/chart/{cfg_id}, но без HTTP-обёртки. (nil, nil) — не ошибка,
// просто нет данных для этого графика (пропускается вызывающим кодом).
func renderChartConfigPNG(cfg map[string]any, seriesValues []map[string]any) ([]byte, error) {
	series := buildChartSeries(cfg, seriesValues)
	if len(series) == 0 {
		return nil, nil
	}
	chartType, _ := cfg["chart_type"].(string)
	if chartType == "" {
		chartType = "line"
	}
	title, _ := cfg["title"].(string)
	xLabel, _ := cfg["x_label"].(string)
	yLabel, _ := cfg["y_label"].(string)
	return renderChart(chartType, title, xLabel, yLabel, series)
}

// findChartConfig ищет конфиг чарта по cfg_id среди методов заявки.
func (s *Server) findChartConfig(ctx context.Context, requestID int64, cfgID string) (map[string]any, int64, error) {
	// метод заявки (1 заявка = 1 метод)
	var methodIDs []int64
	var mid int64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(method_id, 0) FROM requests WHERE id = $1`, requestID).Scan(&mid)
	if err != nil {
		return nil, 0, err
	}
	if mid > 0 {
		methodIDs = append(methodIDs, mid)
	}
	for _, mid := range methodIDs {
		cfg, err := s.loadMethodConfig(ctx, mid)
		if err != nil {
			continue
		}
		for _, c := range cfg.ChartConfigs {
			id, _ := c["id"].(string)
			if id == cfgID {
				return c, mid, nil
			}
		}
	}
	return nil, 0, pgx.ErrNoRows
}

var _ = log.Printf
var _ = fmt.Sprintf
