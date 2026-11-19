// Package cli 实现子命令分发、全局参数与输出约定。
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	appName = "lax"

	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// version 由构建参数
// -X lax/internal/cli.version=<版本> 覆盖；直接 go build 时保留开发版本。
var version = "0.1.0-dev"

// errNotImplemented 用于「子命令的接口定下来了、这一版还没接上实现」这种占位。
// 返回它让退出码落在 1（执行失败）而不是 2（参数写错），脚本据此能分清「这一版没有」
// 与「参数写歪了」。当前所有子命令都已接上实现，因此没有调用点；要删它，得连「未实现」
// 这个语义一起删。
var errNotImplemented = errors.New("尚未实现，见 DESIGN.md 的里程碑")

// usageError 是用法错误，退出码 2。
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// usagef 构造一个用法错误。
func usagef(format string, args ...any) error {
	return usageError{msg: fmt.Sprintf(format, args...)}
}

// Env 保存一次命令执行需要的输出目标与全局选项。
type Env struct {
	Stdout io.Writer
	Stderr io.Writer

	JSON   bool
	Config string
	DB     string
}

type command struct {
	name    string
	summary string
}

// Run 是 CLI 入口，返回进程退出码。
//
// 参数解析全部交给 Cobra：--json / --config / --db 是 root 的持久参数，
// 写在子命令前后都生效。这里只保留两条旧约定：未知一级命令仍打中文提示加顶层帮助；
// 错误的退出码与「<一级命令>: <错误>」前缀与旧分发器一致。
func Run(args []string, stdout, stderr io.Writer) int {
	env := &Env{Stdout: stdout, Stderr: stderr}
	root := newRootCommand(env)

	ui := newUI(stderr)
	name, err := firstCommandName(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n\n", ui.err(appName), err)
		printUsage(stderr)
		return exitUsage
	}
	// 一级命令名先核一遍：Cobra 对未知命令的英文建议文案不是本程序的输出约定，
	// 未知与拼错都仍走「未知子命令 + 顶层帮助」。completion 与 help 是 Cobra 自带的。
	if name != "" && !knownCommandName(name) {
		if strings.HasPrefix(name, "-") {
			fmt.Fprintf(stderr, "%s: 未知参数 %q\n\n", ui.err(appName), name)
		} else {
			fmt.Fprintf(stderr, "%s: 未知子命令 %q\n\n", ui.err(appName), name)
		}
		printUsage(stderr)
		return exitUsage
	}

	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	if err == nil {
		return exitOK
	}
	if errors.Is(err, errReported) {
		return exitUsage
	}
	// 旧分发器的前缀是一级命令名（quota ls 出错也写 quota），这里保持一致。
	name = topLevelCommandName(cmd)
	var he hintError
	if errors.As(err, &he) {
		fmt.Fprintf(stderr, "%s: %s\n", ui.err(name), he.formatWithUI(ui))
	} else {
		fmt.Fprintf(stderr, "%s: %v\n", ui.err(name), err)
	}
	var ue usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	return exitFail
}

// firstCommandName 找出第一个位置参数（也就是一级命令名），跳过全局参数及其取值。
// 返回值是空串表示整条命令行只有全局参数或没有参数。
func firstCommandName(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json" || strings.HasPrefix(arg, "--json="):
		case arg == "--config" || arg == "--db":
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s 需要一个路径", arg)
			}
			i++
		case strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "--db="):
		default:
			return arg, nil
		}
	}
	return "", nil
}

// knownCommandName 判断一级命令名是否可执行。help、completion 与 __complete* 是
// Cobra 自带命令，其余在 commandTable 里核对。
func knownCommandName(name string) bool {
	switch name {
	case "help", "-h", "--help", "completion", "__complete", "__completeNoDesc":
		return true
	}
	_, ok := commandTable[name]
	return ok
}

func printUsage(w io.Writer) {
	u := newUI(w)
	fmt.Fprintf(w, "%s %s本机模型网关\n\n", u.bold(appName), u.dim(version+" — "))
	fmt.Fprintf(w, "%s\n  %s <命令> [选项]\n\n", u.bold("用法:"), appName)
	fmt.Fprintf(w, "%s\n", u.bold("命令:"))
	for _, name := range commandOrder {
		c, ok := commandTable[name]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\n%s\n", u.bold("选项:"))
	fmt.Fprintln(w, "  --json           以 JSON 格式输出")
	fmt.Fprintln(w, "  --config <路径>  指定配置文件路径")
	fmt.Fprintln(w, "  --db <路径>      指定数据库路径")
	fmt.Fprintf(w, "\n%s\n", u.bold("示例:"))
	fmt.Fprintln(w, u.dim("  "+appName+" init"))
	fmt.Fprintln(w, u.dim("  "+appName+" account --help"))
	fmt.Fprintln(w, u.dim("  "+appName+" model ls --json"))
}

// groupItem 描述一个组里的子命令，供组级帮助使用。
type groupItem struct {
	name    string
	summary string
}

// isHelpRequest 判断组级参数是不是在求帮助：-h、--help、help 三种写法等价。
func isHelpRequest(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

// printGroupHelp 把组的用途与子命令清单打到 stdout：这是用户要看的结果，不是错误。
// 组子命令不带参数时也走这里（与 --help 同一个分支），所以退出码 0、内容进 stdout；
// 只留一条「未知子命令」算用法错误。名字列宽按最长的那几个（capability、transcribe、
// transcriptions 一类）留到 11 列。
func printGroupHelp(w io.Writer, name, purpose string, items []groupItem) {
	u := newUI(w)
	fmt.Fprintf(w, "%s %s — %s\n\n", u.bold(appName), u.bold(name), purpose)
	fmt.Fprintf(w, "%s\n  %s %s <命令> [选项]\n\n", u.bold("用法:"), appName, name)
	fmt.Fprintf(w, "%s\n", u.bold("命令:"))
	for _, it := range items {
		fmt.Fprintf(w, "  %-11s %s\n", it.name, it.summary)
	}
	fmt.Fprintf(w, "\n%s\n", u.bold("示例:"))
	if len(items) > 0 {
		fmt.Fprintln(w, u.dim(fmt.Sprintf("  %s %s %s --help", appName, name, items[0].name)))
	} else {
		fmt.Fprintln(w, u.dim(fmt.Sprintf("  %s %s --help", appName, name)))
	}
}

func (e *Env) printJSON(v any) error {
	enc := json.NewEncoder(e.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
