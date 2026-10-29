package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCallEachInboundProtocol 确认 call 能按三条非 gemini 的入站协议各打一条
// （gemini 的另一条用例见 TestCallGeminiInbound）。假上游只说 openai-chat，
// 所以 chat 入站走直通，另两条走转换。
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

// TestCallNoModelsHint 盯住 call 找不到可用模型时的提示：说清空模型列表的账号不参与选号，
// 下一步给得出来、跑得起来（model sync 与 model probe），而不是只让人去手填。
func TestCallNoModelsHint(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := run(t, "account", "add", "bare",
		"--provider", "mine", "--base-url", "http://127.0.0.1:9", "--key", "sk-1",
		"--protocol", "openai-chat"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "call", "hi")
	if code != 2 {
		t.Fatalf("没有可用模型时 call 退出码 = %d，期望 2（用法错误）", code)
	}
	if out != "" {
		t.Fatalf("用法错误不该写 stdout: %s", out)
	}
	for _, want := range []string{"switchboard model sync", "switchboard model probe", "不参与选号"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
}

// gemini 入站的两个用例共用这两份假报文：模型名在路径里，非流式与流式各一条路径
// （见 DESIGN 5.9）。chat 上游的响应体用 fakeupstream_test.go 里的 chatCompletionOK。
const (
	geminiResponseOK = `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`
	geminiStreamOK   = "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"po\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ng\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":2,\"totalTokenCount\":9}}\n\n"
)

// TestCallGeminiInbound 确认 call 能按 gemini 入站打一条：模型名不在请求体里，由本命令拼进
// 入站路径，服务端按路径里的模型名选号（DESIGN 4 的 call 行）。账号是 openai-chat，
// 因此这条请求走 gemini → chat 的转换路径，拿到的响应是 gemini 形状。
func TestCallGeminiInbound(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: 200, body: chatCompletionOK},
	})
	mustInit(t)

	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	code, out, stderr := run(t, "call", "hi", "--model", "m1", "--protocol", "gemini")
	if code != 0 {
		t.Fatalf("call --protocol gemini 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("call --protocol gemini 输出 = %q，期望包含 pong", out)
	}
	// 上游收到的是 chat 请求，路径里没有 gemini 的模型名：模型名走的是入站路径。
	if call := up.mustSingleCall(t); call.path != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %+v，期望转发到 chat 路由", call)
	}

	// --json 打的是入站协议的报文，gemini 的就是 candidates。
	code, out, stderr = run(t, "call", "hi", "--model", "m1", "--protocol", "gemini", "--json")
	if code != 0 {
		t.Fatalf("call --protocol gemini --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json 输出不是合法 JSON: %v，%s", err, out)
	}
	if _, ok := payload["candidates"]; !ok {
		t.Fatalf("gemini 入站的 --json 应带 candidates: %s", out)
	}
}

// TestCallGeminiInboundStream 确认 --stream 走 :streamGenerateContent 这条入站路径，
// 并且响应按 SSE 逐帧解码后输出文本增量，不是把帧原文倒出来。
func TestCallGeminiInboundStream(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1beta/models/m1:generateContent":       {status: 200, body: geminiResponseOK},
		"POST /v1beta/models/m1:streamGenerateContent": {status: 200, body: geminiStreamOK},
	})
	mustInit(t)

	if code, _, stderr := run(t, "account", "add", "g1", "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test", "--protocol", "gemini", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	code, out, stderr := run(t, "call", "hi", "--model", "m1", "--protocol", "gemini", "--stream")
	if code != 0 {
		t.Fatalf("call --protocol gemini --stream 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "pong") || strings.Contains(out, "data:") {
		t.Fatalf("流式输出 = %q，期望是解码后的文本增量", out)
	}
	call := up.mustSingleCall(t)
	if call.path != "/v1beta/models/m1:streamGenerateContent" || call.googKey != "sk-test" {
		t.Fatalf("流式的上游调用 = %+v，期望路径带 :streamGenerateContent、头是 x-goog-api-key", call)
	}
}

// TestCallGeminiInboundStreamJSON 确认 gemini 流式的 --json 是一个 data 帧一行 JSON：
// 帧里没有模型名，原文就是协议对客户端说的全部内容，脚本能逐行解析。
func TestCallGeminiInboundStreamJSON(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1beta/models/m1:streamGenerateContent": {status: 200, body: geminiStreamOK},
	})
	mustInit(t)

	if code, _, stderr := run(t, "account", "add", "g1", "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test", "--protocol", "gemini", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	code, out, stderr := run(t, "call", "hi", "--model", "m1", "--protocol", "gemini", "--stream", "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("--json 流式输出应为两行（一帧一行），得到 %d 行: %q", len(lines), out)
	}
	for _, line := range lines {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("--json 的每一行都应是合法 JSON: %v，行 = %q", err, line)
		}
	}
}
