package gateway_test

import (
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

func routeUpstream(t *testing.T, status int, body string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setRoutePolicy(t *testing.T, st *store.Store, policy store.RoutePolicy) {
	t.Helper()
	if err := st.SetRoutePolicy(policy); err != nil {
		t.Fatalf("写选号策略: %v", err)
	}
}

func TestGatewayRotateUsesPerModelSequence(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	first := routeUpstream(t, http.StatusOK, chatCompletion, &firstCalls)
	second := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	f.add(t, "first", gateway.ProtocolChat, first.URL, "sk-first", []string{"m1"}, 100)
	f.add(t, "second", gateway.ProtocolChat, second.URL, "sk-second", []string{"m1"}, 200)
	policy := store.DefaultRoutePolicy()
	policy.Model = "m1"
	policy.Strategy = store.RouteStrategyRotate
	policy.Sticky = store.RouteStickyOff
	setRoutePolicy(t, f.st, policy)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		if status, response := postMessages(t, srv, body); status != http.StatusOK {
			t.Fatalf("第 %d 次请求 = %d：%s", i+1, status, response)
		}
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("两个账号的调用数 = %d / %d，期望各一次", firstCalls, secondCalls)
	}
}

func TestGatewayRouteStatsFollowAccountFailures(t *testing.T) {
	f := newFixture(t)
	var limitedCalls, healthyCalls int
	limited := routeUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"limited"}}`, &limitedCalls)
	healthy := routeUpstream(t, http.StatusOK, chatCompletion, &healthyCalls)
	limitedAccount := f.add(t, "limited", gateway.ProtocolChat, limited.URL, "sk-limited", []string{"m1"}, 10)
	healthyAccount := f.add(t, "healthy", gateway.ProtocolChat, healthy.URL, "sk-healthy", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	if status, response := postMessages(t, srv, body); status != http.StatusOK {
		t.Fatalf("429 后应换号成功，实际 %d：%s", status, response)
	}
	if limitedCalls != 1 || healthyCalls != 1 {
		t.Fatalf("调用数 = %d / %d，期望限流一次后换号一次", limitedCalls, healthyCalls)
	}
	stats, err := f.st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("读统计: %v", err)
	}
	if got := stats[limitedAccount.ID]; got.Attempts != 1 || got.Successes != 0 {
		t.Fatalf("429 账号统计 = %+v，期望一次失败样本", got)
	}
	if got := stats[healthyAccount.ID]; got.Attempts != 1 || got.Successes != 1 {
		t.Fatalf("成功账号统计 = %+v，期望一次成功样本", got)
	}

	// 400 是请求本身写错，不算账号表现，也不换号。
	f2 := newFixture(t)
	var badCalls, spareCalls int
	bad := routeUpstream(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`, &badCalls)
	spare := routeUpstream(t, http.StatusOK, chatCompletion, &spareCalls)
	badAccount := f2.add(t, "bad", gateway.ProtocolChat, bad.URL, "sk-bad", []string{"m1"}, 10)
	f2.add(t, "spare", gateway.ProtocolChat, spare.URL, "sk-spare", []string{"m1"}, 100)
	srv2 := f2.server(t)
	if status, _ := postMessages(t, srv2, body); status != http.StatusBadRequest {
		t.Fatalf("400 应原样返回，实际 %d", status)
	}
	if badCalls != 1 || spareCalls != 0 {
		t.Fatalf("400 的调用数 = %d / %d，期望不换号", badCalls, spareCalls)
	}
	stats, err = f2.st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("读 400 统计: %v", err)
	}
	if _, ok := stats[badAccount.ID]; ok {
		t.Fatalf("400 不该进入选号统计: %+v", stats[badAccount.ID])
	}
}

func TestGatewayStrictStickyReturns503(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusOK, chatCompletion, &calls)
	f.add(t, "first", gateway.ProtocolChat, up.URL, "sk-first", []string{"m1"}, 100)
	f.add(t, "second", gateway.ProtocolChat, up.URL, "sk-second", []string{"m1"}, 200)
	accounts, err := f.st.ListAccounts()
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	ids := []int64{accounts[0].ID, accounts[1].ID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	session := ""
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("strict-%d", i)
		h := fnv.New32a()
		_, _ = h.Write([]byte(candidate))
		if h.Sum32()%2 == 1 {
			session = candidate
			break
		}
	}
	if session == "" {
		t.Fatal("没有找到哈希到第二个账号的会话")
	}
	if err := f.st.SetEnabled(ids[1], false, "测试停用"); err != nil {
		t.Fatalf("停用粘性目标: %v", err)
	}
	policy := store.DefaultRoutePolicy()
	policy.Model = "m1"
	policy.Sticky = store.RouteStickyStrict
	setRoutePolicy(t, f.st, policy)
	srv := f.server(t)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-lax-session", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "strict") {
		t.Fatalf("strict 粘性 = %d：%s", resp.StatusCode, raw)
	}
	if calls != 0 {
		t.Fatalf("strict 目标不可用时不该请求上游，实际 %d 次", calls)
	}
}

func TestGatewayDoesNotRetryRateLimitedAccountInSameRequest(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"慢一点"}}`, &calls)
	acc := f.add(t, "only", gateway.ProtocolChat, up.URL, "sk-only", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503（唯一账号 429，候选耗尽），响应 %s", status, body)
	}
	if calls != 1 {
		t.Fatalf("上游调用 = %d 次，期望 1 次（429 不在同一请求内重试同一账号）", calls)
	}
	got, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if failure := got.ModelFailures["m1"]; failure.FailStreak != 1 || failure.CooldownUntilMS == 0 {
		t.Fatalf("429 应落一次冷却与一次失败计数: %+v", failure)
	}
	stats, err := f.st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("读统计: %v", err)
	}
	if stat := stats[acc.ID]; stat.Attempts != 1 || stat.Successes != 0 {
		t.Fatalf("选号统计 = %+v，期望一次失败尝试", stat)
	}
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望只有一次失败", recs)
	}
}

func TestGatewayTriesEveryCandidateOnceOnRateLimit(t *testing.T) {
	f := newFixture(t)
	var limitedCalls, brokenCalls int
	limited := routeUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"limited"}}`, &limitedCalls)
	broken := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"broken"}}`, &brokenCalls)
	f.add(t, "limited", gateway.ProtocolChat, limited.URL, "sk-limited", []string{"m1"}, 100)
	f.add(t, "broken", gateway.ProtocolChat, broken.URL, "sk-broken", []string{"m1"}, 200)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503，响应 %s", status, body)
	}
	if limitedCalls != 1 || brokenCalls != 1 {
		t.Fatalf("上游调用 = %d / %d，期望各一次：429 与 5xx 都只换号，不在同一请求内重试", limitedCalls, brokenCalls)
	}
}

func TestGatewayRateLimitStopsAfterCandidatesExhausted(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusTooManyRequests, `{"error":{"message":"limited"}}`, &calls)
	f.add(t, "limited", gateway.ProtocolChat, up.URL, "sk-limited", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503，响应 %s", status, body)
	}
	if calls != 1 {
		t.Fatalf("上游调用 = %d 次，期望 1 次：唯一候选 429 后直接返回 503", calls)
	}
}
