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

// TestEncodeAnthropicRequestFoldsToolResults 确认 RoleTool 消息编成 user 加 tool_result 块：
// anthropic 没有 role=tool，原样写过去会被上游以 400 拒绝。
func TestEncodeAnthropicRequestFoldsToolResults(t *testing.T) {
	req := &Request{
		Model: "claude-x",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "查两个城市"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "toolu_1", Name: "weather", Input: json.RawMessage(`{"city":"北京"}`)}},
				{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "toolu_2", Name: "weather", Input: json.RawMessage(`{"city":"上海"}`)}},
			}},
			{Role: RoleTool, Blocks: []Block{
				{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "toolu_1", Text: "晴"}},
			}},
			{Role: RoleTool, Blocks: []Block{
				{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "toolu_2", Text: "雨", IsError: true}},
			}},
		},
	}

	raw, err := EncodeAnthropicRequest(req, "")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	if strings.Contains(string(raw), `"role":"tool"`) {
		t.Fatalf("报文里不该出现 role=tool: %s", raw)
	}

	var wire AnthropicRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3（user、assistant、合并后的 user）", len(wire.Messages))
	}
	results := wire.Messages[2]
	if results.Role != "user" {
		t.Fatalf("工具结果消息的角色 = %q，期望 user", results.Role)
	}
	if len(results.Content.Blocks) != 2 {
		t.Fatalf("工具结果块数 = %d，期望两条并在同一条 user 消息里", len(results.Content.Blocks))
	}
	first, second := results.Content.Blocks[0], results.Content.Blocks[1]
	if first.Type != "tool_result" || first.ToolUseID != "toolu_1" || string(first.Content) != `"晴"` {
		t.Fatalf("第一个 tool_result = %+v", first)
	}
	if second.Type != "tool_result" || second.ToolUseID != "toolu_2" || string(second.Content) != `"雨"` {
		t.Fatalf("第二个 tool_result = %+v", second)
	}
	// is_error 只有 anthropic 有落点，从 anthropic 解出来再编回去要保住。
	if !second.IsError {
		t.Fatalf("is_error 丢了: %+v", second)
	}
}

// TestEncodeAnthropicRequestDropsEmptyMessages 确认编码后一块内容都不剩的消息整条不写出去：
// anthropic 拒绝空的 content 数组，一条只剩无签名思考块的 assistant 消息就是这么变空的。
// 丢弃之后上下两条 user 消息并成一条，块顺序不变；全部消息都被丢弃时编码报错，
// 不写出空的 messages。
func TestEncodeAnthropicRequestDropsEmptyMessages(t *testing.T) {
	req := &Request{
		Model: "claude-x",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "6 乘 7 是多少"}}},
			{Role: RoleAssistant, Blocks: []Block{
				// 没有 signature 的思考块编不成 anthropic 的块，整条消息因此变空。
				{Kind: BlockThinking, Thinking: &Thinking{Text: "先算 6×7=42。"}},
			}},
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "那 8 乘 9 呢"}}},
		},
	}

	raw, err := EncodeAnthropicRequest(req, "")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	if strings.Contains(string(raw), `"content":[]`) {
		t.Fatalf("报文里不该出现空的 content 数组: %s", raw)
	}
	var wire AnthropicRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if len(wire.Messages) != 1 {
		t.Fatalf("消息数 = %d，期望 1（两条 user 并成一条，只剩思考的 assistant 整条丢弃）: %s", len(wire.Messages), raw)
	}
	merged := wire.Messages[0]
	if merged.Role != "user" || len(merged.Content.Blocks) != 2 {
		t.Fatalf("合并后的消息 = %+v，期望一条两个文本块的 user 消息", merged)
	}
	if merged.Content.Blocks[0].Text != "6 乘 7 是多少" || merged.Content.Blocks[1].Text != "那 8 乘 9 呢" {
		t.Fatalf("块顺序 = %+v，期望按原顺序", merged.Content.Blocks)
	}

	// 全部消息都被丢弃时自己报错：上游的 messages 至少一条，写出空数组只会换回一个
	// 指不到原因上的 400。
	if _, err := EncodeAnthropicRequest(&Request{Model: "claude-x", Messages: req.Messages[1:2]}, ""); err == nil {
		t.Fatal("所有消息都变空时应报错")
	}
}

// TestEncodeAnthropicRequestKeepsSignedThinkingOnlyAssistant 确认一条只剩 signed 思考块的
// assistant 消息编回 anthropic 时整条保住：anthropic 是唯一要求回填思考块的协议，
// 带 signature 的块不丢，块前面的用户消息与后面的追问也不会被并到别处。
func TestEncodeAnthropicRequestKeepsSignedThinkingOnlyAssistant(t *testing.T) {
	req := &Request{
		Model: "claude-x",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "6 乘 7 是多少"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockThinking, Thinking: &Thinking{Text: "用户问乘法，先算 6×7=42。", Signature: "sig-abc"}},
			}},
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "那 8 乘 9 呢"}}},
		},
	}

	raw, err := EncodeAnthropicRequest(req, "")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	var wire AnthropicRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3（user、只剩思考的 assistant、user）: %s", len(wire.Messages), raw)
	}
	for i, want := range []string{"user", "assistant", "user"} {
		if wire.Messages[i].Role != want {
			t.Fatalf("第 %d 条消息的角色 = %q，期望 %q: %s", i+1, wire.Messages[i].Role, want, raw)
		}
	}
	mid := wire.Messages[1].Content.Blocks
	if len(mid) != 1 {
		t.Fatalf("assistant 的块数 = %d，期望 1（带签名的思考块）: %s", len(mid), raw)
	}
	if mid[0].Type != "thinking" || mid[0].Thinking != "用户问乘法，先算 6×7=42。" || mid[0].Signature != "sig-abc" {
		t.Fatalf("思考块 = %+v，期望带 sig-abc 的 thinking 块", mid[0])
	}
	if len(wire.Messages[0].Content.Blocks) != 1 || wire.Messages[0].Content.Blocks[0].Text != "6 乘 7 是多少" {
		t.Fatalf("第一条用户消息 = %+v", wire.Messages[0].Content.Blocks)
	}
	if len(wire.Messages[2].Content.Blocks) != 1 || wire.Messages[2].Content.Blocks[0].Text != "那 8 乘 9 呢" {
		t.Fatalf("最后一条用户消息 = %+v（追问不能被并进思考那条）", wire.Messages[2].Content.Blocks)
	}
}

// TestAnthropicToolTypeRoundTrip 确认服务端内置工具的 type 编回 anthropic 时保留，
// 不会被补上一个伪造的 input_schema。
func TestAnthropicToolTypeRoundTrip(t *testing.T) {
	decoded, err := DecodeAnthropicRequest([]byte(`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"},
		         {"name":"get_weather","input_schema":{"type":"object"}}]}`))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if len(decoded.Tools) != 2 {
		t.Fatalf("tools = %+v", decoded.Tools)
	}
	if decoded.Tools[0].Type != "web_search_20250305" {
		t.Fatalf("内置工具的 type = %q，期望保留", decoded.Tools[0].Type)
	}
	if len(decoded.Tools[0].Schema) != 0 {
		t.Fatalf("内置工具不该补 input_schema，实际 %s", decoded.Tools[0].Schema)
	}

	raw, err := EncodeAnthropicRequest(decoded, "")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	var wire AnthropicRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if len(wire.Tools) != 2 || wire.Tools[0].Type != "web_search_20250305" {
		t.Fatalf("编回 anthropic 的 tools = %+v", wire.Tools)
	}
	if strings.Contains(string(raw), `"input_schema":null`) {
		t.Fatalf("内置工具不该写出 input_schema: %s", raw)
	}
	if string(wire.Tools[1].InputSchema) != `{"type":"object"}` {
		t.Fatalf("客户端工具的 input_schema = %s", wire.Tools[1].InputSchema)
	}
}

// TestAnthropicStreamEncoderKeepsDeltasOnOpenBlocks 确认思考增量与工具参数交替时，
// 每个 content_block_delta 都落在未关闭的块上：块被关掉之后要重开一个新块，不能往
// 已经发过 content_block_stop 的块继续写。
func TestAnthropicStreamEncoderKeepsDeltasOnOpenBlocks(t *testing.T) {
	enc := newAnthropicStreamEncoder("claude-x")
	events := enc.Start()
	deltas := []Delta{
		{Kind: DeltaToolStart, Index: 1, ToolID: "toolu_1", ToolName: "weather"},
		{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{"city":`},
		{Kind: DeltaThinking, Text: "再确认一下城市名"},
		{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `"北京"}`},
		{Kind: DeltaToolStart, Index: 2, ToolID: "toolu_2", ToolName: "weather"},
		{Kind: DeltaToolArgs, Index: 2, ArgsJSON: `{"city":"上海"}`},
		{Kind: DeltaStop, StopReason: StopToolUse},
	}
	for _, d := range deltas {
		events = append(events, enc.Emit(d)...)
	}
	events = append(events, enc.Finish()...)

	open := map[int]bool{}
	toolBlocks := map[int]string{}
	for i, e := range events {
		var frame struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				ID string `json:"id"`
			} `json:"content_block"`
		}
		if e.Event == "" || json.Unmarshal(e.Data, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "content_block_start":
			if open[frame.Index] {
				t.Fatalf("第 %d 个事件重开了块 %d", i+1, frame.Index)
			}
			open[frame.Index] = true
			if frame.ContentBlock.ID != "" {
				toolBlocks[frame.Index] = frame.ContentBlock.ID
			}
		case "content_block_delta":
			if !open[frame.Index] {
				t.Fatalf("第 %d 个事件往已关闭的块 %d 发增量", i+1, frame.Index)
			}
		case "content_block_stop":
			if !open[frame.Index] {
				t.Fatalf("第 %d 个事件停了没开的块 %d", i+1, frame.Index)
			}
			delete(open, frame.Index)
		}
	}
	if len(open) != 0 {
		t.Fatalf("收尾之后还有块开着: %v", open)
	}

	// toolu_1 的块因思考增量被关过一次，重开后是另一个块号，id 保持原样。
	want := map[int]string{0: "toolu_1", 1: "", 2: "toolu_1", 3: "toolu_2"}
	if len(toolBlocks) != 3 {
		t.Fatalf("工具块 = %v", toolBlocks)
	}
	for idx, id := range want {
		if id == "" {
			continue
		}
		if got := toolBlocks[idx]; got != id {
			t.Fatalf("块 %d 的 id = %q，期望 %q", idx, got, id)
		}
	}
}

// TestAnthropicStreamEncoderTruncatedToolArgs 确认断流时错误事件说明工具参数不完整。
func TestAnthropicStreamEncoderTruncatedToolArgs(t *testing.T) {
	enc := newAnthropicStreamEncoder("claude-x")
	enc.Start()
	enc.Emit(Delta{Kind: DeltaToolStart, Index: 1, ToolID: "toolu_1", ToolName: "weather"})
	enc.Emit(Delta{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{"city":"北`})

	events := enc.Emit(Delta{Kind: DeltaError, Text: "上游流式响应中断: 收尾帧之前连接就结束了"})
	if len(events) != 1 || events[0].Event != "error" {
		t.Fatalf("错误事件 = %+v", events)
	}
	var frame struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(events[0].Data, &frame); err != nil {
		t.Fatalf("错误事件不是合法 JSON: %v", err)
	}
	if !strings.Contains(frame.Error.Message, "工具调用参数在上游中断") {
		t.Fatalf("错误事件文案 = %q，期望说明工具参数不完整", frame.Error.Message)
	}
}
