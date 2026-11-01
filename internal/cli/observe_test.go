package cli_test

import (
	"strings"
	"testing"
	"time"

	"lax/internal/store"
	"lax/internal/store/storetest"
)

// addRecord 往库里写一行请求记录，返回时带上自动生成的行 id。
// 记录走 store.InsertRequest，不绕开 cli 不直接连库这条边界（见 storetest 的说明）。
func addRecord(t *testing.T, rec store.RequestRecord) {
	t.Helper()
	db := storetest.Open(t)
	if rec.TS == 0 {
		rec.TS = time.Now().Unix()
	}
	if err := db.InsertRequest(&rec); err != nil {
		t.Fatalf("写请求记录: %v", err)
	}
}

// accountID 取账号 id，记录里的账号归属要用它。
func accountID(t *testing.T, name string) int64 {
	t.Helper()
	db := storetest.Open(t)
	acc, err := db.GetAccount(name)
	if err != nil {
		t.Fatalf("读账号 %s: %v", name, err)
	}
	return acc.ID
}

// addPricedAccount 加一个账号并给它的模型定好价，用来断言估算花费的具体金额。
// 单价在模型目录上，所以定价是第二步 model price set，不再走 account add。
func addPricedAccount(t *testing.T, name, priceIn, priceOut string) {
	t.Helper()
	if code, _, stderr := addAccount(t, name, "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add %s 退出码 = %d，stderr = %s", name, code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine", "--in", priceIn, "--out", priceOut); code != 0 {
		t.Fatalf("model price set 退出码 = %d，stderr = %s", code, stderr)
	}
}

// usageRowJSON 是 usage --json 的一行。
type usageRowJSON struct {
	Key             string  `json:"key"`
	Requests        int64   `json:"requests"`
	InTokens        int64   `json:"in_tokens"`
	OutTokens       int64   `json:"out_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	Cost            float64 `json:"cost"`
}

// logRowJSON 是 log tail --json 的一行，只取用例关心的字段。
type logRowJSON struct {
	Model           string `json:"model"`
	InTokens        int64  `json:"in_tokens"`
	OutTokens       int64  `json:"out_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CacheRead       int64  `json:"cache_read"`
	CacheWrite      int64  `json:"cache_write"`
}

// mustUsageRows 读 usage --json 并按 key 索引。
func mustUsageRows(t *testing.T, args ...string) map[string]usageRowJSON {
	t.Helper()
	code, out, stderr := run(t, append([]string{"usage", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("usage %v 退出码 = %d，stderr = %s", args, code, stderr)
	}
	var rows []usageRowJSON
	mustJSON(t, out, &rows)
	byKey := make(map[string]usageRowJSON, len(rows))
	for _, r := range rows {
		byKey[r.Key] = r
	}
	return byKey
}

// mustLogRows 读 log tail --json 并按模型名索引。
func mustLogRows(t *testing.T, args ...string) map[string]logRowJSON {
	t.Helper()
	code, out, stderr := run(t, append([]string{"log", "tail", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("log tail %v 退出码 = %d，stderr = %s", args, code, stderr)
	}
	var rows []logRowJSON
	mustJSON(t, out, &rows)
	byModel := make(map[string]logRowJSON, len(rows))
	for _, r := range rows {
		byModel[r.Model] = r
	}
	return byModel
}

// tableRowFields 找出以第一个字段开头的表格行，按空白切成字段。
func tableRowFields(t *testing.T, out, first string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == first {
			return fields
		}
	}
	t.Fatalf("表格里没有以 %q 开头的行:\n%s", first, out)
	return nil
}

// logRowFields 找出模型列等于 model 的表格行，按空白切成字段。
func logRowFields(t *testing.T, out, model string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 3 && fields[3] == model {
			return fields
		}
	}
	t.Fatalf("表格里没有模型列为 %q 的行:\n%s", model, out)
	return nil
}

// TestLogTailShowsCacheTokens 覆盖 log tail 的两个缓存列：
// 文本表格在输出之后多出「缓存读」「缓存写」，--json 多出 cache_read 与 cache_write，
// 有值与非 0 的两种记录都按原数显示，0 显示 0（与输入输出 token 的写法一致）。
func TestLogTailShowsCacheTokens(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m-cache,m-plain"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	// 先写带缓存的那条，再写全 0 的那条：log tail 按时间倒序，全 0 的排在前面。
	addRecord(t, store.RequestRecord{
		TS: time.Now().Add(-time.Minute).Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m-cache", ModelUsed: "m-cache", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 100, OutTokens: 20,
		CacheRead: 1500, CacheWrite: 300, LatencyMS: 800,
	})
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "openai-chat",
		ModelRequested: "m-plain", ModelUsed: "m-plain", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 40, OutTokens: 5, LatencyMS: 120,
	})

	code, out, stderr := run(t, "log", "tail", "-n", "5")
	if code != 0 {
		t.Fatalf("log tail 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"输入", "输出", "思考", "缓存读", "缓存写", "耗时", "错误"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log tail 的表头缺 %q:\n%s", want, out)
		}
	}
	// 时间占两列（日期 + 时刻），后面依次是协议、模型、账号、状态、输入、输出、思考、缓存读、缓存写、耗时。
	cached := logRowFields(t, out, "m-cache")
	if len(cached) < 12 {
		t.Fatalf("带缓存的记录列数 = %d，少于 12 列：%v", len(cached), cached)
	}
	for i, want := range map[int]string{6: "100", 7: "20", 8: "0", 9: "1500", 10: "300"} {
		if cached[i] != want {
			t.Fatalf("带缓存的记录第 %d 列 = %q，期望 %q：%v", i, cached[i], want, cached)
		}
	}
	plain := logRowFields(t, out, "m-plain")
	if len(plain) < 12 {
		t.Fatalf("全 0 的记录列数 = %d，少于 12 列：%v", len(plain), plain)
	}
	if plain[8] != "0" || plain[9] != "0" || plain[10] != "0" {
		t.Fatalf("全 0 的记录思考与缓存列 = %q / %q / %q，期望都显示 0：%v",
			plain[8], plain[9], plain[10], plain)
	}

	byModel := mustLogRows(t)
	if got := byModel["m-cache"]; got.CacheRead != 1500 || got.CacheWrite != 300 {
		t.Fatalf("--json 的 m-cache = %+v，期望 cache_read 1500、cache_write 300", got)
	}
	if got := byModel["m-cache"]; got.InTokens != 100 || got.OutTokens != 20 {
		t.Fatalf("--json 的 m-cache 输入输出 = %+v，期望 100 / 20", got)
	}
	if got := byModel["m-plain"]; got.CacheRead != 0 || got.CacheWrite != 0 {
		t.Fatalf("--json 的 m-plain = %+v，期望两个缓存字段都是 0", got)
	}
}

// TestUsageCacheTotalsPerDimension 覆盖 usage 三种分组下的缓存合计：
// 每个分组都有 cache_read 与 cache_write，文本表格有这两列与合计行。
func TestUsageCacheTotalsPerDimension(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m-a,m-b"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	addRecord(t, store.RequestRecord{
		TS: time.Now().Add(-time.Hour).Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m-a", ModelUsed: "m-a", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 100, OutTokens: 20,
		CacheRead: 1000, CacheWrite: 200, LatencyMS: 300,
	})
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m-b", ModelUsed: "m-b", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 50, OutTokens: 10,
		CacheRead: 500, CacheWrite: 100, LatencyMS: 200,
	})

	// 按账号：一行，含两条记录的缓存合计。
	byAccount := mustUsageRows(t, "--by", "account", "--since", "1d")
	acc := byAccount["ds1"]
	if acc.CacheRead != 1500 || acc.CacheWrite != 300 {
		t.Fatalf("按账号的缓存合计 = %d / %d，期望 1500 / 300", acc.CacheRead, acc.CacheWrite)
	}
	if acc.InTokens != 150 || acc.OutTokens != 30 || acc.Requests != 2 {
		t.Fatalf("按账号的输入输出与请求数 = %+v", acc)
	}

	// 按模型：每个模型各一行，各自的缓存数。
	byModel := mustUsageRows(t, "--by", "model", "--since", "1d")
	if got := byModel["m-a"]; got.CacheRead != 1000 || got.CacheWrite != 200 {
		t.Fatalf("按模型的 m-a = %+v，期望 1000 / 200", got)
	}
	if got := byModel["m-b"]; got.CacheRead != 500 || got.CacheWrite != 100 {
		t.Fatalf("按模型的 m-b = %+v，期望 500 / 100", got)
	}

	// 按日期：两条记录同一天，合成一行，合计与按账号一致。
	byDay := mustUsageRows(t, "--by", "day", "--since", "1d")
	if len(byDay) != 1 {
		t.Fatalf("按日期应有 1 行，实际 %d 行：%+v", len(byDay), byDay)
	}
	for key, row := range byDay {
		if len(key) != 10 {
			t.Fatalf("日期分组的键 = %q，期望 yyyy-mm-dd", key)
		}
		if row.CacheRead != 1500 || row.CacheWrite != 300 {
			t.Fatalf("按日期的缓存合计 = %d / %d，期望 1500 / 300", row.CacheRead, row.CacheWrite)
		}
	}

	// 文本表格：思考、两列缓存与合计行都要在。
	code, out, stderr := run(t, "usage", "--by", "account", "--since", "1d")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"输入 token", "输出 token", "思考 token", "缓存读 token", "缓存写 token", "估算花费", "合计"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage 表格缺 %q:\n%s", want, out)
		}
	}
	total := tableRowFields(t, out, "合计")
	if len(total) < 8 {
		t.Fatalf("合计行列数 = %d，少于 8 列：%v", len(total), total)
	}
	if total[5] != "1500" || total[6] != "300" {
		t.Fatalf("合计行的缓存列 = %q / %q，期望 1500 / 300：%v", total[5], total[6], total)
	}
}

// TestUsageCacheHintOnlyWithCacheTokens 钉住末尾那句提示：
// 有缓存 token 时出现，全 0 时不出现；两种情况的提示风格与「花费为 0」那句一致。
func TestUsageCacheHintOnlyWithCacheTokens(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	const hint = "缓存读写已按模型的缓存单价计入估算花费"
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 30, OutTokens: 6, LatencyMS: 90,
	})
	code, out, stderr := run(t, "usage", "--since", "1d")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(out, hint) {
		t.Fatalf("缓存 token 全 0 时不该出现提示:\n%s", out)
	}
	// 没定价时写「未定价」，与「算出来就是 0」分开。
	if !strings.Contains(out, "花费未定价") {
		t.Fatalf("没定价时应提示未定价:\n%s", out)
	}

	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 30, OutTokens: 6,
		CacheRead: 900, CacheWrite: 0, LatencyMS: 90,
	})
	code, out, stderr = run(t, "usage", "--since", "1d")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, hint) {
		t.Fatalf("有缓存 token 时应出现提示:\n%s", out)
	}
}

// TestUsageCostCountsCacheTokens 钉住估算花费的新口径：
// 未命中缓存的输入按输入价、缓存读写按缓存单价、输出按输出价，四项都在金额里。
// 与 TestUsageCacheFallbackToInputPrice 成对：那个用例看缓存单价缺失时的回退。
func TestUsageCostCountsCacheTokens(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "price", "set", "m1", "--provider", "mine",
		"--in", "2", "--out", "4", "--cache-read", "0.5", "--cache-write", "3"); code != 0 {
		t.Fatalf("model price set 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	// 1M 未命中输入 × 2 + 2M 缓存读 × 0.5 + 1M 缓存写 × 3 + 1M 输出 × 4 = 2 + 1 + 3 + 4 = 10
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: id,
		HTTPStatus: 200, OK: true,
		InTokens: 3_000_000, InTokensUncached: 1_000_000, OutTokens: 1_000_000,
		CacheRead: 2_000_000, CacheWrite: 1_000_000, LatencyMS: 1000,
	})

	rows := mustUsageRows(t, "--by", "account", "--since", "1d")
	if got := rows["ds1"]; got.Cost != 10 {
		t.Fatalf("估算花费 = %v，期望 10（输入 2 + 缓存读 1 + 缓存写 3 + 输出 4）", got.Cost)
	}
	if got := rows["ds1"]; got.CacheRead != 2_000_000 || got.CacheWrite != 1_000_000 {
		t.Fatalf("缓存合计 = %+v，期望 2000000 / 1000000", got)
	}

	code, out, stderr := run(t, "usage", "--by", "account", "--since", "1d")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "10.00") {
		t.Fatalf("文本输出的估算花费应为 10.00:\n%s", out)
	}
	if strings.Contains(out, "花费为 0") {
		t.Fatalf("定了价时不该出现「花费为 0」:\n%s", out)
	}
}

// TestUsageCacheFallbackToInputPrice 钉住两个回退：缓存单价没填时按输入价折，
// 只填输入输出价时的金额与旧公式一模一样（历史花费不变）。
func TestUsageCacheFallbackToInputPrice(t *testing.T) {
	sandbox(t)
	mustInit(t)
	addPricedAccount(t, "ds1", "2", "4")
	id := accountID(t, "ds1")

	// 1M 未命中输入按 2 元 + 1M 输出按 4 元 = 6 元，与旧公式一致。
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 1_000_000, InTokensUncached: 1_000_000,
		OutTokens: 1_000_000, LatencyMS: 1000,
	})
	rows := mustUsageRows(t, "--by", "account", "--since", "1d")
	if got := rows["ds1"]; got.Cost != 6 {
		t.Fatalf("估算花费 = %v，期望 6（1M 输入 2 元 + 1M 输出 4 元）", got.Cost)
	}

	// 再记 1M 缓存读 + 1M 缓存写：没填缓存单价，按输入价 2 元各算一遍，合计 6 + 2 + 2 = 10。
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: id,
		HTTPStatus: 200, OK: true, CacheRead: 1_000_000, CacheWrite: 1_000_000, LatencyMS: 1000,
	})
	rows = mustUsageRows(t, "--by", "account", "--since", "1d")
	if got := rows["ds1"]; got.Cost != 10 {
		t.Fatalf("缓存单价缺失时的估算花费 = %v，期望 10（缓存读写按输入价回退）", got.Cost)
	}
}

// TestUsageAbsoluteRange 覆盖 --from/--to 的过滤与边界：
// 只给 --from 时上界不限，只给 --to 时下界不限，与 --since 同给是用法错误。
func TestUsageAbsoluteRange(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	now := time.Now().Truncate(time.Second)
	for _, rec := range []struct {
		at     time.Time
		tokens int64
	}{
		{now.Add(-2 * time.Hour), 100},
		{now.Add(-1 * time.Hour), 200},
		{now.Add(-10 * time.Minute), 300},
	} {
		addRecord(t, store.RequestRecord{
			TS: rec.at.Unix(), Inbound: "anthropic", Outbound: "anthropic",
			ModelRequested: "m1", ModelUsed: "m1", AccountID: id,
			HTTPStatus: 200, OK: true, InTokens: rec.tokens,
		})
	}

	from := now.Add(-90 * time.Minute).Format(time.RFC3339)
	to := now.Add(-30 * time.Minute).Format(time.RFC3339)
	rows := mustUsageRows(t, "--by", "model", "--from", from, "--to", to)
	if got := rows["m1"]; got.Requests != 1 || got.InTokens != 200 {
		t.Fatalf("--from/--to 汇总 = %+v，期望只保留中间那条 200 token", got)
	}

	rows = mustUsageRows(t, "--by", "model", "--from", from)
	if got := rows["m1"]; got.Requests != 2 || got.InTokens != 500 {
		t.Fatalf("只给 --from 的汇总 = %+v，期望包含现在仍在区间内的两条", got)
	}

	rows = mustUsageRows(t, "--by", "model", "--to", to)
	if got := rows["m1"]; got.Requests != 2 || got.InTokens != 300 {
		t.Fatalf("只给 --to 的汇总 = %+v，期望从头到 --to 的两条", got)
	}

	for _, args := range [][]string{
		{"usage", "--since", "nope"},
		{"usage", "--since", "1d", "--from", from},
		{"usage", "--from", "昨天"},
		{"usage", "--from", to, "--to", from},
		{"usage", "--from", "-1d", "11:00"},
	} {
		code, _, stderr := run(t, args...)
		if code != 2 {
			t.Fatalf("%v 退出码 = %d，期望用法错误 2，stderr = %s", args, code, stderr)
		}
	}
}

// TestUsageFiltersAndDayOrder 覆盖 usage 的三个筛选参数与 --by day 的日期升序：
// 筛选走 SQL 而不是取完再筛，日期分组按 key 升序，不再按请求数降序。
func TestUsageFiltersAndDayOrder(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "a1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add a1 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "a2", "other", "http://127.0.0.1:9", "openai-chat", "--key", "sk-2", "--models", "m1"); code != 0 {
		t.Fatalf("account add a2 退出码 = %d，stderr = %s", code, stderr)
	}
	a1 := accountID(t, "a1")
	a2 := accountID(t, "a2")

	now := time.Now().Truncate(time.Second)
	day1 := time.Date(now.Year(), now.Month(), now.Day()-2, 12, 0, 0, 0, time.Local)
	day2 := time.Date(now.Year(), now.Month(), now.Day()-1, 12, 0, 0, 0, time.Local)
	for _, rec := range []store.RequestRecord{
		{TS: day1.Unix(), Inbound: "anthropic", Outbound: "anthropic", ModelRequested: "m1", ModelUsed: "m1", AccountID: a1, HTTPStatus: 200, OK: true, InTokens: 10},
		{TS: day1.Add(time.Hour).Unix(), Inbound: "anthropic", Outbound: "anthropic", ModelRequested: "m1", ModelUsed: "m1", AccountID: a2, HTTPStatus: 200, OK: true, InTokens: 20},
		{TS: day2.Unix(), Inbound: "anthropic", Outbound: "anthropic", ModelRequested: "m2", ModelUsed: "m2", AccountID: a1, HTTPStatus: 200, OK: true, InTokens: 30},
	} {
		addRecord(t, rec)
	}

	code, out, stderr := run(t, "usage", "--by", "day", "--since", "7d", "--json")
	if code != 0 {
		t.Fatalf("usage --by day 退出码 = %d，stderr = %s", code, stderr)
	}
	var days []usageRowJSON
	mustJSON(t, out, &days)
	if len(days) != 2 || days[0].Key != day1.Format("2006-01-02") || days[1].Key != day2.Format("2006-01-02") {
		t.Fatalf("--by day 顺序 = %+v，期望日期升序 %s、%s", days, day1.Format("2006-01-02"), day2.Format("2006-01-02"))
	}

	if got := mustUsageRows(t, "--by", "account", "--account", "a1", "--since", "7d"); len(got) != 1 || got["a1"].Requests != 2 {
		t.Fatalf("--account a1 = %+v，期望只有 a1 的两条", got)
	}
	if got := mustUsageRows(t, "--by", "model", "--model", "m1", "--since", "7d"); len(got) != 1 || got["m1"].Requests != 2 {
		t.Fatalf("--model m1 = %+v，期望只有 m1 的两条", got)
	}
	if got := mustUsageRows(t, "--by", "account", "--provider", "other", "--since", "7d"); len(got) != 1 || got["a2"].Requests != 1 {
		t.Fatalf("--provider other = %+v，期望只有 a2 的一条", got)
	}
	if got := mustUsageRows(t, "--by", "model", "--account", "a1", "--model", "m2", "--since", "7d"); len(got) != 1 || got["m2"].Requests != 1 {
		t.Fatalf("--account a1 --model m2 = %+v，期望只有 m2 的一条", got)
	}
}

// TestUsageLightweightTimeRange 覆盖 usage 对共享时间写法的复用：
// 11:00 是当天 11:00，-1d 11:00 是昨天 11:00，区间仍是 [from, to)。
func TestUsageLightweightTimeRange(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m-yesterday,m-before,m-at,m-after"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	yesterday := today.AddDate(0, 0, -1)
	for model, at := range map[string]time.Time{
		"m-yesterday": yesterday.Add(11*time.Hour + 30*time.Minute),
		"m-before":    today.Add(10*time.Hour + 59*time.Minute),
		"m-at":        today.Add(11 * time.Hour),
		"m-after":     today.Add(11*time.Hour + 30*time.Minute),
	} {
		addRecord(t, store.RequestRecord{
			TS: at.Unix(), Inbound: "anthropic", Outbound: "anthropic",
			ModelRequested: model, ModelUsed: model, AccountID: id,
			HTTPStatus: 200, OK: true, InTokens: 10,
		})
	}

	rows := mustUsageRows(t, "--by", "model", "--from", "11:00")
	if len(rows) != 2 || rows["m-at"].Requests != 1 || rows["m-after"].Requests != 1 {
		t.Fatalf("--from 11:00 = %+v，期望只有当天 11:00 及之后的 m-at、m-after", rows)
	}

	rows = mustUsageRows(t, "--by", "model", "--from", "-1d 11:00", "--to", "11:00")
	if len(rows) != 2 || rows["m-yesterday"].Requests != 1 || rows["m-before"].Requests != 1 {
		t.Fatalf("--from -1d 11:00 --to 11:00 = %+v，期望昨天 11:00 到今天 11:00 的两条", rows)
	}
}

// TestLogTailLightweightTimeRange 钉住 log tail 与 usage 共用同一套轻量时间写法，
// 并且 --since 与 --from/--to 同时出现是用法错误。
func TestLogTailLightweightTimeRange(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "m-yesterday,m-before,m-at,m-after"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	yesterday := today.AddDate(0, 0, -1)
	for model, at := range map[string]time.Time{
		"m-yesterday": yesterday.Add(11*time.Hour + 30*time.Minute),
		"m-before":    today.Add(10*time.Hour + 59*time.Minute),
		"m-at":        today.Add(11 * time.Hour),
		"m-after":     today.Add(11*time.Hour + 30*time.Minute),
	} {
		addRecord(t, store.RequestRecord{
			TS: at.Unix(), Inbound: "anthropic", Outbound: "anthropic",
			ModelRequested: model, ModelUsed: model, AccountID: id,
			HTTPStatus: 200, OK: true,
		})
	}

	rows := mustLogRows(t, "-n", "10", "--from", "11:00")
	if len(rows) != 2 || rows["m-at"].Model != "m-at" || rows["m-after"].Model != "m-after" {
		t.Fatalf("log tail --from 11:00 = %+v，期望只有当天 11:00 及之后的记录", rows)
	}

	rows = mustLogRows(t, "-n", "10", "--from", "-1d 11:00", "--to", "11:00")
	if len(rows) != 2 || rows["m-yesterday"].Model != "m-yesterday" || rows["m-before"].Model != "m-before" {
		t.Fatalf("log tail --from -1d 11:00 --to 11:00 = %+v，期望昨天 11:00 到今天 11:00 的两条", rows)
	}

	code, _, stderr := run(t, "log", "tail", "--since", "1d", "--from", "11:00")
	if code != 2 || !strings.Contains(stderr, "--since") {
		t.Fatalf("--since 与 --from 同给时 code=%d stderr=%s，期望用法错误 2", code, stderr)
	}
	code, _, stderr = run(t, "log", "tail", "--from", "-1d", "11:00")
	if code != 2 || !strings.Contains(stderr, "加引号") {
		t.Fatalf("未加引号的相对日期 code=%d stderr=%s，期望用法错误 2", code, stderr)
	}
}
