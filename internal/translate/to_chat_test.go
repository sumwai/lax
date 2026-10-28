package translate

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToChatMapsSystemAndText(t *testing.T) {
	in := &AnthropicRequest{
		Model:     "claude-x",
		MaxTokens: 128,
		System:    json.RawMessage(`"你是助手"`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: Content{Text: strPtr("你好")}},
		},
		StopSequences: []string{"STOP"},
	}

	out, err := ToChat(in, "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	if out.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q，期望上游模型名", out.Model)
	}
	if out.MaxTokens != 128 {
		t.Fatalf("max_tokens = %d", out.MaxTokens)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("消息数 = %d，期望 2（system + user）", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "你是助手" {
		t.Fatalf("system 消息 = %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "你好" {
		t.Fatalf("user 消息 = %+v", out.Messages[1])
	}
	if len(out.Stop) != 1 || out.Stop[0] != "STOP" {
		t.Fatalf("stop = %v", out.Stop)
	}
}

func TestToChatMapsToolUseAndToolResult(t *testing.T) {
	raw := `{
	  "model": "claude-x",
	  "max_tokens": 256,
	  "messages": [
	    {"role": "assistant", "content": [
	      {"type": "text", "text": "查一下"},
	      {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "上海"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴"},
	      {"type": "text", "text": "谢谢"}
	    ]}
	  ],
	  "tools": [{"name": "get_weather", "description": "天气", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
	  "tool_choice": {"type": "any", "disable_parallel_tool_use": true}
	}`

	var in AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("解析请求: %v", err)
	}

	out, err := ToChat(&in, "m")
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3（assistant / tool / user）: %+v", len(out.Messages), out.Messages)
	}

	assistant := out.Messages[0]
	if assistant.Role != "assistant" || assistant.Content != "查一下" {
		t.Fatalf("assistant = %+v", assistant)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.ID != "toolu_1" || call.Function.Name != "get_weather" {
		t.Fatalf("tool_call = %+v", call)
	}
	if !jsonEqual(t, call.Function.Arguments, `{"city":"上海"}`) {
		t.Fatalf("arguments = %s，期望与原始 input 等价", call.Function.Arguments)
	}

	tool := out.Messages[1]
	if tool.Role != "tool" || tool.ToolCallID != "toolu_1" || tool.Content != "晴" {
		t.Fatalf("tool 消息 = %+v", tool)
	}

	user := out.Messages[2]
	if user.Role != "user" || user.Content != "谢谢" {
		t.Fatalf("user 消息 = %+v", user)
	}

	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	if out.ToolChoice != "required" {
		t.Fatalf("tool_choice = %v，期望 required", out.ToolChoice)
	}
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls 应为 false")
	}
}

func TestToChatMapsImageBlock(t *testing.T) {
	raw := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[
	  {"type":"text","text":"看图"},
	  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}
	]}]}`
	var in AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("解析请求: %v", err)
	}

	out, err := ToChat(&in, "m")
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("消息数 = %d", len(out.Messages))
	}
	parts, ok := out.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("content 类型 = %T，期望数组", out.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("part 数 = %d，期望 2", len(parts))
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "看图" {
		t.Fatalf("第一块 = %+v", first)
	}
	second, _ := parts[1].(map[string]any)
	img, _ := second["image_url"].(map[string]string)
	if img["url"] != "data:image/png;base64,QUJD" {
		t.Fatalf("图片地址 = %q", img["url"])
	}
}

func TestFromChatMapsTextAndUsage(t *testing.T) {
	in := &ChatResponse{
		ID: "chatcmpl-9",
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: "答案"},
			FinishReason: "stop",
		}},
		Usage: &ChatUsage{
			PromptTokens:        11,
			CompletionTokens:    7,
			PromptTokensDetails: &PromptTokenDetail{CachedTokens: 3},
		},
	}

	out, err := FromChat(in, "claude-x")
	if err != nil {
		t.Fatalf("FromChat: %v", err)
	}
	if out.Model != "claude-x" {
		t.Fatalf("model = %q，应写回客户端请求的模型名", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "答案" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 7 || out.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestFromChatMapsToolCalls(t *testing.T) {
	in := &ChatResponse{
		ID: "chatcmpl-9",
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ChatToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: ChatFunctionCall{Name: "get_weather", Arguments: `{"city":"上海"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	out, err := FromChat(in, "m")
	if err != nil {
		t.Fatalf("FromChat: %v", err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Content[0].ID != "call_1" || out.Content[0].Name != "get_weather" {
		t.Fatalf("tool_use = %+v", out.Content[0])
	}
	if string(out.Content[0].Input) != `{"city":"上海"}` {
		t.Fatalf("input = %s", out.Content[0].Input)
	}
	if out.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", out.StopReason)
	}
}

func TestFromChatRejectsInvalidToolArguments(t *testing.T) {
	in := &ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMessage{
				ToolCalls: []ChatToolCall{{Function: ChatFunctionCall{Name: "x", Arguments: "{不是 JSON"}}},
			},
		}},
	}
	if _, err := FromChat(in, "m"); err == nil {
		t.Fatal("参数不是合法 JSON 时应返回错误，而不是猜一个值")
	}
}

func strPtr(s string) *string { return &s }

// jsonEqual 比较两段 JSON 的结构，忽略空白与键顺序。
func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("第一段不是合法 JSON: %s", a)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("第二段不是合法 JSON: %s", b)
	}
	return reflect.DeepEqual(av, bv)
}
