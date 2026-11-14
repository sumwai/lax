package store_test

import (
	"testing"
	"time"

	"lax/internal/quota"
	"lax/internal/store"
)

func TestRoutePolicyResolveAndFallback(t *testing.T) {
	st := openTemp(t)

	got, source, err := st.ResolveRoutePolicy("m1")
	if err != nil {
		t.Fatalf("读内置默认: %v", err)
	}
	if source != "builtin" || got.Strategy != store.RouteStrategyPriority || got.Sticky != store.RouteStickyPrefer {
		t.Fatalf("内置默认 = %+v / %s", got, source)
	}

	def := store.DefaultRoutePolicy()
	def.Strategy = store.RouteStrategyRotate
	if err := st.SetRoutePolicy(def); err != nil {
		t.Fatalf("写默认策略: %v", err)
	}
	got, source, err = st.ResolveRoutePolicy("m1")
	if err != nil {
		t.Fatalf("读默认策略: %v", err)
	}
	if source != "default" || got.Strategy != store.RouteStrategyRotate {
		t.Fatalf("默认策略 = %+v / %s", got, source)
	}

	model := store.DefaultRoutePolicy()
	model.Model = "m1"
	model.Strategy = store.RouteStrategySoftmax
	model.Temperature = 0.3
	model.Explore = 0.1
	if err := st.SetRoutePolicy(model); err != nil {
		t.Fatalf("写模型策略: %v", err)
	}
	got, source, err = st.ResolveRoutePolicy("m1")
	if err != nil {
		t.Fatalf("读模型策略: %v", err)
	}
	if source != "model" || got.Strategy != store.RouteStrategySoftmax || got.Temperature != 0.3 {
		t.Fatalf("模型策略 = %+v / %s", got, source)
	}

	if removed, err := st.DeleteRoutePolicy("m1"); err != nil || !removed {
		t.Fatalf("删除模型策略 = %v, %v", removed, err)
	}
	got, source, err = st.ResolveRoutePolicy("m1")
	if err != nil || source != "default" || got.Strategy != store.RouteStrategyRotate {
		t.Fatalf("删模型策略后 = %+v / %s, %v", got, source, err)
	}
	if removed, err := st.DeleteRoutePolicy("m1"); err != nil || removed {
		t.Fatalf("重复删除应是幂等的 = %v, %v", removed, err)
	}
}

func TestRouteWeightsPrecedenceAndDelete(t *testing.T) {
	st := openTemp(t)
	a := newAccount(t, st, "a", []string{"m1"})
	b := newAccount(t, st, "b", []string{"m1"})

	if err := st.SetRouteAccountWeight(a.ID, "", 2); err != nil {
		t.Fatalf("写账号默认权重: %v", err)
	}
	if err := st.SetRouteAccountWeight(a.ID, "m1", 5); err != nil {
		t.Fatalf("写模型权重: %v", err)
	}
	weights, err := st.RouteWeights("m1")
	if err != nil {
		t.Fatalf("读 m1 权重: %v", err)
	}
	if weights[a.ID] != 5 || weights[b.ID] != 0 {
		t.Fatalf("m1 权重 = %+v，期望 a=5、b 未设置", weights)
	}
	weights, err = st.RouteWeights("m2")
	if err != nil {
		t.Fatalf("读 m2 权重: %v", err)
	}
	if weights[a.ID] != 2 {
		t.Fatalf("m2 应回落到账号默认 2，实际 %+v", weights)
	}

	if err := st.DeleteAccount("a"); err != nil {
		t.Fatalf("删账号: %v", err)
	}
	rows, err := st.ListRouteAccountWeights()
	if err != nil {
		t.Fatalf("读权重行: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("删账号后还有权重: %+v", rows)
	}
}

func TestRecordAttemptWritesRouteStatsAndPrunes(t *testing.T) {
	st := openTemp(t)
	a := newAccount(t, st, "a", []string{"m1"})
	rec := func(ts int64, status int, ok bool, latency int64, sample, success bool) *store.RequestRecord {
		r := &store.RequestRecord{
			TS: ts, Inbound: "openai-chat", Outbound: "openai-chat",
			ModelRequested: "m1", ModelUsed: "m1", AccountID: a.ID,
			HTTPStatus: status, OK: ok, LatencyMS: latency,
		}
		if err := st.RecordAttempt(r, store.AttemptState{RouteSample: sample, RouteSuccess: success}); err != nil {
			t.Fatalf("写尝试: %v", err)
		}
		return r
	}

	rec(100, 200, true, 120, true, true)
	rec(200, 429, false, 0, true, false)
	rec(300, 400, false, 80, false, false)

	stats, err := st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("读统计: %v", err)
	}
	got := stats[a.ID]
	if got.Attempts != 2 || got.Successes != 1 || got.LatencySumMS != 120 || got.LatencyCount != 1 {
		t.Fatalf("统计 = %+v，期望两次样本、一次成功、延迟 120ms", got)
	}

	recent, err := st.RouteStats("m1", time.Now().Unix()-3600)
	if err != nil {
		t.Fatalf("读窗口统计: %v", err)
	}
	if recent[a.ID].Attempts != 0 {
		t.Fatalf("窗口外的旧统计不该进来: %+v", recent)
	}

	if _, err := st.PruneRouteStats(150); err != nil {
		t.Fatalf("清理统计: %v", err)
	}
	stats, err = st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("清理后读统计: %v", err)
	}
	if stats[a.ID].Attempts != 1 {
		t.Fatalf("清理后应只剩 1 次样本: %+v", stats[a.ID])
	}

	if err := st.DeleteAccount("a"); err != nil {
		t.Fatalf("删账号: %v", err)
	}
	stats, err = st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("删账号后读统计: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("删账号后统计没有清掉: %+v", stats)
	}
}

func TestRouteQuotaPolicyAndSnapshot(t *testing.T) {
	st := openTemp(t)

	def := store.DefaultRoutePolicy()
	if def.QuotaOrder != quota.OrderOff || def.QuotaMinUSD != 0 {
		t.Fatalf("默认额度设置 = %q / %v，期望 off / 0", def.QuotaOrder, def.QuotaMinUSD)
	}
	// 空串按 off 处理：旧行或手工构造的策略没写这个字段时不该被拒绝，也不能变成非法值。
	legacy := store.DefaultRoutePolicy()
	legacy.QuotaOrder = ""
	if err := store.ValidateRoutePolicy(legacy); err != nil {
		t.Fatalf("空额度排序应按 off 通过校验: %v", err)
	}

	policy := store.DefaultRoutePolicy()
	policy.Model = "m1"
	policy.QuotaOrder = quota.OrderUSDDesc
	policy.QuotaMinUSD = 1.5
	if err := st.SetRoutePolicy(policy); err != nil {
		t.Fatalf("写含额度设置的政策: %v", err)
	}
	got, source, err := st.ResolveRoutePolicy("m1")
	if err != nil {
		t.Fatalf("读政策: %v", err)
	}
	if source != "model" || got.QuotaOrder != quota.OrderUSDDesc || got.QuotaMinUSD != 1.5 {
		t.Fatalf("额度设置没有持久化: %+v / %s", got, source)
	}

	invalid := policy
	invalid.QuotaOrder = "bogus"
	if err := st.SetRoutePolicy(invalid); err == nil {
		t.Fatal("非法额度排序应被拒绝")
	}
	negative := policy
	negative.QuotaMinUSD = -1
	if err := st.SetRoutePolicy(negative); err == nil {
		t.Fatal("负数 min_usd 应被拒绝")
	}

	// 没有任何额度行时，快照给空 map（非 nil），选号侧因此跳过额度过滤。
	snapshot, err := st.RouteSnapshot("m1", time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("空快照: %v", err)
	}
	if snapshot.Quotas == nil || len(snapshot.Quotas) != 0 {
		t.Fatalf("没有额度行时 Quotas = %#v，期望非 nil 空 map", snapshot.Quotas)
	}

	acc := newAccount(t, st, "qa", []string{"m1"})
	if err := st.SaveQuotaSnapshot(acc.ID, quota.Facts{
		Status: quota.StatusOK, Balance: 10, BalanceKnown: true, Currency: "USD", CheckedAt: 1,
	}); err != nil {
		t.Fatalf("存快照: %v", err)
	}
	snapshot, err = st.RouteSnapshot("m1", time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("带额度快照: %v", err)
	}
	entry, ok := snapshot.Quotas[acc.ID]
	if !ok {
		t.Fatalf("快照缺少账号 %d: %+v", acc.ID, snapshot.Quotas)
	}
	if entry.Facts.BalanceUSD == nil || *entry.Facts.BalanceUSD != 10 {
		t.Fatalf("快照的折合余额 = %+v，期望 10", entry.Facts.BalanceUSD)
	}
	if entry.Settings != quota.DefaultSettings() {
		t.Fatalf("没有设置行时应使用默认设置: %+v", entry.Settings)
	}
}
