//go:build integration

package store

// 三个数据库后端共用一套代码，但方言差异只有真库能验：占位符、UPSERT 语法、自增主键、
// 布尔与二进制列、日期分组函数、外键级联。
//
// 这些用例默认不编译（要 `-tags integration`），要连库时给出 DSN：
//
//	LAX_TEST_POSTGRES_DSN='host=127.0.0.1 port=55432 user=postgres password=verify dbname=lax sslmode=disable' \
//	LAX_TEST_MYSQL_DSN='root:verify@tcp(127.0.0.1:53306)/lax?charset=utf8mb4&parseTime=True&loc=Local' \
//	go test -tags integration ./internal/store/ -run TestBackends -v
//
// 两个变量都没给时整个用例跳过，`make check` 不受影响。SQLite 后端永远跑（不需要外部服务）。
//
// 用例结束时会 DROP 掉自己建的表：DSN 指向的库必须是一次性的，不要指向有数据的库。

import (
	"os"
	"testing"
	"time"

	"lax/internal/translate"
)

type backend struct {
	driver string
	dsn    string
}

// backends 列出这次要跑的后端。SQLite 用临时文件，另外两个看环境变量。
func backends(t *testing.T) []backend {
	t.Helper()
	list := []backend{{driver: DriverSQLite}}
	if dsn := os.Getenv("LAX_TEST_POSTGRES_DSN"); dsn != "" {
		list = append(list, backend{driver: DriverPostgres, dsn: dsn})
	}
	if dsn := os.Getenv("LAX_TEST_MYSQL_DSN"); dsn != "" {
		list = append(list, backend{driver: DriverMySQL, dsn: dsn})
	}
	return list
}

func TestBackends(t *testing.T) {
	ran := 0
	for _, b := range backends(t) {
		b := b
		name := b.driver
		t.Run(name, func(t *testing.T) {
			opts := Options{Driver: b.driver, DSN: b.dsn}
			if b.driver == DriverSQLite {
				opts.Path = t.TempDir() + "/lax.db"
			}
			st, err := OpenWith(opts)
			if err != nil {
				t.Fatalf("打开 %s: %v", name, err)
			}
			defer func() {
				// 清掉这次建的表，让下一次运行从空库开始。
				if err := st.gorm.Migrator().DropTable(tables()...); err != nil {
					t.Logf("清理 %s 的表: %v", name, err)
				}
				_ = st.Close()
			}()
			exerciseBackend(t, st)
			ran++
		})
	}
	if ran == 0 {
		t.Skip("没有可用的后端")
	}
}

// exerciseBackend 在给定后端上跑一遍写读全流程，覆盖那些只在方言上有差异的地方。
func exerciseBackend(t *testing.T, st *Store) {
	t.Helper()
	now := time.Now().Unix()

	// 1. 种子：建库时写入了 9 家厂商与 14 条路由。
	providers, err := st.ListProviders()
	if err != nil {
		t.Fatalf("列厂商: %v", err)
	}
	if len(providers) != len(seedProviders) {
		t.Fatalf("种子厂商数 = %d，期望 %d", len(providers), len(seedProviders))
	}
	route, ok, err := st.LookupProviderRoute("deepseek", translate.ProtocolChat)
	if err != nil || !ok {
		t.Fatalf("读 deepseek 的 chat 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "/v1/chat/completions" || route.ModelsPath != "/v1/models" {
		t.Fatalf("deepseek 的 chat 路由 = %+v", route)
	}

	// 2. 账号：自增主键回填、布尔列、二进制列、可空列、LEFT JOIN 派生地址。
	acc := Account{
		Name:      "ds1",
		Provider:  "deepseek",
		KeyCipher: []byte{1, 2, 3, 4},
		Priority:  10,
		Enabled:   true,
		Models:    []string{"m1", "m2"},
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if acc.ID == 0 {
		t.Fatal("CreateAccount 没有回填 ID")
	}
	got, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !got.Enabled || got.Priority != 10 || len(got.Models) != 2 {
		t.Fatalf("读回来的账号不对: %+v", got)
	}
	if got.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("派生地址 = %q，期望厂商行上的地址", got.BaseURL)
	}
	if got.Protocol != translate.ProtocolChat {
		t.Fatalf("派生协议 = %q，期望 openai-chat", got.Protocol)
	}
	if string(got.KeyCipher) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("二进制列读回来变了: %v", got.KeyCipher)
	}

	// 3. 可达性：复合主键 UPSERT，先插后更新。
	if err := st.SetAccountModelAccess(acc.ID, "m1", AccessAllowed, 200, "", now); err != nil {
		t.Fatalf("写可达性: %v", err)
	}
	if err := st.SetAccountModelAccess(acc.ID, "m1", AccessForbidden, 403, "forbidden", now+1); err != nil {
		t.Fatalf("再写可达性: %v", err)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达性: %v", err)
	}
	var m1 *AccountModel
	for i := range rows {
		if rows[i].Model == "m1" {
			m1 = &rows[i]
		}
	}
	if m1 == nil {
		t.Fatal("没有 m1 的可达性行")
	}
	if m1.Access != AccessForbidden || m1.HTTPStatus != 403 || m1.Error != "forbidden" {
		t.Fatalf("UPSERT 之后的行 = %+v", m1)
	}

	// 4. 协议覆盖与账号级价：COALESCE 表达式只在传了值时才改。
	if err := st.SetAccountModelProtocol(acc.ID, "m1", translate.ProtocolAnthropic); err != nil {
		t.Fatalf("写协议: %v", err)
	}
	if got, err := st.ModelProtocol(acc.ID, "m1"); err != nil || got != translate.ProtocolAnthropic {
		t.Fatalf("读协议 = %q err=%v", got, err)
	}

	// 5. 厂商写入口：新建、增量改、外键级联删除。
	custom := Provider{Name: "myco", BaseURL: "https://my.example.com", DefaultProtocol: translate.ProtocolChat}
	routes := []ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}
	if err := st.CreateProvider(custom, routes); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	note := "自建"
	if err := st.SetProvider("myco", ProviderPatch{Note: &note}); err != nil {
		t.Fatalf("改厂商: %v", err)
	}
	if err := st.SetProviderHeader("myco", "", "X-Extra", "1"); err != nil {
		t.Fatalf("写附加头: %v", err)
	}
	if err := st.DeleteProvider("myco"); err != nil {
		t.Fatalf("删厂商: %v", err)
	}
	if headers, err := st.ProviderHeaders("myco"); err != nil {
		t.Fatalf("读附加头: %v", err)
	} else if len(headers) != 0 {
		t.Fatalf("厂商删掉之后附加头还在: %+v", headers)
	}

	// 6. 请求记录：自增主键、可空列、倒序读。
	rec := RequestRecord{
		TS:               now,
		Inbound:          translate.ProtocolAnthropic,
		Outbound:         translate.ProtocolChat,
		ModelRequested:   "m1",
		ModelUsed:        "m1",
		AccountID:        acc.ID,
		HTTPStatus:       200,
		OK:               true,
		InTokens:         100,
		InTokensUncached: 100,
		OutTokens:        20,
		LatencyMS:        50,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}
	if rec.ID == 0 {
		t.Fatal("InsertRequest 没有回填 ID")
	}
	recs, err := st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK || recs[0].InTokens != 100 {
		t.Fatalf("读回来的请求记录 = %+v", recs)
	}

	// 7. 用量汇总：三种分组各跑一次。by day 走按后端分支的日期函数，
	// 花费那一列走 CASE WHEN 与 COALESCE 聚合。
	if err := st.SetProviderModelPrices("deepseek", "m1", ModelPrices{
		In: floatPtr(2), Out: floatPtr(4),
	}); err != nil {
		t.Fatalf("写目录价: %v", err)
	}
	for _, dim := range []string{"account", "model", "day"} {
		usage, err := st.Usage(dim, 0)
		if err != nil {
			t.Fatalf("按 %s 汇总: %v", dim, err)
		}
		if len(usage) != 1 {
			t.Fatalf("按 %s 汇总得到 %d 行，期望 1", dim, len(usage))
		}
		if usage[0].Requests != 1 || usage[0].InTokens != 100 || usage[0].OutTokens != 20 {
			t.Fatalf("按 %s 汇总的用量 = %+v", dim, usage[0])
		}
		if usage[0].Cost == nil {
			t.Fatalf("按 %s 汇总的花费是 nil，期望算出了金额", dim)
		}
	}

	// 8. 别名：UPSERT 覆盖目标，删除按主键。
	if err := st.SetModelAlias("fast", "m1"); err != nil {
		t.Fatalf("写别名: %v", err)
	}
	if err := st.SetModelAlias("fast", "m2"); err != nil {
		t.Fatalf("覆盖别名: %v", err)
	}
	if target, ok, err := st.ResolveModelAlias("fast"); err != nil || !ok || target != "m2" {
		t.Fatalf("解析别名 = %q ok=%v err=%v，期望 m2", target, ok, err)
	}
	if err := st.DeleteModelAlias("fast"); err != nil {
		t.Fatalf("删别名: %v", err)
	}
}

func floatPtr(v float64) *float64 { return &v }
