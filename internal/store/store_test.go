package store_test

import (
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

func TestAgentState(t *testing.T) {
	st := openTemp(t)

	st1 := store.AgentState{Name: "claude-code", ConfigPath: "/tmp/settings.json", BackupPath: "/tmp/settings.json.bak", Model: "m1", AppliedAt: 1}
	if err := st.PutAgent(st1); err != nil {
		t.Fatalf("写入: %v", err)
	}
	st1.Model = "m2"
	if err := st.PutAgent(st1); err != nil {
		t.Fatalf("覆盖写入: %v", err)
	}

	got, err := st.GetAgent("claude-code")
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if got.Model != "m2" {
		t.Fatalf("model = %q", got.Model)
	}

	list, err := st.ListAgents()
	if err != nil || len(list) != 1 {
		t.Fatalf("列表 = %+v，错误 %v", list, err)
	}

	if err := st.DeleteAgent("claude-code"); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if _, err := st.GetAgent("claude-code"); err == nil {
		t.Fatal("删除后应取不到")
	}
}
