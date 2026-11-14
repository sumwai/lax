package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lax/internal/gateway"
	"lax/internal/quota"
	"lax/internal/store"
)

const quotaTestKey = "sk-cc-secret-123"

// quotaTestCall 是一次额度请求：方法、路径与三种要看得到的头。
type quotaTestCall struct {
	method string
	path   string
	apiKey string
	bearer string
	ua     string
}

type quotaTestReply struct {
	status int
	body   string
}

// quotaTestUpstream 是按路径回固定响应的假上游。它要求浏览器 UA：缺 UA 回 403，
// 用来证明我们的适配器确实带了 UA，而不是靠上游宽容。
type quotaTestUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []quotaTestCall
	routes map[string]quotaTestReply
}

func newQuotaTestUpstream(t *testing.T, routes map[string]quotaTestReply) *quotaTestUpstream {
	t.Helper()
	up := &quotaTestUpstream{routes: routes}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		up.mu.Lock()
		up.calls = append(up.calls, quotaTestCall{
			method: r.Method, path: r.URL.Path,
			apiKey: r.Header.Get("x-api-key"), bearer: r.Header.Get("Authorization"), ua: ua,
		})
		up.mu.Unlock()

		if !strings.Contains(ua, "Chrome") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"missing browser user-agent"}}`)
			return
		}
		resp, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"没有这个路由"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(up.srv.Close)
	return up
}

func (u *quotaTestUpstream) all() []quotaTestCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]quotaTestCall, len(u.calls))
	copy(out, u.calls)
	return out
}

func quotaGateway(t *testing.T, f *fixture) *gateway.Server {
	t.Helper()
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	return gs
}

func setQuotaProviderBase(t *testing.T, f *fixture, name, base string) {
	t.Helper()
	if err := f.st.SetProvider(name, store.ProviderPatch{BaseURL: &base}); err != nil {
		t.Fatalf("把厂商 %s 的地址改到假上游: %v", name, err)
	}
}

func requireQuotaFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s 为空，期望 %v", name, want)
	}
	if diff := *got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("%s = %v，期望约 %v", name, *got, want)
	}
}

func commandCodeQuotaRoutes() map[string]quotaTestReply {
	return map[string]quotaTestReply{
		"/alpha/whoami": {status: 200, body: `{"user":{"displayName":"thesumwaimzkz"}}`},
		"/alpha/billing/credits": {status: 200, body: `{
			"credits":{"monthlyCredits":24.6301},
			"windowLimits":{
				"fiveHour":{"used":2,"cap":10,"resetAt":1760000000000},
				"weekly":{"used":30,"cap":100,"resetAt":1760500000000}
			}
		}`},
		"/alpha/billing/subscriptions": {status: 200, body: `{
			"plan":{"id":"pro","status":"active"},
			"currentPeriodEnd":"2026-12-01T00:00:00Z",
			"cancelAtPeriodEnd":true
		}`},
		"/alpha/usage/summary": {status: 200, body: `{"summary":{"totalCredits":45.33,"requests":123,"successRate":0.98}}`},
	}
}

func TestRefreshQuotaCommandCode(t *testing.T) {
	f := newFixture(t)
	up := newQuotaTestUpstream(t, commandCodeQuotaRoutes())
	gs := quotaGateway(t, f)
	setQuotaProviderBase(t, f, "commandcode", up.srv.URL)
	acc := f.createAccount(t, "cc1", "commandcode", quotaTestKey, []string{"m1"}, 100)

	cfg := quota.DefaultSettings() // source=auto，按厂商映射到 commandcode
	facts, err := gs.RefreshQuota(context.Background(), acc, cfg)
	if err != nil {
		t.Fatalf("刷新额度: %v", err)
	}
	if facts.Source != quota.SourceCommandCode || facts.Status != quota.StatusOK {
		t.Fatalf("来源/状态 = %q / %q", facts.Source, facts.Status)
	}
	if facts.DisplayName != "thesumwaimzkz" || facts.PlanID != "pro" || facts.PlanStatus != "active" || !facts.CancelAtPeriodEnd {
		t.Fatalf("显示名/套餐 = %+v", facts)
	}
	if !facts.BalanceKnown || facts.Balance != 24.6301 || !facts.UsedKnown || facts.Used != 45.33 || facts.Currency != "USD" {
		t.Fatalf("余额/已用 = %+v", facts)
	}
	requireQuotaFloat(t, "BalanceUSD", facts.BalanceUSD, 24.6301)
	requireQuotaFloat(t, "UsedUSD", facts.UsedUSD, 45.33)
	requireQuotaFloat(t, "TotalUSD", facts.TotalUSD, 69.9601)
	requireQuotaFloat(t, "RemainingRatio", facts.RemainingRatio, 24.6301/69.9601)
	wantEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if facts.PeriodEnd != wantEnd {
		t.Fatalf("周期结束 = %d，期望 %d", facts.PeriodEnd, wantEnd)
	}
	if facts.FiveHour == nil || facts.FiveHour.Used == nil || *facts.FiveHour.Used != 2 ||
		facts.FiveHour.Limit == nil || *facts.FiveHour.Limit != 10 || facts.FiveHour.ResetAt != 1760000000000 {
		t.Fatalf("5 小时窗口 = %+v", facts.FiveHour)
	}
	if facts.Weekly == nil || facts.Weekly.Used == nil || *facts.Weekly.Used != 30 || facts.Weekly.ResetAt != 1760500000000 {
		t.Fatalf("每周窗口 = %+v", facts.Weekly)
	}
	if facts.Usage.TotalCredits == nil || *facts.Usage.TotalCredits != 45.33 ||
		facts.Usage.Requests == nil || *facts.Usage.Requests != 123 ||
		facts.Usage.SuccessRate == nil || *facts.Usage.SuccessRate != 0.98 {
		t.Fatalf("上游用量 = %+v", facts.Usage)
	}
	if facts.CheckedAt <= 0 || len(facts.SourceErrors) != 0 || facts.LastError != "" {
		t.Fatalf("刷新时间/错误 = %+v", facts)
	}

	calls := up.all()
	if len(calls) != 4 {
		t.Fatalf("上游调用 = %+v，期望 4 次", calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.method != http.MethodGet {
			t.Fatalf("方法 = %s，期望 GET", c.method)
		}
		switch c.path {
		case "/alpha/whoami", "/alpha/billing/credits", "/alpha/billing/subscriptions", "/alpha/usage/summary":
		default:
			t.Fatalf("意外路径 %s", c.path)
		}
		if seen[c.path] {
			t.Fatalf("路径 %s 打了两次", c.path)
		}
		seen[c.path] = true
		if c.apiKey != quotaTestKey {
			t.Fatalf("x-api-key = %q，期望账号 key", c.apiKey)
		}
		if c.bearer != "" {
			t.Fatalf("CommandCode 不该发 Authorization，实际 %q", c.bearer)
		}
		if !strings.Contains(c.ua, "Chrome") {
			t.Fatalf("User-Agent = %q，期望浏览器 UA", c.ua)
		}
	}

	// 额度刷新绝不改账号状态、不写请求记录。
	got, err := f.st.GetAccount("cc1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !got.Enabled {
		t.Fatal("额度刷新不应停用账号")
	}
	if recs, err := f.st.RecentRequests(10); err != nil || len(recs) != 0 {
		t.Fatalf("额度刷新不该写请求记录: %v / %v", recs, err)
	}
}

func TestRefreshQuotaPartialKeepsSuccessfulSegments(t *testing.T) {
	f := newFixture(t)
	routes := commandCodeQuotaRoutes()
	routes["/alpha/billing/credits"] = quotaTestReply{
		status: 500,
		body:   `{"error":{"message":"boom ` + quotaTestKey + `"}}`,
	}
	up := newQuotaTestUpstream(t, routes)
	gs := quotaGateway(t, f)
	acc := f.add(t, "cc2", gateway.ProtocolChat, up.srv.URL, quotaTestKey, []string{"m1"}, 100)

	cfg := quota.DefaultSettings()
	cfg.Source = quota.SourceCommandCode
	facts, err := gs.RefreshQuota(context.Background(), acc, cfg)
	if err != nil {
		t.Fatalf("部分失败不该返回错误: %v", err)
	}
	if facts.Status != quota.StatusPartial {
		t.Fatalf("状态 = %q，期望 partial", facts.Status)
	}
	if facts.SourceErrors["credits"] == "" {
		t.Fatalf("缺少 credits 的端点错误: %+v", facts.SourceErrors)
	}
	if strings.Contains(facts.SourceErrors["credits"], quotaTestKey) || strings.Contains(facts.LastError, quotaTestKey) {
		t.Fatalf("错误文字泄漏了 key: %+v / %q", facts.SourceErrors, facts.LastError)
	}
	if facts.DisplayName != "thesumwaimzkz" || !facts.UsedKnown || facts.Used != 45.33 {
		t.Fatalf("成功段应保留: %+v", facts)
	}
	if facts.BalanceKnown {
		t.Fatalf("credits 失败时余额不该有值: %+v", facts)
	}
	if len(up.all()) != 4 {
		t.Fatalf("四个端点都应尝试，实际 %+v", up.all())
	}
	if got, _ := f.st.GetAccount("cc2"); !got.Enabled {
		t.Fatal("部分失败不应停用账号")
	}
	if recs, _ := f.st.RecentRequests(10); len(recs) != 0 {
		t.Fatalf("额度刷新不该写请求记录: %+v", recs)
	}
}

func TestRefreshQuota401DoesNotDisableAccount(t *testing.T) {
	f := newFixture(t)
	routes := map[string]quotaTestReply{}
	body := `{"error":{"message":"invalid key ` + quotaTestKey + `"}}`
	for _, path := range []string{"/alpha/whoami", "/alpha/billing/credits", "/alpha/billing/subscriptions", "/alpha/usage/summary"} {
		routes[path] = quotaTestReply{status: 401, body: body}
	}
	up := newQuotaTestUpstream(t, routes)
	gs := quotaGateway(t, f)
	acc := f.add(t, "cc3", gateway.ProtocolChat, up.srv.URL, quotaTestKey, []string{"m1"}, 100)

	cfg := quota.DefaultSettings()
	cfg.Source = quota.SourceCommandCode
	facts, err := gs.RefreshQuota(context.Background(), acc, cfg)
	if err == nil {
		t.Fatal("401 应返回错误给 CLI")
	}
	if facts.Status != quota.StatusAuth {
		t.Fatalf("状态 = %q，期望 auth", facts.Status)
	}
	if strings.Contains(facts.LastError, quotaTestKey) {
		t.Fatalf("LastError 泄漏 key: %q", facts.LastError)
	}
	if len(facts.SourceErrors) == 0 {
		t.Fatal("应记录每个失败端点")
	}
	for endpoint, msg := range facts.SourceErrors {
		if strings.Contains(msg, quotaTestKey) {
			t.Fatalf("端点 %s 的错误泄漏 key: %q", endpoint, msg)
		}
	}

	got, err := f.st.GetAccount("cc3")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !got.Enabled {
		t.Fatal("额度 401 绝不能停用账号；只有真实请求 401 才走停用逻辑")
	}
	if recs, err := f.st.RecentRequests(10); err != nil || len(recs) != 0 {
		t.Fatalf("额度刷新不该写请求记录: %v / %v", recs, err)
	}
}

func TestRefreshQuotaDeepSeekBalance(t *testing.T) {
	f := newFixture(t)
	up := newQuotaTestUpstream(t, map[string]quotaTestReply{
		"/user/balance": {status: 200, body: `{"is_available":true,"balance_infos":[
			{"currency":"USD","total_balance":"1.00"},
			{"currency":"CNY","total_balance":"117.84"}
		]}`},
	})
	gs := quotaGateway(t, f)
	setQuotaProviderBase(t, f, "deepseek", up.srv.URL)
	acc := f.createAccount(t, "ds1", "deepseek", quotaTestKey, []string{"m1"}, 100)

	facts, err := gs.RefreshQuota(context.Background(), acc, quota.DefaultSettings())
	if err != nil {
		t.Fatalf("刷新 DeepSeek 余额: %v", err)
	}
	if facts.Source != quota.SourceDeepSeek || facts.Status != quota.StatusOK {
		t.Fatalf("来源/状态 = %q / %q", facts.Source, facts.Status)
	}
	if !facts.BalanceKnown || facts.Balance != 117.84 || facts.Currency != "CNY" {
		t.Fatalf("余额 = %+v", facts)
	}
	if facts.Available == nil || !*facts.Available {
		t.Fatalf("is_available = %v", facts.Available)
	}
	if facts.BalanceUSD != nil || facts.UsedUSD != nil || facts.TotalUSD != nil || facts.RemainingRatio != nil {
		t.Fatalf("汇率未知时应全部为空: %+v", facts)
	}

	cfg := quota.DefaultSettings()
	cfg.FXRate = &[]float64{0.14}[0]
	facts, err = gs.RefreshQuota(context.Background(), acc, cfg)
	if err != nil {
		t.Fatalf("带汇率刷新: %v", err)
	}
	requireQuotaFloat(t, "折算余额", facts.BalanceUSD, 117.84*0.14)
	if facts.TotalUSD != nil || facts.RemainingRatio != nil {
		t.Fatalf("DeepSeek 只有余额时总额/比例应未知: %+v", facts)
	}

	calls := up.all()
	if len(calls) != 2 {
		t.Fatalf("上游调用 = %+v，期望 2 次", calls)
	}
	for _, c := range calls {
		if c.path != "/user/balance" || c.method != http.MethodGet {
			t.Fatalf("DeepSeek 调用 = %+v", c)
		}
		if c.bearer != "Bearer "+quotaTestKey {
			t.Fatalf("Authorization = %q", c.bearer)
		}
		if c.apiKey != "" {
			t.Fatalf("DeepSeek 不该发 x-api-key，实际 %q", c.apiKey)
		}
		if !strings.Contains(c.ua, "Chrome") {
			t.Fatalf("User-Agent = %q", c.ua)
		}
	}
}

// TestRefreshQuotaRefusesCrossHostRedirect 钉住额度请求不跟随跨 host 重定向：
// 默认 client 会把自定义的 x-api-key 一并带到新主机，等于把账号 key 发给第三方。
func TestRefreshQuotaRefusesCrossHostRedirect(t *testing.T) {
	f := newFixture(t)
	target := newQuotaTestUpstream(t, map[string]quotaTestReply{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(source.Close)
	gs := quotaGateway(t, f)
	setQuotaProviderBase(t, f, "commandcode", source.URL)
	acc := f.createAccount(t, "cc-redirect", "commandcode", quotaTestKey, []string{"m1"}, 100)

	facts, err := gs.RefreshQuota(context.Background(), acc, quota.DefaultSettings())
	if err == nil {
		t.Fatal("跨 host 重定向应作为失败，而不是把请求带到新地址")
	}
	if facts.Status != quota.StatusError {
		t.Fatalf("状态 = %q，期望 error", facts.Status)
	}
	if calls := target.all(); len(calls) != 0 {
		t.Fatalf("跨 host 重定向把额度请求（含 key）发到了新主机: %+v", calls)
	}
}

func TestRefreshQuotaUnsupportedVendorSkips(t *testing.T) {
	f := newFixture(t)
	up := newQuotaTestUpstream(t, map[string]quotaTestReply{})
	gs := quotaGateway(t, f)
	acc := f.add(t, "other", gateway.ProtocolChat, up.srv.URL, quotaTestKey, []string{"m1"}, 100)

	facts, err := gs.RefreshQuota(context.Background(), acc, quota.DefaultSettings())
	if err != nil {
		t.Fatalf("不支持的厂商不该报错: %v", err)
	}
	if facts.Status != quota.StatusUnsupported {
		t.Fatalf("状态 = %q，期望 unsupported", facts.Status)
	}
	if facts.LastError == "" {
		t.Fatal("应说明为什么跳过")
	}
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("不支持的厂商不该发请求: %+v", calls)
	}
}

func TestRefreshQuotaUsesSettingsBaseURLFirst(t *testing.T) {
	f := newFixture(t)
	unused := newQuotaTestUpstream(t, map[string]quotaTestReply{})
	chosen := newQuotaTestUpstream(t, commandCodeQuotaRoutes())
	gs := quotaGateway(t, f)
	acc := f.add(t, "cc4", gateway.ProtocolChat, unused.srv.URL, quotaTestKey, []string{"m1"}, 100)

	cfg := quota.DefaultSettings()
	cfg.Source = quota.SourceCommandCode
	cfg.BaseURL = chosen.srv.URL
	facts, err := gs.RefreshQuota(context.Background(), acc, cfg)
	if err != nil {
		t.Fatalf("刷新: %v", err)
	}
	if facts.Status != quota.StatusOK {
		t.Fatalf("状态 = %q", facts.Status)
	}
	if len(unused.all()) != 0 {
		t.Fatalf("设置了额度地址后不该打账号厂商的地址: %+v", unused.all())
	}
	if len(chosen.all()) != 4 {
		t.Fatalf("应打设置的地址 4 次，实际 %+v", chosen.all())
	}
}
