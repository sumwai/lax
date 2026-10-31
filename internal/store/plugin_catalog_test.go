package store_test

import (
	"errors"
	"strings"
	"testing"

	"lax/internal/store"
)

// 插件目录落库：一次安装把厂商、路由、模型目录、方言绑定与别名一起写进去，
// 冲突一律整包失败，删除插件带走它自己那一份，用户的写法留着。

// pluginCatalogFixture 是一家插件带回的目录：两条路由、两个模型、每个模型两条绑定，
// 客户端可见名字都与上游名字不同（每个模型各要一条别名）。
func pluginCatalogFixture() store.PluginCatalog {
	return store.PluginCatalog{
		Provider:   "acme",
		Name:       "Acme 上游",
		BaseURL:    "https://api.acme.example",
		ModelsPath: "/v1/models",
		Routes: []store.ProviderRoute{
			{Protocol: "openai-chat", Path: "/v1/chat/completions"},
			{Protocol: "openai-speech", Path: "/v1/audio/speech"},
		},
		Models: []store.PluginModelEntry{
			{
				Model:      "acme-chat",
				UpstreamID: "acme-gpt-4o",
				Operations: []store.ModelOperation{
					{Operation: "chat.complete@1", DialectID: "com.acme/openai-chat@1", CapabilitiesJSON: `{"tools":true}`},
					{Operation: "embeddings.create@1", DialectID: "com.acme/openai-embeddings@1", CapabilitiesJSON: `{}`},
				},
			},
			{
				Model:      "acme-voice",
				UpstreamID: "acme-tts",
				Operations: []store.ModelOperation{
					{Operation: "speech.synthesize@1", DialectID: "com.acme/mimo-chat-speech@1", CapabilitiesJSON: `{}`},
					{Operation: "speech.transcribe@1", DialectID: "com.acme/mimo-chat-asr@1", CapabilitiesJSON: `{}`},
				},
			},
		},
	}
}

// catalogCounts 是插件会碰的五张表的行数，用来钉住「冲突时一行都没写」。
type catalogCounts struct {
	providers  int
	routes     int
	models     int
	operations int
	aliases    int
}

func countCatalog(t *testing.T, st *store.Store) catalogCounts {
	t.Helper()
	count := func(table string) int {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("数 %s 的行数: %v", table, err)
		}
		return n
	}
	return catalogCounts{
		providers:  count("providers"),
		routes:     count("provider_routes"),
		models:     count("provider_models"),
		operations: count("model_operations"),
		aliases:    count("model_aliases"),
	}
}

// pluginIDOf 直接读 providers.plugin_id：业务结构体上没有这个字段，只有归属清理看它。
func pluginIDOf(t *testing.T, st *store.Store, provider string) string {
	t.Helper()
	var id string
	err := st.DB().QueryRow(`SELECT plugin_id FROM providers WHERE name = ?`, provider).Scan(&id)
	if err != nil {
		t.Fatalf("读厂商 %s 的 plugin_id: %v", provider, err)
	}
	return id
}

// TestApplyPluginCatalogFreshInstall 钉住全新安装的落库结果，以及重复调用幂等。
func TestApplyPluginCatalogFreshInstall(t *testing.T) {
	st := openTemp(t)
	const id = "com.acme"

	if err := st.ApplyPluginCatalog(id, pluginCatalogFixture(), "plugin"); err != nil {
		t.Fatalf("安装插件目录: %v", err)
	}

	p, err := st.GetProvider("acme")
	if err != nil {
		t.Fatalf("厂商没建出来: %v", err)
	}
	if p.BaseURL != "https://api.acme.example" || p.ModelsPath != "/v1/models" || p.Note != "Acme 上游" {
		t.Fatalf("厂商行 = %+v", p)
	}
	if got := pluginIDOf(t, st, "acme"); got != id {
		t.Fatalf("providers.plugin_id = %q，期望 %q", got, id)
	}

	routes, err := st.ProviderRoutes("acme")
	if err != nil || len(routes) != 2 {
		t.Fatalf("路由 = %+v, err = %v", routes, err)
	}
	// 按协议名升序：openai-chat 在前，openai-speech 在后。
	if routes[0].Protocol != "openai-chat" || routes[0].Path != "/v1/chat/completions" {
		t.Fatalf("chat 路由 = %+v", routes[0])
	}
	if routes[1].Protocol != "openai-speech" || routes[1].Path != "/v1/audio/speech" {
		t.Fatalf("speech 路由 = %+v", routes[1])
	}
	// 没声明 models_path 的路由按路径推导，与 provider add / provider set 同一套规则。
	if routes[0].ModelsPath != "/v1/models" || routes[1].ModelsPath != "" {
		t.Fatalf("路由的 models_path = %q / %q", routes[0].ModelsPath, routes[1].ModelsPath)
	}

	models, err := st.ProviderModels("acme")
	if err != nil || len(models) != 2 {
		t.Fatalf("模型目录 = %+v, err = %v", models, err)
	}
	for i, want := range []string{"acme-gpt-4o", "acme-tts"} {
		if models[i].Model != want {
			t.Fatalf("目录第 %d 行 = %q，期望 %q", i, models[i].Model, want)
		}
		if models[i].Source != "plugin" {
			t.Fatalf("目录 %s 的来源 = %q，期望 plugin", models[i].Model, models[i].Source)
		}
	}

	ops, err := st.ListModelOperations("acme", "")
	if err != nil || len(ops) != 4 {
		t.Fatalf("方言绑定 = %+v, err = %v", ops, err)
	}
	chat, ok, err := st.ModelOperation("acme", "acme-gpt-4o", "chat.complete@1")
	if err != nil || !ok {
		t.Fatalf("读 chat 绑定: ok=%v err=%v", ok, err)
	}
	if chat.DialectID != "com.acme/openai-chat@1" || chat.Source != "plugin" || chat.CapabilitiesJSON != `{"tools":true}` {
		t.Fatalf("chat 绑定 = %+v", chat)
	}

	aliases, err := st.ModelAliases()
	if err != nil || len(aliases) != 2 {
		t.Fatalf("别名 = %+v, err = %v", aliases, err)
	}
	want := map[string]string{"acme-chat": "acme-gpt-4o", "acme-voice": "acme-tts"}
	for _, a := range aliases {
		if want[a.Alias] != a.Target {
			t.Fatalf("别名 %q 指向 %q，期望 %q", a.Alias, a.Target, want[a.Alias])
		}
	}

	before := countCatalog(t, st)
	if err := st.ApplyPluginCatalog(id, pluginCatalogFixture(), "plugin"); err != nil {
		t.Fatalf("重复安装应幂等，得到 %v", err)
	}
	if after := countCatalog(t, st); after != before {
		t.Fatalf("重复安装改变了行数：%+v → %+v", before, after)
	}
	if got, _, _ := st.ModelOperation("acme", "acme-gpt-4o", "chat.complete@1"); got.DialectID != chat.DialectID {
		t.Fatalf("重复安装改掉了方言：%+v", got)
	}
}

// TestApplyPluginCatalogProviderBaseURLConflict 钉住地址不一致时整包失败，一行都不写。
func TestApplyPluginCatalogProviderBaseURLConflict(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://other.example"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	before := countCatalog(t, st)

	err := st.ApplyPluginCatalog("com.acme", pluginCatalogFixture(), "plugin")
	if err == nil {
		t.Fatal("地址不同应被拒绝")
	}
	for _, want := range []string{"https://other.example", "https://api.acme.example", "lax provider rm acme"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案里缺 %q：%v", want, err)
		}
	}
	if after := countCatalog(t, st); after != before {
		t.Fatalf("被拒绝的安装写了行：%+v → %+v", before, after)
	}
	if got := pluginIDOf(t, st, "acme"); got != "" {
		t.Fatalf("被拒绝的安装写了归属：plugin_id = %q", got)
	}
}

// TestApplyPluginCatalogRouteConflict 钉住路由路径冲突时报错并回滚（含已经写下去的归属）。
func TestApplyPluginCatalogRouteConflict(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://api.acme.example"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/older/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	before := countCatalog(t, st)

	err := st.ApplyPluginCatalog("com.acme", pluginCatalogFixture(), "plugin")
	if err == nil {
		t.Fatal("路由路径不同应被拒绝")
	}
	for _, want := range []string{"/v1/older/chat/completions", "/v1/chat/completions", "lax provider set acme --openai"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案里缺 %q：%v", want, err)
		}
	}
	if after := countCatalog(t, st); after != before {
		t.Fatalf("被拒绝的安装写了行：%+v → %+v", before, after)
	}
	// 厂商与地址都对得上，归属本来会被补上；路由冲突之后必须一起回滚。
	if got := pluginIDOf(t, st, "acme"); got != "" {
		t.Fatalf("回滚之后 plugin_id = %q，期望空串", got)
	}
}

// TestApplyPluginCatalogManualDialectWins 钉住「手写绑定优先」：方言不同就整包失败。
func TestApplyPluginCatalogManualDialectWins(t *testing.T) {
	st := openTemp(t)
	manual := store.ModelOperation{
		Provider: "acme", Model: "acme-gpt-4o", Operation: "chat.complete@1",
		DialectID: "lax/openai-chat@1", Source: "manual",
	}
	if err := st.SetModelOperation(manual); err != nil {
		t.Fatalf("写手写绑定: %v", err)
	}
	before := countCatalog(t, st)

	err := st.ApplyPluginCatalog("com.acme", pluginCatalogFixture(), "plugin")
	if err == nil {
		t.Fatal("手写绑定不同方言应被拒绝")
	}
	for _, want := range []string{"lax/openai-chat@1", "com.acme/openai-chat@1", "手写"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案里缺 %q：%v", want, err)
		}
	}
	// 厂商、路由与目录都是这次事务里先写的，失败之后一行都不留。
	if _, err := st.GetProvider("acme"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("回滚之后厂商仍在: %v", err)
	}
	if after := countCatalog(t, st); after != before {
		t.Fatalf("回滚之后的行数 = %+v，期望回到 %+v", after, before)
	}
	got, ok, err := st.ModelOperation("acme", "acme-gpt-4o", "chat.complete@1")
	if err != nil || !ok || got.DialectID != "lax/openai-chat@1" || got.Source != "manual" {
		t.Fatalf("手写绑定被改动了: %+v, ok=%v, err=%v", got, ok, err)
	}
}

// TestApplyPluginCatalogAliasConflict 钉住别名指向别的目标时整包失败。
func TestApplyPluginCatalogAliasConflict(t *testing.T) {
	st := openTemp(t)
	if err := st.SetModelAlias("acme-chat", "gpt-4o"); err != nil {
		t.Fatalf("写别名: %v", err)
	}
	before := countCatalog(t, st)

	err := st.ApplyPluginCatalog("com.acme", pluginCatalogFixture(), "plugin")
	if err == nil {
		t.Fatal("别名指向别的目标应被拒绝")
	}
	for _, want := range []string{"acme-chat", "gpt-4o", "acme-gpt-4o", "lax model alias"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案里缺 %q：%v", want, err)
		}
	}
	if _, err := st.GetProvider("acme"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("回滚之后厂商仍在: %v", err)
	}
	if after := countCatalog(t, st); after != before {
		t.Fatalf("回滚之后的行数 = %+v，期望回到 %+v", after, before)
	}
	if target, ok, err := st.ResolveModelAlias("acme-chat"); err != nil || !ok || target != "gpt-4o" {
		t.Fatalf("别名被改动了: %q, ok=%v, err=%v", target, ok, err)
	}
}

// TestDeletePluginRemovesCatalog 钉住连带清理的范围：插件自己的厂商、路由、目录、绑定与别名都走，
// 用户自己配的厂商与手写绑定留着。
func TestDeletePluginRemovesCatalog(t *testing.T) {
	st := openTemp(t)
	const id = "com.acme"

	if err := st.PutPluginVersion(store.PluginVersion{PluginID: id, Version: "1.0.0"}, true); err != nil {
		t.Fatalf("装插件: %v", err)
	}
	if err := st.ApplyPluginCatalog(id, pluginCatalogFixture(), "plugin"); err != nil {
		t.Fatalf("写插件目录: %v", err)
	}
	if err := st.CreateAccount(&store.Account{
		Name: "acme-1", Provider: "acme", KeyCipher: []byte{1}, Enabled: true,
	}); err != nil {
		t.Fatalf("建插件的账号: %v", err)
	}

	// 用户自己的一套：厂商、路由、手写绑定、别名。
	if err := st.CreateProvider(store.Provider{Name: "myco", BaseURL: "https://my.example"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建用户厂商: %v", err)
	}
	if err := st.SetModelOperation(store.ModelOperation{
		Provider: "myco", Model: "m1", Operation: "chat.complete@1",
		DialectID: "lax/openai-chat@1", Source: "manual",
	}); err != nil {
		t.Fatalf("写用户绑定: %v", err)
	}
	if err := st.SetModelAlias("my-fast", "m1"); err != nil {
		t.Fatalf("写用户别名: %v", err)
	}

	if err := st.DeletePlugin(id); err != nil {
		t.Fatalf("卸载插件: %v", err)
	}

	if _, err := st.GetProvider("acme"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("插件的厂商还在: %v", err)
	}
	// 指向插件厂商的账号跟着停用：厂商没了，它剩下的是一个必然失败的请求。
	if account, err := st.GetAccount("acme-1"); err != nil {
		t.Fatalf("读账号: %v", err)
	} else if account.Enabled {
		t.Fatalf("账号 %s 还是启用状态", account.Name)
	}
	if routes, _ := st.ProviderRoutes("acme"); len(routes) != 0 {
		t.Fatalf("插件的路由还在: %+v", routes)
	}
	if models, _ := st.ProviderModels("acme"); len(models) != 0 {
		t.Fatalf("插件的模型目录还在: %+v", models)
	}
	if ops, _ := st.ListModelOperations("acme", ""); len(ops) != 0 {
		t.Fatalf("插件的方言绑定还在: %+v", ops)
	}
	aliases, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("读别名: %v", err)
	}
	if len(aliases) != 1 || aliases[0].Alias != "my-fast" || aliases[0].Target != "m1" {
		t.Fatalf("别名 = %+v，期望只剩用户那条 my-fast", aliases)
	}

	if _, err := st.GetProvider("myco"); err != nil {
		t.Fatalf("用户厂商被删了: %v", err)
	}
	if routes, _ := st.ProviderRoutes("myco"); len(routes) != 1 {
		t.Fatalf("用户路由 = %+v", routes)
	}
	bound, ok, err := st.ModelOperation("myco", "m1", "chat.complete@1")
	if err != nil || !ok || bound.Source != "manual" {
		t.Fatalf("用户手写绑定 = %+v, ok=%v, err=%v", bound, ok, err)
	}
}

// TestApplyPluginCatalogSameNameSkipsAlias 钉住「客户端名字等于上游名字时不写别名」。
func TestApplyPluginCatalogSameNameSkipsAlias(t *testing.T) {
	st := openTemp(t)
	catalog := store.PluginCatalog{
		Provider: "acme2",
		Name:     "Acme2",
		BaseURL:  "https://api.acme2.example",
		Routes:   []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}},
		Models: []store.PluginModelEntry{{
			Model:      "shared-name",
			UpstreamID: "shared-name",
			Operations: []store.ModelOperation{
				{Operation: "chat.complete@1", DialectID: "com.acme2/openai-chat@1"},
			},
		}},
	}
	if err := st.ApplyPluginCatalog("com.acme2", catalog, "plugin"); err != nil {
		t.Fatalf("安装插件目录: %v", err)
	}
	if aliases, err := st.ModelAliases(); err != nil || len(aliases) != 0 {
		t.Fatalf("别名 = %+v, err = %v，期望一条都没有", aliases, err)
	}
	models, err := st.ProviderModels("acme2")
	if err != nil || len(models) != 1 || models[0].Model != "shared-name" {
		t.Fatalf("模型目录 = %+v, err = %v", models, err)
	}
}
