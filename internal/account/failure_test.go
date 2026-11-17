package account_test

import (
	"strings"
	"testing"
	"time"

	"lax/internal/account"
	"lax/internal/store"
)

// matchPolicy 造一条用户规则，默认 return、retry_base 500ms。
func matchPolicy(id int64, mutate func(*store.RouteFailurePolicy)) store.RouteFailurePolicy {
	p := store.RouteFailurePolicy{
		ID: id, Enabled: true, Priority: int(id), ReturnError: true, RetryBaseMS: 500,
	}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

func TestMatchFailureOrderAndDisabled(t *testing.T) {
	facts := account.FailureFacts{Status: 503, Message: "upstream down", Provider: "p", Model: "m1", AccountID: 1}

	first := matchPolicy(1, func(p *store.RouteFailurePolicy) {
		p.Statuses = []string{"5xx"}
		p.Note = "第一条"
	})
	second := matchPolicy(2, func(p *store.RouteFailurePolicy) {
		p.Statuses = []string{"5xx"}
		p.NextRoute = true
		p.ReturnError = false
		p.Note = "第二条"
	})
	decision, ok := account.MatchFailure(facts, []store.RouteFailurePolicy{first, second})
	if !ok {
		t.Fatal("应有规则命中")
	}
	if decision.PolicyID != 1 || decision.Name != "第一条" || !decision.ReturnError || decision.NextRoute {
		t.Fatalf("命中 = %+v，期望第一条且 return", decision)
	}
	if decision.Source != account.FailureSourceUser {
		t.Fatalf("来源 = %q，期望 user", decision.Source)
	}

	disabled := matchPolicy(0, func(p *store.RouteFailurePolicy) {
		p.Enabled = false
		p.Statuses = []string{"5xx"}
		p.Note = "停用规则"
	})
	decision, ok = account.MatchFailure(facts, []store.RouteFailurePolicy{disabled, second})
	if !ok || decision.PolicyID != 2 {
		t.Fatalf("停用的用户规则应被跳过，命中 = %+v, %v", decision, ok)
	}
}

func TestMatchFailureConditions(t *testing.T) {
	base := account.FailureFacts{
		Status: 429, Message: "Rate Limited. Please slow down.",
		Provider: "cmd", Model: "m1", AccountID: 7,
	}
	cases := []struct {
		name   string
		mutate func(*store.RouteFailurePolicy)
		facts  account.FailureFacts
		want   bool
	}{
		{"状态精确命中", func(p *store.RouteFailurePolicy) { p.Statuses = []string{"429"} }, base, true},
		{"4xx 段命中", func(p *store.RouteFailurePolicy) { p.Statuses = []string{"4xx"} }, base, true},
		{"5xx 段不命中", func(p *store.RouteFailurePolicy) { p.Statuses = []string{"5xx"} }, base, false},
		{"状态为空表示任意", func(p *store.RouteFailurePolicy) { p.Statuses = nil }, base, true},
		{"账号不符", func(p *store.RouteFailurePolicy) { p.AccountID = 8 }, base, false},
		{"厂商不符", func(p *store.RouteFailurePolicy) { p.Provider = "other" }, base, false},
		{"模型不符", func(p *store.RouteFailurePolicy) { p.Model = "m2" }, base, false},
		{"错误文本大小写不敏感", func(p *store.RouteFailurePolicy) { p.ErrorContains = []string{"rate limited"} }, base, true},
		{"错误文本 OR", func(p *store.RouteFailurePolicy) { p.ErrorContains = []string{"nope", "slow down"} }, base, true},
		{"错误文本都不命中", func(p *store.RouteFailurePolicy) { p.ErrorContains = []string{"nope"} }, base, false},
		{"正则命中", func(p *store.RouteFailurePolicy) { p.ErrorRegex = `Rate\s+Limit` }, base, true},
		{"正则默认区分大小写", func(p *store.RouteFailurePolicy) { p.ErrorRegex = `rate` }, base, false},
		{"包含与正则都要命中", func(p *store.RouteFailurePolicy) {
			p.ErrorContains = []string{"rate"}
			p.ErrorRegex = `nope`
		}, base, false},
		{"包含与正则同时命中", func(p *store.RouteFailurePolicy) {
			p.ErrorContains = []string{"rate"}
			p.ErrorRegex = `Rate\s+Limit`
		}, base, true},
		{"条件之间是 AND", func(p *store.RouteFailurePolicy) {
			p.Statuses = []string{"4xx"}
			p.Provider = "cmd"
			p.Model = "m1"
			p.ErrorContains = []string{"slow"}
		}, base, true},
		{"任一条件不符即不命中", func(p *store.RouteFailurePolicy) {
			p.Statuses = []string{"4xx"}
			p.Provider = "other"
		}, base, false},
		{"传输失败按 502 匹配 5xx", func(p *store.RouteFailurePolicy) {
			p.Statuses = []string{"5xx"}
		}, account.FailureFacts{Status: 502, Message: "EOF", Transport: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := matchPolicy(1, tc.mutate)
			_, got := account.MatchFailure(tc.facts, []store.RouteFailurePolicy{p})
			if got != tc.want {
				t.Fatalf("命中 = %v，期望 %v（策略 %+v）", got, tc.want, p)
			}
		})
	}
}

func TestBuiltinFailureFallbacks(t *testing.T) {
	cases := []struct {
		name        string
		facts       account.FailureFacts
		want        bool
		disable     bool
		nextRoute   bool
		returnError bool
	}{
		{"401 停用并返回", account.FailureFacts{Status: 401, Message: "invalid key"}, true, true, false, true},
		{"402 停用并换号", account.FailureFacts{Status: 402, Message: ""}, true, true, true, false},
		{"400 计费关键词", account.FailureFacts{Status: 400, Message: "You have insufficient credits to make this request"}, true, true, true, false},
		{"403 中文计费关键词", account.FailureFacts{Status: 403, Message: "余额不足，请充值"}, true, true, true, false},
		{"400 普通错误不命中", account.FailureFacts{Status: 400, Message: "bad request"}, false, false, false, false},
		{"403 普通错误不命中", account.FailureFacts{Status: 403, Message: "forbidden"}, false, false, false, false},
		{"裸 credit 不命中", account.FailureFacts{Status: 403, Message: "invalid credit card"}, false, false, false, false},
		{"500 不命中内置", account.FailureFacts{Status: 500, Message: "boom"}, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, got := account.MatchFailure(tc.facts, nil)
			if got != tc.want {
				t.Fatalf("命中 = %v，期望 %v", got, tc.want)
			}
			if !got {
				return
			}
			if decision.Source != account.FailureSourceBuiltin || decision.PolicyID != 0 || decision.Name == "" {
				t.Fatalf("内置决策 = %+v，期望 source=builtin、id=0、有名称", decision)
			}
			if decision.Disable != tc.disable || decision.NextRoute != tc.nextRoute || decision.ReturnError != tc.returnError {
				t.Fatalf("内置动作 = disable %v / next %v / return %v，期望 %v / %v / %v",
					decision.Disable, decision.NextRoute, decision.ReturnError, tc.disable, tc.nextRoute, tc.returnError)
			}
			if !tc.disable && !tc.nextRoute && !tc.returnError {
				t.Fatal("内置规则不应没有任何动作")
			}
		})
	}

	// 用户规则优先于内置：401 只返回，不停用。
	user := matchPolicy(9, func(p *store.RouteFailurePolicy) {
		p.Statuses = []string{"401"}
		p.Note = "401 只返回"
	})
	decision, ok := account.MatchFailure(account.FailureFacts{Status: 401, Message: "invalid key"}, []store.RouteFailurePolicy{user})
	if !ok || decision.PolicyID != 9 || decision.Disable || !decision.ReturnError || decision.Source != account.FailureSourceUser {
		t.Fatalf("用户 401 规则应覆盖内置，决策 = %+v", decision)
	}
}

func TestBuiltinFailurePoliciesAreCopies(t *testing.T) {
	got := account.BuiltinFailurePolicies()
	if len(got) != 3 {
		t.Fatalf("内置规则 = %d 条，期望 3 条", len(got))
	}
	if !strings.Contains(got[0].Note, "401") || !strings.Contains(got[1].Note, "402") || !strings.Contains(got[2].Note, "400") {
		t.Fatalf("内置规则顺序或名称不对: %+v", got)
	}
	got[0].Statuses[0] = "999"
	again := account.BuiltinFailurePolicies()
	if again[0].Statuses[0] != "401" {
		t.Fatal("BuiltinFailurePolicies 必须返回副本，调用方改动不能污染内置规则")
	}
}

func TestFailureDecisionActions(t *testing.T) {
	facts := account.FailureFacts{Status: 500, Message: "boom", Model: "m1"}

	// 模型级冷却：带了 model，只冷却账号-模型。
	modelScoped := matchPolicy(1, func(p *store.RouteFailurePolicy) {
		p.Statuses = []string{"5xx"}
		p.Model = "m1"
		p.CooldownSeconds = 60
		p.ReturnError = false
		p.NextRoute = true
	})
	d, ok := account.MatchFailure(facts, []store.RouteFailurePolicy{modelScoped})
	if !ok || d.Cooldown != time.Minute || !d.ModelScopedCooldown || !d.NextRoute || d.ReturnError {
		t.Fatalf("模型级冷却决策 = %+v", d)
	}

	// 没带 model：账号级冷却。
	accountScoped := matchPolicy(2, func(p *store.RouteFailurePolicy) {
		p.Statuses = []string{"5xx"}
		p.CooldownSeconds = 30 * 60
		p.ReturnError = false
		p.NextRoute = true
	})
	d, ok = account.MatchFailure(facts, []store.RouteFailurePolicy{accountScoped})
	if !ok || d.Cooldown != 30*time.Minute || d.ModelScopedCooldown {
		t.Fatalf("账号级冷却决策 = %+v", d)
	}

	// 流程位都没写时按 return；重试次数夹到上限。
	defaulted := matchPolicy(3, func(p *store.RouteFailurePolicy) {
		p.Statuses = []string{"5xx"}
		p.ReturnError = false
		p.RetryCount = 99
	})
	d, ok = account.MatchFailure(facts, []store.RouteFailurePolicy{defaulted})
	if !ok || !d.ReturnError || d.NextRoute || d.RetryCount != account.MaxRetryCount {
		t.Fatalf("默认流程/重试上限决策 = %+v", d)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		base  time.Duration
		index int
		want  time.Duration
	}{
		{500 * time.Millisecond, 1, 500 * time.Millisecond},
		{500 * time.Millisecond, 2, time.Second},
		{500 * time.Millisecond, 3, 2 * time.Second},
		{500 * time.Millisecond, 4, 4 * time.Second},
		{500 * time.Millisecond, 5, 8 * time.Second},
		{500 * time.Millisecond, 6, 8 * time.Second},
		{time.Second, 4, 8 * time.Second},
		{10 * time.Second, 1, 8 * time.Second},
		{500 * time.Millisecond, 0, 500 * time.Millisecond},
		{0, 1, account.DefaultRetryBase},
		{-time.Second, 1, account.DefaultRetryBase},
	}
	for _, tc := range cases {
		if got := account.RetryDelay(tc.base, tc.index); got != tc.want {
			t.Fatalf("RetryDelay(%v, %d) = %v，期望 %v", tc.base, tc.index, got, tc.want)
		}
	}
}

func TestInAccountCooldown(t *testing.T) {
	now := time.UnixMilli(10_000)
	cases := []struct {
		until int64
		want  bool
	}{
		{0, false},
		{9_999, false},
		{10_000, false},
		{10_001, true},
	}
	for _, tc := range cases {
		a := store.Account{CooldownUntilMS: tc.until}
		if got := account.InAccountCooldown(a, now); got != tc.want {
			t.Fatalf("InAccountCooldown(until=%d) = %v，期望 %v", tc.until, got, tc.want)
		}
	}
}

func TestAvailableForModelChecksAccountCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	a := acc(1, "a", 100, []string{"m1"}, true, 0)

	a.CooldownUntilMS = now.Add(time.Second).UnixMilli()
	if account.AvailableForModel(a, "m1", now) {
		t.Fatal("账号级冷却中时不该可用")
	}
	a.CooldownUntilMS = now.UnixMilli()
	if !account.AvailableForModel(a, "m1", now) {
		t.Fatal("冷却截止等于此刻应算已结束")
	}
	a.CooldownUntilMS = now.UnixMilli() - 1
	if !account.AvailableForModel(a, "m1", now) {
		t.Fatal("冷却已过期时应可用")
	}
}

func TestRankAccountCooldownExclusions(t *testing.T) {
	now := time.Unix(1000, 0)
	disabled := acc(1, "disabled", 10, []string{"m1"}, false, 0)
	disabled.CooldownUntilMS = now.Add(time.Minute).UnixMilli()

	accountCool := acc(2, "account-cool", 20, []string{"m1"}, true, 0)
	accountCool.CooldownUntilMS = now.Add(time.Minute).UnixMilli()

	noModel := acc(3, "no-model", 30, []string{"m2"}, true, 0)
	modelCool := acc(4, "model-cool", 40, []string{"m1"}, true, now.Unix()+60)
	available := acc(5, "available", 50, []string{"m1"}, true, 0)
	expired := acc(6, "expired", 60, []string{"m1"}, true, 0)
	expired.CooldownUntilMS = now.UnixMilli() - 1

	accounts := []store.Account{disabled, accountCool, noModel, modelCool, available, expired}
	decision := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: store.DefaultRoutePolicy(),
	})

	if !equalStrings(names(decision), []string{"available", "expired"}) {
		t.Fatalf("候选 = %v，期望 available、expired", names(decision))
	}
	wantReasons := []string{"账号已停用", "账号冷却中", "没有声明这个模型", "模型冷却中"}
	if len(decision.Excluded) != len(wantReasons) {
		t.Fatalf("排除 = %+v，期望 %d 条", decision.Excluded, len(wantReasons))
	}
	for i, want := range wantReasons {
		if decision.Excluded[i].Reason != want {
			t.Fatalf("第 %d 条排除原因 = %q，期望 %q（顺序按账号列表）", i, decision.Excluded[i].Reason, want)
		}
	}
}

func TestRankStrictStickyMissOnAccountCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := routeAccounts()
	policy := store.DefaultRoutePolicy()
	policy.Sticky = store.RouteStickyStrict

	first := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-x",
	})
	if first.StickyAccountID == 0 {
		t.Fatal("应算出粘性目标")
	}
	for i := range accounts {
		if accounts[i].ID == first.StickyAccountID {
			accounts[i].CooldownUntilMS = now.Add(time.Minute).UnixMilli()
		}
	}

	strict := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-x",
	})
	if !strict.StickyMiss || len(strict.Candidates) != 0 {
		t.Fatalf("strict 目标在账号冷却中时应直接 503，实际 = %+v", strict)
	}

	// prefer 则回落到下一个候选，不返回空集。
	policy.Sticky = store.RouteStickyPrefer
	prefer := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-x",
	})
	if prefer.StickyMiss || len(prefer.Candidates) != 2 {
		t.Fatalf("prefer 应回落到其它候选，实际 = %+v", prefer)
	}
	for _, a := range prefer.Candidates {
		if a.ID == first.StickyAccountID {
			t.Fatalf("prefer 不应把冷却中的账号放回候选: %+v", prefer.Candidates)
		}
	}
}
