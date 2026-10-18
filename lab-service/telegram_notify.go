package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Telegram-уведомления по проектам (2026-10-18). Дизайн:
// docs/superpowers/specs/2026-10-18-sbe-lims-telegram-project-notifications-design.md,
// план — docs/superpowers/plans/2026-10-18-sbe-lims-telegram-project-notifications-plan.md.
//
// Правило проекта — (project_id, method_id). Наследование идёт по методу: у
// проекта ровно одна строка на метод — активная либо исключение (deleted).
// Отправляет фоновая очередь через portal-embed (единственный держатель
// SOCKS-туннеля к Telegram).

// telegramChannel — один канал правила (id + подпись для человека). ThreadID —
// тема форум-супергруппы (message_thread_id): 0 — не задана, сообщение уходит в
// «General».
type telegramChannel struct {
	ChatID   string `json:"chat_id"`
	Label    string `json:"label"`
	ThreadID int64  `json:"thread_id"`
}

// telegramRule — правило проекта для одного метода.
type telegramRule struct {
	MethodID       int64             `json:"method_id"`
	Deleted        bool              `json:"deleted"`
	Channels       []telegramChannel `json:"channels"`
	OnCreated      bool              `json:"on_created"`
	OnCompleted    bool              `json:"on_completed"`
	WithAttachment bool              `json:"with_attachment"`
}

// telegramRuleMatchesEvent — относится ли правило к событию "created"/"completed".
func telegramRuleMatchesEvent(r *telegramRule, event string) bool {
	if r == nil {
		return false
	}
	switch event {
	case "created":
		return r.OnCreated
	case "completed":
		return r.OnCompleted
	}
	return false
}

// telegramRuleLookup — доступ к одной строке правила и id родителя проекта.
// found=false — у проекта своей строки по этому методу нет (идём выше).
type telegramRuleLookup func(projectID, methodID int64) (rule *telegramRule, parentID int64, found bool, err error)

// resolveTelegramRule поднимается от проекта вверх по дереву до первой строки
// по методу: активная — результат; исключение (deleted) — правил нет и выше не
// смотрим; нигде нет — nil. Второе значение — id проекта-источника (0, если
// правила нет) — нужно интерфейсу для пометки «унаследовано от ‹родитель›».
// Заявка без проекта (startProjectID == 0) — nil.
func resolveTelegramRule(methodID, startProjectID int64, lookup telegramRuleLookup) (*telegramRule, int64, error) {
	p := startProjectID
	for p != 0 {
		rule, parentID, found, err := lookup(p, methodID)
		if err != nil {
			return nil, 0, err
		}
		if found {
			if rule.Deleted {
				return nil, 0, nil
			}
			return rule, p, nil
		}
		p = parentID
	}
	return nil, 0, nil
}

// telegramRuleLookup связывает чистый resolveTelegramRule с базой: одна строка
// правила проекта и id его родителя.
func (s *Server) telegramRuleLookup(ctx context.Context) telegramRuleLookup {
	return func(projectID, methodID int64) (*telegramRule, int64, bool, error) {
		rule := &telegramRule{MethodID: methodID}
		var channelsRaw []byte
		err := s.pool.QueryRow(ctx, `
SELECT deleted, channels, on_created, on_completed, with_attachment
FROM telegram_notify_rules WHERE project_id = $1 AND method_id = $2`,
			projectID, methodID).
			Scan(&rule.Deleted, &channelsRaw, &rule.OnCreated, &rule.OnCompleted, &rule.WithAttachment)
		found := true
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
		} else if err != nil {
			return nil, 0, false, err
		}
		if found && len(channelsRaw) > 0 {
			_ = json.Unmarshal(channelsRaw, &rule.Channels)
		}

		var parentID int64
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(parent_id, 0) FROM projects WHERE id = $1`, projectID).Scan(&parentID); err != nil {
			return nil, 0, false, err
		}
		if !found {
			return nil, parentID, false, nil
		}
		return rule, parentID, true, nil
	}
}

// ---- конфиг моста до portal-embed ----

// portalEmbedURL — внутренний адрес portal-embed (та же docker-сеть). Через
// него уходит и текст, и документ: только он держит SOCKS-туннель к Telegram.
func portalEmbedURL() string {
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("PORTAL_EMBED_INTERNAL_URL")), "/")
	if v == "" {
		v = "http://portal-embed:3000"
	}
	return v
}

// telegramBridgeSecret — общий статический секрет auth-service/portal-embed
// (защищает /internal/telegram/*), см. спеку.
func telegramBridgeSecret() string {
	return strings.TrimSpace(os.Getenv("TELEGRAM_BRIDGE_SECRET"))
}

// sendTelegramText — POST /internal/telegram/send у portal-embed. threadID > 0 —
// тема форум-супергруппы (message_thread_id).
func (s *Server) sendTelegramText(ctx context.Context, chatID, text string, threadID int64) error {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if threadID > 0 {
		payload["message_thread_id"] = threadID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.postPortalEmbed(ctx, "/internal/telegram/send", body)
}

// sendTelegramDocument — POST /internal/telegram/send-document у portal-embed
// (файл передаётся base64: JSON-канал не таскает бинарь). caption — короткая
// подпись к файлу (тот же текст уведомления), уходит вместе с документом.
// threadID > 0 — тема форум-супергруппы.
func (s *Server) sendTelegramDocument(ctx context.Context, chatID, fileName string, content []byte, caption string, threadID int64) error {
	payload := map[string]any{
		"chat_id":        chatID,
		"filename":       fileName,
		"content_base64": base64.StdEncoding.EncodeToString(content),
	}
	if strings.TrimSpace(caption) != "" {
		payload["caption"] = caption
	}
	if threadID > 0 {
		payload["message_thread_id"] = threadID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.postPortalEmbed(ctx, "/internal/telegram/send-document", body)
}

func (s *Server) postPortalEmbed(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, portalEmbedURL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Secret", telegramBridgeSecret())
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("portal-embed ответил %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// validateTelegramRules — набор правил проекта приходит на замену целиком:
// один метод — одна строка; исключение каналов не несёт; у активного правила
// канал обязателен и непуст.
func validateTelegramRules(rules []telegramRule) error {
	seen := map[int64]bool{}
	for i := range rules {
		r := rules[i]
		if r.MethodID <= 0 {
			return errors.New("method_id is required")
		}
		if seen[r.MethodID] {
			return fmt.Errorf("duplicate method_id %d", r.MethodID)
		}
		seen[r.MethodID] = true
		if r.Deleted {
			if len(r.Channels) > 0 {
				return fmt.Errorf("deleted rule for method %d must have no channels", r.MethodID)
			}
			continue
		}
		// Канал-центричная модель (2026-10-18): у метода в проекте ровно один
		// канал. Несколько каналов на метод не поддерживаем — так редактирование
		// «канал → методы» однозначно.
		if len(r.Channels) != 1 {
			return fmt.Errorf("rule for method %d must have exactly one channel", r.MethodID)
		}
		for _, c := range r.Channels {
			if strings.TrimSpace(c.ChatID) == "" {
				return fmt.Errorf("rule for method %d has empty chat_id", r.MethodID)
			}
			if c.ThreadID < 0 {
				return fmt.Errorf("rule for method %d has negative thread_id", r.MethodID)
			}
		}
	}
	return nil
}

// loadOwnTelegramRules — свои строки проекта (включая исключения).
func (s *Server) loadOwnTelegramRules(ctx context.Context, projectID int64) ([]telegramRule, error) {
	rows, err := s.pool.Query(ctx, `
SELECT method_id, deleted, channels, on_created, on_completed, with_attachment
FROM telegram_notify_rules WHERE project_id = $1 ORDER BY method_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]telegramRule, 0, 8)
	for rows.Next() {
		var r telegramRule
		var channels []byte
		if err := rows.Scan(&r.MethodID, &r.Deleted, &channels, &r.OnCreated, &r.OnCompleted, &r.WithAttachment); err != nil {
			continue
		}
		if len(channels) > 0 {
			_ = json.Unmarshal(channels, &r.Channels)
		}
		if r.Channels == nil {
			r.Channels = []telegramChannel{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// projectNames — id → название проекта одним запросом.
func (s *Server) projectNames(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM projects WHERE id = ANY($1)`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err == nil {
			out[id] = name
		}
	}
	return out
}

// inheritedTelegramRule — унаследованное правило с указанием проекта-источника.
type inheritedTelegramRule struct {
	telegramRule
	SourceProjectID   int64  `json:"source_project_id"`
	SourceProjectName string `json:"source_project_name"`
}

// handleGetTelegramRules — GET /api/lab/projects/{id}/telegram-rules (admin):
// свои строки проекта и вычисленные унаследованные (для методов, по которым
// своей строки у проекта нет).
func (s *Server) handleGetTelegramRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || projectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	own, err := s.loadOwnTelegramRules(ctx, projectID)
	if err != nil {
		log.Printf("telegram rules: load own: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	ownMethods := map[int64]bool{}
	for _, rr := range own {
		ownMethods[rr.MethodID] = true
	}

	// Методы, упомянутые в правилах предков (и самого проекта — отсеются по
	// ownMethods). Обход дерева вверх одним рекурсивным CTE.
	rows, err := s.pool.Query(ctx, `
WITH RECURSIVE up AS (
	SELECT id, parent_id FROM projects WHERE id = $1
	UNION ALL
	SELECT p.id, p.parent_id FROM projects p JOIN up ON p.id = up.parent_id
)
SELECT DISTINCT r.method_id FROM telegram_notify_rules r JOIN up ON r.project_id = up.id`, projectID)
	if err != nil {
		log.Printf("telegram rules: candidates: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	candidates := make([]int64, 0, 8)
	for rows.Next() {
		var mid int64
		if err := rows.Scan(&mid); err == nil {
			candidates = append(candidates, mid)
		}
	}
	rows.Close()

	lookup := s.telegramRuleLookup(ctx)
	inherited := make([]inheritedTelegramRule, 0, len(candidates))
	sourceIDs := make([]int64, 0, len(candidates))
	for _, mid := range candidates {
		if ownMethods[mid] {
			continue
		}
		rule, srcID, err := resolveTelegramRule(mid, projectID, lookup)
		if err != nil || rule == nil {
			continue
		}
		if rule.Channels == nil {
			rule.Channels = []telegramChannel{}
		}
		inherited = append(inherited, inheritedTelegramRule{telegramRule: *rule, SourceProjectID: srcID})
		sourceIDs = append(sourceIDs, srcID)
	}
	names := s.projectNames(ctx, sourceIDs)
	for i := range inherited {
		inherited[i].SourceProjectName = names[inherited[i].SourceProjectID]
	}
	writeJSON(w, http.StatusOK, map[string]any{"own": own, "inherited": inherited})
}

// handlePutTelegramRules — PUT /api/lab/projects/{id}/telegram-rules (admin):
// заменить свои правила проекта целиком.
func (s *Server) handlePutTelegramRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || projectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var body struct {
		Rules []telegramRule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if err := validateTelegramRules(body.Rules); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)`, projectID).Scan(&exists); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM telegram_notify_rules WHERE project_id = $1`, projectID); err != nil {
		log.Printf("telegram rules: delete: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	for _, rr := range body.Rules {
		channels := rr.Channels
		if channels == nil {
			channels = []telegramChannel{}
		}
		raw, err := json.Marshal(channels)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO telegram_notify_rules (project_id, method_id, deleted, channels, on_created, on_completed, with_attachment)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			projectID, rr.MethodID, rr.Deleted, raw, rr.OnCreated, rr.OnCompleted, rr.WithAttachment); err != nil {
			log.Printf("telegram rules: insert: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTestTelegram — POST /api/lab/telegram-rules/test (admin): проверочное
// сообщение в канал; ошибка доставки отдаётся текстом (бот не админ канала и
// т.п. — частая причина, человеку нужно её увидеть).
func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatID   string `json:"chat_id"`
		ThreadID int64  `json:"thread_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	chatID := strings.TrimSpace(body.ChatID)
	if chatID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "chat_id is required"})
		return
	}
	if body.ThreadID < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "negative thread_id"})
		return
	}
	if err := s.sendTelegramText(r.Context(), chatID, "✅ Проверка уведомлений ЛИМС — канал настроен.", body.ThreadID); err != nil {
		log.Printf("telegram test: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListTelegramLog — GET /api/lab/requests/{id}/telegram-log: журнал
// отправок заявки для её карточки.
func (s *Server) handleListTelegramLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || requestID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if ok, err := s.requireLabRead(ctx, currentEmail(r), requestID); err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: not lab member"})
		return
	}
	rows, err := s.pool.Query(ctx, `
SELECT event, chat_id, status, attempts, last_error, created_at, sent_at
FROM telegram_notify_outbox WHERE request_id = $1 ORDER BY id DESC`, requestID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 8)
	for rows.Next() {
		var event, chatID, status, lastErr string
		var attempts int
		var createdAt time.Time
		var sentAt *time.Time
		if err := rows.Scan(&event, &chatID, &status, &attempts, &lastErr, &createdAt, &sentAt); err != nil {
			continue
		}
		entry := map[string]any{
			"event": event, "chat_id": chatID, "status": status,
			"attempts": attempts, "last_error": lastErr,
			"created_at": createdAt.Format(time.RFC3339Nano),
		}
		if sentAt != nil {
			entry["sent_at"] = sentAt.Format(time.RFC3339Nano)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": out})
}

// ---- события: постановка уведомлений в очередь ----

// telegramNames — человекочитаемые подписи для сообщения.
type telegramNames struct {
	ProjectName string
	MethodLabel string
	ObjectName  string
}

// portalRequestURL — ссылка на заявку в веб-портале. База переопределяема
// (LAB_PORTAL_BASE_URL), значение по умолчанию — прод.
func portalRequestURL(id int64) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("LAB_PORTAL_BASE_URL")), "/")
	if base == "" {
		base = "https://epyur.fvds.ru/app"
	}
	return fmt.Sprintf("%s/#/requests/%d", base, id)
}

// formatTelegramDate — «DD.MM.YYYY HH:MM» из RFC3339Nano; не разобралось —
// как есть (первые 10 символов, если похоже на дату).
func formatTelegramDate(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Format("02.01.2006 15:04")
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func (s *Server) loadTelegramNames(ctx context.Context, req *Request) telegramNames {
	var n telegramNames
	if req.ProjectID > 0 {
		_ = s.pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, req.ProjectID).Scan(&n.ProjectName)
	}
	if req.MethodID > 0 {
		var code, name string
		if err := s.pool.QueryRow(ctx, `SELECT code, name FROM methods WHERE id = $1`, req.MethodID).Scan(&code, &name); err == nil {
			n.MethodLabel = strings.TrimSpace(code)
			if strings.TrimSpace(name) != "" {
				n.MethodLabel = strings.TrimSpace(code + " · " + name)
			}
		}
	}
	if req.ObjectID > 0 {
		_ = s.pool.QueryRow(ctx, `SELECT name FROM objects WHERE id = $1`, req.ObjectID).Scan(&n.ObjectName)
	}
	return n
}

// buildTelegramText — текст уведомления по событию. Пустые поля не печатаются
// (без «: »-хвостов).
func buildTelegramText(req *Request, event string, names telegramNames) string {
	var b strings.Builder
	if event == "completed" {
		b.WriteString("✅ Выполнена заявка №" + req.CustomerNumber + "\n")
	} else {
		b.WriteString("🆕 Создана заявка №" + req.CustomerNumber + "\n")
	}
	field := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			b.WriteString(label + ": " + value + "\n")
		}
	}
	field("Проект", names.ProjectName)
	field("Метод", names.MethodLabel)
	field("Объект", names.ObjectName)
	field("Заказчик", req.OwnerEmail)
	if event == "completed" {
		if d := formatTelegramDate(req.CompletedAt); d != "" {
			b.WriteString("Завершена: " + d + "\n")
		}
		field("Результат", req.Result)
		field("Соответствие", req.Compliance)
	} else if d := formatTelegramDate(req.CreatedAt); d != "" {
		b.WriteString("Создана: " + d + "\n")
	}
	b.WriteString("🔗 " + portalRequestURL(req.ID))
	return b.String()
}

// triggerTelegramCreated / triggerTelegramCompleted — точки входа из мест
// создания и завершения заявки. Best-effort: вызываются в горутине, ошибки
// только логируются (как triggerCompletionEmails в outbound_email.go).
func (s *Server) triggerTelegramCreated(ctx context.Context, requestID int64) {
	s.triggerTelegramEvent(ctx, requestID, "created")
}

func (s *Server) triggerTelegramCompleted(ctx context.Context, requestID int64) {
	s.triggerTelegramEvent(ctx, requestID, "completed")
}

func (s *Server) triggerTelegramEvent(ctx context.Context, requestID int64, event string) {
	req, err := s.loadRequest(ctx, requestID)
	if err != nil || req == nil {
		log.Printf("telegram notify: load request %d: %v", requestID, err)
		return
	}
	if req.ProjectID <= 0 || req.MethodID <= 0 {
		return
	}
	rule, _, err := resolveTelegramRule(req.MethodID, req.ProjectID, s.telegramRuleLookup(ctx))
	if err != nil {
		log.Printf("telegram notify: resolve rule request %d: %v", requestID, err)
		return
	}
	if rule == nil || !telegramRuleMatchesEvent(rule, event) || len(rule.Channels) == 0 {
		return
	}
	if event == "completed" {
		// enrichResultCompliance правит элементы ПЕРЕДАННОГО среза — на копии
		// результат потерялся бы, поэтому забираем изменённую запись обратно.
		reqs := []Request{*req}
		_ = s.enrichResultCompliance(ctx, reqs)
		*req = reqs[0]
	}
	text := buildTelegramText(req, event, s.loadTelegramNames(ctx, req))
	withAttachment := rule.WithAttachment && event == "completed"
	enqueued := false
	for _, ch := range rule.Channels {
		chatID := strings.TrimSpace(ch.ChatID)
		if chatID == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
INSERT INTO telegram_notify_outbox (request_id, event, chat_id, text, with_attachment, thread_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING`, requestID, event, chatID, text, withAttachment, ch.ThreadID); err != nil {
			log.Printf("telegram notify: enqueue request %d chat %s: %v", requestID, chatID, err)
			continue
		}
		enqueued = true
	}
	if enqueued {
		s.nudgeTelegramNotify()
	}
}

// ---- очередь отправки ----

const (
	telegramNotifyInterval    = 30 * time.Second
	telegramNotifyBatch       = 20
	maxTelegramNotifyAttempts = 10
)

// nextAttemptStatus — статус строки после очередного сбоя: attempts уже
// включает текущую неудачную попытку. Потолок попыток — прекращаем авто-повтор.
func nextAttemptStatus(attempts int) string {
	if attempts+1 >= maxTelegramNotifyAttempts {
		return "failed"
	}
	return "pending"
}

// attachReport — прикладывать ли отчёт: только когда правило просит вложение и
// отчёт реально собрался (пустой DOCX — шлём текстом).
func attachReport(withAttachment bool, docxLen int) bool {
	return withAttachment && docxLen > 0
}

// startTelegramNotifyJob — фоновая отправка очереди (2026-10-18): тикер +
// «толчок» от постановки события. Образец — startUploadFlushWorker (S3Store).
func (s *Server) startTelegramNotifyJob() {
	go func() {
		ticker := time.NewTicker(telegramNotifyInterval)
		defer ticker.Stop()
		s.runTelegramNotifyOnce(context.Background())
		for {
			select {
			case <-ticker.C:
				s.runTelegramNotifyOnce(context.Background())
			case <-s.notifyWake:
				s.runTelegramNotifyOnce(context.Background())
			}
		}
	}()
}

type telegramOutboxRow struct {
	id             int64
	requestID      int64
	chatID         string
	text           string
	withAttachment bool
	threadID       int64
}

func (s *Server) runTelegramNotifyOnce(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `
SELECT id, request_id, chat_id, text, with_attachment, thread_id
FROM telegram_notify_outbox WHERE status = 'pending'
ORDER BY id LIMIT $1`, telegramNotifyBatch)
	if err != nil {
		log.Printf("telegram notify worker: select: %v", err)
		return
	}
	items := make([]telegramOutboxRow, 0, telegramNotifyBatch)
	for rows.Next() {
		var it telegramOutboxRow
		if err := rows.Scan(&it.id, &it.requestID, &it.chatID, &it.text, &it.withAttachment, &it.threadID); err == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	for _, it := range items {
		s.sendTelegramOutboxRow(ctx, it)
	}
}

func (s *Server) sendTelegramOutboxRow(ctx context.Context, it telegramOutboxRow) {
	var sendErr error
	attachmentSent := false
	if it.withAttachment {
		res, err := s.ensureReport(ctx, it.requestID)
		switch {
		case err != nil:
			log.Printf("telegram notify worker: report request %d: %v", it.requestID, err)
		case res == nil || !attachReport(true, len(res.DOCX)):
			log.Printf("telegram notify worker: report request %d empty — sending text", it.requestID)
		default:
			attachmentSent = true
			// Подпись к файлу — тот же текст уведомления (номер, проект, метод,
			// объект, заказчик, результат/соответствие, ссылка), спека: «отчёт
			// приходит документом с короткой подписью».
			sendErr = s.sendTelegramDocument(ctx, it.chatID, reportFilename(it.requestID), res.DOCX, it.text, it.threadID)
		}
	}
	if !attachmentSent {
		sendErr = s.sendTelegramText(ctx, it.chatID, it.text, it.threadID)
	}
	if sendErr == nil {
		if _, err := s.pool.Exec(ctx, `
UPDATE telegram_notify_outbox SET status = 'sent', sent_at = now(), attempts = attempts + 1
WHERE id = $1`, it.id); err != nil {
			log.Printf("telegram notify worker: mark sent %d: %v", it.id, err)
		}
		return
	}

	var attempts int
	_ = s.pool.QueryRow(ctx, `SELECT attempts FROM telegram_notify_outbox WHERE id = $1`, it.id).Scan(&attempts)
	status := nextAttemptStatus(attempts)
	if _, err := s.pool.Exec(ctx, `
UPDATE telegram_notify_outbox SET status = $2, attempts = attempts + 1, last_error = $3
WHERE id = $1`, it.id, status, sendErr.Error()); err != nil {
		log.Printf("telegram notify worker: mark error %d: %v", it.id, err)
	}
	log.Printf("telegram notify worker: request %d chat %s: %v", it.requestID, it.chatID, sendErr)
}

// nudgeTelegramNotify будит воркер очереди (неблокирующе).
func (s *Server) nudgeTelegramNotify() {
	if s.notifyWake == nil {
		return
	}
	select {
	case s.notifyWake <- struct{}{}:
	default:
	}
}
