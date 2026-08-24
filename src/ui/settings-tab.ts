import { App, Notice, PluginSettingTab, Setting } from 'obsidian';
import type SbeLimsPlugin from '../main';

export class LimsSettingsTab extends PluginSettingTab {
  plugin: SbeLimsPlugin;

  constructor(app: App, plugin: SbeLimsPlugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();

    new Setting(containerEl).setHeading().setName('Сервер');

    new Setting(containerEl)
      .setName('Адрес сервера (apiUrl)')
      .setDesc('База URL lab-service, например https://epyur.fvds.ru. JWT берётся из ЦУП СБЕ.')
      .addText(text => text
        .setPlaceholder('https://epyur.fvds.ru')
        .setValue(this.plugin.settings.apiUrl)
        .onChange(async (value) => {
          this.plugin.settings.apiUrl = value.trim();
          await this.plugin.saveSettings();
        }));

    new Setting(containerEl)
      .setName('Модель LLM')
      .setDesc('Модель для ИИ-помощника формул и черновика конфигурации из стандарта (используется sbe-llm). Пусто — модель по умолчанию.')
      .addText(text => text
        .setPlaceholder('gpt-5.6-luna')
        .setValue(this.plugin.settings.llmModel)
        .onChange(async (value) => {
          this.plugin.settings.llmModel = value.trim();
          await this.plugin.saveSettings();
        }));

    new Setting(containerEl)
      .setHeading()
      .setName('Права доступа');

    const permsDiv = containerEl.createDiv({ cls: 'tn-lims-meta' });
    permsDiv.setText('Загрузка…');
    void this.renderPermissions(permsDiv);
  }

  private async renderPermissions(container: HTMLElement): Promise<void> {
    try {
      const me = await this.plugin.syncService.getMyPermission();
      if (!me.hasAccess) {
        container.setText('Нет доступа к серверу. Запросите ключ в ЦУП и получите доступ у администратора.');
        return;
      }
      container.setText(`Ваша роль: ${me.role || '—'}. Для доступа к ЛИМС нужна роль сотрудника лаборатории (lab_members).`);
      if (me.role !== 'admin' && me.role !== 'superadmin') return;
      container.empty();
      const members = await this.plugin.syncService.listLabMembers();
      const table = container.createEl('table', { cls: 'tn-table' });
      const thead = table.createEl('thead').createEl('tr');
      thead.createEl('th').setText('Лаборатория');
      thead.createEl('th').setText('Email');
      thead.createEl('th').setText('Роль');
      thead.createEl('th').setText('');
      const tbody = table.createEl('tbody');
      for (const m of members) {
        const tr = tbody.createEl('tr');
        tr.createEl('td').setText(String(m.lab_id));
        tr.createEl('td').setText(m.email);
        const roleCell = tr.createEl('td');
        const roleSelect = roleCell.createEl('select', { cls: 'tn-lims-input' });
        roleSelect.createEl('option', { value: 'lab_operator', text: 'Сотрудник' });
        roleSelect.createEl('option', { value: 'lab_admin', text: 'Админ лабы' });
        roleSelect.createEl('option', { value: 'lab_auditor', text: 'Аудитор (только чтение)' });
        roleSelect.value = m.role;
        roleSelect.addEventListener('change', async () => {
          try {
            await this.plugin.syncService.setLabMember(m.lab_id, m.email, roleSelect.value);
            new Notice(`Роль ${m.email} обновлена`);
          } catch (e: unknown) {
            new Notice(`Ошибка: ${e instanceof Error ? e.message : String(e)}`);
          }
        });
        const removeBtn = tr.createEl('td').createEl('button', { text: '✖', cls: 'tn-btn tn-btn-ghost' });
        removeBtn.addEventListener('click', async () => {
          if (!window.confirm(`Убрать «${m.email}» из лаборатории ${m.lab_id}?`)) return;
          try {
            await this.plugin.syncService.removeLabMember(m.lab_id, m.email);
            new Notice('Сотрудник удалён');
            this.display();
          } catch (e: unknown) {
            new Notice(`Ошибка: ${e instanceof Error ? e.message : String(e)}`);
          }
        });
      }
      const addRow = tbody.createEl('tr');
      const labCell = addRow.createEl('td');
      const labInput = labCell.createEl('input', { attr: { type: 'number', min: '1' }, cls: 'tn-lims-input' });
      const emailCell = addRow.createEl('td');
      const emailInput = emailCell.createEl('input', { attr: { type: 'text', placeholder: 'email@tn.ru' }, cls: 'tn-lims-input' });
      const roleCell = addRow.createEl('td');
      const roleSelect = roleCell.createEl('select', { cls: 'tn-lims-input' });
      roleSelect.createEl('option', { value: 'lab_operator', text: 'Сотрудник' });
      roleSelect.createEl('option', { value: 'lab_admin', text: 'Админ лабы' });
      roleSelect.createEl('option', { value: 'lab_auditor', text: 'Аудитор (только чтение)' });
      const addCell = addRow.createEl('td');
      const addBtn = addCell.createEl('button', { text: '➕', cls: 'tn-btn tn-btn-primary' });
      addBtn.addEventListener('click', async () => {
        const labId = Number(labInput.value);
        const email = emailInput.value.trim();
        if (!labId || !email) { new Notice('Введите lab_id и email'); return; }
        try {
          await this.plugin.syncService.setLabMember(labId, email, roleSelect.value);
          new Notice(`Сотрудник добавлен в лабораторию ${labId}`);
          this.display();
        } catch (e: unknown) {
          new Notice(`Ошибка: ${e instanceof Error ? e.message : String(e)}`);
        }
      });
    } catch (e: unknown) {
      container.setText(`Не удалось загрузить права: ${e instanceof Error ? e.message : String(e)}`);
    }
  }
}
