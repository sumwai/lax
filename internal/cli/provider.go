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
	{"ls", "列厂商、地址、默认协议与各协议的路由"},
	{"add", "写入或更新厂商，一次可以声明多条协议路由"},
	{"rm", "删除厂商与它的全部路由，账号还在用时要求 --force"},
}

// cmdProvider 是 provider 这一组子命令的入口。
func cmdProvider(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "provider", "厂商与路由", providerSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：ls | add | rm\n用 switchboard provider --help 查看子命令")
	}
	switch args[0] {
	case "ls":
		return cmdProviderLs(e, args[1:])
	case "add":
		return cmdProviderAdd(e, args[1:])
	case "rm":
		return cmdProviderRm(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | add | rm\n用 switchboard provider --help 查看子命令", args[0])
	}
}

// providerRouteJSON 是 provider ls --json 里的一条路由。
type providerRouteJSON struct {
	Protocol   string `json:"protocol"`
	Path       string `json:"path"`
	ModelsPath string `json:"models_path"`
}

// providerJSON 是 provider ls --json 的一行。protocols 恒为数组，没有路由时是 []。
type providerJSON struct {
	Name      string              `json:"name"`
	BaseURL   string              `json:"base_url"`
	Protocols []providerRouteJSON `json:"protocols"`
	Note      string              `json:"note"`
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

// defaultProtocol 是厂商的默认协议：按协议名排序的第一条路由。
// 账号的 --protocol 留空时用它，provider ls 也显示它，两处同一个规则。
func defaultProtocol(routes []store.ProviderRoute) string {
	if len(routes) == 0 {
		return ""
	}
	return routes[0].Protocol
}

func cmdProviderLs(e *Env, args []string) error {
	fs := newFlagSet(e, "provider ls", "列出厂商与路由")
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

	routesByProvider := make([][]store.ProviderRoute, len(providers))
	for i, p := range providers {
		routes, err := a.st.ProviderRoutes(p.Name)
		if err != nil {
			return err
		}
		routesByProvider[i] = routes
	}

	if e.JSON {
		out := make([]providerJSON, 0, len(providers))
		for i, p := range providers {
			out = append(out, providerJSON{
				Name:      p.Name,
				BaseURL:   p.BaseURL,
				Protocols: routeJSONs(routesByProvider[i]),
				Note:      p.Note,
			})
		}
		return e.printJSON(out)
	}

	if len(providers) == 0 {
		fmt.Fprintln(e.Stdout, "厂商表是空的，账号按 base_url 后缀猜路径。加一个厂商：switchboard provider add deepseek --base-url https://api.deepseek.com --route openai-chat=/v1/chat/completions:/v1/models")
		return nil
	}

	rows := make([][]string, 0, len(providers))
	for i, p := range providers {
		proto := defaultProtocol(routesByProvider[i])
		if proto == "" {
			proto = "-"
		}
		rows = append(rows, []string{p.Name, p.BaseURL, proto, routeCell(routesByProvider[i]), p.Note})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "地址", "默认协议", "路由与路径", "备注"}, rows))
	return nil
}

// routeFlags 收集可重复的 --route，每项原样保存表达式，解析与报错在 parseRoute 里。
type routeFlags []string

func (r *routeFlags) String() string { return strings.Join(*r, "、") }

func (r *routeFlags) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// routeExample 是 --route 的正确写法，每条错误文案都带上它，不让用户去猜格式。
const routeExample = "openai-chat=/v1/chat/completions:/v1/models"

// parseRoute 把「协议=路径[:模型列表路径]」拆成一条路由。
func parseRoute(provider, expr string) (store.ProviderRoute, error) {
	proto, rest, ok := strings.Cut(expr, "=")
	if !ok {
		return store.ProviderRoute{}, usagef("--route %q 缺少 =，写法是 <协议>=<路径>[:<模型列表路径>]，例如 --route %s", expr, routeExample)
	}
	proto = strings.TrimSpace(proto)
	if !isWireProtocol(proto) {
		return store.ProviderRoute{}, usagef("--route %q 的协议名 %q 不认识，只能是 anthropic、openai-chat、openai-responses，例如 --route %s", expr, proto, routeExample)
	}
	path, modelsPath, _ := strings.Cut(rest, ":")
	path, modelsPath = strings.TrimSpace(path), strings.TrimSpace(modelsPath)
	if !strings.HasPrefix(path, "/") {
		return store.ProviderRoute{}, usagef("--route %q 的路径要以 / 开头，例如 --route %s", expr, routeExample)
	}
	if modelsPath != "" && !strings.HasPrefix(modelsPath, "/") {
		return store.ProviderRoute{}, usagef("--route %q 的模型列表路径要以 / 开头，例如 --route %s", expr, routeExample)
	}
	return store.ProviderRoute{Provider: provider, Protocol: proto, Path: path, ModelsPath: modelsPath}, nil
}

func cmdProviderAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "provider add", "写入或更新厂商与它的路由")
	baseURL := fs.String("base-url", "", "厂商的默认地址，如 https://api.deepseek.com")
	note := fs.String("note", "", "备注，留空时保留已有备注")
	var exprs routeFlags
	fs.Var(&exprs, "route", "路由表达式 <协议>=<路径>[:<模型列表路径>]，可重复，例如 "+routeExample)
	if err := fs.Parse(args); err != nil {
		return err
	}

	usage := fmt.Sprintf("用法：switchboard provider add <名称> --base-url <地址> [--note <备注>] [--route %s]", routeExample)
	if fs.NArg() > 1 {
		return usagef("%s", usage)
	}
	name := fs.Arg(0)
	if name == "" {
		return usagef("%s", usage)
	}
	if *baseURL == "" {
		return usagef("--base-url 必填，例如 switchboard provider add %s --base-url https://api.example.com --route %s", name, routeExample)
	}
	if !strings.HasPrefix(*baseURL, "http://") && !strings.HasPrefix(*baseURL, "https://") {
		return usagef("--base-url %q 要以 http:// 或 https:// 开头，例如 switchboard provider add %s --base-url https://api.example.com", *baseURL, name)
	}

	routes := make([]store.ProviderRoute, 0, len(exprs))
	for _, expr := range exprs {
		r, err := parseRoute(name, expr)
		if err != nil {
			return err
		}
		routes = append(routes, r)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 末尾斜杠在这里去掉，存进来的就是拼接时实际用的形式（见 DESIGN 5.8）。
	p := store.Provider{Name: name, BaseURL: strings.TrimRight(*baseURL, "/"), Note: *note}
	existing, getErr := a.st.GetProvider(name)
	switch {
	case getErr == nil:
		// 没给 --note 时保留原备注：改一条路由不该把备注顺手清掉。
		if !flagWasSet(fs, "note") {
			p.Note = existing.Note
		}
	case errors.Is(getErr, store.ErrProviderNotFound):
	default:
		return getErr
	}

	if err := a.st.AddProvider(p, routes); err != nil {
		return fmt.Errorf("写入厂商 %s 失败: %w", name, err)
	}

	after, err := a.st.ProviderRoutes(name)
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(providerJSON{
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			Protocols: routeJSONs(after),
			Note:      p.Note,
		})
	}
	fmt.Fprintf(e.Stdout, "已写入厂商 %s（%s），这次提交 %d 条路由，现有 %d 条\n", p.Name, p.BaseURL, len(routes), len(after))
	if len(after) == 0 {
		fmt.Fprintln(e.Stdout, "还没有路由，账号会按 base_url 后缀猜路径；声明一条：switchboard provider add "+name+" --base-url "+p.BaseURL+" --route "+routeExample)
	}
	fmt.Fprintf(e.Stdout, "下一步：switchboard provider ls 核对，switchboard account add <名称> --provider %s --key <key> 用它加账号\n", name)
	return nil
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

// isWireProtocol 判断是不是三种线协议之一。厂商路由与账号的 --protocol 共用这一个判断。
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

// lookupProvider 按厂商表解析默认地址与默认协议，第二个返回值 false 表示表里没有这一行。
// 厂商行存在但一条路由都没声明时协议回退到 openai-chat，地址仍是厂商的 base_url。
func lookupProvider(st *store.Store, name string) (baseURL, protocol string, found bool, err error) {
	p, err := st.GetProvider(name)
	if errors.Is(err, store.ErrProviderNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	routes, err := st.ProviderRoutes(name)
	if err != nil {
		return "", "", false, err
	}
	proto := defaultProtocol(routes)
	if proto == "" {
		proto = gateway.ProtocolChat
	}
	return p.BaseURL, proto, true, nil
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
	msg += fmt.Sprintf("；也可以直接用 --provider %s --base-url <地址> 自建地址，或先 switchboard provider add %s --base-url <地址> 把它登记进厂商表", name, name)
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
