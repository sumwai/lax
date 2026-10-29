package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	"switchboard/internal/gateway"
)

// 这个文件钉住流式请求上的思考配置参数：流式与非流式走同一条 buildRequest，但入站的
// stream 字段、出站 gemini 的流式端点都会改变报文形状，所以两条方向各留一条用例，
// 断言假上游收到的请求体里仍然有换算后的字段，且客户端这一侧照常收齐帧。

// TestAnthropicThinkingStreamRequestToChatUpstreamEffort 覆盖流式 anthropic 入站、chat 出站：
// 上游收到的请求体里 reasoning_effort 仍是 medium（budget 4096），客户端流照常收齐。
func TestAnthropicThinkingStreamRequestToChatUpstreamEffort(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeStreamUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-stream-thinking", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":8192,"stream":true,` +
		`"thinking":{"type":"enabled","budget_tokens":4096},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	status, got := postTo(t, srv, body, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	sent := upstreamJSON(t, rec)
	if effort, _ := sent["reasoning_effort"].(string); effort != "medium" {
		t.Fatalf("reasoning_effort = %#v，期望 medium，请求体 %s", sent["reasoning_effort"], rec.body)
	}
	if stream, _ := sent["stream"].(bool); !stream {
		t.Fatalf("上游请求体里 stream 不是 true: %s", rec.body)
	}
	// 流本身要按 anthropic 形状走完：思考参数错了上游不会回包，这里连带钉住转发成功。
	if !strings.Contains(got, "message_stop") {
		t.Fatalf("客户端流缺少收尾帧:\n%s", tail(got, 200))
	}
}

// TestChatReasoningEffortStreamRequestToAnthropicUpstreamThinking 覆盖流式 chat 入站、
// anthropic 出站：上游收到的请求体里 thinking 是 enabled/8192，客户端 anthropic 流照常收齐。
func TestChatReasoningEffortStreamRequestToAnthropicUpstreamThinking(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeStreamUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-stream-effort", []string{"m1"}, 100)
	srv := f.server(t)

	// max_tokens 大于折出来的 8192：anthropic 要求 budget_tokens 小于 max_tokens。
	body := `{"model":"m1","max_tokens":16384,"stream":true,"reasoning_effort":"high",` +
		`"messages":[{"role":"user","content":"hi"}]}`
	status, got := postTo(t, srv, body, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	assertJSONField(t, upstreamJSON(t, rec), "thinking", `{"type":"enabled","budget_tokens":8192}`)
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("客户端流缺少收尾帧:\n%s", tail(got, 200))
	}
}
