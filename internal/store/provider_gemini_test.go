package store_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"switchboard/internal/store"

	_ "modernc.org/sqlite"
)

// geminiProvider 是迁移 0009 种下的厂商行与它的 gemini 路由，逐字段写死。
// 模型列表与转发路径是两条：列表是 `/v1beta/models`，转发把模型名放在路径里。
var geminiProvider = struct {
	baseURL         string
	defaultProtocol string
	path            string
	modelsPath      string
}{
	baseURL:         "https://generativelanguage.googleapis.com",
	defaultProtocol: "gemini",
	path:            "/v1beta/models/{model}:generateContent",
	modelsPath:      "/v1beta/models",
}

// assertGeminiSeed 断言 gemini 的厂商行与路由都在，且 `{model}` 占位符原样落库：
// 写入侧只合并斜杠，花括号与冒号不做任何改写，替换发生在网关拼好地址之后（见 DESIGN 5.9）。
func assertGeminiSeed(t *testing.T, st *store.Store) {
	t.Helper()

	p, err := st.GetProvider("google-gemini")
	if err != nil {
		t.Fatalf("取厂商 google-gemini: %v", err)
	}
	if p.BaseURL != geminiProvider.baseURL {
		t.Fatalf("google-gemini 的 base_url = %q，期望 %q", p.BaseURL, geminiProvider.baseURL)
	}
	if p.DefaultProtocol != geminiProvider.defaultProtocol {
		t.Fatalf("google-gemini 的 default_protocol = %q，期望 %q", p.DefaultProtocol, geminiProvider.defaultProtocol)
	}
	if p.ModelsPath != "" {
		t.Fatalf("google-gemini 不该有厂商级模型列表覆盖，实际 %q", p.ModelsPath)
	}

	route, ok, err := st.LookupProviderRoute("google-gemini", "gemini")
	if err != nil {
		t.Fatalf("取 gemini 路由: %v", err)
	}
	if !ok {
		t.Fatalf("google-gemini 缺 gemini 路由")
	}
	if route.Path != geminiProvider.path {
		t.Fatalf("gemini 路由的 path = %q，期望 %q（占位符原样保留）", route.Path, geminiProvider.path)
	}
	if !strings.Contains(route.Path, "{model}") {
		t.Fatalf("gemini 路由的 path 里 {model} 占位符丢了: %q", route.Path)
	}
	if route.ModelsPath != geminiProvider.modelsPath {
		t.Fatalf("gemini 路由的 models_path = %q，期望 %q", route.ModelsPath, geminiProvider.modelsPath)
	}

	// 这家只有一条路由，默认协议得指得到它，否则 account add 取默认协议时只能按固定顺序回退。
	protocol, err := st.EffectiveProviderProtocol("google-gemini")
	if err != nil {
		t.Fatalf("读 google-gemini 的有效默认协议: %v", err)
	}
	if protocol != "gemini" {
		t.Fatalf("有效默认协议 = %q，期望 gemini", protocol)
	}
}

// TestMigration0009SeedsGeminiProvider 确认空库跑到最新版本后 gemini 的种子齐全。
func TestMigration0009SeedsGeminiProvider(t *testing.T) {
	st := openTemp(t)

	assertLatestVersion(t, st)
	assertGeminiSeed(t, st)

	// 老厂商一个字段都没被这一条迁移改动。
	for _, want := range seedRoutes {
		if want.migration == 9 {
			continue
		}
		route, ok, err := st.LookupProviderRoute(want.provider, want.protocol)
		if err != nil || !ok {
			t.Fatalf("路由 %s/%s 丢了: ok=%v err=%v", want.provider, want.protocol, ok, err)
		}
		if route.Path != want.path || route.ModelsPath != want.modelsPath {
			t.Fatalf("路由 %s/%s 被改动: %+v", want.provider, want.protocol, route)
		}
	}
}

// TestMigration0009UpgradesOldDatabase 模拟一个停在 0008 的库：升级只加 google-gemini 一行与它的
// gemini 路由，老账号、模型行与用户自建的厂商一个字段都不动，user_version 推到最新。
func TestMigration0009UpgradesOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 老库用 0001..0008 的迁移文件本身来建，不手抄 DDL：手抄的形状会随实现漂移。
	for _, name := range []string{
		"0001_init.sql", "0002_drop_agents.sql", "0003_account_models.sql",
		"0004_fail_streak.sql", "0005_model_protocol.sql", "0006_providers.sql",
		"0007_provider_default_protocol.sql", "0008_provider_models_path.sql",
	} {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("读迁移 %s: %v", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("执行迁移 %s: %v", name, err)
		}
	}
	for _, stmt := range []string{
		// 一个走 gemini 协议的账号（老库里对应的是自建地址），它的模型行与协议发现都得留着。
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('gem', 'google-gemini', 'gemini', 'https://gemini-proxy.example.com', X'010203',
				'["gemini-2.5-pro"]', 1, 100)`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'gemini-2.5-pro', 'usable', 200, '', 12345, 'gemini')`,
		// 用户自建的厂商，带一条模型列表路径：升级不该碰它。
		`INSERT INTO providers (name, base_url, note, default_protocol, models_path)
			VALUES ('myco', 'https://my.example.com', '自建', '', '/v2/models')`,
		`INSERT INTO provider_routes (provider, protocol, path, models_path)
			VALUES ('myco', 'openai-chat', '/v2/chat', '/v2/models')`,
		`PRAGMA user_version = 8`,
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

	assertLatestVersion(t, st)
	assertGeminiSeed(t, st)

	// 老账号与模型行原样：升级只加厂商与路由，不动账号一侧的任何表。
	// 地址不再是账号自己的：它从厂商行取（迁移 0014 删掉了 accounts.base_url，
	// 账号的 provider 已经在表里就不会被补建，旧地址随列一起丢掉），协议同样取自
	// google-gemini 的默认协议。
	acc, err := st.GetAccount("gem")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	if acc.Provider != "google-gemini" || acc.Protocol != "gemini" ||
		acc.BaseURL != "https://generativelanguage.googleapis.com" || len(acc.Models) != 1 {
		t.Fatalf("老账号的派生值不对: %+v", acc)
	}
	if got, err := st.EffectiveProtocol(acc.ID, "gemini-2.5-pro"); err != nil {
		t.Fatalf("取有效协议: %v", err)
	} else if got != "gemini" {
		t.Fatalf("老账号的有效协议 = %q，期望模型行上的 gemini", got)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读老模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != "gemini" || rows[0].Status != store.ModelUsable {
		t.Fatalf("老模型行被改动: %+v", rows)
	}

	// 用户自建的厂商与它的路由原样保留。
	myco, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取自建厂商: %v", err)
	}
	if myco.BaseURL != "https://my.example.com" || myco.ModelsPath != "/v2/models" || myco.DefaultProtocol != "" {
		t.Fatalf("自建厂商被改动: %+v", myco)
	}
	route, ok, err := st.LookupProviderRoute("myco", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("自建厂商的路由丢了: ok=%v err=%v", ok, err)
	}
	if route.Path != "/v2/chat" || route.ModelsPath != "/v2/models" {
		t.Fatalf("自建厂商的路由被改动: %+v", route)
	}
}

// TestMigration0009KeepsUserDefinedGeminiProvider 钉住 0009 的写入方式是 INSERT OR IGNORE：
// 用户可能已经用 `provider add google-gemini` 自己建过这家（地址指向代理），升级不能覆盖他的
// 写法，也不能因为主键冲突让整笔迁移失败。
func TestMigration0009KeepsUserDefinedGeminiProvider(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	for _, name := range []string{
		"0001_init.sql", "0002_drop_agents.sql", "0003_account_models.sql",
		"0004_fail_streak.sql", "0005_model_protocol.sql", "0006_providers.sql",
		"0007_provider_default_protocol.sql", "0008_provider_models_path.sql",
	} {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("读迁移 %s: %v", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("执行迁移 %s: %v", name, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO providers (name, base_url, note, default_protocol, models_path)
			VALUES ('google-gemini', 'https://gemini-proxy.example.com', '自建代理', 'gemini', '')`,
		`INSERT INTO provider_routes (provider, protocol, path, models_path)
			VALUES ('google-gemini', 'gemini', '/gemini/v1beta/models/{model}:generateContent', '')`,
		`PRAGMA user_version = 8`,
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

	assertLatestVersion(t, st)

	p, err := st.GetProvider("google-gemini")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://gemini-proxy.example.com" || p.Note != "自建代理" {
		t.Fatalf("用户自建的 google-gemini 被种子覆盖了: %+v", p)
	}
	route, ok, err := st.LookupProviderRoute("google-gemini", "gemini")
	if err != nil || !ok {
		t.Fatalf("取路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "/gemini/v1beta/models/{model}:generateContent" {
		t.Fatalf("用户的 gemini 路由被改动了: %q", route.Path)
	}
}
