# AGENTS.md — lab-service (Заявки на испытания)

Go-сервис «Заявки на испытания» для SBE-плагина sbe-requests. Контейнер `lab`,
БД `lab` (postgres `lab-db`), авторизация — JWT HS256 (общий `JWT_SECRET` с auth-service)
+ роли из `lab_permissions`. Универсальный для лабораторий (потребители: sbe-requests,
sbe-lims, sbe-ekn). Файлы заявок — в S3 (бакет `sbe-doc`) через rclone CLI.
Деплой: `/opt/mailers/lab-service/`.

## Назначение (текущее)

- Справочники: `labs` / `methods` (метод привязан к лаборатории `lab_id`) / `objects`
  (характеристики JSONB). Создание: labs/methods — admin, objects — editor+.
- Проекты: дерево `projects.parent_id` (произвольная вложенность), `code` UNIQUE
  (ИД проекта вводит пользователь), `is_ekn` (проект-ЕКН для серийной продукции).
- Заявки: `requests` (статус new/processing/completed, `owner_email`, `group_id`).
  Видимость: owner ИЛИ участник группы заявки; admin — все.
- Нумерация (см. дизайн-спеку §9): сервер присваивает глобальный по году NNN
  (`request_seq`), одна заявка = один NNN, каждый метод заявки получает два номера
  (`request_methods.customer_number` / `.lab_number`):
  - заказчику: `{projectID}-{NNN}/{yyyy}-{labID}-{methodID}`;
  - лаборатории: `{NNN}/{yyyy}-{methodID}`.
  NNN — простое значение сквозного счётчика по году (1, 2, 3, ...), без ширины/ведущих
  нулей; часть `{NNN}/{yyyy}` не зависит от проекта (единая сквозная нумерация по году).
  projectID = `projects.code` (вне проекта — `0`; пользовательский проект приоритетнее ЕКН).
  labID/methodID = `labs.code`/`methods.code`.
  Номера резервируются при создании/добавлении метода и не пересчитываются.
- Группы: `groups` + `group_members` (роль viewer/editor в группе). Владелец группы —
  создатель; управление участниками — владелец или admin.
- Синхронизация: `GET /api/lab/sync/pull` — полный слепок (фильтр по видимости на сервере);
  `POST /api/lab/sync/push` — только заявки (upsert, LWW по `updated_at`; `id=0` + `client_id` —
  создание, в ответе `created:[{client_id,request}]` с полной заявкой и номерами).
- Права: `lab_permissions(app, email, role)` + `lab_common_access(app, level)`.
  Роли: viewer(1) < editor(2) < admin(3).

## ЛИМС (расширение 2026-08-18, задеплоено)

Подсистема лаборатории: результаты испытаний (серии), испытатели, оборудование, привязка
сотрудников к лабораториям (`lab_members`), конфиги методов (formulas/classification/
chart_configs/input_parameters), расширенный ЖЦ заявки (received/processing), графики PNG,
протокол (HTML+docx), дашборд. Дизайн/план:
`docs/superpowers/specs/2026-08-18-sbe-lims-design.md` +
`docs/superpowers/plans/2026-08-18-sbe-lims-plan.md`.

- **Роуты** Go-сервиса с 2026-08-18 регистрируются **с префиксом `/api/lab/*` внутри сервиса**
  (раньше были голые пути `/health`, `/requests`...). Снаружи через Caddy (`handle` не режет
  префикс) API тот же: `https://epyur.fvds.ru/api/lab/...`. **Внутренняя проверка**:
  `docker compose exec lab wget -qO- http://localhost:3000/api/lab/health` (не `/health`!).
- **Роли**: существующие viewer/editor/admin + `lab_members.role` (lab_operator/lab_admin) —
  лабораторный скоуп заявок (видимость по сотруднику лаборатории). Ввод результатов — editor+
  (сервер валидирует принадлежность к лаборатории по `lab_members`).
- **DSL формул**: безопасный интерпретатор в `dsl.go` (арифметика, сравнения, if/else,
  агрегации avg/min/max/sum/count/median/std) — без exec/eval. Классификация (`classification.go`
  внутри dsl.go/results.go) + авто-статистика (стат-строка `is_statistical_row=true`,
  `calculation_type='auto_statistics'`) — генерируются при сохранении серии.
- **Графики**: `GET /requests/{id}/chart/{cfg_id}` → PNG (по `methods.chart_configs`).
- **Протокол/выписка/короткий вид** (2026-08-23, блоки rich-text заменили секции от
  2026-08-22): `POST /requests/{id}/protocol?template=ui|excerpt|protocol&format=html|full`
  → `{html, docx_base64}` (`template` по умолчанию `protocol`; `format=html` — не
  строить DOCX, для карточки результатов/предпросмотра). `methods.presentation`
  теперь `{blocks:[...]}` — блоки форматированного текста (абзацы/заголовки/списки/
  таблицы) с плейсхолдер-чипами вместо секций полей (см. История ниже,
  `parseMethodPresentation`). Отдельный JSON-эндпоинт `GET .../short-view` удалён —
  клиенты вставляют HTML из `protocol?template=ui&format=html`.

## Endpoints (`/api/lab/*`, внутри сервиса — те же пути)

| Метод | Путь | Роль |
|---|---|---|
| GET | `/health` | — |
| GET | `/labs`, `/methods`, `/objects` | viewer |
| POST/PATCH | `/labs`, `/labs/{id}` | superadmin (внешняя лаба требует `parent_lab_id` — внутренней) |
| POST | `/methods` | admin |
| POST/PATCH | `/objects`, `/objects/{id}` | editor (PATCH — 2026-08-24, characteristics заменяются целиком) |
| GET | `/projects` | viewer |
| POST | `/projects` | editor |
| PATCH | `/projects/{id}` | editor+/владелец |
| GET | `/requests` | viewer (видимость) |
| POST | `/requests` | editor |
| GET | `/requests/{id}` | viewer (видимость) |
| PATCH | `/requests/{id}` | editor+/владелец |
| POST | `/requests/{id}/status` | editor (new/received/processing/completed) — legacy, авторизация не ужесточена (тоже используется sbe-requests) |
| POST | `/requests/{id}/kanban-move` | editor (2026-08-24, Kanban-доска sbe-lims: `{status?, assigned_to?}`, `canApplyKanbanMove` — admin+ свободно; lab_operator/lab_admin этой лабы — своя карточка между received/processing/completed, либо самозабор `new`→`received` СЕБЕ) |
| GET | `/groups` | viewer (мои + где участник) |
| POST | `/groups` | editor |
| POST | `/groups/{id}/members` | владелец/admin |
| DELETE | `/groups/{id}/members/{email}` | владелец/admin |
| GET | `/sync/pull` | viewer |
| POST | `/sync/push` | editor |
| POST | `/file` | editor |
| GET | `/file?key=` | viewer |
| GET | `/permissions/me` | viewer |
| GET/POST | `/permissions` | admin |
| GET/POST | `/common-access` | admin |
| GET/POST | `/requests/{id}/results` | lab_operator+ / editor+ |
| GET | `/requests/{id}/results/aggregated` | viewer |
| POST | `/requests/{id}/results/{series}/calculate` | lab_operator+ (пересчёт DSL) |
| GET/POST | `/inventors` | viewer / editor+ |
| PATCH/DELETE | `/inventors/{id}` | editor+ |
| GET/POST | `/equipment` | viewer / editor+ |
| PATCH/DELETE | `/equipment/{id}` | editor+ |
| GET | `/lab-members` | admin (без `?lab_id=`) / любой участник лабы (с `?lab_id=`, 2026-08-24) |
| POST | `/lab-members` | admin |
| DELETE | `/lab-members/{lab_id}/{email}` | admin |
| PATCH | `/methods/{id}` | admin (formulas/classification/chart_configs/input_parameters/presentation/operator_form/lab_ids/description) |
| DELETE | `/methods/{id}` | admin |
| GET | `/requests/{id}/chart/{cfg_id}` | viewer |
| POST | `/requests/{id}/protocol?template=&format=` | editor+ |
| GET | `/requests/{id}/export.xlsx` | editor (2026-08-24, серии + агрегаты/статистика, `excelize`) |
| GET | `/dashboard?period=` | viewer |

## Полный сброс тестовых данных (2026-08-2x)

По явному запросу пользователя: вся БД `lab` обнулена (все 22 таблицы —
`TRUNCATE ... RESTART IDENTITY CASCADE`), включая справочники (`labs`/`methods`/
`method_labs`/`inventors`/`equipment`) — весь текущий набор считался тестовыми
заглушками (в т.ч. известный баг конфига `GG-M1` про `comb_length`/
`Comb_lenth_1..4` — снят вместе с методом). Дальше наполнение — реальными
определениями лабораторий/методов вручную.
- Бэкап перед сбросом: `/root/lab_backup_before_wipe_20260821_100030.sql` (pg_dump,
  на VDS, не в git — сервер не версионируется).
- После `TRUNCATE` — `docker compose restart lab` (без `--build`, код не менялся):
  `seedOwner()` переисполнился при старте, владелец (`LAB_OWNER_EMAIL=polishchuk@tn.ru`)
  — единственная оставшаяся строка, `role=superadmin`. Все остальные таблицы — 0 строк,
  все SERIAL-счётчики (id, `request_seq`) — с начала.
- Проверено: health ok, `lab_permissions` = 1 строка (владелец), `requests`/`labs`/
  `methods` = 0.

## PATCH /methods/{id}: добавлено description (2026-08-19)

По жалобе пользователя (sbe-lims): справочник методов в sbe-requests показывает
колонку «Описание», но в sbe-lims не было способа её заполнить/поменять — сервер
уже принимал `description` при создании (`POST /methods`), но `PATCH /methods/{id}`
его не поддерживал вовсе. `lims_refs.go handleUpdateMethodConfig`: добавлено
`Description *string` в тело запроса, `UPDATE methods SET ... description =
COALESCE($6, description), ...` (не перетирает, если поле не передано — как и
остальные необязательные поля этого хендлера). Задеплоено на VDS
(`docker compose up -d --build lab`, собралось с первого раза, health ok).

## Метод → несколько лабораторий (2026-08-19)

По явному требованию пользователя (после введения parent_lab_id, см. ниже): метод
может принадлежать **нескольким** лабораториям — старая единичная `methods.lab_id`
упразднена (колонка НЕ удалена из БД — тот же принцип, что `request_methods` в
декомпозиции 2026-08-18 — просто не пишется новым кодом), заменена таблицей связи
`method_labs (method_id, lab_id)`. Заявка теперь обязана явно зафиксировать ОДНУ
конкретную лабу из `method_labs` метода — новая колонка `requests.lab_id`, которая
одновременно заменяет и старую роль `methods.lab_id` (нумерация/видимость), и
отдельную `requests.external_lab_id` (упразднена по решению пользователя — была
независимым чекбоксом «внешняя лаборатория», не связанным с методом, что стало
избыточным/потенциально противоречивым после введения `method_labs`).

- `main.go`: `CREATE TABLE method_labs` (`method_id` CASCADE, `lab_id` без CASCADE —
  нельзя удалить лабу, пока она используется методом) + одноразовый backfill
  (`INSERT ... SELECT id, lab_id FROM methods WHERE lab_id IS NOT NULL ON CONFLICT DO
  NOTHING`, безопасен при каждом перезапуске); `ALTER TABLE requests ADD lab_id` +
  backfill из `methods.lab_id` через `method_id` (**критично** — без него все
  существующие заявки стали бы невидимы никому, кроме app-admin, при переходе на
  `requests.lab_id`-ориентированную видимость).
- `references.go`: `Method.LabIDs []int64` (замена `LabID`); новые хелперы
  `loadMethodLabsMap`/`validateLabIDs`; `handleCreateMethod` — `lab_ids` (минимум
  один, каждый должен существовать), транзакция INSERT methods + N×INSERT
  method_labs; `handleListMethods` подгружает `LabIDs` одним доп. запросом (не N+1).
- `lims_refs.go` (`handleUpdateMethodConfig`): опциональный `lab_ids` в PATCH — если
  передан, полностью заменяет набор лабораторий метода (DELETE+INSERT в той же
  транзакции, что и формулы/классификация).
- `requests.go`: `Request.LabID` (замена `ExternalLabID`); новый `loadMethodLabRow`
  (проверяет пару method_id+lab_id по `method_labs`, возвращает оба кода для
  `buildNumbers`); `handleCreateRequest` — тело `methods:[{method_id,lab_id}]`
  (замена `method_ids`+`external_lab_id`), валидирует каждую пару; `requestVisible`/
  `visibleRequestsQuery` резолвят видимость напрямую через `requests.lab_id`
  (сильно проще — больше не нужен JOIN через `methods`).
- `lims_refs.go` (`requestLabID`, гейт результатов/графиков/протокола): тоже
  напрямую через `requests.lab_id` вместо `methods.lab_id`.
- `sync.go`: `PushRequest.LabID`; `pushCreate` валидирует пару через
  `loadMethodLabRow`, как `handleCreateRequest`.
- `email_ingest.go`: новая обязательная переменная `LAB_MAIL_LAB_ID` (лаба,
  которой принадлежит почтовый ящик) — воркер не может интерактивно выбрать лабу
  метода из нескольких, поэтому конфиг фиксирует её явно; `applyApplicationEmail`
  проверяет пару (`method_id` из `LAB_MAIL_METHOD_MAP`, `LAB_MAIL_LAB_ID`) через
  `method_labs`, пишет `requests.lab_id`.
- `dashboard.go`: фильтр `?lab_id=` переведён с `methods.lab_id` (потерял смысл —
  метод не привязан к одной лабе) на `requests.lab_id` (проще и точнее — это и есть
  лаба, которая реально выполняет конкретную заявку). Эндпоинт всё ещё не используется
  никаким плагином (дашборд вынесен), фикс — на будущее.
- **Задеплоено на VDS 2026-08-19** (`docker compose up -d --build lab`, собралось с
  первого раза). **Проверено после деплоя** (`GET /methods`/`GET /requests`/`GET
  /labs` с временным JWT владельца): `method_labs` забэкфиллен (3 метода → лаба 1);
  все 12 существующих заявок получили `lab_id=1`, номера/файлы не пострадали;
  `GET /methods` отдаёт `lab_ids`. Временный скрипт генерации JWT удалён сразу после
  проверки (как всегда в этой сессии — не оставляем credential-скрипты).

## Внешние лаборатории привязаны к внутренним (2026-08-19)

По явному требованию пользователя: внешняя лаборатория (`labs.type='external'`) **не
может существовать самостоятельно** — обязана быть привязана к внутренней при
создании. При этом внешняя лаба **может иметь свои методы, которых нет у внутренней**
(расширяет её возможности, например подрядчик с уникальным оборудованием) — методы не
ограничены по типу лаборатории владельца. Заявки по таким методам должны «попадать»
(быть видны/управляемы) сотрудникам внутренней лабы, потому что у внешней организации
нет пользователей этой системы.

- `main.go`: миграция `labs.parent_lab_id BIGINT REFERENCES labs(id)` (NULL у
  внутренних; у ранее созданных внешних без родителя — переходное состояние, не
  мигрировано автоматически, см. ниже про `FAER`).
- `references.go`: `Lab.ParentLabID`; `handleCreateLab` — `type=external` **требует**
  `parent_lab_id` (400 без него) и проверяет, что он ссылается на существующую лабу с
  `type=internal` (400 иначе); `type=internal` с непустым `parent_lab_id` — тоже 400
  (внутренняя не может иметь родителя). Парная `handleUpdateLab` (`PATCH /labs/{id}`,
  частичный, та же валидация комбинации type+parent_lab_id — эффективное значение
  берётся из тела запроса там, где поле передано, иначе из текущей строки в БД;
  дополнительно отдельно проверяет `parent_lab_id != id`, лаба не может быть родителем
  себе). `handleListLabs` — не-admin ветка теперь видит
  свои лабы (`lab_members`) **плюс** внешние лабы, чей `parent_lab_id` — одна из своих
  (иначе сотрудник внутренней лабы не увидел бы «свою» внешнюю в `GET /labs`, а она
  нужна, например, в select «внешняя лаборатория» sbe-requests).
- `sync.go` (`handlePull`): `parent_lab_id` добавлен в SELECT/скан (сам список labs в
  pull остаётся нефильтрованным по видимости — общий справочник, как раньше).
- **Резолвинг видимости через родителя** (ключевая механика — «заявки на внешнюю лабу
  попадают к внутренней»): везде, где раньше проверялось `lab_members.lab_id = m.lab_id`
  напрямую, теперь `lab_members.lab_id = COALESCE(l.parent_lab_id, l.id)` (JOIN labs l
  ON l.id = m.lab_id) — для внутренней лабы (`parent_lab_id IS NULL`) не меняет
  поведения (резолвится в себя), для внешней — резолвится в родителя:
  - `requests.go`: `requestVisible`, `visibleRequestsQuery` (список/детали заявок).
  - `lims_refs.go`: `requestLabID` (использует `requireLabAccess`/`requireLabRead` —
    гейт записи/чтения результатов, графиков, протокола).
- `lims_refs.go` (`handleSetLabMember`): `lab_members` можно завести только для
  **внутренней** лабы (400 иначе) — у внешней своих участников нет по определению,
  видимость всегда идёт через родителя, а не через несуществующих `lab_members` самой
  внешней лабы.
- Внешняя лаба `FAER` (id=2) изначально была оставлена без `parent_lab_id` (по
  явному решению пользователя — «оставить как есть, разберётесь вручную позже»);
  **обновление**: после появления UI редактирования лаб (см. sbe-lims/AGENTS.md,
  раздел «Настройки») пользователь сам проставил `parent_lab_id = 1` (GG) через
  форму — подтверждено `GET /labs` после деплоя. Также найдена и **не исправлена**
  мимоходом тестовая заявка
  (id=9) с `external_lab_id=1` (указывает на `GG`, внутреннюю — явно тестовые данные
  прошлых сессий, не тронуто).
- Локальной Go-сборки нет — компиляция проверяется сборкой в Docker при деплое (как
  всегда для этого сервиса).

## Справочники: правка/удаление (2026-08-19)

По жалобе из sbe-lims («нет возможности редактировать/удалять испытателей/
оборудование, нет удаления методов, не видно как создавать методы») — CRUD
справочников был неполным (только Create+Read; `sbe-lims/AGENTS.md` уже
документировал их как «CRUD editor+», что было **не соответствием документации
реальности**, а не просто пробелом UI).

- `lims_refs.go`: `handleUpdateInventor`/`handleDeleteInventor`,
  `handleUpdateEquipment`/`handleDeleteEquipment` (частичный PATCH — только
  переданные поля; DELETE). Новые хелперы `isForeignKeyViolation`/`isUniqueViolation`
  (`pgconn.PgError.Code` 23503/23505) — чтобы удаление занятого справочника отвечало
  понятной 409 (`"испытатель используется в результатах испытаний..."` и т.п.), а не
  голой 500.
- `references.go`: `handleDeleteMethod` — тем же паттерном (23503 → 409 «метод
  используется в заявках или справочниках»); на практике метод, по которому уже
  подавались заявки, удалить не получится — это ожидаемо, не баг.
- `main.go`: маршруты `PATCH/DELETE /inventors/{id}`, `PATCH/DELETE /equipment/{id}`
  (editor+, тот же уровень, что create), `DELETE /methods/{id}` (admin, тот же
  уровень, что create методов).
- Создание методов (`POST /methods`) уже существовало на сервере — не хватало формы
  в плагине (см. запись sbe-lims ниже).

## Приём заявок/результатов по почте (email-ingestion)

Фоновый воркер внутри сервиса (`email_ingest.go`), опрашивает IMAP-ящик лаборатории
каждые `LAB_MAIL_POLL_INTERVAL_SECONDS`. Дизайн/план:
`docs/superpowers/specs/2026-08-19-lab-email-ingestion-design.md` +
`docs/superpowers/plans/2026-08-19-lab-email-ingestion-plan.md`. Заменяет часть
функциональности десктопной ЛИМС (`LIMS_LPI/service/email_processor.py`,
`email_monitor.py`) для бесшовного перехода — **без переноса исторических писем**
(backfill решается отдельно, позже, по решению пользователя).

- Папка `LPITrack` (от `lpi@tracker.tn.ru`) — заявки (`type: "application"`) →
  создаёт заявку с `external_id` = число после префикса `LPIZAYAVKINAPRO-`.
- Папки `Comb`/`Flam`/`FlamProp` — результаты от ручных Яндекс.Форм
  (`type: "result"`), поля письма передаются в `values` как есть (без маппинга —
  имена уже совпадают с параметрами formulas/classification метода). Письма с
  `mesure_data` (самосыл сырых сигналов прибора) — пропускаются (`skipped_signal`,
  не MVP, решение пользователя).
- **Синонимы атрибута** (2026-08-21, конфигуратор методов): помимо глобальных
  `canonicalFieldNames`/`knownRawFields` ниже, каждый атрибут метода может иметь
  свой `synonyms: string[]` (raw-имя → id атрибута) — проверяется первым
  (`resolveResultKey`), чтобы конфигуратор мог назвать атрибут как удобно, не
  оглядываясь на legacy-имя поля из письма.
- Дедуп по `Message-ID` (или, если письмо его не прислало, псевдо-ключ
  `uid:{folder}:{uid}`) — таблица `email_ingest_processed` (аудит: `raw_payload`,
  `request_id`, `error`). Результат, пришедший раньше заявки — персистентный буфер
  `email_ingest_pending_results` (retry каждый цикл, до 20 попыток, потом не удаляется,
  но не спамит лог).
- `results.go`: `handleCreateResult` разбит — тело вынесено в переиспользуемую
  `saveResultSeries(ctx, requestID, methodID, inventorID, seriesNum, values,
  photoBefore, photoAfter)`, вызывается и HTTP-хендлером, и воркером напрямую (без HTTP).
- **Безопасно по умолчанию**: без `LAB_MAIL_ENABLED=true` воркер не стартует (лог
  `email ingest: disabled`), остальной сервис работает как раньше — можно задеплоить
  код без риска, включить отдельным шагом с реальными учётными данными.
- Конфиг: `LAB_MAIL_IMAP_SERVER`/`LAB_MAIL_LOGIN`/`LAB_MAIL_PASSWORD`/
  `LAB_MAIL_POLL_INTERVAL_SECONDS` (default 120)/`LAB_MAIL_METHOD_MAP` (JSON
  `{"method1":1,...}` — код метода из письма → `methods.id`)/`LAB_MAIL_ENABLED`.
- Зависимости: `github.com/emersion/go-imap` (client, read-only, `BODY.PEEK[]` —
  письма не помечаются прочитанными) + `golang.org/x/text/encoding/charmap`
  (декодирование `windows-1251`/`koi8-r` — письма трекера/форм не всегда utf-8).
  **Намеренно не добавлены в `go.mod` руками** (нет возможности проверить актуальные
  версионные теги без сети на этой машине) — `GOFLAGS=-mod=mod` в Dockerfile
  разрешает `go build` самостоятельно резолвить и добавить новые `require` при
  сборке на сервере (там сеть есть; тот же механизм, которым в это го.mod попали
  `jackc/pgx`/`golang-jwt`, см. «Статистика ошибок и отступлений» ниже).
- ⚠️ **Известная проблема конфигурации методов, не исправлена** (найдена при
  подготовке дизайн-спеки, требует правки **до** включения в проде): конфиг метода
  `GG-M1` (`formulas`/`classification`) ссылается на параметр `comb_length`
  (единственное значение), а реальные письма Яндекс.Формы шлют `Comb_lenth_1`..`4`
  (4 отдельных замера, опечатка «lenth» — в самом названии поля формы). Пока не
  исправлено — `comb_grade`/`mass_loss_grade` не посчитаются для реальных писем
  этого метода (структура сохранится верно, просто агрегация не найдёт параметр).
- **Реализовано и задеплоено (safe-режим, 2026-08-19)**: код написан (`email_ingest.go`
  новый, `main.go`/`results.go` правки), локальной компиляции не было (тулчейн не
  установлен) — первая проверка компиляции прошла прямо в Docker-сборке на сервере,
  зелёная с первого раза. `go.mod` не трогали руками — `GOFLAGS=-mod=mod` сам нашёл и
  добавил `github.com/emersion/go-imap v1.2.1` и `golang.org/x/text v0.18.0`
  (транзитивно `go-sasl`). Задеплоено на VDS (`docker compose up -d --build lab`),
  `health` → `{"status":"ok"}`, лог подтверждает `email ingest: disabled`
  (`LAB_MAIL_ENABLED` не выставлен) — сервис работает как раньше, воркер не стартовал.
  **Следующий шаг** (по отдельному подтверждению): выставить `LAB_MAIL_*` в
  `/opt/mailers/.env` с реальными учётными данными `lpitn@yandex.ru` и
  `LAB_MAIL_ENABLED=true`, пересоздать контейнер, пройти E2E из спеки. Также перед этим
  шагом нужно поправить конфиг метода `GG-M1` (см. предупреждение выше о
  `comb_length`/`Comb_lenth_1..4`).

## S3 (rclone)

- Бакет `sbe-doc`, remote `firstvds_doc` из env (`S3_ENDPOINT`/`S3_ACCESS_KEY`/`S3_SECRET_KEY`).
- Ключи: `lab/{uuid}/main-{name}`.
- НЕ использовать `mailers-backup` (ротация 7 дней).
- История aws-sdk-go-v2 → rclone — см. documents-service/AGENTS.md (SDK вешал сервер).

## Конфиг (env)

`DATABASE_URL`, `PORT`, `JWT_SECRET`, `LAB_APP_ID` (default `lab`), `LAB_APP_NAME`,
`LAB_OWNER_EMAIL`, `LAB_SERVICE_SECRET`, `AUTH_SERVICE_URL`,
`S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET` (default `sbe-doc`).
Email-ingestion (см. раздел выше, все опциональны — без них воркер не стартует):
`LAB_MAIL_ENABLED`, `LAB_MAIL_IMAP_SERVER`, `LAB_MAIL_LOGIN`, `LAB_MAIL_PASSWORD`,
`LAB_MAIL_POLL_INTERVAL_SECONDS`, `LAB_MAIL_METHOD_MAP`.

## Сборка / проверка

```
docker compose up -d --build lab        # на сервере
docker compose logs lab --tail 20
docker compose exec lab wget -qO- http://localhost:3000/api/lab/health   # внутренняя проверка
```

## История

- **2026-08-24 — Kanban-доска «Очередь лаборатории» (sbe-lims): новый `kanban.go`,
  `assigned_to`/`completed_at` на `requests`, `lab-members` открыт для чтения любому
  участнику лабы.** ЗАДЕПЛОЕНО на VDS (`docker compose up -d --build lab`), миграция
  подтверждена (`\d requests`), health `{"status":"ok"}`.
  - **Модель**: `requests.assigned_to TEXT NOT NULL DEFAULT ''` (email испытателя,
    `lab_members.email`) + `requests.completed_at TIMESTAMPTZ` (момент перехода в
    `completed`, НЕ `updated_at` — основа 10-рабочедневного окна показа в колонке
    "Завершённые" на клиенте). Оба поддерживаются во ВСЕХ трёх местах записи
    `requests.status` — новом `handleKanbanMove`, легаси `handleSetRequestStatus`
    (`requests.go`, использует и sbe-requests) и `pushUpdate` (`sync.go`,
    offline-синхронизация) — иначе окно работало бы непредсказуемо в зависимости от
    того, кто/как поменял статус.
  - **`POST /requests/{id}/kanban-move`** (новый файл `kanban.go`) — единственная
    точка входа для Kanban-доски (и drag-and-drop, и контролов в детали заявки).
    `canApplyKanbanMove` (чистая функция, юнит-тестируется без БД):
    1. Глобальная роль admin/superadmin ("руководитель лабы" в терминах задачи) —
       разрешено всё.
    2. `lab_operator`/`lab_admin` ИМЕННО этой лабы ("испытатель") — либо забирает
       СЕБЕ неназначенную заявку из `new` (`oldAssignedTo==''`, `newAssignedTo==
       actorEmail`, `newStatus=='received'` — самозабор, уточнено пользователем
       отдельным вопросом уже после согласования исходного текста задачи, где
       назначение было только прерогативой руководителя), либо двигает СВОЮ уже
       назначенную карточку между `received`/`processing`/`completed` (не может
       переоткрыть завершённую, не может тронуть чужую/неназначенную никаким иным
       путём, не может сам поменять `assigned_to`).
    3. Остальные — 403.
    Авторизация легаси `POST /requests/{id}/status` СОЗНАТЕЛЬНО не ужесточена (её же
    использует sbe-requests для смены статуса владельцем заявки — риск регрессии).
  - **`GET /lab-members?lab_id=`** — без параметра поведение не изменилось (полный
    список, только admin, используется «Настройками» sbe-lims); с параметром —
    ростер ОДНОЙ лабы, доступен ЛЮБОМУ её участнику (`lab_operator`/`lab_admin`/
    `lab_auditor`), не только admin — испытателю нужно видеть состав всех ячеек
    колонок 2/3, не только свою.
  - Тесты: `kanban_test.go` — 10 юнит-тестов `canApplyKanbanMove`/
    `normalizeKanbanTarget` (свобода руководителя, запрет переназначения, движение
    своей карточки, запрет чужой/неназначенной, запрет забрать из "новых"/
    переоткрыть завершённую, самозабор разрешён/запрет забрать для другого,
    неучастник лабы отказан). `go build`/`vet`/`test` — все зелёные, без регрессий
    существующих тестов.

- **2026-08-24 — 3 доработки протокола по запросу пользователя: центрирование/
  границы таблиц, статические таблицы + выравнивание, верхние/нижние индексы; плюс
  фикс битых DOCX и новый Excel-экспорт.** Часть общего списка из 6 задач (остальные
  3 — целиком на стороне плагинов, см. `sbe-lims`/`sbe-requests` `AGENTS.md`).
  ЗАДЕПЛОЕНО на VDS, health ok.
  1. **Таблицы результатов — центрирование + границы.** HTML (`protocolHTML`,
     `<style>`) — добавлен `text-align:center` на `td,th` (границы уже были).
     DOCX (`renderTableDocx`) — новый `docxCenteredParagraph`, применён к
     заголовку и данным каждой ячейки.
  2. **Статические таблицы в тексте + выравнивание абзаца.** `RichNode.Align`
     (""/"center"/"right"/"justify" — OOXML называет "justify" → "both",
     `docxAlignPPr`/`htmlAlignAttr`) на paragraph/heading. Новый
     `RichNode.Type="static_table"` с `Rows [][][]InlineNode` (строка → колонка →
     inline-содержимое ячейки) — пользовательская таблица вне данных серий,
     отдельный рендер `renderStaticTableHTML`/`renderStaticTableDocx` (переиспользует
     `renderInlineHTML`/`renderInlineDocxRuns` на ячейку, НЕ переиспользует
     `renderTableHTML`/`renderTableDocx` — те привязаны к `TableColumn`/сериям).
  3. **Верхние/нижние индексы.** `InlineNode.Sup`/`.Sub bool` — в HTML оборачивание
     в `<sup>/<sub>` (`renderInlineHTML`), в DOCX — `<w:vertAlign w:val=
     "superscript|subscript"/>` в `<w:rPr>` прогона (`renderInlineDocxRuns`).
  4. **Баг (найден пользователем при проверке): скачанный DOCX не открывался в
     Word, "ошибка открытия/повреждённый файл".** Причина — в hand-rolled
     `protocolDocx` отсутствовала ОБЯЗАТЕЛЬНАЯ OPC-часть пакета `_rels/.rels`
     (Relationship Type=officeDocument → `word/document.xml`) — без неё Word
     считает пакет невалидным независимо от корректности самого
     `word/document.xml`. Добавлены `_rels/.rels` + `word/_rels/document.xml.rels`.
     Новый тест `TestProtocolDocxIsValidOOXMLPackage` — распаковывает zip и
     XML-валидирует КАЖДУЮ часть пакета (не только `word/document.xml`
     строковым сравнением, как все прежние тесты, — тот класс проверки, который
     И ДОЛЖЕН БЫЛ поймать этот баг раньше).
  5. **`GET /requests/{id}/export.xlsx`** (новый `export.go`) — таблица всех данных
     заявки: лист "Серии" (строка на серию, столбцы по `input_parameters`) + лист
     "Агрегаты и статистика" (по решению пользователя — не только серии, полный
     набор). Зависимость `github.com/xuri/excelize/v2 v2.9.0` (тот же паттерн, что
     `agent-service/files.go`), добавлена в `go.mod`/`go.sum`.
  - DSL/лексер и модель `dsl.go`/`results.go` не менялись в этом раунде (см. запись
    ниже про живое тестирование ГВ — она про другой набор находок).
  - `go build`/`vet`/`test` — зелёные, включая новые тесты центрирования/
    static_table/align/sup-sub/OOXML-валидности.

- **2026-08-23/24 — живое тестирование ГВ вскрыло 3 независимых бага классификации/
  формул + добавлен `PATCH /objects/{id}`.** Все три ЗАДЕПЛОЕНЫ на VDS
  (`docker compose up -d --build lab`), health `{"status":"ok"}` после каждого.
  1. **Классификация никогда не видела агрегированные входы** —
     `applyClassification` вызывалась только с values ОДНОЙ серии; subject вида
     "agg_flam_flow_density → flammability_group" (оба уровня "aggregated") не
     мог совпасть НИКОГДА, ни при одном вызове, — агрегированное значение просто
     не существует в per-series values. Исправлено: два прохода —
     `applyRuleToSubjects(..., aggregatedIDs, wantAggregated)` — обычный (per-series)
     и новый `applyAggregatedClassification` (вызывается из `applyAggregatedFormulas`
     после вычисления формул уровня "aggregated", видит уже посчитанные значения
     того же прохода). Тесты: `TestApplyRuleToSubjectsAggregatedOutputRequiresAggregatedPass`.
  2. **DSL не понимал двойные кавычки** — только одинарные; формула
     `target_group_compliance` (написана с `"..."`, привычка из большинства
     языков) валилась с "неожиданный символ '\"'" на каждом расчёте. Лексер
     (`dsl.go`) теперь принимает оба вида кавычек, закрывающая — та же, что
     открывающая. Тест: `TestDSLDoubleQuotedStrings`,
     `TestDSLRealGVTargetGroupComplianceFormula` (сама формула из БД).
  3. **`deriveFormulasFromAttributes` подхватывала устаревшие `.formula`/
     `.aggregation`** — при переключении `fill_method` через конфигуратор
     (напр. "Формула" → "Классификация") эти поля не чистились, только
     скрывались из UI; сервер ориентировался лишь на "поле не пусто", игнорируя
     fill_method, поэтому старая формула продолжала молча исполняться наравне с
     новым правилом классификации — и, ошибаясь на кавычках (баг 2), обрывала
     ВЕСЬ `applyAggregatedFormulas` целиком, включая исправные формулы того же
     метода (`agg_flam_flow_density`). Теперь оба case в свиче проверяют
     `fill_method` явно. Тесты:
     `TestDeriveFormulasFromAttributesIgnoresStaleFormulaAfterFillMethodSwitch`,
     `TestDeriveFormulasFromAttributesIgnoresStaleAggregationAfterClassificationSwitch`.
  4. **`PATCH /api/lab/objects/{id}`** (новый, `references.go`/`main.go`) — у
     объектов исследования не было НИКАКОГО способа обновления, только
     `POST /objects` (создание). sbe-requests при редактировании заявки с уже
     привязанным объектом был вынужден каждый раз создавать новый объект — старый
     оставался ни к чему не привязанным навсегда (найдено на живом кейсе:
     целевой показатель осел в новом объекте, заявка продолжала смотреть на
     старый — классификация не находила целевой показатель, хотя пользователь
     его ввёл). `characteristics` заменяется ЦЕЛИКОМ, не мержится (клиент всегда
     собирает объект из состояния формы, не присылает частичный diff).
  - Все найдено пользователем через живое тестирование ГВ с реальными email-
    данными (см. запись выше про системные атрибуты) — не гипотетические баги.

- **2026-08-23 — системные атрибуты заявки** (испытатель/даты/условия среды при
  испытании — универсальные для ЛЮБОГО метода, не per-method `MethodAttribute`).
  ЗАДЕПЛОЕНО на VDS (`docker compose up -d --build lab`), новые колонки/FK
  подтверждены `\d requests` на сервере, health `{"status":"ok"}`.
  - Найдено при проверке синонимов метода ГВ по запросу пользователя (загрузить
    пример данных из `mail_records.jsonl`, папка Flam): `amb_temp`/`amb_pres`/
    `amb_moist`/`exp_date` были заведены СВОИМИ атрибутами метода ГВ, хотя эти поля
    несёт КАЖДОЕ письмо-результат (Comb/Flam/FlamProp, см. `json_attr.md`)
    независимо от метода; `flam_inventor`/`flam_rep_date`/`flam_date_material_in`
    канонизировались (`canonicalFieldNames`) в `inventor`/`report_date`/
    `samples_in_date`, но у ГВ вообще не было атрибута-адресата — данные попадали
    бы в `values` как "сирота". Решение пользователя: "инвентор он системный...
    заведи эти атрибуты как универсальные, так же как например «Наименование
    материала»" — полное правило и три поверхности (плейсхолдер/справка
    конфигуратора/форма испытателя) см. sbe-lims `AGENTS.md`, «Правило: системные
    атрибуты».
  - `main.go`: новые колонки `requests.inventor_id` (FK → `inventors`, `ON DELETE
    SET NULL`), `report_date`/`samples_in_date`/`exp_date`/`amb_temp`/`amb_pres`/
    `amb_moist` (все TEXT, nullable).
  - `requests.go`: `Request` — 7 новых полей. Заодно устранена реальная угроза
    расхождения (тот же класс риска, что раньше нашли у `presentation` —
    `loadRequest`/`visibleRequestsQuery` × 2 держали ТРИ независимые копии одного
    и того же списка колонок) — вынесены в `requestColumnsSQL` + `scanRequestRow`,
    единая точка для всех трёх SELECT.
  - `email_ingest.go`: `systemRequestFields` — множество канонических имён (после
    `resolveResultKey`), которые относятся к ЗАЯВКЕ, не к методу; `applyResultPayload`
    теперь разделяет payload на `values` (атрибуты метода) и `sysFields`, последние
    уходят в новый `applyRequestSystemFields` (UPDATE `requests.*`, а не JSONB
    серии). `inventor` резолвится в `inventor_id` ТОЧНЫМ совпадением имени по
    `inventors.name` — не найден → предупреждение в лог, `inventor_id` не пишется
    (не создаём испытателя автоматически, справочник ведёт лаборатория через
    `/inventors`).
  - `protocol.go`: `resolveSystemPlaceholder` — 7 новых case; `placeholderCtx`
    получил `inventorName` (резолвится ОДИН раз в `buildProtocol`, не на каждый
    плейсхолдер).
  - Тесты: `TestSystemRequestFieldsCoversUniversalConcepts` (email_ingest_test.go)
    — сверяет ВСЕ raw-имена из реальных писем (Comb/Flam/FlamProp) с
    `systemRequestFields`, отдельно проверяет, что `additional_info`/`substrate`/
    `mounting_method` (те же ИМЕНА полей у разных методов, но метод-специфичное
    СОДЕРЖАНИЕ) туда не попали; `TestResolveSystemPlaceholder`/`testCtx` —
    покрытие 7 новых плейсхолдеров.
  - `go build`/`go vet`/`go test` — чисто до и после деплоя.

- **2026-08-23 — блоки rich-text (заменили секции от 2026-08-22), рендер HTML+DOCX
  по AST, колонка `series_no`. ЗАДЕПЛОЕНО на VDS (`docker compose up -d --build lab`),
  health `{"status":"ok"}`.**
  - Пользователь отверг секции как неподходящую модель: "редактор не функциональный...
    нужно сделать редактор в котором пользователь будет выбирать не показатели...
    а блоки информации" — эталон (реальный полный протокол с реквизитами/
    юридическим футером/описаниями, не только таблицами). Согласовано: визуальный
    (WYSIWYG) редактор с чипами-плейсхолдерами; DOCX поддерживает форматирование
    (жирный/заголовки) сразу.
  - `results.go`: `DocumentBlock{ID,Title,Content []RichNode,ChartID,ShowInUI/Excerpt/Protocol}`,
    `RichNode` (paragraph/heading/bullet_list/table), `InlineNode` (text с bold/italic,
    или placeholder: `Source` system|attribute, `AttributeID`, `Agg` — обязателен для
    атрибута уровня experiment вне таблицы). `TableColumn.Kind` — `""`/`"attribute"`
    (обычная) или `"series_no"` (номер серии по порядку; раньше жёстко prepend-илась
    сервером первой колонкой без права пользователя её убрать/переместить —
    убрано в пользу обычной колонки в списке).
  - **`parseMethodPresentation`** — легаси-фолбэк теперь на ДВА шага назад без
    SQL-миграции: плоский `{"fields":[...]}` (до 2026-08-21) и секции
    `{"sections":[...]}` (2026-08-22, жили меньше суток) оба конвертируются в
    блоки на лету; обе ветки ПРЕПЕНДЯТ колонку `{Kind:"series_no"}` первой —
    сохраняет старое implicit-поведение для легаси-данных, которые сами не могли
    её задать. Все 3 места разбора (`loadMethodConfig`, `handleListMethods`,
    `sync.go` pull) продолжают звать эту единую функцию.
  - `protocol.go` — полностью переписан: `renderInlineHTML`/`renderNodeHTML`/
    `renderTableHTML` и DOCX-аналоги рендерят по AST; заголовки в DOCX — прямое
    форматирование прогона (`<w:b/>`+`<w:sz>`), НЕ именованный стиль (нет
    `styles.xml`); bullet_list — префикс "• " текстом (без `<w:numPr>`/
    `numbering.xml`, упрощение v1). **Регресс от секций**: секции для
    `template=protocol` молча добирали несконфигурированные атрибуты алфавитным
    хвостом "Прочие показатели" (гарантия "протокол не теряет данные") — у блоков
    этой гарантии нет, документ полностью авторский. Не восстановлено намеренно.
  - `handleProtocol` — новый параметр `?format=html|full` (по умолчанию `full`):
    `html` не строит DOCX (быстрее для карточки результатов/предпросмотра).
    **`handleShortView`/`buildShortView` (`GET .../short-view`) удалены** — клиенты
    (sbe-lims, sbe-requests) теперь вставляют `html` из `protocol?template=ui&format=html`.
  - `buildProtocol` расширен: `SELECT` объекта теперь тянет `characteristics` (для
    системных плейсхолдеров `batch_number`/`sample_id`/`thickness`).
  - `protocol_test.go` — переписан под rich-node рендер (~15 тестов: резолв
    плейсхолдеров, HTML/DOCX рендер узлов, легаси-фолбэк, `series_no` колонка).
  - `main.go` — роут `GET .../short-view` удалён. `lims_refs.go` — текст ошибки
    валидации presentation "sections" → "blocks".
  - `go build`/`go vet`/`go test` — чисто. Деплой — заливка `main.go`,
    `lims_refs.go`, `results.go`, `protocol.go`, `protocol_test.go` (pscp) +
    `docker compose up -d --build lab` на VDS, health `{"status":"ok"}` (2026-08-23,
    в тот же день, что и код — предыдущая секционная версия НЕ была задеплоена
    вообще, минуя промежуточный релиз).

- **2026-08-22 — секции представления (заменили плоский `presentation.fields`),
  3 вида вывода (ui/excerpt/protocol), короткий вид (JSON), форма испытателя
  (`operator_form`). НЕ ЗАДЕПЛОЕНО на момент записи — локально build/vet/test
  чисто, деплой отдельным подтверждённым шагом.**
  - Жалоба пользователя: "все атрибуты формируют одну длинную сводную таблицу" —
    эталон структуры (присланные пользователем десктопные отчёты `30244.html`/
    `30402.html`) — пронумерованные тематические подразделы, каждый со своей
    мини-таблицей/графиком/выводом.
  - `results.go`: `PresentationField` — новое поле `Role` ("table"|"summary") и
    третий флаг `ShowInExcerpt` (было `ShowInUI`/`ShowInProtocol`); новые
    `PresentationChartRef`, `PresentationSection` (`ID`/`Title`/`Fields`/`Charts`);
    `MethodPresentation.Sections` заменяет `.Fields`. Новые `OperatorFormField`/
    `MethodOperatorForm` + поле `MethodConfig.OperatorForm`.
  - **`parseMethodPresentation(raw []byte) MethodPresentation`** — единая точка
    разбора JSONB `presentation`, с легаси-фолбэком: старая плоская форма
    (`{"fields":[...]}`, до этой правки) оборачивается в одну секцию "Показатели"
    на лету, без SQL-миграции данных. Критично: ДО этой правки сырой JSON
    `presentation` парсили **три независимых места** (`loadMethodConfig` в
    results.go, `handleListMethods` через `unmarshalPresentation` в references.go,
    `sync.go`'s pull-хендлер) — если поправить только одно, остальные продолжили
    бы отдавать старую форму. Все три теперь вызывают `parseMethodPresentation`.
  - `protocol.go`: `filterSectionsForTemplate`-аналог — `buildProtocolSections`
    (использует `showInKind`/`buildSectionFields`) строит секции для запрошенного
    вида; для `template=protocol` несконфигурированные ключи серий/статистики
    добираются алфавитным хвостом "Прочие показатели" (сохраняет старую гарантию
    "протокол не теряет данные молча"); `ui`/`excerpt` показывают только явно
    сконфигурированное (по решению пользователя — "пользователь должен
    определять, что включить в выписку"). `protocolHTML`/`protocolDocx`
    переписаны на рендер по секциям (заголовок + мини-таблица + резюме-список +
    график секции) вместо одной сводной таблицы + отдельных блоков stats/aggregated.
  - `handleProtocol` читает `?template=ui|excerpt|protocol` (по умолчанию
    `protocol` — совместимость со старыми клиентами).
  - Новый `handleShortView`/`buildShortView` (`GET .../short-view`) — та же
    группировка, что и `template=ui`, но отдаёт структурированный JSON
    (`shortViewSection`/`shortViewTable`/`shortViewColumn`), не HTML — общая точка
    для карточки результатов sbe-lims и нового read-only блока в sbe-requests
    (раньше sbe-requests вообще не показывал результаты метода).
  - `main.go`: миграция `ALTER TABLE methods ADD COLUMN IF NOT EXISTS
    operator_form JSONB NOT NULL DEFAULT '{}'`; новый роут `GET
    /api/lab/requests/{id}/short-view` (`requirePerm("viewer")` + `requireLabRead`
    внутри хендлера, тот же паттерн, что `/results`/`/chart`).
  - `lims_refs.go`: `handleUpdateMethodConfig` — новое поле `OperatorForm
    *json.RawMessage` в PATCH-запросе, валидация JSON-объекта, COALESCE-параметр
    в SQL (тот же паттерн, что `Presentation`).
  - `protocol_test.go` — переписан под секции: `TestBuildSectionFieldsRespectsOrder`/
    `SkipsFieldWithoutData`/`SummaryRole`/`PerKindVisibility`,
    `TestBuildProtocolSectionsFallbackAlphabetical`/`NoFallbackForUIOrExcerpt`/
    `DropsEmptySection`, `TestParseMethodPresentationLegacyFallback`/`NewShape`/`Empty`
    (не удалялись старые тесты, переписаны 1:1 под новые функции — см. память
    keep-test-and-check-scripts).
  - `go build`/`go vet`/`go test ./...` — чисто.

- **2026-08-22 — правила классификации переписаны в третий раз за сессию:
  неявная левая часть сравнения + динамическая таблица subjects; ЗАДЕПЛОЕНО.
  ⚠️ Пользователь явно подтвердил: сейчас проверяется УДОБСТВО/ЛОГИКА UI, а
  НЕ реальное функционирование расчётов/классификации — то проверят отдельно,
  после того как эргономика будет закончена. Не удивляться дальнейшим правкам
  этой модели.**
  - `applyClassification` → `applyRuleToSubjects`: правило теперь = ОДНА схема
    условий (`branches`), применяемая ПО ОТДЕЛЬНОСТИ к каждой строке
    `subjects` (`input_attribute_id` → `output_attribute_id`) — значение
    input-атрибута подставляется как НЕЯВНАЯ левая часть во все `clauses`,
    сама схема условий конкретных атрибутов не упоминает. Заменяет версию с
    `output_name`/один атрибут-источник на всё правило (см. запись 2026-08-22
    ниже про branches/clauses) — та по факту не позволяла оценивать несколько
    атрибутов одной и той же логикой, чего пользователю и было нужно.
  - Убрана агрегация по сериям ЦЕЛИКОМ (по прямой правке пользователя — «всегда
    не сравнивать»): `classifyCtx.seriesValues`/`agg` и мёртвый код
    (`aggregateNumeric`, `collectParamValues`) удалены. `applyClassification`
    больше не принимает `seriesValues` — вызовы из `saveResultSeries`/
    `handleCalculateSeries` обновлены.
  - `evalClause`/`evalBranch`/`evaluateBranches` принимают `subjectValue`
    явным параметром вместо чтения `clause["left"]`.
  - `results_test.go` переписан (13 тестов; новый `TestApplyRuleToSubjectsMultiple`
    — ключевой сценарий: разные subjects с разными значениями input-атрибута
    получают разные grade в СВОИХ output-атрибутах). `go build`/`go vet`/
    `go test`/`gofmt -l` — чисто. **Задеплоено на VDS**, health OK.

- **2026-08-22 — email-импорт: разрешение названия продукта по ЕКН вместо
  заглушки "Без названия"; разовый backfill; ЗАДЕПЛОЕНО, backfill выполнен.**
  - Баг, найденный пользователем на реальной импортированной заявке: ЕКН указан
    и валиден, но название продукта — "Без названия" навсегда, потому что
    `applyRequestEmail` (`email_ingest.go`) при отсутствии `product_name` в
    письме ставила заглушку и НИКОГДА не обращалась к справочнику ЕКН
    (ekn-service), даже когда сам номер был в справочнике.
  - **`ekn_client.go` (новый файл)**: `lookupEknProductName(ctx, ekn)` — GET
    `http://ekn:3000/api/ekn/product/{ekn}` (тот же docker-network `internal`,
    без изменений docker-compose.yml). Авторизация — служебный JWT, подписанный
    тем же `JWT_SECRET` (общий для всех сервисов стека, см. `jwt.go
    mintServiceJWT`), с `app_id="ekn"` и email существующего владельца
    приложения `ekn` (`polishchuk@tn.ru`, admin в `ekn_permissions`) — по
    согласованию с пользователем НЕ заводили отдельную служебную запись, чтобы
    не трогать БД ekn лишний раз. Отказоустойчиво: пустой результат при любой
    ошибке/недоступности ekn-service, заявка всё равно создаётся (с заглушкой).
  - `email_ingest.go`: перед заглушкой `missingNamePlaceholder` ("Без названия")
    теперь пробует `lookupEknProductName`, если `ekn` задан, а `product_name`
    из письма — нет. Работает для ПИСЕМ, ПОСТУПАЮЩИХ С ЭТОГО МОМЕНТА.
  - **Разовый backfill уже импортированных заявок**: `POST
    /api/lab/admin/backfill-ekn-names` (admin) — сканирует `objects` с именем
    `''`/`missingNamePlaceholder` и непустым `characteristics.ekn`, резолвит по
    тому же `lookupEknProductName`, обновляет `objects.name` и `requests.title`
    (только там, где title тоже был заглушкой). Идемпотентна (повторный вызов
    не находит уже исправленные строки), НЕ вызывается автоматически при
    старте (не хотим сетевых вызовов к ekn-service в цепочке запуска — `lab`
    не зависит от `ekn` в docker-compose). **Выполнен вручную сразу после
    деплоя**: `{"checked":1,"fixed":1}` — единственная затронутая заявка
    проверена в БД, `objects.name`/`requests.title` теперь корректны, заглушек
    среди ЕКН-заявок не осталось.
  - Смежный фронтенд-баг в `sbe-requests` (локальный кэш объектов не обновлялся
    после `createObject()`, из-за чего сохранение заявки визуально "стирало"
    только что введённые ЕКН/номер партии/название) — исправлен там же, см.
    `sbe-requests/AGENTS.md` (2026-08-22).
  - `go build`/`go vet`/`go test`/`gofmt -l` — чисто. **Задеплоено на VDS**
    (`docker compose up -d --build lab`), health OK.

- **2026-08-22 — правила классификации переработаны в модель branches/clauses;
  запрет терминов «лучше»/«хуже»; закрыт скрытый PATCH-баг; ЗАДЕПЛОЕНО на VDS.**
  - `results.go`: `applyClassification` переписан с единого `conditions[]` (один
    фиксированный атрибут-источник на правило) на составную модель —
    `ClassificationRule{output_name, aggregation_rule, branches[]}`,
    `ClassificationBranch{clauses?, join?, grade}`, `ClassificationClause{left:
    Operand, operator, right: Operand}` (оба операнда — атрибут/литерал/целевой
    показатель, симметрично). Причина: предыдущая единая модель по факту
    сводила пороговое/булево/соответствие к одному шаблону, но пользователь
    указал на реальную потребность — составные условия с И/ИЛИ, как в Excel
    `IF`/`AND`/`OR`, со словом «Если» явно видимым в форме клиента (не только в
    подписи-подсказке). Новые функции: `evaluateBranches`/`evalBranch`/
    `evalClause`/`classifyCtx.resolveOperand`/`resolveAttributeValue`.
  - **Явное указание пользователя: термины «лучше»/«хуже» (best/worst,
    лучший/худший) больше НЕ используются нигде в проекте** — оценочные категории
    неприменимы к объективной оценке результатов испытаний. Заменено: DSL
    `worst_grade`/`best_grade` → `min_grade`/`max_grade` (`dsl.go`); правило
    классификации `aggregation_rule`: `best`/`worst` → `min`/`max`, плюс новое
    явное значение **`none`** (по умолчанию) — НЕ сравнивать между сериями, брать
    значение текущей записи (одной серии/агрегата) как есть, без свода по
    сериям; `avg`/`min`/`max` — сводить числовые значения атрибута по всем сериям.
  - **Попутно найден и исправлен скрытый баг** `handleUpdateMethodConfig`
    (`lims_refs.go`): PATCH безусловно перезатирал `formulas`/`classification`/
    `chart_configs`/`input_parameters`/`presentation` до `"[]"`/`"{}"`, если поле
    не пришло в запросе (не COALESCE, в отличие от уже частичного `description`) —
    риск потери данных при любом действительно частичном PATCH. Теперь честный
    частичный PATCH для всех полей; добавлена поддержка `determinable_indicators`
    (позволяет клиенту редактировать порядок показателей после создания метода).
  - На машине разработки команда `go` резолвилась в несвязанный npm-пакет
    (`gocli/go`, легитимный, но не Go) — переустановлен реальный Go
    (`winget install --id GoLang.Go`, 1.26.7); из-за этого несколько прогонов
    `go build`/`go test` в начале дня были фиктивными (гонял npm-пакет, не Go) —
    после исправления все прогоны настоящие.
  - Тесты: `results_test.go` полностью переписан под branches/clauses (13
    тестов: порядок веток, И/ИЛИ, атрибут-к-атрибуту, ранговая инверсия,
    соответствие целевому показателю, пропущенный операнд); `dsl_test.go`
    переименован `TestDSLWorstBestGrade` → `TestDSLMinMaxGrade`.
    `go build`/`go vet`/`go test`/`gofmt -l` — все чистые.
  - **Задеплоено на VDS** (`docker compose up -d --build lab`, несколько раз по
    ходу правок; последний — с моделью branches/clauses и терминологией),
    health `{"status":"ok"}`. Заодно выяснилось, что весь конфигуратор методов
    (запись 2026-08-21 ниже) фактически НИКОГДА не деплоился до этого дня — этим
    объяснялось «атрибуты/правила не сохраняются» при тестировании клиентом:
    старый живой сервер вообще не отдавал `input_parameters`/`classification` в
    `GET`/`sync/pull`.

- **2026-08-21 — конфигуратор методов: атрибуты/формулы/классификация/представление
  (блоки 1-3 из запроса пользователя); ИИ-черновик из стандарта; синонимы для
  email-импорта. НЕ ЗАДЕПЛОЕНО — код готов и протестирован локально (`go build`/
  `go test` зелёные), деплой на VDS ждёт отдельного подтверждения пользователя.**
  - **Блокирующий баг (исправлен первым)**: `GET /methods` и `GET /sync/pull`
    (`references.go`/`sync.go`) вообще не выбирали `formulas`/`classification`/
    `chart_configs`/`input_parameters` — конфигуратор в клиенте всегда открывался
    пустым, `PATCH` безусловно перезатирал реальные данные до `"[]"`. `Method`
    struct дополнена всеми 4 полями + новым `presentation`.
  - **`input_parameters` → структурированные атрибуты метода** (`MethodAttribute`:
    id/name/data_type/fill_method/level/formula?/aggregation?/synonyms?, см. ниже) —
    переиспользована существующая пустая JSONB-колонка, без миграции схемы.
    `deriveFormulasFromAttributes` (`lims_refs.go`) при каждом `PATCH` с
    `input_parameters` **перестраивает `formulas[]` целиком** из атрибутов —
    расчётные атрибуты передают формулу как есть, агрегированные без своей формулы
    получают автоформулу `{avg|min|max}(source)` (тот же исполнитель, что уже
    работал для агрегированных формул).
  - **DSL (`dsl.go`) — точечные built-in**, не общий скриптинг (согласовано с
    пользователем): многоаргументные `avg(a,b,c)`/`min`/`max`/`sum` (среднее
    нескольких атрибутов ОДНОЙ серии, не по сериям); `worst_grade`/`best_grade`
    (сравнение по порядку `determinable_indicators` метода); `interpolate(x, xs,
    ys)` (линейная интерполяция по атрибутам-массивам, калибровочные таблицы);
    `agg_where(fn, value_attr, cond_attr, value)` (условная агрегация по сериям).
  - **Классификация — новый `rule_type: "compliance"`** (`results.go`): цель —
    `objects.characteristics.target_indicators[methodId]` заявки (уже реализовано
    в sbe-requests, не нужна отдельная таблица целевых классов продукта, как в
    legacy); сравнение по индексу в `determinable_indicators`. Существующий
    `threshold`-классификатор проверен — уже сортирует пороги по `value` ↑ и берёт
    первый подходящий (соответствует confirmed-семантике из
    `LIMS_LPI/service/classification_service.py`), фикс не потребовался.
  - **`presentation` (новая JSONB-колонка, блок 3)** — порядок/подписи/видимость
    столбцов таблицы результатов (UI) и протокола: `{fields: [{attribute_id, label?,
    show_in_ui, show_in_protocol}]}`. **Найден и исправлен попутно баг**:
    `protocol.go` строил список колонок через `keys := map[string]bool{}` — обход
    `map` в Go недетерминирован, порядок столбцов протокола был случайным от
    рендера к рендеру. `orderedColumns` теперь берёт порядок из `presentation.fields`
    (отфильтрованных по `show_in_protocol`), остальные найденные ключи — алфавитным
    хвостом (детерминированный фолбэк для методов без presentation — фикс
    недетерминированности касается ВСЕХ методов, не только сконфигурированных).
    Фото-атрибуты (`data_type=photo`) — `<img>` превью в HTML-протоколе; в DOCX —
    текст/ссылка (свой DOCX-writer не умеет медиа-части/relationships, встраивание
    изображений — отдельная, более крупная задача, осознанно не в этой фазе).
  - **Синонимы атрибута** (`MethodAttribute.Synonyms []string`) — конфигуратор может
    назвать атрибут как удобно (например по смыслу из текста стандарта), не
    оглядываясь на то, как поле называется в legacy email-письмах десктопной ЛИМС.
    `resolveResultKey` (`email_ingest.go`, вынесена в чистую функцию с тестами) —
    приоритет: synonyms конкретного атрибута метода (настроены пользователем) >
    глобальный `canonicalFieldNames` > `knownRawFields` (оставлен как есть) >
    неизвестное (лог). Найдено на практике: ИИ-черновик атрибутов из ГОСТ 30402-96
    назвал длительность воспламенения `flame_duration`, а email-импорт уже ждёт
    `flam_time` (`canonicalFieldNames`/`knownRawFields`, метод-специфичных карт там
    нет) — без synonyms результаты из писем не попадали бы в нужный атрибут.
  - Тесты: `dsl_test.go` (built-in функции, включая реальный пример ГГ `ap00022`:
    `avg_comb_length=100.0`, `delta_by_mass≈20.077`), `results_test.go`
    (threshold-сортировка, compliance), `protocol_test.go` (детерминированный
    порядок колонок + фолбэк), `lims_refs_test.go` (`deriveFormulasFromAttributes`),
    `email_ingest_test.go` (`resolveResultKey`, приоритет synonyms).
  - `go build ./...`/`go vet ./...`/`go test ./...` — все зелёные. **Деплоя на VDS не
    было** — ждёт явного подтверждения пользователя (см. пометку в заголовке записи).

- **2026-08-21 — перенос исторических заявок LPITrack в проект «Old»; дедуп объектов;
  режим CLI `import-lpitrack-history`; uppercase кода проекта.**
  - Новый файл `import_history.go` + ветка в `main.go` (`./mailers-lab
    import-lpitrack-history -file=<path.json> [-dry-run]`) — постоянный CLI-режим
    (сервер не поднимается), не одноразовый выброс-скрипт. Переносит исторические
    заявки из почтового трекера `LPITrack` (подготовка — `plugins/scripts/
    prepare_lpitrack_import.py`, сверяет `mail_records.jsonl` с desktop-базой
    `LIMS_LPI/config/lims.db`) с нумерацией по РЕАЛЬНОМУ году подачи, а не текущему
    году сервера — поэтому не через `POST /requests`/`sync/push` (оба всегда берут
    `time.Now().UTC().Year()`), а прямой вызов `nextSeq`/`buildNumbers`
    (`requests.go`) с историческим годом, по прецеденту `rollout.go` (явные
    `created_at`/`updated_at` в INSERT).
  - **Багфикс, найден на собственном `-dry-run`**: `nextSeq` пишет в `request_seq`
    через `s.pool` напрямую, не через переданную транзакцию — откат транзакции при
    `-dry-run` НЕ отменял продвижение счётчика (реальный побочный эффект без единой
    вставленной строки заявки). Добавлен `nextSeqTx(ctx, tx, year)` — тот же SQL,
    но через `tx`, используется вместо `nextSeq` в `importOneLpitrackRequest`.
    Испорченные счётчики (`request_seq` 2025/2026) сброшены (`DELETE`, безопасно —
    `requests` была пуста).
  - Перенесено **469 заявок** (сверка почты (546 уникальных `external_id`) с
    desktop-базой (565 заявок, метод/статус уже разрешён там) — 449 есть в обоих
    источниках, 20 только в почте с известным методом; 77 только в почте без метода
    вовсе и 116 только в desktop без надёжной даты подачи (там `created_at` —
    дата внутреннего бэкфилла desktop-приложения, не реальная дата) — пропущены по
    решению пользователя, не гадать). Все — в проект `OLD` (`code=OLD`, уже создан
    пользователем), `status='completed'` (жёстко, по решению пользователя),
    `external_id` = число из письма без префикса `LPIZAYAVKINAPRO-` — благодаря
    этому `applyApplicationEmail` (`email_ingest.go:329-334`, проверка
    `SELECT id FROM requests WHERE external_id=$1`) автоматически не создаст дублей
    для этих заявок, когда включат живой приём почты — отдельного шага
    «пометить письма как обработанные» не нужно. `pg_dump`-бэкап перед запуском.
  - **Дедуп справочника `objects`**: 469 → 134 (335 удалено, столько же заявок
    `requests.object_id` перепривязано на канон). Группировка строго по
    `trim(name)` (регистр важен — по решению пользователя, включая объекты
    «без названия», которые тоже объединены — 129 заявок теперь у одного объекта).
    Канон группы — `MIN(id)` (самая ранняя запись; импорт шёл в хронологическом
    порядке, поэтому `id` коррелирует с реальной датой). Проверено: единственная
    FK на `objects` — `requests.object_id` (`pg_constraint` запрос), после дедупа
    0 висящих ссылок. `pg_dump`-бэкап перед запуском. Компенсирующая фича в UI —
    кнопка «→ Заявки» в справочнике объектов обоих плагинов (см. `sbe-requests/
    AGENTS.md`, `sbe-lims/AGENTS.md`, v0.1.12 у обоих).
  - `projects.go`: `handleCreateProject`/`handleUpdateProject` — код проекта теперь
    всегда приводится к верхнему регистру (`strings.ToUpper(strings.TrimSpace(...))`)
    перед валидацией/записью. Не меняли в `ensureEknProject` (EKN-коды сравниваются
    без учёта регистра нигде специально — риск разъехаться с уже созданными
    EKN-проектами, если один вызов апперкейснет, а поиск по `code=$1` — нет).
  - Проверено: `go build ./...` OK, деплой на VDS (`docker compose up -d --build
    lab`), health OK, оба `-dry-run` (до и после багфикса) чисто откатились,
    реальный прогон подтверждён точечными запросами (количество/статус/годы/
    диапазон номеров, отсутствие дублей имён, 0 висящих `object_id`).

- **2026-08-19 — модель ролей: superadmin, lab_auditor, видимость `/labs` (реализовано,
  задеплоено, E2E).** По плану наполнения sbe-lims (роли, ранее только спроектированы
  в sbe-lims/AGENTS.md «Запланировано»). Объём **ограничен намеренно** — делегированные
  полномочия `lab_admin` внутри своей лабы (без app-level admin: добавление участников,
  правка методов своей лабы) **не реализованы** — это отдельная задача авторизации
  по ресурсу, не просто сравнение рангов; `lab_admin` сегодня по факту равен
  `lab_operator` с точки зрения сервера (только запись результатов, не более).
  - `jwt.go`: `roleRank` — `superadmin`(4) выше `admin`(3).
  - `permissions.go`: `handleSetPermission` принимает `superadmin`; назначать/снимать
    `superadmin` может только действующий `superadmin` (иначе admin мог бы повысить
    себя через тот же admin-only роут — явная защита от самоповышения).
  - `register.go`: `seedOwner` теперь `DO UPDATE` (не `DO NOTHING`) — `LAB_OWNER_EMAIL`
    всегда становится/остаётся `superadmin` при каждом старте сервиса.
  - `references.go`: `handleListLabs` — admin/superadmin видят все лабы; остальные —
    только те, где есть строка в `lab_members` (`WHERE id IN (SELECT lab_id FROM
    lab_members WHERE email = $1)`).
  - `main.go`: `POST /labs` — `admin` → `superadmin` (создание лабораторий теперь
    только у superadmin, по дизайн-таблице ролей; ранее мог любой admin).
  - `lims_refs.go`: `lab_members.role` принимает новую `lab_auditor`; новая
    `requireLabRead` (=`requireLabAccess` + `lab_auditor`) — для ЧТЕНИЯ (результаты/
    графики/протокол); `requireLabAccess` остаётся write-only (lab_operator/lab_admin/
    app-admin+), auditor туда не допускается.
  - `results.go` (`handleListResults`), `charts.go` (`handleChart`), `protocol.go`
    (`handleProtocol`) переведены на `requireLabRead`; `handleCreateResult`/
    `handleCalculateSeries` (запись) — на прежнем `requireLabAccess`.
  - ⚠️ **Не исправлено, задокументировано как известный пробел**: `handleSetRequestStatus`
    (`POST /requests/{id}/status`) гейтится только `requestVisible` (видимость), а не
    write-правом — то есть теоретически смену статуса может вызвать любой, кому заявка
    просто *видна* (включая participant группы), а не только lab_operator/lab_admin/
    владелец. Не трогал: используется также sbe-requests для смены статуса владельцем
    заявки, и правильная семантика (кто именно должен мочь менять статус — только
    владелец? только сотрудник лабы? оба?) не была явно зафиксирована ни в одной спеке —
    менять сейчас означало бы гадать и рисковать регрессией в обоих потребителях.
  - **E2E** (тестовые `lab_members`/`lab_permissions` строки, удалены после): auditor
    видит только свою лабу в `GET /labs`, читает результаты (200), не может их писать
    (403); обычный admin не может создать лабу (403) и не может назначить себе/другому
    superadmin (403); superadmin создаёт лабу (200); владелец подтверждён как
    `superadmin` в БД после деплоя.

- **2026-08-19 — E2E ЛИМС-расширения (графики/протокол/дашборд ОК; найден пробел
  в aggregated_results).** Проверено вживую через curl с реальным JWT (не только по БД,
  как раньше) на существующей тест-заявке 12 (метод GG-M1, chart_config `c1`, formulas
  с `mass_loss`/`comb_grade`): `GET .../chart/c1` → 200, `image/png`, 2453 байт;
  `POST .../protocol` → 200, корректный HTML (шапка + таблица серий + статистика) и
  валидный `docx_base64` (PK-сигнатура zip); `GET /dashboard?period=month` → 200,
  агрегаты по статусу/методу совпадают с БД (total 12). **Найден пробел**:
  `GET .../results/aggregated` всегда возвращает `[]` — не потому что не протестировано,
  а потому что **ничего не пишет в `aggregated_results`** (таблица только читается —
  `results.go:550`, `protocol.go:123` — и мигрируется в `rollout.go`, INSERT нигде нет).
  Причина глубже: `applyFormulas` (`results.go`) применяет ВСЕ формулы метода одинаково,
  к текущей серии, **игнорируя `apply_level`** — формула с `apply_level: "aggregated"`
  (пример: `comb_grade` метода GG-M1, `if avg(comb_length) > 150 then 'A' else 'B'`,
  задумана как одна оценка на заявку) на практике считается для каждой серии отдельно
  и пишется в `values` серии, а не в `aggregated_results`. `protocol.go`'s
  `loadAggregatedRow` вызывается с проигнорированной ошибкой (`agg, _ :=`) — деградирует
  тихо, протокол просто не показывает агрегатный блок, не падает.
  **Исправлено и задеплоено в этой же сессии**: `applyFormulas` теперь пропускает
  формулы с `apply_level == "aggregated"`; новая `applyAggregatedFormulas(ctx, requestID,
  methodID)` считает их по всем сериям заявки+метода (`buildFormulaEnv` с пустым
  «текущим» — только `SeriesParams`) и пишет результат в `aggregated_results`
  (delete+insert по `calculation_type='formula_aggregated'`, та же схема, что
  `recomputeStatistics` использует для стат-строки). Вызывается из `handleCreateResult`
  и `handleCalculateSeries` рядом с `recomputeStatistics`. Задеплоено на VDS
  (`docker compose up -d --build lab`). **E2E повторно подтверждён** на заявке 12:
  `comb_grade` больше не попадает в `values` серии (проверено `GET /results`), зато
  появляется в `GET /results/aggregated` (`{"comb_grade":"B"}`,
  `calculation_type: "formula_aggregated"`) и в HTML протокола (`POST /protocol`).
  Стартовая заявка 12/серия 1 очищена от старого некорректного `comb_grade` в `values`
  (`UPDATE measurement_results SET values = values - 'comb_grade' ...`) перед повторным
  пересчётом — остальные тестовые данные (1-15) не трогались.

- **2026-08-19 — видимость заявок по lab-скоупу (пробел найден, исправлено, задеплоено, E2E).**
  При проверке документации перед наполнением sbe-lims найден пробел: specification.md
  утверждал, что сотрудник лаборатории видит заявки своих методов (по `lab_members`),
  но `visibleRequestsQuery`/`loadVisibleRequests` и `requestVisible` (`requests.go`)
  реально фильтровали только по owner/группе/admin — без единого упоминания
  `lab_members`. `requireLabAccess` (`lims_refs.go`) проверяет lab-скоуп, но только на
  уровне результатов/графиков/протокола конкретной заявки — не на уровне списка/детали
  заявки, т.е. сотрудник лаборатории, не будучи владельцем/участником группы, не мог
  увидеть заявку своего метода вовсе (при том что мог бы работать с её результатами,
  если бы узнал id откуда-то ещё). **Правки**: `requestVisible` и `visibleRequestsQuery`
  (обе — не-admin ветка) дополнены условием `method_id IN (SELECT m.id FROM methods m
  JOIN lab_members lm ON lm.lab_id = m.lab_id WHERE lm.email = $1)` (OR к существующим
  owner/группа). Дашборд (`dashboard.go`) не трогали — он агрегирует по всем заявкам без
  фильтра по пользователю, это отдельное, ранее принятое поведение. Локальной Go-сборки
  нет — компиляция проверяется сборкой в Docker при деплое.
  **Задеплоено на VDS** (`docker compose up -d --build lab`, health `{"status":"ok"}`).
  **E2E пройден**: тестовый `lab_members(lab_id=1, role=lab_operator)` без владения/группы
  → видит 12 заявок лаборатории 1 в `GET /requests` (200) и конкретную заявку в
  `GET /requests/{id}` (200); пользователь без какой-либо связи с заявками → `GET /requests`
  возвращает 0, `GET /requests/{id}` → 403 (видимость по умолчанию не потекла). Тестовые
  данные удалены (`DELETE FROM lab_members WHERE email = 'e2e-labop@test.local'`).

- **2026-08-19 — `external_id` для заявок переходного периода миграции (реализовано, задеплоено).**
  По решению пользователя: перед переносом заявок из десктопной ЛИМС (`LIMS_LPI`, см. её
  AGENTS.md для контекста ревью) заведена сущность `requests.external_id` (TEXT, default '',
  индекс по непустым) — хранит номер из legacy email-трекера (`LPITrack`, вид
  `"LPIZAYAVKINAPRO-<N>"`, простая сквозная последовательность без года/проекта/лаборатории —
  несовместима с форматом `{projectID}-{NNN}/{yyyy}-{labID}-{methodID}`). У новых заявок,
  созданных в этой системе, поле остаётся пустым; заполняется только при импорте legacy-заявок.
  Проведена живая индексация почты `lpitn@yandex.ru` (IMAP, read-only, `Message-ID`-safe) —
  подтверждено: одно email-заявка из трекера = один метод (`ID: "LPIZAYAVKINAPRO-727"` и т.п.),
  что ложится на текущую модель «1 заявка = 1 метод» без конфликта.
  **Правки**: `main.go` (миграция `ALTER TABLE requests ADD COLUMN external_id` + индекс);
  `requests.go` (`Request.ExternalID`, `loadRequest`/`visibleRequestsQuery`/`loadVisibleRequests`
  читают поле; `handleCreateRequest`/`handleUpdateRequest` принимают и пишут); `sync.go`
  (`PushRequest.ExternalID`, `pushCreate`/`pushUpdate` читают и пишут). Локальной Go-сборки
  для проверки нет (тулчейн не установлен на машине) — компиляция проверяется сборкой в
  Docker на сервере при деплое (как всегда для этого сервиса). **Задеплоено на VDS
  2026-08-19**: `docker compose up -d --build lab`, health `{"status":"ok"}`, колонка
  `external_id` проверена (`text`, default `''::text`).

- **2026-08-19 — видимость проектов по группе (реализовано, задеплоено, E2E).**
  Дизайн/план: `docs/superpowers/specs/2026-08-19-sbe-requests-project-group-visibility-design.md`
  + `docs/superpowers/plans/2026-08-19-sbe-requests-project-group-visibility-plan.md`.
  `projects.group_id BIGINT REFERENCES groups(id)` (миграция). `Project.GroupID`.
  `handleCreateProject`/`handleUpdateProject` принимают `group_id` (0 → NULL через
  `nullableID`; валидация существования группы → 400 `group not found`).
  Новый `loadVisibleProjects(ctx, email)`: видимость = публичный (`group_id IS NULL`) ∨
  владелец ∨ admin (effectiveRole) ∨ член группы (`group_members`); **цепочка предков**
  видимых проектов добавляется рекурсивно (дерево не рвётся). Применён в
  `handleListProjects` и `handlePull`. Правки на операциях не менялись (create editor,
  update владелец/admin).
  Залито на VDS `/opt/mailers/lab-service/` (main.go, projects.go, sync.go, md5 = локальным),
  контейнер `lab` пересобран и пересоздан. **E2E пройден**: viewer (член группы 1) видит
  проект группы 1 + публичные, не видит группу 2; stray без роли — только публичные;
  admin — все; цепочка предков (видимый подпроект группы 1 тянет скрытого родителя группы 2);
  PATCH group_id (привязать/отвязать) меняет видимость; pull фильтрует так же; 400 на
  несуществующую группу. E2E-данные удалены (проекты GRP-*, пользователь testgrp).
  ⚠️ Первая сборка упала на отсутствующем `context` в projects.go (класс багов groups.go
  2026-08-18) — добавлен импорт, сборка зелёная.

- **2026-08-18 — добавление методов к заявке: под-заявка с тем же NNN (реализовано, E2E).**
  `PushRequest` += `parent_id`. В `handlePush` новый `resolveSeq`: если `parent_id` указан,
  `reuseParentNumber` возвращает (number_seq, number_year) родителя — **только** для
  владельца/admin и **только** при статусе родителя `new`; иначе (нет/не владелец/не new) —
  выделяется новый NNN (заявка не теряется). Задеплоено (sync.go md5 `17051df5...`),
  контейнер `lab` пересобран.
  **Фикс сканирования**: `loadRequest`/`visibleRequestsQuery` падали на `NULL object_id`
  (`can't scan NULL into *int64`) — заявка с `object_id IS NULL` создавалась, но ответ
  push был `created:[]` (loadRequest падал после коммита). Добавлен
  `COALESCE(object_id, 0)` в три SELECT (requests.go).
  **E2E пройден**: push c `parent_id`=2 (NNN 2/2026, status new) → создана заявка с тем же
  `2/2026` и новым методом, в ответе `created[].request` с номерами; негатив — parent
  в `processing` → новый NNN; E2E-строки удалены.

- **2026-08-18 — ДИЗАЙН: декомпозиция заявки на под-заявки по методам (реализовано и задеплоено).**
  Дизайн: `docs/superpowers/specs/2026-08-18-sbe-requests-per-method-design.md`.
  1 заявка = 1 метод: в `requests` добавлены `method_id`/`customer_number`/`lab_number`,
  `request_methods` упраздняется (таблица остаётся пустой для совместимости, данные
  перенесены); группа с общим NNN — только через одинаковые `number_seq`+`number_year`
  (без шапки); `POST /requests` и `pushCreate` — один запрос → N под-заявок с общим NNN;
  `pushCreate` группирует новые заявки по `group_key` (сервер выделяет один NNN на группу,
  отвечает `created[]` с каждым client_id); `pushUpdate` больше не синхронизирует методы
  (метод фиксируется при создании); потребители `request_methods` (`charts.go`,
  `dashboard.go`, `lims_refs.go`, `protocol.go`) переведены на `requests.method_id`.
  **Раскатка данных** — `rollout.go`, запускается при старте после миграций (идемпотентно,
  детект по наличию строк в `request_methods`): одно-методные заявки получают
  метод/номера в свою строку (`UPDATE` без смены id); мульти-методные разбиваются на N
  под-заявок (`INSERT` с копией полей + одинаковые number_seq/number_year), файлы
  копируются во все под-заявки, `measurement_results`/`aggregated_results` переносятся по
  `(request_id, method_id)`, исходная заявка удаляется (каскад). `request_seq` не трогается.
  **Задеплоено на VDS 2026-08-18**: залиты `requests.go`/`sync.go`/`rollout.go`/`charts.go`/
  `dashboard.go`/`lims_refs.go`/`protocol.go`/`main.go`, контейнер `lab` пересобран
  (`docker compose up -d --build lab`), образ собрался с первого раза (Go-компиляция в Docker).
  **Rollout выполнен**: 10 legacy-заявок раскатаны («Заявка 1» → под-заявки 12/13, NNN=1;
  «проверка работы» → 14/15, NNN=7), `request_methods` пуст.
  **E2E пройден (нет JWT — токен сгенерирован из `.env` через openssl)**: push группы
  из 2 под-заявок (`group_key`) → созданы id 16/17 с общим NNN=14 (`14/2026-GG-M1`/
  `14/2026-GG-M2`), номера правильные; независимая смена статуса (16→processing, 17→new);
  ЛИМС-результат сохранён только на своей под-заявке 17. E2E-данные удалены из БД.
  ⚠️ **Плагин sbe-requests обновлён** (типы/БД-миграция кэша/push с `method_id`+`group_key`/
  view: таблица под-заявок отдельными строками, метод в карточке фиксирован); tsc/build OK.
  **Номер заявки больше не привязан к стеку методов — следующий NNN после rollout = 15.**

- **2026-08-18 — ЛИМС: результаты/DSL/графики/протокол/дашборд (план 2026-08-18-sbe-lims-plan.md).**
  Добавлены файлы: `dsl.go` (безопасный DSL-интерпретатор формул + классификация),
  `results.go` (CRUD серий результатов + авто-статистика + расчёт), `lims_refs.go`
  (справочники испытателей/оборудования/сотрудников лабораторий), `charts.go` (PNG по
  `methods.chart_configs`), `protocol.go` (HTML+docx), `dashboard.go` (агрегации).
  `main.go`: миграции новых таблиц (`inventors`, `inventor_methods`, `equipment`,
  `method_equipment`, `lab_members`, `measurement_results`, `aggregated_results`,
  `methods.formulas/classification/chart_configs/input_parameters`) + **все роуты переведены
  на префикс `/api/lab/*` внутри сервиса** + ЛИМС-endpoints. Статусы заявки расширены
  (received). `requests.go`/`sync.go` обновлены (поле status + received).
  **Задеплоено на VDS** (все файлы залиты, md5 совпадают с локальными), образ `mailers-lab`
  собран, контейнер пересоздан, `registerApp: lab registered`.
  **E2E частичный** (подтверждено по БД): создана серия измерений (request 1) + авто-статистика
  (`is_statistical_row=t`, `auto_statistics`), испытатели (2), оборудование (1), lab_members (1),
  статус заявки `processing`. `aggregated_results` пуст — **агрегаты/графики/протокол/дашборд
  полным E2E не покрыты** (требуется проверка с JWT).
  ⚠️ **Схема внутренних путей изменилась**: раньше `/health`/`/requests` (без префикса),
  теперь `/api/lab/*` — старая команда проверки (`wget localhost:3000/health`) даёт 404.

- **2026-08-18 — адаптация формы заявки под практику (план 2026-08-18-sbe-requests-form-adaptation-plan.md).**
  Миграции: `labs.type` (internal/external), `methods.determinable_indicators` (JSONB),
  `requests.priority`/`test_purpose`/`external_lab_id`/`ekn`. `requests.go`: новые поля в
  create/update/load/pull/push + **автопроект-ЕКН** (`ensureEknProject`): при `ekn` без проекта
  создаётся/переиспользуется проект с `code=ekn` (is_ekn=true). `references.go`:
  `Lab.type`, `Method.determinable_indicators` (list/create). sync.go: pull/push с новыми
  полями + автопроект в `pushCreate`. Фикс: после создания автопроекта в tx `loadProjectInfo`
  через пул не видел незакоммиченный проект (500) → `pi.code = ekn` напрямую.
  Залит на VDS `/opt/mailers/lab-service/`, контейнер `lab` пересобран. **E2E пройден**:
  внешняя лаба (type=external), метод с показателями, объект с ЕКН (batch_number/ekn_snapshot),
  автопроект 068863 (is_ekn=true) + переиспользование при повторной заявке, приоритет/цель/
  external_lab_id/ekn в заявке, экспериментальный образец без ЕКН, pull с новыми полями.

- **2026-08-18 — push create возвращает созданную заявку (фикс номера в плагине).**
  Баг: клиент пушил новую заявку с положительным локальным id (`Date.now()`), сервер
  трактовал `p.ID > 0` как UPDATE несуществующей → 0 строк, `pushCreate` не вызывался,
  номер не присваивался. Фикс: `PushRequest` += `client_id`; `pushCreate` возвращает
  полную созданную заявку (`*Request`); `handlePush` отвечает
  `{"inserted", "updated", "created": [{client_id, request}]}` (пустой массив при отсутствии
  созданий). Залит на VDS `/opt/mailers/lab-service/sync.go` (md5 `184744af8c4220d4c287129b1ea8e16f`),
  контейнер `lab` пересобран и пересоздан (recreate OK, контейнер Running).

- **2026-08-18 — создание (Этап 1 плана 2026-08-17-sbe-requests-lab-service-plan.md).**
  Сервис создан зеркалом documents-service (jwt.go/register.go/permissions.go/s3.go скопированы
  с адаптацией: роли viewer/editor/admin, таблицы lab_*). Миграции всех таблиц спеки §2.
  Нумерация заявок по §9 аддендума спеки (request_seq, глобальный по году NNN,
  customer_number/lab_number в request_methods). docker-compose: `lab-db` + `lab`,
  Caddy `/api/lab/*` → `lab:3000` (до `/api/*`); `.env.example`: `LAB_*`.
  auth-service `seedApps` → seed приложения `lab`.
  E2E на сервере — не проведён (деплой не выполнялся).
- **2026-08-18 — уточнение нумерации:** `{NNN}/{yyyy}` — единая сквозная нумерация по году,
  **не зависит от проекта**. Убраны ширина/ведущие нули (`%04d`/`%06d` для ЕКН-проектов) —
  NNN теперь простое значение счётчика (1, 2, 3, ...); `projectInfo` сведён к `code`
  (`loadProjectInfo` не читает `is_ekn`). Спека §9, AGENTS.md и план обновлены.
- **2026-08-18 — деплой + E2E на сервере (Этап 1 завершён).**
  Залиты все файлы в `/opt/mailers/lab-service/`, compose (`lab-db`+`lab`, LAB_* в auth-service,
  `lab_pgdata`, caddy `/api/lab/*` перед `/api/documents/*`) и Caddyfile; `.env` дополнен LAB_*.
  Пересобран `auth-service` (seed приложения `lab` — старый seed.go без LAB был причиной 403
  `/apps/register`). E2E-чек-лист: health 200, 401 без JWT, permissions/me (admin),
  labs/methods/objects/projects/groups/members создание и листинг, заявки с номерами
  (`ЕКН-2026-001-1/2026-GG-GG-M1` для заказчика + `1/2026-GG-M1` для лаборатории, оба метода
  делят NNN), сквозная нумерация (1/2/3/5 — 4 «сгорел» при неудачном push, по дизайну),
  видимость (test2-viewer видит заявку 1 через группу, не видит 3; status/PATCH — 403),
  sync/pull (фильтр видимости, даты справочников заполнены), sync/push (update LWW + create),
  файлы S3 upload/download MATCH (sbe-doc через rclone), permissions + common-access
  (test3 без роли: 403 → после common-access viewer — viewer, без доступа к приватным заявкам).
  Найденные в E2E дефекты исправлены (см. ниже). Тестовые данные в БД (`GG`, методы, заявки 1–5,
  группа) и файл в S3 оставлены для разработки плагина (Этап 2).

### Исправления по итогам E2E (2026-08-18)

1. **`groups.go` не компилировался**: `context` не был импортирован (первая сборка в Docker упала).
   Добавлен `context` в импорты.
2. **Класс багов «0 вместо NULL» для FK-колонок** (нарушение `..._fkey` SQLSTATE 23503):
   `projects.parent_id`, `requests.project_id`/`group_id`/`object_id` передавались как `0`,
   а не `NULL` (строки с id=0 нет) — создание проекта/заявки и push падали. Исправлено:
   хелпер `nullableID(id)` (0 → NULL) применён в create project/request, push create/update;
   в `UPDATE requests`/`projects` вместо `COALESCE($N, col)` — `CASE WHEN $N = 0 THEN NULL ELSE COALESCE(...)`
   (0 = «отвязать»).
3. **NULL-scan**: `projects.parent_id` NULL не сканировался в `int64` (`can't scan NULL into *int64`)
   → list/pull проектов падал. Исправлено: `COALESCE(parent_id, 0)` в SELECT list и pull;
   аналогично `COALESCE(project_id, 0)`/`COALESCE(group_id, 0)` в loadRequest/visibleRequestsQuery.
4. **Маскировка ошибок в `handleCreateProject`**: любая ошибка (в т.ч. 23503) отвечала
   «code already exists». Теперь `pgx.ErrNoRows` → конфликт (409), прочие ошибки — `db error` + лог.
5. **pull labs/methods/objects**: не возвращались `created_at`/`updated_at` (SELECT их не тянул) —
   клиентский кэш не мог строить LWW. Добавлены даты во все три SELECT.

## Статистика ошибок и отступлений

- Локальной Go-сборки нет (на машине отсутствует тулчейн) — компиляция проверяется
  сборкой в Docker на сервере; первая сборка 2026-08-18 упала на отсутствующем `context`,
  после фикса сборка зелёная, сервис развёрнут.
- План (Этап 1, register.go) обещал «seed справочников» при старте — **не реализован**
  (есть только seedOwner). Справочники labs/methods/objects при первом старте пусты,
  заполняются через API (POST labs/methods — admin, POST objects — editor). Отмечено в плане ⚠️.
- `currentEmail` возвращает "" без токена — всегда вызывается после `requirePerm`, деградации нет.

### Найденные дефекты (ревью 2026-08-18) — исправлено

1. **Нетранзакционное создание заявки** (`handleCreateRequest`): при неверном `method_id`
   заявка уже вставлена, методы — нет → «битая» заявка в БД. Исправлено: всё создание
   (INSERT + reconcileMethodsTx) обёрнуто в `BEGIN/COMMIT` с `defer Rollback`.
2. **`randomID` на `/dev/urandom`**: при провале чтения возвращал `0000...0` → коллизии
   ключей файлов в S3. Заменён на `crypto/rand.Read`.
3. **Мёртвый код**: `addRequestMethod` после транзакционного рефакторинга не вызывался —
   удалён (номер генерит `reconcileMethodsTx`).

### Замечания (не критично, на усмотрение)

- `s3.go Put/Get`: `ctx` параметра не используется (rclone CLI без ctx) — запрос не
  отменяется при таймауте HTTP. Совместимо с documents-service.
- `files.go handleUploadFile`: если `request_id` указан, но заявки нет — файл уже загружен
  в S3, в БД не привязан (осиротевший объект). Проверка существования есть, но без FK.
- `pushUpdate` (sync.go): обновление заявки и reconcile методов — в разных транзакциях;
  при сбое reconcile заявка уже обновлена. Низкий риск (ошибки там — только неверный
  method_id, маловероятно при LWW).
- `go.mod` без `go.sum` — в Dockerfile `GOFLAGS=-mod=mod` доскачает/создаст его при сборке
  (нужна сеть на сервере); при желании закоммитить go.sum после первой сборки.
- `handleCreateRequest`: `nextSeq` вызывается до транзакции — при откате заявки номер
  «сгорает» (не переиспользуется). Это соответствует дизайну (монотонный, не переиспользуется).
- `handleUpdateRequest`: `effProjectID` использует новый `project_id` из того же PATCH —
  номера для новых методов строятся от нового проекта (существующие заморожены), ок.
- Проекты в pull отдаются всем viewer (без фильтра видимости) — по дизайну (общие
  справочники), отличие от requests (там фильтр).
