package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
)

// 这个文件从网关这一层钉住思考内容的两个方向：
//
//   - 请求方向不回填：anthropic 客户端的历史里带签名 thinking 块，换成 openai-chat 或
//     openai-responses 账号时，发给上游的报文里不能有 reasoning_content、也不能有
//     reasoning 字段/条目——主流推理上游对回填这两种字段直接回 400。
//   - 响应方向照旧搬运：上游回来的思考正文仍要写进客户端协议的落点。
//
// 只剩思考内容的 assistant 消息在这两条路上会被整条丢掉，用例一并断言丢掉之后上游报文
// 仍然合法：没有空正文的消息，也没有 content 为空数组的消息。

// signedThinkingHistory 是一条 anthropic 入站请求：带签名思考块与正文的 assistant 消息
// 夹在两条用户消息中间，正是 Claude Code 开着 thinking 回填历史的样子。
const signedThinkingHistory = `{"model":"m1","max_tokens":256,"messages":[` +
	`{"role":"user","content":"6 乘 7 是多少"},` +
	`{"role":"assistant","content":[` +
	`{"type":"thinking","thinking":"用户问乘法，先算 6×7=42。","signature":"sig-abc123"},` +
	`{"type":"text","text":"42"}]},` +
	`{"role":"user","content":"那 8 乘 9 呢"}]}`

// thinkingOnlyHistory 是同一个壳，assistant 那条只剩一个带签名的 thinking 块：
// 编到 chat 与 responses 时整条消息什么都不剩。
const thinkingOnlyHistory = `{"model":"m1","max_tokens":256,"messages":[` +
	`{"role":"user","content":"6 乘 7 是多少"},` +
	`{"role":"assistant","content":[` +
	`{"type":"thinking","thinking":"用户问乘法，先算 6×7=42。","signature":"sig-abc123"}]},` +
	`{"role":"user","content":"那 8 乘 9 呢"}]}`

// TestAnthropicSignedThinkingHistoryToChatUpstreamOmitsReasoning 覆盖 anthropic 入站、chat 出站：
// 历史里带签名的思考块不回填成 assistant 的 reasoning_content，写给上游的只有三条消息，
// assistant 那条只剩正文，签名在 chat 里也没有落点。
func TestAnthropicSignedThinkingHistoryToChatUpstreamOmitsReasoning(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-thinking-history", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postTo(t, srv, signedThinkingHistory, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	// reasoning_content 与 reasoning 两种写法、以及签名本身，都不该出现在发给上游的报文里。
	for _, bad := range []string{"reasoning_content", `"reasoning"`, "sig-abc123", "用户问乘法，先算 6×7=42。"} {
		if strings.Contains(rec.body, bad) {
			t.Fatalf("上游请求里不该出现 %q: %s", bad, rec.body)
		}
	}

	var sent struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("消息条数 = %d，期望 3（用户、assistant 正文、用户）: %s", len(sent.Messages), rec.body)
	}
	wantRoles := []string{"user", "assistant", "user"}
	wantContents := []string{"6 乘 7 是多少", "42", "那 8 乘 9 呢"}
	for i, raw := range sent.Messages {
		var role, content string
		if err := json.Unmarshal(raw["role"], &role); err != nil {
			t.Fatalf("第 %d 条消息的 role 不是字符串: %s", i+1, rec.body)
		}
		if err := json.Unmarshal(raw["content"], &content); err != nil {
			t.Fatalf("第 %d 条消息的 content 不是字符串: %s", i+1, rec.body)
		}
		if role != wantRoles[i] || content != wantContents[i] {
			t.Fatalf("第 %d 条消息 = %s/%q，期望 %s/%q", i+1, role, content, wantRoles[i], wantContents[i])
		}
	}
}

// TestAnthropicSignedThinkingHistoryToResponsesUpstreamOmitsReasoningItem 覆盖 anthropic 入站、
// responses 出站：input 里没有 reasoning item，只有两条 user message 与一条 assistant message，
// 正文都在 output_text 里。
func TestAnthropicSignedThinkingHistoryToResponsesUpstreamOmitsReasoningItem(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolResponses)
	f.add(t, "rs1", gateway.ProtocolResponses, up.URL, "sk-thinking-input", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postTo(t, srv, signedThinkingHistory, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	if strings.Contains(rec.body, "reasoning") {
		t.Fatalf("上游 input 里不该出现 reasoning 条目: %s", rec.body)
	}

	var sent struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Input) != 3 {
		t.Fatalf("input 条目数 = %d，期望 3: %s", len(sent.Input), rec.body)
	}
	want := []struct {
		role, textType, text string
	}{
		{"user", "input_text", "6 乘 7 是多少"},
		{"assistant", "output_text", "42"},
		{"user", "input_text", "那 8 乘 9 呢"},
	}
	for i, item := range sent.Input {
		if item.Type != "message" || item.Role != want[i].role {
			t.Fatalf("第 %d 个条目 = %s/%s，期望 message/%s", i+1, item.Type, item.Role, want[i].role)
		}
		if len(item.Content) != 1 || item.Content[0].Type != want[i].textType || item.Content[0].Text != want[i].text {
			t.Fatalf("第 %d 个条目的 content = %+v，期望 %s/%s", i+1, item.Content, want[i].textType, want[i].text)
		}
	}
}

// TestChatReasoningResponseToResponsesClientKeepsReasoningItem 覆盖响应方向的另一半：
// responses 客户端的历史里带着上一轮的 reasoning item（请求方向丢掉它），上游 chat 回来的
// reasoning_content 仍要编成 reasoning item 交给客户端，正文在 summary 里。
func TestChatReasoningResponseToResponsesClientKeepsReasoningItem(t *testing.T) {
	f := newFixture(t)
	var sentBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sentBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-r","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"先想一下","content":"答案"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	t.Cleanup(up.Close)

	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-response-reasoning", []string{"m1"}, 100)
	srv := f.server(t)

	request := `{"model":"m1","max_output_tokens":64,"input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"上一轮的思考"}]},` +
		`{"type":"message","role":"user","content":"继续"}]}`
	status, body := postTo(t, srv, request, gateway.ProtocolResponses)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	// 请求方向：回填的 reasoning item 不进上游报文。
	for _, bad := range []string{"reasoning", "上一轮的思考"} {
		if strings.Contains(sentBody, bad) {
			t.Fatalf("上游请求里不该出现 %q: %s", bad, sentBody)
		}
	}

	var out struct {
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，%s", err, body)
	}
	if len(out.Output) != 2 {
		t.Fatalf("output 条目数 = %d，期望 2（思考 item 与正文 item）: %s", len(out.Output), body)
	}
	thinking := out.Output[0]
	if thinking.Type != "reasoning" || len(thinking.Summary) != 1 ||
		thinking.Summary[0].Type != "summary_text" || thinking.Summary[0].Text != "先想一下" {
		t.Fatalf("第一个条目 = %+v，期望 reasoning item、summary 正文为 先想一下", thinking)
	}
	message := out.Output[1]
	if message.Type != "message" || message.Role != "assistant" ||
		len(message.Content) != 1 || message.Content[0].Text != "答案" {
		t.Fatalf("第二个条目 = %+v，期望 assistant message、正文为 答案", message)
	}
}

// TestThinkingOnlyAssistantToChatUpstreamLeavesNoEmptyMessage 覆盖只剩思考的 assistant 消息：
// anthropic 入站的那条 assistant 只有带签名的 thinking 块，编到 chat 时整条丢掉。
// 丢掉之后上游收到的是两条用户消息——chat 接受相邻的 user 消息，但绝不能是空正文的
// assistant，也不能是 content 为 null 的消息。
func TestThinkingOnlyAssistantToChatUpstreamLeavesNoEmptyMessage(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-thinking-only-chat", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postTo(t, srv, thinkingOnlyHistory, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	if strings.Contains(rec.body, "reasoning") {
		t.Fatalf("上游请求里不该出现 reasoning 字段: %s", rec.body)
	}

	var sent struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Messages) != 2 {
		t.Fatalf("消息条数 = %d，期望 2（只剩两条用户消息）: %s", len(sent.Messages), rec.body)
	}
	wantContents := []string{"6 乘 7 是多少", "那 8 乘 9 呢"}
	for i, raw := range sent.Messages {
		var role, content string
		if err := json.Unmarshal(raw["role"], &role); err != nil {
			t.Fatalf("第 %d 条消息的 role 不是字符串: %s", i+1, rec.body)
		}
		if role != "user" {
			t.Fatalf("第 %d 条消息的 role = %q，期望 user（只剩思考的 assistant 整条丢掉）", i+1, role)
		}
		if err := json.Unmarshal(raw["content"], &content); err != nil {
			t.Fatalf("第 %d 条消息的 content 不是字符串，报文里有空消息: %s", i+1, rec.body)
		}
		if content != wantContents[i] {
			t.Fatalf("第 %d 条消息的 content = %q，期望 %q", i+1, content, wantContents[i])
		}
	}
}

// TestThinkingOnlyAssistantToResponsesUpstreamLeavesNoEmptyItem 是同一个历史走 responses 出站：
// input 里同样没有 reasoning item，也没有 content 为空数组的 message item。
func TestThinkingOnlyAssistantToResponsesUpstreamLeavesNoEmptyItem(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolResponses)
	f.add(t, "rs1", gateway.ProtocolResponses, up.URL, "sk-thinking-only-resp", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postTo(t, srv, thinkingOnlyHistory, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	if strings.Contains(rec.body, "reasoning") {
		t.Fatalf("上游 input 里不该出现 reasoning 条目: %s", rec.body)
	}

	var sent struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Input) != 2 {
		t.Fatalf("input 条目数 = %d，期望 2（只剩两条用户消息）: %s", len(sent.Input), rec.body)
	}
	wantTexts := []string{"6 乘 7 是多少", "那 8 乘 9 呢"}
	for i, item := range sent.Input {
		if item.Type != "message" || item.Role != "user" {
			t.Fatalf("第 %d 个条目 = %s/%s，期望 message/user", i+1, item.Type, item.Role)
		}
		if len(item.Content) != 1 || item.Content[0].Type != "input_text" || item.Content[0].Text != wantTexts[i] {
			t.Fatalf("第 %d 个条目的 content = %+v，期望一条 input_text %q（不能是空数组）",
				i+1, item.Content, wantTexts[i])
		}
	}
}
