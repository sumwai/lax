package cli_test

import (
	"net/http"
	"strings"
	"testing"

	"lax/internal/store"
	"lax/internal/store/storetest"
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

// TestAccountLsModelsColumnNeverSaysUnlimited 盯住模型列在没有模型行时的文案：
// 没有一行表示该账号不提供任何模型、不参与选号（见 lessons/2026-10-29-empty-model-list.md），
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
	// 手填了名字的账号与同步过的账号一样走计数：--models 写的是目录行与一条 allowed，
	// 分子分母都从 account_models 来。
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "filled") && !strings.Contains(line, "2 可用 / 2 已登记") {
			t.Fatalf("手填了模型的账号应写「2 可用 / 2 已登记」: %q", line)
		}
	}

	// --json 与文本说的是同一件事：没有任何模型行 = models 为空且 models_synced 为 0。
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
		t.Fatalf("没有任何模型行的账号 = %+v，期望 models 为空、两个计数都是 0", b)
	}
	if f := byName["filled"]; len(f.Models) != 2 || f.ModelsSynced != 2 || f.ModelsUsable != 2 {
		t.Fatalf("手填模型名的账号 = %+v，期望 models 有两个名字、两个计数都是 2", f)
	}
}

// TestAccountLsCountsModelsByAccess 覆盖模型列的两个计数都来自 account_models：
// 分子数 access='allowed'，分母数全部行。零可用但不为零行时仍走计数分支，
// 「0 可用 / N 已登记」把「同步过但一个都不通」与「一行都没有」分开。
func TestAccountLsCountsModelsByAccess(t *testing.T) {
	sandbox(t)
	mustInit(t)
	addBareAccount(t, "mixed", "sk-1")
	addBareAccount(t, "none", "sk-2")

	db := storetest.Open(t)
	// mixed：一个能用、一个无权、一个不存在。
	mixed, err := db.GetAccount("mixed")
	if err != nil {
		t.Fatalf("读账号 mixed: %v", err)
	}
	for model, access := range map[string]string{
		"m1": store.AccessAllowed,
		"m2": store.AccessForbidden,
		"m3": store.AccessMissing,
	} {
		if err := db.SetAccountModelAccess(mixed.ID, model, access, 0, "", 1700000000); err != nil {
			t.Fatalf("写 %s 的可达性: %v", model, err)
		}
	}
	// none：同步过两行，但两行都不是 allowed。
	none, err := db.GetAccount("none")
	if err != nil {
		t.Fatalf("读账号 none: %v", err)
	}
	for _, model := range []string{"m1", "m2"} {
		if err := db.SetAccountModelAccess(none.ID, model, store.AccessForbidden, http.StatusForbidden, "", 1700000000); err != nil {
			t.Fatalf("写 %s 的可达性: %v", model, err)
		}
	}

	code, out, stderr := run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "1 可用 / 3 已登记") {
		t.Fatalf("mixed 的模型列应写「1 可用 / 3 已登记」: %s", out)
	}
	if !strings.Contains(out, "0 可用 / 2 已登记") {
		t.Fatalf("none 的模型列应写「0 可用 / 2 已登记」: %s", out)
	}
	if strings.Contains(out, "无模型") {
		t.Fatalf("有可达性行的账号不该落到「无模型」: %s", out)
	}

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
	if m := byName["mixed"]; m.ModelsUsable != 1 || m.ModelsSynced != 3 || len(m.Models) != 1 || m.Models[0] != "m1" {
		t.Fatalf("mixed = %+v，期望可用 1、总共 3、models 只有 m1", m)
	}
	if n := byName["none"]; n.ModelsUsable != 0 || n.ModelsSynced != 2 || len(n.Models) != 0 {
		t.Fatalf("none = %+v，期望可用 0、总共 2、models 为空", n)
	}
}

// TestModelLsShowsCatalogMetadata 覆盖 model ls 的目录元数据列：上下文长度、最大输出与
// 支持的协议集合都取自 provider_models（长度按「账号覆盖 → 目录」，见 DESIGN 5.13），
// 目录里没填的项在文本里写 -、在 --json 里是 null 或空数组。
func TestModelLsShowsCatalogMetadata(t *testing.T) {
	sandbox(t)
	mustInit(t)
	// m1 有完整元数据，m2 只有名字：两种取值都要显示得出来。
	if code, _, stderr := addAccount(t, "ds1", "custom", "http://127.0.0.1:9", "", "--key", "sk-test", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	contextWindow, maxOutput := int64(200000), int64(8192)
	db := storetest.Open(t)
	if err := db.UpsertProviderModels("custom", []store.ProviderModel{{
		Model:         "m1",
		Source:        store.SourceManual,
		ContextWindow: &contextWindow,
		MaxOutput:     &maxOutput,
		Protocols:     []string{"openai-chat", "anthropic"},
	}}); err != nil {
		t.Fatalf("写 m1 的目录行: %v", err)
	}

	code, out, stderr := run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Model         string   `json:"model"`
		ContextWindow *int64   `json:"context_window"`
		MaxOutput     *int64   `json:"max_output"`
		Protocols     []string `json:"protocols"`
	}
	mustJSON(t, out, &rows)
	byModel := make(map[string]int, len(rows))
	for i, r := range rows {
		byModel[r.Model] = i
	}
	i, ok := byModel["m1"]
	if !ok {
		t.Fatalf("model ls 缺 m1: %s", out)
	}
	got := rows[i]
	if got.ContextWindow == nil || *got.ContextWindow != contextWindow {
		t.Fatalf("m1 的 context_window = %v，期望 %d", got.ContextWindow, contextWindow)
	}
	if got.MaxOutput == nil || *got.MaxOutput != maxOutput {
		t.Fatalf("m1 的 max_output = %v，期望 %d", got.MaxOutput, maxOutput)
	}
	if len(got.Protocols) != 2 || got.Protocols[0] != "openai-chat" || got.Protocols[1] != "anthropic" {
		t.Fatalf("m1 的 protocols = %v，期望目录里的协议集合", got.Protocols)
	}
	j, ok := byModel["m2"]
	if !ok {
		t.Fatalf("model ls 缺 m2: %s", out)
	}
	two := rows[j]
	if two.ContextWindow != nil || two.MaxOutput != nil {
		t.Fatalf("m2 的元数据 = %v / %v，期望都是 null（目录没填）", two.ContextWindow, two.MaxOutput)
	}
	if two.Protocols == nil || len(two.Protocols) != 0 {
		t.Fatalf("m2 的 protocols = %v，期望空数组而不是 null", two.Protocols)
	}

	code, out, stderr = run(t, "model", "ls")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"上下文长度", "最大输出", "支持协议", "200000", "8192", "openai-chat,anthropic"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model ls 文本输出缺 %q: %s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "m2") && !strings.Contains(line, " - ") {
			t.Fatalf("目录里没填的元数据应写 -: %q", line)
		}
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
	for _, want := range []string{"lax model sync", "lax model probe"} {
		if !strings.Contains(out, want) {
			t.Fatalf("空清单的提示应给出下一步 %q: %s", want, out)
		}
	}
	if strings.Contains(out, "account probe") {
		t.Fatalf("提示里不该出现不存在的 account probe: %s", out)
	}
}
