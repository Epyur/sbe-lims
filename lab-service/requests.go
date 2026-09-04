package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Нумерация заявок ----

// nextSeq выдаёт следующий глобальный по году номер NNN.
func (s *Server) nextSeq(ctx context.Context, year int) (int64, error) {
	var v int64
	err := s.pool.QueryRow(ctx, `
INSERT INTO request_seq (seq_year, last_value) VALUES ($1, 1)
ON CONFLICT (seq_year) DO UPDATE SET last_value = request_seq.last_value + 1
RETURNING last_value`, year).Scan(&v)
	return v, err
}

// formatSeq форматирует NNN: простое значение сквозного счётчика по году.
func formatSeq(seq int64) string {
	return strconv.FormatInt(seq, 10)
}

// buildNumbers строит пару номеров (заказчику / лаборатории) для пары (заявка, метод).
func buildNumbers(seq int64, year int, projectCode, labCode, methodCode string) (string, string) {
	seqStr := formatSeq(seq)
	customer := fmt.Sprintf("%s-%s/%d-%s-%s", projectCode, seqStr, year, labCode, methodCode)
	lab := fmt.Sprintf("%s/%d-%s", seqStr, year, methodCode)
	return customer, lab
}

// nullableID возвращает NULL для неположительных id (0 = отсутствует) — для FK-колонок.
func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// ---- Типы ----

type RequestFile struct {
	FileKey  string `json:"file_key"`
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size"`
	FileURL  string `json:"file_url"`
}

// Request — LabID (2026-08-19) заменяет старую ExternalLabID: методы теперь могут
// принадлежать нескольким лабораториям (method_labs), поэтому заявка обязана явно
// зафиксировать, КАКАЯ конкретно лаба из списка метода выполняет испытание — именно
// LabID определяет и лабораторный код в номере (buildNumbers), и видимость заявки
// (requestVisible/visibleRequestsQuery), заменяя оба прежних механизма (methods.lab_id
// и отдельный чекбокс "внешняя лаборатория"). Колонка requests.external_lab_id в БД
// не удалена (исторический артефакт, как request_methods в декомпозиции 2026-08-18),
// но больше не читается/не пишется.
type Request struct {
	ID             int64  `json:"id"`
	NumberSeq      int64  `json:"number_seq"`
	NumberYear     int    `json:"number_year"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	ObjectID       int64  `json:"object_id"`
	ProjectID      int64  `json:"project_id"`
	GroupID        int64  `json:"group_id"`
	OwnerEmail     string `json:"owner_email"`
	Status         string `json:"status"`
	Priority       string `json:"priority"`
	TestPurpose    string `json:"test_purpose"`
	EKN            string `json:"ekn"`
	MethodID       int64  `json:"method_id"`
	LabID          int64  `json:"lab_id"`
	CustomerNumber string `json:"customer_number"`
	LabNumber      string `json:"lab_number"`
	ExternalID     string `json:"external_id"`
	// Системные атрибуты (2026-08-23) — общие для любого метода (см. results.go/
	// protocol.go resolveSystemPlaceholder), заполняются автоматически при приёме
	// письма-результата (email_ingest.go); не MethodAttribute, не настраиваются
	// per-method (см. sbe-lims/AGENTS.md, "Системные атрибуты").
	InventorID    int64  `json:"inventor_id"`
	ReportDate    string `json:"report_date"`
	SamplesInDate string `json:"samples_in_date"`
	ExpDate       string `json:"exp_date"`
	AmbTemp       string `json:"amb_temp"`
	AmbPres       string `json:"amb_pres"`
	AmbMoist      string `json:"amb_moist"`
	// AssignedTo/CompletedAt — Kanban-доска «Очередь лаборатории» (2026-08-24,
	// см. kanban.go). AssignedTo — email испытателя (lab_members.email,
	// lab_operator/lab_admin лабы заявки); назначает/переназначает только
	// руководитель лабы, либо испытатель может забрать СЕБЕ неназначенную
	// заявку из "new". CompletedAt — момент перехода в status="completed"
	// (не UpdatedAt) — основа 10-рабочедневного окна показа в колонке
	// "Завершённые".
	AssignedTo  string        `json:"assigned_to"`
	CompletedAt string        `json:"completed_at"`
	Files       []RequestFile `json:"files"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
	// Result/Compliance (2026-09-04) — НЕ колонки requests, а производные значения,
	// подставляемые enrichResultCompliance после загрузки: "Результат" (сырая
	// достигнутая оценка метода, напр. "Г4") и "Соответствие" ("Соответствует"/
	// "Не соответствует"/"Не оценивается") — см. resolveResultCompliance (results.go)
	// и AGENTS.md. Пусто, если у метода нет compliance-правила или результаты ещё
	// не посчитаны.
	Result     string `json:"result"`
	Compliance string `json:"compliance"`
}

// requestColumnsSQL — колонки requests в порядке, ожидаемом scanRequestRow. Единая
// точка (2026-08-23) — раньше loadRequest/visibleRequestsQuery (x2) держали три
// независимые копии этого списка, что уже приводило к риску расхождения в других
// местах проекта (см. parseMethodPresentation) при добавлении новых колонок.
const requestColumnsSQL = `id, number_seq, number_year, title, description, COALESCE(object_id, 0),
	COALESCE(project_id, 0), COALESCE(group_id, 0), owner_email, status, priority,
	test_purpose, ekn,
	COALESCE(method_id, 0), COALESCE(lab_id, 0), customer_number, lab_number, external_id,
	COALESCE(inventor_id, 0), COALESCE(report_date, ''), COALESCE(samples_in_date, ''),
	COALESCE(exp_date, ''), COALESCE(amb_temp, ''), COALESCE(amb_pres, ''), COALESCE(amb_moist, ''),
	assigned_to, completed_at,
	created_at, updated_at`

// rowScanner — общий интерфейс pgx.Row (QueryRow) и pgx.Rows (Query, в цикле Next()).
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRequestRow сканирует одну строку requests (порядок колонок — requestColumnsSQL)
// в Request; created_at/updated_at возвращаются отдельно (time.Time), т.к. Request
// хранит их уже отформatированными строками (RFC3339) — форматирование остаётся на
// вызывающей стороне, как и раньше.
func scanRequestRow(row rowScanner) (Request, time.Time, time.Time, error) {
	var req Request
	var ca, ua time.Time
	var completedAt *time.Time
	err := row.Scan(&req.ID, &req.NumberSeq, &req.NumberYear, &req.Title,
		&req.Description, &req.ObjectID, &req.ProjectID, &req.GroupID, &req.OwnerEmail,
		&req.Status, &req.Priority, &req.TestPurpose, &req.EKN,
		&req.MethodID, &req.LabID, &req.CustomerNumber, &req.LabNumber, &req.ExternalID,
		&req.InventorID, &req.ReportDate, &req.SamplesInDate,
		&req.ExpDate, &req.AmbTemp, &req.AmbPres, &req.AmbMoist,
		&req.AssignedTo, &completedAt,
		&ca, &ua)
	if err == nil && completedAt != nil {
		req.CompletedAt = completedAt.Format(time.RFC3339)
	}
	return req, ca, ua, err
}

// projectInfo — сведения проекта для построения номера заказчику.
type projectInfo struct {
	code string
}

func (s *Server) loadProjectInfo(ctx context.Context, projectID int64) (projectInfo, error) {
	if projectID <= 0 {
		return projectInfo{code: "0"}, nil
	}
	var pi projectInfo
	err := s.pool.QueryRow(ctx,
		`SELECT code FROM projects WHERE id = $1`, projectID).Scan(&pi.code)
	if err != nil {
		return projectInfo{}, err
	}
	return pi, nil
}

// ensureEknProject находит проект с code = ekn; если нет — создаёт (is_ekn=true).
// Возвращает project_id (0 при пустом ekn). Используется, когда у заявки нет проекта, но указан ЕКН.
func (s *Server) ensureEknProject(ctx context.Context, tx pgx.Tx, ekn string) (int64, error) {
	return s.ensureProjectByCode(ctx, tx, ekn, true)
}

// ensureProjectByCode находит проект с code = code; если нет — создаёт. isEkn
// помечает проект как автоматически созданный из ЕКН письма (см. ensureEknProject
// выше) — для проекта по умолчанию почтового приёма БЕЗ ЕКН (2026-08-29,
// email_ingest.go applyApplicationEmail, LAB_MAIL_DEFAULT_PROJECT_CODE) isEkn=false
// (это не ЕКН-проект, просто общий "накопитель" для писем без указанного ЕКН).
func (s *Server) ensureProjectByCode(ctx context.Context, tx pgx.Tx, code string, isEkn bool) (int64, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return 0, nil
	}
	var projectID int64
	err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE code = $1`, code).Scan(&projectID)
	if err == nil {
		return projectID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRow(ctx, `
INSERT INTO projects (code, name, is_ekn, owner_email) VALUES ($1, $1, $2, $3)
RETURNING id`, code, isEkn, "").Scan(&projectID)
	if err != nil {
		return 0, err
	}
	return projectID, nil
}

// findProjectByMailTrigger ищет проект с явным триггером почтового приёма
// (2026-09-02, прямой запрос пользователя): mail_trigger_ekn — точное
// совпадение ЕКН письма; mail_trigger_sender — отправитель (точный email)
// или домен (значение вида "@company.ru", суффиксное совпадение). ЕКН-триггер
// проверяется первым (более узкое/конкретное правило важнее правила по
// отправителю/домену). Совпадений нет — (0, "", nil), вызывающий код падает
// на общие правила (свой автопроект по ЕКН, затем EML по умолчанию).
func (s *Server) findProjectByMailTrigger(ctx context.Context, tx pgx.Tx, ekn, custMail string) (int64, string, error) {
	if ekn != "" {
		var id int64
		var code string
		err := tx.QueryRow(ctx, `
SELECT id, code FROM projects WHERE mail_trigger_ekn <> '' AND mail_trigger_ekn = $1 LIMIT 1`, ekn).Scan(&id, &code)
		if err == nil {
			return id, code, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, "", err
		}
	}

	custMail = strings.ToLower(strings.TrimSpace(custMail))
	if custMail == "" {
		return 0, "", nil
	}
	rows, err := tx.Query(ctx, `
SELECT id, code, mail_trigger_sender FROM projects WHERE mail_trigger_sender <> '' ORDER BY id`)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var code, sender string
		if err := rows.Scan(&id, &code, &sender); err != nil {
			return 0, "", err
		}
		if matchesMailSenderTrigger(sender, custMail) {
			return id, code, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	return 0, "", nil
}

// matchesMailSenderTrigger — trigger вида "@company.ru" матчит по суффиксу
// домена, иначе — точное совпадение адреса. custMail уже приведён к нижнему
// регистру и обрезан вызывающим кодом.
func matchesMailSenderTrigger(trigger, custMail string) bool {
	trigger = strings.ToLower(strings.TrimSpace(trigger))
	if strings.HasPrefix(trigger, "@") {
		return strings.HasSuffix(custMail, trigger)
	}
	return custMail == trigger
}

// loadRequestFiles возвращает файлы заявки.
func (s *Server) loadRequestFiles(ctx context.Context, requestID int64) ([]RequestFile, error) {
	rows, err := s.pool.Query(ctx, `
SELECT file_key, file_name, file_size, file_url FROM files WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	files := make([]RequestFile, 0, 8)
	for rows.Next() {
		var f RequestFile
		if err := rows.Scan(&f.FileKey, &f.FileName, &f.FileSize, &f.FileURL); err != nil {
			log.Printf("request files scan: %v", err)
			continue
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// loadRequest загружает заявку целиком (метод и номера — прямо в строке, файлы отдельно).
func (s *Server) loadRequest(ctx context.Context, id int64) (*Request, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+requestColumnsSQL+" FROM requests WHERE id = $1", id)
	req, ca, ua, err := scanRequestRow(row)
	if err != nil {
		return nil, err
	}
	req.CreatedAt = ca.Format(time.RFC3339)
	req.UpdatedAt = ua.Format(time.RFC3339)
	if req.Files, err = s.loadRequestFiles(ctx, id); err != nil {
		return nil, err
	}
	if req.Files == nil {
		req.Files = []RequestFile{}
	}
	return &req, nil
}

// requestVisible проверяет правило видимости: owner ИЛИ участник группы заявки
// ИЛИ сотрудник лаборатории заявки (lab_members по req.LabID) (admin — всегда).
// Если лаба заявки внешняя (расширяет возможности внутренней, сама не имеет
// lab_members — там нет пользователей этой системы), видимость резолвится через
// её parent_lab_id (внешняя лаба не существует самостоятельно).
func (s *Server) requestVisible(ctx context.Context, req *Request, email string) (bool, error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return false, err
	}
	if roleRank(role) >= roleRank("admin") {
		return true, nil
	}
	if req.OwnerEmail == email {
		return true, nil
	}
	if req.GroupID > 0 {
		var inGroup bool
		err = s.pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM group_members WHERE group_id = $1 AND email = $2)`,
			req.GroupID, email).Scan(&inGroup)
		if err != nil {
			return false, err
		}
		if inGroup {
			return true, nil
		}
	}
	if req.LabID <= 0 {
		return false, nil
	}
	var inLab bool
	err = s.pool.QueryRow(ctx, `
SELECT EXISTS(
	SELECT 1 FROM labs l
	JOIN lab_members lm ON lm.lab_id = COALESCE(l.parent_lab_id, l.id)
	WHERE l.id = $1 AND lm.email = $2)`,
		req.LabID, email).Scan(&inLab)
	if err != nil {
		return false, err
	}
	return inLab, nil
}

// visibleRequestsQuery возвращает запрос с фильтром видимости: owner ИЛИ участник
// группы заявки ИЛИ сотрудник лаборатории заявки (lab_members по lab_id) (admin — все).
// Внешние лабы резолвятся через parent_lab_id — см. requestVisible.
func (s *Server) visibleRequestsQuery(ctx context.Context, email string) (rows pgx.Rows, err error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return nil, err
	}
	if roleRank(role) >= roleRank("admin") {
		return s.pool.Query(ctx, "SELECT "+requestColumnsSQL+" FROM requests ORDER BY id")
	}
	return s.pool.Query(ctx, `SELECT `+requestColumnsSQL+`
FROM requests
WHERE owner_email = $1
	OR group_id IN (SELECT group_id FROM group_members WHERE email = $1)
	OR lab_id IN (
		SELECT l.id FROM labs l
		JOIN lab_members lm ON lm.lab_id = COALESCE(l.parent_lab_id, l.id)
		WHERE lm.email = $1)
ORDER BY id`, email)
}

// loadVisibleRequests загружает видимые заявки целиком.
func (s *Server) loadVisibleRequests(ctx context.Context, email string) ([]Request, error) {
	rows, err := s.visibleRequestsQuery(ctx, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	requests := make([]Request, 0, 16)
	for rows.Next() {
		req, ca, ua, err := scanRequestRow(rows)
		if err != nil {
			log.Printf("requests scan: %v", err)
			continue
		}
		req.CreatedAt = ca.Format(time.RFC3339)
		req.UpdatedAt = ua.Format(time.RFC3339)
		if req.Files, err = s.loadRequestFiles(ctx, req.ID); err != nil {
			return nil, err
		}
		if req.Files == nil {
			req.Files = []RequestFile{}
		}
		requests = append(requests, req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.enrichResultCompliance(ctx, requests); err != nil {
		return nil, err
	}
	return requests, nil
}

// enrichResultCompliance заполняет Request.Result/Compliance по каждой заявке —
// общая точка для handleListRequests и handlePull (оба идут через
// loadVisibleRequests, см. выше). Не N+1: MethodConfig и resolveResultCompliance
// кэшируются по method_id (в видимом наборе заявок методов немного), а
// aggregated_results читается ОДНИМ батч-запросом по всем id заявок сразу.
// Источник — та же строка (request_id, method_id, calculation_type=
// 'formula_aggregated'), куда applyAggregatedFormulas пишет и aggregated-формулы,
// И classification-выходы уровня "aggregated" (applyAggregatedClassification
// мутирует тот же result перед записью, см. results.go) — т.е. compliance уже
// там, если у метода вообще есть aggregated-правило классификации.
func (s *Server) enrichResultCompliance(ctx context.Context, requests []Request) error {
	if len(requests) == 0 {
		return nil
	}
	type resultCompliancePair struct{ resultID, complianceID string }
	pairByMethod := make(map[int64]resultCompliancePair, 8)
	ids := make([]int64, 0, len(requests))
	for _, req := range requests {
		ids = append(ids, req.ID)
		if req.MethodID <= 0 {
			continue
		}
		if _, ok := pairByMethod[req.MethodID]; ok {
			continue
		}
		cfg, err := s.loadMethodConfig(ctx, req.MethodID)
		if err != nil {
			log.Printf("enrichResultCompliance: loadMethodConfig method=%d: %v", req.MethodID, err)
			pairByMethod[req.MethodID] = resultCompliancePair{}
			continue
		}
		resultID, complianceID := resolveResultCompliance(cfg)
		pairByMethod[req.MethodID] = resultCompliancePair{resultID, complianceID}
	}

	rows, err := s.pool.Query(ctx, `
SELECT request_id, result_data FROM aggregated_results
WHERE calculation_type = 'formula_aggregated' AND request_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	dataByRequest := make(map[int64]map[string]any, len(ids))
	for rows.Next() {
		var requestID int64
		var dataRaw []byte
		if err := rows.Scan(&requestID, &dataRaw); err != nil {
			log.Printf("enrichResultCompliance scan: %v", err)
			continue
		}
		data := map[string]any{}
		if len(dataRaw) > 0 && string(dataRaw) != "{}" {
			_ = json.Unmarshal(dataRaw, &data)
		}
		dataByRequest[requestID] = data
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range requests {
		pair, ok := pairByMethod[requests[i].MethodID]
		if !ok || pair.resultID == "" {
			continue
		}
		data := dataByRequest[requests[i].ID]
		requests[i].Result, _ = data[pair.resultID].(string)
		if pair.complianceID != "" {
			requests[i].Compliance, _ = data[pair.complianceID].(string)
		}
	}
	return nil
}

// ---- Handlers ----

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	email := currentEmail(r)
	requests, err := s.loadVisibleRequests(r.Context(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	req, err := s.loadRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	visible, err := s.requestVisible(r.Context(), req, currentEmail(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if !visible {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": req})
}

// methodLabRow — код метода/лабы для пары (method_id, lab_id), проверенной по method_labs.
type methodLabRow struct {
	methodID, labID     int64
	methodCode, labCode string
}

// loadMethodLabRow проверяет, что lab_id действительно привязан к методу (method_labs),
// и возвращает коды обоих для нумерации (buildNumbers).
func loadMethodLabRow(ctx context.Context, q pgx.Tx, methodID, labID int64) (methodLabRow, error) {
	var row methodLabRow
	row.methodID, row.labID = methodID, labID
	err := q.QueryRow(ctx, `
SELECT m.code, l.code
FROM methods m
JOIN method_labs ml ON ml.method_id = m.id AND ml.lab_id = $2
JOIN labs l ON l.id = ml.lab_id
WHERE m.id = $1`, methodID, labID).Scan(&row.methodCode, &row.labCode)
	return row, err
}

func (s *Server) handleCreateRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		ObjectID    int64  `json:"object_id"`
		ProjectID   int64  `json:"project_id"`
		GroupID     int64  `json:"group_id"`
		Priority    string `json:"priority"`
		TestPurpose string `json:"test_purpose"`
		EKN         string `json:"ekn"`
		ExternalID  string `json:"external_id"`
		Methods     []struct {
			MethodID int64 `json:"method_id"`
			LabID    int64 `json:"lab_id"`
		} `json:"methods"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}
	if req.ObjectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "object_id is required"})
		return
	}
	if len(req.Methods) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one method is required"})
		return
	}
	for _, m := range req.Methods {
		if m.MethodID <= 0 || m.LabID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "each method requires method_id and lab_id"})
			return
		}
	}

	pi, err := s.loadProjectInfo(r.Context(), req.ProjectID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "project not found"})
		return
	}
	year := time.Now().UTC().Year()
	seq, err := s.nextSeq(r.Context(), year)
	if err != nil {
		log.Printf("nextSeq: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	email := currentEmail(r)
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer tx.Rollback(r.Context())

	effProjectID := req.ProjectID
	if req.ProjectID <= 0 && req.EKN != "" {
		effProjectID, err = s.ensureEknProject(r.Context(), tx, req.EKN)
		if err != nil {
			log.Printf("ensureEknProject: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if effProjectID > 0 {
			// Автопроект создан в tx — loadProjectInfo через пул его не увидит.
			// Код проекта = номер ЕКН (как в ensureEknProject).
			pi.code = req.EKN
		}
	}

	// Валидируем пары (method_id, lab_id) по method_labs и строим номера ДО вставки
	// (свой метод+лаба — своя строчка).
	type mRow struct {
		mid, lid      int64
		mCode, lCode  string
		customer, lab string
	}
	methods := make([]mRow, 0, len(req.Methods))
	for _, sel := range req.Methods {
		mlr, err := loadMethodLabRow(r.Context(), tx, sel.MethodID, sel.LabID)
		if err != nil {
			log.Printf("create request: invalid method_id/lab_id %d/%d: %v", sel.MethodID, sel.LabID, err)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid method_id/lab_id combination"})
			return
		}
		m := mRow{mid: mlr.methodID, lid: mlr.labID, mCode: mlr.methodCode, lCode: mlr.labCode}
		m.customer, m.lab = buildNumbers(seq, year, pi.code, m.lCode, m.mCode)
		methods = append(methods, m)
	}
	// Дедуп по (method_id, lab_id) в одном запросе.
	seen := map[[2]int64]bool{}
	uniq := methods[:0]
	for _, m := range methods {
		key := [2]int64{m.mid, m.lid}
		if !seen[key] {
			seen[key] = true
			uniq = append(uniq, m)
		}
	}
	methods = uniq

	// Один запрос → N под-заявок с общим NNN (одинаковые number_seq + number_year).
	created := make([]*Request, 0, len(methods))
	for _, m := range methods {
		var id int64
		err := tx.QueryRow(r.Context(), `
INSERT INTO requests (number_seq, number_year, title, description, object_id, project_id,
	group_id, owner_email, status, priority, test_purpose, ekn,
	method_id, lab_id, customer_number, lab_number, external_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'new', $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING id`,
			seq, year, req.Title, req.Description, req.ObjectID, nullableID(effProjectID),
			nullableID(req.GroupID), email, req.Priority, req.TestPurpose, req.EKN,
			m.mid, m.lid, m.customer, m.lab, req.ExternalID).Scan(&id)
		if err != nil {
			log.Printf("create request: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		created = append(created, &Request{ID: id})
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Printf("create request commit: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	full := make([]*Request, 0, len(created))
	for _, c := range created {
		f, err := s.loadRequest(r.Context(), c.ID)
		if err != nil {
			log.Printf("create request load: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		full = append(full, f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": full})
}

func (s *Server) handleUpdateRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		ObjectID    *int64  `json:"object_id"`
		ProjectID   *int64  `json:"project_id"`
		GroupID     *int64  `json:"group_id"`
		Priority    *string `json:"priority"`
		TestPurpose *string `json:"test_purpose"`
		EKN         *string `json:"ekn"`
		ExternalID  *string `json:"external_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	existing, err := s.loadRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	email := currentEmail(r)
	visible, err := s.requestVisible(r.Context(), existing, email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if !visible || existing.OwnerEmail != email {
		role, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if existing.OwnerEmail != email && roleRank(role) < roleRank("admin") {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not owner"})
			return
		}
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer tx.Rollback(r.Context())

	effProjectID := int64(0)
	if existing.ProjectID > 0 {
		effProjectID = existing.ProjectID
	}
	if req.ProjectID != nil {
		effProjectID = *req.ProjectID
	}
	if effProjectID <= 0 && req.EKN != nil && strings.TrimSpace(*req.EKN) != "" {
		effProjectID, err = s.ensureEknProject(r.Context(), tx, *req.EKN)
		if err != nil {
			log.Printf("ensureEknProject (update): %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}

	_, err = tx.Exec(r.Context(), `
UPDATE requests SET
	title = COALESCE($2, title),
	description = COALESCE($3, description),
	object_id = CASE WHEN $4 = 0 THEN NULL ELSE COALESCE($4, object_id) END,
	project_id = CASE WHEN $5 = 0 THEN NULL ELSE COALESCE($5, project_id) END,
	group_id = CASE WHEN $6 = 0 THEN NULL ELSE COALESCE($6, group_id) END,
	priority = COALESCE($7, priority),
	test_purpose = COALESCE($8, test_purpose),
	ekn = COALESCE($9, ekn),
	external_id = COALESCE($10, external_id),
	updated_at = now()
WHERE id = $1`, id, req.Title, req.Description, req.ObjectID, req.ProjectID, req.GroupID,
		req.Priority, req.TestPurpose, req.EKN, req.ExternalID)
	if err != nil {
		log.Printf("update request: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	full, err := s.loadRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": full})
}

func (s *Server) handleSetRequestStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if req.Status != "new" && req.Status != "received" && req.Status != "processing" && req.Status != "completed" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status must be new, received, processing or completed"})
		return
	}
	email := currentEmail(r)
	existing, err := s.loadRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
		return
	}
	// Write-право (2026-08-26, ревью безопасности — известный документированный пробел):
	// раньше проверялась только ВИДИМОСТЬ (requestVisible) — участник группы без роли
	// lab_operator/lab_admin/владельца мог сменить статус ЛЮБОЙ видимой ему заявки.
	// requireLabAccess — та же проверка, что уже используется для ввода результатов/
	// расчёта этой же заявки (её собственный комментарий явно упоминает "статус" в
	// списке действий, которые она защищает — этот эндпоинт единственный, что её не
	// вызывал). Владелец заявки сохраняет право менять статус своей заявки — сервис
	// общий с sbe-requests, где именно владелец инициирует переходы.
	canChangeStatus := existing.OwnerEmail == email
	if !canChangeStatus {
		canChangeStatus, err = s.requireLabAccess(r.Context(), email, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}
	if !canChangeStatus {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not owner or lab staff"})
		return
	}
	_, err = s.pool.Exec(r.Context(), `
UPDATE requests SET
	status = $2,
	assigned_to = CASE WHEN $2 = 'new' THEN '' ELSE assigned_to END,
	completed_at = CASE
		WHEN $2 = 'completed' AND status <> 'completed' THEN now()
		WHEN $2 <> 'completed' THEN NULL
		ELSE completed_at
	END,
	updated_at = now()
WHERE id = $1`, id, req.Status)
	if err != nil {
		log.Printf("set status: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	// WP2 (2026-08-28): автоотправка писем при РЕАЛЬНОМ переходе в completed (не при
	// повторном сохранении уже completed-заявки) — та же проверка, что и в SQL CASE выше.
	// Best-effort — не блокирует ответ, ошибки только в лог/журнал (см. outbound_email.go).
	if req.Status == "completed" && existing.Status != "completed" {
		go s.triggerCompletionEmails(context.WithoutCancel(r.Context()), id)
	}
	// WP2 (2026-08-29): та же автоотправка, только при РЕАЛЬНОМ переходе в processing —
	// см. triggerProcessingEmail. assigned_to этот эндпоинт не меняет (только чистит на
	// "new") — существующее значение уже актуально к моменту вызова.
	if req.Status == "processing" && existing.Status != "processing" {
		go s.triggerProcessingEmail(context.WithoutCancel(r.Context()), id, existing.AssignedTo)
	}
	// WP8 (2026-08-29): журнал изменений — та же best-effort дисциплина, что и письма
	// выше (см. audit_log.go). logStatusChange сама не пишет строку при old==new.
	s.logStatusChange(r.Context(), id, currentEmail(r), existing.Status, req.Status)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetTargetIndicator — узкий эндпоинт (2026-09-04, по прямому запросу
// пользователя): заказчик (владелец заявки) сам исправляет ОТСУТСТВУЮЩИЙ
// целевой показатель объекта для метода этой заявки (objects.characteristics.
// target_indicators[method_id], см. loadTargetIndicator), не имея editor-доступа
// к объекту в целом. В отличие от handleUpdateObject (references.go — полная
// замена characteristics целиком, editor-only) делает ТОЧЕЧНЫЙ jsonb_set только
// по одному ключу — остальные поля characteristics (ekn/batch_number/...) не
// затрагиваются. Разрешает запись, только если показатель ещё НЕ задан — правка
// уже заданного значения остаётся editor+ действием через handleUpdateObject
// (409, если уже есть). После записи пересчитывает формулы/классификацию тем же
// путём, что handleCalculateSeries (recalcRequestMethod, recalc_all.go), чтобы
// compliance ("Не оценивается" → оценка) в уже сохранённых результатах сразу
// обновился без отдельного действия лаборанта.
func (s *Server) handleSetTargetIndicator(w http.ResponseWriter, r *http.Request) {
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Indicator string `json:"indicator"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	req.Indicator = strings.TrimSpace(req.Indicator)
	if req.Indicator == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "indicator is required"})
		return
	}

	ctx := r.Context()
	var ownerEmail string
	var objectID, methodID int64
	err = s.pool.QueryRow(ctx,
		`SELECT owner_email, object_id, method_id FROM requests WHERE id = $1`, requestID).
		Scan(&ownerEmail, &objectID, &methodID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Владелец заявки правит свой же показатель; сотрудник лаборатории заявки
	// (requireLabAccess — та же граница, что и у расчёта/статуса выше) — тоже,
	// на случай если он вводит его по просьбе заказчика.
	email := currentEmail(r)
	canEdit := email != "" && email == ownerEmail
	if !canEdit {
		canEdit, err = s.requireLabAccess(ctx, email, requestID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}
	if !canEdit {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not owner or lab staff"})
		return
	}

	rankOrder, err := s.loadMethodRankOrder(ctx, methodID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if indexOfString(rankOrder, req.Indicator) < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "недопустимый целевой показатель для этого метода"})
		return
	}

	existing, err := s.loadTargetIndicator(ctx, requestID, methodID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if existing != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "целевой показатель уже задан — правка через справочник объектов"})
		return
	}

	if _, err := s.pool.Exec(ctx, `
UPDATE objects SET
	characteristics = jsonb_set(coalesce(characteristics, '{}'::jsonb), ARRAY['target_indicators', $1], to_jsonb($2::text), true),
	updated_at = now()
WHERE id = $3`, strconv.FormatInt(methodID, 10), req.Indicator, objectID); err != nil {
		log.Printf("set target indicator: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Показатель уже сохранён к этому моменту — ошибка пересчёта ниже не должна
	// выглядеть как "показатель не сохранился" (неправда). Логируем и сообщаем
	// об этом отдельным полем, не трогая "ok" (мягкая деградация, как best-effort
	// шаги в handleDeleteResultSeries/WP2-почта — см. их комментарии).
	if err := s.recalcRequestMethod(ctx, requestID, methodID, email); err != nil {
		log.Printf("set target indicator: recompute request %d method %d: %v", requestID, methodID, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"warning": "показатель сохранён, но автоматический пересчёт не выполнен — обратитесь в лабораторию",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
