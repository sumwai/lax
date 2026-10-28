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

	"switchboard/internal/gateway"
	"switchboard/internal/secret"
	"switchboard/internal/store"
)

type providerDefault struct {
	baseURL  string
	protocol string
}

// providers 记录已知厂商的默认地址与线协议。表中没有的必须显式给 --base-url。
var providers = map[string]providerDefault{
	"anthropic":  {baseURL: "https://api.anthropic.com", protocol: gateway.ProtocolAnthropic},
	"openai":     {baseURL: "https://api.openai.com", protocol: gateway.ProtocolChat},
	"deepseek":   {baseURL: "https://api.deepseek.com", protocol: gateway.ProtocolChat},
	"moonshot":   {baseURL: "https://api.moonshot.cn", protocol: gateway.ProtocolChat},
	"kimi":       {baseURL: "https://api.moonshot.cn", protocol: gateway.ProtocolChat},
	"openrouter": {baseURL: "https://openrouter.ai/api", protocol: gateway.ProtocolChat},
	"ollama":     {baseURL: "http://127.0.0.1:11434", protocol: gateway.ProtocolChat},
	// commandcode 上 claude-* 只接受 /messages，加账号时给 --protocol anthropic。
	"commandcode": {
		baseURL:  "https://api.commandcode.ai/provider/v1",
		protocol: gateway.ProtocolChat,
	},
}

func cmdAccount(e *Env, args []string) error {
	if len(args) == 0 {
		return usagef("需要子命令：add | ls | rm | test | enable | disable")
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
		return usagef("未知子命令 %q，可用：add | ls | rm | test | enable | disable", args[0])
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

func cmdAccountAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "account add", "新增一个上游账号")
	provider := fs.String("provider", "", "上游厂商标识，如 deepseek")
	key := fs.String("key", "", "上游 API key；给 - 从标准输入读，留空则读环境变量 SWITCHBOARD_KEY")
	name := fs.String("name", "", "账号名称，也可以用位置参数给出")
	protocol := fs.String("protocol", "", "上游线协议：openai-chat | anthropic，留空按厂商默认")
	baseURL := fs.String("base-url", "", "上游地址，留空按厂商默认")
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
		return usagef("用法：switchboard account add <名称> --provider <厂商> --key <key>")
	}
	if *provider == "" {
		return usagef("--provider 必填，例如 --provider deepseek")
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

	proto := *protocol
	base := *baseURL
	if def, ok := providers[*provider]; ok {
		if proto == "" {
			proto = def.protocol
		}
		if base == "" {
			base = def.baseURL
		}
	}
	if proto == "" {
		proto = gateway.ProtocolChat
	}
	if base == "" {
		return fmt.Errorf("厂商 %q 不在已知列表里，需要显式给 --base-url", *provider)
	}
	if proto != gateway.ProtocolChat && proto != gateway.ProtocolAnthropic {
		return usagef("不支持的协议 %q，可用 openai-chat、anthropic", proto)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

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
			acc.Protocol,
			modelsCell(acc.Models, counts[acc.ID]),
			fmt.Sprintf("%d", acc.Priority),
			accountStatus(acc, now),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "厂商", "协议", "模型", "优先级", "状态"}, rows))
	return nil
}

// modelCount 是一个账号在 account_models 里的汇总。
type modelCount struct {
	Total  int
	Usable int
}

func countModels(rows []store.AccountModel) map[int64]modelCount {
	out := map[int64]modelCount{}
	for _, r := range rows {
		c := out[r.AccountID]
		c.Total++
		if r.Status == store.ModelUsable {
			c.Usable++
		}
		out[r.AccountID] = c
	}
	return out
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

	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key})
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

// cmdModel 是 model 这一组子命令的入口。
func cmdModel(e *Env, args []string) error {
	if len(args) == 0 {
		return usagef("需要子命令：ls | sync | probe")
	}
	switch args[0] {
	case "ls":
		return cmdModelLs(e, args[1:])
	case "sync":
		return cmdModelSync(e, args[1:])
	case "probe":
		return cmdModelProbe(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | sync | probe", args[0])
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

	type accEntry struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	type entry struct {
		model    string
		accounts []accEntry
		priceIn  *float64
		priceOut *float64
	}
	byModel := map[string]*entry{}
	var order []string

	add := func(model, accountName, status string, acc store.Account) {
		ent, ok := byModel[model]
		if !ok {
			ent = &entry{model: model, priceIn: acc.PriceIn, priceOut: acc.PriceOut}
			byModel[model] = ent
			order = append(order, model)
		}
		ent.accounts = append(ent.accounts, accEntry{Name: accountName, Status: status})
	}

	modelRows, err := a.st.AllAccountModels()
	if err != nil {
		return err
	}
	for _, r := range modelRows {
		acc, ok := byID[r.AccountID]
		if !ok {
			continue
		}
		add(r.Model, acc.Name, r.Status, acc)
	}
	if len(modelRows) == 0 {
		for _, acc := range accounts {
			for _, m := range acc.Models {
				add(m, acc.Name, "manual", acc)
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
		rows = append(rows, []string{ent.model, strings.Join(names, "、"), price})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"模型", "账号", "输入价 / 输出价（每百万 token）"}, rows))
	return nil
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
	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key})
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
		models, err := gs.SyncModels(ctx, acc)
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
	limit := fs.Int("limit", 0, "每个账号最多探多少个模型，0 表示全部")
	concurrency := fs.Int("concurrency", 4, "并发数")
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
	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key})
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

	var hits []hit
	usableByAccount := map[string]int{}
	authFailed := ""
	syncFailed := 0

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
		if *only != "" {
			targets = append(targets, *only)
		} else {
			for _, r := range rows {
				targets = append(targets, r.Model)
			}
		}
		if len(targets) == 0 {
			if !e.JSON {
				fmt.Fprintf(e.Stdout, "%s：没有可探的模型，先运行 switchboard model sync --account %s\n", acc.Name, acc.Name)
			}
			continue
		}
		if *limit > 0 && len(targets) > *limit {
			targets = targets[:*limit]
		}

		results, err := probeModels(ctx, gs, a.st, acc, targets, *concurrency)
		if err != nil {
			return err
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

		if counts[store.ModelAuthFailed] > 0 {
			authFailed = acc.Name
			continue
		}

		// 选号列表取库里全部 usable，不是本次探出来的那几个：
		// 用 --limit 或 --model 只探一部分时，之前探出来的可用模型不该被丢掉。
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
				fmt.Fprintf(e.Stdout, "%s：本次探活 %d 个，库里无一可用，模型列表保持原样\n", acc.Name, len(results))
			}
			continue
		}
		if err := a.st.SetModels(acc.ID, usable); err != nil {
			return err
		}
		usableByAccount[acc.Name] = len(usable)
		if !e.JSON {
			fmt.Fprintf(e.Stdout, "%s：本次探活 %d 个，可用 %d，无权 %d，不存在 %d，失败 %d；选号列表共 %d 个\n",
				acc.Name, len(results), counts[store.ModelUsable], counts[store.ModelForbidden],
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
		return fmt.Errorf("没有账号探出可用模型，账号模型列表未改动")
	}
	if syncFailed > 0 {
		return fmt.Errorf("%d 个账号同步失败", syncFailed)
	}
	return nil
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
				res = gateway.ModelProbe{Model: model, Status: store.ModelError, Message: err.Error()}
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
