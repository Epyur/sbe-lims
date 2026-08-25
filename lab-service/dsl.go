package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ---- DSL формул: безопасный интерпретатор (без exec/eval) ----
//
// Поддерживается:
//   - числа (int/float), строки ('...'), булевы (true/false)
//   - арифметика + - * / ^, унарный минус, скобки
//   - сравнения == != < <= > >=, and/or/not
//   - if cond then A else B
//   - агрегации avg/min/max/sum/count/median/std(expr) над коллекцией
//   - ссылки на параметры по имени (из values серии/агрегата)
//   - присваивание result = ...; промежуточные переменные
//
// Безопасность по построению: нет функций/доступа к объектам/файлам/сети.

// FormulaError — ошибка разбора/исполнения формулы.
type FormulaError struct{ msg string }

func (e *FormulaError) Error() string { return "formula: " + e.msg }

func ferrf(format string, a ...any) error {
	return &FormulaError{msg: fmt.Sprintf(format, a...)}
}

// FormulaEnv — среда исполнения: параметры (значения) + коллекции для агрегаций.
type FormulaEnv struct {
	// Params — скалярные параметры (name -> число/строка/bool).
	Params map[string]any
	// SeriesParams — для агрегаций: name -> список значений по сериям.
	SeriesParams map[string][]any
	// RankOrder — determinable_indicators текущего метода (по убыванию — первый
	// считается больше/выше остальных), нужен min_grade/max_grade — замена legacy
	// find_rank_extreme.
	RankOrder []string
}

func (e *FormulaEnv) resolve(name string) (any, error) {
	if v, ok := e.Params[name]; ok {
		return v, nil
	}
	if v, ok := e.SeriesParams[name]; ok {
		if len(v) == 1 {
			return v[0], nil
		}
		return v, nil
	}
	return nil, ferrf("параметр %q не найден", name)
}

// ---- Токены и лексер ----

type tokenKind int

const (
	tokNum tokenKind = iota
	tokStr
	tokIdent
	tokOp
	tokLParen
	tokRParen
	tokComma
	tokEOF
)

type token struct {
	kind tokenKind
	val  string
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			toks = append(toks, token{tokNum, src[i:j]})
			i = j
		case c == '\'' || c == '"':
			// Двойные кавычки (2026-08-23) — раньше принимались только одинарные;
			// метод ГВ (target_group_compliance) хранил формулу со строковыми
			// литералами в двойных кавычках ("Не оценивается" и т.п.), что давало
			// "неожиданный символ '\"'" при каждом расчёте — привычка из
			// большинства языков, где "" — тоже валидная строка. Закрывающая
			// кавычка — ТА ЖЕ, что открывающая (одинарная строка не обязана
			// заканчиваться на двойную и наоборот).
			quote := c
			j := i + 1
			var b strings.Builder
			for j < len(src) && src[j] != quote {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				b.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, ferrf("незакрытая строка")
			}
			toks = append(toks, token{tokStr, b.String()})
			i = j + 1
		case isIdentStart(c):
			m := identRe.FindString(src[i:])
			toks = append(toks, token{tokIdent, m})
			i += len(m)
		case c == '(':
			toks = append(toks, token{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")"})
			i++
		case c == ',':
			toks = append(toks, token{tokComma, ","})
			i++
		case strings.ContainsRune("+-*/^<>=!", rune(c)) || (c == '=' && i+1 < len(src) && src[i+1] == '='):
			// оператор: учёт двухсимвольных
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			if two == "==" || two == "!=" || two == "<=" || two == ">=" {
				toks = append(toks, token{tokOp, two})
				i += 2
			} else {
				toks = append(toks, token{tokOp, string(c)})
				i++
			}
		default:
			return nil, ferrf("неожиданный символ %q", string(c))
		}
	}
	toks = append(toks, token{tokEOF, ""})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

// ---- AST ----

type expr interface {
	eval(env *FormulaEnv) (any, error)
}

type numLit struct{ v float64 }
type strLit struct{ v string }
type boolLit struct{ v bool }
type identRef struct{ name string }
type unary struct {
	op string
	x  expr
}
type binary struct {
	op string
	l  expr
	r  expr
}
type condExpr struct {
	c  expr
	th expr
	el expr
}
type callExpr struct {
	fn   string
	args []expr
}
type assignExpr struct {
	name string
	val  expr
}

func (n *numLit) eval(_ *FormulaEnv) (any, error) { return n.v, nil }
func (n *strLit) eval(_ *FormulaEnv) (any, error) { return n.v, nil }
func (n *boolLit) eval(_ *FormulaEnv) (any, error) {
	return n.v, nil
}

func (n *identRef) eval(env *FormulaEnv) (any, error) {
	v, err := env.resolve(n.name)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (n *unary) eval(env *FormulaEnv) (any, error) {
	v, err := n.x.eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "-":
		f, err := toFloat(v)
		if err != nil {
			return nil, err
		}
		return -f, nil
	case "not":
		b, err := toBool(v)
		if err != nil {
			return nil, err
		}
		return !b, nil
	}
	return nil, ferrf("неизвестный унарный оператор %q", n.op)
}

func (n *binary) eval(env *FormulaEnv) (any, error) {
	l, err := n.l.eval(env)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "and":
		lb, err := toBool(l)
		if err != nil {
			return nil, err
		}
		rb, err := toBool(r)
		if err != nil {
			return nil, err
		}
		return lb && rb, nil
	case "or":
		lb, err := toBool(l)
		if err != nil {
			return nil, err
		}
		rb, err := toBool(r)
		if err != nil {
			return nil, err
		}
		return lb || rb, nil
	case "+", "-", "*", "/", "^":
		lf, err := toFloat(l)
		if err != nil {
			return nil, err
		}
		rf, err := toFloat(r)
		if err != nil {
			return nil, err
		}
		switch n.op {
		case "+":
			return lf + rf, nil
		case "-":
			return lf - rf, nil
		case "*":
			return lf * rf, nil
		case "/":
			if rf == 0 {
				return nil, ferrf("деление на ноль")
			}
			return lf / rf, nil
		case "^":
			return math.Pow(lf, rf), nil
		}
	case "==", "!=", "<", "<=", ">", ">=":
		return compare(n.op, l, r)
	}
	return nil, ferrf("неизвестный оператор %q", n.op)
}

func (n *condExpr) eval(env *FormulaEnv) (any, error) {
	c, err := toBoolEval(env, n.c)
	if err != nil {
		return nil, err
	}
	if c {
		return n.th.eval(env)
	}
	return n.el.eval(env)
}

func (n *callExpr) eval(env *FormulaEnv) (any, error) {
	switch n.fn {
	case "any", "all":
		// Логическая агрегация по сериям для текстовых Да/Нет-полей (2026-08-25, по
		// прямому запросу пользователя — заявка 287/2026, метод ГГ: agg_burning_drops
		// пытался считаться как max(burning_drops), а burning_drops — текстовое Да/Нет,
		// не число; см. AGENTS.md). any(field) — "Да", если хоть в одной серии "Да";
		// all(field) — "Да" только если ВО ВСЕХ сериях "Да" (т.е. "Нет", если хоть в
		// одной серии "Нет") — два явных, разных по смыслу варианта, которые
		// пользователь попросил как выбор в конфигураторе (см. AttributeAggregation).
		if len(n.args) != 1 {
			return nil, ferrf("%s ожидает 1 аргумент", n.fn)
		}
		bools, err := collectBools(env, n.args[0])
		if err != nil {
			return nil, err
		}
		result := n.fn == "all"
		for _, b := range bools {
			if n.fn == "any" {
				result = result || b
			} else {
				result = result && b
			}
		}
		if result {
			return "Да", nil
		}
		return "Нет", nil
	case "avg", "min", "max", "sum", "count", "median", "std":
		if len(n.args) == 0 {
			return nil, ferrf("%s ожидает хотя бы 1 аргумент", n.fn)
		}
		var vals []float64
		var err error
		if len(n.args) == 1 {
			// один аргумент: если это ссылка на серийный параметр — агрегируем по
			// сериям (как раньше); иначе — по развёрнутому массиву-значению.
			vals, err = collectVals(env, n.args[0])
		} else {
			// несколько аргументов: каждый — отдельное скалярное значение (напр.
			// avg(comb_length_1, comb_length_2, comb_length_3, comb_length_4) —
			// среднее нескольких атрибутов ОДНОЙ серии, а не серий одного атрибута).
			vals = make([]float64, 0, len(n.args))
			for _, a := range n.args {
				v, aerr := a.eval(env)
				if aerr != nil {
					return nil, aerr
				}
				f, ferr := toFloat(v)
				if ferr != nil {
					return nil, ferr
				}
				vals = append(vals, f)
			}
		}
		if err != nil {
			return nil, err
		}
		return aggregate(n.fn, vals)
	case "min_grade", "max_grade":
		return evalGradeExtreme(env, n.fn, n.args)
	case "interpolate":
		return evalInterpolate(env, n.args)
	case "agg_where":
		return evalAggWhere(env, n.args)
	default:
		return nil, ferrf("неизвестная функция %q", n.fn)
	}
}

// evalGradeExtreme — min_grade(v1, v2, ...)/max_grade(v1, v2, ...): выбирает
// минимальный/максимальный из переданных показателей по позиции в env.RankOrder
// (индекс 0 — максимальный/больше остальных). Замена legacy
// find_rank_extreme(values, rank_name, 'worst'|'best') — тех же legacy-терминов
// "худший"/"лучший" в новом коде намеренно нет (оценочные категории неприменимы к
// объективной оценке результатов испытаний), только "минимальный"/"максимальный".
func evalGradeExtreme(env *FormulaEnv, fn string, args []expr) (any, error) {
	if len(env.RankOrder) == 0 {
		return nil, ferrf("%s: у метода не задан порядок показателей (determinable_indicators)", fn)
	}
	if len(args) == 0 {
		return nil, ferrf("%s ожидает хотя бы 1 аргумент", fn)
	}
	extremeIdx := -1
	extremeGrade := ""
	for _, a := range args {
		v, err := a.eval(env)
		if err != nil {
			return nil, err
		}
		grade, ok := v.(string)
		if !ok || grade == "" {
			continue
		}
		idx := indexOfString(env.RankOrder, grade)
		if idx < 0 {
			continue
		}
		if extremeIdx == -1 ||
			(fn == "min_grade" && idx > extremeIdx) ||
			(fn == "max_grade" && idx < extremeIdx) {
			extremeIdx = idx
			extremeGrade = grade
		}
	}
	if extremeIdx == -1 {
		return nil, ferrf("%s: ни один аргумент не распознан как показатель метода", fn)
	}
	return extremeGrade, nil
}

// evalInterpolate — interpolate(x, xs, ys): линейная интерполяция по параллельным
// массивам-параметрам xs/ys (напр. калибровочная таблица прибора). За пределами
// диапазона xs — продолжение по касательной крайнего отрезка (не ошибка).
// Замена legacy калибровочной формулы (frm00020).
func evalInterpolate(env *FormulaEnv, args []expr) (any, error) {
	if len(args) != 3 {
		return nil, ferrf("interpolate ожидает 3 аргумента (x, xs, ys)")
	}
	xv, err := args[0].eval(env)
	if err != nil {
		return nil, err
	}
	x, err := toFloat(xv)
	if err != nil {
		return nil, err
	}
	xsv, err := args[1].eval(env)
	if err != nil {
		return nil, err
	}
	xs, err := toFloatSlice(xsv)
	if err != nil {
		return nil, err
	}
	ysv, err := args[2].eval(env)
	if err != nil {
		return nil, err
	}
	ys, err := toFloatSlice(ysv)
	if err != nil {
		return nil, err
	}
	return linearInterpolate(x, xs, ys)
}

// evalAggWhere — agg_where(fn, value_param, cond_param, cond_value): агрегирует
// value_param по сериям, где cond_param == cond_value (сравнение как строки).
// Замена legacy формул вида "min(get_all_values(x) where условие)" (frm00015).
func evalAggWhere(env *FormulaEnv, args []expr) (any, error) {
	if len(args) != 4 {
		return nil, ferrf("agg_where ожидает 4 аргумента (fn, value_param, cond_param, cond_value)")
	}
	fnv, err := args[0].eval(env)
	if err != nil {
		return nil, err
	}
	fn, _ := fnv.(string)
	valueIdent, ok := args[1].(*identRef)
	if !ok {
		return nil, ferrf("agg_where: второй аргумент должен быть именем параметра")
	}
	condIdent, ok := args[2].(*identRef)
	if !ok {
		return nil, ferrf("agg_where: третий аргумент должен быть именем параметра")
	}
	condTargetV, err := args[3].eval(env)
	if err != nil {
		return nil, err
	}
	condTarget := fmt.Sprintf("%v", condTargetV)

	valueSeries := env.SeriesParams[valueIdent.name]
	condSeries := env.SeriesParams[condIdent.name]
	if len(valueSeries) == 0 || len(valueSeries) != len(condSeries) {
		return nil, ferrf("agg_where: %q и %q должны быть сериями одинаковой длины", valueIdent.name, condIdent.name)
	}
	vals := make([]float64, 0, len(valueSeries))
	for i, cv := range condSeries {
		if fmt.Sprintf("%v", cv) != condTarget {
			continue
		}
		f, err := toFloat(valueSeries[i])
		if err != nil {
			return nil, err
		}
		vals = append(vals, f)
	}
	if len(vals) == 0 {
		return nil, ferrf("agg_where: нет серий, удовлетворяющих условию %s == %v", condIdent.name, condTargetV)
	}
	return aggregate(fn, vals)
}

func (n *assignExpr) eval(env *FormulaEnv) (any, error) {
	v, err := n.val.eval(env)
	if err != nil {
		return nil, err
	}
	env.Params[n.name] = v
	return v, nil
}

// ---- Парсер (рекурсивный спуск) ----

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func parseExpr(src string) (expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	// Поддержка "result = ..." и промежуточных присваиваний: expr может быть
	// seq = value; next = seq + 1; result = next. Разделитель — точка с запятой
	// либо перенос строки. Для простоты: парсим список выражений, result — последнее.
	// Здесь: если есть "=" на верхнем уровне — это присваивание (переменной).
	e, err := p.parseAssign()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, ferrf("лишний токен %q", p.peek().val)
	}
	return e, nil
}

// parseAssign обрабатывает "name = expr" (одиночное) — для "result = ...".
func (p *parser) parseAssign() (expr, error) {
	// Пробуем: если следующий токен ident и за ним '=', это присваивание.
	if p.peek().kind == tokIdent {
		save := p.pos
		name := p.next().val
		if p.peek().kind == tokOp && p.peek().val == "=" {
			p.next() // =
			val, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			return &assignExpr{name: name, val: val}, nil
		}
		p.pos = save
	}
	return p.parseOr()
}

func (p *parser) parseOr() (expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokIdent && p.peek().val == "or" {
		p.next()
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &binary{op: "or", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (expr, error) {
	l, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokIdent && p.peek().val == "and" {
		p.next()
		r, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		l = &binary{op: "and", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseCmp() (expr, error) {
	l, err := p.parseAddSub()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && (p.peek().val == "==" || p.peek().val == "!=" ||
		p.peek().val == "<" || p.peek().val == "<=" || p.peek().val == ">" || p.peek().val == ">=") {
		op := p.next().val
		r, err := p.parseAddSub()
		if err != nil {
			return nil, err
		}
		l = &binary{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAddSub() (expr, error) {
	l, err := p.parseMulDiv()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && (p.peek().val == "+" || p.peek().val == "-") {
		op := p.next().val
		r, err := p.parseMulDiv()
		if err != nil {
			return nil, err
		}
		l = &binary{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseMulDiv() (expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && (p.peek().val == "*" || p.peek().val == "/" || p.peek().val == "^") {
		op := p.next().val
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &binary{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseUnary() (expr, error) {
	if p.peek().kind == tokOp && (p.peek().val == "-" || p.peek().val == "!") {
		op := p.next().val
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if op == "!" {
			op = "not"
		}
		return &unary{op: op, x: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (expr, error) {
	t := p.peek()
	switch t.kind {
	case tokNum:
		p.next()
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, ferrf("не число %q", t.val)
		}
		return &numLit{v: f}, nil
	case tokStr:
		p.next()
		return &strLit{v: t.val}, nil
	case tokIdent:
		// true/false/if или идентификатор/функция
		switch t.val {
		case "true", "false":
			p.next()
			return &boolLit{v: t.val == "true"}, nil
		case "if":
			p.next()
			c, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if !(p.peek().kind == tokIdent && p.peek().val == "then") {
				return nil, ferrf("ожидается then")
			}
			p.next()
			th, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if !(p.peek().kind == tokIdent && p.peek().val == "else") {
				return nil, ferrf("ожидается else")
			}
			p.next()
			el, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			return &condExpr{c: c, th: th, el: el}, nil
		}
		// функция или идентификатор
		name := p.next().val
		if p.peek().kind == tokLParen {
			p.next()
			var args []expr
			if p.peek().kind != tokRParen {
				for {
					a, err := p.parseOr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.peek().kind == tokComma {
						p.next()
						continue
					}
					break
				}
			}
			if p.peek().kind != tokRParen {
				return nil, ferrf("ожидается )")
			}
			p.next()
			return &callExpr{fn: name, args: args}, nil
		}
		return &identRef{name: name}, nil
	case tokLParen:
		p.next()
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, ferrf("ожидается )")
		}
		p.next()
		return e, nil
	}
	return nil, ferrf("неожиданный токен %q", t.val)
}

// ---- Хелперы ----

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, ferrf("не число %q", x)
		}
		return f, nil
	default:
		return 0, ferrf("ожидалось число, получено %v", v)
	}
}

// toBool — "да"/"нет" (любой регистр, 2026-08-22) добавлены отдельно от "true"/"1":
// атрибуты data_type="boolean", заполняемые из email-импорта десктопной ЛИМС
// (Яндекс.Формы), реально приходят как русские строки "Да"/"Нет" (см. json_attr.md,
// пример: burning_drops в письмах Comb), не JSON true/false.
func toBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "да":
			return true, nil
		case "false", "0", "нет", "":
			return false, nil
		default:
			return false, ferrf("ожидалось булево, получено %q", x)
		}
	case float64:
		return x != 0, nil
	default:
		return false, ferrf("ожидалось булево, получено %v", v)
	}
}

func toBoolEval(env *FormulaEnv, e expr) (bool, error) {
	v, err := e.eval(env)
	if err != nil {
		return false, err
	}
	return toBool(v)
}

func compare(op string, l, r any) (any, error) {
	// Числа сравниваем как float; строки — как строки.
	if op == "==" {
		return fmt.Sprintf("%v", l) == fmt.Sprintf("%v", r), nil
	}
	if op == "!=" {
		return fmt.Sprintf("%v", l) != fmt.Sprintf("%v", r), nil
	}
	lf, lerr := toFloat(l)
	rf, rerr := toFloat(r)
	if lerr == nil && rerr == nil {
		switch op {
		case "<":
			return lf < rf, nil
		case "<=":
			return lf <= rf, nil
		case ">":
			return lf > rf, nil
		case ">=":
			return lf >= rf, nil
		}
	}
	ls, rss := fmt.Sprintf("%v", l), fmt.Sprintf("%v", r)
	switch op {
	case "<":
		return ls < rss, nil
	case "<=":
		return ls <= rss, nil
	case ">":
		return ls > rss, nil
	case ">=":
		return ls >= rss, nil
	}
	return nil, ferrf("нельзя сравнить %v и %v", l, r)
}

// collectVals собирает коллекцию значений для агрегации: если аргумент —
// идентификатор с массивом в SeriesParams — вернуть массив; если выражение —
// вычислить и обернуть в массив.
func collectVals(env *FormulaEnv, e expr) ([]float64, error) {
	if id, ok := e.(*identRef); ok {
		if arr, ok2 := env.SeriesParams[id.name]; ok2 {
			out := make([]float64, 0, len(arr))
			for _, v := range arr {
				f, err := toFloat(v)
				if err != nil {
					return nil, err
				}
				out = append(out, f)
			}
			return out, nil
		}
	}
	v, err := e.eval(env)
	if err != nil {
		return nil, err
	}
	// если v — массив (серия), развернуть
	if arr, ok := v.([]any); ok {
		out := make([]float64, 0, len(arr))
		for _, x := range arr {
			f, err := toFloat(x)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, nil
	}
	f, err := toFloat(v)
	if err != nil {
		return nil, err
	}
	return []float64{f}, nil
}

// collectBools — то же самое, что collectVals, но для логических агрегаций any/all
// (2026-08-25): каждое значение по сериям приводится через toBool ("Да"/"Нет"/
// true/false/1/0), а не toFloat — источник (напр. burning_drops) текстовый Да/Нет,
// не число.
func collectBools(env *FormulaEnv, e expr) ([]bool, error) {
	var raw []any
	if id, ok := e.(*identRef); ok {
		if arr, ok2 := env.SeriesParams[id.name]; ok2 {
			raw = arr
		}
	}
	if raw == nil {
		v, err := e.eval(env)
		if err != nil {
			return nil, err
		}
		if arr, ok := v.([]any); ok {
			raw = arr
		} else {
			raw = []any{v}
		}
	}
	out := make([]bool, 0, len(raw))
	for _, v := range raw {
		b, err := toBool(v)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func aggregate(fn string, vals []float64) (any, error) {
	n := len(vals)
	if n == 0 {
		return nil, ferrf("%s: пустая выборка", fn)
	}
	switch fn {
	case "count":
		return float64(n), nil
	case "sum":
		s := 0.0
		for _, v := range vals {
			s += v
		}
		return s, nil
	case "avg":
		s := 0.0
		for _, v := range vals {
			s += v
		}
		return s / float64(n), nil
	case "min":
		m := vals[0]
		for _, v := range vals[1:] {
			if v < m {
				m = v
			}
		}
		return m, nil
	case "max":
		m := vals[0]
		for _, v := range vals[1:] {
			if v > m {
				m = v
			}
		}
		return m, nil
	case "median":
		cp := append([]float64(nil), vals...)
		sortFloats(cp)
		if n%2 == 1 {
			return cp[n/2], nil
		}
		return (cp[n/2-1] + cp[n/2]) / 2, nil
	case "std":
		s := 0.0
		for _, v := range vals {
			s += v
		}
		mean := s / float64(n)
		v := 0.0
		for _, x := range vals {
			v += (x - mean) * (x - mean)
		}
		return math.Sqrt(v / float64(n)), nil
	}
	return nil, ferrf("неизвестная агрегация %q", fn)
}

// toFloatSlice приводит значение параметра (обычно []any после json.Unmarshal) к
// []float64 — для interpolate(x, xs, ys).
func toFloatSlice(v any) ([]float64, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, ferrf("ожидался массив, получено %v", v)
	}
	out := make([]float64, 0, len(arr))
	for _, x := range arr {
		f, err := toFloat(x)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// linearInterpolate — кусочно-линейная интерполяция по (возможно невозрастающим или
// невозрастающим — как хранится калибровочная таблица) параллельным массивам xs/ys.
// За пределами диапазона xs продолжает по касательной крайнего отрезка, а не
// обрывается ошибкой (реальные калибровочные таблицы иногда не покрывают весь
// диапазон измеренных значений).
func linearInterpolate(x float64, xs, ys []float64) (float64, error) {
	n := len(xs)
	if n == 0 || len(ys) != n {
		return 0, ferrf("interpolate: xs и ys должны быть непустыми массивами одной длины")
	}
	if n == 1 {
		return ys[0], nil
	}
	asc := xs[1] >= xs[0]
	for i := 0; i < n-1; i++ {
		x0, x1 := xs[i], xs[i+1]
		y0, y1 := ys[i], ys[i+1]
		inRange := (asc && x >= x0 && x <= x1) || (!asc && x <= x0 && x >= x1)
		if inRange {
			if x1 == x0 {
				return y0, nil
			}
			t := (x - x0) / (x1 - x0)
			return y0 + t*(y1-y0), nil
		}
	}
	var x0, x1, y0, y1 float64
	if (asc && x < xs[0]) || (!asc && x > xs[0]) {
		x0, x1, y0, y1 = xs[0], xs[1], ys[0], ys[1]
	} else {
		x0, x1, y0, y1 = xs[n-2], xs[n-1], ys[n-2], ys[n-1]
	}
	if x1 == x0 {
		return y0, nil
	}
	t := (x - x0) / (x1 - x0)
	return y0 + t*(y1-y0), nil
}

func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// runFormula выполняет формулу (expression) с заданной средой. Возвращает результат.
func runFormula(expression string, env *FormulaEnv) (any, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, ferrf("пустая формула")
	}
	ast, err := parseExpr(expression)
	if err != nil {
		return nil, err
	}
	return ast.eval(env)
}

var _ = errors.New // резерв
