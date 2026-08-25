import { Plugin, WorkspaceLeaf } from 'obsidian';
import { LimsSyncService } from './services/sync.service';
import { LimsLlmAssist } from './services/llm-assist.service';
import { LimsView, SBE_LIMS_VIEW_TYPE } from './ui/lims-view';
import { LimsSettingsTab } from './ui/settings-tab';
import { getService, publishService, unpublishService } from '../../sbe-core/src/bridge';
import { errorMessage } from '../../sbe-core/src/utils/errors';
import type { SbeLimsApi } from '../../sbe-core/src/types';
import type { LabMethod } from './types/lims';

export interface SbeLimsSettings {
  apiUrl: string;
  /** Модель для sbe-llm (сам sbe-llm модель не хранит — её выбирает каждый
   * потребитель, см. llmModel в sbe-mailer/sbe-presentations). Без модели
   * шлюз chadgpt.ru может отвечать не тем форматом, который ждёт клиент. */
  llmModel: string;
  /** Последняя версия, о которой опубликована новость в ЦУП (2026-08-22,
   * см. announceUpdate ниже) — не даёт публиковать новость на каждый запуск. */
  lastAnnouncedVersion?: string;
}

const DEFAULT_SETTINGS: SbeLimsSettings = {
  apiUrl: 'https://epyur.fvds.ru',
  llmModel: 'gpt-5.6-luna',
};

export default class SbeLimsPlugin extends Plugin {
  settings!: SbeLimsSettings;
  syncService!: LimsSyncService;
  llmAssist!: LimsLlmAssist;
  /** Кэш методов (из pull) для отображения. */
  methods: LabMethod[] = [];

  async onload(): Promise<void> {
    await this.loadSettings();
    this.syncService = new LimsSyncService(() => this.settings.apiUrl);
    this.llmAssist = new LimsLlmAssist(() => this.settings.llmModel);

    this.registerView(SBE_LIMS_VIEW_TYPE, (leaf: WorkspaceLeaf) => new LimsView(leaf, this));
    this.addSettingTab(new LimsSettingsTab(this.app, this));

    await this.refreshMethods();

    publishService<SbeLimsApi>('sbe-lims', {
      open: async () => {
        await this.activateView();
      },
    }, {
      version: this.manifest.version,
      name: this.manifest.name,
    });

    if (this.settings.lastAnnouncedVersion !== this.manifest.version) {
      void this.announceUpdateSafely(
        'Графики в заявках доработаны: блок "Графики" больше не показывается там, где для ' +
        'него ещё нет данных (раньше висела пустая сломанная картинка). Подписи левой и правой ' +
        'осей Y теперь читаются как положено — сбоку от шкалы, повёрнутые по вертикали, — и у ' +
        'делений появились числа (раньше были только линии сетки без значений). В конструкторе ' +
        'метода для каждой оси графика можно отдельно задать название, точку начала отсчёта и ' +
        'шаг деления шкалы — например, чтобы шкала температуры начиналась строго с нуля, а не ' +
        'уходила в минус там, где отрицательных значений нет.',
      );
    }
  }

  onunload(): void {
    unpublishService('sbe-lims');
  }

  /** Публикует в ЦУП («Новости») сообщение об обновлении плагина — один раз на
   * версию (см. правило в корневом AGENTS.md, добавлено 2026-08-22). Никогда
   * не должно мешать загрузке плагина, если ЦУП недоступен. */
  private async announceUpdateSafely(summary: string): Promise<void> {
    try {
      const apstore = await getService('sbe-apstore');
      await apstore.announceUpdate({
        appId: this.manifest.id,
        appName: this.manifest.name,
        version: this.manifest.version,
        summary,
      });
      this.settings.lastAnnouncedVersion = this.manifest.version;
      await this.saveSettings();
    } catch (e: unknown) {
      console.warn(`${this.manifest.name}: не удалось опубликовать новость об обновлении:`, errorMessage(e));
    }
  }

  /** Загружает методы из pull (для конфигов/отображения). */
  async refreshMethods(): Promise<void> {
    try {
      const token = await this.syncService.token();
      const res = await this.requestApi(token, '/api/lab/sync/pull');
      const data = JSON.parse(res.text) as { methods?: LabMethod[] };
      this.methods = Array.isArray(data.methods) ? data.methods : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не удалось загрузить методы:', e);
    }
  }

  private async requestApi(token: string, path: string): Promise<{ status: number; text: string }> {
    // используем requestUrl через syncService (обёрнутый таймаут)
    return this.syncService.rawRequest(`${this.syncService.baseUrl}${path}`, token);
  }

  async loadSettings(): Promise<void> {
    const data = (await this.loadData() as Partial<SbeLimsSettings>) || {};
    this.settings = Object.assign({}, DEFAULT_SETTINGS, data);
  }

  async saveSettings(): Promise<void> {
    await this.saveData(this.settings);
  }

  async activateView(): Promise<void> {
    const { workspace } = this.app;
    const existing = workspace.getLeavesOfType(SBE_LIMS_VIEW_TYPE)[0];
    if (existing) {
      workspace.revealLeaf(existing);
      return;
    }
    const leaf = workspace.getLeaf(false);
    await leaf.setViewState({ type: SBE_LIMS_VIEW_TYPE, active: true });
    workspace.revealLeaf(leaf);
  }
}
