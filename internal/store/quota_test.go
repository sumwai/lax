package store_test

import (
	"math"
	"testing"
	"time"

	"lax/internal/quota"
)

func qf(v float64) *float64 { return &v }
func qi(v int64) *int64     { return &v }
func qb(v bool) *bool       { return &v }

func quotaNear(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9
}

func requireQuotaPtr(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s 为空，期望 %v", name, want)
	}
	if !quotaNear(*got, want) {
		t.Fatalf("%s = %v，期望约 %v", name, *got, want)
	}
}

// TestQuotaSnapshotRoundTripAndUpsert 钉住行与 domain 的完整映射，以及按 account_id 覆盖写。
func TestQuotaSnapshotRoundTripAndUpsert(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "qa", []string{"m1"})

	cfg := quota.DefaultSettings()
	cfg.FXRate = qf(0.14)
	cfg.AutoRefresh = true
	cfg.ExpiryMode = quota.ExpiryModeExclude
	cfg.ExpiryDays = 5
	cfg.BaseURL = "https://quota.example.com"
	if err := st.SetQuotaSettings(acc.ID, cfg); err != nil {
		t.Fatalf("写设置: %v", err)
	}
	gotCfg, found, err := st.QuotaSettings(acc.ID)
	if err != nil || !found {
		t.Fatalf("读设置 = %+v / %v / %v", gotCfg, found, err)
	}
	if gotCfg.Source != quota.SourceAuto || gotCfg.FXRate == nil || *gotCfg.FXRate != 0.14 ||
		!gotCfg.AutoRefresh || gotCfg.ExpiryMode != quota.ExpiryModeExclude ||
		gotCfg.ExpiryDays != 5 || gotCfg.BaseURL != "https://quota.example.com" {
		t.Fatalf("设置 = %+v", gotCfg)
	}

	periodEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	facts := quota.Facts{
		Source:            quota.SourceCommandCode,
		Status:            quota.StatusOK,
		DisplayName:       "thesumwaimzkz",
		Balance:           24.6301,
		BalanceKnown:      true,
		Used:              45.33,
		UsedKnown:         true,
		Currency:          "USD",
		PeriodEnd:         periodEnd,
		PlanID:            "pro",
		PlanStatus:        "active",
		CancelAtPeriodEnd: true,
		FiveHour:          &quota.Window{Used: qf(2), Limit: qf(10), ResetAt: 1760000000000},
		Weekly:            &quota.Window{Used: qf(30), Limit: qf(100), ResetAt: 1760500000000},
		Usage: quota.UpstreamUsage{
			TotalCredits: qf(45.33), Requests: qi(123), SuccessRate: qf(0.98),
		},
		Available:    qb(true),
		CheckedAt:    1700000000,
		LastError:    "",
		SourceErrors: map[string]string{"whoami": "超时"},
	}
	facts = quota.Apply(facts, cfg)
	if err := st.SaveQuotaSnapshot(acc.ID, facts); err != nil {
		t.Fatalf("存快照: %v", err)
	}

	got, found, err := st.QuotaSnapshot(acc.ID)
	if err != nil || !found {
		t.Fatalf("读快照 = %+v / %v / %v", got, found, err)
	}
	if got.Source != quota.SourceCommandCode || got.Status != quota.StatusOK ||
		got.DisplayName != "thesumwaimzkz" || !got.BalanceKnown || got.Balance != 24.6301 ||
		!got.UsedKnown || got.Used != 45.33 || got.Currency != "USD" {
		t.Fatalf("基本字段 = %+v", got)
	}
	requireQuotaPtr(t, "BalanceUSD", got.BalanceUSD, 24.6301)
	requireQuotaPtr(t, "UsedUSD", got.UsedUSD, 45.33)
	requireQuotaPtr(t, "TotalUSD", got.TotalUSD, 69.9601)
	requireQuotaPtr(t, "RemainingRatio", got.RemainingRatio, 24.6301/69.9601)
	if got.PeriodEnd != periodEnd || got.PlanID != "pro" || got.PlanStatus != "active" || !got.CancelAtPeriodEnd {
		t.Fatalf("订阅字段 = %+v", got)
	}
	if got.FiveHour == nil || got.FiveHour.Used == nil || *got.FiveHour.Used != 2 ||
		got.FiveHour.Limit == nil || *got.FiveHour.Limit != 10 || got.FiveHour.ResetAt != 1760000000000 {
		t.Fatalf("5 小时窗口 = %+v", got.FiveHour)
	}
	if got.Weekly == nil || got.Weekly.Used == nil || *got.Weekly.Used != 30 || got.Weekly.ResetAt != 1760500000000 {
		t.Fatalf("每周窗口 = %+v", got.Weekly)
	}
	if got.Usage.TotalCredits == nil || *got.Usage.TotalCredits != 45.33 ||
		got.Usage.Requests == nil || *got.Usage.Requests != 123 ||
		got.Usage.SuccessRate == nil || *got.Usage.SuccessRate != 0.98 {
		t.Fatalf("上游用量 = %+v", got.Usage)
	}
	if got.Available == nil || !*got.Available || got.CheckedAt != 1700000000 {
		t.Fatalf("可用/刷新时间 = %+v", got)
	}
	if got.SourceErrors["whoami"] != "超时" {
		t.Fatalf("端点错误 = %+v", got.SourceErrors)
	}

	// 覆盖写：同一账号只留一行。
	facts.Balance = 30
	facts.Used = 40
	facts = quota.Apply(facts, cfg)
	if err := st.SaveQuotaSnapshot(acc.ID, facts); err != nil {
		t.Fatalf("覆盖快照: %v", err)
	}
	got, found, err = st.QuotaSnapshot(acc.ID)
	if err != nil || !found {
		t.Fatalf("再读快照 = %+v / %v / %v", got, found, err)
	}
	if got.Balance != 30 || got.Used != 40 {
		t.Fatalf("覆盖后的余额 = %+v", got)
	}
	requireQuotaPtr(t, "覆盖后的 TotalUSD", got.TotalUSD, 70)
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM quota_snapshots WHERE account_id = ?`, acc.ID).Scan(&n); err != nil {
		t.Fatalf("数快照行: %v", err)
	}
	if n != 1 {
		t.Fatalf("快照行数 = %d，期望 1", n)
	}
	var updated int64
	if err := st.DB().QueryRow(`SELECT updated_at FROM quota_snapshots WHERE account_id = ?`, acc.ID).Scan(&updated); err != nil {
		t.Fatalf("读 updated_at: %v", err)
	}
	if updated <= 0 {
		t.Fatalf("updated_at = %d，期望写入时间", updated)
	}

	// 写快照不能顺带改设置。
	gotCfg, found, err = st.QuotaSettings(acc.ID)
	if err != nil || !found || gotCfg.ExpiryDays != 5 || gotCfg.FXRate == nil || *gotCfg.FXRate != 0.14 {
		t.Fatalf("快照写入后设置 = %+v / %v / %v", gotCfg, found, err)
	}
}

func TestQuotaSettingsDefaultsValidationAndDelete(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "qb", nil)

	def := quota.DefaultSettings()
	got, found, err := st.QuotaSettings(acc.ID)
	if err != nil || found {
		t.Fatalf("没有设置行时 = %+v / %v / %v", got, found, err)
	}
	if got.Source != def.Source || got.FXRate != nil || got.AutoRefresh != def.AutoRefresh ||
		got.ExpiryMode != def.ExpiryMode || got.ExpiryDays != def.ExpiryDays || got.BaseURL != def.BaseURL {
		t.Fatalf("默认设置 = %+v，期望 %+v", got, def)
	}
	all, err := st.AllQuotaSettings()
	if err != nil || len(all) != 0 {
		t.Fatalf("全部设置 = %+v / %v，期望空", all, err)
	}

	bad := []quota.Settings{}
	c := quota.DefaultSettings()
	c.Source = "nope"
	bad = append(bad, c)
	c = quota.DefaultSettings()
	c.FXRate = qf(0)
	bad = append(bad, c)
	c = quota.DefaultSettings()
	c.ExpiryMode = "nope"
	bad = append(bad, c)
	c = quota.DefaultSettings()
	c.ExpiryDays = 0
	bad = append(bad, c)
	for i, cfg := range bad {
		if err := st.SetQuotaSettings(acc.ID, cfg); err == nil {
			t.Fatalf("非法设置 %d 应被拒绝: %+v", i, cfg)
		}
	}
	if _, found, err := st.QuotaSettings(acc.ID); err != nil || found {
		t.Fatalf("拒绝写入后不该留下设置行: found=%v err=%v", found, err)
	}

	valid := quota.Settings{
		Source: quota.SourceOff, FXRate: qf(0.14), AutoRefresh: true,
		ExpiryMode: quota.ExpiryModeDeprioritize, ExpiryDays: 10,
		BaseURL: "https://quota.example.com",
	}
	if err := st.SetQuotaSettings(acc.ID, valid); err != nil {
		t.Fatalf("写合法设置: %v", err)
	}
	got, found, err = st.QuotaSettings(acc.ID)
	if err != nil || !found || got.Source != quota.SourceOff || got.FXRate == nil || *got.FXRate != 0.14 ||
		!got.AutoRefresh || got.ExpiryMode != quota.ExpiryModeDeprioritize || got.ExpiryDays != 10 {
		t.Fatalf("读回设置 = %+v / %v / %v", got, found, err)
	}

	removed, err := st.DeleteQuotaSettings(acc.ID)
	if err != nil || !removed {
		t.Fatalf("删设置 = %v / %v", removed, err)
	}
	if removed, err := st.DeleteQuotaSettings(acc.ID); err != nil || removed {
		t.Fatalf("重复删设置 = %v / %v", removed, err)
	}
	if _, found, err := st.QuotaSettings(acc.ID); err != nil || found {
		t.Fatalf("删后仍有设置: found=%v err=%v", found, err)
	}
}

// TestQuotaEntriesUsesOnlySnapshotsAndCurrentSettings 钉住两件事：
// 选号侧只看到有快照的账号；USD 值用当前设置重算，改汇率立即生效。
func TestQuotaEntriesUsesOnlySnapshotsAndCurrentSettings(t *testing.T) {
	st := openTemp(t)
	a := newAccount(t, st, "qa", nil)
	b := newAccount(t, st, "qb", nil)

	cfg := quota.DefaultSettings()
	cfg.FXRate = qf(0.14)
	if err := st.SetQuotaSettings(a.ID, cfg); err != nil {
		t.Fatalf("写设置: %v", err)
	}
	raw := quota.Facts{
		Source: quota.SourceDeepSeek, Status: quota.StatusOK,
		Balance: 117.84, BalanceKnown: true, Currency: "CNY", CheckedAt: 1700000000,
	}
	if err := st.SaveQuotaSnapshot(a.ID, raw); err != nil {
		t.Fatalf("存快照: %v", err)
	}

	entries, err := st.QuotaEntries()
	if err != nil {
		t.Fatalf("读额度条目: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("额度条目 = %+v，期望只有有快照的账号", entries)
	}
	entry, ok := entries[a.ID]
	if !ok {
		t.Fatalf("缺少账号 %d 的额度条目", a.ID)
	}
	if _, ok := entries[b.ID]; ok {
		t.Fatal("没有快照的账号不该出现在额度条目里")
	}
	requireQuotaPtr(t, "折算余额", entry.Facts.BalanceUSD, 117.84*0.14)
	if entry.Facts.UsedUSD != nil || entry.Facts.TotalUSD != nil || entry.Facts.RemainingRatio != nil {
		t.Fatalf("DeepSeek 只有余额时不应算出已用/总额/比例: %+v", entry.Facts)
	}
	if entry.Settings.FXRate == nil || *entry.Settings.FXRate != 0.14 {
		t.Fatalf("条目设置 = %+v", entry.Settings)
	}

	cfg.FXRate = qf(0.10)
	if err := st.SetQuotaSettings(a.ID, cfg); err != nil {
		t.Fatalf("改汇率: %v", err)
	}
	entries, err = st.QuotaEntries()
	if err != nil {
		t.Fatalf("再读额度条目: %v", err)
	}
	requireQuotaPtr(t, "改汇率后的折算余额", entries[a.ID].Facts.BalanceUSD, 11.784)

	// 关闭监控后，即使旧快照还在，选号侧也必须看不到它：否则 quota-min-usd 会按
	// 旧余额把账号排除，网关报「额度过滤后没有候选」。
	cfg.Source = quota.SourceOff
	if err := st.SetQuotaSettings(a.ID, cfg); err != nil {
		t.Fatalf("关闭监控: %v", err)
	}
	entries, err = st.QuotaEntries()
	if err != nil {
		t.Fatalf("关闭监控后读额度条目: %v", err)
	}
	if _, ok := entries[a.ID]; ok {
		t.Fatalf("source=off 的账号不该出现在额度条目里: %+v", entries)
	}

	// 删除快照幂等：第一次删到行，第二次没有行。
	removed, err := st.DeleteQuotaSnapshot(a.ID)
	if err != nil || !removed {
		t.Fatalf("删除快照 = %v / %v，期望删到行", removed, err)
	}
	removed, err = st.DeleteQuotaSnapshot(a.ID)
	if err != nil || removed {
		t.Fatalf("重复删除快照 = %v / %v，期望 false / nil", removed, err)
	}
}

func TestQuotaDeleteAccountCascades(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "qc", nil)
	if err := st.SaveQuotaSnapshot(acc.ID, quota.Facts{Status: quota.StatusOK, CheckedAt: 1}); err != nil {
		t.Fatalf("存快照: %v", err)
	}
	if err := st.SetQuotaSettings(acc.ID, quota.DefaultSettings()); err != nil {
		t.Fatalf("写设置: %v", err)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE account_id = ?`, acc.ID).Scan(&n); err != nil {
			t.Fatalf("数 %s: %v", table, err)
		}
		return n
	}
	if n := count("quota_snapshots"); n != 1 {
		t.Fatalf("删除前快照行 = %d", n)
	}
	if n := count("quota_settings"); n != 1 {
		t.Fatalf("删除前设置行 = %d", n)
	}
	if err := st.DeleteAccount(acc.Name); err != nil {
		t.Fatalf("删账号: %v", err)
	}
	if n := count("quota_snapshots"); n != 0 {
		t.Fatalf("删账号后快照行 = %d，期望级联删除", n)
	}
	if n := count("quota_settings"); n != 0 {
		t.Fatalf("删账号后设置行 = %d，期望级联删除", n)
	}
	if _, found, err := st.QuotaSnapshot(acc.ID); err != nil || found {
		t.Fatalf("删账号后读快照 = found=%v err=%v", found, err)
	}
	if _, found, err := st.QuotaSettings(acc.ID); err != nil || found {
		t.Fatalf("删账号后读设置 = found=%v err=%v", found, err)
	}
}

// TestQuotaUnknownUSDStaysNull 钉住「汇率未知时 USD 指针存回来仍然是空」，
// 不能把「不知道」读成 0，否则 min_usd 过滤会误伤。
func TestQuotaUnknownUSDStaysNull(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "qd", nil)
	facts := quota.Apply(quota.Facts{
		Source: quota.SourceDeepSeek, Status: quota.StatusOK,
		Balance: 117.84, BalanceKnown: true, Currency: "CNY", CheckedAt: 1700000000,
	}, quota.DefaultSettings())
	if err := st.SaveQuotaSnapshot(acc.ID, facts); err != nil {
		t.Fatalf("存快照: %v", err)
	}
	got, found, err := st.QuotaSnapshot(acc.ID)
	if err != nil || !found {
		t.Fatalf("读快照 = %+v / %v / %v", got, found, err)
	}
	if got.BalanceUSD != nil || got.UsedUSD != nil || got.TotalUSD != nil ||
		got.RemainingRatio != nil || got.FXRate != nil {
		t.Fatalf("汇率未知时应保持空指针: %+v", got)
	}
	if !got.BalanceKnown || got.Balance != 117.84 || got.Currency != "CNY" {
		t.Fatalf("原值不该丢: %+v", got)
	}
	if got.FiveHour != nil || got.Weekly != nil || got.PeriodEnd != 0 {
		t.Fatalf("没有窗口/周期时应为空: %+v", got)
	}
}
