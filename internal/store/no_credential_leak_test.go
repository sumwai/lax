package store_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"switchboard/internal/secret"
	"switchboard/internal/store"
)

// TestUsageAndAccountOutputsCarryNoCredential 钉住凭据只以密文出现：账号行、用量汇总行
// 序列化之后与库文件里都找不到 key 明文。价格已经不在账号上（迁移 0016），这条断言防的是
// 以后往这些结构里加字段时顺手把 key 搬进来。
func TestUsageAndAccountOutputsCarryNoCredential(t *testing.T) {
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
	const plaintext = "sk-usage-abcdef0123456789"
	cipher, err := secret.Encrypt(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("加密凭据: %v", err)
	}
	if err := st.CreateAccount(&store.Account{
		Name: "a1", Provider: "acme", KeyCipher: cipher, Models: []string{"m1"}, Enabled: true,
	}); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{In: floatPtr(2), Out: floatPtr(4)}); err != nil {
		t.Fatalf("写覆盖价: %v", err)
	}
	if err := st.InsertRequest(&store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: 1,
		HTTPStatus: 200, OK: true, InTokens: 100, InTokensUncached: 100, OutTokens: 50,
	}); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}

	acc, err := st.GetAccount("a1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	rows, err := st.Usage("account", 0)
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	for name, v := range map[string]any{"账号": acc, "用量": rows} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化%s: %v", name, err)
		}
		if strings.Contains(string(b), plaintext) {
			t.Fatalf("%s的序列化输出里出现了 key 明文: %s", name, b)
		}
	}
	if rows[0].Cost == nil {
		t.Fatalf("这一组有价，花费不该是 nil")
	}

	// 库里也只有密文：整份文件字节里找不到明文。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读库文件: %v", err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatalf("库文件里出现了 key 明文")
	}
}
