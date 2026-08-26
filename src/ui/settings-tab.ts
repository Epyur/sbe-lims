import { App, Notice, PluginSettingTab, Setting } from 'obsidian';
import type SbeLimsPlugin from '../main';
import { getService } from '../../../sbe-core/src/bridge';
import { errorMessage } from '../../../sbe-core/src/utils/errors';

/** Ключи env почтового приёма lab-service, управляемые через ЦУП (auth-service
 * /auth/apps/env, белый список — на сервере, env_admin.go). Пароль сюда НЕ входит —
 * он рендерится/собирается отдельно (никогда не подтягивается с сервера обратно). */
const LAB_MAIL_ENV_KEYS = [
  'LAB_MAIL_ENABLED',
  'LAB_MAIL_IMAP_SERVER',
  'LAB_MAIL_LOGIN',
  'LAB_MAIL_PASSWORD',
  'LAB_MAIL_POLL_INTERVAL_SECONDS',
  'LAB_MAIL_METHOD_MAP',
] as const;

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

    new Setting(containerEl)
      .setHeading()
      .setName('Приём результатов по email (администратор)');

    const mailDiv = containerEl.createDiv({ cls: 'tn-lims-meta' });
    mailDiv.setText('Загрузка…');
    void this.renderMailSettings(mailDiv);
  }

  /** Учётка почты (IMAP), с которой lab-service принимает письма-результаты —
   * значения уходят на сервер в .env через общий admin-механизм ЦУП
   * (auth-service /auth/apps/env → очередь → хост-скрипт secret-applier.sh
   * пишет .env и пересоздаёт контейнер lab), не хранятся ни в вольте, ни в
   * data.json плагина, ни где-либо в клиенте после отправки. Раздел виден
   * только admin/superadmin (тот же гейт, что «Права доступа» выше). */
  private async renderMailSettings(container: HTMLElement): Promise<void> {
    try {
      const me = await this.plugin.syncService.getMyPermission();
      if (!me.hasAccess || (me.role !== 'admin' && me.role !== 'superadmin')) {
        container.setText('Доступно только администратору.');
        return;
      }
      container.empty();

      const apstore = await getService('sbe-apstore');
      const status = await apstore.auth.getAppEnvStatus('lab');
      const byKey = new Map(status.keys.map(k => [k.key, k]));

      const statusEl = container.createDiv({ cls: 'tn-lims-mb12' });
      const describeKey = (key: string, label: string): string => {
        const st = byKey.get(key);
        if (!st) return `${label}: —`;
        if (st.pending) return `${label}: ⏳ применяется…`;
        if (st.set) return `${label}: ✓ задано${st.updatedAt ? ` (${new Date(st.updatedAt).toLocaleString('ru-RU')})` : ''}`;
        return `${label}: не задано`;
      };
      statusEl.createEl('div', { text: describeKey('LAB_MAIL_ENABLED', 'Приём включён'), cls: 'tn-muted' });
      statusEl.createEl('div', { text: describeKey('LAB_MAIL_IMAP_SERVER', 'IMAP-сервер'), cls: 'tn-muted' });
      statusEl.createEl('div', { text: describeKey('LAB_MAIL_LOGIN', 'Логин'), cls: 'tn-muted' });
      statusEl.createEl('div', { text: describeKey('LAB_MAIL_PASSWORD', 'Пароль'), cls: 'tn-muted' });

      let enabled = byKey.get('LAB_MAIL_ENABLED')?.set ?? false;
      let imapServer = '';
      let login = '';
      let password = '';
      let pollSeconds = '';
      let methodMap = '';

      new Setting(container)
        .setName('Включить приём почты')
        .setDesc('Постоянный опрос почтового ящика — сервис пересоздаётся после сохранения.')
        .addToggle(t => t.setValue(enabled).onChange(v => { enabled = v; }));

      new Setting(container)
        .setName('IMAP-сервер')
        .setDesc('Например: imap.yandex.ru:993')
        .addText(t => t.setPlaceholder('imap.yandex.ru:993').onChange(v => { imapServer = v.trim(); }));

      new Setting(container)
        .setName('Логин почты')
        .addText(t => t.setPlaceholder('lpitn@yandex.ru').onChange(v => { login = v.trim(); }));

      new Setting(container)
        .setName('Пароль почты')
        .setDesc('Оставьте пустым, чтобы не менять уже сохранённый пароль.')
        .addText(t => {
          t.inputEl.type = 'password';
          t.setPlaceholder('••••••••').onChange(v => { password = v; });
        });

      new Setting(container)
        .setName('Интервал опроса, сек')
        .addText(t => t.setPlaceholder('120').onChange(v => { pollSeconds = v.trim(); }));

      new Setting(container)
        .setName('Карта методов (JSON)')
        .setDesc('Код метода из письма → id метода, например {"method1":1}. Оставьте пустым, чтобы не менять.')
        .addTextArea(t => {
          t.setPlaceholder('{"method1":1}').onChange(v => { methodMap = v.trim(); });
          t.inputEl.rows = 3;
        });

      new Setting(container).addButton(b => b
        .setButtonText('💾 Сохранить и применить')
        .setCta()
        .onClick(async () => {
          const values: Record<string, string> = { LAB_MAIL_ENABLED: enabled ? 'true' : 'false' };
          if (imapServer) values.LAB_MAIL_IMAP_SERVER = imapServer;
          if (login) values.LAB_MAIL_LOGIN = login;
          if (password) values.LAB_MAIL_PASSWORD = password;
          if (pollSeconds) values.LAB_MAIL_POLL_INTERVAL_SECONDS = pollSeconds;
          if (methodMap) {
            try {
              JSON.parse(methodMap);
            } catch {
              new Notice('Карта методов — некорректный JSON');
              return;
            }
            values.LAB_MAIL_METHOD_MAP = methodMap;
          }
          try {
            await apstore.auth.setAppEnv('lab', values);
            new Notice('Изменения поставлены в очередь — применятся в течение минуты (сервис перезапустится).');
            this.display();
          } catch (e: unknown) {
            new Notice(`Ошибка: ${errorMessage(e)}`);
          }
        }));
    } catch (e: unknown) {
      container.setText(`Не удалось загрузить настройки почты: ${errorMessage(e)}`);
    }
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
