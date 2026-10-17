# План: sbe-lims — аналогичные объекты по материалу (вместо ЕКН)

Спека: `docs/superpowers/specs/2026-10-17-sbe-lims-analogue-material-identity-design.md`.
Решения (согласованы 2026-10-17):

- ключ материала — **(тип материала, линейка, модификатор)**;
- **толщина не входит в ключ** — все толщины линейки анализируются вместе,
  различие толщин показывается в отчёте (закономерности при изменении толщины);
- фасовка (ширина × длина, габарит плиты) и цвет — не важны никогда;
- торговые двойники (Ecoplast / ECOROOF / TSTN) и модификаторы (FR / Pro / Arctic) —
  **разные материалы**;
- три ступени аналогов: тот же материал с ЕКН / тот же материал без ЕКН / модификация
  линейки; в статистике не смешиваются;
- тренд по толщине — текстом только при **≥ 3 испытаниях на толщину**;
- мусорный ЕКН (название с номером в поле) — вытащить шестизначный хвост, лечить.

## Принцип порядка работ

Только сервер (`lab-service`, ветка `backend`); клиент `sbe-lims` не меняется,
версия плагина **не поднимается**. `ekn-service` тоже не меняется — используется
существующая ручка `GET /api/ekn/product/{ekn}`. Каждый этап — самостоятельно
проверяемый: A/B — чистые функции и кэш, C/D — отбор и статистика (покрыты
Go-тестами на фикстурах), E — композиция документа. Коммит/деплой — по отдельной
явной команде пользователя.

`go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `gofmt -l .` —
после каждого этапа. В конце — `node scripts/check_lab_openapi.mjs`.

---

## Часть A — разбор признаков материала (чистые функции)

## Этап A1. Нормализация названий

Новый файл `lab-service/material_key.go`, без БД и сети.

`normalizeMaterialText(raw string) string`:

- Unicode NFKC (`golang.org/x/text/unicode/norm` — `golang.org/x/text` уже прямая
  зависимость `lab-service`, v0.19.0; лицензия BSD-3-Clause, правило «только
  свободные Go-библиотеки» соблюдено, новых зависимостей не появляется);
- кириллические омоглифы внутри латинских слов → латиница: `а→a`, `е→e`, `о→o`,
  `р→p`, `с→c`, `х→x`, `у→y`, `к→k`, `м→m`, `т→t`, `в→b`, `н→h` (только когда слово
  целиком латинское — иначе пострадают русские слова);
- `_` в номере модели (`V_RP`) → `-`;
- кириллическая `х` **между цифрами** (габариты `2385х1185`) → `x`;
- схлопывание пробелов, обрезка, нижний регистр.

Тесты: `TestNormalizeMaterialText` — `Lodgicroof` (не трогаем: это опечатка,
чинится словарём ниже), `V_RP`, `Arctiс` (с кириллической `с` на конце),
`2385х1185`, `LOGICPIR  PROF` (двойной пробел).

## Этап A2. Разбор названия в ключ материала

В том же файле:

```go
type MaterialKey struct {
    Type     string // канонический тип: roofing_polym_membr | thermal_insulation | sealant | ...
    Line     string // "logicroof v-rp", "logicpir prof", "ecoplast v-rp"
    Modifier string // "fr", "pro", "arctic", "floors", "" (нет)
}

func ParseMaterialFromName(raw string) (MaterialKey, float64) // ключ + толщина (0 — нет)
```

Алгоритм:

1. `normalizeMaterialText`.
2. Вырезать шум: `№п.<N>`, `партия <N>`, `лот# <N>`, цвет в скобках `(ral NNNN)`,
   габарит `\d+[.,]\d+\s*[xх]\s*\d+`, толщина `\d+[.,]\d+\s*(мм|mm|м\b)` — thickness
   извлекается, а не выбрасывается.
3. Определить **тип** по словарю ключевых слов: `плиты|пенополистирол|экструзионн|
   логикпир|logicpir|техноплекс|теплопровод` → `thermal_insulation`;
   `мембрана|пвх|pvх|tstn|logicroof|ecoplast|ecoroof|logicbase|ecobase|logicflex`
   → `roofing_polym_membr`; `герметик` → `sealant`; `геомембрана|геопруф` →
   `geomembrane`; неизвестно → `unknown:<первое слово>`.
4. Вырезать **модификатор** по словарю: `fr`, `pro`, `profr`, `arctic|арктик`,
   `полы фл/фл|полы`, `ф/ф`, `prof` (у LOGICPIR), **`middle`** — отдельная
   модификация (решение пользователя 2026-10-17): входит в ключ как
   `modifier = "middle"`, заявки уходят в **ступень 3**. Позиция слова не важна
   (`Logicroof Middle V-RP` = `Logicroof V-RP Middle`).
5. Оставшийся блок модели — **линейка**: латинские буквы/цифры/дефисы, включая
   приклеенный модификатор в конце (`logicroof v-rp fr` → line `logicroof v-rp`,
   modifier `fr`).
6. Словарь опечаток: `lodgicroof|logiroof|logicroof|logicroof` → `logicroof`;
   `pvх|pvh` → `pvc`.

Тесты — на **живых названиях** (сняты с `lab.objects` 2026-10-17), в табличном виде
«вход → ожидаемый ключ + толщина»:

| Вход | Type | Line | Modifier | Толщина |
|---|---|---|---|---|
| `Logicroof V-RP` | membrane | logicroof v-rp | — | 0 |
| `Logicroof Middle V-RP 1,5mm` | membrane | logicroof v-rp | middle | 1.5 |
| `Logicroof V-RP middle 1.5mm` | membrane | logicroof v-rp | middle | 1.5 |
| `Logicroof V_RP  темно-серая` | membrane | logicroof v-rp | — | 0 |
| `Lodgicroof V-RP Middle` | membrane | logicroof v-rp | middle | 0 |
| `ПВХ Logicroof V-RP Arctiс 2,0 мембрана серая 2,10х15 м` | membrane | logicroof v-rp | arctic | 2.0 |
| `Logicroof V-RP FR 1,2 мм` | membrane | logicroof v-rp | fr | 1.2 |
| `Logicroof V-RP Pro` | membrane | logicroof v-rp | pro | 0 |
| `ПВХ Ecoplast V-RP 1,2 мм мембрана серая 2,10х25 м` | membrane | ecoplast v-rp | — | 1.2 |
| `TSTN ПВХ-МЕМБРАНА V-RP 1,2 мм (2,10х25 м)` | membrane | tstn pvc-membrane v-rp | — | 1.2 |
| `ПВХ Logicroof V-RP 1,5 мм мембрана синяя (RAL 5005) 2,10x20 м. сурьма RG-69` | membrane | logicroof v-rp | — | 1.5 |
| `LOGICPIR PROF Ф/Ф Г1 L-2385х1185х40` | insulation | logicpir prof | f/f | 40 |
| `LOGICPIR Полы ФЛ/ФЛ L-1190х590х40` | insulation | logicpir | floors | 40 |
| `LOGICPIR Ф/Ф L-1190х590х40` | insulation | logicpir | f/f | 40 |
| `Герметик ПУ ТехноНИКОЛЬ Logicflex для плоских кровель, шт.` | sealant | logicflex | — | 0 |

Ключевая проверка: `Ecoplast V-RP` ≠ `Logicroof V-RP` (разные материалы),
`V-RP FR` ≠ `V-RP` (модификатор), `LOGICPIR Полы ФЛ/ФЛ` ≠ `LOGICPIR Ф/Ф`.

## Этап A3. Разбор карточки QRC

```go
type EknFeatures struct {
    NameWeb, Color, Armature string
    TypeMat, TypeOfGid       string
    Thickness, Width, Length float64 // мм; 0 — нет
}

func parseEknFeatures(groups map[string]any) EknFeatures
func ParseMaterialFromFeatures(f EknFeatures) (MaterialKey, float64)
```

- `NameWeb` — `groups.Web.name_web.value`;
- тип — `groups.Modif.atr_type_material[0]` (список!), fallback
  `groups.Modif.type_ofgidro.data`;
- толщина — `groups.geometric_characteristics.Thikness.value.amount` (+ `unit`,
  `MILLIMETER` → мм; `METER` → ×1000);
- габарит — `Width`/`Length` (тоже `.value.amount`), ширина также
  `atr_width_m_min05pct_plus1pct` как fallback;
- цвет — `groups.Spec.atr_color_material.data`, армирование —
  `groups.Spec.armirovanie.data`.

`ParseMaterialFromFeatures` приоритетно использует **`name_web`** для линейки и
модификатора (структурно их в QRC нет — проверено на 38 карточках), тип — из
`Modif`, толщину — из `Thikness`. Тесты — на JSON-фрагментах, снятых 2026-10-17:
мембрана `500464` (1,5 / 2,1 / 20 / `polyester_setka` / `silk_gray_ral7047`),
плита `644712` (40 / 1185 / 2385), герметик `40415` (тип и толщина пустые),
геомембрана `149759` (толщина 1,5, габарит только в названии).

## Этап A4. Лечение мусорного ЕКН

```go
func cleanEkn(raw string) string // последние 6 цифр из строки, иначе ""
```

Тесты: `"ПВХ Ecoplast V-RP 1.2 мм мембрана серая 2.10х25, 591278"` → `591278`;
`"591278"` → `591278`; `"001517"` → `001517` (не обрезать нули); `"ПВХ мембрана"`
→ `""`.

---

## Часть B — кэш признаков ЕКН

## Этап B1. Таблица и чтение/запись

`main.go` — миграция (идемпотентно, в общем блоке `stmts`):

```sql
CREATE TABLE IF NOT EXISTS ekn_material_features (
  ekn        TEXT PRIMARY KEY,
  type       TEXT NOT NULL DEFAULT '',
  line       TEXT NOT NULL DEFAULT '',
  modifier   TEXT NOT NULL DEFAULT '',
  thickness  DOUBLE PRECISION,
  width_m    DOUBLE PRECISION,
  length_m   DOUBLE PRECISION,
  color      TEXT NOT NULL DEFAULT '',
  source     TEXT NOT NULL DEFAULT '',
  fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Новый файл `lab-service/material_cache.go`:

- `loadMaterialFeature(ctx, ekn) (*materialFeature, bool)` — из кэша;
- `storeMaterialFeature(ctx, f)` — upsert (`ON CONFLICT (ekn) DO UPDATE`,
  `source='manual'` не перетирается автоматикой — `WHERE ekn_material_features.source
  <> 'manual'`);
- `resolveMaterial(ctx, ekn, rawName string) (MaterialKey, float64, string)` —
  приоритет: кэш → QRC (через новый `lookupEknFeatures`) → разбор названия.
  Возвращает ключ, толщину и источник (`ekn` | `qrc` | `name` | `title`).

`ekn_client.go`: новый `lookupEknFeatures(ctx, ekn) (map[string]any, error)` —
тот же сетевой путь и служебный токен, что `lookupEknProductName` (переиспользовать
`serviceToken(ctx, "ekn")`); возвращает `data.groups` сырым `map`. Если
ekn-service недоступен — **не ошибка**: падаем на разбор названия (best-effort,
как у `lookupEknProductName`).

## Этап B2. Ручка диагностики

| Метод | Путь | Роль |
|---|---|---|
| `GET` | `/api/lab/materials/{ekn}` | viewer |

Ответ: `{ekn, type, line, modifier, thickness, width_mm, length_mm, color, source,
name_web}`. Нужна, чтобы проверить разбор на конкретной карточке, не собирая отчёт.
Схема — в `static/openapi.yaml`; `check_lab_openapi.mjs` — сходится.

Тесты: чистые — `materialFeature` ↔ строка; кэш со стороны БД не тестируется
(в проекте нет мок-инфраструктуры, см. «Статистика ошибок» в AGENTS.md
`lab-service`) — проверяется живым запросом после деплоя.

---

## Часть C — отбор аналогов

`report_context.go`, переписать `loadAnalogueRequests`:

1. Материал текущей заявки: `resolveRequestMaterial(ctx, req)` —
   `cleanEkn(req.EKN)` → `resolveMaterial`; при пустом — название объекта
   (`objects.name`), затем `requests.title`.
2. Выборка: заявки того же метода, `id <> текущая`, с полями `ekn`, `object_id`,
   `object.name`, `object.characteristics`, `title`, `completed_at`.
   **`LIMIT` поднять до 2000** (сейчас `maxAnalogues = 300` и он режет выборку
   до фильтра — при отборе по материалу в памяти этого мало).
3. Для каждой — материал (кэш признаков уже тёплый после первого прохода;
   счёт сетевых обращений — только по ЕКН, которых нет в кэше, по одному на ЕКН).
4. Раскладка по ступеням: `Tier1` (ключ равен, у заявки есть ЕКН),
   `Tier2` (ключ равен, ЕКН пуст), `Tier3` (Line равна, Modifier отличается).

Новый тип строки:

```go
type analogueRow struct {
    ...прежние поля...
    Tier         int    `json:"tier"`
    Line         string `json:"line"`
    Thickness    string `json:"thickness"`  // "1,5" / "" 
    Pack         string `json:"pack"`       // "2,10×20 м" / "2385×1185" / ""
    Color        string `json:"color"`
    MaterialNote string `json:"material_note"` // «ЕКН не указан — определено по названию»
}
```

Тесты: `classifyAnalogueTier` (чистая) — 8 случаев, включая «та же база, другой
модификатор», «ЕКН пуст, название совпало», «совсем другой материал».

## Этап D — статистика с разрезом по толщине

`computeIndicatorStats` → добавляет `ByThickness []thicknessStat` (см. спеку) и
`Trend *trendNote`.

Чистые функции:

- `groupByThickness(analogues []analogueRow, seriesByReq, aggByReq, attr) []thicknessStat`
  — строит срезы; `Tests` считается по **заявкам**, а не по сериям;
- `computeTrend(byThickness []thicknessStat, minTests int) *trendNote` —
  при `Tests < minTests` (3) хотя бы у одной толщины → `nil`;
  направление — знак приращения **среднего** между соседними толщинами на всей оси
  (1,2 → 1,5 → 2,0); знак одинаков на каждом шаге — «растёт»/«падает», смена знака
  хотя бы на одном шаге — «немонотонно» и `nil` текста. Пересечение `[min..max]`
  соседних толщин тренда **не отменяет** (решение пользователя 2026-10-17).

Вызов из `loadReportContext` после существующего расчёта; срез — **только по
ступени 1** (ступени 2/3 в тренд не попадают, решение пользователя: ступени не
смешиваются; `Middle` — тоже ступень 3).

Тесты `TestGroupByThickness`, `TestComputeTrend` — фикстуры: 3 толщины × 3
испытания (тренд есть), 2 испытания на одну толщину (тренда нет), рост на одном
шаге и падение на следующем (немонотонно), пересекающиеся диапазоны при
постоянном знаке (тренд есть), толщина не определена (строка «не указана»).

## Этап E — приложение в отчёте

`report.go` (`composeAppendixNodes`) и `report_ai.go` (`renderReportContext`):

1. Шапка приложения: расшифровка материала («Линейка: Logicroof V-RP · тип:
   полимерная ПВХ-мембрана · модификатор: — · толщины в выборке: 1,2 / 1,5 / 2,0 мм»).
2. Свод по показателям (ступень 1) — как сейчас.
3. **Таблицы по толщинам — мини-таблица на каждый показатель** (решение
   пользователя 2026-10-17; одна широкая со склейкой заголовков отвергнута):
   строки = толщины (+ «не указана»), колонки = «Испытаний / Серий / Среднее /
   Мин / Макс»; для нечисловых показателей — «Испытаний / Распределение».
   Под таблицей — строка `trendNote`, если он есть. Реализация — `static_table`
   (рендер `renderStaticTableDocx`/`HTML` уже есть), БЕЗ склейки ячеек.
4. **Вывод о закономерностях**: при наличии `Trend` — детерминированная строка
   («При увеличении толщины с 1,2 до 2,0 мм среднее значение X растёт на 12 %»),
   плюс контекст передаётся ИИ — он пишет раздел своими словами.
   При отсутствии тренда (мало данных) — в контекст ИИ идёт явное указание
   «данных недостаточно для вывода о зависимости от толщины».
5. Таблица «Аналоги» — с колонками линейки/толщины/фасовки/цвета.
6. Ступень 2 и ступень 3 — отдельными таблицами с подписями (в ступени 3
   отдельными строками видны модификации, включая `Middle`).

`renderReportContext` получает блок:

```
ЗАВИСИМОСТЬ ОТ ТОЛЩИНЫ (найдено аналогичных испытаний: N):
- толщина 1,2 мм: 4 испытания, среднее прочности = 15.2 (мин 14.1, макс 16.0)
- толщина 1,5 мм: 6 испытаний, среднее = 16.4 ...
- толщина 2,0 мм: 3 испытания, среднее = 17.9 ...
Вывод (детерминированный): при увеличении толщины показатель растёт на 18%.
```

## Этап F — проверки и закрытие

- `gofmt -l .`, `go build ./...`, `go vet ./...`, `go test -count=1 ./...`.
- `node scripts/check_lab_openapi.mjs` — 86 маршрутов после новой ручки.
- Read-only сверка на живых данных: для 5–10 заявок разных методов — сколько
  аналогов находится по каждой ступени (ожидание: десятки, а не единицы).
- Живой E2E: собрать `POST /requests/{id}/report` на реальной заявке мембраны и
  на заявке LOGICPIR, открыть DOCX, проверить таблицу по толщинам.
- Версия плагина `sbe-lims` **не поднимается** (правка серверная); при выпуске
  в `main` — обычный перенос плагин-части без изменений.
