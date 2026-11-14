package quota_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"lax/internal/quota"
)

func fp(v float64) *float64 { return &v }

func approx(got, want, eps float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= eps
}

func requireFloat(t *testing.T, name string, got float64, want, eps float64) {
	t.Helper()
	if !approx(got, want, eps) {
		t.Fatalf("%s = %v，期望约 %v", name, got, want)
	}
}

func requirePtr(t *testing.T, name string, got, want *float64, eps float64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("%s = %v，期望 %v", name, got, want)
		}
		return
	}
	if !approx(*got, *want, eps) {
		t.Fatalf("%s = %v，期望约 %v", name, *got, *want)
	}
}

// TestApplyCommandCode 钉住「分母 = 余额 + 已用」与 credits 按 1.0 折算。
func TestApplyCommandCode(t *testing.T) {
	f := quota.Facts{
		Currency:     "USD",
		Balance:      24.6301,
		BalanceKnown: true,
		Used:         45.33,
		UsedKnown:    true,
	}
	got := quota.Apply(f, quota.DefaultSettings())

	requireFloat(t, "BalanceUSD", deref(got.BalanceUSD), 24.6301, 1e-9)
	requireFloat(t, "UsedUSD", deref(got.UsedUSD), 45.33, 1e-9)
	requireFloat(t, "TotalUSD", deref(got.TotalUSD), 69.9601, 1e-9)
	if got.RemainingRatio == nil {
		t.Fatal("余额与已用都已知时应有剩余比例")
	}
	requireFloat(t, "RemainingRatio", *got.RemainingRatio, 24.6301/69.9601, 1e-9)
	requireFloat(t, "FXRate", deref(got.FXRate), 1, 1e-9)

	// 余额很小（例如 0.069）时仍按同一公式算，不能被当成未知。
	small := quota.Apply(quota.Facts{
		Currency: "USD", Balance: 0.069, BalanceKnown: true, UsedKnown: true,
	}, quota.DefaultSettings())
	requireFloat(t, "小余额 BalanceUSD", deref(small.BalanceUSD), 0.069, 1e-9)
	requireFloat(t, "小余额 TotalUSD", deref(small.TotalUSD), 0.069, 1e-9)
	if small.RemainingRatio == nil || *small.RemainingRatio != 1 {
		t.Fatalf("小余额的剩余比例 = %v，期望 1", small.RemainingRatio)
	}
}

// TestApplyCNYWithoutFX 钉住「汇率未设置时 USD 值一律未知，原值仍保留」。
func TestApplyCNYWithoutFX(t *testing.T) {
	f := quota.Facts{
		Currency:     "CNY",
		Balance:      117.84,
		BalanceKnown: true,
	}
	got := quota.Apply(f, quota.DefaultSettings())
	if got.BalanceUSD != nil || got.UsedUSD != nil || got.TotalUSD != nil || got.RemainingRatio != nil || got.FXRate != nil {
		t.Fatalf("CNY 未设汇率时应全部未知，实际 %+v", got)
	}
	if !got.BalanceKnown || got.Balance != 117.84 || got.Currency != "CNY" {
		t.Fatalf("原值不该被清掉: %+v", got)
	}

	cfg := quota.DefaultSettings()
	cfg.FXRate = fp(0.14)
	got = quota.Apply(f, cfg)
	requireFloat(t, "CNY BalanceUSD", deref(got.BalanceUSD), 117.84*0.14, 1e-9)
	if got.TotalUSD != nil || got.RemainingRatio != nil {
		t.Fatalf("DeepSeek 只有余额时总额与比例应未知: %+v", got)
	}
}

// TestApplyCreditsAndUSD 钉住 USD/CREDITS 永远按 1.0，不被 CNY 汇率影响。
func TestApplyCreditsAndUSD(t *testing.T) {
	cfg := quota.DefaultSettings()
	cfg.FXRate = fp(7.2)
	for _, currency := range []string{"USD", "CREDITS", ""} {
		got := quota.Apply(quota.Facts{
			Currency: currency, Balance: 5, BalanceKnown: true, UsedKnown: true, Used: 5,
		}, cfg)
		requireFloat(t, currency+" BalanceUSD", deref(got.BalanceUSD), 5, 1e-9)
		requireFloat(t, currency+" FXRate", deref(got.FXRate), 1, 1e-9)
	}
}

func TestExpiryAt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cfg := quota.DefaultSettings() // warn / 3 天

	near := now.Add(48 * time.Hour).UnixMilli()
	state, action := quota.ExpiryAt(near, now, cfg)
	if state != quota.ExpiryNear || action != quota.ExpiryModeWarn {
		t.Fatalf("2 天后到期 = %q / %q，期望 near / warn", state, action)
	}

	expired := now.Add(-time.Minute).UnixMilli()
	state, action = quota.ExpiryAt(expired, now, cfg)
	if state != quota.ExpiryExpired || action != quota.ExpiryModeWarn {
		t.Fatalf("已到期 = %q / %q，期望 expired / warn", state, action)
	}

	far := now.Add(10 * 24 * time.Hour).UnixMilli()
	if state, action := quota.ExpiryAt(far, now, cfg); state != "" || action != "" {
		t.Fatalf("10 天后到期 = %q / %q，期望都不设", state, action)
	}

	cfg.ExpiryMode = quota.ExpiryModeExclude
	if _, action := quota.ExpiryAt(near, now, cfg); action != quota.ExpiryModeExclude {
		t.Fatalf("exclude 模式下动作 = %q", action)
	}
	if state, _ := quota.ExpiryAt(0, now, cfg); state != "" {
		t.Fatalf("周期未知时状态 = %q，期望空", state)
	}
}

func TestValidateSettings(t *testing.T) {
	if err := quota.ValidateSettings(quota.DefaultSettings()); err != nil {
		t.Fatalf("默认设置应合法: %v", err)
	}
	off := quota.DefaultSettings()
	off.Source = quota.SourceOff
	if err := quota.ValidateSettings(off); err != nil {
		t.Fatalf("off 来源应合法: %v", err)
	}

	bad := []struct {
		name string
		cfg  quota.Settings
	}{
		{"未知来源", func() quota.Settings { c := quota.DefaultSettings(); c.Source = "nope"; return c }()},
		{"汇率 0", func() quota.Settings { c := quota.DefaultSettings(); c.FXRate = fp(0); return c }()},
		{"汇率负数", func() quota.Settings { c := quota.DefaultSettings(); c.FXRate = fp(-1); return c }()},
		{"汇率 NaN", func() quota.Settings { c := quota.DefaultSettings(); c.FXRate = fp(math.NaN()); return c }()},
		{"未知到期处理", func() quota.Settings { c := quota.DefaultSettings(); c.ExpiryMode = "nope"; return c }()},
		{"提前 0 天", func() quota.Settings { c := quota.DefaultSettings(); c.ExpiryDays = 0; return c }()},
		{"提前 3651 天", func() quota.Settings { c := quota.DefaultSettings(); c.ExpiryDays = 3651; return c }()},
		{"缺 scheme", func() quota.Settings { c := quota.DefaultSettings(); c.BaseURL = "api.example.com"; return c }()},
		{"ftp", func() quota.Settings { c := quota.DefaultSettings(); c.BaseURL = "ftp://api.example.com"; return c }()},
		{"明文 http 非本机", func() quota.Settings { c := quota.DefaultSettings(); c.BaseURL = "http://api.example.com"; return c }()},
	}
	for _, tc := range bad {
		if err := quota.ValidateSettings(tc.cfg); err == nil {
			t.Fatalf("%s：期望校验失败", tc.name)
		}
	}

	withURL := quota.DefaultSettings()
	withURL.BaseURL = "https://api.commandcode.ai/provider/v1"
	if err := quota.ValidateSettings(withURL); err != nil {
		t.Fatalf("合法地址应通过: %v", err)
	}

	// 本机代理允许明文 http：127.0.0.1 与 ::1 都算回环。
	for _, raw := range []string{
		"http://127.0.0.1:8080/v1",
		"http://[::1]:8080/v1",
		"http://localhost:8080/v1",
	} {
		cfg := quota.DefaultSettings()
		cfg.BaseURL = raw
		if err := quota.ValidateSettings(cfg); err != nil {
			t.Fatalf("本机明文地址 %s 应通过: %v", raw, err)
		}
	}
}

func TestValidateOrderAndMinUSD(t *testing.T) {
	for _, order := range []string{
		quota.OrderOff, quota.OrderExpiryAsc, quota.OrderExpiryDesc,
		quota.OrderRatioDesc, quota.OrderRatioAsc, quota.OrderUSDDesc, quota.OrderUSDAsc,
	} {
		if err := quota.ValidateOrder(order); err != nil {
			t.Fatalf("排序 %q 应合法: %v", order, err)
		}
	}
	if err := quota.ValidateOrder("nope"); err == nil {
		t.Fatal("未知排序应报错")
	}
	if err := quota.ValidateMinUSD(0); err != nil {
		t.Fatalf("0 应合法: %v", err)
	}
	for _, v := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := quota.ValidateMinUSD(v); err == nil {
			t.Fatalf("%v 应被拒绝", v)
		}
	}
}

func TestQuotaURLs(t *testing.T) {
	urls, err := quota.CommandCodeURLs("https://api.commandcode.ai/provider/v1?x=1")
	if err != nil {
		t.Fatalf("拼 CommandCode 地址: %v", err)
	}
	if urls.WhoAmI != "https://api.commandcode.ai/alpha/whoami" ||
		urls.Credits != "https://api.commandcode.ai/alpha/billing/credits" ||
		urls.Subscriptions != "https://api.commandcode.ai/alpha/billing/subscriptions" ||
		urls.Usage != "https://api.commandcode.ai/alpha/usage/summary" {
		t.Fatalf("CommandCode 地址 = %+v", urls)
	}
	if _, err := quota.CommandCodeURLs(""); err == nil {
		t.Fatal("空地址应报错")
	}
	if _, err := quota.CommandCodeURLs("api.commandcode.ai"); err == nil {
		t.Fatal("缺 scheme 应报错")
	}

	// 额度请求带账号 key：非本机地址不允许明文 http，本机回环地址放行。
	for _, raw := range []string{
		"http://api.commandcode.ai/v1",
		"http://203.0.113.10/v1",
	} {
		if _, err := quota.CommandCodeURLs(raw); err == nil {
			t.Fatalf("%s 是明文 http 的非本机地址，应拒绝", raw)
		}
	}
	for _, raw := range []string{
		"http://127.0.0.1:8080/v1",
		"http://[::1]:8080/v1",
		"http://localhost:8080/v1",
	} {
		if _, err := quota.CommandCodeURLs(raw); err != nil {
			t.Fatalf("%s 应允许: %v", raw, err)
		}
	}

	ds, err := quota.DeepSeekBalanceURL("https://api.deepseek.com/v1")
	if err != nil || ds != "https://api.deepseek.com/user/balance" {
		t.Fatalf("DeepSeek 地址 = %q, %v", ds, err)
	}

	ua := quota.BrowserUserAgent()
	if !strings.Contains(ua, "Mozilla/5.0") || !strings.Contains(ua, "Chrome") {
		t.Fatalf("浏览器 UA = %q", ua)
	}
}

func TestParseCommandCodeWhoAmI(t *testing.T) {
	name, err := quota.ParseCommandCodeWhoAmI([]byte(`{"displayName":"thesumwaimzkz"}`))
	if err != nil || name != "thesumwaimzkz" {
		t.Fatalf("顶层显示名 = %q, %v", name, err)
	}
	name, err = quota.ParseCommandCodeWhoAmI([]byte(`{"user":{"displayName":"nested-name"}}`))
	if err != nil || name != "nested-name" {
		t.Fatalf("嵌套显示名 = %q, %v", name, err)
	}
	if _, err := quota.ParseCommandCodeWhoAmI([]byte(`{"user":{}}`)); err == nil {
		t.Fatal("没有显示名时应报错，交给调用方当非致命失败")
	}
	if _, err := quota.ParseCommandCodeWhoAmI([]byte(`not-json`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestParseCommandCodeCredits(t *testing.T) {
	credits, err := quota.ParseCommandCodeCredits([]byte(`{
		"credits":{"monthlyCredits":24.6301},
		"windowLimits":{
			"limited":true,
			"fiveHour":{"used":14.0221354831,"cap":14,"resetAt":1794694603609},
			"weekly":{"used":14.0221354831,"cap":35,"resetAt":1795281403609}
		}
	}`))
	if err != nil {
		t.Fatalf("解析 credits: %v", err)
	}
	requireFloat(t, "余额", credits.Balance, 24.6301, 1e-9)
	if credits.Currency != "USD" {
		t.Fatalf("默认币种 = %q，期望 USD", credits.Currency)
	}
	if credits.FiveHour == nil || credits.FiveHour.Used == nil || *credits.FiveHour.Used != 14.0221354831 ||
		credits.FiveHour.Limit == nil || *credits.FiveHour.Limit != 14 || credits.FiveHour.ResetAt != 1794694603609 {
		t.Fatalf("5 小时窗口 = %+v", credits.FiveHour)
	}
	if credits.Weekly == nil || credits.Weekly.Used == nil || *credits.Weekly.Used != 14.0221354831 ||
		credits.Weekly.Limit == nil || *credits.Weekly.Limit != 35 || credits.Weekly.ResetAt != 1795281403609 {
		t.Fatalf("每周窗口 = %+v", credits.Weekly)
	}

	credits, err = quota.ParseCommandCodeCredits([]byte(`{"data":{"monthlyCredits":"117.84","currency":"cny"}}`))
	if err != nil {
		t.Fatalf("解析包装后的 credits: %v", err)
	}
	requireFloat(t, "包装余额", credits.Balance, 117.84, 1e-9)
	if credits.Currency != "CNY" {
		t.Fatalf("币种 = %q，期望 CNY", credits.Currency)
	}
	if credits.FiveHour != nil || credits.Weekly != nil {
		t.Fatalf("没有 windowLimits 时不该有窗口: %+v", credits)
	}

	if _, err := quota.ParseCommandCodeCredits([]byte(`{"foo":1}`)); err == nil {
		t.Fatal("没有余额字段时应报错")
	}
}

func TestParseCommandCodeSubscription(t *testing.T) {
	body := []byte(`{
		"plan":{"id":"pro","status":"active"},
		"currentPeriodEnd":"2026-12-01T00:00:00Z",
		"cancelAtPeriodEnd":true
	}`)
	sub, err := quota.ParseCommandCodeSubscription(body)
	if err != nil {
		t.Fatalf("解析订阅: %v", err)
	}
	wantEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if sub.PeriodEnd != wantEnd {
		t.Fatalf("PeriodEnd = %d，期望 %d", sub.PeriodEnd, wantEnd)
	}
	if sub.PlanID != "pro" || sub.PlanStatus != "active" || !sub.CancelAtPeriodEnd {
		t.Fatalf("套餐信息 = %+v", sub)
	}

	// currentPeriodEnd 同时接受 Unix 秒与 Unix 毫秒。
	for _, raw := range []string{
		`{"currentPeriodEnd":1764547200}`,
		`{"current_period_end":1764547200000}`,
	} {
		sub, err := quota.ParseCommandCodeSubscription([]byte(raw))
		if err != nil {
			t.Fatalf("解析 %s: %v", raw, err)
		}
		if sub.PeriodEnd != 1764547200000 {
			t.Fatalf("%s 的 PeriodEnd = %d", raw, sub.PeriodEnd)
		}
	}

	// 200 但没有任何已知字段时必须报错：否则零值会被当成成功，把上一次的到期时间、
	// 套餐与窗口清空，expiry_mode=exclude 就会静默失效。
	for _, raw := range []string{
		`{}`,
		`{"subscription":null}`,
		`{"data":{}}`,
		`{"status":"ok"}`,
	} {
		if _, err := quota.ParseCommandCodeSubscription([]byte(raw)); err == nil {
			t.Fatalf("%s 没有已知字段，应报错", raw)
		}
	}
}

func TestParseCommandCodeUsage(t *testing.T) {
	usage, err := quota.ParseCommandCodeUsage([]byte(`{"usage":{"summary":{"totalCredits":45.33,"requests":123,"successRate":0.98}}}`))
	if err != nil {
		t.Fatalf("解析用量: %v", err)
	}
	requireFloat(t, "totalCredits", deref(usage.TotalCredits), 45.33, 1e-9)
	requireFloat(t, "successRate", deref(usage.SuccessRate), 0.98, 1e-9)
	if usage.Requests == nil || *usage.Requests != 123 {
		t.Fatalf("requests = %v", usage.Requests)
	}

	empty, err := quota.ParseCommandCodeUsage([]byte(`{"summary":{}}`))
	if err != nil {
		t.Fatalf("空 summary 不应报错: %v", err)
	}
	if empty.TotalCredits != nil || empty.Requests != nil || empty.SuccessRate != nil {
		t.Fatalf("空 summary 应是零值: %+v", empty)
	}
}

func TestParseDeepSeekBalance(t *testing.T) {
	balance, currency, available, err := quota.ParseDeepSeekBalance([]byte(`{
		"is_available":true,
		"balance_infos":[
			{"currency":"USD","total_balance":"1.00"},
			{"currency":"CNY","total_balance":"117.84"}
		]
	}`))
	if err != nil {
		t.Fatalf("解析 DeepSeek 余额: %v", err)
	}
	requireFloat(t, "余额", balance, 117.84, 1e-9)
	if currency != "CNY" {
		t.Fatalf("多币种应优先 CNY，实际 %q", currency)
	}
	if available == nil || !*available {
		t.Fatalf("is_available = %v，期望 true", available)
	}

	if _, _, _, err := quota.ParseDeepSeekBalance([]byte(`{"balance_infos":[]}`)); err == nil {
		t.Fatal("没有余额时应报错")
	}
}

func deref(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}
