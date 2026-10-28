package cli_test

import (
	"strings"
	"testing"
)

// accountAddJSON 是 account add --json 的输出里用例关心的字段。
type accountAddJSON struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Protocol string `json:"protocol"`
	BaseURL  string `json:"base_url"`
}

func mustAccountAdd(t *testing.T, out string) accountAddJSON {
	t.Helper()
	var got accountAddJSON
	mustJSON(t, out, &got)
	return got
}

// TestAccountAddUsesProviderTable 覆盖 account add 从厂商表补默认地址与默认协议，
// 以及 --base-url 与 --protocol 各自能覆盖它们。
func TestAccountAddUsesProviderTable(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	// 默认协议取该厂商按协议名排序的第一条路由：deepseek 的 anthropic 排在 openai-chat 前。
	code, out, stderr := run(t, "account", "add", "ds1", "--provider", "deepseek", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds1 := mustAccountAdd(t, out)
	if ds1.BaseURL != "https://api.deepseek.com" || ds1.Protocol != "anthropic" {
		t.Fatalf("deepseek 账号的默认值 = %+v，期望 base_url 取厂商表、协议取第一条路由", ds1)
	}

	// --protocol 覆盖默认协议，地址仍来自厂商表。
	code, out, stderr = run(t, "account", "add", "ds2",
		"--provider", "deepseek", "--protocol", "openai-chat", "--key", "sk-2", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds2 := mustAccountAdd(t, out)
	if ds2.BaseURL != "https://api.deepseek.com" || ds2.Protocol != "openai-chat" {
		t.Fatalf("--protocol 应覆盖默认协议 = %+v", ds2)
	}

	// --base-url 覆盖厂商地址，协议仍按厂商表补。
	code, out, stderr = run(t, "account", "add", "ds3",
		"--provider", "deepseek", "--base-url", "http://127.0.0.1:9", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds3 := mustAccountAdd(t, out)
	if ds3.BaseURL != "http://127.0.0.1:9" || ds3.Protocol != "anthropic" {
		t.Fatalf("--base-url 应覆盖厂商地址 = %+v", ds3)
	}

	// 只声明了 openai-responses 的账号也能加：三种线协议都收。
	code, out, stderr = run(t, "account", "add", "oa1",
		"--provider", "openai", "--protocol", "openai-responses", "--key", "sk-4", "--json")
	if code != 0 {
		t.Fatalf("openai-responses 账号的退出码 = %d，stderr = %s", code, stderr)
	}
	oa1 := mustAccountAdd(t, out)
	if oa1.Protocol != "openai-responses" || oa1.BaseURL != "https://api.openai.com" {
		t.Fatalf("openai-responses 账号 = %+v", oa1)
	}

	// 不认识的协议仍是用法错误，文案要列出三种可选值。
	code, _, stderr = run(t, "account", "add", "bad", "--provider", "openai", "--protocol", "gemini", "--key", "sk-5")
	if code != 2 {
		t.Fatalf("不支持的协议退出码 = %d，期望 2", code)
	}
	for _, want := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("协议错误的文案应列出 %q: %s", want, stderr)
		}
	}
}

// TestAccountAddUnknownProvider 覆盖厂商不在表里时的文案：已知厂商清单、最接近的名字、
// 自建地址的走法。
func TestAccountAddUnknownProvider(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	// 拼错一个字：提示最接近的已知厂商。
	code, out, stderr := run(t, "account", "add", "x1", "--provider", "deepsek", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("报错时不该写 stdout: %s", out)
	}
	for _, want := range []string{"deepseek", "已知：", "最接近", "--base-url"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	// 差得远：只列已知厂商，不猜。
	code, _, stderr = run(t, "account", "add", "x2", "--provider", "zzzzzzzz", "--key", "sk-2")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "已知：") {
		t.Fatalf("stderr 应列出已知厂商: %s", stderr)
	}
	if strings.Contains(stderr, "最接近") {
		t.Fatalf("编辑距离超过 2 时不该给最接近的名字: %s", stderr)
	}

	// 自定义名加 --base-url 仍然可用。
	code, out, stderr = run(t, "account", "add", "x3",
		"--provider", "deepsek", "--base-url", "http://127.0.0.1:9", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("自建地址的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Provider != "deepsek" || got.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("自建地址的账号 = %+v", got)
	}
}

// TestAccountAddWithoutProviders 覆盖厂商表被删空后的行为：
// 带 --base-url 照常可用，不带时报错并指出两条下一步。
func TestAccountAddWithoutProviders(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for name := range mustProviderRows(t, out) {
		if code, _, stderr := run(t, "provider", "rm", name, "--force"); code != 0 {
			t.Fatalf("provider rm %s 退出码 = %d，stderr = %s", name, code, stderr)
		}
	}

	code, out, _ = run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d", code)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("厂商表应已清空: %s", out)
	}

	code, out, stderr = run(t, "account", "add", "a1", "--provider", "deepseek", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("厂商表为空时的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("报错时不该写 stdout: %s", out)
	}
	for _, want := range []string{"switchboard provider ls", "--base-url"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	code, out, stderr = run(t, "account", "add", "a1",
		"--provider", "deepseek", "--base-url", "http://127.0.0.1:9", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("带 --base-url 的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("厂商表为空时 --base-url 应直接用上: %+v", got)
	}
}
