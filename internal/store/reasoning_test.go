package store_test

import (
	"database/sql"
	"math"
	"testing"
	"time"

	"lax/internal/store"

	_ "github.com/glebarez/go-sqlite"
)

// TestReasoningExtra 钉住思考 token 的判定：按上报的数字判，不按协议猜。
// extra 只有「思考是输出之外的一项」时非 0，其余情况为 0，原值本身不受影响。
func TestReasoningExtra(t *testing.T) {
	cases := []struct {
		name                   string
		total, in, out, reason int64
		want                   int64
	}{
		// 中转：prompt 7 + completion 1 + reasoning 124 = total 132，思考是独立的一项。
		{"独立一项", 132, 7, 1, 124, 124},
		// OpenAI 官方：reasoning 是 completion 的子集，total 就是 prompt + completion。
		{"子集", 8, 7, 1, 124, 0},
		// total 比 prompt + completion 还小时也按子集处理：没有证据说它独立成项。
		{"子集且 total 偏小", 5, 7, 1, 124, 0},
		// 上游没报 total：按子集处理，宁可少算一次也不重复计价。
		{"无 total", 0, 7, 1, 124, 0},
		// 没有思考 token 时怎么判都是 0。
		{"reasoning 为 0", 132, 7, 1, 0, 0},
		{"reasoning 为 0 且 total 缺省", 0, 7, 1, 0, 0},
	}
	for _, c := range cases {
		if got := store.ReasoningExtra(c.total, c.in, c.out, c.reason); got != c.want {
			t.Fatalf("%s：ReasoningExtra(%d, %d, %d, %d) = %d，期望 %d",
				c.name, c.total, c.in, c.out, c.reason, got, c.want)
		}
	}
}

// TestUsageCostCountsIndependentReasoning 钉住独立思考 token 的花费：
// 输出价 8、prompt 7、completion 1、total 132、reasoning 124 时，花费是 (1+124) × 8 / 1e6。
// 只按 completion 算输出会把这 124 个思考 token 的九成以上花费漏掉。
func TestUsageCostCountsIndependentReasoning(t *testing.T) {
	st := openTemp(t)
	priceOut := 8.0
	acc := store.Account{
		Name: "relay", Provider: "test", Protocol: "openai-chat",
		BaseURL: "http://127.0.0.1:1", KeyCipher: []byte{1}, Models: []string{"m1"},
		Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	// 单价不在账号上：写到这个账号在这个模型上的覆盖值。输入价不填，
	// 这个用例只看输出侧，没价的输入项不计（不是按 0 元算）。
	if err := st.SetAccountModelPrices("relay", "m1", store.ModelPrices{Out: &priceOut}); err != nil {
		t.Fatalf("写输出价: %v", err)
	}

	const in, out, total, reasoning = 7, 1, 132, 124
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "openai-chat", Outbound: "openai-chat",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true,
		InTokens: in, InTokensUncached: in, OutTokens: out,
		ReasoningTokens:      reasoning,
		ReasoningTokensExtra: store.ReasoningExtra(total, in, out, reasoning),
	}
	if err := st.RecordAttempt(&rec, store.AttemptState{Clear: true}); err != nil {
		t.Fatalf("记一次尝试: %v", err)
	}

	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix(), 0)
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，期望 1", len(rows))
	}
	want := float64(out+reasoning) * 8 / 1e6
	if math.Abs(costOf(t, rows[0])-want) > 1e-15 {
		t.Fatalf("花费 = %v，期望 (1+124)×8/1e6 = %v", rows[0].Cost, want)
	}
	// 思考 token 原值整列进汇总，展示与计价是两件事。
	if rows[0].ReasoningTokens != reasoning {
		t.Fatalf("思考 token 合计 = %d，期望 %d", rows[0].ReasoningTokens, reasoning)
	}
	if rows[0].OutTokens != out {
		t.Fatalf("输出 token 合计 = %d，期望 %d（思考不并进输出）", rows[0].OutTokens, out)
	}
}

// TestUsageCostIgnoresSubsetReasoning 钉住子集场景不重复计价：total = prompt + completion 时
// reasoning 已经在输出里，花费只按 completion 算一遍。
func TestUsageCostIgnoresSubsetReasoning(t *testing.T) {
	st := openTemp(t)
	priceIn, priceOut := 2.0, 8.0
	acc := store.Account{
		Name: "official", Provider: "test", Protocol: "openai-chat",
		BaseURL: "http://127.0.0.1:1", KeyCipher: []byte{1}, Models: []string{"m1"},
		Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.SetAccountModelPrices("official", "m1", store.ModelPrices{
		In: &priceIn, Out: &priceOut,
	}); err != nil {
		t.Fatalf("写单价: %v", err)
	}

	const in, out, reasoning int64 = 7, 1, 124
	total := in + out
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "openai-chat", Outbound: "openai-chat",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true,
		InTokens: in, InTokensUncached: in, OutTokens: out,
		ReasoningTokens:      reasoning,
		ReasoningTokensExtra: store.ReasoningExtra(total, in, out, reasoning),
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}

	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix(), 0)
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	// 7 × 2 + 1 × 8 = 22，思考不再另算，否则会凭空多出 124 × 8 / 1e6。
	want := float64(in)*priceIn/1e6 + float64(out)*priceOut/1e6
	if math.Abs(costOf(t, rows[0])-want) > 1e-15 {
		t.Fatalf("子集场景的花费 = %v，期望 %v（思考已在输出里，不重复计价）", rows[0].Cost, want)
	}
	// 展示上仍然能看到思考 token：0 extra 不等于 0 思考。
	if rows[0].ReasoningTokens != reasoning {
		t.Fatalf("思考 token 合计 = %d，期望仍然显示 %d", rows[0].ReasoningTokens, reasoning)
	}
}

// TestReasoningColumnsDefaultToZeroFromEmptyDB 确认空库跑到最新之后两列都在，默认值是 0：
// 没读过思考 token 的行（anthropic、失败行）不会因为新列变成 NULL。
func TestReasoningColumnsDefaultToZeroFromEmptyDB(t *testing.T) {
	st := openTemp(t)
	assertLatestVersion(t, st)

	acc := newAccount(t, st, "ds1", []string{"m1"})
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, InTokens: 30, InTokensUncached: 30, OutTokens: 6,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}
	recs, err := st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].ReasoningTokens != 0 || recs[0].ReasoningTokensExtra != 0 {
		t.Fatalf("没读过思考 token 的行两列都应是 0，实际 %+v", recs)
	}
}

// TestMigration0012KeepsHistoryAtZero 模拟一个停在 0011 的库：升上来之后 requests 多出
// reasoning_tokens 与 reasoning_tokens_extra 两列，历史行都是 0，其余字段与花费一个不动。
func TestMigration0012KeepsHistoryAtZero(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 0011 之后的形状：requests 已经有 in_tokens_uncached，没有 reasoning 两列。
	// providers 与 provider_routes 得建出来：0011 那会儿它们已经在（0006 建的），
	// 迁移 0014 会按它们判断哪个账号属于孤儿、要不要补建厂商。
	ddl := []string{
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
			protocol TEXT NOT NULL, base_url TEXT NOT NULL, key_cipher BLOB NOT NULL,
			models TEXT NOT NULL DEFAULT '[]', price_in REAL, price_out REAL,
			price_cache_read REAL, price_cache_write REAL,
			priority INTEGER NOT NULL DEFAULT 100, enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER, last_error TEXT,
			fail_streak INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE requests (
			id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, inbound TEXT NOT NULL,
			outbound TEXT NOT NULL, model_requested TEXT NOT NULL, model_used TEXT NOT NULL,
			account_id INTEGER, http_status INTEGER NOT NULL, ok INTEGER NOT NULL,
			in_tokens INTEGER NOT NULL DEFAULT 0, in_tokens_uncached INTEGER NOT NULL DEFAULT 0,
			out_tokens INTEGER NOT NULL DEFAULT 0,
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
			VALUES ('ds1', 'test', 'openai-chat', 'http://127.0.0.1:1', X'010203', '["m1"]', 2, 8, 1, 100)`,
		`INSERT INTO requests (ts, inbound, outbound, model_requested, model_used, account_id,
			http_status, ok, in_tokens, in_tokens_uncached, out_tokens, cache_read, cache_write, latency_ms, error)
			VALUES (1700000000, 'openai-chat', 'openai-chat', 'm1', 'm1', 1, 200, 1, 7, 7, 1, 0, 0, 900, '')`,
		`PRAGMA user_version = 11`,
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
	if recs[0].ReasoningTokens != 0 || recs[0].ReasoningTokensExtra != 0 {
		t.Fatalf("历史行的思考两列应都是 0，实际 %d / %d",
			recs[0].ReasoningTokens, recs[0].ReasoningTokensExtra)
	}
	if recs[0].InTokens != 7 || recs[0].OutTokens != 1 || recs[0].InTokensUncached != 7 {
		t.Fatalf("历史行的其它用量被改动: %+v", recs[0])
	}

	// 历史花费与升级前一致：7 × 2 / 1e6 + 1 × 8 / 1e6，思考两列是 0 不改金额。
	rows, err := st.Usage("account", 0, 0)
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	want := 7*2.0/1e6 + 1*8.0/1e6
	if len(rows) != 1 || math.Abs(costOf(t, rows[0])-want) > 1e-15 {
		t.Fatalf("历史行的花费 = %+v，期望 %v", rows, want)
	}
	if rows[0].ReasoningTokens != 0 {
		t.Fatalf("历史行的思考合计 = %d，期望 0", rows[0].ReasoningTokens)
	}
}
