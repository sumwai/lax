package account_test

import (
	"math"
	"testing"
	"time"

	"lax/internal/account"
	"lax/internal/store"
)

func routeAccounts() []store.Account {
	return []store.Account{
		acc(1, "a", 10, []string{"m1"}, true, 0),
		acc(2, "b", 20, []string{"m1"}, true, 0),
		acc(3, "c", 30, []string{"m1"}, true, 0),
	}
}

func names(decision account.RouteDecision) []string {
	out := make([]string, 0, len(decision.Candidates))
	for _, a := range decision.Candidates {
		out = append(out, a.Name)
	}
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestRankFiltersAndPriority(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := append(routeAccounts(),
		acc(4, "disabled", 1, []string{"m1"}, false, 0),
		acc(5, "cooling", 2, []string{"m1"}, true, now.Unix()+60),
		acc(6, "other", 3, []string{"m2"}, true, 0),
	)
	policy := store.DefaultRoutePolicy()
	got := account.Rank(account.RouteInput{Accounts: accounts, Model: "m1", Now: now, Policy: policy})
	if !equalStrings(names(got), []string{"a", "b", "c"}) {
		t.Fatalf("priority 顺序 = %v", names(got))
	}
	if len(got.Excluded) != 3 {
		t.Fatalf("排除数 = %d，期望 3：%+v", len(got.Excluded), got.Excluded)
	}
}

func TestRankRotateAndWeighted(t *testing.T) {
	now := time.Unix(1000, 0)
	policy := store.DefaultRoutePolicy()
	policy.Strategy = store.RouteStrategyRotate
	got := account.Rank(account.RouteInput{
		Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Sequence: 1,
	})
	if !equalStrings(names(got), []string{"b", "c", "a"}) {
		t.Fatalf("rotate 顺序 = %v", names(got))
	}

	policy.Strategy = store.RouteStrategyWeighted
	weights := map[int64]int{1: 2, 2: 1, 3: 1}
	cases := map[uint64]string{0: "a", 1: "a", 2: "b", 3: "c"}
	for sequence, first := range cases {
		got := account.Rank(account.RouteInput{
			Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy,
			Weights: weights, Sequence: sequence,
		})
		if len(got.Candidates) != 3 || got.Candidates[0].Name != first {
			t.Fatalf("weighted 第 %d 次第一名 = %v，期望 %s", sequence, names(got), first)
		}
	}
}

func TestRankSuccessUsesSmoothedRate(t *testing.T) {
	now := time.Unix(1000, 0)
	policy := store.DefaultRoutePolicy()
	policy.Strategy = store.RouteStrategySuccess
	policy.MinSamples = 1
	stats := map[int64]store.RouteStat{
		1: {Attempts: 10, Successes: 2},
		2: {Attempts: 10, Successes: 9},
		3: {Attempts: 10, Successes: 5},
	}
	got := account.Rank(account.RouteInput{
		Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Stats: stats,
	})
	if !equalStrings(names(got), []string{"b", "c", "a"}) {
		t.Fatalf("success 顺序 = %v", names(got))
	}
	if got.Choices[0].SuccessRate <= got.Choices[1].SuccessRate {
		t.Fatalf("成功率没有按降序排: %+v", got.Choices)
	}
}

func TestRankSoftmaxSeedAndExplore(t *testing.T) {
	now := time.Unix(1000, 0)
	policy := store.DefaultRoutePolicy()
	policy.Strategy = store.RouteStrategySoftmax
	policy.Explore = 0.2
	input := account.RouteInput{Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Seed: 42}
	first := account.Rank(input)
	again := account.Rank(input)
	if !equalStrings(names(first), names(again)) {
		t.Fatalf("同一个 seed 得到不同顺序: %v vs %v", names(first), names(again))
	}
	sum := 0.0
	for _, c := range first.Choices {
		if c.Probability == nil {
			t.Fatal("softmax 没有给概率")
		}
		if *c.Probability < 0.2/3-1e-9 {
			t.Fatalf("探索保底没有生效：%+v", *c.Probability)
		}
		sum += *c.Probability
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("概率和 = %v，期望 1", sum)
	}

	policy.Temperature = 1
	policy.Explore = 0
	equal := routeAccounts()
	for i := range equal {
		equal[i].Priority = 10
	}
	uniform := account.Rank(account.RouteInput{Accounts: equal, Model: "m1", Now: now, Policy: policy, Seed: 1})
	for _, c := range uniform.Choices {
		*c.Probability -= 1.0 / 3
		if math.Abs(*c.Probability) > 1e-9 {
			t.Fatalf("等分候选的概率不相等: %+v", uniform.Choices)
		}
	}
}

func TestRankStickyPreferAndStrict(t *testing.T) {
	now := time.Unix(1000, 0)
	accounts := routeAccounts()
	policy := store.DefaultRoutePolicy()
	first := account.Rank(account.RouteInput{Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-x"})
	again := account.Rank(account.RouteInput{Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-x"})
	if first.StickyAccountID == 0 || first.StickyAccountID != again.StickyAccountID {
		t.Fatalf("同一会话的粘性账号不稳定: %d vs %d", first.StickyAccountID, again.StickyAccountID)
	}
	if first.Candidates[0].ID != first.StickyAccountID {
		t.Fatalf("prefer 没把粘性账号提到第一位: %v", names(first))
	}

	// 让哈希到的账号停用：prefer 换号，strict 直接返回不可用。
	for i := range accounts {
		if accounts[i].ID == first.StickyAccountID {
			accounts[i].Enabled = false
		}
	}
	policy.Sticky = store.RouteStickyStrict
	strict := account.Rank(account.RouteInput{Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-x"})
	if !strict.StickyMiss || len(strict.Candidates) != 0 {
		t.Fatalf("strict 在目标不可用时仍换了号: %+v", strict)
	}
}
