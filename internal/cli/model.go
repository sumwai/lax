package cli

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lax/internal/account"
	"lax/internal/gateway"
	"lax/internal/store"
)

// modelSubs 是 model 一组的子命令清单。
var modelSubs = []groupItem{
	{"ls", "列出可用模型、账号状态与目录元数据"},
	{"show", "查看模型在各账号下的可达性详情"},
	{"add", "手动声明账号可用模型（同步写入目录与可达性）"},
	{"sync", "从上游拉取模型列表并写入目录"},
	{"probe", "探测模型连通性并更新可达性状态"},
	{"alias", "配置模型别名映射（set / unset / ls）"},
	{"price", "管理模型单价与账号覆盖价（set / unset / ls）"},
	{"capability", "声明模型能力位、音色与音频格式（add / set / unset / ls）"},
	{"dialect", "管理模型方言绑定（ls / set / unset）"},
	{"call", "发送对话试打请求并输出选号过程"},
	{"decide", "发送 systemone 决策试打请求并输出结果"},
	{"speak", "发送语音合成试打请求"},
	{"transcribe", "发送语音转写试打请求"},
}

// modelSubCommands 是「可用子命令」那条错误文案里的清单，与 modelSubs 同序。
const modelSubCommands = "ls | show | add | sync | probe | alias | price | capability | dialect | call | decide | speak | transcribe"

func cmdModelLs(e *Env, fs *leafFlags, args []string) error {
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

	// 支持的协议集合只在目录里：账号侧没有协议集合的覆盖值，合并只发生在长度与价格上。一家厂商的目录读一次就够，按模型名索引。
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
		// 协议按账号覆盖 → 目录里的协议集合 → 厂商默认协议定序。
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
					model: r.Model,
					// 价格列读合并之后的单价（账号覆盖 → 厂商级标价）：
					// 单价不在 accounts 上，账号的 PriceIn 只是历史遗留，不再是这一列的来源。
					priceIn:       info.PriceIn,
					priceOut:      info.PriceOut,
					contextWindow: info.ContextWindow,
					maxOutput:     info.MaxOutput,
					protocols:     cat[r.Model].Protocols,
				}
				byModel[r.Model] = ent
				order = append(order, r.Model)
			}
			ent.accounts = append(ent.accounts, modelAccountJSON{
				Name:            acc.Name,
				Status:          info.Access,
				Protocol:        info.Protocol,
				HTTPStatus:      r.HTTPStatus,
				CheckedAt:       r.CheckedAt,
				CooldownUntilMS: info.CooldownUntilMS,
				FailStreak:      info.FailStreak,
				Error:           r.Error,
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
		fmt.Fprintln(e.Stdout, "还没有模型信息：先 lax model sync 拉上游模型列表，再 lax model probe 逐个探活；也可以用 lax model add <模型> --account <账号> --protocol <协议> 手写一条（决策模型只能这样声明）")
		return nil
	}

	rows := make([][]string, 0, len(order))
	for _, m := range order {
		ent := byModel[m]
		names := make([]string, 0, len(ent.accounts))
		for _, ae := range ent.accounts {
			names = append(names, fmt.Sprintf("%s(%s)", ae.Name, modelAccessText(ae.Status, ae.CheckedAt, ae.CooldownUntilMS, time.Now())))
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
	fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
		Header:    []string{"模型", "账号", "协议", "上下文", "最大输出", "支持协议", "价格 (/1M)"},
		Align:     []tableAlign{alignLeft, alignLeft, alignLeft, alignRight, alignRight, alignLeft, alignRight},
		Rows:      rows,
		Style:     newUI(e.Stdout),
		HeaderSep: true,
	}))
	return nil
}

// modelAccountJSON 是 model ls --json 里某个模型下的一个账号。
// Status 取 account_models.access（allowed / forbidden / missing / error），
// Protocol 是该账号在这个模型上的有效出站协议。
// HTTPStatus、CheckedAt 与 Error 是同一行上的探活事实：脚本靠这三列判断「可用」是手写进来
// 还是真打过一条上游请求，不必再跑一次 model show。
type modelAccountJSON struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Protocol        string `json:"protocol"`
	CooldownUntilMS int64  `json:"cooldown_until_ms"`
	FailStreak      int    `json:"fail_streak"`
	// HTTPStatus 为 0 表示没探过（同步、手写与未判定的行就是这一档）。
	HTTPStatus int `json:"http_status"`
	// CheckedAt 是探活时间的 Unix 秒，0 表示没探过。
	CheckedAt int64  `json:"checked_at"`
	Error     string `json:"error"`
}

// modelAccessText 把可达性换成人话。「可用」只给探过的 allowed 行：刚同步进来或手填的
// allowed 行还是一次都没验证过的，写成「可用」会让人以为这些模型已经试通了。
func modelAccessText(access string, checkedAt int64, cooldownUntilMS int64, now time.Time) string {
	if cooldownUntilMS > now.UnixMilli() {
		return "冷却至 " + cooldownClock(cooldownUntilMS)
	}
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

// cooldownClock 把 Unix 毫秒写成带毫秒的本地时间：500 毫秒的冷却如果只显示到秒，
// 同一秒内的截止时间看起来与现在没有区别。
func cooldownClock(ms int64) string {
	return time.UnixMilli(ms).Format("15:04:05.000")
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
// 空串表示这次没拿到足以改可达性的判定（连接层失败、429/5xx、其它 400、本地编码或构造失败），
// 写成「未判定」而不是「失败」：可达性保持上一行结论，这一行不是结论。
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
	case "":
		return "未判定"
	default:
		return status
	}
}

// cmdModelSync 从上游拉模型列表。按 --provider / --account 选账号，不给就全选。
func cmdModelSync(e *Env, fs *leafFlags, args []string) error {
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
			return usagef("--endpoint %q 里没有路径：它是相对该厂商 base_url 的完整路径，例如 lax model sync --endpoint /v1/models；不想临时覆盖就整个去掉这个参数", *endpoint)
		}
		if err := checkEndpoint(*endpoint, "用法：lax model sync [--provider 厂商] [--account 账号] [--endpoint <路径>]"); err != nil {
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
		// 抓取走网关、写库走 store 的同步入口（store.ReplaceModels）：
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
		fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
			Header:    []string{"账号", "厂商", "模型", "新增", "备注"},
			Align:     []tableAlign{alignLeft, alignLeft, alignRight, alignRight, alignLeft},
			Rows:      table,
			Style:     newUI(e.Stdout),
			HeaderSep: true,
		}))
		fmt.Fprintln(e.Stdout, newUI(e.Stdout).dim("下一步: lax model probe"))
	}

	if failed > 0 {
		return fmt.Errorf("%d 个账号同步失败", failed)
	}
	return nil
}

// modelProbeUsage 是 model probe 的用法行，--protocol 的空值与不识协议时都打出来。
const modelProbeUsage = "用法：lax model probe [--provider 厂商] [--account 账号] [--sync] [--model 模型] [--protocol 协议] [--limit N] [--concurrency N] [--max-age 时长] [--refresh]"

// cmdModelProbe 逐个模型探活。按 --provider / --account 选账号，不给就全选。
// --protocol 把候选收窄到一条：只验一条路时不读已发现协议、不读目录声明、不取厂商默认协议。
func cmdModelProbe(e *Env, fs *leafFlags, args []string) error {
	provider := fs.String("provider", "", "只处理这个厂商的账号")
	accountName := fs.String("account", "", "只处理这个账号")
	syncFirst := fs.Bool("sync", false, "探活前先拉一次上游模型列表")
	only := fs.String("model", "", "只探这一个模型")
	protocolFlag := fs.String("protocol", "", "只探这一条协议")
	limit := fs.Int("limit", 0, "每个账号最多探多少个待探模型，0 表示全部")
	concurrency := fs.Int("concurrency", 4, "并发数")
	maxAge := fs.Duration("max-age", 24*time.Hour, "已探过的结论新鲜多久，超期重探；0 与 --refresh 等价")
	refresh := fs.Bool("refresh", false, "忽略新鲜度，全部重探")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if flagWasSet(fs, "protocol") && strings.TrimSpace(*protocolFlag) == "" {
		return usagef("--protocol 传了空值：要么写一条报文协议（例如 systemone），要么整个去掉；去掉时按已发现协议、目录声明与厂商默认协议排候选\n%s", modelProbeUsage)
	}
	protocol, err := protocolFromCLIName(*protocolFlag, "--protocol", "lax model probe --account ts --model typesafe/jev --protocol systemone")
	if err != nil {
		return usagef("%v\n%s", err, modelProbeUsage)
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
	// 选号只看这一列，探活不再另写一份模型清单。
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
				fmt.Fprintf(e.Stdout, "%s：没有可探的模型，先运行 lax model sync --account %s\n", acc.Name, acc.Name)
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
				return probeModels(ctx, gs, a.st, acc, targets, protocol, *concurrency, func(done int, model string) {
					progress.update(acc.Name, done, len(targets), model)
				})
			}()
			if err != nil {
				return err
			}
		}

		for _, res := range results {
			hit := hit{
				Account:    acc.Name,
				Model:      res.Model,
				Status:     res.Status,
				HTTPStatus: res.HTTPStatus,
				LatencyMS:  res.Latency.Milliseconds(),
				Message:    res.Message,
			}
			if res.Undecided {
				// 没拿到足以改可达性的 HTTP 判定：这一行不是结论，status 留空，
				// 人读输出写「未判定」，库里那一行保持原值。
				hit.Status = ""
			}
			hits = append(hits, hit)
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
		reachable := 0
		for _, r := range after {
			if r.Access == store.AccessAllowed {
				reachable++
			}
		}
		// 本轮各结论的计数取自这一轮的探活结果，不读库里那一行：未判定的那几行没写库，
		// 库里留着的是上一轮的结论，拿上一轮的值计数会把「没结论」算成「可用 / 失败」。
		counts := map[string]int{}
		undecided := 0
		for _, res := range results {
			if res.Undecided {
				undecided++
				continue
			}
			counts[res.Access]++
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
			summary := fmt.Sprintf("%s：本次探活 %d 个，跳过 %d 个，可用 %d，无权 %d，不存在 %d，失败 %d",
				acc.Name, len(results), skipped, counts[store.AccessAllowed], counts[store.AccessForbidden],
				counts[store.AccessMissing], counts[store.AccessError])
			// 「未判定」只在这一轮真出现过时追加：没有未判定的输出形状一个字不变。
			if undecided > 0 {
				summary += fmt.Sprintf("，未判定 %d", undecided)
			}
			fmt.Fprintf(e.Stdout, "%s；可达 %d 个\n", summary, reachable)
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
			fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
				Header:    []string{"账号", "模型", "状态", "HTTP", "耗时", "备注"},
				Align:     []tableAlign{alignLeft, alignLeft, alignLeft, alignRight, alignRight, alignLeft},
				Rows:      table,
				Style:     newUI(e.Stdout),
				HeaderSep: true,
			}))
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
		return "没有可探的模型，先运行 lax model sync --account <名称>"
	}
	msg := fmt.Sprintf("没有账号探出可用模型：本次探活 %d 个、跳过 %d 个", probed, skipped)
	if skipped > 0 {
		msg += "（跳过的结论仍在 --max-age 窗口内，用 --refresh 强制重探）"
	}
	if syncFailed > 0 {
		msg += fmt.Sprintf("；另有 %d 个账号同步失败", syncFailed)
	}
	// 零可用不等于「什么都没写」：可达性已经按这一轮的结论落库，进不了候选的是
	// access 为 forbidden / missing 的那些行。
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

// probeAuthFailed 判断这一轮里有没有凭据被拒：口径是上游回 401。
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
// protocol 非空时只探这一条协议（candidates 就是这一条），空串时按已发现协议、目录声明与
// 厂商默认协议排候选。
// onDone 非空时每探完一个模型调用一次，参数是已完成数与刚完成的模型名；
// 调用来自任意 worker goroutine，实现要自己保证并发安全。
func probeModels(ctx context.Context, gs *gateway.Server, st *store.Store,
	acc store.Account, targets []string, protocol string, concurrency int,
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
			var res gateway.ModelProbe
			var err error
			if protocol != "" {
				res, err = gs.ProbeModelProtocol(pctx, acc, model, protocol)
			} else {
				res, err = gs.ProbeModel(pctx, acc, model)
			}
			if err != nil {
				res = mergeProbeError(model, res, err)
			}
			results[i] = res
			// 未判定的一行不是结论：不写库，保留上一行的 access、http_status、error 与 checked_at，
			// 一次 TLS 超时不会把一个已确认的 allowed 抹成 error 而让选号选不到号。
			if res.Undecided {
				if onDone != nil {
					onDone(int(done.Add(1)), model)
				}
				return
			}
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
			return nil, fmt.Errorf("还没有账号，先运行 lax account add")
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
		return "-"
	}
	return fmt.Sprintf("%.2f", *v)
}
