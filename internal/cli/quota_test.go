package cli_test

import (
	"math"
	"strings"
	"testing"
	"time"
)

const quotaCLIKey = "sk-cc-secret-123"

func quotaCLIRoutes() map[string]fakeResponse {
	return map[string]fakeResponse{
		"GET /alpha/whoami":          {status: 200, body: `{"user":{"displayName":"thesumwaimzkz"}}`},
		"GET /alpha/billing/credits": {status: 200, body: `{"monthlyCredits":24.6301}`},
		"GET /alpha/billing/subscriptions": {status: 200, body: `{
			"plan":{"id":"pro","status":"active"},
			"currentPeriodEnd":"2026-12-01T00:00:00Z",
			"cancelAtPeriodEnd":true,
			"windowLimits":{
				"fiveHour":{"used":2,"limit":10,"resetAt":1760000000000},
				"weekly":{"used":30,"limit":100,"resetAt":1760500000000}
			}
		}`},
		"GET /alpha/usage/summary": {status: 200, body: `{"summary":{"totalCredits":45.33,"requests":123,"successRate":0.98}}`},
	}
}

func quotaMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("缺少字段 %q: %+v", key, m)
	}
	out, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%q 不是对象: %T", key, v)
	}
	return out
}

func quotaNumber(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("缺少字段 %q: %+v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%q 不是数字: %T", key, v)
	}
	return f
}

func quotaNearly(got, want float64) bool {
	return math.Abs(got-want) <= 1e-6
}

func quotaAssertNoSecret(t *testing.T, where, out string) {
	t.Helper()
	if strings.Contains(out, quotaCLIKey) {
		t.Fatalf("%s 的输出泄漏了账号 key: %s", where, out)
	}
}

func TestQuotaCommands(t *testing.T) {
	sandbox(t)
	mustInit(t)
	up := newRecorderUpstream(t, quotaCLIRoutes())
	if code, _, stderr := addAccount(t, "cc1", "commandcode", up.srv.URL, "", "--key", quotaCLIKey); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	// 没刷新过：ls 人读输出显示「未刷新」，JSON 里 snapshot=false。
	code, out, stderr := run(t, "quota", "ls")
	if code != 0 {
		t.Fatalf("quota ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "未刷新") {
		t.Fatalf("quota ls 应显示未刷新: %s", out)
	}
	code, out, stderr = run(t, "quota", "ls", "--json")
	if code != 0 {
		t.Fatalf("quota ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var ls []map[string]any
	mustJSON(t, out, &ls)
	if len(ls) != 1 || ls[0]["account"] != "cc1" || ls[0]["snapshot"] != false || ls[0]["status"] != "" {
		t.Fatalf("刷新前的 quota ls = %+v", ls)
	}
	quotaAssertNoSecret(t, "quota ls", out)

	// 手动刷新：4 个端点、x-api-key、不带 Authorization，落一行快照。
	code, out, stderr = run(t, "quota", "refresh", "cc1", "--json")
	if code != 0 {
		t.Fatalf("quota refresh 退出码 = %d，stderr = %s", code, stderr)
	}
	var refreshRows []map[string]any
	mustJSON(t, out, &refreshRows)
	if len(refreshRows) != 1 || refreshRows[0]["status"] != "ok" || refreshRows[0]["skipped"] != false {
		t.Fatalf("刷新结果 = %+v", refreshRows)
	}
	facts := quotaMap(t, refreshRows[0], "facts")
	if facts["status"] != "ok" || facts["display_name"] != "thesumwaimzkz" || facts["currency"] != "USD" {
		t.Fatalf("刷新事实 = %+v", facts)
	}
	if !quotaNearly(quotaNumber(t, facts, "balance"), 24.6301) ||
		!quotaNearly(quotaNumber(t, facts, "balance_usd"), 24.6301) ||
		!quotaNearly(quotaNumber(t, facts, "used"), 45.33) ||
		!quotaNearly(quotaNumber(t, facts, "used_usd"), 45.33) ||
		!quotaNearly(quotaNumber(t, facts, "total_usd"), 69.9601) ||
		!quotaNearly(quotaNumber(t, facts, "remaining_ratio"), 24.6301/69.9601) {
		t.Fatalf("金额字段 = %+v", facts)
	}
	usage := quotaMap(t, facts, "usage")
	if !quotaNearly(quotaNumber(t, usage, "total_credits"), 45.33) ||
		quotaNumber(t, usage, "requests") != 123 ||
		!quotaNearly(quotaNumber(t, usage, "success_rate"), 0.98) {
		t.Fatalf("上游用量 = %+v", usage)
	}
	five := quotaMap(t, facts, "five_hour")
	if !quotaNearly(quotaNumber(t, five, "used"), 2) || !quotaNearly(quotaNumber(t, five, "limit"), 10) {
		t.Fatalf("5 小时窗口 = %+v", five)
	}
	wantEnd := float64(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	if quotaNumber(t, facts, "period_end") != wantEnd {
		t.Fatalf("周期结束 = %v，期望 %v", facts["period_end"], wantEnd)
	}
	if facts["plan_id"] != "pro" || facts["plan_status"] != "active" || facts["cancel_at_period_end"] != true {
		t.Fatalf("套餐字段 = %+v", facts)
	}
	quotaAssertNoSecret(t, "quota refresh", out)
	quotaAssertNoSecret(t, "quota refresh stderr", stderr)

	calls := up.all()
	if len(calls) != 4 {
		t.Fatalf("上游调用 = %+v，期望 4 次", calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.method != "GET" {
			t.Fatalf("方法 = %s", c.method)
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
		if c.apiKey != quotaCLIKey {
			t.Fatalf("x-api-key = %q", c.apiKey)
		}
		if c.bearer != "" {
			t.Fatalf("CommandCode 不该发 Authorization: %q", c.bearer)
		}
	}

	// show 展示完整事实与两条说明。
	code, out, stderr = run(t, "quota", "show", "cc1", "--json")
	if code != 0 {
		t.Fatalf("quota show 退出码 = %d，stderr = %s", code, stderr)
	}
	var show map[string]any
	mustJSON(t, out, &show)
	if show["account"] != "cc1" || show["snapshot"] != true || show["status"] != "ok" {
		t.Fatalf("quota show = %+v", show)
	}
	if show["plan_id"] != "pro" || show["display_name"] != "thesumwaimzkz" {
		t.Fatalf("quota show 事实 = %+v", show)
	}
	quotaAssertNoSecret(t, "quota show", out)

	code, out, stderr = run(t, "quota", "show", "cc1")
	if code != 0 {
		t.Fatalf("quota show 人读退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"5 小时/每周窗口是限流窗口", "上游用量是所有客户端合计", "thesumwaimzkz"} {
		if !strings.Contains(out, want) {
			t.Fatalf("quota show 人读输出缺少 %q: %s", want, out)
		}
	}

	// 额度刷新不改账号启用状态，也不写请求记录。
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var accounts []map[string]any
	mustJSON(t, out, &accounts)
	if len(accounts) != 1 || accounts[0]["enabled"] != true {
		t.Fatalf("额度刷新后账号 = %+v，期望仍启用", accounts)
	}
	code, out, stderr = run(t, "log", "tail", "--json")
	if code != 0 {
		t.Fatalf("log tail 退出码 = %d，stderr = %s", code, stderr)
	}
	var records []map[string]any
	mustJSON(t, out, &records)
	if len(records) != 0 {
		t.Fatalf("额度刷新不该新增请求记录: %+v", records)
	}

	// set 是 patch 语义：只改显式给出的项。
	code, out, stderr = run(t, "quota", "set", "cc1",
		"--source", "commandcode", "--fx-rate", "0.14", "--auto-refresh",
		"--expiry-mode", "deprioritize", "--expiry-days", "5", "--json")
	if code != 0 {
		t.Fatalf("quota set 退出码 = %d，stderr = %s", code, stderr)
	}
	var setOut map[string]any
	mustJSON(t, out, &setOut)
	settings := quotaMap(t, setOut, "settings")
	if settings["source"] != "commandcode" || settings["auto_refresh"] != true ||
		settings["expiry_mode"] != "deprioritize" || settings["expiry_days"] != float64(5) ||
		!quotaNearly(quotaNumber(t, settings, "fx_rate"), 0.14) {
		t.Fatalf("set 后的设置 = %+v", settings)
	}

	code, out, stderr = run(t, "quota", "set", "cc1", "--expiry-days", "7", "--json")
	if code != 0 {
		t.Fatalf("quota set patch 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &setOut)
	settings = quotaMap(t, setOut, "settings")
	if settings["source"] != "commandcode" || settings["auto_refresh"] != true ||
		settings["expiry_mode"] != "deprioritize" || settings["expiry_days"] != float64(7) ||
		!quotaNearly(quotaNumber(t, settings, "fx_rate"), 0.14) {
		t.Fatalf("patch 应保留其它项: %+v", settings)
	}

	code, out, stderr = run(t, "quota", "unset", "cc1", "fx_rate", "--json")
	if code != 0 {
		t.Fatalf("quota unset 字段退出码 = %d，stderr = %s", code, stderr)
	}
	var unsetOut map[string]any
	mustJSON(t, out, &unsetOut)
	settings = quotaMap(t, unsetOut, "settings")
	if settings["fx_rate"] != nil || settings["auto_refresh"] != true || settings["source"] != "commandcode" {
		t.Fatalf("unset 只该清一项: %+v", settings)
	}

	code, out, stderr = run(t, "quota", "unset", "cc1", "--json")
	if code != 0 {
		t.Fatalf("quota unset 整行退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &unsetOut)
	if unsetOut["removed"] != true {
		t.Fatalf("整行 unset 应删到行: %+v", unsetOut)
	}
	settings = quotaMap(t, unsetOut, "settings")
	if settings["source"] != "auto" || settings["fx_rate"] != nil || settings["auto_refresh"] != false ||
		settings["expiry_mode"] != "warn" || settings["expiry_days"] != float64(3) {
		t.Fatalf("整行 unset 后应回到默认: %+v", settings)
	}

	// --all 走同样的链路；默认 auto 来源仍按厂商映射到 commandcode。
	code, out, stderr = run(t, "quota", "refresh", "--all", "--json")
	if code != 0 {
		t.Fatalf("quota refresh --all 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &refreshRows)
	if len(refreshRows) != 1 || refreshRows[0]["status"] != "ok" {
		t.Fatalf("refresh --all 结果 = %+v", refreshRows)
	}
	quotaAssertNoSecret(t, "quota refresh --all", out)
}

// TestQuotaSourceOffClearsSnapshot 钉住 quota set --source off 后不再显示旧快照，
// 后续刷新按 unsupported 跳过，也不会把旧余额写回来。
func TestQuotaSourceOffClearsSnapshot(t *testing.T) {
	sandbox(t)
	mustInit(t)
	up := newRecorderUpstream(t, quotaCLIRoutes())
	if code, _, stderr := addAccount(t, "cc1", "commandcode", up.srv.URL, "", "--key", quotaCLIKey); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "quota", "refresh", "cc1", "--json"); code != 0 {
		t.Fatalf("刷新退出码 = %d，stderr = %s", code, stderr)
	}

	code, _, stderr := run(t, "quota", "set", "cc1", "--source", "off", "--json")
	if code != 0 {
		t.Fatalf("关闭监控退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr := run(t, "quota", "show", "cc1", "--json")
	if code != 0 {
		t.Fatalf("quota show 退出码 = %d，stderr = %s", code, stderr)
	}
	var show map[string]any
	mustJSON(t, out, &show)
	if show["snapshot"] != false {
		t.Fatalf("关闭监控后不该再显示旧快照: %+v", show)
	}
	if settings := quotaMap(t, show, "settings"); settings["source"] != "off" {
		t.Fatalf("来源 = %v，期望 off", settings["source"])
	}

	code, out, stderr = run(t, "quota", "refresh", "cc1", "--json")
	if code != 0 {
		t.Fatalf("关闭后刷新退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []map[string]any
	mustJSON(t, out, &rows)
	if len(rows) != 1 || rows[0]["skipped"] != true || rows[0]["status"] != "unsupported" {
		t.Fatalf("关闭后的刷新结果 = %+v", rows)
	}
	code, out, stderr = run(t, "quota", "show", "cc1", "--json")
	if code != 0 {
		t.Fatalf("再次 quota show 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &show)
	if show["snapshot"] != false {
		t.Fatalf("跳过的刷新不该写回快照: %+v", show)
	}
}

func TestQuotaRefresh401KeepsAccountEnabled(t *testing.T) {
	sandbox(t)
	mustInit(t)
	body := `{"error":{"message":"invalid key ` + quotaCLIKey + `"}}`
	routes := map[string]fakeResponse{}
	for _, path := range []string{"/alpha/whoami", "/alpha/billing/credits", "/alpha/billing/subscriptions", "/alpha/usage/summary"} {
		routes["GET "+path] = fakeResponse{status: 401, body: body}
	}
	up := newRecorderUpstream(t, routes)
	if code, _, stderr := addAccount(t, "cc1", "commandcode", up.srv.URL, "", "--key", quotaCLIKey); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "quota", "refresh", "cc1", "--json")
	if code != 1 {
		t.Fatalf("401 刷新的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	var rows []map[string]any
	mustJSON(t, out, &rows)
	if len(rows) != 1 || rows[0]["status"] != "auth" {
		t.Fatalf("刷新结果 = %+v", rows)
	}
	facts := quotaMap(t, rows[0], "facts")
	if facts["status"] != "auth" || facts["last_error"] == "" {
		t.Fatalf("失败事实 = %+v", facts)
	}
	sourceErrors := quotaMap(t, facts, "source_errors")
	if len(sourceErrors) == 0 {
		t.Fatal("应记录失败端点")
	}
	for endpoint, raw := range sourceErrors {
		msg, _ := raw.(string)
		if strings.Contains(msg, quotaCLIKey) {
			t.Fatalf("端点 %s 的错误泄漏 key: %q", endpoint, msg)
		}
	}
	quotaAssertNoSecret(t, "quota refresh 401", out)
	quotaAssertNoSecret(t, "quota refresh 401 stderr", stderr)

	// 账号仍启用；额度 401 不能触发真实请求的停用逻辑。
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var accounts []map[string]any
	mustJSON(t, out, &accounts)
	if len(accounts) != 1 || accounts[0]["enabled"] != true {
		t.Fatalf("额度 401 后账号 = %+v，期望仍启用", accounts)
	}
	code, out, stderr = run(t, "log", "tail", "--json")
	if code != 0 {
		t.Fatalf("log tail 退出码 = %d，stderr = %s", code, stderr)
	}
	var records []map[string]any
	mustJSON(t, out, &records)
	if len(records) != 0 {
		t.Fatalf("额度刷新不该写请求记录: %+v", records)
	}

	// 失败状态要落库，show 能看到。
	code, out, stderr = run(t, "quota", "show", "cc1", "--json")
	if code != 0 {
		t.Fatalf("quota show 退出码 = %d，stderr = %s", code, stderr)
	}
	var show map[string]any
	mustJSON(t, out, &show)
	if show["snapshot"] != true || show["status"] != "auth" {
		t.Fatalf("失败快照 = %+v", show)
	}
	quotaAssertNoSecret(t, "quota show 401", out)
}

func TestQuotaUnsupportedAndValidation(t *testing.T) {
	sandbox(t)
	mustInit(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{})
	if code, _, stderr := addAccount(t, "c1", "custom", up.srv.URL, "", "--key", quotaCLIKey); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	// 不支持的厂商跳过，不算失败，也不发请求、不落快照。
	code, out, stderr := run(t, "quota", "refresh", "c1", "--json")
	if code != 0 {
		t.Fatalf("不支持厂商的退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []map[string]any
	mustJSON(t, out, &rows)
	if len(rows) != 1 || rows[0]["skipped"] != true || rows[0]["status"] != "unsupported" {
		t.Fatalf("跳过结果 = %+v", rows)
	}
	if len(up.all()) != 0 {
		t.Fatalf("不支持的厂商不该发请求: %+v", up.all())
	}
	code, out, stderr = run(t, "quota", "show", "c1", "--json")
	if code != 0 {
		t.Fatalf("quota show 退出码 = %d，stderr = %s", code, stderr)
	}
	var show map[string]any
	mustJSON(t, out, &show)
	if show["snapshot"] != false {
		t.Fatalf("跳过的账号不该有快照: %+v", show)
	}

	// 非法设置都是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"quota", "set", "c1", "--fx-rate", "0", "--json"},
		{"quota", "set", "c1", "--source", "nope", "--json"},
		{"quota", "set", "c1", "--expiry-days", "0", "--json"},
		{"quota", "set", "c1", "--expiry-mode", "nope", "--json"},
		{"quota", "set", "c1", "--json"},
		{"quota", "unset", "c1", "nope"},
	} {
		code, out, stderr := run(t, args...)
		if code != 2 {
			t.Fatalf("%v 的退出码 = %d，期望 2（用法错误）；stderr = %s", args, code, stderr)
		}
		if out != "" {
			t.Fatalf("%v 的用法错误不该写 stdout: %s", args, out)
		}
		if !strings.Contains(stderr, "用法：") {
			t.Fatalf("%v 的 stderr 应带用法行: %s", args, stderr)
		}
	}

	// 组不带子命令等于帮助，退出码 0。
	code, out, stderr = run(t, "quota")
	if code != 0 {
		t.Fatalf("quota 组帮助退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"quota", "ls", "show", "refresh", "set", "unset"} {
		if !strings.Contains(out, want) {
			t.Fatalf("quota 组帮助缺少 %q: %s", want, out)
		}
	}
}
