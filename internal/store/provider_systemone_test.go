package store_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"lax/internal/secret"
	"lax/internal/store"
	"lax/internal/translate"
)

// systemOnePath 是决策协议的出站路径：入站与出站同一个路径。
const systemOnePath = "/v1/systemone"

// TestWireProtocolsOrder 钉住线协议清单与顺序：anthropic → openai-chat → openai-responses
// → gemini → systemone → openai-embeddings。错误文案、CLI 的 flag 与默认协议的候选都从这一份清单取，
// 顺序或成员错了，`--default-protocol` 的提示会跟着错。
func TestWireProtocolsOrder(t *testing.T) {
	want := []string{"anthropic", "openai-chat", "openai-responses", "gemini", "systemone", "openai-embeddings", "openai-speech", "openai-transcriptions"}
	got := store.WireProtocols()
	if len(got) != len(want) {
		t.Fatalf("WireProtocols() = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WireProtocols()[%d] = %q，期望 %q（整份：%v）", i, got[i], want[i], got)
		}
	}

	// 协议名只有一份：translate 与 store 认的是同一个字符串。
	if translate.ProtocolSystemOne != "systemone" {
		t.Fatalf("translate.ProtocolSystemOne = %q，期望 systemone", translate.ProtocolSystemOne)
	}
	if !store.IsWireProtocol(translate.ProtocolSystemOne) {
		t.Fatal("systemone 是线协议：厂商路由、默认协议与协议覆盖都要认它")
	}
	if store.IsWireProtocol("decide") {
		t.Fatal("不认识的取值不该被当成线协议")
	}

	// 错误文案里能查到 systemone：允许的取值由 wireProtocolNames 拼出来，不另写一份。
	if err := openTemp(t).CreateProvider(store.Provider{
		Name: "bad", BaseURL: "https://bad.example.com",
	}, []store.ProviderRoute{{Protocol: "decide", Path: systemOnePath}}); err == nil {
		t.Fatal("不认识的协议应报错")
	} else if !strings.Contains(err.Error(), "systemone") {
		t.Fatalf("协议清单的错误文案里应列出 systemone，实际 %v", err)
	}
}

// TestMissingDefaultRouteSuggestsCLIFlag 钉住错误文案里的下一步命令：
// 线协议全名与 CLI flag 不同的几条要映射回去，不能建议 --openai-speech 这类不存在的参数。
func TestMissingDefaultRouteSuggestsCLIFlag(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{Name: "flags", BaseURL: "https://api.example.com"},
		[]store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	for _, c := range []struct {
		protocol string
		flag     string
	}{
		{translate.ProtocolAnthropic, "--anthropic"},
		{translate.ProtocolResponses, "--responses"},
		{translate.ProtocolGemini, "--gemini"},
		{translate.ProtocolSystemOne, "--systemone"},
		{translate.ProtocolEmbeddings, "--embeddings"},
		{translate.ProtocolSpeech, "--speech"},
		{translate.ProtocolTranscriptions, "--transcriptions"},
	} {
		def := c.protocol
		err := st.SetProvider("flags", store.ProviderPatch{DefaultProtocol: &def})
		if err == nil || !strings.Contains(err.Error(), c.flag) {
			t.Fatalf("默认协议 %s 的错误文案应给出 %s，实际 %v", c.protocol, c.flag, err)
		}
	}
}

// TestProviderSystemOneRouteRoundTrip 钉住 systemone 路由的写入与读回：
// `provider add/set --systemone <路径>` 落库之后能原样读回，路径按同一套规则规整，
// 且这条路由不推导 models_path（决策端点没有列表接口）。
func TestProviderSystemOneRouteRoundTrip(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{
		Name: "typesafe", BaseURL: "https://api.typesafe.ai",
	}, []store.ProviderRoute{{Protocol: translate.ProtocolSystemOne, Path: systemOnePath}}); err != nil {
		t.Fatalf("provider add --systemone: %v", err)
	}

	route, ok, err := st.LookupProviderRoute("typesafe", translate.ProtocolSystemOne)
	if err != nil || !ok {
		t.Fatalf("取 systemone 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != systemOnePath {
		t.Fatalf("systemone 路由的 path = %q，期望 %q", route.Path, systemOnePath)
	}
	if route.ModelsPath != "" {
		t.Fatalf("systemone 路由不该推导 models_path，实际 %q", route.ModelsPath)
	}
	// 推导规则本身也钉住：只有 openai-chat 推 /models，gemini 与 systemone 留空。
	for _, protocol := range []string{translate.ProtocolSystemOne, translate.ProtocolGemini} {
		if got := store.DeriveModelsPath(protocol, systemOnePath); got != "" {
			t.Fatalf("DeriveModelsPath(%q, %q) = %q，期望空串", protocol, systemOnePath, got)
		}
	}

	// `provider set --systemone`：相对路径按同一套规则规整（合并斜杠、去尾斜杠）。
	if err := st.SetProvider("typesafe", store.ProviderPatch{
		Routes: map[string]string{translate.ProtocolSystemOne: "v1//systemone/"},
	}); err != nil {
		t.Fatalf("provider set --systemone 相对路径: %v", err)
	}
	route, ok, err = st.LookupProviderRoute("typesafe", translate.ProtocolSystemOne)
	if err != nil || !ok {
		t.Fatalf("改完取 systemone 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != systemOnePath {
		t.Fatalf("规整后的 path = %q，期望 %q", route.Path, systemOnePath)
	}
	if route.ModelsPath != "" {
		t.Fatalf("改路径不该推出 models_path，实际 %q", route.ModelsPath)
	}

	// 整条地址也收，并且仍然不推 models_path。
	const full = "https://api.typesafe.ai/v1/systemone"
	if err := st.SetProvider("typesafe", store.ProviderPatch{
		Routes: map[string]string{translate.ProtocolSystemOne: full + "/"},
	}); err != nil {
		t.Fatalf("provider set --systemone 整条地址: %v", err)
	}
	route, ok, err = st.LookupProviderRoute("typesafe", translate.ProtocolSystemOne)
	if err != nil || !ok {
		t.Fatalf("取整条地址的 systemone 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != full || route.ModelsPath != "" {
		t.Fatalf("整条地址的 systemone 路由 = %+v，期望 path=%q 且 models_path 为空", route, full)
	}
}

// TestSystemOneDefaultProtocolRequiresDeclaredRoute 钉住 `--default-protocol systemone`：
// 声明了这条路由时通过并落库，没声明时两个写入口都拒绝，且拒绝时一个字段都不落库。
func TestSystemOneDefaultProtocolRequiresDeclaredRoute(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{
		Name: "typesafe", BaseURL: "https://api.typesafe.ai", DefaultProtocol: translate.ProtocolSystemOne,
	}, []store.ProviderRoute{{Protocol: translate.ProtocolSystemOne, Path: systemOnePath}}); err != nil {
		t.Fatalf("provider add --default-protocol systemone: %v", err)
	}
	p, err := st.GetProvider("typesafe")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.DefaultProtocol != translate.ProtocolSystemOne {
		t.Fatalf("default_protocol = %q，期望 systemone", p.DefaultProtocol)
	}
	if got, err := st.EffectiveProviderProtocol("typesafe"); err != nil {
		t.Fatalf("取有效默认协议: %v", err)
	} else if got != translate.ProtocolSystemOne {
		t.Fatalf("有效默认协议 = %q，期望 systemone", got)
	}

	// 只声明 chat 路由：新建时带 systemone 作为默认协议要整笔拒绝。
	if err := st.CreateProvider(store.Provider{
		Name: "chatonly", BaseURL: "https://chat.example.com", DefaultProtocol: translate.ProtocolSystemOne,
	}, []store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}); err == nil {
		t.Fatal("没声明 systemone 路由时默认协议应被拒")
	}
	if _, err := st.GetProvider("chatonly"); err == nil {
		t.Fatal("被拒绝的新建不该留下厂商行")
	}

	// 已经有这家厂商：用 provider set 改默认协议，指不到路由时同样拒绝，
	// 文案给出 `--systemone` 这条下一步命令（protocolFlagName 的取值）。
	if err := st.CreateProvider(store.Provider{
		Name: "chatonly", BaseURL: "https://chat.example.com",
	}, []store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建 chatonly: %v", err)
	}
	def := translate.ProtocolSystemOne
	err = st.SetProvider("chatonly", store.ProviderPatch{DefaultProtocol: &def})
	if err == nil {
		t.Fatal("没声明 systemone 路由时 provider set --default-protocol systemone 应被拒")
	}
	if !strings.Contains(err.Error(), "--systemone") || !strings.Contains(err.Error(), "路由不存在") {
		t.Fatalf("拒绝文案应给出 --systemone 与「路由不存在」，实际 %v", err)
	}
	p, err = st.GetProvider("chatonly")
	if err != nil {
		t.Fatalf("取 chatonly: %v", err)
	}
	if p.DefaultProtocol != "" {
		t.Fatalf("被拒绝的写入改了 default_protocol：%q", p.DefaultProtocol)
	}

	// 补上 systemone 路由之后同一笔写入就通过。
	if err := st.SetProvider("chatonly", store.ProviderPatch{
		Routes: map[string]string{translate.ProtocolSystemOne: systemOnePath},
	}); err != nil {
		t.Fatalf("补 systemone 路由: %v", err)
	}
	if err := st.SetProvider("chatonly", store.ProviderPatch{DefaultProtocol: &def}); err != nil {
		t.Fatalf("声明路由之后 provider set --default-protocol systemone: %v", err)
	}
	if p, err = st.GetProvider("chatonly"); err != nil {
		t.Fatalf("取 chatonly: %v", err)
	}
	if p.DefaultProtocol != translate.ProtocolSystemOne {
		t.Fatalf("default_protocol = %q，期望 systemone", p.DefaultProtocol)
	}
}

// TestEffectiveProtocolSystemOneFromCatalog 钉住有效协议在协议集合里的顺序：
// 厂商默认协议在集合里就赢；否则 chat → responses → anthropic → gemini → systemone。
// systemone 排在最后，所以厂商同时声明 chat 与 systemone 时决策协议不会被选成默认。
func TestEffectiveProtocolSystemOneFromCatalog(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{
		Name: "proxy", BaseURL: "https://api.commandcode.ai",
	}, []store.ProviderRoute{
		{Protocol: translate.ProtocolChat, Path: "/provider/v1/chat/completions"},
		{Protocol: translate.ProtocolSystemOne, Path: "/provider/v1/systemone"},
	}); err != nil {
		t.Fatalf("建 proxy: %v", err)
	}
	acc := store.Account{Name: "p1", Provider: "proxy", KeyCipher: []byte{1}, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.UpsertProviderModels("proxy", []store.ProviderModel{
		{Model: "typesafe/jev", Source: store.SourceManual, Protocols: []string{translate.ProtocolSystemOne}},
		{Model: "chat-with-decision", Source: store.SourceManual,
			Protocols: []string{translate.ProtocolSystemOne, translate.ProtocolChat}},
		{Model: "gemini-with-decision", Source: store.SourceManual,
			Protocols: []string{translate.ProtocolSystemOne, translate.ProtocolGemini}},
	}); err != nil {
		t.Fatalf("写目录: %v", err)
	}

	cases := []struct{ model, want string }{
		// 集合里只有 systemone：默认协议的 chat 不在集合里，就取它。
		{"typesafe/jev", translate.ProtocolSystemOne},
		// 同时声明 chat 与 systemone：厂商的默认协议 chat 先赢，决策协议只在最后兜底。
		{"chat-with-decision", translate.ProtocolChat},
		// 集合里 gemini 与 systemone 都有时 gemini 在前。
		{"gemini-with-decision", translate.ProtocolGemini},
	}
	for _, c := range cases {
		got, err := st.EffectiveProtocol(acc.ID, c.model)
		if err != nil {
			t.Fatalf("%s 的有效协议: %v", c.model, err)
		}
		if got != c.want {
			t.Fatalf("%s 的有效协议 = %q，期望 %q", c.model, got, c.want)
		}
	}

	// 只有 systemone 一条路由的厂商算不出默认协议：它不该被当成厂商默认，
	// 有效协议靠目录里的协议集合给出。
	if err := st.CreateProvider(store.Provider{
		Name: "decision-only", BaseURL: "https://api.typesafe.ai",
	}, []store.ProviderRoute{{Protocol: translate.ProtocolSystemOne, Path: systemOnePath}}); err != nil {
		t.Fatalf("建 decision-only: %v", err)
	}
	p, err := st.GetProvider("decision-only")
	if err != nil {
		t.Fatalf("取 decision-only: %v", err)
	}
	routes, err := st.ProviderRoutes("decision-only")
	if err != nil {
		t.Fatalf("取 decision-only 路由: %v", err)
	}
	if got := store.ResolveDefaultProtocol(p.DefaultProtocol, routes); got != "" {
		t.Fatalf("systemone 不该被算成厂商默认协议，得到 %q", got)
	}
}

// TestSystemOneRowsCarryNoCredentialText 钉住新协议的存储路径同样只搬密文：
// 厂商行、路由行、目录行与账号行一起序列化时不出现 key 明文，systemone 路由写入失败的错误文案里也不带。
func TestSystemOneRowsCarryNoCredentialText(t *testing.T) {
	st := openTemp(t)
	const plaintext = "sk-systemone-0123456789abcdef"
	cipher, err := secret.Encrypt(bytes.Repeat([]byte{7}, 32), []byte(plaintext))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}

	if err := st.CreateProvider(store.Provider{
		Name: "typesafe", BaseURL: "https://api.typesafe.ai", DefaultProtocol: translate.ProtocolSystemOne,
	}, []store.ProviderRoute{{Protocol: translate.ProtocolSystemOne, Path: systemOnePath}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	acc := store.Account{Name: "ts1", Provider: "typesafe", KeyCipher: cipher, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.UpsertProviderModels("typesafe", []store.ProviderModel{{
		Model: "typesafe/jev", Source: store.SourceManual, Protocols: []string{translate.ProtocolSystemOne},
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}

	p, err := st.GetProvider("typesafe")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	routes, err := st.ProviderRoutes("typesafe")
	if err != nil {
		t.Fatalf("取路由: %v", err)
	}
	models, err := st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("取模型行: %v", err)
	}
	for name, v := range map[string]any{
		"provider": p, "routes": routes, "models": models, "account": mustGet(t, st, "ts1"),
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化 %s: %v", name, err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("%s 的序列化输出里出现了 key 明文: %s", name, b)
		}
	}

	// 写入失败的错误文案同样只该有路径与服务名，不带凭据。
	if err := st.SetProvider("typesafe", store.ProviderPatch{
		Routes: map[string]string{translate.ProtocolSystemOne: "https:///v1/systemone"},
	}); err == nil {
		t.Fatal("空路径应报错")
	} else if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("错误文案里出现了 key 明文: %v", err)
	}
}
