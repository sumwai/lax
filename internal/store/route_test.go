package store_test

import (
	"testing"
	"time"

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
