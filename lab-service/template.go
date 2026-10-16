package main

import _ "embed"

// standardTemplateDocx — корпоративный бланк организации (логотип в шапке,
// реквизиты/QR/водяной знак в футере) — 2026-10-16, для отчёта по испытанию
// (см. report_docx.go, reportDocx). Копия бланка из sbe-agent/agent-service
// (template/standard.docx); источник — «Стандартный шаблон.docx» в корне вольта.
// Тело документа заменяется сгенерированным содержимым (splitTemplateBody),
// шапка/футер/тема/медиа берутся как есть.
//
//go:embed template/standard.docx
var standardTemplateDocx []byte
