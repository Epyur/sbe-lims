import { ItemView, Notice, WorkspaceLeaf } from 'obsidian';
import type SbeLimsPlugin from '../main';
import type { LimsRequest, MeasurementResult, MethodConfig } from '../types/lims';
import { errorMessage } from '../../../sbe-core/src/utils/errors';

export const SBE_LIMS_VIEW_TYPE = 'sbe-lims-view';

const STATUS_LABELS: Record<string, string> = {
  new: '🟢 Новая',
  received: '📦 Образцы получены',
  processing: '🟡 В работе',
  completed: '✅ Завершена',
};

type LimsTab = 'requests' | 'refs' | 'dashboard';

export class LimsView extends ItemView {
  plugin: SbeLimsPlugin;
  private containerElContent!: HTMLElement;
  private tab: LimsTab = 'requests';
  private myRole = '';

  constructor(leaf: WorkspaceLeaf, plugin: SbeLimsPlugin) {
    super(leaf);
    this.plugin = plugin;
  }

  getViewType(): string {
    return SBE_LIMS_VIEW_TYPE;
  }

  getDisplayText(): string {
    return 'ЛИМС';
  }

  getIcon(): string {
    return 'flask-conical';
  }

  async onOpen(): Promise<void> {
    const container = this.contentEl;
    container.addClass('tn-lims-container');
    this.containerElContent = container.createDiv();
    this.render();
  }

  refresh(): void {
    this.render();
  }

  private render(): void {
    const container = this.containerElContent;
    container.empty();

    // вкладки
    const tabs = container.createDiv({ cls: 'tn-lims-tabs' });
    const tabBtn = (id: LimsTab, label: string): void => {
      const b = tabs.createEl('button', { cls: 'tn-nav-item', text: label });
      if (id === this.tab) b.addClass('active');
      b.addEventListener('click', () => {
        this.tab = id;
        this.render();
      });
    };
    tabBtn('requests', '📋 Заявки');
    tabBtn('refs', '📚 Справочники');
    tabBtn('dashboard', '📊 Дашборд');

    this.bodyEl = container.createDiv({ cls: 'tn-lims-body' });
    switch (this.tab) {
      case 'requests':
        void this.renderRequests();
        break;
      case 'refs':
        void this.renderRefs();
        break;
      case 'dashboard':
        void this.renderDashboard();
        break;
    }
  }

  private bodyEl!: HTMLElement;

  // ---- Заявки ----

  private async renderRequests(): Promise<void> {
    this.bodyEl.empty();
    this.bodyEl.createDiv({ cls: 'tn-req-meta', text: 'Загрузка…' });
    try {
      const requests = await this.plugin.syncService.listRequests();
      this.bodyEl.empty();
      const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      for (const h of ['Номер', 'Объект', 'Статус', 'Методы']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const r of requests) {
        const row = tbody.createEl('tr', { cls: 'tn-lims-row' });
        row.addEventListener('click', () => this.renderRequestDetail(r));
        const first = r.methods && r.methods.length > 0 ? r.methods[0].customer_number : '—';
        row.createEl('td').setText(first);
        row.createEl('td').setText(r.title || `#${r.id}`);
        row.createEl('td').setText(STATUS_LABELS[r.status] || r.status);
        row.createEl('td').setText(r.methods.map(m => this.methodName(m.method_id)).join(', '));
      }
      if (requests.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-req-meta tn-req-p24' }).setText('Нет заявок.');
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

    this.bodyEl.createEl('h3', { text: `№ ${req.number_seq}/${req.number_year} — ${req.title || 'без названия'}` });

    const meta = this.bodyEl.createDiv({ cls: 'tn-req-meta tn-req-mb8' });
    meta.setText(`Статус: ${STATUS_LABELS[req.status] || req.status} · Заказчик: ${req.owner_email || '—'}`);

    // статус
    if (this.canEditStatus) {
      const statusSelect = this.bodyEl.createEl('select', { cls: 'tn-req-select tn-req-mb8' });
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

    // результаты по методам
    for (const rm of req.methods) {
      const method = this.plugin.methods.find(md => md.id === rm.method_id);
      const methodDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
      methodDiv.createEl('h4', { text: `Метод: ${this.methodName(rm.method_id)}` });

      // форма ввода новой серии
      if (this.canEdit) {
        const form = methodDiv.createDiv({ cls: 'tn-lims-series-form' });
        const valuesRow = form.createDiv({ cls: 'tn-req-flex' });
        const input = valuesRow.createEl('input', {
          attr: { type: 'text', placeholder: 'параметр=значение; параметр2=значение2' },
          cls: 'tn-req-input',
        });
        const addBtn = valuesRow.createEl('button', { text: '➕ Добавить серию', cls: 'tn-btn tn-btn-primary' });
        addBtn.addEventListener('click', async () => {
          const values = this.parseValues(input.value);
          if (Object.keys(values).length === 0) { new Notice('Введите параметры (параметр=значение)'); return; }
          try {
            await this.plugin.syncService.saveResult(req.id, {
              method_id: rm.method_id,
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
      const seriesRows = results.filter(r => r.method_id === rm.method_id && !r.is_statistical_row);
      if (seriesRows.length > 0) {
        this.renderResultsTable(methodDiv, seriesRows);
      } else {
        methodDiv.createDiv({ cls: 'tn-req-meta' }).setText('Результатов пока нет');
      }
    }

    // графики
    const chartDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    chartDiv.createEl('h4', { text: '📈 Графики' });
    for (const rm of req.methods) {
      const cfg = this.methodConfigOf(rm.method_id);
      for (const c of cfg.chart_configs) {
        const id = String(c.id || '');
        const title = String(c.title || id);
        chartDiv.createEl('img', {
          attr: { src: this.plugin.syncService.chartUrl(req.id, id), alt: title },
          cls: 'tn-lims-chart',
        });
      }
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
    head.createEl('b', { text: `Протокол № ${req.number_seq}/${req.number_year}` });
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

  // ---- Справочники ----

  private async renderRefs(): Promise<void> {
    this.bodyEl.empty();
    this.bodyEl.createDiv({ cls: 'tn-req-meta', text: 'Загрузка…' });
    try {
      const [inventors, equipment, methods] = await Promise.all([
        this.plugin.syncService.listInventors(),
        this.plugin.syncService.listEquipment(),
        Promise.resolve(this.plugin.methods),
      ]);
      this.bodyEl.empty();

      // Испытатели
      const invSection = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
      invSection.createEl('h4', { text: 'Испытатели' });
      const invAdd = invSection.createDiv({ cls: 'tn-req-flex' });
      const invName = invAdd.createEl('input', { attr: { type: 'text', placeholder: 'ФИО' }, cls: 'tn-req-input' });
      const invEmail = invAdd.createEl('input', { attr: { type: 'text', placeholder: 'email' }, cls: 'tn-req-input' });
      const invBtn = invAdd.createEl('button', { text: '➕', cls: 'tn-btn tn-btn-primary' });
      invBtn.addEventListener('click', async () => {
        if (!invName.value.trim()) { new Notice('Введите ФИО'); return; }
        try {
          await this.plugin.syncService.createInventor({ name: invName.value.trim(), email: invEmail.value.trim() });
          new Notice('Испытатель добавлен');
          void this.renderRefs();
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
      const invTable = invSection.createEl('table', { cls: 'tn-table' });
      const h = invTable.createEl('thead').createEl('tr');
      h.createEl('th').setText('ФИО');
      h.createEl('th').setText('Email');
      for (const i of inventors) {
        const tr = invTable.createEl('tbody').createEl('tr');
        tr.createEl('td').setText(i.name);
        tr.createEl('td').setText(i.email || '—');
      }

      // Оборудование
      const eqSection = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
      eqSection.createEl('h4', { text: 'Оборудование' });
      const eqAdd = eqSection.createDiv({ cls: 'tn-req-flex' });
      const eqCode = eqAdd.createEl('input', { attr: { type: 'text', placeholder: 'Код' }, cls: 'tn-req-input' });
      const eqName = eqAdd.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-req-input' });
      const eqBtn = eqAdd.createEl('button', { text: '➕', cls: 'tn-btn tn-btn-primary' });
      eqBtn.addEventListener('click', async () => {
        if (!eqCode.value.trim()) { new Notice('Введите код'); return; }
        try {
          await this.plugin.syncService.createEquipment({ code: eqCode.value.trim(), name: eqName.value.trim() });
          new Notice('Оборудование добавлено');
          void this.renderRefs();
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
      const eqTable = eqSection.createEl('table', { cls: 'tn-table' });
      const eh = eqTable.createEl('thead').createEl('tr');
      eh.createEl('th').setText('Код');
      eh.createEl('th').setText('Название');
      for (const e of equipment) {
        const tr = eqTable.createEl('tbody').createEl('tr');
        tr.createEl('td').setText(e.code);
        tr.createEl('td').setText(e.name || '—');
      }

      // Методы (конфиги)
      const mSection = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
      mSection.createEl('h4', { text: 'Методы (формулы/классификация/графики)' });
      for (const m of methods) {
        const mRow = mSection.createDiv({ cls: 'tn-lims-method' });
        mRow.createEl('b', { text: `${m.code}${m.name ? ' — ' + m.name : ''}` });
        const editBtn = mRow.createEl('button', { text: '⚙ Редактировать', cls: 'tn-btn tn-btn-ghost' });
        editBtn.addEventListener('click', () => this.renderMethodConfig(m.id, mRow));
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  /** Форма конфигурации метода (formulas/classification/chart_configs — JSON). */
  private renderMethodConfig(methodId: number, container: HTMLElement): void {
    const cfg = this.methodConfigOf(methodId);
    const wrap = container.createDiv({ cls: 'tn-lims-method' });

    const formulasLabel = wrap.createEl('label', { text: 'Формулы (JSON-массив)', cls: 'tn-req-label' });
    const formulasInput = wrap.createEl('textarea', { cls: 'tn-req-textarea' });
    formulasInput.value = JSON.stringify(cfg.formulas, null, 2);

    const classLabel = wrap.createEl('label', { text: 'Классификация (JSON-массив)', cls: 'tn-req-label' });
    const classInput = wrap.createEl('textarea', { cls: 'tn-req-textarea' });
    classInput.value = JSON.stringify(cfg.classification, null, 2);

    const chartLabel = wrap.createEl('label', { text: 'Графики (JSON-массив)', cls: 'tn-req-label' });
    const chartInput = wrap.createEl('textarea', { cls: 'tn-req-textarea' });
    chartInput.value = JSON.stringify(cfg.chart_configs, null, 2);

    const saveBtn = wrap.createEl('button', { text: '💾 Сохранить конфиг', cls: 'tn-btn tn-btn-primary' });
    saveBtn.addEventListener('click', async () => {
      try {
        await this.plugin.syncService.updateMethodConfig(methodId, {
          formulas: JSON.parse(formulasInput.value || '[]'),
          classification: JSON.parse(classInput.value || '[]'),
          chart_configs: JSON.parse(chartInput.value || '[]'),
        });
        new Notice('Конфиг метода сохранён');
        await this.plugin.refreshMethods();
        this.render();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  // ---- Дашборд ----

  private async renderDashboard(): Promise<void> {
    this.bodyEl.empty();
    this.bodyEl.createDiv({ cls: 'tn-req-meta', text: 'Загрузка…' });
    try {
      const data = await this.plugin.syncService.getDashboard('month');
      this.bodyEl.empty();
      this.bodyEl.createEl('h4', { text: `Дашборд (${data.period})` });
      const statDiv = this.bodyEl.createDiv({ cls: 'tn-req-flex' });
      for (const [k, v] of Object.entries(data.by_status)) {
        statDiv.createDiv({ cls: 'tn-lims-stat' }).setText(`${STATUS_LABELS[k] || k}: ${v}`);
      }
      statDiv.createDiv({ cls: 'tn-lims-stat' }).setText(`Всего: ${data.total}`);
      statDiv.createDiv({ cls: 'tn-lims-stat' }).setText(`Завершено за период: ${data.completed_in_period}`);

      if (data.by_method.length > 0) {
        this.bodyEl.createEl('h4', { text: 'По методам' });
        const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
        const h = table.createEl('thead').createEl('tr');
        h.createEl('th').setText('Метод');
        h.createEl('th').setText('Заявок');
        for (const mc of data.by_method) {
          const tr = table.createEl('tbody').createEl('tr');
          tr.createEl('td').setText(this.methodName(mc.method_id));
          tr.createEl('td').setText(String(mc.count));
        }
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }
}
