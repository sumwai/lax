package store_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"lax/internal/store"
)

// TestRouteFailurePolicyCRUDOrderingAndJSON 覆盖策略的增删改查、启用过滤、排序与
// statuses/error_contains 的 JSON 文本往返。
func TestRouteFailurePolicyCRUDOrderingAndJSON(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", []string{"m1"})

	first := store.RouteFailurePolicy{
		Enabled:       true,
		Priority:      20,
		AccountID:     acc.ID,
		Provider:      "p1",
		Model:         "m1",
		Statuses:      []string{"500", "5xx"},
		ErrorContains: []string{"Insufficient Credits", `引号 " 与逗号,`},
		ErrorRegex:    `(?i)rate.?limit`,
		RetryCount:    2,
		RetryBaseMS:   250,
		NextRoute:     true,
		Note:          "换号",
	}
	if err := st.CreateRouteFailurePolicy(&first); err != nil {
		t.Fatalf("新增第一条: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("新增后应有 id")
	}
	if first.CreatedAt == 0 || first.UpdatedAt == 0 || first.CreatedAt != first.UpdatedAt {
		t.Fatalf("创建时间 = %d / %d，期望同一时刻且非 0", first.CreatedAt, first.UpdatedAt)
	}

	got, err := st.GetRouteFailurePolicy(first.ID)
	if err != nil {
		t.Fatalf("按 id 取: %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("读回的策略 = %+v\n期望 %+v", got, first)
	}

	second := store.RouteFailurePolicy{
		Enabled:         true,
		Priority:        10,
		Statuses:        []string{"4xx"},
		RetryCount:      0,
		CooldownSeconds: 60,
		ReturnError:     true,
	}
	if err := st.CreateRouteFailurePolicy(&second); err != nil {
		t.Fatalf("新增第二条: %v", err)
	}
	third := store.RouteFailurePolicy{
		Enabled:    false,
		Priority:   5,
		Statuses:   []string{"401"},
		RetryCount: 1,
		NextRoute:  true,
	}
	if err := st.CreateRouteFailurePolicy(&third); err != nil {
		t.Fatalf("新增第三条: %v", err)
	}

	list, err := st.ListRouteFailurePolicies()
	if err != nil {
		t.Fatalf("列启用规则: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("启用规则顺序 = %+v，期望按 priority 10、20", list)
	}
	all, err := st.ListAllRouteFailurePolicies()
	if err != nil {
		t.Fatalf("列全部规则: %v", err)
	}
	if len(all) != 3 || all[0].ID != third.ID || all[1].ID != second.ID || all[2].ID != first.ID {
		t.Fatalf("全部规则顺序 = %+v，期望按 priority 5、10、20", all)
	}

	// 整条覆盖：优先级、条件、动作都换掉，未写的字段落成零值。
	updated := first
	updated.Priority = 1
	updated.Provider = ""
	updated.Model = ""
	updated.Statuses = []string{"429"}
	updated.ErrorContains = nil
	updated.ErrorRegex = ""
	updated.RetryCount = 1
	updated.RetryBaseMS = 0 // 归一成 500
	updated.NextRoute = false
	updated.ReturnError = true
	updated.Note = "更新后"
	if err := st.UpdateRouteFailurePolicy(updated); err != nil {
		t.Fatalf("更新: %v", err)
	}
	got, err = st.GetRouteFailurePolicy(first.ID)
	if err != nil {
		t.Fatalf("更新后再取: %v", err)
	}
	if got.Priority != 1 || got.Statuses[0] != "429" || len(got.ErrorContains) != 0 ||
		got.ErrorRegex != "" || got.RetryCount != 1 || got.RetryBaseMS != 500 ||
		got.NextRoute || !got.ReturnError || got.Note != "更新后" || got.AccountID != acc.ID {
		t.Fatalf("更新后的策略 = %+v", got)
	}
	if got.CreatedAt != first.CreatedAt {
		t.Fatalf("更新不该改 created_at: %d -> %d", first.CreatedAt, got.CreatedAt)
	}
	if got.UpdatedAt == 0 {
		t.Fatal("更新应写 updated_at")
	}

	// 停用第二条后，启用列表只剩第一条。
	if err := st.SetRouteFailurePolicyEnabled(second.ID, false); err != nil {
		t.Fatalf("停用: %v", err)
	}
	list, err = st.ListRouteFailurePolicies()
	if err != nil {
		t.Fatalf("停用后再列: %v", err)
	}
	if len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("停用后的启用规则 = %+v", list)
	}
	all, err = st.ListAllRouteFailurePolicies()
	if err != nil {
		t.Fatalf("停用后再列全部: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("停用不该删行，全部规则 = %d 条", len(all))
	}

	if err := st.DeleteRouteFailurePolicy(second.ID); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if _, err := st.GetRouteFailurePolicy(second.ID); !errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
		t.Fatalf("删后取 = %v，期望 ErrRouteFailurePolicyNotFound", err)
	}
	if err := st.DeleteRouteFailurePolicy(second.ID); !errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
		t.Fatalf("重复删除 = %v，期望 ErrRouteFailurePolicyNotFound", err)
	}
	if err := st.SetRouteFailurePolicyEnabled(second.ID, true); !errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
		t.Fatalf("启停不存在的规则 = %v，期望 ErrRouteFailurePolicyNotFound", err)
	}

	// 账号删除要连带删掉挂在它身上的规则（显式删除 + 外键两条路径都应成立）。
	scoped := store.RouteFailurePolicy{
		Enabled: true, Priority: 0, AccountID: acc.ID, Statuses: []string{"401"},
		Disable: true, ReturnError: true,
	}
	if err := st.CreateRouteFailurePolicy(&scoped); err != nil {
		t.Fatalf("新增账号级规则: %v", err)
	}
	if err := st.DeleteAccount(acc.Name); err != nil {
		t.Fatalf("删除账号: %v", err)
	}
	if _, err := st.GetRouteFailurePolicy(scoped.ID); !errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
		t.Fatalf("账号删除后的规则 = %v，期望已被级联清掉", err)
	}
}

// TestRouteFailurePolicyNormalizeAndCreateDefaults 钉住两个写入口共用的归一规则：
// retry_base_ms 0 -> 500，流程位都为空 -> return。
func TestRouteFailurePolicyNormalizeAndCreateDefaults(t *testing.T) {
	p := store.RouteFailurePolicy{Enabled: true, Priority: 100, RetryCount: 1}
	if err := store.ValidateRouteFailurePolicy(p); err != nil {
		t.Fatalf("流程位都为空应允许校验，交给归一处理: %v", err)
	}
	store.NormalizeRouteFailurePolicy(&p)
	if p.RetryBaseMS != 500 || !p.ReturnError || p.NextRoute {
		t.Fatalf("归一后的策略 = %+v，期望 retry_base_ms=500、return", p)
	}

	st := openTemp(t)
	created := store.RouteFailurePolicy{Enabled: true, Priority: 100, RetryCount: 1}
	if err := st.CreateRouteFailurePolicy(&created); err != nil {
		t.Fatalf("新增: %v", err)
	}
	got, err := st.GetRouteFailurePolicy(created.ID)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.RetryBaseMS != 500 || !got.ReturnError || got.NextRoute {
		t.Fatalf("落库后的默认值 = %+v，期望 retry_base_ms=500、return", got)
	}
}

// TestUpdateRouteFailurePolicyCanDisable 钉住整条覆盖也能把 enabled 从 true 改成 false：
// GORM 对带 default:true 的零值字段会省略，行定义必须用指针才能显式写 false。
func TestUpdateRouteFailurePolicyCanDisable(t *testing.T) {
	st := openTemp(t)
	p := store.RouteFailurePolicy{Enabled: true, Priority: 10, Statuses: []string{"500"}, RetryCount: 1}
	if err := st.CreateRouteFailurePolicy(&p); err != nil {
		t.Fatalf("新增: %v", err)
	}
	updated := p
	updated.Enabled = false
	if err := st.UpdateRouteFailurePolicy(updated); err != nil {
		t.Fatalf("更新为停用: %v", err)
	}
	got, err := st.GetRouteFailurePolicy(p.ID)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.Enabled {
		t.Fatal("整条更新应能把规则改成停用")
	}
}

func TestValidateRouteFailurePolicy(t *testing.T) {
	base := func() store.RouteFailurePolicy {
		return store.RouteFailurePolicy{
			Enabled: true, Statuses: []string{"500"}, RetryCount: 1, RetryBaseMS: 500,
		}
	}
	cases := []struct {
		name   string
		mutate func(*store.RouteFailurePolicy)
		ok     bool
	}{
		{"基准有效", func(*store.RouteFailurePolicy) {}, true},
		{"没有动作", func(p *store.RouteFailurePolicy) { p.RetryCount = 0 }, false},
		{"停用与冷却互斥", func(p *store.RouteFailurePolicy) { p.Disable = true; p.CooldownSeconds = 1 }, false},
		{"换号与返回互斥", func(p *store.RouteFailurePolicy) { p.RetryCount = 0; p.NextRoute = true; p.ReturnError = true }, false},
		{"重试次数上限", func(p *store.RouteFailurePolicy) { p.RetryCount = 11 }, false},
		{"重试次数下限", func(p *store.RouteFailurePolicy) { p.RetryCount = -1 }, false},
		{"重试等待太小", func(p *store.RouteFailurePolicy) { p.RetryBaseMS = 99 }, false},
		{"重试等待太大", func(p *store.RouteFailurePolicy) { p.RetryBaseMS = 10001 }, false},
		{"冷却为负", func(p *store.RouteFailurePolicy) { p.RetryCount = 0; p.CooldownSeconds = -1 }, false},
		{"冷却超过 30 天", func(p *store.RouteFailurePolicy) { p.RetryCount = 0; p.CooldownSeconds = 30*24*3600 + 1 }, false},
		{"优先级为负", func(p *store.RouteFailurePolicy) { p.Priority = -1 }, false},
		{"账号 id 为负", func(p *store.RouteFailurePolicy) { p.AccountID = -1 }, false},
		{"正则编译失败", func(p *store.RouteFailurePolicy) { p.ErrorRegex = "(" }, false},
		{"正则过长", func(p *store.RouteFailurePolicy) { p.ErrorRegex = strings.Repeat("a", 513) }, false},
		{"错误文本为空", func(p *store.RouteFailurePolicy) { p.ErrorContains = []string{""} }, false},
		{"错误文本只有空白", func(p *store.RouteFailurePolicy) { p.ErrorContains = []string{"  "} }, false},
		{"错误文本过多", func(p *store.RouteFailurePolicy) {
			p.ErrorContains = make([]string, 21)
			for i := range p.ErrorContains {
				p.ErrorContains[i] = "x"
			}
		}, false},
		{"错误文本过长", func(p *store.RouteFailurePolicy) { p.ErrorContains = []string{strings.Repeat("x", 201)} }, false},
		{"状态码段外", func(p *store.RouteFailurePolicy) { p.Statuses = []string{"600"} }, false},
		{"状态码重复", func(p *store.RouteFailurePolicy) { p.Statuses = []string{"500", "500"} }, false},
		{"状态 token 为空", func(p *store.RouteFailurePolicy) { p.Statuses = []string{""} }, false},
		{"边界有效", func(p *store.RouteFailurePolicy) {
			p.RetryCount = 10
			p.RetryBaseMS = 10000
			p.CooldownSeconds = 30 * 24 * 3600
			p.Priority = 0
			p.Statuses = []string{"4xx", "400"}
			p.ErrorContains = []string{strings.Repeat("x", 200)}
			p.ErrorRegex = strings.Repeat("a", 512)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mutate(&p)
			err := store.ValidateRouteFailurePolicy(p)
			if tc.ok && err != nil {
				t.Fatalf("期望通过，实际 %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}

	st := openTemp(t)
	bad := store.RouteFailurePolicy{Enabled: true, Statuses: []string{"500"}, RetryCount: 11}
	if err := st.CreateRouteFailurePolicy(&bad); err == nil {
		t.Fatal("非法策略不该写进库")
	}
	if err := st.UpdateRouteFailurePolicy(store.RouteFailurePolicy{ID: 999, Enabled: true, RetryCount: 1}); !errors.Is(err, store.ErrRouteFailurePolicyNotFound) {
		t.Fatalf("更新不存在的规则 = %v，期望 ErrRouteFailurePolicyNotFound", err)
	}
}

func TestStatusTokenHelpers(t *testing.T) {
	got, err := store.ParseStatusTokens(" 5xx, 400 ,4XX,400 ")
	if err != nil {
		t.Fatalf("解析状态: %v", err)
	}
	if want := []string{"5xx", "400", "4xx"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("解析结果 = %v，期望 %v", got, want)
	}
	if got, err := store.ParseStatusTokens("   "); err != nil || got != nil {
		t.Fatalf("空输入 = %v, %v，期望 nil, nil", got, err)
	}
	if _, err := store.ParseStatusTokens("400,,500"); err == nil {
		t.Fatal("空 token 应报错")
	}
	if _, err := store.ParseStatusTokens("399,600,4xx5"); err == nil {
		t.Fatal("非法 token 应报错")
	}
	if err := store.ValidateStatusTokens([]string{"4xx", "400", "5xx"}); err != nil {
		t.Fatalf("合法 token 组合: %v", err)
	}
	if err := store.ValidateStatusTokens([]string{"400", "400"}); err == nil {
		t.Fatal("重复 token 应报错")
	}

	match := []struct {
		tokens []string
		status int
		want   bool
	}{
		{nil, 200, true},
		{[]string{"4xx"}, 404, true},
		{[]string{"4xx"}, 500, false},
		{[]string{"5xx"}, 503, true},
		{[]string{"500"}, 500, true},
		{[]string{"500"}, 501, false},
		{[]string{"429", "5xx"}, 429, true},
		{[]string{"429", "5xx"}, 200, false},
	}
	for _, tc := range match {
		if got := store.StatusTokenMatches(tc.tokens, tc.status); got != tc.want {
			t.Fatalf("StatusTokenMatches(%v, %d) = %v，期望 %v", tc.tokens, tc.status, got, tc.want)
		}
	}
}

func TestRouteFailurePolicyAccountCooldownAttemptState(t *testing.T) {
	st := openTemp(t)
	acc := newAccount(t, st, "ds1", []string{"m1"})

	rec := &store.RequestRecord{
		TS: 100, Inbound: "anthropic", Outbound: "openai-chat",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 400, LatencyMS: 5, Error: "余额不足",
	}
	until := time.Now().Add(30 * time.Minute).UnixMilli()
	if err := st.RecordAttempt(rec, store.AttemptState{
		AccountCooldown: true, CooldownUntilMS: until, LastError: "余额不足",
	}); err != nil {
		t.Fatalf("写账号级冷却: %v", err)
	}
	got, err := st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if got.CooldownUntilMS != until || got.LastError != "余额不足" {
		t.Fatalf("账号状态 = 冷却 %d / 错误 %q，期望 %d / 余额不足", got.CooldownUntilMS, got.LastError, until)
	}
	if f := got.ModelFailures["m1"]; f.CooldownUntilMS != 0 || f.FailStreak != 0 {
		t.Fatalf("账号级冷却不该写模型行: %+v", f)
	}

	// ClearFailures 同时清账号级与模型级冷却，并清 last_error。
	if err := st.ClearFailures(acc.ID); err != nil {
		t.Fatalf("清失败状态: %v", err)
	}
	got, _ = st.GetAccount(acc.Name)
	if got.CooldownUntilMS != 0 || got.LastError != "" {
		t.Fatalf("ClearFailures 后 = %d / %q，期望都清空", got.CooldownUntilMS, got.LastError)
	}

	// account enable 也要清账号级冷却。
	if err := st.RecordAttempt(rec, store.AttemptState{AccountCooldown: true, CooldownUntilMS: until, LastError: "额度不足"}); err != nil {
		t.Fatalf("再次写账号级冷却: %v", err)
	}
	if err := st.SetEnabled(acc.ID, true, ""); err != nil {
		t.Fatalf("启用: %v", err)
	}
	got, _ = st.GetAccount(acc.Name)
	if got.CooldownUntilMS != 0 || got.LastError != "" {
		t.Fatalf("启用后 = %d / %q，期望都清空", got.CooldownUntilMS, got.LastError)
	}

	// 换 key 也清账号级冷却并重新启用。
	if err := st.RecordAttempt(rec, store.AttemptState{AccountCooldown: true, CooldownUntilMS: until, LastError: "额度不足"}); err != nil {
		t.Fatalf("第三次写账号级冷却: %v", err)
	}
	if err := st.SetEnabled(acc.ID, false, "手工停用"); err != nil {
		t.Fatalf("停用: %v", err)
	}
	if err := st.RotateKey(acc.ID, []byte{9, 9, 9}); err != nil {
		t.Fatalf("换 key: %v", err)
	}
	got, _ = st.GetAccount(acc.Name)
	if got.CooldownUntilMS != 0 || got.LastError != "" || !got.Enabled {
		t.Fatalf("换 key 后 = 冷却 %d / 错误 %q / 启用 %v，期望清空并启用", got.CooldownUntilMS, got.LastError, got.Enabled)
	}
}

// TestRouteFailurePolicyTableShape 钉住新库里的两处结构（由 AutoMigrate 从 model 建出）：
// 新表存在，accounts 多了 cooldown_until_ms 列。迁移文件本身由 TestMigration0018ExecutesCleanly 执行。
func TestRouteFailurePolicyTableShape(t *testing.T) {
	st := openTemp(t)
	var name string
	if err := st.DB().QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?",
		"route_failure_policies").Scan(&name); err != nil {
		t.Fatalf("route_failure_policies 表不存在: %v", err)
	}

	rows, err := st.DB().Query("PRAGMA table_info(accounts)")
	if err != nil {
		t.Fatalf("读 accounts 列: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var col, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &col, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("扫描列: %v", err)
		}
		if col == "cooldown_until_ms" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列: %v", err)
	}
	if !found {
		t.Fatal("accounts 缺少 cooldown_until_ms 列")
	}
}

// TestMigration0018ExecutesCleanly 直接执行 0018 迁移文件本身，钉住表、索引与账号冷却列。
// 运行时新库由 AutoMigrate 按 model 定义建出同一形状，只有这个测试会执行迁移 SQL；
// 语句写错、索引名与 model tag 不一致，只有这里能发现。
func TestMigration0018ExecutesCleanly(t *testing.T) {
	path := t.TempDir() + "/v18.db"
	db := openDatabaseAt(t, path, 18)
	defer db.Close()

	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'route_failure_policies'`).Scan(&name); err != nil {
		t.Fatalf("迁移 0018 后 route_failure_policies 表不存在: %v", err)
	}
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'route_failure_policies_order'`).Scan(&name); err != nil {
		t.Fatalf("迁移 0018 后排序索引 route_failure_policies_order 不存在: %v", err)
	}

	rows, err := db.Query(`PRAGMA table_info(accounts)`)
	if err != nil {
		t.Fatalf("读 accounts 列: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var col, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &col, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("扫描列: %v", err)
		}
		if col == "cooldown_until_ms" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列: %v", err)
	}
	if !found {
		t.Fatal("迁移 0018 后 accounts 缺少 cooldown_until_ms 列")
	}
}
