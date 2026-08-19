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
| GET/POST | `/equipment` | viewer / editor+ | `{"equipment":[...]}` / `{code,name,location?,responsible?}` → `{id}` |
| GET/POST | `/lab-members` | admin | `{"members":[{lab_id,email,role}]}` / `{lab_id,email,role}` |
| DELETE | `/lab-members/{lab_id}/{email}` | admin | → `{ok}` |
| PATCH | `/methods/{id}` | admin | `{formulas, classification, chart_configs, input_parameters}` (JSONB, любой поднабор) → `{ok}` |
| GET | `/methods` | viewer | `{"methods":[...]}` (включая конфиги) |

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
             owner_email, status, priority, test_purpose, external_lab_id, ekn,
             method_id, customer_number, lab_number,
             files: RequestFile[], created_at, updated_at }

LabMethod{ id, code, name, lab_id, description, determinable_indicators: string[],
           formulas: any[], classification: any[], chart_configs: any[], input_parameters: any[],
           created_at, updated_at }

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
  median/std), ссылки на параметры из `values`.
- Классификация `methods.classification` — threshold/boolean/compliance + ранги.
- Авто-статистика: при сохранении серии создаётся стат-строка
  (`is_statistical_row=true`, `calculation_type='auto_statistics'`) с avg/count по параметрам.
- План `chart_configs` `[{id, chart_type: line|scatter|bar, x_column, series_config, title, x_label, y_label}]`.

## Статусы заявки

`new` → `received` (образцы получены) → `processing` (испытания, ввод результатов) →
`completed`. Ввод/расчёт результатов доступен в `received`/`processing`. sbe-requests
показывает те же статусы.

## Роли

`viewer`(1) < `editor`(2) < `admin`(3) [+ `superadmin`(4) — проектируется] + лабораторный
скоуп: `lab_members(lab_id, email, role)` — `lab_operator`/`lab_admin` [+ `lab_auditor` —
проектируется]. Сотрудник лаборатории видит заявки своих методов. Ввод результатов —
lab_operator+. Справочники испытателей/оборудования — editor+; методы-конфиги и
`lab-members` — admin. Будущая модель прав (согласована 2026-08-18): superadmin создаёт
лаборатории и админов лабораторий; lab_admin управляет своей лабой (участники, методы,
админы); lab_operator — испытания/ввод данных/движение заявки; lab_auditor — чтение всех
разделов своей лабы.

## UI

- Вьюха «Лабораторная информационная менеджмент система СБЕ ПМиПИР» (тип `sbe-lims-view`,
  иконка flask-conical), **фасад** (v0.1.1):
  - **Шапка** 54px: титул модуля, crumb `{лаба} · {раздел}`, справа «＋ Создать».
  - **Сайдбар-карточка** (320px, сворачивается в 64px): переключатель лабораторий
    (из `GET /api/lab/labs`; скрыт, если лаборатория одна) + дерево навигации:
    - «Заявки»: Все заявки, Очередь лаборатории;
    - «Лаборатория»: Методы, Объекты, Результаты и протоколы, Испытатели, Оборудование,
      Сотрудники.
  - **Контент-карточка**: заголовок раздела + подзаголовок. Пункт «Все заявки» наполнен
    (2026-08-19): таблица заявок → карточка (статус, ввод серии результатов, расчёт, таблица
    серий, графики, генерация протокола+DOCX). Остальные пункты — **заглушка** (наполняются
    следующими этапами).
- **Дашборд из плагина удалён** (2026-08-18): таб и метод отсутствуют; отдельный плагин-дашборд
  будет использовать серверный `GET /dashboard`. Серверный эндпоинт не удалять.
- Настройки: `apiUrl` + раздел «Права доступа» (роли + общий доступ, admin).
- **Запланировано (наполнение)**: подключить к узлам дерева заявки (список+карточка),
  очередь, методы, объекты, результаты/протоколы, справочники; затем бэкенд прав
  (superadmin/lab_auditor/фильтрация `/labs`).

## Локальные данные

Постоянного БД-кэша нет: методы кэшируются в памяти плагина (`refreshMethods` → pull при
onload), заявки/результаты грузятся при открытии вьюхи. Локальные изменения — только через
сервер (сервер — канон).
