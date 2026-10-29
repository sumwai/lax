package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// 这个文件钉住 gemini 流式端点的后缀改写只认路径末尾（查询串之前）：url.PathEscape 不转义
// `:`，模型名里可以自带 :generateContent；自定义路由本身没有这个动作后缀时，它在路径中间，
// 改写它会让同一个模型的流式与非流式打到两个不同地址（DESIGN 5.9）。

// TestGeminiStreamEndpointRewritesOnlyPathTail 用两个形状各跑一遍流式与非流式：
// 自定义路由没有动作后缀（模型名里的 :generateContent 必须原样留在路径中间），
// 以及动作后缀确实在路径末尾（只换最后那一段，模型名里的不动）。
func TestGeminiStreamEndpointRewritesOnlyPathTail(t *testing.T) {
	const model = "m:generateContent"
	cases := []struct {
		name       string
		route      string
		wantPlain  string
		wantStream string
	}{
		{
			name:       "自定义路由没有动作后缀",
			route:      "/custom/models/{model}/invoke",
			wantPlain:  "/custom/models/" + model + "/invoke",
			wantStream: "/custom/models/" + model + "/invoke?alt=sse",
		},
		{
			name:       "动作后缀在路径末尾",
			route:      "/v1beta/models/{model}:generateContent",
			wantPlain:  "/v1beta/models/" + model + ":generateContent",
			wantStream: "/v1beta/models/" + model + ":streamGenerateContent?alt=sse",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)

			var uris []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uris = append(uris, r.URL.RequestURI())
				if r.URL.Query().Get("alt") == "sse" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, geminiStreamBody)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, geminiReply)
			}))
			t.Cleanup(up.Close)

			if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.URL},
				[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: c.route}}); err != nil {
				t.Fatalf("加厂商: %v", err)
			}
			f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini-tail", []string{model})
			gs, _ := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			plain := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
			if status, body := postTo(t, srv, plain, translate.ProtocolChat); status != http.StatusOK {
				t.Fatalf("非流式状态 = %d，响应 %s", status, body)
			}
			stream := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			if status, body := postTo(t, srv, stream, translate.ProtocolChat); status != http.StatusOK {
				t.Fatalf("流式状态 = %d，响应 %s", status, body)
			}

			if len(uris) != 2 {
				t.Fatalf("上游调用数 = %d，期望 2：%v", len(uris), uris)
			}
			if uris[0] != c.wantPlain {
				t.Fatalf("非流式 RequestURI = %q，期望 %q", uris[0], c.wantPlain)
			}
			if uris[1] != c.wantStream {
				t.Fatalf("流式 RequestURI = %q，期望 %q", uris[1], c.wantStream)
			}
		})
	}
}
