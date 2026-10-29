package gateway_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switchboard/internal/gateway"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

type fixture struct {
	st  *store.Store
	key []byte
	// fakeProviders 记下 add 自己建的假厂商：同一对 (协议, 地址) 只建一次，后面的账号复用它。
	// 迁移 0014 之后账号行只存厂商名，地址与默认协议都在厂商行上（见 DESIGN 5.8）。
	fakeProviders map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	return &fixture{st: st, key: key, fakeProviders: map[string]string{}}
}

// add 建一个账号。地址与默认协议只在厂商行上，所以先保证有一家厂商：同一对 (协议, 地址) 复用同一家，
// 账号行只留厂商名。假厂商声明三条入站协议各一条按后缀规整出来的路由（协议是 gemini 时再加 gemini
// 那条），默认协议就是传进来的协议，因此转发、探活与模型同步看到的地址与升级前一致。
func (f *fixture) add(t *testing.T, name, protocol, baseURL, secretValue string, models []string, priority int) store.Account {
	t.Helper()
	provider := f.fakeProvider(t, protocol, baseURL)
	return f.createAccount(t, name, provider, secretValue, models, priority)
}

// fakeProvider 返回一对 (协议, 地址) 对应的假厂商名，第一次调用时建行。
func (f *fixture) fakeProvider(t *testing.T, protocol, baseURL string) string {
	t.Helper()
	cacheKey := protocol + " " + baseURL
	if name, ok := f.fakeProviders[cacheKey]; ok {
		return name
	}
	name := fmt.Sprintf("fake%d", len(f.fakeProviders)+1)
	if err := f.st.AddProvider(store.Provider{Name: name, BaseURL: baseURL, DefaultProtocol: protocol},
		fakeRoutes(baseURL, protocol)); err != nil {
		t.Fatalf("建假厂商 %s（%s，%s）: %v", name, protocol, baseURL, err)
	}
	f.fakeProviders[cacheKey] = name
	return name
}

// fakeRoutes 给假厂商声明路由，取值就是升级前从 base_url 后缀猜出来的那些地址：
// 三条入站协议各一条，protocol 是 gemini 时再加上 gemini（没有 gemini 路由时默认协议指不到它）。
// 路径能写成相对路径就写相对的（这样 base_url 仍是地址来源），地址里带脏写法拼不出来时
// 退回整条地址：路由收整条地址时不拼 base_url，最终请求地址一样（见 DESIGN 5.8）。
func fakeRoutes(baseURL, protocol string) []store.ProviderRoute {
	protocols := []string{gateway.ProtocolAnthropic, gateway.ProtocolChat, gateway.ProtocolResponses}
	if protocol == gateway.ProtocolGemini {
		protocols = append(protocols, gateway.ProtocolGemini)
	}
	modelsPath := relativeToBase(baseURL, gateway.ModelsEndpoint(baseURL))
	routes := make([]store.ProviderRoute, 0, len(protocols))
	for _, p := range protocols {
		routes = append(routes, store.ProviderRoute{
			Protocol:   p,
			Path:       relativeToBase(baseURL, gateway.Endpoint(baseURL, p)),
			ModelsPath: modelsPath,
		})
	}
	return routes
}

// relativeToBase 把完整地址换成相对 base_url 的路径。去前缀失败（base_url 有重复斜杠一类的脏写法）
// 时原样返回整条地址，调用方存成路由的 path 或 models_path。
func relativeToBase(baseURL, full string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base != "" && strings.HasPrefix(full, base) {
		if rest := strings.TrimPrefix(full, base); rest != "" {
			return rest
		}
	}
	return full
}

// createAccount 只写账号自己能存的东西：名字、厂商、凭据、模型与优先级。
// 返回读回来的那一行，因为 BaseURL 与 Protocol 是派生值（见 store.Account），
// 写入侧不看也不回写它们。
func (f *fixture) createAccount(t *testing.T, name, provider, secretValue string, models []string, priority int) store.Account {
	t.Helper()
	cipher, err := secret.Encrypt(f.key, []byte(secretValue))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}
	acc := store.Account{
		Name:      name,
		Provider:  provider,
		KeyCipher: cipher,
		Models:    models,
		Priority:  priority,
		Enabled:   true,
	}
	if err := f.st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	got, err := f.st.GetAccount(name)
	if err != nil {
		t.Fatalf("读账号 %s: %v", name, err)
	}
	return got
}

// addOrphan 建一个指向不存在的厂商的账号：迁移 0014 之前留下的孤儿行，
// 地址与默认协议都是空串（见 DESIGN 5.8）。
func (f *fixture) addOrphan(t *testing.T, name, provider, secretValue string, models []string) store.Account {
	t.Helper()
	return f.createAccount(t, name, provider, secretValue, models, 100)
}

func (f *fixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postMessages(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
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

const chatCompletion = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"你好，世界"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`

func TestTranslateNonStream(t *testing.T) {
	f := newFixture(t)

	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatCompletion)
	}))
	defer up.Close()

	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	var out translate.AnthropicResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是 anthropic 格式: %v，原文 %s", err, body)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "你好，世界" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", out.Usage)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("上游 Authorization = %q，凭据应在转发时解密并使用", gotAuth)
	}
	if !strings.Contains(gotBody, `"role":"user"`) || !strings.Contains(gotBody, `"model":"m1"`) {
		t.Fatalf("上游请求体 = %s", gotBody)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d", len(recs))
	}
	if !recs[0].OK || recs[0].InTokens != 5 || recs[0].OutTokens != 3 || recs[0].Outbound != gateway.ProtocolChat {
		t.Fatalf("请求记录 = %+v", recs[0])
	}
}

func TestPassthroughAnthropic(t *testing.T) {
	f := newFixture(t)

	upstreamBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2}}`
	var gotAPIKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer up.Close()

	f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"claude-x"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"ping"}],"unknown_field":1}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if body != upstreamBody {
		t.Fatalf("直通响应被改动了:\n得到 %s\n期望 %s", body, upstreamBody)
	}
	if gotAPIKey != "sk-ant" {
		t.Fatalf("上游 x-api-key = %q", gotAPIKey)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].InTokens != 8 || recs[0].OutTokens != 2 {
		t.Fatalf("请求记录 = %+v", recs)
	}
}

func TestStreamTranslate(t *testing.T) {
	f := newFixture(t)

	chunks := []string{
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m1","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读流: %v", err)
	}
	body := string(b)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	for _, want := range []string{"event: message_start", `"text":"he"`, `"text":"llo"`, "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("流里缺少 %q：\n%s", want, body)
		}
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].InTokens != 4 || recs[0].OutTokens != 2 {
		t.Fatalf("请求记录 = %+v", recs)
	}
}

func TestFailoverOnRateLimit(t *testing.T) {
	f := newFixture(t)

	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"慢一点","type":"rate_limit"}}`)
	}))
	defer limited.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, chatCompletion)
	}))
	defer healthy.Close()

	first := f.add(t, "limited", gateway.ProtocolChat, limited.URL, "sk-1", []string{"m1"}, 100)
	f.add(t, "healthy", gateway.ProtocolChat, healthy.URL, "sk-2", []string{"m1"}, 200)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	acc, err := f.st.GetAccount(first.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if acc.CooldownUntil == 0 {
		t.Fatalf("被限流的账号没有进冷却: %+v", acc)
	}
	if acc.FailStreak != 1 {
		t.Fatalf("连续失败次数 = %d，期望 1", acc.FailStreak)
	}

	healthyAcc, err := f.st.GetAccount("healthy")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if healthyAcc.FailStreak != 0 || healthyAcc.CooldownUntil != 0 {
		t.Fatalf("成功的账号不应留下失败计数: %+v", healthyAcc)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("请求记录数 = %d，期望 2（失败一次 + 成功一次）", len(recs))
	}
}

func TestUnauthorizedDisablesAccount(t *testing.T) {
	f := newFixture(t)

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"key 无效","type":"invalid_api_key"}}`)
	}))
	defer denied.Close()

	acc := f.add(t, "bad", gateway.ProtocolChat, denied.URL, "sk-bad", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "已经停用") && !strings.Contains(body, "已停用") {
		t.Fatalf("错误提示应说明账号被停用: %s", body)
	}

	got, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if got.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
}

func TestForbiddenDoesNotDisableAccount(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"MODEL_NOT_IN_PLAN"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusForbidden {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "无权使用该模型") {
		t.Fatalf("错误提示应说明是权限问题: %s", body)
	}

	got, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !got.Enabled {
		t.Fatal("403 不应停用账号，凭据本身还能用")
	}
}

// 没有任何账号声明提供这个模型时返回 404，并说明下一步命令；不是 503。
// TestInboundRejectsOtherMethodsWithAllowPost 钉住三条入站路径的 405 带 `Allow: POST`：
// 两个只读清单接口的 405 带 `Allow: GET`，两边对称，客户端据此能区分「方法用错了」
// 与「路径不存在」。路径认不出来时仍是 404，不带 `Allow`。
func TestInboundRejectsOtherMethodsWithAllowPost(t *testing.T) {
	f := newFixture(t)
	f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-1", []string{"m1"}, 100)
	srv := f.server(t)

	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead, http.MethodPatch} {
			req, err := http.NewRequest(method, srv.URL+path, nil)
			if err != nil {
				t.Fatalf("构造请求: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s 状态 = %d，期望 405（响应 %s）", method, path, resp.StatusCode, string(b))
			}
			if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
				t.Fatalf("%s %s Allow = %q，期望 POST", method, path, allow)
			}
			// HEAD 的响应体由 net/http 按协议丢掉，只校验状态与 Allow。
			if method == http.MethodHead {
				continue
			}
			if !strings.Contains(string(b), "只接受 POST") {
				t.Fatalf("%s %s 响应 = %s，期望说明只接受 POST", method, path, string(b))
			}
		}
	}

	// 未知路径是 404（没有这个地址），不是 405。
	status, _, header := callWithMethod(t, http.MethodGet, srv.URL+"/v1/unknown")
	if status != http.StatusNotFound {
		t.Fatalf("未知路径状态 = %d，期望 404", status)
	}
	if allow := header.Get("Allow"); allow != "" {
		t.Fatalf("未知路径 Allow = %q，期望不带这个头", allow)
	}
}

func TestModelWithoutProviderIsNotFound(t *testing.T) {
	f := newFixture(t)
	// 只放一个模型列表为空的账号：它刚建号、不提供任何模型，不该接住任意模型名。
	f.add(t, "fresh", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-1", nil, 100)
	srv := f.server(t)

	const model = "deepseek/deepseek-v4.1-flash"
	cases := []struct {
		path   string
		body   string
		wantTy string
	}{
		{"/v1/messages",
			`{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			"not_found_error"},
		{"/v1/chat/completions",
			`{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			"invalid_request_error"},
		{"/v1/responses",
			`{"model":"` + model + `","max_output_tokens":16,"input":"hi"}`,
			"invalid_request_error"},
	}
	for _, tc := range cases {
		resp, err := http.Post(srv.URL+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s 请求网关: %v", tc.path, err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s 读响应: %v", tc.path, err)
		}
		body := string(b)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 状态 = %d，响应 %s", tc.path, resp.StatusCode, body)
		}
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("%s 响应不是 JSON: %v，原文 %s", tc.path, err, body)
		}
		if env.Error.Type != tc.wantTy {
			t.Fatalf("%s 错误类型 = %q，期望 %q：%s", tc.path, env.Error.Type, tc.wantTy, body)
		}
		for _, want := range []string{model, "switchboard model ls", "switchboard model probe"} {
			if !strings.Contains(env.Error.Message, want) {
				t.Fatalf("%s 错误文案应含 %q：%s", tc.path, want, body)
			}
		}
	}
}

// 有账号提供该模型，但全部在冷却或停用时仍是 503：那是可用性问题，不是清单问题。
func TestAllProvidersCoolingOrDisabled(t *testing.T) {
	f := newFixture(t)
	cooling := f.add(t, "cooling", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-1", []string{"m1"}, 100)
	disabled := f.add(t, "disabled", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-2", []string{"m1"}, 200)
	if err := f.st.RecordFailure(cooling.ID, time.Now().Add(time.Minute).Unix(), "429 冷却中"); err != nil {
		t.Fatalf("设置冷却: %v", err)
	}
	if err := f.st.SetEnabled(disabled.ID, false, "手工停用"); err != nil {
		t.Fatalf("停用账号: %v", err)
	}
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "冷却") || !strings.Contains(body, "停用") {
		t.Fatalf("错误提示应说明账号在冷却或被停用: %s", body)
	}
}

// 新增的 404 与 503 路径都不碰凭据：响应体与日志里都不该出现 key 明文。
func TestCandidateErrorsDoNotLeakKey(t *testing.T) {
	f := newFixture(t)
	const key = "sk-leak-canary-0123456789"
	fresh := f.add(t, "fresh", gateway.ProtocolChat, "http://127.0.0.1:1", key, nil, 100)

	var logs strings.Builder
	logf := func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	}
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Logf: logf})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	// 没有账号提供 m1：404。
	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if strings.Contains(body, key) {
		t.Fatalf("404 响应体泄漏了凭据: %s", body)
	}

	// 有账号提供 m1 但正在冷却：503。
	if err := f.st.RecordFailure(fresh.ID, time.Now().Add(time.Minute).Unix(), "429 冷却中"); err != nil {
		t.Fatalf("设置冷却: %v", err)
	}
	if err := f.st.SetModels(fresh.ID, []string{"m1"}); err != nil {
		t.Fatalf("写模型列表: %v", err)
	}
	status, body = postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if strings.Contains(body, key) {
		t.Fatalf("503 响应体泄漏了凭据: %s", body)
	}

	logged := logs.String()
	if strings.Contains(logged, key) {
		t.Fatalf("日志泄漏了凭据: %s", logged)
	}
}

// 模型列表为空表示不提供任何模型：刚建号的账号因此不会被选中，也不会被上游 400 打脸。
func TestEmptyModelListAccountIsNotCandidate(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("模型列表为空的账号不该被选号，却收到了请求")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"The supported API model names are deepseek-flash, deepseek-v4-pro"}}`)
	}))
	defer up.Close()

	f.add(t, "yulong", gateway.ProtocolChat, up.URL, "sk-1", nil, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
}

func TestAllCandidatesFail(t *testing.T) {
	f := newFixture(t)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"上游挂了"}}`)
	}))
	defer broken.Close()

	f.add(t, "broken", gateway.ProtocolChat, broken.URL, "sk-1", []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "上游挂了") {
		t.Fatalf("响应体应带上最后一次上游错误: %s", body)
	}
}

func TestUpstreamErrorDoesNotLeakKey(t *testing.T) {
	f := newFixture(t)

	const key = "sk-super-secret-value"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key `+key+`"}}`)
	}))
	defer up.Close()

	f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if strings.Contains(body, key) {
		t.Fatalf("响应体泄漏了凭据: %s", body)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || strings.Contains(recs[0].Error, key) {
		t.Fatalf("请求记录泄漏了凭据: %+v", recs)
	}
}

func TestEndpoint(t *testing.T) {
	cases := []struct{ base, protocol, want string }{
		{"https://api.deepseek.com", gateway.ProtocolChat, "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/", gateway.ProtocolChat, "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1", gateway.ProtocolChat, "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.anthropic.com", gateway.ProtocolAnthropic, "https://api.anthropic.com/v1/messages"},
		{"https://proxy.example.com/v1", gateway.ProtocolAnthropic, "https://proxy.example.com/v1/messages"},
		// gemini 的模型名在路径里，兜底同样留占位符，转发前才替换（DESIGN 5.9）。
		{"https://generativelanguage.googleapis.com", gateway.ProtocolGemini,
			"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"},
		{"https://generativelanguage.googleapis.com/v1beta/", gateway.ProtocolGemini,
			"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"},
	}
	for _, c := range cases {
		if got := gateway.Endpoint(c.base, c.protocol); got != c.want {
			t.Fatalf("Endpoint(%q, %q) = %q，期望 %q", c.base, c.protocol, got, c.want)
		}
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-1", []string{"m1", "m2"}, 100)
	srv := f.server(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求 healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"models":2`) {
		t.Fatalf("healthz = %s", b)
	}
}
