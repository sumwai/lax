package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"switchboard/internal/store"
)

// model alias 三条子命令只做命令面：参数个数与空值在这里判（退出码 2），
// 遮蔽真名、成环与超过跳数上限由 store 判并把下一步写在错误里，cli 原样带出（退出码 1）。
// 解析走 store.ResolveModelAlias 这一处，cli 不自己走链、也不另写一份回退规则（见 DESIGN 5.12）。

// modelAliasSubs 是 model alias 一组的子命令清单，组级帮助与错误提示都读它。
var modelAliasSubs = []groupItem{
	{"set", "把客户端用的名字指向真实模型名（同名再写一次就是改目标）"},
	{"unset", "删掉一条别名"},
	{"ls", "列别名、目标与能否解析（不给别名就列全部）"},
}

// 三份用法行，所有用法错误都把它打出来。
const (
	modelAliasSetUsage   = "用法：switchboard model alias set <别名> <目标>"
	modelAliasUnsetUsage = "用法：switchboard model alias unset <别名>"
	modelAliasLsUsage    = "用法：switchboard model alias ls [<别名>]"
	modelAliasUsageText  = "用法：\n" +
		"  switchboard model alias set <别名> <目标>\n" +
		"  switchboard model alias unset <别名>\n" +
		"  switchboard model alias ls [<别名>]"
)

// modelAliasJSON 是 alias ls 的一项，也是 set 的 --json 输出。
// Resolved 是解析到的真实模型名，链走不通时是空串；走不通的原因（带上是哪几条串在一起）
// 只出现在人读输出的解析列里，--json 靠 ok=false 表示，不再多一个错误字段。
type modelAliasJSON struct {
	Alias    string `json:"alias"`
	Target   string `json:"target"`
	Resolved string `json:"resolved"`
	OK       bool   `json:"ok"`
}

// modelAliasRow 是一条别名连同它的解析结果，文本与 --json 两条输出都从它来。
type modelAliasRow struct {
	alias  string
	target string
	// resolveErr 是链走不通的原因（store 的原文）；非 nil 时这条别名用不了。
	resolveErr error
	resolved   string
}

func (r modelAliasRow) ok() bool { return r.resolveErr == nil }

func (r modelAliasRow) toJSON() modelAliasJSON {
	return modelAliasJSON{Alias: r.alias, Target: r.target, Resolved: r.resolved, OK: r.ok()}
}

// printModelAliasHelp 是 `model alias --help` 的输出：三条用法、一个示例，
// 以及「只影响请求方向」「请求记录里两个模型名会不同」两段说明（见 DESIGN 5.12）。
// 光列子命令说不清别名改的是什么、不改的是什么，所以不走 printGroupHelp。
func printModelAliasHelp(w io.Writer) {
	var b strings.Builder
	b.WriteString(appName + " model alias — 客户端写死的模型名指到一个真实模型名上\n\n")
	b.WriteString(modelAliasUsageText + "\n\n")
	b.WriteString("别名只影响请求方向：网关按解析出的真实名字选号、改请求体与路径，响应不重写，\n")
	b.WriteString("响应里的模型名仍是上游的真实名字。\n")
	b.WriteString("客户端请求别名时，请求记录里的请求模型名与使用模型名会不同：log tail 的模型列\n")
	b.WriteString("写成「别名 → 真名」，一次别名命中因此一眼能看出实际打了哪个模型。\n\n")
	b.WriteString("别名精确匹配、区分大小写，可以指向另一个别名（链最长 8 跳）；名字已经被某个账号\n")
	b.WriteString("或模型目录当成真实模型时写入会被拒绝，否则别名会遮蔽真名。目标眼下没有账号提供\n")
	b.WriteString("也能先写，之后加账号即生效；解析不了时这条别名不参与选号。\n\n")
	b.WriteString("示例：\n")
	b.WriteString("  switchboard model alias set fast glm-4.6\n\n")
	b.WriteString("查看某个子命令的参数：switchboard model alias <子命令> --help\n")
	_, _ = io.WriteString(w, b.String())
}

// cmdModelAlias 是 model alias 这一组子命令的入口。
func cmdModelAlias(e *Env, args []string) error {
	if isHelpRequest(args) {
		printModelAliasHelp(e.Stdout)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：set | unset | ls\n用 switchboard model alias --help 查看用法")
	}
	switch args[0] {
	case "set":
		return cmdModelAliasSet(e, args[1:])
	case "unset":
		return cmdModelAliasUnset(e, args[1:])
	case "ls":
		return cmdModelAliasLs(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：set | unset | ls\n用 switchboard model alias --help 查看用法", args[0])
	}
}

// checkAliasName 在做任何事之前判一遍别名：空值（含只有空白）算用法错误、退出码 2。
// store 的写入口也会去空白并判一次，这里的重复只为在动库之前把「命令写错了」与
// 「运行失败」分开——用法错误要打出用法行。
func checkAliasName(raw, field, usageLine string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", usagef("%s不能为空\n%s", field, usageLine)
	}
	return name, nil
}

// cmdModelAliasSet 写一条别名，覆盖语义：同名再写一次就是改目标。
func cmdModelAliasSet(e *Env, args []string) error {
	fs := newFlagSet(e, "model alias set", "把客户端用的名字指向真实模型名")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("%s\n别名与目标两个位置参数都要给；同名再写一次就是改目标", modelAliasSetUsage)
	}
	alias, err := checkAliasName(fs.Arg(0), "别名", modelAliasSetUsage)
	if err != nil {
		return err
	}
	target, err := checkAliasName(fs.Arg(1), "目标", modelAliasSetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 遮蔽真名、空值、别名等于目标都由 store.SetModelAlias 判；它的文案已经写清发生了什么、
	// 下一步换哪个名字，这里不重写一遍。
	if err := a.st.SetModelAlias(alias, target); err != nil {
		return err
	}

	// 写完再解析一次：成环与超长的链在设置时就报出来，而不是等请求到了才发现。
	// 这时这一行已经落库，所以文案里补一句它已经写进去了，并指向查与删的两条命令。
	resolved, found, err := a.st.ResolveModelAlias(alias)
	if err != nil {
		return fmt.Errorf("别名 %s → %s 已经写进库，但它现在解析不出真实模型名：%w；"+
			"用 switchboard model alias ls %s 看这条链，改掉链上一条或 switchboard model alias unset %s 撤掉它",
			alias, target, err, alias, alias)
	}

	row := modelAliasRow{alias: alias, target: target}
	if found {
		row.resolved = resolved
	}

	if e.JSON {
		return e.printJSON(row.toJSON())
	}
	fmt.Fprintf(e.Stdout, "已设置别名 %s → %s\n", alias, target)
	fmt.Fprintln(e.Stdout, "下一步：switchboard model alias ls 核对")
	return nil
}

// cmdModelAliasUnset 删掉一条别名。删不到时报错并列出当前有哪些别名：
// 多半是别名写错了，光说一句「没有这条别名」看不出下一步该改哪个参数（与 provider header unset 同一做法）。
func cmdModelAliasUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "model alias unset", "删掉一条别名")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n一次删一条，别名要写全（别名精确匹配，不看大小写以外的近似）", modelAliasUnsetUsage)
	}
	alias, err := checkAliasName(fs.Arg(0), "别名", modelAliasUnsetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.DeleteModelAlias(alias); err != nil {
		if errors.Is(err, store.ErrModelAliasNotFound) {
			return aliasNotFoundError(a.st, alias)
		}
		return err
	}

	aliases, err := a.st.ModelAliases()
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"alias":     alias,
			"removed":   true,
			"remaining": len(aliases),
		})
	}
	fmt.Fprintf(e.Stdout, "已删除别名 %s，现有 %d 条别名\n", alias, len(aliases))
	return nil
}

// aliasNotFoundError 在删不到（或查询不到）一条别名时列出库里现有的别名。
// 一条都没有时把下一步写成设置一条的命令，有别名时把它们的名字列出来，
// 让「写错了哪个字母」一眼可见。
func aliasNotFoundError(st *store.Store, alias string) error {
	aliases, err := st.ModelAliases()
	if err != nil {
		return err
	}
	if len(aliases) == 0 {
		return fmt.Errorf("别名 %q 不存在：现在一条别名都没有，设置一条用 switchboard model alias set %s <目标>",
			alias, alias)
	}
	names := make([]string, 0, len(aliases))
	for _, m := range aliases {
		names = append(names, m.Alias)
	}
	return fmt.Errorf("别名 %q 不存在：现有 %s；看全部用 switchboard model alias ls，设置一条用 switchboard model alias set <别名> <目标>",
		alias, strings.Join(names, "、"))
}

// cmdModelAliasLs 列别名、目标与能不能解析。不给别名就列全部（按别名升序），给了就只列这一条。
func cmdModelAliasLs(e *Env, args []string) error {
	fs := newFlagSet(e, "model alias ls", "列别名、目标与能否解析")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("%s\n一次最多看一条别名，看全部就整个省掉别名", modelAliasLsUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	var want string
	if fs.NArg() == 1 {
		want, err = checkAliasName(fs.Arg(0), "别名", modelAliasLsUsage)
		if err != nil {
			return err
		}
	}

	aliases, err := a.st.ModelAliases()
	if err != nil {
		return err
	}
	if want != "" {
		// 别名不在表里时报错，不合成一张空表：写错一个字母会看起来「没设置过别名」。
		found := false
		for _, m := range aliases {
			if m.Alias == want {
				found = true
				aliases = []store.ModelAlias{m}
				break
			}
		}
		if !found {
			return aliasNotFoundError(a.st, want)
		}
	}

	rows := make([]modelAliasRow, 0, len(aliases))
	for _, m := range aliases {
		row := modelAliasRow{alias: m.Alias, target: m.Target}
		resolved, ok, err := a.st.ResolveModelAlias(m.Alias)
		switch {
		case err != nil:
			row.resolveErr = err
		case ok:
			row.resolved = resolved
		default:
			// 表里有这一行却说自己不是别名：只可能是判定与读出之间表被改掉了。
			// 不当成「可解析」蒙混过去，也不编一个原因，照实说一句。
			row.resolveErr = errors.New("库里读不出这条别名，重跑一次 switchboard model alias ls 确认")
		}
		rows = append(rows, row)
	}

	if e.JSON {
		out := make([]modelAliasJSON, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.toJSON())
		}
		return e.printJSON(out)
	}

	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "还没有设置任何别名。客户端把模型名写死时，用 switchboard model alias set fast glm-4.6 把那个名字指到真实模型名上")
		return nil
	}

	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		status, resolved := "可解析", r.resolved
		if !r.ok() {
			// 解析列写 store 的原文：它带着是哪几条串在一起，照着就能找到要改的那一条。
			status, resolved = "不可解析", r.resolveErr.Error()
		}
		table = append(table, []string{r.alias, r.target, status, resolved})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"别名", "目标", "状态", "解析"}, table))
	return nil
}
