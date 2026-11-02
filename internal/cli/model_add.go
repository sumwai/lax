package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"lax/internal/store"
)

// modelAddUsage 是 model add 的用法行：用法错误都打出来，用户不必回头猜语法。
const modelAddUsage = "用法：lax model add <模型> --account <账号> [--provider <厂商>] [--protocol <协议>]\n" +
	"手写一条「这个账号能用这个模型」：同一个事务写模型目录与账号可达性，写完就能选号，不需要先跑 model sync 或探活\n" +
	"--account 必填（可达性是账号侧的结论）；--provider 不给就取该账号所属厂商；--protocol 只收线协议，不给就只写可达性、不动协议"

// cmdModelAdd 手写一条「这个账号能用这个模型」。
//
// 一次事务写两张表的事全在 store.DeclareAccountModel 里，cli 不拼 SQL、也不自己决定写哪张表。
// 写完再读一遍这一行：可达性状态与有效协议都取库里的派生值，输出与 `model show` 说的是同一份事实。
func cmdModelAdd(e *Env, args []string) error {
	fs := newFlagSet(e, "model add", "手写一条账号能用的模型（同事务写目录与可达性）")
	accountName := fs.String("account", "", "账号名，必填：可达性写在账号上，没有账号就没有可写的那一行")
	provider := fs.String("provider", "", "厂商名，可省略；给了必须与账号所属厂商一致")
	protocolFlag := fs.String("protocol", "", "这个模型在这个账号上的协议，只收线协议；不给就只写可达性、不动协议")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("参数里要有且只有一个模型名，例如 lax model add typesafe/jev --account ts\n%s", modelAddUsage)
	}
	model := strings.TrimSpace(fs.Arg(0))

	if strings.TrimSpace(*accountName) == "" {
		return usagef("缺少 --account：可达性是账号侧的结论，模型名单独一条写不到任何行上，例如 --account ts\n%s", modelAddUsage)
	}
	accName := strings.TrimSpace(*accountName)

	if flagWasSet(fs, "protocol") && strings.TrimSpace(*protocolFlag) == "" {
		return usagef("--protocol 传了空值：它要么写一条线协议，要么整个去掉（不给就只写可达性、不动协议）；清掉已有的协议发现用 lax model probe --account %s --model %s 重探\n%s",
			accName, model, modelAddUsage)
	}
	protocol, err := protocolFromCLIName(*protocolFlag, "--protocol", "lax model add typesafe/jev --account ts --protocol systemone")
	if err != nil {
		return usagef("%v\n%s", err, modelAddUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	acc, err := a.st.GetAccount(accName)
	if err != nil {
		return fmt.Errorf("账号 %q 不存在，模型的可达性写在账号上，没有账号就没有可写的那一行：先运行 lax account add %s --provider <厂商> --key <key> 建账号，再跑 lax model add %s --account %s",
			accName, accName, model, accName)
	}
	// 目录的主键是 (厂商, 模型)，账号只记厂商名：给了 --provider 就必须与账号一致，
	// 否则这条可达性会被记到一个账号根本不属于的厂商目录下。
	if p := strings.TrimSpace(*provider); p != "" && p != acc.Provider {
		return fmt.Errorf("账号 %s 的厂商是 %s，与 --provider %s 不符：模型目录按 (厂商, 模型) 建行，厂商对不上就写不到这个账号的目录行；去掉 --provider（默认取账号所属厂商）或改成 --provider %s 再试",
			acc.Name, acc.Provider, p, acc.Provider)
	}

	res, err := a.st.DeclareAccountModel(acc.ID, model, protocol)
	if err != nil {
		return fmt.Errorf("声明模型 %s 失败（账号 %s，厂商 %s）：%v；用 lax model show %s 看现有行，或用 lax doctor 检查数据库",
			model, acc.Name, acc.Provider, err, model)
	}

	// 写完再读一次：状态文本与有效协议都取库里的值，与 `model show`、`model ls` 说的是同一份事实。
	rows, err := a.st.AccountModels(acc.ID)
	if err != nil {
		return err
	}
	row, ok := findAccountModel(rows, model)
	if !ok {
		return fmt.Errorf("写完读回时没找到账号 %s 的模型 %s 那一行：写入可能没落盘；用 lax model show %s 核对，或用 lax doctor 检查数据库",
			acc.Name, model, model)
	}
	effective, err := a.st.EffectiveProtocol(acc.ID, model)
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":    model,
			"provider": acc.Provider,
			"account":  acc.Name,
			// access 是库里那一行的原文（allowed），不是给人看的「可用 / 待探活」。
			"access": row.Access,
			// protocol 是有效协议（账号覆盖 → 目录协议集合 → 厂商默认协议），算不出来时是空串。
			"protocol": effective,
			// declared_protocol 是这次参数写进账号级的协议，没给就是空串（协议没动）。
			"declared_protocol": protocol,
			// created 表示 account_models 这一行是不是这次新建的，重复执行时为 false。
			"created": res.Created,
		})
	}

	fmt.Fprintf(e.Stdout, "已声明模型 %s（厂商 %s，账号 %s，%s，协议 %s）\n",
		model, acc.Provider, acc.Name,
		modelAccessText(row.Access, row.CheckedAt, row.CooldownUntilMS, time.Now()), dashIfEmpty(effective))
	// 下一步只探这一条路：声明过的协议排在候选最前面，写出来省掉一次猜。
	next := fmt.Sprintf("下一步：lax model probe --account %s --model %s", acc.Name, model)
	if effective != "" {
		next += " --protocol " + effective
	}
	fmt.Fprintln(e.Stdout, next)
	return nil
}

// findAccountModel 在账号的模型行里找某一个模型。
func findAccountModel(rows []store.AccountModel, model string) (store.AccountModel, bool) {
	for _, r := range rows {
		if r.Model == model {
			return r, true
		}
	}
	return store.AccountModel{}, false
}

// modelShowUsage 是 model show 的用法行。
const modelShowUsage = "用法：lax model show <模型> [--provider 厂商] [--account 账号]\n" +
	"只读库：列这个模型在各账号上的有效协议、可达性、HTTP 状态、探活时间与错误，不发上游请求，也不改可达性\n" +
	"一行都没有时先 lax model sync 拉清单，或用 lax model add <模型> --account <账号> --protocol <协议> 手写一条"

// modelShowRow 是 model show --json 的一行：一个账号上一个模型的可达性事实。
type modelShowRow struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Access 是 account_models.access 的原文（allowed / forbidden / missing / error）。
	Access   string `json:"access"`
	Protocol string `json:"protocol"`
	// HTTPStatus 为 0 表示没探过（手写与同步的行就是这一档），不是「上游回了 0」。
	HTTPStatus int `json:"http_status"`
	// CheckedAt 是 Unix 秒，0 表示没探过。
	CheckedAt int64 `json:"checked_at"`
	// CooldownUntilMS 是这个账号-模型的冷却截止时间，Unix 毫秒；0 表示没在冷却。
	CooldownUntilMS int64  `json:"cooldown_until_ms"`
	FailStreak      int    `json:"fail_streak"`
	Error           string `json:"error"`
}

// cmdModelShow 按账号列出一个模型的可达性事实。只读库：不发上游请求，也不写可达性。
func cmdModelShow(e *Env, args []string) error {
	fs := newFlagSet(e, "model show", "按账号看一个模型的可达性事实")
	provider := fs.String("provider", "", "只看这个厂商的账号")
	accountName := fs.String("account", "", "只看这个账号")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return usagef("参数里要有且只有一个模型名，例如 lax model show typesafe/jev\n%s", modelShowUsage)
	}
	model := strings.TrimSpace(fs.Arg(0))

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	// 账号与厂商的筛选判定与 model sync / model probe 同一套（selectAccounts）：
	// 账号写错、厂商对不上在这里报错，而不是显示成「这个模型一行都没有」。
	accounts, err := a.st.ListAccounts()
	if err != nil {
		return err
	}
	if *provider != "" || *accountName != "" {
		accounts, err = selectAccounts(a.st, *provider, *accountName)
		if err != nil {
			// 判定与 model sync / model probe 同一套，只补一条下一步：这一条是只读视图，
			// 筛不到行时先看清单里到底有哪些账号与厂商。
			return fmt.Errorf("%v；用 lax account ls 看账号名与厂商，或去掉 --provider / --account 看全部账号", err)
		}
	}
	selected := make(map[int64]store.Account, len(accounts))
	for _, acc := range accounts {
		selected[acc.ID] = acc
	}

	all, err := a.st.AllAccountModels()
	if err != nil {
		return err
	}
	rows := make([]modelShowRow, 0, len(all))
	for _, r := range all {
		if r.Model != model {
			continue
		}
		acc, ok := selected[r.AccountID]
		if !ok {
			continue
		}
		// 有效协议只问 store.EffectiveProtocol：账号覆盖 → 目录协议集合 → 厂商默认协议，
		// cli 不重算一份合并规则。
		protocol, err := a.st.EffectiveProtocol(r.AccountID, model)
		if err != nil {
			return err
		}
		rows = append(rows, modelShowRow{
			Account:         acc.Name,
			Provider:        acc.Provider,
			Model:           r.Model,
			Access:          r.Access,
			Protocol:        protocol,
			HTTPStatus:      r.HTTPStatus,
			CheckedAt:       r.CheckedAt,
			CooldownUntilMS: r.CooldownUntilMS,
			FailStreak:      r.FailStreak,
			Error:           r.Error,
		})
	}

	if len(rows) == 0 {
		return fmt.Errorf("模型 %s 在任何账号上都没有可达性记录：model show 只读 account_models，一行都没有说明这个模型还没被同步下来、没被探活，也没被手写声明；先运行 lax model sync 拉上游模型清单，或 lax model add %s --account <账号> --protocol <协议> 手写一条",
			model, model)
	}

	if e.JSON {
		return e.printJSON(rows)
	}

	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.Account,
			r.Model,
			dashIfEmpty(r.Protocol),
			modelAccessText(r.Access, r.CheckedAt, r.CooldownUntilMS, time.Now()),
			httpStatusCell(r.HTTPStatus),
			checkedAtCell(r.CheckedAt),
			dashIfEmpty(r.Error),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"账号", "模型", "协议", "状态", "HTTP", "探活时间", "备注"}, table))
	return nil
}

// httpStatusCell 是 HTTP 状态列：0 表示这次没探过（或没拿到响应），写 - 而不是 0。
func httpStatusCell(code int) string {
	if code == 0 {
		return "-"
	}
	return strconv.Itoa(code)
}

// checkedAtCell 是探活时间列：0 写「未探活」，其余按本地时间写到分钟。
// 手写与同步的行没有探活时间，那不是错误，所以不写空白也不写 1970。
func checkedAtCell(ts int64) string {
	if ts == 0 {
		return "未探活"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}
