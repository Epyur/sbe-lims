package main

import (
	"net/http"
	"strconv"
	"time"
)

// handleDashboard возвращает агрегаты по заявкам лаборатории за период.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	labID, _ := strconv.ParseInt(r.URL.Query().Get("lab_id"), 10, 64)
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "month"
	}
	// период в днях
	days := 30
	switch period {
	case "week":
		days = 7
	case "quarter":
		days = 90
	case "year":
		days = 365
	}
	since := time.Now().UTC().AddDate(0, 0, -days)

	// базовый фильтр: заявки видимые + (если lab_id задан) заявки этой лаборатории.
	// requests.lab_id — конкретная лаба, выбранная при создании заявки (методы теперь
	// могут принадлежать нескольким лабам через method_labs, поэтому фильтр по
	// methods.lab_id больше не имеет смысла — см. lab-service/AGENTS.md).
	filter := ""
	args := []any{since}
	if labID > 0 {
		filter = `AND lab_id = $2`
		args = append(args, labID)
	}

	// по статусам
	byStatus := map[string]int{"new": 0, "received": 0, "processing": 0, "completed": 0}
	rows, err := s.pool.Query(r.Context(), `
SELECT status, COUNT(*) FROM requests
WHERE created_at >= $1 `+filter+` GROUP BY status`, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			continue
		}
		byStatus[st] = n
	}

	// по методам
	type methodCount struct {
		MethodID int64 `json:"method_id"`
		Count    int   `json:"count"`
	}
	var byMethod []methodCount
	filterReq := filter
	if labID > 0 {
		filterReq = `AND req.lab_id = $2`
	}
	rows, err = s.pool.Query(r.Context(), `
SELECT req.method_id, COUNT(*) FROM requests req
WHERE req.created_at >= $1 `+filterReq+` AND req.method_id IS NOT NULL
GROUP BY req.method_id ORDER BY COUNT(*) DESC LIMIT 20`, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var mc methodCount
		if err := rows.Scan(&mc.MethodID, &mc.Count); err != nil {
			continue
		}
		byMethod = append(byMethod, mc)
	}

	// total + completed in period
	var total, completed int
	_ = s.pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM requests WHERE created_at >= $1 `+filter, args...).Scan(&total)
	_ = s.pool.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM requests WHERE status = 'completed' AND updated_at >= $1 `+filter, args...).Scan(&completed)

	writeJSON(w, http.StatusOK, map[string]any{
		"by_status":           byStatus,
		"by_method":           byMethod,
		"total":               total,
		"completed_in_period": completed,
		"period":              period,
	})
}
