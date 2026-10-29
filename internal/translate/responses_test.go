package translate

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func responsesF64Ptr(v float64) *float64 { return &v }

func responsesBoolPtr(v bool) *bool { return &v }

// responsesEventType 断言事件名与 data 里的 type 一致，并返回 type。
func responsesEventType(t *testing.T, e SSEEvent) string {
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

// TestDecodeResponsesRequest 覆盖入站请求的字段映射。
func TestDecodeResponsesRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    *Request
		wantErr bool
	}{
		{
			name: "instructions 与字符串输入",
			body: `{"model":"gpt-5","instructions":"只回答中文","input":"你好"}`,
			want: &Request{
				Model:  "gpt-5",
				System: []Block{{Kind: BlockText, Text: "只回答中文"}},
				Messages: []Message{
					{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "你好"}}},
				},
			},
		},
		{
			name: "item 数组含工具调用与工具结果",
			body: `{"model":"gpt-5","input":[
				{"type":"message","role":"user","content":[
					{"type":"input_text","text":"天气如何"},
					{"type":"input_image","image_url":"https://example.com/a.png"}
				]},
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
				{"type":"function_call_output","call_id":"call_1","output":"晴"}
			]}`,
			want: &Request{
				Model: "gpt-5",
				Messages: []Message{
					{Role: RoleUser, Blocks: []Block{
						{Kind: BlockText, Text: "天气如何"},
						{Kind: BlockImage, Image: &Image{URL: "https://example.com/a.png"}},
					}},
					{Role: RoleAssistant, Blocks: []Block{
						{Kind: BlockToolUse, ToolUse: &ToolUse{
							ID:    "call_1",
							Name:  "get_weather",
							Input: json.RawMessage(`{"city":"北京"}`),
						}},
					}},
					{Role: RoleTool, Blocks: []Block{
						{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Text: "晴"}},
					}},
				},
			},
		},
		{
			name: "tools 与 tool_choice 及采样参数",
			body: `{"model":"gpt-5","input":[],
				"tools":[
					{"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object","properties":{}}},
					{"type":"web_search"}
				],
				"tool_choice":"required",
				"max_output_tokens":256,
				"temperature":0.3,
				"top_p":0.9,
				"stream":true,
				"parallel_tool_calls":false}`,
			want: &Request{
				Model: "gpt-5",
				Tools: []Tool{{
					Name:        "get_weather",
					Description: "查天气",
					Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
				}},
				ToolChoice:        &ToolChoice{Mode: ToolChoiceRequired},
				MaxTokens:         256,
				Temperature:       responsesF64Ptr(0.3),
				TopP:              responsesF64Ptr(0.9),
				Stream:            true,
				ParallelToolCalls: responsesBoolPtr(false),
			},
		},
		{
			name: "命名工具选择",
			body: `{"model":"gpt-5","input":"你好","tool_choice":{"type":"function","name":"get_weather"}}`,
			want: &Request{
				Model:      "gpt-5",
				Messages:   []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "你好"}}}},
				ToolChoice: &ToolChoice{Mode: ToolChoiceNamed, Name: "get_weather"},
			},
		},
		{
			name: "tool_choice 为 auto",
			body: `{"model":"gpt-5","input":"你好","tool_choice":"auto"}`,
			want: &Request{
				Model:      "gpt-5",
				Messages:   []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "你好"}}}},
				ToolChoice: &ToolChoice{Mode: ToolChoiceAuto},
			},
		},
		{
			name:    "非法 JSON",
			body:    `{"model":`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeResponsesRequest([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeResponsesRequest: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("解码结果不符\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// TestEncodeResponsesRequest 检查请求编码的关键字段与 item 顺序。
func TestEncodeResponsesRequest(t *testing.T) {
	req := &Request{
		System: []Block{
			{Kind: BlockText, Text: "系统设定"},
		},
		Messages: []Message{
			{Role: RoleSystem, Blocks: []Block{{Kind: BlockText, Text: "补充"}}},
			{Role: RoleUser, Blocks: []Block{
				{Kind: BlockText, Text: "你好"},
				{Kind: BlockImage, Image: &Image{URL: "https://example.com/a.png"}},
			}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockText, Text: "稍等"},
				{Kind: BlockToolUse, ToolUse: &ToolUse{
					ID:    "call_1",
					Name:  "get_weather",
					Input: json.RawMessage(`{"city":"北京"}`),
				}},
			}},
			{Role: RoleTool, Blocks: []Block{
				{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Text: "晴"}},
			}},
		},
		Tools: []Tool{{
			Name:        "get_weather",
			Description: "查天气",
			Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		}},
		ToolChoice:        &ToolChoice{Mode: ToolChoiceRequired},
		ParallelToolCalls: responsesBoolPtr(false),
		MaxTokens:         128,
		Temperature:       responsesF64Ptr(0.2),
		Stream:            true,
	}

	raw, err := EncodeResponsesRequest(req, "up-model")
	if err != nil {
		t.Fatalf("EncodeResponsesRequest: %v", err)
	}

	var got struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"input"`
		Tools []struct {
			Type        string         `json:"type"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"tools"`
		ToolChoice        string   `json:"tool_choice"`
		MaxOutputTokens   int      `json:"max_output_tokens"`
		Temperature       *float64 `json:"temperature"`
		Stream            bool     `json:"stream"`
		ParallelToolCalls *bool    `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v", err)
	}

	if got.Model != "up-model" {
		t.Fatalf("model = %q, 期望 up-model", got.Model)
	}
	if got.Instructions != "系统设定\n补充" {
		t.Fatalf("instructions = %q", got.Instructions)
	}
	if got.MaxOutputTokens != 128 || !got.Stream {
		t.Fatalf("max_output_tokens=%d stream=%v", got.MaxOutputTokens, got.Stream)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Fatalf("temperature = %v", got.Temperature)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v", got.ParallelToolCalls)
	}
	if got.ToolChoice != "required" {
		t.Fatalf("tool_choice = %q", got.ToolChoice)
	}

	if len(got.Tools) != 1 {
		t.Fatalf("tools 数量 = %d", len(got.Tools))
	}
	if got.Tools[0].Type != "function" || got.Tools[0].Name != "get_weather" ||
		got.Tools[0].Description != "查天气" || got.Tools[0].Parameters["type"] != "object" {
		t.Fatalf("tools[0] = %+v", got.Tools[0])
	}

	if len(got.Input) != 4 {
		t.Fatalf("input 项数 = %d, 期望 4: %+v", len(got.Input), got.Input)
	}
	user := got.Input[0]
	if user.Type != "message" || user.Role != "user" || len(user.Content) != 2 {
		t.Fatalf("input[0] = %+v", user)
	}
	if user.Content[0].Type != "input_text" || user.Content[0].Text != "你好" {
		t.Fatalf("input[0].content[0] = %+v", user.Content[0])
	}
	if user.Content[1].Type != "input_image" || user.Content[1].ImageURL != "https://example.com/a.png" {
		t.Fatalf("input[0].content[1] = %+v", user.Content[1])
	}

	assistant := got.Input[1]
	if assistant.Type != "message" || assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("input[1] = %+v", assistant)
	}
	if assistant.Content[0].Type != "output_text" || assistant.Content[0].Text != "稍等" {
		t.Fatalf("input[1].content[0] = %+v", assistant.Content[0])
	}

	call := got.Input[2]
	if call.Type != "function_call" || call.CallID != "call_1" || call.Name != "get_weather" {
		t.Fatalf("input[2] = %+v", call)
	}
	if !json.Valid([]byte(call.Arguments)) {
		t.Fatalf("function_call 的 arguments 不是合法 JSON: %q", call.Arguments)
	}

	result := got.Input[3]
	if result.Type != "function_call_output" || result.CallID != "call_1" || result.Output != "晴" {
		t.Fatalf("input[3] = %+v", result)
	}
}

// TestDecodeResponsesResponse 覆盖非流式响应的文本、工具调用、未完成与用量。
func TestDecodeResponsesResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    *Response
		wantErr bool
	}{
		{
			name: "纯文本与用量",
			body: `{"id":"resp_1","model":"gpt-5","status":"completed",
				"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"你好"}]}],
				"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13,
					"input_tokens_details":{"cached_tokens":4}}}`,
			want: &Response{
				ID:         "resp_1",
				Model:      "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "你好"}},
				StopReason: StopEndTurn,
				Usage:      Usage{In: 10, Out: 3, CacheRead: 4},
			},
		},
		{
			name: "工具调用",
			body: `{"id":"resp_2","status":"completed",
				"output":[{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"上海\"}"}]}`,
			want: &Response{
				ID:    "resp_2",
				Model: "client-model",
				Blocks: []Block{{Kind: BlockToolUse, ToolUse: &ToolUse{
					ID:    "call_9",
					Name:  "get_weather",
					Input: json.RawMessage(`{"city":"上海"}`),
				}}},
				StopReason: StopToolUse,
			},
		},
		{
			name: "未完成且输出超长",
			body: `{"id":"resp_3","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
				"output":[{"type":"message","content":[{"type":"output_text","text":"半句"}]}],
				"usage":{"input_tokens":5,"output_tokens":7}}`,
			want: &Response{
				ID:         "resp_3",
				Model:      "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "半句"}},
				StopReason: StopMaxTokens,
				Usage:      Usage{In: 5, Out: 7},
			},
		},
		{
			name:    "工具参数不是合法 JSON",
			body:    `{"id":"resp_4","status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{"}]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeResponsesResponse([]byte(tc.body), "client-model")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeResponsesResponse: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("解码结果不符\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// TestEncodeResponsesResponse 检查响应编码的字段，并与解码器对拍。
func TestEncodeResponsesResponse(t *testing.T) {
	resp := &Response{
		ID:    "resp_1",
		Model: "gpt-5",
		Blocks: []Block{
			{Kind: BlockText, Text: "你好"},
			{Kind: BlockToolUse, ToolUse: &ToolUse{
				ID:    "call_1",
				Name:  "get_weather",
				Input: json.RawMessage(`{"city":"北京"}`),
			}},
		},
		StopReason: StopToolUse,
		Usage:      Usage{In: 10, Out: 3, CacheRead: 4},
	}

	raw, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponsesResponse: %v", err)
	}

	var got struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			TotalTokens        int64 `json:"total_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v", err)
	}

	if got.ID != "resp_1" || got.Object != "response" || got.Status != "completed" || got.Model != "gpt-5" {
		t.Fatalf("响应头不符: %+v", got)
	}
	if len(got.Output) != 2 {
		t.Fatalf("output 项数 = %d", len(got.Output))
	}
	if got.Output[0].Type != "message" || got.Output[0].Role != "assistant" ||
		len(got.Output[0].Content) != 1 || got.Output[0].Content[0].Type != "output_text" ||
		got.Output[0].Content[0].Text != "你好" {
		t.Fatalf("output[0] = %+v", got.Output[0])
	}
	if got.Output[1].Type != "function_call" || got.Output[1].CallID != "call_1" ||
		got.Output[1].Name != "get_weather" || !json.Valid([]byte(got.Output[1].Arguments)) {
		t.Fatalf("output[1] = %+v", got.Output[1])
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 3 || got.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.Usage.InputTokensDetails == nil || got.Usage.InputTokensDetails.CachedTokens != 4 {
		t.Fatalf("input_tokens_details = %+v", got.Usage.InputTokensDetails)
	}

	back, err := DecodeResponsesResponse(raw, "gpt-5")
	if err != nil {
		t.Fatalf("解码自己编出的响应失败: %v", err)
	}
	if back.StopReason != StopToolUse {
		t.Fatalf("回读停止原因 = %q", back.StopReason)
	}
	if len(back.Blocks) != 2 || back.Blocks[1].Kind != BlockToolUse || back.Blocks[1].ToolUse.ID != "call_1" {
		t.Fatalf("回读内容块 = %+v", back.Blocks)
	}

	// 输出超长时写成 incomplete，并把原因带回 incomplete_details。
	raw, err = EncodeResponsesResponse(&Response{ID: "resp_2", Model: "gpt-5", StopReason: StopMaxTokens})
	if err != nil {
		t.Fatalf("EncodeResponsesResponse: %v", err)
	}
	var truncated struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &truncated); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v", err)
	}
	if truncated.Status != "incomplete" || truncated.IncompleteDetails == nil ||
		truncated.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("截断响应 = %+v", truncated)
	}
}

// TestResponsesStreamDecode 覆盖文本增量、工具参数增量与带用量的收尾。
func TestResponsesStreamDecode(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.in_progress","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"你"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"好"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"city\":\""}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"北京\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}],"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}`,
	}

	d := &responsesStreamDecoder{}
	var deltas []Delta
	for _, frame := range frames {
		got, err := d.Decode([]byte(frame))
		if err != nil {
			t.Fatalf("Decode(%s): %v", frame, err)
		}
		deltas = append(deltas, got...)
	}
	if extra := d.Close(); len(extra) != 0 {
		t.Fatalf("收到 completed 后 Close 不应再产出增量: %+v", extra)
	}

	var text, args strings.Builder
	var starts, stops []Delta
	for _, dl := range deltas {
		switch dl.Kind {
		case DeltaText:
			text.WriteString(dl.Text)
		case DeltaToolStart:
			starts = append(starts, dl)
		case DeltaToolArgs:
			args.WriteString(dl.ArgsJSON)
		case DeltaStop:
			stops = append(stops, dl)
		default:
			t.Fatalf("意外增量: %+v", dl)
		}
	}

	if text.String() != "你好" {
		t.Fatalf("文本增量 = %q", text.String())
	}
	if len(starts) != 1 || starts[0].ToolID != "call_1" || starts[0].ToolName != "get_weather" || starts[0].Index != 1 {
		t.Fatalf("工具开始增量 = %+v", starts)
	}
	if args.String() != `{"city":"北京"}` {
		t.Fatalf("工具参数增量 = %q", args.String())
	}
	if len(stops) != 1 {
		t.Fatalf("收尾增量数量 = %d", len(stops))
	}
	stop := stops[0]
	if stop.StopReason != StopToolUse {
		t.Fatalf("停止原因 = %q", stop.StopReason)
	}
	if stop.Usage == nil || stop.Usage.In != 10 || stop.Usage.Out != 2 || stop.Usage.CacheRead != 4 {
		t.Fatalf("收尾用量 = %+v", stop.Usage)
	}
}

// TestResponsesStreamDecodeClose 覆盖上游没有 completed 时的补齐与未完成映射。
func TestResponsesStreamDecodeClose(t *testing.T) {
	d := &responsesStreamDecoder{}
	if _, err := d.Decode([]byte(`{"type":"response.output_text.delta","delta":"半"}`)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := d.Close()
	if len(got) != 1 || got[0].Kind != DeltaStop || got[0].StopReason != StopEndTurn {
		t.Fatalf("Close 补齐 = %+v", got)
	}
	if again := d.Close(); len(again) != 0 {
		t.Fatalf("重复 Close 不应再产出: %+v", again)
	}

	d = &responsesStreamDecoder{}
	got, err := d.Decode([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":5,"output_tokens":7}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got) != 1 || got[0].Kind != DeltaStop || got[0].StopReason != StopMaxTokens {
		t.Fatalf("incomplete 收尾 = %+v", got)
	}
	if got[0].Usage == nil || got[0].Usage.In != 5 || got[0].Usage.Out != 7 {
		t.Fatalf("incomplete 用量 = %+v", got[0].Usage)
	}
	if extra := d.Close(); len(extra) != 0 {
		t.Fatalf("incomplete 之后 Close 不应再产出: %+v", extra)
	}
}

// TestResponsesStreamEncode 检查事件名、data 里的 type 与 output 序号。
func TestResponsesStreamEncode(t *testing.T) {
	e := newResponsesStreamEncoder("gpt-5")

	start := e.Start()
	if len(start) != 1 || start[0].Event != "response.created" {
		t.Fatalf("Start = %+v", start)
	}
	responsesEventType(t, start[0])

	textEvents := e.Emit(Delta{Kind: DeltaText, Text: "你好"})
	if len(textEvents) != 2 || textEvents[0].Event != "response.output_item.added" ||
		textEvents[1].Event != "response.output_text.delta" {
		t.Fatalf("文本事件 = %+v，期望先开 message item 再发增量", textEvents)
	}
	responsesEventType(t, textEvents[0])
	responsesEventType(t, textEvents[1])
	var addedData struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Content []any  `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(textEvents[0].Data, &addedData); err != nil {
		t.Fatalf("文本 item 的 added 不是合法 JSON: %v", err)
	}
	if addedData.OutputIndex != 0 || addedData.Item.Type != "message" ||
		addedData.Item.Status != "in_progress" || len(addedData.Item.Content) != 0 {
		t.Fatalf("文本 item 的 added 字段 = %+v，期望 message/in_progress 与空 content", addedData)
	}
	var textData struct {
		OutputIndex  int    `json:"output_index"`
		ContentIndex int    `json:"content_index"`
		Delta        string `json:"delta"`
	}
	if err := json.Unmarshal(textEvents[1].Data, &textData); err != nil {
		t.Fatalf("文本事件不是合法 JSON: %v", err)
	}
	if textData.Delta != "你好" || textData.OutputIndex != 0 || textData.ContentIndex != 0 {
		t.Fatalf("文本事件字段 = %+v", textData)
	}

	startEvents := e.Emit(Delta{Kind: DeltaToolStart, Index: 0, ToolID: "call_1", ToolName: "get_weather"})
	if len(startEvents) != 1 || startEvents[0].Event != "response.output_item.added" {
		t.Fatalf("工具开始事件 = %+v", startEvents)
	}
	responsesEventType(t, startEvents[0])
	var startData struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal(startEvents[0].Data, &startData); err != nil {
		t.Fatalf("工具开始事件不是合法 JSON: %v", err)
	}
	if startData.OutputIndex != 1 || startData.Item.Type != "function_call" ||
		startData.Item.CallID != "call_1" || startData.Item.Name != "get_weather" ||
		startData.Item.Arguments != "" {
		t.Fatalf("工具开始事件字段 = %+v", startData)
	}

	argEvents := e.Emit(Delta{Kind: DeltaToolArgs, Index: 0, ArgsJSON: `{"city":"北京"}`})
	if len(argEvents) != 1 || argEvents[0].Event != "response.function_call_arguments.delta" {
		t.Fatalf("工具参数事件 = %+v", argEvents)
	}
	responsesEventType(t, argEvents[0])
	var argData struct {
		OutputIndex int    `json:"output_index"`
		Delta       string `json:"delta"`
	}
	if err := json.Unmarshal(argEvents[0].Data, &argData); err != nil {
		t.Fatalf("工具参数事件不是合法 JSON: %v", err)
	}
	if argData.OutputIndex != 1 || argData.Delta != `{"city":"北京"}` {
		t.Fatalf("工具参数事件字段 = %+v", argData)
	}

	stopEvents := e.Emit(Delta{Kind: DeltaStop, StopReason: StopEndTurn, Usage: &Usage{In: 10, Out: 2}})
	// 收尾按 output 序号逐条补 done：文本 item 先关（序号 0），再是工具 item（序号 1）。
	if len(stopEvents) != 4 {
		t.Fatalf("收尾应发文本 output_item.done、arguments.done、工具 output_item.done 与 completed 四帧，实际 %d", len(stopEvents))
	}
	if stopEvents[0].Event != "response.output_item.done" ||
		stopEvents[1].Event != "response.function_call_arguments.done" ||
		stopEvents[2].Event != "response.output_item.done" ||
		stopEvents[3].Event != "response.completed" {
		t.Fatalf("收尾事件顺序 = %s/%s/%s/%s",
			stopEvents[0].Event, stopEvents[1].Event, stopEvents[2].Event, stopEvents[3].Event)
	}
	for _, ev := range stopEvents {
		responsesEventType(t, ev)
	}
	var textDone struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(stopEvents[0].Data, &textDone); err != nil {
		t.Fatalf("文本 output_item.done 不是合法 JSON: %v", err)
	}
	if textDone.OutputIndex != 0 || textDone.Item.Type != "message" || textDone.Item.Status != "completed" ||
		len(textDone.Item.Content) != 1 || textDone.Item.Content[0].Type != "output_text" ||
		textDone.Item.Content[0].Text != "你好" {
		t.Fatalf("文本 output_item.done = %+v，期望 message/completed 与全文", textDone)
	}
	var doneData struct {
		OutputIndex int    `json:"output_index"`
		ItemID      string `json:"item_id"`
		Name        string `json:"name"`
		Arguments   string `json:"arguments"`
	}
	if err := json.Unmarshal(stopEvents[1].Data, &doneData); err != nil {
		t.Fatalf("arguments.done 不是合法 JSON: %v", err)
	}
	if doneData.OutputIndex != 1 || doneData.Name != "get_weather" ||
		doneData.Arguments != `{"city":"北京"}` || !json.Valid([]byte(doneData.Arguments)) {
		t.Fatalf("arguments.done 字段 = %+v", doneData)
	}
	var itemDone struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		} `json:"item"`
	}
	if err := json.Unmarshal(stopEvents[2].Data, &itemDone); err != nil {
		t.Fatalf("output_item.done 不是合法 JSON: %v", err)
	}
	if itemDone.OutputIndex != 1 || itemDone.Item.Type != "function_call" ||
		itemDone.Item.CallID != "call_1" || itemDone.Item.Name != "get_weather" ||
		itemDone.Item.Arguments != `{"city":"北京"}` || itemDone.Item.Status != "completed" {
		t.Fatalf("output_item.done 字段 = %+v", itemDone)
	}

	stopData := struct {
		Response struct {
			Status string `json:"status"`
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}{}
	if err := json.Unmarshal(stopEvents[3].Data, &stopData); err != nil {
		t.Fatalf("收尾事件不是合法 JSON: %v", err)
	}
	if stopData.Response.Status != "completed" ||
		stopData.Response.Usage.InputTokens != 10 ||
		stopData.Response.Usage.OutputTokens != 2 ||
		stopData.Response.Usage.TotalTokens != 12 {
		t.Fatalf("收尾事件字段 = %+v", stopData.Response)
	}
	// output 要回填文本 item 与完整的 function_call，不能一直是空数组：按 output_index 排序，
	// 文本 item（序号 0）在工具 item（序号 1）之前。
	if len(stopData.Response.Output) != 2 {
		t.Fatalf("收尾事件的 output = %+v，期望一个 message 与一个 function_call", stopData.Response.Output)
	}
	if msg := stopData.Response.Output[0]; msg.Type != "message" || len(msg.Content) != 1 ||
		msg.Content[0].Type != "output_text" || msg.Content[0].Text != "你好" {
		t.Fatalf("收尾事件 output[0] = %+v，期望正文为「你好」的 message", msg)
	}
	call := stopData.Response.Output[1]
	if call.Type != "function_call" || call.CallID != "call_1" || call.Name != "get_weather" ||
		call.Arguments != `{"city":"北京"}` || !json.Valid([]byte(call.Arguments)) {
		t.Fatalf("收尾事件 output[1] = %+v", call)
	}

	if after := e.Emit(Delta{Kind: DeltaText, Text: "多余"}); len(after) != 0 {
		t.Fatalf("completed 之后不应再发事件: %+v", after)
	}
	if finish := e.Finish(); len(finish) != 0 {
		t.Fatalf("completed 之后 Finish 不应再发事件: %+v", finish)
	}
}

// TestResponsesStreamEncodeFinish 覆盖没收到 DeltaStop 时的收尾。
func TestResponsesStreamEncodeFinish(t *testing.T) {
	e := newResponsesStreamEncoder("gpt-5")
	if events := e.Emit(Delta{Kind: DeltaText, Text: "你"}); len(events) != 3 {
		// 一帧 response.created、一帧文本 item 的 output_item.added 加一帧文本增量。
		t.Fatalf("Emit = %+v", events)
	}

	finish := e.Finish()
	if len(finish) != 2 || finish[0].Event != "response.output_item.done" || finish[1].Event != "response.completed" {
		t.Fatalf("Finish = %+v，期望先补文本 output_item.done 再发 completed", finish)
	}
	responsesEventType(t, finish[0])
	responsesEventType(t, finish[1])
	if again := e.Finish(); len(again) != 0 {
		t.Fatalf("重复 Finish 不应再发事件: %+v", again)
	}
}

// TestResponsesUsageScanner 覆盖直通流里读用量。
func TestResponsesUsageScanner(t *testing.T) {
	var sc responsesUsageScanner
	if in, out, cacheRead, cacheWrite := sc.Usage(); in != 0 || out != 0 || cacheRead != 0 || cacheWrite != 0 {
		t.Fatalf("初始用量 = %d/%d/%d/%d", in, out, cacheRead, cacheWrite)
	}

	sc.Feed([]byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
	sc.Feed([]byte(`{"type":"response.output_text.delta","delta":"x"}`))
	sc.Feed([]byte(`[DONE]`))
	sc.Feed([]byte(`不是 JSON`))
	if in, out, _, _ := sc.Usage(); in != 0 || out != 0 {
		t.Fatalf("收尾前用量 = %d/%d", in, out)
	}

	sc.Feed([]byte(`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16,"input_tokens_details":{"cached_tokens":3}}}}`))
	in, out, cacheRead, cacheWrite := sc.Usage()
	if in != 11 || out != 5 || cacheRead != 3 || cacheWrite != 0 {
		t.Fatalf("用量 = %d/%d/%d/%d", in, out, cacheRead, cacheWrite)
	}

	var incomplete responsesUsageScanner
	incomplete.Feed([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":7,"output_tokens":2}}}`))
	in, out, cacheRead, cacheWrite = incomplete.Usage()
	if in != 7 || out != 2 || cacheRead != 0 || cacheWrite != 0 {
		t.Fatalf("incomplete 用量 = %d/%d/%d/%d", in, out, cacheRead, cacheWrite)
	}
}

// TestResponsesUsageScannerNonStream 确认非流式响应体里的顶层 usage 也能读到，探活记账走这条。
func TestResponsesUsageScannerNonStream(t *testing.T) {
	var sc responsesUsageScanner
	sc.Feed([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":1,"total_tokens":10,"input_tokens_details":{"cached_tokens":4}}}`))
	in, out, cacheRead, cacheWrite := sc.Usage()
	if in != 9 || out != 1 || cacheRead != 4 || cacheWrite != 0 {
		t.Fatalf("用量 = %d/%d/%d/%d，期望 9/1/4/0", in, out, cacheRead, cacheWrite)
	}
}

// TestResponsesStreamDecodeItemDoneOnly 覆盖上游只发 response.output_item.done 的形状：
// 没有 added、也没有 function_call_arguments.delta 时，整条工具调用要在 done 里补齐。
func TestResponsesStreamDecodeItemDoneOnly(t *testing.T) {
	d := &responsesStreamDecoder{}
	frames := []string{
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"稍等"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`,
	}
	var deltas []Delta
	for _, f := range frames {
		got, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode(%s): %v", f, err)
		}
		deltas = append(deltas, got...)
	}

	var starts, args []Delta
	for _, dl := range deltas {
		switch dl.Kind {
		case DeltaToolStart:
			starts = append(starts, dl)
		case DeltaToolArgs:
			args = append(args, dl)
		}
	}
	if len(starts) != 1 || starts[0].ToolID != "call_1" || starts[0].ToolName != "get_weather" ||
		starts[0].Index != 1 {
		t.Fatalf("工具开始增量 = %+v", starts)
	}
	if len(args) != 1 || args[0].ArgsJSON != `{"city":"北京"}` || args[0].Index != 1 {
		t.Fatalf("工具参数增量 = %+v", args)
	}
	if last := deltas[len(deltas)-1]; last.Kind != DeltaStop || last.StopReason != StopToolUse {
		t.Fatalf("收尾增量 = %+v，期望 tool_use", last)
	}
}

// TestResponsesStreamDecodeItemDoneAfterDeltas 确认 added 与 delta 都到过之后，done 不再重复发增量。
func TestResponsesStreamDecodeItemDoneAfterDeltas(t *testing.T) {
	d := &responsesStreamDecoder{}
	frames := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"city\":\"北"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"京\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}","status":"completed"}}`,
	}
	var deltas []Delta
	for _, f := range frames {
		got, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode(%s): %v", f, err)
		}
		deltas = append(deltas, got...)
	}
	var starts int
	var args strings.Builder
	for _, dl := range deltas {
		switch dl.Kind {
		case DeltaToolStart:
			starts++
		case DeltaToolArgs:
			args.WriteString(dl.ArgsJSON)
		default:
			t.Fatalf("意外增量: %+v", dl)
		}
	}
	if starts != 1 {
		t.Fatalf("工具开始增量数量 = %d，期望 1", starts)
	}
	if args.String() != `{"city":"北京"}` {
		t.Fatalf("参数增量 = %q，期望只发一遍", args.String())
	}
}

// TestEncodeResponsesRequestDropsBuiltinTools 确认 anthropic 的服务端内置工具不会变成 function。
func TestEncodeResponsesRequestDropsBuiltinTools(t *testing.T) {
	raw, err := EncodeResponsesRequest(&Request{
		Model: "m1",
		Tools: []Tool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "get_weather", Schema: json.RawMessage(`{"type":"object"}`)},
			{Type: "custom", Name: "bash", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}, "up")
	if err != nil {
		t.Fatalf("EncodeResponsesRequest: %v", err)
	}
	var got struct {
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v", err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != "get_weather" || got.Tools[1].Name != "bash" {
		t.Fatalf("tools = %+v，期望内置工具被丢弃", got.Tools)
	}
	for _, tool := range got.Tools {
		if tool.Type != "function" {
			t.Fatalf("tools 里的 type = %q，期望 function", tool.Type)
		}
	}
}

// TestResponsesStreamEncoderTruncatedToolArgs 确认断流时错误文案说明工具参数不完整。
func TestResponsesStreamEncoderTruncatedToolArgs(t *testing.T) {
	e := newResponsesStreamEncoder("gpt-5")
	e.Emit(Delta{Kind: DeltaToolStart, Index: 1, ToolID: "call_1", ToolName: "get_weather"})
	e.Emit(Delta{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{"city":"北`})

	events := e.Emit(Delta{Kind: DeltaError, Text: "上游流式响应中断: 收尾帧之前连接就结束了"})
	if len(events) != 2 || events[0].Event != "error" || events[1].Event != "response.failed" {
		t.Fatalf("断流事件 = %+v", events)
	}
	var errData struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(events[0].Data, &errData); err != nil {
		t.Fatalf("error 事件不是合法 JSON: %v", err)
	}
	if !strings.Contains(errData.Message, "工具调用参数在上游中断") {
		t.Fatalf("error 文案 = %q，期望说明工具参数不完整", errData.Message)
	}
	var failed struct {
		Response struct {
			Status string `json:"status"`
			Output []any  `json:"output"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(events[1].Data, &failed); err != nil {
		t.Fatalf("response.failed 不是合法 JSON: %v", err)
	}
	if failed.Response.Status != "failed" || len(failed.Response.Output) != 0 {
		t.Fatalf("response.failed = %+v，期望失败且不回填残缺的 output", failed.Response)
	}
	if !strings.Contains(failed.Response.Error.Message, "工具调用参数在上游中断") {
		t.Fatalf("response.failed 的 message = %q", failed.Response.Error.Message)
	}
}

// TestResponsesStreamDecodeItemDoneWithoutOutputIndex 覆盖上游不给 output_index 的形状：
// added 与 done 都靠自增序号，done 不能把同一个 item 再发一遍。
func TestResponsesStreamDecodeItemDoneWithoutOutputIndex(t *testing.T) {
	d := &responsesStreamDecoder{}
	frames := []string{
		`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":\"北京\"}"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":"{\"city\":\"北京\"}","status":"completed"}}`,
	}
	var deltas []Delta
	for _, f := range frames {
		got, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode(%s): %v", f, err)
		}
		deltas = append(deltas, got...)
	}
	var starts int
	var args strings.Builder
	for _, dl := range deltas {
		switch dl.Kind {
		case DeltaToolStart:
			starts++
			if dl.Index != 0 {
				t.Fatalf("工具开始增量的序号 = %d，期望 0", dl.Index)
			}
		case DeltaToolArgs:
			args.WriteString(dl.ArgsJSON)
		}
	}
	if starts != 1 {
		t.Fatalf("工具开始增量数量 = %d，期望 1（done 不该重复发）", starts)
	}
	if args.String() != `{"city":"北京"}` {
		t.Fatalf("参数增量 = %q", args.String())
	}
}

// responsesCaptureLog 接管标准库 logger，返回一个取回已记日志文案的函数。
func responsesCaptureLog(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, string(p))
		return len(p), nil
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// writerFunc 让测试把日志收进内存。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestResponsesStreamEncoderErrorKeepsTextOutOfOutput 确认断流时不把半截正文写进 output：
// 正文已经按增量下发过，但 response.failed 的 output 仍是空数组（DESIGN 5.4）。
func TestResponsesStreamEncoderErrorKeepsTextOutOfOutput(t *testing.T) {
	e := newResponsesStreamEncoder("gpt-5")
	e.Emit(Delta{Kind: DeltaText, Text: "说到一半"})

	events := e.Emit(Delta{Kind: DeltaError, Text: "上游流式响应中断: 收尾帧之前连接就结束了"})
	if len(events) != 2 || events[1].Event != "response.failed" {
		t.Fatalf("断流事件 = %+v", events)
	}
	var failed struct {
		Response struct {
			Output []any `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(events[1].Data, &failed); err != nil {
		t.Fatalf("response.failed 不是合法 JSON: %v", err)
	}
	if len(failed.Response.Output) != 0 {
		t.Fatalf("response.failed 的 output = %+v，期望空数组：半截正文不进 output", failed.Response.Output)
	}
	// 断流之后不再补任何 item 的收尾事件。
	if finish := e.Finish(); len(finish) != 0 {
		t.Fatalf("response.failed 之后 Finish 不应再发事件: %+v", finish)
	}
}

// TestResponsesStreamEncoderTextCap 确认正文总长超过 1MB 后停止继续攒：收尾的 output 里
// 放弃攒不全的文本 item（客户端仍能从增量拼），其它 item 与 completed 的字段照旧给出。
func TestResponsesStreamEncoderTextCap(t *testing.T) {
	logs := responsesCaptureLog(t)
	e := newResponsesStreamEncoder("gpt-5")

	chunk := strings.Repeat("x", 64<<10)
	// 17 个 64KiB 的增量刚好越过 1MB 上限（第 17 个越线）。
	for i := 0; i < 17; i++ {
		e.Emit(Delta{Kind: DeltaText, Text: chunk})
	}
	// 超限之后的正文一律不攒。
	if e.textCapped != true || e.textTotal != 16*(64<<10) {
		t.Fatalf("超限后 textCapped=%v、已攒 %d 字节，期望停止在 1MiB 以内", e.textCapped, e.textTotal)
	}
	if seg := e.textSegments[0]; !seg.dropped || seg.text.Len() != 16*(64<<10) {
		t.Fatalf("文本 item = dropped %v、%d 字节，期望标记成 dropped 且停在 1MiB 以内", seg.dropped, seg.text.Len())
	}

	// 夹一次工具调用，确认超限只影响文本 item，不影响别的 item 与 completed 的其它字段。
	e.Emit(Delta{Kind: DeltaToolStart, Index: 0, ToolID: "call_1", ToolName: "weather"})
	e.Emit(Delta{Kind: DeltaToolArgs, Index: 0, ArgsJSON: `{"city":"北京"}`})
	e.Emit(Delta{Kind: DeltaText, Text: "超限之后的正文"})
	events := e.Emit(Delta{Kind: DeltaStop, StopReason: StopEndTurn, Usage: &Usage{In: 3, Out: 4}})

	var names []string
	for _, ev := range events {
		names = append(names, ev.Event)
	}
	// 收尾只剩工具 item 的 arguments.done 与 output_item.done，文本 item 不给收尾。
	want := []string{"response.function_call_arguments.done", "response.output_item.done", "response.completed"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("收尾事件 = %v，期望 %v", names, want)
	}

	var completed struct {
		Response struct {
			Status string `json:"status"`
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(events[len(events)-1].Data, &completed); err != nil {
		t.Fatalf("response.completed 不是合法 JSON: %v", err)
	}
	if completed.Response.Status != "completed" || completed.Response.Usage.InputTokens != 3 ||
		completed.Response.Usage.OutputTokens != 4 {
		t.Fatalf("response.completed = %+v，期望状态与用量照旧给出", completed.Response)
	}
	if len(completed.Response.Output) != 1 || completed.Response.Output[0].Type != "function_call" {
		t.Fatalf("response.completed 的 output = %+v，期望只剩 function_call", completed.Response.Output)
	}

	lines := logs()
	if len(lines) != 1 {
		t.Fatalf("超限日志行数 = %d，期望 1 行：%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "超过") || !strings.Contains(lines[0], "上限") ||
		!strings.Contains(lines[0], "增量") {
		t.Fatalf("超限日志 = %q，期望写明超过上限、客户端仍可从增量拼出全文", lines[0])
	}
}
