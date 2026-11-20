package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"lax/internal/store"
)

var modelPriceTierSubs = []groupItem{
	{"unset", "删除指定上限的阶梯档位"},
	{"clear", "清空全部阶梯档位"},
}

var modelPriceWindowSubs = []groupItem{
	{"unset", "删除指定名称的分时窗口"},
	{"clear", "清空全部分时窗口"},
}

var modelPriceRuleSubs = []groupItem{
	{"show", "查看模型当前生效的计费规则与来源"},
	{"set", "从文件或 JSON 字符串写入完整计费规则"},
	{"unset", "清除计费规则"},
}

const (
	modelPriceTierUnsetUsage = "用法：lax model price tier unset <模型> [--provider 供应商|--account 账号] --tier <200k|1m|inf>"
	modelPriceTierClearUsage = "用法：lax model price tier clear <模型> [--provider 供应商|--account 账号]"

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

func parseTierThreshold(raw string) (*int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("必须指定 --tier <阈值|inf>，如 200k、1m、32768 或 inf")
	}
	lower := strings.ToLower(s)
	if lower == "inf" || lower == "+inf" || lower == "unlimited" || s == "+" || s == "∞" {
		return nil, nil
	}
	var mult float64
	switch {
	case strings.HasSuffix(lower, "k"):
		mult = 1000
		lower = lower[:len(lower)-1]
	case strings.HasSuffix(lower, "m"):
		mult = 1000000
		lower = lower[:len(lower)-1]
	default:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("--tier 值 %q 无效：必须是正整数（支持 k/m 后缀，如 200k、1.5m）或 inf", raw)
		}
		return &n, nil
	}
	f, err := strconv.ParseFloat(lower, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return nil, fmt.Errorf("--tier 值 %q 无效：必须是正整数（支持 k/m 后缀，如 200k、1.5m）或 inf", raw)
	}
	n := int64(math.Round(f * mult))
	if n <= 0 {
		return nil, fmt.Errorf("--tier 值 %q 无效：必须是正整数（支持 k/m 后缀，如 200k、1.5m）或 inf", raw)
	}
	return &n, nil
}

func parseTierArg(raw string) (threshold *int64, isAbove bool, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false, fmt.Errorf("必须指定 --tier <阈值|inf>，如 200k、200k+、>200k、1m 或 inf")
	}
	if strings.HasPrefix(s, ">") {
		inner := strings.TrimSpace(s[1:])
		n, err := parseTierThreshold(inner)
		if err != nil {
			return nil, false, err
		}
		if n == nil {
			return nil, false, fmt.Errorf("--tier %q 无效：> 后必须是具体的 token 阈值，如 >200k 或 200k+", raw)
		}
		return n, true, nil
	}
	if strings.HasSuffix(s, "+") && len(s) > 1 {
		inner := strings.TrimSpace(s[:len(s)-1])
		n, err := parseTierThreshold(inner)
		if err != nil {
			return nil, false, err
		}
		if n == nil {
			return nil, false, fmt.Errorf("--tier %q 无效：+ 前必须是具体的 token 阈值，如 200k+ 或 >200k", raw)
		}
		return n, true, nil
	}
	n, err := parseTierThreshold(s)
	return n, false, err
}

func parseTierMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "full", store.TierModeAllIn:
		return store.TierModeAllIn, nil
	case "split", "graduated", store.TierModeProgressive:
		return store.TierModeProgressive, nil
	default:
		return "", fmt.Errorf("未知的阶梯模式 %q，可用 full（全量）或 split（分段）", raw)
	}
}

func formatTierModeLabel(mode string) string {
	switch emptyDefault(mode, store.TierModeAllIn) {
	case store.TierModeProgressive:
		return "split (" + store.TierModeProgressive + ")"
	default:
		return "full (" + store.TierModeAllIn + ")"
	}
}

func formatTierDetail(u uiStyle, t store.PriceTier) string {
	var parts []string
	if t.Multiplier != nil {
		parts = append(parts, u.warn(fmt.Sprintf("%gx", *t.Multiplier)))
	}
	if t.Prices.In != nil || t.Prices.Out != nil || t.Prices.CacheRead != nil || t.Prices.CacheWrite != nil {
		parts = append(parts, formatPriceSetInline(u, t.Prices))
	}
	if len(parts) == 0 {
		return u.dim("继承基础单价 (1x)")
	}
	return strings.Join(parts, "  ")
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

type tierSetInput struct {
	model      string
	provider   string
	account    string
	tierRaw    string
	tierMode   string
	multiplier *float64
	in         *float64
	out        *float64
	cacheRead  *float64
	cacheWrite *float64
}

func ensureTierBaseBracket(tp *store.TieredPricing, n int64) {
	for _, b := range tp.Brackets {
		if b.MaxTokens != nil && *b.MaxTokens == n {
			return
		}
	}
	tp.Brackets = append(tp.Brackets, store.PriceTier{
		MaxTokens: &n,
		Prices:    store.PriceSet{},
	})
}

func upsertTierBracket(tp *store.TieredPricing, targetMax *int64, ps store.PriceSet, mult *float64) {
	for i := range tp.Brackets {
		if sameTierMax(tp.Brackets[i].MaxTokens, targetMax) {
			if ps.In != nil {
				tp.Brackets[i].Prices.In = ps.In
			}
			if ps.Out != nil {
				tp.Brackets[i].Prices.Out = ps.Out
			}
			if ps.CacheRead != nil {
				tp.Brackets[i].Prices.CacheRead = ps.CacheRead
			}
			if ps.CacheWrite != nil {
				tp.Brackets[i].Prices.CacheWrite = ps.CacheWrite
			}
			if mult != nil {
				tp.Brackets[i].Multiplier = mult
			}
			return
		}
	}
	tp.Brackets = append(tp.Brackets, store.PriceTier{
		MaxTokens:  targetMax,
		Prices:     ps,
		Multiplier: mult,
	})
}

func applyModelPriceTierSet(e *Env, fs *leafFlags, in tierSetInput) error {
	hasTier := flagWasSet(fs, "tier")
	hasMode := flagWasSet(fs, "tier-mode")

	var mappedMode string
	if hasMode {
		var err error
		mappedMode, err = parseTierMode(in.tierMode)
		if err != nil {
			return usagef("%v\n%s", err, modelPriceSetUsage)
		}
	}

	ps := store.PriceSet{
		In:         priceFlagValue(fs, "in", in.in),
		Out:        priceFlagValue(fs, "out", in.out),
		CacheRead:  priceFlagValue(fs, "cache-read", in.cacheRead),
		CacheWrite: priceFlagValue(fs, "cache-write", in.cacheWrite),
	}
	hasPrice := ps.In != nil || ps.Out != nil || ps.CacheRead != nil || ps.CacheWrite != nil
	hasMult := in.multiplier != nil

	var threshold *int64
	var isAbove bool
	if hasTier {
		var err error
		threshold, isAbove, err = parseTierArg(in.tierRaw)
		if err != nil {
			return usagef("%v\n%s", err, modelPriceSetUsage)
		}
		if !hasPrice && !hasMult {
			return usagef("必须至少指定一项单价（--in / --out / --cache-read / --cache-write）或倍率（--multiple / --multiplier）\n%s", modelPriceSetUsage)
		}
	} else if hasPrice || hasMult {
		return usagef("设置阶梯档位单价或倍率必须指定 --tier <200k|200k+|inf>\n%s", modelPriceSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, in.model, in.provider, in.account, modelPriceSetUsage)
	if err != nil {
		return err
	}
	cur, err := loadScopeRule(a.st, sc)
	if err != nil {
		return err
	}

	if !hasTier {
		if cur == nil || cur.Tiers == nil || len(cur.Tiers.Brackets) == 0 {
			return fmt.Errorf("模型 %q 尚未配置任何阶梯档位，请先用 lax model price set %s --tier <200k|inf> ... 添加档位", in.model, in.model)
		}
		next := cloneRuleOrInit(cur)
		next.Tiers.Mode = mappedMode
		if err := saveScopeRule(a.st, sc, next, modelPriceSetUsage); err != nil {
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
		fmt.Fprintf(e.Stdout, "%s 已更新阶梯模式\n", u.ok("✓"))
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("作用域", 8)), formatScopeValue(u, sc))
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模型", 8)), u.info(sc.model))
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模式", 8)), u.paint("35", formatTierModeLabel(next.Tiers.Mode)))
		return nil
	}

	next := cloneRuleOrInit(cur)
	if next.Tiers == nil {
		next.Tiers = &store.TieredPricing{
			Mode:  store.TierModeAllIn,
			Basis: store.TierBasisTotalInput,
		}
	}
	if hasMode {
		next.Tiers.Mode = mappedMode
	}

	var displayRange string
	switch {
	case isAbove:
		ensureTierBaseBracket(next.Tiers, *threshold)
		upsertTierBracket(next.Tiers, nil, ps, in.multiplier)
		displayRange = fmt.Sprintf(">%s (inf)", formatTierMax(threshold))
	case threshold != nil && hasMult && !hasPrice:
		// 直观语义：--tier 200k --multiple 2 表示 <=200k 按基础价 1x，>200k 按倍率 M。
		ensureTierBaseBracket(next.Tiers, *threshold)
		upsertTierBracket(next.Tiers, nil, store.PriceSet{}, in.multiplier)
		displayRange = fmt.Sprintf(">%s (inf)", formatTierMax(threshold))
	default:
		upsertTierBracket(next.Tiers, threshold, ps, in.multiplier)
		displayRange = formatTierMax(threshold)
	}
	sortTierBrackets(next.Tiers.Brackets)

	if err := saveScopeRule(a.st, sc, next, modelPriceSetUsage); err != nil {
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
	fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("档位", 8)), u.paint("35", displayRange))
	if in.multiplier != nil {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("倍率", 8)), u.warn(fmt.Sprintf("%gx", *in.multiplier)))
	}
	if hasPrice {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("单价", 8)), formatPriceSetInline(u, ps))
	}
	if hasMode {
		fmt.Fprintf(e.Stdout, "  %s  %s\n", u.dim(pad("模式", 8)), u.paint("35", formatTierModeLabel(next.Tiers.Mode)))
	}
	return nil
}

func cmdModelPriceTierUnset(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "厂商；与 --account 互斥，不给时按模型名唯一确定")
	accountName := fs.String("account", "", "账号名；给定时清该账号的覆盖规则")
	tierRaw := fs.String("tier", "", "要删除的档位上限（如 200k、1m、32768 或 inf）")
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
	maxPtr, err := parseTierThreshold(*tierRaw)
	if err != nil {
		return usagef("%v\n%s", err, modelPriceTierUnsetUsage)
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

type windowSetInput struct {
	model      string
	provider   string
	account    string
	windowName string
	tz         string
	start      string
	end        string
	daysRaw    string
	multiplier *float64
	in         *float64
	out        *float64
	cacheRead  *float64
	cacheWrite *float64
}

func applyModelPriceWindowSet(e *Env, fs *leafFlags, in windowSetInput) error {
	wName := strings.TrimSpace(in.windowName)
	if wName == "" {
		return usagef("必须指定 --window <名称>\n%s", modelPriceSetUsage)
	}
	wStart := strings.TrimSpace(in.start)
	wEnd := strings.TrimSpace(in.end)
	if wStart == "" || wEnd == "" {
		return usagef("必须指定 --start <HH:MM> 与 --end <HH:MM>\n%s", modelPriceSetUsage)
	}
	days, err := parseDaysList(in.daysRaw, modelPriceSetUsage)
	if err != nil {
		return err
	}
	multPtr := in.multiplier
	override := &store.PriceSet{
		In:         priceFlagValue(fs, "in", in.in),
		Out:        priceFlagValue(fs, "out", in.out),
		CacheRead:  priceFlagValue(fs, "cache-read", in.cacheRead),
		CacheWrite: priceFlagValue(fs, "cache-write", in.cacheWrite),
	}
	hasOverride := override.In != nil || override.Out != nil || override.CacheRead != nil || override.CacheWrite != nil
	if !hasOverride {
		override = nil
	}
	if multPtr == nil && override == nil {
		return usagef("必须至少设置 --multiplier 或一项覆盖单价（--in / --out / --cache-read / --cache-write）\n%s", modelPriceSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	sc, err := resolveRuleScope(a.st, in.model, in.provider, in.account, modelPriceSetUsage)
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
		next.Schedule.Timezone = strings.TrimSpace(in.tz)
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

	if err := saveScopeRule(a.st, sc, next, modelPriceSetUsage); err != nil {
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
		mode := formatTierModeLabel(rule.Tiers.Mode)
		basis := emptyDefault(rule.Tiers.Basis, store.TierBasisTotalInput)
		fmt.Fprintf(e.Stdout, "    %s  %s  %s\n",
			u.paint("35", "阶梯定价"),
			u.dim("mode=")+mode,
			u.dim("basis=")+basis)
		for i, b := range rule.Tiers.Brackets {
			lbl := formatBracketRange(i, rule.Tiers.Brackets)
			fmt.Fprintf(e.Stdout, "      %s  %s\n",
				u.paint("35", pad(lbl, 18)),
				formatTierDetail(u, b))
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
	fs.flagGroup("作用域", "provider", "account")
	fs.flagGroup("Token 用量", "in", "out", "cache-read", "cache-write")
	fs.flagGroup("语音用量", "tts-char", "audio-second")
	fs.flagGroup("时间模拟", "at")
	fs.setExamples(
		"lax model price test claude-sonnet-4 --in 150000 --out 2000",
		"lax model price test glm-4.6 --in 10000 --out 500 --at \"2026-11-20 02:00\"",
	)
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
