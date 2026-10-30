package store_test

import (
	"strings"
	"testing"

	"lax/internal/store"
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
		// gemini 的模型名占位符不是斜杠：花括号与冒号原样保留，转发时才替换（DESIGN 5.9）。
		{"/v1beta/models/{model}:generateContent", "/v1beta/models/{model}:generateContent"},
		{"v1beta//models//{model}:generateContent/", "/v1beta/models/{model}:generateContent"},
	}
	for _, c := range cases {
		if got := store.NormalizePath(c.raw); got != c.want {
			t.Fatalf("NormalizePath(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

// TestNormalizeBaseURL 钉住地址规整：scheme 只认 http、https（大小写不敏感，归一成小写），
// 缺主机名是错误（`https://` 拼不出可用地址），规整只作用在路径部分——授权部分、查询串
// 与 `#` 之后的内容原样保留，`..` 与 `.` 段也不静默改写。
// 前缀不对仍然是错误，这一条与「不拒绝输入」无关：没有 scheme 的地址拼不出可用的请求地址。
func TestNormalizeBaseURL(t *testing.T) {
	ok := []struct{ raw, want string }{
		{"https://api.example.com", "https://api.example.com"},
		{"https://api.example.com/", "https://api.example.com"},
		{"https://api.example.com//", "https://api.example.com"},
		{"https://api.example.com/api/paas/v4/", "https://api.example.com/api/paas/v4"},
		{"https://api.example.com//api//paas//v4//", "https://api.example.com/api/paas/v4"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		// 带端口的地址：端口属于授权部分，不能被当成路径合并或去尾。
		{"https://api.example.com:8443//v1/", "https://api.example.com:8443/v1"},
		// scheme 大小写不敏感，落库时归一成小写；主机名的大小写不属于规整范围。
		{"HTTPS://api.example.com/", "https://api.example.com"},
		{"HTTPS://API.Example.COM/v1/", "https://API.Example.COM/v1"},
		{"  Http://127.0.0.1:9000  ", "http://127.0.0.1:9000"},
		// 查询串里的连写斜杠是值的一部分，不合并；带查询串时路径照旧去尾斜杠。
		{"https://api.example.com/v1?next=//a//b", "https://api.example.com/v1?next=//a//b"},
		{"https://api.example.com//v1//?a=1", "https://api.example.com/v1?a=1"},
		// `#` 之后的内容（含 //）原样保留。
		{"https://api.example.com/a//b#frag//z", "https://api.example.com/a/b#frag//z"},
		// `..` 与 `.` 段原样保留：改写会把一个地址换成另一个地址。
		{"https://api.example.com/../v1/./x/", "https://api.example.com/../v1/./x"},
		// 用户信息与端口一起照拄，只有路径被规整。
		{"https://user:pw@api.example.com:8443//v1//", "https://user:pw@api.example.com:8443/v1"},
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

	for _, raw := range []string{"", "   ", "api.example.com", "ftp://api.example.com", "https://api example.com"} {
		if _, err := store.NormalizeBaseURL(raw); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) 应报错", raw)
		}
	}

	// 只有 scheme 没有主机名的写法曾经被规整成 `https:`并落库，拼出来的地址是畸形的：
	// 这一条必须报错，且文案要说清缺的是主机名。
	for _, raw := range []string{"https://", "https:///", "https:///v1", "http://"} {
		_, err := store.NormalizeBaseURL(raw)
		if err == nil {
			t.Fatalf("NormalizeBaseURL(%q) 应报错", raw)
		}
		if !strings.Contains(err.Error(), "主机名") {
			t.Fatalf("NormalizeBaseURL(%q) 的错误 = %q，期望指出缺主机名", raw, err)
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

// TestGeminiRouteKeepsModelPlaceholder 钉住 gemini 路由的写法：`{model}` 占位符在写入与读取
// 两侧都原样保留（相对路径与整条地址都一样），路径规整只合并斜杠、不吃掉花括号与冒号；
// gemini 也没有模型列表推导，`models_path` 留空表示回退到同厂商其它协议（DESIGN 5.9）。
func TestGeminiRouteKeepsModelPlaceholder(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{Name: "gem", BaseURL: "https://generativelanguage.googleapis.com"},
		[]store.ProviderRoute{
			{Protocol: "gemini", Path: "v1beta//models//{model}:generateContent"},
			{Protocol: "openai-chat", Path: "/v1/chat/completions"},
		}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}
	route, ok, err := st.LookupProviderRoute("gem", "gemini")
	if err != nil || !ok {
		t.Fatalf("取 gemini 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "/v1beta/models/{model}:generateContent" {
		t.Fatalf("gemini 路由路径 = %q，占位符与冒号应当原样保留", route.Path)
	}
	if route.ModelsPath != "" {
		t.Fatalf("gemini 不该有推导出来的模型列表路径，实际 %q", route.ModelsPath)
	}

	// 整条地址同样保留占位符：规整只作用在斜杠上。
	if err := st.SetProvider("gem", store.ProviderPatch{Routes: map[string]string{
		"gemini": "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent/",
	}}); err != nil {
		t.Fatalf("改 gemini 路由: %v", err)
	}
	route, ok, err = st.LookupProviderRoute("gem", "gemini")
	if err != nil || !ok {
		t.Fatalf("取 gemini 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent" {
		t.Fatalf("整条地址里的占位符 = %q", route.Path)
	}

	// 线协议的判定只有一份，gemini 在里面（只作出站），不认识的取值仍然是错的。
	if !store.IsWireProtocol("gemini") {
		t.Fatal("gemini 是线协议：账号的 --protocol 与厂商路由都要认它")
	}
	if store.IsWireProtocol("cohere") {
		t.Fatal("不认识的取值不该被当成线协议")
	}
}
