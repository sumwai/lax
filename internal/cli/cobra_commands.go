package cli

import "github.com/spf13/cobra"

// 这个文件只做装配：把每个命令体接到原生 Cobra 命令上，并写清三样东西——
// 组级用途（组帮助的标题）、子命令清单（组帮助与「未知子命令」文案）、以及
// 叶子命令的帮助标题。业务逻辑都在各自的 cmd* 函数里，这里不碰。

func newInitCommand(e *Env) *cobra.Command {
	return newLeafCommand(e, "init", "初始化配置目录与数据库", cmdInit)
}

func newServeCommand(e *Env) *cobra.Command {
	return newLeafCommand(e, "serve", "启动网关，前台运行", cmdServe)
}

func newVersionCommand(e *Env) *cobra.Command {
	return newLeafCommand(e, "version", "打印版本", cmdVersion)
}

func newDoctorCommand(e *Env) *cobra.Command {
	return newLeafCommand(e, "doctor", "自检配置、目录与端口", cmdDoctor)
}

func newUsageCommand(e *Env) *cobra.Command {
	return newLeafCommand(e, "usage", "按账号、模型或日期汇总用量", cmdUsage)
}

func newLogCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "log",
		purpose: "查看请求记录",
		items:   logSubs,
		unknown: "未知子命令 %q，可用：tail\n用 lax log --help 查看子命令",
	},
		newLeafCommand(e, "log tail", "查看最近的请求记录", cmdLogTail),
	)
}

func newAccountCommand(e *Env) *cobra.Command {
	// update 是 set 的同义词：帮助里只列 set，但两个写法都收，老脚本不用改。
	set := newLeafCommand(e, "account set", "更新账号凭据，重置冷却与失败计数并重新启用", cmdAccountSet)
	set.Aliases = []string{"update"}
	return newGroupCommand(e, groupSpec{
		name:    "account",
		purpose: "管理上游账号",
		items:   accountSubs,
		unknown: "未知子命令 %q，可用：add | ls | set | rm | test | enable | disable\n用 lax account --help 查看子命令",
	},
		newLeafCommand(e, "account add", "新增一个上游账号", cmdAccountAdd),
		newLeafCommand(e, "account ls", "列出账号", cmdAccountLs),
		set,
		newLeafCommand(e, "account rm", "删除账号", cmdAccountRm),
		newLeafCommand(e, "account test", "检测账号连通性与凭据有效性", cmdAccountTest),
		newLeafCommand(e, "account enable", "启用或停用账号", func(e *Env, fs *leafFlags, args []string) error {
			return cmdAccountSetEnabled(e, fs, args, "enable", true)
		}),
		newLeafCommand(e, "account disable", "启用或停用账号", func(e *Env, fs *leafFlags, args []string) error {
			return cmdAccountSetEnabled(e, fs, args, "disable", false)
		}),
	)
}

func newModelCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "model",
		purpose: "模型同步、探活、别名、单价与手工试打",
		items:   modelSubs,
		unknown: "未知子命令 %q，可用：" + modelSubCommands + "\n用 lax model --help 查看子命令",
	},
		newLeafCommand(e, "model ls", "列出网关可提供的模型", cmdModelLs),
		newLeafCommand(e, "model show", "按账号看一个模型的可达性事实", cmdModelShow),
		newLeafCommand(e, "model add", "手写一条账号能用的模型（同事务写目录与可达性）", cmdModelAdd),
		newLeafCommand(e, "model sync", "从上游拉取模型列表", cmdModelSync),
		newLeafCommand(e, "model probe", "逐个模型探活，把可用模型写进账号", cmdModelProbe),
		newModelAliasCommand(e),
		newModelPriceCommand(e),
		newModelCapabilityCommand(e),
		newModelDialectCommand(e),
		newLeafCommand(e, "model call", "手工发一条请求，打印选号过程", cmdCall),
		newLeafCommand(e, "model decide", "发一条 systemone 决策请求：一个 state 加一组问题，打印每个问题的答案与概率", cmdDecide),
		newLeafCommand(e, "model speak", "发一条语音合成请求（POST /v1/audio/speech），音频写标准输出或文件", cmdSpeak),
		newLeafCommand(e, "model transcribe", "发一条语音转写请求（POST /v1/audio/transcriptions），响应体原样输出", cmdTranscribe),
	)
}

func newModelAliasCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:     "model alias",
		purpose:  "客户端写死的模型名指到一个真实模型名上",
		items:    modelAliasSubs,
		docs:     "https://docs.larkax.com/self-hosted/configure/models",
		examples: []string{"lax model alias set fast glm-4.6"},
		hint:     "完整参数与示例：lax model alias set --help",
		unknown:  "未知子命令 %q，可用：set | unset | ls\n用 lax model alias --help 查看用法",
	},
		newLeafCommand(e, "model alias set", "配置模型别名映射", cmdModelAliasSet),
		newLeafCommand(e, "model alias unset", "删掉一条别名", cmdModelAliasUnset),
		newLeafCommand(e, "model alias ls", "列别名、目标与能否解析", cmdModelAliasLs),
	)
}

func newModelPriceCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:     "model price",
		purpose:  "模型单价，挂在「供应商 × 模型」上",
		items:    modelPriceSubs,
		docs:     "https://docs.larkax.com/self-hosted/configure/pricing",
		examples: []string{"lax model price set glm-4.6 --in 0.6 --out 2.2"},
		hint:     "完整参数与示例：lax model price set --help",
		unknown:  "未知子命令 %q，可用：set | unset | ls | tier | window | rule | test\n用 lax model price --help 查看用法",
	},
		newLeafCommand(e, "model price set", "写模型单价（每百万 token）", cmdModelPriceSet),
		newLeafCommand(e, "model price unset", "清掉模型单价的一项或几项", cmdModelPriceUnset),
		newLeafCommand(e, "model price ls", "列目录里的模型单价", cmdModelPriceLs),
		newModelPriceTierCommand(e),
		newModelPriceWindowCommand(e),
		newModelPriceRuleCommand(e),
		newLeafCommand(e, "model price test", "本地试算模型单次请求花费", cmdModelPriceTest),
	)
}

func newModelPriceTierCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:     "model price tier",
		purpose:  "管理模型的阶梯定价档位（新增或更新档位请用 lax model price set ... --tier）",
		items:    modelPriceTierSubs,
		examples: []string{"lax model price set glm-4.6 --tier 200k --in 1.0 --out 4.0"},
		unknown:  "未知子命令 %q，可用：unset | clear\n用 lax model price tier --help 查看用法",
	},
		newLeafCommand(e, "model price tier unset", "删除指定上限的阶梯档位", cmdModelPriceTierUnset),
		newLeafCommand(e, "model price tier clear", "清空全部阶梯档位", cmdModelPriceTierClear),
	)
}

func newModelPriceWindowCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:     "model price window",
		purpose:  "管理模型的分时计费窗口（新增或更新窗口请用 lax model price set ... --window）",
		items:    modelPriceWindowSubs,
		examples: []string{"lax model price set glm-4.6 --window off-peak --start 00:00 --end 08:00 --multiplier 0.5"},
		unknown:  "未知子命令 %q，可用：unset | clear\n用 lax model price window --help 查看用法",
	},
		newLeafCommand(e, "model price window unset", "删除指定名称的分时窗口", cmdModelPriceWindowUnset),
		newLeafCommand(e, "model price window clear", "清空全部分时窗口", cmdModelPriceWindowClear),
	)
}

func newModelPriceRuleCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "model price rule",
		purpose: "查看或整条导入/清除计费规则",
		items:   modelPriceRuleSubs,
		unknown: "未知子命令 %q，可用：show | set | unset\n用 lax model price rule --help 查看用法",
	},
		newLeafCommand(e, "model price rule show", "查看模型当前生效的计费规则与来源", cmdModelPriceRuleShow),
		newLeafCommand(e, "model price rule set", "从文件或 JSON 字符串写入完整计费规则", cmdModelPriceRuleSet),
		newLeafCommand(e, "model price rule unset", "清除计费规则", cmdModelPriceRuleUnset),
	)
}

func newModelCapabilityCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "model capability",
		purpose: "模型能力声明：入站端点能不能用这个模型、能用哪些音色",
		items:   capabilitySubs,
		unknown: "未知子命令 %q，可用：add | set | unset | ls\n用 lax model capability --help 查看子命令",
	},
		newLeafCommand(e, "model capability add", "把能力、音色或音频格式并进现有声明", func(e *Env, fs *leafFlags, args []string) error {
			return cmdModelCapabilityWrite(e, fs, args, true)
		}),
		newLeafCommand(e, "model capability set", "用新列表替换能力声明，命令行没给的字段不动", func(e *Env, fs *leafFlags, args []string) error {
			return cmdModelCapabilityWrite(e, fs, args, false)
		}),
		newLeafCommand(e, "model capability unset", "清掉一项能力声明", cmdModelCapabilityUnset),
		newLeafCommand(e, "model capability ls", "列出已声明能力、音色与音频格式的模型", cmdModelCapabilityLs),
	)
}

func newModelDialectCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "model dialect",
		purpose: "方言绑定：模型的上游形状由哪条方言描述",
		items:   dialectSubs,
		unknown: "未知子命令 %q，可用：ls | set | unset\n用 lax model dialect --help 查看子命令",
	},
		newLeafCommand(e, "model dialect ls", "列出模型绑定的方言", cmdModelDialectLs),
		newLeafCommand(e, "model dialect set", "把模型的一个操作绑到一条方言", cmdModelDialectSet),
		newLeafCommand(e, "model dialect unset", "删掉一条方言绑定", cmdModelDialectUnset),
	)
}

func newRouteCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "route",
		purpose: "管理选号策略、账号权重与失败处理",
		items:   routeSubs,
		unknown: "未知子命令 %q，可用：ls | show | set | unset | account | explain | policy\n用 lax route --help 查看子命令",
	},
		newLeafCommand(e, "route ls", "列出默认策略与按模型的覆盖", cmdRouteLs),
		newLeafCommand(e, "route show", "查看一个模型或默认策略最终生效的设置", cmdRouteShow),
		newLeafCommand(e, "route set", "设置模型或默认路由策略（整条覆盖）", cmdRouteSet),
		newLeafCommand(e, "route unset", "删除一条策略，回落到默认或内置默认", cmdRouteUnset),
		newRouteAccountCommand(e),
		newLeafCommand(e, "route explain", "预演一次选号，不请求上游、不写记录", cmdRouteExplain),
		newRoutePolicyCommand(e),
	)
}

func newRouteAccountCommand(e *Env) *cobra.Command {
	items := []groupItem{
		{"ls", "列出显式权重，或看一个模型上所有账号的最终权重"},
		{"set", "设置账号默认权重或账号在某个模型上的权重"},
		{"unset", "删除一条账号权重，回落到账号默认或 1"},
	}
	return newGroupCommand(e, groupSpec{
		name:    "route account",
		purpose: "设置账号在选号里的权重",
		items:   items,
		unknown: "未知子命令 %q，可用：ls | set | unset\n用 lax route account --help 查看子命令",
	},
		newLeafCommand(e, "route account ls", "列出账号权重，或看一个模型上所有账号的最终权重", cmdRouteAccountLs),
		newLeafCommand(e, "route account set", "设置账号默认权重或账号在某个模型上的权重", cmdRouteAccountSet),
		newLeafCommand(e, "route account unset", "删除一条账号权重，回落到账号默认或 1", cmdRouteAccountUnset),
	)
}

func newRoutePolicyCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "route policy",
		purpose: "维护上游失败处理策略",
		items:   routePolicySubs,
		unknown: "未知子命令 %q，可用：ls | show | add | set | unset | rm | enable | disable | test\n用 lax route policy --help 查看子命令",
	},
		newLeafCommand(e, "route policy add", "新建一条上游失败处理规则", cmdRoutePolicyAdd),
		newLeafCommand(e, "route policy set", "修改一条规则，只改显式给出的字段", cmdRoutePolicySet),
		newLeafCommand(e, "route policy unset", "清除一条规则里的指定字段", cmdRoutePolicyUnset),
		newLeafCommand(e, "route policy ls", "按生效顺序列出用户规则，再列内置兜底", cmdRoutePolicyLs),
		newLeafCommand(e, "route policy show", "查看一条用户规则的完整字段", cmdRoutePolicyShow),
		newLeafCommand(e, "route policy rm", "删除一条用户规则", cmdRoutePolicyRm),
		newLeafCommand(e, "route policy enable", "启用或停用一条用户规则", func(e *Env, fs *leafFlags, args []string) error {
			return cmdRoutePolicyToggle(e, fs, args, "enable", true)
		}),
		newLeafCommand(e, "route policy disable", "启用或停用一条用户规则", func(e *Env, fs *leafFlags, args []string) error {
			return cmdRoutePolicyToggle(e, fs, args, "disable", false)
		}),
		newLeafCommand(e, "route policy test", "按给定事实预演匹配，不请求上游、不写库", cmdRoutePolicyTest),
	)
}

func newProviderCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "provider",
		purpose: "厂商与路由",
		items:   providerSubs,
		unknown: "未知子命令 %q，可用：ls | add | set | unset | rm | header\n用 lax provider --help 查看子命令",
	},
		newLeafCommand(e, "provider ls", "列出厂商来源与路由概况；--verbose 看完整路径与模型列表", cmdProviderLs),
		newLeafCommand(e, "provider add", "新增厂商与协议路由", cmdProviderAdd),
		newLeafCommand(e, "provider set", "更新厂商地址、路由、默认协议或备注", cmdProviderSet),
		newLeafCommand(e, "provider unset", "删掉一条路由，或清掉模型列表覆盖、默认协议、备注", cmdProviderUnset),
		newLeafCommand(e, "provider rm", "删除厂商与它的全部路由", cmdProviderRm),
		newProviderHeaderCommand(e),
	)
}

func newProviderHeaderCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "provider header",
		purpose: "厂商附加请求头：有些上游除凭据头之外还要额外的头才路由",
		items:   providerHeaderSubs,
		notes: []string{
			"值可写 {session} / {env:NAME} / {model}；协议级覆盖厂商级，ls 与 --json 脱敏。",
		},
		docs:     "https://docs.larkax.com/self-hosted/gateway/conversion",
		examples: []string{"lax provider header set opencode x-opencode-session '{session}' --protocol openai-chat"},
		hint:     "完整参数与示例：lax provider header set --help",
		unknown:  "未知子命令 %q，可用：set | unset | ls\n用 lax provider header --help 查看用法",
	},
		newLeafCommand(e, "provider header set", "声明或覆盖一家厂商的一条附加请求头", cmdProviderHeaderSet),
		newLeafCommand(e, "provider header unset", "删掉一条声明过的附加请求头", cmdProviderHeaderUnset),
		newLeafCommand(e, "provider header ls", "列已声明的附加请求头（值脱敏）", cmdProviderHeaderLs),
	)
}

func newPluginCommand(e *Env) *cobra.Command {
	return newGroupCommand(e, groupSpec{
		name:    "plugin",
		purpose: "插件：安装、升级、回滚、校验、日志、启停、卸载与自检",
		items:   pluginSubs,
		unknown: "未知子命令 %q，可用：install | update | rollback | verify | logs | reload | config | ls | show | enable | disable | rm | doctor\n用 lax plugin --help 查看子命令",
	},
		newLeafCommand(e, "plugin install", "从本地目录、GitHub 仓库或官方插件名安装插件，登记它的厂商、模型与方言", cmdPluginInstall),
		newLeafCommand(e, "plugin update", "把已装插件升到来源里的新版本", cmdPluginUpdate),
		newLeafCommand(e, "plugin rollback", "把活动版本切回一个已装过的旧版本", cmdPluginRollback),
		newLeafCommand(e, "plugin verify", "只做校验不改库：包哈希、manifest 哈希、方言与运行时", cmdPluginVerify),
		newLeafCommand(e, "plugin logs", "查看插件的运行日志", cmdPluginLogs),
		newLeafCommand(e, "plugin reload", "让正在运行的 lax serve 重读插件表", cmdPluginReload),
		newPluginConfigCommand(e),
		newLeafCommand(e, "plugin ls", "列出已装插件，或官方插件仓库里可安装的插件", cmdPluginLs),
		newLeafCommand(e, "plugin show", "查看插件概览、权限、路由与最近事件", cmdPluginShow),
		newLeafCommand(e, "plugin enable", "启用一个插件，网关重启后加载它的方言", func(e *Env, fs *leafFlags, args []string) error {
			return cmdPluginToggle(e, fs, args, true)
		}),
		newLeafCommand(e, "plugin disable", "停用一个插件，网关重启后不再加载它的方言", func(e *Env, fs *leafFlags, args []string) error {
			return cmdPluginToggle(e, fs, args, false)
		}),
		newLeafCommand(e, "plugin rm", "卸载插件，默认保留包目录", cmdPluginRm),
		newLeafCommand(e, "plugin doctor", "检查已装插件：包完整性、manifest 哈希、方言登记与状态", cmdPluginDoctor),
	)
}

func newPluginConfigCommand(e *Env) *cobra.Command {
	items := []groupItem{
		{"get", "读取配置项，没写过的显示 manifest 的默认值"},
		{"set", "写一个配置项"},
		{"unset", "删掉一个配置项，回到默认值"},
	}
	return newGroupCommand(e, groupSpec{
		name:    "plugin config",
		purpose: "插件配置项：manifest 里声明的 options",
		items:   items,
		unknown: "未知子命令 %q，可用：get | set | unset\n用 lax plugin config --help 查看子命令",
	},
		newLeafCommand(e, "plugin config get", "读取插件的配置项", cmdPluginConfigGet),
		newLeafCommand(e, "plugin config set", "写一个插件配置项", cmdPluginConfigSet),
		newLeafCommand(e, "plugin config unset", "删掉一个插件配置项", cmdPluginConfigUnset),
	)
}
