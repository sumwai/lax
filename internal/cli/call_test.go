package cli_test

import (
	"strings"
	"testing"
)

// TestCallEachInboundProtocol 确认 call 能按三种入站协议各打一条。
// 假上游只说 openai-chat，所以 chat 入站走直通，另两条走转换。
func TestCallEachInboundProtocol(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	for _, protocol := range []string{"anthropic", "chat", "responses"} {
		code, out, stderr := run(t, "call", "hi", "--model", "m1", "--protocol", protocol)
		if code != 0 {
			t.Fatalf("call --protocol %s 退出码 = %d，stderr = %s", protocol, code, stderr)
		}
		if !strings.Contains(out, "pong") {
			t.Fatalf("call --protocol %s 输出 = %q，期望包含 pong", protocol, out)
		}
	}

	code, _, stderr := run(t, "call", "hi", "--model", "m1", "--protocol", "nope")
	if code != 2 {
		t.Fatalf("未知协议退出码 = %d，期望 2（用法错误）", code)
	}
	if !strings.Contains(stderr, "anthropic") || !strings.Contains(stderr, "responses") {
		t.Fatalf("stderr 应列出可用协议: %s", stderr)
	}
}

// TestCallJSONShapeFollowsInboundProtocol 确认 --json 返回入站协议的原始报文。
func TestCallJSONShapeFollowsInboundProtocol(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)

	if code, _, _ := run(t, "init"); code != 0 {
		t.Fatal("init 失败")
	}
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	cases := map[string]string{
		"anthropic": `"type":"message"`,
		"chat":      `"object":"chat.completion"`,
		"responses": `"object":"response"`,
	}
	for protocol, marker := range cases {
		code, out, stderr := run(t, "call", "hi", "--model", "m1", "--protocol", protocol, "--json")
		if code != 0 {
			t.Fatalf("call --protocol %s --json 退出码 = %d，stderr = %s", protocol, code, stderr)
		}
		if !strings.Contains(out, marker) {
			t.Fatalf("%s 的 --json 输出缺少 %s: %s", protocol, marker, out)
		}
	}
}
