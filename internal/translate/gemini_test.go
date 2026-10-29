package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEncodeGeminiRequestAllowsStream 钉住流式请求的报文形状：gemini 的流式由路径后缀
// （:streamGenerateContent）与 alt=sse 表示，报文里没有 stream 字段，因此与非流式同一形状。
func TestEncodeGeminiRequestAllowsStream(t *testing.T) {
	raw, err := EncodeGeminiRequest(&Request{
		Stream: true,
		Messages: []Message{{
			Role:   RoleUser,
			Blocks: []Block{{Kind: BlockText, Text: "hi"}},
		}},
	}, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("流式请求应当编码成功，实际 %v", err)
	}
	if strings.Contains(string(raw), "stream") {
		t.Fatalf("gemini 的请求体里不该有 stream 字段: %s", raw)
	}
}

// TestGeminiPathTarget 钉住入站路径的解析：模型名是前缀与动作后缀之间那一段，
// 后缀不算模型名的一部分，`%2F` 按路径段还原，两种后缀各自给出是否流式。
func TestGeminiPathTarget(t *testing.T) {
	cases := []struct {
		path   string
		model  string
		stream bool
		ok     bool
	}{
		{"/v1beta/models/gemini-2.5-flash:generateContent", "gemini-2.5-flash", false, true},
		{"/v1beta/models/gemini-2.5-flash:streamGenerateContent", "gemini-2.5-flash", true, true},
		{"/v1beta/models/org%2Fgemini:generateContent", "org/gemini", false, true},
		{"/v1beta/models/org%2Fgemini:streamGenerateContent", "org/gemini", true, true},
		{"/v1beta/models/:generateContent", "", false, false},
		{"/v1beta/models/gemini-2.5-flash", "", false, false},
		{"/v1beta/models/gemini-2.5-flash:embedContent", "", false, false},
	}
	for _, c := range cases {
		model, stream, ok := GeminiPathTarget(c.path)
		if model != c.model || stream != c.stream || ok != c.ok {
			t.Fatalf("GeminiPathTarget(%q) = %q,%v,%v，期望 %q,%v,%v",
				c.path, model, stream, ok, c.model, c.stream, c.ok)
		}
		// PathProtocol 与它同一个口径：两条路都得说 gemini。
		got, ok := PathProtocol(c.path)
		if c.ok && (!ok || got != ProtocolGemini) {
			t.Fatalf("PathProtocol(%q) = %q,%v，期望 gemini", c.path, got, ok)
		}
		if !c.ok && ok {
			t.Fatalf("PathProtocol(%q) 不该认成任何协议", c.path)
		}
	}
}

// TestGeminiStreamDecoderDeltas 钉住 gemini 上游流式帧折成增量的序列：文本逐帧给，
// functionCall 同一帧里给出 tool_start 与一整段 tool_args，finishReason 给出 stop 与用量。
func TestGeminiStreamDecoderDeltas(t *testing.T) {
	dec, err := NewStreamDecoder(ProtocolGemini)
	if err != nil {
		t.Fatalf("造流式解码器: %v", err)
	}

	frames := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"index":0}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"北京"}}}]},"index":0}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"index":0,"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"cachedContentTokenCount":4}}`,
	}
	var deltas []Delta
	for i, raw := range frames {
		got, err := dec.Decode([]byte(raw))
		if err != nil {
			t.Fatalf("解码第 %d 帧: %v", i+1, err)
		}
		deltas = append(deltas, got...)
	}
	deltas = append(deltas, dec.Close()...)

	want := []Delta{
		{Kind: DeltaText, Text: "po"},
		{Kind: DeltaText, Text: "ng"},
		{Kind: DeltaToolStart, Index: 1, ToolID: "gemini_call_1", ToolName: "weather"},
		{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{"city":"北京"}`},
		{Kind: DeltaStop, StopReason: StopToolUse, Usage: &Usage{In: 12, Out: 8, CacheRead: 4}},
	}
	if len(deltas) != len(want) {
		t.Fatalf("增量数 = %d，期望 %d：%+v", len(deltas), len(want), deltas)
	}
	for i, w := range want {
		g := deltas[i]
		if g.Kind != w.Kind || g.Text != w.Text || g.Index != w.Index || g.ToolID != w.ToolID ||
			g.ToolName != w.ToolName || g.ArgsJSON != w.ArgsJSON || g.StopReason != w.StopReason {
			t.Fatalf("第 %d 个增量 = %+v，期望 %+v", i+1, g, w)
		}
		if w.Usage == nil {
			if g.Usage != nil {
				t.Fatalf("第 %d 个增量不该带用量: %+v", i+1, g.Usage)
			}
			continue
		}
		if g.Usage == nil || *g.Usage != *w.Usage {
			t.Fatalf("第 %d 个增量的用量 = %+v，期望 %+v", i+1, g.Usage, *w.Usage)
		}
	}
}

// TestGeminiStreamDecoderUsageAfterStop 钉住 finishReason 与 usageMetadata 分两帧到达时
// 收尾会补一个只带用量的 stop：用量不能因为先收尾了就丢掉。
func TestGeminiStreamDecoderUsageAfterStop(t *testing.T) {
	dec, err := NewStreamDecoder(ProtocolGemini)
	if err != nil {
		t.Fatalf("造流式解码器: %v", err)
	}
	deltas, err := dec.Decode([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0,"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	if len(deltas) != 2 || deltas[1].Kind != DeltaStop || deltas[1].Usage != nil {
		t.Fatalf("首帧增量 = %+v", deltas)
	}
	if _, err := dec.Decode([]byte(`{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`)); err != nil {
		t.Fatalf("解码用量帧: %v", err)
	}
	tail := dec.Close()
	if len(tail) != 1 || tail[0].Kind != DeltaStop || tail[0].Usage == nil {
		t.Fatalf("收尾增量 = %+v，期望一个带用量的 stop", tail)
	}
	if tail[0].Usage.In != 3 || tail[0].Usage.Out != 2 {
		t.Fatalf("收尾用量 = %+v，期望 3/2", tail[0].Usage)
	}
}

// TestGeminiStreamDecoderBlockedPrompt 钉住提示词被拦：上游只给 promptFeedback，
// 要当成错误增量说出来，与 DecodeGeminiResponse 同一条规则。
func TestGeminiStreamDecoderBlockedPrompt(t *testing.T) {
	dec, err := NewStreamDecoder(ProtocolGemini)
	if err != nil {
		t.Fatalf("造流式解码器: %v", err)
	}
	deltas, err := dec.Decode([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	if len(deltas) != 1 || deltas[0].Kind != DeltaError || !strings.Contains(deltas[0].Text, "SAFETY") {
		t.Fatalf("增量 = %+v，期望一条带 SAFETY 的错误", deltas)
	}
}

// TestGeminiStreamEncoderBuffersToolArgs 钉住 gemini 入站的流式编码：工具参数攒到收尾才
// 一次发出（gemini 的 functionCall 没有增量写法），文本逐帧发出，finishReason 与用量在最后一帧。
func TestGeminiStreamEncoderBuffersToolArgs(t *testing.T) {
	enc, err := NewStreamEncoder(ProtocolGemini, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("造流式编码器: %v", err)
	}
	events := enc.Start()
	if len(events) != 1 || events[0].Event != "" {
		t.Fatalf("首帧 = %+v，期望一帧没有事件名的 data", events)
	}
	for _, d := range []Delta{
		{Kind: DeltaThinking, Text: "想想"},
		{Kind: DeltaToolStart, Index: 1, ToolID: "toolu_1", ToolName: "weather"},
		{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{"city":"北京"}`},
	} {
		if got := enc.Emit(d); len(got) != 0 {
			t.Fatalf("%s 增量不该立刻发帧: %+v", d.Kind, got)
		}
	}
	// stop 只把攒下的工具发出去；带 finishReason 与用量的结束帧在 Finish 才发。
	events = enc.Emit(Delta{Kind: DeltaStop, StopReason: StopToolUse,
		Usage: &Usage{In: 3, Out: 2}})
	if len(events) != 1 || !strings.Contains(string(events[0].Data), "functionCall") {
		t.Fatalf("stop 应当只发工具帧: %+v", events)
	}
	events = enc.Finish()
	if len(events) != 1 {
		t.Fatalf("Finish 应当发一帧结束帧: %+v", events)
	}
	if !strings.Contains(string(events[0].Data), `"finishReason":"STOP"`) ||
		!strings.Contains(string(events[0].Data), "usageMetadata") {
		t.Fatalf("结束帧应当带 finishReason 与用量: %s", events[0].Data)
	}
	if got := enc.Finish(); len(got) != 0 {
		t.Fatalf("已经收尾的流不该再追加帧: %+v", got)
	}
}

// TestGeminiCodecEntries 钉住四个入口都认 gemini：入站能编码响应、能造流式编码器，
// 出站能造流式解码器。
func TestGeminiCodecEntries(t *testing.T) {
	raw, err := EncodeResponse(ProtocolGemini, &Response{
		Model: "gemini-2.5-flash",
		Blocks: []Block{
			{Kind: BlockText, Text: "pong"},
			{Kind: BlockThinking, Thinking: &Thinking{Text: "想想"}},
		},
		StopReason: StopMaxTokens,
		Usage:      Usage{In: 3, Out: 1},
	})
	if err != nil {
		t.Fatalf("编码 gemini 响应: %v", err)
	}
	var wire GeminiResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("编码结果不是 gemini 形状: %v，原文 %s", err, raw)
	}
	if len(wire.Candidates) != 1 || len(wire.Candidates[0].Content.Parts) != 1 ||
		wire.Candidates[0].Content.Parts[0].Text != "pong" {
		t.Fatalf("候选 = %+v", wire.Candidates)
	}
	if wire.Candidates[0].FinishReason != "MAX_TOKENS" {
		t.Fatalf("finishReason = %q", wire.Candidates[0].FinishReason)
	}
	if wire.UsageMetadata == nil || wire.UsageMetadata.PromptTokenCount != 3 ||
		wire.UsageMetadata.TotalTokenCount != 4 {
		t.Fatalf("usageMetadata = %+v", wire.UsageMetadata)
	}

	if _, err := NewStreamEncoder(ProtocolGemini, "m"); err != nil {
		t.Fatalf("gemini 入站应当有流式编码器: %v", err)
	}
	if _, err := NewStreamDecoder(ProtocolGemini); err != nil {
		t.Fatalf("gemini 出站应当有流式解码器: %v", err)
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

// TestGeminiStopNoteKeepsUnmappedReason 钉住非正常 finishReason：中间表示只有四种结束原因，
// SAFETY 一类统一映射成 end_turn，原值放进 StopNote 备查，不编给客户端（DESIGN 5.9）。
func TestGeminiStopNoteKeepsUnmappedReason(t *testing.T) {
	resp, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"role":"model",`+
		`"parts":[{"text":"我不能回答"}]},"finishReason":"SAFETY"}]}`), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	if resp.StopReason != StopEndTurn || resp.StopNote != "SAFETY" {
		t.Fatalf("结束原因 = %q，备注 = %q，期望 end_turn 与 SAFETY", resp.StopReason, resp.StopNote)
	}

	dec, err := NewStreamDecoder(ProtocolGemini)
	if err != nil {
		t.Fatalf("造流式解码器: %v", err)
	}
	deltas, err := dec.Decode([]byte(`{"candidates":[{"content":{"role":"model","parts":[]},` +
		`"index":0,"finishReason":"RECITATION"}]}`))
	if err != nil {
		t.Fatalf("解码流式帧: %v", err)
	}
	if len(deltas) != 1 || deltas[0].StopReason != StopEndTurn || deltas[0].StopNote != "RECITATION" {
		t.Fatalf("增量 = %+v，期望 end_turn 与 RECITATION", deltas)
	}
}

// TestDecodeGeminiRequestAlignsToolCallIDs 钉住入站工具调用的 id 对齐：gemini 只有函数名，
// 编给 anthropic 与 chat 时工具结果必须引用同一轮 tool_use 的 id，否则上游会拒掉整条请求。
// 同名函数被调两次时按出现顺序各对一次（DESIGN 5.9）。
func TestDecodeGeminiRequestAlignsToolCallIDs(t *testing.T) {
	req, err := DecodeGeminiRequest([]byte(`{"contents":[
		{"role":"user","parts":[{"text":"北京和上海天气"}]},
		{"role":"model","parts":[
			{"functionCall":{"name":"weather","args":{"city":"北京"}}},
			{"functionCall":{"name":"weather","args":{"city":"上海"}}}
		]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"weather","response":{"result":"晴"}}},
			{"functionResponse":{"name":"weather","response":{"result":"雨"}}}
		]}
	]}`))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3：%+v", len(req.Messages), req.Messages)
	}
	calls := req.Messages[1].Blocks
	if len(calls) != 2 || calls[0].ToolUse.ID != "gemini_call_1" || calls[1].ToolUse.ID != "gemini_call_2" {
		t.Fatalf("工具调用的 id = %+v，期望按顺序补 gemini_call_1 与 gemini_call_2", calls)
	}
	results := req.Messages[2].Blocks
	if len(results) != 2 || results[0].ToolResult.ToolUseID != "gemini_call_1" ||
		results[1].ToolResult.ToolUseID != "gemini_call_2" {
		t.Fatalf("工具结果的 id = %+v，期望各自对上前面那次调用", results)
	}

	// 上一轮的结果早已对回、这一轮又调了一次同名函数：新的调用拿到新 id，旧队列不再参与。
	req, err = DecodeGeminiRequest([]byte(`{"contents":[
		{"role":"user","parts":[{"text":"再查一次"}]},
		{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"广州"}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"result":"阴"}}}]}
	]}`))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if id := req.Messages[1].Blocks[0].ToolUse.ID; id != "gemini_call_1" {
		t.Fatalf("新一轮调用的 id = %q，期望 gemini_call_1", id)
	}
	if id := req.Messages[2].Blocks[0].ToolResult.ToolUseID; id != "gemini_call_1" {
		t.Fatalf("工具结果的 id = %q，期望对上这一轮的调用", id)
	}
}

// TestDecodeGeminiRequestOrphanToolResultKeepsName 钉住历史里查不到调用的工具结果：退回函数名，
// 不丢整条结果（DESIGN 5.9）。
func TestDecodeGeminiRequestOrphanToolResultKeepsName(t *testing.T) {
	req, err := DecodeGeminiRequest([]byte(`{"contents":[
		{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"result":"晴"}}}]}
	]}`))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if id := req.Messages[0].Blocks[0].ToolResult.ToolUseID; id != "weather" {
		t.Fatalf("孤立工具结果的 id = %q，期望退回函数名", id)
	}
}

// TestGeminiStreamEncoderToolArgsShapes 钉住收尾时工具帧的三种参数形状：空参数照发、合法对象
// 原样写进 args、断在中间的参数整块丢弃（gemini 的 args 是 Struct，写不了半截 JSON）。
func TestGeminiStreamEncoderToolArgsShapes(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantPart bool
	}{
		{"没有参数", "", true},
		{"完整参数", `{"city":"北京"}`, true},
		{"断在中间", `{"city":"北`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc, err := NewStreamEncoder(ProtocolGemini, "gemini-2.5-flash")
			if err != nil {
				t.Fatalf("造流式编码器: %v", err)
			}
			enc.Start()
			enc.Emit(Delta{Kind: DeltaToolStart, Index: 0, ToolName: "weather"})
			if c.args != "" {
				enc.Emit(Delta{Kind: DeltaToolArgs, Index: 0, ArgsJSON: c.args})
			}
			events := append(enc.Emit(Delta{Kind: DeltaStop, StopReason: StopEndTurn}), enc.Finish()...)
			if len(events) == 0 {
				t.Fatal("收尾必须发出结束帧")
			}
			var parts []GeminiPart
			if len(events) == 2 {
				var frame GeminiResponse
				if err := json.Unmarshal(events[0].Data, &frame); err != nil {
					t.Fatalf("工具帧不是合法 JSON: %v", err)
				}
				parts = frame.Candidates[0].Content.Parts
			}
			if got := len(parts) > 0; got != c.wantPart {
				t.Fatalf("工具帧 = %+v，期望有 part = %v", parts, c.wantPart)
			}
			if c.wantPart {
				if parts[0].FunctionCall == nil || parts[0].FunctionCall.Name != "weather" {
					t.Fatalf("工具帧的 part = %+v", parts[0])
				}
				if c.args == "" && len(parts[0].FunctionCall.Args) != 0 {
					t.Fatalf("没有参数时不该写 args: %s", parts[0].FunctionCall.Args)
				}
			}
		})
	}
}

// TestDecodeGeminiRequestDropsBuiltinTools 钉住内置工具：gemini 的 googleSearch 一类只出现在
// tools[] 的对应键上、没有 functionDeclarations，整条丢弃，不伪造也不转成别的协议的工具定义。
func TestDecodeGeminiRequestDropsBuiltinTools(t *testing.T) {
	req, err := DecodeGeminiRequest([]byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"tools":[
			{"googleSearch":{}},
			{"functionDeclarations":[{"name":"weather","description":"查天气"}]}
		]
	}`))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "weather" {
		t.Fatalf("工具定义 = %+v，期望只剩 weather", req.Tools)
	}
	if len(req.Tools[0].Schema) == 0 {
		t.Fatalf("缺参数结构的工具应当补空 schema: %+v", req.Tools[0])
	}
}
