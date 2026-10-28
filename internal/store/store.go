// Package store 负责 SQLite 连接、迁移与账号、请求记录、客户端状态的读写。
package store

import (
	"database/sql"
	"embed"
	"encoding/json"
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
	CreatedAt     int64
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

// AgentState 是某个客户端配置被改写的状态。
type AgentState struct {
	Name       string
	ConfigPath string
	BackupPath string
	Model      string
	AppliedAt  int64
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
		price_in, price_out, priority, enabled, COALESCE(cooldown_until, 0), COALESCE(last_error, ''), created_at
		FROM accounts ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		var models string
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.Provider, &a.Protocol, &a.BaseURL, &a.KeyCipher,
			&models, &a.PriceIn, &a.PriceOut, &a.Priority, &enabled, &a.CooldownUntil, &a.LastError, &a.CreatedAt); err != nil {
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
		price_in, price_out, priority, enabled, COALESCE(cooldown_until, 0), COALESCE(last_error, ''), created_at
		FROM accounts WHERE name = ?`, name).Scan(&a.ID, &a.Name, &a.Provider, &a.Protocol, &a.BaseURL,
		&a.KeyCipher, &models, &a.PriceIn, &a.PriceOut, &a.Priority, &enabled, &a.CooldownUntil, &a.LastError, &a.CreatedAt)
	if err != nil {
		return a, err
	}
	a.Models = decodeModels(models)
	a.Enabled = enabled == 1
	return a, nil
}

// DeleteAccount 按名称删除账号。
func (s *Store) DeleteAccount(name string) error {
	res, err := s.db.Exec(`DELETE FROM accounts WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("账号 %q 不存在", name)
	}
	return nil
}

// SetCooldown 记录冷却截止时间与最近一次错误。
func (s *Store) SetCooldown(id int64, until int64, lastErr string) error {
	_, err := s.db.Exec(`UPDATE accounts SET cooldown_until = ?, last_error = ? WHERE id = ?`, until, lastErr, id)
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

// PutAgent 记录客户端配置的改写状态。
func (s *Store) PutAgent(st AgentState) error {
	_, err := s.db.Exec(`INSERT INTO agents (name, config_path, backup_path, model, applied_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET config_path = excluded.config_path,
			backup_path = excluded.backup_path, model = excluded.model, applied_at = excluded.applied_at`,
		st.Name, st.ConfigPath, st.BackupPath, st.Model, st.AppliedAt)
	return err
}

// GetAgent 取客户端状态。
func (s *Store) GetAgent(name string) (AgentState, error) {
	var st AgentState
	err := s.db.QueryRow(`SELECT name, config_path, COALESCE(backup_path, ''), COALESCE(model, ''), COALESCE(applied_at, 0)
		FROM agents WHERE name = ?`, name).Scan(&st.Name, &st.ConfigPath, &st.BackupPath, &st.Model, &st.AppliedAt)
	return st, err
}

// ListAgents 列出全部客户端状态。
func (s *Store) ListAgents() ([]AgentState, error) {
	rows, err := s.db.Query(`SELECT name, config_path, COALESCE(backup_path, ''), COALESCE(model, ''), COALESCE(applied_at, 0)
		FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentState
	for rows.Next() {
		var st AgentState
		if err := rows.Scan(&st.Name, &st.ConfigPath, &st.BackupPath, &st.Model, &st.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// DeleteAgent 删除客户端状态。
func (s *Store) DeleteAgent(name string) error {
	_, err := s.db.Exec(`DELETE FROM agents WHERE name = ?`, name)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
