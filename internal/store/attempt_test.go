package store_test

import (
	"testing"

	"lax/internal/store"
)

// TestRecordAttemptWritesRowAndStateTogether 钉住一次尝试的账与结论在同一个事务里落库：
// 请求行与冷却/清零/停用要么都写，要么都不写。
func TestRecordAttemptWritesRowAndStateTogether(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", nil)

	rec := func(status int, ok bool) *store.RequestRecord {
		return &store.RequestRecord{
			TS: 100, Inbound: "anthropic", Outbound: "openai-chat",
			ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
			HTTPStatus: status, OK: ok, LatencyMS: 5,
		}
	}
	get := func() store.Account {
		t.Helper()
		a, err := st.GetAccount(acc.Name)
		if err != nil {
			t.Fatalf("读账号: %v", err)
		}
		return a
	}

	// 失败：请求行与冷却一起写，fail_streak 加一。
	fail := rec(429, false)
	fail.Error = "rate limited"
	if err := st.RecordAttempt(fail, store.AttemptState{
		Failure: true, CooldownUntil: 160, LastError: "rate limited",
	}); err != nil {
		t.Fatalf("记录失败尝试: %v", err)
	}
	if fail.ID == 0 {
		t.Fatal("请求记录应拿到自增 id")
	}
	a := get()
	if a.CooldownUntil != 160 || a.LastError != "rate limited" || a.FailStreak != 1 {
		t.Fatalf("失败后的账号状态 = %+v", a)
	}

	// 成功：请求行与清零一起写。
	if err := st.RecordAttempt(rec(200, true), store.AttemptState{Clear: true}); err != nil {
		t.Fatalf("记录成功尝试: %v", err)
	}
	a = get()
	if a.CooldownUntil != 0 || a.LastError != "" || a.FailStreak != 0 {
		t.Fatalf("成功后的账号状态 = %+v，期望冷却与连续失败清掉", a)
	}

	// 401：请求行与停用一起写。
	if err := st.RecordAttempt(rec(401, false), store.AttemptState{Disable: true, LastError: "invalid key"}); err != nil {
		t.Fatalf("记录 401 尝试: %v", err)
	}
	a = get()
	if a.Enabled || a.LastError != "invalid key" {
		t.Fatalf("401 后的账号状态 = %+v，期望停用并记下错误", a)
	}

	recs, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("请求记录 = %d 行，期望 3 行（每次尝试各一行）", len(recs))
	}

	// 写不进去时账号状态也不该变：一个事务里要么都成要么都不成。
	if _, err := st.DB().Exec("PRAGMA query_only = 1"); err != nil {
		t.Fatalf("把库设成只读: %v", err)
	}
	if err := st.RecordAttempt(rec(500, false), store.AttemptState{
		Failure: true, CooldownUntil: 999, LastError: "boom",
	}); err == nil {
		t.Fatal("只读库上记录尝试应报错")
	}
	if _, err := st.DB().Exec("PRAGMA query_only = 0"); err != nil {
		t.Fatalf("恢复可写: %v", err)
	}
	a = get()
	if a.CooldownUntil != 0 || a.FailStreak != 0 || a.LastError != "invalid key" {
		t.Fatalf("写入失败后账号状态不该变: %+v", a)
	}
	recs, err = st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("写入失败后不该多出请求记录，实际 %d 行", len(recs))
	}
}
