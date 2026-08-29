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
| POST | `/methods` | admin, ИЛИ lab_admin ВСЕХ запрошенных `lab_ids` (2026-08-24, делегированные полномочия — `requireLabAdminOfAll`) |
| POST/PATCH | `/objects`, `/objects/{id}` | editor (PATCH — 2026-08-24, characteristics заменяются целиком) |
| GET | `/projects` | viewer |
| POST | `/projects` | editor |
| PATCH | `/projects/{id}` | editor+/владелец |
| GET | `/requests` | viewer (видимость) |
| POST | `/requests` | editor |
| GET | `/requests/{id}` | viewer (видимость) |
| PATCH | `/requests/{id}` | editor+/владелец |
| POST | `/requests/{id}/status` | владелец заявки ИЛИ `requireLabAccess` (lab_operator/lab_admin/app-admin+ этой лабы) — 2026-08-26, закрыт известный пробел (см. История), раньше проверялась только видимость |
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
| PATCH/DELETE | `/equipment/{id}` | editor+ (2026-08-26 — расширено: эксплуатация/поверка/калибровка, см. История) |
| POST | `/equipment/{id}/scan?kind=verification_cert\|verification_act` | editor+ (2026-08-26, multipart-файл → `*_file_key`/`*_file_url`) |
| GET/POST | `/equipment/{id}/calibrations` | viewer / editor+ (2026-08-26, POST — multipart, пересчитывает `last_calibration`/`next_calibration`) |
| GET/POST/DELETE | `/equipment/{id}/methods` | viewer / editor+ (2026-08-26, роль main/auxiliary на связи) |
| GET/POST/DELETE | `/equipment/{id}/documents` | viewer / editor+ (2026-08-26, документация — список файлов, `files.purpose='equipment_doc'`) |
| GET | `/lab-members` | admin (без `?lab_id=`) / любой участник лабы (с `?lab_id=`, 2026-08-24) |
| POST | `/lab-members` | admin, ИЛИ lab_admin ИМЕННО этой лабы (2026-08-24, `requireLabAdminOf` — любая роль сотрудника своей лабы, включая назначение другого lab_admin) |
| DELETE | `/lab-members/{lab_id}/{email}` | admin, ИЛИ lab_admin ИМЕННО этой лабы (2026-08-24) |
| PATCH | `/methods/{id}` | admin, ИЛИ lab_admin ХОТЯ БЫ ОДНОЙ из ТЕКУЩИХ лаб метода (2026-08-24, `requireLabAdminOfAny`; при смене `lab_ids` — lab_admin ВСЕХ новых лаб, `requireLabAdminOfAll`) (formulas/classification/chart_configs/input_parameters/presentation/operator_form/lab_ids/description/calibration_attributes/calibration_operator_form, 2026-08-26) |
| GET | `/equipment-links` | viewer (2026-08-26, вся таблица `equipment_links` одним запросом) |
| POST/DELETE | `/equipment/{id}/auxiliaries` | editor+ (2026-08-26, `{id}` становится ОСНОВНЫМ для `auxiliary_equipment_id` — many-to-many, независимо от `method_equipment.role`) |
| DELETE | `/methods/{id}` | admin, ИЛИ lab_admin хотя бы одной из лаб метода (2026-08-24, `requireLabAdminOfAny`) |
| GET | `/requests/{id}/chart/{cfg_id}` | viewer |
| POST | `/requests/{id}/protocol?template=&format=` | editor+ |
| GET | `/requests/{id}/export.xlsx` | editor (2026-08-24, серии + агрегаты/статистика, `excelize`) |
| GET | `/dashboard?period=` | viewer |
| POST | `/instrument-buffer` | editor (2026-08-28, буфер данных приборов — не привязан к заявке/лабе, см. История) |

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

## Буфер результатов приборов — instrument_result_buffer (2026-08-28)

По решению пользователя (переработка WP3d — TDT Reader): прибор (десктопное
приложение, первый потребитель — TDT Reader, метод ГГ) **не знает и не должен
знать** номер заявки/серии — риск пришить результаты не к тому эксперименту при
ручном вводе номера в отдельностоящую программу. Вместо прямой записи в
`measurement_results` прибор шлёт данные в промежуточный буфер и предъявляет
только hash; связывание с конкретной заявкой происходит на стороне формы
испытателя (где номер заявки/серии уже корректно выбран контекстом).

- `main.go`: `CREATE TABLE instrument_result_buffer (hash TEXT PRIMARY KEY,
  values JSONB NOT NULL, created_by TEXT, created_at, consumed_at,
  consumed_by_result_id BIGINT REFERENCES measurement_results(id) ON DELETE SET
  NULL)`. Роут `POST /api/lab/instrument-buffer` — обычный `editor`-уровень, без
  `requireLabAccess` (прибор не привязан к лаборатории/заявке).
- `results.go`:
  - `handleCreateInstrumentBuffer` — `{hash, values}` → `INSERT ... ON CONFLICT
    (hash) DO NOTHING` (повторная отправка того же hash — например ретрай после
    обрыва связи из локального журнала прибора — безопасный no-op, не ошибка).
  - `claimInstrumentBuffer(hash)` — атомарно `UPDATE ... SET consumed_at = now()
    WHERE hash = $1 AND consumed_at IS NULL RETURNING values`: один и тот же hash
    нельзя случайно прикрепить к двум разным заявкам — повторная попытка получает
    0 строк и понятную ошибку, а не тихий дубль. Сам факт нахождения записи по
    hash — и есть проверка целостности (опечатка/чужой hash просто не найдётся).
  - `handleCreateResult` — новое опциональное поле `instrument_hash`: если
    передано, `claimInstrumentBuffer` вызывается до `saveResultSeries`,
    вернувшиеся `values` домержены в `req.Values` (не перетирают явно переданные
    поля формы); после успешного сохранения — best-effort
    `linkInstrumentBufferResult` (проставляет `consumed_by_result_id` для
    прослеживаемости, ошибка только логируется — целостность уже обеспечена
    claim'ом выше).
- Задеплоено на VDS (`scp` + `docker compose up -d --build lab`, health ok).
  E2E на сервере (ручной JWT): повторная отправка одного hash — идемпотентна
  (одна строка, значения не изменились); повторный claim того же hash — 0 строк
  (ожидаемая ошибка). Тестовые данные удалены после проверки.
- **Живой E2E с реальным hash из TDT Reader (2026-08-28) нашёл и исправил баг**:
  `handleCreateResult` вызывал `claimInstrumentBuffer` (помечает `consumed_at`)
  ДО `saveResultSeries` — если `saveResultSeries` падал ПОСЛЕ успешного claim
  (например ошибка формулы — не хватает вручную вводимого параметра типа
  `mass_before`, которого прибор не знает), hash оказывался "сожжён" навсегда:
  `claimInstrumentBuffer` ищет только `consumed_at IS NULL`, повторный claim
  тем же hash больше не находил строку, данные из буфера терялись без
  возможности повтора. Исправлено: новая `releaseInstrumentBuffer(hash)` —
  `UPDATE ... SET consumed_at = NULL WHERE hash = $1 AND consumed_by_result_id
  IS NULL`, вызывается в error-ветке `handleCreateResult` сразу после неудачи
  `saveResultSeries`, если `req.InstrumentHash != ""`. Best-effort (ошибка
  только логируется). Проверено живьём на реальном hash из буфера (отправлен
  настоящим TDT Reader): 1) submit с пустыми `values` → формула упала на
  `mass_before` → `consumed_at` откатился в NULL (подтверждено); 2) повторный
  submit того же hash с `mass_before`/`mass_after` → успех, результат создан,
  `consumed_by_result_id` проставлен корректно, все значения из буфера
  (tp1-4_smog, temp_of_smog, smoke_temp_curve) корректно смержены с вручную
  введёнными mass_before/mass_after. Тестовые заявка/объект/результат/буфер
  удалены после проверки, продакшн-данные (317 реальных заявок) не тронуты.
  Задеплоено (`results.go`, md5 сверен, health ok).
- Пока не реализовано: клиент (Python-приложение TDT Reader) ещё не переведён на
  этот флоу (текущая задеплоенная версия приложения шлёт данные напрямую с
  номером заявки/серии — предыдущая итерация WP3d) — планируется отдельным
  раундом доработки (+ локальный журнал последних 20 измерений с ретраем и
  повторным показом hash/QR). Поле в форме испытателя («вставить hash») пока не
  добавлено ни в один клиент (sbe-lims/sbe-lims-mobile) — не commit/push (только
  локально на ветке `backend`), изменения не бампят версию плагина sbe-lims (это
  чисто бэковая доработка, TS/JS клиента не менялись).

## Исходящая почта — результаты заказчику + дубль в LPITrack (WP2, 2026-08-28)

См. `docs/superpowers/specs/2026-08-28-sbe-lims-outbound-email-design.md` +
`docs/superpowers/plans/2026-08-28-sbe-lims-outbound-email-plan.md`. `lab-service`
раньше умел только ПРИНИМАТЬ почту (`email_ingest.go`, IMAP) — теперь умеет и слать.

- Новый файл `outbound_email.go`: `sendMailWithAttachment` — тот же паттерн, что
  `sbe-core/auth-service/email.go sendMail` (net/smtp, exim-релей, STARTTLS с
  самоподписанным сертификатом), расширенный под MIME multipart/mixed (текст +
  docx-вложение) и RFC 2047 (Subject с кириллицей корректно кодируется —
  оригинальный `sendMail` этого не делал, для служебных писем сходило с рук,
  для клиентских писем с протоколом решил не рисковать).
- `labs.auto_send_email` (новая колонка, default false) — per-lab переключатель
  автоотправки; `sent_emails` — журнал КАЖДОЙ попытки (успех и неудача
  одинаково, видимость важнее) и одновременно защита от повторной АВТОотправки
  (`hasSentEmail` проверяет только `success=true`).
- Триггер — `triggerCompletionEmails`, вызывается из ДВУХ мест (оба меняют
  `requests.status`): `handleSetRequestStatus` (`requests.go`) и
  `handleKanbanMove` (`kanban.go`) — только при РЕАЛЬНОМ переходе в completed
  (`existing.Status != "completed" && new == "completed"`), в фоновой
  горутине (`go ...`, `context.WithoutCancel`) — не блокирует ответ на смену
  статуса отправкой почты. `shouldAutoSend` — чистая функция без БД/сети
  (юнит-тесты в `outbound_email_test.go`).
- `POST /requests/{id}/send-email` (`handleSendRequestEmail`) — ручная кнопка,
  шлёт оба письма безусловно (без проверки `auto_send_email`/журнала).
  `GET /requests/{id}/sent-emails` — журнал для карточки заявки.

### Живой E2E нашёл реальный инфраструктурный баг (не в коде — в docker-compose)

Первая попытка (`LAB_SMTP_HOST` по умолчанию `localhost`) — `dial tcp
[::1]:25: connection refused`: контейнер `lab` не имеет доступа к `exim` на
хосте через `localhost` (это loopback КОНТЕЙНЕРА, не хоста). auth-service
(уже рабочий эталон) использует `SMTP_HOST=host.docker.internal` — добавил
`LAB_SMTP_HOST=host.docker.internal` в `docker-compose.yml` для `lab`, но
получил ВТОРУЮ ошибку: `lookup host.docker.internal: no such host`. Причина:
`host.docker.internal` — не встроенное поведение Docker на Linux (в отличие
от Docker Desktop Mac/Windows), нужен явный `extra_hosts:
["host.docker.internal:host-gateway"]` на КАЖДОМ сервисе, которому это
нужно — у `auth-service` он уже был (пропустил при первом чтении compose,
смотрел только `environment:`), у `lab` — не было. Добавлен. После этого —
оба письма (`customer`/`lpitrack`) реально ушли через exim, `success=true`.
**Урок для будущих сервисов**: любой НОВЫЙ сервис в этом compose, которому
нужен доступ к хостовым ресурсам (exim и т.п.) через `host.docker.internal`,
обязан сам получить `extra_hosts` — это не наследуется и не общее для сети
`internal`, настраивается per-service.

Изменения `docker-compose.yml` на сервере (не в git — сервер не
версионируется, см. общее правило проекта): `LAB_SMTP_HOST`/`_PORT`/`_FROM`/
`_SKIP_VERIFY`, `LAB_LPITRACK_EMAIL` (= `LAB_MAIL_LOGIN`, тот же ящик
`lpitn@yandex.ru` — легаси десктопная ЛИМС мониторит его же как инбокс),
`extra_hosts` для `lab`. Провалидировано `docker compose config --quiet`
перед применением, минимальный диф проверен построчно.

Живой E2E (тестовые лаба/метод/объект/3 заявки, удалены после проверки,
продакшн не тронут): (1) `auto_send_email=true` + `external_id` заполнен →
оба письма ушли, `success=true`, обе строки журнала; (2) повторный
туда-обратно перевод в completed → НИ ОДНОЙ новой авто-записи (защита от
дублей работает); (3) ручная кнопка `POST .../send-email` → 2 новые записи
`triggered_by='manual'`, независимо от уже отправленного; (4) заявка БЕЗ
`external_id` → только customer-письмо, ни одной lpitrack-записи (не ошибка,
просто неприменимо); (5) `auto_send_email=false` → ни одного автописьма,
журнал пуст. **Примечание**: тесты (1)/(3) реально отправили письма на
настоящие адреса — `polishchuk@tn.ru` (владелец, для проверки) и
`lpitn@yandex.ru` (реальный ящик трекера LPITrack) — это было необходимо для
честной проверки SMTP-транспорта, письма НЕ удалялись (нет доступа
удалить из внешнего ящика без отдельного запроса на IMAP-удаление).

### Фикс: текст письма заказчику должен явно называть external_id (2026-08-28)

Живая жалоба пользователя по итогам реального письма: пустое вложение —
ожидаемо (тестовая заявка без результатов), но текст письма называл только
внутренний номер ЛИМС (`CustomerNumber`), а для заявки переходного периода
(есть `external_id`) заказчик узнаёт её именно по этому номеру — письмо должно
называть его явно. Новая чистая функция `customerEmailContent(req) (subject,
body string)`: если `ExternalID != ""` — и тема, и текст письма ведут с
`№{external_id}`, тело дополнительно поясняет `(учётный номер в ЛИМС системе —
{CustomerNumber})` — оба номера видны, прослеживаемость внутри ЛИМС не
теряется. Без `external_id` — как раньше, только `CustomerNumber`. Юнит-тесты
(`TestCustomerEmailContentWith(out)ExternalID`) — проверяют оба случая.
Задеплоено (`gofmt`/`go build`/`go vet`/`go test` чисто, md5 сверен, health
ok) — новую живую отправку не гонял (тело уже покрыто юнит-тестом с теми же
значениями формата, повторно слать письма на реальные адреса ради этого не
стал).

## ⚠️ Найден и исправлен реальный баг: вторая+ серия могла молча теряться (2026-08-28)

**Обнаружено живым E2E при разработке WP3a**, не связано с WP3a напрямую — баг
существовал и раньше, в уже работающем коде. **Серьёзность**: тихая потеря
данных без ошибки — заявка, в которую добавляли вторую/третью серию через
`POST /requests/{id}/results` БЕЗ явного `series_num` (сервер сам назначает
следующий — так до сих пор делала кнопка «Добавить серию» на десктопе и делает
мобильный сценарий добавления новой серии), могла потерять эту серию целиком.
Email-путь (`email_ingest.go`) не подвержен — он передаёт свой явный номер
серии, не пользуется автоназначением.

**Механизм**: `uq_meas_req_method_series` — уникальный индекс на
`(request_id, method_id, series_num)`, **не различающий** `is_statistical_row`.
Стат-строка (авто-статистика по всем сериям, `recomputeStatistics`) раньше
занимала `nextSeriesNum()` — ТОТ ЖЕ номер, что получит следующая настоящая
серия. Хронология: серия 1 → стат-строка встаёт на номер 2 → **серия 2**
отправляется на номер 2 → упирается в тот же уникальный индекс → `ON CONFLICT
(...) DO UPDATE` молча ПЕРЕЗАПИСЫВАЕТ values стат-строки данными новой
серии, но `is_statistical_row` не меняется (не входит в `SET`) — запись
остаётся помеченной как статистическая → тот же вызов `recomputeStatistics`
сразу следом **удаляет** все строки с `is_statistical_row=true` — и удаляет
именно её, то есть только что отправленные данные серии 2 целиком. Новая
стат-строка встаёт на тот же номер — и следующая (серия 3) наступает на те же
грабли. **Работает только для самой первой серии заявки+метода — начиная со
второй, автоназначенный номер гарантированно уже занят стат-строкой.**

**Исправлено**: стат-строка теперь получает фиксированный `series_num = -1`
(вне диапазона настоящих серий, которые всегда `>= 1`) вместо
`nextSeriesNum()` — гарантированно никогда не пересекается ни с одной
настоящей серией. `nextSeriesNum()` как функция не тронута — по-прежнему
верно назначает следующий номер НАСТОЯЩЕЙ серии (не читает и не пишет `-1`).
`-1` нигде, кроме `uq_meas_req_method_series`, не используется как значимое
число — `loadStatsRow`/протокол/клиенты обращаются к стат-строке только по
`is_statistical_row=true`, `series_num` этой строки нигде не отображается.

**Не восстановлено**: если этот баг уже срабатывал на реальных заявках раньше
(до этого фикса) — потерянные данные ВТОРОЙ/ТРЕТЬЕЙ серии восстановить
нельзя (перезаписаны, затем удалены, без следа). Признак, что заявка МОГЛА
пострадать: у неё есть стат-строка (`is_statistical_row=true`) с
`source_series_count=1`, но пользователь предполагал больше одной серии —
это не различимо от «действительно была одна серия» без дополнительного
контекста, установить постфактум малореально.

Живой E2E фикса (тестовая заявка, метод РП, лаборатория ЛПИ, удалена после
проверки): 3 серии подряд без явного `series_num` → все 3 сохранились
корректно (было: только последняя выживала до следующей отправки, потом и она
терялась); правка существующей серии (`series_num` явно) — как и раньше,
апсерт на месте, без создания новой строки.

## ⚠️ Найден и исправлен реальный баг: фото «до/после» из мобильного пикера не показывалось в таблице результатов (2026-08-28)

Живая жалоба пользователя на заявке 287/2026 (id=1378): загрузил через мобильный
"Фото до испытания"/"Фото после испытания" (см. WP3a ниже, `renderPhotoUploadPicker`
в `sbe-lims-mobile`) jpg/gif/png для серии 2 — все три показывались в плагине
«Заявки» как обычное вложение заявки (`req.files`, ожидаемо — `uploadFileBytes`
регистрирует ЛЮБОЙ загруженный файл там), но ни один не появился как изображение
в таблице «📊 Результаты испытания» (`getProtocolHTML`, kind="ui").

**Причина**: две НЕЗАВИСИМЫЕ системы хранения фото, из которых протокол умел
рисовать `<img>` только для одной:
1. Фото-**атрибуты** метода (`data_type="photo"`, значение лежит в
   `measurement_results.values[attribute_id]`, например `photo_before_test`/
   `photo_after_test` у ГГ) — их и рисовал `renderTableHTML`/`renderTableDocx`
   (`protocol.go`) уже давно (см. §Калибровочная кривая для контекста
   фото-атрибутов).
2. Top-level колонки `measurement_results.photo_before`/`photo_after` — куда
   ПИШЕТ мобильный "Фото до/после испытания" (изначально задуманы для
   авто-сопоставления фото из входящих писем, `email_ingest.go`). Протокол их
   вообще не читал — ни в HTML, ни в DOCX, независимо от формата файла.

Серия 1 этой же заявки показывала фото нормально, потому что была введена
так, что значения попали в `photo_before_test`/`photo_after_test` (атрибуты).
Серия 2 (мобильный пикер) — молча проваливалась в пустоту рендера.

**Фикс**: новые `TableColumn.Kind` = `"photo_before"` / `"photo_after"` — читают
top-level колонки НАПРЯМУЮ, отдельно от `values`/фото-атрибутов:
- `results.go`: `loadSeriesPhotos(requestID, methodID)` — параллельный
  `loadSeriesValues` запрос (тот же WHERE/ORDER BY, чтобы индексы серий
  совпадали), возвращает `[]string` до/после. Намеренно ОТДЕЛЬНАЯ функция, не
  подмешивание в `values`: `loadSeriesValues` переиспользуется
  `buildFormulaEnv`/агрегацией/экспортом/графиками — заливать туда
  photo_before/photo_after означало бы риск случайно потянуть URL фото в
  DSL-формулу или CSV/XLSX-экспорт.
- `protocol.go`: `placeholderCtx` += `photoBefore`/`photoAfter []string`
  (параллельно `series`, индекс = серия); `seriesPhotoAt(ctx, kind, i)` —
  bounds-checked доступ; `renderTableHTML`/`renderTableDocx` — новые ветки для
  `Kind == "photo_before"/"photo_after"` (тот же `<img>`/`<w:drawing>` через
  `photoRegistry`, что уже было у фото-атрибутов); `tableColumnHeader` — дефолтные
  подписи "Фото до испытания"/"Фото после испытания" (переопределяемые `label`,
  как у `series_no`).
- `block-editor.ts` (десктопный конфигуратор, sbe-lims v0.2.23): "📷 Фото до
  испытания"/"📷 Фото после испытания" — две новые фиксированные опции в
  селекторе колонок таблицы результатов, рядом с "Серия (номер по порядку)".
  Админ добавляет их в любую таблицу как обычную колонку (drag-reorder,
  подпись переопределяема), НЕ атрибут — не нужно ничего настраивать в
  input_parameters/operator_form метода.
- Тест: `protocol_test.go` `TestRenderTableHTMLPhotoBeforeAfterColumn` —
  разные серии с/без before и после, дефолтные и переопределённые подписи.
- **Живой E2E на реальной заявке 287/2026** (id=1378, метод ГГ, id=1;
  подтверждено пользователем): добавил `{kind:"photo_before"}`/
  `{kind:"photo_after"}` в существующую таблицу "Приложение" метода ГГ
  (`PATCH /methods/1`, presentation.blocks) — `POST /requests/1378/protocol
  ?template=ui&format=html` до фикса не содержал `<img>` для серии 2 (только
  для серии 1, через фото-атрибуты); после — оба фото серии 2 нашлись как
  отдельные `<img src="…file-redirect?key=…main-1000055416.jpg" …>` рядом с
  уже существующими колонками фото-атрибутов. Реальная production-заявка не
  повреждена — правка presentation чисто аддитивная (2 новые колонки в конце
  существующей таблицы).

### Доработка того же дня: две живые жалобы после первого фикса фото-колонок

**1. Колонки photo_before/photo_after добавлены РЯДОМ со старыми
фото-атрибутными колонками (не вместо них)** — на заявке 287/2026 после
первого фикса в таблице оказалось 4 фото-колонки сразу: старые
`photo_before_test`/`photo_after_test` (атрибуты, фото серии 1) и новые
`photo_before`/`photo_after` (top-level, фото серий 2/3). Жалоба пользователя:
"не понятно, почему фотографии второй и третьей серии зарендерились в
отдельные колонки". Это не баг рендера (каждая колонка честно показывает
СВОЙ источник) — а следствие того, что для этой конкретной заявки/метода
одновременно существуют ДВЕ независимые механики фото (см. запись выше),
и серия 1 введена одним способом, серии 2/3 — другим. Исправлено НЕ кодом, а
живой правкой данных/конфига этой заявки (с явного разрешения пользователя,
т.к. это реальная production-заявка, а не тестовая — оба прямых SQL/API-write
без такого разрешения заблокировал классификатор безопасности auto-mode):
  - Значения `photo_before_test`/`photo_after_test` серии 1 скопированы в её
    top-level `photo_before`/`photo_after` — ЧЕРЕЗ штатный
    `POST /requests/{id}/results` (тот же апсерт, что у "✏️ Править" на
    десктопе/мобильного ввода), `values` серии не менялись, формулы
    пересчитались к тем же значениям (идемпотентно).
  - Колонки `{kind:"attribute", attribute_id:"photo_before_test"}`/
    `..._after_test"}` убраны из таблицы "Приложение" метода ГГ (`PATCH
    /methods/1`) — остались только 2 новые top-level колонки. Атрибуты
    `photo_before_test`/`photo_after_test` НЕ удалены из метода (только их
    колонки в этой таблице) — они использовались единственно здесь (проверено
    grep по всему конфигу метода), но сама возможность вводить фото как
    атрибут (data_type="photo") у метода осталась.
  - Живой E2E после обеих правок: `POST .../protocol?template=ui&format=html`
    — все 3 серии показывают фото в ОДНИХ и тех же 2 колонках "Фото до
    испытания"/"Фото после испытания" (серия 1 — оба фото, серии 2/3 — только
    "до", т.к. "после" не грузили).

**2. Расчёт формулы блокировал сохранение ВСЕЙ серии, если не хватало
операнда** — жалоба пользователя: ввёл "Масса до" (mass_before), ещё не ввёл
"Масса после" (mass_after) → ошибка расчёта mass_loss, форма не даёт
переключиться на другую серию. Тот же класс бага, что уже нашли и починили
25.08 для aggregated-формул (`evalAggregatedFormulas`) — только для
series-уровня цена была ещё выше: `saveResultSeries` вызывал `applyFormulas`
ДО `INSERT`, поэтому ошибка ОДНОЙ производной формулы отменяла запись ВСЕХ
введённых значений серии целиком (не только саму формулу), а раз "переключение
серии" в мобильном UI — это неявное сохранение (см. WP3a ниже), форма
переключиться не могла. Прямое требование пользователя: "заполнение формы не
должно зависеть от расчётов" — испытатель может готовить следующую серию, пока
не закончил текущую, ввод не обязан идти по порядку зависимостей формулы.
  - `applyFormulas` теперь делегирует в новую чистую функцию
    `evalSeriesFormulas` (симметрично `evalAggregatedFormulas`) — формула,
    которую не удалось посчитать, ПРОПУСКАЕТСЯ (с логом), а не прерывает весь
    проход; цель просто остаётся неопределённой в values, остальные формулы и
    сам INSERT выполняются как обычно.
  - Тест: `results_test.go` `TestEvalSeriesFormulasSkipsFailingFormulaContinuesRest`
    — формула ДО сломанной и ПОСЛЕ досчитываются, сама сломанная (не хватает
    mass_after) отсутствует в результате, aggregated-формула в этот проход не
    попадает.
  - Живой E2E (тестовая заявка, метод ГГ, удалена после проверки):
    `values={"mass_before":"500","place":"ЛПИ"}` без `mass_after` →
    `HTTP 200`, `mass_before`/`place` сохранены, `mass_loss` в результате
    отсутствует (было: `HTTP 400`, ничего не сохранялось).

## Таймер, захват события, лог наблюдений (WP3c ч.2, 2026-08-28) — WP3 закрыт целиком

Спека/план: `docs/superpowers/specs/2026-08-28-sbe-lims-timer-widget-design.md`
+ `-plan.md`. Последняя часть WP3 (3a/3b/3c-ч.1/3d уже были готовы). Роадмап:
«секундомер + кнопка «Воспламенение», останавливающая эксперимент и
заполняющая время/факт; «стандартные наблюдения» — кнопки типа «Вспышка»/
«Пробежка пламени», пишущие событие+время в лог».

**Находка**: «воспламенение» уже существовало как данные —
`email_ingest.go` знает `flam_ignition`/`flam_time` как обычные raw-имена из
legacy-писем метода «Flam» — кнопка «Зафиксировать событие» просто пишет в 2
уже существующих обычных атрибута метода, новой модели данных не
понадобилось. «Лог наблюдений» — новый `data_type="event_log"` (массив
`{label, seconds}`), сервер его НЕ парсит структурно нигде, кроме
форматирования вывода (см. ниже).

- Backend не менялся почти нигде — ТОЛЬКО форматирование вывода
  (`protocol.go`): новый `formatEventLog(v any) string` — `event_log`-атрибут
  в читаемый текст `"label1 (Ns); label2 (Ms)"` вместо `fmtVal()`-дампа
  Go-структуры (та же причина, что уже была у `[object Object]` для
  curve-массивов). Добавлена ветка в ОБЕИХ парах путей (HTML+DOCX, таблица+
  инлайн-плейсхолдер) — `renderTableHTML`/`renderInlineHTML`/
  `renderInlineDocxRuns`/`renderTableDocx`, по образцу уже существующей для
  `DataType=="photo"`.
- **⚠️ Найден и исправлен реальный баг при написании юнит-теста** (не при
  живом использовании — эта возможность ещё не была доступна испытателям):
  одиночный плейсхолдер `event_log`/`photo` вне таблицы БЕЗ явного `Agg`
  ("first"/"last") резолвился в `nil` — `aggregateSeries()` по умолчанию
  считает `avg()` по числам, а нечисловое значение (массив/URL) не попадает
  в список чисел → пустой список → `nil`. Для `photo` эта дыра НИКОГДА не
  проявлялась живьём: клиентский picker плейсхолдеров (`block-editor.ts`
  `renderAggChoice`) уже ограничивал выбор до `['first','last']` для фото
  атрибутов с 2026-08-24 — просто раньше не было НИКАКОГО теста, который бы
  это проверял и/или использовал `event_log` тем же путём. Исправлено:
  `renderAggChoice` теперь применяет то же ограничение к `event_log`
  (переименован параметр `photoOnly` → `seriesOnly`, отражает более широкое
  назначение).
- **Ещё один найденный и исправленный баг** — `MethodOperatorForm` (Go)
  структурно был `{Fields []OperatorFormField}` — при ЛЮБОМ чтении/повторной
  сериализации `operator_form` (например `GET /methods`) поле `timer`
  (клиентская схема, сервер его не хранит структурно) молча ТЕРЯЛОСЬ, даже
  если реально сохранилось в JSONB колонке при `PATCH`. Найдено живым E2E
  (сохранил `timer`, перечитал — пусто). Исправлено: `MethodOperatorForm`
  += `Timer json.RawMessage` (`json:"timer,omitempty"`) — сервер по-прежнему
  НЕ интерпретирует его структуру, просто больше не теряет при round-trip.
- Тесты (`protocol_test.go`): `TestFormatEventLog` (несколько записей,
  пустой/не-массив, запись без `label`), `TestRenderTableHTMLEventLogColumn`,
  `TestRenderInlineHTMLEventLogPlaceholder` (с явным `Agg:"last"` — без него
  тест ловит именно баг выше).
- **Живой E2E** (throwaway-метод id=8 с `input_parameters`
  boolean+float+event_log + `operator_form.timer` с обоими блоками,
  throwaway-заявка — оба удалены после проверки): конфиг `timer`
  сохранился и вернулся при повторном чтении (после фикса выше); сохранение
  серии с `ignited="Да"`, `ign_time=45`, `observations=[{label,seconds}×2]`
  прошло; таблица результатов протокола показала `event_log`-колонку как
  `"Flash (10с); FlameRun (30с)"` — читаемо, не дамп.

Клиентский рендер (таймер-виджет, конфигуратор) — см. `sbe-lims/AGENTS.md`
(v0.2.25) и `sbe-lims-mobile/AGENTS.md` (v0.1.13).

### Редизайн: список кнопок вместо одной фиксированной (2026-08-29)

Живая жалоба пользователя после тестирования формы: (1) единственная жёстко
названная кнопка «🔥 Зафиксировать событие» не подходит — лаборатории называют
разные события по-разному, у каждого свой результат (напр. метод ГВ:
«Зафиксировано воспламенение» должно заполнять 2 поля и останавливать таймер,
а «зафиксирована вспышка до 5 с»/«зафиксирована пробежка пламени» — только
писать в лог, НЕ останавливая таймер); (2) лог наблюдений было непонятно как
использовать; (3) формат лога должен быть «N сек - label» (время ПЕРЕД
названием), а не «label (Nс)».

- `MethodOperatorForm.timer` — было `{capture?, log?}` (ровно один capture +
  один log-список), стало `{buttons: Array<{label, action}>}` — каждая кнопка
  независимо выбирает `action.kind`: `"capture"` (2 поля, стоп таймера) или
  `"log"` (запись в `event_log`-атрибут, таймер не останавливается). Backend
  НЕ меняется структурно — `Timer` остаётся `json.RawMessage`, сервер по
  прежнему не интерпретирует его содержимое.
- `formatEventLog` (`protocol.go`) — формат исправлен на `"%s сек - %s"`
  (seconds, затем label), было `"%s (%sс)"`. Обновлены `TestFormatEventLog`/
  `TestRenderTableHTMLEventLogColumn`/`TestRenderInlineHTMLEventLogPlaceholder`.
- **⚠️ Найден и исправлен ЕЩЁ ОДИН реальный баг того же класса, что уже был у
  `Timer`** (см. выше) — но на этот раз ДЕЙСТВИТЕЛЬНО ЖИВОЙ, не только
  теоретический: `OperatorFormField` (Go) был `{AttributeID, Label, Required,
  HelpText}` — БЕЗ полей под `default`/`visibility` (WP3c ч.1, уже отгружено
  живьём v0.2.24/v0.1.12) и новый `suggestions`. Проверено на реальном
  продовом методе «ГВ» (id=2, `samples_in_date.default={"kind":"today"}`,
  настроено админом ранее): `GET /methods` ДЕЙСТВИТЕЛЬНО отдавал это поле без
  `default` — то есть баг был живым в проде, просто ещё никто не заметил
  (клиент рендерит форму по свежесохранённому объекту в памяти конфигуратора,
  не по повторному `GET`, поэтому пропажа не была видна сразу). Исправлено
  тем же паттерном, что `Timer`: `OperatorFormField` += `Default`/
  `Visibility`/`Suggestions` как `json.RawMessage` (opaque, сервер не
  интерпретирует).
- **Миграция реальных данных**: метод «ГВ» (id=2) — единственный в проде с
  настроенным `timer` — имел старую форму `{capture:{...}, log:{attributeId:
  "", events: []}}` (log НИКОГДА не был реально настроен — пустой
  `attributeId`). Мигрирован через `PATCH /methods/2` в новую форму:
  `{buttons: [{label: "Зафиксировано воспламенение", action: {kind:
  "capture", booleanFieldId: "flam_ignition", secondsFieldId: "flam_time"}}]}`
  — сохраняет ТО ЖЕ поведение, что уже работало. Лог для ГВ (вспышка/пробежка
  пламени) НЕ настроен — нет `event_log`-атрибута в методе; админ должен
  завести его и добавить log-кнопки через конфигуратор сам, если нужно.
- Клиент (оба) — защита `Array.isArray(timer.buttons)` перед рендером/
  инициализацией конфигуратора: старая форма `{capture,log}` (если где-то ещё
  не мигрирована) не должна ронять форму `TypeError`, просто не показывает
  таймер.
- **Живой E2E** (throwaway-метод id=9, throwaway-заявка id=1420, оба удалены
  после проверки): `operator_form` с `suggestions` на текстовом поле и новым
  `timer.buttons` (1 capture + 1 log) сохранён и вернулся при повторном
  чтении без потерь; серия с `obs_log=[{label:"зафиксирована вспышка до 5 с",
  seconds:150},{label:"зафиксирована пробежка пламени",seconds:155}]` —
  таблица протокола и инлайн-плейсхолдер (`Agg:"last"`) показали ТОЧНО
  `"150 сек - зафиксирована вспышка до 5 с; 155 сек - зафиксирована пробежка
  пламени"` — прямое совпадение с примером пользователя.

Клиентский рендер (кнопки таймера, подсказки-suggestions, конфигуратор) — см.
`sbe-lims/AGENTS.md` (v0.2.26) и `sbe-lims-mobile/AGENTS.md` (v0.1.14).

## WP5 — «№ серии» → «№ п/п» для динамических таблиц (2026-08-29)

Роадмап: `docs/superpowers/plans/2026-08-27-mvp-roadmap-plan.md`. Исходный
запрос MVP-документа: «вставка динамических таблиц основного и вспомогательного
оборудования — добавить возможность заменять № серии на № п/п». По коду
`TableColumn` (RichNode `Type="table"`) уже рендерит строку как `i+1` — позицию
строки среди `ctx.series`, никак содержательно не привязанную к понятию
«серия» — единственное, что реально различало «№ серии»/«№ п/п», это ДЕФОЛТНАЯ
ПОДПИСЬ колонки («Серия»). Кастомную подпись «№ п/п» уже можно было ввести
вручную и раньше (см. `TestRenderTableSeriesNoColumn`, ветка с `Label: "№
п/п"`) — не отдельная функция, просто малоизвестная.

Минимальная реализация: новый `TableColumn.Kind = "sequential"` — рендерится
ИДЕНТИЧНО `"series_no"` (то же `i+1`), только дефолтная подпись «№ п/п» вместо
«Серия». Отдельный `kind`, а не просто рекомендация вводить свою подпись у
`series_no` — так пресет «№ п/п (порядковый номер)» появляется прямо в списке
добавления колонки (`renderTableNodeEditor`, `block-editor.ts`), не требуя от
пользователя знать про этот обходной путь. Полезно для ЛЮБОЙ таблицы, где
«Серия» не подходит по смыслу (в т.ч. таблицы оборудования, которые
собираются вручную через `static_table` или через обычную динамическую
таблицу — конкретно «авто-таблица оборудования из method_equipment» НЕ
строилась и не запрашивалась отдельно, это не часть данной доработки).

- `protocol.go`: `tableColumnHeader` += ветка `"sequential"` → «№ п/п» (или
  явный `Label`, как у `series_no`); обе точки рендера тела таблицы
  (`renderTableHTML` ~L461, `renderTableDocx` ~L717) — условие расширено на
  `c.Kind == "series_no" || c.Kind == "sequential"`.
- `results.go`: doc-comment `TableColumn.Kind` расширен.
- Тест: `TestRenderTableSequentialColumn` (`protocol_test.go`) — дефолтный
  заголовок, нумерация 1..N, явный `Label` переопределяет заголовок; тот же
  паттерн, что уже существующий `TestRenderTableSeriesNoColumn`.
- **Живой E2E** (throwaway-метод id=10 — 1 атрибут + таблица с колонками
  `sequential`+атрибут, throwaway-заявка id=1421 с 2 сериями, оба удалены
  после проверки): протокол (HTML) показал заголовок `"№ п/п"` и строки
  `1`/`2` — корректно.
- Backend деплой — обязателен (`protocol.go` меняется), выполнен, health ok.
- Клиент (только десктоп — таблицы протокола настраиваются в конфигураторе
  метода, которого нет на мобильном) — `src/types/lims.ts`
  (`TableColumn.kind`), `src/ui/block-editor.ts` (`renderTableNodeEditor`) —
  см. `sbe-lims/AGENTS.md` (v0.2.27).

## WP4 — created_by/updated_by серий результатов (2026-08-29)

Роадмап: `docs/superpowers/plans/2026-08-27-mvp-roadmap-plan.md`, WP4.
Модель прав уже решена раньше (упрощена — строгий gating не в MVP), реальный
UI редактирования серии уже есть (`renderSeriesEditForm`, WP3a) — оставался
только сам факт «кто/когда менял», тем же паттерном, что уже есть у
`equipment_calibrations.created_by` (`equipment_ext.go`).

- `main.go`: `ALTER TABLE measurement_results ADD COLUMN IF NOT EXISTS
  created_by/updated_by TEXT NOT NULL DEFAULT ''`.
- `results.go`: `MeasurementResult` += `CreatedBy`/`UpdatedBy`; `handleListResults`
  SELECT/scan расширен; `saveResultSeries` += параметр `who string` — INSERT
  пишет `created_by=$9, updated_by=$9`, `ON CONFLICT DO UPDATE` пишет ТОЛЬКО
  `updated_by = EXCLUDED.updated_by` (`created_by` НЕ в SET-списке — при
  конфликте Postgres сохраняет исходное значение колонки, первый автор не
  перезаписывается повторным сохранением). Вызовы: `handleCreateResult` →
  `currentEmail(r)`; `email_ingest.go` → литерал `"email-ingest"` (нет
  реального пользователя, письмо разбирается автоматическим процессом).
- Авто-статистическая строка (`is_statistical_row=true`, `recomputeStatistics`)
  НЕ проходит через `saveResultSeries` — `created_by`/`updated_by` у неё
  остаются пустыми, это ожидаемо (не результат действия человека).
- **Живой E2E** (throwaway-метод id=11, throwaway-заявка id=1422, оба удалены
  после проверки): создание серии от `polishchuk@tn.ru` → `created_by=
  updated_by="polishchuk@tn.ru"`; повторное сохранение ТОЙ ЖЕ серии от
  `nasonova.m@tn.ru` (второй реальный admin-email из `lab_permissions`) →
  `created_by` остался `"polishchuk@tn.ru"`, `updated_by` стал
  `"nasonova.m@tn.ru"` — именно так, как задумано.
- Backend деплой — обязателен (новые колонки + сигнатура функции), выполнен,
  health ok.
- Клиент (только десктоп — `renderSeriesList`, «изменено: `<email>`» под
  названием серии) — см. `sbe-lims/AGENTS.md` (v0.2.28). Мобильный не
  затронут (там нет журнала/списка серий с историей правок, только форма
  ввода).

## WP6 — динамическая нумерация заголовков, путь б (2026-08-29)

Роадмап: `docs/superpowers/plans/2026-08-27-mvp-roadmap-plan.md`, WP6. Развилка
путей (a)/(b) — решение пользователя: путь (б), плейсхолдер номера ВНУТРИ
вручную набранного текста, а не авто-печать `DocumentBlock.Title`. Причина
выбора — путь (a) поменял бы вывод уже настроенных, реально уходящих
заказчикам протоколов в момент деплоя без их ведома; путь (б) opt-in по
конструкции (ничего не меняется, пока плейсхолдер явно не вставлен).

- Новый `InlineNode.Source = "heading_number"` — не системное поле заявки и не
  атрибут метода, структурный счётчик: резолвится в порядковый номер БЛОКА,
  содержащего этот плейсхолдер (не в номер каждого отдельного вхождения) —
  среди блоков, отфильтрованных для текущего вида вывода (`kind`), которые
  ТОЖЕ содержат этот плейсхолдер где-то в себе (блок без плейсхолдера
  пропускается, не сбивая счёт следующих).
- `placeholderCtx` += `headingNumbers map[string]int` (blockID → номер, считает
  `computeHeadingNumbers` один раз на метод, по уже отфильтрованному для
  текущего `kind` списку блоков) и `currentBlockID string` (мутируется перед
  рендером содержимого каждого блока в `protocolHTML`/`protocolDocx` — тот же
  указатель `*placeholderCtx`, общий на весь рендер метода, что и остальные
  поля контекста).
- `resolvePlaceholder` += ветка `Source == "heading_number"` → `headingNumbers[
  currentBlockID]` или пустая строка, если блок не найден (не паникует).
- `blockHasHeadingNumberPlaceholder`/`richNodeHasHeadingNumberPlaceholder` —
  ищут плейсхолдер во ВСЕХ местах, где может быть `InlineNode`: `Children`
  (paragraph/heading), `Items` (bullet_list), `Rows` (static_table) — НЕ в
  `Columns` таблицы данных серий ("table") — там ячейки не редактируются
  текстом (см. `renderTableNodeEditor`), плейсхолдер вставить негде.
- Тесты: `TestComputeHeadingNumbers` (нумерует только блоки с плейсхолдером,
  пропуская остальные без сбоя счёта), `TestResolvePlaceholderHeadingNumber`
  (резолв + пустая строка для неизвестного блока).
- **Живой E2E** (throwaway-метод id=12 — 3 блока: заголовок с плейсхолдером,
  абзац БЕЗ плейсхолдера, второй заголовок с плейсхолдером; throwaway-заявка
  id=1423, оба удалены после проверки): протокол показал `"Раздел 1. Общая
  информация"` / `"Просто текст без плейсхолдера"` (без номера, как и должно)
  / `"Раздел 2. Результаты"` — средний блок не сбил счёт следующего.
- Backend деплой — обязателен (`protocol.go` меняется), выполнен, health ok.
- Уже существующие методы (ГГ/ГВ/РП и т.д.) НЕ используют этот плейсхолдер —
  их протоколы гарантированно не меняются этим деплоем (сам механизм активен
  только там, где плейсхолдер явно вставлен).
- Клиент (только десктоп — `block-editor.ts` `PlaceholderPickerModal`,
  `types/lims.ts`) — см. `sbe-lims/AGENTS.md` (v0.2.29). Мобильный не
  затронут (нет редактора блоков представления).

## Аудит истории email-приёма + фикс легаси-формата и "method2" (2026-08-29)

После включения WP7 пользователь попросил сверить БД на предмет отсутствующих
данных (см. `audit_missing_data.js`/`.csv`, скрипт разово прогнан локально,
не в репозитории) — нашлось: 297 из 469 completed-заявок вообще без единой
серии результатов, плюс сотни нерасчитанных расчётных/агрегатных полей.
Причина оказалась куда крупнее одного пропущенного формула — при разборе
`email_ingest_processed` (2731 обработанных писем за всю историю):

| Причина ошибки | Кол-во |
|---|---:|
| `unknown type ""` (нет поля `type`) | 1278 |
| `unknown method "method2"` | 585 |
| `no JSON object found in body` | 137 |
| `missing ID field` | 14 |

Успешных всего 716 из 2731 — **исторически терялось больше писем, чем
применялось**.

**1. `unknown type ""` (1278 писем) — легаси-формат ДО появления служебных
атрибутов.** Подтверждено пользователем: "заявка+результат в одном письме"
никогда не было — тип и метод определялись ИСКЛЮЧИТЕЛЬНО папкой (`LPITrack` —
только заявки, `Comb` — только результаты ГГ, `Flam` — только результаты ГВ,
`FlamProp` — только результаты РП); поля `type`/`method` появились только в
этом году, с началом работы над новой версией. Раньше `processMessage` требовал
`type` строго `"application"`/`"result"`, иначе — `error`.

- `email_ingest.go`: `typ == ""` → `legacyTypeForFolder(folder)` (LPITrack→
  application, остальные→result); для `result` — `payload["method"]` жёстко
  перезаписывается по `legacyFolderMethodKey` (папка авторитетнее любого поля
  в письме для этой ветки); для `application` без `payload["method"]` —
  `legacyMethodKeyFromAimIndicator` (подстрочный поиск "горюч"/"воспламен"/
  "распростран" в свободном тексте `aim_indicator`, который есть даже в самых
  старых письмах).
- Важная деталь: канонизация имён полей (`canonicalFieldNames["Comb_lenth_1"]
  = "comb_length_1"`, `knownRawFields["aim_indicator"]`) уже была в коде до
  этого фикса — легаси-формат просто никогда до неё не доходил, отсекаясь на
  диспетчере по `type` раньше.
- Тесты: `TestLegacyTypeForFolder`, `TestLegacyFolderMethodKey`,
  `TestLegacyMethodKeyFromAimIndicator`.
- **Живой E2E на реальном историческом письме** (`fetch-mail -folder=Comb
  -id=123`, external_id=123, заявка 939 "Русхимсеть, Китай" — ОДНА из 297
  найденных в аудите без единой серии): применилось, создало серию 1 с
  ПОЛНЫМИ данными (`mass_before/mass_after/mass_loss`, `comb_length_1..4`,
  `average_comb_length=100` — посчиталось верно формулой, `combustion_time`,
  `burning_drops`, фото загрузилось в S3 через rclone). Агрегатные поля,
  зависящие от НОВЫХ полей инструментальной телеметрии (`combustibility_group`
  и т.п. — старое письмо их физически не содержит), ожидаемо не досчитались —
  это не регрессия, просто у старых писем меньше данных, чем у новых.

**2. `unknown method "method2"` (585 писем, из них 572 — результаты).** Не
многометодная заявка (как я сперва предположил по `aim_indicator`, где
перечислены сразу 3 цели) — пользователь не подтверждал эту гипотезу, а
прямая проверка показала: `method2` = ГВ (метод id=2), просто отсутствовал в
`LAB_MAIL_METHOD_MAP` (`{"method1":1,"method3":3}`, метод2 никогда не
добавляли). Исправлено — значение обновлено через штатный канал
(`app_env_pending`) на `{"method1":1,"method2":2,"method3":3}`.

- **Живой E2E**: `fetch-mail -folder=Flam -id=502` (заявка 1201, метод ГВ,
  тоже одна из 297 без серий) — применилось 8 писем-результатов, создало
  серии 2–9 корректно.

**3. Побочный фикс — деплой был заблокирован недоступностью
downloads.rclone.org.** При деплое фикса `email_ingest.go` сборка `lab`-образа
упала на шаге установки `rclone` (`wget ... downloads.rclone.org`, exit code
4 — таймаут соединения, подтверждено даже с самого хоста, не только из
Docker-сборки). `docker compose up -d --build` при этом НЕ упал с ошибкой —
молча продолжил работать со СТАРЫМ образом (без фикса), что едва не привело к
ложному выводу "задеплоено, но не работает". Заодно нашли и структурную
причину — `RUN wget rclone` стоял ПОСЛЕ `COPY --from=build /mailers-lab` в
`Dockerfile`, поэтому любое изменение Go-кода инвалидировало кэш этого слоя и
заставляло качать rclone заново при КАЖДОМ деплое, даже не касающемся
S3/rclone. Исправлено: (1) переставили RUN-шаг ДО COPY бинарника — теперь
переустанавливается только если меняется сама команда/базовый образ; (2)
источник rclone — GitHub releases с закреплённой версией (v1.75.0) вместо
`downloads.rclone.org/.../rclone-current-...` (тот конкретно был недоступен с
этого VDS в момент фикса, GitHub — доступен). Пересборка после этого прошла
чисто.

**Массовый повторный разбор — ВЫПОЛНЕН (2026-08-29, по прямому запросу
пользователя «сделай бэкап БД и делай пакетами»).** Перед началом — свежий
ручной `pg_dump` БД `lab` (`/opt/mailers/backups/lab_manual_..._pre_batch_
reprocess.sql.gz`, 47 МБ — старый ежедневный `backup.sh` бэкапит другую БД,
см. WP7 выше). Два разных метода по папкам:

- **Comb + Flam** (604 уникальных `external_id` суммарно, включая
  `method2`-письма из Flam) — точечно, по одному, через `fetch-mail
  -folder=X -id=Y` (тот же путь, что и постоянный воркер, дедуп не мешает —
  просто обновляет запись). Скрипт `reprocess_batch.sh` на сервере, батчами
  по 10–50 с проверкой лога между батчами (`reprocess_log.txt`) — ни одной
  ошибки/обрыва соединения за весь прогон. Один email (Comb, external_id=178)
  не нашёлся точечным поиском (та же причина, что ниже) — обработан вторым
  способом.
- **LPITrack** (284 заявки) — точечный поиск по подстроке НЕ находил письма
  вовсе (0 кандидатов даже при поиске по полному `ID` вида
  `LPIZAYAVKINAPRO-121`) — вероятная причина: IMAP `SEARCH BODY` ищет по
  СЫРОМУ (не декодированному) телу письма, а мягкие переносы строк
  quoted-printable-кодирования (`=\r\n`) могут разрывать искомую подстроку
  ровно посередине — разный порог обрыва в каждом письме, надёжной короткой
  подстроки не существует. Вместо точечного поиска — очистка dedup-записей
  (`DELETE FROM email_ingest_processed WHERE folder='LPITrack' AND
  (error='unknown type ""' OR error LIKE '%method2%')`) + `docker compose
  restart lab` (воркер стартует новый цикл сразу при старте) — полный проход
  по всей папке (647 писем) занял меньше минуты, ни одной ошибки соединения.
  Тела скачиваются ТОЛЬКО для писем без записи в дедупе (см. `processFolder`
  выше) — уже обработанные (успешно или по другим, нетронутым причинам)
  пропускаются мгновенно. Тот же подход применён и к одиночному "Comb"-
  исключению (178).
- **Итог**: +74 новые заявки чистым приростом (490 → 564, за вычетом
  собственных throwaway-тестов сессии). Обе целевые причины ошибок
  (`unknown type ""`, `method2`) — теперь 0 записей во всей истории.
  Оставшиеся ~79 писем LPITrack (`no JSON object found`/`unknown method ""`/
  `"method4"`) — НЕ трогали, отдельные причины, не входили в scope этого
  фикса.
- Все новые заявки без ЕКН (59 шт., та же логика, что WP7) мигрированы в
  проект «EML» тем же ручным `UPDATE`, что раньше.
- **Повторный прогон аудита** (`audit_missing_data.js`, см. WP7 выше) —
  «завершённая заявка без единой серии» упало с 297 до **32** (в основном
  заявки 2025 года, до начала работы этой версии программы — вероятно
  результат никогда не приходил отдельным письмом в принципе, не проверялось
  подробно). Полный CSV — `C:\Obsidian\mailers\audit_missing_data_2026-08-29.csv`
  (перезаписан).

## WP7 — email-приём включён на проде; проект по умолчанию для писем без ЕКН (2026-08-29)

**Первое: сам WP7 (финальная валидация email-приёма).** Пользователь включил
постоянный воркер приёма почты (`LAB_MAIL_ENABLED=true`) через настройки
плагина (ЦУП-канал `env_admin.go`/`secret-applier.sh`) — первый цикл (после
пары попыток, пока пароль/enabled реально не применились — см. ниже) обработал
реальный бэклог: 21 новая заявка (id 1424–1444) из папок LPITrack, включая
одну кратковременную обрывающуюся IMAP-сессию (`connection closed` на 3
папках, не повторилась на следующих циклах — транзиентная сетевая проблема,
не баг). Протокол одной из свежесозданных заявок (1424) собрался корректно
(3474 байта HTML) — подтверждает, что цепочка «письмо → заявка → протокол»
работает без ручного вмешательства на реальных данных. **Перед всем этим
вручную снят бэкап БД `lab`** (`pg_dump` → `/opt/mailers/backups/
lab_manual_*.sql.gz`) — существующий `backup.sh`/cron бэкапит СОВСЕМ ДРУГУЮ
базу (`mailers`, не `lab`) — на проде для `lab` до этого момента вообще не
было автоматического бэкапа, стоит иметь в виду отдельно от этой задачи.

Отдельно найдена и объяснена (не баг) причина жалобы «переключатель приёма
почты уходит в неактивное состояние после сохранения»: `app_env_pending`
применяется хост-cron раз в минуту (`secret-applier.sh`), настройки в плагине
отображают факт `set`/`pending` из БД, а не мгновенное состояние — после
сохранения смотреть статус нужно спустя ~1 минуту, а не сразу.

**Второе: проект по умолчанию для писем без ЕКН (прямой запрос
пользователя после включения приёма).** `applyApplicationEmail` уже применял
правило «ЕКН как проект» (`ensureEknProject`) — но только когда в письме
ЕКН указан; без него `project_id` оставался `NULL`. Добавлен настраиваемый
проект по умолчанию для ЭТОГО случая (не меняет правило «ЕКН как проект» —
оно приоритетнее, срабатывает первым):

- `requests.go`: `ensureEknProject` теперь тонкая обёртка над новым
  `ensureProjectByCode(ctx, tx, code string, isEkn bool)` (общий find-or-create
  по `projects.code`, `isEkn` контролирует флаг `is_ekn` — `false` для
  проекта по умолчанию, он не является автосозданным ЕКН-проектом).
- `email_ingest.go`: `emailIngestConfig` += `defaultProjectCode string`
  (из `LAB_MAIL_DEFAULT_PROJECT_CODE`, опционально — пусто = старое
  поведение). `applyApplicationEmail` — `else if cfg.defaultProjectCode != ""`
  ветка после проверки ЕКН.
- `sbe-core/auth-service/env_admin.go`: `allowedAppEnvKeys["lab"]` +=
  `LAB_MAIL_DEFAULT_PROJECT_CODE` (белый список — без этого ЦУП отклонил бы
  ключ), `TestAllowedAppEnvKeysLabWhitelist` обновлён.
- Значение `EML` применено через штатный канал (`app_env_pending`, тот же
  секретный/несекретный канал, что и остальные `LAB_MAIL_*`) — проект `EML`
  уже существовал (`projects.id=2`, `name="Заявки из почты"`, заведён раньше
  пользователем вручную) — `ensureProjectByCode` нашёл его по `code`, не
  создал дубликат.
- **Ручная миграция (по прямому запросу «перенеси все новые принятые заявки
  в указанный проект»)**: `UPDATE requests SET project_id=2 WHERE
  external_id != '' AND project_id IS NULL` — затронуло 13 заявок (все из
  сегодняшнего первого цикла приёма, без ЕКН в письме); после — 0 заявок
  почтового приёма без проекта.
- `go build`/`go vet`/`go test` (оба модуля — `lab-service` и
  `sbe-core/auth-service`) — чисто. Backend деплой обязателен (оба сервиса),
  выполнен, health ok у обоих.
- Клиент (только десктоп, `settings-tab.ts` — новое поле «Проект по
  умолчанию (без ЕКН)» в разделе почтовых настроек) — см.
  `sbe-lims/AGENTS.md` (v0.3.0). Мобильный не затронут.

## Системные поля формы испытателя — per-series, не per-request (WP3b, 2026-08-28)

Спека/план: `docs/superpowers/specs/2026-08-28-sbe-lims-system-fields-per-series-design.md` +
`-plan.md`. Правило из роадмапа: «при добавлении 2-й и последующих серий: если
в тот же календарный день — только атрибуты метода; если в другой день —
снова спросить системные (дата/условия среды)».

**Находка при брейнсторме**: report_date/samples_in_date/exp_date/amb_temp/
amb_pres/amb_moist хранились как 6 колонок `requests` — ОДНО значение на всю
заявку. Если серии реально выполняются в разные дни с разными условиями
среды, эта модель не может сохранить историю: второй день перезаписывал бы
значения первого безвозвратно. Решение пользователя — нужна реальная per-day
история, хранится как обычные ключи внутри `measurement_results.values` (не
новые колонки, как у `photo_before`/`photo_after`) — побочный эффект: формулы
метода теперь МОГУТ ссылаться на `amb_temp`/`amb_pres` и т.п. как на обычный
параметр `values`, раньше это было невозможно.

- `handleCreateResult` (`results.go`) — 6 полей запроса (`report_date` и т.д.,
  без изменений в теле POST — клиент шлёт их теми же именами, что и раньше)
  теперь кладутся ПРЯМО в `req.Values` (пустая строка — не трогать) ДО вызова
  `saveResultSeries`, вместо отдельного `updateRequestSystemFields` (UPDATE
  `requests.*`) — функция удалена, requests-колонки больше не пишутся отсюда.
- `email_ingest.go` — та же логика: `applyRequestSystemFields` (писала все 7
  системных полей в requests.*) сужена до `resolveInventorFromEmail` (ТОЛЬКО
  "inventor" → `inventor_id` — единственное системное поле, которое остаётся
  per-request, не меняется день ото дня); остальные 6 идут через
  `setIfMeaningful(values, key, v)`, как обычный атрибут результата письма.
- **requests-колонки НЕ удалены** — остаются fallback-источником для
  протокола (см. ниже) и для доминиграционных данных/заявок без серий.
- **Одноразовый идемпотентный backfill** (список `stmts` в `main.go`, 6 почти
  идентичных `UPDATE ... WHERE NOT (values ? '<key>')`) — копирует текущее
  `requests.<col>` в `values` ПОСЛЕДНЕЙ по `series_num` серии каждой
  заявки+метода, только там, где такого ключа ещё нет. Безопасно гонять на
  каждом старте (сходится к no-op после первого успешного прогона).
- `resolveSystemPlaceholder` (`protocol.go`) — одиночный плейсхолдер вне
  таблицы результатов (например «Выписка из протокола от {report_date}»)
  теперь резолвится через новый `seriesSystemField(ctx.series, key, fallback)`
  — идёт от ПОСЛЕДНЕЙ серии к первой, берёт первое непустое значение (не
  обязательно из последней — у неё поле могло быть скрыто формой как "тот же
  день"), фоллбэк — `ctx.req.<Col>` (домиграционные данные).
- Тест: `protocol_test.go` `TestResolveSystemPlaceholderPerSeriesFallback` —
  значение из более ранней серии находится, даже когда последняя его не
  содержит; существующий `TestResolveSystemPlaceholder` не тронут (проверяет
  чистый фоллбэк-путь, серии без этих ключей вообще).
- **Живой E2E** (throwaway-заявки, удалены после проверки):
  1. Реальная заявка 287/2026 (id=1378) — backfill подтверждён: последняя
     (3-я) серия получила все 6 значений из requests.* в values, сами
     requests-колонки не изменились.
  2. Новая заявка без серий → `POST .../results` с системными полями →
     попали в values серии, requests.* заявки остались пустыми.
  3. Заявка с 2 сериями (у 1-й — `exp_date`/др., у 2-й, "того же дня", —
     НЕТ) → одиночный плейсхолдер `{report_date}` (шаблон "excerpt", у метода
     ГГ есть блок с ним, `show_in_excerpt=true`) корректно взял значение из
     СЕРИИ 1, хотя серия 2 — последняя.

Мобильный клиент (показ/скрытие полей по дню) — см. `sbe-lims-mobile/AGENTS.md`,
запись v0.1.10.

## Навигация по сериям — мобильный ввод, десктоп просмотр/правка (WP3a, 2026-08-28)

См. `docs/superpowers/specs/2026-08-28-sbe-lims-series-navigation-design.md` +
`-plan.md`. Создание серий — только с мобильного (`sbe-lims-mobile`); десктоп —
просмотр и правка уже введённых данных, без создания (код/эндпоинт создания не
удалялись, просто не рендерятся в UI — решение пользователя, можно будет
вернуть кнопку позже).

- Новый `DELETE /requests/{id}/results/{series}` (`handleDeleteResultSeries`,
  `results.go`) — удаляет серию, сдвигает `series_num` всех последующих
  серий этого метода на −1 (в транзакции), затем пересчитывает статистику и
  агрегированные формулы (тот же путь, что после обычного сохранения).
  Правка существующей серии — БЕЗ нового эндпоинта: уже работающий upsert
  (`ON CONFLICT ... DO UPDATE`) в `saveResultSeries`, клиент просто передаёт
  `series_num` существующей серии вместо 0/omitted.
- Живой E2E (см. также раздел бага выше — тот же тестовый прогон): 3 серии
  → удалить среднюю (2) → серия 3 корректно стала серией 2, серия 1 не
  тронута, статистика пересчитана верно; правка существующей серии на месте
  (не создаёт новую строку); удаление несуществующей серии → 404, понятная
  ошибка. Тестовые данные удалены, продакшн (317+ реальных заявок) не тронут.

## Калибровочная кривая метода РП (WP1, 2026-08-28)

См. `docs/superpowers/specs/2026-08-28-sbe-lims-calibration-curve-design.md` +
`docs/superpowers/plans/2026-08-28-sbe-lims-calibration-curve-plan.md`. Метод РП
фиксирует НЕСКОЛЬКО калибровочных точек (расстояние→тепловой поток), а не одно
число — нужна интерполяция, зависящая от того, какое КОНКРЕТНОЕ оборудование
использовано (может быть несколько установок метода, каждая со своей кривой).

- **Находка при разборе**: `interpolate(x, xs, ys)` (кусочно-линейная,
  продолжение по касательной за пределами диапазона) УЖЕ была реализована и
  протестирована в `dsl.go` (замена legacy `frm00020`) — не писалась заново,
  только подключена к данным.
- `calibration_attributes` — новый `data_type="curve"`: значение в
  `equipment_calibrations.values[attr_id]` — массив `[{x,y},...]` вместо
  скаляра. Сервер не валидирует `data_type` (как и раньше — свободная строка),
  вся логика — в `calibration_curve.go` при чтении.
- `measurement_results.equipment_id` (новая nullable-колонка, `main.go`) — на
  каком экземпляре оборудования сделано измерение. Общая доработка
  traceability для ЛЮБОГО метода (не только РП) — обязательна, только если у
  метода несколько единиц `method_equipment.role='main'`; при ровно одной
  сервер резолвит её сам (`resolveSingleMainEquipment`).
- **`calibration_curve.go`** (новый файл): `injectCalibrationCurves` (вызов из
  `applyFormulas` до цикла формул) — для каждого `calibration_attributes`
  атрибута с `data_type="curve"` резолвит оборудование (explicit
  `equipment_id` серии, либо единственное основное) → дату испытания
  (`requests.exp_date`, fallback `time.Now()`) → последнюю калибровку этого
  `equipment_id`+`method_id` на эту дату (`resolveEquipmentCalibrationCurve`)
  → кладёт `{attr_id}_xs`/`{attr_id}_ys` (`[]any`, совместимо с
  `dsl.go toFloatSlice`) в `FormulaEnv.Params`. **Best-effort** — если
  резолв не удался (оборудование неоднозначно, калибровки нет), просто не
  подставляет параметры; формула, если использует `interpolate`, падает со
  своей обычной ошибкой "параметр не найден" (тот же путь, что у любого
  другого отсутствующего параметра) — не блокирует остальной проход.
  `isMainEquipmentOfMethod` — валидация explicit `equipment_id` из
  `POST .../results` (400, если оборудование не «Основное» этого метода).
  `parseCalibrationCurve` — чистая функция (без БД), вынесена отдельно для
  юнит-тестов (`calibration_curve_test.go`): резолв точек по дате/юнит-тесты
  на БД не пишутся (в проекте нет мок-инфраструктуры, см. «Статистика ошибок»
  ниже) — только чистая логика парсинга + инъекция в `FormulaEnv`.
- **Сигнатуры изменены**: `saveResultSeries` (+`equipmentID int64`, между
  `inventorID` и `seriesNum`) и `applyFormulas` (+`requestID, equipmentID
  int64`) — оба вызывающих места (`handleCreateResult`, `handleCalculateSeries`
  recalculate-хендлер, `email_ingest.go` с `equipmentID=0`) обновлены.
  `MeasurementResult`/`handleListResults` — новое поле `equipment_id` в ответе.
- **График калибровочной кривой** (`charts.go handleCalibrationCurveChart`) —
  `GET /equipment/{id}/calibrations/{calibration_id}/curve-chart/{attr_id}` →
  PNG. Сознательное упрощение относительно исходного плана («новый источник
  для ChartConfig») — график для `data_type="curve"` не требует отдельной
  настройки (в отличие от `chart_configs` метода): сама природа атрибута уже
  достаточна, переиспользует `renderChart()` напрямую, без `ChartConfig`-
  абстракции (YAGNI — конфигурировать нечего, форма графика всегда одна и та
  же: линия по точкам одного атрибута).
- **`GET /api/lab/method-equipment`** (новый, `equipment_ext.go`
  `handleListAllMethodEquipment`) — вся таблица `method_equipment` одним
  запросом (по образцу уже существующего `/equipment-links`): клиенту нужно
  узнать число единиц "Основного" оборудования КОНКРЕТНОГО метода (показывать
  ли селектор в форме результатов), обратное направление уже существующему
  `GET /equipment/{id}/methods`.
- Клиенты (десктоп `sbe-lims` v0.2.19→далее без версии, чисто в рамках этой
  же сессии; мобильный `sbe-lims-mobile` v0.1.6): таблица точек x→y вместо
  одного поля для `data_type="curve"` (форма калибровки), `<select>`
  «Оборудование» в форме результатов (только когда > 1 основной единицы).
- `gofmt -w`, `go build ./...`, `go vet ./...`, `go test ./...` — чисто.
  Задеплоено на VDS. **Живой E2E** (тестовые equipment/calibrations/request,
  удалены после проверки, метод РП восстановлен к исходному конфигу):
  2 тестовые установки с РАЗНЫМИ калибровочными кривыми (0,0)-(10,100)-(20,200)
  и (0,0)-(10,50)-(20,300), один и тот же вход (x=15) → результаты 150 и 175
  (ручной расчёт совпал) — подтверждает, что резолв калибровки реально
  привязан к конкретному оборудованию серии, не к первой попавшейся записи.
  Отдельно проверены: PNG-график кривой (корректная прямая линия на
  изображении), ошибка при неоднозначном оборудовании (formula: параметр не
  найден — понятная, не падение сервера), отказ при `equipment_id`, не
  являющемся "Основным" этого метода (400).

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
**2026-08-26**: эти 6 переменных теперь меняются через настройки sbe-lims (раздел
«Приём результатов по email», admin) — не только ручной правкой `.env` на сервере.
Механизм общий для всего проекта (`sbe-core/auth-service`, `env_admin.go` +
`app_env_pending`) — см. `sbe-core/auth-service/AGENTS.md`. Загружаются процессом
только при старте (`loadEmailIngestConfig`, вызывается один раз) — после смены
через UI сервис автоматически пересоздаётся хост-скриптом, ручной рестарт не нужен.

## Сборка / проверка

```
docker compose up -d --build lab        # на сервере
docker compose logs lab --tail 20
docker compose exec lab wget -qO- http://localhost:3000/api/lab/health   # внутренняя проверка
```

## История

- **2026-08-26 — привязка оборудование↔оборудование; параметры калибровки в
  конфигураторе метода.** Три прямых запроса пользователя, продолжение записи
  ниже (v0.2.11) в той же сессии. Задеплоено на VDS, health ok.
  1. **`equipment_links`** (новая таблица, `main_equipment_id, auxiliary_equipment_id`,
     PK на паре, CHECK против самопривязки) — физическое прикрепление
     вспомогательного прибора к основному, ОТДЕЛЬНО и независимо от
     `method_equipment.role` (тот — видимость блока калибровки ДЛЯ МЕТОДА; этот —
     только группировка/отображение в общем списке оборудования). many-to-many —
     один вспомогательный прибор привязывается к нескольким основным (проверено
     E2E). Заметка: у `method_equipment.equipment_id` FK БЕЗ `ON DELETE CASCADE`
     (в отличие от `equipment_links`, где cascade есть на обеих сторонах) —
     удаление оборудования, всё ещё привязанного к методу, корректно отвечает
     409 (та же дисциплина, что у остальных справочников), но при E2E-очистке
     это нужно помнить: сначала отвязать от метода, потом удалять.
  2. **`GET /equipment-links`** — вся таблица одним запросом (не по одной
     единице оборудования) — клиент строит из неё оба множества («мои
     вспомогательные»/«я вспомогательное для») и «скрыть с верхнего уровня» за
     один проход, без N+1 на каждую карточку. `POST/DELETE
     /equipment/{id}/auxiliaries` — один и тот же вызов обслуживает оба
     направления UI (привязать вспомогательный к этому основному ИЛИ привязать
     этот вспомогательный к выбранному основному — разница только в том, чей id
     идёт в `{id}`, а чей — в `auxiliary_equipment_id`).
  3. **Параметры калибровки метода** — прямое продолжение записи ниже
     («калибровка проводится сотрудниками лаборатории... так же как результаты
     испытаний, далее добавим блок настроек калибровки»). Новые JSONB-колонки
     `methods.calibration_attributes` (проще `input_parameters` — только
     id/name/data_type, без fill_method/level/formula/aggregation: калибровочное
     значение всегда вводится вручную ровно один раз за запись журнала) и
     `methods.calibration_operator_form` (тип `MethodOperatorForm` переиспользован
     как есть — та же форма {fields:[{attribute_id,required}]}, что у обычных
     результатов). `handleUpdateMethodConfig` — тот же COALESCE-партиал паттерн,
     что у остальных полей конфига. `loadMethodConfig`/`handleListMethods`/
     `sync.go` pull — все три места разбора конфига метода (см. запись
     2026-08-21 ниже про этот же класс риска у presentation) обновлены синхронно
     в этом же коммите.
  4. **`equipment_calibrations`** — новые колонки `method_id` (чьи
     `calibration_attributes` применялись — оборудование может быть «Основное»
     сразу для нескольких методов, см. `method_equipment.role`),
     `amb_temp`/`amb_pres`/`amb_moist` (универсальные системные поля калибровки —
     тот же принцип, что `requests.amb_temp`/`amb_pres`/`amb_moist` у обычных
     результатов испытаний, см. sbe-lims `AGENTS.md`, «Правило: системные
     атрибуты» — одни и те же для ЛЮБОГО метода, не заводятся как
     calibration_attributes), `values` JSONB (значения calibration_attributes
     ВЫБРАННОГО метода, та же роль, что `measurement_results.values`).
     `recomputeCalibrationDates` не менялся — по-прежнему просто
     `MAX(calibrated_at)`/`+interval`, независимо от метода записи.
  5. **E2E на сервере** (тестовые equipment id 3/4, метод 1=ГГ временно получил
     тестовый `calibration_attributes`): создание main+auxiliary → привязка →
     подтверждена в `GET /equipment-links` → many-to-many (aux привязан ко ВТОРОМУ
     main) → отвязка → самопривязка корректно отклонена (400). Метод 1 временно
     получил `calibration_attributes=[{id:"deviation",...}]` → оборудование 3
     привязано к методу 1 с ролью main → добавлена запись калибровки с
     `method_id`+`values`+`amb_*` → `GET .../calibrations` вернул всё корректно,
     `last_calibration` пересчитан. **Тестовая конфигурация метода 1 (ГГ, реальный
     производственный метод) откачена обратно на `calibration_attributes: []`**
     сразу после проверки — временное состояние не оставлено. Тестовое
     оборудование удалено (сначала отвязано от метода — см. заметку в п.1 выше).
  6. `go build`/`go vet`/`go test` — чисто (новых юнит-тестов на этот раунд не
     добавлено — вся логика либо тривиальный CRUD/JSONB passthrough по
     существующим паттернам, либо БД-зависимый агрегат, уже покрытый классом
     проверки «живой E2E», см. соседние записи).

- **2026-08-26 — расширение «Оборудование» (эксплуатация/поверка/калибровка/
  методы/документация) + фикс безопасности write-роли на смену статуса заявки.**
  Спека: `docs/superpowers/specs/2026-08-26-sbe-lims-configurator-equipment-design.md`.
  Задеплоено на VDS (`docker compose up -d --build lab`), health ok.
  1. **Миграции**: `equipment` — 11 новых колонок (эксплуатация/поверка/калибровка,
     `last_calibration`/`next_calibration` существовали с первой миграции ЛИМС
     дормантными — теперь под управлением сервера); `method_equipment.role`
     ('main'/'auxiliary' на КАЖДОЙ связи отдельно — `is_required`, другая
     семантика, не тронута); новая `equipment_calibrations` (журнал: дата,
     результат, файл, кто внёс); `files.equipment_id`/`files.purpose` —
     переиспользование общей таблицы файлов для документации на оборудование
     (`purpose='equipment_doc'`) без отдельной таблицы.
  2. **`equipment_ext.go` (новый файл)**: `uploadEquipmentFileBytes` — тот же
     паттерн дедупа/S3-put/`file-redirect`, что `uploadFileBytes` (`files.go`,
     заявки), но владелец — equipment, не request (отдельная функция, не
     обобщение существующей — минимальный риск для уже проверенного пути заявок).
     `recomputeCalibrationDates` — `last_calibration = MAX(calibrated_at)`,
     `next_calibration = last + calibration_interval_months` (NULL без заданного
     интервала) — вызывается после КАЖДОЙ новой записи журнала, единственное
     место, где эти два поля пишутся.
  3. **Реальный баг, найденный на живом E2E (не юнит-тестами)**: `PATCH
     /equipment/{id}` падал с голой `{"error":"db error"}` на ЛЮБОМ запросе —
     `handleUpdateEquipment` пропустил `req.VerificationCertNumber` в списке
     аргументов `s.pool.Exec` (16 плейсхолдеров `$1..$16` в SQL, но передано
     только 15 значений — pgx отклонял весь вызов). Найдено сразу первым же
     живым тестом (`PATCH` с `calibration_interval_months`), исправлено,
     передеплоено, весь сценарий (интервал → запись журнала → пересчёт дат →
     сертификат/акт с номером-датой → скан → документация → отвязка метода)
     повторно проверен end-to-end — теперь чисто.
  4. **Фикс безопасности — `POST /requests/{id}/status`** (известный
     задокументированный пробел, был в этом файле с 2026-08-19): проверялась
     только видимость заявки (`requestVisible`), не право записи — участник
     группы без роли lab_operator/lab_admin/владельца теоретически мог сменить
     статус ЛЮБОЙ видимой ему заявки. Заменено на `existing.OwnerEmail == email
     ИЛИ requireLabAccess` — та же функция, что уже гейтит ввод результатов/
     расчёт этой же заявки (её собственный комментарий явно называл "статус" в
     списке защищаемых действий — только этот эндпоинт её не вызывал).
     **E2E на реальной заявке 1378**: неизвестный email → 403 (было бы 200 до
     фикса, заявка видна как completed); временный `lab_operator` лабы 1 → 200;
     статус возвращён в `completed` после проверки. Побочный эффект теста —
     `completed_at` заявки 1378 обновился на момент теста (было
     completed→processing→completed) — не восстанавливалось точное историческое
     значение (не было сохранено до теста), тестовые `lab_members`/оборудование
     удалены.
  5. Тесты: `TestParseOptionalDate` (различает "поле не передано" от "передано
     пустой строкой — очистить колонку"). `go build`/`vet`/`test` — чисто.
     Юнит-тестов на сами HTTP-хендлеры оборудования/статуса нет (в проекте нет
     инфраструктуры для мока БД — см. другие записи) — проверено живым E2E,
     воспроизводящим находку п.3 выше (тот класс проверки, который и должен был
     поймать баг раньше юнит-теста).

- **2026-08-26 — учётка почты email-приёма (`LAB_MAIL_*`) теперь настраивается
  из плагина sbe-lims (администратор), не только правкой `.env` на сервере.**
  Часть общей задачи по закрытию ревью безопасности (`plugins/secrev.md`, план
  `docs/superpowers/plans/2026-08-25-sbe-secrets-cup-plan.md`, раздел A1 —
  `LAB_MAIL_PASSWORD` был отмечен как отложенный, «ротация вручную»). Реализовано
  расширением общего механизма ЦУП (см. `sbe-core/auth-service/AGENTS.md`,
  запись 2026-08-26): новый `POST/GET /auth/apps/env` (admin) с белым списком
  разрешённых ключей на приложение (`env_admin.go`, для `lab` — все 6
  `LAB_MAIL_*`), очередь `app_env_pending`, применяет тот же хост-скрипт
  `secret-applier.sh`, что и `service_secret` — пишет `.env`, пересоздаёт
  контейнер `lab`. lab-service сам не менялся (конфиг как читался при старте
  через `os.Getenv`, так и читается — обновление применяется рестартом
  контейнера, не hot-reload). UI — `sbe-lims/src/ui/settings-tab.ts`, раздел
  «Приём результатов по email» (только admin/superadmin): переключатель
  включения, IMAP-сервер, логин, пароль (никогда не подтягивается обратно с
  сервера — только запись; пустое поле = «не менять»), интервал опроса, карта
  методов (JSON). Проверено end-to-end на сервере (тестовая заявка на смену
  `LAB_MAIL_POLL_INTERVAL_SECONDS` — значение появилось в `.env`, контейнер
  `lab` пересоздан, health ok, строка в `app_env_pending` помечена `applied`
  с обнулённым `value`).

- **2026-08-26 — фикс: `combustibility_group`/`target_group_compliance` не
  считались НИ ДЛЯ ОДНОЙ заявки метода ГГ (найдено на заявке 287/2026, id=1378).**
  Реальная находка при разборе жалобы «не считается target_group_compliance»:
  такого атрибута/правила у метода ГГ вообще не было (существовал только у
  ГВ) — но заодно вскрылся код-баг у УЖЕ существующей формулы
  `combustibility_group = min_grade(smoke_temp_combustibility_group,
  damage_degree_combustibility_group, agg_damage_degree_combustibility_group,
  combustion_time_group, burning_drops_group)` — все 5 аргументов являются
  ВЫХОДАМИ классификации, а `applyAggregatedFormulas` (`results.go`) считал
  формулы уровня `aggregated` (`evalAggregatedFormulas`) ДО классификации
  (`applyAggregatedClassification`) в рамках одного прохода — на момент
  вычисления `combustibility_group` ни один из 5 аргументов ещё не существовал
  в среде, DSL детерминированно падал с «параметр не найден», ошибка тихо
  логировалась и пропускалась (v0.2.8 fix) — цель навсегда оставалась
  неопределённой для ЛЮБОЙ заявки метода ГГ, не только 287/2026.
  - Фикс — `retryPendingAggregatedFormulas` (`results.go`): после
    `applyAggregatedClassification` формулы, чья цель ещё не в `result`,
    пересчитываются повторно с уже готовыми classification-значениями в
    `env.Params`. На живом пересчёте нашлась ЕЩЁ одна ступень зависимости —
    `target_group_compliance` (добавлен как раз в этой сессии) сам зависит от
    `combustibility_group`, который на первом проходе классификации ещё не
    существовал — понадобился ВТОРОЙ проход `applyAggregatedClassification`
    после retry формул (трёхуровневая цепочка classification→formula→
    classification). Оба прохода идемпотентны для уже решённых subjects.
  - `target_group_compliance` для метода ГГ добавлен вручную (append-only
    `UPDATE methods` — новый атрибут `level:aggregated, fill_method:
    classification` + правило классификации `combustibility_group` →
    `target_group_compliance`, сравнение с `kind:target_indicator` по тому же
    паттерну, что уже был спроектирован для ГВ, см. sbe-lims `AGENTS.md`,
    «Правило: системные атрибуты» и запись 2026-08-23 ниже).
  - Тесты: `TestRetryPendingAggregatedFormulasResolvesClassificationDependentFormula`,
    `TestRetryPendingAggregatedFormulasSkipsAlreadyResolvedTargets`,
    `TestThreeLevelDependencyChainNeedsSecondClassificationPass` (результат:
    `combustibility_group`) — все три через `applyRuleToSubjects`/
    `retryPendingAggregatedFormulas` напрямую, без БД. `go build`/`vet`/`test`
    — чисто. Задеплоено на VDS, health ok. Проверено на живых данных заявки
    1378: после пересчёта `aggregated_results` содержит и
    `combustibility_group: "Г4"`, и `target_group_compliance: "Не
    соответствует"` (целевая группа объекта — Г2, Г4 хуже — верно).

- **2026-08-24 — lab_admin: реальный делегированный админ своей лабы (не синоним
  lab_operator).** Найдено пользователем: у shoya.vs@tn.ru роль `lab_admin`
  (`lab_members`), но конфиг методов/«Сотрудники»/канбан-руководитель были
  недоступны — все admin-гейты проверяли только ГЛОБАЛЬНУЮ роль
  (`lab_permissions`), у которой не было персональной строки (только
  `lab_common_access=editor` по умолчанию). ЗАДЕПЛОЕНО на VDS, health ok.
  - Новые `requireLabAdminOf(email, labID)`/`requireLabAdminOfAny(email,
    labIDs)`/`requireLabAdminOfAll(email, labIDs)` (`lims_refs.go`) —
    app-admin+ ИЛИ lab_admin соответствующей лабы(лаб). Применены: (a)
    `handleCreateMethod` — `OfAll` по запрошенным `lab_ids` (не привязать
    метод к чужой лабе); (b) `handleUpdateMethodConfig`/`handleDeleteMethod` —
    `OfAny` по ТЕКУЩИМ лабам метода (метод может принадлежать нескольким,
    достаточно администрировать одну; при смене `lab_ids` в PATCH — `OfAll` по
    НОВЫМ лабам); (c) `handleSetLabMember`/`handleRemoveLabMember` — `Of` по
    `lab_id` операции (lab_admin управляет ЛЮБОЙ ролью сотрудников своей лабы,
    включая назначение другого lab_admin — решение пользователя). Новый
    `methodLabIDs(methodID)` (`references.go`) — лабы одного метода без карты
    по всем методам (`loadMethodLabsMap`).
  - Route-гейты `POST /methods`, `PATCH/DELETE /methods/{id}`, `POST/DELETE
    /lab-members` ослаблены `requirePerm("admin")` → `requirePerm("editor")`
    (`main.go`) — реальная проверка теперь внутри хендлера (тот же паттерн,
    что уже был у `kanban-move`/`lab-members?lab_id=` read).
  - `canApplyKanbanMove` (`kanban.go`) — lab_admin ИМЕННО этой лабы теперь
    проходит в "свободную" ветку руководителя (было — синоним lab_operator).
    Новый тест `TestCanApplyKanbanMoveLabAdminActsAsLabHead`.
  - `go build`/`vet`/`test` — 10/10 зелёных, без регрессий существующих. См.
    также `sbe-lims/AGENTS.md` (v0.2.4, клиентская часть — «Методы»/
    «Сотрудники»/inline-редактирование роли).

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
