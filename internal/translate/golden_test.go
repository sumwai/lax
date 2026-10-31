package translate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenCase 是对拍用例的信封。from 是入站（客户端）协议，to 是出站（上游）协议：
// 请求由 from 解码、往 to 编码；响应与流式由 to 解码、往 from 编码。
// 用例名说明场景，覆盖各协议之间的映射表。
type goldenCase struct {
	From   string            `json:"from"`
	To     string            `json:"to"`
	Kind   string            `json:"kind"`
	Model  string            `json:"model"`
	Body   json.RawMessage   `json:"body"`
	Frames []json.RawMessage `json:"frames"`
}

// TestGolden 逐个跑 testdata/in 下的用例，与 testdata/out 下的期望输出对拍。
// 比较的是规范化后的 JSON，因此报文里的键顺序与空白不影响结论。
func TestGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "in", "*.json"))
	if err != nil {
		t.Fatalf("列出用例: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatal("testdata/in 里没有对拍用例")
	}

	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".json")
		t.Run(name, func(t *testing.T) {
			var c goldenCase
			if err := json.Unmarshal(readGoldenFile(t, in), &c); err != nil {
				t.Fatalf("用例文件不是合法 JSON: %v", err)
			}
			got := canonicalGolden(t, runGolden(t, c))
			want := canonicalGolden(t, decodeGolden(t, readGoldenFile(t, filepath.Join("testdata", "out", name+".json"))))
			if got != want {
				t.Fatalf("%s\n得到:\n%s\n期望:\n%s", name, got, want)
			}
		})
	}
}

// runGolden 按用例种类跑一遍编解码，返回实际产出的报文。
func runGolden(t *testing.T, c goldenCase) any {
	t.Helper()
	switch c.Kind {
	case "request":
		req, err := DecodeRequest(c.From, c.Body)
		if err != nil {
			t.Fatalf("解码请求: %v", err)
		}
		raw, err := EncodeRequest(c.To, req, c.Model)
		if err != nil {
			t.Fatalf("编码请求: %v", err)
		}
		return decodeGolden(t, raw)

	case "response":
		resp, err := DecodeResponse(c.To, c.Body, c.Model)
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		raw, err := EncodeResponse(c.From, resp)
		if err != nil {
			t.Fatalf("编码响应: %v", err)
		}
		return decodeGolden(t, raw)

	case "stream":
		return runGoldenStream(t, c)

	default:
		t.Fatalf("未知的用例种类 %q，只认 request、response、stream", c.Kind)
		return nil
	}
}

// runGoldenStream 按事件推进：上游帧逐条解码成增量，增量逐条编成客户端事件。
// 收尾走 Close 与 Finish，与 gateway 的转换路径同一套顺序。
func runGoldenStream(t *testing.T, c goldenCase) any {
	t.Helper()
	dec, err := NewStreamDecoder(c.To)
	if err != nil {
		t.Fatalf("造流式解码器: %v", err)
	}
	enc, err := NewStreamEncoder(c.From, c.Model)
	if err != nil {
		t.Fatalf("造流式编码器: %v", err)
	}

	events := enc.Start()
	for i, frame := range c.Frames {
		deltas, err := dec.Decode(goldenFrameData(t, frame))
		if err != nil {
			t.Fatalf("解码第 %d 帧: %v", i+1, err)
		}
		for _, d := range deltas {
			events = append(events, enc.Emit(d)...)
		}
	}
	for _, d := range dec.Close() {
		events = append(events, enc.Emit(d)...)
	}
	events = append(events, enc.Finish()...)

	// 期望输出的形状：每帧是 {event, data}，chat 下发的帧没有事件名，只有 data。
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		item := map[string]any{"data": decodeGolden(t, e.Data)}
		if e.Event != "" {
			item["event"] = e.Event
		}
		out = append(out, item)
	}
	return out
}

// goldenFrameData 取一帧的 data 内容：字符串帧（例如 chat 的 [DONE]）按原文，
// 其它帧按 JSON 原文。
func goldenFrameData(t *testing.T, frame json.RawMessage) []byte {
	t.Helper()
	raw := bytes.TrimSpace(frame)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("帧不是合法字符串: %v", err)
		}
		return []byte(s)
	}
	return raw
}

func readGoldenFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v；新增用例时先建 testdata/in 下的输入与 testdata/out 下的期望输出", path, err)
	}
	return raw
}

// decodeGolden 把报文读成通用值：不是 JSON 的内容（例如 [DONE]）按原文返回。
func decodeGolden(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// canonicalGolden 把通用值重新序列化成带缩进的 JSON，用来比对。
func canonicalGolden(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化比对值: %v", err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		t.Fatalf("缩进比对值: %v", err)
	}
	return out.String()
}
