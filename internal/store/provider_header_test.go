package store_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"lax/internal/store"

	_ "modernc.org/sqlite"
)

// newHeaderProvider 建一家只有一条 chat 路由的厂商，附加头的用例都从它开始。
func newHeaderProvider(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if err := st.CreateProvider(
		store.Provider{Name: name, BaseURL: "https://" + name + ".example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商 %s: %v", name, err)
	}
}

// headerRows 数一家厂商的附加头行数，用来确认「拒绝写入时一行都没落」。
func headerRows(t *testing.T, st *store.Store, provider string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM provider_headers WHERE provider = ?`, provider).Scan(&n); err != nil {
		t.Fatalf("数附加头行: %v", err)
	}
	return n
}

// TestProviderHeaderTableFromEmptyDatabase 确认空库一路跑到最新之后表在、行数为零、
// 外键是真级联，且连接上 foreign_keys 是开着的——级联靠它才成立。
func TestProviderHeaderTableFromEmptyDatabase(t *testing.T) {
	st := openTemp(t)
	assertLatestVersion(t, st)

	var tables int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'provider_headers'`).Scan(&tables); err != nil {
		t.Fatalf("查 sqlite_master: %v", err)
	}
	if tables != 1 {
		t.Fatalf("迁移之后没有 provider_headers 表（查到 %d 个同名对象）", tables)
	}
	if n := headerRows(t, st, "deepseek"); n != 0 {
		t.Fatalf("新库的附加头行数 = %d，期望 0", n)
	}

	var fk int
	if err := st.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("读 foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("连接上的 foreign_keys = %d，级联删除要靠它为 1", fk)
	}

	var (
		id, seq                                     int
		parent, from, to, onUpdate, onDelete, match string
	)
	if err := st.DB().QueryRow(`PRAGMA foreign_key_list(provider_headers)`).
		Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
		t.Fatalf("读外键: %v", err)
	}
	if parent != "providers" || from != "provider" || to != "name" {
		t.Fatalf("外键 = %s(%s) → %s(%s)，期望 provider → providers(name)", from, from, parent, to)
	}
	if onDelete != "CASCADE" {
		t.Fatalf("外键的 ON DELETE = %q，期望 CASCADE", onDelete)
	}

	// 头名归一成小写：查一行也得用小写查得到（主键认大小写，归一发生在写入口）。
	newHeaderProvider(t, st, "myco")
	if err := st.SetProviderHeader("myco", "openai-chat", "X-Opencode-Session", "{session}"); err != nil {
		t.Fatalf("声明附加头: %v", err)
	}
	got, err := st.ProviderHeaders("myco")
	if err != nil {
		t.Fatalf("读附加头: %v", err)
	}
	if len(got) != 1 || got[0].Name != "x-opencode-session" || got[0].Protocol != "openai-chat" {
		t.Fatalf("附加头 = %+v，期望头名归一成小写 x-opencode-session", got)
	}
}

// TestSetProviderHeaderNormalizesAndUpserts 确认同名再写一次是覆盖（值换新、行数不变），
// 头名大小写不敏感，厂商级与协议级是两行互不影响。
func TestSetProviderHeaderNormalizesAndUpserts(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	if err := st.SetProviderHeader("  myco  ", "", "X-API-Version", "2024-10-21"); err != nil {
		t.Fatalf("声明厂商级附加头: %v", err)
	}
	if err := st.SetProviderHeader("myco", "openai-chat", "x-api-version", "2025-01-01"); err != nil {
		t.Fatalf("声明协议级附加头: %v", err)
	}
	// 同一个键换大小写写第二遍：还是那一行，值被覆盖。
	if err := st.SetProviderHeader("myco", "", "x-api-version", "2024-12-01"); err != nil {
		t.Fatalf("覆盖厂商级附加头: %v", err)
	}

	rows, err := st.ProviderHeaders("myco")
	if err != nil {
		t.Fatalf("读附加头: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("附加头 = %+v，期望厂商级与协议级各一行", rows)
	}
	if rows[0].Protocol != "" || rows[0].Value != "2024-12-01" {
		t.Fatalf("厂商级那一行没被覆盖成新值: %+v", rows[0])
	}
	if rows[1].Protocol != "openai-chat" || rows[1].Value != "2025-01-01" {
		t.Fatalf("协议级那一行 = %+v，期望不受厂商级覆盖影响", rows[1])
	}
}

// TestSetProviderHeaderRejectsInvalidInput 逐个钉住拒绝条件：非法头名（空格、冒号、中文）、
// 空白值、带折行的值、非法协议，以及厂商不存在。每次拒绝之后都不留行。
func TestSetProviderHeaderRejectsInvalidInput(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	cases := []struct {
		desc     string
		provider string
		protocol string
		name     string
		value    string
		wantErr  error // 非空时按 errors.Is 比
		wantText string
	}{
		{desc: "头名带空格", provider: "myco", name: "x opencode session", value: "v", wantText: "非法字符"},
		{desc: "头名带冒号", provider: "myco", name: "x-opencode-session:", value: "v", wantText: "非法字符"},
		{desc: "头名是中文", provider: "myco", name: "会话标识", value: "v", wantText: "非法字符"},
		{desc: "头名是空串", provider: "myco", name: "   ", value: "v", wantText: "头名不能为空"},
		{desc: "值是空串", provider: "myco", name: "x-a", value: "", wantText: "头的值不能为空"},
		{desc: "值是空白", provider: "myco", name: "x-a", value: " \t ", wantText: "头的值不能为空"},
		{desc: "值带换行", provider: "myco", name: "x-a", value: "a\r\nx-injected: 1", wantText: "不能有回车"},
		{desc: "协议是命令行别名", provider: "myco", protocol: "openai", name: "x-a", value: "v", wantText: "不是线协议"},
		{desc: "协议不认识", provider: "myco", protocol: "chatgpt", name: "x-a", value: "v", wantText: "不是线协议"},
		{desc: "厂商不存在", provider: "nope", name: "x-a", value: "v", wantErr: store.ErrProviderNotFound},
	}
	for _, c := range cases {
		err := st.SetProviderHeader(c.provider, c.protocol, c.name, c.value)
		if err == nil {
			t.Fatalf("%s：应该报错，实际写进去了", c.desc)
		}
		if c.wantErr != nil && !errors.Is(err, c.wantErr) {
			t.Fatalf("%s：错误 = %v，期望 errors.Is(%v)", c.desc, err, c.wantErr)
		}
		if c.wantText != "" && !strings.Contains(err.Error(), c.wantText) {
			t.Fatalf("%s：错误文案 %q 里没有 %q", c.desc, err.Error(), c.wantText)
		}
	}
	for _, provider := range []string{"myco", "nope"} {
		if n := headerRows(t, st, provider); n != 0 {
			t.Fatalf("被拒绝的写入在 %s 上留下了 %d 行", provider, n)
		}
	}

	// 空厂商名与「一行都没声明」是另一类：前者是用法错误，后者读出来是空切片而不是错误。
	if err := st.SetProviderHeader("  ", "", "x-a", "v"); err == nil {
		t.Fatal("空厂商名应该报错")
	}
	rows, err := st.ProviderHeaders("myco")
	if err != nil {
		t.Fatalf("读附加头: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("一条都没声明时读出来应是空切片，实际 %+v", rows)
	}
	if rows == nil {
		t.Fatal("没声明时也该返回空切片而不是 nil，调用方才能直接 range")
	}
	if got, err := st.ProviderHeaders("nope"); err != nil || len(got) != 0 {
		t.Fatalf("厂商不存在时读附加头应是空切片且不报错，实际 %+v err=%v", got, err)
	}
}

// TestProviderHeadersSorted 确认一次全量读取按 protocol、name 升序，厂商级（空串）排在最前。
func TestProviderHeadersSorted(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	for _, h := range []struct{ protocol, name, value string }{
		{"openai-responses", "x-b", "3"},
		{"openai-chat", "x-z", "2"},
		{"", "x-m", "1"},
		{"openai-chat", "x-a", "0"},
	} {
		if err := st.SetProviderHeader("myco", h.protocol, h.name, h.value); err != nil {
			t.Fatalf("声明 %s/%s: %v", h.protocol, h.name, err)
		}
	}

	rows, err := st.ProviderHeaders("myco")
	if err != nil {
		t.Fatalf("读附加头: %v", err)
	}
	want := []store.ProviderHeader{
		{Protocol: "", Name: "x-m", Value: "1"},
		{Protocol: "openai-chat", Name: "x-a", Value: "0"},
		{Protocol: "openai-chat", Name: "x-z", Value: "2"},
		{Protocol: "openai-responses", Name: "x-b", Value: "3"},
	}
	if len(rows) != len(want) {
		t.Fatalf("附加头 = %+v，期望 %d 行", rows, len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("第 %d 行 = %+v，期望 %+v（按 protocol、name 升序）", i, rows[i], want[i])
		}
	}
}

// TestEffectiveProviderHeadersProtocolOverrides 是转发侧真正读的那一份：
// 协议级按同名覆盖厂商级，其它协议不受影响，删掉协议级那行就回退到厂商级。
func TestEffectiveProviderHeadersProtocolOverrides(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	if err := st.SetProviderHeader("myco", "", "x-opencode-session", "厂商级值"); err != nil {
		t.Fatalf("声明厂商级: %v", err)
	}
	if err := st.SetProviderHeader("myco", "", "api-version", "2024-10-21"); err != nil {
		t.Fatalf("声明厂商级第二个头: %v", err)
	}
	if err := st.SetProviderHeader("myco", "openai-chat", "X-Opencode-Session", "协议级值"); err != nil {
		t.Fatalf("声明协议级: %v", err)
	}
	if err := st.SetProviderHeader("myco", "openai-chat", "x-extra", "只给 chat"); err != nil {
		t.Fatalf("声明协议级独有头: %v", err)
	}

	got, err := st.EffectiveProviderHeaders("myco", "openai-chat")
	if err != nil {
		t.Fatalf("取有效附加头: %v", err)
	}
	want := []struct{ name, value, protocol string }{
		{"api-version", "2024-10-21", ""},
		{"x-extra", "只给 chat", "openai-chat"},
		{"x-opencode-session", "协议级值", "openai-chat"},
	}
	if len(got) != len(want) {
		t.Fatalf("chat 的有效附加头 = %+v，期望 %d 条", got, len(want))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Value != w.value || got[i].Protocol != w.protocol {
			t.Fatalf("第 %d 条 = %+v，期望 %+v（协议级覆盖厂商级、只留一条）", i, got[i], w)
		}
	}

	// 另一条协议只拿到厂商级的两条。
	other, err := st.EffectiveProviderHeaders("myco", "openai-responses")
	if err != nil {
		t.Fatalf("取 responses 的有效附加头: %v", err)
	}
	if len(other) != 2 || other[0].Name != "api-version" || other[1].Name != "x-opencode-session" ||
		other[1].Value != "厂商级值" {
		t.Fatalf("responses 的有效附加头 = %+v，期望只有厂商级的两条", other)
	}

	// 删掉协议级那行，chat 回退到厂商级的值。
	if err := st.DeleteProviderHeader("myco", "openai-chat", "x-opencode-session"); err != nil {
		t.Fatalf("删协议级: %v", err)
	}
	back, err := st.EffectiveProviderHeaders("myco", "openai-chat")
	if err != nil {
		t.Fatalf("再取有效附加头: %v", err)
	}
	if len(back) != 3 {
		t.Fatalf("删掉协议级之后 = %+v，期望三条（x-extra 还在，覆盖那行回退成厂商级）", back)
	}
	if back[2].Name != "x-opencode-session" || back[2].Value != "厂商级值" || back[2].Protocol != "" {
		t.Fatalf("回退后的那一行 = %+v，期望厂商级的原值", back[2])
	}

	// 协议取值同样要过白名单；厂商不存在时返回空切片且不报错。
	if _, err := st.EffectiveProviderHeaders("myco", "openai"); err == nil {
		t.Fatal("协议写成命令行别名 openai 时应报错，否则这条头永远匹配不上真实协议")
	}
	if empty, err := st.EffectiveProviderHeaders("nope", "openai-chat"); err != nil || len(empty) != 0 {
		t.Fatalf("厂商不存在时应是空切片且不报错，实际 %+v err=%v", empty, err)
	}
}

// TestDeleteProviderHeader 覆盖三种结果：删到了、这家厂商没这条头、厂商本身不存在。
func TestDeleteProviderHeader(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	if err := st.SetProviderHeader("myco", "openai-chat", "x-opencode-session", "{session}"); err != nil {
		t.Fatalf("声明附加头: %v", err)
	}
	// 大小写不敏感：用小写删掉大写的声明。
	if err := st.DeleteProviderHeader("myco", "openai-chat", "X-Opencode-Session"); err != nil {
		t.Fatalf("删附加头: %v", err)
	}
	if n := headerRows(t, st, "myco"); n != 0 {
		t.Fatalf("删完还剩 %d 行", n)
	}

	err := st.DeleteProviderHeader("myco", "openai-chat", "x-opencode-session")
	if !errors.Is(err, store.ErrProviderHeaderNotFound) {
		t.Fatalf("重复删除应报 ErrProviderHeaderNotFound，实际 %v", err)
	}
	// 协议写错也归到「没这条头」：那行确实不在这个作用范围里。
	if err := st.DeleteProviderHeader("myco", "anthropic", "x-opencode-session"); !errors.Is(err, store.ErrProviderHeaderNotFound) {
		t.Fatalf("协议写错应报 ErrProviderHeaderNotFound，实际 %v", err)
	}
	if err := st.DeleteProviderHeader("nope", "", "x-a"); !errors.Is(err, store.ErrProviderNotFound) {
		t.Fatalf("厂商不存在应报 ErrProviderNotFound，实际 %v", err)
	}
}

// TestDeleteProviderCascadesHeaders 确认删厂商时它的附加头跟着走：这张表有外键，
// 级联由 SQLite 做，DeleteProvider 不用手删（provider_routes 建于 0006、没有外键，才要手删）。
func TestDeleteProviderCascadesHeaders(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	if err := st.SetProviderHeader("myco", "", "api-version", "2024-10-21"); err != nil {
		t.Fatalf("声明厂商级: %v", err)
	}
	if err := st.SetProviderHeader("myco", "openai-chat", "x-opencode-session", "{session}"); err != nil {
		t.Fatalf("声明协议级: %v", err)
	}
	if n := headerRows(t, st, "myco"); n != 2 {
		t.Fatalf("删厂商之前应有 2 行附加头，实际 %d", n)
	}

	if err := st.DeleteProvider("myco"); err != nil {
		t.Fatalf("删厂商: %v", err)
	}
	var total int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM provider_headers`).Scan(&total); err != nil {
		t.Fatalf("数全部附加头: %v", err)
	}
	if total != 0 {
		t.Fatalf("删厂商后全表还剩 %d 行附加头，外键级联没生效", total)
	}
}

// TestMigration0010UpgradesOldDatabase 从 0009 升上来：迁移只加表，厂商、路由、账号一个字段不动，
// 新表为空且可用，升级上来的库上外键级联同样成立。
//
// 老库用 0001–0009 的迁移文件本身来建，不手抄 DDL：手抄的形状会随实现漂移，
// 而这里要验的正是真实老库升上来的结果。
func TestMigration0010UpgradesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	db := openDatabaseAt(t, path, 9)

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
	defer st.Close()

	assertLatestVersion(t, st)

	// 老数据原样保留。
	p, err := st.GetProvider("myco")
	if err != nil {
		t.Fatalf("读老厂商: %v", err)
	}
	if p.BaseURL != "https://my.example.com" || p.Note != "自建" || p.DefaultProtocol != "openai-chat" {
		t.Fatalf("老厂商被改动: %+v", p)
	}
	routes, err := st.ProviderRoutes("myco")
	if err != nil {
		t.Fatalf("读老路由: %v", err)
	}
	if len(routes) != 1 || routes[0].Path != "/v1/chat/completions" || routes[0].ModelsPath != "/v1/models" {
		t.Fatalf("老路由被改动: %+v", routes)
	}
	acc, err := st.GetAccount("mc1")
	if err != nil {
		t.Fatalf("读老账号: %v", err)
	}
	if acc.Provider != "myco" || len(acc.Models) != 1 || string(acc.KeyCipher) != "\x01\x02\x03" {
		t.Fatalf("老账号被改动: %+v", acc)
	}
	if n := headerRows(t, st, "myco"); n != 0 {
		t.Fatalf("升级之后附加头应为空，实际 %d 行", n)
	}

	// 新表在新库上可用，且级联在升级上来的库上同样生效。
	if err := st.SetProviderHeader("myco", "openai-chat", "x-opencode-session", "{session}"); err != nil {
		t.Fatalf("升级后声明附加头: %v", err)
	}
	if got, err := st.EffectiveProviderHeaders("myco", "openai-chat"); err != nil {
		t.Fatalf("升级后取有效附加头: %v", err)
	} else if len(got) != 1 || got[0].Name != "x-opencode-session" {
		t.Fatalf("升级后的有效附加头 = %+v", got)
	}
	if err := st.DeleteProvider("myco"); err != nil {
		t.Fatalf("升级后删厂商: %v", err)
	}
	var total int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM provider_headers`).Scan(&total); err != nil {
		t.Fatalf("数附加头: %v", err)
	}
	if total != 0 {
		t.Fatalf("升级上来的库上，删厂商之后还剩 %d 行附加头", total)
	}
}

// TestProviderHeaderValueNeverLeaks 钉住「明文只活在内存里」：错误文案、日志输出与 %v 之外的结构化
// 输出都不带头的值，日志里只出前四位加省略号。
func TestProviderHeaderValueNeverLeaks(t *testing.T) {
	st := openTemp(t)
	newHeaderProvider(t, st, "myco")

	const secretValue = "sk-live-9f8e7d6c5b4a"
	if err := st.SetProviderHeader("myco", "openai-chat", "x-api-key", secretValue); err != nil {
		t.Fatalf("声明附加头: %v", err)
	}

	// 错误文案：被拒绝的写入把值留在库里，报错里回显值等于把它写进日志与终端。
	err := st.SetProviderHeader("myco", "openai-chat", "x bad name: x", secretValue)
	if err == nil {
		t.Fatal("非法头名应该报错")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("错误文案里带出了头的明文: %v", err)
	}
	if settled, err := st.ProviderHeaders("myco"); err != nil || len(settled) != 1 || settled[0].Value != secretValue {
		t.Fatalf("被拒绝的写入动了已有的行: %+v err=%v", settled, err)
	}

	// 日志：slog 收到 ProviderHeader 时只打头名与截断后的值。
	rows, err := st.ProviderHeaders("myco")
	if err != nil {
		t.Fatalf("读附加头: %v", err)
	}
	h := rows[0]
	if got, want := h.Masked(), "sk-l…"; got != want {
		t.Fatalf("Masked() = %q，期望 %q（前四位加省略号）", got, want)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	// 单条与整片都打一遍：slog.Any 收到切片时按元素 fmt，单条上的 String 照样生效。
	logger.Info("转发附加头", "header", h, "headers", rows)
	if strings.Contains(buf.String(), secretValue) {
		t.Fatalf("日志里带出了头的明文: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "sk-l…") || !strings.Contains(buf.String(), "x-api-key") {
		t.Fatalf("日志里应出截断后的值与头名，实际 %s", buf.String())
	}
	if got, want := h.String(), "openai-chat/x-api-key=sk-l…"; got != want {
		t.Fatalf("String() = %q，期望 %q", got, want)
	}
	// 短值连前四位都不给，只留省略号。
	if got := (store.ProviderHeader{Name: "x-a", Value: "abc"}).Masked(); got != "…" {
		t.Fatalf("短值的 Masked() = %q，期望只剩省略号", got)
	}
}

// openDatabaseAt 用一个停在某个版本的库：把 migrations 目录里 ≤ version 的迁移文件照原样执行，
// 再把 PRAGMA user_version 设成 version。迁移文件本身是唯一事实来源，不手抄表结构。
func openDatabaseAt(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("读迁移目录: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	applied := 0
	for _, name := range names {
		var v int
		if _, err := fmt.Sscanf(name, "%04d_", &v); err != nil {
			t.Fatalf("迁移文件名 %q 不是 <四位序号>_<名字>.sql: %v", name, err)
		}
		if v > version {
			continue
		}
		b, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatalf("读迁移 %s: %v", name, err)
		}
		// 与 store.execScript 同一套拆语句规则：按分号切开逐条执行。
		for _, stmt := range strings.Split(string(b), ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("执行迁移 %s 的语句（%s）: %v", name, stmt, err)
			}
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("没有可执行的迁移文件（目标版本 %d）", version)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("设 user_version: %v", err)
	}
	return db
}
