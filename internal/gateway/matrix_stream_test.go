package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/translate"
)

// 各协议的最小流式响应，正文都是先 po 再 ng。
var streamBodies = map[string]string{
	translate.ProtocolAnthropic: strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"),

	translate.ProtocolChat: strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"po"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ng"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"),

	translate.ProtocolResponses: strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"up"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"po"}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ng"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`,
		``,
	}, "\n"),

	// gemini 的流式帧没有事件名，只有 data 行；末尾的 finishReason 就是收尾。
	translate.ProtocolGemini: strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[]},"index":0,"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
		``,
	}, "\n"),
}

// 各协议流式请求的收尾标记。
var streamEndMarkers = map[string]string{
	translate.ProtocolAnthropic: "message_stop",
	translate.ProtocolChat:      "[DONE]",
	translate.ProtocolResponses: "response.completed",
	// gemini 没有单独的结束事件，最后一帧的 finishReason 就是收尾（DESIGN 5.4）。
	translate.ProtocolGemini: "finishReason",
}

func fakeStreamUpstream(t *testing.T, protocol string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	body := streamBodies[protocol]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		rec.set(r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), string(reqBody))
		rec.uri = r.URL.RequestURI()
		rec.googKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestProtocolMatrixStreaming 验证十六种组合的流式转发。
func TestProtocolMatrixStreaming(t *testing.T) {
	for _, inbound := range fourProtocols {
		for _, outbound := range fourProtocols {
			t.Run(inbound+"_to_"+outbound, func(t *testing.T) {
				f := newFixture(t)
				up, rec := fakeStreamUpstream(t, outbound)
				f.add(t, "up-"+outbound, outbound, up.URL, "sk-test", []string{"m1"}, 100)
				srv := f.server(t)

				// gemini 的流式由路径后缀定，请求体里没有 stream 字段；其余三条要显式开流。
				body := requestBodies[inbound]
				if inbound != translate.ProtocolGemini {
					body = strings.Replace(body, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
				}
				status, got := postStreamTo(t, srv, body, inbound)
				if status != http.StatusOK {
					t.Fatalf("状态 = %d，响应 %s", status, got)
				}

				if !strings.Contains(got, "po") || !strings.Contains(got, "ng") {
					t.Fatalf("流里缺少正文片段:\n%s", got)
				}
				if !strings.Contains(got, streamEndMarkers[inbound]) {
					t.Fatalf("%s 流缺少收尾标记 %q:\n%s", inbound, streamEndMarkers[inbound], got)
				}

				// gemini 客户端的帧没有事件名：只有 data 行，带 event: 就不是 gemini 的形状。
				if inbound == translate.ProtocolGemini && strings.Contains(got, "event:") {
					t.Fatalf("gemini 客户端的流不该有 event 行:\n%s", got)
				}

				// 出站是 gemini 时，流式端点由 :generateContent 换成 :streamGenerateContent
				// 并补 alt=sse：不加 alt=sse 上游返回的是一段 JSON 数组，不是 SSE。
				if outbound == translate.ProtocolGemini {
					want := "/v1beta/models/m1:streamGenerateContent?alt=sse"
					if rec.uri != want {
						t.Fatalf("上游 URI = %q，期望 %q", rec.uri, want)
					}
					if rec.googKey != "sk-test" || rec.bearer != "" || rec.apiKey != "" {
						t.Fatalf("gemini 凭据头 = x-goog-api-key %q，Authorization %q，x-api-key %q",
							rec.googKey, rec.bearer, rec.apiKey)
					}
					if strings.Contains(rec.body, `"stream"`) {
						t.Fatalf("gemini 的请求体里不该有 stream 字段: %s", rec.body)
					}
				}

				// 同协议直通：字节应与上游完全一致。
				if inbound == outbound {
					if strings.TrimSpace(got) != strings.TrimSpace(streamBodies[outbound]) {
						t.Fatalf("直通改动了流字节:\n得到 %s\n期望 %s", got, streamBodies[outbound])
					}
				}

				recs, err := f.st.RecentRequests(5)
				if err != nil {
					t.Fatalf("读请求记录: %v", err)
				}
				if len(recs) != 1 || !recs[0].OK {
					t.Fatalf("请求记录 = %+v", recs)
				}
				if recs[0].InTokens != 3 || recs[0].OutTokens != 2 {
					t.Fatalf("流式用量记录 = %d/%d，期望 3/2", recs[0].InTokens, recs[0].OutTokens)
				}
			})
		}
	}
}
