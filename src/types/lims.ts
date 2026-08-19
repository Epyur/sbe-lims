/** Типы плагина «ЛИМС» (sbe-lims). Модель совместима с lab-service. */

/** Лаборатория. */
export interface Lab {
  id: number;
  code: string;
  name: string;
  description: string;
  type: string;
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
  external_lab_id: number;
  ekn: string;
  /** Метод испытаний (1 заявка = 1 метод). */
  method_id: number;
  /** Номер заказчику: {projectCode}-{NNN}/{yyyy}-{labCode}-{methodCode}. */
  customer_number: string;
  /** Номер лаборатории: {NNN}/{yyyy}-{methodCode}. */
  lab_number: string;
  files: Array<{ file_key: string; file_name: string; file_size: number; file_url: string }>;
  created_at: string;
  updated_at: string;
}

/** Метод испытаний (справочник lab-service). */
export interface LabMethod {
  id: number;
  code: string;
  name: string;
  lab_id: number;
  description: string;
  determinable_indicators: string[];
  formulas: Array<Record<string, unknown>>;
  classification: Array<Record<string, unknown>>;
  chart_configs: Array<Record<string, unknown>>;
  input_parameters: Array<Record<string, unknown>>;
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
  classification: Array<Record<string, unknown>>;
  chart_configs: Array<Record<string, unknown>>;
  input_parameters: Array<Record<string, unknown>>;
}

/** Протокол заявки. */
export interface ProtocolResponse {
  html: string;
  docx_base64: string;
  generated_at: string;
}
