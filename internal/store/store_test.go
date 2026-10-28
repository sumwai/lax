package store_test

import (
	"strings"
	"testing"
	"time"

	"switchboard/internal/store"
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

	price := 2.5
	acc := store.Account{
		Name:      "ds1",
		Provider:  "deepseek",
		Protocol:  "openai-chat",
		BaseURL:   "https://api.deepseek.com",
		KeyCipher: []byte{1, 2, 3},
		Models:    []string{"m1", "m2"},
		PriceIn:   &price,
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
	if got.PriceIn == nil || *got.PriceIn != 2.5 {
		t.Fatalf("价格 = %v", got.PriceIn)
	}
	if !got.Enabled {
		t.Fatal("新账号应启用")
	}

	if err := st.CreateAccount(&store.Account{Name: "ds1", Provider: "x", Protocol: "openai-chat", BaseURL: "u", KeyCipher: []byte{1}}); err == nil {
		t.Fatal("重名应报错")
	}

	if err := st.SetCooldown(acc.ID, time.Now().Add(time.Minute).Unix(), "限流"); err != nil {
		t.Fatalf("设冷却: %v", err)
	}
	if err := st.SetEnabled(acc.ID, false, "key 无效"); err != nil {
		t.Fatalf("停用: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("再取: %v", err)
	}
	if got.CooldownUntil == 0 || got.Enabled || got.LastError != "key 无效" {
		t.Fatalf("状态 = %+v", got)
	}

	if err := st.DeleteAccount("ds1"); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if err := st.DeleteAccount("ds1"); err == nil {
		t.Fatal("删不存在的账号应报错")
	}
}

func TestRequestRecordsAndUsage(t *testing.T) {
	st := openTemp(t)

	priceIn, priceOut := 2.0, 4.0
	acc := store.Account{
		Name: "ds1", Provider: "deepseek", Protocol: "openai-chat",
		BaseURL: "u", KeyCipher: []byte{1}, Models: []string{"m1"},
		PriceIn: &priceIn, PriceOut: &priceOut, Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增: %v", err)
	}

	now := time.Now().Unix()
	rec := store.RequestRecord{
		TS: now, Inbound: "anthropic", Outbound: "openai-chat",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, InTokens: 1_000_000, OutTokens: 1_000_000,
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

	rows, err := st.Usage("account", now-60)
	if err != nil {
		t.Fatalf("按账号汇总: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if rows[0].Key != "ds1" || rows[0].Requests != 1 {
		t.Fatalf("行 = %+v", rows[0])
	}
	if rows[0].Cost != 6.0 {
		t.Fatalf("花费 = %v，期望 6（1M 输入 2 元 + 1M 输出 4 元）", rows[0].Cost)
	}

	rows, err = st.Usage("model", now-60)
	if err != nil {
		t.Fatalf("按模型汇总: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != "m1" {
		t.Fatalf("按模型汇总 = %+v", rows)
	}

	if _, err := st.Usage("nope", now-60); err == nil {
		t.Fatal("不支持的维度应报错")
	}

	rows, err = st.Usage("day", now-60*60*24*2)
	if err != nil {
		t.Fatalf("按日期汇总: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Key) != 10 {
		t.Fatalf("按日期汇总 = %+v", rows)
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
	want := "m1/usable,m3/synced"
	if got != want {
		t.Fatalf("刷新后 = %s，期望 %s（usable 保留、m2 删除、m3 记为 synced）", got, want)
	}
}

func TestReplaceModelsEmptyClearsOnlyOneAccount(t *testing.T) {
	st := openTemp(t)
	one := newAccount(t, st, "ds1", nil)
	two := newAccount(t, st, "ds2", nil)

	if _, err := st.ReplaceModels(one.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("刷新 ds1: %v", err)
	}
	if _, err := st.ReplaceModels(two.ID, []string{"m9"}); err != nil {
		t.Fatalf("刷新 ds2: %v", err)
	}

	added, err := st.ReplaceModels(one.ID, nil)
	if err != nil {
		t.Fatalf("清空 ds1: %v", err)
	}
	if added != 0 {
		t.Fatalf("清空时新增数 = %d，期望 0", added)
	}
	if got := modelStates(t, st, one.ID); len(got) != 0 {
		t.Fatalf("ds1 应被清空，剩下 %v", got)
	}
	if got := modelStates(t, st, two.ID); len(got) != 1 || got[0] != "m9/synced" {
		t.Fatalf("ds2 不应受影响，得到 %v", got)
	}

	all, err := st.AllAccountModels()
	if err != nil {
		t.Fatalf("读全部模型状态: %v", err)
	}
	if len(all) != 1 || all[0].AccountID != two.ID {
		t.Fatalf("全部模型状态 = %+v，期望只剩 ds2 的一行", all)
	}
}

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

func TestSetModelsUpdatesAccount(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", []string{"init"})

	if err := st.SetModels(acc.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("改写模型列表: %v", err)
	}
	got, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if len(got.Models) != 2 || got.Models[0] != "m1" || got.Models[1] != "m2" {
		t.Fatalf("账号模型列表 = %v，期望 [m1 m2]", got.Models)
	}

	if err := st.SetModels(acc.ID, nil); err != nil {
		t.Fatalf("清空模型列表: %v", err)
	}
	got, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("清空后再读账号: %v", err)
	}
	if len(got.Models) != 0 {
		t.Fatalf("清空后模型列表 = %v，期望空", got.Models)
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
