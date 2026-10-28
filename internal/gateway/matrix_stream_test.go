package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/translate"
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
}

// 各协议流式请求的收尾标记。
var streamEndMarkers = map[string]string{
	translate.ProtocolAnthropic: "message_stop",
	translate.ProtocolChat:      "[DONE]",
	translate.ProtocolResponses: "response.completed",
}

func fakeStreamUpstream(t *testing.T, protocol string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	body := streamBodies[protocol]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		rec.set(r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), string(reqBody))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestProtocolMatrixStreaming 验证九种组合的流式转发。
func TestProtocolMatrixStreaming(t *testing.T) {
	protocols := []string{translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses}

	for _, inbound := range protocols {
		for _, outbound := range protocols {
			t.Run(inbound+"_to_"+outbound, func(t *testing.T) {
				f := newFixture(t)
				up, _ := fakeStreamUpstream(t, outbound)
				f.add(t, "up-"+outbound, outbound, up.URL, "sk-test", []string{"m1"}, 100)
				srv := f.server(t)

				body := strings.Replace(requestBodies[inbound], `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
				status, got := postTo(t, srv, body, inbound)
				if status != http.StatusOK {
					t.Fatalf("状态 = %d，响应 %s", status, got)
				}

				if !strings.Contains(got, "po") || !strings.Contains(got, "ng") {
					t.Fatalf("流里缺少正文片段:\n%s", got)
				}
				if !strings.Contains(got, streamEndMarkers[inbound]) {
					t.Fatalf("%s 流缺少收尾标记 %q:\n%s", inbound, streamEndMarkers[inbound], got)
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
