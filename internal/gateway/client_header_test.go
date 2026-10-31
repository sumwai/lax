package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// postWithHeaders 往入站路径发一条带自定义请求头的 POST，返回状态与响应体。
// clientHost 非空时用它当 Host（Host 不是普通头，只能走 Request.Host）。
func postWithHeaders(t *testing.T, srv *httptest.Server, path, body string,
	headers http.Header, clientHost string) (int, string) {

	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造入站请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if clientHost != "" {
		req.Host = clientHost
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return resp.StatusCode, string(b)
}

// anthropicMessagesBody 是一条最小的 anthropic 入站请求体。
const anthropicMessagesBody = `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// TestClientHeadersAnthropicPassthrough 钉住 anthropic 同协议透传：anthropic-beta 可以重复出现
// （同时开多个 beta），按出现顺序全部带上；客户端给了 anthropic-version 就用客户端的。
func TestClientHeadersAnthropicPassthrough(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/messages": probeAnthropicOK,
	})
	f.addAs(t, "a1", "atest", gateway.ProtocolAnthropic, up.srv.URL, "sk-ant", []string{"claude-x"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	headers := http.Header{}
	headers.Add("anthropic-beta", "context-1m-2025-08-07")
	headers.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
	headers.Set("anthropic-version", "2024-10-22")
	status, body := postWithHeaders(t, srv, "/v1/messages", anthropicMessagesBody, headers, "")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	h := up.mustCalls(t, 1)[0].Header
	if got := h.Values("anthropic-beta"); len(got) != 2 ||
		got[0] != "context-1m-2025-08-07" || got[1] != "interleaved-thinking-2025-05-14" {
		t.Fatalf("anthropic-beta = %q，期望两个 beta 按出现顺序都带上", got)
	}
	if got := h.Get("anthropic-version"); got != "2024-10-22" {
		t.Fatalf("anthropic-version = %q，期望客户端给的值", got)
	}
	if got := h.Get("x-api-key"); got != "sk-ant" {
		t.Fatalf("x-api-key = %q，期望账号凭据", got)
	}
}

// TestClientHeadersAnthropicVersionDefault 确认客户端没给 anthropic-version 时仍是 setAuth 的默认值：
// 白名单头「取不到就不设」，不改默认值。
func TestClientHeadersAnthropicVersionDefault(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/messages": probeAnthropicOK,
	})
	f.addAs(t, "a1", "atest", gateway.ProtocolAnthropic, up.srv.URL, "sk-ant", []string{"claude-x"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postWithHeaders(t, srv, "/v1/messages", anthropicMessagesBody, nil, "")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	h := up.mustCalls(t, 1)[0].Header
	if got := h.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q，期望 setAuth 的默认值 2023-06-01", got)
	}
	if _, ok := h["Anthropic-Beta"]; ok {
		t.Fatalf("客户端没给 anthropic-beta 时不该凭空发这个头：%q", h.Get("anthropic-beta"))
	}
}

// TestClientHeadersOpenAIPassthrough 钉住两条 openai 协议各透传三个头：openai-beta、
// openai-organization、openai-project。
func TestClientHeadersOpenAIPassthrough(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		path     string
		body     string
		reply    string
	}{
		{
			name:     "chat",
			protocol: gateway.ProtocolChat,
			path:     "/v1/chat/completions",
			body:     `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			reply:    chatCompletion,
		},
		{
			name:     "responses",
			protocol: gateway.ProtocolResponses,
			path:     "/v1/responses",
			body:     `{"model":"o3-mini","max_output_tokens":16,"input":"hi"}`,
			reply:    probeResponsesOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			up := newHeaderUpstream(t, map[string]string{
				"POST " + tc.path: tc.reply,
			})
			f.addAs(t, "a1", "otest", tc.protocol, up.srv.URL, "sk-openai", []string{"gpt-x", "o3-mini"})
			gs, _ := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			headers := http.Header{}
			headers.Set("openai-beta", "assistants=v2")
			headers.Set("openai-organization", "org-42")
			headers.Set("openai-project", "proj-7")
			status, body := postWithHeaders(t, srv, tc.path, tc.body, headers, "")
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, body)
			}

			h := up.mustCalls(t, 1)[0].Header
			for name, want := range map[string]string{
				"openai-beta":         "assistants=v2",
				"openai-organization": "org-42",
				"openai-project":      "proj-7",
			} {
				if got := h.Get(name); got != want {
					t.Fatalf("%s = %q，期望 %q", name, got, want)
				}
			}
			if got := h.Get("Authorization"); got != "Bearer sk-openai" {
				t.Fatalf("Authorization = %q，期望账号凭据", got)
			}
		})
	}
}

// TestClientHeadersCrossProtocolDropped 确认入站协议与有效出站协议不同时一个白名单头都不透传：
// anthropic-beta 发给 chat 上游只会换回 400。
func TestClientHeadersCrossProtocolDropped(t *testing.T) {
	t.Run("chat 入站到 anthropic 出站", func(t *testing.T) {
		f := newFixture(t)
		up := newHeaderUpstream(t, map[string]string{
			"POST /v1/messages": probeAnthropicOK,
		})
		f.addAs(t, "a1", "atest", gateway.ProtocolAnthropic, up.srv.URL, "sk-ant", []string{"gpt-x"})
		gs, _ := newGatewayWith(t, f, gateway.Options{})
		srv := httptest.NewServer(gs.Handler())
		t.Cleanup(srv.Close)

		headers := http.Header{}
		headers.Set("openai-beta", "assistants=v2")
		headers.Set("openai-organization", "org-42")
		headers.Set("openai-project", "proj-7")
		headers.Set("anthropic-beta", "context-1m-2025-08-07")
		body := `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		status, resp := postWithHeaders(t, srv, "/v1/chat/completions", body, headers, "")
		if status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, resp)
		}

		h := up.mustCalls(t, 1)[0].Header
		for _, name := range []string{"openai-beta", "openai-organization", "openai-project", "anthropic-beta"} {
			if got := h.Get(name); got != "" {
				t.Fatalf("跨协议时 %s 不该透传，实际 %q", name, got)
			}
		}
	})

	t.Run("anthropic 入站到 chat 出站", func(t *testing.T) {
		f := newFixture(t)
		up := newHeaderUpstream(t, map[string]string{
			"POST /v1/chat/completions": chatCompletion,
		})
		f.addAs(t, "a1", "otest", gateway.ProtocolChat, up.srv.URL, "sk-openai", []string{"claude-x"})
		gs, _ := newGatewayWith(t, f, gateway.Options{})
		srv := httptest.NewServer(gs.Handler())
		t.Cleanup(srv.Close)

		headers := http.Header{}
		headers.Set("anthropic-beta", "context-1m-2025-08-07")
		headers.Set("anthropic-version", "2024-10-22")
		status, resp := postWithHeaders(t, srv, "/v1/messages", anthropicMessagesBody, headers, "")
		if status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, resp)
		}

		h := up.mustCalls(t, 1)[0].Header
		for _, name := range []string{"anthropic-beta", "anthropic-version"} {
			if got := h.Get(name); got != "" {
				t.Fatalf("跨协议时 %s 不该透传，实际 %q", name, got)
			}
		}
	})
}

// TestClientHeadersGeminiPassthroughNone 确认 gemini 的白名单是空的：gemini 入站到 gemini 出站
// （同协议直通）时，客户端带来的任何白名单头都不上去，凭据仍是账号的 x-goog-api-key。
func TestClientHeadersGeminiPassthroughNone(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1beta/models/gemini-2.5-flash:generateContent": geminiReply,
	})
	f.providerWithRoutes(t, "gtest", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolGemini, Path: geminiRoute})
	f.addAs(t, "g1", "gtest", gateway.ProtocolGemini, up.srv.URL, "sk-gemini", []string{"gemini-2.5-flash"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	headers := http.Header{}
	headers.Set("anthropic-beta", "context-1m-2025-08-07")
	headers.Set("openai-beta", "assistants=v2")
	headers.Set("x-goog-api-key", "client-key")
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	status, resp := postWithHeaders(t, srv, geminiInboundPath, body, headers, "")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	h := up.mustCalls(t, 1)[0].Header
	for _, name := range []string{"anthropic-beta", "anthropic-version", "openai-beta",
		"openai-organization", "openai-project"} {
		if got := h.Get(name); got != "" {
			t.Fatalf("gemini 的白名单是空的，%s 不该透传，实际 %q", name, got)
		}
	}
	if got := h.Get("x-goog-api-key"); got != "sk-gemini" {
		t.Fatalf("x-goog-api-key = %q，期望账号凭据而不是客户端的", got)
	}
}

// TestClientCredentialsAndHopByHopNotForwarded 钉住「永远不透传」名单：客户端的凭据、
// Cookie、Host 与 Content-Length 都不带上去，上游收到的仍是账号凭据与它自己的 Host、
// Content-Length。
func TestClientCredentialsAndHopByHopNotForwarded(t *testing.T) {
	f := newFixture(t)
	var upstreamBody string
	var upstreamReq *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		upstreamReq = r.Clone(t.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, probeAnthropicOK)
	}))
	t.Cleanup(up.Close)

	f.addAs(t, "a1", "atest", gateway.ProtocolAnthropic, up.URL, "sk-account", []string{"claude-x"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer client-token")
	headers.Set("x-api-key", "client-api-key")
	headers.Set("x-goog-api-key", "client-goog-key")
	headers.Set("Cookie", "session=client-cookie")
	headers.Set("x-lax-session", "sess-client")
	// 入站请求体故意写松（带空格与换行）：上游请求体是 patchModel 重新编的紧凑 JSON，
	// 长度与入站不同，Content-Length 若照抄客户端值就会对不上。
	inbound := "{\n  \"model\": \"claude-x\",\n  \"max_tokens\": 16,\n  \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}]\n}"
	status, body := postWithHeaders(t, srv, "/v1/messages", inbound, headers, "client.example")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if upstreamReq == nil {
		t.Fatal("上游没收到请求")
	}

	h := upstreamReq.Header
	if got := h.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q，客户端凭据不该透传", got)
	}
	if got := h.Get("x-api-key"); got != "sk-account" {
		t.Fatalf("x-api-key = %q，期望账号凭据", got)
	}
	if got := h.Get("x-goog-api-key"); got != "" {
		t.Fatalf("x-goog-api-key = %q，客户端凭据不该透传", got)
	}
	if got := h.Get("Cookie"); got != "" {
		t.Fatalf("Cookie = %q，客户端 Cookie 不该透传", got)
	}
	if got := h.Get("x-lax-session"); got != "" {
		t.Fatalf("x-lax-session = %q，本网关自己的会话键不该透传", got)
	}
	if upstreamReq.Host != strings.TrimPrefix(up.URL, "http://") {
		t.Fatalf("上游 Host = %q，期望上游自己的地址（客户端给的是 client.example）", upstreamReq.Host)
	}
	if upstreamReq.ContentLength != int64(len(upstreamBody)) {
		t.Fatalf("上游 Content-Length = %d，期望等于实际请求体长度 %d",
			upstreamReq.ContentLength, len(upstreamBody))
	}
	if upstreamReq.ContentLength == int64(len(inbound)) {
		t.Fatalf("上游 Content-Length 与入站请求体长度相同（%d），像是照抄了客户端的值",
			upstreamReq.ContentLength)
	}
}

// TestClientHeaderDeclarationOverridesClient 钉住优先级由顺序决定：厂商附加头声明排在客户端头
// 之后叠加，同名时声明压住客户端带来的值。
func TestClientHeaderDeclarationOverridesClient(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions": chatCompletion,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"gpt-x"})
	f.mustSetHeader(t, "acme", "", "openai-beta", "declared-beta")
	f.mustSetHeader(t, "acme", "", "openai-organization", "org-declared")

	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	headers := http.Header{}
	headers.Set("openai-beta", "client-beta")
	headers.Set("openai-organization", "org-client")
	headers.Set("openai-project", "proj-client")
	body := `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	status, resp := postWithHeaders(t, srv, "/v1/chat/completions", body, headers, "")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	h := up.mustCalls(t, 1)[0].Header
	if got := h.Values("openai-beta"); len(got) != 1 || got[0] != "declared-beta" {
		t.Fatalf("openai-beta = %q，期望只剩声明的 declared-beta", got)
	}
	if got := h.Get("openai-organization"); got != "org-declared" {
		t.Fatalf("openai-organization = %q，期望声明的 org-declared", got)
	}
	if got := h.Get("openai-project"); got != "proj-client" {
		t.Fatalf("openai-project = %q，没有声明时客户端值照常透传", got)
	}
}

// TestClientHeaderTooLongDropped 确认单个头值超过 8KB 时整条头被丢弃，日志里有头名与长度、
// 没有值：异常客户端把头发爆不该变成上游的 431。
func TestClientHeaderTooLongDropped(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/messages": probeAnthropicOK,
	})
	f.addAs(t, "a1", "atest", gateway.ProtocolAnthropic, up.srv.URL, "sk-ant", []string{"claude-x"})
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	const tooLong = 8<<10 + 1
	big := strings.Repeat("A", tooLong-8) + "MARKERXX"
	if len(big) != tooLong {
		t.Fatalf("测试值长度 = %d，期望 %d", len(big), tooLong)
	}
	headers := http.Header{}
	headers.Set("anthropic-beta", big)
	status, body := postWithHeaders(t, srv, "/v1/messages", anthropicMessagesBody, headers, "")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	h := up.mustCalls(t, 1)[0].Header
	if _, ok := h["Anthropic-Beta"]; ok {
		t.Fatalf("超长的 anthropic-beta 该被丢弃，实际长度 %d", len(h.Get("anthropic-beta")))
	}
	got := logs()
	if !strings.Contains(got, "anthropic-beta") || !strings.Contains(got, strconv.Itoa(tooLong)) {
		t.Fatalf("日志里要写头名与长度：\n%s", got)
	}
	if strings.Contains(got, "MARKERXX") || strings.Contains(got, strings.Repeat("A", 64)) {
		t.Fatalf("日志里出现了头的值：\n%s", got)
	}
}
