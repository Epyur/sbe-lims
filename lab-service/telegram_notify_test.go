package main

import (
	"strings"
	"testing"
)

// fakeRule — строка правила для подставного lookup (без БД).
type fakeRule struct {
	deleted        bool
	onCreated      bool
	onCompleted    bool
	withAttachment bool
	channels       []telegramChannel
}

// makeLookup собирает telegramRuleLookup из двух карт: родители проектов и
// правила «проект → метод → строка».
func makeLookup(parents map[int64]int64, rules map[int64]map[int64]fakeRule) telegramRuleLookup {
	return func(projectID, methodID int64) (*telegramRule, int64, bool, error) {
		if byMethod, ok := rules[projectID]; ok {
			if r, ok := byMethod[methodID]; ok {
				return &telegramRule{
					MethodID:       methodID,
					Deleted:        r.deleted,
					Channels:       r.channels,
					OnCreated:      r.onCreated,
					OnCompleted:    r.onCompleted,
					WithAttachment: r.withAttachment,
				}, parents[projectID], true, nil
			}
		}
		return nil, parents[projectID], false, nil
	}
}

func TestResolveTelegramRuleNearestAncestorWins(t *testing.T) {
	parents := map[int64]int64{2: 1, 1: 0}
	rules := map[int64]map[int64]fakeRule{
		1: {5: {onCreated: true, channels: []telegramChannel{{ChatID: "-100", Label: "Родитель"}}}},
	}
	got, src, err := resolveTelegramRule(5, 2, makeLookup(parents, rules))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected parent rule to be inherited, got nil")
	}
	if len(got.Channels) != 1 || got.Channels[0].ChatID != "-100" {
		t.Fatalf("wrong inherited rule: %+v", got)
	}
	if !got.OnCreated || src != 1 {
		t.Fatalf("expected rule from project 1 with OnCreated, got %+v src=%d", got, src)
	}
}

func TestResolveTelegramRuleOwnActiveBeatsParent(t *testing.T) {
	parents := map[int64]int64{2: 1, 1: 0}
	rules := map[int64]map[int64]fakeRule{
		2: {5: {onCompleted: true, channels: []telegramChannel{{ChatID: "-200", Label: "Своё"}}}},
		1: {5: {onCreated: true, channels: []telegramChannel{{ChatID: "-100", Label: "Родитель"}}}},
	}
	got, src, err := resolveTelegramRule(5, 2, makeLookup(parents, rules))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got.Channels) != 1 || got.Channels[0].ChatID != "-200" {
		t.Fatalf("expected own rule, got %+v", got)
	}
	if got.OnCreated || src != 2 {
		t.Fatalf("parent must not leak: %+v src=%d", got, src)
	}
}

func TestResolveTelegramRuleExclusionStopsInheritance(t *testing.T) {
	parents := map[int64]int64{2: 1, 1: 0}
	rules := map[int64]map[int64]fakeRule{
		2: {5: {deleted: true}},
		1: {5: {onCreated: true, channels: []telegramChannel{{ChatID: "-100"}}}},
	}
	got, src, err := resolveTelegramRule(5, 2, makeLookup(parents, rules))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil || src != 0 {
		t.Fatalf("exclusion must stop inheritance, got %+v src=%d", got, src)
	}
}

func TestResolveTelegramRuleExclusionInMiddleStopsDescendants(t *testing.T) {
	parents := map[int64]int64{3: 2, 2: 1, 1: 0}
	rules := map[int64]map[int64]fakeRule{
		2: {5: {deleted: true}},
		1: {5: {onCreated: true, channels: []telegramChannel{{ChatID: "-100"}}}},
	}
	got, src, err := resolveTelegramRule(5, 3, makeLookup(parents, rules))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil || src != 0 {
		t.Fatalf("middle exclusion must stop descendants, got %+v src=%d", got, src)
	}
}

func TestResolveTelegramRuleNoProjectNoRules(t *testing.T) {
	called := 0
	lookup := func(projectID, methodID int64) (*telegramRule, int64, bool, error) {
		called++
		return nil, 0, false, nil
	}
	got, src, err := resolveTelegramRule(5, 0, lookup)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil || src != 0 {
		t.Fatalf("no project → no rules, got %+v src=%d", got, src)
	}
	if called != 0 {
		t.Fatalf("lookup must not be called for project 0, called %d times", called)
	}
}

func TestResolveTelegramRuleNoRowsAnywhere(t *testing.T) {
	parents := map[int64]int64{2: 1, 1: 0}
	got, src, err := resolveTelegramRule(5, 2, makeLookup(parents, map[int64]map[int64]fakeRule{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil || src != 0 {
		t.Fatalf("expected nil when no rows in chain, got %+v src=%d", got, src)
	}
}

func TestTelegramRuleMatchesEvent(t *testing.T) {
	created := &telegramRule{OnCreated: true}
	if !telegramRuleMatchesEvent(created, "created") {
		t.Fatal("created rule must match created event")
	}
	if telegramRuleMatchesEvent(created, "completed") {
		t.Fatal("created rule must not match completed event")
	}
	completed := &telegramRule{OnCompleted: true}
	if !telegramRuleMatchesEvent(completed, "completed") {
		t.Fatal("completed rule must match completed event")
	}
	if telegramRuleMatchesEvent(nil, "created") {
		t.Fatal("nil rule must match nothing")
	}
}

func TestValidateTelegramRulesRejectsDuplicateMethods(t *testing.T) {
	dup := []telegramRule{
		{MethodID: 1, Channels: []telegramChannel{{ChatID: "-100"}}},
		{MethodID: 1, Channels: []telegramChannel{{ChatID: "-200"}}},
	}
	if err := validateTelegramRules(dup); err == nil {
		t.Fatal("duplicate method_id must be rejected")
	}
}

func TestValidateTelegramRulesRejectsDeletedWithChannels(t *testing.T) {
	rules := []telegramRule{{MethodID: 1, Deleted: true, Channels: []telegramChannel{{ChatID: "-100"}}}}
	if err := validateTelegramRules(rules); err == nil {
		t.Fatal("deleted rule with channels must be rejected")
	}
}

func TestValidateTelegramRulesRejectsMultipleChannels(t *testing.T) {
	rules := []telegramRule{{MethodID: 1, Channels: []telegramChannel{{ChatID: "-100"}, {ChatID: "-200"}}}}
	if err := validateTelegramRules(rules); err == nil {
		t.Fatal("more than one channel per method must be rejected")
	}
}

func TestValidateTelegramRulesRejectsNegativeThread(t *testing.T) {
	rules := []telegramRule{{MethodID: 1, Channels: []telegramChannel{{ChatID: "-100", ThreadID: -5}}}}
	if err := validateTelegramRules(rules); err == nil {
		t.Fatal("negative thread_id must be rejected")
	}
}

func TestValidateTelegramRulesRejectsEmptyChatID(t *testing.T) {
	rules := []telegramRule{{MethodID: 1, Channels: []telegramChannel{{ChatID: "  "}}}}
	if err := validateTelegramRules(rules); err == nil {
		t.Fatal("empty chat_id must be rejected")
	}
}

func TestValidateTelegramRulesAcceptsValidSet(t *testing.T) {
	rules := []telegramRule{
		{MethodID: 1, Channels: []telegramChannel{{ChatID: "-100", Label: "А"}}, OnCreated: true},
		{MethodID: 2, Deleted: true},
	}
	if err := validateTelegramRules(rules); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
}

func TestTelegramNotifyAttemptsCap(t *testing.T) {
	if got := nextAttemptStatus(0); got != "pending" {
		t.Fatalf("first failure must stay pending, got %q", got)
	}
	if got := nextAttemptStatus(maxTelegramNotifyAttempts - 2); got != "pending" {
		t.Fatalf("before cap must stay pending, got %q", got)
	}
	if got := nextAttemptStatus(maxTelegramNotifyAttempts - 1); got != "failed" {
		t.Fatalf("at cap must fail, got %q", got)
	}
}

func TestTelegramNotifyAttachmentFallback(t *testing.T) {
	if attachReport(false, 100) {
		t.Fatal("no attachment requested — must not attach")
	}
	if attachReport(true, 0) {
		t.Fatal("empty report — must fall back to text")
	}
	if !attachReport(true, 100) {
		t.Fatal("requested and non-empty — must attach")
	}
}

func TestBuildTelegramTextCreated(t *testing.T) {
	req := &Request{
		ID:             123,
		CustomerNumber: "OLD-1/2026-ГГ",
		OwnerEmail:     "a@tn.ru",
		CreatedAt:      "2026-10-18T10:30:00Z",
	}
	names := telegramNames{ProjectName: "Кровли", MethodLabel: "ГГ · Горючесть", ObjectName: "Мембрана"}
	text := buildTelegramText(req, "created", names)
	for _, want := range []string{
		"🆕 Создана заявка №OLD-1/2026-ГГ",
		"Проект: Кровли",
		"Метод: ГГ · Горючесть",
		"Объект: Мембрана",
		"Заказчик: a@tn.ru",
		"🔗 https://epyur.fvds.ru/app/#/requests/123",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestBuildTelegramTextCompletedNoCompliance(t *testing.T) {
	req := &Request{
		ID:             7,
		CustomerNumber: "7/2026-ГВ",
		OwnerEmail:     "b@tn.ru",
		CompletedAt:    "2026-10-18T11:00:00Z",
	}
	text := buildTelegramText(req, "completed", telegramNames{})
	if !strings.Contains(text, "✅ Выполнена заявка №7/2026-ГВ") {
		t.Fatalf("missing completed header in:\n%s", text)
	}
	if !strings.Contains(text, "Завершена:") {
		t.Fatalf("missing completion date in:\n%s", text)
	}
	if strings.Contains(text, "Результат:") || strings.Contains(text, "Соответствие:") {
		t.Fatalf("empty result/compliance must not add lines:\n%s", text)
	}
}

func TestBuildTelegramTextCompletedWithCompliance(t *testing.T) {
	req := &Request{
		ID:             8,
		CustomerNumber: "8/2026-ГГ",
		OwnerEmail:     "c@tn.ru",
		CompletedAt:    "2026-10-18T12:00:00Z",
		Result:         "Г4",
		Compliance:     "Не соответствует",
	}
	text := buildTelegramText(req, "completed", telegramNames{})
	if !strings.Contains(text, "Результат: Г4") {
		t.Fatalf("missing result line:\n%s", text)
	}
	if !strings.Contains(text, "Соответствие: Не соответствует") {
		t.Fatalf("missing compliance line:\n%s", text)
	}
}
