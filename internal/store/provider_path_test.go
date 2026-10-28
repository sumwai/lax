package store_test

import (
	"strings"
	"testing"

	"switchboard/internal/store"
)

// TestNormalizePath 钉住路径规整的唯一规则：补一个前导斜杠、合并连续斜杠、去尾斜杠，
// 空白与只有斜杠的输入视为空串。路径写法是抄上游文档时的差异，不是用法错误。
func TestNormalizePath(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"v1/chat/completions", "/v1/chat/completions"},
		{"//v1//chat//completions", "/v1/chat/completions"},
		{"/v1/chat/completions/", "/v1/chat/completions"},
		{"v1/models/", "/v1/models"},
		{"  v1/models  ", "/v1/models"},
		// base_url 里真实存在的前缀原样保留，只合并重复斜杠。
		{"/api/paas/v4/chat/completions", "/api/paas/v4/chat/completions"},
		{"//api//paas//v4//chat/completions", "/api/paas/v4/chat/completions"},
		{"", ""},
		{"/", ""},
		{"///", ""},
		{"   ", ""},
		{"chat/completions", "/chat/completions"},
	}
	for _, c := range cases {
		if got := store.NormalizePath(c.raw); got != c.want {
			t.Fatalf("NormalizePath(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

// TestNormalizeBaseURL 钉住地址规整：scheme 之后合并重复斜杠、去掉末尾斜杠，
// 真实存在的前缀不改；协议头不对仍然是错误，这一条与「不拒绝输入」无关——
// 没有 scheme 的地址拼不出可用的请求地址。
func TestNormalizeBaseURL(t *testing.T) {
	ok := []struct{ raw, want string }{
		{"https://api.example.com", "https://api.example.com"},
		{"https://api.example.com/", "https://api.example.com"},
		{"https://api.example.com//", "https://api.example.com"},
		{"https://api.example.com/api/paas/v4/", "https://api.example.com/api/paas/v4"},
		{"https://api.example.com//api//paas//v4//", "https://api.example.com/api/paas/v4"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, c := range ok {
		got, err := store.NormalizeBaseURL(c.raw)
		if err != nil {
			t.Fatalf("NormalizeBaseURL(%q) 报错: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("NormalizeBaseURL(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}

	for _, raw := range []string{"", "   ", "api.example.com", "ftp://api.example.com"} {
		if _, err := store.NormalizeBaseURL(raw); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) 应报错", raw)
		}
	}
}

// TestCreateProviderNormalizesPaths 确认写入侧只规整不拒绝：没有前导斜杠、重复斜杠、
// 末尾多一个斜杠的路径都照收，落库时是同一个规整结果，模型列表路径的推导值跟着规整后的路径走。
func TestCreateProviderNormalizesPaths(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://api.example.com//api//paas//v4/"},
		[]store.ProviderRoute{
			{Protocol: "openai-chat", Path: "//chat//completions/"},
			{Protocol: "anthropic", Path: "messages"},
		}); err != nil {
		t.Fatalf("不规范路径应被接受: %v", err)
	}

	p, err := st.GetProvider("acme")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://api.example.com/api/paas/v4" {
		t.Fatalf("base_url = %q，期望合并重复斜杠并去掉末尾斜杠", p.BaseURL)
	}

	chat, ok, err := st.LookupProviderRoute("acme", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取 chat 路由: ok=%v err=%v", ok, err)
	}
	if chat.Path != "/chat/completions" {
		t.Fatalf("chat 路由路径 = %q，期望 /chat/completions", chat.Path)
	}
	if chat.ModelsPath != "/models" {
		t.Fatalf("chat 路由的推导值 = %q，期望按规整后的路径推出 /models", chat.ModelsPath)
	}

	anthropic, ok, err := st.LookupProviderRoute("acme", "anthropic")
	if err != nil || !ok {
		t.Fatalf("取 anthropic 路由: ok=%v err=%v", ok, err)
	}
	if anthropic.Path != "/messages" {
		t.Fatalf("anthropic 路由路径 = %q，期望 /messages", anthropic.Path)
	}

	// 完整地址在 scheme 之外不出现 //，base_url 里真实存在的前缀原样保留。
	joined, err := store.NormalizeBaseURL(p.BaseURL)
	if err != nil {
		t.Fatalf("规整地址: %v", err)
	}
	full := joined + store.NormalizePath(chat.Path)
	if strings.Contains(strings.TrimPrefix(full, "https://"), "//") {
		t.Fatalf("拼出的完整地址含重复斜杠: %q", full)
	}
	if full != "https://api.example.com/api/paas/v4/chat/completions" {
		t.Fatalf("完整地址 = %q，前缀应原样保留", full)
	}
}

// TestSetProviderNormalizesPaths 确认改路由与改厂商级模型列表路径同样只规整不拒绝，
// 并且规整发生在写库之前：库里存的永远是规整形态，读取侧不必再猜。
func TestSetProviderNormalizesPaths(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://api.example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}

	if err := st.SetProvider("acme", store.ProviderPatch{
		Routes:     map[string]string{"openai-chat": "v2//chat//completions/"},
		ModelsPath: strPtr("//models//v2/"),
	}); err != nil {
		t.Fatalf("不规范路径应被接受: %v", err)
	}

	route, ok, err := st.LookupProviderRoute("acme", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "/v2/chat/completions" {
		t.Fatalf("路由路径 = %q，期望 /v2/chat/completions", route.Path)
	}
	if route.ModelsPath != "/v2/models" {
		t.Fatalf("推导值 = %q，期望跟着新路径换成 /v2/models", route.ModelsPath)
	}
	p, err := st.GetProvider("acme")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.ModelsPath != "/models/v2" {
		t.Fatalf("厂商级模型列表路径 = %q，期望 /models/v2", p.ModelsPath)
	}
}

// strPtr 是取字段指针的小工具，ProviderPatch 用 nil 表示「这一项不动」。
func strPtr(v string) *string { return &v }
