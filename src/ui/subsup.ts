/** Палитра надстрочных/подстрочных символов для человекочитаемых названий,
 * подписей и т.п. на обычных plain `<input>` (2026-08-22, изначально —
 * название атрибута; 2026-08-24 — вынесено в общий модуль, чтобы переиспользовать
 * и для подписи колонки таблицы в block-editor.ts). HTML-инпут не умеет
 * rich-text `<sup>/<sub>`, поэтому единственный переносимый способ (работает
 * одинаково в UI, протоколе HTML/DOCX, везде) — литеральные юникод-символы
 * (₀-₉/⁰-⁹ и т.п.), вставляемые в текст по месту курсора. Полного юникод-
 * алфавита над-/подстрочных букв не существует — набор ограничен цифрами и
 * несколькими буквами, чего достаточно для типичных случаев (CO₂, м³, H₂O).
 *
 * Для настоящего contenteditable (абзацы/заголовки/пункты списка в
 * block-editor.ts) индексы делаются иначе — реальным `<sup>/<sub>` через
 * `document.execCommand('superscript'/'subscript')`, см. renderInlineEditable
 * в block-editor.ts; эта палитра — только для plain-текстовых полей, где
 * execCommand неприменим. */
export function toggleSubSupPalette(row: HTMLElement, target: HTMLInputElement): void {
  const existing = row.querySelector('.tn-lims-subsup');
  if (existing) { existing.remove(); return; }
  const panel = row.createDiv({ cls: 'tn-lims-subsup tn-lims-flex' });
  panel.createSpan({ cls: 'tn-lims-meta', text: 'Вставить в название:' });
  const CHARS = ['⁰', '¹', '²', '³', '⁴', '⁵', '⁶', '⁷', '⁸', '⁹', '₀', '₁', '₂', '₃', '₄', '₅', '₆', '₇', '₈', '₉'];
  for (const ch of CHARS) {
    const btn = panel.createEl('button', { text: ch, cls: 'tn-btn tn-btn-ghost' });
    btn.addEventListener('click', (e) => {
      e.preventDefault();
      const start = target.selectionStart ?? target.value.length;
      const end = target.selectionEnd ?? target.value.length;
      target.value = target.value.slice(0, start) + ch + target.value.slice(end);
      target.setSelectionRange(start + ch.length, start + ch.length);
      target.focus();
      target.dispatchEvent(new Event('change'));
    });
  }
}
