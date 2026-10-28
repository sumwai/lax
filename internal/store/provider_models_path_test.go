package store_test

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"switchboard/internal/store"

	_ "modernc.org/sqlite"
)

// TestMigration0008UpgradesOldDatabase 模拟一个停在 0007 的库：升级只给 providers 加一列
// models_path，默认空串；provider_routes 的形状与数据一个字段都不动（每协议的 models_path 还在），
// user_version 推到 8。
func TestMigration0008UpgradesOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 老库用 0001..0007 的迁移文件本身来建，不手抄 DDL：手抄的形状会随实现漂移。
	for _, name := range []string{
		"0001_init.sql", "0002_drop_agents.sql", "0003_account_models.sql",
		"0004_fail_streak.sql", "0005_model_protocol.sql", "0006_providers.sql",
		"0007_provider_default_protocol.sql",
	} {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("读迁移 %s: %v", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("执行迁移 %s: %v", name, err)
		}
	}
	// 一家用户自建的厂商，带一条手工填的模型列表路径：升级后必须原样还在。
	for _, stmt := range []string{
		`INSERT INTO providers (name, base_url, note, default_protocol)
			VALUES ('myco', 'https://my.example.com', '自建', '')`,
		`INSERT INTO provider_routes (provider, protocol, path, models_path)
			VALUES ('myco', 'openai-chat', '/v2/chat', '/v2/models')`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("写入老库（%s）: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关老库: %v", err)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("升级老库: %v", err)
	}
	defer st.Close()

	var version int
	if err := st.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读 user_version: %v", err)
	}
	if version != 8 {
		t.Fatalf("user_version = %d，期望 8（0008 执行过）", version)
	}

	// providers.models_path 加上了，老行的值一律空串（不覆盖，按路由取）。
	for _, name := range []string{"deepseek", "anthropic", "myco"} {
		p, err := st.GetProvider(name)
		if err != nil {
			t.Fatalf("取厂商 %s: %v", name, err)
		}
		if p.ModelsPath != "" {
			t.Fatalf("升级后 %s 的 models_path = %q，期望空串", name, p.ModelsPath)
		}
	}
	// 8 家种子的显式默认协议原样保留（这里不能直接用 assertSeedDefaults：
	// 老库里多了一家自建厂商 myco，那条用例钉的是「表里恰好 8 家」）。
	for _, want := range seedDefaults {
		p, err := st.GetProvider(want.provider)
		if err != nil {
			t.Fatalf("取厂商 %s: %v", want.provider, err)
		}
		if p.DefaultProtocol != want.protocol {
			t.Fatalf("升级后 %s 的 default_protocol = %q，期望 %q", want.provider, p.DefaultProtocol, want.protocol)
		}
	}

	// provider_routes 的模型列表路径列还在：内置 anthropic 厂商只有 anthropic 一条路由，
	// 没有 openai 路由可以推导，读时推导会让 anthropic 型账号丢掉列表来源。
	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('provider_routes') WHERE name = 'models_path'`).Scan(&n); err != nil {
		t.Fatalf("查表结构: %v", err)
	}
	if n != 1 {
		t.Fatalf("provider_routes.models_path 列不存在，升级不该删这一列")
	}

	// 路由数据原样保留：种子逐条对上，自建厂商那条手工路径也在。
	route, ok, err := st.LookupProviderRoute("myco", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("myco 的路由丢了: ok=%v err=%v", ok, err)
	}
	if route.Path != "/v2/chat" || route.ModelsPath != "/v2/models" {
		t.Fatalf("myco 的路由被改动: %+v", route)
	}
	route, ok, err = st.LookupProviderRoute("anthropic", "anthropic")
	if err != nil || !ok {
		t.Fatalf("anthropic 的路由丢了: ok=%v err=%v", ok, err)
	}
	if route.Path != "/v1/messages" || route.ModelsPath != "/v1/models" {
		t.Fatalf("anthropic 的路由被改动: %+v", route)
	}
	var nRoutes int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM provider_routes").Scan(&nRoutes); err != nil {
		t.Fatalf("数路由行: %v", err)
	}
	if nRoutes != len(seedRoutes)+1 {
		t.Fatalf("升级后的路由数 = %d，期望 %d", nRoutes, len(seedRoutes)+1)
	}
}

// TestCreateProviderRejectsDuplicateName 钉住 `provider add` 只新建：重名报 ErrProviderExists
// 并提示改用 `provider set`，库里那一行一个字段都不动。
func TestCreateProviderRejectsDuplicateName(t *testing.T) {
	st := openTemp(t)

	p := store.Provider{Name: "myco", BaseURL: "https://my.example.com/", Note: "自建"}
	routes := []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}
	if err := st.CreateProvider(p, routes); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}

	// 地址在写入时去掉末尾斜杠，模型列表路径按 chat 路由的路径推导。
	got, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if got.BaseURL != "https://my.example.com" || got.Note != "自建" || got.ModelsPath != "" {
		t.Fatalf("新建的厂商 = %+v", got)
	}
	route, ok, err := st.LookupProviderRoute("myco", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取路由: ok=%v err=%v", ok, err)
	}
	if route.ModelsPath != "/v1/models" {
		t.Fatalf("chat 路由的 models_path = %q，期望按路径推导出的 /v1/models", route.ModelsPath)
	}

	// 重名：报错并给出下一步命令。
	p.BaseURL = "https://other.example.com"
	err = st.CreateProvider(p, []store.ProviderRoute{{Protocol: "anthropic", Path: "/v1/messages"}})
	if !errors.Is(err, store.ErrProviderExists) {
		t.Fatalf("重名应报 ErrProviderExists，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "provider set myco") {
		t.Fatalf("错误文案应提示改用 provider set: %v", err)
	}
	got, err = st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if got.BaseURL != "https://my.example.com" {
		t.Fatalf("被拒的重名不该改地址，实际 %q", got.BaseURL)
	}
	if routes, err := st.ProviderRoutes("myco"); err != nil || len(routes) != 1 {
		t.Fatalf("被拒的重名不该加路由: %+v err=%v", routes, err)
	}
}

// TestCreateProviderValidatesAddressAndRoutes 覆盖新建时的每一类拒绝：地址、路由数量、路径形状、
// 协议名、默认协议；被拒时一条记录都不留。
func TestCreateProviderValidatesAddressAndRoutes(t *testing.T) {
	st := openTemp(t)
	chat := []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}

	cases := []struct {
		name     string
		provider store.Provider
		routes   []store.ProviderRoute
		want     string
	}{
		{"缺 base_url", store.Provider{Name: "a"}, chat, "base_url"},
		{"地址不带协议头", store.Provider{Name: "b", BaseURL: "api.example.com"}, chat, "http://"},
		{"没有路由", store.Provider{Name: "c", BaseURL: "https://c.example.com"}, nil, "至少要声明一条协议路由"},
		{"路径不以 / 开头", store.Provider{Name: "d", BaseURL: "https://d.example.com"},
			[]store.ProviderRoute{{Protocol: "openai-chat", Path: "v1/chat/completions"}}, "要以 / 开头"},
		{"协议不认识", store.Provider{Name: "e", BaseURL: "https://e.example.com"},
			[]store.ProviderRoute{{Protocol: "gemini", Path: "/v1/chat/completions"}}, "不认识"},
		{"默认协议不在这次路由里", store.Provider{Name: "f", BaseURL: "https://f.example.com", DefaultProtocol: "anthropic"},
			chat, "没有声明这个协议的路由"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := st.CreateProvider(c.provider, c.routes)
			if err == nil {
				t.Fatalf("%s 应报错", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误文案 %q 缺少 %q", err.Error(), c.want)
			}
			if _, err := st.GetProvider(c.provider.Name); !errors.Is(err, store.ErrProviderNotFound) {
				t.Fatalf("被拒的新建不该留下厂商行，实际 %v", err)
			}
		})
	}
}

// TestSetProviderPatchesOnlyGivenFields 钉住 `provider set` 的增量语义：nil 的字段不动，
// 改地址、备注、默认协议、模型列表覆盖各自只影响自己那一列，路由可以一条条加。
func TestSetProviderPatchesOnlyGivenFields(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{
		Name: "myco", BaseURL: "https://my.example.com", Note: "自建", DefaultProtocol: "openai-chat",
	}, []store.ProviderRoute{
		{Protocol: "openai-chat", Path: "/v1/chat/completions"},
		{Protocol: "anthropic", Path: "/v1/messages"},
	}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}

	note := "改过"
	if err := st.SetProvider("myco", store.ProviderPatch{Note: &note}); err != nil {
		t.Fatalf("改备注: %v", err)
	}
	got, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if got.Note != "改过" || got.BaseURL != "https://my.example.com" || got.DefaultProtocol != "openai-chat" {
		t.Fatalf("只改备注时其他字段应保持原样: %+v", got)
	}
	if routes, err := st.ProviderRoutes("myco"); err != nil || len(routes) != 2 {
		t.Fatalf("只改备注不该动路由: %+v err=%v", routes, err)
	}

	base := "https://api2.example.com/"
	if err := st.SetProvider("myco", store.ProviderPatch{BaseURL: &base}); err != nil {
		t.Fatalf("改地址: %v", err)
	}
	if got, err := st.GetProvider("myco"); err != nil || got.BaseURL != "https://api2.example.com" {
		t.Fatalf("改地址后 = %+v err=%v，期望去掉末尾斜杠", got, err)
	}

	// 加一条 responses 路由，别的路由不动。
	if err := st.SetProvider("myco", store.ProviderPatch{
		Routes: map[string]string{"openai-responses": "/v1/responses"},
	}); err != nil {
		t.Fatalf("加路由: %v", err)
	}
	routes, err := st.ProviderRoutes("myco")
	if err != nil {
		t.Fatalf("取路由: %v", err)
	}
	if len(routes) != 3 {
		t.Fatalf("加一条路由后 = %+v，期望三条", routes)
	}

	// 删掉 anthropic 那条，默认协议仍是 openai-chat。
	if err := st.SetProvider("myco", store.ProviderPatch{Routes: map[string]string{"anthropic": ""}}); err != nil {
		t.Fatalf("删路由: %v", err)
	}
	if _, ok, err := st.LookupProviderRoute("myco", "anthropic"); err != nil || ok {
		t.Fatalf("anthropic 路由应被删掉: ok=%v err=%v", ok, err)
	}
	if got, err := st.EffectiveProviderProtocol("myco"); err != nil || got != "openai-chat" {
		t.Fatalf("删一条别的路由不该影响默认协议: %q err=%v", got, err)
	}

	// 改默认协议到 responses；传空串回到固定顺序（openai-chat 排第一）。
	def := "openai-responses"
	if err := st.SetProvider("myco", store.ProviderPatch{DefaultProtocol: &def}); err != nil {
		t.Fatalf("改默认协议: %v", err)
	}
	if got, err := st.EffectiveProviderProtocol("myco"); err != nil || got != "openai-responses" {
		t.Fatalf("默认协议 = %q err=%v，期望 openai-responses", got, err)
	}
	empty := ""
	if err := st.SetProvider("myco", store.ProviderPatch{DefaultProtocol: &empty}); err != nil {
		t.Fatalf("清默认协议: %v", err)
	}
	got, err = st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if got.DefaultProtocol != "" {
		t.Fatalf("清空后 default_protocol = %q，期望空串", got.DefaultProtocol)
	}
	if proto, err := st.EffectiveProviderProtocol("myco"); err != nil || proto != "openai-chat" {
		t.Fatalf("清空后有效默认协议 = %q err=%v，期望按固定顺序取 openai-chat", proto, err)
	}

	// 厂商级的模型列表覆盖：写入后读得出来，传空串回到按路由取。
	modelsPath := "/v2/models"
	if err := st.SetProvider("myco", store.ProviderPatch{ModelsPath: &modelsPath}); err != nil {
		t.Fatalf("写模型列表覆盖: %v", err)
	}
	if got, err := st.GetProvider("myco"); err != nil || got.ModelsPath != "/v2/models" {
		t.Fatalf("models_path = %+v err=%v", got, err)
	}
	if err := st.SetProvider("myco", store.ProviderPatch{ModelsPath: &empty}); err != nil {
		t.Fatalf("清模型列表覆盖: %v", err)
	}
	if got, err := st.GetProvider("myco"); err != nil || got.ModelsPath != "" {
		t.Fatalf("清空后 models_path = %+v err=%v，期望空串", got, err)
	}
	noSlash := "models"
	if err := st.SetProvider("myco", store.ProviderPatch{ModelsPath: &noSlash}); err == nil {
		t.Fatal("模型列表路径不以 / 开头应报错")
	}

	// 不存在的厂商与空地址都要拒绝。
	if err := st.SetProvider("nope", store.ProviderPatch{Note: &note}); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("改不存在的厂商应报 ErrProviderNotFound，实际 %v", err)
	}
	if err := st.SetProvider("myco", store.ProviderPatch{BaseURL: &empty}); err == nil {
		t.Fatal("base_url 清空应报错")
	}
}

// TestSetProviderRejectsEmptyRoutesAndDanglingDefault 钉住两条写入侧约束：删到一条不剩要拒绝，
// 默认协议指不到路由也要拒绝（同一事务回滚，路由一条都不少）。
func TestSetProviderRejectsEmptyRoutesAndDanglingDefault(t *testing.T) {
	st := openTemp(t)

	// 两条都删：拒绝，并提示整体删厂商走 provider rm。
	err := st.SetProvider("deepseek", store.ProviderPatch{
		Routes: map[string]string{"openai-chat": "", "anthropic": ""},
	})
	if err == nil {
		t.Fatal("删到一条不剩应报错")
	}
	if !strings.Contains(err.Error(), "至少要保留一条协议路由") || !strings.Contains(err.Error(), "provider rm deepseek") {
		t.Fatalf("错误文案应说明至少保留一条并给出 provider rm: %v", err)
	}
	if routes, err := st.ProviderRoutes("deepseek"); err != nil || len(routes) != 2 {
		t.Fatalf("被拒的改动不该删路由: %+v err=%v", routes, err)
	}

	// 默认协议是 openai-chat，把这条路由删掉会让默认协议悬空：拒绝，并给出两条出路。
	err = st.SetProvider("deepseek", store.ProviderPatch{Routes: map[string]string{"openai-chat": ""}})
	if err == nil {
		t.Fatal("默认协议指向的路由被删应报错")
	}
	for _, want := range []string{"openai-chat", "路由不存在", "--default-protocol", "--openai"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案 %q 缺少 %q", err.Error(), want)
		}
	}
	if routes, err := st.ProviderRoutes("deepseek"); err != nil || len(routes) != 2 {
		t.Fatalf("被拒的改动不该删路由: %+v err=%v", routes, err)
	}

	// 同一次里删路由并改默认协议：允许，最后只剩 anthropic 一条。
	def := "anthropic"
	if err := st.SetProvider("deepseek", store.ProviderPatch{
		Routes: map[string]string{"openai-chat": ""}, DefaultProtocol: &def,
	}); err != nil {
		t.Fatalf("同一次里删路由并改默认协议: %v", err)
	}
	routes, err := st.ProviderRoutes("deepseek")
	if err != nil {
		t.Fatalf("取路由: %v", err)
	}
	if len(routes) != 1 || routes[0].Protocol != "anthropic" {
		t.Fatalf("删完之后 = %+v，期望只剩 anthropic", routes)
	}
	if got, err := st.EffectiveProviderProtocol("deepseek"); err != nil || got != "anthropic" {
		t.Fatalf("默认协议 = %q err=%v，期望 anthropic", got, err)
	}

	// 不认识的协议名与指向不存在路由的默认协议都拒绝。
	if err := st.SetProvider("deepseek", store.ProviderPatch{Routes: map[string]string{"gemini": "/v1/generate"}}); err == nil {
		t.Fatal("不认识的协议应报错")
	}
	bad := "openai-responses"
	if err := st.SetProvider("deepseek", store.ProviderPatch{DefaultProtocol: &bad}); err == nil {
		t.Fatal("默认协议指向不存在的路由应报错")
	}
}

// TestSetProviderEditsRouteLessProvider 确认一条路由都没有的厂商仍能改备注与地址：
// 「至少要保留一条路由」拦的是这次把路由删光，不是本来就没配过路由的厂商。
// 它只由过渡期的 AddProvider 造得出来（`provider add` 要求至少一条路由）。
func TestSetProviderEditsRouteLessProvider(t *testing.T) {
	st := openTemp(t)
	if err := st.AddProvider(store.Provider{Name: "noroute", BaseURL: "https://nr.example.com"}, nil); err != nil {
		t.Fatalf("建无路由厂商: %v", err)
	}

	note := "没有路由"
	if err := st.SetProvider("noroute", store.ProviderPatch{Note: &note}); err != nil {
		t.Fatalf("改无路由厂商的备注: %v", err)
	}
	if got, err := st.GetProvider("noroute"); err != nil || got.Note != "没有路由" {
		t.Fatalf("备注 = %+v err=%v", got, err)
	}

	// 给它补第一条路由之后，「删光」才该被拦。
	if err := st.SetProvider("noroute", store.ProviderPatch{
		Routes: map[string]string{"openai-chat": "/v1/chat/completions"},
	}); err != nil {
		t.Fatalf("补路由: %v", err)
	}
	if err := st.SetProvider("noroute", store.ProviderPatch{
		Routes: map[string]string{"openai-chat": ""},
	}); err == nil {
		t.Fatal("把仅有的路由删掉应报错")
	}
}

// TestDeriveModelsPath 钉住写入侧的推导规则：只给 openai-chat 推，尾部 /chat/completions 换成 /models。
func TestDeriveModelsPath(t *testing.T) {
	cases := []struct{ protocol, path, want string }{
		{"openai-chat", "/v1/chat/completions", "/v1/models"},
		{"openai-chat", "/chat/completions", "/models"},
		{"openai-chat", "/v2/chat", ""},
		{"openai-responses", "/v1/responses", ""},
		{"anthropic", "/v1/messages", ""},
	}
	for _, c := range cases {
		if got := store.DeriveModelsPath(c.protocol, c.path); got != c.want {
			t.Fatalf("DeriveModelsPath(%q, %q) = %q，期望 %q", c.protocol, c.path, got, c.want)
		}
	}
}

// TestResolveModelsPath 钉住读取侧的规则与「路径带着协议一起返回」：
// 厂商覆盖优先（协议取账号协议），其次是账号协议那条路由，再退到协议名排序的第一条非空值，
// 回退时协议跟着那条路由走，不再用账号协议。
func TestResolveModelsPath(t *testing.T) {
	routes := []store.ProviderRoute{
		{Protocol: "anthropic", Path: "/v1/messages", ModelsPath: ""},
		{Protocol: "openai-chat", Path: "/v1/chat/completions", ModelsPath: "/v1/models"},
	}

	path, protocol := store.ResolveModelsPath("", routes, "openai-chat")
	if path != "/v1/models" || protocol != "openai-chat" {
		t.Fatalf("账号协议那条有值时应取它: %q/%q", path, protocol)
	}
	path, protocol = store.ResolveModelsPath("", routes, "anthropic")
	if path != "/v1/models" || protocol != "openai-chat" {
		t.Fatalf("账号协议那条为空时应回退到 chat 的 /v1/models 并用 chat 的协议: %q/%q", path, protocol)
	}
	path, protocol = store.ResolveModelsPath("/v2/models", routes, "anthropic")
	if path != "/v2/models" || protocol != "anthropic" {
		t.Fatalf("厂商覆盖应优先且协议取账号协议: %q/%q", path, protocol)
	}
	path, protocol = store.ResolveModelsPath("", []store.ProviderRoute{
		{Protocol: "anthropic", Path: "/v1/messages"},
	}, "anthropic")
	if path != "" || protocol != "" {
		t.Fatalf("一条非空 models_path 都没有时应返回空串: %q/%q", path, protocol)
	}
	if path, protocol = store.ResolveModelsPath("", nil, "openai-chat"); path != "" || protocol != "" {
		t.Fatalf("没有路由时应返回空串: %q/%q", path, protocol)
	}
}

// TestRouteModelsPathDerivedOnWrite 钉住写入默认值与手工值的区别：
// 推导值跟着路径走，手工填的值不动——内置 anthropic 厂商那条 /v1/models 因此不会因为
// 重写 anthropic 路由而消失（它没有 openai 路由可以推导）。
func TestRouteModelsPathDerivedOnWrite(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{Name: "derived", BaseURL: "https://d.example.com"},
		[]store.ProviderRoute{
			{Protocol: "openai-chat", Path: "/v1/chat/completions"},
			{Protocol: "openai-responses", Path: "/v1/responses"},
		}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}
	if err := st.CreateProvider(store.Provider{Name: "explicit", BaseURL: "https://e.example.com"},
		[]store.ProviderRoute{
			{Protocol: "openai-chat", Path: "/v1/chat/completions", ModelsPath: "/custom/models"},
		}); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}

	modelsPath := func(provider, protocol string) string {
		t.Helper()
		r, ok, err := st.LookupProviderRoute(provider, protocol)
		if err != nil || !ok {
			t.Fatalf("取路由 %s/%s: ok=%v err=%v", provider, protocol, ok, err)
		}
		return r.ModelsPath
	}
	if got := modelsPath("derived", "openai-chat"); got != "/v1/models" {
		t.Fatalf("chat 路由的推导值 = %q，期望 /v1/models", got)
	}
	if got := modelsPath("derived", "openai-responses"); got != "" {
		t.Fatalf("responses 路由应留空表示回退，实际 %q", got)
	}
	if got := modelsPath("explicit", "openai-chat"); got != "/custom/models" {
		t.Fatalf("显式给的模型列表路径 = %q，期望不被推导覆盖", got)
	}

	// 改路径：推导值跟着换，手工值不动。
	if err := st.SetProvider("derived", store.ProviderPatch{
		Routes: map[string]string{"openai-chat": "/v2/chat/completions"},
	}); err != nil {
		t.Fatalf("改 chat 路径: %v", err)
	}
	if got := modelsPath("derived", "openai-chat"); got != "/v2/models" {
		t.Fatalf("改路径后推导值 = %q，期望 /v2/models", got)
	}
	if err := st.SetProvider("explicit", store.ProviderPatch{
		Routes: map[string]string{"openai-chat": "/v2/chat/completions"},
	}); err != nil {
		t.Fatalf("改 chat 路径: %v", err)
	}
	if got := modelsPath("explicit", "openai-chat"); got != "/custom/models" {
		t.Fatalf("手工填的值不该被改路径覆盖，实际 %q", got)
	}

	// 内置 anthropic 厂商只有 anthropic 一条路由，重写这条路由不能把它的 /v1/models 清掉。
	if err := st.SetProvider("anthropic", store.ProviderPatch{
		Routes: map[string]string{"anthropic": "/v1/messages"},
	}); err != nil {
		t.Fatalf("重写 anthropic 路由: %v", err)
	}
	if got := modelsPath("anthropic", "anthropic"); got != "/v1/models" {
		t.Fatalf("anthropic 厂商的模型列表路径 = %q，期望仍是种子里那条 /v1/models", got)
	}
}

// TestStoreWritePathsValidateBaseURL 确认地址格式与去尾斜杠在 store 的三个写入口都成立：
// cli 那层不再是唯一防线，第二个写入口不能把脏地址落库。
func TestStoreWritePathsValidateBaseURL(t *testing.T) {
	st := openTemp(t)
	chat := []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}

	if err := st.AddProvider(store.Provider{Name: "a", BaseURL: "api.example.com"}, chat); err == nil {
		t.Fatal("AddProvider 应拒绝不带协议头的地址")
	}
	if err := st.AddProvider(store.Provider{Name: "b", BaseURL: "https://b.example.com/"}, chat); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if got, err := st.GetProvider("b"); err != nil || got.BaseURL != "https://b.example.com" {
		t.Fatalf("AddProvider 应去掉末尾斜杠: %+v err=%v", got, err)
	}

	empty := ""
	if err := st.SetProvider("b", store.ProviderPatch{BaseURL: &empty}); err == nil {
		t.Fatal("SetProvider 应拒绝空地址")
	}
	bad := "ftp://c.example.com"
	if err := st.SetProvider("b", store.ProviderPatch{BaseURL: &bad}); err == nil {
		t.Fatal("SetProvider 应拒绝非 http(s) 地址")
	}
	if got, err := st.GetProvider("b"); err != nil || got.BaseURL != "https://b.example.com" {
		t.Fatalf("被拒的地址改动不该落库: %+v err=%v", got, err)
	}
}
