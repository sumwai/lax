package store_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"lax/internal/store"

	_ "github.com/glebarez/go-sqlite"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/sb.db")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAccountLifecycle(t *testing.T) {
	st := openTemp(t)

	acc := store.Account{
		Name:      "ds1",
		Provider:  "deepseek",
		Protocol:  "openai-chat",
		BaseURL:   "https://api.deepseek.com",
		KeyCipher: []byte{1, 2, 3},
		Models:    []string{"m1", "m2"},
		Enabled:   true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增: %v", err)
	}
	if acc.ID == 0 {
		t.Fatal("新增后应有 id")
	}

	got, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("按名称取: %v", err)
	}
	if len(got.Models) != 2 || got.Models[1] != "m2" {
		t.Fatalf("模型 = %v", got.Models)
	}
	if !got.Enabled {
		t.Fatal("新账号应启用")
	}

	if err := st.CreateAccount(&store.Account{Name: "ds1", Provider: "x", Protocol: "openai-chat", BaseURL: "u", KeyCipher: []byte{1}}); err == nil {
		t.Fatal("重名应报错")
	}

	if err := st.RecordFailure(acc.ID, "m1", time.Now().Add(time.Minute).UnixMilli(), "限流"); err != nil {
		t.Fatalf("记失败: %v", err)
	}
	if err := st.SetEnabled(acc.ID, false, "key 无效"); err != nil {
		t.Fatalf("停用: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("再取: %v", err)
	}
	if got.Enabled || got.LastError != "key 无效" {
		t.Fatalf("状态 = %+v", got)
	}
	if got.ModelFailures["m1"].FailStreak != 1 || got.ModelFailures["m1"].CooldownUntilMS == 0 {
		t.Fatalf("m1 冷却状态 = %+v，期望 1 次连续失败且截止时间非零", got.ModelFailures["m1"])
	}

	if err := st.RecordFailure(acc.ID, "m1", time.Now().Add(2*time.Minute).UnixMilli(), "还是限流"); err != nil {
		t.Fatalf("再记失败: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("第三次取: %v", err)
	}
	if got.ModelFailures["m1"].FailStreak != 2 {
		t.Fatalf("m1 连续失败次数 = %d，期望 2", got.ModelFailures["m1"].FailStreak)
	}
	if got.LastError != "还是限流" {
		t.Fatalf("last_error = %q，期望更新为最近一次失败", got.LastError)
	}

	if err := st.ClearFailure(acc.ID, "m1"); err != nil {
		t.Fatalf("清失败: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("第四次取: %v", err)
	}
	if f := got.ModelFailures["m1"]; f.FailStreak != 0 || f.CooldownUntilMS != 0 {
		t.Fatalf("清 m1 冷却后状态 = %+v", f)
	}
	if got.LastError != "还是限流" {
		t.Fatalf("清一个模型的冷却不该抹掉账号最近错误，last_error = %q", got.LastError)
	}

	if err := st.DeleteAccount("ds1"); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if err := st.DeleteAccount("ds1"); err == nil {
		t.Fatal("删不存在的账号应报错")
	}
}

// 冷却按账号-模型隔离：一个模型的失败与清零不碰同账号的其它模型。
func TestModelFailuresAreIndependent(t *testing.T) {
	st := openTemp(t)
	acc := store.Account{
		Name: "ds1", Provider: "deepseek", Protocol: "openai-chat",
		BaseURL: "u", KeyCipher: []byte{1}, Models: []string{"m1", "m2"}, Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增: %v", err)
	}

	until1 := time.Now().Add(time.Second).UnixMilli()
	if err := st.RecordFailure(acc.ID, "m1", until1, "m1 限流"); err != nil {
		t.Fatalf("记 m1 失败: %v", err)
	}
	got, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if f := got.ModelFailures["m1"]; f.FailStreak != 1 || f.CooldownUntilMS != until1 {
		t.Fatalf("m1 冷却 = %+v，期望 1 次与 %d", f, until1)
	}
	if f, ok := got.ModelFailures["m2"]; ok {
		t.Fatalf("m1 失败不该影响 m2: %+v", f)
	}

	if err := st.RecordFailure(acc.ID, "m1", until1, "m1 又限流"); err != nil {
		t.Fatalf("再记 m1 失败: %v", err)
	}
	if err := st.RecordFailure(acc.ID, "m2", until1, "m2 限流"); err != nil {
		t.Fatalf("记 m2 失败: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("再读账号: %v", err)
	}
	if got.ModelFailures["m1"].FailStreak != 2 || got.ModelFailures["m2"].FailStreak != 1 {
		t.Fatalf("连续失败次数 m1/m2 = %d/%d，期望 2/1",
			got.ModelFailures["m1"].FailStreak, got.ModelFailures["m2"].FailStreak)
	}

	if err := st.ClearFailure(acc.ID, "m1"); err != nil {
		t.Fatalf("清 m1: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("清后读账号: %v", err)
	}
	if f := got.ModelFailures["m1"]; f.FailStreak != 0 || f.CooldownUntilMS != 0 {
		t.Fatalf("m1 没清干净: %+v", f)
	}
	if got.ModelFailures["m2"].FailStreak != 1 {
		t.Fatalf("清 m1 不该清 m2: %+v", got.ModelFailures["m2"])
	}

	if err := st.ClearFailures(acc.ID); err != nil {
		t.Fatalf("清全部: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("清全部后读账号: %v", err)
	}
	if len(got.ModelFailures) != 0 {
		t.Fatalf("清全部后仍有冷却: %+v", got.ModelFailures)
	}
}

func TestRequestRecordsAndUsage(t *testing.T) {
	st := openTemp(t)

	priceIn, priceOut := 2.0, 4.0
	acc := store.Account{
		Name: "ds1", Provider: "deepseek", Protocol: "openai-chat",
		BaseURL: "u", KeyCipher: []byte{1}, Models: []string{"m1"},
		Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增: %v", err)
	}
	// 单价不在账号上：写到这个账号在这个模型上的覆盖值。
	if err := st.SetAccountModelPrices("ds1", "m1", store.ModelPrices{
		In: &priceIn, Out: &priceOut,
	}); err != nil {
		t.Fatalf("写单价: %v", err)
	}

	now := time.Now().Unix()
	rec := store.RequestRecord{
		TS: now, Inbound: "anthropic", Outbound: "openai-chat",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true,
		// 这里直接给归一化之后的数：网关入账时按出站协议算它，本用例只关心汇总与折价。
		InTokens: 1_000_000, InTokensUncached: 1_000_000, OutTokens: 1_000_000,
		LatencyMS: 120,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写记录: %v", err)
	}

	recs, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK || recs[0].InTokens != 1_000_000 {
		t.Fatalf("记录 = %+v", recs)
	}

	rows, err := st.Usage("account", now-60, 0)
	if err != nil {
		t.Fatalf("按账号汇总: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if rows[0].Key != "ds1" || rows[0].Requests != 1 {
		t.Fatalf("行 = %+v", rows[0])
	}
	if rows[0].Cost == nil || *rows[0].Cost != 6.0 {
		t.Fatalf("花费 = %v，期望 6（1M 输入 2 元 + 1M 输出 4 元）", rows[0].Cost)
	}

	rows, err = st.Usage("model", now-60, 0)
	if err != nil {
		t.Fatalf("按模型汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != "m1" {
		t.Fatalf("按模型汇总 = %+v", rows)
	}

	if _, err := st.Usage("nope", now-60, 0); err == nil {
		t.Fatal("不支持的维度应报错")
	}

	rows, err = st.Usage("day", now-60*60*24*2, 0)
	if err != nil {
		t.Fatalf("按日期汇总: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Key) != 10 {
		t.Fatalf("按日期汇总 = %+v", rows)
	}
}

// TestUsageRangeUpperBoundIsExclusive 钉住 [since, until) 的边界：
// 起始时刻算在内，结束时刻不算；until 为 0 时不设上界。
func TestUsageRangeUpperBoundIsExclusive(t *testing.T) {
	st := openTemp(t)
	acc := store.Account{
		Name: "range", Provider: "deepseek", Protocol: "openai-chat",
		BaseURL: "u", KeyCipher: []byte{1}, Models: []string{"m-range"}, Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增账号: %v", err)
	}

	base := time.Now().Unix()
	for i, ts := range []int64{base - 5400, base - 3600, base - 1800} {
		rec := store.RequestRecord{
			TS: ts, Inbound: "anthropic", Outbound: "anthropic",
			ModelRequested: "m-range", ModelUsed: "m-range", AccountID: acc.ID,
			HTTPStatus: 200, OK: true, InTokens: int64(10 * (i + 1)),
		}
		if err := st.InsertRequest(&rec); err != nil {
			t.Fatalf("写第 %d 条记录: %v", i+1, err)
		}
	}

	rows, err := st.Usage("model", base-5400, base-1800)
	if err != nil {
		t.Fatalf("按区间汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].InTokens != 30 {
		t.Fatalf("区间内汇总 = %+v，期望 2 条、输入 30（起点计入，终点不计）", rows)
	}

	rows, err = st.Usage("model", base-5400, 0)
	if err != nil {
		t.Fatalf("不设上界汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 3 {
		t.Fatalf("不设上界的汇总 = %+v，期望 3 条", rows)
	}

	if _, err := st.Usage("model", base-1800, base-1800); err == nil {
		t.Fatal("结束时间等于起始时间应报错")
	}
}

// TestRequestsAfterReturnsEveryNewRow 钉住跟随读取按 id 升序分批，
// 不会因为一次只取最新 N 条而永久跳过突发里的旧行。
func TestRequestsAfterReturnsEveryNewRow(t *testing.T) {
	st := openTemp(t)
	acc := store.Account{
		Name: "follow", Provider: "deepseek", Protocol: "openai-chat",
		BaseURL: "u", KeyCipher: []byte{1}, Models: []string{"m-follow"}, Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增账号: %v", err)
	}

	base := time.Now().Unix()
	ids := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		rec := store.RequestRecord{
			TS: base - int64(2-i)*60, Inbound: "anthropic", Outbound: "anthropic",
			ModelRequested: "m-follow", ModelUsed: "m-follow", AccountID: acc.ID,
			HTTPStatus: 200, OK: true, InTokens: int64(i + 1),
		}
		if err := st.InsertRequest(&rec); err != nil {
			t.Fatalf("写第 %d 条记录: %v", i+1, err)
		}
		ids = append(ids, rec.ID)
	}

	first, err := st.RequestsAfter(ids[0], 2, 0, 0)
	if err != nil {
		t.Fatalf("读第一批: %v", err)
	}
	if len(first) != 2 || first[0].ID != ids[1] || first[1].ID != ids[2] {
		t.Fatalf("第一批 = %+v，期望按 id 升序的 %d、%d", first, ids[1], ids[2])
	}

	second, err := st.RequestsAfter(first[len(first)-1].ID, 2, 0, 0)
	if err != nil {
		t.Fatalf("读第二批: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("第二批应没有新行，实际 %+v", second)
	}

	// 起始时间也算过滤：只看最后两分钟时，第一条更早的记录不应出现。
	recent, err := st.RequestsAfter(ids[0], 10, base-60, 0)
	if err != nil {
		t.Fatalf("按时间过滤: %v", err)
	}
	if len(recent) != 2 || recent[0].ID != ids[1] || recent[1].ID != ids[2] {
		t.Fatalf("按时间过滤 = %+v，期望只保留 %d、%d", recent, ids[1], ids[2])
	}
}

// TestPluginLogsAfterReturnsEveryNewRow 钉住 plugin logs --follow 的分批读取：
// 按 id 升序返回，一轮突发超过抓取上限时分批追完，不会漏中间行。
func TestPluginLogsAfterReturnsEveryNewRow(t *testing.T) {
	st := openTemp(t)
	ids := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		entry := store.PluginLog{
			PluginID: "com.example.echo", Level: "info",
			Message: "line", At: time.Now().Add(time.Duration(i) * time.Second).Unix(),
		}
		if err := st.AppendPluginLog(entry); err != nil {
			t.Fatalf("写第 %d 条日志: %v", i+1, err)
		}
		rows, err := st.PluginLogs("com.example.echo", 3, "", 0)
		if err != nil {
			t.Fatalf("读第 %d 条日志 ID: %v", i+1, err)
		}
		ids = append(ids, rows[0].ID)
	}

	first, err := st.PluginLogsAfter("com.example.echo", ids[0], 2, "", 0, 0)
	if err != nil {
		t.Fatalf("读第一批: %v", err)
	}
	if len(first) != 2 || first[0].ID != ids[1] || first[1].ID != ids[2] {
		t.Fatalf("第一批 = %+v，期望按 id 升序的 %d、%d", first, ids[1], ids[2])
	}

	second, err := st.PluginLogsAfter("com.example.echo", first[len(first)-1].ID, 2, "", 0, 0)
	if err != nil {
		t.Fatalf("读第二批: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("第二批应没有新行，实际 %+v", second)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sb.db"

	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("第一次打开: %v", err)
	}
	if err := first.CreateAccount(&store.Account{
		Name: "a", Provider: "p", Protocol: "openai-chat", BaseURL: "u", KeyCipher: []byte{1}, Enabled: true,
	}); err != nil {
		t.Fatalf("新增: %v", err)
	}
	_ = first.Close()

	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("第二次打开: %v", err)
	}
	defer second.Close()

	accounts, err := second.ListAccounts()
	if err != nil {
		t.Fatalf("列表: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("重复打开后账号数 = %d，迁移不应重跑", len(accounts))
	}
}

// —— 模型同步与探活：account_models ——

func newAccount(t *testing.T, st *store.Store, name string, models []string) store.Account {
	t.Helper()
	acc := store.Account{
		Name:      name,
		Provider:  "test",
		Protocol:  "openai-chat",
		BaseURL:   "http://127.0.0.1:1",
		KeyCipher: []byte{1, 2, 3},
		Models:    models,
		Enabled:   true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号 %s: %v", name, err)
	}
	return acc
}

// modelStates 把模型状态整理成 "模型/状态" 列表，方便整表比对。
func modelStates(t *testing.T, st *store.Store, accountID int64) []string {
	t.Helper()
	rows, err := st.AccountModels(accountID)
	if err != nil {
		t.Fatalf("读模型状态: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Model+"/"+r.Status)
	}
	return out
}

func TestReplaceModelsFirstInsert(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", nil)

	added, err := st.ReplaceModels(acc.ID, []string{"m2", "m1"})
	if err != nil {
		t.Fatalf("刷新模型: %v", err)
	}
	if added != 2 {
		t.Fatalf("新增数 = %d，期望 2", added)
	}

	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型状态: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 = %d，期望 2", len(rows))
	}
	if rows[0].Model != "m1" || rows[1].Model != "m2" {
		t.Fatalf("应按模型名排序，得到 %q、%q", rows[0].Model, rows[1].Model)
	}
	for _, r := range rows {
		if r.Status != store.ModelSynced || r.HTTPStatus != 0 || r.Error != "" || r.CheckedAt != 0 {
			t.Fatalf("新行应是未探活的 synced: %+v", r)
		}
	}

	// 同样的列表再来一次，没有新增。
	added, err = st.ReplaceModels(acc.ID, []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("重复刷新: %v", err)
	}
	if added != 0 {
		t.Fatalf("重复刷新新增数 = %d，期望 0", added)
	}
}

// TestReplaceModelsKeepsProbeState 钉住同步只做加法：清单里没出现的名字不删、不清状态
// （迁移 0015 之前那种「消失的行删除」随 accounts.models 一起没了）。
func TestReplaceModelsKeepsProbeState(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", nil)

	if _, err := st.ReplaceModels(acc.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("首次刷新: %v", err)
	}
	if err := st.SetModelStatus(acc.ID, "m1", store.ModelUsable, 200, ""); err != nil {
		t.Fatalf("记录探活结果: %v", err)
	}

	// m2 从上游列表里消失，m3 是新出现的。
	added, err := st.ReplaceModels(acc.ID, []string{"m3", "m1"})
	if err != nil {
		t.Fatalf("第二次刷新: %v", err)
	}
	if added != 1 {
		t.Fatalf("新增数 = %d，期望 1（只有 m3 是新的）", added)
	}

	got := strings.Join(modelStates(t, st, acc.ID), ",")
	want := "m1/usable,m2/synced,m3/synced"
	if got != want {
		t.Fatalf("刷新后 = %s，期望 %s（usable 保留、m2 不删、m3 记为 synced）", got, want)
	}
}

// TestReplaceModelsMarksAllowed 钉住新语义：清单里的名字一律标 allowed，
// 其余行与另外的账号都不受影响；空清单等于什么都不做。
func TestReplaceModelsMarksAllowed(t *testing.T) {
	st := openTemp(t)
	one := newAccount(t, st, "ds1", nil)
	two := newAccount(t, st, "ds2", nil)

	if _, err := st.ReplaceModels(one.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("刷新 ds1: %v", err)
	}
	if _, err := st.ReplaceModels(two.ID, []string{"m9"}); err != nil {
		t.Fatalf("刷新 ds2: %v", err)
	}
	// 上一轮探出 m2 无权：下一次同步再看到它时标回 allowed，但不碰探活时间与备注。
	if err := st.SetAccountModelAccess(one.ID, "m2", store.AccessForbidden, 403, "套餐不含该模型", 111); err != nil {
		t.Fatalf("记录无权: %v", err)
	}

	added, err := st.ReplaceModels(one.ID, []string{"m2", "m3"})
	if err != nil {
		t.Fatalf("再刷新 ds1: %v", err)
	}
	if added != 1 {
		t.Fatalf("新增数 = %d，期望 1（只有 m3 是新的）", added)
	}

	rows, err := st.AccountModels(one.ID)
	if err != nil {
		t.Fatalf("读 ds1 的模型行: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("ds1 的模型行数 = %d，期望 3（m1、m2、m3 都在）", len(rows))
	}
	for _, r := range rows {
		if r.Access != store.AccessAllowed {
			t.Fatalf("%s 的可达性 = %q，期望 %q", r.Model, r.Access, store.AccessAllowed)
		}
		if r.Model == "m2" && (r.HTTPStatus != 403 || r.Error != "套餐不含该模型" || r.CheckedAt != 111) {
			t.Fatalf("标 allowed 不该动上一轮的探活结论: %+v", r)
		}
	}

	// 空清单不删任何行：这是「这次没有新名字」，不是「清空账号」。
	if added, err := st.ReplaceModels(one.ID, nil); err != nil || added != 0 {
		t.Fatalf("空清单: added = %d, err = %v，期望什么都不做", added, err)
	}
	if got := modelStates(t, st, one.ID); len(got) != 3 {
		t.Fatalf("空清单之后 ds1 还剩 %v，期望 3 行不动", got)
	}
	if got := modelStates(t, st, two.ID); len(got) != 1 || got[0] != "m9/synced" {
		t.Fatalf("ds2 不应受影响，得到 %v", got)
	}

	// 账号不存在时读不到厂商，目录写不了：这一条要报错，不能悄悄只写可达性。
	if _, err := st.ReplaceModels(9999, []string{"m1"}); err == nil {
		t.Fatal("账号不存在时应报错")
	}
}

// TestReplaceModelsWritesManualCatalog 确认同步进来的名字在目录里补成 source='manual' 的行，
// 且不覆盖已有的目录行（同步只说明名字存在，不提供长度、价格与协议；已有的行连 source 都不动，
// 否则这一行会被钉成 manual，之后同步再也写不进去，见 providerModelInsertOnly）。
func TestReplaceModelsWritesManualCatalog(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", nil)

	window := int64(200000)
	if err := st.UpsertProviderModels("test", []store.ProviderModel{{
		Model: "m1", Source: store.SourceSync, ContextWindow: &window,
		PriceIn: floatPtr(1.5), Protocols: []string{"openai-chat"},
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}

	if _, err := st.ReplaceModels(acc.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("刷新模型: %v", err)
	}

	models, err := st.ProviderModels("test")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("目录里的模型数 = %d，期望 2", len(models))
	}
	if models[0].Model != "m1" || models[0].Source != store.SourceSync {
		t.Fatalf("m1 的来源 = %q，期望保持 sync（已有的目录行一个字段都不动）", models[0].Source)
	}
	if got := strings.Join(models[0].Protocols, ","); got != "openai-chat" {
		t.Fatalf("m1 声明的协议 = %q，期望原样保留", got)
	}
	if models[0].ContextWindow == nil || *models[0].ContextWindow != window {
		t.Fatalf("m1 的上下文长度被抹掉: %+v", models[0])
	}
	if models[1].Model != "m2" || models[1].Source != store.SourceManual || models[1].ContextWindow != nil {
		t.Fatalf("m2 应是只有名字的 manual 行: %+v", models[1])
	}
}

func intPtr(v int64) *int64 { return &v }

func TestSetModelStatusUpserts(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", nil)

	if err := st.SetModelStatus(acc.ID, "m1", store.ModelForbidden, 403, "套餐不含该模型"); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if got := modelStates(t, st, acc.ID); len(got) != 1 || got[0] != "m1/forbidden" {
		t.Fatalf("首次写入后 = %v", got)
	}

	// 把 checked_at 拨回过去，好让第二次写入的时间戳变化可被观察到；同时确认没有多出一行。
	if _, err := st.DB().Exec(`UPDATE account_models SET checked_at = 1 WHERE account_id = ? AND model = ?`, acc.ID, "m1"); err != nil {
		t.Fatalf("调整 checked_at: %v", err)
	}
	if err := st.SetModelStatus(acc.ID, "m1", store.ModelUsable, 200, ""); err != nil {
		t.Fatalf("二次写入: %v", err)
	}

	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型状态: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，同一 (account_id, model) 应只占一行", len(rows))
	}
	if rows[0].Status != store.ModelUsable || rows[0].HTTPStatus != 200 || rows[0].Error != "" {
		t.Fatalf("二次写入后 = %+v", rows[0])
	}
	if rows[0].CheckedAt <= 1 {
		t.Fatalf("checked_at = %d，二次写入应更新时间", rows[0].CheckedAt)
	}
}

// TestAccountModelsDerivedFromAccess 钉住 Account.Models 是派生值：
// 它等于该账号 access='allowed' 的行集合，`account add --models` 与探活各写一半。
func TestAccountModelsDerivedFromAccess(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", []string{"init"})

	if err := st.SetModels(acc.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("标 allowed: %v", err)
	}
	got, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	// 加法语义：init 仍在，新名字按模型名升序排在后面。
	if strings.Join(got.Models, ",") != "init,m1,m2" {
		t.Fatalf("账号模型列表 = %v，期望 [init m1 m2]", got.Models)
	}

	// 标成不可达就从选号列表里消失，但行本身还在（结论是探活的一部分）。
	if err := st.SetAccountModelAccess(acc.ID, "m1", store.AccessForbidden, 403, "无权", 222); err != nil {
		t.Fatalf("记无权: %v", err)
	}
	if got, err = st.GetAccount("ds1"); err != nil {
		t.Fatalf("再读账号: %v", err)
	}
	if strings.Join(got.Models, ",") != "init,m2" {
		t.Fatalf("账号模型列表 = %v，期望 [init m2]", got.Models)
	}
	if allowed, err := st.AllowedModels(acc.ID); err != nil {
		t.Fatalf("读可达性: %v", err)
	} else if strings.Join(allowed, ",") != "init,m2" {
		t.Fatalf("AllowedModels = %v，期望 [init m2]", allowed)
	}

	// 空列表不删行：SetModels 只剩加法。
	if err := st.SetModels(acc.ID, nil); err != nil {
		t.Fatalf("空列表: %v", err)
	}
	if got, err = st.GetAccount("ds1"); err != nil {
		t.Fatalf("空列表后读账号: %v", err)
	}
	if strings.Join(got.Models, ",") != "init,m2" {
		t.Fatalf("空列表后模型列表 = %v，期望原样不动", got.Models)
	}
}

// 删账号要连带删模型行：SQLite 的 rowid 会被复用，遗留行会让新账号继承旧状态。
func TestDeleteAccountClearsModelRows(t *testing.T) {
	st := openTemp(t)

	first := newAccount(t, st, "first", nil)
	if _, err := st.ReplaceModels(first.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("写入模型: %v", err)
	}
	if err := st.SetModelStatus(first.ID, "m1", store.ModelUsable, 200, ""); err != nil {
		t.Fatalf("记录探活: %v", err)
	}

	if err := st.DeleteAccount("first"); err != nil {
		t.Fatalf("删除账号: %v", err)
	}
	left, err := st.AllAccountModels()
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("删账号后还剩 %d 行模型状态: %+v", len(left), left)
	}

	second := newAccount(t, st, "second", nil)
	if rows, err := st.AccountModels(second.ID); err != nil {
		t.Fatalf("读新账号模型: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("新账号继承了旧账号的模型行: %+v", rows)
	}
}

// —— 模型级协议：account_models.protocol（迁移 0005） ——

// TestMigrationsRunToLatestFromEmptyDB 确认空库能一路跑到最新的 user_version，
// 且新列的默认值就是「继承账号协议」的空串。
func TestMigrationsRunToLatestFromEmptyDB(t *testing.T) {
	st := openTemp(t)

	assertLatestVersion(t, st)

	acc := newAccount(t, st, "ds1", nil)
	if _, err := st.ReplaceModels(acc.ID, []string{"m1"}); err != nil {
		t.Fatalf("写入模型行: %v", err)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != "" {
		t.Fatalf("新行的协议应为空串（继承账号），实际 %+v", rows)
	}
}

// TestMigration0005UpgradesOldDatabase 模拟一个停在 0004 的库：加列之后老行拿到空串，
// 已有数据与状态原样保留，user_version 推到 5。
func TestMigration0005UpgradesOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	// 0004 之后的形状：accounts 带 fail_streak，account_models 还没有 protocol 列。
	ddl := []string{
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
			protocol TEXT NOT NULL, base_url TEXT NOT NULL, key_cipher BLOB NOT NULL,
			models TEXT NOT NULL DEFAULT '[]', price_in REAL, price_out REAL,
			priority INTEGER NOT NULL DEFAULT 100, enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_until INTEGER, last_error TEXT,
			fail_streak INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE requests (
			id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, inbound TEXT NOT NULL,
			outbound TEXT NOT NULL, model_requested TEXT NOT NULL, model_used TEXT NOT NULL,
			account_id INTEGER, http_status INTEGER NOT NULL, ok INTEGER NOT NULL,
			in_tokens INTEGER NOT NULL DEFAULT 0, out_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0, error TEXT)`,
		`CREATE INDEX requests_ts ON requests(ts DESC)`,
		`CREATE TABLE account_models (
			account_id INTEGER NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
			http_status INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
			checked_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (account_id, model))`,
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('ds1', 'test', 'openai-chat', 'http://127.0.0.1:1', X'010203', '["m1","m2"]', 1, 100)`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at)
			VALUES (1, 'm1', 'usable', 200, '', 12345)`,
		`PRAGMA user_version = 4`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建老库（%s）: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关老库: %v", err)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("升级老库: %v", err)
	}
	defer st.Close()

	assertLatestVersion(t, st)

	rows, err := st.AccountModels(1)
	if err != nil {
		t.Fatalf("读老行: %v", err)
	}
	// 老库里的 m1 有一条 usable 的探活行，手填清单里还有 m2：0015 之后前者是 allowed 的可达性行，
	// 后者补成一行 allowed（迁移 0015）。
	if len(rows) != 2 || rows[0].Model != "m1" || rows[1].Model != "m2" {
		t.Fatalf("老库的模型行 = %+v，期望 m1 与 m2", rows)
	}
	for _, r := range rows {
		if r.Protocol != "" {
			t.Fatalf("%s 的协议应为空串（继承厂商默认协议），实际 %q", r.Model, r.Protocol)
		}
		if r.Access != store.AccessAllowed {
			t.Fatalf("%s 的可达性 = %q，期望 %q", r.Model, r.Access, store.AccessAllowed)
		}
	}
	// 探活结论原样保留：usable 与它的时间戳都在。
	if rows[0].Status != store.ModelUsable || rows[0].HTTPStatus != 200 || rows[0].CheckedAt != 12345 {
		t.Fatalf("老行的探活结论被改动: %+v", rows[0])
	}
	if rows[1].CheckedAt != 0 {
		t.Fatalf("手填清单补的行不带探活时间，实际 %+v", rows[1])
	}

	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	// 迁移 0014 把孤儿账号的地址搬成了一行厂商（名字取账号的 provider，地址取账号的 base_url），
	// 账号的 BaseURL 因此是那条厂商的地址；这家一条路由都没声明，默认协议算不出来，
	// Protocol 就是空串。
	if len(acc.Models) != 2 || acc.BaseURL != "http://127.0.0.1:1" || acc.Protocol != "" {
		t.Fatalf("老账号的派生值不对: %+v", acc)
	}
	p, err := st.GetProvider("test")
	if err != nil {
		t.Fatalf("迁移没给孤儿账号补建厂商: %v", err)
	}
	if p.BaseURL != "http://127.0.0.1:1" || p.DefaultProtocol != "openai-chat" || p.Note != "由旧库搬迁从账号地址补建" {
		t.Fatalf("补建的厂商行不对: %+v", p)
	}

	// 模型行没有协议、厂商又没有路由时，有效协议是空串（不报错，也不去猜）。
	if got, err := st.EffectiveProtocol(acc.ID, "m1"); err != nil {
		t.Fatalf("取有效协议: %v", err)
	} else if got != "" {
		t.Fatalf("有效协议 = %q，期望空串（厂商没有路由，算不出默认协议）", got)
	}
}

// TestSetModelProtocolSurvivesReplaceModels 确认同步不覆盖协议发现结果：
// 已有的协议非空，ReplaceModels 之后还是它，新出现的行才是空串。
func TestSetModelProtocolSurvivesReplaceModels(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", nil)

	if _, err := st.ReplaceModels(acc.ID, []string{"claude-a", "claude-b"}); err != nil {
		t.Fatalf("首次刷新: %v", err)
	}
	if err := st.SetModelProtocol(acc.ID, "claude-a", "anthropic"); err != nil {
		t.Fatalf("记协议: %v", err)
	}
	if err := st.SetModelStatus(acc.ID, "claude-a", store.ModelUsable, 200, ""); err != nil {
		t.Fatalf("记探活结果: %v", err)
	}

	added, err := st.ReplaceModels(acc.ID, []string{"claude-a", "claude-c"})
	if err != nil {
		t.Fatalf("再次刷新: %v", err)
	}
	if added != 1 {
		t.Fatalf("新增数 = %d，期望 1（claude-c 是新的）", added)
	}

	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	byModel := map[string]store.AccountModel{}
	for _, r := range rows {
		byModel[r.Model] = r
	}
	if got := byModel["claude-a"]; got.Protocol != "anthropic" || got.Status != store.ModelUsable {
		t.Fatalf("同步之后 claude-a 的协议与状态被清掉: %+v", got)
	}
	if got := byModel["claude-c"]; got.Protocol != "" || got.Status != store.ModelSynced {
		t.Fatalf("新行应是 synced 且协议为空串: %+v", got)
	}

	// SetModelStatus 只动状态，不动协议。
	if err := st.SetModelStatus(acc.ID, "claude-a", store.ModelForbidden, 403, "套餐不含"); err != nil {
		t.Fatalf("改状态: %v", err)
	}
	if got := modelRow(t, st, acc.ID, "claude-a"); got.Protocol != "anthropic" {
		t.Fatalf("SetModelStatus 改动了协议列: %+v", got)
	}
}

// modelRow 取某个账号某个模型的模型行，读不到就直接失败。
func modelRow(t *testing.T, st *store.Store, accountID int64, model string) store.AccountModel {
	t.Helper()
	rows, err := st.AccountModels(accountID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	for _, r := range rows {
		if r.Model == model {
			return r
		}
	}
	t.Fatalf("找不到模型行 %s", model)
	return store.AccountModel{}
}

// TestEffectiveProtocolFollowsModelAndProvider 确认新签名 (accountID, model) 的三种情况：
// 模型行非空用列值，列为空用账号所属厂商的有效默认协议，厂商算不出默认协议时为空串。
// 厂商的默认协议刻意选 anthropic 而不是固定顺序里的 chat：deepseek 那种两家都声明了路由的
// 厂商，靠排序会算错。
func TestEffectiveProtocolFollowsModelAndProvider(t *testing.T) {
	st := openTemp(t)

	if err := st.AddProvider(store.Provider{
		Name: "co", BaseURL: "https://api.example.com", DefaultProtocol: "anthropic",
	}, []store.ProviderRoute{
		{Provider: "co", Protocol: "openai-chat", Path: "/v1/chat/completions"},
		{Provider: "co", Protocol: "anthropic", Path: "/v1/messages"},
	}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	acc := store.Account{Name: "c1", Provider: "co", KeyCipher: []byte{1}, Models: []string{"m1"}, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}

	if got := store.ResolveProtocol("anthropic", ""); got != "anthropic" {
		t.Fatalf("ResolveProtocol 空列应回退厂商默认协议，得到 %q", got)
	}
	if got := store.ResolveProtocol("anthropic", "openai-chat"); got != "openai-chat" {
		t.Fatalf("ResolveProtocol 非空列应用列值，得到 %q", got)
	}

	// 行不存在：用厂商的有效默认协议。
	if got, err := st.EffectiveProtocol(acc.ID, "m-missing"); err != nil {
		t.Fatalf("行不存在不该报错: %v", err)
	} else if got != "anthropic" {
		t.Fatalf("行不存在时有效协议 = %q，期望厂商默认协议 anthropic", got)
	}

	// 行存在但列为空：同样回退厂商默认协议。
	if _, err := st.ReplaceModels(acc.ID, []string{"m1"}); err != nil {
		t.Fatalf("写入模型行: %v", err)
	}
	if got, err := st.EffectiveProtocol(acc.ID, "m1"); err != nil {
		t.Fatalf("取有效协议: %v", err)
	} else if got != "anthropic" {
		t.Fatalf("列为空时有效协议 = %q，期望厂商默认协议 anthropic", got)
	}

	// 列非空：压过厂商默认协议。另一行不受影响。
	if err := st.SetModelProtocol(acc.ID, "m1", "gemini"); err != nil {
		t.Fatalf("记协议: %v", err)
	}
	if _, err := st.ReplaceModels(acc.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("再加一行: %v", err)
	}
	if got, err := st.EffectiveProtocol(acc.ID, "m1"); err != nil {
		t.Fatalf("取 m1 的有效协议: %v", err)
	} else if got != "gemini" {
		t.Fatalf("列非空时有效协议 = %q，期望用列值 gemini", got)
	}
	if got, err := st.EffectiveProtocol(acc.ID, "m2"); err != nil {
		t.Fatalf("取 m2 的有效协议: %v", err)
	} else if got != "anthropic" {
		t.Fatalf("m2 的有效协议 = %q，期望厂商默认协议 anthropic", got)
	}

	// 厂商一条路由都没声明：算不出默认协议，返回空串，不报错。
	if err := st.AddProvider(store.Provider{Name: "bare", BaseURL: "https://bare.example.com"}, nil); err != nil {
		t.Fatalf("建没有路由的厂商: %v", err)
	}
	bare := store.Account{Name: "b1", Provider: "bare", KeyCipher: []byte{1}, Enabled: true}
	if err := st.CreateAccount(&bare); err != nil {
		t.Fatalf("建 bare 账号: %v", err)
	}
	if got, err := st.EffectiveProtocol(bare.ID, "anything"); err != nil {
		t.Fatalf("厂商没有路由时不该报错: %v", err)
	} else if got != "" {
		t.Fatalf("厂商没有路由时有效协议 = %q，期望空串", got)
	}

	// 旧的三参数写法还有调用方（internal/gateway、internal/cli）：第二个参数被忽略，
	// 第三个参数才是模型名，结果与新写法一致。
	if got, err := st.EffectiveProtocol(acc.ID, "whatever-account-protocol", "m1"); err != nil {
		t.Fatalf("旧写法取有效协议: %v", err)
	} else if got != "gemini" {
		t.Fatalf("旧写法的有效协议 = %q，期望与新写法一样是 gemini", got)
	}
}
