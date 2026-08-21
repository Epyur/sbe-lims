/** Извлечение читаемого текста из .rtf/.txt (импорт стандарта для ИИ-черновика
 * конфигуратора методов). Нет готовой RTF-библиотеки в проекте — конвертер
 * написан и проверен на реальных файлах (ГОСТ 30244-94/30402-96 из базы
 * Кодекс): {\rtf1\ansi\ansicpg1251...} с текстом через \'XX hex-escape.
 *
 * Известное ограничение (не устранимо парсингом текста): числовые значения в
 * некоторых таблицах этих документов встроены как WMF-картинки ({\pict
 * \wmetafile8 ...}), а не текст — такие ячейки в результате будут пустыми.
 * ИИ-черновик (llm-assist.service.ts) явно инструктируется не выдумывать эти
 * значения, а помечать их как требующие ручной проверки.
 */

const SKIP_GROUP_NAMES = new Set([
  'fonttbl', 'colortbl', 'stylesheet', 'info', 'header', 'footer',
  'headerf', 'footerf', 'pict', 'object', 'generator',
]);

function rtfToText(raw: string): string {
  const cpMatch = raw.match(/\\ansicpg(\d+)/);
  const codepage = cpMatch ? `windows-${cpMatch[1]}` : 'windows-1251';
  let decoder: TextDecoder;
  try {
    decoder = new TextDecoder(codepage);
  } catch {
    decoder = new TextDecoder('windows-1251');
  }

  const n = raw.length;
  let i = 0;
  let depth = 0;
  const skipDepths: number[] = [];
  const bytes: number[] = [];
  const isSkipping = (): boolean => skipDepths.length > 0 && depth >= skipDepths[skipDepths.length - 1];

  while (i < n) {
    const c = raw[i];
    if (c === '{') {
      depth++;
      if (raw[i + 1] === '\\') {
        let k = i + 2;
        while (k < n && /[a-zA-Z]/.test(raw[k])) k++;
        const word = raw.slice(i + 2, k);
        if (SKIP_GROUP_NAMES.has(word)) skipDepths.push(depth);
      }
      i++;
      continue;
    }
    if (c === '}') {
      if (skipDepths.length > 0 && skipDepths[skipDepths.length - 1] === depth) skipDepths.pop();
      depth--;
      i++;
      continue;
    }
    if (c === '\\') {
      // \'XX — байт исходной кодовой страницы
      if (raw[i + 1] === "'") {
        const hex = raw.slice(i + 2, i + 4);
        if (!isSkipping()) bytes.push(parseInt(hex, 16));
        i += 4;
        continue;
      }
      // \uN — прямой unicode-код-пойнт (редкие спецсимволы); следом по спеку
      // RTF идёт \ucN ascii-символов-фолбэков, которые нужно пропустить —
      // здесь берём 1 (самый частый случай, \uc1 в обоих образцах)
      if (raw[i + 1] === 'u' && /[0-9-]/.test(raw[i + 2] || '')) {
        let k = i + 2;
        if (raw[k] === '-') k++;
        while (k < n && /[0-9]/.test(raw[k])) k++;
        const code = parseInt(raw.slice(i + 2, k), 10);
        if (!isSkipping() && Number.isFinite(code)) {
          const ch = String.fromCodePoint(code < 0 ? 0x10000 + code : code);
          for (const b of new TextEncoder().encode(ch)) bytes.push(b);
        }
        if (raw[k] === ' ') k++;
        i = k + 1; // пропустить один ascii-фолбэк-символ
        continue;
      }
      // контрол-слово \wordNNN(опц. пробел)
      let j = i + 1;
      while (j < n && /[a-zA-Z]/.test(raw[j])) j++;
      const word = raw.slice(i + 1, j);
      let k = j;
      while (k < n && /[-0-9]/.test(raw[k])) k++;
      if (raw[k] === ' ') k++;
      if (!isSkipping()) {
        if (word === 'par' || word === 'line' || word === 'row') bytes.push(10);
        else if (word === 'tab' || word === 'cell') bytes.push(9);
      }
      i = k;
      continue;
    }
    if (c !== '\n' && c !== '\r') {
      if (!isSkipping()) bytes.push(c.charCodeAt(0) & 0xff);
    }
    i++;
  }

  const decoded = decoder.decode(new Uint8Array(bytes));
  return decoded.replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
}

/** extractStandardText — .rtf конвертируется через rtfToText, .txt/прочее
 * декодируется как UTF-8 без изменений. */
export function extractStandardText(buf: ArrayBuffer, fileName: string): string {
  const isRtf = fileName.toLowerCase().endsWith('.rtf');
  if (!isRtf) {
    return new TextDecoder('utf-8').decode(buf);
  }
  const raw = new TextDecoder('latin1').decode(buf); // 1 char = 1 byte, безопасно для \'XX
  return rtfToText(raw);
}
