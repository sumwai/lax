package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"switchboard/internal/gateway"
)

// 这个文件从网关这一层钉住思考内容跨协议的两侧：上游没有签名时 anthropic 客户端
// 仍能看到思考块（只发正文、不发签名，也不报错），anthropic 的签名不会漏进另外两条协议，
// responses 的 reasoning item 两个方向都还在。

// TestChatReasoningStreamToAnthropicClient 覆盖 chat 上游的 reasoning_content 到 anthropic
// 客户端：思考正文编成 thinking 块，签名一个都没有（上游给不出），流照常收尾、不报错。
func TestChatReasoningStreamToAnthropicClient(t *testing.T) {
	f := newFixture(t)
	upstream := "" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先想一下"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"答案"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	up := fixedSSEUpstream(t, upstream)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-reason", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	events := anthropicEvents(t, got)
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", events, want)
	}

	// 第一个块是 thinking：只有 type 与空正文的起始帧，随后是 thinking_delta。
	frames := sseFrames(t, got)
	var start struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(frames[1].data), &start); err != nil {
		t.Fatalf("content_block_start 的 data 不是合法 JSON: %v", err)
	}
	if start.ContentBlock.Type != "thinking" || start.Index != 0 {
		t.Fatalf("第一个块 = %+v，期望 index=0、type=thinking", start.ContentBlock)
	}

	var thinking string
	for _, fr := range frames {
		if fr.event != "content_block_delta" {
			continue
		}
		var delta struct {
			Delta struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(fr.data), &delta); err != nil {
			t.Fatalf("content_block_delta 的 data 不是合法 JSON: %v", err)
		}
		if delta.Delta.Type == "thinking_delta" {
			thinking += delta.Delta.Thinking
		}
	}
	if thinking != "先想一下" {
		t.Fatalf("思考正文 = %q，期望 先想一下", thinking)
	}
	// chat 上游给不出签名：不该凭空造一个 signature_delta 出来。
	if strings.Contains(got, "signature") {
		t.Fatalf("chat 上游转出来的流里不该有签名:\n%s", got)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("完整流不该出现错误帧:\n%s", tail(got, 200))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// TestAnthropicSignedThinkingStreamToChatClient 覆盖 anthropic 上游带签名的思考块到 chat
// 客户端：正文写进 delta.reasoning_content，签名在 chat 里没有落点，不能出现在报文里。
func TestAnthropicSignedThinkingStreamToChatClient(t *testing.T) {
	const signature = "sig-0123456789abcdef"
	f := newFixture(t)
	upstream := "" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想一下"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	up := fixedSSEUpstream(t, upstream)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-signed", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}
	if strings.Contains(got, signature) {
		t.Fatalf("签名不该出现在 chat 的流里:\n%s", got)
	}

	frames := chatFrames(t, got)
	var reasoning, content strings.Builder
	for _, fr := range frames {
		for _, ch := range fr.Choices {
			if ch.Delta.ReasoningContent != nil {
				reasoning.WriteString(*ch.Delta.ReasoningContent)
			}
			if ch.Delta.Content != nil {
				content.WriteString(*ch.Delta.Content)
			}
		}
	}
	if thinking := reasoning.String(); thinking != "先想一下" {
		t.Fatalf("reasoning_content = %q，期望 先想一下", thinking)
	}
	if text := content.String(); text != "答案" {
		t.Fatalf("正文 = %q，期望 答案", text)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("完整流不该出现错误帧:\n%s", tail(got, 200))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// TestResponsesReasoningStreamToChatClient 覆盖 responses 上游的 reasoning item 到 chat
// 客户端：reasoning_summary_text.delta 的正文写进 delta.reasoning_content。
func TestResponsesReasoningStreamToChatClient(t *testing.T) {
	f := newFixture(t)
	upstream := "" +
		`data: {"type":"response.created","response":{"id":"r1","model":"up"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}` + "\n\n" +
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"先想一下"}` + "\n\n" +
		`data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"答案"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":5,"output_tokens":4}}}` + "\n\n"
	up := fixedSSEUpstream(t, upstream)
	f.add(t, "resp", gateway.ProtocolResponses, up.URL, "sk-resp", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	frames := chatFrames(t, got)
	var reasoning, content strings.Builder
	for _, fr := range frames {
		for _, ch := range fr.Choices {
			if ch.Delta.ReasoningContent != nil {
				reasoning.WriteString(*ch.Delta.ReasoningContent)
			}
			if ch.Delta.Content != nil {
				content.WriteString(*ch.Delta.Content)
			}
		}
	}
	if thinking := reasoning.String(); thinking != "先想一下" {
		t.Fatalf("reasoning_content = %q，期望 先想一下", thinking)
	}
	if text := content.String(); text != "答案" {
		t.Fatalf("正文 = %q，期望 答案", text)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// TestResponsesReasoningItemToChatUpstream 覆盖 responses 入站请求里的 reasoning item：
// 客户端把上一轮的思考回填进 input 时，编到 chat 上游不回填 reasoning_content——
// 主流推理模型对回填该字段直接回 400，所以请求里只剩它后面的用户消息。
func TestResponsesReasoningItemToChatUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-chat", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_output_tokens":64,"input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"先想一下"}]},` +
		`{"type":"message","role":"user","content":"继续"}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolResponses)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Messages) != 1 {
		t.Fatalf("消息条数 = %d，期望 1（思考不回填，只剩用户消息）: %s", len(sent.Messages), rec.body)
	}
	if strings.Contains(rec.body, "reasoning_content") {
		t.Fatalf("上游请求里不该出现 reasoning_content: %s", rec.body)
	}
	if sent.Messages[0].Role != "user" || sent.Messages[0].Content != "继续" {
		t.Fatalf("第一条消息 = %+v，期望 role=user、content=继续", sent.Messages[0])
	}
}
