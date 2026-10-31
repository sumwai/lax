package translate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testChatBody 是断言用的一次性结构，字段与 openai-chat 请求体对齐。
type testChatBody struct {
	Model             string          `json:"model"`
	MaxTokens         int             `json:"max_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stop              []string        `json:"stop"`
	Stream            bool            `json:"stream"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	StreamOptions     *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

// testChatChunk 是流式帧的断言结构。FinishReason 用 RawMessage 以区分 null 与缺省。
type testChatChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		FinishReason json.RawMessage `json:"finish_reason"`
		Delta        struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *ChatUsage `json:"usage"`
}

func testChatBodyOf(t *testing.T, raw []byte) testChatBody {
	t.Helper()
	var body testChatBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("报文不是合法 JSON: %v\n%s", err, raw)
	}
	return body
}

func testChatChunkOf(t *testing.T, e SSEEvent) testChatChunk {
	t.Helper()
	if e.Event != "" {
		t.Fatalf("chat 事件不应带 event 名，实际 %q", e.Event)
	}
	var chunk testChatChunk
	if err := json.Unmarshal(e.Data, &chunk); err != nil {
		t.Fatalf("帧不是合法 JSON: %v\n%s", err, e.Data)
	}
	return chunk
}

func TestChatDecodeRequest(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	yes := true

	cases := []struct {
		name string
		raw  string
		want *Request
	}{
		{
			name: "system 与用户文本",
			raw:  `{"model":"m1","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}]}`,
			want: &Request{
				Model:    "m1",
				System:   []Block{{Kind: BlockText, Text: "你是助手"}},
				Messages: []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "你好"}}}},
			},
		},
		{
			name: "多段 system 与图片",
			raw: `{"model":"m1","messages":[
				{"role":"system","content":[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]},
				{"role":"user","content":[
					{"type":"text","text":"看图"},
					{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
					{"type":"image_url","image_url":"https://example.test/a.png"}
				]}
			]}`,
			want: &Request{
				Model:  "m1",
				System: []Block{{Kind: BlockText, Text: "第一段"}, {Kind: BlockText, Text: "第二段"}},
				Messages: []Message{{Role: RoleUser, Blocks: []Block{
					{Kind: BlockText, Text: "看图"},
					{Kind: BlockImage, Image: &Image{URL: "data:image/png;base64,AAAA"}},
					{Kind: BlockImage, Image: &Image{URL: "https://example.test/a.png"}},
				}}},
			},
		},
		{
			name: "assistant 工具调用",
			raw: `{"model":"m1","messages":[
				{"role":"user","content":"查天气"},
				{"role":"assistant","content":"稍等","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}
				]}
			]}`,
			want: &Request{
				Model: "m1",
				Messages: []Message{
					{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "查天气"}}},
					{Role: RoleAssistant, Blocks: []Block{
						{Kind: BlockText, Text: "稍等"},
						{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "call_1", Name: "weather", Input: json.RawMessage(`{"city":"北京"}`)}},
					}},
				},
			},
		},
		{
			name: "tool 消息",
			raw: `{"model":"m1","messages":[
				{"role":"tool","tool_call_id":"call_1","content":"晴，26 度"}
			]}`,
			want: &Request{
				Model: "m1",
				Messages: []Message{{Role: RoleTool, Blocks: []Block{
					{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Text: "晴，26 度"}},
				}}},
			},
		},
		{
			name: "tool_choice required 与 max_tokens",
			raw: `{"model":"m1","max_tokens":256,"tool_choice":"required",
				"messages":[{"role":"user","content":"hi"}]}`,
			want: &Request{
				Model:      "m1",
				MaxTokens:  256,
				ToolChoice: &ToolChoice{Mode: ToolChoiceRequired},
				Messages:   []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}},
			},
		},
		{
			name: "tool_choice 指定函数",
			raw: `{"model":"m1","tool_choice":{"type":"function","function":{"name":"weather"}},
				"messages":[{"role":"user","content":"hi"}]}`,
			want: &Request{
				Model:      "m1",
				ToolChoice: &ToolChoice{Mode: ToolChoiceNamed, Name: "weather"},
				Messages:   []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}},
			},
		},
		{
			name: "工具定义与采样参数",
			raw: `{"model":"m1","temperature":0.5,"top_p":0.9,"stop":["\n\n"],"stream":true,
				"parallel_tool_calls":true,
				"tools":[{"type":"function","function":{"name":"weather","description":"查天气",
					"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: &Request{
				Model:             "m1",
				Temperature:       f(0.5),
				TopP:              f(0.9),
				Stop:              []string{"\n\n"},
				Stream:            true,
				ParallelToolCalls: &yes,
				Tools: []Tool{{
					Name:        "weather",
					Description: "查天气",
					Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
				}},
				Messages: []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}},
			},
		},
		{
			name: "tool_choice none 与空参工具",
			raw: `{"model":"m1","tool_choice":"none",
				"tools":[{"type":"function","function":{"name":"ping"}}],
				"messages":[{"role":"user","content":"hi"}]}`,
			want: &Request{
				Model:      "m1",
				ToolChoice: &ToolChoice{Mode: ToolChoiceNone},
				Tools:      []Tool{{Name: "ping", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}},
				Messages:   []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeChatRequest([]byte(tc.raw))
			if err != nil {
				t.Fatalf("DecodeChatRequest: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("解出的 IR 与期望不符\n得到 %#v\n期望 %#v", got, tc.want)
			}
		})
	}
}

func TestChatDecodeRequestInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"请求体不是 JSON", `not-json`},
		{"工具参数不是 JSON", `{"model":"m1","messages":[{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{oops"}}]}]}`},
		{"未知角色", `{"model":"m1","messages":[{"role":"narrator","content":"hi"}]}`},
		{"未知 tool_choice", `{"model":"m1","tool_choice":"always","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeChatRequest([]byte(tc.raw)); err == nil {
				t.Fatalf("期望返回错误，实际成功")
			}
		})
	}
}

func TestChatEncodeRequest(t *testing.T) {
	temp := 0.4
	topP := 0.8
	no := false

	in := &Request{
		Model:       "client-model",
		System:      []Block{{Kind: BlockText, Text: "系统提示"}},
		MaxTokens:   512,
		Temperature: &temp,
		TopP:        &topP,
		Stop:        []string{"STOP"},
		Stream:      true,
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{
				{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Text: "结果"}},
				{Kind: BlockText, Text: "接着看这张图"},
				{Kind: BlockImage, Image: &Image{URL: "data:image/png;base64,AAAA"}},
			}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockText, Text: "好的"},
				{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "call_2", Name: "weather", Input: json.RawMessage(`{"city":"北京"}`)}},
			}},
			{Role: RoleTool, Blocks: []Block{
				{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_2", Text: "晴"}},
			}},
		},
		Tools:             []Tool{{Name: "weather", Description: "查天气", Schema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:        &ToolChoice{Mode: ToolChoiceNamed, Name: "weather"},
		ParallelToolCalls: &no,
	}

	raw, err := EncodeChatRequest(in, "upstream-model")
	if err != nil {
		t.Fatalf("EncodeChatRequest: %v", err)
	}
	body := testChatBodyOf(t, raw)

	if body.Model != "upstream-model" {
		t.Fatalf("model = %q，期望上游模型名", body.Model)
	}
	if body.MaxTokens != 512 {
		t.Fatalf("max_tokens = %d", body.MaxTokens)
	}
	if body.Temperature == nil || *body.Temperature != 0.4 {
		t.Fatalf("temperature = %v", body.Temperature)
	}
	if body.TopP == nil || *body.TopP != 0.8 {
		t.Fatalf("top_p = %v", body.TopP)
	}
	if len(body.Stop) != 1 || body.Stop[0] != "STOP" {
		t.Fatalf("stop = %v", body.Stop)
	}
	if !body.Stream {
		t.Fatalf("stream 应为 true")
	}
	if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options.include_usage 未开启: %+v", body.StreamOptions)
	}
	if body.ParallelToolCalls == nil || *body.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v，期望 false", body.ParallelToolCalls)
	}

	if len(body.Messages) != 5 {
		t.Fatalf("消息数 = %d，期望 5：%+v", len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "system" || string(body.Messages[0].Content) != `"系统提示"` {
		t.Fatalf("system 消息 = %+v", body.Messages[0])
	}
	// tool 消息必须排在同一条用户消息的文本之前。
	if body.Messages[1].Role != "tool" || body.Messages[1].ToolCallID != "call_1" || string(body.Messages[1].Content) != `"结果"` {
		t.Fatalf("tool 消息 = %+v", body.Messages[1])
	}
	if body.Messages[2].Role != "user" {
		t.Fatalf("用户消息角色 = %q", body.Messages[2].Role)
	}
	var parts []map[string]any
	if err := json.Unmarshal(body.Messages[2].Content, &parts); err != nil {
		t.Fatalf("图片消息 content 应为 part 数组: %v\n%s", err, body.Messages[2].Content)
	}
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[0]["text"] != "接着看这张图" {
		t.Fatalf("文本 part = %+v", parts)
	}
	img, _ := parts[1]["image_url"].(map[string]any)
	if parts[1]["type"] != "image_url" || img["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("图片 part = %+v", parts[1])
	}
	if body.Messages[3].Role != "assistant" || string(body.Messages[3].Content) != `"好的"` {
		t.Fatalf("assistant 消息 = %+v", body.Messages[3])
	}
	if len(body.Messages[3].ToolCalls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(body.Messages[3].ToolCalls))
	}
	call := body.Messages[3].ToolCalls[0]
	if call.ID != "call_2" || call.Type != "function" || call.Function.Name != "weather" || call.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("tool_call = %+v", call)
	}
	if body.Messages[4].Role != "tool" || body.Messages[4].ToolCallID != "call_2" || string(body.Messages[4].Content) != `"晴"` {
		t.Fatalf("工具结果消息 = %+v", body.Messages[4])
	}

	if len(body.Tools) != 1 || body.Tools[0].Type != "function" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if body.Tools[0].Function.Name != "weather" || body.Tools[0].Function.Description != "查天气" {
		t.Fatalf("工具定义 = %+v", body.Tools[0].Function)
	}
	if string(body.Tools[0].Function.Parameters) != `{"type":"object"}` {
		t.Fatalf("工具参数结构 = %s", body.Tools[0].Function.Parameters)
	}

	var choice struct {
		Type     string            `json:"type"`
		Function map[string]string `json:"function"`
	}
	if err := json.Unmarshal(body.ToolChoice, &choice); err != nil {
		t.Fatalf("tool_choice 不是对象: %v\n%s", err, body.ToolChoice)
	}
	if choice.Type != "function" || choice.Function["name"] != "weather" {
		t.Fatalf("tool_choice = %+v", choice)
	}
}

func TestChatEncodeRequestModeAndPlainText(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "auto", mode: ToolChoiceAuto, want: `"auto"`},
		{name: "required", mode: ToolChoiceRequired, want: `"required"`},
		{name: "none", mode: ToolChoiceNone, want: `"none"`},
		{name: "指定工具但缺 name", mode: ToolChoiceNamed, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &Request{
				Model:      "m1",
				ToolChoice: &ToolChoice{Mode: tc.mode},
				Messages:   []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}},
			}
			raw, err := EncodeChatRequest(in, "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望返回错误")
				}
				return
			}
			if err != nil {
				t.Fatalf("EncodeChatRequest: %v", err)
			}
			body := testChatBodyOf(t, raw)
			if body.Model != "m1" {
				t.Fatalf("model 未回落到请求里的模型名: %q", body.Model)
			}
			if strings.TrimSpace(string(body.ToolChoice)) != tc.want {
				t.Fatalf("tool_choice = %s，期望 %s", body.ToolChoice, tc.want)
			}
			if len(body.Messages) != 1 || string(body.Messages[0].Content) != `"hi"` {
				t.Fatalf("纯文本用户消息应编成字符串: %+v", body.Messages)
			}
			if body.StreamOptions != nil {
				t.Fatalf("非流式不应带 stream_options")
			}
		})
	}
}

func TestChatDecodeResponse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *Response
	}{
		{
			name: "文本与用量",
			raw: `{"id":"chatcmpl-1","model":"upstream","choices":[{"index":0,
				"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11,
					"prompt_tokens_details":{"cached_tokens":4}}}`,
			want: &Response{
				ID:         "chatcmpl-1",
				Model:      "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "你好"}},
				StopReason: StopEndTurn,
				Usage:      Usage{In: 9, Out: 2, CacheRead: 4, Total: 11},
			},
		},
		{
			// 直通路径的非流式响应体走解码器记账，思考 token 与总 token 也要从这四个字段里读出来：
			// 少读一个，入账时就判不出思考是不是独立的一项。
			name: "思考 token",
			raw: `{"id":"chatcmpl-9","choices":[{"index":0,
				"message":{"role":"assistant","content":"想完了"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":132,
					"completion_tokens_details":{"reasoning_tokens":124}}}`,
			want: &Response{
				ID:         "chatcmpl-9",
				Model:      "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "想完了"}},
				StopReason: StopEndTurn,
				Usage:      Usage{In: 7, Out: 1, Total: 132, Reasoning: 124},
			},
		},
		{
			name: "工具调用",
			raw: `{"id":"chatcmpl-2","choices":[{"index":0,
				"message":{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_9","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}
				]},"finish_reason":"tool_calls"}]}`,
			want: &Response{
				ID:    "chatcmpl-2",
				Model: "client-model",
				Blocks: []Block{{Kind: BlockToolUse, ToolUse: &ToolUse{
					ID: "call_9", Name: "weather", Input: json.RawMessage(`{"city":"北京"}`),
				}}},
				StopReason: StopToolUse,
			},
		},
		{
			name: "被截断",
			raw: `{"id":"chatcmpl-3","choices":[{"index":0,
				"message":{"role":"assistant","content":"半句"},"finish_reason":"length"}]}`,
			want: &Response{
				ID:         "chatcmpl-3",
				Model:      "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "半句"}},
				StopReason: StopMaxTokens,
			},
		},
		{
			name: "content 为 part 数组",
			raw: `{"id":"chatcmpl-4","choices":[{"index":0,
				"message":{"role":"assistant","content":[{"type":"text","text":"甲"},{"type":"text","text":"乙"}]},
				"finish_reason":"stop"}]}`,
			want: &Response{
				ID:         "chatcmpl-4",
				Model:      "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "甲\n乙"}},
				StopReason: StopEndTurn,
			},
		},
		{
			name: "内容过滤按结束处理",
			raw: `{"id":"chatcmpl-5","choices":[{"index":0,
				"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}]}`,
			want: &Response{
				ID:         "chatcmpl-5",
				Model:      "client-model",
				Blocks:     []Block{},
				StopReason: StopEndTurn,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeChatResponse([]byte(tc.raw), "client-model")
			if err != nil {
				t.Fatalf("DecodeChatResponse: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("响应 IR 与期望不符\n得到 %#v\n期望 %#v", got, tc.want)
			}
		})
	}
}

func TestChatDecodeResponseInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"响应体不是 JSON", `oops`},
		{"没有 choices", `{"id":"chatcmpl-1","choices":[]}`},
		{"工具参数不是 JSON", `{"id":"chatcmpl-1","choices":[{"index":0,
			"message":{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{oops"}}]},
			"finish_reason":"tool_calls"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeChatResponse([]byte(tc.raw), "m"); err == nil {
				t.Fatalf("期望返回错误，实际成功")
			}
		})
	}
}

func TestChatEncodeResponse(t *testing.T) {
	cases := []struct {
		name         string
		in           *Response
		wantFinish   string
		wantContent  string
		wantToolCall bool
		wantArgs     string
	}{
		{
			name: "文本与用量",
			in: &Response{
				ID: "msg_abc", Model: "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "你好"}},
				StopReason: StopEndTurn,
				Usage:      Usage{In: 9, Out: 2, CacheRead: 4},
			},
			wantFinish:  "stop",
			wantContent: "你好",
		},
		{
			name: "工具调用",
			in: &Response{
				ID: "chatcmpl-2", Model: "client-model",
				Blocks: []Block{
					{Kind: BlockText, Text: "稍等"},
					{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "call_9", Name: "weather", Input: json.RawMessage(`{"city":"北京"}`)}},
				},
				StopReason: StopToolUse,
			},
			wantFinish:   "tool_calls",
			wantContent:  "稍等",
			wantToolCall: true,
			wantArgs:     `{"city":"北京"}`,
		},
		{
			name: "被截断",
			in: &Response{
				ID: "chatcmpl-3", Model: "client-model",
				Blocks:     []Block{{Kind: BlockText, Text: "半句"}},
				StopReason: StopMaxTokens,
			},
			wantFinish:  "length",
			wantContent: "半句",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := EncodeChatResponse(tc.in)
			if err != nil {
				t.Fatalf("EncodeChatResponse: %v", err)
			}
			var out ChatResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("报文不是合法 JSON: %v\n%s", err, raw)
			}
			if out.Object != "chat.completion" {
				t.Fatalf("object = %q", out.Object)
			}
			if out.Model != "client-model" {
				t.Fatalf("model = %q", out.Model)
			}
			if out.ID == "" {
				t.Fatalf("id 不能为空")
			}
			if len(out.Choices) != 1 {
				t.Fatalf("choices 数 = %d", len(out.Choices))
			}
			choice := out.Choices[0]
			if choice.Message.Role != "assistant" {
				t.Fatalf("role = %q", choice.Message.Role)
			}
			if got, _ := choice.Message.Content.(string); got != tc.wantContent {
				t.Fatalf("content = %v，期望 %q", choice.Message.Content, tc.wantContent)
			}
			if choice.FinishReason != tc.wantFinish {
				t.Fatalf("finish_reason = %q，期望 %q", choice.FinishReason, tc.wantFinish)
			}
			if tc.wantToolCall {
				if len(choice.Message.ToolCalls) != 1 {
					t.Fatalf("tool_calls 数 = %d", len(choice.Message.ToolCalls))
				}
				call := choice.Message.ToolCalls[0]
				if call.ID != "call_9" || call.Type != "function" || call.Function.Name != "weather" {
					t.Fatalf("tool_call = %+v", call)
				}
				if call.Function.Arguments != tc.wantArgs {
					t.Fatalf("arguments = %q，期望 %q", call.Function.Arguments, tc.wantArgs)
				}
			}
			if out.Usage == nil {
				t.Fatalf("usage 缺失")
			}
			if out.Usage.PromptTokens != tc.in.Usage.In || out.Usage.CompletionTokens != tc.in.Usage.Out {
				t.Fatalf("usage = %+v", out.Usage)
			}
			if out.Usage.TotalTokens != tc.in.Usage.In+tc.in.Usage.Out {
				t.Fatalf("total_tokens = %d", out.Usage.TotalTokens)
			}
			if tc.in.Usage.CacheRead > 0 {
				if out.Usage.PromptTokensDetails == nil || out.Usage.PromptTokensDetails.CachedTokens != tc.in.Usage.CacheRead {
					t.Fatalf("prompt_tokens_details = %+v", out.Usage.PromptTokensDetails)
				}
			}
		})
	}
}

func TestChatEncodeResponseInvalidInput(t *testing.T) {
	in := &Response{Blocks: []Block{{Kind: BlockToolUse, ToolUse: &ToolUse{Name: "f", Input: json.RawMessage(`{oops`)}}}}
	if _, err := EncodeChatResponse(in); err == nil {
		t.Fatalf("工具输入不是 JSON 时期望返回错误")
	}
}

func TestChatProtocolWiring(t *testing.T) {
	// 经由 ir.go 的分派入口，确认这三处接线指向本文件的实现。
	if _, err := DecodeRequest(ProtocolChat, []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if _, err := EncodeRequest(ProtocolChat, &Request{Model: "m1"}, "m1"); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	dec, err := NewStreamDecoder(ProtocolChat)
	if err != nil {
		t.Fatalf("NewStreamDecoder: %v", err)
	}
	if _, ok := dec.(*chatStreamDecoder); !ok {
		t.Fatalf("NewStreamDecoder 返回的不是 chatStreamDecoder: %T", dec)
	}
	enc, err := NewStreamEncoder(ProtocolChat, "m1")
	if err != nil {
		t.Fatalf("NewStreamEncoder: %v", err)
	}
	if _, ok := enc.(*chatStreamEncoder); !ok {
		t.Fatalf("NewStreamEncoder 返回的不是 chatStreamEncoder: %T", enc)
	}
	if _, err := NewUsageScanner(ProtocolChat); err != nil {
		t.Fatalf("NewUsageScanner: %v", err)
	}
}

func TestChatStreamDecoderText(t *testing.T) {
	frames := []string{
		`{"id":"chatcmpl-1","model":"upstream","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"你"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"好"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
	}

	d := &chatStreamDecoder{}
	var got []Delta
	for _, f := range frames {
		deltas, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got = append(got, deltas...)
	}
	want := []Delta{
		{Kind: DeltaText, Text: "你"},
		{Kind: DeltaText, Text: "好"},
		{Kind: DeltaStop, StopReason: StopEndTurn},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("增量与期望不符\n得到 %#v\n期望 %#v", got, want)
	}

	closed := d.Close()
	wantClose := []Delta{{Kind: DeltaStop, Usage: &Usage{In: 9, Out: 2}}}
	if !reflect.DeepEqual(closed, wantClose) {
		t.Fatalf("Close 与期望不符\n得到 %#v\n期望 %#v", closed, wantClose)
	}
	if again := d.Close(); again != nil {
		t.Fatalf("重复 Close 应返回空，实际 %#v", again)
	}
}

func TestChatStreamDecoderToolArgs(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"clock","arguments":"{}"}}]}}]}`,
	}

	d := &chatStreamDecoder{}
	var got []Delta
	for _, f := range frames {
		deltas, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got = append(got, deltas...)
	}
	want := []Delta{
		{Kind: DeltaToolStart, Index: 0, ToolID: "call_1", ToolName: "weather"},
		{Kind: DeltaToolArgs, Index: 0, ArgsJSON: `{"city":`},
		{Kind: DeltaToolArgs, Index: 0, ArgsJSON: `"北京"}`},
		{Kind: DeltaToolStart, Index: 1, ToolID: "call_2", ToolName: "clock"},
		{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("增量与期望不符\n得到 %#v\n期望 %#v", got, want)
	}
}

func TestChatStreamDecoderUsageBeforeFinish(t *testing.T) {
	d := &chatStreamDecoder{}
	var got []Delta
	for _, f := range []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
	} {
		deltas, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got = append(got, deltas...)
	}
	want := []Delta{
		{Kind: DeltaText, Text: "hi"},
		{Kind: DeltaStop, StopReason: StopMaxTokens, Usage: &Usage{In: 3, Out: 1, CacheRead: 2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("增量与期望不符\n得到 %#v\n期望 %#v", got, want)
	}
	if closed := d.Close(); closed != nil {
		t.Fatalf("用量已随 finish 发出，Close 应为空，实际 %#v", closed)
	}
}

func TestChatStreamDecoderCloseFillsStop(t *testing.T) {
	d := &chatStreamDecoder{}
	if _, err := d.Decode([]byte(`{"choices":[{"index":0,"delta":{"content":"半句"}}]}`)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []Delta{{Kind: DeltaStop, StopReason: StopEndTurn}}
	if got := d.Close(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Close 与期望不符\n得到 %#v\n期望 %#v", got, want)
	}
}

func TestChatStreamDecoderTolerant(t *testing.T) {
	d := &chatStreamDecoder{}
	for _, f := range []string{``, `   `, `[DONE]`} {
		deltas, err := d.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode(%q): %v", f, err)
		}
		if len(deltas) != 0 {
			t.Fatalf("Decode(%q) 应返回空，实际 %#v", f, deltas)
		}
	}

	deltas, err := d.Decode([]byte(`{"error":{"type":"invalid_request_error","message":"模型名不对"}}`))
	if err != nil {
		t.Fatalf("Decode 错误帧: %v", err)
	}
	want := []Delta{{Kind: DeltaError, Text: "模型名不对"}}
	if !reflect.DeepEqual(deltas, want) {
		t.Fatalf("错误帧应与期望不符\n得到 %#v\n期望 %#v", deltas, want)
	}

	if _, err := d.Decode([]byte(`{oops`)); err == nil {
		t.Fatalf("非 JSON 帧应返回错误")
	}
}

func TestChatStreamEncoderFrames(t *testing.T) {
	e := newChatStreamEncoder("client-model")

	start := e.Start()
	if len(start) != 1 {
		t.Fatalf("Start 帧数 = %d", len(start))
	}
	first := testChatChunkOf(t, start[0])
	if first.Object != "chat.completion.chunk" {
		t.Fatalf("object = %q", first.Object)
	}
	if first.Model != "client-model" || first.ID == "" {
		t.Fatalf("id/model = %q/%q", first.ID, first.Model)
	}
	if len(first.Choices) != 1 {
		t.Fatalf("choices 数 = %d", len(first.Choices))
	}
	choice := first.Choices[0]
	if choice.Delta.Role != "assistant" {
		t.Fatalf("role = %q", choice.Delta.Role)
	}
	if choice.Delta.Content == nil || *choice.Delta.Content != "" {
		t.Fatalf("首帧 content 应为空字符串，实际 %v", choice.Delta.Content)
	}
	if string(choice.FinishReason) != "null" {
		t.Fatalf("首帧 finish_reason 应为 null，实际 %s", choice.FinishReason)
	}

	text := e.Emit(Delta{Kind: DeltaText, Text: "你"})
	if len(text) != 1 {
		t.Fatalf("文本帧数 = %d", len(text))
	}
	textChunk := testChatChunkOf(t, text[0])
	if got := textChunk.Choices[0].Delta.Content; got == nil || *got != "你" {
		t.Fatalf("文本增量 = %v", got)
	}

	startTool := e.Emit(Delta{Kind: DeltaToolStart, Index: 0, ToolID: "call_1", ToolName: "weather"})
	toolChunk := testChatChunkOf(t, startTool[0])
	calls := toolChunk.Choices[0].Delta.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(calls))
	}
	if calls[0].Index == nil || *calls[0].Index != 0 || calls[0].ID != "call_1" || calls[0].Type != "function" {
		t.Fatalf("tool_calls 首帧 = %+v", calls[0])
	}
	if calls[0].Function.Name != "weather" || calls[0].Function.Arguments != "" {
		t.Fatalf("tool_calls 首帧 function = %+v", calls[0].Function)
	}

	argsEvent := e.Emit(Delta{Kind: DeltaToolArgs, Index: 0, ArgsJSON: `{"city":`})
	argsChunk := testChatChunkOf(t, argsEvent[0])
	argsCalls := argsChunk.Choices[0].Delta.ToolCalls
	if len(argsCalls) != 1 || argsCalls[0].Index == nil || *argsCalls[0].Index != 0 {
		t.Fatalf("参数帧 tool_calls = %+v", argsCalls)
	}
	if argsCalls[0].Function.Arguments != `{"city":` {
		t.Fatalf("参数增量 = %q", argsCalls[0].Function.Arguments)
	}
	if argsCalls[0].ID != "" {
		t.Fatalf("参数帧不应重复带 id，实际 %q", argsCalls[0].ID)
	}

	stop := e.Emit(Delta{Kind: DeltaStop, StopReason: StopToolUse, Usage: &Usage{In: 7, Out: 3}})
	if len(stop) != 2 {
		t.Fatalf("带用量的结束应发两帧，实际 %d", len(stop))
	}
	stopChunk := testChatChunkOf(t, stop[0])
	if string(stopChunk.Choices[0].FinishReason) != `"tool_calls"` {
		t.Fatalf("finish_reason = %s", stopChunk.Choices[0].FinishReason)
	}
	usageChunk := testChatChunkOf(t, stop[1])
	if len(usageChunk.Choices) != 0 {
		t.Fatalf("用量帧的 choices 应为空，实际 %+v", usageChunk.Choices)
	}
	if usageChunk.Usage == nil {
		t.Fatalf("用量帧缺少 usage")
	}
	if usageChunk.Usage.PromptTokens != 7 || usageChunk.Usage.CompletionTokens != 3 || usageChunk.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v", usageChunk.Usage)
	}

	fin := e.Finish()
	if len(fin) != 1 || string(fin[0].Data) != "[DONE]" || fin[0].Event != "" {
		t.Fatalf("结束帧 = %+v", fin)
	}
	if again := e.Finish(); again != nil {
		t.Fatalf("重复 Finish 应返回空，实际 %+v", again)
	}
}

func TestChatStreamEncoderUsageAfterStop(t *testing.T) {
	e := newChatStreamEncoder("m1")
	e.Start()

	stop := e.Emit(Delta{Kind: DeltaStop, StopReason: StopEndTurn})
	if len(stop) != 1 {
		t.Fatalf("无用量时结束只发一帧，实际 %d", len(stop))
	}

	late := e.Emit(Delta{Kind: DeltaStop, Usage: &Usage{In: 4, Out: 6}})
	if len(late) != 1 {
		t.Fatalf("补发的用量应为一帧，实际 %d", len(late))
	}
	chunk := testChatChunkOf(t, late[0])
	if chunk.Usage == nil || chunk.Usage.PromptTokens != 4 || chunk.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v", chunk.Usage)
	}

	fin := e.Finish()
	if len(fin) != 1 || string(fin[0].Data) != "[DONE]" {
		t.Fatalf("用量已发过，Finish 只补 [DONE]，实际 %+v", fin)
	}
}

func TestChatUsageScanner(t *testing.T) {
	s := &chatUsageScanner{}
	s.Feed([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))
	if u := s.Usage(); u != (Usage{}) {
		t.Fatalf("还没出现用量帧时应是零值，实际 %+v", u)
	}

	s.Feed([]byte(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,
		"prompt_tokens_details":{"cached_tokens":3}}}`))
	u := s.Usage()
	if u.In != 11 || u.Out != 7 || u.CacheRead != 3 || u.CacheWrite != 0 {
		t.Fatalf("用量 = %+v，期望 11/7/3/0", u)
	}
	if u.Total != 18 {
		t.Fatalf("总 token = %d，期望 18（判定思考是否独立成项要用它）", u.Total)
	}

	// 非 JSON 与 [DONE] 不影响已读到的值。
	s.Feed([]byte(`[DONE]`))
	s.Feed([]byte(`not-json`))
	s.Feed(nil)
	if u := s.Usage(); u.In != 11 || u.Out != 7 || u.CacheRead != 3 || u.CacheWrite != 0 {
		t.Fatalf("用量被非 JSON 帧改动：%+v", u)
	}
}

// TestChatUsageScannerReadsReasoning 钉住思考 token 的读法：chat 把它报在
// completion_tokens_details.reasoning_tokens 里，与 total_tokens 一起读出来交给入账判定。
func TestChatUsageScannerReadsReasoning(t *testing.T) {
	s := &chatUsageScanner{}
	s.Feed([]byte(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":132,
		"completion_tokens_details":{"reasoning_tokens":124}}}`))
	u := s.Usage()
	if u.Reasoning != 124 {
		t.Fatalf("思考 token = %d，期望 124", u.Reasoning)
	}
	if u.Total != 132 || u.In != 7 || u.Out != 1 {
		t.Fatalf("用量 = %+v，期望 输入 7、输出 1、总 132", u)
	}
}

func TestChatUsageScannerEmpty(t *testing.T) {
	s := &chatUsageScanner{}
	if u := s.Usage(); u != (Usage{}) {
		t.Fatalf("空扫描器应是零值，实际 %+v", u)
	}
}

// TestEncodeChatRequestDropsThinking 确认请求方向不回填思考内容：chat 的
// reasoning_content 是上游自己产生的字段，客户端把它回填给推理模型会被以 400 拒掉
// 整条请求，响应方向照旧搬运。
func TestEncodeChatRequestDropsThinking(t *testing.T) {
	req := &Request{
		Model: "m1",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "问题"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockThinking, Thinking: &Thinking{Text: "想一下", Signature: "sig-abc", Redacted: "opaque"}},
				{Kind: BlockText, Text: "答"},
			}},
		},
	}
	raw, err := EncodeChatRequest(req, "")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	out := string(raw)
	for _, bad := range []string{`"reasoning_content"`, `"reasoning":`, "想一下", "sig-abc", "opaque"} {
		if strings.Contains(out, bad) {
			t.Fatalf("请求报文里不该出现 %q：%s", bad, out)
		}
	}

	// 响应方向仍然把思考正文写进 reasoning_content，两条方向不一样。
	resp, err := EncodeChatResponse(&Response{
		Model:  "m1",
		Blocks: []Block{{Kind: BlockThinking, Thinking: &Thinking{Text: "想一下", Signature: "sig-abc"}}},
	})
	if err != nil {
		t.Fatalf("编码响应: %v", err)
	}
	if !strings.Contains(string(resp), `"reasoning_content":"想一下"`) {
		t.Fatalf("响应报文里应有 reasoning_content：%s", resp)
	}
}

func TestDecodeChatReasoningOnlyMessage(t *testing.T) {
	// 解码时只有思考内容、没有正文的 assistant 消息仍保留，否则响应方向的搬运会丢掉它。
	raw := `{"model":"m1","messages":[
	  {"role":"user","content":"问题"},
	  {"role":"assistant","reasoning_content":"想一下"}
	]}`
	req, err := DecodeChatRequest([]byte(raw))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[1].Role != RoleAssistant {
		t.Fatalf("消息 = %+v", req.Messages)
	}
	b := req.Messages[1].Blocks
	if len(b) != 1 || b[0].Kind != BlockThinking || b[0].Thinking.Text != "想一下" {
		t.Fatalf("块 = %+v", b)
	}

	// 再编回请求：思考内容不回填，这条 assistant 消息什么都不剩，整条不写。
	out, err := EncodeChatRequest(req, "")
	if err != nil {
		t.Fatalf("编码请求: %v", err)
	}
	if strings.Contains(string(out), "reasoning_content") {
		t.Fatalf("请求报文里不该有 reasoning_content：%s", out)
	}
	if strings.Contains(string(out), `"role":"assistant"`) {
		t.Fatalf("只剩思考内容的 assistant 消息不该写出去：%s", out)
	}
}

// TestChatStreamEncoderDenseToolIndex 确认工具序号按出现顺序从 0 连续编号：
// 上游 anthropic 有文本块时首个工具是 1，并行工具是 1、2，照搬块号会让按稠密下标
// 建数组的客户端撞空位。
func TestChatStreamEncoderDenseToolIndex(t *testing.T) {
	e := newChatStreamEncoder("m1")
	e.Start()

	// 上游块号：文本 0、工具 1、工具 2。
	first := e.Emit(Delta{Kind: DeltaToolStart, Index: 1, ToolID: "toolu_1", ToolName: "weather"})
	got := e.indexesOf(t, first)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("首个工具的下标 = %v，期望 0", got)
	}

	second := e.Emit(Delta{Kind: DeltaToolStart, Index: 2, ToolID: "toolu_2", ToolName: "clock"})
	got = e.indexesOf(t, second)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("第二个工具的下标 = %v，期望 1", got)
	}

	// 同一个上游块号的后续参数片段仍落在同一个序号上。
	args := e.Emit(Delta{Kind: DeltaToolArgs, Index: 2, ArgsJSON: `{"zone":"UTC"}`})
	got = e.indexesOf(t, args)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("参数片段的下标 = %v，期望 1", got)
	}
}

// indexesOf 取事件里 tool_calls[].index。
func (e *chatStreamEncoder) indexesOf(t *testing.T, events []SSEEvent) []int {
	t.Helper()
	var idxs []int
	for _, ev := range events {
		for _, c := range testChatChunkOf(t, ev).Choices {
			for _, call := range c.Delta.ToolCalls {
				if call.Index == nil {
					t.Fatalf("tool_calls 缺少 index")
				}
				idxs = append(idxs, *call.Index)
			}
		}
	}
	return idxs
}

// TestEncodeChatRequestDropsBuiltinTools 确认 anthropic 服务端内置工具不会变成 function。
func TestEncodeChatRequestDropsBuiltinTools(t *testing.T) {
	raw, err := EncodeChatRequest(&Request{
		Model:    "m1",
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}},
		Tools: []Tool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "get_weather", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}, "")
	if err != nil {
		t.Fatalf("EncodeChatRequest: %v", err)
	}
	body := testChatBodyOf(t, raw)
	if len(body.Tools) != 1 || body.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v，期望只剩客户端函数", body.Tools)
	}
	if body.Tools[0].Type != "function" {
		t.Fatalf("tools[0].type = %q", body.Tools[0].Type)
	}
}

// TestChatStreamEncoderTruncatedToolArgs 确认断流时错误帧说明工具参数不完整。
func TestChatStreamEncoderTruncatedToolArgs(t *testing.T) {
	e := newChatStreamEncoder("m1")
	e.Start()
	e.Emit(Delta{Kind: DeltaToolStart, Index: 1, ToolID: "toolu_1", ToolName: "weather"})
	e.Emit(Delta{Kind: DeltaToolArgs, Index: 1, ArgsJSON: `{"city":"北`})

	events := e.Emit(Delta{Kind: DeltaError, Text: "上游流式响应中断: 收尾帧之前连接就结束了"})
	if len(events) != 1 {
		t.Fatalf("错误帧数 = %d", len(events))
	}
	var frame struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(events[0].Data, &frame); err != nil {
		t.Fatalf("错误帧不是合法 JSON: %v", err)
	}
	if !strings.Contains(frame.Error.Message, "工具调用参数在上游中断") {
		t.Fatalf("错误帧文案 = %q，期望说明工具参数不完整", frame.Error.Message)
	}

	// 收尾之后再来的错误不带这条说明：那时工具块已经收了。
	other := newChatStreamEncoder("m1")
	other.Start()
	other.Emit(Delta{Kind: DeltaToolStart, Index: 0, ToolID: "t1", ToolName: "f"})
	other.Emit(Delta{Kind: DeltaStop, StopReason: StopToolUse})
	events = other.Emit(Delta{Kind: DeltaError, Text: "上游报错"})
	if strings.Contains(string(events[0].Data), "工具调用参数在上游中断") {
		t.Fatalf("工具块已收尾，不该带参数不完整的说明: %s", events[0].Data)
	}
}
