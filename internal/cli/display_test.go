package cli_test

import (
	"strings"
	"testing"

	"switchboard/internal/store/storetest"
)

// TestModelLsShowsEffectiveProtocol 覆盖 model ls 的协议列：
// 每个账号显示该模型的有效协议，模型行没有协议时回退到账号协议。
func TestModelLsShowsEffectiveProtocol(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "acc1",
		"--provider", "mine", "--base-url", "http://127.0.0.1:9", "--key", "sk-test",
		"--protocol", "openai-chat", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	db := storetest.Open(t)
	acc, err := db.GetAccount("acc1")
	if err != nil {
		t.Fatalf("读账号 acc1: %v", err)
	}
	// m1 探活时发现该走 anthropic；m2 的协议列为空，应当回退到账号协议。
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
		t.Fatalf("m2 的有效协议 = %q，期望回退到账号协议 openai-chat", byModel["m2"])
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
	if code, _, stderr := run(t, "account", "add", "mixed",
		"--provider", "mine", "--base-url", "http://127.0.0.1:9", "--key", "sk-1",
		"--protocol", "openai-chat", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "solo",
		"--provider", "mine", "--base-url", "http://127.0.0.1:9", "--key", "sk-2",
		"--protocol", "openai-chat", "--models", "m1"); code != 0 {
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
