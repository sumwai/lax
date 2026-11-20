package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// finalizeLeafCommand 统一叶子命令的帮助与参数错误行为：
// --help 打该命令自己的用法（写 stderr），参数写错也打同一份用法并算用法错误（退出码 2）。
// newLeafCommand 与 quota 的五个原生子命令都走这里，避免帮助版式与退出码分成两套。
func finalizeLeafCommand(e *Env, cmd *cobra.Command, summary string, examples ...string) {
	finalizeLeafCommandWith(e, cmd, summary, leafHelpOptions{examples: examples})
}

func finalizeLeafCommandWith(e *Env, cmd *cobra.Command, summary string, opts leafHelpOptions) {
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		printCobraCommandUsageWith(e.Stderr, c, summary, opts)
	})
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		if errors.Is(err, pflag.ErrHelp) {
			return err
		}
		// 旧版 flag 包在解析出错时先把整个用法写到 stderr；这里保持同样的形状。
		// --json 时错误流只留 Run 写的一个对象，不再混入用法文本。
		if !e.JSON && !e.jsonRequested {
			printCobraCommandUsageWith(e.Stderr, c, summary, opts)
		}
		return usagef("%v", err)
	})
}

// printCobraDefaultHelp 复刻 Cobra 的默认帮助模板，给 completion 这类自带命令用：
// 它们不是本程序定义的命令，帮助保持 Cobra 原文，写 stdout 并以 0 退出。
func printCobraDefaultHelp(w io.Writer, cmd *cobra.Command) {
	text := cmd.Long
	if text == "" {
		text = cmd.Short
	}
	text = strings.TrimRightFunc(text, unicode.IsSpace)
	if text != "" {
		fmt.Fprintln(w, text)
		fmt.Fprintln(w)
	}
	if cmd.Runnable() || cmd.HasSubCommands() {
		fmt.Fprint(w, cmd.UsageString())
	}
}

type cobraFlagLine struct {
	name        string
	aliases     []string
	placeholder string
	usage       string
	defValue    string
}

type renderedFlagGroup struct {
	title string
	lines []cobraFlagLine
}

// printCobraCommandUsage 打印原生子命令的「用法 + 选项」。参数按名字排序，
// 只列本地参数与 --json（--config/--db 不在这里重复）；examples 非空时附在选项后面。
// 叶子命令的帮助写 stderr，这是参数说明而不是命令结果。
func printCobraCommandUsage(w io.Writer, cmd *cobra.Command, summary string, examples ...string) {
	printCobraCommandUsageWith(w, cmd, summary, leafHelpOptions{examples: examples})
}

func printCobraCommandUsageWith(w io.Writer, cmd *cobra.Command, summary string, opts leafHelpOptions) {
	u := newUI(w)
	name := strings.TrimPrefix(cmd.CommandPath(), appName+" ")
	fmt.Fprintf(w, "%s %s — %s\n\n", u.bold(appName), u.bold(name), summary)
	fmt.Fprintf(w, "%s\n  %s %s [选项]\n\n", u.bold("用法:"), appName, name)

	lines := applyFlagAliases(cobraFlagLines(cmd), opts.aliases)
	if len(opts.groups) == 0 {
		fmt.Fprintf(w, "%s\n", u.bold("选项:"))
		for _, f := range lines {
			fmt.Fprintln(w, formatCobraFlagLine(u, f))
		}
	} else {
		groups := groupCobraFlagLines(lines, opts.groups)
		for i, g := range groups {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "%s\n", u.bold(g.title+":"))
			for _, f := range g.lines {
				fmt.Fprintln(w, formatCobraFlagLine(u, f))
			}
		}
	}

	if len(opts.examples) > 0 {
		fmt.Fprintf(w, "\n%s\n", u.bold("示例:"))
		for _, ex := range opts.examples {
			fmt.Fprintln(w, u.dim("  "+ex))
		}
	}
}

func applyFlagAliases(lines []cobraFlagLine, aliases map[string][]string) []cobraFlagLine {
	if len(aliases) == 0 {
		return lines
	}
	hidden := make(map[string]bool)
	for _, list := range aliases {
		for _, a := range list {
			hidden[a] = true
		}
	}
	out := make([]cobraFlagLine, 0, len(lines))
	for _, f := range lines {
		if hidden[f.name] {
			continue
		}
		if list := aliases[f.name]; len(list) > 0 {
			f.aliases = list
		}
		out = append(out, f)
	}
	return out
}

func groupCobraFlagLines(lines []cobraFlagLine, specs []flagGroupSpec) []renderedFlagGroup {
	byName := make(map[string]cobraFlagLine, len(lines))
	for _, f := range lines {
		byName[f.name] = f
	}
	used := make(map[string]bool, len(lines))
	groups := make([]renderedFlagGroup, 0, len(specs)+1)
	generalIdx := -1
	for _, s := range specs {
		var gl []cobraFlagLine
		for _, n := range s.names {
			if f, ok := byName[n]; ok && !used[n] {
				used[n] = true
				gl = append(gl, f)
			}
		}
		if s.title == "通用" {
			generalIdx = len(groups)
		}
		groups = append(groups, renderedFlagGroup{title: s.title, lines: gl})
	}
	var rest []cobraFlagLine
	for _, f := range lines {
		if !used[f.name] {
			rest = append(rest, f)
		}
	}
	if len(rest) > 0 {
		if generalIdx >= 0 {
			groups[generalIdx].lines = append(groups[generalIdx].lines, rest...)
		} else {
			groups = append(groups, renderedFlagGroup{title: "通用", lines: rest})
		}
	}
	nonEmpty := make([]renderedFlagGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.lines) > 0 {
			nonEmpty = append(nonEmpty, g)
		}
	}
	return nonEmpty
}

func formatCobraFlagLine(u uiStyle, f cobraFlagLine) string {
	line := "  --" + f.name
	for _, al := range f.aliases {
		line += ", --" + al
	}
	if f.placeholder != "" {
		line += " " + f.placeholder
	}
	if f.placeholder == "" && len(f.name) == 1 && len(f.aliases) == 0 {
		line += "\t"
	} else {
		line += "\n    \t"
	}
	line += strings.ReplaceAll(f.usage, "\n", "\n    \t")
	if !zeroCobraDefault(f) {
		if f.placeholder == "string" {
			line += u.dim(fmt.Sprintf(" (默认: %q)", f.defValue))
		} else {
			line += u.dim(fmt.Sprintf(" (默认: %s)", f.defValue))
		}
	}
	return line
}

// cobraFlagLines 收集要展示的参数。Cobra 在解析时会把 root 的持久参数并进
// cmd.Flags()；这里同时读 InheritedFlags，避免帮助路径没合并时漏掉 --json。
func cobraFlagLines(cmd *cobra.Command) []cobraFlagLine {
	seen := map[string]bool{}
	lines := make([]cobraFlagLine, 0, 8)
	add := func(f *pflag.Flag) {
		if seen[f.Name] || f.Name == "help" || f.Hidden {
			return
		}
		switch f.Name {
		case "config", "db":
			return
		}
		seen[f.Name] = true
		placeholder, usage := pflag.UnquoteUsage(f)
		lines = append(lines, cobraFlagLine{name: f.Name, placeholder: placeholder, usage: usage, defValue: f.DefValue})
	}
	cmd.Flags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)
	if !seen["json"] {
		lines = append(lines, cobraFlagLine{name: "json", usage: "以 JSON 格式输出", defValue: "false"})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].name < lines[j].name })
	return lines
}

// zeroCobraDefault 与 zeroDefault 同一个判断，只是输入换成了 pflag 的名称与默认值。
func zeroCobraDefault(f cobraFlagLine) bool {
	switch f.placeholder {
	case "string":
		return f.defValue == ""
	case "":
		return f.defValue == "false"
	case "duration":
		return f.defValue == "0s"
	case "int", "int64", "uint", "uint64", "float", "float64":
		return f.defValue == "0"
	}
	return f.defValue == ""
}
