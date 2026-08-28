package main

import "testing"

// parseCalibrationCurve/floatsToAny — единственные части этого файла без обращения к БД
// (resolveSingleMainEquipment/resolveExperimentDate/resolveEquipmentCalibrationCurve
// используют s.pool — в проекте нет мок-инфраструктуры БД, см. AGENTS.md, проверяются
// живым E2E, не юнит-тестами).

func TestParseCalibrationCurve(t *testing.T) {
	raw := []byte(`{"heat_flow_curve":[{"x":0,"y":0},{"x":10,"y":5},{"x":20,"y":20}],"other_attr":42}`)
	_, xs, ys, err := parseCalibrationCurve(raw, "heat_flow_curve")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantXs := []float64{0, 10, 20}
	wantYs := []float64{0, 5, 20}
	if len(xs) != len(wantXs) || len(ys) != len(wantYs) {
		t.Fatalf("got xs=%v ys=%v, want xs=%v ys=%v", xs, ys, wantXs, wantYs)
	}
	for i := range wantXs {
		if xs[i] != wantXs[i] || ys[i] != wantYs[i] {
			t.Errorf("point %d: got (%v,%v), want (%v,%v)", i, xs[i], ys[i], wantXs[i], wantYs[i])
		}
	}
}

func TestParseCalibrationCurveMissingAttribute(t *testing.T) {
	raw := []byte(`{"other_attr":42}`)
	_, _, _, err := parseCalibrationCurve(raw, "heat_flow_curve")
	if err == nil {
		t.Fatal("expected error for missing curve attribute, got nil")
	}
}

func TestParseCalibrationCurveNotACurve(t *testing.T) {
	// scalar-калибровка (старый формат "один скаляр на атрибут") — attrID указывает не
	// на массив точек, а на обычное число: должно быть отклонено, а не молча съедено.
	raw := []byte(`{"heat_flow_curve": 42}`)
	_, _, _, err := parseCalibrationCurve(raw, "heat_flow_curve")
	if err == nil {
		t.Fatal("expected error for non-array attribute value, got nil")
	}
}

func TestParseCalibrationCurveSkipsInvalidPoints(t *testing.T) {
	// точка без валидных x/y (напр. испытатель оставил строку пустой в таблице
	// точек на клиенте) пропускается, а не валит весь разбор.
	raw := []byte(`{"c":[{"x":0,"y":0},{"x":"","y":""},{"x":10,"y":5}]}`)
	_, xs, ys, err := parseCalibrationCurve(raw, "c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(xs) != 2 || len(ys) != 2 {
		t.Fatalf("expected 2 valid points (invalid one skipped), got xs=%v ys=%v", xs, ys)
	}
}

// floatsToAny + interpolate — подтверждает, что формат, в котором
// injectCalibrationCurves кладёт точки в FormulaEnv.Params ([]any, не []float64),
// действительно совместим с тем, что уже ожидает dsl.go evalInterpolate/toFloatSlice
// (см. TestDSLInterpolate в dsl_test.go — тот же принцип, здесь — сквозная проверка
// именно пути данных из calibration_curve.go).
func TestFloatsToAnyCompatibleWithInterpolate(t *testing.T) {
	xs := []float64{0, 10, 20}
	ys := []float64{0, 5, 20}
	env := &FormulaEnv{Params: map[string]any{
		"x":                  5.0,
		"heat_flow_curve_xs": floatsToAny(xs),
		"heat_flow_curve_ys": floatsToAny(ys),
	}}
	res, err := runFormula("interpolate(x, heat_flow_curve_xs, heat_flow_curve_ys)", env)
	if err != nil {
		t.Fatalf("interpolate over injected curve failed: %v", err)
	}
	got, ok := res.(float64)
	if !ok {
		t.Fatalf("expected float64 result, got %T", res)
	}
	want := 2.5 // между (0,0) и (10,5): 5/10 * 5 = 2.5
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("interpolate(5) = %v, want %v", got, want)
	}
}
