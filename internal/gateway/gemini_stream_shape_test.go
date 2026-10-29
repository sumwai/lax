package gateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// 这个文件钉住 gemini 流式两个方向的帧序列。gemini 的流没有事件名，一帧就是一个
// GenerateContentResponse，所以断言拿「帧签名」列表逐位比较：顺序不同、少一帧、多一帧都算错。
// 上游一律用假服务，帧里带工具调用与真实用量。

// geminiStreamWithTool 是假 gemini 上游的流式响应：两帧正文、一帧 functionCall、
// 一帧 finishReason 加 usageMetadata。functionCall 的参数一次给全（gemini 不是增量的）。
const geminiStreamWithTool = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"北京"}}}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[]},"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}

`

// chatStreamWithTool 是假 chat 上游的流式响应：正文两帧，工具调用分两段参数（chat 是增量的），
// 收尾帧给 finish_reason=tool_calls，用量单独一帧。
const chatStreamWithTool = `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"po"}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ng"}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}

data: [DONE]

`

// anthropicStreamWithTool 是假 anthropic 上游的流式响应：正文两段之后开一个 tool_use 块，
// 参数分两段给出，最后 message_delta 带 stop_reason=tool_use 与用量。
const anthropicStreamWithTool = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":3,"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// chatStreamSignatures 把 chat 客户端的流翻成签名列表：每帧只留正文、工具片段、结束原因与用量。
func chatStreamSignatures(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, f := range sseFrames(t, body) {
		if f.data == "[DONE]" {
			out = append(out, "[DONE]")
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
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
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(f.data), &chunk); err != nil {
			t.Fatalf("chat 客户端的帧不是合法 JSON: %v，原文 %s", err, f.data)
		}
		switch {
		case chunk.Usage != nil:
			out = append(out, fmt.Sprintf("usage=%d/%d choices=%d",
				chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, len(chunk.Choices)))
		case len(chunk.Choices) == 0:
			out = append(out, "空 choices")
		default:
			c := chunk.Choices[0]
			if c.FinishReason != nil {
				out = append(out, "finish="+*c.FinishReason)
				continue
			}
			if c.Delta.Content != nil {
				out = append(out, fmt.Sprintf("role=%q content=%q", c.Delta.Role, *c.Delta.Content))
				continue
			}
			for _, tc := range c.Delta.ToolCalls {
				idx := -1
				if tc.Index != nil {
					idx = *tc.Index
				}
				if tc.ID != "" || tc.Function.Name != "" {
					out = append(out, fmt.Sprintf("tool[%d] id=%s type=%s name=%s args=%q",
						idx, tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments))
					continue
				}
				out = append(out, fmt.Sprintf("args[%d]=%q", idx, tc.Function.Arguments))
			}
		}
	}
	return out
}

// anthropicStreamSignatures 把 anthropic 客户端的流翻成签名列表：事件名加该事件里有意义的那几个值。
func anthropicStreamSignatures(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, f := range sseFrames(t, body) {
		var env struct {
			Index   *int `json:"index"`
			Message *struct {
				Model string `json:"model"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(f.data), &env); err != nil {
			t.Fatalf("anthropic 客户端的帧不是合法 JSON: %v，原文 %s", err, f.data)
		}
		idx := -1
		if env.Index != nil {
			idx = *env.Index
		}
		switch f.event {
		case "message_start":
			model := ""
			if env.Message != nil {
				model = env.Message.Model
			}
			out = append(out, "message_start model="+model)
		case "content_block_start":
			out = append(out, fmt.Sprintf("block_start[%d] %s id=%s name=%s text=%q",
				idx, env.ContentBlock.Type, env.ContentBlock.ID, env.ContentBlock.Name, env.ContentBlock.Text))
		case "content_block_delta":
			value := env.Delta.Text
			if env.Delta.Type == "input_json_delta" {
				value = env.Delta.PartialJSON
			}
			out = append(out, fmt.Sprintf("delta[%d] %s %q", idx, env.Delta.Type, value))
		case "content_block_stop":
			out = append(out, fmt.Sprintf("block_stop[%d]", idx))
		case "message_delta":
			out = append(out, fmt.Sprintf("message_delta %s %d/%d",
				env.Delta.StopReason, env.Usage.InputTokens, env.Usage.OutputTokens))
		case "message_stop":
			out = append(out, "message_stop")
		default:
			out = append(out, "未知事件 "+f.event)
		}
	}
	return out
}

// geminiStreamSignatures 把 gemini 客户端的流翻成签名列表，同时断言帧里没有事件名。
func geminiStreamSignatures(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, f := range sseFrames(t, body) {
		if f.event != "" {
			t.Fatalf("gemini 的帧没有事件名，实际拿到 %q，原文 %s", f.event, f.data)
		}
		var frame struct {
			ModelVersion string `json:"modelVersion"`
			Candidates   []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall *struct {
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int64 `json:"promptTokenCount"`
				CandidatesTokenCount int64 `json:"candidatesTokenCount"`
				TotalTokenCount      int64 `json:"totalTokenCount"`
			} `json:"usageMetadata"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(f.data), &frame); err != nil {
			t.Fatalf("gemini 客户端的帧不是合法 JSON: %v，原文 %s", err, f.data)
		}
		if len(frame.Error) > 0 {
			// 错误帧的正文由用例自己断言（消息里带本次失败的原因），签名只记形状。
			out = append(out, "error")
			continue
		}
		if len(frame.Candidates) != 1 {
			out = append(out, fmt.Sprintf("候选数=%d", len(frame.Candidates)))
			continue
		}
		cand := frame.Candidates[0]
		if cand.FinishReason != "" {
			in, outTok, total := int64(0), int64(0), int64(0)
			if frame.UsageMetadata != nil {
				in = frame.UsageMetadata.PromptTokenCount
				outTok = frame.UsageMetadata.CandidatesTokenCount
				total = frame.UsageMetadata.TotalTokenCount
			}
			out = append(out, fmt.Sprintf("finish %s %d/%d total=%d", cand.FinishReason, in, outTok, total))
			continue
		}
		if len(cand.Content.Parts) == 0 {
			out = append(out, fmt.Sprintf("start %s role=%s", frame.ModelVersion, cand.Content.Role))
			continue
		}
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil {
				out = append(out, fmt.Sprintf("functionCall name=%s args=%s",
					part.FunctionCall.Name, part.FunctionCall.Args))
				continue
			}
			out = append(out, fmt.Sprintf("text %q", part.Text))
		}
	}
	return out
}

// geminiAccount 起一台假 gemini 上游并建一个 gemini 账号，返回上游收到的 URI。
func geminiAccount(t *testing.T, payload string) (srv *httptest.Server, uri *string) {
	t.Helper()
	f := newFixture(t)
	var gotURI, reqBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(up.Close)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.URL},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini-shape",
		[]string{"gemini-2.5-flash"})
	srv = f.server(t)
	t.Cleanup(func() {
		if strings.Contains(reqBody, `"stream"`) {
			t.Errorf("gemini 的请求体里不该有 stream 字段: %s", reqBody)
		}
		if strings.Contains(reqBody, `"model"`) {
			t.Errorf("gemini 的请求体里不该有 model 字段: %s", reqBody)
		}
	})
	return srv, &gotURI
}

// TestGeminiOutboundStreamToChatToolCall 钉住 gemini 出站流式对 chat 客户端的帧序列：
// 正文逐帧（po、ng 两帧）、工具调用分成「开工具 + 整段参数」两帧（id 补 gemini_call_1）、
// 结束帧是 finish_reason=tool_calls、用量单独一帧、末帧 [DONE]。上游只有 data 行，没有事件名。
func TestGeminiOutboundStreamToChatToolCall(t *testing.T) {
	srv, uri := geminiAccount(t, geminiStreamWithTool)

	status, body := postStreamTo(t, srv, `{"model":"gemini-2.5-flash","max_tokens":16,"stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if want := "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"; *uri != want {
		t.Fatalf("上游 URI = %q，期望 %q", *uri, want)
	}

	want := []string{
		`role="assistant" content=""`,
		`role="" content="po"`,
		`role="" content="ng"`,
		`tool[0] id=gemini_call_1 type=function name=get_weather args=""`,
		`args[0]="{\"city\":\"北京\"}"`,
		`finish=tool_calls`,
		`usage=12/8 choices=0`,
		`[DONE]`,
	}
	if got := chatStreamSignatures(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("chat 客户端的帧序列:\n得到 %v\n期望 %v", got, want)
	}
}

// TestGeminiOutboundStreamToAnthropicToolCall 钉住同一份 gemini 流对 anthropic 客户端的帧序列：
// 正文块先开先关，工具调用是独立的 tool_use 块（id 补 gemini_call_1、参数一次给全），
// message_delta 的 stop_reason 是 tool_use 并带用量，末帧 message_stop。
func TestGeminiOutboundStreamToAnthropicToolCall(t *testing.T) {
	srv, uri := geminiAccount(t, geminiStreamWithTool)

	status, body := postMessages(t, srv, `{"model":"gemini-2.5-flash","max_tokens":16,"stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if want := "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"; *uri != want {
		t.Fatalf("上游 URI = %q，期望 %q", *uri, want)
	}

	want := []string{
		"message_start model=gemini-2.5-flash",
		`block_start[0] text id= name= text=""`,
		`delta[0] text_delta "po"`,
		`delta[0] text_delta "ng"`,
		"block_stop[0]",
		`block_start[1] tool_use id=gemini_call_1 name=get_weather text=""`,
		`delta[1] input_json_delta "{\"city\":\"北京\"}"`,
		"block_stop[1]",
		"message_delta tool_use 12/8",
		"message_stop",
	}
	if got := anthropicStreamSignatures(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("anthropic 客户端的帧序列:\n得到 %v\n期望 %v", got, want)
	}
}

// TestGeminiInboundStreamFromChatToolCall 钉住 chat 上游流式转成 gemini 客户端的帧：
// 首帧立刻给出 role=model 的空候选，正文逐帧，工具调用攒到收尾一次给出（参数由 chat 的
// 两段片段拼成完整对象），最后一帧带 finishReason 与从 chat 用量转来的 usageMetadata。
func TestGeminiInboundStreamFromChatToolCall(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, chatStreamWithTool)
	f.addAs(t, "c1", "ctest", translate.ProtocolChat, up.URL, "sk-chat-shape",
		[]string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	want := []string{
		"start gemini-2.5-flash role=model",
		`text "po"`,
		`text "ng"`,
		`functionCall name=get_weather args={"city":"北京"}`,
		"finish STOP 3/2 total=5",
	}
	if got := geminiStreamSignatures(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini 客户端的帧序列:\n得到 %v\n期望 %v", got, want)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolGemini || recs[0].Outbound != translate.ProtocolChat {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 3 || recs[0].OutTokens != 2 {
		t.Fatalf("记录的用量 = %d/%d，期望 3/2", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestGeminiInboundStreamFromAnthropicToolCall 钉住 anthropic 上游流式转成 gemini 客户端：
// 工具调用在 anthropic 那头是分段的 input_json_delta，到了 gemini 客户端仍然是收尾时
// 一整段 functionCall（id 丢弃，参数拼成合法对象）；收尾帧的 finishReason 是 STOP。
func TestGeminiInboundStreamFromAnthropicToolCall(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, anthropicStreamWithTool)
	f.addAs(t, "a1", "atest", translate.ProtocolAnthropic, up.URL, "sk-ant-shape",
		[]string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	want := []string{
		"start gemini-2.5-flash role=model",
		`text "po"`,
		`text "ng"`,
		`functionCall name=get_weather args={"city":"北京"}`,
		"finish STOP 3/2 total=5",
	}
	if got := geminiStreamSignatures(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini 客户端的帧序列:\n得到 %v\n期望 %v", got, want)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolGemini || recs[0].Outbound != translate.ProtocolAnthropic {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 3 || recs[0].OutTokens != 2 {
		t.Fatalf("记录的用量 = %d/%d，期望 3/2", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestGeminiInboundStreamAcceptsAltSSEQuery 钉住 Google SDK 的请求写法：流式一律带 `?alt=sse`。
// 入站协议与模型名只看路径本身，查询串不影响判定，帧照旧转成 gemini 形状。
func TestGeminiInboundStreamAcceptsAltSSEQuery(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, anthropicStreamWithTool)
	f.addAs(t, "a1", "atest", translate.ProtocolAnthropic, up.URL, "sk-ant-alt-sse",
		[]string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, body := postPathTo(t, srv,
		"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	want := []string{
		"start gemini-2.5-flash role=model",
		`text "po"`,
		`text "ng"`,
		`functionCall name=get_weather args={"city":"北京"}`,
		"finish STOP 3/2 total=5",
	}
	if got := geminiStreamSignatures(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini 客户端的帧序列:\n得到 %v\n期望 %v", got, want)
	}
}
