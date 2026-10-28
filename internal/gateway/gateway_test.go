package gateway_test

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

type fixture struct {
	st  *store.Store
	key []byte
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
	return &fixture{st: st, key: key}
}

func (f *fixture) add(t *testing.T, name, protocol, baseURL, secretValue string, models []string, priority int) store.Account {
	t.Helper()
	cipher, err := secret.Encrypt(f.key, []byte(secretValue))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}
	acc := store.Account{
		Name:      name,
		Provider:  "test",
		Protocol:  protocol,
		BaseURL:   baseURL,
		KeyCipher: cipher,
		Models:    models,
		Priority:  priority,
		Enabled:   true,
	}
	if err := f.st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	return acc
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

func TestNoCandidates(t *testing.T) {
	f := newFixture(t)
	f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-1", []string{"other-model"}, 100)
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "account add") {
		t.Fatalf("错误提示应告诉用户下一步命令: %s", body)
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
