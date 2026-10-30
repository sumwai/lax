package store_test

import (
	"database/sql"
	"testing"
	"time"

	"lax/internal/store"

	_ "modernc.org/sqlite"
)

// floatPtr 是价格参数的取值辅助：nil 表示「这一列不动」。
func floatPtr(v float64) *float64 { return &v }

// TestUncachedInTokensByOutboundProtocol 钉住输入 token 的协议差异（DESIGN 5.5）：
// anthropic 的 input_tokens 已排除缓存读写，直接用；chat、responses 与 gemini 的上游
// 把命中缓存的部分也算在输入里，要减掉 cache_read，减到 0 为止。
// systemone 是决策协议，只报 input_tokens / output_tokens，没有缓存字段（DESIGN 5.14），
// 与 anthropic 一样不减。
func TestUncachedInTokensByOutboundProtocol(t *testing.T) {
	cases := []struct {
		name     string
		outbound string
		in       int64
		cache    int64
		want     int64
	}{
		{"anthropic 不减", "anthropic", 100, 60, 100},
		{"anthropic 没有缓存", "anthropic", 100, 0, 100},
		{"chat 减掉命中缓存的部分", "openai-chat", 100, 60, 40},
		{"responses 同样减", "openai-responses", 100, 25, 75},
		{"gemini 同样减", "gemini", 100, 40, 60},
		{"systemone 的 input_tokens 不含缓存，不减", "systemone", 100, 60, 100},
		{"systemone 没有缓存字段时原样", "systemone", 100, 0, 100},
		{"缓存比输入还多时夹到 0", "openai-chat", 10, 50, 0},
		{"刚好全命中", "gemini", 100, 100, 0},
		{"没有用量时是 0", "openai-chat", 0, 0, 0},
	}
	for _, c := range cases {
		if got := store.UncachedInTokens(c.outbound, c.in, c.cache); got != c.want {
			t.Fatalf("%s：UncachedInTokens(%q, %d, %d) = %d，期望 %d",
				c.name, c.outbound, c.in, c.cache, got, c.want)
		}
	}
}

// TestUsageCostUsesCachePrices 钉住花费公式（DESIGN 5.5）：
// 未命中缓存的输入按 price_in、缓存读按 price_cache_read、缓存写按 price_cache_write、
// 输出按 price_out，四项都除以 1e6。历史行（in_tokens_uncached 还没归一化过）不算在内。
func TestUsageCostUsesCachePrices(t *testing.T) {
	st := openTemp(t)

	// 1M 未命中输入 × 2 + 2M 缓存读 × 0.5 + 1M 缓存写 × 3 + 1M 输出 × 4 = 2 + 1 + 3 + 4 = 10
	acc := store.Account{
		Name: "ds1", Provider: "test", Protocol: "openai-chat",
		BaseURL: "http://127.0.0.1:1", KeyCipher: []byte{1}, Models: []string{"m1"},
		Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.SetAccountModelPrices("ds1", "m1", store.ModelPrices{
		In: floatPtr(2), Out: floatPtr(4),
		CacheRead: floatPtr(0.5), CacheWrite: floatPtr(3),
	}); err != nil {
		t.Fatalf("写单价: %v", err)
	}
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true,
		InTokens: 3_000_000, InTokensUncached: 1_000_000, OutTokens: 1_000_000,
		CacheRead: 2_000_000, CacheWrite: 1_000_000,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}

	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("按账号汇总: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if rows[0].Cost == nil || *rows[0].Cost != 10 {
		t.Fatalf("花费 = %v，期望 10（输入 2 + 缓存读 1 + 缓存写 3 + 输出 4）", rows[0].Cost)
	}
	// 展示用的输入 token 仍是上游原值，只有花费用归一化后的那一列。
	if rows[0].InTokens != 3_000_000 {
		t.Fatalf("输入 token 合计 = %d，期望上游原值 3000000", rows[0].InTokens)
	}
}

// TestUsageCostFallsBackToInputPrice 钉住两个回退：
// 缓存单价缺失时按输入价折；只填输入输出价时的金额与旧公式（in_tokens × price_in + out × price_out）一致。
func TestUsageCostFallsBackToInputPrice(t *testing.T) {
	st := openTemp(t)
	acc := store.Account{
		Name: "ds1", Provider: "test", Protocol: "anthropic",
		BaseURL: "http://127.0.0.1:1", KeyCipher: []byte{1}, Models: []string{"m1"},
		Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.SetAccountModelPrices("ds1", "m1", store.ModelPrices{
		In: floatPtr(2), Out: floatPtr(4),
	}); err != nil {
		t.Fatalf("写单价: %v", err)
	}

	// 只有输入输出价时：1M 未命中输入 + 1M 输出 = 6，与旧公式一模一样。
	plain := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, InTokens: 1_000_000, InTokensUncached: 1_000_000,
		OutTokens: 1_000_000,
	}
	if err := st.InsertRequest(&plain); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}
	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Cost == nil || *rows[0].Cost != 6 {
		t.Fatalf("只有输入输出价时的花费 = %+v，期望 6", rows)
	}

	// 缓存读写按输入价回退：再记 1M 缓存读 + 1M 缓存写，各按 2 元算，合计 6 + 2 + 2 = 10。
	cached := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, CacheRead: 1_000_000, CacheWrite: 1_000_000,
	}
	if err := st.InsertRequest(&cached); err != nil {
		t.Fatalf("写第二条请求记录: %v", err)
	}
	rows, err = st.Usage("account", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("再汇总: %v", err)
	}
	if rows[0].Cost == nil || *rows[0].Cost != 10 {
		t.Fatalf("缓存单价缺失时的花费 = %v，期望 10（缓存读写各按输入价 2 元）", rows[0].Cost)
	}
}

// TestUsageCostWithoutAnyPrice 钉住「缺价记 null」（DESIGN 5.5）：一个价都没填时花费是 nil（未定价），
// 而不是 0——0 是「确实不花钱」，两件事混成一个数就看不出该不该去补价。
func TestUsageCostWithoutAnyPrice(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", []string{"m1"})
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, InTokens: 100, InTokensUncached: 100, OutTokens: 50,
		CacheRead: 1000, CacheWrite: 1000,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}
	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Cost != nil {
		t.Fatalf("没定价时的花费 = %+v，期望 nil（未定价）", rows)
	}
	// 缺价不影响 token 合计：用量是真的，只是不知道多少钱。
	if rows[0].InTokens != 100 || rows[0].OutTokens != 50 || rows[0].CacheRead != 1000 {
		t.Fatalf("缺价不该把用量一起抹掉: %+v", rows[0])
	}
}

// TestAccountRowHasNoPriceColumns 钉住迁移 0016 的结果：账号行上不再有四个价格列，
// 单价只存在 provider_models 与 account_models 上（DESIGN 5.13）。
func TestAccountRowHasNoPriceColumns(t *testing.T) {
	st := openTemp(t)
	// 从空库跑到最新也该是这个形状，不只能升级来的库。
	assertAccountHasNoPriceColumns(t, st)

	newAccount(t, st, "ds1", []string{"m1"})
	assertAccountHasNoPriceColumns(t, st)
}

// assertAccountHasNoPriceColumns 直接问表结构：四个价格列一个都不许在。
// 写字入口已经不碰它们了，只剩迁移能保证它们真的消失。
func assertAccountHasNoPriceColumns(t *testing.T, st *store.Store) {
	t.Helper()
	rows, err := st.DB().Query(`PRAGMA table_info(accounts)`)
	if err != nil {
		t.Fatalf("读 accounts 表结构: %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, kind string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("读列: %v", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读列: %v", err)
	}
	for _, col := range cols {
		switch col {
		case "price_in", "price_out", "price_cache_read", "price_cache_write":
			t.Fatalf("accounts 上还有价格列 %q：迁移 0016 应该删掉它，现有列 %v", col, cols)
		}
	}
}

// TestMigration0011BackfillsUncachedTokens 模拟一个停在 0010 的库：
// 升上来之后两列缓存单价是 NULL（老账号回退到输入价），历史请求行的
// in_tokens_uncached 按当时的 in_tokens 回填，其余字段一个不动。
func TestMigration0011BackfillsUncachedTokens(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 0010 之后的形状：accounts 还没有缓存单价，requests 还没有 in_tokens_uncached。
	// providers 与 provider_routes 得建出来：0010 那会儿它们已经在（0006 建的），
	// 迁移 0014 会按它们判断哪个账号属于孤儿、要不要补建厂商。
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
		`CREATE TABLE providers (
			name TEXT PRIMARY KEY, base_url TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
			default_protocol TEXT NOT NULL DEFAULT '', models_path TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE provider_routes (
			provider TEXT NOT NULL, protocol TEXT NOT NULL, path TEXT NOT NULL,
			models_path TEXT NOT NULL DEFAULT '', PRIMARY KEY (provider, protocol))`,
		`INSERT INTO providers (name, base_url) VALUES ('test', 'http://127.0.0.1:1')`,
		`INSERT INTO provider_routes (provider, protocol, path)
			VALUES ('test', 'openai-chat', '/v1/chat/completions')`,
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, price_in, price_out, enabled, created_at)
			VALUES ('ds1', 'test', 'openai-chat', 'http://127.0.0.1:1', X'010203', '["m1"]', 2, 4, 1, 100)`,
		`INSERT INTO requests (ts, inbound, outbound, model_requested, model_used, account_id,
			http_status, ok, in_tokens, out_tokens, cache_read, cache_write, latency_ms, error)
			VALUES (1700000000, 'anthropic', 'openai-chat', 'm1', 'm1', 1, 200, 1, 321, 45, 100, 20, 900, '')`,
		`PRAGMA user_version = 10`,
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

	recs, err := st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读历史行: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("历史行数 = %d，期望 1", len(recs))
	}
	if recs[0].InTokens != 321 || recs[0].InTokensUncached != 321 {
		t.Fatalf("历史行的输入 token = %d / 归一化后 %d，期望两边都是原值 321",
			recs[0].InTokens, recs[0].InTokensUncached)
	}
	if recs[0].OutTokens != 45 || recs[0].CacheRead != 100 || recs[0].CacheWrite != 20 {
		t.Fatalf("历史行的其它用量被改动: %+v", recs[0])
	}

	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	// 老账号的价搬进了 account_models 的覆盖值（迁移 0016）：这个账号名下的 m1 行拿到 2 与 4，
	// 缓存两列本来就没填，搬完仍是空（回退输入价）。
	info := oneInfo(t, st, acc.ID, "m1")
	if info.PriceIn == nil || *info.PriceIn != 2 || info.PriceOut == nil || *info.PriceOut != 4 {
		t.Fatalf("老账号的输入输出价没搬成覆盖值: %v / %v", info.PriceIn, info.PriceOut)
	}
	if info.PriceCacheRead != nil || info.PriceCacheWrite != nil {
		t.Fatalf("老账号的缓存单价应为空（回退输入价），实际 %v / %v",
			info.PriceCacheRead, info.PriceCacheWrite)
	}

	// 回填之后老数据的估算与旧公式一致：1M 未命中输入按 2 元 + 1M 输出按 4 元。
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, InTokens: 1_000_000, InTokensUncached: 1_000_000,
		OutTokens: 1_000_000,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写新行: %v", err)
	}
	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Cost == nil || *rows[0].Cost != 6 {
		t.Fatalf("新行的花费 = %+v，期望 6", rows)
	}
}
