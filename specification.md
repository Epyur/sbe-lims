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
| PATCH | `/methods/{id}` | admin | `{formulas, classification, chart_configs, input_parameters, presentation, lab_ids?, description?}` (JSONB-поля — любой поднабор; `lab_ids`, если передан, полностью заменяет набор лабораторий; `description` — COALESCE, не перетирает при отсутствии; **`formulas` игнорируется, если передан `input_parameters`** — сервер перестраивает `formulas[]` целиком из атрибутов, см. модели ниже) → `{ok}` |
| DELETE | `/methods/{id}` | admin | → `{ok}`; 409, если метод используется в заявках/справочниках |
| GET | `/methods` | viewer | `{"methods":[...]}` (включая конфиги) |
| GET | `/objects` | viewer | `{"objects":[...]}` — только чтение в sbe-lims, создание в sbe-requests |
| GET | `/projects` | viewer | `{"projects":[...]}` — только чтение в sbe-lims (2026-08-21, для карточки заявки — название проекта, как у заявителя), создание/правка в sbe-requests |
| GET | `/groups` | viewer | `{"groups":[...]}` — только чтение в sbe-lims (2026-08-21, для карточки заявки — название группы, как у заявителя), создание в sbe-requests |

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
           formulas: any[],  // строится сервером из input_parameters — не редактируется напрямую (2026-08-21)
           classification: ClassificationRule[], chart_configs: ChartConfig[],
           input_parameters: MethodAttribute[], presentation: MethodPresentation,  // 2026-08-21, блок 3
           created_at, updated_at }

// Конфигуратор методов (2026-08-21) — структурированные формы вместо raw JSON:

// Атрибут метода (input_parameters) — единственный источник formulas, привязанных к
// атрибуту (сервер перестраивает formulas[] из этого списка при каждом сохранении).
MethodAttribute{ id: string,  // ^[A-Za-z_][A-Za-z0-9_]*$, уникален в пределах метода
                  name: string,
                  data_type: 'text'|'int'|'float'|'date'|'time'|'photo'|'boolean',  // boolean: true/false в БД, Да/Нет в UI (2026-08-22)
                  fill_method: 'manual'|'instrument'|'calculated',
                  level: 'experiment'|'aggregated',
                  formula?: string,  // DSL-выражение — для fill_method="calculated"
                  aggregation?: { source: string, method: 'avg'|'min'|'max' },  // level="aggregated" без своей формулы
                  synonyms?: string[] }  // альтернативные raw-имена для email-импорта (resolveResultKey, lab-service)

// Правило классификации (2026-08-22v2, единая модель — заменяет прежние три
// rule_type: threshold/boolean/compliance): "Если А [знак] Б (И/ИЛИ …), то
// output_name = grade" — как в Excel IF/AND/OR. branches проверяются ПО ПОРЯДКУ
// (сервер не пересортировывает), первая совпавшая — результат.
Operand = { kind: 'attribute', id: string } | { kind: 'literal', value: string|number } | { kind: 'target_indicator' }
          // target_indicator — objects.characteristics.target_indicators[methodId] заявки
ClassificationClause{ left: Operand, operator: '=='|'!='|'<'|'<='|'>'|'>=', right: Operand }
ClassificationBranch{ clauses?: ClassificationClause[],  // пусто/не задано — безусловная ветка "Иначе"
                       join?: 'and'|'or',  // как связаны МЕЖДУ СОБОЙ несколько clauses (по умолч. "and")
                       grade: string }
ClassificationRule{ output_name: string,
                     aggregation_rule?: 'none'|'avg'|'min'|'max',  // при нескольких сериях; "none" (по умолч.) — не сравнивать между сериями
                     branches: ClassificationBranch[] }
// Термины "лучше"/"хуже" в проекте не используются (оценочные категории неприменимы к
// объективной оценке результатов испытаний) — только "выше/ниже", "больше/меньше",
// "максимальное/минимальное" (см. AGENTS.md, раздел «Правила»).

// Представление данных (presentation, блок 3) — порядок/подписи/видимость столбцов в
// UI-таблице результатов и в протоколе; порядок элементов fields = порядок столбцов.
PresentationField{ attribute_id: string, label?: string, show_in_ui: boolean, show_in_protocol: boolean }
MethodPresentation{ fields: PresentationField[] }
// Атрибуты, не упомянутые в fields, — показываются как раньше (все ключи без явного
// порядка) — обратная совместимость для не сконфигурированных в блоке 3 методов.

// График (chart_configs) — рендерится сервером в PNG (charts.go), без внешних зависимостей.
ChartConfig{ id: string, title?: string, chart_type: 'line'|'scatter'|'bar',
             x_column?: string, x_label?: string, y_label?: string,
             series_config: Array<{source_param: string, label?: string}> }

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
MethodConfig{ formulas: any[], classification: ClassificationRule[], chart_configs: ChartConfig[],
              input_parameters: MethodAttribute[], presentation: MethodPresentation }
ProtocolResponse{ html, docx_base64, generated_at }
// DashboardData удалён из клиента (2026-08-18) — см. серверный ответ /dashboard
```

## Расчёт на сервере (DSL)

- Формулы `methods.formulas` `[{expression, target_parameter, apply_level}]` —
  безопасный интерпретатор (арифметика, сравнения, if/else; ссылки на параметры из
  `values`). **Формируется сервером из `input_parameters` при каждом сохранении
  конфига** (2026-08-21, `deriveFormulasFromAttributes`) — клиент не редактирует
  `formulas` напрямую. `apply_level: "series"` — к текущей серии; `apply_level:
  "aggregated"` — один раз по всем сериям заявки+метода, пишется в
  `aggregated_results` (`calculation_type: "formula_aggregated"`).
  Агрегации: `avg`/`min`/`max`/`sum`/`count`/`median`/`std` — один аргумент-идентификатор
  агрегирует по сериям; несколько аргументов (`avg(a,b,c)`) — среднее нескольких
  атрибутов ОДНОЙ серии (2026-08-21). Плюс: `min_grade`/`max_grade` (2026-08-21,
  переименовано из `worst_grade`/`best_grade` 2026-08-22 — см. AGENTS.md «Правила»;
  сравнение показателей по порядку `determinable_indicators` метода, индекс 0 —
  максимальный/больше остальных), `interpolate(x, xs, ys)` (линейная интерполяция по
  параллельным атрибутам-массивам), `agg_where('avg'|'min'|'max'|'sum', value_attr,
  cond_attr, 'значение')` (агрегат по сериям, где условие выполняется).
- Классификация `methods.classification` (2026-08-22v2, единая модель — см. модели
  выше): каждое правило — `branches[]`, проверяемые по порядку, первая совпавшая
  ветка пишет `grade` в `output_name`. Ветка — `clauses[]` (атомарные "Operand
  [знак] Operand"), объединённые `join` ("and"/"or"); пустой `clauses` — безусловная
  ветка "Иначе". Рекомендуемый паттерн для соответствия целевому показателю: две
  ветки `achieved >= target_indicator` / `achieved < target_indicator` плюс
  catch-all "Не оценивается". `aggregation_rule="none"` (по умолчанию) — не сравнивать
  между сериями, брать значение текущей записи как есть; `avg`/`min`/`max` — свести
  числовые значения атрибута по всем сериям заявки+метода.
- Авто-статистика: при сохранении серии создаётся стат-строка
  (`is_statistical_row=true`, `calculation_type='auto_statistics'`) с avg/count по параметрам.
- `chart_configs` `[{id, chart_type: line|scatter|bar, x_column, series_config, title, x_label, y_label}]`
  — рендерится в PNG (`charts.go`, без внешних зависимостей); редактор — структурная
  форма в клиенте (было raw JSON).
- **Порядок столбцов протокола/UI-таблицы** (2026-08-21) — из `methods.presentation.
  fields` (см. модели выше); для методов без presentation — алфавитный порядок
  найденных ключей (детерминированный фолбэк, раньше был случайный обход `map`
  в Go — баг, исправлен как побочный эффект блока 3). «Фотография»-атрибуты — превью
  `<img>` в HTML-протоколе и в UI-таблице; в DOCX — текст/ссылка (DOCX-writer не
  встраивает изображения).

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
      (`completed`) — один и тот же список (таблица) с разным фильтром по статусу,
      всегда отсортирован новыми сверху (`number_year` убыв., затем `number_seq`
      убыв. — 2026-08-21, тот же порядок, что в sbe-requests) → карточка
      заявки (статус, ввод серии результатов, расчёт, таблица серий, графики, генерация
      протокола+DOCX). Номер заявки — **сокращённый, лабораторный** — `lab_number`
      (`{NNN}/{yyyy}-{methodCode}`), не полный номер заказчику. Карточка заявки
      (2026-08-21) показывает все те же детали, что видит заявитель в sbe-requests —
      номер заказчику, проект, группа, объект, ЕКН, внешний идентификатор (если есть,
      с восстановленным префиксом `LPIZAYAVKINAPRO-`), характеристики объекта
      (номер партии/идентификатор образца/**целевой показатель**, 2026-08-21 —
      выбранное в sbe-requests значение для метода этой заявки), приоритет,
      цель испытания, лаборатория, владелец, даты создания/обновления, статус, описание —
      единым блоком сразу под номером.
    - «Методы» — список из кэша, **не фильтруется по текущей лабе** (2026-08-19: метод
      может принадлежать нескольким/внешней лабе, список показывает все методы, у
      каждого — строка «Лаборатории: …» с именами всех его лаб); форма создания
      (код/название/описание/чекбоксы лабораторий/показатели, admin); удаление (admin,
      409 при использовании). **Конфигуратор метода** (кнопка «✎ Конфиг», admin,
      2026-08-21 — структурные построчные редакторы вместо raw JSON):
      - **«📄 Импортировать из стандарта (ИИ)»** — загрузка `.rtf`/`.txt` текста
        стандарта → черновик атрибутов+правил+представления от ИИ (`sbe-llm`,
        модель — настройка плагина, см. ниже) заполняет редакторы ниже для ревью
        (никогда не сохраняется автоматически).
      - **Атрибуты метода** (`input_parameters`) — построчно: id/название/тип
        данных/способ заполнения/уровень; для расчётных — формула (DSL) + «🤖 ИИ»
        (предложение формулы по описанию); для агрегированных без формулы —
        атрибут-источник + принцип; кнопка «🔗 синонимы» — альтернативные raw-имена
        для сопоставления с email-импортом десктопной ЛИМС.
      - **Правила классификации** — построчно: пороговое/булево/«соответствие
        целевому показателю».
      - **Представление данных** — drag-and-drop порядок/подписи/видимость столбцов
        (UI-таблица результатов + протокол) с живым предпросмотром.
      - **Графики** (`chart_configs`) — карточки тип/оси/подписи/ряды.
      - `formulas` — только просмотр (строится сервером из атрибутов).
    - «Объекты» — список, только чтение (создание — в sbe-requests); у каждой строки
      кнопка **«→ Заявки»** (2026-08-21) — переход к списку заявок этого объекта
      (понадобилось после объединения дублей объектов в lab-service, 469 → 134,
      у объекта-плейсхолдера «Без названия» — 129 заявок).
    - «Испытатели» / «Оборудование» — список + создание/правка/удаление, editor+.
      Правка — инлайн-форма по месту строки таблицы (кнопка «✎»), без перехода;
      удаление — кнопка «✖» с подтверждением, 409 от сервера (используется в
      результатах/методе) показывается как обычная ошибка.
    - «Сотрудники» — список (по текущей лабе) + добавление/удаление, admin (раздел
      скрыт для не-admin — `GET /lab-members` сам admin-only на сервере).
- **Дашборд из плагина удалён** (2026-08-18): таб и метод отсутствуют; отдельный плагин-дашборд
  будет использовать серверный `GET /dashboard`. Серверный эндпоинт не удалять.
- Настройки: `apiUrl`, **`llmModel`** (2026-08-21, дефолт `gpt-5.6-luna` — модель для
  `sbe-llm`, которая сама модель не хранит, см. `sbe-mailer`/`sbe-presentations` — тот же
  паттерн) + раздел «Права доступа» (роли + общий доступ, admin).
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
