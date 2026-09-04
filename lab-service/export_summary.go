package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// ---- Сводный экспорт нескольких заявок в Excel (2026-09-04, по прямому
// запросу пользователя: "выгружать сводную таблицу по выбранным заявкам").
// В отличие от handleExportExcel (export.go, ровно одна заявка = один метод =
// один файл), здесь заявок много и методы у них могут различаться — поэтому
// один лист книги = один метод, одна строка листа = одна заявка этого метода.
// Загрузчики (loadMethodConfig/loadAggregatedRow/loadStatsRow) те же самые,
// что у одиночного экспорта — просто вызываются в цикле по группе заявок
// одного метода вместо одной заявки. ----

// exportSummaryRequest — тело POST /requests/export-summary.xlsx.
type exportSummaryRequest struct {
	RequestIDs []int64 `json:"request_ids"`
}

// requestIdentity — идентификационные колонки заявки для строки сводной
// таблицы. Отдельный узкий тип вместо loadRequest (requests.go) — loadRequest
// тянет ещё и файлы заявки отдельным запросом на каждую (loadRequestFiles),
// что здесь не нужно и было бы N+1 на весь список.
type requestIdentity struct {
	ID             int64
	MethodID       int64
	CustomerNumber string
	LabNumber      string
	ExternalID     string
	OwnerEmail     string
	CreatedAt      time.Time
	ObjectID       int64
}

// loadRequestIdentities — батч-загрузка идентификационных колонок ОДНИМ
// запросом (request_id = ANY($1)) — тот же паттерн, что enrichResultCompliance
// (requests.go) уже использует для батч-чтения aggregated_results по списку id.
func (s *Server) loadRequestIdentities(ctx context.Context, ids []int64) ([]requestIdentity, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, COALESCE(method_id, 0), customer_number, lab_number, external_id, owner_email,
	created_at, COALESCE(object_id, 0)
FROM requests WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]requestIdentity, 0, len(ids))
	for rows.Next() {
		var it requestIdentity
		if err := rows.Scan(&it.ID, &it.MethodID, &it.CustomerNumber, &it.LabNumber, &it.ExternalID,
			&it.OwnerEmail, &it.CreatedAt, &it.ObjectID); err != nil {
			log.Printf("loadRequestIdentities scan: %v", err)
			continue
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// loadObjectNames — id -> name одним запросом по списку различных object_id
// (собранных вызывающей стороной из батча заявок), а не запросом на заявку.
func (s *Server) loadObjectNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM objects WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Printf("loadObjectNames scan: %v", err)
			continue
		}
		out[id] = name
	}
	return out, rows.Err()
}

// methodSheetMeta — код/название метода для имени листа.
type methodSheetMeta struct {
	Code string
	Name string
}

// loadMethodSheetMetas — code/name методов одним запросом по списку id (не по
// одному запросу на каждую группу).
func (s *Server) loadMethodSheetMetas(ctx context.Context, ids []int64) (map[int64]methodSheetMeta, error) {
	out := make(map[int64]methodSheetMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, code, name FROM methods WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var meta methodSheetMeta
		if err := rows.Scan(&id, &meta.Code, &meta.Name); err != nil {
			log.Printf("loadMethodSheetMetas scan: %v", err)
			continue
		}
		out[id] = meta
	}
	return out, rows.Err()
}

// sheetNameForbidden — символы, запрещённые Excel в имени листа.
var sheetNameForbidden = strings.NewReplacer(
	":", "", "\\", "", "/", "", "?", "", "*", "", "[", "", "]", "")

// sanitizeSheetName приводит произвольную строку к допустимому имени листа
// Excel: без ': \ / ? * [ ]', не длиннее 31 символа (лимит формата), не пустое.
func sanitizeSheetName(name string) string {
	name = strings.TrimSpace(sheetNameForbidden.Replace(name))
	if name == "" {
		return "Sheet"
	}
	r := []rune(name)
	if len(r) > 31 {
		r = r[:31]
	}
	return string(r)
}

// uniqueSheetName добавляет " (2)", " (3)", ... при коллизии с уже
// использованным именем листа, укорачивая базу так, чтобы уложиться в лимит
// 31 символ вместе с суффиксом. Коллизии ожидаются редко (реальный ключ
// группировки — method_id, а не код/название), но excelize.NewSheet иначе
// вернёт ошибку на дубликате и оборвёт весь экспорт.
func uniqueSheetName(base string, used map[string]bool) string {
	if !used[base] {
		used[base] = true
		return base
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		maxBase := 31 - len([]rune(suffix))
		if maxBase < 0 {
			maxBase = 0
		}
		r := []rune(base)
		if len(r) > maxBase {
			r = r[:maxBase]
		}
		candidate := string(r) + suffix
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// handleExportSummaryExcel — POST /requests/export-summary.xlsx. Видимость —
// как у одиночного экспорта (requireLabRead, см. handleExportExcel), но по
// списку id и БЕЗ ошибки на отдельных отказах: недоступные заявки молча
// исключаются из выборки (тот же принцип, что и общий список видимых заявок,
// loadVisibleRequests в requests.go, — не раскрывать вызывающему сам факт
// существования id, к которым у него нет доступа).
func (s *Server) handleExportSummaryExcel(w http.ResponseWriter, r *http.Request) {
	var body exportSummaryRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if len(body.RequestIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "request_ids is required"})
		return
	}

	ctx := r.Context()
	email := currentEmail(r)
	visible := make([]int64, 0, len(body.RequestIDs))
	for _, id := range body.RequestIDs {
		ok, err := s.requireLabRead(ctx, email, id)
		if err != nil {
			log.Printf("handleExportSummaryExcel requireLabRead id=%d: %v", id, err)
			continue
		}
		if ok {
			visible = append(visible, id)
		}
	}
	if len(visible) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "нет доступных заявок для экспорта"})
		return
	}

	identities, err := s.loadRequestIdentities(ctx, visible)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if len(identities) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "нет доступных заявок для экспорта"})
		return
	}

	// object_id -> name, одним запросом по различным id, встреченным в батче.
	objectIDs := make([]int64, 0, len(identities))
	seenObj := make(map[int64]bool, len(identities))
	for _, it := range identities {
		if it.ObjectID > 0 && !seenObj[it.ObjectID] {
			seenObj[it.ObjectID] = true
			objectIDs = append(objectIDs, it.ObjectID)
		}
	}
	objectNames, err := s.loadObjectNames(ctx, objectIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Группировка по методу — порядок групп следует порядку первого появления
	// метода в identities (который сохраняет порядок ANY($1)/визибл-фильтрации,
	// т.е. фактически порядок исходного request_ids за вычетом недоступных).
	var methodOrder []int64
	groups := make(map[int64][]requestIdentity, 8)
	for _, it := range identities {
		if _, ok := groups[it.MethodID]; !ok {
			methodOrder = append(methodOrder, it.MethodID)
		}
		groups[it.MethodID] = append(groups[it.MethodID], it)
	}
	methodMetas, err := s.loadMethodSheetMetas(ctx, methodOrder)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	f := excelize.NewFile()
	defer f.Close()

	usedSheetNames := make(map[string]bool, len(methodOrder))
	firstSheetDone := false

	for _, methodID := range methodOrder {
		group := groups[methodID]

		cfg, err := s.loadMethodConfig(ctx, methodID)
		if err != nil {
			// Одна битая конфигурация метода не должна валить весь сводный
			// экспорт по остальным методам — пропускаем только этот лист.
			log.Printf("handleExportSummaryExcel loadMethodConfig method=%d: %v", methodID, err)
			continue
		}
		attrsByID := methodAttributesByID(cfg)

		// Загружаем agg/stats на каждую заявку группы и сливаем их в одну
		// карту на заявку — agg-ключи первыми, затем stats-ключи поверх (тот
		// же порядок, что writeKV в export.go применяет при записи obou карт
		// подряд в один KV-лист; там это не настоящий merge — обе карты пишутся
		// как отдельные строки без дедупликации ключей — поэтому реального
		// прецедента "кто выигрывает при коллизии" в коде нет; здесь коллизия
		// разрешается в пользу stats, т.к. именно stats применяется последним
		// согласно заданному порядку "agg keys first, then stats keys").
		type rowData struct {
			identity requestIdentity
			merged   map[string]any
		}
		rows := make([]rowData, 0, len(group))
		keySet := make(map[string]bool, 16)
		for _, it := range group {
			agg, _ := s.loadAggregatedRow(ctx, it.ID, methodID)
			stats, _ := s.loadStatsRow(ctx, it.ID, methodID)
			merged := make(map[string]any, len(agg)+len(stats))
			for k, v := range agg {
				merged[k] = v
				keySet[k] = true
			}
			for k, v := range stats {
				merged[k] = v
				keySet[k] = true
			}
			rows = append(rows, rowData{identity: it, merged: merged})
		}
		resultKeys := make([]string, 0, len(keySet))
		for k := range keySet {
			resultKeys = append(resultKeys, k)
		}
		sort.Strings(resultKeys)

		meta := methodMetas[methodID]
		base := meta.Code
		if base == "" {
			base = meta.Name
		}
		if base == "" {
			base = fmt.Sprintf("Метод %d", methodID)
		}
		sheetName := uniqueSheetName(sanitizeSheetName(base), usedSheetNames)

		if !firstSheetDone {
			// excelize.NewFile() уже создаёт лист по умолчанию "Sheet1" —
			// переименовываем его под первую группу вместо того, чтобы
			// оставлять лишний пустой лист (тот же приём, что buildExportXlsx
			// в export.go делает для листа "Серии").
			if err := f.SetSheetName("Sheet1", sheetName); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "xlsx: " + err.Error()})
				return
			}
			firstSheetDone = true
		} else {
			if _, err := f.NewSheet(sheetName); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "xlsx: " + err.Error()})
				return
			}
		}

		header := make([]any, 0, 5+len(resultKeys))
		header = append(header, "№ заявки", "Внешний ID", "Объект", "Заказчик", "Дата создания")
		for _, k := range resultKeys {
			label := k
			if a, ok := attrsByID[k]; ok && a.Name != "" {
				label = a.Name
			}
			header = append(header, label)
		}
		if err := f.SetSheetRow(sheetName, "A1", &header); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "xlsx: " + err.Error()})
			return
		}

		for i, rd := range rows {
			num := rd.identity.CustomerNumber
			if num == "" {
				num = rd.identity.LabNumber
			}
			if num == "" {
				num = fmt.Sprintf("#%d", rd.identity.ID)
			}
			row := make([]any, 0, 5+len(resultKeys))
			row = append(row, num, rd.identity.ExternalID, objectNames[rd.identity.ObjectID],
				rd.identity.OwnerEmail, rd.identity.CreatedAt.Format(time.RFC3339))
			for _, k := range resultKeys {
				if v, ok := rd.merged[k]; ok {
					// fmtVal (protocol.go) — та же функция, которой export.go
					// приводит и серии, и agg/stats-значения к строке для
					// ячейки; здесь применяется по той же причине (числа с
					// плавающей точкой без хвоста нулей, всё прочее — как есть).
					row = append(row, fmtVal(v))
				} else {
					row = append(row, "")
				}
			}
			cell, err := excelize.CoordinatesToCellName(1, i+2)
			if err != nil {
				continue
			}
			if err := f.SetSheetRow(sheetName, cell, &row); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "xlsx: " + err.Error()})
				return
			}
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "xlsx: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"xlsx_base64": toBase64(buf.Bytes()),
	})
}
