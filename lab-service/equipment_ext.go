package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Расширение справочника «Оборудование» (2026-08-26): эксплуатация/поверка/калибровка,
// привязка к методам с ролью, документация. См. AGENTS.md + спека
// docs/superpowers/specs/2026-08-26-sbe-lims-configurator-equipment-design.md.
// Калибровочные атрибуты/формулы (по аналогии с методами) — вне рамок этой задачи,
// прямое решение пользователя; журнал калибровок сейчас — дата+результат+скан.

// ---- Сканы сертификата/акта поверки ----

// uploadEquipmentFileBytes — тот же паттерн, что uploadFileBytes (files.go), но владелец —
// equipment, не request; purpose различает назначение файла при листинге документации
// ("scan" — сертификат/акт поверки, обновляется отдельно в equipment.*_file_key/url,
// не появляется в списке /documents; "equipment_doc" — документация, listable/deletable).
func (s *Server) uploadEquipmentFileBytes(ctx context.Context, equipmentID int64, filename string,
	data []byte, uploadedBy, purpose string) (fileURL, fileKey string, err error) {
	// Дедуп по (equipment_id, file_name, file_size, purpose) — тот же принцип, что у
	// uploadFileBytes (files.go, инцидент заявки 287/2026, v0.2.9): повторная загрузка
	// байт-в-байт того же файла не плодит новую запись/новый объект в S3.
	var existingURL, existingKey string
	scanErr := s.pool.QueryRow(ctx, `
SELECT file_url, file_key FROM files
WHERE equipment_id = $1 AND file_name = $2 AND file_size = $3 AND purpose = $4
ORDER BY id LIMIT 1`, equipmentID, filename, len(data), purpose).Scan(&existingURL, &existingKey)
	if scanErr == nil {
		return existingURL, existingKey, nil
	}
	key := s3Key(filename)
	size, _, err := s.s3.Put(ctx, key, data)
	if err != nil {
		return "", "", err
	}
	fileURL = fmt.Sprintf("%s/api/lab/file-redirect?key=%s", publicBaseURL(), url.QueryEscape(key))
	if _, err := s.pool.Exec(ctx, `
INSERT INTO files (equipment_id, file_key, file_name, file_size, file_url, uploaded_by, purpose)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		equipmentID, key, filename, size, fileURL, uploadedBy, purpose); err != nil {
		return "", "", err
	}
	return fileURL, key, nil
}

// handleEquipmentScan — POST /equipment/{id}/scan?kind=verification_cert|verification_act —
// multipart-файл, обновляет соответствующую пару *_file_key/*_file_url. Номер/дата
// сертификата/акта — обычные текстовые поля PATCH /equipment/{id} (handleUpdateEquipment),
// этот эндпоинт отвечает только за сам файл.
func (s *Server) handleEquipmentScan(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	kind := r.URL.Query().Get("kind")
	var keyCol, urlCol string
	switch kind {
	case "verification_cert":
		keyCol, urlCol = "verification_cert_file_key", "verification_cert_file_url"
	case "verification_act":
		keyCol, urlCol = "verification_act_file_key", "verification_act_file_url"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind must be verification_cert or verification_act"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file is required"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read file"})
		return
	}
	fileURL, fileKey, err := s.uploadEquipmentFileBytes(r.Context(), id, header.Filename, data, currentEmail(r), "scan_"+kind)
	if err != nil {
		log.Printf("equipment scan upload: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "s3 error"})
		return
	}
	tag, err := s.pool.Exec(r.Context(), fmt.Sprintf(
		`UPDATE equipment SET %s = $2, %s = $3, updated_at = now() WHERE id = $1`, keyCol, urlCol),
		id, fileKey, fileURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "equipment not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file_url": fileURL})
}

// ---- Журнал калибровок ----

// MethodID (2026-08-26) — чьи calibration_attributes применялись (оборудование
// может быть "Основное" сразу для нескольких методов). AmbTemp/AmbPres/AmbMoist —
// универсальные системные поля калибровки, одни и те же для ЛЮБОГО метода (тот же
// принцип, что requests.amb_temp/amb_pres/amb_moist у обычных результатов — см.
// sbe-lims/AGENTS.md, "Правило: системные атрибуты"). Values — значения
// calibration_attributes ВЫБРАННОГО метода (ключ — id атрибута).
type EquipmentCalibration struct {
	ID           int64          `json:"id"`
	EquipmentID  int64          `json:"equipment_id"`
	MethodID     int64          `json:"method_id"`
	CalibratedAt string         `json:"calibrated_at"`
	AmbTemp      string         `json:"amb_temp"`
	AmbPres      string         `json:"amb_pres"`
	AmbMoist     string         `json:"amb_moist"`
	Values       map[string]any `json:"values"`
	Result       string         `json:"result"`
	FileURL      string         `json:"file_url"`
	CreatedBy    string         `json:"created_by"`
	CreatedAt    string         `json:"created_at"`
}

func (s *Server) handleListEquipmentCalibrations(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
SELECT id, equipment_id, COALESCE(method_id, 0), calibrated_at, amb_temp, amb_pres, amb_moist, values,
	result, file_url, created_by, created_at
FROM equipment_calibrations WHERE equipment_id = $1 ORDER BY calibrated_at DESC, id DESC`, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]EquipmentCalibration, 0, 8)
	for rows.Next() {
		var it EquipmentCalibration
		var calibratedAt time.Time
		var createdAt time.Time
		var valuesRaw []byte
		if err := rows.Scan(&it.ID, &it.EquipmentID, &it.MethodID, &calibratedAt, &it.AmbTemp, &it.AmbPres, &it.AmbMoist,
			&valuesRaw, &it.Result, &it.FileURL, &it.CreatedBy, &createdAt); err != nil {
			continue
		}
		it.CalibratedAt = calibratedAt.Format("2006-01-02")
		it.CreatedAt = createdAt.Format(time.RFC3339)
		it.Values = map[string]any{}
		if len(valuesRaw) > 0 && string(valuesRaw) != "{}" {
			_ = json.Unmarshal(valuesRaw, &it.Values)
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"calibrations": out})
}

// handleCreateEquipmentCalibration — POST /equipment/{id}/calibrations, multipart:
// calibrated_at (обязательно), method_id (обязательно, если равнодоступно НЕСКОЛЬКО
// методов "Основное" для этого оборудования — какого метода calibration_attributes
// использовать; при ровно одном — клиент передаёт его же), amb_temp/amb_pres/
// amb_moist (универсальные, всегда), values (JSON-объект — значения
// calibration_attributes выбранного метода), result, опциональный "file". После
// вставки пересчитывает equipment.last_calibration/next_calibration.
func (s *Server) handleCreateEquipmentCalibration(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart"})
		return
	}
	calibratedAt, err := time.Parse("2006-01-02", strings.TrimSpace(r.FormValue("calibrated_at")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "calibrated_at is required (YYYY-MM-DD)"})
		return
	}
	var methodID *int64
	if v := strings.TrimSpace(r.FormValue("method_id")); v != "" {
		if parsed, perr := strconv.ParseInt(v, 10, 64); perr == nil && parsed > 0 {
			methodID = &parsed
		}
	}
	ambTemp := strings.TrimSpace(r.FormValue("amb_temp"))
	ambPres := strings.TrimSpace(r.FormValue("amb_pres"))
	ambMoist := strings.TrimSpace(r.FormValue("amb_moist"))
	result := strings.TrimSpace(r.FormValue("result"))
	valuesJSON := strings.TrimSpace(r.FormValue("values"))
	if valuesJSON == "" {
		valuesJSON = "{}"
	}
	var valuesCheck map[string]any
	if err := json.Unmarshal([]byte(valuesJSON), &valuesCheck); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "values: expected JSON object"})
		return
	}
	email := currentEmail(r)

	var fileURL string
	if file, header, ferr := r.FormFile("file"); ferr == nil {
		defer file.Close()
		data, rerr := io.ReadAll(file)
		if rerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read file"})
			return
		}
		u, _, uerr := s.uploadEquipmentFileBytes(r.Context(), id, header.Filename, data, email, "calibration")
		if uerr != nil {
			log.Printf("calibration file upload: %v", uerr)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "s3 error"})
			return
		}
		fileURL = u
	}

	var calID int64
	err = s.pool.QueryRow(r.Context(), `
INSERT INTO equipment_calibrations (equipment_id, method_id, calibrated_at, amb_temp, amb_pres, amb_moist, values, result, file_url, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10) RETURNING id`,
		id, methodID, calibratedAt, ambTemp, ambPres, ambMoist, valuesJSON, result, fileURL, email).Scan(&calID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if err := s.recomputeCalibrationDates(r.Context(), id); err != nil {
		log.Printf("recompute calibration dates: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": calID})
}

// recomputeCalibrationDates — last_calibration = MAX(calibrated_at) записей журнала;
// next_calibration = last_calibration + calibration_interval_months, ЕСЛИ интервал
// задан, иначе NULL (не считаем то, для чего нет данных). Вызывается после КАЖДОЙ
// новой записи журнала — единственное место, где эти два поля пишутся.
func (s *Server) recomputeCalibrationDates(ctx context.Context, equipmentID int64) error {
	_, err := s.pool.Exec(ctx, `
UPDATE equipment e SET
	last_calibration = (SELECT MAX(calibrated_at) FROM equipment_calibrations WHERE equipment_id = e.id),
	next_calibration = CASE WHEN e.calibration_interval_months IS NULL THEN NULL
		ELSE (SELECT MAX(calibrated_at) FROM equipment_calibrations WHERE equipment_id = e.id)
			+ (e.calibration_interval_months || ' months')::interval END,
	updated_at = now()
WHERE e.id = $1`, equipmentID)
	return err
}

// ---- Привязка к методам (main/auxiliary) ----

type EquipmentMethodLink struct {
	MethodID int64  `json:"method_id"`
	Role     string `json:"role"`
}

// handleListAllMethodEquipment — вся таблица method_equipment одним запросом (2026-08-28,
// WP1 — по образцу уже существующего handleListAllEquipmentLinks): клиенту нужно узнать,
// сколько единиц "Основного" оборудования у КОНКРЕТНОГО метода (показывать ли селектор
// оборудования в форме результатов испытания), а не наоборот — один общий запрос дешевле,
// чем N+1 по каждому методу/оборудованию.
func (s *Server) handleListAllMethodEquipment(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(),
		`SELECT method_id, equipment_id, role FROM method_equipment ORDER BY method_id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	type methodEquipmentRow struct {
		MethodID    int64  `json:"method_id"`
		EquipmentID int64  `json:"equipment_id"`
		Role        string `json:"role"`
	}
	out := make([]methodEquipmentRow, 0, 16)
	for rows.Next() {
		var it methodEquipmentRow
		if err := rows.Scan(&it.MethodID, &it.EquipmentID, &it.Role); err != nil {
			continue
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

func (s *Server) handleListEquipmentMethods(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	rows, err := s.pool.Query(r.Context(),
		`SELECT method_id, role FROM method_equipment WHERE equipment_id = $1 ORDER BY method_id`, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]EquipmentMethodLink, 0, 4)
	for rows.Next() {
		var it EquipmentMethodLink
		if err := rows.Scan(&it.MethodID, &it.Role); err != nil {
			continue
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"methods": out})
}

// handleSetEquipmentMethod — POST /equipment/{id}/methods: привязывает метод к
// оборудованию (upsert). Роль связи ('main'/'auxiliary') клиент больше не
// присылает (2026-09-03) — берётся из equipment.type привязываемого оборудования:
// «Основное/Вспомогательное» теперь единая роль на всё оборудование, а не выбор
// на каждой связи с методом. calibration_curve.go продолжает читать role из
// method_equipment без изменений — эта колонка держится синхронной с
// equipment.type здесь и в handleUpdateEquipment (см. lims_refs.go).
func (s *Server) handleSetEquipmentMethod(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		MethodID int64 `json:"method_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.MethodID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "method_id is required"})
		return
	}
	var equipmentType string
	if err := s.pool.QueryRow(r.Context(), `SELECT type FROM equipment WHERE id = $1`, id).Scan(&equipmentType); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "equipment not found"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
INSERT INTO method_equipment (method_id, equipment_id, role) VALUES ($1, $2, $3)
ON CONFLICT (method_id, equipment_id) DO UPDATE SET role = EXCLUDED.role`,
		req.MethodID, id, equipmentType); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteEquipmentMethod(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	methodID, err := strconv.ParseInt(r.PathValue("method_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid method_id"})
		return
	}
	if _, err := s.pool.Exec(r.Context(),
		`DELETE FROM method_equipment WHERE equipment_id = $1 AND method_id = $2`, id, methodID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Документация на оборудование (список файлов) ----

type EquipmentDocument struct {
	ID        int64  `json:"id"`
	FileName  string `json:"file_name"`
	FileURL   string `json:"file_url"`
	FileSize  int64  `json:"file_size"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) handleListEquipmentDocuments(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
SELECT id, file_name, file_url, file_size, created_at FROM files
WHERE equipment_id = $1 AND purpose = 'equipment_doc' ORDER BY created_at DESC`, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]EquipmentDocument, 0, 8)
	for rows.Next() {
		var it EquipmentDocument
		var createdAt time.Time
		if err := rows.Scan(&it.ID, &it.FileName, &it.FileURL, &it.FileSize, &createdAt); err != nil {
			continue
		}
		it.CreatedAt = createdAt.Format(time.RFC3339)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out})
}

func (s *Server) handleUploadEquipmentDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file is required"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read file"})
		return
	}
	fileURL, _, err := s.uploadEquipmentFileBytes(r.Context(), id, header.Filename, data, currentEmail(r), "equipment_doc")
	if err != nil {
		log.Printf("equipment document upload: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "s3 error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file_url": fileURL})
}

func (s *Server) handleDeleteEquipmentDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	fileID, err := strconv.ParseInt(r.PathValue("file_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid file_id"})
		return
	}
	if _, err := s.pool.Exec(r.Context(),
		`DELETE FROM files WHERE id = $1 AND equipment_id = $2 AND purpose = 'equipment_doc'`, fileID, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Привязка оборудование↔оборудование (2026-08-26) ----
//
// Физическое прикрепление вспомогательного прибора к основному (напр. датчик
// к анализатору) — ОТДЕЛЬНО и независимо от method_equipment.role (тот решает,
// показывается ли блок калибровки для КОНКРЕТНОГО метода; этот — только
// группировку/отображение в общем списке оборудования, "как у документов":
// прибор, привязанный хотя бы к одному основному, больше не показывается
// отдельной карточкой верхнего уровня, только внутри карточки(-ек) своего
// основного прибора(-ов)). many-to-many — один вспомогательный прибор может
// быть привязан к нескольким основным.

type EquipmentLink struct {
	MainEquipmentID      int64 `json:"main_equipment_id"`
	AuxiliaryEquipmentID int64 `json:"auxiliary_equipment_id"`
}

// handleListAllEquipmentLinks — ВСЯ таблица связей одним запросом (не по одной
// единице оборудования) — клиент строит из неё и списки "мои вспомогательные"/
// "я вспомогательное для", и множество "скрыть из общего списка" за один проход,
// без N+1 запроса на каждую карточку верхнего уровня.
func (s *Server) handleListAllEquipmentLinks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(),
		`SELECT main_equipment_id, auxiliary_equipment_id FROM equipment_links ORDER BY main_equipment_id`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]EquipmentLink, 0, 8)
	for rows.Next() {
		var it EquipmentLink
		if err := rows.Scan(&it.MainEquipmentID, &it.AuxiliaryEquipmentID); err != nil {
			continue
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

// handleAddEquipmentAuxiliary — POST /equipment/{id}/auxiliaries: {id} становится
// ОСНОВНЫМ для указанного auxiliary_equipment_id. Тот же вызов обслуживает оба
// сценария UI ("привязать вспомогательный к этому основному" — {id} = основной;
// "привязать этот вспомогательный к основному" — {id} = выбранный основной,
// auxiliary_equipment_id = собственный id прибора со своей карточки).
func (s *Server) handleAddEquipmentAuxiliary(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		AuxiliaryEquipmentID int64 `json:"auxiliary_equipment_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.AuxiliaryEquipmentID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "auxiliary_equipment_id is required"})
		return
	}
	if req.AuxiliaryEquipmentID == id {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "оборудование не может быть привязано само к себе"})
		return
	}
	// Цепочки вспомогательного оборудования разрешены (2026-09-03, многоуровневое
	// дерево) — но не циклы: если id (будущий основной) уже входит в потомков
	// req.AuxiliaryEquipmentID (будущего вспомогательного), привязка замкнула бы
	// граф. Рекурсивный обход существующих equipment_links вниз от
	// AuxiliaryEquipmentID.
	var wouldCycle bool
	if err := s.pool.QueryRow(r.Context(), `
WITH RECURSIVE reachable AS (
	SELECT auxiliary_equipment_id AS eid FROM equipment_links WHERE main_equipment_id = $1
	UNION
	SELECT el.auxiliary_equipment_id FROM equipment_links el JOIN reachable r ON el.main_equipment_id = r.eid
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE eid = $2)`,
		req.AuxiliaryEquipmentID, id).Scan(&wouldCycle); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if wouldCycle {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "привязка образует цикл"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
INSERT INTO equipment_links (main_equipment_id, auxiliary_equipment_id) VALUES ($1, $2)
ON CONFLICT (main_equipment_id, auxiliary_equipment_id) DO NOTHING`, id, req.AuxiliaryEquipmentID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRemoveEquipmentAuxiliary(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	auxID, err := strconv.ParseInt(r.PathValue("auxiliary_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid auxiliary_id"})
		return
	}
	if _, err := s.pool.Exec(r.Context(),
		`DELETE FROM equipment_links WHERE main_equipment_id = $1 AND auxiliary_equipment_id = $2`, id, auxID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
