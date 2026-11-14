package cli_test

import (
	"math"
	"strings"
	"testing"
)

func TestRoutePolicyAccountWeightsAndExplain(t *testing.T) {
	sandbox(t)
	mustInit(t)
	for _, name := range []string{"a1", "b1"} {
		code, _, stderr := addAccount(t, name, "route-test", "http://127.0.0.1:1", "openai-chat",
			"--key", "sk-"+name, "--models", "glm-5.2")
		if code != 0 {
			t.Fatalf("建账号 %s 退出码 = %d，stderr = %s", name, code, stderr)
		}
	}

	code, out, stderr := run(t, "route", "set", "glm-5.2", "--strategy", "softmax",
		"--sticky", "off", "--temperature", "0.3", "--explore", "10",
		"--score-priority", "0", "--score-success", "1", "--score-latency", "0")
	if code != 0 {
		t.Fatalf("route set 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "已设置 glm-5.2") || !strings.Contains(out, "softmax") {
		t.Fatalf("route set 输出 = %s", out)
	}

	code, out, stderr = run(t, "route", "show", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("route show 退出码 = %d，stderr = %s", code, stderr)
	}
	var policy map[string]any
	mustJSON(t, out, &policy)
	if policy["target"] != "glm-5.2" || policy["source"] != "model" || policy["strategy"] != "softmax" ||
		policy["sticky"] != "off" || policy["temperature"].(float64) != 0.3 ||
		policy["explore"].(float64) != 0.1 || policy["score_priority"].(float64) != 0 ||
		policy["score_success"].(float64) != 1 || policy["score_latency"].(float64) != 0 {
		t.Fatalf("route show = %+v", policy)
	}

	code, out, stderr = run(t, "route", "account", "set", "a1", "--model", "glm-5.2", "--weight", "3")
	if code != 0 {
		t.Fatalf("account set 模型权重退出码 = %d，stderr = %s", code, stderr)
	}
	code, _, stderr = run(t, "route", "account", "set", "a1", "--weight", "2")
	if code != 0 {
		t.Fatalf("account set 默认权重退出码 = %d，stderr = %s", code, stderr)
	}
	code, _, stderr = run(t, "route", "account", "set", "b1", "--model", "glm-5.2", "--weight", "1")
	if code != 0 {
		t.Fatalf("account set b1 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr = run(t, "route", "account", "ls", "--model", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var weights []struct {
		Account string `json:"account"`
		Model   string `json:"model"`
		Weight  int    `json:"weight"`
		Source  string `json:"source"`
	}
	mustJSON(t, out, &weights)
	if len(weights) != 2 {
		t.Fatalf("账号权重 = %+v，期望两个账号", weights)
	}
	byName := map[string]int{}
	source := map[string]string{}
	for _, row := range weights {
		byName[row.Account] = row.Weight
		source[row.Account] = row.Source
	}
	if byName["a1"] != 3 || source["a1"] != "model" || byName["b1"] != 1 || source["b1"] != "model" {
		t.Fatalf("模型权重解析 = %+v", weights)
	}

	if code, _, stderr := run(t, "route", "account", "unset", "a1", "--model", "glm-5.2"); code != 0 {
		t.Fatalf("unset 模型权重退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "account", "ls", "--model", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("unset 后 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &weights)
	for _, row := range weights {
		if row.Account == "a1" && (row.Weight != 2 || row.Source != "account") {
			t.Fatalf("删模型权重后应回落到账号默认 2: %+v", row)
		}
	}

	code, out, stderr = run(t, "route", "explain", "glm-5.2", "--seed", "42", "--json")
	if code != 0 {
		t.Fatalf("route explain 退出码 = %d，stderr = %s", code, stderr)
	}
	var explain struct {
		Model      string   `json:"model"`
		Order      []string `json:"order"`
		Candidates []struct {
			Account     string  `json:"account"`
			Weight      int     `json:"weight"`
			Probability float64 `json:"probability"`
		} `json:"candidates"`
		Excluded []any `json:"excluded"`
	}
	mustJSON(t, out, &explain)
	if explain.Model != "glm-5.2" || len(explain.Candidates) != 2 || len(explain.Order) != 2 {
		t.Fatalf("explain = %+v", explain)
	}
	sum := 0.0
	for _, c := range explain.Candidates {
		sum += c.Probability
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("explain 概率和 = %v", sum)
	}
	_, again, _ := run(t, "route", "explain", "glm-5.2", "--seed", "42", "--json")
	var replay struct {
		Order []string `json:"order"`
	}
	mustJSON(t, again, &replay)
	if strings.Join(explain.Order, ",") != strings.Join(replay.Order, ",") {
		t.Fatalf("同一 seed 的 explain 顺序不同: %v vs %v", explain.Order, replay.Order)
	}

	if code, _, stderr := run(t, "route", "unset", "glm-5.2"); code != 0 {
		t.Fatalf("route unset 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "show", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("unset 后 show 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &policy)
	if policy["source"] != "builtin" || policy["strategy"] != "priority" {
		t.Fatalf("删模型策略后应回到内置默认: %+v", policy)
	}
}

func TestRouteDefaultAliasAndUsage(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "a1", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-a", "--models", "glm-5.2"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "alias", "set", "fast", "glm-5.2"); code != 0 {
		t.Fatalf("建别名退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "route", "set", "--default", "--strategy", "rotate", "--window", "7d")
	if code != 0 {
		t.Fatalf("route set --default 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "lax route show --default") {
		t.Fatalf("--default 的下一步命令不对: %s", out)
	}
	code, out, stderr = run(t, "route", "show", "--default", "--json")
	if code != 0 {
		t.Fatalf("route show --default 退出码 = %d，stderr = %s", code, stderr)
	}
	var policy map[string]any
	mustJSON(t, out, &policy)
	if policy["source"] != "default" || policy["strategy"] != "rotate" || policy["window_seconds"].(float64) != 7*86400 {
		t.Fatalf("默认策略 = %+v", policy)
	}

	if code, _, stderr := run(t, "route", "set", "fast", "--strategy", "weighted"); code != 0 {
		t.Fatalf("按别名写策略退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "show", "fast", "--json")
	if code != 0 {
		t.Fatalf("按别名看策略退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &policy)
	if policy["target"] != "glm-5.2" || policy["source"] != "model" || policy["strategy"] != "weighted" {
		t.Fatalf("别名应写到真实模型名: %+v", policy)
	}

	code, out, stderr = run(t, "route", "ls")
	if code != 0 {
		t.Fatalf("route ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"default", "glm-5.2", "默认", "模型"} {
		if !strings.Contains(out, want) {
			t.Fatalf("route ls 缺 %q:\n%s", want, out)
		}
	}

	if code, _, _ := run(t, "route", "unset", "--default"); code != 0 {
		t.Fatalf("route unset --default 退出码 = %d", code)
	}
	if code, _, _ := run(t, "route", "unset", "--default"); code != 0 {
		t.Fatalf("重复 unset --default 应幂等")
	}

	cases := [][]string{
		{"route", "set", "glm-5.2"},
		{"route", "set", "glm-5.2", "--strategy", "unknown"},
		{"route", "set", "--default", "glm-5.2", "--strategy", "rotate"},
		{"route", "show"},
		{"route", "show", "--default", "glm-5.2"},
		{"route", "set", "glm-5.2", "--strategy", "softmax", "--window", "31d"},
		{"route", "account", "set", "a1"},
		{"route", "account", "set", "a1", "--weight", "0"},
	}
	for _, args := range cases {
		code, out, _ := run(t, args...)
		if code != 2 || out != "" {
			t.Fatalf("%v 应退出 2 且不写 stdout，实际 %d / %q", args, code, out)
		}
	}
}

// TestREADMEQuotaRouteSetExample 逐字跑 README「账号额度与余额」一节的 route set 示例。
// route set 要求 --strategy；文档里的这行漏过它，示例照抄会以用法错误退出。
func TestREADMEQuotaRouteSetExample(t *testing.T) {
	sandbox(t)
	mustInit(t)
	up := newRecorderUpstream(t, quotaCLIRoutes())
	if code, _, stderr := addAccount(t, "cc1", "commandcode", up.srv.URL, "", "--key", quotaCLIKey, "--models", "glm-5.2"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	// 与 README 第 290 行逐字一致（换行后拼成一条命令）。
	code, _, stderr := run(t, "route", "set", "glm-5.2", "--strategy", "priority",
		"--quota-order", "usd_desc", "--quota-min-usd", "1")
	if code != 0 {
		t.Fatalf("README route set 示例退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr := run(t, "route", "show", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("route show 退出码 = %d，stderr = %s", code, stderr)
	}
	var policy map[string]any
	mustJSON(t, out, &policy)
	if policy["strategy"] != "priority" || policy["quota_order"] != "usd_desc" ||
		policy["quota_min_usd"].(float64) != 1 {
		t.Fatalf("README 示例写入的策略 = %+v", policy)
	}
}

func TestRouteQuotaPolicyAndExplain(t *testing.T) {
	sandbox(t)
	mustInit(t)
	up := newRecorderUpstream(t, quotaCLIRoutes())
	for _, name := range []string{"a1", "b1"} {
		code, _, stderr := addAccount(t, name, "commandcode", up.srv.URL, "", "--key", quotaCLIKey, "--models", "glm-5.2")
		if code != 0 {
			t.Fatalf("建账号 %s 退出码 = %d，stderr = %s", name, code, stderr)
		}
	}

	// 没有额度数据时，人读表格保持旧列，只多一行默认额度策略。
	code, out, stderr := run(t, "route", "explain", "glm-5.2")
	if code != 0 {
		t.Fatalf("无额度 explain 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "最终顺序") || !strings.Contains(out, "额度排序：off / 最低折合 USD：0.00") {
		t.Fatalf("无额度 explain 输出 = %s", out)
	}
	if strings.Contains(out, "剩余") || strings.Contains(out, "到期") {
		t.Fatalf("没有额度数据时不该出现额度列: %s", out)
	}

	code, out, stderr = run(t, "route", "set", "glm-5.2", "--strategy", "priority",
		"--sticky", "off", "--quota-order", "usd_desc", "--quota-min-usd", "1.5")
	if code != 0 {
		t.Fatalf("route set 额度参数退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "show", "glm-5.2")
	if code != 0 {
		t.Fatalf("route show 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "额度排序：usd_desc / 最低折合 USD：1.50") {
		t.Fatalf("route show 人读输出缺少额度设置: %s", out)
	}
	code, out, stderr = run(t, "route", "show", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("route show --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var policy map[string]any
	mustJSON(t, out, &policy)
	if policy["quota_order"] != "usd_desc" || policy["quota_min_usd"].(float64) != 1.5 {
		t.Fatalf("route show JSON 额度字段 = %+v", policy)
	}

	for _, args := range [][]string{
		{"route", "set", "glm-5.2", "--strategy", "priority", "--quota-order", "bogus"},
		{"route", "set", "glm-5.2", "--strategy", "priority", "--quota-min-usd", "-1"},
	} {
		if code, out, _ := run(t, args...); code != 2 || out != "" {
			t.Fatalf("%v 应退出 2 且不写 stdout，实际 %d / %q", args, code, out)
		}
	}

	// 刷新出快照后才能看到额度列；24.6301 / (24.6301+45.33) ≈ 35.2%。
	code, _, stderr = run(t, "quota", "refresh", "--all")
	if code != 0 {
		t.Fatalf("quota refresh --all 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "explain", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("带额度 explain 退出码 = %d，stderr = %s", code, stderr)
	}
	var explain struct {
		Policy     map[string]any `json:"policy"`
		Order      []string       `json:"order"`
		Candidates []struct {
			Account string         `json:"account"`
			Quota   map[string]any `json:"quota"`
		} `json:"candidates"`
	}
	mustJSON(t, out, &explain)
	if explain.Policy["quota_order"] != "usd_desc" || explain.Policy["quota_min_usd"].(float64) != 1.5 {
		t.Fatalf("explain policy 额度字段 = %+v", explain.Policy)
	}
	if len(explain.Candidates) != 2 || len(explain.Order) != 2 {
		t.Fatalf("explain 候选 = %+v", explain)
	}
	for _, c := range explain.Candidates {
		q := c.Quota
		if len(q) == 0 {
			t.Fatalf("候选 %s 缺少 quota 对象", c.Account)
		}
		if q["status"] != "ok" || q["currency"] != "USD" || q["expiry_mode"] != "warn" ||
			q["expiry_days"] != float64(3) {
			t.Fatalf("候选 %s 的 quota = %+v", c.Account, q)
		}
		if !quotaNearly(quotaNumber(t, q, "balance"), 24.6301) ||
			!quotaNearly(quotaNumber(t, q, "balance_usd"), 24.6301) ||
			!quotaNearly(quotaNumber(t, q, "remaining_ratio"), 24.6301/69.9601) {
			t.Fatalf("候选 %s 的金额 = %+v", c.Account, q)
		}
	}

	code, out, stderr = run(t, "route", "explain", "glm-5.2")
	if code != 0 {
		t.Fatalf("带额度 explain 人读退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"剩余", "到期", "24.6301 USD"} {
		if !strings.Contains(out, want) {
			t.Fatalf("带额度 explain 人读输出缺少 %q: %s", want, out)
		}
	}

	// 阈值高于两个账号的已知余额时，候选为空且原因是额度，不是冷却。
	code, _, stderr = run(t, "route", "set", "glm-5.2", "--strategy", "priority",
		"--sticky", "off", "--quota-min-usd", "30")
	if code != 0 {
		t.Fatalf("调高 min_usd 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "explain", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("过滤后 explain 退出码 = %d，stderr = %s", code, stderr)
	}
	var filtered struct {
		Candidates []any `json:"candidates"`
		Excluded   []struct {
			Account string `json:"account"`
			Reason  string `json:"reason"`
		} `json:"excluded"`
	}
	mustJSON(t, out, &filtered)
	if len(filtered.Candidates) != 0 {
		t.Fatalf("min_usd=30 应过滤掉两个账号: %+v", filtered.Candidates)
	}
	if len(filtered.Excluded) != 2 {
		t.Fatalf("排除列表 = %+v，期望两个账号", filtered.Excluded)
	}
	for _, ex := range filtered.Excluded {
		if !strings.HasPrefix(ex.Reason, "额度：") {
			t.Fatalf("账号 %s 的排除原因应标明额度: %q", ex.Account, ex.Reason)
		}
	}
}
