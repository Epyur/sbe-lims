import { ItemView, Notice, WorkspaceLeaf } from 'obsidian';
import type SbeLimsPlugin from '../main';
import type {
  AttributeDataType,
  AttributeFillMethod,
  AttributeLevel,
  BooleanClassificationRule,
  ChartConfig,
  ClassificationRule,
  ComplianceClassificationRule,
  Equipment,
  Inventor,
  Lab,
  LabGroup,
  LabMember,
  LabObject,
  LabProject,
  LimsRequest,
  MeasurementResult,
  MethodAttribute,
  MethodConfig,
  PresentationField,
  ThresholdClassificationRule,
} from '../types/lims';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import type { ExistingAttributeSummary } from '../services/llm-assist.service';
import { extractStandardText } from '../services/rtf-to-text';

export const SBE_LIMS_VIEW_TYPE = 'sbe-lims-view';

const STATUS_LABELS: Record<string, string> = {
  new: '🟢 Новая',
  received: '📦 Образцы получены',
  processing: '🟡 В работе',
  completed: '✅ Завершена',
};

/** Префикс legacy-номера трекера почты (LIMS_LPI); requests.external_id хранит
 * только числовую часть — префикс восстанавливается для отображения. */
const EXTERNAL_ID_PREFIX = 'LPIZAYAVKINAPRO-';

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
  /** Кэш объектов/проектов/групп — только для отображения деталей заявки
   * (как у заявителя в sbe-requests), не для редактирования. */
  private objects: LabObject[] = [];
  private projects: LabProject[] = [];
  private groups: LabGroup[] = [];
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
    return 'LogicLAB.ЛИМС';
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
    topbar.createDiv({ cls: 'tn-lims-module-title', text: 'LogicLAB.ЛИМС' });
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

    // синхронизация — оперативное обновление лабораторий/методов/текущей страницы
    const syncBtn = sidebar.createDiv({ cls: 'tn-lims-collapse' });
    syncBtn.createSpan({ text: '🔄' });
    syncBtn.createSpan({ cls: 'tn-lims-collapse-lbl', text: 'Синхронизация' });
    syncBtn.addEventListener('click', () => { void this.syncAndRender(); });

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
      const [labs, objects, projects, groups] = await Promise.all([
        this.plugin.syncService.listLabs(),
        this.plugin.syncService.listObjects(),
        this.plugin.syncService.listProjects(),
        this.plugin.syncService.listGroups(),
      ]);
      this.labs = labs;
      this.objects = objects;
      this.projects = projects;
      this.groups = groups;
    } catch (e: unknown) {
      this.myRole = '';
      this.labs = [];
      this.objects = [];
      this.projects = [];
      this.groups = [];
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

  /** Оперативное обновление: роль, лаборатории, кэш методов и текущая страница — с сервера. */
  async syncAndRender(): Promise<void> {
    try {
      const perm = await this.plugin.syncService.getMyPermission();
      this.myRole = perm.role;
      const [labs, objects, projects, groups] = await Promise.all([
        this.plugin.syncService.listLabs(),
        this.plugin.syncService.listObjects(),
        this.plugin.syncService.listProjects(),
        this.plugin.syncService.listGroups(),
      ]);
      this.labs = labs;
      this.objects = objects;
      this.projects = projects;
      this.groups = groups;
      await this.plugin.refreshMethods();
      this.refreshLabSwitcher();
      if (this.labId !== null && this.labSwitchEl.querySelector(`option[value="${this.labId}"]`)) {
        this.labSwitchEl.value = String(this.labId);
      } else if (this.labSwitchEl.options.length > 0) {
        this.labId = this.labs[0].id;
        this.labSwitchEl.value = String(this.labId);
      } else {
        this.labId = null;
      }
      this.settingsBtnEl.style.display = (this.myRole === 'admin' || this.myRole === 'superadmin') ? '' : 'none';
      this.syncNavActive();
      await this.renderPage();
    } catch (e: unknown) {
      new Notice(`ЛИМС: синхронизация не выполнена — ${errorMessage(e)}`);
      this.syncNavActive();
      await this.renderPage();
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

  /** Переход из справочника «Объекты» к заявкам конкретного объекта (нужен
   * из-за объединения дублей объектов в справочнике — 2026-08-21: у одного
   * объекта может быть много заявок, в т.ч. объекты-плейсхолдеры «без
   * названия», у которых иначе не отличить, какая заявка к ним относится). */
  private async showRequestsForObject(objectId: number, objectLabel: string): Promise<void> {
    this.key = 'requests';
    this.syncNavActive();
    const title = `Заявки объекта «${objectLabel}»`;
    this.crumbEl.setText(`${this.currentLabName()} · ${title}`);
    this.pageTitleEl.setText(title);
    this.pageSubEl.setText('Отфильтровано по объекту из справочника «Объекты»');
    await this.renderRequests(r => r.object_id === objectId);
  }

  /** Список заявок (возврат из карточки). Опциональный фильтр — для «Очереди»/«Результатов». */
  private async renderRequests(filter?: (r: LimsRequest) => boolean): Promise<void> {
    this.currentRequestsFilter = filter;
    this.bodyEl.empty();
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      let requests = await this.plugin.syncService.listRequests();
      if (filter) requests = requests.filter(filter);
      // Новые сверху: сперва по году номера, затем по самому номеру (2026-08-21).
      requests = [...requests].sort((a, b) => (b.number_year - a.number_year) || (b.number_seq - a.number_seq));
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

  private objectName(objectId: number): string {
    const obj = this.objects.find(o => o.id === objectId);
    return obj ? obj.name : '—';
  }

  private projectName(projectId: number): string {
    if (projectId <= 0) return 'Без проекта';
    const p = this.projects.find(pr => pr.id === projectId);
    return p ? `${p.code}${p.name ? ' — ' + p.name : ''}` : '—';
  }

  private groupName(groupId: number): string {
    if (groupId <= 0) return '—';
    const g = this.groups.find(gr => gr.id === groupId);
    return g ? g.name : '—';
  }

  private priorityLabel(priority: string): string {
    switch (priority) {
      case 'critical': return 'Критичный';
      case 'blocker': return 'Блокер (остановить исполнение других заявок)';
      case 'normal': return 'Средний';
      default: return priority || 'Средний';
    }
  }

  private purposeLabel(purpose: string): string {
    switch (purpose) {
      case 'quality_control': return 'Текущий контроль';
      case 'rnd': return 'НИОКР';
      case 'certification': return 'Сертификация';
      case 'declaration': return 'Декларирование';
      default: return purpose || '—';
    }
  }

  private formatDate(iso: string): string {
    if (!iso) return '—';
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? '—' : d.toLocaleDateString();
  }

  /** Карточка заявки: серии результатов, ввод, расчёт, статус, графики, протокол. */
  private async renderRequestDetail(req: LimsRequest): Promise<void> {
    this.bodyEl.empty();

    const back = this.bodyEl.createEl('button', { text: '← Назад', cls: 'tn-btn tn-btn-ghost' });
    back.addEventListener('click', () => void this.renderRequests(this.currentRequestsFilter));

    this.bodyEl.createEl('h3', { text: `№ ${req.lab_number || `${req.number_seq}/${req.number_year}`} — ${req.title || 'без названия'}` });

    // Все детали заявки, как у заявителя (sbe-requests) — сразу под номером,
    // единым блоком (2026-08-21, по просьбе пользователя).
    const meta = this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-mb12' });
    meta.createDiv({ text: `Номер заказчику: ${req.customer_number || '—'}` });
    meta.createDiv({ text: `📁 Проект: ${this.projectName(req.project_id)}` });
    meta.createDiv({ text: `👥 Группа: ${this.groupName(req.group_id)}` });
    meta.createDiv({ text: `🔬 Объект: ${this.objectName(req.object_id)}` });
    if (req.ekn) meta.createDiv({ text: `🔢 ЕКН: ${req.ekn}` });
    if (req.external_id) meta.createDiv({ text: `📧 Внешний идентификатор: ${EXTERNAL_ID_PREFIX}${req.external_id}` });
    const obj = this.objects.find(o => o.id === req.object_id);
    const chars = obj?.characteristics as
      { batch_number?: unknown; sample_id?: unknown; target_indicators?: Record<string, string> }
      | undefined;
    if (chars) {
      if (chars.batch_number !== undefined) meta.createDiv({ text: `📦 Номер партии: ${chars.batch_number}` });
      if (chars.sample_id) meta.createDiv({ text: `🏷 Идентификатор образца: ${chars.sample_id}` });
      const chosenTarget = chars.target_indicators?.[String(req.method_id)];
      if (chosenTarget) meta.createDiv({ text: `🎯 Целевой показатель: ${chosenTarget}` });
    }
    meta.createDiv({ text: `⚡ Приоритет: ${this.priorityLabel(req.priority)}` });
    if (req.test_purpose) meta.createDiv({ text: `🎯 Цель испытания: ${this.purposeLabel(req.test_purpose)}` });
    if (req.lab_id > 0) {
      const lab = this.labs.find(l => l.id === req.lab_id);
      meta.createDiv({ text: `🏢 Лаборатория: ${lab ? (lab.name || lab.code) : '—'}` });
    }
    meta.createDiv({ text: `👤 Владелец: ${req.owner_email || '—'}` });
    meta.createDiv({ text: `📅 Создана: ${this.formatDate(req.created_at)}` });
    meta.createDiv({ text: `📅 Обновлена: ${this.formatDate(req.updated_at)}` });
    meta.createDiv({ text: `Статус: ${STATUS_LABELS[req.status] || req.status}` });

    if (req.description) {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-mb12' }).createDiv({ text: `📝 ${req.description}` });
    }

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
      this.renderResultsTable(methodDiv, seriesRows, req.method_id);
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

  /** Таблица серий — колонки по methods.presentation (show_in_ui, порядок/подписи),
   * фолбэк на все найденные ключи в алфавитном порядке для методов, ещё не
   * сконфигурированных в блоке 3. «Фотография»-атрибуты — превью, не текст. */
  private renderResultsTable(container: HTMLElement, rows: MeasurementResult[], methodId: number): void {
    const cfg = this.methodConfigOf(methodId);
    const attrs = cfg.input_parameters;
    const keys = new Set<string>();
    for (const r of rows) for (const k of Object.keys(r.values)) keys.add(k);

    const used = new Set<string>();
    const columns: Array<{ key: string; label: string; isPhoto: boolean }> = [];
    for (const f of cfg.presentation.fields) {
      if (used.has(f.attribute_id)) continue;
      used.add(f.attribute_id);
      if (!f.show_in_ui || !keys.has(f.attribute_id)) continue;
      const attr = attrs.find(a => a.id === f.attribute_id);
      columns.push({ key: f.attribute_id, label: f.label || attr?.name || f.attribute_id, isPhoto: attr?.data_type === 'photo' });
    }
    const rest = Array.from(keys).filter(k => !used.has(k)).sort();
    for (const k of rest) {
      const attr = attrs.find(a => a.id === k);
      columns.push({ key: k, label: attr?.name || k, isPhoto: attr?.data_type === 'photo' });
    }

    const table = container.createEl('table', { cls: 'tn-table' });
    const thead = table.createEl('thead').createEl('tr');
    thead.createEl('th').setText('Серия');
    for (const col of columns) thead.createEl('th').setText(col.label);
    const tbody = table.createEl('tbody');
    for (const r of rows) {
      const tr = tbody.createEl('tr');
      tr.createEl('td').setText(String(r.series_num));
      for (const col of columns) {
        const td = tr.createEl('td');
        const val = r.values[col.key];
        if (col.isPhoto && typeof val === 'string' && val) {
          td.createEl('img', { attr: { src: val, alt: col.label }, cls: 'tn-lims-thumb' });
        } else {
          td.setText(this.fmt(val));
        }
      }
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

  /** Конфигуратор метода (admin): атрибуты (input_parameters) + правила
   * классификации — структурные редакторы; chart_configs — пока JSON-текстареа
   * (WYSIWYG-конструктор — отдельная фаза); formulas вычисляется сервером из
   * атрибутов при каждом сохранении (deriveFormulasFromAttributes, lab-service) —
   * здесь только просмотр, редактирование напрямую не имеет смысла. */
  private renderMethodConfigForm(container: HTMLElement, methodId: number): void {
    const existing = container.querySelector('.tn-lims-series-form');
    if (existing) { existing.remove(); return; }
    const cfg = this.methodConfigOf(methodId);
    const method = this.plugin.methods.find(m => m.id === methodId);
    const determinableIndicators = method?.determinable_indicators || [];
    const form = container.createDiv({ cls: 'tn-lims-series-form' });

    form.createDiv({ cls: 'tn-lims-meta' }).setText('Описание:');
    const description = form.createEl('input', { attr: { type: 'text', placeholder: 'Описание' }, cls: 'tn-lims-input' });
    description.value = method?.description || '';
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Лаборатории (метод может принадлежать нескольким):');
    const getLabIDs = this.renderLabCheckboxes(form, method?.lab_ids || []);

    // ---- Импорт из стандарта (ИИ-черновик атрибутов/правил/представления) ----
    // DOM-элементы создаются здесь (чтобы блок был визуально первым), обработчик
    // click навешивается ниже — после того, как attrs/rules/presentationFields и
    // их redraw-замыкания уже объявлены (нужны обработчику).
    form.createEl('h4', { text: '📄 Импортировать из стандарта (ИИ)' });
    const importRow = form.createDiv({ cls: 'tn-lims-flex' });
    const importFileInput = importRow.createEl('input', { attr: { type: 'file', accept: '.rtf,.txt' } });
    const importBtn = importRow.createEl('button', { text: '🤖 Сформировать проект', cls: 'tn-btn tn-btn-ghost' });

    // ---- Блок 1: атрибуты метода ----
    const attrs: MethodAttribute[] = cfg.input_parameters.map(a => ({ ...a }));
    form.createEl('h4', { text: 'Атрибуты метода' });
    const attrsListEl = form.createDiv();
    let redrawAttrs: () => void;
    redrawAttrs = () => {
      attrsListEl.empty();
      this.renderAttributeRows(attrsListEl, attrs, determinableIndicators, () => redrawAttrs());
    };
    redrawAttrs();
    const addAttrBtn = form.createEl('button', { text: '➕ Добавить атрибут', cls: 'tn-btn tn-btn-ghost' });
    addAttrBtn.addEventListener('click', () => {
      attrs.push({ id: '', name: '', data_type: 'text', fill_method: 'manual', level: 'experiment' });
      redrawAttrs();
    });

    // ---- Блок 2: правила классификации ----
    const rules: ClassificationRule[] = cfg.classification.map(r => ({ ...r }));
    form.createEl('h4', { text: 'Правила классификации' });
    const rulesListEl = form.createDiv();
    let redrawRules: () => void;
    redrawRules = () => {
      rulesListEl.empty();
      this.renderClassificationRows(rulesListEl, rules, attrs, determinableIndicators, () => redrawRules());
    };
    redrawRules();
    const addRuleBtn = form.createEl('button', { text: '➕ Добавить правило', cls: 'tn-btn tn-btn-ghost' });
    addRuleBtn.addEventListener('click', () => {
      rules.push({ rule_type: 'threshold', parameter_name: '', thresholds: [] });
      redrawRules();
    });

    // formulas — вычисляется сервером, только просмотр
    form.createDiv({ cls: 'tn-lims-meta' }).setText('formulas (вычисляется сервером из атрибутов — только просмотр):');
    const formulasPreview = form.createEl('pre', { cls: 'tn-lims-input' });
    formulasPreview.setText(JSON.stringify(cfg.formulas, null, 2));

    // ---- Блок 3а: представление данных (порядок/подписи/видимость колонок в
    // UI-таблице результатов и в протоколе) — drag-and-drop + живой предпросмотр ----
    const presentationFields: PresentationField[] = cfg.presentation.fields
      .filter(f => attrs.some(a => a.id === f.attribute_id))
      .map(f => ({ ...f }));
    // атрибуты, которых пока нет в presentation (новые/метод впервые конфигурируется
    // в блоке 3) — добавляем в конец в их текущем порядке, чтобы список сразу
    // отражал все атрибуты, а не только уже расставленные.
    for (const a of attrs) {
      if (!a.id || presentationFields.some(f => f.attribute_id === a.id)) continue;
      presentationFields.push({ attribute_id: a.id, show_in_ui: true, show_in_protocol: true });
    }
    form.createEl('h4', { text: 'Представление данных (таблица результатов и протокол)' });
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Перетащите строки, чтобы задать порядок столбцов:');
    const presentationListEl = form.createDiv();
    const previewEl = form.createDiv();
    let redrawPresentation: () => void;
    redrawPresentation = () => {
      presentationListEl.empty();
      this.renderPresentationRows(presentationListEl, presentationFields, attrs, () => redrawPresentation());
      this.renderPresentationPreview(previewEl, presentationFields, attrs);
    };
    redrawPresentation();

    importBtn.addEventListener('click', async () => {
      const file = importFileInput.files?.[0];
      if (!file) { new Notice('Выберите файл стандарта (.rtf или .txt)'); return; }
      if ((attrs.length > 0 || rules.length > 0) && !window.confirm(
        'Заменить текущие атрибуты и правила проектом от ИИ? Ничего не сохранится, пока вы не нажмёте «Сохранить».',
      )) {
        return;
      }
      importBtn.disabled = true;
      try {
        const buf = await file.arrayBuffer();
        const standardText = extractStandardText(buf, file.name);
        // атрибуты ДРУГИХ методов — для переиспользования по смыслу, не самоцитирование
        const seenIds = new Set<string>();
        const existingAttributes: ExistingAttributeSummary[] = [];
        for (const m of this.plugin.methods) {
          if (m.id === methodId) continue;
          // methodConfigOf нормализует input_parameters — напрямую m.input_parameters
          // трогать нельзя: сервер, с которым сейчас говорит клиент, может быть
          // старее фронтенда (см. AGENTS.md) и вообще не отдавать это поле.
          for (const a of this.methodConfigOf(m.id).input_parameters) {
            if (!a.id || seenIds.has(a.id)) continue;
            seenIds.add(a.id);
            existingAttributes.push({ id: a.id, name: a.name, data_type: a.data_type, fill_method: a.fill_method, level: a.level });
          }
        }
        const draft = await this.plugin.llmAssist.draftAttributesAndClassification(standardText, existingAttributes);
        attrs.splice(0, attrs.length, ...(Array.isArray(draft.attributes) ? draft.attributes : []));
        rules.splice(0, rules.length, ...(Array.isArray(draft.classification) ? draft.classification : []));
        if (attrs.length === 0) {
          new Notice('ИИ не смог сформировать ни одного атрибута из этого файла — проверьте, что это текстовый .rtf/.txt со стандартом, и что модель LLM настроена в настройках плагина');
          return;
        }
        const draftPresentation = await this.plugin.llmAssist.draftPresentation(attrs);
        presentationFields.splice(0, presentationFields.length, ...(Array.isArray(draftPresentation.fields) ? draftPresentation.fields : []));
        redrawAttrs();
        redrawRules();
        redrawPresentation();
        new Notice('Проект сформирован — проверьте, особенно значения с пометкой «ТРЕБУЕТ ПРОВЕРКИ», перед сохранением');
      } catch (e: unknown) {
        // без явного console.error ошибка была видна только в Notice (текст без
        // стека) — обнаружено при живой отладке "объект не итерабельный": Notice
        // не даёт понять, где именно бросило исключение.
        console.error('ЛИМС: ошибка импорта из стандарта:', e);
        new Notice(`Ошибка формирования проекта: ${errorMessage(e)}`);
      } finally {
        importBtn.disabled = false;
      }
    });

    // ---- Блок 3б: графики (chart_configs) — структурный редактор ----
    // Нормализация на случай конфигов, сохранённых старым raw-JSON textarea
    // (до блока 3) без части полей — иначе редактор упал бы на .map(undefined).
    const charts: ChartConfig[] = cfg.chart_configs.map((c, i) => ({
      id: c.id || `chart_${i}`,
      title: c.title,
      chart_type: c.chart_type === 'scatter' || c.chart_type === 'bar' ? c.chart_type : 'line',
      x_column: c.x_column,
      x_label: c.x_label,
      y_label: c.y_label,
      series_config: Array.isArray(c.series_config) ? c.series_config.map(s => ({ ...s })) : [],
    }));
    form.createEl('h4', { text: 'Графики' });
    const chartsListEl = form.createDiv();
    let redrawCharts: () => void;
    redrawCharts = () => {
      chartsListEl.empty();
      this.renderChartRows(chartsListEl, charts, attrs, () => redrawCharts());
    };
    redrawCharts();
    const addChartBtn = form.createEl('button', { text: '➕ Добавить график', cls: 'tn-btn tn-btn-ghost' });
    addChartBtn.addEventListener('click', () => {
      charts.push({ id: `chart_${Date.now()}`, chart_type: 'line', series_config: [] });
      redrawCharts();
    });

    const saveBtn = form.createEl('button', { text: '💾 Сохранить', cls: 'tn-btn tn-btn-primary' });
    saveBtn.addEventListener('click', async () => {
      const labIDs = getLabIDs();
      if (labIDs.length === 0) { new Notice('Укажите хотя бы одну лабораторию'); return; }
      const validationError = this.validateAttributesAndRules(attrs, rules) || this.validateChartsAndPresentation(charts, presentationFields, attrs);
      if (validationError) { new Notice(validationError); return; }
      const attrIds = new Set(attrs.map(a => a.id));
      const patch: Partial<MethodConfig> & { lab_ids?: number[]; description?: string } = {
        lab_ids: labIDs,
        description: description.value.trim(),
        input_parameters: attrs,
        classification: rules,
        chart_configs: charts,
        presentation: { fields: presentationFields.filter(f => attrIds.has(f.attribute_id)) },
      };
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

  /** Рисует перетаскиваемые строки блока «Представление данных» (нативный HTML5
   * drag-and-drop, без библиотек) — порядок элементов массива fields = порядок
   * столбцов. onChange — полная перерисовка (после drop/удаления). */
  private renderPresentationRows(
    container: HTMLElement,
    fields: PresentationField[],
    attrs: MethodAttribute[],
    onChange: () => void,
  ): void {
    let dragFromIdx: number | null = null;
    fields.forEach((f, idx) => {
      const attr = attrs.find(a => a.id === f.attribute_id);
      const row = container.createDiv({ cls: 'tn-lims-method', attr: { draggable: 'true' } });
      row.style.cursor = 'grab';
      row.addEventListener('dragstart', () => { dragFromIdx = idx; });
      row.addEventListener('dragover', (ev) => { ev.preventDefault(); });
      row.addEventListener('drop', (ev) => {
        ev.preventDefault();
        if (dragFromIdx === null || dragFromIdx === idx) return;
        const [moved] = fields.splice(dragFromIdx, 1);
        fields.splice(idx, 0, moved);
        dragFromIdx = null;
        onChange();
      });

      const rowFlex = row.createDiv({ cls: 'tn-lims-flex' });
      rowFlex.createSpan({ text: '⠿', cls: 'tn-lims-meta' });
      rowFlex.createSpan({ text: attr?.name || f.attribute_id });
      const labelInput = rowFlex.createEl('input', {
        attr: { type: 'text', placeholder: 'подпись (по умолчанию — название атрибута)' },
        cls: 'tn-lims-input',
      });
      labelInput.value = f.label || '';
      labelInput.addEventListener('change', () => { f.label = labelInput.value.trim() || undefined; onChange(); });

      const uiLabel = rowFlex.createEl('label', { cls: 'tn-lims-flex' });
      const uiCb = uiLabel.createEl('input', { attr: { type: 'checkbox' } });
      uiCb.checked = f.show_in_ui;
      uiLabel.createSpan({ text: 'в UI' });
      uiCb.addEventListener('change', () => { f.show_in_ui = uiCb.checked; onChange(); });

      const protoLabel = rowFlex.createEl('label', { cls: 'tn-lims-flex' });
      const protoCb = protoLabel.createEl('input', { attr: { type: 'checkbox' } });
      protoCb.checked = f.show_in_protocol;
      protoLabel.createSpan({ text: 'в протоколе' });
      protoCb.addEventListener('change', () => { f.show_in_protocol = protoCb.checked; onChange(); });
    });
  }

  /** Живой предпросмотр таблицы результатов по текущему порядку/подписям/видимости
   * (одна вымышленная строка примера — без реальных данных, только layout). */
  private renderPresentationPreview(container: HTMLElement, fields: PresentationField[], attrs: MethodAttribute[]): void {
    container.empty();
    container.createDiv({ cls: 'tn-lims-meta' }).setText('Предпросмотр (пример):');
    const visible = fields.filter(f => f.show_in_ui);
    const table = container.createEl('table', { cls: 'tn-table' });
    const thead = table.createEl('thead').createEl('tr');
    thead.createEl('th').setText('Серия');
    for (const f of visible) {
      const attr = attrs.find(a => a.id === f.attribute_id);
      thead.createEl('th').setText(f.label || attr?.name || f.attribute_id);
    }
    const tr = table.createEl('tbody').createEl('tr');
    tr.createEl('td').setText('1');
    for (const f of visible) {
      const attr = attrs.find(a => a.id === f.attribute_id);
      const td = tr.createEl('td');
      if (attr?.data_type === 'photo') {
        td.setText('[фото]');
      } else {
        td.setText('—');
      }
    }
  }

  /** Карточки графиков (блок 3б, chart_configs) — тот же паттерн, что атрибуты/
   * правила классификации. */
  private renderChartRows(
    container: HTMLElement,
    charts: ChartConfig[],
    attrs: MethodAttribute[],
    onChange: () => void,
  ): void {
    charts.forEach((chart, idx) => {
      const card = container.createDiv({ cls: 'tn-lims-method' });
      const row1 = card.createDiv({ cls: 'tn-lims-flex' });
      const titleInput = row1.createEl('input', { attr: { type: 'text', placeholder: 'Заголовок графика' }, cls: 'tn-lims-input' });
      titleInput.value = chart.title || '';
      titleInput.addEventListener('change', () => { chart.title = titleInput.value.trim() || undefined; });
      row1.createSpan({ text: 'Тип:' });
      const typeSelect = row1.createEl('select', { cls: 'tn-lims-select' });
      for (const [val, label] of [['line', 'Линия'], ['scatter', 'Точки'], ['bar', 'Столбцы']] as Array<[ChartConfig['chart_type'], string]>) {
        typeSelect.createEl('option', { attr: { value: val }, text: label });
      }
      typeSelect.value = chart.chart_type;
      typeSelect.addEventListener('change', () => { chart.chart_type = typeSelect.value as ChartConfig['chart_type']; });
      const delBtn = row1.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => { charts.splice(idx, 1); onChange(); });

      const row2 = card.createDiv({ cls: 'tn-lims-flex' });
      row2.createSpan({ text: 'Ось X:' });
      const xSelect = row2.createEl('select', { cls: 'tn-lims-select' });
      xSelect.createEl('option', { attr: { value: '' }, text: '— номер серии —' });
      for (const a of attrs) {
        if (!a.id) continue;
        xSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
      }
      xSelect.value = chart.x_column || '';
      xSelect.addEventListener('change', () => { chart.x_column = xSelect.value || undefined; });
      const xLabelInput = row2.createEl('input', { attr: { type: 'text', placeholder: 'Подпись оси X' }, cls: 'tn-lims-input' });
      xLabelInput.value = chart.x_label || '';
      xLabelInput.addEventListener('change', () => { chart.x_label = xLabelInput.value.trim() || undefined; });
      const yLabelInput = row2.createEl('input', { attr: { type: 'text', placeholder: 'Подпись оси Y' }, cls: 'tn-lims-input' });
      yLabelInput.value = chart.y_label || '';
      yLabelInput.addEventListener('change', () => { chart.y_label = yLabelInput.value.trim() || undefined; });

      card.createDiv({ cls: 'tn-lims-meta' }).setText('Ряды:');
      const seriesListEl = card.createDiv();
      const redrawSeries = () => {
        seriesListEl.empty();
        chart.series_config.forEach((sc, sIdx) => {
          const sRow = seriesListEl.createDiv({ cls: 'tn-lims-flex' });
          const srcSelect = sRow.createEl('select', { cls: 'tn-lims-select' });
          srcSelect.createEl('option', { attr: { value: '' }, text: '— источник —' });
          for (const a of attrs) {
            if (!a.id) continue;
            srcSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
          }
          srcSelect.value = sc.source_param;
          srcSelect.addEventListener('change', () => { sc.source_param = srcSelect.value; });
          const sLabelInput = sRow.createEl('input', { attr: { type: 'text', placeholder: 'Подпись ряда' }, cls: 'tn-lims-input' });
          sLabelInput.value = sc.label || '';
          sLabelInput.addEventListener('change', () => { sc.label = sLabelInput.value.trim() || undefined; });
          const sDelBtn = sRow.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
          sDelBtn.addEventListener('click', () => { chart.series_config.splice(sIdx, 1); redrawSeries(); });
        });
      };
      redrawSeries();
      const addSeriesBtn = card.createEl('button', { text: '➕ Ряд', cls: 'tn-btn tn-btn-ghost' });
      addSeriesBtn.addEventListener('click', () => { chart.series_config.push({ source_param: '' }); redrawSeries(); });
    });
  }

  /** Валидация графиков и представления перед сохранением. */
  private validateChartsAndPresentation(charts: ChartConfig[], fields: PresentationField[], attrs: MethodAttribute[]): string | null {
    const attrIds = new Set(attrs.map(a => a.id));
    for (const c of charts) {
      if (c.series_config.length === 0) return `График «${c.title || c.id}»: добавьте хотя бы один ряд`;
      for (const sc of c.series_config) {
        if (!sc.source_param.trim()) return `График «${c.title || c.id}»: укажите источник для каждого ряда`;
      }
    }
    const seen = new Set<string>();
    for (const f of fields) {
      if (!attrIds.has(f.attribute_id)) continue; // устаревший атрибут — молча пропускаем
      if (seen.has(f.attribute_id)) return `Повторяющийся атрибут в представлении: ${f.attribute_id}`;
      seen.add(f.attribute_id);
    }
    return null;
  }

  /** Рисует строки атрибутов метода (блок 1) — одна строка = все свойства ОДНОГО
   * атрибута (единый tn-lims-flex, без разбивки на подблоки). Мутирует attrs
   * "на месте"; onChange — полная перерисовка списка (нужна при переключении
   * fill_method/level, которые меняют набор видимых полей строки). */
  private renderAttributeRows(
    container: HTMLElement,
    attrs: MethodAttribute[],
    determinableIndicators: string[],
    onChange: () => void,
  ): void {
    attrs.forEach((attr, idx) => {
      const row = container.createDiv({ cls: 'tn-lims-method tn-lims-flex' });
      const idInput = row.createEl('input', { attr: { type: 'text', placeholder: 'id (напр. comb_length_1)' }, cls: 'tn-lims-input' });
      idInput.value = attr.id;
      idInput.addEventListener('change', () => { attr.id = idInput.value.trim(); });
      const nameInput = row.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-lims-input' });
      nameInput.value = attr.name;
      nameInput.addEventListener('change', () => { attr.name = nameInput.value.trim(); });

      const typeSelect = row.createEl('select', { cls: 'tn-lims-select' });
      const typeOptions: Array<[AttributeDataType, string]> = [
        ['text', 'Текст'], ['int', 'Целое число'], ['float', 'Дробное число'],
        ['date', 'Дата'], ['time', 'Время'], ['photo', 'Фотография'],
      ];
      for (const [val, label] of typeOptions) typeSelect.createEl('option', { attr: { value: val }, text: label });
      typeSelect.value = attr.data_type;
      typeSelect.addEventListener('change', () => { attr.data_type = typeSelect.value as AttributeDataType; });

      const fillSelect = row.createEl('select', { cls: 'tn-lims-select' });
      const fillOptions: Array<[AttributeFillMethod, string]> = [
        ['manual', 'Ручной ввод'], ['instrument', 'Данные прибора'], ['calculated', 'Расчёт'],
      ];
      for (const [val, label] of fillOptions) fillSelect.createEl('option', { attr: { value: val }, text: label });
      fillSelect.value = attr.fill_method;
      fillSelect.addEventListener('change', () => { attr.fill_method = fillSelect.value as AttributeFillMethod; onChange(); });

      const levelSelect = row.createEl('select', { cls: 'tn-lims-select' });
      const levelOptions: Array<[AttributeLevel, string]> = [
        ['experiment', 'Данные эксперимента'], ['aggregated', 'Агрегированные результаты'],
      ];
      for (const [val, label] of levelOptions) levelSelect.createEl('option', { attr: { value: val }, text: label });
      levelSelect.value = attr.level;
      levelSelect.addEventListener('change', () => { attr.level = levelSelect.value as AttributeLevel; onChange(); });

      if (attr.fill_method === 'calculated') {
        const formulaInput = row.createEl('input', {
          attr: { type: 'text', placeholder: 'Формула (DSL, на основе атрибутов метода)' },
          cls: 'tn-lims-input',
        });
        formulaInput.value = attr.formula || '';
        formulaInput.addEventListener('change', () => { attr.formula = formulaInput.value; });
        const aiBtn = row.createEl('button', { text: '🤖 ИИ', cls: 'tn-btn tn-btn-ghost' });
        aiBtn.addEventListener('click', () => this.openAiFormulaAssist(row, attrs, determinableIndicators, formulaInput));
      } else if (attr.level === 'aggregated') {
        row.createSpan({ text: 'по:' });
        const sourceSelect = row.createEl('select', { cls: 'tn-lims-select' });
        sourceSelect.createEl('option', { attr: { value: '' }, text: '— атрибут —' });
        for (const other of attrs) {
          if (other === attr || other.level !== 'experiment' || !other.id) continue;
          sourceSelect.createEl('option', { attr: { value: other.id }, text: other.name || other.id });
        }
        sourceSelect.value = attr.aggregation?.source || '';
        const methodSelect = row.createEl('select', { cls: 'tn-lims-select' });
        const aggMethodOptions: Array<['avg' | 'min' | 'max', string]> = [
          ['avg', 'среднему'], ['min', 'минимальному'], ['max', 'максимальному'],
        ];
        for (const [val, label] of aggMethodOptions) methodSelect.createEl('option', { attr: { value: val }, text: label });
        methodSelect.value = attr.aggregation?.method || 'avg';
        const syncAggregation = () => {
          attr.aggregation = sourceSelect.value
            ? { source: sourceSelect.value, method: methodSelect.value as 'avg' | 'min' | 'max' }
            : undefined;
        };
        sourceSelect.addEventListener('change', syncAggregation);
        methodSelect.addEventListener('change', syncAggregation);
      }

      const synBtn = row.createEl('button', { text: '🔗 синонимы', cls: 'tn-btn tn-btn-ghost' });
      synBtn.addEventListener('click', () => this.toggleSynonymsPanel(row, attr));

      const delBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => { attrs.splice(idx, 1); onChange(); });
    });
  }

  /** Панель синонимов атрибута (2026-08-21): альтернативные raw-имена поля из
   * legacy email-импорта (десктопная ЛИМС) — позволяет называть атрибут как удобно
   * в конфигураторе без оглядки на то, как поле называется в старых письмах;
   * соответствие используется при приёме результатов (resolveResultKey,
   * email_ingest.go) раньше глобальных canonicalFieldNames/knownRawFields. */
  private toggleSynonymsPanel(row: HTMLElement, attr: MethodAttribute): void {
    const existing = row.querySelector('.tn-lims-synonyms');
    if (existing) { existing.remove(); return; }
    const panel = row.createDiv({ cls: 'tn-lims-synonyms tn-lims-series-form' });
    panel.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Синонимы — альтернативные имена этого поля в старых письмах/десктопной ЛИМС, через запятую (напр. flam_time):',
    );
    const input = panel.createEl('input', {
      attr: { type: 'text', placeholder: 'flam_time, old_field_name' },
      cls: 'tn-lims-input',
    });
    input.value = (attr.synonyms || []).join(', ');
    input.addEventListener('change', () => {
      const list = input.value.split(',').map(s => s.trim()).filter(Boolean);
      attr.synonyms = list.length > 0 ? list : undefined;
    });
  }

  /** Мини-панель ИИ-помощника (блок 1): описание задачи от лаборанта → предложенное
   * DSL-выражение показывается для правки/подтверждения — НИКОГДА не вставляется в
   * поле формулы автоматически, только по явному действию пользователя. Панель
   * появляется под строкой атрибута (не часть самой строки — это инструмент, не
   * настройка). */
  private openAiFormulaAssist(
    row: HTMLElement,
    attrs: MethodAttribute[],
    determinableIndicators: string[],
    formulaInput: HTMLInputElement,
  ): void {
    const existingPanel = row.querySelector('.tn-lims-ai-assist');
    if (existingPanel) { existingPanel.remove(); return; }
    const panel = row.createDiv({ cls: 'tn-lims-ai-assist tn-lims-series-form' });
    panel.createDiv({ cls: 'tn-lims-meta' }).setText('Опишите, что должна вычислять формула:');
    const descArea = panel.createEl('textarea', { cls: 'tn-lims-input' });
    descArea.rows = 2;
    const resultArea = panel.createEl('textarea', { cls: 'tn-lims-input' });
    resultArea.rows = 2;
    resultArea.disabled = true;
    resultArea.placeholder = 'Здесь появится предложенная формула';
    const btnRow = panel.createDiv({ cls: 'tn-lims-flex' });
    const askBtn = btnRow.createEl('button', { text: '🤖 Предложить формулу', cls: 'tn-btn tn-btn-primary' });
    const insertBtn = btnRow.createEl('button', { text: '↳ Вставить в формулу', cls: 'tn-btn tn-btn-ghost' });
    insertBtn.style.display = 'none';
    askBtn.addEventListener('click', async () => {
      if (!descArea.value.trim()) { new Notice('Опишите задачу для ИИ'); return; }
      askBtn.disabled = true;
      try {
        const suggestion = await this.plugin.llmAssist.suggestFormula(descArea.value.trim(), attrs, determinableIndicators);
        resultArea.value = suggestion;
        insertBtn.style.display = '';
      } catch (e: unknown) {
        new Notice(`Ошибка ИИ-помощника: ${errorMessage(e)}`);
      } finally {
        askBtn.disabled = false;
      }
    });
    insertBtn.addEventListener('click', () => {
      formulaInput.value = resultArea.value;
      panel.remove();
    });
  }

  /** Рисует строки правил классификации (блок 2) — одна строка = все свойства
   * ОДНОГО правила (единый tn-lims-flex; для «пороговых» правил список порогов —
   * переменной длины, поэтому он остаётся отдельным вложенным списком под строкой). */
  private renderClassificationRows(
    container: HTMLElement,
    rules: ClassificationRule[],
    attrs: MethodAttribute[],
    determinableIndicators: string[],
    onChange: () => void,
  ): void {
    rules.forEach((rule, idx) => {
      const card = container.createDiv({ cls: 'tn-lims-method' });
      const row = card.createDiv({ cls: 'tn-lims-flex' });
      const typeSelect = row.createEl('select', { cls: 'tn-lims-select' });
      const typeOptions: Array<[ClassificationRule['rule_type'], string]> = [
        ['threshold', 'Пороговое'], ['boolean', 'Булево'], ['compliance', 'Соответствие целевому показателю'],
      ];
      for (const [val, label] of typeOptions) typeSelect.createEl('option', { attr: { value: val }, text: label });
      typeSelect.value = rule.rule_type;
      typeSelect.addEventListener('change', () => {
        const newType = typeSelect.value as ClassificationRule['rule_type'];
        const common = { parameter_name: rule.parameter_name, output_name: rule.output_name };
        if (newType === 'threshold') rules[idx] = { ...common, rule_type: 'threshold', thresholds: [] };
        else if (newType === 'boolean') rules[idx] = { ...common, rule_type: 'boolean', operator: '==', value: '', true_grade: '', false_grade: '' };
        else rules[idx] = { ...common, rule_type: 'compliance' };
        onChange();
      });

      const paramSelect = row.createEl('select', { cls: 'tn-lims-select' });
      paramSelect.createEl('option', { attr: { value: '' }, text: '— параметр —' });
      for (const a of attrs) {
        if (!a.id) continue;
        paramSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
      }
      paramSelect.value = rule.parameter_name;
      paramSelect.addEventListener('change', () => { rule.parameter_name = paramSelect.value; });

      const outputInput = row.createEl('input', {
        attr: { type: 'text', placeholder: `результат (по умолч.: ${rule.parameter_name || '...'}_grade)` },
        cls: 'tn-lims-input',
      });
      outputInput.value = rule.output_name || '';
      outputInput.addEventListener('change', () => { rule.output_name = outputInput.value.trim() || undefined; });

      if (rule.rule_type === 'threshold') {
        this.renderThresholdFields(row, card, rule, determinableIndicators);
      } else if (rule.rule_type === 'boolean') {
        this.renderBooleanFields(row, rule, determinableIndicators);
      } else {
        this.renderComplianceFields(row, rule);
      }

      const delBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => { rules.splice(idx, 1); onChange(); });
    });
  }

  /** Пороговое правило: сортировка порогов по возрастанию и выбор первого подходящего
   * — на сервере (classifyThreshold). Агрегация — простое поле строки; сам список
   * порогов ({значение, показатель}) переменной длины — вложенный список под строкой. */
  private renderThresholdFields(
    row: HTMLElement,
    card: HTMLElement,
    rule: ThresholdClassificationRule,
    determinableIndicators: string[],
  ): void {
    row.createSpan({ text: 'при неск. сериях:' });
    const aggSelect = row.createEl('select', { cls: 'tn-lims-select' });
    const aggOptions: Array<[string, string]> = [['', 'среднее'], ['best', 'лучшее'], ['worst', 'худшее']];
    for (const [val, label] of aggOptions) aggSelect.createEl('option', { attr: { value: val }, text: label });
    aggSelect.value = rule.aggregation_rule || '';
    aggSelect.addEventListener('change', () => {
      rule.aggregation_rule = (aggSelect.value || undefined) as ThresholdClassificationRule['aggregation_rule'];
    });

    card.createDiv({ cls: 'tn-lims-meta' }).setText(`Пороги (показатели метода: ${determinableIndicators.join(', ') || '—'}):`);
    const list = card.createDiv();
    let redraw: () => void;
    redraw = () => {
      list.empty();
      rule.thresholds.forEach((t, i) => {
        const tRow = list.createDiv({ cls: 'tn-lims-flex' });
        const valueInput = tRow.createEl('input', { attr: { type: 'number', placeholder: 'значение' }, cls: 'tn-lims-input' });
        valueInput.value = String(t.value);
        valueInput.addEventListener('change', () => { t.value = Number(valueInput.value) || 0; });
        const gradeSelect = tRow.createEl('select', { cls: 'tn-lims-select' });
        gradeSelect.createEl('option', { attr: { value: '' }, text: '— показатель —' });
        for (const g of determinableIndicators) gradeSelect.createEl('option', { attr: { value: g }, text: g });
        gradeSelect.value = t.grade;
        gradeSelect.addEventListener('change', () => { t.grade = gradeSelect.value; });
        const delBtn = tRow.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
        delBtn.addEventListener('click', () => { rule.thresholds.splice(i, 1); redraw(); });
      });
    };
    redraw();
    const addBtn = card.createEl('button', { text: '➕ Порог', cls: 'tn-btn tn-btn-ghost' });
    addBtn.addEventListener('click', () => {
      rule.thresholds.push({ value: 0, grade: determinableIndicators[0] || '' });
      redraw();
    });
  }

  /** Булево правило: одно условие над значением параметра-источника — все поля
   * инлайн в общей строке правила. */
  private renderBooleanFields(
    row: HTMLElement,
    rule: BooleanClassificationRule,
    determinableIndicators: string[],
  ): void {
    const opSelect = row.createEl('select', { cls: 'tn-lims-select' });
    for (const op of ['==', '!=', '<', '<=', '>', '>=']) opSelect.createEl('option', { attr: { value: op }, text: op });
    opSelect.value = rule.operator;
    opSelect.addEventListener('change', () => { rule.operator = opSelect.value as BooleanClassificationRule['operator']; });
    const valueInput = row.createEl('input', { attr: { type: 'text', placeholder: 'значение' }, cls: 'tn-lims-input' });
    valueInput.value = String(rule.value ?? '');
    valueInput.addEventListener('change', () => { rule.value = valueInput.value; });

    row.createSpan({ text: 'если верно →' });
    const trueSelect = row.createEl('select', { cls: 'tn-lims-select' });
    trueSelect.createEl('option', { attr: { value: '' }, text: '— показатель —' });
    for (const g of determinableIndicators) trueSelect.createEl('option', { attr: { value: g }, text: g });
    trueSelect.value = rule.true_grade;
    trueSelect.addEventListener('change', () => { rule.true_grade = trueSelect.value; });

    row.createSpan({ text: 'иначе →' });
    const falseSelect = row.createEl('select', { cls: 'tn-lims-select' });
    falseSelect.createEl('option', { attr: { value: '' }, text: '— показатель —' });
    for (const g of determinableIndicators) falseSelect.createEl('option', { attr: { value: g }, text: g });
    falseSelect.value = rule.false_grade;
    falseSelect.addEventListener('change', () => { rule.false_grade = falseSelect.value; });
  }

  /** Правило соответствия целевому показателю: цель всегда «Целевой показатель»
   * заявки (objects.characteristics.target_indicators) — здесь только тексты
   * результата, инлайн в общей строке правила, с разумными подписями по умолчанию. */
  private renderComplianceFields(row: HTMLElement, rule: ComplianceClassificationRule): void {
    const complyInput = row.createEl('input', { attr: { type: 'text', placeholder: 'Соответствует' }, cls: 'tn-lims-input' });
    complyInput.value = rule.comply_text || '';
    complyInput.addEventListener('change', () => { rule.comply_text = complyInput.value.trim() || undefined; });
    const notComplyInput = row.createEl('input', { attr: { type: 'text', placeholder: 'Не соответствует' }, cls: 'tn-lims-input' });
    notComplyInput.value = rule.not_comply_text || '';
    notComplyInput.addEventListener('change', () => { rule.not_comply_text = notComplyInput.value.trim() || undefined; });
    const notAssessedInput = row.createEl('input', { attr: { type: 'text', placeholder: 'Не оценивается' }, cls: 'tn-lims-input' });
    notAssessedInput.value = rule.not_assessed_text || '';
    notAssessedInput.addEventListener('change', () => { rule.not_assessed_text = notAssessedInput.value.trim() || undefined; });
  }

  /** Валидация перед сохранением — сообщения на русском, показываются через Notice. */
  private validateAttributesAndRules(attrs: MethodAttribute[], rules: ClassificationRule[]): string | null {
    const ids = new Set<string>();
    for (const a of attrs) {
      if (!a.id.trim()) return 'У каждого атрибута должен быть заполнен id';
      // id атрибута — идентификатор в DSL-формулах (lab-service/dsl.go): только
      // латиница/цифры/подчёркивание, не с цифры — иначе формулы, ссылающиеся на
      // атрибут, не разберутся на сервере.
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(a.id)) {
        return `id атрибута «${a.id}» должен быть латиницей без пробелов (буквы/цифры/«_», не начинать с цифры)`;
      }
      if (ids.has(a.id)) return `Повторяющийся id атрибута: ${a.id}`;
      ids.add(a.id);
      if (!a.name.trim()) return `Атрибут «${a.id}»: укажите название`;
      if (a.fill_method === 'calculated' && !a.formula?.trim()) {
        return `Атрибут «${a.id}»: укажите формулу (способ заполнения — «Расчёт»)`;
      }
      if (a.fill_method !== 'calculated' && a.level === 'aggregated' && !a.aggregation) {
        return `Атрибут «${a.id}»: укажите принцип агрегирования или формулу`;
      }
    }
    for (const r of rules) {
      if (!r.parameter_name.trim()) return 'У каждого правила классификации укажите параметр-источник';
      if (r.rule_type === 'threshold' && r.thresholds.length === 0) {
        return `Правило по «${r.parameter_name}»: добавьте хотя бы один порог`;
      }
      if (r.rule_type === 'boolean' && (!r.true_grade.trim() || !r.false_grade.trim())) {
        return `Правило по «${r.parameter_name}»: укажите показатели для «верно»/«иначе»`;
      }
    }
    return null;
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
      for (const h of ['Название', 'Описание', 'Характеристики', '']) thead.createEl('th').setText(h);
      const tbody = table.createEl('tbody');
      for (const o of objects) {
        const tr = tbody.createEl('tr');
        tr.createEl('td').setText(o.name || `#${o.id}`);
        tr.createEl('td').setText(o.description || '—');
        tr.createEl('td').setText(Object.keys(o.characteristics || {}).length > 0 ? JSON.stringify(o.characteristics) : '—');
        const actions = tr.createEl('td');
        const linkBtn = actions.createEl('button', { text: '→ Заявки', cls: 'tn-btn tn-btn-ghost' });
        linkBtn.addEventListener('click', () => void this.showRequestsForObject(o.id, o.name || `#${o.id}`));
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
      presentation: m?.presentation && Array.isArray(m.presentation.fields) ? m.presentation : { fields: [] },
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