// Package cli 实现子命令分发、全局参数与输出约定。
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

const (
	appName = "lax"
	version = "0.1.0-dev"

	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

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
	run     func(*Env, []string) error
}

// Run 是 CLI 入口，返回进程退出码。
func Run(args []string, stdout, stderr io.Writer) int {
	env := &Env{Stdout: stdout, Stderr: stderr}

	rest, err := env.parseGlobals(args)
	if errors.Is(err, flag.ErrHelp) {
		printUsage(stdout)
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(rest) == 0 {
		printUsage(stderr)
		return exitUsage
	}

	cmd, ok := commandTable[rest[0]]
	if !ok {
		fmt.Fprintf(stderr, "未知子命令 %q\n\n", rest[0])
		printUsage(stderr)
		return exitUsage
	}
	if err := cmd.run(env, rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "%s: %v\n", cmd.name, err)
		var ue usageError
		if errors.As(err, &ue) {
			return exitUsage
		}
		return exitFail
	}
	return exitOK
}

// parseGlobals 只消费子命令之前的全局参数，返回剩余参数。
func (e *Env) parseGlobals(args []string) ([]string, error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			e.JSON = true
		case "--config":
			if i+1 >= len(args) {
				return nil, errors.New("--config 需要一个路径")
			}
			e.Config = args[i+1]
			i++
		case "--db":
			if i+1 >= len(args) {
				return nil, errors.New("--db 需要一个路径")
			}
			e.DB = args[i+1]
			i++
		case "-h", "--help", "help":
			return nil, flag.ErrHelp
		default:
			return args[i:], nil
		}
	}
	return nil, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "%s %s — 本机模型总机\n\n用法：\n  %s <子命令> [参数]\n\n子命令：\n", appName, version, appName)
	for _, name := range commandOrder {
		c, ok := commandTable[name]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\n分组子命令的清单：%s <组> --help\n", appName)
	fmt.Fprint(w, "\n全局参数：\n  --json     以 JSON 输出\n  --config   指定配置文件路径\n  --db       指定数据库路径\n")
}

// groupItem 描述一个组里的子命令，供组级帮助使用。
type groupItem struct {
	name    string
	summary string
}

// isHelpRequest 判断组级参数是不是在求帮助：-h、--help、help 三种写法等价。
// 子命令自己的帮助仍走 newFlagSet 那条路径，不受影响。
func isHelpRequest(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

// printGroupHelp 把组的用途与子命令清单打到 stdout：这是用户要看的结果，不是错误。
// 名字列宽按最长的那几个（capability、transcribe、transcriptions 一类）留到 11 列。
func printGroupHelp(w io.Writer, name, purpose string, items []groupItem) {
	fmt.Fprintf(w, "%s %s — %s\n\n用法：\n  %s %s <子命令> [参数]\n\n子命令：\n", appName, name, purpose, appName, name)
	for _, it := range items {
		fmt.Fprintf(w, "  %-11s %s\n", it.name, it.summary)
	}
	fmt.Fprintf(w, "\n查看某个子命令的参数：%s %s <子命令> --help\n", appName, name)
}

func newFlagSet(e *Env, name, summary string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	fs.BoolVar(&e.JSON, "json", e.JSON, "以 JSON 输出")
	fs.Usage = func() {
		fmt.Fprintf(e.Stderr, "%s %s — %s\n\n用法：\n  %s %s [参数]\n\n参数：\n", appName, name, summary, appName, name)
		printFlagDefaults(e.Stderr, fs)
	}
	return &flagSet{FlagSet: fs}
}

// printFlagDefaults 打印参数清单。flag 自带的 PrintDefaults 只印一个短横线，
// 与本程序其它地方的写法不一致，所以自己印。
func printFlagDefaults(w io.Writer, fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		placeholder, usage := flag.UnquoteUsage(f)
		line := "  --" + f.Name
		if placeholder != "" {
			line += " " + placeholder
		}
		if placeholder == "" && len(f.Name) == 1 {
			// 单个字母的开关参数值不值一行，跟在名字后面。
			line += "\t"
		} else {
			line += "\n    \t"
		}
		line += strings.ReplaceAll(usage, "\n", "\n    \t")
		if !zeroDefault(f, placeholder) {
			if placeholder == "string" {
				line += fmt.Sprintf(" (default %q)", f.DefValue)
			} else {
				line += fmt.Sprintf(" (default %s)", f.DefValue)
			}
		}
		fmt.Fprintln(w, line)
	})
}

// zeroDefault 判断参数默认值是否等于该类型的零值，是就不写进参数清单。
// 清单里只列非零默认值：每一行都挂一个 (default false) 会把真正需要注意的默认值淹掉。
func zeroDefault(f *flag.Flag, placeholder string) bool {
	switch placeholder {
	case "string":
		return f.DefValue == ""
	case "":
		// 布尔 flag 没有占位符，零值是 "false"：默认值是真时才值得在清单里写出来。
		return f.DefValue == "false"
	case "duration":
		return f.DefValue == "0s"
	case "int", "int64", "uint", "uint64", "float", "float64":
		return f.DefValue == "0"
	}
	return f.DefValue == ""
}

// flagSet 让位置参数可以出现在参数前后任意位置，与常见命令行习惯一致。
type flagSet struct {
	*flag.FlagSet
	pos []string
}

// Parse 逐段解析：每遇到一个位置参数就收起来，继续解析它后面的参数。
// 参数名写错、取值解析不了都是用法错误（退出码 2）：flag 包在出错时已经把
// 这个子命令的用法行与参数清单写到 stderr 了，这里只把错误包成用法错误，让退出码对得上。
func (f *flagSet) Parse(args []string) error {
	for {
		if err := f.FlagSet.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return usagef("%v", err)
		}
		rest := f.FlagSet.Args()
		if len(rest) == 0 {
			return nil
		}
		f.pos = append(f.pos, rest[0])
		args = rest[1:]
		if len(args) == 0 {
			return nil
		}
	}
}

func (f *flagSet) NArg() int { return len(f.pos) }

func (f *flagSet) Arg(i int) string {
	if i < 0 || i >= len(f.pos) {
		return ""
	}
	return f.pos[i]
}

func (f *flagSet) Args() []string { return f.pos }

func (e *Env) printJSON(v any) error {
	enc := json.NewEncoder(e.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
