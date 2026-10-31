package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"lax/internal/store"
)

// provider header 三条子命令只做命令面：校验参数、调 store、出文本与 --json 两条输出。
// 表的形状、头名与值的校验、协议级覆盖厂商级的合并都在 store 与网关，这里不重算：
// cli 另写一份就会与转发时实际生效的规则不一致。

// providerHeaderSubs 是 provider header 一组的子命令清单，组级帮助与错误提示都读它。
var providerHeaderSubs = []groupItem{
	{"set", "声明或覆盖一家厂商的一条附加请求头"},
	{"unset", "删掉一条声明过的附加请求头"},
	{"ls", "列已声明的附加请求头（值脱敏）"},
}

// 三份用法行，所有用法错误都把它打出来。`--protocol` 不给就是厂商级：这家厂商的每条路由都带上它。
const (
	providerHeaderSetUsage   = "用法：lax provider header set <厂商> <头名> <值> [--protocol <协议>]"
	providerHeaderUnsetUsage = "用法：lax provider header unset <厂商> <头名> [--protocol <协议>]"
	providerHeaderLsUsage    = "用法：lax provider header ls [<厂商>]"
	providerHeaderUsageText  = "用法：\n" +
		"  lax provider header set <厂商> <头名> <值> [--protocol <协议>]\n" +
		"  lax provider header unset <厂商> <头名> [--protocol <协议>]\n" +
		"  lax provider header ls [<厂商>]"
)

// providerHeaderJSON 是 provider header ls 的一项，也是 set 的 --json 输出。
// Value 是脱敏后的值（secret.Mask，只露前四位），ValueMasked 恒为 true：
// 附加头的值可能是密钥（自定义鉴权头、会话 token），读输出的人不能以为手里拿到的是明文。
// Protocol 为空串表示厂商级。
type providerHeaderJSON struct {
	Provider    string `json:"provider"`
	Protocol    string `json:"protocol"`
	Name        string `json:"name"`
	Value       string `json:"value"`
	ValueMasked bool   `json:"value_masked"`
}

// providerHeaderItem 是一项声明连同它属于哪家厂商（ls 不带厂商时要把厂商名打在每一行上）。
type providerHeaderItem struct {
	provider string
	header   store.ProviderHeader
}

// toJSON 把一项声明换成 --json 的形状：值一律走 ProviderHeader.Masked。
func (it providerHeaderItem) toJSON() providerHeaderJSON {
	return providerHeaderJSON{
		Provider:    it.provider,
		Protocol:    it.header.Protocol,
		Name:        it.header.Name,
		Value:       it.header.Masked(),
		ValueMasked: true,
	}
}

// headerScopeLabel 是「整个厂商」与协议名在人读文案里的写法，空串表示厂商级。
func headerScopeLabel(protocol string) string {
	if protocol == "" {
		return "整个厂商"
	}
	return protocol
}

// headerScopeCell 是协议列：厂商级显示 -，与 provider ls 的空值写法一致。
func headerScopeCell(protocol string) string {
	if protocol == "" {
		return "-"
	}
	return protocol
}

// printProviderHeaderHelp 是 `provider header --help` 的输出：三条用法、一个示例，
// 以及三种占位符写法与「厂商级 vs 协议级、协议级覆盖厂商级」两段说明。
// 这段说明比组级帮助长，所以不走 printGroupHelp：光列子命令说清不了值怎么展开。
func printProviderHeaderHelp(w io.Writer) {
	var b strings.Builder
	b.WriteString(appName + " provider header — 厂商附加请求头：有些上游除凭据头之外还要额外的头才路由\n\n")
	b.WriteString(providerHeaderUsageText + "\n\n")
	b.WriteString("可用的 --protocol：anthropic | openai | responses | gemini（也认线协议全名 openai-chat、openai-responses）。\n")
	b.WriteString("不给 --protocol 就是厂商级：这家厂商的每条路由都带上这条头；给了协议就是协议级：只加给这条协议。\n")
	b.WriteString("同名头两边都有时协议级覆盖厂商级，删掉协议级那行就回退到厂商级。\n\n")
	b.WriteString("值的写法（转发前才展开，展开后为空则这条头不发）：\n")
	b.WriteString("  字面量      原样发出，例如 api-version 这类固定值\n")
	b.WriteString("  {env:NAME}  取进程环境变量 NAME 的值，适合不该落库的会话 token\n")
	b.WriteString("  {session}   本次请求的会话键（x-lax-session 或 metadata.user_id），都没有时每次新生成一个 UUID\n")
	b.WriteString("  {model}     本次请求的模型名（裸名，不转义）\n\n")
	b.WriteString("值可能是密钥，ls 与 --json 只输出脱敏后的前四位，完整值不打印。\n\n")
	b.WriteString("示例：\n")
	b.WriteString("  lax provider header set opencode x-opencode-session '{session}' --protocol openai-chat\n\n")
	b.WriteString("查看某个子命令的参数：lax provider header <子命令> --help\n")
	_, _ = w.Write([]byte(b.String()))
}

// cmdProviderHeader 是 provider header 这一组子命令的入口。
func cmdProviderHeader(e *Env, args []string) error {
	if isHelpRequest(args) {
		printProviderHeaderHelp(e.Stdout)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：set | unset | ls\n用 lax provider header --help 查看用法")
	}
	switch args[0] {
	case "set":
		return cmdProviderHeaderSet(e, args[1:])
	case "unset":
		return cmdProviderHeaderUnset(e, args[1:])
	case "ls":
		return cmdProviderHeaderLs(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：set | unset | ls\n用 lax provider header --help 查看用法", args[0])
	}
}

// headerProtocolFlag 把 --protocol 的取值换成线协议，不给（或没出现在命令行上）就是厂商级（空串）。
// 映射与 --default-protocol 同一份（protocolFromCLIName），落到线协议全名：store 只认线协议，
// 命令行写法 `openai` 直接落库会让这条头永远匹配不上真实协议，等于这条头从来没生效过。
func headerProtocolFlag(fs *flagSet, raw, usageLine string) (string, error) {
	if !flagWasSet(fs, "protocol") {
		return "", nil
	}
	if strings.TrimSpace(raw) == "" {
		return "", usagef("--protocol 传了空值：厂商级（每一条路由都加）就是整个省掉这个参数，只加给某条协议才给协议名\n%s", usageLine)
	}
	proto, err := protocolFromCLIName(raw, "--protocol", "lax provider header set opencode x-opencode-session '{session}' --protocol openai")
	if err != nil {
		return "", err
	}
	return proto, nil
}

// checkHeaderName 在写库之前判一遍头名，让非法头名拿到退出码 2 与用法行。
// 写库时的权威仍是 store.normalizeHeaderName（头名归一成小写也在那里），这里重复同一组
// RFC 9110 token 字符，只为在动库之前把「命令写错了」与「运行失败」分开：退出码 2 要打出用法行。
func checkHeaderName(raw, usageLine string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", usagef("头名不能为空\n%s", usageLine)
	}
	for _, r := range name {
		if !isHeaderTokenRune(r) {
			return "", usagef("头名 %q 含非法字符 %q：HTTP 头名只能是字母、数字与 %s，例如 x-opencode-session\n%s",
				raw, r, headerNameSymbols, usageLine)
		}
	}
	return name, nil
}

// headerNameSymbols 是头名里除了字母数字以外允许的字符，与 RFC 9110 的 token 定义一致。
const headerNameSymbols = "!#$%&'*+-.^_`|~"

// isHeaderTokenRune 判断一个字符能不能出现在 HTTP 头名里，非 ASCII 一律不行（中文头名无法拼进请求行）。
func isHeaderTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r < 0x80 && strings.ContainsRune(headerNameSymbols, r)
}

// checkHeaderValue 在写库之前判一遍值：空值（删一条头用 unset）、带 CR/LF/NUL（拼进请求头会把
// 请求拆成两条）都是用法错误。store 的写入口还会再判一次。
func checkHeaderValue(raw, usageLine string) error {
	if strings.TrimSpace(raw) == "" {
		return usagef("头的值不能为空：删掉这条头用 lax provider header unset <厂商> <头名>；值可能是环境变量里的密钥，写成 {env:NAME}\n%s", usageLine)
	}
	if i := strings.IndexAny(raw, "\r\n\x00"); i >= 0 {
		return usagef("头的值里不能有回车、换行或 NUL（第 %d 个字节）：值原样拼进上游请求头，带折行等于多拼出一条头\n%s", i+1, usageLine)
	}
	return nil
}

// cmdProviderHeaderSet 声明一条附加头，覆盖语义：同名同作用范围再写一次就是改值。
func cmdProviderHeaderSet(e *Env, args []string) error {
	fs := newFlagSet(e, "provider header set", "声明或覆盖一家厂商的一条附加请求头")
	protocol := fs.String("protocol", "", "只加给这条协议：anthropic | openai | responses | gemini；不给就是厂商级，这家厂商的每条路由都带上它")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 3 {
		return usagef("%s\nset 是一次声明一条头：厂商、头名、值三个位置参数都要给，改值就再跑一次同样的命令", providerHeaderSetUsage)
	}

	provider := fs.Arg(0)
	if strings.TrimSpace(provider) == "" {
		return usagef("厂商名不能为空\n%s", providerHeaderSetUsage)
	}
	name, err := checkHeaderName(fs.Arg(1), providerHeaderSetUsage)
	if err != nil {
		return err
	}
	value := fs.Arg(2)
	if err := checkHeaderValue(value, providerHeaderSetUsage); err != nil {
		return err
	}
	proto, err := headerProtocolFlag(fs, *protocol, providerHeaderSetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 厂商不存在时 store 报 ErrProviderNotFound，文案已指向 provider ls 与 provider add。
	if err := a.st.SetProviderHeader(provider, proto, name, value); err != nil {
		return err
	}

	// store 落库时把头名归一成小写，输出照同一个形态写。
	item := providerHeaderItem{
		provider: provider,
		header:   store.ProviderHeader{Protocol: proto, Name: strings.ToLower(name), Value: value},
	}
	if e.JSON {
		return e.printJSON(item.toJSON())
	}
	fmt.Fprintf(e.Stdout, "已声明 %s 的附加头 %s（%s），值 %s\n",
		item.provider, item.header.Name, headerScopeLabel(proto), item.header.Masked())
	fmt.Fprintf(e.Stdout, "值可能是密钥，输出只显示前四位；下一步：lax provider header ls %s 核对\n", provider)
	return nil
}

// cmdProviderHeaderUnset 删掉一条声明。删不到时报错并列出这家厂商现有的声明：
// 多半是头名或协议写错了，光说一句「没有这条头」看不出下一步该改哪个参数。
func cmdProviderHeaderUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "provider header unset", "删掉一条声明过的附加请求头")
	protocol := fs.String("protocol", "", "删的是哪一条：anthropic | openai | responses | gemini；不给就是厂商级那条")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("%s\n想清掉厂商级那条就整个省掉 --protocol，协议级那条要写清协议", providerHeaderUnsetUsage)
	}

	provider := fs.Arg(0)
	if strings.TrimSpace(provider) == "" {
		return usagef("厂商名不能为空\n%s", providerHeaderUnsetUsage)
	}
	name, err := checkHeaderName(fs.Arg(1), providerHeaderUnsetUsage)
	if err != nil {
		return err
	}
	proto, err := headerProtocolFlag(fs, *protocol, providerHeaderUnsetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.st.DeleteProviderHeader(provider, proto, name); err != nil {
		if errors.Is(err, store.ErrProviderHeaderNotFound) {
			return headerNotFoundError(a.st, provider, proto, name)
		}
		return err
	}

	remaining, err := a.st.ProviderHeaders(provider)
	if err != nil {
		return err
	}
	lower := strings.ToLower(name)

	if e.JSON {
		return e.printJSON(map[string]any{
			"provider":  provider,
			"protocol":  proto,
			"name":      lower,
			"removed":   true,
			"remaining": len(remaining),
		})
	}
	fmt.Fprintf(e.Stdout, "%s 已删掉附加头 %s（%s）\n", provider, lower, headerScopeLabel(proto))
	// 删掉的是协议级那行时，同名头可能还有厂商级的一条：转发会改用它，说清楚免得以为这条头没了。
	if proto != "" {
		for _, h := range remaining {
			if h.Protocol == "" && h.Name == lower {
				fmt.Fprintf(e.Stdout, "同名头还有厂商级的一条（值 %s），转发时会用它；要连它一起删就省掉 --protocol 再跑一次\n", h.Masked())
				break
			}
		}
	}
	fmt.Fprintf(e.Stdout, "现有 %d 条附加头，用 lax provider header ls %s 看全部\n", len(remaining), provider)
	return nil
}

// headerNotFoundError 在删不到时列出这家厂商现有的声明，并给出声明与查看两条下一步命令。
// 厂商标识本身不在（ErrProviderNotFound）不走这里：那条错误由 store 给，已经指向 provider ls。
func headerNotFoundError(st *store.Store, provider, proto, name string) error {
	headers, err := st.ProviderHeaders(provider)
	if err != nil {
		return err
	}
	lower := strings.ToLower(name)
	declared := make([]string, 0, len(headers))
	for _, h := range headers {
		declared = append(declared, fmt.Sprintf("%s（%s）", h.Name, headerScopeLabel(h.Protocol)))
	}
	msg := fmt.Sprintf("厂商 %s 没有声明附加头 %s（%s）", provider, lower, headerScopeLabel(proto))
	if len(declared) == 0 {
		msg += fmt.Sprintf("：这家厂商现在一条附加头都没有，声明一条用 lax provider header set %s %s <值> [--protocol <协议>]", provider, lower)
		return errors.New(msg)
	}
	return fmt.Errorf("%s：现有 %s；看全部用 lax provider header ls %s，声明一条用 lax provider header set %s %s <值> [--protocol <协议>]",
		msg, strings.Join(declared, "、"), provider, provider, lower)
}

// cmdProviderHeaderLs 列已声明的附加头。不给厂商就列全部厂商的，一次看全。
func cmdProviderHeaderLs(e *Env, args []string) error {
	fs := newFlagSet(e, "provider header ls", "列已声明的附加请求头（值脱敏）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("%s\n一次最多看一家厂商，看全部就整个省掉厂商名", providerHeaderLsUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	var names []string
	if fs.NArg() == 1 {
		name := fs.Arg(0)
		// 厂商不在表里时合成空列表等于骗人：写错一个字母会看起来「这条头没声明过」。
		if _, err := a.st.GetProvider(name); err != nil {
			return err
		}
		names = []string{name}
	} else {
		providers, err := a.st.ListProviders()
		if err != nil {
			return err
		}
		for _, p := range providers {
			names = append(names, p.Name)
		}
	}

	items := make([]providerHeaderItem, 0, len(names))
	for _, name := range names {
		headers, err := a.st.ProviderHeaders(name)
		if err != nil {
			return err
		}
		for _, h := range headers {
			items = append(items, providerHeaderItem{provider: name, header: h})
		}
	}

	if e.JSON {
		out := make([]providerHeaderJSON, 0, len(items))
		for _, it := range items {
			out = append(out, it.toJSON())
		}
		return e.printJSON(out)
	}

	if len(items) == 0 {
		fmt.Fprintln(e.Stdout, "还没有声明任何附加头。有些上游除凭据头之外还要额外的头才路由，例如：lax provider header set opencode x-opencode-session '{session}' --protocol openai-chat")
		return nil
	}

	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{
			it.provider, headerScopeCell(it.header.Protocol), it.header.Name, it.header.Masked(),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"厂商", "协议", "头名", "值（脱敏）"}, rows))
	return nil
}
