package store_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"lax/internal/secret"
	"lax/internal/store"
)

// 这一组用例钉住单价的归属与取值链：价格属于「供应商 × 模型」，
// 账号只在套餐不同时覆盖，`usage` 按「这一行的账号 + 这一行的模型」现算。

// catalogAccountForPrice 建一家厂商、一个账号，并把 m1 落进目录与该账号的可达性行，
// 但不给任何价格：价格由每条用例自己走新写入口写。
func catalogAccountForPrice(t *testing.T, st *store.Store, account string) store.Account {
	t.Helper()
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
		Model: "m1", Source: store.SourceSync,
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	acc := newCatalogAccount(t, st, account, "acme")
	if _, err := st.ReplaceModels(acc.ID, []string{"m1"}); err != nil {
		t.Fatalf("标可达: %v", err)
	}
	return acc
}

// TestSetProviderModelPricesReachesEffectiveModels 钉住厂商级标价的写入与合并：
// 只写给过的那几项，来源等级不动，updated_at 刷新，读侧（EffectiveModels）看到的就是这份价。
func TestSetProviderModelPricesReachesEffectiveModels(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")

	// 把 updated_at 拨回过去，好让这次写入的刷新可被观察到。
	if _, err := st.DB().Exec(`UPDATE provider_models SET updated_at = 1 WHERE provider = 'acme' AND model = 'm1'`); err != nil {
		t.Fatalf("改 updated_at: %v", err)
	}

	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In:  floatPtr(1.5),
		Out: floatPtr(6),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	m := oneModel(t, st, "acme", "m1")
	if m.PriceIn == nil || *m.PriceIn != 1.5 || m.PriceOut == nil || *m.PriceOut != 6 {
		t.Fatalf("目录价 = in %v / out %v，期望 1.5 / 6", m.PriceIn, m.PriceOut)
	}
	if m.PriceCacheRead != nil || m.PriceCacheWrite != nil {
		t.Fatalf("没给的缓存价应保持为空: %+v", m)
	}
	if m.Source != store.SourceSync {
		t.Fatalf("来源 = %q，改价格不该动 source（期望 sync）", m.Source)
	}
	if m.UpdatedAt <= 1 {
		t.Fatalf("updated_at = %d，写价格应刷新它", m.UpdatedAt)
	}

	// 第二次只给缓存价：上一次的输入输出价留着（nil 表示这一项不动，不是清掉）。
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		CacheRead:  floatPtr(0.5),
		CacheWrite: floatPtr(0),
	}); err != nil {
		t.Fatalf("写缓存价: %v", err)
	}
	m = oneModel(t, st, "acme", "m1")
	if m.PriceIn == nil || *m.PriceIn != 1.5 || m.PriceOut == nil || *m.PriceOut != 6 {
		t.Fatalf("只写缓存价时输入输出价应保留: %+v", m)
	}
	if m.PriceCacheRead == nil || *m.PriceCacheRead != 0.5 {
		t.Fatalf("缓存读价 = %v，期望 0.5", m.PriceCacheRead)
	}
	if m.PriceCacheWrite == nil || *m.PriceCacheWrite != 0 {
		t.Fatalf("缓存写价 = %v，期望 0（0 是合法值）", m.PriceCacheWrite)
	}

	info := oneInfo(t, st, acc.ID, "m1")
	if info.PriceIn == nil || *info.PriceIn != 1.5 || info.PriceOut == nil || *info.PriceOut != 6 {
		t.Fatalf("EffectiveModels 读到的价 = in %v / out %v，期望厂商级 1.5 / 6", info.PriceIn, info.PriceOut)
	}
	if info.PriceCacheRead == nil || *info.PriceCacheRead != 0.5 {
		t.Fatalf("EffectiveModels 的缓存读价 = %v，期望 0.5", info.PriceCacheRead)
	}
}

// TestAccountModelPricesOverrideCatalog 钉住「账号覆盖压过厂商级」，以及清掉覆盖回到厂商级。
func TestAccountModelPricesOverrideCatalog(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In: floatPtr(1), Out: floatPtr(2), CacheRead: floatPtr(0.25),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}

	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{In: floatPtr(9)}); err != nil {
		t.Fatalf("写账号覆盖价: %v", err)
	}
	info := oneInfo(t, st, acc.ID, "m1")
	if info.PriceIn == nil || *info.PriceIn != 9 {
		t.Fatalf("输入价 = %v，期望账号覆盖的 9", info.PriceIn)
	}
	// 没覆盖的那几项仍取厂商级，不因为写了覆盖价就清掉。
	if info.PriceOut == nil || *info.PriceOut != 2 {
		t.Fatalf("输出价 = %v，期望厂商级的 2", info.PriceOut)
	}
	if info.PriceCacheRead == nil || *info.PriceCacheRead != 0.25 {
		t.Fatalf("缓存读价 = %v，期望厂商级的 0.25", info.PriceCacheRead)
	}

	// 清掉覆盖 → 回到厂商级（不是「免费」）。清一项，其余覆盖值不受影响。
	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{Out: floatPtr(7)}); err != nil {
		t.Fatalf("再写一项覆盖价: %v", err)
	}
	if err := st.ClearAccountModelPrices("a1", "m1", []string{"in"}); err != nil {
		t.Fatalf("清账号覆盖价: %v", err)
	}
	info = oneInfo(t, st, acc.ID, "m1")
	if info.PriceIn == nil || *info.PriceIn != 1 {
		t.Fatalf("清掉覆盖之后输入价应回到厂商级的 1，实际 %v", info.PriceIn)
	}
	if info.PriceOut == nil || *info.PriceOut != 7 {
		t.Fatalf("清另外一项不该动输出价（期望 7），实际 %v", info.PriceOut)
	}

	// 厂商级清掉 → 这一项未知（nil），不是 0。
	if err := st.ClearProviderModelPrices("acme", "m1", []string{"cache-read"}); err != nil {
		t.Fatalf("清厂商级价: %v", err)
	}
	info = oneInfo(t, st, acc.ID, "m1")
	if info.PriceCacheRead != nil {
		t.Fatalf("清掉的厂商级缓存读价应为 nil，实际 %v", info.PriceCacheRead)
	}
}

// TestModelPricesRejectNegative 钉住「负数报错、0 合法」，两个写入口同一条规则。
func TestModelPricesRejectNegative(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")

	cases := []struct {
		name  string
		write func() error
	}{
		{"厂商级输入价", func() error {
			return st.SetProviderModelPrices("acme", "m1", store.ModelPrices{In: floatPtr(-0.1)})
		}},
		{"厂商级缓存写价", func() error {
			return st.SetProviderModelPrices("acme", "m1", store.ModelPrices{CacheWrite: floatPtr(-1)})
		}},
		{"账号覆盖输出价", func() error {
			return st.SetAccountModelPrices("a1", "m1", store.ModelPrices{Out: floatPtr(-2)})
		}},
	}
	for _, c := range cases {
		err := c.write()
		if !errors.Is(err, store.ErrNegativePrice) {
			t.Fatalf("%s：错误 = %v，期望 ErrNegativePrice", c.name, err)
		}
	}
	// 被拒的写入一个字段都不该落库。
	if m := oneModel(t, st, "acme", "m1"); m.PriceIn != nil || m.PriceCacheWrite != nil {
		t.Fatalf("被拒的写入改了目录: %+v", m)
	}
	if info := oneInfo(t, st, 1, "m1"); info.PriceOut != nil {
		t.Fatalf("被拒的写入改了账号覆盖: %+v", info)
	}

	// 0 是合法值：免费或打包不该被当成「没填」。
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{In: floatPtr(0)}); err != nil {
		t.Fatalf("写 0 价应成功: %v", err)
	}
	m := oneModel(t, st, "acme", "m1")
	if m.PriceIn == nil || *m.PriceIn != 0 {
		t.Fatalf("0 价落库之后读到 %v，期望非 nil 的 0", m.PriceIn)
	}
}

// TestModelPriceWritesRequireCatalogRow 钉住「不顺手插一行」：两个写入口都要先有这个模型的行，
// 错误文案指向 model sync / model probe。
func TestModelPriceWritesRequireCatalogRow(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")

	// 目录里没有这个模型。
	err := st.SetProviderModelPrices("acme", "nope", store.ModelPrices{In: floatPtr(1)})
	if !errors.Is(err, store.ErrModelNotInCatalog) {
		t.Fatalf("厂商级写入的错误 = %v，期望 ErrModelNotInCatalog", err)
	}
	if !mentionsSyncOrProbe(err) {
		t.Fatalf("厂商级写入的错误没提示先同步或探活: %v", err)
	}
	if models, err := st.ProviderModels("acme"); err != nil || len(models) != 1 {
		t.Fatalf("被拒的写入不该插行：目录 %+v（err %v）", models, err)
	}

	// 账号没有这个模型的可达性行。
	err = st.SetAccountModelPrices("a1", "nope", store.ModelPrices{In: floatPtr(1)})
	if !errors.Is(err, store.ErrAccountModelNotRegistered) {
		t.Fatalf("账号覆盖写入的错误 = %v，期望 ErrAccountModelNotRegistered", err)
	}
	if !mentionsSyncOrProbe(err) {
		t.Fatalf("账号覆盖写入的错误没提示先同步或探活: %v", err)
	}
	rows, err := st.AccountModels(1)
	if err != nil {
		t.Fatalf("读可达性: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("被拒的写入不该插可达性行：%+v", rows)
	}

	// 清价格同样先确认目录行在。
	if err := st.ClearProviderModelPrices("acme", "nope", []string{"in"}); !errors.Is(err, store.ErrModelNotInCatalog) {
		t.Fatalf("清厂商级价的错误 = %v，期望 ErrModelNotInCatalog", err)
	}
	// 账号没有那个模型的行 = 没有覆盖值可清，幂等成功。
	if err := st.ClearAccountModelPrices("a1", "nope", []string{"in"}); err != nil {
		t.Fatalf("清不存在的账号覆盖价应成功（幂等），实际 %v", err)
	}
	// 账号名写错仍要报错：那不是「没有覆盖值」。
	if err := st.SetAccountModelPrices("nobody", "m1", store.ModelPrices{In: floatPtr(1)}); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("账号不存在时的错误 = %v，期望提示账号不存在", err)
	}
}

func mentionsSyncOrProbe(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "model sync") && strings.Contains(msg, "model probe")
}

// TestClearModelPricesValidatesFields 钉住字段名解析：未知字段、带 -- 前缀、大小写不符都报错
// 并列出可用字段；本来为空的字段清掉不报错（幂等）；重复的字段只处理一次。
func TestClearModelPricesValidatesFields(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{In: floatPtr(1)}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}

	bad := []struct {
		name   string
		fields []string
	}{
		{"未知字段", []string{"input"}},
		{"大小写不符", []string{"In"}},
		{"带 -- 前缀", []string{"--in"}},
		{"混了一个错的", []string{"in", "cache_read"}},
	}
	for _, c := range bad {
		err := st.ClearProviderModelPrices("acme", "m1", c.fields)
		if err == nil {
			t.Fatalf("%s：应报错", c.name)
		}
		for _, want := range []string{"in", "out", "cache-read", "cache-write"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s：错误文案没列出可用字段 %q: %v", c.name, want, err)
			}
		}
	}
	// 报错时不该清掉任何东西。
	if m := oneModel(t, st, "acme", "m1"); m.PriceIn == nil || *m.PriceIn != 1 {
		t.Fatalf("报错的清价格改了目录: %+v", m)
	}
	// 一个字段都不给也是用法错误。
	if err := st.ClearProviderModelPrices("acme", "m1", nil); err == nil {
		t.Fatalf("一个字段都不给应报错")
	}

	// 幂等：清一个本来为空的字段不报错，清两次也不报错；重复字段只算一次。
	if err := st.ClearAccountModelPrices("a1", "m1", []string{"out", "out"}); err != nil {
		t.Fatalf("清本来为空的覆盖价应成功，实际 %v", err)
	}
	if err := st.ClearProviderModelPrices("acme", "m1", []string{"in", "in"}); err != nil {
		t.Fatalf("清两次应成功，实际 %v", err)
	}
	if m := oneModel(t, st, "acme", "m1"); m.PriceIn != nil {
		t.Fatalf("清两次之后输入价应为 nil，实际 %v", *m.PriceIn)
	}
}

// TestParsePriceField 钉住字段名的白名单只有一份：命令行写法与 store 常量一一对应。
func TestParsePriceField(t *testing.T) {
	// PriceFieldNames 的顺序是固定的，帮助文本与错误文案都按它排。
	want := []string{"in", "out", "cache-read", "cache-write"}
	got := store.PriceFieldNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("字段名 = %v，期望 %v", got, want)
	}
	for _, name := range want {
		f, err := store.ParsePriceField(name)
		if err != nil || string(f) != name {
			t.Fatalf("解析 %q = %q（err %v）", name, f, err)
		}
	}
	// 首尾空白是对用户手误的容忍，不算改字段名。
	if f, err := store.ParsePriceField(" in "); err != nil || f != store.FieldPriceIn {
		t.Fatalf("解析带空白的 in = %q（err %v）", f, err)
	}
	if _, err := store.ParsePriceField("cache_read"); err == nil {
		t.Fatalf("下划线写法应报错（可用字段是 cache-read）")
	}
}

// insertUsageRow 写一条请求记录：token 数固定，用例只关心这一次的单价怎么算。
func insertUsageRow(t *testing.T, st *store.Store, accountID int64, model string) {
	t.Helper()
	rec := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: model, ModelUsed: model, AccountID: accountID,
		HTTPStatus: 200, OK: true,
		InTokens: 3_000_000, InTokensUncached: 1_000_000, OutTokens: 1_000_000,
		CacheRead: 2_000_000, CacheWrite: 4_000_000,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}
}

// oneUsage 取汇总里的一行，并确认没有别的行：被漏掉的那一行不该从汇总里消失，
// 多出来的行说明 join 把一行请求乘成了多行。
func oneUsage(t *testing.T, st *store.Store, dimension, key string) store.UsageRow {
	t.Helper()
	rows, err := st.Usage(dimension, time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("汇总用量: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s 维度汇总出 %d 行，期望 1 行: %+v", dimension, len(rows), rows)
	}
	if rows[0].Key != key {
		t.Fatalf("汇总的键 = %q，期望 %q", rows[0].Key, key)
	}
	if rows[0].Requests != 1 {
		t.Fatalf("请求数 = %d，期望 1", rows[0].Requests)
	}
	return rows[0]
}

// wantCost 断言花费，容差 1e-9：几个小数相乘之后不该指望浮点精确相等。
// nil（未定价）不算通过：用这个辅助的用例都是「这一组有价」的场景。
func wantCost(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("花费 = nil（未定价），期望 %v", want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("花费 = %v，期望 %v（容差 1e-9）", *got, want)
	}
}

// costOf 取一行的花费，nil 直接失败：与 wantCost 一样，给「期望有金额」的断言用。
func costOf(t *testing.T, r store.UsageRow) float64 {
	t.Helper()
	if r.Cost == nil {
		t.Fatalf("花费 = nil（未定价），这个用例期望一个具体金额: %+v", r)
	}
	return *r.Cost
}

// TestUsageCostOnlyProviderPrices 钉住「只有厂商级标价」时的花费：
// 1M 未命中输入 × 1 + 2M 缓存读 × 0.25 + 4M 缓存写 × 0.75 + 1M 输出 × 2 = 6.5。
func TestUsageCostOnlyProviderPrices(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In: floatPtr(1), Out: floatPtr(2), CacheRead: floatPtr(0.25), CacheWrite: floatPtr(0.75),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	insertUsageRow(t, st, acc.ID, "m1")

	wantCost(t, oneUsage(t, st, "model", "m1").Cost, 6.5)
	wantCost(t, oneUsage(t, st, "account", "a1").Cost, 6.5)
}

// TestUsageCostAccountOverrideWins 钉住「账号覆盖压过厂商级」时的花费：
// 覆盖输入价 3、输出价 1，缓存读写仍取厂商级 0.25 / 0.75
// → 1M × 3 + 2M × 0.25 + 4M × 0.75 + 1M × 1 = 7.5。
func TestUsageCostAccountOverrideWins(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In: floatPtr(1), Out: floatPtr(2), CacheRead: floatPtr(0.25), CacheWrite: floatPtr(0.75),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{
		In: floatPtr(3), Out: floatPtr(1),
	}); err != nil {
		t.Fatalf("写账号覆盖价: %v", err)
	}
	insertUsageRow(t, st, acc.ID, "m1")

	wantCost(t, oneUsage(t, st, "model", "m1").Cost, 7.5)

	// 清掉覆盖之后回到厂商级：6.5，与只有厂商级价时一致。
	if err := st.ClearAccountModelPrices("a1", "m1", []string{"in", "out"}); err != nil {
		t.Fatalf("清账号覆盖价: %v", err)
	}
	wantCost(t, oneUsage(t, st, "model", "m1").Cost, 6.5)
}

// TestUsageCostWithoutAnyPriceStayNull 钉住「缺价记 null」：目录与账号都没有这个模型的价、
// 账号行上的旧价又已经删了，这一行仍然进汇总（请求确实发生过），但花费是 nil——不知道多少钱，
// 不是 0（0 是确实不花钱）。
func TestUsageCostWithoutAnyPriceStayNull(t *testing.T) {
	st := openTemp(t)
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	acc := newCatalogAccount(t, st, "a1", "acme")
	insertUsageRow(t, st, acc.ID, "old-model")

	row := oneUsage(t, st, "model", "old-model")
	if row.Cost != nil {
		t.Fatalf("没有任何单价的模型花费 = %v，期望 nil（未定价）", *row.Cost)
	}
	// 用量不受缺价影响。
	if row.InTokens != 3_000_000 || row.OutTokens != 1_000_000 {
		t.Fatalf("缺价把用量一起抹掉了: %+v", row)
	}
}

// TestUsageCostPartialPriceStillCounts 钉住「部分缺项照算」：只有输出价时输入与缓存三项没有价，
// 不计；输出那一项照算，整组不是 null。1M 输出 × 4 = 4。
func TestUsageCostPartialPriceStillCounts(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{Out: floatPtr(4)}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	insertUsageRow(t, st, acc.ID, "m1")

	wantCost(t, oneUsage(t, st, "model", "m1").Cost, 4)
}

// TestUsageCostGroupNullOnlyWhenEveryRequestUnpriced 钉住聚合的口径：一组里只要有一行请求算出过钱，
// 整组就不是 null，缺价的行按「缺项不计」处理，不当成 0 元把合计拉低成别的数。
// 两行各 1M 输出：m1 有输出价 4 → 4；m2 一个价都没有 → 不计，合计仍是 4。
func TestUsageCostGroupNullOnlyWhenEveryRequestUnpriced(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if _, err := st.ReplaceModels(acc.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("标可达: %v", err)
	}
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
		Model: "m2", Source: store.SourceSync,
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{Out: floatPtr(4)}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	for _, model := range []string{"m1", "m2"} {
		rec := store.RequestRecord{
			TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
			ModelRequested: model, ModelUsed: model, AccountID: acc.ID,
			HTTPStatus: 200, OK: true, OutTokens: 1_000_000,
		}
		if err := st.InsertRequest(&rec); err != nil {
			t.Fatalf("写请求记录 %s: %v", model, err)
		}
	}

	rows, err := st.Usage("account", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 {
		t.Fatalf("按账号汇总 = %+v，期望一行两条请求", rows)
	}
	wantCost(t, rows[0].Cost, 4)
	// m2 自己那一组一项价都没有 → null，与它所在的账号组不同。
	byModel, err := st.Usage("model", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("按模型汇总: %v", err)
	}
	found := false
	for _, r := range byModel {
		if r.Key != "m2" {
			continue
		}
		found = true
		if r.Cost != nil {
			t.Fatalf("全部缺价的模型组花费 = %v，期望 nil", *r.Cost)
		}
	}
	if !found {
		t.Fatalf("按模型汇总里没有 m2: %+v", byModel)
	}
}

// TestUsageCostCachePriceFallsBackToMergedInputPrice 钉住缓存价的回退：
// 缓存读写的「未填回退输入价」里的输入价本身走合并链。账号只覆盖输入价 5，厂商级是 1，
// 缓存读写两边都没填 → 按账号的 5 算，不按厂商级的 1。
// 1M × 5 + 2M × 5 + 4M × 5 + 1M × 2 = 5 + 10 + 20 + 2 = 37。
func TestUsageCostCachePriceFallsBackToMergedInputPrice(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In: floatPtr(1), Out: floatPtr(2),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{In: floatPtr(5)}); err != nil {
		t.Fatalf("写账号覆盖价: %v", err)
	}
	insertUsageRow(t, st, acc.ID, "m1")

	wantCost(t, oneUsage(t, st, "model", "m1").Cost, 37)
}

// TestPriceWriteErrorsCarryNoCredential 钉住凭据不外溢：单价写入口只认账号名与模型名，
// 报错与读回的价格输出里都不该出现 key 明文。
func TestPriceWriteErrorsCarryNoCredential(t *testing.T) {
	path := t.TempDir() + "/sb.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer st.Close()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	const plaintext = "sk-price-abcdef0123456789"
	cipher, err := secret.Encrypt(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	acc := store.Account{Name: "a1", Provider: "acme", KeyCipher: cipher, Models: []string{"m1"}, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{Model: "m1", Source: store.SourceSync}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}

	// 三条失败路径的文案：负数、目录里没有、账号没有这一行。
	var msgs []string
	for _, err := range []error{
		st.SetProviderModelPrices("acme", "m1", store.ModelPrices{In: floatPtr(-1)}),
		st.SetProviderModelPrices("acme", "nope", store.ModelPrices{In: floatPtr(1)}),
		st.SetAccountModelPrices("a1", "nope", store.ModelPrices{In: floatPtr(1)}),
		st.ClearProviderModelPrices("acme", "m1", []string{"nope"}),
	} {
		if err == nil {
			t.Fatalf("这一步应该报错")
		}
		msgs = append(msgs, err.Error())
	}
	for _, msg := range msgs {
		if strings.Contains(msg, plaintext) {
			t.Fatalf("错误文案里出现了 key 明文: %s", msg)
		}
	}

	// 写完价格之后，读回的结构里也只有价格。
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{In: floatPtr(1)}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{Out: floatPtr(2)}); err != nil {
		t.Fatalf("写账号覆盖价: %v", err)
	}
	models, err := st.ProviderModels("acme")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达性: %v", err)
	}
	infos, err := st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("读有效模型: %v", err)
	}
	for name, v := range map[string]any{"目录": models, "可达性": rows, "有效模型": infos} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化%s: %v", name, err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("%s的序列化输出里出现了 key 明文: %s", name, b)
		}
	}
}
