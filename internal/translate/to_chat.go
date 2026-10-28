package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ToChat 把 anthropic 请求转成 openai-chat 请求。model 是上游实际使用的模型名。
// 映射规则依据 DESIGN.md 第 5.4 节，表里没有的字段丢弃。
func ToChat(in *AnthropicRequest, model string) (*ChatRequest, error) {
	if in == nil {
		return nil, errors.New("请求为空")
	}
	if model == "" {
		model = in.Model
	}

	out := &ChatRequest{
		Model:       model,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.StopSequences,
		Stream:      in.Stream,
	}
	if in.Stream {
		out.StreamOptions = &ChatStreamOptions{IncludeUsage: true}
	}

	if sys := systemText(in.System); sys != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: sys})
	}

	for _, m := range in.Messages {
		msgs, err := messagesFor(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for _, t := range in.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, ChatTool{
			Type: "function",
			Function: ChatToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}

	if tc := in.ToolChoice; tc != nil {
		choice, parallel, err := toChatToolChoice(tc)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = choice
		out.ParallelToolCalls = parallel
	}

	return out, nil
}

// systemText 把 system 字段的两种形态都折成一个字符串。
func systemText(raw json.RawMessage) string {
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
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func toChatToolChoice(tc *AnthropicToolChoice) (any, *bool, error) {
	var parallel *bool
	if tc.DisableParallelToolUse {
		no := false
		parallel = &no
	}
	switch tc.Type {
	case "", "auto":
		return "auto", parallel, nil
	case "any":
		return "required", parallel, nil
	case "none":
		return "none", parallel, nil
	case "tool":
		if tc.Name == "" {
			return nil, nil, errors.New("tool_choice 为 tool 时必须给出 name")
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}, parallel, nil
	default:
		return nil, nil, fmt.Errorf("不支持的 tool_choice.type %q", tc.Type)
	}
}

// messagesFor 把一条 anthropic 消息展开成若干条 openai 消息。
// user 里的 tool_result 块必须变成独立的 tool 消息，其余内容合成一条 user 消息。
func messagesFor(m AnthropicMessage) ([]ChatMessage, error) {
	blocks := m.Content.All()

	switch m.Role {
	case "assistant":
		var text []string
		var calls []ChatToolCall
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text = append(text, b.Text)
			case "tool_use":
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				calls = append(calls, ChatToolCall{
					ID:   b.ID,
					Type: "function",
					Function: ChatFunctionCall{
						Name:      b.Name,
						Arguments: string(args),
					},
				})
			}
		}
		if len(text) == 0 && len(calls) == 0 {
			return nil, nil
		}
		msg := ChatMessage{Role: "assistant"}
		if len(text) > 0 {
			msg.Content = strings.Join(text, "")
		}
		msg.ToolCalls = calls
		return []ChatMessage{msg}, nil

	case "user", "":
		var out []ChatMessage
		var parts []any
		var text []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text = append(text, b.Text)
			case "image":
				if b.Source == nil {
					continue
				}
				if b.Source.Type == "base64" {
					parts = append(parts, map[string]any{
						"type": "image_url",
						"image_url": map[string]string{
							"url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
						},
					})
				} else if b.Source.URL != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]string{"url": b.Source.URL},
					})
				}
				if len(text) > 0 {
					parts = append([]any{map[string]any{"type": "text", "text": strings.Join(text, "")}}, parts...)
					text = nil
				}
			case "tool_result":
				out = append(out, ChatMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    toolResultText(b.Content),
				})
			}
		}
		if len(parts) > 0 {
			if len(text) > 0 {
				parts = append([]any{map[string]any{"type": "text", "text": strings.Join(text, "")}}, parts...)
			}
			out = append(out, ChatMessage{Role: "user", Content: parts})
		} else if len(text) > 0 {
			out = append(out, ChatMessage{Role: "user", Content: strings.Join(text, "")})
		}
		return out, nil

	default:
		return nil, fmt.Errorf("不支持的消息角色 %q", m.Role)
	}
}

// toolResultText 把 tool_result 的 content 折成一个字符串。
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

// FromChat 把 openai-chat 的非流式响应转成 anthropic 响应。
// 工具调用的 arguments 必须是合法 JSON，无法解析时返回错误，不猜内容。
func FromChat(in *ChatResponse, requestedModel string) (*AnthropicResponse, error) {
	if in == nil {
		return nil, errors.New("响应为空")
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("上游响应没有 choices")
	}
	choice := in.Choices[0]

	out := &AnthropicResponse{
		ID:      normalizeID(in.ID),
		Type:    "message",
		Role:    "assistant",
		Model:   requestedModel,
		Content: []ContentBlock{},
	}
	if out.Model == "" {
		out.Model = in.Model
	}

	if s := textOf(choice.Message.Content); s != "" {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: s})
	}
	for i, call := range choice.Message.ToolCalls {
		input := strings.TrimSpace(call.Function.Arguments)
		if input == "" {
			input = "{}"
		}
		if !json.Valid([]byte(input)) {
			return nil, fmt.Errorf("上游第 %d 个工具调用的参数不是合法 JSON", i+1)
		}
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("toolu_%s_%d", out.ID, i)
		}
		out.Content = append(out.Content, ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  call.Function.Name,
			Input: json.RawMessage(input),
		})
	}

	out.StopReason = stopReasonFromChat(choice.FinishReason)
	if in.Usage != nil {
		out.Usage = Usage{
			InputTokens:  in.Usage.PromptTokens,
			OutputTokens: in.Usage.CompletionTokens,
		}
		if in.Usage.PromptTokensDetails != nil {
			out.Usage.CacheReadInputTokens = in.Usage.PromptTokensDetails.CachedTokens
		}
	}
	return out, nil
}

func textOf(content any) string {
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
			if t, ok := m["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// stopReasonFromChat 按 DESIGN.md 第 5.4 节的表映射结束原因。
func stopReasonFromChat(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	case "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// normalizeID 保证 anthropic 的 id 以 msg_ 开头。
func normalizeID(id string) string {
	if id == "" {
		return "msg_switchboard"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}
