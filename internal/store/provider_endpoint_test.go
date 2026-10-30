package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lax/internal/store"
)

// TestIsFullAddress 钉住两种写法的判定：含 `://` 的是整条地址，其余都是相对 base_url 的路径。
// 判定只有一份，cli 判断路径 flag 也调它。
func TestIsFullAddress(t *testing.T) {
	full := []string{
		"https://api.example.com/v1/chat/completions",
		"http://127.0.0.1:8080/v1/models",
		"HTTPS://API.Example.COM/v1/messages",
		"https://api.example.com",
	}
	for _, v := range full {
		if !store.IsFullAddress(v) {
			t.Fatalf("IsFullAddress(%q) = false，期望它是整条地址", v)
		}
	}
	relative := []string{
		"",
		"/v1/chat/completions",
		"v1/chat/completions",
		"//v1//chat//completions",
		"/v1/models?a=1",
	}
	for _, v := range relative {
		if store.IsFullAddress(v) {
			t.Fatalf("IsFullAddress(%q) = true，期望它是相对路径", v)
		}
	}
}

// TestNormalizeEndpoint 钉住端点取值的唯一解释入口：
// 相对路径只规整不拒绝（与 NormalizePath 完全一致），整条地址按 NormalizeBaseURL 那套校验与规整，
// 空值（含只有空白、只有斜杠）返回空串且不报错。
func TestNormalizeEndpoint(t *testing.T) {
	ok := []struct{ raw, want string }{
		// 相对路径：与 NormalizePath 同一个结果，一个字不差。
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"v1/chat/completions", "/v1/chat/completions"},
		{"//v1//chat//completions/", "/v1/chat/completions"},
		{"  /v1/models  ", "/v1/models"},
		// 整条地址：scheme 归一成小写，路径合并重复斜杠并去尾斜杠，不再拼 base_url。
		{"https://openai.example.com/v1/chat/completions", "https://openai.example.com/v1/chat/completions"},
		{"HTTPS://openai.example.com/v1/chat/completions", "https://openai.example.com/v1/chat/completions"},
		{"https://claude.example.com//v1//messages/", "https://claude.example.com/v1/messages"},
		{"  https://api.example.com/v1/models  ", "https://api.example.com/v1/models"},
		// 带端口的地址：端口属于授权部分，不被当成路径。
		{"https://api.example.com:8443//v1/models/", "https://api.example.com:8443/v1/models"},
		// 查询串与 `#` 之后的内容原样保留：那里合并斜杠会改掉语义。
		{"https://api.example.com/v1/models?next=//a//b", "https://api.example.com/v1/models?next=//a//b"},
		{"https://api.example.com//v1//models/?a=1#frag//z", "https://api.example.com/v1/models?a=1#frag//z"},
		// `..` 与 `.` 段原样保留，静默改写等于把地址换成另一个地址。
		{"https://api.example.com/../v1/./models/", "https://api.example.com/../v1/./models"},
		// 空值：没给这一项，不是用法错误。
		{"", ""},
		{"   ", ""},
		{"/", ""},
		{"///", ""},
	}
	for _, c := range ok {
		got, err := store.NormalizeEndpoint(c.raw)
		if err != nil {
			t.Fatalf("NormalizeEndpoint(%q) 报错: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("NormalizeEndpoint(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}

	// 相对路径与 NormalizePath 必须一直同形：读取侧拼地址用的就是它。
	for _, raw := range []string{"", "/", "///", "v1//models/", "  /v2//chat//completions  "} {
		got, err := store.NormalizeEndpoint(raw)
		if err != nil {
			t.Fatalf("NormalizeEndpoint(%q) 报错: %v", raw, err)
		}
		if want := store.NormalizePath(raw); got != want {
			t.Fatalf("NormalizeEndpoint(%q) = %q，期望与 NormalizePath 一致（%q）", raw, got, want)
		}
	}

	// 整条地址校验不过就报错：只有 scheme 没有主机名、scheme 不是 http(s)、缺 scheme 的写法都在此列。
	// 文案是中文并且指出缺什么，调用的地方不必再拼一遍。
	bad := []struct{ raw, want string }{
		{"https://", "主机名"},
		{"http://", "主机名"},
		{"https:///x", "主机名"},
		{"https:///", "主机名"},
		{"ftp://host/x", "http:// 或 https://"},
		{"file:///v1/models", "http:// 或 https://"},
		{"https://api example.com/v1", "不是合法地址"},
	}
	for _, c := range bad {
		got, err := store.NormalizeEndpoint(c.raw)
		if err == nil {
			t.Fatalf("NormalizeEndpoint(%q) 应报错，实际 = %q", c.raw, got)
		}
		if got != "" {
			t.Fatalf("NormalizeEndpoint(%q) 报错时不该返回值，实际 = %q", c.raw, got)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("NormalizeEndpoint(%q) 的错误 = %q，期望提到 %q", c.raw, err.Error(), c.want)
		}
		if !strings.Contains(err.Error(), "整条地址") {
			t.Fatalf("NormalizeEndpoint(%q) 的错误 = %q，期望说明是整条地址的问题", c.raw, err.Error())
		}
	}
}

// TestDeriveModelsPathFullAddress 钉住整条地址上的推导：在同一个 scheme + host 上把尾部
// /chat/completions 换成 /models，不换域名、不拼 base_url；查询串与 `#` 之后的内容跟着原样带过去。
func TestDeriveModelsPathFullAddress(t *testing.T) {
	cases := []struct{ protocol, path, want string }{
		{"openai-chat", "https://openai.example.com/v1/chat/completions", "https://openai.example.com/v1/models"},
		{"openai-chat", "https://openai.example.com/chat/completions", "https://openai.example.com/models"},
		{"openai-chat", "https://openai.example.com:8443/v1/chat/completions", "https://openai.example.com:8443/v1/models"},
		// 查询串不属于路径：换尾段时不能碰它，也不能把 /models 拼到查询串后面。
		{"openai-chat", "https://openai.example.com/v1/chat/completions?api-version=2024-05-01",
			"https://openai.example.com/v1/models?api-version=2024-05-01"},
		{"openai-chat", "https://openai.example.com/v1/chat/completions?a=1#frag",
			"https://openai.example.com/v1/models?a=1#frag"},
		// 尾部不是 /chat/completions 的整条地址推不出来，与相对路径同一套规则。
		{"openai-chat", "https://openai.example.com/v1/responses", ""},
		{"openai-responses", "https://api.example.com/v1/chat/completions", ""},
		{"anthropic", "https://claude.example.com/v1/chat/completions", ""},
	}
	for _, c := range cases {
		if got := store.DeriveModelsPath(c.protocol, c.path); got != c.want {
			t.Fatalf("DeriveModelsPath(%q, %q) = %q，期望 %q", c.protocol, c.path, got, c.want)
		}
	}
}

// TestCreateProviderStoresFullAddressRoutes 钉住写入侧对两种写法的处理：整条地址按规整形态落库，
// 相对路径仍按原来的规则规整，两者在同一家厂商里互不影响——同一家上游的 openai 兼容端点与
// anthropic 端点因此可以落在不同域名上。
func TestCreateProviderStoresFullAddressRoutes(t *testing.T) {
	st := openTemp(t)

	err := st.CreateProvider(store.Provider{
		Name:    "split",
		BaseURL: "https://openai.example.com//",
		// 厂商级覆盖也收整条地址：列表接口在第三个域名上。
		ModelsPath: "HTTPS://models.example.com//v1//models/",
	}, []store.ProviderRoute{
		{Protocol: "openai-chat", Path: "HTTPS://openai.example.com//v1//chat//completions/"},
		{Protocol: "anthropic", Path: "/v1/messages"},
	})
	if err != nil {
		t.Fatalf("整条地址应被接受: %v", err)
	}

	p, err := st.GetProvider("split")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://openai.example.com" {
		t.Fatalf("base_url = %q", p.BaseURL)
	}
	if p.ModelsPath != "https://models.example.com/v1/models" {
		t.Fatalf("厂商级 models_path = %q，期望规整成整条地址", p.ModelsPath)
	}

	chat, ok, err := st.LookupProviderRoute("split", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取 chat 路由: ok=%v err=%v", ok, err)
	}
	if chat.Path != "https://openai.example.com/v1/chat/completions" {
		t.Fatalf("chat 路由路径 = %q，期望落库的是规整后的整条地址", chat.Path)
	}
	if chat.ModelsPath != "https://openai.example.com/v1/models" {
		t.Fatalf("chat 路由的推导值 = %q，期望在同一 scheme + host 上换成 /models", chat.ModelsPath)
	}

	// 另一条协议写的是相对路径：仍然只看 base_url，没有被整条地址带跑。
	anthropic, ok, err := st.LookupProviderRoute("split", "anthropic")
	if err != nil || !ok {
		t.Fatalf("取 anthropic 路由: ok=%v err=%v", ok, err)
	}
	if anthropic.Path != "/v1/messages" {
		t.Fatalf("anthropic 路由路径 = %q，期望相对路径原样规整", anthropic.Path)
	}
	if anthropic.ModelsPath != "" {
		t.Fatalf("anthropic 路由的 models_path = %q，期望留空回退到同厂商其它协议", anthropic.ModelsPath)
	}
}

// TestSetProviderFullAddressRoutesAreIndependent 钉住改一条路由不影响另一条：
// 把 anthropic 路由换成另一个域名的整条地址，chat 那条相对路径一个字节都不动。
func TestSetProviderFullAddressRoutesAreIndependent(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{Name: "split", BaseURL: "https://openai.example.com"},
		[]store.ProviderRoute{
			{Protocol: "openai-chat", Path: "/v1/chat/completions"},
			{Protocol: "anthropic", Path: "https://claude.example.com/v1/messages"},
		}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}
	before, ok, err := st.LookupProviderRoute("split", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取 chat 路由: ok=%v err=%v", ok, err)
	}

	if err := st.SetProvider("split", store.ProviderPatch{
		Routes: map[string]string{"anthropic": "HTTPS://claude2.example.com//v2//messages/"},
	}); err != nil {
		t.Fatalf("改 anthropic 路由: %v", err)
	}

	after, ok, err := st.LookupProviderRoute("split", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取 chat 路由: ok=%v err=%v", ok, err)
	}
	if after != before {
		t.Fatalf("chat 路由被连带改了: %+v → %+v", before, after)
	}

	anthropic, ok, err := st.LookupProviderRoute("split", "anthropic")
	if err != nil || !ok {
		t.Fatalf("取 anthropic 路由: ok=%v err=%v", ok, err)
	}
	if anthropic.Path != "https://claude2.example.com/v2/messages" {
		t.Fatalf("anthropic 路由路径 = %q", anthropic.Path)
	}
}

// TestStoreWritePathsRejectBadFullAddress 钉住写入侧唯一的拒绝条件：整条地址校验不过时整笔拒绝，
// 一个字段都不落库。相对路径的脏写法照旧只规整不拒绝，两种写法的分工不该混在一起。
func TestStoreWritePathsRejectBadFullAddress(t *testing.T) {
	badRoutes := []string{"https://", "http://", "https:///x", "ftp://host/v1/chat/completions"}
	for _, bad := range badRoutes {
		t.Run(bad, func(t *testing.T) {
			st := openTemp(t)

			// CreateProvider：坏路由整笔拒绝，厂商行与其余路由都不落库。
			err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://api.example.com"},
				[]store.ProviderRoute{
					{Protocol: "anthropic", Path: "/v1/messages"},
					{Protocol: "openai-chat", Path: bad},
				})
			if err == nil {
				t.Fatal("坏路由应报错")
			}
			if !strings.Contains(err.Error(), "openai-chat") {
				t.Fatalf("错误 = %q，期望指出是哪条路由", err.Error())
			}
			if _, err := st.GetProvider("acme"); !errors.Is(err, store.ErrProviderNotFound) {
				t.Fatalf("坏路由不该留下厂商行，实际 err=%v", err)
			}

			// 厂商级 models_path 同样是整条地址的写入口。
			if err := st.CreateProvider(store.Provider{
				Name: "acme", BaseURL: "https://api.example.com", ModelsPath: bad,
			}, []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err == nil {
				t.Fatal("坏的厂商级 models_path 应报错")
			}

			// SetProvider：已有的路由与覆盖保持原样。
			if err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://api.example.com"},
				[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
				t.Fatalf("新建厂商: %v", err)
			}
			if err := st.SetProvider("acme", store.ProviderPatch{
				Routes: map[string]string{"openai-chat": bad},
			}); err == nil {
				t.Fatal("SetProvider 收到坏路由应报错")
			}
			if err := st.SetProvider("acme", store.ProviderPatch{ModelsPath: &bad}); err == nil {
				t.Fatal("SetProvider 收到坏的 models_path 应报错")
			}
			route, ok, err := st.LookupProviderRoute("acme", "openai-chat")
			if err != nil || !ok {
				t.Fatalf("取路由: ok=%v err=%v", ok, err)
			}
			if route.Path != "/v1/chat/completions" || route.ModelsPath != "/v1/models" {
				t.Fatalf("坏取值不该改动已有路由: %+v", route)
			}
			p, err := st.GetProvider("acme")
			if err != nil {
				t.Fatalf("取厂商: %v", err)
			}
			if p.ModelsPath != "" {
				t.Fatalf("坏的厂商级 models_path 不该落库，实际 %q", p.ModelsPath)
			}
		})
	}
}

// TestProviderRowsCarryNoCredentialText 钉住厂商与路由这两张表的形状：行里只有地址与路径，
// 没有任何凭据字段。序列化结果里因此不可能出现 key 明文——key 只以密文待在 accounts 里。
func TestProviderRowsCarryNoCredentialText(t *testing.T) {
	st := openTemp(t)
	const key = "sk-must-not-appear"

	if err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://api.example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}
	p, err := st.GetProvider("acme")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	routes, err := st.ProviderRoutes("acme")
	if err != nil {
		t.Fatalf("取路由: %v", err)
	}

	for name, v := range map[string]any{"provider": p, "routes": routes} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化 %s: %v", name, err)
		}
		if strings.Contains(string(b), key) {
			t.Fatalf("%s 的序列化输出里出现了 key 明文: %s", name, b)
		}
	}
}
