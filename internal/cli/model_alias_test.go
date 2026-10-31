package cli_test

import (
	"errors"
	"strings"
	"testing"

	"lax/internal/store"
	"lax/internal/store/storetest"
)

// aliasRow 是 model alias ls --json 的一项。
type aliasRow struct {
	Alias    string `json:"alias"`
	Target   string `json:"target"`
	Resolved string `json:"resolved"`
	OK       bool   `json:"ok"`
}

// mustAliasRows 跑 model alias ls 并解析 --json。
func mustAliasRows(t *testing.T, args ...string) []aliasRow {
	t.Helper()
	full := append([]string{"model", "alias", "ls"}, args...)
	full = append(full, "--json")
	code, out, stderr := run(t, full...)
	if code != 0 {
		t.Fatalf("%v 退出码 = %d，stderr = %s", full, code, stderr)
	}
	var rows []aliasRow
	mustJSON(t, out, &rows)
	return rows
}

// TestModelAliasSetTextAndJSON 覆盖 set 的两条输出与覆盖语义：
// 文本一行确认加一句核对的下一步；同名再写一次就是改目标，不留旧行；--json 的四个字段。
func TestModelAliasSetTextAndJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "model", "alias", "set", "fast", "glm-4.6")
	if code != 0 {
		t.Fatalf("model alias set 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已设置别名 fast → glm-4.6", "lax model alias ls"} {
		if !strings.Contains(out, want) {
			t.Fatalf("set 的文本输出缺 %q:\n%s", want, out)
		}
	}

	// 覆盖语义：同一个别名再写一次就是改目标，不新增一行。
	if code, _, stderr := run(t, "model", "alias", "set", "fast", "glm-5"); code != 0 {
		t.Fatalf("覆盖写退出码 = %d，stderr = %s", code, stderr)
	}
	rows := mustAliasRows(t)
	if len(rows) != 1 {
		t.Fatalf("覆盖后应只有一条别名，实际 %+v", rows)
	}
	if rows[0].Alias != "fast" || rows[0].Target != "glm-5" || rows[0].Resolved != "glm-5" || !rows[0].OK {
		t.Fatalf("覆盖后的行 = %+v，期望 fast → glm-5 且可解析", rows[0])
	}

	code, out, stderr = run(t, "model", "alias", "set", "fast", "glm-4.6", "--json")
	if code != 0 {
		t.Fatalf("model alias set --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var got aliasRow
	mustJSON(t, out, &got)
	if got.Alias != "fast" || got.Target != "glm-4.6" || got.Resolved != "glm-4.6" || !got.OK {
		t.Fatalf("--json 的项 = %+v，期望 fast → glm-4.6 且可解析", got)
	}
}

// TestModelAliasChainResolves 确认 ls 的解析列走的是整条链：quick → fast → glm-4.6
// 解析结果是真名，目标列仍写紧邻的那一跳。
func TestModelAliasChainResolves(t *testing.T) {
	sandbox(t)
	mustInit(t)

	for _, alias := range [][2]string{{"fast", "glm-4.6"}, {"quick", "fast"}} {
		if code, _, stderr := run(t, "model", "alias", "set", alias[0], alias[1]); code != 0 {
			t.Fatalf("set %v 退出码 = %d，stderr = %s", alias, code, stderr)
		}
	}

	rows := mustAliasRows(t, "quick")
	if len(rows) != 1 {
		t.Fatalf("只看一条时不该返回 %d 项: %+v", len(rows), rows)
	}
	if rows[0].Target != "fast" || rows[0].Resolved != "glm-4.6" || !rows[0].OK {
		t.Fatalf("链上的别名 = %+v，期望目标 fast、解析到 glm-4.6", rows[0])
	}

	code, out, stderr := run(t, "model", "alias", "ls")
	if code != 0 {
		t.Fatalf("model alias ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"别名", "目标", "状态", "解析", "quick", "fast", "glm-4.6", "可解析"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls 的表格缺 %q:\n%s", want, out)
		}
	}
}

// TestModelAliasUsageErrors 钉住退出码 2：参数个数不对、空值（含只有空白）都算用法错误，
// 要打出用法行，且一个字节都不写 stdout。
func TestModelAliasUsageErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	cases := [][]string{
		{"model", "alias", "set"},
		{"model", "alias", "set", "fast"},
		{"model", "alias", "set", "fast", "glm-4.6", "extra"},
		{"model", "alias", "set", "", "glm-4.6"},
		{"model", "alias", "set", "   ", "glm-4.6"},
		{"model", "alias", "set", "fast", ""},
		{"model", "alias", "unset"},
		{"model", "alias", "unset", ""},
		{"model", "alias", "unset", "fast", "extra"},
		{"model", "alias", "ls", "a", "b"},
		{"model", "alias", "ls", ""},
	}
	for _, args := range cases {
		code, out, stderr := run(t, args...)
		if code != 2 {
			t.Fatalf("%v 的退出码 = %d，期望 2（用法错误）", args, code)
		}
		if out != "" {
			t.Fatalf("%v 的用法错误不该写 stdout: %s", args, out)
		}
		if !strings.Contains(stderr, "用法：") {
			t.Fatalf("%v 的 stderr 应打出用法行: %s", args, stderr)
		}
	}

	// 不带子命令与 --help 等价：帮助写 stdout、退出码 0；未知子命令仍是用法错误，
	// 提示指向组级帮助。
	if code, out, stderr := run(t, "model", "alias"); code != 0 {
		t.Fatalf("不带子命令的退出码 = %d，期望 0；stderr = %s", code, stderr)
	} else if !strings.Contains(out, "lax model alias set") {
		t.Fatalf("不带子命令应打出组级帮助: %s", out)
	}
	if code, out, stderr := run(t, "model", "alias", "nope"); code != 2 || out != "" {
		t.Fatalf("未知子命令的退出码 = %d，stdout = %q", code, out)
	} else if !strings.Contains(stderr, "lax model alias --help") {
		t.Fatalf("未知子命令的提示应指向组级帮助: %s", stderr)
	}

	// 一条别名都没写成：上面的用例都不该动库。
	if rows := mustAliasRows(t); len(rows) != 0 {
		t.Fatalf("用法错误不该写库，实际 %+v", rows)
	}
}

// TestModelAliasUnsetTextAndJSON 覆盖 unset 的两条输出。
func TestModelAliasUnsetTextAndJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)

	for _, alias := range [][2]string{{"fast", "glm-4.6"}, {"quick", "fast"}} {
		if code, _, stderr := run(t, "model", "alias", "set", alias[0], alias[1]); code != 0 {
			t.Fatalf("set %v 退出码 = %d，stderr = %s", alias, code, stderr)
		}
	}

	code, out, stderr := run(t, "model", "alias", "unset", "quick")
	if code != 0 {
		t.Fatalf("model alias unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已删除别名 quick，现有 1 条别名"; !strings.Contains(out, want) {
		t.Fatalf("unset 的文本输出缺 %q:\n%s", want, out)
	}

	code, out, stderr = run(t, "model", "alias", "unset", "fast", "--json")
	if code != 0 {
		t.Fatalf("model alias unset --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var got struct {
		Alias     string `json:"alias"`
		Removed   bool   `json:"removed"`
		Remaining int    `json:"remaining"`
	}
	mustJSON(t, out, &got)
	if got.Alias != "fast" || !got.Removed || got.Remaining != 0 {
		t.Fatalf("--json = %+v，期望 alias=fast、removed=true、remaining=0", got)
	}
	if rows := mustAliasRows(t); len(rows) != 0 {
		t.Fatalf("删完应一条不剩，实际 %+v", rows)
	}
}

// TestModelAliasNotFoundListsAliases 钉住两类「找不到这条别名」：unset 与 ls <别名>。
// 两种情况都要退出码 1（不是静默成功、也不是用法错误），并在文案里列出已有的别名。
func TestModelAliasNotFoundListsAliases(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// 一条都没有时把下一步写成设置一条的命令。
	code, out, stderr := run(t, "model", "alias", "unset", "fast")
	if code != 1 || out != "" {
		t.Fatalf("空表 unset 的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "lax model alias set fast") {
		t.Fatalf("空表 unset 的文案应给出设置一条的命令: %s", stderr)
	}

	if code, _, stderr := run(t, "model", "alias", "set", "fast", "glm-4.6"); code != 0 {
		t.Fatalf("set 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "alias", "set", "quick", "fast"); code != 0 {
		t.Fatalf("set 退出码 = %d，stderr = %s", code, stderr)
	}

	for _, args := range [][]string{
		{"model", "alias", "unset", "nope"},
		{"model", "alias", "ls", "nope"},
	} {
		code, out, stderr := run(t, args...)
		if code != 1 {
			t.Fatalf("%v 的退出码 = %d，期望 1", args, code)
		}
		if out != "" {
			t.Fatalf("%v 失败时不该写 stdout: %s", args, out)
		}
		for _, want := range []string{"nope", "fast", "quick", "lax model alias ls"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%v 的文案缺 %q: %s", args, want, stderr)
			}
		}
	}
}

// TestModelAliasLsBrokenChain 覆盖 ls 里「解析不了」的显示：状态写不可解析，
// 解析列写 store 的原文（带上是哪几条串在一起），--json 的 resolved 是空串、ok 为 false。
//
// 这一对成环的别名用裸 SQL 写进去：写入侧会拦住成环的链，而这里要测的正是
// 「库里已经有一条坏链时 ls 怎么显示」（早先写进去的、或别的工具写进去的）。
func TestModelAliasLsBrokenChain(t *testing.T) {
	sandbox(t)
	mustInit(t)

	st := storetest.Open(t)
	if _, err := st.DB().Exec(`INSERT INTO model_aliases (alias, target, created_at) VALUES (?, ?, 0), (?, ?, 0)`,
		"a", "b", "b", "a"); err != nil {
		t.Fatalf("写入成环的别名: %v", err)
	}

	code, out, stderr := run(t, "model", "alias", "ls")
	if code != 0 {
		t.Fatalf("model alias ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"a", "b", "不可解析", store.ErrModelAliasCycle.Error()} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls 的输出缺 %q:\n%s", want, out)
		}
	}
	rows := mustAliasRows(t)
	if len(rows) != 2 {
		t.Fatalf("应有两条别名，实际 %+v", rows)
	}
	for _, r := range rows {
		if r.OK || r.Resolved != "" {
			t.Fatalf("坏链的 --json 项 = %+v，期望 ok=false、resolved 为空串", r)
		}
	}
}

// TestModelAliasLsEmpty 覆盖空表的提示：一行提示加一条示例命令，不是一张空表。
func TestModelAliasLsEmpty(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "model", "alias", "ls")
	if code != 0 {
		t.Fatalf("model alias ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "lax model alias set fast glm-4.6"; !strings.Contains(out, want) {
		t.Fatalf("空表的提示缺示例 %q:\n%s", want, out)
	}
	if code, out, _ := run(t, "model", "alias", "ls", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("空表的 --json 应是空数组，退出码 %d，实际 %q", code, out)
	}
}

// TestModelAliasShadowRejected 钉住「会遮蔽真实模型名」这条库层错误怎么出：
// 退出码 1、stdout 空、stderr 原样带出 store 的文案（含下一步建议），cli 不另写一份判定。
// 期望文案由 store 现算一次，用例因此不随库层的措辞改动而失效。
func TestModelAliasShadowRejected(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := addAccount(t, "ds", "acme", "https://api.example.com", "", "--key", "sk-test", "--models", "fast"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	wantErr := st.SetModelAlias("fast", "glm-4.6")
	if !errors.Is(wantErr, store.ErrModelAliasShadow) {
		t.Fatalf("库里写一条遮蔽真名的别名 = %v，期望 ErrModelAliasShadow", wantErr)
	}

	code, out, stderr := run(t, "model", "alias", "set", "fast", "glm-4.6")
	if code != 1 {
		t.Fatalf("遮蔽真名时退出码 = %d，期望 1（运行失败，不是用法错误）", code)
	}
	if out != "" {
		t.Fatalf("失败时不该写 stdout: %s", out)
	}
	if !strings.Contains(stderr, wantErr.Error()) {
		t.Fatalf("cli 应原样带出库层文案：\n得到 %s\n期望包含 %q", stderr, wantErr.Error())
	}
	if rows := mustAliasRows(t); len(rows) != 0 {
		t.Fatalf("被拒绝的别名不该落库，实际 %+v", rows)
	}
}

// TestModelAliasCycleRejected 钉住写入侧造出坏链时的行为：这一行已经落库，
// 所以退出码 1 的文案要说清「已经写进去了」并给出查看与撤掉两条命令。
func TestModelAliasCycleRejected(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "model", "alias", "set", "a", "b"); code != 0 {
		t.Fatalf("set a b 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr := run(t, "model", "alias", "set", "b", "a")
	if code != 1 {
		t.Fatalf("成环时退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("失败时不该写 stdout: %s", out)
	}
	for _, want := range []string{
		"已经写进库",
		store.ErrModelAliasCycle.Error(),
		"lax model alias ls b",
		"lax model alias unset b",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("成环的文案缺 %q: %s", want, stderr)
		}
	}

	// 撤掉其中一条就能解开：剩下的那条回到可解析。
	if code, _, stderr := run(t, "model", "alias", "unset", "b"); code != 0 {
		t.Fatalf("unset b 退出码 = %d，stderr = %s", code, stderr)
	}
	rows := mustAliasRows(t)
	if len(rows) != 1 || !rows[0].OK || rows[0].Resolved != "b" {
		t.Fatalf("删掉一条后 = %+v，期望 a → b 解析到 b", rows)
	}
}

// TestModelAliasGroupHelp 钉住组级帮助：三条用法、一个示例，以及只影响请求方向、
// 请求记录里两个模型名会不同这两段说明；结果打 stdout、退出码 0、不读数据库。
func TestModelAliasGroupHelp(t *testing.T) {
	sandbox(t)

	for _, arg := range []string{"-h", "--help", "help"} {
		code, out, stderr := run(t, "model", "alias", arg)
		if code != 0 {
			t.Fatalf("model alias %s 的退出码 = %d，期望 0", arg, code)
		}
		if stderr != "" {
			t.Fatalf("组级帮助不该写 stderr: %s", stderr)
		}
		for _, want := range []string{
			"lax model alias set <别名> <目标>",
			"lax model alias unset <别名>",
			"lax model alias ls [<别名>]",
			"lax model alias set fast glm-4.6",
			"只影响请求方向",
			"log tail",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("model alias %s 的帮助缺 %q:\n%s", arg, want, out)
			}
		}
	}
}

// TestModelAliasInGroupHelp 确认 model 一组的帮助里能看到 alias，子命令清单与分发对得上。
func TestModelAliasInGroupHelp(t *testing.T) {
	sandbox(t)

	code, out, stderr := run(t, "model", "--help")
	if code != 0 {
		t.Fatalf("model --help 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"ls", "sync", "probe", "alias"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model --help 的子命令清单缺 %q:\n%s", want, out)
		}
	}

	code, out, stderr = run(t, "model", "alias", "set", "--help")
	if code != 0 {
		t.Fatalf("model alias set --help 退出码 = %d", code)
	}
	if !strings.Contains(stderr, "--json") {
		t.Fatalf("子命令帮助应列出自己的参数: %s", stderr)
	}
	if out != "" {
		t.Fatalf("子命令帮助写 stderr，stdout 应是空的: %s", out)
	}
}

// TestREADMEModelAliasExample 逐字跑 README「模型从上游拉，再探活」一节的别名命令序列：
// set / ls / ls <别名> / unset，输出与 README 印出的形状一致。
// 唯一的额外一步是给 glm-4.6 造一个提供它的账号，让「别名指到真名」有个真实目标。
func TestREADMEModelAliasExample(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := addAccount(t, "ds", "acme", "https://api.example.com", "", "--key", "sk-xxx", "--models", "glm-4.6"); code != 0 {
		t.Fatalf("README 示例 account add ds 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "alias", "set", "fast", "glm-4.6")
	if code != 0 {
		t.Fatalf("README 示例 model alias set 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已设置别名 fast → glm-4.6"; !strings.Contains(out, want) {
		t.Fatalf("model alias set 的输出与 README 不符:\n得到 %s\n期望包含 %q", out, want)
	}

	code, out, stderr = run(t, "model", "alias", "ls")
	if code != 0 {
		t.Fatalf("README 示例 model alias ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"别名", "目标", "状态", "解析", "fast", "glm-4.6", "可解析"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model alias ls 的输出与 README 不符，缺 %q:\n%s", want, out)
		}
	}

	if code, _, stderr := run(t, "model", "alias", "ls", "fast"); code != 0 {
		t.Fatalf("README 示例 model alias ls fast 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "model", "alias", "unset", "fast")
	if code != 0 {
		t.Fatalf("README 示例 model alias unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已删除别名 fast"; !strings.Contains(out, want) {
		t.Fatalf("model alias unset 的输出与 README 不符:\n得到 %s\n期望包含 %q", out, want)
	}
}
