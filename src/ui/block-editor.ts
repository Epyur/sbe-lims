import { App, Modal } from 'obsidian';
import { toggleSubSupPalette } from './subsup';
import type {
  ChartConfig,
  DocumentBlock,
  InlineNode,
  MethodAttribute,
  PlaceholderAgg,
  PlaceholderSource,
  RichNode,
  TableColumn,
} from '../types/lims';

/** Каталог системных плейсхолдеров (заявка/объект) — резолвится сервером из
 * уже загруженных данных при рендере (см. lab-service/protocol.go
 * resolveSystemPlaceholder). Список держим синхронизированным вручную с Go —
 * добавление нового системного поля требует правки в обоих местах (и в
 * systemRequestFields/email_ingest.go, если поле заполняется из письма).
 * Экспортирован — переиспользуется справкой конфигуратора методов и
 * автоматическим разделом формы для испытателя (lims-view.ts), см.
 * sbe-lims/AGENTS.md, "Системные атрибуты". */
export const SYSTEM_PLACEHOLDERS: Array<{ id: string; label: string }> = [
  { id: 'title', label: 'Наименование заявки' },
  { id: 'number', label: 'Номер заявки' },
  { id: 'object_name', label: 'Материал (объект)' },
  { id: 'ekn', label: 'ЕКН' },
  { id: 'owner_email', label: 'Заказчик (email)' },
  { id: 'priority', label: 'Приоритет' },
  { id: 'test_purpose', label: 'Цель испытания' },
  { id: 'status', label: 'Статус заявки' },
  { id: 'created_at', label: 'Дата создания заявки' },
  { id: 'customer_number', label: 'Номер заказчику' },
  { id: 'lab_number', label: 'Номер лаборатории' },
  { id: 'target_indicator', label: 'Подтверждаемая характеристика' },
  { id: 'batch_number', label: 'Номер партии' },
  { id: 'sample_id', label: 'Идентификатор образца' },
  { id: 'thickness', label: 'Толщина образца' },
  // Универсальные для ЛЮБОГО метода (2026-08-23) — испытатель/даты/условия среды
  // при испытании; заполняются автоматически из письма-результата (email-импорт),
  // не заводятся как атрибут конкретного метода (см. AGENTS.md).
  { id: 'inventor', label: 'Испытатель (ФИО)' },
  { id: 'report_date', label: 'Дата протокола' },
  { id: 'samples_in_date', label: 'Дата поступления материала' },
  { id: 'exp_date', label: 'Дата эксперимента' },
  { id: 'amb_temp', label: 'Температура воздуха при испытании, °C' },
  { id: 'amb_pres', label: 'Атмосферное давление при испытании, мм.рт.ст' },
  { id: 'amb_moist', label: 'Влажность воздуха при испытании, %' },
];

const AGG_LABELS: Record<PlaceholderAgg, string> = {
  avg: 'среднее', min: 'минимальное', max: 'максимальное', first: 'первая серия', last: 'последняя серия',
};

/** Подпись атрибута для пикеров/списков — фото помечены иконкой (2026-08-24,
 * по жалобе "не понятно, как вставлять фотографии в отчёт"): без неё
 * photo-атрибут неотличим от обычного текстового в общем списке. */
function attrDisplayName(attr: { name?: string; id: string; data_type?: string } | undefined, fallbackId: string): string {
  const base = attr?.name || fallbackId || '?';
  return attr?.data_type === 'photo' ? `📷 ${base}` : base;
}

export interface BlockEditorDeps {
  app: App;
  attrs: MethodAttribute[];
  charts: ChartConfig[];
}

/** Заголовок чипа-плейсхолдера — используется и при вставке, и при построении
 * DOM из уже сохранённого AST (перезагрузка редактора). */
function resolvePlaceholderLabel(n: InlineNode, attrs: MethodAttribute[]): string {
  if (n.source === 'system') {
    return SYSTEM_PLACEHOLDERS.find(s => s.id === n.attribute_id)?.label || n.attribute_id || '?';
  }
  const attr = attrs.find(a => a.id === n.attribute_id);
  const base = attrDisplayName(attr, n.attribute_id || '?');
  if (n.agg) return `${base} (${AGG_LABELS[n.agg] || n.agg})`;
  return base;
}

function buildChipEl(n: InlineNode, attrs: MethodAttribute[]): HTMLElement {
  const chip = document.createElement('span');
  chip.className = 'tn-lims-chip';
  chip.contentEditable = 'false';
  chip.setAttribute('data-source', n.source || 'system');
  chip.setAttribute('data-attribute-id', n.attribute_id || '');
  if (n.agg) chip.setAttribute('data-agg', n.agg);
  chip.textContent = resolvePlaceholderLabel(n, attrs);
  return chip;
}

/** Строит DOM строки из AST (первичная отрисовка/перезагрузка — дальше DOM
 * редактируется пользователем напрямую через contenteditable, не перестраивается
 * на каждое нажатие клавиши). */
function renderInlineNodesIntoDOM(container: HTMLElement, nodes: InlineNode[], attrs: MethodAttribute[]): void {
  container.empty();
  for (const n of nodes) {
    if (n.type === 'placeholder') {
      container.appendChild(buildChipEl(n, attrs));
      continue;
    }
    let el: Node = document.createTextNode(n.text || '');
    if (n.bold) { const b = document.createElement('b'); b.appendChild(el); el = b; }
    if (n.italic) { const i = document.createElement('i'); i.appendChild(el); el = i; }
    if (n.sup) { const sup = document.createElement('sup'); sup.appendChild(el); el = sup; }
    else if (n.sub) { const sub = document.createElement('sub'); sub.appendChild(el); el = sub; }
    container.appendChild(el);
  }
}

/** Обход DOM строки обратно в AST (после правки текста/чипов пользователем) —
 * плоский, предсказуемый набор узлов (текст, b/strong, i/em, .tn-lims-chip) —
 * paste вставляет только plainText (см. renderInlineEditable), посторонние
 * теги не протаскиваются. */
function domToInlineNodes(root: HTMLElement): InlineNode[] {
  const out: InlineNode[] = [];
  const walk = (node: Node, bold: boolean, italic: boolean, sup: boolean, sub: boolean): void => {
    if (node.nodeType === Node.TEXT_NODE) {
      const text = node.textContent || '';
      if (text) {
        out.push({
          type: 'text', text, bold: bold || undefined, italic: italic || undefined,
          sup: sup || undefined, sub: (!sup && sub) || undefined,
        });
      }
      return;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return;
    const el = node as HTMLElement;
    if (el.classList.contains('tn-lims-chip')) {
      out.push({
        type: 'placeholder',
        source: (el.getAttribute('data-source') as PlaceholderSource) || 'system',
        attribute_id: el.getAttribute('data-attribute-id') || '',
        agg: (el.getAttribute('data-agg') as PlaceholderAgg) || undefined,
        bold: bold || undefined,
        italic: italic || undefined,
        sup: sup || undefined,
        sub: (!sup && sub) || undefined,
      });
      return;
    }
    const tag = el.tagName.toLowerCase();
    const style = el.getAttribute('style') || '';
    const nextBold = bold || tag === 'b' || tag === 'strong' || /font-weight:\s*(bold|[6-9]00)/i.test(style);
    const nextItalic = italic || tag === 'i' || tag === 'em' || /font-style:\s*italic/i.test(style);
    // sup/sub взаимоисключающие (2026-08-24) — если оба тега умудрились
    // вложиться друг в друга (execCommand такого не делает, но paste — в
    // теории могло бы, если не был бы санитизирован до plainText), sup
    // побеждает: см. приоритет !sup&&sub при записи текстового узла выше.
    const nextSup = sup || tag === 'sup';
    const nextSub = sub || tag === 'sub';
    for (const child of Array.from(el.childNodes)) walk(child, nextBold, nextItalic, nextSup, nextSub);
  };
  for (const child of Array.from(root.childNodes)) walk(child, false, false, false, false);
  return out;
}

function insertChipAtCursor(editable: HTMLElement, chip: HTMLElement): void {
  editable.focus();
  const sel = window.getSelection();
  if (!sel || sel.rangeCount === 0 || !editable.contains(sel.anchorNode)) {
    editable.appendChild(chip);
    editable.appendChild(document.createTextNode(' '));
    return;
  }
  const range = sel.getRangeAt(0);
  range.deleteContents();
  range.insertNode(chip);
  const space = document.createTextNode(' ');
  chip.after(space);
  range.setStartAfter(space);
  range.setEndAfter(space);
  sel.removeAllRanges();
  sel.addRange(range);
}

/** Одна редактируемая строка (абзац/заголовок/пункт списка) — небольшой
 * contenteditable под полным контролем (не единый canvas на блок), с тулбаром
 * Ж/К/Плейсхолдер. getNodes/setNodes — текущее содержимое читается/пишется
 * без перерисовки DOM (перерисовка на каждое нажатие клавиши убила бы фокус/
 * позицию курсора) — редрав нужен только для СТРУКТУРНЫХ изменений выше
 * (добавить/удалить строку и т.п.), не для правки текста. */
function renderInlineEditable(
  container: HTMLElement,
  initialNodes: InlineNode[],
  setNodes: (nodes: InlineNode[]) => void,
  deps: BlockEditorDeps,
): void {
  const toolbar = container.createDiv({ cls: 'tn-lims-flex' });
  const boldBtn = toolbar.createEl('button', { text: 'Ж', cls: 'tn-btn tn-btn-ghost', attr: { title: 'Жирный' } });
  const italicBtn = toolbar.createEl('button', { text: 'К', cls: 'tn-btn tn-btn-ghost', attr: { title: 'Курсив' } });
  // Верхний/нижний индекс (2026-08-24, по запросу пользователя — "во всех
  // элементах... как это сделано в настройках атрибутов", но там из-за plain
  // <input> — юникод-символ; тут настоящий contenteditable, execCommand даёт
  // реальные <sup>/<sub>, как Ж/К через execCommand('bold'/'italic') выше —
  // браузер сам взаимоисключает superscript/subscript на одном выделении.
  const supBtn = toolbar.createEl('button', { text: 'x²', cls: 'tn-btn tn-btn-ghost', attr: { title: 'Верхний индекс' } });
  const subBtn = toolbar.createEl('button', { text: 'x₂', cls: 'tn-btn tn-btn-ghost', attr: { title: 'Нижний индекс' } });
  const placeholderBtn = toolbar.createEl('button', { text: '🏷 Плейсхолдер', cls: 'tn-btn tn-btn-ghost' });

  const editable = container.createDiv({ cls: 'tn-lims-rich-line', attr: { contenteditable: 'true' } });
  renderInlineNodesIntoDOM(editable, initialNodes, deps.attrs);

  const serialize = (): void => setNodes(domToInlineNodes(editable));

  boldBtn.addEventListener('mousedown', (ev) => ev.preventDefault());
  boldBtn.addEventListener('click', () => { editable.focus(); document.execCommand('bold'); serialize(); });
  italicBtn.addEventListener('mousedown', (ev) => ev.preventDefault());
  italicBtn.addEventListener('click', () => { editable.focus(); document.execCommand('italic'); serialize(); });
  supBtn.addEventListener('mousedown', (ev) => ev.preventDefault());
  supBtn.addEventListener('click', () => { editable.focus(); document.execCommand('superscript'); serialize(); });
  subBtn.addEventListener('mousedown', (ev) => ev.preventDefault());
  subBtn.addEventListener('click', () => { editable.focus(); document.execCommand('subscript'); serialize(); });
  placeholderBtn.addEventListener('click', () => {
    new PlaceholderPickerModal(deps.app, deps.attrs, (node) => {
      insertChipAtCursor(editable, buildChipEl(node, deps.attrs));
      serialize();
    }).open();
  });

  editable.addEventListener('input', serialize);
  editable.addEventListener('paste', (ev) => {
    ev.preventDefault();
    const text = ev.clipboardData?.getData('text/plain') || '';
    document.execCommand('insertText', false, text);
  });
}

/** Модалка выбора плейсхолдера — системные / агрегированные (сразу) /
 * эксперимента (доп. шаг — выбор одного значения серии: avg/min/max/first/last,
 * т.к. вне таблицы динамические данные обязаны свернуться до одного значения,
 * прямое требование пользователя 2026-08-23). */
class PlaceholderPickerModal extends Modal {
  private attrs: MethodAttribute[];
  private onPick: (node: InlineNode) => void;

  constructor(app: App, attrs: MethodAttribute[], onPick: (node: InlineNode) => void) {
    super(app);
    this.attrs = attrs;
    this.onPick = onPick;
  }

  onOpen(): void {
    this.renderList();
  }

  private renderList(): void {
    this.contentEl.empty();
    this.titleEl.setText('Вставить плейсхолдер');

    this.contentEl.createDiv({ cls: 'tn-lims-meta' }).setText('Системные (заявка/объект):');
    for (const s of SYSTEM_PLACEHOLDERS) {
      this.buildRow(s.label, () => this.pick({ type: 'placeholder', source: 'system', attribute_id: s.id }));
    }

    const aggregated = this.attrs.filter(a => a.level === 'aggregated');
    if (aggregated.length > 0) {
      this.contentEl.createDiv({ cls: 'tn-lims-meta tn-lims-mt8' }).setText('Агрегированные результаты:');
      for (const a of aggregated) {
        this.buildRow(attrDisplayName(a, a.id), () => this.pick({ type: 'placeholder', source: 'attribute', attribute_id: a.id }));
      }
    }

    const experiment = this.attrs.filter(a => a.level === 'experiment');
    if (experiment.length > 0) {
      this.contentEl.createDiv({ cls: 'tn-lims-meta tn-lims-mt8' }).setText(
        'Атрибуты эксперимента (нужно выбрать одно значение серии; 📷 — фотография, вставляется как изображение):',
      );
      for (const a of experiment) {
        // Фото — не число: "среднее/минимальное/максимальное" для него не имеют
        // смысла (2026-08-24, по жалобе "не понятно, как вставлять фотографии") —
        // предлагаем только выбор серии, а не полный список агрегаций.
        this.buildRow(attrDisplayName(a, a.id), () => this.renderAggChoice(a, a.data_type === 'photo'));
      }
    }
  }

  private renderAggChoice(attr: MethodAttribute, photoOnly: boolean): void {
    this.contentEl.empty();
    this.titleEl.setText(`${attrDisplayName(attr, attr.id)} — ${photoOnly ? 'фото какой серии показать?' : 'какое значение серии?'}`);
    const opts: PlaceholderAgg[] = photoOnly ? ['first', 'last'] : ['avg', 'min', 'max', 'first', 'last'];
    for (const agg of opts) {
      this.buildRow(AGG_LABELS[agg], () => this.pick({ type: 'placeholder', source: 'attribute', attribute_id: attr.id, agg }));
    }
    const backBtn = this.contentEl.createEl('button', { text: '← Назад', cls: 'tn-btn tn-btn-ghost tn-lims-mt8' });
    backBtn.addEventListener('click', () => this.renderList());
  }

  private buildRow(label: string, onClick: () => void): void {
    const btn = this.contentEl.createEl('button', { text: label, cls: 'tn-btn tn-btn-ghost tn-btn-block' });
    btn.addEventListener('click', onClick);
  }

  private pick(node: InlineNode): void {
    this.onPick(node);
    this.close();
  }

  onClose(): void {
    this.contentEl.empty();
  }
}

/** Таблица (RichNode "table") — НЕ contenteditable: структурный виджет, выбор
 * колонок из атрибутов уровня "experiment" (одна строка на серию при рендере,
 * ячейки не редактируются текстом — динамические данные обязаны идти только
 * табличной формой, прямое требование пользователя). Порядок колонок —
 * drag-reorder (тот же паттерн, что у блоков/строк содержимого); "Серия" —
 * обычная колонка (kind="series_no"), не жёстко prepend-ится сервером, как
 * раньше — пользователь сам решает, включать её и куда поставить (2026-08-23,
 * по жалобе "отсутствует опция создания колонки с номером серии"). */
function renderTableNodeEditor(container: HTMLElement, node: RichNode, attrs: MethodAttribute[], onStructuralChange: () => void): void {
  if (!node.columns) node.columns = [];
  container.createDiv({ cls: 'tn-lims-meta' }).setText('Колонки таблицы (одна строка — одна серия эксперимента, ⠿ — перетащить для смены порядка; 📷 — колонка с фотографией, автоматически показывается как изображение):');
  const listEl = container.createDiv();
  let dragFromIdx: number | null = null;
  const redraw = (): void => {
    listEl.empty();
    node.columns!.forEach((col: TableColumn, idx: number) => {
      const row = listEl.createDiv({ cls: 'tn-lims-flex', attr: { draggable: 'true' } });
      row.style.cursor = 'grab';
      row.addEventListener('dragstart', (ev) => { dragFromIdx = idx; ev.stopPropagation(); });
      row.addEventListener('dragover', (ev) => ev.preventDefault());
      row.addEventListener('drop', (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        if (dragFromIdx === null || dragFromIdx === idx) return;
        const [moved] = node.columns!.splice(dragFromIdx, 1);
        node.columns!.splice(idx, 0, moved);
        dragFromIdx = null;
        redraw();
      });
      row.createSpan({ text: '⠿', cls: 'tn-lims-meta' });
      if (col.kind === 'series_no') {
        row.createSpan({ text: 'Серия (номер по порядку)' });
      } else {
        const attr = attrs.find(a => a.id === col.attribute_id);
        row.createSpan({ text: attrDisplayName(attr, col.attribute_id || '') });
      }
      const labelInput = row.createEl('input', {
        attr: { type: 'text', placeholder: col.kind === 'series_no' ? 'подпись (по умолч. «Серия»)' : 'подпись (опц.)' },
        cls: 'tn-lims-input',
      });
      labelInput.value = col.label || '';
      labelInput.addEventListener('change', () => { col.label = labelInput.value.trim() || undefined; });
      // x² — тот же юникод-приём, что у названия атрибута (subsup.ts): подпись
      // колонки — plain input, execCommand неприменим (2026-08-24).
      const colSupBtn = row.createEl('button', { text: 'x²', cls: 'tn-btn tn-btn-ghost', attr: { title: 'Вставить надстрочный/подстрочный символ' } });
      colSupBtn.addEventListener('click', (e) => { e.preventDefault(); toggleSubSupPalette(row, labelInput); });
      const delBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => { node.columns!.splice(idx, 1); onStructuralChange(); });
    });
  };
  redraw();
  const addRow = container.createDiv({ cls: 'tn-lims-flex' });
  const select = addRow.createEl('select', { cls: 'tn-lims-select' });
  select.createEl('option', { attr: { value: '' }, text: '— выбрать колонку —' });
  if (!node.columns!.some((c: TableColumn) => c.kind === 'series_no')) {
    select.createEl('option', { attr: { value: '__series_no__' }, text: 'Серия (номер по порядку)' });
  }
  for (const a of attrs.filter(a => a.level === 'experiment')) {
    if (node.columns!.some((c: TableColumn) => c.attribute_id === a.id)) continue;
    select.createEl('option', { attr: { value: a.id }, text: attrDisplayName(a, a.id) });
  }
  const addBtn = addRow.createEl('button', { text: '➕ Колонка', cls: 'tn-btn tn-btn-ghost' });
  addBtn.addEventListener('click', () => {
    if (!select.value) return;
    if (select.value === '__series_no__') {
      node.columns!.push({ kind: 'series_no' });
    } else {
      node.columns!.push({ kind: 'attribute', attribute_id: select.value });
    }
    onStructuralChange();
  });
}

// Перетаскивание пунктов списка (2026-08-24, по запросу пользователя) — тот же
// паттерн, что уже есть в этом файле для колонок таблицы (renderTableNodeEditor)
// и строк содержимого блока (renderBlockEditor): draggable-строка + ⠿-хэндл +
// dragstart/dragover/drop + splice/onStructuralChange (полный редрав списка).
/** Статическая таблица (RichNode "static_table", 2026-08-24) — визуальный
 * конструктор: пользователь сам задаёт число строк/столбцов и содержимое КАЖДОЙ
 * ячейки (мини rich-text через renderInlineEditable — та же модель, что абзац,
 * ячейки сразу получают bold/italic/индексы/плейсхолдеры без отдельной логики).
 * В отличие от RichNode "table" (данные серий, авто-заполняемые ячейки) —
 * здесь ничего не подставляется автоматически из результатов эксперимента. */
function renderStaticTableEditor(
  container: HTMLElement,
  node: RichNode,
  deps: BlockEditorDeps,
  onStructuralChange: () => void,
): void {
  if (!node.rows || node.rows.length === 0) node.rows = [[[], []], [[], []]]; // 2×2 по умолчанию
  const rows = node.rows;
  const colCount = rows[0]?.length || 1;

  container.createDiv({ cls: 'tn-lims-meta' }).setText('Статическая таблица — содержимое каждой ячейки вводится вручную:');
  const table = container.createEl('table', { cls: 'tn-table' });

  // строка управления столбцами — ✖ под каждым столбцом + ➕ столбец в конце
  const colControlRow = table.createEl('tr');
  colControlRow.createEl('td'); // угловая ячейка — под управление строками
  for (let c = 0; c < colCount; c++) {
    const td = colControlRow.createEl('td');
    const delColBtn = td.createEl('button', { text: '✖ столбец', cls: 'tn-btn tn-btn-ghost' });
    delColBtn.addEventListener('click', () => {
      for (const r of rows) r.splice(c, 1);
      onStructuralChange();
    });
  }
  const addColTd = colControlRow.createEl('td');
  const addColBtn = addColTd.createEl('button', { text: '➕ столбец', cls: 'tn-btn tn-btn-ghost' });
  addColBtn.addEventListener('click', () => {
    for (const r of rows) r.push([]);
    onStructuralChange();
  });

  rows.forEach((row, ri) => {
    const tr = table.createEl('tr');
    const rowCtlTd = tr.createEl('td');
    const delRowBtn = rowCtlTd.createEl('button', { text: '✖ строка', cls: 'tn-btn tn-btn-ghost' });
    delRowBtn.addEventListener('click', () => { rows.splice(ri, 1); onStructuralChange(); });
    row.forEach((cell, ci) => {
      const td = tr.createEl('td');
      renderInlineEditable(td, cell, (nodes) => { rows[ri][ci] = nodes; }, deps);
    });
  });

  const addRowTr = table.createEl('tr');
  const addRowTd = addRowTr.createEl('td');
  const addRowBtn = addRowTd.createEl('button', { text: '➕ строка', cls: 'tn-btn tn-btn-ghost' });
  addRowBtn.addEventListener('click', () => {
    rows.push(Array.from({ length: colCount }, () => []));
    onStructuralChange();
  });
}

function renderBulletListEditor(container: HTMLElement, node: RichNode, deps: BlockEditorDeps, onStructuralChange: () => void): void {
  if (!node.items) node.items = [];
  let dragFromIdx: number | null = null;
  node.items.forEach((item: InlineNode[], idx: number) => {
    const row = container.createDiv({ cls: 'tn-lims-flex', attr: { draggable: 'true' } });
    row.style.cursor = 'grab';
    row.addEventListener('dragstart', (ev) => { dragFromIdx = idx; ev.stopPropagation(); });
    row.addEventListener('dragover', (ev) => ev.preventDefault());
    row.addEventListener('drop', (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      if (dragFromIdx === null || dragFromIdx === idx) return;
      const [moved] = node.items!.splice(dragFromIdx, 1);
      node.items!.splice(idx, 0, moved);
      dragFromIdx = null;
      onStructuralChange();
    });
    row.createSpan({ text: '⠿ •', cls: 'tn-lims-meta' });
    const lineWrap = row.createDiv();
    renderInlineEditable(lineWrap, item, (nodes) => { node.items![idx] = nodes; }, deps);
    const delBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    delBtn.addEventListener('click', () => { node.items!.splice(idx, 1); onStructuralChange(); });
  });
  const addBtn = container.createEl('button', { text: '➕ Пункт списка', cls: 'tn-btn tn-btn-ghost' });
  addBtn.addEventListener('click', () => { node.items!.push([]); onStructuralChange(); });
}

/** Выравнивание абзаца/заголовка (2026-08-24, по запросу пользователя — "в
 * абзаце настройки выравнивания (по ширине, центр, право, лево)"). Применимо к
 * paragraph/heading — оба блочные текстовые узлы; для bullet_list/table не
 * имеет смысла (список — свой маркер слева, таблица — своя ширина колонок). */
function renderAlignSelect(container: HTMLElement, node: RichNode): void {
  const row = container.createDiv({ cls: 'tn-lims-flex' });
  row.createSpan({ text: 'Выравнивание:', cls: 'tn-lims-meta' });
  const select = row.createEl('select', { cls: 'tn-lims-select' });
  const OPTIONS: Array<[string, string]> = [
    ['', 'слева (по умолч.)'], ['center', 'по центру'], ['right', 'справа'], ['justify', 'по ширине'],
  ];
  for (const [val, label] of OPTIONS) select.createEl('option', { attr: { value: val }, text: label });
  select.value = node.align || '';
  select.addEventListener('change', () => {
    node.align = (select.value || undefined) as RichNode['align'];
  });
}

/** Один узел содержимого блока — заголовок редактора зависит от типа.
 * onStructuralChange — единственная точка перерисовки: она всегда полностью
 * перестраивает весь список блоков сверху (см. LimsView.renderBlocksList),
 * поэтому здесь НЕ нужен свой локальный редрав перед её вызовом — локальный
 * редрав, не очищающий контейнер, и был причиной "блок дублируется"/
 * "невозможно удалить" (обнаружено пользователем, 2026-08-23). */
function renderRichNodeEditor(
  container: HTMLElement,
  node: RichNode,
  deps: BlockEditorDeps,
  onStructuralChange: () => void,
): void {
  if (node.type === 'heading') {
    const head = container.createDiv({ cls: 'tn-lims-flex' });
    head.createSpan({ text: 'Заголовок, уровень:' });
    const levelSelect = head.createEl('select', { cls: 'tn-lims-select' });
    for (const lvl of [2, 3, 4]) levelSelect.createEl('option', { attr: { value: String(lvl) }, text: String(lvl) });
    levelSelect.value = String(node.level || 3);
    levelSelect.addEventListener('change', () => { node.level = (Number(levelSelect.value) || 3) as 2 | 3 | 4; });
    renderAlignSelect(container, node);
    if (!node.children) node.children = [];
    renderInlineEditable(container, node.children, (nodes) => { node.children = nodes; }, deps);
    return;
  }
  if (node.type === 'bullet_list') {
    renderBulletListEditor(container, node, deps, onStructuralChange);
    return;
  }
  if (node.type === 'table') {
    renderTableNodeEditor(container, node, deps.attrs, onStructuralChange);
    return;
  }
  if (node.type === 'static_table') {
    renderStaticTableEditor(container, node, deps, onStructuralChange);
    return;
  }
  // "paragraph"
  renderAlignSelect(container, node);
  if (!node.children) node.children = [];
  renderInlineEditable(container, node.children, (nodes) => { node.children = nodes; }, deps);
}

const NODE_TYPE_LABEL: Record<RichNode['type'], string> = {
  paragraph: 'Абзац', heading: 'Заголовок', bullet_list: 'Список', table: 'Таблица',
  static_table: 'Статическая таблица',
};

/** Редактор ОДНОГО блока документа — список строк (абзац/заголовок/список/
 * таблица), управляется кнопками; порядок строк — drag-reorder. Плюс выбор
 * привязанного графика (из уже настроенных в блоке «Графики» метода). */
export function renderBlockEditor(
  container: HTMLElement,
  block: DocumentBlock,
  deps: BlockEditorDeps,
  onStructuralChange: () => void,
): void {
  const rowsEl = container.createDiv();
  let dragFromIdx: number | null = null;

  // Единственный редрав при структурных изменениях — onStructuralChange
  // (полная перестройка всего списка блоков сверху, см. LimsView.renderBlocksList);
  // здесь строим DOM только один раз при открытии редактора.
  block.content.forEach((node, idx) => {
    const row = rowsEl.createDiv({ cls: 'tn-lims-method', attr: { draggable: 'true' } });
    row.style.cursor = 'grab';
    row.addEventListener('dragstart', (ev) => { dragFromIdx = idx; ev.stopPropagation(); });
    row.addEventListener('dragover', (ev) => ev.preventDefault());
    row.addEventListener('drop', (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      if (dragFromIdx === null || dragFromIdx === idx) return;
      const [moved] = block.content.splice(dragFromIdx, 1);
      block.content.splice(idx, 0, moved);
      dragFromIdx = null;
      onStructuralChange();
    });

    const head = row.createDiv({ cls: 'tn-lims-flex' });
    head.createSpan({ text: `⠿ ${NODE_TYPE_LABEL[node.type]}`, cls: 'tn-lims-meta' });
    const delBtn = head.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    delBtn.addEventListener('click', () => {
      block.content.splice(idx, 1);
      onStructuralChange();
    });

    const bodyEl = row.createDiv();
    renderRichNodeEditor(bodyEl, node, deps, onStructuralChange);
  });

  const addRow = container.createDiv({ cls: 'tn-lims-flex' });
  const addParagraphBtn = addRow.createEl('button', { text: '➕ Абзац', cls: 'tn-btn tn-btn-ghost' });
  addParagraphBtn.addEventListener('click', () => {
    block.content.push({ type: 'paragraph', children: [] });
    onStructuralChange();
  });
  const addHeadingBtn = addRow.createEl('button', { text: '➕ Заголовок', cls: 'tn-btn tn-btn-ghost' });
  addHeadingBtn.addEventListener('click', () => {
    block.content.push({ type: 'heading', level: 3, children: [] });
    onStructuralChange();
  });
  const addListBtn = addRow.createEl('button', { text: '➕ Список', cls: 'tn-btn tn-btn-ghost' });
  addListBtn.addEventListener('click', () => {
    block.content.push({ type: 'bullet_list', items: [] });
    onStructuralChange();
  });
  const addTableBtn = addRow.createEl('button', { text: '➕ Таблица', cls: 'tn-btn tn-btn-ghost' });
  addTableBtn.addEventListener('click', () => {
    // По умолчанию с колонкой "Серия" — раньше сервер всегда prepend-ил её
    // implicit; так поведение по умолчанию не меняется, но теперь колонку
    // можно убрать/переместить/переименовать (2026-08-23).
    block.content.push({ type: 'table', columns: [{ kind: 'series_no' }] });
    onStructuralChange();
  });
  const addStaticTableBtn = addRow.createEl('button', { text: '➕ Статическая таблица', cls: 'tn-btn tn-btn-ghost' });
  addStaticTableBtn.addEventListener('click', () => {
    block.content.push({ type: 'static_table', rows: [[[], []], [[], []]] });
    onStructuralChange();
  });

  if (deps.charts.length > 0) {
    const chartRow = container.createDiv({ cls: 'tn-lims-flex tn-lims-mt8' });
    chartRow.createSpan({ text: 'График блока:' });
    const chartSelect = chartRow.createEl('select', { cls: 'tn-lims-select' });
    chartSelect.createEl('option', { attr: { value: '' }, text: '— нет —' });
    for (const c of deps.charts) {
      chartSelect.createEl('option', { attr: { value: c.id }, text: c.title || c.id });
    }
    chartSelect.value = block.chart_id || '';
    chartSelect.addEventListener('change', () => { block.chart_id = chartSelect.value || undefined; });
  }
}
