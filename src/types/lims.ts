/** Типы плагина «ЛИМС» (sbe-lims). Модель совместима с lab-service. */

/** Лаборатория. Внешняя (type='external') не существует самостоятельно —
 * parent_lab_id указывает на внутреннюю, обязателен при создании внешней. */
export interface Lab {
  id: number;
  code: string;
  name: string;
  description: string;
  type: string;
  parent_lab_id: number;
  created_at: string;
  updated_at: string;
}

/** Заявка (видимая лаборатории). 1 заявка = 1 метод (метод и номера прямо в
 * строке, как в lab-service/sbe-requests после декомпозиции 2026-08-18). */
export interface LimsRequest {
  id: number;
  number_seq: number;
  number_year: number;
  title: string;
  description: string;
  object_id: number;
  project_id: number;
  group_id: number;
  owner_email: string;
  status: string;
  priority: string;
  test_purpose: string;
  ekn: string;
  /** Номер из legacy-системы (email-трекер LPITrack, «LPIZAYAVKINAPRO-<N>») —
   * для заявок переходного периода миграции; у новых заявок пусто. */
  external_id: string;
  /** Метод испытаний (1 заявка = 1 метод). */
  method_id: number;
  /** Конкретная лаборатория из lab_ids метода, выбранная при создании заявки
   * (2026-08-19, заменяет старую external_lab_id — методы теперь могут принадлежать
   * нескольким лабам, поэтому заявка обязана явно фиксировать одну). */
  lab_id: number;
  /** Номер заказчику: {projectCode}-{NNN}/{yyyy}-{labCode}-{methodCode}. */
  customer_number: string;
  /** Номер лаборатории: {NNN}/{yyyy}-{methodCode}. */
  lab_number: string;
  files: Array<{ file_key: string; file_name: string; file_size: number; file_url: string }>;
  created_at: string;
  updated_at: string;
}

/** Объект исследования (справочник lab-service; создание — в sbe-requests). */
export interface LabObject {
  id: number;
  name: string;
  description: string;
  characteristics: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

/** Проект (справочник lab-service; создание/правка — в sbe-requests, здесь
 * только чтение для отображения деталей заявки, как у заявителя). */
export interface LabProject {
  id: number;
  parent_id: number;
  code: string;
  name: string;
  description: string;
  is_ekn: boolean;
  group_id: number;
  owner_email: string;
  created_at: string;
  updated_at: string;
}

/** Группа участников (видимость заявок группы; создание — в sbe-requests). */
export interface LabGroup {
  id: number;
  name: string;
  owner_email: string;
  members: Array<{ email: string; role: string }>;
  created_at: string;
  updated_at: string;
}

/** Тип данных атрибута метода (конфигуратор методов, блок 1). "photo" — значение
 * хранится как URL (в перспективе — фото с мобильного терминала, загруженное в S3);
 * "boolean" (2026-08-22) — значение хранится как настоящий JSON true/false, в UI —
 * Да/Нет. */
export type AttributeDataType = 'text' | 'int' | 'float' | 'date' | 'time' | 'photo' | 'boolean';
/** Знак сравнения — пороговое правило классификации (условие на строку, 2026-08-22). */
export type ComparisonOperator = '==' | '!=' | '<' | '<=' | '>' | '>=';
/** Способ заполнения атрибута. "classification" (2026-08-22) — значение пишет
 * механизм правил классификации (applyClassification, lab-service): атрибут с
 * этим способом заполнения ДОЛЖЕН быть output_name хотя бы одного правила
 * классификации метода — проверяется в валидации (см. validateAttributesAndRules). */
export type AttributeFillMethod = 'manual' | 'instrument' | 'calculated' | 'classification';
/** Уровень атрибута: значение по каждой серии («данные эксперимента») или одно
 * значение на заявку+метод («агрегированные результаты»). */
export type AttributeLevel = 'experiment' | 'aggregated';

/** Простое агрегирование атрибута уровня "aggregated" без своей формулы — сервер сам
 * строит formulas-запись `{method}({source})` (lab-service, deriveFormulasFromAttributes). */
export interface AttributeAggregation {
  source: string;
  method: 'avg' | 'min' | 'max';
}

/** Атрибут метода — элемент methods.input_parameters. Единственный источник formulas
 * для расчётных/агрегированных атрибутов (сервер перестраивает formulas из этого
 * списка при каждом сохранении конфига). */
export interface MethodAttribute {
  id: string;
  name: string;
  data_type: AttributeDataType;
  fill_method: AttributeFillMethod;
  level: AttributeLevel;
  /** DSL-выражение — для fill_method="calculated" (в т.ч. агрегированные атрибуты со
   * сложной формулой, напр. калибровочная интерполяция). */
  formula?: string;
  /** Простая агрегация — для level="aggregated" без своей формулы. */
  aggregation?: AttributeAggregation;
  /** Альтернативные raw-имена этого атрибута (2026-08-21) — позволяет назвать
   * атрибут как удобно в конфигураторе без оглядки на то, как поле называется в
   * legacy-источниках (email-импорт от десктопной ЛИМС); при приёме результатов
   * из письма synonyms сопоставляются с id (см. email_ingest.go resolveResultKey). */
  synonyms?: string[];
}

/** Операнд правой части атомарного сравнения в правиле классификации
 * (2026-08-22v3): литеральное значение (число/текст/"Да"-"Нет"/показатель),
 * другой атрибут текущей записи, или «целевой показатель» заявки
 * (objects.characteristics.target_indicators, sbe-requests). Левая часть
 * сравнения НЕЯВНАЯ — см. ClassificationRule.subjects. */
export type Operand =
  | { kind: 'attribute'; id: string }
  | { kind: 'literal'; value: string | number }
  | { kind: 'target_indicator' };

/** Один атомарный тест «[оцениваемый атрибут] [знак] [сравнить с]» — строительный
 * блок ветки правила (см. ClassificationBranch). Левая часть НЕ упоминается
 * здесь вовсе (по прямой правке пользователя: схема условий должна быть одной
 * на всё правило, без привязки к конкретному атрибуту) — на исполнении сервер
 * подставляет туда значение текущей строки subjects (см. ClassificationRule). */
export interface ClassificationClause {
  operator: ComparisonOperator;
  compare_to: Operand;
}

/** Одна ветка правила — «Если [clauses, объединённые join], то показатель = grade».
 * `clauses` пустой/не задан — безусловная ветка («Иначе», без явного «Если»);
 * ставится последней в списке, заменяет старый неявный фолбэк/спец-поля. Ветки
 * проверяются ПО ПОРЯДКУ массива (сервер не пересортировывает), первая
 * совпавшая — результат. */
export interface ClassificationBranch {
  clauses?: ClassificationClause[];
  /** Как объединяются clauses при их больше одного — везде "И" или везде "ИЛИ"
   * (без вложенных групп — соответствует базовому AND()/OR() из Excel, к которому
   * апеллировал пользователь). По умолчанию "И". */
  join?: 'and' | 'or';
  grade: string;
}

/** Одна строка таблицы «Оцениваемый атрибут» / «Куда записать результат оценки»
 * (2026-08-22v3). Одна и та же схема условий (ClassificationRule.branches)
 * применяется К КАЖДОЙ строке ПО ОТДЕЛЬНОСТИ: значение input_attribute_id
 * подставляется как неявная левая часть во все clauses правила, результат
 * (grade совпавшей ветки) пишется в output_attribute_id. Строк может быть
 * несколько — по прямой формулировке пользователя: «прогоняем оба списка
 * через циклы», т.е. один и тот же набор условий проверяется отдельно для
 * каждой пары (оцениваемый атрибут, куда писать), без взаимного влияния строк
 * друг на друга. */
export interface ClassificationSubject {
  input_attribute_id: string;
  output_attribute_id: string;
}

/** Правило классификации (2026-08-22v3, по прямой правке пользователя — версия
 * v2 привязывала весь набор условий к ОДНОМУ output_name и не давала переиспользовать
 * одну и ту же логику для нескольких атрибутов сразу). Теперь: `branches` —
 * ОДНА схема условий «Если [оцениваемый атрибут] [знак] Б (И/ИЛИ …), то
 * показатель = значение», без упоминания конкретных атрибутов внутри самих
 * условий; `subjects` — динамическая таблица, определяющая, к каким парам
 * (оцениваемый атрибут → куда писать результат) эта схема применяется —
 * может быть несколько строк. Свод по нескольким сериям убран целиком (по
 * прямой правке пользователя) — значение атрибута берётся из текущей записи
 * как есть. Знаки `<`/`<=`/`>`/`>=` при сравнении ДВУХ показателей метода (оба
 * входят в determinable_indicators) трактуются как сравнение по позиции в этом
 * списке — первый введённый показатель считается «большим» (Г1,Г2,Г3,Г4 ->
 * Г1>Г2>Г3>Г4, подтверждено пользователем); иначе — обычное числовое сравнение. */
export interface ClassificationRule {
  branches: ClassificationBranch[];
  subjects: ClassificationSubject[];
}

/** Один ряд графика — источник значений (id атрибута) + подпись в легенде. */
export interface ChartSeriesConfig {
  source_param: string;
  label?: string;
}

/** Конфиг графика — элемент methods.chart_configs (рендерится charts.go, свой
 * PNG без внешних зависимостей). Тип графика/оси/ряды — конфигуратор методов, блок 3. */
export interface ChartConfig {
  id: string;
  title?: string;
  chart_type: 'line' | 'scatter' | 'bar';
  x_column?: string;
  x_label?: string;
  y_label?: string;
  series_config: ChartSeriesConfig[];
}

/** Вид вывода — ровно три фиксированных (2026-08-22, по решению пользователя:
 * "ровно 3, простые галочки" вместо расширяемого списка шаблонов). Состав
 * "выписки" не зафиксирован программно — админ метода сам решает, что и в
 * каком виде туда включить. */
export type PresentationKind = 'ui' | 'excerpt' | 'protocol';

/** Показатель внутри секции — колонка таблицы по сериям ("table") или строка
 * резюме "label: значение" ("summary"); порядок элементов PresentationSection.fields
 * = порядок отображения внутри секции. Три независимых флага (было два до
 * 2026-08-22) — админ решает видимость для каждого из трёх видов отдельно. */
export interface PresentationField {
  attribute_id: string;
  label?: string;
  role: 'table' | 'summary';
  show_in_ui: boolean;
  show_in_excerpt: boolean;
  show_in_protocol: boolean;
}

/** График секции (methods.chart_configs), с той же 3-way видимостью, что и поля —
 * рендерится внутри секции, а не общим хвостом в конце документа. */
export interface PresentationChartRef {
  chart_id: string;
  show_in_ui: boolean;
  show_in_excerpt: boolean;
  show_in_protocol: boolean;
}

/** Тематическая группа показателей (напр. "Температура дымовых газов") — своя
 * мини-таблица/резюме/график, а не общая сводная таблица на весь метод
 * (2026-08-22 — устраняет жалобу "все атрибуты формируют одну длинную сводную
 * таблицу"; эталон структуры — легаси-отчёты десктопного приложения). */
export interface PresentationSection {
  id: string;
  title: string;
  fields: PresentationField[];
  charts?: PresentationChartRef[];
}

/** methods.presentation — секции показателей. Ровно 3 вида вывода читают один
 * и тот же набор секций, отличаясь только фильтром show_in_ui/show_in_excerpt/
 * show_in_protocol на полях/графиках. */
export interface MethodPresentation {
  sections: PresentationSection[];
}

/** Один вводимый испытателем показатель эксперимента — конструктор схемы
 * (2026-08-22). Реальный фронт ввода для лаборанта (мобильный/веб) пока не
 * разрабатывается — здесь только описание формы. */
export interface OperatorFormField {
  attribute_id: string;
  label?: string;
  required: boolean;
  help_text?: string;
}

/** methods.operator_form — схема формы для испытателя. */
export interface MethodOperatorForm {
  fields: OperatorFormField[];
}

/** Метод испытаний (справочник lab-service). Может принадлежать нескольким
 * лабораториям (2026-08-19, method_labs many-to-many) — lab_ids заменяет старую
 * единичную lab_id. */
export interface LabMethod {
  id: number;
  code: string;
  name: string;
  lab_ids: number[];
  description: string;
  determinable_indicators: string[];
  /** Формируется сервером из input_parameters — не редактируется напрямую (2026-08-21). */
  formulas: Array<Record<string, unknown>>;
  classification: ClassificationRule[];
  chart_configs: ChartConfig[];
  input_parameters: MethodAttribute[];
  presentation: MethodPresentation;
  operator_form: MethodOperatorForm;
  created_at: string;
  updated_at: string;
}

/** Результат измерения (строка = серия). */
export interface MeasurementResult {
  id: number;
  request_id: number;
  method_id: number;
  inventor_id: number;
  series_num: number;
  values: Record<string, unknown>;
  file_links: Record<string, unknown>;
  photo_before: string;
  photo_after: string;
  is_statistical_row: boolean;
  calculation_type: string;
  source_series_count: number;
  source_series_range: string;
  created_at: string;
  updated_at: string;
}

/** Агрегированный результат на заявку+метод. */
export interface AggregatedResult {
  id: number;
  request_id: number;
  method_id: number;
  calculation_type: string;
  result_data: Record<string, unknown>;
  source_series_count: number;
  source_series_range: string;
  created_at: string;
  updated_at: string;
}

/** Испытатель. */
export interface Inventor {
  id: number;
  name: string;
  email: string;
  phone: string;
  department: string;
  position: string;
  created_at: string;
  updated_at: string;
}

/** Оборудование. */
export interface Equipment {
  id: number;
  code: string;
  name: string;
  location: string;
  responsible: string;
  last_calibration: string;
  next_calibration: string;
  status: string;
  created_at: string;
  updated_at: string;
}

/** Сотрудник лаборатории. */
export interface LabMember {
  lab_id: number;
  email: string;
  role: string;
}

/** Конфигурация метода (formulas/classification/chart_configs/input_parameters/
 * presentation/operator_form). */
export interface MethodConfig {
  formulas: Array<Record<string, unknown>>;
  classification: ClassificationRule[];
  chart_configs: ChartConfig[];
  input_parameters: MethodAttribute[];
  presentation: MethodPresentation;
  operator_form: MethodOperatorForm;
}

/** Протокол / выписка / краткий вид заявки — HTML+DOCX, вид задаёт kind
 * (см. PresentationKind), передаваемый в LimsSyncService.getProtocol(). */
export interface ProtocolResponse {
  html: string;
  docx_base64: string;
  generated_at: string;
}

/** Одна строка резюме в коротком виде (GET .../short-view). */
export interface ShortViewSummaryRow {
  label: string;
  value: string;
}

/** Одна колонка мини-таблицы короткого вида — is_photo: значение в rows —
 * URL картинки (нужно рендерить <img>), а не текст. */
export interface ShortViewColumn {
  label: string;
  is_photo: boolean;
}

/** Мини-таблица секции в коротком виде — те же колонки/строки, что и в
 * протоколе, но как данные (не HTML), чтобы sbe-lims и sbe-requests могли
 * отрендерить их каждый в своей карточке без дублирования группировки. */
export interface ShortViewTable {
  columns: ShortViewColumn[];
  rows: string[][];
}

/** Одна секция короткого вида (GET /api/lab/requests/{id}/short-view). */
export interface ShortViewSection {
  title: string;
  table?: ShortViewTable;
  summary?: ShortViewSummaryRow[];
}
