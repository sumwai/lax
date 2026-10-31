package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"lax/internal/gateway"
	"lax/internal/store"
)

// providerSubs 是 provider 一组的子命令清单，组级帮助与错误提示都读它。
var providerSubs = []groupItem{
	{"ls", "列厂商、地址、默认协议、各协议路由与模型列表路径"},
	{"add", "新建一家厂商与它的协议路由"},
	{"set", "改一家厂商的地址、路由、默认协议或备注"},
	{"unset", "删掉一条路由，或清掉模型列表覆盖、默认协议、备注"},
	{"rm", "删除厂商与它的全部路由，账号还在用时要求 --force"},
	{"header", "声明上游除凭据头之外还要的附加请求头：set / unset / ls"},
}

// 六个协议 flag（`--anthropic`、`--openai`、`--responses`、`--gemini`、`--systemone`、
// `--embeddings`）的取值有两种写法：相对厂商 base_url 的完整路径，或整条地址（含 `://`，直接用、
// 不再拼 base_url）。相对路径由 store.NormalizePath 规整、不拒绝输入：`v1/chat/completions`、
// `//v1//chat/completions`、`/v1/chat/completions/` 落成同一个地址；整条地址交给
// store.NormalizeEndpoint 校验 scheme 与主机名，因此 `https://`、`https:///x`、`ftp://host/x`
// 都是用法错误。cli 只判两件事：「规整之后为空」与「整条地址校验不过」，删除语义归 provider unset。
// gemini 的入站与出站都可用，路由路径里的 `{model}` 占位符原样落库，由网关在
// 发请求之前换成模型名：cli 不展开、不校验、也不改写它。systemone 的入站与出站是同一个
// `POST /v1/systemone`，没有占位符，与其它协议走同一套规整与校验。

// 三份用法行，所有用法错误都把它打出来，不让用户看着一句「参数错误」去猜参数。
// `--base-url` 在 add 里是可选的：每条路由与 models_path 都写成整条地址时，它拼不进任何地址，
// 这一项确实无处可用，给了也只过一遍本机校验、不参与拼地址。
const (
	providerAddUsage   = "用法：lax provider add <名称> [--base-url <地址>] [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--gemini <路径>] [--systemone <路径>] [--embeddings <路径>] [--default-protocol <协议>] [--models-path <路径>] [--note <备注>]"
	providerSetUsage   = "用法：lax provider set <名称> [--base-url <地址>] [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--gemini <路径>] [--systemone <路径>] [--embeddings <路径>] [--models-path <路径>] [--default-protocol <协议>] [--note <备注>]"
	providerUnsetUsage = "用法：lax provider unset <名称> <字段> [<字段>...]\n可用字段：anthropic、openai、responses、gemini、systemone、embeddings、models-path、default-protocol、note"
)

// providerUnsetField 是一个可以被 `provider unset` 清掉的字段。字段名用 flag 名，
// 删路由的几项带上线协议；base-url 与厂商名不在这里：前者只能改成另一个地址（清掉之后相对路径
// 的路由就拼不出完整地址），后者是主键。
type providerUnsetField struct {
	name     string // 位置参数上的字段名，也是这一项的 flag 名
	what     string // 人读输出里的说法：输出里写「anthropic 路由」比写 anthropic 清楚
	protocol string // 删掉路由时的线协议；空串表示这一项不是路由，不碰 provider_routes
}

var providerUnsetFields = []providerUnsetField{
	{"anthropic", "anthropic 路由", gateway.ProtocolAnthropic},
	{"openai", "openai 路由", gateway.ProtocolChat},
	{"responses", "openai-responses 路由", gateway.ProtocolResponses},
	{"gemini", "gemini 路由", gateway.ProtocolGemini},
	{"systemone", "systemone 路由", gateway.ProtocolSystemOne},
	{"embeddings", "openai-embeddings 路由", gateway.ProtocolEmbeddings},
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
		return usagef("需要子命令：ls | add | set | unset | rm | header\n用 lax provider --help 查看子命令")
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
	case "header":
		return cmdProviderHeader(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | add | set | unset | rm | header\n用 lax provider --help 查看子命令", args[0])
	}
}

// providerRouteJSON 是 provider ls --json 里的一条路由。
type providerRouteJSON struct {
	Protocol   string `json:"protocol"`
	Path       string `json:"path"`
	ModelsPath string `json:"models_path"`
}

// providerJSON 是 provider ls / add / set / unset --json 的一行。protocols 恒为数组，没有路由时是 []。
// DefaultProtocol 是有效默认协议，一条路由都没有时为空串。
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

// joinCN 按中文正文的空格规则拼两段文本：两侧都是中文时不加空格，
// 中文与拉丁字母、数字之间加一个空格。逐项确认的行因此写成「mine 已清掉 anthropic 路由」
// 与「mine 已清掉备注」，不会多出「已清掉 备注」这种空格。
func joinCN(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	last, _ := utf8.DecodeLastRuneInString(left)
	first, _ := utf8.DecodeRuneInString(right)
	if last > unicode.MaxASCII && first > unicode.MaxASCII {
		return left + right
	}
	return left + " " + right
}

// providerModelsCell 是 provider ls 的「模型列表」列：厂商级覆盖非空就显示它，否则显示各路由
// 自己的 models_path，都没有时显示 -（这种情况 model sync 会按 base_url 后缀猜）。
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

// effectiveDefaultProtocol 问 store 要厂商的有效默认协议，cli 不自己算排序兜底：两份判定迟早对不上。
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

// baseURLCell 是地址列：base_url 可以为空（每条路由与 models_path 都是整条地址时它拼不进
// 任何地址），空值显示 -，与默认协议列同一种写法，不打出一个空单元格。
func baseURLCell(base string) string {
	if base == "" {
		return "-"
	}
	return base
}

// baseURLSummary 是确认行里的地址：空值写清「无 base_url」，比一个空括号有用。
func baseURLSummary(base string) string {
	if base == "" {
		return "无 base_url，路由都是整条地址"
	}
	return base
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
		fmt.Fprintln(e.Stdout, "厂商表是空的，账号按 base_url 后缀猜路径。加一个厂商：lax provider add deepseek --base-url https://api.deepseek.com --openai /v1/chat/completions --anthropic /anthropic/v1/messages")
		return nil
	}

	rows := make([][]string, 0, len(snaps))
	for _, s := range snaps {
		rows = append(rows, []string{
			s.row.Name, baseURLCell(s.json.BaseURL), defaultProtocolCell(s),
			routeCell(s.routes), providerModelsCell(s.row, s.routes), s.json.Note,
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "地址", "默认协议", "路由与路径", "模型列表", "备注"}, rows))
	return nil
}

// providerPathFlags 是五个协议 flag 的当前值。add 用它们声明路由，set 用它们覆盖一条路由的路径。
type providerPathFlags struct {
	anthropic  *string
	openai     *string
	responses  *string
	gemini     *string
	systemone  *string
	embeddings *string
}

func newProviderPathFlags(fs *flagSet) providerPathFlags {
	return providerPathFlags{
		anthropic: fs.String("anthropic", "", "anthropic 协议的路由路径：相对 --base-url 的完整路径，例如 /v1/messages；也可以写整条地址，例如 https://claude.example.com/v1/messages（相对路径的斜杠写法由程序规整）"),
		openai:    fs.String("openai", "", "openai-chat 协议的路由路径：相对 --base-url 的完整路径，例如 /v1/chat/completions；也可以写整条地址，例如 https://openai.example.com/v1/chat/completions（相对路径的斜杠写法由程序规整）"),
		responses: fs.String("responses", "", "openai-responses 协议的路由路径：相对 --base-url 的完整路径，例如 /v1/responses；也可以写整条地址（相对路径的斜杠写法由程序规整）"),
		gemini: fs.String("gemini", "", "gemini 协议的路由路径：相对 --base-url 的完整路径，例如 /v1beta/models/{model}:generateContent；也可以写整条地址（相对路径的斜杠写法由程序规整）。\n"+
			"{model} 是占位符，原样落库，发请求时才换成模型名（按路径段转义），例：\n"+
			"  lax provider add google-gemini --base-url https://generativelanguage.googleapis.com --gemini /v1beta/models/{model}:generateContent --default-protocol gemini"),
		systemone: fs.String("systemone", "", "systemone 决策协议的路由路径：相对 --base-url 的完整路径，例如 /v1/systemone；也可以写整条地址（相对路径的斜杠写法由程序规整）。\n"+
			"这条协议一个 state 加一组问题、不出文本，lax decide 用它，例：\n"+
			"  lax provider add typesafe --base-url https://api.typesafe.ai --systemone /v1/systemone\n"+
			"  只声明 systemone 这一条时再加 --default-protocol systemone（固定回退顺序只认 openai-chat → openai-responses → anthropic）"),
		embeddings: fs.String("embeddings", "", "openai-embeddings 向量化协议的路由路径：相对 --base-url 的完整路径，例如 /v1/embeddings；也可以写整条地址（相对路径的斜杠写法由程序规整）。\n"+
			"向量化只做同协议直通，/v1/embeddings 入站只打声明了这条路由的厂商，例：\n"+
			"  lax provider set openai openai-embeddings=/v1/embeddings"),
	}
}

// providerPathFlag 是一个协议 flag 本次的取值与它是否出现在命令行上。
type providerPathFlag struct {
	flagName string
	protocol string
	value    string
	given    bool
}

// pairs 按 anthropic、openai、responses、gemini、systemone 的固定顺序列出五个 flag，
// 报错顺序因此稳定。
func (p providerPathFlags) pairs(fs *flagSet) []providerPathFlag {
	return []providerPathFlag{
		{"anthropic", gateway.ProtocolAnthropic, *p.anthropic, flagWasSet(fs, "anthropic")},
		{"openai", gateway.ProtocolChat, *p.openai, flagWasSet(fs, "openai")},
		{"responses", gateway.ProtocolResponses, *p.responses, flagWasSet(fs, "responses")},
		{"gemini", gateway.ProtocolGemini, *p.gemini, flagWasSet(fs, "gemini")},
		{"systemone", gateway.ProtocolSystemOne, *p.systemone, flagWasSet(fs, "systemone")},
		{"embeddings", gateway.ProtocolEmbeddings, *p.embeddings, flagWasSet(fs, "embeddings")},
	}
}

// pathGiven 判断一个路径类取值算不算给了值：空串、只有空白、只有斜杠都算没给。
// 判定走 store.NormalizePath，cli 因此只多判「规整之后为空」一条，不另立一套斜杠写法校验。整条地址的取值同样算给了值：它在 checkEndpoint 里单独判。
func pathGiven(raw string) bool { return store.NormalizePath(raw) != "" }

// checkRoutePathFlag 校验一个协议 flag 的取值。两条用法错误：空值（删除语义归 provider unset）、
// 整条地址校验不过（缺主机名、scheme 不是 http(s)，判定交给 store.NormalizeEndpoint）。
// 其余写法——缺前导斜杠、重复斜杠、末尾斜杠——继续交给 store 规整，cli 不拒绝：
// 路径是抄上游文档抄来的，多一个斜杠不算用法错误。
func checkRoutePathFlag(f providerPathFlag, setCmd bool, usageLine string) error {
	if !f.given {
		return nil
	}
	if !pathGiven(f.value) {
		return emptyValueError(setCmd, f.flagName, usageLine)
	}
	return checkEndpoint(f.value, usageLine)
}

// checkModelsPathFlag 校验 --models-path 的取值，规则与协议 flag 相同：相对路径与整条地址都收。
func checkModelsPathFlag(value string, setCmd bool, usageLine string) error {
	if !pathGiven(value) {
		return emptyValueError(setCmd, "models-path", usageLine)
	}
	return checkEndpoint(value, usageLine)
}

// protocolFromCLI 把 --default-protocol 的取值换成线协议。命令行写法是 anthropic、openai、responses、
// gemini、systemone，线协议全名（anthropic、openai-chat、openai-responses、gemini、systemone）也认：
// 输入 openai 就是 chat，gemini 与 systemone 两种写法同形。
func protocolFromCLI(value string) (string, error) {
	return protocolFromCLIName(value, "--default-protocol",
		"lax provider add acme --base-url https://api.example.com --openai /v1/chat/completions --default-protocol openai")
}

// protocolFromCLIName 是 protocolFromCLI 的同一套映射，只把错误文案里的参数名与示例换成调用方那一个：
// provider header 的 --protocol 也收这四个命令行写法，落库的必须是线协议全名：
// 库里的路由与行按线协议全名匹配，存了一简称就永远匹配不上。
func protocolFromCLIName(value, flagName, example string) (string, error) {
	switch v := strings.TrimSpace(value); v {
	case "":
		return "", nil
	case "openai", gateway.ProtocolChat:
		return gateway.ProtocolChat, nil
	case "responses", gateway.ProtocolResponses:
		return gateway.ProtocolResponses, nil
	case gateway.ProtocolAnthropic:
		return gateway.ProtocolAnthropic, nil
	case gateway.ProtocolGemini:
		return gateway.ProtocolGemini, nil
	case gateway.ProtocolSystemOne:
		return gateway.ProtocolSystemOne, nil
	case "embeddings", gateway.ProtocolEmbeddings:
		return gateway.ProtocolEmbeddings, nil
	}
	return "", usagef("%s %q 不认识，可用 anthropic、openai、responses、gemini、systemone、embeddings（也认线协议全名 openai-chat、openai-responses、openai-embeddings），例如 %s", flagName, value, example)
}

// emptyValueError 是「给了参数但值是空」的统一文案，路径参数规整之后为空也算空。
// 空串不再表示删除或清空：两者一律走 `provider unset`，避免 --note "" 这类写法
// 顺手把已有值抹掉。末尾附上这个子命令的用法行，用户不必再猜语法。
func emptyValueError(setCmd bool, flagName, usageLine string) error {
	if setCmd {
		return usagef("--%s 传了空值：provider set 只负责设值，取值不能为空；删除一条路由或清掉一项设置用 lax provider unset <名称> %s\n%s",
			flagName, flagName, usageLine)
	}
	return usagef("--%s 传了空值：新建厂商不接受空值，去掉这个参数再试；建好之后要删掉就用 lax provider unset <名称> %s\n%s",
		flagName, flagName, usageLine)
}

// lookupUnsetField 把一个位置参数换成字段描述。未知字段、带 -- 前缀、大小写不符都当场报错
// 并列出可用字段：静默忽略会让「以为删了」留到下游转发才发现。
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
		return providerUnsetField{}, usagef("字段名 %q 不要带 -- 前缀，直接写名字，例如 lax provider unset <名称> openai；可用：%s", raw, fields)
	}
	switch name {
	case "base-url":
		return providerUnsetField{}, usagef("base-url 不能 unset：清掉之后相对路径的路由就拼不出完整地址；改地址用 lax provider set <名称> --base-url <地址>，不需要地址的厂商从一开始就该省掉这个参数（见 DESIGN 5.8）")
	case "name", "名称", "厂商名":
		return providerUnsetField{}, usagef("厂商名不能 unset：它是主键，清不掉；不再需要这家厂商就用 lax provider rm <名称>")
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
	return providerUnsetField{}, usagef("字段名 %q 不认识，可用：%s，例如 lax provider unset <名称> openai", raw, fields)
}

// cmdProviderAdd 只新建一家厂商。重名时报错并指向 provider set，不顺手改已有的行。
func cmdProviderAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "provider add", "新建一家厂商与它的协议路由（支持 anthropic、openai、responses、gemini、systemone 五种协议）")
	baseURL := fs.String("base-url", "", "厂商的默认地址，如 https://api.deepseek.com（可省略：每条路由与 --models-path 都是整条地址时不需要它）")
	paths := newProviderPathFlags(fs)
	defProtocol := fs.String("default-protocol", "", "这家的默认协议：anthropic | openai | responses | gemini | systemone，必须落在这次声明的路由上；不给按 openai → responses → anthropic 取第一条")
	modelsPath := fs.String("models-path", "", "厂商级模型列表路径覆盖，相对 --base-url 的路径（如 /v1/models）或整条地址；不给按各路由的 models_path 取（相对路径的斜杠写法由程序规整）")
	note := fs.String("note", "", "备注")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usagef("%s\n每次只新建一家厂商，改一家已有的厂商用 lax provider set <名称>", providerAddUsage)
	}
	name := fs.Arg(0)

	// 空值一律是用法错误：删除与清空走 `provider unset`，不做「空串即删除」的约定。
	// 不给 --base-url 则是合法的：三条路由与 models_path 都是整条地址时它拼不进任何地址，
	// 这个前提由 store.CreateProvider 判（store 是唯一一份校验），cli 只把它包成用法错误。
	if flagWasSet(fs, "base-url") && strings.TrimSpace(*baseURL) == "" {
		return usagef("--base-url 传了空值：要么给一个带主机名的完整地址（例如 --base-url https://api.example.com），要么整个去掉这个参数\n%s", providerAddUsage)
	}
	// 地址交给 store.NormalizeBaseURL 判定与规整，cli 不另写一份 scheme/主机名预检；
	// 落库用的就是它返回的形态，与 provider set、account add 同一份规则：
	// 三处各写一份预检的话，同一串地址在三条命令上会有两种命运。
	if *baseURL != "" {
		norm, err := checkBaseURL(*baseURL, providerAddUsage)
		if err != nil {
			return err
		}
		*baseURL = norm
	}
	for _, f := range paths.pairs(fs) {
		if err := checkRoutePathFlag(f, false, providerAddUsage); err != nil {
			return err
		}
	}
	if flagWasSet(fs, "default-protocol") && strings.TrimSpace(*defProtocol) == "" {
		return emptyValueError(false, "default-protocol", providerAddUsage)
	}
	if flagWasSet(fs, "models-path") {
		if err := checkModelsPathFlag(*modelsPath, false, providerAddUsage); err != nil {
			return err
		}
	}
	if flagWasSet(fs, "note") && strings.TrimSpace(*note) == "" {
		return emptyValueError(false, "note", providerAddUsage)
	}

	// 路径取值原样交给 store：写法（前导斜杠、重复斜杠、末尾斜杠）由 NormalizePath 规整，
	// cli 不提前拒绝。上面只判过「规整之后为空」与「写成了整条地址」两种用法错误。
	routes := make([]store.ProviderRoute, 0, 5)
	for _, f := range paths.pairs(fs) {
		if !f.given {
			continue
		}
		routes = append(routes, store.ProviderRoute{Provider: name, Protocol: f.protocol, Path: f.value})
	}
	if len(routes) == 0 {
		return usagef("缺少协议路由：--anthropic、--openai、--responses、--gemini、--systemone 至少给一条，例如 lax provider add %s --base-url https://api.example.com --openai /v1/chat/completions；不再需要这家厂商就用 lax provider rm %s\n%s", name, name, providerAddUsage)
	}

	def, err := protocolFromCLI(*defProtocol)
	if err != nil {
		return err
	}
	// 没给 --models-path 时厂商级覆盖为空，按各路由的 models_path 取。
	models := ""
	if flagWasSet(fs, "models-path") {
		models = *modelsPath
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	p := store.Provider{Name: name, BaseURL: *baseURL, Note: *note, DefaultProtocol: def, ModelsPath: models}
	// 默认协议指不到这次声明的路由、或有相对路径而缺 base_url 时 store 整笔拒绝，
	// 错误文案已经写清缺哪条、下一步敲什么；缺 base_url 是命令写法的问题，包成用法错误（退出码 2）。
	switch err := a.st.CreateProvider(p, routes); {
	case errors.Is(err, store.ErrProviderExists):
		return fmt.Errorf("厂商 %s 已存在，改用 lax provider set %s 修改", name, name)
	case errors.Is(err, store.ErrBaseURLRequired):
		return usagef("%v\n%s", err, providerAddUsage)
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
	fmt.Fprintf(e.Stdout, "已添加厂商 %s（%s），%d 条路由，默认协议 %s\n", snap.row.Name, baseURLSummary(snap.row.BaseURL), len(snap.routes), defaultProtocolCell(snap))
	fmt.Fprintf(e.Stdout, "下一步：lax account add <名称> --provider %s --key <key> 用它加账号\n", name)
	return nil
}

// cmdProviderSet 只设值：传了的字段改成新值，没传的字段一律保留。
// 删除一条路由、清掉覆盖或备注走 `provider unset`：只设值不删值，一个写错的空参数不会抹掉已有配置。
func cmdProviderSet(e *Env, args []string) error {
	fs := newFlagSet(e, "provider set", "改一家厂商的地址、路由、默认协议或备注（支持 anthropic、openai、responses、gemini、systemone 五种协议）")
	baseURL := fs.String("base-url", "", "改厂商的默认地址（带主机名的完整地址；每条路由都是整条地址时不需要它）")
	paths := newProviderPathFlags(fs)
	modelsPath := fs.String("models-path", "", "厂商级模型列表路径覆盖，相对 --base-url 的路径（如 /v1/models）或整条地址（相对路径的斜杠写法由程序规整）")
	defProtocol := fs.String("default-protocol", "", "这家的默认协议：anthropic | openai | responses | gemini | systemone，必须落在已声明的路由上")
	note := fs.String("note", "", "改备注")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usagef("%s\n每次只改一家厂商，新建厂商用 lax provider add", providerSetUsage)
	}
	name := fs.Arg(0)

	patch := store.ProviderPatch{Routes: map[string]string{}}
	changed := make([]string, 0, 7)

	if flagWasSet(fs, "base-url") {
		if strings.TrimSpace(*baseURL) == "" {
			return usagef("--base-url 传了空值：改地址要给带主机名的完整地址，例如 --base-url https://api.example.com；不需要地址的厂商本来就该省略这一项（见 DESIGN 5.8）")
		}
		// 地址走 checkBaseURL，与 provider add、account add 是同一份校验与规整：含 `://` 的整条地址
		// 交给 store.NormalizeEndpoint 校验 scheme 与主机名，其余按相对路径规整。cli 不另写一份预检，
		// 两份判定迟早对不上，`https://` 这种只有 scheme 的写法会被误报成「地址为空」。
		norm, err := checkBaseURL(*baseURL, providerSetUsage)
		if err != nil {
			return err
		}
		*baseURL = norm
		patch.BaseURL = baseURL
		changed = append(changed, "base-url")
	}
	if flagWasSet(fs, "models-path") {
		if err := checkModelsPathFlag(*modelsPath, true, providerSetUsage); err != nil {
			return err
		}
		patch.ModelsPath = modelsPath
		changed = append(changed, "models-path")
	}
	if flagWasSet(fs, "default-protocol") {
		if strings.TrimSpace(*defProtocol) == "" {
			return emptyValueError(true, "default-protocol", providerSetUsage)
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
			return emptyValueError(true, "note", providerSetUsage)
		}
		patch.Note = note
		changed = append(changed, "note")
	}
	for _, f := range paths.pairs(fs) {
		if !f.given {
			continue
		}
		if err := checkRoutePathFlag(f, true, providerSetUsage); err != nil {
			return err
		}
		// 写法交给 store 规整，不再要求以 / 开头。
		patch.Routes[f.protocol] = f.value
		changed = append(changed, f.flagName)
	}
	if len(changed) == 0 {
		return usagef("没有要改的字段：至少给一个参数，比如 --base-url、--openai、--gemini、--systemone、--models-path、--default-protocol 或 --note\n%s", providerSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.SetProvider(name, patch); err != nil {
		// 把一条路由从整条地址改成相对路径、又没给 --base-url、库里也没有时，store 整笔拒绕：
		// 这是命令写法的问题，包成用法错误（退出码 2），文案保留 store 的原话。
		if errors.Is(err, store.ErrBaseURLRequired) {
			return usagef("%v\n%s", err, providerSetUsage)
		}
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
	fmt.Fprintln(e.Stdout, "下一步：lax provider ls 核对")
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

	patch := store.ProviderPatch{Routes: map[string]string{}}
	lines := make([]string, 0, len(fields))
	changed := 0
	for _, f := range fields {
		if !unsetPresent(before, f) {
			lines = append(lines, joinCN(joinCN(name, "本来就没有"), f.what))
			continue
		}
		applyUnset(&patch, f)
		lines = append(lines, joinCN(joinCN(name, "已清掉"), f.what))
		changed++
	}

	snap := before
	if changed > 0 {
		// 删到一条路由都不剩、或删掉的正好是默认协议那条时，store 整笔拒绝并把两条出路写在文案里
		// （换成别条已声明的协议，或 provider unset <名称> default-protocol）。cli 不另算一份回退规则。
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
		return usagef("用法：lax provider rm <名称> [--force]")
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
		return fmt.Errorf("厂商 %q 还有 %d 个账号在用（%s），删掉后这些账号不能再用它补默认地址；确认要删就加 --force，或先 lax account rm 掉它们再用 lax provider rm %s",
			name, len(names), strings.Join(names, "、"), name)
	}

	// --force 走 store.DeleteProviderForce：同一个事务里把指向它的账号停用，
	// 否则这些账号留在选号池里必然 502（它们已经没有地址与路由了）。
	if len(names) > 0 {
		if err := a.st.DeleteProviderForce(name); err != nil {
			return err
		}
	} else if err := a.st.DeleteProvider(name); err != nil {
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
		fmt.Fprintf(e.Stdout, "这些账号已停用（地址与路由随厂商一起没了）：%s；重建厂商或改账号的厂商之后用 lax account enable 才能接着用\n", strings.Join(names, "、"))
	}
	return nil
}

// isWireProtocol 判断一个取值是不是线协议。判定只有 store.IsWireProtocol 一份（六条线协议：
// gemini 的入站与出站都可用；systemone 的入站与出站是同一个 POST /v1/systemone；向量化是第六条）：
// 账号的 --protocol 与厂商的默认协议共用这一个判断，两处各写一份名单迟早会对不上。
func isWireProtocol(proto string) bool {
	return store.IsWireProtocol(proto)
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

// unknownProviderError 说明厂商表里没有这一行，并给出最接近的已知厂商与下一步命令。
// 账号的地址与默认协议只在厂商行上，所以出路只有一条：先把厂商登记进去。
func unknownProviderError(st *store.Store, name string) error {
	providers, err := st.ListProviders()
	if err != nil {
		return err
	}
	register := fmt.Sprintf("lax provider add %s --base-url <地址> --openai /v1/chat/completions", name)
	if len(providers) == 0 {
		// 厂商表可以被删光，这时「最接近的名字」无从谈起，只给怎么登记。
		return fmt.Errorf("厂商表是空的，没有地址可用；先运行 %s 登记这家厂商，再建账号", register)
	}

	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name)
	}
	msg := fmt.Sprintf("厂商 %q 不在厂商表里（已知：%s）", name, strings.Join(names, "、"))
	if closest := closestName(names, name); closest != "" {
		msg += fmt.Sprintf("；最接近的是 %q，确认拼错就改 --provider %s 再试", closest, closest)
	}
	msg += fmt.Sprintf("；地址与默认协议只存在厂商行上，先运行 %s 登记，再建账号", register)
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
