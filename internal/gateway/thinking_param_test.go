package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"lax/internal/gateway"
)

// 这个文件从网关这一层钉住请求侧的思考配置参数：跨协议时
// 假上游真正收到的请求体里有没有那个字段、值是多少；同协议直通时不写不删；客户端没给时
// 一个字段都不出现。translate 层的对拍用例钉的是编解码结果，这里钉的是转发链路上
// 传出去的那份报文——中间任何一层把 ThinkingConfig 丢掉或多写一个字段，都只有这一层看得出来。

// upstreamJSON 把假上游收到的请求体解成通用 JSON，断言里直接取具体值。
func upstreamJSON(t *testing.T, rec *recordedRequest) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(rec.body), &got); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	return got
}

// assertJSONField 断言出站请求体里某个顶层键的值等于期望的 JSON（按解码后的值比较，
// 不看键顺序与空白）。
func assertJSONField(t *testing.T, got map[string]any, key, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("期望值 %q 不是合法 JSON: %v", wantJSON, err)
	}
	actual, ok := got[key]
	if !ok {
		t.Fatalf("出站请求体里没有 %s 字段: %v", key, got)
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("%s = %#v，期望 %#v", key, actual, want)
	}
}

// assertNoThinkingParams 断言出站请求体里没有任何思考配置字段。gemini 的落点是
// generationConfig.thinkingConfig，单独往下一层看。
func assertNoThinkingParams(t *testing.T, body string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, body)
	}
	for _, key := range []string{"thinking", "reasoning_effort", "reasoning"} {
		if raw, ok := top[key]; ok {
			t.Fatalf("客户端没给思考参数，出站请求体里却有 %s: %s", key, raw)
		}
	}
	raw, ok := top["generationConfig"]
	if !ok {
		return
	}
	var gen map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gen); err != nil {
		t.Fatalf("generationConfig 不是合法 JSON: %v，%s", err, raw)
	}
	if tc, ok := gen["thinkingConfig"]; ok {
		t.Fatalf("客户端没给思考参数，generationConfig 里却有 thinkingConfig: %s", tc)
	}
}

// TestAnthropicThinkingBudgetToChatUpstreamEffort 覆盖 anthropic 入站、chat 出站：
// thinking{budget_tokens} 折成 reasoning_effort，档位按四档折算：
// （≥8192 high、≥2048 medium、否则 low）。预算本身在 chat 上没有落点。
func TestAnthropicThinkingBudgetToChatUpstreamEffort(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{1024, "low"},
		{4096, "medium"},
		{8192, "high"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("budget_%d_to_%s", c.budget, c.want), func(t *testing.T) {
			f := newFixture(t)
			up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
			f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-thinking", []string{"m1"}, 100)
			srv := f.server(t)

			// max_tokens 给到比预算大：anthropic 要求 budget_tokens 小于 max_tokens，
			// 这里造一份自身合法的入站报文，避免用例把两种约束混在一起。
			body := fmt.Sprintf(`{"model":"m1","max_tokens":16384,`+
				`"thinking":{"type":"enabled","budget_tokens":%d},`+
				`"messages":[{"role":"user","content":"hi"}]}`, c.budget)
			status, resp := postTo(t, srv, body, gateway.ProtocolAnthropic)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, resp)
			}

			got := upstreamJSON(t, rec)
			if effort, _ := got["reasoning_effort"].(string); effort != c.want {
				t.Fatalf("reasoning_effort = %#v，期望 %q，请求体 %s", got["reasoning_effort"], c.want, rec.body)
			}
			if _, ok := got["thinking"]; ok {
				t.Fatalf("chat 出站不该带 thinking 字段: %s", rec.body)
			}
		})
	}
}

// TestChatReasoningEffortToAnthropicUpstreamThinking 覆盖 chat 入站、anthropic 出站：
// reasoning_effort:high 折成 thinking{type:enabled,budget_tokens:8192}，8192 就是档位→预算换算表里 high 那一档。
func TestChatReasoningEffortToAnthropicUpstreamThinking(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-effort", []string{"m1"}, 100)
	srv := f.server(t)

	// max_tokens 大于折出来的 8192：anthropic 要求 budget_tokens 小于 max_tokens。
	body := `{"model":"m1","max_tokens":16384,"reasoning_effort":"high",` +
		`"messages":[{"role":"user","content":"hi"}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	got := upstreamJSON(t, rec)
	assertJSONField(t, got, "thinking", `{"type":"enabled","budget_tokens":8192}`)
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("anthropic 出站不该带 reasoning_effort: %s", rec.body)
	}
}

// TestAnthropicThinkingBudgetToResponsesUpstreamEffort 覆盖 anthropic 入站、responses 出站：
// 预算同样折成档位，但 responses 的落点是 reasoning.effort，不是顶层 reasoning_effort。
func TestAnthropicThinkingBudgetToResponsesUpstreamEffort(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolResponses)
	f.add(t, "rs1", gateway.ProtocolResponses, up.URL, "sk-thinking-responses", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":8192,` +
		`"thinking":{"type":"enabled","budget_tokens":4096},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	got := upstreamJSON(t, rec)
	assertJSONField(t, got, "reasoning", `{"effort":"medium"}`)
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("responses 出站不该带顶层 reasoning_effort: %s", rec.body)
	}
}

// TestGeminiThinkingBudgetToAnthropicUpstreamThinking 覆盖 gemini 入站、anthropic 出站：
// generationConfig.thinkingConfig.thinkingBudget 原样落到 budget_tokens。
func TestGeminiThinkingBudgetToAnthropicUpstreamThinking(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-gemini-budget", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"maxOutputTokens":8192,"thinkingConfig":{"thinkingBudget":2048}}}`
	status, resp := postTo(t, srv, body, gateway.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	assertJSONField(t, upstreamJSON(t, rec), "thinking", `{"type":"enabled","budget_tokens":2048}`)
}

// TestGeminiThinkingDisabledToAnthropicUpstreamDisabled 覆盖「不要思考」这一档走到 anthropic：
// thinkingBudget:0 解成关闭，anthropic 出站写 thinking{type:"disabled"}，不带 budget_tokens。
func TestGeminiThinkingDisabledToAnthropicUpstreamDisabled(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-gemini-off", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"maxOutputTokens":256,"thinkingConfig":{"thinkingBudget":0}}}`
	status, resp := postTo(t, srv, body, gateway.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	// 逐字段相等：多写一个 budget_tokens 会让 anthropic 把整条请求 400 掉。
	assertJSONField(t, upstreamJSON(t, rec), "thinking", `{"type":"disabled"}`)
}

// TestGeminiThinkingDisabledToChatUpstreamOmitsEffort 覆盖同一个「不要思考」走到 chat：
// 两条 openai 协议没有「不思考」的档位，所以 reasoning_effort 一个都不写，而不是写个 low 冒充。
func TestGeminiThinkingDisabledToChatUpstreamOmitsEffort(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-gemini-off-chat", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"maxOutputTokens":256,"thinkingConfig":{"thinkingBudget":0}}}`
	status, resp := postTo(t, srv, body, gateway.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	got := upstreamJSON(t, rec)
	for _, key := range []string{"reasoning_effort", "thinking", "reasoning"} {
		if raw, ok := got[key]; ok {
			t.Fatalf("chat 出站不该写 %s: %v", key, raw)
		}
	}
}

// TestAnthropicPassthroughKeepsThinkingVerbatim 覆盖同协议直通：入站 anthropic 带 thinking、
// 出站也是 anthropic 时，上游收到的报文除 model 外逐字段与客户端给的字节相同。
// 直通路径不经过编解码，多写或删掉一个字段都是转发链路的错。
func TestAnthropicPassthroughKeepsThinkingVerbatim(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-passthrough", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":8192,` +
		`"thinking":{"type":"enabled","budget_tokens":3000},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	var sent, got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("客户端请求体不是合法 JSON: %v，%s", err, body)
	}
	if err := json.Unmarshal([]byte(rec.body), &got); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	// model 是直通唯一允许改的字段：换成解析出来的 m1。
	if string(got["model"]) != `"m1"` {
		t.Fatalf("上游 model = %s，期望 \"m1\"", got["model"])
	}
	delete(sent, "model")
	delete(got, "model")
	if len(got) != len(sent) {
		t.Fatalf("直通改了字段数：上游 %d 个，客户端 %d 个，上游报文 %s", len(got), len(sent), rec.body)
	}
	for key, want := range sent {
		raw, ok := got[key]
		if !ok {
			t.Fatalf("直通丢了 %s 字段: %s", key, rec.body)
		}
		if string(raw) != string(want) {
			t.Fatalf("%s 被改动了：上游 %s，客户端 %s", key, raw, want)
		}
	}
}

// TestNoThinkingParamsAnyOutbound 覆盖客户端完全没给思考参数的情形：四条出站协议都不得
// 凭空写 thinking、reasoning_effort、reasoning 或 thinkingConfig——上游对不认识的参数直接 400。
// 每条都走跨协议编码，编码器才有机会多写这个字段。
func TestNoThinkingParamsAnyOutbound(t *testing.T) {
	cases := []struct {
		name     string
		inbound  string
		outbound string
		body     string
		// genConfig 为真的那条要顺带确认 generationConfig 真的写出来了：
		// gemini 的 thinkingConfig 就藏在它里面，字段整块缺失时那条断言没有牙齿。
		genConfig bool
	}{
		{
			name:     "anthropic_in_chat_out",
			inbound:  gateway.ProtocolAnthropic,
			outbound: gateway.ProtocolChat,
			body:     `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "chat_in_responses_out",
			inbound:  gateway.ProtocolChat,
			outbound: gateway.ProtocolResponses,
			body:     `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "responses_in_anthropic_out",
			inbound:  gateway.ProtocolResponses,
			outbound: gateway.ProtocolAnthropic,
			body:     `{"model":"m1","max_output_tokens":16,"input":"hi"}`,
		},
		{
			name:      "responses_in_gemini_out",
			inbound:   gateway.ProtocolResponses,
			outbound:  gateway.ProtocolGemini,
			body:      `{"model":"m1","max_output_tokens":16,"input":"hi"}`,
			genConfig: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up, rec := fakeProtocolUpstream(t, c.outbound)
			f.add(t, "up-"+c.outbound, c.outbound, up.URL, "sk-plain", []string{"m1"}, 100)
			srv := f.server(t)

			status, resp := postTo(t, srv, c.body, c.inbound)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, resp)
			}
			assertNoThinkingParams(t, rec.body)
			if c.genConfig && !strings.Contains(rec.body, "generationConfig") {
				t.Fatalf("gemini 出站少了 generationConfig，thinkingConfig 的断言没有落点: %s", rec.body)
			}
		})
	}
}
