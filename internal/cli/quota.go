package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lax/internal/gateway"
	"lax/internal/quota"
	"lax/internal/store"
)

// quotaSubs 是 quota 一组的子命令清单，组级帮助与错误提示都读它。
var quotaSubs = []groupItem{
	{"ls", "列出各账号的额度快照"},
	{"show", "查看一个账号的额度明细；加 --settings 看监控设置"},
	{"refresh", "手动刷新一个账号或全部账号的额度"},
	{"set", "修改账号的额度监控设置（只改显式给出的项）"},
	{"unset", "清除账号的额度监控设置（整行或某一项）"},
}

// quotaAutoRefreshInterval 是 serve 里自动刷新的间隔。自动刷新默认关闭，
// 只有设置了 auto_refresh=true 的账号会被刷；启动后先刷一次，再按这个间隔走。
const quotaAutoRefreshInterval = 30 * time.Minute

func cmdQuota(e *Env, args []string) error {
	if isHelpRequest(args) || len(args) == 0 {
		printGroupHelp(e.Stdout, "quota", "账号额度监控与到期提示（只读，不改选号行为）", quotaSubs)
		return nil
	}
	switch args[0] {
	case "ls":
		return cmdQuotaLs(e, args[1:])
	case "show":
		return cmdQuotaShow(e, args[1:])
	case "refresh":
		return cmdQuotaRefresh(e, args[1:])
	case "set":
		return cmdQuotaSet(e, args[1:])
	case "unset":
		return cmdQuotaUnset(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | show | refresh | set | unset\n用 lax quota --help 查看子命令", args[0])
	}
}

func cmdQuotaLs(e *Env, args []string) error {
	fs := newFlagSet(e, "quota ls", "列出各账号的额度快照")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("用法：lax quota ls [--json]\n看一个账号的明细用 lax quota show <账号>")
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
	entries, err := a.st.QuotaEntries()
	if err != nil {
		return err
	}
	settings, err := a.st.AllQuotaSettings()
	if err != nil {
		return err
	}

	type lsItem struct {
		acc     store.Account
		entry   *quota.Entry
		setting quota.Settings
	}
	items := make([]lsItem, 0, len(accounts))
	for _, acc := range accounts {
		item := lsItem{acc: acc, setting: quota.DefaultSettings()}
		if cfg, ok := settings[acc.ID]; ok {
			item.setting = cfg
		}
		if entry, ok := entries[acc.ID]; ok {
			e := entry
			item.entry = &e
			item.setting = entry.Settings
		}
		items = append(items, item)
	}

	if e.JSON {
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			entry := map[string]any{
				"account":  item.acc.Name,
				"provider": item.acc.Provider,
				"settings": quotaSettingsJSON(item.setting),
			}
			if item.entry == nil {
				entry["status"] = ""
				entry["status_text"] = quotaStatusText("")
				entry["snapshot"] = false
			} else {
				for k, v := range quotaFactsJSON(item.entry.Facts, item.setting) {
					entry[k] = v
				}
				entry["snapshot"] = true
			}
			// 显示名与来源错误只为 show 排障用，ls --json 不默认带出。
			delete(entry, "display_name")
			delete(entry, "source_errors")
			out = append(out, entry)
		}
		return e.printJSON(out)
	}

	if len(items) == 0 {
		fmt.Fprintln(e.Stdout, "还没有账号。先运行 lax account add <名称> --provider <厂商> --key <key>")
		return nil
	}
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		status, balance, usd, ratio, periodEnd, checkedAt, note := "未刷新", "-", "-", "-", "-", "-", "-"
		if item.entry != nil {
			f := item.entry.Facts
			status = quotaStatusText(f.Status)
			balance = quotaBalanceCell(f)
			usd = quotaUSDCell(f.BalanceUSD)
			ratio = quotaPercentCell(f.RemainingRatio)
			periodEnd = quotaTimeCell(f.PeriodEnd)
			checkedAt = quotaCheckedAtCell(f.CheckedAt)
			note = truncate(quotaNote(f), 48)
		}
		rows = append(rows, []string{item.acc.Name, item.acc.Provider, status, balance, usd, ratio, periodEnd, checkedAt, note})
	}
	fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
		Header:    []string{"账号", "厂商", "状态", "余额", "折合 USD", "剩余比例", "周期结束", "刷新时间", "备注"},
		Align:     []tableAlign{alignLeft, alignLeft, alignLeft, alignRight, alignRight, alignRight, alignLeft, alignLeft, alignLeft},
		Rows:      rows,
		Style:     newUI(e.Stdout),
		HeaderSep: true,
	}))
	fmt.Fprintln(e.Stdout, "刷新一个账号：lax quota refresh <账号>；看明细：lax quota show <账号>")
	return nil
}

func cmdQuotaShow(e *Env, args []string) error {
	fs := newFlagSet(e, "quota show", "查看一个账号的额度明细；加 --settings 看监控设置")
	settingsOnly := fs.Bool("settings", false, "只看监控设置，不显示额度快照")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：lax quota show <账号> [--settings] [--json]")
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	acc, err := a.st.GetAccount(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", fs.Arg(0))
	}
	cfg, _, err := a.st.QuotaSettings(acc.ID)
	if err != nil {
		return err
	}

	// 监控设置是配置，不是额度事实：默认不跟快照混在一起，
	// 需要时用 --settings 单独看，避免在明细下面再叠一张配置表。
	if *settingsOnly {
		if e.JSON {
			return e.printJSON(map[string]any{
				"account":  acc.Name,
				"provider": acc.Provider,
				"settings": quotaSettingsJSON(cfg),
			})
		}
		fmt.Fprintf(e.Stdout, "%s 的额度监控设置\n\n", acc.Name)
		fmt.Fprint(e.Stdout, quotaSettingsReport(cfg))
		return nil
	}

	facts, found, err := a.st.QuotaSnapshot(acc.ID)
	if err != nil {
		return err
	}
	if found {
		// 与选号侧一致：按当前设置重新折算一次，改过汇率后不用等下一次刷新。
		facts = quota.Apply(facts, cfg)
	}
	state, action := quota.ExpiryAt(facts.PeriodEnd, time.Now(), cfg)

	if e.JSON {
		out := map[string]any{
			"account":  acc.Name,
			"provider": acc.Provider,
			"snapshot": found,
			"settings": quotaSettingsJSON(cfg),
		}
		if found {
			for k, v := range quotaFactsJSON(facts, cfg) {
				out[k] = v
			}
		}
		out["expiry_state"] = state
		out["expiry_action"] = action
		return e.printJSON(out)
	}

	if !found {
		fmt.Fprintf(e.Stdout, "账号 %s 还没有额度快照。运行 lax quota refresh %s 查一次（账号仍按原样参与选号）\n", acc.Name, acc.Name)
		return nil
	}

	ui := newUI(e.Stdout)
	fmt.Fprint(e.Stdout, renderQuota(ui, acc, facts, state, time.Now()))
	if note := quotaFactNote(facts); note != "" {
		fmt.Fprintf(e.Stdout, "\n%s\n", note)
	}
	return nil
}

// quotaLabelWidth 是所有「标签 + 值」行共用的标签宽度，让各区块的值对齐在同一列。
// 按终端显示宽度算，中文字符占两格。
const quotaLabelWidth = 8

// quotaRow 是一行「标签 + 值」。color 是值的 ANSI 颜色，空表示不着色。
type quotaRow struct {
	label string
	value string
	color string
}

// quotaSection 是人读输出里的一个区块：标题加若干行；行全为空时整块不输出。
type quotaSection struct {
	title string
	rows  []quotaRow
}

func renderQuotaRows(b *strings.Builder, u uiStyle, indent int, rows []quotaRow) {
	for _, row := range rows {
		if row.value == "" {
			continue
		}
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(pad(row.label, quotaLabelWidth))
		b.WriteString("  ")
		b.WriteString(u.paint(row.color, row.value))
		b.WriteByte('\n')
	}
}

// renderQuota 按「账号 / 额度 / 周期 / 限流窗口 / 上游 / 排障」分组输出。
// 没有内容的区块整块省略，不拿空行和 "-" 占位。
func renderQuota(u uiStyle, acc store.Account, f quota.Facts, state string, now time.Time) string {
	var b strings.Builder

	top := []quotaRow{
		{"账号", acc.Name + "（" + acc.Provider + "）", ""},
		{"状态", quotaStatusText(f.Status), quotaStatusColor(f.Status)},
	}
	if f.CheckedAt > 0 {
		top = append(top, quotaRow{"刷新", quotaCheckedAtCell(f.CheckedAt), ""})
	}
	if name := quotaSafeText(f.DisplayName); name != "" {
		top = append(top, quotaRow{"显示名", name, ""})
	}
	// 来源与厂商一致时是重复信息；只有用户强制换过来源才值得单独显示。
	if f.Source != "" && f.Source != acc.Provider {
		top = append(top, quotaRow{"接口", quotaSourceText(f.Source), ""})
	}
	renderQuotaRows(&b, u, 0, top)

	sections := []quotaSection{
		{"额度", quotaCreditRows(f)},
		{"周期", quotaPeriodRows(f, state, now)},
		{"限流窗口", quotaWindowRows(f)},
		{"上游", quotaUpstreamRows(f)},
		{"排障", quotaTroubleRows(f)},
	}
	for _, section := range sections {
		if len(section.rows) == 0 {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(u.bold(section.title))
		b.WriteByte('\n')
		renderQuotaRows(&b, u, 2, section.rows)
	}
	return b.String()
}

func quotaCreditRows(f quota.Facts) []quotaRow {
	rows := make([]quotaRow, 0, 7)
	if f.BalanceKnown {
		rows = append(rows, quotaRow{"余额", quotaBalanceCell(f), ""})
	}
	if f.UsedKnown {
		rows = append(rows, quotaRow{"本期已用", quotaMoneyCell(f.Used, true, f.Currency), ""})
	}
	if f.FXRate != nil && *f.FXRate != 1 {
		rows = append(rows, quotaRow{"汇率", quotaRateCell(f.FXRate), ""})
		if f.BalanceUSD != nil {
			rows = append(rows, quotaRow{"折合余额", quotaUSDWithUnit(f.BalanceUSD), ""})
		}
		if f.UsedUSD != nil {
			rows = append(rows, quotaRow{"折合已用", quotaUSDWithUnit(f.UsedUSD), ""})
		}
	}
	if f.TotalUSD != nil {
		// CommandCode 的计费周期是按月的，余额 + 本期已用就是月限额；其它来源没有这个口径。
		label := "总额"
		if f.Source == quota.SourceCommandCode {
			label = "月限额"
		}
		rows = append(rows, quotaRow{label, quotaUSDWithUnit(f.TotalUSD), ""})
	}
	if f.RemainingRatio != nil {
		rows = append(rows, quotaRow{
			"剩余",
			quotaPercentCell(f.RemainingRatio) + "  " + quotaBar(f.RemainingRatio),
			quotaRatioColor(f.RemainingRatio),
		})
	}
	return rows
}

func quotaPeriodRows(f quota.Facts, state string, now time.Time) []quotaRow {
	rows := make([]quotaRow, 0, 3)
	if f.PeriodEnd > 0 {
		rows = append(rows, quotaRow{"结束", quotaPeriodEndCell(f.PeriodEnd, state, now), ""})
	}
	if plan := quotaPlanCell(f); plan != "" {
		rows = append(rows, quotaRow{"套餐", plan, ""})
	}
	if f.PlanID != "" || f.PeriodEnd > 0 {
		rows = append(rows, quotaRow{"续费", quotaRenewalText(f), ""})
	}
	return rows
}

func quotaWindowRows(f quota.Facts) []quotaRow {
	rows := make([]quotaRow, 0, 2)
	if f.FiveHour != nil {
		value, color := quotaWindowCell(f.FiveHour)
		rows = append(rows, quotaRow{"5 小时", value, color})
	}
	if f.Weekly != nil {
		value, color := quotaWindowCell(f.Weekly)
		rows = append(rows, quotaRow{"每周", value, color})
	}
	return rows
}

// quotaWindowCell 给限流窗口补一条已用比例进度条。窗口是「已用 / 上限」，
// 方向与余额相反：用得多更紧张，所以条越满颜色越红。
func quotaWindowCell(w *quota.Window) (string, string) {
	base, notes := quotaWindowParts(w)
	if w == nil || w.Used == nil || w.Limit == nil || *w.Limit <= 0 {
		return quotaWindowText(w), ""
	}
	ratio := *w.Used / *w.Limit
	parts := []string{base, quotaBar(&ratio)}
	if len(notes) > 0 {
		parts = append(parts, "（"+strings.Join(notes, "，")+"）")
	}
	return strings.Join(parts, "  "), quotaWindowColor(ratio)
}

func quotaWindowColor(ratio float64) string {
	switch {
	case ratio >= 1:
		return "31"
	case ratio >= 0.8:
		return "33"
	default:
		return "32"
	}
}

func quotaUpstreamRows(f quota.Facts) []quotaRow {
	rows := make([]quotaRow, 0, 2)
	if usage := quotaUsageText(f.Usage); usage != "-" {
		rows = append(rows, quotaRow{"用量", usage, ""})
	}
	if f.Available != nil {
		rows = append(rows, quotaRow{"可用", quotaAvailableText(f.Available), ""})
	}
	return rows
}

func quotaTroubleRows(f quota.Facts) []quotaRow {
	rows := make([]quotaRow, 0, 2)
	if note := quotaSafeText(f.LastError); note != "" {
		rows = append(rows, quotaRow{"备注", note, ""})
	}
	if errs := quotaSourceErrorsText(f.SourceErrors); errs != "-" {
		rows = append(rows, quotaRow{"失败端点", errs, ""})
	}
	return rows
}

// quotaBar 画一条剩余比例进度条。未知比例不画；大于 0 但不足一格时至少画一格，
// 让「还剩一点点」也能被看见。
func quotaBar(ratio *float64) string {
	if ratio == nil {
		return ""
	}
	const width = 20
	filled := int(math.Round(*ratio * width))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	if filled == 0 && *ratio > 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func quotaStatusColor(status string) string {
	switch status {
	case quota.StatusOK:
		return "32"
	case quota.StatusPartial:
		return "33"
	case quota.StatusAuth, quota.StatusError:
		return "31"
	case "":
		return "2"
	default:
		return ""
	}
}

func quotaRatioColor(ratio *float64) string {
	if ratio == nil {
		return ""
	}
	switch {
	case *ratio >= 0.5:
		return "32"
	case *ratio >= 0.2:
		return "33"
	default:
		return "31"
	}
}

// quotaFactNote 只在表格里真的出现窗口或上游用量时补一句说明，
// 没有这些数据时不打印一段与本次无关的话。
func quotaFactNote(f quota.Facts) string {
	hasUsage := f.Usage.TotalCredits != nil || f.Usage.Requests != nil || f.Usage.SuccessRate != nil
	if f.FiveHour == nil && f.Weekly == nil && !hasUsage {
		return ""
	}
	return "说明：5 小时/每周窗口是限流窗口，不是月度总额；上游用量是所有客户端合计，仅供查看，不参与选号。"
}

func cmdQuotaRefresh(e *Env, args []string) error {
	fs := newFlagSet(e, "quota refresh", "手动刷新一个账号或全部账号的额度")
	all := fs.Bool("all", false, "刷新全部账号")
	usage := "用法：lax quota refresh <账号> | lax quota refresh --all"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all && fs.NArg() != 0 {
		return usagef("--all 与账号名不能一起写\n%s", usage)
	}
	if !*all && fs.NArg() != 1 {
		return usagef("要写一个账号名，或加 --all\n%s", usage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	var accounts []store.Account
	if *all {
		accounts, err = a.st.ListAccounts()
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			if e.JSON {
				return e.printJSON([]any{})
			}
			fmt.Fprintln(e.Stdout, "还没有账号。先运行 lax account add <名称> --provider <厂商> --key <key>")
			return nil
		}
	} else {
		acc, err2 := a.st.GetAccount(fs.Arg(0))
		if err2 != nil {
			return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", fs.Arg(0))
		}
		accounts = []store.Account{acc}
	}

	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: e.gatewayLogf()})
	if err != nil {
		return err
	}

	type refreshRow struct {
		Account  string                 `json:"account"`
		Provider string                 `json:"provider"`
		Status   string                 `json:"status"`
		Skipped  bool                   `json:"skipped"`
		Error    string                 `json:"error,omitempty"`
		Facts    map[string]any         `json:"facts,omitempty"`
		Settings quotaSettingsJSONValue `json:"settings"`
	}
	rows := make([]refreshRow, 0, len(accounts))
	failed := 0
	for _, acc := range accounts {
		cfg, _, err := a.st.QuotaSettings(acc.ID)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		facts, refreshErr := gs.RefreshQuota(ctx, acc, cfg)
		cancel()

		row := refreshRow{Account: acc.Name, Provider: acc.Provider, Settings: quotaSettingsJSON(cfg)}
		if facts.Status == quota.StatusUnsupported {
			row.Skipped = true
			row.Status = facts.Status
			row.Error = facts.LastError
			// 来源已关闭或厂商不支持时，旧快照不能再留在库里参与选号。
			if _, err := a.st.DeleteQuotaSnapshot(acc.ID); err != nil {
				return fmt.Errorf("清理账号 %s 的旧额度快照失败：%w", acc.Name, err)
			}
			rows = append(rows, row)
			continue
		}
		prev, hadPrev, err := a.st.QuotaSnapshot(acc.ID)
		if err != nil {
			return err
		}
		if hadPrev {
			facts = mergeQuotaFacts(prev, facts, cfg)
		} else {
			facts = quota.Apply(facts, cfg)
		}
		if err := a.st.SaveQuotaSnapshot(acc.ID, facts); err != nil {
			return fmt.Errorf("保存账号 %s 的额度快照失败：%w", acc.Name, err)
		}
		row.Status = facts.Status
		row.Error = facts.LastError
		row.Facts = quotaFactsJSON(facts, cfg)
		if refreshErr != nil {
			failed++
		}
		rows = append(rows, row)
	}

	if e.JSON {
		if err := e.printJSON(rows); err != nil {
			return err
		}
	} else {
		table := make([][]string, 0, len(rows))
		for _, r := range rows {
			note := r.Error
			if r.Skipped && note == "" {
				note = "不支持这家厂商，已跳过"
			}
			if note == "" {
				note = "-"
			}
			table = append(table, []string{r.Account, r.Provider, quotaStatusText(r.Status), note})
		}
		fmt.Fprint(e.Stdout, renderTableWithSpec(tableSpec{
			Header:    []string{"账号", "厂商", "状态", "备注"},
			Rows:      table,
			Style:     newUI(e.Stdout),
			HeaderSep: true,
		}))
		if failed == 0 {
			fmt.Fprintln(e.Stdout, "额度刷新失败只更新快照状态，不会停用账号、也不写请求记录。")
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 个账号的额度刷新失败；失败只更新快照状态，账号仍按原样参与选号", failed)
	}
	return nil
}

func cmdQuotaSet(e *Env, args []string) error {
	fs := newFlagSet(e, "quota set", "修改账号的额度监控设置，只改显式给出的项")
	source := fs.String("source", "", "监控来源：auto / commandcode / deepseek / off")
	fxRate := fs.Float64("fx-rate", 0, "非 USD 币种折 USD 的手工汇率，例如 CNY 填 0.14")
	autoRefresh := fs.Bool("auto-refresh", false, "serve 时是否定时刷新这个账号的额度")
	expiryMode := fs.String("expiry-mode", "", "到期处理：warn / deprioritize / exclude")
	expiryDays := fs.Int("expiry-days", 0, "临近到期的提前天数，默认 3")
	baseURL := fs.String("base-url", "", "额度接口地址；留空时用账号所属厂商的 base_url")
	usage := "用法：lax quota set <账号> [--source auto|commandcode|deepseek|off] [--fx-rate N] " +
		"[--auto-refresh[=true|false]] [--expiry-mode warn|deprioritize|exclude] [--expiry-days N] [--base-url <地址>]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n账号名要写一个", usage)
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	acc, err := a.st.GetAccount(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", fs.Arg(0))
	}
	cfg, _, err := a.st.QuotaSettings(acc.ID)
	if err != nil {
		return err
	}
	oldSource := cfg.Source

	changed := false
	if fs.Changed("source") {
		cfg.Source = strings.TrimSpace(*source)
		changed = true
	}
	if fs.Changed("fx-rate") {
		v := *fxRate
		cfg.FXRate = &v
		changed = true
	}
	if fs.Changed("auto-refresh") {
		cfg.AutoRefresh = *autoRefresh
		changed = true
	}
	if fs.Changed("expiry-mode") {
		cfg.ExpiryMode = strings.TrimSpace(*expiryMode)
		changed = true
	}
	if fs.Changed("expiry-days") {
		cfg.ExpiryDays = *expiryDays
		changed = true
	}
	if fs.Changed("base-url") {
		cfg.BaseURL = strings.TrimSpace(*baseURL)
		changed = true
	}
	if !changed {
		return usagef("至少给一个要改的项\n%s", usage)
	}
	if err := quota.ValidateSettings(cfg); err != nil {
		return usagef("%v\n%s", err, usage)
	}
	if err := a.st.SetQuotaSettings(acc.ID, cfg); err != nil {
		return err
	}
	// 监控来源换了，旧快照就属于上一个来源：清掉，别让旧余额跨来源继续参与过滤与排序。
	if cfg.Source != oldSource {
		if _, err := a.st.DeleteQuotaSnapshot(acc.ID); err != nil {
			return fmt.Errorf("监控来源变化后清理账号 %s 的旧额度快照失败：%w", acc.Name, err)
		}
	}
	if e.JSON {
		return e.printJSON(map[string]any{"account": acc.Name, "settings": quotaSettingsJSON(cfg)})
	}
	fmt.Fprintf(e.Stdout, "已更新 %s 的额度监控设置\n", acc.Name)
	fmt.Fprint(e.Stdout, quotaSettingsTable(cfg))
	return nil
}

func cmdQuotaUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "quota unset", "清除账号的额度监控设置，恢复默认")
	usage := "用法：lax quota unset <账号> [source|fx_rate|auto_refresh|expiry_mode|expiry_days|base_url]"
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return usagef("%s\n不写字段时删掉整行设置；写字段时只把那一项恢复默认", usage)
	}
	field := ""
	if fs.NArg() == 2 {
		field = strings.TrimSpace(fs.Arg(1))
		switch field {
		case "source", "fx_rate", "auto_refresh", "expiry_mode", "expiry_days", "base_url":
		default:
			return usagef("不认识的字段 %q，可用 source、fx_rate、auto_refresh、expiry_mode、expiry_days、base_url\n%s", field, usage)
		}
	}
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	acc, err := a.st.GetAccount(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("账号 %q 不存在；用 lax account ls 看现有账号", fs.Arg(0))
	}
	cfg, found, err := a.st.QuotaSettings(acc.ID)
	if err != nil {
		return err
	}

	removed := false
	if found {
		if field == "" {
			removed, err = a.st.DeleteQuotaSettings(acc.ID)
			if err != nil {
				return err
			}
			cfg = quota.DefaultSettings()
		} else {
			defaults := quota.DefaultSettings()
			switch field {
			case "source":
				cfg.Source = defaults.Source
			case "fx_rate":
				cfg.FXRate = nil
			case "auto_refresh":
				cfg.AutoRefresh = defaults.AutoRefresh
			case "expiry_mode":
				cfg.ExpiryMode = defaults.ExpiryMode
			case "expiry_days":
				cfg.ExpiryDays = defaults.ExpiryDays
			case "base_url":
				cfg.BaseURL = defaults.BaseURL
			}
			if quotaSettingsIsDefault(cfg) {
				removed, err = a.st.DeleteQuotaSettings(acc.ID)
				if err != nil {
					return err
				}
				cfg = quota.DefaultSettings()
			} else if err := a.st.SetQuotaSettings(acc.ID, cfg); err != nil {
				return err
			}
		}
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"account": acc.Name, "field": field, "removed": removed,
			"settings": quotaSettingsJSON(cfg),
		})
	}
	if !found {
		fmt.Fprintf(e.Stdout, "%s 本来没有额度监控设置，按默认值生效\n", acc.Name)
	} else if field == "" {
		if removed {
			fmt.Fprintf(e.Stdout, "已删除 %s 的额度监控设置，恢复默认\n", acc.Name)
		} else {
			fmt.Fprintf(e.Stdout, "%s 本来没有额度监控设置，按默认值生效\n", acc.Name)
		}
	} else {
		fmt.Fprintf(e.Stdout, "已把 %s 的 %s 恢复默认\n", acc.Name, field)
	}
	fmt.Fprint(e.Stdout, quotaSettingsTable(cfg))
	return nil
}

// startQuotaAutoRefresh 是 serve 的自动刷新循环：只有 auto_refresh=true 的账号会被刷。
// 它是单 goroutine 加 ticker，ctx 取消就退出；启动后先刷一轮，再等间隔。
// 失败只打日志并写入快照状态，不改账号启用状态、不写请求记录。
func startQuotaAutoRefresh(ctx context.Context, st *store.Store, gs *gateway.Server,
	logf func(string, ...any), interval time.Duration) {

	if interval <= 0 {
		interval = quotaAutoRefreshInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	refresh := func() {
		accounts, err := st.ListAccounts()
		if err != nil {
			logf("额度自动刷新读账号失败：%v", err)
			return
		}
		settings, err := st.AllQuotaSettings()
		if err != nil {
			logf("额度自动刷新读设置失败：%v", err)
			return
		}
		for _, acc := range accounts {
			if ctx.Err() != nil {
				return
			}
			cfg, ok := settings[acc.ID]
			if !ok || !cfg.AutoRefresh {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			facts, refreshErr := gs.RefreshQuota(rctx, acc, cfg)
			cancel()
			if facts.Status == quota.StatusUnsupported {
				// 同上：来源关闭或厂商不支持时清掉旧快照，避免过期余额继续参与选号。
				if _, derr := st.DeleteQuotaSnapshot(acc.ID); derr != nil {
					logf("额度自动刷新清理 %s 的旧快照失败：%v", acc.Name, derr)
				}
				continue
			}
			prev, found, err := st.QuotaSnapshot(acc.ID)
			if err != nil {
				logf("额度自动刷新读 %s 的快照失败：%v", acc.Name, err)
				continue
			}
			if found {
				facts = mergeQuotaFacts(prev, facts, cfg)
			} else {
				facts = quota.Apply(facts, cfg)
			}
			if err := st.SaveQuotaSnapshot(acc.ID, facts); err != nil {
				logf("额度自动刷新写 %s 的快照失败：%v", acc.Name, err)
				continue
			}
			if refreshErr != nil {
				logf("额度刷新 %s 失败：%v", acc.Name, refreshErr)
			}
		}
	}

	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// mergeQuotaFacts 把新快照与上一次快照合并：某个端点失败时，那一段保留旧值。
// 这样一次部分失败不会把上一次已知的余额、周期或用量清成空；整体失败（凭据解密失败、
// 地址不合法）保留全部旧值，只更新状态、错误与刷新时间。
func mergeQuotaFacts(prev, next quota.Facts, cfg quota.Settings) quota.Facts {
	wholeFailure := next.Status == quota.StatusAuth || next.Status == quota.StatusError
	if wholeFailure {
		if next.SourceErrors["auth"] != "" || next.SourceErrors["base_url"] != "" || len(next.SourceErrors) == 0 {
			next = copyQuotaValues(prev, next)
			return quota.Apply(next, cfg)
		}
	}
	for endpoint := range next.SourceErrors {
		switch endpoint {
		case "whoami":
			next.DisplayName = prev.DisplayName
		case "credits":
			next.Balance, next.BalanceKnown, next.Currency = prev.Balance, prev.BalanceKnown, prev.Currency
			next.FiveHour, next.Weekly = prev.FiveHour, prev.Weekly
		case "usage":
			next.Used, next.UsedKnown, next.Usage = prev.Used, prev.UsedKnown, prev.Usage
		case "subscriptions":
			next.PeriodEnd = prev.PeriodEnd
			next.PlanID, next.PlanStatus = prev.PlanID, prev.PlanStatus
			next.CancelAtPeriodEnd = prev.CancelAtPeriodEnd
		case "balance":
			next.Balance, next.BalanceKnown, next.Currency = prev.Balance, prev.BalanceKnown, prev.Currency
			next.Available = prev.Available
		}
	}
	// usage 端点 200 但 summary 为空时解析会成功返回零值，SourceErrors 里没有 "usage"；
	// 缺字段不是清零，不能拿 unknown 覆盖上一次已知的已用与用量。字段逐个合并：
	// 新响应给了哪一项就用新的，没给的沿用旧值。
	if !next.UsedKnown && prev.UsedKnown {
		next.Used, next.UsedKnown = prev.Used, prev.UsedKnown
	}
	next.Usage = mergeUpstreamUsage(prev.Usage, next.Usage)
	// 限流窗口也是「缺字段不是清零」：credits 端点报错、或它 200 但响应里没有窗口时，
	// 沿用上一次已知的 5 小时与每周窗口。窗口自带重置时间，旧窗口到期后界面上自然显示过期，
	// 不能因为这一次没读到就把上一次的数字擦掉。
	if next.FiveHour == nil {
		next.FiveHour = prev.FiveHour
	}
	if next.Weekly == nil {
		next.Weekly = prev.Weekly
	}
	return quota.Apply(next, cfg)
}

// mergeUpstreamUsage 按字段合并上游用量：新响应缺的字段沿用上一次的值。
func mergeUpstreamUsage(prev, next quota.UpstreamUsage) quota.UpstreamUsage {
	if next.TotalCredits == nil {
		next.TotalCredits = prev.TotalCredits
	}
	if next.Requests == nil {
		next.Requests = prev.Requests
	}
	if next.SuccessRate == nil {
		next.SuccessRate = prev.SuccessRate
	}
	return next
}

// copyQuotaValues 只拷事实值，不拷状态、错误与刷新时间：整体失败时旧余额仍要显示，
// 但状态必须写这次的失败，否则界面上看不出刷新失败过。
func copyQuotaValues(prev, next quota.Facts) quota.Facts {
	next.DisplayName = prev.DisplayName
	next.Balance, next.BalanceKnown = prev.Balance, prev.BalanceKnown
	next.Used, next.UsedKnown = prev.Used, prev.UsedKnown
	next.Currency = prev.Currency
	next.PeriodEnd = prev.PeriodEnd
	next.PlanID, next.PlanStatus = prev.PlanID, prev.PlanStatus
	next.CancelAtPeriodEnd = prev.CancelAtPeriodEnd
	next.FiveHour, next.Weekly = prev.FiveHour, prev.Weekly
	next.Usage = prev.Usage
	next.Available = prev.Available
	return next
}

type quotaSettingsJSONValue struct {
	Source      string   `json:"source"`
	FXRate      *float64 `json:"fx_rate"`
	AutoRefresh bool     `json:"auto_refresh"`
	ExpiryMode  string   `json:"expiry_mode"`
	ExpiryDays  int      `json:"expiry_days"`
	BaseURL     string   `json:"base_url"`
}

func quotaSettingsJSON(s quota.Settings) quotaSettingsJSONValue {
	return quotaSettingsJSONValue{
		Source: s.Source, FXRate: s.FXRate, AutoRefresh: s.AutoRefresh,
		ExpiryMode: s.ExpiryMode, ExpiryDays: s.ExpiryDays, BaseURL: s.BaseURL,
	}
}

func quotaFactsJSON(f quota.Facts, s quota.Settings) map[string]any {
	var state, action string
	state, action = quota.ExpiryAt(f.PeriodEnd, time.Now(), s)
	balance := any(nil)
	if f.BalanceKnown {
		balance = f.Balance
	}
	used := any(nil)
	if f.UsedKnown {
		used = f.Used
	}
	usage := map[string]any{
		"total_credits": f.Usage.TotalCredits,
		"requests":      f.Usage.Requests,
		"success_rate":  f.Usage.SuccessRate,
	}
	sourceErrors := f.SourceErrors
	if sourceErrors == nil {
		sourceErrors = map[string]string{}
	}
	return map[string]any{
		"source":               f.Source,
		"status":               f.Status,
		"display_name":         f.DisplayName,
		"balance":              balance,
		"balance_known":        f.BalanceKnown,
		"used":                 used,
		"used_known":           f.UsedKnown,
		"currency":             f.Currency,
		"fx_rate":              f.FXRate,
		"balance_usd":          f.BalanceUSD,
		"used_usd":             f.UsedUSD,
		"total_usd":            f.TotalUSD,
		"remaining_ratio":      f.RemainingRatio,
		"period_end":           f.PeriodEnd,
		"plan_id":              f.PlanID,
		"plan_status":          f.PlanStatus,
		"cancel_at_period_end": f.CancelAtPeriodEnd,
		"five_hour":            quotaWindowJSON(f.FiveHour),
		"weekly":               quotaWindowJSON(f.Weekly),
		"usage":                usage,
		"available":            f.Available,
		"checked_at":           f.CheckedAt,
		"last_error":           f.LastError,
		"source_errors":        sourceErrors,
		"expiry_state":         state,
		"expiry_action":        action,
	}
}

func quotaWindowJSON(w *quota.Window) any {
	if w == nil {
		return nil
	}
	return map[string]any{"used": w.Used, "limit": w.Limit, "reset_at": w.ResetAt}
}

// quotaSettingsRows 是监控设置的人读行。内部取值（auto、warn 这类）
// 只出现在命令参数里，展示给用户时换成中文说法。
func quotaSettingsRows(s quota.Settings) []quotaRow {
	rate := "未设置"
	if s.FXRate != nil {
		rate = quotaAmount(*s.FXRate)
	}
	auto := "关闭"
	if s.AutoRefresh {
		auto = "开启"
	}
	base := "跟随账号的 base_url"
	if strings.TrimSpace(s.BaseURL) != "" {
		base = s.BaseURL
	}
	return []quotaRow{
		{"监控来源", quotaSourceText(s.Source), ""},
		{"汇率", rate, ""},
		{"自动刷新", auto, ""},
		{"到期处理", quotaExpiryModeText(s.ExpiryMode), ""},
		{"提前天数", strconv.Itoa(s.ExpiryDays) + " 天", ""},
		{"额度地址", base, ""},
	}
}

func quotaSettingsReport(s quota.Settings) string {
	var b strings.Builder
	renderQuotaRows(&b, uiStyle{}, 2, quotaSettingsRows(s))
	return b.String()
}

func quotaSettingsTable(s quota.Settings) string {
	return "额度监控设置\n\n" + quotaSettingsReport(s)
}

func quotaSourceText(source string) string {
	switch source {
	case quota.SourceAuto:
		return "自动识别（按厂商）"
	case quota.SourceCommandCode:
		return "CommandCode"
	case quota.SourceDeepSeek:
		return "DeepSeek"
	case quota.SourceOff:
		return "已关闭"
	case "":
		return "-"
	default:
		return source
	}
}

func quotaStatusText(status string) string {
	switch status {
	case "":
		return "未刷新"
	case quota.StatusOK:
		return "正常"
	case quota.StatusPartial:
		return "部分成功"
	case quota.StatusAuth:
		return "凭据被拒"
	case quota.StatusError:
		return "查询失败"
	case quota.StatusUnsupported:
		return "暂不支持"
	default:
		return status
	}
}

func quotaExpiryModeText(mode string) string {
	switch mode {
	case quota.ExpiryModeWarn:
		return "只提示"
	case quota.ExpiryModeDeprioritize:
		return "排到后面"
	case quota.ExpiryModeExclude:
		return "不参与选号"
	default:
		return mode
	}
}

func quotaBalanceCell(f quota.Facts) string {
	return quotaMoneyCell(f.Balance, f.BalanceKnown, f.Currency)
}

// quotaMoneyCell 渲染带币种的金额。币种为空时只给数字，不硬塞一个 USD。
func quotaMoneyCell(v float64, known bool, currency string) string {
	if !known {
		return "-"
	}
	if currency == "" {
		return quotaAmount(v)
	}
	return quotaAmount(v) + " " + currency
}

func quotaAmount(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		s = "0"
	}
	return s
}

func quotaUSDCell(v *float64) string {
	if v == nil {
		return "-"
	}
	return quotaAmount(*v)
}

// quotaUSDWithUnit 用在 quota show：行名不带 USD，单位直接写在值里，读起来更自然。
func quotaUSDWithUnit(v *float64) string {
	if v == nil {
		return "-"
	}
	return quotaAmount(*v) + " USD"
}

func quotaRateCell(v *float64) string {
	if v == nil {
		return "-"
	}
	return quotaAmount(*v)
}

func quotaPercentCell(v *float64) string {
	if v == nil {
		return "-"
	}
	s := strconv.FormatFloat(*v*100, 'f', 1, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		s = "0"
	}
	return s + "%"
}

func quotaTimeCell(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// quotaPeriodEndCell 渲染周期结束，并在后面补一句人读的到期提示。
// 这里只回答「还有多久」，到期之后怎么处理属于监控设置，不在这里展示。
func quotaPeriodEndCell(ms int64, state string, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	out := quotaTimeCell(ms)
	switch state {
	case quota.ExpiryExpired:
		return out + "（已到期）"
	case quota.ExpiryNear:
		days := int(math.Ceil(float64(ms-now.UnixMilli()) / float64(24*time.Hour/time.Millisecond)))
		if days < 1 {
			days = 1
		}
		return out + "（还剩 " + strconv.Itoa(days) + " 天）"
	default:
		return out
	}
}

// quotaCheckedAtCell 渲染 Unix 秒。额度快照的 checked_at 按秒存，周期结束与窗口重置按毫秒存；
// 两者不能共用一个渲染函数，否则「刷新时间」会被显示成 1970 年。
func quotaCheckedAtCell(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

// quotaSafeText 去掉上游文本里的控制字符。显示名与错误摘录会原样进终端表格，
// ANSI/OSC 序列能改标题、伪造输出甚至写剪贴板，必须在渲染前清掉。
func quotaSafeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func quotaRenewalText(f quota.Facts) string {
	if f.PlanID == "" && f.PeriodEnd == 0 {
		return "-"
	}
	if f.CancelAtPeriodEnd {
		return "到期取消"
	}
	return "自动续费"
}

func quotaWindowText(w *quota.Window) string {
	base, notes := quotaWindowParts(w)
	if len(notes) > 0 {
		base += "（" + strings.Join(notes, "，") + "）"
	}
	return base
}

// quotaWindowParts 把窗口拆成「已用 / 上限」与附加说明，方便进度条插在两者之间。
func quotaWindowParts(w *quota.Window) (string, []string) {
	if w == nil {
		return "-", nil
	}
	used, limit := "-", "-"
	if w.Used != nil {
		used = quotaAmount(*w.Used)
	}
	if w.Limit != nil {
		limit = quotaAmount(*w.Limit)
	}
	notes := make([]string, 0, 2)
	if w.Used != nil && w.Limit != nil && *w.Used > *w.Limit {
		notes = append(notes, "已超限")
	}
	if w.ResetAt > 0 {
		notes = append(notes, "重置 "+quotaTimeCell(w.ResetAt))
	}
	return used + " / " + limit, notes
}

func quotaUsageText(u quota.UpstreamUsage) string {
	parts := make([]string, 0, 3)
	if u.TotalCredits != nil {
		parts = append(parts, "本期已用 "+quotaAmount(*u.TotalCredits)+" credits")
	}
	if u.Requests != nil {
		parts = append(parts, "请求 "+strconv.FormatInt(*u.Requests, 10))
	}
	if u.SuccessRate != nil {
		parts = append(parts, "成功率 "+quotaAmount(*u.SuccessRate))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "，")
}

// quotaPlanCell 把套餐名与状态合成一格，状态翻成常见中文；不认识的取值原样保留。
func quotaPlanCell(f quota.Facts) string {
	id := quotaSafeText(f.PlanID)
	status := quotaPlanStatusText(f.PlanStatus)
	switch {
	case id != "" && status != "":
		return id + "（" + status + "）"
	case id != "":
		return id
	default:
		return status
	}
}

func quotaPlanStatusText(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "":
		return ""
	case "active":
		return "生效中"
	case "trialing":
		return "试用中"
	case "canceled", "cancelled":
		return "已取消"
	case "past_due":
		return "待付款"
	case "unpaid":
		return "未付款"
	case "paused":
		return "已暂停"
	case "incomplete":
		return "未完成"
	case "incomplete_expired":
		return "已失效"
	default:
		return quotaSafeText(status)
	}
}

func quotaAvailableText(v *bool) string {
	if v == nil {
		return "-"
	}
	if *v {
		return "可用"
	}
	return "不可用"
}

func quotaSourceErrorsText(errs map[string]string) string {
	if len(errs) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"："+quotaSafeText(errs[k]))
	}
	return strings.Join(parts, "；")
}

func quotaNote(f quota.Facts) string {
	if f.LastError != "" {
		return quotaSafeText(f.LastError)
	}
	if len(f.SourceErrors) > 0 {
		return "部分端点失败：" + quotaSourceErrorsText(f.SourceErrors)
	}
	return "-"
}

func quotaSettingsIsDefault(s quota.Settings) bool {
	d := quota.DefaultSettings()
	if s.Source != d.Source || s.AutoRefresh != d.AutoRefresh ||
		s.ExpiryMode != d.ExpiryMode || s.ExpiryDays != d.ExpiryDays || s.BaseURL != d.BaseURL {
		return false
	}
	if s.FXRate == nil {
		return true
	}
	return d.FXRate != nil && *s.FXRate == *d.FXRate
}
