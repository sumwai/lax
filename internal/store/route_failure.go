package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
)

// ErrRouteFailurePolicyNotFound 表示库里没有这条失败处理策略。
// CLI 的 show/set/unset/rm/enable/disable 用 errors.Is 判它，把「id 写错了」
// 与「数据库打不开」分开。
var ErrRouteFailurePolicyNotFound = errors.New("失败处理策略不存在")

const (
	routeFailureDefaultRetryBaseMS int64 = 500
	routeFailureMinRetryBaseMS     int64 = 100
	routeFailureMaxRetryBaseMS     int64 = 10000
	routeFailureMaxRetryCount            = 10
	routeFailureMaxCooldownSeconds int64 = 30 * 24 * 3600
	routeFailureMaxRegexRunes            = 512
	routeFailureMaxContains              = 20
	routeFailureMaxContainsRunes         = 200
)

// RouteFailurePolicy 是一行上游失败处理策略。
//
// 条件都可选，零值表示不限：AccountID 为 0、Provider/Model 为空串、Statuses 为空
// 表示任意状态、ErrorContains 为空表示任意错误文本。匹配时不同条件之间是 AND，
// 同一个条件里的多个值是 OR（见 account.MatchFailure）。
//
// 动作用整条替换语义：命中的规则定义全部行为，没写的动作不做。NextRoute 与
// ReturnError 互斥；两者都没写时按默认流程处理，即 ReturnError。写入口
// （CreateRouteFailurePolicy / UpdateRouteFailurePolicy）会先调用
// NormalizeRouteFailurePolicy 把缺省流程归一成 ReturnError，CLI 也应当用同一个
// 函数，保证打印出来的规则与落库的规则一致。
type RouteFailurePolicy struct {
	ID              int64
	Enabled         bool
	Priority        int
	AccountID       int64 // 0 = 不限账号
	Provider        string
	Model           string
	Statuses        []string
	ErrorContains   []string
	ErrorRegex      string
	RetryCount      int
	RetryBaseMS     int64
	CooldownSeconds int64
	Disable         bool
	NextRoute       bool
	ReturnError     bool
	Note            string
	CreatedAt       int64
	UpdatedAt       int64
}

// NormalizeRouteFailurePolicy 把可省的零值补成约定值：
//   - RetryBaseMS 为 0 时补 500ms；
//   - NextRoute 与 ReturnError 都没写时补 ReturnError（默认返回，不换号）。
//
// 它不碰互斥冲突与范围错误，那些交给 ValidateRouteFailurePolicy 报出来，
// 免得把用户写错的值悄悄改成另一种行为。CLI 与 store 写入口共用这一个函数。
func NormalizeRouteFailurePolicy(p *RouteFailurePolicy) {
	if p == nil {
		return
	}
	if p.RetryBaseMS == 0 {
		p.RetryBaseMS = routeFailureDefaultRetryBaseMS
	}
	if !p.NextRoute && !p.ReturnError {
		p.ReturnError = true
	}
}

// ValidateRouteFailurePolicy 校验一条策略。CLI 与 store 写入口共用它。
//
// 流程位：NextRoute 与 ReturnError 互斥；两者都没写是允许的，写入口会按
// NormalizeRouteFailurePolicy 归一为 ReturnError，所以校验本身不重复报错。
func ValidateRouteFailurePolicy(p RouteFailurePolicy) error {
	if p.AccountID < 0 {
		return fmt.Errorf("账号 id 不能为负数，当前是 %d", p.AccountID)
	}
	if p.Priority < 0 {
		return fmt.Errorf("优先级不能为负数，当前是 %d", p.Priority)
	}
	if err := ValidateStatusTokens(p.Statuses); err != nil {
		return err
	}
	if len(p.ErrorContains) > routeFailureMaxContains {
		return fmt.Errorf("错误文本条件最多 %d 条，当前 %d 条", routeFailureMaxContains, len(p.ErrorContains))
	}
	for _, s := range p.ErrorContains {
		if strings.TrimSpace(s) == "" {
			return errors.New("错误文本条件不能为空串")
		}
		if utf8.RuneCountInString(s) > routeFailureMaxContainsRunes {
			return fmt.Errorf("错误文本条件 %q 超过 %d 个字符", s, routeFailureMaxContainsRunes)
		}
	}
	if p.ErrorRegex != "" {
		if utf8.RuneCountInString(p.ErrorRegex) > routeFailureMaxRegexRunes {
			return fmt.Errorf("错误正则最长 %d 个字符，当前 %d 个", routeFailureMaxRegexRunes, utf8.RuneCountInString(p.ErrorRegex))
		}
		if _, err := regexp.Compile(p.ErrorRegex); err != nil {
			return fmt.Errorf("错误正则 %q 无法编译：%w", p.ErrorRegex, err)
		}
	}
	if p.RetryCount < 0 || p.RetryCount > routeFailureMaxRetryCount {
		return fmt.Errorf("重试次数必须在 0 到 %d 之间，当前是 %d", routeFailureMaxRetryCount, p.RetryCount)
	}
	if p.RetryBaseMS != 0 && (p.RetryBaseMS < routeFailureMinRetryBaseMS || p.RetryBaseMS > routeFailureMaxRetryBaseMS) {
		return fmt.Errorf("重试起始等待必须在 0 或 %d 到 %d 毫秒之间，当前是 %d",
			routeFailureMinRetryBaseMS, routeFailureMaxRetryBaseMS, p.RetryBaseMS)
	}
	if p.CooldownSeconds != 0 && (p.CooldownSeconds < 1 || p.CooldownSeconds > routeFailureMaxCooldownSeconds) {
		return fmt.Errorf("冷却时长必须在 1 秒到 30 天之间，当前是 %d 秒", p.CooldownSeconds)
	}

	hasAction := p.RetryCount > 0 || p.CooldownSeconds > 0 || p.Disable || p.NextRoute || p.ReturnError
	if !hasAction {
		return errors.New("至少要有一个动作：--retry、--cooldown、--disable、--next_route 或 --return")
	}
	if p.Disable && p.CooldownSeconds > 0 {
		return errors.New("--disable 与 --cooldown 不能同时使用")
	}
	if p.NextRoute && p.ReturnError {
		return errors.New("--next_route 与 --return 不能同时使用")
	}
	return nil
}

// ParseStatusTokens 解析逗号分隔的状态条件，返回去空白、转小写并去重后的
// 规范 token（如 400、4xx、5xx）。空串返回 nil。
func ParseStatusTokens(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(s, ",") {
		token, err := canonicalStatusToken(raw)
		if err != nil {
			return nil, err
		}
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out, nil
}

// ValidateStatusTokens 校验状态 token：只收 400–599 的精确码与 4xx、5xx，
// 并且同一个 token 不能重复（4xx 与 400 不算重复，前者覆盖一段）。
func ValidateStatusTokens(tokens []string) error {
	seen := make(map[string]bool, len(tokens))
	for _, raw := range tokens {
		token, err := canonicalStatusToken(raw)
		if err != nil {
			return err
		}
		if seen[token] {
			return fmt.Errorf("状态条件 %q 重复", token)
		}
		seen[token] = true
	}
	return nil
}

func canonicalStatusToken(raw string) (string, error) {
	token := strings.ToLower(strings.TrimSpace(raw))
	if token == "" {
		return "", errors.New("状态条件不能为空")
	}
	if token == "4xx" || token == "5xx" {
		return token, nil
	}
	if len(token) == 3 {
		n, err := strconv.Atoi(token)
		if err == nil && n >= 400 && n <= 599 {
			return token, nil
		}
	}
	return "", fmt.Errorf("状态条件 %q 不合法，只收 400–599 的精确码与 4xx、5xx", raw)
}

// StatusTokenMatches 判断状态码是否命中 token 列表。空列表匹配任意状态；
// 4xx 覆盖 400–499，5xx 覆盖 500–599，其余按精确码比较。
func StatusTokenMatches(tokens []string, status int) bool {
	if len(tokens) == 0 {
		return true
	}
	exact := strconv.Itoa(status)
	for _, raw := range tokens {
		token := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case token == exact:
			return true
		case token == "4xx" && status >= 400 && status <= 499:
			return true
		case token == "5xx" && status >= 500 && status <= 599:
			return true
		}
	}
	return false
}

// ListRouteFailurePolicies 返回启用的用户规则，按 priority ASC、id ASC 排列。
// account.MatchFailure 按这个顺序取第一条命中的规则。
func (s *Store) ListRouteFailurePolicies() ([]RouteFailurePolicy, error) {
	return s.listRouteFailurePolicies(true)
}

// ListAllRouteFailurePolicies 返回全部用户规则（含停用的），排序同 ListRouteFailurePolicies。
func (s *Store) ListAllRouteFailurePolicies() ([]RouteFailurePolicy, error) {
	return s.listRouteFailurePolicies(false)
}

func (s *Store) listRouteFailurePolicies(enabledOnly bool) ([]RouteFailurePolicy, error) {
	q := s.gorm
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	var rows []routeFailurePolicyRow
	if err := q.Order("priority ASC").Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RouteFailurePolicy, 0, len(rows))
	for _, row := range rows {
		p, err := routeFailurePolicyFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GetRouteFailurePolicy 按 id 取一条规则。不存在时返回 ErrRouteFailurePolicyNotFound。
func (s *Store) GetRouteFailurePolicy(id int64) (RouteFailurePolicy, error) {
	var row routeFailurePolicyRow
	err := s.gorm.Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RouteFailurePolicy{}, ErrRouteFailurePolicyNotFound
	}
	if err != nil {
		return RouteFailurePolicy{}, err
	}
	return routeFailurePolicyFromRow(row)
}

// CreateRouteFailurePolicy 新建一条规则，写回自增 id。
// created_at 与 updated_at 在同一个时间点写入，避免创建后立刻出现 updated_at < created_at。
func (s *Store) CreateRouteFailurePolicy(p *RouteFailurePolicy) error {
	if p == nil {
		return errors.New("要新建的失败处理策略为空")
	}
	NormalizeRouteFailurePolicy(p)
	if err := ValidateRouteFailurePolicy(*p); err != nil {
		return err
	}
	now := unixNow()
	p.ID = 0
	p.CreatedAt = now
	p.UpdatedAt = now
	row, err := routeFailurePolicyRowFrom(*p)
	if err != nil {
		return err
	}
	if err := s.gorm.Create(&row).Error; err != nil {
		return err
	}
	p.ID = row.ID
	return nil
}

// UpdateRouteFailurePolicy 整条覆盖一条已有规则（未给出的字段落成零值），
// updated_at 由仓储层统一置为当前时间。规则不存在时返回 ErrRouteFailurePolicyNotFound。
// 传入的是值，归一化后的默认流程不会回写到调用方；CLI 可以先调用
// NormalizeRouteFailurePolicy 再打印。
func (s *Store) UpdateRouteFailurePolicy(p RouteFailurePolicy) error {
	if p.ID <= 0 {
		return errors.New("更新失败处理策略需要有效的 id")
	}
	NormalizeRouteFailurePolicy(&p)
	if err := ValidateRouteFailurePolicy(p); err != nil {
		return err
	}
	row, err := routeFailurePolicyRowFrom(p)
	if err != nil {
		return err
	}
	// account_id 用无类型 nil 落成 NULL；直接把 *int64(nil) 放进 map 交给 GORM，
	// 不同驱动对带类型的空指针处理不一致。
	var accountID any
	if row.AccountID != nil {
		accountID = *row.AccountID
	}
	var enabled bool
	if row.Enabled != nil {
		enabled = *row.Enabled
	}
	return s.withTx(func(tx *gorm.DB) error {
		// 先确认行还在再更新：MySQL 的 UPDATE 在值没有实际变化时 RowsAffected 返回 0，
		// 不能拿它判断「行不存在」（同 plugin.go 的 updatePluginRow）。updated_at 是秒级，
		// 同一秒内再写一次值不变，这里就会误报「规则不存在」。
		exists, err := routeFailurePolicyExists(tx, p.ID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrRouteFailurePolicyNotFound
		}
		return tx.Model(&routeFailurePolicyRow{}).Where("id = ?", p.ID).Updates(map[string]any{
			"enabled":          enabled,
			"priority":         row.Priority,
			"account_id":       accountID,
			"provider":         row.Provider,
			"model":            row.Model,
			"statuses":         row.Statuses,
			"error_contains":   row.ErrorContains,
			"error_regex":      row.ErrorRegex,
			"retry_count":      row.RetryCount,
			"retry_base_ms":    row.RetryBaseMS,
			"cooldown_seconds": row.CooldownSeconds,
			"disable":          row.Disable,
			"next_route":       row.NextRoute,
			"return_error":     row.ReturnError,
			"note":             row.Note,
			"updated_at":       unixNow(),
		}).Error
	})
}

// DeleteRouteFailurePolicy 删除一条用户规则。规则不存在时返回 ErrRouteFailurePolicyNotFound。
func (s *Store) DeleteRouteFailurePolicy(id int64) error {
	res := s.gorm.Where("id = ?", id).Delete(&routeFailurePolicyRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRouteFailurePolicyNotFound
	}
	return nil
}

// SetRouteFailurePolicyEnabled 启停一条用户规则，不动其它字段。规则不存在时返回
// ErrRouteFailurePolicyNotFound。updated_at 同样由仓储层写。
func (s *Store) SetRouteFailurePolicyEnabled(id int64, enabled bool) error {
	return s.withTx(func(tx *gorm.DB) error {
		exists, err := routeFailurePolicyExists(tx, id)
		if err != nil {
			return err
		}
		if !exists {
			return ErrRouteFailurePolicyNotFound
		}
		return tx.Model(&routeFailurePolicyRow{}).Where("id = ?", id).Updates(map[string]any{
			"enabled":    enabled,
			"updated_at": unixNow(),
		}).Error
	})
}

// routeFailurePolicyExists 在同一个事务里确认规则还在。见 UpdateRouteFailurePolicy 的注释：
// UPDATE 的 RowsAffected 不能当存在性判断用。
func routeFailurePolicyExists(tx *gorm.DB, id int64) (bool, error) {
	var count int64
	if err := tx.Model(&routeFailurePolicyRow{}).Where("id = ?", id).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// decodeStringList 读 JSON 数组文本。空串、[] 与 null 都表示「没有值」（任意）。
func decodeStringList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeStringList 写 JSON 数组文本，空列表写空串。
func encodeStringList(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func routeFailurePolicyFromRow(r routeFailurePolicyRow) (RouteFailurePolicy, error) {
	statuses, err := decodeStringList(r.Statuses)
	if err != nil {
		return RouteFailurePolicy{}, fmt.Errorf("策略 %d 的状态条件不是 JSON 数组：%w", r.ID, err)
	}
	contains, err := decodeStringList(r.ErrorContains)
	if err != nil {
		return RouteFailurePolicy{}, fmt.Errorf("策略 %d 的错误文本条件不是 JSON 数组：%w", r.ID, err)
	}
	var accountID int64
	if r.AccountID != nil {
		accountID = *r.AccountID
	}
	enabled := r.Enabled != nil && *r.Enabled
	return RouteFailurePolicy{
		ID:              r.ID,
		Enabled:         enabled,
		Priority:        r.Priority,
		AccountID:       accountID,
		Provider:        r.Provider,
		Model:           r.Model,
		Statuses:        statuses,
		ErrorContains:   contains,
		ErrorRegex:      r.ErrorRegex,
		RetryCount:      r.RetryCount,
		RetryBaseMS:     r.RetryBaseMS,
		CooldownSeconds: r.CooldownSeconds,
		Disable:         r.Disable,
		NextRoute:       r.NextRoute,
		ReturnError:     r.ReturnError,
		Note:            r.Note,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}, nil
}

func routeFailurePolicyRowFrom(p RouteFailurePolicy) (routeFailurePolicyRow, error) {
	statuses, err := encodeStringList(p.Statuses)
	if err != nil {
		return routeFailurePolicyRow{}, fmt.Errorf("编码状态条件：%w", err)
	}
	contains, err := encodeStringList(p.ErrorContains)
	if err != nil {
		return routeFailurePolicyRow{}, fmt.Errorf("编码错误文本条件：%w", err)
	}
	enabled := p.Enabled
	row := routeFailurePolicyRow{
		ID:              p.ID,
		Enabled:         &enabled,
		Priority:        p.Priority,
		Provider:        p.Provider,
		Model:           p.Model,
		Statuses:        statuses,
		ErrorContains:   contains,
		ErrorRegex:      p.ErrorRegex,
		RetryCount:      p.RetryCount,
		RetryBaseMS:     p.RetryBaseMS,
		CooldownSeconds: p.CooldownSeconds,
		Disable:         p.Disable,
		NextRoute:       p.NextRoute,
		ReturnError:     p.ReturnError,
		Note:            p.Note,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
	if p.AccountID != 0 {
		id := p.AccountID
		row.AccountID = &id
	}
	return row, nil
}
