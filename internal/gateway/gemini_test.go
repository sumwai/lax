package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// geminiRoute 是 gemini 的路由写法：相对 base_url 的路径，模型名用 {model} 占位符
// （DESIGN 5.9）。gemini 的模型名在 URL 路径里，不在请求体里。
const geminiRoute = "/v1beta/models/{model}:generateContent"

// geminiReply 是一个最小的 gemini 非流式响应：一段正文，用量 12/7，其中 4 个命中缓存。
const geminiReply = `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":7,"totalTokenCount":19,"cachedContentTokenCount":4}}`

// geminiCalls 记下每次上游调用的路径与凭据头。callRecorder 存不下 gemini 的凭据头，
// 这里单独记一份，断言时才不必猜。
type geminiCalls struct {
	uri           []string
	googAPIKey    []string
	authorization []string
	apiKey        []string
	anthropicVer  []string
	body          []string
}

func (g *geminiCalls) add(uri, googKey, auth, apiKey, anthropicVersion, body string) {
	g.uri = append(g.uri, uri)
	g.googAPIKey = append(g.googAPIKey, googKey)
	g.authorization = append(g.authorization, auth)
	g.apiKey = append(g.apiKey, apiKey)
	g.anthropicVer = append(g.anthropicVer, anthropicVersion)
	g.body = append(g.body, body)
}

// TestForwardChatToGemini 钉住 chat 入站 → gemini 出站的整条非流式链路：路径里带模型名、
// 凭据走 x-goog-api-key、请求体是 gemini 的形状（没有 model 字段）、响应转回 chat 且带用量。
func TestForwardChatToGemini(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-chat"

	var got geminiCalls
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.add(r.URL.RequestURI(), r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization"),
			r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiReply)
	}))
	t.Cleanup(up.Close)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.URL},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, key, []string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, body := postTo(t, srv, `{"model":"gemini-2.5-flash","max_tokens":32,`+
		`"messages":[{"role":"system","content":"只说一句"},{"role":"user","content":"hi"}]}`,
		translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	// 上游：模型名在路径里，占位符已经换掉；凭据是 x-goog-api-key，不发另两种。
	if len(got.uri) != 1 {
		t.Fatalf("上游调用数 = %d，期望 1：%v", len(got.uri), got.uri)
	}
	if got.uri[0] != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("上游 RequestURI = %q", got.uri[0])
	}
	if got.googAPIKey[0] != key {
		t.Fatalf("x-goog-api-key = %q", got.googAPIKey[0])
	}
	if got.authorization[0] != "" || got.apiKey[0] != "" || got.anthropicVer[0] != "" {
		t.Fatalf("gemini 不该发其它凭据头：Authorization %q，x-api-key %q，anthropic-version %q",
			got.authorization[0], got.apiKey[0], got.anthropicVer[0])
	}

	// 请求体：gemini 的生成配置与 systemInstruction，且没有 model 字段。
	var sent map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got.body[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，原文 %s", err, got.body[0])
	}
	if _, ok := sent["model"]; ok {
		t.Fatalf("gemini 请求体里不该有 model 字段: %s", got.body[0])
	}
	var req translate.GeminiRequest
	if err := json.Unmarshal([]byte(got.body[0]), &req); err != nil {
		t.Fatalf("上游请求体不是 gemini 形状: %v，原文 %s", err, got.body[0])
	}
	if req.SystemInstruction == nil || len(req.SystemInstruction.Parts) != 1 ||
		req.SystemInstruction.Parts[0].Text != "只说一句" {
		t.Fatalf("systemInstruction = %+v", req.SystemInstruction)
	}
	if len(req.Contents) != 1 || req.Contents[0].Role != "user" ||
		len(req.Contents[0].Parts) != 1 || req.Contents[0].Parts[0].Text != "hi" {
		t.Fatalf("contents = %+v", req.Contents)
	}
	if req.GenerationConfig == nil || req.GenerationConfig.MaxOutputTokens != 32 {
		t.Fatalf("generationConfig = %+v", req.GenerationConfig)
	}

	// 客户端拿到的是 chat 的形状，用量从 gemini 的 usageMetadata 转过来。
	res, err := translate.DecodeResponse(translate.ProtocolChat, []byte(body), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("响应不是 chat 形状: %v，原文 %s", err, body)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Text != "pong" {
		t.Fatalf("响应内容 = %+v", res.Blocks)
	}
	if res.StopReason != translate.StopEndTurn {
		t.Fatalf("结束原因 = %q", res.StopReason)
	}
	if res.Usage.In != 12 || res.Usage.Out != 7 || res.Usage.CacheRead != 4 {
		t.Fatalf("用量 = %+v，期望 12/7/4", res.Usage)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolChat || recs[0].Outbound != translate.ProtocolGemini {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 12 || recs[0].OutTokens != 7 || recs[0].CacheRead != 4 {
		t.Fatalf("记录的用量 = %d/%d/%d", recs[0].InTokens, recs[0].OutTokens, recs[0].CacheRead)
	}
}

// TestForwardAnthropicToGemini 钉住 anthropic 入站 → gemini 出站：system 进 systemInstruction，
// 工具定义进 functionDeclarations，响应转回 anthropic 并带上 stop_reason 与用量。
func TestForwardAnthropicToGemini(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-anthropic"

	var got geminiCalls
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.add(r.URL.RequestURI(), r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization"),
			r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiReply)
	}))
	t.Cleanup(up.Close)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.URL},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, key, []string{"gemini-2.5-flash"})
	srv := f.server(t)

	body := `{"model":"gemini-2.5-flash","max_tokens":16,"system":"只说一句",` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"weather","description":"查天气","input_schema":{"type":"object"}}]}`
	status, resp := postMessages(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	if got.uri[0] != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("上游 RequestURI = %q", got.uri[0])
	}
	if got.googAPIKey[0] != key || got.apiKey[0] != "" || got.anthropicVer[0] != "" {
		t.Fatalf("gemini 凭据头 = x-goog-api-key %q，x-api-key %q，anthropic-version %q",
			got.googAPIKey[0], got.apiKey[0], got.anthropicVer[0])
	}
	var req translate.GeminiRequest
	if err := json.Unmarshal([]byte(got.body[0]), &req); err != nil {
		t.Fatalf("上游请求体不是 gemini 形状: %v，原文 %s", err, got.body[0])
	}
	if len(req.Tools) != 1 || len(req.Tools[0].FunctionDeclarations) != 1 ||
		req.Tools[0].FunctionDeclarations[0].Name != "weather" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.SystemInstruction == nil || req.SystemInstruction.Parts[0].Text != "只说一句" {
		t.Fatalf("systemInstruction = %+v", req.SystemInstruction)
	}

	var out translate.AnthropicResponse
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是 anthropic 形状: %v，原文 %s", err, resp)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "pong" || out.StopReason != "end_turn" {
		t.Fatalf("响应 = %+v", out)
	}
	if out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 7 {
		t.Fatalf("用量 = %+v", out.Usage)
	}
}

// TestGeminiRoutePlaceholderInFullAddress 钉住两种写法与占位符共存：路由写整条地址时
// 仍然直接访问那个地址、不再拼 base_url，占位符同样被替换；账号 base_url 那台一次都不该收到请求。
func TestGeminiRoutePlaceholderInFullAddress(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-full"

	up := newURIUpstream(t, geminiReply)
	decoy := newURIUpstream(t, geminiReply)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: decoy.url},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: up.url + geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, decoy.url, key, []string{"gemini-2.5-flash"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postTo(t, srv, `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`,
		translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if up.hits != 1 {
		t.Fatalf("整条地址那个上游收到 %d 次请求，期望 1 次", up.hits)
	}
	if up.uri != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("上游 RequestURI = %q", up.uri)
	}
	if decoy.hits != 0 {
		t.Fatalf("账号 base_url 那个域名收到 %d 次请求，整条地址路由不该碰它", decoy.hits)
	}
	if up.apiKey != "" || up.bearer != "" {
		t.Fatalf("gemini 不该发 x-api-key 或 Authorization，实际 %q / %q", up.apiKey, up.bearer)
	}
}

// TestGeminiModelNameIsPathEscaped 钉住占位符的替换规则：模型名按路径段转义，
// 含 `/` 的名字因此仍是路径里的一个段，不会被拆开。
func TestGeminiModelNameIsPathEscaped(t *testing.T) {
	f := newFixture(t)
	up := newURIUpstream(t, geminiReply)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.url},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.url, "sk-gemini-escape", []string{"org/gemini"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postTo(t, srv, `{"model":"org/gemini","messages":[{"role":"user","content":"hi"}]}`,
		translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if up.uri != "/v1beta/models/org%2Fgemini:generateContent" {
		t.Fatalf("上游 RequestURI = %q，模型名应当按路径段转义", up.uri)
	}
}

// TestRouteWithoutPlaceholderUnchanged 钉住占位符只在写了它的时候生效：
// 没有 {model} 的路由行为与此前完全一样。
func TestRouteWithoutPlaceholderUnchanged(t *testing.T) {
	f := newFixture(t)
	up := newURIUpstream(t, upstreamBodies[translate.ProtocolChat])

	if err := f.st.CreateProvider(store.Provider{Name: "ctest", BaseURL: up.url},
		[]store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "c1", "ctest", translate.ProtocolChat, up.url, "sk-chat-plain", []string{"m1"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postTo(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if up.uri != "/v1/chat/completions" {
		t.Fatalf("上游 RequestURI = %q，不含占位符的路由不该被改写", up.uri)
	}
}

// TestGeminiOutboundRejectsStream 钉住第一步的边界：gemini 出站只做非流式。
// 流式请求在写响应之前就报错，不打上游、不把这个账号打入冷却（不是上游的错）。
func TestGeminiOutboundRejectsStream(t *testing.T) {
	f := newFixture(t)
	up := newURIUpstream(t, geminiReply)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.url},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	acc := f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.url, "sk-gemini-stream", []string{"gemini-2.5-flash"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postTo(t, srv, `{"model":"gemini-2.5-flash","stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolChat)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 %s", status, body)
	}
	if !strings.Contains(body, "流式") {
		t.Fatalf("错误文案应说明流式还没实现: %s", body)
	}
	if up.hits != 0 {
		t.Fatalf("流式请求不该打到上游，实际 %d 次", up.hits)
	}

	after, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if after.CooldownUntil != 0 || !after.Enabled || after.FailStreak != 0 {
		t.Fatalf("本机不支持这种组合不该惩罚账号: %+v", after)
	}
	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("没有上游调用就没有请求记录，实际 %+v", recs)
	}
}

// TestProbeGeminiRoute 钉住探活也能用 gemini：模型名同样在路径里，用量从 usageMetadata 读。
func TestProbeGeminiRoute(t *testing.T) {
	f := newFixture(t)
	up := newURIUpstream(t, geminiReply)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.url},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	acc := f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.url, "sk-gemini-probe", []string{"gemini-2.5-flash"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})

	res, err := gs.ProbeModel(t.Context(), acc, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelUsable || res.Protocol != translate.ProtocolGemini {
		t.Fatalf("探活结论 = %+v", res)
	}
	if up.uri != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("探活请求的 RequestURI = %q", up.uri)
	}
	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].Outbound != translate.ProtocolGemini {
		t.Fatalf("探活记录 = %+v", recs)
	}
	if recs[0].InTokens != 12 || recs[0].OutTokens != 7 {
		t.Fatalf("探活记录的用量 = %d/%d", recs[0].InTokens, recs[0].OutTokens)
	}
}
