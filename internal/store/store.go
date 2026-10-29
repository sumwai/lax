// Package store 负责 SQLite 连接、迁移与账号、请求记录、客户端状态的读写。
package store

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"switchboard/internal/translate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 是数据库句柄。写入串行化，连接数固定为 1。
type Store struct {
	db *sql.DB
}

// Account 是一行账号。
//
// Provider 是唯一的地址来源：转发用的地址、默认协议与路由都按它在 providers / provider_routes
// 里查（见 DESIGN 5.8）。BaseURL 与 Protocol 是只读派生值，账号表里没有这两列（迁移 0014 删掉了）：
// 读账号时由 queryAccounts 填上，写入（CreateAccount）不看它们，改了也不会落库。
type Account struct {
	ID       int64
	Name     string
	Provider string
	// BaseURL 派生自 providers.base_url，厂商行不存在时为空串。
	BaseURL string
	// Protocol 派生自这家厂商的有效默认协议（ResolveDefaultProtocol 的结果：显式值落在已声明的
	// 路由上才用它，否则按固定顺序取第一条，没有路由时为空串），厂商行不存在时同样是空串。
	// 逐模型的协议覆盖仍在 account_models.protocol，见 EffectiveProtocol。
	Protocol  string
	KeyCipher []byte
	Models    []string
	PriceIn   *float64
	PriceOut  *float64
	// PriceCacheRead 与 PriceCacheWrite 是缓存读写的单价（每百万 token），可空：
	// 空表示没填，花费估算回退到输入价（见 Usage）。
	PriceCacheRead  *float64
	PriceCacheWrite *float64
	Priority        int
	Enabled         bool
	CooldownUntil   int64
	LastError       string
	// FailStreak 是连续失败次数，跨请求累计，用于冷却翻倍。成功一次即清零。
	FailStreak int
	CreatedAt  int64
}

// RequestRecord 是一次上游尝试。
type RequestRecord struct {
	ID             int64
	TS             int64
	Inbound        string
	Outbound       string
	ModelRequested string
	ModelUsed      string
	AccountID      int64
	HTTPStatus     int
	OK             bool
	InTokens       int64
	// InTokensUncached 是归一化之后的输入 token：没命中缓存的那部分，花费按它折输入价。
	// 入账时由 UncachedInTokens 按出站协议算出，见那里与 DESIGN 5.5。
	InTokensUncached int64
	OutTokens        int64
	CacheRead        int64
	CacheWrite       int64
	// ReasoningTokens 是上游上报的思考 token 原值（anthropic 没有独立字段，恒为 0），
	// ReasoningTokensExtra 是其中「输出之外的一项」那部分：只给 extra 按输出价再计一次，
	// 判定由 ReasoningExtra 给（见 DESIGN 5.5）。两列都进 requests 的独立列。
	ReasoningTokens      int64
	ReasoningTokensExtra int64
	LatencyMS            int64
	Error                string
}

// Open 打开数据库，按需建目录并执行迁移。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接，供测试与统计使用。
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}

	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}

	files := make([]migrationFile, 0, len(names))
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			return err
		}
		files = append(files, migrationFile{version: v, name: name})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].version != files[j].version {
			return files[i].version < files[j].version
		}
		return files[i].name < files[j].name
	})

	for i, f := range files {
		if i > 0 && f.version == files[i-1].version {
			return fmt.Errorf("迁移版本 %d 重复：%s 与 %s", f.version, files[i-1].name, f.name)
		}
		if f.version <= version {
			continue
		}
		b, err := migrationsFS.ReadFile(f.name)
		if err != nil {
			return err
		}
		if err := s.execScript(string(b), f.version); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

// migrationFile 是一个待执行的迁移：版本号从文件名前缀解析，不再用它在下标里的位置。
// 早先按下标当版本号，改个文件名或删掉一个文件就会让后面的版本整体错位，老库升级时
// 会被静默跳过——那些迁移的 SQL 一条都不会执行，却已经把 user_version 推到了「最新」。
type migrationFile struct {
	version int
	name    string
}

// migrationVersion 从 <序号>_<名字>.sql 里取出版本号。
func migrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(filepath.Base(name), ".sql")
	prefix, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("迁移文件名 %q 不是 <序号>_<名字>.sql", name)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("迁移文件名 %q 的序号 %q 不是正整数", name, prefix)
	}
	return v, nil
}

func (s *Store) execScript(script string, version int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

func encodeModels(models []string) string {
	if models == nil {
		models = []string{}
	}
	b, _ := json.Marshal(models)
	return string(b)
}

func decodeModels(s string) []string {
	var models []string
	if err := json.Unmarshal([]byte(s), &models); err != nil {
		return nil
	}
	return models
}

// accountSelect 是读账号的唯一列清单：账号自己的列，加上厂商行上的地址。
// Protocol 不在这里取：它是这家厂商路由算出来的有效默认协议，一条 JOIN 算不出来，
// 由 fillDerivedProtocols 填（规则只有 ResolveDefaultProtocol 一份）。
const accountSelect = `SELECT a.id, a.name, a.provider, a.key_cipher, a.models,
	a.price_in, a.price_out, a.price_cache_read, a.price_cache_write,
	a.priority, a.enabled, COALESCE(a.cooldown_until, 0), COALESCE(a.last_error, ''), a.fail_streak,
	a.created_at, COALESCE(p.base_url, '')
	FROM accounts a LEFT JOIN providers p ON p.name = a.provider`

// CreateAccount 插入账号，名称重复时返回错误。
// 只写账号自己的列：地址与默认协议在厂商行上（见迁移 0014 与 DESIGN 5.8），
// 传进来的 BaseURL 与 Protocol 不落库，读回来的是派生值。
func (s *Store) CreateAccount(a *Account) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.Priority == 0 {
		a.Priority = 100
	}
	res, err := s.db.Exec(`INSERT INTO accounts
		(name, provider, key_cipher, models, price_in, price_out,
		 price_cache_read, price_cache_write, priority, enabled, cooldown_until, last_error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?)`,
		a.Name, a.Provider, a.KeyCipher, encodeModels(a.Models),
		a.PriceIn, a.PriceOut, a.PriceCacheRead, a.PriceCacheWrite, a.Priority, boolInt(a.Enabled), a.CreatedAt)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// ListAccounts 按 priority、id 升序返回全部账号。
func (s *Store) ListAccounts() ([]Account, error) {
	return s.queryAccounts("")
}

// queryAccounts 跑一条账号查询并填上派生字段：账号读的三个入口（ListAccounts、GetAccount、
// ListAccountsByProvider）都走它，派生规则因此只有一份。
// where 是附加的筛选条件（不含 ORDER BY，带 WHERE 关键字），空串表示全表。
func (s *Store) queryAccounts(where string, args ...any) ([]Account, error) {
	q := accountSelect
	if where != "" {
		q += " " + where
	}
	rows, err := s.db.Query(q+" ORDER BY a.priority ASC, a.id ASC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanAccounts(rows)
	if err != nil {
		return nil, err
	}
	if err := s.fillDerivedProtocols(list); err != nil {
		return nil, err
	}
	return list, nil
}

// scanAccounts 把一行账号读成 Account，账号列表的几个查询共用它。
// Protocol 此时还是零值，由 fillDerivedProtocols 填。
func scanAccounts(rows *sql.Rows) ([]Account, error) {
	var out []Account
	for rows.Next() {
		var a Account
		var models string
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.Provider, &a.KeyCipher,
			&models, &a.PriceIn, &a.PriceOut, &a.PriceCacheRead, &a.PriceCacheWrite,
			&a.Priority, &enabled, &a.CooldownUntil, &a.LastError, &a.FailStreak, &a.CreatedAt, &a.BaseURL); err != nil {
			return nil, err
		}
		a.Models = decodeModels(models)
		a.Enabled = enabled == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// fillDerivedProtocols 填上派生的 Protocol：账号所属厂商的有效默认协议。
// 同一家厂商只算一次：账号再多，最多也只多查两回（厂商行与它的路由）。
func (s *Store) fillDerivedProtocols(list []Account) error {
	cache := make(map[string]string, len(list))
	for i := range list {
		name := list[i].Provider
		def, ok := cache[name]
		if !ok {
			var err error
			def, err = s.providerDefaultProtocol(name)
			if err != nil {
				return err
			}
			cache[name] = def
		}
		list[i].Protocol = def
	}
	return nil
}

// GetAccount 按名称取账号。不存在时返回 sql.ErrNoRows。
func (s *Store) GetAccount(name string) (Account, error) {
	list, err := s.queryAccounts("WHERE a.name = ?", name)
	if err != nil {
		return Account{}, err
	}
	if len(list) == 0 {
		return Account{}, sql.ErrNoRows
	}
	return list[0], nil
}

// DeleteAccount 按名称删除账号，连带删除它的模型行。
// SQLite 的 rowid 会被复用，不删子行会让新账号继承旧账号的探活状态。
func (s *Store) DeleteAccount(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRow(`SELECT id FROM accounts WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("账号 %q 不存在", name)
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM account_models WHERE account_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM accounts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// withTx 跑一个事务：状态写入只有这一个形状，中途失败一律回滚。
func (s *Store) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordFailure 记录一次失败：冷却截止时间、最近一次错误，并把连续失败次数加一。
func (s *Store) RecordFailure(id int64, until int64, lastErr string) error {
	return s.withTx(func(tx *sql.Tx) error { return recordFailureTx(tx, id, until, lastErr) })
}

// ClearFailure 清掉冷却、最近一次错误与连续失败次数。
func (s *Store) ClearFailure(id int64) error {
	return s.withTx(func(tx *sql.Tx) error { return clearFailureTx(tx, id) })
}

// SetEnabled 启用或停用账号。
func (s *Store) SetEnabled(id int64, enabled bool, lastErr string) error {
	return s.withTx(func(tx *sql.Tx) error { return setEnabledTx(tx, id, enabled, lastErr) })
}

// RotateKey 换掉一个账号的凭据，并在同一个事务里清掉旧凭据留下的结论：
// 清冷却、清最近错误与连续失败次数，并把账号重新启用（`account set --key`，见 DESIGN 4、5.6）。
// 换 key 意味着之前那些失败都是上一个凭据的结果，留在库里会让新 key 一上来就排在候选之外：
// 干净的凭据应该从干净的账号状态开始。冷却、清零与启停的 SQL 复用 clearFailureTx 与 setEnabledTx，
// 不另拼一份；`account_models`、`accounts.models` 与协议发现结果一个字不动，只换凭据与失败状态。
func (s *Store) RotateKey(id int64, cipher []byte) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE accounts SET key_cipher = ? WHERE id = ?`, cipher, id); err != nil {
			return err
		}
		if err := clearFailureTx(tx, id); err != nil {
			return err
		}
		return setEnabledTx(tx, id, true, "")
	})
}

// AccountPrices 是账号价格的增量改动：nil 表示这一项不动。
// `account set` 用它只改传入的价格字段，不顺手把没给的那些清掉。
type AccountPrices struct {
	In         *float64
	Out        *float64
	CacheRead  *float64
	CacheWrite *float64
}

// SetAccountPrices 只写这个结构里非 nil 的价格列，其余原样保留。
// 单独开一个写入口而不复用换凭据的 RotateKey：换 key 会顺手清冷却与失败计数，
// 改价格不能把账号状态一起抹掉（见 DESIGN 4 的 `account set`）。
func (s *Store) SetAccountPrices(id int64, p AccountPrices) error {
	// COALESCE 把「没给」（NULL 参数）变成「保留原值」：一条 UPDATE 就够，不拼动态 SQL。
	_, err := s.db.Exec(`UPDATE accounts
		SET price_in = COALESCE(?, price_in),
		    price_out = COALESCE(?, price_out),
		    price_cache_read = COALESCE(?, price_cache_read),
		    price_cache_write = COALESCE(?, price_cache_write)
		WHERE id = ?`, p.In, p.Out, p.CacheRead, p.CacheWrite, id)
	return err
}

// recordFailureTx 是冷却的写入口（加一），只给事务内部用。
func recordFailureTx(tx *sql.Tx, id int64, until int64, lastErr string) error {
	_, err := tx.Exec(`UPDATE accounts
		SET cooldown_until = ?, last_error = ?, fail_streak = fail_streak + 1
		WHERE id = ?`, until, lastErr, id)
	return err
}

// clearFailureTx 是清冷却的写入口，只给事务内部用。
func clearFailureTx(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`UPDATE accounts
		SET cooldown_until = NULL, last_error = '', fail_streak = 0
		WHERE id = ?`, id)
	return err
}

// setEnabledTx 是启停的写入口，只给事务内部用。
func setEnabledTx(tx *sql.Tx, id int64, enabled bool, lastErr string) error {
	_, err := tx.Exec(`UPDATE accounts SET enabled = ?, last_error = ? WHERE id = ?`, boolInt(enabled), lastErr, id)
	return err
}

// AttemptState 是一次上游尝试对账号状态的改动。三者互斥，零值表示什么都不改。
type AttemptState struct {
	// Failure 为真：写入 CooldownUntil 与 LastError，并把 fail_streak 加一。
	Failure bool
	// Clear 为真：清掉冷却、最近错误与 fail_streak。
	Clear bool
	// Disable 为真：停用账号并记下 LastError。
	Disable       bool
	CooldownUntil int64
	LastError     string
}

// RecordAttempt 在同一个事务里写一行请求记录并更新账号状态：要么一起提交，要么都不提交。
// 一次尝试的账（requests 一行）与它的结论（冷却、清零、停用）本来就是同一件事，
// 分开写在中途失败时会留下「有请求行但没设冷却」或者反过来，事后对不上账。
// 冷却、清零与停用的 SQL 只在 recordFailureTx、clearFailureTx、setEnabledTx 里，这里不另拼。
func (s *Store) RecordAttempt(rec *RequestRecord, state AttemptState) error {
	return s.withTx(func(tx *sql.Tx) error {
		if err := insertRequestTx(tx, rec); err != nil {
			return err
		}
		switch {
		case state.Clear:
			return clearFailureTx(tx, rec.AccountID)
		case state.Disable:
			return setEnabledTx(tx, rec.AccountID, false, state.LastError)
		case state.Failure:
			return recordFailureTx(tx, rec.AccountID, state.CooldownUntil, state.LastError)
		}
		return nil
	})
}

// InsertRequest 写一行请求记录。与账号状态一起写的场合用 RecordAttempt。
func (s *Store) InsertRequest(r *RequestRecord) error {
	return s.withTx(func(tx *sql.Tx) error { return insertRequestTx(tx, r) })
}

func insertRequestTx(tx *sql.Tx, r *RequestRecord) error {
	res, err := tx.Exec(`INSERT INTO requests
		(ts, inbound, outbound, model_requested, model_used, account_id, http_status, ok,
		 in_tokens, in_tokens_uncached, out_tokens, cache_read, cache_write,
		 reasoning_tokens, reasoning_tokens_extra, latency_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TS, r.Inbound, r.Outbound, r.ModelRequested, r.ModelUsed, r.AccountID, r.HTTPStatus, boolInt(r.OK),
		r.InTokens, r.InTokensUncached, r.OutTokens, r.CacheRead, r.CacheWrite,
		r.ReasoningTokens, r.ReasoningTokensExtra, r.LatencyMS, r.Error)
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

// RecentRequests 返回最近的请求记录，按时间倒序。
func (s *Store) RecentRequests(limit int) ([]RequestRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, ts, inbound, outbound, model_requested, model_used, COALESCE(account_id, 0),
		http_status, ok, in_tokens, in_tokens_uncached, out_tokens, cache_read, cache_write,
		reasoning_tokens, reasoning_tokens_extra, latency_ms, COALESCE(error, '')
		FROM requests ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var ok int
		if err := rows.Scan(&r.ID, &r.TS, &r.Inbound, &r.Outbound, &r.ModelRequested, &r.ModelUsed, &r.AccountID,
			&r.HTTPStatus, &ok, &r.InTokens, &r.InTokensUncached, &r.OutTokens, &r.CacheRead, &r.CacheWrite,
			&r.ReasoningTokens, &r.ReasoningTokensExtra, &r.LatencyMS, &r.Error); err != nil {
			return nil, err
		}
		r.OK = ok == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// UncachedInTokens 把上游报的输入 token 归一成「没命中缓存的那部分」，入账时只算这一列的花费。
// 三种出站协议对「输入 token 含不含缓存」的做法不同，不归一化时两个方向都错：
//
//   - anthropic 的 input_tokens 已经排除缓存读写，直接用；
//   - chat 的 prompt_tokens 与 gemini 的 promptTokenCount 都包含命中缓存的部分（
//     prompt_tokens_details.cached_tokens 与 cachedContentTokenCount），要减掉 cache_read。
//
// 减到 0 为止：上游若报出自相矛盾的数（缓存比输入还多），负的输入 token 会让花费变成负数，
// 库里的列与实际账目都比一个夹到 0 的估算更难解释。in_tokens 本身不改，展示与 log tail 不变。
// 以后加出站协议时要在这里确认它的输入计数含不含缓存：默认分支按「不含缓存」处理。
func UncachedInTokens(outbound string, inTokens, cacheRead int64) int64 {
	switch outbound {
	case translate.ProtocolChat, translate.ProtocolResponses, translate.ProtocolGemini:
		if rest := inTokens - cacheRead; rest > 0 {
			return rest
		}
		return 0
	default:
		return inTokens
	}
}

// ReasoningExtra 判定上游上报的思考 token 里有多少是「输出之外的一项」，入账时只给这一部分
// 按输出价再计一次（DESIGN 5.5）。各家对「思考算不算输出」的做法不同，只能按上报的数字判，
// 不按协议猜：
//
//   - total > in + out：思考是输出之外的一项。有的中转只把思考报在 reasoning_tokens 里、
//     没有并进 completion_tokens，total 却是三者相加；不单独计价会漏掉九成以上的输出花费
//     （实测一个 prompt=7、completion=1、total=132、reasoning=124 的中转，按 1 个输出 token 收费）。
//   - total <= in + out：OpenAI 官方把 reasoning 当作 completion_tokens 的子集，已经算在输出里，
//     再加一遍就是重复收费。
//   - total 为 0（上游没报）：按子集处理，宁可少算一次也不重复计价。
//
// reasoning 原值仍然整列进 reasoning_tokens 供展示，判定只决定 extra 这一列，所以 0 值不丢信息。
func ReasoningExtra(total, in, out, reasoning int64) int64 {
	if reasoning <= 0 || total <= 0 {
		return 0
	}
	if total > in+out {
		return reasoning
	}
	return 0
}

// UsageRow 是一行用量汇总。
// Cost 按 UncachedInTokens 归一化之后的输入、缓存读、缓存写、输出与独立的思考 token 折价
// （见 DESIGN 5.5）：缓存单价未填时回退到输入价，账号一个价都没填时是 0。
type UsageRow struct {
	Key       string
	Requests  int64
	InTokens  int64
	OutTokens int64
	// ReasoningTokens 是上游上报的思考 token 合计；只有其中被判定成输出之外一项的那部分
	// 才进 Cost，见 ReasoningExtra。
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite      int64
	Cost            float64
}

// Usage 按 dimension 汇总 since 之后的请求。dimension 取 account、model 或 day。
func (s *Store) Usage(dimension string, since int64) ([]UsageRow, error) {
	var expr string
	switch dimension {
	case "account":
		expr = "COALESCE(a.name, '(停用)')"
	case "model":
		expr = "r.model_used"
	case "day":
		expr = "date(r.ts, 'unixepoch', 'localtime')"
	default:
		return nil, fmt.Errorf("不支持的维度 %q，可用 account、model、day", dimension)
	}

	const join = "LEFT JOIN accounts a ON a.id = r.account_id"

	q := fmt.Sprintf(`SELECT %s AS k, COUNT(*), COALESCE(SUM(r.in_tokens),0), COALESCE(SUM(r.out_tokens),0),
		COALESCE(SUM(r.reasoning_tokens),0),
		COALESCE(SUM(r.cache_read),0), COALESCE(SUM(r.cache_write),0),
		COALESCE(SUM(
			r.in_tokens_uncached * COALESCE(a.price_in,0)
			+ r.cache_read * COALESCE(a.price_cache_read, a.price_in, 0)
			+ r.cache_write * COALESCE(a.price_cache_write, a.price_in, 0)
			+ r.out_tokens * COALESCE(a.price_out,0)
			+ r.reasoning_tokens_extra * COALESCE(a.price_out,0)
		) / 1e6, 0)
		FROM requests r %s WHERE r.ts >= ? GROUP BY k ORDER BY 2 DESC`, expr, join)

	rows, err := s.db.Query(q, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Key, &u.Requests, &u.InTokens, &u.OutTokens, &u.ReasoningTokens,
			&u.CacheRead, &u.CacheWrite, &u.Cost); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// 模型在某个账号上的状态。
const (
	// ModelSynced 表示刚从上游列表里拉下来，还没探活。
	ModelSynced = "synced"
	// ModelUsable 表示探活通过，账号能用这个模型。
	ModelUsable = "usable"
	// ModelForbidden 表示上游拒绝：套餐不含或无权使用。
	ModelForbidden = "forbidden"
	// ModelNotFound 表示上游不认识这个模型名。
	ModelNotFound = "notfound"
	// ModelAuthFailed 表示凭据在这个账号上不成立。
	ModelAuthFailed = "auth"
	// ModelError 表示其他临时错误，结论不可用。
	ModelError = "error"
)

// AccountModel 是一个账号上一个模型的探活状态。
// Protocol 是探活发现的「这个模型该用哪种协议」，空字符串表示继承厂商的有效默认协议（见 5.8）。
type AccountModel struct {
	AccountID  int64
	Model      string
	Status     string
	HTTPStatus int
	Error      string
	CheckedAt  int64
	Protocol   string
}

// ResolveProtocol 是有效出站协议的纯规则：模型行上有协议就用它，否则用厂商的有效默认协议
// （账号行不再存协议，见迁移 0014 与 DESIGN 5.8）。两部分都是空串时结果就是空串。
func ResolveProtocol(providerProtocol, modelProtocol string) string {
	if modelProtocol != "" {
		return modelProtocol
	}
	return providerProtocol
}

// ModelProtocol 读一个模型行的协议发现结果。没有这一行时返回空字符串（也就是继承），
// 不当作错误。
func (s *Store) ModelProtocol(accountID int64, model string) (string, error) {
	var protocol string
	err := s.db.QueryRow(`SELECT protocol FROM account_models WHERE account_id = ? AND model = ?`,
		accountID, model).Scan(&protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return protocol, nil
}

// EffectiveProtocol 返回账号在某个模型上的有效出站协议，转发与展示共用这一次读。
// 写法是 (accountID, model)：模型行的协议非空就用它，否则用账号所属厂商的有效默认协议
// （与 Account.Protocol 同一个规则）；行不存在、列为空、厂商行不存在或这家没有路由时都回退到
// 空串，不报错。账号行不再存协议，所以第二个参数也从旧写法里的「账号协议」换成了模型名。
//
// Deprecated: 三参数写法 EffectiveProtocol(accountID, accountProtocol, model) 只为尚未改完的调用点
// 保留（internal/gateway、internal/cli），第二个参数是账号协议，现在已经无用且被忽略；
// 两个写法走的是同一条规则，改完调用点后就收回这个口子。
func (s *Store) EffectiveProtocol(accountID int64, model string, legacyAccountProtocol ...string) (string, error) {
	if len(legacyAccountProtocol) > 1 {
		return "", fmt.Errorf("EffectiveProtocol 只收 (accountID, model) 两个参数：账号不再存协议，多余的参数是 %v", legacyAccountProtocol[1:])
	}
	if len(legacyAccountProtocol) == 1 {
		// 旧写法：第二个参数是账号协议（忽略），真正的模型名落在第三个参数上。
		model = legacyAccountProtocol[0]
	}
	protocol, err := s.ModelProtocol(accountID, model)
	if err != nil {
		return "", err
	}
	if protocol != "" {
		return protocol, nil
	}
	provider, err := s.accountProvider(accountID)
	if err != nil {
		return "", err
	}
	fallback, err := s.providerDefaultProtocol(provider)
	if err != nil {
		return "", err
	}
	return ResolveProtocol(fallback, protocol), nil
}

// accountProvider 读账号的厂商名。账号不存在时返回空串、不报错：有效协议的契纡是
// 「算不出来就是空串」，不因为一行不存在或读不到而中断转发。
func (s *Store) accountProvider(accountID int64) (string, error) {
	var provider string
	err := s.db.QueryRow(`SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return provider, nil
}

// SetModelProtocol 记录协议发现的结果，是这一列唯一的写入口。
// 模型行还不存在时先落一行 synced 占位（探活本身就证明上游有这个模型），
// 随后 SetModelStatus 再写状态；已存在的行只改协议列。
func (s *Store) SetModelProtocol(accountID int64, model, protocol string) error {
	_, err := s.db.Exec(`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
		VALUES (?, ?, 'synced', 0, '', 0, ?)
		ON CONFLICT(account_id, model) DO UPDATE SET protocol = excluded.protocol`,
		accountID, model, protocol)
	return err
}

// ReplaceModels 用上游列表刷新账号的模型集合。已有的探活状态保留，消失的行删除，
// 新出现的行记为 synced。返回新增的行数。
func (s *Store) ReplaceModels(accountID int64, models []string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if len(models) == 0 {
		if _, err := tx.Exec(`DELETE FROM account_models WHERE account_id = ?`, accountID); err != nil {
			return 0, err
		}
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(models)), ",")
		args := make([]any, 0, len(models)+1)
		args = append(args, accountID)
		for _, m := range models {
			args = append(args, m)
		}
		q := `DELETE FROM account_models WHERE account_id = ? AND model NOT IN (` + placeholders + `)`
		if _, err := tx.Exec(q, args...); err != nil {
			return 0, err
		}
	}

	added := 0
	for _, m := range models {
		// DO NOTHING：已存在的行一个字段都不动，探活状态、时间戳与发现到的协议原样保留，
		// 一次同步不会把发现结果推倒重来。只有新出现的行拿默认值。
		res, err := tx.Exec(`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at)
			VALUES (?, ?, 'synced', 0, '', 0)
			ON CONFLICT(account_id, model) DO NOTHING`, accountID, m)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// SetModelStatus 记录一个模型的探活结果。只动状态、HTTP 状态、备注与时间，
// 不动 protocol：协议发现的结果只由 SetModelProtocol 写。
func (s *Store) SetModelStatus(accountID int64, model, status string, httpStatus int, msg string) error {
	_, err := s.db.Exec(`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, model) DO UPDATE SET
			status = excluded.status, http_status = excluded.http_status,
			error = excluded.error, checked_at = excluded.checked_at`,
		accountID, model, status, httpStatus, msg, time.Now().Unix())
	return err
}

// AccountModels 返回一个账号的模型与状态，按模型名排序。
func (s *Store) AccountModels(accountID int64) ([]AccountModel, error) {
	rows, err := s.db.Query(`SELECT account_id, model, status, http_status, error, checked_at, protocol
		FROM account_models WHERE account_id = ? ORDER BY model`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccountModels(rows)
}

// AllAccountModels 返回全部账号的模型状态。
func (s *Store) AllAccountModels() ([]AccountModel, error) {
	rows, err := s.db.Query(`SELECT account_id, model, status, http_status, error, checked_at, protocol
		FROM account_models ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccountModels(rows)
}

func scanAccountModels(rows *sql.Rows) ([]AccountModel, error) {
	var out []AccountModel
	for rows.Next() {
		var m AccountModel
		if err := rows.Scan(&m.AccountID, &m.Model, &m.Status, &m.HTTPStatus, &m.Error, &m.CheckedAt, &m.Protocol); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetModels 改写账号自身声明的模型列表，选号用它。
func (s *Store) SetModels(accountID int64, models []string) error {
	_, err := s.db.Exec(`UPDATE accounts SET models = ? WHERE id = ?`, encodeModels(models), accountID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
