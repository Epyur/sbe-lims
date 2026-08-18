import { Plugin, WorkspaceLeaf } from 'obsidian';
import { LimsSyncService } from './services/sync.service';
import { LimsView, SBE_LIMS_VIEW_TYPE } from './ui/lims-view';
import { LimsSettingsTab } from './ui/settings-tab';
import { publishService, unpublishService } from '../../sbe-core/src/bridge';
import type { SbeLimsApi } from '../../sbe-core/src/types';
import type { LabMethod } from './types/lims';

export interface SbeLimsSettings {
  apiUrl: string;
}

const DEFAULT_SETTINGS: SbeLimsSettings = {
  apiUrl: 'https://epyur.fvds.ru',
};

export default class SbeLimsPlugin extends Plugin {
  settings!: SbeLimsSettings;
  syncService!: LimsSyncService;
  /** Кэш методов (из pull) для отображения. */
  methods: LabMethod[] = [];

  async onload(): Promise<void> {
    await this.loadSettings();
    this.syncService = new LimsSyncService(() => this.settings.apiUrl);

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
  }

  onunload(): void {
    unpublishService('sbe-lims');
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
