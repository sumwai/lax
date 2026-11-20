package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"lax/internal/store"
)

var modelPriceTierSubs = []groupItem{
	{"set", "新增或更新一个阶梯档位"},
	{"config", "配置阶梯模式（all_in / progressive）与判定基数"},
	{"unset", "删除指定上限的阶梯档位"},
	{"clear", "清空全部阶梯档位"},
}

var modelPriceWindowSubs = []groupItem{
	{"set", "新增或更新一个分时计费窗口"},
	{"unset", "删除指定名称的分时窗口"},
	{"clear", "清空全部分时窗口"},
}

var modelPriceRuleSubs = []groupItem{
	{"show", "查看模型当前生效的计费规则与来源"},
	{"set", "从文件或 JSON 字符串写入完整计费规则"},
	{"unset", "清除计费规则"},
}

const (
	modelPriceTierSetUsage = "用法：lax model price tier set <模型> [--provider 供应商|--account 账号] " +
		"--max <N|inf> [--in X] [--out X] [--cache-read X] [--cache-write X]"
	modelPriceTierConfigUsage = "用法：lax model price tier config <模型> [--provider 供应商|--account 账号] " +
		"[--mode all_in|progressive] [--basis total_input|uncached_input]"
	modelPriceTierUnsetUsage = "用法：lax model price tier unset <模型> [--provider 供应商|--account 账号] --max <N|inf>"
	modelPriceTierClearUsage = "用法：lax model price tier clear <模型> [--provider 供应商|--account 账号]"

	modelPriceWindowSetUsage = "用法：lax model price window set <模型> [--provider 供应商|--account 账号] " +
		"--name <名称> [--tz <IANA>] --start <HH:MM> --end <HH:MM> [--days sun,mon,...] " +
		"[--multiplier X] [--in X] [--out X] [--cache-read X] [--cache-write X]"
	modelPriceWindowUnsetUsage = "用法：lax model price window unset <模型> [--provider 供应商|--account 账号] --name <名称>"
	modelPriceWindowClearUsage = "用法：lax model price window clear <模型> [--provider 供应商|--account 账号]"

	modelPriceRuleShowUsage  = "用法：lax model price rule show <模型> [--provider 供应商|--account 账号]"
	modelPriceRuleSetUsage   = "用法：lax model price rule set <模型> [--provider 供应商|--account 账号] (--file <路径|-> | --rule-json <json>)"
	modelPriceRuleUnsetUsage = "用法：lax model price rule unset <模型> [--provider 供应商|--account 账号]"

	modelPriceTestUsage = "用法：lax model price test <模型> [--provider 供应商|--account 账号] " +
		"[--in N] [--out N] [--cache-read N] [--cache-write N] [--tts-char N] [--audio-second N] " +
		"[--at <RFC3339|YYYY-MM-DD HH:MM>]"
)

type ruleScope struct {
	model    string
	provider string
	account  string
}

func resolveRuleScope(st *store.Store, model, rawProvider, rawAccount, usageLine string) (ruleScope, error) {
	prov := strings.TrimSpace(rawProvider)
	account := strings.TrimSpace(rawAccount)
	if prov != "" && account != "" {
		return ruleScope{}, usagef("--provider 与 --account 只能给一个\n%s", usageLine)
	}
	if account != "" {
		acc, err := st.GetAccount(account)
		if err != nil {
			return ruleScope{}, err
		}
		return ruleScope{model: model, provider: acc.Provider, account: account}, nil
	}
	resolved, err := resolvePriceProvider(st, model, prov, usageLine)
	if err != nil {
		return ruleScope{}, err
	}
	return ruleScope{model: model, provider: resolved}, nil
}

func loadScopeRule(st *store.Store, sc ruleScope) (*store.PricingRule, error) {
	if sc.account != "" {
		acc, err := st.GetAccount(sc.account)
		if err != nil {
			return nil, err
		}
		rows, err := st.AccountModels(acc.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Model == sc.model {
				return r.PricingRule, nil
			}
		}
		return nil, fmt.Errorf("账号 %q 没有模型 %q 的记录", sc.account, sc.model)
	}
	rows, err := st.ProviderModels(sc.provider)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Model == sc.model {
			return r.PricingRule, nil
		}
	}
	return nil, fmt.Errorf("厂商 %q 的目录里没有模型 %q：%w", sc.provider, sc.model, store.ErrModelNotInCatalog)
}

func saveScopeRule(st *store.Store, sc ruleScope, rule *store.PricingRule, usageLine string) error {
	if rule == nil || (rule.Tiers == nil && rule.Schedule == nil) {
		if sc.account != "" {
			return st.ClearAccountPricingRule(sc.account, sc.model)
		}
		return st.ClearProviderPricingRule(sc.provider, sc.model)
	}
	if err := store.ValidatePricingRule(rule); err != nil {
		return usagef("%v\n%s", err, usageLine)
	}
	var err error
	if sc.account != "" {
		err = st.SetAccountPricingRule(sc.account, sc.model, rule)
	} else {
		err = st.SetProviderPricingRule(sc.provider, sc.model, rule)
	}
	if err != nil {
		return priceWriteError(err, usageLine)
	}
	return nil
}

func clearScopeRule(st *store.Store, sc ruleScope) error {
	if sc.account != "" {
		return st.ClearAccountPricingRule(sc.account, sc.model)
	}
	return st.ClearProviderPricingRule(sc.provider, sc.model)
}

func cloneRuleOrInit(r *store.PricingRule) *store.PricingRule {
	if r == nil {
		return &store.PricingRule{Version: 1}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return &store.PricingRule{Version: 1}
	}
	var out store.PricingRule
	if err := json.Unmarshal(b, &out); err != nil {
		return &store.PricingRule{Version: 1}
	}
	if out.Version == 0 {
		out.Version = 1
	}
	return &out
}

func parseTierMax(raw string, usageLine string) (*int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, usagef("必须指定 --max <N|inf>\n%s", usageLine)
	}
	lower := strings.ToLower(s)
	if lower == "inf" || lower == "+inf" || s == "∞" {
		return nil, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return nil, usagef("--max 值 %q 无效：必须是正整数或 inf\n%s", raw, usageLine)
	}
	return &n, nil
}

func sameTierMax(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func formatTierMax(m *int64) string {
	if m == nil {
		return "inf"
	}
	return strconv.FormatInt(*m, 10)
}

func sortTierBrackets(brackets []store.PriceTier) {
	sort.SliceStable(brackets, func(i, j int) bool {
		a, b := brackets[i].MaxTokens, brackets[j].MaxTokens
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return *a < *b
	})
}

func formatPriceSetInline(u uiStyle, p store.PriceSet) string {
	var parts []string
	if p.In != nil {
		parts = append(parts, u.dim("in=")+u.warn(fmt.Sprintf("$%s", priceText(p.In))))
	}
	if p.Out != nil {
		parts = append(parts, u.dim("out=")+u.warn(fmt.Sprintf("$%s", priceText(p.Out))))
	}
	if p.CacheRead != nil {
		parts = append(parts, u.dim("cache-read=")+u.warn(fmt.Sprintf("$%s", priceText(p.CacheRead))))
	}
	if p.CacheWrite != nil {
		parts = append(parts, u.dim("cache-write=")+u.warn(fmt.Sprintf("$%s", priceText(p.CacheWrite))))
	}
	if len(parts) == 0 {
		return u.dim("-")
	}
	return strings.Join(parts, "  ")
}

func formatScopeValue(u uiStyle, sc ruleScope) string {
	if sc.account != "" {
		return fmt.Sprintf("%s %s %s", u.dim("账号"), u.info(sc.account), u.dim("("+sc.provider+")"))
	}
	return fmt.Sprintf("%s %s", u.dim("厂商"), u.info(sc.provider))
}

func cmdModelPriceTierSet(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时写该账号的覆盖规则")
	maxRaw := fs.String("max", "", "档位上限 token 数（正整数）或 inf（无上限）")
	in := fs.Float64("in", 0, "输入价，每百万 token")
	out := fs.Float64("out", 0, "输出价，每百万 token")
	cacheRead := fs.Float64("cache-read", 0, "缓存读价，每百万 token")
	cacheWrite := fs.Float64("cache-write", 0, "缓存写价，每百万 token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceTierSetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceTierSetUsage)
	if err != nil {
		return err
	}
	maxPtr, err := parseTierMax(*maxRaw, modelPriceTierSetUsage)
	if err != nil {
		return err
	}
	ps := store.PriceSet{
		In:         priceFlagValue(fs, "in", in),
		Out:        priceFlagValue(fs, "out", out),
		CacheRead:  priceFlagValue(fs, "cache-read", cacheRead),
		CacheWrite: priceFlagValue(fs, "cache-write", cacheWrite),
	}
	if ps.In == nil && ps.Out == nil && ps.CacheRead == nil && ps.CacheWrite == nil {
		return usagef("至少需要指定一项单价（--in / --out / --cache-read / --cache-write）\n%s", modelPriceTierSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceTierSetUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	next := cloneRuleOrInit(cur)
	if next.Tiers == nil {
		next.Tiers = &store.TieredPricing{
			Mode:  store.TierModeAllIn,
			Basis: store.TierBasisTotalInput,
		}
	}

	updated := false
	for i := range next.Tiers.Brackets {
		if sameTierMax(next.Tiers.Brackets[i].MaxTokens, maxPtr) {
			if ps.In != nil {
				next.Tiers.Brackets[i].Prices.In = ps.In
			}
			if ps.Out != nil {
				next.Tiers.Brackets[i].Prices.Out = ps.Out
			}
			if ps.CacheRead != nil {
				next.Tiers.Brackets[i].Prices.CacheRead = ps.CacheRead
			}
			if ps.CacheWrite != nil {
				next.Tiers.Brackets[i].Prices.CacheWrite = ps.CacheWrite
			}
			updated = true
			break
		}
	}
	if !updated {
		next.Tiers.Brackets = append(next.Tiers.Brackets, store.PriceTier{
			MaxTokens: maxPtr,
			Prices:    ps,
		})
		sortTierBrackets(next.Tiers.Brackets)
	}

	if err := saveScopeRule(a.st, sc, next, modelPriceTierSetUsage); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"rule":     next,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已写入阶梯档位\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("档位上限", 8)), u.paint("35", formatTierMax(maxPtr)))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("单价", 8)), formatPriceSetInline(u, ps))
	return nil
}

func cmdModelPriceTierConfig(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时写该账号的覆盖规则")
	mode := fs.String("mode", "", "阶梯模式：all_in 或 progressive")
	basis := fs.String("basis", "", "判定基数：total_input 或 uncached_input")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceTierConfigUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceTierConfigUsage)
	if err != nil {
		return err
	}
	hasMode := flagWasSet(fs, "mode")
	hasBasis := flagWasSet(fs, "basis")
	if !hasMode && !hasBasis {
		return usagef("至少需要指定 --mode 或 --basis\n%s", modelPriceTierConfigUsage)
	}
	mVal := strings.TrimSpace(*mode)
	if hasMode && mVal != store.TierModeAllIn && mVal != store.TierModeProgressive {
		return usagef("未知的阶梯模式 %q，可用 all_in、progressive\n%s", mVal, modelPriceTierConfigUsage)
	}
	bVal := strings.TrimSpace(*basis)
	if hasBasis && bVal != store.TierBasisTotalInput && bVal != store.TierBasisUncachedInput {
		return usagef("未知的阶梯基数 %q，可用 total_input、uncached_input\n%s", bVal, modelPriceTierConfigUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceTierConfigUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	if cur == nil || cur.Tiers == nil || len(cur.Tiers.Brackets) == 0 {
		return fmt.Errorf("模型 %q 尚未配置任何阶梯档位，请先用 lax model price tier set %s --max <N|inf> ... 添加档位", model, model)
	}
	next := cloneRuleOrInit(cur)
	if hasMode {
		next.Tiers.Mode = mVal
	}
	if hasBasis {
		next.Tiers.Basis = bVal
	}
	if err := saveScopeRule(a.st, sc, next, modelPriceTierConfigUsage); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"rule":     next,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已更新阶梯配置\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	fmt.Fprintf(e.Stdout, "  %s  %s  %s\n",
		u.dim(pad("配置", 8)),
		u.dim("mode=")+u.paint("35", emptyDefault(next.Tiers.Mode, store.TierModeAllIn)),
		u.dim("basis=")+u.paint("35", emptyDefault(next.Tiers.Basis, store.TierBasisTotalInput)))
	return nil
}

func cmdModelPriceTierUnset(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号的覆盖规则")
	maxRaw := fs.String("max", "", "要删除的档位上限（正整数或 inf）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceTierUnsetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceTierUnsetUsage)
	if err != nil {
		return err
	}
	maxPtr, err := parseTierMax(*maxRaw, modelPriceTierUnsetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceTierUnsetUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	if cur == nil || cur.Tiers == nil || len(cur.Tiers.Brackets) == 0 {
		return fmt.Errorf("模型 %q 当前没有阶梯档位", model)
	}
	next := cloneRuleOrInit(cur)
	kept := make([]store.PriceTier, 0, len(next.Tiers.Brackets))
	found := false
	for _, b := range next.Tiers.Brackets {
		if sameTierMax(b.MaxTokens, maxPtr) {
			found = true
			continue
		}
		kept = append(kept, b)
	}
	if !found {
		return fmt.Errorf("未找到上限为 %s 的阶梯档位", formatTierMax(maxPtr))
	}
	if len(kept) == 0 {
		next.Tiers = nil
	} else {
		next.Tiers.Brackets = kept
	}
	if err := saveScopeRule(a.st, sc, next, modelPriceTierUnsetUsage); err != nil {
		return err
	}

	var outRule *store.PricingRule
	if next.Tiers != nil || next.Schedule != nil {
		outRule = next
	}
	if e.JSON {
		return e.printJSON(map[string]any{
			"model":       sc.model,
			"provider":    sc.provider,
			"account":     sc.account,
			"removed_max": formatTierMax(maxPtr),
			"rule":        outRule,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已删除阶梯档位 %s\n", u.ok("✓"), u.paint("35", formatTierMax(maxPtr)))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	return nil
}

func cmdModelPriceTierClear(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号的覆盖规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceTierClearUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceTierClearUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceTierClearUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	cleared := false
	var outRule *store.PricingRule
	if cur != nil && cur.Tiers != nil {
		next := cloneRuleOrInit(cur)
		next.Tiers = nil
		if err := saveScopeRule(a.st, sc, next, modelPriceTierClearUsage); err != nil {
			return err
		}
		cleared = true
		if next.Schedule != nil {
			outRule = next
		}
	} else if cur != nil && cur.Schedule != nil {
		outRule = cur
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"cleared":  cleared,
			"rule":     outRule,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已清空阶梯定价\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	return nil
}

var weekdayMap = map[string]int{
	"0": 0, "sun": 0, "sunday": 0,
	"1": 1, "mon": 1, "monday": 1,
	"2": 2, "tue": 2, "tuesday": 2,
	"3": 3, "wed": 3, "wednesday": 3,
	"4": 4, "thu": 4, "thursday": 4,
	"5": 5, "fri": 5, "friday": 5,
	"6": 6, "sat": 6, "saturday": 6,
}

var weekdayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func parseDaysList(raw, usageLine string) ([]int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	seen := make(map[int]bool, len(parts))
	var days []int
	for _, p := range parts {
		token := strings.ToLower(strings.TrimSpace(p))
		if token == "" {
			continue
		}
		d, ok := weekdayMap[token]
		if !ok {
			return nil, usagef("无效的星期 %q，可用 sun,mon,tue,wed,thu,fri,sat 或 0..6\n%s", p, usageLine)
		}
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	sort.Ints(days)
	return days, nil
}

func formatDaysList(days []int) string {
	if len(days) == 0 {
		return "每天"
	}
	out := make([]string, 0, len(days))
	for _, d := range days {
		if d >= 0 && d < len(weekdayNames) {
			out = append(out, weekdayNames[d])
		}
	}
	return strings.Join(out, ",")
}

func cmdModelPriceWindowSet(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时写该账号的覆盖规则")
	name := fs.String("name", "", "分时窗口名称")
	tz := fs.String("tz", "", "IANA 时区，例如 Asia/Shanghai 或 UTC")
	start := fs.String("start", "", "开始时间 HH:MM")
	end := fs.String("end", "", "结束时间 HH:MM（支持 24:00）")
	daysRaw := fs.String("days", "", "生效星期，逗号分隔如 sun,mon,sat（留空表示每天）")
	multiplier := fs.Float64("multiplier", 0, "价格倍率，如 0.5")
	in := fs.Float64("in", 0, "覆盖输入价，每百万 token")
	out := fs.Float64("out", 0, "覆盖输出价，每百万 token")
	cacheRead := fs.Float64("cache-read", 0, "覆盖缓存读价，每百万 token")
	cacheWrite := fs.Float64("cache-write", 0, "覆盖缓存写价，每百万 token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceWindowSetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceWindowSetUsage)
	if err != nil {
		return err
	}
	wName := strings.TrimSpace(*name)
	if wName == "" {
		return usagef("必须指定 --name <名称>\n%s", modelPriceWindowSetUsage)
	}
	wStart := strings.TrimSpace(*start)
	wEnd := strings.TrimSpace(*end)
	if wStart == "" || wEnd == "" {
		return usagef("必须指定 --start <HH:MM> 与 --end <HH:MM>\n%s", modelPriceWindowSetUsage)
	}
	days, err := parseDaysList(*daysRaw, modelPriceWindowSetUsage)
	if err != nil {
		return err
	}
	multPtr := priceFlagValue(fs, "multiplier", multiplier)
	override := &store.PriceSet{
		In:         priceFlagValue(fs, "in", in),
		Out:        priceFlagValue(fs, "out", out),
		CacheRead:  priceFlagValue(fs, "cache-read", cacheRead),
		CacheWrite: priceFlagValue(fs, "cache-write", cacheWrite),
	}
	hasOverride := override.In != nil || override.Out != nil || override.CacheRead != nil || override.CacheWrite != nil
	if !hasOverride {
		override = nil
	}
	if multPtr == nil && override == nil {
		return usagef("必须至少设置 --multiplier 或一项覆盖单价（--in / --out / --cache-read / --cache-write）\n%s", modelPriceWindowSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceWindowSetUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	next := cloneRuleOrInit(cur)
	if next.Schedule == nil {
		next.Schedule = &store.TimeSchedulePricing{}
	}
	if flagWasSet(fs, "tz") {
		next.Schedule.Timezone = strings.TrimSpace(*tz)
	}

	win := store.TimeWindow{
		Name:       wName,
		Days:       days,
		Start:      wStart,
		End:        wEnd,
		Multiplier: multPtr,
		Override:   override,
	}
	replaced := false
	for i := range next.Schedule.Windows {
		if next.Schedule.Windows[i].Name == wName {
			next.Schedule.Windows[i] = win
			replaced = true
			break
		}
	}
	if !replaced {
		next.Schedule.Windows = append(next.Schedule.Windows, win)
	}

	if err := saveScopeRule(a.st, sc, next, modelPriceWindowSetUsage); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"rule":     next,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已写入分时窗口\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	fmt.Fprintf(e.Stdout, "  %s  %s  %s-%s  %s\n",
		u.dim(pad("窗口", 8)), u.paint("35", wName), wStart, wEnd, u.dim("("+formatDaysList(days)+")"))
	if next.Schedule.Timezone != "" {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("时区", 8)), next.Schedule.Timezone)
	}
	if multPtr != nil {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("倍率", 8)), u.warn(fmt.Sprintf("%gx", *multPtr)))
	}
	if override != nil {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("覆盖单价", 8)), formatPriceSetInline(u, *override))
	}
	return nil
}

func cmdModelPriceWindowUnset(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号的覆盖规则")
	name := fs.String("name", "", "要删除的分时窗口名称")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceWindowUnsetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceWindowUnsetUsage)
	if err != nil {
		return err
	}
	wName := strings.TrimSpace(*name)
	if wName == "" {
		return usagef("必须指定 --name <名称>\n%s", modelPriceWindowUnsetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceWindowUnsetUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	if cur == nil || cur.Schedule == nil || len(cur.Schedule.Windows) == 0 {
		return fmt.Errorf("模型 %q 当前没有分时窗口", model)
	}
	next := cloneRuleOrInit(cur)
	kept := make([]store.TimeWindow, 0, len(next.Schedule.Windows))
	found := false
	for _, w := range next.Schedule.Windows {
		if w.Name == wName {
			found = true
			continue
		}
		kept = append(kept, w)
	}
	if !found {
		return fmt.Errorf("未找到名称为 %q 的分时窗口", wName)
	}
	if len(kept) == 0 {
		next.Schedule = nil
	} else {
		next.Schedule.Windows = kept
	}
	if err := saveScopeRule(a.st, sc, next, modelPriceWindowUnsetUsage); err != nil {
		return err
	}

	var outRule *store.PricingRule
	if next.Tiers != nil || next.Schedule != nil {
		outRule = next
	}
	if e.JSON {
		return e.printJSON(map[string]any{
			"model":        sc.model,
			"provider":     sc.provider,
			"account":      sc.account,
			"removed_name": wName,
			"rule":         outRule,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已删除分时窗口 %s\n", u.ok("✓"), u.paint("35", wName))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	return nil
}

func cmdModelPriceWindowClear(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号的覆盖规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceWindowClearUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceWindowClearUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceWindowClearUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}
	cleared := false
	var outRule *store.PricingRule
	if cur != nil && cur.Schedule != nil {
		next := cloneRuleOrInit(cur)
		next.Schedule = nil
		if err := saveScopeRule(a.st, sc, next, modelPriceWindowClearUsage); err != nil {
			return err
		}
		cleared = true
		if next.Tiers != nil {
			outRule = next
		}
	} else if cur != nil && cur.Tiers != nil {
		outRule = cur
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"cleared":  cleared,
			"rule":     outRule,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已清空分时计费窗口\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	return nil
}

func cmdModelPriceRuleShow(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时查看该账号合并后的有效规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceRuleShowUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceRuleShowUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceRuleShowUsage)
	if err != nil {
		return err
	}
	base, rule, baseSource, ruleSource, resolvedProvider, err := a.st.EffectivePricing(sc.account, sc.provider, sc.model)
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":       sc.model,
			"provider":    resolvedProvider,
			"account":     sc.account,
			"base":        base,
			"base_source": baseSource,
			"rule":        rule,
			"rule_source": ruleSource,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 模型计费规则\n", u.bold(u.info(sc.model)))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("厂商", 8)), u.info(resolvedProvider))
	if sc.account != "" {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("账号", 8)), u.info(sc.account))
	}
	baseLabel := "基础单价"
	if baseSource != "" {
		baseLabel += " (" + baseSource + ")"
	}
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad(baseLabel, 18)), formatPriceSetInline(u, store.PriceSet{
		In: base.In, Out: base.Out, CacheRead: base.CacheRead, CacheWrite: base.CacheWrite,
	}))

	if rule == nil {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("计费规则", 18)), u.dim("无"))
		return nil
	}
	ruleLabel := "计费规则"
	if ruleSource != "" {
		ruleLabel += " (" + ruleSource + ")"
	}
	fmt.Fprintf(e.Stdout, "  %s\n", u.dim(ruleLabel))

	if rule.Tiers != nil {
		mode := emptyDefault(rule.Tiers.Mode, store.TierModeAllIn)
		basis := emptyDefault(rule.Tiers.Basis, store.TierBasisTotalInput)
		fmt.Fprintf(e.Stdout, "    %s  %s  %s\n",
			u.paint("35", "阶梯定价"),
			u.dim("mode=")+mode,
			u.dim("basis=")+basis)
		for i, b := range rule.Tiers.Brackets {
			lbl := formatBracketRange(i, rule.Tiers.Brackets)
			fmt.Fprintf(e.Stdout, "      %s  %s\n",
				u.paint("35", pad(lbl, 18)),
				formatPriceSetInline(u, b.Prices))
		}
	}
	if rule.Schedule != nil {
		tz := emptyDefault(rule.Schedule.Timezone, "UTC")
		fmt.Fprintf(e.Stdout, "    %s  %s\n", u.paint("35", "分时计费"), u.dim("tz=")+tz)
		for _, w := range rule.Schedule.Windows {
			var extras []string
			if w.Multiplier != nil {
				extras = append(extras, u.warn(fmt.Sprintf("%gx", *w.Multiplier)))
			}
			if w.Override != nil {
				extras = append(extras, formatPriceSetInline(u, *w.Override))
			}
			fmt.Fprintf(e.Stdout, "      %s  %s-%s  %s  %s\n",
				u.paint("35", pad(w.Name, 12)),
				w.Start, w.End,
				u.dim(pad("("+formatDaysList(w.Days)+")", 14)),
				strings.Join(extras, "  "))
		}
	}
	return nil
}

func formatBracketRange(idx int, brackets []store.PriceTier) string {
	b := brackets[idx]
	if b.MaxTokens == nil {
		if idx == 0 {
			return "all"
		}
		return fmt.Sprintf("> %d", *brackets[idx-1].MaxTokens)
	}
	if idx == 0 {
		return fmt.Sprintf("<= %d", *b.MaxTokens)
	}
	return fmt.Sprintf("%d..%d", *brackets[idx-1].MaxTokens+1, *b.MaxTokens)
}

func cmdModelPriceRuleSet(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时写该账号的覆盖规则")
	filePath := fs.String("file", "", "从 JSON 文件读取规则（写 - 表示从标准输入读取）")
	ruleJSON := fs.String("rule-json", "", "直接传入计费规则 JSON 字符串")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceRuleSetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceRuleSetUsage)
	if err != nil {
		return err
	}
	hasFile := flagWasSet(fs, "file")
	hasJSON := flagWasSet(fs, "rule-json")
	if hasFile == hasJSON {
		return usagef("必须且只能指定 --file 或 --rule-json 之一\n%s", modelPriceRuleSetUsage)
	}

	var rawBytes []byte
	if hasFile {
		fp := strings.TrimSpace(*filePath)
		if fp == "" {
			return usagef("--file 路径不能为空\n%s", modelPriceRuleSetUsage)
		}
		if fp == "-" {
			rawBytes, err = io.ReadAll(e.stdin())
		} else {
			rawBytes, err = os.ReadFile(fp)
		}
		if err != nil {
			return fmt.Errorf("读取规则文件失败：%w", err)
		}
	} else {
		rawBytes = []byte(*ruleJSON)
	}

	var rule store.PricingRule
	if err := json.Unmarshal(rawBytes, &rule); err != nil {
		return usagef("解析计费规则 JSON 失败：%v\n%s", err, modelPriceRuleSetUsage)
	}
	if rule.Version == 0 {
		rule.Version = 1
	}
	if rule.Tiers == nil && rule.Schedule == nil {
		return usagef("计费规则不能为空：至少需要配置 tiers 或 schedule（若要清空规则请用 lax model price rule unset）\n%s", modelPriceRuleSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceRuleSetUsage)
	if err != nil {
		return err
	}
	if err := saveScopeRule(a.st, sc, &rule, modelPriceRuleSetUsage); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"rule":     &rule,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已写入计费规则\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("摘要", 8)), formatPricingRuleSummary(&rule))
	return nil
}

func cmdModelPriceRuleUnset(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号的覆盖规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceRuleUnsetUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceRuleUnsetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceRuleUnsetUsage)
	if err != nil {
		return err
	}
	if err := clearScopeRule(a.st, sc); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    sc.model,
			"provider": sc.provider,
			"account":  sc.account,
			"cleared":  true,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 已清除计费规则\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
	return nil
}

func parseAtTime(raw string, usageLine string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Now().Unix(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, usagef("--at 时间 %q 格式无效，支持 RFC3339（如 2026-11-20T02:30:00+08:00）或 YYYY-MM-DD HH:MM\n%s", raw, usageLine)
}

var costDimensionLabels = map[string]string{
	"in":           "输入 token",
	"cache_read":   "缓存读 token",
	"cache_write":  "缓存写 token",
	"out":          "输出 token",
	"reasoning":    "推理 token",
	"tts_char":     "语音合成字符",
	"audio_second": "语音转写秒",
}

func cmdModelPriceTest(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；按该账号的有效定价试算")
	inTokens := fs.Int64("in", 0, "未命中缓存的输入 token 数")
	outTokens := fs.Int64("out", 0, "输出 token 数")
	cacheRead := fs.Int64("cache-read", 0, "缓存读 token 数")
	cacheWrite := fs.Int64("cache-write", 0, "缓存写 token 数")
	ttsChar := fs.Int64("tts-char", 0, "语音合成字符数")
	audioSec := fs.Int64("audio-second", 0, "语音转写秒数")
	atRaw := fs.String("at", "", "模拟请求时间（RFC3339 或 YYYY-MM-DD HH:MM）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s", modelPriceTestUsage)
	}
	model, err := checkPriceModel(fs.Arg(0), modelPriceTestUsage)
	if err != nil {
		return err
	}
	for _, chk := range []struct {
		name string
		val  int64
	}{
		{"--in", *inTokens},
		{"--out", *outTokens},
		{"--cache-read", *cacheRead},
		{"--cache-write", *cacheWrite},
		{"--tts-char", *ttsChar},
		{"--audio-second", *audioSec},
	} {
		if chk.val < 0 {
			return usagef("%s 不能为负数（%d）\n%s", chk.name, chk.val, modelPriceTestUsage)
		}
	}
	ts, err := parseAtTime(*atRaw, modelPriceTestUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, model, *provider, *accountName, modelPriceTestUsage)
	if err != nil {
		return err
	}
	in := store.CostInput{
		TS:               ts,
		InTokensUncached: *inTokens,
		CacheRead:        *cacheRead,
		CacheWrite:       *cacheWrite,
		OutTokens:        *outTokens,
		TTSChars:         *ttsChar,
		AudioSeconds:     *audioSec,
	}
	bd, baseSource, ruleSource, resolvedProvider, err := a.st.SimulateModelCost(sc.account, sc.provider, sc.model, in)
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":       sc.model,
			"provider":    resolvedProvider,
			"account":     sc.account,
			"base_source": baseSource,
			"rule_source": ruleSource,
			"input":       in,
			"breakdown":   bd,
		})
	}

	u := newUI(e.Stdout)
	fmt.Fprintf(e.Stdout, "%s 费用试算\n", u.ok("✓"))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 10)), u.info(sc.model))
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("厂商", 10)), u.info(resolvedProvider))
	if sc.account != "" {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("账号", 10)), u.info(sc.account))
	}
	if bd.TierLabel != "" {
		fmt.Fprintf(e.Stdout, "  %s  %s %s\n",
			u.dim(pad("命中档位", 10)),
			u.paint("35", bd.TierLabel),
			u.dim(fmt.Sprintf("(basis=%d)", bd.BasisTokens)))
	}
	if bd.WindowName != "" {
		multText := ""
		if bd.Multiplier != nil {
			multText = " " + u.warn(fmt.Sprintf("%gx", *bd.Multiplier))
		}
		fmt.Fprintf(e.Stdout, "  %s  %s%s\n", u.dim(pad("命中窗口", 10)), u.paint("35", bd.WindowName), multText)
	}

	fmt.Fprintf(e.Stdout, "  %s\n", u.dim("费用明细"))
	printedItem := false
	for _, item := range bd.Items {
		if item.Quantity == 0 {
			continue
		}
		printedItem = true
		label := costDimensionLabels[item.Dimension]
		if label == "" {
			label = item.Dimension
		}
		unitStr := u.dim("-")
		if item.UnitPrice != nil {
			unitStr = u.warn("$" + priceText(item.UnitPrice))
		}
		costStr := u.dim("未知")
		if item.Cost != nil {
			costStr = u.warn(fmt.Sprintf("$%.6f", *item.Cost))
		}
		tierTag := ""
		if item.TierIndex >= 0 && item.Dimension == "in" {
			tierTag = u.paint("35", fmt.Sprintf(" [tier %d]", item.TierIndex+1))
		}
		fmt.Fprintf(e.Stdout, "    %s  %s  %s  %s%s\n",
			u.dim(pad(label, 14)),
			padLeft(strconv.FormatInt(item.Quantity, 10), 10),
			u.dim("@ ")+unitStr,
			u.dim("= ")+costStr,
			tierTag)
	}
	if !printedItem {
		fmt.Fprintf(e.Stdout, "    %s\n", u.dim("无用量输入（所有维度均为 0）"))
	}

	totalStr := u.dim("未知（缺少单价）")
	if bd.TotalCost != nil {
		totalStr = u.bold(u.warn(fmt.Sprintf("$%.6f", *bd.TotalCost)))
	}
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("总计费用", 10)), totalStr)
	return nil
}

func formatPricingRuleSummary(r *store.PricingRule) string {
	if r == nil || (r.Tiers == nil && r.Schedule == nil) {
		return "-"
	}
	var parts []string
	if r.Tiers != nil && len(r.Tiers.Brackets) > 0 {
		mode := emptyDefault(r.Tiers.Mode, store.TierModeAllIn)
		unit := "tiers"
		if len(r.Tiers.Brackets) == 1 {
			unit = "tier"
		}
		parts = append(parts, fmt.Sprintf("%d %s(%s)", len(r.Tiers.Brackets), unit, mode))
	}
	if r.Schedule != nil && len(r.Schedule.Windows) > 0 {
		if len(r.Schedule.Windows) == 1 {
			w := r.Schedule.Windows[0]
			if w.Multiplier != nil {
				parts = append(parts, fmt.Sprintf("1 window(%s %gx)", w.Name, *w.Multiplier))
			} else {
				parts = append(parts, fmt.Sprintf("1 window(%s)", w.Name))
			}
		} else {
			parts = append(parts, fmt.Sprintf("%d windows", len(r.Schedule.Windows)))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func emptyDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
