package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

var jwtSecret []byte

// loadJWTSecret — per-service ключ (Блок D, ревью 1.2): вместо общего JWT_SECRET
// валидируем токены своим {APP}_SERVICE_SECRET (тем же, что у /apps/register).
func loadJWTSecret() error {
	key := strings.ToUpper(appIDFromEnv()) + "_SERVICE_SECRET"
	secret := os.Getenv(key)
	if secret == "" {
		return fmt.Errorf("%s is required", key)
	}
	jwtSecret = []byte(secret)
	return nil
}

type jwtClaims struct {
	Email    string `json:"email"`
	DeviceID string `json:"device_id"`
	AppID    string `json:"app_id"`
	// Channel — "plugin" (Obsidian) или "web" («ЦУП Веб», 2026-09-02, magic-link).
	// Веб-сессиям эффективная роль superadmin клэмпится до admin, см. requirePerm.
	Channel string `json:"channel"`
	jwt.RegisteredClaims
}

func parseJWT(tokenStr string) (*jwtClaims, error) {
	claims, err := parseWithKey(tokenStr, jwtSecret)
	if err != nil {
		// Переходный период (Блок D): принимаем и токены, подписанные прежним
		// общим JWT_SECRET, пока он ещё присутствует в env.
		if legacy := os.Getenv("JWT_SECRET"); legacy != "" {
			if c, e2 := parseWithKey(tokenStr, []byte(legacy)); e2 == nil {
				return c, nil
			}
		}
	}
	return claims, err
}

func parseWithKey(tokenStr string, key []byte) (*jwtClaims, error) {
	claims := &jwtClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return key, nil
	}, jwt.WithExpirationRequired(), jwt.WithLeeway(30*time.Second))
	if err != nil || !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

func claimStringsContains(a jwt.ClaimStrings, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

func appIDFromEnv() string {
	if v := os.Getenv("LAB_APP_ID"); v != "" {
		return v
	}
	return "lab"
}

// Роли lab: viewer(1) < editor(2) < admin(3) < superadmin(4).
func roleRank(role string) int {
	switch role {
	case "superadmin":
		return 4
	case "admin":
		return 3
	case "editor":
		return 2
	case "viewer":
		return 1
	default:
		return 0
	}
}

type permEmailCtx struct{}
type permChannelCtx struct{}
type viewAsRoleCtx struct{}

// clampRoleForChannel — «ЦУП Веб» (2026-09-02): веб-сессии (channel="web")
// никогда не действуют как superadmin, независимо от реальной роли в БД —
// действия уровня superadmin (создание/правка лабораторий, назначение роли
// superadmin другим) доступны только через Obsidian-плагин.
func clampRoleForChannel(role, channel string) string {
	if channel == "web" && role == "superadmin" {
		return "admin"
	}
	return role
}

// normalizeViewAsRole — валидирует заголовок X-View-As-Role («Просмотр от
// лица роли», 2026-09-03): пусто/неизвестное значение → выключено. Заведомо
// не пропускает "superadmin" — не должно быть способом обойти
// clampRoleForChannel, см. effectiveRole.
func normalizeViewAsRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "viewer", "editor", "admin":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func (s *Server) roleFor(ctx context.Context, appID, email string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM lab_permissions WHERE app = $1 AND email = $2`,
		appID, email).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return role, nil
}

// rawRole — реальная роль пользователя (персональная либо общий уровень
// доступа) БЕЗ клэмпа по каналу и БЕЗ учёта «просмотра от лица роли».
// Использовать для проверок прав — effectiveRole; rawRole — только там, где
// нужна настоящая роль независимо от активной симуляции (например, показать
// суперадмину переключатель ролей, даже когда он сам сейчас «смотрит как
// viewer»).
func (s *Server) rawRole(ctx context.Context, appID, email string) (string, error) {
	role, err := s.roleFor(ctx, appID, email)
	if err != nil {
		return "", err
	}
	if role != "" {
		return role, nil
	}
	var level string
	err = s.pool.QueryRow(ctx,
		`SELECT level FROM lab_common_access WHERE app = $1`, appID).Scan(&level)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return level, nil
}

// effectiveRole — роль для ВСЕХ проверок прав и фильтрации видимости: реальная
// роль → клэмп по каналу (web никогда не superadmin) → «просмотр от лица
// роли», если активен (заголовок X-View-As-Role, см. requirePerm/
// normalizeViewAsRole). Подмена возможна ТОЛЬКО когда реальная роль —
// superadmin, канал — web, и запрошенная роль не выше уже клэмпнутой (то есть
// не выше admin) — так режим просмотра не может стать способом эскалации
// прав или обхода запрета "superadmin не действует через веб". Проверяется
// заново на каждый вызов по реальной роли из БД, а не один раз в мидлваре —
// поэтому симуляция ограничивает вообще ВСЁ (видимость данных, gate в
// requirePerm, проверки внутри обработчиков), а не только то, что явно
// вызвало requirePerm.
func (s *Server) effectiveRole(ctx context.Context, appID, email string) (string, error) {
	raw, err := s.rawRole(ctx, appID, email)
	if err != nil {
		return "", err
	}
	channel, _ := ctx.Value(permChannelCtx{}).(string)
	role := clampRoleForChannel(raw, channel)

	if viewAs, ok := ctx.Value(viewAsRoleCtx{}).(string); ok && viewAs != "" &&
		channel == "web" && raw == "superadmin" && roleRank(viewAs) <= roleRank(role) {
		role = viewAs
	}
	return role, nil
}

func (s *Server) requirePerm(minRole string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			tokenStr := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer"))
			if tokenStr == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}

			claims, err := parseJWT(tokenStr)
			if err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
			if claims.AppID != appIDFromEnv() {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
				return
			}
			// Блок D (ревью 1.2): iss/aud — строго, когда присутствуют
			// (старые токены без них допускаются в переходный период).
			if claims.Issuer != "" && claims.Issuer != "auth-service" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
			if len(claims.Audience) > 0 && !claimStringsContains(claims.Audience, appIDFromEnv()) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}

			// Контекст (email/channel/view-as) строится ДО первого вызова
			// effectiveRole — иначе gate ниже проверял бы РЕАЛЬНУЮ роль вместо
			// симулированной, и «просмотр от лица роли» не ограничивал бы
			// доступ к самим роутам, только видимость данных внутри них.
			ctx := context.WithValue(r.Context(), permEmailCtx{}, claims.Email)
			ctx = context.WithValue(ctx, permChannelCtx{}, claims.Channel)
			if viewAs := normalizeViewAsRole(r.Header.Get("X-View-As-Role")); viewAs != "" {
				ctx = context.WithValue(ctx, viewAsRoleCtx{}, viewAs)
			}

			role, err := s.effectiveRole(ctx, claims.AppID, claims.Email)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
				return
			}
			if roleRank(role) < roleRank(minRole) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: insufficient role"})
				return
			}

			next(w, r.WithContext(ctx))
		}
	}
}

// currentEmail возвращает email из контекста (установлен requirePerm).
func currentEmail(r *http.Request) string {
	if v, ok := r.Context().Value(permEmailCtx{}).(string); ok {
		return v
	}
	return ""
}

// currentChannel возвращает channel из контекста (установлен requirePerm).
func currentChannel(r *http.Request) string {
	if v, ok := r.Context().Value(permChannelCtx{}).(string); ok {
		return v
	}
	return ""
}

// requireMinRoleForWebChannel — как requirePerm, но поднимает порог роли
// ТОЛЬКО для channel=web; для channel=plugin — прозрачный проход (базовый
// minRole уже проверил внешний requirePerm). Смена статуса заявки для editor
// через веб запрещена (2026-09-03, по решению пользователя) — кнопки в UI
// остаются видимыми, но неактивными; это финальный страж на случай прямого
// вызова API. Роль пересчитывается через effectiveRole — значит корректно
// учитывает и «просмотр от лица роли» (симулирующий editor/viewer суперадмин
// тоже получит 403, как настоящий editor/viewer).
func (s *Server) requireMinRoleForWebChannel(minRoleForWeb string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if currentChannel(r) != "web" {
				next(w, r)
				return
			}
			role, err := s.effectiveRole(r.Context(), appIDFromEnv(), currentEmail(r))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
				return
			}
			if roleRank(role) < roleRank(minRoleForWeb) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: insufficient role for web"})
				return
			}
			next(w, r)
		}
	}
}
