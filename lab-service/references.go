package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Labs ----

type Lab struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	// ParentLabID — только у внешних лабораторий (обязателен при создании, см.
	// handleCreateLab): внешняя лаба не существует самостоятельно, привязана к
	// внутренней. 0 — у внутренних лабораторий (или у внешних, заведённых раньше
	// этого поля — переходное состояние, не мешает работе, см. AGENTS.md).
	ParentLabID int64  `json:"parent_lab_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// handleListLabs — admin/superadmin видят все лаборатории; остальные — свои
// (lab_members) плюс внешние лабы, привязанные к своим как к родителю (внешняя лаба
// расширяет возможности внутренней и должна быть видна её сотрудникам, хотя сама
// lab_members для внешней лабы не заводится — там нет пользователей этой системы).
func (s *Server) handleListLabs(w http.ResponseWriter, r *http.Request) {
	email := currentEmail(r)
	role, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	var rows pgx.Rows
	if roleRank(role) >= roleRank("admin") {
		rows, err = s.pool.Query(r.Context(), `
SELECT id, code, name, description, type, COALESCE(parent_lab_id, 0), created_at, updated_at
FROM labs ORDER BY id`)
	} else {
		rows, err = s.pool.Query(r.Context(), `
SELECT id, code, name, description, type, COALESCE(parent_lab_id, 0), created_at, updated_at
FROM labs
WHERE id IN (SELECT lab_id FROM lab_members WHERE email = $1)
   OR parent_lab_id IN (SELECT lab_id FROM lab_members WHERE email = $1)
ORDER BY id`, email)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()

	labs := make([]Lab, 0, 16)
	for rows.Next() {
		var l Lab
		var ca, ua time.Time
		if err := rows.Scan(&l.ID, &l.Code, &l.Name, &l.Description, &l.Type, &l.ParentLabID, &ca, &ua); err != nil {
			log.Printf("labs scan: %v", err)
			continue
		}
		l.CreatedAt = ca.Format(time.RFC3339)
		l.UpdatedAt = ua.Format(time.RFC3339)
		labs = append(labs, l)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"labs": labs})
}

// handleCreateLab — внешняя лаба (type=external) ОБЯЗАНА указать parent_lab_id
// существующей внутренней лабы (не может существовать самостоятельно); внутренняя —
// без родителя (parent_lab_id недопустим).
func (s *Server) handleCreateLab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code        string `json:"code"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		ParentLabID int64  `json:"parent_lab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code is required"})
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	if req.Type != "" && req.Type != "internal" && req.Type != "external" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be internal or external"})
		return
	}
	if req.Type == "" {
		req.Type = "internal"
	}
	if req.Type == "external" {
		if req.ParentLabID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "external lab requires parent_lab_id (внешняя лаборатория не может существовать самостоятельно)"})
			return
		}
		var parentType string
		err := s.pool.QueryRow(r.Context(), `SELECT type FROM labs WHERE id = $1`, req.ParentLabID).Scan(&parentType)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent_lab_id: lab not found"})
			return
		}
		if parentType != "internal" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent_lab_id must reference an internal lab"})
			return
		}
	} else if req.ParentLabID != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "internal lab cannot have parent_lab_id"})
		return
	}
	var id int64
	err := s.pool.QueryRow(r.Context(), `
INSERT INTO labs (code, name, description, type, parent_lab_id) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (code) DO NOTHING RETURNING id`,
		req.Code, req.Name, req.Description, req.Type, nullableID(req.ParentLabID)).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleUpdateLab — частичный PATCH (только переданные поля). Валидирует ту же
// комбинацию type+parent_lab_id, что handleCreateLab, но с учётом уже сохранённого
// значения для полей, не переданных в этом запросе (эффективное type/parent_lab_id —
// новое, если передано, иначе текущее из БД).
func (s *Server) handleUpdateLab(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Code        *string `json:"code"`
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Type        *string `json:"type"`
		ParentLabID *int64  `json:"parent_lab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Code != nil && strings.TrimSpace(*req.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code is required"})
		return
	}
	if req.Type != nil {
		t := strings.TrimSpace(*req.Type)
		if t != "internal" && t != "external" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be internal or external"})
			return
		}
		req.Type = &t
	}

	var curType string
	var curParent int64
	if err := s.pool.QueryRow(r.Context(),
		`SELECT type, COALESCE(parent_lab_id, 0) FROM labs WHERE id = $1`, id).Scan(&curType, &curParent); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "lab not found"})
		return
	}
	effType := curType
	if req.Type != nil {
		effType = *req.Type
	}
	effParent := curParent
	if req.ParentLabID != nil {
		effParent = *req.ParentLabID
	}
	if effType == "external" {
		if effParent <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "external lab requires parent_lab_id (внешняя лаборатория не может существовать самостоятельно)"})
			return
		}
		if effParent == id {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lab cannot be its own parent"})
			return
		}
		var parentType string
		if err := s.pool.QueryRow(r.Context(), `SELECT type FROM labs WHERE id = $1`, effParent).Scan(&parentType); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent_lab_id: lab not found"})
			return
		}
		if parentType != "internal" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent_lab_id must reference an internal lab"})
			return
		}
	} else if effParent != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "internal lab cannot have parent_lab_id"})
		return
	}

	parentProvided := req.ParentLabID != nil
	var parentValue any
	if parentProvided {
		parentValue = nullableID(*req.ParentLabID)
	}
	_, err = s.pool.Exec(r.Context(), `
UPDATE labs SET
	code = COALESCE($2, code), name = COALESCE($3, name), description = COALESCE($4, description),
	type = COALESCE($5, type),
	parent_lab_id = CASE WHEN $6 THEN $7 ELSE parent_lab_id END,
	updated_at = now()
WHERE id = $1`, id, req.Code, req.Name, req.Description, req.Type, parentProvided, parentValue)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Methods ----

// Method — методы теперь принадлежат НЕСКОЛЬКИМ лабораториям (method_labs,
// 2026-08-19, по требованию пользователя): LabIDs заменяет старую единичную
// lab_id (колонка в БД осталась нетронутой как неактивный исторический артефакт,
// см. main.go migrate()). При создании заявки клиент выбирает ОДНУ конкретную
// лабу из LabIDs метода — она уходит в requests.lab_id.
type Method struct {
	ID                     int64    `json:"id"`
	Code                   string   `json:"code"`
	Name                   string   `json:"name"`
	LabIDs                 []int64  `json:"lab_ids"`
	Description            string   `json:"description"`
	DeterminableIndicators []string `json:"determinable_indicators"`
	// Formulas/Classification/ChartConfigs/InputParams — раньше не читались
	// вообще ни в одном GET (баг, 2026-08-21): конфигуратор всегда открывался
	// пустым и PATCH безусловно перезатирал реальные данные до "[]". InputParams
	// теперь хранит структуру атрибутов метода (см. MethodAttribute), не просто
	// открытый JSON.
	Formulas       []map[string]any `json:"formulas"`
	Classification []map[string]any `json:"classification"`
	ChartConfigs   []map[string]any `json:"chart_configs"`
	InputParams    []map[string]any `json:"input_parameters"`
	// Presentation — конфигуратор методов, блок 3: секции показателей, порядок/
	// подписи/видимость (UI/выписка/протокол) полей и графиков внутри секции.
	Presentation MethodPresentation `json:"presentation"`
	// OperatorForm — схема формы для испытателя (конструктор, 2026-08-22).
	OperatorForm MethodOperatorForm `json:"operator_form"`
	CreatedAt    string             `json:"created_at"`
	UpdatedAt    string             `json:"updated_at"`
}

// unmarshalJSONBArray разбирает JSONB-массив объектов (formulas/classification/
// chart_configs/input_parameters) — при NULL/пустом/некорректном значении
// возвращает пустой (не nil) слайс, чтобы клиент всегда получал `[]`, не `null`.
func unmarshalJSONBArray(raw []byte) []map[string]any {
	out := []map[string]any{}
	if len(raw) == 0 || string(raw) == "[]" || string(raw) == "null" {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

// loadMethodLabsMap группирует method_labs в map[method_id][]lab_id (одним запросом,
// для листинга — избегает N+1).
func (s *Server) loadMethodLabsMap(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT method_id, lab_id FROM method_labs ORDER BY method_id, lab_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var mid, lid int64
		if err := rows.Scan(&mid, &lid); err != nil {
			continue
		}
		out[mid] = append(out[mid], lid)
	}
	return out, rows.Err()
}

// methodLabIDs — лабы ОДНОГО метода (для проверки requireLabAdminOfAny/OfAll на
// PATCH/DELETE — не тянуть карту по всем методам ради одного).
func (s *Server) methodLabIDs(ctx context.Context, methodID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT lab_id FROM method_labs WHERE method_id = $1`, methodID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// validateLabIDs дедуплицирует и проверяет, что каждый id ссылается на существующую лабу.
func (s *Server) validateLabIDs(ctx context.Context, ids []int64) ([]int64, error) {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM labs WHERE id = $1)`, id).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("lab not found: %d", id)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

func (s *Server) handleListMethods(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
SELECT id, code, name, description, determinable_indicators, formulas, classification,
	chart_configs, input_parameters, presentation, operator_form, created_at, updated_at FROM methods ORDER BY id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()

	methods := make([]Method, 0, 16)
	for rows.Next() {
		var m Method
		var ca, ua time.Time
		var indsRaw, formulasRaw, classRaw, chartsRaw, inputsRaw, presRaw, opFormRaw []byte
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.Description, &indsRaw,
			&formulasRaw, &classRaw, &chartsRaw, &inputsRaw, &presRaw, &opFormRaw, &ca, &ua); err != nil {
			log.Printf("methods scan: %v", err)
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
		m.CreatedAt = ca.Format(time.RFC3339)
		m.UpdatedAt = ua.Format(time.RFC3339)
		methods = append(methods, m)
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"methods": methods})
}

func (s *Server) handleCreateMethod(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code                   string   `json:"code"`
		Name                   string   `json:"name"`
		LabIDs                 []int64  `json:"lab_ids"`
		Description            string   `json:"description"`
		DeterminableIndicators []string `json:"determinable_indicators"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code is required"})
		return
	}
	if req.DeterminableIndicators == nil {
		req.DeterminableIndicators = []string{}
	}
	indsJSON, err := json.Marshal(req.DeterminableIndicators)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid determinable_indicators"})
		return
	}
	labIDs, err := s.validateLabIDs(r.Context(), req.LabIDs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(labIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one lab_id is required"})
		return
	}
	// lab_admin создаёт методы ТОЛЬКО для лаб, которые администрирует (2026-08-24,
	// делегированные полномочия) — не может привязать метод к чужой лабе.
	if ok, err := s.requireLabAdminOfAll(r.Context(), currentEmail(r), labIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	} else if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer all listed labs"})
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer tx.Rollback(r.Context())

	var id int64
	err = tx.QueryRow(r.Context(), `
INSERT INTO methods (code, name, description, determinable_indicators) VALUES ($1, $2, $3, $4)
ON CONFLICT (code) DO NOTHING RETURNING id`, req.Code, req.Name, req.Description, indsJSON).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
		return
	}
	for _, labID := range labIDs {
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO method_labs (method_id, lab_id) VALUES ($1, $2)`, id, labID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleDeleteMethod удаляет метод. 23503 (есть заявки/результаты/испытатели/
// оборудование, ссылающиеся на метод) → понятная 409, а не голая 500 — на практике
// метод, по которому уже подавались заявки, удалить не получится, и это ожидаемо.
func (s *Server) handleDeleteMethod(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	// lab_admin удаляет только методы своих лаб (2026-08-24, делегированные полномочия).
	labIDs, err := s.methodLabIDs(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if ok, err := s.requireLabAdminOfAny(r.Context(), currentEmail(r), labIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	} else if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer at least one of this method's labs"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM methods WHERE id = $1`, id); err != nil {
		if isForeignKeyViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "метод используется в заявках или справочниках, удаление невозможно"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Objects ----

type Object struct {
	ID              int64          `json:"id"`
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	Characteristics map[string]any `json:"characteristics"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at"`
}

func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
SELECT id, name, description, characteristics, created_at, updated_at FROM objects ORDER BY id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()

	objects := make([]Object, 0, 16)
	for rows.Next() {
		var o Object
		var charsRaw []byte
		var ca, ua time.Time
		if err := rows.Scan(&o.ID, &o.Name, &o.Description, &charsRaw, &ca, &ua); err != nil {
			log.Printf("objects scan: %v", err)
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
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"objects": objects})
}

func (s *Server) handleCreateObject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string         `json:"name"`
		Description     string         `json:"description"`
		Characteristics map[string]any `json:"characteristics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	if req.Characteristics == nil {
		req.Characteristics = map[string]any{}
	}
	charsJSON, err := json.Marshal(req.Characteristics)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid characteristics"})
		return
	}
	var id int64
	err = s.pool.QueryRow(r.Context(), `
INSERT INTO objects (name, description, characteristics) VALUES ($1, $2, $3) RETURNING id`,
		req.Name, req.Description, charsJSON).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleUpdateObject — PATCH /objects/{id} (2026-08-24, по прямому запросу
// пользователя: "проверь и исправь чтобы у нас не плодились сироты"). До этой
// правки объекта нечем было обновить, кроме POST /objects (создание) — sbe-requests
// при редактировании заявки с уже привязанным объектом каждый раз создавал НОВЫЙ
// объект (с обновлёнными характеристиками, напр. целевым показателем) и молча
// оставлял старый висеть без единой ссылки — заявка либо продолжала указывать на
// старый (если переключение object_id не успевало синхронизироваться), либо
// переключалась на новый, а старый оставался мусором навсегда в любом случае.
// characteristics — ПОЛНАЯ замена (как и при создании), не JSON-merge: клиент
// всегда собирает объект целиком из состояния формы, не присылает частичный diff.
func (s *Server) handleUpdateObject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Name            *string        `json:"name"`
		Description     *string        `json:"description"`
		Characteristics map[string]any `json:"characteristics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name cannot be empty"})
		return
	}
	var charsParam any
	if req.Characteristics != nil {
		charsJSON, err := json.Marshal(req.Characteristics)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid characteristics"})
			return
		}
		charsParam = string(charsJSON)
	}
	tag, err := s.pool.Exec(r.Context(), `
UPDATE objects SET
	name = COALESCE($2, name),
	description = COALESCE($3, description),
	characteristics = COALESCE($4::jsonb, characteristics),
	updated_at = now()
WHERE id = $1`, id, req.Name, req.Description, charsParam)
	if err != nil {
		log.Printf("update object: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "object not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
