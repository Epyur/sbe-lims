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
  /** Системные атрибуты (2026-08-23) — испытатель/даты/условия среды, общие для
   * ЛЮБОГО метода; заполняются автоматически из письма-результата, не через
   * MethodAttribute per-method (см. AGENTS.md, «Системные атрибуты»). Пусто/0,
   * пока не пришли из email-импорта. */
  inventor_id: number;
  report_date: string;
  samples_in_date: string;
  exp_date: string;
  amb_temp: string;
  amb_pres: string;
  amb_moist: string;
  /** Kanban-доска «Очередь лаборатории»: email испытателя (lab_members.email,
   * lab_operator/lab_admin лабы заявки) — назначает руководитель лабы, либо
   * испытатель забирает СЕБЕ неназначенную заявку из "new". Пусто — не назначено. */
  assigned_to: string;
  /** Момент перехода в status="completed" (не updated_at) — основа окна показа
   * в колонке "Завершённые" канбан-доски (10 рабочих дней). Пусто — не завершена. */
  completed_at: string;
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
/** "timeseries" (2026-08-24) — значение не число/текст, а весь ряд датчика целиком
 * ({time, channels, average_temp, derivative}) — заполняется автоматически из письма
 * прибора (synonyms на "mesure_data"), не вводится вручную. См. ChartConfig.kind —
 * единственный способ показать такой атрибут содержательно — через график. */
export type AttributeDataType = 'text' | 'int' | 'float' | 'date' | 'time' | 'photo' | 'boolean' | 'timeseries';
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

/** Один ряд графика "по сериям" (kind не задан) — источник значений (id атрибута) +
 * подпись в легенде. */
export interface ChartSeriesConfig {
  source_param: string;
  label?: string;
}

/** Один ряд графика "по времени" (kind="timeseries", 2026-08-24) — НЕЗАВИСИМЫЙ ряд:
 * свой источник (атрибут data_type="timeseries") + свой канал внутри него. Список из
 * нескольких таких рядов, а не общий source_param+channels на весь график — по прямому
 * запросу пользователя учесть случай "два и более графика... ось X в одних единицах, но
 * пары X-Y не совпадают" (напр. будущий второй датчик с другой частотой опроса). */
export interface TimeseriesSeriesConfig {
  /** id атрибута data_type="timeseries", чьё значение — весь ряд {time, channels,
   * average_temp, derivative}. */
  source_param: string;
  /** какой под-ряд взять — имя канала ("channel_1" и т.п.) или "average_temp"/"derivative". */
  channel: string;
  label?: string;
  /** "y2" — рисовать по второй (правой) оси, со своим масштабом — для наложения ряда
   * другого порядка величины (напр. производная поверх температуры) на одно изображение
   * без взаимного искажения масштаба. Основная (левая) ось — когда не задано. */
  axis?: 'y2';
}

/** Конфиг графика — элемент methods.chart_configs (рендерится charts.go, свой
 * PNG без внешних зависимостей). Тип графика/оси/ряды — конфигуратор методов, блок 3. */
export interface ChartConfig {
  id: string;
  title?: string;
  chart_type: 'line' | 'scatter' | 'bar';
  /** "timeseries" (2026-08-24) — график ВНУТРИ одной серии по точкам датчика
   * (timeseries_series ниже), а не по сериям-повторам (обычный режим — kind не задан,
   * читает x_column/series_config как раньше). */
  kind?: 'timeseries';
  x_column?: string;
  x_label?: string;
  y_label?: string;
  /** Подпись второй (правой) оси Y — есть смысл только если хотя бы один элемент
   * timeseries_series ниже имеет axis:"y2". */
  y2_label?: string;
  /** Ручная настройка шкалы деления по каждой оси (2026-08-24, прямой запрос
   * пользователя: "для каждой оси нужна возможность настраивать точку начала
   * отсчёта и цену деления" — до этого авто-диапазон с отступом 10% мог утащить
   * шкалу в отрицательные значения, которых в данных не было). `*_axis_min` —
   * точка начала отсчёта (первое деление); `*_axis_step` — шаг между соседними
   * делениями. Оба поля у каждой оси независимо опциональны — пусто значит
   * "автоматически" (см. lab-service/charts.go resolveAxisTicks, единственное
   * место, которое их читает через chartAxisSpecFromConfig). */
  x_axis_min?: number;
  x_axis_step?: number;
  y_axis_min?: number;
  y_axis_step?: number;
  y2_axis_min?: number;
  y2_axis_step?: number;
  series_config: ChartSeriesConfig[];
  /** kind="timeseries": список независимых рядов для наложения на одно изображение. */
  timeseries_series?: TimeseriesSeriesConfig[];
}

/** Вид вывода — ровно три фиксированных (2026-08-22, по решению пользователя:
 * "ровно 3, простые галочки" вместо расширяемого списка шаблонов). Состав
 * "выписки" не зафиксирован программно — админ метода сам решает, что и в
 * каком виде туда включить. */
export type PresentationKind = 'ui' | 'excerpt' | 'protocol';

/** Источник плейсхолдера: "system" — заявка/объект (партия, материал, ЕКН,
 * заказчик и т.п. — см. SYSTEM_PLACEHOLDERS в block-editor.ts), "attribute" —
 * показатель метода. */
export type PlaceholderSource = 'system' | 'attribute';

/** Функция свёртки серий эксперимента в одно значение — обязательна для
 * атрибута уровня "experiment", используемого ВНЕ таблицы (2026-08-23, прямое
 * требование пользователя: динамические данные вне таблицы — только одно
 * значение). Внутри RichNode "table" агрегация не нужна — там одна строка на
 * серию. Для системных плейсхолдеров и атрибутов уровня "aggregated" не
 * применяется (там значение уже единственное). */
export type PlaceholderAgg = 'avg' | 'min' | 'max' | 'first' | 'last';

/** Инлайн-узел форматированного текста — обычный текст (с necessary bold/
 * italic/sup/sub) или плейсхолдер-чип. Зеркало Go InlineNode (results.go).
 * sup/sub — верхний/нижний индекс (2026-08-24, по запросу пользователя),
 * взаимоисключающие — UI не должен выставлять оба одновременно. */
export interface InlineNode {
  type: 'text' | 'placeholder';
  text?: string;
  bold?: boolean;
  italic?: boolean;
  sup?: boolean;
  sub?: boolean;
  source?: PlaceholderSource;
  attribute_id?: string;
  agg?: PlaceholderAgg;
}

/** Один столбец динамической таблицы (RichNode "table") — одна строка на
 * серию эксперимента, без агрегации. kind="series_no" — номер серии (1,2,3...)
 * как обычная колонка (раньше жёстко prepend-илась сервером без права
 * пользователя её убрать/переместить/переименовать, 2026-08-23). */
export interface TableColumn {
  kind?: 'attribute' | 'series_no';
  attribute_id?: string;
  label?: string;
}

/** Один блочный узел форматированного текста внутри DocumentBlock.
 * align (2026-08-24) — выравнивание, применимо к paragraph/heading (не к
 * bullet_list/table/static_table — у списка свой маркер, у таблиц своя
 * структура). "static_table" (2026-08-24) — таблица, введённая пользователем
 * вручную (визуальный конструктор), в отличие от "table" (данные серий,
 * авто-заполняемые ячейки, столбцы из TableColumn) — здесь и структура
 * (строки/столбцы), и содержимое каждой ячейки задаются руками; `rows` —
 * строка → колонка → inline-содержимое ячейки (та же модель, что абзац —
 * ячейки сразу получают bold/italic/индексы/плейсхолдеры без отдельной логики). */
export interface RichNode {
  type: 'paragraph' | 'heading' | 'bullet_list' | 'table' | 'static_table';
  level?: 2 | 3 | 4;
  align?: 'center' | 'right' | 'justify';
  children?: InlineNode[];
  items?: InlineNode[][];
  columns?: TableColumn[];
  rows?: InlineNode[][][];
}

/** Один блок документа (напр. "Общая информация", "Результаты измерения
 * температуры") — форматированный текст с плейсхолдерами, собранный в
 * визуальном редакторе (2026-08-23, block-editor.ts). Заменяет секции полей
 * от 2026-08-22 — пользователь явно отверг модель "показать/скрыть атрибут"
 * как не подходящую для документа с реквизитами/описаниями/юридическим
 * футером (см. AGENTS.md). Видимость — ровно 3 фиксированных вида вывода
 * (не расширяемый список шаблонов, по решению пользователя), на уровне
 * ВСЕГО блока (не на уровне отдельных узлов внутри) — если для другого вида
 * нужен другой текст, это отдельный блок с другими галочками. */
export interface DocumentBlock {
  id: string;
  title: string;
  content: RichNode[];
  chart_id?: string;
  show_in_ui: boolean;
  show_in_excerpt: boolean;
  show_in_protocol: boolean;
}

/** methods.presentation — блоки форматированного текста. Ровно 3 вида
 * вывода читают один и тот же список блоков, отличаясь только фильтром
 * show_in_ui/show_in_excerpt/show_in_protocol на блоке. */
export interface MethodPresentation {
  blocks: DocumentBlock[];
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

