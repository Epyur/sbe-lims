# AGENTS.md — sbe-lims (ЛИМС)

SBE-плагин «ЛИМС» для сотрудников лабораторий: заявки лаборатории (по `lab_members`),
результаты испытаний по сериям, расчёты (DSL-формулы), классификация, статистика, графики,
протоколы (HTML/docx), справочники лаборатории, дашборд. Клиент lab-service
(сервер — канон).

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
- **Справочники**: методы (конфиги formulas/classification/chart_configs/input_parameters,
  JSON-редактор, PATCH admin), испытатели (CRUD editor+), оборудование (CRUD editor+),
  сотрудники лабораторий (`lab-members`, admin).
- **Графики**: `GET /requests/{id}/chart/{cfg_id}` → PNG (сервер рендерит по
  `methods.chart_configs`), показываются в карточке заявки.
- **Протокол**: `POST /requests/{id}/protocol` → `{html, docx_base64}`; HTML в модальном
  окне, DOCX скачивается.
- **Дашборд**: `GET /dashboard?period=` (неделя/месяц/квартал/год) — статусы, методы, итоги.
- **Точка входа** — магазин: «Установленные → Открыть» (`publishService('sbe-lims', {open})`).
- Вид открывается из ЦУП (`SbeLimsApi extends SbeOpenableApi`), без ribbon.

## Структура

| Файл | Что это |
|---|---|
| `src/main.ts` | `SbeLimsPlugin`: настройки, syncService, refreshMethods (pull методов при onload), view, publishService |
| `src/services/sync.service.ts` | `LimsSyncService`: JWT из ЦУП, заявки/результаты/расчёт/справочники/графики/протокол/дашборд, permissions/me, таймауты 30с, понятные 401/403 |
| `src/ui/lims-view.ts` | `LimsView` (тип `sbe-lims-view`): вкладки Заявки / Справочники / Дашборд; карточка заявки (серии, статус, графики, протокол) |
| `src/ui/settings-tab.ts` | Настройки: apiUrl + «Права доступа» (роли + общий доступ) |
| `src/types/lims.ts` | `LimsRequest`, `LabMethod`, `MeasurementResult`, `AggregatedResult`, `Inventor`, `Equipment`, `LabMember`, `MethodConfig`, `ProtocolResponse`, `DashboardData` |
| `src/styles.css` | Классы `tn-lims-*` на семантических токенах |

## Настройки (data.json)

`apiUrl` (default `https://epyur.fvds.ru`). Роли lab: `viewer`(1) < `editor`(2) < `admin`(3)
+ лабораторный скоуп через `lab_members` (lab_operator/lab_admin).

## Правила

- `catch(e: unknown)` + `errorMessage()`; `requestUrl()` (в `Promise.race` с
  `window.setTimeout`); без `any`; UI на русском; автор — Полищук Евгений (polishchuk@tn.ru).
  Классы `tn-lims-*` / `tn-btn*` / `tn-table` на семантических токенах sbe-core.
- Коммиты/пуши — только по явной команде пользователя.
- **«Фиксируй» = поднять версию (+0.0.1 в `manifest.json` и `package.json`), обновить
  документацию, подготовить сообщение для коммита и СПРОСИТЬ подтверждение commit/push.**

## История работ

### 2026-08-18 — v0.1.0 (создание, Этап 5-6 плана 2026-08-18-sbe-lims-plan)
- Скаффолд как sbe-requests. `LimsSyncService` (JWT из ЦУП app `lab`, заявки, результаты,
  расчёт, справочники испытателей/оборудования/сотрудников, конфиги методов PATCH,
  графики PNG, протокол HTML+docx, дашборд, permissions/me, таймауты 30с, 401/403-сообщения).
  `LimsView`: вкладки «Заявки» (список + карточка: серии, статус, графики, протокол),
  «Справочники» (методы с конфигами JSON, испытатели, оборудование, сотрудники лабораторий),
  «Дашборд». `refreshMethods()` при onload (pull методов из sync/pull). Settings (apiUrl +
  «Права доступа»). `publishService('sbe-lims', {open})`.
- `sbe-core`: добавлены `SbeLimsApi` (extends `SbeOpenableApi`), `'sbe-lims'` в
  `SbeServiceMap` и `getServiceName` → «ЛИМС».
- `npx tsc --noEmit` EXIT=0; `npm run build` OK (main.js 31KB, styles.css 19KB — склеен из
  tokens/components/own через build.onEnd).
- ⏳ Не сделано: AGENTS.md/specification.md (этот файл — первая версия), реестр `sbe-lims`,
  `community-plugins.json`, git-репо `Epyur/sbe-lims` — по команде. E2E в Obsidian (открытие
  из ЦУП, результаты, графики, протокол) — после включения плагина.

## Статистика ошибок и отступлений

- Нарушений правил нет: 0 `any`, 0 `fetch` (только `requestUrl`), 0 инлайн-стилей,
  `window.setTimeout` корректен (в `Promise.race` с очисткой в `finally`),
  все `catch(e: unknown)` + `errorMessage()`.
- `npx tsc --noEmit` EXIT=0, `npm run build` OK (без предупреждений).
- Замечание: `main.ts` использует `JSON.parse(res.text)` в `catch` — нотификация не показывается
  (только `console.warn`), без деградации; результаты из pull загружаются без правок.
