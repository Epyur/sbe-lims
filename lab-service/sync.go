package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// ---- Pull: полный слепок для кэша плагина ----

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	email := currentEmail(r)

	requests, err := s.loadVisibleRequests(r.Context(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	projects, err := s.loadVisibleProjects(r.Context(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	groups := make([]Group, 0, 16)
	rows, err := s.pool.Query(r.Context(), `
SELECT id, name, owner_email, created_at, updated_at
FROM groups
WHERE owner_email = $1 OR id IN (SELECT group_id FROM group_members WHERE email = $1)
ORDER BY id`, email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	for rows.Next() {
		var g Group
		var ca, ua time.Time
		if err := rows.Scan(&g.ID, &g.Name, &g.OwnerEmail, &ca, &ua); err != nil {
			log.Printf("pull groups scan: %v", err)
			continue
		}
		g.CreatedAt = ca.Format(time.RFC3339)
		g.UpdatedAt = ua.Format(time.RFC3339)
		if g.Members, err = s.loadGroupMembers(r.Context(), g.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	labs := make([]Lab, 0, 16)
	rows, err = s.pool.Query(r.Context(), `
SELECT id, code, name, description, type, COALESCE(parent_lab_id, 0), created_at, updated_at
FROM labs ORDER BY id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	for rows.Next() {
		var l Lab
		var ca, ua time.Time
		if err := rows.Scan(&l.ID, &l.Code, &l.Name, &l.Description, &l.Type, &l.ParentLabID, &ca, &ua); err != nil {
			log.Printf("pull labs scan: %v", err)
			continue
		}
		l.CreatedAt = ca.Format(time.RFC3339)
		l.UpdatedAt = ua.Format(time.RFC3339)
		labs = append(labs, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	methods := make([]Method, 0, 16)
	rows, err = s.pool.Query(r.Context(), `
SELECT id, code, name, description, determinable_indicators, formulas, classification,
	chart_configs, input_parameters, presentation, operator_form,
	calibration_attributes, calibration_operator_form, created_at, updated_at FROM methods ORDER BY id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	for rows.Next() {
		var m Method
		var ca, ua time.Time
		var indsRaw, formulasRaw, classRaw, chartsRaw, inputsRaw, presRaw, opFormRaw, calibAttrsRaw, calibFormRaw []byte
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.Description, &indsRaw,
			&formulasRaw, &classRaw, &chartsRaw, &inputsRaw, &presRaw, &opFormRaw,
			&calibAttrsRaw, &calibFormRaw, &ca, &ua); err != nil {
			log.Printf("pull methods scan: %v", err)
			continue
		}
		m.DeterminableIndicators = []string{}
		if len(indsRaw) > 0 && string(indsRaw) != "[]" {
			_ = json.Unmarshal(indsRaw, &m.DeterminableIndicators)
		}
		m.Formulas = unmarshalJSONBArray(formulasRaw)
		m.Classification = unmarshalJSONBArray(classRaw)
		m.ChartConfigs = unmarshalJSONBArray(chartsRaw)
		m.InputParams = unmarshalJSONBArray(inputsRaw)
		m.Presentation = parseMethodPresentation(presRaw)
		m.OperatorForm = parseMethodOperatorForm(opFormRaw)
		m.CalibrationAttributes = unmarshalJSONBArray(calibAttrsRaw)
		m.CalibrationOperatorForm = parseMethodOperatorForm(calibFormRaw)
		m.CreatedAt = ca.Format(time.RFC3339)
		m.UpdatedAt = ua.Format(time.RFC3339)
		methods = append(methods, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	labsByMethod, err := s.loadMethodLabsMap(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	for i := range methods {
		methods[i].LabIDs = labsByMethod[methods[i].ID]
		if methods[i].LabIDs == nil {
			methods[i].LabIDs = []int64{}
		}
	}

	objects := make([]Object, 0, 16)
	rows, err = s.pool.Query(r.Context(), `
SELECT id, name, description, characteristics, created_at, updated_at FROM objects ORDER BY id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	for rows.Next() {
		var o Object
		var charsRaw []byte
		var ca, ua time.Time
		if err := rows.Scan(&o.ID, &o.Name, &o.Description, &charsRaw, &ca, &ua); err != nil {
			log.Printf("pull objects scan: %v", err)
			continue
		}
		o.Characteristics = map[string]any{}
		if len(charsRaw) > 0 && string(charsRaw) != "{}" {
			_ = json.Unmarshal(charsRaw, &o.Characteristics)
		}
		o.CreatedAt = ca.Format(time.RFC3339)
		o.UpdatedAt = ua.Format(time.RFC3339)
		objects = append(objects, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"requests": requests,
		"projects": projects,
		"groups":   groups,
		"labs":     labs,
		"methods":  methods,
		"objects":  objects,
	})
}

// ---- Push: локальные изменения (LWW по updated_at) ----

// PushRequest — LabID (2026-08-19) заменяет ExternalLabID: конкретная лаба из
// method_labs метода, выбранная при создании (см. requests.go, тип Request).
type PushRequest struct {
	ID          int64   `json:"id"`
	ClientID    int64   `json:"client_id"`
	GroupKey    string  `json:"group_key"`
	ParentID    int64   `json:"parent_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	ObjectID    int64   `json:"object_id"`
	ProjectID   int64   `json:"project_id"`
	GroupID     int64   `json:"group_id"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`
	TestPurpose string  `json:"test_purpose"`
	EKN         string  `json:"ekn"`
	ExternalID  string  `json:"external_id"`
	UpdatedAt   string  `json:"updated_at"`
	MethodID    int64   `json:"method_id"`
	LabID       int64   `json:"lab_id"`
	MethodIDs   []int64 `json:"method_ids"`
}

// resolvedMethodID возвращает единственный метод для заявки: предпочтителен
// MethodID (новая модель); для совместимости берётся первый из MethodIDs.
func (p PushRequest) resolvedMethodID() int64 {
	if p.MethodID > 0 {
		return p.MethodID
	}
	if len(p.MethodIDs) > 0 {
		return p.MethodIDs[0]
	}
	return 0
}

// CreatedPush — созданная сервером заявка с привязкой к локальному id клиента.
type CreatedPush struct {
	ClientID int64    `json:"client_id"`
	GroupKey string   `json:"group_key,omitempty"`
	Request  *Request `json:"request"`
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Requests []PushRequest `json:"requests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if len(req.Requests) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"inserted": 0, "updated": 0, "created": []CreatedPush{}})
		return
	}

	email := currentEmail(r)
	now := time.Now().UTC()
	inserted := 0
	updated := 0
	created := make([]CreatedPush, 0)

	updates := make([]PushRequest, 0, len(req.Requests))
	creates := make([]PushRequest, 0, len(req.Requests))
	for _, p := range req.Requests {
		if p.ID > 0 {
			updates = append(updates, p)
		} else {
			creates = append(creates, p)
		}
	}

	for _, p := range updates {
		ok, err := s.pushUpdate(r.Context(), p, email, now)
		if err != nil {
			log.Printf("push update %d: %v", p.ID, err)
			continue
		}
		if ok {
			updated++
		}
	}

	// Новой заявке (id = 0) сервер присваивает NNN. Если указан parent_id —
	// создаётся под-заявка с тем же NNN, что у родителя (только пока родитель
	// в статусе new; иначе — новый NNN). Без parent_id/group_key каждая — свой NNN.
	year := now.Year()
	groupSeq := map[string]int64{}
	resolveSeq := func(p PushRequest) (int64, int, bool) {
		if p.ParentID > 0 {
			if seq, y, ok := s.reuseParentNumber(r.Context(), p.ParentID, email); ok {
				return seq, y, true
			}
			log.Printf("push create %d: parent %d не переиспользуется (нет/не владелец/не new), новый NNN",
				p.ClientID, p.ParentID)
		}
		if p.GroupKey == "" {
			s, err := s.nextSeq(r.Context(), year)
			if err != nil {
				log.Printf("push create nextSeq: %v", err)
				return 0, year, false
			}
			return s, year, true
		}
		if gs, ok := groupSeq[p.GroupKey]; ok {
			return gs, year, true
		}
		s, err := s.nextSeq(r.Context(), year)
		if err != nil {
			log.Printf("push create nextSeq: %v", err)
			return 0, year, false
		}
		groupSeq[p.GroupKey] = s
		return s, year, true
	}
	for _, p := range creates {
		seq, y, ok := resolveSeq(p)
		if !ok {
			continue
		}

		full, err := s.pushCreate(r.Context(), p, email, now, seq, y)
		if err != nil {
			log.Printf("push create: %v", err)
			continue
		}
		if full == nil {
			continue
		}
		created = append(created, CreatedPush{ClientID: p.ClientID, GroupKey: p.GroupKey, Request: full})
		inserted++
	}

	writeJSON(w, http.StatusOK, map[string]any{"inserted": inserted, "updated": updated, "created": created})
}

func (s *Server) pushUpdate(ctx context.Context, p PushRequest, email string, now time.Time) (bool, error) {
	existing, err := s.loadRequest(ctx, p.ID)
	if err != nil {
		return false, err
	}
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return false, err
	}
	if existing.OwnerEmail != email && roleRank(role) < roleRank("admin") {
		return false, nil
	}
	if p.Status != "" && p.Status != "new" && p.Status != "received" && p.Status != "processing" && p.Status != "completed" {
		p.Status = existing.Status
	}
	updatedAt := parseTime(p.UpdatedAt, now)

	tag, err := s.pool.Exec(ctx, `
UPDATE requests SET
	title = $2, description = $3, object_id = $4, project_id = $5, group_id = $6,
	status = $7, priority = $8, test_purpose = $9, ekn = $10,
	external_id = $11, updated_at = $12,
	assigned_to = CASE WHEN $7 = 'new' THEN '' ELSE assigned_to END,
	completed_at = CASE
		WHEN $7 = 'completed' AND status <> 'completed' THEN now()
		WHEN $7 <> 'completed' THEN NULL
		ELSE completed_at
	END
WHERE id = $1 AND updated_at < $12`,
		p.ID, p.Title, p.Description, nullableID(p.ObjectID), nullableID(p.ProjectID),
		nullableID(p.GroupID), p.Status, p.Priority, p.TestPurpose, p.EKN,
		p.ExternalID, updatedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	// WP8 (2026-08-29): журнал изменений (см. audit_log.go) — офлайн-правка клиента,
	// синхронизированная позже, тоже реальная смена статуса пользователем.
	// logStatusChange сама не пишет строку, если p.Status == existing.Status.
	s.logStatusChange(ctx, p.ID, email, existing.Status, p.Status)
	return true, nil
}

// reuseParentNumber возвращает (seq, year) родительской заявки для под-заявки
// с тем же номером. Разрешено только для заявок в статусе new: владелец или
// admin. Иначе — (0, 0, false) → сервер выделит новый NNN.
func (s *Server) reuseParentNumber(ctx context.Context, parentID int64, email string) (int64, int, bool) {
	var seq int64
	var year int
	var status, owner string
	err := s.pool.QueryRow(ctx, `
SELECT number_seq, number_year, status, owner_email FROM requests WHERE id = $1`,
		parentID).Scan(&seq, &year, &status, &owner)
	if err != nil {
		return 0, 0, false
	}
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return 0, 0, false
	}
	if owner != email && roleRank(role) < roleRank("admin") {
		return 0, 0, false
	}
	if status != "new" {
		return 0, 0, false
	}
	return seq, year, true
}

// pushCreate создаёт одну заявку (1 метод + 1 лаба из method_labs метода) с уже
// выделенным seq.
func (s *Server) pushCreate(ctx context.Context, p PushRequest, email string, now time.Time, seq int64, year int) (*Request, error) {
	if strings.TrimSpace(p.Title) == "" {
		return nil, nil
	}
	methodID := p.resolvedMethodID()
	if methodID <= 0 {
		log.Printf("push create: method_id missing")
		return nil, nil
	}
	if p.LabID <= 0 {
		log.Printf("push create: lab_id missing")
		return nil, nil
	}
	pi, err := s.loadProjectInfo(ctx, p.ProjectID)
	if err != nil {
		return nil, err
	}
	status := p.Status
	if status != "new" && status != "received" && status != "processing" && status != "completed" {
		status = "new"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	effProjectID := p.ProjectID
	if p.ProjectID <= 0 && p.EKN != "" {
		effProjectID, err = s.ensureEknProject(ctx, tx, p.EKN)
		if err != nil {
			return nil, err
		}
		if effProjectID > 0 {
			pi.code = p.EKN
		}
	}

	mlr, err := loadMethodLabRow(ctx, tx, methodID, p.LabID)
	if err != nil {
		log.Printf("push create: invalid method_id/lab_id %d/%d: %v", methodID, p.LabID, err)
		return nil, nil
	}
	customer, lab := buildNumbers(seq, year, pi.code, mlr.labCode, mlr.methodCode)

	var id int64
	err = tx.QueryRow(ctx, `
INSERT INTO requests (number_seq, number_year, title, description, object_id, project_id,
	group_id, owner_email, status, priority, test_purpose, ekn,
	method_id, lab_id, customer_number, lab_number, external_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING id`,
		seq, year, p.Title, p.Description, nullableID(p.ObjectID), nullableID(effProjectID),
		nullableID(p.GroupID), email, status, p.Priority, p.TestPurpose, p.EKN,
		methodID, p.LabID, customer, lab, p.ExternalID).Scan(&id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.loadRequest(ctx, id)
}
