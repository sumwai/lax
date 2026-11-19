package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// errReported 表示错误已经按最终形状写进 stderr，Run 只需要据此决定退出码。
// 未知子命令沿用了旧分发器的两段输出（错误行 + 顶层帮助），不能让 Run 再补一遍。
var errReported = errors.New("错误已经报告过")

// newRootCommand 构造一级命令树。所有命令都是原生 Cobra 命令：
// root 上挂着 --json / --config / --db 三个持久参数，写在子命令前后都生效。
func newRootCommand(e *Env) *cobra.Command {
	root := &cobra.Command{
		Use:           appName,
		Short:         "本机模型网关",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				printUsage(e.Stdout)
				return nil
			}
			// 正常情况下 Run 已经在进 Cobra 之前拦下未知一级命令；这里只兜底。
			// --json 时不再先打一段文字，交给 Run 输出结构化错误。
			// jsonRequested 兜住 Cobra 解析前就认出的写法，与 FlagErrorFunc 保持一致。
			if e.JSON || e.jsonRequested {
				return usagef("未知子命令 %q，用 lax --help 查看可用命令", args[0])
			}
			fmt.Fprintf(e.Stderr, "%s: 未知子命令 %q\n\n", newUI(e.Stderr).err(appName), args[0])
			printUsage(e.Stderr)
			return errReported
		},
	}
	root.SetOut(e.Stdout)
	root.SetErr(e.Stderr)

	root.PersistentFlags().BoolVar(&e.JSON, "json", e.JSON, "以 JSON 格式输出")
	root.PersistentFlags().StringVar(&e.Config, "config", e.Config, "指定配置文件路径")
	root.PersistentFlags().StringVar(&e.DB, "db", e.DB, "指定数据库路径")

	// 叶子命令自己的参数错误由它自己的 FlagErrorFunc 打用法；能落到 root 上的
	// 只有全局参数写错，这里打顶层用法并把错误包成用法错误（退出码 2）。
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		// --json 的错误流只留 Run 写的一个对象；用法文本会把 JSON 挤成第二段。
		if !e.JSON && !e.jsonRequested {
			printUsage(e.Stderr)
		}
		return usagef("%v", err)
	})
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if cmd == root {
			printUsage(e.Stdout)
			return
		}
		// completion 等 Cobra 自带命令沿用它们自己的帮助模板，写 stdout。
		printCobraDefaultHelp(e.Stdout, cmd)
	})
	// 顶层 help 等价于无参数；带命令名时显示那个命令的帮助。
	root.SetHelpCommand(&cobra.Command{
		Use:                "help [命令]",
		Short:              "显示帮助",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				printUsage(e.Stdout)
				return nil
			}
			target, rest, err := root.Find(args)
			if err != nil {
				return usagef("%v", err)
			}
			if target == root {
				return usagef("没有命令 %q，用 lax --help 查看可用命令", args[0])
			}
			// Find 只走到最深的已知命令，剩下的位置参数没人认领：`help account nope`
			// 以前会静默显示 account 的帮助，让人以为 nope 是一个有效子命令。
			for _, arg := range rest {
				if !strings.HasPrefix(arg, "-") {
					return usagef("没有命令 %q，用 lax --help 查看可用命令", strings.Join(args, " "))
				}
			}
			return target.Help()
		},
	})

	root.AddCommand(
		newInitCommand(e),
		newServeCommand(e),
		newAccountCommand(e),
		newModelCommand(e),
		newRouteCommand(e),
		newQuotaCommand(e),
		newProviderCommand(e),
		newPluginCommand(e),
		newUsageCommand(e),
		newLogCommand(e),
		newDoctorCommand(e),
		newVersionCommand(e),
	)
	root.InitDefaultCompletionCmd()
	finalizeCompletionCommand(e, root)
	return root
}

// finalizeCompletionCommand 让 Cobra 自带的 completion 组遵守本程序的输出约定：
// 帮助走 stdout、退出码 0；未知 shell 与多余参数是中文用法错误（退出码 2）；
// 参数写错时打 completion 自己的用法，而不是顶层用法。
//
// 自带命令不可运行时 Cobra 会在校验参数之前就打帮助，所以父命令补一个只打帮助的
// RunE，让 Args 先跑起来；help 作为位置参数也认，`lax completion help` 因此与
// `lax completion --help` 等价。
func finalizeCompletionCommand(e *Env, root *cobra.Command) {
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		completion := c
		completion.Args = func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			if args[0] == "help" {
				if len(args) == 1 {
					return nil
				}
				return usagef("多余的参数 %q：lax completion help 不接受位置参数", args[1])
			}
			return usagef("未知的 shell %q，可用：bash | zsh | fish | powershell\n用法：lax completion bash|zsh|fish|powershell", args[0])
		}
		completion.RunE = func(cmd *cobra.Command, _ []string) error {
			printCobraDefaultHelp(e.Stdout, cmd)
			return nil
		}
		// 子命令继承这个函数：`completion bash --nope` 也打 completion 的用法，
		// 而不是顶层的 lax 用法。
		completion.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
			if !e.JSON && !e.jsonRequested {
				printCobraDefaultHelp(e.Stderr, cmd)
			}
			return usagef("%v", err)
		})
		for _, shell := range completion.Commands() {
			shell.Args = func(cmd *cobra.Command, args []string) error {
				if len(args) == 0 {
					return nil
				}
				return usagef("多余的参数 %q：lax completion %s 不接受位置参数", args[0], cmd.Name())
			}
		}
		return
	}
}

// topLevelCommandName 取失败命令所属的一级命令名，用来还原旧分发器
// 「<一级命令>: <错误>」的前缀。root 自己失败时退回 appName。
func topLevelCommandName(cmd *cobra.Command) string {
	for cmd != nil && cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	if cmd == nil || cmd.Name() == "" {
		return appName
	}
	return cmd.Name()
}
