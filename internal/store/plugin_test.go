package store_test

import (
	"errors"
	"testing"

	"lax/internal/store"
)

// 插件状态的写入口：版本行只插不改、活动版本由 active 决定、删除插件带走它自己的东西。

// TestPluginTablesFromEmptyDatabase 确认空库跑完之后四张插件表都在，且都能读写。
func TestPluginTablesFromEmptyDatabase(t *testing.T) {
	st := openTemp(t)

	for _, table := range []string{"plugins", "plugin_versions", "plugin_options", "model_operations"} {
		var name string
		if err := st.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("表 %s 不存在: %v", table, err)
		}
	}

	if err := st.PutPluginVersion(store.PluginVersion{
		PluginID: "com.example.mimo", Version: "1.0.0", Source: "local",
	}, true); err != nil {
		t.Fatalf("写版本: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO plugin_options (plugin_id, key, value_json, updated_at) VALUES (?, ?, ?, ?)`,
		"com.example.mimo", "region", `{"v":"SGP"}`, 5); err != nil {
		t.Fatalf("写配置项: %v", err)
	}
	if err := st.SetModelOperation(store.ModelOperation{
		Provider: "acme", Model: "m1", Operation: "chat.complete@1",
		DialectID: "lax/openai-chat@1", Source: "manual",
	}); err != nil {
		t.Fatalf("写方言绑定: %v", err)
	}

	plugins, err := st.ListPlugins()
	if err != nil || len(plugins) != 1 {
		t.Fatalf("插件列表 = %v, err = %v", plugins, err)
	}
	if _, ok, err := st.ModelOperation("acme", "m1", "chat.complete@1"); err != nil || !ok {
		t.Fatalf("读回方言绑定: ok=%v err=%v", ok, err)
	}
}

// TestPutPluginVersion 钉住版本写入的语义：首装建插件行、重复版本被拒、
// active=false 不动活动版本、active=true 切换活动版本。
func TestPutPluginVersion(t *testing.T) {
	st := openTemp(t)
	const id = "com.example.mimo"

	if err := st.PutPluginVersion(store.PluginVersion{
		PluginID: id, Version: "1.0.0", ABI: "lax.plugin/v1", Runtime: "declarative",
		ManifestJSON: `{"id":"com.example.mimo"}`, PackageSHA256: "abc",
		Source: "local", SignatureStatus: "unsigned", InstalledAt: 100,
	}, true); err != nil {
		t.Fatalf("首次安装: %v", err)
	}

	p, ok, err := st.PluginByID(id)
	if err != nil || !ok {
		t.Fatalf("读插件: ok=%v err=%v", ok, err)
	}
	if p.ActiveVersion != "1.0.0" || !p.Enabled || p.State != "ok" || p.LastError != "" || p.Source != "local" {
		t.Fatalf("新装的插件行 = %+v", p)
	}

	v, ok, err := st.ActivePluginVersion(id)
	if err != nil || !ok {
		t.Fatalf("读活动版本: ok=%v err=%v", ok, err)
	}
	if v.ManifestJSON != `{"id":"com.example.mimo"}` || v.InstalledAt != 100 || v.SignatureStatus != "unsigned" {
		t.Fatalf("活动版本 = %+v", v)
	}

	// 同一个版本再写一次：拒绝，且原来的行一个字不动。
	err = st.PutPluginVersion(store.PluginVersion{
		PluginID: id, Version: "1.0.0", ManifestJSON: `{"id":"changed"}`,
	}, true)
	if !errors.Is(err, store.ErrPluginVersionExists) {
		t.Fatalf("重复版本应被拒，得到 %v", err)
	}
	if v, _, _ := st.ActivePluginVersion(id); v.ManifestJSON != `{"id":"com.example.mimo"}` {
		t.Fatalf("被拒的写入改动了已装版本: %+v", v)
	}

	// active=false：装上了，但不是活动版本。
	if err := st.PutPluginVersion(store.PluginVersion{
		PluginID: id, Version: "1.1.0", Source: "github", InstalledAt: 200,
	}, false); err != nil {
		t.Fatalf("安装 1.1.0: %v", err)
	}
	p, _, _ = st.PluginByID(id)
	if p.ActiveVersion != "1.0.0" {
		t.Fatalf("active=false 改动了活动版本: %q", p.ActiveVersion)
	}
	versions, err := st.PluginVersions(id)
	if err != nil || len(versions) != 2 {
		t.Fatalf("版本列表 = %v, err = %v", versions, err)
	}
	if versions[0].Version != "1.0.0" || versions[1].Version != "1.1.0" {
		t.Fatalf("版本列表应按安装时间升序: %v", versions)
	}

	// active=true：切过去，source 跟着最新装的版本。
	if err := st.PutPluginVersion(store.PluginVersion{
		PluginID: id, Version: "1.2.0", Source: "npm", InstalledAt: 300,
	}, true); err != nil {
		t.Fatalf("安装 1.2.0: %v", err)
	}
	p, _, _ = st.PluginByID(id)
	if p.ActiveVersion != "1.2.0" || p.Source != "npm" {
		t.Fatalf("装新版本之后 = %+v", p)
	}
	if _, ok, _ := st.ActivePluginVersion(id); !ok {
		t.Fatal("切过之后应能读到活动版本")
	}
	if _, ok, err := st.ActivePluginVersion("com.example.nope"); err != nil || ok {
		t.Fatalf("没装过的插件不该有活动版本: ok=%v err=%v", ok, err)
	}
}

// TestSetPluginEnabledAndState 区分「没这行」与「改成功了」。
func TestSetPluginEnabledAndState(t *testing.T) {
	st := openTemp(t)
	const id = "com.example.mimo"

	if err := st.SetPluginEnabled(id, false); !errors.Is(err, store.ErrPluginNotFound) {
		t.Fatalf("停用没装过的插件应返回 ErrPluginNotFound，得到 %v", err)
	}
	if err := st.SetPluginState(id, "failed", "manifest 坏了"); !errors.Is(err, store.ErrPluginNotFound) {
		t.Fatalf("写没装过的插件状态应返回 ErrPluginNotFound，得到 %v", err)
	}

	if err := st.PutPluginVersion(store.PluginVersion{PluginID: id, Version: "1.0.0"}, true); err != nil {
		t.Fatalf("装插件: %v", err)
	}
	if err := st.SetPluginEnabled(id, false); err != nil {
		t.Fatalf("停用: %v", err)
	}
	// 重复停用同一个值也要成功：MySQL 的 RowsAffected 在值没变时是 0。
	if err := st.SetPluginEnabled(id, false); err != nil {
		t.Fatalf("重复停用: %v", err)
	}
	if p, _, _ := st.PluginByID(id); p.Enabled {
		t.Fatal("停用之后 enabled 仍为真")
	}

	if err := st.SetPluginState(id, "tampered", "哈希对不上"); err != nil {
		t.Fatalf("写状态: %v", err)
	}
	p, _, _ := st.PluginByID(id)
	if p.State != "tampered" || p.LastError != "哈希对不上" {
		t.Fatalf("状态 = %q / %q", p.State, p.LastError)
	}
}

// TestDeletePlugin 钉住连带删除的范围：版本、配置项与它自己的方言绑定都删掉，
// 手写（manual）与别的插件的绑定留着。
func TestDeletePlugin(t *testing.T) {
	st := openTemp(t)
	const id = "com.example.mimo"

	if err := st.PutPluginVersion(store.PluginVersion{PluginID: id, Version: "1.0.0"}, true); err != nil {
		t.Fatalf("装 1.0.0: %v", err)
	}
	if err := st.PutPluginVersion(store.PluginVersion{PluginID: id, Version: "1.1.0"}, false); err != nil {
		t.Fatalf("装 1.1.0: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO plugin_options (plugin_id, key, value_json, updated_at) VALUES (?, ?, ?, ?)`,
		id, "region", `{"v":"SGP"}`, 5); err != nil {
		t.Fatalf("写配置项: %v", err)
	}

	bindings := []store.ModelOperation{
		{Provider: "mimo", Model: "tts", Operation: "speech.synthesize@1",
			DialectID: id + "/mimo-chat-speech@1", Source: "plugin"},
		{Provider: "mimo", Model: "tts", Operation: "speech.transcribe@1",
			DialectID: id + "/mimo-chat-asr@1", Source: "plugin"},
		// 同一插件前缀但手写：插件卸载之后仍然成立。
		{Provider: "mimo", Model: "tts", Operation: "chat.complete@1",
			DialectID: id + "/mimo-chat-speech@1", Source: "manual"},
		// 别的插件的绑定。
		{Provider: "other", Model: "m1", Operation: "chat.complete@1",
			DialectID: "com.example.other/openai-chat@1", Source: "plugin"},
	}
	for _, b := range bindings {
		if err := st.SetModelOperation(b); err != nil {
			t.Fatalf("写绑定 %+v: %v", b, err)
		}
	}

	if err := st.DeletePlugin(id); err != nil {
		t.Fatalf("删插件: %v", err)
	}

	if _, ok, _ := st.PluginByID(id); ok {
		t.Fatal("插件行还在")
	}
	if versions, _ := st.PluginVersions(id); len(versions) != 0 {
		t.Fatalf("版本行还在: %v", versions)
	}
	var options int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM plugin_options WHERE plugin_id = ?`, id).Scan(&options); err != nil {
		t.Fatalf("数配置项: %v", err)
	}
	if options != 0 {
		t.Fatalf("配置项还剩 %d 行", options)
	}

	rows, err := st.ListModelOperations("", "")
	if err != nil {
		t.Fatalf("列绑定: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("删除后剩下的绑定 = %+v，期望 2 行（手写 1 行 + 别的插件 1 行）", rows)
	}
	for _, r := range rows {
		if r.Source == "plugin" && r.Provider != "other" {
			t.Fatalf("插件自己的绑定没删掉: %+v", r)
		}
	}

	if err := st.DeletePlugin(id); !errors.Is(err, store.ErrPluginNotFound) {
		t.Fatalf("重复删除应返回 ErrPluginNotFound，得到 %v", err)
	}
}

// TestPluginTablesOnLegacyDatabase 用一个停在旧版本号的库验：升级之后四张新表可用，
// 再打开一次不会把数据弄丢、也不会重复回填。
func TestPluginTablesOnLegacyDatabase(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	db := openDatabaseAt(t, path, 13)

	stmts := []string{
		`INSERT INTO providers (name, base_url, note, default_protocol, models_path)
			VALUES ('myco', 'https://my.example.com', '自建', 'openai-chat', '')`,
		`INSERT INTO provider_routes (provider, protocol, path, models_path)
			VALUES ('myco', 'openai-chat', '/v1/chat/completions', '/v1/models')`,
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('mc1', 'myco', 'openai-chat', 'https://my.example.com', X'010203', '["m1"]', 1, 100)`,
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
	if _, err := st.GetAccount("mc1"); err != nil {
		t.Fatalf("升级之后老账号应还在: %v", err)
	}

	const id = "com.example.mimo"
	if err := st.PutPluginVersion(store.PluginVersion{PluginID: id, Version: "1.0.0", InstalledAt: 100}, true); err != nil {
		t.Fatalf("在老库上装插件: %v", err)
	}
	if err := st.UpsertProviderModels("myco", []store.ProviderModel{{
		Model: "voice", Source: store.SourceManual, Capabilities: []string{store.CapabilitySpeech},
	}}); err != nil {
		t.Fatalf("写语音模型: %v", err)
	}
	if n, err := st.BackfillSpeechDialects(1000); err != nil || n != 1 {
		t.Fatalf("回填 = %d, err = %v，期望 1", n, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}

	// 再打开一次：新表的数据都在，回填仍然是幂等的。
	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("再次打开: %v", err)
	}
	defer st.Close()

	if p, ok, err := st.PluginByID(id); err != nil || !ok || p.ActiveVersion != "1.0.0" {
		t.Fatalf("重开之后插件行 = %+v, ok = %v, err = %v", p, ok, err)
	}
	if _, ok, err := st.ModelOperation("myco", "voice", "speech.synthesize@1"); err != nil || !ok {
		t.Fatalf("重开之后绑定丢了: ok=%v err=%v", ok, err)
	}
	if n, err := st.BackfillSpeechDialects(2000); err != nil || n != 0 {
		t.Fatalf("第二次回填 = %d, err = %v，期望 0", n, err)
	}
}
