package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
)

// 这个文件从网关这一层钉住工具调用跨协议的两侧：客户端发来的工具结果怎么写到上游，
// 上游的工具调用怎么还给客户端，流式下的序号、收尾事件与断流说明各是什么形状。
// 上游一律是 httptest 假服务，断言的是客户端实际收到的字节与上游实际收到的报文。

// sseFrame 是客户端收到的一帧 SSE：事件名（chat 的流没有事件名）与 data 行原文。
type sseFrame struct {
	event string
	data  string
}

// sseFrames 把客户端收到的流按空行切成帧。只用于形状完好的流：
// 断流用例里转发出去的半截参数由各用例自己断言。
func sseFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE 流必须以空行结尾，实际结尾 %q", tail(body, 40))
	}
	var out []sseFrame
	for _, chunk := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		var frame sseFrame
		seen := false
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				seen = true
				if frame.data != "" {
					frame.data += "\n"
				}
				frame.data += strings.TrimPrefix(line, "data: ")
			}
		}
		if !seen {
			t.Fatalf("这一帧没有 data 行: %q", chunk)
		}
		out = append(out, frame)
	}
	if len(out) == 0 {
		t.Fatalf("流里一帧都没有")
	}
	return out
}

// chatFrame 是下发给 chat 客户端的一帧，只取用例要断言的字段。
type chatFrame struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string  `json:"role"`
			ReasoningContent *string `json:"reasoning_content"`
			Content          *string `json:"content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// chatFrames 解析 chat 流：除末尾的 data: [DONE] 外每帧都必须是 chat.completion.chunk。
func chatFrames(t *testing.T, body string) []chatFrame {
	t.Helper()
	frames := sseFrames(t, body)
	if last := frames[len(frames)-1]; last.data != "[DONE]" {
		t.Fatalf("chat 流应以 data: [DONE] 结尾，实际 %q", last.data)
	}
	out := make([]chatFrame, 0, len(frames)-1)
	for i, f := range frames[:len(frames)-1] {
		if f.event != "" {
			t.Fatalf("第 %d 帧不该带事件名（chat 只有 data 帧）: %q", i+1, f.event)
		}
		var c chatFrame
		if err := json.Unmarshal([]byte(f.data), &c); err != nil {
			t.Fatalf("第 %d 帧的 data 不是合法 JSON: %v，%q", i+1, err, f.data)
		}
		out = append(out, c)
	}
	return out
}

// chatErrorFrame 取 chat 流里错误帧的类型与说明，没有错误帧或多于一帧时失败。
func chatErrorFrame(t *testing.T, body string) (kind, message string) {
	t.Helper()
	found := false
	for _, f := range sseFrames(t, body) {
		if f.data == "[DONE]" {
			continue
		}
		var env struct {
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(f.data), &env); err != nil {
			t.Fatalf("帧的 data 不是合法 JSON: %v，%q", err, f.data)
		}
		if env.Error == nil {
			continue
		}
		if found {
			t.Fatalf("流里出现了多个错误帧:\n%s", tail(body, 300))
		}
		found = true
		kind, message = env.Error.Type, env.Error.Message
	}
	if !found {
		t.Fatalf("断流之后应有一个错误帧:\n%s", tail(body, 300))
	}
	return kind, message
}

// chatToolFragment 是 chat 流里一个 tool_calls 分片，字段是这一帧实际写出的值。
type chatToolFragment struct {
	Index    int
	HasIndex bool
	ID       string
	Name     string
	Args     string
}

// chatToolFragments 按出现顺序摊平 chat 流里的全部 tool_calls 分片。
func chatToolFragments(t *testing.T, frames []chatFrame) []chatToolFragment {
	t.Helper()
	var out []chatToolFragment
	for _, f := range frames {
		for _, ch := range f.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				frag := chatToolFragment{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments}
				if tc.Index != nil {
					frag.Index, frag.HasIndex = *tc.Index, true
				}
				out = append(out, frag)
			}
		}
	}
	return out
}

// jsonUpstream 原样返回一份 JSON 响应体，并记录上游收到的路径与报文。
func jsonUpstream(t *testing.T, payload string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.set(r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// postStreamBody 用给定请求体打开一条流，返回状态码与客户端收到的全部字节。
func postStreamBody(t *testing.T, srv *httptest.Server, inbound, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+inboundPath(inbound), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// anthropicToolUseUpstream 是一条完整的 anthropic 流：文本块加两个并行工具块，
// 工具块号是 1 与 2（有文本块时 anthropic 不从 0 编号）。
const anthropicParallelToolsSSE = "" +
	"event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"稍等"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"weather","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"clock","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"zone\":\"UTC\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":2}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// TestChatToolResultsToAnthropicUpstream 覆盖工具结果进 anthropic 出站：chat 客户端的三条
// role=tool 消息编成 anthropic 请求时，必须折叠成一条 role=user 消息里的 tool_result 块，
// 上游不能看到 role=tool（anthropic 会以 400 拒绝），tool_use_id 与 assistant 的 tool_use 对应。
func TestChatToolResultsToAnthropicUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":64,"messages":[` +
		`{"role":"user","content":"北京天气怎么样"},` +
		`{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}},` +
		`{"id":"call_2","type":"function","function":{"name":"clock","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"晴 25 度"},` +
		`{"role":"tool","tool_call_id":"call_2","content":"08:00"}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if rec.path != "/v1/messages" {
		t.Fatalf("上游路径 = %q，期望 /v1/messages", rec.path)
	}
	if strings.Contains(rec.body, `"role":"tool"`) {
		t.Fatalf("发往 anthropic 的报文里不该有 role=tool: %s", rec.body)
	}

	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("消息条数 = %d，期望 3（user、assistant、合并后的一条 user）: %s", len(sent.Messages), rec.body)
	}
	if sent.Messages[0].Role != "user" || sent.Messages[1].Role != "assistant" {
		t.Fatalf("前两条消息的角色 = %q、%q，期望 user、assistant", sent.Messages[0].Role, sent.Messages[1].Role)
	}
	if got := sent.Messages[2].Role; got != "user" {
		t.Fatalf("工具结果那条消息的角色 = %q，期望 user", got)
	}

	assistant := sent.Messages[1].Content
	if len(assistant) != 2 || assistant[0].Type != "tool_use" || assistant[1].Type != "tool_use" {
		t.Fatalf("assistant 的内容块 = %+v，期望两个 tool_use", assistant)
	}
	if assistant[0].ID != "call_1" || assistant[0].Name != "weather" {
		t.Fatalf("第一个 tool_use = %s/%s，期望 call_1/weather", assistant[0].ID, assistant[0].Name)
	}
	if got := string(assistant[0].Input); got != `{"city":"北京"}` {
		t.Fatalf("第一个 tool_use 的 input = %s，期望上游原样的 {\"city\":\"北京\"}", got)
	}
	if assistant[1].ID != "call_2" || assistant[1].Name != "clock" {
		t.Fatalf("第二个 tool_use = %s/%s，期望 call_2/clock", assistant[1].ID, assistant[1].Name)
	}

	results := sent.Messages[2].Content
	if len(results) != 2 {
		t.Fatalf("user 消息里的内容块 = %+v，期望两个 tool_result", results)
	}
	if results[0].Type != "tool_result" || results[1].Type != "tool_result" {
		t.Fatalf("内容块类型 = %q、%q，期望 tool_result", results[0].Type, results[1].Type)
	}
	if results[0].ToolUseID != "call_1" || results[1].ToolUseID != "call_2" {
		t.Fatalf("tool_use_id = %q、%q，期望 call_1、call_2（顺序不能换）", results[0].ToolUseID, results[1].ToolUseID)
	}
	if got := string(results[0].Content); got != `"晴 25 度"` {
		t.Fatalf("第一个 tool_result 的 content = %s，期望 \"晴 25 度\"", got)
	}
	if got := string(results[1].Content); got != `"08:00"` {
		t.Fatalf("第二个 tool_result 的 content = %s，期望 \"08:00\"", got)
	}
}

// TestAnthropicToolUseResponseToChatClient 覆盖响应侧：anthropic 上游的 tool_use 块转回
// chat 时，tool_calls 的 id、name、arguments 与上游一致，结束原因是 tool_calls。
func TestAnthropicToolUseResponseToChatClient(t *testing.T) {
	f := newFixture(t)
	const upstreamBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"北京"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":7}}`
	up, _ := jsonUpstream(t, upstreamBody)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postTo(t, srv, requestBodies[gateway.ProtocolChat], gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，%s", err, body)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices 条数 = %d，期望 1", len(out.Choices))
	}
	if got := out.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish_reason = %q，期望 tool_calls", got)
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v，期望 1 个", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Type != "function" || calls[0].Function.Name != "weather" {
		t.Fatalf("tool_calls[0] = %+v，期望 id=toolu_1、type=function、name=weather", calls[0])
	}
	if got := calls[0].Function.Arguments; got != `{"city":"北京"}` {
		t.Fatalf("arguments = %q，期望与上游 input 一致", got)
	}
}

// TestAnthropicParallelToolsStreamToChatDenseIndex 覆盖 chat 客户端看到的工具序号：anthropic
// 上游的块号是 1 与 2（前面有文本块），chat 的 tool_calls[].index 必须从 0 连续编号，
// 每个 id 归自己的序号，参数分片拼起来是一个合法 JSON。
func TestAnthropicParallelToolsStreamToChatDenseIndex(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, anthropicParallelToolsSSE)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"m1"}, 100)
	srv := f.server(t)

	body := strings.Replace(requestBodies[gateway.ProtocolChat], `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	status, got := postStreamBody(t, srv, gateway.ProtocolChat, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	frames := chatFrames(t, got)
	frags := chatToolFragments(t, frames)
	if len(frags) != 5 {
		t.Fatalf("tool_calls 分片数 = %d，期望 5（两个开始帧、三个参数帧）: %+v", len(frags), frags)
	}
	// 上游块号 1、2 依次落到 chat 的 0、1：序号稠密、从 0 开始，没有空位。
	wantIdx := []int{0, 0, 0, 1, 1}
	for i, f := range frags {
		if !f.HasIndex {
			t.Fatalf("第 %d 个分片没有 index: %+v", i+1, f)
		}
		if f.Index != wantIdx[i] {
			t.Fatalf("分片序号 = %v，期望 %v（从 0 连续编号）", fragIndices(frags), wantIdx)
		}
	}
	if frags[0].ID != "toolu_1" || frags[0].Name != "weather" {
		t.Fatalf("第 1 个分片 = %s/%s，期望 toolu_1/weather", frags[0].ID, frags[0].Name)
	}
	if frags[3].ID != "toolu_2" || frags[3].Name != "clock" {
		t.Fatalf("第 4 个分片 = %s/%s，期望 toolu_2/clock（id 不能串到另一个序号上）", frags[3].ID, frags[3].Name)
	}

	// 每个序号的参数分片拼起来是完整、合法的 JSON。
	gotArgs := map[int]string{}
	for _, f := range frags {
		gotArgs[f.Index] += f.Args
	}
	if a := gotArgs[0]; a != `{"city":"北京"}` || !json.Valid([]byte(a)) {
		t.Fatalf("序号 0 的参数 = %q，期望合法的 {\"city\":\"北京\"}", a)
	}
	if a := gotArgs[1]; a != `{"zone":"UTC"}` || !json.Valid([]byte(a)) {
		t.Fatalf("序号 1 的参数 = %q，期望合法的 {\"zone\":\"UTC\"}", a)
	}

	// 正文仍然完整，结束原因是 tool_calls。
	var text strings.Builder
	var finish string
	for _, f := range frames {
		for _, ch := range f.Choices {
			if ch.Delta.Content != nil {
				text.WriteString(*ch.Delta.Content)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
	}
	if got := text.String(); got != "稍等" {
		t.Fatalf("正文 = %q，期望 稍等", got)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q，期望 tool_calls", finish)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// fragIndices 取分片序号列表，报错文案里用。
func fragIndices(frags []chatToolFragment) []int {
	out := make([]int, 0, len(frags))
	for _, f := range frags {
		if f.HasIndex {
			out = append(out, f.Index)
		}
	}
	return out
}

// chatToolStreamSSE 是一条 chat 上游的工具调用流：一个工具，参数分两片。
const chatToolStreamSSE = "" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"稍等"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8}}` + "\n\n" +
	`data: [DONE]` + "\n\n"

// TestChatToolStreamToResponsesInboundDoneEvents 覆盖 responses 客户端的工具收尾：chat 上游
// 的工具流编成 responses 事件时，收尾前必须有 function_call_arguments.done 与
// output_item.done，response.completed 的 output 里要有完整的 function_call——
// 只发 added 与 delta 的流会让客户端拿不到工具调用。
func TestChatToolStreamToResponsesInboundDoneEvents(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, chatToolStreamSSE)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-chat", []string{"m1"}, 100)
	srv := f.server(t)

	body := strings.Replace(requestBodies[gateway.ProtocolResponses], `"max_tokens":16`, `"max_output_tokens":16,"stream":true`, 1)
	status, got := postStreamBody(t, srv, gateway.ProtocolResponses, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	want := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.output_item.done",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	events := responsesEvents(t, got)
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", events, want)
	}

	var done struct {
		OutputIndex int    `json:"output_index"`
		ItemID      string `json:"item_id"`
		Name        string `json:"name"`
		Arguments   string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "response.function_call_arguments.done")), &done); err != nil {
		t.Fatalf("done 事件的 data 不是合法 JSON: %v", err)
	}
	if done.Name != "weather" || done.Arguments != `{"city":"北京"}` || done.ItemID != "fc_call_1" {
		t.Fatalf("function_call_arguments.done = %+v，期望 name=weather、完整参数、item_id=fc_call_1", done)
	}

	var itemDone struct {
		Item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "response.output_item.done")), &itemDone); err != nil {
		t.Fatalf("output_item.done 的 data 不是合法 JSON: %v", err)
	}
	if itemDone.Item.Type != "function_call" || itemDone.Item.CallID != "call_1" ||
		itemDone.Item.Name != "weather" || itemDone.Item.Status != "completed" {
		t.Fatalf("output_item.done 的 item = %+v，期望 function_call/call_1/weather/completed", itemDone.Item)
	}
	if itemDone.Item.Arguments != `{"city":"北京"}` {
		t.Fatalf("output_item.done 的参数 = %q，期望完整参数", itemDone.Item.Arguments)
	}

	var completed struct {
		Response struct {
			Status string `json:"status"`
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "response.completed")), &completed); err != nil {
		t.Fatalf("response.completed 的 data 不是合法 JSON: %v", err)
	}
	if completed.Response.Status != "completed" {
		t.Fatalf("response.completed 的 status = %q，期望 completed", completed.Response.Status)
	}
	if len(completed.Response.Output) != 2 {
		t.Fatalf("output 条数 = %d，期望 2（文本 item 与回填的那个 function_call）", len(completed.Response.Output))
	}
	// 文本 item 排在前面：正文整段落在 message 的 content 里，序号与事件里的 output_index 一致。
	if msg := completed.Response.Output[0]; msg.Type != "message" {
		t.Fatalf("output[0] = %+v，期望文本 item", msg)
	}
	got0 := completed.Response.Output[1]
	if got0.Type != "function_call" || got0.CallID != "call_1" || got0.Name != "weather" {
		t.Fatalf("output[1] = %+v，期望 function_call/call_1/weather", got0)
	}
	if got0.Arguments != `{"city":"北京"}` {
		t.Fatalf("output[1] 的参数 = %q，期望完整的 {\"city\":\"北京\"}", got0.Arguments)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// TestChatToolStreamToAnthropicClientToolUseBlocks 覆盖 anthropic 客户端看到的工具调用：
// chat 上游的 tool_calls 分片编成 content_block_start(tool_use) 与 input_json_delta，
// id 与函数名只在起始帧里出现，参数分片拼起来是合法 JSON，结束原因是 tool_use。
func TestChatToolStreamToAnthropicClientToolUseBlocks(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, chatToolStreamSSE)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-chat", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	type contentBlockStart struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	var toolStart contentBlockStart
	startIndex := -1
	var args strings.Builder
	for _, fr := range sseFrames(t, got) {
		switch fr.event {
		case "content_block_start":
			var block contentBlockStart
			if err := json.Unmarshal([]byte(fr.data), &block); err != nil {
				t.Fatalf("content_block_start 的 data 不是合法 JSON: %v", err)
			}
			if block.ContentBlock.Type != "tool_use" {
				continue
			}
			toolStart, startIndex = block, block.Index
		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(fr.data), &delta); err != nil {
				t.Fatalf("content_block_delta 的 data 不是合法 JSON: %v", err)
			}
			if delta.Delta.Type == "input_json_delta" {
				args.WriteString(delta.Delta.PartialJSON)
			}
		}
	}
	if startIndex < 0 {
		t.Fatalf("流里没有 tool_use 块的起始帧:\n%s", tail(got, 300))
	}
	if toolStart.ContentBlock.ID != "call_1" || toolStart.ContentBlock.Name != "weather" {
		t.Fatalf("tool_use 块 = %s/%s，期望 call_1/weather", toolStart.ContentBlock.ID, toolStart.ContentBlock.Name)
	}
	if gotArgs := args.String(); gotArgs != `{"city":"北京"}` || !json.Valid([]byte(gotArgs)) {
		t.Fatalf("拼起来的参数 = %q，期望合法的 {\"city\":\"北京\"}", gotArgs)
	}

	var delta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "message_delta")), &delta); err != nil {
		t.Fatalf("message_delta 的 data 不是合法 JSON: %v", err)
	}
	if delta.Delta.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q，期望 tool_use", delta.Delta.StopReason)
	}
	if events := anthropicEvents(t, got); events[len(events)-1] != "message_stop" {
		t.Fatalf("流应以 message_stop 收尾，事件序列 = %v", events)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// TestResponsesDoneOnlyStreamKeepsToolCall 覆盖只发 output_item.done 的 responses 上游：
// 整条工具调用只能在 done 里补齐，不能重复发；结束原因仍是 tool_use，客户端因此能看到
// 与「用了工具」对应的 tool_calls，而不是一个没有工具调用的 tool_calls 结束原因。
func TestResponsesDoneOnlyStreamKeepsToolCall(t *testing.T) {
	f := newFixture(t)
	upstream := "" +
		`data: {"type":"response.created","response":{"id":"r1","model":"up"}}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"weather","arguments":"{\"city\":\"北京\"}"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":12,"output_tokens":7}}}` + "\n\n"
	up := fixedSSEUpstream(t, upstream)
	f.add(t, "resp", gateway.ProtocolResponses, up.URL, "sk-resp", []string{"m1"}, 100)
	srv := f.server(t)

	body := strings.Replace(requestBodies[gateway.ProtocolChat], `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	status, got := postStreamBody(t, srv, gateway.ProtocolChat, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}

	frames := chatFrames(t, got)
	frags := chatToolFragments(t, frames)
	if len(frags) != 2 {
		t.Fatalf("tool_calls 分片数 = %d，期望 2（一个开始、一个参数，不能重复）: %+v", len(frags), frags)
	}
	if frags[0].Index != 0 || frags[0].ID != "call_9" || frags[0].Name != "weather" {
		t.Fatalf("第 1 个分片 = %+v，期望序号 0、id=call_9、name=weather", frags[0])
	}
	if frags[1].Index != 0 || frags[1].Args != `{"city":"北京"}` {
		t.Fatalf("第 2 个分片 = %+v，期望序号 0 且参数为完整 JSON", frags[1])
	}
	var finish string
	for _, f := range frames {
		for _, ch := range f.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q，期望 tool_calls（结束原因说有工具调用，就必须有对应的 tool_calls）", finish)
	}
}

// TestToolArgsCutToResponsesInboundEndsFailed 覆盖 responses 客户端的断流：工具参数发到
// 一半上游就断开，客户端收到 error 事件与 response.failed，文案说明参数不完整；
// 已转发的参数片段确实解析不出 JSON，前端解析失败是可预期的。
func TestToolArgsCutToResponsesInboundEndsFailed(t *testing.T) {
	// 停在工具参数中途：帧本身完整，但没有 finish 帧也没有 [DONE]。
	written := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"稍等"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北"}}]}}]}`,
		``,
	}, "\n")

	f := newFixture(t)
	up := closedSSEUpstream(t, written)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-cut-resp", []string{"m1"}, 100)
	srv := f.server(t)

	body := strings.Replace(requestBodies[gateway.ProtocolResponses], `"max_tokens":16`, `"max_output_tokens":16,"stream":true`, 1)
	status, got := postStreamBody(t, srv, gateway.ProtocolResponses, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（响应头已经发出）", status)
	}

	// 参数片段照原样转发：客户端拿到的就是上游中断时的那一截。
	args := ""
	for _, e := range sseFrames(t, got) {
		if !strings.Contains(e.data, `"response.function_call_arguments.delta"`) {
			continue
		}
		var delta struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(e.data), &delta); err != nil {
			t.Fatalf("参数帧不是合法 JSON: %v，%q", err, e.data)
		}
		args += delta.Delta
	}
	if args != `{"city":"北` {
		t.Fatalf("已转发的参数 = %q，期望上游中断时的那一截", args)
	}
	if json.Valid([]byte(args)) {
		t.Fatalf("这一截参数不该是合法 JSON，收到 %q 说明转发被补齐过", args)
	}

	events := responsesEvents(t, got)
	if n := len(events); n < 2 || events[n-2] != "error" || events[n-1] != "response.failed" {
		t.Fatalf("事件序列的结尾 = %v，期望 error 之后紧接 response.failed", events)
	}
	var errEvent struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "error")), &errEvent); err != nil {
		t.Fatalf("error 事件的 data 不是合法 JSON: %v", err)
	}
	if !strings.Contains(errEvent.Message, "内容不完整") {
		t.Fatalf("error 事件的说明 = %q，期望说明工具参数不完整", errEvent.Message)
	}

	var failed struct {
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "response.failed")), &failed); err != nil {
		t.Fatalf("response.failed 的 data 不是合法 JSON: %v", err)
	}
	if failed.Response.Status != "failed" || !strings.Contains(failed.Response.Error.Message, "内容不完整") {
		t.Fatalf("response.failed = %+v，期望 status=failed 且说明参数不完整", failed.Response)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
	if !strings.Contains(recs[0].Error, "收尾帧") {
		t.Fatalf("请求记录的 error = %q，期望写明收尾帧没到", recs[0].Error)
	}
}

// TestToolArgsCutToChatInboundEndsWithErrorFrame 覆盖 chat 客户端的断流：anthropic 上游
// 停在 input_json_delta 之后，客户端收到的 tool_calls 参数是半截，随后是一帧错误说明
// 参数不完整，最后仍是 [DONE]；这一次记 ok=0。
func TestToolArgsCutToChatInboundEndsWithErrorFrame(t *testing.T) {
	written := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"usage":{"input_tokens":9,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"北"}}`,
		``,
	}, "\n")

	f := newFixture(t)
	up := closedSSEUpstream(t, written)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-cut-chat", []string{"m1"}, 100)
	srv := f.server(t)

	body := strings.Replace(requestBodies[gateway.ProtocolChat], `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	status, got := postStreamBody(t, srv, gateway.ProtocolChat, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（响应头已经发出）", status)
	}

	frames := chatFrames(t, got)
	frags := chatToolFragments(t, frames)
	if len(frags) != 2 {
		t.Fatalf("tool_calls 分片数 = %d，期望 2（开始 + 半截参数）: %+v", len(frags), frags)
	}
	if frags[0].ID != "toolu_1" || frags[0].Name != "weather" {
		t.Fatalf("第 1 个分片 = %+v，期望 id=toolu_1、name=weather", frags[0])
	}
	if frags[1].Args != `{"city":"北` {
		t.Fatalf("已转发的参数 = %q，期望上游中断时的那一截", frags[1].Args)
	}
	if json.Valid([]byte(frags[1].Args)) {
		t.Fatalf("这一截参数不该是合法 JSON，收到 %q 说明转发被补齐过", frags[1].Args)
	}

	kind, message := chatErrorFrame(t, got)
	if kind != "upstream_error" {
		t.Fatalf("错误帧的类型 = %q，期望 upstream_error", kind)
	}
	if !strings.Contains(message, "内容不完整") {
		t.Fatalf("错误帧的说明 = %q，期望说明工具参数不完整", message)
	}
	// 断流之后仍要收尾：[DONE] 在最后一帧，客户端不会一直等。
	if last := sseFrames(t, got); last[len(last)-1].data != "[DONE]" {
		t.Fatalf("流应以 data: [DONE] 收尾，实际结尾 %q", tail(got, 60))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
}

// TestAnthropicBuiltinToolDroppedOnChatUpstream 覆盖服务端内置工具编到 chat：入站定义的
// web_search_20250305 没有 chat 的对应形状，整条丢弃；客户端自己的函数照旧保留，
// 不伪造一个 function 出来。
func TestAnthropicBuiltinToolDroppedOnChatUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-chat", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"查一下"}]` +
		`,"tools":[{"type":"web_search_20250305","name":"web_search"},` +
		`{"name":"weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	if strings.Contains(rec.body, "web_search") {
		t.Fatalf("内置工具不该出现在 chat 的请求体里: %s", rec.body)
	}
	var sent struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Tools) != 1 {
		t.Fatalf("tools 条数 = %d，期望 1（只剩客户端函数）: %s", len(sent.Tools), rec.body)
	}
	if sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "weather" {
		t.Fatalf("tools[0] = %+v，期望 type=function、name=weather", sent.Tools[0])
	}
}

// TestAnthropicBuiltinToolPreservedOnPassthrough 覆盖 anthropic 直通：入站的内置工具定义
// 原样带到上游，type 不丢，也不补一个空的 input_schema。
func TestAnthropicBuiltinToolPreservedOnPassthrough(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"查一下"}]` +
		`,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	var sent struct {
		Model string `json:"model"`
		Tools []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			MaxUses     int             `json:"max_uses"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Tools) != 1 {
		t.Fatalf("tools 条数 = %d，期望 1: %s", len(sent.Tools), rec.body)
	}
	if sent.Tools[0].Type != "web_search_20250305" || sent.Tools[0].Name != "web_search" {
		t.Fatalf("tools[0] = %+v，期望 type 原样保留", sent.Tools[0])
	}
	if sent.Tools[0].MaxUses != 3 {
		t.Fatalf("max_uses = %d，期望直通原样保留 3", sent.Tools[0].MaxUses)
	}
	if len(sent.Tools[0].InputSchema) != 0 {
		t.Fatalf("内置工具不该被补上 input_schema，实际 %s", sent.Tools[0].InputSchema)
	}
}
