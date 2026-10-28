package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SSEEvent 是一条下发给客户端的 SSE 帧。
type SSEEvent struct {
	Event string
	Data  []byte
}

// Bytes 按 SSE 格式序列化，事件名与数据之间空行结尾。
func (e SSEEvent) Bytes() []byte {
	var b bytes.Buffer
	if e.Event != "" {
		fmt.Fprintf(&b, "event: %s\n", e.Event)
	}
	b.WriteString("data: ")
	b.Write(e.Data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// ErrorEvent 构造一条 anthropic 的错误事件。
func ErrorEvent(kind, message string) SSEEvent {
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    kind,
			"message": message,
		},
	})
	return SSEEvent{Event: "error", Data: body}
}

// ScanSSE 按 SSE 规则逐帧读取，回调里拿到事件名与已拼接的 data。
// 注释行忽略；单行长度上限 4MB，够放整段工具参数。
func ScanSSE(r io.Reader, fn func(event string, data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var event string
	var data []byte

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				if err := fn(event, data); err != nil {
					return err
				}
			}
			event, data = "", nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if len(data) > 0 {
		if err := fn(event, data); err != nil {
			return err
		}
	}
	return sc.Err()
}

// StreamState 把 openai-chat 的流式增量翻译成 anthropic 事件序列。
// 每收到一帧就吐出此刻该下发的事件，不缓存整段响应。
type StreamState struct {
	model      string
	id         string
	started    bool
	nextIndex  int
	openIndex  int
	openKind   string
	toolBlocks map[int]int
	stopReason string
	inTokens   int64
	outTokens  int64
	finished   bool
}

// NewChatStream 构造翻译器。model 是客户端请求的模型名，会原样写回响应。
func NewChatStream(model string) *StreamState {
	return &StreamState{
		model:      model,
		id:         "msg_switchboard",
		openIndex:  -1,
		toolBlocks: map[int]int{},
	}
}

// Translate 处理一条上游 data 帧，返回需要下发的事件。
func (s *StreamState) Translate(data []byte) ([]SSEEvent, error) {
	var chunk ChatStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("上游流式帧不是合法 JSON: %w", err)
	}

	var events []SSEEvent
	if !s.started {
		if chunk.ID != "" {
			s.id = normalizeID(chunk.ID)
		}
		events = append(events, s.messageStart())
		s.started = true
	}
	if chunk.Usage != nil {
		s.inTokens = chunk.Usage.PromptTokens
		s.outTokens = chunk.Usage.CompletionTokens
	}

	for _, ch := range chunk.Choices {
		if ch.Index != 0 {
			continue
		}
		if ch.Delta.Content != "" {
			if s.openKind != "text" {
				s.startBlock(&events, ContentBlock{Type: "text", Text: ""})
			}
			events = append(events, s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.openIndex,
				"delta": map[string]string{"type": "text_delta", "text": ch.Delta.Content},
			}))
		}
		for _, tc := range ch.Delta.ToolCalls {
			upstream := 0
			if tc.Index != nil {
				upstream = *tc.Index
			}
			idx, seen := s.toolBlocks[upstream]
			if !seen {
				s.closeBlock(&events)
				s.startBlock(&events, ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(`{}`),
				})
				idx = s.openIndex
				s.toolBlocks[upstream] = idx
			} else {
				s.openIndex = idx
				s.openKind = "tool_use"
			}
			if tc.Function.Arguments != "" {
				events = append(events, s.event("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				}))
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.stopReason = stopReasonFromChat(*ch.FinishReason)
		}
	}

	if s.stopReason != "" {
		s.closeBlock(&events)
	}
	return events, nil
}

// Finish 补齐收尾事件。上游中途结束时也要给出合法的 message_delta 与 message_stop。
func (s *StreamState) Finish() []SSEEvent {
	if s.finished {
		return nil
	}
	s.finished = true

	var events []SSEEvent
	if !s.started {
		events = append(events, s.messageStart())
		s.started = true
	}
	s.closeBlock(&events)

	reason := s.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	events = append(events, s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": Usage{InputTokens: s.inTokens, OutputTokens: s.outTokens},
	}))
	events = append(events, s.event("message_stop", map[string]any{"type": "message_stop"}))
	return events
}

// Usage 返回已经解析到的用量。
func (s *StreamState) Usage() (in, out int64) { return s.inTokens, s.outTokens }

// StopReason 返回已经解析到的结束原因。
func (s *StreamState) StopReason() string { return s.stopReason }

func (s *StreamState) messageStart() SSEEvent {
	msg := AnthropicResponse{
		ID:      s.id,
		Type:    "message",
		Role:    "assistant",
		Model:   s.model,
		Content: []ContentBlock{},
		Usage:   Usage{InputTokens: s.inTokens},
	}
	return s.event("message_start", map[string]any{"type": "message_start", "message": msg})
}

func (s *StreamState) startBlock(events *[]SSEEvent, block ContentBlock) {
	idx := s.nextIndex
	s.nextIndex++
	s.openIndex = idx
	s.openKind = block.Type

	// text 块要带上空字符串，客户端据此初始化文本累积。
	var payload any = block
	if block.Type == "text" {
		payload = map[string]any{"type": "text", "text": ""}
	}
	*events = append(*events, s.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": payload,
	}))
}

func (s *StreamState) closeBlock(events *[]SSEEvent) {
	if s.openIndex < 0 {
		return
	}
	*events = append(*events, s.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": s.openIndex,
	}))
	s.openIndex = -1
	s.openKind = ""
}

func (s *StreamState) event(name string, v any) SSEEvent {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"type":"error"}`)
	}
	return SSEEvent{Event: name, Data: b}
}

// AnthropicUsageScanner 从 anthropic 的流式响应里读出用量与结束原因，
// 用于同协议直通时记账，不改动转发的内容。
type AnthropicUsageScanner struct {
	in, out    int64
	stopReason string
	model      string
}

// Feed 送入一条 data 帧。
func (s *AnthropicUsageScanner) Feed(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var env struct {
		Type    string             `json:"type"`
		Message *AnthropicResponse `json:"message"`
		Usage   *Usage             `json:"usage"`
		Delta   *struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	switch env.Type {
	case "message_start":
		if env.Message != nil {
			s.in = env.Message.Usage.InputTokens
			s.out = env.Message.Usage.OutputTokens
			if env.Message.Model != "" {
				s.model = env.Message.Model
			}
		}
	case "message_delta":
		if env.Usage != nil {
			s.out = env.Usage.OutputTokens
			if env.Usage.InputTokens > 0 {
				s.in = env.Usage.InputTokens
			}
		}
		if env.Delta != nil && env.Delta.StopReason != "" {
			s.stopReason = env.Delta.StopReason
		}
	}
}

// Usage 返回读到的用量。
func (s *AnthropicUsageScanner) Usage() (in, out int64) { return s.in, s.out }

// StopReason 返回读到的结束原因。
func (s *AnthropicUsageScanner) StopReason() string { return s.stopReason }
