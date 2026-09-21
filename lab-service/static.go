package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// Страница Swagger UI и схема OpenAPI (2026-09-21, правило корневого AGENTS.md:
// сервис обязан сам показывать, что он умеет).
//
// Дистрибутив UI вшит в бинарь, без CDN: сервис должен работать без внешней
// сети. Лицензия Apache-2.0 (static/swagger/LICENSE) — правилу о свободных
// библиотеках соответствует.
//
// Пути обязательно внутри /api/lab/ (см. main.go): Caddy проксирует сервису
// только этот префикс, корневой /docs/ снаружи не откроется. Образец, с
// которого это снято (C:\Obsidian\ot-p\backend), отдаёт UI по корневому пути —
// скопировать его как есть было бы граблями.

//go:embed static/swagger
var swaggerUIFS embed.FS

// swaggerFS — под-ФС с корнем в static/swagger, чтобы http.FileServer отдавал
// index.html прямо по /api/lab/docs/, а не листинг встроенного корня.
var swaggerFS, _ = fs.Sub(swaggerUIFS, "static/swagger")

//go:embed static/openapi.yaml
var openapiYAML []byte

// serveOpenAPI отдаёт схему OpenAPI. Схема открыта (страница доступна без
// ключа), поэтому секретов, токенов и примеров с реальными данными в ней нет.
func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapiYAML)
}
