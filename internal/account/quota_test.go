package account_test

import (
	"testing"
	"time"

	"lax/internal/account"
	"lax/internal/quota"
	"lax/internal/store"
)

// quotaValue 是测试里造可选数值的简写。
func quotaValue(v float64) *float64 { return &v }

// quotaEntry 造一条只有余额（USD）的快照。
func quotaEntry(usd float64) quota.Entry {
	return quota.Entry{
		Facts:    quota.Facts{Balance: usd, BalanceKnown: true, Currency: "USD", BalanceUSD: quotaValue(usd)},
		Settings: quota.DefaultSettings(),
	}
}

func TestRankLegacyBehaviorWhenQuotasEmpty(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	policy := store.DefaultRoutePolicy()
	got := account.Rank(account.RouteInput{Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy})
	if !equalStrings(names(got), []string{"a", "b", "c"}) {
		t.Fatalf("没有额度数据时的顺序 = %v，应与旧版一致", names(got))
	}
	for _, c := range got.Choices {
		if c.Quota != nil {
			t.Fatalf("没有 Quotas 时不该填额度: %+v", c.Quota)
		}
	}
	if len(got.Excluded) != 0 {
		t.Fatalf("没有额度数据时不该多出排除: %+v", got.Excluded)
	}
}

func TestRankQuotaMinUSDFiltersKnownBalanceOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	accounts := routeAccounts()
	// a 有 10 USD，b 只有 0.069，c 的汇率未知（USD 指针为 nil）：b 被过滤，c 必须保留。
	quotas := map[int64]quota.Entry{
		1: quotaEntry(10),
		2: quotaEntry(0.069),
		3: {Facts: quota.Facts{Balance: 117.84, BalanceKnown: true, Currency: "CNY"},
			Settings: quota.DefaultSettings()},
	}
	policy := store.DefaultRoutePolicy()
	policy.QuotaMinUSD = 1
	got := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy, Quotas: quotas,
	})
	if !equalStrings(names(got), []string{"a", "c"}) {
		t.Fatalf("min_usd 过滤后 = %v，期望 a 与未知的 c", names(got))
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Account.Name != "b" ||
		!account.IsQuotaExclusionReason(got.Excluded[0].Reason) {
		t.Fatalf("排除 = %+v，期望只有 b 且原因是额度", got.Excluded)
	}
	// 排除只影响本次选号，不改账号启用状态。
	if !got.Choices[0].Account.Enabled || !got.Choices[1].Account.Enabled {
		t.Fatal("额度过滤不该改账号的 enabled")
	}
}

// TestRankQuotaOffSourceIsIgnored 钉住 quota set --source off 后旧快照不再参与选号：
// 既不能按旧余额排除，也不能按旧余额参与排序。
func TestRankQuotaOffSourceIsIgnored(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	off := quota.DefaultSettings()
	off.Source = quota.SourceOff
	off.ExpiryMode = quota.ExpiryModeExclude

	// a 的旧快照余额很低且已到期；关闭监控后必须当作没有额度数据。
	low := quotaEntry(0.069)
	low.Settings = off
	low.Facts.PeriodEnd = now.Add(-time.Hour).UnixMilli()
	quotas := map[int64]quota.Entry{1: low, 2: quotaEntry(10)}

	policy := store.DefaultRoutePolicy()
	policy.QuotaMinUSD = 1
	got := account.Rank(account.RouteInput{
		Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Quotas: quotas,
	})
	if !equalStrings(names(got), []string{"a", "b", "c"}) {
		t.Fatalf("source=off 的旧快照不该过滤账号: %v", names(got))
	}
	if len(got.Excluded) != 0 {
		t.Fatalf("source=off 的旧快照不该产生排除: %+v", got.Excluded)
	}

	// 旧快照余额很高时也不能左右 usd_desc 排序。
	high := quotaEntry(100)
	high.Settings = off
	quotas[1] = high
	policy = store.DefaultRoutePolicy()
	policy.QuotaOrder = quota.OrderUSDDesc
	got = account.Rank(account.RouteInput{
		Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Quotas: quotas,
	})
	if !equalStrings(names(got), []string{"b", "a", "c"}) {
		t.Fatalf("source=off 的旧快照不该参与排序: %v，期望 b 按 10 USD 排第一", names(got))
	}
}

func TestRankQuotaExpiryModes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	near := now.Add(48 * time.Hour).UnixMilli()
	far := now.Add(30 * 24 * time.Hour).UnixMilli()
	expired := now.Add(-time.Hour).UnixMilli()

	accounts := []store.Account{
		acc(1, "warn", 10, []string{"m1"}, true, 0),
		acc(2, "exclude", 20, []string{"m1"}, true, 0),
		acc(3, "deprioritize", 30, []string{"m1"}, true, 0),
		acc(4, "far", 40, []string{"m1"}, true, 0),
	}
	warnCfg := quota.DefaultSettings()
	excludeCfg := quota.DefaultSettings()
	excludeCfg.ExpiryMode = quota.ExpiryModeExclude
	deprioritizeCfg := quota.DefaultSettings()
	deprioritizeCfg.ExpiryMode = quota.ExpiryModeDeprioritize

	quotas := map[int64]quota.Entry{
		1: {Facts: quota.Facts{PeriodEnd: near}, Settings: warnCfg},
		// exclude 同时覆盖「临近」与「已到期」两种状态。
		2: {Facts: quota.Facts{PeriodEnd: expired}, Settings: excludeCfg},
		3: {Facts: quota.Facts{PeriodEnd: expired}, Settings: deprioritizeCfg},
		4: {Facts: quota.Facts{PeriodEnd: far}, Settings: quota.DefaultSettings()},
	}
	policy := store.DefaultRoutePolicy()
	policy.QuotaOrder = quota.OrderUSDDesc
	// 给排序一个明确的键：没有余额时按未知处理，这里补上 USD，验证 deprioritize 压过排序。
	quotas[1] = quota.Entry{Facts: quota.Facts{PeriodEnd: near, BalanceUSD: quotaValue(10)}, Settings: warnCfg}
	quotas[3] = quota.Entry{Facts: quota.Facts{PeriodEnd: expired, BalanceUSD: quotaValue(100)}, Settings: deprioritizeCfg}
	quotas[4] = quota.Entry{Facts: quota.Facts{PeriodEnd: far, BalanceUSD: quotaValue(50)}, Settings: quota.DefaultSettings()}

	got := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy, Quotas: quotas,
	})
	// exclude 被排除；deprioritize 即使余额最高也排在最后。
	if !equalStrings(names(got), []string{"far", "warn", "deprioritize"}) {
		t.Fatalf("到期模式后的顺序 = %v，期望 far、warn、deprioritize", names(got))
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Account.Name != "exclude" {
		t.Fatalf("排除 = %+v，期望只有 exclude", got.Excluded)
	}
}

func TestRankQuotaOrderDirectionsAndUnknownLast(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	accounts := routeAccounts()
	endA := now.Add(30 * 24 * time.Hour).UnixMilli()
	endB := now.Add(10 * 24 * time.Hour).UnixMilli()
	ratioA, ratioB := 0.5, 0.8
	quotas := map[int64]quota.Entry{
		1: {Facts: quota.Facts{PeriodEnd: endA, RemainingRatio: &ratioA, BalanceUSD: quotaValue(5)}},
		2: {Facts: quota.Facts{PeriodEnd: endB, RemainingRatio: &ratioB, BalanceUSD: quotaValue(10)}},
		// c 没有任何额度事实：所有排序里都必须排最后。
	}

	cases := []struct {
		order string
		want  []string
	}{
		{quota.OrderOff, []string{"a", "b", "c"}},
		{quota.OrderExpiryAsc, []string{"b", "a", "c"}},
		{quota.OrderExpiryDesc, []string{"a", "b", "c"}},
		{quota.OrderRatioDesc, []string{"b", "a", "c"}},
		{quota.OrderRatioAsc, []string{"a", "b", "c"}},
		{quota.OrderUSDDesc, []string{"b", "a", "c"}},
		{quota.OrderUSDAsc, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		policy := store.DefaultRoutePolicy()
		policy.QuotaOrder = tc.order
		got := account.Rank(account.RouteInput{
			Accounts: accounts, Model: "m1", Now: now, Policy: policy, Quotas: quotas,
		})
		if !equalStrings(names(got), tc.want) {
			t.Fatalf("order=%s 顺序 = %v，期望 %v", tc.order, names(got), tc.want)
		}
	}
}

func TestRankQuotaOrderStableTie(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	same := 0.9
	low := 0.1
	quotas := map[int64]quota.Entry{
		1: {Facts: quota.Facts{RemainingRatio: &same}},
		2: {Facts: quota.Facts{RemainingRatio: &same}},
		3: {Facts: quota.Facts{RemainingRatio: &low}},
	}
	policy := store.DefaultRoutePolicy()
	policy.QuotaOrder = quota.OrderRatioDesc
	got := account.Rank(account.RouteInput{
		Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Quotas: quotas,
	})
	if !equalStrings(names(got), []string{"a", "b", "c"}) {
		t.Fatalf("相同额度键应保持原有顺序: %v", names(got))
	}
}

func TestRankStrictStickyExcludedByQuota(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	accounts := routeAccounts()
	policy := store.DefaultRoutePolicy()
	policy.Sticky = store.RouteStickyStrict

	first := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy, Session: "session-quota",
	})
	if first.StickyAccountID == 0 {
		t.Fatal("没有算出粘性账号")
	}
	var target string
	for _, a := range accounts {
		if a.ID == first.StickyAccountID {
			target = a.Name
		}
	}
	quotas := map[int64]quota.Entry{
		first.StickyAccountID: quotaEntry(0.069),
	}
	policy.QuotaMinUSD = 1
	got := account.Rank(account.RouteInput{
		Accounts: accounts, Model: "m1", Now: now, Policy: policy,
		Quotas: quotas, Session: "session-quota",
	})
	if !got.StickyMiss || len(got.Candidates) != 0 {
		t.Fatalf("strict 粘性被额度排除时应 StickyMiss，实际 sticky=%s 候选=%v", target, names(got))
	}
	if len(got.Excluded) != 1 || !account.IsQuotaExclusionReason(got.Excluded[0].Reason) {
		t.Fatalf("应记录额度排除原因: %+v", got.Excluded)
	}
}

func TestRankSoftmaxThenQuotaOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	quotas := map[int64]quota.Entry{
		1: quotaEntry(1),
		2: quotaEntry(2),
		3: quotaEntry(3),
	}
	policy := store.DefaultRoutePolicy()
	policy.Strategy = store.RouteStrategySoftmax
	policy.Temperature = 1
	policy.Explore = 0
	policy.QuotaOrder = quota.OrderUSDDesc
	got := account.Rank(account.RouteInput{
		Accounts: routeAccounts(), Model: "m1", Now: now, Policy: policy, Quotas: quotas, Seed: 7,
	})
	if !equalStrings(names(got), []string{"c", "b", "a"}) {
		t.Fatalf("softmax 之后应按 usd_desc 重排: %v", names(got))
	}
	sum := 0.0
	for _, c := range got.Choices {
		if c.Probability == nil {
			t.Fatal("softmax 概率丢了")
		}
		sum += *c.Probability
	}
	if sum < 0.999999 || sum > 1.000001 {
		t.Fatalf("排序后概率和 = %v", sum)
	}
}
