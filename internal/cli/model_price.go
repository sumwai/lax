package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"lax/internal/store"
)

// model price 三条子命令只做命令面：参数个数、互斥与空值在这里判（退出码 2 并打用法行），
// 单价归属、负数与「行不在目录里」由 store 判（见 internal/store/model_price.go），
// 错误文案原样带出（退出码 1），cli 不自己写第二份规则：两份规则会在同一条命令上给出两种结论。
//
// 价格属于「供应商 × 模型」，不在账号上：账号只在套餐不同时覆盖。
// 因此这里有两个作用域——厂商级标价写 provider_models，账号覆盖写 account_models。

// 这一组的 --help 不走 printGroupHelp：光列子命令说不清价格挂在谁身上。
// 三条用法、一个示例与两段说明都写在 printModelPriceHelp 里。

const (
	modelPriceSetUsage = "用法：lax model price set <模型> [--provider 供应商] [--account 账号] " +
		"[--in N] [--out N] [--cache-read N] [--cache-write N] [--tts-char N] [--audio-second N]"
	modelPriceUnsetUsage = "用法：lax model price unset <模型> [--provider 供应商] [--account 账号] " +
		"<字段> [<字段>...]"
	modelPriceLsUsage = "用法：lax model price ls [<模型>] [--provider 供应商] [--account 账号]"
	modelPriceUsage   = "用法：\n" +
		"  lax model price set <模型> [--provider 供应商] [--account 账号] [--in N] [--out N] [--cache-read N] [--cache-write N] [--tts-char N] [--audio-second N]\n" +
		"  lax model price unset <模型> [--provider 供应商] [--account 账号] <字段> [<字段>...]\n" +
		"  lax model price ls [<模型>] [--provider 供应商] [--account 账号]"
)

// printModelPriceHelp 是 `model price --help` 的输出：三条用法、一个示例与两段说明。
// 光列子命令说不清价格挂在谁身上，所以不走 printGroupHelp。
func printModelPriceHelp(w io.Writer) {
	var b strings.Builder
	b.WriteString(appName + " model price — 模型单价（每百万 token），挂在「供应商 × 模型」上\n\n")
	b.WriteString(modelPriceUsage + "\n\n")
	b.WriteString("价格属于模型，不属于账号：同一家上游的同一个模型只有一份标价，model sync 或 model probe\n")
	b.WriteString("把模型写进目录之后用 model price set 定它的价，usage 与 model ls 都读这一份。\n\n")
	b.WriteString("账号级价格只在套餐不同时覆盖：给 --account 写的价只作用于那一个账号，不影响同厂商的\n")
	b.WriteString("其它账号，也不改厂商级标价。清掉账号覆盖（model price unset --account）表示回到厂商级\n")
	b.WriteString("标价，不是「免费」；缓存读写没定价时按输入价算。\n\n")
	b.WriteString("示例：\n")
	b.WriteString("  lax model price set glm-4.6 --in 0.6 --out 2.2\n\n")
	b.WriteString("查看某个子命令的参数：lax model price <子命令> --help\n")
	_, _ = io.WriteString(w, b.String())
}

// cmdModelPrice 是 model price 这一组子命令的入口。
func cmdModelPrice(e *Env, args []string) error {
	if isHelpRequest(args) || len(args) == 0 {
		printModelPriceHelp(e.Stdout)
		return nil
	}
	switch args[0] {
	case "set":
		return cmdModelPriceSet(e, args[1:])
	case "unset":
		return cmdModelPriceUnset(e, args[1:])
	case "ls":
		return cmdModelPriceLs(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：set | unset | ls\n用 lax model price --help 查看用法", args[0])
	}
}

// priceFieldLabels 是四个字段在人读输出里的名字。字段名本身（in / out / cache-read /
// cache-write）只由 store.PriceFieldNames 给，这里只负责翻译成中文，不另存一份字段清单。
var priceFieldLabels = map[store.PriceField]string{
	store.FieldPriceIn:          "输入价",
	store.FieldPriceOut:         "输出价",
	store.FieldPriceCacheRead:   "缓存读价",
	store.FieldPriceCacheWrite:  "缓存写价",
	store.FieldPriceTTSChar:     "合成价",
	store.FieldPriceAudioSecond: "转写价",
}

// priceValues 是一行上的六项单价，nil 表示这一项没填（账号侧即继承目录）。
type priceValues struct {
	in, out, cacheRead, cacheWrite, ttsChar, audioSec *float64
}

// providerPriceValues 取目录行上的六项价。
func providerPriceValues(m store.ProviderModel) priceValues {
	return priceValues{in: m.PriceIn, out: m.PriceOut, cacheRead: m.PriceCacheRead, cacheWrite: m.PriceCacheWrite,
		ttsChar: m.PriceTTSChar, audioSec: m.PriceAudioSec}
}

// accountPriceValues 取账号覆盖行上的六项价。
func accountPriceValues(m store.AccountModel) priceValues {
	return priceValues{in: m.PriceIn, out: m.PriceOut, cacheRead: m.PriceCacheRead, cacheWrite: m.PriceCacheWrite,
		ttsChar: m.PriceTTSChar, audioSec: m.PriceAudioSec}
}

// get 按字段名取那一项，供「清掉了什么 / 本来就没有」的判断用。
func (v priceValues) get(f store.PriceField) *float64 {
	switch f {
	case store.FieldPriceIn:
		return v.in
	case store.FieldPriceOut:
		return v.out
	case store.FieldPriceCacheRead:
		return v.cacheRead
	case store.FieldPriceCacheWrite:
		return v.cacheWrite
	case store.FieldPriceTTSChar:
		return v.ttsChar
	case store.FieldPriceAudioSecond:
		return v.audioSec
	}
	return nil
}

// count 数这一行有几项填了价：unset 的输出报「剩余几项」用它。
func (v priceValues) count() int {
	n := 0
	for _, f := range []store.PriceField{
		store.FieldPriceIn, store.FieldPriceOut, store.FieldPriceCacheRead, store.FieldPriceCacheWrite,
		store.FieldPriceTTSChar, store.FieldPriceAudioSecond,
	} {
		if v.get(f) != nil {
			n++
		}
	}
	return n
}

// priceFlagValue 把一个价格参数变成增量写入值：命令行没给就是 nil（这一项不动），
// 给了 0 就是 0（免费是合法值）。flag 包的默认值 0 不能被当成「填了 0 元」。
func priceFlagValue(fs *flagSet, name string, v *float64) *float64 {
	if !flagWasSet(fs, name) {
		return nil
	}
	value := *v
	return &value
}

// checkPriceModel 在动库之前判一遍模型名：空值（含只有空白）是用法错误（退出码 2）。
func checkPriceModel(raw, usageLine string) (string, error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "", usagef("模型名不能为空，例如 lax model price set glm-4.6 --in 0.6 --out 2.2\n%s", usageLine)
	}
	return model, nil
}

// priceWriteError 把 store 的写价错误分成两类：负数是用法错误（退出码 2，保留 store 的原文），
// 其余（模型不在目录里、账号没有这一行）原样带出（退出码 1），它们的文案已经写清下一步。
func priceWriteError(err error, usageLine string) error {
	if errors.Is(err, store.ErrNegativePrice) {
		return usagef("%v\n单价单位是每百万 token，0 是合法值（免费或打包），负数只可能是写错了\n%s", err, usageLine)
	}
	return err
}

// providersWithModel 找出目录里有这个模型的厂商，按名称升序。
func providersWithModel(st *store.Store, model string) ([]string, error) {
	providers, err := st.ListProviders()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range providers {
		rows, err := st.ProviderModels(p.Name)
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			if m.Model == model {
				out = append(out, p.Name)
				break
			}
		}
	}
	return out, nil
}

// resolvePriceProvider 决定厂商级写价落在哪一家上：--provider 给了就用它（行在不在由 store 判），
// 没给就按模型名唯一确定。同名模型出现在多家的目录里时是歧义，报错并列出候选——
// 静默挑一家会让人以为改的是另一家的价。
func resolvePriceProvider(st *store.Store, model, flagValue, usageLine string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}
	names, err := providersWithModel(st, model)
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("模型 %q 不在任何厂商的目录里：先用 lax model sync 拉上游清单或 lax model probe 探活，把它写进目录；lax model ls 能看到现有的模型名", model)
	case 1:
		return names[0], nil
	default:
		return "", usagef("模型 %q 同时出现在 %d 家厂商的目录里：%s\n价格按「供应商 × 模型」存，用 --provider 指定其中一家（例如 lax model price set %s --provider %s --in N）\n%s",
			model, len(names), strings.Join(names, "、"), model, names[0], usageLine)
	}
}

// priceFieldText 把写入的那几项写成「输入 0.60、输出 2.20」，只列给过的，顺序固定。
func priceFieldText(prices store.ModelPrices) string {
	all := []struct {
		field store.PriceField
		value *float64
	}{
		{store.FieldPriceIn, prices.In},
		{store.FieldPriceOut, prices.Out},
		{store.FieldPriceCacheRead, prices.CacheRead},
		{store.FieldPriceCacheWrite, prices.CacheWrite},
		{store.FieldPriceTTSChar, prices.TTSChar},
		{store.FieldPriceAudioSecond, prices.AudioSec},
	}
	parts := make([]string, 0, len(all))
	for _, item := range all {
		if item.value == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", priceFieldLabels[item.field], priceText(item.value)))
	}
	return strings.Join(parts, "、")
}

// modelPriceSetJSON 是 model price set --json 的形状。
// 价格报的是这次写进去的那几项，没写的为 null：它回答的是「改了什么」，
// 「合并之后是多少」由 model price ls 回答。
type modelPriceSetJSON struct {
	Model           string   `json:"model"`
	Provider        string   `json:"provider"`
	Account         string   `json:"account"`
	PriceIn         *float64 `json:"price_in"`
	PriceOut        *float64 `json:"price_out"`
	PriceCacheRead  *float64 `json:"price_cache_read"`
	PriceCacheWrite *float64 `json:"price_cache_write"`
	PriceTTSChar    *float64 `json:"price_tts_char"`
	PriceAudioSec   *float64 `json:"price_audio_second"`
}

// cmdModelPriceSet 写模型单价：给 --account 写该账号的覆盖价，否则写厂商级标价。
// 只写命令行上给过的那几项，没给的留空（表示继承），不做「一次写四个」。
func cmdModelPriceSet(e *Env, args []string) error {
	fs := newFlagSet(e, "model price set", "写模型单价（每百万 token）")
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时写该账号在这个模型上的覆盖价")
	in := fs.Float64("in", 0, "输入价，每百万 token；0 合法（免费）")
	out := fs.Float64("out", 0, "输出价，每百万 token")
	cacheRead := fs.Float64("cache-read", 0, "缓存读价，每百万 token；留空表示回退到输入价")
	cacheWrite := fs.Float64("cache-write", 0, "缓存写价，每百万 token；留空表示回退到输入价")
	ttsChar := fs.Float64("tts-char", 0, "语音合成价，每百万字符")
	audioSecond := fs.Float64("audio-second", 0, "语音转写价，每秒")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n一次写一个模型，模型名要写全；只写命令行上给过的那几项", modelPriceSetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceSetUsage)
	if err != nil {
		return err
	}
	prov := strings.TrimSpace(*provider)
	account := strings.TrimSpace(*accountName)
	if prov != "" && account != "" {
		return usagef("--provider 与 --account 只能给一个：厂商级标价用 --provider（或让它按模型名唯一确定），某个账号的覆盖价用 --account\n%s", modelPriceSetUsage)
	}

	prices := store.ModelPrices{
		In:         priceFlagValue(fs, "in", in),
		Out:        priceFlagValue(fs, "out", out),
		CacheRead:  priceFlagValue(fs, "cache-read", cacheRead),
		CacheWrite: priceFlagValue(fs, "cache-write", cacheWrite),
		TTSChar:    priceFlagValue(fs, "tts-char", ttsChar),
		AudioSec:   priceFlagValue(fs, "audio-second", audioSecond),
	}
	if prices.In == nil && prices.Out == nil && prices.CacheRead == nil && prices.CacheWrite == nil &&
		prices.TTSChar == nil && prices.AudioSec == nil {
		return usagef("六项单价一项都没给：至少给一项，例如 lax model price set %s --in 0.6 --out 2.2\n%s",
			model, modelPriceSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if account != "" {
		if err := a.st.SetAccountModelPrices(account, model, prices); err != nil {
			return priceWriteError(err, modelPriceSetUsage)
		}
		// 账号的厂商是派生值，输出里带上它才知道这条覆盖价对着哪一份厂商级标价。
		acc, err := a.st.GetAccount(account)
		if err != nil {
			return err
		}
		prov = acc.Provider
	} else {
		if prov, err = resolvePriceProvider(a.st, model, prov, modelPriceSetUsage); err != nil {
			return err
		}
		if err := a.st.SetProviderModelPrices(prov, model, prices); err != nil {
			return priceWriteError(err, modelPriceSetUsage)
		}
	}

	if e.JSON {
		return e.printJSON(modelPriceSetJSON{
			Model:           model,
			Provider:        prov,
			Account:         account,
			PriceIn:         prices.In,
			PriceOut:        prices.Out,
			PriceCacheRead:  prices.CacheRead,
			PriceCacheWrite: prices.CacheWrite,
			PriceTTSChar:    prices.TTSChar,
			PriceAudioSec:   prices.AudioSec,
		})
	}
	scope := fmt.Sprintf("厂商 %s 的厂商级标价", prov)
	if account != "" {
		scope = fmt.Sprintf("账号 %s（%s）的覆盖价", account, dashIfEmpty(prov))
	}
	fmt.Fprintf(e.Stdout, "已写入%s：%s %s（token 价每百万 token，合成价每百万字符，转写价每秒）\n", scope, model, priceFieldText(prices))
	fmt.Fprintln(e.Stdout, "下一步：lax model ls")
	return nil
}

// modelPriceUnsetJSON 是 model price unset --json 的形状。cleared 与 unchanged 用字段名
// （in / out / cache-read / cache-write）：脚本拿它判断哪几项真的清掉了。
type modelPriceUnsetJSON struct {
	Model     string   `json:"model"`
	Provider  string   `json:"provider"`
	Account   string   `json:"account"`
	Cleared   []string `json:"cleared"`
	Unchanged []string `json:"unchanged"`
	Remaining int      `json:"remaining"`
}

// rejectDashPriceFields 拦下把字段写成 `--in` 这类参数写法：flag 包只会说「参数不认识」，
// 这里能说清字段是位置参数并列出可用的四个。
func rejectDashPriceFields(args []string) error {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		for _, f := range store.PriceFieldNames() {
			if name == f {
				return usagef("未知的价格字段 %q：字段是位置参数，不带 -- 前缀，可用的是 %s\n%s",
					arg, strings.Join(store.PriceFieldNames(), "、"), modelPriceUnsetUsage)
			}
		}
	}
	return nil
}

// parsePriceFieldList 解析位置参数给的字段名：解析与报错只走 store.ParsePriceField 一份，
// 重复的只留一次（`unset m in in` 与单个 in 等价）。
func parsePriceFieldList(raw []string) ([]store.PriceField, error) {
	fields := make([]store.PriceField, 0, len(raw))
	seen := make(map[store.PriceField]bool, len(raw))
	for _, name := range raw {
		f, err := store.ParsePriceField(name)
		if err != nil {
			return nil, err
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		fields = append(fields, f)
	}
	return fields, nil
}

// fieldNames 把字段换成 store 的入口收的名字串；顺序与传入一致，重复的已经在前一步去掉。
func fieldNames(fields []store.PriceField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, string(f))
	}
	return out
}

// cmdModelPriceUnset 清掉某一项或几项：账号级清掉回到厂商级标价（不是免费），
// 厂商级清掉表示这一项未知。本来为空的不报错（幂等），输出说明「本来就没有」。
func cmdModelPriceUnset(e *Env, args []string) error {
	if err := rejectDashPriceFields(args); err != nil {
		return err
	}
	fs := newFlagSet(e, "model price unset", "清掉模型单价的一项或几项")
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号在这个模型上的覆盖价")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return usagef("%s\n模型名之后跟至少一个字段：%s", modelPriceUnsetUsage, strings.Join(store.PriceFieldNames(), "、"))
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceUnsetUsage)
	if err != nil {
		return err
	}
	fields, err := parsePriceFieldList(fs.Args()[1:])
	if err != nil {
		return usagef("%v\n%s", err, modelPriceUnsetUsage)
	}
	prov := strings.TrimSpace(*provider)
	account := strings.TrimSpace(*accountName)
	if prov != "" && account != "" {
		return usagef("--provider 与 --account 只能给一个：清厂商级标价用 --provider（或让它按模型名唯一确定），清某个账号的覆盖价用 --account\n%s", modelPriceUnsetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 先读再清：逐项输出要知道这一项「原来有」还是「本来就没有」。
	var before priceValues
	var accountID int64
	if account != "" {
		acc, err := a.st.GetAccount(account)
		if err != nil {
			return err
		}
		accountID = acc.ID
		prov = acc.Provider
		rows, err := a.st.AccountModels(accountID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Model == model {
				before = accountPriceValues(row)
				break
			}
		}
	} else {
		if prov, err = resolvePriceProvider(a.st, model, prov, modelPriceUnsetUsage); err != nil {
			return err
		}
		rows, err := a.st.ProviderModels(prov)
		if err != nil {
			return err
		}
		found := false
		for _, row := range rows {
			if row.Model == model {
				before = providerPriceValues(row)
				found = true
				break
			}
		}
		if !found {
			// 目录里没有这一行：交给 store 的同一个入口报错，文案与 set 同一份（含下一步）。
			if err := a.st.ClearProviderModelPrices(prov, model, fieldNames(fields)); err != nil {
				return err
			}
		}
	}

	if account != "" {
		err = a.st.ClearAccountModelPrices(account, model, fieldNames(fields))
	} else {
		err = a.st.ClearProviderModelPrices(prov, model, fieldNames(fields))
	}
	if err != nil {
		return err
	}

	// 清完再读一次：输出报的「剩余几项」是库里实际的值，不是命令行期望的值。
	after := before
	if account != "" {
		rows, err := a.st.AccountModels(accountID)
		if err != nil {
			return err
		}
		after = priceValues{}
		for _, row := range rows {
			if row.Model == model {
				after = accountPriceValues(row)
				break
			}
		}
	} else {
		rows, err := a.st.ProviderModels(prov)
		if err != nil {
			return err
		}
		after = priceValues{}
		for _, row := range rows {
			if row.Model == model {
				after = providerPriceValues(row)
				break
			}
		}
	}

	cleared := make([]string, 0, len(fields))
	unchanged := make([]string, 0, len(fields))
	for _, f := range fields {
		if before.get(f) == nil {
			unchanged = append(unchanged, string(f))
			continue
		}
		cleared = append(cleared, string(f))
	}

	if e.JSON {
		return e.printJSON(modelPriceUnsetJSON{
			Model:     model,
			Provider:  prov,
			Account:   account,
			Cleared:   cleared,
			Unchanged: unchanged,
			Remaining: after.count(),
		})
	}

	for _, f := range fields {
		if before.get(f) == nil {
			fmt.Fprintf(e.Stdout, "%s的%s本来就没有\n", model, priceFieldLabels[f])
			continue
		}
		fmt.Fprintf(e.Stdout, "%s已清掉%s\n", model, priceFieldLabels[f])
	}
	if account != "" {
		fmt.Fprintf(e.Stdout, "账号 %s 在 %s 上现有 %d 项覆盖价，其余按厂商级标价\n", account, model, after.count())
	} else {
		fmt.Fprintf(e.Stdout, "厂商 %s 的 %s 现有 %d 项标价，其余项未知\n", prov, model, after.count())
	}
	return nil
}

// modelPriceEffectiveJSON 是 model price ls --json 里的 effective：账号覆盖之后的合并结果，
// 合并规则只在 store.EffectiveModels 一处，cli 不重算。
type modelPriceEffectiveJSON struct {
	PriceIn         *float64 `json:"price_in"`
	PriceOut        *float64 `json:"price_out"`
	PriceCacheRead  *float64 `json:"price_cache_read"`
	PriceCacheWrite *float64 `json:"price_cache_write"`
	PriceTTSChar    *float64 `json:"price_tts_char"`
	PriceAudioSec   *float64 `json:"price_audio_second"`
}

// modelPriceLsJSON 是 model price ls --json 的一项。Effective 只在给了 --account 时有值。
type modelPriceLsJSON struct {
	Model           string                   `json:"model"`
	Provider        string                   `json:"provider"`
	PriceIn         *float64                 `json:"price_in"`
	PriceOut        *float64                 `json:"price_out"`
	PriceCacheRead  *float64                 `json:"price_cache_read"`
	PriceCacheWrite *float64                 `json:"price_cache_write"`
	PriceTTSChar    *float64                 `json:"price_tts_char"`
	PriceAudioSec   *float64                 `json:"price_audio_second"`
	Effective       *modelPriceEffectiveJSON `json:"effective,omitempty"`
}

// priceRow 是 model price ls 的一行：目录那一份价，加上（给了 --account 时）账号的有效价。
type priceRow struct {
	model    string
	provider string
	catalog  priceValues
	// account 是账号合并之后的值；账号上没有这个模型的行时四项都是 nil（显示成 -）。
	account priceValues
}

// cmdModelPriceLs 列目录里的模型单价：不给模型就列所有有价格的目录行（按模型名升序），
// 给了就只列这一个。给 --account 时并排显示该账号覆盖后的有效价。
func cmdModelPriceLs(e *Env, args []string) error {
	fs := newFlagSet(e, "model price ls", "列目录里的模型单价")
	provider := fs.String("provider", "", "只看这家厂商的目录")
	accountName := fs.String("account", "", "并排显示这个账号覆盖后的有效价")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("%s\n一次最多看一个模型，看全部就整个省掉模型名", modelPriceLsUsage)
	}
	var want string
	if fs.NArg() == 1 {
		var err error
		if want, err = checkPriceModel(fs.Arg(0), modelPriceLsUsage); err != nil {
			return err
		}
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	prov := strings.TrimSpace(*provider)
	account := strings.TrimSpace(*accountName)

	var providerNames []string
	if prov != "" {
		// 厂商不在表里时报错，不把「写错厂商名」显示成「一项价格都没有」。
		if _, err := a.st.GetProvider(prov); err != nil {
			return err
		}
		providerNames = []string{prov}
	} else {
		providers, err := a.st.ListProviders()
		if err != nil {
			return err
		}
		for _, p := range providers {
			providerNames = append(providerNames, p.Name)
		}
	}

	rows := make([]priceRow, 0, len(providerNames))
	inCatalog := false
	for _, name := range providerNames {
		list, err := a.st.ProviderModels(name)
		if err != nil {
			return err
		}
		for _, m := range list {
			if want != "" && m.Model != want {
				continue
			}
			inCatalog = inCatalog || m.Model == want || want == ""
			values := providerPriceValues(m)
			// 不给模型时只列有价的行：一份没有价格的目录行不回答「单价是多少」。
			if want == "" && values.count() == 0 {
				continue
			}
			rows = append(rows, priceRow{model: m.Model, provider: name, catalog: values})
		}
	}
	if want != "" && !inCatalog {
		return fmt.Errorf("模型 %q 不在目录里：先用 lax model ls 看现有的模型名；模型要先由 lax model sync 或 lax model probe 写进目录，价格才挂得上", want)
	}

	// 按模型名升序；同名模型在多家目录里时再按供应商名排，顺序固定。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].model != rows[j].model {
			return rows[i].model < rows[j].model
		}
		return rows[i].provider < rows[j].provider
	})

	if account != "" {
		acc, err := a.st.GetAccount(account)
		if err != nil {
			// 账号名写错时给下一步，不把一句 sql.ErrNoRows 摔给用户。
			return fmt.Errorf("账号 %q 不存在：用 lax account ls 看现有的账号，或 lax account add %s --provider <厂商> --key <key> 建它", account, account)
		}
		infos, err := a.st.EffectiveModels(acc.ID)
		if err != nil {
			return err
		}
		byModel := make(map[string]store.ModelInfo, len(infos))
		for _, info := range infos {
			byModel[info.Model] = info
		}
		for i := range rows {
			info, ok := byModel[rows[i].model]
			if !ok {
				continue
			}
			rows[i].account = priceValues{
				in: info.PriceIn, out: info.PriceOut,
				cacheRead: info.PriceCacheRead, cacheWrite: info.PriceCacheWrite,
				ttsChar: info.PriceTTSChar, audioSec: info.PriceAudioSec,
			}
		}
	}

	if e.JSON {
		out := make([]modelPriceLsJSON, 0, len(rows))
		for _, r := range rows {
			item := modelPriceLsJSON{
				Model:           r.model,
				Provider:        r.provider,
				PriceIn:         r.catalog.in,
				PriceOut:        r.catalog.out,
				PriceCacheRead:  r.catalog.cacheRead,
				PriceCacheWrite: r.catalog.cacheWrite,
				PriceTTSChar:    r.catalog.ttsChar,
				PriceAudioSec:   r.catalog.audioSec,
			}
			if account != "" {
				item.Effective = &modelPriceEffectiveJSON{
					PriceIn:         r.account.in,
					PriceOut:        r.account.out,
					PriceCacheRead:  r.account.cacheRead,
					PriceCacheWrite: r.account.cacheWrite,
					PriceTTSChar:    r.account.ttsChar,
					PriceAudioSec:   r.account.audioSec,
				}
			}
			out = append(out, item)
		}
		return e.printJSON(out)
	}

	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "一项价格都没有。模型先进目录（lax model sync 或 model probe），再定它的价，例如 lax model price set glm-4.6 --in 0.6 --out 2.2")
		return nil
	}

	// 给了 --account 时每个价格格子里并排两段：目录价 | 有效价。两段之间用 |，缓存那一格
	// 自己还要用 / 分读写，两种斜杠混在一起就分不清哪一段是哪一段。
	header := []string{"模型", "供应商", "输入价", "输出价", "缓存读 / 缓存写", "合成价 / 百万字符", "转写价 / 秒"}
	if account != "" {
		header = []string{"模型", "供应商", "输入价（目录 | 有效）", "输出价（目录 | 有效）", "缓存读 / 缓存写（目录 | 有效）", "合成价 / 百万字符（目录 | 有效）", "转写价 / 秒（目录 | 有效）"}
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.model,
			r.provider,
			priceColumnCell(r.catalog.in, r.account.in, account != ""),
			priceColumnCell(r.catalog.out, r.account.out, account != ""),
			cacheColumnCell(r.catalog, r.account, account != ""),
			priceColumnCell(r.catalog.ttsChar, r.account.ttsChar, account != ""),
			priceColumnCell(r.catalog.audioSec, r.account.audioSec, account != ""),
		})
	}
	fmt.Fprint(e.Stdout, renderTable(header, table))
	return nil
}

// priceColumnCell 是一个价格单元格：没填写 -；给了 --account 时写成「目录 | 有效」两列并排。
func priceColumnCell(catalog, effective *float64, withAccount bool) string {
	if !withAccount {
		return dashPrice(catalog)
	}
	return dashPrice(catalog) + " | " + dashPrice(effective)
}

// cacheColumnCell 是缓存那一格：两项都空时写「回退输入价」而不是 0（0 会让人以为缓存不花钱，
// 而实际是没单独定价、按输入价算），只空一项时那一项写 -。给了 --account 时两边各一格、并排显示。
func cacheColumnCell(catalog, effective priceValues, withAccount bool) string {
	if !withAccount {
		return cacheCell(catalog.cacheRead, catalog.cacheWrite)
	}
	if catalog.cacheRead == nil && catalog.cacheWrite == nil &&
		effective.cacheRead == nil && effective.cacheWrite == nil {
		return "回退输入价"
	}
	return cacheCell(catalog.cacheRead, catalog.cacheWrite) + " | " + cacheCell(effective.cacheRead, effective.cacheWrite)
}

// dashPrice 把没填的单价写成 -：空单元格看不出是「没定价」还是「渲染漏了」。
func dashPrice(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *v)
}

// cacheCell 是一项缓存单价的展示：两项都空时不是两个数，而是「回退输入价」这个实际行为。
func cacheCell(read, write *float64) string {
	if read == nil && write == nil {
		return "回退输入价"
	}
	return dashPrice(read) + " / " + dashPrice(write)
}
