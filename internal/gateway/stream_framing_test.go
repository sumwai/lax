package gateway_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/translate"
)

// 这个文件钉住上游的分帧形状不影响跨协议转换的结果：上游怎么把同一串 SSE 字节写出来
// （一字节一写、手写分块编码、CRLF 行尾）都是传输层的事，客户端拿到的增量与结尾必须一样。
// 每条用例都是 chat 上游 → anthropic 入站的转换路径，断言的是编出来的事件序列，不是上游形状。

// byteAtATimeUpstream 每写一个字节就 flush，把帧切得最碎：网关必须在多次读之间
// 把半个事件攒起来，不能把每段读到的字节当成一帧。
func byteAtATimeUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < len(payload); i++ {
			if _, err := w.Write([]byte{payload[i]}); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chunkedUpstream 自己 hijack 连接并手写分块编码，块边界故意落在 data 行的中间：
// 客户端读到的 chunk 边界与事件边界无关，网关不能把 chunk 当成帧。
func chunkedUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	const chunk = 7
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("假上游不支持 hijack，没法手写分块编码")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack 连接: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream\r\n" +
			"Transfer-Encoding: chunked\r\n" +
			"Connection: close\r\n\r\n")
		for i := 0; i < len(payload); i += chunk {
			end := i + chunk
			if end > len(payload) {
				end = len(payload)
			}
			part := payload[i:end]
			if _, err := fmt.Fprintf(buf, "%x\r\n%s\r\n", len(part), part); err != nil {
				return
			}
			if err := buf.Flush(); err != nil {
				return
			}
		}
		_, _ = buf.WriteString("0\r\n\r\n")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// crlfUpstream 用 CRLF 行尾发同一串事件：SSE 规范两种行尾都合法，
// 解析时不能把 \r 留在 data 里当成 JSON 的一部分。
func crlfUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.ReplaceAll(payload, "\n", "\r\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStreamFramingShapesKeepIncrementsAndEnding 覆盖三种上游分帧形状下的跨协议转换：
// 事件序列、正文增量与结尾都与「一次性写完」时一致，并且完整流不会被追加错误帧。
func TestStreamFramingShapesKeepIncrementsAndEnding(t *testing.T) {
	cases := []struct {
		name   string
		server func(t *testing.T, payload string) *httptest.Server
	}{
		{"上游逐字节写", byteAtATimeUpstream},
		{"上游手写分块编码", chunkedUpstream},
		{"上游用 CRLF 行尾", crlfUpstream},
	}

	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := c.server(t, chatSSE)
			f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-framing", []string{"m1"}, 100)
			srv := f.server(t)

			status, body, ctype := streamFrom(t, srv, gateway.ProtocolAnthropic, "m1")
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，期望 200", status)
			}
			if !strings.HasPrefix(ctype, "text/event-stream") {
				t.Fatalf("Content-Type = %q", ctype)
			}

			events := anthropicEvents(t, body)
			if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
				t.Fatalf("事件序列 = %v，期望 %v", events, wantEvents)
			}
			// 增量按帧边界切开，不因上游的写法和读到的字节块而合并或拆散。
			if got := strings.Join(textDeltas(t, body), "|"); got != "你好|世界" {
				t.Fatalf("正文增量 = %q，期望 %q", got, "你好|世界")
			}
			// 收尾帧到齐的完整流不补错误帧。
			if strings.Contains(body, "event: error") {
				t.Fatalf("完整流不该出现错误帧：\n%s", body)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || !recs[0].OK || recs[0].Error != "" {
				t.Fatalf("请求记录 = %+v，期望一行 ok=1 且无错误摘要", recs)
			}
		})
	}
}

// passthroughCRLFUpstream 用 CRLF 行尾发 anthropic 流并停在收尾帧之前，
// 用来钉住直通路径在 CRLF 断流时补出的收尾帧仍是合法帧。
func passthroughCRLFUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("假上游不支持 hijack")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack 连接: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
		_, _ = buf.WriteString(strings.ReplaceAll(payload, "\n", "\r\n"))
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPassthroughCRLFDisconnectKeepsLegalEnding 覆盖直通路径在 CRLF 行尾下的断流收尾：
// 已转发的字节一个不改，追加的 message_delta 与 message_stop 仍各自以空行结尾，
// CRLF 之后不会留下把两帧粘在一起的半行。
func TestPassthroughCRLFDisconnectKeepsLegalEnding(t *testing.T) {
	full := streamBodies[translate.ProtocolAnthropic]
	idx := strings.Index(full, "event: message_stop")
	if idx <= 0 {
		t.Fatalf("测试用例写错了：完整流里没有 message_stop")
	}
	written := full[:idx]

	f := newFixture(t)
	up := passthroughCRLFUpstream(t, written)
	f.add(t, "c1", gateway.ProtocolAnthropic, up.URL, "sk-crlf", []string{"m1"}, 100)
	srv := f.server(t)

	status, body, _ := streamFrom(t, srv, gateway.ProtocolAnthropic, "m1")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", status)
	}
	// 已转发的前缀原样（CRLF 保持 CRLF），追加的帧只往后接。
	if !strings.HasPrefix(body, strings.ReplaceAll(written, "\n", "\r\n")) {
		t.Fatalf("直通改动了已写出的字节：\n%s", tail(body, 200))
	}
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("CRLF 断流之后应有一个 error 事件：\n%s", tail(body, 200))
	}
	if want := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"; !strings.HasSuffix(body, want) {
		t.Fatalf("流应以 message_stop 结尾，实际结尾 %q", tail(body, 80))
	}
	// 追加部分本身是合法 SSE：第二帧的数据行前必须是空行，而不是接在 CRLF 后面。
	if !bytes.Contains([]byte(body), []byte("\n\nevent: message_delta\n")) {
		t.Fatalf("追加的 message_delta 前面缺空行：\n%s", tail(body, 200))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=0", recs)
	}
}
