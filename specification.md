# specification.md — sbe-lims (ЛИМС)

SBE-плагин «ЛИМС» для сотрудников лабораторий. Клиент lab-service (сервер — канон).
База URL по умолчанию: `https://epyur.fvds.ru`. JWT для `app_id=lab` берётся из ЦУП СБЕ
(`getService('sbe-apstore').auth.getToken('lab')`).

## API (lab-service, `/api/lab/*`)

### Заявки и статусы

| Метод | Путь | Роль | Тело / ответ |
|---|---|---|---|
| GET | `/requests` | viewer | `{"requests":[...]}` (только видимые: владелец / участник группы / admin / сотрудник лабы по `lab_members`) |
| GET | `/requests/{id}` | viewer | `{"request":{...}}`; 403 если не видно |
| POST | `/requests/{id}/status` | editor+ | `{status}`: `new`/`received`/`processing`/`completed` → `{ok}` |

### Результаты и расчёт

| Метод | Путь | Роль | Тело / ответ |
|---|---|---|---|
| GET | `/requests/{id}/results` | viewer | `{"results":[MeasurementResult]}` (в т.ч. стат-строки `is_statistical_row=true`) |
| POST | `/requests/{id}/results` | lab_operator+/editor+ | `{method_id, inventor_id, series_num, values:{параметр:значение}, photo_before?, photo_after?}` → `{id, series_num, values}`; при сохранении сервер выполняет DSL/классификацию/статистику и пишет стат-строку |
| GET | `/requests/{id}/results/aggregated` | viewer | `{"aggregated":[AggregatedResult]}` |
| POST | `/requests/{id}/results/{series}/calculate` | lab_operator+ | пересчёт формул/классификации серии → `{ok}` |

### Справочники (ЛИМС)

| Метод | Путь | Роль | Тело / ответ |
|---|---|---|---|
| GET/POST | `/inventors` | viewer / editor+ | `{"inventors":[...]}` / `{name,email,phone?,department?,position?}` → `{id}` |
| PATCH/DELETE | `/inventors/{id}` | editor+ | частичный `{name?,email?,phone?,department?,position?}` → `{ok}` / → `{ok}`; DELETE 409, если испытатель уже есть в результатах |
| GET/POST | `/equipment` | viewer / editor+ | `{"equipment":[...]}` / `{code,name,location?,responsible?}` → `{id}` |
| PATCH/DELETE | `/equipment/{id}` | editor+ | частичный `{code?,name?,location?,responsible?}` → `{ok}` / → `{ok}`; DELETE 409, если используется методом |
| GET/POST | `/lab-members` | admin | `{"members":[{lab_id,email,role}]}` / `{lab_id,email,role}` |
| DELETE | `/lab-members/{lab_id}/{email}` | admin | → `{ok}` |
| POST | `/methods` | admin | `{code,name,lab_ids: number[],description?,determinable_indicators?}` → `{id}` (минимум одна лаба; метод может принадлежать нескольким, 2026-08-19) |
| PATCH | `/methods/{id}` | admin | `{formulas, classification, chart_configs, input_parameters, lab_ids?, description?}` (JSONB-поля — любой поднабор; `lab_ids`, если передан, полностью заменяет набор лабораторий; `description` — COALESCE, не перетирает при отсутствии) → `{ok}` |
| DELETE | `/methods/{id}` | admin | → `{ok}`; 409, если метод используется в заявках/справочниках |
| GET | `/methods` | viewer | `{"methods":[...]}` (включая конфиги) |
| GET | `/objects` | viewer | `{"objects":[...]}` — только чтение в sbe-lims, создание в sbe-requests |

### Графики / протокол / дашборд

| Метод | Путь | Роль | Ответ |
|---|---|---|---|
| GET | `/requests/{id}/chart/{cfg_id}` | viewer | PNG (рендер по `methods.chart_configs`), используется в `<img src>` |
| POST | `/requests/{id}/protocol` | editor+ | `{html, docx_base64, generated_at}` |
| GET | `/dashboard?period=week\|month\|quarter\|year` | viewer | `{by_status, by_method:[{method_id,count}], total, completed_in_period, period}` — ⚠️ **HЕ используется плагином** (дашборд вынесен, 2026-08-18), эндпоинт оставлен на сервере |

### Общие (все плагины lab-service)

`/health`, `/sync/pull`, `/sync/push`, `/file` (S3), `/permissions/me`, `/permissions`,
`/common-access` — см. `sbe-requests/specification.md`.

## Модели (JSON, соответствуют Go-структурам lab-service)

```ts
// 1 заявка = 1 метод (декомпозиция 2026-08-18, см. lab-service/AGENTS.md) — method_id/
// customer_number/lab_number прямо на заявке, без массива methods[].
LimsRequest{ id, number_seq, number_year, title, description, object_id, project_id, group_id,
             owner_email, status, priority, test_purpose, ekn,
             external_id,  // номер legacy email-трекера («LPIZAYAVKINAPRO-<N>»); у новых заявок пусто
             method_id,
             lab_id,  // конкретная лаба из method.lab_ids, зафиксирована при создании заявки
                       // (заменяет старую external_lab_id — упразднена 2026-08-19)
             customer_number, lab_number,
             files: RequestFile[], created_at, updated_at }

LabMethod{ id, code, name,
           lab_ids: number[],  // может принадлежать нескольким лабам (method_labs, 2026-08-19)
           description, determinable_indicators: string[],
           formulas: any[], classification: any[], chart_configs: any[], input_parameters: any[],
           created_at, updated_at }

LabObject{ id, name, description, characteristics: Record<string, unknown>, created_at, updated_at }

MeasurementResult{ id, request_id, method_id, inventor_id, series_num,
                   values: Record<string, unknown>, file_links: Record<string, unknown>,
                   photo_before, photo_after, is_statistical_row: boolean,
                   calculation_type, source_series_count, source_series_range,
                   created_at, updated_at }

AggregatedResult{ id, request_id, method_id, calculation_type, result_data,
                  source_series_count, source_series_range, created_at, updated_at }

Inventor{ id, name, email, phone, department, position, created_at, updated_at }
Equipment{ id, code, name, location, responsible, last_calibration, next_calibration,
           status, created_at, updated_at }
LabMember{ lab_id, email, role: 'lab_operator' | 'lab_admin' }
MethodConfig{ formulas, classification, chart_configs, input_parameters }   // массивы JSON
ProtocolResponse{ html, docx_base64, generated_at }
// DashboardData удалён из клиента (2026-08-18) — см. серверный ответ /dashboard
```

## Расчёт на сервере (DSL)

- Формулы `methods.formulas` `[{id, expression, target_parameter, apply_level, order}]` —
  безопасный интерпретатор (арифметика, сравнения, if/else, агрегации avg/min/max/sum/count/
  median/std), ссылки на параметры из `values`. `apply_level: "series"` — как обычно,
  к текущей серии; `apply_level: "aggregated"` — считается один раз по всем сериям
  заявки+метода и пишется в `aggregated_results` (`calculation_type: "formula_aggregated"`,
  fixed 2026-08-19 — до этого игнорировался и применялся как `series`, см. lab-service/AGENTS.md).
- Классификация `methods.classification` — threshold/boolean/compliance + ранги.
- Авто-статистика: при сохранении серии создаётся стат-строка
  (`is_statistical_row=true`, `calculation_type='auto_statistics'`) с avg/count по параметрам.
- План `chart_configs` `[{id, chart_type: line|scatter|bar, x_column, series_config, title, x_label, y_label}]`.

## Статусы заявки

`new` → `received` (образцы получены) → `processing` (испытания, ввод результатов) →
`completed`. Ввод/расчёт результатов доступен в `received`/`processing`. sbe-requests
показывает те же статусы.

## Роли

`viewer`(1) < `editor`(2) < `admin`(3) < `superadmin`(4) + лабораторный скоуп:
`lab_members(lab_id, email, role)` — `lab_operator`/`lab_admin`/`lab_auditor`.
Сотрудник лаборатории видит заявки своих методов (`requestVisible`/`visibleRequestsQuery`,
условие по `lab_members`, реализовано 2026-08-19). Чтение результатов/графиков/протокола —
lab_operator/lab_admin/**lab_auditor**/app-admin+ (`requireLabRead`); запись (ввод серии,
расчёт) — lab_operator/lab_admin/app-admin+ (`requireLabAccess`, auditor не допускается).
Справочники испытателей/оборудования — editor+; методы-конфиги и `lab-members` — admin+.
`GET /labs` — admin/superadmin видят все; остальные — лабы со своей строкой в
`lab_members`, плюс внешние лабы, у которых `parent_lab_id` — одна из своих (внешняя
лаба своих `lab_members` не имеет по определению). `POST /labs` / `PATCH /labs/{id}`
(создание/правка лабораторий) — только superadmin; внешняя (`type=external`)
**обязана** указать `parent_lab_id` существующей внутренней лабы (400 без него,
и той же валидацией на PATCH) — внешняя лаба не существует самостоятельно. Она может
иметь свои методы, отсутствующих у внутренней (расширяет её
возможности); видимость таких заявок и доступ к результатам/графикам/протоколу
резолвятся через `parent_lab_id` (`requestVisible`/`visibleRequestsQuery`/
`requestLabID` берут `COALESCE(l.parent_lab_id, l.id)`), т.е. фактически «попадают» к
сотрудникам родительской внутренней лабы. `lab_members` можно завести только для
внутренней лабы (400 на попытке для внешней). Назначать/снимать роль `superadmin`
может только действующий superadmin (`handleSetPermission`). Владелец
(`LAB_OWNER_EMAIL`) при каждом старте сервиса гарантированно становится superadmin
(`seedOwner`, `DO UPDATE`).
⚠️ **Не реализовано**: делегированные полномочия `lab_admin` внутри своей лабы без
app-level admin (добавление участников, правка методов своей лабы) — `lab_admin`
сегодня равен `lab_operator` по факту прав. ⚠️ **Известный пробел**: смена статуса
заявки (`POST /requests/{id}/status`) проверяет только видимость, не write-право.

## UI

- Вьюха «LogicLAB.ЛИМС» (2026-08-21, было «Лабораторная информационная менеджмент
  система СБЕ ПМиПИР» — та же схема брендинга, что в sbe-requests, «LogicLAB.Заявки»)
  (тип `sbe-lims-view`, иконка flask-conical), **фасад** (v0.1.1):
  - **Шапка** 54px: титул модуля, crumb `{лаба} · {раздел}`. Кнопки «＋ Создать» в
    шапке больше нет (2026-08-19) — была facade-заглушкой без действия с v0.1.1,
    создание везде подключено кнопками внутри самих разделов.
  - **Сайдбар-карточка** (320px, сворачивается в 64px): переключатель лабораторий
    (из `GET /api/lab/labs`; скрыт, если лаборатория одна) + дерево навигации:
    - «Заявки»: Все заявки, Очередь лаборатории;
    - «Лаборатория»: Методы, Объекты, Результаты и протоколы, Испытатели, Оборудование,
      Сотрудники.
    Сразу под деревом — кнопка **«🔄 Синхронизация»** (2026-08-21, аналог одноимённой
    кнопки в sbe-requests): перезапрашивает роль, лаборатории и кэш методов с сервера,
    перерисовывает текущую страницу — для оперативного обновления без переоткрытия
    вьюхи.
    Внизу сайдбара (после дерева, `flex:1` у дерева прижимает блок к низу) — кнопка
    **«⚙ Настройки»** (admin+; видна независимо от роли, но контент внутри гейтится
    по разделам), открывает страницу **«Настройки»** в контенте (не в сайдбаре —
    2026-08-19, было там до переноса):
    - Список лабораторий с правкой («✎ Изменить», только superadmin) + «➕ Новая
      лаборатория» (только superadmin, `POST`/`PATCH /api/lab/labs`).
    - Под каждой **внутренней** лабой — список её администраторов (`lab_members`
      `role=lab_admin`) + назначение нового по e-mail / снятие (admin+, те же
      `setLabMember`/`removeLabMember`, что использует «Сотрудники»). У внешних
      лаб этого блока нет — `lab_members` не заводится (нет пользователей системы).
  - **Контент-карточка**: заголовок раздела + подзаголовок; **все 8 пунктов дерева
    наполнены** (2026-08-19):
    - «Все заявки» / «Очередь лаборатории» (`new`/`received`) / «Результаты и протоколы»
      (`completed`) — один и тот же список (таблица) с разным фильтром по статусу → карточка
      заявки (статус, ввод серии результатов, расчёт, таблица серий, графики, генерация
      протокола+DOCX). Номер заявки — **сокращённый, лабораторный** — `lab_number`
      (`{NNN}/{yyyy}-{methodCode}`), не полный номер заказчику.
    - «Методы» — список из кэша, **не фильтруется по текущей лабе** (2026-08-19: метод
      может принадлежать нескольким/внешней лабе, список показывает все методы, у
      каждого — строка «Лаборатории: …» с именами всех его лаб); форма создания
      (код/название/описание/чекбоксы лабораторий/показатели, admin), JSON-редактор
      конфигов (formulas/classification/chart_configs/input_parameters + описание +
      те же чекбоксы лабораторий, admin), удаление (admin, 409 при использовании).
    - «Объекты» — список, только чтение (создание — в sbe-requests).
    - «Испытатели» / «Оборудование» — список + создание/правка/удаление, editor+.
      Правка — инлайн-форма по месту строки таблицы (кнопка «✎»), без перехода;
      удаление — кнопка «✖» с подтверждением, 409 от сервера (используется в
      результатах/методе) показывается как обычная ошибка.
    - «Сотрудники» — список (по текущей лабе) + добавление/удаление, admin (раздел
      скрыт для не-admin — `GET /lab-members` сам admin-only на сервере).
- **Дашборд из плагина удалён** (2026-08-18): таб и метод отсутствуют; отдельный плагин-дашборд
  будет использовать серверный `GET /dashboard`. Серверный эндпоинт не удалять.
- Настройки: `apiUrl` + раздел «Права доступа» (роли + общий доступ, admin).
- **Реализовано 2026-08-19**: бэкенд ролей (superadmin/lab_auditor/фильтрация `/labs`) —
  см. раздел «Роли» выше.
- **Запланировано**: делегированные полномочия `lab_admin` внутри своей лабы (не
  реализовано — см. «Роли»); точная клиентская проверка `lab_operator`/`lab_admin`/
  `lab_auditor` per-лаба (нужен новый «моя роль в этой лабе» эндпоинт — сейчас
  `canEdit`/`canEditStatus` для результатов/статуса заявки разрешают всё любой
  app-роли, сервер всё равно валидирует по `requireLabAccess`/`requireLabRead`).

## Локальные данные

Постоянного БД-кэша нет: методы кэшируются в памяти плагина (`refreshMethods` → pull при
onload), заявки/результаты грузятся при открытии вьюхи. Локальные изменения — только через
сервер (сервер — канон).
