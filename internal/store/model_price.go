package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 模型单价的写入口与花费的取值链。
//
// 价格属于「供应商 × 模型」：同一家上游的同一个模型只该有一份标价，账号只在套餐不同时覆盖，
// 否则改一次价要改 N 个账号，`usage` 还会给同一件事算出 N 个单价。两个写入口因此分工明确：
//
//   - SetProviderModelPrices 写目录（provider_models）的厂商级标价；
//   - SetAccountModelPrices 写该账号在这个模型上的覆盖值（account_models）。
//
// 清空另走 ClearProviderModelPrices / ClearAccountModelPrices：`model price set` 只写命令行上
// 给过的那几项，nil 表示「这一项不动」（继承），删除必须显式声明：把空值当成删除时，
// 一个写错的参数会静默清掉一项已有的价。
//
// `accounts` 上曾经有同名四列，是模型事实还没拆出账号时的过渡形态：迁移 0016 把它们搬成
// account_models 的覆盖值后删掉，取值链因此只剩上面两层。

var (
	// ErrNegativePrice 表示单价是负数。0 是合法值（免费或打包），负数只可能是写错了。
	ErrNegativePrice = errors.New("模型单价不能是负数")

	// ErrModelNotInCatalog 表示目录里没有 (厂商, 模型) 这一行。价格是模型事实的附属信息，
	// 写入不顺手插一行：模型本身要先由 `model sync` 或 `model probe` 落进目录，
	// 否则会留下一行没有长度、没有协议集合、只有价格的空壳，读目录的人分不清它是不是真的存在。
	ErrModelNotInCatalog = errors.New("模型不在目录里")

	// ErrAccountModelNotRegistered 表示这个账号没有该模型的可达性行。
	// 覆盖价挂在可达性行上（account_models 的主键是 (账号, 模型)），没有那一行就无处可挂；
	// 先同步或探活一次，让这个账号与这个模型的关系有结论。
	ErrAccountModelNotRegistered = errors.New("账号没有这个模型的行")
)

// PriceField 是单价的四个字段名，也就是 `model price set/unset` 的字段参数
// （`in`、`out`、`cache-read`、`cache-write`）。
//
// 字段名与列名的对应只在 store 层有一份：cli 把命令行给的名字原样交过来，
// 不自己拼列名，也不在别处散落裸字符串。
type PriceField string

const (
	// FieldPriceIn 是输入价（每百万 token）。
	FieldPriceIn PriceField = "in"
	// FieldPriceOut 是输出价。
	FieldPriceOut PriceField = "out"
	// FieldPriceCacheRead 是缓存读价，空表示回退到输入价（见 Usage）。
	FieldPriceCacheRead PriceField = "cache-read"
	// FieldPriceCacheWrite 是缓存写价，空表示回退到输入价。
	FieldPriceCacheWrite PriceField = "cache-write"
)

// allPriceFields 是四个字段的固定顺序。错误文案、帮助与清价格的 SET 都按它排，
// 同一份数据多跑几次结论一致。
var allPriceFields = []PriceField{
	FieldPriceIn, FieldPriceOut, FieldPriceCacheRead, FieldPriceCacheWrite,
}

// priceColumns 把字段名映射成两张表里同名的列。两张表（provider_models 与 account_models）
// 的价格列名一致，因此一张表够用。列名只从这里取，不拿用户输入拼 SQL。
var priceColumns = map[PriceField]string{
	FieldPriceIn:         "price_in",
	FieldPriceOut:        "price_out",
	FieldPriceCacheRead:  "price_cache_read",
	FieldPriceCacheWrite: "price_cache_write",
}

// PriceFieldNames 按固定顺序返回四个字段名，供错误文案与帮助文本用。
func PriceFieldNames() []string {
	names := make([]string, 0, len(allPriceFields))
	for _, f := range allPriceFields {
		names = append(names, string(f))
	}
	return names
}

// ParsePriceField 把命令行给的字段名解析成 PriceField。
// 大小写不符、带 `--` 前缀都算写错，报错时列出可用字段：静默纠正会让人以为改的是另一项。
func ParsePriceField(name string) (PriceField, error) {
	trimmed := strings.TrimSpace(name)
	for _, f := range allPriceFields {
		if trimmed == string(f) {
			return f, nil
		}
	}
	hint := ""
	if strings.HasPrefix(trimmed, "-") {
		hint = "；字段是位置参数，不带 -- 前缀"
	}
	return "", fmt.Errorf("未知的价格字段 %q%s，可用的是 %s",
		name, hint, strings.Join(PriceFieldNames(), "、"))
}

// parsePriceFields 解析一组字段名，重复的只保留一次（`model price unset a a` 与单个 a 等价）。
// 一个字段都没给是用法错误：那条 UPDATE 什么都不写，当成功返回等于让人以为清掉了。
func parsePriceFields(fields []string) ([]PriceField, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("清价格至少要给一个字段，可用的是 %s", strings.Join(PriceFieldNames(), "、"))
	}
	out := make([]PriceField, 0, len(fields))
	seen := make(map[PriceField]bool, len(fields))
	for _, raw := range fields {
		f, err := ParsePriceField(raw)
		if err != nil {
			return nil, err
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out, nil
}

// ModelPrices 是四个单价的增量改动：nil 表示「这一项不动」（保留库里已有的值，账号侧即继承目录），
// 0 是合法值（免费），负数报 ErrNegativePrice。目录里的厂商级标价与 account_models 上的账号覆盖价
// 共用这个形状。
type ModelPrices struct {
	In         *float64
	Out        *float64
	CacheRead  *float64
	CacheWrite *float64
}

// empty 表示四项都没给。那样的写入只会刷新 updated_at，调用方该把它当用法错误拦在更外层。
func (p ModelPrices) empty() bool {
	return p.In == nil && p.Out == nil && p.CacheRead == nil && p.CacheWrite == nil
}

// validate 检查四项的取值：负数只能是写错，0 与小数都合法。
func (p ModelPrices) validate(model string) error {
	for _, f := range allPriceFields {
		v := p.field(f)
		if v != nil && *v < 0 {
			return fmt.Errorf("模型 %q 的 %s 价是 %v：%w", model, f, *v, ErrNegativePrice)
		}
	}
	return nil
}

// field 按字段名取对应的那一项，供校验与拼 SET 用。
func (p ModelPrices) field(f PriceField) *float64 {
	switch f {
	case FieldPriceIn:
		return p.In
	case FieldPriceOut:
		return p.Out
	case FieldPriceCacheRead:
		return p.CacheRead
	case FieldPriceCacheWrite:
		return p.CacheWrite
	}
	return nil
}

// SetProviderModelPrices 写目录里 (厂商, 模型) 这一行的厂商级标价。
//
// 行必须已经在目录里，否则报 ErrModelNotInCatalog：价格是模型事实的附属信息，不顺手插一行
// （理由见那个错误值的说明）。只写传进来的那几项，nil 表示这一项不动，0 是合法值。
// 来源等级（source）不动——这只改价格，不动「这一行是谁写的」；updated_at 刷新成当前秒，
// 因为价格确实是这一行的一次改动。
func (s *Store) SetProviderModelPrices(provider, model string, prices ModelPrices) error {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return fmt.Errorf("写模型单价需要厂商名与模型名：目录以 (厂商, 模型) 为主键，两者都不能为空")
	}
	if prices.empty() {
		return fmt.Errorf("厂商 %q 的模型 %q 一项价都没给：这次写入只会刷新 updated_at", provider, model)
	}
	if err := prices.validate(model); err != nil {
		return err
	}

	return s.withTx(func(tx *gorm.DB) error {
		if err := requireProviderModelTx(tx, provider, model); err != nil {
			return err
		}
		return tx.Model(&providerModelRow{}).
			Where("provider = ? AND model = ?", provider, model).
			Updates(map[string]any{
				"price_in":          coalescePrice("price_in", prices.In),
				"price_out":         coalescePrice("price_out", prices.Out),
				"price_cache_read":  coalescePrice("price_cache_read", prices.CacheRead),
				"price_cache_write": coalescePrice("price_cache_write", prices.CacheWrite),
				"updated_at":        time.Now().Unix(),
			}).Error
	})
}

// coalescePrice 拼一个「传了值就改、nil 就不动」的赋值。
//
// COALESCE(?, col) 三方言通用（SQLite、PostgreSQL 与 MySQL 都有这个函数），
// 因此不需要为「只改部分列」写三套代码。
func coalescePrice(column string, v *float64) clause.Expr {
	return gorm.Expr("COALESCE(?, "+column+")", v)
}

// SetAccountModelPrices 写该账号在这个模型上的覆盖价。account 是账号名。
//
// 该账号必须有这个模型的可达性行（不论 access 是 allowed、forbidden 还是别的结论），
// 否则报 ErrAccountModelNotRegistered：覆盖价挂在那一行上，没有结论就说明这个账号与这个模型
// 还没打过交道，先 `model sync` 或 `model probe` 一次。语义与 SetProviderModelPrices 一致：
// nil 不动、0 合法、负数报错。可达性与探活各列一个字不动。
func (s *Store) SetAccountModelPrices(account, model string, prices ModelPrices) error {
	account = strings.TrimSpace(account)
	model = strings.TrimSpace(model)
	if account == "" || model == "" {
		return fmt.Errorf("写账号覆盖价需要账号名与模型名：account_models 以 (账号, 模型) 为主键，两者都不能为空")
	}
	if prices.empty() {
		return fmt.Errorf("账号 %q 的模型 %q 一项价都没给：这次写入什么都不改", account, model)
	}
	if err := prices.validate(model); err != nil {
		return err
	}

	return s.withTx(func(tx *gorm.DB) error {
		accountID, err := accountIDByNameTx(tx, account)
		if err != nil {
			return err
		}
		if err := requireAccountModelTx(tx, accountID, account, model); err != nil {
			return err
		}
		return tx.Model(&accountModelRow{}).
			Where("account_id = ? AND model = ?", accountID, model).
			Updates(map[string]any{
				"price_in":          coalescePrice("price_in", prices.In),
				"price_out":         coalescePrice("price_out", prices.Out),
				"price_cache_read":  coalescePrice("price_cache_read", prices.CacheRead),
				"price_cache_write": coalescePrice("price_cache_write", prices.CacheWrite),
			}).Error
	})
}

// ClearProviderModelPrices 把目录里这一行的指定价格列置 NULL（= 这一项未知）。
//
// 本来为空的字段不报错：清两次与清一次结果相同，幂等。
// 行不在目录里时报 ErrModelNotInCatalog：那多半是模型名写错了，静默当成功会让人以为
// 那一项已经清掉，而实际上整个模型名都是错的。
func (s *Store) ClearProviderModelPrices(provider, model string, fields []string) error {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return fmt.Errorf("清模型单价需要厂商名与模型名：目录以 (厂商, 模型) 为主键，两者都不能为空")
	}
	parsed, err := parsePriceFields(fields)
	if err != nil {
		return err
	}

	return s.withTx(func(tx *gorm.DB) error {
		if err := requireProviderModelTx(tx, provider, model); err != nil {
			return err
		}
		return execSQL(tx, `UPDATE provider_models SET `+clearPriceSet(parsed)+`
			WHERE provider = ? AND model = ?`, provider, model)
	})
}

// ClearAccountModelPrices 把该账号在这个模型上的指定覆盖列置 NULL（= 回到厂商级标价，
// 不是「免费」）。account 是账号名。
//
// 该账号根本没有这个模型的行时不报错：没有行就没有覆盖值，要清的那几项本来就不存在，
// 与「本来为空不报错」同一条幂等规则。账号本身不存在时仍报错——那是账号名写错了。
func (s *Store) ClearAccountModelPrices(account, model string, fields []string) error {
	account = strings.TrimSpace(account)
	model = strings.TrimSpace(model)
	if account == "" || model == "" {
		return fmt.Errorf("清账号覆盖价需要账号名与模型名：account_models 以 (账号, 模型) 为主键，两者都不能为空")
	}
	parsed, err := parsePriceFields(fields)
	if err != nil {
		return err
	}

	return s.withTx(func(tx *gorm.DB) error {
		accountID, err := accountIDByNameTx(tx, account)
		if err != nil {
			return err
		}
		var exists int64
		if err := tx.Model(&accountModelRow{}).
			Where("account_id = ? AND model = ?", accountID, model).
			Count(&exists).Error; err != nil {
			return err
		}
		if exists == 0 {
			return nil
		}
		return execSQL(tx, `UPDATE account_models SET `+clearPriceSet(parsed)+`
			WHERE account_id = ? AND model = ?`, accountID, model)
	})
}

// clearPriceSet 拼一段只含固定列名的 SET：列名来自 priceColumns，值一律是 NULL，
// 因此不拼用户输入。
func clearPriceSet(fields []PriceField) string {
	sets := make([]string, 0, len(fields))
	for _, f := range fields {
		sets = append(sets, priceColumns[f]+" = NULL")
	}
	return strings.Join(sets, ", ")
}

// requireProviderModelTx 确认目录里有这一行，没有就按「先同步或探活」报错。
func requireProviderModelTx(tx *gorm.DB, provider, model string) error {
	var count int64
	if err := tx.Model(&providerModelRow{}).
		Where("provider = ? AND model = ?", provider, model).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("厂商 %q 的目录里没有模型 %q，%w：先 lax model sync --provider %s 或 lax model probe --model %s",
			provider, model, ErrModelNotInCatalog, provider, model)
	}
	return nil
}

// requireAccountModelTx 确认这个账号在这个模型上有可达性行，没有就按「先同步或探活」报错。
func requireAccountModelTx(tx *gorm.DB, accountID int64, account, model string) error {
	var count int64
	if err := tx.Model(&accountModelRow{}).
		Where("account_id = ? AND model = ?", accountID, model).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("账号 %q 还没有模型 %q 的可达性行，%w：先 lax model sync --account %s 或 lax model probe --account %s",
			account, model, ErrAccountModelNotRegistered, account, account)
	}
	return nil
}

// accountIDByNameTx 在事务里把账号名解析成 id：价格写入口收的是命令行给的账号名，
// 表里存的是 id。
func accountIDByNameTx(tx *gorm.DB, account string) (int64, error) {
	var row accountRow
	if err := tx.Where("name = ?", account).Limit(1).Find(&row).Error; err != nil {
		return 0, err
	}
	if row.ID == 0 {
		return 0, fmt.Errorf("账号 %q 不存在：先用 lax account add 建它", account)
	}
	return row.ID, nil
}

// 花费公式里单价的取值链。顺序只有这一处：这一行请求的账号 + 这一行的
// model_used 在 account_models 里的覆盖值 → provider_models 里 (账号的厂商, model_used) 的
// 厂商级标价 → 都没有就是 NULL（这一项不计，整组都没价时花费记 null，见 store.Usage）。
//
// 缓存读写的回退保持原来的语义：自己那一项没填就回退到输入价，而输入价本身也走这条合并链
// （所以账号只覆盖了输入价时，没覆盖的缓存读写按账号的输入价算，不按目录的）。回退链的尽头
// 也是 NULL 而不是 0：输入价缺时缓存那一项就真的没价，不能拿 0 冒充「免费」。
const (
	usagePriceIn         = "COALESCE(am.price_in, pm.price_in)"
	usagePriceOut        = "COALESCE(am.price_out, pm.price_out)"
	usagePriceCacheRead  = "COALESCE(am.price_cache_read, pm.price_cache_read, " + usagePriceIn + ")"
	usagePriceCacheWrite = "COALESCE(am.price_cache_write, pm.price_cache_write, " + usagePriceIn + ")"
)

// usagePriceJoin 给请求行挂上算单价要用的两张表。
//
// account_models 以 (account_id, model) 为键、provider_models 以 (provider, model) 为键，
// 两者的 model 都对着 r.model_used（解析别名之后的真实名字）：这两次 join 因此都是唯一匹配，
// 不会把一行请求乘成多行。厂商从这一行请求的账号取（accounts.provider），不从别处猜。
//
// 两个目录 join 都是 LEFT JOIN：老数据里的模型名可能在目录里没有行，那一行仍要进汇总
// （模型清单是后加的，历史请求不该从 `usage` 里消失），只是它的花费按缺价记 null。
const usagePriceJoin = `LEFT JOIN accounts a ON a.id = r.account_id
	LEFT JOIN account_models am ON am.account_id = r.account_id AND am.model = r.model_used
	LEFT JOIN provider_models pm ON pm.provider = a.provider AND pm.model = r.model_used`
