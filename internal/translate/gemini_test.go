package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEncodeGeminiRequestRejectsStream 钉住第一步的边界：gemini 出站只做非流式。
// 写出一个非流式请求换回一整段 JSON，客户端拿到的就不是它要的流，宁可当场报错（DESIGN 5.9）。
func TestEncodeGeminiRequestRejectsStream(t *testing.T) {
	_, err := EncodeGeminiRequest(&Request{
		Stream: true,
		Messages: []Message{{
			Role:   RoleUser,
			Blocks: []Block{{Kind: BlockText, Text: "hi"}},
		}},
	}, "gemini-2.5-flash")
	if err == nil || !strings.Contains(err.Error(), "流式") {
		t.Fatalf("流式请求应当报错并说明原因，实际 %v", err)
	}
}

// TestEncodeGeminiRequestDropsOrphanToolResult 钉住工具结果找不到函数名时的处理：
// gemini 的 functionResponse 只按函数名对回调用。查不到名字就整块丢弃，不拿 tool_use_id
// 冒充函数名——那会让上游认为请求里有一个没声明过的函数（DESIGN 5.9）。
func TestEncodeGeminiRequestDropsOrphanToolResult(t *testing.T) {
	raw, err := EncodeGeminiRequest(&Request{
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}},
			{Role: RoleTool, Blocks: []Block{{Kind: BlockToolResult, ToolResult: &ToolResult{
				ToolUseID: "call_missing",
				Text:      "晴",
			}}}},
		},
	}, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	if strings.Contains(string(raw), "functionResponse") {
		t.Fatalf("查不到函数名的工具结果不该写出去: %s", raw)
	}
	if !strings.Contains(string(raw), `{"text":"hi"}`) {
		t.Fatalf("其余内容应当照旧编码: %s", raw)
	}
}

// TestEncodeGeminiRequestOmitsNonObjectArgs 钉住工具参数的边界：gemini 的 args 是对象，
// 非对象的参数整段省略，不让一条参数把整条请求换成 400（DESIGN 5.9 的取舍）。
func TestEncodeGeminiRequestOmitsNonObjectArgs(t *testing.T) {
	raw, err := EncodeGeminiRequest(&Request{
		Messages: []Message{{
			Role: RoleAssistant,
			Blocks: []Block{
				{Kind: BlockToolUse, ToolUse: &ToolUse{Name: "a", Input: json.RawMessage(`"x"`)}},
				{Kind: BlockToolUse, ToolUse: &ToolUse{Name: "b", Input: json.RawMessage(`{"k":1}`)}},
			},
		}},
	}, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	var wire GeminiRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("解析编码结果: %v", err)
	}
	parts := wire.Contents[0].Parts
	if len(parts) != 2 || parts[0].FunctionCall.Args != nil {
		t.Fatalf("非对象参数应当省略 args: %+v", parts[0].FunctionCall)
	}
	if string(parts[1].FunctionCall.Args) != `{"k":1}` {
		t.Fatalf("对象参数应当原样保留: %s", parts[1].FunctionCall.Args)
	}
}

// TestDecodeGeminiResponseBlockedPrompt 钉住「提示词自己被拦」：上游没给候选但给了
// promptFeedback.blockReason，报错要说清是上游拒绝，而不是响应形状不对。
func TestDecodeGeminiResponseBlockedPrompt(t *testing.T) {
	raw := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	_, err := DecodeGeminiResponse([]byte(raw), "gemini-2.5-flash")
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("被拦的提示词应当报出 blockReason，实际 %v", err)
	}

	if _, err := DecodeGeminiResponse([]byte(`{}`), "gemini-2.5-flash"); err == nil {
		t.Fatal("没有 candidates 也没有 blockReason 时应当报错")
	}
}

// TestGeminiUsageScanner 钉住用量读取：promptTokenCount、candidatesTokenCount 与
// cachedContentTokenCount 三项各自落到 In、Out、CacheRead，缓存写恒为 0。
func TestGeminiUsageScanner(t *testing.T) {
	scanner, err := NewUsageScanner(ProtocolGemini)
	if err != nil {
		t.Fatalf("造用量读取器: %v", err)
	}
	scanner.Feed([]byte(`{"candidates":[{"content":{"parts":[{"text":"pong"}]}}],` +
		`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":7,"cachedContentTokenCount":4}}`))
	in, out, cacheRead, cacheWrite := scanner.Usage()
	if in != 12 || out != 7 || cacheRead != 4 || cacheWrite != 0 {
		t.Fatalf("用量 = %d/%d/%d/%d，期望 12/7/4/0", in, out, cacheRead, cacheWrite)
	}
}

// TestGeminiHasNoInboundOrStreamCodec 钉住第一步的范围：gemini 没有入站路径，也没有
// 流式编解码器。新加这两个入口时这条用例会失败，提醒同步 DESIGN 5.9 与对拍用例。
func TestGeminiHasNoInboundOrStreamCodec(t *testing.T) {
	if _, err := EncodeResponse(ProtocolGemini, &Response{}); err == nil {
		t.Fatal("gemini 没有入站路径，不该能编码客户端响应")
	}
	if _, err := NewStreamEncoder(ProtocolGemini, "m"); err == nil {
		t.Fatal("gemini 没有入站路径，不该有流式编码器")
	}
	if _, err := NewStreamDecoder(ProtocolGemini); err == nil {
		t.Fatal("gemini 出站还没有流式解码器（第一步只做非流式）")
	}
}
