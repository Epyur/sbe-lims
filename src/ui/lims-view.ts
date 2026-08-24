import { ItemView, Modal, Notice, WorkspaceLeaf } from 'obsidian';
import type { App } from 'obsidian';
import type SbeLimsPlugin from '../main';
import type {
  AttributeDataType,
  AttributeFillMethod,
  AttributeLevel,
  ChartConfig,
  ClassificationRule,
  ComparisonOperator,
  DocumentBlock,
  Equipment,
  Inventor,
  Lab,
  LabGroup,
  LabMember,
  LabObject,
  LabProject,
  LimsRequest,
  MethodAttribute,
  MethodConfig,
  Operand,
  OperatorFormField,
  PresentationKind,
  TimeseriesSeriesConfig,
} from '../types/lims';
import { renderBlockEditor, SYSTEM_PLACEHOLDERS } from './block-editor';
import { toggleSubSupPalette } from './subsup';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import { downloadBase64File } from '../../../sbe-core/src/utils/download';
import { sanitizeAttributesWithRename } from '../services/llm-assist.service';
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

/** Роли lab_members — общий список опций для формы добавления И для inline-
 * редактирования роли (2026-08-24, по запросу пользователя: раньше смену роли
 * можно было сделать только через удаление/повторное добавление сотрудника). */
const LAB_MEMBER_ROLE_LABELS: Array<[string, string]> = [
  ['lab_operator', 'Испытатель (lab_operator)'],
  ['lab_admin', 'Администратор лабы (lab_admin)'],
  ['lab_auditor', 'Аудитор, только чтение (lab_auditor)'],
];

/** Знаки сравнения для условий правила классификации (2026-08-22). */
const COMPARISON_OPERATOR_OPTIONS: Array<[ComparisonOperator, string]> = [
  ['<', 'меньше'], ['<=', 'меньше или равно'], ['>', 'больше'], ['>=', 'больше или равно'],
  ['==', 'точно равно'], ['!=', 'точно не равно'],
];

/** Стандартные варианты вывода для правил "сравнение с целевым показателем"
 * (Operand kind="target_indicator") — 2026-08-23, по запросу пользователя:
 * результат ветки правила классификации был жёстко ограничен списком
 * показателей метода (determinableIndicators), что не давало написать вывод о
 * соответствии. Предлагаются как быстрая вставка в select рядом со свободным
 * текстовым полем (см. renderBranchRows) — не единственный допустимый вариант. */
const COMPLIANCE_VERDICTS = ['Соответствует', 'Не соответствует', 'Не оценивается'];

/** Полный номер заявки (2026-08-24, по прямому запросу пользователя — везде
 * должен фигурировать полный номер, "с кодом проекта, лаборатории, метода",
 * не сокращённый вид). `customer_number` ({projectCode}-{NNN}/{yyyy}-{labCode}-
 * {methodCode}) — единственная форма со ВСЕМИ тремя кодами; `lab_number` не
 * содержит код проекта. Короткий seq/year и #id — только fallback для ещё не
 * пронумерованной сервером заявки. Модульная функция (не метод LimsView) —
 * нужна и в ProtocolHtmlModal, отдельном классе этого файла. */
function fullRequestNumber(req: LimsRequest): string {
  return req.customer_number || req.lab_number
    || (req.number_seq > 0 ? `${req.number_seq}/${req.number_year}` : `#${req.id}`);
}

/** Kanban-доска «Очередь лаборатории»: заявка в колонке "Завершённые" видна
 * ровно 10 РАБОЧИХ дней (Пн–Пт) от completed_at, затем пропадает из канбана
 * (сама заявка/статус не трогается — она всё ещё видна на плоской странице
 * «Результаты»). Пустой completed_at — завершена до этой миграции, данных для
 * отсчёта нет, показываем всегда. Календаря праздников в проекте нигде нет —
 * не изобретаем, считаем только по дням недели. */
function withinCompletedWindow(completedAtIso: string, now: Date = new Date()): boolean {
  if (!completedAtIso) return true;
  const start = new Date(completedAtIso);
  if (Number.isNaN(start.getTime())) return true;
  const cursor = new Date(start);
  cursor.setHours(0, 0, 0, 0);
  const today = new Date(now);
  today.setHours(0, 0, 0, 0);
  let elapsed = 0;
  while (cursor.getTime() < today.getTime()) {
    cursor.setDate(cursor.getDate() + 1);
    const dow = cursor.getDay();
    if (dow !== 0 && dow !== 6) elapsed++;
    if (elapsed >= 10) return false;
  }
  return true;
}

/** Значение из текстового поля операнда «значение» — число, если строка целиком
 * читается как число, иначе как есть (текст/«Да»-«Нет»/показатель). */
function parseConditionLiteral(raw: string): string | number {
  const trimmed = raw.trim();
  if (trimmed !== '' && !Number.isNaN(Number(trimmed))) return Number(trimmed);
  return raw;
}

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
  // Открытая модалка протокола (2026-08-23) — раньше showHtmlModal вставляла
  // обычный div прямо в bodyEl без какого-либо singleton-контроля: клик на
  // "Краткий вид"/"Выписка"/"Полный протокол" несколько раз (в т.ч. по разным
  // кнопкам подряд) плодил накладывающиеся друг на друга div'ы ("плодятся много
  // экземпляров вывода", жалоба пользователя) — ни ESC, ни клик по фону их не
  // закрывали, т.к. это не настоящий Modal. Теперь настоящий Modal с закрытием
  // предыдущего перед открытием нового.
  private openProtocolModal: ProtocolHtmlModal | null = null;
  private collapsed = false;
  private labs: Lab[] = [];
  /** Кэш объектов/проектов/групп — только для отображения деталей заявки
   * (как у заявителя в sbe-requests), не для редактирования. */
  private objects: LabObject[] = [];
  private projects: LabProject[] = [];
  private groups: LabGroup[] = [];
  private myRole = '';
  private myEmail = '';
  private currentRequestsFilter?: (r: LimsRequest) => boolean;
  /** Kanban-доска «Очередь лаборатории»: заявка, которую в данный момент тащат
   * (см. renderQueueBoard) — module-scope не подходит, т.к. вьюх может быть
   * несколько, а обработчики drop читают именно СВОЙ инстанс. */
  private draggedCard: LimsRequest | null = null;

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
      this.myEmail = perm.email;
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
      this.myEmail = '';
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
      this.myEmail = perm.email;
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
        await this.renderQueueBoard();
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
      for (const r of requests) {
        const card = this.bodyEl.createDiv({ cls: 'tn-lims-req-card' });
        card.addEventListener('click', () => this.renderRequestDetail(r));
        const head = card.createDiv({ cls: 'tn-lims-req-card-head' });
        // Списки заявок ("Все заявки"/"Очередь лаборатории", 2026-08-24, по
        // прямому запросу пользователя — частичный откат) — единственное
        // место, где показывается короткий "лабораторный" номер (lab_number,
        // без кода проекта); везде остальном (деталь заявки, протокол,
        // имя файла) — полный fullRequestNumber (customer_number).
        head.createEl('h4', { text: `№ ${r.lab_number || fullRequestNumber(r)}` });
        head.createSpan({ cls: 'tn-lims-req-card-status', text: STATUS_LABELS[r.status] || r.status });
        card.createDiv({ cls: 'tn-lims-req-card-title', text: r.title || '(без названия)' });
        const meta = card.createDiv({ cls: 'tn-lims-req-card-meta' });
        meta.createSpan({ text: `🔬 ${this.objectName(r.object_id)}` });
        meta.createSpan({ text: `🧪 ${this.methodName(r.method_id)}` });
      }
      if (requests.length === 0) {
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Нет заявок.');
      }
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  // ---- Kanban-доска «Очередь лаборатории» (2026-08-24) ----
  //
  // 4 колонки: new/received/processing/completed (маппинг подтверждён — подпись
  // processing "🟡 В работе" буквально равна названию колонки 3). Колонки 2/3
  // делятся на ячейки по испытателям лабы (lab_members.role IN ('lab_operator',
  // 'lab_admin')); руководитель (canAdmin ИЛИ lab_admin именно этой лабы,
  // 2026-08-24 — делегированные полномочия) двигает карточки свободно, испытатель
  // (lab_operator) — только свои уже назначенные (между своими ячейками 2⇄3,
  // дальше в 4), либо забирает СЕБЕ неназначенную заявку прямо из колонки 1.
  // Авторизация реально проверяется на сервере (kanban.go, canApplyKanbanMove) —
  // клиентские предикаты ниже только скрывают то, что заведомо будет отклонено,
  // не единственная защита.

  /** Ростер лабы заявки (для деления колонок 2/3 на ячейки и для пикера в детали).
   * Внешняя лаба не имеет своих lab_members — резолвим в parent_lab_id, как и
   * остальной код в этом файле (см. renderSettingsLabRow). */
  private async fetchLabRoster(labId: number): Promise<LabMember[]> {
    const lab = this.labs.find(l => l.id === labId);
    const resolvedId = lab?.parent_lab_id || labId;
    try {
      return await this.plugin.syncService.listLabMembers(resolvedId);
    } catch (e: unknown) {
      return []; // не участник этой лабы (403 от сервера) — контролы просто скрываются
    }
  }

  private testersOf(roster: LabMember[]): LabMember[] {
    return roster.filter(m => m.role === 'lab_operator' || m.role === 'lab_admin');
  }

  private myRoleIn(roster: LabMember[]): string {
    return roster.find(m => m.email === this.myEmail)?.role || '';
  }

  /** Лабы, где текущий пользователь — lab_admin (делегированные полномочия,
   * 2026-08-24) — нужно для гейтинга «Методов» (метод может принадлежать
   * нескольким лабам, достаточно администрировать одну из них). Не вызывать
   * для глобального admin+ — там всё разрешено без этого списка. */
  private async myAdminLabIds(): Promise<Set<number>> {
    const ids = new Set<number>();
    await Promise.all(this.labs.map(async (lab) => {
      const roster = await this.fetchLabRoster(lab.id);
      if (this.myRoleIn(roster) === 'lab_admin') ids.add(lab.id);
    }));
    return ids;
  }

  private async renderQueueBoard(): Promise<void> {
    this.bodyEl.empty();
    if (!this.labId) {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Выберите лабораторию.');
      return;
    }
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const roster = await this.fetchLabRoster(this.labId);
      const testers = this.testersOf(roster);
      const myLabRole = this.myRoleIn(roster);
      // lab_admin ИМЕННО этой лабы — тоже руководитель (2026-08-24, делегированные
      // полномочия), не только глобальный admin/superadmin.
      const isLabHead = this.canAdmin || myLabRole === 'lab_admin';
      const all = await this.plugin.syncService.listRequests();
      const requests = all.filter(r => r.lab_id === this.labId);
      this.bodyEl.empty();

      const board = this.bodyEl.createDiv({ cls: 'tn-lims-kanban' });
      this.renderFlatKanbanColumn(board, 'Новые заявки', requests.filter(r => r.status === 'new'), 'new',
        () => isLabHead || myLabRole !== '', // можно тащить: руководитель, либо любой испытатель (самозабор — цель проверяет ячейка колонки 2)
        () => isLabHead,                     // дропнуть ОБРАТНО в "новые" (снять назначение) — только руководитель
        () => '');
      this.renderPerTesterKanbanColumn(board, 'В работу', requests.filter(r => r.status === 'received'),
        'received', testers, isLabHead, myLabRole);
      this.renderPerTesterKanbanColumn(board, 'В работе', requests.filter(r => r.status === 'processing'),
        'processing', testers, isLabHead, myLabRole);
      const completed = requests.filter(r => r.status === 'completed' && withinCompletedWindow(r.completed_at));
      this.renderFlatKanbanColumn(board, 'Завершённые', completed, 'completed',
        () => isLabHead, // из завершённых обратно тащит только руководитель (переоткрытие)
        (dragged) => isLabHead || (myLabRole !== '' && dragged.assigned_to === this.myEmail), // завершить может руководитель или тот испытатель, кому назначена карточка
        () => undefined); // исполнителя при завершении не трогаем
    } catch (e: unknown) {
      this.bodyEl.empty();
      this.bodyEl.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка: ${errorMessage(e)}`);
    }
  }

  /** Колонка без деления на ячейки (1 «Новые» и 4 «Завершённые»).
   * @param canDrag — можно ли тащить конкретную карточку ИЗ этой колонки.
   * @param canDrop — можно ли дропнуть ТЕКУЩУЮ перетаскиваемую карточку В эту колонку.
   * @param assignedToOnDrop — значение assigned_to, которое нужно проставить при
   *   дропе (`undefined` — не трогать, как для «Завершённые», где исполнитель
   *   сохраняется для истории). */
  private renderFlatKanbanColumn(
    board: HTMLElement, title: string, requests: LimsRequest[], targetStatus: string,
    canDrag: (r: LimsRequest) => boolean,
    canDrop: (dragged: LimsRequest) => boolean,
    assignedToOnDrop: (dragged: LimsRequest) => string | undefined,
  ): void {
    const col = board.createDiv({ cls: 'tn-lims-kanban-col' });
    const head = col.createDiv({ cls: 'tn-lims-kanban-col-head' });
    head.createSpan({ text: title });
    head.createSpan({ cls: 'tn-lims-kanban-col-count', text: String(requests.length) });
    const list = col.createDiv();
    for (const r of requests) this.renderKanbanCard(list, r, canDrag(r));
    this.makeDropZone(col, () => {
      const dragged = this.draggedCard;
      if (!dragged || !canDrop(dragged)) return;
      void this.performKanbanMove(dragged, targetStatus, assignedToOnDrop(dragged)).then(() => this.renderQueueBoard());
    });
  }

  /** Колонки 2/3 — одна ячейка на испытателя + хвостовая ячейка «Не назначено»
   * (видна/принимает дроп только руководителю — испытатель сам никого не
   * назначает и не видит смысла в этой ячейке). */
  private renderPerTesterKanbanColumn(
    board: HTMLElement, title: string, requests: LimsRequest[], targetStatus: string,
    testers: LabMember[], isLabHead: boolean, myLabRole: string,
  ): void {
    const col = board.createDiv({ cls: 'tn-lims-kanban-col' });
    const head = col.createDiv({ cls: 'tn-lims-kanban-col-head' });
    head.createSpan({ text: title });
    head.createSpan({ cls: 'tn-lims-kanban-col-count', text: String(requests.length) });

    const canDropCell = (testerEmail: string): boolean =>
      isLabHead || (myLabRole !== '' && testerEmail === this.myEmail);
    const canDrag = (r: LimsRequest): boolean =>
      isLabHead || (myLabRole !== '' && r.assigned_to === this.myEmail);

    const renderCell = (label: string, testerEmail: string): void => {
      const cell = col.createDiv({ cls: 'tn-lims-kanban-cell' });
      cell.createDiv({ cls: 'tn-lims-kanban-cell-head', text: label });
      for (const r of requests.filter(r => r.assigned_to === testerEmail)) {
        this.renderKanbanCard(cell, r, canDrag(r));
      }
      if (canDropCell(testerEmail)) {
        this.makeDropZone(cell, () => {
          const dragged = this.draggedCard;
          if (!dragged) return;
          void this.performKanbanMove(dragged, targetStatus, testerEmail).then(() => this.renderQueueBoard());
        });
      }
    };
    for (const tester of testers) renderCell(tester.email, tester.email);
    const unassigned = requests.filter(r => r.assigned_to === '' || !testers.some(t => t.email === r.assigned_to));
    if (isLabHead || unassigned.length > 0) renderCell('Не назначено', '');
  }

  private renderKanbanCard(container: HTMLElement, req: LimsRequest, draggable: boolean): void {
    const card = container.createDiv({ cls: 'tn-lims-kanban-card' });
    if (draggable) {
      card.setAttribute('draggable', 'true');
      card.addEventListener('dragstart', (ev) => { this.draggedCard = req; ev.stopPropagation(); });
      card.addEventListener('dragend', () => { this.draggedCard = null; });
    }
    card.createDiv({ text: `№ ${req.lab_number || fullRequestNumber(req)}` });
    card.createDiv({ cls: 'tn-lims-meta', text: req.title || '(без названия)' });
    card.createDiv({ cls: 'tn-lims-meta', text: this.methodName(req.method_id) });
    card.addEventListener('click', () => void this.renderRequestDetail(req));
  }

  private makeDropZone(container: HTMLElement, onDrop: () => void): void {
    container.addEventListener('dragover', (ev) => { ev.preventDefault(); container.classList.add('tn-lims-kanban-dropzone-over'); });
    container.addEventListener('dragleave', () => container.classList.remove('tn-lims-kanban-dropzone-over'));
    container.addEventListener('drop', (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      container.classList.remove('tn-lims-kanban-dropzone-over');
      onDrop();
    });
  }

  /** Выполняет смену статуса/назначения на сервере (см. kanban.go, handleKanbanMove) —
   * используется и перетаскиванием на доске (renderQueueBoard, дальше сама
   * перерисовывает доску), и контролами в детали заявки (renderRequestDetail,
   * сама перерисовывает деталь свежими данными) — поэтому НЕ решает здесь, что
   * перерисовывать, а просто возвращает обновлённую заявку (`null` — если патч
   * пуст или запрос отклонён/упал). */
  private async performKanbanMove(card: LimsRequest, targetStatus: string, targetAssignedTo?: string): Promise<LimsRequest | null> {
    const patch: { status?: string; assigned_to?: string } = {};
    if (targetStatus !== card.status) patch.status = targetStatus;
    if (targetAssignedTo !== undefined && targetAssignedTo !== card.assigned_to) patch.assigned_to = targetAssignedTo;
    if (Object.keys(patch).length === 0) return null;
    try {
      const updated = await this.plugin.syncService.moveKanbanCard(card.id, patch);
      new Notice('Карточка перемещена');
      return updated;
    } catch (e: unknown) {
      new Notice(`Ошибка: ${errorMessage(e)}`);
      return null;
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

    this.bodyEl.createEl('h3', { text: `№ ${fullRequestNumber(req)} — ${req.title || 'без названия'}` });

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
    meta.createDiv({ text: `👷 Исполнитель: ${req.assigned_to || 'не назначен'}` });

    if (req.description) {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-mb12' }).createDiv({ text: `📝 ${req.description}` });
    }

    // статус/назначение (Kanban-доска «Очередь лаборатории», 2026-08-24) — то же
    // правило, что при перетаскивании карточки (renderQueueBoard/kanban.go):
    // руководитель — свободно; испытатель — только свою уже назначенную заявку,
    // либо самозабор неназначенной "новой" себе.
    const roster = req.lab_id > 0 ? await this.fetchLabRoster(req.lab_id) : [];
    const myLabRole = this.myRoleIn(roster);
    // lab_admin ИМЕННО этой лабы — тоже руководитель (2026-08-24, делегированные полномочия).
    if (this.canAdmin || myLabRole === 'lab_admin') {
      const statusSelect = this.bodyEl.createEl('select', { cls: 'tn-lims-select tn-lims-mb8' });
      for (const [v, l] of Object.entries(STATUS_LABELS)) statusSelect.createEl('option', { value: v, text: l });
      statusSelect.value = req.status;
      statusSelect.addEventListener('change', () => {
        void this.performKanbanMove(req, statusSelect.value).then(updated => {
          if (updated) void this.renderRequestDetail(updated);
        });
      });

      const testerSelect = this.bodyEl.createEl('select', { cls: 'tn-lims-select tn-lims-mb12' });
      testerSelect.createEl('option', { value: '', text: 'Не назначено' });
      for (const t of this.testersOf(roster)) testerSelect.createEl('option', { value: t.email, text: t.email });
      testerSelect.value = req.assigned_to;
      testerSelect.addEventListener('change', () => {
        void this.performKanbanMove(req, req.status, testerSelect.value).then(updated => {
          if (updated) void this.renderRequestDetail(updated);
        });
      });
    } else if (myLabRole !== '' && req.status === 'new' && req.assigned_to === '') {
      const pickupBtn = this.bodyEl.createEl('button', { text: '📥 Взять в работу', cls: 'tn-btn tn-lims-mb12' });
      pickupBtn.addEventListener('click', () => {
        void this.performKanbanMove(req, 'received', this.myEmail).then(updated => {
          if (updated) void this.renderRequestDetail(updated);
        });
      });
    } else if (myLabRole !== '' && req.assigned_to === this.myEmail && req.status !== 'new' && req.status !== 'completed') {
      const statusSelect = this.bodyEl.createEl('select', { cls: 'tn-lims-select tn-lims-mb8' });
      for (const v of ['received', 'processing', 'completed']) {
        statusSelect.createEl('option', { value: v, text: STATUS_LABELS[v] });
      }
      statusSelect.value = req.status;
      statusSelect.addEventListener('change', () => {
        void this.performKanbanMove(req, statusSelect.value).then(updated => {
          if (updated) void this.renderRequestDetail(updated);
        });
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

    // результаты — краткий вид (блоки метода, вид "ui"), тот же эндпоинт, что
    // и остальные виды вывода, но format=html — не тратим время на DOCX только
    // для предпросмотра в карточке заявки.
    const shortViewDiv = methodDiv.createDiv();
    await this.renderShortView(shortViewDiv, req.id);

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

    // протокол / выписка / краткий вид — ровно 3 фиксированных вида вывода
    // (2026-08-22, по решению пользователя), каждый — HTML+DOCX тем же эндпоинтом
    // с разным ?template=.
    const protoDiv = this.bodyEl.createDiv({ cls: 'tn-lims-method' });
    protoDiv.createEl('h4', { text: '📄 Документы' });
    const protoRow = protoDiv.createDiv({ cls: 'tn-lims-flex' });
    const kinds: Array<{ kind: PresentationKind; label: string }> = [
      { kind: 'ui', label: 'Краткий вид' },
      { kind: 'excerpt', label: 'Выписка из протокола' },
      { kind: 'protocol', label: 'Полный протокол' },
    ];
    for (const { kind, label } of kinds) {
      const btn = protoRow.createEl('button', { text: label, cls: 'tn-btn tn-btn-primary' });
      btn.addEventListener('click', async () => {
        try {
          const proto = await this.plugin.syncService.getProtocol(req.id, kind);
          this.showHtmlModal(req, proto.html, () =>
            this.downloadDocx(proto.docx_base64, `${kind}_${fullRequestNumber(req).replace(/\//g, '-')}.docx`));
        } catch (e: unknown) {
          new Notice(`Ошибка: ${errorMessage(e)}`);
        }
      });
    }
  }

  /** Краткий вид результатов — блоки метода с show_in_ui, отрисованные
   * сервером (protocol.go, kind="ui", format=html — без сборки DOCX). HTML
   * приходит готовым (плейсхолдеры уже резолвлены), клиент просто вставляет
   * его в контейнер — не дублирует рендер форматированного текста на TS. */
  private async renderShortView(container: HTMLElement, requestId: number): Promise<void> {
    try {
      const proto = await this.plugin.syncService.getProtocol(requestId, 'ui', 'html');
      // proto.html — целый документ (свой <style> с глобальными селекторами
      // напр. body{...}) — вставлять его напрямую в живой DOM плагина нельзя
      // (протёк бы на весь Obsidian); берём только содержимое <body>.
      const doc = new DOMParser().parseFromString(proto.html, 'text/html');
      container.empty();
      // .tn-protocol-html (2026-08-24, sbe-core) — серверный <style> сюда не
      // доходит (см. комментарий выше), зеркалим границы/центрирование таблиц
      // отдельным общим классом, чтобы «Краткий вид» выглядел как в модалке.
      container.addClass('tn-protocol-html');
      container.innerHTML = doc.body.innerHTML;
    } catch (e: unknown) {
      container.createDiv({ cls: 'tn-lims-error' }).setText(`Ошибка загрузки результатов: ${errorMessage(e)}`);
    }
  }

  // ---- Справочники ----

  /** Методы: список из кэша (pull); создание + JSON-редактор конфигов + удаление —
   * глобальный admin+, либо lab_admin хотя бы ОДНОЙ из лаб метода (2026-08-24,
   * делегированные полномочия — метод может принадлежать нескольким лабам, для
   * своих lab_admin получает полный доступ, для чужих — только просмотр). */
  private async renderMethods(): Promise<void> {
    this.bodyEl.empty();
    const adminLabIds = this.canAdmin ? null : await this.myAdminLabIds();
    const isAdminOf = (labIds: number[]): boolean =>
      this.canAdmin || (adminLabIds !== null && labIds.some(id => adminLabIds.has(id)));
    if (this.canAdmin || (adminLabIds !== null && adminLabIds.size > 0)) {
      this.renderMethodCreateForm(adminLabIds);
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
      if (isAdminOf(m.lab_ids)) {
        const editBtn = head.createEl('button', { text: '✎ Конфиг', cls: 'tn-btn tn-btn-ghost' });
        editBtn.addEventListener('click', () => new MethodConfigModal(this, m.id).open());
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
  private renderLabCheckboxes(container: HTMLElement, selected: number[], labs: Lab[] = this.labs): () => number[] {
    const boxes: Array<{ id: number; el: HTMLInputElement }> = [];
    for (const lab of labs) {
      const row = container.createDiv({ cls: 'tn-lims-flex' });
      const cb = row.createEl('input', { attr: { type: 'checkbox' } });
      cb.checked = selected.includes(lab.id);
      row.createSpan({ text: lab.name || lab.code });
      boxes.push({ id: lab.id, el: cb });
    }
    return () => boxes.filter(b => b.el.checked).map(b => b.id);
  }

  /** Форма создания метода — глобальный admin+, либо lab_admin (список лабораторий
   * ограничен своими, 2026-08-24 — не даёт даже попытаться привязать метод к чужой
   * лабе, хотя сервер и так это отклонит). Лаборатории — чекбоксы (метод может
   * принадлежать нескольким), из уже загруженного списка лабораторий. */
  private renderMethodCreateForm(adminLabIds: Set<number> | null): void {
    const form = this.bodyEl.createDiv({ cls: 'tn-lims-series-form' });
    const row = form.createDiv({ cls: 'tn-lims-flex' });
    const code = row.createEl('input', { attr: { type: 'text', placeholder: 'Код метода' }, cls: 'tn-lims-input' });
    const name = row.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-lims-input' });
    const description = row.createEl('input', { attr: { type: 'text', placeholder: 'Описание' }, cls: 'tn-lims-input' });
    const indicators = row.createEl('input', {
      attr: { type: 'text', placeholder: 'Показатели по убыванию, напр. Г1, Г2, Г3, Г4' },
      cls: 'tn-lims-input',
    });
    form.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Порядок ввода показателей задаёт ранг: первый считается «выше»/«больше» остальных ' +
      '(Г1, Г2, Г3, Г4 → Г1 > Г2 > Г3 > Г4) — используется в правилах классификации и формулах min_grade/max_grade.',
    );
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Лаборатории (метод может принадлежать нескольким):');
    const availableLabs = adminLabIds === null ? this.labs : this.labs.filter(l => adminLabIds.has(l.id));
    const getLabIDs = this.renderLabCheckboxes(form, [], availableLabs);
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
   * здесь только просмотр, редактирование напрямую не имеет смысла.
   *
   * Возвращает {isDirty, save} — используется MethodConfigModal.close() для
   * защиты от потери несохранённых правок при случайном закрытии окна. */
  renderMethodConfigForm(container: HTMLElement, methodId: number): MethodConfigHandle {
    const cfg = this.methodConfigOf(methodId);
    const method = this.plugin.methods.find(m => m.id === methodId);
    const determinableIndicators: string[] = [...(method?.determinable_indicators || [])];
    // Форвард-объявления: редакторы атрибутов/правил читают determinableIndicators
    // для списков-опций показателя — при редактировании показателей (см. ниже)
    // их нужно перерисовать вместе со списком показателей.
    // redrawBlocks/redrawOperatorForm форвард-объявлены здесь по той же причине,
    // что и redrawAttrs/redrawRules — их сборки предикатов "какой атрибут можно
    // выбрать" построены на момент рендера, а не лениво; без пересборки после
    // любого изменения attrs новый/переименованный атрибут был бы недоступен в
    // правилах классификации/представлении/форме испытателя до закрытия и
    // повторного открытия конфигуратора (прямая жалоба пользователя, 2026-08-23).
    let redrawAttrs: () => void = () => {};
    let redrawRules: () => void = () => {};
    let redrawBlocks: () => void = () => {};
    let redrawOperatorForm: () => void = () => {};
    const form = container.createDiv({ cls: 'tn-lims-series-form' });

    form.createDiv({ cls: 'tn-lims-meta' }).setText('Описание:');
    const description = form.createEl('input', { attr: { type: 'text', placeholder: 'Описание' }, cls: 'tn-lims-input' });
    description.value = method?.description || '';
    form.createDiv({ cls: 'tn-lims-meta' }).setText('Лаборатории (метод может принадлежать нескольким):');
    const getLabIDs = this.renderLabCheckboxes(form, method?.lab_ids || []);

    // ---- Показатели метода (determinable_indicators) — порядок = ранг ----
    form.createEl('h4', { text: 'Показатели метода' });
    form.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Порядок задаёт ранг: первый показатель считается «выше»/«больше» остальных ' +
      '(напр. Г1, Г2, Г3, Г4 → Г1 > Г2 > Г3 > Г4) — используется в правилах ' +
      'классификации и в min_grade/max_grade. Если ошиблись при создании метода — ' +
      'поправьте порядок здесь стрелками.',
    );
    const indicatorsListEl = form.createDiv();
    let redrawIndicators: () => void;
    redrawIndicators = () => {
      indicatorsListEl.empty();
      determinableIndicators.forEach((val, i) => {
        const row = indicatorsListEl.createDiv({ cls: 'tn-lims-flex' });
        const upBtn = row.createEl('button', { text: '▲', cls: 'tn-btn tn-btn-ghost' });
        upBtn.disabled = i === 0;
        upBtn.addEventListener('click', () => {
          [determinableIndicators[i - 1], determinableIndicators[i]] = [determinableIndicators[i], determinableIndicators[i - 1]];
          redrawIndicators(); redrawAttrs(); redrawRules();
        });
        const downBtn = row.createEl('button', { text: '▼', cls: 'tn-btn tn-btn-ghost' });
        downBtn.disabled = i === determinableIndicators.length - 1;
        downBtn.addEventListener('click', () => {
          [determinableIndicators[i], determinableIndicators[i + 1]] = [determinableIndicators[i + 1], determinableIndicators[i]];
          redrawIndicators(); redrawAttrs(); redrawRules();
        });
        const valInput = row.createEl('input', { attr: { type: 'text', placeholder: 'показатель' }, cls: 'tn-lims-input' });
        valInput.value = val;
        valInput.addEventListener('change', () => {
          determinableIndicators[i] = valInput.value.trim();
          redrawAttrs(); redrawRules();
        });
        const delBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
        delBtn.addEventListener('click', () => {
          determinableIndicators.splice(i, 1);
          redrawIndicators(); redrawAttrs(); redrawRules();
        });
      });
    };
    redrawIndicators();
    const addIndicatorBtn = form.createEl('button', { text: '➕ Показатель', cls: 'tn-btn tn-btn-ghost' });
    addIndicatorBtn.addEventListener('click', () => {
      determinableIndicators.push('');
      redrawIndicators();
    });

    // ---- Импорт из стандарта (ИИ-черновик атрибутов/правил/представления) ----
    // ---- Загрузка атрибутов из JSON (без ИИ — готовый список) ----
    // DOM-элементы создаются здесь (чтобы блоки были визуально первыми), обработчики
    // click навешиваются ниже — после того, как attrs/rules/presentationFields и
    // их redraw-замыкания уже объявлены (нужны обработчикам).
    form.createEl('h4', { text: '📄 Импортировать из стандарта (ИИ)' });
    const importRow = form.createDiv({ cls: 'tn-lims-flex' });
    const importFileInput = importRow.createEl('input', { attr: { type: 'file', accept: '.rtf,.txt' } });
    const importBtn = importRow.createEl('button', { text: '🤖 Сформировать проект', cls: 'tn-btn tn-btn-ghost' });

    form.createEl('h4', { text: '📋 Загрузить атрибуты из JSON' });
    const jsonImportArea = form.createEl('textarea', {
      attr: { placeholder: 'Вставьте JSON-массив атрибутов (или объект с полем attributes/input_parameters)' },
      cls: 'tn-lims-input',
    });
    jsonImportArea.rows = 4;
    const jsonImportBtn = form.createEl('button', { text: '⬆ Загрузить', cls: 'tn-btn tn-btn-ghost' });

    // ---- Блок 1: атрибуты метода ----
    const attrs: MethodAttribute[] = cfg.input_parameters.map(a => ({ ...a }));
    form.createEl('h4', { text: 'Атрибуты метода' });
    // Справка "Системные атрибуты" (2026-08-23) — по прямому решению пользователя:
    // испытатель/даты/условия среды общие для ЛЮБОГО метода, заводить их отдельным
    // атрибутом на каждом методе не нужно — заполняются автоматически (email-импорт
    // результатов) и доступны как плейсхолдер в блочном редакторе представления И
    // автоматически показываются в форме для испытателя (см. renderOperatorFormPreview).
    // Один каталог на три поверхности — SYSTEM_PLACEHOLDERS (block-editor.ts).
    const systemHint = form.createDiv({ cls: 'tn-lims-meta tn-lims-mb8' });
    systemHint.createSpan({
      text: 'Системные данные (партия, материал, испытатель, даты, условия среды и т.п.) ' +
        'подставляются автоматически — не создавайте для них отдельный атрибут метода: ',
    });
    systemHint.createSpan({ text: SYSTEM_PLACEHOLDERS.map(s => s.label).join(', ') + '.' });
    const attrsListEl = form.createDiv();
    // onChange у строки атрибута дополнительно обновляет правила/представление/
    // форму испытателя — их выпадающие списки атрибутов собираются при рендере,
    // не лениво, поэтому переименование/добавление/удаление атрибута иначе не
    // видно там до перезакрытия конфигуратора.
    const onAttrsChanged = (): void => { redrawAttrs(); redrawRules(); redrawBlocks(); redrawOperatorForm(); };
    redrawAttrs = () => {
      attrsListEl.empty();
      this.renderAttributeRows(attrsListEl, attrs, methodId, determinableIndicators, onAttrsChanged);
    };
    redrawAttrs();
    const addAttrBtn = form.createEl('button', { text: '➕ Добавить атрибут', cls: 'tn-btn tn-btn-ghost' });
    addAttrBtn.addEventListener('click', () => {
      attrs.push({ id: '', name: '', data_type: 'text', fill_method: 'manual', level: 'experiment' });
      onAttrsChanged();
    });

    // ---- Блок 2: правила классификации ----
    // branches/subjects/grade/clauses/compare_to могут отсутствовать или быть
    // некорректными у правил, сохранённых до текущей модели (v1/v2 этой же
    // сессии) — без этой нормализации renderBranchRows/renderSubjectRows падают
    // на .forEach/.trim() у undefined и всё окно конфигуратора не открывается
    // ни для одного метода (см. баг у метода "ГВ", 2026-08-23).
    const rules: ClassificationRule[] = cfg.classification.map(r => ({
      ...r,
      branches: (r.branches || []).map(b => ({
        ...b,
        grade: b.grade || '',
        clauses: (b.clauses || []).map(c => ({
          ...c,
          compare_to: c.compare_to && c.compare_to.kind ? c.compare_to : { kind: 'literal', value: '' },
        })),
      })),
      subjects: r.subjects || [],
    }));
    form.createEl('h4', { text: 'Правила классификации' });
    const rulesListEl = form.createDiv();
    redrawRules = () => {
      rulesListEl.empty();
      this.renderClassificationRows(rulesListEl, rules, attrs, determinableIndicators, () => redrawRules());
    };
    redrawRules();
    const addRuleBtn = form.createEl('button', { text: '➕ Добавить правило', cls: 'tn-btn tn-btn-ghost' });
    addRuleBtn.addEventListener('click', () => {
      rules.push({ branches: [], subjects: [] });
      redrawRules();
    });

    // formulas — вычисляется сервером, только просмотр
    form.createDiv({ cls: 'tn-lims-meta' }).setText('formulas (вычисляется сервером из атрибутов — только просмотр):');
    const formulasPreview = form.createEl('pre', { cls: 'tn-lims-input' });
    formulasPreview.setText(JSON.stringify(cfg.formulas, null, 2));

    // ---- Блок 3а: графики (chart_configs) — структурный редактор. Идёт ПЕРЕД
    // представлением (2026-08-22), т.к. секции представления теперь могут
    // ссылаться на уже настроенные здесь графики. ----
    // Нормализация на случай конфигов, сохранённых старым raw-JSON textarea
    // (до блока 3) без части полей — иначе редактор упал бы на .map(undefined).
    const charts: ChartConfig[] = cfg.chart_configs.map((c, i) => ({
      id: c.id || `chart_${i}`,
      title: c.title,
      chart_type: c.chart_type === 'scatter' || c.chart_type === 'bar' ? c.chart_type : 'line',
      // kind/timeseries_series/y2_label (2026-08-24) — до этого фикса редактор их не
      // знал и терял при пересохранении (см. AGENTS.md "график по датчику" — реальный
      // инцидент, ДВАЖДЫ: сперва с source_param/channels, потом при переходе на
      // timeseries_series — пока фронтенд не обновлён вслед за форматом на сервере,
      // любое открытие+сохранение конфигуратора стирает поля, которых типы/редактор
      // ещё не знают). Сохраняем при чтении, чтобы дальше не терять.
      kind: c.kind === 'timeseries' ? 'timeseries' : undefined,
      x_column: c.x_column,
      x_label: c.x_label,
      y_label: c.y_label,
      y2_label: c.y2_label,
      series_config: Array.isArray(c.series_config) ? c.series_config.map(s => ({ ...s })) : [],
      timeseries_series: Array.isArray(c.timeseries_series)
        ? c.timeseries_series
          .filter((s): s is TimeseriesSeriesConfig => !!s && typeof s.source_param === 'string' && typeof s.channel === 'string')
          .map(s => ({ ...s }))
        : undefined,
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

    // ---- Блок 3б: представление данных — блоки форматированного текста
    // (2026-08-23, визуальный редактор с плейсхолдерами — заменил секции
    // полей от 2026-08-22, отвергнутые пользователем как неподходящая модель
    // для документа с реквизитами/описаниями/юридическим футером). Ровно 3
    // вида вывода (Кратко/Выписка/Протокол) — по решению пользователя простые
    // галочки на каждом блоке, без отдельного списка шаблонов. ----
    const blocks: DocumentBlock[] = cfg.presentation.blocks.map(b => ({ ...b, content: b.content.map(n => ({ ...n })) }));
    form.createEl('h4', { text: 'Представление данных (короткий вид / выписка / протокол)' });
    form.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Блоки — форматированный текст с плейсхолдерами (системными — партия/материал/ЕКН и ' +
      'т.п., атрибутами метода). Динамические данные (несколько серий) — только в таблице; ' +
      'вне таблицы атрибут эксперимента сворачивается до одного значения (среднее/мин/макс/ ' +
      'первая/последняя серия). Три галочки на каждом блоке — в каком из трёх видов вывода он показывается.',
    );
    const blocksListEl = form.createDiv();
    redrawBlocks = () => {
      blocksListEl.empty();
      this.renderBlocksList(blocksListEl, blocks, attrs, charts);
    };
    redrawBlocks();
    const addBlockBtn = form.createEl('button', { text: '➕ Добавить блок', cls: 'tn-btn tn-btn-ghost' });
    addBlockBtn.addEventListener('click', () => {
      blocks.push({ id: `blk_${Date.now()}`, title: 'Новый блок', content: [], show_in_ui: true, show_in_excerpt: false, show_in_protocol: true });
      redrawBlocks();
    });

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
        const existingAttributes = this.collectExistingAttributeSummaries(methodId, []);
        const draft = await this.plugin.llmAssist.draftAttributesAndClassification(standardText, existingAttributes);
        attrs.splice(0, attrs.length, ...(Array.isArray(draft.attributes) ? draft.attributes : []));
        rules.splice(0, rules.length, ...(Array.isArray(draft.classification) ? draft.classification : []));
        if (attrs.length === 0) {
          new Notice('ИИ не смог сформировать ни одного атрибута из этого файла — проверьте, что это текстовый .rtf/.txt со стандартом, и что модель LLM настроена в настройках плагина');
          return;
        }
        // Представление (блоки форматированного текста) ИИ не черновит —
        // пользователь строит блоки протокола вручную в редакторе ниже.
        onAttrsChanged();
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

    jsonImportBtn.addEventListener('click', () => {
      const text = jsonImportArea.value.trim();
      if (!text) { new Notice('Вставьте JSON с атрибутами в поле выше'); return; }
      if (attrs.length > 0 && !window.confirm(
        'Заменить текущие атрибуты вставленным JSON? Ничего не сохранится, пока вы не нажмёте «Сохранить».',
      )) {
        return;
      }
      try {
        const parsed = JSON.parse(text) as unknown;
        const rawList = this.extractAttributeArray(parsed);
        if (!rawList) {
          new Notice('Не нашёл массив атрибутов (ожидается JSON-массив или объект с полем attributes/input_parameters)');
          return;
        }
        const { attributes } = sanitizeAttributesWithRename(rawList);
        if (attributes.length === 0) {
          new Notice('Не нашлось ни одного распознанного атрибута (у каждого нужно как минимум "name")');
          return;
        }
        attrs.splice(0, attrs.length, ...attributes);
        onAttrsChanged();
        jsonImportArea.value = '';
        new Notice(`Загружено атрибутов: ${attributes.length} — проверьте перед сохранением`);
      } catch (e: unknown) {
        console.error('ЛИМС: ошибка загрузки атрибутов из JSON:', e);
        new Notice(`Ошибка загрузки JSON: ${errorMessage(e)}`);
      }
    });

    // ---- Блок 3в: форма для испытателя — только конструктор схемы (2026-08-22).
    // Реальный фронт ввода данных лаборантом (мобильный/веб) пока не разрабатывается —
    // здесь описывается только, какие атрибуты он будет заполнять. ----
    const operatorFormFields: OperatorFormField[] = cfg.operator_form.fields
      .filter(f => attrs.some(a => a.id === f.attribute_id))
      .map(f => ({ ...f }));
    form.createEl('h4', { text: 'Форма для испытателя (данные эксперимента)' });
    form.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Какие атрибуты лаборант вводит при эксперименте — конструктор схемы; сам ввод ' +
      '(мобильный/веб-фронт для испытателя) появится позже, здесь только описание формы.',
    );
    const operatorFormListEl = form.createDiv();
    const operatorFormPreviewEl = form.createDiv();
    redrawOperatorForm = () => {
      operatorFormListEl.empty();
      this.renderOperatorFormRows(operatorFormListEl, operatorFormFields, attrs, () => redrawOperatorForm());
      this.renderOperatorFormPreview(operatorFormPreviewEl, operatorFormFields, attrs);
    };
    redrawOperatorForm();
    const addOperatorFieldBtn = form.createEl('button', { text: '➕ Поле формы', cls: 'tn-btn tn-btn-ghost' });
    addOperatorFieldBtn.addEventListener('click', () => {
      const free = attrs.find(a => a.id && !operatorFormFields.some(f => f.attribute_id === a.id));
      if (!free) { new Notice('Все атрибуты метода уже добавлены в форму испытателя'); return; }
      operatorFormFields.push({ attribute_id: free.id, required: false });
      redrawOperatorForm();
    });

    // buildPatch/performSave вынесены в переиспользуемые замыкания — их же
    // использует проверка несохранённых изменений при закрытии окна
    // (MethodConfigModal.close, 2026-08-23 — прямой запрос пользователя: не
    // было защиты от потери правок при случайном закрытии конфигуратора).
    const buildPatch = (): Partial<MethodConfig> & { lab_ids?: number[]; description?: string; determinable_indicators?: string[] } => {
      const attrIds = new Set(attrs.map(a => a.id));
      return {
        lab_ids: getLabIDs(),
        description: description.value.trim(),
        input_parameters: attrs,
        classification: rules,
        chart_configs: charts,
        presentation: { blocks },
        operator_form: { fields: operatorFormFields.filter(f => attrIds.has(f.attribute_id)) },
        determinable_indicators: determinableIndicators.filter(Boolean),
      };
    };
    // Снимок "как сохранено" — сравнение с текущим buildPatch() определяет
    // isDirty(); обновляется на текущее состояние после каждого удачного
    // сохранения (иначе сразу после «Сохранить» окно продолжило бы считаться
    // "с несохранёнными изменениями" и спрашивало бы повторно при закрытии).
    let savedSnapshot = JSON.stringify(buildPatch());
    const performSave = async (): Promise<boolean> => {
      const labIDs = getLabIDs();
      if (labIDs.length === 0) { new Notice('Укажите хотя бы одну лабораторию'); return false; }
      const validationError = this.validateAttributesAndRules(attrs, rules) || this.validateCharts(charts);
      if (validationError) { new Notice(validationError); return false; }
      try {
        await this.plugin.syncService.updateMethodConfig(methodId, buildPatch());
        await this.plugin.refreshMethods();
        new Notice('Конфиг метода обновлён');
        await this.renderMethods();
        savedSnapshot = JSON.stringify(buildPatch());
        return true;
      } catch (e: unknown) {
        new Notice(`Ошибка: ${errorMessage(e)}`);
        return false;
      }
    };

    const saveBtn = form.createEl('button', { text: '💾 Сохранить', cls: 'tn-btn tn-btn-primary' });
    saveBtn.addEventListener('click', () => { void performSave(); });

    return {
      isDirty: () => JSON.stringify(buildPatch()) !== savedSnapshot,
      save: performSave,
    };
  }

  /** Список блоков документа (2026-08-23) — каждый блок своя карточка:
   * заголовок (для списка, не печатается), три галочки видимости, редактор
   * содержимого (block-editor.ts — визуальный, с чипами-плейсхолдерами),
   * привязанный график. Перетаскивание — порядок блоков в документе. */
  private renderBlocksList(container: HTMLElement, blocks: DocumentBlock[], attrs: MethodAttribute[], charts: ChartConfig[]): void {
    let dragFromIdx: number | null = null;
    const redraw = (): void => {
      container.empty();
      this.renderBlocksList(container, blocks, attrs, charts);
    };
    blocks.forEach((block, idx) => {
      const card = container.createDiv({ cls: 'tn-lims-method', attr: { draggable: 'true' } });
      card.style.cursor = 'grab';
      card.addEventListener('dragstart', (ev) => { dragFromIdx = idx; ev.stopPropagation(); });
      card.addEventListener('dragover', (ev) => ev.preventDefault());
      card.addEventListener('drop', (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        if (dragFromIdx === null || dragFromIdx === idx) return;
        const [moved] = blocks.splice(dragFromIdx, 1);
        blocks.splice(idx, 0, moved);
        dragFromIdx = null;
        redraw();
      });

      const head = card.createDiv({ cls: 'tn-lims-flex' });
      head.createSpan({ text: '⠿', cls: 'tn-lims-meta' });
      const titleInput = head.createEl('input', {
        attr: { type: 'text', placeholder: 'Название блока (напр. «Общая информация»)' },
        cls: 'tn-lims-input',
      });
      titleInput.value = block.title;
      titleInput.addEventListener('change', () => { block.title = titleInput.value.trim() || 'Без названия'; });

      const uiLabel = head.createEl('label', { cls: 'tn-lims-flex' });
      const uiCb = uiLabel.createEl('input', { attr: { type: 'checkbox' } });
      uiCb.checked = block.show_in_ui;
      uiLabel.createSpan({ text: 'Кратко' });
      uiCb.addEventListener('change', () => { block.show_in_ui = uiCb.checked; });

      const excerptLabel = head.createEl('label', { cls: 'tn-lims-flex' });
      const excerptCb = excerptLabel.createEl('input', { attr: { type: 'checkbox' } });
      excerptCb.checked = block.show_in_excerpt;
      excerptLabel.createSpan({ text: 'Выписка' });
      excerptCb.addEventListener('change', () => { block.show_in_excerpt = excerptCb.checked; });

      const protoLabel = head.createEl('label', { cls: 'tn-lims-flex' });
      const protoCb = protoLabel.createEl('input', { attr: { type: 'checkbox' } });
      protoCb.checked = block.show_in_protocol;
      protoLabel.createSpan({ text: 'Протокол' });
      protoCb.addEventListener('change', () => { block.show_in_protocol = protoCb.checked; });

      const delBtn = head.createEl('button', { text: '✖ Удалить блок', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => {
        if (block.content.length > 0 && !window.confirm(`Удалить блок «${block.title}»?`)) return;
        blocks.splice(idx, 1);
        redraw();
      });

      const bodyEl = card.createDiv();
      renderBlockEditor(bodyEl, block, { app: this.app, attrs, charts }, redraw);
    });
  }

  /** Строки формы для испытателя (блок 3в) — тот же паттерн drag-and-drop, что
   * представление: выбор атрибута, подпись, «обязательное», подсказка. */
  private renderOperatorFormRows(
    container: HTMLElement,
    fields: OperatorFormField[],
    attrs: MethodAttribute[],
    onChange: () => void,
  ): void {
    let dragFromIdx: number | null = null;
    fields.forEach((f, idx) => {
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
      const attrSelect = rowFlex.createEl('select', { cls: 'tn-lims-select' });
      for (const a of attrs) {
        if (!a.id) continue;
        attrSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
      }
      attrSelect.value = f.attribute_id;
      attrSelect.addEventListener('change', () => { f.attribute_id = attrSelect.value; onChange(); });

      const labelInput = rowFlex.createEl('input', {
        attr: { type: 'text', placeholder: 'подпись для испытателя (по умолчанию — название атрибута)' },
        cls: 'tn-lims-input',
      });
      labelInput.value = f.label || '';
      labelInput.addEventListener('change', () => { f.label = labelInput.value.trim() || undefined; onChange(); });

      const reqLabel = rowFlex.createEl('label', { cls: 'tn-lims-flex' });
      const reqCb = reqLabel.createEl('input', { attr: { type: 'checkbox' } });
      reqCb.checked = f.required;
      reqLabel.createSpan({ text: 'обязательное' });
      reqCb.addEventListener('change', () => { f.required = reqCb.checked; onChange(); });

      const delBtn = rowFlex.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => { fields.splice(idx, 1); onChange(); });

      const helpInput = row.createEl('input', {
        attr: { type: 'text', placeholder: 'подсказка испытателю (опц.)' },
        cls: 'tn-lims-input',
      });
      helpInput.value = f.help_text || '';
      helpInput.addEventListener('change', () => { f.help_text = helpInput.value.trim() || undefined; onChange(); });
    });
  }

  /** Read-only предпросмотр «как увидит испытатель» — поля формы отрисованы
   * disabled, только layout. Системные данные (2026-08-23) показаны ВСЕГДА, даже
   * если метод-специфичных полей нет — испытатель получает их автоматически
   * (заполняются из письма-результата или позже из фронта ввода), в
   * operator_form.fields их заводить не нужно (правило: системные атрибуты
   * автоматически попадают в форму испытателя, см. AGENTS.md). */
  private renderOperatorFormPreview(container: HTMLElement, fields: OperatorFormField[], attrs: MethodAttribute[]): void {
    container.empty();
    container.createDiv({ cls: 'tn-lims-meta' }).setText('Системные данные (заполняются автоматически, настраивать не нужно):');
    const sysRow = container.createDiv({ cls: 'tn-lims-flex tn-lims-mb8' });
    sysRow.createSpan({ text: SYSTEM_PLACEHOLDERS.map(s => s.label).join(', ') + '.' });
    if (fields.length === 0) return;
    container.createDiv({ cls: 'tn-lims-meta' }).setText('Предпросмотр — как увидит испытатель:');
    for (const f of fields) {
      const attr = attrs.find(a => a.id === f.attribute_id);
      const row = container.createDiv({ cls: 'tn-lims-flex' });
      row.createSpan({ text: (f.label || attr?.name || f.attribute_id) + (f.required ? ' *' : '') });
      const input = row.createEl('input', { attr: { type: 'text', disabled: true }, cls: 'tn-lims-input' });
      input.placeholder = attr?.data_type === 'photo' ? '[фото]' : attr?.data_type || '';
      if (f.help_text) row.createSpan({ text: f.help_text, cls: 'tn-lims-meta' });
    }
  }

  /** Каналы графика "по времени" (kind="timeseries") — фиксированный список под
   * реальную форму письма прибора (mesure_data.channels.channel_1-4/average_temp/
   * derivative, см. extractInstrumentFields в lab-service) — не свободный ввод, чтобы
   * не разойтись с тем, что реально понимает сервер (buildChartSeriesFromTimeseries). */
  private static readonly TIMESERIES_CHANNEL_OPTIONS: Array<[string, string]> = [
    ['channel_1', 'Канал 1'], ['channel_2', 'Канал 2'], ['channel_3', 'Канал 3'], ['channel_4', 'Канал 4'],
    ['average_temp', 'Среднее по каналам'], ['derivative', 'Скорость нарастания'],
  ];

  /** Карточки графиков (блок 3б, chart_configs) — тот же паттерн, что атрибуты/
   * правила классификации. Два несовместимых режима (2026-08-24, см. AGENTS.md
   * "график по датчику"): "по сериям" (обычный — X/Y читаются из значений атрибутов
   * ПОПЕРЁК серий-повторов) и "по времени" (kind="timeseries" — X/Y читаются ИЗ ОДНОГО
   * значения атрибута data_type="timeseries", целого ряда датчика). */
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

      const row1b = card.createDiv({ cls: 'tn-lims-flex' });
      row1b.createSpan({ text: 'Режим:' });
      const modeSelect = row1b.createEl('select', { cls: 'tn-lims-select' });
      modeSelect.createEl('option', { attr: { value: '' }, text: 'По сериям (атрибут на каждый повтор)' });
      modeSelect.createEl('option', { attr: { value: 'timeseries' }, text: 'По времени (весь ряд датчика)' });
      modeSelect.value = chart.kind || '';

      const bodyEl = card.createDiv();
      const redrawBody = (): void => {
        bodyEl.empty();
        if (chart.kind === 'timeseries') {
          this.renderTimeseriesChartBody(bodyEl, chart, attrs);
        } else {
          this.renderSeriesChartBody(bodyEl, chart, attrs);
        }
      };
      modeSelect.addEventListener('change', () => {
        chart.kind = modeSelect.value === 'timeseries' ? 'timeseries' : undefined;
        redrawBody();
      });

      const labelsRow = card.createDiv({ cls: 'tn-lims-flex' });
      const xLabelInput = labelsRow.createEl('input', { attr: { type: 'text', placeholder: 'Подпись оси X' }, cls: 'tn-lims-input' });
      xLabelInput.value = chart.x_label || '';
      xLabelInput.addEventListener('change', () => { chart.x_label = xLabelInput.value.trim() || undefined; });
      const yLabelInput = labelsRow.createEl('input', { attr: { type: 'text', placeholder: 'Подпись оси Y' }, cls: 'tn-lims-input' });
      yLabelInput.value = chart.y_label || '';
      yLabelInput.addEventListener('change', () => { chart.y_label = yLabelInput.value.trim() || undefined; });

      redrawBody();
    });
  }

  /** Тело карточки графика "по сериям" (обычный режим, kind не задан) — Ось X +
   * список рядов, поведение не изменилось относительно версии до режима "по времени". */
  private renderSeriesChartBody(bodyEl: HTMLElement, chart: ChartConfig, attrs: MethodAttribute[]): void {
    const row2 = bodyEl.createDiv({ cls: 'tn-lims-flex' });
    row2.createSpan({ text: 'Ось X:' });
    const xSelect = row2.createEl('select', { cls: 'tn-lims-select' });
    xSelect.createEl('option', { attr: { value: '' }, text: '— номер серии —' });
    for (const a of attrs) {
      if (!a.id || a.data_type === 'timeseries') continue;
      xSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
    }
    xSelect.value = chart.x_column || '';
    xSelect.addEventListener('change', () => { chart.x_column = xSelect.value || undefined; });

    bodyEl.createDiv({ cls: 'tn-lims-meta' }).setText('Ряды:');
    const seriesListEl = bodyEl.createDiv();
    const redrawSeries = () => {
      seriesListEl.empty();
      chart.series_config.forEach((sc, sIdx) => {
        const sRow = seriesListEl.createDiv({ cls: 'tn-lims-flex' });
        const srcSelect = sRow.createEl('select', { cls: 'tn-lims-select' });
        srcSelect.createEl('option', { attr: { value: '' }, text: '— источник —' });
        for (const a of attrs) {
          if (!a.id || a.data_type === 'timeseries') continue; // не скаляр — сюда не годится
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
    const addSeriesBtn = bodyEl.createEl('button', { text: '➕ Ряд', cls: 'tn-btn tn-btn-ghost' });
    addSeriesBtn.addEventListener('click', () => { chart.series_config.push({ source_param: '' }); redrawSeries(); });
  }

  /** Тело карточки графика "по времени" (kind="timeseries", 2026-08-24) — список
   * НЕЗАВИСИМЫХ рядов (timeseries_series): каждый со своим источником (атрибут
   * data_type="timeseries"), каналом и осью — не общий источник+канал на весь график,
   * чтобы можно было наложить ряды из РАЗНЫХ атрибутов (см. TimeseriesSeriesConfig,
   * прямой запрос пользователя учесть случай "пары X-Y не совпадают"). См.
   * lab-service/charts.go buildChartSeriesFromTimeseries — ровно это поле он читает. */
  private renderTimeseriesChartBody(bodyEl: HTMLElement, chart: ChartConfig, attrs: MethodAttribute[]): void {
    const timeseriesAttrs = attrs.filter(a => a.data_type === 'timeseries' && a.id);
    if (!chart.timeseries_series) chart.timeseries_series = [];
    if (timeseriesAttrs.length === 0) {
      bodyEl.createDiv({ cls: 'tn-lims-meta' }).setText(
        'Нет ни одного атрибута с типом "Временной ряд" — сначала заведите такой атрибут выше.',
      );
    }

    bodyEl.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Ряды (у каждого свой источник и канал — можно совмещать разные приборы; "Вторая ось" — для рядов другого масштаба, напр. производная поверх температуры):',
    );
    const listEl = bodyEl.createDiv();
    const redraw = (): void => {
      listEl.empty();
      chart.timeseries_series!.forEach((spec, idx) => {
        const row = listEl.createDiv({ cls: 'tn-lims-flex' });
        const srcSelect = row.createEl('select', { cls: 'tn-lims-select' });
        srcSelect.createEl('option', { attr: { value: '' }, text: '— источник —' });
        for (const a of timeseriesAttrs) {
          srcSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
        }
        srcSelect.value = spec.source_param;
        srcSelect.addEventListener('change', () => { spec.source_param = srcSelect.value; });

        const chSelect = row.createEl('select', { cls: 'tn-lims-select' });
        chSelect.createEl('option', { attr: { value: '' }, text: '— канал —' });
        for (const [key, label] of LimsView.TIMESERIES_CHANNEL_OPTIONS) {
          chSelect.createEl('option', { attr: { value: key }, text: label });
        }
        chSelect.value = spec.channel;
        chSelect.addEventListener('change', () => { spec.channel = chSelect.value; });

        const labelInput = row.createEl('input', { attr: { type: 'text', placeholder: 'Подпись в легенде (опц.)' }, cls: 'tn-lims-input' });
        labelInput.value = spec.label || '';
        labelInput.addEventListener('change', () => { spec.label = labelInput.value.trim() || undefined; });

        const axisSelect = row.createEl('select', { cls: 'tn-lims-select' });
        axisSelect.createEl('option', { attr: { value: '' }, text: 'Основная ось' });
        axisSelect.createEl('option', { attr: { value: 'y2' }, text: 'Вторая ось' });
        axisSelect.value = spec.axis || '';
        axisSelect.addEventListener('change', () => { spec.axis = axisSelect.value === 'y2' ? 'y2' : undefined; });

        const delBtn = row.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
        delBtn.addEventListener('click', () => { chart.timeseries_series!.splice(idx, 1); redraw(); });
      });
    };
    redraw();
    const addBtn = bodyEl.createEl('button', { text: '➕ Ряд', cls: 'tn-btn tn-btn-ghost' });
    addBtn.addEventListener('click', () => {
      chart.timeseries_series!.push({ source_param: '', channel: '' });
      redraw();
    });

    const y2LabelRow = bodyEl.createDiv({ cls: 'tn-lims-flex tn-lims-mt8' });
    const y2LabelInput = y2LabelRow.createEl('input', {
      attr: { type: 'text', placeholder: 'Подпись второй оси (если используется)' },
      cls: 'tn-lims-input',
    });
    y2LabelInput.value = chart.y2_label || '';
    y2LabelInput.addEventListener('change', () => { chart.y2_label = y2LabelInput.value.trim() || undefined; });
  }

  /** Валидация графиков перед сохранением. Представление (блоки форматированного
   * текста) не валидируется структурно — это свободный текст, устаревшая ссылка
   * на атрибут в плейсхолдере просто резолвится в пустую строку при рендере,
   * не является ошибкой сохранения. */
  private validateCharts(charts: ChartConfig[]): string | null {
    for (const c of charts) {
      if (c.kind === 'timeseries') {
        if (!c.timeseries_series || c.timeseries_series.length === 0) {
          return `График «${c.title || c.id}»: добавьте хотя бы один ряд`;
        }
        for (const spec of c.timeseries_series) {
          if (!spec.source_param) return `График «${c.title || c.id}»: укажите источник для каждого ряда`;
          if (!spec.channel) return `График «${c.title || c.id}»: укажите канал для каждого ряда`;
        }
        continue;
      }
      if (c.series_config.length === 0) return `График «${c.title || c.id}»: добавьте хотя бы один ряд`;
      for (const sc of c.series_config) {
        if (!sc.source_param.trim()) return `График «${c.title || c.id}»: укажите источник для каждого ряда`;
      }
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
    methodId: number,
    determinableIndicators: string[],
    onChange: () => void,
  ): void {
    attrs.forEach((attr, idx) => {
      const row = container.createDiv({ cls: 'tn-lims-method tn-lims-flex' });
      const idInput = row.createEl('input', { attr: { type: 'text', placeholder: 'id (напр. comb_length_1)' }, cls: 'tn-lims-input' });
      idInput.value = attr.id;
      idInput.addEventListener('change', () => { attr.id = idInput.value.trim(); onChange(); });
      const nameInput = row.createEl('input', { attr: { type: 'text', placeholder: 'Название' }, cls: 'tn-lims-input' });
      nameInput.value = attr.name;
      nameInput.addEventListener('change', () => { attr.name = nameInput.value.trim(); });
      const suggestIdBtn = row.createEl('button', { text: '🤖', cls: 'tn-btn tn-btn-ghost' });
      suggestIdBtn.setAttribute('title', 'Предложить id по названию (или найти уже существующий атрибут)');
      suggestIdBtn.addEventListener('click', (e) => {
        e.preventDefault();
        void this.suggestAttributeId(idInput, nameInput, attr, methodId, attrs, suggestIdBtn).then(() => onChange());
      });
      const subSupBtn = row.createEl('button', { text: 'x²', cls: 'tn-btn tn-btn-ghost' });
      subSupBtn.setAttribute('title', 'Вставить надстрочный/подстрочный символ в название (напр. CO₂, м³)');
      subSupBtn.addEventListener('click', (e) => { e.preventDefault(); toggleSubSupPalette(row, nameInput); });

      const typeSelect = row.createEl('select', { cls: 'tn-lims-select' });
      const typeOptions: Array<[AttributeDataType, string]> = [
        ['text', 'Текст'], ['int', 'Целое число'], ['float', 'Дробное число'],
        ['date', 'Дата'], ['time', 'Время'], ['boolean', 'Да/Нет'], ['photo', 'Фотография'],
        ['timeseries', 'Временной ряд (для графика)'],
      ];
      for (const [val, label] of typeOptions) typeSelect.createEl('option', { attr: { value: val }, text: label });
      typeSelect.value = attr.data_type;
      typeSelect.addEventListener('change', () => { attr.data_type = typeSelect.value as AttributeDataType; });

      const fillSelect = row.createEl('select', { cls: 'tn-lims-select' });
      const fillOptions: Array<[AttributeFillMethod, string]> = [
        ['manual', 'Ручной ввод'], ['instrument', 'Данные прибора'], ['calculated', 'Расчёт'],
        ['classification', 'Правила классификации'],
      ];
      for (const [val, label] of fillOptions) fillSelect.createEl('option', { attr: { value: val }, text: label });
      fillSelect.value = attr.fill_method;
      fillSelect.addEventListener('change', () => {
        // Чистим поля предыдущего способа заполнения (2026-08-23) — раньше
        // .formula/.aggregation оставались в атрибуте после переключения
        // fill_method, просто скрытые из вида; сервер их больше не подхватывает
        // молча (см. deriveFormulasFromAttributes, lab-service), но оставлять
        // невидимые устаревшие данные в конфиге — источник путаницы при
        // повторном открытии/экспорте, поэтому чистим и здесь.
        const next = fillSelect.value as AttributeFillMethod;
        if (next !== 'calculated') attr.formula = undefined;
        if (next === 'calculated' || next === 'classification') attr.aggregation = undefined;
        attr.fill_method = next;
        onChange();
      });

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
        aiBtn.addEventListener('click', () => this.openAiFormulaAssist(row, attrs, determinableIndicators, formulaInput, attr));
      } else if (attr.fill_method === 'classification') {
        row.createSpan({ cls: 'tn-lims-meta', text: 'значение пишет правило классификации (см. блок «Правила классификации» ниже)' });
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

  /** Сводка уже существующих атрибутов для контекста ИИ (переиспользование по
   * смыслу вместо изобретения дублей) — атрибуты ЭТОГО метода (currentAttrs, если
   * передан) + атрибуты ВСЕХ ДРУГИХ методов системы, без дублей id. currentAttrs
   * не обязателен: черновик из стандарта (блок «Импортировать из стандарта»)
   * заменяет текущие атрибуты целиком, поэтому им незачем фигурировать как
   * «уже существующие». methodConfigOf нормализует input_parameters — напрямую
   * m.input_parameters трогать нельзя: сервер может быть старее фронтенда (см.
   * AGENTS.md) и вообще не отдавать это поле. */
  private collectExistingAttributeSummaries(methodId: number, currentAttrs: MethodAttribute[]): ExistingAttributeSummary[] {
    const seenIds = new Set<string>();
    const out: ExistingAttributeSummary[] = [];
    for (const a of currentAttrs) {
      if (!a.id || seenIds.has(a.id)) continue;
      seenIds.add(a.id);
      out.push({ id: a.id, name: a.name, data_type: a.data_type, fill_method: a.fill_method, level: a.level });
    }
    for (const m of this.plugin.methods) {
      if (m.id === methodId) continue;
      for (const a of this.methodConfigOf(m.id).input_parameters) {
        if (!a.id || seenIds.has(a.id)) continue;
        seenIds.add(a.id);
        out.push({ id: a.id, name: a.name, data_type: a.data_type, fill_method: a.fill_method, level: a.level });
      }
    }
    return out;
  }

  /** Значок-помощник в строке атрибута (2026-08-22, перенесено из общей панели
   * списка по просьбе пользователя): по уже введённому пользовательскому названию
   * атрибута (nameInput) ищет среди уже существующих атрибутов (этого и других
   * методов) смыслово подходящий — если нашёлся, копирует его id как есть (эффект
   * переиспользования: та же величина под тем же id); иначе просит ИИ предложить
   * новый id (перевод смысла на английский, не транслитерация). Меняет ТОЛЬКО
   * idInput/attr.id — остальные поля строки пользователь заполняет сам. */
  private async suggestAttributeId(
    idInput: HTMLInputElement,
    nameInput: HTMLInputElement,
    attr: MethodAttribute,
    methodId: number,
    attrs: MethodAttribute[],
    btn: HTMLButtonElement,
  ): Promise<void> {
    const name = nameInput.value.trim();
    if (!name) { new Notice('Сначала введите название атрибута'); return; }
    btn.disabled = true;
    try {
      const existingAttributes = this.collectExistingAttributeSummaries(methodId, attrs);
      const { matched, drafted } = await this.plugin.llmAssist.suggestAttributesFromNames([name], existingAttributes);
      if (matched.length > 0) {
        const ex = matched[0].existing;
        idInput.value = ex.id;
        attr.id = ex.id;
        new Notice(`Уже есть такой атрибут: «${ex.name}» (${ex.id}) — id скопирован`);
      } else if (drafted.length > 0) {
        idInput.value = drafted[0].id;
        attr.id = drafted[0].id;
        new Notice(`Предложен новый id: ${drafted[0].id}`);
      } else {
        new Notice('Не удалось предложить id — попробуйте переформулировать название');
      }
    } catch (e: unknown) {
      console.error('ЛИМС: ошибка подбора id атрибута:', e);
      new Notice(`Ошибка ИИ-помощника: ${errorMessage(e)}`);
    } finally {
      btn.disabled = false;
    }
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
    attr: MethodAttribute,
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
      // Раньше это меняло только видимый текст поля (formulaInput.value) — не
      // сам attr.formula, который читает валидация/сохранение (он обновляется
      // только по DOM-событию 'change', а после программной вставки такое
      // событие не возникает без блюра/повторного фокуса поля). Внешне формула
      // выглядела вставленной, но «Сохранить» отвергал атрибут как без формулы.
      // 2026-08-22, обнаружено пользователем.
      formulaInput.value = resultArea.value;
      attr.formula = resultArea.value;
      panel.remove();
    });
  }

  /** Рисует строки правил классификации (блок 2, 2026-08-22v3 — по прямой правке
   * пользователя: убрана «нефункциональная» строка результата/агрегации, и из
   * самой схемы условий убрано упоминание конкретных атрибутов — «Если [знак] Б
   * (И/ИЛИ …), то показатель» — ОДНА схема на всё правило (список ВЕТОК,
   * порядок важен, ветка без условий = безусловное «Иначе»). Ниже — динамическая
   * таблица «Оцениваемый атрибут» / «Куда записать результат оценки»: эта же
   * схема условий применяется ПО ОТДЕЛЬНОСТИ к каждой строке (пользователь:
   * «прогоняем оба списка через циклы») — можно оценивать сразу несколько
   * атрибутов одним и тем же набором условий. Свод по нескольким сериям убран
   * целиком (по прямой правке пользователя) — значение атрибута всегда берётся
   * из текущей записи как есть. */
  private renderClassificationRows(
    container: HTMLElement,
    rules: ClassificationRule[],
    attrs: MethodAttribute[],
    determinableIndicators: string[],
    onChange: () => void,
  ): void {
    rules.forEach((rule, idx) => {
      const card = container.createDiv({ cls: 'tn-lims-method' });
      const topRow = card.createDiv({ cls: 'tn-lims-flex' });
      const delBtn = topRow.createEl('button', { text: '✖ правило', cls: 'tn-btn tn-btn-ghost' });
      delBtn.addEventListener('click', () => { rules.splice(idx, 1); onChange(); });

      card.createDiv({ cls: 'tn-lims-meta' }).setText(
        'Ветки «Если…» проверяются по порядку сверху вниз, первая совпавшая — результат. ' +
        'Ветка «Иначе» — без условий, срабатывает всегда (обычно последняя). ' +
        'Эта схема условий применяется отдельно к каждой строке таблицы ниже.',
      );
      this.renderBranchRows(card, rule, attrs, determinableIndicators);
      this.renderSubjectRows(card, rule, attrs);
    });
  }

  /** Список веток одного правила (переменной длины, порядок важен — см. ▲▼). Каждая
   * ветка — явное «Если [clause] (И/ИЛИ [clause] …) → [показатель]» либо «Иначе →
   * [показатель]» (пустой список clauses). Левая часть каждого clause — НЕЯВНАЯ
   * (текущий оцениваемый атрибут из таблицы subjects), поэтому здесь только
   * оператор + «сравнить с». */
  private renderBranchRows(
    card: HTMLElement,
    rule: ClassificationRule,
    attrs: MethodAttribute[],
    determinableIndicators: string[],
  ): void {
    const list = card.createDiv();
    let redraw: () => void;
    redraw = () => {
      list.empty();
      rule.branches.forEach((branch, i) => {
        const branchBox = list.createDiv({ cls: 'tn-lims-method' });
        const topRow = branchBox.createDiv({ cls: 'tn-lims-flex' });
        const upBtn = topRow.createEl('button', { text: '▲', cls: 'tn-btn tn-btn-ghost' });
        upBtn.disabled = i === 0;
        upBtn.addEventListener('click', () => {
          [rule.branches[i - 1], rule.branches[i]] = [rule.branches[i], rule.branches[i - 1]];
          redraw();
        });
        const downBtn = topRow.createEl('button', { text: '▼', cls: 'tn-btn tn-btn-ghost' });
        downBtn.disabled = i === rule.branches.length - 1;
        downBtn.addEventListener('click', () => {
          [rule.branches[i], rule.branches[i + 1]] = [rule.branches[i + 1], rule.branches[i]];
          redraw();
        });
        const delBranchBtn = topRow.createEl('button', { text: '✖ ветку', cls: 'tn-btn tn-btn-ghost' });
        delBranchBtn.addEventListener('click', () => { rule.branches.splice(i, 1); redraw(); });

        const clauses = branch.clauses || [];
        if (clauses.length === 0) {
          topRow.createEl('strong', { text: 'Иначе' });
        } else {
          branchBox.createEl('strong', { text: 'Если' });
          const clausesEl = branchBox.createDiv();
          clauses.forEach((clause, ci) => {
            const clauseRow = clausesEl.createDiv({ cls: 'tn-lims-flex' });
            if (ci === 0) {
              clauseRow.createSpan({ cls: 'tn-lims-meta', text: '' });
            } else {
              const joinSelect = clauseRow.createEl('select', { cls: 'tn-lims-select' });
              joinSelect.createEl('option', { attr: { value: 'and' }, text: 'И' });
              joinSelect.createEl('option', { attr: { value: 'or' }, text: 'ИЛИ' });
              joinSelect.value = branch.join || 'and';
              joinSelect.addEventListener('change', () => {
                branch.join = joinSelect.value === 'or' ? 'or' : 'and';
                redraw();
              });
            }
            const opSelect = clauseRow.createEl('select', { cls: 'tn-lims-select' });
            for (const [val, label] of COMPARISON_OPERATOR_OPTIONS) opSelect.createEl('option', { attr: { value: val }, text: label });
            opSelect.value = clause.operator;
            opSelect.addEventListener('change', () => { clause.operator = opSelect.value as ComparisonOperator; });
            // Защита от повреждённых/устаревших данных: у метода "ГВ" нашлось
            // условие без compare_to (или с некорректным kind), что валило весь
            // рендер конфигуратора (Cannot read properties of undefined (reading
            // 'kind')) — самоисправляем на литерал по умолчанию, чтобы окно
            // открывалось, а не падало целиком из-за одного повреждённого правила.
            if (!clause.compare_to || !clause.compare_to.kind) {
              clause.compare_to = { kind: 'literal', value: '' };
            }
            this.renderOperandEditor(clauseRow, clause.compare_to, attrs);
            const delClauseBtn = clauseRow.createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
            delClauseBtn.addEventListener('click', () => { clauses.splice(ci, 1); redraw(); });
          });
          const addClauseRow = branchBox.createDiv({ cls: 'tn-lims-flex' });
          const addAndBtn = addClauseRow.createEl('button', { text: '➕ И условие', cls: 'tn-btn tn-btn-ghost' });
          addAndBtn.addEventListener('click', () => {
            if (clauses.length > 0) branch.join = 'and';
            clauses.push({ operator: '<=', compare_to: { kind: 'literal', value: '' } });
            branch.clauses = clauses;
            redraw();
          });
          const addOrBtn = addClauseRow.createEl('button', { text: '➕ ИЛИ условие', cls: 'tn-btn tn-btn-ghost' });
          addOrBtn.addEventListener('click', () => {
            if (clauses.length > 0) branch.join = 'or';
            clauses.push({ operator: '<=', compare_to: { kind: 'literal', value: '' } });
            branch.clauses = clauses;
            redraw();
          });
        }

        const thenRow = branchBox.createDiv({ cls: 'tn-lims-flex' });
        thenRow.createSpan({ text: 'то показатель =' });
        // Свободный ввод (2026-08-23, по прямому запросу пользователя) — раньше
        // результат ветки был ЖЁСТКО ограничен выпадающим списком показателей
        // метода (determinableIndicators, напр. В1/В2/В3 у ГВ) — не давало
        // написать вывод вида "Соответствует"/"Не соответствует" для правил
        // сравнения с целевым показателем (target_group_compliance). Текстовое
        // поле — источник истины; select рядом — только подсказка/быстрая
        // вставка (показатели метода + стандартные варианты соответствия),
        // сбрасывается на placeholder после вставки.
        const gradeInput = thenRow.createEl('input', {
          attr: { type: 'text', placeholder: 'показатель или вывод (свободный ввод)' },
          cls: 'tn-lims-input',
        });
        gradeInput.value = branch.grade;
        gradeInput.addEventListener('change', () => { branch.grade = gradeInput.value.trim(); });
        const gradeSelect = thenRow.createEl('select', { cls: 'tn-lims-select', attr: { title: 'быстрая вставка' } });
        gradeSelect.createEl('option', { attr: { value: '' }, text: '— вставить —' });
        const quickPicks = [...determinableIndicators, ...COMPLIANCE_VERDICTS.filter(v => !determinableIndicators.includes(v))];
        for (const g of quickPicks) gradeSelect.createEl('option', { attr: { value: g }, text: g });
        gradeSelect.addEventListener('change', () => {
          if (!gradeSelect.value) return;
          branch.grade = gradeSelect.value;
          gradeInput.value = gradeSelect.value;
          gradeSelect.value = '';
        });
      });
    };
    redraw();
    const addRow = card.createDiv({ cls: 'tn-lims-flex' });
    const addIfBtn = addRow.createEl('button', { text: '➕ Ветка «Если…»', cls: 'tn-btn tn-btn-ghost' });
    addIfBtn.addEventListener('click', () => {
      rule.branches.push({
        clauses: [{ operator: '<=', compare_to: { kind: 'literal', value: '' } }],
        grade: determinableIndicators[0] || '',
      });
      redraw();
    });
    const addElseBtn = addRow.createEl('button', { text: '➕ Ветка «Иначе»', cls: 'tn-btn tn-btn-ghost' });
    addElseBtn.addEventListener('click', () => {
      rule.branches.push({ grade: determinableIndicators[0] || '' });
      redraw();
    });
  }

  /** Динамическая таблица «Оцениваемый атрибут» / «Куда записать результат
   * оценки» (2026-08-22v3, по прямой правке пользователя) — схема условий
   * правила (branches) применяется ОТДЕЛЬНО к каждой строке: значение
   * input_attribute_id подставляется как неявная левая часть во все условия,
   * результат совпавшей ветки пишется в output_attribute_id. Строк может быть
   * несколько — одно и то же правило можно применить сразу к нескольким
   * атрибутам. */
  private renderSubjectRows(card: HTMLElement, rule: ClassificationRule, attrs: MethodAttribute[]): void {
    card.createDiv({ cls: 'tn-lims-meta' }).setText('Оцениваемый атрибут → куда записать результат оценки:');
    const table = card.createEl('table', { cls: 'tn-table' });
    const thead = table.createEl('thead').createEl('tr');
    thead.createEl('th', { text: 'Оцениваемый атрибут' });
    thead.createEl('th', { text: 'Куда записать результат оценки' });
    thead.createEl('th');
    const tbody = table.createEl('tbody');
    let redraw: () => void;
    redraw = () => {
      tbody.empty();
      rule.subjects.forEach((subject, i) => {
        const tr = tbody.createEl('tr');
        const inputSelect = tr.createEl('td').createEl('select', { cls: 'tn-lims-select' });
        inputSelect.createEl('option', { attr: { value: '' }, text: '— атрибут —' });
        for (const a of attrs) {
          if (!a.id) continue;
          inputSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
        }
        inputSelect.value = subject.input_attribute_id;
        inputSelect.addEventListener('change', () => { subject.input_attribute_id = inputSelect.value; });

        const outputSelect = tr.createEl('td').createEl('select', { cls: 'tn-lims-select' });
        outputSelect.createEl('option', { attr: { value: '' }, text: '— атрибут —' });
        for (const a of attrs) {
          if (!a.id) continue;
          outputSelect.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
        }
        outputSelect.value = subject.output_attribute_id;
        outputSelect.addEventListener('change', () => { subject.output_attribute_id = outputSelect.value; });

        const delBtn = tr.createEl('td').createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
        delBtn.addEventListener('click', () => { rule.subjects.splice(i, 1); redraw(); });
      });
    };
    redraw();
    const addBtn = card.createEl('button', { text: '➕ Строка', cls: 'tn-btn tn-btn-ghost' });
    addBtn.addEventListener('click', () => {
      rule.subjects.push({ input_attribute_id: '', output_attribute_id: '' });
      redraw();
    });
  }

  /** Редактор операнда правой части сравнения («сравнить с») — переключатель
   * вида (атрибут/значение/целевой показатель заявки) + соответствующий
   * под-контрол. Мутирует operand на месте. */
  private renderOperandEditor(container: HTMLElement, operand: Operand, attrs: MethodAttribute[]): void {
    const box = container.createDiv({ cls: 'tn-lims-flex' });
    const kindSelect = box.createEl('select', { cls: 'tn-lims-select' });
    const kindOptions: Array<[Operand['kind'], string]> = [
      ['literal', 'значению'], ['attribute', 'другому атрибуту'], ['target_indicator', 'целевому показателю заявки'],
    ];
    for (const [val, kLabel] of kindOptions) kindSelect.createEl('option', { attr: { value: val }, text: kLabel });
    kindSelect.value = operand.kind;

    const subEl = box.createDiv({ cls: 'tn-lims-flex' });
    const renderSub = () => {
      subEl.empty();
      if (operand.kind === 'attribute') {
        const sel = subEl.createEl('select', { cls: 'tn-lims-select' });
        sel.createEl('option', { attr: { value: '' }, text: '— атрибут —' });
        for (const a of attrs) {
          if (!a.id) continue;
          sel.createEl('option', { attr: { value: a.id }, text: a.name || a.id });
        }
        sel.value = operand.id;
        sel.addEventListener('change', () => { (operand as { kind: 'attribute'; id: string }).id = sel.value; });
      } else if (operand.kind === 'literal') {
        const input = subEl.createEl('input', { attr: { type: 'text', placeholder: 'значение (число, Да/Нет, показатель…)' }, cls: 'tn-lims-input' });
        input.value = String(operand.value);
        input.addEventListener('change', () => { (operand as { kind: 'literal'; value: string | number }).value = parseConditionLiteral(input.value); });
      } else {
        subEl.createSpan({ cls: 'tn-lims-meta', text: 'целевой показатель заявки' });
      }
    };
    renderSub();
    kindSelect.addEventListener('change', () => {
      const kind = kindSelect.value as Operand['kind'];
      if (kind === 'attribute') Object.assign(operand, { kind: 'attribute', id: attrs.find(a => a.id)?.id || '' });
      else if (kind === 'literal') Object.assign(operand, { kind: 'literal', value: '' });
      else Object.assign(operand, { kind: 'target_indicator' });
      // удалить более не относящиеся к делу поля прежнего вида операнда
      if (kind !== 'attribute') delete (operand as Record<string, unknown>).id;
      if (kind !== 'literal') delete (operand as Record<string, unknown>).value;
      renderSub();
    });
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
      if (a.fill_method === 'classification' && !rules.some(r => r.subjects.some(s => s.output_attribute_id === a.id))) {
        return `Атрибут «${a.id}»: способ заполнения «Правила классификации», но ни одно правило не пишет в этот атрибут — добавьте строку с результатом «${a.id}» в таблицу правила или смените способ заполнения`;
      }
      if (a.fill_method !== 'calculated' && a.fill_method !== 'classification' && a.level === 'aggregated' && !a.aggregation) {
        return `Атрибут «${a.id}»: укажите принцип агрегирования или формулу`;
      }
    }
    for (const r of rules) {
      if (r.branches.length === 0) return 'У каждого правила классификации добавьте хотя бы одну ветку («Если…» или «Иначе»)';
      if (r.subjects.length === 0) return 'У каждого правила классификации добавьте хотя бы одну строку «Оцениваемый атрибут → куда записать результат»';
      for (const branch of r.branches) {
        if (!branch.grade.trim()) return 'У каждой ветки правила классификации укажите показатель';
        for (const clause of branch.clauses || []) {
          // compare_to может быть не задан у повреждённых/устаревших данных
          // (см. renderOperandEditor) — к моменту сохранения renderBranchRows
          // уже самоисправил их в памяти, но не полагаемся на порядок вызовов.
          if (clause.compare_to?.kind === 'attribute' && !ids.has(clause.compare_to.id)) {
            return `Условие правила классификации ссылается на несуществующий атрибут «${clause.compare_to.id}»`;
          }
        }
      }
      for (const subject of r.subjects) {
        if (!subject.input_attribute_id || !ids.has(subject.input_attribute_id)) {
          return 'В таблице правила классификации укажите оцениваемый атрибут для каждой строки';
        }
        if (!subject.output_attribute_id || !ids.has(subject.output_attribute_id)) {
          return `Правило классификации: укажите, куда записать результат оценки атрибута «${subject.input_attribute_id}»`;
        }
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

  /** Сотрудники лаборатории — глобальный admin+, либо lab_admin ИМЕННО текущей
   * лабы (2026-08-24, делегированные полномочия: lab_admin теперь полноценный
   * руководитель своей лабы, не синоним lab_operator — см. lab-service kanban.go/
   * requireLabAdminOf). Роль каждого сотрудника редактируется на месте (select) —
   * раньше единственным способом сменить роль было удалить и добавить заново. */
  private async renderLabMembers(): Promise<void> {
    if (!this.labId) {
      this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText('Выберите лабораторию.');
      return;
    }
    this.bodyEl.createDiv({ cls: 'tn-lims-meta', text: 'Загрузка…' });
    try {
      const roster = await this.fetchLabRoster(this.labId);
      const isLabAdmin = this.myRoleIn(roster) === 'lab_admin';
      if (!this.canAdmin && !isLabAdmin) {
        this.bodyEl.empty();
        this.bodyEl.createDiv({ cls: 'tn-lims-meta tn-lims-p24' }).setText(
          'Раздел доступен только руководителю лаборатории (администратору или lab_admin этой лабы).'
        );
        return;
      }
      const members = roster.filter(m => m.lab_id === this.labId);
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
        const roleSelect = tr.createEl('td').createEl('select', { cls: 'tn-lims-select' });
        for (const [v, l] of LAB_MEMBER_ROLE_LABELS) roleSelect.createEl('option', { value: v, text: l });
        roleSelect.value = m.role;
        roleSelect.addEventListener('change', async () => {
          try {
            await this.plugin.syncService.setLabMember(m.lab_id, m.email, roleSelect.value);
            new Notice(`Роль ${m.email} обновлена`);
            await this.renderLabMembers();
          } catch (e: unknown) {
            new Notice(`Ошибка: ${errorMessage(e)}`);
          }
        });
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
    for (const [v, l] of LAB_MEMBER_ROLE_LABELS) roleSelect.createEl('option', { value: v, text: l });
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
      presentation: m?.presentation && Array.isArray(m.presentation.blocks) ? m.presentation : { blocks: [] },
      operator_form: m?.operator_form && Array.isArray(m.operator_form.fields) ? m.operator_form : { fields: [] },
    };
  }

  /** Достаёт массив "сырых" атрибутов из вставленного JSON — три распознаваемых
   * формата: (1) уже массив атрибутов; (2) объект-обёртка с полем attributes/
   * input_parameters (напр. выгрузка целого метода); (3) — 2026-08-22, по
   * практике — плоский объект ПРИМЕРА результата (реальное письмо email-импорта:
   * ключ = raw-имя поля, значение = пример измерения) — в этом случае атрибуты
   * не заданы явно, а ВЫВОДЯТСЯ из ключей/значений (см. inferAttributesFromSample).
   * null — только если вообще не удалось трактовать ни так, ни так. */
  private extractAttributeArray(parsed: unknown): unknown[] | null {
    if (Array.isArray(parsed)) return parsed;
    if (parsed && typeof parsed === 'object') {
      const obj = parsed as Record<string, unknown>;
      if (Array.isArray(obj.attributes)) return obj.attributes;
      if (Array.isArray(obj.input_parameters)) return obj.input_parameters;
      // плоский объект без обёртки — трактуем как пример result-payload
      return this.inferAttributesFromSample(obj);
    }
    return null;
  }

  /** Поля письма email-импорта, которые НЕ становятся generic-атрибутами — либо
   * маршрутизация (type/method/ID/series_num/aim_indicator), либо уже есть свои
   * колонки на MeasurementResult (photo_before/photo_after), см. resultMetaFields
   * в email_ingest.go (тот же список, серверная сторона). */
  private static readonly SAMPLE_SKIP_KEYS = new Set([
    'type', 'method', 'id', 'series_num', 'aim_indicator',
    'photo_before', 'photo_after', 'source_file', 'timestamp',
  ]);

  /** ЗЕРКАЛО server_back/lab-service/email_ingest.go canonicalFieldNames — держать
   * в синхроне вручную. Raw-имя письма переименовывается сервером ДО попадания в
   * values, поэтому при выводе атрибута из примера письма нужно угадывать id по
   * уже каноническому имени, а не по сырому ключу — иначе реальные результаты
   * никогда не попадут в атрибут с "правильным" на вид, но не тем id. */
  private static readonly CANONICAL_FIELD_NAMES: Record<string, string> = {
    Comb_lenth_1: 'comb_length_1', Comb_lenth_2: 'comb_length_2',
    Comb_lenth_3: 'comb_length_3', Comb_lenth_4: 'comb_length_4',
    sampels_in_date: 'samples_in_date', flam_date_material_in: 'samples_in_date',
    flam_fixation: 'mounting_method', flam_subst: 'substrate',
    flam_inventor: 'inventor', additional_inf: 'additional_info',
    flam_additional_inf: 'additional_info', flam_exp_date: 'exp_date',
    flam_rep_date: 'report_date',
  };

  /** Выводит черновик атрибутов из ПРИМЕРА значений (плоский объект "имя поля" →
   * "значение") — практичнее, чем требовать от пользователя написать полное
   * определение атрибута вручную: вставил реальное письмо результата — получил
   * черновик атрибутов с угаданным типом данных по значению, дальше правишь как
   * обычно. id/name — сам ключ (в реальных письмах уже осмысленное raw-имя). */
  private inferAttributesFromSample(sample: Record<string, unknown>): unknown[] {
    const out: unknown[] = [];
    for (const [key, value] of Object.entries(sample)) {
      if (LimsView.SAMPLE_SKIP_KEYS.has(key.toLowerCase())) continue;
      // raw-имя может быть переименовано сервером (canonicalFieldNames) ДО того,
      // как попадёт в values — атрибут заводим сразу под тем именем, под которым
      // реальные результаты и придут, иначе они пройдут мимо.
      const id = LimsView.CANONICAL_FIELD_NAMES[key] || key;
      out.push({
        id,
        name: key,
        data_type: this.guessDataType(value),
        fill_method: 'manual',
        level: 'experiment',
      });
    }
    return out;
  }

  /** Угадывает data_type атрибута по примеру значения — пустая строка (часто
   * встречается в письмах прибора, см. json_attr.md) не даёт сигнала, дефолт text. */
  private guessDataType(value: unknown): AttributeDataType {
    if (typeof value === 'boolean') return 'boolean';
    if (typeof value === 'number') return Number.isInteger(value) ? 'int' : 'float';
    if (typeof value !== 'string') return 'text';
    const v = value.trim();
    if (!v) return 'text';
    if (/^(да|нет)$/i.test(v)) return 'boolean';
    if (/^-?\d+$/.test(v)) return 'int';
    if (/^-?\d+[.,]\d+$/.test(v)) return 'float';
    if (/^\d{4}-\d{2}-\d{2}$/.test(v) || /^\d{2}\.\d{2}\.\d{4}$/.test(v)) return 'date';
    if (/^\d{1,2}:\d{2}(:\d{2})?$/.test(v)) return 'time';
    return 'text';
  }

  /** Показывает HTML протокола/выписки/краткого вида в настоящей Obsidian Modal
   * (2026-08-23) — раньше вставляла обычный div прямо в bodyEl без singleton-
   * контроля, из-за чего повторные клики по кнопкам вида (или клик по нескольким
   * подряд) плодили накладывающиеся друг на друга div'ы, не закрываемые ни ESC,
   * ни кликом по фону ("плодятся много экземпляров вывода"). Теперь закрывает
   * предыдущую открытую модалку протокола перед тем, как открыть новую. */
  private showHtmlModal(req: LimsRequest, html: string, onDownloadDocx: () => void): void {
    this.openProtocolModal?.close();
    const modal = new ProtocolHtmlModal(this.app, req, html, onDownloadDocx);
    this.openProtocolModal = modal;
    modal.open();
  }

  private downloadDocx(base64Data: string, fileName: string): void {
    try {
      downloadBase64File(base64Data, fileName, 'application/vnd.openxmlformats-officedocument.wordprocessingml.document');
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

  /** Admin+ (app-level, включая superadmin) — глобально разрешено всё, что ниже
   * дополнительно открыто lab_admin ТОЛЬКО для своей лабы (2026-08-24,
   * делегированные полномочия — по прямому запросу пользователя: lab_admin должен
   * быть реальным админом своей лабы, а не синонимом lab_operator): конфиг/
   * создание/удаление методов своей лабы, сотрудники своей лабы (любая роль, в
   * т.ч. назначение другого lab_admin), роль «руководителя» в Kanban-доске своей
   * лабы. Точная lab_admin/lab_auditor роль текущего пользователя для конкретной
   * лабы — через fetchLabRoster/myRoleIn (GET /lab-members?lab_id= доступен
   * любому участнику лабы, не только admin — см. lims_refs.go
   * handleListLabMembers); за пределами своей лабы у lab_admin прав нет — там
   * решает именно этот геттер (глобальная роль). */
  private get canAdmin(): boolean {
    return this.myRole === 'admin' || this.myRole === 'superadmin';
  }

  /** Editor+ (app-level, включая admin/superadmin) — испытатели/оборудование. */
  private get canEditRefs(): boolean {
    return this.myRole === 'editor' || this.myRole === 'admin' || this.myRole === 'superadmin';
  }
}

/** Возвращается renderMethodConfigForm — MethodConfigModal.close() использует
 * это для защиты от потери несохранённых правок при случайном закрытии окна
 * (крестик/Esc/клик вне модалки — все идут через close(), см. ниже). */
export interface MethodConfigHandle {
  isDirty(): boolean;
  save(): Promise<boolean>;
}

/** Протокол/выписка/краткий вид в настоящей Obsidian Modal (2026-08-23) —
 * заменила plain div, вставлявшийся прямо в bodyEl без singleton-контроля (см.
 * LimsView.showHtmlModal). onClose закрывает через встроенный механизм Modal
 * (крестик/Esc/клик по фону), в отличие от старой ручной кнопки ✖. */
class ProtocolHtmlModal extends Modal {
  constructor(app: App, private req: LimsRequest, private html: string, private onDownloadDocx: () => void) {
    super(app);
  }

  onOpen(): void {
    this.modalEl.addClass('tn-lims-protocol-modal');
    this.titleEl.setText(`Протокол № ${fullRequestNumber(this.req)}`);
    const iframe = this.contentEl.createEl('iframe', { attr: { sandbox: '' }, cls: 'tn-lims-iframe' });
    iframe.setAttr('srcdoc', this.html);
    const docxBtn = this.contentEl.createEl('button', { text: 'Скачать DOCX', cls: 'tn-btn tn-btn-ghost' });
    docxBtn.addEventListener('click', () => this.onDownloadDocx());
  }

  onClose(): void {
    this.contentEl.empty();
  }
}

/** Конфигуратор метода в отдельном окне (2026-08-22, по просьбе пользователя —
 * раньше форма разворачивалась прямо под карточкой метода в списке; «Сохранить»
 * вызывал полный renderMethods() списка, который стирал bodyEl вместе с этой
 * инлайн-формой — окно визуально «сворачивалось», хотя данные сохранялись
 * верно. Modal рендерится вне bodyEl, поэтому renderMethods() в фоне его больше
 * не трогает — форма остаётся открытой сколько угодно после сохранения. */
class MethodConfigModal extends Modal {
  private view: LimsView;
  private methodId: number;
  private handle: MethodConfigHandle | null = null;

  constructor(view: LimsView, methodId: number) {
    super(view.app);
    this.view = view;
    this.methodId = methodId;
    this.modalEl.addClass('tn-lims-config-modal');
  }

  onOpen(): void {
    const method = this.view.plugin.methods.find(m => m.id === this.methodId);
    this.titleEl.setText(method ? `Конфигурация метода: ${method.code} — ${method.name || 'без названия'}` : 'Конфигурация метода');
    this.handle = this.view.renderMethodConfigForm(this.contentEl, this.methodId);
  }

  /** Перехватывает ЛЮБОЕ закрытие (крестик/Esc/клик вне модалки — Obsidian
   * прогоняет их все через close()) — без этого правки терялись без
   * предупреждения при случайном закрытии (прямой запрос пользователя,
   * 2026-08-23: "нет защиты от выхода без сохранения, нужно сделать"). */
  close(): void {
    if (this.handle?.isDirty()) {
      new UnsavedChangesModal(
        this.app,
        () => { void this.handle!.save().then(ok => { if (ok) super.close(); }); },
        () => super.close(),
      ).open();
      return;
    }
    super.close();
  }

  onClose(): void {
    this.contentEl.empty();
  }
}

/** Три варианта при попытке закрыть конфигуратор метода с несохранёнными
 * правками (2026-08-23) — сохранить/закрыть без сохранения/остаться. */
class UnsavedChangesModal extends Modal {
  private onSave: () => void;
  private onDiscard: () => void;

  constructor(app: App, onSave: () => void, onDiscard: () => void) {
    super(app);
    this.onSave = onSave;
    this.onDiscard = onDiscard;
  }

  onOpen(): void {
    this.titleEl.setText('Есть несохранённые изменения');
    this.contentEl.createDiv({ cls: 'tn-lims-meta' }).setText(
      'Сохранить изменения в конфигурации метода перед закрытием?',
    );
    const row = this.contentEl.createDiv({ cls: 'tn-lims-flex tn-lims-mt8' });
    const saveBtn = row.createEl('button', { text: '💾 Сохранить и закрыть', cls: 'tn-btn tn-btn-primary' });
    saveBtn.addEventListener('click', () => { this.close(); this.onSave(); });
    const discardBtn = row.createEl('button', { text: 'Закрыть без сохранения', cls: 'tn-btn tn-btn-ghost' });
    discardBtn.addEventListener('click', () => { this.close(); this.onDiscard(); });
    const cancelBtn = row.createEl('button', { text: 'Отмена', cls: 'tn-btn tn-btn-ghost' });
    cancelBtn.addEventListener('click', () => this.close());
  }

  onClose(): void {
    this.contentEl.empty();
  }
}