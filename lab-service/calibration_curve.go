package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ---- Калибровочная кривая (WP1, 2026-08-28) ----
//
// См. docs/superpowers/specs/2026-08-28-sbe-lims-calibration-curve-design.md.
// DSL-функция interpolate(x, xs, ys) уже реализована и протестирована в dsl.go — этот файл
// только подставляет ей данные: находит калибровку нужного оборудования, действовавшую на
// дату испытания, и кладёт её точки в FormulaEnv.Params под именами {attr_id}_xs/{attr_id}_ys.

// injectCalibrationCurves — best-effort: если оборудование неоднозначно или калибровки нет,
// просто не подставляет параметры для этого атрибута — формула, если она их использует,
// упадёт со своей обычной ошибкой "параметр не найден" (тем же путём, что и для любого
// другого отсутствующего параметра формулы), а не блокирует весь проход applyFormulas.
func (s *Server) injectCalibrationCurves(ctx context.Context, requestID, methodID, equipmentID int64, cfg *MethodConfig, env *FormulaEnv) {
	var curveAttrIDs []string
	for _, a := range cfg.CalibrationAttributes {
		if dt, _ := a["data_type"].(string); dt != "curve" {
			continue
		}
		if id, _ := a["id"].(string); id != "" {
			curveAttrIDs = append(curveAttrIDs, id)
		}
	}
	if len(curveAttrIDs) == 0 {
		return
	}

	eqID := equipmentID
	if eqID <= 0 {
		resolved, err := s.resolveSingleMainEquipment(ctx, methodID)
		if err != nil {
			return
		}
		eqID = resolved
	}
	asOf := s.resolveExperimentDate(ctx, requestID)

	for _, attrID := range curveAttrIDs {
		xs, ys, err := s.resolveEquipmentCalibrationCurve(ctx, eqID, methodID, attrID, asOf)
		if err != nil {
			continue
		}
		env.Params[attrID+"_xs"] = floatsToAny(xs)
		env.Params[attrID+"_ys"] = floatsToAny(ys)
	}
}

func floatsToAny(vals []float64) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

// resolveSingleMainEquipment возвращает id оборудования, если у метода РОВНО ОДНА единица
// "Основного" (method_equipment.role='main') — иначе ошибка (неоднозначно, испытатель
// обязан был указать equipment_id явно в форме результатов, см. §2 спеки).
func (s *Server) resolveSingleMainEquipment(ctx context.Context, methodID int64) (int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT equipment_id FROM method_equipment WHERE method_id = $1 AND role = 'main'`, methodID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var id int64
	count := 0
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		count++
	}
	if count != 1 {
		return 0, errors.New("equipment ambiguous or not linked")
	}
	return id, nil
}

// isMainEquipmentOfMethod проверяет, что equipmentID — одна из связей method_equipment
// этого метода с role='main' (валидация explicit equipment_id, присланного в
// POST /requests/{id}/results — см. handleCreateResult, results.go).
func (s *Server) isMainEquipmentOfMethod(ctx context.Context, methodID, equipmentID int64) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM method_equipment WHERE method_id = $1 AND equipment_id = $2 AND role = 'main')`,
		methodID, equipmentID).Scan(&exists)
	return exists, err
}

// resolveExperimentDate — дата испытания для выбора "активной на тот момент" калибровки
// (requests.exp_date, опциональное системное поле формы испытателя — TEXT "YYYY-MM-DD",
// см. AGENTS.md "Правило: системные атрибуты"). Если поле не заполнено/не распозналось —
// text.Now() (испытание считается "сейчас", простое и предсказуемое поведение).
func (s *Server) resolveExperimentDate(ctx context.Context, requestID int64) time.Time {
	var expDate string
	if err := s.pool.QueryRow(ctx, `SELECT exp_date FROM requests WHERE id = $1`, requestID).Scan(&expDate); err != nil {
		return time.Now()
	}
	if expDate == "" {
		return time.Now()
	}
	t, err := time.Parse("2006-01-02", expDate)
	if err != nil {
		return time.Now()
	}
	return t
}

// resolveEquipmentCalibrationCurve — последняя (по calibrated_at, не позже asOf) запись
// equipment_calibrations нужных equipmentID+methodID (одно оборудование может калиброваться
// под разные методы разными наборами calibration_attributes, см. equipment_ext.go
// handleCreateEquipmentCalibration) — достаёт values[attrID] (массив {x,y}) и возвращает
// параллельные xs/ys.
func (s *Server) resolveEquipmentCalibrationCurve(ctx context.Context, equipmentID, methodID int64, attrID string, asOf time.Time) ([]float64, []float64, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
SELECT values FROM equipment_calibrations
WHERE equipment_id = $1 AND method_id = $2 AND calibrated_at <= $3
ORDER BY calibrated_at DESC, id DESC LIMIT 1`, equipmentID, methodID, asOf).Scan(&raw)
	if err != nil {
		return nil, nil, err
	}
	values, xs, ys, err := parseCalibrationCurve(raw, attrID)
	_ = values
	return xs, ys, err
}

// parseCalibrationCurve — вынесено из resolveEquipmentCalibrationCurve для юнит-тестов (без
// обращения к БД): разбирает values JSON записи калибровки и достаёт точки атрибута attrID.
func parseCalibrationCurve(raw []byte, attrID string) (map[string]any, []float64, []float64, error) {
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, nil, nil, err
	}
	pointsRaw, ok := values[attrID].([]any)
	if !ok {
		return values, nil, nil, errors.New("calibration attribute is not a curve or is missing")
	}
	xs := make([]float64, 0, len(pointsRaw))
	ys := make([]float64, 0, len(pointsRaw))
	for _, p := range pointsRaw {
		pt, ok := p.(map[string]any)
		if !ok {
			continue
		}
		x, xok := toFloatOK(pt["x"])
		y, yok := toFloatOK(pt["y"])
		if xok && yok {
			xs = append(xs, x)
			ys = append(ys, y)
		}
	}
	if len(xs) == 0 {
		return values, nil, nil, errors.New("calibration curve has no valid points")
	}
	return values, xs, ys, nil
}
