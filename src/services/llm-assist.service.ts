import { Notice } from 'obsidian';
import { getService } from '../../../sbe-core/src/bridge';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import type { SbeLlmApi } from '../../../sbe-core/src/types';
import type {
  AttributeDataType,
  AttributeFillMethod,
  AttributeLevel,
  ClassificationBranch,
  ClassificationClause,
  ClassificationRule,
  ComparisonOperator,
  MethodAttribute,
  MethodPresentation,
  Operand,
  PresentationField,
} from '../types/lims';

const DSL_GRAMMAR = `Грамматика DSL формул lab-service (безопасный интерпретатор выражений — без eval/exec):
- Числа: 1, 2.5; строки: 'текст'; булевы: true/false.
- Арифметика: + - * / ^ (степень), скобки, унарный минус.
- Сравнения: == != < <= > >=; логика: and or not.
- Условие: if <условие> then <выражение> else <выражение>.
- Ссылка на атрибут метода — просто его id, без кавычек.
- avg(a, b, c) / min(a, b, c) / max(a, b, c) / sum(a, b, c) — среднее/мин/макс/сумма нескольких атрибутов ОДНОЙ серии.
- avg(id) / min(id) / max(id) / sum(id) / count(id) / median(id) / std(id) — то же по ОДНОМУ атрибуту, агрегируя по всем сериям заявки (для атрибутов уровня "Агрегированные результаты").
- min_grade(g1, g2, ...) / max_grade(g1, g2, ...) — минимальный/максимальный из перечисленных показателей-атрибутов по порядку показателей метода (по убыванию).
- interpolate(x, xs, ys) — линейная интерполяция: x — число (или атрибут), xs/ys — атрибуты-массивы одной длины (напр. калибровочная таблица прибора).
- agg_where('avg'|'min'|'max'|'sum', значение_атрибут, условие_атрибут, 'значение') — агрегат по сериям заявки, где условие_атрибут равен указанному значению (только для уровня "Агрегированные результаты").
- Значение формулы — значение самого выражения, присваивание не нужно.`;

/** Сводка уже существующего атрибута (для контекста ИИ при черновике из стандарта —
 * не полный MethodAttribute, только то, что нужно для переиспользования по смыслу). */
export interface ExistingAttributeSummary {
  id: string;
  name: string;
  data_type: string;
  fill_method: string;
  level: string;
}

const VALID_DATA_TYPES = new Set(['text', 'int', 'float', 'date', 'time', 'photo', 'boolean']);
const VALID_FILL_METHODS = new Set(['manual', 'instrument', 'calculated', 'classification']);
const VALID_LEVELS = new Set(['experiment', 'aggregated']);
const VALID_OPERATORS = new Set(['==', '!=', '<', '<=', '>', '>=']);
const VALID_OPERAND_KINDS = new Set(['literal', 'attribute', 'target_indicator']);

const CYRILLIC_TO_LATIN: Record<string, string> = {
  а: 'a', б: 'b', в: 'v', г: 'g', д: 'd', е: 'e', ё: 'e', ж: 'zh', з: 'z', и: 'i', й: 'i',
  к: 'k', л: 'l', м: 'm', н: 'n', о: 'o', п: 'p', р: 'r', с: 's', т: 't', у: 'u', ф: 'f',
  х: 'h', ц: 'c', ч: 'ch', ш: 'sh', щ: 'sch', ъ: '', ы: 'y', ь: '', э: 'e', ю: 'yu', я: 'ya',
};

function transliterate(s: string): string {
  return s.toLowerCase().split('').map(ch => CYRILLIC_TO_LATIN[ch] ?? ch).join('');
}

/** Приводит произвольную строку к валидному id атрибута
 * (^[A-Za-z_][A-Za-z0-9_]*$, уникальному в пределах taken) — транслитерация
 * кириллицы, замена недопустимых символов, дедуп числовым суффиксом.
 * Экспортируется — тот же хелпер годится и для ручного ввода id в UI. */
export function sanitizeAttributeId(raw: string, taken: Set<string>): string {
  let id = transliterate(raw.trim())
    .replace(/[^a-z0-9_]+/g, '_')
    .replace(/^_+|_+$/g, '')
    .replace(/_{2,}/g, '_');
  if (!id || /^[0-9]/.test(id)) id = `attr_${id}`;
  if (!taken.has(id)) return id;
  let i = 2;
  while (taken.has(`${id}_${i}`)) i++;
  return `${id}_${i}`;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Заменяет идентификаторы-ссылки на атрибуты (в DSL-формуле) по карте
 * переименований — нужно, когда sanitizeAttributeId поменял чей-то id и другие
 * черновые формулы/aggregation.source в этом же ответе ИИ на него ссылались. */
function remapIdentifiers(expr: string, renameMap: Map<string, string>): string {
  let out = expr;
  for (const [oldId, newId] of renameMap) {
    if (oldId === newId) continue;
    out = out.replace(new RegExp(`\\b${escapeRegExp(oldId)}\\b`, 'g'), newId);
  }
  return out;
}

/** Санитизация черновика атрибутов (от ИИ ИЛИ из загруженного вручную JSON —
 * экспортируется для повторного использования обоих путей одной и той же
 * гарантией валидности): неизвестные data_type/fill_method/level заменяются на
 * безопасный дефолт (не отбрасывают весь атрибут — только теряют специфику,
 * которую пользователь всё равно проверит), id приводится к валидному виду.
 * Возвращает и карту переименований — для remapIdentifiers по formula/
 * aggregation.source и для parameter_name правил классификации того же набора. */
export function sanitizeAttributesWithRename(raw: unknown): { attributes: MethodAttribute[]; renameMap: Map<string, string> } {
  const renameMap = new Map<string, string>();
  if (!Array.isArray(raw)) return { attributes: [], renameMap };
  const taken = new Set<string>();
  const attrs: MethodAttribute[] = [];
  for (const item of raw) {
    if (!item || typeof item !== 'object') continue;
    const r = item as Record<string, unknown>;
    const name = typeof r.name === 'string' ? r.name.trim() : '';
    if (!name) continue;
    const rawId = typeof r.id === 'string' && r.id.trim() ? r.id.trim() : name;
    const id = sanitizeAttributeId(rawId, taken);
    taken.add(id);
    if (id !== rawId) renameMap.set(rawId, id);

    const data_type = (VALID_DATA_TYPES.has(r.data_type as string) ? r.data_type : 'text') as AttributeDataType;
    const fill_method = (VALID_FILL_METHODS.has(r.fill_method as string) ? r.fill_method : 'manual') as AttributeFillMethod;
    const level = (VALID_LEVELS.has(r.level as string) ? r.level : 'experiment') as AttributeLevel;
    const attr: MethodAttribute = { id, name, data_type, fill_method, level };
    if (fill_method === 'calculated' && typeof r.formula === 'string' && r.formula.trim()) {
      attr.formula = r.formula.trim();
    }
    if (level === 'aggregated' && r.aggregation && typeof r.aggregation === 'object') {
      const agg = r.aggregation as Record<string, unknown>;
      const method = agg.method;
      const source = typeof agg.source === 'string' ? agg.source.trim() : '';
      if ((method === 'avg' || method === 'min' || method === 'max') && source) {
        attr.aggregation = { source, method };
      }
    }
    // не запрошено в промпте, но принимаем на случай, если модель всё же вернёт
    // synonyms (напр. по инерции из общего контекста) — не отбрасывать молча.
    if (Array.isArray(r.synonyms)) {
      const synonyms = r.synonyms.filter((s): s is string => typeof s === 'string' && s.trim() !== '').map(s => s.trim());
      if (synonyms.length > 0) attr.synonyms = synonyms;
    }
    attrs.push(attr);
  }
  // ссылки на другие атрибуты этого же черновика могли указывать на id ДО
  // санитизации — применить renameMap постфактум
  for (const attr of attrs) {
    if (attr.formula) attr.formula = remapIdentifiers(attr.formula, renameMap);
    if (attr.aggregation) {
      const mapped = renameMap.get(attr.aggregation.source);
      if (mapped) attr.aggregation.source = mapped;
    }
  }
  return { attributes: attrs, renameMap };
}

/** Санитизирует один операнд атомарного сравнения (см. Operand) — неизвестный/
 * отсутствующий kind коэрцируется к литералу с пустым значением. id атрибута
 * прогоняется через renameMap (тот же черновик, см. sanitizeAttributesWithRename). */
function sanitizeOperand(raw: unknown, renameMap: Map<string, string>): Operand {
  const o = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  const kind = VALID_OPERAND_KINDS.has(o.kind as string) ? o.kind : 'literal';
  if (kind === 'attribute') {
    const rawId = typeof o.id === 'string' ? o.id.trim() : '';
    return { kind: 'attribute', id: renameMap.get(rawId) || rawId };
  }
  if (kind === 'target_indicator') return { kind: 'target_indicator' };
  const value = o.value;
  return { kind: 'literal', value: typeof value === 'number' ? value : String(value ?? '') };
}

/** Санитизация черновика правил классификации (2026-08-22v3, единая модель —
 * "Если [знак] Б (И/ИЛИ …), то показатель", применяется к каждой строке subjects):
 * запись без единой ветки ИЛИ без единого subject-а отбрасывается целиком; остальные
 * поля коэрцируются к ожидаемым типам. Ссылки на атрибуты (Operand.id,
 * subjects[].input_attribute_id/output_attribute_id) прогоняются через renameMap
 * атрибутов того же черновика (см. sanitizeAttributesWithRename). */
function sanitizeClassification(raw: unknown, renameMap: Map<string, string>): ClassificationRule[] {
  if (!Array.isArray(raw)) return [];
  const out: ClassificationRule[] = [];
  for (const item of raw) {
    if (!item || typeof item !== 'object') continue;
    const r = item as Record<string, unknown>;

    const branches = (Array.isArray(r.branches) ? r.branches : [])
      .filter((b): b is Record<string, unknown> => !!b && typeof b === 'object')
      .map((b): ClassificationBranch | null => {
        const grade = typeof b.grade === 'string' ? b.grade.trim() : '';
        if (!grade) return null;
        const rawClauses = Array.isArray(b.clauses) ? b.clauses : [];
        const clauses = rawClauses
          .filter((c): c is Record<string, unknown> => !!c && typeof c === 'object')
          .map((c): ClassificationClause | null => {
            if (!VALID_OPERATORS.has(c.operator as string)) return null;
            return {
              operator: c.operator as ComparisonOperator,
              compare_to: sanitizeOperand(c.compare_to, renameMap),
            };
          })
          .filter((c): c is ClassificationClause => c !== null);
        const join = b.join === 'or' ? 'or' : undefined;
        return clauses.length > 0 ? { clauses, join, grade } : { grade };
      })
      .filter((b): b is ClassificationBranch => b !== null);
    if (branches.length === 0) continue;

    const subjects = (Array.isArray(r.subjects) ? r.subjects : [])
      .filter((s): s is Record<string, unknown> => !!s && typeof s === 'object')
      .map((s) => {
        const rawInput = typeof s.input_attribute_id === 'string' ? s.input_attribute_id.trim() : '';
        const rawOutput = typeof s.output_attribute_id === 'string' ? s.output_attribute_id.trim() : '';
        if (!rawInput || !rawOutput) return null;
        return {
          input_attribute_id: renameMap.get(rawInput) || rawInput,
          output_attribute_id: renameMap.get(rawOutput) || rawOutput,
        };
      })
      .filter((s): s is { input_attribute_id: string; output_attribute_id: string } => s !== null);
    if (subjects.length === 0) continue;

    out.push({ branches, subjects });
  }
  return out;
}

/** Санитизация черновика представления — attribute_id, не входящие в validIds
 * (устаревшие/выдуманные ссылки), отбрасываются; атрибуты, которые ИИ не упомянул,
 * дописываются в конец (ничего из атрибутов не должно "потеряться" из представления). */
function sanitizePresentationFields(raw: unknown, validIds: Set<string>): PresentationField[] {
  const seen = new Set<string>();
  const out: PresentationField[] = [];
  if (Array.isArray(raw)) {
    for (const item of raw) {
      if (!item || typeof item !== 'object') continue;
      const r = item as Record<string, unknown>;
      const attribute_id = typeof r.attribute_id === 'string' ? r.attribute_id.trim() : '';
      if (!attribute_id || !validIds.has(attribute_id) || seen.has(attribute_id)) continue;
      seen.add(attribute_id);
      out.push({
        attribute_id,
        label: typeof r.label === 'string' && r.label.trim() ? r.label.trim() : undefined,
        show_in_ui: r.show_in_ui !== false,
        show_in_protocol: r.show_in_protocol !== false,
      });
    }
  }
  for (const id of validIds) {
    if (!seen.has(id)) out.push({ attribute_id: id, show_in_ui: true, show_in_protocol: true });
  }
  return out;
}

/** ИИ-помощник в написании формул конфигуратора методов (блок 1), а также в
 * черновике конфигурации метода целиком из текста стандарта. Прямой вызов
 * sbe-llm (простой stateless-шлюз) — без sbe-agent, тот не вызывается как сервис. */
export class LimsLlmAssist {
  private getModel: () => string;
  private servicePromise: Promise<SbeLlmApi> | null = null;

  constructor(getModel: () => string) {
    this.getModel = getModel;
  }

  private async llm(): Promise<SbeLlmApi> {
    if (!this.servicePromise) {
      this.servicePromise = getService('sbe-llm').catch((e: unknown) => {
        this.servicePromise = null;
        new Notice(`ЛИМС: включите плагин sbe-llm и настройте API-ключ (${errorMessage(e)})`);
        throw e;
      });
    }
    return this.servicePromise;
  }

  /** sbe-llm сам не хранит модель (её выбирает каждый потребитель) — без явной
   * модели шлюз chadgpt.ru может ответить не тем форматом, который ждёт клиент
   * (ломает completeJson). model?: string — точечный оверрайд на конкретный вызов,
   * иначе — из настроек плагина (SbeLimsSettings.llmModel). */
  private resolveModel(model?: string): string {
    if (model && model.trim()) return model.trim();
    return this.getModel();
  }

  /** Предлагает DSL-выражение формулы по описанию задачи от лаборанта + контексту
   * текущих атрибутов/показателей метода. Результат — только предложение для ревью,
   * вставка в поле формулы — решение пользователя (никогда не применяется автоматически). */
  async suggestFormula(
    description: string,
    attrs: MethodAttribute[],
    determinableIndicators: string[],
  ): Promise<string> {
    const service = await this.llm();
    const attrsList = attrs
      .map(a => `- ${a.id} (${a.name || a.id}): ${a.data_type}, уровень ${a.level === 'aggregated' ? 'агрегированные результаты' : 'данные эксперимента'}`)
      .join('\n');
    const system = `${DSL_GRAMMAR}

Атрибуты метода, доступные в формуле:
${attrsList || '(атрибутов пока нет)'}

Показатели метода (determinable_indicators, по убыванию): ${determinableIndicators.join(', ') || '(не заданы)'}

Ответь ТОЛЬКО самим выражением формулы на этой грамматике, без пояснений, без markdown-обёртки, без кавычек вокруг всего ответа.`;
    const result = await service.complete(system, description, { model: this.resolveModel() });
    return result.trim().replace(/^```[a-z]*\n?/i, '').replace(/```$/, '').trim();
  }

  /** Черновик атрибутов + правил классификации метода по тексту стандарта
   * (уже извлечённому из .rtf/.txt, см. rtf-to-text.ts). existingAttributes —
   * атрибуты ДРУГИХ методов системы, для переиспользования по смыслу вместо
   * изобретения дублей. Результат — только черновик для ревью в обычных
   * редакторах (блоки 1-2), никогда не сохраняется автоматически. Числа,
   * нечитаемые в исходном файле (напр. таблица встроена как картинка), ИИ
   * инструктируется не выдумывать, а помечать явно — санитизация здесь этого
   * не проверяет, это делает сам промпт. */
  async draftAttributesAndClassification(
    standardText: string,
    existingAttributes: ExistingAttributeSummary[],
  ): Promise<{ attributes: MethodAttribute[]; classification: ClassificationRule[] }> {
    const service = await this.llm();
    const existingList = existingAttributes
      .map(a => `- ${a.id} (${a.name}): ${a.data_type}, заполнение ${a.fill_method}, уровень ${a.level}`)
      .join('\n');
    const system = `Ты помогаешь настроить конфигуратор методов лабораторной информационной системы (ЛИМС) по тексту нормативного стандарта испытаний.

${DSL_GRAMMAR}

Формат атрибута метода (JSON):
{"id": "лат_идентификатор", "name": "название на русском", "data_type": "text"|"int"|"float"|"date"|"time"|"boolean"|"photo", "fill_method": "manual"|"instrument"|"calculated"|"classification", "level": "experiment"|"aggregated", "formula": "DSL-выражение (только если fill_method=calculated)", "aggregation": {"source": "id атрибута уровня experiment", "method": "avg"|"min"|"max"} (только если level=aggregated и своей формулы нет)}
Правила для id: только латиница/цифры/подчёркивание, не начинать с цифры, уникальны в пределах метода (^[A-Za-z_][A-Za-z0-9_]*$).
id — ЭТО ПЕРЕВОД смысла на английский (или общепринятый латинский термин), а НЕ фонетическая транслитерация русских слов: "длительность пламенного горения" -> "flame_duration" (правильно), НЕ "dlit_plam_gor" (неправильно — транслитерация русских слов латиницей нечитаема и не является переводом).
data_type="boolean" — значение true/false (в UI — Да/Нет); используй для атрибутов вида "наблюдалось/не наблюдалось", "да/нет" из текста стандарта, а не text.
fill_method="classification" — используй ТОЛЬКО для атрибута, значение которого целиком определяет правило классификации (его id должен встречаться как output_attribute_id хотя бы одного subject хотя бы одного правила в "classification" ниже) — например итоговый показатель/группа/класс, а не для промежуточных измеряемых величин.

Формат правила классификации (JSON, единая модель — "Если [знак] Б (И/ИЛИ [знак] Б2 …), то показатель", как в Excel IF/AND/OR; левая часть каждого условия НЕЯВНАЯ — см. subjects ниже, конкретный атрибут в самой схеме условий не упоминается):
{"branches": [{"clauses": [{"operator": "<"|"<="|">"|">="|"=="|"!=", "compare_to": Операнд}], "join": "and"|"or" (опц., по умолч. "and" — как связаны МЕЖДУ СОБОЙ несколько clauses одной ветки), "grade": "показатель"}], "subjects": [{"input_attribute_id": "id оцениваемого атрибута", "output_attribute_id": "id атрибута-результата"}]}
Операнд (compare_to) — {"kind": "attribute", "id": "id атрибута"} | {"kind": "literal", "value": число|строка} | {"kind": "target_indicator"} (целевой показатель заявки).
Ветка без "clauses" (пустой массив/не задан) — безусловная ветка "Иначе", срабатывает всегда — ставь последней, если нужен catch-all вместо отсутствия результата.
Ветки проверяются ПО ПОРЯДКУ массива (сервер не сортирует), первая совпавшая — результат; ОДНА и та же схема branches применяется ОТДЕЛЬНО к каждой строке subjects (можно оценивать сразу несколько атрибутов одним и тем же набором условий — обязательно заполни subjects, иначе правило ничего не делает).
Значение атрибута всегда берётся из текущей записи как есть, без свода по нескольким сериям.
Примеры: пороговое правило — clause {"operator":"<=","compare_to":{"kind":"literal","value":50}}; булево (data_type=boolean) — compare_to.kind="literal" со значением true/false, operator "=="; соответствие целевому показателю заявки — compare_to.kind="target_indicator" (обычно 2 ветки: ">=" / "<" плюс catch-all "не оценивается"); несколько условий сразу — несколько clauses в одной ветке с join "and"/"or".
Знаки "<"/"<="/">"/">=" при сравнении ДВУХ показателей метода (оба входят в determinable_indicators) трактуются как сравнение по порядку ввода показателей (первый введённый — "больше"/"выше"), иначе — обычное числовое сравнение.

Уже существующие атрибуты в системе (из других методов — переиспользуй id и название, если параметр стандарта смыслово совпадает, вместо того чтобы придумывать новый):
${existingList || '(пока нет)'}

КРИТИЧЕСКИ ВАЖНО: если числовое значение (например, порог классификации) в тексте стандарта не читается однозначно — отсутствует, дано неполно, была таблица без текста — НЕ ВЫДУМЫВАЙ его. Поставь 0 и добавь в "name" пометку "(ТРЕБУЕТ ПРОВЕРКИ)".

Верни ТОЛЬКО JSON-объект: {"attributes": [...], "classification": [...]}, без markdown-обёртки и пояснений.`;

    const raw = await service.completeJson<{ attributes?: unknown; classification?: unknown }>(
      system, standardText, { model: this.resolveModel() },
    );
    const { attributes, renameMap } = sanitizeAttributesWithRename(raw?.attributes);
    const classification = sanitizeClassification(raw?.classification, renameMap);
    return { attributes, classification };
  }

  /** Подбор атрибутов по списку пользовательских названий/описаний (кнопка
   * «Предложить атрибуты по описанию», блок 1): для каждой строки — либо находит
   * смыслово подходящий уже существующий атрибут (этого или другого метода), либо
   * предлагает новый черновик. matched — только информационно (ничего не
   * добавляется); drafted — черновики новых атрибутов для ревью, санитизированы
   * тем же sanitizeAttributesWithRename, что и остальные ИИ-черновики. */
  async suggestAttributesFromNames(
    names: string[],
    existingAttributes: ExistingAttributeSummary[],
  ): Promise<{ matched: Array<{ input: string; existing: ExistingAttributeSummary }>; drafted: MethodAttribute[] }> {
    const service = await this.llm();
    const existingList = existingAttributes
      .map(a => `- ${a.id} (${a.name}): ${a.data_type}, заполнение ${a.fill_method}, уровень ${a.level}`)
      .join('\n');
    const system = `Ты помогаешь конфигуратору методов лабораторной информационной системы (ЛИМС) подобрать атрибуты по списку пользовательских названий/описаний измеряемых величин.

${DSL_GRAMMAR}

Формат атрибута метода (JSON):
{"id": "лат_идентификатор", "name": "название на русском", "data_type": "text"|"int"|"float"|"date"|"time"|"boolean"|"photo", "fill_method": "manual"|"instrument"|"calculated"|"classification", "level": "experiment"|"aggregated", "formula": "DSL-выражение (только если fill_method=calculated)", "aggregation": {"source": "id атрибута уровня experiment", "method": "avg"|"min"|"max"} (только если level=aggregated и своей формулы нет)}
Правила для id: только латиница/цифры/подчёркивание, не начинать с цифры, уникальны в пределах метода (^[A-Za-z_][A-Za-z0-9_]*$).
id — ЭТО ПЕРЕВОД смысла на английский (или общепринятый латинский термин), а НЕ фонетическая транслитерация русских слов: "длительность пламенного горения" -> "flame_duration" (правильно), НЕ "dlit_plam_gor" (неправильно).

Уже существующие атрибуты в системе (переиспользуй, если смыслово совпадает с одной из строк списка, вместо того чтобы придумывать новый):
${existingList || '(пока нет)'}

Для КАЖДОЙ строки списка реши: если она смыслово совпадает с одним из уже существующих атрибутов выше — верни его id как "existing_id"; иначе предложи НОВЫЙ атрибут как "new_attribute" по формату выше.

Верни ТОЛЬКО JSON-объект: {"results": [{"input": "<строка списка как есть>", "existing_id": "id"} | {"input": "<строка списка как есть>", "new_attribute": {...}}]}, без markdown-обёртки и пояснений.`;

    const raw = await service.completeJson<{ results?: unknown }>(
      system, names.map(n => `- ${n}`).join('\n'), { model: this.resolveModel() },
    );
    const existingById = new Map(existingAttributes.map(a => [a.id, a]));
    const matched: Array<{ input: string; existing: ExistingAttributeSummary }> = [];
    const rawDrafts: unknown[] = [];
    for (const item of Array.isArray(raw?.results) ? raw.results : []) {
      if (!item || typeof item !== 'object') continue;
      const r = item as Record<string, unknown>;
      const input = typeof r.input === 'string' ? r.input : '';
      const existingId = typeof r.existing_id === 'string' ? r.existing_id : '';
      const match = existingId ? existingById.get(existingId) : undefined;
      if (match) matched.push({ input, existing: match });
      else if (r.new_attribute) rawDrafts.push(r.new_attribute);
    }
    const { attributes: drafted } = sanitizeAttributesWithRename(rawDrafts);
    return { matched, drafted };
  }

  /** Черновик представления (порядок/подписи/видимость столбцов в UI-таблице
   * результатов и в протоколе) по уже сформированному черновику атрибутов —
   * низкий риск (нет числовых значений), поэтому отдельный, более дешёвый вызов. */
  async draftPresentation(attributes: MethodAttribute[]): Promise<MethodPresentation> {
    const service = await this.llm();
    const attrsList = attributes
      .map(a => `- ${a.id} (${a.name}): ${a.data_type}, уровень ${a.level === 'aggregated' ? 'агрегированные результаты' : 'данные эксперимента'}`)
      .join('\n');
    const system = `Ты настраиваешь представление данных метода испытаний: порядок и подписи столбцов в таблице результатов (UI) и в протоколе испытаний.

Формат поля представления (JSON): {"attribute_id": "id атрибута", "label": "подпись столбца (опц.)", "show_in_ui": true|false, "show_in_protocol": true|false}.
Порядок элементов массива — порядок столбцов слева-направо.

Атрибуты метода:
${attrsList || '(атрибутов нет)'}

По умолчанию показывай все атрибуты и в UI, и в протоколе, в логичном порядке: сначала данные эксперимента в порядке измерения, затем расчётные показатели, затем агрегированные/итоговые показатели и классификация.

Верни ТОЛЬКО JSON-объект: {"fields": [...]}, без markdown-обёртки и пояснений.`;

    const raw = await service.completeJson<{ fields?: unknown }>(
      system, 'Сформируй представление для перечисленных атрибутов.', { model: this.resolveModel() },
    );
    return { fields: sanitizePresentationFields(raw?.fields, new Set(attributes.map(a => a.id))) };
  }
}
