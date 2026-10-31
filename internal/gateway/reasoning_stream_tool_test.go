package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lax/internal/gateway"
)

// 这个文件从网关这一层钉住「思考增量在前、工具参数分片在后」这类跨协议流式：
// chat 上游先发 reasoning_content，再发 tool_calls 的参数分片，下发给 anthropic 客户端的事件
// 必须合法——每个 content_block_delta 都落在一个开着、还没收到 content_block_stop 的块上，
// 收尾事件各一份，拼起来的工具参数本身是合法 JSON。

// TestChatReasoningThenToolArgsStreamToAnthropicClient 覆盖 chat 上游的
// reasoning_content → tool_calls 分片序列走 anthropogenic 客户端：
// 思考块先开先关，工具块随后开，两个块各自收一次 stop，思考内容不冒充工具参数。
func TestChatReasoningThenToolArgsStreamToAnthropicClient(t *testing.T) {
	f := newFixture(t)
	upstream := "" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先想一下"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":5}}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	up := fixedSSEUpstream(t, upstream)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-reasoning-tool", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	// 事件序列：思考块、工具块各一对 start/stop，收尾只有一条 message_delta 与一条 message_stop。
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	events := anthropicEvents(t, got)
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", events, want)
	}
	if n := strings.Count(got, "event: content_block_stop"); n != 2 {
		t.Fatalf("content_block_stop 帧数 = %d，期望 2（思考块与工具块各一次）", n)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("完整流不该出现错误帧:\n%s", tail(got, 200))
	}

	// 逐帧检查块的开合：delta 只能落在开着的块上，stop 之后同一个块号不能再收到 delta。
	open := map[int]bool{}
	var thinking string
	var toolJSON string
	var toolID, toolName string
	toolStarts := 0
	for i, fr := range sseFrames(t, got) {
		var payload struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(fr.data), &payload); err != nil {
			t.Fatalf("第 %d 帧的 data 不是合法 JSON: %v，%q", i+1, err, fr.data)
		}
		switch fr.event {
		case "content_block_start":
			if open[payload.Index] {
				t.Fatalf("第 %d 帧在块 %d 还开着的时候又开了同一个块", i+1, payload.Index)
			}
			open[payload.Index] = true
			if payload.ContentBlock.Type == "tool_use" {
				toolStarts++
				toolID, toolName = payload.ContentBlock.ID, payload.ContentBlock.Name
			}
		case "content_block_delta":
			if !open[payload.Index] {
				t.Fatalf("第 %d 帧的 delta 落在没有开着的块 %d 上:\n%s", i+1, payload.Index, got)
			}
			switch payload.Delta.Type {
			case "thinking_delta":
				thinking += payload.Delta.Thinking
			case "input_json_delta":
				toolJSON += payload.Delta.PartialJSON
			case "signature_delta":
				if !strings.HasPrefix(payload.Delta.Signature, "lax-replay:") {
					t.Fatalf("第 %d 帧的签名 = %q，期望带 lax-replay: 前缀的回执签名", i+1, payload.Delta.Signature)
				}
			default:
				t.Fatalf("第 %d 帧的增量类型 = %q，期望 thinking_delta、signature_delta 或 input_json_delta", i+1, payload.Delta.Type)
			}
		case "content_block_stop":
			if !open[payload.Index] {
				t.Fatalf("第 %d 帧关的块 %d 没有开着", i+1, payload.Index)
			}
			delete(open, payload.Index)
		}
	}
	if len(open) != 0 {
		t.Fatalf("流结束时还有 %d 个块没关: %v", len(open), open)
	}

	if thinking != "先想一下" {
		t.Fatalf("思考正文 = %q，期望 先想一下", thinking)
	}
	if toolStarts != 1 {
		t.Fatalf("tool_use 块数 = %d，期望 1", toolStarts)
	}
	if toolID != "call_1" || toolName != "get_weather" {
		t.Fatalf("tool_use 块 = %s/%s，期望 call_1/get_weather", toolID, toolName)
	}

	// 参数分片拼起来是合法 JSON，且思考内容没有混进参数里。
	var args map[string]string
	if err := json.Unmarshal([]byte(toolJSON), &args); err != nil {
		t.Fatalf("工具参数拼起来不是合法 JSON: %v，拼出来是 %q", err, toolJSON)
	}
	if args["city"] != "北京" || len(args) != 1 {
		t.Fatalf("工具参数 = %v，期望只有 city=北京", args)
	}

	// 收尾事件带这次的上游用量：finish_reason 之前收到的 usage 要落在 message_delta 上。
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	var stopReason string
	for _, fr := range sseFrames(t, got) {
		if fr.event != "message_delta" {
			continue
		}
		var payload struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(fr.data), &payload); err != nil {
			t.Fatalf("message_delta 的 data 不是合法 JSON: %v", err)
		}
		stopReason = payload.Delta.StopReason
		usage.InputTokens, usage.OutputTokens = payload.Usage.InputTokens, payload.Usage.OutputTokens
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q，期望 tool_use", stopReason)
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 5 {
		t.Fatalf("message_delta 的 usage = %d/%d，期望 9/5", usage.InputTokens, usage.OutputTokens)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK || recs[0].InTokens != 9 || recs[0].OutTokens != 5 {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1、用量 9/5", recs)
	}
}

// TestChatReasoningBetweenToolArgsStreamToAnthropicClient 覆盖思考增量插在两次工具参数之间：
// 已经关掉的工具块不能再收 input_json_delta，编码器必须先关块再重开，每个 delta 仍落在
// 开着的块上。同一个上游块号因此会出现两个 tool_use 块：已经发过 content_block_stop 的块不能再收
// content_block_delta，只能重开一个。
func TestChatReasoningBetweenToolArgsStreamToAnthropicClient(t *testing.T) {
	f := newFixture(t)
	upstream := "" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"北京\""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"再查一下温度"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	up := fixedSSEUpstream(t, upstream)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-interleaved", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	open := map[int]bool{}
	toolBlocks := 0
	var fragments []string
	for i, fr := range sseFrames(t, got) {
		var payload struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(fr.data), &payload); err != nil {
			t.Fatalf("第 %d 帧的 data 不是合法 JSON: %v，%q", i+1, err, fr.data)
		}
		switch fr.event {
		case "content_block_start":
			open[payload.Index] = true
			if payload.ContentBlock.Type == "tool_use" {
				toolBlocks++
			}
		case "content_block_delta":
			if !open[payload.Index] {
				t.Fatalf("第 %d 帧的 delta 落在已经关闭的块 %d 上:\n%s", i+1, payload.Index, got)
			}
			if payload.Delta.Type == "input_json_delta" {
				fragments = append(fragments, payload.Delta.PartialJSON)
			}
		case "content_block_stop":
			if !open[payload.Index] {
				t.Fatalf("第 %d 帧关的块 %d 没有开着", i+1, payload.Index)
			}
			delete(open, payload.Index)
		}
	}
	if len(open) != 0 {
		t.Fatalf("流结束时还有 %d 个块没关: %v", len(open), open)
	}

	// 思考增量把开着的工具块关掉，后半截参数只能重开一个块：两个 tool_use 块、id 相同。
	if toolBlocks != 2 {
		t.Fatalf("tool_use 块数 = %d，期望 2（参数被思考增量截成两段）", toolBlocks)
	}
	if n := strings.Count(got, `"id":"call_1"`); n != 2 {
		t.Fatalf("tool_use 的 id 出现 %d 次，期望 2（重开的块沿用同一个 id）", n)
	}
	if joined := strings.Join(fragments, ""); joined != `{"city":"北京"}` {
		t.Fatalf("参数分片拼起来 = %q，期望 {\"city\":\"北京\"}", joined)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("完整流不该出现错误帧:\n%s", tail(got, 200))
	}
}
