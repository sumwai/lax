package store_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"lax/internal/secret"
	"lax/internal/store"
)

// declareCatalogRow 读目录里的一行，读不到直接失败：声明之后目录里必须有一行。
func declareCatalogRow(t *testing.T, st *store.Store, provider, model string) store.ProviderModel {
	t.Helper()
	m, ok, err := st.ProviderModel(provider, model)
	if err != nil {
		t.Fatalf("读目录 %s/%s: %v", provider, model, err)
	}
	if !ok {
		t.Fatalf("目录里没有 %s/%s 这一行", provider, model)
	}
	return m
}

// declareModelRow 读一个账号在一个模型上的可达性行，读不到直接失败。
func declareModelRow(t *testing.T, st *store.Store, accountID int64, model string) store.AccountModel {
	t.Helper()
	rows, err := st.AccountModels(accountID)
	if err != nil {
		t.Fatalf("读账号 %d 的模型行: %v", accountID, err)
	}
	for _, row := range rows {
		if row.Model == model {
			return row
		}
	}
	t.Fatalf("账号 %d 没有模型 %q 的行", accountID, model)
	return store.AccountModel{}
}

// TestDeclareAccountModelWritesCatalogAndAccess 钉住「一次事务写两张表」：
// 目录补一行 source='manual' 并带上声明的协议，可达性标成 allowed 且没有探活结论。
func TestDeclareAccountModelWritesCatalogAndAccess(t *testing.T) {
	st := openTemp(t)
	acc := newCatalogAccount(t, st, "dec1", "acme")

	res, err := st.DeclareAccountModel(acc.ID, "  typesafe/jev  ", "systemone")
	if err != nil {
		t.Fatalf("声明模型: %v", err)
	}
	want := store.DeclareModelResult{
		Model:            "typesafe/jev",
		Provider:         "acme",
		Access:           store.AccessAllowed,
		Protocol:         "systemone",
		Created:          true,
		ProtocolDeclared: true,
	}
	if res != want {
		t.Fatalf("结果 = %+v，期望 %+v（模型名去空白、账号行新建、协议新进目录）", res, want)
	}

	dir := declareCatalogRow(t, st, "acme", "typesafe/jev")
	if dir.Source != store.SourceManual {
		t.Fatalf("目录行来源 = %q，期望 manual", dir.Source)
	}
	if !reflect.DeepEqual(dir.Protocols, []string{"systemone"}) {
		t.Fatalf("目录行协议 = %v，期望 [systemone]", dir.Protocols)
	}
	if dir.ContextWindow != nil || dir.MaxOutput != nil || dir.PriceIn != nil ||
		dir.PriceOut != nil || dir.PriceCacheRead != nil || dir.PriceCacheWrite != nil {
		t.Fatalf("手写声明不该编造元数据，实际 = %+v", dir)
	}
	if len(dir.Capabilities) != 0 {
		t.Fatalf("手写声明不该编造能力位，实际 = %v", dir.Capabilities)
	}
	if dir.UpdatedAt == 0 {
		t.Fatal("新写的目录行 updated_at 不该是 0")
	}

	row := declareModelRow(t, st, acc.ID, "typesafe/jev")
	if row.Access != store.AccessAllowed {
		t.Fatalf("可达性 = %q，期望 allowed", row.Access)
	}
	if row.Protocol != "systemone" {
		t.Fatalf("账号级协议 = %q，期望 systemone", row.Protocol)
	}
	if row.HTTPStatus != 0 || row.Error != "" || row.CheckedAt != 0 {
		t.Fatalf("手写不是探活结论，探活三列应为 0/空/0，实际 = %d/%q/%d",
			row.HTTPStatus, row.Error, row.CheckedAt)
	}
	if bodies, err := st.AllowedModels(acc.ID); err != nil {
		t.Fatalf("读可用模型: %v", err)
	} else if !reflect.DeepEqual(bodies, []string{"typesafe/jev"}) {
		t.Fatalf("可用模型 = %v，期望 [typesafe/jev]", bodies)
	}
}

// TestDeclareAccountModelIsIdempotent 钉住「可重复执行」：同一账号、模型、协议再写一次，
// 不新增行、不清掉已经写下的探活三列、协议集合不重复。
func TestDeclareAccountModelIsIdempotent(t *testing.T) {
	st := openTemp(t)
	acc := newCatalogAccount(t, st, "dec2", "acme")

	if _, err := st.DeclareAccountModel(acc.ID, "jev", "openai-chat"); err != nil {
		t.Fatalf("第一次声明: %v", err)
	}
	// 两次声明之间跑过一轮探活：这条结论不该被第二次手写冲掉。
	if err := st.SetAccountModelAccess(acc.ID, "jev", store.AccessAllowed, 200, "上游说可用", 1730000000); err != nil {
		t.Fatalf("写探活结论: %v", err)
	}

	res, err := st.DeclareAccountModel(acc.ID, "jev", "openai-chat")
	if err != nil {
		t.Fatalf("第二次声明: %v", err)
	}
	if res.Created {
		t.Fatal("第二次声明不该新建账号行")
	}
	if res.ProtocolDeclared {
		t.Fatal("协议已经在目录集合里，ProtocolDeclared 应为假")
	}

	row := declareModelRow(t, st, acc.ID, "jev")
	if row.Access != store.AccessAllowed {
		t.Fatalf("可达性 = %q，期望 allowed", row.Access)
	}
	if row.HTTPStatus != 200 || row.Error != "上游说可用" || row.CheckedAt != 1730000000 {
		t.Fatalf("已有的 allowed 行要保留探活三列，实际 = %d/%q/%d",
			row.HTTPStatus, row.Error, row.CheckedAt)
	}
	if row.Protocol != "openai-chat" {
		t.Fatalf("账号级协议 = %q，期望 openai-chat", row.Protocol)
	}

	dir := declareCatalogRow(t, st, "acme", "jev")
	if got := strings.Join(dir.Protocols, ","); got != "openai-chat" {
		t.Fatalf("协议集合 = %q，重复声明不该多出一项", got)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读账号行: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("账号行数 = %d，期望 1", len(rows))
	}
}

// TestDeclareAccountModelClearsStaleVerdict 钉住「旧结论作废」：从 forbidden / missing / error
// 翻回 allowed 时把 http_status、error 与 checked_at 清成 0 / 空串 / 0，下一轮照常重探。
func TestDeclareAccountModelClearsStaleVerdict(t *testing.T) {
	for _, stale := range []string{store.AccessForbidden, store.AccessMissing, store.AccessError} {
		t.Run(stale, func(t *testing.T) {
			st := openTemp(t)
			acc := newCatalogAccount(t, st, "dec3-"+stale, "acme")
			if err := st.SetAccountModelAccess(acc.ID, "m1", stale, 403, "旧结论", 999); err != nil {
				t.Fatalf("写旧结论: %v", err)
			}

			res, err := st.DeclareAccountModel(acc.ID, "m1", "")
			if err != nil {
				t.Fatalf("声明模型: %v", err)
			}
			if res.Created {
				t.Fatal("这一行是探活早就写下的，不该算新建")
			}
			if res.Protocol != "" {
				t.Fatalf("没给协议时 Protocol 应为空串，实际 = %q", res.Protocol)
			}
			if res.ProtocolDeclared {
				t.Fatal("没给协议时不该声明任何协议")
			}

			row := declareModelRow(t, st, acc.ID, "m1")
			if row.Access != store.AccessAllowed {
				t.Fatalf("可达性 = %q，期望 allowed", row.Access)
			}
			if row.HTTPStatus != 0 || row.Error != "" || row.CheckedAt != 0 {
				t.Fatalf("从 %s 翻回 allowed 要清掉探活三列，实际 = %d/%q/%d",
					stale, row.HTTPStatus, row.Error, row.CheckedAt)
			}
			if row.Protocol != "" {
				t.Fatalf("没给协议时账号级协议应保持原值（空串），实际 = %q", row.Protocol)
			}

			// 目录行的协议集合没给协议就保持空串，表示上游没声明。
			if dir := declareCatalogRow(t, st, "acme", "m1"); len(dir.Protocols) != 0 {
				t.Fatalf("目录行协议 = %v，期望空集合", dir.Protocols)
			}
		})
	}
}

// TestDeclareAccountModelMergesProtocols 钉住「协议并进集合、元数据不动」：
// 已有的同步行保留顺序与全部元数据，新协议追加到末尾，来源升到 manual 之后同步冲不掉它。
func TestDeclareAccountModelMergesProtocols(t *testing.T) {
	st := openTemp(t)
	acc := newCatalogAccount(t, st, "dec4", "acme")
	window, maxOut := int64(200000), int64(8000)
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
		Model:         "m1",
		Source:        store.SourceSync,
		ContextWindow: &window,
		MaxOutput:     &maxOut,
		Protocols:     []string{"anthropic", "openai-chat"},
		Capabilities:  []string{"chat", "tools"},
		PriceIn:       floatPtr(1.5),
		PriceOut:      floatPtr(6),
	}}); err != nil {
		t.Fatalf("写同步行: %v", err)
	}

	res, err := st.DeclareAccountModel(acc.ID, "m1", "gemini")
	if err != nil {
		t.Fatalf("声明模型: %v", err)
	}
	if !res.Created || !res.ProtocolDeclared {
		t.Fatalf("结果 = %+v，期望新建账号行且 gemini 是新声明的协议", res)
	}

	dir := declareCatalogRow(t, st, "acme", "m1")
	want := []string{"anthropic", "openai-chat", "gemini"}
	if !reflect.DeepEqual(dir.Protocols, want) {
		t.Fatalf("协议集合 = %v，期望 %v（旧顺序保留、新协议在末尾）", dir.Protocols, want)
	}
	if dir.Source != store.SourceManual {
		t.Fatalf("来源 = %q，期望升到 manual", dir.Source)
	}
	if dir.ContextWindow == nil || *dir.ContextWindow != window {
		t.Fatalf("上下文长度 = %v，声明不该抹掉它", dir.ContextWindow)
	}
	if dir.MaxOutput == nil || *dir.MaxOutput != maxOut {
		t.Fatalf("最大输出 = %v，声明不该抹掉它", dir.MaxOutput)
	}
	if dir.PriceIn == nil || *dir.PriceIn != 1.5 || dir.PriceOut == nil || *dir.PriceOut != 6 {
		t.Fatalf("价格 = %v/%v，声明不该抹掉它", dir.PriceIn, dir.PriceOut)
	}
	if !reflect.DeepEqual(dir.Capabilities, []string{"chat", "tools"}) {
		t.Fatalf("能力位 = %v，声明不该抹掉它", dir.Capabilities)
	}

	// 再声明一个已经在集合里的协议：集合原样不动，ProtocolDeclared 为假。
	again, err := st.DeclareAccountModel(acc.ID, "m1", "anthropic")
	if err != nil {
		t.Fatalf("重复声明已有协议: %v", err)
	}
	if again.ProtocolDeclared {
		t.Fatal("anthropic 已经在集合里，ProtocolDeclared 应为假")
	}
	if dir = declareCatalogRow(t, st, "acme", "m1"); !reflect.DeepEqual(dir.Protocols, want) {
		t.Fatalf("协议集合 = %v，期望仍是 %v", dir.Protocols, want)
	}

	// 来源升到 manual 之后，同步不再改写这一行：手写的协议不会被上游清单冲掉。
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{{
		Model:     "m1",
		Source:    store.SourceSync,
		Protocols: []string{"openai-chat"},
	}}); err != nil {
		t.Fatalf("再写同步行: %v", err)
	}
	if dir = declareCatalogRow(t, st, "acme", "m1"); !reflect.DeepEqual(dir.Protocols, want) {
		t.Fatalf("同步之后协议集合 = %v，期望仍是 %v", dir.Protocols, want)
	}
}

// TestDeclareAccountModelRejectsBadInput 挡住四种会造出脏行或写错厂商的写法：
// 空模型名、非法协议、不存在的账号、厂商为空的账号。报错之后两张表都不该多出行。
func TestDeclareAccountModelRejectsBadInput(t *testing.T) {
	st := openTemp(t)
	acc := newCatalogAccount(t, st, "dec5", "acme")

	cases := []struct {
		name      string
		accountID int64
		model     string
		protocol  string
		want      string
	}{
		{"空模型名", acc.ID, "", "systemone", "主键"},
		{"只有空白的模型名", acc.ID, "   ", "systemone", "主键"},
		{"非法协议", acc.ID, "m1", "openai", "只能是空串"},
		{"不存在的账号", 999, "m1", "systemone", "账号 id 999 不存在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := st.DeclareAccountModel(c.accountID, c.model, c.protocol)
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误 = %q，期望含 %q", err, c.want)
			}
			if c.accountID == 999 && !strings.Contains(err.Error(), "lax account add") {
				t.Fatalf("账号不存在时要指出下一步 lax account add，实际 = %q", err)
			}
			if rows, err := st.AllAccountModels(); err != nil {
				t.Fatalf("读可达性行: %v", err)
			} else if len(rows) != 0 {
				t.Fatalf("报错之后可达性表多了 %d 行", len(rows))
			}
			if rows, err := st.ProviderModels("acme"); err != nil {
				t.Fatalf("读目录: %v", err)
			} else if len(rows) != 0 {
				t.Fatalf("报错之后目录多了 %d 行", len(rows))
			}
		})
	}

	// 厂商为空的账号定位不到目录行：报错，两张表都不落库。
	noProvider := newCatalogAccount(t, st, "dec5-empty", "")
	if _, err := st.DeclareAccountModel(noProvider.ID, "m1", ""); err == nil {
		t.Fatal("厂商为空时应报错")
	} else if !strings.Contains(err.Error(), "厂商为空") {
		t.Fatalf("错误 = %q，期望说明厂商为空", err)
	}
	if rows, err := st.AccountModels(noProvider.ID); err != nil {
		t.Fatalf("读可达性行: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("厂商为空时不该落可达性行，实际 %d 行", len(rows))
	}
}

// TestDeclareAccountModelCarriesNoCredential 钉住凭据只以密文出现：日志之外能被打印出去的三份
// 东西——声明结果、错误文案、库文件字节——都找不到 key 明文。store 本身不写日志，
// 这条断言防的是以后往结果结构或错误文案里顺手塞凭据。
func TestDeclareAccountModelCarriesNoCredential(t *testing.T) {
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
	const plaintext = "sk-declare-abcdef0123456789"
	cipher, err := secret.Encrypt(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}
	acc := store.Account{Name: "dec6", Provider: "acme", KeyCipher: cipher, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}

	res, err := st.DeclareAccountModel(acc.ID, "jev", "systemone")
	if err != nil {
		t.Fatalf("声明模型: %v", err)
	}
	got, err := st.GetAccount("dec6")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	infos, err := st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("读有效模型行: %v", err)
	}
	for name, v := range map[string]any{"声明结果": res, "账号": got, "有效模型": infos} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化%s: %v", name, err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("%s的序列化输出里出现了 key 明文: %s", name, b)
		}
	}

	// 报错路径同样不带凭据：错误文案里只有账号 id。
	if _, err := st.DeclareAccountModel(999, "jev", "systemone"); err == nil {
		t.Fatal("不存在的账号应报错")
	} else if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("错误文案里出现了 key 明文: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读库文件: %v", err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatal("库文件里出现了 key 明文")
	}
}
