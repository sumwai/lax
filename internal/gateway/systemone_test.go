package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// 这个文件钉住第五条线协议 systemone 的入站、协议互通、错误体与探活。
// 它只与自己互通：决策请求没有对话消息序列也没有流，中间表示里没有它的位置，所以本文件里
// 没有一条「跨协议转换」的用例，只有同协议直通与两个方向的 400。

const systemOnePath = "/v1/systemone"

// systemOneOKBody 是假决策上游的正常响应：两个用量字段加一组答案，响应必须按字节原样回给客户端。
const systemOneOKBody = `{"answers":{"present":{"answer":"yes","probability":0.99}},` +
	`"usage":{"input_tokens":11,"output_tokens":4}}`

// systemOneRequestBody 是一条合法的最小决策请求，字段里带 state、questions 与 assets，
// 用来断言「除 model 之外的键原样转发」。
const systemOneRequestBody = `{"model":"typesafe/jev","state":{"text":"hello"},` +
	`"questions":{"present":{"type":"noul","instructions":"Is this text present?"}},` +
	`"assets":[{"id":"a1","kind":"text"}],"metadata":{"user_id":"s-1"}}`

// systemOneCall 是一次打到假决策上游的请求：路径、凭据头、整份头与请求体原文。
// 整份头留着断言「一个客户端头都不透传」（白名单里没有 systemone）。
type systemOneCall struct {
	path   string
	bearer string
	header http.Header
	body   string
}

// systemOneUpstream 是只说 systemone 的假上游：任何路径都回同一条响应，按顺序记下每次调用。
type systemOneUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []systemOneCall
}

func newSystemOneUpstream(t *testing.T, reply string) *systemOneUpstream {
	t.Helper()
	up := &systemOneUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.calls = append(up.calls, systemOneCall{
			path:   r.URL.Path,
			bearer: r.Header.Get("Authorization"),
			header: r.Header.Clone(),
			body:   string(body),
		})
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(up.srv.Close)
	return up
}

func (u *systemOneUpstream) all() []systemOneCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]systemOneCall, len(u.calls))
	copy(out, u.calls)
	return out
}

// addSystemOneProvider 建一家只声明 systemone 路由、默认协议也是 systemone 的厂商，
// 并返回它名下带 models 的账号。决策模型的名字上游不列（commandcode 的清单里没有），
// 只能手工声明：上游的模型清单里没有决策端点这一项。
func addSystemOneProvider(t *testing.T, f *fixture, name, baseURL, secretValue string, models []string) store.Account {
	t.Helper()
	provider := "typesafe-" + name
	if err := f.st.AddProvider(store.Provider{
		Name:            provider,
		BaseURL:         baseURL,
		DefaultProtocol: gateway.ProtocolSystemOne,
	}, []store.ProviderRoute{{
		Protocol: gateway.ProtocolSystemOne,
		Path:     systemOnePath,
	}}); err != nil {
		t.Fatalf("建 systemone 厂商 %s: %v", provider, err)
	}
	return f.createAccount(t, name, provider, secretValue, models, 100)
}

// decodedKeys 把两条 JSON 对象按顶层键解成 map，用于逐键比对「原样转发」。
func decodedKeys(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析 JSON 对象失败（%s）：%v", body, err)
	}
	return out
}

// detailError 解析我们自己给的 systemone 错误体：{"detail":{"error_type":"…","message":"…"}}。
// 字段名是 error_type 而不是 type（TypeSafe SDK 读的就是它）。
func detailError(t *testing.T, body string) (kind, message string) {
	t.Helper()
	var env struct {
		Detail struct {
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
		} `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("错误体不是 systemone 形状: %v，原文 %s", err, body)
	}
	if env.Detail.Message == "" || env.Detail.ErrorType == "" {
		t.Fatalf("错误体缺少 detail.message 或 detail.error_type: %s", body)
	}
	kind = env.Detail.ErrorType
	message = env.Detail.Message
	// 顶层只有 detail 一个键：多写一层 error 会让读 detail 的 SDK 拿到空对象。
	keys := decodedKeys(t, body)
	if len(keys) != 1 {
		t.Fatalf("错误体顶层键 = %v，期望只有 detail", keys)
	}
	// detail 是对象而不是纯字符串，且只写这两个键：SDK 读的是对象里的字段。
	detail, ok := keys["detail"].(map[string]any)
	if !ok {
		t.Fatalf("detail = %v，期望是对象", keys["detail"])
	}
	if len(detail) != 2 {
		t.Fatalf("detail 的键 = %v，期望只有 error_type 与 message", detail)
	}
	return kind, message
}

// TestSystemOneForwardReplacesModel 钉住同协议直通：只把 model 换成解析后的真名，
// state、questions、assets 与响应体一个字节不改，用量写进请求记录。
func TestSystemOneForwardReplacesModel(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	status, body := postPath(t, srv, systemOnePath, systemOneRequestBody)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if body != systemOneOKBody {
		t.Fatalf("响应被改动了:\n得到 %s\n期望 %s", body, systemOneOKBody)
	}

	calls := up.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用 %d 次，期望 1 次: %+v", len(calls), calls)
	}
	if calls[0].path != systemOnePath {
		t.Fatalf("上游路径 = %q，期望 %q", calls[0].path, systemOnePath)
	}
	if calls[0].bearer != "Bearer sk-ts" {
		t.Fatalf("上游 Authorization = %q，期望解密后的凭据", calls[0].bearer)
	}
	sent, want := decodedKeys(t, calls[0].body), decodedKeys(t, systemOneRequestBody)
	for key, value := range want {
		if key == "model" {
			continue
		}
		if !reflect.DeepEqual(sent[key], value) {
			t.Fatalf("上游请求体里的 %s = %v，期望原样保留 %v（整段 %s）", key, sent[key], value, calls[0].body)
		}
	}
	if sent["model"] != "typesafe/jev" {
		t.Fatalf("上游请求体里的 model = %v，期望解析后的真名 typesafe/jev", sent["model"])
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d，期望 1", len(recs))
	}
	rec := recs[0]
	if !rec.OK || rec.Inbound != gateway.ProtocolSystemOne || rec.Outbound != gateway.ProtocolSystemOne {
		t.Fatalf("请求记录 = %+v，期望 ok 且 inbound/outbound 都是 systemone", rec)
	}
	if rec.InTokens != 11 || rec.OutTokens != 4 {
		t.Fatalf("用量 = %d/%d，期望 11/4", rec.InTokens, rec.OutTokens)
	}
	// 这条协议没有缓存与思考字段，输入 token 按不含缓存那一档记（UncachedInTokens 的默认分支）。
	if rec.InTokensUncached != 11 || rec.CacheRead != 0 || rec.CacheWrite != 0 {
		t.Fatalf("入账 = %+v，期望 in_tokens_uncached 等于 in_tokens、缓存两列为 0", rec)
	}
}

// TestSystemOneAliasUsesRealName 钉住别名：入口解析一次，请求体里的 model 是解析后的真名，
// 请求记录两列分工（model_requested 是别名，model_used 是真名）。
func TestSystemOneAliasUsesRealName(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	mustAlias(t, f, "jev", "typesafe/jev")
	srv := f.server(t)

	body := strings.Replace(systemOneRequestBody, `"model":"typesafe/jev"`, `"model":"jev"`, 1)
	status, resp := postPath(t, srv, systemOnePath, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	calls := up.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用 %d 次，期望 1 次", len(calls))
	}
	if model := decodedKeys(t, calls[0].body)["model"]; model != "typesafe/jev" {
		t.Fatalf("上游收到的 model = %v，期望别名解析后的真名", model)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].ModelRequested != "jev" || recs[0].ModelUsed != "typesafe/jev" {
		t.Fatalf("请求记录 = %+v，期望 requested=jev、used=typesafe/jev", recs)
	}
}

// TestSystemOneRejectsStream 钉住「这条协议没有流式」：带 stream: true 的请求按 400 拒绝，
// 不静默当同步（等 SSE 的客户端会挂到超时），也不往上游发一次请求。
func TestSystemOneRejectsStream(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	body := strings.Replace(systemOneRequestBody, `"state"`, `"stream":true,"state"`, 1)
	status, resp := postPath(t, srv, systemOnePath, body)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 %s", status, resp)
	}
	kind, message := detailError(t, resp)
	if kind != "invalid_request_error" {
		t.Fatalf("错误类型 = %q，期望 invalid_request_error", kind)
	}
	if !strings.Contains(message, "没有流式") {
		t.Fatalf("文案应说明这条协议没有流式: %s", message)
	}
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("400 不该往上游发请求，实际发了 %d 次", len(calls))
	}
}

// TestSystemOneRejectsEmptyQuestions 钉住最少校验里的 questions：空对象与缺字段都按 400 拒绝。
func TestSystemOneRejectsEmptyQuestions(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	for _, body := range []string{
		`{"model":"typesafe/jev","state":"ping","questions":{}}`,
		`{"model":"typesafe/jev","state":"ping"}`,
	} {
		status, resp := postPath(t, srv, systemOnePath, body)
		if status != http.StatusBadRequest {
			t.Fatalf("请求 %s 的状态 = %d，期望 400，响应 %s", body, status, resp)
		}
		kind, message := detailError(t, resp)
		if kind != "invalid_request_error" || !strings.Contains(message, "questions") {
			t.Fatalf("错误 = %s/%s，期望 invalid_request_error 且提到 questions", kind, message)
		}
	}
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("400 不该往上游发请求，实际发了 %d 次", len(calls))
	}
}

// TestSystemOneRejectsOversizedBody 钉住 8 MiB 上限：超了给 413 而不是截断成一条解析失败。
func TestSystemOneRejectsOversizedBody(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	padding := make([]byte, 9<<20)
	for i := range padding {
		padding[i] = 'x'
	}
	body := `{"model":"typesafe/jev","state":"ping","questions":{"ok":{"type":"noul"}},"pad":"` +
		string(padding) + `"}`
	status, resp := postPath(t, srv, systemOnePath, body)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态 = %d，期望 413，响应 %s", status, resp)
	}
	kind, message := detailError(t, resp)
	if kind != "invalid_request_error" || !strings.Contains(message, "8 MiB") {
		t.Fatalf("错误 = %s/%s，期望 invalid_request_error 且说明 8 MiB 上限", kind, message)
	}
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("413 不该往上游发请求，实际发了 %d 次", len(calls))
	}
}

// TestSystemOneModelNotFoundShape 钉住 404 的错误体形状与类型：detail 对象、类型是
// not_found_error（与 anthropic 那套同名）。
func TestSystemOneModelNotFoundShape(t *testing.T) {
	f := newFixture(t)
	addSystemOneProvider(t, f, "ts1", "http://127.0.0.1:1", "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	body := strings.Replace(systemOneRequestBody, `"typesafe/jev"`, `"ghost-model"`, 1)
	status, resp := postPath(t, srv, systemOnePath, body)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404，响应 %s", status, resp)
	}
	kind, message := detailError(t, resp)
	if kind != "not_found_error" {
		t.Fatalf("错误类型 = %q，期望 not_found_error", kind)
	}
	if !strings.Contains(message, "ghost-model") {
		t.Fatalf("文案应带上模型名: %s", message)
	}
}

// TestSystemOneRejectsOtherProtocolModel 钉住互通判定的一个方向：入站 systemone、模型的有效
// 出站协议是另外四种时 400，文案说明这个模型走哪条协议、该用哪条入站路径。
func TestSystemOneRejectsOtherProtocolModel(t *testing.T) {
	f := newFixture(t)
	up, _ := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"claude-x"}, 100)
	srv := f.server(t)

	body := strings.Replace(systemOneRequestBody, `"typesafe/jev"`, `"claude-x"`, 1)
	status, resp := postPath(t, srv, systemOnePath, body)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 %s", status, resp)
	}
	kind, message := detailError(t, resp)
	if kind != "invalid_request_error" {
		t.Fatalf("错误类型 = %q，期望 invalid_request_error", kind)
	}
	if !strings.Contains(message, gateway.ProtocolAnthropic) || !strings.Contains(message, "/v1/messages") {
		t.Fatalf("文案应说明这个模型走 anthropic、对话请求用 /v1/messages: %s", message)
	}
}

// TestChatRejectsSystemOneModel 钉住互通判定的另一个方向：入站是四条之一、模型的有效出站
// 协议是 systemone 时同样 400，且错误体跟随入站协议（这里是 anthropic 的形状）。
func TestChatRejectsSystemOneModel(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	status, resp := postMessages(t, srv,
		`{"model":"typesafe/jev","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 %s", status, resp)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &env); err != nil {
		t.Fatalf("错误体不是 anthropic 形状: %v，原文 %s", err, resp)
	}
	if env.Type != "error" || env.Error.Type != "invalid_request_error" {
		t.Fatalf("错误体 = %+v，期望 anthropic 形状的 invalid_request_error", env)
	}
	if !strings.Contains(env.Error.Message, gateway.ProtocolSystemOne) ||
		!strings.Contains(env.Error.Message, systemOnePath) {
		t.Fatalf("文案应说明这个模型走 systemone、决策请求用 %s: %s", systemOnePath, env.Error.Message)
	}
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("400 不该往上游发请求，实际发了 %d 次", len(calls))
	}
}

// TestSystemOneMethodNotAllowed 钉住这条入站路径只收 POST：其它方法 405 并带 Allow: POST。
func TestSystemOneMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	srv := f.server(t)

	req, err := http.NewRequest(http.MethodGet, srv.URL+systemOnePath, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("状态 = %d，期望 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q，期望 POST", allow)
	}
}

// TestHealthzListsSystemOne 钉住 /healthz 的协议清单：入站与出站都列出 systemone。
func TestHealthzListsSystemOne(t *testing.T) {
	f := newFixture(t)
	srv := f.server(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求 /healthz: %v", err)
	}
	defer resp.Body.Close()
	var env struct {
		Inbound  []string `json:"inbound"`
		Outbound []string `json:"outbound"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("解析 /healthz: %v", err)
	}
	for name, list := range map[string][]string{"inbound": env.Inbound, "outbound": env.Outbound} {
		found := false
		for _, p := range list {
			if p == gateway.ProtocolSystemOne {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s = %v，期望包含 systemone", name, list)
		}
	}
}

// TestProbeSystemOneSendsDecisionRequest 钉住探活：systemone 模型收到的是一条最小决策请求
// （有 questions、没有 messages），不是 max_tokens=1 的对话请求；2xx 即 allowed。
func TestProbeSystemOneSendsDecisionRequest(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	acc := addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", nil)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "typesafe/jev")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Access != store.AccessAllowed || res.Protocol != gateway.ProtocolSystemOne {
		t.Fatalf("结论 = %+v，期望 allowed/systemone", res)
	}

	calls := up.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用 %d 次，期望 1 次: %+v", len(calls), calls)
	}
	if calls[0].path != systemOnePath {
		t.Fatalf("探活路径 = %q，期望 %q", calls[0].path, systemOnePath)
	}
	sent := decodedKeys(t, calls[0].body)
	if _, ok := sent["questions"]; !ok {
		t.Fatalf("探活请求体里没有 questions，探活可能发成了对话请求: %s", calls[0].body)
	}
	if _, ok := sent["messages"]; ok {
		t.Fatalf("探活请求体里出现了 messages，说明发的是对话请求: %s", calls[0].body)
	}
	if sent["state"] != "ping" || sent["model"] != "typesafe/jev" {
		t.Fatalf("探活请求体 = %s，期望 state=ping、model=typesafe/jev", calls[0].body)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].Inbound != gateway.ProtocolSystemOne ||
		recs[0].Outbound != gateway.ProtocolSystemOne || !recs[0].OK {
		t.Fatalf("探活记录 = %+v，期望一次 systemone→systemone 的成功行", recs)
	}
}

// TestSystemOneRejectsBadModelField 钉住 model 的两种不合格写法：不是字符串、是空串。
func TestSystemOneRejectsBadModelField(t *testing.T) {
	f := newFixture(t)
	up := newSystemOneUpstream(t, systemOneOKBody)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	for _, body := range []string{
		`{"model":123,"state":"ping","questions":{"ok":{"type":"noul"}}}`,
		`{"model":"  ","state":"ping","questions":{"ok":{"type":"noul"}}}`,
		`{"model":null,"state":"ping","questions":{"ok":{"type":"noul"}}}`,
	} {
		status, resp := postPath(t, srv, systemOnePath, body)
		if status != http.StatusBadRequest {
			t.Fatalf("请求 %s 的状态 = %d，期望 400，响应 %s", body, status, resp)
		}
		kind, message := detailError(t, resp)
		if kind != "invalid_request_error" || !strings.Contains(message, "model") {
			t.Fatalf("错误 = %s/%s，期望 invalid_request_error 且提到 model", kind, message)
		}
	}
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("400 不该往上游发请求，实际发了 %d 次", len(calls))
	}
}

// TestSystemOneUpstreamErrorKeepsType 钉住上游失败的形状与冷却：单账号 429 冷却后候选耗尽，
// 回 503 与 api_error；429 不在一次请求内重试同一个账号，错误体始终是 detail 对象。
func TestSystemOneUpstreamErrorKeepsType(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"detail":{"message":"slow down"}}`)
	}))
	defer up.Close()
	addSystemOneProvider(t, f, "ts1", up.URL, "sk-ts", []string{"typesafe/jev"})
	srv := f.server(t)

	status, resp := postPath(t, srv, systemOnePath, systemOneRequestBody)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503（429 已冷却、候选耗尽），响应 %s", status, resp)
	}
	kind, message := detailError(t, resp)
	if kind != "api_error" || !strings.Contains(message, "slow down") {
		t.Fatalf("错误 = %s/%s，期望 api_error 且带上上游原话", kind, message)
	}

	// 一次 429 只走一次限流档，失败计数因此是 1。
	acc, err := f.st.GetAccount("ts1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	failure := acc.ModelFailures["typesafe/jev"]
	if failure.CooldownUntilMS == 0 || failure.FailStreak != 1 {
		t.Fatalf("账号 = %+v，期望一次 429 之后进冷却且 fail_streak=1", acc)
	}
}

// TestSystemOneUpstreamErrorPassedThrough 钉住实测的上游错误透传：上游的 422 校验失败正文
// 是 detail 数组，400 未知模型用的是 api_usage_error 一类的取值。这两段都不是我们自己的
// 错误体，不重新包装、也不把数组转成对象，状态码与字节都照旧给客户端。
func TestSystemOneUpstreamErrorPassedThrough(t *testing.T) {
	const (
		unprocessable = `{"detail":[{"type":"missing","loc":["body","questions"],"msg":"Field required"}]}`
		unknownModel  = `{"detail":{"error_type":"api_usage_error","message":"Unknown model: ghost"}}`
	)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "422 校验失败", status: http.StatusUnprocessableEntity, body: unprocessable},
		{name: "400 未知模型", status: http.StatusBadRequest, body: unknownModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer up.Close()
			addSystemOneProvider(t, f, "ts1", up.URL, "sk-ts", []string{"typesafe/jev"})
			srv := f.server(t)

			status, resp := postPath(t, srv, systemOnePath, systemOneRequestBody)
			if status != tc.status {
				t.Fatalf("状态 = %d，期望 %d，响应 %s", status, tc.status, resp)
			}
			if resp != tc.body {
				t.Fatalf("上游错误正文被改写了:\n得到 %s\n期望 %s", resp, tc.body)
			}

			// 原样透传的是发给客户端的那一份；请求记录里仍是一句解出来并脱敏的摘要。
			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || recs[0].OK || recs[0].HTTPStatus != tc.status || recs[0].Error == "" {
				t.Fatalf("请求记录 = %+v，期望一行失败且有错误摘要", recs)
			}
		})
	}
}

// TestSystemOneResponseModelNotRewritten 钉住实测的上游行为：请求 model 是 jev-latest，
// 响应里的 model 是上游自己解析出的 jev-1.14.0。响应方向不解析别名、也不改写字节
// 响应方向不解析别名、也不改写字节。
func TestSystemOneResponseModelNotRewritten(t *testing.T) {
	const reply = `{"model":"jev-1.14.0","answers":{"present":{"answer":"yes","probability":0.99}},` +
		`"usage":{"input_tokens":9,"output_tokens":2}}`
	f := newFixture(t)
	up := newSystemOneUpstream(t, reply)
	addSystemOneProvider(t, f, "ts1", up.srv.URL, "sk-ts", []string{"jev-latest"})
	srv := f.server(t)

	body := strings.Replace(systemOneRequestBody, `"typesafe/jev"`, `"jev-latest"`, 1)
	status, resp := postPath(t, srv, systemOnePath, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if resp != reply {
		t.Fatalf("响应被改动了:\n得到 %s\n期望 %s", resp, reply)
	}
	if model := decodedKeys(t, resp)["model"]; model != "jev-1.14.0" {
		t.Fatalf("响应里的 model = %v，期望上游原值 jev-1.14.0", model)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK || recs[0].ModelUsed != "jev-latest" ||
		recs[0].InTokens != 9 || recs[0].OutTokens != 2 {
		t.Fatalf("请求记录 = %+v，期望 model_used=jev-latest、用量 9/2", recs)
	}
}
