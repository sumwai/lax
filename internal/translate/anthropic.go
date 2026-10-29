package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DecodeAnthropicRequest 把 /v1/messages 的请求体解析成 IR。
func DecodeAnthropicRequest(raw []byte) (*Request, error) {
	var wire AnthropicRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(wire.Model) == "" {
		return nil, errors.New("缺少 model 字段")
	}

	req := &Request{
		Model:       wire.Model,
		MaxTokens:   wire.MaxTokens,
		Temperature: wire.Temperature,
		TopP:        wire.TopP,
		Stop:        wire.StopSequences,
		Stream:      wire.Stream,
	}
	req.System = anthropicSystemBlocks(wire.System)

	for _, m := range wire.Messages {
		msg, err := anthropicMessage(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msg)
	}

	for _, t := range wire.Tools {
		tool := Tool{Type: t.Type, Name: t.Name, Description: t.Description, Schema: t.InputSchema}
		// 只有客户端函数的 schema 缺失时才补空对象：内置工具本来就没有 input_schema，
		// 补一个出来就是伪造上游事实。
		if isClientTool(tool) && len(tool.Schema) == 0 {
			tool.Schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, tool)
	}

	if tc := wire.ToolChoice; tc != nil {
		choice, parallel := fromAnthropicToolChoice(tc)
		req.ToolChoice = choice
		req.ParallelToolCalls = parallel
	}
	return req, nil
}

// EncodeAnthropicRequest 把 IR 编成 /v1/messages 的请求体。model 是上游模型名。
func EncodeAnthropicRequest(r *Request, model string) ([]byte, error) {
	if model == "" {
		model = r.Model
	}
	wire := AnthropicRequest{
		Model:         model,
		MaxTokens:     r.MaxTokens,
		Messages:      make([]AnthropicMessage, 0, len(r.Messages)),
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		StopSequences: r.Stop,
		Stream:        r.Stream,
	}
	// anthropic 要求 max_tokens 必填。
	if wire.MaxTokens <= 0 {
		wire.MaxTokens = 4096
	}

	if sys := joinText(r.System); sys != "" {
		b, err := json.Marshal(sys)
		if err != nil {
			return nil, err
		}
		wire.System = b
	}

	// 工具结果必须并进 user 消息里的 tool_result 块：anthropic 没有 role=tool，
	// 原样写过去会被上游以 400 拒绝。同一个 assistant 消息之后的多条结果并成一条
	// user 消息，顺序不变（DESIGN 5.4）。
	var pendingToolResults *AnthropicMessage
	flushToolResults := func() {
		if pendingToolResults == nil {
			return
		}
		wire.Messages = append(wire.Messages, *pendingToolResults)
		pendingToolResults = nil
	}
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			blocks := toolResultBlocks(m)
			if len(blocks) == 0 {
				continue
			}
			if pendingToolResults == nil {
				pendingToolResults = &AnthropicMessage{Role: "user", Content: Content{Blocks: blocks}}
				continue
			}
			pendingToolResults.Content.Blocks = append(pendingToolResults.Content.Blocks, blocks...)
			continue
		}
		flushToolResults()
		msg, err := toAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		wire.Messages = append(wire.Messages, msg)
	}
	flushToolResults()

	for _, t := range r.Tools {
		wire.Tools = append(wire.Tools, AnthropicTool{
			Type:        t.Type,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
		})
	}

	if r.ToolChoice != nil {
		tc, err := toAnthropicToolChoice(r.ToolChoice, r.ParallelToolCalls)
		if err != nil {
			return nil, err
		}
		wire.ToolChoice = tc
	}
	return json.Marshal(wire)
}

// DecodeAnthropicResponse 把 /v1/messages 的响应解析成 IR。
func DecodeAnthropicResponse(raw []byte, requestedModel string) (*Response, error) {
	var wire AnthropicResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON: %w", err)
	}

	resp := &Response{
		ID:         wire.ID,
		Model:      requestedModel,
		StopReason: normalizeStop(wire.StopReason),
		Usage: Usage{
			In:         wire.Usage.InputTokens,
			Out:        wire.Usage.OutputTokens,
			CacheRead:  wire.Usage.CacheReadInputTokens,
			CacheWrite: wire.Usage.CacheCreationInputTokens,
		},
	}
	if resp.Model == "" {
		resp.Model = wire.Model
	}
	for _, b := range wire.Content {
		switch b.Type {
		case "text":
			resp.Blocks = append(resp.Blocks, Block{Kind: BlockText, Text: b.Text})
		case "thinking":
			if b.Thinking == "" && b.Signature == "" {
				continue
			}
			resp.Blocks = append(resp.Blocks, Block{Kind: BlockThinking, Thinking: &Thinking{
				Text: b.Thinking, Signature: b.Signature,
			}})
		case "redacted_thinking":
			if b.Data == "" {
				continue
			}
			resp.Blocks = append(resp.Blocks, Block{Kind: BlockThinking, Thinking: &Thinking{Redacted: b.Data}})
		case "tool_use":
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			resp.Blocks = append(resp.Blocks, Block{Kind: BlockToolUse, ToolUse: &ToolUse{
				ID: b.ID, Name: b.Name, Input: input,
			}})
		}
	}
	return resp, nil
}

// EncodeAnthropicResponse 把 IR 编成 /v1/messages 的响应体。
func EncodeAnthropicResponse(r *Response) ([]byte, error) {
	wire := AnthropicResponse{
		ID:         firstNonEmpty(r.ID, "msg_switchboard"),
		Type:       "message",
		Role:       "assistant",
		Model:      r.Model,
		Content:    make([]ContentBlock, 0, len(r.Blocks)),
		StopReason: fromNormalizeStop(r.StopReason),
		Usage: AnthropicUsage{
			InputTokens:              r.Usage.In,
			OutputTokens:             r.Usage.Out,
			CacheReadInputTokens:     r.Usage.CacheRead,
			CacheCreationInputTokens: r.Usage.CacheWrite,
		},
	}
	for _, b := range r.Blocks {
		switch b.Kind {
		case BlockText:
			wire.Content = append(wire.Content, ContentBlock{Type: "text", Text: b.Text})
		case BlockThinking:
			if b.Thinking == nil {
				continue
			}
			if blk, ok := anthropicThinkingBlock(*b.Thinking); ok {
				wire.Content = append(wire.Content, blk)
			}
		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			input := b.ToolUse.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			wire.Content = append(wire.Content, ContentBlock{
				Type: "tool_use", ID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: input,
			})
		}
	}
	return json.Marshal(wire)
}

// anthropicStreamDecoder 把 anthropic 的流式事件翻成增量。
type anthropicStreamDecoder struct {
	stopReason string
	inTokens   int64
	outTokens  int64
	done       bool
}

func (d *anthropicStreamDecoder) Decode(data []byte) ([]Delta, error) {
	var env struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Message *AnthropicResponse `json:"message"`
		Usage   *AnthropicUsage    `json:"usage"`
		Error   *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("上游流式帧不是合法 JSON: %w", err)
	}

	switch env.Type {
	case "message_start":
		if env.Message != nil {
			d.inTokens = env.Message.Usage.InputTokens
			d.outTokens = env.Message.Usage.OutputTokens
		}
	case "content_block_start":
		if env.ContentBlock.Type == "tool_use" {
			return []Delta{{
				Kind:     DeltaToolStart,
				Index:    env.Index,
				ToolID:   env.ContentBlock.ID,
				ToolName: env.ContentBlock.Name,
			}}, nil
		}
	case "content_block_delta":
		switch env.Delta.Type {
		case "thinking_delta":
			return []Delta{{Kind: DeltaThinking, Text: env.Delta.Thinking, Index: env.Index}}, nil
		case "signature_delta":
			return []Delta{{Kind: DeltaThinking, Signature: env.Delta.Signature, Index: env.Index}}, nil
		case "text_delta":
			return []Delta{{Kind: DeltaText, Text: env.Delta.Text, Index: env.Index}}, nil
		case "input_json_delta":
			return []Delta{{Kind: DeltaToolArgs, ArgsJSON: env.Delta.PartialJSON, Index: env.Index}}, nil
		}
	case "message_delta":
		if env.Delta.StopReason != "" {
			d.stopReason = normalizeStop(env.Delta.StopReason)
		}
		if env.Usage != nil {
			d.outTokens = env.Usage.OutputTokens
			if env.Usage.InputTokens > 0 {
				d.inTokens = env.Usage.InputTokens
			}
		}
		if d.stopReason != "" {
			d.done = true
			return []Delta{{
				Kind:       DeltaStop,
				StopReason: d.stopReason,
				Usage:      &Usage{In: d.inTokens, Out: d.outTokens},
			}}, nil
		}
	case "error":
		msg := "上游返回错误"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		d.done = true
		return []Delta{{Kind: DeltaError, Text: msg}}, nil
	}
	return nil, nil
}

func (d *anthropicStreamDecoder) Close() []Delta {
	if d.done {
		return nil
	}
	d.done = true
	return []Delta{{
		Kind:       DeltaStop,
		StopReason: normalizeStop(d.stopReason),
		Usage:      &Usage{In: d.inTokens, Out: d.outTokens},
	}}
}

// anthropicStreamEncoder 把增量编成 anthropic 的 SSE 事件。
type anthropicStreamEncoder struct {
	model      string
	started    bool
	nextIndex  int
	openIndex  int
	openKind   BlockKind
	toolBlocks map[int]int
	stopReason string
	usage      Usage
	stopped    bool
}

func newAnthropicStreamEncoder(model string) *anthropicStreamEncoder {
	return &anthropicStreamEncoder{model: model, openIndex: -1, toolBlocks: map[int]int{}}
}

func (e *anthropicStreamEncoder) Start() []SSEEvent {
	if e.started {
		return nil
	}
	e.started = true
	return []SSEEvent{e.event("message_start", map[string]any{
		"type": "message_start",
		"message": AnthropicResponse{
			ID: "msg_switchboard", Type: "message", Role: "assistant",
			Model: e.model, Content: []ContentBlock{},
		},
	})}
}

func (e *anthropicStreamEncoder) Emit(d Delta) []SSEEvent {
	var events []SSEEvent
	switch d.Kind {
	case DeltaThinking:
		// 思考正文与签名分两个 delta 到达，共用一个块；先开块再发增量。
		if d.Text == "" && d.Signature == "" {
			break
		}
		if e.openKind != BlockThinking {
			e.closeBlock(&events)
			e.startBlock(&events, ContentBlock{Type: "thinking"})
		}
		if d.Text != "" {
			events = append(events, e.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.openIndex,
				"delta": map[string]string{"type": "thinking_delta", "thinking": d.Text},
			}))
		}
		if d.Signature != "" {
			events = append(events, e.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": e.openIndex,
				"delta": map[string]string{"type": "signature_delta", "signature": d.Signature},
			}))
		}
	case DeltaText:
		if e.openKind != BlockText {
			e.closeBlock(&events)
			e.startBlock(&events, ContentBlock{Type: "text", Text: ""})
		}
		events = append(events, e.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.openIndex,
			"delta": map[string]string{"type": "text_delta", "text": d.Text},
		}))
	case DeltaToolStart:
		idx, ok := e.toolBlocks[d.Index]
		if !ok {
			e.closeBlock(&events)
			e.startBlock(&events, ContentBlock{
				Type: "tool_use", ID: d.ToolID, Name: d.ToolName, Input: json.RawMessage(`{}`),
			})
			idx = e.openIndex
			e.toolBlocks[d.Index] = idx
		}
		e.openIndex = idx
		e.openKind = BlockToolUse
	case DeltaToolArgs:
		idx, ok := e.toolBlocks[d.Index]
		if !ok {
			// 没收到 start 就来了参数，补一个只有 id 的块，避免丢内容。
			e.closeBlock(&events)
			e.startBlock(&events, ContentBlock{
				Type: "tool_use", ID: fmt.Sprintf("toolu_%d", d.Index), Input: json.RawMessage(`{}`),
			})
			idx = e.openIndex
			e.toolBlocks[d.Index] = idx
		}
		events = append(events, e.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": d.ArgsJSON},
		}))
	case DeltaStop:
		if d.Usage != nil {
			e.usage = *d.Usage
		}
		if d.StopReason != "" {
			e.stopReason = d.StopReason
		}
		events = append(events, e.stopEvents()...)
	case DeltaError:
		events = append(events, ErrorEvent("api_error", withIncompleteToolArgs(d.Text, e.openKind == BlockToolUse)))
	}
	return events
}

func (e *anthropicStreamEncoder) Finish() []SSEEvent {
	if !e.started {
		return nil
	}
	var events []SSEEvent
	if !e.stopped {
		events = append(events, e.stopEvents()...)
	}
	events = append(events, e.event("message_stop", map[string]any{"type": "message_stop"}))
	return events
}

func (e *anthropicStreamEncoder) stopEvents() []SSEEvent {
	if e.stopped {
		return nil
	}
	e.stopped = true
	var events []SSEEvent
	e.closeBlock(&events)
	reason := e.stopReason
	if reason == "" {
		reason = StopEndTurn
	}
	events = append(events, e.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": AnthropicUsage{InputTokens: e.usage.In, OutputTokens: e.usage.Out},
	}))
	return events
}

func (e *anthropicStreamEncoder) startBlock(events *[]SSEEvent, block ContentBlock) {
	idx := e.nextIndex
	e.nextIndex++
	e.openIndex = idx
	e.openKind = BlockKind(block.Type)
	var payload any = block
	switch block.Type {
	case "text":
		payload = map[string]any{"type": "text", "text": ""}
	case "thinking":
		payload = map[string]any{"type": "thinking", "thinking": ""}
	}
	*events = append(*events, e.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": payload,
	}))
}

func (e *anthropicStreamEncoder) closeBlock(events *[]SSEEvent) {
	if e.openIndex < 0 {
		return
	}
	*events = append(*events, e.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.openIndex,
	}))
	e.openIndex = -1
	e.openKind = ""
}

func (e *anthropicStreamEncoder) event(name string, v any) SSEEvent {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"type":"error"}`)
	}
	return SSEEvent{Event: name, Data: b}
}

// anthropicSystemBlocks 把 system 字段的两种形态都折成 IR 块。
func anthropicSystemBlocks(raw json.RawMessage) []Block {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return []Block{{Kind: BlockText, Text: s}}
		}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []Block
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, Block{Kind: BlockText, Text: b.Text})
		}
	}
	return out
}

func anthropicMessage(m AnthropicMessage) (Message, error) {
	role := Role(m.Role)
	if role != RoleUser && role != RoleAssistant {
		return Message{}, fmt.Errorf("不支持的消息角色 %q", m.Role)
	}
	msg := Message{Role: role}
	for _, b := range m.Content.All() {
		switch b.Type {
		case "text":
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockText, Text: b.Text})
		case "image":
			if b.Source == nil {
				continue
			}
			url := b.Source.URL
			if b.Source.Type == "base64" {
				url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
			}
			if url == "" {
				continue
			}
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockImage, Image: &Image{URL: url}})
		case "tool_use":
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockToolUse, ToolUse: &ToolUse{
				ID: b.ID, Name: b.Name, Input: input,
			}})
		case "tool_result":
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockToolResult, ToolResult: &ToolResult{
				ToolUseID: b.ToolUseID,
				Text:      toolResultText(b.Content),
				IsError:   b.IsError,
			}})
		case "thinking":
			if b.Thinking == "" && b.Signature == "" {
				continue
			}
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockThinking, Thinking: &Thinking{
				Text: b.Thinking, Signature: b.Signature,
			}})
		case "redacted_thinking":
			if b.Data == "" {
				continue
			}
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockThinking, Thinking: &Thinking{Redacted: b.Data}})
		}
	}
	return msg, nil
}

func toAnthropicMessage(m Message) (AnthropicMessage, error) {
	out := AnthropicMessage{Role: string(m.Role)}
	blocks := make([]ContentBlock, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText:
			blocks = append(blocks, ContentBlock{Type: "text", Text: b.Text})
		case BlockThinking:
			if b.Thinking == nil {
				continue
			}
			if blk, ok := anthropicThinkingBlock(*b.Thinking); ok {
				blocks = append(blocks, blk)
			}
		case BlockImage:
			if b.Image == nil {
				continue
			}
			blocks = append(blocks, ContentBlock{Type: "image", Source: imageSource(b.Image.URL)})
		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			input := b.ToolUse.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			blocks = append(blocks, ContentBlock{
				Type: "tool_use", ID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: input,
			})
		case BlockToolResult:
			if b.ToolResult == nil {
				continue
			}
			content, err := json.Marshal(b.ToolResult.Text)
			if err != nil {
				return out, err
			}
			blocks = append(blocks, ContentBlock{
				Type: "tool_result", ToolUseID: b.ToolResult.ToolUseID,
				Content: content, IsError: b.ToolResult.IsError,
			})
		}
	}
	out.Content = Content{Blocks: blocks}
	return out, nil
}

// toolResultBlocks 把一条 RoleTool 消息里的工具结果编成 anthropic 的 tool_result 块，
// 其余块丢弃：anthropic 的 user 消息里只有 tool_result 能承接工具结果。
func toolResultBlocks(m Message) []ContentBlock {
	var blocks []ContentBlock
	for _, b := range m.Blocks {
		if b.Kind != BlockToolResult || b.ToolResult == nil {
			continue
		}
		content, err := json.Marshal(b.ToolResult.Text)
		if err != nil {
			continue
		}
		blocks = append(blocks, ContentBlock{
			Type: "tool_result", ToolUseID: b.ToolResult.ToolUseID,
			Content: content, IsError: b.ToolResult.IsError,
		})
	}
	return blocks
}

// anthropicThinkingBlock 把思考块编成 anthropic 的 content block（DESIGN 5.4）。
// redacted_thinking 的 data 原样写回；普通思考块没有签名就不写——伪造签名会让上游以
// 400 拒绝整条请求，宁可丢掉这段正文。
func anthropicThinkingBlock(t Thinking) (ContentBlock, bool) {
	if t.Redacted != "" {
		return ContentBlock{Type: "redacted_thinking", Data: t.Redacted}, true
	}
	if t.Text == "" || t.Signature == "" {
		return ContentBlock{}, false
	}
	return ContentBlock{Type: "thinking", Thinking: t.Text, Signature: t.Signature}, true
}

// imageSource 把 data URI 或普通地址转成 anthropic 的 source 结构。
func imageSource(url string) *ImageSource {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		mediaType, data, found := strings.Cut(rest, ";base64,")
		if found {
			return &ImageSource{Type: "base64", MediaType: mediaType, Data: data}
		}
	}
	return &ImageSource{Type: "url", URL: url}
}

func fromAnthropicToolChoice(tc *AnthropicToolChoice) (*ToolChoice, *bool) {
	var parallel *bool
	if tc.DisableParallelToolUse {
		no := false
		parallel = &no
	}
	switch tc.Type {
	case "", "auto":
		return &ToolChoice{Mode: ToolChoiceAuto}, parallel
	case "any":
		return &ToolChoice{Mode: ToolChoiceRequired}, parallel
	case "none":
		return &ToolChoice{Mode: ToolChoiceNone}, parallel
	case "tool":
		return &ToolChoice{Mode: ToolChoiceNamed, Name: tc.Name}, parallel
	default:
		return &ToolChoice{Mode: ToolChoiceAuto}, parallel
	}
}

func toAnthropicToolChoice(tc *ToolChoice, parallel *bool) (*AnthropicToolChoice, error) {
	out := &AnthropicToolChoice{}
	if parallel != nil && !*parallel {
		out.DisableParallelToolUse = true
	}
	switch tc.Mode {
	case "", ToolChoiceAuto:
		out.Type = "auto"
	case ToolChoiceRequired:
		out.Type = "any"
	case ToolChoiceNone:
		out.Type = "none"
	case ToolChoiceNamed:
		if tc.Name == "" {
			return nil, errors.New("tool_choice 为 tool 时必须给出 name")
		}
		out.Type, out.Name = "tool", tc.Name
	default:
		return nil, fmt.Errorf("不支持的 tool_choice %q", tc.Mode)
	}
	return out, nil
}

// normalizeStop 把各协议的结束原因折成 IR 的四种说法。
func normalizeStop(reason string) string {
	switch reason {
	case "", StopEndTurn, "stop":
		return StopEndTurn
	case StopMaxTokens, "length", "max_output_tokens":
		return StopMaxTokens
	case StopToolUse, "tool_calls", "function_call":
		return StopToolUse
	case StopSequence:
		return StopSequence
	default:
		return StopEndTurn
	}
}

// fromNormalizeStop 把 IR 的结束原因翻成 anthropic 的说法。
func fromNormalizeStop(reason string) string {
	switch normalizeStop(reason) {
	case StopMaxTokens:
		return "max_tokens"
	case StopToolUse:
		return "tool_use"
	case StopSequence:
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

// toolResultText 把 anthropic 的 tool_result.content 折成一个字符串。
func toolResultText(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func joinText(blocks []Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Kind == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
