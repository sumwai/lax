package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"lax/internal/account"
	"lax/internal/gateway"
	"lax/internal/secret"
	"lax/internal/store"
)

// accountSubs 是 account 一组的子命令清单，组级帮助与错误提示都读它。
var accountSubs = []groupItem{
	{"add", "新增上游账号并加密存储凭据"},
	{"ls", "列出账号、模型数与当前状态"},
	{"set", "更新账号凭据，重置冷却与失败计数并重新启用"},
	{"rm", "删除账号（保留历史请求记录）"},
	{"test", "检测账号连通性与凭据有效性"},
	{"enable", "启用账号并清除冷却状态"},
	{"disable", "停用账号（不再参与选号）"},
}

func cmdAccountSetEnabled(e *Env, fs *leafFlags, args []string, sub string, enabled bool) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax account %s <名称>", sub)
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
		if err := a.st.ClearFailures(acc.ID); err != nil {
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
const accountSetUsage = "用法：lax account set <名称> [--key <key>]\n" +
	"--key 三种给法：直接写值、写 - 从标准输入读、留空读环境变量 LAX_KEY\n" +
	"价格不在账号上：模型单价属于「供应商 × 模型」，改价用 lax model price set <模型> --in N --out N"

// retiredAccountPriceFlags 是从 account add 与 account set 上删掉的四个价格参数。
// 单价搬到模型目录之后账号不再是价格的落点：同一家上游的同一个模型只该有一份标价，
// 账号只在套餐不同时覆盖。误敲这四个参数仍是用法错误（退出码 2），文案说清它去了哪、下一步跑什么。
const retiredAccountPriceHint = "模型单价已经只存在模型目录上：同一家上游的同一个模型只该有一份标价，账号不再是价格的落点；改价用 lax model price set <模型> --in N --out N，套餐不同时才对某个账号加 --account 覆盖"

var retiredAccountPriceFlags = map[string]string{
	"--price-in":          retiredAccountPriceHint,
	"--price-out":         retiredAccountPriceHint,
	"--price-cache-read":  retiredAccountPriceHint,
	"--price-cache-write": retiredAccountPriceHint,
}

// cmdAccountSet 换掉一个已有账号的凭据，不删号重建：删了会丢掉探活结果、协议发现与请求记录归属。
// 换 key 之后旧凭据的结论作废，同一个事务里清掉冷却、最近错误与连续失败次数并重新启用账号。
// 价格不在这条命令上：单价属于「供应商 × 模型」，改价走 model price set。
// `account_models` 里的可达性、覆盖值与协议发现结果一个字不动，也不碰厂商表与模型目录。
func cmdAccountSet(e *Env, fs *leafFlags, args []string) error {
	if err := rejectRetiredFlags(fs, retiredAccountPriceFlags, accountSetUsage); err != nil {
		return err
	}
	key := fs.String("key", "", "新的上游 API key；给 - 从标准输入读，留空则读环境变量 LAX_KEY")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usagef("%s", accountSetUsage)
	}
	name := fs.Arg(0)

	// 换凭据是这条子命令唯一的作用，因此在动库之前就要能解析出凭据。
	// 三种给法与 account add 完全一致，共用 resolveKey：两处判定不一致时，换 key 会突然多出一个坑。
	keyValue, err := resolveKey(*key, "lax account set "+name+" --key - < key.txt", accountSetUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	acc, err := a.st.GetAccount(name)
	if err != nil {
		// 改动针对已有账号：不存在时给下一步命令，不要只抛一句 sql.ErrNoRows。
		return fmt.Errorf("账号 %q 不存在，先运行 lax account add %s --provider <厂商> --key <key>\n%s", name, name, accountSetUsage)
	}

	// 与 account add 走同一条加密路径；明文不进日志，也不进 --json 输出（那里只有掩码）。
	cipher, err := secret.Encrypt(a.key, []byte(keyValue))
	if err != nil {
		return err
	}
	if err := a.st.RotateKey(acc.ID, cipher); err != nil {
		return fmt.Errorf("更新 %s 的凭据失败: %w", name, err)
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"name":                name,
			"key_rotated":         true,
			"key":                 secret.Mask(keyValue),
			"cooldown_cleared":    true,
			"fail_streak_cleared": true,
			"enabled":             true,
		})
	}
	// 厂商是厂商行派生的只读值，确认行里带上它才知道换的是哪个上游的凭据。
	fmt.Fprintf(e.Stdout, "已更新 %s（%s）的凭据（%s）；冷却与失败计数已清零，账号已启用\n",
		name, dashIfEmpty(acc.Provider), secret.Mask(keyValue))
	return nil
}

// accountAddUsage 是 account add 的用法行，用法错误与厂商不在表里时都打出来。
const accountAddUsage = "用法：lax account add <名称> --provider <厂商> --key <key> [--models <模型>] [--priority N]\n" +
	"地址与默认协议不落在账号上：先 lax provider add <厂商> --base-url <地址> --openai /v1/chat/completions 登记厂商，再建账号\n" +
	"--models 手填的名字写进模型目录（不带长度与价格），并把这个账号标成可用；留空则等 model sync 与 model probe"

// retiredAccountAddFlags 是已经从 account add 上删掉的两个参数与各自的替代做法：
// 地址与默认协议只在厂商行上，账号不再持有副本。误用时给一句指出下一步的用法错误，
// 而不是让人对着「参数不存在」自己找原因。
var retiredAccountAddFlags = map[string]string{
	"--base-url": "地址只在厂商行上，先用 lax provider add <厂商> --base-url <地址> --openai /v1/chat/completions 登记厂商",
	"--protocol": "默认协议只在厂商行上，登记厂商时用 --default-protocol 指定，或改用 lax provider set <厂商> --default-protocol <协议>",
}

// rejectRetiredFlags 在 flag 包之前拦下已经删掉的参数：flag 只会说「参数不认识」，
// 这里能说清它去了哪、下一步该跑什么命令。`--名字 值` 与 `--名字=值` 两种写法都拦。
// rejectRetiredFlags 把已经删掉的参数登记成隐藏参数：pflag 不会再说「参数不认识」，
// 执行时再按 Changed 判出误用，给出它去了哪、下一步该跑什么命令。
// `--名字 值` 与 `--名字=值` 两种写法都能被 pflag 收下。
func rejectRetiredFlags(fs *leafFlags, retired map[string]string, usageLine string) error {
	names := make([]string, 0, len(retired))
	for name := range retired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flagName := strings.TrimPrefix(name, "--")
		fs.String(flagName, "", "")
		_ = fs.cmd.Flags().MarkHidden(flagName)
	}
	for _, name := range names {
		if fs.Changed(strings.TrimPrefix(name, "--")) {
			return usagef("%s 已经不在这个子命令上：%s\n%s", name, retired[name], usageLine)
		}
	}
	return nil
}

func cmdAccountAdd(e *Env, fs *leafFlags, args []string) error {
	if err := rejectRetiredFlags(fs, retiredAccountAddFlags, accountAddUsage); err != nil {
		return err
	}
	if err := rejectRetiredFlags(fs, retiredAccountPriceFlags, accountAddUsage); err != nil {
		return err
	}
	provider := fs.String("provider", "", "上游厂商标识，地址与默认协议都从这家厂商读，如 deepseek")
	key := fs.String("key", "", "上游 API key；给 - 从标准输入读，留空则读环境变量 LAX_KEY")
	name := fs.String("name", "", "账号名称，也可以用位置参数给出")
	models := fs.String("models", "", "该账号可用的模型，逗号分隔：名字进模型目录，账号侧记一条可用")
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
		return usagef("--provider 必填，例如 --provider deepseek\n%s", accountAddUsage)
	}

	keyValue, err := resolveKey(*key, "lax account add x --provider p --key - < key.txt", accountAddUsage)
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 地址与默认协议只在厂商行上：账号只认厂商名，厂商不在表里就没有
	// 地址可拼、没有协议可推导，先在厂商表里登记它。厂商行在但一条路由都没声明不算错：
	// 转发时按厂商默认协议推导，推导为空由转发层报错。
	if _, err := a.st.GetProvider(*provider); err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			return unknownProviderError(a.st, *provider)
		}
		return err
	}

	if _, err := a.st.GetAccount(accountName); err == nil {
		return fmt.Errorf("账号 %q 已存在，换个名称或先运行 lax account rm %s", accountName, accountName)
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
		// 价格不在这里：单价属于「供应商 × 模型」，写入口是 model price set。
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
			// 给了 --models 就是那些名字（先写目录，再标这个账号可用）。
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
	for _, name := range []string{"LAX_KEY", "LAX_API_KEY"} {
		if v := lookupEnv(name); v != "" {
			return v
		}
	}
	return ""
}

// resolveKey 把 --key 的三种给法解成一个值：直接写值、`-`（或 `--`）从标准输入读、
// 留空读环境变量 LAX_KEY（兼容 LAX_API_KEY）。account add 与 account set 共用一份，
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
		return "", usagef("缺少凭据：--key <key>、--key - 从标准输入读，或环境变量 LAX_KEY\n%s", usageLine)
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

func cmdAccountLs(e *Env, fs *leafFlags, args []string) error {
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
	now := time.Now()
	counts := countModels(modelCounts, now)

	if e.JSON {
		out := make([]map[string]any, 0, len(accounts))
		for _, acc := range accounts {
			c := counts[acc.ID]
			// protocol 与 base_url 都是厂商行派生的只读值：协议取厂商的有效默认协议
			// 协议取厂商的有效默认协议，地址取厂商的 base_url，厂商行不在时都是空串。
			out = append(out, map[string]any{
				"name":              acc.Name,
				"provider":          acc.Provider,
				"protocol":          acc.Protocol,
				"base_url":          acc.BaseURL,
				"models":            acc.Models,
				"priority":          acc.Priority,
				"enabled":           acc.Enabled,
				"status":            accountStatus(acc, counts[acc.ID], now),
				"cooldown_until_ms": acc.CooldownUntilMS,
				"cooling_models":    counts[acc.ID].Cooling,
				"next_cooldown_ms":  counts[acc.ID].NextCooldownMS,
				"last_error":        acc.LastError,
				// 与文本输出的「N 可用 / M 已登记」同一组计数：两个数都来自 account_models，
				// usable 数 access='allowed' 的行，synced 数全部行。
				"models_usable": c.Usable,
				"models_synced": c.Total,
				// 默认协议在前，其余是探活发现到的、与默认不同的协议。
				"protocols": allProtocols(acc.Protocol, c.Protocols),
			})
		}
		return e.printJSON(out)
	}

	if len(accounts) == 0 {
		fmt.Fprintln(e.Stdout, "还没有账号。运行 lax account add <名称> --provider <厂商> --key <key>")
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
			accountStatus(acc, counts[acc.ID], now),
		})
	}
	fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
		Header:    []string{"名称", "厂商", "地址", "协议", "模型", "优先级", "状态"},
		Align:     []tableAlign{alignLeft, alignLeft, alignLeft, alignLeft, alignLeft, alignRight, alignLeft},
		Rows:      rows,
		Style:     newUI(e.Stdout),
		HeaderSep: true,
	}))
	return nil
}

// modelCount 是一个账号在 account_models 里的汇总。
// Usable 是 access='allowed' 的行数（能不能进候选只看这一列），
// Total 是这个账号在 account_models 里的全部行数：同步、手填与探活都会留行。
// Protocols 是模型行上发现过的协议，升序去重，含与账号默认协议相同的那些。
type modelCount struct {
	Total          int
	Usable         int
	Cooling        int
	NextCooldownMS int64
	Protocols      []string
}

func countModels(rows []store.AccountModel, now time.Time) map[int64]modelCount {
	out := map[int64]modelCount{}
	seen := map[int64]map[string]bool{}
	for _, r := range rows {
		c := out[r.AccountID]
		c.Total++
		if r.Access == store.AccessAllowed {
			c.Usable++
		}
		if r.CooldownUntilMS > now.UnixMilli() {
			c.Cooling++
			if r.CooldownUntilMS > c.NextCooldownMS {
				c.NextCooldownMS = r.CooldownUntilMS
			}
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

// accountStatus 是 account ls 的状态列，优先级固定：
// 停用 > 账号级冷却 > 账号-模型冷却 > 正常。账号级冷却来自失败处理策略的
// --cooldown（没带 --model），会挡住整个账号；模型冷却只影响对应模型。
func accountStatus(acc store.Account, c modelCount, now time.Time) string {
	if !acc.Enabled {
		return "停用"
	}
	if acc.CooldownUntilMS > now.UnixMilli() {
		return "账号冷却中（还剩 " + routePolicyRemainingText(acc.CooldownUntilMS, now) + "）"
	}
	if c.Cooling > 0 {
		return fmt.Sprintf("%d 个模型冷却", c.Cooling)
	}
	return "正常"
}

func cmdAccountRm(e *Env, fs *leafFlags, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax account rm <名称>")
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
// 它只判凭据与连通性，某个模型能不能用由 `model probe` 决定。
func cmdAccountTest(e *Env, fs *leafFlags, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax account test <名称>")
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
		payload := map[string]any{
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
		}
		// 没命中策略时不加这两个键，旧输出形状保持不变；命中时把来源、id、名称与动作带给脚本。
		if res.Policy != nil {
			payload["policy"] = res.Policy
			payload["policy_action"] = res.PolicyAction
		}
		if err := e.printJSON(payload); err != nil {
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
		fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
			Header:    []string{"账号", "状态", "HTTP", "耗时", "上游模型数", "备注"},
			Align:     []tableAlign{alignLeft, alignLeft, alignRight, alignRight, alignRight, alignLeft},
			Rows:      rows,
			Style:     newUI(e.Stdout),
			HeaderSep: true,
		}))
		if res.Policy != nil {
			fmt.Fprintf(e.Stdout, "命中策略：%s（%s）\n", probePolicyDisplay(res.Policy), res.PolicyAction)
		}
	}

	if !res.OK {
		return fmt.Errorf("账号 %s 探活失败（%s，%d）：%s", res.Account, probeStageLabel(res.Stage), res.Status, res.Message)
	}
	return nil
}

// probePolicyDisplay 是 account test 人读输出里命中规则的称呼：
// 内置规则用它的名字，用户规则带 id 与备注，与 gateway 写进错误里的说法一致。
func probePolicyDisplay(p *gateway.ProbePolicy) string {
	if p == nil {
		return ""
	}
	if p.Source == account.FailureSourceBuiltin {
		return p.Name
	}
	if strings.TrimSpace(p.Name) != "" {
		return fmt.Sprintf("用户规则 #%d（%s）", p.ID, p.Name)
	}
	return fmt.Sprintf("用户规则 #%d", p.ID)
}

// probeStageLabel 把阶段标识翻成给人看的说法：备注与错误文案里说清结论出自哪一步。
func probeStageLabel(stage string) string {
	if stage == gateway.ProbeStageModels {
		return "列表接口"
	}
	return "模型调用"
}

// splitModels 把 --models 的逗号分隔写法拆成名字列表，空项直接丢掉。
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
