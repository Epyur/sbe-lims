package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
)

// runRecalcAll — одноразовый CLI-режим «пересчитать все заявки» (2026-08-29,
// по прямому запросу пользователя после накопления живых правок формул/
// классификаций/калибровок). Проходит по каждой паре (request_id, method_id),
// встречающейся в measurement_results, и повторяет ТОТ ЖЕ порядок действий,
// что handleCalculateSeries для одной заявки: applyFormulas на каждую серию →
// applyClassification на каждую серию → recomputeStatistics →
// applyAggregatedFormulas. Ошибка на одной паре не прерывает обход —
// логируется и пропускается, чтобы один сломанный расчёт не блокировал
// пересчёт остальных сотен заявок.
//
//	./lab-service recalc-all
func runRecalcAll(ctx context.Context, s *Server, args []string) {
	fs := flag.NewFlagSet("recalc-all", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "только показать, сколько пар (заявка, метод) будет пересчитано, без записи")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("recalc-all: %v", err)
	}

	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT request_id, method_id FROM measurement_results
WHERE is_statistical_row = false ORDER BY request_id, method_id`)
	if err != nil {
		log.Fatalf("recalc-all: query pairs: %v", err)
	}
	type pair struct {
		requestID int64
		methodID  int64
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.requestID, &p.methodID); err != nil {
			rows.Close()
			log.Fatalf("recalc-all: scan pair: %v", err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()

	log.Printf("recalc-all: найдено %d пар (заявка, метод)", len(pairs))
	if *dryRun {
		return
	}

	ok, failed := 0, 0
	for i, p := range pairs {
		if err := s.recalcRequestMethod(ctx, p.requestID, p.methodID); err != nil {
			failed++
			log.Printf("recalc-all: заявка %d метод %d: ОШИБКА: %v", p.requestID, p.methodID, err)
			continue
		}
		ok++
		if (i+1)%50 == 0 {
			log.Printf("recalc-all: %d/%d обработано", i+1, len(pairs))
		}
	}
	log.Printf("recalc-all: готово — успешно %d, с ошибкой %d", ok, failed)
}

// recalcRequestMethod пересчитывает формулы/классификацию/статистику/агрегаты
// для всех серий одной пары (заявка, метод) — общая логика, вынесенная из
// handleCalculateSeries (results.go), чтобы её же использовал массовый
// recalc-all без HTTP-запроса на заявку.
func (s *Server) recalcRequestMethod(ctx context.Context, requestID, methodID int64) error {
	rows, err := s.pool.Query(ctx, `
SELECT id, series_num, values, COALESCE(equipment_id, 0) FROM measurement_results
WHERE request_id = $1 AND method_id = $2 AND is_statistical_row = false ORDER BY series_num`,
		requestID, methodID)
	if err != nil {
		return err
	}
	type rowT struct {
		id          int64
		seriesNum   int
		values      map[string]any
		equipmentID int64
	}
	var rowsList []rowT
	allValues := []map[string]any{}
	for rows.Next() {
		var rw rowT
		var raw []byte
		if err := rows.Scan(&rw.id, &rw.seriesNum, &raw, &rw.equipmentID); err != nil {
			rows.Close()
			return err
		}
		rw.values = map[string]any{}
		if len(raw) > 0 && string(raw) != "{}" {
			_ = json.Unmarshal(raw, &rw.values)
		}
		rowsList = append(rowsList, rw)
		allValues = append(allValues, rw.values)
	}
	rows.Close()

	for _, rw := range rowsList {
		if err := s.applyFormulas(ctx, requestID, methodID, rw.equipmentID, allValues, rw.values); err != nil {
			return &formulaApplyError{err}
		}
		if err := s.applyClassification(ctx, requestID, methodID, rw.values); err != nil {
			return err
		}
		j, err := json.Marshal(rw.values)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `
UPDATE measurement_results SET values = $2::jsonb, updated_at = now() WHERE id = $1`,
			rw.id, string(j)); err != nil {
			return err
		}
	}

	if err := s.recomputeStatistics(ctx, requestID, methodID); err != nil {
		log.Printf("recalc-all: заявка %d метод %d: статистика: %v", requestID, methodID, err)
	}
	if err := s.applyAggregatedFormulas(ctx, requestID, methodID); err != nil {
		log.Printf("recalc-all: заявка %d метод %d: агрегированные формулы: %v", requestID, methodID, err)
	}
	return nil
}
