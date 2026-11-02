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
	"strings"
	"time"

	glebarez "github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
	// driver 是后端名，少数几处方言分支（目前只有 Usage 的按天分组）用它选表达式。
	driver string
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
	Protocol string
	// AuthHeader 与 AuthScheme 派生自 providers 的同名两列，规则与 BaseURL、Protocol 一样：
	// 账号行不存它们，改写厂商行立即对所有引用它的账号生效。两项都为空表示按线协议推导凭据头。
	AuthHeader string
	AuthScheme string
	KeyCipher  []byte
	// Models 是运行期派生的字段：读账号时它等于该账号 access='allowed' 的模型行集合
	// （迁移 0015 删掉了 accounts.models 列），按模型名升序。
	// 写账号时（CreateAccount）它表示「这个账号要声明哪些模型」：这些名字会被落成
	// provider_models 里 source='manual' 的行与该账号 access='allowed' 的可达性行。
	Models []string
	// 价格不在这里：单价属于「供应商 × 模型」，厂商级标价在 provider_models、套餐差异在
	// account_models 的覆盖列。账号行上的四个价格列由迁移 0016 删除。
	Priority int
	Enabled  bool
	// CooldownUntil 与 FailStreak 是旧的账号级冷却字段，只作只读汇总保留：
	// 选号按 ModelFailures 里的账号-模型状态判断，不再用这两个字段。
	CooldownUntil int64
	LastError     string
	// FailStreak 是各模型连续失败次数的最大值，只用于展示与旧调用方兼容。
	FailStreak int
	// ModelFailures 是账号-模型级冷却状态，键是模型名。没有冷却与失败记录的模型不在表里。
	ModelFailures map[string]ModelFailure
	CreatedAt     int64
}

// ModelFailure 是一个账号在一个模型上的冷却状态。
type ModelFailure struct {
	// CooldownUntilMS 是冷却截止时间，Unix 毫秒；0 表示没在冷却。
	CooldownUntilMS int64
	// FailStreak 是这个账号在这个模型上的连续失败次数。
	FailStreak int
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
	// TTSChars 是语音合成的入参字符数，AudioSeconds 是语音转写的音频秒数。
	// 其余协议的这两列恒为 0，与 token 四列互斥：语音端点不报 token。
	TTSChars     int64
	AudioSeconds int64
	LatencyMS    int64
	Error        string
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

	s := &Store{db: sqlDB, gorm: gdb, driver: driver}
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

// accountJoinRow 是账号与它的厂商地址的联合行，供 queryAccounts 扫描。
// 列名与 accountSelect 的别名一一对应。
type accountJoinRow struct {
	ID            int64
	Name          string
	Provider      string
	KeyCipher     []byte
	Priority      int
	Enabled       bool
	CooldownUntil int64
	LastError     string
	FailStreak    int
	CreatedAt     int64
	BaseURL       string
	AuthHeader    string
	AuthScheme    string
}

// CreateAccount 插入账号，名称重复时返回错误。
// 只写账号自己的列：地址与默认协议在厂商行上，传进来的 BaseURL 与 Protocol 不落库，读回来的是派生值。
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
	return s.withTx(func(tx *gorm.DB) error {
		row := accountRow{
			Name:      a.Name,
			Provider:  a.Provider,
			KeyCipher: a.KeyCipher,
			Priority:  a.Priority,
			Enabled:   a.Enabled,
			LastError: ptrStr(""),
			CreatedAt: a.CreatedAt,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		a.ID = row.ID
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
	q := s.gorm.Table("accounts AS a").
		Select(`a.id, a.name, a.provider, a.key_cipher, a.priority, a.enabled,
			COALESCE(a.cooldown_until, 0) AS cooldown_until, COALESCE(a.last_error, '') AS last_error,
			a.fail_streak, a.created_at, COALESCE(p.base_url, '') AS base_url,
			COALESCE(p.auth_header, '') AS auth_header, COALESCE(p.auth_scheme, '') AS auth_scheme`).
		Joins("LEFT JOIN providers p ON p.name = a.provider")
	if where != "" {
		q = q.Where(where, args...)
	}
	var rows []accountJoinRow
	if err := q.Order("a.priority ASC").Order("a.id ASC").Scan(&rows).Error; err != nil {
		return nil, err
	}
	list := scanAccounts(rows)
	if err := s.fillDerivedProtocols(list); err != nil {
		return nil, err
	}
	if err := s.fillDerivedModels(list); err != nil {
		return nil, err
	}
	return list, nil
}

// scanAccounts 把联合行转成 Account，账号列表的几个查询共用它。
// Protocol 与 Models 此时还是零值，由 fillDerivedProtocols 与 fillDerivedModels 填。
func scanAccounts(rows []accountJoinRow) []Account {
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		out = append(out, Account{
			ID:            r.ID,
			Name:          r.Name,
			Provider:      r.Provider,
			KeyCipher:     r.KeyCipher,
			Priority:      r.Priority,
			Enabled:       r.Enabled,
			CooldownUntil: r.CooldownUntil,
			LastError:     r.LastError,
			FailStreak:    r.FailStreak,
			CreatedAt:     r.CreatedAt,
			BaseURL:       r.BaseURL,
			AuthHeader:    r.AuthHeader,
			AuthScheme:    r.AuthScheme,
		})
	}
	return out
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
	list, err := s.queryAccounts("a.name = ?", name)
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
func (s *Store) withTx(fn func(*gorm.DB) error) error {
	return s.gorm.Transaction(fn)
}

// RecordFailure 记录一次账号-模型失败：冷却截止时间（Unix 毫秒），并把该模型的连续失败次数加一。
func (s *Store) RecordFailure(id int64, model string, untilMS int64, lastErr string) error {
	return s.withTx(func(tx *gorm.DB) error { return recordModelFailureTx(tx, id, model, untilMS, lastErr) })
}

// ClearFailure 清掉一个账号-模型的冷却与连续失败次数，不动同账号的其它模型。
func (s *Store) ClearFailure(id int64, model string) error {
	return s.withTx(func(tx *gorm.DB) error { return clearModelFailureTx(tx, id, model) })
}

// ClearFailures 清掉一个账号全部模型的冷却与连续失败次数，并清掉旧账号级字段。
func (s *Store) ClearFailures(id int64) error {
	return s.withTx(func(tx *gorm.DB) error { return clearAccountFailuresTx(tx, id) })
}

// SetEnabled 启用或停用账号。
func (s *Store) SetEnabled(id int64, enabled bool, lastErr string) error {
	return s.withTx(func(tx *gorm.DB) error { return setEnabledTx(tx, id, enabled, lastErr) })
}

// RotateKey 换掉一个账号的凭据，并在同一个事务里清掉旧凭据留下的结论：
// 清该账号全部模型的冷却与连续失败次数，并把账号重新启用（`account set --key`）。
// 换 key 意味着之前那些失败都是上一个凭据的结果，留在库里会让新 key 一上来就排在候选之外：
// 干净的凭据应该从干净的账号状态开始。冷却、清零与启停的 SQL 复用 clearAccountFailuresTx 与
// setEnabledTx，不另拼一份；`account_models` 的可达性、协议发现与覆盖值一个字不动，只换凭据与失败状态。
func (s *Store) RotateKey(id int64, cipher []byte) error {
	return s.withTx(func(tx *gorm.DB) error {
		if err := tx.Model(&accountRow{}).Where("id = ?", id).
			Update("key_cipher", cipher).Error; err != nil {
			return err
		}
		if err := clearAccountFailuresTx(tx, id); err != nil {
			return err
		}
		return setEnabledTx(tx, id, true, "")
	})
}

// recordModelFailureTx 是账号-模型冷却的写入口（加一），只给事务内部用。
// 行不存在时补一行 allowed 占位：只有选号选中的账号-模型才会走到这里，理论上行一定存在，
// 但手工构造的请求记录与旧数据也不该让冷却写丢。
func recordModelFailureTx(tx *gorm.DB, id int64, model string, untilMS int64, lastErr string) error {
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("记录冷却需要模型名：账号 %d 的哪一行都定位不了", id)
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}, {Name: "model"}},
		DoUpdates: clause.Assignments(map[string]any{
			"cooldown_until_ms": untilMS,
			"fail_streak":       gorm.Expr("fail_streak + 1"),
		}),
	}).Create(&accountModelRow{
		AccountID:       id,
		Model:           model,
		Access:          AccessAllowed,
		CooldownUntilMS: &untilMS,
		FailStreak:      1,
	}).Error; err != nil {
		return err
	}
	// error 列只属于探活结论；运行时失败的最近一条留在账号行上，供 account ls 展示。
	return tx.Model(&accountRow{}).Where("id = ?", id).Update("last_error", lastErr).Error
}

// clearModelFailureTx 是清一个账号-模型冷却的写入口，只给事务内部用。
func clearModelFailureTx(tx *gorm.DB, id int64, model string) error {
	return tx.Model(&accountModelRow{}).Where("account_id = ? AND model = ?", id, model).Updates(map[string]any{
		"cooldown_until_ms": nil,
		"fail_streak":       0,
	}).Error
}

// clearAccountFailuresTx 清一个账号全部模型的冷却，并顺带清掉旧账号级字段。
func clearAccountFailuresTx(tx *gorm.DB, id int64) error {
	if err := tx.Model(&accountModelRow{}).Where("account_id = ?", id).Updates(map[string]any{
		"cooldown_until_ms": nil,
		"fail_streak":       0,
	}).Error; err != nil {
		return err
	}
	return tx.Model(&accountRow{}).Where("id = ?", id).Updates(map[string]any{
		"cooldown_until": nil,
		"fail_streak":    0,
	}).Error
}

// setEnabledTx 是启停的写入口，只给事务内部用。
func setEnabledTx(tx *gorm.DB, id int64, enabled bool, lastErr string) error {
	return tx.Model(&accountRow{}).Where("id = ?", id).Updates(map[string]any{
		"enabled":    enabled,
		"last_error": lastErr,
	}).Error
}

// AttemptState 是一次上游尝试对账号状态的改动。三者互斥，零值表示什么都不改。
type AttemptState struct {
	// Failure 为真：给这次尝试的账号-模型写入 CooldownUntilMS，并把该模型的 fail_streak 加一。
	Failure bool
	// Clear 为真：清掉这次尝试的账号-模型的冷却与 fail_streak。
	Clear bool
	// Disable 为真：停用账号并记下 LastError。
	Disable bool
	// CooldownUntilMS 是冷却截止时间，Unix 毫秒。
	CooldownUntilMS int64
	LastError       string
}

// RecordAttempt 在同一个事务里写一行请求记录并更新账号状态：要么一起提交，要么都不提交。
// 一次尝试的账（requests 一行）与它的结论（冷却、清零、停用）本来就是同一件事，
// 分开写在中途失败时会留下「有请求行但没设冷却」或者反过来，事后对不上账。
// 冷却、清零与停用的 SQL 只在 recordModelFailureTx、clearModelFailureTx、setEnabledTx 里，这里不另拼。
func (s *Store) RecordAttempt(rec *RequestRecord, state AttemptState) error {
	return s.withTx(func(tx *gorm.DB) error {
		if err := insertRequestTx(tx, rec); err != nil {
			return err
		}
		switch {
		case state.Clear:
			return clearModelFailureTx(tx, rec.AccountID, rec.ModelUsed)
		case state.Disable:
			return setEnabledTx(tx, rec.AccountID, false, state.LastError)
		case state.Failure:
			return recordModelFailureTx(tx, rec.AccountID, rec.ModelUsed, state.CooldownUntilMS, state.LastError)
		}
		return nil
	})
}

// InsertRequest 写一行请求记录。与账号状态一起写的场合用 RecordAttempt。
func (s *Store) InsertRequest(r *RequestRecord) error {
	return s.withTx(func(tx *gorm.DB) error { return insertRequestTx(tx, r) })
}

func insertRequestTx(tx *gorm.DB, r *RequestRecord) error {
	row := requestRow{
		TS:                   r.TS,
		Inbound:              r.Inbound,
		Outbound:             r.Outbound,
		ModelRequested:       r.ModelRequested,
		ModelUsed:            r.ModelUsed,
		AccountID:            ptrInt64(r.AccountID),
		HTTPStatus:           r.HTTPStatus,
		OK:                   r.OK,
		InTokens:             r.InTokens,
		InTokensUncached:     r.InTokensUncached,
		OutTokens:            r.OutTokens,
		CacheRead:            r.CacheRead,
		CacheWrite:           r.CacheWrite,
		ReasoningTokens:      r.ReasoningTokens,
		ReasoningTokensExtra: r.ReasoningTokensExtra,
		TTSChars:             r.TTSChars,
		AudioSeconds:         r.AudioSeconds,
		LatencyMS:            r.LatencyMS,
		Error:                ptrStr(r.Error),
	}
	if err := tx.Create(&row).Error; err != nil {
		return err
	}
	r.ID = row.ID
	return nil
}

// RecentRequests 返回最近的请求记录，按时间倒序。
func (s *Store) RecentRequests(limit int) ([]RequestRecord, error) {
	return s.RecentRequestsBetween(limit, 0, 0)
}

// RecentRequestsBetween 返回 [since, until) 内最近的请求记录，按时间倒序。
// since 或 until 为 0 表示那一端不设限。
func (s *Store) RecentRequestsBetween(limit int, since, until int64) ([]RequestRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	query := s.gorm
	if since > 0 {
		query = query.Where("ts >= ?", since)
	}
	if until > 0 {
		query = query.Where("ts < ?", until)
	}
	var rows []requestRow
	if err := query.Order("ts DESC").Order("id DESC").Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return requestRecordsFromRows(rows), nil
}

// RequestsAfter 返回 [since, until) 内 id 大于 afterID 的请求记录，按 id 升序，最多 limit 条。
// since 或 until 为 0 表示那一端不设限。`log tail --follow` 用它按写入顺序追赶新行，
// 不依赖按时间倒序的 RecentRequests：一轮突发超过一次抓取上限时，倒序取最新 N 条会把
// 前面那些永久跳过。
func (s *Store) RequestsAfter(afterID int64, limit int, since, until int64) ([]RequestRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	query := s.gorm.Where("id > ?", afterID)
	if since > 0 {
		query = query.Where("ts >= ?", since)
	}
	if until > 0 {
		query = query.Where("ts < ?", until)
	}
	var rows []requestRow
	if err := query.Order("id ASC").Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return requestRecordsFromRows(rows), nil
}

// requestRecordsFromRows 把数据库行转成对外的请求记录，两个读取入口共用一份字段映射。
func requestRecordsFromRows(rows []requestRow) []RequestRecord {
	out := make([]RequestRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, RequestRecord{
			ID:                   r.ID,
			TS:                   r.TS,
			Inbound:              r.Inbound,
			Outbound:             r.Outbound,
			ModelRequested:       r.ModelRequested,
			ModelUsed:            r.ModelUsed,
			AccountID:            derefInt64(r.AccountID),
			HTTPStatus:           r.HTTPStatus,
			OK:                   r.OK,
			InTokens:             r.InTokens,
			InTokensUncached:     r.InTokensUncached,
			OutTokens:            r.OutTokens,
			CacheRead:            r.CacheRead,
			CacheWrite:           r.CacheWrite,
			ReasoningTokens:      r.ReasoningTokens,
			ReasoningTokensExtra: r.ReasoningTokensExtra,
			TTSChars:             r.TTSChars,
			AudioSeconds:         r.AudioSeconds,
			LatencyMS:            r.LatencyMS,
			Error:                derefStr(r.Error),
		})
	}
	return out
}

// ptrInt64 把零值写进可空列。写入侧沿用旧行为：没有账号写 0、没有错误写空串，
// 而不是写 NULL——读侧用 COALESCE 把两者看成同一个值，但库里的行保持一致更好查。
func ptrInt64(v int64) *int64 { return &v }

// ptrStr 同上，用于可空文本列。
func ptrStr(v string) *string { return &v }

// derefInt64 把可空列读成零值：调用方拿到的 RequestRecord 里没有「未设置」这个区分。
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// derefStr 同上，用于可空文本列。
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
	// TTSChars 与 AudioSeconds 是语音两条协议的合计，其余协议恒为 0。
	TTSChars     int64
	AudioSeconds int64
	// Cost 是这一组的估算花费（单位元）。nil 表示未定价：这一组里所有请求的所有计价项都
	// 因缺价没算，不知道多少钱。nil 与 0 是两件事：0 是「确实不花钱」（免费或打包价）。
	Cost *float64
}

// UsageFilter 是 usage 汇总的附加筛选。空串表示这一项不过滤。
// Account 按账号名匹配，Model 按解析别名后的 model_used 匹配，Provider 按账号所属厂商匹配。
type UsageFilter struct {
	Account  string
	Model    string
	Provider string
}

// Usage 按 dimension 汇总 [since, until) 内的请求；until 为 0 表示不设上界。
// dimension 取 account、model 或 day。
// 每行花费按它自己的账号与 model_used 现算单价，见 usagePriceIn 那一组常量。
//
// 花费分两层算：内层按行把每一项折成金额，单价缺的那一项整项是 NULL（不计，
// 而不是按 0 元算）；外层只把非 NULL 的项加起来，并数一遍有没有任何一项拿到价——
// 一项价都没拿到时整组是 NULL（未定价），拿到过就是那些项的和（部分缺项照算）。
func (s *Store) Usage(dimension string, since, until int64, filters ...UsageFilter) ([]UsageRow, error) {
	var filter UsageFilter
	if len(filters) > 1 {
		return nil, fmt.Errorf("用量汇总最多接受一个筛选条件组")
	}
	if len(filters) == 1 {
		filter = filters[0]
	}
	if until != 0 && until <= since {
		return nil, fmt.Errorf("用量区间不对：结束时间必须晚于起始时间")
	}
	var expr string
	order := "ORDER BY 2 DESC"
	switch dimension {
	case "account":
		expr = "COALESCE(a.name, '(停用)')"
	case "model":
		expr = "r.model_used"
	case "day":
		expr = s.dateExpr("r.ts")
		order = "ORDER BY k ASC"
	default:
		return nil, fmt.Errorf("不支持的维度 %q，可用 account、model、day", dimension)
	}

	conditions := []string{"r.ts >= ?", "(? = 0 OR r.ts < ?)"}
	args := []any{since, until, until}
	if filter.Account != "" {
		conditions = append(conditions, "a.name = ?")
		args = append(args, filter.Account)
	}
	if filter.Model != "" {
		conditions = append(conditions, "r.model_used = ?")
		args = append(args, filter.Model)
	}
	if filter.Provider != "" {
		conditions = append(conditions, "a.provider = ?")
		args = append(args, filter.Provider)
	}

	// 单价按行现算：账号覆盖 → 目录标价（见 usagePriceIn 与 usagePriceJoin）。
	// 不把单价快照进 requests：改价之后历史行按新价重算，与账号级价格时的行为一致。
	q := fmt.Sprintf(`SELECT k, COUNT(*), COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0),
		COALESCE(SUM(reasoning_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
		COALESCE(SUM(tts_chars),0), COALESCE(SUM(audio_seconds),0),
		CASE WHEN SUM(CASE WHEN c_in IS NOT NULL THEN 1 ELSE 0 END
		              + CASE WHEN c_cache_read IS NOT NULL THEN 1 ELSE 0 END
		              + CASE WHEN c_cache_write IS NOT NULL THEN 1 ELSE 0 END
		              + CASE WHEN c_out IS NOT NULL THEN 1 ELSE 0 END
		              + CASE WHEN c_reasoning IS NOT NULL THEN 1 ELSE 0 END
		              + CASE WHEN c_tts IS NOT NULL THEN 1 ELSE 0 END
		              + CASE WHEN c_audio IS NOT NULL THEN 1 ELSE 0 END) = 0
		     THEN NULL
		     ELSE SUM(COALESCE(c_in,0) + COALESCE(c_cache_read,0) + COALESCE(c_cache_write,0)
		              + COALESCE(c_out,0) + COALESCE(c_reasoning,0)
		              + COALESCE(c_tts,0) + COALESCE(c_audio,0))
		END
		FROM (
			SELECT %s AS k,
				r.in_tokens AS in_tokens, r.out_tokens AS out_tokens,
				r.reasoning_tokens AS reasoning_tokens,
				r.cache_read AS cache_read, r.cache_write AS cache_write,
				r.tts_chars AS tts_chars, r.audio_seconds AS audio_seconds,
				r.in_tokens_uncached * (%s) / 1e6 AS c_in,
				r.cache_read * (%s) / 1e6 AS c_cache_read,
				r.cache_write * (%s) / 1e6 AS c_cache_write,
				r.out_tokens * (%s) / 1e6 AS c_out,
				r.reasoning_tokens_extra * (%s) / 1e6 AS c_reasoning,
				r.tts_chars * (%s) / 1e6 AS c_tts,
				r.audio_seconds * (%s) AS c_audio
			FROM requests r %s WHERE %s
		) AS t GROUP BY k %s`,
		expr, usagePriceIn, usagePriceCacheRead, usagePriceCacheWrite, usagePriceOut, usagePriceOut,
		usagePriceTTSChar, usagePriceAudioSecond, usagePriceJoin, strings.Join(conditions, " AND "), order)

	// 走 GORM 的 Raw 而不是底层 *sql.DB：查询里的 ? 占位符要由 dialector 转成 PostgreSQL
	// 的 $1，直接用 *sql.DB 会把这个问号原样发给 PG。
	rows, err := s.gorm.Raw(q, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		var cost sql.NullFloat64
		if err := rows.Scan(&u.Key, &u.Requests, &u.InTokens, &u.OutTokens, &u.ReasoningTokens,
			&u.CacheRead, &u.CacheWrite, &u.TTSChars, &u.AudioSeconds, &cost); err != nil {
			return nil, err
		}
		if cost.Valid {
			u.Cost = &cost.Float64
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// dateExpr 把一个 Unix 秒列分成本地日期。
//
// 三个后端没有共同的日期函数，这一处只能按后端分支。PostgreSQL 的 to_timestamp 需要显式时区，
// 用会话的 TimeZone 才与 SQLite 的 'localtime' 同义；MySQL 的 FROM_UNIXTIME 按会话时区解释。
// 只有 `usage --by day` 会走到它，其余维度与后端无关。
func (s *Store) dateExpr(col string) string {
	switch s.driver {
	case DriverPostgres:
		return "to_char(to_timestamp(" + col + ") AT TIME ZONE current_setting('TimeZone'), 'YYYY-MM-DD')"
	case DriverMySQL:
		return "DATE(FROM_UNIXTIME(" + col + "))"
	default:
		return "date(" + col + ", 'unixepoch', 'localtime')"
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
