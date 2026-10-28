package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrProviderNotFound 表示厂商表里没有这一行。GetProvider 与 DeleteProvider 用 errors.Is 判断它；
// LookupProviderRoute 不返回这个错误，那里用 ok=false 更好用。
var ErrProviderNotFound = errors.New("厂商不存在")

// Provider 是一行厂商。行里只有默认地址与备注，协议的路径细节都在 ProviderRoute 上。
// 账号行仍固化自己的 base_url，改这张表只影响之后新加的账号。
type Provider struct {
	Name    string
	BaseURL string
	Note    string
}

// ProviderRoute 是一行 (厂商, 协议) 的路由。完整地址是厂商的 base_url 去掉末尾斜杠后直接拼 Path，
// 不再做任何后缀猜测。ModelsPath 为空表示这家在这个协议下没有模型列表接口。
type ProviderRoute struct {
	Provider   string
	Protocol   string
	Path       string
	ModelsPath string
}

// ListProviders 按名称升序返回全部厂商。表为空时返回空切片，不算错误。
func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT name, base_url, note FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Provider{}
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Name, &p.BaseURL, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider 按名称取厂商。
func (s *Store) GetProvider(name string) (Provider, error) {
	var p Provider
	err := s.db.QueryRow(`SELECT name, base_url, note FROM providers WHERE name = ?`, name).
		Scan(&p.Name, &p.BaseURL, &p.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, fmt.Errorf("%w：%q，用 switchboard provider ls 看已配置的厂商", ErrProviderNotFound, name)
	}
	if err != nil {
		return Provider{}, err
	}
	return p, nil
}

// ProviderRoutes 返回一个厂商的全部路由，按协议名升序（顺序固定，回退取第一条时结果稳定）。
// 厂商不存在时返回空切片：这是「没配过路由」，不是错误。
func (s *Store) ProviderRoutes(provider string) ([]ProviderRoute, error) {
	rows, err := s.db.Query(`SELECT provider, protocol, path, models_path
		FROM provider_routes WHERE provider = ? ORDER BY protocol`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProviderRoute{}
	for rows.Next() {
		var r ProviderRoute
		if err := rows.Scan(&r.Provider, &r.Protocol, &r.Path, &r.ModelsPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LookupProviderRoute 按 (厂商, 协议) 取一条路由，是读路由的唯一入口：转发、模型同步与探活都走它。
// 厂商不存在或这家没有该协议的路由时 ok=false，且不返回错误，调用方据此退回后缀猜测。
func (s *Store) LookupProviderRoute(provider, protocol string) (ProviderRoute, bool, error) {
	if provider == "" || protocol == "" {
		return ProviderRoute{}, false, nil
	}
	var r ProviderRoute
	err := s.db.QueryRow(`SELECT provider, protocol, path, models_path
		FROM provider_routes WHERE provider = ? AND protocol = ?`, provider, protocol).
		Scan(&r.Provider, &r.Protocol, &r.Path, &r.ModelsPath)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRoute{}, false, nil
	}
	if err != nil {
		return ProviderRoute{}, false, err
	}
	return r, true, nil
}

// ProviderProtocols 返回该厂商声明过路由的协议，按协议名升序。
// 没有厂商行时返回 nil，调用方据此退回「三种协议全试」。
func (s *Store) ProviderProtocols(provider string) ([]string, error) {
	routes, err := s.ProviderRoutes(provider)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routes {
		if r.Protocol != "" {
			out = append(out, r.Protocol)
		}
	}
	return out, nil
}

// AddProvider 在一个事务里写入厂商行与它的路由行：厂商已存在时更新 base_url 与备注，
// 路由按 (厂商, 协议) 覆盖或新增。routes 为空时只写厂商行。
func (s *Store) AddProvider(p Provider, routes []ProviderRoute) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("厂商名不能为空")
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return fmt.Errorf("厂商 %s 缺少 base_url", p.Name)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`INSERT INTO providers (name, base_url, note) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET base_url = excluded.base_url, note = excluded.note`,
		p.Name, p.BaseURL, p.Note); err != nil {
		return err
	}

	for _, r := range routes {
		if r.Protocol == "" || strings.TrimSpace(r.Path) == "" {
			return fmt.Errorf("厂商 %s 的路由缺少协议名或 path", p.Name)
		}
		if _, err := tx.Exec(`INSERT INTO provider_routes (provider, protocol, path, models_path)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(provider, protocol) DO UPDATE SET
				path = excluded.path, models_path = excluded.models_path`,
			p.Name, r.Protocol, r.Path, r.ModelsPath); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteProvider 在一个事务里删掉厂商行与它的全部路由行。两张表之间没有外键，
// 分两条语句删就必须同一个事务，否则中途失败会留下没有厂商的孤儿路由。
// 仍有账号使用这个厂商时这里不拦：那是调用方的事（`provider rm` 要求 --force），
// 账号表只存 provider 字符串，删厂商不影响已有账号的 base_url。
func (s *Store) DeleteProvider(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`DELETE FROM providers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w：%q", ErrProviderNotFound, name)
	}

	if _, err := tx.Exec(`DELETE FROM provider_routes WHERE provider = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAccountsByProvider 返回 provider 字段等于 name 的账号，按 priority、id 升序。
// `provider rm` 用它列出还在用的账号。
func (s *Store) ListAccountsByProvider(name string) ([]Account, error) {
	rows, err := s.db.Query(`SELECT id, name, provider, protocol, base_url, key_cipher, models,
		price_in, price_out, priority, enabled, COALESCE(cooldown_until, 0), COALESCE(last_error, ''), fail_streak, created_at
		FROM accounts WHERE provider = ? ORDER BY priority ASC, id ASC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}
