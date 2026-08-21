import { Notice } from 'obsidian';
import { getService } from '../../../sbe-core/src/bridge';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import type { SbeLlmApi } from '../../../sbe-core/src/types';
import type {
  AttributeDataType,
  AttributeFillMethod,
  AttributeLevel,
  BooleanClassificationRule,
  ClassificationRule,
  MethodAttribute,
  MethodPresentation,
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
- worst_grade(g1, g2, ...) / best_grade(g1, g2, ...) — худший/лучший из перечисленных показателей-атрибутов по порядку показателей метода (от лучшего к худшему).
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

const VALID_DATA_TYPES = new Set(['text', 'int', 'float', 'date', 'time', 'photo']);
const VALID_FILL_METHODS = new Set(['manual', 'instrument', 'calculated']);
const VALID_LEVELS = new Set(['experiment', 'aggregated']);
const VALID_RULE_TYPES = new Set(['threshold', 'boolean', 'compliance']);
const VALID_OPERATORS = new Set(['==', '!=', '<', '<=', '>', '>=']);

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

/** Санитизация черновика атрибутов от ИИ: неизвестные data_type/fill_method/level
 * заменяются на безопасный дефолт (не отбрасывают весь атрибут — только теряют
 * специфику, которую пользователь всё равно проверит), id приводится к валидному
 * виду. Возвращает и карту переименований — для remapIdentifiers по formula/
 * aggregation.source и для parameter_name правил классификации того же черновика. */
function sanitizeAttributesWithRename(raw: unknown): { attributes: MethodAttribute[]; renameMap: Map<string, string> } {
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

/** Санитизация черновика правил классификации — неизвестный rule_type отбрасывает
 * запись целиком (не понятно, как безопасно её достроить); остальные поля
 * коэрцируются к ожидаемым типам. parameter_name прогоняется через renameMap
 * атрибутов того же черновика (см. sanitizeAttributesWithRename). */
function sanitizeClassification(raw: unknown, renameMap: Map<string, string>): ClassificationRule[] {
  if (!Array.isArray(raw)) return [];
  const out: ClassificationRule[] = [];
  for (const item of raw) {
    if (!item || typeof item !== 'object') continue;
    const r = item as Record<string, unknown>;
    if (!VALID_RULE_TYPES.has(r.rule_type as string)) continue;
    const rawParam = typeof r.parameter_name === 'string' ? r.parameter_name.trim() : '';
    if (!rawParam) continue;
    const parameter_name = renameMap.get(rawParam) || rawParam;
    const output_name = typeof r.output_name === 'string' && r.output_name.trim() ? r.output_name.trim() : undefined;

    if (r.rule_type === 'threshold') {
      const thresholds = (Array.isArray(r.thresholds) ? r.thresholds : [])
        .filter((t): t is Record<string, unknown> => !!t && typeof t === 'object')
        .map(t => ({ value: Number(t.value) || 0, grade: typeof t.grade === 'string' ? t.grade.trim() : '' }))
        .filter(t => t.grade);
      const aggregation_rule = r.aggregation_rule === 'best' || r.aggregation_rule === 'worst' ? r.aggregation_rule : undefined;
      out.push({ rule_type: 'threshold', parameter_name, output_name, thresholds, aggregation_rule });
    } else if (r.rule_type === 'boolean') {
      const operator = (VALID_OPERATORS.has(r.operator as string) ? r.operator : '==') as BooleanClassificationRule['operator'];
      const value = typeof r.value === 'string' || typeof r.value === 'number' ? r.value : '';
      out.push({
        rule_type: 'boolean',
        parameter_name,
        output_name,
        operator,
        value,
        true_grade: typeof r.true_grade === 'string' ? r.true_grade.trim() : '',
        false_grade: typeof r.false_grade === 'string' ? r.false_grade.trim() : '',
      });
    } else {
      out.push({
        rule_type: 'compliance',
        parameter_name,
        output_name,
        comply_text: typeof r.comply_text === 'string' ? r.comply_text.trim() || undefined : undefined,
        not_comply_text: typeof r.not_comply_text === 'string' ? r.not_comply_text.trim() || undefined : undefined,
        not_assessed_text: typeof r.not_assessed_text === 'string' ? r.not_assessed_text.trim() || undefined : undefined,
      });
    }
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

Показатели метода (determinable_indicators, от лучшего к худшему): ${determinableIndicators.join(', ') || '(не заданы)'}

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
{"id": "лат_идентификатор", "name": "название на русском", "data_type": "text"|"int"|"float"|"date"|"time"|"photo", "fill_method": "manual"|"instrument"|"calculated", "level": "experiment"|"aggregated", "formula": "DSL-выражение (только если fill_method=calculated)", "aggregation": {"source": "id атрибута уровня experiment", "method": "avg"|"min"|"max"} (только если level=aggregated и своей формулы нет)}
Правила для id: только латиница/цифры/подчёркивание, не начинать с цифры, уникальны в пределах метода (^[A-Za-z_][A-Za-z0-9_]*$).
id — ЭТО ПЕРЕВОД смысла на английский (или общепринятый латинский термин), а НЕ фонетическая транслитерация русских слов: "длительность пламенного горения" -> "flame_duration" (правильно), НЕ "dlit_plam_gor" (неправильно — транслитерация русских слов латиницей нечитаема и не является переводом).

Формат правила классификации (JSON, один из трёх типов):
{"rule_type": "threshold", "parameter_name": "id атрибута-источника", "output_name": "id результата (опц.)", "thresholds": [{"value": число, "grade": "показатель"}], "aggregation_rule": "best"|"worst" (опц., по умолч. среднее)}
{"rule_type": "boolean", "parameter_name": "...", "operator": "=="|"!="|"<"|"<="|">"|">=", "value": "...", "true_grade": "...", "false_grade": "..."}
{"rule_type": "compliance", "parameter_name": "...", "comply_text": "...", "not_comply_text": "...", "not_assessed_text": "..."}

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
