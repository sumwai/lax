package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"switchboard/internal/gateway"
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
}

// TestPassthroughStreamDisconnectRecordsTruncation 覆盖同协议直通时上游掉线：
// 已经读到的字节按原样转发给客户端，请求记录写明上游中断。
//
// 直通路径目前只转发已读到的字节，不补错误帧也不补收尾事件（gateway.passthroughStream
// 在 readErr 之后直接返回），与 DESIGN 5.2「上游中断则向客户端发一个错误事件」不一致；
// 这里钉的是当前行为，改实现时把断言一起改掉。
func TestPassthroughStreamDisconnectRecordsTruncation(t *testing.T) {
	firstEvent := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n"
	prefix := firstEvent + `data: {"id":"c1","choices":[{"index":0,"delta":`
	if !strings.HasPrefix(chatSSE, prefix) {
		t.Fatalf("测试用例写错了：prefix 必须是 chatSSE 的真前缀")
	}
	f := newFixture(t)
	up := truncatedSSEUpstream(t, chatSSE, prefix)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-passthru-cut", []string{"m1"}, 100)
	srv := f.server(t)

	status, body, ctype := streamFrom(t, srv, gateway.ProtocolChat, "m1")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", status)
	}
	if !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ctype)
	}
	if body != prefix {
		t.Fatalf("直通应在断点处停下，收到 %q，期望 %q", body, prefix)
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
}
