// Package storetest 是 store 的测试专用入口：按沙箱环境打开库、直接改探活时间戳这类
// 子命令不对外暴露的状态。
//
// 它只给测试用，生产代码不引用。放在 store 下面是因为改表的语句属于 store 的职责：
// cli 的测试因此不必自己打开库，也不必写裸 SQL，否则「cli 不直接连数据库」这条边界
// 会在测试里被绕过。
package storetest

import (
	"os"
	"testing"

	"lax/internal/config"
	"lax/internal/store"
)

// DB 是测试用的库句柄。读写仍走 store 的方法，这里只补测试需要的那几个捷径。
type DB struct {
	*store.Store
}

// Open 按当前进程的 XDG 环境打开沙箱里的库，测试结束时关闭。
// 库必须已经由 `lax init` 建好：这里不初始化，免得漏跑 init 的用例
// 拿到一个空库却以为是「还没同步」。
func Open(t *testing.T) *DB {
	t.Helper()
	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析数据路径: %v", err)
	}
	if _, err := os.Stat(paths.DB); err != nil {
		t.Fatalf("数据库 %s 不存在，用例要先跑 init", paths.DB)
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &DB{Store: st}
}

// BackdateModel 把某个账号某个模型的 checked_at 直接改成 ts，用来模拟
// 「这条结论已经放了多久」。ts 为 0 表示「有结论但没记时间」。
// 影响行数不是 1 就直接失败：模型名写错时不能让用例悄悄通过。
func (d *DB) BackdateModel(t *testing.T, account, model string, ts int64) {
	t.Helper()
	acc, err := d.GetAccount(account)
	if err != nil {
		t.Fatalf("读账号 %s: %v", account, err)
	}
	res, err := d.DB().Exec(`UPDATE account_models SET checked_at = ? WHERE account_id = ? AND model = ?`,
		ts, acc.ID, model)
	if err != nil {
		t.Fatalf("改 %s/%s 的 checked_at: %v", account, model, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("读影响行数: %v", err)
	}
	if n != 1 {
		t.Fatalf("改 %s/%s 的 checked_at 影响了 %d 行，期望 1 行", account, model, n)
	}
}

// ModelRow 返回某个账号某个模型当前的探活行。
func (d *DB) ModelRow(t *testing.T, account, model string) store.AccountModel {
	t.Helper()
	acc, err := d.GetAccount(account)
	if err != nil {
		t.Fatalf("读账号 %s: %v", account, err)
	}
	rows, err := d.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读 %s 的模型行: %v", account, err)
	}
	for _, r := range rows {
		if r.Model == model {
			return r
		}
	}
	t.Fatalf("账号 %s 没有模型 %s 的行", account, model)
	return store.AccountModel{}
}
