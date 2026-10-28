package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeAnthropicRequestToIR(t *testing.T) {
	raw := `{
	  "model": "claude-x",
	  "max_tokens": 256,
	  "system": [{"type": "text", "text": "你是助手"}],
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "text", "text": "看图"},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "上海"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴"}
	    ]}
	  ],
	  "tools": [{"name": "get_weather", "description": "天气", "input_schema": {"type": "object"}}],
	  "tool_choice": {"type": "any", "disable_parallel_tool_use": true},
	  "stop_sequences": ["STOP"],
	  "temperature": 0.2,
	  "stream": true
	}`

	req, err := DecodeAnthropicRequest([]byte(raw))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if req.Model != "claude-x" || req.MaxTokens != 256 || !req.Stream {
		t.Fatalf("基础字段 = %+v", req)
	}
	if len(req.System) != 1 || req.System[0].Text != "你是助手" {
		t.Fatalf("system = %+v", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d", len(req.Messages))
	}

	user := req.Messages[0]
	if user.Role != RoleUser || len(user.Blocks) != 2 {
		t.Fatalf("第一条消息 = %+v", user)
	}
	if user.Blocks[0].Kind != BlockText || user.Blocks[0].Text != "看图" {
		t.Fatalf("文本块 = %+v", user.Blocks[0])
	}
	if user.Blocks[1].Kind != BlockImage || user.Blocks[1].Image.URL != "data:image/png;base64,QUJD" {
		t.Fatalf("图片块 = %+v", user.Blocks[1])
	}

	assistant := req.Messages[1]
	if assistant.Role != RoleAssistant || assistant.Blocks[0].Kind != BlockToolUse {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant.Blocks[0].ToolUse.ID != "toolu_1" || assistant.Blocks[0].ToolUse.Name != "get_weather" {
		t.Fatalf("tool_use = %+v", assistant.Blocks[0].ToolUse)
	}
	if string(assistant.Blocks[0].ToolUse.Input) != `{"city": "上海"}` &&
		string(assistant.Blocks[0].ToolUse.Input) != `{"city":"上海"}` {
		t.Fatalf("tool input = %s", assistant.Blocks[0].ToolUse.Input)
	}

	toolMsg := req.Messages[2]
	if toolMsg.Blocks[0].Kind != BlockToolResult || toolMsg.Blocks[0].ToolResult.Text != "晴" {
		t.Fatalf("tool_result = %+v", toolMsg.Blocks[0])
	}

	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ToolChoiceRequired {
		t.Fatalf("tool_choice = %+v", req.ToolChoice)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatal("disable_parallel_tool_use 应映射成 ParallelToolCalls=false")
	}
	if len(req.Stop) != 1 || req.Stop[0] != "STOP" {
		t.Fatalf("stop = %v", req.Stop)
	}
	if req.Temperature == nil || *req.Temperature != 0.2 {
		t.Fatalf("temperature = %v", req.Temperature)
	}
}

func TestDecodeAnthropicRequestRejectsMissingModel(t *testing.T) {
	if _, err := DecodeAnthropicRequest([]byte(`{"max_tokens":1,"messages":[]}`)); err == nil {
		t.Fatal("缺少 model 应报错")
	}
}

func TestEncodeAnthropicRequestFromIR(t *testing.T) {
	temp := 0.3
	req := &Request{
		Model:     "claude-x",
		MaxTokens: 0, // anthropic 要求必填，编码时要补默认值
		System:    []Block{{Kind: BlockText, Text: "系统提示"}},
		Messages: []Message{{
			Role: RoleUser,
			Blocks: []Block{
				{Kind: BlockText, Text: "你好"},
				{Kind: BlockImage, Image: &Image{URL: "data:image/png;base64,QUJD"}},
			},
		}},
		Tools:       []Tool{{Name: "t", Schema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:  &ToolChoice{Mode: ToolChoiceRequired},
		Temperature: &temp,
	}

	raw, err := EncodeAnthropicRequest(req, "upstream-model")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}

	var wire AnthropicRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if wire.Model != "upstream-model" {
		t.Fatalf("model = %q", wire.Model)
	}
	if wire.MaxTokens != 4096 {
		t.Fatalf("max_tokens 应为默认值 4096，实际 %d", wire.MaxTokens)
	}
	if string(wire.System) != `"系统提示"` {
		t.Fatalf("system = %s", wire.System)
	}
	if len(wire.Messages) != 1 || len(wire.Messages[0].Content.Blocks) != 2 {
		t.Fatalf("messages = %+v", wire.Messages)
	}
	img := wire.Messages[0].Content.Blocks[1].Source
	if img == nil || img.Type != "base64" || img.MediaType != "image/png" {
		t.Fatalf("图片 source = %+v", img)
	}
	if wire.ToolChoice == nil || wire.ToolChoice.Type != "any" {
		t.Fatalf("tool_choice = %+v", wire.ToolChoice)
	}
}

func TestAnthropicResponseRoundTrip(t *testing.T) {
	raw := `{
	  "id": "msg_1",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-x",
	  "content": [
	    {"type": "text", "text": "好的"},
	    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "上海"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 12, "output_tokens": 5, "cache_read_input_tokens": 3}
	}`

	resp, err := DecodeAnthropicResponse([]byte(raw), "claude-x")
	if err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[0].Text != "好的" {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	if resp.Blocks[1].Kind != BlockToolUse || resp.Blocks[1].ToolUse.Name != "get_weather" {
		t.Fatalf("tool_use = %+v", resp.Blocks[1])
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop = %q", resp.StopReason)
	}
	if resp.Usage.In != 12 || resp.Usage.Out != 5 || resp.Usage.CacheRead != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	out, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatalf("编码响应: %v", err)
	}
	var wire AnthropicResponse
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if wire.Type != "message" || wire.Role != "assistant" {
		t.Fatalf("wire = %+v", wire)
	}
	if wire.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", wire.StopReason)
	}
	if len(wire.Content) != 2 || wire.Content[1].Type != "tool_use" {
		t.Fatalf("content = %+v", wire.Content)
	}
	if wire.Usage.InputTokens != 12 || wire.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %+v", wire.Usage)
	}
}

func TestAnthropicStreamCodec(t *testing.T) {
	// 上游 anthropic 事件 → Delta
	dec := &anthropicStreamDecoder{}
	frames := []string{
		`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":9,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
	}

	var deltas []Delta
	for _, f := range frames {
		got, err := dec.Decode([]byte(f))
		if err != nil {
			t.Fatalf("解码 %s: %v", f, err)
		}
		deltas = append(deltas, got...)
	}
	deltas = append(deltas, dec.Close()...)

	kinds := make([]string, 0, len(deltas))
	for _, d := range deltas {
		kinds = append(kinds, string(d.Kind))
	}
	want := []string{"text", "tool_start", "tool_args", "stop"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("增量序列 = %v，期望 %v", kinds, want)
	}
	stop := deltas[len(deltas)-1]
	if stop.StopReason != StopToolUse || stop.Usage == nil || stop.Usage.In != 9 || stop.Usage.Out != 4 {
		t.Fatalf("收尾增量 = %+v", stop)
	}

	// Delta → 客户端 anthropic 事件
	enc := newAnthropicStreamEncoder("claude-x")
	var events []SSEEvent
	events = append(events, enc.Start()...)
	for _, d := range deltas {
		events = append(events, enc.Emit(d)...)
	}
	events = append(events, enc.Finish()...)

	var names []string
	for _, e := range events {
		names = append(names, e.Event)
	}
	joined := strings.Join(names, ",")
	for _, wantEvent := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(joined, wantEvent) {
			t.Fatalf("事件里缺少 %s：%s", wantEvent, joined)
		}
	}
}

func TestPathProtocol(t *testing.T) {
	cases := map[string]string{
		"/v1/messages":         ProtocolAnthropic,
		"/v1/chat/completions": ProtocolChat,
		"/v1/responses":        ProtocolResponses,
	}
	for path, want := range cases {
		got, ok := PathProtocol(path)
		if !ok || got != want {
			t.Fatalf("PathProtocol(%q) = %q,%v，期望 %q", path, got, ok, want)
		}
	}
	if _, ok := PathProtocol("/v1/nope"); ok {
		t.Fatal("未知路径应返回 false")
	}
}
