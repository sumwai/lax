package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/translate"
)

// 这个文件钉住流内错误帧的脱敏：上游自己发来的错误帧正文会写进 requests.error，也会随错误帧
// 发给客户端，必须与非 2xx 路径一样过一遍 scrubSecret。gemini 的
// {"error":{…}} 是流式解码里新增的入口，上游把凭据回显在 message 里时不能落库、不能出网关。

// TestStreamErrorFrameScrubsSecret 让 gemini 上游在流里发一帧带凭据的错误体，三条入站协议
// （anthropic、chat、responses）各收一次：客户端拿到的错误帧与 requests 里那一行都不能有明文。
func TestStreamErrorFrameScrubsSecret(t *testing.T) {
	const key = "sk-leak-in-stream"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"error":{"code":401,"message":"API key `+key+` is invalid"}}`+"\n\n")
	}))
	t.Cleanup(up.Close)

	for _, inbound := range []string{
		translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses,
	} {
		t.Run(inbound, func(t *testing.T) {
			f := newFixture(t)
			f.add(t, "g1", translate.ProtocolGemini, up.URL, key, []string{"m1"}, 100)
			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			// 入站的流式请求体：三条协议各自把 stream 写在报文里，模型名统一是 m1。
			body := strings.Replace(requestBodies[inbound], `"model":"m1"`, `"model":"m1","stream":true`, 1)
			status, got := postTo(t, srv, body, inbound)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, got)
			}
			if strings.Contains(got, key) {
				t.Fatalf("客户端拿到的错误帧泄漏了凭据: %s", tail(got, 400))
			}
			if !strings.Contains(got, "***") {
				t.Fatalf("错误帧应当保留上游原话并替换凭据: %s", tail(got, 400))
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("请求记录行数 = %d，期望 1", len(recs))
			}
			if recs[0].OK {
				t.Fatalf("上游发错误帧这一次应当记成失败: %+v", recs[0])
			}
			if strings.Contains(recs[0].Error, key) {
				t.Fatalf("请求记录泄漏了凭据: %q", recs[0].Error)
			}
			if !strings.Contains(recs[0].Error, "上游流式响应失败") || !strings.Contains(recs[0].Error, "***") {
				t.Fatalf("请求记录的错误摘要 = %q，期望带上游原话（已脱敏）", recs[0].Error)
			}
			if leaked := logs(); strings.Contains(leaked, key) {
				t.Fatalf("日志泄漏了凭据: %s", leaked)
			}
		})
	}
}
