package cli_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"lax/internal/store"
	"lax/internal/store/storetest"
)

// declaredModelRow 是 model ls --json 的一行，取用例关心的字段：
// 目录里的协议集合与每个账号那一行的可达性事实。
type declaredModelRow struct {
	Model     string   `json:"model"`
	Protocols []string `json:"protocols"`
	Accounts  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Protocol   string `json:"protocol"`
		HTTPStatus int    `json:"http_status"`
		CheckedAt  int64  `json:"checked_at"`
		Error      string `json:"error"`
	} `json:"accounts"`
}

// modelShowJSONRow 是 model show --json 的一行。
type modelShowJSONRow struct {
	Account    string `json:"account"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Access     string `json:"access"`
	Protocol   string `json:"protocol"`
	HTTPStatus int    `json:"http_status"`
	CheckedAt  int64  `json:"checked_at"`
	Error      string `json:"error"`
}

// mustDeclaredModel 读 model ls --json 并只取其中一个模型的那一行。
func mustDeclaredModel(t *testing.T, model string) declaredModelRow {
	t.Helper()
	code, out, stderr := run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []declaredModelRow
	mustJSON(t, out, &rows)
	for _, r := range rows {
		if r.Model == model {
			return r
		}
	}
	t.Fatalf("model ls 里没有模型 %s: %s", model, out)
	return declaredModelRow{}
}

// mustModelShowRows 读 model show --json。
func mustModelShowRows(t *testing.T, model string) []modelShowJSONRow {
	t.Helper()
	code, out, stderr := run(t, "model", "show", model, "--json")
	if code != 0 {
		t.Fatalf("model show 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []modelShowJSONRow
	mustJSON(t, out, &rows)
	return rows
}

// TestModelAddWritesBothTables 钉住 model add 的写入范围与输出：
// 目录里补一行（协议集合带上声明的协议）、账号侧记一条 allowed，且一条上游请求都不发。
func TestModelAddWritesBothTables(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ts", "typesafe", up.srv.URL, "systemone", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "add", "typesafe/jev", "--account", "ts", "--protocol", "systemone")
	if code != 0 {
		t.Fatalf("model add 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	for _, want := range []string{
		"已声明模型 typesafe/jev（厂商 typesafe，账号 ts，待探活，协议 systemone）",
		"下一步：lax model probe --account ts --model typesafe/jev --protocol systemone",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout 缺 %q: %s", want, out)
		}
	}
	// 手写一条可达性不花上游的钱：声明是本地写库，探活才是调用。
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("model add 不该发上游请求: %+v", calls)
	}

	ls := mustDeclaredModel(t, "typesafe/jev")
	if len(ls.Accounts) != 1 || ls.Accounts[0].Name != "ts" || ls.Accounts[0].Status != store.AccessAllowed {
		t.Fatalf("model ls 的账号行 = %+v，期望 ts 一行 allowed", ls.Accounts)
	}
	if len(ls.Protocols) != 1 || ls.Protocols[0] != "systemone" {
		t.Fatalf("目录协议集合 = %v，期望只有 systemone", ls.Protocols)
	}

	rows := mustModelShowRows(t, "typesafe/jev")
	if len(rows) != 1 {
		t.Fatalf("model show 应有 1 行，实际 %d 行: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Account != "ts" || row.Provider != "typesafe" || row.Model != "typesafe/jev" ||
		row.Access != store.AccessAllowed || row.Protocol != "systemone" {
		t.Fatalf("model show 行 = %+v", row)
	}
	// 手写不是探活结论：HTTP 状态与探活时间都还是 0，状态因此是「待探活」。
	if row.HTTPStatus != 0 || row.CheckedAt != 0 || row.Error != "" {
		t.Fatalf("手写的行不该有探活事实: %+v", row)
	}
}

// TestModelAddIsIdempotent 钉住可重复执行：第二次 created 为 false、不加重复行、协议集合不重复。
func TestModelAddIsIdempotent(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ts", "typesafe", up.srv.URL, "systemone", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	args := []string{"model", "add", "typesafe/jev", "--account", "ts", "--protocol", "systemone"}
	code, first, stderr := run(t, args...)
	if code != 0 {
		t.Fatalf("第一次 model add 退出码 = %d，stdout = %s，stderr = %s", code, first, stderr)
	}
	var created map[string]any
	code, out, stderr := run(t, append([]string{"--json"}, args...)...)
	if code != 0 {
		t.Fatalf("第二次 model add 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &created)
	if created["created"] != false {
		t.Fatalf("第二次 model add 的 created = %v，期望 false: %s", created["created"], out)
	}
	if created["declared_protocol"] != "systemone" || created["protocol"] != "systemone" {
		t.Fatalf("declared_protocol 与 protocol 都该是 systemone: %s", out)
	}

	if rows := mustModelShowRows(t, "typesafe/jev"); len(rows) != 1 {
		t.Fatalf("重复声明不该多留一行，实际 %d 行: %+v", len(rows), rows)
	}
	ls := mustDeclaredModel(t, "typesafe/jev")
	if len(ls.Accounts) != 1 {
		t.Fatalf("重复声明不该多留账号行: %+v", ls.Accounts)
	}
	seen := 0
	for _, p := range ls.Protocols {
		if p == "systemone" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("目录协议集合里 systemone 出现了 %d 次，期望 1 次: %v", seen, ls.Protocols)
	}
}

// TestModelAddErrors 钉住两类失败：参数写歪是用法错误（退出码 2 且打出用法行），
// 账号与厂商对不上是运行失败（退出码 1 且给出可执行的下一步）。
func TestModelAddErrors(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ts", "typesafe", up.srv.URL, "systemone", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "other", "otherco", up.srv.URL, "systemone", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	usage := []struct {
		name string
		args []string
		want string
	}{
		{"缺 --account", []string{"model", "add", "typesafe/jev"}, "--account"},
		{"模型名为空", []string{"model", "add", "", "--account", "ts"}, "模型名"},
		{"协议不认识", []string{"model", "add", "typesafe/jev", "--account", "ts", "--protocol", "cohere"}, "--protocol"},
		{"协议给了空值", []string{"model", "add", "typesafe/jev", "--account", "ts", "--protocol", ""}, "--protocol"},
	}
	for _, c := range usage {
		t.Run(c.name, func(t *testing.T) {
			code, out, stderr := run(t, c.args...)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2（用法错误）；stderr = %s", code, stderr)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			for _, want := range []string{c.want, "用法：lax model add"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr 应提到 %q: %s", want, stderr)
				}
			}
		})
	}

	t.Run("未知账号", func(t *testing.T) {
		code, _, stderr := run(t, "model", "add", "typesafe/jev", "--account", "nope", "--protocol", "systemone")
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1（运行失败）；stderr = %s", code, stderr)
		}
		for _, want := range []string{"不存在", "lax account add nope"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	})

	t.Run("厂商与账号不符", func(t *testing.T) {
		code, _, stderr := run(t, "model", "add", "typesafe/jev", "--account", "ts", "--provider", "otherco")
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1（运行失败）；stderr = %s", code, stderr)
		}
		for _, want := range []string{"typesafe", "otherco", "--provider typesafe"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	})
}

// TestModelShow 钉住只读的视图：文本七列都取自库，--json 字段齐全，没有行时报错并给出两条下一步。
func TestModelShow(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ts", "typesafe", up.srv.URL, "systemone", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	acc, err := st.GetAccount("ts")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	pinned := time.Date(2026, 1, 2, 3, 4, 0, 0, time.Local).Unix()
	if err := st.SetAccountModelAccess(acc.ID, "typesafe/jev", store.AccessForbidden, http.StatusForbidden, "套餐不含这个模型", pinned); err != nil {
		t.Fatalf("写可达性: %v", err)
	}
	if err := st.SetAccountModelProtocol(acc.ID, "typesafe/jev", "systemone"); err != nil {
		t.Fatalf("写协议: %v", err)
	}

	code, out, stderr := run(t, "model", "show", "typesafe/jev")
	if code != 0 {
		t.Fatalf("model show 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"账号", "模型", "协议", "状态", "HTTP", "探活时间", "备注",
		"ts", "typesafe/jev", "systemone", "无权", "403",
		time.Unix(pinned, 0).Format("2006-01-02 15:04"), "套餐不含这个模型",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("文本输出缺 %q: %s", want, out)
		}
	}

	rows := mustModelShowRows(t, "typesafe/jev")
	if len(rows) != 1 {
		t.Fatalf("model show --json 应有 1 行，实际 %d 行: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Account != "ts" || row.Provider != "typesafe" || row.Model != "typesafe/jev" ||
		row.Access != store.AccessForbidden || row.Protocol != "systemone" ||
		row.HTTPStatus != http.StatusForbidden || row.CheckedAt != pinned || row.Error != "套餐不含这个模型" {
		t.Fatalf("model show --json 行 = %+v", row)
	}
	// 只读库：一条上游请求都不发。
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("model show 不该发上游请求: %+v", calls)
	}

	t.Run("未知模型", func(t *testing.T) {
		code, out, stderr := run(t, "model", "show", "nope/unknown")
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1（运行失败）；stdout = %s", code, out)
		}
		for _, want := range []string{"nope/unknown", "没有可达性记录", "lax model sync", "lax model add nope/unknown --account"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	})
}

// TestModelLsJSONProbeFacts 钉住 model ls --json 的账号行：探活事实（HTTP 状态、探活时间、错误）
// 与协议都在同一行上，脚本不必再跑一次 model show。
func TestModelLsJSONProbeFacts(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ts", "typesafe", up.srv.URL, "systemone", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	acc, err := st.GetAccount("ts")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	pinned := time.Now().Add(-time.Hour).Unix()
	if err := st.SetAccountModelAccess(acc.ID, "typesafe/jev", store.AccessAllowed, http.StatusOK, "上游回了一句提醒", pinned); err != nil {
		t.Fatalf("写可达性: %v", err)
	}
	if err := st.SetAccountModelProtocol(acc.ID, "typesafe/jev", "systemone"); err != nil {
		t.Fatalf("写协议: %v", err)
	}

	ls := mustDeclaredModel(t, "typesafe/jev")
	if len(ls.Accounts) != 1 {
		t.Fatalf("账号行 = %+v，期望 1 行", ls.Accounts)
	}
	got := ls.Accounts[0]
	if got.Status != store.AccessAllowed || got.Protocol != "systemone" ||
		got.HTTPStatus != http.StatusOK || got.CheckedAt != pinned || got.Error != "上游回了一句提醒" {
		t.Fatalf("model ls 的账号行 = %+v", got)
	}
}

// TestModelProbeProtocolOnly 钉住 --protocol 只打一条路：候选就是这一条，
// 不读目录声明、不取厂商默认协议，因此上游只会收到这条协议的路径。
func TestModelProbeProtocolOnly(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/systemone":        {status: 200, body: `{"answers":{"ok":{"type":"noul","noul":0.9}},"usage":{"input_tokens":3,"output_tokens":1}}`},
		"POST /v1/chat/completions": {status: 200, body: chatCompletionOK},
	})
	mustInit(t)
	// 两条路由都声明：默认协议是 openai-chat，不指定 --protocol 时会先打默认协议。
	ensureProvider(t, "typesafe", up.srv.URL,
		"--openai", "/v1/chat/completions",
		"--systemone", "/v1/systemone")
	if code, _, stderr := run(t, "account", "add", "ts", "--provider", "typesafe", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "add", "typesafe/jev", "--account", "ts", "--protocol", "systemone"); code != 0 {
		t.Fatalf("model add 退出码 = %d，stderr = %s", code, stderr)
	}
	up.reset()

	code, out, stderr := run(t, "model", "probe", "--account", "ts", "--model", "typesafe/jev", "--protocol", "systemone", "--refresh")
	if code != 0 {
		t.Fatalf("model probe --protocol 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	if paths := up.postPaths(); len(paths) != 1 || paths[0] != "/v1/systemone" {
		t.Fatalf("上游收到的探活路径 = %v，期望只有 /v1/systemone", paths)
	}

	// 认不出的协议是用法错误，且一条请求都不再发出去。
	before := len(up.postCalls())
	code, _, stderr = run(t, "model", "probe", "--account", "ts", "--model", "typesafe/jev", "--protocol", "cohere")
	if code != 2 {
		t.Fatalf("--protocol cohere 退出码 = %d，期望 2（用法错误）；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "用法：lax model probe") {
		t.Fatalf("用法错误应打出用法行: %s", stderr)
	}
	if after := len(up.postCalls()); after != before {
		t.Fatalf("协议写歪时不该发上游请求：%d -> %d", before, after)
	}
}

// TestModelProbeUndecidedKeepsAllowed 钉住未判定不改可达性：上游回 500 时那一行不是结论，
// 库里已确认的 allowed 与探活时间都保持原样，输出写「未判定」并计入未判定数。
func TestModelProbeUndecidedKeepsAllowed(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, nil, map[string]int{"m1": http.StatusInternalServerError})
	addProbeAccount(t, up, "ds1")

	st := storetest.Open(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	pinned := time.Now().Add(-time.Hour).Unix()
	if err := st.SetAccountModelAccess(acc.ID, "m1", store.AccessAllowed, http.StatusOK, "", pinned); err != nil {
		t.Fatalf("写可达性: %v", err)
	}

	code, out, stderr := run(t, "model", "probe", "--refresh")
	if code != 0 {
		t.Fatalf("未判定时退出码 = %d，期望 0（库里仍有可用模型）；stdout = %s，stderr = %s", code, out, stderr)
	}
	if !strings.Contains(out, "未判定 1") {
		t.Fatalf("汇总行应计入 1 个未判定: %s", out)
	}
	if !strings.Contains(out, "未判定") {
		t.Fatalf("表格的状态列应写未判定: %s", out)
	}

	row := st.ModelRow(t, "ds1", "m1")
	if row.Access != store.AccessAllowed || row.HTTPStatus != http.StatusOK || row.CheckedAt != pinned || row.Error != "" {
		t.Fatalf("未判定的那一轮不该改可达性，实际 %+v", row)
	}
}
