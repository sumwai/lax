package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
)

// providerSubs 是 provider 一组的子命令清单，组级帮助与错误提示都读它。
var providerSubs = []groupItem{
	{"ls", "列厂商、地址、默认协议、各协议路由与模型列表路径"},
	{"add", "新建一家厂商与它的协议路由"},
	{"set", "改一家厂商的地址、路由、默认协议或备注"},
	{"unset", "删掉一条路由，或清掉模型列表覆盖、默认协议、备注"},
	{"rm", "删除厂商与它的全部路由，账号还在用时要求 --force"},
}

// 三个协议 flag 与它们的正确写法。路径都是相对厂商 base_url 的完整路径，必须以 / 开头；
// 没有 --gemini：gemini 协议尚未实现，留一条永远用不上的路由只会误导（见 DESIGN 5.8）。
const (
	anthropicPathExample = "--anthropic /v1/messages"
	openaiPathExample    = "--openai /v1/chat/completions"
	responsesPathExample = "--responses /v1/responses"
)

// 三份用法行，所有用法错误都把它打出来，不让用户看着一句「参数错误」去猜参数（见 AGENTS.md）。
const (
	providerAddUsage   = "用法：switchboard provider add <名称> --base-url <地址> [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--default-protocol <协议>] [--models-path <路径>] [--note <备注>]"
	providerSetUsage   = "用法：switchboard provider set <名称> [--base-url <地址>] [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--models-path <路径>] [--default-protocol <协议>] [--note <备注>]"
	providerUnsetUsage = "用法：switchboard provider unset <名称> <字段> [<字段>...]\n可用字段：anthropic、openai、responses、models-path、default-protocol、note"
)

// providerUnsetField 是一个可以被 `provider unset` 清掉的字段。字段名用 flag 名，
// 删路由的几项带上线协议；base-url 与厂商名不在这里：前者是必填地址，后者是主键（见 DESIGN 4）。
type providerUnsetField struct {
	name     string // 位置参数上的字段名
	what     string // 人读输出里的说法
	protocol string // 删掉路由时的线协议；空表示不是路由
}

var providerUnsetFields = []providerUnsetField{
	{"anthropic", "anthropic 路由", gateway.ProtocolAnthropic},
	{"openai", "openai 路由", gateway.ProtocolChat},
	{"responses", "openai-responses 路由", gateway.ProtocolResponses},
	{"models-path", "模型列表覆盖", ""},
	{"default-protocol", "显式默认协议", ""},
	{"note", "备注", ""},
}

// cmdProvider 是 provider 这一组子命令的入口。
func cmdProvider(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "provider", "厂商与路由", providerSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：ls | add | set | unset | rm\n用 switchboard provider --help 查看子命令")
	}
	switch args[0] {
	case "ls":
		return cmdProviderLs(e, args[1:])
	case "add":
		return cmdProviderAdd(e, args[1:])
	case "set":
		return cmdProviderSet(e, args[1:])
	case "unset":
		return cmdProviderUnset(e, args[1:])
	case "rm":
		return cmdProviderRm(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | add | set | unset | rm\n用 switchboard provider --help 查看子命令", args[0])
	}
}

// providerRouteJSON 是 provider ls --json 里的一条路由。
type providerRouteJSON struct {
	Protocol   string `json:"protocol"`
	Path       string `json:"path"`
	ModelsPath string `json:"models_path"`
}

// providerJSON 是 provider ls / add / set / unset --json 的一行。protocols 恒为数组，没有路由时是 []。
// DefaultProtocol 是有效默认协议（见 DESIGN 5.8），一条路由都没有时为空串。
// ModelsPath 是厂商级的模型列表路径覆盖，空串表示按各路由自己的 models_path 取。
type providerJSON struct {
	Name            string              `json:"name"`
	BaseURL         string              `json:"base_url"`
	DefaultProtocol string              `json:"default_protocol"`
	ModelsPath      string              `json:"models_path"`
	Protocols       []providerRouteJSON `json:"protocols"`
	Note            string              `json:"note"`
}

func routeJSONs(routes []store.ProviderRoute) []providerRouteJSON {
	out := make([]providerRouteJSON, 0, len(routes))
	for _, r := range routes {
		out = append(out, providerRouteJSON{Protocol: r.Protocol, Path: r.Path, ModelsPath: r.ModelsPath})
	}
	return out
}

// routeCell 把一家厂商的路由压进一格：协议名加路径，声明了模型列表路径的跟在括号里。
func routeCell(routes []store.ProviderRoute) string {
	if len(routes) == 0 {
		return "无路由（按 base_url 后缀猜路径）"
	}
	parts := make([]string, 0, len(routes))
	for _, r := range routes {
		cell := r.Protocol + " " + r.Path
		if r.ModelsPath != "" {
			cell += "（列表 " + r.ModelsPath + "）"
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, "、")
}

// providerModelsCell 是 provider ls 的「模型列表」列：厂商级覆盖非空就显示它，否则显示各路由
// 自己的 models_path，都没有时显示 -（这种情况 model sync 会按 base_url 后缀猜，见 DESIGN 5.7）。
func providerModelsCell(p store.Provider, routes []store.ProviderRoute) string {
	if p.ModelsPath != "" {
		return p.ModelsPath
	}
	// 多条路由共用一个列表路径时只显示一次（openai 的 chat 与 responses 都是 /v1/models）。
	paths := make([]string, 0, len(routes))
	seen := map[string]bool{}
	for _, r := range routes {
		if r.ModelsPath != "" && !seen[r.ModelsPath] {
			seen[r.ModelsPath] = true
			paths = append(paths, r.ModelsPath)
		}
	}
	if len(paths) == 0 {
		return "-"
	}
	return strings.Join(paths, "、")
}

// effectiveDefaultProtocol 问 store 要厂商的有效默认协议，cli 不自己算排序兜底（见 DESIGN 5.8）。
// 厂商在但一条路由都没有时 store 返回 ErrProviderNoRoutes：这里换成空串，
// 让调用方显示「无路由」而不是让整条命令失败。
func effectiveDefaultProtocol(st *store.Store, name string) (string, error) {
	proto, err := st.EffectiveProviderProtocol(name)
	if errors.Is(err, store.ErrProviderNoRoutes) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return proto, nil
}

// providerSnapshot 是一家厂商的库状态：row 是原始行（unset 判断某项在不在要看它），
// json 是 --json 与确认输出共用的形状，routes 给表格与计数用。
type providerSnapshot struct {
	row    store.Provider
	json   providerJSON
	routes []store.ProviderRoute
}

// readProvider 读一家厂商的库状态。ls、add、set、unset 四个子命令都走这里，
// 输出字段因此不会各写一遍。
func readProvider(st *store.Store, name string) (providerSnapshot, error) {
	p, err := st.GetProvider(name)
	if err != nil {
		return providerSnapshot{}, err
	}
	routes, err := st.ProviderRoutes(name)
	if err != nil {
		return providerSnapshot{}, err
	}
	def, err := effectiveDefaultProtocol(st, name)
	if err != nil {
		return providerSnapshot{}, err
	}
	return providerSnapshot{
		row: p,
		json: providerJSON{
			Name:            p.Name,
			BaseURL:         p.BaseURL,
			DefaultProtocol: def,
			ModelsPath:      p.ModelsPath,
			Protocols:       routeJSONs(routes),
			Note:            p.Note,
		},
		routes: routes,
	}, nil
}

// defaultProtocolCell 是默认协议列：一条路由都没有时显示 -，不算错误。
func defaultProtocolCell(s providerSnapshot) string {
	if s.json.DefaultProtocol == "" {
		return "-"
	}
	return s.json.DefaultProtocol
}

func cmdProviderLs(e *Env, args []string) error {
	fs := newFlagSet(e, "provider ls", "列出厂商、路由与模型列表路径")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	providers, err := a.st.ListProviders()
	if err != nil {
		return err
	}

	snaps := make([]providerSnapshot, len(providers))
	for i, p := range providers {
		if snaps[i], err = readProvider(a.st, p.Name); err != nil {
			return err
		}
	}

	if e.JSON {
		out := make([]providerJSON, 0, len(snaps))
		for _, s := range snaps {
			out = append(out, s.json)
		}
		return e.printJSON(out)
	}

	if len(snaps) == 0 {
		fmt.Fprintln(e.Stdout, "厂商表是空的，账号按 base_url 后缀猜路径。加一个厂商：switchboard provider add deepseek --base-url https://api.deepseek.com --openai /v1/chat/completions --anthropic /anthropic/v1/messages")
		return nil
	}

	rows := make([][]string, 0, len(snaps))
	for _, s := range snaps {
		rows = append(rows, []string{
			s.row.Name, s.json.BaseURL, defaultProtocolCell(s),
			routeCell(s.routes), providerModelsCell(s.row, s.routes), s.json.Note,
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "地址", "默认协议", "路由与路径", "模型列表", "备注"}, rows))
	return nil
}

// providerPathFlags 是三个协议 flag 的当前值。add 用它们声明路由，set 用它们覆盖一条路由的路径。
type providerPathFlags struct {
	anthropic *string
	openai    *string
	responses *string
}

func newProviderPathFlags(fs *flagSet) providerPathFlags {
	return providerPathFlags{
		anthropic: fs.String("anthropic", "", "anthropic 协议的路由路径，相对 --base-url 的完整路径，必须以 / 开头，例如 /v1/messages"),
		openai:    fs.String("openai", "", "openai-chat 协议的路由路径，相对 --base-url 的完整路径，必须以 / 开头，例如 /v1/chat/completions"),
		responses: fs.String("responses", "", "openai-responses 协议的路由路径，相对 --base-url 的完整路径，必须以 / 开头，例如 /v1/responses"),
	}
}

// providerPathFlag 是一个协议 flag 本次的取值与它是否出现在命令行上。
type providerPathFlag struct {
	flagName string
	protocol string
	example  string
	value    string
	given    bool
}

// pairs 按 anthropic、openai、responses 的固定顺序列出三个 flag，报错顺序因此稳定。
func (p providerPathFlags) pairs(fs *flagSet) []providerPathFlag {
	return []providerPathFlag{
		{"anthropic", gateway.ProtocolAnthropic, anthropicPathExample, *p.anthropic, flagWasSet(fs, "anthropic")},
		{"openai", gateway.ProtocolChat, openaiPathExample, *p.openai, flagWasSet(fs, "openai")},
		{"responses", gateway.ProtocolResponses, responsesPathExample, *p.responses, flagWasSet(fs, "responses")},
	}
}

// routePath 校验一条路由路径：它是相对厂商 base_url 的完整路径，必须以 / 开头。
// 删除路由走 `provider unset`，空值在前面已经报错了，这里不接受空串（见 DESIGN 4）。
func routePath(f providerPathFlag) (string, error) {
	path := strings.TrimSpace(f.value)
	if !strings.HasPrefix(path, "/") {
		return "", usagef("--%s %q 要以 / 开头：它是相对 --base-url 的完整路径，例如 %s（base_url 是 https://api.example.com 时最终打到 https://api.example.com/v1/...）",
			f.flagName, f.value, f.example)
	}
	return path, nil
}

// protocolFromCLI 把 --default-protocol 的取值换成线协议。命令行写法是 anthropic、openai、responses，
// 线协议全名（anthropic、openai-chat、openai-responses）也认：输入 openai 就是 chat。
func protocolFromCLI(value string) (string, error) {
	switch v := strings.TrimSpace(value); v {
	case "":
		return "", nil
	case "openai", gateway.ProtocolChat:
		return gateway.ProtocolChat, nil
	case "responses", gateway.ProtocolResponses:
		return gateway.ProtocolResponses, nil
	case gateway.ProtocolAnthropic:
		return gateway.ProtocolAnthropic, nil
	}
	return "", usagef("--default-protocol %q 不认识，可用 anthropic、openai、responses（也认线协议全名 openai-chat、openai-responses），例如 switchboard provider add acme --base-url https://api.example.com --openai /v1/chat/completions --default-protocol openai", value)
}

// cliProtocolName 把线协议换成命令行写法：错误文案里让人抄的是 `--default-protocol openai`
// 这种形式，而不是 `openai-chat`（见 DESIGN 5.8）。
func cliProtocolName(protocol string) string {
	switch protocol {
	case gateway.ProtocolChat:
		return "openai"
	case gateway.ProtocolResponses:
		return "responses"
	}
	return protocol
}

// modelsPathFlag 校验厂商级的模型列表路径覆盖。空值由调用方先报成用法错误：
// 清掉覆盖要用 `provider unset <名称> models-path`（见 DESIGN 4）。
func modelsPathFlag(value string) (string, error) {
	path := strings.TrimSpace(value)
	if !strings.HasPrefix(path, "/") {
		return "", usagef("--models-path %q 要以 / 开头：它是相对 --base-url 的完整路径，例如 --models-path /v1/models", value)
	}
	return path, nil
}

// emptyValueError 是「给了参数但值是空」的统一文案。空串不再表示删除或清空：
// 两者一律走 `provider unset`，避免 --note \"\" 这类写法顺手把已有值抹掉（见 DESIGN 4）。
func emptyValueError(setCmd bool, flagName string) error {
	if setCmd {
		return usagef("--%s 传了空值：provider set 只负责设值，删除一条路由或清掉一项设置要用 switchboard provider unset <名称> %s",
			flagName, flagName)
	}
	return usagef("--%s 传了空值：新建厂商不接受空值，去掉这个参数再试；建好之后要删掉就用 switchboard provider unset <名称> %s",
		flagName, flagName)
}

// lookupUnsetField 把一个位置参数换成字段描述。未知字段、带 -- 前缀、大小写不符都当场报错
// 并列出可用字段：静默忽略会让「以为删了」留到下游转发才发现（见 DESIGN 4）。
func lookupUnsetField(raw string) (providerUnsetField, error) {
	available := make([]string, 0, len(providerUnsetFields))
	for _, f := range providerUnsetFields {
		available = append(available, f.name)
	}
	fields := strings.Join(available, "、")
	name := strings.TrimSpace(raw)
	if name == "" {
		return providerUnsetField{}, usagef("%s", providerUnsetUsage)
	}
	if strings.HasPrefix(name, "-") {
		return providerUnsetField{}, usagef("字段名 %q 不要带 -- 前缀，直接写名字，例如 switchboard provider unset <名称> openai；可用：%s", raw, fields)
	}
	switch name {
	case "base-url":
		return providerUnsetField{}, usagef("base-url 不能 unset：它是厂商的必填地址，清掉就无处转发；改地址用 switchboard provider set <名称> --base-url <地址>")
	case "name", "名称", "厂商名":
		return providerUnsetField{}, usagef("厂商名不能 unset：它是主键，清不掉；不再需要这家厂商就用 switchboard provider rm <名称>")
	}
	for _, f := range providerUnsetFields {
		if name == f.name {
			return f, nil
		}
	}
	for _, f := range providerUnsetFields {
		if strings.EqualFold(name, f.name) {
			return providerUnsetField{}, usagef("字段名 %q 大小写不符，写 %s；可用：%s", raw, f.name, fields)
		}
	}
	return providerUnsetField{}, usagef("字段名 %q 不认识，可用：%s，例如 switchboard provider unset <名称> openai", raw, fields)
}

// baseURLFlag 校验地址的形状。store 的写入口还会再校验一遍（见 DESIGN 5.8），
// 这里提前报成用法错误，用户不必等写库失败才知道地址写错了。
func baseURLFlag(value string) error {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return usagef("--base-url %q 要以 http:// 或 https:// 开头，例如 --base-url https://api.example.com", value)
	}
	return nil
}

// cmdProviderAdd 只新建一家厂商。重名时报错并指向 provider set，不顺手改已有的行。
func cmdProviderAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "provider add", "新建一家厂商与它的协议路由（目前支持 anthropic、openai、responses 三种协议）")
	baseURL := fs.String("base-url", "", "厂商的默认地址，必填，如 https://api.deepseek.com")
	paths := newProviderPathFlags(fs)
	defProtocol := fs.String("default-protocol", "", "这家的默认协议：anthropic | openai | responses，必须落在这次声明的路由上；不给按 openai → responses → anthropic 取第一条")
	modelsPath := fs.String("models-path", "", "厂商级模型列表路径覆盖，相对 --base-url，必须以 / 开头；不给按各路由的 models_path 取")
	note := fs.String("note", "", "备注")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usagef("%s\n每次只新建一家厂商，改一家已有的厂商用 switchboard provider set <名称>", providerAddUsage)
	}
	name := fs.Arg(0)

	// 空值一律是用法错误：删除与清空走 `provider unset`，不做「空串即删除」的约定（见 DESIGN 4）。
	if flagWasSet(fs, "base-url") && strings.TrimSpace(*baseURL) == "" {
		return usagef("--base-url 传了空值：新建厂商必须给地址，例如 --base-url https://api.example.com\n%s", providerAddUsage)
	}
	if *baseURL == "" {
		return usagef("缺少 --base-url：新建厂商必须给地址，例如 switchboard provider add %s --base-url https://api.example.com --openai /v1/chat/completions\n%s", name, providerAddUsage)
	}
	if err := baseURLFlag(*baseURL); err != nil {
		return err
	}
	for _, f := range paths.pairs(fs) {
		if f.given && strings.TrimSpace(f.value) == "" {
			return emptyValueError(false, f.flagName)
		}
	}
	if flagWasSet(fs, "default-protocol") && strings.TrimSpace(*defProtocol) == "" {
		return emptyValueError(false, "default-protocol")
	}
	if flagWasSet(fs, "models-path") && strings.TrimSpace(*modelsPath) == "" {
		return emptyValueError(false, "models-path")
	}
	if flagWasSet(fs, "note") && strings.TrimSpace(*note) == "" {
		return emptyValueError(false, "note")
	}

	routes := make([]store.ProviderRoute, 0, 3)
	for _, f := range paths.pairs(fs) {
		if !f.given {
			continue
		}
		path, err := routePath(f)
		if err != nil {
			return err
		}
		routes = append(routes, store.ProviderRoute{Provider: name, Protocol: f.protocol, Path: path})
	}
	if len(routes) == 0 {
		return usagef("缺少协议路由：--anthropic、--openai、--responses 至少给一条，例如 switchboard provider add %s --base-url https://api.example.com --openai /v1/chat/completions；不再需要这家厂商就用 switchboard provider rm %s\n%s", name, name, providerAddUsage)
	}

	def, err := protocolFromCLI(*defProtocol)
	if err != nil {
		return err
	}
	// 没给 --models-path 时厂商级覆盖为空，按各路由的 models_path 取（见 DESIGN 5.7）。
	models := ""
	if flagWasSet(fs, "models-path") {
		if models, err = modelsPathFlag(*modelsPath); err != nil {
			return err
		}
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	p := store.Provider{Name: name, BaseURL: *baseURL, Note: *note, DefaultProtocol: def, ModelsPath: models}
	// 默认协议指不到这次声明的路由时 store 整笔拒绝，错误文案已经写清缺哪条、下一步敲什么。
	switch err := a.st.CreateProvider(p, routes); {
	case errors.Is(err, store.ErrProviderExists):
		return fmt.Errorf("厂商 %s 已存在，改用 switchboard provider set %s 修改", name, name)
	case err != nil:
		return err
	}

	snap, err := readProvider(a.st, name)
	if err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(snap.json)
	}
	fmt.Fprintf(e.Stdout, "已添加厂商 %s（%s），%d 条路由，默认协议 %s\n", snap.row.Name, snap.json.BaseURL, len(snap.routes), defaultProtocolCell(snap))
	fmt.Fprintf(e.Stdout, "下一步：switchboard account add <名称> --provider %s --key <key> 用它加账号\n", name)
	return nil
}

// cmdProviderSet 只设值：传了的字段改成新值，没传的字段一律保留。
// 删除一条路由、清掉覆盖或备注走 `provider unset`（见 DESIGN 4）。
func cmdProviderSet(e *Env, args []string) error {
	fs := newFlagSet(e, "provider set", "改一家厂商的地址、路由、默认协议或备注（目前支持 anthropic、openai、responses 三种协议）")
	baseURL := fs.String("base-url", "", "改厂商的默认地址")
	paths := newProviderPathFlags(fs)
	modelsPath := fs.String("models-path", "", "厂商级模型列表路径覆盖，必须以 / 开头")
	defProtocol := fs.String("default-protocol", "", "这家的默认协议：anthropic | openai | responses，必须落在已声明的路由上")
	note := fs.String("note", "", "改备注")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usagef("%s\n每次只改一家厂商，新建厂商用 switchboard provider add", providerSetUsage)
	}
	name := fs.Arg(0)

	patch := store.ProviderPatch{Routes: map[string]string{}}
	changed := make([]string, 0, 7)

	if flagWasSet(fs, "base-url") {
		if strings.TrimSpace(*baseURL) == "" {
			return usagef("--base-url 传了空值：地址是必填项，给完整地址，例如 --base-url https://api.example.com")
		}
		if err := baseURLFlag(*baseURL); err != nil {
			return err
		}
		patch.BaseURL = baseURL
		changed = append(changed, "base-url")
	}
	if flagWasSet(fs, "models-path") {
		if strings.TrimSpace(*modelsPath) == "" {
			return emptyValueError(true, "models-path")
		}
		models, err := modelsPathFlag(*modelsPath)
		if err != nil {
			return err
		}
		patch.ModelsPath = &models
		changed = append(changed, "models-path")
	}
	if flagWasSet(fs, "default-protocol") {
		if strings.TrimSpace(*defProtocol) == "" {
			return emptyValueError(true, "default-protocol")
		}
		def, err := protocolFromCLI(*defProtocol)
		if err != nil {
			return err
		}
		patch.DefaultProtocol = &def
		changed = append(changed, "default-protocol")
	}
	if flagWasSet(fs, "note") {
		if strings.TrimSpace(*note) == "" {
			return emptyValueError(true, "note")
		}
		patch.Note = note
		changed = append(changed, "note")
	}
	for _, f := range paths.pairs(fs) {
		if !f.given {
			continue
		}
		if strings.TrimSpace(f.value) == "" {
			return emptyValueError(true, f.flagName)
		}
		path, err := routePath(f)
		if err != nil {
			return err
		}
		patch.Routes[f.protocol] = path
		changed = append(changed, f.flagName)
	}
	if len(changed) == 0 {
		return usagef("没有要改的字段：至少给一个参数，比如 --base-url、--openai、--models-path、--default-protocol 或 --note\n%s", providerSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.SetProvider(name, patch); err != nil {
		return err
	}

	snap, err := readProvider(a.st, name)
	if err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(snap.json)
	}
	fmt.Fprintf(e.Stdout, "已修改厂商 %s：改动 %d 项（%s），现有 %d 条路由，默认协议 %s\n",
		snap.row.Name, len(changed), strings.Join(changed, "、"), len(snap.routes), defaultProtocolCell(snap))
	fmt.Fprintln(e.Stdout, "下一步：switchboard provider ls 核对")
	return nil
}

// cmdProviderUnset 删掉一条路由，或清掉模型列表覆盖、默认协议、备注。
// 要清的那一项本来就不存在时不报错：再跑一遍同一条命令的结果一样，输出会说明这次什么都没动。
func cmdProviderUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "provider unset", "删掉一条路由，或清掉模型列表覆盖、默认协议、备注")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return usagef("%s", providerUnsetUsage)
	}
	name := fs.Arg(0)

	// 字段名先全部校验、去重，再动库：一条写错的字段名不该让前面几个字段先落库。
	fields := make([]providerUnsetField, 0, fs.NArg()-1)
	seen := map[string]bool{}
	for _, raw := range fs.Args()[1:] {
		f, err := lookupUnsetField(raw)
		if err != nil {
			return err
		}
		if seen[f.name] {
			continue
		}
		seen[f.name] = true
		fields = append(fields, f)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 厂商不存在时 GetProvider 的错误已经写清用 provider add 新建、用 provider ls 看已有厂商。
	before, err := readProvider(a.st, name)
	if err != nil {
		return err
	}

	// 删掉的路由正好是显式默认协议时 store 会整笔拒绝，但它给出的下一步是「传空串」——
	// 命令行已经改成 provider unset，这里先用自己的话拦住，并算出一条仍然存在的路由当建议，
	// 免得把人指到一条会被拒的命令上。
	if !seen["default-protocol"] && before.row.DefaultProtocol != "" {
		deleting := map[string]bool{}
		for _, f := range fields {
			if f.protocol != "" && unsetPresent(before, f) {
				deleting[f.protocol] = true
			}
		}
		if deleting[before.row.DefaultProtocol] {
			remaining := make([]store.ProviderRoute, 0, len(before.routes))
			for _, r := range before.routes {
				if !deleting[r.Protocol] {
					remaining = append(remaining, r)
				}
			}
			alt := store.ResolveDefaultProtocol("", remaining)
			if alt == "" {
				return fmt.Errorf("厂商 %s 的默认协议是 %s，正要删的就是这条路由，删完一条都不剩；先加一条别的协议路由（switchboard provider set %s --anthropic <路径>），或不再需要这家厂商就用 switchboard provider rm %s",
					name, before.row.DefaultProtocol, name, name)
			}
			return fmt.Errorf("厂商 %s 的默认协议是 %s，正要删的就是这条路由；先换默认协议：switchboard provider set %s --default-protocol %s（改成仍然声明的路由），或 switchboard provider unset %s default-protocol（回到按固定顺序取第一条），再删这条路由",
				name, before.row.DefaultProtocol, name, cliProtocolName(alt), name)
		}
	}

	patch := store.ProviderPatch{Routes: map[string]string{}}
	lines := make([]string, 0, len(fields))
	changed := 0
	for _, f := range fields {
		if !unsetPresent(before, f) {
			lines = append(lines, fmt.Sprintf("%s 本来就没有 %s", name, f.what))
			continue
		}
		applyUnset(&patch, f)
		lines = append(lines, fmt.Sprintf("%s 已清掉 %s", name, f.what))
		changed++
	}

	snap := before
	if changed > 0 {
		// 删到一条路由都不剩时 store 整笔拒绝，错误文案指向 provider rm。
		if err := a.st.SetProvider(name, patch); err != nil {
			return err
		}
		if snap, err = readProvider(a.st, name); err != nil {
			return err
		}
	}

	if e.JSON {
		return e.printJSON(snap.json)
	}
	for _, line := range lines {
		fmt.Fprintln(e.Stdout, line)
	}
	fmt.Fprintf(e.Stdout, "现有 %d 条路由，默认协议 %s\n", len(snap.routes), defaultProtocolCell(snap))
	return nil
}

// unsetPresent 判断这一项现在有没有值：没有就不写库，让 unset 可以重复执行。
func unsetPresent(s providerSnapshot, f providerUnsetField) bool {
	switch {
	case f.protocol != "":
		for _, r := range s.routes {
			if r.Protocol == f.protocol {
				return true
			}
		}
		return false
	case f.name == "models-path":
		return s.row.ModelsPath != ""
	case f.name == "default-protocol":
		return s.row.DefaultProtocol != ""
	case f.name == "note":
		return s.row.Note != ""
	}
	return false
}

// applyUnset 把一项清空落到 patch 上：路由传空串即删（见 store.SetProvider），
// 三列设置传空串即回到默认行为。
func applyUnset(patch *store.ProviderPatch, f providerUnsetField) {
	empty := ""
	switch {
	case f.protocol != "":
		patch.Routes[f.protocol] = empty
	case f.name == "models-path":
		patch.ModelsPath = &empty
	case f.name == "default-protocol":
		patch.DefaultProtocol = &empty
	case f.name == "note":
		patch.Note = &empty
	}
}

func cmdProviderRm(e *Env, args []string) error {
	fs := newFlagSet(e, "provider rm", "删除厂商与它的全部路由")
	force := fs.Bool("force", false, "仍有账号用这个厂商时也删除")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：switchboard provider rm <名称> [--force]")
	}
	name := fs.Arg(0)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if _, err := a.st.GetProvider(name); err != nil {
		return err
	}
	routes, err := a.st.ProviderRoutes(name)
	if err != nil {
		return err
	}
	accounts, err := a.st.ListAccountsByProvider(name)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		names = append(names, acc.Name)
	}

	if len(names) > 0 && !*force {
		return fmt.Errorf("厂商 %q 还有 %d 个账号在用（%s），删掉后这些账号不能再用它补默认地址；确认要删就加 --force，或先 switchboard account rm 掉它们再用 switchboard provider rm %s",
			name, len(names), strings.Join(names, "、"), name)
	}

	if err := a.st.DeleteProvider(name); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"removed":  name,
			"routes":   len(routes),
			"accounts": names,
		})
	}
	fmt.Fprintf(e.Stdout, "已删除厂商 %s 与它的 %d 条路由\n", name, len(routes))
	if len(names) > 0 {
		fmt.Fprintf(e.Stdout, "这些账号仍保留自己的 base_url，不受影响：%s\n", strings.Join(names, "、"))
	}
	return nil
}

// isWireProtocol 判断是不是三种线协议之一。账号的 --protocol 与厂商的默认协议共用这一个判断。
func isWireProtocol(proto string) bool {
	switch proto {
	case gateway.ProtocolAnthropic, gateway.ProtocolChat, gateway.ProtocolResponses:
		return true
	}
	return false
}

// flagWasSet 判断某个参数有没有在命令行上出现过：用来区分「给了空值」与「没给」。
func flagWasSet(fs *flagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// providerDefaults 是厂商表能给账号补的两项默认值。
// Found 为 false 表示表里没有这一行；NoRoutes 为 true 表示厂商行在、但一条路由都没声明，
// 这时算不出默认协议，由调用方决定报错还是回退（见 DESIGN 5.8）。
type providerDefaults struct {
	BaseURL  string
	Protocol string
	Found    bool
	NoRoutes bool
}

// lookupProvider 按厂商表解析默认地址与默认协议。协议只问 store 的有效默认协议，
// 不在 cli 里再算一遍排序兜底（见 DESIGN 5.8）。
func lookupProvider(st *store.Store, name string) (providerDefaults, error) {
	p, err := st.GetProvider(name)
	if errors.Is(err, store.ErrProviderNotFound) {
		return providerDefaults{}, nil
	}
	if err != nil {
		return providerDefaults{}, err
	}
	d := providerDefaults{BaseURL: p.BaseURL, Found: true}
	proto, err := st.EffectiveProviderProtocol(name)
	if errors.Is(err, store.ErrProviderNoRoutes) {
		d.NoRoutes = true
		return d, nil
	}
	if err != nil {
		return providerDefaults{}, err
	}
	d.Protocol = proto
	return d, nil
}

// unknownProviderError 说明厂商表里没有这一行，并给出最接近的已知厂商与下一步命令。
func unknownProviderError(st *store.Store, name string) error {
	providers, err := st.ListProviders()
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		return fmt.Errorf("厂商表是空的，没法按厂商补 --base-url；先运行 switchboard provider ls 看已知厂商，或直接给 --base-url 自建地址，例如 switchboard account add <名称> --provider %s --base-url https://api.example.com --key <key>", name)
	}

	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name)
	}
	msg := fmt.Sprintf("厂商 %q 不在厂商表里（已知：%s）", name, strings.Join(names, "、"))
	if closest := closestName(names, name); closest != "" {
		msg += fmt.Sprintf("；最接近的是 %q，确认拼错就改 --provider %s 再试", closest, closest)
	}
	msg += fmt.Sprintf("；也可以直接用 --provider %s --base-url <地址> 自建地址，或先 switchboard provider add %s --base-url <地址> --openai /v1/chat/completions 把它登记进厂商表", name, name)
	return errors.New(msg)
}

// closestName 返回与 target 编辑距离不超过 2 的已知名字里最近的一个。
// 一个都不够近时返回空串：宁可不猜，也不要把用户引到一个无关的厂商上。
func closestName(cands []string, target string) string {
	best, bestDist := "", 3
	t := strings.ToLower(strings.TrimSpace(target))
	for _, c := range cands {
		if d := editDistance(t, strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance 是按字符算的 Levenshtein 距离，中文厂商名也能得到合理结果。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
