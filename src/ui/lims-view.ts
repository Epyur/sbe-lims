import { ItemView, Notice, WorkspaceLeaf } from 'obsidian';
import type SbeLimsPlugin from '../main';
import type { LimsRequest, MeasurementResult, MethodConfig, Lab } from '../types/lims';
import { errorMessage } from '../../../sbe-core/src/utils/errors';

export const SBE_LIMS_VIEW_TYPE = 'sbe-lims-view';

const STATUS_LABELS: Record<string, string> = {
  new: '🟢 Новая',
  received: '📦 Образцы получены',
  processing: '🟡 В работе',
  completed: '✅ Завершена',
};

/** Ключи разделов дерева навигации (фасад; наполнение подключается позже). */
type NavKey =
  | 'requests'
  | 'queue'
  | 'methods'
  | 'objects'
  | 'results'
  | 'inventors'
  | 'equipment'
  | 'lab-members';

interface NavItem {
  key: NavKey;
  label: string;
  sub: string;
}

interface NavGroup {
  id: string;
  icon: string;
  label: string;
  items: NavItem[];
}

/** Группы дерева навигации (по мокапу tn-center, адаптировано под данные lab-service). */
const NAV_GROUPS: NavGroup[] = [
  {
    id: 'req',
    icon: '📋',
    label: 'Заявки',
    items: [
      { key: 'requests', label: 'Все заявки', sub: 'Доступные вам заявки' },
      { key: 'queue', label: 'Очередь лаборатории', sub: 'Заявки, ещё не взятые в работу' },
    ],
  },
  {
    id: 'lab',
    icon: '🧪',
    label: 'Лаборатория',
    items: [
      { key: 'methods', label: 'Методы', sub: 'Каталог методов испытаний' },
      { key: 'objects', label: 'Объекты', sub: 'Объекты исследования' },
      { key: 'results', label: 'Результаты и протоколы', sub: 'Завершённые заявки' },
      { key: 'inventors', label: 'Испытатели', sub: 'Справочник' },
      { key: 'equipment', label: 'Оборудование', sub: 'Справочник' },
      { key: 'lab-members', label: 'Сотрудники', sub: 'Состав и права лаборатории' },
    ],
  },
];

const PAGE_META: Record<NavKey, { title: string; sub: string }> = {
  requests: { title: 'Все заявки', sub: 'Доступные вам заявки' },
  queue: { title: 'Очередь лаборатории', sub: 'Заявки, ещё не взятые в работу' },
  methods: { title: 'Методы', sub: 'Каталог методов испытаний' },
  objects: { title: 'Объекты', sub: 'Объекты исследования' },
  results: { title: 'Результаты и протоколы', sub: 'Завершённые заявки' },
  inventors: { title: 'Испытатели', sub: 'Справочник' },
  equipment: { title: 'Оборудование', sub: 'Справочник' },
  'lab-members': { title: 'Сотрудники', sub: 'Состав и права лаборатории' },
};

export class LimsView extends ItemView {
  plugin: SbeLimsPlugin;

  private rootEl!: HTMLElement;
  private navEl!: HTMLElement;
  private contentBoxEl!: HTMLElement;
  private pageTitleEl!: HTMLElement;
  private pageSubEl!: HTMLElement;
  private crumbEl!: HTMLElement;
  private labBarEl!: HTMLElement;
  private labSwitchEl!: HTMLSelectElement;

  private key: NavKey = 'requests';
  private labId: number | null = null;
  private collapsed = false;
  private labs: Lab[] = [];
  private myRole = '';

  private bodyEl!: HTMLElement;

  constructor(leaf: WorkspaceLeaf, plugin: SbeLimsPlugin) {
    super(leaf);
    this.plugin = plugin;
  }

  getViewType(): string {
    return SBE_LIMS_VIEW_TYPE;
  }

  getDisplayText(): string {
    return 'Лабораторная информационная менеджмент система СБЕ ПМиПИР';
  }

  getIcon(): string {
    return 'flask-conical';
  }

  async onOpen(): Promise<void> {
    const container = this.contentEl;
    container.addClass('tn-lims-container');
    this.rootEl = container.createDiv({ cls: 'tn-lims-app' });

    this.buildShell();
    await this.initShell();
  }

  refresh(): void {
    void this.renderPage();
  }

  // ---- Каркас ----

  private buildShell(): void {
    // шапка
    const topbar = this.rootEl.createDiv({ cls: 'tn-lims-topbar' });
    topbar.createDiv({ cls: 'tn-lims-module-title', text: 'Лабораторная информационная менеджмент система СБЕ ПМиПИР' });
    this.crumbEl = topbar.createDiv({ cls: 'tn-lims-crumb' });
    const spacer = topbar.createDiv({ cls: 'tn-lims-spacer' });
    spacer.empty();
    const createBtn = topbar.createEl('button', { text: '＋ Создать', cls: 'tn-nav-item tn-lims-create' });
    createBtn.addEventListener('click', () => {
      new Notice('Фасад: создание будет подключено на этапе наполнения');
    });

    // главная область: сайдбар + контент
    const main = this.rootEl.createDiv({ cls: 'tn-lims-main' });

    const sidebar = main.createDiv({ cls: 'tn-lims-sidebar' });

    // сворачивание
    const collapseBtn = sidebar.createDiv({ cls: 'tn-lims-collapse' });
    collapseBtn.createSpan({ text: '▧' });
    this.collapseLabel = collapseBtn.createSpan({ cls: 'tn-lims-collapse-lbl', text: 'Свернуть' });
    collapseBtn.addEventListener('click', () => this.toggleCollapse());

    // переключатель лабораторий
    this.labBarEl = sidebar.createDiv({ cls: 'tn-lims-labbar' });
    this.labBarEl.createDiv({ cls: 'tn-lims-nav-label', text: 'ЛАБОРАТОРИЯ' });
    this.labSwitchEl = this.labBarEl.createEl('select', { cls: 'tn-lims-lab-select' });
    this.labSwitchEl.addEventListener('change', () => {
      this.labId = Number(this.labSwitchEl.value) || null;
      void this.renderPage();
    });

    // дерево навигации
    this.navEl = sidebar.createDiv({ cls: 'tn-lims-nav' });
    this.buildNav();

    const content = main.createDiv({ cls: 'tn-lims-content' });
    this.pageTitleEl = content.createEl('h1', { cls: 'tn-lims-page-title' });
    this.pageSubEl = content.createDiv({ cls: 'tn-lims-page-sub' });
    this.contentBoxEl = content.createDiv({ cls: 'tn-lims-view' });
    this.bodyEl = this.contentBoxEl.createDiv();
  }

  private collapseLabel!: HTMLElement;

  private buildNav(): void {
    this.navEl.empty();
    for (const group of NAV_GROUPS) {
      const grpBtn = this.navEl.createEl('button', { cls: 'tn-lims-grp' });
      grpBtn.createSpan({ cls: 'tn-lims-grp-ico', text: group.icon });
      grpBtn.createSpan({ cls: 'tn-lims-grp-lbl', text: group.label });
      grpBtn.createSpan({ cls: 'tn-lims-grp-chev', text: '▶' });
      grpBtn.addEventListener('click', () => {
        grpBtn.classList.toggle('open');
        grpBtn.classList.toggle('active');
        this.syncOpenGroups();
      });

      const submenu = this.navEl.createDiv({ cls: 'tn-lims-submenu' });
      for (const item of group.items) {
        const a = submenu.createEl('a', { cls: 'tn-lims-nav-item', attr: { href: '#' } });
        a.createSpan({ cls: 'tn-lims-nav-lbl', text: item.label });
        a.dataset.key = item.key;
        a.addEventListener('click', (ev) => {
          ev.preventDefault();
          this.key = item.key;
          this.syncNavActive();
          void this.renderPage();
        });
      }
    }

    const firstGroup = this.navEl.querySelector('.tn-lims-grp');
    if (firstGroup) {
      firstGroup.classList.add('open', 'active');
    }
    this.syncNavActive();
  }

  private syncOpenGroups(): void {
    // актуальное состояние расставала классов ведёт buildNav по кликам; здесь оставляем
    // группы открытыми, чтобы подменю было видно (фасад).
  }

  private toggleCollapse(): void {
    this.collapsed = !this.collapsed;
    this.rootEl.classList.toggle('collapsed', this.collapsed);
    if (this.collapseLabel) {
      this.collapseLabel.setText(this.collapsed ? 'Развернуть' : 'Свернуть');
    }
  }

  private syncNavActive(): void {
    this.navEl.querySelectorAll('.tn-lims-nav-item').forEach((el) => {
      const navEl = el as HTMLElement;
      navEl.classList.toggle('active', navEl.dataset.key === this.key);
    });
  }

  // ---- Инициализация ----

  private async initShell(): Promise<void> {
    try {
      const perm = await this.plugin.syncService.getMyPermission();
      this.myRole = perm.role;
      const data = await this.plugin.syncService.listLabs();
      this.labs = data;
    } catch (e: unknown) {
      this.myRole = '';
      this.labs = [];
    }

    this.labBarEl.style.display = this.labs.length <= 1 ? 'none' : '';
    this.labSwitchEl.empty();
    for (const lab of this.labs) {
      this.labSwitchEl.createEl('option', { value: String(lab.id), text: lab.name || lab.code });
    }
    if (this.labSwitchEl.options.length > 0) {
      this.labId = this.labs[0].id;
      this.labSwitchEl.value = String(this.labId);
    }

    this.syncNavActive();
    await this.renderPage();
  }

  // ---- Контент (фасад: заглушки) ----

  private currentLabName(): string {
    const lab = this.labs.find(l => l.id === this.labId);
    return lab ? lab.name || lab.code : '—';
  }

  private async renderPage(): Promise<void> {
    const meta = PAGE_META[this.key];
    this.crumbEl.setText(`${this.currentLabName()} · ${meta.title}`);
    this.pageTitleEl.setText(meta.title);
    this.pageSubEl.setText(meta.sub);

    this.bodyEl.empty();

    if (this.key === 'requests') {
      await this.renderRequests();
      return;
    }

    this.bodyEl.createDiv({ cls: 'tn-lims-stub' }).setText(
      `Раздел «${meta.title}» — фасад готов, наполнение будет подключено следующим этапом.`
    );
  }

  // ---- (наполнение: методы прошлой вьюхи, будут подключены к узлам дерева) ----

  /** Список заявок (возврат из карточки). */
  private async renderRequests(): Promise<void> {
    this.bodyEl.empty();
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const requests = await this.plugin.syncService.listRequests();
      this.bodyEl.empty();
      const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      for (const h of ['Номер', 'Объект', 'Статус', 'Метод']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const r of requests) {
        const row = tbody.createEl('tr', { cls: 'tn-lims-row' });
        row.addEventListener('click', () => this.renderRequestDetail(r));
        row.createEl('td').setText(r.lab_number || '—');
        row.createEl('td').setText(r.title || `#${r.id}`);
        row.createEl('td').setText(STATUS_LABELS[r.status] || r.status);
        row.createEl('td').setText(this.methodName(r.method_id));
      }
      if (requests.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Нет заявок.');
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  private methodName(methodId: number): string {
    const m = this.plugin.methods.find(md => md.id === methodId);
    return m ? `${m.code}${m.name ? ' — ' + m.name : ''}` : `#${methodId}`;
  }

  /** Карточка заявки: серии результатов, ввод, расчёт, статус, графики, протокол. */
  private async renderRequestDetail(req: LimsRequest): Promise<void> {
    this.bodyEl.empty();

    const back = this.bodyEl.createEl('button', { text: '← Назад', cls: 'tn-btn tn-btn-ghost' });
    back.addEventListener('click', () => void this.renderRequests());

    this.bodyEl.createEl('h3', { text: `№ ${req.lab_number || `${req.number_seq}/${req.number_year}`} — ${req.title || 'без названия'}` });

    const meta = this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-mb8' });
    meta.setText(`Статус: ${STATUS_LABELS[req.status] || req.status} · Заказчик: ${req.owner_email || '—'}`);

    // статус
    if (this.canEditStatus) {
      const statusSelect = this.bodyEl.createEl('select', { cls: 'tn-lims-select tn-lims-mb8' });
      for (const [v, l] of Object.entries(STATUS_LABELS)) statusSelect.createEl('option', { value: v, text: l });
      statusSelect.value = req.status;
      statusSelect.addEventListener('change', async () => {
        try {
          await this.plugin.syncService.setStatus(req.id, statusSelect.value);
          req.status = statusSelect.value;
          new Notice('Статус обновлён');
          void this.renderRequestDetail(req);
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
    }

    // результаты по методу заявки (1 заявка = 1 метод)
    const methodDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    methodDiv.createEl('h4', { text: `Метод: ${this.methodName(req.method_id)}` });

    // форма ввода новой серии
    if (this.canEdit) {
      const form = methodDiv.createDiv({ cls: 'tn-lims-series-form' });
      const valuesRow = form.createDiv({ cls: 'tn-lims-flex' });
      const input = valuesRow.createEl('input', {
        attr: { type: 'text', placeholder: 'параметр=значение; параметр2=значение2' },
        cls: 'tn-lims-input',
      });
      const addBtn = valuesRow.createEl('button', { text: '➕ Добавить серию', cls: 'tn-btn tn-btn-primary' });
      addBtn.addEventListener('click', async () => {
        const values = this.parseValues(input.value);
        if (Object.keys(values).length === 0) { new Notice('Введите параметры (параметр=значение)'); return; }
        try {
          await this.plugin.syncService.saveResult(req.id, {
            method_id: req.method_id,
            inventor_id: 0,
            series_num: 0,
            values,
          });
          new Notice('Серия добавлена, расчёт выполнен');
          void this.renderRequestDetail(req);
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
      const calcBtn = form.createEl('button', { text: '🔄 Рассчитать', cls: 'tn-btn tn-btn-ghost' });
      calcBtn.addEventListener('click', async () => {
        try {
          const results = await this.plugin.syncService.listResults(req.id);
          const series = results.filter(r => !r.is_statistical_row);
          if (series.length === 0) { new Notice('Нет серий'); return; }
          await this.plugin.syncService.calculateSeries(req.id, series[0].series_num);
          new Notice('Расчёт выполнен');
          void this.renderRequestDetail(req);
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
    }

    // таблица серий
    const results = await this.plugin.syncService.listResults(req.id);
    const seriesRows = results.filter(r => r.method_id === req.method_id && !r.is_statistical_row);
    if (seriesRows.length > 0) {
      this.renderResultsTable(methodDiv, seriesRows);
    } else {
      methodDiv.createDiv({ cls: 'tn-lims-meta' }).setText('Результатов пока нет');
    }

    // графики
    const chartDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    chartDiv.createEl('h4', { text: '📈 Графики' });
    const cfg = this.methodConfigOf(req.method_id);
    for (const c of cfg.chart_configs) {
      const id = String(c.id || '');
      const title = String(c.title || id);
      chartDiv.createEl('img', {
        attr: { src: this.plugin.syncService.chartUrl(req.id, id), alt: title },
        cls: 'tn-lims-chart',
      });
    }

    // протокол
    const protoDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    protoDiv.createEl('h4', { text: '📄 Протокол' });
    const protoBtn = protoDiv.createEl('button', { text: 'Сгенерировать протокол', cls: 'tn-btn tn-btn-primary' });
    protoBtn.addEventListener('click', async () => {
      try {
        const proto = await this.plugin.syncService.getProtocol(req.id);
        const modal = this.showHtmlModal(req, proto.html);
        const docxBtn = modal.createEl('button', { text: 'Скачать DOCX', cls: 'tn-btn tn-btn-ghost' });
        docxBtn.addEventListener('click', () => this.downloadDocx(proto.docx_base64, `protocol_${req.number_seq}_${req.number_year}.docx`));
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  private renderResultsTable(container: HTMLElement, rows: MeasurementResult[]): void {
    const table = container.createEl('table', { cls: 'tn-table' });
    const keys = new Set<string>();
    for (const r of rows) for (const k of Object.keys(r.values)) keys.add(k);
    const keyList = Array.from(keys);
    const thead = table.createEl('thead').createEl('tr');
    thead.createEl('th').setText('Серия');
    for (const k of keyList) thead.createEl('th').setText(k);
    const tbody = table.createEl('tbody');
    for (const r of rows) {
      const tr = tbody.createEl('tr');
      tr.createEl('td').setText(String(r.series_num));
      for (const k of keyList) tr.createEl('td').setText(this.fmt(r.values[k]));
    }
  }

  private methodConfigOf(methodId: number): MethodConfig {
    const m = this.plugin.methods.find(md => md.id === methodId);
    return {
      formulas: Array.isArray(m?.formulas) ? m.formulas : [],
      classification: Array.isArray(m?.classification) ? m.classification : [],
      chart_configs: Array.isArray(m?.chart_configs) ? m.chart_configs : [],
      input_parameters: Array.isArray(m?.input_parameters) ? m.input_parameters : [],
    };
  }

  private showHtmlModal(req: LimsRequest, html: string): HTMLElement {
    const modal = this.bodyEl.createDiv({ cls: 'tn-lims-modal' });
    const head = modal.createDiv({ cls: 'tn-lims-modal-head' });
    head.createEl('b', { text: `Протокол № ${req.lab_number || `${req.number_seq}/${req.number_year}`}` });
    const close = head.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    close.addEventListener('click', () => modal.remove());
    const iframe = modal.createEl('iframe', { attr: { sandbox: '' }, cls: 'tn-lims-iframe' });
    iframe.setAttr('srcdoc', html);
    return modal;
  }

  private downloadDocx(base64Data: string, fileName: string): void {
    try {
      const bin = atob(base64Data);
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      const blob = new Blob([bytes], { type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = fileName;
      a.click();
      URL.revokeObjectURL(a.href);
    } catch (e: unknown) {
      new Notice(`Ошибка скачивания: ${errorMessage(e)}`);
    }
  }

  private parseValues(input: string): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const part of input.split(';')) {
      const [k, ...rest] = part.split('=');
      const key = (k || '').trim();
      const val = rest.join('=').trim();
      if (!key) continue;
      if (val === '') continue;
      const num = Number(val);
      out[key] = Number.isNaN(num) ? val : num;
    }
    return out;
  }

  private fmt(v: unknown): string {
    if (v === undefined || v === null) return '—';
    if (typeof v === 'object') return JSON.stringify(v);
    return String(v);
  }

  private get canEdit(): boolean {
    return this.myRole !== '';
  }

  private get canEditStatus(): boolean {
    return this.myRole !== '';
  }
}