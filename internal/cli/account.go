package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
		if err := a.st.SetCooldown(acc.ID, 0, ""); err != nil {
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
	now := time.Now()

	if e.JSON {
		out := make([]map[string]any, 0, len(accounts))
		for _, acc := range accounts {
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
			modelsCell(acc.Models),
			fmt.Sprintf("%d", acc.Priority),
			accountStatus(acc, now),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "厂商", "协议", "模型", "优先级", "状态"}, rows))
	return nil
}

func modelsCell(models []string) string {
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

func cmdModel(e *Env, args []string) error {
	if len(args) == 0 || args[0] != "ls" {
		return usagef("需要子命令：ls")
	}
	fs := newFlagSet(e, "model ls", "列出网关可提供的模型")
	if err := fs.Parse(args[1:]); err != nil {
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

	type entry struct {
		model    string
		accounts []string
		priceIn  *float64
		priceOut *float64
	}
	order := make([]string, 0)
	byModel := map[string]*entry{}
	for _, acc := range accounts {
		for _, m := range acc.Models {
			e, ok := byModel[m]
			if !ok {
				e = &entry{model: m, priceIn: acc.PriceIn, priceOut: acc.PriceOut}
				byModel[m] = e
				order = append(order, m)
			}
			e.accounts = append(e.accounts, acc.Name)
		}
	}

	if e.JSON {
		out := make([]map[string]any, 0, len(order))
		for _, m := range order {
			ent := byModel[m]
			out = append(out, map[string]any{
				"model":    ent.model,
				"accounts": ent.accounts,
			})
		}
		return e.printJSON(out)
	}

	if len(order) == 0 {
		fmt.Fprintln(e.Stdout, "还没有可用模型。先用 switchboard account add 加账号，并给出 --models")
		return nil
	}

	rows := make([][]string, 0, len(order))
	for _, m := range order {
		ent := byModel[m]
		price := "未定价"
		if ent.priceIn != nil || ent.priceOut != nil {
			price = fmt.Sprintf("%s / %s", priceText(ent.priceIn), priceText(ent.priceOut))
		}
		rows = append(rows, []string{ent.model, strings.Join(ent.accounts, "、"), price})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"模型", "账号", "输入价 / 输出价（每百万 token）"}, rows))
	return nil
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
