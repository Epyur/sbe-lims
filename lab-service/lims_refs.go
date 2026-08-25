package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// isForeignKeyViolation/isUniqueViolation распознают конфликты БД, чтобы отвечать
// понятной 409-ошибкой вместо голого 500 (используются при удалении/переименовании
// справочников: испытателей/оборудования/методов).
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isLabMember возвращает роль пользователя в лаборатории (или "").
func (s *Server) labMemberRole(ctx context.Context, email string, labID int64) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM lab_members WHERE lab_id = $1 AND email = $2`, labID, email).Scan(&role)
	if err != nil {
		return "", nil // не участник
	}
	return role, nil
}

// requestLabID возвращает ЭФФЕКТИВНУЮ лабораторию заявки (0 если не определена):
// для внутренней лабы заявки (requests.lab_id, конкретная лаба из method_labs,
// зафиксированная при создании) — её id; для внешней (расширяет возможности
// внутренней, сама не имеет lab_members) — id её parent_lab_id. Внешняя лаба без
// родителя (переходное состояние, см. AGENTS.md) резолвится в себя — доступ получит
// только app-admin+, пока родитель не проставлен.
func (s *Server) requestLabID(ctx context.Context, requestID int64) (int64, error) {
	var labID int64
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(l.parent_lab_id, l.id, 0)
FROM requests r
LEFT JOIN labs l ON l.id = r.lab_id
WHERE r.id = $1`, requestID).Scan(&labID)
	if err != nil {
		return 0, err
	}
	return labID, nil
}

// requireLabAccess проверяет право ЗАПИСИ (ввод результатов, расчёт, статус): сотрудник
// лаборатории заявки с ролью lab_operator/lab_admin (или app-admin+). lab_auditor
// (только чтение) сюда не допускается — см. requireLabRead.
func (s *Server) requireLabAccess(ctx context.Context, email string, requestID int64) (bool, error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return false, err
	}
	if roleRank(role) >= roleRank("admin") {
		return true, nil
	}
	labID, err := s.requestLabID(ctx, requestID)
	if err != nil || labID <= 0 {
		return false, nil
	}
	memberRole, err := s.labMemberRole(ctx, email, labID)
	if err != nil {
		return false, err
	}
	return memberRole == "lab_operator" || memberRole == "lab_admin", nil
}

// requireLabRead проверяет право ПРОСМОТРА (результаты/графики/протокол): то же, что
// requireLabAccess, плюс lab_auditor (только чтение своей лабы).
func (s *Server) requireLabRead(ctx context.Context, email string, requestID int64) (bool, error) {
	ok, err := s.requireLabAccess(ctx, email, requestID)
	if err != nil || ok {
		return ok, err
	}
	labID, err := s.requestLabID(ctx, requestID)
	if err != nil || labID <= 0 {
		return false, nil
	}
	memberRole, err := s.labMemberRole(ctx, email, labID)
	if err != nil {
		return false, err
	}
	return memberRole == "lab_auditor", nil
}

// requireLabAdminOf — делегированные полномочия lab_admin внутри своей лабы
// (2026-08-24, по прямому запросу пользователя: lab_admin должен быть реальным
// админом своей лабы — конфиг методов, сотрудники, роль "руководителя" в
// Kanban-доске — а не просто synonym для lab_operator). true, если app-admin+
// (глобально), либо lab_admin ИМЕННО лабы labID.
func (s *Server) requireLabAdminOf(ctx context.Context, email string, labID int64) (bool, error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return false, err
	}
	if roleRank(role) >= roleRank("admin") {
		return true, nil
	}
	memberRole, err := s.labMemberRole(ctx, email, labID)
	if err != nil {
		return false, err
	}
	return memberRole == "lab_admin", nil
}

// requireLabAdminOfAny — app-admin+ либо lab_admin ХОТЯ БЫ ОДНОЙ из labIDs (метод
// может принадлежать нескольким лабам — достаточно администрировать одну из них,
// чтобы редактировать/удалить метод целиком).
func (s *Server) requireLabAdminOfAny(ctx context.Context, email string, labIDs []int64) (bool, error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return false, err
	}
	if roleRank(role) >= roleRank("admin") {
		return true, nil
	}
	for _, labID := range labIDs {
		memberRole, err := s.labMemberRole(ctx, email, labID)
		if err != nil {
			return false, err
		}
		if memberRole == "lab_admin" {
			return true, nil
		}
	}
	return false, nil
}

// requireLabAdminOfAll — app-admin+ либо lab_admin КАЖДОЙ из labIDs (создание
// метода/переназначение его лаб — нельзя привязать метод к лабе, которой не
// администрируешь, даже если администрируешь другую).
func (s *Server) requireLabAdminOfAll(ctx context.Context, email string, labIDs []int64) (bool, error) {
	role, err := s.effectiveRole(ctx, appIDFromEnv(), email)
	if err != nil {
		return false, err
	}
	if roleRank(role) >= roleRank("admin") {
		return true, nil
	}
	if len(labIDs) == 0 {
		return false, nil
	}
	for _, labID := range labIDs {
		memberRole, err := s.labMemberRole(ctx, email, labID)
		if err != nil {
			return false, err
		}
		if memberRole != "lab_admin" {
			return false, nil
		}
	}
	return true, nil
}

// ---- Inventors ----

type Inventor struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	Department string `json:"department"`
	Position   string `json:"position"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func (s *Server) handleListInventors(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
SELECT id, name, email, phone, department, position, created_at, updated_at
FROM inventors ORDER BY name`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]Inventor, 0, 16)
	for rows.Next() {
		var it Inventor
		var ca, ua time.Time
		if err := rows.Scan(&it.ID, &it.Name, &it.Email, &it.Phone, &it.Department,
			&it.Position, &ca, &ua); err != nil {
			continue
		}
		it.CreatedAt = ca.Format(time.RFC3339)
		it.UpdatedAt = ua.Format(time.RFC3339)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"inventors": out})
}

func (s *Server) handleCreateInventor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Email      string `json:"email"`
		Phone      string `json:"phone"`
		Department string `json:"department"`
		Position   string `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	var id int64
	err := s.pool.QueryRow(r.Context(), `
INSERT INTO inventors (name, email, phone, department, position) VALUES ($1, $2, $3, $4, $5)
RETURNING id`, req.Name, req.Email, req.Phone, req.Department, req.Position).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleUpdateInventor(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Name       *string `json:"name"`
		Email      *string `json:"email"`
		Phone      *string `json:"phone"`
		Department *string `json:"department"`
		Position   *string `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	tag, err := s.pool.Exec(r.Context(), `
UPDATE inventors SET
	name = COALESCE($2, name), email = COALESCE($3, email), phone = COALESCE($4, phone),
	department = COALESCE($5, department), position = COALESCE($6, position), updated_at = now()
WHERE id = $1`, id, req.Name, req.Email, req.Phone, req.Department, req.Position)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "inventor not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteInventor удаляет испытателя. 23503 (использован в measurement_results
// или inventor_methods) → понятная 409, а не голая 500.
func (s *Server) handleDeleteInventor(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM inventors WHERE id = $1`, id); err != nil {
		if isForeignKeyViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "испытатель используется в результатах испытаний, удаление невозможно"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Equipment ----

type Equipment struct {
	ID              int64  `json:"id"`
	Code            string `json:"code"`
	Name            string `json:"name"`
	Location        string `json:"location"`
	Responsible     string `json:"responsible"`
	LastCalibration string `json:"last_calibration"`
	NextCalibration string `json:"next_calibration"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func (s *Server) handleListEquipment(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
SELECT id, code, name, location, responsible, last_calibration, next_calibration, status,
	created_at, updated_at FROM equipment ORDER BY code`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]Equipment, 0, 16)
	for rows.Next() {
		var it Equipment
		var lc, nc *time.Time
		var ca, ua time.Time
		if err := rows.Scan(&it.ID, &it.Code, &it.Name, &it.Location, &it.Responsible,
			&lc, &nc, &it.Status, &ca, &ua); err != nil {
			continue
		}
		if lc != nil {
			it.LastCalibration = lc.Format("2006-01-02")
		}
		if nc != nil {
			it.NextCalibration = nc.Format("2006-01-02")
		}
		it.CreatedAt = ca.Format(time.RFC3339)
		it.UpdatedAt = ua.Format(time.RFC3339)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"equipment": out})
}

func (s *Server) handleCreateEquipment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code            string `json:"code"`
		Name            string `json:"name"`
		Location        string `json:"location"`
		Responsible     string `json:"responsible"`
		LastCalibration string `json:"last_calibration"`
		NextCalibration string `json:"next_calibration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code is required"})
		return
	}
	var lc, nc *time.Time
	if t, err := time.Parse("2006-01-02", req.LastCalibration); err == nil {
		lc = &t
	}
	if t, err := time.Parse("2006-01-02", req.NextCalibration); err == nil {
		nc = &t
	}
	var id int64
	err := s.pool.QueryRow(r.Context(), `
INSERT INTO equipment (code, name, location, responsible, last_calibration, next_calibration)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (code) DO NOTHING RETURNING id`,
		req.Code, req.Name, req.Location, req.Responsible, lc, nc).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleUpdateEquipment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		Code        *string `json:"code"`
		Name        *string `json:"name"`
		Location    *string `json:"location"`
		Responsible *string `json:"responsible"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Code != nil && strings.TrimSpace(*req.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code is required"})
		return
	}
	tag, err := s.pool.Exec(r.Context(), `
UPDATE equipment SET
	code = COALESCE($2, code), name = COALESCE($3, name),
	location = COALESCE($4, location), responsible = COALESCE($5, responsible), updated_at = now()
WHERE id = $1`, id, req.Code, req.Name, req.Location, req.Responsible)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "code already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "equipment not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteEquipment удаляет оборудование. 23503 (использовано в method_equipment)
// → понятная 409, а не голая 500.
func (s *Server) handleDeleteEquipment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM equipment WHERE id = $1`, id); err != nil {
		if isForeignKeyViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "оборудование используется методом, удаление невозможно"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Lab members ----

type LabMember struct {
	LabID int64  `json:"lab_id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// handleListLabMembers — без ?lab_id: полный список ВСЕХ лаб, только руководитель
// (глобальная роль admin/superadmin), как раньше — используется "Настройками". С
// ?lab_id: ростер ОДНОЙ лабы, доступен ЛЮБОМУ её участнику (lab_operator/
// lab_admin/lab_auditor), не только руководителю — нужно Kanban-доске sbe-lims
// (renderQueueBoard/renderRequestDetail, см. kanban.go): испытателю нужно видеть
// состав ВСЕХ ячеек колонок 2/3, не только свою.
func (s *Server) handleListLabMembers(w http.ResponseWriter, r *http.Request) {
	email := currentEmail(r)
	role, err := s.effectiveRole(r.Context(), appIDFromEnv(), email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	isHead := roleRank(role) >= roleRank("admin")
	labIDParam := strings.TrimSpace(r.URL.Query().Get("lab_id"))

	var rows pgx.Rows
	if labIDParam == "" {
		if !isHead {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lab_id is required"})
			return
		}
		rows, err = s.pool.Query(r.Context(), `SELECT lab_id, email, role FROM lab_members ORDER BY lab_id, email`)
	} else {
		labID, perr := strconv.ParseInt(labIDParam, 10, 64)
		if perr != nil || labID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid lab_id"})
			return
		}
		if !isHead {
			memberRole, merr := s.labMemberRole(r.Context(), email, labID)
			if merr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
				return
			}
			if memberRole == "" {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not a member of this lab"})
				return
			}
		}
		rows, err = s.pool.Query(r.Context(), `SELECT lab_id, email, role FROM lab_members WHERE lab_id = $1 ORDER BY email`, labID)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]LabMember, 0, 16)
	for rows.Next() {
		var m LabMember
		if err := rows.Scan(&m.LabID, &m.Email, &m.Role); err != nil {
			continue
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (s *Server) handleSetLabMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LabID int64  `json:"lab_id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.LabID <= 0 || req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lab_id and email are required"})
		return
	}
	if req.Role == "" {
		req.Role = "lab_operator"
	}
	if req.Role != "lab_operator" && req.Role != "lab_admin" && req.Role != "lab_auditor" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role must be lab_operator, lab_admin or lab_auditor"})
		return
	}
	// lab_admin управляет сотрудниками ТОЛЬКО своей лабы, но ЛЮБОЙ их ролью, включая
	// назначение другого lab_admin (2026-08-24, делегированные полномочия — по
	// решению пользователя: полноценный админ своей лабы, без ограничения "только
	// обычных сотрудников").
	if ok, err := s.requireLabAdminOf(r.Context(), currentEmail(r), req.LabID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	} else if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer this lab"})
		return
	}
	// lab_members заводится только для внутренних лаб — у внешней своих пользователей
	// системы нет, видимость её заявок резолвится через parent_lab_id (requestLabID),
	// а не через lab_members самой внешней лабы.
	var labType string
	if err := s.pool.QueryRow(r.Context(), `SELECT type FROM labs WHERE id = $1`, req.LabID).Scan(&labType); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lab not found"})
		return
	}
	if labType != "internal" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lab_members can only be set on an internal lab (external labs route through parent_lab_id)"})
		return
	}
	_, err := s.pool.Exec(r.Context(), `
INSERT INTO lab_members (lab_id, email, role) VALUES ($1, $2, $3)
ON CONFLICT (lab_id, email) DO UPDATE SET role = EXCLUDED.role`,
		req.LabID, req.Email, req.Role)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRemoveLabMember(w http.ResponseWriter, r *http.Request) {
	labID, err := strconv.ParseInt(r.PathValue("lab_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid lab_id"})
		return
	}
	if ok, err := s.requireLabAdminOf(r.Context(), currentEmail(r), labID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	} else if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer this lab"})
		return
	}
	email := r.PathValue("email")
	_, err = s.pool.Exec(r.Context(),
		`DELETE FROM lab_members WHERE lab_id = $1 AND email = $2`, labID, email)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Methods config (PATCH) ----

// MethodAttribute — элемент methods.input_parameters (конфигуратор методов,
// 2026-08-21). Раньше это поле хранилось, но не читалось и не использовалось
// ни клиентом, ни сервером — теперь это структурированный список атрибутов
// метода, и он же — единственный источник для methods.formulas у "расчётных"
// и "агрегированных" атрибутов (см. deriveFormulasFromAttributes).
type MethodAttribute struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	DataType    string                `json:"data_type"`   // text|int|float|date|time|photo
	FillMethod  string                `json:"fill_method"` // manual|instrument|calculated
	Level       string                `json:"level"`       // experiment|aggregated
	Formula     string                `json:"formula,omitempty"`
	Aggregation *AttributeAggregation `json:"aggregation,omitempty"`
	// Synonyms — альтернативные raw-имена этого атрибута (2026-08-21): позволяет
	// назвать атрибут как удобно в конфигураторе без оглядки на то, как поле
	// называется в legacy-источниках (email-импорт от десктопной ЛИМС) — при
	// приёме результатов из письма synonyms сопоставляются с ID здесь же, до
	// canonicalFieldNames/knownRawFields (см. email_ingest.go applyResultPayload).
	Synonyms []string `json:"synonyms,omitempty"`
}

// AttributeAggregation — простое агрегирование по одному атрибуту
// эксперимент-уровня (среднее/минимум/максимум/логическое) без ручного ввода
// формулы — сервер сам строит формулу `{method}({source})` с
// apply_level="aggregated" (тот же путь исполнения, что уже работает для
// существующих агрегированных формул, см. results.go applyAggregatedFormulas).
// Для более сложных случаев (напр. калибровочная интерполяция) — атрибут задаёт
// Formula напрямую.
//
// "any"/"all" (2026-08-25, прямой запрос пользователя — заявка 287/2026, метод
// ГГ: agg_burning_drops пытался считаться как max(burning_drops), а
// burning_drops — текстовое Да/Нет-поле, не число; см. AGENTS.md) — для таких
// полей: any — "Да", если хоть в одной серии "Да"; all — "Да" только если ВО
// ВСЕХ сериях "Да" (т.е. "Нет", если хоть в одной серии "Нет"). См. dsl.go
// callExpr.eval/collectBools.
type AttributeAggregation struct {
	Source string `json:"source"`
	Method string `json:"method"` // avg|min|max|any|all
}

var validAggregationMethods = map[string]bool{
	"avg": true, "min": true, "max": true, "any": true, "all": true,
}

// loadAttributeSynonymMap строит raw-имя -> id атрибута из methods.input_parameters
// (только атрибуты с непустыми Synonyms) — используется при приёме результатов из
// email (см. email_ingest.go applyResultPayload): конфигуратор может назвать
// атрибут как удобно, не оглядываясь на то, как поле называется в legacy-письмах.
func (s *Server) loadAttributeSynonymMap(ctx context.Context, methodID int64) (map[string]string, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT input_parameters FROM methods WHERE id = $1`, methodID).Scan(&raw); err != nil {
		return nil, err
	}
	var attrs []MethodAttribute
	if len(raw) > 0 && string(raw) != "[]" {
		_ = json.Unmarshal(raw, &attrs)
	}
	out := map[string]string{}
	for _, a := range attrs {
		for _, syn := range a.Synonyms {
			syn = strings.TrimSpace(syn)
			if syn != "" {
				out[syn] = a.ID
			}
		}
	}
	return out, nil
}

// loadAttributeIDSet возвращает множество ID атрибутов, объявленных в
// methods.input_parameters — используется email_ingest.go/applyResultPayload
// (2026-08-24), чтобы принимать из письма-результата ТОЛЬКО то, что реально
// заведено в конфигураторе метода: письмо может содержать поля, которые метод
// сознательно не заводит (напр. calibration_* у РП — по решению пользователя
// "все что связано с калибровкой не заводи... вводим только прямые измерения
// и расчеты"), и такие поля не должны попадать в values, даже если
// resolveResultKey их узнаёт как canonicalFieldNames/knownRawFields.
func (s *Server) loadAttributeIDSet(ctx context.Context, methodID int64) (map[string]bool, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT input_parameters FROM methods WHERE id = $1`, methodID).Scan(&raw); err != nil {
		return nil, err
	}
	var attrs []MethodAttribute
	if len(raw) > 0 && string(raw) != "[]" {
		_ = json.Unmarshal(raw, &attrs)
	}
	out := make(map[string]bool, len(attrs))
	for _, a := range attrs {
		out[a.ID] = true
	}
	return out, nil
}

// deriveFormulasFromAttributes строит methods.formulas ЦЕЛИКОМ из
// input_parameters — единственный источник истины для формул, привязанных к
// конкретному атрибуту (по target_parameter == attribute.id). Вызывается
// только когда input_parameters присутствует в PATCH-запросе (иначе formulas
// не трогаем — см. handleUpdateMethodConfig).
func deriveFormulasFromAttributes(inputParamsRaw json.RawMessage) (json.RawMessage, error) {
	var attrs []MethodAttribute
	if err := json.Unmarshal(inputParamsRaw, &attrs); err != nil {
		return nil, err
	}
	formulas := make([]map[string]any, 0, len(attrs))
	for _, a := range attrs {
		applyLevel := "series"
		if a.Level == "aggregated" {
			applyLevel = "aggregated"
		}
		switch {
		case a.FillMethod == "calculated" && strings.TrimSpace(a.Formula) != "":
			// Расчётный атрибут (fill_method="calculated") — формула как есть.
			// fill_method проверяется явно (2026-08-23) — раньше проверялось
			// только "Formula не пусто": если админ переключал атрибут с
			// "Формула" на "Классификация" через конфигуратор, поле .Formula
			// оставалось в JSON (конфигуратор не чистит его при смене
			// fill_method), и эта СТАРАЯ формула продолжала молча исполняться
			// при каждом расчёте наравне с новым правилом классификации — баг,
			// найденный на методе ГВ (target_group_compliance): пользователь
			// добавил корректное правило классификации, но расчёт всё равно
			// падал на давно неактуальной формуле.
			formulas = append(formulas, map[string]any{
				"expression":       a.Formula,
				"target_parameter": a.ID,
				"apply_level":      applyLevel,
			})
		case a.FillMethod != "classification" && a.Level == "aggregated" && a.Aggregation != nil:
			// fill_method проверяется явно по той же причине, что выше: стартовое
			// .Aggregation, оставленное конфигуратором после переключения на
			// "Классификация", не должно молча породить ещё одну авто-формулу
			// поверх правила классификации, которое реально выбрал пользователь.
			// "calculated" здесь ещё разрешён — .aggregation без .formula на
			// calculated-атрибуте (агрегация одной строкой, без DSL) — рабочий,
			// поддерживаемый сервером сценарий (см. TestDeriveFormulasFromAttributes).
			if !validAggregationMethods[a.Aggregation.Method] {
				return nil, fmt.Errorf("attribute %q: invalid aggregation method %q (avg/min/max/any/all)", a.ID, a.Aggregation.Method)
			}
			if strings.TrimSpace(a.Aggregation.Source) == "" {
				return nil, fmt.Errorf("attribute %q: aggregation.source is required", a.ID)
			}
			formulas = append(formulas, map[string]any{
				"expression":       fmt.Sprintf("%s(%s)", a.Aggregation.Method, a.Aggregation.Source),
				"target_parameter": a.ID,
				"apply_level":      "aggregated",
			})
		}
	}
	return json.Marshal(formulas)
}

func (s *Server) handleUpdateMethodConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	email := currentEmail(r)
	// lab_admin конфигурирует только методы своих лаб (2026-08-24, делегированные
	// полномочия) — достаточно администрировать ОДНУ из текущих лаб метода.
	currentLabIDs, err := s.methodLabIDs(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if ok, err := s.requireLabAdminOfAny(r.Context(), email, currentLabIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	} else if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer at least one of this method's labs"})
		return
	}
	var req struct {
		Formulas               *json.RawMessage `json:"formulas"`
		Classification         *json.RawMessage `json:"classification"`
		ChartConfigs           *json.RawMessage `json:"chart_configs"`
		InputParams            *json.RawMessage `json:"input_parameters"`
		Presentation           *json.RawMessage `json:"presentation"`
		OperatorForm           *json.RawMessage `json:"operator_form"`
		LabIDs                 *[]int64         `json:"lab_ids"`
		Description            *string          `json:"description"`
		DeterminableIndicators *[]string        `json:"determinable_indicators"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	// валидация JSON-массивов
	for _, v := range []*json.RawMessage{req.Formulas, req.Classification, req.ChartConfigs, req.InputParams} {
		if v == nil {
			continue
		}
		var arr []any
		if err := json.Unmarshal(*v, &arr); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected JSON array"})
			return
		}
	}
	// presentation — объект {blocks: [...]} (блоки форматированного текста с
	// плейсхолдерами, 2026-08-23 — заменили секции полей от 2026-08-22, которые
	// заменяли плоский {fields:[...]} блока 3 от 2026-08-21); attribute_id
	// внутри плейсхолдеров/таблиц не проверяются на существование здесь —
	// устаревшие ссылки просто резолвятся в пустую строку/без данных при рендере.
	if req.Presentation != nil {
		var pres MethodPresentation
		if err := json.Unmarshal(*req.Presentation, &pres); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "presentation: expected JSON object with \"blocks\" array"})
			return
		}
	}
	var labIDs []int64
	if req.LabIDs != nil {
		var err error
		labIDs, err = s.validateLabIDs(r.Context(), *req.LabIDs)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if len(labIDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one lab_id is required"})
			return
		}
		// Переназначение лаб метода — lab_admin должен администрировать ВСЕ новые
		// лабы (не только одну из текущих, как для остальных полей PATCH), иначе
		// мог бы привязать метод к чужой лабе.
		if ok, err := s.requireLabAdminOfAll(r.Context(), email, labIDs); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		} else if !ok {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: must administer all newly assigned labs"})
			return
		}
	}
	// COALESCE-партиал: поле, не пришедшее в PATCH, остаётся как в БД — раньше
	// formulas/classification/chart_configs/input_parameters/presentation
	// безусловно перезатирались до "[]"/"{}" при отсутствии в теле запроса
	// (в отличие от description, который уже был COALESCE) — скрытый риск потери
	// данных при любом частичном PATCH; сейчас частичный PATCH всегда посылает
	// клиент этой формы (все поля вместе), но потери данных при отсутствии поля
	// быть не должно в принципе.
	var formulasParam any
	if req.InputParams != nil {
		// input_parameters — единственный источник истины для formulas,
		// привязанных к атрибуту (конфигуратор методов, 2026-08-21): если
		// атрибуты пришли в PATCH, formulas всегда перестраивается из них
		// целиком, клиентское поле "formulas" в этом случае игнорируется.
		derived, err := deriveFormulasFromAttributes(*req.InputParams)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		formulasParam = string(derived)
	} else if req.Formulas != nil {
		formulasParam = string(*req.Formulas)
	}
	classParam := rawOrNil(req.Classification)
	chartsParam := rawOrNil(req.ChartConfigs)
	inputParam := rawOrNil(req.InputParams)
	presParam := rawOrNil(req.Presentation)
	opFormParam := rawOrNil(req.OperatorForm)
	if req.OperatorForm != nil {
		var form MethodOperatorForm
		if err := json.Unmarshal(*req.OperatorForm, &form); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "operator_form: expected JSON object with \"fields\" array"})
			return
		}
	}
	var indicatorsParam any
	if req.DeterminableIndicators != nil {
		b, err := json.Marshal(*req.DeterminableIndicators)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid determinable_indicators"})
			return
		}
		indicatorsParam = string(b)
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer tx.Rollback(r.Context())

	if _, err := tx.Exec(r.Context(), `
UPDATE methods SET
	formulas = COALESCE($2::jsonb, formulas),
	classification = COALESCE($3::jsonb, classification),
	chart_configs = COALESCE($4::jsonb, chart_configs),
	input_parameters = COALESCE($5::jsonb, input_parameters),
	presentation = COALESCE($6::jsonb, presentation),
	description = COALESCE($7, description),
	determinable_indicators = COALESCE($8::jsonb, determinable_indicators),
	operator_form = COALESCE($9::jsonb, operator_form),
	updated_at = now()
WHERE id = $1`, id, formulasParam, classParam, chartsParam, inputParam, presParam, req.Description, indicatorsParam, opFormParam); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if req.LabIDs != nil {
		if _, err := tx.Exec(r.Context(), `DELETE FROM method_labs WHERE method_id = $1`, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		for _, labID := range labIDs {
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO method_labs (method_id, lab_id) VALUES ($1, $2)`, id, labID); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// rawOrNil — nil, если поле не пришло в PATCH (COALESCE в SQL сохранит текущее
// значение колонки), иначе строковое представление для ::jsonb-параметра.
func rawOrNil(v *json.RawMessage) any {
	if v == nil {
		return nil
	}
	return string(*v)
}
