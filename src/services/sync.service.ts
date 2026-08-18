import { requestUrl, RequestUrlParam } from 'obsidian';
import { getService } from '../../../sbe-core/src/bridge';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import type {
  AggregatedResult,
  DashboardData,
  Equipment,
  Inventor,
  LabMember,
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

  /** Обновляет конфигурацию метода (admin). */
  async updateMethodConfig(methodId: number, cfg: Partial<MethodConfig>): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/methods/${methodId}`,
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify(cfg),
    });
    this.assertOk(res);
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

  async getDashboard(period: string): Promise<DashboardData> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/dashboard?period=${encodeURIComponent(period)}`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      return JSON.parse(res.text) as DashboardData;
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе dashboard:', errorMessage(e));
      return { by_status: {}, by_method: [], total: 0, completed_in_period: 0, period };
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
