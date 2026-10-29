package cli_test

import (
	"strings"
	"testing"

	"switchboard/internal/store/storetest"
)

// TestModelLsShowsEffectiveProtocol 覆盖 model ls 的协议列：
// 每个账号显示该模型的有效协议，模型行没有协议时回退到厂商的有效默认协议（见 DESIGN 5.8）。
func TestModelLsShowsEffectiveProtocol(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "acc1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-test", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	db := storetest.Open(t)
	acc, err := db.GetAccount("acc1")
	if err != nil {
		t.Fatalf("读账号 acc1: %v", err)
	}
	// m1 探活时发现该走 anthropic；m2 的协议列为空，应当回退到厂商的默认协议。
	if err := db.SetModelStatus(acc.ID, "m1", "usable", 200, ""); err != nil {
		t.Fatalf("写 m1 状态: %v", err)
	}
	if err := db.SetModelProtocol(acc.ID, "m1", "anthropic"); err != nil {
		t.Fatalf("写 m1 协议: %v", err)
	}
	if err := db.SetModelStatus(acc.ID, "m2", "usable", 200, ""); err != nil {
		t.Fatalf("写 m2 状态: %v", err)
	}

	code, out, stderr := run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Model    string `json:"model"`
		Accounts []struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			Protocol string `json:"protocol"`
		} `json:"accounts"`
	}
	mustJSON(t, out, &rows)
	if len(rows) != 2 {
		t.Fatalf("model ls 应有 2 个模型，实际 %d 个: %s", len(rows), out)
	}
	byModel := map[string]string{}
	for _, r := range rows {
		if len(r.Accounts) != 1 {
			t.Fatalf("模型 %s 下应有 1 个账号，实际 %d 个: %s", r.Model, len(r.Accounts), out)
		}
		byModel[r.Model] = r.Accounts[0].Protocol
	}
	if byModel["m1"] != "anthropic" {
		t.Fatalf("m1 的有效协议 = %q，期望探活发现的 anthropic", byModel["m1"])
	}
	if byModel["m2"] != "openai-chat" {
		t.Fatalf("m2 的有效协议 = %q，期望回退到厂商默认协议 openai-chat", byModel["m2"])
	}

	code, out, stderr = run(t, "model", "ls")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"协议", "anthropic", "openai-chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model ls 文本输出缺 %q: %s", want, out)
		}
	}
}

// TestAccountLsProtocolSummary 覆盖 account ls 的协议列：
// 默认协议保持原样，存在别的已发现协议时标出「另 N 种」。
func TestAccountLsProtocolSummary(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	// mixed 有两个模型，其中一个探活发现该走 anthropic；solo 只有一个协议。
	if code, _, stderr := addAccount(t, "mixed", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "solo", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-2", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	db := storetest.Open(t)
	acc, err := db.GetAccount("mixed")
	if err != nil {
		t.Fatalf("读账号 mixed: %v", err)
	}
	if err := db.SetModelStatus(acc.ID, "m1", "usable", 200, ""); err != nil {
		t.Fatalf("写 m1 状态: %v", err)
	}
	if err := db.SetModelProtocol(acc.ID, "m1", "anthropic"); err != nil {
		t.Fatalf("写 m1 协议: %v", err)
	}
	if err := db.SetModelStatus(acc.ID, "m2", "synced", 0, ""); err != nil {
		t.Fatalf("写 m2 状态: %v", err)
	}

	code, out, stderr := run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Name      string   `json:"name"`
		Protocol  string   `json:"protocol"`
		Protocols []string `json:"protocols"`
	}
	mustJSON(t, out, &rows)
	byName := make(map[string][]string, len(rows))
	defaults := make(map[string]string, len(rows))
	for _, r := range rows {
		byName[r.Name], defaults[r.Name] = r.Protocols, r.Protocol
	}
	if defaults["mixed"] != "openai-chat" {
		t.Fatalf("mixed 的默认协议 = %q，应保持账号自己的协议", defaults["mixed"])
	}
	if got := byName["mixed"]; len(got) != 2 || got[0] != "openai-chat" || got[1] != "anthropic" {
		t.Fatalf("mixed 的协议 = %+v，期望默认协议在前、其中另有 anthropic", got)
	}
	if got := byName["solo"]; len(got) != 1 || got[0] != "openai-chat" {
		t.Fatalf("solo 的协议 = %+v，只有一个协议时不该多出别的", got)
	}

	code, out, stderr = run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "openai-chat + 另 1 种") {
		t.Fatalf("存在别的已发现协议时应标出「另 1 种」: %s", out)
	}
	// 只有一个协议的账号不该被动过。
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "solo") && strings.Contains(line, "另") {
			t.Fatalf("solo 的协议列 = %q，不该标「另 N 种」", line)
		}
	}
}

// addBareAccount 加一个没有任何模型信息的账号：不给 --models，也没有 account_models 行。
func addBareAccount(t *testing.T, name, key string) {
	t.Helper()
	if code, _, stderr := addAccount(t, name, "mine", "http://127.0.0.1:9", "openai-chat", "--key", key); code != 0 {
		t.Fatalf("account add %s 退出码 = %d，stderr = %s", name, code, stderr)
	}
}

// TestAccountLsModelsColumnNeverSaysUnlimited 盯住模型列在空模型列表时的文案：
// 空列表表示该账号不提供任何模型、不参与选号（见 lessons/2026-10-29-empty-model-list.md），
// 所以文案不能写成看起来表示「什么模型都收」的样子；--json 用同一份事实表达同一个意思。
func TestAccountLsModelsColumnNeverSaysUnlimited(t *testing.T) {
	sandbox(t)
	mustInit(t)
	addBareAccount(t, "bare", "sk-1")
	if code, _, stderr := addAccount(t, "filled", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-2", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add filled 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(out, "不限") {
		t.Fatalf("空模型列表不该写成「不限」: %s", out)
	}
	if !strings.Contains(out, "无模型（先 model sync）") {
		t.Fatalf("没有任何模型信息时应写明下一步: %s", out)
	}
	// 手填了名字的账号照旧直接列出模型名，文案不变。
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "filled") && !strings.Contains(line, "m1 等 2 个") {
			t.Fatalf("手填了模型的账号应列出模型名: %q", line)
		}
	}

	// --json 与文本说的是同一件事：没有任何模型信息 = models 为空且 models_synced 为 0。
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []accountRow
	mustJSON(t, out, &rows)
	byName := make(map[string]accountRow, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	if b := byName["bare"]; len(b.Models) != 0 || b.ModelsSynced != 0 || b.ModelsUsable != 0 {
		t.Fatalf("没有任何模型信息的账号 = %+v，期望 models 为空、两个计数都是 0", b)
	}
	if f := byName["filled"]; len(f.Models) != 2 || f.ModelsSynced != 0 || f.ModelsUsable != 0 {
		t.Fatalf("手填模型名的账号 = %+v，期望 models 有两个名字、两个计数都是 0", f)
	}
}

// TestAccountLsSyncedZeroUsable 覆盖「同步过但零可用」：模型列走「0 可用 / N 同步」，
// 不落到「无模型」，--json 的 models_synced 大于 0 能把这种情况与没同步过分开。
func TestAccountLsSyncedZeroUsable(t *testing.T) {
	sandbox(t)
	mustInit(t)
	addBareAccount(t, "ds1", "sk-1")

	db := storetest.Open(t)
	acc, err := db.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号 ds1: %v", err)
	}
	// model sync 写进来的是 synced：列表拉到了，但一个都还没探过。
	for _, m := range []string{"m1", "m2"} {
		if err := db.SetModelStatus(acc.ID, m, "synced", 0, ""); err != nil {
			t.Fatalf("写 %s 状态: %v", m, err)
		}
	}

	code, out, stderr := run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(out, "不限") || strings.Contains(out, "无模型") {
		t.Fatalf("同步过的账号应写「0 可用 / 2 同步」: %s", out)
	}
	if !strings.Contains(out, "0 可用 / 2 同步") {
		t.Fatalf("同步过但零可用时应写「0 可用 / 2 同步」: %s", out)
	}

	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	row := mustAccountRow(t, out)
	if len(row.Models) != 0 || row.ModelsSynced != 2 || row.ModelsUsable != 0 {
		t.Fatalf("同步过但零可用的账号 = %+v，期望 models 为空、models_synced 2、models_usable 0", row)
	}
}

// TestModelLsEmptyHintNamesRunnableCommands 盯住 model ls 空清单时的下一步提示：
// 提示里的命令必须真的存在，并且指向把模型列表拉进来的两步（sync 与 probe）。
func TestModelLsEmptyHintNamesRunnableCommands(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "model", "ls")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"switchboard model sync", "switchboard model probe"} {
		if !strings.Contains(out, want) {
			t.Fatalf("空清单的提示应给出下一步 %q: %s", want, out)
		}
	}
	if strings.Contains(out, "account probe") {
		t.Fatalf("提示里不该出现不存在的 account probe: %s", out)
	}
}
