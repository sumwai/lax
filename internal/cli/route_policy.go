package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lax/internal/account"
	"lax/internal/store"
)

// routePolicySubs 是 route policy 一组的子命令清单。
var routePolicySubs = []groupItem{
	{"ls", "按生效顺序列出用户规则，再列内置兜底"},
	{"show", "查看一条用户规则的完整字段"},
	{"add", "新建一条失败处理规则"},
	{"set", "修改一条规则，只改显式给出的字段"},
	{"unset", "清除一条规则里的指定字段"},
	{"rm", "删除一条用户规则"},
	{"enable", "启用一条用户规则"},
	{"disable", "停用一条用户规则，规则保留"},
	{"test", "按给定事实预演匹配，不请求上游、不写库"},
}

const (
	routePolicyMinRetryBase = 100 * time.Millisecond
	routePolicyMaxRetryBase = 10 * time.Second
	routePolicyMaxCooldown  = 30 * 24 * time.Hour
	routePolicyMaxRegexLen  = 512
)

// cmdRoutePolicy 是 route policy 一组的入口。
func cmdRoutePolicy(e *Env, args []string) error {
	if isHelpRequest(args) || len(args) == 0 {
		printGroupHelp(e.Stdout, "route policy", "维护上游失败处理策略", routePolicySubs)
		return nil
	}
	switch args[0] {
	case "ls":
		return cmdRoutePolicyLs(e, args[1:])
	case "show":
		return cmdRoutePolicyShow(e, args[1:])
	case "add":
		return cmdRoutePolicyAdd(e, args[1:])
	case "set":
		return cmdRoutePolicySet(e, args[1:])
	case "unset":
		return cmdRoutePolicyUnset(e, args[1:])
	case "rm":
		return cmdRoutePolicyRm(e, args[1:])
	case "enable":
		return cmdRoutePolicyToggle(e, args[1:], "enable", true)
	case "disable":
		return cmdRoutePolicyToggle(e, args[1:], "disable", false)
	case "test":
		return cmdRoutePolicyTest(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | show | add | set | unset | rm | enable | disable | test\n用 lax route policy --help 查看子命令", args[0])
	}
}

// stringListFlag 是可重复的字符串参数：--status 5xx --status 429 与
// --status 5xx,429 两种写法都收，最终交给 store.ParseStatusTokens 归一。
type stringListFlag struct {
	values []string
}

func (f *stringListFlag) String() string { return strings.Join(f.values, ",") }

func (f *stringListFlag) Set(value string) error {
	f.values = append(f.values, value)
	return nil
}

// routePolicyConditionFlags 是一组规则条件的参数。add 与 set 共用。
type routePolicyConditionFlags struct {
	account   *string
	provider  *string
	model     *string
	statuses  *stringListFlag
	contains  *stringListFlag
	regex     *string
	allErrors *bool
}

func addRoutePolicyConditionFlags(fs *flagSet) *routePolicyConditionFlags {
	f := &routePolicyConditionFlags{
		statuses: &stringListFlag{},
		contains: &stringListFlag{},
	}
	f.account = fs.String("account", "", "只匹配这个账号；写入时解析成账号 id")
	f.provider = fs.String("provider", "", "只匹配这个厂商")
	f.model = fs.String("model", "", "只匹配这个模型；支持别名，写入时解析成真名")
	fs.Var(f.statuses, "status", "`状态`条件，逗号分隔且可重复，例如 5xx,429；收 400–599 与 4xx、5xx")
	fs.Var(f.contains, "error-contains", "`文本`条件，可重复；大小写不敏感，多个值任一命中即可")
	f.regex = fs.String("error-regex", "", "错误文本正则，区分大小写，要忽略大小写写 (?i)；最长 512 字符")
	f.allErrors = fs.Bool("all-errors", false, "匹配范围内所有状态和错误文本；没写状态或错误条件时必须显式给出")
	return f
}

// routePolicyConditionInput 是解析后的条件，写入数据库前还差动作。
type routePolicyConditionInput struct {
	AccountID     int64
	Provider      string
	Model         string
	Statuses      []string
	ErrorContains []string
	ErrorRegex    string
	AllErrors     bool
}

// routePolicyConditionFlagsChanged 判断命令行是否显式给了状态或错误条件。
func routePolicyConditionFlagsChanged(fs *flagSet) bool {
	return fs.Changed("status") || fs.Changed("error-contains") || fs.Changed("error-regex")
}

// resolveRoutePolicyConditions 解析账号、厂商、模型与文本条件。
// 账号与厂商必须已存在；模型别名走 resolveRouteModel；状态一律过 ParseStatusTokens，
// 这样落库的 token 是小写、去重后的规范写法。
func resolveRoutePolicyConditions(a *app, f *routePolicyConditionFlags, fs *flagSet, usage string) (routePolicyConditionInput, error) {
	var in routePolicyConditionInput

	if fs.Changed("account") || strings.TrimSpace(*f.account) != "" {
		name := strings.TrimSpace(*f.account)
		if name == "" {
			return in, usagef("--account 不能是空串；要清掉账号条件用 lax route policy unset <id> --account\n%s", usage)
		}
		acc, err := a.st.GetAccount(name)
		if err != nil {
			return in, fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", name)
		}
		in.AccountID = acc.ID
	}

	if fs.Changed("provider") || strings.TrimSpace(*f.provider) != "" {
		name := strings.TrimSpace(*f.provider)
		if name == "" {
			return in, usagef("--provider 不能是空串；要清掉厂商条件用 lax route policy unset <id> --provider\n%s", usage)
		}
		if _, err := a.st.GetProvider(name); err != nil {
			return in, unknownProviderError(a.st, name)
		}
		in.Provider = name
	}

	if fs.Changed("model") || strings.TrimSpace(*f.model) != "" {
		resolved, err := resolveRouteModel(a.st, *f.model)
		if err != nil {
			return in, err
		}
		in.Model = resolved
	}

	if fs.Changed("status") {
		raw := strings.Join(f.statuses.values, ",")
		if strings.TrimSpace(raw) == "" {
			return in, usagef("--status 不能是空串；要清掉状态条件用 --all-errors 或 lax route policy unset <id> --status\n%s", usage)
		}
		tokens, err := store.ParseStatusTokens(raw)
		if err != nil {
			return in, usagef("--status：%v\n%s", err, usage)
		}
		in.Statuses = tokens
	}

	if fs.Changed("error-contains") {
		values := make([]string, 0, len(f.contains.values))
		for _, raw := range f.contains.values {
			value := strings.TrimSpace(raw)
			if value == "" {
				return in, usagef("--error-contains 不能是空串；要清掉错误文本条件用 --all-errors 或 lax route policy unset <id> --error-contains\n%s", usage)
			}
			values = append(values, value)
		}
		in.ErrorContains = values
	}

	if fs.Changed("error-regex") {
		expr := *f.regex
		if strings.TrimSpace(expr) == "" {
			return in, usagef("--error-regex 不能是空串；要清掉正则条件用 --all-errors 或 lax route policy unset <id> --error-regex\n%s", usage)
		}
		if utf8.RuneCountInString(expr) > routePolicyMaxRegexLen {
			return in, usagef("--error-regex 最长 %d 个字符，当前 %d 个\n%s", routePolicyMaxRegexLen, utf8.RuneCountInString(expr), usage)
		}
		if _, err := regexp.Compile(expr); err != nil {
			return in, usagef("--error-regex 无法编译：%v\n%s", err, usage)
		}
		in.ErrorRegex = expr
	}

	in.AllErrors = *f.allErrors
	return in, nil
}

// routePolicyHasConditions 判断一条规则是否写了具体条件。
func routePolicyHasConditions(p store.RouteFailurePolicy) bool {
	return len(p.Statuses) > 0 || len(p.ErrorContains) > 0 || p.ErrorRegex != ""
}

func validateRoutePolicyRetryFlags(fs *flagSet, retry int, retryBase time.Duration, usage string) error {
	if fs.Changed("retry") && (retry < 1 || retry > account.MaxRetryCount) {
		return usagef("--retry 必须在 1 到 %d 之间，当前是 %d\n%s", account.MaxRetryCount, retry, usage)
	}
	if fs.Changed("retry-base") && (retryBase < routePolicyMinRetryBase || retryBase > routePolicyMaxRetryBase) {
		return usagef("--retry-base 必须在 100ms 到 10s 之间，当前是 %s\n%s", retryBase, usage)
	}
	return nil
}

func validateRoutePolicyCooldownFlag(fs *flagSet, cooldown time.Duration, usage string) error {
	if !fs.Changed("cooldown") {
		return nil
	}
	if cooldown < time.Second || cooldown > routePolicyMaxCooldown {
		return usagef("--cooldown 必须在 1s 到 720h（30 天）之间，当前是 %s\n%s", cooldown, usage)
	}
	// 库里按整秒存。1500ms 这种写法会被截成 1s，实际冷却比用户写的短，先拒绝而不是悄悄改。
	if cooldown%time.Second != 0 {
		return usagef("--cooldown 按整秒存储，请写成整秒（例如 30s、30m、2h），当前是 %s\n%s", cooldown, usage)
	}
	return nil
}

func validateRoutePolicyConditionMode(f *routePolicyConditionFlags, fs *flagSet, usage string) error {
	conditionGiven := routePolicyConditionFlagsChanged(fs)
	if *f.allErrors && conditionGiven {
		return usagef("--all-errors 不能和 --status、--error-contains、--error-regex 同时使用\n%s", usage)
	}
	if !*f.allErrors && !conditionGiven {
		return usagef("没写 --status、--error-contains 或 --error-regex 时必须显式带 --all-errors，免得把正常的 400 也按失败处理\n%s", usage)
	}
	return nil
}

// cmdRoutePolicyAdd 新建一条用户规则。
func cmdRoutePolicyAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy add", "新建一条上游失败处理规则")
	c := addRoutePolicyConditionFlags(fs)
	priority := fs.Int("priority", 100, "匹配优先级，越小越先匹配；同优先级按 id 升序")
	note := fs.String("note", "", "备注，不要写密钥或明文凭据")
	retry := fs.Int("retry", 0, "同一账号同一模型额外重试次数，1 到 10")
	retryBase := fs.Duration("retry-base", account.DefaultRetryBase, "首次重试等待，100ms 到 10s；之后翻倍，单次最多 8s")
	cooldown := fs.Duration("cooldown", 0, "冷却时长，1s 到 30 天；带 --model 时只冷却账号-模型")
	disable := fs.Bool("disable", false, "永久停用整个账号，需要手工 lax account enable 恢复")
	nextRoute := fs.Bool("next_route", false, "处理完重试与状态动作后换下一个候选")
	returnError := fs.Bool("return", false, "把上游错误返回客户端")
	usage := "用法：lax route policy add (--all-errors | --status/--error-contains/--error-regex) [条件 ...] [动作 ...] [--json]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("%s\nadd 不接受位置参数", usage)
	}
	if *priority < 0 {
		return usagef("--priority 不能为负数，当前是 %d\n%s", *priority, usage)
	}
	if err := validateRoutePolicyRetryFlags(fs, *retry, *retryBase, usage); err != nil {
		return err
	}
	if err := validateRoutePolicyCooldownFlag(fs, *cooldown, usage); err != nil {
		return err
	}
	if *disable && *cooldown > 0 {
		return usagef("--disable 与 --cooldown 互斥，只能选一个\n%s", usage)
	}
	if *nextRoute && *returnError {
		return usagef("--next_route 与 --return 互斥，只能选一个\n%s", usage)
	}
	if !fs.Changed("retry") && !fs.Changed("cooldown") && !*disable && !*nextRoute && !*returnError {
		return usagef("至少要有一个动作：--retry、--cooldown、--disable、--next_route 或 --return\n%s", usage)
	}
	if err := validateRoutePolicyConditionMode(c, fs, usage); err != nil {
		return err
	}
	if !*nextRoute && !*returnError {
		*returnError = true
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	cond, err := resolveRoutePolicyConditions(a, c, fs, usage)
	if err != nil {
		return err
	}
	p := store.RouteFailurePolicy{
		Enabled:       true,
		Priority:      *priority,
		AccountID:     cond.AccountID,
		Provider:      cond.Provider,
		Model:         cond.Model,
		Statuses:      cond.Statuses,
		ErrorContains: cond.ErrorContains,
		ErrorRegex:    cond.ErrorRegex,
		RetryCount:    *retry,
		RetryBaseMS:   retryBase.Milliseconds(),
		Disable:       *disable,
		NextRoute:     *nextRoute,
		ReturnError:   *returnError,
		Note:          *note,
	}
	if fs.Changed("cooldown") {
		p.CooldownSeconds = int64(*cooldown / time.Second)
	}
	store.NormalizeRouteFailurePolicy(&p)
	if err := store.ValidateRouteFailurePolicy(p); err != nil {
		return usagef("%v\n%s", err, usage)
	}
	if err := a.st.CreateRouteFailurePolicy(&p); err != nil {
		return err
	}
	// 写入口会归一默认值；重新读一次，打印的就是库里的形状，不拿入参猜。
	saved, err := a.st.GetRouteFailurePolicy(p.ID)
	if err != nil {
		return err
	}
	names, err := routePolicyAccountNames(a.st)
	if err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(routeFailurePolicyToJSON(saved, account.FailureSourceUser, names))
	}
	fmt.Fprintf(e.Stdout, "已新建失败处理策略 #%d\n", saved.ID)
	printRoutePolicyDetail(e, saved, names)
	fmt.Fprintf(e.Stdout, "下一步：lax route policy test --status 400 --explain 预演匹配；lax route policy set %d ... 修改\n", saved.ID)
	return nil
}

// cmdRoutePolicySet 修改一条规则。只改命令行上显式给出的字段。
func cmdRoutePolicySet(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy set", "修改一条规则，只改显式给出的字段")
	c := addRoutePolicyConditionFlags(fs)
	priority := fs.Int("priority", 100, "匹配优先级，越小越先匹配")
	note := fs.String("note", "", "备注，不要写密钥或明文凭据")
	retry := fs.Int("retry", 0, "同一账号同一模型额外重试次数，1 到 10")
	retryBase := fs.Duration("retry-base", account.DefaultRetryBase, "首次重试等待，100ms 到 10s")
	cooldown := fs.Duration("cooldown", 0, "冷却时长，1s 到 30 天；设置它时会清掉 --disable")
	disable := fs.Bool("disable", false, "永久停用整个账号；设置它时会清掉冷却")
	nextRoute := fs.Bool("next_route", false, "换下一个候选；设置它时会清掉 --return")
	returnError := fs.Bool("return", false, "返回上游错误；设置它时会清掉 --next_route")
	usage := "用法：lax route policy set <id> [要修改的字段 ...] [--json]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n要写一个策略 id；清字段用 unset，删整条用 rm", usage)
	}
	id, err := parseRoutePolicyID(fs.Arg(0))
	if err != nil {
		return err
	}
	if *priority < 0 {
		return usagef("--priority 不能为负数，当前是 %d\n%s", *priority, usage)
	}
	if err := validateRoutePolicyRetryFlags(fs, *retry, *retryBase, usage); err != nil {
		return err
	}
	if err := validateRoutePolicyCooldownFlag(fs, *cooldown, usage); err != nil {
		return err
	}
	if *disable && fs.Changed("cooldown") && *cooldown > 0 {
		return usagef("--disable 与 --cooldown 互斥，只能选一个\n%s", usage)
	}
	if fs.Changed("next_route") && fs.Changed("return") && *nextRoute && *returnError {
		return usagef("--next_route 与 --return 互斥，只能选一个\n%s", usage)
	}
	if *c.allErrors && routePolicyConditionFlagsChanged(fs) {
		return usagef("--all-errors 不能和 --status、--error-contains、--error-regex 同时使用\n%s", usage)
	}
	changed := fs.Changed("account") || fs.Changed("provider") || fs.Changed("model") ||
		routePolicyConditionFlagsChanged(fs) || fs.Changed("priority") || fs.Changed("note") ||
		fs.Changed("retry") || fs.Changed("retry-base") || fs.Changed("cooldown") ||
		(*disable && fs.Changed("disable")) || (*nextRoute && fs.Changed("next_route")) ||
		(*returnError && fs.Changed("return")) || *c.allErrors
	if !changed {
		return usagef("没有指定要修改的字段\n%s\n清字段用 lax route policy unset <id> ...", usage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	p, err := getRoutePolicy(a.st, id)
	if err != nil {
		return err
	}
	cond, err := resolveRoutePolicyConditions(a, c, fs, usage)
	if err != nil {
		return err
	}
	if fs.Changed("account") {
		p.AccountID = cond.AccountID
	}
	if fs.Changed("provider") {
		p.Provider = cond.Provider
	}
	if fs.Changed("model") {
		p.Model = cond.Model
	}
	if fs.Changed("status") {
		p.Statuses = cond.Statuses
	}
	if fs.Changed("error-contains") {
		p.ErrorContains = cond.ErrorContains
	}
	if fs.Changed("error-regex") {
		p.ErrorRegex = cond.ErrorRegex
	}
	if *c.allErrors {
		p.Statuses = nil
		p.ErrorContains = nil
		p.ErrorRegex = ""
	}
	if fs.Changed("priority") {
		p.Priority = *priority
	}
	if fs.Changed("note") {
		p.Note = *note
	}
	if fs.Changed("retry") {
		p.RetryCount = *retry
	}
	if fs.Changed("retry-base") {
		p.RetryBaseMS = retryBase.Milliseconds()
	}
	if fs.Changed("cooldown") {
		p.CooldownSeconds = int64(*cooldown / time.Second)
		p.Disable = false
	}
	if fs.Changed("disable") && *disable {
		p.Disable = true
		p.CooldownSeconds = 0
	}
	if fs.Changed("next_route") && *nextRoute {
		p.NextRoute = true
		p.ReturnError = false
	}
	if fs.Changed("return") && *returnError {
		p.ReturnError = true
		p.NextRoute = false
	}
	conditionsTouched := routePolicyConditionFlagsChanged(fs) || *c.allErrors
	if conditionsTouched && !*c.allErrors && !routePolicyHasConditions(p) {
		return usagef("修改后没有任何状态或错误条件；要匹配所有错误请同时带 --all-errors\n%s", usage)
	}

	store.NormalizeRouteFailurePolicy(&p)
	if err := store.ValidateRouteFailurePolicy(p); err != nil {
		return usagef("%v\n%s", err, usage)
	}
	if err := a.st.UpdateRouteFailurePolicy(p); err != nil {
		if errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
			return routePolicyNotFound(id)
		}
		return err
	}
	saved, err := getRoutePolicy(a.st, id)
	if err != nil {
		return err
	}
	names, err := routePolicyAccountNames(a.st)
	if err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(routeFailurePolicyToJSON(saved, account.FailureSourceUser, names))
	}
	fmt.Fprintf(e.Stdout, "已更新失败处理策略 #%d\n", saved.ID)
	printRoutePolicyDetail(e, saved, names)
	return nil
}

// cmdRoutePolicyUnset 清除一条规则里的指定字段。字段用开关表示，不接受空串当删除信号。
func cmdRoutePolicyUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy unset", "清除一条规则里的指定字段")
	accountFlag := fs.Bool("account", false, "清除账号条件")
	providerFlag := fs.Bool("provider", false, "清除厂商条件")
	modelFlag := fs.Bool("model", false, "清除模型条件")
	statusFlag := fs.Bool("status", false, "清除状态条件")
	containsFlag := fs.Bool("error-contains", false, "清除错误文本条件")
	regexFlag := fs.Bool("error-regex", false, "清除错误正则条件")
	priorityFlag := fs.Bool("priority", false, "恢复默认优先级 100")
	noteFlag := fs.Bool("note", false, "清除备注")
	retryFlag := fs.Bool("retry", false, "清除重试次数")
	retryBaseFlag := fs.Bool("retry-base", false, "恢复默认重试起始等待 500ms")
	cooldownFlag := fs.Bool("cooldown", false, "清除冷却")
	disableFlag := fs.Bool("disable", false, "清除停用动作")
	allErrors := fs.Bool("all-errors", false, "清空所有状态与错误条件后，确认改按“所有错误”匹配")
	usage := "用法：lax route policy unset <id> <要清掉的字段开关 ...> [--all-errors] [--json]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n要写一个策略 id；删整条规则用 lax route policy rm <id>", usage)
	}
	id, err := parseRoutePolicyID(fs.Arg(0))
	if err != nil {
		return err
	}
	any := *accountFlag || *providerFlag || *modelFlag || *statusFlag || *containsFlag ||
		*regexFlag || *priorityFlag || *noteFlag || *retryFlag || *retryBaseFlag ||
		*cooldownFlag || *disableFlag
	if !any {
		return usagef("没有指定要清除的字段；要删整条规则用 lax route policy rm <id>\n%s", usage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	p, err := getRoutePolicy(a.st, id)
	if err != nil {
		return err
	}
	hadConditions := routePolicyHasConditions(p)
	conditionsCleared := false
	if *accountFlag {
		p.AccountID = 0
	}
	if *providerFlag {
		p.Provider = ""
	}
	if *modelFlag {
		p.Model = ""
	}
	if *statusFlag {
		p.Statuses = nil
		conditionsCleared = true
	}
	if *containsFlag {
		p.ErrorContains = nil
		conditionsCleared = true
	}
	if *regexFlag {
		p.ErrorRegex = ""
		conditionsCleared = true
	}
	if *priorityFlag {
		p.Priority = 100
	}
	if *noteFlag {
		p.Note = ""
	}
	if *retryFlag {
		p.RetryCount = 0
	}
	if *retryBaseFlag {
		p.RetryBaseMS = 0 // 写入口会归一成 500ms
	}
	if *cooldownFlag {
		p.CooldownSeconds = 0
	}
	if *disableFlag {
		p.Disable = false
	}
	if conditionsCleared && hadConditions && !routePolicyHasConditions(p) && !*allErrors {
		return usagef("清空后没有任何状态或错误条件；要保留一条匹配所有错误的规则，请同时带 --all-errors\n%s", usage)
	}

	store.NormalizeRouteFailurePolicy(&p)
	if err := store.ValidateRouteFailurePolicy(p); err != nil {
		return usagef("%v\n%s", err, usage)
	}
	if err := a.st.UpdateRouteFailurePolicy(p); err != nil {
		if errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
			return routePolicyNotFound(id)
		}
		return err
	}
	saved, err := getRoutePolicy(a.st, id)
	if err != nil {
		return err
	}
	names, err := routePolicyAccountNames(a.st)
	if err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(routeFailurePolicyToJSON(saved, account.FailureSourceUser, names))
	}
	fmt.Fprintf(e.Stdout, "已清除失败处理策略 #%d 的指定字段\n", saved.ID)
	printRoutePolicyDetail(e, saved, names)
	return nil
}

// cmdRoutePolicyLs 列出用户规则与内置兜底。
func cmdRoutePolicyLs(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy ls", "按生效顺序列出用户规则，再列内置兜底")
	all := fs.Bool("all", false, "同时列出已停用的用户规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("用法：lax route policy ls [--all] [--json]")
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	var policies []store.RouteFailurePolicy
	if *all {
		policies, err = a.st.ListAllRouteFailurePolicies()
	} else {
		policies, err = a.st.ListRouteFailurePolicies()
	}
	if err != nil {
		return err
	}
	names, err := routePolicyAccountNames(a.st)
	if err != nil {
		return err
	}
	builtins := account.BuiltinFailurePolicies()

	if e.JSON {
		out := routeFailurePolicyListJSON{
			Policies: make([]routeFailurePolicyJSON, 0, len(policies)),
			Builtins: make([]routeFailurePolicyJSON, 0, len(builtins)),
		}
		for _, p := range policies {
			out.Policies = append(out.Policies, routeFailurePolicyToJSON(p, account.FailureSourceUser, names))
		}
		for _, p := range builtins {
			out.Builtins = append(out.Builtins, routeFailurePolicyToJSON(p, account.FailureSourceBuiltin, names))
		}
		return e.printJSON(out)
	}

	rows := make([][]string, 0, len(policies)+len(builtins))
	for _, p := range policies {
		rows = append(rows, []string{
			strconv.FormatInt(p.ID, 10),
			routePolicySourceText(account.FailureSourceUser),
			boolText(p.Enabled),
			strconv.Itoa(p.Priority),
			routePolicyConditionSummary(p, names),
			routePolicyActionSummary(p),
			routePolicyNoteCell(p.Note),
		})
	}
	for _, p := range builtins {
		rows = append(rows, []string{
			"-",
			routePolicySourceText(account.FailureSourceBuiltin),
			boolText(p.Enabled),
			"-",
			routePolicyConditionSummary(p, names),
			routePolicyActionSummary(p),
			routePolicyNoteCell(p.Note),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"ID", "来源", "启用", "优先级", "条件", "动作", "备注"}, rows))
	if len(policies) == 0 {
		fmt.Fprintln(e.Stdout, "还没有用户规则；内置兜底始终生效。")
	}
	fmt.Fprintln(e.Stdout, "修改用户规则：lax route policy set <id> ...；试跑匹配：lax route policy test --status 400 --explain")
	return nil
}

// cmdRoutePolicyShow 查看一条用户规则的完整字段。
func cmdRoutePolicyShow(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy show", "查看一条用户规则的完整字段")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax route policy show <id> [--json]")
	}
	if strings.TrimSpace(fs.Arg(0)) == "0" {
		return usagef("内置兜底规则没有 id，不能按 id 查看；用 lax route policy ls 看完整列表，或用 lax route policy test --explain 看匹配过程")
	}
	id, err := parseRoutePolicyID(fs.Arg(0))
	if err != nil {
		return err
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	p, err := getRoutePolicy(a.st, id)
	if err != nil {
		return err
	}
	names, err := routePolicyAccountNames(a.st)
	if err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(routeFailurePolicyToJSON(p, account.FailureSourceUser, names))
	}
	printRoutePolicyDetail(e, p, names)
	fmt.Fprintf(e.Stdout, "修改：lax route policy set %d ...；清字段：lax route policy unset %d ...\n", p.ID, p.ID)
	return nil
}

// cmdRoutePolicyRm 删除一条用户规则。内置兜底不在库里，删不掉。
func cmdRoutePolicyRm(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy rm", "删除一条用户规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax route policy rm <id>")
	}
	if strings.TrimSpace(fs.Arg(0)) == "0" {
		return fmt.Errorf("内置兜底规则不能删除；用 lax route policy ls 查看用户规则")
	}
	id, err := parseRoutePolicyID(fs.Arg(0))
	if err != nil {
		return err
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.DeleteRouteFailurePolicy(id); err != nil {
		if errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
			return routePolicyNotFound(id)
		}
		return err
	}
	if e.JSON {
		return e.printJSON(map[string]any{"id": id, "removed": true})
	}
	fmt.Fprintf(e.Stdout, "已删除失败处理策略 #%d\n", id)
	return nil
}

// cmdRoutePolicyToggle 启用或停用一条用户规则。
func cmdRoutePolicyToggle(e *Env, args []string, sub string, enabled bool) error {
	fs := newFlagSet(e, "route policy "+sub, "启用或停用一条用户规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax route policy %s <id>", sub)
	}
	if strings.TrimSpace(fs.Arg(0)) == "0" {
		return fmt.Errorf("内置兜底规则不能启停；用 lax route policy ls 查看用户规则")
	}
	id, err := parseRoutePolicyID(fs.Arg(0))
	if err != nil {
		return err
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.SetRouteFailurePolicyEnabled(id, enabled); err != nil {
		if errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
			return routePolicyNotFound(id)
		}
		return err
	}
	word := "已停用"
	if enabled {
		word = "已启用"
	}
	if e.JSON {
		return e.printJSON(map[string]any{"id": id, "enabled": enabled})
	}
	if enabled {
		fmt.Fprintf(e.Stdout, "%s失败处理策略 #%d，它会重新参与匹配\n", word, id)
	} else {
		fmt.Fprintf(e.Stdout, "%s失败处理策略 #%d，规则保留但不再参与匹配\n", word, id)
	}
	return nil
}

// routePolicyExplainEntry 是 test --explain 的一行。
type routePolicyExplainEntry struct {
	Source   string `json:"source"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
	Matched  bool   `json:"matched"`
	Reason   string `json:"reason"`
}

// cmdRoutePolicyTest 按给定事实预演匹配。这里只读数据库，绝不请求上游、绝不写库。
func cmdRoutePolicyTest(e *Env, args []string) error {
	fs := newFlagSet(e, "route policy test", "按给定事实预演匹配，不请求上游、不写库")
	statusRaw := fs.String("status", "", "上游状态码，400 到 599；例如 400、429、503")
	message := fs.String("message", "", "上游错误文本，按网关已脱敏并截断后的形状给")
	provider := fs.String("provider", "", "厂商名；只校验存在，不写库")
	model := fs.String("model", "", "模型名；支持别名")
	accountName := fs.String("account", "", "账号名；只校验存在，不写库")
	explain := fs.Bool("explain", false, "逐条列出用户规则与内置兜底的命中或跳过原因")
	usage := "用法：lax route policy test --status <码> [--message <文本>] [--provider <厂商>] [--model <模型>] [--account <账号>] [--explain] [--json]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("%s\ntest 不接受位置参数", usage)
	}
	if strings.TrimSpace(*statusRaw) == "" {
		return usagef("--status 必填，例如 --status 400 或 --status 503\n%s", usage)
	}
	status, err := strconv.Atoi(strings.TrimSpace(*statusRaw))
	if err != nil || status < 400 || status > 599 {
		return usagef("--status 需要一个 400 到 599 的具体状态码；4xx、5xx 只能写在规则条件里\n%s", usage)
	}
	if fs.Changed("provider") && strings.TrimSpace(*provider) == "" {
		return usagef("--provider 不能是空串\n%s", usage)
	}
	if fs.Changed("model") && strings.TrimSpace(*model) == "" {
		return usagef("--model 不能是空串\n%s", usage)
	}
	if fs.Changed("account") && strings.TrimSpace(*accountName) == "" {
		return usagef("--account 不能是空串\n%s", usage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	facts := account.FailureFacts{Status: status, Message: *message}
	canonicalAccount := ""
	if strings.TrimSpace(*accountName) != "" {
		name := strings.TrimSpace(*accountName)
		acc, err := a.st.GetAccount(name)
		if err != nil {
			return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", name)
		}
		facts.AccountID = acc.ID
		canonicalAccount = acc.Name
	}
	if strings.TrimSpace(*provider) != "" {
		name := strings.TrimSpace(*provider)
		if _, err := a.st.GetProvider(name); err != nil {
			return unknownProviderError(a.st, name)
		}
		facts.Provider = name
	}
	if strings.TrimSpace(*model) != "" {
		resolved, err := resolveRouteModel(a.st, *model)
		if err != nil {
			return err
		}
		facts.Model = resolved
	}

	all, err := a.st.ListAllRouteFailurePolicies()
	if err != nil {
		return err
	}
	names, err := routePolicyAccountNames(a.st)
	if err != nil {
		return err
	}
	enabled := make([]store.RouteFailurePolicy, 0, len(all))
	for _, p := range all {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	builtins := account.BuiltinFailurePolicies()
	decision, matched := account.MatchFailure(facts, enabled)

	explainEntries := make([]routePolicyExplainEntry, 0, len(all)+len(builtins))
	for _, p := range all {
		ok, reason := routePolicyExplain(p, account.FailureSourceUser, facts, names)
		explainEntries = append(explainEntries, routePolicyExplainEntry{
			Source: account.FailureSourceUser, ID: p.ID,
			Name:    routePolicyName(account.FailureSourceUser, p.ID, p.Note),
			Enabled: p.Enabled, Priority: p.Priority, Matched: ok, Reason: reason,
		})
	}
	for _, p := range builtins {
		ok, reason := routePolicyExplain(p, account.FailureSourceBuiltin, facts, names)
		explainEntries = append(explainEntries, routePolicyExplainEntry{
			Source: account.FailureSourceBuiltin, ID: p.ID,
			Name:    routePolicyName(account.FailureSourceBuiltin, p.ID, p.Note),
			Enabled: p.Enabled, Priority: p.Priority, Matched: ok, Reason: reason,
		})
	}

	if e.JSON {
		var matchedJSON any
		reason := "没有规则命中，按旧行为处理"
		if matched {
			matchedJSON = routePolicyDecisionJSON(decision)
			reason = "命中"
		}
		return e.printJSON(map[string]any{
			"status":     status,
			"message":    *message,
			"provider":   facts.Provider,
			"model":      facts.Model,
			"account":    canonicalAccount,
			"account_id": facts.AccountID,
			"matched":    matchedJSON,
			"reason":     reason,
			"explain":    explainEntries,
		})
	}

	fmt.Fprintf(e.Stdout, "%s\n", routePolicyFactsText(facts, canonicalAccount))
	if *explain {
		fmt.Fprintln(e.Stdout, "匹配过程（按生效顺序）：")
		for _, entry := range explainEntries {
			fmt.Fprintf(e.Stdout, "  %s：%s（%s）\n",
				routePolicyDisplay(entry.Source, entry.ID, entry.Name),
				routePolicyMatchedText(entry.Matched), entry.Reason)
		}
	}
	if matched {
		printRoutePolicyDecision(e, decision)
	} else {
		fmt.Fprintln(e.Stdout, "没有规则命中，按旧行为处理。")
		fmt.Fprintln(e.Stdout, routePolicyOldBehaviorText(status))
	}
	return nil
}

// routePolicyDecisionJSON 是 test --json 里命中的决策与动作。
func routePolicyDecisionJSON(d account.FailureDecision) map[string]any {
	return map[string]any{
		"source":                d.Source,
		"id":                    d.PolicyID,
		"name":                  routePolicyName(d.Source, d.PolicyID, d.Name),
		"retry":                 d.RetryCount,
		"retry_base_ms":         d.RetryBase.Milliseconds(),
		"cooldown_seconds":      int64(d.Cooldown / time.Second),
		"disable":               d.Disable,
		"next_route":            d.NextRoute,
		"return_error":          d.ReturnError,
		"model_scoped_cooldown": d.ModelScopedCooldown,
	}
}

func printRoutePolicyDecision(e *Env, d account.FailureDecision) {
	fmt.Fprintf(e.Stdout, "命中：%s\n", routePolicyDisplay(d.Source, d.PolicyID, d.Name))
	fmt.Fprintf(e.Stdout, "动作：%s\n", routePolicyDecisionActionSummary(d))
	if d.RetryCount > 0 {
		delays := make([]string, 0, d.RetryCount)
		for i := 1; i <= d.RetryCount; i++ {
			delays = append(delays, routePolicyDurationText(account.RetryDelay(d.RetryBase, i)))
		}
		fmt.Fprintf(e.Stdout, "重试：额外 %d 次，等待 %s\n", d.RetryCount, strings.Join(delays, "、"))
	} else {
		fmt.Fprintln(e.Stdout, "重试：不重试")
	}
	if d.Cooldown > 0 {
		scope := "账号级"
		if d.ModelScopedCooldown {
			scope = "模型级"
		}
		fmt.Fprintf(e.Stdout, "冷却：%s %s\n", scope, routePolicyDurationText(d.Cooldown))
	} else {
		fmt.Fprintln(e.Stdout, "冷却：无")
	}
	if d.Disable {
		fmt.Fprintln(e.Stdout, "停用：永久停用整个账号，需要手工 lax account enable 恢复")
	}
	flow := "返回上游错误"
	if d.NextRoute {
		flow = "换下一个候选账号"
	}
	fmt.Fprintf(e.Stdout, "流程：%s\n", flow)
}

func routePolicyFactsText(facts account.FailureFacts, accountName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "测试事实：状态 %d", facts.Status)
	if facts.Provider != "" {
		fmt.Fprintf(&b, " / 厂商 %s", facts.Provider)
	}
	if facts.Model != "" {
		fmt.Fprintf(&b, " / 模型 %s", facts.Model)
	}
	if accountName != "" {
		fmt.Fprintf(&b, " / 账号 %s", accountName)
	}
	if facts.Message != "" {
		fmt.Fprintf(&b, " / 错误文本 %q", facts.Message)
	}
	return b.String()
}

func routePolicyOldBehaviorText(status int) string {
	switch {
	case status == 408 || status == 429 || (status >= 500 && status <= 599):
		return "旧行为：按账号-模型冷却后换下一个候选账号。"
	case status == 401:
		return "旧行为：停用账号并返回上游错误。"
	default:
		return "旧行为：原样返回上游错误，不改账号状态。"
	}
}

func routePolicyMatchedText(matched bool) string {
	if matched {
		return "命中"
	}
	return "跳过"
}

// routePolicyExplain 复刻 account 的匹配顺序，给 --explain 生成逐条原因。
func routePolicyExplain(p store.RouteFailurePolicy, source string, facts account.FailureFacts, names map[int64]string) (bool, string) {
	if !p.Enabled {
		return false, "规则已停用"
	}
	if p.AccountID != 0 && p.AccountID != facts.AccountID {
		return false, fmt.Sprintf("账号不匹配（规则限 %s）", routePolicyAccountText(p.AccountID, names))
	}
	if p.Provider != "" && p.Provider != facts.Provider {
		return false, fmt.Sprintf("厂商不匹配（规则限 %s）", p.Provider)
	}
	if p.Model != "" && p.Model != facts.Model {
		return false, fmt.Sprintf("模型不匹配（规则限 %s）", p.Model)
	}
	if !store.StatusTokenMatches(p.Statuses, facts.Status) {
		return false, fmt.Sprintf("状态不匹配（规则限 %s）", strings.Join(p.Statuses, "、"))
	}
	if len(p.ErrorContains) > 0 {
		message := strings.ToLower(facts.Message)
		found := false
		for _, want := range p.ErrorContains {
			if strings.Contains(message, strings.ToLower(want)) {
				found = true
				break
			}
		}
		if !found {
			return false, "错误文本不含任一关键词"
		}
	}
	if p.ErrorRegex != "" {
		re, err := regexp.Compile(p.ErrorRegex)
		if err != nil || !re.MatchString(facts.Message) {
			return false, "错误文本不匹配正则"
		}
	}
	return true, "命中"
}

// routeFailurePolicyJSON 是 --json 的单条规则形状。字段名与 store 字段一一对应。
type routeFailurePolicyJSON struct {
	ID              int64    `json:"id"`
	Source          string   `json:"source"`
	Enabled         bool     `json:"enabled"`
	Priority        int      `json:"priority"`
	AccountID       int64    `json:"account_id"`
	Account         string   `json:"account"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Statuses        []string `json:"statuses"`
	ErrorContains   []string `json:"error_contains"`
	ErrorRegex      string   `json:"error_regex"`
	Retry           int      `json:"retry"`
	RetryBaseMS     int64    `json:"retry_base_ms"`
	CooldownSeconds int64    `json:"cooldown_seconds"`
	Disable         bool     `json:"disable"`
	NextRoute       bool     `json:"next_route"`
	ReturnError     bool     `json:"return_error"`
	Note            string   `json:"note"`
	CreatedAt       int64    `json:"created_at"`
	UpdatedAt       int64    `json:"updated_at"`
}

// routeFailurePolicyListJSON 是 ls --json 的形状：用户规则与内置兜底分两段。
type routeFailurePolicyListJSON struct {
	Policies []routeFailurePolicyJSON `json:"policies"`
	Builtins []routeFailurePolicyJSON `json:"builtins"`
}

func routeFailurePolicyToJSON(p store.RouteFailurePolicy, source string, names map[int64]string) routeFailurePolicyJSON {
	statuses := append([]string{}, p.Statuses...)
	contains := append([]string{}, p.ErrorContains...)
	accountName := ""
	if p.AccountID != 0 {
		accountName = routePolicyAccountText(p.AccountID, names)
	}
	return routeFailurePolicyJSON{
		ID:              p.ID,
		Source:          source,
		Enabled:         p.Enabled,
		Priority:        p.Priority,
		AccountID:       p.AccountID,
		Account:         accountName,
		Provider:        p.Provider,
		Model:           p.Model,
		Statuses:        statuses,
		ErrorContains:   contains,
		ErrorRegex:      p.ErrorRegex,
		Retry:           p.RetryCount,
		RetryBaseMS:     p.RetryBaseMS,
		CooldownSeconds: p.CooldownSeconds,
		Disable:         p.Disable,
		NextRoute:       p.NextRoute,
		ReturnError:     p.ReturnError,
		Note:            p.Note,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
}

// printRoutePolicyDetail 打印一条规则的完整字段。
func printRoutePolicyDetail(e *Env, p store.RouteFailurePolicy, names map[int64]string) {
	rows := [][]string{
		{"ID", strconv.FormatInt(p.ID, 10)},
		{"来源", routePolicySourceText(account.FailureSourceUser)},
		{"启用", boolText(p.Enabled)},
		{"优先级", strconv.Itoa(p.Priority)},
		{"账号", routePolicyAccountText(p.AccountID, names)},
		{"厂商", routePolicyAnyText(p.Provider)},
		{"模型", routePolicyAnyText(p.Model)},
		{"状态", routePolicyStatusesText(p.Statuses)},
		{"错误包含", routePolicyContainsText(p.ErrorContains)},
		{"错误正则", routePolicyAnyText(p.ErrorRegex)},
		{"重试", routePolicyRetryText(p)},
		{"重试起始等待", routePolicyDurationText(time.Duration(p.RetryBaseMS) * time.Millisecond)},
		{"冷却", routePolicyCooldownText(p)},
		{"停用账号", boolText(p.Disable)},
		{"流程", routePolicyFlowText(p)},
		{"备注", routePolicyAnyText(p.Note)},
		{"创建时间", routePolicyTimeText(p.CreatedAt)},
		{"更新时间", routePolicyTimeText(p.UpdatedAt)},
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"项", "值"}, rows))
}

func routePolicyConditionSummary(p store.RouteFailurePolicy, names map[int64]string) string {
	var parts []string
	if p.AccountID != 0 {
		parts = append(parts, "账号 "+routePolicyAccountText(p.AccountID, names))
	}
	if p.Provider != "" {
		parts = append(parts, "厂商 "+p.Provider)
	}
	if p.Model != "" {
		parts = append(parts, "模型 "+p.Model)
	}
	if len(p.Statuses) > 0 {
		parts = append(parts, "状态 "+strings.Join(p.Statuses, ","))
	}
	if len(p.ErrorContains) > 0 {
		switch len(p.ErrorContains) {
		case 1:
			parts = append(parts, "错误含 "+p.ErrorContains[0])
		case 2:
			parts = append(parts, "错误含 "+p.ErrorContains[0]+" 或 "+p.ErrorContains[1])
		default:
			parts = append(parts, fmt.Sprintf("错误含 %d 条关键词", len(p.ErrorContains)))
		}
	}
	if p.ErrorRegex != "" {
		parts = append(parts, "错误匹配 /"+truncate(p.ErrorRegex, 30)+"/")
	}
	if len(parts) == 0 {
		return "全部错误"
	}
	return strings.Join(parts, " / ")
}

func routePolicyActionSummary(p store.RouteFailurePolicy) string {
	var parts []string
	if p.RetryCount > 0 {
		part := fmt.Sprintf("重试 %d 次", p.RetryCount)
		if p.RetryBaseMS != account.DefaultRetryBase.Milliseconds() {
			part += "（" + routePolicyDurationText(time.Duration(p.RetryBaseMS)*time.Millisecond) + " 起）"
		}
		parts = append(parts, part)
	}
	if p.CooldownSeconds > 0 {
		scope := "冷却账号"
		if p.Model != "" {
			scope = "冷却模型"
		}
		parts = append(parts, scope+" "+routePolicyDurationText(time.Duration(p.CooldownSeconds)*time.Second))
	}
	if p.Disable {
		parts = append(parts, "停用账号")
	}
	if p.NextRoute {
		parts = append(parts, "换号")
	}
	if p.ReturnError {
		parts = append(parts, "返回")
	}
	if len(parts) == 0 {
		return "无动作"
	}
	return strings.Join(parts, " + ")
}

func routePolicyDecisionActionSummary(d account.FailureDecision) string {
	var parts []string
	if d.RetryCount > 0 {
		parts = append(parts, fmt.Sprintf("重试 %d 次", d.RetryCount))
	}
	if d.Cooldown > 0 {
		scope := "冷却账号"
		if d.ModelScopedCooldown {
			scope = "冷却模型"
		}
		parts = append(parts, scope+" "+routePolicyDurationText(d.Cooldown))
	}
	if d.Disable {
		parts = append(parts, "停用账号")
	}
	if d.NextRoute {
		parts = append(parts, "换号")
	}
	if d.ReturnError {
		parts = append(parts, "返回")
	}
	if len(parts) == 0 {
		return "不改状态"
	}
	return strings.Join(parts, " + ")
}

func routePolicyStatusesText(statuses []string) string {
	if len(statuses) == 0 {
		return "所有状态（--all-errors）"
	}
	return strings.Join(statuses, "、")
}

func routePolicyContainsText(values []string) string {
	if len(values) == 0 {
		return "不限"
	}
	return strings.Join(values, " 或 ")
}

func routePolicyRetryText(p store.RouteFailurePolicy) string {
	if p.RetryCount == 0 {
		return "不重试"
	}
	return fmt.Sprintf("额外 %d 次", p.RetryCount)
}

func routePolicyCooldownText(p store.RouteFailurePolicy) string {
	if p.CooldownSeconds == 0 {
		return "无"
	}
	scope := "账号级"
	if p.Model != "" {
		scope = "模型级"
	}
	return scope + " " + routePolicyDurationText(time.Duration(p.CooldownSeconds)*time.Second)
}

func routePolicyFlowText(p store.RouteFailurePolicy) string {
	if p.NextRoute {
		return "换下一个候选账号"
	}
	if p.ReturnError {
		return "返回上游错误"
	}
	return "未设置"
}

func routePolicyAccountText(id int64, names map[int64]string) string {
	if id == 0 {
		return "不限"
	}
	if name := names[id]; name != "" {
		return name
	}
	return fmt.Sprintf("#%d", id)
}

func routePolicyAnyText(value string) string {
	if value == "" {
		return "不限"
	}
	return value
}

func routePolicyNoteCell(note string) string {
	if strings.TrimSpace(note) == "" {
		return "-"
	}
	return truncate(note, 40)
}

func routePolicySourceText(source string) string {
	if source == account.FailureSourceBuiltin {
		return "内置"
	}
	return "用户"
}

// routePolicyName 是规则名：有备注用备注，没有就用来源与 id 兜底。
func routePolicyName(source string, id int64, note string) string {
	if strings.TrimSpace(note) != "" {
		return note
	}
	if source == account.FailureSourceBuiltin {
		return "内置失败处理规则"
	}
	return fmt.Sprintf("用户规则 #%d", id)
}

// routePolicyDisplay 是给人读的完整称呼。
func routePolicyDisplay(source string, id int64, note string) string {
	if source == account.FailureSourceBuiltin {
		return routePolicyName(source, id, note)
	}
	name := strings.TrimSpace(note)
	// explain 的 Name 在没有备注时已经是「用户规则 #id」，不要再套一层括号。
	if name != "" && name != fmt.Sprintf("用户规则 #%d", id) {
		return fmt.Sprintf("用户规则 #%d（%s）", id, name)
	}
	return fmt.Sprintf("用户规则 #%d", id)
}

func boolText(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func routePolicyTimeText(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	return time.Unix(seconds, 0).Local().Format("2006-01-02 15:04:05")
}

// routePolicyDurationText 把时长写成人读的 500ms / 30m / 1h / 2d。
func routePolicyDurationText(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d%time.Millisecond == 0:
		return fmt.Sprintf("%dms", int(d/time.Millisecond))
	default:
		return d.String()
	}
}

// routePolicyRemainingText 是账号级冷却的剩余时长，向上取整到秒。
func routePolicyRemainingText(untilMS int64, now time.Time) string {
	d := time.Duration(untilMS-now.UnixMilli()) * time.Millisecond
	if d < time.Second {
		d = time.Second
	}
	// 向上取整到秒：还剩 1.4 秒写「2s」，写「1s」会让人以为马上就要解冻。
	d = ((d + time.Second - 1) / time.Second) * time.Second
	return routePolicyDurationText(d)
}

func parseRoutePolicyID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, usagef("策略 id 必须是正整数，当前是 %q；用 lax route policy ls 查看现有规则", raw)
	}
	return id, nil
}

func getRoutePolicy(st *store.Store, id int64) (store.RouteFailurePolicy, error) {
	p, err := st.GetRouteFailurePolicy(id)
	if errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
		return p, routePolicyNotFound(id)
	}
	return p, err
}

func routePolicyNotFound(id int64) error {
	return fmt.Errorf("失败处理策略 #%d 不存在；用 lax route policy ls 查看现有规则", id)
}

func routePolicyAccountNames(st *store.Store) (map[int64]string, error) {
	accounts, err := st.ListAccounts()
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(accounts))
	for _, acc := range accounts {
		names[acc.ID] = acc.Name
	}
	return names, nil
}
