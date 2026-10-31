// Package store 负责数据库连接、建表与加列、账号与请求记录的读写。
//
// 支持三种后端：SQLite（缺省，单文件）、PostgreSQL 与 MySQL。后端差异由 GORM 的 dialector
// 负责：占位符、UPSERT 语法、自增主键与列类型都不在调用方出现。表结构的唯一来源是
// model.go 里的 row 定义，建表与加列走 AutoMigrate，没有迁移文件与版本号。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	glebarez "github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"lax/internal/translate"
)

// 后端名，与配置文件里的 driver 取值一致。
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
	DriverMySQL    = "mysql"
)

// Store 是数据库句柄。
//
// SQLite 后端只有一个连接：单文件只有一个写者，多连接会撞 database is locked。
// PostgreSQL 与 MySQL 不设上限，用驱动默认的连接池。
type Store struct {
	db   *sql.DB
	gorm *gorm.DB
}

// Account 是一行账号。
//
// Provider 是唯一的地址来源：转发用的地址、默认协议与路由都按它在 providers / provider_routes
// 里查。BaseURL 与 Protocol 是只读派生值，账号表里没有这两列（迁移 0014 删掉了）：
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
	// Models 是运行期派生的字段：读账号时它等于该账号 access='allowed' 的模型行集合
	// （迁移 0015 删掉了 accounts.models 列），按模型名升序。
	// 写账号时（CreateAccount）它表示「这个账号要声明哪些模型」：这些名字会被落成
	// provider_models 里 source='manual' 的行与该账号 access='allowed' 的可达性行。
	Models []string
	// 价格不在这里：单价属于「供应商 × 模型」，厂商级标价在 provider_models、套餐差异在
	// account_models 的覆盖列。账号行上的四个价格列由迁移 0016 删除。
	Priority      int
	Enabled       bool
	CooldownUntil int64
	LastError     string
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
	// 入账时由 UncachedInTokens 按出站协议算出：这一列是花费里输入那一项的乘数，
	// 留默认值 0 会让输入的花费整块消失。
	InTokensUncached int64
	OutTokens        int64
	CacheRead        int64
	CacheWrite       int64
	// ReasoningTokens 是上游上报的思考 token 原值（anthropic 没有独立字段，恒为 0），
	// ReasoningTokensExtra 是其中「输出之外的一项」那部分：只给 extra 按输出价再计一次，
	// 判定由 ReasoningExtra 给。两列都进 requests 的独立列，方便事后按列重算花费。
	ReasoningTokens      int64
	ReasoningTokensExtra int64
	LatencyMS            int64
	Error                string
}

// Options 是打开数据库的选项。
//
// Path 只给 SQLite 用：DSN 为空时由它拼出 file:… 的连接串。外部后端的 DSN 必须显式给。
type Options struct {
	Driver string
	DSN    string
	Path   string
}

// Open 按 SQLite 单文件打开数据库，按需建目录并建表。
func Open(path string) (*Store, error) {
	return OpenWith(Options{Path: path})
}

// OpenWith 按指定后端打开数据库，按需搬迁旧库、建表并写种子。
func OpenWith(o Options) (*Store, error) {
	driver := o.Driver
	if driver == "" {
		driver = DriverSQLite
	}
	if driver == DriverSQLite && o.DSN == "" {
		if err := upgradeIfLegacy(o.Path); err != nil {
			return nil, err
		}
	}
	dialector, err := dialectorFor(driver, o)
	if err != nil {
		return nil, err
	}
	gdb, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("连接 %s 数据库: %w", driver, err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("取 %s 连接池: %w", driver, err)
	}
	if driver == DriverSQLite {
		sqlDB.SetMaxOpenConns(1)
	}

	s := &Store{db: sqlDB, gorm: gdb}
	if err := migrate(gdb); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return s, nil
}

// dialectorFor 按后端名给出 GORM 的 dialector。
func dialectorFor(driver string, o Options) (gorm.Dialector, error) {
	switch driver {
	case DriverSQLite:
		if o.DSN != "" {
			return glebarez.Open(o.DSN), nil
		}
		if o.Path == "" {
			return nil, errors.New("打开 SQLite 需要文件路径")
		}
		if dir := filepath.Dir(o.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		return glebarez.Open(sqliteDSN(o.Path)), nil
	case DriverPostgres:
		if o.DSN == "" {
			return nil, errors.New("postgres 后端需要在配置里写 dsn：连接串为空")
		}
		return postgres.Open(o.DSN), nil
	case DriverMySQL:
		if o.DSN == "" {
			return nil, errors.New("mysql 后端需要在配置里写 dsn：连接串为空")
		}
		return mysql.Open(o.DSN), nil
	}
	return nil, fmt.Errorf("不支持的数据库后端 %q，可用 %s、%s、%s", driver, DriverSQLite, DriverPostgres, DriverMySQL)
}

// sqliteDSN 拼 SQLite 的连接串。
//
// busy_timeout 让并发写入等锁而不是立刻报 database is locked；WAL 让读不挡写；
// foreign_keys 打开外键约束（provider_headers 的 ON DELETE CASCADE 靠它）。
func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

// upgradeIfLegacy 在库里还留着旧版版本号（PRAGMA user_version > 0）时先把它搬成新形状。
// 搬迁失败不碰原库，错误直接抛给调用方。
func upgradeIfLegacy(path string) error {
	version, err := legacyVersion(path)
	if err != nil {
		return err
	}
	if version == 0 {
		return nil
	}
	if _, err := upgradeLegacy(path); err != nil {
		return fmt.Errorf("搬迁旧版数据库: %w", err)
	}
	return nil
}

// legacyVersion 读出旧版迁移器留下的版本号。文件不存在或读不出来都当 0 处理。
func legacyVersion(path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, nil
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return 0, fmt.Errorf("探测旧库版本: %w", err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, nil
	}
	return version, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接，供测试与统计使用。
func (s *Store) DB() *sql.DB { return s.db }

// accountSelect 是读账号的唯一列清单：账号自己的列，加上厂商行上的地址。
// Protocol 不在这里取：它是这家厂商路由算出来的有效默认协议，一条 JOIN 算不出来，
// 由 fillDerivedProtocols 填（规则只有 ResolveDefaultProtocol 一份）。
const accountSelect = `SELECT a.id, a.name, a.provider, a.key_cipher,
	a.priority, a.enabled, COALESCE(a.cooldown_until, 0), COALESCE(a.last_error, ''), a.fail_streak,
	a.created_at, COALESCE(p.base_url, '')
	FROM accounts a LEFT JOIN providers p ON p.name = a.provider`

// CreateAccount 插入账号，名称重复时返回错误。
// 只写账号自己的列：地址与默认协议在厂商行上（迁移 0014 把这两列从账号表里删了），
// 传进来的 BaseURL 与 Protocol 不落库，读回来的是派生值。
// a.Models 是「这个账号要声明哪些模型」（`account add --models`）：名字去空白去重之后落成
// provider_models 的 manual 行与该账号 access='allowed' 的可达性行，与账号本身同一个事务：
// 拆开写会出现「账号建好了但没有模型」或「模型可达但账号不存在」的半截状态。
// 没有厂商名的账号不写目录：目录以厂商为键，空厂商名只会造出一批谁都用不上的行。
func (s *Store) CreateAccount(a *Account) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.Priority == 0 {
		a.Priority = 100
	}
	return s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO accounts
			(name, provider, key_cipher, priority, enabled, cooldown_until, last_error, created_at)
			VALUES (?, ?, ?, ?, ?, NULL, '', ?)`,
			a.Name, a.Provider, a.KeyCipher, a.Priority, boolInt(a.Enabled), a.CreatedAt)
		if err != nil {
			return err
		}
		if a.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		return declareModelsTx(tx, a.ID, a.Provider, a.Models)
	})
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
	if err := s.fillDerivedModels(list); err != nil {
		return nil, err
	}
	return list, nil
}

// scanAccounts 把一行账号读成 Account，账号列表的几个查询共用它。
// Protocol 与 Models 此时还是零值，由 fillDerivedProtocols 与 fillDerivedModels 填。
func scanAccounts(rows *sql.Rows) ([]Account, error) {
	var out []Account
	for rows.Next() {
		var a Account
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.Provider, &a.KeyCipher,
			&a.Priority, &enabled, &a.CooldownUntil, &a.LastError, &a.FailStreak, &a.CreatedAt, &a.BaseURL); err != nil {
			return nil, err
		}
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
// 清冷却、清最近错误与连续失败次数，并把账号重新启用（`account set --key`）。
// 换 key 意味着之前那些失败都是上一个凭据的结果，留在库里会让新 key 一上来就排在候选之外：
// 干净的凭据应该从干净的账号状态开始。冷却、清零与启停的 SQL 复用 clearFailureTx 与 setEnabledTx，
// 不另拼一份；`account_models` 的可达性、协议发现与覆盖值一个字不动，只换凭据与失败状态。
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
// 各出站协议对「输入 token 含不含缓存」的做法不同，不归一化时两个方向都错：
//
//   - anthropic 的 input_tokens 已经排除缓存读写，直接用；
//   - chat 的 prompt_tokens 与 gemini 的 promptTokenCount 都包含命中缓存的部分（
//     prompt_tokens_details.cached_tokens 与 cachedContentTokenCount），要减掉 cache_read；
//   - systemone（决策协议）只报 input_tokens 与 output_tokens，没有缓存字段。
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
	case translate.ProtocolSystemOne:
		// systemone 的 input_tokens 不含缓存：决策端点没有缓存读写这一说，
		// 减 cache_read 只会把花费算少。
		return inTokens
	default:
		return inTokens
	}
}

// ReasoningExtra 判定上游上报的思考 token 里有多少是「输出之外的一项」，入账时只给这一部分
// 按输出价再计一次。各家对「思考算不算输出」的做法不同，只能按上报的数字判，
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
// （单价按「这一行的账号 + 这一行的 model_used」现算，取值链见
// usagePriceIn 那一组常量（账号覆盖 → 目录标价）；缓存单价未填时回退到输入价。
// 每条请求按自己当时的单价折，不按一个全局价目表：同一模型在不同账号上可以有不同的套餐价。
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
	// Cost 是这一组的估算花费（单位元）。nil 表示未定价：这一组里所有请求的所有计价项都
	// 因缺价没算，不知道多少钱。nil 与 0 是两件事：0 是「确实不花钱」（免费或打包价）。
	Cost *float64
}

// Usage 按 dimension 汇总 since 之后的请求。dimension 取 account、model 或 day。
// 每行花费按它自己的账号与 model_used 现算单价，见 usagePriceIn 那一组常量。
//
// 花费分两层算：内层按行把每一项折成金额，单价缺的那一项整项是 NULL（不计，
// 而不是按 0 元算）；外层只把非 NULL 的项加起来，并数一遍有没有任何一项拿到价——
// 一项价都没拿到时整组是 NULL（未定价），拿到过就是那些项的和（部分缺项照算）。
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

	// 单价按行现算：账号覆盖 → 目录标价（见 usagePriceIn 与 usagePriceJoin）。
	// 不把单价快照进 requests：改价之后历史行按新价重算，与账号级价格时的行为一致。
	q := fmt.Sprintf(`SELECT k, COUNT(*), COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0),
		COALESCE(SUM(reasoning_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
		CASE WHEN SUM((c_in IS NOT NULL) + (c_cache_read IS NOT NULL) + (c_cache_write IS NOT NULL)
		              + (c_out IS NOT NULL) + (c_reasoning IS NOT NULL)) = 0
		     THEN NULL
		     ELSE SUM(COALESCE(c_in,0) + COALESCE(c_cache_read,0) + COALESCE(c_cache_write,0)
		              + COALESCE(c_out,0) + COALESCE(c_reasoning,0)) / 1e6
		END
		FROM (
			SELECT %s AS k,
				r.in_tokens AS in_tokens, r.out_tokens AS out_tokens,
				r.reasoning_tokens AS reasoning_tokens,
				r.cache_read AS cache_read, r.cache_write AS cache_write,
				r.in_tokens_uncached * (%s) AS c_in,
				r.cache_read * (%s) AS c_cache_read,
				r.cache_write * (%s) AS c_cache_write,
				r.out_tokens * (%s) AS c_out,
				r.reasoning_tokens_extra * (%s) AS c_reasoning
			FROM requests r %s WHERE r.ts >= ?
		) GROUP BY k ORDER BY 2 DESC`,
		expr, usagePriceIn, usagePriceCacheRead, usagePriceCacheWrite, usagePriceOut, usagePriceOut, usagePriceJoin)

	rows, err := s.db.Query(q, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		var cost sql.NullFloat64
		if err := rows.Scan(&u.Key, &u.Requests, &u.InTokens, &u.OutTokens, &u.ReasoningTokens,
			&u.CacheRead, &u.CacheWrite, &cost); err != nil {
			return nil, err
		}
		if cost.Valid {
			u.Cost = &cost.Float64
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
