package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"lax/internal/store"

	_ "github.com/glebarez/go-sqlite"
)

// TestProviderAuthColumns 钉住凭据头声明的写入、读取、清除与旧库补列。
// 两列只走 AutoMigrate：老库没有它们时打开就补上，缺省值是空串（按线协议推导）。
func TestProviderAuthColumns(t *testing.T) {
	t.Run("写入与读取", func(t *testing.T) {
		st := openTemp(t)
		if err := st.CreateProvider(store.Provider{
			Name: "doubao-voice", BaseURL: "https://openspeech.bytedance.com",
			AuthHeader: "X-Api-Key", AuthScheme: "raw",
		}, []store.ProviderRoute{{Protocol: "openai-speech", Path: "/api/v3/tts/unidirectional"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		p, err := st.GetProvider("doubao-voice")
		if err != nil {
			t.Fatalf("取厂商: %v", err)
		}
		// 头名落库归一成小写，读回来也是小写。
		if p.AuthHeader != "x-api-key" || p.AuthScheme != "raw" {
			t.Fatalf("凭据头 = %q/%q，期望 x-api-key/raw", p.AuthHeader, p.AuthScheme)
		}
	})

	t.Run("只给头时写法归一成 raw", func(t *testing.T) {
		st := openTemp(t)
		if err := st.CreateProvider(store.Provider{
			Name: "x", BaseURL: "https://api.x.test", AuthHeader: "X-Api-Key",
		}, []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		p, err := st.GetProvider("x")
		if err != nil {
			t.Fatalf("取厂商: %v", err)
		}
		if p.AuthScheme != "raw" {
			t.Fatalf("写法 = %q，期望缺省成 raw", p.AuthScheme)
		}
	})

	t.Run("改法与清掉", func(t *testing.T) {
		st := openTemp(t)
		if err := st.CreateProvider(store.Provider{
			Name: "x", BaseURL: "https://api.x.test", AuthHeader: "X-Api-Key", AuthScheme: "raw",
		}, []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		scheme := "bearer"
		if err := st.SetProvider("x", store.ProviderPatch{AuthScheme: &scheme}); err != nil {
			t.Fatalf("改写法: %v", err)
		}
		p, err := st.GetProvider("x")
		if err != nil {
			t.Fatalf("取厂商: %v", err)
		}
		if p.AuthHeader != "x-api-key" || p.AuthScheme != "bearer" {
			t.Fatalf("凭据头 = %q/%q，期望 x-api-key/bearer", p.AuthHeader, p.AuthScheme)
		}

		// 清头：写法一并清空，只留一个头名没有写法的行写不出任何凭据。
		empty := ""
		if err := st.SetProvider("x", store.ProviderPatch{AuthHeader: &empty}); err != nil {
			t.Fatalf("清凭据头: %v", err)
		}
		p, err = st.GetProvider("x")
		if err != nil {
			t.Fatalf("取厂商: %v", err)
		}
		if p.AuthHeader != "" || p.AuthScheme != "" {
			t.Fatalf("凭据头 = %q/%q，期望两项都是空串", p.AuthHeader, p.AuthScheme)
		}
	})

	t.Run("只有写法没有头是错误", func(t *testing.T) {
		st := openTemp(t)
		if err := st.CreateProvider(store.Provider{
			Name: "x", BaseURL: "https://api.x.test",
		}, []store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		scheme := "bearer"
		err := st.SetProvider("x", store.ProviderPatch{AuthScheme: &scheme})
		if err == nil || !strings.Contains(err.Error(), "auth.header") {
			t.Fatalf("应报「没有头就没有地方写凭据」，实际 %v", err)
		}
	})

	t.Run("旧库补列", func(t *testing.T) {
		path := t.TempDir() + "/old.db"
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("开老库: %v", err)
		}
		// 老版本的 providers 表：没有 auth_header 与 auth_scheme 两列。
		for _, stmt := range []string{
			`CREATE TABLE providers (name TEXT PRIMARY KEY, base_url TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
				default_protocol TEXT NOT NULL DEFAULT '', models_path TEXT NOT NULL DEFAULT '', plugin_id TEXT NOT NULL DEFAULT '')`,
			`INSERT INTO providers (name, base_url) VALUES ('old', 'https://api.old.test')`,
		} {
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

		p, err := st.GetProvider("old")
		if err != nil {
			t.Fatalf("取老厂商: %v", err)
		}
		if p.AuthHeader != "" || p.AuthScheme != "" {
			t.Fatalf("补出来的列 = %q/%q，期望两项都是空串", p.AuthHeader, p.AuthScheme)
		}
	})
}

// TestAccountDerivedAuth 确认账号读出来的凭据头跟着厂商行走：账号行不存这两列，
// 改厂商行立即对引用它的账号生效。
func TestAccountDerivedAuth(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{
		Name: "doubao-voice", BaseURL: "https://openspeech.bytedance.com",
		AuthHeader: "X-Api-Key", AuthScheme: "raw",
	}, []store.ProviderRoute{{Protocol: "openai-speech", Path: "/api/v3/tts/unidirectional"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	if err := st.CreateAccount(&store.Account{Name: "d1", Provider: "doubao-voice", KeyCipher: []byte("k"), Enabled: true}); err != nil {
		t.Fatalf("建账号: %v", err)
	}

	acc, err := st.GetAccount("d1")
	if err != nil {
		t.Fatalf("取账号: %v", err)
	}
	if acc.AuthHeader != "x-api-key" || acc.AuthScheme != "raw" {
		t.Fatalf("账号派生的凭据头 = %q/%q，期望 x-api-key/raw", acc.AuthHeader, acc.AuthScheme)
	}

	scheme := "bearer"
	if err := st.SetProvider("doubao-voice", store.ProviderPatch{AuthScheme: &scheme}); err != nil {
		t.Fatalf("改厂商写法: %v", err)
	}
	acc, err = st.GetAccount("d1")
	if err != nil {
		t.Fatalf("再取账号: %v", err)
	}
	if acc.AuthScheme != "bearer" {
		t.Fatalf("账号派生的写法 = %q，期望跟着厂商行变成 bearer", acc.AuthScheme)
	}
}

// TestPluginCatalogAuthConflict 钉住插件安装对凭据头的两条规则：
// 库里已声明且不同 → 整笔失败、一行都不写；库里是空值 → 补上。
func TestPluginCatalogAuthConflict(t *testing.T) {
	t.Run("冲突时整笔失败", func(t *testing.T) {
		st := openTemp(t)
		if err := st.CreateProvider(store.Provider{
			Name: "doubao-voice", BaseURL: "https://openspeech.bytedance.com",
			AuthHeader: "Authorization", AuthScheme: "bearer",
		}, []store.ProviderRoute{{Protocol: "openai-speech", Path: "/api/v3/tts/unidirectional"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		err := st.ApplyPluginCatalog("com.example.doubao", store.PluginCatalog{
			Provider: "doubao-voice", Name: "Doubao Voice", BaseURL: "https://openspeech.bytedance.com",
			AuthHeader: "x-api-key", AuthScheme: "raw",
			Routes: []store.ProviderRoute{{Protocol: "openai-speech", Path: "/api/v3/tts/unidirectional"}},
		}, "plugin")
		if err == nil || !strings.Contains(err.Error(), "凭据头") {
			t.Fatalf("应报凭据头冲突并给出两条出路，实际 %v", err)
		}
		// 整笔回滚：厂商归属没有被写进 plugin_id。
		if got := pluginIDOf(t, st, "doubao-voice"); got != "" {
			t.Fatalf("失败的安装写了归属：plugin_id = %q", got)
		}
		p, err := st.GetProvider("doubao-voice")
		if err != nil {
			t.Fatalf("取厂商: %v", err)
		}
		if p.AuthHeader != "authorization" {
			t.Fatalf("失败的安装改了已有声明: %q", p.AuthHeader)
		}
	})

	t.Run("空值补上", func(t *testing.T) {
		st := openTemp(t)
		// 厂商先由用户手建：地址一致、凭据头还没声明。
		if err := st.CreateProvider(store.Provider{
			Name: "doubao-voice", BaseURL: "https://openspeech.bytedance.com",
		}, []store.ProviderRoute{{Protocol: "openai-speech", Path: "/api/v3/tts/unidirectional"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		if err := st.ApplyPluginCatalog("com.example.doubao", store.PluginCatalog{
			Provider: "doubao-voice", Name: "Doubao Voice", BaseURL: "https://openspeech.bytedance.com",
			AuthHeader: "x-api-key", AuthScheme: "raw",
			Routes: []store.ProviderRoute{{Protocol: "openai-speech", Path: "/api/v3/tts/unidirectional"}},
		}, "plugin"); err != nil {
			t.Fatalf("安装: %v", err)
		}
		p, err := st.GetProvider("doubao-voice")
		if err != nil {
			t.Fatalf("取厂商: %v", err)
		}
		if p.AuthHeader != "x-api-key" || p.AuthScheme != "raw" {
			t.Fatalf("凭据头 = %q/%q，期望安装把空值补上", p.AuthHeader, p.AuthScheme)
		}
	})
}
