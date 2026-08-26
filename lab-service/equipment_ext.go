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

type EquipmentCalibration struct {
	ID           int64  `json:"id"`
	EquipmentID  int64  `json:"equipment_id"`
	CalibratedAt string `json:"calibrated_at"`
	Result       string `json:"result"`
	FileURL      string `json:"file_url"`
	CreatedBy    string `json:"created_by"`
	CreatedAt    string `json:"created_at"`
}

func (s *Server) handleListEquipmentCalibrations(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	rows, err := s.pool.Query(r.Context(), `
SELECT id, equipment_id, calibrated_at, result, file_url, created_by, created_at
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
		if err := rows.Scan(&it.ID, &it.EquipmentID, &calibratedAt, &it.Result, &it.FileURL, &it.CreatedBy, &createdAt); err != nil {
			continue
		}
		it.CalibratedAt = calibratedAt.Format("2006-01-02")
		it.CreatedAt = createdAt.Format(time.RFC3339)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"calibrations": out})
}

// handleCreateEquipmentCalibration — POST /equipment/{id}/calibrations, multipart
// (файл необязателен: поля calibrated_at/result + опциональный "file"). После вставки
// пересчитывает equipment.last_calibration/next_calibration (recomputeCalibrationDates).
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
	result := strings.TrimSpace(r.FormValue("result"))
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
INSERT INTO equipment_calibrations (equipment_id, calibrated_at, result, file_url, created_by)
VALUES ($1, $2, $3, $4, $5) RETURNING id`, id, calibratedAt, result, fileURL, email).Scan(&calID)
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

// handleSetEquipmentMethod — POST /equipment/{id}/methods: создаёт или обновляет роль
// (upsert, одна связь на пару method_id+equipment_id — повторный вызов с новой ролью
// просто меняет её, как и везде в проекте, где роль редактируется на месте).
func (s *Server) handleSetEquipmentMethod(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req struct {
		MethodID int64  `json:"method_id"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Role != "main" && req.Role != "auxiliary" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role must be main or auxiliary"})
		return
	}
	if req.MethodID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "method_id is required"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
INSERT INTO method_equipment (method_id, equipment_id, role) VALUES ($1, $2, $3)
ON CONFLICT (method_id, equipment_id) DO UPDATE SET role = EXCLUDED.role`,
		req.MethodID, id, req.Role); err != nil {
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
