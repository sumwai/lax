package cli_test

import (
	"strings"
	"testing"
	"time"

	"switchboard/internal/store"
)

// 这个文件钉住思考 token 的两处展示：`log tail` 逐行一列、`usage` 三种分组各一列，
// 文本与 `--json` 都带（DESIGN 第 4 节的列说明、5.5 的计价口径）。
// 思考 token 与输出 token 分开显示：上游可能把它报成输出之外的一项，并进输出会看不出账。

// TestLogTailShowsReasoningTokens 覆盖 log tail 的「思考」列：文本表格在输出之后多一列，
// `--json` 多出 reasoning_tokens，有值按原数显示，0 显示 0（与输入输出的写法一致）。
func TestLogTailShowsReasoningTokens(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "relay", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-secret-reasoning", "--models", "m-think,m-plain"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "relay")

	// 中转上报的形状：prompt 7、completion 1、total 132、reasoning 124。
	addRecord(t, store.RequestRecord{
		TS: time.Now().Add(-time.Minute).Unix(), Inbound: "openai-chat", Outbound: "openai-chat",
		ModelRequested: "m-think", ModelUsed: "m-think", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 7, InTokensUncached: 7, OutTokens: 1,
		ReasoningTokens: 124, ReasoningTokensExtra: 124, LatencyMS: 500,
	})
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m-plain", ModelUsed: "m-plain", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 40, InTokensUncached: 40, OutTokens: 5, LatencyMS: 120,
	})

	code, out, stderr := run(t, "log", "tail", "-n", "5")
	if code != 0 {
		t.Fatalf("log tail 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"思考", "输出", "缓存读"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log tail 的表头缺 %q:\n%s", want, out)
		}
	}
	// 时间占两列，后面依次是协议、模型、账号、状态、输入、输出、思考。
	think := logRowFields(t, out, "m-think")
	if len(think) < 12 {
		t.Fatalf("带思考的记录列数 = %d，少于 12 列：%v", len(think), think)
	}
	if think[7] != "1" || think[8] != "124" {
		t.Fatalf("带思考的记录输出与思考列 = %q / %q，期望 1 / 124：%v", think[7], think[8], think)
	}
	plain := logRowFields(t, out, "m-plain")
	if plain[8] != "0" {
		t.Fatalf("没有思考的记录思考列 = %q，期望显示 0：%v", plain[8], plain)
	}

	rows := mustLogRows(t)
	if got := rows["m-think"]; got.ReasoningTokens != 124 {
		t.Fatalf("--json 的 m-think reasoning_tokens = %d，期望 124", got.ReasoningTokens)
	}
	if got := rows["m-plain"]; got.ReasoningTokens != 0 {
		t.Fatalf("--json 的 m-plain reasoning_tokens = %d，期望 0", got.ReasoningTokens)
	}

	// 这两条展示路径都不该带出凭据明文：思考 token 是新加的一列，断言一起再走一遍。
	if strings.Contains(out, "sk-secret-reasoning") {
		t.Fatalf("log tail 的文本输出泄漏了凭据:\n%s", out)
	}
	code, jsonOut, stderr := run(t, "log", "tail", "--json")
	if code != 0 {
		t.Fatalf("log tail --json 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(jsonOut, "sk-secret-reasoning") {
		t.Fatalf("log tail --json 的输出泄漏了凭据:\n%s", jsonOut)
	}
}

// TestUsageReasoningTotalsPerDimension 覆盖 usage 三种分组下的思考合计：
// 每个分组都有 reasoning_tokens，文本表格有「思考 token」这一列与合计行。
func TestUsageReasoningTotalsPerDimension(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "relay", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-secret-usage", "--models", "m-a,m-b"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	// 单价在模型目录上：两个模型各定一份，思考 token 按输出价计（见 DESIGN 5.5）。
	for _, model := range []string{"m-a", "m-b"} {
		if code, _, stderr := run(t, "model", "price", "set", model, "--provider", "mine", "--in", "2", "--out", "8"); code != 0 {
			t.Fatalf("model price set %s 退出码 = %d，stderr = %s", model, code, stderr)
		}
	}
	id := accountID(t, "relay")

	addRecord(t, store.RequestRecord{
		TS: time.Now().Add(-time.Hour).Unix(), Inbound: "openai-chat", Outbound: "openai-chat",
		ModelRequested: "m-a", ModelUsed: "m-a", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 7, InTokensUncached: 7, OutTokens: 1,
		ReasoningTokens: 124, ReasoningTokensExtra: 124, LatencyMS: 300,
	})
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "openai-chat", Outbound: "openai-chat",
		ModelRequested: "m-b", ModelUsed: "m-b", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 7, InTokensUncached: 7, OutTokens: 1,
		ReasoningTokens: 40, ReasoningTokensExtra: 40, LatencyMS: 200,
	})

	// 按账号：一行，两条记录的思考合计 164。
	byAccount := mustUsageRows(t, "--by", "account", "--since", "1d")
	if got := byAccount["relay"]; got.ReasoningTokens != 164 {
		t.Fatalf("按账号的思考合计 = %d，期望 164（124 + 40）", got.ReasoningTokens)
	}
	// 花费 = (7 × 2 + 1 × 8 + 124 × 8) / 1e6 + (7 × 2 + 1 × 8 + 40 × 8) / 1e6
	if got := byAccount["relay"]; got.Cost <= 0 {
		t.Fatalf("定了价又有思考 token 时花费应当大于 0，实际 %v", got.Cost)
	}

	// 按模型：每个模型各一行，各自的思考数。
	byModel := mustUsageRows(t, "--by", "model", "--since", "1d")
	if got := byModel["m-a"]; got.ReasoningTokens != 124 {
		t.Fatalf("按模型的 m-a 思考 = %d，期望 124", got.ReasoningTokens)
	}
	if got := byModel["m-b"]; got.ReasoningTokens != 40 {
		t.Fatalf("按模型的 m-b 思考 = %d，期望 40", got.ReasoningTokens)
	}

	// 按日期：两条记录同一天，合成一行，合计与按账号一致。
	byDay := mustUsageRows(t, "--by", "day", "--since", "1d")
	if len(byDay) != 1 {
		t.Fatalf("按日期应有 1 行，实际 %d 行：%+v", len(byDay), byDay)
	}
	for _, row := range byDay {
		if row.ReasoningTokens != 164 {
			t.Fatalf("按日期的思考合计 = %d，期望 164", row.ReasoningTokens)
		}
	}

	// 文本表格：「思考 token」一列与合计行都要在。
	code, out, stderr := run(t, "usage", "--by", "account", "--since", "1d")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"输入 token", "输出 token", "思考 token", "缓存读 token", "估算花费", "合计"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage 表格缺 %q:\n%s", want, out)
		}
	}
	total := tableRowFields(t, out, "合计")
	if len(total) < 8 {
		t.Fatalf("合计行列数 = %d，少于 8 列：%v", len(total), total)
	}
	if total[4] != "164" {
		t.Fatalf("合计行的思考列 = %q，期望 164：%v", total[4], total)
	}

	code, jsonOut, stderr := run(t, "usage", "--by", "account", "--since", "1d", "--json")
	if code != 0 {
		t.Fatalf("usage --json 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(jsonOut, "sk-secret-usage") {
		t.Fatalf("usage --json 的输出泄漏了凭据:\n%s", jsonOut)
	}
}
