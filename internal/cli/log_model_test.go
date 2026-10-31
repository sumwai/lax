package cli_test

import (
	"strings"
	"testing"
	"time"

	"lax/internal/store"
)

// 这个文件钉住 `log tail` 的模型列怎么处理 requests 的两列：
// 客户端给的名字与真正打出去的名字不同时写成 `别名 → 真名`，相同时只写一个名字；
// --json 两个字段都给，`model` 仍是客户端给的那个名字。

// displayCol 给出从行首到 sub 第一次出现处的显示宽度（CJK 算两列），
// 用来断言含 `→` 的行与别的行列对齐、不错行。
func displayCol(t *testing.T, line, sub string) int {
	t.Helper()
	i := strings.Index(line, sub)
	if i < 0 {
		t.Fatalf("行里没有 %q：%q", sub, line)
	}
	w := 0
	for _, r := range line[:i] {
		if r >= 0x2e80 && r <= 0xa4cf {
			w += 2
			continue
		}
		w++
	}
	return w
}

// lineWith 取第一行含 sub 的输出行。
func lineWith(t *testing.T, out, sub string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("输出里没有含 %q 的行:\n%s", sub, out)
	return ""
}

// TestLogTailModelColumnShowsAlias 覆盖别名命中的行与普通请求的行：
// 命中的行写成 `fast → glm-4.6`，普通行只有一个名字（协议列本来各带一个 `→`，
// 所以按 `→` 的个数判定模型列有没有多出一段）；
// 两行的账号列落在同一个显示列上，`→` 不会把后面的列挤歪。
func TestLogTailModelColumnShowsAlias(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "glm-4.6"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	// 普通请求先写，别名命中的后写：log tail 倒序，别名那行排在前面。
	addRecord(t, store.RequestRecord{
		TS: time.Now().Add(-time.Minute).Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "glm-4.6", ModelUsed: "glm-4.6", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 20, OutTokens: 4, LatencyMS: 90,
	})
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "openai-chat",
		ModelRequested: "fast", ModelUsed: "glm-4.6", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 10, OutTokens: 2, LatencyMS: 120,
	})

	code, out, stderr := run(t, "log", "tail", "-n", "5")
	if code != 0 {
		t.Fatalf("log tail 退出码 = %d，stderr = %s", code, stderr)
	}

	// 别名命中：模型列是 `fast → glm-4.6`，两侧名字都对。
	aliased := lineWith(t, out, "fast")
	if !strings.Contains(aliased, "fast → glm-4.6") {
		t.Fatalf("别名命中的行模型列应是 `fast → glm-4.6`:\n%s", aliased)
	}
	// 一个 `→` 来自协议列，一个来自模型列。
	if got := strings.Count(aliased, "→"); got != 2 {
		t.Fatalf("别名命中的行应有 2 个箭头（协议 + 模型列），实际 %d 个:\n%s", got, aliased)
	}

	// 普通请求：模型列只有一个名字，不出现恒定的重复。
	var plain string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "glm-4.6") && !strings.Contains(line, "fast") {
			plain = line
			break
		}
	}
	if plain == "" {
		t.Fatalf("输出里没有普通请求的行:\n%s", out)
	}
	if strings.Contains(plain, "glm-4.6 →") || strings.Contains(plain, "→ glm-4.6") {
		t.Fatalf("两个模型名相同时只该显示一个名字:\n%s", plain)
	}
	if got := strings.Count(plain, "→"); got != 1 {
		t.Fatalf("普通请求的行应只有协议列那 1 个箭头，实际 %d 个:\n%s", got, plain)
	}

	// 宽度对齐：账号列与表头的「账号」落在同一个显示列上，含 `→` 的行也不例外。
	header := lineWith(t, out, "账号")
	want := displayCol(t, header, "账号")
	if got := displayCol(t, aliased, "ds1"); got != want {
		t.Fatalf("别名命中的行账号列在第 %d 列，表头在第 %d 列:\n%s", got, want, out)
	}
	if got := displayCol(t, plain, "ds1"); got != want {
		t.Fatalf("普通请求的行账号列在第 %d 列，表头在第 %d 列:\n%s", got, want, out)
	}
}

// TestLogTailJSONHasBothModelNames 钉住 --json 的两个字段：
// `model` 仍是客户端给的名字，新增的 `model_used` 是真正打出去的名字；
// 两者相同时两个字段都有值，不做省略。
func TestLogTailJSONHasBothModelNames(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", "sk-1", "--models", "glm-4.6"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	id := accountID(t, "ds1")

	addRecord(t, store.RequestRecord{
		TS: time.Now().Add(-time.Minute).Unix(), Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "glm-4.6", ModelUsed: "glm-4.6", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 20, OutTokens: 4, LatencyMS: 90,
	})
	addRecord(t, store.RequestRecord{
		TS: time.Now().Unix(), Inbound: "anthropic", Outbound: "openai-chat",
		ModelRequested: "fast", ModelUsed: "glm-4.6", AccountID: id,
		HTTPStatus: 200, OK: true, InTokens: 10, OutTokens: 2, LatencyMS: 120,
	})

	code, out, stderr := run(t, "log", "tail", "--json")
	if code != 0 {
		t.Fatalf("log tail --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []map[string]any
	mustJSON(t, out, &rows)
	if len(rows) != 2 {
		t.Fatalf("应有 2 行，实际 %d 行：%s", len(rows), out)
	}
	byModel := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		model, ok := row["model"].(string)
		if !ok {
			t.Fatalf("每行都要有 model 字段：%v", row)
		}
		byModel[model] = row
	}

	aliased, ok := byModel["fast"]
	if !ok {
		t.Fatalf("--json 没有 model 为 fast 的行：%s", out)
	}
	if got := aliased["model_used"]; got != "glm-4.6" {
		t.Fatalf("别名命中的行 model_used = %v，期望 glm-4.6", got)
	}

	plain, ok := byModel["glm-4.6"]
	if !ok {
		t.Fatalf("--json 没有 model 为 glm-4.6 的行：%s", out)
	}
	if got, present := plain["model_used"]; !present || got != "glm-4.6" {
		t.Fatalf("两个名字相同时 model_used 也要有值，实际 %v（存在=%v）", got, present)
	}
}
