import { ItemView, Notice, WorkspaceLeaf } from 'obsidian';
import type SbeLimsPlugin from '../main';
import type { Equipment, Inventor, Lab, LabMember, LimsRequest, MeasurementResult, MethodConfig } from '../types/lims';
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
  | 'lab-members'
  | 'settings';

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
  settings: { title: 'Настройки', sub: 'Лаборатории и их администраторы' },
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
  private settingsBtnEl!: HTMLElement;

  private key: NavKey = 'requests';
  private labId: number | null = null;
  private collapsed = false;
  private labs: Lab[] = [];
  private myRole = '';
  private currentRequestsFilter?: (r: LimsRequest) => boolean;

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

    // «Настройки» — внизу сайдбара (flex:1 у navEl прижимает этот блок к низу),
    // отдельно от дерева: лаборатории/администраторы — не раздел работы с заявками.
    // Видна admin+ (лабы — только superadmin, назначение админов — admin+, обе
    // ветки гейтятся внутри renderSettings/renderSettingsLabRow).
    this.settingsBtnEl = sidebar.createDiv({ cls: 'tn-lims-collapse' });
    this.settingsBtnEl.createSpan({ text: '⚙' });
    this.settingsBtnEl.createSpan({ cls: 'tn-lims-collapse-lbl', text: 'Настройки' });
    this.settingsBtnEl.addEventListener('click', () => {
      this.key = 'settings';
      this.syncNavActive();
      void this.renderPage();
    });

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

    this.refreshLabSwitcher();
    if (this.labSwitchEl.options.length > 0) {
      this.labId = this.labs[0].id;
      this.labSwitchEl.value = String(this.labId);
    }
    this.settingsBtnEl.style.display = (this.myRole === 'admin' || this.myRole === 'superadmin') ? '' : 'none';

    this.syncNavActive();
    await this.renderPage();
  }

  /** Пересобирает select лабораторий; скрыт при 0/1 (нечего переключать). */
  private refreshLabSwitcher(): void {
    this.labBarEl.style.display = this.labs.length <= 1 ? 'none' : '';
    this.labSwitchEl.empty();
    for (const lab of this.labs) {
      this.labSwitchEl.createEl('option', { value: String(lab.id), text: lab.name || lab.code });
    }
  }

  /** «Настройки»: лаборатории (создание/правка — только superadmin) + назначение
   * администраторов лабораторий (lab_members role=lab_admin — admin+, только для
   * внутренних лаб, у внешних lab_members не бывает по определению). */
  private async renderSettings(): Promise<void> {
    if (this.myRole !== 'admin' && this.myRole !== 'superadmin') {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Раздел доступен администраторам.');
      return;
    }
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const members = await this.plugin.syncService.listLabMembers();
      this.bodyEl.empty();
      this.bodyEl.createEl('h3', { text: 'Лаборатории' });
      for (const lab of this.labs) {
        this.renderSettingsLabRow(lab, members.filter(m => m.lab_id === lab.id));
      }
      if (this.myRole === 'superadmin') {
        const addBtn = this.bodyEl.createEl('button', { text: '➕ Новая лаборатория', cls: 'tn-btn tn-btn-ghost' });
        addBtn.addEventListener('click', () => {
          const existing = this.bodyEl.querySelector('.tn-lims-lab-form');
          if (existing) { existing.remove(); return; }
          this.renderLabForm(null, this.bodyEl);
        });
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  /** Карточка одной лаборатории в «Настройках»: правка (superadmin) + список/
   * назначение администраторов (admin+, только для внутренних лаб). */
  private renderSettingsLabRow(lab: Lab, members: LabMember[]): void {
    const card = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    const head = card.createDiv({ cls: 'tn-lims-flex' });
    const parent = lab.parent_lab_id ? this.labs.find(l => l.id === lab.parent_lab_id) : undefined;
    let title = lab.name || lab.code;
    if (lab.type === 'external') {
      title += parent ? ` (внешняя → ${parent.name || parent.code})` : ' (внешняя, без родителя!)';
    }
    head.createEl('h4', { text: title });
    if (this.myRole === 'superadmin') {
      const editBtn = head.createEl('button', { text: '✎ Изменить', cls: 'tn-btn tn-btn-ghost' });
      editBtn.addEventListener('click', () => {
        const existing = card.querySelector('.tn-lims-lab-form');
        if (existing) { existing.remove(); return; }
        this.renderLabForm(lab, card);
      });
    }

    if (lab.type !== 'internal') return; // у внешней лабы lab_members не бывает
    const admins = members.filter(m => m.role === 'lab_admin');
    card.createDiv({ cls: 'tn-lims-meta' }).setText(
      admins.length > 0 ? `Администраторы: ${admins.map(a => a.email).join(', ')}` : 'Администраторов пока нет',
    );
    for (const a of admins) {
      const row = card.createDiv({ cls: 'tn-lims-flex' });
      row.createSpan({ text: a.email });
      const rmBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
      rmBtn.addEventListener('click', async () => {
        if (!window.confirm(`Убрать «${a.email}» из администраторов «${lab.name || lab.code}»?`)) return;
        try {
          await this.plugin.syncService.removeLabMember(lab.id, a.email);
          new Notice('Администратор убран');
          await this.renderSettings();
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
    }
    const addRow = card.createDiv({ cls: 'tn-lims-flex' });
    const emailInp = addRow.createEl('input', {
      attr: { type: 'text', placeholder: 'E-mail нового администратора' },
      cls: 'tn-lims-input',
    });
    const addBtn = addRow.createEl('button', { text: '➕ Назначить администратором', cls: 'tn-btn tn-btn-primary' });
    addBtn.addEventListener('click', async () => {
      if (!emailInp.value.trim()) { new Notice('Укажите e-mail'); return; }
      try {
        await this.plugin.syncService.setLabMember(lab.id, emailInp.value.trim(), 'lab_admin');
        new Notice('Администратор назначен');
        await this.renderSettings();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** Форма создания/правки лаборатории (lab=null — создание), рендерится в
   * переданный container (карточка лабы или общий bodyEl для «новой»). Внешняя не
   * существует самостоятельно — обязана указать родительскую внутреннюю
   * (parentSelect появляется только при выборе «внешняя»). */
  private renderLabForm(lab: Lab | null, container: HTMLElement): void {
    const form = container.createDiv({ cls: 'tn-lims-series-form tn-lims-lab-form' });
    const code = form.createEl('input', { attr: { type: 'text', placeholder: 'Код (уникальный)' }, cls: 'tn-lims-input' });
    code.value = lab?.code || '';
    const name = form.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-lims-input' });
    name.value = lab?.name || '';
    const description = form.createEl('input', { attr: { type: 'text', placeholder: 'Описание' }, cls: 'tn-lims-input' });
    description.value = lab?.description || '';
    const typeSelect = form.createEl('select', { cls: 'tn-lims-select' });
    typeSelect.createEl('option', { value: 'internal', text: 'Внутренняя' });
    typeSelect.createEl('option', { value: 'external', text: 'Внешняя (привязывается к внутренней)' });
    typeSelect.value = lab?.type === 'external' ? 'external' : 'internal';
    const internalLabs = this.labs.filter(l => l.type === 'internal' && l.id !== lab?.id);
    const parentSelect = form.createEl('select', { cls: 'tn-lims-select' });
    parentSelect.createEl('option', { value: '', text: '— Родительская внутренняя лаба —' });
    for (const l of internalLabs) parentSelect.createEl('option', { value: String(l.id), text: l.name || l.code });
    if (lab?.parent_lab_id) parentSelect.value = String(lab.parent_lab_id);
    parentSelect.style.display = typeSelect.value === 'external' ? '' : 'none';
    typeSelect.addEventListener('change', () => {
      parentSelect.style.display = typeSelect.value === 'external' ? '' : 'none';
    });
    const saveBtn = form.createEl('button', { text: lab ? '💾 Сохранить' : '➕ Создать', cls: 'tn-btn tn-btn-primary' });
    const cancelBtn = form.createEl('button', { text: '✖ Отмена', cls: 'tn-btn tn-btn-ghost' });
    cancelBtn.addEventListener('click', () => form.remove());
    saveBtn.addEventListener('click', async () => {
      if (!code.value.trim() || !name.value.trim()) { new Notice('Укажите код и название'); return; }
      if (typeSelect.value === 'external' && !parentSelect.value) {
        new Notice('Внешняя лаборатория обязана быть привязана к внутренней');
        return;
      }
      try {
        if (lab) {
          await this.plugin.syncService.updateLab(lab.id, {
            code: code.value.trim(),
            name: name.value.trim(),
            description: description.value.trim(),
            type: typeSelect.value,
            parent_lab_id: typeSelect.value === 'external' ? Number(parentSelect.value) : 0,
          });
          new Notice('Лаборатория обновлена');
        } else {
          const id = await this.plugin.syncService.createLab({
            code: code.value.trim(),
            name: name.value.trim(),
            description: description.value.trim() || undefined,
            type: typeSelect.value,
            parent_lab_id: typeSelect.value === 'external' ? Number(parentSelect.value) : undefined,
          });
          this.labId = id;
          new Notice('Лаборатория создана');
        }
        this.labs = await this.plugin.syncService.listLabs();
        this.refreshLabSwitcher();
        if (this.labId !== null && this.labSwitchEl.querySelector(`option[value="${this.labId}"]`)) {
          this.labSwitchEl.value = String(this.labId);
        }
        await this.renderSettings();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
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

    switch (this.key) {
      case 'requests':
        await this.renderRequests();
        return;
      case 'queue':
        await this.renderRequests(r => r.status === 'new' || r.status === 'received');
        return;
      case 'results':
        await this.renderRequests(r => r.status === 'completed');
        return;
      case 'methods':
        await this.renderMethods();
        return;
      case 'objects':
        await this.renderObjects();
        return;
      case 'inventors':
        await this.renderInventors();
        return;
      case 'equipment':
        await this.renderEquipment();
        return;
      case 'lab-members':
        await this.renderLabMembers();
        return;
      case 'settings':
        await this.renderSettings();
        return;
    }
  }

  // ---- (наполнение: методы прошлой вьюхи, будут подключены к узлам дерева) ----

  /** Список заявок (возврат из карточки). Опциональный фильтр — для «Очереди»/«Результатов». */
  private async renderRequests(filter?: (r: LimsRequest) => boolean): Promise<void> {
    this.currentRequestsFilter = filter;
    this.bodyEl.empty();
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      let requests = await this.plugin.syncService.listRequests();
      if (filter) requests = requests.filter(filter);
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
    back.addEventListener('click', () => void this.renderRequests(this.currentRequestsFilter));

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

    // результаты по методу заявки (1 заявка = 1 метод, выполняется конкретной лабой
    // из lab_ids метода — зафиксирована на заявке как lab_id, метод мог принадлежать
    // нескольким)
    const methodDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    const labLabel = this.labs.find(l => l.id === req.lab_id);
    methodDiv.createEl('h4', {
      text: `Метод: ${this.methodName(req.method_id)}${labLabel ? ` · Лаборатория: ${labLabel.name || labLabel.code}` : ''}`,
    });

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

  // ---- Справочники ----

  /** Методы: список из кэша (pull); создание + JSON-редактор конфигов + удаление — admin. */
  private async renderMethods(): Promise<void> {
    this.bodyEl.empty();
    if (this.canAdmin) {
      this.renderMethodCreateForm();
    }
    // Все методы, не только текущей выбранной лабы — метод может принадлежать
    // нескольким (включая внешние), фильтр по this.labId скрывал бы только что
    // созданный метод, если он не привязан к лабе, открытой в переключателе сейчас.
    const methods = this.plugin.methods;
    if (methods.length === 0) {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Методов пока нет.');
      return;
    }
    for (const m of methods) {
      const card = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
      const head = card.createDiv({ cls: 'tn-lims-flex' });
      head.createEl('h4', { text: `${m.code} — ${m.name || 'без названия'}` });
      const labNames = m.lab_ids
        .map(id => this.labs.find(l => l.id === id))
        .filter((l): l is Lab => !!l)
        .map(l => l.name || l.code);
      card.createDiv({ cls: 'tn-lims-meta' }).setText(`Лаборатории: ${labNames.join(', ') || '—'}`);
      if (m.description) {
        card.createDiv({ cls: 'tn-lims-meta' }).setText(m.description);
      }
      if (m.determinable_indicators.length > 0) {
        card.createDiv({ cls: 'tn-lims-meta' }).setText(`Показатели: ${m.determinable_indicators.join(', ')}`);
      }
      if (this.canAdmin) {
        const editBtn = head.createEl('button', { text: '✎ Конфиг', cls: 'tn-btn tn-btn-ghost' });
        editBtn.addEventListener('click', () => this.renderMethodConfigForm(card, m.id));
        const delBtn = head.createEl('button', { text: '✖ Удалить', cls: 'tn-btn tn-btn-ghost' });
        delBtn.addEventListener('click', async () => {
          if (!window.confirm(`Удалить метод «${m.code}»? Если по нему уже есть заявки, сервер откажет.`)) return;
          try {
            await this.plugin.syncService.deleteMethod(m.id);
            await this.plugin.refreshMethods();
            new Notice('Метод удалён');
            await this.renderMethods();
          } catch (e: unknown) {
            new Notice(`Ошибка: ${errorMessage(e)}`);
          }
        });
      }
    }
  }

  /** Чекбоксы выбора лабораторий метода (принадлежит нескольким — 2026-08-19).
   * Возвращает функцию, читающую текущий выбор в момент сохранения. */
  private renderLabCheckboxes(container: HTMLElement, selected: number[]): () => number[] {
    const boxes: Array<{ id: number; el: HTMLInputElement }> = [];
    for (const lab of this.labs) {
      const row = container.createDiv({ cls: 'tn-lims-flex' });
      const cb = row.createEl('input', { attr: { type: 'checkbox' } });
      cb.checked = selected.includes(lab.id);
      row.createSpan({ text: lab.name || lab.code });
      boxes.push({ id: lab.id, el: cb });
    }
    return () => boxes.filter(b => b.el.checked).map(b => b.id);
  }

  /** Форма создания метода (admin). Лаборатории — чекбоксы (метод может принадлежать
   * нескольким), из уже загруженного списка лабораторий. */
  private renderMethodCreateForm(): void {
    const form = this.bodyEl.createDiv({ cls: 'tn-lims-series-form' });
    const row = form.createDiv({ cls: 'tn-lims-flex' });
    const code = row.createEl('input', { attr: { type: 'text', placeholder: 'Код метода' }, cls: 'tn-lims-input' });
    const name = row.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-lims-input' });
    const description = row.createEl('input', { attr: { type: 'text', placeholder: 'Описание' }, cls: 'tn-lims-input' });
    const indicators = row.createEl('input', {
      attr: { type: 'text', placeholder: 'Показатели (через запятую)' },
      cls: 'tn-lims-input',
    });
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Лаборатории (метод может принадлежать нескольким):');
    const getLabIDs = this.renderLabCheckboxes(form, []);
    const addBtn = form.createEl('button', { text: '➕ Добавить метод', cls: 'tn-btn tn-btn-primary' });
    addBtn.addEventListener('click', async () => {
      const labIDs = getLabIDs();
      if (!code.value.trim() || labIDs.length === 0) { new Notice('Укажите код метода и хотя бы одну лабораторию'); return; }
      try {
        await this.plugin.syncService.createMethod({
          code: code.value.trim(),
          name: name.value.trim(),
          lab_ids: labIDs,
          description: description.value.trim() || undefined,
          determinable_indicators: indicators.value.split(',').map(s => s.trim()).filter(Boolean),
        });
        await this.plugin.refreshMethods();
        new Notice('Метод создан');
        await this.renderMethods();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** JSON-редактор formulas/classification/chart_configs/input_parameters метода
   * (admin) + чекбоксы лабораторий (lab_ids, метод может принадлежать нескольким). */
  private renderMethodConfigForm(container: HTMLElement, methodId: number): void {
    const existing = container.querySelector('.tn-lims-series-form');
    if (existing) { existing.remove(); return; }
    const cfg = this.methodConfigOf(methodId);
    const method = this.plugin.methods.find(m => m.id === methodId);
    const form = container.createDiv({ cls: 'tn-lims-series-form' });
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Описание:');
    const description = form.createEl('input', { attr: { type: 'text', placeholder: 'Описание' }, cls: 'tn-lims-input' });
    description.value = method?.description || '';
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Лаборатории (метод может принадлежать нескольким):');
    const getLabIDs = this.renderLabCheckboxes(form, method?.lab_ids || []);
    const fields: Array<{ key: keyof MethodConfig; label: string }> = [
      { key: 'formulas', label: 'formulas (JSON)' },
      { key: 'classification', label: 'classification (JSON)' },
      { key: 'chart_configs', label: 'chart_configs (JSON)' },
      { key: 'input_parameters', label: 'input_parameters (JSON)' },
    ];
    const areas: Partial<Record<keyof MethodConfig, HTMLTextAreaElement>> = {};
    for (const f of fields) {
      form.createDiv({ cls: 'tn-lims-meta' }).setText(f.label);
      const ta = form.createEl('textarea', { cls: 'tn-lims-input' });
      ta.rows = 4;
      ta.value = JSON.stringify(cfg[f.key], null, 2);
      areas[f.key] = ta;
    }
    const saveBtn = form.createEl('button', { text: '💾 Сохранить', cls: 'tn-btn tn-btn-primary' });
    saveBtn.addEventListener('click', async () => {
      const labIDs = getLabIDs();
      if (labIDs.length === 0) { new Notice('Укажите хотя бы одну лабораторию'); return; }
      const patch: Partial<MethodConfig> & { lab_ids?: number[]; description?: string } = {
        lab_ids: labIDs,
        description: description.value.trim(),
      };
      try {
        for (const f of fields) {
          patch[f.key] = JSON.parse(areas[f.key]!.value) as never;
        }
      } catch (e: unknown) {
        new Notice(`Неверный JSON: ${errorMessage(e)}`);
        return;
      }
      try {
        await this.plugin.syncService.updateMethodConfig(methodId, patch);
        await this.plugin.refreshMethods();
        new Notice('Конфиг метода обновлён');
        await this.renderMethods();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** Объекты исследования — только чтение (создание в sbe-requests). */
  private async renderObjects(): Promise<void> {
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const objects = await this.plugin.syncService.listObjects();
      this.bodyEl.empty();
      if (objects.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Объектов пока нет.');
        return;
      }
      const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      for (const h of ['Название', 'Описание', 'Характеристики']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const o of objects) {
        const tr = tbody.createEl('tr');
        tr.createEl('td').setText(o.name || `#${o.id}`);
        tr.createEl('td').setText(o.description || '—');
        tr.createEl('td').setText(Object.keys(o.characteristics || {}).length > 0 ? JSON.stringify(o.characteristics) : '—');
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  /** Испытатели — список (viewer) + создание/правка/удаление (editor+). */
  private async renderInventors(): Promise<void> {
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const inventors = await this.plugin.syncService.listInventors();
      this.bodyEl.empty();
      if (this.canEditRefs) {
        this.renderInventorForm();
      }
      if (inventors.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Испытателей пока нет.');
        return;
      }
      const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      for (const h of ['ФИО', 'E-mail', 'Телефон', 'Отдел', 'Должность', '']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const inv of inventors) {
        this.renderInventorRow(tbody, inv);
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  private renderInventorRow(tbody: HTMLElement, inv: Inventor): void {
    const tr = tbody.createEl('tr');
    tr.createEl('td').setText(inv.name);
    tr.createEl('td').setText(inv.email || '—');
    tr.createEl('td').setText(inv.phone || '—');
    tr.createEl('td').setText(inv.department || '—');
    tr.createEl('td').setText(inv.position || '—');
    const actions = tr.createEl('td');
    if (!this.canEditRefs) return;
    const editBtn = actions.createEl('button', { text: '✎', cls: 'tn-btn tn-btn-ghost' });
    const delBtn = actions.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    editBtn.addEventListener('click', () => this.renderInventorEditRow(tbody, tr, inv));
    delBtn.addEventListener('click', async () => {
      if (!window.confirm(`Удалить испытателя «${inv.name}»?`)) return;
      try {
        await this.plugin.syncService.deleteInventor(inv.id);
        new Notice('Испытатель удалён');
        await this.renderInventors();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** Заменяет строку таблицы на форму правки испытателя (по месту, без перехода). */
  private renderInventorEditRow(tbody: HTMLElement, tr: HTMLElement, inv: Inventor): void {
    tr.empty();
    const nameInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    nameInp.value = inv.name;
    const emailInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    emailInp.value = inv.email;
    const phoneInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    phoneInp.value = inv.phone;
    const deptInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    deptInp.value = inv.department;
    const posInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    posInp.value = inv.position;
    const actions = tr.createEl('td');
    const saveBtn = actions.createEl('button', { text: '💾', cls: 'tn-btn tn-btn-primary' });
    const cancelBtn = actions.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    saveBtn.addEventListener('click', async () => {
      if (!nameInp.value.trim() || !emailInp.value.trim()) { new Notice('Укажите ФИО и e-mail'); return; }
      try {
        await this.plugin.syncService.updateInventor(inv.id, {
          name: nameInp.value.trim(),
          email: emailInp.value.trim(),
          phone: phoneInp.value.trim(),
          department: deptInp.value.trim(),
          position: posInp.value.trim(),
        });
        new Notice('Испытатель обновлён');
        await this.renderInventors();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
    cancelBtn.addEventListener('click', () => void this.renderInventors());
  }

  private renderInventorForm(): void {
    const form = this.bodyEl.createDiv({ cls: 'tn-lims-series-form' });
    const row = form.createDiv({ cls: 'tn-lims-flex' });
    const name = row.createEl('input', { attr: { type: 'text', placeholder: 'ФИО' }, cls: 'tn-lims-input' });
    const email = row.createEl('input', { attr: { type: 'text', placeholder: 'E-mail' }, cls: 'tn-lims-input' });
    const phone = row.createEl('input', { attr: { type: 'text', placeholder: 'Телефон' }, cls: 'tn-lims-input' });
    const department = row.createEl('input', { attr: { type: 'text', placeholder: 'Отдел' }, cls: 'tn-lims-input' });
    const position = row.createEl('input', { attr: { type: 'text', placeholder: 'Должность' }, cls: 'tn-lims-input' });
    const addBtn = row.createEl('button', { text: '➕ Добавить', cls: 'tn-btn tn-btn-primary' });
    addBtn.addEventListener('click', async () => {
      if (!name.value.trim() || !email.value.trim()) { new Notice('Укажите ФИО и e-mail'); return; }
      try {
        await this.plugin.syncService.createInventor({
          name: name.value.trim(), email: email.value.trim(),
          phone: phone.value.trim() || undefined,
          department: department.value.trim() || undefined,
          position: position.value.trim() || undefined,
        });
        new Notice('Испытатель добавлен');
        await this.renderInventors();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** Оборудование — список (viewer) + создание/правка/удаление (editor+). */
  private async renderEquipment(): Promise<void> {
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const equipment = await this.plugin.syncService.listEquipment();
      this.bodyEl.empty();
      if (this.canEditRefs) {
        this.renderEquipmentForm();
      }
      if (equipment.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Оборудования пока нет.');
        return;
      }
      const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      for (const h of ['Код', 'Название', 'Расположение', 'Ответственный', 'Статус', '']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const eq of equipment) {
        this.renderEquipmentRow(tbody, eq);
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  private renderEquipmentRow(tbody: HTMLElement, eq: Equipment): void {
    const tr = tbody.createEl('tr');
    tr.createEl('td').setText(eq.code);
    tr.createEl('td').setText(eq.name);
    tr.createEl('td').setText(eq.location || '—');
    tr.createEl('td').setText(eq.responsible || '—');
    tr.createEl('td').setText(eq.status || '—');
    const actions = tr.createEl('td');
    if (!this.canEditRefs) return;
    const editBtn = actions.createEl('button', { text: '✎', cls: 'tn-btn tn-btn-ghost' });
    const delBtn = actions.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    editBtn.addEventListener('click', () => this.renderEquipmentEditRow(tbody, tr, eq));
    delBtn.addEventListener('click', async () => {
      if (!window.confirm(`Удалить оборудование «${eq.name}»?`)) return;
      try {
        await this.plugin.syncService.deleteEquipment(eq.id);
        new Notice('Оборудование удалено');
        await this.renderEquipment();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** Заменяет строку таблицы на форму правки оборудования (по месту, без перехода). */
  private renderEquipmentEditRow(tbody: HTMLElement, tr: HTMLElement, eq: Equipment): void {
    tr.empty();
    const codeInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    codeInp.value = eq.code;
    const nameInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    nameInp.value = eq.name;
    const locInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    locInp.value = eq.location;
    const respInp = tr.createEl('td').createEl('input', { attr: { type: 'text' }, cls: 'tn-lims-input' });
    respInp.value = eq.responsible;
    tr.createEl('td').setText(eq.status || '—');
    const actions = tr.createEl('td');
    const saveBtn = actions.createEl('button', { text: '💾', cls: 'tn-btn tn-btn-primary' });
    const cancelBtn = actions.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
    saveBtn.addEventListener('click', async () => {
      if (!codeInp.value.trim() || !nameInp.value.trim()) { new Notice('Укажите код и название'); return; }
      try {
        await this.plugin.syncService.updateEquipment(eq.id, {
          code: codeInp.value.trim(),
          name: nameInp.value.trim(),
          location: locInp.value.trim(),
          responsible: respInp.value.trim(),
        });
        new Notice('Оборудование обновлено');
        await this.renderEquipment();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
    cancelBtn.addEventListener('click', () => void this.renderEquipment());
  }

  private renderEquipmentForm(): void {
    const form = this.bodyEl.createDiv({ cls: 'tn-lims-series-form' });
    const row = form.createDiv({ cls: 'tn-lims-flex' });
    const code = row.createEl('input', { attr: { type: 'text', placeholder: 'Код' }, cls: 'tn-lims-input' });
    const name = row.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-lims-input' });
    const location = row.createEl('input', { attr: { type: 'text', placeholder: 'Расположение' }, cls: 'tn-lims-input' });
    const responsible = row.createEl('input', { attr: { type: 'text', placeholder: 'Ответственный' }, cls: 'tn-lims-input' });
    const addBtn = row.createEl('button', { text: '➕ Добавить', cls: 'tn-btn tn-btn-primary' });
    addBtn.addEventListener('click', async () => {
      if (!code.value.trim() || !name.value.trim()) { new Notice('Укажите код и название'); return; }
      try {
        await this.plugin.syncService.createEquipment({
          code: code.value.trim(), name: name.value.trim(),
          location: location.value.trim() || undefined,
          responsible: responsible.value.trim() || undefined,
        });
        new Notice('Оборудование добавлено');
        await this.renderEquipment();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
  }

  /** Сотрудники лаборатории — только admin (GET /lab-members сам требует admin на сервере). */
  private async renderLabMembers(): Promise<void> {
    if (!this.canAdmin) {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText(
        'Раздел доступен только администратору (управление сотрудниками лаборатории).'
      );
      return;
    }
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const all = await this.plugin.syncService.listLabMembers();
      const members = this.labId ? all.filter(m => m.lab_id === this.labId) : all;
      this.bodyEl.empty();
      this.renderLabMemberForm();
      if (members.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Сотрудников пока нет.');
        return;
      }
      const table = this.bodyEl.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      for (const h of ['E-mail', 'Роль', '']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const m of members) {
        const tr = tbody.createEl('tr');
        tr.createEl('td').setText(m.email);
        tr.createEl('td').setText(m.role);
        const removeBtn = tr.createEl('td').createEl('button', { text: '✖ Убрать', cls: 'tn-btn tn-btn-ghost' });
        removeBtn.addEventListener('click', async () => {
          if (!window.confirm(`Убрать «${m.email}» из лаборатории?`)) return;
          try {
            await this.plugin.syncService.removeLabMember(m.lab_id, m.email);
            new Notice('Сотрудник удалён');
            await this.renderLabMembers();
          } catch (e: unknown) {
            new Notice(`Ошибка: ${errorMessage(e)}`);
          }
        });
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  private renderLabMemberForm(): void {
    const form = this.bodyEl.createDiv({ cls: 'tn-lims-series-form' });
    const row = form.createDiv({ cls: 'tn-lims-flex' });
    const email = row.createEl('input', { attr: { type: 'text', placeholder: 'E-mail сотрудника' }, cls: 'tn-lims-input' });
    const roleSelect = row.createEl('select', { cls: 'tn-lims-select' });
    roleSelect.createEl('option', { value: 'lab_operator', text: 'Испытатель (lab_operator)' });
    roleSelect.createEl('option', { value: 'lab_admin', text: 'Администратор лабы (lab_admin)' });
    const addBtn = row.createEl('button', { text: '➕ Добавить', cls: 'tn-btn tn-btn-primary' });
    addBtn.addEventListener('click', async () => {
      if (!email.value.trim() || !this.labId) { new Notice('Укажите e-mail и выберите лабораторию'); return; }
      try {
        await this.plugin.syncService.setLabMember(this.labId, email.value.trim(), roleSelect.value);
        new Notice('Сотрудник добавлен');
        await this.renderLabMembers();
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
      }
    });
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

  /** Admin+ (app-level, включая superadmin) — методы-конфиги, сотрудники лаборатории.
   * Точная проверка lab_admin/lab_auditor (per-lab) недоступна клиенту без нового
   * «моя роль в этой лабе» эндпоинта (GET /lab-members сам admin-only) — оставлено
   * на будущее; сервер всё равно валидирует через lab_members (requireLabAccess/
   * requireLabRead), так что риска нет — только UX-огрубление. */
  private get canAdmin(): boolean {
    return this.myRole === 'admin' || this.myRole === 'superadmin';
  }

  /** Editor+ (app-level, включая admin/superadmin) — испытатели/оборудование. */
  private get canEditRefs(): boolean {
    return this.myRole === 'editor' || this.myRole === 'admin' || this.myRole === 'superadmin';
  }
}