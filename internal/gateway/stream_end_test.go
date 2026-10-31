package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/translate"
)

// 这个文件钉住「一帧怎么才算收尾」：判据是帧里的 type 字段或 SSE 的 event 名，不是字节子串，
// 收尾帧到齐的完整流一个字节都不追加；收尾帧没到就是断流，转换与直通两条路径对断流给出同一个
// 结论：都补错误帧，都把那一次记成 ok=0。

// streamPost 用某协议的最小请求体打开一条流，返回状态与客户端收到的全部字节。
func streamPost(t *testing.T, srv *httptest.Server, protocol string) (int, string) {
	t.Helper()
	body := strings.Replace(requestBodies[protocol], `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	return postTo(t, srv, body, protocol)
}

// fixedSSEUpstream 原样写出一段 SSE 字节，不做任何分帧处理。
func fixedSSEUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPassthroughEndFrameShapeDoesNotMatter 覆盖收尾帧的写法：JSON 里多几个空格、把 JSON 缩进成
// 多行 data 行、只写 event 名不写 type，都还是同一个收尾帧。完整流因此不追加任何字节，
// 直通的字节与上游逐字节相同，账上记 ok=1。
func TestPassthroughEndFrameShapeDoesNotMatter(t *testing.T) {
	spacedAnthropic := strings.Replace(streamBodies[gateway.ProtocolAnthropic],
		"data: {\"type\":\"message_stop\"}",
		"data: {\ndata:   \"type\": \"message_stop\"\ndata: }", 1)
	spacedResponses := strings.Replace(streamBodies[gateway.ProtocolResponses],
		"\"type\":\"response.completed\"", "\"type\": \"response.completed\"", 1)
	// 只有事件名、data 里没有 type 的收尾帧：responses 的客户端认事件名。
	eventOnlyResponses := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"up"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"po"}`,
		``,
		`event: response.completed`,
		`data: {"response":{"id":"r1","status":"completed"}}`,
		``,
	}, "\n")

	cases := []struct {
		name     string
		protocol string
		payload  string
		// rewritten 为真表示这条用例换掉了收尾帧的写法，用来确认改写真的生效。
		rewritten bool
	}{
		{"anthropic 收尾帧缩进成多行", gateway.ProtocolAnthropic, spacedAnthropic, true},
		{"responses 收尾帧带空格", gateway.ProtocolResponses, spacedResponses, true},
		{"responses 只写事件名", gateway.ProtocolResponses, eventOnlyResponses, true},
		{"chat 收尾帧是 [DONE]", gateway.ProtocolChat, streamBodies[gateway.ProtocolChat], false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.rewritten && c.payload == streamBodies[c.protocol] {
				t.Fatalf("测试用例写错了：%s 的收尾帧没有被改写", c.name)
			}
			f := newFixture(t)
			const key = "sk-end-frame"
			up := fixedSSEUpstream(t, c.payload)
			f.add(t, "ds1", c.protocol, up.URL, key, []string{"m1"}, 100)
			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			status, body := streamPost(t, srv, c.protocol)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，期望 200", status)
			}
			// 收尾帧到齐：一个字节都不追加，连补的空行都不该有。
			if body != c.payload {
				t.Fatalf("完整流被改动了：\n得到 %q\n期望 %q", tail(body, 120), tail(c.payload, 120))
			}
			if strings.Contains(body, "event: error") || strings.Contains(body, `"error"`) {
				t.Fatalf("完整流不该出现错误帧：\n%s", tail(body, 200))
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || !recs[0].OK || recs[0].Error != "" {
				t.Fatalf("请求记录 = %+v，期望一行 ok=1 且无错误摘要", recs)
			}
			if leaked := logs(); strings.Contains(leaked, key) {
				t.Fatalf("网关日志泄漏了凭据：%s", leaked)
			}
		})
	}
}

// cutAtFrameBoundary 把完整流截在帧边界上（以空行结尾、没有收尾帧）：上游把话说完一半
// 就干净地关掉连接，解码器一声不响，只有「收尾帧没到」能说明这次是断流。
func cutAtFrameBoundary(t *testing.T, protocol, marker string) string {
	t.Helper()
	full := streamBodies[protocol]
	idx := strings.Index(full, marker)
	if idx <= 0 {
		t.Fatalf("测试用例写错了：完整流里找不到 %q", marker)
	}
	cut := full[:idx]
	if !strings.HasSuffix(cut, "\n\n") {
		t.Fatalf("测试用例写错了：切点应落在帧边界上，实际结尾 %q", tail(cut, 20))
	}
	return cut
}

// TestConversionStreamCleanTruncationIsFailure 覆盖转换路径上的干净截断（两帧正文都到了，
// 收尾帧没到，连接干净结束）：客户端仍拿到错误帧加合法收尾，这一次记成失败。
// 直通路径对同一个形状的处理见 TestPassthroughStreamDisconnectKeepsLegalEnding。
func TestConversionStreamCleanTruncationIsFailure(t *testing.T) {
	written := cutAtFrameBoundary(t, gateway.ProtocolChat, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	const key = "sk-clean-cut"
	f := newFixture(t)
	up := closedSSEUpstream(t, written)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（响应头已经发出）", status)
	}
	if got := strings.Join(textDeltas(t, body), "|"); got != "po|ng" {
		t.Fatalf("正文增量 = %q，期望 %q", got, "po|ng")
	}
	events := anthropicEvents(t, body)
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"error", "content_block_stop", "message_delta", "message_stop",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", events, want)
	}
	if _, message := errorFrame(t, body); !strings.Contains(message, "收尾帧") {
		t.Fatalf("错误帧的说明 = %q，期望指出收尾帧没到", message)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
	if !strings.Contains(recs[0].Error, "收尾帧") {
		t.Fatalf("请求记录的 error = %q，期望写明收尾帧没到", recs[0].Error)
	}
	// 断流的说明进日志、进流、进请求记录，这三处都不该出现凭据明文。
	if leaked := logs(); strings.Contains(leaked, key) {
		t.Fatalf("网关日志泄漏了凭据：%s", leaked)
	}
	if strings.Contains(body, key) || strings.Contains(recs[0].Error, key) {
		t.Fatalf("流或请求记录泄漏了凭据：%q", recs[0].Error)
	}
}

// TestConversionStreamResponsesInboundEndsFailed 覆盖 responses 入站的转换路径断流：
// 客户端拿到的终态是 response.failed，不是 response.completed——上游出错却告诉客户端
// 「已完成」，与 requests 里的 ok=0 自相矛盾。
func TestConversionStreamResponsesInboundEndsFailed(t *testing.T) {
	written := cutAtFrameBoundary(t, gateway.ProtocolChat, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	f := newFixture(t)
	up := closedSSEUpstream(t, written)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-resp-cut", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolResponses)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}
	if strings.Contains(got, "response.completed") {
		t.Fatalf("断流不该报 completed：\n%s", tail(got, 300))
	}

	var failed struct {
		Type     string `json:"type"`
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(lastEventData(t, got, "response.failed")), &failed); err != nil {
		t.Fatalf("response.failed 的 data 不是合法 JSON: %v", err)
	}
	if failed.Response.Status != "failed" || failed.Response.Error.Code != "server_error" {
		t.Fatalf("response.failed = %+v，期望 status=failed、code=server_error", failed.Response)
	}
	if !strings.Contains(failed.Response.Error.Message, "收尾帧") {
		t.Fatalf("response.failed 的 message = %q，期望写明收尾帧没到", failed.Response.Error.Message)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("SSE 流必须以空行结尾，实际结尾 %q", tail(got, 40))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
}

// TestConversionStreamCompleteKeepsCompleted 确认修完断流判定之后，完整流的转换结果没变：
// responses 入站的完整流仍以 response.completed 收尾，不出现 response.failed。
func TestConversionStreamCompleteKeepsCompleted(t *testing.T) {
	f := newFixture(t)
	up := fixedSSEUpstream(t, streamBodies[gateway.ProtocolChat])
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-resp-complete", []string{"m1"}, 100)
	srv := f.server(t)

	status, got := streamPost(t, srv, gateway.ProtocolResponses)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, got)
	}
	if strings.Contains(got, "response.failed") {
		t.Fatalf("完整流不该报 failed：\n%s", tail(got, 300))
	}
	if !strings.Contains(got, "response.completed") {
		t.Fatalf("完整流应以 response.completed 收尾：\n%s", tail(got, 300))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=1", recs)
	}
}

// TestPassthroughCRLFTailStartsAtFrameBoundary 钉住 CRLF 流的尾帧边界：`\r\n\r\n` 已经是帧边界，
// 追加之前不该再补一个 `\n\n`，否则客户端会收到一个空帧，收尾事件前面多出一段空白。
func TestPassthroughCRLFTailStartsAtFrameBoundary(t *testing.T) {
	full := streamBodies[translate.ProtocolAnthropic]
	idx := strings.Index(full, "event: message_stop")
	if idx <= 0 {
		t.Fatalf("测试用例写错了：完整流里没有 message_stop")
	}
	written := full[:idx]
	if !strings.HasSuffix(written, "\n\n") {
		t.Fatalf("测试用例写错了：切点应落在帧边界上，实际结尾 %q", tail(written, 20))
	}

	f := newFixture(t)
	up := passthroughCRLFUpstream(t, written)
	f.add(t, "c1", gateway.ProtocolAnthropic, up.URL, "sk-crlf-tail", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", status)
	}
	// 已转发的字节一个不改，CRLF 保持 CRLF。
	prefix := strings.ReplaceAll(written, "\n", "\r\n")
	if !strings.HasPrefix(body, prefix) {
		t.Fatalf("直通改动了已写出的字节：\n%s", tail(body, 200))
	}
	// 追加的第一帧紧接在 `\r\n\r\n` 之后，中间没有多出来的空行。
	if !strings.Contains(body, "}\r\n\r\nevent: error\n") {
		t.Fatalf("追加的帧没有紧接在帧边界之后：\n%s", tail(body, 200))
	}
	if strings.Contains(body, "\r\n\r\n\n") {
		t.Fatalf("CRLF 帧边界之后多补了空行：\n%s", tail(body, 200))
	}
	if !strings.HasSuffix(body, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Fatalf("流应以 message_stop 收尾，实际结尾 %q", tail(body, 80))
	}
	if bytes.Count([]byte(body), []byte("event: error\n")) != 1 {
		t.Fatalf("断流只该补一个错误帧：\n%s", tail(body, 200))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=0", recs)
	}
}

// TestConversionStreamUpstreamErrorFrameIsFailure 覆盖上游自己发错误帧、随后干净关连接的形状：
// 客户端只收到一条错误帧（不重复补一条），这一次仍记成失败，错误摘要写上游的原话。
func TestConversionStreamUpstreamErrorFrameIsFailure(t *testing.T) {
	written := "" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"po\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"模型名不对\",\"type\":\"invalid_request_error\"}}\n\n"

	f := newFixture(t)
	up := closedSSEUpstream(t, written)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-upstream-err", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（响应头已经发出）", status)
	}
	// errorFrame 在多条错误帧时直接失败：上游那条之后不该再补一条。
	if _, message := errorFrame(t, body); !strings.Contains(message, "模型名不对") {
		t.Fatalf("错误帧的说明 = %q，期望带上游原话", message)
	}
	events := anthropicEvents(t, body)
	if events[len(events)-1] != "message_stop" {
		t.Fatalf("流仍应以 message_stop 收尾，事件序列 = %v", events)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
	if !strings.Contains(recs[0].Error, "模型名不对") {
		t.Fatalf("请求记录的 error = %q，期望写上游原话", recs[0].Error)
	}
}

// TestConversionStreamTruncatedToolArgsNote 覆盖上游停在工具参数中间的断流：客户端的
// 错误帧要说明参数不完整（已转发的 partial_json 撤不回来），这一次仍记 ok=0。
func TestConversionStreamTruncatedToolArgsNote(t *testing.T) {
	written := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"稍等"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"北"}}]}}]}`,
		``,
	}, "\n")

	f := newFixture(t)
	up := closedSSEUpstream(t, written)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-tool-cut", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := streamPost(t, srv, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（响应头已经发出）", status)
	}
	// 参数片段照原样转发，客户端看到的就是上游中断时的那一截。
	if !strings.Contains(body, `"partial_json":"{\"city\":\"北"`) {
		t.Fatalf("已转发的参数片段被改动了:\n%s", tail(body, 300))
	}
	if _, message := errorFrame(t, body); !strings.Contains(message, "工具调用参数在上游中断") {
		t.Fatalf("错误帧的说明 = %q，期望说明工具参数不完整", message)
	}
	events := anthropicEvents(t, body)
	if events[len(events)-1] != "message_stop" {
		t.Fatalf("流仍应以 message_stop 收尾，事件序列 = %v", events)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
	if !strings.Contains(recs[0].Error, "收尾帧") {
		t.Fatalf("请求记录的 error = %q，期望写明收尾帧没到", recs[0].Error)
	}
}
