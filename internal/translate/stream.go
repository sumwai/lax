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

// 断流时工具调用的参数可能只到一半（DESIGN 5.4 已知边界）：错误文案要说明这一条，
// 客户端才不会把残缺的 input 当成完整参数。
const incompleteToolArgsNote = "工具调用参数在上游中断，内容不完整"

// withIncompleteToolArgs 在断流的错误文案里补上「参数不完整」这层意思。
// openTool 由各编码器自己给：它知道有没有工具块开了还没收。
func withIncompleteToolArgs(message string, openTool bool) string {
	if !openTool {
		return message
	}
	if message == "" {
		return incompleteToolArgsNote
	}
	return message + "；" + incompleteToolArgsNote
}

// ErrorEvent 构造一条 anthropic 形状的错误事件。
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

// AnthropicUsageScanner 从 anthropic 的响应里读出用量与结束原因（流式帧或非流式响应体），
// 用于同协议直通与探活时记账，不改动转发的内容。
type AnthropicUsageScanner struct {
	in, out    int64
	cacheRead  int64
	cacheWrite int64
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
		Usage   *AnthropicUsage    `json:"usage"`
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
			s.cacheRead = env.Message.Usage.CacheReadInputTokens
			s.cacheWrite = env.Message.Usage.CacheCreationInputTokens
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
	case "message":
		// 非流式响应体：整条 message 一次给全，探活就按它记账。
		if env.Usage != nil {
			s.in = env.Usage.InputTokens
			s.out = env.Usage.OutputTokens
			s.cacheRead = env.Usage.CacheReadInputTokens
			s.cacheWrite = env.Usage.CacheCreationInputTokens
		}
	}
}

// Usage 返回读到的用量。
func (s *AnthropicUsageScanner) Usage() (in, out, cacheRead, cacheWrite int64) {
	return s.in, s.out, s.cacheRead, s.cacheWrite
}

// StopReason 返回读到的结束原因。
func (s *AnthropicUsageScanner) StopReason() string { return s.stopReason }
