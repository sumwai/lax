package account_test

import (
	"testing"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/store"
)

func acc(id int64, name string, priority int, models []string, enabled bool, cooldownUntil int64) store.Account {
	return store.Account{
		ID: id, Name: name, Priority: priority, Models: models,
		Enabled: enabled, CooldownUntil: cooldownUntil,
	}
}

func TestCandidatesFiltersAndOrders(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := []store.Account{
		acc(1, "b", 200, []string{"m1"}, true, 0),
		acc(2, "a", 100, []string{"m1"}, true, 0),
		acc(3, "disabled", 50, []string{"m1"}, false, 0),
		acc(4, "cooling", 50, []string{"m1"}, true, now.Unix()+60),
		acc(5, "other-model", 10, []string{"m2"}, true, 0),
		acc(6, "any-model", 300, nil, true, 0),
	}

	got := account.Candidates(accounts, "m1", now, "")
	var names []string
	for _, a := range got {
		names = append(names, a.Name)
	}
	want := []string{"a", "b", "any-model"}
	if len(names) != len(want) {
		t.Fatalf("候选 = %v，期望 %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("候选 = %v，期望 %v", names, want)
		}
	}
}

func TestCandidatesStickySession(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := []store.Account{
		acc(1, "a", 100, []string{"m1"}, true, 0),
		acc(2, "b", 100, []string{"m1"}, true, 0),
		acc(3, "c", 100, []string{"m1"}, true, 0),
	}

	first := account.Candidates(accounts, "m1", now, "session-1")
	again := account.Candidates(accounts, "m1", now, "session-1")
	if len(first) != 3 || len(again) != 3 {
		t.Fatalf("候选数 = %d / %d", len(first), len(again))
	}
	for i := range first {
		if first[i].Name != again[i].Name {
			t.Fatalf("同一会话的结果不稳定: %v vs %v", first, again)
		}
	}

	// 换一个会话，顺序应当可能是别的账号打头，但集合不变。
	seen := map[string]bool{}
	for _, a := range first {
		seen[a.Name] = true
	}
	for _, a := range account.Candidates(accounts, "m1", now, "session-2") {
		if !seen[a.Name] {
			t.Fatalf("换了会话后出现了集合外的账号 %q", a.Name)
		}
	}
}

func TestCooldownGrowsAndCaps(t *testing.T) {
	if d := account.Cooldown(429, 0); d != time.Minute {
		t.Fatalf("429 首次冷却 = %v", d)
	}
	if d := account.Cooldown(500, 0); d != 10*time.Second {
		t.Fatalf("5xx 首次冷却 = %v", d)
	}
	if d := account.Cooldown(503, 1); d != 20*time.Second {
		t.Fatalf("连败两次冷却 = %v", d)
	}
	if d := account.Cooldown(500, 60); d != account.MaxCooldown {
		t.Fatalf("冷却上限 = %v，期望 %v", d, account.MaxCooldown)
	}
}

func TestRetryable(t *testing.T) {
	cases := map[int]bool{
		429: true, 408: true, 500: true, 503: true,
		400: false, 401: false, 403: false, 404: false, 200: false,
	}
	for status, want := range cases {
		if got := account.Retryable(status); got != want {
			t.Fatalf("Retryable(%d) = %v，期望 %v", status, got, want)
		}
	}
}
