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

/** Тип данных атрибута метода (конфигуратор методов, блок 1). */
export type AttributeDataType = 'text' | 'int' | 'float' | 'date' | 'time' | 'photo';
/** Способ заполнения атрибута. */
export type AttributeFillMethod = 'manual' | 'instrument' | 'calculated';
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

/** Правило классификации — "пороговое": вычисленный показатель сравнивается с
 * отсортированными по возрастанию порогами (сервер сортирует сам), берётся первый, где
 * значение <= value, иначе — последний (самый худший). */
export interface ThresholdClassificationRule {
  rule_type: 'threshold';
  parameter_name: string;
  output_name?: string;
  thresholds: Array<{ value: number; grade: string }>;
  aggregation_rule?: 'avg' | 'best' | 'worst';
}

/** Правило классификации — "булево": простое условие над значением атрибута. */
export interface BooleanClassificationRule {
  rule_type: 'boolean';
  parameter_name: string;
  output_name?: string;
  operator: '==' | '!=' | '<' | '<=' | '>' | '>=';
  value: string | number;
  true_grade: string;
  false_grade: string;
}

/** Правило классификации — "соответствие целевому показателю»: сравнение вычисленного
 * показателя с «Целевым показателем» заявки (objects.characteristics.target_indicators,
 * sbe-requests) по порядку determinable_indicators метода. */
export interface ComplianceClassificationRule {
  rule_type: 'compliance';
  parameter_name: string;
  output_name?: string;
  comply_text?: string;
  not_comply_text?: string;
  not_assessed_text?: string;
}

export type ClassificationRule =
  | ThresholdClassificationRule
  | BooleanClassificationRule
  | ComplianceClassificationRule;

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

/** Один столбец таблицы результатов (UI) и/или протокола — элемент
 * methods.presentation.fields; порядок элементов массива = порядок отображения
 * (конфигуратор методов, блок 3, 2026-08-21). */
export interface PresentationField {
  attribute_id: string;
  label?: string;
  show_in_ui: boolean;
  show_in_protocol: boolean;
}

/** methods.presentation — представление данных метода в UI-таблице результатов
 * и в протоколе. Атрибуты, не упомянутые в fields, показываются как раньше (все
 * ключи, без явного порядка) — обратная совместимость для нетронутых методов. */
export interface MethodPresentation {
  fields: PresentationField[];
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

/** Конфигурация метода (formulas/classification/chart_configs/input_parameters). */
export interface MethodConfig {
  formulas: Array<Record<string, unknown>>;
  classification: ClassificationRule[];
  chart_configs: ChartConfig[];
  input_parameters: MethodAttribute[];
  presentation: MethodPresentation;
}

/** Протокол заявки. */
export interface ProtocolResponse {
  html: string;
  docx_base64: string;
  generated_at: string;
}
