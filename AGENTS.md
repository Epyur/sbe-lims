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

## Статус

Дорожная карта MVP (WP1–WP9) закрыта 2026-08-29: весь план
`docs/superpowers/plans/2026-08-27-mvp-roadmap-plan.md` выполнен, версия при
завершении поднята сразу до **1.0.0**. Снимок статуса на 2026-08-28 и вся история
работ по версиям (с 2026-08-18) — в [архиве плагина](agents_arhiv.md). Текущие
номера версий и правила синхронизации — в корневом `plugins/AGENTS.md`.

## Статистика ошибок и отступлений

- Нарушений правил нет: 0 `any`, 0 `fetch` (только `requestUrl`), 0 инлайн-стилей,
  `window.setTimeout` корректен (в `Promise.race` с очисткой в `finally`),
  все `catch(e: unknown)` + `errorMessage()`.
- `npx tsc --noEmit` EXIT=0, `npm run build` OK (без предупреждений).
- Замечание: `main.ts` использует `JSON.parse(res.text)` в `catch` — нотификация не показывается
  (только `console.warn`), без деградации; результаты из pull загружаются без правок.
