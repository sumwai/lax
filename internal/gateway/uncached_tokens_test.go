package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"lax/internal/store"
	"lax/internal/translate"
)

// 三条出站协议的用量报文：都在输入里报了 100 个 token，其中 60 个命中缓存。
// anthropic 的 100 已经不含缓存（另外单列 cache_read_input_tokens 与 cache_creation_input_tokens），
// chat 的 prompt_tokens 与 gemini 的 promptTokenCount 都包含命中缓存的那 60 个。
const (
	anthropicUsageReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m1",` +
		`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":60,"cache_creation_input_tokens":20}}`
	chatUsageReply = `{"id":"chatcmpl-1","object":"chat.completion","model":"m1",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":60}}}`
	geminiUsageReply = `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10,"totalTokenCount":110,"cachedContentTokenCount":60}}`
)

// TestRecordedUncachedInTokensByOutbound 钉住入账时的输入 token 归一化（DESIGN 5.5）：
// 出站是 anthropic 时 in_tokens_uncached 等于上游的 input_tokens，出站是 chat 或 gemini 时
// 要减掉 cache_read；in_tokens 本身仍是上游原值，log tail 与 usage 的输入列因此不变。
func TestRecordedUncachedInTokensByOutbound(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		route    string
		reply    string
		want     int64
	}{
		{"anthropic 不减", translate.ProtocolAnthropic, "/v1/messages", anthropicUsageReply, 100},
		{"openai-chat 减掉命中缓存的部分", translate.ProtocolChat, "/v1/chat/completions", chatUsageReply, 40},
		{"gemini 减掉命中缓存的部分", translate.ProtocolGemini, "/v1beta/models/{model}:generateContent", geminiUsageReply, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.reply))
			}))
			t.Cleanup(up.Close)

			// 路由按协议声明：地址与路径都走厂商表，用例只关心用量怎么记。
			if err := f.st.CreateProvider(store.Provider{Name: "p" + c.protocol, BaseURL: up.URL},
				[]store.ProviderRoute{{Protocol: c.protocol, Path: c.route}}); err != nil {
				t.Fatalf("加厂商: %v", err)
			}
			f.addAs(t, "acc", "p"+c.protocol, c.protocol, up.URL, "sk-test", []string{"m1"})
			srv := f.server(t)

			status, body := postTo(t, srv, `{"model":"m1","max_tokens":16,`+
				`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolChat)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, body)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("请求记录数 = %d，期望 1", len(recs))
			}
			rec := recs[0]
			if rec.Outbound != c.protocol {
				t.Fatalf("记录的出站协议 = %q，期望 %q", rec.Outbound, c.protocol)
			}
			if rec.InTokens != 100 {
				t.Fatalf("记录的输入 token = %d，期望上游原值 100（展示不变）", rec.InTokens)
			}
			if rec.InTokensUncached != c.want {
				t.Fatalf("记录的未命中输入 token = %d，期望 %d", rec.InTokensUncached, c.want)
			}
			// anthropic 的报文另外单列缓存写，另两条协议没有这个字段。
			wantCacheWrite := int64(0)
			if c.protocol == translate.ProtocolAnthropic {
				wantCacheWrite = 20
			}
			if rec.CacheRead != 60 || rec.CacheWrite != wantCacheWrite {
				t.Fatalf("记录的缓存读 = %d，缓存写 = %d，期望 60 / %d",
					rec.CacheRead, rec.CacheWrite, wantCacheWrite)
			}
		})
	}
}

// TestRecordedUncachedInTokensFloorsAtZero 钉住「减到 0 为止」：上游报出来的缓存比输入还多时
// 记 0，不记负数——负的输入 token 会让估算花费变成负数。
func TestRecordedUncachedInTokensFloorsAtZero(t *testing.T) {
	f := newFixture(t)
	const reply = `{"id":"chatcmpl-1","object":"chat.completion","model":"m1",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":50}}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(up.Close)

	if err := f.st.CreateProvider(store.Provider{Name: "pchat", BaseURL: up.URL},
		[]store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "acc", "pchat", translate.ProtocolChat, up.URL, "sk-test", []string{"m1"})
	srv := f.server(t)

	if status, body := postTo(t, srv, `{"model":"m1","max_tokens":16,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d，期望 1", len(recs))
	}
	if recs[0].InTokens != 10 || recs[0].InTokensUncached != 0 {
		t.Fatalf("记录 = %d / 归一化 %d，期望 10 / 0", recs[0].InTokens, recs[0].InTokensUncached)
	}
}
