package cli_test

import (
	"strings"
	"testing"

	"lax/internal/store"
)

// 这个文件钉住 model price 这一组命令面（DESIGN 4 的三行）：厂商级标价写 provider_models，
// 账号覆盖写 account_models，两者互斥；清值与列价都只读模型目录与合并结果。
// 价格不在账号上，所以建账号之后定价是第二步。

// priceRowJSON 是 model price ls --json 的一项。
type priceRowJSON struct {
	Model           string   `json:"model"`
	Provider        string   `json:"provider"`
	PriceIn         *float64 `json:"price_in"`
	PriceOut        *float64 `json:"price_out"`
	PriceCacheRead  *float64 `json:"price_cache_read"`
	PriceCacheWrite *float64 `json:"price_cache_write"`
	Effective       *struct {
		PriceIn         *float64 `json:"price_in"`
		PriceOut        *float64 `json:"price_out"`
		PriceCacheRead  *float64 `json:"price_cache_read"`
		PriceCacheWrite *float64 `json:"price_cache_write"`
	} `json:"effective"`
}

// priceLine 找出以模型名开头的那一行原文：单价单元格里有空格，按空白切字段会把一格切成三格。
func priceLine(t *testing.T, out, model string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, model+" ") {
			return line
		}
	}
	t.Fatalf("表格里没有以 %q 开头的行:\n%s", model, out)
	return ""
}

// mustPriceRows 读 model price ls --json。
func mustPriceRows(t *testing.T, args ...string) (string, []priceRowJSON) {
	t.Helper()
	code, out, stderr := run(t, append([]string{"model", "price", "ls", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("model price ls %v 退出码 = %d，stderr = %s", args, code, stderr)
	}
	var rows []priceRowJSON
	mustJSON(t, out, &rows)
	return out, rows
}

// providerPrice 读目录里 (厂商, 模型) 这一行的四项价，行不存在时直接失败。
func providerPrice(t *testing.T, provider, model string) store.ProviderModel {
	t.Helper()
	st, _ := mustStore(t)
	rows, err := st.ProviderModels(provider)
	if err != nil {
		t.Fatalf("读目录 %s: %v", provider, err)
	}
	for _, m := range rows {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("目录 %s 里没有模型 %s：%+v", provider, model, rows)
	return store.ProviderModel{}
}

// accountPrice 读账号覆盖行上的四项价；行不存在时直接失败。
func accountPrice(t *testing.T, account, model string) store.AccountModel {
	t.Helper()
	st, _ := mustStore(t)
	acc, err := st.GetAccount(account)
	if err != nil {
		t.Fatalf("读账号 %s: %v", account, err)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读 %s 的模型行: %v", account, err)
	}
	for _, m := range rows {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("账号 %s 没有模型 %s 的行：%+v", account, model, rows)
	return store.AccountModel{}
}

func mustFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s 没落库，期望 %v", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %v，期望 %v", name, *got, want)
	}
}

// TestModelPriceSetProviderAndAccount 钉住两个作用域各写哪张表：不给 --account 写
// provider_models 的厂商级标价，给 --account 写 account_models 的覆盖价，两边互不影响。
// 只写命令行上给过的那几项，没给的是空（继承）。
func TestModelPriceSetProviderAndAccount(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 厂商级：模型名只在一家目录里，--provider 可以省略。
	code, out, stderr := run(t, "model", "price", "set", "m1", "--in", "0.6", "--out", "2.2")
	if code != 0 {
		t.Fatalf("model price set 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "厂商 mine 的厂商级标价") || !strings.Contains(out, "m1 输入价 0.60、输出价 2.20") {
		t.Fatalf("确认行应写清作用域、模型与写入的几项价: %s", out)
	}
	if !strings.Contains(out, "下一步：lax model ls") {
		t.Fatalf("确认行之后应给出下一步: %s", out)
	}
	row := providerPrice(t, "mine", "m1")
	mustFloat(t, "目录 m1 的输入价", row.PriceIn, 0.6)
	mustFloat(t, "目录 m1 的输出价", row.PriceOut, 2.2)
	if row.PriceCacheRead != nil || row.PriceCacheWrite != nil {
		t.Fatalf("没给的缓存单价应留空，实际 %v / %v", row.PriceCacheRead, row.PriceCacheWrite)
	}
	// 账号行上的价一个都没动：厂商级标价不是账号价。
	if acc := accountPrice(t, "ds1", "m1"); acc.PriceIn != nil || acc.PriceOut != nil {
		t.Fatalf("厂商级标价不该写进 account_models: %+v", acc)
	}

	// 账号级：--account 写覆盖价，只动这一个账号的这一行。
	code, out, stderr = run(t, "model", "price", "set", "m1", "--account", "ds1", "--out", "9.9", "--json")
	if code != 0 {
		t.Fatalf("账号级 model price set 退出码 = %d，stderr = %s", code, stderr)
	}
	var got map[string]any
	mustJSON(t, out, &got)
	if got["model"] != "m1" || got["account"] != "ds1" || got["provider"] != "mine" {
		t.Fatalf("--json 的作用域 = %+v，期望 model=m1、provider=mine、account=ds1", got)
	}
	if got["price_out"] != 9.9 {
		t.Fatalf("--json 的 price_out = %v，期望 9.9", got["price_out"])
	}
	if v, ok := got["price_in"]; !ok || v != nil {
		t.Fatalf("--json 里没写的 price_in 应为 null，实际 %v（存在=%v）", v, ok)
	}
	acc := accountPrice(t, "ds1", "m1")
	mustFloat(t, "账号覆盖的输出价", acc.PriceOut, 9.9)
	if acc.PriceIn != nil {
		t.Fatalf("没给的账号输入价应继承目录，实际 %v", *acc.PriceIn)
	}
	// 厂商级标价没被账号覆盖改写。
	mustFloat(t, "目录 m1 的输出价", providerPrice(t, "mine", "m1").PriceOut, 2.2)
}

// TestModelPriceSetRejectsUnknownScopes 覆盖三种用法错误：--provider 与 --account 互斥、
// 四项价一个都没给、以及同一模型出现在两家供应商目录里时要求 --provider。
func TestModelPriceSetRejectsUnknownScopes(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add ds1 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine", "--account", "ds1", "--in", "1")
	if code != 2 || out != "" {
		t.Fatalf("互斥的退出码 = %d，stdout = %q，期望 2 与空 stdout", code, out)
	}
	for _, want := range []string{"--provider 与 --account 只能给一个", "用法：lax model price set <模型>"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("互斥的 stderr 应包含 %q: %s", want, stderr)
		}
	}

	code, out, stderr = run(t, "model", "price", "set", "m1", "--provider", "mine")
	if code != 2 || out != "" {
		t.Fatalf("一项价都没给的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "一项都没给") || !strings.Contains(stderr, "model price set m1 --in 0.6 --out 2.2") {
		t.Fatalf("一项价都没给时应给出命令形状: %s", stderr)
	}

	// 第二家供应商的目录里也有 m1：模型名自己确定不了写哪一份标价。
	if code, _, stderr := addAccount(t, "ds2", "other", "http://127.0.0.1:9", "openai-chat", "--key", "sk-2", "--models", "m1"); code != 0 {
		t.Fatalf("account add ds2 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "model", "price", "set", "m1", "--in", "1")
	if code != 2 || out != "" {
		t.Fatalf("同名模型两家的退出码 = %d，stdout = %q，期望 2 与空 stdout", code, out)
	}
	for _, want := range []string{"同时出现在 2 家厂商的目录里", "mine", "other", "--provider"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("歧义文案应包含 %q: %s", want, stderr)
		}
	}
	// 指定 --provider 之后照常写。
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "other", "--in", "3"); code != 0 {
		t.Fatalf("指定 --provider 的退出码 = %d，stderr = %s", code, stderr)
	}
	mustFloat(t, "other 的 m1 输入价", providerPrice(t, "other", "m1").PriceIn, 3)
	if providerPrice(t, "mine", "m1").PriceIn != nil {
		t.Fatal("写 other 的标价却改了 mine 的目录行")
	}
}

// TestModelPriceSetNegativeIsUsageError 钉住负数（退出码 2，保留 store 的原文）与 0 合法。
func TestModelPriceSetNegativeIsUsageError(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine", "--in=-0.5")
	if code != 2 || out != "" {
		t.Fatalf("负数的退出码 = %d，stdout = %q，期望 2 与空 stdout", code, out)
	}
	for _, want := range []string{"模型单价不能是负数", "--in N", "0 是合法值"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("负数的 stderr 应包含 %q: %s", want, stderr)
		}
	}
	if providerPrice(t, "mine", "m1").PriceIn != nil {
		t.Fatal("负数被拒绝后不该落库")
	}

	// 0 是合法值：本地模型就是免费。
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine", "--in", "0"); code != 0 {
		t.Fatalf("0 价的退出码 = %d，stderr = %s", code, stderr)
	}
	mustFloat(t, "0 输入价", providerPrice(t, "mine", "m1").PriceIn, 0)
}

// TestModelPriceSetMissingRows 确认 store 的两个错误原样带出（退出码 1）：模型不在目录里、
// 账号没有这个模型的可达性行。文案里已经写了下一步命令，cli 不另写一份。
func TestModelPriceSetMissingRows(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "price", "set", "ghost", "--provider", "mine", "--in", "1")
	if code != 1 || out != "" {
		t.Fatalf("模型不在目录里的退出码 = %d，stdout = %q，期望 1", code, out)
	}
	for _, want := range []string{"不在目录里", "lax model sync --provider mine"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应包含 %q: %s", want, stderr)
		}
	}

	code, out, stderr = run(t, "model", "price", "set", "ghost", "--account", "ds1", "--in", "1")
	if code != 1 || out != "" {
		t.Fatalf("账号没有该模型行的退出码 = %d，stdout = %q，期望 1", code, out)
	}
	for _, want := range []string{"还没有模型", "lax model sync --account ds1"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应包含 %q: %s", want, stderr)
		}
	}
}

// TestModelPriceUnsetIsIdempotent 钉住逐项确认、重复字段只处理一次、清第二次说明「本来就没有」，
// 以及账号级清掉之后回到厂商级标价（不是免费）。
func TestModelPriceUnsetIsIdempotent(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine",
		"--in", "0.6", "--out", "2.2", "--cache-read", "0.1", "--cache-write", "0.2"); code != 0 {
		t.Fatalf("model price set 退出码 = %d，stderr = %s", code, stderr)
	}

	// 重复给 in 只处理一次；两个字段各输出一行。
	code, out, stderr := run(t, "model", "price", "unset", "m1", "--provider", "mine", "in", "cache-read", "in")
	if code != 0 {
		t.Fatalf("model price unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Count(out, "已清掉输入价") != 1 {
		t.Fatalf("重复字段只该处理一次: %s", out)
	}
	if !strings.Contains(out, "已清掉缓存读价") || !strings.Contains(out, "现有 2 项标价") {
		t.Fatalf("逐项确认与剩余项数都要在: %s", out)
	}
	row := providerPrice(t, "mine", "m1")
	if row.PriceIn != nil || row.PriceCacheRead != nil {
		t.Fatalf("要清的项没清掉: %+v", row)
	}
	mustFloat(t, "没清的输出价", row.PriceOut, 2.2)

	// 再清一次同样的字段：幂等，说明「本来就没有」，退出码仍是 0。
	code, out, stderr = run(t, "model", "price", "unset", "m1", "--provider", "mine", "in", "cache-read")
	if code != 0 {
		t.Fatalf("第二次 model price unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "m1的输入价本来就没有") || !strings.Contains(out, "m1的缓存读价本来就没有") {
		t.Fatalf("清第二次应说明本来就没有: %s", out)
	}
	if !strings.Contains(out, "现有 2 项标价") {
		t.Fatalf("剩余项数不变: %s", out)
	}

	// --json 把真清掉的与本来就没有的分开报。
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--account", "ds1", "--out", "9.9"); code != 0 {
		t.Fatalf("账号覆盖 set 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "model", "price", "unset", "m1", "--account", "ds1", "out", "--json")
	if code != 0 {
		t.Fatalf("账号级 unset 退出码 = %d，stderr = %s", code, stderr)
	}
	var got struct {
		Model     string   `json:"model"`
		Provider  string   `json:"provider"`
		Account   string   `json:"account"`
		Cleared   []string `json:"cleared"`
		Unchanged []string `json:"unchanged"`
		Remaining int      `json:"remaining"`
	}
	mustJSON(t, out, &got)
	if got.Account != "ds1" || got.Provider != "mine" || len(got.Cleared) != 1 || got.Cleared[0] != "out" || len(got.Unchanged) != 0 {
		t.Fatalf("unset --json = %+v，期望账号 ds1 清掉 out", got)
	}
	if got.Remaining != 0 {
		t.Fatalf("账号覆盖清空后 remaining = %d，期望 0", got.Remaining)
	}
	// 账号级清掉 = 回到厂商级标价：目录的输出价还是 2.2。
	if acc := accountPrice(t, "ds1", "m1"); acc.PriceOut != nil {
		t.Fatalf("账号覆盖没清掉: %v", *acc.PriceOut)
	}
	mustFloat(t, "厂商级输出价", providerPrice(t, "mine", "m1").PriceOut, 2.2)
}

// TestModelPriceUnsetRejectsBadFields 覆盖字段写错的三类：未知名字、大小写不符、带 -- 前缀。
// 三者都是用法错误（退出码 2），并列出可用的四个字段。
func TestModelPriceUnsetRejectsBadFields(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"m1", "--provider", "mine", "cacheRead"}, "未知的价格字段"},
		{[]string{"m1", "--provider", "mine", "In"}, "未知的价格字段"},
		{[]string{"m1", "--provider", "mine", "--in"}, "不带 -- 前缀"},
		{[]string{"m1", "--provider", "mine"}, "至少一个字段"},
	}
	for _, c := range cases {
		code, out, stderr := run(t, append([]string{"model", "price", "unset"}, c.args...)...)
		if code != 2 || out != "" {
			t.Fatalf("%v 的退出码 = %d，stdout = %q，期望 2 与空 stdout", c.args, code, out)
		}
		for _, want := range []string{c.want, "in、out、cache-read、cache-write", "用法：lax model price unset <模型>"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%v 的 stderr 应包含 %q: %s", c.args, want, stderr)
			}
		}
	}

	// --provider 与 --account 在 unset 上同样互斥。
	code, out, stderr := run(t, "model", "price", "unset", "m1", "--provider", "mine", "--account", "ds1", "in")
	if code != 2 || out != "" {
		t.Fatalf("unset 互斥的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "--provider 与 --account 只能给一个") {
		t.Fatalf("unset 互斥的文案: %s", stderr)
	}
}

// TestModelPriceLsTextAndJSON 钉住列表的两种输出：不给模型列所有有价的目录行（按模型名升序），
// 五列里缓存那格空着写 -、两项都空写「回退输入价」；给 --account 时并排显示有效价。
func TestModelPriceLsTextAndJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1,m2,m3"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	steps := [][]string{
		{"model", "price", "set", "m1", "--provider", "mine", "--in", "0.6", "--out", "2.2"},
		{"model", "price", "set", "m2", "--provider", "mine", "--in", "1", "--cache-read", "0.1"},
		{"model", "price", "set", "m1", "--account", "ds1", "--out", "9.9"},
	}
	for _, args := range steps {
		if code, _, stderr := run(t, args...); code != 0 {
			t.Fatalf("%v 退出码 = %d，stderr = %s", args, code, stderr)
		}
	}

	code, out, stderr := run(t, "model", "price", "ls")
	if code != 0 {
		t.Fatalf("model price ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"模型", "供应商", "输入价", "输出价", "缓存读 / 缓存写"} {
		if !strings.Contains(out, want) {
			t.Fatalf("表头缺 %q:\n%s", want, out)
		}
	}
	m1 := priceLine(t, out, "m1")
	for _, want := range []string{"mine", "0.60", "2.20", "回退输入价"} {
		if !strings.Contains(m1, want) {
			t.Fatalf("m1 行 = %q，应包含 %q", m1, want)
		}
	}
	m2 := priceLine(t, out, "m2")
	for _, want := range []string{"mine", "1.00", "0.10 / -"} {
		if !strings.Contains(m2, want) {
			t.Fatalf("m2 行 = %q，应包含 %q", m2, want)
		}
	}
	// 只有输入价时输出价那一格写 -（缓存写也一样）。
	if fields := strings.Fields(m2); len(fields) < 5 || fields[2] != "1.00" || fields[3] != "-" {
		t.Fatalf("m2 的输出价应写 -: %q", m2)
	}
	// m3 一项价都没有：不列出来。
	if strings.Contains(out, "m3") {
		t.Fatalf("没有价格的目录行不该出现在列表里:\n%s", out)
	}

	raw, rows := mustPriceRows(t)
	if len(rows) != 2 || rows[0].Model != "m1" || rows[1].Model != "m2" {
		t.Fatalf("--json 行 = %+v，期望按模型名升序的 m1、m2", rows)
	}
	mustFloat(t, "--json m1 price_in", rows[0].PriceIn, 0.6)
	if rows[1].PriceOut != nil || rows[1].PriceCacheWrite != nil {
		t.Fatalf("m2 的没写的项应为 null: %+v", rows[1])
	}
	mustFloat(t, "--json m2 price_cache_read", rows[1].PriceCacheRead, 0.1)
	if strings.Contains(raw, `"effective"`) {
		t.Fatalf("没给 --account 时不该有 effective 字段: %s", raw)
	}

	// 给 --account：目录价与有效价并排。
	code, out, stderr = run(t, "model", "price", "ls", "m1", "--account", "ds1")
	if code != 0 {
		t.Fatalf("带 --account 的 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "输出价（目录 | 有效）") {
		t.Fatalf("带 --account 时表头应标明两列并排:\n%s", out)
	}
	row := priceLine(t, out, "m1")
	for _, want := range []string{"0.60 | 0.60", "2.20 | 9.90"} {
		if !strings.Contains(row, want) {
			t.Fatalf("m1 行 = %q，应包含并排的 %q", row, want)
		}
	}
	_, rows = mustPriceRows(t, "m1", "--account", "ds1")
	if len(rows) != 1 || rows[0].Effective == nil {
		t.Fatalf("--account 时每项都要带 effective: %+v", rows)
	}
	mustFloat(t, "effective price_out", rows[0].Effective.PriceOut, 9.9)
	mustFloat(t, "目录 price_out", rows[0].PriceOut, 2.2)

	// 模型不在目录里：报错并指向 model ls，不合成一张空表。
	code, out, stderr = run(t, "model", "price", "ls", "ghost")
	if code != 1 || out != "" {
		t.Fatalf("不在目录里的退出码 = %d，stdout = %q，期望 1", code, out)
	}
	if !strings.Contains(stderr, "lax model ls") {
		t.Fatalf("不在目录里时应指向 model ls: %s", stderr)
	}

	// 一项价都没有时给提示与一条 model price set 示例。
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "model", "price", "ls")
	if code != 0 {
		t.Fatalf("空列表的退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "model price set glm-4.6 --in 0.6 --out 2.2") {
		t.Fatalf("空列表应给一条示例: %s", out)
	}
}

// TestModelLsPricesComeFromMergedCatalog 钉住 model ls 的价格列来自合并值
// （账号覆盖 → 厂商级标价，见 DESIGN 5.13）：一个模型只有厂商级标价、另一个被账号覆盖，
// 两行都要显示对，文本与 --json 同源。
func TestModelLsPricesComeFromMergedCatalog(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine", "--in", "0.6", "--out", "2.2"); code != 0 {
		t.Fatalf("m1 定价退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m2", "--provider", "mine", "--in", "1"); code != 0 {
		t.Fatalf("m2 定价退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--account", "ds1", "--out", "9.9"); code != 0 {
		t.Fatalf("m1 账号覆盖退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "ls")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "0.60 / 9.90") {
		t.Fatalf("m1 的价格列应是账号覆盖后的合并值 0.60 / 9.90:\n%s", out)
	}
	if !strings.Contains(out, "1.00 / -") {
		t.Fatalf("m2 只有厂商级输入价，输出价应写 -:\n%s", out)
	}

	code, out, stderr = run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Model    string   `json:"model"`
		PriceIn  *float64 `json:"price_in"`
		PriceOut *float64 `json:"price_out"`
	}
	mustJSON(t, out, &rows)
	byModel := map[string]struct {
		PriceIn  *float64
		PriceOut *float64
	}{}
	for _, r := range rows {
		byModel[r.Model] = struct {
			PriceIn  *float64
			PriceOut *float64
		}{r.PriceIn, r.PriceOut}
	}
	m1 := byModel["m1"]
	if m1.PriceIn == nil || *m1.PriceIn != 0.6 || m1.PriceOut == nil || *m1.PriceOut != 9.9 {
		t.Fatalf("--json 的 m1 价格 = %+v，期望 0.6 / 9.9（合并之后的值）", m1)
	}
	m2 := byModel["m2"]
	if m2.PriceIn == nil || *m2.PriceIn != 1 || m2.PriceOut != nil {
		t.Fatalf("--json 的 m2 价格 = %+v，期望 1 / null", m2)
	}
}

// TestModelPriceHelp 钉住 model price --help 的形状：三条用法、一个示例与两段说明。
// 光列子命令说不清价格挂在谁身上（DESIGN 4）。
func TestModelPriceHelp(t *testing.T) {
	sandbox(t)

	code, out, stderr := run(t, "model", "price", "--help")
	if code != 0 {
		t.Fatalf("model price --help 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"lax model price set <模型>",
		"lax model price unset <模型>",
		"lax model price ls [<模型>]",
		"lax model price set glm-4.6 --in 0.6 --out 2.2",
		"价格属于模型，不属于账号",
		"账号级价格只在套餐不同时覆盖",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("model price --help 应包含 %q:\n%s", want, out)
		}
	}

	// model --help 的子命令清单里要有 price。
	code, out, stderr = run(t, "model", "--help")
	if code != 0 {
		t.Fatalf("model --help 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "price") || !strings.Contains(out, "单价") {
		t.Fatalf("model --help 应列出 price:\n%s", out)
	}
}
