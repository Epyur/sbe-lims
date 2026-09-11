package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ВНИМАНИЕ: этот файл ИДЕНТИЧЕН во всех plugin-сервисах. Правка — сразу во всех
// копиях; тест живёт в photo-service (authperms_test.go).
//
// Списка сервисов и их числа здесь СОЗНАТЕЛЬНО НЕТ. Пока они были в этой шапке,
// каждый новый сервис на центральных правах заставлял править все копии: три
// таких захода подряд (2026-09-10 и 2026-09-11) не изменили ни строки кода, но
// стоили двух с половиной десятков коммитов. Актуальный список — в корневом
// AGENTS.md папки плагинов, он вне git и правится в одном месте.
//
// Общим go-модулем файл не сделан сознательно: у сервисов отдельные модули и
// отдельные Docker-контексты сборки, ради ста строк транспорта переделывать
// сборку каждого дороже (см. дизайн
// docs/superpowers/specs/2026-09-09-central-permissions-design.md, раздел
// «Компромисс, который остаётся»).
//
// Копии можно сверять побайтово: с 2026-09-11 в каждом репозитории с Go-кодом
// лежит .gitattributes с `*.go text eol=lf`, поэтому переводы строк везде
// одинаковые. До этого на Windows git подставлял в рабочие копии CRLF
// (core.autocrlf=true), из-за чего копии расходились побайтово при одинаковом
// содержимом, а `gofmt -l` шумел на сотне файлов. Если сверка вдруг снова
// покажет расхождение только в переводах строк — проверять .gitattributes.
//
// Что делает: держит карту «email → роль» своего приложения и уровень общего
// доступа, полученные из auth-service — единственного источника правды с
// 2026-09-09. Раньше каждый сервис читал свою таблицу {app}_permissions, и семь
// копий кода прав разошлись (в Фотобанке «Нет общего доступа» молча не
// работал).

const (
	// Насколько карта считается свежей. 30 секунд — компромисс между «отзыв
	// прав виден почти сразу» и «не ходим в auth на каждый запрос».
	permsFreshFor = 30 * time.Second
	// Таймаут обхода за картой: auth-service рядом в docker-сети.
	permsFetchTimeout = 5 * time.Second
)

// permSnapshot — карта прав приложения на момент последнего успешного обхода.
type permSnapshot struct {
	Roles           map[string]string
	CommonAccess    string
	CommonAccessSet bool
	FetchedAt       time.Time
}

// permCache — кэш карты прав. Пустой кэш до первого успешного ответа: роли
// резолвятся только для глобальных админов (их список сервис знает из env).
type permCache struct {
	mu       sync.RWMutex
	snap     permSnapshot
	loaded   bool
	lastWarn time.Time
}

var appPerms permCache

// authServiceURL — адрес auth-service внутри docker-сети.
func authServiceURL() string {
	if v := strings.TrimSpace(os.Getenv("AUTH_SERVICE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://auth-service:3000"
}

func serviceSecret() string {
	return strings.TrimSpace(os.Getenv(strings.ToUpper(appIDFromEnv()) + "_SERVICE_SECRET"))
}

// permissionsSnapshot — актуальная карта прав. Если кэш протух, ходит в
// auth-service; если тот не ответил — ВОЗВРАЩАЕТ ПОСЛЕДНЮЮ ИЗВЕСТНУЮ карту и
// пишет предупреждение (не чаще раза в минуту, чтобы не залить лог). Полное
// закрытие доступа при недоступности auth означало бы, что падение одного
// сервиса гасит все плагины разом.
func (s *Server) permissionsSnapshot(ctx context.Context) permSnapshot {
	appPerms.mu.RLock()
	snap, loaded := appPerms.snap, appPerms.loaded
	appPerms.mu.RUnlock()
	if loaded && time.Since(snap.FetchedAt) < permsFreshFor {
		return snap
	}

	fresh, err := fetchPermissions(ctx)
	if err != nil {
		appPerms.mu.Lock()
		if time.Since(appPerms.lastWarn) > time.Minute {
			log.Printf("auth-service недоступен, работаем по последним известным правам (загружены: %v): %v",
				snap.FetchedAt.Format(time.RFC3339), err)
			appPerms.lastWarn = time.Now()
		}
		appPerms.mu.Unlock()
		return snap
	}
	appPerms.mu.Lock()
	appPerms.snap = fresh
	appPerms.loaded = true
	appPerms.mu.Unlock()
	return fresh
}

// fetchPermissions — один обход за картой прав приложения.
func fetchPermissions(ctx context.Context) (permSnapshot, error) {
	secret := serviceSecret()
	if secret == "" {
		return permSnapshot{}, fmt.Errorf("нет %s_SERVICE_SECRET", strings.ToUpper(appIDFromEnv()))
	}
	url := fmt.Sprintf("%s/internal/apps/%s/permissions", authServiceURL(), appIDFromEnv())
	reqCtx, cancel := context.WithTimeout(ctx, permsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return permSnapshot{}, err
	}
	req.Header.Set("X-Service-Secret", secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return permSnapshot{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return permSnapshot{}, err
	}
	if res.StatusCode != http.StatusOK {
		return permSnapshot{}, fmt.Errorf("auth-service ответил %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Permissions     map[string]string `json:"permissions"`
		CommonAccess    string            `json:"common_access"`
		CommonAccessSet bool              `json:"common_access_set"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return permSnapshot{}, err
	}
	roles := make(map[string]string, len(parsed.Permissions))
	for email, role := range parsed.Permissions {
		roles[strings.ToLower(strings.TrimSpace(email))] = role
	}
	return permSnapshot{
		Roles:           roles,
		CommonAccess:    strings.TrimSpace(parsed.CommonAccess),
		CommonAccessSet: parsed.CommonAccessSet,
		FetchedAt:       time.Now(),
	}, nil
}

// roleFromSnapshot — роль пользователя по карте: персональная, иначе общий
// доступ. Пустой уровень общего доступа — это «доступа нет», а НЕ «настройка не
// задана»: подмена пустого значения на viewer как раз и была багом Фотобанка.
// Записи об общем доступе нет вовсе — действует историческое значение
// приложения (defaultLevel).
func roleFromSnapshot(snap permSnapshot, email, defaultLevel string) string {
	if role, ok := snap.Roles[strings.ToLower(strings.TrimSpace(email))]; ok && strings.TrimSpace(role) != "" {
		return strings.TrimSpace(role)
	}
	if snap.CommonAccessSet {
		return snap.CommonAccess
	}
	return defaultLevel
}

// setPermissionUpstream — прокси записи роли в auth-service от имени
// пользователя (право проверяет auth-service).
func setPermissionUpstream(ctx context.Context, actorEmail, email, role string) error {
	return postUpstream(ctx, "permissions", map[string]string{
		"actor_email": actorEmail, "email": email, "role": role,
	})
}

// setCommonAccessUpstream — прокси записи общего доступа в auth-service.
func setCommonAccessUpstream(ctx context.Context, actorEmail, level string) error {
	return postUpstream(ctx, "common-access", map[string]string{
		"actor_email": actorEmail, "level": level,
	})
}

func postUpstream(ctx context.Context, path string, payload map[string]string) error {
	secret := serviceSecret()
	if secret == "" {
		return fmt.Errorf("нет %s_SERVICE_SECRET", strings.ToUpper(appIDFromEnv()))
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/internal/apps/%s/%s", authServiceURL(), appIDFromEnv(), path)
	reqCtx, cancel := context.WithTimeout(ctx, permsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("X-Service-Secret", secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("auth-service ответил %d: %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Права изменились — карту в кэше считаем протухшей, чтобы следующий запрос
	// её перечитал и изменение подействовало сразу, а не через 30 секунд.
	appPerms.mu.Lock()
	appPerms.snap.FetchedAt = time.Time{}
	appPerms.mu.Unlock()
	return nil
}
