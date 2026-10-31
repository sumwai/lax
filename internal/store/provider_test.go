package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"lax/internal/store"

	_ "github.com/glebarez/go-sqlite"
)

// seedRoutes 是迁移种入的全部路由，逐行写死。migration 记的是哪一次迁移种下它：
// 0006 是升级前就有的 8 家，0009 是 gemini 那一条。
// 完整地址 = 厂商 base_url 去掉末尾斜杠后直接拼 path / models_path，不推 /v1、不猜后缀。
var seedRoutes = []struct {
	provider   string
	baseURL    string
	protocol   string
	path       string
	modelsPath string
	migration  int
}{
	{"anthropic", "https://api.anthropic.com", "anthropic", "/v1/messages", "/v1/models", 6},
	{"openai", "https://api.openai.com", "openai-chat", "/v1/chat/completions", "/v1/models", 6},
	{"openai", "https://api.openai.com", "openai-responses", "/v1/responses", "/v1/models", 6},
	{"deepseek", "https://api.deepseek.com", "openai-chat", "/v1/chat/completions", "/v1/models", 6},
	{"deepseek", "https://api.deepseek.com", "anthropic", "/anthropic/v1/messages", "", 6},
	{"moonshot", "https://api.moonshot.cn", "openai-chat", "/v1/chat/completions", "/v1/models", 6},
	{"kimi", "https://api.moonshot.cn", "openai-chat", "/v1/chat/completions", "/v1/models", 6},
	{"openrouter", "https://openrouter.ai/api", "openai-chat", "/v1/chat/completions", "/v1/models", 6},
	{"ollama", "http://127.0.0.1:11434", "openai-chat", "/v1/chat/completions", "/v1/models", 6},
	{"commandcode", "https://api.commandcode.ai/provider/v1", "openai-chat", "/chat/completions", "/models", 6},
	{"commandcode", "https://api.commandcode.ai/provider/v1", "anthropic", "/messages", "", 6},
	// 0009 种的 gemini 厂商：模型列表在 /v1beta/models，转发路径把模型名放在路径里（占位符原样落库）。
	{"google-gemini", "https://generativelanguage.googleapis.com", "gemini",
		"/v1beta/models/{model}:generateContent", "/v1beta/models", 9},
	// 0017 只给这两家种向量化路由。
	{"openai", "https://api.openai.com", "openai-embeddings", "/v1/embeddings", "", 17},
	{"ollama", "http://127.0.0.1:11434", "openai-embeddings", "/v1/embeddings", "", 17},
	// 0018 给 openai 种两条语音路由（TTS 与 ASR）。
	{"openai", "https://api.openai.com", "openai-speech", "/v1/audio/speech", "", 18},
	{"openai", "https://api.openai.com", "openai-transcriptions", "/v1/audio/transcriptions", "", 18},
}

// seedRoutesBefore 给出某一次迁移之前就已经在库里的路由，用来搭「停在那个版本」的老库：
// 手抄一份老形状会随新种子漂移，而这里要的正是当时那份。
func seedRoutesBefore(version int) []int {
	var idx []int
	for i, route := range seedRoutes {
		if route.migration <= version {
			idx = append(idx, i)
		}
	}
	return idx
}

// seedDefaults 是迁移 0007 种入的默认协议，逐行写死。
// 升级前 deepseek 与 commandcode 的账号默认走 openai-chat；按协议名排序会得到 anthropic，
// 所以这两家的默认协议必须显式存，不能靠排序猜。
var seedDefaults = []struct {
	provider string
	protocol string
}{
	{"anthropic", "anthropic"},
	{"openai", "openai-chat"},
	{"deepseek", "openai-chat"},
	{"moonshot", "openai-chat"},
	{"kimi", "openai-chat"},
	{"openrouter", "openai-chat"},
	{"ollama", "openai-chat"},
	{"commandcode", "openai-chat"},
	// 0009 种的 gemini 厂商：这家只有 gemini 一条路由，默认协议显式写成 gemini。
	{"google-gemini", "gemini"},
}

// assertSeedDefaults 逐行比对种子厂商的默认协议，并确认没有多余的行。
func assertSeedDefaults(t *testing.T, st *store.Store) {
	t.Helper()

	got := map[string]string{}
	providers, err := st.ListProviders()
	if err != nil {
		t.Fatalf("列厂商: %v", err)
	}
	for _, p := range providers {
		got[p.Name] = p.DefaultProtocol
	}
	if len(got) != len(seedDefaults) {
		t.Fatalf("厂商数 = %d，期望 %d", len(got), len(seedDefaults))
	}
	for _, want := range seedDefaults {
		one, err := st.GetProvider(want.provider)
		if err != nil {
			t.Fatalf("取厂商 %s: %v", want.provider, err)
		}
		if one.DefaultProtocol != want.protocol {
			t.Fatalf("厂商 %s 的 default_protocol = %q，期望 %q", want.provider, one.DefaultProtocol, want.protocol)
		}
		if got[want.provider] != want.protocol {
			t.Fatalf("ListProviders 里 %s 的默认协议 = %q，期望 %q", want.provider, got[want.provider], want.protocol)
		}
	}
}

// TestProvidersSeededByMigration 确认空库跑到最新版本后种子齐全：
// 9 家厂商、12 条路由，每条拼出的完整地址与模型列表地址都逐一对上，默认协议也逐行对上。
func TestProvidersSeededByMigration(t *testing.T) {
	st := openTemp(t)

	assertLatestVersion(t, st)

	var nProviders, nRoutes int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM providers").Scan(&nProviders); err != nil {
		t.Fatalf("数厂商行: %v", err)
	}
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM provider_routes").Scan(&nRoutes); err != nil {
		t.Fatalf("数路由行: %v", err)
	}
	if nProviders != len(seedDefaults) || nRoutes != len(seedRoutes) {
		t.Fatalf("种子规模 = %d 家 / %d 条，期望 %d 家 / %d 条",
			nProviders, nRoutes, len(seedDefaults), len(seedRoutes))
	}
	assertSeedDefaults(t, st)

	providers, err := st.ListProviders()
	if err != nil {
		t.Fatalf("列厂商: %v", err)
	}
	names := make([]string, 0, len(providers))
	baseByProvider := map[string]string{}
	for _, p := range providers {
		names = append(names, p.Name)
		baseByProvider[p.Name] = p.BaseURL
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("厂商应按名称排序，实际 %v", names)
	}

	for _, want := range seedRoutes {
		base, err := st.GetProvider(want.provider)
		if err != nil {
			t.Fatalf("取厂商 %s: %v", want.provider, err)
		}
		if base.BaseURL != want.baseURL {
			t.Fatalf("厂商 %s 的 base_url = %q，期望 %q", want.provider, base.BaseURL, want.baseURL)
		}

		route, ok, err := st.LookupProviderRoute(want.provider, want.protocol)
		if err != nil {
			t.Fatalf("取路由 %s/%s: %v", want.provider, want.protocol, err)
		}
		if !ok {
			t.Fatalf("路由 %s/%s 不存在", want.provider, want.protocol)
		}
		full := strings.TrimRight(want.baseURL, "/") + want.path
		gotFull := strings.TrimRight(base.BaseURL, "/") + route.Path
		if route.Path != want.path || gotFull != full {
			t.Fatalf("路由 %s/%s 的地址 = %q（path %q），期望 %q（path %q）",
				want.provider, want.protocol, gotFull, route.Path, full, want.path)
		}
		if route.ModelsPath != want.modelsPath {
			t.Fatalf("路由 %s/%s 的 models_path = %q，期望 %q",
				want.provider, want.protocol, route.ModelsPath, want.modelsPath)
		}
		if route.ModelsPath != "" {
			wantModels := strings.TrimRight(want.baseURL, "/") + want.modelsPath
			gotModels := strings.TrimRight(base.BaseURL, "/") + route.ModelsPath
			if gotModels != wantModels {
				t.Fatalf("路由 %s/%s 的模型列表地址 = %q，期望 %q",
					want.provider, want.protocol, gotModels, wantModels)
			}
		}
	}
}

// TestMigration0006UpgradesOldDatabase 模拟一个停在 0005 的库：升上来之后两张表在、种子齐全，
// 老账号与它的模型行一个字段都不动，user_version 推到最新。
func TestMigration0006UpgradesOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 0005 之后的形状：account_models 已有 protocol 列，还没有 providers / provider_routes。
	ddl := []string{
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
			protocol TEXT NOT NULL, base_url TEXT NOT NULL, key_cipher BLOB NOT NULL,
			models TEXT NOT NULL DEFAULT '[]', price_in REAL, price_out REAL,
			priority INTEGER NOT NULL DEFAULT 100, enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER, last_error TEXT,
			fail_streak INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE requests (
			id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, inbound TEXT NOT NULL,
			outbound TEXT NOT NULL, model_requested TEXT NOT NULL, model_used TEXT NOT NULL,
			account_id INTEGER, http_status INTEGER NOT NULL, ok INTEGER NOT NULL,
			in_tokens INTEGER NOT NULL DEFAULT 0, out_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0, error TEXT)`,
		`CREATE INDEX requests_ts ON requests(ts DESC)`,
		`CREATE TABLE account_models (
			account_id INTEGER NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
			http_status INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
			checked_at INTEGER NOT NULL DEFAULT 0, protocol TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (account_id, model))`,
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('ds1', 'deepseek', 'openai-chat', 'https://api.deepseek.com', X'010203', '["m1"]', 1, 100)`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm1', 'usable', 200, '', 12345, 'anthropic')`,
		`PRAGMA user_version = 5`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建老库（%s）: %v", stmt, err)
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

	providers, err := st.ListProviders()
	if err != nil {
		t.Fatalf("列厂商: %v", err)
	}
	if len(providers) != len(seedDefaults) {
		t.Fatalf("升级后的厂商数 = %d，期望 %d", len(providers), len(seedDefaults))
	}
	assertSeedDefaults(t, st)
	var nRoutes int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM provider_routes").Scan(&nRoutes); err != nil {
		t.Fatalf("数路由行: %v", err)
	}
	if nRoutes != len(seedRoutes) {
		t.Fatalf("升级后的路由数 = %d，期望 %d", nRoutes, len(seedRoutes))
	}

	// 老数据原样保留：升级只加表，不动 accounts 与 account_models。
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	if acc.Provider != "deepseek" || acc.BaseURL != "https://api.deepseek.com" || len(acc.Models) != 1 {
		t.Fatalf("老账号被改动: %+v", acc)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读老模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != "anthropic" || rows[0].Status != store.ModelUsable {
		t.Fatalf("老模型行被改动: %+v", rows)
	}
}

// TestMigration0007UpgradesOldDatabase 模拟一个停在 0006 的库：升级只加 default_protocol 一列，
// 种子里的 8 家厂商逐行拿上升级前的默认协议（0009 那家由它自己种），用户自建的厂商保持空串，
// 路由与地址一个都不动。
func TestMigration0007UpgradesOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 0006 之后的形状：providers 还没有 default_protocol 列。
	// accounts、requests 与 account_models 按当时的样子给出：真实的老库一直有这三张表
	// （account_models 由 0003 建立），后面的迁移（例如 0011 回填 in_tokens_uncached、
	// 0015 把可达性搬进新表）会在它们上面动列。
	ddl := []string{
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
			protocol TEXT NOT NULL, base_url TEXT NOT NULL, key_cipher BLOB NOT NULL,
			models TEXT NOT NULL DEFAULT '[]', price_in REAL, price_out REAL,
			priority INTEGER NOT NULL DEFAULT 100, enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER, last_error TEXT,
			fail_streak INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE requests (
			id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, inbound TEXT NOT NULL,
			outbound TEXT NOT NULL, model_requested TEXT NOT NULL, model_used TEXT NOT NULL,
			account_id INTEGER, http_status INTEGER NOT NULL, ok INTEGER NOT NULL,
			in_tokens INTEGER NOT NULL DEFAULT 0, out_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0, error TEXT)`,
		`CREATE TABLE providers (
			name TEXT PRIMARY KEY, base_url TEXT NOT NULL, note TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE provider_routes (
			provider TEXT NOT NULL, protocol TEXT NOT NULL, path TEXT NOT NULL,
			models_path TEXT NOT NULL DEFAULT '', PRIMARY KEY (provider, protocol))`,
		`CREATE TABLE account_models (
			account_id INTEGER NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
			http_status INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
			checked_at INTEGER NOT NULL DEFAULT 0, protocol TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (account_id, model))`,
	}
	// 种子行照 0006 写回，保证升级前的库与真实老库同一个形状。0009 才种下的 gemini 厂商
	// 那时候还不存在，这里不回放：模拟的是真实的 0006 库，不是「倒着算」的库。
	for _, i := range seedRoutesBefore(6) {
		want := seedRoutes[i]
		ddl = append(ddl,
			fmt.Sprintf(`INSERT OR IGNORE INTO providers (name, base_url) VALUES ('%s', '%s')`, want.provider, want.baseURL),
			fmt.Sprintf(`INSERT INTO provider_routes (provider, protocol, path, models_path)
				VALUES ('%s', '%s', '%s', '%s')`, want.provider, want.protocol, want.path, want.modelsPath))
	}
	ddl = append(ddl,
		`INSERT INTO providers (name, base_url, note) VALUES ('myco', 'https://my.example.com', '自建')`,
		`PRAGMA user_version = 6`)
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建老库（%s）: %v", stmt, err)
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

	// 种子逐行断言。升级路径与空库路径必须得出同一份默认协议。
	for _, want := range seedDefaults {
		p, err := st.GetProvider(want.provider)
		if err != nil {
			t.Fatalf("取厂商 %s: %v", want.provider, err)
		}
		if p.DefaultProtocol != want.protocol {
			t.Fatalf("升级后 %s 的 default_protocol = %q，期望 %q", want.provider, p.DefaultProtocol, want.protocol)
		}
	}

	// 用户自建的厂商不在种子名单里，升级后仍是空串，有效默认协议退到排序规则；它没声明路由则报错。
	myco, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取自建厂商: %v", err)
	}
	if myco.DefaultProtocol != "" {
		t.Fatalf("自建厂商的默认协议 = %q，期望空串", myco.DefaultProtocol)
	}
	if _, err := st.EffectiveProviderProtocol("myco"); !errors.Is(err, store.ErrProviderNoRoutes) {
		t.Fatalf("自建厂商没有路由，应报 ErrProviderNoRoutes，实际 %v", err)
	}

	// 升级只加列：路由与地址一个都不动。
	var nRoutes int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM provider_routes").Scan(&nRoutes); err != nil {
		t.Fatalf("数路由行: %v", err)
	}
	if nRoutes != len(seedRoutes) {
		t.Fatalf("升级后的路由数 = %d，期望 %d", nRoutes, len(seedRoutes))
	}
	p, err := st.GetProvider("commandcode")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://api.commandcode.ai/provider/v1" {
		t.Fatalf("升级后 base_url 被改动: %q", p.BaseURL)
	}
}

// TestLookupProviderRoute 覆盖三种情况：命中、厂商没有该协议的路由、厂商不存在。
// 后两种情况都返回 ok=false 且不报错，调用方据此退回后缀猜测。
func TestLookupProviderRoute(t *testing.T) {
	st := openTemp(t)

	route, ok, err := st.LookupProviderRoute("deepseek", "anthropic")
	if err != nil || !ok {
		t.Fatalf("deepseek/anthropic 应命中，实际 ok=%v err=%v", ok, err)
	}
	if route.Path != "/anthropic/v1/messages" || route.ModelsPath != "" {
		t.Fatalf("deepseek/anthropic 路由 = %+v", route)
	}

	if _, ok, err := st.LookupProviderRoute("anthropic", "openai-chat"); err != nil || ok {
		t.Fatalf("anthropic 没有 chat 路由，期望 ok=false 且无错误，实际 ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.LookupProviderRoute("不存在的厂商", "openai-chat"); err != nil || ok {
		t.Fatalf("未知厂商，期望 ok=false 且无错误，实际 ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.LookupProviderRoute("", ""); err != nil || ok {
		t.Fatalf("空厂商，期望 ok=false 且无错误，实际 ok=%v err=%v", ok, err)
	}
}

// TestProviderProtocols 返回该厂商声明过的协议并按协议名排序；没有声明时是 nil。
func TestProviderProtocols(t *testing.T) {
	st := openTemp(t)

	got, err := st.ProviderProtocols("deepseek")
	if err != nil {
		t.Fatalf("取协议: %v", err)
	}
	if len(got) != 2 || got[0] != "anthropic" || got[1] != "openai-chat" {
		t.Fatalf("deepseek 的协议 = %v，期望 [anthropic openai-chat]", got)
	}

	got, err = st.ProviderProtocols("anthropic")
	if err != nil {
		t.Fatalf("取协议: %v", err)
	}
	if len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("anthropic 的协议 = %v，期望只有 anthropic", got)
	}

	got, err = st.ProviderProtocols("不存在的厂商")
	if err != nil {
		t.Fatalf("未知厂商不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("未知厂商的协议 = %v，期望空", got)
	}
}

// TestAddProviderWritesProviderAndRoutes 确认新增厂商与路由在一个事务里落库，
// 同一 (厂商, 协议) 重复添加即覆盖，routes 为空时也能只写厂商行。
func TestAddProviderWritesProviderAndRoutes(t *testing.T) {
	st := openTemp(t)

	if err := st.AddProvider(store.Provider{Name: "myco", BaseURL: "https://my.example.com", Note: "自建"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions", ModelsPath: "/v1/models"}}); err != nil {
		t.Fatalf("新增厂商: %v", err)
	}
	p, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://my.example.com" || p.Note != "自建" {
		t.Fatalf("厂商 = %+v", p)
	}

	// 再加一条 anthropic 路由，并覆盖 chat 那条的 models_path。
	if err := st.AddProvider(store.Provider{Name: "myco", BaseURL: "https://my2.example.com"},
		[]store.ProviderRoute{
			{Protocol: "anthropic", Path: "/anthropic/v1/messages"},
			{Protocol: "openai-chat", Path: "/v1/chat/completions", ModelsPath: "/models"},
		}); err != nil {
		t.Fatalf("再新增厂商: %v", err)
	}

	p, err = st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://my2.example.com" || p.Note != "" {
		t.Fatalf("重复添加应覆盖厂商行，实际 %+v", p)
	}
	routes, err := st.ProviderRoutes("myco")
	if err != nil {
		t.Fatalf("取路由: %v", err)
	}
	if len(routes) != 2 || routes[0].Protocol != "anthropic" || routes[1].Protocol != "openai-chat" {
		t.Fatalf("路由 = %+v，期望按协议名排序的两条", routes)
	}
	if routes[1].ModelsPath != "/models" {
		t.Fatalf("chat 路由的 models_path 应被覆盖，实际 %q", routes[1].ModelsPath)
	}

	// 缺 base_url 或路由缺 path 都要拒绝，且不留下半条记录。
	if err := st.AddProvider(store.Provider{Name: "nobase"}, nil); err == nil {
		t.Fatal("缺 base_url 应报错")
	}
	if err := st.AddProvider(store.Provider{Name: "bad", BaseURL: "https://bad.example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: ""}}); err == nil {
		t.Fatal("路由缺 path 应报错")
	}
	if _, err := st.GetProvider("bad"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("失败的新增不该留下厂商行，实际 err=%v", err)
	}
}

// TestEffectiveProviderProtocol 覆盖三种情况：显式默认优先（哪怕排序排在后面）、
// 空值回退到按协议名排序的第一条路由、一条路由都没有时报错且返回空串。
func TestEffectiveProviderProtocol(t *testing.T) {
	st := openTemp(t)

	// deepseek 的两条路由按协议名排序是 anthropic 在前，但升级前的默认是 openai-chat。
	got, err := st.EffectiveProviderProtocol("deepseek")
	if err != nil {
		t.Fatalf("deepseek 的默认协议: %v", err)
	}
	if got != "openai-chat" {
		t.Fatalf("deepseek 的默认协议 = %q，期望显式值 openai-chat（排序会给 anthropic）", got)
	}
	for _, name := range []string{"anthropic", "openai", "commandcode", "ollama"} {
		want := "openai-chat"
		if name == "anthropic" {
			want = "anthropic"
		}
		got, err := st.EffectiveProviderProtocol(name)
		if err != nil || got != want {
			t.Fatalf("%s 的默认协议 = %q err=%v，期望 %q", name, got, err, want)
		}
	}

	// 没声明默认协议的自建厂商按固定顺序回退：openai-chat 在 anthropic 前，
	// 与协议名字典序相反（字典序会给 anthropic，同一家上游的默认协议就从中变成 anthropic）。
	if err := st.AddProvider(store.Provider{Name: "myco", BaseURL: "https://my.example.com"},
		[]store.ProviderRoute{
			{Protocol: "openai-chat", Path: "/v1/chat/completions"},
			{Protocol: "anthropic", Path: "/v1/messages"},
		}); err != nil {
		t.Fatalf("新增厂商: %v", err)
	}
	if got, err := st.EffectiveProviderProtocol("myco"); err != nil || got != "openai-chat" {
		t.Fatalf("空默认协议应回退到固定顺序第一条 openai-chat，实际 %q err=%v", got, err)
	}

	// 没有路由：空串加明确错误，由调用方决定文案。
	if err := st.AddProvider(store.Provider{Name: "noroute", BaseURL: "https://nr.example.com"}, nil); err != nil {
		t.Fatalf("新增无路由厂商: %v", err)
	}
	got, err = st.EffectiveProviderProtocol("noroute")
	if !errors.Is(err, store.ErrProviderNoRoutes) {
		t.Fatalf("无路由厂商应报 ErrProviderNoRoutes，实际 %v", err)
	}
	if got != "" {
		t.Fatalf("无路由厂商的默认协议 = %q，期望空串", got)
	}

	// 厂商不存在时报 ErrProviderNotFound，与「厂商在但没路由」分开。
	if _, err := st.EffectiveProviderProtocol("不存在的厂商"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("未知厂商应报 ErrProviderNotFound，实际 %v", err)
	}
}

// TestResolveDefaultProtocol 固定纯规则本身：显式值落在已声明路由上才赢，
// 指不到路由与空值都按固定顺序 openai-chat → openai-responses → anthropic 回退，空路由返回空串。
func TestResolveDefaultProtocol(t *testing.T) {
	routes := []store.ProviderRoute{{Protocol: "anthropic"}, {Protocol: "openai-chat"}}

	if got := store.ResolveDefaultProtocol("openai-chat", routes); got != "openai-chat" {
		t.Fatalf("显式值落在路由上应优先，实际 %q（排序兜底会给 openai-chat，这里要看显式值本身）", got)
	}
	if got := store.ResolveDefaultProtocol("", routes); got != "openai-chat" {
		t.Fatalf("空值应取固定顺序第一条 openai-chat，实际 %q", got)
	}
	if got := store.ResolveDefaultProtocol("", []store.ProviderRoute{{Protocol: "anthropic"}, {Protocol: "openai-responses"}}); got != "openai-responses" {
		t.Fatalf("没有 chat 路由时应取 openai-responses，实际 %q", got)
	}
	if got := store.ResolveDefaultProtocol("", []store.ProviderRoute{{Protocol: "anthropic"}}); got != "anthropic" {
		t.Fatalf("只有 anthropic 路由时应取 anthropic，实际 %q", got)
	}
	// 历史脏数据：默认协议指向一条已经不存在的路由，按固定顺序回退到还在的那条。
	if got := store.ResolveDefaultProtocol("openai-responses", routes); got != "openai-chat" {
		t.Fatalf("显式值不在路由里应回退到 openai-chat，实际 %q", got)
	}
	if got := store.ResolveDefaultProtocol("", nil); got != "" {
		t.Fatalf("无路由应返回空串，实际 %q", got)
	}
}

// TestAddProviderValidatesDefaultProtocol 确认默认协议必须落在已声明的路由上：
// 指不到就整笔拒绝，文案里带「先加路由、或改成已有协议」两条路；传空串仍可通过。
func TestAddProviderValidatesDefaultProtocol(t *testing.T) {
	st := openTemp(t)

	// 默认协议落在这次传入的路由上。
	if err := st.AddProvider(store.Provider{
		Name: "myco", BaseURL: "https://my.example.com", DefaultProtocol: "openai-chat",
	}, []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("默认协议属于本次路由: %v", err)
	}
	if got, err := st.EffectiveProviderProtocol("myco"); err != nil || got != "openai-chat" {
		t.Fatalf("默认协议 = %q err=%v，期望 openai-chat", got, err)
	}

	// 默认协议落在之前已经声明的路由上：这次不传路由也认。
	if err := st.AddProvider(store.Provider{Name: "myco", BaseURL: "https://my.example.com"},
		[]store.ProviderRoute{{Protocol: "anthropic", Path: "/v1/messages"}}); err != nil {
		t.Fatalf("补一条 anthropic 路由: %v", err)
	}
	if err := st.AddProvider(store.Provider{
		Name: "myco", BaseURL: "https://my.example.com", DefaultProtocol: "anthropic",
	}, nil); err != nil {
		t.Fatalf("默认协议属于之前声明的路由: %v", err)
	}
	if got, err := st.EffectiveProviderProtocol("myco"); err != nil || got != "anthropic" {
		t.Fatalf("默认协议 = %q err=%v，期望 anthropic", got, err)
	}

	// 传空串不报错，也不改已存的默认协议。
	if err := st.AddProvider(store.Provider{Name: "myco", BaseURL: "https://my2.example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("空默认协议应可通过: %v", err)
	}
	if got, err := st.EffectiveProviderProtocol("myco"); err != nil || got != "anthropic" {
		t.Fatalf("空默认协议不该覆盖已存值，实际 %q err=%v", got, err)
	}

	// 指向这家没有声明的协议：拒绝，且文案说清楚缺哪条、下一步两条路。
	err := st.AddProvider(store.Provider{
		Name: "myco", BaseURL: "https://my.example.com", DefaultProtocol: "openai-responses",
	}, nil)
	if err == nil {
		t.Fatal("默认协议不在已声明路由里应报错")
	}
	for _, want := range []string{"openai-responses", "provider set myco", "--responses", "已声明：anthropic、openai-chat", "把默认协议改成"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案 %q 缺少 %q", err.Error(), want)
		}
	}
	// 拒绝是整笔回滚：base_url 与默认协议都不该被改。
	p, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "https://my2.example.com" || p.DefaultProtocol != "anthropic" {
		t.Fatalf("被拒的新增留下了改动: %+v", p)
	}

	// 这家一条路由都没有时也要拒绝，并提示先加路由。
	err = st.AddProvider(store.Provider{
		Name: "noroute", BaseURL: "https://nr.example.com", DefaultProtocol: "openai-chat",
	}, nil)
	if err == nil {
		t.Fatal("没有任何路由时设默认协议应报错")
	}
	for _, want := range []string{"还没有任何路由", "provider set noroute", "--openai"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案 %q 缺少 %q", err.Error(), want)
		}
	}
	if _, err := st.GetProvider("noroute"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("被拒的新增不该留下厂商行，实际 err=%v", err)
	}
}

// TestDeleteProviderRemovesRoutes 确认删厂商连带删路由，删不存在的厂商报 ErrProviderNotFound。
func TestDeleteProviderRemovesRoutes(t *testing.T) {
	st := openTemp(t)

	if err := st.DeleteProvider("commandcode"); err != nil {
		t.Fatalf("删厂商: %v", err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM provider_routes WHERE provider = 'commandcode'`).Scan(&n); err != nil {
		t.Fatalf("数路由行: %v", err)
	}
	if n != 0 {
		t.Fatalf("删厂商后还剩 %d 条路由", n)
	}
	if _, ok, err := st.LookupProviderRoute("commandcode", "openai-chat"); err != nil || ok {
		t.Fatalf("删掉之后路由应不存在，实际 ok=%v err=%v", ok, err)
	}

	if err := st.DeleteProvider("commandcode"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("重复删除应报 ErrProviderNotFound，实际 %v", err)
	}
}

// TestEmptyProvidersIsNotAnError 确认用户把种子厂商全删光之后一切照旧：
// 列表是空切片，路由查询是 ok=false，都不报错。
func TestEmptyProvidersIsNotAnError(t *testing.T) {
	st := openTemp(t)

	providers, err := st.ListProviders()
	if err != nil {
		t.Fatalf("列厂商: %v", err)
	}
	for _, p := range providers {
		if err := st.DeleteProvider(p.Name); err != nil {
			t.Fatalf("删厂商 %s: %v", p.Name, err)
		}
	}

	providers, err = st.ListProviders()
	if err != nil {
		t.Fatalf("列为空也应是空列表而不是错误: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("厂商列表 = %+v，期望空", providers)
	}
	if got, err := st.ProviderProtocols("deepseek"); err != nil || len(got) != 0 {
		t.Fatalf("空表的协议 = %v err=%v，期望空且无错误", got, err)
	}
}

// TestListAccountsByProvider 给 `provider rm` 用：按厂商列出还在用它的账号。
func TestListAccountsByProvider(t *testing.T) {
	st := openTemp(t)

	for _, c := range []struct{ name, provider string }{
		{"ds1", "deepseek"}, {"ds2", "deepseek"}, {"cc1", "commandcode"},
	} {
		acc := newAccount(t, st, c.name, nil)
		acc.Provider = c.provider
		if _, err := st.DB().Exec(`UPDATE accounts SET provider = ? WHERE id = ?`, c.provider, acc.ID); err != nil {
			t.Fatalf("改账号厂商: %v", err)
		}
	}

	got, err := st.ListAccountsByProvider("deepseek")
	if err != nil {
		t.Fatalf("按厂商列账号: %v", err)
	}
	if len(got) != 2 || got[0].Name != "ds1" || got[1].Name != "ds2" {
		t.Fatalf("deepseek 的账号 = %+v", got)
	}

	got, err = st.ListAccountsByProvider("openai")
	if err != nil {
		t.Fatalf("按厂商列账号: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("openai 的账号 = %+v，期望空", got)
	}
}

// latestMigrationVersion 数出 migrations 目录里最大的迁移文件号，也就是「升到最新」的目标。
// 直接写死数字的话，新增一个迁移就要来改这条用例，而它想钉的是「链条能走到底」。
func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("读迁移目录: %v", err)
	}
	latest := 0
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil {
			t.Fatalf("迁移文件名 %q 不是 <四位序号>_<名字>.sql: %v", e.Name(), err)
		}
		if v > latest {
			latest = v
		}
	}
	if latest == 0 {
		t.Fatal("迁移目录里没有文件")
	}
	return latest
}

// assertLatestVersion 确认库已经是最新形状。
//
// 迁移链已经不存在（表结构由 AutoMigrate 从 model 定义推导），所以不再验 user_version 等于
// 目录里的最大编号，而是验两件事：八张表一张不少，且旧版迁移器留下的版本号已经归零。
func assertLatestVersion(t *testing.T, st *store.Store) {
	t.Helper()
	var version int
	if err := st.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读 user_version: %v", err)
	}
	if version != 0 {
		t.Fatalf("user_version = %d，期望 0（旧版迁移器已被 AutoMigrate 取代）", version)
	}
	for _, table := range []string{
		"accounts", "requests", "providers", "provider_routes",
		"provider_headers", "provider_models", "account_models", "model_aliases",
	} {
		var name string
		err := st.DB().QueryRow(
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 不存在: %v", table, err)
		}
	}
}

// TestMigrationChainFrom0001ToLatest 确认一个停在 0001 的老库能沿迁移链一路升到最新：
// providers、provider_routes 与 account_models.protocol 三者都补上，种子厂商与路由逐条对上，
// 只存在于 0001 的 agents 表被 0002 删掉，老账号与老请求记录一个字段都不动。
//
// 单步升级的用例各自只跨一两个版本，链条中间漏一环（例如某个迁移只断言过空库、
// 在带数据的库上会失败）只有从最老的版本一次走到底才能发现。
func TestMigrationChainFrom0001ToLatest(t *testing.T) {
	path := t.TempDir() + "/v1.db"

	// 老库用 0001 的迁移文件本身来建，不手抄 DDL：手抄的形状会随实现漂移，
	// 而这里要验的正是真实老库升上来的结果。
	initSQL, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("读迁移 0001: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	if _, err := db.Exec(string(initSQL)); err != nil {
		t.Fatalf("建 0001 老库: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('ds1', 'deepseek', 'openai-chat', 'https://api.deepseek.com', X'010203', '["m1"]', 1, 100)`,
		`INSERT INTO requests (ts, inbound, outbound, model_requested, model_used, account_id,
			http_status, ok, in_tokens, out_tokens, latency_ms)
			VALUES (123, 'anthropic', 'openai-chat', 'm1', 'm1', 1, 200, 1, 7, 3, 42)`,
		`INSERT INTO agents (name, config_path) VALUES ('claude', '/tmp/claude.json')`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("写入 0001 老库（%s）: %v", stmt, err)
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

	// providers：种子都在，默认协议逐行对上。
	providers, err := st.ListProviders()
	if err != nil {
		t.Fatalf("列厂商: %v", err)
	}
	if len(providers) != len(seedDefaults) {
		t.Fatalf("升级后的厂商数 = %d，期望 %d", len(providers), len(seedDefaults))
	}
	assertSeedDefaults(t, st)

	// provider_routes：每条 (厂商, 协议) 都在，路径与种子一致。
	// 这里不比 models_path：模型列表路径怎么存是实现细节，
	// 升级要保证的是「路由还在、地址没变」。
	for _, want := range seedRoutes {
		route, ok, err := st.LookupProviderRoute(want.provider, want.protocol)
		if err != nil {
			t.Fatalf("取路由 %s/%s: %v", want.provider, want.protocol, err)
		}
		if !ok {
			t.Fatalf("升级后缺路由 %s/%s", want.provider, want.protocol)
		}
		if route.Path != want.path {
			t.Fatalf("路由 %s/%s 的 path = %q，期望 %q", want.provider, want.protocol, route.Path, want.path)
		}
	}

	// 可达性：0001 里手填的 m1 由 0015 补成 allowed，同步进来的 m2 也是 allowed。
	// 0015 之前这里断言的是「账号的模型集合被整体改写成 m2」——accounts.models 那一列已经删掉，
	// 同步只做加法。
	if _, err := st.ReplaceModels(1, []string{"m2"}); err != nil {
		t.Fatalf("写入模型行: %v", err)
	}
	rows, err := st.AccountModels(1)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 2 || rows[0].Model != "m1" || rows[1].Model != "m2" {
		t.Fatalf("模型行 = %+v，期望 m1（0015 从手填清单补的）与 m2", rows)
	}
	for _, r := range rows {
		if r.Access != store.AccessAllowed {
			t.Fatalf("模型 %s 的可达性 = %q，期望 %q", r.Model, r.Access, store.AccessAllowed)
		}
		if r.Protocol != "" {
			t.Fatalf("新行的 protocol = %q，期望空串（继承厂商默认协议）", r.Protocol)
		}
	}

	// 0002 删掉的 agents 表不该留下。
	var nAgents int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agents'`).Scan(&nAgents); err != nil {
		t.Fatalf("查 agents 表: %v", err)
	}
	if nAgents != 0 {
		t.Fatalf("升级后 agents 表还在，期望被 0002 删掉")
	}

	// 老数据原样：升级只加表加列，不动账号本身与请求记录。
	// 账号的模型集合是派生值（access='allowed' 的行），上面那一步同步进来 m2 之后是 m1 与 m2 两个。
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	if acc.Provider != "deepseek" || acc.Protocol != "openai-chat" ||
		acc.BaseURL != "https://api.deepseek.com" ||
		len(acc.Models) != 2 || acc.Models[0] != "m1" || acc.Models[1] != "m2" {
		t.Fatalf("老账号被改动: %+v", acc)
	}
	recs, err := st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读老请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].InTokens != 7 || recs[0].OutTokens != 3 || !recs[0].OK {
		t.Fatalf("老请求记录 = %+v，期望 token 7/3 且 ok", recs)
	}
}
