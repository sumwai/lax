package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// TestSandboxChatDialect 用真实沙箱跑一条对话请求（anthropic 入站 → chat 出站）：
// JS 在宿主编好的报文上加字段、改写响应正文，宿主再按入站协议编码回 anthropic。
func TestSandboxChatDialect(t *testing.T) {
	f := newFixture(t)
	var sent map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	t.Cleanup(up.Close)

	if err := f.st.AddProvider(store.Provider{
		Name: "river", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolChat,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "river1", "river", "sk-river", []string{"river-fast"}, 100)

	const dialectID = "com.example.hookchat/chat@1"
	bindDialect(t, f, "river", "river-fast", plugin.OpChatComplete, dialectID)

	sandbox, err := plugin.OpenSandbox(plugin.SandboxOptions{
		Manifest: plugin.Manifest{ID: "com.example.hookchat"},
		Root:     "testdata/hookchat",
		Entry:    "index.js",
		Trust:    plugin.TrustSandboxed,
	})
	if err != nil {
		t.Fatalf("打开沙箱失败: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Close() })

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.hookchat", plugin.HookDialect{
		IDValue:      dialectID,
		OperationVal: plugin.OpChatComplete,
		WireVal:      plugin.WireOpenAIChat,
		Sandbox:      sandbox,
	}); err != nil {
		t.Fatalf("登记沙箱方言失败: %v", err)
	}
	srv := serverWithDialect(t, f, reg)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"river-fast","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d", resp.StatusCode)
	}
	if len(payload.Content) != 1 || payload.Content[0].Text != "钩子:pong" {
		t.Fatalf("客户端正文 = %+v，期望方言改写过的文本", payload.Content)
	}

	if sent["enable_thinking"] != true {
		t.Fatalf("上游报文 = %v，期望带上方言补的 enable_thinking", sent)
	}
	if sent["model"] != "river-fast" {
		t.Fatalf("上游模型名 = %v", sent["model"])
	}

	recs := waitRequests(t, f.st, 1)
	if rec := recs[0]; rec.InTokens != 5 || rec.OutTokens != 2 || !rec.OK {
		t.Fatalf("用量/状态 = in %d / out %d / ok %v，期望 5 / 2 / true", rec.InTokens, rec.OutTokens, rec.OK)
	}
}
