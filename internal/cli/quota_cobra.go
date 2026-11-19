package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// quota 一组的原生 Cobra 定义。组与五个子命令都在这里；具体执行仍调用
// quota.go 里的 runQuota* 函数，那些函数只接收已经解析好的值，不碰命令行。
const (
	quotaPurpose = "账号额度监控与到期提示（只读，不改选号行为）"

	quotaLsUsage      = "用法：lax quota ls [--json]\n看一个账号的明细用 lax quota show <账号>"
	quotaShowUsage    = "用法：lax quota show <账号> [--settings] [--json]"
	quotaRefreshUsage = "用法：lax quota refresh <账号> | lax quota refresh --all"
	quotaSetUsage     = "用法：lax quota set <账号> [--source auto|commandcode|deepseek|off] [--fx-rate N] " +
		"[--auto-refresh[=true|false]] [--expiry-mode warn|deprioritize|exclude] [--expiry-days N] [--base-url <地址>]"
	quotaUnsetUsage = "用法：lax quota unset <账号> [source|fx_rate|auto_refresh|expiry_mode|expiry_days|base_url]"
)

// newQuotaCommand 构造 quota 组。组级帮助写 stdout（与旧分发器一样），
// 子命令自己的 --help 仍写 stderr（那是参数说明，不是命令结果）。
func newQuotaCommand(e *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "监控账号额度与刷新周期",
		Args:  cobra.ArbitraryArgs,
		// 组级保持旧分发器的判定：只有无参数、help/-h/--help 与已知子命令算正常路径。
		// 关掉组级参数解析后，全局参数要自己认（stripGroupGlobals），否则
		// `quota --db <路径> --help` 会把 --db 当成未知子命令。
		// 子命令自己仍是原生 Cobra，照常解析 --json/--config/--db。
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rest, err := stripGroupGlobals(e, args)
			if err != nil {
				return err
			}
			if len(rest) == 0 || isHelpRequest(rest) {
				printGroupHelp(e.Stdout, "quota", quotaPurpose, quotaSubs)
				return nil
			}
			return usagef("未知子命令 %q，可用：ls | show | refresh | set | unset\n用 lax quota --help 查看子命令", rest[0])
		},
	}
	cmd.SetHelpFunc(func(*cobra.Command, []string) {
		printGroupHelp(e.Stdout, "quota", quotaPurpose, quotaSubs)
	})
	cmd.AddCommand(
		newQuotaLsCommand(e),
		newQuotaShowCommand(e),
		newQuotaRefreshCommand(e),
		newQuotaSetCommand(e),
		newQuotaUnsetCommand(e),
	)
	return cmd
}

func newQuotaLsCommand(e *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: quotaLsSummary,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usagef("%s", quotaLsUsage)
			}
			return runQuotaLs(e)
		},
	}
	finalizeLeafCommand(e, cmd, quotaLsSummary)
	return cmd
}

func newQuotaShowCommand(e *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: quotaShowSummary,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("%s", quotaShowUsage)
			}
			settingsOnly, _ := cmd.Flags().GetBool("settings")
			return runQuotaShow(e, args[0], settingsOnly)
		},
	}
	cmd.Flags().Bool("settings", false, "只看监控设置，不显示额度快照")
	finalizeLeafCommand(e, cmd, quotaShowSummary)
	return cmd
}

func newQuotaRefreshCommand(e *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: quotaRefreshSummary,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			if all && len(args) != 0 {
				return usagef("--all 与账号名不能一起写\n%s", quotaRefreshUsage)
			}
			if !all && len(args) != 1 {
				return usagef("要写一个账号名，或加 --all\n%s", quotaRefreshUsage)
			}
			account := ""
			if !all {
				account = args[0]
			}
			return runQuotaRefresh(e, account, all)
		},
	}
	cmd.Flags().Bool("all", false, "刷新全部账号")
	finalizeLeafCommand(e, cmd, quotaRefreshSummary)
	return cmd
}

func newQuotaSetCommand(e *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set",
		Short: quotaSetSummary,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usagef("%s\n账号名要写一个", quotaSetUsage)
			}
			var patch quotaSetPatch
			patch.source, _ = cmd.Flags().GetString("source")
			patch.fxRate, _ = cmd.Flags().GetFloat64("fx-rate")
			patch.autoRefresh, _ = cmd.Flags().GetBool("auto-refresh")
			patch.expiryMode, _ = cmd.Flags().GetString("expiry-mode")
			patch.expiryDays, _ = cmd.Flags().GetInt("expiry-days")
			patch.baseURL, _ = cmd.Flags().GetString("base-url")
			patch.sourceSet = cmd.Flags().Changed("source")
			patch.fxRateSet = cmd.Flags().Changed("fx-rate")
			patch.autoRefreshSet = cmd.Flags().Changed("auto-refresh")
			patch.expiryModeSet = cmd.Flags().Changed("expiry-mode")
			patch.expiryDaysSet = cmd.Flags().Changed("expiry-days")
			patch.baseURLSet = cmd.Flags().Changed("base-url")
			return runQuotaSet(e, args[0], patch)
		},
	}
	cmd.Flags().String("source", "", "监控来源：auto / commandcode / deepseek / off")
	cmd.Flags().Float64("fx-rate", 0, "非 USD 币种折 USD 的手工汇率，例如 CNY 填 0.14")
	cmd.Flags().Bool("auto-refresh", false, "serve 时是否定时刷新这个账号的额度")
	cmd.Flags().String("expiry-mode", "", "到期处理：warn / deprioritize / exclude")
	cmd.Flags().Int("expiry-days", 0, "临近到期的提前天数，默认 3")
	cmd.Flags().String("base-url", "", "额度接口地址；留空时用账号所属厂商的 base_url")
	finalizeLeafCommand(e, cmd, quotaSetHelpSummary)
	return cmd
}

func newQuotaUnsetCommand(e *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset",
		Short: quotaUnsetSummary,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usagef("%s\n不写字段时删掉整行设置；写字段时只把那一项恢复默认", quotaUnsetUsage)
			}
			field := ""
			if len(args) == 2 {
				field = strings.TrimSpace(args[1])
				switch field {
				case "source", "fx_rate", "auto_refresh", "expiry_mode", "expiry_days", "base_url":
				default:
					return usagef("不认识的字段 %q，可用 source、fx_rate、auto_refresh、expiry_mode、expiry_days、base_url\n%s", field, quotaUnsetUsage)
				}
			}
			return runQuotaUnset(e, args[0], field)
		},
	}
	finalizeLeafCommand(e, cmd, quotaUnsetHelpSummary)
	return cmd
}
