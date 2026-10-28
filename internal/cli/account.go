package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/gateway"
	"switchboard/internal/secret"
	"switchboard/internal/store"
)

// accountSubs 是 account 一组的子命令清单，组级帮助与错误提示都读它。
var accountSubs = []groupItem{
	{"add", "新增一个上游账号，key 加密后入库"},
	{"ls", "列出账号、模型数与当前状态"},
	{"rm", "删除账号，历史请求记录保留"},
	{"test", "用最小请求探活一个账号"},
	{"enable", "启用账号并清掉冷却"},
	{"disable", "停用账号，不再参与选号"},
}

func cmdAccount(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "account", "管理上游账号", accountSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：add | ls | rm | test | enable | disable\n用 switchboard account --help 查看子命令")
	}
	switch args[0] {
	case "add":
		return cmdAccountAdd(e, args[1:])
	case "ls":
		return cmdAccountLs(e, args[1:])
	case "rm":
		return cmdAccountRm(e, args[1:])
	case "test":
		return cmdAccountTest(e, args[1:])
	case "enable":
		return cmdAccountSetEnabled(e, args[1:], "enable", true)
	case "disable":
		return cmdAccountSetEnabled(e, args[1:], "disable", false)
	default:
		return usagef("未知子命令 %q，可用：add | ls | rm | test | enable | disable\n用 switchboard account --help 查看子命令", args[0])
	}
}

func cmdAccountSetEnabled(e *Env, args []string, sub string, enabled bool) error {
	fs := newFlagSet(e, "account "+sub, "启用或停用账号")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：switchboard account %s <名称>", sub)
	}
	name := fs.Arg(0)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	acc, err := a.st.GetAccount(name)
	if err != nil {
		return fmt.Errorf("账号 %q 不存在", name)
	}
	reason := ""
	if !enabled {
		reason = "手工停用"
	}
	if err := a.st.SetEnabled(acc.ID, enabled, reason); err != nil {
		return err
	}
	if enabled {
		// 手工启用意味着人工确认过，顺手清掉冷却，不用再等。
		if err := a.st.ClearFailure(acc.ID); err != nil {
			return err
		}
	}

	word := "已启用"
	if !enabled {
		word = "已停用"
	}
	if e.JSON {
		return e.printJSON(map[string]any{"name": name, "enabled": enabled})
	}
	fmt.Fprintf(e.Stdout, "%s %s\n", word, name)
	return nil
}

// accountAddUsage 是 account add 的用法行，用法错误与 --base-url 校验失败时都打出来。
const accountAddUsage = "用法：switchboard account add <名称> --provider <厂商> --key <key> [--protocol <协议>] [--base-url <地址>] [--models <模型>] [--priority N]"

func cmdAccountAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "account add", "新增一个上游账号")
	provider := fs.String("provider", "", "上游厂商标识，如 deepseek")
	key := fs.String("key", "", "上游 API key；给 - 从标准输入读，留空则读环境变量 SWITCHBOARD_KEY")
	name := fs.String("name", "", "账号名称，也可以用位置参数给出")
	protocol := fs.String("protocol", "", "上游线协议：anthropic | openai-chat | openai-responses，留空按厂商默认")
	baseURL := fs.String("base-url", "", "上游地址，留空按厂商默认（必须是带主机名的完整地址，如 https://api.deepseek.com）")
	models := fs.String("models", "", "该账号可提供的模型，逗号分隔")
	priceIn := fs.Float64("price-in", 0, "输入价，每百万 token")
	priceOut := fs.Float64("price-out", 0, "输出价，每百万 token")
	priority := fs.Int("priority", 100, "同模型多个账号时的优先级，小的先试")
	if err := fs.Parse(args); err != nil {
		return err
	}

	accountName := *name
	if accountName == "" && fs.NArg() > 0 {
		accountName = fs.Arg(0)
	}
	if accountName == "" {
		return usagef("%s", accountAddUsage)
	}
	if *provider == "" {
		return usagef("--provider 必填，例如 --provider deepseek")
	}

	// --base-url 与 provider add/set 走同一份校验（store.NormalizeBaseURL，见 DESIGN 5.8）：
	// 只有 scheme 没有主机名、或压根没写 scheme 的地址当场报成用法错误、退出码 2，
	// 不落到库里再去发无效请求。空值仍是「按厂商默认」。
	if flagWasSet(fs, "base-url") && strings.TrimSpace(*baseURL) != "" {
		norm, err := checkBaseURL(*baseURL, accountAddUsage)
		if err != nil {
			return err
		}
		*baseURL = norm
	}

	keyValue := *key
	if keyValue == "" {
		keyValue = envKey()
	}
	if keyValue == "-" || keyValue == "--" {
		v, err := readKeyFromStdin()
		if err != nil {
			return err
		}
		keyValue = v
	}
	if keyValue == "" {
		return usagef("缺少凭据：--key <key>、--key - 从标准输入读，或环境变量 SWITCHBOARD_KEY")
	}

	if *protocol != "" && !isWireProtocol(*protocol) {
		return usagef("不支持的协议 %q，可用 anthropic、openai-chat、openai-responses", *protocol)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 厂商的默认地址与默认协议来自厂商表，不在代码里硬编码（见 DESIGN 5.8）。
	proto := *protocol
	base := *baseURL
	defs, err := lookupProvider(a.st, *provider)
	if err != nil {
		return err
	}
	if defs.Found {
		if base == "" {
			base = defs.BaseURL
		}
		if proto == "" {
			proto = defs.Protocol
		}
	}
	// 厂商行在但一条路由都没声明时 store 算不出默认协议：显式给了 --base-url
	// 就按自建地址加账号，协议回退 openai-chat（见 DESIGN 5.8）；否则报错让人先声明路由。
	if proto == "" && defs.NoRoutes {
		if *baseURL == "" {
			return fmt.Errorf("厂商 %q 在厂商表里但一条路由都没声明，算不出默认协议（地址 %s）；先运行 switchboard provider set %s --openai /v1/chat/completions（或 --anthropic / --responses）声明路由，或给本命令加 --protocol <协议> 指定，或给 --base-url 按自建地址加账号",
				*provider, defs.BaseURL, *provider)
		}
		proto = gateway.ProtocolChat
	}
	if proto == "" {
		proto = gateway.ProtocolChat
	}
	if base == "" {
		return unknownProviderError(a.st, *provider)
	}

	if _, err := a.st.GetAccount(accountName); err == nil {
		return fmt.Errorf("账号 %q 已存在，换个名称或先运行 switchboard account rm %s", accountName, accountName)
	}

	cipher, err := secret.Encrypt(a.key, []byte(keyValue))
	if err != nil {
		return err
	}

	acc := store.Account{
		Name:      accountName,
		Provider:  *provider,
		Protocol:  proto,
		BaseURL:   base,
		KeyCipher: cipher,
		Models:    splitModels(*models),
		Priority:  *priority,
		Enabled:   true,
	}
	if *priceIn > 0 {
		v := *priceIn
		acc.PriceIn = &v
	}
	if *priceOut > 0 {
		v := *priceOut
		acc.PriceOut = &v
	}
	if err := a.st.CreateAccount(&acc); err != nil {
		return fmt.Errorf("新增账号失败: %w", err)
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"name":     acc.Name,
			"provider": acc.Provider,
			"protocol": acc.Protocol,
			"base_url": acc.BaseURL,
			"models":   acc.Models,
			"key":      secret.Mask(keyValue),
		})
	}
	fmt.Fprintf(e.Stdout, "已添加 %s（%s，%s，凭据 %s）\n", acc.Name, acc.Provider, acc.Protocol, secret.Mask(keyValue))
	return nil
}

func envKey() string {
	for _, name := range []string{"SWITCHBOARD_KEY", "SWITCHBOARD_API_KEY"} {
		if v := lookupEnv(name); v != "" {
			return v
		}
	}
	return ""
}

// readKeyFromStdin 从标准输入读凭据，去掉首尾空白。
// 直接在终端上运行且没有管道时报错，避免命令挂起等输入。
func readKeyFromStdin() (string, error) {
	info, statErr := os.Stdin.Stat()
	if statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
		return "", usagef("--key - 需要从管道或文件读入，例如 switchboard account add x --provider p --key - < key.txt")
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<10))
	if err != nil {
		return "", fmt.Errorf("读取标准输入失败: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", usagef("标准输入是空的，没有读到凭据")
	}
	return s, nil
}

func cmdAccountLs(e *Env, args []string) error {
	fs := newFlagSet(e, "account ls", "列出账号")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	accounts, err := a.st.ListAccounts()
	if err != nil {
		return err
	}
	modelCounts, err := a.st.AllAccountModels()
	if err != nil {
		return err
	}
	counts := countModels(modelCounts)
	now := time.Now()

	if e.JSON {
		out := make([]map[string]any, 0, len(accounts))
		for _, acc := range accounts {
			c := counts[acc.ID]
			out = append(out, map[string]any{
				"name":           acc.Name,
				"provider":       acc.Provider,
				"protocol":       acc.Protocol,
				"base_url":       acc.BaseURL,
				"models":         acc.Models,
				"priority":       acc.Priority,
				"enabled":        acc.Enabled,
				"status":         accountStatus(acc, now),
				"cooldown_until": acc.CooldownUntil,
				"last_error":     acc.LastError,
				// 与文本输出的「N 可用 / M 同步」同一组计数。
				"models_usable": c.Usable,
				"models_synced": c.Total,
				// 默认协议在前，其余是探活发现到的、与默认不同的协议。
				"protocols": allProtocols(acc.Protocol, c.Protocols),
			})
		}
		return e.printJSON(out)
	}

	if len(accounts) == 0 {
		fmt.Fprintln(e.Stdout, "还没有账号。运行 switchboard account add <名称> --provider <厂商> --key <key>")
		return nil
	}

	rows := make([][]string, 0, len(accounts))
	for _, acc := range accounts {
		rows = append(rows, []string{
			acc.Name,
			acc.Provider,
			protocolCell(acc.Protocol, counts[acc.ID].Protocols),
			modelsCell(acc.Models, counts[acc.ID]),
			fmt.Sprintf("%d", acc.Priority),
			accountStatus(acc, now),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "厂商", "协议", "模型", "优先级", "状态"}, rows))
	return nil
}

// modelCount 是一个账号在 account_models 里的汇总。
// Protocols 是模型行上发现过的协议，升序去重，含与账号默认协议相同的那些。
type modelCount struct {
	Total     int
	Usable    int
	Protocols []string
}

func countModels(rows []store.AccountModel) map[int64]modelCount {
	out := map[int64]modelCount{}
	seen := map[int64]map[string]bool{}
	for _, r := range rows {
		c := out[r.AccountID]
		c.Total++
		if r.Status == store.ModelUsable {
			c.Usable++
		}
		if r.Protocol != "" {
			if seen[r.AccountID] == nil {
				seen[r.AccountID] = map[string]bool{}
			}
			if !seen[r.AccountID][r.Protocol] {
				seen[r.AccountID][r.Protocol] = true
				c.Protocols = append(c.Protocols, r.Protocol)
			}
		}
		out[r.AccountID] = c
	}
	for id, c := range out {
		sort.Strings(c.Protocols)
		out[id] = c
	}
	return out
}

// allProtocols 把默认协议排到前面，后面跟模型行上发现到的其它协议，去重后升序。
// 默认协议自己不在发现结果里时也要出现：它是账号当前的出站协议。
func allProtocols(accountProtocol string, discovered []string) []string {
	out := []string{accountProtocol}
	for _, p := range discovered {
		if p != accountProtocol {
			out = append(out, p)
		}
	}
	return out
}

// protocolCell 是 account ls 的协议列：只有默认协议时就是它本身，
// 存在与默认不同的已发现协议时标出「另 N 种」，一眼能看出这个账号不止走一种协议。
func protocolCell(accountProtocol string, discovered []string) string {
	all := allProtocols(accountProtocol, discovered)
	if len(all) == 1 {
		return accountProtocol
	}
	return fmt.Sprintf("%s + 另 %d 种", accountProtocol, len(all)-1)
}

func modelsCell(models []string, c modelCount) string {
	if c.Total > 0 {
		return fmt.Sprintf("%d 可用 / %d 同步", c.Usable, c.Total)
	}
	if len(models) == 0 {
		return "(不限)"
	}
	if len(models) == 1 {
		return models[0]
	}
	return fmt.Sprintf("%s 等 %d 个", models[0], len(models))
}

func accountStatus(acc store.Account, now time.Time) string {
	if !acc.Enabled {
		return "停用"
	}
	if acc.CooldownUntil > now.Unix() {
		return "冷却至 " + time.Unix(acc.CooldownUntil, 0).Format("15:04:05")
	}
	return "正常"
}

func cmdAccountRm(e *Env, args []string) error {
	fs := newFlagSet(e, "account rm", "删除账号")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：switchboard account rm <名称>")
	}
	name := fs.Arg(0)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.DeleteAccount(name); err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(map[string]string{"removed": name})
	}
	fmt.Fprintf(e.Stdout, "已删除 %s，历史请求记录保留\n", name)
	return nil
}

func cmdAccountTest(e *Env, args []string) error {
	fs := newFlagSet(e, "account test", "用最小请求探活一个账号")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：switchboard account test <名称>")
	}
	name := fs.Arg(0)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	acc, err := a.st.GetAccount(name)
	if err != nil {
		return fmt.Errorf("账号 %q 不存在", name)
	}

	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: e.gatewayLogf()})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := gs.Probe(ctx, acc)
	if err != nil {
		return err
	}

	if e.JSON {
		if err := e.printJSON(map[string]any{
			"account": res.Account,
			"status":  res.Status,
			"ok":      res.OK,
			"latency": res.Latency.String(),
			"message": res.Message,
		}); err != nil {
			return err
		}
	} else {
		state := "失败"
		if res.OK {
			state = "正常"
		}
		fmt.Fprintf(e.Stdout, "%s %s %d %s %s\n", res.Account, state, res.Status, res.Latency.Round(time.Millisecond), res.Message)
	}

	if !res.OK {
		return fmt.Errorf("账号 %s 探活失败（%d）：%s", res.Account, res.Status, res.Message)
	}
	return nil
}

// modelSubs 是 model 一组的子命令清单。
var modelSubs = []groupItem{
	{"ls", "列出网关可提供的模型与该模型下的账号状态"},
	{"sync", "从上游拉取模型列表，写入 account_models"},
	{"probe", "逐个模型发 max_tokens=1 的请求探活，把可用模型写进账号"},
}

// cmdModel 是 model 这一组子命令的入口。
func cmdModel(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "model", "模型同步与探活", modelSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：ls | sync | probe\n用 switchboard model --help 查看子命令")
	}
	switch args[0] {
	case "ls":
		return cmdModelLs(e, args[1:])
	case "sync":
		return cmdModelSync(e, args[1:])
	case "probe":
		return cmdModelProbe(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | sync | probe\n用 switchboard model --help 查看子命令", args[0])
	}
}

func cmdModelLs(e *Env, args []string) error {
	fs := newFlagSet(e, "model ls", "列出网关可提供的模型")
	provider := fs.String("provider", "", "只看这个厂商的账号")
	accountName := fs.String("account", "", "只看这个账号")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	accounts, err := a.st.ListAccounts()
	if err != nil {
		return err
	}
	if *provider != "" || *accountName != "" {
		accounts, err = selectAccounts(a.st, *provider, *accountName)
		if err != nil {
			return err
		}
	}
	byID := make(map[int64]store.Account, len(accounts))
	for _, acc := range accounts {
		byID[acc.ID] = acc
	}

	type entry struct {
		model    string
		accounts []modelAccountJSON
		priceIn  *float64
		priceOut *float64
	}
	byModel := map[string]*entry{}
	var order []string

	add := func(model, accountName, status, protocol string, acc store.Account) {
		ent, ok := byModel[model]
		if !ok {
			ent = &entry{model: model, priceIn: acc.PriceIn, priceOut: acc.PriceOut}
			byModel[model] = ent
			order = append(order, model)
		}
		ent.accounts = append(ent.accounts, modelAccountJSON{Name: accountName, Status: status, Protocol: protocol})
	}

	modelRows, err := a.st.AllAccountModels()
	if err != nil {
		return err
	}
	// 有效协议只问 store：行不存在或协议列为空时 EffectiveProtocol 已经回退到账号协议。
	for _, r := range modelRows {
		acc, ok := byID[r.AccountID]
		if !ok {
			continue
		}
		proto, err := a.st.EffectiveProtocol(acc.ID, acc.Protocol, r.Model)
		if err != nil {
			return err
		}
		add(r.Model, acc.Name, r.Status, proto, acc)
	}
	if len(modelRows) == 0 {
		for _, acc := range accounts {
			for _, m := range acc.Models {
				proto, err := a.st.EffectiveProtocol(acc.ID, acc.Protocol, m)
				if err != nil {
					return err
				}
				add(m, acc.Name, "manual", proto, acc)
			}
		}
	}
	sort.Strings(order)

	if e.JSON {
		out := make([]map[string]any, 0, len(order))
		for _, m := range order {
			ent := byModel[m]
			out = append(out, map[string]any{
				"model":     ent.model,
				"accounts":  ent.accounts,
				"price_in":  ent.priceIn,
				"price_out": ent.priceOut,
			})
		}
		return e.printJSON(out)
	}

	if len(order) == 0 {
		fmt.Fprintln(e.Stdout, "还没有可用模型。先 switchboard account add，再 switchboard account probe <名称>")
		return nil
	}

	rows := make([][]string, 0, len(order))
	for _, m := range order {
		ent := byModel[m]
		names := make([]string, 0, len(ent.accounts))
		for _, ae := range ent.accounts {
			names = append(names, fmt.Sprintf("%s(%s)", ae.Name, modelStatusText(ae.Status)))
		}
		price := "未定价"
		if ent.priceIn != nil || ent.priceOut != nil {
			price = fmt.Sprintf("%s / %s", priceText(ent.priceIn), priceText(ent.priceOut))
		}
		rows = append(rows, []string{ent.model, strings.Join(names, "、"), modelProtocolCell(ent.accounts), price})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"模型", "账号", "协议", "输入价 / 输出价（每百万 token）"}, rows))
	return nil
}

// modelAccountJSON 是 model ls --json 里某个模型下的一个账号。
// Protocol 是该账号在这个模型上的有效出站协议。
type modelAccountJSON struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Protocol string `json:"protocol"`
}

// modelProtocolCell 是一个模型下各账号的有效协议：只有一个账号时直接写协议，
// 多个账号时逐个写上名字，免得同一格里分不清哪个账号走哪种协议。
func modelProtocolCell(entries []modelAccountJSON) string {
	if len(entries) == 0 {
		return "-"
	}
	if len(entries) == 1 {
		return entries[0].Protocol
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Name+" "+e.Protocol)
	}
	return strings.Join(parts, "、")
}

// modelStatusText 把状态码换成人话。
func modelStatusText(status string) string {
	switch status {
	case store.ModelUsable:
		return "可用"
	case store.ModelSynced:
		return "待探活"
	case store.ModelForbidden:
		return "无权"
	case store.ModelNotFound:
		return "不存在"
	case store.ModelAuthFailed:
		return "凭据被拒"
	case store.ModelError:
		return "失败"
	case "manual":
		return "手填"
	default:
		return status
	}
}

// cmdModelSync 从上游拉模型列表。按 --provider / --account 选账号，不给就全选。
func cmdModelSync(e *Env, args []string) error {
	fs := newFlagSet(e, "model sync", "从上游拉取模型列表")
	provider := fs.String("provider", "", "只同步这个厂商的账号")
	accountName := fs.String("account", "", "只同步这个账号")
	endpoint := fs.String("endpoint", "", "临时覆盖模型列表路径，只影响这一次调用：相对账号 base_url 的完整路径，例如 /v1/models（写法由程序规整；整条地址写在账号的 base_url，不要写在这里）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 取值原样往下走：写法（前导斜杠、重复斜杠、末尾斜杠）由 store.NormalizePath 规整，
	// cli 只判两种用法错误：「规整之后为空」——给了参数却没给路径，去掉这个参数再试；
	// 与「写成了整条地址」——完整地址属于账号的 base_url，写进路径会被规整成 /https:/…（见 DESIGN 4、5.8）。
	if flagWasSet(fs, "endpoint") {
		if !pathGiven(*endpoint) {
			return usagef("--endpoint %q 里没有路径：它是相对该厂商 base_url 的完整路径，例如 switchboard model sync --endpoint /v1/models；不想临时覆盖就整个去掉这个参数", *endpoint)
		}
		if hasFullAddress(*endpoint) {
			return pathGotFullAddress("endpoint", *endpoint,
				"账号的 base_url（account add --base-url <地址>，或 provider set <名称> --base-url <地址>）",
				"switchboard model sync --account ds --endpoint /v1/models",
				"用法：switchboard model sync [--provider 厂商] [--account 账号] [--endpoint <路径>]")
		}
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	accounts, err := selectAccounts(a.st, *provider, *accountName)
	if err != nil {
		return err
	}
	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: e.gatewayLogf()})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	type row struct {
		Account  string `json:"account"`
		Provider string `json:"provider"`
		Models   int    `json:"models"`
		Added    int    `json:"added"`
		Error    string `json:"error,omitempty"`
	}

	rows := make([]row, 0, len(accounts))
	failed := 0
	for _, acc := range accounts {
		r := row{Account: acc.Name, Provider: acc.Provider}
		models, err := gs.SyncModelsWithEndpoint(ctx, acc, strings.TrimSpace(*endpoint))
		if err != nil {
			r.Error = err.Error()
			failed++
		} else if added, err := a.st.ReplaceModels(acc.ID, models); err != nil {
			return err
		} else {
			r.Models, r.Added = len(models), added
		}
		rows = append(rows, r)
	}

	if e.JSON {
		if err := e.printJSON(rows); err != nil {
			return err
		}
	} else {
		table := make([][]string, 0, len(rows))
		for _, r := range rows {
			table = append(table, []string{
				r.Account, r.Provider,
				fmt.Sprintf("%d", r.Models), fmt.Sprintf("%d", r.Added),
				truncate(r.Error, 48),
			})
		}
		fmt.Fprint(e.Stdout, renderTable([]string{"账号", "厂商", "模型", "新增", "备注"}, table))
		fmt.Fprintln(e.Stdout, "下一步：switchboard model probe（每个模型发一条 max_tokens=1 的请求）")
	}

	if failed > 0 {
		return fmt.Errorf("%d 个账号同步失败", failed)
	}
	return nil
}

// cmdModelProbe 逐个模型探活。按 --provider / --account 选账号，不给就全选。
func cmdModelProbe(e *Env, args []string) error {
	fs := newFlagSet(e, "model probe", "逐个模型探活，把可用模型写进账号")
	provider := fs.String("provider", "", "只处理这个厂商的账号")
	accountName := fs.String("account", "", "只处理这个账号")
	syncFirst := fs.Bool("sync", false, "探活前先拉一次上游模型列表")
	only := fs.String("model", "", "只探这一个模型")
	limit := fs.Int("limit", 0, "每个账号最多探多少个待探模型，0 表示全部")
	concurrency := fs.Int("concurrency", 4, "并发数")
	maxAge := fs.Duration("max-age", 24*time.Hour, "已探过的结论新鲜多久，超期重探；0 与 --refresh 等价")
	refresh := fs.Bool("refresh", false, "忽略新鲜度，全部重探")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	accounts, err := selectAccounts(a.st, *provider, *accountName)
	if err != nil {
		return err
	}
	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: e.gatewayLogf()})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	type hit struct {
		Account    string `json:"account"`
		Model      string `json:"model"`
		Status     string `json:"status"`
		HTTPStatus int    `json:"http_status"`
		LatencyMS  int64  `json:"latency_ms"`
		Message    string `json:"message"`
	}

	// 跳过的不进 hits，--json 只给被探的模型；空结果是 [] 而不是 null。
	hits := make([]hit, 0, len(accounts))
	usableByAccount := map[string]int{}
	authFailed := ""
	syncFailed := 0
	// 零可用时的错误文案要能分清「全被跳过」与「探了都不行」，
	// 所以整轮累加探活数与跳过数，不只是当前账号的。
	totalProbed, totalSkipped := 0, 0

	for _, acc := range accounts {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rows, err := a.st.AccountModels(acc.ID)
		if err != nil {
			return err
		}
		if *syncFirst || (len(rows) == 0 && *only == "") {
			models, err := gs.SyncModels(ctx, acc)
			if err != nil {
				syncFailed++
				if !e.JSON {
					fmt.Fprintf(e.Stdout, "%s：同步失败 %v\n", acc.Name, err)
				}
				continue
			}
			if _, err := a.st.ReplaceModels(acc.ID, models); err != nil {
				return err
			}
			if !e.JSON {
				fmt.Fprintf(e.Stdout, "%s：已同步 %d 个模型\n", acc.Name, len(models))
			}
			if rows, err = a.st.AccountModels(acc.ID); err != nil {
				return err
			}
		}

		targets := make([]string, 0, len(rows))
		freshEnabled := !*refresh && *maxAge > 0
		now := time.Now()
		skipped := 0
		for _, c := range probeCandidates(rows, *only) {
			if freshEnabled && account.SettledFresh(c.status, c.checkedAt, now, *maxAge) {
				skipped++
				continue
			}
			targets = append(targets, c.model)
		}
		if len(targets) == 0 && skipped == 0 {
			if !e.JSON {
				fmt.Fprintf(e.Stdout, "%s：没有可探的模型，先运行 switchboard model sync --account %s\n", acc.Name, acc.Name)
			}
			continue
		}
		// --limit 截断的是跳过之后剩下的待探模型，跳过的模型不占额度。
		if *limit > 0 && len(targets) > *limit {
			targets = targets[:*limit]
		}

		var results []gateway.ModelProbe
		if len(targets) > 0 {
			if results, err = probeModels(ctx, gs, a.st, acc, targets, *concurrency); err != nil {
				return err
			}
		}

		var counts = map[string]int{}
		for _, res := range results {
			counts[res.Status]++
			hits = append(hits, hit{
				Account:    acc.Name,
				Model:      res.Model,
				Status:     res.Status,
				HTTPStatus: res.HTTPStatus,
				LatencyMS:  res.Latency.Milliseconds(),
				Message:    res.Message,
			})
		}
		totalProbed += len(results)
		totalSkipped += skipped

		if counts[store.ModelAuthFailed] > 0 {
			// 凭据不行了，后续账号接着探只会重复同一条错误：停下，
			// 但已经探出来的行仍要打印，人得看得见是哪一步卡住的。
			authFailed = acc.Name
			break
		}

		// 选号列表取库里全部 usable，不是本次探出来的那几个：
		// 用 --limit 或 --model 只探一部分时，之前探出来的可用模型不该被丢掉。
		// 待探模型被全部跳过时也走这里：结论都还在新鲜期内，不该因为「这次什么都没探」报错。
		after, err := a.st.AccountModels(acc.ID)
		if err != nil {
			return err
		}
		usable := make([]string, 0, len(after))
		for _, r := range after {
			if r.Status == store.ModelUsable {
				usable = append(usable, r.Model)
			}
		}
		if len(usable) == 0 {
			if !e.JSON {
				fmt.Fprintf(e.Stdout, "%s：本次探活 %d 个，跳过 %d 个，库里无一可用，模型列表保持原样\n", acc.Name, len(results), skipped)
			}
			continue
		}
		if err := a.st.SetModels(acc.ID, usable); err != nil {
			return err
		}
		usableByAccount[acc.Name] = len(usable)
		if !e.JSON {
			fmt.Fprintf(e.Stdout, "%s：本次探活 %d 个，跳过 %d 个，可用 %d，无权 %d，不存在 %d，失败 %d；选号列表共 %d 个\n",
				acc.Name, len(results), skipped, counts[store.ModelUsable], counts[store.ModelForbidden],
				counts[store.ModelNotFound], counts[store.ModelError], len(usable))
		}
	}

	if e.JSON {
		if err := e.printJSON(hits); err != nil {
			return err
		}
	} else {
		table := make([][]string, 0, len(hits))
		for _, h := range hits {
			table = append(table, []string{
				h.Account,
				h.Model,
				modelStatusText(h.Status),
				fmt.Sprintf("%d", h.HTTPStatus),
				fmt.Sprintf("%dms", h.LatencyMS),
				truncate(h.Message, 44),
			})
		}
		if len(table) > 0 {
			fmt.Fprint(e.Stdout, renderTable([]string{"账号", "模型", "状态", "HTTP", "耗时", "备注"}, table))
		}
	}

	// 先出结果再判定，保证 --json 与文本两条路径的退出码与写库行为一致。
	if authFailed != "" {
		return fmt.Errorf("账号 %s 的凭据被上游拒绝，账号已停用，其余结论不作数", authFailed)
	}
	if len(usableByAccount) == 0 {
		return fmt.Errorf("%s，账号模型列表未改动", zeroUsableReason(totalProbed, totalSkipped, syncFailed))
	}
	if syncFailed > 0 {
		return fmt.Errorf("%d 个账号同步失败", syncFailed)
	}
	return nil
}

// zeroUsableReason 说清零可用的原因：是跳过、是真探了都不行，还是连模型列表都没拉到。
// 三种情况下一步差很多：强制重探、换凭据或换模型、先修同步。
func zeroUsableReason(probed, skipped, syncFailed int) string {
	if probed == 0 && skipped == 0 {
		if syncFailed > 0 {
			return fmt.Sprintf("没有账号探出可用模型：%d 个账号同步失败，一个模型都没拉到", syncFailed)
		}
		return "没有可探的模型，先运行 switchboard model sync --account <名称>"
	}
	msg := fmt.Sprintf("没有账号探出可用模型：本次探活 %d 个、跳过 %d 个", probed, skipped)
	if skipped > 0 {
		msg += "（跳过的结论仍在 --max-age 窗口内，用 --refresh 强制重探）"
	}
	if syncFailed > 0 {
		msg += fmt.Sprintf("；另有 %d 个账号同步失败", syncFailed)
	}
	return msg
}

// probeCandidate 是一个待判定的模型。status 与 checkedAt 取库里已有结论，
// 没有对应行时为零值，即「从未探过」。
type probeCandidate struct {
	model     string
	status    string
	checkedAt int64
}

// probeCandidates 列出这一轮要考虑的模型。给了 --model 就只算那一个（库里没行也照探），
// 否则按库里的模型行。
func probeCandidates(rows []store.AccountModel, only string) []probeCandidate {
	if only != "" {
		c := probeCandidate{model: only}
		for _, r := range rows {
			if r.Model == only {
				c.status, c.checkedAt = r.Status, r.CheckedAt
				break
			}
		}
		return []probeCandidate{c}
	}
	out := make([]probeCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, probeCandidate{model: r.Model, status: r.Status, checkedAt: r.CheckedAt})
	}
	return out
}

// mergeProbeError 把探活返回的错误并进结果。结论已定时只把原因补进备注，不改写状态：
// 401 之后停用账号失败的错误若改写成 error，auth 就被吞掉，调用方不再「碰到 401 就停下」，
// 账号里也会留下一个与实际情况相反的结论。只有还没定结论的错误（解密失败、构造请求失败）
// 才记成 error。
func mergeProbeError(model string, res gateway.ModelProbe, err error) gateway.ModelProbe {
	if res.Status == "" {
		return gateway.ModelProbe{Model: model, Status: store.ModelError, Message: err.Error()}
	}
	if res.Message == "" {
		res.Message = err.Error()
		return res
	}
	res.Message += "；" + err.Error()
	return res
}

// probeModels 用固定大小的 worker pool 并发探活一个账号的模型。
func probeModels(ctx context.Context, gs *gateway.Server, st *store.Store,
	acc store.Account, targets []string, concurrency int) ([]gateway.ModelProbe, error) {

	workers := concurrency
	if workers < 1 {
		workers = 1
	}
	// 并发数不超过待探模型数，避免空转的 goroutine。
	if workers > len(targets) {
		workers = len(targets)
	}

	results := make([]gateway.ModelProbe, len(targets))
	writeErrs := make([]error, len(targets))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, model := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, model string) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, pcancel := context.WithTimeout(ctx, 60*time.Second)
			defer pcancel()
			res, err := gs.ProbeModel(pctx, acc, model)
			if err != nil {
				res = mergeProbeError(model, res, err)
			}
			results[i] = res
			// 每个 goroutine 只写自己那一行，底层连接串行，不共享事务。
			writeErrs[i] = st.SetModelStatus(acc.ID, model, res.Status, res.HTTPStatus, res.Message)
		}(i, model)
	}
	wg.Wait()

	for _, err := range writeErrs {
		if err != nil {
			return results, fmt.Errorf("探活结果写库失败: %w", err)
		}
	}
	return results, nil
}

// selectAccounts 按厂商与账号名筛选待处理账号，两个都不给时返回全部。
func selectAccounts(st *store.Store, provider, name string) ([]store.Account, error) {
	accounts, err := st.ListAccounts()
	if err != nil {
		return nil, err
	}

	if name != "" {
		for _, acc := range accounts {
			if acc.Name != name {
				continue
			}
			if provider != "" && acc.Provider != provider {
				return nil, fmt.Errorf("账号 %s 的厂商是 %s，与 --provider %s 不符", name, acc.Provider, provider)
			}
			return []store.Account{acc}, nil
		}
		return nil, fmt.Errorf("账号 %q 不存在", name)
	}

	if provider == "" {
		if len(accounts) == 0 {
			return nil, fmt.Errorf("还没有账号，先运行 switchboard account add")
		}
		return accounts, nil
	}

	var out []store.Account
	for _, acc := range accounts {
		if acc.Provider == provider {
			out = append(out, acc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有厂商为 %q 的账号", provider)
	}
	return out, nil
}

func priceText(v *float64) string {
	if v == nil {
		return "?"
	}
	return fmt.Sprintf("%.2f", *v)
}

func splitModels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
