package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// errFlagsDeclared 是声明遍的止步信号：命令体声明完参数、走到 Parse 就停下，
// 真正的执行留给 Cobra 解析之后那一遍。
var errFlagsDeclared = errors.New("leaf flags declared")

// leafFlags 是命令行参数在本程序里的唯一入口，替换旧的 flagSet。
//
// 一个命令体会被调用两遍：
//   - 构造时一遍，只为把参数声明进 cmd.Flags()，走到 Parse 停下；
//   - 执行时一遍，所有取值函数返回声明那一遍创建的指针（Cobra 已经把值解析进去），
//     Parse 只收下位置参数。
//
// 这样做的好处是命令体只需要声明参数并 Parse，业务逻辑按旧写法原样保留。
type flagGroupSpec struct {
	title string
	names []string
}

type leafHelpOptions struct {
	examples []string
	groups   []flagGroupSpec
	aliases  map[string][]string
}

type leafFlags struct {
	cmd     *cobra.Command
	vals    map[string]any
	pos     []string
	running bool
	// examples、groups、aliases 由声明遍收集，帮助输出读它们。
	examples []string
	groups   []flagGroupSpec
	aliases  map[string][]string
}

// laxStringValues 让可重复参数在声明遍与执行遍之间传递取值：
// 执行遍会新建一个同类型的值，pflag 解析的却是声明遍那个，所以要把值抄过来。
type laxStringValues interface {
	laxValues() []string
	setLaxValues([]string)
}

// leafPflagValue 把一个标准库 flag.Value 交给 pflag。
// 自定义类型的占位符由用法里的反引号给出；没有反引号时显示 "value"，
// 与旧版 flag.UnquoteUsage 对自定义类型的结果一致。
type leafPflagValue struct{ flag.Value }

func (leafPflagValue) Type() string { return "value" }

// leafShorthand 让单字母参数（目前只有 log tail 的 -n）同时接受 -n 与 --n：
// 旧版标准库 flag 把 -n 与 --n 当同一个参数，pflag 默认把单个短横线当 shorthand。
func leafShorthand(name string) string {
	if len(name) == 1 {
		return name
	}
	return ""
}

func (f *leafFlags) String(name, def, usage string) *string {
	if p, ok := f.vals[name]; ok {
		return p.(*string)
	}
	p := f.cmd.Flags().StringP(name, leafShorthand(name), def, usage)
	f.vals[name] = p
	return p
}

func (f *leafFlags) Bool(name string, def bool, usage string) *bool {
	if p, ok := f.vals[name]; ok {
		return p.(*bool)
	}
	p := f.cmd.Flags().BoolP(name, leafShorthand(name), def, usage)
	f.vals[name] = p
	return p
}

func (f *leafFlags) Int(name string, def int, usage string) *int {
	if p, ok := f.vals[name]; ok {
		return p.(*int)
	}
	p := f.cmd.Flags().IntP(name, leafShorthand(name), def, usage)
	f.vals[name] = p
	return p
}

func (f *leafFlags) Int64(name string, def int64, usage string) *int64 {
	if p, ok := f.vals[name]; ok {
		return p.(*int64)
	}
	p := f.cmd.Flags().Int64P(name, leafShorthand(name), def, usage)
	f.vals[name] = p
	return p
}

func (f *leafFlags) Float64(name string, def float64, usage string) *float64 {
	if p, ok := f.vals[name]; ok {
		return p.(*float64)
	}
	p := f.cmd.Flags().Float64P(name, leafShorthand(name), def, usage)
	f.vals[name] = p
	return p
}

func (f *leafFlags) Duration(name string, def time.Duration, usage string) *time.Duration {
	if p, ok := f.vals[name]; ok {
		return p.(*time.Duration)
	}
	p := f.cmd.Flags().DurationP(name, leafShorthand(name), def, usage)
	f.vals[name] = p
	return p
}

// Var 注册一个可重复参数。执行遍不重新注册，只把声明遍收到的值抄进调用方新建的值里。
func (f *leafFlags) Var(value flag.Value, name, usage string) {
	if stored, ok := f.vals[name]; ok {
		src, ok := stored.(laxStringValues)
		if !ok {
			panic(fmt.Sprintf("参数 --%s 在声明遍的类型 %T 不支持跨遍传值", name, stored))
		}
		dst, ok := value.(laxStringValues)
		if !ok {
			panic(fmt.Sprintf("参数 --%s 在执行遍的类型 %T 不支持跨遍传值", name, value))
		}
		dst.setLaxValues(src.laxValues())
		return
	}
	f.cmd.Flags().VarP(leafPflagValue{Value: value}, name, leafShorthand(name), usage)
	f.vals[name] = value
}

// Parse 在声明遍止步，在执行遍只记录位置参数：参数本身已经由 Cobra 解析好了。
func (f *leafFlags) Parse(args []string) error {
	if !f.running {
		return errFlagsDeclared
	}
	f.pos = args
	return nil
}

func (f *leafFlags) NArg() int { return len(f.pos) }

func (f *leafFlags) Arg(i int) string {
	if i < 0 || i >= len(f.pos) {
		return ""
	}
	return f.pos[i]
}

func (f *leafFlags) Args() []string { return f.pos }

// Changed 判断参数在命令行上显式出现过。Cobra 解析前会把父命令的持久参数并进
// cmd.Flags()，因此 --json / --config / --db 也能查。
func (f *leafFlags) Changed(name string) bool { return f.cmd.Flags().Changed(name) }

func (f *leafFlags) setExamples(lines ...string) {
	if f.running {
		return
	}
	f.examples = append(f.examples, lines...)
}

func (f *leafFlags) aliasFlag(primary string, aliases ...string) {
	if f.running || len(aliases) == 0 {
		return
	}
	if f.aliases == nil {
		f.aliases = map[string][]string{}
	}
	f.aliases[primary] = append(f.aliases[primary], aliases...)
}

func (f *leafFlags) flagGroup(title string, names ...string) {
	if f.running {
		return
	}
	f.groups = append(f.groups, flagGroupSpec{
		title: title,
		names: append([]string(nil), names...),
	})
}

// newLeafCommand 把一个命令体接到原生 Cobra 命令上。name 是带父命令的完整名字
// （例如 "account add"），used 只取最后一段；summary 是 --help 的标题。
func newLeafCommand(e *Env, name, summary string, body func(*Env, *leafFlags, []string) error) *cobra.Command {
	use := name
	if i := strings.LastIndexByte(name, ' '); i >= 0 {
		use = name[i+1:]
	}
	decl := &leafFlags{vals: map[string]any{}}
	cmd := &cobra.Command{
		Use:   use,
		Short: summary,
		Args:  cobra.ArbitraryArgs,
	}
	decl.cmd = cmd
	// 声明遍：命令体会把参数注册进 cmd.Flags()，然后在 Parse 处停下。
	if err := body(e, decl, nil); err != nil && !errors.Is(err, errFlagsDeclared) {
		panic(fmt.Sprintf("声明命令 %s 的参数失败: %v", name, err))
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		run := &leafFlags{cmd: c, vals: decl.vals, running: true}
		return body(e, run, args)
	}
	finalizeLeafCommandWith(e, cmd, summary, leafHelpOptions{
		examples: decl.examples,
		groups:   decl.groups,
		aliases:  decl.aliases,
	})
	return cmd
}

// groupSpec 描述一组子命令的静态信息：组级帮助与「未知子命令」文案都用它。
type groupSpec struct {
	name    string
	purpose string
	items   []groupItem
	// notes、docs、examples、hint 是组级帮助的可选附加内容，见 groupHelpOptions。
	notes    []string
	docs     string
	examples []string
	hint     string
	// unknown 是未知子命令的完整文案，含一个留给 %q 的位置。
	unknown string
}

// stripGroupGlobals 认组级命令行上的全局参数。组关掉了参数解析，--json / --config / --db
// 不会自己进 Env，这里补上；剩下的位置参数交给「是否在求帮助 / 未知子命令」的判断。
// 这样 `lax account --db <路径> --help` 与 `lax account --json` 不会把参数名当成子命令。
func stripGroupGlobals(e *Env, args []string) ([]string, error) {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			e.JSON = true
		case strings.HasPrefix(arg, "--json="):
			v := strings.TrimPrefix(arg, "--json=")
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, usagef("--json 的值 %q 不是布尔值，写 true 或 false", v)
			}
			e.JSON = b
		case arg == "--config" || arg == "--db":
			if i+1 >= len(args) {
				return nil, usagef("%s 需要一个路径", arg)
			}
			i++
			if arg == "--config" {
				e.Config = args[i]
			} else {
				e.DB = args[i]
			}
		case strings.HasPrefix(arg, "--config="):
			e.Config = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "--db="):
			e.DB = strings.TrimPrefix(arg, "--db=")
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

// newGroupCommand 构造一个组。组不做参数解析：无参数与 -h/--help/help 都打组级帮助，
// 其余参数交给子命令；找不到子命令时由这层报「未知子命令」并给出查看帮助的办法。
func newGroupCommand(e *Env, spec groupSpec, children ...*cobra.Command) *cobra.Command {
	use := spec.name
	if i := strings.LastIndexByte(use, ' '); i >= 0 {
		use = use[i+1:]
	}
	printHelp := func(w io.Writer) {
		printGroupHelpWith(w, spec.name, spec.purpose, spec.items, groupHelpOptions{
			notes:    spec.notes,
			docs:     spec.docs,
			examples: spec.examples,
			hint:     spec.hint,
		})
	}
	cmd := &cobra.Command{
		Use:                use,
		Short:              spec.purpose,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rest, err := stripGroupGlobals(e, args)
			if err != nil {
				return err
			}
			if len(rest) == 0 || isHelpRequest(rest) {
				printHelp(e.Stdout)
				return nil
			}
			return usagef(spec.unknown, rest[0])
		},
	}
	cmd.SetHelpFunc(func(*cobra.Command, []string) { printHelp(e.Stdout) })
	cmd.AddCommand(children...)
	return cmd
}
