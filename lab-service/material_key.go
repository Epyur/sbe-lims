package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ---- Идентичность материала для отбора аналогов (2026-10-17) ----
//
// Аналоги в приложении ИИ-отчёта раньше отбирались точным равенством ЕКН, а ЕКН —
// это артикул конкретной фасовки: один материал выпускается рулоном 2,10x20 или
// 2,10x25 м, плитой 2385x1185 или 2400x1200 мм, в сером/белом/хаки цвете, и каждая
// комбинация имеет свой ЕКН. Поэтому аналоги собирались внутри одной фасовки, а у
// 613 заявок из 682 ЕКН вообще не заполнен.
//
// Решение пользователя 2026-10-17: материал = (тип, линейка, модификатор).
// Толщина в ключ НЕ входит — все толщины линейки анализируются вместе, а различие
// толщин показывается в отчёте (закономерности при изменении толщины). Фасовка и
// цвет не важны никогда. Торговые двойники (Ecoplast / ECOROOF / TSTN) и
// модификаторы (FR / Pro / Arctic / Middle / Полы ФЛ/ФЛ) — разные материалы.
//
// Файл — чистые функции: ни БД, ни сети. Разбор названий проверяется юнит-тестами
// на реальных названиях из lab.objects (сняты 2026-10-17).

// Типы материала (канонические значения MaterialKey.Type).
const (
	materialTypeMembrane = "roofing_polym_membr"
	materialTypeInsul    = "thermal_insulation"
	materialTypeSealant  = "sealant"
	materialTypeGeomembr = "geomembrane"
	materialTypeUnknown  = "unknown"
)

// MaterialKey — идентичность материала для отбора аналогов.
type MaterialKey struct {
	Type     string // канонический тип материала
	Line     string // "logicroof v-rp", "logicpir prof", "ecoplast v-rp"
	Modifier string // "fr", "pro", "arctic", "middle", "f/f", "floors", "" (нет)
}

// materialFeatures — признаки, снятые с карточки ЕКН (QRC) либо разобранные из
// названия. Толщина/габариты в миллиметрах, 0 — не определено.
type materialFeatures struct {
	NameWeb   string
	Type      string  // сырой тип из QRC (atr_type_material[0] / type_ofgidro.data)
	Line      string  // сырая/разобранная линейка
	Modifier  string  // разобранный модификатор
	Thickness float64 // мм
	Width     float64 // мм
	Length    float64 // мм
	Color     string
	Armature  string
}

var reSpaces = regexp.MustCompile(`\s+`)
var reNoisePart = regexp.MustCompile(`(?i)\b(?:\x{2116}\s*\x{043F}\.?|\x{2116}|\x{043F}\x{0430}\x{0440}\x{0442}\x{0438}\x{044F}|party|lot|\x{043B}\x{043E}\x{0442}#?)\s*[:.]?\s*\d+`)
var rePercentJunk = regexp.MustCompile(`(?i)\b\d+\s*%\s*(?:\x{043F}\x{043B}\x{0430}\x{0441}\x{0442}\x{0438}\x{043A}\x{0430}\x{0442}|\x{043F}\x{043B}\x{0430}\x{0441}\x{0442}\x{0438}\x{0444}\x{0438}\x{043A}\x{0430}\x{0442}[\x{0430}-\x{044F}]*|\x{0441}\x{0443}\x{0440}\x{044C}\x{043C}[\x{0430}-\x{044F}]*)\b`)
var reSurmaJunk = regexp.MustCompile(`(?i)\b\x{0441}\x{0443}\x{0440}\x{044C}\x{043C}[\x{0430}-\x{044F}]*\s*[a-z\x{0430}-\x{044F}]*[-\s]?\d+\b`)
var reRal = regexp.MustCompile(`(?i)\bral\s*\d{4}\b`)
var reParens = regexp.MustCompile(`\([^()]*\)`)
var reSizeX = regexp.MustCompile(`(?i)\b\d+(?:[.,]\d+)?\s*[x\x{0445}]\s*\d+(?:[.,]\d+)?(?:\s*(?:\x{043C}\x{043C}|m|mm))?`)
var reSizeTriple = regexp.MustCompile(`(?i)\b\d+(?:[.,]\d+)?\s*[x\x{0445}]\s*\d+(?:[.,]\d+)?\s*[x\x{0445}]\s*\d+(?:[.,]\d+)?\b`)
var reThicknessMm = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*(?:\x{043C}\x{043C}|(?i)mm)`)
var reOneDecimal = regexp.MustCompile(`\b(\d+[.,]\d)\b`)
var reThicknessMmOnly = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*\x{043C}\x{043C}`)
var reThicknessDigit = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*`)
var reValueAfterMm = regexp.MustCompile(`([0-9][0-9.,]*)\s*(?:\x{043C}\x{043C}|mm)`) // запасной путь
var reCyrInLatinWord = regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]*[\x{0430}-\x{044F}\x{0451}][a-z0-9\-]*`)
var reTstn = regexp.MustCompile(`(?i)\btstn\b`)
var reMarkerLat = regexp.MustCompile(`(?i)\b(logicpir|logicroof|lodgicroof|logiroof|logicbase|ecobase|ecoplast|ecoroof|logicflex|logicbarrier)\b`)

// cyrToLat — омоглифы, встречающиеся в латинских словах живых названий
// («Arctiс» с кириллической «с», «pvх»).
var cyrToLat = map[rune]rune{
	'а': 'a', 'в': 'b', 'е': 'e', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o',
	'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'ё': 'e',
}

// normalizeMaterialText — приведение названия к канонической форме для разбора:
// NFKC, схлопывание пробелов, кириллические омоглифы в латинских словах,
// `_` в номере модели и кириллическая `х` в габаритах, нижний регистр.
func normalizeMaterialText(raw string) string {
	s := norm.NFKC.String(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.ToLower(s)
	s = reCyrInLatinWord.ReplaceAllStringFunc(s, replaceCyrHomoglyphs)
	s = regexp.MustCompile(`(\d)\s*\x{0445}\s*(\d)`).ReplaceAllString(s, "${1}x${2}")
	return s
}

// replaceCyrHomoglyphs заменяет кириллические омоглифы на латиницу внутри слова.
func replaceCyrHomoglyphs(word string) string {
	var b strings.Builder
	for _, r := range word {
		if lat, ok := cyrToLat[r]; ok {
			b.WriteRune(lat)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseThicknessToken — "1,5" / "1.5" → 1.5, иначе 0.
func parseThicknessToken(tok string) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(tok, ",", "."), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return f
}

// cleanEkn — вытаскивает ЕКН из поля, куда записали название с номером
// («ПВХ Ecoplast V-RP 1.2 мм мембрана серая 2.10х25, 591278» → «591278»).
func cleanEkn(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	allDigits := true
	for _, r := range s {
		if !unicode.IsDigit(r) {
			allDigits = false
			break
		}
	}
	if allDigits {
		return s
	}
	fields := regexp.MustCompile(`\b\d{5,6}\b`).FindAllString(s, -1)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// Порядок важен: «Герметик ПУ … Logicflex» — герметик, хотя в названии есть
// маркер мембранного бренда; «Геомембрана ГЕОПРУФ» — геомембрана, а не мембрана
// (внутри слова «Геомембрана» есть «мембран»). Специфичные типы проверяются
// раньше мембраны.
var materialTypeKeywords = []struct {
	typ  string
	keys []string
}{
	{materialTypeSealant, []string{"герметик"}},
	{materialTypeGeomembr, []string{"геомембран", "геопруф", "geopruf"}},
	{materialTypeInsul, []string{"логикпир", "logicpir", "техноплекс", "теплекс", "теплоизоляц", "теплозвукоизоляц", "минват"}},
	{materialTypeMembrane, []string{"мембран", "pvc", "tstn", "logicroof", "lodgicroof", "logiroof", "ecoplast", "ecoroof", "logicbase", "ecobase", "logicflex", "extrabase"}},
}

var modifierKeywords = []struct {
	mod  string
	keys []string
}{
	{"middle", []string{"middle"}},
	{"arctic", []string{"arctic", "арктик"}},
	{"floors", []string{"полы фл/фл", "полы"}},
	{"f/f", []string{"ф/ф", "f/f"}},
	{"fr", []string{"fr"}},
	{"pro", []string{"pro"}},
}

var lineFixes = []struct{ from, to string }{
	{"lodgicroof", "logicroof"},
	{"logiroof", "logicroof"},
	{"loqicroof", "logicroof"},
	{"logicrof", "logicroof"},
	{"pvх", "pvc"},
	{"pvh", "pvc"},
}

// ParseMaterialFromName — разбор названия объекта / name_web в ключ материала и
// толщину. Логика: нормализация → вытаскивание толщины → вырезание шума
// (партии, №п., RAL, габариты) → тип → модификатор → номер модели → линейка.
func ParseMaterialFromName(raw string) (MaterialKey, float64) {
	s := normalizeMaterialText(raw)
	if s == "" {
		return MaterialKey{Type: materialTypeUnknown}, 0
	}

	thickness := 0.0
	if m := reThicknessMm.FindStringSubmatch(s); len(m) == 2 {
		thickness = parseThicknessToken(m[1])
		s = strings.Replace(s, m[0], " ", 1)
	}
	if thickness == 0 {
		// Габарит из трёх чисел («L-2385x1185x40», «1890x1190x40») — толщина
		// последним числом; двойной габарит («2,10x20») толщиной не считается.
		if size := reSizeTriple.FindString(s); size != "" {
			parts := regexp.MustCompile(`\d+`).FindAllString(size, -1)
			if len(parts) == 3 {
				thickness = parseThicknessToken(parts[2])
			}
		}
	}
	if thickness == 0 {
		// Толщина без единицы измерения («Arctiс 2,0 мембрана», «Logicroof V-RP 1,2»).
		// Правило пользователя 2026-10-17: один знак после запятой — толщина в мм
		// (2,0 / 1,2 / 1,6), два знака — ширина в метрах (2,10), целое — не толщина.
		if m := reOneDecimal.FindStringSubmatch(s); len(m) == 2 {
			thickness = parseThicknessToken(m[1])
			s = strings.Replace(s, m[0], " ", 1)
		}
	}

	// Шум: габариты, RAL и цвет в скобках, партии, проценты пластиката, сурьма.
	s = reParens.ReplaceAllString(s, " ")
	s = reRal.ReplaceAllString(s, " ")
	s = reSizeTriple.ReplaceAllString(s, " ")
	s = reSizeX.ReplaceAllString(s, " ")
	s = reNoisePart.ReplaceAllString(s, " ")
	s = rePercentJunk.ReplaceAllString(s, " ")
	s = reSurmaJunk.ReplaceAllString(s, " ")
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	key := MaterialKey{Type: parseMaterialType(s)}

	// Модификатор — отдельным словом; порядок в названии не важен. Проверяем
	// «полы», «ф/ф» раньше «pro», иначе «PROF» съедался бы как Pro.
	for _, m := range modifierKeywords {
		for _, k := range m.keys {
			if containsWholeToken(s, k) {
				key.Modifier = m.mod
				s = removeWholeToken(s, k)
				break
			}
		}
		if key.Modifier != "" {
			break
		}
	}

	key.Line = extractMaterialLine(s)
	return key, thickness
}

// parseMaterialType — тип материала по словарю ключевых слов.
func parseMaterialType(s string) string {
	for _, t := range materialTypeKeywords {
		for _, k := range t.keys {
			if strings.Contains(s, k) {
				return t.typ
			}
		}
	}
	if s == "" {
		return materialTypeUnknown
	}
	first := strings.Fields(s)[0]
	return materialTypeUnknown + ":" + first
}

// extractMaterialLine — линейка материала из остатка названия. Сначала
// известные марки (они есть у всех проверенных карточек), затем — первый
// латинский блок модели с номером ("logicroof v-rp", "logicpir").
func extractMaterialLine(s string) string {
	if m := reMarkerLat.FindString(s); m != "" {
		line := strings.ToLower(m)
		// Бренд-модель может идти отдельным токеном: "logicroof v-rp".
		after := s[strings.Index(s, m)+len(m):]
		for _, tok := range strings.Fields(after) {
			tok = strings.Trim(tok, ".,;:")
			if strings.HasPrefix(tok, "v-") || strings.HasPrefix(tok, "uw-") || strings.HasPrefix(tok, "v") && len(tok) <= 4 && strings.Contains(tok, "-") {
				line += " " + tok
				break
			}
			break
		}
		for _, fix := range lineFixes {
			line = strings.ReplaceAll(line, fix.from, fix.to)
		}
		return line
	}
	if reTstn.MatchString(s) {
		return "tstn"
	}
	tokens := strings.Fields(s)
	words := make([]string, 0, 4)
	for _, tok := range tokens {
		tok = strings.Trim(tok, ".,;:")
		if tok == "" {
			continue
		}
		if !isLatinWord(tok) {
			if len(words) > 0 {
				break
			}
			continue
		}
		words = append(words, tok)
	}
	line := strings.Join(words, " ")
	for _, fix := range lineFixes {
		line = strings.ReplaceAll(line, fix.from, fix.to)
	}
	return line
}

// isLatinWord — слово состоит из латиницы/цифр/дефисов/слэша.
func isLatinWord(s string) bool {
	hasLetter := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '-', r == '/':
		default:
			return false
		}
	}
	return hasLetter
}

// containsWholeToken — есть ли фрагмент отдельным словом (с границами).
func containsWholeToken(s, token string) bool {
	idx := 0
	for {
		i := strings.Index(s[idx:], token)
		if i < 0 {
			return false
		}
		start, end := idx+i, idx+i+len(token)
		leftOK := start == 0 || !isWordRune(rune(s[start-1]))
		rightOK := end >= len(s) || !isWordRune(rune(s[end]))
		if leftOK && rightOK {
			return true
		}
		idx = start + len(token)
	}
}

// removeWholeToken — удаляет фрагмент как отдельное слово.
func removeWholeToken(s, token string) string {
	out := s
	for containsWholeToken(out, token) {
		idx := strings.Index(out, token)
		out = out[:idx] + " " + out[idx+len(token):]
	}
	return reSpaces.ReplaceAllString(out, " ")
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-'
}

// ParseMaterialFromFeatures — ключ и толщина из структурно снятых признаков
// карточки ЕКН. Линейка и модификатор структурно в QRC не заведены (atr_series_ent
// и brands пусты у всех проверенных карточек), поэтому берутся из name_web;
// тип — из Modif, толщина — из Thickness, габарит — из Width/Length.
func ParseMaterialFromFeatures(f materialFeatures) (MaterialKey, float64) {
	key, thickness := ParseMaterialFromName(f.NameWeb)
	if key.Line == "" {
		key.Line = strings.ToLower(strings.TrimSpace(f.Line))
	}
	if key.Modifier == "" {
		key.Modifier = strings.ToLower(strings.TrimSpace(f.Modifier))
	}
	if t := canonicalMaterialType(f.Type); t != "" {
		key.Type = t
	}
	if thickness == 0 {
		thickness = f.Thickness
	}
	return key, thickness
}

// canonicalMaterialType — приведение сырого типа QRC к каноническому.
func canonicalMaterialType(raw string) string {
	s := normalizeMaterialText(raw)
	if s == "" {
		return ""
	}
	if t := parseMaterialType(s); !strings.HasPrefix(t, materialTypeUnknown+":") {
		return t
	}
	return ""
}

// SameMaterial — ключи описывают один материал (все три составляющие равны).
func SameMaterial(a, b MaterialKey) bool {
	return a.Type == b.Type && a.Line == b.Line && a.Modifier == b.Modifier
}

// SameMaterialLine — та же линейка, другой модификатор (ступень 3).
func SameMaterialLine(a, b MaterialKey) bool {
	return a.Type == b.Type && a.Line == b.Line && a.Modifier != b.Modifier
}

// MaterialKeyFilled — ключ годен для отбора аналогов (есть тип и линейка).
func MaterialKeyFilled(k MaterialKey) bool {
	return k.Line != "" && k.Type != materialTypeUnknown && !strings.HasPrefix(k.Type, materialTypeUnknown+":")
}

// materialKeyLabel — человекочитаемая расшифровка для документа.
func materialKeyLabel(k MaterialKey) string {
	parts := []string{}
	if k.Line != "" {
		parts = append(parts, titleFirst(k.Line))
	}
	if t := materialTypeLabel(k.Type); t != "" {
		parts = append(parts, t)
	}
	if m := modifierLabel(k.Modifier); m != "" {
		parts = append(parts, "модификация: "+m)
	}
	if len(parts) == 0 {
		return "не определён"
	}
	return strings.Join(parts, " · ")
}

func materialTypeLabel(t string) string {
	switch t {
	case materialTypeMembrane:
		return "полимерная мембрана"
	case materialTypeInsul:
		return "теплоизоляционные плиты"
	case materialTypeSealant:
		return "герметик"
	case materialTypeGeomembr:
		return "геомембрана"
	}
	return ""
}

func modifierLabel(m string) string {
	switch m {
	case "middle":
		return "Middle"
	case "arctic":
		return "Arctic"
	case "fr":
		return "FR"
	case "pro":
		return "Pro"
	case "f/f":
		return "Ф/Ф"
	case "floors":
		return "Полы ФЛ/ФЛ"
	}
	return m
}

// titleFirst — первая буква заглавной (для подписей в документе).
func titleFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
