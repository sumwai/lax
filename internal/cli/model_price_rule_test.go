package cli_test

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/store"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestModelPriceTierCommands(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 1. tier set 添加两档（乱序添加也应按 max_tokens 升序，inf 在最后）
	code, out, stderr := run(t, "model", "price", "tier", "set", "m1", "--max", "inf", "--in", "2.0", "--out", "8.0")
	if code != 0 {
		t.Fatalf("tier set inf 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "已写入阶梯档位") || !strings.Contains(out, "inf") {
		t.Fatalf("tier set 输出不符合预期:\n%s", out)
	}

	code, out, stderr = run(t, "model", "price", "tier", "set", "m1", "--max", "32000", "--in", "1.0", "--out", "4.0", "--json")
	if code != 0 {
		t.Fatalf("tier set 32000 退出码 = %d，stderr = %s", code, stderr)
	}
	var setResp struct {
		Model    string             `json:"model"`
		Provider string             `json:"provider"`
		Rule     *store.PricingRule `json:"rule"`
	}
	mustJSON(t, out, &setResp)
	if setResp.Rule == nil || setResp.Rule.Tiers == nil || len(setResp.Rule.Tiers.Brackets) != 2 {
		t.Fatalf("tier set --json 应返回 2 个档位: %+v", setResp)
	}
	if setResp.Rule.Tiers.Brackets[0].MaxTokens == nil || *setResp.Rule.Tiers.Brackets[0].MaxTokens != 32000 {
		t.Fatalf("第一档应为 32000，实际 %+v", setResp.Rule.Tiers.Brackets[0])
	}
	if setResp.Rule.Tiers.Brackets[1].MaxTokens != nil {
		t.Fatalf("第二档应为 inf (nil)，实际 %+v", setResp.Rule.Tiers.Brackets[1])
	}

	// 2. tier config 修改 mode 与 basis
	code, out, stderr = run(t, "model", "price", "tier", "config", "m1", "--mode", "progressive", "--basis", "uncached_input", "--json")
	if code != 0 {
		t.Fatalf("tier config 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &setResp)
	if setResp.Rule.Tiers.Mode != store.TierModeProgressive || setResp.Rule.Tiers.Basis != store.TierBasisUncachedInput {
		t.Fatalf("tier config 未生效: %+v", setResp.Rule.Tiers)
	}

	// 3. tier unset 删除一档
	code, out, stderr = run(t, "model", "price", "tier", "unset", "m1", "--max", "32000")
	if code != 0 {
		t.Fatalf("tier unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "已删除阶梯档位") {
		t.Fatalf("tier unset 输出不符合预期:\n%s", out)
	}
	if pm := providerPrice(t, "mine", "m1"); pm.PricingRule == nil || pm.PricingRule.Tiers == nil || len(pm.PricingRule.Tiers.Brackets) != 1 {
		t.Fatalf("tier unset 后应剩 1 档: %+v", pm.PricingRule)
	}

	// 4. tier clear 清空全部档位（无 schedule 时应把 pricing_rule 置空）
	code, _, stderr = run(t, "model", "price", "tier", "clear", "m1", "--json")
	if code != 0 {
		t.Fatalf("tier clear 退出码 = %d，stderr = %s", code, stderr)
	}
	if pm := providerPrice(t, "mine", "m1"); pm.PricingRule != nil {
		t.Fatalf("tier clear 后 pricing_rule 应为空: %+v", pm.PricingRule)
	}

	// 5. 错误参数校验
	if code, _, _ := run(t, "model", "price", "tier", "set", "m1", "--max", "0", "--in", "1"); code != 2 {
		t.Fatalf("--max 0 应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "tier", "set", "m1", "--max", "1000"); code != 2 {
		t.Fatalf("未给单价应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "tier", "config", "m1", "--mode", "bad"); code != 2 {
		t.Fatalf("非法 mode 应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "tier", "config", "m1", "--mode", "all_in"); code != 1 {
		t.Fatalf("无档位时 tier config 应返回退出码 1，实际 %d", code)
	}
}

func TestModelPriceWindowCommands(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "price", "window", "set", "m1",
		"--name", "off-peak", "--tz", "Asia/Shanghai", "--start", "00:00", "--end", "08:00",
		"--days", "sat,sun", "--multiplier", "0.5", "--json")
	if code != 0 {
		t.Fatalf("window set 退出码 = %d，stderr = %s", code, stderr)
	}
	var resp struct {
		Rule *store.PricingRule `json:"rule"`
	}
	mustJSON(t, out, &resp)
	if resp.Rule == nil || resp.Rule.Schedule == nil || len(resp.Rule.Schedule.Windows) != 1 {
		t.Fatalf("window set --json 应返回 1 个窗口: %+v", resp)
	}
	w := resp.Rule.Schedule.Windows[0]
	if w.Name != "off-peak" || resp.Rule.Schedule.Timezone != "Asia/Shanghai" || len(w.Days) != 2 || w.Days[0] != 0 || w.Days[1] != 6 {
		t.Fatalf("window 内容不符合预期: %+v", resp.Rule.Schedule)
	}

	// 同名更新
	code, _, stderr = run(t, "model", "price", "window", "set", "m1",
		"--name", "off-peak", "--start", "01:00", "--end", "07:00", "--in", "0.3")
	if code != 0 {
		t.Fatalf("window set 更新退出码 = %d，stderr = %s", code, stderr)
	}
	pm := providerPrice(t, "mine", "m1")
	if len(pm.PricingRule.Schedule.Windows) != 1 || pm.PricingRule.Schedule.Windows[0].Start != "01:00" {
		t.Fatalf("window 同名更新应覆盖原窗口: %+v", pm.PricingRule.Schedule)
	}

	// window unset
	code, _, stderr = run(t, "model", "price", "window", "unset", "m1", "--name", "off-peak")
	if code != 0 {
		t.Fatalf("window unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if pm := providerPrice(t, "mine", "m1"); pm.PricingRule != nil {
		t.Fatalf("删除最后一个 window 后 pricing_rule 应为空: %+v", pm.PricingRule)
	}

	// window unset 不存在时报错
	if code, _, _ := run(t, "model", "price", "window", "unset", "m1", "--name", "nope"); code != 1 {
		t.Fatalf("删除不存在窗口应返回退出码 1，实际 %d", code)
	}

	// 错误参数校验
	if code, _, _ := run(t, "model", "price", "window", "set", "m1", "--name", "x", "--start", "00:00", "--end", "08:00"); code != 2 {
		t.Fatalf("未给 multiplier/override 应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "window", "set", "m1", "--name", "x", "--start", "00:00", "--end", "08:00", "--days", "funday", "--multiplier", "0.5"); code != 2 {
		t.Fatalf("非法 days 应返回退出码 2，实际 %d", code)
	}
}

func TestModelPriceRuleShowSetUnsetAndLsSummary(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--in", "1.0", "--out", "2.0"); code != 0 {
		t.Fatalf("price set 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m2", "--in", "0.5", "--out", "1.5"); code != 0 {
		t.Fatalf("price set m2 退出码 = %d，stderr = %s", code, stderr)
	}

	// 1. rule set via --rule-json
	ruleJSON := `{"version":1,"tiers":{"mode":"all_in","basis":"total_input","brackets":[{"max_tokens":32000,"prices":{"in":1.0,"out":2.0}},{"prices":{"in":2.0,"out":4.0}}]},"schedule":{"timezone":"UTC","windows":[{"name":"off-peak","start":"00:00","end":"08:00","multiplier":0.5}]}}`
	code, out, stderr := run(t, "model", "price", "rule", "set", "m1", "--rule-json", ruleJSON)
	if code != 0 {
		t.Fatalf("rule set --rule-json 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "2 tiers(all_in), 1 window(off-peak 0.5x)") {
		t.Fatalf("rule set 摘要输出不符合预期:\n%s", out)
	}

	// 2. model price ls 表格应包含 RULES 列和规则摘要
	code, out, stderr = run(t, "model", "price", "ls")
	if code != 0 {
		t.Fatalf("model price ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "RULES") {
		t.Fatalf("model price ls 表头应包含 RULES:\n%s", out)
	}
	m1Line := priceLine(t, out, "m1")
	if !strings.Contains(m1Line, "2 tiers(all_in), 1 window(off-peak 0.5x)") {
		t.Fatalf("m1 行应包含 RULES 摘要: %q", m1Line)
	}
	m2Line := priceLine(t, out, "m2")
	if !strings.HasSuffix(strings.TrimSpace(m2Line), "-") {
		t.Fatalf("m2 无规则时 RULES 列应为 -: %q", m2Line)
	}

	// --json 应包含 pricing_rule
	code, out, stderr = run(t, "model", "price", "ls", "m1", "--json")
	if code != 0 {
		t.Fatalf("model price ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"pricing_rule"`) {
		t.Fatalf("ls --json 应包含 pricing_rule:\n%s", out)
	}

	// 3. rule set via --file <path> & --file - (stdin) on account override
	tmpFile := filepath.Join(t.TempDir(), "rule.json")
	accRule := `{"version":1,"schedule":{"timezone":"UTC","windows":[{"name":"night","start":"00:00","end":"06:00","multiplier":0.25}]}}`
	if err := os.WriteFile(tmpFile, []byte(accRule), 0o600); err != nil {
		t.Fatalf("写临时规则文件: %v", err)
	}
	code, _, stderr = run(t, "model", "price", "rule", "set", "m1", "--account", "ds1", "--file", tmpFile)
	if code != 0 {
		t.Fatalf("rule set --file 退出码 = %d，stderr = %s", code, stderr)
	}

	// rule show --account 应看到账号级覆盖规则
	code, out, stderr = run(t, "model", "price", "rule", "show", "m1", "--account", "ds1")
	if code != 0 {
		t.Fatalf("rule show 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "night") || !strings.Contains(out, "0.25x") || !strings.Contains(out, "(account)") {
		t.Fatalf("rule show 应展示账号级规则与来源:\n%s", out)
	}

	// rule unset --account 后应回退到厂商级规则
	code, _, stderr = run(t, "model", "price", "rule", "unset", "m1", "--account", "ds1")
	if code != 0 {
		t.Fatalf("rule unset --account 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "model", "price", "rule", "show", "m1", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("rule show --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var showResp struct {
		RuleSource string             `json:"rule_source"`
		Rule       *store.PricingRule `json:"rule"`
	}
	mustJSON(t, out, &showResp)
	if showResp.RuleSource != "provider" || showResp.Rule == nil || showResp.Rule.Tiers == nil {
		t.Fatalf("rule unset 后应回退到 provider 规则: %+v", showResp)
	}

	// --file - 从 stdin 读取
	code, _, stderr = runWithStdin(t, strings.NewReader(accRule), "model", "price", "rule", "set", "m2", "--file", "-")
	if code != 0 {
		t.Fatalf("rule set --file - 退出码 = %d，stderr = %s", code, stderr)
	}
	if pm := providerPrice(t, "mine", "m2"); pm.PricingRule == nil || pm.PricingRule.Schedule == nil {
		t.Fatalf("从 stdin 导入规则未落库: %+v", pm.PricingRule)
	}

	// 错误输入校验
	if code, _, _ := run(t, "model", "price", "rule", "set", "m1"); code != 2 {
		t.Fatalf("既不给 --file 也不给 --rule-json 应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "rule", "set", "m1", "--file", ""); code != 2 {
		t.Fatalf("--file 空路径应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "rule", "set", "m1", "--rule-json", "{bad"); code != 2 {
		t.Fatalf("非法 JSON 应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "rule", "set", "m1", "--rule-json", `{}`); code != 2 {
		t.Fatalf("空规则 {} 应返回退出码 2，实际 %d", code)
	}
	// 空规则 {} 不应清空已有规则
	if pm := providerPrice(t, "mine", "m1"); pm.PricingRule == nil {
		t.Fatalf("空规则 {} 报错后不应清空原有规则")
	}
	if code, _, _ := run(t, "model", "price", "rule", "set", "m1", "--rule-json", `{"tiers":{"brackets":[]}}`); code != 2 {
		t.Fatalf("非法规则结构应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "window", "set", "m1", "--name", "badtz", "--tz", "Invalid/Zone", "--start", "00:00", "--end", "08:00", "--multiplier", "0.5"); code != 2 {
		t.Fatalf("非法时区应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "window", "set", "m1", "--name", "same", "--start", "08:00", "--end", "08:00", "--multiplier", "0.5"); code != 2 {
		t.Fatalf("起止时间相同应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "window", "set", "m1", "--name", "neg", "--start", "00:00", "--end", "08:00", "--multiplier", "-0.5"); code != 2 {
		t.Fatalf("负倍率应返回退出码 2，实际 %d", code)
	}
}

func TestModelPriceTestCommand(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--in", "1.0", "--out", "2.0"); code != 0 {
		t.Fatalf("price set 退出码 = %d，stderr = %s", code, stderr)
	}
	// 阶梯（progressive）：<=10000 按 $1.0，>10000 按 $2.0；分时：UTC 00:00-08:00 0.5x
	if code, _, stderr := run(t, "model", "price", "tier", "set", "m1", "--max", "10000", "--in", "1.0", "--out", "2.0"); code != 0 {
		t.Fatalf("tier set 10000: %s", stderr)
	}
	if code, _, stderr := run(t, "model", "price", "tier", "set", "m1", "--max", "inf", "--in", "2.0", "--out", "4.0"); code != 0 {
		t.Fatalf("tier set inf: %s", stderr)
	}
	if code, _, stderr := run(t, "model", "price", "tier", "config", "m1", "--mode", "progressive"); code != 0 {
		t.Fatalf("tier config: %s", stderr)
	}
	if code, _, stderr := run(t, "model", "price", "window", "set", "m1", "--name", "off-peak", "--tz", "UTC", "--start", "00:00", "--end", "08:00", "--multiplier", "0.5"); code != 0 {
		t.Fatalf("window set: %s", stderr)
	}

	// 命中 off-peak (02:00 UTC)，in=15000 (10000*1.0 + 5000*2.0)/1e6 * 0.5 = 0.01，out=1000*4.0/1e6 * 0.5 = 0.002 => total 0.012
	code, out, stderr := run(t, "model", "price", "test", "m1",
		"--in", "15000", "--out", "1000", "--at", "2026-11-20T02:00:00Z", "--json")
	if code != 0 {
		t.Fatalf("model price test --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var resp struct {
		Model     string               `json:"model"`
		Provider  string               `json:"provider"`
		Breakdown *store.CostBreakdown `json:"breakdown"`
	}
	mustJSON(t, out, &resp)
	if resp.Breakdown == nil || resp.Breakdown.TotalCost == nil {
		t.Fatalf("breakdown 不应为空: %+v", resp)
	}
	if !almostEqual(*resp.Breakdown.TotalCost, 0.012) {
		t.Fatalf("TotalCost = %v，期望 0.012", *resp.Breakdown.TotalCost)
	}
	if resp.Breakdown.WindowName != "off-peak" {
		t.Fatalf("WindowName = %q，期望 off-peak", resp.Breakdown.WindowName)
	}

	// 文本输出校验
	code, out, stderr = run(t, "model", "price", "test", "m1",
		"--in", "15000", "--out", "1000", "--at", "2026-11-20T02:00:00Z")
	if code != 0 {
		t.Fatalf("model price test 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"费用试算", "off-peak", "0.5x", "$0.012000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("试算文本输出应包含 %q:\n%s", want, out)
		}
	}

	// 支持 YYYY-MM-DD HH:MM 格式
	if code, _, stderr := run(t, "model", "price", "test", "m1", "--in", "1000", "--at", "2026-11-20 12:00"); code != 0 {
		t.Fatalf("YYYY-MM-DD HH:MM 应合法: %s", stderr)
	}

	// 错误输入校验
	if code, _, _ := run(t, "model", "price", "test", "m1", "--in", "-1"); code != 2 {
		t.Fatalf("负数 token 应返回退出码 2，实际 %d", code)
	}
	if code, _, _ := run(t, "model", "price", "test", "m1", "--at", "not-a-time"); code != 2 {
		t.Fatalf("非法 --at 应返回退出码 2，实际 %d", code)
	}
}

func TestModelPriceLsMultiProviderAccountIsolation(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add ds1 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds2", "other", "http://127.0.0.1:9", "openai-chat", "--key", "sk-2", "--models", "m1"); code != 0 {
		t.Fatalf("account add ds2 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine", "--in", "1.0", "--out", "2.0"); code != 0 {
		t.Fatalf("set mine 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "other", "--in", "5.0", "--out", "6.0"); code != 0 {
		t.Fatalf("set other 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--account", "ds1", "--in", "0.5"); code != 0 {
		t.Fatalf("set ds1 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "window", "set", "m1", "--account", "ds1", "--name", "off", "--start", "00:00", "--end", "08:00", "--multiplier", "0.5"); code != 0 {
		t.Fatalf("window set ds1 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "price", "ls", "m1", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("ls --account ds1 --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Model     string `json:"model"`
		Provider  string `json:"provider"`
		Effective *struct {
			PriceIn     *float64           `json:"price_in"`
			PricingRule *store.PricingRule `json:"pricing_rule"`
		} `json:"effective"`
	}
	mustJSON(t, out, &rows)
	if len(rows) != 2 {
		t.Fatalf("期望 2 行，实际 %d 行: %s", len(rows), out)
	}
	for _, r := range rows {
		switch r.Provider {
		case "mine":
			if r.Effective == nil || r.Effective.PriceIn == nil || *r.Effective.PriceIn != 0.5 || r.Effective.PricingRule == nil {
				t.Fatalf("mine 行应命中 ds1 的有效价与规则，实际 %+v", r.Effective)
			}
		case "other":
			if r.Effective != nil && (r.Effective.PriceIn != nil || r.Effective.PricingRule != nil) {
				t.Fatalf("other 行不应错配 ds1 的有效价或规则，实际 %+v", r.Effective)
			}
		default:
			t.Fatalf("未知 provider %q", r.Provider)
		}
	}
}

