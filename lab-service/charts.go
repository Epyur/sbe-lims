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
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// ---- Свой PNG-рендер простых графиков (line/scatter/bar) ----
// Без внешних зависимостей на рендер геометрии: image/png + image/draw. Текст
// (заголовок/подписи осей/легенда, 2026-08-24) — golang.org/x/image/font/opentype +
// встроенный шрифт Go Regular (поддерживает кириллицу) — до этого фикса title/x_label/
// y_label принимались функцией и тут же отбрасывались (`_ = title` и т.п.), поэтому
// пользователь их видел в редакторе, но НИКОГДА на самой картинке — не баг редактора,
// баг рендера. См. AGENTS.md "график по датчику".

const (
	chartW          = 800
	chartPlotH      = 360 // высота области графика (без заголовка/легенды/подписей осей)
	chartMarginSide = 60
	legendRowH      = 18
	titleBandH      = 26
	xLabelBandH     = 32
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
	chartPurple = color.RGBA{150, 60, 180, 255}
	chartTeal   = color.RGBA{20, 150, 150, 255}
)

var chartPalette = []color.RGBA{chartRed, chartBlue, chartGreen, chartOrange, chartPurple, chartTeal}

// chartFace — шрифт для текста на графике (Go Regular, поддерживает кириллицу — важно,
// подписи/заголовки методов лаборатории на русском). Парсится один раз при старте
// процесса; panic здесь означало бы, что встроенный в бинарник шрифт повреждён —
// в норме не происходит, это не пользовательский ввод.
var chartFace font.Face

func init() {
	parsed, err := opentype.Parse(goregular.TTF)
	if err != nil {
		panic(fmt.Sprintf("chart font parse: %v", err))
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: 13, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(fmt.Sprintf("chart font face: %v", err))
	}
	chartFace = face
}

type chartSeries struct {
	Name  string
	Color color.RGBA
	X     []float64
	Y     []float64
	// Y2 — рисовать по ВТОРОЙ (правой) оси Y со своим собственным масштабом
	// (2026-08-24, прямой запрос пользователя — "наложение графика производной на
	// график температуры": на одной линейной оси производная (~±40) была бы
	// незаметна на фоне температуры (~20-900)). X-массив у каждой серии свой —
	// НЕ предполагается, что точки разных серий совпадают (пользователь явно
	// попросил учесть случай "ось X в одних единицах, но пары X-Y не совпадают" —
	// напр. серии из разных source_param с разной частотой опроса).
	Y2 bool
}

type legendItem struct {
	label string
	color color.RGBA
}

// layoutLegendRows раскладывает подписи серий в строки легенды с переносом по ширине
// maxW — до этого фикса легенды не было вовсе, серии на графике различались только
// цветом без объяснения, что каждый цвет означает.
func layoutLegendRows(series []chartSeries, maxW int) [][]legendItem {
	var rows [][]legendItem
	var cur []legendItem
	curW := 0
	for idx, s := range series {
		if s.Name == "" {
			continue
		}
		col := s.Color
		if col == (color.RGBA{}) {
			col = chartPalette[idx%len(chartPalette)]
		}
		label := s.Name
		if s.Y2 {
			label += " (ось 2)"
		}
		itemW := 16 + chartTextWidth(label) + 18
		if curW+itemW > maxW && len(cur) > 0 {
			rows = append(rows, cur)
			cur = nil
			curW = 0
		}
		cur = append(cur, legendItem{label: label, color: col})
		curW += itemW
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	return rows
}

// renderChart строит PNG-график из серий. chartType: line|scatter|bar. y2Label — подпись
// правой оси, пусто — если вторых осей нет ни у одной серии.
func renderChart(chartType, title, xLabel, yLabel, y2Label string, series []chartSeries) ([]byte, error) {
	plotW := chartW - 2*chartMarginSide

	legendRows := layoutLegendRows(series, plotW)
	marginTop := 10
	if title != "" {
		marginTop = titleBandH
	}
	if len(legendRows) > 0 {
		marginTop += len(legendRows)*legendRowH + 6
	}
	marginBottom := 20
	if xLabel != "" {
		marginBottom = xLabelBandH
	}
	top := marginTop
	totalH := marginTop + chartPlotH + marginBottom

	img := image.NewRGBA(image.Rect(0, 0, chartW, totalH))
	draw.Draw(img, img.Bounds(), &image.Uniform{chartBg}, image.Point{}, draw.Src)

	// границы: X общий для всех серий (одни единицы по X, см. Y2 выше), Y — раздельно
	// для основной и второй оси.
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY1, maxY1 := math.Inf(1), math.Inf(-1)
	minY2, maxY2 := math.Inf(1), math.Inf(-1)
	anyPts, anyY1, anyY2 := false, false, false
	for _, s := range series {
		for i := range s.X {
			if s.X[i] < minX {
				minX = s.X[i]
			}
			if s.X[i] > maxX {
				maxX = s.X[i]
			}
			anyPts = true
			if s.Y2 {
				if s.Y[i] < minY2 {
					minY2 = s.Y[i]
				}
				if s.Y[i] > maxY2 {
					maxY2 = s.Y[i]
				}
				anyY2 = true
			} else {
				if s.Y[i] < minY1 {
					minY1 = s.Y[i]
				}
				if s.Y[i] > maxY1 {
					maxY1 = s.Y[i]
				}
				anyY1 = true
			}
		}
	}
	if !anyPts {
		return nil, errors.New("нет данных для графика")
	}
	if minX == maxX {
		maxX = minX + 1
	}
	if anyY1 {
		if minY1 == maxY1 {
			maxY1 = minY1 + 1
		}
		pad := (maxY1 - minY1) * 0.1
		minY1 -= pad
		maxY1 += pad
	} else {
		minY1, maxY1 = 0, 1
	}
	if anyY2 {
		if minY2 == maxY2 {
			maxY2 = minY2 + 1
		}
		pad := (maxY2 - minY2) * 0.1
		minY2 -= pad
		maxY2 += pad
	}

	toX := func(x float64) int {
		return chartMarginSide + int((x-minX)/(maxX-minX)*float64(plotW))
	}
	toY1 := func(y float64) int {
		return top + chartPlotH - int((y-minY1)/(maxY1-minY1)*float64(chartPlotH))
	}
	toY2 := func(y float64) int {
		return top + chartPlotH - int((y-minY2)/(maxY2-minY2)*float64(chartPlotH))
	}

	// сетка (по основной оси)
	for i := 0; i <= 5; i++ {
		gy := minY1 + (maxY1-minY1)*float64(i)/5
		y := toY1(gy)
		drawHLine(img, chartMarginSide, chartMarginSide+plotW, y, chartGrid)
	}
	for i := 0; i <= 5; i++ {
		gx := minX + (maxX-minX)*float64(i)/5
		x := toX(gx)
		drawVLine(img, x, top, top+chartPlotH, chartGrid)
	}

	// рамка
	drawRect(img, chartMarginSide, top, chartMarginSide+plotW, top+chartPlotH, chartAxis)
	if anyY2 {
		// вторая ось — отдельная вертикальная линия правее рамки (2026-08-24,
		// наложение графиков разного масштаба на одно изображение).
		drawVLine(img, chartMarginSide+plotW+6, top, top+chartPlotH, chartAxis)
	}

	// серии
	for idx, s := range series {
		if len(s.X) == 0 || len(s.Y) == 0 {
			continue
		}
		col := s.Color
		if col == (color.RGBA{}) {
			col = chartPalette[idx%len(chartPalette)]
		}
		toY := toY1
		if s.Y2 {
			toY = toY2
		}
		switch chartType {
		case "bar":
			step := float64(plotW) / float64(len(s.X)+1)
			for i := range s.X {
				x0 := chartMarginSide + int(float64(i+1)*step) - int(step*0.3)
				x1 := chartMarginSide + int(float64(i+1)*step) + int(step*0.3)
				y := toY(s.Y[i])
				drawRectFilled(img, x0, y, x1, top+chartPlotH, col)
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

	// заголовок
	if title != "" {
		drawTextCentered(img, chartW/2, 20, title, chartText)
	}
	// легенда — под заголовком (или у верхнего края, если заголовка нет), с переносом
	legendY := marginTop - len(legendRows)*legendRowH
	for _, row := range legendRows {
		x := chartMarginSide
		for _, item := range row {
			drawRectFilled(img, x, legendY-9, x+12, legendY+3, item.color)
			x += 16
			drawText(img, x, legendY, item.label, chartText)
			x += chartTextWidth(item.label) + 18
		}
		legendY += legendRowH
	}
	// подпись оси X — под графиком по центру
	if xLabel != "" {
		drawTextCentered(img, chartMarginSide+plotW/2, top+chartPlotH+24, xLabel, chartText)
	}
	// подпись оси Y — у верхнего края графика слева (без поворота: собственный
	// рендерер рисует только горизонтальный текст, вращение глифов не реализовано)
	if yLabel != "" {
		drawText(img, chartMarginSide, top-8, yLabel, chartText)
	}
	if y2Label != "" {
		w := chartTextWidth(y2Label)
		drawText(img, chartMarginSide+plotW-w, top-8, y2Label, chartText)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// chartTextWidth измеряет ширину строки в пикселях для данного шрифта — нужно для
// центрирования заголовка и переноса легенды по ширине.
func chartTextWidth(s string) int {
	d := &font.Drawer{Face: chartFace}
	return d.MeasureString(s).Round()
}

// drawText рисует строку, (x, baselineY) — левый край базовой линии (baseline), как
// принято в font.Drawer — НЕ верхний левый угол.
func drawText(img *image.RGBA, x, baselineY int, s string, c color.RGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: chartFace,
		Dot:  fixed.P(x, baselineY),
	}
	d.DrawString(s)
}

func drawTextCentered(img *image.RGBA, centerX, baselineY int, s string, c color.RGBA) {
	drawText(img, centerX-chartTextWidth(s)/2, baselineY, s, c)
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

// buildChartSeriesFromTimeseries — chart_config с "kind":"timeseries" (2026-08-24, по
// прямому запросу пользователя — до этого фикса данные датчика (mesure_data:
// time/channels/average_temp/derivative) не заводились совсем, "не MVP"). В отличие от
// buildChartSeries (одна точка НА КАЖДУЮ серию-повтор, X — атрибут или номер серии),
// здесь ОДНА серия эксперимента несёт целый временной ряд — X/Y читаются ИЗ ОДНОГО
// значения атрибута data_type="timeseries" (см. MethodAttribute), а не поперёк строк
// measurement_results.
//
// "timeseries_series" — список НЕЗАВИСИМЫХ рядов для наложения на одно изображение
// (2026-08-24, переработано по прямому запросу пользователя: "в будущем могут
// накладываться два и более графика... ось X в одних единицах, но пары X-Y не
// совпадают" — каждый элемент списка тянет СВОЙ time-массив из СВОЕГО source_param, не
// предполагает общий с другими элементами массив точек):
//
//	[{"source_param": "smoke_temp_curve", "channel": "channel_1", "label": "Канал 1"},
//	 {"source_param": "smoke_temp_curve", "channel": "derivative", "axis": "y2"}]
//
// source_param — id атрибута data_type="timeseries" со значением {"time":[...],
// "channels":{"channel_1":[...],...}, "average_temp":[...], "derivative":[...]};
// channel — какой под-ряд взять (имя канала, "average_temp" или "derivative");
// axis: "y2" — рисовать по второй (правой) оси (см. chartSeries.Y2).
func buildChartSeriesFromTimeseries(cfg map[string]any, seriesValues []map[string]any) []chartSeries {
	specsRaw, _ := cfg["timeseries_series"].([]any)
	var out []chartSeries
	for _, specRaw := range specsRaw {
		spec, ok := specRaw.(map[string]any)
		if !ok {
			continue
		}
		srcParam, _ := spec["source_param"].(string)
		channelKey, _ := spec["channel"].(string)
		if srcParam == "" || channelKey == "" {
			continue
		}
		label, _ := spec["label"].(string)
		if label == "" {
			label = chartTimeseriesLabel(channelKey)
		}
		axis, _ := spec["axis"].(string)
		for _, sv := range seriesValues {
			raw, ok := sv[srcParam].(map[string]any)
			if !ok {
				continue
			}
			timeArr, err := toFloatSlice(raw["time"])
			if err != nil || len(timeArr) == 0 {
				continue
			}
			var src any
			if channelKey == "average_temp" || channelKey == "derivative" {
				src = raw[channelKey]
			} else if channels, ok := raw["channels"].(map[string]any); ok {
				src = channels[channelKey]
			}
			yArr, err := toFloatSlice(src)
			if err != nil || len(yArr) == 0 {
				continue
			}
			n := len(timeArr)
			if len(yArr) < n {
				n = len(yArr)
			}
			out = append(out, chartSeries{Name: label, X: timeArr[:n], Y: yArr[:n], Y2: axis == "y2"})
		}
	}
	return out
}

// chartTimeseriesLabel — человекочитаемая подпись под ряда графика по имени канала,
// когда конфиг сам не задаёт "label".
func chartTimeseriesLabel(key string) string {
	switch key {
	case "average_temp":
		return "Среднее по каналам"
	case "derivative":
		return "Скорость нарастания"
	default:
		return key
	}
}

// renderChartConfigPNG рендерит один chart_config в PNG — тот же путь, что
// GET /requests/{id}/chart/{cfg_id}, но без HTTP-обёртки. (nil, nil) — не ошибка,
// просто нет данных для этого графика (пропускается вызывающим кодом).
func renderChartConfigPNG(cfg map[string]any, seriesValues []map[string]any) ([]byte, error) {
	var series []chartSeries
	if kind, _ := cfg["kind"].(string); kind == "timeseries" {
		series = buildChartSeriesFromTimeseries(cfg, seriesValues)
	} else {
		series = buildChartSeries(cfg, seriesValues)
	}
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
	y2Label, _ := cfg["y2_label"].(string)
	return renderChart(chartType, title, xLabel, yLabel, y2Label, series)
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
