package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"lax/internal/account"
	"lax/internal/store"
)

var routeSubs = []groupItem{
	{"ls", "列出默认策略与按模型的覆盖"},
	{"show", "查看一个模型或默认策略最终生效的设置"},
	{"set", "写一条模型策略或默认策略（整条覆盖）"},
	{"unset", "删除一条策略，回落到默认或内置默认"},
	{"account", "设置账号权重：ls / set / unset"},
	{"explain", "预演一次选号，不请求上游、不写记录"},
}

func cmdRoute(e *Env, args []string) error {
	if isHelpRequest(args) || len(args) == 0 {
		printGroupHelp(e.Stdout, "route", "管理选号策略与账号权重", routeSubs)
		return nil
	}
	switch args[0] {
	case "ls":
		return cmdRouteLs(e, args[1:])
	case "show":
		return cmdRouteShow(e, args[1:])
	case "set":
		return cmdRouteSet(e, args[1:])
	case "unset":
		return cmdRouteUnset(e, args[1:])
	case "account":
		return cmdRouteAccount(e, args[1:])
	case "explain":
		return cmdRouteExplain(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | show | set | unset | account | explain\n用 lax route --help 查看子命令", args[0])
	}
}

func cmdRouteLs(e *Env, args []string) error {
	fs := newFlagSet(e, "route ls", "列出默认策略与按模型的覆盖")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("用法：lax route ls [--json]\n它列出全部策略；看一个模型用 lax route show <模型>")
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	policies, err := a.st.ListRoutePolicies()
	if err != nil {
		return err
	}
	hasDefault := false
	for _, p := range policies {
		if p.Model == "" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		policies = append([]store.RoutePolicy{store.DefaultRoutePolicy()}, policies...)
	}

	items := make([]routePolicyJSON, 0, len(policies))
	for _, p := range policies {
		source := "model"
		if p.Model == "" {
			source = "default"
			if !hasDefault {
				source = "builtin"
			}
		}
		items = append(items, routePolicyToJSON(p, source))
	}
	if e.JSON {
		return e.printJSON(items)
	}
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		rows = append(rows, []string{
			p.Target, routeSourceText(p.Source), p.Strategy, p.Sticky,
			fmt.Sprintf("%.2f", p.Temperature), routePercentText(p.Explore),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"目标", "来源", "策略", "会话", "温度", "探索"}, rows))
	fmt.Fprintln(e.Stdout, "查看一个模型的完整设置：lax route show <模型>")
	return nil
}

func cmdRouteShow(e *Env, args []string) error {
	fs := newFlagSet(e, "route show", "查看一个模型或默认策略最终生效的设置")
	useDefault := fs.Bool("default", false, "查看默认策略")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target, err := routeTargetArg(fs, *useDefault, "lax route show <模型> | lax route show --default")
	if err != nil {
		return err
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	if target != "" {
		target, err = resolveRouteModel(a.st, target)
		if err != nil {
			return err
		}
	}
	p, source, err := a.st.ResolveRoutePolicy(target)
	if err != nil {
		return err
	}
	item := routePolicyToJSON(p, source)
	if e.JSON {
		return e.printJSON(item)
	}
	rows := [][]string{
		{"目标", item.Target},
		{"来源", routeSourceText(source)},
		{"策略", item.Strategy},
		{"会话", item.Sticky},
		{"温度", fmt.Sprintf("%.2f", item.Temperature)},
		{"探索", routePercentText(item.Explore)},
		{"统计窗口", routeWindowText(item.WindowSeconds)},
		{"最小样本", strconv.Itoa(item.MinSamples)},
		{"优先级评分", fmt.Sprintf("%.2f", item.ScorePriority)},
		{"成功率评分", fmt.Sprintf("%.2f", item.ScoreSuccess)},
		{"延迟评分", fmt.Sprintf("%.2f", item.ScoreLatency)},
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"项", "值"}, rows))
	return nil
}

func cmdRouteSet(e *Env, args []string) error {
	defaults := store.DefaultRoutePolicy()
	fs := newFlagSet(e, "route set", "写一条模型策略或默认策略，整条覆盖")
	useDefault := fs.Bool("default", false, "写默认策略")
	strategy := fs.String("strategy", "", "priority / rotate / weighted / success / softmax")
	sticky := fs.String("sticky", defaults.Sticky, "会话粘性：prefer / off / strict")
	temperature := fs.Float64("temperature", defaults.Temperature, "softmax 温度，越低越集中")
	explore := fs.Float64("explore", defaults.Explore*100, "探索比例，按百分比给，例如 2 表示 2%")
	scorePriority := fs.Float64("score-priority", defaults.PriorityWeight, "softmax 的优先级评分权重")
	scoreSuccess := fs.Float64("score-success", defaults.SuccessWeight, "softmax 的成功率评分权重")
	scoreLatency := fs.Float64("score-latency", defaults.LatencyWeight, "softmax 的延迟评分权重")
	window := fs.String("window", routeWindowText(defaults.WindowSeconds), "统计窗口：90m、24h、7d")
	minSamples := fs.Int("min-samples", defaults.MinSamples, "成功率平滑的最小样本")
	usage := "用法：lax route set <模型> --strategy priority|rotate|weighted|success|softmax [参数]\n" +
		"      lax route set --default --strategy <策略> [参数]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*strategy) == "" {
		return usagef("%s\n--strategy 必填：先选 priority（主备）、rotate（轮转）、weighted（按权重轮转）、success（按成功率）或 softmax（按概率）", usage)
	}
	target, err := routeTargetArg(fs, *useDefault, usage)
	if err != nil {
		return err
	}
	windowSeconds, err := parseRouteWindow(*window)
	if err != nil {
		return usagef("--window：%v\n%s", err, usage)
	}
	p := store.RoutePolicy{
		Strategy:       strings.TrimSpace(*strategy),
		Sticky:         strings.TrimSpace(*sticky),
		Temperature:    *temperature,
		Explore:        *explore / 100,
		PriorityWeight: *scorePriority,
		SuccessWeight:  *scoreSuccess,
		LatencyWeight:  *scoreLatency,
		WindowSeconds:  windowSeconds,
		MinSamples:     *minSamples,
	}
	if err := store.ValidateRoutePolicy(p); err != nil {
		return usagef("%v\n%s", err, usage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	if target != "" {
		target, err = resolveRouteModel(a.st, target)
		if err != nil {
			return err
		}
	}
	p.Model = target
	if err := a.st.SetRoutePolicy(p); err != nil {
		return err
	}
	saved, source, err := a.st.ResolveRoutePolicy(target)
	if err != nil {
		return err
	}
	item := routePolicyToJSON(saved, source)
	if e.JSON {
		return e.printJSON(item)
	}
	fmt.Fprintf(e.Stdout, "已设置 %s 的选号策略：%s，会话 %s，温度 %.2f，探索 %s\n",
		item.Target, item.Strategy, item.Sticky, item.Temperature, routePercentText(item.Explore))
	fmt.Fprintf(e.Stdout, "下一步：%s 核对\n", routeShowCommand(target))
	return nil
}

func cmdRouteUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "route unset", "删除一条策略，回落到默认或内置默认")
	useDefault := fs.Bool("default", false, "删除默认策略")
	usage := "用法：lax route unset <模型> | lax route unset --default"
	if err := fs.Parse(args); err != nil {
		return err
	}
	target, err := routeTargetArg(fs, *useDefault, usage)
	if err != nil {
		return err
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	if target != "" {
		target, err = resolveRouteModel(a.st, target)
		if err != nil {
			return err
		}
	}
	removed, err := a.st.DeleteRoutePolicy(target)
	if err != nil {
		return err
	}
	p, source, err := a.st.ResolveRoutePolicy(target)
	if err != nil {
		return err
	}
	item := routePolicyToJSON(p, source)
	if e.JSON {
		return e.printJSON(map[string]any{
			"target":    routeTargetText(target),
			"removed":   removed,
			"effective": item,
		})
	}
	if removed {
		fmt.Fprintf(e.Stdout, "已删除 %s 的策略；现在生效：%s / %s（%s）\n",
			routeTargetText(target), item.Strategy, item.Sticky, routeSourceText(source))
	} else {
		fmt.Fprintf(e.Stdout, "%s 本来没有策略；现在生效：%s / %s（%s）\n",
			routeTargetText(target), item.Strategy, item.Sticky, routeSourceText(source))
	}
	return nil
}

func cmdRouteAccount(e *Env, args []string) error {
	if isHelpRequest(args) || len(args) == 0 {
		printGroupHelp(e.Stdout, "route account", "设置账号在选号里的权重", []groupItem{
			{"ls", "列出显式权重，或看一个模型上所有账号的最终权重"},
			{"set", "设置账号默认权重或账号在某个模型上的权重"},
			{"unset", "删除一条账号权重，回落到账号默认或 1"},
		})
		return nil
	}
	switch args[0] {
	case "ls":
		return cmdRouteAccountLs(e, args[1:])
	case "set":
		return cmdRouteAccountSet(e, args[1:])
	case "unset":
		return cmdRouteAccountUnset(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | set | unset\n用 lax route account --help 查看子命令", args[0])
	}
}

func cmdRouteAccountLs(e *Env, args []string) error {
	fs := newFlagSet(e, "route account ls", "列出账号权重，或看一个模型上所有账号的最终权重")
	model := fs.String("model", "", "只看这个模型上的最终权重")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("用法：lax route account ls [--model <模型>] [--json]")
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if *model != "" {
		resolved, err := resolveRouteModel(a.st, *model)
		if err != nil {
			return err
		}
		accounts, err := a.st.ListAccounts()
		if err != nil {
			return err
		}
		rows, err := a.st.ListRouteAccountWeights()
		if err != nil {
			return err
		}
		defaultWeights := map[int64]int{}
		modelWeights := map[int64]int{}
		for _, row := range rows {
			if row.Model == "" {
				defaultWeights[row.AccountID] = row.Weight
			}
			if row.Model == resolved {
				modelWeights[row.AccountID] = row.Weight
			}
		}
		items := make([]routeAccountWeightJSON, 0, len(accounts))
		for _, acc := range accounts {
			weight, source := 1, "builtin"
			if w, ok := defaultWeights[acc.ID]; ok {
				weight, source = w, "account"
			}
			if w, ok := modelWeights[acc.ID]; ok {
				weight, source = w, "model"
			}
			items = append(items, routeAccountWeightJSON{
				Account: acc.Name, Model: resolved, Weight: weight, Source: source,
			})
		}
		return printRouteAccountWeights(e, items)
	}

	rows, err := a.st.ListRouteAccountWeights()
	if err != nil {
		return err
	}
	accounts, err := a.st.ListAccounts()
	if err != nil {
		return err
	}
	names := make(map[int64]string, len(accounts))
	for _, acc := range accounts {
		names[acc.ID] = acc.Name
	}
	items := make([]routeAccountWeightJSON, 0, len(rows))
	for _, row := range rows {
		model := row.Model
		source := "model"
		if model == "" {
			model, source = "*", "account"
		}
		items = append(items, routeAccountWeightJSON{
			Account: names[row.AccountID], Model: model, Weight: row.Weight, Source: source,
		})
	}
	return printRouteAccountWeights(e, items)
}

func printRouteAccountWeights(e *Env, items []routeAccountWeightJSON) error {
	if e.JSON {
		return e.printJSON(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(e.Stdout, "还没有设置账号权重。用 lax route account set <账号> --weight 3 给账号加权")
		return nil
	}
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{item.Account, item.Model, strconv.Itoa(item.Weight), routeWeightSourceText(item.Source)})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"账号", "模型", "权重", "来源"}, rows))
	return nil
}

func cmdRouteAccountSet(e *Env, args []string) error {
	fs := newFlagSet(e, "route account set", "设置账号默认权重或账号在某个模型上的权重")
	model := fs.String("model", "", "只覆盖这个模型；不写就是账号默认")
	weight := fs.Int("weight", 1, "权重，1 到 1000")
	usage := "用法：lax route account set <账号> [--model <模型>] --weight N"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n账号名要写一个；--weight 必填", usage)
	}
	if !fs.Changed("weight") {
		return usagef("%s\n少了 --weight：例如 --weight 3 表示这个账号被选中的相对次数是普通账号的 3 倍", usage)
	}
	if err := store.ValidateRouteWeight(*weight); err != nil {
		return usagef("%v\n%s", err, usage)
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	acc, err := a.st.GetAccount(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", fs.Arg(0))
	}
	resolved := ""
	if *model != "" {
		resolved, err = resolveRouteModel(a.st, *model)
		if err != nil {
			return err
		}
	}
	if err := a.st.SetRouteAccountWeight(acc.ID, resolved, *weight); err != nil {
		return err
	}
	item := routeAccountWeightJSON{
		Account: acc.Name, Model: resolved, Weight: *weight,
		Source: routeWeightSource(resolved),
	}
	if e.JSON {
		return e.printJSON(item)
	}
	scope := "账号默认"
	if resolved != "" {
		scope = "模型 " + resolved
	}
	fmt.Fprintf(e.Stdout, "已设置 %s 的%s权重为 %d；按权重轮转（weighted）与按概率（softmax）会按这个比例选号\n",
		acc.Name, scope, *weight)
	return nil
}

func cmdRouteAccountUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "route account unset", "删除一条账号权重，回落到账号默认或 1")
	model := fs.String("model", "", "删除这个模型上的覆盖；不写就删账号默认")
	usage := "用法：lax route account unset <账号> [--model <模型>]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n账号名要写一个", usage)
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	acc, err := a.st.GetAccount(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", fs.Arg(0))
	}
	resolved := ""
	if *model != "" {
		resolved, err = resolveRouteModel(a.st, *model)
		if err != nil {
			return err
		}
	}
	removed, err := a.st.DeleteRouteAccountWeight(acc.ID, resolved)
	if err != nil {
		return err
	}
	weights, err := a.st.RouteWeights(resolved)
	if err != nil {
		return err
	}
	effective := weights[acc.ID]
	if effective == 0 {
		effective = 1
	}
	if e.JSON {
		return e.printJSON(map[string]any{
			"account": acc.Name, "model": resolved, "removed": removed, "effective": effective,
		})
	}
	if removed {
		fmt.Fprintf(e.Stdout, "已删除 %s 的权重；现在生效 %d\n", acc.Name, effective)
	} else {
		fmt.Fprintf(e.Stdout, "%s 本来没有这条权重；现在生效 %d\n", acc.Name, effective)
	}
	return nil
}

func cmdRouteExplain(e *Env, args []string) error {
	fs := newFlagSet(e, "route explain", "预演一次选号，不请求上游、不写记录")
	session := fs.String("session", "", "会话键，用来演示粘性")
	seed := fs.Int64("seed", 1, "softmax 的随机种子；同一个种子与同样数据得到同样顺序")
	usage := "用法：lax route explain <模型> [--session <会话键>] [--seed N] [--json]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n要预演的模型名必须给一个", usage)
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	model, err := resolveRouteModel(a.st, fs.Arg(0))
	if err != nil {
		return err
	}
	accounts, err := a.st.ListAccounts()
	if err != nil {
		return err
	}
	now := time.Now()
	snapshot, err := a.st.RouteSnapshot(model, now)
	if err != nil {
		return err
	}
	decision := account.Rank(account.RouteInput{
		Accounts: accounts,
		Model:    model,
		Now:      now,
		Policy:   snapshot.Policy,
		Weights:  snapshot.Weights,
		Stats:    snapshot.Stats,
		Session:  *session,
		Seed:     *seed,
	})
	policy := routePolicyToJSON(snapshot.Policy, snapshot.PolicySource)
	if e.JSON {
		return e.printJSON(routeExplainJSON(model, policy, decision, account.ExplainReason(decision)))
	}

	fmt.Fprintf(e.Stdout, "模型 %s：%s / 会话 %s / 来源 %s\n",
		model, policy.Strategy, policy.Sticky, routeSourceText(policy.Source))
	if len(decision.Choices) == 0 {
		fmt.Fprintln(e.Stdout, "没有候选账号。")
	}
	if len(decision.Choices) > 0 {
		rows := make([][]string, 0, len(decision.Choices))
		for i, c := range decision.Choices {
			prob := "-"
			if c.Probability != nil {
				prob = routePercentText(*c.Probability)
			}
			latency := "-"
			if c.LatencySamples > 0 {
				latency = fmt.Sprintf("%.0fms", c.AvgLatencyMS)
			}
			rows = append(rows, []string{
				strconv.Itoa(i + 1), c.Account.Name, strconv.Itoa(c.Weight),
				strconv.FormatInt(c.Attempts, 10), routePercentText(c.SuccessRate),
				latency, fmt.Sprintf("%.3f", c.Score), prob,
			})
		}
		fmt.Fprint(e.Stdout, renderTable([]string{"顺序", "账号", "权重", "样本", "成功率", "平均延迟", "得分", "概率"}, rows))
		names := make([]string, 0, len(decision.Choices))
		for _, c := range decision.Choices {
			names = append(names, c.Account.Name)
		}
		fmt.Fprintf(e.Stdout, "最终顺序：%s\n", strings.Join(names, " → "))
	}
	if len(decision.Excluded) > 0 {
		parts := make([]string, 0, len(decision.Excluded))
		for _, ex := range decision.Excluded {
			parts = append(parts, fmt.Sprintf("%s（%s）", ex.Account.Name, ex.Reason))
		}
		fmt.Fprintf(e.Stdout, "排除：%s\n", strings.Join(parts, "；"))
	}
	if reason := account.ExplainReason(decision); reason != "" {
		fmt.Fprintf(e.Stdout, "原因：%s\n", reason)
	}
	return nil
}

func routeTargetArg(fs *flagSet, useDefault bool, usage string) (string, error) {
	if useDefault && fs.NArg() > 0 {
		return "", usagef("--default 与模型名不能一起写\n%s", usage)
	}
	if !useDefault && fs.NArg() == 0 {
		return "", usagef("要写一个模型名，或加 --default 改默认策略\n%s", usage)
	}
	if fs.NArg() > 1 {
		return "", usagef("一次只能写一个目标\n%s", usage)
	}
	if useDefault {
		return "", nil
	}
	target := strings.TrimSpace(fs.Arg(0))
	if target == "" {
		return "", usagef("模型名不能为空\n%s", usage)
	}
	return target, nil
}

func resolveRouteModel(st *store.Store, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", usagef("模型名不能为空")
	}
	resolved, isAlias, err := st.ResolveModelAlias(raw)
	if err != nil {
		return "", fmt.Errorf("模型名 %s 的别名链解析失败：%v；用 lax model alias ls 看这条链", raw, err)
	}
	if isAlias {
		return resolved, nil
	}
	return raw, nil
}

type routePolicyJSON struct {
	Target        string  `json:"target"`
	Model         string  `json:"model"`
	Source        string  `json:"source"`
	Strategy      string  `json:"strategy"`
	Sticky        string  `json:"sticky"`
	Temperature   float64 `json:"temperature"`
	Explore       float64 `json:"explore"`
	ScorePriority float64 `json:"score_priority"`
	ScoreSuccess  float64 `json:"score_success"`
	ScoreLatency  float64 `json:"score_latency"`
	WindowSeconds int64   `json:"window_seconds"`
	MinSamples    int     `json:"min_samples"`
	UpdatedAt     int64   `json:"updated_at"`
}

func routePolicyToJSON(p store.RoutePolicy, source string) routePolicyJSON {
	return routePolicyJSON{
		Target: routeTargetText(p.Model), Model: p.Model, Source: source,
		Strategy: p.Strategy, Sticky: p.Sticky, Temperature: p.Temperature,
		Explore: p.Explore, ScorePriority: p.PriorityWeight, ScoreSuccess: p.SuccessWeight,
		ScoreLatency: p.LatencyWeight, WindowSeconds: p.WindowSeconds,
		MinSamples: p.MinSamples, UpdatedAt: p.UpdatedAt,
	}
}

type routeAccountWeightJSON struct {
	Account string `json:"account"`
	Model   string `json:"model"`
	Weight  int    `json:"weight"`
	Source  string `json:"source"`
}

func routeTargetText(model string) string {
	if model == "" {
		return "default"
	}
	return model
}

func routeShowCommand(model string) string {
	if model == "" {
		return "lax route show --default"
	}
	return "lax route show " + model
}

func routeSourceText(source string) string {
	switch source {
	case "builtin":
		return "内置"
	case "default":
		return "默认"
	case "model":
		return "模型"
	default:
		return source
	}
}

func routeWeightSourceText(source string) string {
	switch source {
	case "builtin":
		return "内置 1"
	case "account":
		return "账号默认"
	case "model":
		return "模型覆盖"
	default:
		return source
	}
}

func routeWeightSource(model string) string {
	if model == "" {
		return "account"
	}
	return "model"
}

func routePercentText(v float64) string {
	s := strconv.FormatFloat(v*100, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		s = "0"
	}
	return s + "%"
}

func routeWindowText(seconds int64) string {
	switch {
	case seconds > 0 && seconds%86400 == 0:
		return strconv.FormatInt(seconds/86400, 10) + "d"
	case seconds > 0 && seconds%3600 == 0:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	case seconds > 0 && seconds%60 == 0:
		return strconv.FormatInt(seconds/60, 10) + "m"
	default:
		return strconv.FormatInt(seconds, 10) + "s"
	}
}

func parseRouteWindow(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("不能为空，例如 24h、7d")
	}
	multiplier := int64(0)
	number := raw
	switch {
	case strings.HasSuffix(raw, "d"):
		multiplier, number = 86400, strings.TrimSuffix(raw, "d")
	case strings.HasSuffix(raw, "w"):
		multiplier, number = 7*86400, strings.TrimSuffix(raw, "w")
	}
	if multiplier > 0 {
		n, err := strconv.ParseInt(number, 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q 不是有效的天数或周数，例如 7d、2w", raw)
		}
		seconds := n * multiplier
		if seconds > store.RouteMaxWindowSeconds {
			return 0, fmt.Errorf("统计窗口最长 30 天，当前是 %s", raw)
		}
		return seconds, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Second {
		return 0, fmt.Errorf("%q 不是有效时长，例如 90m、24h、7d", raw)
	}
	seconds := int64(d / time.Second)
	if seconds > store.RouteMaxWindowSeconds {
		return 0, fmt.Errorf("统计窗口最长 30 天，当前是 %s", raw)
	}
	return seconds, nil
}

func routeExplainJSON(model string, policy routePolicyJSON, d account.RouteDecision, reason string) map[string]any {
	candidates := make([]map[string]any, 0, len(d.Choices))
	order := make([]string, 0, len(d.Choices))
	for i, c := range d.Choices {
		var probability any
		if c.Probability != nil {
			probability = *c.Probability
		}
		candidates = append(candidates, map[string]any{
			"order":           i + 1,
			"account":         c.Account.Name,
			"id":              c.Account.ID,
			"weight":          c.Weight,
			"attempts":        c.Attempts,
			"success_rate":    c.SuccessRate,
			"latency_samples": c.LatencySamples,
			"avg_latency_ms":  c.AvgLatencyMS,
			"score":           c.Score,
			"probability":     probability,
		})
		order = append(order, c.Account.Name)
	}
	excluded := make([]map[string]any, 0, len(d.Excluded))
	for _, ex := range d.Excluded {
		excluded = append(excluded, map[string]any{
			"account": ex.Account.Name, "id": ex.Account.ID, "reason": ex.Reason,
		})
	}
	return map[string]any{
		"model":             model,
		"policy":            policy,
		"candidates":        candidates,
		"excluded":          excluded,
		"order":             order,
		"sticky_account_id": d.StickyAccountID,
		"sticky_miss":       d.StickyMiss,
		"reason":            reason,
	}
}
