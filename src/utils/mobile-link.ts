import type { App } from 'obsidian';
import QRCode from 'qrcode';

export type MobileLinkKind = 'request' | 'equipment';

/** obsidian://-диплинк на форму мобильного плагина sbe-lims-mobile (сканирование
 * QR штатной камерой телефона). QR — просто указатель на ресурс, без токена:
 * доступ проверяет сервер по JWT сканирующего испытателя (requireLabAccess в
 * lab-service), как и везде в проекте — знание id само по себе доступа не даёт. */
export function buildMobileDeepLink(app: App, kind: MobileLinkKind, id: number): string {
  const vault = encodeURIComponent(app.vault.getName());
  const action = kind === 'request' ? 'result' : 'calibrate';
  const param = kind === 'request' ? 'request' : 'equipment';
  return `obsidian://sbe-lims-mobile?vault=${vault}&action=${action}&${param}=${id}`;
}

/** Рисует QR диплинка в canvas (для показа в детали заявки/карточке оборудования). */
export async function renderMobileQr(canvas: HTMLCanvasElement, data: string): Promise<void> {
  await QRCode.toCanvas(canvas, data, { width: 160, margin: 1 });
}

/** dataURL QR диплинка (для печатной этикетки оборудования). */
export function mobileQrDataUrl(data: string): Promise<string> {
  return QRCode.toDataURL(data, { width: 240, margin: 1 });
}
