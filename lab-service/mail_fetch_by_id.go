package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// runFetchMailByID — точечный режим «взять письма по ID» (2026-08-24, по прямому
// запросу пользователя, тестовый запуск мониторинга почты без разбора всего
// исторического архива): CLI-команда
//
//	./lab-service fetch-mail -folder=Comb -id=698
//
// подключается к живому IMAP-ящику и применяет ТОЛЬКО письма, чей встроенный
// JSON-идентификатор ("ID" — тот же, что requests.external_id) точно равен
// -id — в отличие от постоянного воркера (startEmailIngest/runIngestCycle),
// который при включении сразу обрабатывает ВСЮ папку целиком. Письма, не
// совпавшие по ID, не трогаются вовсе — не помечаются как обработанные,
// остаются нетронутыми для будущего полного запуска воркера. Применение
// совпавшего письма идёт через ТОТ ЖЕ processMessage, что и постоянный
// воркер — идентичное поведение (дедуп по Message-ID, канонизация полей,
// пропуск сырой телеметрии прибора и т.п.).
func runFetchMailByID(ctx context.Context, s *Server, args []string) {
	fs := flag.NewFlagSet("fetch-mail", flag.ExitOnError)
	folder := fs.String("folder", "", "IMAP-папка (Comb, Flam, FlamProp, LPITrack)")
	targetID := fs.String("id", "", "значение JSON-поля \"ID\" письма (= requests.external_id)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("fetch-mail: %v", err)
	}
	*folder = strings.TrimSpace(*folder)
	*targetID = strings.TrimSpace(*targetID)
	if *folder == "" || *targetID == "" {
		log.Fatalf("fetch-mail: -folder и -id обязательны")
	}

	cfg, ok := loadEmailIngestConfigCore()
	if !ok {
		log.Fatalf("fetch-mail: LAB_MAIL_IMAP_SERVER/LOGIN/PASSWORD/METHOD_MAP/LAB_ID не заданы или некорректны")
	}

	c, err := client.DialTLS(cfg.imapServer+":993", nil)
	if err != nil {
		log.Fatalf("fetch-mail: dial: %v", err)
	}
	defer c.Logout()
	if err := c.Login(cfg.login, cfg.password); err != nil {
		log.Fatalf("fetch-mail: login: %v", err)
	}

	n, err := s.fetchAndApplyByID(ctx, c, cfg, *folder, *targetID)
	if err != nil {
		log.Fatalf("fetch-mail: %v", err)
	}
	log.Printf("fetch-mail: папка %s, id=%s: применено писем: %d", *folder, *targetID, n)
}

// fetchAndApplyByID — сначала СЕРВЕР-СТОРОННИЙ IMAP SEARCH BODY по подстроке
// targetID (без скачивания единого байта тела — на папке в 500+ писем
// скачивание ВСЕХ тел в один Fetch обрывает соединение Яндекса, см.
// AGENTS.md/2026-08-24: "imap: connection closed" на 175/524), затем строгая
// проверка встроенного JSON "ID" (peekPayload, без побочных эффектов) только
// по кандидатам от SEARCH, и наконец ОТДЕЛЬНЫМ Fetch — свежий Literal-ридер
// только для точно совпавших писем — применяет их через processMessage (тот
// же путь, что у постоянного воркера). Раздельные проходы для проверки и
// применения нужны, т.к. imap.Literal — это io.Reader, читаемый один раз;
// тело, прочитанное в peekPayload для проверки ID, уже исчерпано и не годится
// для processMessage напрямую.
func (s *Server) fetchAndApplyByID(ctx context.Context, c *client.Client, cfg *emailIngestConfig, folder, targetID string) (int, error) {
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}

	matchedSeqNums, err := searchMatchingSeqNums(c, folder, targetID, section)
	if err != nil {
		return 0, err
	}
	if len(matchedSeqNums) == 0 {
		return 0, nil
	}

	matchSeqset := new(imap.SeqSet)
	matchSeqset.AddNum(matchedSeqNums...)
	matched := make(chan *imap.Message, len(matchedSeqNums))
	done2 := make(chan error, 1)
	go func() {
		done2 <- c.Fetch(matchSeqset, items, matched)
	}()
	applied := 0
	for msg := range matched {
		dedupKey := ""
		if msg.Envelope != nil {
			dedupKey = strings.TrimSpace(msg.Envelope.MessageId)
		}
		if dedupKey == "" {
			dedupKey = fmt.Sprintf("uid:%s:%d", folder, msg.Uid)
		}
		s.processMessage(ctx, folder, cfg, msg, section, dedupKey)
		applied++
	}
	if err := <-done2; err != nil {
		return applied, fmt.Errorf("fetch matched: %w", err)
	}
	return applied, nil
}

// searchMatchingSeqNums — сначала СЕРВЕР-СТОРОННИЙ IMAP SEARCH BODY по подстроке
// targetID (без скачивания единого байта тела — на папке в 500+ писем скачивание ВСЕХ
// тел в один Fetch обрывает соединение Яндекса, см. AGENTS.md/2026-08-24: "imap:
// connection closed" на 175/524), затем строгая проверка встроенного JSON "ID"
// (peekPayload, без побочных эффектов) только по кандидатам от SEARCH. Общий поисковый
// шаг для fetchAndApplyByID и runFetchMailPhotos — сами вложения/применение письма у них
// разные, поиск письма по ID — один и тот же.
func searchMatchingSeqNums(c *client.Client, folder, targetID string, section *imap.BodySectionName) ([]uint32, error) {
	mbox, err := c.Select(folder, true)
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", folder, err)
	}
	log.Printf("fetch-mail: select %s: %d писем в папке", folder, mbox.Messages)
	if mbox.Messages == 0 {
		return nil, nil
	}

	candidates, err := c.Search(&imap.SearchCriteria{Body: []string{targetID}})
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", folder, err)
	}
	log.Printf("fetch-mail: search по подстроке %q: кандидатов %d", targetID, len(candidates))
	if len(candidates) == 0 {
		return nil, nil
	}

	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
	candSeqset := new(imap.SeqSet)
	candSeqset.AddNum(candidates...)
	messages := make(chan *imap.Message, len(candidates))
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(candSeqset, items, messages)
	}()

	var matchedSeqNums []uint32
	seen := 0
	for msg := range messages {
		seen++
		payload, ok := peekPayload(msg, section)
		if !ok {
			continue
		}
		if strings.TrimSpace(idToString(payload["ID"])) == targetID {
			matchedSeqNums = append(matchedSeqNums, msg.SeqNum)
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch candidates (seen %d/%d): %w", seen, len(candidates), err)
	}
	log.Printf("fetch-mail: проверено кандидатов %d, точное совпадение ID=%s: %d", seen, targetID, len(matchedSeqNums))
	return matchedSeqNums, nil
}

// runFetchMailPhotos — одноразовый режим восстановления фото УЖЕ ПРИНЯТОЙ заявки
// (2026-08-24): CLI-команда
//
//	./lab-service fetch-mail-photos -folder=Comb -id=698 -request=1378
//
// находит письмо тем же способом, что fetch-mail (searchMatchingSeqNums), извлекает
// вложения, сопоставляет photo_before/photo_after и грузит совпавшие в S3
// (resolvePhotoAttachments — тот же путь, что применяется для НОВЫХ писем) — и печатает
// получившиеся URL. НЕ вызывает processMessage/applyResultPayload/saveResultSeries: у
// fetch-mail нет дедупликации по содержимому, повторный прогон создал бы дублирующую
// серию измерений (см. AGENTS.md). Результат применяется вручную, проверенным SQL UPDATE.
// -request — id заявки (для регистрации файла в таблице files); 0 (по умолчанию), если
// заявка не важна для этого прогона.
func runFetchMailPhotos(ctx context.Context, s *Server, args []string) {
	fs := flag.NewFlagSet("fetch-mail-photos", flag.ExitOnError)
	folder := fs.String("folder", "", "IMAP-папка (Comb, FlamProp)")
	targetID := fs.String("id", "", "значение JSON-поля \"ID\" письма (= requests.external_id)")
	requestID := fs.Int64("request", 0, "id заявки, к которой привязать файлы в таблице files (опционально)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("fetch-mail-photos: %v", err)
	}
	*folder = strings.TrimSpace(*folder)
	*targetID = strings.TrimSpace(*targetID)
	if *folder == "" || *targetID == "" {
		log.Fatalf("fetch-mail-photos: -folder и -id обязательны")
	}

	cfg, ok := loadEmailIngestConfigCore()
	if !ok {
		log.Fatalf("fetch-mail-photos: LAB_MAIL_IMAP_SERVER/LOGIN/PASSWORD/METHOD_MAP/LAB_ID не заданы или некорректны")
	}

	c, err := client.DialTLS(cfg.imapServer+":993", nil)
	if err != nil {
		log.Fatalf("fetch-mail-photos: dial: %v", err)
	}
	defer c.Logout()
	if err := c.Login(cfg.login, cfg.password); err != nil {
		log.Fatalf("fetch-mail-photos: login: %v", err)
	}

	section := &imap.BodySectionName{Peek: true}
	matchedSeqNums, err := searchMatchingSeqNums(c, *folder, *targetID, section)
	if err != nil {
		log.Fatalf("fetch-mail-photos: %v", err)
	}
	if len(matchedSeqNums) == 0 {
		log.Fatalf("fetch-mail-photos: письмо с ID=%s в папке %s не найдено", *targetID, *folder)
	}

	// Один external_id в Comb/FlamProp обычно даёт НЕСКОЛЬКО писем: настоящий
	// результат (форма испытателя, type="result", без mesure_data) и, отдельно,
	// сырую телеметрию прибора (mesure_data — тихо пропускается и обычным
	// воркером, см. processMessage/skipped_signal, решение пользователя
	// 2026-08-19). Фильтруем к ровно тому письму, что реально применил бы
	// processMessage — а не к первому найденному.
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
	matchSeqset := new(imap.SeqSet)
	matchSeqset.AddNum(matchedSeqNums...)
	matched := make(chan *imap.Message, len(matchedSeqNums))
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(matchSeqset, items, matched)
	}()
	type candidate struct {
		payload     map[string]any
		attachments []mailAttachment
	}
	var candidates []candidate
	for msg := range matched {
		body := msg.GetBody(section)
		if body == nil {
			log.Printf("fetch-mail-photos: seq=%d: пустое тело письма, пропущено", msg.SeqNum)
			continue
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			log.Printf("fetch-mail-photos: seq=%d: read body: %v", msg.SeqNum, err)
			continue
		}
		text, att, err := extractMailTextAndAttachments(raw)
		if err != nil {
			log.Printf("fetch-mail-photos: seq=%d: extract text: %v", msg.SeqNum, err)
			continue
		}
		payload, ok := extractJSON(text)
		if !ok {
			log.Printf("fetch-mail-photos: seq=%d: не найден JSON в теле письма, пропущено", msg.SeqNum)
			continue
		}
		if _, hasSignal := payload["mesure_data"]; hasSignal {
			log.Printf("fetch-mail-photos: seq=%d: сырая телеметрия прибора (mesure_data) — пропущено", msg.SeqNum)
			continue
		}
		if typ, _ := payload["type"].(string); typ != "result" {
			log.Printf("fetch-mail-photos: seq=%d: type=%q, не result — пропущено", msg.SeqNum, typ)
			continue
		}
		candidates = append(candidates, candidate{payload: payload, attachments: att})
	}
	if err := <-done; err != nil {
		log.Fatalf("fetch-mail-photos: fetch matched: %v", err)
	}
	if len(candidates) != 1 {
		log.Fatalf("fetch-mail-photos: ID=%s в папке %s: писем-результатов (после отсева телеметрии) найдено %d, ожидалось 1",
			*targetID, *folder, len(candidates))
	}
	payload, attachments := candidates[0].payload, candidates[0].attachments

	log.Printf("fetch-mail-photos: найдено вложений: %d", len(attachments))
	for _, a := range attachments {
		log.Printf("fetch-mail-photos: вложение %q (%s, %d байт)", a.Filename, a.ContentType, len(a.Data))
	}
	photoURLs := s.resolvePhotoAttachments(ctx, cfg, *requestID, payload, attachments)
	if len(photoURLs) == 0 {
		log.Printf("fetch-mail-photos: ни одно фото не сопоставилось/не загрузилось — см. предупреждения выше")
		return
	}
	for field, url := range photoURLs {
		log.Printf("fetch-mail-photos: %s -> %s", field, url)
	}
}

// peekPayload — та же экстракция JSON из тела письма, что происходит внутри
// processMessage, но БЕЗ каких-либо побочных эффектов (никакого recordProcessed) —
// используется только для фильтрации по ID в fetchAndApplyByID; настоящее
// применение письма всегда идёт через processMessage на отдельном, свежем Fetch.
func peekPayload(msg *imap.Message, section *imap.BodySectionName) (map[string]any, bool) {
	body := msg.GetBody(section)
	if body == nil {
		return nil, false
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, false
	}
	text, err := extractMailText(raw)
	if err != nil {
		return nil, false
	}
	return extractJSON(text)
}
