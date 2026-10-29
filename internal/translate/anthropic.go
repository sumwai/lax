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
	// 顶层 thinking 是请求侧的思考配置参数，与消息里的 thinking 块无关（DESIGN 5.4）。
	// 只认 enabled 与 disabled 两种 type，其余写法丢弃，不猜它的语义。
	if th := wire.Thinking; th != nil {
		switch th.Type {
		case "enabled":
			req.Thinking = &ThinkingConfig{Enabled: true, Budget: th.BudgetTokens}
		case "disabled":
			req.Thinking = &ThinkingConfig{}
		}
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

	// 思考配置参数：Enabled 为假写 disabled，有预算（或能折成预算的档位）写 enabled 加
	// budget_tokens。既没有预算也没有认得出的档位时不写：anthropic 的 enabled 必须带预算，
	// 编造一个数字等于替客户端下了它没说过的决心（DESIGN 5.4）。
	if th := r.Thinking; th != nil {
		if !th.Enabled {
			wire.Thinking = &AnthropicThinking{Type: "disabled"}
		} else if budget, ok := th.budgetTokens(); ok {
			wire.Thinking = &AnthropicThinking{Type: "enabled", BudgetTokens: budget}
		}
	}

	if sys := joinText(r.System); sys != "" {
		b, err := json.Marshal(sys)
		if err != nil {
			return nil, err
		}
		wire.System = b
	}

	// anthropic 没有 role=tool，工具结果必须落进 user 消息里的 tool_result 块，原样写过去
	// 会被上游以 400 拒绝。anthropic 也不接受两条相邻的 user 消息，所以连续的 user 侧内容
	// （工具结果与普通 user 消息）并成一条：tool_result 块在前，普通内容按原顺序跟随
	// （DESIGN 5.4）。
	var pendingResults, pendingBlocks []ContentBlock
	// emit 写出一条消息：块数为 0 的不写。anthropic 拒绝空的 content 数组，一条只剩无签名
	// 思考块的 assistant 消息编出来就是空的（DESIGN 5.4）。丢弃之后相邻的同角色消息按块
	// 顺序并成一条，assistant 也一样：丢一条空消息不该在报文里留下两条相邻的 assistant。
	emit := func(msg AnthropicMessage) {
		if len(msg.Content.Blocks) == 0 {
			return
		}
		if n := len(wire.Messages); n > 0 && wire.Messages[n-1].Role == msg.Role {
			last := &wire.Messages[n-1]
			last.Content.Blocks = append(last.Content.Blocks, msg.Content.Blocks...)
			return
		}
		wire.Messages = append(wire.Messages, msg)
	}
	flushUser := func() {
		if len(pendingResults) == 0 && len(pendingBlocks) == 0 {
			return
		}
		blocks := make([]ContentBlock, 0, len(pendingResults)+len(pendingBlocks))
		blocks = append(blocks, pendingResults...)
		blocks = append(blocks, pendingBlocks...)
		emit(AnthropicMessage{Role: "user", Content: Content{Blocks: blocks}})
		pendingResults, pendingBlocks = nil, nil
	}
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			pendingResults = append(pendingResults, toolResultBlocks(m)...)
			continue
		}
		if m.Role == RoleUser {
			msg, err := toAnthropicMessage(m)
			if err != nil {
				return nil, err
			}
			pendingBlocks = append(pendingBlocks, msg.Content.Blocks...)
			continue
		}
		flushUser()
		msg, err := toAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		emit(msg)
	}
	flushUser()
	// 消息全被丢弃时宁可自己报错：上游的 messages 至少要一条，写出空数组只会换回一个
	// 指不到原因上的 400。
	if len(wire.Messages) == 0 {
		return nil, errors.New("请求里没有可发给 anthropic 的消息：每条消息的内容块都被丢弃了")
	}

	for _, t := range r.Tools {
		// `input_schema` 是必填项，客户端函数缺它时补空对象；内置工具本来就没有 schema，
		// 补一个出来就是伪造上游事实（DESIGN 5.4）。
		schema := t.Schema
		if isClientTool(t) {
			schema = schemaOrEmpty(schema)
		}
		wire.Tools = append(wire.Tools, AnthropicTool{
			Type:        t.Type,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
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
	model     string
	started   bool
	nextIndex int
	openIndex int
	openKind  BlockKind
	// closedBlocks 记哪些块号已经发过 content_block_stop：收过 stop 的块不能再收
	// content_block_delta，需要重开一个新块（DESIGN 5.4）。
	closedBlocks map[int]bool
	// toolBlocks 是上游块号 → 最近一次开的 anthropic 块号，toolSpecs 记重开时要写的
	// id 与 name，两个并行工具各自映射到自己的块。
	toolBlocks map[int]int
	toolSpecs  map[int]ContentBlock
	// toolOpen 记「有工具块收了还没收尾」，断流时用来补说明。
	toolOpen   bool
	stopReason string
	usage      Usage
	stopped    bool
}

func newAnthropicStreamEncoder(model string) *anthropicStreamEncoder {
	return &anthropicStreamEncoder{
		model:        model,
		openIndex:    -1,
		closedBlocks: map[int]bool{},
		toolBlocks:   map[int]int{},
		toolSpecs:    map[int]ContentBlock{},
	}
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
		idx := e.ensureOpen(&events, BlockThinking, ContentBlock{Type: "thinking"})
		if d.Text != "" {
			events = append(events, e.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "thinking_delta", "thinking": d.Text},
			}))
		}
		if d.Signature != "" {
			events = append(events, e.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "signature_delta", "signature": d.Signature},
			}))
		}
	case DeltaText:
		idx := e.ensureOpen(&events, BlockText, ContentBlock{Type: "text", Text: ""})
		events = append(events, e.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]string{"type": "text_delta", "text": d.Text},
		}))
	case DeltaToolStart:
		if d.ToolID != "" || d.ToolName != "" {
			spec := e.toolSpecs[d.Index]
			spec.Type = "tool_use"
			if d.ToolID != "" {
				spec.ID = d.ToolID
			}
			if d.ToolName != "" {
				spec.Name = d.ToolName
			}
			spec.Input = json.RawMessage(`{}`)
			e.toolSpecs[d.Index] = spec
		}
		e.toolBlock(&events, d.Index)
		e.toolOpen = true
	case DeltaToolArgs:
		idx := e.toolBlock(&events, d.Index)
		e.toolOpen = true
		events = append(events, e.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": d.ArgsJSON},
		}))
	case DeltaStop:
		e.toolOpen = false
		if d.Usage != nil {
			e.usage = *d.Usage
		}
		if d.StopReason != "" {
			e.stopReason = d.StopReason
		}
		events = append(events, e.stopEvents()...)
	case DeltaError:
		events = append(events, ErrorEvent("api_error", withIncompleteToolArgs(d.Text, e.toolOpen)))
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

// ensureOpen 保证当前开着一个 kind 类型的块，返回它的块号。需要换块或重开时，把
// 关闭与开块的帧追加到 events。
func (e *anthropicStreamEncoder) ensureOpen(events *[]SSEEvent, kind BlockKind, tmpl ContentBlock) int {
	if e.openIndex >= 0 && e.openKind == kind {
		return e.openIndex
	}
	e.closeBlock(events)
	return e.startBlock(events, tmpl)
}

// toolBlock 返回某个上游工具块在本协议里的块号，必要时开块。块已经被关掉（例如中间
// 插进了一段思考增量）时重开一个新块：往已经发过 content_block_stop 的块发
// input_json_delta 是非法事件序列，客户端会把它当成另一个块的内容。
func (e *anthropicStreamEncoder) toolBlock(events *[]SSEEvent, upstream int) int {
	if idx, ok := e.toolBlocks[upstream]; ok {
		if !e.closedBlocks[idx] {
			return idx
		}
	}
	spec, ok := e.toolSpecs[upstream]
	if !ok {
		// 没收到 start 就来了参数，补一个只有 id 的块，避免丢内容。
		spec = ContentBlock{Type: "tool_use", ID: fmt.Sprintf("toolu_%d", upstream)}
	}
	if len(spec.Input) == 0 {
		spec.Input = json.RawMessage(`{}`)
	}
	e.closeBlock(events)
	idx := e.startBlock(events, spec)
	e.toolBlocks[upstream] = idx
	return idx
}

// startBlock 开一个新块，返回它的块号。
func (e *anthropicStreamEncoder) startBlock(events *[]SSEEvent, tmpl ContentBlock) int {
	idx := e.nextIndex
	e.nextIndex++
	e.openIndex = idx
	e.openKind = BlockKind(tmpl.Type)
	var payload any = tmpl
	switch tmpl.Type {
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
	delete(e.closedBlocks, idx)
	return idx
}

func (e *anthropicStreamEncoder) closeBlock(events *[]SSEEvent) {
	if e.openIndex < 0 {
		return
	}
	*events = append(*events, e.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": e.openIndex,
	}))
	e.closedBlocks[e.openIndex] = true
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
			// anthropic 是唯一认签名的协议，带签名的思考块回填给它才是它要求的样子；
			// 没有签名的块由 anthropicThinkingBlock 丢弃（DESIGN 5.4）。
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
	case ToolChoiceAllowedTools:
		// 白名单只搬名字（DESIGN 5.4）。一项名字都没有的空白名单整条丢弃：这一档说的是
		// 「只准调这些」，一个名字都没有时把它的意图改成 auto 或 none 都是替客户端说话，
		// 不写出去时上游按自己的默认值走（与编码侧的空白名单同一个处理）。
		names := make([]string, 0, len(tc.Tools))
		for _, ref := range tc.Tools {
			if ref.Name != "" {
				names = append(names, ref.Name)
			}
		}
		if len(names) == 0 {
			return nil, parallel
		}
		return &ToolChoice{Mode: ToolChoiceAllowedTools, Allowed: names}, parallel
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
	case ToolChoiceAllowedTools:
		// 空白名单不编：写一个 {"type":"allowed_tools","tools":[]} 只能换回一个 400，
		// 整条不写时上游按自己默认的 auto 走（DESIGN 5.4）。
		if len(tc.Allowed) == 0 {
			return nil, nil
		}
		out.Type = ToolChoiceAllowedTools
		out.Tools = make([]AnthropicToolChoiceRef, 0, len(tc.Allowed))
		for _, name := range tc.Allowed {
			out.Tools = append(out.Tools, AnthropicToolChoiceRef{Type: "tool", Name: name})
		}
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
