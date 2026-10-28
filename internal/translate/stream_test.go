package translate

import (
	"strings"
	"testing"
)

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
	sc.Feed([]byte(`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":42,"output_tokens":1,"cache_read_input_tokens":7}}}`))
	sc.Feed([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`))

	in, out, cacheRead, _ := sc.Usage()
	if in != 42 || out != 17 || cacheRead != 7 {
		t.Fatalf("用量 = %d/%d/缓存 %d，期望 42/17/7", in, out, cacheRead)
	}
	if sc.StopReason() != "end_turn" {
		t.Fatalf("stop_reason = %q", sc.StopReason())
	}
}

func TestNormalizeStop(t *testing.T) {
	cases := map[string]string{
		"":                  StopEndTurn,
		"end_turn":          StopEndTurn,
		"stop":              StopEndTurn,
		"max_tokens":        StopMaxTokens,
		"length":            StopMaxTokens,
		"max_output_tokens": StopMaxTokens,
		"tool_use":          StopToolUse,
		"tool_calls":        StopToolUse,
		"stop_sequence":     StopSequence,
	}
	for in, want := range cases {
		if got := normalizeStop(in); got != want {
			t.Fatalf("normalizeStop(%q) = %q，期望 %q", in, got, want)
		}
	}
}
