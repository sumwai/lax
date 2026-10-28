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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 是数据库句柄。写入串行化，连接数固定为 1。
type Store struct {
	db *sql.DB
}

// Account 是一行账号。
type Account struct {
	ID            int64
	Name          string
	Provider      string
	Protocol      string
	BaseURL       string
	KeyCipher     []byte
	Models        []string
	PriceIn       *float64
	PriceOut      *float64
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
	OutTokens      int64
	CacheRead      int64
	CacheWrite     int64
	LatencyMS      int64
	Error          string
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
	sort.Strings(names)

	for i, name := range names {
		v := i + 1
		if v <= version {
			continue
		}
		b, err := migrationsFS.ReadFile(name)
		if err != nil {
			return err
		}
		if err := s.execScript(string(b), v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
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

// CreateAccount 插入账号，名称重复时返回错误。
func (s *Store) CreateAccount(a *Account) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.Priority == 0 {
		a.Priority = 100
	}
	res, err := s.db.Exec(`INSERT INTO accounts
		(name, provider, protocol, base_url, key_cipher, models, price_in, price_out, priority, enabled, cooldown_until, last_error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?)`,
		a.Name, a.Provider, a.Protocol, a.BaseURL, a.KeyCipher, encodeModels(a.Models),
		a.PriceIn, a.PriceOut, a.Priority, boolInt(a.Enabled), a.CreatedAt)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// ListAccounts 按 priority、id 升序返回全部账号。
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT id, name, provider, protocol, base_url, key_cipher, models,
		price_in, price_out, priority, enabled, COALESCE(cooldown_until, 0), COALESCE(last_error, ''), fail_streak, created_at
		FROM accounts ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}

// scanAccounts 把一行账号读成 Account，账号列表的几个查询共用它。
func scanAccounts(rows *sql.Rows) ([]Account, error) {
	var out []Account
	for rows.Next() {
		var a Account
		var models string
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.Provider, &a.Protocol, &a.BaseURL, &a.KeyCipher,
			&models, &a.PriceIn, &a.PriceOut, &a.Priority, &enabled, &a.CooldownUntil, &a.LastError, &a.FailStreak, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Models = decodeModels(models)
		a.Enabled = enabled == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 按名称取账号。
func (s *Store) GetAccount(name string) (Account, error) {
	var a Account
	var models string
	var enabled int
	err := s.db.QueryRow(`SELECT id, name, provider, protocol, base_url, key_cipher, models,
		price_in, price_out, priority, enabled, COALESCE(cooldown_until, 0), COALESCE(last_error, ''), fail_streak, created_at
		FROM accounts WHERE name = ?`, name).Scan(&a.ID, &a.Name, &a.Provider, &a.Protocol, &a.BaseURL,
		&a.KeyCipher, &models, &a.PriceIn, &a.PriceOut, &a.Priority, &enabled, &a.CooldownUntil, &a.LastError, &a.FailStreak, &a.CreatedAt)
	if err != nil {
		return a, err
	}
	a.Models = decodeModels(models)
	a.Enabled = enabled == 1
	return a, nil
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

// RecordFailure 记录一次失败：冷却截止时间、最近一次错误，并把连续失败次数加一。
func (s *Store) RecordFailure(id int64, until int64, lastErr string) error {
	_, err := s.db.Exec(`UPDATE accounts
		SET cooldown_until = ?, last_error = ?, fail_streak = fail_streak + 1
		WHERE id = ?`, until, lastErr, id)
	return err
}

// ClearFailure 清掉冷却、最近一次错误与连续失败次数。
func (s *Store) ClearFailure(id int64) error {
	_, err := s.db.Exec(`UPDATE accounts
		SET cooldown_until = NULL, last_error = '', fail_streak = 0
		WHERE id = ?`, id)
	return err
}

// SetEnabled 启用或停用账号。
func (s *Store) SetEnabled(id int64, enabled bool, lastErr string) error {
	_, err := s.db.Exec(`UPDATE accounts SET enabled = ?, last_error = ? WHERE id = ?`, boolInt(enabled), lastErr, id)
	return err
}

// InsertRequest 写一行请求记录。
func (s *Store) InsertRequest(r *RequestRecord) error {
	res, err := s.db.Exec(`INSERT INTO requests
		(ts, inbound, outbound, model_requested, model_used, account_id, http_status, ok,
		 in_tokens, out_tokens, cache_read, cache_write, latency_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TS, r.Inbound, r.Outbound, r.ModelRequested, r.ModelUsed, r.AccountID, r.HTTPStatus, boolInt(r.OK),
		r.InTokens, r.OutTokens, r.CacheRead, r.CacheWrite, r.LatencyMS, r.Error)
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
		http_status, ok, in_tokens, out_tokens, cache_read, cache_write, latency_ms, COALESCE(error, '')
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
			&r.HTTPStatus, &ok, &r.InTokens, &r.OutTokens, &r.CacheRead, &r.CacheWrite, &r.LatencyMS, &r.Error); err != nil {
			return nil, err
		}
		r.OK = ok == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageRow 是一行用量汇总。
type UsageRow struct {
	Key       string
	Requests  int64
	InTokens  int64
	OutTokens int64
	Cost      float64
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
		COALESCE(SUM(r.in_tokens * COALESCE(a.price_in,0) / 1e6 + r.out_tokens * COALESCE(a.price_out,0) / 1e6),0)
		FROM requests r %s WHERE r.ts >= ? GROUP BY k ORDER BY 2 DESC`, expr, join)

	rows, err := s.db.Query(q, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Key, &u.Requests, &u.InTokens, &u.OutTokens, &u.Cost); err != nil {
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
// Protocol 是探活发现的「这个模型该用哪种协议」，空字符串表示继承账号的 protocol。
type AccountModel struct {
	AccountID  int64
	Model      string
	Status     string
	HTTPStatus int
	Error      string
	CheckedAt  int64
	Protocol   string
}

// ResolveProtocol 是有效出站协议的纯规则：模型行上有协议就用它，否则继承账号的 protocol。
func ResolveProtocol(accountProtocol, modelProtocol string) string {
	if modelProtocol != "" {
		return modelProtocol
	}
	return accountProtocol
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
// 模型行不存在或协议列为空时回退账号的 protocol，不报错。
func (s *Store) EffectiveProtocol(accountID int64, accountProtocol, model string) (string, error) {
	protocol, err := s.ModelProtocol(accountID, model)
	if err != nil {
		return "", err
	}
	return ResolveProtocol(accountProtocol, protocol), nil
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
