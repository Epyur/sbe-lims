package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/encoding/charmap"
)

// Дизайн: docs/superpowers/specs/2026-08-19-lab-email-ingestion-design.md
// Приём заявок (папка LPITrack) и результатов (Comb/Flam/FlamProp) из почтового
// ящика лаборатории — заменяет часть функциональности десктопной ЛИМС LIMS_LPI.
// Без переноса исторических писем (backfill) — только новые письма с момента включения.

var emailIngestFolders = []string{"LPITrack", "Comb", "Flam", "FlamProp"}

const pendingResultMaxAttempts = 20

// resultMetaFields — поля письма-результата, не относящиеся к values серии
// (маршрутизация/метаданные); всё остальное передаётся в values (при условии,
// что резолвится в объявленный атрибут метода — см. loadAttributeIDSet).
// photo_before/photo_after ЗДЕСЬ не перечислены (до 2026-08-24 были) — уходят
// в values через тот же synonyms-механизм, что и любое другое поле, поэтому
// попадают в протокол как атрибут data_type="photo" (если у метода настроен
// синоним на photo_before_test/photo_after_test и т.п.); applyResultPayload
// ОТДЕЛЬНО, до этого фильтра, всё равно кладёт их в измеренные photo_before/
// photo_after колонки measurement_results — тот путь не меняется.
var resultMetaFields = map[string]bool{
	"type": true, "method": true, "ID": true, "series_num": true,
	"aim_indicator": true, "computing": true,
	// statistics/experiment_params (2026-08-24) — вложенные объекты письма прибора,
	// не плоские значения: цикл ниже их не читает, нужные под-поля отдельно
	// достаёт extractInstrumentFields. "mesure_data" ЗДЕСЬ не перечислен
	// (до 2026-08-24 был) — по прямому запросу пользователя ("почему не перенесены
	// ряды данных времени и температуры... как строить графики") теперь идёт через
	// тот же synonyms-путь, что и любое другое поле: если у метода настроен
	// синоним на атрибут data_type="timeseries" (у ГГ — "smoke_temp_curve"),
	// вложенный объект {time,channels,average_temp,derivative} сохраняется
	// целиком как значение этого атрибута — то же самое, что уже было сделано для
	// photo_before/photo_after. Без настроенного синонима — declaredAttrs всё
	// равно отбросит "mesure_data" как незаведённый атрибут (тот же итог, что
	// раньше, для методов без графика по датчику).
	"statistics": true, "experiment_params": true,
	"source_file": true, "timestamp": true,
}

// canonicalFieldNames — раз опечатки и разнобой имён полей в реальных письмах
// (Comb/Flam/FlamProp используют разные raw-имена для одного и того же понятия,
// см. json_attr.md) закреплены во всех письмах схемы, а не случайны — приводим
// их к единому имени параметра здесь, до попадания в values. Ключ — raw-имя из
// письма, значение — каноническое имя параметра результата.
var canonicalFieldNames = map[string]string{
	"Comb_lenth_1": "comb_length_1", "Comb_lenth_2": "comb_length_2",
	"Comb_lenth_3": "comb_length_3", "Comb_lenth_4": "comb_length_4",
	"sampels_in_date": "samples_in_date", "flam_date_material_in": "samples_in_date",
	"flam_fixation": "mounting_method", "flam_subst": "substrate",
	"flam_inventor": "inventor", "additional_inf": "additional_info",
	"flam_additional_inf": "additional_info", "flam_exp_date": "exp_date",
	"flam_rep_date": "report_date",
}

// knownRawFields — метод-специфичные raw-имена без аналога в других методах:
// уже канонические сами по себе, переименование не нужно. Отдельно от
// canonicalFieldNames, чтобы отличать «оставлено как есть осознанно» от
// «неизвестное поле» в логе applyResultPayload.
var knownRawFields = map[string]bool{
	"burning_drops": true, "combustion_time": true, "temp_of_smog": true,
	"tp1_smog": true, "tp2_smog": true, "tp3_smog": true, "tp4_smog": true,
	"time_of_max_temp": true, "start_time": true, "mass_before": true, "mass_after": true,
	"mounting_method": true, "substrate": true, "inventor": true, "additional_info": true,
	"exp_date": true, "report_date": true,
	"flam_ignition": true, "flam_time": true, "flam_flow_density": true,
	"length_of_distraction": true, "calibration_length": true, "exp_time": true,
	"calibration_flux_csi": true, "calibration_flux_firelab": true,
	"calibration_flux_lpi": true, "calibration_flux_vniipo": true,
	"amb_temp": true, "amb_pres": true, "amb_moist": true, "place": true,
	// photo_before/photo_after (2026-08-24) — уже канонические raw-имена;
	// метод-специфичный целевой атрибут (data_type="photo") настраивается через
	// Synonyms на этом атрибуте (см. loadAttributeSynonymMap) — если для метода
	// синоним не настроен, applyResultPayload отбрасывает эти два поля тем же
	// declaredAttrs-фильтром, что и любое незаведённое поле (не ошибка).
	"photo_before": true, "photo_after": true,
}

type emailIngestConfig struct {
	imapServer   string
	login        string
	password     string
	pollInterval time.Duration
	methodMap    map[string]int64
	labID        int64
}

// loadEmailIngestConfig читает LAB_MAIL_* из env. Возвращает (nil, false), если
// LAB_MAIL_ENABLED не "true" или обязательные переменные не заданы/некорректны —
// в этом случае воркер не стартует, сервис работает как раньше (безопасно по умолчанию).
func loadEmailIngestConfig() (*emailIngestConfig, bool) {
	if strings.ToLower(strings.TrimSpace(os.Getenv("LAB_MAIL_ENABLED"))) != "true" {
		return nil, false
	}
	return loadEmailIngestConfigCore()
}

// loadEmailIngestConfigCore — та же разбор LAB_MAIL_* переменных, что и
// loadEmailIngestConfig, БЕЗ проверки LAB_MAIL_ENABLED (2026-08-24) — нужна
// точечному режиму fetch-mail (mail_fetch_by_id.go): это явный одноразовый
// запуск командой оператора, не постоянный фоновый воркер, поэтому "выключатель"
// воркера (ENABLED) к нему не относится — учётные данные и метод-карта должны
// быть доступны независимо от того, включён ли постоянный опрос почты.
func loadEmailIngestConfigCore() (*emailIngestConfig, bool) {
	cfg := &emailIngestConfig{
		imapServer: strings.TrimSpace(os.Getenv("LAB_MAIL_IMAP_SERVER")),
		login:      strings.TrimSpace(os.Getenv("LAB_MAIL_LOGIN")),
		password:   os.Getenv("LAB_MAIL_PASSWORD"),
	}
	if cfg.imapServer == "" || cfg.login == "" || cfg.password == "" {
		log.Printf("email ingest: LAB_MAIL_IMAP_SERVER/LAB_MAIL_LOGIN/LAB_MAIL_PASSWORD required, disabled")
		return nil, false
	}
	pollSec := 120
	if v := strings.TrimSpace(os.Getenv("LAB_MAIL_POLL_INTERVAL_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pollSec = n
		}
	}
	cfg.pollInterval = time.Duration(pollSec) * time.Second

	cfg.methodMap = map[string]int64{}
	if raw := strings.TrimSpace(os.Getenv("LAB_MAIL_METHOD_MAP")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.methodMap); err != nil {
			log.Printf("email ingest: invalid LAB_MAIL_METHOD_MAP (%v), disabled", err)
			return nil, false
		}
	}
	if len(cfg.methodMap) == 0 {
		log.Printf("email ingest: LAB_MAIL_METHOD_MAP is empty, disabled")
		return nil, false
	}

	// Лаборатория, которой принадлежит ящик — с 2026-08-19 методы могут состоять
	// в нескольких лабах (method_labs), почтовый воркер не может "выбрать" одну
	// интерактивно, поэтому LAB_MAIL_LAB_ID обязателен и фиксирован конфигом.
	labID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("LAB_MAIL_LAB_ID")), 10, 64)
	if err != nil || labID <= 0 {
		log.Printf("email ingest: LAB_MAIL_LAB_ID is required (lab owning the mailbox), disabled")
		return nil, false
	}
	cfg.labID = labID
	return cfg, true
}

// startEmailIngest запускает фоновый воркер опроса почты, если сконфигурирован
// и включён. ctx должен жить весь процесс (не стартовый ctx с таймаутом).
func (s *Server) startEmailIngest(ctx context.Context) {
	cfg, ok := loadEmailIngestConfig()
	if !ok {
		log.Printf("email ingest: disabled")
		return
	}
	log.Printf("email ingest: enabled, polling %s every %s", cfg.imapServer, cfg.pollInterval)
	go func() {
		s.runIngestCycleSafe(ctx, cfg)
		ticker := time.NewTicker(cfg.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runIngestCycleSafe(ctx, cfg)
			}
		}
	}()
}

// runIngestCycleSafe перехватывает панику одного цикла — воркер не должен валить процесс.
func (s *Server) runIngestCycleSafe(ctx context.Context, cfg *emailIngestConfig) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("email ingest: panic recovered: %v", r)
		}
	}()
	if err := s.runIngestCycle(ctx, cfg); err != nil {
		log.Printf("email ingest: cycle: %v", err)
	}
}

func (s *Server) runIngestCycle(ctx context.Context, cfg *emailIngestConfig) error {
	c, err := client.DialTLS(cfg.imapServer+":993", nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Logout()
	if err := c.Login(cfg.login, cfg.password); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	for _, folder := range emailIngestFolders {
		if err := s.processFolder(ctx, c, cfg, folder); err != nil {
			log.Printf("email ingest: folder %s: %v", folder, err)
		}
	}
	s.retryPendingResults(ctx, cfg)
	return nil
}

// processFolder обходит папку в read-only режиме (SELECT readOnly=true, BODY.PEEK[] —
// письма не помечаются прочитанными). Дедуп по Message-ID: тело скачивается только
// для писем, которых ещё нет в email_ingest_processed.
func (s *Server) processFolder(ctx context.Context, c *client.Client, cfg *emailIngestConfig, folder string) error {
	mbox, err := c.Select(folder, true)
	if err != nil {
		return err
	}
	if mbox.Messages == 0 {
		return nil
	}
	allSeqset := new(imap.SeqSet)
	allSeqset.AddRange(1, mbox.Messages)

	envelopes := make(chan *imap.Message, int(mbox.Messages))
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(allSeqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, envelopes)
	}()

	var newSeqNums []uint32
	dedupKeyBySeq := map[uint32]string{}
	for msg := range envelopes {
		dedupKey := ""
		if msg.Envelope != nil {
			dedupKey = strings.TrimSpace(msg.Envelope.MessageId)
		}
		if dedupKey == "" {
			// Message-ID отсутствует (редкий случай) — дедупим по UID, письмо стабильно
			// адресуемо, пока остаётся в папке (см. спеку, «Обработка ошибок и дублей»).
			dedupKey = fmt.Sprintf("uid:%s:%d", folder, msg.Uid)
			log.Printf("email ingest: %s: seq=%d without Message-ID, using %s", folder, msg.SeqNum, dedupKey)
		}
		dedupKeyBySeq[msg.SeqNum] = dedupKey
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM email_ingest_processed WHERE message_id = $1)`,
			dedupKey).Scan(&exists); err != nil {
			log.Printf("email ingest: dedup check: %v", err)
			continue
		}
		if !exists {
			newSeqNums = append(newSeqNums, msg.SeqNum)
		}
	}
	if err := <-done; err != nil {
		return err
	}
	if len(newSeqNums) == 0 {
		return nil
	}

	bodySeqset := new(imap.SeqSet)
	bodySeqset.AddNum(newSeqNums...)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
	bodies := make(chan *imap.Message, len(newSeqNums))
	done2 := make(chan error, 1)
	go func() {
		done2 <- c.Fetch(bodySeqset, items, bodies)
	}()
	for msg := range bodies {
		s.processMessage(ctx, folder, cfg, msg, section, dedupKeyBySeq[msg.SeqNum])
	}
	return <-done2
}

func (s *Server) processMessage(ctx context.Context, folder string, cfg *emailIngestConfig,
	msg *imap.Message, section *imap.BodySectionName, dedupKey string) {
	body := msg.GetBody(section)
	if body == nil {
		log.Printf("email ingest: %s: empty body, seq=%d", folder, msg.SeqNum)
		return
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		log.Printf("email ingest: %s: read body: %v", folder, err)
		return
	}
	text, attachments, err := extractMailTextAndAttachments(raw)
	if err != nil {
		s.recordProcessed(ctx, dedupKey, folder, "error", nil, 0, "extract text: "+err.Error())
		return
	}
	payload, ok := extractJSON(text)
	if !ok {
		s.recordProcessed(ctx, dedupKey, folder, "error", nil, 0, "no JSON object found in body")
		return
	}
	typ, _ := payload["type"].(string)
	switch typ {
	case "application":
		s.applyApplicationEmail(ctx, cfg, dedupKey, folder, payload)
	case "result":
		// Раньше письма с "mesure_data" (сырая телеметрия прибора) целиком
		// пропускались (решение 2026-08-19, "не MVP") — но такое письмо несёт не
		// только сырые массивы по каналам (их и сейчас не заводим), а ЕЩЁ
		// statistics.by_channel/average_max_temp и experiment_params — реальные
		// показатели метода ГГ (tp1-4_smog/temp_of_smog/time_of_max_temp,
		// pre_time_seconds и т.п.), которые до этого фикса просто никогда не
		// заполнялись (2026-08-24, найдено пользователем на живом письме
		// external_id=698). "mesure_data" сам по себе (и statistics/
		// experiment_params как сырые объекты) — в resultMetaFields, не читается
		// напрямую циклом в applyResultPayload; нужные под-поля извлекает
		// extractInstrumentFields.
		s.applyResultEmail(ctx, cfg, dedupKey, folder, payload, attachments)
	default:
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, 0, fmt.Sprintf("unknown type %q", typ))
	}
}

// recordProcessed фиксирует итог обработки письма (дедуп + аудит). ON CONFLICT DO
// UPDATE — если письмо увидено повторно (например, тот же UID-фолбэк), запись
// обновляется, но повторного применения не происходит (dedup-проверка в processFolder
// отсекает такие письма до скачивания тела).
func (s *Server) recordProcessed(ctx context.Context, dedupKey, folder, emailType string,
	payload map[string]any, requestID int64, errText string) {
	if payload == nil {
		payload = map[string]any{}
	}
	rawJSON, err := json.Marshal(payload)
	if err != nil {
		rawJSON = []byte("{}")
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO email_ingest_processed (message_id, folder, email_type, raw_payload, request_id, error)
VALUES ($1, $2, $3, $4::jsonb, $5, $6)
ON CONFLICT (message_id) DO UPDATE SET
	folder = EXCLUDED.folder, email_type = EXCLUDED.email_type, raw_payload = EXCLUDED.raw_payload,
	request_id = EXCLUDED.request_id, error = EXCLUDED.error, processed_at = now()`,
		dedupKey, folder, emailType, string(rawJSON), nullableID(requestID), errText); err != nil {
		log.Printf("email ingest: record processed: %v", err)
	}
}

// ---- applyApplicationEmail (папка LPITrack) ----

func (s *Server) applyApplicationEmail(ctx context.Context, cfg *emailIngestConfig,
	dedupKey, folder string, payload map[string]any) {
	rawID, _ := payload["ID"].(string)
	externalID := strings.TrimPrefix(strings.TrimSpace(rawID), "LPIZAYAVKINAPRO-")
	if externalID == "" {
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, 0, "missing ID field")
		return
	}

	var existingID int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM requests WHERE external_id = $1`, externalID).Scan(&existingID)
	if err == nil {
		s.recordProcessed(ctx, dedupKey, folder, "application", payload, existingID, "duplicate: request already exists")
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("email ingest: check existing external_id: %v", err)
		return
	}

	methodKey, _ := payload["method"].(string)
	methodID, ok := cfg.methodMap[methodKey]
	if !ok {
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, 0,
			fmt.Sprintf("unknown method %q, not in LAB_MAIL_METHOD_MAP", methodKey))
		return
	}

	custMail, _ := payload["cust_mail"].(string)
	productName := strings.TrimSpace(stringField(payload, "product_name"))
	ekn := strings.TrimSpace(stringField(payload, "ekn"))
	// 2026-08-22: письмо не всегда содержит читаемое название продукта, хотя
	// ЕКН указан и реально есть в справочнике — раньше в этом случае заявка/
	// объект создавались с заглушкой "Без названия" навсегда (справочник
	// никогда не запрашивался). Пробуем разрешить по ЕКН, прежде чем ставить
	// заглушку.
	if productName == "" && ekn != "" {
		if name := lookupEknProductName(ctx, ekn); name != "" {
			productName = name
		}
	}
	if productName == "" {
		productName = missingNamePlaceholder
	}
	thickness := stringField(payload, "thickness")
	materialType := stringField(payload, "material_type")
	aimIndicator := stringField(payload, "aim_indicator")
	description := stringField(payload, "description")
	if strings.TrimSpace(description) == "" {
		description = aimIndicator
	}

	characteristics := map[string]any{
		"ekn":              ekn,
		"thickness_mm":     thickness,
		"material_type":    materialType,
		"target_indicator": aimIndicator,
	}
	if ekn != "" {
		characteristics["sample_type"] = "series"
		if n, ok := parseIntLoose(payload["batch_number"]); ok {
			characteristics["batch_number"] = n
		}
	} else {
		characteristics["sample_type"] = "experimental"
	}
	charsJSON, err := json.Marshal(characteristics)
	if err != nil {
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, 0, "marshal characteristics: "+err.Error())
		return
	}

	priority := mapEmailPriority(stringField(payload, "priority"))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("email ingest: begin tx: %v", err)
		return
	}
	defer tx.Rollback(ctx)

	var objectID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO objects (name, description, characteristics) VALUES ($1, '', $2::jsonb)
RETURNING id`, productName, string(charsJSON)).Scan(&objectID); err != nil {
		log.Printf("email ingest: create object: %v", err)
		return
	}

	var effProjectID int64
	pi := projectInfo{code: "0"}
	if ekn != "" {
		effProjectID, err = s.ensureEknProject(ctx, tx, ekn)
		if err != nil {
			log.Printf("email ingest: ensureEknProject: %v", err)
			return
		}
		if effProjectID > 0 {
			pi.code = ekn
		}
	}

	// LAB_MAIL_LAB_ID — лаба, которой принадлежит ящик; метод обязан быть привязан
	// к ней в method_labs (иначе конфиг противоречив — не гадаем, отказываем).
	mlr, err := loadMethodLabRow(ctx, tx, methodID, cfg.labID)
	if err != nil {
		log.Printf("email ingest: method %d not linked to LAB_MAIL_LAB_ID=%d: %v", methodID, cfg.labID, err)
		return
	}

	year := time.Now().UTC().Year()
	seq, err := s.nextSeq(ctx, year)
	if err != nil {
		log.Printf("email ingest: nextSeq: %v", err)
		return
	}
	customer, lab := buildNumbers(seq, year, pi.code, mlr.labCode, mlr.methodCode)

	var requestID int64
	err = tx.QueryRow(ctx, `
INSERT INTO requests (number_seq, number_year, title, description, object_id, project_id,
	owner_email, status, priority, method_id, lab_id, customer_number, lab_number, external_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'new', $8, $9, $10, $11, $12, $13)
RETURNING id`,
		seq, year, productName, description, objectID, nullableID(effProjectID),
		custMail, priority, methodID, cfg.labID, customer, lab, externalID).Scan(&requestID)
	if err != nil {
		log.Printf("email ingest: create request: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("email ingest: commit: %v", err)
		return
	}

	log.Printf("email ingest: created request %d (external_id=%s, method=%s) from %s",
		requestID, externalID, methodKey, folder)
	s.recordProcessed(ctx, dedupKey, folder, "application", payload, requestID, "")
}

// mapEmailPriority переводит русский текст трекера в enum requests.priority.
// Неизвестное значение → normal + предупреждение (не блокирует создание заявки).
func mapEmailPriority(raw string) string {
	switch strings.TrimSpace(raw) {
	case "", "Обычный":
		return "normal"
	case "Критичный":
		return "critical"
	case "Блокер":
		return "blocker"
	default:
		log.Printf("email ingest: unknown priority %q, defaulting to normal", raw)
		return "normal"
	}
}

// ---- applyResultEmail (папки Comb/Flam/FlamProp) ----

func (s *Server) applyResultEmail(ctx context.Context, cfg *emailIngestConfig,
	dedupKey, folder string, payload map[string]any, attachments []mailAttachment) {
	externalID := idToString(payload["ID"])
	if externalID == "" {
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, 0, "missing ID field")
		return
	}
	methodKey, _ := payload["method"].(string)
	methodID, ok := cfg.methodMap[methodKey]
	if !ok {
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, 0,
			fmt.Sprintf("unknown method %q, not in LAB_MAIL_METHOD_MAP", methodKey))
		return
	}

	var requestID int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM requests WHERE external_id = $1`, externalID).Scan(&requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		// заявка ещё не пришла (или не придёт) — буферизуем, retryPendingResults
		// подхватит её в одном из следующих циклов (см. спеку).
		s.enqueuePendingResult(ctx, externalID, methodID, payload, attachments)
		s.recordProcessed(ctx, dedupKey, folder, "result", payload, 0, "pending: request not found yet")
		return
	}
	if err != nil {
		log.Printf("email ingest: lookup request by external_id=%s: %v", externalID, err)
		return
	}

	if err := s.applyResultPayload(ctx, requestID, methodID, payload, cfg, attachments); err != nil {
		s.recordProcessed(ctx, dedupKey, folder, "error", payload, requestID, "apply result: "+err.Error())
		return
	}
	log.Printf("email ingest: applied result for request %d (external_id=%s) from %s", requestID, externalID, folder)
	s.recordProcessed(ctx, dedupKey, folder, "result", payload, requestID, "")
}

// applyResultPayload передаёт поля письма (минус метаданные маршрутизации) как values
// в saveResultSeries, приводя raw-имена полей формы к каноническим именам параметров
// (canonicalFieldNames) — раз в реальных письмах разные методы называют одно и то же
// понятие по-разному и с опечатками (см. json_attr.md), формулы/классификация метода
// должны ссылаться на одно устойчивое имя параметра независимо от метода-источника.
// resolveResultKey определяет каноническое имя параметра результата для одного
// raw-ключа письма: synonyms конкретного атрибута метода (осознанная настройка) >
// глобальный canonicalFieldNames (закреплённые переименования) > knownRawFields
// (уже каноническое само по себе, оставляем как есть) > неизвестное (raw как есть,
// known=false — вызывающий код логирует это отдельно).
func resolveResultKey(raw string, synonyms map[string]string) (key string, known bool) {
	if id, ok := synonyms[raw]; ok {
		return id, true
	}
	if canon, ok := canonicalFieldNames[raw]; ok {
		return canon, true
	}
	return raw, knownRawFields[raw]
}

// systemRequestFields — канонические имена результата (после resolveResultKey),
// относящиеся к ЗАЯВКЕ целиком, а не к конкретному методу: испытатель/даты/условия
// среды одинаковы для ЛЮБОГО метода (2026-08-23, по решению пользователя — "инвентор
// он системный, как и другие перечисленные... заведи эти атрибуты как универсальные,
// так же как например «Наименование материала»"; подтверждено кросс-проверкой по
// Comb/Flam/FlamProp в json_attr.md — amb_temp/amb_pres/amb_moist/exp_date несёт
// КАЖДОЕ письмо-результат независимо от метода). Не попадают в values (т.е. не
// MethodAttribute) — пишутся напрямую в requests.* через applyRequestSystemFields.
// Тот же каталог продублирован (согласованно, вручную) в трёх местах: здесь,
// resolveSystemPlaceholder (protocol.go) и SYSTEM_PLACEHOLDERS (sbe-lims
// block-editor.ts) — см. sbe-lims/AGENTS.md, "Системные атрибуты".
var systemRequestFields = map[string]bool{
	"inventor": true, "report_date": true, "samples_in_date": true, "exp_date": true,
	"amb_temp": true, "amb_pres": true, "amb_moist": true,
}

// extractInstrumentFields достаёт из письма прибора (payload содержит "mesure_data")
// реальные показатели метода ГГ — БЕЗ самих сырых массивов по каналам
// (mesure_data.time/channels/average_temp/derivative — решение пользователя
// 2026-08-19, "не MVP", по-прежнему не заводим). Найдено пользователем на живом письме
// (external_id=698, 2026-08-24): "statistics"/"experiment_params" несут реальные
// значения для tp1-4_smog/temp_of_smog/time_of_max_temp и pre_time_seconds и т.п. — до
// этого фикса терялись целиком вместе с массивами, т.к. письмо просто пропускалось.
func extractInstrumentFields(payload map[string]any) map[string]any {
	out := map[string]any{}
	if stats, ok := payload["statistics"].(map[string]any); ok {
		if byChannel, ok := stats["by_channel"].(map[string]any); ok {
			var maxTimes []float64
			for _, chNum := range []string{"1", "2", "3", "4"} {
				ch, ok := byChannel[chNum].(map[string]any)
				if !ok {
					continue
				}
				if maxTemp, ok := ch["max_temp"]; ok {
					out["tp"+chNum+"_smog"] = maxTemp
				}
				if maxTime, ok := toFloatOK(ch["max_time"]); ok {
					maxTimes = append(maxTimes, maxTime)
				}
			}
			// time_of_max_temp — единый атрибут метода, а прибор отдаёт время пика
			// ПОКАНАЛЬНО (обычно близкие, но разные значения) — среднее по каналам,
			// тот же принцип, что уже применён ниже к average_max_temp -> temp_of_smog
			// (решение пользователя 2026-08-24).
			if len(maxTimes) > 0 {
				sum := 0.0
				for _, v := range maxTimes {
					sum += v
				}
				out["time_of_max_temp"] = sum / float64(len(maxTimes))
			}
		}
		if avgMaxTemp, ok := stats["average_max_temp"]; ok {
			out["temp_of_smog"] = avgMaxTemp
		}
	}
	if params, ok := payload["experiment_params"].(map[string]any); ok {
		for _, key := range []string{"pre_time_seconds", "main_time_seconds", "post_time_seconds", "num_channels", "poll_interval_seconds"} {
			if v, ok := params[key]; ok && v != nil {
				out[key] = v
			}
		}
	}
	return out
}

// setIfMeaningful пишет values[key]=v, НО не даёт пустому значению из ОДНОГО письма
// затереть уже сохранённое непустое значение из ДРУГОГО письма той же серии
// (2026-08-24, найдено на живых данных external_id=698): письмо-форма несёт
// "tp1_smog":"" как заглушку "это поле заполнит прибор" — при повторной обработке
// формы ПОСЛЕ письма прибора (порядок обработки писем одной серии не гарантирован) эта
// пустая строка иначе затирала бы уже сохранённое реальное число. Тот же принцип, что
// уже применялся при восстановлении ЕКН этой сессии — "первое НЕПУСТОЕ значение
// выигрывает", не "последнее записанное".
func setIfMeaningful(values map[string]any, key string, v any) {
	if isBlankValue(v) {
		if existing, ok := values[key]; ok && !isBlankValue(existing) {
			return
		}
	}
	values[key] = v
}

func isBlankValue(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

func (s *Server) applyResultPayload(ctx context.Context, requestID, methodID int64, payload map[string]any,
	cfg *emailIngestConfig, attachments []mailAttachment) error {
	// synonyms настроены в конфигураторе метода (per-атрибут, см. MethodAttribute.
	// Synonyms) — проверяются раньше глобальных canonicalFieldNames/knownRawFields,
	// поскольку это осознанная настройка конкретного атрибута конкретного метода.
	synonyms, err := s.loadAttributeSynonymMap(ctx, methodID)
	if err != nil {
		synonyms = nil // атрибуты метода могли быть ещё не настроены — не критично
	}
	// declaredAttrs — только то, что реально заведено в input_parameters метода
	// (2026-08-24): письмо может нести поля, которые метод сознательно не
	// заводит (напр. calibration_* у РП — решение пользователя "все что связано
	// с калибровкой не заводи... вводим только прямые измерения и расчеты") —
	// такие поля не должны оседать в values, даже если resolveResultKey их
	// узнаёт как canonicalFieldNames/knownRawFields.
	declaredAttrs, err := s.loadAttributeIDSet(ctx, methodID)
	if err != nil {
		declaredAttrs = nil // не критично — см. ниже, при nil фильтр не применяется
	}
	// photoURLs — фото-вложения письма, сопоставленные с photo_before/photo_after и
	// загруженные в наше объектное хранилище (2026-08-24) — заменяют яндекс-ссылку
	// из payload (недоступна анонимно) везде, где она использовалась бы ниже.
	photoURLs := s.resolvePhotoAttachments(ctx, cfg, requestID, payload, attachments)

	// explicitSeriesNum — письмо САМО указывает свой series_num (несёт его КАЖДОЕ
	// письмо-результат, Comb/Flam/FlamProp — см. TestCanonicalFieldNamesAgainstRealKeys).
	// 2026-08-24: письмо-форма и письмо прибора для ОДНОГО измерения несут ОДИН и тот
	// же series_num (external_id=698 — оба "1") — раньше это игнорировалось (всегда
	// auto-increment), из-за чего два письма одной серии создавали ДВЕ РАЗНЫЕ строки
	// вместо слияния. Теперь явный series_num — целевая строка; если она уже
	// существует, её values подгружаются и НОВЫЕ поля добавляются поверх (не
	// затирают то, что уже было от другого письма той же серии).
	explicitSeriesNum, _ := parseIntLoose(payload["series_num"])
	values := map[string]any{}
	if explicitSeriesNum > 0 {
		if existing, err := s.loadSeriesValuesAt(ctx, requestID, methodID, explicitSeriesNum); err == nil {
			for k, v := range existing {
				values[k] = v
			}
		}
	}
	for k, v := range extractInstrumentFields(payload) {
		if declaredAttrs != nil && !declaredAttrs[k] {
			continue // атрибут не заведён у этого метода — не сохраняем
		}
		setIfMeaningful(values, k, v)
	}
	// inventor — единственное системное поле, которое остаётся per-request
	// (резолвится в requests.inventor_id по имени, см. resolveInventorFromEmail);
	// остальные 6 (2026-08-28, WP3b) идут прямо в values ЭТОЙ серии — испытания
	// одной заявки могут выполняться в разные дни с разными условиями среды, см.
	// docs/superpowers/specs/2026-08-28-sbe-lims-system-fields-per-series-design.md.
	inventorField := map[string]any{}
	for k, v := range payload {
		if resultMetaFields[k] {
			continue
		}
		if k == "photo_before" || k == "photo_after" {
			url, ok := photoURLs[k]
			if !ok {
				continue // вложение не сопоставилось/не загрузилось — не пишем мёртвую ссылку
			}
			v = url
		}
		key, known := resolveResultKey(k, synonyms)
		if !known {
			log.Printf("email ingest: request %d: неизвестное поле результата %q, передано как есть", requestID, k)
		}
		if key == "inventor" {
			inventorField["inventor"] = v
			continue
		}
		if systemRequestFields[key] {
			setIfMeaningful(values, key, v)
			continue
		}
		if declaredAttrs != nil && !declaredAttrs[key] {
			continue // не заведено в input_parameters этого метода — не сохраняем
		}
		setIfMeaningful(values, key, v)
	}
	if len(inventorField) > 0 {
		if err := s.resolveInventorFromEmail(ctx, requestID, inventorField); err != nil {
			log.Printf("email ingest: request %d: не удалось записать испытателя: %v", requestID, err)
		}
	}
	photoBefore := photoURLs["photo_before"]
	photoAfter := photoURLs["photo_after"]
	// equipment_id=0 — письмо не знает, каким оборудованием сделано измерение; если у
	// метода несколько единиц "Основного" оборудования, калибровочная кривая (WP1) для
	// таких результатов просто не подставится (см. calibration_curve.go
	// injectCalibrationCurves — best-effort, не блокирует сохранение).
	// "email-ingest" (2026-08-29, WP4) — нет реального пользователя, письмо
	// разбирается автоматическим процессом; отличимо от настоящего email в UI.
	_, _, err = s.saveResultSeries(ctx, requestID, methodID, 0, 0, explicitSeriesNum, values, photoBefore, photoAfter, "email-ingest")
	return err
}

// resolveInventorFromEmail резолвит "inventor" (имя из письма-результата) в
// requests.inventor_id точным совпадением по inventors.name; если не найден —
// предупреждение в лог, inventor_id не устанавливается (не создаём испытателя
// автоматически — справочник ведёт лаборатория через /inventors). До 2026-08-28
// эта функция (тогда applyRequestSystemFields) писала СЮДА ЖЕ ещё 6 полей
// (report_date и т.п.) — теперь они идут в values серии (WP3b, см. вызывающий
// код) и остаются единственным системным полем per-request, не per-series
// (инвентор не меняется день ото дня так, как условия среды).
func (s *Server) resolveInventorFromEmail(ctx context.Context, requestID int64, fields map[string]any) error {
	v, ok := fields["inventor"]
	if !ok {
		return nil
	}
	name := strings.TrimSpace(fmt.Sprint(v))
	if name == "" {
		return nil
	}
	var inventorID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM inventors WHERE name = $1`, name).Scan(&inventorID); err != nil {
		log.Printf("email ingest: request %d: испытатель %q не найден в справочнике, inventor_id не установлен", requestID, name)
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE requests SET inventor_id = $2 WHERE id = $1`, requestID, inventorID)
	return err
}

func (s *Server) enqueuePendingResult(ctx context.Context, externalID string, methodID int64,
	payload map[string]any, attachments []mailAttachment) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("email ingest: marshal pending payload: %v", err)
		return
	}
	// attachments сериализуются вместе с payload (2026-08-24) — иначе фото письма-
	// результата, пришедшего раньше самой заявки, терялись бы к моменту retryPendingResults
	// (Data []byte кодируется encoding/json как base64 автоматически).
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		log.Printf("email ingest: marshal pending attachments: %v", err)
		attachmentsJSON = []byte("[]")
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO email_ingest_pending_results (external_id, method_id, payload, attachments)
VALUES ($1, $2, $3::jsonb, $4::jsonb)`, externalID, methodID, string(payloadJSON), string(attachmentsJSON)); err != nil {
		log.Printf("email ingest: enqueue pending result: %v", err)
	}
}

// retryPendingResults пробует привязать результаты, буферизованные до появления заявки.
// attempts растёт при каждой неудаче; после pendingResultMaxAttempts строка не удаляется
// (данные не теряются), но предупреждение логируется только на переходе через порог —
// не на каждом цикле.
func (s *Server) retryPendingResults(ctx context.Context, cfg *emailIngestConfig) {
	rows, err := s.pool.Query(ctx, `
SELECT id, external_id, method_id, payload, attachments, attempts FROM email_ingest_pending_results ORDER BY id`)
	if err != nil {
		log.Printf("email ingest: load pending: %v", err)
		return
	}
	type pendingRow struct {
		id          int64
		externalID  string
		methodID    int64
		payload     map[string]any
		attachments []mailAttachment
		attempts    int
	}
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		var raw, attRaw []byte
		if err := rows.Scan(&p.id, &p.externalID, &p.methodID, &raw, &attRaw, &p.attempts); err != nil {
			log.Printf("email ingest: pending scan: %v", err)
			continue
		}
		p.payload = map[string]any{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &p.payload)
		}
		if len(attRaw) > 0 {
			_ = json.Unmarshal(attRaw, &p.attachments)
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("email ingest: pending rows: %v", err)
		return
	}

	for _, p := range pending {
		var requestID int64
		err := s.pool.QueryRow(ctx, `SELECT id FROM requests WHERE external_id = $1`, p.externalID).Scan(&requestID)
		if errors.Is(err, pgx.ErrNoRows) {
			attempts := p.attempts + 1
			if attempts == pendingResultMaxAttempts+1 {
				log.Printf("email ingest: pending result external_id=%s exceeded %d attempts, still waiting",
					p.externalID, pendingResultMaxAttempts)
			}
			if _, err := s.pool.Exec(ctx,
				`UPDATE email_ingest_pending_results SET attempts = $2 WHERE id = $1`, p.id, attempts); err != nil {
				log.Printf("email ingest: update pending attempts: %v", err)
			}
			continue
		}
		if err != nil {
			log.Printf("email ingest: pending lookup external_id=%s: %v", p.externalID, err)
			continue
		}
		if err := s.applyResultPayload(ctx, requestID, p.methodID, p.payload, cfg, p.attachments); err != nil {
			log.Printf("email ingest: apply pending result external_id=%s: %v", p.externalID, err)
			continue
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM email_ingest_pending_results WHERE id = $1`, p.id); err != nil {
			log.Printf("email ingest: delete pending: %v", err)
			continue
		}
		log.Printf("email ingest: applied pending result external_id=%s -> request %d", p.externalID, requestID)
	}
}

// ---- Разбор письма: MIME + кодировки + извлечение JSON ----

// extractMailText разбирает сырое RFC822-сообщение (BODY.PEEK[]) и возвращает
// человекочитаемый текст письма (text/plain приоритетнее multipart-альтернативы,
// иначе text/html с очисткой тегов), с явным декодированием заявленной кодировки
// (письма трекера/форм бывают windows-1251, не только utf-8).
func extractMailText(raw []byte) (string, error) {
	text, _, err := extractMailCore(raw)
	return text, err
}

// mailAttachment — вложение письма, извлечённое при разборе MIME (2026-08-24): реальное
// фото приходит как вложение письма, а не по яндекс-ссылке в JSON (photo_before/
// photo_after) — та ссылка недоступна анонимно (302 -> auth.cloud.yandex.ru/oauth,
// подтверждено прямым HTTP-тестом), см. AGENTS.md "фото в протоколе".
type mailAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// extractMailTextAndAttachments — тот же разбор, что extractMailText, плюс вложения
// письма — нужно applyResultEmail/applyResultPayload для photo_before/photo_after.
// extractMailText остаётся отдельной тонкой обёрткой для мест, где вложения не нужны
// (peekPayload).
func extractMailTextAndAttachments(raw []byte) (string, []mailAttachment, error) {
	return extractMailCore(raw)
}

func extractMailCore(raw []byte) (string, []mailAttachment, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		mediaType, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		return extractMultipartCore(msg.Body, params["boundary"])
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return "", nil, err
	}
	text := decodePart(body, msg.Header.Get("Content-Transfer-Encoding"), params["charset"])
	if mediaType == "text/html" {
		text = stripHTML(text)
	}
	return text, nil, nil
}

func extractMultipartCore(r io.Reader, boundary string) (string, []mailAttachment, error) {
	if boundary == "" {
		return "", nil, errors.New("multipart without boundary")
	}
	mr := multipart.NewReader(r, boundary)
	var plain, htmlText string
	var attachments []mailAttachment
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		mt, params, perr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if perr != nil {
			mt = "text/plain"
			params = map[string]string{}
		}
		if strings.HasPrefix(mt, "multipart/") {
			if nestedText, nestedAtt, err := extractMultipartCore(part, params["boundary"]); err == nil {
				if plain == "" {
					plain = nestedText
				}
				attachments = append(attachments, nestedAtt...)
			}
			continue
		}
		data, err := io.ReadAll(part)
		if err != nil {
			continue
		}
		switch mt {
		case "text/plain":
			if plain == "" {
				plain = decodePart(data, part.Header.Get("Content-Transfer-Encoding"), params["charset"])
			}
		case "text/html":
			if htmlText == "" {
				htmlText = decodePart(data, part.Header.Get("Content-Transfer-Encoding"), params["charset"])
			}
		default:
			// Вложение (фото и т.п.) — раньше тихо отбрасывалось здесь (2026-08-24: реальный
			// баг, из-за которого photo_before/photo_after никогда не доходили до протокола
			// как валидные картинки — см. AGENTS.md). charset тут не при чём (не текст) —
			// снимаем только Content-Transfer-Encoding.
			if filename := part.FileName(); filename != "" {
				attachments = append(attachments, mailAttachment{
					Filename:    filename,
					ContentType: mt,
					Data:        decodeTransferEncoding(data, part.Header.Get("Content-Transfer-Encoding")),
				})
			}
		}
	}
	text := plain
	var textErr error
	if text == "" {
		if htmlText != "" {
			text = stripHTML(htmlText)
		} else {
			textErr = errors.New("no text part found")
		}
	}
	return text, attachments, textErr
}

// decodeTransferEncoding снимает Content-Transfer-Encoding (base64/quoted-printable) без
// интерпретации charset — общий первый шаг decodePart, нужен отдельно для бинарных вложений
// (charset-декодирование недопустимо для не-текстовых байт, 2026-08-24).
func decodeTransferEncoding(data []byte, transferEncoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "base64":
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, string(data))
		if decoded, err := base64.StdEncoding.DecodeString(clean); err == nil {
			return decoded
		}
	case "quoted-printable":
		if decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data))); err == nil {
			return decoded
		}
	}
	return data
}

// decodePart снимает Content-Transfer-Encoding и приводит к UTF-8 по заявленной
// кодировке (по умолчанию — как есть, считается utf-8).
func decodePart(data []byte, transferEncoding, charset string) string {
	data = decodeTransferEncoding(data, transferEncoding)
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "windows-1251", "cp1251", "cp-1251":
		if out, err := charmap.Windows1251.NewDecoder().Bytes(data); err == nil {
			return string(out)
		}
	case "koi8-r":
		if out, err := charmap.KOI8R.NewDecoder().Bytes(data); err == nil {
			return string(out)
		}
	}
	return string(data)
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	return html.UnescapeString(htmlTagRe.ReplaceAllString(s, " "))
}

// extractJSON вырезает JSON-объект из текста письма: от первой "{" до последней "}"
// (тело письма может содержать текст/подпись вокруг JSON-полезной нагрузки).
func extractJSON(text string) (map[string]any, bool) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end == -1 || end < start {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text[start:end+1]), &payload); err != nil {
		return nil, false
	}
	return payload, true
}

// ---- Сопоставление фото-вложений (2026-08-24) ----

// extractYandexFormsFilename достаёт имя файла из ссылки вида
// "https://forms.yandex.ru/cloud/files?path=%2F<id>%2F<name>.jpg" (значение payload
// photo_before/photo_after) — query-параметр "path" URL-декодируется автоматически
// url.Query(), берётся последний "/"-сегмент. Пустая строка/не URL/нет path= -> false.
func extractYandexFormsFilename(rawURL string) (string, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	p := u.Query().Get("path")
	if p == "" {
		return "", false
	}
	name := path.Base(p)
	if name == "" || name == "." || name == "/" {
		return "", false
	}
	return name, true
}

// matchAttachmentByFilename ищет РОВНО ОДНО вложение, чьё имя файла является ХВОСТОМ
// urlFilename — не точным совпадением: подтверждено на реальном письме (external_id=698,
// см. AGENTS.md "фото в протоколе"), что Яндекс.Формы при заливке в cloud-хранилище
// добавляет свой хеш-префикс к оригинальному имени файла ("<24-символьный hex>_
// <оригинальное-имя-вложения>"), а само письмо несёт вложение под оригинальным именем
// без префикса. 0 или больше одного совпадения — false, не гадаем (та же дисциплина, что
// resolveResultKey — не подставляем случайное значение при неопределённости).
func matchAttachmentByFilename(urlFilename string, attachments []mailAttachment) (mailAttachment, bool) {
	var found mailAttachment
	count := 0
	for _, a := range attachments {
		if strings.HasSuffix(urlFilename, a.Filename) {
			found = a
			count++
		}
	}
	if count != 1 {
		return mailAttachment{}, false
	}
	return found, true
}

// matchPhotoFields сопоставляет photo_before/photo_after payload-а с вложениями письма —
// по имени файла (см. matchAttachmentByFilename — хвост, не точное совпадение;
// подтверждено на реальном письме external_id=698, 2026-08-24). Поле отсутствует в
// результате, если оно пустое в payload, ссылку не удалось разобрать, или совпадение
// неоднозначно.
func matchPhotoFields(payload map[string]any, attachments []mailAttachment) map[string]mailAttachment {
	out := map[string]mailAttachment{}
	for _, field := range []string{"photo_before", "photo_after"} {
		filename, ok := extractYandexFormsFilename(stringField(payload, field))
		if !ok {
			continue
		}
		att, ok := matchAttachmentByFilename(filename, attachments)
		if !ok {
			log.Printf("email ingest: %s: имя файла %q из ссылки не совпало ровно с одним вложением письма", field, filename)
			continue
		}
		out[field] = att
	}
	return out
}

// resolvePhotoAttachments сопоставляет photo_before/photo_after с вложениями письма
// (matchPhotoFields) и грузит совпавшие в объектное хранилище (uploadFileBytes, files.go) —
// яндекс-ссылка недоступна анонимно, см. mailAttachment. requestID может быть 0 (заявка
// ещё не найдена, письмо буферизуется в email_ingest_pending_results) — тогда файл всё
// равно грузится (uploadFileBytes просто не привязывает его к заявке в таблице files).
func (s *Server) resolvePhotoAttachments(ctx context.Context, cfg *emailIngestConfig,
	requestID int64, payload map[string]any, attachments []mailAttachment) map[string]string {
	matched := matchPhotoFields(payload, attachments)
	out := map[string]string{}
	for field, att := range matched {
		url, err := s.uploadFileBytes(ctx, requestID, att.Filename, att.Data, cfg.login)
		if err != nil {
			log.Printf("email ingest: upload photo attachment %q (%s): %v", att.Filename, field, err)
			continue
		}
		out[field] = url
	}
	return out
}

// ---- Мелкие хелперы разбора payload ----

func stringField(payload map[string]any, key string) string {
	v, _ := payload[key].(string)
	return v
}

// idToString приводит поле "ID" письма-результата (число или строка в JSON) к строке
// для сравнения с requests.external_id.
func idToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}

// parseIntLoose разбирает batch_number, который в JSON может прийти и числом, и строкой.
func parseIntLoose(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return 0, false
		}
		n, err := strconv.Atoi(t)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}
