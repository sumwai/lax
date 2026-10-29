package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"switchboard/internal/gateway"
	"switchboard/internal/translate"
)

// 这个文件钉住 gemini 上游的流式分帧边界。gemini 的帧没有事件名，一帧就是一个
// GenerateContentResponse：断流的判据是 candidates[].finishReason，闪断的形态与另外三条协议一样，
// 只是补出来的收尾帧还是 gemini 形状。三条用例各切一个位置：收尾帧之前、事件中间、多字节字符中间。

// geminiSSEPrefix 是「两条完整正文帧」的 gemini 流：后面接什么由各用例自己拼。
const geminiSSEPrefix = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"世界"}]},"index":0}]}

`

// geminiSSEFinish 是收尾帧：带 finishReason 与用量。
const geminiSSEFinish = `data: {"candidates":[{"content":{"role":"model","parts":[]},"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}

`

// TestGeminiPassthroughDisconnectKeepsGeminiTail 钉住 gemini 直通断流的收尾：已转发的前缀字节
// 一个不改，后面接一帧 gemini 形状的错误帧，再接一帧带 finishReason 与 usageMetadata 的候选
// （gemini 没有单独的结束事件，收尾就是这一帧）；这一笔记成 ok=0。
func TestGeminiPassthroughDisconnectKeepsGeminiTail(t *testing.T) {
	f := newFixture(t)
	written := geminiSSEPrefix
	up := truncatedSSEUpstream(t, written+geminiSSEFinish, written)
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini-cut",
		[]string{"gemini-2.5-flash"})
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（流式已在写响应头之后），响应 %s", status, body)
	}

	// 已写出的字节不动：直通的内容原样是上游的前缀，收尾帧只往后接。
	if !strings.HasPrefix(body, written) {
		t.Fatalf("直通改动了已写出的字节：\n得到 %q\n期望前缀 %q", tail(body, 120), tail(written, 120))
	}
	want := []string{
		`text "你好"`,
		`text "世界"`,
		"error",
		// 用量读不到（这次没有转发过 usageMetadata 帧）就是 0，不能编造一个数字。
		"finish STOP 0/0 total=0",
	}
	if got := geminiStreamSignatures(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini 客户端的帧序列:\n得到 %v\n期望 %v", got, want)
	}
	if !strings.Contains(body, "上游流式响应中断") {
		t.Fatalf("错误帧应当说明上游中断:\n%s", tail(body, 200))
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("流必须以空行结尾，实际结尾 %q", tail(body, 40))
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 ok=0", recs)
	}
	if !strings.Contains(recs[0].Error, "上游流式响应中断") {
		t.Fatalf("请求记录的 error = %q，期望写明上游中断", recs[0].Error)
	}
	if leaked := logs(); strings.Contains(leaked, "sk-gemini-cut") {
		t.Fatalf("网关日志泄漏了凭据：%s", leaked)
	}
}

// TestGeminiUpstreamCutMidFrameEndsAnthropicStream 覆盖 gemini 上游在事件中间、在多字节字符中间
// 断开时转给 anthropic 客户端的结果：断点前的完整正文原样到达，客户端收到的整段字节仍是合法
// UTF-8，末尾是错误事件加 message_delta、message_stop，这一笔记成 ok=0。
func TestGeminiUpstreamCutMidFrameEndsAnthropicStream(t *testing.T) {
	// 第一帧完整（你好），第二帧的 JSON 断在字符串里；「世」是 e4 b8 96，
	// 多字节那一档只发它的前两个字节。
	first := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]},"index":0}]}` + "\n\n"
	midEvent := first + `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"世`
	midRune := first + `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"` + "世"[:2]

	cases := []struct{ name, prefix string }{
		{"切在事件中间", midEvent},
		{"切在多字节字符中间", midRune},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.prefix == midEvent && !strings.HasSuffix(c.prefix, `"世`) {
				t.Fatalf("测试用例写错了：切点应落在第二帧的 JSON 中间")
			}
			if c.prefix == midRune && utf8.ValidString(c.prefix) {
				t.Fatalf("测试用例写错了：切点应落在多字节字符中间: %q", tail(c.prefix, 20))
			}

			f := newFixture(t)
			up := closedSSEUpstream(t, c.prefix)
			f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini-mid",
				[]string{"m1"})
			srv := f.server(t)

			status, body, _ := streamFrom(t, srv, translate.ProtocolAnthropic, "m1")
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，期望 200", status)
			}
			if !utf8.ValidString(body) {
				t.Fatalf("客户端收到的字节里有被截断的多字节字符: %q", tail(body, 80))
			}
			// 断点之前那条完整帧的中文正文必须原样到达，且没有被拆成两段。
			if got := textDeltas(t, body); len(got) != 1 || got[0] != "你好" {
				t.Fatalf("正文增量 = %v，期望只有断点前的 [你好]", got)
			}
			events := anthropicEvents(t, body)
			if got := strings.Join(events, ","); !strings.HasSuffix(got, "error,content_block_stop,message_delta,message_stop") {
				t.Fatalf("事件序列的结尾 = %q，期望 error 之后收束到 message_stop", got)
			}
			if kind, message := errorFrame(t, body); kind != "api_error" || !strings.Contains(message, "上游流式响应中断") {
				t.Fatalf("错误帧 = %q/%q，期望 api_error 且说明上游中断", kind, message)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || recs[0].OK {
				t.Fatalf("请求记录 = %+v，期望一行且 ok=0", recs)
			}
			if recs[0].Inbound != translate.ProtocolAnthropic || recs[0].Outbound != translate.ProtocolGemini {
				t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
			}
		})
	}
}
