package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func eventKind(t *testing.T, e SSEEvent) string {
	t.Helper()
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(e.Data, &env); err != nil {
		t.Fatalf("事件不是合法 JSON: %s", e.Data)
	}
	if env.Type != e.Event {
		t.Fatalf("事件名 %q 与 type %q 不一致", e.Event, env.Type)
	}
	return env.Type
}

func TestChatStreamText(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-1","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"好"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
	}

	st := NewChatStream("claude-x")
	var kinds []string
	var text strings.Builder
	for _, c := range chunks {
		events, err := st.Translate([]byte(c))
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		for _, e := range events {
			kinds = append(kinds, eventKind(t, e))
			var env struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			_ = json.Unmarshal(e.Data, &env)
			text.WriteString(env.Delta.Text)
		}
	}
	for _, e := range st.Finish() {
		kinds = append(kinds, eventKind(t, e))
	}

	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", kinds, want)
	}
	if text.String() != "你好" {
		t.Fatalf("文本 = %q", text.String())
	}
	in, out := st.Usage()
	if in != 9 || out != 2 {
		t.Fatalf("用量 = %d/%d，期望 9/2", in, out)
	}
}

func TestChatStreamStartEventsMatchAnthropicShape(t *testing.T) {
	st := NewChatStream("m")
	events, err := st.Translate([]byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	var start struct {
		Message map[string]any `json:"message"`
	}
	if err := json.Unmarshal(events[0].Data, &start); err != nil {
		t.Fatalf("解析 message_start: %v", err)
	}
	if _, ok := start.Message["stop_reason"]; ok {
		t.Fatalf("message_start 不应带 stop_reason: %s", events[0].Data)
	}

	var block struct {
		ContentBlock map[string]any `json:"content_block"`
	}
	if err := json.Unmarshal(events[1].Data, &block); err != nil {
		t.Fatalf("解析 content_block_start: %v", err)
	}
	if block.ContentBlock["type"] != "text" || block.ContentBlock["text"] != "" {
		t.Fatalf("text 块应带空的 text 字段: %s", events[1].Data)
	}
}

func TestChatStreamToolCalls(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"上海\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	st := NewChatStream("m")
	var kinds []string
	var partials strings.Builder
	var started ContentBlock
	for _, c := range chunks {
		events, err := st.Translate([]byte(c))
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		for _, e := range events {
			kinds = append(kinds, eventKind(t, e))
			switch e.Event {
			case "content_block_start":
				var env struct {
					ContentBlock ContentBlock `json:"content_block"`
				}
				if err := json.Unmarshal(e.Data, &env); err != nil {
					t.Fatalf("解析 content_block_start: %v", err)
				}
				started = env.ContentBlock
			case "content_block_delta":
				var env struct {
					Delta struct {
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal(e.Data, &env); err != nil {
					t.Fatalf("解析 content_block_delta: %v", err)
				}
				partials.WriteString(env.Delta.PartialJSON)
			}
		}
	}
	for _, e := range st.Finish() {
		eventKind(t, e)
	}

	if started.Type != "tool_use" || started.ID != "call_1" || started.Name != "get_weather" {
		t.Fatalf("tool_use 块 = %+v", started)
	}
	if partials.String() != `{"city":"上海"}` {
		t.Fatalf("参数拼接 = %q", partials.String())
	}
	if st.StopReason() != "tool_use" {
		t.Fatalf("stop_reason = %q", st.StopReason())
	}
}

func TestChatStreamFinishWithoutUpstreamStop(t *testing.T) {
	st := NewChatStream("m")
	if _, err := st.Translate([]byte(`{"choices":[{"index":0,"delta":{"content":"半"}}]}`)); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	events := st.Finish()
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, eventKind(t, e))
	}
	want := []string{"content_block_stop", "message_delta", "message_stop"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("收尾事件 = %v，期望 %v", kinds, want)
	}
}

func TestSSEEventBytesFormat(t *testing.T) {
	e := ErrorEvent("api_error", "上游中断")
	out := string(e.Bytes())
	if !strings.HasPrefix(out, "event: error\ndata: ") {
		t.Fatalf("SSE 格式不对: %q", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("SSE 结尾必须是空行: %q", out)
	}
}

func TestScanSSEHandlesCommentsAndMultilineData(t *testing.T) {
	in := ": 注释\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\n" +
		"data:  \"usage\":{\"output_tokens\":3}}\n\n"

	var got []string
	err := ScanSSE(strings.NewReader(in), func(event string, data []byte) error {
		got = append(got, event+"|"+string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("ScanSSE: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("帧数 = %d，期望 1", len(got))
	}
	if !strings.HasPrefix(got[0], "message_delta|") {
		t.Fatalf("事件名 = %q", got[0])
	}
	if !strings.Contains(got[0], "\"output_tokens\":3") {
		t.Fatalf("多行 data 未拼接: %q", got[0])
	}
}

func TestAnthropicUsageScanner(t *testing.T) {
	var sc AnthropicUsageScanner
	sc.Feed([]byte(`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":42,"output_tokens":1}}}`))
	sc.Feed([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`))

	in, out := sc.Usage()
	if in != 42 || out != 17 {
		t.Fatalf("用量 = %d/%d，期望 42/17", in, out)
	}
	if sc.StopReason() != "end_turn" {
		t.Fatalf("stop_reason = %q", sc.StopReason())
	}
}
