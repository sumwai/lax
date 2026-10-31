package store_test

import (
	"testing"

	"lax/internal/store"
)

func TestPluginAuditAppendAndList(t *testing.T) {
	st := openTemp(t)

	entries := []store.PluginAudit{
		{PluginID: "com.example.mimo", Version: "1.0.0", Action: store.AuditInstall, At: 100},
		{PluginID: "com.example.mimo", Version: "1.1.0", Action: store.AuditUpdate, At: 200,
			PermissionsBeforeJSON: `{"network":["a.example"]}`, PermissionsAfterJSON: `{"network":["a.example","b.example"]}`},
		{PluginID: "com.example.other", Version: "0.1.0", Action: store.AuditInstall, At: 150},
	}
	for _, entry := range entries {
		if err := st.AppendPluginAudit(entry); err != nil {
			t.Fatalf("追加审计失败: %v", err)
		}
	}

	got, err := st.PluginAuditLog("com.example.mimo", 10)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("审计条数 = %d，期望 2", len(got))
	}
	if got[0].Action != store.AuditUpdate || got[1].Action != store.AuditInstall {
		t.Fatalf("排序 = %s、%s，期望按时间倒序", got[0].Action, got[1].Action)
	}
	if got[0].Result != "ok" {
		t.Fatalf("默认结果 = %q，期望 ok", got[0].Result)
	}
	if got[0].PermissionsAfterJSON == "" || got[0].PermissionsBeforeJSON == "" {
		t.Fatalf("权限快照丢了: %+v", got[0])
	}

	if _, err := st.PluginAuditLog("com.example.missing", 10); err != nil {
		t.Fatalf("没有审计不是错误: %v", err)
	}
	if err := st.AppendPluginAudit(store.PluginAudit{Action: store.AuditInstall}); err == nil {
		t.Fatal("缺插件 ID 的审计应当被拒")
	}
	if err := st.AppendPluginAudit(store.PluginAudit{PluginID: "x"}); err == nil {
		t.Fatal("缺动作的审计应当被拒")
	}
}
