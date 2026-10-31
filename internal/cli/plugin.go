package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"lax/internal/plugin"
	"lax/internal/store"
)

// 插件一组：把声明式的包装进数据目录，登记它带来的厂商、模型与方言，让网关启动时能加载。
//
// 库是活动版本的唯一依据，磁盘上的包是字节的事实：两者对不上时 doctor 报 tampered，
// 网关不加载那个版本。命令面只碰 plugins、plugin_versions 与插件自己带来的厂商目录，
// 手写的厂商、路由与绑定不会被插件改写（冲突一律整笔失败，并给出下一步命令）。

// 插件状态。ok 与 disabled 由启停写，tampered 只由 doctor 写：包内容与安装时记的哈希
// 不一致时，插件不进网关的方言表。
const (
	pluginStateOK       = "ok"
	pluginStateDisabled = "disabled"
	pluginStateTampered = "tampered"
	// pluginStateFailed 由启动加载写：包打不开或方言登不进去时记下原因，网关照常起来。
	pluginStateFailed = "failed"
)

// plugin_versions 上两列的固定取值：来源是本地目录，没有签名，包哈希是自证的。
const (
	pluginVersionSource   = "dir"
	pluginSignatureStatus = "unverified"
)

// pluginSourcePlugin 是写进 provider_models 与 model_operations 的来源标记，
// 与 store 判「这一行是不是插件写的」用的是同一个取值。
const pluginSourcePlugin = "plugin"

// pluginActionInstall 与 pluginActionUpdate 决定一次写库之后该怎么说：
// 安装与升级共用同一个计划结构，只有开头与下一步不同。
const (
	pluginActionInstall = "install"
	pluginActionUpdate  = "update"
)

const pluginInstallUsage = "用法：lax plugin install <目录> [--trust declarative|sandboxed] [--no-enable] [--dry-run] [--yes]\n" +
	"<目录> 里要有 lax.plugin.json，例如 lax plugin install ./my-plugin"

var pluginSubs = []groupItem{
	{"install", "从本地目录安装一个插件：声明式映射，或跑在 moejs 沙箱里的代码"},
	{"update", "把已装插件升到源目录里的新版本，活动版本跟着换过去"},
	{"rollback", "把活动版本切回一个已装过的旧版本"},
	{"verify", "只做校验不改库：包哈希、manifest 哈希、方言与运行时是否与安装时一致"},
	{"logs", "查看插件的运行日志，--follow 持续跟随"},
	{"reload", "让正在运行的 lax serve 重读插件表，不重启进程"},
	{"config", "插件配置项：manifest 里声明的 options（get / set / unset）"},
	{"ls", "列出已装插件：版本、类型、运行时、信任、状态、厂商与方言数"},
	{"show", "显示一个插件的 manifest、权限、版本、厂商、方言、审计与日志"},
	{"enable", "启用一个插件，网关重启后加载它的方言"},
	{"disable", "停用一个插件，网关重启后不再加载它的方言"},
	{"rm", "卸载插件的库记录与它带来的厂商目录，默认保留包目录"},
	{"doctor", "检查已装插件：包完整性、manifest 哈希、沙箱与导出钩子、方言登记与状态"},
}

// 审计里的「谁」。命令行写入的动作都标 cli，serve 收到 SIGHUP 自己重载的标 sighup：
// 后者不是人在敲命令，而是另一个进程被信号叫起来的。
const (
	pluginActorCLI    = "cli"
	pluginActorSIGHUP = "sighup"
)

// cmdPlugin 是 plugin 这一组子命令的入口。
func cmdPlugin(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "plugin", "插件：安装、升级、回滚、校验、日志、启停、卸载与自检", pluginSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：install | update | rollback | verify | logs | reload | config | ls | show | enable | disable | rm | doctor\n用 lax plugin --help 查看子命令")
	}
	switch args[0] {
	case "install":
		return cmdPluginInstall(e, args[1:])
	case "update":
		return cmdPluginUpdate(e, args[1:])
	case "rollback":
		return cmdPluginRollback(e, args[1:])
	case "verify":
		return cmdPluginVerify(e, args[1:])
	case "logs":
		return cmdPluginLogs(e, args[1:])
	case "reload":
		return cmdPluginReload(e, args[1:])
	case "config":
		return cmdPluginConfig(e, args[1:])
	case "ls":
		return cmdPluginLs(e, args[1:])
	case "show":
		return cmdPluginShow(e, args[1:])
	case "enable":
		return cmdPluginToggle(e, args[1:], true)
	case "disable":
		return cmdPluginToggle(e, args[1:], false)
	case "rm":
		return cmdPluginRm(e, args[1:])
	case "doctor":
		return cmdPluginDoctor(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：install | update | rollback | verify | logs | reload | ls | show | enable | disable | rm | doctor\n用 lax plugin --help 查看子命令", args[0])
	}
}

// ---------------------------------------------------------------- 审计

// auditPlugin 写一条审计。写不进去只在 stderr 打一行：
// 审计是记账，账没记上不该让一次已经完成的安装、升级或卸载整体失败。
func auditPlugin(e *Env, st *store.Store, entry store.PluginAudit) {
	if entry.Actor == "" {
		entry.Actor = pluginActorCLI
	}
	if err := st.AppendPluginAudit(entry); err != nil {
		fmt.Fprintf(e.Stderr, "警告：写插件审计失败（%s %s）：%v\n", entry.PluginID, entry.Action, err)
	}
}

// auditPluginPermission 在权限真的变了时再记一条。
// 前后有一边为空不记：那是安装（之前没有版本）或旧版本没留下权限原文，
// 归到 update / rollback 那一条里已经说清楚了。
func auditPluginPermission(e *Env, st *store.Store, id, version, actor, before, after string) {
	if before == "" || after == "" || before == after {
		return
	}
	auditPlugin(e, st, store.PluginAudit{
		PluginID: id, Version: version, Action: store.AuditPermission, Actor: actor,
		PermissionsBeforeJSON: before, PermissionsAfterJSON: after,
	})
}

// activePermissionsJSON 取活动版本入库的权限原文。没有活动版本时返回空串。
func activePermissionsJSON(st *store.Store, id string) string {
	pv, ok, err := st.ActivePluginVersion(id)
	if err != nil || !ok {
		return ""
	}
	return pv.PermissionsJSON
}

// auditPluginReload 给每个会被网关加载的插件记一条热加载审计。
//
// 审计行按插件存（plugin_audit 的主键带插件 ID），一次整表重载因此是多条行；
// only 非空时只记那一个，给 `plugin reload <id>` 用。
func auditPluginReload(e *Env, st *store.Store, actor, only string) {
	plugins, err := st.ListPlugins()
	if err != nil {
		fmt.Fprintf(e.Stderr, "警告：读插件表失败，热加载审计没写成：%v\n", err)
		return
	}
	for _, p := range plugins {
		if !p.Enabled || p.ActiveVersion == "" || (only != "" && p.ID != only) {
			continue
		}
		auditPlugin(e, st, store.PluginAudit{
			PluginID: p.ID, Version: p.ActiveVersion, Action: store.AuditReload, Actor: actor,
		})
	}
}

// unknownPluginError 说明库里没有这一行，并列出已装的 ID 与最接近的一个。
// 插件 ID 是反向域名，拼错一位很难自己看出来。
func unknownPluginError(st *store.Store, id string) error {
	plugins, err := st.ListPlugins()
	if err != nil {
		return err
	}
	if len(plugins) == 0 {
		return fmt.Errorf("插件 %q 没有安装，现在一个插件都没有：先运行 lax plugin install <目录>", id)
	}
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.ID)
	}
	msg := fmt.Sprintf("插件 %q 没有安装（已装：%s）", id, strings.Join(names, "、"))
	if closest := closestName(names, id); closest != "" {
		msg += fmt.Sprintf("；最接近的是 %q", closest)
	}
	return errors.New(msg + "；用 lax plugin ls 看全部插件")
}

// installedPlugin 按库里的活动版本读磁盘上的包。包不在或读不动时返回错误，
// 调用方按「这一项看不全」处理，不让整条命令失败。
func installedPlugin(dataDir string, p store.Plugin) (plugin.Plugin, error) {
	if p.ActiveVersion == "" {
		return plugin.Plugin{}, fmt.Errorf("插件 %s 没有活动版本", p.ID)
	}
	return plugin.LoadDir(pluginVersionDir(dataDir, p.ID, p.ActiveVersion))
}

// storeManifest 读活动版本入库的 manifest 快照。包被删掉时 ls 与 show 靠它显示厂商。
func storeManifest(st *store.Store, p store.Plugin) (plugin.Manifest, bool) {
	pv, ok, err := st.ActivePluginVersion(p.ID)
	if err != nil || !ok || pv.ManifestJSON == "" {
		return plugin.Manifest{}, false
	}
	var m plugin.Manifest
	if err := json.Unmarshal([]byte(pv.ManifestJSON), &m); err != nil {
		return plugin.Manifest{}, false
	}
	return m, true
}

// providerIDs 取 manifest 里的厂商 ID，顺序照 manifest。
func providerIDs(m plugin.Manifest) []string {
	out := make([]string, 0, len(m.Providers))
	for _, p := range m.Providers {
		out = append(out, p.ID)
	}
	return out
}

// permissionSummary 把 manifest 的权限翻成一行中文。文件、进程与环境变量本版恒为 none，
// 照样写出来：装之前要看到「没有」本身，省略会被读成「没说」。
func permissionSummary(p plugin.Permissions) string {
	parts := []string{networkSummary(p.Network)}
	if len(p.Secrets.Account) == 0 {
		parts = append(parts, "账号凭据 无")
	} else {
		parts = append(parts, fmt.Sprintf("账号凭据 %s（宿主注入，插件看不到原文）", strings.Join(p.Secrets.Account, "、")))
	}
	for _, item := range []struct{ name, value string }{
		{"文件", p.Filesystem}, {"进程", p.Process}, {"环境变量", p.Env},
	} {
		value := item.value
		if value == "" {
			value = "none"
		}
		parts = append(parts, item.name+" "+value)
	}
	return strings.Join(parts, "；")
}

func networkSummary(n plugin.NetworkPerm) string {
	switch n.Mode {
	case "", "none":
		return "网络 none"
	case "host-proxy":
		return "网络 host-proxy → " + strings.Join(n.Hosts, "、")
	}
	return "网络 " + n.Mode
}

// ---------------------------------------------------------------- install

// pluginPlanModel 是目录里的一个模型，Operations 的键是操作名、值是方言 ID。
type pluginPlanModel struct {
	Model      string            `json:"model"`
	UpstreamID string            `json:"upstream_id"`
	Operations map[string]string `json:"operations"`
}

// pluginPlanProvider 是一次安装对一家厂商要说清的事实：路由、模型、方言与别名。
type pluginPlanProvider struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	BaseURL  string            `json:"base_url"`
	Routes   []string          `json:"routes"`
	Models   []pluginPlanModel `json:"models"`
	Dialects []string          `json:"dialects"`
	Aliases  []string          `json:"aliases"`
}

// pluginInstallPlan 是一次安装要写入的全部事实。dry-run 与真装用同一份，
// 两边输出因此对得上：dry-run 里看到的就是真装会写的。
type pluginInstallPlan struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Kind        string               `json:"kind"`
	Runtime     string               `json:"runtime"`
	Trust       string               `json:"trust"`
	Target      string               `json:"target"`
	PackageSHA  string               `json:"package_sha256"`
	Permissions string               `json:"permissions"`
	Providers   []pluginPlanProvider `json:"providers"`
	DryRun      bool                 `json:"dry_run"`
	Enabled     bool                 `json:"enabled"`
}

// pluginRoute 是一家厂商要打的端点：哪条方言、哪种线协议、哪个路径。
type pluginRoute struct {
	Dialect  string
	Protocol string
	Path     string
}

// dialectRoutes 列出一家厂商要建的端点：插件方言按它的 request.path，宿主方言按
// plugin.DefaultRoutePath 推。同一家厂商的两条方言落在同一条线协议上时只留第一条，
// 因为地址按厂商路由走，多建的路由没地方生效。
func dialectRoutes(pkg plugin.Plugin, providerID string) []pluginRoute {
	decls := make(map[string]plugin.DialectDecl, len(pkg.Dialects))
	for _, d := range pkg.Dialects {
		decls[d.ID] = d
	}

	// 按目录里引用的方言收集，而不是按 dialects 文件：目录可以只引用宿主方言。
	order := make([]string, 0, len(pkg.DialectsByProvider[providerID]))
	seenDialect := map[string]bool{}
	add := func(id string) {
		if id == "" || seenDialect[id] {
			return
		}
		seenDialect[id] = true
		order = append(order, id)
	}
	for _, id := range pkg.DialectsByProvider[providerID] {
		add(id)
	}
	for _, model := range pkg.Catalogs[providerID].Models {
		for _, operation := range slices.Sorted(maps.Keys(model.Operations)) {
			add(model.Operations[operation].Dialect)
		}
	}

	seen := map[string]bool{}
	out := make([]pluginRoute, 0, len(order))
	for _, id := range order {
		wire, path := "", ""
		if d, ok := decls[id]; ok {
			wire = d.UpstreamWire
			if d.Request != nil {
				path = d.Request.Path
			} else {
				// hook 方言不写声明式 request，端点只能按线协议的默认路径来；
				// 真正发去哪由每次 Plan 的 path 决定。
				path = plugin.DefaultRoutePath(wire)
			}
		} else if plugin.IsHostDialect(id) {
			for _, candidate := range []string{plugin.WireAnthropic, plugin.WireOpenAIChat, plugin.WireOpenAIResponses, plugin.WireGemini, plugin.WireEmbeddings, plugin.WireSpeech, plugin.WireTranscriptions, plugin.WireSystemOne} {
				if plugin.WireDialect(candidate) == id {
					wire = candidate
					path = plugin.DefaultRoutePath(candidate)
					break
				}
			}
		}
		if wire == "" || path == "" || seen[wire] {
			continue
		}
		seen[wire] = true
		out = append(out, pluginRoute{Dialect: id, Protocol: wire, Path: path})
	}
	return out
}

// checkExecutableOperations 只放行本版真正执行的方言所承载的操作。
//
// 两条形式的插件方言（声明式映射、hook 代码）都能承载语音；对话、向量化与决策请求
// 只能由 hook 方言承载：宿主编好报文后交给它打补丁，而声明式映射没有读这些报文的节点。
// 声明式方言绑到这些操作上会被拒，以免装下去却只能在语音那条路上生效。
func checkExecutableOperations(pkg plugin.Plugin) error {
	hookDialects := make(map[string]bool, len(pkg.Dialects))
	wires := make(map[string]string, len(pkg.Dialects))
	for _, decl := range pkg.Dialects {
		wires[decl.ID] = decl.UpstreamWire
		if decl.Implementation.Prepare == plugin.ImplHook && decl.Implementation.Decode == plugin.ImplHook {
			hookDialects[decl.ID] = true
		}
	}
	for _, catalog := range pkg.Catalogs {
		for _, model := range catalog.Models {
			for _, operation := range slices.Sorted(maps.Keys(model.Operations)) {
				dialect := model.Operations[operation].Dialect
				switch operation {
				case plugin.OpSpeechSynthesize, plugin.OpSpeechTranscribe:
					continue
				}
				if plugin.IsHostDialect(dialect) || hookDialects[dialect] {
					continue
				}
				hint := "改成宿主方言（lax/ 前缀的 dialects 引用），或把这条方言写成 hook 实现"
				if wire, ok := wires[dialect]; ok {
					if host := plugin.WireDialect(wire); host != "" {
						hint = fmt.Sprintf("改成宿主方言 %s（端点按 %s 建），或把这条方言写成 hook 实现",
							host, plugin.DefaultRoutePath(wire))
					}
				}
				return fmt.Errorf("模型 %s 的 %s 绑定了声明式方言 %s：对话、向量化与决策请求要读宿主编好的报文，"+
					"声明式映射没有这类节点，%s", model.Model, operation, dialect, hint)
			}
		}
	}
	return nil
}

// buildInstallPlan 把解析好的包翻成「要写进库的事实」：每家厂商一条目录，含路由、
// 模型、按操作的方言绑定与别名。runtime 与 trust 一并定下：网关启动时按它们选加载路径。
func buildInstallPlan(pkg plugin.Plugin, trust string) (pluginInstallPlan, []store.PluginCatalog) {
	m := pkg.Manifest
	plan := pluginInstallPlan{
		ID:          m.ID,
		Name:        m.Name,
		Version:     m.Version,
		Kind:        m.Kind,
		Runtime:     m.Runtime,
		Trust:       trust,
		Permissions: permissionSummary(m.Permissions),
	}

	catalogs := make([]store.PluginCatalog, 0, len(m.Providers))
	for _, decl := range m.Providers {
		item := pluginPlanProvider{ID: decl.ID, Name: decl.Name, BaseURL: decl.BaseURL}
		catalog := store.PluginCatalog{Provider: decl.ID, Name: decl.Name, BaseURL: decl.BaseURL}

		item.Dialects = append(item.Dialects, pkg.DialectsByProvider[decl.ID]...)
		for _, route := range dialectRoutes(pkg, decl.ID) {
			item.Routes = append(item.Routes, route.Protocol+" "+route.Path)
			catalog.Routes = append(catalog.Routes, store.ProviderRoute{
				Provider: decl.ID, Protocol: route.Protocol, Path: route.Path,
			})
		}

		for _, model := range pkg.Catalogs[decl.ID].Models {
			upstream := model.UpstreamID
			if upstream == "" {
				upstream = model.Model
			}
			entry := store.PluginModelEntry{Model: model.Model, UpstreamID: upstream}
			planned := pluginPlanModel{Model: model.Model, UpstreamID: upstream, Operations: map[string]string{}}
			for _, op := range slices.Sorted(maps.Keys(model.Operations)) {
				opDecl := model.Operations[op]
				entry.Operations = append(entry.Operations, store.ModelOperation{
					Operation:        op,
					DialectID:        opDecl.Dialect,
					CapabilitiesJSON: capabilitiesJSON(opDecl.Capabilities),
				})
				planned.Operations[op] = opDecl.Dialect
			}
			catalog.Models = append(catalog.Models, entry)
			item.Models = append(item.Models, planned)
			if model.Model != upstream {
				item.Aliases = append(item.Aliases, model.Model+" → "+upstream)
			}
		}
		plan.Providers = append(plan.Providers, item)
		catalogs = append(catalogs, catalog)
	}
	return plan, catalogs
}

// capabilitiesJSON 把插件的能力声明转成落库的 JSON：三态值原样写（true、false、"unknown"），
// 没声明的字段不写——省略与 false 是两件事，写成 null 会被读侧当成「明确不支持」。
func capabilitiesJSON(caps *plugin.Capabilities) string {
	if caps == nil {
		return "{}"
	}
	out := map[string]any{}
	put := func(key string, t *plugin.Tri) {
		if t == nil {
			return
		}
		switch t.Value {
		case "true":
			out[key] = true
		case "false":
			out[key] = false
		default:
			out[key] = "unknown"
		}
	}
	put("supported", caps.Supported)
	put("streaming", caps.Streaming)
	put("tools", caps.Tools)
	put("reasoning", caps.Reasoning)
	put("json_mode", caps.JSONMode)
	put("vision", caps.Vision)
	putList := func(key string, l *plugin.TriList) {
		if l == nil {
			return
		}
		if l.Unknown {
			out[key] = "unknown"
			return
		}
		values := l.Values
		if values == nil {
			values = []string{}
		}
		out[key] = values
	}
	putList("formats", caps.Formats)
	putList("voices", caps.Voices)
	encoded, err := json.Marshal(out)
	if err != nil {
		// 只装了布尔、字符串与字符串数组，编不出来说明这里的代码写错了，落一个空声明。
		return "{}"
	}
	return string(encoded)
}

// checkDialectConflicts 在「内置方言 + 其余已装插件」的范围里试一遍方言 ID：撞车时整包拒绝，
// 内置的 lax/ 前缀也不许插件冒用。同一个插件自己的旧版本跳过，否则新版本的方言跟旧版本撞名，
// 升级永远装不上。
//
// 这里只比 ID，不编译别人的方言：已装的 moejs 插件要开沙箱才能编，安装时不该跑别人的插件代码。
func checkDialectConflicts(st *store.Store, dataDir string, pkg plugin.Plugin) error {
	// 先查这一包自己的规则：ID 归属、形状、包内重复、hook 方言与 runtime 是否配套。
	if err := pkg.CheckDialects(nil); err != nil {
		return fmt.Errorf("%v\n下一步：把 dialects.json 里那条方言改对（插件方言必须用 <插件 ID>/ 前缀），或把 manifest 的 runtime 改成 moejs", err)
	}
	taken := map[string]string{}
	for _, id := range plugin.DefaultRegistry().IDs() {
		taken[id] = "内置方言"
	}
	plugins, err := st.ListPlugins()
	if err != nil {
		return err
	}
	for _, p := range plugins {
		if p.ID == pkg.Manifest.ID || p.ActiveVersion == "" {
			continue
		}
		// 读不动的插件登记不进网关，也就占不住方言 ID；它由 doctor 报出来。
		loaded, err := installedPlugin(dataDir, p)
		if err != nil {
			continue
		}
		for _, decl := range loaded.Dialects {
			taken[decl.ID] = "插件 " + p.ID
		}
	}
	for _, decl := range pkg.Dialects {
		if owner, exists := taken[decl.ID]; exists {
			return fmt.Errorf("方言 %s 已经由%s占用：一个 ID 只能有一个实现\n下一步：把插件 dialects.json 里的方言 ID 换一个，或先 lax plugin rm 掉占用它的插件", decl.ID, owner)
		}
	}
	return nil
}

// resolveTrust 定下这次安装的信任级别。默认按 manifest 的 runtime 推断：带代码的只能按沙箱装，
// 声明式没有代码可跑。显式给的 --trust 与推断不一致就是用法错误，不静默改成推断值。
func resolveTrust(m plugin.Manifest, requested string) (string, error) {
	inferred := plugin.TrustDeclarative
	if m.Runtime == plugin.RuntimeMoejs {
		inferred = plugin.TrustSandboxed
	}
	switch requested {
	case "":
		return inferred, nil
	case plugin.TrustDeclarative, plugin.TrustSandboxed:
	case plugin.TrustTrustedWorker:
		return "", usagef("--trust trusted-worker 本版不支持：它要独立进程，以及文件、进程与环境变量权限；现在只有 declarative 与 sandboxed 两级，把 --trust 去掉或写 sandboxed")
	default:
		return "", usagef("--trust 是 %q：只能是 declarative 或 sandboxed", requested)
	}
	if requested == inferred {
		return requested, nil
	}
	if m.Runtime == plugin.RuntimeMoejs {
		return "", usagef("插件 runtime 是 moejs，--trust declarative 不成立：它带代码，只能按沙箱装，去掉 --trust 或写 --trust sandboxed")
	}
	return "", usagef("插件 runtime 是 declarative，--trust sandboxed 升不上去：包里只有声明式映射，没有代码可跑，去掉 --trust")
}

// cmdPluginInstall 是安装流水线：解析校验 → 查版本与方言冲突 → 复制到 installed/ → 写目录 → 写版本行 → 启停。
func cmdPluginInstall(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin install", "从本地目录安装插件，登记它的厂商、模型与方言")
	noEnable := fs.Bool("no-enable", false, "只安装，不启用")
	trust := fs.String("trust", "", "信任级别：declarative 或 sandboxed，默认按 manifest 的 runtime 推断")
	dryRun := fs.Bool("dry-run", false, "只打印将要写入的厂商、路由、模型、方言、别名与权限，不写库也不复制文件")
	yes := fs.Bool("yes", false, "切换已装插件的活动版本时必给：装第二个版本会换掉正在跑的那一个")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("%s\n每次只装一个插件", pluginInstallUsage)
	}
	src := fs.Arg(0)

	// 1 解析与校验：manifest、方言文件、模型目录与三者之间的引用都在这一步查完，
	// 错误文案里带文件名与字段路径，改完直接重跑同一条命令。
	pkg, err := plugin.LoadDir(src)
	if err != nil {
		return err
	}
	trustLevel, err := resolveTrust(pkg.Manifest, *trust)
	if err != nil {
		return err
	}
	if err := checkExecutableOperations(pkg); err != nil {
		return err
	}
	packageSHA, err := plugin.PluginDirHash(src)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	plan, catalogs := buildInstallPlan(pkg, trustLevel)
	plan.Target = pluginVersionDir(dataDir, plan.ID, plan.Version)
	plan.PackageSHA = packageSHA
	plan.Enabled = !*noEnable

	// 2 版本冲突：版本行不可变，同一个版本再装一次没有可改的东西。
	versions, err := a.st.PluginVersions(plan.ID)
	if err != nil {
		return err
	}
	installed := make([]string, 0, len(versions))
	for _, v := range versions {
		if v.Version == plan.Version {
			return fmt.Errorf("插件 %s 的版本 %s 已经装过（已装版本：%s）：已安装的版本不可变，改完插件把 lax.plugin.json 里的 version 换成新号再装；不要这个插件就 lax plugin rm %s",
				plan.ID, plan.Version, strings.Join(versionStrings(versions), "、"), plan.ID)
		}
		installed = append(installed, v.Version)
	}

	// 3 方言冲突：同一个方言 ID 只能有一个实现，内置的 lax/ 前缀不许插件冒用。
	if err := checkDialectConflicts(a.st, dataDir, pkg); err != nil {
		return err
	}

	if *dryRun {
		plan.DryRun = true
		return printInstallPlan(e, plan, pluginActionInstall)
	}

	// 4 换活动版本是显式决定：旧版本行还在，网关重启后跑的是新版本。
	if len(installed) > 0 && !*yes {
		return fmt.Errorf("插件 %s 已经装过 %s，这次装 %s 会把活动版本切过去：确认切换就加 --yes；先看差异用 lax plugin install %s --dry-run",
			plan.ID, strings.Join(installed, "、"), plan.Version, src)
	}

	// 5 复制：先落 staging，再整目录 rename 到 installed/<ID>/<版本>/。
	if err := copyPluginTree(src, plan.Target, pluginStagingRoot(dataDir)); err != nil {
		return err
	}
	rollback := func(cause error) error {
		_ = os.RemoveAll(plan.Target)
		return fmt.Errorf("%v\n已删掉刚复制到 %s 的包；厂商目录是逐家一次事务，前面几家可能已经写进去了，用 lax plugin rm %s 清掉（插件行没写进去时改用 lax provider rm <厂商>）",
			cause, plan.Target, plan.ID)
	}

	// 6 厂商目录：逐家一次事务，冲突整笔回滚。
	for _, catalog := range catalogs {
		if err := a.st.ApplyPluginCatalog(plan.ID, catalog, pluginSourcePlugin); err != nil {
			return rollback(fmt.Errorf("登记厂商 %s 失败：%w", catalog.Provider, err))
		}
	}

	// 7 版本行：manifest 与权限原文一并入库，doctor 与 show 之后不必再猜装的是什么。
	permissionsJSON, err := json.Marshal(pkg.Manifest.Permissions)
	if err != nil {
		return rollback(fmt.Errorf("序列化权限失败：%w", err))
	}
	if err := a.st.PutPluginVersion(store.PluginVersion{
		PluginID:        plan.ID,
		Version:         plan.Version,
		Kind:            pkg.Manifest.Kind,
		Trust:           plan.Trust,
		ABI:             pkg.Manifest.ABI,
		Runtime:         pkg.Manifest.Runtime,
		ManifestJSON:    string(pkg.ManifestBytes),
		PermissionsJSON: string(permissionsJSON),
		PackageSHA256:   packageSHA,
		ManifestSHA256:  plugin.ManifestHash(pkg.ManifestBytes),
		Source:          pluginVersionSource,
		SignatureStatus: pluginSignatureStatus,
	}, true); err != nil {
		return rollback(err)
	}

	// 8 启停：--no-enable 只装不用，状态写 disabled，网关因此不会加载它。
	if *noEnable {
		if err := a.st.SetPluginEnabled(plan.ID, false); err != nil {
			return err
		}
		if err := a.st.SetPluginState(plan.ID, pluginStateDisabled, ""); err != nil {
			return err
		}
	}

	// 9 审计：装了什么、带什么权限。审计写不进去不让命令失败，它不改变已经落库的事实。
	auditPlugin(e, a.st, store.PluginAudit{
		PluginID: plan.ID, Version: plan.Version, Action: store.AuditInstall,
		PermissionsAfterJSON: string(permissionsJSON),
	})
	return printInstallPlan(e, plan, pluginActionInstall)
}

// versionStrings 把版本行压成版本号列表。
func versionStrings(versions []store.PluginVersion) []string {
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.Version)
	}
	return out
}

// copyPluginTree 把源目录复制到 staging，再整目录改名到目标位置。
//
// 符号链接一律拒绝：链接可以让包里的一个相对路径指到包外，装进来等于替插件开了宿主侧的后门。
// 权限收紧到 0o700 目录、0o600 文件：包只需要被当前用户读。同一个文件系统内 rename 是原子的，
// 因此 installed/ 下不会出现装到一半的目录。
func copyPluginTree(src, target, stagingBase string) error {
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("目录 %s 已经存在：已安装的版本不可变，先 lax plugin rm 掉它，或换 manifest 里的版本号", target)
	}
	if err := os.MkdirAll(stagingBase, 0o700); err != nil {
		return fmt.Errorf("建临时目录 %s 失败：%v；检查数据目录的权限", stagingBase, err)
	}
	tmp, err := os.MkdirTemp(stagingBase, "install-")
	if err != nil {
		return fmt.Errorf("在 %s 里建临时目录失败：%v；检查数据目录的权限与剩余空间", stagingBase, err)
	}
	moved := false
	defer func() {
		if !moved {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := copyTree(src, tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("建目录 %s 失败：%v；检查数据目录的权限", filepath.Dir(target), err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("把 %s 移到 %s 失败：%v；两者要在同一个文件系统上，也别有另一个安装正在写同一个插件", tmp, target, err)
	}
	moved = true
	return nil
}

// copyTree 逐个目录项复制。符号链接用 Lstat 判，不跟随。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("遍历 %s 失败：%v", path, walkErr)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("读 %s 失败：%v", path, err)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("插件包里有符号链接 %s：链接能指到包外，安装不复制它；把它换成普通文件再装", path)
		case info.IsDir():
			return os.MkdirAll(to, 0o700)
		case info.Mode().IsRegular():
			return copyPluginFile(path, to)
		}
		return fmt.Errorf("插件包里有特殊文件 %s：只装普通文件与目录，删掉它再装", path)
	})
}

// copyPluginFile 复制一个文件，权限固定 0o600：插件包里的可执行位不带到数据目录里。
func copyPluginFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读 %s 失败：%v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("写 %s 失败：%v；检查数据目录的权限与剩余空间", dst, err)
	}
	return nil
}

// printInstallPlan 打一次安装或升级的结论。dry-run 与真写库共用它，两边说的是同一件事。
func printInstallPlan(e *Env, plan pluginInstallPlan, action string) error {
	if e.JSON {
		return e.printJSON(plan)
	}
	done, will := "已安装插件", "将安装插件"
	if action == pluginActionUpdate {
		done, will = "已更新插件", "将更新插件"
	}
	prefix := done
	if plan.DryRun {
		prefix = will
	}
	fmt.Fprintf(e.Stdout, "%s %s %s（%s，%s）\n", prefix, plan.ID, plan.Version, plan.Kind, plan.Trust)
	if plan.DryRun {
		fmt.Fprintf(e.Stdout, "包目录 %s（dry-run：不复制、不写库）\n", plan.Target)
	} else {
		fmt.Fprintf(e.Stdout, "包目录 %s\n", plan.Target)
	}
	fmt.Fprintf(e.Stdout, "包哈希 %s\n", plan.PackageSHA)
	// 运行时与信任写在一起：带代码的插件跑在沙箱里，超时是它的硬上限。
	if plan.Runtime == plugin.RuntimeMoejs {
		fmt.Fprintf(e.Stdout, "运行时 %s（沙箱，钩子超时 %s/%s）\n", plan.Runtime, plugin.DefaultPrepareTimeout, plugin.DefaultDecodeTimeout)
	} else {
		fmt.Fprintf(e.Stdout, "运行时 %s（不执行代码）\n", plan.Runtime)
	}
	fmt.Fprintf(e.Stdout, "权限 %s\n", plan.Permissions)
	for _, p := range plan.Providers {
		fmt.Fprintf(e.Stdout, "厂商 %s · %s · %s\n", p.ID, p.Name, p.BaseURL)
		fmt.Fprintf(e.Stdout, "  路由 %s\n", cellOrNone(p.Routes))
		fmt.Fprintf(e.Stdout, "  方言 %s\n", cellOrNone(p.Dialects))
		fmt.Fprintf(e.Stdout, "  别名 %s\n", cellOrNone(p.Aliases))
		for _, m := range p.Models {
			ops := make([]string, 0, len(m.Operations))
			for _, op := range slices.Sorted(maps.Keys(m.Operations)) {
				ops = append(ops, op+" → "+m.Operations[op])
			}
			fmt.Fprintf(e.Stdout, "  模型 %s（上游 %s）：%s\n", m.Model, m.UpstreamID, strings.Join(ops, "、"))
		}
	}
	switch {
	case plan.DryRun && action == pluginActionUpdate:
		fmt.Fprintf(e.Stdout, "下一步：加 --yes 把活动版本切到 %s（只打印不写库用 --dry-run）\n", plan.Version)
	case plan.DryRun:
		fmt.Fprintln(e.Stdout, "下一步：去掉 --dry-run 真正安装")
	case !plan.Enabled:
		fmt.Fprintf(e.Stdout, "下一步：lax plugin enable %s 启用它，或直接 lax plugin doctor %s 复核\n", plan.ID, plan.ID)
	case action == pluginActionUpdate:
		fmt.Fprintf(e.Stdout, "下一步：lax plugin doctor %s 复核新版本，lax plugin reload %s 让正在跑的网关换上新版本\n", plan.ID, plan.ID)
	default:
		fmt.Fprintf(e.Stdout, "下一步：lax plugin doctor %s 复核包完整性，重启 lax serve 后网关会加载它的方言\n", plan.ID)
	}
	return nil
}

// cellOrNone 把空列表写成「无」：一个空白行会让人以为输出被截断了。
func cellOrNone(values []string) string {
	if len(values) == 0 {
		return "无"
	}
	return strings.Join(values, "、")
}

// ---------------------------------------------------------------- update

const pluginUpdateUsage = "用法：lax plugin update <插件 ID> --source <目录> [--dry-run] [--yes]\n" +
	"<目录> 里要有新版本的 lax.plugin.json，版本号要与活动版本不同；先看差异用 --dry-run"

// cmdPluginUpdate 把已装插件升到源目录里的新版本。
//
// 流水线与 install 是同一套：解析校验 → 版本与方言冲突 → 复制到 installed/ → 写目录与版本行。
// 区别在失败的时候：活动版本只在最后一步（版本行入库）才切，包或目录写不进去时旧版本仍然可服务。
func cmdPluginUpdate(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin update", "把已装插件升到源目录里的新版本")
	source := fs.String("source", "", "新版本的包目录，目录里要有 lax.plugin.json")
	dryRun := fs.Bool("dry-run", false, "只打印将要写入的变化，不复制也不写库")
	yes := fs.Bool("yes", false, "确实要切换活动版本；不给时只打印将发生的变化")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" || strings.TrimSpace(*source) == "" {
		return usagef("%s", pluginUpdateUsage)
	}
	id := strings.TrimSpace(fs.Arg(0))
	src := strings.TrimSpace(*source)

	// 1 解析与校验：用与 install 同一套，包写得不对在这里就露出来，库一个字节不动。
	pkg, err := plugin.LoadDir(src)
	if err != nil {
		return err
	}
	if pkg.Manifest.ID != id {
		return usagef("源目录里的插件是 %s，命令给的是 %s：一次只升一个插件，改对 ID 或换个目录", pkg.Manifest.ID, id)
	}
	// 信任级别按新包的 runtime 推断，规则与 install 一致：带代码的只能按沙箱装。
	trustLevel, err := resolveTrust(pkg.Manifest, "")
	if err != nil {
		return err
	}
	if err := checkExecutableOperations(pkg); err != nil {
		return err
	}
	packageSHA, err := plugin.PluginDirHash(src)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	p, ok, err := a.st.PluginByID(id)
	if err != nil {
		return err
	}
	if !ok {
		return unknownPluginError(a.st, id)
	}

	// 2 版本规则：活动版本就是源目录里这个号，已经切到它了，没什么可升。
	// 版本行不可变，换号才能升级。
	if pkg.Manifest.Version == p.ActiveVersion {
		return fmt.Errorf("插件 %s 的活动版本就是 %s：换个数才叫升级，把 lax.plugin.json 里的 version 改成新号，或用 lax plugin rollback %s --version <旧版本> 切回去",
			id, pkg.Manifest.Version, id)
	}
	versions, err := a.st.PluginVersions(id)
	if err != nil {
		return err
	}
	for _, v := range versions {
		if v.Version == pkg.Manifest.Version {
			return fmt.Errorf("插件 %s 的版本 %s 已经装过（已装版本：%s）：已安装的版本不可变，换个新号再升，或直接 lax plugin rollback %s --version %s 切回去",
				id, pkg.Manifest.Version, strings.Join(versionStrings(versions), "、"), id, pkg.Manifest.Version)
		}
	}

	plan, catalogs := buildInstallPlan(pkg, trustLevel)
	plan.Target = pluginVersionDir(dataDir, plan.ID, plan.Version)
	plan.PackageSHA = packageSHA
	// 启停状态跟着插件走：升级不改用户对它的启停决定。
	plan.Enabled = p.Enabled

	// 3 方言冲突：同一个 ID 只能有一个实现，装新版本不能偷别人的登记。
	if err := checkDialectConflicts(a.st, dataDir, pkg); err != nil {
		return err
	}

	if *dryRun {
		plan.DryRun = true
		return printInstallPlan(e, plan, pluginActionUpdate)
	}
	if !*yes {
		// 先把将发生的变化摆出来，再把 --yes 的要求当错误给出：
		// 不给 --yes 时退出码是 1，不是静默不动——调用方靠退出码判成败。
		plan.DryRun = true
		if err := printInstallPlan(e, plan, pluginActionUpdate); err != nil {
			return err
		}
		return fmt.Errorf("插件 %s 的活动版本还是 %s，这次要切到 %s：确认切换就加 --yes", id, p.ActiveVersion, plan.Version)
	}

	// 4 复制：目标目录装完之前不存在，活动版本还指着旧的。
	if err := copyPluginTree(src, plan.Target, pluginStagingRoot(dataDir)); err != nil {
		return err
	}
	rollback := func(cause error) error {
		_ = os.RemoveAll(plan.Target)
		return fmt.Errorf("%v\n已删掉刚复制到 %s 的包，活动版本仍是 %s；厂商目录是逐家一次事务，前面几家可能已经写进去了。"+
			"按上面的提示把撞车的行清掉后重跑同一条 update：目录写入是幂等的，缺的那几家会补上",
			cause, plan.Target, p.ActiveVersion)
	}

	// 5 厂商目录：逐家一次事务，冲突整笔回滚。新版本引用的厂商、模型与方言从这一行开始才成立。
	for _, catalog := range catalogs {
		if err := a.st.ApplyPluginCatalog(id, catalog, pluginSourcePlugin); err != nil {
			return rollback(fmt.Errorf("登记厂商 %s 失败：%w", catalog.Provider, err))
		}
	}

	permissionsJSON, err := json.Marshal(pkg.Manifest.Permissions)
	if err != nil {
		return rollback(fmt.Errorf("序列化权限失败：%w", err))
	}
	before := activePermissionsJSON(a.st, id)

	// 6 版本行入库并切活动版本：这是唯一改活动指针的一步，之前任何一步失败旧版本都还能服务。
	if err := a.st.PutPluginVersion(store.PluginVersion{
		PluginID:        id,
		Version:         plan.Version,
		Kind:            pkg.Manifest.Kind,
		Trust:           plan.Trust,
		ABI:             pkg.Manifest.ABI,
		Runtime:         pkg.Manifest.Runtime,
		ManifestJSON:    string(pkg.ManifestBytes),
		PermissionsJSON: string(permissionsJSON),
		PackageSHA256:   packageSHA,
		ManifestSHA256:  plugin.ManifestHash(pkg.ManifestBytes),
		Source:          pluginVersionSource,
		SignatureStatus: pluginSignatureStatus,
	}, true); err != nil {
		return rollback(err)
	}

	// 7 审计：这次从哪个版本升到哪个版本，权限变了就另记一条。
	auditPlugin(e, a.st, store.PluginAudit{
		PluginID: id, Version: plan.Version, Action: store.AuditUpdate, Actor: pluginActorCLI,
		PermissionsBeforeJSON: before, PermissionsAfterJSON: string(permissionsJSON),
	})
	auditPluginPermission(e, a.st, id, plan.Version, pluginActorCLI, before, string(permissionsJSON))
	return printInstallPlan(e, plan, pluginActionUpdate)
}

// ---------------------------------------------------------------- rollback

// cmdPluginRollback 把活动版本切回一个已装过的旧版本，并按那个版本重写厂商目录事实。
//
// 切换活动指针与写目录事实是两件事：指针先切（任务要求的顺序），目录写不进去时把指针改回来，
// 否则库里的「活动版本」会指着一个没有目录事实的版本。
func cmdPluginRollback(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin rollback", "把活动版本切回一个已装过的旧版本")
	version := fs.String("version", "", "要切回的已装版本号")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" || strings.TrimSpace(*version) == "" {
		return usagef("用法：lax plugin rollback <插件 ID> --version <已装版本>\n已装版本用 lax plugin show <插件 ID> 看")
	}
	id := strings.TrimSpace(fs.Arg(0))
	target := strings.TrimSpace(*version)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	p, ok, err := a.st.PluginByID(id)
	if err != nil {
		return err
	}
	if !ok {
		return unknownPluginError(a.st, id)
	}
	versions, err := a.st.PluginVersions(id)
	if err != nil {
		return err
	}
	if !slices.Contains(versionStrings(versions), target) {
		return fmt.Errorf("插件 %s 没有装过版本 %s（已装：%s）：只能切回装过的版本，新版本用 lax plugin update %s --source <目录>",
			id, target, cellOrNone(versionStrings(versions)), id)
	}
	if target == p.ActiveVersion {
		return fmt.Errorf("插件 %s 的活动版本已经是 %s：回滚要指明另一个号，用 lax plugin show %s 看已装版本", id, target, id)
	}

	// 包要先读得动：读不动的版本即使切过去，网关也加载不进来。
	dir := pluginVersionDir(dataDir, id, target)
	pkg, err := plugin.LoadDir(dir)
	if err != nil {
		return fmt.Errorf("读 %s 失败：%v；这个版本的包目录已不在或已改坏，重新装一个同号版本需要先 lax plugin rm %s", dir, err, id)
	}
	if err := checkDialectConflicts(a.st, dataDir, pkg); err != nil {
		return err
	}
	_, catalogs := buildInstallPlan(pkg, p.Trust)
	before := activePermissionsJSON(a.st, id)

	if err := a.st.SetActivePluginVersion(id, target); err != nil {
		return err
	}
	for _, catalog := range catalogs {
		if err := a.st.ApplyPluginCatalog(id, catalog, pluginSourcePlugin); err != nil {
			// 目录事实没写回去：把活动版本指回原来的号，让库与包继续对得上。
			if restoreErr := a.st.SetActivePluginVersion(id, p.ActiveVersion); restoreErr != nil {
				return fmt.Errorf("登记厂商 %s 失败（%w），活动版本已切到 %s，改回 %s 也失败了（%v）：先用 lax plugin show %s 看清当前状态",
					catalog.Provider, err, target, p.ActiveVersion, restoreErr, id)
			}
			return fmt.Errorf("登记厂商 %s 失败，活动版本已改回 %s：%w\n清掉撞车的行（lax plugin rm <插件 ID>、lax provider rm <厂商>）后重试",
				catalog.Provider, p.ActiveVersion, err)
		}
	}

	// 审计：version 写切过去的那一个，权限变了再记一条。
	// 权限序列化失败时按空处理：审计那条主体记录照样要写，少一个权限原文总能补。
	after := ""
	if encoded, err := json.Marshal(pkg.Manifest.Permissions); err == nil {
		after = string(encoded)
	}
	auditPlugin(e, a.st, store.PluginAudit{
		PluginID: id, Version: target, Action: store.AuditRollback, Actor: pluginActorCLI,
		PermissionsBeforeJSON: before, PermissionsAfterJSON: after,
	})
	auditPluginPermission(e, a.st, id, target, pluginActorCLI, before, after)

	if e.JSON {
		return e.printJSON(map[string]any{
			"id": id, "from": p.ActiveVersion, "to": target,
			"package_dir": dir, "providers": len(catalogs),
		})
	}
	fmt.Fprintf(e.Stdout, "已回滚插件 %s：%s → %s\n", id, cellOrDash(p.ActiveVersion), target)
	fmt.Fprintf(e.Stdout, "包目录 %s，重写了 %d 家厂商的目录事实\n", dir, len(catalogs))
	fmt.Fprintf(e.Stdout, "下一步：lax plugin doctor %s 核对包哈希，lax plugin reload %s 让正在跑的网关换回去\n", id, id)
	return nil
}

// ---------------------------------------------------------------- ls

// pluginJSON 是 plugin ls 的一行。Dialects 是方言条数，与表格里的「方言数」同一口径。
type pluginJSON struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Kind         string   `json:"kind"`
	Runtime      string   `json:"runtime"`
	Trust        string   `json:"trust"`
	State        string   `json:"state"`
	Enabled      bool     `json:"enabled"`
	Providers    []string `json:"providers"`
	DialectCount int      `json:"dialect_count"`
	LastError    string   `json:"last_error"`
}

// describeInstalled 从磁盘上的活动版本取厂商与方言数：库里的 manifest 只是安装时的快照，
// 包被删掉或改动过时两者会不一致。读不动就退回快照：厂商还看得见，方言数只能记 0。
func describeInstalled(st *store.Store, dataDir string, p store.Plugin) ([]string, int) {
	pkg, err := installedPlugin(dataDir, p)
	if err == nil {
		return providerIDs(pkg.Manifest), len(pkg.Dialects)
	}
	if m, ok := storeManifest(st, p); ok {
		return providerIDs(m), 0
	}
	return nil, 0
}

// installedRuntime 取活动版本的运行时。版本行缺就返回空串：ls 只摆库里的事实，不猜。
func installedRuntime(st *store.Store, p store.Plugin) string {
	pv, ok, err := st.ActivePluginVersion(p.ID)
	if err != nil || !ok {
		return ""
	}
	return pv.Runtime
}

func cmdPluginLs(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin ls", "列出已装插件")
	onlyEnabled := fs.Bool("enabled", false, "只列启用的插件")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	plugins, err := a.st.ListPlugins()
	if err != nil {
		return err
	}
	rows := make([]pluginJSON, 0, len(plugins))
	for _, p := range plugins {
		if *onlyEnabled && !p.Enabled {
			continue
		}
		providers, dialects := describeInstalled(a.st, dataDir, p)
		rows = append(rows, pluginJSON{
			ID:           p.ID,
			Version:      p.ActiveVersion,
			Kind:         p.Kind,
			Runtime:      installedRuntime(a.st, p),
			Trust:        p.Trust,
			State:        p.State,
			Enabled:      p.Enabled,
			Providers:    providers,
			DialectCount: dialects,
			LastError:    p.LastError,
		})
	}

	if e.JSON {
		return e.printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "还没有安装插件。装一个本地目录：lax plugin install <目录>（目录里要有 lax.plugin.json）")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.ID, cellOrDash(r.Version), r.Kind, cellOrDash(r.Runtime), r.Trust, r.State,
			cellOrNone(r.Providers), fmt.Sprintf("%d", r.DialectCount),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"ID", "版本", "类型", "运行时", "信任", "状态", "厂商", "方言数"}, table))
	return nil
}

// cellOrDash 把一个可能为空的字段写成 -：表格里空单元格看不出是「没有」还是「漏了」。
func cellOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// ---------------------------------------------------------------- show

// pluginShowProviderJSON 是 show 里的一家厂商：路由、方言与模型都列出来。
type pluginShowProviderJSON struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	BaseURL  string   `json:"base_url"`
	Routes   []string `json:"routes"`
	Dialects []string `json:"dialects"`
	Models   []string `json:"models"`
}

// pluginShowJSON 是 plugin show 的 --json 形状。Manifest 是安装入库的原文。
// Audit 与 Logs 没有时给空数组而不是省略：调用方不用再判 null。
type pluginShowJSON struct {
	Manifest    json.RawMessage          `json:"manifest"`
	Permissions plugin.Permissions       `json:"permissions"`
	Versions    []string                 `json:"versions"`
	Providers   []pluginShowProviderJSON `json:"providers"`
	Dialects    []string                 `json:"dialects"`
	State       string                   `json:"state"`
	LastError   string                   `json:"last_error"`
	Conflicts   []string                 `json:"conflicts"`
	Audit       []pluginAuditJSON        `json:"audit"`
	Logs        []pluginLogJSON          `json:"logs"`
}

func cmdPluginShow(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin show", "显示一个插件的 manifest、权限、版本、厂商与方言")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("用法：lax plugin show <插件 ID>\n插件 ID 用 lax plugin ls 看")
	}
	id := strings.TrimSpace(fs.Arg(0))

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	p, ok, err := a.st.PluginByID(id)
	if err != nil {
		return err
	}
	if !ok {
		return unknownPluginError(a.st, id)
	}
	versions, err := a.st.PluginVersions(id)
	if err != nil {
		return err
	}

	out := pluginShowJSON{
		Versions:  versionStrings(versions),
		State:     p.State,
		LastError: p.LastError,
		Providers: []pluginShowProviderJSON{},
		Dialects:  []string{},
		Conflicts: []string{},
		Audit:     []pluginAuditJSON{},
		Logs:      []pluginLogJSON{},
	}
	// manifest 用库里的原文：它是安装当时的样子，装好之后手改目录不会改动它。
	out.Manifest = activeManifestJSON(a.st, p)
	if m, ok := storeManifest(a.st, p); ok {
		out.Permissions = m.Permissions
	} else if pkg, err := installedPlugin(dataDir, p); err == nil {
		out.Permissions = pkg.Manifest.Permissions
	}

	if pkg, err := installedPlugin(dataDir, p); err == nil {
		for _, d := range pkg.Dialects {
			out.Dialects = append(out.Dialects, d.ID)
		}
		for _, prov := range pkg.Manifest.Providers {
			item := pluginShowProviderJSON{ID: prov.ID, Name: prov.Name, BaseURL: prov.BaseURL}
			item.Dialects = append(item.Dialects, pkg.DialectsByProvider[prov.ID]...)
			for _, route := range dialectRoutes(pkg, prov.ID) {
				item.Routes = append(item.Routes, route.Protocol+" "+route.Path)
			}
			for _, model := range pkg.Catalogs[prov.ID].Models {
				item.Models = append(item.Models, model.Model)
			}
			out.Providers = append(out.Providers, item)
		}
	}
	out.Conflicts = append(out.Conflicts, pluginDialectConflicts(a.st, dataDir, id, out.Dialects)...)
	// 最近审计与最近日志给「刚刚报错」提供上下文：不必再另敲两条命令。
	// 两条查询都失败只当没数据：show 的主体是 manifest，不该被它拖垮。
	if audit, err := a.st.PluginAuditLog(id, pluginShowRecent); err == nil {
		out.Audit = auditRows(audit)
	}
	if logs, err := a.st.PluginLogs(id, pluginShowRecent, "", 0); err == nil {
		out.Logs = logRows(logs)
	}

	if e.JSON {
		return e.printJSON(out)
	}

	fmt.Fprintf(e.Stdout, "插件 %s %s（%s，%s）\n", p.ID, cellOrDash(p.ActiveVersion), cellOrDash(p.Kind), cellOrDash(p.Trust))
	fmt.Fprintf(e.Stdout, "信任 %s    状态 %s    启用 %s\n", cellOrDash(p.Trust), p.State, boolCell(p.Enabled))
	if p.ActiveVersion != "" {
		fmt.Fprintf(e.Stdout, "包目录 %s\n", pluginVersionDir(dataDir, p.ID, p.ActiveVersion))
	}
	fmt.Fprintf(e.Stdout, "权限 %s\n", permissionSummary(out.Permissions))
	fmt.Fprintf(e.Stdout, "已装版本 %s（活动 %s）\n", cellOrNone(out.Versions), cellOrDash(p.ActiveVersion))
	if p.LastError != "" {
		fmt.Fprintf(e.Stdout, "最近错误 %s\n", p.LastError)
	}
	for _, prov := range out.Providers {
		fmt.Fprintf(e.Stdout, "厂商 %s · %s · %s\n", prov.ID, prov.Name, prov.BaseURL)
		fmt.Fprintf(e.Stdout, "  路由 %s\n", cellOrNone(prov.Routes))
		fmt.Fprintf(e.Stdout, "  方言 %s\n", cellOrNone(prov.Dialects))
		fmt.Fprintf(e.Stdout, "  模型 %s\n", cellOrNone(prov.Models))
	}
	if len(out.Conflicts) == 0 {
		fmt.Fprintln(e.Stdout, "冲突 无")
	} else {
		for _, c := range out.Conflicts {
			fmt.Fprintf(e.Stdout, "冲突 %s\n", c)
		}
	}
	printRecentAudit(e, out.Audit)
	printRecentLogs(e, out.Logs)
	if len(out.Manifest) > 0 {
		fmt.Fprintln(e.Stdout, "manifest")
		fmt.Fprintln(e.Stdout, indentJSON(out.Manifest))
	}
	return nil
}

// activeManifestJSON 取活动版本入库的 manifest 原文；没有活动版本时退回最近装的那个版本。
func activeManifestJSON(st *store.Store, p store.Plugin) json.RawMessage {
	if pv, ok, err := st.ActivePluginVersion(p.ID); err == nil && ok && pv.ManifestJSON != "" {
		return json.RawMessage(pv.ManifestJSON)
	}
	versions, err := st.PluginVersions(p.ID)
	if err != nil || len(versions) == 0 || versions[len(versions)-1].ManifestJSON == "" {
		return nil
	}
	return json.RawMessage(versions[len(versions)-1].ManifestJSON)
}

// pluginDialectConflicts 找出与内置方言或别的已装插件撞车的方言 ID：一个 ID 只有一个实现，
// 网关按登记顺序装配，撞车的那一个整包进不去。
func pluginDialectConflicts(st *store.Store, dataDir, id string, dialects []string) []string {
	builtin := map[string]bool{}
	for _, d := range plugin.DefaultRegistry().IDs() {
		builtin[d] = true
	}
	owner := map[string]string{}
	plugins, err := st.ListPlugins()
	if err != nil {
		return nil
	}
	for _, p := range plugins {
		if p.ID == id {
			continue
		}
		pkg, err := installedPlugin(dataDir, p)
		if err != nil {
			continue
		}
		for _, d := range pkg.Dialects {
			owner[d.ID] = p.ID
		}
	}
	out := make([]string, 0, len(dialects))
	for _, d := range dialects {
		if builtin[d] {
			out = append(out, fmt.Sprintf("方言 %s 是内置方言，插件不能覆盖它", d))
		}
		if other, ok := owner[d]; ok {
			out = append(out, fmt.Sprintf("方言 %s 已经由插件 %s 登记：一个 ID 只能有一个实现", d, other))
		}
	}
	return out
}

// boolCell 把布尔写成中文，输出里不留 true/false 让人二次翻译。
func boolCell(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

// indentJSON 按两格缩进重排一段 JSON；不是合法 JSON 时原样返回，让读者看到原文。
func indentJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// ---------------------------------------------------------------- 审计与日志的展示

// pluginShowRecent 是 show 里审计与日志各取几条。
const pluginShowRecent = 5

// pluginAuditJSON 是审计的一行。权限前后原文留在库里，不在这里逐行重印：
// 它只对「那一次改了什么」有意义，混进列表会把动作本身淹掉。
type pluginAuditJSON struct {
	ID      int64  `json:"id"`
	Version string `json:"version"`
	Action  string `json:"action"`
	Actor   string `json:"actor"`
	Result  string `json:"result"`
	Error   string `json:"error"`
	At      int64  `json:"at"`
}

// pluginLogJSON 是插件日志的一行。
type pluginLogJSON struct {
	ID      int64  `json:"id"`
	Level   string `json:"level"`
	Hook    string `json:"hook"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

func auditRows(entries []store.PluginAudit) []pluginAuditJSON {
	out := make([]pluginAuditJSON, 0, len(entries))
	for _, a := range entries {
		out = append(out, pluginAuditJSON{
			ID: a.ID, Version: a.Version, Action: a.Action, Actor: a.Actor,
			Result: a.Result, Error: a.Error, At: a.At,
		})
	}
	return out
}

func logRows(entries []store.PluginLog) []pluginLogJSON {
	out := make([]pluginLogJSON, 0, len(entries))
	for _, l := range entries {
		out = append(out, pluginLogJSON{
			ID: l.ID, Level: l.Level, Hook: l.Hook, Message: l.Message, At: l.At,
		})
	}
	return out
}

func printRecentAudit(e *Env, rows []pluginAuditJSON) {
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "最近审计 无")
		return
	}
	fmt.Fprintf(e.Stdout, "最近审计（%d 条）\n", len(rows))
	for _, a := range rows {
		result := a.Result
		if result == "" {
			result = "ok"
		}
		line := fmt.Sprintf("  %s %s %s 结果 %s",
			time.Unix(a.At, 0).Format("01-02 15:04:05"), a.Action, cellOrDash(a.Version), result)
		if a.Actor != "" {
			line += " 发起 " + a.Actor
		}
		if a.Error != "" {
			line += " 错误 " + a.Error
		}
		fmt.Fprintln(e.Stdout, line)
	}
}

func printRecentLogs(e *Env, rows []pluginLogJSON) {
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "最近日志 无")
		return
	}
	fmt.Fprintf(e.Stdout, "最近日志（%d 条）\n", len(rows))
	for _, l := range rows {
		fmt.Fprintf(e.Stdout, "  %s %s %s\n",
			time.Unix(l.At, 0).Format("01-02 15:04:05"), pad(cellOrDash(l.Level), 6), l.Message)
	}
}

// ---------------------------------------------------------------- verify

// cmdPluginVerify 只做校验、不改库：对得上就把每一行印成 ok，对不上就是 fail 并退出码 1。
//
// 与 doctor 的分工：doctor 会写状态、会开沙箱调钩子，是「现在还能用吗」；
// verify 只看静态事实（哈希、方言声明、运行时），是「跟装的时候还一样吗」，可以随时跑。
func cmdPluginVerify(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin verify", "只做校验不改库：包哈希、manifest 哈希、方言与运行时")
	all := fs.Bool("all", false, "校验全部已装插件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("用法：lax plugin verify [<插件 ID>] [--all]\n不带 ID 或给 --all 时校验全部已装插件")
	}
	if *all && fs.NArg() == 1 {
		return usagef("--all 不能与插件 ID 一起给：要么校验全部，要么只校验一个")
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	plugins, err := a.st.ListPlugins()
	if err != nil {
		return err
	}
	targets := plugins
	if fs.NArg() == 1 {
		id := strings.TrimSpace(fs.Arg(0))
		p, ok, err := a.st.PluginByID(id)
		if err != nil {
			return err
		}
		if !ok {
			return unknownPluginError(a.st, id)
		}
		targets = []store.Plugin{p}
	}

	results := make([]pluginDoctorResult, 0, len(targets))
	failed := 0
	for _, p := range targets {
		result := verifyPlugin(a.st, dataDir, p)
		if !result.OK {
			failed++
		}
		results = append(results, result)
	}

	if e.JSON {
		if err := e.printJSON(results); err != nil {
			return err
		}
	} else if len(results) == 0 {
		fmt.Fprintln(e.Stdout, "还没有安装插件，没什么可校验的：lax plugin ls 看已装插件")
	} else {
		for _, r := range results {
			fmt.Fprintf(e.Stdout, "%s  %s\n", marks(r.OK), r.Name)
			for _, c := range r.Checks {
				fmt.Fprintf(e.Stdout, "  %s  %s %s\n", marks(c.OK), pad(c.Name, 12), c.Detail)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 个插件的校验未通过", failed)
	}
	return nil
}

// verifyPlugin 是一个插件的静态校验。它不写任何状态、不开沙箱，所以可以跟在任何地方跑。
func verifyPlugin(st *store.Store, dataDir string, p store.Plugin) pluginDoctorResult {
	result := pluginDoctorResult{Name: p.ID, OK: true}
	add := func(name string, ok bool, detail string) {
		result.Checks = append(result.Checks, pluginCheck{Name: name, OK: ok, Detail: detail})
		if !ok {
			result.OK = false
		}
	}

	if p.ActiveVersion == "" {
		add("活动版本", false, "没有活动版本：lax plugin show "+p.ID+" 看已装版本")
		return result
	}
	add("活动版本", true, p.ActiveVersion)

	pv, ok, err := st.ActivePluginVersion(p.ID)
	if err != nil {
		add("版本记录", false, err.Error()+"；先运行 lax doctor 检查数据库")
		return result
	}
	if !ok {
		add("版本记录", false, "库里没有这个活动版本的版本行，包对不上任何哈希：lax plugin update 装一个新版本")
		return result
	}

	root := pluginVersionDir(dataDir, p.ID, p.ActiveVersion)
	pkg, err := plugin.LoadDir(root)
	if err != nil {
		// manifest 的 schema 与 abi 在 LoadDir 里已经判过：这里报失败就说明包本身读不动了。
		add("manifest", false, err.Error())
		return result
	}
	add("manifest", true, fmt.Sprintf("schema %s，abi %s", pkg.Manifest.Schema, pkg.Manifest.ABI))

	// 方言登记：只做静态检查（ID 归属、形状、与内置方言撞车），不编方言也不开沙箱。
	if err := pkg.CheckDialects(plugin.DefaultRegistry()); err != nil {
		add("方言登记", false, err.Error())
	} else {
		ids := make([]string, 0, len(pkg.Dialects))
		for _, d := range pkg.Dialects {
			ids = append(ids, d.ID)
		}
		add("方言登记", true, cellOrNone(ids))
	}

	// 运行时取安装时记的那一列：网关启动时按它选加载路径，
	// 包里的 manifest 与它不一致时，这一包会被当成被改过的。
	if pv.Runtime == pkg.Manifest.Runtime {
		add("运行时", true, pv.Runtime)
	} else {
		add("运行时", false, fmt.Sprintf("安装时记的是 %s，manifest 写的是 %s", cellOrDash(pv.Runtime), cellOrDash(pkg.Manifest.Runtime)))
	}

	manifestSHA := plugin.ManifestHash(pkg.ManifestBytes)
	if manifestSHA == pv.ManifestSHA256 {
		add("manifest 哈希", true, shortHash(manifestSHA))
	} else {
		add("manifest 哈希", false, fmt.Sprintf("manifest 的哈希是 %s，安装时记的是 %s：manifest 被改过",
			shortHash(manifestSHA), shortHash(pv.ManifestSHA256)))
	}

	switch dirSHA, err := plugin.PluginDirHash(root); {
	case err != nil:
		add("包哈希", false, err.Error())
	case dirSHA != pv.PackageSHA256:
		add("包哈希", false, fmt.Sprintf("包的哈希是 %s，安装时记的是 %s：包内容被改过", shortHash(dirSHA), shortHash(pv.PackageSHA256)))
	default:
		add("包哈希", true, shortHash(dirSHA))
	}
	return result
}

// ---------------------------------------------------------------- logs

// pluginLogQuery 是 plugin logs 的三个筛选条件，--follow 每秒重查时原样带上。
type pluginLogQuery struct {
	Level string
	Since int64
	Limit int
}

// pluginFollowInterval 是 --follow 的轮询间隔，与 log tail --follow 一致。
const pluginFollowInterval = time.Second

func cmdPluginLogs(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin logs", "查看插件的运行日志")
	level := fs.String("level", "", "只显示某个级别：debug | info | warn | error")
	since := fs.String("since", "", "只看这个时间之后的：RFC3339（2025-01-02T15:04:05Z）或时长（1h、30m）")
	limit := fs.Int("limit", 50, "最多显示多少条")
	follow := fs.Bool("follow", false, "持续输出新日志，Ctrl-C 退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("用法：lax plugin logs <插件 ID> [--level <级别>] [--since <时间或时长>] [--limit N] [--follow]\n插件 ID 用 lax plugin ls 看")
	}
	id := strings.TrimSpace(fs.Arg(0))
	if *level != "" && !knownPluginLogLevel(*level) {
		return usagef("--level 是 %q：只能是 debug、info、warn、error", *level)
	}
	if *limit <= 0 {
		return usagef("--limit 是 %d：要一个正数", *limit)
	}
	sinceTS, err := parsePluginLogSince(*since, time.Now())
	if err != nil {
		return err
	}
	query := pluginLogQuery{Level: *level, Since: sinceTS, Limit: *limit}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if _, ok, err := a.st.PluginByID(id); err != nil {
		return err
	} else if !ok {
		return unknownPluginError(a.st, id)
	}

	// 跟随模式先装信号处理再打初始行：靠「已经看到输出」判定可以发信号的调用方（含测试）
	// 那时处理器一定已经就位，信号不会撞上默认动作。
	var ctx context.Context
	if *follow {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
	}

	rows, err := a.st.PluginLogs(id, query.Limit, query.Level, query.Since)
	if err != nil {
		return err
	}
	if err := printPluginLogs(e, rows); err != nil {
		return err
	}
	if !*follow {
		return nil
	}

	lastID := int64(0)
	if len(rows) > 0 {
		lastID = rows[0].ID
	}
	return followPluginLogs(ctx, e, a.st, id, query, lastID)
}

// parsePluginLogSince 把 --since 换成起始时间戳。空串表示不过滤。
func parsePluginLogSince(text string, now time.Time) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	if ts, err := time.Parse(time.RFC3339, text); err == nil {
		return ts.Unix(), nil
	}
	if d, err := time.ParseDuration(text); err == nil && d > 0 {
		return now.Add(-d).Unix(), nil
	}
	return 0, usagef("--since 是 %q：写成 RFC3339 时间（2025-01-02T15:04:05Z）或时长（1h、30m、90s）", text)
}

// pluginFollowFetch 是一次轮询取多少条：比 --limit 宽得多，
// 免得两次轮询之间的突发只留下最后一个窗口，中间那几行谁也没看见。
const pluginFollowFetch = 200

// followPluginLogs 每秒按 id 递增取新行，直到 ctx 结束。
func followPluginLogs(ctx context.Context, e *Env, st *store.Store, id string, query pluginLogQuery, lastID int64) error {
	fetch := max(query.Limit, pluginFollowFetch)
	ticker := time.NewTicker(pluginFollowInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			fresh, err := st.PluginLogs(id, fetch, query.Level, query.Since)
			if err != nil {
				return err
			}
			// 查询按时间倒序：倒着扫，按写入顺序打出来。
			newOnes := make([]store.PluginLog, 0, len(fresh))
			for i := len(fresh) - 1; i >= 0; i-- {
				if fresh[i].ID > lastID {
					newOnes = append(newOnes, fresh[i])
				}
			}
			if len(newOnes) == 0 {
				continue
			}
			if err := printPluginLogs(e, newOnes); err != nil {
				return err
			}
			lastID = newOnes[len(newOnes)-1].ID
		}
	}
}

func printPluginLogs(e *Env, rows []store.PluginLog) error {
	if e.JSON {
		return e.printJSON(logRows(rows))
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "没有符合条件的插件日志")
		return nil
	}
	// 库按时间倒序给：人看日志习惯先看最新的，所以不再翻回正序。
	table := make([][]string, 0, len(rows))
	for _, l := range rows {
		table = append(table, []string{
			time.Unix(l.At, 0).Format("01-02 15:04:05"),
			cellOrDash(l.Level),
			truncate(l.Message, 100),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"时间", "级别", "消息"}, table))
	return nil
}

// ---------------------------------------------------------------- reload

// cmdPluginReload 给正在运行的 serve 发一个 SIGHUP，让它重读插件表并整表换掉方言注册表。
//
// 不重启进程：在途请求不断，换表后新请求按新版本走。pid 从数据目录里的 lax.pid 读，
// 读不到或进程不在时直接报错，不猜。
func cmdPluginReload(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin reload", "让正在运行的 lax serve 重读插件表")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("用法：lax plugin reload [<插件 ID>]\nSIGHUP 是整表重载，带 ID 只用来确认那一个插件确实已装")
	}
	only := ""
	if fs.NArg() == 1 {
		only = strings.TrimSpace(fs.Arg(0))
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	if only != "" {
		// 只做存在性校对：SIGHUP 是整表重载，带不了「只换这一个」的信息。
		if _, ok, err := a.st.PluginByID(only); err != nil {
			return err
		} else if !ok {
			return unknownPluginError(a.st, only)
		}
	}

	// 名单在发信号之前取：它说的是「这次重载会碰到谁」，不是重载之后的状态。
	reloaded := reloadedPluginIDs(a.st, only)
	pidPath := servePidPath(dataDir)
	pid, err := readServePid(pidPath)
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("pid %d 的进程不存在：lax serve 没在跑，%s 是上次留下的；删掉它再启动 lax serve", pid, pidPath)
		}
		return fmt.Errorf("给 lax serve（pid %d）发 SIGHUP 失败：%v", pid, err)
	}

	auditPluginReload(e, a.st, pluginActorCLI, only)

	if e.JSON {
		return e.printJSON(map[string]any{"pid": pid, "plugins": reloaded})
	}
	target := "全部已启用的插件"
	if only != "" {
		target = only + "（实际是整表重载）"
	}
	fmt.Fprintf(e.Stdout, "已让 lax serve（pid %d）重读插件表：%s\n", pid, target)
	fmt.Fprintln(e.Stdout, "下一步：lax plugin logs <插件 ID> 看新版本有没有报错")
	return nil
}

// reloadedPluginIDs 列出这次重载会碰到的插件 ID，给 --json 的输出用。
func reloadedPluginIDs(st *store.Store, only string) []string {
	plugins, err := st.ListPlugins()
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if !p.Enabled || p.ActiveVersion == "" || (only != "" && p.ID != only) {
			continue
		}
		out = append(out, p.ID)
	}
	return out
}

// ---------------------------------------------------------------- enable / disable

func cmdPluginToggle(e *Env, args []string, enable bool) error {
	name, summary := "plugin disable", "停用一个插件，网关重启后不再加载它的方言"
	if enable {
		name, summary = "plugin enable", "启用一个插件，网关重启后加载它的方言"
	}
	fs := newFlagSet(e, name, summary)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("用法：lax %s <插件 ID>\n插件 ID 用 lax plugin ls 看", name)
	}
	id := strings.TrimSpace(fs.Arg(0))

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	p, ok, err := a.st.PluginByID(id)
	if err != nil {
		return err
	}
	if !ok {
		return unknownPluginError(a.st, id)
	}

	state, lastError := pluginStateOK, ""
	if !enable {
		// 停用不改最近一次错误：那条错误说明了它上次为什么被挡下来。
		state, lastError = pluginStateDisabled, p.LastError
	} else if p.State == pluginStateTampered {
		// 启用不洗掉 tampered：包被改过这件事只有人看过 doctor 的结论才能翻回去。
		state, lastError = pluginStateTampered, p.LastError
	}
	if err := a.st.SetPluginEnabled(id, enable); err != nil {
		return err
	}
	if err := a.st.SetPluginState(id, state, lastError); err != nil {
		return err
	}

	action := store.AuditDisable
	if enable {
		action = store.AuditEnable
	}
	auditPlugin(e, a.st, store.PluginAudit{
		PluginID: id, Version: p.ActiveVersion, Action: action, Actor: pluginActorCLI,
	})

	if e.JSON {
		return e.printJSON(map[string]any{
			"id": id, "version": p.ActiveVersion, "enabled": enable,
			"state": state, "last_error": lastError,
		})
	}
	if enable {
		fmt.Fprintf(e.Stdout, "已启用插件 %s %s（状态 %s）\n", id, cellOrDash(p.ActiveVersion), state)
		if state == pluginStateTampered {
			fmt.Fprintf(e.Stdout, "包内容与安装时记的哈希不一致，网关仍然不会加载它：先跑 lax plugin doctor %s 看哪一项没过\n", id)
			return nil
		}
		fmt.Fprintf(e.Stdout, "下一步：重启 lax serve 让它加载这个插件的方言\n")
		return nil
	}
	fmt.Fprintf(e.Stdout, "已停用插件 %s %s（状态 %s）\n", id, cellOrDash(p.ActiveVersion), state)
	fmt.Fprintln(e.Stdout, "下一步：重启 lax serve 后不再加载它的方言；绑定它方言的模型会报「方言没有注册」，用 lax plugin ls 核对")
	return nil
}

// ---------------------------------------------------------------- rm

func cmdPluginRm(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin rm", "卸载插件，默认保留包目录")
	purge := fs.Bool("purge", false, "连同 installed/<插件 ID> 下的包一起删除")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("用法：lax plugin rm <插件 ID> [--purge]\n插件 ID 用 lax plugin ls 看")
	}
	id := strings.TrimSpace(fs.Arg(0))

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	p, ok, err := a.st.PluginByID(id)
	if err != nil {
		return err
	}
	if !ok {
		return unknownPluginError(a.st, id)
	}
	versions, err := a.st.PluginVersions(id)
	if err != nil {
		return err
	}
	bindings, err := countPluginDialectBindings(a.st, id)
	if err != nil {
		return err
	}
	providers, _ := describeInstalled(a.st, dataDir, p)

	// 审计在删除之前取权限原文、删除之后写：权限属于那个已经不在的版本，
	// 删完之后再查活动版本就查不到了。
	removedPermissions := activePermissionsJSON(a.st, id)
	if err := a.st.DeletePlugin(id); err != nil {
		return err
	}
	auditPlugin(e, a.st, store.PluginAudit{
		PluginID: id, Version: p.ActiveVersion, Action: store.AuditRemove, Actor: pluginActorCLI,
		PermissionsBeforeJSON: removedPermissions,
	})
	packageDir := filepath.Join(pluginInstalledRoot(dataDir), id)
	if *purge {
		if err := os.RemoveAll(packageDir); err != nil {
			return fmt.Errorf("删包目录 %s 失败：%v；库里的记录已经删了，剩下的目录可以手工 rm -rf", packageDir, err)
		}
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"removed":          id,
			"versions":         len(versions),
			"dialect_bindings": bindings,
			"providers":        providers,
			"purged":           *purge,
			"package_dir":      packageDir,
		})
	}
	fmt.Fprintf(e.Stdout, "已卸载插件 %s：清掉 %d 个版本、%d 条方言绑定、%d 家它登记的厂商\n",
		id, len(versions), bindings, len(providers))
	if *purge {
		fmt.Fprintf(e.Stdout, "包目录已删除 %s\n", packageDir)
	} else {
		fmt.Fprintf(e.Stdout, "包目录保留在 %s：要一起删就加 --purge\n", packageDir)
	}
	fmt.Fprintln(e.Stdout, "下一步：lax plugin ls 核对；重启 lax serve 后网关不再加载它的方言")
	return nil
}

// countPluginDialectBindings 数这个插件带来的方言绑定。归属按方言 ID 的 <插件 ID>/ 前缀判，
// 与 store 删除时的判法一致。
func countPluginDialectBindings(st *store.Store, id string) (int, error) {
	ops, err := st.ListModelOperations("", "")
	if err != nil {
		return 0, err
	}
	prefix := id + "/"
	count := 0
	for _, op := range ops {
		if strings.HasPrefix(op.DialectID, prefix) {
			count++
		}
	}
	return count, nil
}

// ---------------------------------------------------------------- doctor

// pluginCheck 是一项检查。Detail 在失败时就是一句话建议。
type pluginCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// pluginDoctorResult 是一个插件的检查结果。
type pluginDoctorResult struct {
	Name   string        `json:"name"`
	OK     bool          `json:"ok"`
	Checks []pluginCheck `json:"checks"`
}

func cmdPluginDoctor(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin doctor", "检查已装插件：包完整性、manifest 哈希、方言登记与状态")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("用法：lax plugin doctor [<插件 ID>]\n不带 ID 时检查全部启用的插件")
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	dataDir := filepath.Dir(a.paths.DB)

	plugins, err := a.st.ListPlugins()
	if err != nil {
		return err
	}
	targets := make([]store.Plugin, 0, len(plugins))
	if fs.NArg() == 1 {
		id := strings.TrimSpace(fs.Arg(0))
		p, ok, err := a.st.PluginByID(id)
		if err != nil {
			return err
		}
		if !ok {
			return unknownPluginError(a.st, id)
		}
		targets = append(targets, p)
	} else {
		for _, p := range plugins {
			if p.Enabled {
				targets = append(targets, p)
			}
		}
	}

	// 注册表按 ID 顺序累积：后面的插件跟前面装好的撞名时，这里就会露出来。
	reg := plugin.DefaultRegistry()
	results := make([]pluginDoctorResult, 0, len(targets))
	// 沙箱在全部插件查完后再关：注册表里的 hook 方言还指着它们。
	var closers []func()
	failed := 0
	for _, p := range targets {
		result := checkPlugin(a.st, dataDir, reg, p, &closers)
		if !result.OK {
			failed++
		}
		results = append(results, result)
	}
	for _, closeSandbox := range closers {
		closeSandbox()
	}

	if e.JSON {
		if err := e.printJSON(results); err != nil {
			return err
		}
	} else if len(results) == 0 {
		fmt.Fprintln(e.Stdout, "没有启用的插件，没什么可检查的：lax plugin ls 看已装插件")
	} else {
		for _, r := range results {
			fmt.Fprintf(e.Stdout, "%s  %s\n", marks(r.OK), r.Name)
			for _, c := range r.Checks {
				fmt.Fprintf(e.Stdout, "  %s  %s %s\n", marks(c.OK), pad(c.Name, 12), c.Detail)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 个插件的检查未通过", failed)
	}
	return nil
}

// marks 把检查结果写成 ok / fail，与 lax doctor 同一套标记。
func marks(ok bool) string {
	if ok {
		return "ok  "
	}
	return "fail"
}

// checkPlugin 检查一个插件，并把「包被改过」写回库里：状态由宿主写，插件自己改不了。
// closers 收下这次打开的沙箱，由调用方在全部检查结束后关。
func checkPlugin(st *store.Store, dataDir string, reg *plugin.Registry, p store.Plugin, closers *[]func()) pluginDoctorResult {
	result := pluginDoctorResult{Name: p.ID, OK: true}
	add := func(name string, ok bool, detail string) {
		result.Checks = append(result.Checks, pluginCheck{Name: name, OK: ok, Detail: detail})
		if !ok {
			result.OK = false
		}
	}

	if p.ActiveVersion == "" {
		add("活动版本", false, "没有活动版本：lax plugin show "+p.ID+" 看已装版本，再装一个版本")
		return result
	}
	add("活动版本", true, p.ActiveVersion)

	pv, hasVersion, err := st.ActivePluginVersion(p.ID)
	if err != nil {
		add("版本记录", false, err.Error()+"；先运行 lax doctor 检查数据库")
		return result
	}
	if !hasVersion {
		add("版本记录", false, "库里没有这个活动版本的版本行，包对不上任何哈希；重装一个版本：lax plugin install <目录>")
		return result
	}

	root := pluginVersionDir(dataDir, p.ID, p.ActiveVersion)
	pkg, err := plugin.LoadDir(root)
	if err != nil {
		add("包目录", false, err.Error()+"；用 lax plugin install <目录> 装一个干净的版本（同版本号不能覆盖，先把 manifest 的 version 换成新号）")
		return result
	}
	add("包目录", true, root)

	// runtime 取安装时记下的那个：网关启动时也按它选加载路径，doctor 要和网关看同一列。
	moejs := pv.Runtime == plugin.RuntimeMoejs
	sandboxOpts := plugin.SandboxOptions{
		Manifest: pkg.Manifest,
		Root:     root,
		Entry:    pkg.Manifest.Entry,
		Trust:    p.Trust,
	}

	tampered := ""
	manifestSHA := plugin.ManifestHash(pkg.ManifestBytes)
	if manifestSHA != pv.ManifestSHA256 {
		tampered = fmt.Sprintf("manifest 的哈希是 %s，安装时记的是 %s", shortHash(manifestSHA), shortHash(pv.ManifestSHA256))
		add("manifest 哈希", false, tampered+"：manifest 被改过，插件不会被网关加载")
	} else {
		add("manifest 哈希", true, shortHash(manifestSHA))
	}

	dirSHA, err := plugin.PluginDirHash(root)
	switch {
	case err != nil:
		add("包哈希", false, err.Error()+"；包目录读不动时不加载它，重新装一个干净的版本")
	case dirSHA != pv.PackageSHA256:
		if tampered == "" {
			tampered = fmt.Sprintf("包的哈希是 %s，安装时记的是 %s", shortHash(dirSHA), shortHash(pv.PackageSHA256))
		}
		add("包哈希", false, fmt.Sprintf("包的哈希是 %s，安装时记的是 %s：包被改过，插件不会被网关加载", shortHash(dirSHA), shortHash(pv.PackageSHA256)))
	default:
		add("包哈希", true, shortHash(dirSHA))
	}

	// 带代码的插件多两项：沙箱能不能打开（入口语法错、缺文件都在这里露出来）、
	// 入口里有没有导出宿主会调的钩子。两项都不碰网络，也不调钩子。
	if moejs {
		if err := pkg.OpenSandbox(sandboxOpts); err != nil {
			add("沙箱", false, err.Error()+"；改好入口文件后重装一个新版本（同版本号不能覆盖）")
		} else {
			*closers = append(*closers, func() { _ = pkg.Close() })
			add("沙箱", true, fmt.Sprintf("%s（钩子超时 %s/%s）", pkg.Manifest.Entry,
				plugin.DefaultPrepareTimeout, plugin.DefaultDecodeTimeout))
			hooks, err := pkg.ExportedHooks()
			switch {
			case err != nil:
				add("导出钩子", false, err.Error())
			case !slices.Contains(hooks, plugin.HookPrepareRequest) && !slices.Contains(hooks, plugin.HookDecodeResponse):
				add("导出钩子", false, fmt.Sprintf("入口里没有导出 %s 或 %s：至少要有一个，%s 可选",
					plugin.HookPrepareRequest, plugin.HookDecodeResponse, plugin.HookDecodeStream))
			default:
				add("导出钩子", true, strings.Join(hooks, "、"))
			}
		}
	}

	// 方言登记：moejs 走沙箱路径（沙箱已在上一步打开，不会再开），声明式走旧口径。
	var regErr error
	if moejs {
		regErr = pkg.Register(reg, sandboxOpts)
	} else {
		regErr = reg.RegisterPlugin(pkg)
	}
	if regErr != nil {
		add("方言登记", false, regErr.Error()+"；卸载撞车的那一个（lax plugin rm <插件 ID>），或改插件 dialects.json 里的方言 ID")
	} else {
		ids := make([]string, 0, len(pkg.Dialects))
		for _, d := range pkg.Dialects {
			ids = append(ids, d.ID)
		}
		add("方言登记", true, cellOrNone(ids))
	}

	switch {
	case tampered != "":
		if err := st.SetPluginState(p.ID, pluginStateTampered, tampered); err != nil {
			add("状态", false, "写状态失败："+err.Error())
			break
		}
		add("状态", false, "状态 tampered："+tampered+"；用 lax plugin install 装一个没改过的版本，网关不会加载 tampered 的插件")
	case p.State == pluginStateTampered:
		if err := st.SetPluginState(p.ID, pluginStateOK, ""); err != nil {
			add("状态", false, "写状态失败："+err.Error())
			break
		}
		add("状态", true, "哈希已经对上，状态从 tampered 恢复成 ok")
	case p.State == pluginStateFailed && result.OK:
		// 上次启动加载失败的那一行，在本次全部检查通过后翻回 ok，否则它会永远是失败的。
		if err := st.SetPluginState(p.ID, pluginStateOK, ""); err != nil {
			add("状态", false, "写状态失败："+err.Error())
			break
		}
		add("状态", true, "上次加载失败的原因已经消失，状态从 failed 恢复成 ok")
	default:
		add("状态", true, p.State)
	}
	return result
}

// shortHash 取哈希前 12 位：核对的输出里没人逐个字符比对全部 64 位。
func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12] + "…"
}
