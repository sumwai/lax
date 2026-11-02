package account_test

import (
	"testing"
	"time"

	"lax/internal/account"
	"lax/internal/store"
)

func acc(id int64, name string, priority int, models []string, enabled bool, cooldownUntil int64) store.Account {
	a := store.Account{
		ID: id, Name: name, Priority: priority, Models: models,
		Enabled: enabled,
	}
	if cooldownUntil > 0 {
		a.ModelFailures = map[string]store.ModelFailure{}
		for _, model := range models {
			a.ModelFailures[model] = store.ModelFailure{
				CooldownUntilMS: time.Unix(cooldownUntil, 0).UnixMilli(),
			}
		}
	}
	return a
}

func TestCandidatesFiltersAndOrders(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := []store.Account{
		acc(1, "b", 200, []string{"m1"}, true, 0),
		acc(2, "a", 100, []string{"m1"}, true, 0),
		acc(3, "disabled", 50, []string{"m1"}, false, 0),
		acc(4, "cooling", 50, []string{"m1"}, true, now.Unix()+60),
		acc(5, "other-model", 10, []string{"m2"}, true, 0),
		// 模型列表为空的账号不提供任何模型，不参与选号（即使 priority 最小、没有冷却）。
		acc(6, "no-models", 1, nil, true, 0),
	}

	got := account.Candidates(accounts, "m1", now, "")
	var names []string
	for _, a := range got {
		names = append(names, a.Name)
	}
	want := []string{"a", "b"}
	if len(names) != len(want) {
		t.Fatalf("候选 = %v，期望 %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("候选 = %v，期望 %v", names, want)
		}
	}
}

func TestSupportsEmptyModelListMeansNoModels(t *testing.T) {
	fresh := acc(1, "fresh", 100, nil, true, 0)
	if account.Supports(fresh, "anything") {
		t.Fatal("模型列表为空的账号不该支持任何模型")
	}
	fresh.Models = []string{}
	if account.Supports(fresh, "anything") {
		t.Fatal("空切片与 nil 同样表示不提供任何模型")
	}
	if !account.Supports(acc(2, "with-model", 100, []string{"m1"}, true, 0), "m1") {
		t.Fatal("列表里有该模型时应支持")
	}
}

// Providers 不看 enabled 与冷却：它回答的是「清单里有没有人提供这个模型」。
func TestProvidersIgnoresAvailability(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := []store.Account{
		acc(1, "cooling", 100, []string{"m1"}, true, now.Unix()+60),
		acc(2, "disabled", 100, []string{"m1"}, false, 0),
		acc(3, "other-model", 100, []string{"m2"}, true, 0),
		acc(4, "no-models", 100, nil, true, 0),
	}
	got := account.Providers(accounts, "m1")
	if len(got) != 2 || got[0].Name != "cooling" || got[1].Name != "disabled" {
		t.Fatalf("Providers = %+v，期望 cooling 与 disabled", got)
	}
	if got := account.Providers(accounts, "m9"); len(got) != 0 {
		t.Fatalf("没有账号提供 m9 时 Providers = %+v", got)
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

// 冷却按账号-模型隔离：同一个账号的一个模型冷却时，另一个模型照样是候选。
func TestCandidatesCooldownIsPerModel(t *testing.T) {
	now := time.Unix(1000, 0)
	a := acc(1, "a", 100, []string{"m1", "m2"}, true, 0)
	a.ModelFailures = map[string]store.ModelFailure{
		"m1": {CooldownUntilMS: now.Add(time.Minute).UnixMilli(), FailStreak: 2},
	}
	if got := account.Candidates([]store.Account{a}, "m1", now, ""); len(got) != 0 {
		t.Fatalf("m1 冷却时仍有候选: %+v", got)
	}
	if got := account.Candidates([]store.Account{a}, "m2", now, ""); len(got) != 1 {
		t.Fatalf("m2 不该被 m1 的冷却挡住: %+v", got)
	}
	if got := account.FailStreak(a, "m1"); got != 2 {
		t.Fatalf("m1 连续失败次数 = %d，期望 2", got)
	}
	if got := account.FailStreak(a, "m2"); got != 0 {
		t.Fatalf("m2 连续失败次数 = %d，期望 0", got)
	}
}

func TestCooldownGrowsAndCaps(t *testing.T) {
	// 三档起始值、翻倍序列与上限都一样长，只有底数不同：连接层 2 秒、上游 5xx 10 秒、
	// 限流 500 毫秒。
	cases := []struct {
		name string
		kind account.FailureKind
		base time.Duration
	}{
		{"连接层", account.KindTransport, 2 * time.Second},
		{"上游 5xx", account.KindServer, 10 * time.Second},
		{"限流", account.KindRateLimit, 500 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if d := account.Cooldown(c.kind, 0); d != c.base {
				t.Fatalf("首次冷却 = %v，期望 %v", d, c.base)
			}
			if d := account.Cooldown(c.kind, 1); d != 2*c.base {
				t.Fatalf("连败两次冷却 = %v，期望 %v", d, 2*c.base)
			}
			if d := account.Cooldown(c.kind, 2); d != 4*c.base {
				t.Fatalf("连败三次冷却 = %v，期望 %v", d, 4*c.base)
			}
			if d := account.Cooldown(c.kind, 60); d != account.MaxCooldown {
				t.Fatalf("冷却上限 = %v，期望 %v", d, account.MaxCooldown)
			}
		})
	}
}

// TestFailureKind 钉住状态码与冷却档的对应，以及没有给出档位时按服务端那一档兜底：
// 502 这类状态本身说明不了失败来自哪一层，定档要靠调用方显式给 KindTransport。
func TestFailureKind(t *testing.T) {
	if got := account.KindFromStatus(429); got != account.KindRateLimit {
		t.Fatalf("429 的档位 = %v，期望限流", got)
	}
	for _, status := range []int{500, 502, 503} {
		if got := account.KindFromStatus(status); got != account.KindServer {
			t.Fatalf("%d 的档位 = %v，期望服务端", status, got)
		}
	}
	if got := account.Cooldown(account.FailureKind(0), 0); got != account.CooldownServer {
		t.Fatalf("零值档位的冷却 = %v，期望按服务端 %v", got, account.CooldownServer)
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
