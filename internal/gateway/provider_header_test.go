package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
)

// headerUpstream 是记录每次上游请求头与路径的假上游。附加头的用例只看「这个头发没发、值对不对」，
// 响应体一律按路径给固定形状：转发、模型列表、探活三段共用它。
type headerUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []*http.Request
}

func newHeaderUpstream(t *testing.T, replies map[string]string) *headerUpstream {
	t.Helper()
	up := &headerUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.calls = append(up.calls, r.Clone(context.Background()))
		up.mu.Unlock()

		body, ok := replies[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"没有这个路由"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.srv.Close)
	return up
}

// all 按顺序取回收到的请求。头要在请求处理完之后读，所以留的是克隆。
func (u *headerUpstream) all() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]*http.Request, len(u.calls))
	copy(out, u.calls)
	return out
}

func (u *headerUpstream) mustCalls(t *testing.T, want int) []*http.Request {
	t.Helper()
	calls := u.all()
	if len(calls) != want {
		paths := make([]string, len(calls))
		for i, c := range calls {
			paths[i] = c.Method + " " + c.URL.Path
		}
		t.Fatalf("上游调用数 = %d（%v），期望 %d", len(calls), paths, want)
	}
	return calls
}

// providerWithRoutes 建一家厂商行。附加头有外键指向它，所以带附加头的用例都必须有这一行；
// 路由同时决定转发地址与协议作用域。
func (f *fixture) providerWithRoutes(t *testing.T, name, baseURL string, routes ...store.ProviderRoute) {
	t.Helper()
	if err := f.st.CreateProvider(store.Provider{Name: name, BaseURL: baseURL}, routes); err != nil {
		t.Fatalf("建厂商 %s: %v", name, err)
	}
}

func (f *fixture) mustSetHeader(t *testing.T, provider, protocol, name, value string) {
	t.Helper()
	if err := f.st.SetProviderHeader(provider, protocol, name, value); err != nil {
		t.Fatalf("声明附加头 %s/%s: %v", protocol, name, err)
	}
}

// accountRow 取回账号的最新行：转发、同步与探活的入口都吃 store.Account。
func (f *fixture) accountRow(t *testing.T, name string) store.Account {
	t.Helper()
	acc, err := f.st.GetAccount(name)
	if err != nil {
		t.Fatalf("读账号 %s: %v", name, err)
	}
	return acc
}

// TestForwardInjectsProviderHeaders 确认转发请求带上厂商级附加头，且 {model} 展开成
// 这次请求的模型名。
func TestForwardInjectsProviderHeaders(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions": chatCompletion,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"gpt-x"})
	f.mustSetHeader(t, "acme", "", "api-version", "2024-10-21")
	f.mustSetHeader(t, "acme", "", "x-routed-model", "{model}")

	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	status, body := postMessages(t, srv, `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	calls := up.mustCalls(t, 1)
	if got := calls[0].Header.Get("api-version"); got != "2024-10-21" {
		t.Fatalf("api-version = %q，期望 2024-10-21", got)
	}
	if got := calls[0].Header.Get("x-routed-model"); got != "gpt-x" {
		t.Fatalf("x-routed-model = %q，期望展开成请求里的模型名 gpt-x", got)
	}
	if got := calls[0].Header.Get("Authorization"); got != "Bearer sk-acme" {
		t.Fatalf("Authorization = %q，没声明时仍按 setAuth 发 Bearer", got)
	}
	if strings.Contains(logs(), "附加头 api-version") || strings.Contains(logs(), "附加头 x-routed-model") {
		t.Fatalf("正常发送不记日志，日志里不该出现头名：\n%s", logs())
	}
}

// TestProviderHeadersScopePerProtocol 确认协议级只对那条协议生效，同名时压过厂商级，
// 另一条协议仍拿厂商级的声明。
func TestProviderHeadersScopePerProtocol(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions":   chatCompletion,
		"POST /anthropic/v1/messages": probeAnthropicOK,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"},
		store.ProviderRoute{Protocol: gateway.ProtocolAnthropic, Path: "/anthropic/v1/messages"})
	acc := f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"claude-x", "gpt-x"})
	// claude-x 的协议发现结果是 anthropic：同一个账号的两个模型各走各的协议。
	if err := f.st.SetModelProtocol(acc.ID, "claude-x", gateway.ProtocolAnthropic); err != nil {
		t.Fatalf("写协议发现结果: %v", err)
	}
	f.mustSetHeader(t, "acme", "", "x-scope", "vendor")
	f.mustSetHeader(t, "acme", gateway.ProtocolChat, "x-scope", "chat")
	f.mustSetHeader(t, "acme", gateway.ProtocolChat, "x-chat-only", "c")
	f.mustSetHeader(t, "acme", gateway.ProtocolAnthropic, "x-anthropic-only", "a")

	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	if status, body := postMessages(t, srv, `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("chat 模型状态 = %d，响应 %s", status, body)
	}
	if status, body := postMessages(t, srv, `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("anthropic 模型状态 = %d，响应 %s", status, body)
	}

	calls := up.mustCalls(t, 2)
	chat, anth := calls[0].Header, calls[1].Header
	if got := chat.Get("x-scope"); got != "chat" {
		t.Fatalf("chat 的 x-scope = %q，期望协议级覆盖厂商级后的 chat", got)
	}
	if got := chat.Get("x-chat-only"); got != "c" {
		t.Fatalf("chat 的 x-chat-only = %q，期望 c", got)
	}
	if got := chat.Get("x-anthropic-only"); got != "" {
		t.Fatalf("anthropic 级的头不该出现在 chat 请求上，实际 %q", got)
	}
	if got := anth.Get("x-scope"); got != "vendor" {
		t.Fatalf("anthropic 的 x-scope = %q，期望没被协议级覆盖时仍是厂商级的 vendor", got)
	}
	if got := anth.Get("x-anthropic-only"); got != "a" {
		t.Fatalf("anthropic 的 x-anthropic-only = %q，期望 a", got)
	}
	if got := anth.Get("x-chat-only"); got != "" {
		t.Fatalf("chat 级的头不该出现在 anthropic 请求上，实际 %q", got)
	}
}

// TestProviderHeaderEnvPlaceholder 覆盖 {env:NAME} 的三种情形：变量有值就发、没设置与设置为空串
// 都不发，且日志只说环境变量名与原因，不带值。
func TestProviderHeaderEnvPlaceholder(t *testing.T) {
	const (
		envHit     = "SWITCHBOARD_TEST_HEADER_TOKEN"
		envEmpty   = "SWITCHBOARD_TEST_HEADER_EMPTY"
		envMissing = "SWITCHBOARD_TEST_HEADER_MISSING"
		tokenValue = "env-token-value-77"
	)
	t.Setenv(envHit, tokenValue)
	t.Setenv(envEmpty, "")
	if err := os.Unsetenv(envMissing); err != nil {
		t.Fatalf("清环境变量 %s: %v", envMissing, err)
	}

	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions": chatCompletion,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"gpt-x"})
	f.mustSetHeader(t, "acme", "", "x-env-hit", "{env:"+envHit+"}")
	f.mustSetHeader(t, "acme", "", "x-env-empty", "{env:"+envEmpty+"}")
	f.mustSetHeader(t, "acme", "", "x-env-missing", "{env:"+envMissing+"}")

	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	if status, body := postMessages(t, srv, `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	h := up.mustCalls(t, 1)[0].Header
	if got := h.Get("x-env-hit"); got != tokenValue {
		t.Fatalf("x-env-hit = %q，期望环境变量的值", got)
	}
	if _, ok := h["x-env-empty"]; ok {
		t.Fatalf("环境变量是空串时不该发这个头，实际 %q", h.Get("x-env-empty"))
	}
	if _, ok := h["x-env-missing"]; ok {
		t.Fatalf("环境变量没设置时不该发这个头，实际 %q", h.Get("x-env-missing"))
	}
	got := logs()
	if !strings.Contains(got, "附加头 x-env-empty 未发送") || !strings.Contains(got, envEmpty) {
		t.Fatalf("空环境变量要在日志里说明是哪个变量：\n%s", got)
	}
	if !strings.Contains(got, "附加头 x-env-missing 未发送") || !strings.Contains(got, "环境变量 "+envMissing+" 未设置") {
		t.Fatalf("未设置的环境变量要写明「环境变量 X 未设置」：\n%s", got)
	}
	if strings.Contains(got, tokenValue) {
		t.Fatalf("日志里出现了附加头的值：\n%s", got)
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestProviderHeaderSessionPlaceholder 确认 {session} 与选号粘性取同一个会话键：
// 有键时同一对话稳定、没键时每次请求现生成一个不同的 UUID。
func TestProviderHeaderSessionPlaceholder(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions": chatCompletion,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"gpt-x"})
	f.mustSetHeader(t, "acme", "", "x-opencode-session", "{session}")

	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	body := `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	postWithSession := func(session, payload string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("x-switchboard-session", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求网关: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("状态 = %d，响应 %s", resp.StatusCode, b)
		}
	}

	// 有会话键：同一个键两次请求拿到同一个值。
	postWithSession("sess-stable", body)
	postWithSession("sess-stable", body)
	// 会话键也可以来自请求体的 metadata.user_id，与选号粘性同一套取值。
	postWithSession("", `{"model":"gpt-x","max_tokens":16,"metadata":{"user_id":"user-42"},"messages":[{"role":"user","content":"hi"}]}`)
	// 都没有：每次请求现生成一个。
	postWithSession("", body)
	postWithSession("", body)

	calls := up.mustCalls(t, 5)
	values := make([]string, len(calls))
	for i, c := range calls {
		values[i] = c.Header.Get("x-opencode-session")
	}
	if values[0] != "sess-stable" || values[1] != "sess-stable" {
		t.Fatalf("有会话键时 {session} 应稳定为 sess-stable，实际 %q / %q", values[0], values[1])
	}
	if values[2] != "user-42" {
		t.Fatalf("metadata.user_id 应作为会话键，实际 %q", values[2])
	}
	for i := 3; i < 5; i++ {
		if !uuidV4.MatchString(values[i]) {
			t.Fatalf("没有会话键时应现生成一个 UUID v4，第 %d 次是 %q", i, values[i])
		}
	}
	if values[3] == values[4] {
		t.Fatalf("没有会话键时两个请求不该复用同一个值，实际都是 %q", values[3])
	}
}

// TestProviderHeaderOverridesDefaultAuth 确认「声明优先」：显式声明的 Authorization 压过
// setAuth 给的 Bearer，默认凭据头不再出现在请求上。
func TestProviderHeaderOverridesDefaultAuth(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions": chatCompletion,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"gpt-x"})
	f.mustSetHeader(t, "acme", "", "authorization", "Token declared-value")

	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	if status, body := postMessages(t, srv, `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if got := up.mustCalls(t, 1)[0].Header.Get("Authorization"); got != "Token declared-value" {
		t.Fatalf("Authorization = %q，期望声明值覆盖 setAuth 的 Bearer", got)
	}
}

// TestModelSyncSendsProviderHeaders 确认模型列表请求也带厂商附加头：{model} 没有模型名时
// 那条头不发，其余照发。
func TestModelSyncSendsProviderHeaders(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"GET /v1/models": `{"data":[{"id":"m1"}]}`,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions", ModelsPath: "/v1/models"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"m1"})
	f.mustSetHeader(t, "acme", "", "api-version", "2024-10-21")
	f.mustSetHeader(t, "acme", "", "x-routed-model", "{model}")

	gs, logs := newGatewayWith(t, f, gateway.Options{})
	models, err := gs.SyncModels(context.Background(), f.accountRow(t, "a1"))
	if err != nil {
		t.Fatalf("同步模型列表: %v", err)
	}
	if len(models) != 1 || models[0] != "m1" {
		t.Fatalf("模型列表 = %v，期望 [m1]", models)
	}

	h := up.mustCalls(t, 1)[0].Header
	if got := h.Get("api-version"); got != "2024-10-21" {
		t.Fatalf("模型列表请求的 api-version = %q，期望带上厂商附加头", got)
	}
	if _, ok := h["x-routed-model"]; ok {
		t.Fatalf("模型列表请求没有模型名，{model} 那条头不该发，实际 %q", h.Get("x-routed-model"))
	}
	if got := logs(); !strings.Contains(got, "附加头 x-routed-model 未发送") {
		t.Fatalf("没发的头要在日志里留一行原因：\n%s", got)
	}
}

// TestProbeSendsProviderHeaders 确认 `account test` 的两段（模型列表与最小调用）都带附加头。
func TestProbeSendsProviderHeaders(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"GET /v1/models":            `{"data":[{"id":"m1"}]}`,
		"POST /v1/chat/completions": chatCompletion,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions", ModelsPath: "/v1/models"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"m1"})
	f.mustSetHeader(t, "acme", "", "api-version", "2024-10-21")

	gs, _ := newGatewayWith(t, f, gateway.Options{})
	res, err := gs.Probe(context.Background(), f.accountRow(t, "a1"))
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if !res.OK {
		t.Fatalf("探活没有通过：%+v", res)
	}

	calls := up.mustCalls(t, 2)
	if calls[0].Method != http.MethodGet || calls[1].Method != http.MethodPost {
		t.Fatalf("两段顺序不对：%s %s，随后 %s", calls[0].Method, calls[0].URL.Path, calls[1].Method)
	}
	for _, c := range calls {
		if got := c.Header.Get("api-version"); got != "2024-10-21" {
			t.Fatalf("%s %s 的 api-version = %q，期望两段都带附加头", c.Method, c.URL.Path, got)
		}
	}
}

// TestProviderHeaderValueStaysOutOfRecords 确认附加头的值不进日志、也不进 requests：
// 头里可能是密钥，落库等于把密钥写进请求记录。
func TestProviderHeaderValueStaysOutOfRecords(t *testing.T) {
	const headerValue = "hdr-secret-value-8f3a"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "成功", status: http.StatusOK, body: chatCompletion},
		{name: "上游拒了", status: http.StatusBadRequest, body: `{"error":{"message":"missing session"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer up.Close()

			f.providerWithRoutes(t, "acme", up.URL,
				store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"})
			f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.URL, "sk-acme", []string{"gpt-x"})
			f.mustSetHeader(t, "acme", "", "x-opencode-session", headerValue)

			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			defer srv.Close()

			_, _ = postMessages(t, srv, `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

			if strings.Contains(logs(), headerValue) {
				t.Fatalf("日志里出现了附加头的值：\n%s", logs())
			}
			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			for _, rec := range recs {
				if strings.Contains(rec.Error, headerValue) {
					t.Fatalf("requests.error 里出现了附加头的值：%q", rec.Error)
				}
			}
			if strings.Contains(logs(), "附加头 x-opencode-session") {
				t.Fatalf("正常发送不记日志，日志里只允许出现未发送的头名与原因：\n%s", logs())
			}
		})
	}
}

// TestProviderHeaderReadFailureErrors 确认读附加头失败时报错而不是静默跳过：附加头缺一条，
// 上游就回 400，静默跳过等于把配置故障变成看不懂的协议错误。文案要指到 doctor。
func TestProviderHeaderReadFailureErrors(t *testing.T) {
	f := newFixture(t)
	up := newHeaderUpstream(t, map[string]string{
		"POST /v1/chat/completions": chatCompletion,
		"GET /v1/models":            `{"data":[{"id":"m1"}]}`,
	})
	f.providerWithRoutes(t, "acme", up.srv.URL,
		store.ProviderRoute{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions", ModelsPath: "/v1/models"})
	f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.srv.URL, "sk-acme", []string{"gpt-x"})

	// 把表删掉就是一次读库失败：不改代码就能造出这条路径。
	if _, err := f.st.DB().Exec(`DROP TABLE provider_headers`); err != nil {
		t.Fatalf("删表: %v", err)
	}

	gs, _ := newGatewayWith(t, f, gateway.Options{})
	if _, err := gs.SyncModels(context.Background(), f.accountRow(t, "a1")); err == nil ||
		!strings.Contains(err.Error(), "switchboard doctor") {
		t.Fatalf("模型同步读附加头失败时应报错并提示 doctor，实际 %v", err)
	}
	if _, err := gs.Probe(context.Background(), f.accountRow(t, "a1")); err == nil ||
		!strings.Contains(err.Error(), "switchboard doctor") {
		t.Fatalf("探活读附加头失败时应报错并提示 doctor，实际 %v", err)
	}

	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()
	status, body := postMessages(t, srv, `{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503（读配置失败不是协议错误）", status)
	}
	if !strings.Contains(body, "switchboard doctor") {
		t.Fatalf("转发读附加头失败时应提示 doctor，响应 %s", body)
	}
	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) == 0 || !strings.Contains(recs[0].Error, "switchboard doctor") {
		t.Fatalf("requests.error 应写清读附加头失败并提示 doctor，实际 %+v", recs)
	}
	if n := len(up.all()); n != 0 {
		t.Fatalf("读配置失败时不该发上游请求，实际 %d 次", n)
	}
}
