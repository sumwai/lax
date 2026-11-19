package cli

import (
	"errors"
	"fmt"

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
		printUsage(e.Stderr)
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
			target, _, err := root.Find(args)
			if err != nil {
				return usagef("%v", err)
			}
			if target == root {
				return usagef("没有命令 %q，用 lax --help 查看可用命令", args[0])
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
	// completion 是 Cobra 自带命令，帮助与补全脚本都交给 Cobra；这里只把未知 shell
	// 改成中文用法错误（退出码 2），否则 Cobra 默认按运行失败退 1。
	// 自带命令不可运行时 Cobra 会在校验参数之前就打帮助，所以补一个只打帮助的 RunE，
	// 让 Args 先跑起来。
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		c.Args = func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return usagef("未知的 shell %q，可用：bash | zsh | fish | powershell\n用法：lax completion bash|zsh|fish|powershell", args[0])
		}
		c.RunE = func(cmd *cobra.Command, _ []string) error {
			printCobraDefaultHelp(e.Stdout, cmd)
			return nil
		}
		break
	}
	return root
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
