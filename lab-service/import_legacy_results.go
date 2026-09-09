package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
)

// legacyResultRecord — одна серия измерений для переноса исторических данных
// из старой ЛИМС (файлы Yandex.Disk-lpitn\ПК ЛПИ\out\<external_id>), которые
// не попали в measurement_results при первоначальном переносе заявок (проект
// OLD, import-lpitrack-history создавал только сами заявки/объекты, без
// результатов испытаний).
type legacyResultRecord struct {
	RequestID  int64          `json:"request_id"`
	MethodID   int64          `json:"method_id"`
	InventorID int64          `json:"inventor_id"`
	SeriesNum  int            `json:"series_num"`
	Values     map[string]any `json:"values"`
}

// runImportLegacyResults — постоянный CLI-режим: `./lab-service
// import-legacy-results -file=<path.json> [-dry-run]`. Переиспользует
// saveResultSeries (results.go) — тот же путь, что HTTP POST
// /requests/{id}/results, поэтому формулы/классификация/стат-строка
// считаются как обычно, без дублирования бизнес-логики.
func runImportLegacyResults(ctx context.Context, s *Server, args []string) {
	fs := flag.NewFlagSet("import-legacy-results", flag.ExitOnError)
	filePath := fs.String("file", "", "путь к JSON-файлу со списком серий")
	dryRun := fs.Bool("dry-run", false, "не коммитить — только проверить, что запись не упадёт")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("import-legacy-results: parse args: %v", err)
	}
	if *filePath == "" {
		log.Fatal("import-legacy-results: -file is required")
	}

	data, err := os.ReadFile(*filePath)
	if err != nil {
		log.Fatalf("import-legacy-results: read file: %v", err)
	}
	var records []legacyResultRecord
	if err := json.Unmarshal(data, &records); err != nil {
		log.Fatalf("import-legacy-results: parse json: %v", err)
	}

	mode := "commit"
	if *dryRun {
		mode = "dry-run"
	}
	log.Printf("import-legacy-results: старт, режим=%s, записей=%d", mode, len(records))

	ok, failed := 0, 0
	for _, rec := range records {
		if *dryRun {
			log.Printf("import-legacy-results: [dry-run] request_id=%d method_id=%d series=%d values=%v",
				rec.RequestID, rec.MethodID, rec.SeriesNum, rec.Values)
			ok++
			continue
		}
		id, seriesNum, err := s.saveResultSeries(ctx, rec.RequestID, rec.MethodID, rec.InventorID, 0,
			rec.SeriesNum, rec.Values, "", "", "legacy-import")
		if err != nil {
			log.Printf("import-legacy-results: request_id=%d series=%d: %v — пропущена", rec.RequestID, rec.SeriesNum, err)
			failed++
			continue
		}
		log.Printf("import-legacy-results: request_id=%d -> measurement id=%d series=%d",
			rec.RequestID, id, seriesNum)
		ok++
	}

	log.Printf("import-legacy-results: итог: успешно=%d ошибок=%d режим=%s", ok, failed, mode)
}
