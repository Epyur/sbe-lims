import QRCode from 'qrcode';

/** Рисует QR с номером заявки/кодом оборудования в canvas (деталь заявки,
 * карточка оборудования). QR кодирует ПРОСТОЙ ТЕКСТ (тот же номер/код, что
 * напечатан подписью под ним) — не obsidian://-диплинк: Obsidian mobile не
 * имеет доступа к камере, поэтому сканирует внешнее приложение телефона, а
 * испытатель сам вставляет скопированный номер/код в текстовое поле ручного
 * ввода в sbe-lims-mobile (2026-08-27, по прямому запросу пользователя —
 * открытие плагина по ссылке из QR не сработало и было убрано). */
export async function renderMobileQr(canvas: HTMLCanvasElement, data: string): Promise<void> {
  await QRCode.toCanvas(canvas, data, { width: 160, margin: 1 });
}

/** dataURL QR с номером/кодом (для печатной этикетки оборудования). */
export function mobileQrDataUrl(data: string): Promise<string> {
  return QRCode.toDataURL(data, { width: 240, margin: 1 });
}
