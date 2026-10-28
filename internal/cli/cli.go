// Package cli 实现子命令分发、全局参数与输出约定。
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

const (
	appName = "switchboard"
	version = "0.1.0-dev"

	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// errNotImplemented 表示子命令已在设计文档中定义，但当前里程碑尚未实现。
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
	fmt.Fprint(w, "\n全局参数：\n  --json     以 JSON 输出\n  --config   指定配置文件路径\n  --db       指定数据库路径\n")
}

func newFlagSet(e *Env, name, summary string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	fs.BoolVar(&e.JSON, "json", e.JSON, "以 JSON 输出")
	fs.Usage = func() {
		fmt.Fprintf(e.Stderr, "%s %s — %s\n\n用法：\n  %s %s [参数]\n\n参数：\n", appName, name, summary, appName, name)
		fs.PrintDefaults()
	}
	return &flagSet{FlagSet: fs}
}

// flagSet 让位置参数可以出现在参数前后任意位置，与常见命令行习惯一致。
type flagSet struct {
	*flag.FlagSet
	pos []string
}

// Parse 逐段解析：每遇到一个位置参数就收起来，继续解析它后面的参数。
func (f *flagSet) Parse(args []string) error {
	for {
		if err := f.FlagSet.Parse(args); err != nil {
			return err
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
