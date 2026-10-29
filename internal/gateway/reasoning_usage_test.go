package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/translate"
)

// 这个文件钉住思考 token 的记账链路（DESIGN 5.5）：scanner 与解码器读到的 reasoning 与 total
// 要一路传到 requests 的两列，判定只在 store.ReasoningExtra 里做一次。
//
// 报文取真实中转的形状：prompt 7、completion 1、total 132、reasoning 124——思考既不在
// completion 里，也不在 total 之外，只按 completion 计价会漏掉九成以上的输出花费。

// reasoningPassthroughReply 是中转上报的非流式 chat 响应。
const reasoningPassthroughReply = `{"id":"chatcmpl-1","object":"chat.completion","model":"m1",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":132,` +
	`"completion_tokens_details":{"reasoning_tokens":124}}}`

// reasoningStreamReply 是同一批数字的流式形状：用量在收尾前的那个 chunk 里。
const reasoningStreamReply = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m1",` +
	`"choices":[{"index":0,"delta":{"role":"assistant","content":"pong"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":132,` +
	`"completion_tokens_details":{"reasoning_tokens":124}}}` + "\n\n" +
	`data: [DONE]` + "\n\n"

// TestAccountingRecordsReasoningTokens 让 chat 上游按中转的形状上报思考 token，三种记账路径
// （非流式直通、流式直通、跨协议转换）都要在 requests 里留下同一个数：reasoning_tokens 是原值，
// reasoning_tokens_extra 是判定为「输出之外的一项」的那部分，两者这一例都是 124。
func TestAccountingRecordsReasoningTokens(t *testing.T) {
	const secret = "sk-secret-reasoning-account"

	cases := []struct {
		name    string
		inbound string
		stream  bool
	}{
		{"非流式直通", translate.ProtocolChat, false},
		{"流式直通", translate.ProtocolChat, true},
		{"跨协议转换", translate.ProtocolAnthropic, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, reasoningStreamReply)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, reasoningPassthroughReply)
			}))
			t.Cleanup(up.Close)

			f.add(t, "relay", translate.ProtocolChat, up.URL, secret, []string{"m1"}, 100)
			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			body := requestBodies[c.inbound]
			if c.stream {
				body = strings.Replace(body, `"model":"m1"`, `"model":"m1","stream":true`, 1)
			}
			status, got := postTo(t, srv, body, c.inbound)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, got)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("请求记录行数 = %d，期望 1", len(recs))
			}
			rec := recs[0]
			if rec.ReasoningTokens != 124 {
				t.Fatalf("reasoning_tokens = %d，期望 124（上游报的原值）", rec.ReasoningTokens)
			}
			if rec.ReasoningTokensExtra != 124 {
				t.Fatalf("reasoning_tokens_extra = %d，期望 124（total 132 > prompt 7 + completion 1，思考是独立一项）",
					rec.ReasoningTokensExtra)
			}
			// 思考不并进输出：输出列仍是上游报的 1。
			if rec.OutTokens != 1 || rec.InTokens != 7 {
				t.Fatalf("输入输出 = %d / %d，期望 7 / 1", rec.InTokens, rec.OutTokens)
			}

			// 记账新路径同样不许把凭据带进日志或记录：这是每次新增入账字段都要走一遍的检查。
			if leaked := logs(); strings.Contains(leaked, secret) {
				t.Fatalf("日志泄漏了凭据: %s", leaked)
			}
			if strings.Contains(rec.Error, secret) {
				t.Fatalf("请求记录泄漏了凭据: %q", rec.Error)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("下发给客户端的响应泄漏了凭据: %s", tail(got, 300))
			}
		})
	}
}

// TestAccountingTreatsSubsetReasoningAsOutput 钉住子集场景：total = prompt + completion 时
// 思考已经在输出里，reasoning_tokens 照原值记，reasoning_tokens_extra 记 0，不再重复计价。
func TestAccountingTreatsSubsetReasoningAsOutput(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","model":"m1",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":7,"completion_tokens":125,"total_tokens":132,`+
			`"completion_tokens_details":{"reasoning_tokens":124}}}`)
	}))
	t.Cleanup(up.Close)

	f.add(t, "official", translate.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	if status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录行数 = %d，期望 1", len(recs))
	}
	if recs[0].ReasoningTokens != 124 {
		t.Fatalf("reasoning_tokens = %d，期望原值 124", recs[0].ReasoningTokens)
	}
	if recs[0].ReasoningTokensExtra != 0 {
		t.Fatalf("子集场景的 reasoning_tokens_extra = %d，期望 0（已经在输出里）", recs[0].ReasoningTokensExtra)
	}
	if recs[0].OutTokens != 125 {
		t.Fatalf("输出 token = %d，期望上游原值 125", recs[0].OutTokens)
	}
}
