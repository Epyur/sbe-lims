import { requestUrl, RequestUrlParam } from 'obsidian';
import { getService } from '../../../sbe-core/src/bridge';
import { errorMessage } from '../../../sbe-core/src/utils/errors';
import type {
  AggregatedResult,
  Equipment,
  EquipmentCalibration,
  EquipmentDocument,
  EquipmentLink,
  EquipmentMethodLink,
  Inventor,
  Lab,
  LabGroup,
  LabMember,
  LabObject,
  LabProject,
  LimsRequest,
  MeasurementResult,
  MethodConfig,
  PresentationKind,
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
  /** Kanban-доска «Очередь лаборатории»: смена статуса и/или назначение испытателя
   * (см. server_back/lab-service/kanban.go, handleKanbanMove) — единая точка входа
   * и для перетаскивания карточки, и для контролов в детали заявки; сервер сам
   * проверяет ролевые правила (руководитель — свободно, испытатель — только своё). */
  async moveKanbanCard(requestId: number, patch: { status?: string; assigned_to?: string }): Promise<LimsRequest> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/kanban-move`,
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify(patch),
    });
    this.assertOk(res);
    try {
      return (JSON.parse(res.text) as { request: LimsRequest }).request;
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе kanban-move:', errorMessage(e));
      throw new Error('Сервер вернул не JSON при перемещении карточки');
    }
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
    data: Partial<{
      code: string; name: string; location: string; responsible: string; status: string;
      commissioned_at: string; service_life: string;
      verification_cert_number: string; verification_cert_date: string;
      verification_act_number: string; verification_act_date: string;
      calibration_interval_months: number;
    }>,
  ): Promise<void> {
    await this.patchEntity(`/api/lab/equipment/${id}`, data);
  }

  async deleteEquipment(id: number): Promise<void> {
    await this.deleteEntity(`/api/lab/equipment/${id}`);
  }

  /** Скан сертификата/акта поверки — обновляет соответствующую пару file_key/file_url. */
  async uploadEquipmentScan(id: number, kind: 'verification_cert' | 'verification_act', data: ArrayBuffer, fileName: string): Promise<void> {
    const token = await this.getToken();
    const boundary = this.multipartBoundary();
    const body = this.buildMultipart(boundary, {}, { field: 'file', data, fileName });
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/scan?kind=${kind}`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': `multipart/form-data; boundary=${boundary}` },
      body,
    }, 120000);
    this.assertOk(res);
  }

  async listEquipmentCalibrations(id: number): Promise<EquipmentCalibration[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/calibrations`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { calibrations?: EquipmentCalibration[] };
      return Array.isArray(data.calibrations) ? data.calibrations : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе equipment/calibrations:', errorMessage(e));
      return [];
    }
  }

  /** Новая запись журнала калибровок — сервер сам пересчитывает last_calibration/
   * next_calibration оборудования. Файл необязателен. */
  async createEquipmentCalibration(
    id: number,
    data: {
      calibratedAt: string; methodId?: number; ambTemp?: string; ambPres?: string; ambMoist?: string;
      values?: Record<string, unknown>; result?: string;
    },
    file?: { data: ArrayBuffer; fileName: string },
  ): Promise<void> {
    const token = await this.getToken();
    const boundary = this.multipartBoundary();
    const fields: Record<string, string> = {
      calibrated_at: data.calibratedAt,
      amb_temp: data.ambTemp || '',
      amb_pres: data.ambPres || '',
      amb_moist: data.ambMoist || '',
      result: data.result || '',
      values: JSON.stringify(data.values || {}),
    };
    if (data.methodId) fields.method_id = String(data.methodId);
    const body = this.buildMultipart(
      boundary,
      fields,
      file ? { field: 'file', data: file.data, fileName: file.fileName } : undefined,
    );
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/calibrations`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': `multipart/form-data; boundary=${boundary}` },
      body,
    }, 120000);
    this.assertOk(res);
  }

  async listEquipmentMethods(id: number): Promise<EquipmentMethodLink[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/methods`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { methods?: EquipmentMethodLink[] };
      return Array.isArray(data.methods) ? data.methods : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе equipment/methods:', errorMessage(e));
      return [];
    }
  }

  async setEquipmentMethod(id: number, methodId: number, role: 'main' | 'auxiliary'): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/methods`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ method_id: methodId, role }),
    });
    this.assertOk(res);
  }

  async deleteEquipmentMethod(id: number, methodId: number): Promise<void> {
    await this.deleteEntity(`/api/lab/equipment/${id}/methods/${methodId}`);
  }

  /** Все связи оборудование↔оборудование одним запросом (не по одной единице —
   * общий список строит из этого множество "скрыть из верхнего уровня" и вложенные
   * списки без N+1 запроса на каждую карточку). */
  async listAllEquipmentLinks(): Promise<EquipmentLink[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment-links`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { links?: EquipmentLink[] };
      return Array.isArray(data.links) ? data.links : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе equipment-links:', errorMessage(e));
      return [];
    }
  }

  /** mainId становится ОСНОВНЫМ для auxiliaryId. Один и тот же вызов обслуживает
   * оба направления UI ("привязать вспомогательный к этому основному" и "привязать
   * этот вспомогательный к основному" — во втором случае auxiliaryId = собственный
   * id прибора со своей карточки, mainId — выбранный основной). */
  async addEquipmentAuxiliary(mainId: number, auxiliaryId: number): Promise<void> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${mainId}/auxiliaries`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ auxiliary_equipment_id: auxiliaryId }),
    });
    this.assertOk(res);
  }

  async removeEquipmentAuxiliary(mainId: number, auxiliaryId: number): Promise<void> {
    await this.deleteEntity(`/api/lab/equipment/${mainId}/auxiliaries/${auxiliaryId}`);
  }

  async listEquipmentDocuments(id: number): Promise<EquipmentDocument[]> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/documents`,
      method: 'GET',
      headers: { Authorization: `Bearer ${token}` },
    });
    this.assertOk(res);
    try {
      const data = JSON.parse(res.text) as { documents?: EquipmentDocument[] };
      return Array.isArray(data.documents) ? data.documents : [];
    } catch (e: unknown) {
      console.warn('ЛИМС: не JSON в ответе equipment/documents:', errorMessage(e));
      return [];
    }
  }

  async uploadEquipmentDocument(id: number, data: ArrayBuffer, fileName: string): Promise<void> {
    const token = await this.getToken();
    const boundary = this.multipartBoundary();
    const body = this.buildMultipart(boundary, {}, { field: 'file', data, fileName });
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/equipment/${id}/documents`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': `multipart/form-data; boundary=${boundary}` },
      body,
    }, 120000);
    this.assertOk(res);
  }

  async deleteEquipmentDocument(id: number, fileId: number): Promise<void> {
    await this.deleteEntity(`/api/lab/equipment/${id}/documents/${fileId}`);
  }

  private multipartBoundary(): string {
    return '----sbe-lims-' + Date.now().toString(36);
  }

  /** Собирает multipart/form-data: текстовые поля + опциональный файл. */
  private buildMultipart(
    boundary: string,
    fields: Record<string, string>,
    file?: { field: string; data: ArrayBuffer; fileName: string },
  ): ArrayBuffer {
    const enc = new TextEncoder();
    const parts: Uint8Array[] = [];
    for (const [name, value] of Object.entries(fields)) {
      parts.push(enc.encode(`--${boundary}\r\nContent-Disposition: form-data; name="${name}"\r\n\r\n${value}\r\n`));
    }
    if (file) {
      parts.push(enc.encode(
        `--${boundary}\r\nContent-Disposition: form-data; name="${file.field}"; filename="${file.fileName}"\r\nContent-Type: application/octet-stream\r\n\r\n`,
      ));
      parts.push(new Uint8Array(file.data));
      parts.push(enc.encode('\r\n'));
    }
    parts.push(enc.encode(`--${boundary}--\r\n`));

    let total = 0;
    for (const p of parts) total += p.byteLength;
    const out = new Uint8Array(total);
    let off = 0;
    for (const p of parts) {
      out.set(p, off);
      off += p.byteLength;
    }
    return out.buffer;
  }

  /** Без labId — полный список (только для руководителя лабы, «Настройки»). С
   * labId — ростер одной лабы, доступен любому её участнику (Kanban-доска). */
  async listLabMembers(labId?: number): Promise<LabMember[]> {
    const token = await this.getToken();
    const qs = labId ? `?lab_id=${labId}` : '';
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/lab-members${qs}`,
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

  /** kind — вид вывода: "ui" (краткий), "excerpt" (выписка) или "protocol"
   * (полный, по умолчанию — совпадает со старым поведением без выбора).
   * format="html" — не тратить время на сборку DOCX (для карточки
   * результатов/предпросмотра, docx_base64 в ответе будет пустой строкой). */
  async getProtocol(
    requestId: number,
    kind: PresentationKind = 'protocol',
    format: 'html' | 'full' = 'full',
  ): Promise<ProtocolResponse> {
    const token = await this.getToken();
    const res = await this.request({
      url: `${this.baseUrl}/api/lab/requests/${requestId}/protocol?template=${kind}&format=${format}`,
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
