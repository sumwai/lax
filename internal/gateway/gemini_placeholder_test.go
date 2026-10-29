package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// placeholderUpstream 记录每一次请求的 RequestURI，并按有没有 alt=sse 回对应的报文：
// 带上 alt=sse 的请求是流式，回 gemini 的 SSE；其余回非流式的 gemini 响应。
type placeholderUpstream struct {
	url  string
	uris []string
}

func newPlaceholderUpstream(t *testing.T) *placeholderUpstream {
	t.Helper()
	u := &placeholderUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.uris = append(u.uris, r.URL.RequestURI())
		if r.URL.Query().Get("alt") == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, geminiStreamBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiReply)
	}))
	t.Cleanup(srv.Close)
	u.url = srv.URL
	return u
}

// lastURI 是最近一次请求的 RequestURI；一次都不该多打，所以调用方通常配合「正好一次」断言。
func (u *placeholderUpstream) lastURI() string {
	if len(u.uris) == 0 {
		return ""
	}
	return u.uris[len(u.uris)-1]
}

// TestGeminiPlaceholderEdgeCases 钉住 {model} 的替换边界：只替换这一个字面量、出现几次换几次，
// 替换值是 url.PathEscape(模型名)，路径里的 `:` 与查询串一个字不动；没有占位符的路由一个字节都不改。
//
// 含冒号的模型名有点特殊：PathEscape 不转义 `:`，所以 `a:b` 落成 `a:b`，与动作后缀同形。
// 这里钉住的是「网关不额外转义、也不改写」这条事实，上游怎么切这一段由它自己决定；
// 换成转义 `:` 的写法会让这条用例立刻失败，配置里出现这种模型名时才有人去查。
func TestGeminiPlaceholderEdgeCases(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		route      string
		wantURI    string
		wantStream string
	}{
		{
			name:       "模型名含斜杠",
			model:      "org/gemini",
			route:      geminiRoute,
			wantURI:    "/v1beta/models/org%2Fgemini:generateContent",
			wantStream: "/v1beta/models/org%2Fgemini:streamGenerateContent?alt=sse",
		},
		{
			name:       "模型名含空格",
			model:      "gemini 2.5 pro",
			route:      geminiRoute,
			wantURI:    "/v1beta/models/gemini%202.5%20pro:generateContent",
			wantStream: "/v1beta/models/gemini%202.5%20pro:streamGenerateContent?alt=sse",
		},
		{
			name:       "模型名非 ASCII",
			model:      "模型-1",
			route:      geminiRoute,
			wantURI:    "/v1beta/models/%E6%A8%A1%E5%9E%8B-1:generateContent",
			wantStream: "/v1beta/models/%E6%A8%A1%E5%9E%8B-1:streamGenerateContent?alt=sse",
		},
		{
			// `:` 不在 PathEscape 的转义集里，因此模型名里的冒号原样落在路径上。
			name:       "模型名含冒号",
			model:      "a:b",
			route:      geminiRoute,
			wantURI:    "/v1beta/models/a:b:generateContent",
			wantStream: "/v1beta/models/a:b:streamGenerateContent?alt=sse",
		},
		{
			// 模型名里的 `%` 要被转义成 %25，不能被当成上游的转义序列解一次。
			name:       "模型名含百分号",
			model:      "m%2F1",
			route:      geminiRoute,
			wantURI:    "/v1beta/models/m%252F1:generateContent",
			wantStream: "/v1beta/models/m%252F1:streamGenerateContent?alt=sse",
		},
		{
			name:       "占位符出现两次",
			model:      "m1",
			route:      "/proxy/{model}/v1beta/models/{model}:generateContent",
			wantURI:    "/proxy/m1/v1beta/models/m1:generateContent",
			wantStream: "/proxy/m1/v1beta/models/m1:streamGenerateContent?alt=sse",
		},
		{
			// 没有占位符的 gemini 路由：路径一个字节都不改，流式只加 alt=sse。
			name:       "路径里没有占位符",
			model:      "m1",
			route:      "/gem/raw",
			wantURI:    "/gem/raw",
			wantStream: "/gem/raw?alt=sse",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				label := "非流式"
				want := c.wantURI
				body := `{"model":"` + c.model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
				if stream {
					label = "流式"
					want = c.wantStream
					body = `{"model":"` + c.model + `","max_tokens":16,"stream":true,` +
						`"messages":[{"role":"user","content":"hi"}]}`
				}

				f := newFixture(t)
				up := newPlaceholderUpstream(t)
				if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.url},
					[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: c.route}}); err != nil {
					t.Fatalf("加厂商: %v", err)
				}
				f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.url, "sk-gemini-placeholder",
					[]string{c.model})
				srv := f.server(t)

				status, resp := postStreamTo(t, srv, body, translate.ProtocolChat)
				if status != http.StatusOK {
					t.Fatalf("%s：状态 = %d，响应 %s", label, status, resp)
				}
				if len(up.uris) != 1 {
					t.Fatalf("%s：上游调用 = %v，期望正好一次", label, up.uris)
				}
				if got := up.lastURI(); got != want {
					t.Fatalf("%s：上游 RequestURI = %q，期望 %q", label, got, want)
				}
			}
		})
	}
}
