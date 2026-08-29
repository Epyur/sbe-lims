package main

import (
	"math"
	"testing"
)

// 2026-08-24: до этого фикса данные датчика (mesure_data.time/channels/average_temp/
// derivative) не заводились совсем ("не MVP") — по прямому запросу пользователя
// ("как строить графики") теперь есть отдельный, within-series путь построения графика
// (buildChartSeriesFromTimeseries), в отличие от buildChartSeries (across-series, одна
// точка на серию-повтор). "timeseries_series" — список независимых рядов (каждый со
// своим source_param/channel), не общий "channels" на весь конфиг — переработано по
// запросу пользователя учесть "два и более графика... X в одних единицах, но пары X-Y
// не совпадают". Фикстура — по мотивам реального письма external_id=698.
func TestBuildChartSeriesFromTimeseries(t *testing.T) {
	curve := map[string]any{
		"time": []any{0.0, 10.0, 20.0, 30.0},
		"channels": map[string]any{
			"channel_1": []any{20.0, 100.0, 400.0, 900.0},
			"channel_2": []any{21.0, 110.0, 410.0, 910.0},
		},
		"average_temp": []any{20.5, 105.0, 405.0, 905.0},
		"derivative":   []any{0.0, 8.0, 30.0, 50.0},
	}
	seriesValues := []seriesValuesRow{{SeriesNum: 1, Values: map[string]any{"smoke_temp_curve": curve}}}

	cfg := map[string]any{
		"kind": "timeseries",
		"timeseries_series": []any{
			map[string]any{"source_param": "smoke_temp_curve", "channel": "channel_1"},
			map[string]any{"source_param": "smoke_temp_curve", "channel": "channel_2"},
			map[string]any{"source_param": "smoke_temp_curve", "channel": "average_temp"},
		},
	}
	got := buildChartSeriesFromTimeseries(cfg, seriesValues)
	if len(got) != 3 {
		t.Fatalf("got %d series, want 3 (channel_1, channel_2, average_temp)", len(got))
	}
	names := map[string][]float64{}
	for _, s := range got {
		names[s.Name] = s.Y
	}
	if y, ok := names["channel_1"]; !ok || y[3] != 900.0 {
		t.Errorf("channel_1: got %v, want Y[3]=900.0", y)
	}
	if y, ok := names["channel_2"]; !ok || y[3] != 910.0 {
		t.Errorf("channel_2: got %v, want Y[3]=910.0", y)
	}
	if y, ok := names["Среднее по каналам"]; !ok || y[3] != 905.0 {
		t.Errorf("average_temp -> %q: got %v, want Y[3]=905.0", "Среднее по каналам", y)
	}
	// Все X — общий "time" ряд (в этой фикстуре у всех рядов один source_param, так что
	// совпадение ожидаемо — но buildChartSeriesFromTimeseries не ТРЕБУЕТ этого, см. тест
	// TestBuildChartSeriesFromTimeseriesIndependentXPerSeries ниже).
	for _, s := range got {
		if len(s.X) != 4 || s.X[1] != 10.0 {
			t.Errorf("%s: X = %v, want [0 10 20 30]", s.Name, s.X)
		}
	}
}

// derivative — отдельная величина, отличный масштаб от температуры (доли против сотен
// градусов) — по решению пользователя рисуется на ВТОРОЙ оси Y (axis:"y2") ОДНОГО
// изображения с температурой, не отдельным графиком (2026-08-24, "наложение графика
// производной на график температуры").
func TestBuildChartSeriesFromTimeseriesSecondAxis(t *testing.T) {
	curve := map[string]any{
		"time":       []any{0.0, 10.0},
		"derivative": []any{0.1, 8.5},
	}
	seriesValues := []seriesValuesRow{{SeriesNum: 1, Values: map[string]any{"smoke_temp_curve": curve}}}
	cfg := map[string]any{
		"kind": "timeseries",
		"timeseries_series": []any{
			map[string]any{"source_param": "smoke_temp_curve", "channel": "derivative", "axis": "y2"},
		},
	}
	got := buildChartSeriesFromTimeseries(cfg, seriesValues)
	if len(got) != 1 || got[0].Name != "Скорость нарастания" {
		t.Fatalf("got %+v, want exactly one series named \"Скорость нарастания\"", got)
	}
	if !got[0].Y2 {
		t.Errorf("axis:\"y2\" в конфиге должен дать chartSeries.Y2=true")
	}
}

// Ряды из РАЗНЫХ source_param (2026-08-24, прямой запрос пользователя: "в будущем могут
// быть ситуации, когда... ось X будет в одних единицах, но пары X-Y не будут совпадать")
// — каждый ряд тянет СВОЙ time-массив, не переиспользует чужой; независимо от того,
// сколько точек и в каких координатах у другого ряда.
func TestBuildChartSeriesFromTimeseriesIndependentXPerSeries(t *testing.T) {
	curveA := map[string]any{
		"time":     []any{0.0, 10.0, 20.0},
		"channels": map[string]any{"channel_1": []any{1.0, 2.0, 3.0}},
	}
	curveB := map[string]any{
		"time":     []any{0.0, 5.0, 15.0, 25.0}, // другая частота/сетка точек
		"channels": map[string]any{"channel_1": []any{10.0, 20.0, 30.0, 40.0}},
	}
	seriesValues := []seriesValuesRow{{SeriesNum: 1, Values: map[string]any{"curve_a": curveA, "curve_b": curveB}}}
	cfg := map[string]any{
		"kind": "timeseries",
		"timeseries_series": []any{
			map[string]any{"source_param": "curve_a", "channel": "channel_1", "label": "A"},
			map[string]any{"source_param": "curve_b", "channel": "channel_1", "label": "B"},
		},
	}
	got := buildChartSeriesFromTimeseries(cfg, seriesValues)
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2", len(got))
	}
	byName := map[string][]float64{}
	for _, s := range got {
		byName[s.Name] = s.X
	}
	if x := byName["A"]; len(x) != 3 {
		t.Errorf("A: X = %v, want 3 points (свой массив, не совпадает с B)", x)
	}
	if x := byName["B"]; len(x) != 4 {
		t.Errorf("B: X = %v, want 4 points (свой массив, не совпадает с A)", x)
	}
}

// Несколько серий эксперимента на одном графике (2026-08-29, прямая жалоба
// пользователя) — каждая серия рисует свою кривую того же канала; раньше все
// получали одну и ту же подпись легенды, неотличимые между собой на графике.
func TestBuildChartSeriesFromTimeseriesMultiSeriesLabels(t *testing.T) {
	curve1 := map[string]any{"time": []any{0.0, 10.0}, "channels": map[string]any{"channel_1": []any{1.0, 2.0}}}
	curve2 := map[string]any{"time": []any{0.0, 10.0}, "channels": map[string]any{"channel_1": []any{3.0, 4.0}}}
	seriesValues := []seriesValuesRow{
		{SeriesNum: 1, Values: map[string]any{"smoke_temp_curve": curve1}},
		{SeriesNum: 2, Values: map[string]any{"smoke_temp_curve": curve2}},
	}
	cfg := map[string]any{
		"kind": "timeseries",
		"timeseries_series": []any{
			map[string]any{"source_param": "smoke_temp_curve", "channel": "channel_1", "label": "Канал 1"},
		},
	}
	got := buildChartSeriesFromTimeseries(cfg, seriesValues)
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2 (one per measurement series)", len(got))
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["Канал 1 (серия 1)"] || !names["Канал 1 (серия 2)"] {
		t.Errorf("got names %v, want distinct labels per series (\"Канал 1 (серия 1)\"/\"Канал 1 (серия 2)\")", names)
	}
}

// Ровно одна серия — подпись БЕЗ номера (не загромождать легенду, если
// различать всё равно нечего, см. TestBuildChartSeriesFromTimeseries выше —
// уже проверяет имена без суффикса, этот тест защищает именно от регрессии
// "стало добавлять номер всегда").
func TestBuildChartSeriesFromTimeseriesSingleSeriesNoSuffix(t *testing.T) {
	curve := map[string]any{"time": []any{0.0, 10.0}, "channels": map[string]any{"channel_1": []any{1.0, 2.0}}}
	seriesValues := []seriesValuesRow{{SeriesNum: 1, Values: map[string]any{"smoke_temp_curve": curve}}}
	cfg := map[string]any{
		"kind": "timeseries",
		"timeseries_series": []any{
			map[string]any{"source_param": "smoke_temp_curve", "channel": "channel_1", "label": "Канал 1"},
		},
	}
	got := buildChartSeriesFromTimeseries(cfg, seriesValues)
	if len(got) != 1 || got[0].Name != "Канал 1" {
		t.Fatalf("got %+v, want exactly one series named \"Канал 1\" (no series-number suffix)", got)
	}
}

// Атрибут пустой/не заполнен ни в одной серии — пустой результат, не паника.
func TestBuildChartSeriesFromTimeseriesNoData(t *testing.T) {
	cfg := map[string]any{
		"kind": "timeseries",
		"timeseries_series": []any{
			map[string]any{"source_param": "smoke_temp_curve", "channel": "channel_1"},
		},
	}
	got := buildChartSeriesFromTimeseries(cfg, []seriesValuesRow{{SeriesNum: 1, Values: map[string]any{}}, {SeriesNum: 2, Values: map[string]any{"smoke_temp_curve": "не объект"}}})
	if len(got) != 0 {
		t.Errorf("got %+v, want empty (нет валидного значения атрибута ни в одной серии)", got)
	}
}

// fitsMaxN — простой "fits" из тестов: не более n делений (имитирует ограничение по
// доступному месту в пикселях без завязки на реальный рендер шрифта).
func fitsMaxN(n int) func([]float64) bool {
	return func(ticks []float64) bool { return len(ticks) <= n }
}

func allIntegerTicks(ticks []float64) bool {
	for _, v := range ticks {
		if math.Abs(v-math.Round(v)) > 1e-9 {
			return false
		}
	}
	return true
}

// 2026-08-25: деления авто-режима (без spec.Step) теперь ВСЕГДА целые числа из ряда
// {1,2,5}×10^n (прямой запрос пользователя: "деления всегда должны быть целые числа...
// ни каких дробных делений быть не должно"), с минимально возможным шагом, при котором
// подписи не накладываются (здесь имитируется через fitsMaxN). Целочисленность шага
// как побочный эффект защищает и от старой жалобы (2026-08-24) — авто-диапазон не
// должен пересекать ноль в сторону, где данных нет (напр. -68.8 у температуры, где
// отрицательных значений в данных вообще не было).
func TestResolveAxisTicksAutoRangeIsIntegerAndDoesNotInventSign(t *testing.T) {
	lo, hi, ticks := resolveAxisTicks(20.0, 905.0, chartAxisSpec{}, fitsMaxN(8))
	if lo < 0 {
		t.Errorf("lo = %v, want >= 0 (данные все неотрицательные)", lo)
	}
	if !allIntegerTicks(ticks) {
		t.Errorf("ticks = %v, want все целые", ticks)
	}
	if hi < 905.0 {
		t.Errorf("hi = %v, want >= 905.0 (покрывает весь диапазон данных)", hi)
	}
	if len(ticks) > 8 {
		t.Errorf("got %d ticks, want <= 8 (fitsMaxN(8))", len(ticks))
	}

	// симметричный случай — данные все неположительные, авто-диапазон не должен
	// пересекать ноль в плюс.
	lo2, hi2, ticks2 := resolveAxisTicks(-900.0, -20.0, chartAxisSpec{}, fitsMaxN(8))
	if hi2 > 0 {
		t.Errorf("hi = %v, want <= 0 (данные все неположительные)", hi2)
	}
	if lo2 > -900.0 {
		t.Errorf("lo = %v, want <= -900.0 (покрывает весь диапазон данных)", lo2)
	}
	if !allIntegerTicks(ticks2) {
		t.Errorf("ticks = %v, want все целые", ticks2)
	}
}

// spec.Min без spec.Step — сдвигает начало отсчёта авто-шкалы (напр. явно поставить 0,
// даже когда данные начинаются выше нуля); деления остаются целыми.
func TestResolveAxisTicksMinOverrideWithoutStep(t *testing.T) {
	min := 0.0
	lo, _, ticks := resolveAxisTicks(20.0, 905.0, chartAxisSpec{Min: &min}, fitsMaxN(8))
	if lo != 0.0 {
		t.Errorf("lo = %v, want 0.0 (явный Min без Step)", lo)
	}
	if len(ticks) < 2 || ticks[0] != 0.0 {
		t.Errorf("ticks = %v, want >= 2 делений, начиная с 0.0", ticks)
	}
	if !allIntegerTicks(ticks) {
		t.Errorf("ticks = %v, want все целые", ticks)
	}
}

// spec.Step задаёт цену деления — число делений становится переменным, покрывая
// диапазон от origin (spec.Min, если задан, иначе dataMin, округлённый вниз до
// кратного шагу) до dataMax (прямой запрос пользователя: "для каждой оси нужна
// возможность настраивать точку начала отсчёта и цену делений"). Ручной Step —
// единственный случай, где дробные деления в принципе возможны (пользователь сам
// явно ввёл число в редакторе) — resolveAxisTicks его не округляет.
func TestResolveAxisTicksWithStep(t *testing.T) {
	min := 0.0
	step := 100.0
	lo, hi, ticks := resolveAxisTicks(20.0, 905.0, chartAxisSpec{Min: &min, Step: &step}, nil)
	if lo != 0.0 {
		t.Errorf("lo = %v, want 0.0 (origin = Min)", lo)
	}
	wantTicks := []float64{0, 100, 200, 300, 400, 500, 600, 700, 800, 900, 1000}
	if len(ticks) != len(wantTicks) {
		t.Fatalf("got %d ticks %v, want %d ticks %v", len(ticks), ticks, len(wantTicks), wantTicks)
	}
	for i, v := range wantTicks {
		if ticks[i] != v {
			t.Errorf("ticks[%d] = %v, want %v", i, ticks[i], v)
		}
	}
	if hi != 1000.0 {
		t.Errorf("hi = %v, want 1000.0 (последнее деление)", hi)
	}

	// без явного Min — origin считается от dataMin, округлённого вниз до кратного шагу.
	lo2, _, ticks2 := resolveAxisTicks(20.0, 250.0, chartAxisSpec{Step: &step}, nil)
	if lo2 != 0.0 {
		t.Errorf("lo = %v, want 0.0 (floor(20/100)*100)", lo2)
	}
	if len(ticks2) != 4 { // 0,100,200,300
		t.Errorf("got %d ticks %v, want 4", len(ticks2), ticks2)
	}
}

// 2026-08-24: title/x_label/y_label раньше принимались renderChart и тут же
// отбрасывались (`_ = title` и т.п.) — реальный баг, не заметный по коду редактора
// (пользователь сообщил "невозможно изменить название графика", хотя редактор их
// корректно сохранял). Проверяем, что renderChart хотя бы не падает и производит
// валидный PNG с этими параметрами заданными (пиксельное содержимое текста не
// проверяем — юнит-тест на растровый шрифт неинформативен, полагаемся на визуальную
// проверку по живым данным, см. AGENTS.md).
func TestRenderChartWithTitleAndLabelsDoesNotFail(t *testing.T) {
	series := []chartSeries{
		{Name: "Канал 1", X: []float64{0, 10, 20}, Y: []float64{20, 500, 900}},
		{Name: "Производная", X: []float64{0, 10, 20}, Y: []float64{0, 40, -10}, Y2: true},
	}
	png, err := renderChart("line", "Заголовок графика", "Время, с", "Температура, °C", "°C/с", series, chartAxisOverrides{})
	if err != nil {
		t.Fatalf("renderChart: %v", err)
	}
	if len(png) == 0 {
		t.Errorf("got empty PNG bytes")
	}
}
