package store_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"lax/internal/secret"
	"lax/internal/store"
)

// newCatalogAccount 建一个挂在给定厂商下的账号，模型清单留空：
// 目录与可达性分开之后，测试要自己决定哪些名字进目录、哪些对账号可达。
func newCatalogAccount(t *testing.T, st *store.Store, name, provider string) store.Account {
	t.Helper()
	acc := store.Account{
		Name:      name,
		Provider:  provider,
		KeyCipher: []byte{1, 2, 3},
		Enabled:   true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号 %s: %v", name, err)
	}
	return acc
}

func mustProvider(t *testing.T, st *store.Store, name, baseURL string, routes ...store.ProviderRoute) {
	t.Helper()
	if err := st.AddProvider(store.Provider{Name: name, BaseURL: baseURL}, routes); err != nil {
		t.Fatalf("建厂商 %s: %v", name, err)
	}
}

// TestUpsertProviderModelsSourceRank 钉住「来源不降级」：manual 最强，sync 次之，probe 最弱。
// 弱来源整行都写不进去（不只是 source 那一列），强来源可以补上自己带的字段。
func TestUpsertProviderModelsSourceRank(t *testing.T) {
	st := openTemp(t)

	window := int64(200000)
	out := int64(8000)
	if err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{
		Model:         "m1",
		Source:        store.SourceProbe,
		ContextWindow: &window,
		Protocols:     []string{"anthropic"},
	}}); err != nil {
		t.Fatalf("写探活行: %v", err)
	}

	// sync 可以覆盖 probe：上游清单比「探活顺手补的名字」更权威。
	if err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{
		Model:     "m1",
		Source:    store.SourceSync,
		MaxOutput: &out,
		PriceIn:   floatPtr(2),
	}}); err != nil {
		t.Fatalf("写同步行: %v", err)
	}
	got := oneModel(t, st, "deepseek", "m1")
	if got.Source != store.SourceSync {
		t.Fatalf("来源 = %q，期望 sync（同步压过探活）", got.Source)
	}
	if got.MaxOutput == nil || *got.MaxOutput != out || got.PriceIn == nil || *got.PriceIn != 2 {
		t.Fatalf("同步行带的字段没写进去: %+v", got)
	}
	if got.ContextWindow == nil || *got.ContextWindow != window {
		t.Fatalf("同步行没给的长度应保留原值，实际 %+v", got.ContextWindow)
	}

	// manual 覆盖 sync 并把来源钉住。
	if err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{
		Model:  "m1",
		Source: store.SourceManual,
	}}); err != nil {
		t.Fatalf("写手填行: %v", err)
	}
	got = oneModel(t, st, "deepseek", "m1")
	if got.Source != store.SourceManual {
		t.Fatalf("来源 = %q，期望 manual", got.Source)
	}
	if got.ContextWindow == nil || *got.ContextWindow != window {
		t.Fatalf("手填行不该抹掉已有元数据: %+v", got)
	}

	// 之后来的 sync 与 probe 一行字段都写不进去，连自己带的长度也不行。
	other := int64(1000)
	for _, src := range []string{store.SourceSync, store.SourceProbe} {
		if err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{
			Model:         "m1",
			Source:        src,
			ContextWindow: &other,
			MaxOutput:     nil,
		}}); err != nil {
			t.Fatalf("写 %s 行: %v", src, err)
		}
		got = oneModel(t, st, "deepseek", "m1")
		if got.Source != store.SourceManual {
			t.Fatalf("%s 把来源降级成了 %q", src, got.Source)
		}
		if got.ContextWindow == nil || *got.ContextWindow != window {
			t.Fatalf("%s 改动了 manual 行的字段: %+v", src, got)
		}
		if got.MaxOutput == nil || *got.MaxOutput != out {
			t.Fatalf("%s 抹掉了 manual 行的最大输出: %+v", src, got)
		}
	}
}

// TestUpsertProviderModelsValidatesInput 挡住三种会造出脏行的写法：空厂商、空模型名、非法来源。
func TestUpsertProviderModelsValidatesInput(t *testing.T) {
	st := openTemp(t)

	if err := st.UpsertProviderModels("  ", []store.ProviderModel{{Model: "m1", Source: store.SourceSync}}); err == nil {
		t.Fatal("空厂商名应报错")
	}
	if err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{Model: " ", Source: store.SourceSync}}); err == nil {
		t.Fatal("空模型名应报错")
	}
	err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{Model: "m1", Source: "guess"}})
	if err == nil || !strings.Contains(err.Error(), "sync") {
		t.Fatalf("非法来源应报错并列出可用来源，实际 %v", err)
	}
	if models, err := st.ProviderModels("deepseek"); err != nil || len(models) != 0 {
		t.Fatalf("校验失败不该落库: %+v, %v", models, err)
	}
}

// TestUpsertProviderModelsKeepsUpdatedAt 钉住迁移回填的行 updated_at 是 0、写过的行取当前时间。
func TestUpsertProviderModelsKeepsUpdatedAt(t *testing.T) {
	st := openTemp(t)
	if err := st.UpsertProviderModels("deepseek", []store.ProviderModel{{Model: "m1", Source: store.SourceSync}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if got := oneModel(t, st, "deepseek", "m1"); got.UpdatedAt <= 0 {
		t.Fatalf("写过的行 updated_at = %d，应取当前时间", got.UpdatedAt)
	}
}

// TestEffectiveModelsOverrideWinsOverCatalog 钉住取值优先级：账号覆盖 → 目录 → 厂商默认协议。
func TestEffectiveModelsOverrideWinsOverCatalog(t *testing.T) {
	st := openTemp(t)
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	acc := newCatalogAccount(t, st, "a1", "acme")

	window, out := int64(200000), int64(8000)
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
		Model:         "m1",
		Source:        store.SourceSync,
		ContextWindow: &window,
		MaxOutput:     &out,
		PriceIn:       floatPtr(1),
		PriceOut:      floatPtr(3),
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if _, err := st.ReplaceModels(acc.ID, []string{"m1"}); err != nil {
		t.Fatalf("标可达: %v", err)
	}

	// 只有目录时全部取目录的值。
	info := oneInfo(t, st, acc.ID, "m1")
	if info.Access != store.AccessAllowed {
		t.Fatalf("可达性 = %q，期望 allowed", info.Access)
	}
	if info.ContextWindow == nil || *info.ContextWindow != window || info.PriceIn == nil || *info.PriceIn != 1 {
		t.Fatalf("目录的值没生效: %+v", info)
	}
	// 目录没写价格的那几项留空，不自己造 0。
	if info.PriceCacheRead != nil || info.PriceCacheWrite != nil {
		t.Fatalf("目录没给的价格应是 nil: %+v", info)
	}
	// 目录没写协议、账号也没有覆盖时回退到厂商默认协议。
	if info.Protocol != "openai-chat" {
		t.Fatalf("协议 = %q，期望厂商默认协议 openai-chat", info.Protocol)
	}

	// 账号覆盖压过目录，没覆盖的那几项仍取目录。
	if err := st.SetAccountModelOverride(acc.ID, "m1", store.AccountModelOverride{
		ContextWindow: intPtr(64_000),
		PriceIn:       floatPtr(0.5),
	}); err != nil {
		t.Fatalf("写覆盖: %v", err)
	}
	info = oneInfo(t, st, acc.ID, "m1")
	if info.ContextWindow == nil || *info.ContextWindow != 64_000 {
		t.Fatalf("上下文长度 = %v，期望覆盖值 64000", info.ContextWindow)
	}
	if info.PriceIn == nil || *info.PriceIn != 0.5 {
		t.Fatalf("输入价 = %v，期望覆盖值 0.5", info.PriceIn)
	}
	if info.MaxOutput == nil || *info.MaxOutput != out {
		t.Fatalf("没覆盖的最大输出应回退到目录: %+v", info.MaxOutput)
	}
	if info.PriceOut == nil || *info.PriceOut != 3 {
		t.Fatalf("没覆盖的输出价应回退到目录: %+v", info.PriceOut)
	}

	// 覆盖值整体写：nil 就是 NULL（继承目录），不是「不动」。
	if err := st.SetAccountModelOverride(acc.ID, "m1", store.AccountModelOverride{MaxOutput: intPtr(4096)}); err != nil {
		t.Fatalf("重写覆盖: %v", err)
	}
	info = oneInfo(t, st, acc.ID, "m1")
	if info.ContextWindow == nil || *info.ContextWindow != window {
		t.Fatalf("清掉覆盖之后应回到目录的 200000，实际 %v", info.ContextWindow)
	}
	if info.PriceIn == nil || *info.PriceIn != 1 {
		t.Fatalf("清掉覆盖之后输入价应回到目录的 1，实际 %v", info.PriceIn)
	}
	if info.MaxOutput == nil || *info.MaxOutput != 4096 {
		t.Fatalf("最大输出 = %v，期望覆盖值 4096", info.MaxOutput)
	}
}

// TestEffectiveModelsProtocolOrder 钉住目录给出的协议集合怎么挑一个：
// 厂商默认协议在集合里就用它，否则按固定顺序 chat → responses → anthropic → gemini。
func TestEffectiveModelsProtocolOrder(t *testing.T) {
	st := openTemp(t)
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})

	cases := []struct {
		name      string
		protocols []string
		want      string
	}{
		{"默认协议在集合里", []string{"gemini", "openai-chat"}, "openai-chat"},
		{"默认协议不在集合里", []string{"gemini", "anthropic"}, "anthropic"},
		{"空集合回退默认协议", nil, "openai-chat"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			acc := newCatalogAccount(t, st, "a"+string(rune('1'+i)), "acme")
			model := "m" + string(rune('1'+i))
			if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
				Model: model, Source: store.SourceSync, Protocols: c.protocols,
			}}); err != nil {
				t.Fatalf("写目录: %v", err)
			}
			if _, err := st.ReplaceModels(acc.ID, []string{model}); err != nil {
				t.Fatalf("标可达: %v", err)
			}
			if got := oneInfo(t, st, acc.ID, model); got.Protocol != c.want {
				t.Fatalf("协议 = %q，期望 %q", got.Protocol, c.want)
			}
			// 账号级覆盖最优先，压过目录与厂商默认。
			if err := st.SetAccountModelProtocol(acc.ID, model, "gemini"); err != nil {
				t.Fatalf("记协议: %v", err)
			}
			if got := oneInfo(t, st, acc.ID, model); got.Protocol != "gemini" {
				t.Fatalf("覆盖之后协议 = %q，期望 gemini", got.Protocol)
			}
			if got, err := st.EffectiveProtocol(acc.ID, model); err != nil || got != "gemini" {
				t.Fatalf("EffectiveProtocol = %q, %v，期望与 EffectiveModels 一致", got, err)
			}
		})
	}
}

// TestSetAccountModelAccessValidates 挡住非法可达性；合法值一一落库。
func TestSetAccountModelAccessValidates(t *testing.T) {
	st := openTemp(t)
	acc := newCatalogAccount(t, st, "a1", "acme")

	if err := st.SetAccountModelAccess(acc.ID, "m1", "usable", 200, "", 1); err == nil {
		t.Fatal("旧词表里的 usable 不是 access 的取值，应报错")
	}
	// 失败不该留下半行：读回来是空的。
	if rows, err := st.AccountModels(acc.ID); err != nil || len(rows) != 0 {
		t.Fatalf("非法写入之后不该有行: %+v, %v", rows, err)
	}

	for _, access := range []string{store.AccessAllowed, store.AccessForbidden, store.AccessMissing, store.AccessError} {
		if err := st.SetAccountModelAccess(acc.ID, "m1", access, 400, "备注", 7); err != nil {
			t.Fatalf("写 %s: %v", access, err)
		}
		if got := oneRow(t, st, acc.ID, "m1"); got.Access != access || got.HTTPStatus != 400 || got.Error != "备注" || got.CheckedAt != 7 {
			t.Fatalf("写进去的 %s 没落到库里: %+v", access, got)
		}
	}

	// 协议只能是空串或四条线协议。
	if err := st.SetAccountModelProtocol(acc.ID, "m1", "openai"); err == nil {
		t.Fatal("非线协议应报错")
	}
	if err := st.SetAccountModelProtocol(acc.ID, "m1", "anthropic"); err != nil {
		t.Fatalf("写协议: %v", err)
	}
	// 记协议不动可达性与探活结论。
	if got := oneRow(t, st, acc.ID, "m1"); got.Access != store.AccessError || got.CheckedAt != 7 {
		t.Fatalf("记协议改动了可达性: %+v", got)
	}
}

// TestAccountModelsArePerAccount 钉住同厂商不同账号的可达性互不影响：
// 目录是并集，能不能用由各自的 access 表达。
func TestAccountModelsArePerAccount(t *testing.T) {
	st := openTemp(t)
	one := newCatalogAccount(t, st, "a1", "acme")
	two := newCatalogAccount(t, st, "a2", "acme")

	if _, err := st.ReplaceModels(one.ID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("标 a1: %v", err)
	}
	if _, err := st.ReplaceModels(two.ID, []string{"m1"}); err != nil {
		t.Fatalf("标 a2: %v", err)
	}
	if err := st.SetAccountModelAccess(two.ID, "m1", store.AccessForbidden, 403, "无权", 5); err != nil {
		t.Fatalf("记 a2 无权: %v", err)
	}

	if got, err := st.AllowedModels(one.ID); err != nil || strings.Join(got, ",") != "m1,m2" {
		t.Fatalf("a1 的可达模型 = %v, %v，期望 m1,m2", got, err)
	}
	if got, err := st.AllowedModels(two.ID); err != nil || len(got) != 0 {
		t.Fatalf("a2 的可达模型 = %v, %v，期望空", got, err)
	}
	// 目录只有一份：两个账号的名字落在同一批行上。
	models, err := st.ProviderModels("acme")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(models) != 2 || models[0].Model != "m1" || models[1].Model != "m2" {
		t.Fatalf("目录 = %+v，期望 m1、m2 各一行", models)
	}
}

func oneModel(t *testing.T, st *store.Store, provider, model string) store.ProviderModel {
	t.Helper()
	models, err := st.ProviderModels(provider)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	for _, m := range models {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("目录 %s 里没有模型 %s，实际 %+v", provider, model, models)
	return store.ProviderModel{}
}

func oneRow(t *testing.T, st *store.Store, accountID int64, model string) store.AccountModel {
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
	t.Fatalf("账号 %d 没有模型 %s 的行，实际 %+v", accountID, model, rows)
	return store.AccountModel{}
}

func oneInfo(t *testing.T, st *store.Store, accountID int64, model string) store.ModelInfo {
	t.Helper()
	infos, err := st.EffectiveModels(accountID)
	if err != nil {
		t.Fatalf("读有效模型: %v", err)
	}
	for _, i := range infos {
		if i.Model == model {
			return i
		}
	}
	t.Fatalf("账号 %d 的有效模型里没有 %s，实际 %+v", accountID, model, infos)
	return store.ModelInfo{}
}

// TestCatalogOutputsCarryNoCredential 钉住新加的读入口都不带凭据：目录、可达性、有效模型与账号
// 序列化之后都找不到 key 明文，库文件里也没有。模型目录里只有名字与数字，这条断言防的是以后
// 往这些结构里加字段时顺手把 key 搬进来。
func TestCatalogOutputsCarryNoCredential(t *testing.T) {
	path := t.TempDir() + "/sb.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer st.Close()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	const plaintext = "sk-live-abcdef0123456789"
	cipher, err := secret.Encrypt(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	acc := store.Account{Name: "a1", Provider: "acme", KeyCipher: cipher, Models: []string{"m1"}, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
		Model: "m1", Source: store.SourceSync, ContextWindow: intPtr(1000), PriceIn: floatPtr(1),
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if err := st.SetAccountModelOverride(acc.ID, "m1", store.AccountModelOverride{MaxOutput: intPtr(10)}); err != nil {
		t.Fatalf("写覆盖: %v", err)
	}

	readBack := mustGet(t, st, "a1")
	models, err := st.ProviderModels("acme")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达性: %v", err)
	}
	all, err := st.AllAccountModels()
	if err != nil {
		t.Fatalf("读全部可达性: %v", err)
	}
	infos, err := st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("读有效模型: %v", err)
	}
	allowed, err := st.AllowedModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达模型: %v", err)
	}
	for name, v := range map[string]any{
		"账号":    readBack,
		"目录":    models,
		"可达性":   rows,
		"全部可达性": all,
		"有效模型":  infos,
		"可达模型":  allowed,
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化%s: %v", name, err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("%s的序列化输出里出现了 key 明文: %s", name, b)
		}
	}

	// 库文件（含 WAL）里也不该有明文：凭据只以密文落库。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("读库文件 %s%s: %v", path, suffix, err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("库文件 %s%s 里有 key 明文", path, suffix)
		}
	}
}
