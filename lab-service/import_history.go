package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// importLabID/importProjectCode — см. план "перенос исторических заявок
// (LPITrack) в проект Old" (2026-08-21): все переносимые заявки — лаба ЛПИ
// (единственная, есть в method_labs у всех нужных методов), проект с кодом
// OLD (уже существует, «Заявки прошлых периодов»).
const importLabID = 1
const importProjectCode = "OLD"

// lpitrackImportRecord — формат JSON, который готовит
// scripts/prepare_lpitrack_import.py из mail_records.jsonl + desktop-базы
// LIMS_LPI (метод разрешён на этапе подготовки — из desktop, где заявка туда
// попала, иначе из самого письма; без метода в обоих источниках запись не
// попадает в файл вовсе).
type lpitrackImportRecord struct {
	ExternalID   string `json:"external_id"`
	DateIn       string `json:"date_in"` // YYYY-MM-DD, дата подачи заявки (не дата импорта)
	MethodID     int64  `json:"method_id"`
	CustMail     string `json:"cust_mail"`
	ProductName  string `json:"product_name"`
	Ekn          string `json:"ekn"`
	Thickness    string `json:"thickness"`
	MaterialType string `json:"material_type"`
	AimIndicator string `json:"aim_indicator"`
	Description  string `json:"description"`
	BatchNumber  string `json:"batch_number"`
	PriorityRaw  string `json:"priority_raw"`
}

// runImportLpitrackHistory — постоянный CLI-режим lab-service (не одноразовый
// выброс-скрипт): `./lab-service import-lpitrack-history -file=<path.json>
// [-dry-run]`. В этом режиме сервер не поднимается.
//
// Нумерация номеров (nextSeq/buildNumbers, requests.go) считается по
// РЕАЛЬНОМУ году подачи заявки (rec.DateIn), а не по текущему году сервера —
// поэтому обычные POST /requests / POST /api/lab/sync/push не годятся (они
// всегда берут time.Now().UTC().Year()), нужен прямой вызов с историческим
// годом, как в rollout.go.
func runImportLpitrackHistory(ctx context.Context, s *Server, args []string) {
	fs := flag.NewFlagSet("import-lpitrack-history", flag.ExitOnError)
	filePath := fs.String("file", "", "путь к JSON-файлу (scripts/prepare_lpitrack_import.py)")
	dryRun := fs.Bool("dry-run", false, "не коммитить — только показать, что будет вставлено")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("import-lpitrack-history: parse args: %v", err)
	}
	if *filePath == "" {
		log.Fatal("import-lpitrack-history: -file is required")
	}

	data, err := os.ReadFile(*filePath)
	if err != nil {
		log.Fatalf("import-lpitrack-history: read file: %v", err)
	}
	var records []lpitrackImportRecord
	if err := json.Unmarshal(data, &records); err != nil {
		log.Fatalf("import-lpitrack-history: parse json: %v", err)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].DateIn < records[j].DateIn })

	var projectID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM projects WHERE code = $1`, importProjectCode).
		Scan(&projectID); err != nil {
		log.Fatalf("import-lpitrack-history: проект %q не найден: %v", importProjectCode, err)
	}

	mode := "commit"
	if *dryRun {
		mode = "dry-run"
	}
	log.Printf("import-lpitrack-history: старт, режим=%s, записей=%d, project_id=%d",
		mode, len(records), projectID)

	created, skipped := 0, 0
	byYear := map[int]int{}

	for _, rec := range records {
		dateIn, err := time.Parse("2006-01-02", rec.DateIn)
		if err != nil {
			log.Printf("import-lpitrack-history: external_id=%s: некорректная date_in %q: %v — пропущена",
				rec.ExternalID, rec.DateIn, err)
			skipped++
			continue
		}
		year := dateIn.Year()

		requestID, seq, customer, lab, err := importOneLpitrackRequest(ctx, s, rec, dateIn, year, projectID, *dryRun)
		if err != nil {
			log.Printf("import-lpitrack-history: external_id=%s: %v — пропущена", rec.ExternalID, err)
			skipped++
			continue
		}
		created++
		byYear[year]++
		log.Printf("import-lpitrack-history: [%s] external_id=%s -> request id=%d seq=%d lab_number=%s customer_number=%s",
			mode, rec.ExternalID, requestID, seq, lab, customer)
	}

	log.Printf("import-lpitrack-history: итог: создано=%d пропущено=%d по_годам=%v режим=%s",
		created, skipped, byYear, mode)
}

// nextSeqTx — то же самое, что nextSeq (requests.go:20), но через переданную
// транзакцию, а не s.pool напрямую. nextSeq не годится для -dry-run: его
// UPDATE запроса всегда идёт мимо tx, поэтому откат транзакции не отменяет
// продвижение счётчика request_seq.
func nextSeqTx(ctx context.Context, tx pgx.Tx, year int) (int64, error) {
	var v int64
	err := tx.QueryRow(ctx, `
INSERT INTO request_seq (seq_year, last_value) VALUES ($1, 1)
ON CONFLICT (seq_year) DO UPDATE SET last_value = request_seq.last_value + 1
RETURNING last_value`, year).Scan(&v)
	return v, err
}

// importOneLpitrackRequest вставляет один объект+заявку в отдельной транзакции
// (чтобы одна проблемная запись не откатывала всю партию). При dryRun=true
// транзакция всегда откатывается (через defer tx.Rollback), но номера/id всё
// равно возвращаются для лога — это реальные значения, которые вернула БД в
// рамках незакоммиченной транзакции (nextSeq — тоже часть этой транзакции, так
// что при dry-run счётчик request_seq НЕ продвигается).
func importOneLpitrackRequest(ctx context.Context, s *Server, rec lpitrackImportRecord,
	dateIn time.Time, year int, projectID int64, dryRun bool) (int64, int64, string, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Та же защита от дублей, что у живого приёма почты (email_ingest.go,
	// applyApplicationEmail) — если внешний ID уже есть, ничего не делаем.
	var existingID int64
	err = tx.QueryRow(ctx, `SELECT id FROM requests WHERE external_id = $1`, rec.ExternalID).Scan(&existingID)
	if err == nil {
		return 0, 0, "", "", fmt.Errorf("заявка с external_id уже существует (id=%d)", existingID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", "", fmt.Errorf("check existing external_id: %w", err)
	}

	mlr, err := loadMethodLabRow(ctx, tx, rec.MethodID, importLabID)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("метод %d не привязан к лабе %d: %w", rec.MethodID, importLabID, err)
	}

	productName := strings.TrimSpace(rec.ProductName)
	if productName == "" {
		productName = "Без названия"
	}
	ekn := strings.TrimSpace(rec.Ekn)
	description := strings.TrimSpace(rec.Description)
	if description == "" {
		description = rec.AimIndicator
	}

	// Тот же состав characteristics, что строит applyApplicationEmail
	// (email_ingest.go) для живых писем — чтобы объекты выглядели одинаково
	// независимо от источника (перенос истории vs живой приём).
	characteristics := map[string]any{
		"ekn":              ekn,
		"thickness_mm":     rec.Thickness,
		"material_type":    rec.MaterialType,
		"target_indicator": rec.AimIndicator,
	}
	if ekn != "" {
		characteristics["sample_type"] = "series"
		if n, ok := parseIntLoose(rec.BatchNumber); ok {
			characteristics["batch_number"] = n
		}
	} else {
		characteristics["sample_type"] = "experimental"
	}
	charsJSON, err := json.Marshal(characteristics)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("marshal characteristics: %w", err)
	}

	var objectID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO objects (name, description, characteristics) VALUES ($1, '', $2::jsonb)
RETURNING id`, productName, string(charsJSON)).Scan(&objectID); err != nil {
		return 0, 0, "", "", fmt.Errorf("create object: %w", err)
	}

	// ВАЖНО: nextSeq (requests.go) пишет через s.pool напрямую, не через tx —
	// при dry-run это НЕ откатится вместе с остальной транзакцией. Здесь нужен
	// строго транзакционный вариант, чтобы -dry-run был по-настоящему без
	// побочных эффектов (см. история багфикса, план 2026-08-21).
	seq, err := nextSeqTx(ctx, tx, year)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("nextSeqTx: %w", err)
	}
	customer, lab := buildNumbers(seq, year, importProjectCode, mlr.labCode, mlr.methodCode)

	var requestID int64
	err = tx.QueryRow(ctx, `
INSERT INTO requests (number_seq, number_year, title, description, object_id, project_id,
	owner_email, status, priority, method_id, lab_id, customer_number, lab_number, external_id,
	created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'completed', $8, $9, $10, $11, $12, $13, $14, $14)
RETURNING id`,
		seq, year, productName, description, objectID, projectID,
		strings.TrimSpace(rec.CustMail), mapEmailPriority(rec.PriorityRaw), rec.MethodID, importLabID,
		customer, lab, rec.ExternalID, dateIn).Scan(&requestID)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("create request: %w", err)
	}

	if dryRun {
		return requestID, seq, customer, lab, nil // откатится через defer tx.Rollback
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, "", "", fmt.Errorf("commit: %w", err)
	}
	return requestID, seq, customer, lab, nil
}
