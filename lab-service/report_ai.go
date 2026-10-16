package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ---- ИИ-разделы отчёта (2026-10-16) ----
//
// Вызов идёт на ВНУТРЕННЮЮ ручку llm-service (POST /internal/llm/complete,
// X-Service-Secret) — на ОБЩЕМ («дежурном») ключе: отчёт генерируется
// автоматически при завершении заявки, «пользователя в моменте» нет.
// Пустая инструкция метода → без ИИ.

func llmServiceURL() string {
	if v := strings.TrimSpace(os.Getenv("LLM_SERVICE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://llm:3000"
}

// generateReportSections возвращает разделы, написанные ИИ по report_instruction
// метода. Инструкция пуста → (nil, nil). Ошибка (нет ключа/секрета, таймаут,
// неразобранный ответ) → вызывающий делает фолбэк без ИИ-разделов.
func (s *Server) generateReportSections(ctx context.Context, rc *reportContext) (*aiSections, error) {
	instruction := strings.TrimSpace(rc.Instruction)
	if instruction == "" {
		return nil, nil
	}
	secret := strings.TrimSpace(os.Getenv("LLM_SERVICE_SECRET"))
	if secret == "" {
		return nil, errors.New("LLM_SERVICE_SECRET is not set")
	}

	payload, err := json.Marshal(map[string]any{
		"system": reportSystemPrompt(instruction),
		"user":   renderReportContext(rc),
	})
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, llmServiceURL()+"/internal/llm/complete", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Secret", secret)

	resp, err := (&http.Client{Timeout: 190 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm-service: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("llm-service response: %w", err)
	}
	return parseAiSections(parsed.Content)
}

// reportSystemPrompt — системный промпт: технический писатель отчётов
// лаборатории; инструкция метода обязательна, выдумывать данные нельзя.
func reportSystemPrompt(instruction string) string {
	return `Ты — технический писатель отчётов о лабораторных испытаниях. По предоставленным данным об испытании составь аналитические разделы отчёта строго по инструкции заказчика.

Правила:
- Пиши ТОЛЬКО по предоставленным данным. Не выдумывай числа, оценки и выводы, которых нет в данных.
- Если данных для какого-то вывода мало или нет — напиши это прямо.
- Не ссылайся на нормативные документы, которых нет среди данных.
- Сравнения с аналогичными объектами делай только там, где они подкреплены приведённой статистикой.
- Язык — русский, деловой.

Инструкция заказчика (обязательна к исполнению):
` + instruction + `

Ответь строго JSON-объектом без пояснений и markdown-обёртки:
{"sections":[{"heading":"заголовок раздела","paragraphs":["абзац", ...],"bullets":["пункт", ...]}]}`
}

// renderReportContext — компактное текстовое представление данных испытания
// для модели (заявка/объект/метод, серии, агрегаты, статистика по аналогам).
func renderReportContext(rc *reportContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Заявка №%s, объект: %s, ЕКН: %s\n", rc.Request.CustomerNumber, rc.ObjectName, rc.EKN)
	if batch, _ := rc.ObjectChars["batch_number"].(string); batch != "" {
		fmt.Fprintf(&b, "Партия: %s\n", batch)
	}
	if sample, _ := rc.ObjectChars["sample_id"].(string); sample != "" {
		fmt.Fprintf(&b, "Идентификатор образца: %s\n", sample)
	}
	fmt.Fprintf(&b, "Метод испытания: %s\n", rc.MethodName)
	if rc.TargetIndicator != "" {
		fmt.Fprintf(&b, "Целевой показатель: %s\n", rc.TargetIndicator)
	}
	if rc.Request.CompletedAt != "" {
		fmt.Fprintf(&b, "Завершена: %s\n", rc.Request.CompletedAt)
	}

	b.WriteString("\nРезультаты по сериям:\n")
	for i, series := range rc.Series {
		b.WriteString(fmt.Sprintf("Серия %d: %s\n", i+1, renderKeyValues(series)))
	}
	if len(rc.Agg) > 0 {
		b.WriteString("\nИтоговые (агрегированные) результаты: " + renderKeyValues(rc.Agg) + "\n")
	}

	fmt.Fprintf(&b, "\nСтатистика по аналогичным объектам (тот же метод и ЕКН, аналогов: %d):\n", len(rc.Analogues))
	for _, st := range rc.IndicatorStats {
		if st.HasNumeric {
			fmt.Fprintf(&b, "- %s: n=%d, среднее=%s, минимум=%s, максимум=%s\n", st.Name, st.Count, fmtVal(st.Avg), fmtVal(st.Min), fmtVal(st.Max))
		}
		if len(st.Distribution) > 0 {
			vals := make([]string, 0, len(st.Distribution))
			for k, n := range st.Distribution {
				vals = append(vals, fmt.Sprintf("%s×%d", k, n))
			}
			sort.Strings(vals)
			fmt.Fprintf(&b, "- %s (оценки): %s\n", st.Name, strings.Join(vals, ", "))
		}
	}
	return b.String()
}

// renderKeyValues — "ключ=значение; ..." по карте (ключи по алфавиту — стабильно).
func renderKeyValues(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fmtVal(m[k]))
	}
	return strings.Join(parts, "; ")
}

// parseAiSections разбирает ответ модели: JSON-объект {"sections":[...]},
// возможно в markdown-обёртке/с прозой вокруг. Пусто/не JSON → ошибка.
func parseAiSections(content string) (*aiSections, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return nil, errors.New("LLM response has no JSON object")
	}
	var out aiSections
	if err := json.Unmarshal([]byte(content[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("parse LLM JSON: %w", err)
	}
	if len(out.Sections) == 0 {
		return nil, errors.New("LLM response has no sections")
	}
	return &out, nil
}
