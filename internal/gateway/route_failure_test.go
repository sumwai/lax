package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lax/internal/account"
	"lax/internal/gateway"
	"lax/internal/store"
)

// 这个文件钉住 5.20 的网关与探活行为：策略命中时的重试、冷却、停用与换号，
// 以及没有规则命中时旧行为一个字不变。所有等待都走注入的 sleep，不真实睡眠。

const billing400Body = `{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits."}}`

const failureTestChatBody = `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// createFailurePolicy 建一条启用的用户规则并返回落库后的行（带 id）。
func createFailurePolicy(t *testing.T, f *fixture, p store.RouteFailurePolicy) store.RouteFailurePolicy {
	t.Helper()
	p.Enabled = true
	if p.Priority == 0 {
		p.Priority = 100
	}
	if err := f.st.CreateRouteFailurePolicy(&p); err != nil {
		t.Fatalf("建失败处理策略: %v", err)
	}
	return p
}

// recordingSleep 返回一个记录等待、但不真的睡的实现。ctx.Err() 正常时为 nil。
func recordingSleep(sleeps *[]time.Duration) func(context.Context, time.Duration) error {
	var mu sync.Mutex
	return func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*sleeps = append(*sleeps, d)
		mu.Unlock()
		return ctx.Err()
	}
}

// sleepingGateway 构造一个带等待记录器的网关。
func sleepingGateway(t *testing.T, f *fixture, sleeps *[]time.Duration) (*gateway.Server, func() string) {
	t.Helper()
	return newGatewayWith(t, f, gateway.Options{Sleep: recordingSleep(sleeps)})
}

// failureRows 读最近的请求记录，按 http_status 数行数。
func failureRows(t *testing.T, f *fixture, limit int) (int, map[int]int) {
	t.Helper()
	recs, err := f.st.RecentRequests(limit)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	byStatus := make(map[int]int)
	for _, rec := range recs {
		byStatus[rec.HTTPStatus]++
	}
	return len(recs), byStatus
}

func TestGatewayMatchedServerErrorStillSamplesRouteStats(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &calls)
	acc := f.add(t, "a1", gateway.ProtocolChat, up.URL, "sk-a1", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:        []string{"5xx"},
		CooldownSeconds: 30,
		Note:            "5xx 冷却账号并返回",
	})
	srv := f.server(t)

	if status, body := postMessages(t, srv, failureTestChatBody); status == http.StatusOK {
		t.Fatalf("5xx 不应返回成功: %s", body)
	}
	if calls != 1 {
		t.Fatalf("上游调用 = %d，期望 1 次", calls)
	}
	stats, err := f.st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("读统计: %v", err)
	}
	if got := stats[acc.ID]; got.Attempts != 1 || got.Successes != 0 {
		t.Fatalf("命中 5xx 规则时选号统计 = %+v，期望仍记一次失败样本", got)
	}
}

func TestGatewayMatched400DoesNotSampleRouteStats(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`, &calls)
	acc := f.add(t, "a1", gateway.ProtocolChat, up.URL, "sk-a1", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:      []string{"400"},
		ErrorContains: []string{"bad request"},
		ReturnError:   true,
		Note:          "普通 400 返回",
	})
	srv := f.server(t)

	if status, _ := postMessages(t, srv, failureTestChatBody); status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400", status)
	}
	stats, err := f.st.RouteStats("m1", 0)
	if err != nil {
		t.Fatalf("读统计: %v", err)
	}
	if got, ok := stats[acc.ID]; ok {
		t.Fatalf("命中 400 规则时不该进入选号统计: %+v", got)
	}
}

func TestGatewayUserBilling400DisablesAndSwitches(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusBadRequest, billing400Body, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "elsa", gateway.ProtocolChat, firstUp.URL, "sk-elsa", []string{"m1"}, 10)
	f.add(t, "tokenmp", gateway.ProtocolChat, secondUp.URL, "sk-tokenmp", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:      []string{"400"},
		ErrorContains: []string{"insufficient credits"},
		Disable:       true,
		NextRoute:     true,
		Note:          "计费类 400 停用并换号",
	})
	srv := f.server(t)

	status, body := postMessages(t, srv, failureTestChatBody)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望换号成功 200，响应 %s", status, body)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("调用数 = %d / %d，期望第一个账号一次、第二个账号一次", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("命中停用规则后第一个账号应被停用")
	}
	if !strings.Contains(stored.LastError, "insufficient credits") {
		t.Fatalf("last_error = %q，期望带上游原话", stored.LastError)
	}
	rows, byStatus := failureRows(t, f, 5)
	if rows != 2 || byStatus[400] != 1 || byStatus[200] != 1 {
		t.Fatalf("请求记录 = %d 行 %v，期望 400、200 各一行", rows, byStatus)
	}
}

func TestGatewayAccountCooldownExcludesNextRequest(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusBadRequest, billing400Body, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "elsa", gateway.ProtocolChat, firstUp.URL, "sk-elsa", []string{"m1"}, 10)
	f.add(t, "tokenmp", gateway.ProtocolChat, secondUp.URL, "sk-tokenmp", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		AccountID:       first.ID,
		Statuses:        []string{"400"},
		ErrorContains:   []string{"insufficient credits"},
		CooldownSeconds: 30 * 60,
		NextRoute:       true,
		Note:            "elsa 计费冷却 30m",
	})
	srv := f.server(t)
	before := time.Now()

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusOK {
		t.Fatalf("第一次请求 = %d，期望换号成功 200：%s", status, body)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("冷却规则不该停用账号")
	}
	if stored.CooldownUntilMS == 0 {
		t.Fatal("命中账号冷却规则后 accounts.cooldown_until_ms 应写入")
	}
	got := time.UnixMilli(stored.CooldownUntilMS)
	if got.Before(before.Add(29*time.Minute)) || got.After(before.Add(31*time.Minute)) {
		t.Fatalf("账号冷却截止 = %v，期望约 30 分钟后", got)
	}
	if mf := stored.ModelFailures["m1"]; mf.CooldownUntilMS != 0 {
		t.Fatalf("账号级冷却不该写模型行: %+v", mf)
	}

	// 第二次请求应当在选号阶段就跳过冷却中的第一个账号。
	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusOK {
		t.Fatalf("第二次请求 = %d，期望 200：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 2 {
		t.Fatalf("调用数 = %d / %d，期望冷却账号不再被调用（1 / 2）", firstCalls, secondCalls)
	}
}

// modelStatusUpstream 按请求体里的模型回不同状态：m1 是 500，其余是 200，并分别计数。
func modelStatusUpstream(t *testing.T) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		counts[body.Model]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "m1" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		_, _ = io.WriteString(w, chatCompletion)
	}))
	t.Cleanup(srv.Close)
	return srv, func(model string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[model]
	}
}

func TestGatewayModelScopedCooldownOnlyCoolsThatModel(t *testing.T) {
	f := newFixture(t)
	firstUp, firstCalls := modelStatusUpstream(t)
	var secondCalls int
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1", "m2"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1", "m2"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Model:           "m1",
		Statuses:        []string{"500"},
		CooldownSeconds: 60,
		NextRoute:       true,
		Note:            "m1 冷却 1m",
	})
	srv := f.server(t)
	before := time.Now()

	if status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("m1 请求 = %d，期望换号成功 200：%s", status, body)
	}
	if firstCalls("m1") != 1 || secondCalls != 1 {
		t.Fatalf("m1 调用数 = %d / %d，期望各一次", firstCalls("m1"), secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("模型级冷却不该停用账号")
	}
	mf := stored.ModelFailures["m1"]
	if mf.CooldownUntilMS == 0 {
		t.Fatal("m1 应进入模型级冷却")
	}
	got := time.UnixMilli(mf.CooldownUntilMS)
	if got.Before(before.Add(50*time.Second)) || got.After(before.Add(70*time.Second)) {
		t.Fatalf("m1 冷却截止 = %v，期望约 1 分钟后", got)
	}
	if other := stored.ModelFailures["m2"]; other.CooldownUntilMS != 0 {
		t.Fatalf("同账号的 m2 不该被冷却: %+v", other)
	}

	// 另一个模型不受影响：仍然先打第一个账号。
	if status, body := postMessages(t, srv, `{"model":"m2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("m2 请求 = %d，期望 200：%s", status, body)
	}
	if firstCalls("m2") != 1 || secondCalls != 1 {
		t.Fatalf("m2 调用数 = %d / %d，期望第一个账号直接成功、第二个账号一次都没打", firstCalls("m2"), secondCalls)
	}
}

func TestGatewayRetryOneThenNextRoute(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:    []string{"500"},
		RetryCount:  1,
		RetryBaseMS: 500,
		NextRoute:   true,
		Note:        "500 重试一次再换号",
	})
	var sleeps []time.Duration
	gs, _ := sleepingGateway(t, f, &sleeps)
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postMessages(t, srv, failureTestChatBody)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望重试后换号成功 200：%s", status, body)
	}
	if firstCalls != 2 || secondCalls != 1 {
		t.Fatalf("调用数 = %d / %d，期望第一个账号重试一次、第二个账号一次", firstCalls, secondCalls)
	}
	if len(sleeps) != 1 || sleeps[0] != 500*time.Millisecond {
		t.Fatalf("等待序列 = %v，期望 [500ms]", sleeps)
	}
	rows, byStatus := failureRows(t, f, 10)
	if rows != 3 || byStatus[500] != 2 || byStatus[200] != 1 {
		t.Fatalf("请求记录 = %d 行 %v，期望每次重试各一行（500 两行、200 一行）", rows, byStatus)
	}
}

func TestGatewayRetryOneAloneReturnsUpstreamError(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &calls)
	f.add(t, "only", gateway.ProtocolChat, up.URL, "sk-only", []string{"m1"}, 10)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:    []string{"500"},
		RetryCount:  1,
		RetryBaseMS: 500,
		ReturnError: true,
		Note:        "只重试一次",
	})
	var sleeps []time.Duration
	gs, _ := sleepingGateway(t, f, &sleeps)
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postMessages(t, srv, failureTestChatBody)
	if status != http.StatusInternalServerError {
		t.Fatalf("状态 = %d，期望返回上游 500：%s", status, body)
	}
	if !strings.Contains(body, "账号 only") || !strings.Contains(body, "用户规则") || !strings.Contains(body, "boom") {
		t.Fatalf("错误文案 = %s，期望带账号、命中规则与上游原话", body)
	}
	if calls != 2 {
		t.Fatalf("调用数 = %d，期望同账号调用两次", calls)
	}
	if len(sleeps) != 1 || sleeps[0] != 500*time.Millisecond {
		t.Fatalf("等待序列 = %v，期望 [500ms]", sleeps)
	}
	rows, _ := failureRows(t, f, 10)
	if rows != 2 {
		t.Fatalf("请求记录 = %d 行，期望每次尝试各一行", rows)
	}
}

func TestGatewayRetryThreeDelaySequence(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:    []string{"500"},
		RetryCount:  3,
		RetryBaseMS: 500,
		NextRoute:   true,
		Note:        "500 重试三次再换号",
	})
	var sleeps []time.Duration
	gs, _ := sleepingGateway(t, f, &sleeps)
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusOK {
		t.Fatalf("状态 = %d，期望换号成功 200：%s", status, body)
	}
	if firstCalls != 4 || secondCalls != 1 {
		t.Fatalf("调用数 = %d / %d，期望第一个账号共四次、第二个一次", firstCalls, secondCalls)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("等待序列 = %v，期望 %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("第 %d 次等待 = %v，期望 %v", i+1, sleeps[i], want[i])
		}
	}
}

func TestGatewayRetryTotalWaitCap(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &calls)
	f.add(t, "only", gateway.ProtocolChat, up.URL, "sk-only", []string{"m1"}, 10)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:    []string{"500"},
		RetryCount:  10,
		RetryBaseMS: 8000,
		NextRoute:   true,
		Note:        "退避上限",
	})
	var sleeps []time.Duration
	gs, _ := sleepingGateway(t, f, &sleeps)
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	if status, _ := postMessages(t, srv, failureTestChatBody); status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望唯一候选耗尽后 503", status)
	}
	// 每次等待 8 秒；第 8 次重试前累计 56+8=64 秒，超过 60 秒上限，因此只等 7 次、打 8 次。
	if len(sleeps) != 7 {
		t.Fatalf("等待次数 = %d，期望 7（累计 56 秒后停下）", len(sleeps))
	}
	for i, d := range sleeps {
		if d != 8*time.Second {
			t.Fatalf("第 %d 次等待 = %v，期望 8s", i+1, d)
		}
	}
	if calls != 8 {
		t.Fatalf("上游调用 = %d，期望 8 次", calls)
	}
}

func TestGatewayRetryStopsOnContextCancellation(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		Statuses:   []string{"500"},
		RetryCount: 3,
		NextRoute:  true,
		Note:       "取消就别再试",
	})
	var mu sync.Mutex
	var sleeps []time.Duration
	gs, _ := newGatewayWith(t, f, gateway.Options{Sleep: func(context.Context, time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, time.Second)
		mu.Unlock()
		return context.Canceled
	}})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	_, _ = postMessages(t, srv, failureTestChatBody)
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("调用数 = %d / %d，期望取消后不再发上游、也不换号", firstCalls, secondCalls)
	}
	if len(sleeps) != 1 {
		t.Fatalf("等待次数 = %d，期望只进入一次等待", len(sleeps))
	}
}

func TestGatewayGeneric400StillReturnsWithoutState(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	srv := f.server(t)

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望普通 400 原样返回：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("调用数 = %d / %d，期望普通 400 不换号", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled || stored.CooldownUntilMS != 0 {
		t.Fatalf("普通 400 不该改账号状态: enabled=%v cooldown=%d", stored.Enabled, stored.CooldownUntilMS)
	}
	rows, _ := failureRows(t, f, 5)
	if rows != 1 {
		t.Fatalf("请求记录 = %d 行，期望 1", rows)
	}
}

func TestGateway401BuiltinDisablesAndReturns(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "bad", gateway.ProtocolChat, firstUp.URL, "sk-bad", []string{"m1"}, 10)
	f.add(t, "good", gateway.ProtocolChat, secondUp.URL, "sk-good", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, failureTestChatBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("状态 = %d，期望 401：%s", status, body)
	}
	if !strings.Contains(body, "已停用") || !strings.Contains(body, "invalid key") {
		t.Fatalf("错误文案 = %s，期望带「已停用」与上游原话", body)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("调用数 = %d / %d，期望 401 不换号", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("内置 401 规则应停用账号")
	}
	rows, byStatus := failureRows(t, f, 5)
	if rows != 1 || byStatus[401] != 1 {
		t.Fatalf("请求记录 = %d 行 %v，期望一条 401", rows, byStatus)
	}
}

func TestGatewayUser401CooldownOverridesBuiltin(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "bad", gateway.ProtocolChat, firstUp.URL, "sk-bad", []string{"m1"}, 10)
	f.add(t, "good", gateway.ProtocolChat, secondUp.URL, "sk-good", []string{"m1"}, 100)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		AccountID:       first.ID,
		Statuses:        []string{"401"},
		CooldownSeconds: 10 * 60,
		ReturnError:     true,
		Note:            "401 只冷却",
	})
	srv := f.server(t)
	before := time.Now()

	status, body := postMessages(t, srv, failureTestChatBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("状态 = %d，期望 401：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("调用数 = %d / %d，用户规则默认返回、不换号", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("用户 401 冷却规则不该停用账号")
	}
	got := time.UnixMilli(stored.CooldownUntilMS)
	if got.Before(before.Add(9*time.Minute)) || got.After(before.Add(11*time.Minute)) {
		t.Fatalf("账号冷却截止 = %v，期望约 10 分钟后", got)
	}
	if mf := stored.ModelFailures["m1"]; mf.CooldownUntilMS != 0 {
		t.Fatalf("规则没带 --model，不该写模型级冷却: %+v", mf)
	}
}

func TestGateway402BuiltinDisablesAndSwitches(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusPaymentRequired, `{"error":{"message":"payment required"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	srv := f.server(t)

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 402 后换号成功：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("调用数 = %d / %d，期望各一次", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("内置 402 规则应停用账号")
	}
}

func TestGateway403GenericStillReturnsWithoutState(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusForbidden, `{"error":{"message":"no permission"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	srv := f.server(t)

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusForbidden {
		t.Fatalf("状态 = %d，期望普通 403 原样返回：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("调用数 = %d / %d，期望普通 403 不换号", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled || stored.CooldownUntilMS != 0 {
		t.Fatalf("普通 403 不该改账号状态: enabled=%v cooldown=%d", stored.Enabled, stored.CooldownUntilMS)
	}
}

func TestGateway403BillingBuiltinDisablesAndSwitches(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusForbidden, billing400Body, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "first", gateway.ProtocolChat, firstUp.URL, "sk-first", []string{"m1"}, 10)
	f.add(t, "second", gateway.ProtocolChat, secondUp.URL, "sk-second", []string{"m1"}, 100)
	srv := f.server(t)

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusOK {
		t.Fatalf("状态 = %d，期望计费 403 后换号成功：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("调用数 = %d / %d，期望各一次", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("计费类 403 应停用第一个账号")
	}
}

func TestGatewayResWroteNeverRetriesButStateApplies(t *testing.T) {
	f := newFixture(t)
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// 发一帧内容后直接结束，不发收尾帧：网关已经把这一帧写给客户端，wrote 为真。
		_, _ = fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up.Close)
	acc := f.add(t, "stream", gateway.ProtocolChat, up.URL, "sk-stream", []string{"m1"}, 10)
	createFailurePolicy(t, f, store.RouteFailurePolicy{
		ErrorContains: []string{"收尾帧"},
		RetryCount:    5,
		Disable:       true,
		ReturnError:   true,
		Note:          "断流也要停用",
	})
	var sleeps []time.Duration
	gs, _ := sleepingGateway(t, f, &sleeps)
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, _ := postMessages(t, srv,
		`{"model":"m1","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，已写出的响应应保持 200", status)
	}
	if calls != 1 {
		t.Fatalf("上游调用 = %d，期望 wrote 之后不重试", calls)
	}
	if len(sleeps) != 0 {
		t.Fatalf("等待次数 = %d，期望 wrote 之后不等待", len(sleeps))
	}
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("wrote 之后状态动作仍要应用：账号应被停用")
	}
	rows, _ := failureRows(t, f, 5)
	if rows != 1 {
		t.Fatalf("请求记录 = %d 行，期望 1（不重试）", rows)
	}
}

func TestGatewayPolicyReadFailureUsesBuiltins(t *testing.T) {
	f := newFixture(t)
	var firstCalls, secondCalls int
	firstUp := routeUpstream(t, http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`, &firstCalls)
	secondUp := routeUpstream(t, http.StatusOK, chatCompletion, &secondCalls)
	first := f.add(t, "bad", gateway.ProtocolChat, firstUp.URL, "sk-bad", []string{"m1"}, 10)
	f.add(t, "good", gateway.ProtocolChat, secondUp.URL, "sk-good", []string{"m1"}, 100)
	gs, logs := newGatewayWith(t, f, gateway.Options{
		FailurePolicies: func() ([]store.RouteFailurePolicy, error) {
			return nil, errors.New("数据库打不开")
		},
	})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	if status, body := postMessages(t, srv, failureTestChatBody); status != http.StatusUnauthorized {
		t.Fatalf("状态 = %d，期望策略读失败仍按内置 401 返回：%s", status, body)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("调用数 = %d / %d，期望内置 401 不换号", firstCalls, secondCalls)
	}
	stored, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("策略读失败时内置 401 仍应停用账号")
	}
	if !strings.Contains(logs(), "读取上游失败处理策略失败") || !strings.Contains(logs(), "数据库打不开") {
		t.Fatalf("日志 = %s，期望记录策略读取失败并带原因", logs())
	}
}

// ---- Probe ----

func TestProbeChatBilling400DisablesAndReportsPolicy(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusBadRequest, body: billing400Body},
	})
	acc := f.addAs(t, "billing", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})
	gs := newGateway(t, f)

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	up.mustCalls(t, 2)
	if res.OK || res.Status != http.StatusBadRequest {
		t.Fatalf("探活结果 = %+v，期望 400 且不 OK", res)
	}
	if res.Policy == nil {
		t.Fatal("命中内置计费规则时应在结果里带 policy")
	}
	if res.Policy.Source != account.FailureSourceBuiltin || !strings.Contains(res.Policy.Name, "计费") {
		t.Fatalf("policy = %+v，期望内置计费规则", res.Policy)
	}
	if !strings.Contains(res.PolicyAction, "停用") {
		t.Fatalf("policy_action = %q，期望说明停用", res.PolicyAction)
	}
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("计费 400 应停用账号")
	}
	if !strings.Contains(stored.LastError, "insufficient credits") {
		t.Fatalf("last_error = %q，期望带上游原话", stored.LastError)
	}
}

func TestProbeSpeechBilling400NotMarkedUsable(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, billing400Body)
	}))
	t.Cleanup(up.Close)
	acc := voiceProvider(t, f, up.URL, []string{"tts-1"})
	gs := newGateway(t, f)

	res, err := gs.ProbeModelProtocol(context.Background(), acc, "tts-1", gateway.ProtocolSpeech)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Access == store.AccessAllowed {
		t.Fatalf("计费 400 不能标成 allowed（%+v）", res)
	}
	if !res.Undecided {
		t.Fatalf("探活结论 = %+v，期望未判定", res)
	}
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("语音应错探活命中计费停用规则时账号应被停用")
	}
}

func TestProbeSpeechGeneric400StillAllowed(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"input is required"}}`)
	}))
	t.Cleanup(up.Close)
	acc := voiceProvider(t, f, up.URL, []string{"tts-1"})
	gs := newGateway(t, f)

	res, err := gs.ProbeModelProtocol(context.Background(), acc, "tts-1", gateway.ProtocolSpeech)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Access != store.AccessAllowed {
		t.Fatalf("普通 400 仍应判可达，实际 %+v", res)
	}
	if !strings.Contains(res.Message, "凭据未验证") {
		t.Fatalf("备注 = %q，期望保留「凭据未验证」", res.Message)
	}
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("普通语音 400 不该停用账号")
	}
}

func TestProbeUser401Cooldown(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusUnauthorized, body: `{"error":{"message":"invalid key"}}`},
	})
	acc := f.addAs(t, "cool", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})
	policy := createFailurePolicy(t, f, store.RouteFailurePolicy{
		AccountID:       acc.ID,
		Statuses:        []string{"401"},
		CooldownSeconds: 10 * 60,
		ReturnError:     true,
		Note:            "401 冷却十分钟",
	})
	gs := newGateway(t, f)
	before := time.Now()

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != http.StatusUnauthorized || res.OK {
		t.Fatalf("探活结果 = %+v，期望 401 且不 OK", res)
	}
	if res.Policy == nil || res.Policy.Source != account.FailureSourceUser || res.Policy.ID != policy.ID {
		t.Fatalf("policy = %+v，期望用户规则 #%d", res.Policy, policy.ID)
	}
	if !strings.Contains(res.PolicyAction, "账号冷却 10m") {
		t.Fatalf("policy_action = %q，期望「账号冷却 10m」", res.PolicyAction)
	}
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("用户 401 冷却规则不该停用账号")
	}
	got := time.UnixMilli(stored.CooldownUntilMS)
	if got.Before(before.Add(9*time.Minute)) || got.After(before.Add(11*time.Minute)) {
		t.Fatalf("账号冷却截止 = %v，期望约 10 分钟后", got)
	}
	if mf := stored.ModelFailures["m1"]; mf.CooldownUntilMS != 0 {
		t.Fatalf("规则没带 --model，不该写模型级冷却: %+v", mf)
	}
}

func TestProbeServerErrorWithoutRuleKeepsModelCooldown(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusInternalServerError, body: `{"error":{"message":"upstream boom"}}`},
	})
	acc := f.addAs(t, "boom", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})
	gs := newGateway(t, f)
	before := time.Now()

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Policy != nil {
		t.Fatalf("没有规则命中时不该带 policy: %+v", res.Policy)
	}
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("5xx 不该停用账号")
	}
	mf := stored.ModelFailures["m1"]
	if mf.FailStreak != 1 {
		t.Fatalf("fail_streak = %d，期望 1", mf.FailStreak)
	}
	got := time.UnixMilli(mf.CooldownUntilMS)
	if got.Before(before.Add(9*time.Second)) || got.After(before.Add(11*time.Second)) {
		t.Fatalf("模型冷却截止 = %v，期望首次 5xx 的 10 秒档", got)
	}
}
