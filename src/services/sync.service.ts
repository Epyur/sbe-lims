import { requestUrl, RequestUrlParam } from 'obsidian';
import { getService } from '../../../sbe-core/src/bridge';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import type {
  AggregatedResult,
  Equipment,
  Inventor,
  Lab,
  LabGroup,
  LabMember,
  LabObject,
  LabProject,
  LimsRequest,
  MeasurementResult,
  MethodConfig,
  ProtocolResponse,
} from '../types/lims';

/** Клиент lab-service для ЛИМС через JWT из ЦУП. */
export class LimsSyncService {
  private getApiUrl: () => string;

  constructor(getApiUrl: () => string) {
    this.getApiUrl = getApiUrl;
  }

  get baseUrl(): string {
    return this.getApiUrl().trim().replace(/\/+$/, '');
  }

  private async getToken(): Promise<string> {
    const apstore = await getService('sbe-apstore');
    return apstore.auth.getToken('lab');
  }

  /** Публичный доступ к токену (для main.refreshMethods). */
  async token(): Promise<string> {
    return this.getToken();
  }

  /** Прямой запрос с таймаутом (для pull в main). */
  async rawRequest(url: string, token: string): Promise<{ status: number; text: string }> {
    const res = await this.request({
      url,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    return res;
  }

  // ---- Заявки (видимые лаборатории) ----

  /** Роль текущего пользователя. */
  async getMyPermission(): Promise<{ email: string; role: string; hasAccess: boolean }> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/permissions/me`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      return JSON.parse(res.text) as { email: string; role: string; hasAccess: boolean };
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе permissions/me:', errorMessage(e));
      return { email: '', role: '', hasAccess: false };
    }
  }

  /** Заявки, доступные текущему пользователю. */
  async listRequests(): Promise<LimsRequest[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { requests?: LimsRequest[] };
      return Array.isArray(data.requests) ? data.requests : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе requests:', errorMessage(e));
      return [];
    }
  }

  // ---- Результаты ----

  async listResults(requestId: number): Promise<MeasurementResult[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/results`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { results?: MeasurementResult[] };
      return Array.isArray(data.results) ? data.results : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе results:', errorMessage(e));
      return [];
    }
  }

  /** Сохраняет серию (создание/обновление). */
  async saveResult(
    requestId: number,
    data: { method_id: number; inventor_id: number; series_num: number; values: Record<string, unknown>; photo_before?: string; photo_after?: string },
  ): Promise<{ id: number; series_num: number; values: Record<string, unknown> }> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/results`,
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify(data),
    });
    this.assertOk(res);
    try {
      return JSON.parse(res.text) as { id: number; series_num: number; values: Record<string, unknown> };
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе saveResult:', errorMessage(e));
      throw new Error('Сервер вернул не JSON при сохранении результата');
    }
  }

  async listAggregated(requestId: number): Promise<AggregatedResult[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/results/aggregated`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { aggregated?: AggregatedResult[] };
      return Array.isArray(data.aggregated) ? data.aggregated : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе aggregated:', errorMessage(e));
      return [];
    }
  }

  /** Пересчитывает формулы/классификацию серии. */
  async calculateSeries(requestId: number, seriesNum: number): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/results/${seriesNum}/calculate`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
  }

  /** Меняет статус заявки (received/processing/completed). */
  async setStatus(requestId: number, status: string): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/status`,
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify({ status }),
    });
    this.assertOk(res);
  }

  // ---- Справочники ----

  /** Лаборатории (для переключателя в шапке фасада). */
  async listLabs(): Promise<Lab[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/labs`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { labs?: Lab[] };
      return Array.isArray(data.labs) ? data.labs : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе labs:', errorMessage(e));
      return [];
    }
  }

  /** Создаёт лабораторию (superadmin). Внешняя (type='external') обязана указать
   * parent_lab_id существующей внутренней лабы — сервер иначе откажет (400). */
  async createLab(data: {
    code: string;
    name: string;
    description?: string;
    type?: string;
    parent_lab_id?: number;
  }): Promise<number> {
    return this.createEntity('/api/lab/labs', data);
  }

  /** Правит лабораторию (superadmin), частичный PATCH. Та же валидация type/parent_lab_id,
   * что при создании — сервер откажет несогласованной комбинацией (400). */
  async updateLab(
    id: number,
    data: Partial<{ code: string; name: string; description: string; type: string; parent_lab_id: number }>,
  ): Promise<void> {
    await this.patchEntity(`/api/lab/labs/${id}`, data);
  }

  /** Объекты исследования (только чтение — создание в sbe-requests). */
  async listObjects(): Promise<LabObject[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/objects`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { objects?: LabObject[] };
      return Array.isArray(data.objects) ? data.objects : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе objects:', errorMessage(e));
      return [];
    }
  }

  /** Проекты (только чтение — создание/правка в sbe-requests); нужны здесь
   * только для отображения деталей заявки, как у заявителя. */
  async listProjects(): Promise<LabProject[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/projects`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { projects?: LabProject[] };
      return Array.isArray(data.projects) ? data.projects : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе projects:', errorMessage(e));
      return [];
    }
  }

  /** Группы (только чтение — создание в sbe-requests); нужны здесь только
   * для отображения деталей заявки, как у заявителя. */
  async listGroups(): Promise<LabGroup[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/groups`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { groups?: LabGroup[] };
      return Array.isArray(data.groups) ? data.groups : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе groups:', errorMessage(e));
      return [];
    }
  }

  async listInventors(): Promise<Inventor[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/inventors`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { inventors?: Inventor[] };
      return Array.isArray(data.inventors) ? data.inventors : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе inventors:', errorMessage(e));
      return [];
    }
  }

  async createInventor(data: { name: string; email: string; phone?: string; department?: string; position?: string }): Promise<number> {
    return this.createEntity('/api/lab/inventors', data);
  }

  async updateInventor(
    id: number,
    data: Partial<{ name: string; email: string; phone: string; department: string; position: string }>,
  ): Promise<void> {
    await this.patchEntity(`/api/lab/inventors/${id}`, data);
  }

  async deleteInventor(id: number): Promise<void> {
    await this.deleteEntity(`/api/lab/inventors/${id}`);
  }

  async listEquipment(): Promise<Equipment[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { equipment?: Equipment[] };
      return Array.isArray(data.equipment) ? data.equipment : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе equipment:', errorMessage(e));
      return [];
    }
  }

  async createEquipment(data: { code: string; name: string; location?: string; responsible?: string }): Promise<number> {
    return this.createEntity('/api/lab/equipment', data);
  }

  async updateEquipment(
    id: number,
    data: Partial<{ code: string; name: string; location: string; responsible: string }>,
  ): Promise<void> {
    await this.patchEntity(`/api/lab/equipment/${id}`, data);
  }

  async deleteEquipment(id: number): Promise<void> {
    await this.deleteEntity(`/api/lab/equipment/${id}`);
  }

  async listLabMembers(): Promise<LabMember[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/lab-members`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { members?: LabMember[] };
      return Array.isArray(data.members) ? data.members : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе lab-members:', errorMessage(e));
      return [];
    }
  }

  async setLabMember(labId: number, email: string, role: string): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/lab-members`,
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify({ lab_id: labId, email, role }),
    });
    this.assertOk(res);
  }

  async removeLabMember(labId: number, email: string): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/lab-members/${labId}/${encodeURIComponent(email)}`,
      method: 'DELETE',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
  }

  /** Создаёт метод (admin). lab_ids — метод теперь может принадлежать нескольким
   * лабораториям (2026-08-19, method_labs); сервер требует минимум одну. */
  async createMethod(data: {
    code: string;
    name: string;
    lab_ids: number[];
    description?: string;
    determinable_indicators?: string[];
  }): Promise<number> {
    return this.createEntity('/api/lab/methods', data);
  }

  /** Обновляет конфигурацию метода (admin): formulas/classification/chart_configs/
   * input_parameters + опционально lab_ids (если передан — полностью заменяет набор
   * лабораторий метода, минимум одна) + опционально description. */
  async updateMethodConfig(
    methodId: number,
    cfg: Partial<MethodConfig> & { lab_ids?: number[]; description?: string; determinable_indicators?: string[] },
  ): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/methods/${methodId}`,
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify(cfg),
    });
    this.assertOk(res);
  }

  /** Удаляет метод (admin). 409, если метод уже используется в заявках/справочниках. */
  async deleteMethod(methodId: number): Promise<void> {
    await this.deleteEntity(`/api/lab/methods/${methodId}`);
  }

  // ---- Графики / протокол / дашборд ----

  /** Возвращает URL графика (для <img src>). */
  chartUrl(requestId: number, cfgId: string): string {
    return `${this.baseUrl}/api/lab/requests/${requestId}/chart/${encodeURIComponent(cfgId)}`;
  }

  async getProtocol(requestId: number): Promise<ProtocolResponse> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/protocol`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      return JSON.parse(res.text) as ProtocolResponse;
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе protocol:', errorMessage(e));
      throw new Error('Сервер вернул не JSON при генерации протокола');
    }
  }

  // ---- Хелперы ----

  private async createEntity(path: string, body: Record<string, unknown>): Promise<number> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}${path}`,
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify(body),
    });
    this.assertOk(res);
    try {
      const parsed = JSON.parse(res.text) as { id?: number };
      return parsed.id || 0;
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе entity:', errorMessage(e));
      throw new Error('Сервер вернул не JSON при создании');
    }
  }

  private async patchEntity(path: string, body: Record<string, unknown>): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}${path}`,
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify(body),
    });
    this.assertOk(res);
  }

  private async deleteEntity(path: string): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}${path}`,
      method: 'DELETE',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
  }

  private assertOk(res: { status: number; text: string }): void {
    if (res.status === 401) throw new Error('Ключ доступа недействителен. Запросите новый ключ в ЦУП.');
    if (res.status === 403) throw new Error('Нет прав доступа к ЛИМС. Обратитесь к администратору.');
    if (res.status !== 200) throw new Error(this.errorText(res) || `Сервер вернул HTTP ${res.status}`);
  }

  private errorText(res: { status: number; text: string }): string {
    if (!res.text) return '';
    try {
      const data = JSON.parse(res.text) as { error?: string };
      return data.error || '';
    } catch (e: unknown) {
      console.warn('ЛИМС: ответ сервера не JSON:', errorMessage(e));
      return '';
    }
  }

  private async request(param: RequestUrlParam, timeoutMs = 30000): Promise<{ status: number; text: string }> {
    let timer: number | undefined;
    try {
      const response = await Promise.race([
        requestUrl({ ...param, throw: false }),
        new Promise<never>((_, reject) => {
          timer = window.setTimeout(
            () => reject(new Error(`Сервер не ответил за ${Math.round(timeoutMs / 1000)} сек`)),
            timeoutMs,
          );
        }),
      ]);
      return { status: response.status, text: response.text };
    } finally {
      if (timer !== undefined) window.clearTimeout(timer);
    }
  }
}
