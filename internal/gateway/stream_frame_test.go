package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"lax/internal/gateway"
	"lax/internal/translate"
)

// 这个文件钉住流式转发的分帧边界：上游在事件中间、在 UTF-8 多字节字符中间、在收尾事件之前断开时，
// 客户端拿到的事件序列仍必须是一串合法的 SSE（每帧空行结尾、事件名与 data.type 一致），
// 并以错误帧加收尾事件结束。两种上游掉线的形状各用一条用例：
// 声明了完整 Content-Length 却只写一半就关连接，客户端读到 unexpected EOF；
// hijack 写完半截就关连接，客户端读到干净的 EOF，半截事件会真的进解码器。

// chatSSE 是一条完整的 openai-chat 流，正文分两段中文，最后带结束原因与 [DONE]。
const chatSSE = "" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"世界\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// truncatedSSEUpstream 声明完整的 Content-Length，只写 prefix 就返回：服务器随后关掉连接，
// 客户端读响应体时拿到 unexpected EOF。这是「上游只发一半就掉线」的形状。
func truncatedSSEUpstream(t *testing.T, full, prefix string) *httptest.Server {
	t.Helper()
	if !strings.HasPrefix(full, prefix) || prefix == full {
		t.Fatalf("测试用例写错了：prefix 必须是 full 的真前缀")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(prefix))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedSSEUpstream hijack 连接、写完 prefix 直接关掉，不声明长度：客户端把连接结束当成
// 响应体读完，因此是干净的 EOF，半截事件会原样交给解码器。
func closedSSEUpstream(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("假上游不支持 hijack，没法模拟断线")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack 连接: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
		_, _ = buf.WriteString(prefix)
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// anthropicEvents 把客户端收到的 anthropic 流切成帧并逐帧校验形状，返回事件名序列。
// 每一帧必须是「event: 名字」加「data: JSON」两行、空行结尾，且 JSON 的 type 与事件名一致。
func anthropicEvents(t *testing.T, body string) []string {
	t.Helper()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE 流必须以空行结尾，实际结尾 %q", tail(body, 40))
	}
	chunks := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	names := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		lines := strings.Split(chunk, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") {
			t.Fatalf("第 %d 帧形状不对（应为 event 行 + data 行）: %q", i+1, chunk)
		}
		name := strings.TrimPrefix(lines[0], "event: ")
		if !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("第 %d 帧缺 data 行: %q", i+1, chunk)
		}
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &payload); err != nil {
			t.Fatalf("第 %d 帧的 data 不是合法 JSON: %v，%q", i+1, err, chunk)
		}
		if payload.Type != name {
			t.Fatalf("第 %d 帧的事件名 %q 与 data.type %q 不一致", i+1, name, payload.Type)
		}
		names = append(names, name)
	}
	return names
}

// tail 取字符串末尾 n 个字节，报错文案里用。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// textDeltas 取出流里每个 text_delta 的正文，按出现顺序返回。
func textDeltas(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, chunk := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(chunk, "event: content_block_delta\n") {
			continue
		}
		_, data, ok := strings.Cut(chunk, "data: ")
		if !ok {
			continue
		}
		var payload struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("content_block_delta 的 data 不是合法 JSON: %v，%q", err, data)
		}
		if payload.Delta.Type == "text_delta" {
			out = append(out, payload.Delta.Text)
		}
	}
	return out
}

// errorFrame 取出流里的错误帧内容：type、message 与它在事件序列里的位置。
func errorFrame(t *testing.T, body string) (kind, message string) {
	t.Helper()
	found := false
	for _, chunk := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(chunk, "event: error\n") {
			continue
		}
		if found {
			t.Fatalf("流里出现了多个错误帧:\n%s", body)
		}
		found = true
		_, data, _ := strings.Cut(chunk, "data: ")
		var payload struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("错误帧的 data 不是合法 JSON: %v，%q", err, data)
		}
		kind, message = payload.Error.Type, payload.Error.Message
	}
	if !found {
		t.Fatalf("断流之后应有一个错误帧:\n%s", body)
	}
	return kind, message
}

// streamFrom 用一条 stream=true 的请求打网关，读完整条流。
func streamFrom(t *testing.T, srv *httptest.Server, inbound, model string) (int, string, string) {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+inboundPath(inbound), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String(), resp.Header.Get("Content-Type")
}

// inboundPath 把协议名换成入站路径。
func inboundPath(protocol string) string {
	switch protocol {
	case gateway.ProtocolChat:
		return "/v1/chat/completions"
	case gateway.ProtocolResponses:
		return "/v1/responses"
	}
	return "/v1/messages"
}

// TestStreamCutMidEventEndsWithErrorAndStop 覆盖上游在事件中间断流（干净 EOF）：
// 半截 JSON 交给解码器时报错，客户端仍拿到 message_start、错误帧、message_delta 与 message_stop，
// 请求记录里那一次算失败并写明上游中断。
func TestStreamCutMidEventEndsWithErrorAndStop(t *testing.T) {
	// 切点落在第一条 data 的 JSON 中间，连换行都没有。
	prefix := `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant",`
	f := newFixture(t)
	up := closedSSEUpstream(t, prefix)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-cut-event", []string{"m1"}, 100)
	srv := f.server(t)

	status, body, ctype := streamFrom(t, srv, gateway.ProtocolAnthropic, "m1")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（流式已在写响应头之后）", status)
	}
	if !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ctype)
	}

	events := anthropicEvents(t, body)
	want := []string{"message_start", "error", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", events, want)
	}
	kind, message := errorFrame(t, body)
	if kind != "api_error" {
		t.Fatalf("错误帧的 type = %q，期望 api_error", kind)
	}
	if !strings.Contains(message, "不是合法 JSON") {
		t.Fatalf("错误帧的说明 = %q，期望指出半截帧解不出来", message)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
	if !strings.Contains(recs[0].Error, "上游流式响应中断") {
		t.Fatalf("请求记录的 error = %q，期望写明上游中断", recs[0].Error)
	}
	// 用量取 0/0：转换路径的用量只来自解码出的 usage 增量，chat 的 usage 在最后一帧里，
	// 这次断流一帧 usage 都没转发过去，scanner 无东西可读。
	if recs[0].InTokens != 0 || recs[0].OutTokens != 0 {
		t.Fatalf("请求记录的用量 = %d/%d，期望 0/0（断流前没有 usage 帧）", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestStreamCutInsideMultibyteRuneKeepsValidUTF8 覆盖切点在 UTF-8 三字节字符中间：
// 断点前的完整中文正文原样到达客户端，收到的整段字节仍是合法 UTF-8，
// 末尾仍是错误帧加 message_stop。
func TestStreamCutInsideMultibyteRuneKeepsValidUTF8(t *testing.T) {
	// 第一条事件完整（含「你好」），第二条切在「世」的三个字节中间，后面的字节都没发。
	firstEvent := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n"
	second := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"世"
	prefix := firstEvent + second
	if !utf8.ValidString(prefix) {
		t.Fatalf("测试用例写错了：切点应落在多字节字符中间: %q", prefix)
	}
	// "世" 是 e4 b8 96，去掉最后两个字节就是切在字符中间。
	prefix = firstEvent + second[:len(second)-2]
	if utf8.ValidString(prefix) {
		t.Fatalf("测试用例写错了：切点应落在多字节字符中间: %q", prefix)
	}

	f := newFixture(t)
	up := closedSSEUpstream(t, prefix)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-cut-rune", []string{"m1"}, 100)
	srv := f.server(t)

	status, body, _ := streamFrom(t, srv, gateway.ProtocolAnthropic, "m1")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", status)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("客户端收到的字节里有被截断的多字节字符: %q", tail(body, 80))
	}

	// 断点之前那条完整事件的中文正文必须原样到达。
	if got := textDeltas(t, body); len(got) != 1 || got[0] != "你好" {
		t.Fatalf("正文增量 = %v，期望只有断点前的 [你好]", got)
	}

	events := anthropicEvents(t, body)
	if events[len(events)-1] != "message_stop" {
		t.Fatalf("最后一条事件 = %q，期望 message_stop（事件序列 %v）", events[len(events)-1], events)
	}
	if _, message := errorFrame(t, body); !strings.Contains(message, "不是合法 JSON") {
		t.Fatalf("错误帧的说明 = %q，期望指出半截帧解不出来", message)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK || !strings.Contains(recs[0].Error, "上游流式响应中断") {
		t.Fatalf("请求记录 = %+v，期望一行 ok=0 且写明上游中断", recs)
	}
	// 这个断点同样落在 usage 帧之前：已读到的只有正文帧，记账因此是 0/0。
	if recs[0].InTokens != 0 || recs[0].OutTokens != 0 {
		t.Fatalf("请求记录的用量 = %d/%d，期望 0/0", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestStreamDisconnectBeforeFinishFrameKeepsLegalEnding 覆盖上游把正文都发完、只差收尾帧
// （[DONE]）时掉线：客户端已收到两段正文，末尾仍补上错误帧与 message_stop，
// 不会停在半个流上。
func TestStreamDisconnectBeforeFinishFrameKeepsLegalEnding(t *testing.T) {
	// 三帧事件完整，[DONE] 与它前面的收尾都没发。
	prefix := chatSSE[:strings.Index(chatSSE, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}")]
	f := newFixture(t)
	up := truncatedSSEUpstream(t, chatSSE, prefix)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-cut-finish", []string{"m1"}, 100)
	srv := f.server(t)

	status, body, _ := streamFrom(t, srv, gateway.ProtocolAnthropic, "m1")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", status)
	}

	if got := strings.Join(textDeltas(t, body), "|"); got != "你好|世界" {
		t.Fatalf("正文增量 = %q，期望 %q", got, "你好|世界")
	}
	// 错误帧在收尾事件之前：content_block_stop、message_delta、message_stop 都在它之后补齐。
	events := anthropicEvents(t, body)
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"error", "content_block_stop", "message_delta", "message_stop",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列 = %v，期望 %v", events, want)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
	}
	if recs[0].HTTPStatus != http.StatusOK {
		t.Fatalf("这条记录的 http_status = %d，期望 200（响应头已经发出）", recs[0].HTTPStatus)
	}
	// chat 的 usage 只写在最后一帧（stream_options.include_usage），这次就是那一帧没到，
	// 所以记账是 0/0；写非零就是把没读到的用量当成真数字。
	if recs[0].InTokens != 0 || recs[0].OutTokens != 0 {
		t.Fatalf("请求记录的用量 = %d/%d，期望 0/0（usage 帧没到）", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestPassthroughStreamDisconnectKeepsLegalEnding 覆盖同协议直通时上游掉线，三种协议各一条：
// 已经读到的字节按原样转发，末尾按入站协议补上合法结尾。补结尾是协议要求，不是顺手加的：
// 上游中断时向客户端发一个错误事件并结束本次请求，直通与转换两条路径
// 对断流的处理一致：anthropic 补 event: error 加 message_delta（带已知 usage）
// 与 message_stop，openai-chat 补一帧 data: {"error":…} 加 data: [DONE]，
// openai-responses 补 event: error 加 response.failed。
// 收尾帧到齐的完整流不会被追加任何字节，这条由 matrix_stream_test 的逐字节断言钉住。
func TestPassthroughStreamDisconnectKeepsLegalEnding(t *testing.T) {
	const key = "sk-passthru-cut"

	cutBefore := func(full, marker string) string {
		idx := strings.Index(full, marker)
		if idx <= 0 {
			t.Fatalf("测试用例写错了：完整流里找不到 %q", marker)
		}
		return full[:idx]
	}

	cases := []struct {
		name     string
		protocol string
		// body 是假上游这次只写出来的字节，声明完整长度后关连接，客户端读到 unexpected EOF。
		body func(t *testing.T) string
		// check 断言追加的收尾帧。
		check func(t *testing.T, body string)
		// wantIn / wantOut 是这次断流在 requests 行里的用量，依据逐条写在各用例里。
		wantIn, wantOut int64
	}{
		{
			name:     "anthropic",
			protocol: gateway.ProtocolAnthropic,
			// 用量来自已转发的 message_start（input_tokens=3）与 message_delta（output_tokens=2）：
			// anthropic 把用量分在这两帧里，断在 message_stop 之前时两帧都已经读到了。
			wantIn: 3, wantOut: 2,
			body: func(t *testing.T) string {
				return cutBefore(streamBodies[translate.ProtocolAnthropic], "event: message_stop")
			},
			check: func(t *testing.T, body string) {
				if !strings.Contains(body, "event: error\n") {
					t.Fatalf("断流之后应有一个 error 事件：\n%s", tail(body, 200))
				}
				var delta struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						In  int64 `json:"input_tokens"`
						Out int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(lastEventData(t, body, "message_delta")), &delta); err != nil {
					t.Fatalf("message_delta 的 data 不是合法 JSON: %v", err)
				}
				// 用量取自已转发的 message_start 与 message_delta，不是零值。
				if delta.Usage.In != 3 || delta.Usage.Out != 2 {
					t.Fatalf("message_delta 的 usage = %d/%d，期望 3/2（scanner 已读到的用量）", delta.Usage.In, delta.Usage.Out)
				}
				if delta.Delta.StopReason != "end_turn" {
					t.Fatalf("message_delta 的 stop_reason = %q，期望 end_turn", delta.Delta.StopReason)
				}
				if want := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"; !strings.HasSuffix(body, want) {
					t.Fatalf("流应以 message_stop 结尾，实际结尾 %q", tail(body, 80))
				}
				// 顺序：错误帧在收尾事件之前，客户端先看到出了什么事，再看到流合法结束。
				events := anthropicEvents(t, body)
				if got := strings.Join(events, ","); !strings.HasSuffix(got, "error,message_delta,message_stop") {
					t.Fatalf("事件序列的结尾 = %q，期望 error 之后紧跟 message_delta、message_stop", got)
				}
			},
		},
		{
			name:     "openai-chat",
			protocol: gateway.ProtocolChat,
			// chat 的 usage 只在最后一帧里（stream_options.include_usage），切在正文中间时
			// 一帧 usage 都没转发过去，scanner 读不到，用量就是 0/0。
			wantIn: 0, wantOut: 0,
			body: func(t *testing.T) string {
				// 切在第二条 data 的 JSON 中间：连换行都没有，追加之前必须先补空行。
				first := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n"
				return first + `data: {"id":"c1","choices":[{"index":0,"delta":`
			},
			check: func(t *testing.T, body string) {
				if !strings.Contains(body, "\n\ndata: {\"error\":") {
					t.Fatalf("断流之后应有一个 data: {\"error\":…} 帧：\n%s", tail(body, 200))
				}
				if !strings.HasSuffix(body, "\n\ndata: [DONE]\n\n") {
					t.Fatalf("流应以 data: [DONE] 结尾，实际结尾 %q", tail(body, 80))
				}
				// 错误帧在 [DONE] 之前，内容是 chat 形状的错误体而不是空帧。
				errIdx := strings.Index(body, "data: {\"error\":")
				doneIdx := strings.LastIndex(body, "data: [DONE]")
				if errIdx < 0 || doneIdx < 0 || errIdx > doneIdx {
					t.Fatalf("错误帧应在 [DONE] 之前（error 位置 %d，[DONE] 位置 %d）", errIdx, doneIdx)
				}
				line := body[errIdx+len("data: "):]
				if end := strings.Index(line, "\n"); end >= 0 {
					line = line[:end]
				}
				var frame struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(line), &frame); err != nil {
					t.Fatalf("错误帧的 data 不是合法 JSON: %v，%q", err, line)
				}
				if frame.Error.Type != "api_error" || !strings.Contains(frame.Error.Message, "上游流式响应中断") {
					t.Fatalf("错误帧 = %+v，期望 type=api_error 且说明上游中断", frame.Error)
				}
			},
		},
		{
			name:     "openai-responses",
			protocol: gateway.ProtocolResponses,
			// responses 的用量只写在 response.completed / response.incomplete 里，
			// 断流时那一帧从未到过，所以记账是 0/0。
			wantIn: 0, wantOut: 0,
			body: func(t *testing.T) string {
				return cutBefore(streamBodies[translate.ProtocolResponses], "data: {\"type\":\"response.completed\"")
			},
			check: func(t *testing.T, body string) {
				if !strings.Contains(body, "event: error\n") {
					t.Fatalf("断流之后应有一个 error 事件：\n%s", tail(body, 200))
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
				if err := json.Unmarshal([]byte(lastEventData(t, body, "response.failed")), &failed); err != nil {
					t.Fatalf("response.failed 的 data 不是合法 JSON: %v", err)
				}
				if failed.Type != "response.failed" || failed.Response.Status != "failed" ||
					failed.Response.Error.Code != "server_error" {
					t.Fatalf("response.failed 的内容 = %+v，期望 status=failed、code=server_error", failed)
				}
				if !strings.Contains(failed.Response.Error.Message, "上游流式响应中断") {
					t.Fatalf("response.failed 的 message = %q，期望写明上游中断", failed.Response.Error.Message)
				}
				// 事件序列以设计约定的两个收尾事件结束：error 再 response.failed，
				// responses 没有 [DONE]，客户端认的是终态事件。
				events := responsesEvents(t, body)
				if got := strings.Join(events, ","); !strings.HasSuffix(got, "error,response.failed") {
					t.Fatalf("事件序列的结尾 = %q，期望以 error,response.failed 收束", got)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			written := c.body(t)
			up := truncatedSSEUpstream(t, written+"rest-never-sent", written)
			f := newFixture(t)
			f.add(t, "ds1", c.protocol, up.URL, key, []string{"m1"}, 100)
			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			status, body, ctype := streamFrom(t, srv, c.protocol, "m1")
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，期望 200（流式已在写响应头之后）", status)
			}
			if !strings.HasPrefix(ctype, "text/event-stream") {
				t.Fatalf("Content-Type = %q", ctype)
			}
			// 已写出的字节不动：直通的内容必须原样是上游的前缀，收尾帧只往后接。
			if !strings.HasPrefix(body, written) {
				t.Fatalf("直通改动了已写出的字节：\n得到 %q\n期望前缀 %q", tail(body, 120), tail(written, 120))
			}
			c.check(t, body)

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || recs[0].OK {
				t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
			}
			if !strings.Contains(recs[0].Error, "上游流式响应中断") {
				t.Fatalf("请求记录的 error = %q，期望写明上游中断", recs[0].Error)
			}
			if recs[0].InTokens != c.wantIn || recs[0].OutTokens != c.wantOut {
				t.Fatalf("请求记录的用量 = %d/%d，期望 %d/%d（依据见该用例的 wantIn/wantOut 注释）",
					recs[0].InTokens, recs[0].OutTokens, c.wantIn, c.wantOut)
			}

			// 凭据只以密文入库：日志、客户端收到的字节、请求记录里都不该出现 key 明文。
			if leaked := logs(); strings.Contains(leaked, key) {
				t.Fatalf("网关日志泄漏了凭据：%s", leaked)
			}
			if strings.Contains(body, key) || strings.Contains(recs[0].Error, key) {
				t.Fatalf("流或请求记录泄漏了凭据：%q", tail(body, 120))
			}
		})
	}
}

// responsesEvents 按出现顺序取出每一帧 data 里的 type，返回事件序列。
// responses 的上游帧可以只有 data 行、不写 event 行，所以这里不看 event 行。
// 只用于帧边界完整的流：半截帧的 data 解不出 JSON，那种情况由用例自己的断言处理。
func responsesEvents(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, chunk := range strings.Split(body, "\n\n") {
		line := ""
		for _, l := range strings.Split(chunk, "\n") {
			if strings.HasPrefix(l, "data: ") {
				line = strings.TrimPrefix(l, "data: ")
				break
			}
		}
		if line == "" {
			continue
		}
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("这一帧的 data 不是合法 JSON: %v，%q", err, line)
		}
		if payload.Type == "" {
			t.Fatalf("这一帧没有 type: %q", line)
		}
		out = append(out, payload.Type)
	}
	if len(out) == 0 {
		t.Fatalf("流里一帧都没有:\n%s", body)
	}
	return out
}

// lastEventData 取出某个具名事件最后一次出现时的 data 行内容。
func lastEventData(t *testing.T, body, name string) string {
	t.Helper()
	marker := "event: " + name + "\ndata: "
	idx := strings.LastIndex(body, marker)
	if idx < 0 {
		t.Fatalf("流里没有 %s 事件：\n%s", name, tail(body, 200))
	}
	rest := body[idx+len(marker):]
	if end := strings.Index(rest, "\n"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}
