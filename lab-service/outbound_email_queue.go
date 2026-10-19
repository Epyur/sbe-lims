package main

import (
	"fmt"
	"strings"
)

// ---- Очередь отправки писем заявки (2026-10-19) ----
// Дизайн: docs/superpowers/specs/2026-10-19-sbe-lims-outbound-email-queue-design.md.
// Чистые функции без БД/сети: какие письма заводить, как собирается тема/тело и
// когда попытки исчерпаны. Сама постановка в очередь — outbound_email.go,
// отправка — outbound_email_worker.go.

const maxOutboundEmailAttempts = 5

// outboundTarget — одно письмо, которое нужно поставить в очередь.
type outboundTarget struct {
	RecipientType  string // "customer" | "lpitrack" | "lpitrack_processing"
	Address        string
	WithAttachment bool
}

// completionTargets — письма по завершению заявки: заказчику (если есть адрес)
// и дубль в LPITrack (если есть external_id и служебный адрес лабы). Оба — с
// отчётом.
func completionTargets(req Request, serviceEmail string) []outboundTarget {
	out := make([]outboundTarget, 0, 2)
	if addr := strings.TrimSpace(req.OwnerEmail); addr != "" {
		out = append(out, outboundTarget{RecipientType: "customer", Address: addr, WithAttachment: true})
	}
	if strings.TrimSpace(req.ExternalID) != "" {
		if addr := strings.TrimSpace(serviceEmail); addr != "" {
			out = append(out, outboundTarget{RecipientType: "lpitrack", Address: addr, WithAttachment: true})
		}
	}
	return out
}

// processingTarget — уведомление о взятии заявки в работу: только LPITrack, без
// вложения (результатов ещё нет).
func processingTarget(req Request, serviceEmail string) []outboundTarget {
	if strings.TrimSpace(req.ExternalID) == "" {
		return nil
	}
	addr := strings.TrimSpace(serviceEmail)
	if addr == "" {
		return nil
	}
	return []outboundTarget{{RecipientType: "lpitrack_processing", Address: addr, WithAttachment: false}}
}

// filterCompletionTargets — оставляет только разрешённые автоотправкой письма
// (см. shouldAutoSend), сохраняя порядок исходного списка.
func filterCompletionTargets(targets []outboundTarget, sendCustomer, sendLpitrack bool) []outboundTarget {
	out := make([]outboundTarget, 0, len(targets))
	for _, t := range targets {
		switch t.RecipientType {
		case "customer":
			if sendCustomer {
				out = append(out, t)
			}
		case "lpitrack":
			if sendLpitrack {
				out = append(out, t)
			}
		}
	}
	return out
}

// nextOutboundStatus — статус строки после очередной неудачи. attempts — число
// попыток ДО текущей; на пятой прекращаем автоматические повторы.
func nextOutboundStatus(attempts int) string {
	if attempts+1 >= maxOutboundEmailAttempts {
		return "failed"
	}
	return "pending"
}

// outboundPayload — тема и тело письма по типу адресата. Тексты совпадают с
// прежними (sendCompletionEmails/triggerProcessingEmail), чтобы получатель не
// заметил разницы.
func outboundPayload(req Request, recipientType string) (subject, body string) {
	switch recipientType {
	case "customer":
		return customerEmailContent(req)
	case "lpitrack":
		return legacyExternalKey(req.ExternalID),
			fmt.Sprintf("Заявка №%s (external_id=%s) завершена.", req.CustomerNumber, req.ExternalID)
	case "lpitrack_processing":
		tester := strings.TrimSpace(req.AssignedTo)
		if tester == "" {
			tester = "не назначен"
		}
		return legacyExternalKey(req.ExternalID),
			fmt.Sprintf("В работу. Заявка №%s (external_id=%s) взята в работу испытателем %s.",
				req.CustomerNumber, req.ExternalID, tester)
	}
	return "", ""
}
