package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	pool        *pgxpool.Pool
	s3          *S3Store
	fileBaseURL string
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	if err := loadJWTSecret(); err != nil {
		log.Fatalf("JWT: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	s3Store, err := NewS3Store()
	if err != nil {
		log.Fatalf("S3: %v", err)
	}

	s := &Server{pool: pool, s3: s3Store}
	s.fileBaseURL = s3Store.publicBaseURL()

	// Постоянный CLI-режим (не сервер): перенос исторических заявок LPITrack
	// в requests с историческим годом номера — см. import_history.go и план
	// "перенос исторических заявок (LPITrack) в проект Old", 2026-08-21.
	if len(os.Args) > 1 && os.Args[1] == "import-lpitrack-history" {
		runImportLpitrackHistory(ctx, s, os.Args[2:])
		return
	}

	// Постоянный CLI-режим: точечная выгрузка почты по ID (2026-08-24, см.
	// mail_fetch_by_id.go) — 30-секундный ctx выше рассчитан на инициализацию
	// пула, для IMAP-выгрузки нескольких сотен писем даём отдельный, более
	// щедрый таймаут.
	if len(os.Args) > 1 && os.Args[1] == "fetch-mail" {
		fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer fetchCancel()
		runFetchMailByID(fetchCtx, s, os.Args[2:])
		return
	}

	// Одноразовый режим восстановления фото уже принятой заявки (2026-08-24, см.
	// mail_fetch_by_id.go runFetchMailPhotos) — НЕ вызывает processMessage/saveResultSeries
	// (fetch-mail не идемпотентен), только печатает получившиеся URL для ручного SQL-патча.
	if len(os.Args) > 1 && os.Args[1] == "fetch-mail-photos" {
		fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer fetchCancel()
		runFetchMailPhotos(fetchCtx, s, os.Args[2:])
		return
	}

	// Одноразовый CLI-режим «пересчитать все заявки» (2026-08-29, см. recalc_all.go).
	if len(os.Args) > 1 && os.Args[1] == "recalc-all" {
		recalcCtx, recalcCancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer recalcCancel()
		runRecalcAll(recalcCtx, s, os.Args[2:])
		return
	}

	if err := s.migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if err := s.rolloutRequests(ctx); err != nil {
		log.Fatalf("rolloutRequests: %v", err)
	}
	if err := s.seedOwner(ctx); err != nil {
		log.Fatalf("seedOwner: %v", err)
	}
	regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer regCancel()
	if err := s.registerApp(regCtx); err != nil {
		log.Printf("registerApp (non-fatal): %v", err)
	}

	// Фоновый воркер приёма заявок/результатов по почте — живёт весь процесс,
	// не привязан к стартовому ctx (у него таймаут 30с). Безопасен по умолчанию:
	// без LAB_MAIL_ENABLED=true не стартует (см. email_ingest.go).
	s.startEmailIngest(context.Background())

	// Фоновая проверка приближающегося срока поверки оборудования (2026-09-03) —
	// безопасен по умолчанию: без enabled=true в equipment_notify_settings ничего
	// не шлёт (см. equipment_notify.go).
	s.startEquipmentNotifyJob()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/lab/health", s.handleHealth)

	// Справочники
	mux.HandleFunc("GET /api/lab/labs", s.requirePerm("viewer")(s.handleListLabs))
	mux.HandleFunc("POST /api/lab/labs", s.requirePerm("superadmin")(s.handleCreateLab))
	mux.HandleFunc("PATCH /api/lab/labs/{id}", s.requirePerm("superadmin")(s.handleUpdateLab))
	mux.HandleFunc("GET /api/lab/methods", s.requirePerm("viewer")(s.handleListMethods))
	mux.HandleFunc("POST /api/lab/methods", s.requirePerm("editor")(s.handleCreateMethod))
	mux.HandleFunc("GET /api/lab/objects", s.requirePerm("viewer")(s.handleListObjects))
	mux.HandleFunc("POST /api/lab/objects", s.requirePerm("editor")(s.handleCreateObject))
	mux.HandleFunc("PATCH /api/lab/objects/{id}", s.requirePerm("editor")(s.handleUpdateObject))

	// Проекты (дерево)
	mux.HandleFunc("GET /api/lab/projects", s.requirePerm("viewer")(s.handleListProjects))
	mux.HandleFunc("POST /api/lab/projects", s.requirePerm("editor")(s.handleCreateProject))
	mux.HandleFunc("PATCH /api/lab/projects/{id}", s.requirePerm("editor")(s.handleUpdateProject))

	// Заявки
	mux.HandleFunc("GET /api/lab/requests", s.requirePerm("viewer")(s.handleListRequests))
	mux.HandleFunc("POST /api/lab/requests", s.requirePerm("editor")(s.handleCreateRequest))
	mux.HandleFunc("GET /api/lab/requests/{id}", s.requirePerm("viewer")(s.handleGetRequest))
	mux.HandleFunc("PATCH /api/lab/requests/{id}", s.requirePerm("editor")(s.handleUpdateRequest))
	// Смена статуса заявки для editor через веб запрещена (2026-09-03, по решению
	// пользователя) — через Obsidian-плагин (channel=plugin) editor может, как раньше.
	mux.HandleFunc("POST /api/lab/requests/{id}/status", s.requirePerm("editor")(s.requireMinRoleForWebChannel("admin")(s.handleSetRequestStatus)))
	mux.HandleFunc("POST /api/lab/requests/{id}/kanban-move", s.requirePerm("editor")(s.requireMinRoleForWebChannel("admin")(s.handleKanbanMove)))
	// WP2 (2026-08-28) — исходящая почта: ручная отправка + журнал заявки.
	mux.HandleFunc("POST /api/lab/requests/{id}/send-email", s.requirePerm("editor")(s.handleSendRequestEmail))
	mux.HandleFunc("GET /api/lab/requests/{id}/sent-emails", s.requirePerm("viewer")(s.handleListSentEmails))
	// WP8 (2026-08-29): журнал изменений заявки/результатов, см. audit_log.go.
	mux.HandleFunc("GET /api/lab/requests/{id}/audit-log", s.requirePerm("viewer")(s.handleListAuditLog))

	// Группы
	mux.HandleFunc("GET /api/lab/groups", s.requirePerm("viewer")(s.handleListGroups))
	mux.HandleFunc("POST /api/lab/groups", s.requirePerm("editor")(s.handleCreateGroup))
	mux.HandleFunc("POST /api/lab/groups/{id}/members", s.requirePerm("editor")(s.handleAddGroupMember))
	mux.HandleFunc("DELETE /api/lab/groups/{id}/members/{email}", s.requirePerm("editor")(s.handleRemoveGroupMember))

	// Синхронизация кэша плагина
	mux.HandleFunc("GET /api/lab/sync/pull", s.requirePerm("viewer")(s.handlePull))
	mux.HandleFunc("POST /api/lab/sync/push", s.requirePerm("editor")(s.handlePush))

	// Файлы (S3 через rclone)
	mux.HandleFunc("POST /api/lab/file", s.requirePerm("editor")(s.handleUploadFile))
	mux.HandleFunc("GET /api/lab/file", s.requirePerm("viewer")(s.handleDownloadFile))
	// Без requirePerm (2026-08-24, см. files.go handleFileRedirect) — <img src> в HTML
	// протокола/выписки не может приложить Authorization-заголовок; защита — случайность
	// ключа объекта, не JWT.
	mux.HandleFunc("GET /api/lab/file-redirect", s.handleFileRedirect)

	// Права доступа
	mux.HandleFunc("GET /api/lab/permissions/me", s.requirePerm("viewer")(s.handleMyPermission))
	mux.HandleFunc("GET /api/lab/permissions", s.requirePerm("admin")(s.handleListPermissions))
	mux.HandleFunc("POST /api/lab/permissions", s.requirePerm("admin")(s.handleSetPermission))
	mux.HandleFunc("GET /api/lab/common-access", s.requirePerm("admin")(s.handleGetCommonAccess))
	mux.HandleFunc("POST /api/lab/common-access", s.requirePerm("admin")(s.handleSetCommonAccess))

	// Разовое исправление уже импортированных заявок с заглушкой "Без названия"
	// при заданном ЕКН (2026-08-22, см. ekn_client.go) — админ вызывает вручную.
	mux.HandleFunc("POST /api/lab/admin/backfill-ekn-names", s.requirePerm("admin")(s.handleBackfillEknNames))

	// ЛИМС: результаты
	mux.HandleFunc("GET /api/lab/requests/{id}/results", s.requirePerm("viewer")(s.handleListResults))
	mux.HandleFunc("POST /api/lab/requests/{id}/results", s.requirePerm("editor")(s.handleCreateResult))
	mux.HandleFunc("GET /api/lab/requests/{id}/results/aggregated", s.requirePerm("viewer")(s.handleListAggregated))
	mux.HandleFunc("POST /api/lab/requests/{id}/results/{series}/calculate", s.requirePerm("editor")(s.handleCalculateSeries))
	// WP3a (2026-08-28) — удаление серии с перенумерацией последующих.
	mux.HandleFunc("DELETE /api/lab/requests/{id}/results/{series}", s.requirePerm("editor")(s.handleDeleteResultSeries))
	// Буфер данных приборов (2026-08-28) — не привязан к заявке/лаборатории (прибор не
	// знает номер заявки), поэтому обычный editor-уровень, без requireLabAccess.
	mux.HandleFunc("POST /api/lab/instrument-buffer", s.requirePerm("editor")(s.handleCreateInstrumentBuffer))

	// ЛИМС: справочники
	mux.HandleFunc("GET /api/lab/inventors", s.requirePerm("viewer")(s.handleListInventors))
	mux.HandleFunc("POST /api/lab/inventors", s.requirePerm("editor")(s.handleCreateInventor))
	mux.HandleFunc("PATCH /api/lab/inventors/{id}", s.requirePerm("editor")(s.handleUpdateInventor))
	mux.HandleFunc("DELETE /api/lab/inventors/{id}", s.requirePerm("editor")(s.handleDeleteInventor))
	mux.HandleFunc("GET /api/lab/equipment", s.requirePerm("viewer")(s.handleListEquipment))
	mux.HandleFunc("POST /api/lab/equipment", s.requirePerm("editor")(s.handleCreateEquipment))
	mux.HandleFunc("PATCH /api/lab/equipment/{id}", s.requirePerm("editor")(s.handleUpdateEquipment))
	mux.HandleFunc("DELETE /api/lab/equipment/{id}", s.requirePerm("editor")(s.handleDeleteEquipment))
	mux.HandleFunc("POST /api/lab/equipment/{id}/scan", s.requirePerm("editor")(s.handleEquipmentScan))
	mux.HandleFunc("GET /api/lab/equipment/{id}/calibrations", s.requirePerm("viewer")(s.handleListEquipmentCalibrations))
	mux.HandleFunc("POST /api/lab/equipment/{id}/calibrations", s.requirePerm("editor")(s.handleCreateEquipmentCalibration))
	// Калибровочная кривая (WP1, 2026-08-28) — PNG-график ОДНОГО атрибута data_type="curve"
	// конкретной записи калибровки. В отличие от chart_configs метода, не требует отдельной
	// настройки — любой атрибут-кривая автоматически графируется по своим точкам.
	mux.HandleFunc("GET /api/lab/equipment/{id}/calibrations/{calibration_id}/curve-chart/{attr_id}",
		s.requirePerm("viewer")(s.handleCalibrationCurveChart))
	mux.HandleFunc("GET /api/lab/equipment/{id}/methods", s.requirePerm("viewer")(s.handleListEquipmentMethods))
	mux.HandleFunc("POST /api/lab/equipment/{id}/methods", s.requirePerm("editor")(s.handleSetEquipmentMethod))
	mux.HandleFunc("DELETE /api/lab/equipment/{id}/methods/{method_id}", s.requirePerm("editor")(s.handleDeleteEquipmentMethod))
	mux.HandleFunc("GET /api/lab/equipment/{id}/documents", s.requirePerm("viewer")(s.handleListEquipmentDocuments))
	mux.HandleFunc("POST /api/lab/equipment/{id}/documents", s.requirePerm("editor")(s.handleUploadEquipmentDocument))
	mux.HandleFunc("DELETE /api/lab/equipment/{id}/documents/{file_id}", s.requirePerm("editor")(s.handleDeleteEquipmentDocument))
	mux.HandleFunc("GET /api/lab/equipment-notify-settings", s.requirePerm("admin")(s.handleGetEquipmentNotifySettings))
	mux.HandleFunc("POST /api/lab/equipment-notify-settings", s.requirePerm("admin")(s.handleSetEquipmentNotifySettings))
	mux.HandleFunc("GET /api/lab/equipment-links", s.requirePerm("viewer")(s.handleListAllEquipmentLinks))
	mux.HandleFunc("GET /api/lab/method-equipment", s.requirePerm("viewer")(s.handleListAllMethodEquipment))
	mux.HandleFunc("POST /api/lab/equipment/{id}/auxiliaries", s.requirePerm("editor")(s.handleAddEquipmentAuxiliary))
	mux.HandleFunc("DELETE /api/lab/equipment/{id}/auxiliaries/{auxiliary_id}", s.requirePerm("editor")(s.handleRemoveEquipmentAuxiliary))
	mux.HandleFunc("GET /api/lab/lab-members", s.requirePerm("viewer")(s.handleListLabMembers))
	mux.HandleFunc("POST /api/lab/lab-members", s.requirePerm("editor")(s.handleSetLabMember))
	mux.HandleFunc("DELETE /api/lab/lab-members/{lab_id}/{email}", s.requirePerm("editor")(s.handleRemoveLabMember))

	// ЛИМС: методы (конфиги, удаление)
	mux.HandleFunc("PATCH /api/lab/methods/{id}", s.requirePerm("editor")(s.handleUpdateMethodConfig))
	mux.HandleFunc("DELETE /api/lab/methods/{id}", s.requirePerm("editor")(s.handleDeleteMethod))

	// ЛИМС: графики, протокол, дашборд
	mux.HandleFunc("GET /api/lab/requests/{id}/chart/{cfg_id}", s.requirePerm("viewer")(s.handleChart))
	// protocol/export.xlsx понижены с editor до viewer на роуте (2026-09-03, по
	// прямому запросу пользователя) — тот же паттерн, что chart/results/audit-log:
	// реальная видимость проверяется внутри хендлера через requireLabRead
	// (владелец/заказчик, участник группы, сотрудник лабы, admin+), а не только
	// глобальной ролью на роуте. Раньше viewer (участник группы) не мог дойти
	// даже до requireLabRead — падал на этом внешнем гейте.
	mux.HandleFunc("POST /api/lab/requests/{id}/protocol", s.requirePerm("viewer")(s.handleProtocol))
	mux.HandleFunc("GET /api/lab/requests/{id}/export.xlsx", s.requirePerm("viewer")(s.handleExportExcel))
	mux.HandleFunc("GET /api/lab/dashboard", s.requirePerm("viewer")(s.handleDashboard))

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("lab-service listening on :%s", port)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}

func (s *Server) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS labs (
			id BIGSERIAL PRIMARY KEY,
			code TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS methods (
			id BIGSERIAL PRIMARY KEY,
			code TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			lab_id BIGINT REFERENCES labs(id),
			description TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS objects (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			characteristics JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id BIGSERIAL PRIMARY KEY,
			parent_id BIGINT REFERENCES projects(id),
			code TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			is_ekn BOOLEAN NOT NULL DEFAULT false,
			owner_email TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS groups (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			owner_email TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS group_members (
			group_id BIGINT REFERENCES groups(id) ON DELETE CASCADE,
			email TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'viewer',
			PRIMARY KEY (group_id, email)
		)`,
		`CREATE TABLE IF NOT EXISTS requests (
			id BIGSERIAL PRIMARY KEY,
			number_seq BIGINT NOT NULL DEFAULT 0,
			number_year INT NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			object_id BIGINT REFERENCES objects(id),
			project_id BIGINT REFERENCES projects(id),
			group_id BIGINT REFERENCES groups(id),
			owner_email TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'new',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS request_methods (
			request_id BIGINT REFERENCES requests(id) ON DELETE CASCADE,
			method_id BIGINT REFERENCES methods(id),
			customer_number TEXT NOT NULL DEFAULT '',
			lab_number TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (request_id, method_id)
		)`,
		`CREATE TABLE IF NOT EXISTS files (
			id BIGSERIAL PRIMARY KEY,
			request_id BIGINT REFERENCES requests(id) ON DELETE CASCADE,
			file_key TEXT NOT NULL DEFAULT '',
			file_name TEXT NOT NULL DEFAULT '',
			file_size BIGINT NOT NULL DEFAULT 0,
			file_url TEXT NOT NULL DEFAULT '',
			uploaded_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS lab_permissions (
			app TEXT NOT NULL,
			email TEXT NOT NULL,
			role TEXT NOT NULL,
			PRIMARY KEY (app, email)
		)`,
		`CREATE TABLE IF NOT EXISTS lab_common_access (
			app TEXT PRIMARY KEY,
			level TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS request_seq (
			seq_year INT PRIMARY KEY,
			last_value BIGINT NOT NULL DEFAULT 0
		)`,
		`ALTER TABLE labs ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'internal'`,
		// Внешняя лаборатория не может существовать самостоятельно — обязательно
		// привязывается к внутренней при создании (см. handleCreateLab). У внутренних
		// parent_lab_id пуст. Внешняя лаба может иметь свои методы (расширяет
		// возможности внутренней) — видимость таких заявок резолвится через родителя
		// (requestVisible/visibleRequestsQuery/requestLabID: COALESCE(parent_lab_id, id)).
		`ALTER TABLE labs ADD COLUMN IF NOT EXISTS parent_lab_id BIGINT REFERENCES labs(id)`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS determinable_indicators JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE projects ADD COLUMN IF NOT EXISTS group_id BIGINT REFERENCES groups(id)`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS priority TEXT NOT NULL DEFAULT 'normal'`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS test_purpose TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS external_lab_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS ekn TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS method_id BIGINT REFERENCES methods(id)`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS customer_number TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS lab_number TEXT NOT NULL DEFAULT ''`,
		// external_id — номер из legacy-системы (email-трекер LPITrack, "LPIZAYAVKINAPRO-<N>")
		// для заявок переходного периода миграции; у новых заявок пусто.
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS external_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_requests_external_id ON requests(external_id) WHERE external_id <> ''`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS formulas JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS classification JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS chart_configs JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS input_parameters JSONB NOT NULL DEFAULT '[]'`,
		// presentation — конфигуратор методов, блок 3 (2026-08-21): порядок/подписи/
		// видимость колонок в таблице результатов (UI) и в протоколе — заменяет
		// недетерминированный обход map в protocol.go (см. AGENTS.md).
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS presentation JSONB NOT NULL DEFAULT '{}'`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS operator_form JSONB NOT NULL DEFAULT '{}'`,
		`CREATE TABLE IF NOT EXISTS inventors (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			phone TEXT NOT NULL DEFAULT '',
			department TEXT NOT NULL DEFAULT '',
			position TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS inventor_methods (
			inventor_id BIGINT REFERENCES inventors(id) ON DELETE CASCADE,
			method_id BIGINT REFERENCES methods(id),
			cert_number TEXT NOT NULL DEFAULT '',
			valid_until DATE,
			PRIMARY KEY (inventor_id, method_id)
		)`,
		`CREATE TABLE IF NOT EXISTS equipment (
			id BIGSERIAL PRIMARY KEY,
			code TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			location TEXT NOT NULL DEFAULT '',
			responsible TEXT NOT NULL DEFAULT '',
			last_calibration DATE,
			next_calibration DATE,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS method_equipment (
			method_id BIGINT REFERENCES methods(id) ON DELETE CASCADE,
			equipment_id BIGINT REFERENCES equipment(id),
			is_required BOOLEAN NOT NULL DEFAULT false,
			PRIMARY KEY (method_id, equipment_id)
		)`,
		`CREATE TABLE IF NOT EXISTS lab_members (
			lab_id BIGINT REFERENCES labs(id) ON DELETE CASCADE,
			email TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'lab_operator',
			PRIMARY KEY (lab_id, email)
		)`,
		`CREATE TABLE IF NOT EXISTS measurement_results (
			id BIGSERIAL PRIMARY KEY,
			request_id BIGINT REFERENCES requests(id) ON DELETE CASCADE,
			method_id BIGINT REFERENCES methods(id),
			inventor_id BIGINT REFERENCES inventors(id),
			series_num INT NOT NULL DEFAULT 1,
			values JSONB NOT NULL DEFAULT '{}',
			file_links JSONB NOT NULL DEFAULT '{}',
			photo_before TEXT NOT NULL DEFAULT '',
			photo_after TEXT NOT NULL DEFAULT '',
			is_statistical_row BOOLEAN NOT NULL DEFAULT false,
			calculation_type TEXT NOT NULL DEFAULT '',
			source_series_count INT NOT NULL DEFAULT 0,
			source_series_range TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (request_id, method_id, series_num)
		)`,
		`CREATE TABLE IF NOT EXISTS aggregated_results (
			id BIGSERIAL PRIMARY KEY,
			request_id BIGINT REFERENCES requests(id) ON DELETE CASCADE,
			method_id BIGINT REFERENCES methods(id),
			calculation_type TEXT NOT NULL DEFAULT '',
			result_data JSONB NOT NULL DEFAULT '{}',
			source_series_count INT NOT NULL DEFAULT 0,
			source_series_range TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_meas_req_method_series') THEN
				ALTER TABLE measurement_results ADD CONSTRAINT uq_meas_req_method_series UNIQUE (request_id, method_id, series_num);
			END IF;
		END $$`,
		// Приём заявок/результатов по почте (email_ingest.go) — дедуп по Message-ID
		// (или, если письмо его не прислало, по псевдо-ключу "uid:{folder}:{uid}")
		// и персистентный буфер результатов, пришедших раньше заявки.
		`CREATE TABLE IF NOT EXISTS email_ingest_processed (
			message_id TEXT PRIMARY KEY,
			folder TEXT NOT NULL,
			email_type TEXT NOT NULL DEFAULT '',
			raw_payload JSONB NOT NULL DEFAULT '{}',
			request_id BIGINT,
			error TEXT NOT NULL DEFAULT '',
			processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS email_ingest_pending_results (
			id BIGSERIAL PRIMARY KEY,
			external_id TEXT NOT NULL,
			method_id BIGINT NOT NULL,
			payload JSONB NOT NULL,
			attempts INT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// Вложения письма (фото), буферизованного до появления заявки (2026-08-24) — без
		// этой колонки retryPendingResults применял бы такое письмо уже без фото (см.
		// AGENTS.md "фото в протоколе"). '[]' — письма, буферизованные до этого фикса.
		`ALTER TABLE email_ingest_pending_results ADD COLUMN IF NOT EXISTS attachments JSONB NOT NULL DEFAULT '[]'`,
		// Метод теперь может принадлежать НЕСКОЛЬКИМ лабораториям (по требованию
		// пользователя, 2026-08-19) — старая колонка methods.lab_id (1:1) больше не
		// пишется новым кодом, но НЕ удаляется (как request_methods в декомпозиции
		// 2026-08-18 — DROP COLUMN сломал бы миграцию при повторном запуске, если
		// какой-то шаг ниже ещё читает эту колонку; см. AGENTS.md). method_labs —
		// новый источник истины; method_id CASCADE (удаление метода не должно биться
		// об FK), lab_id — нет (нельзя удалить лабу, пока на неё есть ссылки).
		`CREATE TABLE IF NOT EXISTS method_labs (
			method_id BIGINT REFERENCES methods(id) ON DELETE CASCADE,
			lab_id BIGINT REFERENCES labs(id),
			PRIMARY KEY (method_id, lab_id)
		)`,
		`INSERT INTO method_labs (method_id, lab_id)
			SELECT id, lab_id FROM methods WHERE lab_id IS NOT NULL
			ON CONFLICT (method_id, lab_id) DO NOTHING`,
		// requests.lab_id — КОНКРЕТНАЯ лаборатория (одна из method_labs метода этой
		// заявки), выбранная при создании; заменяет одновременно старую methods.lab_id
		// (для нумерации/видимости заявки) и requests.external_lab_id (упразднён по
		// требованию пользователя — был независимым чекбоксом «внешняя лаборатория»,
		// не связанным с методом, что стало избыточным и потенциально противоречивым
		// после введения method_labs). Колонка external_lab_id тоже НЕ удаляется
		// (тот же принцип, что выше) — просто не читается/не пишется новым кодом.
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS lab_id BIGINT REFERENCES labs(id)`,
		`UPDATE requests SET lab_id = (SELECT lab_id FROM methods WHERE methods.id = requests.method_id)
			WHERE requests.lab_id IS NULL AND requests.method_id IS NOT NULL`,
		// Системные атрибуты (2026-08-23) — данные, общие для ЛЮБОГО метода (испытатель,
		// даты, условия окружающей среды при испытании), не специфичные для конкретной
		// методики. По решению пользователя заводятся ОДИН РАЗ на уровне заявки, а не как
		// per-method MethodAttribute (что раньше приводило к дублированию — см. ГВ,
		// amb_temp/amb_pres/amb_moist/exp_date были собственными атрибутами метода, хотя
		// реальные письма Comb/Flam/FlamProp несут те же поля для ЛЮБОГО метода, см.
		// json_attr.md). Резолвятся как системные плейсхолдеры (resolveSystemPlaceholder,
		// protocol.go) и заполняются автоматически при приёме письма-результата
		// (email_ingest.go, canonicalFieldNames уже переводит flam_inventor/flam_rep_date/
		// flam_date_material_in/flam_exp_date в inventor/report_date/samples_in_date/exp_date).
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS inventor_id BIGINT REFERENCES inventors(id) ON DELETE SET NULL`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS report_date TEXT`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS samples_in_date TEXT`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS exp_date TEXT`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS amb_temp TEXT`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS amb_pres TEXT`,
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS amb_moist TEXT`,
		// Kanban-доска «Очередь лаборатории» (sbe-lims, 2026-08-24, см. kanban.go):
		// assigned_to — email испытателя (lab_members.email, role lab_operator/
		// lab_admin лабы заявки); назначает/переназначает только руководитель лабы
		// (глобальная роль admin/superadmin) — перетаскиванием в ячейку испытателя
		// или пикером в детали заявки; испытатель может лишь забрать СЕБЕ
		// неназначенную заявку из "новых". '' — не назначено (в т.ч. все заявки,
		// созданные до этой миграции).
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS assigned_to TEXT NOT NULL DEFAULT ''`,
		// completed_at — момент перехода в status='completed' (НЕ updated_at) —
		// основа 10-рабочедневного окна показа в колонке "Завершённые". Ставится/
		// чистится автоматически при КАЖДОМ изменении status — во всех трёх местах
		// записи (handleSetRequestStatus, handleKanbanMove, pushUpdate).
		`ALTER TABLE requests ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ`,
		// Расширение справочника «Оборудование» (2026-08-26, см. equipment_ext.go):
		// эксплуатация/поверка/калибровка. last_calibration/next_calibration уже
		// существовали (дормантные с первой миграции ЛИМС) — теперь пересчитываются
		// сервером при каждой новой записи equipment_calibrations, не задаются вручную.
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS commissioned_at DATE`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS service_life TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_cert_number TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_cert_date DATE`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_cert_file_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_cert_file_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_act_number TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_act_date DATE`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_act_file_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_act_file_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS calibration_interval_months INT`,
		// type (2026-09-03) — единая роль оборудования "main"/"auxiliary", заменяет
		// прежний per-method выбор (method_equipment.role, см. ниже) по прямому
		// запросу пользователя: карточка сама решает, основное это оборудование или
		// вспомогательное, а не каждая связь с методом отдельно. Колонка
		// method_equipment.role НЕ удаляется и не перестаёт читаться — её продолжает
		// использовать calibration_curve.go (resolveSingleMainEquipment,
		// isMainEquipmentOfMethod); handleUpdateEquipment/handleSetEquipmentMethod
		// (equipment_ext.go) держат её синхронной с equipment.type, чтобы не
		// переписывать эти запросы.
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'main'`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'equipment_type_check') THEN
				ALTER TABLE equipment ADD CONSTRAINT equipment_type_check CHECK (type IN ('main', 'auxiliary'));
			END IF;
		END $$`,
		// Бэкофилл при первом накате: оборудование, у которого среди существующих
		// method_equipment есть роль 'auxiliary' и НЕТ ни одной 'main' — считаем
		// вспомогательным; остальное (включая оборудование без единой привязки)
		// остаётся на дефолте колонки 'main'.
		`UPDATE equipment SET type = 'auxiliary'
			WHERE id IN (
				SELECT equipment_id FROM method_equipment GROUP BY equipment_id
				HAVING bool_or(role = 'auxiliary') AND NOT bool_or(role = 'main')
			) AND type = 'main'`,
		// verification_expiry_date (2026-09-03) — срок окончания действия акта
		// поверки (в отличие от verification_act_date — даты САМОГО акта, у него
		// раньше не было срока действия вовсе). Основа для оповещений — см.
		// equipment_notify.go.
		`ALTER TABLE equipment ADD COLUMN IF NOT EXISTS verification_expiry_date DATE`,
		// Оповещения о приближении срока поверки (2026-09-03) — единые настройки на
		// весь модуль (получатели+пороги в днях), по образцу
		// documents_notify_settings/documents_notifications в sbe-documents, см.
		// equipment_notify.go.
		`CREATE TABLE IF NOT EXISTS equipment_notify_settings (
			id INT PRIMARY KEY DEFAULT 1,
			enabled BOOLEAN NOT NULL DEFAULT false,
			days TEXT NOT NULL DEFAULT '30,14,7',
			recipients TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO equipment_notify_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS equipment_notifications (
			equipment_id BIGINT NOT NULL REFERENCES equipment(id) ON DELETE CASCADE,
			day INT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (equipment_id, day, email)
		)`,
		// role — 'main'|'auxiliary', на каждой связи оборудование↔метод отдельно (одно
		// и то же оборудование может быть основным для одного метода и вспомогательным
		// для другого). is_required — другая, уже существующая семантика, не трогаем.
		`ALTER TABLE method_equipment ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'auxiliary'`,
		`CREATE TABLE IF NOT EXISTS equipment_calibrations (
			id BIGSERIAL PRIMARY KEY,
			equipment_id BIGINT NOT NULL REFERENCES equipment(id) ON DELETE CASCADE,
			calibrated_at DATE NOT NULL,
			result TEXT NOT NULL DEFAULT '',
			file_key TEXT NOT NULL DEFAULT '',
			file_url TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// files — переиспользуется для «Документации на оборудование» (список файлов,
		// не один сертификат/акт): request_id остаётся NULL, equipment_id заполнен,
		// purpose='equipment_doc' отличает от файлов заявок при листинге.
		`ALTER TABLE files ADD COLUMN IF NOT EXISTS equipment_id BIGINT REFERENCES equipment(id) ON DELETE CASCADE`,
		`ALTER TABLE files ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT ''`,
		// equipment_links (2026-08-26) — привязка оборудование↔оборудование (физическое
		// прикрепление вспомогательного прибора к основному, напр. датчик к анализатору);
		// ОТДЕЛЬНО и независимо от method_equipment.role (тот определяет видимость блока
		// калибровки для метода, этот — только группировку/отображение в общем списке
		// оборудования). many-to-many: один вспомогательный прибор может быть привязан
		// к нескольким основным (CHECK исключает привязку прибора к самому себе).
		`CREATE TABLE IF NOT EXISTS equipment_links (
			main_equipment_id BIGINT NOT NULL REFERENCES equipment(id) ON DELETE CASCADE,
			auxiliary_equipment_id BIGINT NOT NULL REFERENCES equipment(id) ON DELETE CASCADE,
			PRIMARY KEY (main_equipment_id, auxiliary_equipment_id),
			CHECK (main_equipment_id <> auxiliary_equipment_id)
		)`,
		// Параметры калибровки метода (2026-08-26) — конфигуратор метода, новый раздел:
		// calibration_attributes — атрибуты, которые заполняет испытатель ПРИ калибровке
		// (простая форма id/название/тип — без fill_method/formula/aggregation, как у
		// input_parameters: значение калибровки всегда вводится вручную ровно один раз
		// за запись журнала, ни расчётов, ни агрегации по сериям здесь нет);
		// calibration_operator_form — та же структура {fields:[{attribute_id,required}]},
		// что operator_form, но какие ИЗ calibration_attributes показывать испытателю.
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS calibration_attributes JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE methods ADD COLUMN IF NOT EXISTS calibration_operator_form JSONB NOT NULL DEFAULT '{}'`,
		// equipment_calibrations — привязка к методу (чьи calibration_attributes
		// применялись — оборудование может быть "Основное" сразу для нескольких методов,
		// см. method_equipment.role) + универсальные системные поля (одни и те же для
		// ЛЮБОГО метода, тот же принцип, что requests.amb_temp/amb_pres/amb_moist у
		// обычных результатов испытаний — см. sbe-lims/AGENTS.md, "Правило: системные
		// атрибуты") + values — значения calibration_attributes ЭТОГО метода (JSONB,
		// та же роль, что measurement_results.values).
		`ALTER TABLE equipment_calibrations ADD COLUMN IF NOT EXISTS method_id BIGINT REFERENCES methods(id)`,
		`ALTER TABLE equipment_calibrations ADD COLUMN IF NOT EXISTS amb_temp TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment_calibrations ADD COLUMN IF NOT EXISTS amb_pres TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment_calibrations ADD COLUMN IF NOT EXISTS amb_moist TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE equipment_calibrations ADD COLUMN IF NOT EXISTS values JSONB NOT NULL DEFAULT '{}'`,
		// instrument_result_buffer (2026-08-28) — приёмник данных от внешних приборов
		// (первый потребитель: TDT Reader, метод ГГ), НЕ привязан к заявке/методу —
		// прибор не знает и не должен знать номер заявки/серии (риск пришить данные не к
		// тому эксперименту, прямое решение пользователя). Прибор шлёт {hash, values} сюда
		// сразу после эксперимента и показывает hash как QR; испытатель в форме результатов
		// вставляет hash — сервер находит запись ПО hash (сам поиск — и есть проверка
		// целостности: опечатка/чужой hash просто не найдётся), сливает values в
		// measurement_results и помечает запись consumed_at/consumed_by_result_id, чтобы тот
		// же hash нельзя было случайно использовать повторно для другой заявки.
		`CREATE TABLE IF NOT EXISTS instrument_result_buffer (
			hash TEXT PRIMARY KEY,
			values JSONB NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			consumed_at TIMESTAMPTZ,
			consumed_by_result_id BIGINT REFERENCES measurement_results(id) ON DELETE SET NULL
		)`,
		// measurement_results.equipment_id (2026-08-28, WP1 — калибровочная кривая, см.
		// docs/superpowers/specs/2026-08-28-sbe-lims-calibration-curve-design.md): на каком
		// именно экземпляре оборудования выполнено измерение — нужно, когда у метода
		// несколько единиц "Основного" оборудования (method_equipment.role='main'), каждая
		// со своей калибровочной кривой. Nullable — при ровно одной основной единице сервер
		// резолвит её сам (resolveSingleMainEquipment), поле не обязательно. Общая
		// доработка traceability для ЛЮБОГО метода, не только РП.
		`ALTER TABLE measurement_results ADD COLUMN IF NOT EXISTS equipment_id BIGINT REFERENCES equipment(id)`,
		// WP2 (2026-08-28, исходящая почта) — см.
		// docs/superpowers/specs/2026-08-28-sbe-lims-outbound-email-design.md.
		// auto_send_email — per-lab переключатель автоотправки при переходе заявки в
		// completed (по умолчанию выключено — явное решение админа лабы, не глобальный
		// флаг). sent_emails — журнал КАЖДОЙ попытки отправки (включая неудачные —
		// видимость важнее), и заодно источник для защиты от повторной автоотправки
		// (см. triggerCompletionEmails в outbound_email.go).
		`ALTER TABLE labs ADD COLUMN IF NOT EXISTS auto_send_email BOOLEAN NOT NULL DEFAULT false`,
		// service_email (2026-08-29) — живая жалоба: письмо в трекер ушло не на тот
		// адрес (глобальный LAB_LPITRACK_EMAIL из .env не подходил именно этой лабе) —
		// ручное указание адреса служебных писем (дубль в LPITrack при completed,
		// уведомление о взятии в работу) per-лаба, приоритетнее env-переменной. Пусто
		// по умолчанию — тогда используется LAB_LPITRACK_EMAIL, как раньше (обратная
		// совместимость). Адрес заказчика НЕ отсюда — берётся из requests.owner_email,
		// этого поля не касается.
		`ALTER TABLE labs ADD COLUMN IF NOT EXISTS service_email TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS sent_emails (
			id BIGSERIAL PRIMARY KEY,
			request_id BIGINT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			recipient_type TEXT NOT NULL,
			recipient_address TEXT NOT NULL,
			sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			success BOOLEAN NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			triggered_by TEXT NOT NULL
		)`,
		// WP3b (2026-08-28) — системные поля (report_date/samples_in_date/exp_date/
		// amb_temp/amb_pres/amb_moist) переезжают с requests.* (одно значение на
		// всю заявку) на measurement_results.values (по сериям — испытания одной
		// заявки могут идти в разные дни с разными условиями среды). requests.*
		// НЕ удаляются (остаются fallback для протокола, см. resolveSystemPlaceholder/
		// seriesSystemField в protocol.go) — этот backfill одноразово копирует уже
		// накопленные значения в values ПОСЛЕДНЕЙ серии каждой заявки+метода, чтобы
		// не потерять историю для уже существующих заявок. Идемпотентно (безопасно
		// гонять на каждом старте): трогает только строки, где такого ключа в
		// values ЕЩЁ нет — новые сохранения (handleCreateResult/email_ingest.go,
		// см. WP3b) сразу пишут эти поля в values сами, backfill им не мешает.
		// См. docs/superpowers/specs/2026-08-28-sbe-lims-system-fields-per-series-design.md.
		`UPDATE measurement_results mr SET values = jsonb_set(mr.values, '{report_date}', to_jsonb(r.report_date), true)
FROM requests r
WHERE mr.request_id = r.id AND mr.is_statistical_row = false AND COALESCE(r.report_date, '') != ''
  AND NOT (mr.values ? 'report_date')
  AND mr.series_num = (SELECT MAX(series_num) FROM measurement_results mr2
                       WHERE mr2.request_id = mr.request_id AND mr2.method_id = mr.method_id
                         AND mr2.is_statistical_row = false)`,
		`UPDATE measurement_results mr SET values = jsonb_set(mr.values, '{samples_in_date}', to_jsonb(r.samples_in_date), true)
FROM requests r
WHERE mr.request_id = r.id AND mr.is_statistical_row = false AND COALESCE(r.samples_in_date, '') != ''
  AND NOT (mr.values ? 'samples_in_date')
  AND mr.series_num = (SELECT MAX(series_num) FROM measurement_results mr2
                       WHERE mr2.request_id = mr.request_id AND mr2.method_id = mr.method_id
                         AND mr2.is_statistical_row = false)`,
		`UPDATE measurement_results mr SET values = jsonb_set(mr.values, '{exp_date}', to_jsonb(r.exp_date), true)
FROM requests r
WHERE mr.request_id = r.id AND mr.is_statistical_row = false AND COALESCE(r.exp_date, '') != ''
  AND NOT (mr.values ? 'exp_date')
  AND mr.series_num = (SELECT MAX(series_num) FROM measurement_results mr2
                       WHERE mr2.request_id = mr.request_id AND mr2.method_id = mr.method_id
                         AND mr2.is_statistical_row = false)`,
		`UPDATE measurement_results mr SET values = jsonb_set(mr.values, '{amb_temp}', to_jsonb(r.amb_temp), true)
FROM requests r
WHERE mr.request_id = r.id AND mr.is_statistical_row = false AND COALESCE(r.amb_temp, '') != ''
  AND NOT (mr.values ? 'amb_temp')
  AND mr.series_num = (SELECT MAX(series_num) FROM measurement_results mr2
                       WHERE mr2.request_id = mr.request_id AND mr2.method_id = mr.method_id
                         AND mr2.is_statistical_row = false)`,
		`UPDATE measurement_results mr SET values = jsonb_set(mr.values, '{amb_pres}', to_jsonb(r.amb_pres), true)
FROM requests r
WHERE mr.request_id = r.id AND mr.is_statistical_row = false AND COALESCE(r.amb_pres, '') != ''
  AND NOT (mr.values ? 'amb_pres')
  AND mr.series_num = (SELECT MAX(series_num) FROM measurement_results mr2
                       WHERE mr2.request_id = mr.request_id AND mr2.method_id = mr.method_id
                         AND mr2.is_statistical_row = false)`,
		`UPDATE measurement_results mr SET values = jsonb_set(mr.values, '{amb_moist}', to_jsonb(r.amb_moist), true)
FROM requests r
WHERE mr.request_id = r.id AND mr.is_statistical_row = false AND COALESCE(r.amb_moist, '') != ''
  AND NOT (mr.values ? 'amb_moist')
  AND mr.series_num = (SELECT MAX(series_num) FROM measurement_results mr2
                       WHERE mr2.request_id = mr.request_id AND mr2.method_id = mr.method_id
                         AND mr2.is_statistical_row = false)`,
		// WP4 (2026-08-29) — кто создал/последний раз изменил серию (email вошедшего
		// пользователя; для автоматических сохранений из email-приёма — литерал
		// "email-ingest", см. saveResultSeries/email_ingest.go). Строгих прав на
		// основе этого поля НЕТ (решение пользователя — редактировать может любой
		// испытатель/админ лабы) — только факт фиксации + база для будущего
		// журнала изменений (WP8). Тот же паттерн, что уже есть у
		// equipment_calibrations.created_by.
		`ALTER TABLE measurement_results ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE measurement_results ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT ''`,
		// WP8 (2026-08-29) — журнал изменений заявки/результатов, см.
		// docs/superpowers/specs/2026-08-29-sbe-lims-audit-log-design.md. Одна таблица
		// (не две) — единый хронологический список без merge-sort на клиенте.
		// Разреженные колонки: kind='status' использует old_status/new_status,
		// kind='result_created'/'result_updated' использует method_id/series_num/
		// values_before/values_after — осознанный компромисс ради единого API/UI.
		// Только уровень приложения (см. audit_log.go) — прямые SQL-изменения (как
		// массовые скрипты этой же сессии) в журнал не попадают.
		`CREATE TABLE IF NOT EXISTS request_audit_log (
			id BIGSERIAL PRIMARY KEY,
			request_id BIGINT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			who TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			old_status TEXT,
			new_status TEXT,
			method_id BIGINT,
			series_num INT,
			values_before JSONB,
			values_after JSONB
		)`,
		`CREATE INDEX IF NOT EXISTS request_audit_log_request_idx ON request_audit_log(request_id, created_at DESC)`,
		// Триггеры почтового приёма на проекте (2026-09-02, прямой запрос
		// пользователя): заявка без ЕКН (или с ЕКН, не создавшим свой автопроект)
		// маршрутизируется в конкретный проект, если её ЕКН/отправитель совпал с
		// одним из этих полей — см. projects.go Project, email_ingest.go
		// findProjectByMailTrigger. Пусто — проект вне авто-маршрутизации.
		`ALTER TABLE projects ADD COLUMN IF NOT EXISTS mail_trigger_ekn TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN IF NOT EXISTS mail_trigger_sender TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func parseTime(v string, fallback time.Time) time.Time {
	if v == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return fallback
	}
	return t
}
