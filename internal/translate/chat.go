package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件负责 openai-chat 与中间表示之间的编解码，入站与出站共用同一套映射。
// 映射规则依据 DESIGN.md 第 5.4 节，表里没有的字段一律丢弃。

// DecodeChatRequest 把 openai-chat 请求体解析成 IR。
// system/developer 消息归入 Request.System，tool 消息变成 tool_result 块，
// assistant 的 tool_calls 变成 tool_use 块。
func DecodeChatRequest(raw []byte) (*Request, error) {
	var in ChatRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("openai-chat 请求体不是合法 JSON: %w", err)
	}

	out := &Request{
		Model:             in.Model,
		MaxTokens:         in.MaxTokens,
		Temperature:       in.Temperature,
		TopP:              in.TopP,
		Stop:              in.Stop,
		ParallelToolCalls: in.ParallelToolCalls,
		Stream:            in.Stream,
	}

	// 思考配置参数：reasoning_effort 是主流写法，reasoning.effort 是少数实现包一层的写法，
	// 两个都有时以顶层那个为准（DESIGN 5.4「思考配置参数」）。
	effort := strings.TrimSpace(in.ReasoningEffort)
	if effort == "" && in.Reasoning != nil {
		effort = strings.TrimSpace(in.Reasoning.Effort)
	}
	if effort != "" {
		out.Thinking = &ThinkingConfig{Enabled: true, Effort: effort}
	}

	for i, m := range in.Messages {
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			out.System = append(out.System, chatTextBlocks(m.Content)...)
		case "", "user":
			blocks, err := chatUserBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条消息: %w", i+1, err)
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, Message{Role: RoleUser, Blocks: blocks})
			}
		case "assistant":
			blocks, err := chatAssistantBlocks(m)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条消息: %w", i+1, err)
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, Message{Role: RoleAssistant, Blocks: blocks})
			}
		case "tool":
			out.Messages = append(out.Messages, Message{
				Role: RoleTool,
				Blocks: []Block{{
					Kind: BlockToolResult,
					ToolResult: &ToolResult{
						ToolUseID: m.ToolCallID,
						Text:      chatContentText(m.Content),
					},
				}},
			})
		default:
			return nil, fmt.Errorf("第 %d 条消息的角色 %q 不受支持", i+1, m.Role)
		}
	}

	for _, t := range in.Tools {
		out.Tools = append(out.Tools, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      schemaOrEmpty(t.Function.Parameters),
			Strict:      t.Function.Strict,
		})
	}

	choice, err := chatToolChoiceFromWire(in.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = choice

	return out, nil
}

// EncodeChatRequest 把 IR 编成 openai-chat 请求体。model 是上游实际使用的模型名。
func EncodeChatRequest(r *Request, model string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("请求为空")
	}
	if model == "" {
		model = r.Model
	}

	out := ChatRequest{
		Model:             model,
		MaxTokens:         r.MaxTokens,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		Stop:              append([]string(nil), r.Stop...),
		ParallelToolCalls: r.ParallelToolCalls,
		Stream:            r.Stream,
	}
	if r.Stream {
		out.StreamOptions = &ChatStreamOptions{IncludeUsage: true}
	}

	// 思考配置参数：档位在这条协议上原生可写，因此档位优先、没档位时按预算折；
	// Enabled 为假时一个字段都不写——chat 没有「不思考」的档位（DESIGN 5.4）。
	if effort := r.Thinking.openAIEffort(); effort != "" {
		out.ReasoningEffort = effort
	}

	if sys := chatJoinText(r.System); sys != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: sys})
	}

	for i, m := range r.Messages {
		msgs, err := chatMessagesFor(m)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条消息: %w", i+1, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for _, t := range r.Tools {
		if !isClientTool(t) {
			// anthropic 的服务端内置工具在 chat 里没有对应形状，伪造一个 function 会语义错位。
			continue
		}
		out.Tools = append(out.Tools, ChatTool{
			Type: "function",
			Function: ChatToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schemaOrEmpty(t.Schema),
				Strict:      t.Strict,
			},
		})
	}

	if r.ToolChoice != nil {
		choice, err := chatToolChoiceToWire(r.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = choice
	}

	return json.Marshal(out)
}

// DecodeChatResponse 把 openai-chat 的非流式响应解析成 IR。
// 工具调用的 arguments 必须是合法 JSON，无法解析时返回错误，不猜内容。
func DecodeChatResponse(raw []byte, requestedModel string) (*Response, error) {
	var in ChatResponse
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("openai-chat 响应体不是合法 JSON: %w", err)
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("上游响应没有 choices")
	}
	choice := in.Choices[0]

	out := &Response{
		ID:     in.ID,
		Model:  requestedModel,
		Blocks: []Block{},
	}
	if out.Model == "" {
		out.Model = in.Model
	}

	// 思考内容先于正文：chat 的报文里 reasoning_content 与 content 是两个字段，
	// 到达顺序丢失，统一按「先想后说」排。
	if reasoning := chatReasoningText(choice.Message.ReasoningContent, choice.Message.Reasoning); reasoning != "" {
		out.Blocks = append(out.Blocks, Block{Kind: BlockThinking, Thinking: &Thinking{Text: reasoning}})
	}
	if text := chatContentText(choice.Message.Content); text != "" {
		out.Blocks = append(out.Blocks, Block{Kind: BlockText, Text: text})
	}
	for i, call := range choice.Message.ToolCalls {
		input, err := chatArgumentsJSON(call.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("上游第 %d 个工具调用的参数不是合法 JSON: %w", i+1, err)
		}
		out.Blocks = append(out.Blocks, Block{
			Kind: BlockToolUse,
			ToolUse: &ToolUse{
				ID:    call.ID,
				Name:  call.Function.Name,
				Input: input,
			},
		})
	}

	out.StopReason = chatStopToIR(choice.FinishReason)
	out.Usage = chatUsageToIR(in.Usage)

	return out, nil
}

// EncodeChatResponse 把 IR 编成 openai-chat 的 chat completion 对象。
func EncodeChatResponse(r *Response) ([]byte, error) {
	if r == nil {
		return nil, errors.New("响应为空")
	}

	msg := ChatMessage{Role: "assistant"}
	var text strings.Builder
	var reasoning strings.Builder
	for i, b := range r.Blocks {
		switch b.Kind {
		case BlockText:
			text.WriteString(b.Text)
		case BlockThinking:
			// chat 只有 reasoning_content 一个落点，签名与 redacted 没有地方放，丢弃。
			if b.Thinking != nil && b.Thinking.Text != "" {
				reasoning.WriteString(b.Thinking.Text)
			}
		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			args := strings.TrimSpace(string(b.ToolUse.Input))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, fmt.Errorf("第 %d 个工具调用 %q 的输入不是合法 JSON", i+1, b.ToolUse.Name)
			}
			id := b.ToolUse.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", len(msg.ToolCalls))
			}
			msg.ToolCalls = append(msg.ToolCalls, ChatToolCall{
				ID:   id,
				Type: "function",
				Function: ChatFunctionCall{
					Name:      b.ToolUse.Name,
					Arguments: args,
				},
			})
		}
	}
	msg.Content = text.String()
	if r := reasoning.String(); r != "" {
		msg.ReasoningContent = r
	}

	usage := chatUsageToWire(r.Usage)
	out := ChatResponse{
		ID:      chatCompletionID(r.ID),
		Object:  "chat.completion",
		Created: 0,
		Model:   r.Model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: chatStopFromIR(r.StopReason),
		}},
		Usage: &usage,
	}
	return json.Marshal(out)
}

// chatStreamDecoder 把上游 chat 流式帧翻成 Delta。
// 每个工具调用的首个片段产出 DeltaToolStart，其后的参数片段产出 DeltaToolArgs。
type chatStreamDecoder struct {
	toolSeen   map[int]bool
	stopReason string
	stopped    bool
	closed     bool
	usage      *Usage
}

// Decode 处理一条上游 data 帧。[DONE] 由调用方过滤，收到时返回空。
func (d *chatStreamDecoder) Decode(data []byte) ([]Delta, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil, nil
	}

	var frame struct {
		ChatStreamChunk
		Error *chatStreamError `json:"error"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, fmt.Errorf("上游流式帧不是合法 JSON: %w", err)
	}
	if frame.Error != nil {
		return []Delta{{Kind: DeltaError, Text: frame.Error.Message}}, nil
	}

	if d.toolSeen == nil {
		d.toolSeen = map[int]bool{}
	}
	chunk := frame.ChatStreamChunk

	if chunk.Usage != nil {
		d.usage = chatUsagePtr(chunk.Usage)
	}

	var out []Delta
	for _, ch := range chunk.Choices {
		if ch.Index != 0 {
			continue
		}
		if reasoning := chatReasoningText(ch.Delta.ReasoningContent, ch.Delta.Reasoning); reasoning != "" {
			out = append(out, Delta{Kind: DeltaThinking, Text: reasoning})
		}
		if ch.Delta.Content != "" {
			out = append(out, Delta{Kind: DeltaText, Text: ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			index := 0
			if tc.Index != nil {
				index = *tc.Index
			}
			if !d.toolSeen[index] {
				d.toolSeen[index] = true
				out = append(out, Delta{
					Kind:     DeltaToolStart,
					Index:    index,
					ToolID:   tc.ID,
					ToolName: tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				out = append(out, Delta{
					Kind:     DeltaToolArgs,
					Index:    index,
					ArgsJSON: tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" && !d.stopped {
			d.stopped = true
			d.stopReason = chatStopToIR(*ch.FinishReason)
			out = append(out, Delta{
				Kind:       DeltaStop,
				StopReason: d.stopReason,
				Usage:      d.takeUsage(),
			})
		}
	}
	return out, nil
}

// Close 在上游结束时补齐收尾。finish_reason 先到、usage 后到时补一个只带 Usage 的 DeltaStop。
func (d *chatStreamDecoder) Close() []Delta {
	if d.closed {
		return nil
	}
	d.closed = true

	if !d.stopped {
		d.stopped = true
		reason := d.stopReason
		if reason == "" {
			reason = StopEndTurn
		}
		return []Delta{{Kind: DeltaStop, StopReason: reason, Usage: d.takeUsage()}}
	}
	if u := d.takeUsage(); u != nil {
		return []Delta{{Kind: DeltaStop, Usage: u}}
	}
	return nil
}

// takeUsage 取走待附加的用量，避免重复上报。
func (d *chatStreamDecoder) takeUsage() *Usage {
	u := d.usage
	d.usage = nil
	return u
}

// chatStreamEncoder 把 Delta 编成下发给客户端的 chat SSE 帧。
type chatStreamEncoder struct {
	model     string
	id        string
	usage     *Usage
	usageSent bool
	done      bool
	// toolSlots 把上游的块号映射成 chat 的稠密序号：上游 anthropic 有文本块时
	// 首个工具是 1，按稠密下标建数组的客户端会撞上空位（DESIGN 5.4）。
	toolSlots map[int]int
	// toolOpen 记「有工具块开了还没收」，断流时用来补说明。
	toolOpen bool
}

// newChatStreamEncoder 构造编码器。model 是客户端请求的模型名，会写回每一帧。
func newChatStreamEncoder(model string) *chatStreamEncoder {
	return &chatStreamEncoder{model: model, toolSlots: map[int]int{}}
}

// Start 发首帧：role=assistant、content 为空、finish_reason=null。
func (e *chatStreamEncoder) Start() []SSEEvent {
	empty := ""
	return []SSEEvent{e.event(chatOutChunk{
		ID:      e.chunkID(),
		Object:  "chat.completion.chunk",
		Created: 0,
		Model:   e.model,
		Choices: []chatOutChoice{{
			Index: 0,
			Delta: chatOutDelta{Role: "assistant", Content: &empty},
		}},
	})}
}

// Emit 把一条增量编成一帧或几帧。
func (e *chatStreamEncoder) Emit(d Delta) []SSEEvent {
	switch d.Kind {
	case DeltaThinking:
		// 签名在 chat 协议里没有落点，只有正文能写出去。
		if d.Text == "" {
			return nil
		}
		reasoning := d.Text
		return []SSEEvent{e.choiceEvent(chatOutDelta{ReasoningContent: &reasoning}, nil)}

	case DeltaText:
		content := d.Text
		return []SSEEvent{e.choiceEvent(chatOutDelta{Content: &content}, nil)}

	case DeltaToolStart:
		index := e.toolSlot(d.Index)
		e.toolOpen = true
		return []SSEEvent{e.choiceEvent(chatOutDelta{
			ToolCalls: []ChatToolCall{{
				Index: &index,
				ID:    d.ToolID,
				Type:  "function",
				Function: ChatFunctionCall{
					Name:      d.ToolName,
					Arguments: "",
				},
			}},
		}, nil)}

	case DeltaToolArgs:
		index := e.toolSlot(d.Index)
		e.toolOpen = true
		return []SSEEvent{e.choiceEvent(chatOutDelta{
			ToolCalls: []ChatToolCall{{
				Index: &index,
				Function: ChatFunctionCall{
					Arguments: d.ArgsJSON,
				},
			}},
		}, nil)}

	case DeltaStop:
		e.toolOpen = false
		var out []SSEEvent
		if d.StopReason != "" {
			reason := chatStopFromIR(d.StopReason)
			out = append(out, e.choiceEvent(chatOutDelta{}, &reason))
		}
		if d.Usage != nil {
			e.usage = d.Usage
			out = append(out, e.usageEvent(d.Usage))
			e.usageSent = true
		}
		return out

	case DeltaError:
		body, err := json.Marshal(map[string]any{
			"error": map[string]string{
				"type":    "upstream_error",
				"message": withIncompleteToolArgs(d.Text, e.toolOpen),
			},
		})
		if err != nil {
			return nil
		}
		return []SSEEvent{{Data: body}}
	}
	return nil
}

// Finish 收尾：先补一帧用量（若有且未发），再发 [DONE]。
func (e *chatStreamEncoder) Finish() []SSEEvent {
	if e.done {
		return nil
	}
	e.done = true

	var out []SSEEvent
	if e.usage != nil && !e.usageSent {
		out = append(out, e.usageEvent(e.usage))
		e.usageSent = true
	}
	return append(out, SSEEvent{Data: []byte("[DONE]")})
}

// toolSlot 给出某个上游块号对应的 chat 工具序号：从 0 开始按首次出现的顺序连续编号。
func (e *chatStreamEncoder) toolSlot(upstream int) int {
	if idx, ok := e.toolSlots[upstream]; ok {
		return idx
	}
	idx := len(e.toolSlots)
	e.toolSlots[upstream] = idx
	return idx
}

func (e *chatStreamEncoder) choiceEvent(delta chatOutDelta, finish *string) SSEEvent {
	return e.event(chatOutChunk{
		ID:      e.chunkID(),
		Object:  "chat.completion.chunk",
		Created: 0,
		Model:   e.model,
		Choices: []chatOutChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	})
}

// usageEvent 发一帧带 usage 的空 choices。
func (e *chatStreamEncoder) usageEvent(u *Usage) SSEEvent {
	usage := chatUsageToWire(*u)
	return e.event(chatOutChunk{
		ID:      e.chunkID(),
		Object:  "chat.completion.chunk",
		Created: 0,
		Model:   e.model,
		Choices: []chatOutChoice{},
		Usage:   &usage,
	})
}

func (e *chatStreamEncoder) chunkID() string {
	if e.id == "" {
		e.id = chatCompletionID("")
	}
	return e.id
}

func (e *chatStreamEncoder) event(v any) SSEEvent {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"error":{"type":"encode_error","message":"帧序列化失败"}}`)
	}
	return SSEEvent{Data: b}
}

// chatOutChunk 是下发的流式帧。与 ChatStreamChunk 的区别在于 content 用指针，
// 保证空字符串也会写进报文，finish_reason 显式写 null。
type chatOutChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []chatOutChoice `json:"choices"`
	Usage   *ChatUsage      `json:"usage,omitempty"`
}

type chatOutChoice struct {
	Index        int          `json:"index"`
	Delta        chatOutDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type chatOutDelta struct {
	Role             string         `json:"role,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	Content          *string        `json:"content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

type chatStreamError struct {
	Message string `json:"message"`
}

// chatUsageScanner 从 chat SSE 里读用量，用于同协议直通时记账。读不到就返回 0。
type chatUsageScanner struct {
	in, out, cacheRead int64
	reasoning, total   int64
}

// Feed 送入一条 data 帧，非 JSON 与 [DONE] 一律忽略。
func (s *chatUsageScanner) Feed(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var env struct {
		Usage *ChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Usage == nil {
		return
	}
	if env.Usage.PromptTokens > 0 {
		s.in = env.Usage.PromptTokens
	}
	if env.Usage.CompletionTokens > 0 {
		s.out = env.Usage.CompletionTokens
	}
	if env.Usage.TotalTokens > 0 {
		s.total = env.Usage.TotalTokens
	}
	if env.Usage.PromptTokensDetails != nil {
		s.cacheRead = env.Usage.PromptTokensDetails.CachedTokens
	}
	if d := env.Usage.CompletionTokensDetails; d != nil && d.ReasoningTokens > 0 {
		s.reasoning = d.ReasoningTokens
	}
}

// Usage 返回读到的用量。chat 协议没有缓存写入字段，CacheWrite 恒为 0。
func (s *chatUsageScanner) Usage() Usage {
	return Usage{In: s.in, Out: s.out, CacheRead: s.cacheRead, Reasoning: s.reasoning, Total: s.total}
}

// chatUserBlocks 把 user 消息的 content（字符串或 part 数组）转成内容块。
// image_url 里的 data URI 与 http 地址都原样保留。
func chatUserBlocks(content any) ([]Block, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []Block{{Kind: BlockText, Text: v}}, nil
	case []any:
		var blocks []Block
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("content 数组里的元素不是对象")
			}
			kind, _ := m["type"].(string)
			switch kind {
			case "text":
				if t, _ := m["text"].(string); t != "" {
					blocks = append(blocks, Block{Kind: BlockText, Text: t})
				}
			case "image_url":
				if url := chatImageURL(m["image_url"]); url != "" {
					blocks = append(blocks, Block{Kind: BlockImage, Image: &Image{URL: url}})
				}
			}
		}
		return blocks, nil
	default:
		return nil, fmt.Errorf("content 的类型 %T 不受支持", content)
	}
}

// chatAssistantBlocks 把 assistant 消息的思考内容、文本与 tool_calls 转成内容块。
func chatAssistantBlocks(m ChatMessage) ([]Block, error) {
	var blocks []Block
	if reasoning := chatReasoningText(m.ReasoningContent, m.Reasoning); reasoning != "" {
		blocks = append(blocks, Block{Kind: BlockThinking, Thinking: &Thinking{Text: reasoning}})
	}
	blocks = append(blocks, chatTextBlocks(m.Content)...)
	for _, call := range m.ToolCalls {
		input, err := chatArgumentsJSON(call.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("工具调用 %q 的参数不是合法 JSON: %w", call.Function.Name, err)
		}
		blocks = append(blocks, Block{
			Kind: BlockToolUse,
			ToolUse: &ToolUse{
				ID:    call.ID,
				Name:  call.Function.Name,
				Input: input,
			},
		})
	}
	return blocks, nil
}

// chatMessagesFor 把一条 IR 消息展开成若干条 chat 消息。
// user 里的 tool_result 块必须先变成独立的 tool 消息，其后才是同一条消息里的文本与图片。
func chatMessagesFor(m Message) ([]ChatMessage, error) {
	switch m.Role {
	case RoleSystem:
		text := chatJoinText(m.Blocks)
		if text == "" {
			return nil, nil
		}
		return []ChatMessage{{Role: "system", Content: text}}, nil

	case RoleUser:
		var out []ChatMessage
		var parts []any
		var text strings.Builder
		hasImage := false

		flush := func() {
			if text.Len() == 0 {
				return
			}
			parts = append(parts, map[string]any{"type": "text", "text": text.String()})
			text.Reset()
		}

		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockText:
				text.WriteString(b.Text)
			case BlockImage:
				if b.Image == nil || b.Image.URL == "" {
					continue
				}
				flush()
				hasImage = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]string{"url": b.Image.URL},
				})
			case BlockToolResult:
				if b.ToolResult == nil {
					continue
				}
				out = append(out, ChatMessage{
					Role:       "tool",
					ToolCallID: b.ToolResult.ToolUseID,
					Content:    b.ToolResult.Text,
				})
			}
		}

		if hasImage {
			flush()
			out = append(out, ChatMessage{Role: "user", Content: parts})
			return out, nil
		}
		if p := text.String(); p != "" {
			out = append(out, ChatMessage{Role: "user", Content: p})
		}
		return out, nil

	case RoleAssistant:
		var text strings.Builder
		var calls []ChatToolCall
		for i, b := range m.Blocks {
			switch b.Kind {
			case BlockText:
				text.WriteString(b.Text)
			case BlockThinking:
				// 请求方向不回填思考内容：chat 的 reasoning_content 是上游自己产生的字段，
				// 客户端回填它在主流推理模型上会被以 400 拒掉整条请求（DESIGN 5.4）。
				continue
			case BlockToolUse:
				if b.ToolUse == nil {
					continue
				}
				args := strings.TrimSpace(string(b.ToolUse.Input))
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return nil, fmt.Errorf("第 %d 个工具调用 %q 的输入不是合法 JSON", i+1, b.ToolUse.Name)
				}
				calls = append(calls, ChatToolCall{
					ID:   b.ToolUse.ID,
					Type: "function",
					Function: ChatFunctionCall{
						Name:      b.ToolUse.Name,
						Arguments: args,
					},
				})
			}
		}
		if text.Len() == 0 && len(calls) == 0 {
			return nil, nil
		}
		msg := ChatMessage{Role: "assistant", ToolCalls: calls}
		if t := text.String(); t != "" {
			msg.Content = t
		}
		return []ChatMessage{msg}, nil

	case RoleTool:
		var out []ChatMessage
		for _, b := range m.Blocks {
			if b.Kind != BlockToolResult || b.ToolResult == nil {
				continue
			}
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: b.ToolResult.ToolUseID,
				Content:    b.ToolResult.Text,
			})
		}
		return out, nil

	default:
		return nil, fmt.Errorf("不支持的消息角色 %q", m.Role)
	}
}

// chatTextBlocks 把字符串或 part 数组里的文本折成 text 块。
func chatTextBlocks(content any) []Block {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []Block{{Kind: BlockText, Text: v}}
	case []any:
		var blocks []Block
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["text"].(string); t != "" {
				blocks = append(blocks, Block{Kind: BlockText, Text: t})
			}
		}
		return blocks
	default:
		return nil
	}
}

// chatContentText 把 content 折成一个字符串：字符串原样返回，part 数组里的文本用换行拼接。
func chatContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// chatImageURL 取 image_url 的地址，兼容对象与纯字符串两种写法。
func chatImageURL(v any) string {
	switch u := v.(type) {
	case string:
		return u
	case map[string]any:
		url, _ := u["url"].(string)
		return url
	default:
		return ""
	}
}

// chatJoinText 把 text 块按顺序拼成一个字符串。
func chatJoinText(blocks []Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Kind == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// chatReasoningText 取思考内容正文。主流实现写在 reasoning_content，
// 少数写在 reasoning；两个都读，同时出现时以前者为准（DESIGN 5.4）。
func chatReasoningText(primary, compat string) string {
	if primary != "" {
		return primary
	}
	return compat
}

// chatArgumentsJSON 校验工具参数。空串按「没有参数」处理，等价于空对象。
func chatArgumentsJSON(args string) (json.RawMessage, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid([]byte(args)) {
		return nil, errors.New("参数不是合法 JSON")
	}
	return json.RawMessage(args), nil
}

// chatToolChoiceFromWire 解析入站的 tool_choice。
func chatToolChoiceFromWire(raw any) (*ToolChoice, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		switch v {
		case "", "auto":
			return &ToolChoice{Mode: ToolChoiceAuto}, nil
		case "required", "any":
			return &ToolChoice{Mode: ToolChoiceRequired}, nil
		case "none":
			return &ToolChoice{Mode: ToolChoiceNone}, nil
		default:
			return nil, fmt.Errorf("不支持的 tool_choice %q", v)
		}
	case map[string]any:
		kind, _ := v["type"].(string)
		switch kind {
		case "auto":
			return &ToolChoice{Mode: ToolChoiceAuto}, nil
		case "required", "any":
			return &ToolChoice{Mode: ToolChoiceRequired}, nil
		case "none":
			return &ToolChoice{Mode: ToolChoiceNone}, nil
		case "function":
			fn, _ := v["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" {
				return nil, errors.New("tool_choice 指定函数时必须给出 function.name")
			}
			return &ToolChoice{Mode: ToolChoiceNamed, Name: name}, nil
		default:
			return nil, fmt.Errorf("不支持的 tool_choice.type %q", kind)
		}
	default:
		return nil, fmt.Errorf("不支持的 tool_choice 类型 %T", raw)
	}
}

// chatToolChoiceToWire 把 IR 的 tool_choice 编成 chat 的写法。
func chatToolChoiceToWire(tc *ToolChoice) (any, error) {
	switch tc.Mode {
	case "", ToolChoiceAuto:
		return "auto", nil
	case ToolChoiceRequired:
		return "required", nil
	case ToolChoiceNone:
		return "none", nil
	case ToolChoiceAllowedTools:
		// chat 只有 auto、required、none 三档，没有白名单形状：降级为 auto，白名单丢掉
		// （DESIGN 5.4）。空白名单整条不写：没有名字可降级，不写时上游默认就是 auto。
		if len(tc.Allowed) == 0 {
			return nil, nil
		}
		return "auto", nil
	case ToolChoiceNamed:
		if tc.Name == "" {
			return nil, errors.New("tool_choice 为指定工具时必须给出 name")
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}, nil
	default:
		return nil, fmt.Errorf("不支持的 tool_choice %q", tc.Mode)
	}
}

// chatStopToIR 把 finish_reason 映射成 IR 的结束原因。
func chatStopToIR(reason string) string {
	switch reason {
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "stop_sequence":
		return StopSequence
	default:
		return StopEndTurn
	}
}

// chatStopFromIR 把 IR 的结束原因翻成 finish_reason。
func chatStopFromIR(reason string) string {
	switch reason {
	case StopMaxTokens:
		return "length"
	case StopToolUse:
		return "tool_calls"
	default:
		return "stop"
	}
}

// chatUsageToIR 把 chat 的用量映射成 IR 用量。nil 视为零值。
func chatUsageToIR(u *ChatUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{In: u.PromptTokens, Out: u.CompletionTokens, Total: u.TotalTokens}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// chatUsagePtr 是 chatUsageToIR 的指针版本，nil 原样返回。
func chatUsagePtr(u *ChatUsage) *Usage {
	if u == nil {
		return nil
	}
	v := chatUsageToIR(u)
	return &v
}

// chatUsageToWire 把 IR 用量编成 chat 的用量字段。
func chatUsageToWire(u Usage) ChatUsage {
	out := ChatUsage{
		PromptTokens:     u.In,
		CompletionTokens: u.Out,
		TotalTokens:      u.In + u.Out,
	}
	if u.CacheRead > 0 {
		out.PromptTokensDetails = &PromptTokenDetail{CachedTokens: u.CacheRead}
	}
	return out
}

// chatCompletionID 保证 id 以 chatcmpl 开头，缺失时给固定值。
func chatCompletionID(id string) string {
	if id == "" {
		return "chatcmpl-lax"
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	if trimmed := strings.TrimPrefix(id, "msg_"); trimmed != id {
		return "chatcmpl-" + trimmed
	}
	return id
}
