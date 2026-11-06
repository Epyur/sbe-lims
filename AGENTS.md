# AGENTS.md — sbe-lims (ЛИМС)

SBE-плагин «ЛИМС» для сотрудников лабораторий: заявки лаборатории (по `lab_members`),
результаты испытаний по сериям, расчёты (DSL-формулы), классификация, статистика, графики,
протоколы (HTML/docx), справочники лаборатории. Клиент lab-service (сервер — канон).
**Дашборд из плагина удалён (2026-08-18)** — будет вынесен в отдельный плагин; серверный
эндпоинт `GET /api/lab/dashboard` оставлен невостребованным (не удалять).

**Бэк — в этой же папке** (`lab-service/`, 2026-08-24, переехал из `server_back/lab-service/`) —
на отдельной ветке `backend` (main этого репозитория — чистый релизный срез кода плагина,
без бэка; см. правило «Бэки в папках плагинов» в корневом `plugins/AGENTS.md`). `lab-service`
общий и для sbe-requests — его `AGENTS.md` содержит указатель сюда.

## Назначение (текущее)

- **Источник данных** — lab-service `https://epyur.fvds.ru` через JWT из ЦУП СБЕ
  (`getService('sbe-apstore').auth.getToken('lab')`, app_id `lab`). Прямые запросы к
  `/api/lab/*` (без локального БД-кэша справочников; заявки/результаты — в памяти вьюхи).
- **Заявки**: список доступных заявок (`GET /requests` — сервер фильтрует по видимости:
  владелец / участник группы / admin / сотрудник лаборатории по `lab_members`), карточка
  с сериями результатов, сменой статуса (`new/received/processing/completed`), графиками
  и протоколом.
- **Результаты**: ввод серии (параметр=значение), сохранение (`POST /results`) —
  сервер автоматически выполняет DSL-формулы/классификацию/статистику; пересчёт серии
  (`POST /results/{series}/calculate`); агрегаты (`GET /results/aggregated`).
- **Справочники**: методы (создание + конфиги formulas/classification/chart_configs/
  input_parameters через JSON-редактор + удаление, всё — admin), испытатели
  (CRUD editor+), оборудование (CRUD editor+), сотрудники лабораторий
  (`lab-members`, admin).
- **Графики**: `GET /requests/{id}/chart/{cfg_id}` → PNG (сервер рендерит по
  `methods.chart_configs`), показываются в карточке заявки.
- **Протокол/выписка/короткий вид** (2026-08-23, блочный редактор — заменил секции
  от 2026-08-22): `POST /requests/{id}/protocol?template=ui|excerpt|protocol&format=html|full`
  → `{html, docx_base64}` (`template` по умолчанию `protocol`; `format=html` — не тратить
  время на DOCX, для карточки результатов/предпросмотра). Документ — блоки форматированного
  текста (абзацы/заголовки/списки/таблицы) с плейсхолдер-чипами (системные данные заявки/
  объекта, атрибуты метода — эксперимента со сверткой avg/min/max/first/last вне таблицы,
  агрегированные как есть); карточка результатов заявки и read-only блок в sbe-requests
  вставляют HTML из `protocol?template=ui&format=html` — прежний отдельный JSON-эндпоинт
  `GET .../short-view` удалён.
- **Точка входа** — магазин: «Установленные → Открыть» (`publishService('sbe-lims', {open})`).
- Вид открывается из ЦУП (`SbeLimsApi extends SbeOpenableApi`), без ribbon.

## Правило: системные атрибуты (2026-08-23)

Данные, общие для ЛЮБОГО метода испытаний (не специфичные для конкретной методики),
заводятся **ОДИН РАЗ в общий каталог**, а не как `MethodAttribute` per-method. Прямое
решение пользователя: "у нас же инвентор он системный, как и другие перечисленные,
зачем их настраивать под каждый метод, заведи эти атрибуты как универсальные, так же
как например «Наименование материала»". Найдено кросс-проверкой реальных писем
Comb/Flam/FlamProp (`json_attr.md`, `mail_records.jsonl`) — метод ГВ по ошибке завёл
`amb_temp`/`amb_pres`/`amb_moist`/`exp_date` как СВОИ атрибуты, хотя эти поля несёт
КАЖДОЕ письмо-результат независимо от метода.

**Текущий каталог** (`SYSTEM_PLACEHOLDERS`, `sbe-lims/src/ui/block-editor.ts`):
`title`/`number`/`object_name`/`ekn`/`owner_email`/`priority`/`test_purpose`/`status`/
`created_at`/`customer_number`/`lab_number`/`target_indicator`/`batch_number`/
`sample_id`/`thickness` (заявка/объект, известны сразу) + `inventor`/`report_date`/
`samples_in_date`/`exp_date`/`amb_temp`/`amb_pres`/`amb_moist` (испытатель/даты/
условия среды — заполняются автоматически из письма-результата, `requests.*`).

**Что автоматически получает новое системное поле** (три поверхности, каталог
дублирован вручную в трёх местах — синхронизировать при добавлении):
1. Доступно как плейсхолдер-чип в блочном редакторе протокола (`SYSTEM_PLACEHOLDERS`
   → `PlaceholderPickerModal`), резолвится сервером (`resolveSystemPlaceholder`,
   lab-service `protocol.go`).
2. **НЕ заводится как `MethodAttribute`** — конфигуратор метода показывает справку
   со списком системных полей прямо над блоком «Атрибуты метода» (переиспользует тот
   же `SYSTEM_PLACEHOLDERS`), чтобы админ не дублировал их вручную на каждом методе.
3. **Автоматически показывается в форме для испытателя** (`renderOperatorFormPreview`)
   — отдельным read-only разделом «Системные данные», ВСЕГДА, независимо от того,
   какие поля админ добавил в `operator_form.fields` для этого метода конкретно;
   `operator_form.fields` содержит только метод-специфичные поля.

**Отличие от метод-специфичных полей с одинаковым ИМЕНЕМ**: не всякое повторяющееся
raw-имя письма — системное. `additional_info`/`substrate`/`mounting_method` тоже
встречаются у нескольких методов (Comb/Flam), но их СОДЕРЖАНИЕ метод-специфично
(разное описание подложки/крепления/наблюдений) — остаются обычными `MethodAttribute`
на каждом методе, где нужны. Критерий системности — значение ОБЪЕКТИВНО одно и то же
независимо от метода (кто испытывал, когда, какая была температура в лаборатории),
а не просто совпадение имени поля в письме.

**Реализация на сервере** (`lab-service`, 2026-08-23): новые колонки `requests.*`
(`inventor_id` FK на `inventors`, `report_date`/`samples_in_date`/`exp_date`/
`amb_temp`/`amb_pres`/`amb_moist` TEXT) вместо generic `values` каждой серии;
`email_ingest.go` — `systemRequestFields` перехватывает канонические имена ДО того,
как они попали бы в атрибуты метода, `applyRequestSystemFields` резолвит имя
испытателя в `inventor_id` точным совпадением по `inventors.name` (не создаёт
испытателя автоматически). См. lab-service `AGENTS.md` за деталями.

## Структура

| Файл | Что это |
|---|---|
| `src/main.ts` | `SbeLimsPlugin`: настройки, syncService, refreshMethods (pull методов при onload), view, publishService |
| `src/services/sync.service.ts` | `LimsSyncService`: JWT из ЦУП, заявки/результаты/расчёт/справочники/графики/протокол, `moveKanbanCard` (2026-08-24, `POST .../kanban-move`), `listLabMembers(labId?)` (без аргумента — полный список, admin; с `lab_id` — ростер одной лабы, доступен любому её участнику), permissions/me, таймауты 30с, понятные 401/403. sendRequestEmail (2026-10-19) возвращает {ok, queued, already_queued} — сервер только ставит отправку в очередь |
| `src/ui/lims-view.ts` | `LimsView` (тип `sbe-lims-view`): фасад (шапка + сайдбар-дерево + контент). «Все заявки»/«Результаты и протоколы» — один рендер `renderRequests(filter?)` с разными фильтрами по статусу; **«Очередь лаборатории»** (2026-08-24) — Kanban-доска `renderQueueBoard()`, 4 колонки new/received/processing/completed, колонки 2/3 делятся на ячейки по испытателям лабы (`renderPerTesterKanbanColumn`), drag-and-drop + контролы в детали заявки идут через один `performKanbanMove()`; карточка заявки (серии/расчёт/статус-назначение/графики/протокол), «Методы» (список + JSON-редактор конфигов, admin), «Объекты» (только чтение; компактная таблица, название объекта — ссылка на его заявки, 2026-10-18), «Испытатели»/«Оборудование» (список + создание, editor+), «Сотрудники» (список + добавление/удаление, admin). Кнопка «Отправить письмо» (2026-10-19) ставит письмо в очередь: журнал показывает ⏳ в очереди/✅/❌, пока есть pending — кнопка неактивна и журнал опрашивается раз в 3 с |
| `src/ui/settings-tab.ts` | Настройки: apiUrl + «Права доступа» (роли + общий доступ) |
| `src/types/lims.ts` | `LimsRequest` (+ `assigned_to`/`completed_at`, 2026-08-24, Kanban-доска), `Lab`, `LabMethod`, `LabObject`, `MeasurementResult`, `AggregatedResult`, `Inventor`, `Equipment`, `LabMember`, `MethodConfig`, `ProtocolResponse`, `DocumentBlock`/`RichNode`(+`align`/`static_table`/`rows`, 2026-08-24)/`InlineNode`(+`sup`/`sub`, 2026-08-24)/`TableColumn`/`PlaceholderSource`/`PlaceholderAgg`, `OperatorFormField`/`MethodOperatorForm` |
| `src/ui/block-editor.ts` | `renderBlockEditor` — визуальный (WYSIWYG) редактор ОДНОГО блока: строки абзац/заголовок/список/таблица/**статическая таблица** (2026-08-24, `renderStaticTableEditor` — визуальный конструктор, ячейки — mini rich-text), каждая текстовая строка — небольшой `contenteditable` с тулбаром Ж/К/**x²/x₂** (2026-08-24, `execCommand('superscript'/'subscript')`)/🏷Плейсхолдер (чип, `PlaceholderPickerModal`); абзац/заголовок — **выравнивание** (`renderAlignSelect`, 2026-08-24); таблица данных серий — структурный виджет (не contenteditable), колонки атрибутов + "Серия", drag-reorder; **список — drag-reorder пунктов** (2026-08-24) |
| `src/ui/subsup.ts` | `toggleSubSupPalette` (2026-08-24, вынесен из `lims-view.ts`) — юникод-палитра над-/подстрочных символов для plain `<input>` (название атрибута, подпись колонки таблицы) |
| `src/styles.css` | Классы `tn-lims-*` на семантических токенах, `.tn-lims-chip` (плейсхолдер), `.tn-lims-rich-line` (contenteditable-строка), `.tn-lims-kanban*` (2026-08-24, доска) |

## Настройки (data.json)

`apiUrl` (default `https://epyur.fvds.ru`). Роли lab (апп-шкала lab_permissions):
`viewer`(1) < `editor`(2) < `admin`(3) < `superadmin`(4, реализована 2026-08-19).
Лабораторный скоуп — `lab_members(lab_id, email, role)`: `lab_operator` / `lab_admin` /
`lab_auditor` (реализована 2026-08-19, см. таблицу ниже).

## Модель прав ЛИМС (переработка согласована 2026-08-18, основное реализовано 2026-08-19)

| Роль | Где | Возможности | Статус |
|---|---|---|---|
| **superadmin** | `lab_permissions` (новая, выше admin) | Создание лабораторий, назначение admin/superadmin. Видит все лаборатории | ✅ реализовано (сервер) |
| **admin** | `lab_permissions` (существующая) | Видит все лаборатории | ✅ (создание лабораторий у admin забрано — теперь только superadmin) |
| **editor / viewer** | `lab_permissions` (существующие, app-level) | Базовые app-level способности | не менялось |
| **lab_admin** | `lab_members` (per-lab) | *Задумано*: управляет своей лабораторией (добавляет участников, правит методы, назначает админов) | ⚠️ **не реализовано** — сервер сейчас не отличает `lab_admin` от `lab_operator` (оба дают только запись результатов через `requireLabAccess`); делегированные полномочия внутри своей лабы без app-level admin — отдельная задача авторизации по ресурсу, не сделана |
| **lab_operator** (испытатель) | `lab_members` (per-lab) | Проводит испытания, вносит данные | ✅ (`requireLabAccess`, как было) |
| **lab_auditor** | `lab_members` (новая, per-lab) | Доступ ко всем разделам своей лаборатории, только чтение | ✅ реализовано (`requireLabRead`: результаты/графики/протокол читает, писать не может) |

Сделано на сервере 2026-08-19 (см. lab-service/AGENTS.md, запись «модель ролей»):
роль `superadmin` (`roleRank`, защита от самоповышения в `handleSetPermission`,
`seedOwner` теперь ставит владельца в superadmin), роль `lab_auditor` + разделение
`requireLabAccess`(запись)/`requireLabRead`(чтение), фильтрация `GET /labs` по
видимости, `POST /labs` → superadmin-only. Плагин: `canAdmin`/`canEditRefs` в
`lims-view.ts` учитывают `superadmin`; `settings-tab.ts` (оба — sbe-lims и sbe-requests)
пускают superadmin к панели прав и добавляют `lab_auditor`/`superadmin` в списки ролей.

⚠️ **Известный пробел, не исправлен**: `POST /requests/{id}/status` проверяет только
видимость заявки (`requestVisible`), не write-право — теоретически статус может
сменить любой, кому заявка видна (например, участник группы), не только
lab_operator/lab_admin/владелец. Не трогали: эндпоинт общий с sbe-requests, а
правильная политика («кто должен мочь менять статус») не зафиксирована ни в одной
спеке — фикс без явного решения рисковал бы регрессией у обоих потребителей.

## Правила

- `catch(e: unknown)` + `errorMessage()`; `requestUrl()` (в `Promise.race` с
  `window.setTimeout`); без `any`; UI на русском; автор — Полищук Евгений (polishchuk@tn.ru).
  Классы `tn-lims-*` / `tn-btn*` / `tn-table` на семантических токенах sbe-core.
- Коммиты/пуши — только по явной команде пользователя.
- **«Фиксируй» = поднять версию (+0.0.1 в `manifest.json` и `package.json`), обновить
  документацию, подготовить сообщение для коммита и СПРОСИТЬ подтверждение commit/push.**
  Не поднимать версию плагина при коммите чисто бэковых изменений в `lab-service/`
  (ветка `backend`), если сам плагин не менялся.
- **Никогда и нигде не использовать «лучше»/«хуже» (и производные: best/worst,
  лучший/худший)** — ни в UI, ни в коде/идентификаторах, ни в комментариях/документации
  (2026-08-22, явное указание пользователя: это оценочные категории, неприменимые к
  объективной оценке результатов испытаний). Только нейтральные сравнительные термины:
  «выше/ниже», «больше/меньше», «максимальное/минимальное». Пример: DSL-функции
  `min_grade`/`max_grade` (не `worst_grade`/`best_grade`), агрегация правил
  классификации `none`/`avg`/`min`/`max` (не `best`/`worst`).

## 1.1.6 — журнал Telegram: «заменено повторным закрытием» (2026-11-06)

Follow-up to 1.1.5 on the live case (request 504-ГГ). The message arrives now, but
the journal in the request card showed the previous closure's row as «в очереди».

- The row is `superseded` — the closure it belongs to really was delivered, and its
  message was replaced by the message about the later closure. The plugin mapped
  only `sent`/`failed` and sent everything else to «в очереди», so an archived row
  looked like a stuck one.
- `statusLabel` in `lims-view.ts` now says «отправлено (заменено повторным
  закрытием)». `superseded` is an archived state, never a queue state — only
  `pending` means "waiting".

## 1.1.5 — Telegram-уведомление о повторном закрытии заявки (2026-11-06)

Live complaint: a request is closed, the Telegram channel gets «✅ Выполнена»; the
request is sent back for rework and closed again — **the second message never comes**.

- **Root cause (server, `lab-service/telegram_notify.go`).** The trigger fired both
  times (`handleSetRequestStatus` / `handleKanbanMove` / `refresh` paths all call
  `triggerTelegramCompleted` only on a real `→ completed` transition), but the
  enqueue was swallowed by the outbox dedup: partial unique index
  `uq_tg_outbox_dest (request_id, event, chat_id, thread_id) WHERE status <> 'failed'`
  plus `ON CONFLICT DO NOTHING`. The first `completed` row stays `sent` forever, so
  the second insert affected zero rows — silently, with no log line.
- **Fix.** New `supersedePreviousCompleted` runs before every `completed` insert:
  already **sent** rows for the same destination become `status='superseded'`
  (they stay in the request's journal, they already reached the channel) and leave
  the partial index, so the dedup now only collapses repeats *within one closure*.
  The decision is the pure function `completedRedeliverEvent(event)` — redelivery is
  enabled for `completed` only: «Создана» happens once per request and stays
  deduplicated forever. An error in the supersede step now logs and skips the
  channel (before, the silent zero-row insert was the whole failure).
- `enqueued` is now derived from `RowsAffected() > 0`, not from "the query did not
  fail" — no pointless worker nudge on a zero-row insert.
- **Why this stayed invisible for a month.** Exactly **seven** requests were closed
  more than once (`1657` three times, `1518/1519/1385/1535/1656/1522` twice), all
  before the fix, and each has exactly **one** `completed` outbox row — the second
  rows were never inserted, so nothing in the journal hinted at a problem. `1657`
  was reopened and closed three times on 2026-11-06 alone (06:36→09:06,
  09:17:46→09:17:52) and still shows one row.
- `1656`/`1657` belong to project `24` (`PVC SAMPLES`), whose own rule covers
  `completed` into `(chat -1002668536527, thread 10)` — so the missing message is
  indeed the dedup, not a rule mismatch. Anything enqueued after the deploy sends
  normally.
- Tests: `TestCompletedRedeliverEventOnlyForCompleted` (completed yes, created and
  unknown no). `go vet`, `go test ./...`, `gofmt -l` — clean.
- **Deployed 2026-11-06 12:26 MSK**: source tar (`lab-service`, `git archive` of
  `backend` + the two edited files) → `/opt/mailers`, backup `lab-service.bak-lab-20261106`,
  `md5sum` of `telegram_notify.go` matched on both sides
  (`b8245424d566924a401734744a8922df`), `docker compose build lab` OK,
  `up -d --force-recreate --no-deps lab`, `CREATED 09:26:57Z`, health `{"status":"ok"}`,
  `sync/pull` without a key `401`, marker strings `superseded` /
  `completedRedeliverEvent` present in the binary, bridge
  `http://portal-embed:3000/internal/telegram/send` answers `403` to a bad secret
  (reachable, not 404). Live closure after the fix not yet observed.
- Plugin version 1.1.4 → **1.1.5** only because the ЦУП news text changed (the
  `announceUpdate` call is version-gated); the TS logic itself is untouched.
  `tsc --noEmit` EXIT=0, `npm run build` OK, the news string is present in `main.js`
  in its escaped (`\uXXXX`) form.

## Статус

Дорожная карта MVP (WP1–WP9) закрыта 2026-08-29: весь план
`docs/superpowers/plans/2026-08-27-mvp-roadmap-plan.md` выполнен, версия при
завершении поднята сразу до **1.0.0**. Снимок статуса на 2026-08-28 и вся история
работ по версиям (с 2026-08-18) — в [архиве плагина](agents_arhiv.md). Текущие
номера версий и правила синхронизации — в корневом `plugins/AGENTS.md`.

## Найденное при разборе настроек методов ГВ и РП (2026-10-27) — к исполнению

Разбор делался для передачи настроек методов другому разработчику. Полное описание
алгоритмов ГГ / ГВ / РП (формулы, границы классификации, формы, протокол) и снимок
базы `lab` на 2026-10-27 лежат **вне проекта**: `C:\Obsidian\mailers\lab_db_dump\`
(`lab-2026-10-27.dump`, `methods-2026-10-27.tsv`, `methods_json/*.json`,
`ОПИСАНИЕ-метод-ГГ.md`, `ОПИСАНИЕ-методы-ГВ-и-РП.md`). Ниже — что по итогам
найдено и требует правок. **Правки согласованы «в ближайшее время», но НЕ
выполнены.**

1. ~~**Бэкап базы `lab` фактически отсутствует (самое срочное).**~~ —
   **infrastructure part closed 2026-10-29; restore rehearsal still pending** (off-hours).
   Before 2026-10-29: the host backup script dumped **only** `mailers`, `lab` was not in
   the script; the S3 push had been failing since late August (`s3.firstvds.ru` did not
   resolve, and the `rclone.conf` key pair got `403 AccessDenied` on `firsts3.ru`); no
   `Backup OK` line in `/var/log/mailers_backup.log`. The only `lab` snapshot was
   `lab-2026-10-27.dump`.
   - **Done 2026-10-29, working hours, without stopping or restarting any container**
     (`pg_dump` does not pause a service): backups run from the host script (`backup.sh`)
     (host file, never in git; old copy — `backup.sh.bak-20261029`), `rclone.conf` rewritten
     to `https://firsts3.ru` with the application storage keys from the stack environment file
     (old copy — `rclone.conf.bak-20261029`).
   - Script shape: a `container|db` table (12 entries — `mailers`, `lab`, `auth`,
     `documents`, `offers`, `ekn`, `contacts`, `agent`, `photo`, `kb`, `presentations`,
     `llm`), `pg_dump -Fc` (no gzip pipe: the exit code is no longer lost), a
     `pg_restore -l` integrity gate plus a 1 KB floor (below it a dump is rejected; the
     `presentations` database is legitimately 3.2 KB — one table, 11 rows), an honest
     `Backup OK (12/12 dbs, off-site copies ok: 2/2)` / `Backup FAILED ...` line with a
     non-zero exit, rotation that always runs, and a single-instance lock.
   - Two destinations on the same keys: `mailers-backup` and the new `mailers-backup2`.
     Cron stays `30 2 * * *`, now wrapped in `flock -n /var/lock/mailers_backup.cron`.
   - Manual full run 2026-10-29 09:27: 12/12 dumps passed the gate, 2/2 copies landed,
     both buckets match the local set byte for byte; `lab.dump` = 188 945 776 B, disk 71 %
     used (23 GB free).
   - **Restore rehearsal done 2026-10-29 (evening):** `backup-verify.sh` restores the
     newest `lab.dump` into a throwaway database on the same container, compares
     `SELECT count(*) FROM requests`, and drops the scratch database via `trap`. First run:
     `restored lab: 691 requests, live: 699, 15s`, `restore OK`. It runs on its own cron at
     `0 5 * * *`. The script also passes a negative test — a truncated dump is rejected with
     `verify FAILED: pg_restore rejected the dump` and leaves no scratch database behind.
   - **`email_ingest_pending_results` cleaned 2026-10-29** (item 2), only after the
     rehearsal was green: all 172 rows exported to `pending_results_archive.jsonl`
     (metadata) and `pending_results_archive.attachments.jsonl` (247 MB), uploaded to both
     buckets, then deleted and `VACUUM FULL`-ed. Database 273 MB → 24 MB, table 16 KB.
     A dump taken after the cleanup restores 699 = 699.
   - **Root cause fixed and deployed 2026-10-29** (item 2 source, `lab-service`, branch
     `backend`, commit `fdca239`): one row per `external_id` (unique index +
     `ON CONFLICT DO UPDATE`), age-based archival to
     `email_ingest_pending_results_archive` after `pendingResultMaxAge` (30 days), pure
     `pendingRowDecision(age)` covered by a test, and a migration that collapses existing
     duplicates **before** creating the unique index. Plugin version stays 1.1.4 — no TS
     changed. Deployed by rebuilding the `lab` container; health `ok`, migration applied
     (both `email_ingest_pending_results_pkey` and `uq_email_ingest_pending_external`
     present), backup and restore rehearsal re-run afterwards and green.
   - **Code review of the shipped script, 2026-10-29 (read-only, nothing fixed on the spot).**
     Ten findings, table and the evening batch in the plan, section
     "Review of the shipped `backup.sh`". Two of them can still yield a false `Backup OK`:
     (a) `docker exec printenv` / `pg_dump` run outside a `set +e` window, so a failing
     `docker exec` kills the script through `set -e` with no `Backup FAILED` line and no
     rotation; (b) push success is judged by the number of objects, not by their size, so a
     truncated copy under a correct name passes. Also: hardcoded container names with no
     cross-check against `docker compose ps`, a rotation line logged even when nothing was
     deleted, a `-name '20*'` local sweep wider than the date folders, two different locks
     (script plus cron) and the destination list duplicated across push and rotation.
     `shellcheck` is available neither locally nor on the host, and `crapper`/`dryer` do not
     parse bash — the review is a manual reading, not a tool run.
2. **`email_ingest_pending_results` раздулась до 250 МБ из ~266 МБ всей базы**
   (172 строки, все — с фото в base64, до 6,7 МБ на строку; 236 МБ в
   `attachments`). Причины: (а) повторы — 55 уникальных `external_id` из 172 строк;
   (б) `attempts` у них **34 800+** при `pendingResultMaxAttempts = 20`
   (`email_ingest.go`) — по коду после порога строка не удаляется, а воркер
   продолжает крутить её каждые ~2 мин уже полтора месяца; (в) ни у одного
   `external_id` нет пары в `requests.external_id` (0 из 172) — данные протухли в
   августе. Правка: прекращать повтор (удалять или уводить в архив) по возрасту/
   попыткам; разово вычистить накопленное — **только после починки бэкапа п. 1**.
3. **Метод РП: в форме испытателя длина подписана «см», а в БД и в калибровочной
   кривой — миллиметры** (`operator_form.fields`, label у `length_of_distraction`;
   в `input_parameters` аккуратно написано «мм»). Если введут 20 (см), сервер
   посчитает 20 мм.
4. **Метод РП: длина повреждения пишется текстом в `additional_info`, а в
   `length_of_distraction` стоит 0** — так в 138 из 220 серий (сентябрь—октябрь
   2026). Следствие: почти все заявки РП этого периода дают
   `average_damage_length = 0` → `agg_flam_flow_density = 12` (значение кривой в
   x = 0) → `РП1` и «Соответствует» независимо от реального результата. Старые
   заявки 2025 г. (100–240 в поле) считаются нормально. Правка: разобраться, откуда
   идёт такое заполнение (форма/разбор письма), после исправления — пересчёт
   затронутых заявок.
5. **Выбор калибровки РП зависит от `requests.exp_date`, который почти нигде не
   заполнен**: у ГВ пусто 209 из 209, у РП — 1 из 71 (и та в формате `07.08.2026`,
   который сервер не разбирает — ждёт `YYYY-MM-DD`). Пока калибровка одна
   (28.08.2026) это безвредно, но вторая калибровка начнёт применяться и к старым
   испытаниям при пересчёте. Правка: заполнять дату испытания на заявке в
   `YYYY-MM-DD` (или передавать дату серии).
6. **Метод ГГ: пустая итоговая группа горючести при непустом остальном расчёте.**
   Заявка 1443 (28.09.2026): в единственной серии `temp_of_smog = ""` (каналы
   `tp1…tp4_smog` тоже пустые, `smoke_temp_curve` нет). Цепочка
   `avg(temp_of_smog)` → подгруппа по дыму → `min_grade(...)` рвётся на первом
   звене: `avg_temp_of_smog` не считается, `smoke_temp_combustibility_group` нет,
   `combustibility_group` нет, `target_group_compliance` нет — в протоколе пусто
   в группе и в заключении, остальные подгруппы и их соответствия на месте. В
   логах ГГ за 30 дней 25 пропусков `combustibility_group` и 6 —
   `avg_temp_of_smog` (затронуто 6 заявок). Правка: не давать сохранять серию без
   температуры (или считать `temp_of_smog` из `smoke_temp_curve`, если прибор её
   прислал — такой формулы СЕЙЧАС НЕТ), после — пересчёт заявок.
7. **Метод ГГ: `average_comb_length` назван «Степень повреждения по длине, %», а
   фактически это среднее четырёх длин В САНТИМЕТРАХ** (формула
   `avg(comb_length_1..4)`, пороги классификации 65/85 — тоже в см, расчёт верный).
   Название вводит в заблуждение и уходит в протокол — поправить `input_parameters.name`.
8. **Метод ГГ: подложка «Без подложки» (подсказка формы) против «Без основания»
   (реальные данные).** Условие видимости поля «Способ крепления» —
   `substrate != "Без подложки"`; при значении «Без основание/Без основания» поле
   ошибочно показывается. Правка: добавить вариант в подсказки и в условие либо
   ограничить ввод.
9. **Метод ГГ: единицы давления расходятся** — в форме «мм рт. ст.», в блоке
   протокола «кПа»; в данных и 101 (кПа), и 745/755/757 (мм рт. ст.). Выбрать одну
   единицу, привести форму, протокол и данные.
10. **Метод ГГ: подгруппы сравниваются с ОБЩЕЙ целевой группой, а не с целью по
    своему параметру.** Одно правило классификации пишет 6 выходов (итог + пять
    подгрупп) по схеме `>= цели`; в модели нет отдельной цели на параметр, поэтому
    «по длине — не соответствует» означает «подгруппа по длине ниже ОБЩЕЙ цели», а
    не «ниже колонки таблицы ГОСТа для своей подгруппы». Менять — только по
    решению заказчика (нужна новая модель цели).
11. **Метод ГГ: таблицы «Критерии оценивания» и «Итоговая оценка» в протоколе —
    статические, введены вручную.** При правке порогов классификации их надо
    править отдельно, иначе в протоколе останется старый ГОСТовский текст с
    прежними числами. Там же живут все плейсхолдеры подгрупп: добавили/убрали
    подгруппу — правьте и формулу `min_grade`, и эти две таблицы.
12. **Метод ГГ: `fill_method` у агрегированных показателей стоит `manual` при
    заданной `aggregation`** (`avg_temp_of_smog`, `average_mass_loss`,
    `agg_comb_length`, `agg_burning_drops`, `average_combustion_time`). Это не
    ошибка — значения пишет формула, которую сервер строит из `aggregation`;
    менять на `calculated` не нужно (иначе формула не построится).

## Статистика ошибок и отступлений

- Нарушений правил нет: 0 `any`, 0 `fetch` (только `requestUrl`), 0 инлайн-стилей,
  `window.setTimeout` корректен (в `Promise.race` с очисткой в `finally`),
  все `catch(e: unknown)` + `errorMessage()`.
- `npx tsc --noEmit` EXIT=0, `npm run build` OK (без предупреждений).
- Замечание: `main.ts` использует `JSON.parse(res.text)` в `catch` — нотификация не показывается
  (только `console.warn`), без деградации; результаты из pull загружаются без правок.
