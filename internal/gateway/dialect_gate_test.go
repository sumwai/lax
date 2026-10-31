package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// 对话请求绑到插件方言之后的两种边界：绑了却没注册要明确拒绝（不能装成「没绑定」后
// 悄悄走内置编解码），绑宿主方言则照旧由宿主执行。

func chatBindingFixture(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)
	if err := f.st.AddProvider(store.Provider{
		Name: "river", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolChat,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "river1", "river", "sk-river", []string{"river-fast"}, 100)
	return f, up
}

func TestChatBoundToUnregisteredDialectRejected(t *testing.T) {
	f, _ := chatBindingFixture(t)
	if err := f.st.SetModelOperation(store.ModelOperation{
		Provider: "river", Model: "river-fast", Operation: plugin.OpChatComplete,
		DialectID: "com.example.river/chat@1", Source: "plugin",
	}); err != nil {
		t.Fatalf("写绑定: %v", err)
	}
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/chat/completions",
		`{"model":"river-fast","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	for _, want := range []string{"没有注册", "不会回落"} {
		if !strings.Contains(body, want) {
			t.Fatalf("文案没说清方言缺失：%s", body)
		}
	}
}

func TestChatBoundToHostDialectAllowed(t *testing.T) {
	f, _ := chatBindingFixture(t)
	if err := f.st.SetModelOperation(store.ModelOperation{
		Provider: "river", Model: "river-fast", Operation: plugin.OpChatComplete,
		DialectID: plugin.DialectHostOpenAIChat, Source: "plugin",
	}); err != nil {
		t.Fatalf("写绑定: %v", err)
	}
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/chat/completions",
		`{"model":"river-fast","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200，响应 = %s", status, body)
	}
	if !strings.Contains(body, `"ok"`) {
		t.Fatalf("响应 = %s，期望上游正文", body)
	}
}
