package store_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/secret"
	"lax/internal/store"
)

// accountColumns 返回某张表的列名，顺序按 PRAGMA table_info 给的主键/定义顺序。
func accountColumns(t *testing.T, st *store.Store, table string) []string {
	t.Helper()
	rows, err := st.DB().Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("读 %s 的列: %v", table, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("读 %s 的列名: %v", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读 %s 的列名: %v", table, err)
	}
	return out
}

// TestMigration0014MovesOrphanAddressToProvider 从 0013 升上来：
// 先给 provider 不在 providers 表里的账号补一行厂商（地址取账号的 base_url，默认协议取账号的
// protocol 且只认四条线协议），再把 accounts.base_url 与 accounts.protocol 两列删掉。
// 已有厂商的 default_protocol 不被账号上的历史协议覆盖。
func TestMigration0014MovesOrphanAddressToProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.db")
	db := openDatabaseAt(t, path, 13)

	stmts := []string{
		// 两个孤儿账号共用一个厂商名、地址不同：迁移取地址最小的那个，结果与行序无关。
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('self1', 'selfco', 'anthropic', 'https://self.example.com', X'010203', '["m1"]', 1, 100)`,
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('self0', 'selfco', '', 'https://a.example.com', X'010203', '["m2"]', 1, 100)`,
		// 账号协议是脏值：不能当成默认协议落库，留空回到固定顺序回退。
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('junk', 'junkco', 'not-a-protocol', 'https://junk.example.com', X'010203', '[]', 1, 100)`,
		// 厂商已经在表里：账号上那份历史协议不许覆盖 0007 种下的默认协议。
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('ds1', 'deepseek', 'anthropic', 'https://api.deepseek.com', X'010203', '["m1"]', 1, 100)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("往老库里写数据（%s）: %v", stmt, err)
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

	// 两列真的没了。
	cols := strings.Join(accountColumns(t, st, "accounts"), ",")
	for _, gone := range []string{"base_url", "protocol"} {
		for _, c := range strings.Split(cols, ",") {
			if c == gone {
				t.Fatalf("accounts 里还有 %q 列，列清单 = %s", gone, cols)
			}
		}
	}
	if !strings.Contains(cols, "provider") {
		t.Fatalf("accounts 少了 provider 列，列清单 = %s", cols)
	}

	// 孤儿账号补出了厂商行：地址取最小的一份，默认协议取按账号名排序的第一条线协议。
	self, err := st.GetProvider("selfco")
	if err != nil {
		t.Fatalf("孤儿账号没有补出厂商: %v", err)
	}
	if self.BaseURL != "https://a.example.com" || self.DefaultProtocol != "anthropic" ||
		self.Note != "由旧库搬迁从账号地址补建" {
		t.Fatalf("补出来的 selfco = %+v", self)
	}

	// 脏协议留空：这家一条路由都没有，默认协议本来就该是空串，而列里也不能是脏值。
	junk, err := st.GetProvider("junkco")
	if err != nil {
		t.Fatalf("孤儿账号 junkco 没有补出厂商: %v", err)
	}
	if junk.BaseURL != "https://junk.example.com" || junk.DefaultProtocol != "" {
		t.Fatalf("补出来的 junkco = %+v，期望地址照搬、默认协议留空", junk)
	}

	// 已经在表里的厂商一个字段都不动。
	ds, err := st.GetProvider("deepseek")
	if err != nil {
		t.Fatalf("读 deepseek: %v", err)
	}
	if ds.DefaultProtocol != "openai-chat" {
		t.Fatalf("deepseek 的默认协议被账号上的历史协议覆盖成 %q", ds.DefaultProtocol)
	}

	// 账号侧的地址与协议现在是派生值：地址来自刚补的厂商行，默认协议算不出来（这家没有路由）。
	acc, err := st.GetAccount("self1")
	if err != nil {
		t.Fatalf("读 self1: %v", err)
	}
	if acc.BaseURL != "https://a.example.com" || acc.Protocol != "" {
		t.Fatalf("self1 的派生值 = base_url %q、protocol %q，期望地址来自补建的厂商、协议为空串",
			acc.BaseURL, acc.Protocol)
	}
	if len(acc.Models) != 1 || acc.Models[0] != "m1" {
		t.Fatalf("self1 的模型清单被改动: %v", acc.Models)
	}
}

// TestAccountsDeriveAddressAndProtocol 钉住三个账号读入口的派生值：地址取 providers.base_url，
// 协议取该厂商的有效默认协议，两条边界是「厂商在但一条路由都没有」与「厂商不在表里」，两处都为空串。
func TestAccountsDeriveAddressAndProtocol(t *testing.T) {
	st := openTemp(t)

	if err := st.AddProvider(store.Provider{
		Name: "co", BaseURL: "https://api.example.com", DefaultProtocol: "anthropic",
	}, []store.ProviderRoute{
		{Provider: "co", Protocol: "openai-chat", Path: "/v1/chat/completions"},
		{Provider: "co", Protocol: "anthropic", Path: "/v1/messages"},
	}); err != nil {
		t.Fatalf("建厂商 co: %v", err)
	}
	// 一家有地址、一条路由都没有的厂商：默认协议算不出来。
	if err := st.AddProvider(store.Provider{Name: "noroutes", BaseURL: "https://nr.example.com"}, nil); err != nil {
		t.Fatalf("建厂商 noroutes: %v", err)
	}

	withCo := store.Account{Name: "a1", Provider: "co", BaseURL: "https://ignored.example.com", Protocol: "gemini", KeyCipher: []byte{1}, Enabled: true}
	withNoRoutes := store.Account{Name: "a2", Provider: "noroutes", KeyCipher: []byte{1}, Enabled: true}
	// 厂商不在表里：两样都算不出来。
	ghost := store.Account{Name: "a3", Provider: "ghost", BaseURL: "https://ghost.example.com", Protocol: "openai-chat", KeyCipher: []byte{1}, Enabled: true}
	for _, a := range []*store.Account{&withCo, &withNoRoutes, &ghost} {
		if err := st.CreateAccount(a); err != nil {
			t.Fatalf("建账号 %s: %v", a.Name, err)
		}
	}

	want := map[string]struct{ base, protocol string }{
		"a1": {"https://api.example.com", "anthropic"},
		"a2": {"https://nr.example.com", ""},
		"a3": {"", ""},
	}

	// 1. ListAccounts。
	list, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("列账号: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("账号数 = %d，期望 3", len(list))
	}
	for _, a := range list {
		w := want[a.Name]
		if a.BaseURL != w.base || a.Protocol != w.protocol {
			t.Fatalf("ListAccounts 里 %s 的派生值 = %q / %q，期望 %q / %q",
				a.Name, a.BaseURL, a.Protocol, w.base, w.protocol)
		}
	}

	// 2. GetAccount：写入时传进来的 BaseURL 与 Protocol 一个都不落库。
	for name, w := range want {
		got, err := st.GetAccount(name)
		if err != nil {
			t.Fatalf("取账号 %s: %v", name, err)
		}
		if got.BaseURL != w.base || got.Protocol != w.protocol {
			t.Fatalf("GetAccount(%s) 的派生值 = %q / %q，期望 %q / %q",
				name, got.BaseURL, got.Protocol, w.base, w.protocol)
		}
	}
	// 3. ListAccountsByProvider。
	mine, err := st.ListAccountsByProvider("co")
	if err != nil {
		t.Fatalf("按厂商列账号: %v", err)
	}
	if len(mine) != 1 || mine[0].Name != "a1" || mine[0].BaseURL != "https://api.example.com" || mine[0].Protocol != "anthropic" {
		t.Fatalf("ListAccountsByProvider(co) = %+v", mine)
	}
	if none, err := st.ListAccountsByProvider("nobody"); err != nil {
		t.Fatalf("列不存在的厂商: %v", err)
	} else if len(none) != 0 {
		t.Fatalf("不存在的厂商不该有账号，得到 %+v", none)
	}

	// 改厂商地址立刻影响所有引用它的账号：单一来源应有的行为。
	addr := "https://moved.example.com"
	if err := st.SetProvider("co", store.ProviderPatch{BaseURL: &addr}); err != nil {
		t.Fatalf("改厂商地址: %v", err)
	}
	if acc, err := st.GetAccount("a1"); err != nil {
		t.Fatalf("改地址后读 a1: %v", err)
	} else if acc.BaseURL != addr {
		t.Fatalf("改厂商地址后账号的 BaseURL = %q，期望 %q", acc.BaseURL, addr)
	}
}

// TestDeleteProviderForceDisablesAccounts 钉住 `provider rm --force` 的语义：
// 删厂商与路由，并在同一个事务里把指向它的账号停用、写明 last_error；其它厂商的账号不动。
// 不带 --force 的 DeleteProvider 只删厂商，账号状态原样保留（那是调用方先拒绝的事）。
func TestDeleteProviderForceDisablesAccounts(t *testing.T) {
	st := openTemp(t)

	for _, name := range []string{"co", "keep"} {
		if err := st.AddProvider(store.Provider{Name: name, BaseURL: "https://" + name + ".example.com"}, []store.ProviderRoute{
			{Provider: name, Protocol: "openai-chat", Path: "/v1/chat/completions"},
		}); err != nil {
			t.Fatalf("建厂商 %s: %v", name, err)
		}
	}
	gone := store.Account{Name: "a1", Provider: "co", KeyCipher: []byte{1}, Enabled: true}
	stay := store.Account{Name: "a2", Provider: "keep", KeyCipher: []byte{1}, Enabled: true}
	for _, a := range []*store.Account{&gone, &stay} {
		if err := st.CreateAccount(a); err != nil {
			t.Fatalf("建账号 %s: %v", a.Name, err)
		}
	}

	// 不带 --force：只删厂商，账号还启着。
	if err := st.DeleteProvider("keep"); err != nil {
		t.Fatalf("删厂商 keep: %v", err)
	}
	if acc, err := st.GetAccount("a2"); err != nil {
		t.Fatalf("读 a2: %v", err)
	} else if !acc.Enabled || acc.LastError != "" {
		t.Fatalf("不带 --force 删厂商动了账号状态: %+v", acc)
	}
	// 停用之后重新准备一份，用来验 --force。
	if err := st.SetEnabled(stay.ID, true, ""); err != nil {
		t.Fatalf("重置 a2: %v", err)
	}
	if err := st.AddProvider(store.Provider{Name: "keep", BaseURL: "https://keep.example.com"}, []store.ProviderRoute{
		{Provider: "keep", Protocol: "openai-chat", Path: "/v1/chat/completions"},
	}); err != nil {
		t.Fatalf("重建厂商 keep: %v", err)
	}

	if err := st.DeleteProviderForce("co"); err != nil {
		t.Fatalf("--force 删厂商: %v", err)
	}

	acc, err := st.GetAccount("a1")
	if err != nil {
		t.Fatalf("读 a1: %v", err)
	}
	if acc.Enabled {
		t.Fatal("--force 删厂商后指向它的账号应被停用")
	}
	if acc.LastError != "厂商已删除，请 provider add 重建或修改账号的厂商" {
		t.Fatalf("停用原因 = %q", acc.LastError)
	}
	// 账号本身与它的凭据都还在：重建厂商或改厂商之后能接着用。
	if acc.Provider != "co" || len(acc.KeyCipher) == 0 {
		t.Fatalf("账号被删改: %+v", acc)
	}
	// 派生值跟着没了：厂商不在了，地址与协议都算不出来。
	if acc.BaseURL != "" || acc.Protocol != "" {
		t.Fatalf("厂商删掉之后派生值 = %q / %q，期望都是空串", acc.BaseURL, acc.Protocol)
	}
	// 厂商与路由都删干净。
	if _, err := st.GetProvider("co"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("厂商 co 还在: %v", err)
	}
	if routes, err := st.ProviderRoutes("co"); err != nil {
		t.Fatalf("读 co 的路由: %v", err)
	} else if len(routes) != 0 {
		t.Fatalf("co 的路由没删干净: %+v", routes)
	}

	// 别的厂商的账号不受影响。
	if other, err := st.GetAccount("a2"); err != nil {
		t.Fatalf("读 a2: %v", err)
	} else if !other.Enabled || other.LastError != "" {
		t.Fatalf("别的厂商的账号被牵连: %+v", other)
	}

	if err := st.DeleteProviderForce("nobody"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("删不存在的厂商应报 ErrProviderNotFound，得到 %v", err)
	}
}

// TestCredentialStaysCiphertext 钉住凭据只以密文出现：库里、序列化输出里都找不到明文。
func TestCredentialStaysCiphertext(t *testing.T) {
	st := openTemp(t)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	const plaintext = "sk-live-0123456789abcdef"
	cipher, err := secret.Encrypt(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}

	acc := store.Account{Name: "a1", Provider: "deepseek", KeyCipher: cipher, Enabled: true}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}

	// 库里存的是密文，明文一个字节都不在里面。
	var blob []byte
	if err := st.DB().QueryRow(`SELECT key_cipher FROM accounts WHERE name = 'a1'`).Scan(&blob); err != nil {
		t.Fatalf("读密文: %v", err)
	}
	if strings.Contains(string(blob), plaintext) {
		t.Fatal("数据库里出现了凭据明文")
	}
	if got, err := secret.Decrypt(key, blob); err != nil {
		t.Fatalf("解开库里的密文: %v", err)
	} else if string(got) != plaintext {
		t.Fatalf("解密结果 = %q，期望原文", got)
	}

	// 读出来的账号序列化成 JSON 也只有密文。
	for _, a := range []store.Account{mustGet(t, st, "a1")} {
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("序列化账号: %v", err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("序列化输出里出现了凭据明文: %s", b)
		}
	}

	// 错误信息里也不带凭据。
	if err := st.CreateAccount(&store.Account{Name: "a1", Provider: "deepseek", KeyCipher: cipher}); err == nil {
		t.Fatal("重名应报错")
	} else if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("错误信息里出现了凭据明文: %v", err)
	}
}

func mustGet(t *testing.T, st *store.Store, name string) store.Account {
	t.Helper()
	acc, err := st.GetAccount(name)
	if err != nil {
		t.Fatalf("取账号 %s: %v", name, err)
	}
	return acc
}
