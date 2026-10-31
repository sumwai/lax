package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/secret"
	"lax/internal/store"
)

// newAliasAccount 建一个声明了若干真实模型名的账号，别名冲突判定就看它。
func newAliasAccount(t *testing.T, st *store.Store, name string, models ...string) store.Account {
	t.Helper()
	acc := store.Account{
		Name:      name,
		Provider:  "deepseek",
		Protocol:  "openai-chat",
		BaseURL:   "https://api.deepseek.com",
		KeyCipher: []byte{1, 2, 3},
		Models:    models,
		Enabled:   true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号 %s: %v", name, err)
	}
	return acc
}

// TestMigration0013CreatesModelAliases 空库一路跑到最新：model_aliases 存在且为空，
// user_version 推到最新。别名一条都没有是正常状态，读出来是空切片而不是错误。
func TestMigration0013CreatesModelAliases(t *testing.T) {
	st := openTemp(t)
	assertLatestVersion(t, st)

	var cols int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('model_aliases')`).Scan(&cols); err != nil {
		t.Fatalf("读 model_aliases 的列: %v", err)
	}
	if cols != 3 {
		t.Fatalf("model_aliases 有 %d 列，期望 3 列（alias、target、created_at）", cols)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM model_aliases`).Scan(&n); err != nil {
		t.Fatalf("数别名: %v", err)
	}
	if n != 0 {
		t.Fatalf("新库里别名表应为空，实际 %d 行", n)
	}

	aliases, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("读别名: %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("新库里应没有别名，实际 %+v", aliases)
	}
}

// TestMigration0013UpgradesFrom0012 从 0012 升上来：只加表，账号、它的模型行与请求记录一个字段
// 都不动，新表为空；升级上来的库上别名照常可写可解析，冲突判定照样看得见老账号的模型名。
//
// 老库用 0001–0012 的迁移文件本身来建，不手抄 DDL：手抄的形状会随实现漂移，
// 而这里要验的正是真实老库升上来的结果。
func TestMigration0013UpgradesFrom0012(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12.db")
	db := openDatabaseAt(t, path, 12)

	stmts := []string{
		`INSERT INTO accounts (name, provider, protocol, base_url, key_cipher, models, enabled, created_at)
			VALUES ('ds1', 'deepseek', 'openai-chat', 'https://api.deepseek.com', X'010203',
				'["deepseek-chat","deepseek-reasoner"]', 1, 100)`,
		`INSERT INTO account_models (account_id, model, status, http_status, error, checked_at, protocol)
			SELECT id, 'deepseek-chat', 'usable', 200, '', 100, 'openai-chat' FROM accounts WHERE name = 'ds1'`,
		`INSERT INTO requests (ts, inbound, outbound, model_requested, model_used, http_status, ok)
			VALUES (100, 'openai-chat', 'openai-chat', 'deepseek-chat', 'deepseek-chat', 200, 1)`,
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

	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	if len(acc.Models) != 2 || acc.Models[1] != "deepseek-reasoner" || string(acc.KeyCipher) != "\x01\x02\x03" {
		t.Fatalf("老账号被改动: %+v", acc)
	}
	var reqs int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&reqs); err != nil {
		t.Fatalf("数请求记录: %v", err)
	}
	if reqs != 1 {
		t.Fatalf("老请求记录丢了：只剩 %d 行", reqs)
	}
	if aliases, err := st.ModelAliases(); err != nil {
		t.Fatalf("读升级后的别名表: %v", err)
	} else if len(aliases) != 0 {
		t.Fatalf("升级之后别名表应为空，实际 %+v", aliases)
	}

	// 新表在升级上来的库上可用：冲突判定看得到老账号声明的真名，写进去的别名也解析得出来。
	if err := st.SetModelAlias("deepseek-chat", "glm-4.6"); !errors.Is(err, store.ErrModelAliasShadow) {
		t.Fatalf("拿老账号的真名当别名应报 ErrModelAliasShadow，实际 %v", err)
	}
	if err := st.SetModelAlias("fast", "glm-4.6"); err != nil {
		t.Fatalf("升级后写别名: %v", err)
	}
	if got, ok, err := st.ResolveModelAlias("fast"); err != nil || !ok || got != "glm-4.6" {
		t.Fatalf("升级后解析别名 = (%q, %v, %v)，期望 (\"glm-4.6\", true, nil)", got, ok, err)
	}
}

// TestSetModelAliasOverwrites 钉住覆盖语义：同一个别名再写一次就是改目标，不报错、不留旧行，
// created_at 仍是第一次写进来的那一刻（覆盖改的是目标，不是这条别名的出生时间）。
func TestSetModelAliasOverwrites(t *testing.T) {
	st := openTemp(t)

	if err := st.SetModelAlias("fast", "glm-4.6"); err != nil {
		t.Fatalf("写别名: %v", err)
	}
	first, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("读别名: %v", err)
	}
	if len(first) != 1 || first[0].Target != "glm-4.6" {
		t.Fatalf("第一版别名 = %+v", first)
	}

	if err := st.SetModelAlias("fast", "gemini-3.8-flash"); err != nil {
		t.Fatalf("覆盖别名: %v", err)
	}
	got, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("覆盖后读别名: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("覆盖之后应只有一行，实际 %+v", got)
	}
	if got[0].Target != "gemini-3.8-flash" {
		t.Fatalf("覆盖之后目标 = %q，期望 gemini-3.8-flash", got[0].Target)
	}
	if got[0].CreatedAt != first[0].CreatedAt {
		t.Fatalf("覆盖改了 created_at：%d → %d", first[0].CreatedAt, got[0].CreatedAt)
	}
	if got[0].CreatedAt == 0 {
		t.Fatal("created_at 应写成本次写入的 Unix 秒，不是 0")
	}
	if name, ok, err := st.ResolveModelAlias("fast"); err != nil || !ok || name != "gemini-3.8-flash" {
		t.Fatalf("覆盖后解析 = (%q, %v, %v)", name, ok, err)
	}
}

// TestSetModelAliasValidation 钉住写入校验：空白、控制字符、别名等于目标
// （含去空白之后相等）、与账号真实模型名同名，各自报错；写法合规的一条都不拦。
func TestSetModelAliasValidation(t *testing.T) {
	st := openTemp(t)
	newAliasAccount(t, st, "ds1", "glm-4.6", "GLM-4.6", "deepseek-chat")

	cases := []struct {
		name    string
		alias   string
		target  string
		wantSub string
		// wantShadow 表示这条应是「遮蔽真名」，要靠哨兵错误分辨，不能只看文案。
		wantShadow bool
	}{
		{"空别名", "", "glm-4.6", "别名不能为空", false},
		{"空白别名", "   ", "glm-4.6", "别名不能为空", false},
		{"制表符别名", "\t\n", "glm-4.6", "别名不能为空", false},
		{"空目标", "fast", "", "目标不能为空", false},
		{"空白目标", "fast", "  ", "目标不能为空", false},
		{"别名含控制字符", "fa\x00st", "glm-4.6", "控制字符", false},
		{"别名中间带换行", "fa\nst", "glm-4.6", "控制字符", false},
		{"目标含控制字符", "fast", "glm\x1b-4.6", "控制字符", false},
		{"别名等于目标", "glm-4.6", "glm-4.6", "与目标相同", false},
		{"去空白之后相等", "  glm-4.6 ", "glm-4.6", "与目标相同", false},
		{"遮蔽账号真实模型名", "deepseek-chat", "glm-4.6", "别名会遮蔽它", true},
		{"遮蔽判定区分大小写（大写同样撞上）", "GLM-4.6", "glm-4.6", "别名会遮蔽它", true},
	}
	for _, c := range cases {
		err := st.SetModelAlias(c.alias, c.target)
		if err == nil {
			t.Fatalf("%s：SetModelAlias(%q, %q) 应报错，实际通过", c.name, c.alias, c.target)
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Fatalf("%s：错误文案 %q 里没有 %q", c.name, err.Error(), c.wantSub)
		}
		if got := errors.Is(err, store.ErrModelAliasShadow); got != c.wantShadow {
			t.Fatalf("%s：errors.Is(ErrModelAliasShadow) = %v，期望 %v（%v）", c.name, got, c.wantShadow, err)
		}
	}
	if aliases, err := st.ModelAliases(); err != nil {
		t.Fatalf("读别名: %v", err)
	} else if len(aliases) != 0 {
		t.Fatalf("校验失败不该留下任何一行，实际 %+v", aliases)
	}

	// 大小写不同就不是同一个名字（精确匹配），可以写成别名。
	if err := st.SetModelAlias("glm-4.6-latest", "GLM-4.6"); err != nil {
		t.Fatalf("大小写不同的别名不该被拒: %v", err)
	}
	// 首尾空白去掉之后落库，读回来与解析都用去空白后的名字。
	if err := st.SetModelAlias("  fast  ", "  glm-4.6  "); err != nil {
		t.Fatalf("首尾空白的别名应被接受并去空白: %v", err)
	}
	if got, ok, err := st.ResolveModelAlias("fast"); err != nil || !ok || got != "glm-4.6" {
		t.Fatalf("去空白后的别名解析 = (%q, %v, %v)", got, ok, err)
	}
	if _, ok, err := st.ResolveModelAlias("  fast  "); err != nil || ok {
		t.Fatalf("落库的是去空白后的名字，带空白的写法不该命中（ok=%v, err=%v）", ok, err)
	}
	// 目标可以暂时没有账号提供，写进去不报错。
	if err := st.SetModelAlias("later", "no-account-yet"); err != nil {
		t.Fatalf("没有账号提供的目标应可写入: %v", err)
	}
}

// TestSetModelAliasShadowCatalogOnly 钉住目录那一段检查：目录里的名字是真名，
// 眼下没有账号能用它也不算——账号一恢复可用，这个别名的行为就会突然改变。
// 文案要写出是哪家厂商的目录：用户去账号里找一个没人声明的名字是找不到的。
func TestSetModelAliasShadowCatalogOnly(t *testing.T) {
	st := openTemp(t)

	// 目录里有、账号不可用：账号的行被探活判成 forbidden，目录那一行还在。
	acc := newAliasAccount(t, st, "ds1", "glm-4.7")
	if err := st.SetAccountModelAccess(acc.ID, "glm-4.7", store.AccessForbidden, 403, "无权访问", 100); err != nil {
		t.Fatalf("把 glm-4.7 判成无权访问: %v", err)
	}
	err := st.SetModelAlias("glm-4.7", "glm-4.6")
	if !errors.Is(err, store.ErrModelAliasShadow) {
		t.Fatalf("目录里有、账号不可用的名字应报 ErrModelAliasShadow，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "deepseek") {
		t.Fatalf("冲突文案要写出是哪个厂商的目录，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "目录") || !strings.Contains(err.Error(), "眼下没有账号") {
		t.Fatalf("账号不可用时文案要说明眼下没人能用，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "ds1") {
		t.Fatalf("这个账号现在用不了该模型，不该被当成声明方列出来：%q", err.Error())
	}

	// 目录里有一条、一个账号都没有的厂商同样算真名。
	if err := st.UpsertProviderModels("moonshot", []store.ProviderModel{{Model: "kimi-k2", Source: store.SourceSync}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if err := st.SetModelAlias("kimi-k2", "glm-4.6"); !errors.Is(err, store.ErrModelAliasShadow) {
		t.Fatalf("没有账号的目录行也应是真名，实际 %v", err)
	}

	// 同名模型出现在多家厂商的目录里时，厂商名最多列三个，其余只报个数。
	for _, provider := range []string{"aliyun", "dashscope", "openrouter", "volcengine"} {
		if err := st.UpsertProviderModels(provider, []store.ProviderModel{{Model: "qwen-max", Source: store.SourceSync}}); err != nil {
			t.Fatalf("写 %s 的目录: %v", provider, err)
		}
	}
	err = st.SetModelAlias("qwen-max", "glm-4.6")
	if !errors.Is(err, store.ErrModelAliasShadow) {
		t.Fatalf("应报 ErrModelAliasShadow，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "等 4 家厂商") {
		t.Fatalf("四家厂商时只报个数，文案里要有「等 4 家厂商」，实际 %q", err.Error())
	}
	for _, provider := range []string{"aliyun", "dashscope", "openrouter"} {
		if !strings.Contains(err.Error(), provider) {
			t.Fatalf("前三个厂商名要列出来（缺 %s），实际 %q", provider, err.Error())
		}
	}
	if strings.Contains(err.Error(), "volcengine") {
		t.Fatalf("第四个厂商名只该计入个数，不该列出来：%q", err.Error())
	}

	if aliases, err := st.ModelAliases(); err != nil {
		t.Fatalf("读别名: %v", err)
	} else if len(aliases) != 0 {
		t.Fatalf("冲突不该留下任何一行，实际 %+v", aliases)
	}
}

// TestSetModelAliasShadowReportsBothOnce 钉住「两处都命中时只报一次」：目录与账号各命中一次，
// 合成一句话里既有厂商名又有账号名，而不是两条拼起来的错误。
func TestSetModelAliasShadowReportsBothOnce(t *testing.T) {
	st := openTemp(t)
	newAliasAccount(t, st, "ds1", "glm-4.6")

	err := st.SetModelAlias("glm-4.6", "deepseek-chat")
	if !errors.Is(err, store.ErrModelAliasShadow) {
		t.Fatalf("应报 ErrModelAliasShadow，实际 %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "deepseek") || !strings.Contains(msg, "ds1") {
		t.Fatalf("文案要同时写出厂商（deepseek）与账号（ds1），实际 %q", msg)
	}
	if n := strings.Count(msg, store.ErrModelAliasShadow.Error()); n != 1 {
		t.Fatalf("冲突只该报一次，哨兵文案出现了 %d 次：%q", n, msg)
	}
	if n := strings.Count(msg, "换个别名"); n != 1 {
		t.Fatalf("建议只该出现一次，实际 %d 次：%q", n, msg)
	}
}

// TestSetModelAliasNoShadowWrites 是上两个用例的反面：目录与账号都不命中时正常写入，
// 名字相近（前缀、后缀）也不算同名，照旧按精确匹配处理。
func TestSetModelAliasNoShadowWrites(t *testing.T) {
	st := openTemp(t)
	newAliasAccount(t, st, "ds1", "glm-4.6", "deepseek-chat")
	if err := st.UpsertProviderModels("moonshot", []store.ProviderModel{{Model: "kimi-k2", Source: store.SourceSync}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}

	for _, pair := range [][2]string{
		{"fast", "glm-4.6"},
		{"kimi-k2-latest", "kimi-k2"},
		{"glm", "glm-4.6"},
	} {
		if err := st.SetModelAlias(pair[0], pair[1]); err != nil {
			t.Fatalf("目录与账号都不命中的别名 %s 应可写入: %v", pair[0], err)
		}
	}
	aliases, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("读别名: %v", err)
	}
	if len(aliases) != 3 {
		t.Fatalf("应写入三行别名，实际 %+v", aliases)
	}
	if got, ok, err := st.ResolveModelAlias("kimi-k2-latest"); err != nil || !ok || got != "kimi-k2" {
		t.Fatalf("解析 = (%q, %v, %v)，期望 (\"kimi-k2\", true, nil)", got, ok, err)
	}
}

// TestResolveModelAliasChain 钉住链解析：一跳、多跳、8 跳上限、9 跳报错、成环报错（都带上链上的名字），
// 以及「不是别名」时返回空名字与 false。
func TestResolveModelAliasChain(t *testing.T) {
	st := openTemp(t)

	if got, ok, err := st.ResolveModelAlias("glm-4.6"); err != nil || ok || got != "" {
		t.Fatalf("不是别名时应返回 (\"\", false, nil)，实际 (%q, %v, %v)", got, ok, err)
	}

	if err := st.SetModelAlias("fast", "glm-4.6"); err != nil {
		t.Fatalf("写一跳别名: %v", err)
	}
	if got, ok, err := st.ResolveModelAlias("fast"); err != nil || !ok || got != "glm-4.6" {
		t.Fatalf("一跳解析 = (%q, %v, %v)，期望 (\"glm-4.6\", true, nil)", got, ok, err)
	}

	// 多跳：客户端写死的名字 → 项目里的中间名 → 上游真名。
	for _, pair := range [][2]string{
		{"claude-3-5-sonnet", "sonnet"},
		{"sonnet", "my-sonnet"},
		{"my-sonnet", "glm-4.6"},
	} {
		if err := st.SetModelAlias(pair[0], pair[1]); err != nil {
			t.Fatalf("写链上的 %s → %s: %v", pair[0], pair[1], err)
		}
	}
	if got, ok, err := st.ResolveModelAlias("claude-3-5-sonnet"); err != nil || !ok || got != "glm-4.6" {
		t.Fatalf("多跳解析 = (%q, %v, %v)，期望 (\"glm-4.6\", true, nil)", got, ok, err)
	}

	// 8 跳是上限，刚好走满要能解析出来。
	writeAliasChain(t, st, "h", 8, "real-model")
	if got, ok, err := st.ResolveModelAlias("h0"); err != nil || !ok || got != "real-model" {
		t.Fatalf("8 跳解析 = (%q, %v, %v)，期望 (\"real-model\", true, nil)", got, ok, err)
	}

	// 成环：报错并把环上的名字按顺序写出来。
	for _, pair := range [][2]string{{"b1", "b2"}, {"b2", "b3"}, {"b3", "b1"}} {
		if err := st.SetModelAlias(pair[0], pair[1]); err != nil {
			t.Fatalf("写成环的 %s → %s: %v", pair[0], pair[1], err)
		}
	}
	_, ok, err := st.ResolveModelAlias("b1")
	if !errors.Is(err, store.ErrModelAliasCycle) {
		t.Fatalf("成环应报 ErrModelAliasCycle，实际 (%v, %v)", ok, err)
	}
	if !strings.Contains(err.Error(), "b1 → b2 → b3 → b1") {
		t.Fatalf("成环的错误文案要写出环上的名字，实际 %q", err.Error())
	}

	// 9 跳：超长报错，文案里带上是哪一段链。
	writeAliasChain(t, st, "t", 9, "real-model")
	if _, ok, err := st.ResolveModelAlias("t0"); !errors.Is(err, store.ErrModelAliasTooDeep) {
		t.Fatalf("9 跳应报 ErrModelAliasTooDeep，实际 (%v, %v)", ok, err)
	} else if !strings.Contains(err.Error(), "t0") || !strings.Contains(err.Error(), "t8") {
		t.Fatalf("超长的错误文案要写出走过的那段链，实际 %q", err.Error())
	}
}

// writeAliasChain 写一条 prefix0 → prefix1 → … → terminal 的链，一共 hops 跳
// （hops 个别名，最后一个直接指向 terminal）。
func writeAliasChain(t *testing.T, st *store.Store, prefix string, hops int, terminal string) {
	t.Helper()
	for i := 0; i < hops; i++ {
		target := terminal
		if i < hops-1 {
			target = fmt.Sprintf("%s%d", prefix, i+1)
		}
		if err := st.SetModelAlias(fmt.Sprintf("%s%d", prefix, i), target); err != nil {
			t.Fatalf("写链上第 %d 跳（%s%d）: %v", i+1, prefix, i, err)
		}
	}
}

// TestDeleteModelAlias 钉住删除：删得到就没了（再解析就不是别名），删不到报 ErrModelAliasNotFound，
// 不静默当成功。
func TestDeleteModelAlias(t *testing.T) {
	st := openTemp(t)

	if err := st.SetModelAlias("fast", "glm-4.6"); err != nil {
		t.Fatalf("写别名: %v", err)
	}
	if err := st.DeleteModelAlias("fast"); err != nil {
		t.Fatalf("删别名: %v", err)
	}
	if aliases, err := st.ModelAliases(); err != nil {
		t.Fatalf("删后读别名: %v", err)
	} else if len(aliases) != 0 {
		t.Fatalf("删后应为空，实际 %+v", aliases)
	}
	if _, ok, err := st.ResolveModelAlias("fast"); err != nil || ok {
		t.Fatalf("删掉之后不该再解析成别名（ok=%v, err=%v）", ok, err)
	}

	err := st.DeleteModelAlias("fast")
	if !errors.Is(err, store.ErrModelAliasNotFound) {
		t.Fatalf("删不存在的别名应报 ErrModelAliasNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "fast") {
		t.Fatalf("删不到的错误文案要写清是哪个别名，实际 %q", err.Error())
	}
}

// TestModelAliasesOrdered 钉住读出的顺序：按别名升序（SQLite 的 TEXT 是二进制序，
// 大写排在小写前），命令行与 /v1/models 都照这个顺序展示。
func TestModelAliasesOrdered(t *testing.T) {
	st := openTemp(t)

	for _, pair := range [][2]string{
		{"b-alias", "glm-4.6"},
		{"a-alias", "glm-4.6"},
		{"C-alias", "glm-4.6"},
	} {
		if err := st.SetModelAlias(pair[0], pair[1]); err != nil {
			t.Fatalf("写别名 %s: %v", pair[0], err)
		}
	}
	got, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("读别名: %v", err)
	}
	want := []string{"C-alias", "a-alias", "b-alias"}
	if len(got) != len(want) {
		t.Fatalf("读出 %d 行，期望 %d 行", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Alias != name {
			t.Fatalf("第 %d 行是 %q，期望 %q（按别名升序）", i, got[i].Alias, name)
		}
	}
}

// TestModelAliasKeepsCredentialsOut 确认别名这条路上没有凭据：冲突与成环的错误文案、
// ModelAlias 的序列化输出、以及库里别名表与整个库文件都不含账号的 key 明文。
func TestModelAliasKeepsCredentialsOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sb.db")
	key, err := secret.LoadOrCreateKey(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("建密钥: %v", err)
	}
	const plain = "sk-live-0123456789abcdef"
	cipher, err := secret.Encrypt(key, []byte(plain))
	if err != nil {
		t.Fatalf("加密 key: %v", err)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer st.Close()

	acc := store.Account{
		Name:      "ds1",
		Provider:  "deepseek",
		Protocol:  "openai-chat",
		BaseURL:   "https://api.deepseek.com",
		KeyCipher: cipher,
		Models:    []string{"glm-4.6"},
		Enabled:   true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}

	// 冲突与成环是最容易把库里的东西顺手拼进文案的两条路。
	shadowErr := st.SetModelAlias("glm-4.6", "other")
	if !errors.Is(shadowErr, store.ErrModelAliasShadow) {
		t.Fatalf("应报 ErrModelAliasShadow，实际 %v", shadowErr)
	}
	for _, pair := range [][2]string{{"c1", "c2"}, {"c2", "c1"}} {
		if err := st.SetModelAlias(pair[0], pair[1]); err != nil {
			t.Fatalf("写环上的 %s → %s: %v", pair[0], pair[1], err)
		}
	}
	_, _, cycleErr := st.ResolveModelAlias("c1")
	if !errors.Is(cycleErr, store.ErrModelAliasCycle) {
		t.Fatalf("应报 ErrModelAliasCycle，实际 %v", cycleErr)
	}

	aliases, err := st.ModelAliases()
	if err != nil {
		t.Fatalf("读别名: %v", err)
	}
	js, err := json.Marshal(aliases)
	if err != nil {
		t.Fatalf("序列化别名: %v", err)
	}
	writes := map[string]string{
		"冲突错误":   shadowErr.Error(),
		"成环错误":   cycleErr.Error(),
		"一行 %v":  fmt.Sprintf("%v", aliases[0]),
		"一行 %+v": fmt.Sprintf("%+v", aliases[0]),
		"JSON":   string(js),
	}
	for what, text := range writes {
		if strings.Contains(text, plain) {
			t.Fatalf("%s 里带出了 key 明文：%s", what, text)
		}
	}

	// 整份库文件（含 WAL）里也不该有明文：凭据只以密文落库。
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(path + suffix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("读库文件 %s%s: %v", path, suffix, err)
		}
		if strings.Contains(string(b), plain) {
			t.Fatalf("库文件 %s%s 里有 key 明文", path, suffix)
		}
	}
}
