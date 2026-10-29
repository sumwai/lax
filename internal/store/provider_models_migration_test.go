package store_test

import (
	"testing"

	"switchboard/internal/store"
)

// TestMigration0015SplitsModelFactsFromAccess 用一个停在 0014 的库跑完 0015：
// 可达性按 status 映射、目录按账号的厂商回填、手填清单补 manual 行，最后 accounts.models 那一列消失。
//
// 老库用 migrations 目录里 ≤0014 的文件本身来建，不手抄表结构：手抄的形状会随实现漂移。
func TestMigration0015SplitsModelFactsFromAccess(t *testing.T) {
	path := t.TempDir() + "/v14.db"
	db := openDatabaseAt(t, path, 14)

	// deepseek 是 0006 的种子厂商，这里不再建，只用它的名字当账号的 provider。
	// 手填清单里 m3、m4 两个名字：m3 在 account_models 里没有行，m4 有一行 notfound。
	if _, err := db.Exec(`INSERT INTO accounts (id, name, provider, key_cipher, models, priority, enabled, created_at)
		VALUES (1, 'ds1', 'deepseek', X'010203', '["m3","m4"]', 100, 1, 100)`); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm1', 'usable', 200, '', 12345, 'anthropic')`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm2', 'forbidden', 403, 'MODEL_NOT_IN_PLAN', 222, 'gemini')`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm4', 'notfound', 404, 'no such model', 333, '')`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm5', 'synced', 0, '', 0, '')`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm6', 'auth', 401, 'invalid key', 444, '')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建老模型行（%s）: %v", stmt, err)
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

	// status → access：usable → allowed、forbidden → forbidden、notfound → missing、其余 → error。
	wantAccess := map[string]string{
		"m1": store.AccessAllowed,
		"m2": store.AccessForbidden,
		"m3": store.AccessAllowed, // 手填清单补出来的行
		"m4": store.AccessMissing, // 原来是 notfound，手填清单不覆盖已有行
		"m5": store.AccessError,   // synced 没有定论
		"m6": store.AccessError,   // auth 不是模型层面的结论
	}
	rows, err := st.AccountModels(1)
	if err != nil {
		t.Fatalf("读可达性: %v", err)
	}
	if len(rows) != len(wantAccess) {
		t.Fatalf("可达性行数 = %d，期望 %d：%+v", len(rows), len(wantAccess), rows)
	}
	for _, r := range rows {
		if want, ok := wantAccess[r.Model]; !ok {
			t.Fatalf("多出一行 %s", r.Model)
		} else if r.Access != want {
			t.Fatalf("%s 的可达性 = %q，期望 %q", r.Model, r.Access, want)
		}
	}
	// 探活结论原样搬过来：HTTP 状态、备注、时间与协议都不动。
	if r := oneRow(t, st, 1, "m2"); r.HTTPStatus != 403 || r.Error != "MODEL_NOT_IN_PLAN" ||
		r.CheckedAt != 222 || r.Protocol != "gemini" {
		t.Fatalf("m2 的探活结论被改动: %+v", r)
	}
	// 手填清单补出来的行不带探活时间与协议。
	if r := oneRow(t, st, 1, "m3"); r.CheckedAt != 0 || r.HTTPStatus != 0 || r.Protocol != "" {
		t.Fatalf("手填清单补的行应只有 access: %+v", r)
	}

	// 目录：每个模型行搬一行 source='probe'，协议落到 protocols 列，其余列与 updated_at 留空。
	models, err := st.ProviderModels("deepseek")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(models) != len(wantAccess) {
		t.Fatalf("目录行数 = %d，期望 %d：%+v", len(models), len(wantAccess), models)
	}
	for _, m := range models {
		want := store.SourceProbe
		if m.Model == "m3" {
			// 手填清单里出现、account_models 里没有的名字补成 manual。
			want = store.SourceManual
		}
		if m.Source != want {
			t.Fatalf("%s 的来源 = %q，期望 %q", m.Model, m.Source, want)
		}
		if m.ContextWindow != nil || m.MaxOutput != nil || m.PriceIn != nil || m.PriceOut != nil ||
			m.PriceCacheRead != nil || m.PriceCacheWrite != nil || m.UpdatedAt != 0 {
			t.Fatalf("%s 的旧行没有对应来源的列应留空: %+v", m.Model, m)
		}
	}
	if m := oneModel(t, st, "deepseek", "m1"); len(m.Protocols) != 1 || m.Protocols[0] != "anthropic" {
		t.Fatalf("m1 的协议集合 = %v，期望 [anthropic]", m.Protocols)
	}
	if m := oneModel(t, st, "deepseek", "m5"); len(m.Protocols) != 0 {
		t.Fatalf("m5 没有发现过协议，实际 %v", m.Protocols)
	}

	// accounts.models 那一列必须真的没了，否则「派生值」这件事就还是两处真相。
	cols := accountColumns(t, st, "accounts")
	for _, c := range cols {
		if c == "models" {
			t.Fatalf("accounts 还有 models 列: %v", cols)
		}
	}

	// 升级之后选号读的就是可达性：手填与探活通过的模型都能选，其余不行。
	allowed, err := st.AllowedModels(1)
	if err != nil {
		t.Fatalf("读可达模型: %v", err)
	}
	if len(allowed) != 2 || allowed[0] != "m1" || allowed[1] != "m3" {
		t.Fatalf("可达模型 = %v，期望 [m1 m3]", allowed)
	}
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if len(acc.Models) != 2 {
		t.Fatalf("账号的派生模型列表 = %v，期望与可达模型一致", acc.Models)
	}
}

// TestMigration0015BackfillsProtocolFromAnyAccount 钉住「同一个模型在多个账号下各探到不同协议」的回填：
// 目录只有一行，协议取非空的那个，结果不随账号行序变化。
func TestMigration0015BackfillsProtocolFromAnyAccount(t *testing.T) {
	path := t.TempDir() + "/v14.db"
	db := openDatabaseAt(t, path, 14)

	// deepseek 是 0006 的种子厂商，这里不再建，只用它的名字当账号的 provider。
	for _, stmt := range []string{
		`INSERT INTO accounts (id, name, provider, key_cipher, models, priority, enabled, created_at)
			VALUES (1, 'a1', 'deepseek', X'01', '[]', 100, 1, 100)`,
		`INSERT INTO accounts (id, name, provider, key_cipher, models, priority, enabled, created_at)
			VALUES (2, 'a2', 'deepseek', X'02', '[]', 100, 1, 100)`,
		// 第一个账号没探出协议，第二个探到了 anthropic。
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (1, 'm1', 'usable', 200, '', 1, '')`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			VALUES (2, 'm1', 'usable', 200, '', 2, 'anthropic')`,
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

	models, err := st.ProviderModels("deepseek")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("目录行数 = %d，同一个模型在同一家厂商下应只有一行：%+v", len(models), models)
	}
	if len(models[0].Protocols) != 1 || models[0].Protocols[0] != "anthropic" {
		t.Fatalf("目录里的协议 = %v，期望取到非空的 anthropic", models[0].Protocols)
	}
}

// TestMigration0015OnEmptyDatabase 确认空库直接升到最新也是干净的：
// 两张表建好、没有半行数据、账户派生列表为空。
func TestMigration0015OnEmptyDatabase(t *testing.T) {
	st := openTemp(t)
	assertLatestVersion(t, st)

	if models, err := st.ProviderModels("deepseek"); err != nil || len(models) != 0 {
		t.Fatalf("空库的目录 = %+v, %v", models, err)
	}
	acc := newCatalogAccount(t, st, "a1", "deepseek")
	if allowed, err := st.AllowedModels(acc.ID); err != nil || len(allowed) != 0 {
		t.Fatalf("空账号的可达模型 = %v, %v", allowed, err)
	}
	if infos, err := st.EffectiveModels(acc.ID); err != nil || len(infos) != 0 {
		t.Fatalf("空账号的有效模型 = %+v, %v", infos, err)
	}
}
