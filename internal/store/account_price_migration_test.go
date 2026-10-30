package store_test

import (
	"database/sql"
	"testing"

	"switchboard/internal/store"
)

// TestMigration0016MovesAccountPricesToOverride 用一个停在 0015 的库跑完 0016：
// 账号行的四个价搬成 account_models 上的覆盖值（只补空的），四个价格列随后消失，
// user_version 走到 16。
//
// 老库用 migrations 目录里 ≤0015 的文件本身来建，不手抄表结构：手抄的形状会随实现漂移。
func TestMigration0016MovesAccountPricesToOverride(t *testing.T) {
	path := t.TempDir() + "/v15.db"
	db := openDatabaseAt(t, path, 15)

	// ds1 有一份套餐价（输入 2、输出 4、缓存写 0.5，缓存读没填），两个模型行；
	// ds2 也有价，但它一行模型都没有——没有地方可挂，这几份价只能丢。
	if _, err := db.Exec(`INSERT INTO accounts
		(id, name, provider, key_cipher, price_in, price_out, price_cache_read, price_cache_write,
		 priority, enabled, created_at)
		VALUES (1, 'ds1', 'deepseek', X'010203', 2, 4, NULL, 0.5, 100, 1, 100),
		       (2, 'ds2', 'deepseek', X'040506', 3, 5, 1, 2, 100, 1, 100)`); err != nil {
		t.Fatalf("建老账号: %v", err)
	}
	// m1 一个价都没覆盖过；m2 已经有自己的输入价 9，迁移不能把它改回账号的 2。
	for _, stmt := range []string{
		`INSERT INTO account_models (account_id, model, access, http_status, error, checked_at, protocol)
			VALUES (1, 'm1', 'allowed', 200, '', 12345, 'anthropic')`,
		`INSERT INTO account_models (account_id, model, access, http_status, error, checked_at, protocol, price_in)
			VALUES (1, 'm2', 'allowed', 200, '', 222, '', 9)`,
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

	var version int
	if err := st.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读 user_version: %v", err)
	}
	if version != 16 {
		t.Fatalf("user_version = %d，期望 16", version)
	}
	assertLatestVersion(t, st)
	assertAccountHasNoPriceColumns(t, st)

	// 覆盖值：m1 拿到账号上的那几项（缓存读账号上也是空，搬完仍空）；m2 只补空的那几项。
	type prices struct {
		in, out, cacheRead, cacheWrite sql.NullFloat64
	}
	got := map[string]prices{}
	rows, err := st.DB().Query(`SELECT model, price_in, price_out, price_cache_read, price_cache_write
		FROM account_models WHERE account_id = 1`)
	if err != nil {
		t.Fatalf("读覆盖价: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var p prices
		if err := rows.Scan(&model, &p.in, &p.out, &p.cacheRead, &p.cacheWrite); err != nil {
			t.Fatalf("读覆盖价: %v", err)
		}
		got[model] = p
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读覆盖价: %v", err)
	}
	wantFloat(t, "m1 的输入价", got["m1"].in, 2)
	wantFloat(t, "m1 的输出价", got["m1"].out, 4)
	if got["m1"].cacheRead.Valid {
		t.Fatalf("m1 的缓存读价 = %v，账号上本来就没填，搬完该还是空", got["m1"].cacheRead.Float64)
	}
	wantFloat(t, "m1 的缓存写价", got["m1"].cacheWrite, 0.5)
	// m2 已经有的覆盖值不动，空的补上。
	wantFloat(t, "m2 的输入价", got["m2"].in, 9)
	wantFloat(t, "m2 的输出价", got["m2"].out, 4)
	wantFloat(t, "m2 的缓存写价", got["m2"].cacheWrite, 0.5)

	// 搬成覆盖值，不是厂商级标价：目录里的价一列都不该因为这次迁移出现。
	var priced int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM provider_models
		WHERE price_in IS NOT NULL OR price_out IS NOT NULL
		   OR price_cache_read IS NOT NULL OR price_cache_write IS NOT NULL`).Scan(&priced); err != nil {
		t.Fatalf("数目录里的价: %v", err)
	}
	if priced != 0 {
		t.Fatalf("迁移往目录里写了 %d 行价格：账号价是个人套餐价，不是厂商级标价", priced)
	}

	// 账号价对它的每个模型生效：ds1 的 m1 读到 2 / 4，缓存读按输入价回退。
	info := oneInfo(t, st, 1, "m1")
	if info.PriceIn == nil || *info.PriceIn != 2 || info.PriceOut == nil || *info.PriceOut != 4 {
		t.Fatalf("m1 的有效价 = in %v / out %v，期望账号价搬过来的 2 / 4", info.PriceIn, info.PriceOut)
	}

	// 没有模型行的账号（ds2）搬不进任何地方：读回来只剩账号自己的列。
	acc, err := st.GetAccount("ds2")
	if err != nil {
		t.Fatalf("读 ds2: %v", err)
	}
	if acc.Name != "ds2" || acc.Provider != "deepseek" {
		t.Fatalf("ds2 的账号列被改动: %+v", acc)
	}
	if rows, err := st.AccountModels(2); err != nil || len(rows) != 0 {
		t.Fatalf("ds2 不该被补出模型行: %+v（err %v）", rows, err)
	}

	// 重开一次不重跑迁移：user_version 已经是 16，搬过的覆盖值不会被再改一遍。
	if err := st.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}
	again, err := store.Open(path)
	if err != nil {
		t.Fatalf("再开一次: %v", err)
	}
	defer again.Close()
	if err := again.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("再读 user_version: %v", err)
	}
	if version != 16 {
		t.Fatalf("重开之后 user_version = %d，期望 16", version)
	}
	if info := oneInfo(t, again, 1, "m1"); info.PriceIn == nil || *info.PriceIn != 2 {
		t.Fatalf("重开之后 m1 的输入价 = %v，期望仍是 2", info.PriceIn)
	}
}

// wantFloat 断言一个可空列的值。
func wantFloat(t *testing.T, name string, got sql.NullFloat64, want float64) {
	t.Helper()
	if !got.Valid || got.Float64 != want {
		t.Fatalf("%s = %+v，期望 %v", name, got, want)
	}
}
