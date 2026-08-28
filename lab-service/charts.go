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
	chartW = 800
	// axisLabelThickness — толщина (в горизонтальном направлении) повёрнутой на 90°
	// подписи оси Y/Y2 (2026-08-24, см. drawVerticalText) — примерная высота строки
	// шрифта 13pt с запасом; сама подпись после поворота занимает СВОЮ ДЛИНУ по
	// вертикали (укладывается в chartPlotH для типичных подписей), а не по горизонтали.
	axisLabelThickness = 18
	chartPlotH         = 360 // высота области графика (без заголовка/легенды/подписей осей)
	legendRowH         = 18
	titleBandH         = 26
	xLabelBandH        = 32
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

// buildLegendItems строит список подписей серий для легенды — до этого фикса легенды
// не было вовсе, серии на графике различались только цветом без объяснения, что каждый
// цвет означает. Список (не строки с переносом по ширине) — легенда теперь рисуется
// ВНУТРИ поля графика одной колонкой (2026-08-25, прямой запрос пользователя —
// "легенду нужно размещать в поле графика списком", см. renderChart).
func buildLegendItems(series []chartSeries) []legendItem {
	var items []legendItem
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
		items = append(items, legendItem{label: label, color: col})
	}
	return items
}

// drawLegendBox рисует список подписей серий одной колонкой ВНУТРИ поля графика
// (marginLeft..marginLeft+plotW, начиная от top), правый верхний угол по умолчанию
// (2026-08-25, прямой запрос пользователя). Непрозрачный фон + рамка вокруг списка —
// рисуется ПОСЛЕ серий (см. вызов в renderChart), поэтому подписи остаются читаемыми,
// даже если легенда физически перекрывает линию/точки данных под ней.
func drawLegendBox(img *image.RGBA, items []legendItem, marginLeft, top, plotW int) {
	if len(items) == 0 {
		return
	}
	const (
		legendPad     = 6
		legendSwatchW = 12
		legendGap     = 6
	)
	maxLabelW := 0
	for _, it := range items {
		if w := chartTextWidth(it.label); w > maxLabelW {
			maxLabelW = w
		}
	}
	boxW := legendPad*2 + legendSwatchW + legendGap + maxLabelW
	boxH := legendPad*2 + len(items)*legendRowH
	x1 := marginLeft + plotW - legendPad
	x0 := x1 - boxW
	if x0 < marginLeft {
		x0 = marginLeft
	}
	y0 := top + legendPad
	y1 := y0 + boxH

	drawRectFilled(img, x0, y0, x1, y1, chartBg)
	drawRect(img, x0, y0, x1, y1, chartGrid)

	textY := y0 + legendPad + legendRowH - 5
	for _, it := range items {
		sx := x0 + legendPad
		drawRectFilled(img, sx, textY-9, sx+legendSwatchW, textY+3, it.color)
		drawText(img, sx+legendSwatchW+legendGap, textY, it.label, chartText)
		textY += legendRowH
	}
}

// chartAxisSpec — ручная настройка одной оси (2026-08-24, прямой запрос пользователя:
// "для каждой оси нужна возможность настраивать точку начала отсчёта и цену деления").
// Min — точка начала отсчёта (первое деление); Step — цена деления (шаг между
// соседними делениями) — при заданном Step количество делений подстраивается под
// диапазон реальных данных (см. resolveAxisTicks). Без Step число делений тоже
// переменное — авто-режим сам подбирает минимально возможный целый шаг (2026-08-25,
// см. resolveAxisTicks), не фиксированные 6 равных частей. Оба поля опциональны и
// независимы друг от друга (Min без Step — просто сдвигает начало авто-шкалы).
type chartAxisSpec struct {
	Min  *float64
	Step *float64
}

// chartAxisOverrides — ручная настройка по каждой из трёх осей графика.
type chartAxisOverrides struct {
	X, Y1, Y2 chartAxisSpec
}

// niceStepCandidates — возрастающий ряд "круглых" целых шагов деления {1,2,5}×10^n
// (2026-08-25, прямой запрос пользователя: "деления всегда должны быть целые числа...
// ни каких дробных делений быть не должно"). Верхняя граница — заведомо достаточный
// шаг, чтобы диапазон rng покрылся 1-2 делениями (гарантирует, что перебор в
// resolveAxisTicks всегда найдёт подходящий кандидат, а не упрётся в пустой список).
func niceStepCandidates(rng float64) []float64 {
	if rng <= 0 {
		rng = 1
	}
	var steps []float64
	mag := 1.0
	for mag <= rng*2 {
		for _, m := range [3]float64{1, 2, 5} {
			steps = append(steps, m*mag)
		}
		mag *= 10
	}
	if len(steps) == 0 {
		steps = append(steps, math.Ceil(rng))
	}
	return steps
}

// resolveAxisTicks считает диапазон [lo,hi] и список значений делений одной оси из
// реального диапазона данных (dataMin/dataMax) и ручной настройки (spec).
//
// С явным spec.Step (ручная настройка из редактора графика) — деления идут от
// spec.Min (или, при его отсутствии, от dataMin, округлённого вниз до кратного шагу)
// с этим шагом до покрытия dataMax; ответственность за то, целый шаг или дробный,
// здесь на пользователе (сам явно ввёл число в редакторе).
//
// БЕЗ spec.Step (авто-режим, применяется к БОЛЬШИНСТВУ графиков) — деления ВСЕГДА
// целые, шаг — минимально возможный из ряда niceStepCandidates, при котором подписи
// делений не накладываются друг на друга в доступном месте (fits, см. вызовы в
// renderChart — у X это ширина текста, у Y1/Y2 высота строки); spec.Min, если задан,
// — точка отсчёта вместо авто-минимума. Целочисленность нового авто-шага
// автоматически не даёт диапазону пересечь ноль там, где данных с этим знаком нет
// (2026-08-24, по жалобе пользователя: старый 10%-й отступ уводил минимум температуры
// в отрицательные значения, хотя отрицательных значений в данных не было вовсе) —
// floor(dataMin/step)*step при dataMin>=0 и целом положительном step всегда >= 0, и
// симметрично для ceil(dataMax/step)*step при dataMax<=0.
func resolveAxisTicks(dataMin, dataMax float64, spec chartAxisSpec, fits func(ticks []float64) bool) (lo, hi float64, ticks []float64) {
	if spec.Step != nil && *spec.Step > 0 {
		step := *spec.Step
		origin := math.Floor(dataMin/step) * step
		if spec.Min != nil {
			origin = *spec.Min
		}
		n := int(math.Ceil((dataMax-origin)/step)) + 1
		if n < 2 {
			n = 2
		}
		if n > 100 {
			n = 100 // защита от вырожденного шага (напр. почти 0) — не рисуем тысячи делений
		}
		ticks = make([]float64, n)
		for i := 0; i < n; i++ {
			ticks[i] = origin + float64(i)*step
		}
		return origin, ticks[n-1], ticks
	}

	rng := dataMax - dataMin
	var lastStep float64
	for _, step := range niceStepCandidates(rng) {
		lastStep = step
		origin := math.Floor(dataMin/step) * step
		if spec.Min != nil {
			origin = *spec.Min
		}
		top := math.Ceil(dataMax/step) * step
		if top < origin+step {
			top = origin + step
		}
		n := int(math.Round((top-origin)/step)) + 1
		if n < 2 {
			n = 2
			top = origin + step
		}
		candidate := make([]float64, n)
		for i := 0; i < n; i++ {
			candidate[i] = origin + float64(i)*step
		}
		if fits == nil || fits(candidate) {
			return origin, candidate[n-1], candidate
		}
	}
	// Не должно происходить — niceStepCandidates гарантирует кандидат, который
	// укладывается в 1-2 деления. Подстраховка: берём последний (самый крупный).
	origin := math.Floor(dataMin/lastStep) * lastStep
	if spec.Min != nil {
		origin = *spec.Min
	}
	return origin, origin + lastStep, []float64{origin, origin + lastStep}
}

// renderChart строит PNG-график из серий. chartType: line|scatter|bar. y2Label — подпись
// правой оси, пусто — если вторых осей нет ни у одной серии.
func renderChart(chartType, title, xLabel, yLabel, y2Label string, series []chartSeries, axisOverride chartAxisOverrides) ([]byte, error) {
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

	// Y1/Y2 — сначала (доступное место для их делений, chartPlotH, — константа, не
	// зависит от margin'ов); margin'ы (и, значит, доступное место для оси X) считаются
	// из ширины ИХ подписей делений, поэтому ось X резолвится позже (см. xFits ниже).
	const yTickRowMinH = 16 // минимальная высота строки подписи деления (шрифт 13pt)
	yFits := func(ticks []float64) bool {
		if len(ticks) < 2 {
			return true
		}
		rowH := float64(chartPlotH) / float64(len(ticks)-1)
		return rowH >= yTickRowMinH
	}
	var y1TicksF []float64
	if anyY1 {
		minY1, maxY1, y1TicksF = resolveAxisTicks(minY1, maxY1, axisOverride.Y1, yFits)
	} else {
		minY1, maxY1 = 0, 1
	}
	var y2TicksF []float64
	if anyY2 {
		minY2, maxY2, y2TicksF = resolveAxisTicks(minY2, maxY2, axisOverride.Y2, yFits)
	}

	y1Ticks := make([]string, len(y1TicksF))
	maxY1TickW := 0
	for i, v := range y1TicksF {
		y1Ticks[i] = formatTickValue(v)
		if w := chartTextWidth(y1Ticks[i]); w > maxY1TickW {
			maxY1TickW = w
		}
	}
	y2Ticks := make([]string, len(y2TicksF))
	maxY2TickW := 0
	for i, v := range y2TicksF {
		y2Ticks[i] = formatTickValue(v)
		if w := chartTextWidth(y2Ticks[i]); w > maxY2TickW {
			maxY2TickW = w
		}
	}

	// раскладка полей: [подпись оси Y] [деления Y] [график] [деления Y2] [подпись оси Y2]
	// (2026-08-24, по прямой просьбе пользователя — подписи осей стоят РЯДОМ С ОСЬЮ, не
	// поверх графика; числа делений раньше не рисовались вовсе, только линии сетки).
	// Подпись оси Y повёрнута на 90° против часовой стрелки (см. drawVerticalText) —
	// поэтому в горизонтальном направлении занимает не свою текстовую ширину, а толщину
	// строки шрифта (axisLabelThickness).
	const tickGap = 4
	marginLeft := 12 + maxY1TickW + tickGap
	if yLabel != "" {
		marginLeft += axisLabelThickness + 6
	}
	marginRight := 12
	if anyY2 {
		marginRight += maxY2TickW + tickGap
	}
	if y2Label != "" {
		marginRight += axisLabelThickness + 6
	}
	plotW := chartW - marginLeft - marginRight

	// Ось X — теперь, когда известна доступная ширина plotW: минимальный шаг из
	// niceStepCandidates, при котором подписи делений не накладываются друг на друга
	// (2026-08-25, прямой запрос пользователя — "цена делений должна по умолчанию быть
	// минимально возможной для того, чтобы цифры не накладывались друг на друга").
	const tickLabelGapPx = 10
	xFits := func(ticks []float64) bool {
		if len(ticks) < 2 {
			return true
		}
		maxW := 0
		for _, v := range ticks {
			if w := chartTextWidth(formatTickValue(v)); w > maxW {
				maxW = w
			}
		}
		spacing := float64(plotW) / float64(len(ticks)-1)
		return spacing >= float64(maxW+tickLabelGapPx)
	}
	var xTicksF []float64
	minX, maxX, xTicksF = resolveAxisTicks(minX, maxX, axisOverride.X, xFits)
	xTicks := make([]string, len(xTicksF))
	for i, v := range xTicksF {
		xTicks[i] = formatTickValue(v)
	}

	// легенда (2026-08-25) теперь рисуется ВНУТРИ поля графика (см. drawLegendBox
	// ниже, после серий) — больше не отдельная полоса над графиком, поэтому marginTop
	// от неё не зависит.
	marginTop := 10
	if title != "" {
		marginTop = titleBandH
	}
	marginBottom := 30 // деления оси X
	if xLabel != "" {
		marginBottom += xLabelBandH
	}
	top := marginTop
	totalH := marginTop + chartPlotH + marginBottom

	img := image.NewRGBA(image.Rect(0, 0, chartW, totalH))
	draw.Draw(img, img.Bounds(), &image.Uniform{chartBg}, image.Point{}, draw.Src)

	toX := func(x float64) int {
		return marginLeft + int((x-minX)/(maxX-minX)*float64(plotW))
	}
	toY1 := func(y float64) int {
		return top + chartPlotH - int((y-minY1)/(maxY1-minY1)*float64(chartPlotH))
	}
	toY2 := func(y float64) int {
		return top + chartPlotH - int((y-minY2)/(maxY2-minY2)*float64(chartPlotH))
	}

	// сетка (по основной оси, Y1) + деления с числами. Деления Y2 — СВОИ позиции по
	// пикселям (toY2), не привязаны к сетке Y1: при независимой ручной настройке шага
	// числа делений могут не совпадать (2026-08-24), поэтому у Y2 только текст деления,
	// без отдельной сетки линий (не плодим два пересекающихся набора линий).
	for i, gy := range y1TicksF {
		y := toY1(gy)
		drawHLine(img, marginLeft, marginLeft+plotW, y, chartGrid)
		w := chartTextWidth(y1Ticks[i])
		drawText(img, marginLeft-tickGap-w, y+4, y1Ticks[i], chartText)
	}
	for i, gy2 := range y2TicksF {
		y := toY2(gy2)
		drawText(img, marginLeft+plotW+tickGap, y+4, y2Ticks[i], chartText)
	}
	for i, gx := range xTicksF {
		x := toX(gx)
		drawVLine(img, x, top, top+chartPlotH, chartGrid)
		drawTextCentered(img, x, top+chartPlotH+16, xTicks[i], chartText)
	}

	// рамка
	drawRect(img, marginLeft, top, marginLeft+plotW, top+chartPlotH, chartAxis)
	if anyY2 {
		// вторая ось — отдельная вертикальная линия правее рамки (2026-08-24,
		// наложение графиков разного масштаба на одно изображение).
		drawVLine(img, marginLeft+plotW+6, top, top+chartPlotH, chartAxis)
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
				x0 := marginLeft + int(float64(i+1)*step) - int(step*0.3)
				x1 := marginLeft + int(float64(i+1)*step) + int(step*0.3)
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
	// легенда — списком ВНУТРИ поля графика, правый верхний угол по умолчанию
	// (2026-08-25, прямой запрос пользователя: "легенду нужно размещать в поле
	// графика списком... предпочтительное расположение — правый верхний угол").
	// Рисуется ПОСЛЕ серий, с непрозрачным фоном — подписи остаются читаемыми, даже
	// если легенда физически перекрывает линию/точки данных.
	drawLegendBox(img, buildLegendItems(series), marginLeft, top, plotW)
	// подпись оси X — под делениями, по центру графика
	if xLabel != "" {
		drawTextCentered(img, marginLeft+plotW/2, top+chartPlotH+xLabelBandH, xLabel, chartText)
	}
	// подпись оси Y — слева от делений основной оси, по вертикальному центру графика,
	// повёрнута на 90° против часовой стрелки (2026-08-24, прямой запрос пользователя —
	// читается снизу вверх, общепринятая ориентация подписи оси Y).
	if yLabel != "" {
		drawVerticalText(img, yLabel, 4, top+chartPlotH/2, chartText)
	}
	// подпись оси Y2 — справа от делений второй оси, по вертикальному центру, тот же
	// поворот.
	if y2Label != "" {
		drawVerticalText(img, y2Label, chartW-4-axisLabelThickness, top+chartPlotH/2, chartText)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// formatTickValue — компактное текстовое представление числового деления оси. Деления
// авто-режима (resolveAxisTicks без spec.Step) всегда целые (2026-08-25, прямой запрос
// пользователя) — выводим их как обычное целое число ('f', 0 знаков), без ".0" и без
// экспоненциальной записи, в которую 'g' уходит на круглых числах вроде 1000/10000.
// Дробные деления возможны только при ручном spec.Step — для них старое компактное
// представление (до 3 значащих цифр).
func formatTickValue(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'g', 3, 64)
}

// chartTextWidth измеряет ширину строки в пикселях для данного шрифта — нужно для
// центрирования заголовка и переноса легенды по ширине.
func chartTextWidth(s string) int {
	d := &font.Drawer{Face: chartFace}
	return d.MeasureString(s).Round()
}

// rotateImage90CCW поворачивает прямоугольное RGBA-изображение на 90° ПРОТИВ часовой
// стрелки (2026-08-24, прямой запрос пользователя — подпись оси Y читается снизу
// вверх, общепринятая ориентация). Пиксель источника (x,y) переходит в (y, w-1-x) —
// верхний край исходного изображения (начало текста) становится ЛЕВЫМ краем
// результата, правый край (конец текста) — верхним; итог читается снизу вверх слева
// направо в исходном порядке символов.
func rotateImage90CCW(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(y, w-1-x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// drawVerticalText рисует текст, повёрнутый на 90° против часовой стрелки. leftX —
// левый край повёрнутого блока, centerY — вертикальный центр (2026-08-24, подпись оси
// Y/Y2 — собственный текстовый рендерер не умеет поворачивать глифы напрямую, поэтому
// текст сначала рисуется горизонтально на прозрачном холсте нужного размера, затем
// холст целиком поворачивается и накладывается на итоговое изображение).
func drawVerticalText(dst *image.RGBA, s string, leftX, centerY int, c color.RGBA) {
	w := chartTextWidth(s)
	if w == 0 {
		return
	}
	const lineH = 16
	tmp := image.NewRGBA(image.Rect(0, 0, w+2, lineH))
	d := &font.Drawer{Dst: tmp, Src: image.NewUniform(c), Face: chartFace, Dot: fixed.P(0, lineH-4)}
	d.DrawString(s)
	rotated := rotateImage90CCW(tmp)
	rb := rotated.Bounds()
	destRect := image.Rect(leftX, centerY-rb.Dy()/2, leftX+rb.Dx(), centerY-rb.Dy()/2+rb.Dy())
	draw.Draw(dst, destRect, rotated, image.Point{}, draw.Over)
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

// handleCalibrationCurveChart — GET /equipment/{id}/calibrations/{calibration_id}/curve-chart/{attr_id}
// (WP1, 2026-08-28): PNG-график ОДНОГО атрибута data_type="curve" конкретной записи
// калибровки. В отличие от chart_configs метода — не требует отдельной настройки графика,
// сам факт, что атрибут — "кривая", уже достаточен, чтобы её можно было визуализировать.
func (s *Server) handleCalibrationCurveChart(w http.ResponseWriter, r *http.Request) {
	equipmentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	calibrationID, err := strconv.ParseInt(r.PathValue("calibration_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid calibration_id"})
		return
	}
	attrID := r.PathValue("attr_id")

	var methodID int64
	var raw []byte
	err = s.pool.QueryRow(r.Context(), `
SELECT COALESCE(method_id, 0), values FROM equipment_calibrations
WHERE id = $1 AND equipment_id = $2`, calibrationID, equipmentID).Scan(&methodID, &raw)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "calibration not found"})
		return
	}

	_, xs, ys, err := parseCalibrationCurve(raw, attrID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "curve: " + err.Error()})
		return
	}

	label := attrID
	if methodID > 0 {
		if cfg, cerr := s.loadMethodConfig(r.Context(), methodID); cerr == nil {
			for _, a := range cfg.CalibrationAttributes {
				if id, _ := a["id"].(string); id == attrID {
					if name, _ := a["name"].(string); name != "" {
						label = name
					}
					break
				}
			}
		}
	}

	series := []chartSeries{{Name: label, X: xs, Y: ys}}
	pngBytes, err := renderChart("line", label, "", "", "", series, chartAxisOverrides{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "chart: " + err.Error()})
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
	axisOverride := chartAxisOverrides{
		X:  chartAxisSpecFromConfig(cfg, "x_axis_min", "x_axis_step"),
		Y1: chartAxisSpecFromConfig(cfg, "y_axis_min", "y_axis_step"),
		Y2: chartAxisSpecFromConfig(cfg, "y2_axis_min", "y2_axis_step"),
	}
	return renderChart(chartType, title, xLabel, yLabel, y2Label, series, axisOverride)
}

// chartAxisSpecFromConfig достаёт ручную настройку одной оси из chart_config
// (2026-08-24, прямой запрос пользователя — "для каждой оси нужна возможность
// настраивать точку начала отсчёта и цену деления"). Оба поля независимо опциональны.
func chartAxisSpecFromConfig(cfg map[string]any, minKey, stepKey string) chartAxisSpec {
	var spec chartAxisSpec
	if f, ok := toFloatOK(cfg[minKey]); ok {
		spec.Min = &f
	}
	if f, ok := toFloatOK(cfg[stepKey]); ok && f > 0 {
		spec.Step = &f
	}
	return spec
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
