package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"lax/internal/secret"
)

// ErrProviderHeaderNotFound 表示这家厂商没有声明这条附加头。`provider header unset` 用 errors.Is
// 判它，把「删不到」与「厂商不存在」（ErrProviderNotFound）分开报：前者是写错了头名或协议，
// 后者是写错了厂商。
var ErrProviderHeaderNotFound = errors.New("厂商没有声明这条附加头")

// ProviderHeader 是一行厂商附加请求头（迁移 0010，见 DESIGN 5.10）。
// Protocol 为空字符串表示整个厂商的每条路由都加，非空表示只加给这条协议；同名头两边都有时
// 协议级覆盖厂商级，解析在 EffectiveProviderHeaders。Name 落库前归一成小写（HTTP 头名大小写
// 不敏感），Value 是声明的原样：字面量，或 {env:NAME}、{session}、{model} 三种占位符，
// 展开由转发侧做，这一层只负责存取。
type ProviderHeader struct {
	Protocol string
	Name     string
	Value    string
}

// Masked 把值截成前四位加省略号，供日志与表格输出用。
// 附加头的值可能是密钥（自定义鉴权头、会话 token），原样打出去等于泄漏。
func (h ProviderHeader) Masked() string { return secret.Mask(h.Value) }

// String 让 fmt 与 slog 打印附加头时自动只出头名与截断后的值。
// 日志（`slog.Any`、`%v`、`%+v`、整片 `%v`）都不带明文；要看原值直接读 Value 字段。
// 头名照打（排查「这个头发没发」时头名就是全部信息），协议为空串时不带前缀。
func (h ProviderHeader) String() string {
	if h.Protocol == "" {
		return h.Name + "=" + h.Masked()
	}
	return h.Protocol + "/" + h.Name + "=" + h.Masked()
}

// SetProviderHeader 声明（或覆盖）一家厂商的一条附加头，是这张表的唯一写入口。
// 同一个 (厂商, 协议, 头名) 只有一行：再写一次就是改值（UPSERT），不报错、不留旧行。
// 校验：头名必须是合法 HTTP token（大小写不敏感，落库归一成小写），值不能是空白串，
// 也不能带 CR/LF/NUL（那样拼进请求头会把请求拆成两条），协议只能是空串或 IsWireProtocol 认的五种。
// 厂商不存在时报 ErrProviderNotFound：外键虽会拦住这次写入，但报数据库的外键错误等于没说清
// 下一步该干什么。存在性检查与写入在同一个事务里，不会出现半条记录。
func (s *Store) SetProviderHeader(provider, protocol, name, value string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return errors.New("厂商名不能为空")
	}
	proto, err := normalizeHeaderProtocol(protocol)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", provider, err)
	}
	hname, err := normalizeHeaderName(name)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", provider, err)
	}
	hvalue, err := normalizeHeaderValue(value)
	if err != nil {
		return fmt.Errorf("厂商 %s 的头 %s：%w", provider, hname, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireProviderTx(tx, provider); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO provider_headers (provider, protocol, name, value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (provider, protocol, name) DO UPDATE SET value = excluded.value`,
		provider, proto, hname, hvalue); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteProviderHeader 删掉一条声明，是这张表的唯一删除入口。
// 删不到时分两种：厂商还在但没这条头返回 ErrProviderHeaderNotFound（多半是头名或协议写错了，
// 同名头名的另一种协议写法也会落到这里），厂商本身不在返回 ErrProviderNotFound。
// 与 DeleteProvider 一致，删不到都算错误，不静默当成功。
func (s *Store) DeleteProviderHeader(provider, protocol, name string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return errors.New("厂商名不能为空")
	}
	proto, err := normalizeHeaderProtocol(protocol)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", provider, err)
	}
	hname, err := normalizeHeaderName(name)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", provider, err)
	}

	res, err := s.db.Exec(`DELETE FROM provider_headers WHERE provider = ? AND protocol = ? AND name = ?`,
		provider, proto, hname)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := s.GetProvider(provider); err != nil {
			return err
		}
		return fmt.Errorf("%w：%q 在协议 %s 下没有 %q，用 lax provider header ls %s 看已声明的头",
			ErrProviderHeaderNotFound, provider, headerScope(proto), hname, provider)
	}
	return nil
}

// ProviderHeaders 返回一家厂商声明的全部附加头，厂商级（protocol 为空串）与协议级都在里面，
// 按 protocol、name 升序。厂商不存在或一条头都没声明时返回空切片，不算错误——
// 与 ProviderRoutes 的读法一致，这里问的是「声明了什么」，不是「这家厂商在不在」。
func (s *Store) ProviderHeaders(provider string) ([]ProviderHeader, error) {
	return providerHeadersQuery(s.db, provider)
}

// EffectiveProviderHeaders 返回 (厂商, 协议) 这次请求真正要加的头，按 name 升序，是转发侧读头的
// 唯一入口：先取厂商级（protocol 为空串）的声明，再用这个协议自己的声明按同名覆盖。
// 覆盖只按头名（已归一成小写）比，不看值；协议级那行删掉就自动回退到厂商级。
// 厂商不存在、或一条相关声明都没有时返回空切片且不报错：没有附加头是正常情况，
// 不该让一条配置缺失变成对上游的半次调用。
func (s *Store) EffectiveProviderHeaders(provider, protocol string) ([]ProviderHeader, error) {
	proto, err := normalizeHeaderProtocol(protocol)
	if err != nil {
		return nil, err
	}
	rows, err := providerHeadersQuery(s.db, provider)
	if err != nil {
		return nil, err
	}

	// 先铺厂商级，再用协议级覆盖：ProviderHeaders 已按 protocol 升序，空串排在最前，
	// 因此一趟扫下来，后见到的同名协议级声明自然压过先前铺进去的厂商级。
	// 留下的那一行带着自己的 Protocol（空串表示它来自厂商级），不把它改写成这次请求的协议。
	merged := map[string]ProviderHeader{}
	names := make([]string, 0, len(rows))
	for _, h := range rows {
		if h.Protocol != "" && h.Protocol != proto {
			continue
		}
		if _, seen := merged[h.Name]; !seen {
			names = append(names, h.Name)
		}
		merged[h.Name] = h
	}
	sort.Strings(names)

	out := make([]ProviderHeader, 0, len(names))
	for _, name := range names {
		out = append(out, merged[name])
	}
	return out, nil
}

// providerHeadersQuery 按厂商读全部声明，按 protocol、name 升序（空串的厂商级排在最前）。
// 两个读入口共用它：合并只发生在 EffectiveProviderHeaders 里，查询只有一份。
func providerHeadersQuery(db *sql.DB, provider string) ([]ProviderHeader, error) {
	rows, err := db.Query(`SELECT protocol, name, value FROM provider_headers
		WHERE provider = ? ORDER BY protocol, name`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProviderHeader{}
	for rows.Next() {
		var h ProviderHeader
		if err := rows.Scan(&h.Protocol, &h.Name, &h.Value); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// requireProviderTx 在事务里确认厂商行还在，不在时报 ErrProviderNotFound。
// 附加头有外键兜底，但外键报出来的是一句 SQL 错误，说不清下一步该做什么。
func requireProviderTx(tx *sql.Tx, provider string) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM providers WHERE name = ?`, provider).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w：%q，用 lax provider ls 看已配置的厂商", ErrProviderNotFound, provider)
	default:
		return err
	}
}

// headerScope 把协议写成文案里的那一截，空串是「整个厂商」。
func headerScope(protocol string) string {
	if protocol == "" {
		return "整个厂商"
	}
	return protocol
}

// normalizeHeaderProtocol 校验声明的作用范围：空串（整个厂商）或五条线协议之一。
// 别的值一律拒绝：写成 "openai" 这类命令行别名会让这条头永远匹配不上真实协议，
// 症状是「明明声明了却发不出去」。
func normalizeHeaderProtocol(protocol string) (string, error) {
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return "", nil
	}
	if !IsWireProtocol(protocol) {
		return "", fmt.Errorf("协议 %q 不是线协议：只能是空串（整个厂商）或 %s", protocol, wireProtocolNames())
	}
	return protocol, nil
}

// normalizeHeaderName 校验 HTTP 头名并归一成小写。
// 只认 RFC 9110 的 token 字符（字母、数字与 !#$%&'*+-.^_`|~），空格、冒号、折行与中文都不是头名
// 的一部分，带上它们拼出来的请求要么被上游拒，要么在中间被拆成两条头。
func normalizeHeaderName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("头名不能为空")
	}
	for _, r := range name {
		if !isHeaderTokenRune(r) {
			return "", fmt.Errorf("头名 %q 含非法字符 %q：HTTP 头名只能是字母、数字与 %s",
				name, r, headerTokenSymbols)
		}
	}
	return strings.ToLower(name), nil
}

// normalizeHeaderValue 校验头的值：不能是空白串（清掉一条头用 DeleteProviderHeader，不是写空值），
// 也不能带 CR、LF 与 NUL——值原样进请求头，带折行等于让调用方自己拼出第二条头。
func normalizeHeaderValue(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("头的值不能为空：删掉这条头用 provider header unset")
	}
	if i := strings.IndexAny(value, "\r\n\x00"); i >= 0 {
		return "", fmt.Errorf("头的值里不能有回车、换行或 NUL（第 %d 个字节）", i+1)
	}
	return value, nil
}

// headerTokenSymbols 是头名允许的符号集，与 RFC 9110 token 的定义一致。
const headerTokenSymbols = "!#$%&'*+-.^_`|~"

// isHeaderTokenRune 判断一个字符能不能出现在 HTTP 头名里。非 ASCII 一律不行。
func isHeaderTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r < 0x80 && strings.ContainsRune(headerTokenSymbols, r)
}
