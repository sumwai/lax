package store

// 旧库搬迁：把 `migrations/*.sql` 时代的库搬到现在的 model 形状。
//
// 为什么需要单独的搬迁而不是让 AutoMigrate 接管：AutoMigrate 靠比对 sqlite_master 里的建表
// 文本判断列是否一致，旧 DDL 里的行内注释会让它的列解析出错。实测在真实库副本上，它会重建
// 三张表，`provider_models` 的重建 INSERT 列清单里缺 `context_window`，该列数据静默丢失。
//
// 搬迁的做法是「读出全部行 → 建新库 → 写回 → 原子替换」，不依赖任何 DDL 比对：
//  1. 按列名读旧库的小表（哪张不存在就跳过），requests 边读边写，不落内存；
//  2. 在 `<路径>.new` 上建新库并写种子；
//  3. 按最终语义转换后写回：`account_models.status` 映射成 `access`，孤儿账号补厂商行，
//     `default_protocol` 补齐，`accounts.models` 拆进目录与可达性，账号级的价搬进覆盖列，
//     历史请求行的 `in_tokens_uncached` 用 `in_tokens` 回填；
//  4. 成功后把原文件改名为 `.bak`，再把 `.new` 改名为原路径。
//
// 第 4 步之前原文件一直是完整的：中途失败只留下 `.new` 与 `.bak`，原库不动。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// rowValues 是一行旧数据：列名与值按位置对应，便于按列名取值。
//
// 旧库各版本的表结构不同（`accounts` 早期带 base_url 与 protocol，`requests` 早期没有
// in_tokens_uncached 与两个 reasoning 列），所以一律用「列存在才取、不存在给零值」的方式读。
type rowValues struct {
	cols []string
	vals []any
}

func (r rowValues) index(name string) int {
	for i, c := range r.cols {
		if c == name {
			return i
		}
	}
	return -1
}

func (r rowValues) has(name string) bool { return r.index(name) >= 0 }

func (r rowValues) raw(name string) any {
	i := r.index(name)
	if i < 0 {
		return nil
	}
	return r.vals[i]
}

func (r rowValues) str(name string) string {
	switch v := r.raw(name).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (r rowValues) int64(name string) int64 {
	switch v := r.raw(name).(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case []byte:
		var n int64
		if _, err := fmt.Sscan(string(v), &n); err == nil {
			return n
		}
		return 0
	default:
		return 0
	}
}

func (r rowValues) nullInt64(name string) *int64 {
	if r.raw(name) == nil {
		return nil
	}
	v := r.int64(name)
	return &v
}

func (r rowValues) nullFloat(name string) *float64 {
	switch v := r.raw(name).(type) {
	case nil:
		return nil
	case float64:
		return &v
	case int64:
		f := float64(v)
		return &f
	case []byte:
		var f float64
		if _, err := fmt.Sscan(string(v), &f); err == nil {
			return &f
		}
		return nil
	default:
		return nil
	}
}

func (r rowValues) bool(name string) bool { return r.int64(name) != 0 }

func (r rowValues) bytes(name string) []byte {
	switch v := r.raw(name).(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil
	}
}

// nullStr 返回可空的文本：列不存在或值为 NULL 时是 nil。
func (r rowValues) nullStr(name string) *string {
	if r.raw(name) == nil {
		return nil
	}
	v := r.str(name)
	return &v
}

// accessFromLegacyStatus 把 0015 之前的账号级状态词表映射成可达性。
//
// 旧词表：synced（同步到但没探活）、usable、forbidden、notfound、auth、error。
// 新词表只有 allowed、forbidden、missing、error 四个：synced 与 usable 都算 allowed
// （前者是「还没探」），notfound 是 missing，auth 与 error 都落到 error，空值按新行默认
// 的 allowed 处理。
func accessFromLegacyStatus(status string) string {
	switch status {
	case "", "synced", "usable", AccessAllowed:
		return AccessAllowed
	case AccessForbidden:
		return AccessForbidden
	case "notfound", AccessMissing:
		return AccessMissing
	default:
		return AccessError
	}
}

// legacyDefaultProtocol 是 0007 迁移给内置厂商补的默认协议，用户自建的厂商返回空串。
//
// 不能一律按协议名排序猜：deepseek 与 commandcode 都声明了 openai-chat 与 anthropic
// 两条路由，排序会把 anthropic 排前面，默认协议会从 openai-chat 变成 anthropic。
func legacyDefaultProtocol(name string) string {
	for _, p := range seedProviders {
		if p.name == name {
			return p.defaultProtocol
		}
	}
	return ""
}

// legacyModelNames 解析 0015 之前的 `accounts.models` 列：JSON 字符串数组。
// 列不存在、不是合法 JSON 或元素不是字符串时返回 nil，与迁移里的 json_valid 判断一致。
func legacyModelNames(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// legacySnapshot 是旧库里除 requests 之外的小表数据。requests 可能有几十万行，边读边写。
type legacySnapshot struct {
	accounts       []rowValues
	accountModels  []rowValues
	providers      []rowValues
	routes         []rowValues
	headers        []rowValues
	providerModels []rowValues
	aliases        []rowValues
}

func readSnapshot(db *sql.DB) (legacySnapshot, error) {
	var snap legacySnapshot
	var err error
	if snap.accounts, err = readAll(db, "accounts"); err != nil {
		return snap, err
	}
	if snap.accountModels, err = readAll(db, "account_models"); err != nil {
		return snap, err
	}
	if snap.providers, err = readAll(db, "providers"); err != nil {
		return snap, err
	}
	if snap.routes, err = readAll(db, "provider_routes"); err != nil {
		return snap, err
	}
	if snap.headers, err = readAll(db, "provider_headers"); err != nil {
		return snap, err
	}
	if snap.providerModels, err = readAll(db, "provider_models"); err != nil {
		return snap, err
	}
	if snap.aliases, err = readAll(db, "model_aliases"); err != nil {
		return snap, err
	}
	return snap, nil
}

// buildProviders 合出厂商行：种子厂商、旧库的行、孤儿账号补建的行。
//
// 种子先铺上，旧库的行再覆盖：这样「旧库没有 google-gemini 厂商行、但有 provider 指向它的
// 账号」不会被当成孤儿去补一个指向账号旧地址的厂商行。那个账号的地址本来就该丢——0009 的
// 种子把厂商行建在官方地址上，账号的旧地址只在它自己那行里，随列一起消失。
func buildProviders(snap legacySnapshot) []providerRow {
	byName := make(map[string]providerRow, len(seedProviders)+len(snap.providers))
	var order []string
	for _, p := range seedProviders {
		byName[p.name] = providerRow{
			Name:            p.name,
			BaseURL:         p.baseURL,
			Note:            p.note,
			DefaultProtocol: p.defaultProtocol,
			Origin:          ProviderOriginBuiltin,
		}
		order = append(order, p.name)
	}
	for _, r := range snap.providers {
		p := providerRow{
			Name:            r.str("name"),
			BaseURL:         r.str("base_url"),
			Note:            r.str("note"),
			DefaultProtocol: r.str("default_protocol"),
			ModelsPath:      r.str("models_path"),
			Origin:          r.str("origin"),
			PluginID:        r.str("plugin_id"),
		}
		if p.Origin == "" {
			switch {
			case p.PluginID != "":
				p.Origin = ProviderOriginPlugin
			case isBuiltinProvider(p.Name):
				p.Origin = ProviderOriginBuiltin
			default:
				p.Origin = ProviderOriginManual
			}
		}
		if p.DefaultProtocol == "" {
			p.DefaultProtocol = legacyDefaultProtocol(p.Name)
		}
		if _, seen := byName[p.Name]; !seen {
			order = append(order, p.Name)
		}
		byName[p.Name] = p
	}

	// 0014：`provider` 不在 providers 表里的账号补一行厂商。同名账号有多个地址时取最小的
	// 那个，结果与行序、与重跑都无关。
	orphanBase := map[string]string{}
	orphanProto := map[string]string{}
	for _, r := range snap.accounts {
		name := r.str("provider")
		if name == "" {
			continue
		}
		if _, ok := byName[name]; ok {
			continue
		}
		base := r.str("base_url")
		if cur, ok := orphanBase[name]; !ok || (base != "" && (cur == "" || base < cur)) {
			orphanBase[name] = base
		}
		if _, ok := orphanProto[name]; !ok {
			if p := r.str("protocol"); IsWireProtocol(p) {
				orphanProto[name] = p
			}
		}
	}
	orphans := make([]string, 0, len(orphanBase))
	for name := range orphanBase {
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		byName[name] = providerRow{
			Name:            name,
			BaseURL:         orphanBase[name],
			Note:            "由旧库搬迁从账号地址补建",
			DefaultProtocol: orphanProto[name],
			Origin:          ProviderOriginManual,
		}
		order = append(order, name)
	}

	out := make([]providerRow, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

func buildRoutes(snap legacySnapshot) []providerRouteRow {
	out := make([]providerRouteRow, 0, len(snap.routes))
	for _, r := range snap.routes {
		out = append(out, providerRouteRow{
			Provider:   r.str("provider"),
			Protocol:   r.str("protocol"),
			Path:       r.str("path"),
			ModelsPath: r.str("models_path"),
		})
	}
	return out
}

func buildHeaders(snap legacySnapshot) []providerHeaderRow {
	out := make([]providerHeaderRow, 0, len(snap.headers))
	for _, r := range snap.headers {
		out = append(out, providerHeaderRow{
			Provider: r.str("provider"),
			Protocol: r.str("protocol"),
			Name:     r.str("name"),
			Value:    r.str("value"),
		})
	}
	return out
}

// buildProviderModels 合出模型目录：旧库的目录行，加上 0015 从 `accounts.models` 补的
// 手填名字（source=manual）。同名模型以目录行优先。
func buildProviderModels(snap legacySnapshot) []providerModelRow {
	seen := make(map[[2]string]struct{}, len(snap.providerModels))
	out := make([]providerModelRow, 0, len(snap.providerModels))
	for _, r := range snap.providerModels {
		row := providerModelRow{
			Provider:        r.str("provider"),
			Model:           r.str("model"),
			Source:          r.str("source"),
			ContextWindow:   r.nullInt64("context_window"),
			MaxOutput:       r.nullInt64("max_output"),
			Protocols:       r.str("protocols"),
			Capabilities:    r.str("capabilities"),
			PriceIn:         r.nullFloat("price_in"),
			PriceOut:        r.nullFloat("price_out"),
			PriceCacheRead:  r.nullFloat("price_cache_read"),
			PriceCacheWrite: r.nullFloat("price_cache_write"),
			UpdatedAt:       r.int64("updated_at"),
		}
		key := [2]string{row.Provider, row.Model}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}

	for _, r := range snap.accounts {
		provider := r.str("provider")
		if provider == "" {
			continue
		}
		for _, name := range legacyModelNames(r.str("models")) {
			key := [2]string{provider, name}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, providerModelRow{Provider: provider, Model: name, Source: "manual"})
		}
	}
	return out
}

func buildAliases(snap legacySnapshot) []modelAliasRow {
	out := make([]modelAliasRow, 0, len(snap.aliases))
	for _, r := range snap.aliases {
		out = append(out, modelAliasRow{
			Alias:     r.str("alias"),
			Target:    r.str("target"),
			CreatedAt: r.int64("created_at"),
		})
	}
	return out
}

func buildAccounts(snap legacySnapshot) []accountRow {
	out := make([]accountRow, 0, len(snap.accounts))
	for _, r := range snap.accounts {
		out = append(out, accountRow{
			ID:            r.int64("id"),
			Name:          r.str("name"),
			Provider:      r.str("provider"),
			KeyCipher:     r.bytes("key_cipher"),
			Priority:      int(r.int64("priority")),
			Enabled:       r.bool("enabled"),
			CooldownUntil: r.nullInt64("cooldown_until"),
			LastError:     r.nullStr("last_error"),
			FailStreak:    int(r.int64("fail_streak")),
			CreatedAt:     r.int64("created_at"),
		})
	}
	return out
}

// buildAccountModels 合出可达性：旧库的行（status 映射成 access）、0016 从账号行搬来的价、
// 0015 从 `accounts.models` 补的 allowed 行。
func buildAccountModels(snap legacySnapshot) []accountModelRow {
	type prices struct {
		in, out, cacheRead, cacheWrite *float64
	}
	accPrice := make(map[int64]prices, len(snap.accounts))
	for _, r := range snap.accounts {
		accPrice[r.int64("id")] = prices{
			in:         r.nullFloat("price_in"),
			out:        r.nullFloat("price_out"),
			cacheRead:  r.nullFloat("price_cache_read"),
			cacheWrite: r.nullFloat("price_cache_write"),
		}
	}

	seen := make(map[[2]any]struct{}, len(snap.accountModels))
	out := make([]accountModelRow, 0, len(snap.accountModels))
	for _, r := range snap.accountModels {
		access := r.str("access")
		if access == "" {
			access = accessFromLegacyStatus(r.str("status"))
		}
		row := accountModelRow{
			AccountID:       r.int64("account_id"),
			Model:           r.str("model"),
			Access:          access,
			HTTPStatus:      int(r.int64("http_status")),
			Error:           r.str("error"),
			CheckedAt:       r.int64("checked_at"),
			Protocol:        r.str("protocol"),
			ContextWindow:   r.nullInt64("context_window"),
			MaxOutput:       r.nullInt64("max_output"),
			PriceIn:         r.nullFloat("price_in"),
			PriceOut:        r.nullFloat("price_out"),
			PriceCacheRead:  r.nullFloat("price_cache_read"),
			PriceCacheWrite: r.nullFloat("price_cache_write"),
		}
		key := [2]any{row.AccountID, row.Model}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}

	// 0015：`accounts.models` 里的名字补成 allowed 行，已存在的不动。
	for _, r := range snap.accounts {
		id := r.int64("id")
		for _, name := range legacyModelNames(r.str("models")) {
			key := [2]any{id, name}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, accountModelRow{AccountID: id, Model: name, Access: AccessAllowed})
		}
	}

	// 0016：账号行的四个价搬进覆盖列，只补空的，已有覆盖值不动。
	// 这一遗必须在上面两步之后：从 `accounts.models` 拆出来的行同样要拿到价。
	for i := range out {
		p, ok := accPrice[out[i].AccountID]
		if !ok {
			continue
		}
		if out[i].PriceIn == nil {
			out[i].PriceIn = p.in
		}
		if out[i].PriceOut == nil {
			out[i].PriceOut = p.out
		}
		if out[i].PriceCacheRead == nil {
			out[i].PriceCacheRead = p.cacheRead
		}
		if out[i].PriceCacheWrite == nil {
			out[i].PriceCacheWrite = p.cacheWrite
		}
	}
	return out
}

// legacyBatch 是一批搬迁的行数。
const legacyBatch = 500

// writeRows 把一批行写进新库。旧库的数据优先：种子在建新库时已经写过一次，这里用 upsert
// 让旧库的行覆盖它——用户可能改过内置厂商的地址或备注，搬迁不能把它们退回种子值。
func writeRows[T any](dst *gorm.DB, table string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	return dst.Table(table).Clauses(clause.OnConflict{UpdateAll: true}).
		CreateInBatches(&rows, legacyBatch).Error
}

// upgradeLegacy 把一个旧版 SQLite 库搬到新形状，返回搬迁的总行数。
//
// 原文件成功后才被替换：失败时原库仍是完整的，可以直接重跑。
func upgradeLegacy(path string) (int, error) {
	old, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return 0, fmt.Errorf("打开旧库: %w", err)
	}
	defer old.Close()

	snap, err := readSnapshot(old)
	if err != nil {
		return 0, fmt.Errorf("读旧库: %w", err)
	}

	newPath := path + ".new"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(newPath + suffix)
	}

	fresh, err := OpenWith(Options{Path: newPath})
	if err != nil {
		return 0, fmt.Errorf("建新库: %w", err)
	}

	total := 0
	write := func(table string, n int, err error) error {
		if err != nil {
			return fmt.Errorf("搬 %s: %w", table, err)
		}
		total += n
		return nil
	}

	providers := buildProviders(snap)
	if err := write("providers", len(providers), writeRows(fresh.gorm, "providers", providers)); err != nil {
		_ = fresh.Close()
		return 0, err
	}
	routes := buildRoutes(snap)
	if err := write("provider_routes", len(routes), writeRows(fresh.gorm, "provider_routes", routes)); err != nil {
		_ = fresh.Close()
		return 0, err
	}
	headers := buildHeaders(snap)
	if err := write("provider_headers", len(headers), writeRows(fresh.gorm, "provider_headers", headers)); err != nil {
		_ = fresh.Close()
		return 0, err
	}
	catalog := buildProviderModels(snap)
	if err := write("provider_models", len(catalog), writeRows(fresh.gorm, "provider_models", catalog)); err != nil {
		_ = fresh.Close()
		return 0, err
	}
	aliases := buildAliases(snap)
	if err := write("model_aliases", len(aliases), writeRows(fresh.gorm, "model_aliases", aliases)); err != nil {
		_ = fresh.Close()
		return 0, err
	}
	accounts := buildAccounts(snap)
	if err := write("accounts", len(accounts), writeRows(fresh.gorm, "accounts", accounts)); err != nil {
		_ = fresh.Close()
		return 0, err
	}
	access := buildAccountModels(snap)
	if err := write("account_models", len(access), writeRows(fresh.gorm, "account_models", access)); err != nil {
		_ = fresh.Close()
		return 0, err
	}

	n, err := copyRequests(old, fresh.gorm)
	if err := write("requests", n, err); err != nil {
		_ = fresh.Close()
		return 0, err
	}

	if err := fresh.Close(); err != nil {
		return 0, fmt.Errorf("关闭新库: %w", err)
	}

	// 原库留一份 .bak，再把新库换上去。顺序不能反：先改名原库，失败时原库还在 .bak 里。
	if err := old.Close(); err != nil {
		return 0, fmt.Errorf("关闭旧库: %w", err)
	}
	if err := os.Rename(path, path+".bak"); err != nil {
		return 0, fmt.Errorf("备份旧库: %w", err)
	}
	if err := os.Rename(newPath, path); err != nil {
		return 0, fmt.Errorf("替换数据库（旧库已备份为 %s）: %w", filepath.Base(path)+".bak", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	return total, nil
}

// copyRequests 流式搬请求记录，返回行数。
//
// 0011 之前的库没有 `in_tokens_uncached` 列：那一版迁移按当时的 `in_tokens` 回填，不写 0
// （写 0 会让历史花费凭空变成只算输出）。
func copyRequests(old *sql.DB, dst *gorm.DB) (int, error) {
	cols, ok, err := legacyColumns(old, "requests")
	if err != nil || !ok {
		return 0, err
	}
	rows, err := old.Query("SELECT * FROM requests")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	total := 0
	batch := make([]requestRow, 0, legacyBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := dst.Table("requests").CreateInBatches(&batch, legacyBatch).Error; err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return total, err
		}
		r := rowValues{cols: cols, vals: vals}
		uncached := r.int64("in_tokens_uncached")
		if !r.has("in_tokens_uncached") {
			uncached = r.int64("in_tokens")
		}
		batch = append(batch, requestRow{
			ID:                   r.int64("id"),
			TS:                   r.int64("ts"),
			Inbound:              r.str("inbound"),
			Outbound:             r.str("outbound"),
			ModelRequested:       r.str("model_requested"),
			ModelUsed:            r.str("model_used"),
			AccountID:            r.nullInt64("account_id"),
			HTTPStatus:           int(r.int64("http_status")),
			OK:                   r.bool("ok"),
			InTokens:             r.int64("in_tokens"),
			InTokensUncached:     uncached,
			OutTokens:            r.int64("out_tokens"),
			CacheRead:            r.int64("cache_read"),
			CacheWrite:           r.int64("cache_write"),
			ReasoningTokens:      r.int64("reasoning_tokens"),
			ReasoningTokensExtra: r.int64("reasoning_tokens_extra"),
			LatencyMS:            r.int64("latency_ms"),
			Error:                r.nullStr("error"),
		})
		if len(batch) >= legacyBatch {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return total, err
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

// legacyColumns 读出旧库某张表的列名，表不存在时返回 ok=false。
func legacyColumns(db *sql.DB, table string) ([]string, bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, false, fmt.Errorf("读 %s 的列: %w", table, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, false, fmt.Errorf("读 %s 的列: %w", table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return cols, len(cols) > 0, nil
}

// readAll 把一张小表整个读进内存。表不存在时返回 nil。
func readAll(db *sql.DB, table string) ([]rowValues, error) {
	cols, ok, err := legacyColumns(db, table)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := db.Query(fmt.Sprintf("SELECT * FROM %s", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rowValues
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, rowValues{cols: cols, vals: vals})
	}
	return out, rows.Err()
}
