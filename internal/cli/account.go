package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	{"set", "换掉账号的凭据，冷却与失败计数随之清零并重新启用"},
	{"rm", "删除账号，历史请求记录保留"},
	{"test", "先 GET 模型列表、不通时用最小请求，探活一个账号"},
	{"enable", "启用账号并清掉冷却"},
	{"disable", "停用账号，不再参与选号"},
}

func cmdAccount(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "account", "管理上游账号", accountSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：add | ls | set | rm | test | enable | disable\n用 switchboard account --help 查看子命令")
	}
	switch args[0] {
	case "add":
		return cmdAccountAdd(e, args[1:])
	case "ls":
		return cmdAccountLs(e, args[1:])
	case "set", "update":
		// update 是 set 的同义词：帮助里只列 set，但两个写法都收（见 DESIGN 4）。
		return cmdAccountSet(e, args[1:])
	case "rm":
		return cmdAccountRm(e, args[1:])
	case "test":
		return cmdAccountTest(e, args[1:])
	case "enable":
		return cmdAccountSetEnabled(e, args[1:], "enable", true)
	case "disable":
		return cmdAccountSetEnabled(e, args[1:], "disable", false)
	default:
		return usagef("未知子命令 %q，可用：add | ls | set | rm | test | enable | disable\n用 switchboard account --help 查看子命令", args[0])
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

// accountSetUsage 是 account set 的用法行，用法错误与账号不存在时都打出来。
const accountSetUsage = "用法：switchboard account set <名称> [--key <key>] [--price-in N] [--price-out N] [--price-cache-read N] [--price-cache-write N]\n" +
	"--key 三种给法：直接写值、写 - 从标准输入读、留空读环境变量 SWITCHBOARD_KEY\n" +
	"--price-* 只改传入的那几项，其余列保持不动；只改价格时不必给 --key"

// cmdAccountSet 改一个已有账号的凭据与价格，不删号重建：删了会丢掉探活结果、协议发现与请求记录归属。
// 换 key 之后旧凭据的结论作废，同一个事务里清掉冷却、最近错误与连续失败次数并重新启用账号（见 DESIGN 4、5.6）。
// 价格改动走 store.SetAccountPrices，只写传入的那几列：换凭据会清冷却，改价格不能顺手抹掉账号状态。
// `account_models` 里的可达性、覆盖值与协议发现结果一个字不动，也不碰厂商表。
func cmdAccountSet(e *Env, args []string) error {
	fs := newFlagSet(e, "account set", "换掉账号的凭据或改价格，换凭据时冷却与失败计数随之清零并重新启用")
	key := fs.String("key", "", "新的上游 API key；给 - 从标准输入读，留空则读环境变量 SWITCHBOARD_KEY")
	priceIn := fs.Float64("price-in", 0, "输入价，每百万 token")
	priceOut := fs.Float64("price-out", 0, "输出价，每百万 token")
	priceCacheRead := fs.Float64("price-cache-read", 0, "缓存读价，每百万 token，留空回退到输入价")
	priceCacheWrite := fs.Float64("price-cache-write", 0, "缓存写价，每百万 token，留空回退到输入价")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usagef("%s", accountSetUsage)
	}
	name := fs.Arg(0)
	if err := checkPrices(accountSetUsage, knownPriceFlags(fs, priceIn, priceOut, priceCacheRead, priceCacheWrite)...); err != nil {
		return err
	}

	// 给了价格参数就可以不换凭据：改价格与换 key 是两件事，只改价的调用不该顺带换一个凭据。
	// 没给价格参数时仍是原来的行为：必须能解析出凭据，否则是用法错误。
	rotate := flagWasSet(fs, "key") || !anyPriceFlagSet(fs)
	var keyValue string
	if rotate {
		// 三种给法与 account add 完全一致，共用 resolveKey：两处判定不一致时，换 key 会突然多出一个坑。
		v, err := resolveKey(*key, "switchboard account set "+name+" --key - < key.txt", accountSetUsage)
		if err != nil {
			return err
		}
		keyValue = v
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	acc, err := a.st.GetAccount(name)
	if err != nil {
		// 改动针对已有账号：不存在时给下一步命令，不要只抛一句 sql.ErrNoRows。
		return fmt.Errorf("账号 %q 不存在，先运行 switchboard account add %s --provider <厂商> --key <key>\n%s", name, name, accountSetUsage)
	}

	if rotate {
		// 与 account add 走同一条加密路径；明文不进日志，也不进 --json 输出（那里只有掩码）。
		cipher, err := secret.Encrypt(a.key, []byte(keyValue))
		if err != nil {
			return err
		}
		if err := a.st.RotateKey(acc.ID, cipher); err != nil {
			return fmt.Errorf("更新 %s 的凭据失败: %w", name, err)
		}
	}

	prices := store.AccountPrices{
		In:         pricePatch(fs, "price-in", priceIn),
		Out:        pricePatch(fs, "price-out", priceOut),
		CacheRead:  pricePatch(fs, "price-cache-read", priceCacheRead),
		CacheWrite: pricePatch(fs, "price-cache-write", priceCacheWrite),
	}
	if err := a.st.SetAccountPrices(acc.ID, prices); err != nil {
		return fmt.Errorf("更新 %s 的价格失败: %w", name, err)
	}
	// 写完再读一次：输出与 --json 里报的是库里实际的价，而不是命令行上期望的值。
	if acc, err = a.st.GetAccount(name); err != nil {
		return err
	}

	if e.JSON {
		out := map[string]any{
			"name":              name,
			"key_rotated":       rotate,
			"price_in":          acc.PriceIn,
			"price_out":         acc.PriceOut,
			"price_cache_read":  acc.PriceCacheRead,
			"price_cache_write": acc.PriceCacheWrite,
		}
		if rotate {
			// 换凭据顺带清零的那几项只在真的换了凭据时为 true，只改价格时它们没有发生。
			out["key"] = secret.Mask(keyValue)
			out["cooldown_cleared"] = true
			out["fail_streak_cleared"] = true
			out["enabled"] = true
		}
		return e.printJSON(out)
	}
	if rotate {
		fmt.Fprintf(e.Stdout, "已更新 %s 的凭据（%s）；冷却与失败计数已清零，账号已启用\n", name, secret.Mask(keyValue))
	}
	if anyPriceFlagSet(fs) {
		fmt.Fprintf(e.Stdout, "已更新 %s 的价格（每百万 token）：输入 %s、输出 %s、缓存读 %s、缓存写 %s\n",
			name, priceText(acc.PriceIn), priceText(acc.PriceOut),
			cachePriceText(acc.PriceCacheRead), cachePriceText(acc.PriceCacheWrite))
	}
	return nil
}

// accountAddUsage 是 account add 的用法行，用法错误与厂商不在表里时都打出来。
const accountAddUsage = "用法：switchboard account add <名称> --provider <厂商> --key <key> [--models <模型>] [--priority N] [--price-in N] [--price-out N] [--price-cache-read N] [--price-cache-write N]\n" +
	"地址与默认协议不落在账号上：先 switchboard provider add <厂商> --base-url <地址> --openai /v1/chat/completions 登记厂商，再建账号\n" +
	"--models 手填的名字写进模型目录（不带长度与价格），并把这个账号标成可用；留空则等 model sync 与 model probe"

// retiredAccountAddFlags 是已经从 account add 上删掉的两个参数与各自的替代做法（见 DESIGN 4、5.8）：
// 地址与默认协议只在厂商行上，账号不再持有副本。误用时给一句指出下一步的用法错误，
// 而不是让人对着「参数不存在」自己找原因。
var retiredAccountAddFlags = map[string]string{
	"--base-url": "地址只在厂商行上，先用 switchboard provider add <厂商> --base-url <地址> --openai /v1/chat/completions 登记厂商",
	"--protocol": "默认协议只在厂商行上，登记厂商时用 --default-protocol 指定，或改用 switchboard provider set <厂商> --default-protocol <协议>",
}

// rejectRetiredFlags 在 flag 包之前拦下已经删掉的参数：flag 只会说「参数不认识」，
// 这里能说清它去了哪、下一步该跑什么命令。`--名字 值` 与 `--名字=值` 两种写法都拦。
func rejectRetiredFlags(args []string, retired map[string]string, usageLine string) error {
	for _, arg := range args {
		name := arg
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if hint, ok := retired[name]; ok {
			return usagef("%s 已经不在这个子命令上：%s\n%s", name, hint, usageLine)
		}
	}
	return nil
}

func cmdAccountAdd(e *Env, args []string) error {
	if err := rejectRetiredFlags(args, retiredAccountAddFlags, accountAddUsage); err != nil {
		return err
	}
	fs := newFlagSet(e, "account add", "新增一个上游账号")
	provider := fs.String("provider", "", "上游厂商标识，地址与默认协议都从这家厂商读，如 deepseek")
	key := fs.String("key", "", "上游 API key；给 - 从标准输入读，留空则读环境变量 SWITCHBOARD_KEY")
	name := fs.String("name", "", "账号名称，也可以用位置参数给出")
	models := fs.String("models", "", "该账号可用的模型，逗号分隔：名字进模型目录，账号侧记一条可用")
	priceIn := fs.Float64("price-in", 0, "输入价，每百万 token")
	priceOut := fs.Float64("price-out", 0, "输出价，每百万 token")
	priceCacheRead := fs.Float64("price-cache-read", 0, "缓存读价，每百万 token，留空回退到 --price-in")
	priceCacheWrite := fs.Float64("price-cache-write", 0, "缓存写价，每百万 token，留空回退到 --price-in")
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
	if err := checkPrices(accountAddUsage, knownPriceFlags(fs, priceIn, priceOut, priceCacheRead, priceCacheWrite)...); err != nil {
		return err
	}
	if *provider == "" {
		return usagef("--provider 必填，例如 --provider deepseek\n%s", accountAddUsage)
	}

	keyValue, err := resolveKey(*key, "switchboard account add x --provider p --key - < key.txt", accountAddUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 地址与默认协议只在厂商行上（见 DESIGN 5.8）：账号只认厂商名，厂商不在表里就没有
	// 地址可拼、没有协议可推导，先在厂商表里登记它。厂商行在但一条路由都没声明不算错：
	// 转发时按厂商默认协议推导，推导为空由转发层报错。
	if _, err := a.st.GetProvider(*provider); err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			return unknownProviderError(a.st, *provider)
		}
		return err
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
		KeyCipher: cipher,
		Models:    splitModels(*models),
		Priority:  *priority,
		Enabled:   true,
		// 价格只写命令行上真的给过的那几项：没给就留空，花费记为 null，不猜价格（见 DESIGN 5.5）。
		PriceIn:         pricePatch(fs, "price-in", priceIn),
		PriceOut:        pricePatch(fs, "price-out", priceOut),
		PriceCacheRead:  pricePatch(fs, "price-cache-read", priceCacheRead),
		PriceCacheWrite: pricePatch(fs, "price-cache-write", priceCacheWrite),
	}
	if err := a.st.CreateAccount(&acc); err != nil {
		return fmt.Errorf("新增账号失败: %w", err)
	}
	// 地址与协议是厂商行派生出来的只读值，写完再读一次：输出报库里实际生效的那两个。
	if acc, err = a.st.GetAccount(accountName); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"name":     acc.Name,
			"provider": acc.Provider,
			"protocol": acc.Protocol,
			"base_url": acc.BaseURL,
			// models 是写完再读一次的派生值：这个账号 access='allowed' 的名字集合，
			// 给了 --models 就是那些名字（先写目录，再标这个账号可用，见 DESIGN 5.13）。
			"models": acc.Models,
			"key":    secret.Mask(keyValue),
		})
	}
	fmt.Fprintf(e.Stdout, "已添加 %s（%s，%s，凭据 %s）\n", acc.Name, acc.Provider, dashIfEmpty(acc.Protocol), secret.Mask(keyValue))
	return nil
}

// dashIfEmpty 把空值写成 -：账号的地址与默认协议都是厂商行派生来的，厂商没写 base_url
// 或一条路由都没声明时它们就是空串，空单元格看不出是「没配」还是「渲染漏了」。
func dashIfEmpty(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func envKey() string {
	for _, name := range []string{"SWITCHBOARD_KEY", "SWITCHBOARD_API_KEY"} {
		if v := lookupEnv(name); v != "" {
			return v
		}
	}
	return ""
}

// resolveKey 把 --key 的三种给法解成一个值：直接写值、`-`（或 `--`）从标准输入读、
// 留空读环境变量 SWITCHBOARD_KEY（兼容 SWITCHBOARD_API_KEY）。account add 与 account set 共用一份，
// 两处判定不一致时，换 key 会突然多出一个坑。stdinExample 是「从管道读」错误文案里的示例命令，
// usageLine 是这个子命令的用法行：缺凭据是用法错误，输出要带上正确用法（见 AGENTS.md）。
func resolveKey(flagValue, stdinExample, usageLine string) (string, error) {
	value := flagValue
	if value == "" {
		value = envKey()
	}
	if value == "-" || value == "--" {
		v, err := readKeyFromStdin(stdinExample)
		if err != nil {
			return "", err
		}
		value = v
	}
	if value == "" {
		return "", usagef("缺少凭据：--key <key>、--key - 从标准输入读，或环境变量 SWITCHBOARD_KEY\n%s", usageLine)
	}
	return value, nil
}

// readKeyFromStdin 从标准输入读凭据，去掉首尾空白，example 是用在错误文案里的示例命令。
// 直接在终端上运行且没有管道时报错，避免命令挂起等输入。
func readKeyFromStdin(example string) (string, error) {
	info, statErr := os.Stdin.Stat()
	if statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
		return "", usagef("--key - 需要从管道或文件读入，例如 %s", example)
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
			// protocol 与 base_url 都是厂商行派生的只读值：协议取厂商的有效默认协议
			// （见 DESIGN 5.8），地址取厂商的 base_url，厂商行不在时都是空串。
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
				// 与文本输出的「N 可用 / M 已登记」同一组计数：两个数都来自 account_models，
				// usable 数 access='allowed' 的行，synced 数全部行（见 DESIGN 5.13）。
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
			baseURLCell(acc.BaseURL),
			protocolCell(acc.Protocol, counts[acc.ID].Protocols),
			modelsCell(counts[acc.ID]),
			fmt.Sprintf("%d", acc.Priority),
			accountStatus(acc, now),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "厂商", "地址", "协议", "模型", "优先级", "状态"}, rows))
	return nil
}

// modelCount 是一个账号在 account_models 里的汇总。
// Usable 是 access='allowed' 的行数（能不能进候选只看这一列，见 DESIGN 5.13），
// Total 是这个账号在 account_models 里的全部行数：同步、手填与探活都会留行。
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
		if r.Access == store.AccessAllowed {
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
// 默认协议为空串（厂商行不在、或这家一条路由都没声明）时不把它放进列表：
// 空串不是一个协议，`--json` 的 protocols 只列真正算得出来的那几种。
func allProtocols(accountProtocol string, discovered []string) []string {
	out := make([]string, 0, len(discovered)+1)
	if accountProtocol != "" {
		out = append(out, accountProtocol)
	}
	for _, p := range discovered {
		if p != accountProtocol {
			out = append(out, p)
		}
	}
	return out
}

// protocolCell 是 account ls 的协议列：默认协议（厂商的有效默认协议）就是它本身，
// 存在与默认不同的已发现协议时标出「另 N 种」，一眼能看出这个账号不止走一种协议。
// 默认协议算不出来（厂商行不在、或这家一条路由都没声明）时写 -，与地址列同一种写法：
// 这一列说的是账号的默认协议，不是模型行上那些发现结果，后者只在 --json 的 protocols 里列。
func protocolCell(accountProtocol string, discovered []string) string {
	if accountProtocol == "" {
		return "-"
	}
	all := allProtocols(accountProtocol, discovered)
	if len(all) == 1 {
		return accountProtocol
	}
	return fmt.Sprintf("%s + 另 %d 种", accountProtocol, len(all)-1)
}

// modelsCell 是 account ls 的模型列，两种情况都能自解释：
//
//   - account_models 有行时写「N 可用 / M 已登记」，与 --json 的 models_usable、models_synced
//     同一组数：分子数 access='allowed'，分母数这个账号的全部可达性行。分母写「已登记」而不是
//     「已同步」或「已探活」，因为三种来源都会留行（`model sync`、`account add --models`、
//     `model probe`），其中同步与手填的行还没探过。零可用同样走这条分支，「0 可用 / M 已登记」
//     说明这个账号眼下接不了任何模型；
//   - 一行都没有（没同步过、也没手填 --models）时写「无模型（先 model sync）」，
//     与 --json 里 models 为空数组且 models_synced 为 0 对应。
//
// 不能写成像「不限」那样表示「什么模型都收」的文案：空模型列表表示该账号不提供任何模型、
// 不参与选号（见 lessons/2026-10-29-empty-model-list.md），文案与行为必须一致。
func modelsCell(c modelCount) string {
	if c.Total == 0 {
		return "无模型（先 model sync）"
	}
	return fmt.Sprintf("%d 可用 / %d 已登记", c.Usable, c.Total)
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

// cmdAccountTest 探活一个账号。判定分两步：先 GET 模型列表（地址与凭据头跟 `model sync` 同一套规则），
// 证明地址通并报出上游模型数；随后一律发一条 max_tokens=1 的请求证明凭据有效——公开的列表接口
// 对任何 key 都回 200，光凭列表判不出凭据。两段都通过才判正常，状态与退出码按调用那段算。
// 它只判凭据与连通性，某个模型能不能用由 `model probe` 决定（见 DESIGN 4、5.7）。
func cmdAccountTest(e *Env, args []string) error {
	fs := newFlagSet(e, "account test", "先 GET 模型列表证明地址通，再发一条最小请求证明凭据有效")
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
			// stage 是判定成败的那一步：两段都跑到时是 call（凭据），列表接口自己给出结论时是 models。
			"stage":   res.Stage,
			"status":  res.Status,
			"ok":      res.OK,
			"models":  res.Models,
			"latency": res.Latency.String(),
			"message": res.Message,
			// list 是列表那一段自己的结论：地址与连通性、上游模型数。调用那段的 401 不会抹掉它。
			"list": map[string]any{
				"stage":   res.List.Stage,
				"status":  res.List.Status,
				"ok":      res.List.OK,
				"models":  res.List.Models,
				"latency": res.List.Latency.String(),
				"message": res.List.Message,
			},
		}); err != nil {
			return err
		}
	} else {
		state := "失败"
		if res.OK {
			state = "正常"
		}
		// 上游模型数只在列表接口真报出数目时写数字；没走列表接口、或上游返回的不是模型列表时写 -，
		// 免得把「不知道」读成「零个模型」。
		models := "-"
		if res.ModelsKnown {
			models = strconv.Itoa(res.Models)
		}
		rows := [][]string{{
			res.Account,
			state,
			strconv.Itoa(res.Status),
			res.Latency.Round(time.Millisecond).String(),
			models,
			res.Message,
		}}
		fmt.Fprint(e.Stdout, renderTable([]string{"账号", "状态", "HTTP", "耗时", "上游模型数", "备注"}, rows))
	}

	if !res.OK {
		return fmt.Errorf("账号 %s 探活失败（%s，%d）：%s", res.Account, probeStageLabel(res.Stage), res.Status, res.Message)
	}
	return nil
}

// probeStageLabel 把阶段标识翻成给人看的说法：备注与错误文案里说清结论出自哪一步。
func probeStageLabel(stage string) string {
	if stage == gateway.ProbeStageModels {
		return "列表接口"
	}
	return "模型调用"
}

// modelSubs 是 model 一组的子命令清单。
var modelSubs = []groupItem{
	{"ls", "列出网关可提供的模型、账号状态与目录里的元数据"},
	{"sync", "从上游拉取模型列表，名字进模型目录并把这个账号标成可用"},
	{"probe", "逐个模型发 max_tokens=1 的请求探活，把可达性写进账号"},
	{"alias", "维护模型别名：客户端写死的模型名指到真实模型名上（set / unset / ls）"},
}

// cmdModel 是 model 这一组子命令的入口。
func cmdModel(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "model", "模型同步、探活与别名", modelSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：ls | sync | probe | alias\n用 switchboard model --help 查看子命令")
	}
	switch args[0] {
	case "ls":
		return cmdModelLs(e, args[1:])
	case "sync":
		return cmdModelSync(e, args[1:])
	case "probe":
		return cmdModelProbe(e, args[1:])
	case "alias":
		return cmdModelAlias(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | sync | probe | alias\n用 switchboard model --help 查看子命令", args[0])
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
	type entry struct {
		model    string
		accounts []modelAccountJSON
		priceIn  *float64
		priceOut *float64
		// 目录里这个模型的元数据：空值表示目录也没填，输出时写 -。
		contextWindow *int64
		maxOutput     *int64
		protocols     []string
	}

	modelRows, err := a.st.AllAccountModels()
	if err != nil {
		return err
	}
	rowByAccount := make(map[int64][]store.AccountModel, len(accounts))
	for _, r := range modelRows {
		rowByAccount[r.AccountID] = append(rowByAccount[r.AccountID], r)
	}

	// 支持的协议集合只在目录里：账号侧没有协议集合的覆盖值，合并只发生在长度与价格上
	// （见 DESIGN 5.13）。一家厂商的目录读一次就够，按模型名索引。
	catalogs := map[string]map[string]store.ProviderModel{}
	catalogOf := func(provider string) (map[string]store.ProviderModel, error) {
		if cat, ok := catalogs[provider]; ok {
			return cat, nil
		}
		list, err := a.st.ProviderModels(provider)
		if err != nil {
			return nil, err
		}
		cat := make(map[string]store.ProviderModel, len(list))
		for _, pm := range list {
			cat[pm.Model] = pm
		}
		catalogs[provider] = cat
		return cat, nil
	}

	byModel := map[string]*entry{}
	var order []string
	for _, acc := range accounts {
		rows := rowByAccount[acc.ID]
		if len(rows) == 0 {
			continue
		}
		// 元数据与有效协议都问 store.EffectiveModels：长度与价格按「账号覆盖 → 目录」合并，
		// 协议按账号覆盖 → 目录里的协议集合 → 厂商默认协议定序（见 DESIGN 5.13）。
		// cli 不在这里重算一份合并规则。
		infos, err := a.st.EffectiveModels(acc.ID)
		if err != nil {
			return err
		}
		byName := make(map[string]store.ModelInfo, len(infos))
		for _, info := range infos {
			byName[info.Model] = info
		}
		cat, err := catalogOf(acc.Provider)
		if err != nil {
			return err
		}
		for _, r := range rows {
			info, ok := byName[r.Model]
			if !ok {
				continue
			}
			ent, ok := byModel[r.Model]
			if !ok {
				ent = &entry{
					model:    r.Model,
					priceIn:  acc.PriceIn,
					priceOut: acc.PriceOut,
					// 目录元数据取第一个给出这个模型的账号：同一个模型在多个账号上取值不同
					// 只可能来自账号侧的覆盖值，那一列今天还没有写入命令（见 DESIGN 5.13）。
					contextWindow: info.ContextWindow,
					maxOutput:     info.MaxOutput,
					protocols:     cat[r.Model].Protocols,
				}
				byModel[r.Model] = ent
				order = append(order, r.Model)
			}
			ent.accounts = append(ent.accounts, modelAccountJSON{
				Name:      acc.Name,
				Status:    info.Access,
				Protocol:  info.Protocol,
				checkedAt: r.CheckedAt,
			})
		}
	}
	sort.Strings(order)

	if e.JSON {
		out := make([]map[string]any, 0, len(order))
		for _, m := range order {
			ent := byModel[m]
			out = append(out, map[string]any{
				"model":          ent.model,
				"accounts":       ent.accounts,
				"context_window": ent.contextWindow,
				"max_output":     ent.maxOutput,
				"protocols":      protocolList(ent.protocols),
				"price_in":       ent.priceIn,
				"price_out":      ent.priceOut,
			})
		}
		return e.printJSON(out)
	}

	if len(order) == 0 {
		fmt.Fprintln(e.Stdout, "还没有模型信息：先 switchboard model sync 拉上游模型列表，再 switchboard model probe 逐个探活；也可以用 switchboard account add --models 手填")
		return nil
	}

	rows := make([][]string, 0, len(order))
	for _, m := range order {
		ent := byModel[m]
		names := make([]string, 0, len(ent.accounts))
		for _, ae := range ent.accounts {
			names = append(names, fmt.Sprintf("%s(%s)", ae.Name, modelAccessText(ae.Status, ae.checkedAt)))
		}
		price := "未定价"
		if ent.priceIn != nil || ent.priceOut != nil {
			price = fmt.Sprintf("%s / %s", priceText(ent.priceIn), priceText(ent.priceOut))
		}
		rows = append(rows, []string{
			ent.model,
			strings.Join(names, "、"),
			modelProtocolCell(ent.accounts),
			tokenCell(ent.contextWindow),
			tokenCell(ent.maxOutput),
			protocolSetCell(ent.protocols),
			price,
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"模型", "账号", "协议", "上下文长度", "最大输出", "支持协议", "输入价 / 输出价（每百万 token）"}, rows))
	return nil
}

// modelAccountJSON 是 model ls --json 里某个模型下的一个账号。
// Status 取 account_models.access（allowed / forbidden / missing / error），
// Protocol 是该账号在这个模型上的有效出站协议。
type modelAccountJSON struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Protocol string `json:"protocol"`
	// checkedAt 只用在人读输出上：allowed 且从没探过的行要写「待探活」而不是「可用」。
	// 探活时间不是这一列要回答的事，不进 --json。
	checkedAt int64 `json:"-"`
}

// modelAccessText 把可达性换成人话。「可用」只给探过的 allowed 行：刚同步进来或手填的
// allowed 行还是一次都没验证过的，写成「可用」会让人以为这些模型已经试通了。
func modelAccessText(access string, checkedAt int64) string {
	switch access {
	case store.AccessAllowed:
		if checkedAt == 0 {
			return "待探活"
		}
		return "可用"
	case store.AccessForbidden:
		return "无权"
	case store.AccessMissing:
		return "不存在"
	case store.AccessError:
		return "失败"
	default:
		return access
	}
}

// tokenCell 是 model ls 里长度类元数据的单元格：目录里没填就是空，写 - 与别的空值列一致。
func tokenCell(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

// protocolSetCell 是「支持协议」列：目录里这个模型支持的协议集合，空集合写 -。
func protocolSetCell(protocols []string) string {
	if len(protocols) == 0 {
		return "-"
	}
	return strings.Join(protocols, ",")
}

// protocolList 让 --json 的 protocols 在空集合时是 []，不是 null：
// 空数组与字段缺失在读的那一端不是一回事（与 provider ls 的写法一致）。
func protocolList(protocols []string) []string {
	if protocols == nil {
		return []string{}
	}
	return protocols
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

// modelStatusText 把探活结果换成人话。取值来自网关（ModelProbe.Status），
// 与 account_models.access 是两套词表，这里两套都认。
// 注意 ModelForbidden / ModelError 与 AccessForbidden / AccessError 是同一个字符串，
// 一个 case 列一处就够，别再加一遍重复的 case。
func modelStatusText(status string) string {
	switch status {
	case store.ModelUsable, store.AccessAllowed:
		return "可用"
	case store.ModelSynced:
		return "待探活"
	case store.ModelForbidden:
		return "无权"
	case store.ModelNotFound, store.AccessMissing:
		return "不存在"
	case store.ModelAuthFailed:
		return "凭据被拒"
	case store.ModelError:
		return "失败"
	default:
		return status
	}
}

// cmdModelSync 从上游拉模型列表。按 --provider / --account 选账号，不给就全选。
func cmdModelSync(e *Env, args []string) error {
	fs := newFlagSet(e, "model sync", "从上游拉取模型列表")
	provider := fs.String("provider", "", "只同步这个厂商的账号")
	accountName := fs.String("account", "", "只同步这个账号")
	endpoint := fs.String("endpoint", "", "临时覆盖模型列表路径，只影响这一次调用：相对账号 base_url 的完整路径（例如 /v1/models），也可以写整条地址（相对路径的斜杠写法由程序规整）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 取值原样往下走：写法（前导斜杠、重复斜杠、末尾斜杠）由 store 规整，cli 只判两种用法错误：
	// 「规整之后为空」——给了参数却没给路径，去掉这个参数再试；
	// 与「整条地址校验不过」——缺主机名或 scheme 不是 http(s)，判定交给 store.NormalizeEndpoint。
	if flagWasSet(fs, "endpoint") {
		if !pathGiven(*endpoint) {
			return usagef("--endpoint %q 里没有路径：它是相对该厂商 base_url 的完整路径，例如 switchboard model sync --endpoint /v1/models；不想临时覆盖就整个去掉这个参数", *endpoint)
		}
		if err := checkEndpoint(*endpoint, "用法：switchboard model sync [--provider 厂商] [--account 账号] [--endpoint <路径>]"); err != nil {
			return err
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
		// 抓取走网关、写库走 store 的同步入口（store.ReplaceModels，见 DESIGN 5.13）：
		// 名字进模型目录，这个账号的行标 allowed，已有的可达性与协议发现一律保留。
		// cli 不拼 SQL，也不自己决定写哪张表。
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
	// reachableByAccount 记每个账号探完之后库里 access='allowed' 的行数：
	// 选号只看这一列（见 DESIGN 5.13），探活不再另写一份模型清单。
	reachableByAccount := map[string]int{}
	authFailed := ""
	syncFailed := 0
	// 零可用时的错误文案要能分清「全被跳过」与「探了都不行」，
	// 所以整轮累加探活数与跳过数，不只是当前账号的。
	totalProbed, totalSkipped := 0, 0

	// 实时进度只画在终端里的 stderr 上：结果照旧走 stdout，--json 时一行都不出。
	progress := newProbeProgress(e.Stderr, !e.JSON)

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
			// 新鲜度判定说的是旧状态词表（account.SettledFresh）：先把可达性映射回去——
			// allowed 探过就是 usable、没探过是 synced，forbidden / missing 有定论，error 没有。
			if freshEnabled && account.SettledFresh(store.LegacyStatus(c.access, c.checkedAt), c.checkedAt, now, *maxAge) {
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
			// 待探模型为 0（全被跳过）时一帧都不画：没有可报的进度，也没必要留一行残影。
			progress.update(acc.Name, 0, len(targets), targets[0])
			results, err = func() ([]gateway.ModelProbe, error) {
				// 出错返回也要收尾，否则「model probe: …」会接在进度行尾巴上。
				defer progress.finish()
				return probeModels(ctx, gs, a.st, acc, targets, *concurrency, func(done int, model string) {
					progress.update(acc.Name, done, len(targets), model)
				})
			}()
			if err != nil {
				return err
			}
		}

		for _, res := range results {
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

		if probeAuthFailed(results) {
			// 凭据不行了，后续账号接着探只会重复同一条错误：停下，
			// 但已经探出来的行仍要打印，人得看得见是哪一步卡住的。
			authFailed = acc.Name
			break
		}

		// 探活结论写的就是可达性，因此库里全部 allowed 行就是选号集合：
		// 用 --limit 或 --model 只探一部分时，之前探通的模型仍是 allowed，不会被这回探掉。
		// 待探模型被全部跳过时也走这里：结论都还在新鲜期内，不该因为「这次什么都没探」报错。
		after, err := a.st.AccountModels(acc.ID)
		if err != nil {
			return err
		}
		access := make(map[string]string, len(after))
		reachable := 0
		for _, r := range after {
			access[r.Model] = r.Access
			if r.Access == store.AccessAllowed {
				reachable++
			}
		}
		// 本轮各结论的计数取自库里刚写的可达性，不认网关的状态词表：词表换了这几列也不跟着变。
		counts := map[string]int{}
		for _, res := range results {
			counts[access[res.Model]]++
		}
		if reachable == 0 {
			// 零可用不再意味着「清单原样不动」：可达性已经按这一轮的结论写进 account_models，
			// 进不了候选的是 forbidden / missing 那些行。
			if !e.JSON {
				fmt.Fprintf(e.Stdout, "%s：本次探活 %d 个，跳过 %d 个，库里没有可用模型；可达性已记录，access 为 forbidden / missing 的模型不会进候选\n",
					acc.Name, len(results), skipped)
			}
			continue
		}
		reachableByAccount[acc.Name] = reachable
		if !e.JSON {
			fmt.Fprintf(e.Stdout, "%s：本次探活 %d 个，跳过 %d 个，可用 %d，无权 %d，不存在 %d，失败 %d；可达 %d 个\n",
				acc.Name, len(results), skipped, counts[store.AccessAllowed], counts[store.AccessForbidden],
				counts[store.AccessMissing], counts[store.AccessError], reachable)
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
		return fmt.Errorf("账号 %s 的凭据被上游拒绝，账号已停用；可达性已写入，其余账号不再处理", authFailed)
	}
	if len(reachableByAccount) == 0 {
		return fmt.Errorf("%s", zeroUsableReason(totalProbed, totalSkipped, syncFailed))
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
	// 零可用不等于「什么都没写」：可达性已经按这一轮的结论落库，进不了候选的是
	// access 为 forbidden / missing 的那些行（见 DESIGN 5.13）。
	return msg + "；可达性已记录，access 为 forbidden / missing 的模型不会进候选"
}

// probeCandidate 是一个待判定的模型。access 与 checkedAt 取库里已有结论，
// 没有对应行时为零值，即「从未探过」。
type probeCandidate struct {
	model     string
	access    string
	checkedAt int64
}

// probeCandidates 列出这一轮要考虑的模型。给了 --model 就只算那一个（库里没行也照探），
// 否则按库里的模型行。
func probeCandidates(rows []store.AccountModel, only string) []probeCandidate {
	if only != "" {
		c := probeCandidate{model: only}
		for _, r := range rows {
			if r.Model == only {
				c.access, c.checkedAt = r.Access, r.CheckedAt
				break
			}
		}
		return []probeCandidate{c}
	}
	out := make([]probeCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, probeCandidate{model: r.Model, access: r.Access, checkedAt: r.CheckedAt})
	}
	return out
}

// probeAuthFailed 判断这一轮里有没有凭据被拒：口径是上游回 401（见 DESIGN 5.7）。
// 只看 HTTP 状态，不认状态词表——词表由网关给，换成 access 那套也不影响这条判定。
func probeAuthFailed(results []gateway.ModelProbe) bool {
	for _, res := range results {
		if res.HTTPStatus == http.StatusUnauthorized {
			return true
		}
	}
	return false
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
// onDone 非空时每探完一个模型调用一次，参数是已完成数与刚完成的模型名；
// 调用来自任意 worker goroutine，实现要自己保证并发安全。
func probeModels(ctx context.Context, gs *gateway.Server, st *store.Store,
	acc store.Account, targets []string, concurrency int,
	onDone func(done int, model string)) ([]gateway.ModelProbe, error) {

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
	// 已完成数跨 goroutine 累加，原子加之后再回调，进度里的分子只会往上走。
	var done atomic.Int64
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
			if onDone != nil {
				onDone(int(done.Add(1)), model)
			}
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

// cachePriceText 是缓存单价的展示：未填时不是一个数，而是「回退输入价」这个实际行为，
// 写成 0.00 会让人以为缓存读不花钱（见 DESIGN 5.5 的公式）。
func cachePriceText(v *float64) string {
	if v == nil {
		return "回退输入价"
	}
	return fmt.Sprintf("%.2f", *v)
}

// 价格参数的四个名字。校验、拼库与输出都读这一份，名字写错时不会静默少改一列。
const (
	priceFlagIn         = "price-in"
	priceFlagOut        = "price-out"
	priceFlagCacheRead  = "price-cache-read"
	priceFlagCacheWrite = "price-cache-write"
)

// priceFlags 是一组价格参数的取值：name 用于错误文案，value 是解析出来的数。
type priceFlag struct {
	name  string
	value float64
}

// knownPriceFlags 收集命令行上真的给过的价格参数，供 checkPrices 校验。
// 没给的不进列表：没给就是「不写库」，不该因为默认值 0 而被当成「填了 0 元」。
func knownPriceFlags(fs *flagSet, in, out, cacheRead, cacheWrite *float64) []priceFlag {
	all := []priceFlag{
		{priceFlagIn, *in},
		{priceFlagOut, *out},
		{priceFlagCacheRead, *cacheRead},
		{priceFlagCacheWrite, *cacheWrite},
	}
	given := make([]priceFlag, 0, len(all))
	for _, p := range all {
		if flagWasSet(fs, p.name) {
			given = append(given, p)
		}
	}
	return given
}

// checkPrices 拒绝负价格：单价是「每百万 token 多少钱」，负数是用法错误，不是折扣。
// 0 是合法值（本地模型就是免费），因此只查 < 0。
func checkPrices(usageLine string, flags ...priceFlag) error {
	for _, p := range flags {
		if p.value < 0 {
			return usagef("--%s 不能是负数（单位是每百万 token 的价格），现在是 %v\n%s", p.name, p.value, usageLine)
		}
	}
	return nil
}

// anyPriceFlagSet 报告这次调用里有没有价格参数：account set 靠它决定要不要要求 --key。
func anyPriceFlagSet(fs *flagSet) bool {
	for _, name := range []string{priceFlagIn, priceFlagOut, priceFlagCacheRead, priceFlagCacheWrite} {
		if flagWasSet(fs, name) {
			return true
		}
	}
	return false
}

// pricePatch 是只给过的那一项变成的写入值：nil 表示不动这一列（store.SetAccountPrices 的增量语义）。
func pricePatch(fs *flagSet, name string, v *float64) *float64 {
	if !flagWasSet(fs, name) {
		return nil
	}
	value := *v
	return &value
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
