package cli_test

import (
	"net/http"
	"strings"
	"testing"
)

// 这个文件从命令行这一层钉住模型列表的三种已知形状：
// `{data:[…]}`、`{models:[…]}`（ollama 的 /api/tags 这类）与顶层数组。
// 形状自动识别只决定怎么读响应，不改变「地址从哪来」的优先级——厂商级 models_path 与
// `--endpoint` 的顺序照旧，前一条能拿到列表时不因为形状不同就去试后一条。

// syncRow 是 model sync --json 的一行。
type syncRow struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	Models   int    `json:"models"`
	Added    int    `json:"added"`
	Error    string `json:"error"`
}

// mustSyncRows 解析 model sync --json 的输出。
func mustSyncRows(t *testing.T, out string) []syncRow {
	t.Helper()
	var rows []syncRow
	mustJSON(t, out, &rows)
	return rows
}

// mustModelRowStatuses 读 model ls --json，返回「模型 → 该账号上的可达性」。
func mustModelRowStatuses(t *testing.T) map[string]string {
	t.Helper()
	code, out, stderr := run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	statuses := map[string]string{}
	for model, row := range mustModelRows(t, out) {
		if len(row.Accounts) != 1 {
			t.Fatalf("模型 %s 的账号数 = %d，期望 1: %s", model, len(row.Accounts), out)
		}
		statuses[model] = row.Accounts[0].Status
	}
	return statuses
}

// TestModelSyncResponseShapesWriteModels 三种形状各跑一次完整的 model sync：
// 退出码 0、拉到并新增的模型数对得上、每个名字都进模型目录且这个账号的行是 allowed
// （同步写「目录 + 该账号可达性」）。
func TestModelSyncResponseShapesWriteModels(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "data 数组",
			body: `{"object":"list","data":[{"id":"m2"},{"id":"m1"}]}`,
			want: []string{"m1", "m2"},
		},
		{
			// ollama 的 /api/tags：条目的名字在 name 上，没有 id。
			name: "models 数组",
			body: `{"models":[{"name":"qwen2.5"},{"name":"llama3:latest"}]}`,
			want: []string{"llama3:latest", "qwen2.5"},
		},
		{
			name: "顶层数组",
			body: `["m2","m1"]`,
			want: []string{"m1", "m2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sandbox(t)
			up := newRecorderUpstream(t, map[string]fakeResponse{
				"GET /v1/models": {status: http.StatusOK, body: c.body},
			})
			mustInit(t)
			if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
				t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
			}

			code, out, stderr := run(t, "model", "sync", "--account", "ds1", "--json")
			if code != 0 {
				t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
			}
			rows := mustSyncRows(t, out)
			if len(rows) != 1 {
				t.Fatalf("model sync 输出 %d 行，期望 1 行: %s", len(rows), out)
			}
			if rows[0].Models != len(c.want) || rows[0].Added != len(c.want) || rows[0].Error != "" {
				t.Fatalf("model sync = %+v，期望拉到并新增 %d 个模型、没有错误", rows[0], len(c.want))
			}

			// 写库：每个名字一行 allowed（还没探过），顺序按名字排。
			calls := up.all()
			if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/v1/models" {
				t.Fatalf("上游调用 = %+v，期望只打一次 GET /v1/models", calls)
			}
			statuses := mustModelRowStatuses(t)
			if len(statuses) != len(c.want) {
				t.Fatalf("model ls 有 %d 个模型，期望 %d 个: %v", len(statuses), len(c.want), statuses)
			}
			for _, model := range c.want {
				if statuses[model] != "allowed" {
					t.Fatalf("模型 %s 的可达性 = %q，期望 allowed（同步把这批名字标成可用）", model, statuses[model])
				}
			}

			// 同步已经把这个账号的可达性写成 allowed：计数与模型名都跟着变，
			// 「待探活」由 model ls 的 checked_at 表达，不由这里另记一份清单。
			code, out, stderr = run(t, "account", "ls", "--json")
			if code != 0 {
				t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
			}
			acc := mustAccountRow(t, out)
			if len(acc.Models) != len(c.want) {
				t.Fatalf("account ls models = %v，期望同步进来的 %d 个名字", acc.Models, len(c.want))
			}
			for i, want := range c.want {
				if acc.Models[i] != want {
					t.Fatalf("account ls models = %v，期望按名字排序的 %v", acc.Models, c.want)
				}
			}
			if acc.ModelsSynced != len(c.want) || acc.ModelsUsable != len(c.want) {
				t.Fatalf("account ls 模型计数 = 已登记 %d / 可用 %d，期望 %d / %d",
					acc.ModelsSynced, acc.ModelsUsable, len(c.want), len(c.want))
			}
		})
	}
}

// TestModelSyncEmptyModelsFieldIsEmptyListFailure 钉住「字段存在但为空」不是形状不对：
// 空列表算这个账号失败，不拿后面的形状兜底，也不写库。
func TestModelSyncEmptyModelsFieldIsEmptyListFailure(t *testing.T) {
	cases := []struct{ name, body string }{
		{"models 为空", `{"models":[]}`},
		{"data 为空而 models 有内容", `{"data":[],"models":[{"name":"m1"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sandbox(t)
			up := newRecorderUpstream(t, map[string]fakeResponse{
				"GET /v1/models": {status: http.StatusOK, body: c.body},
			})
			mustInit(t)
			if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
				t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
			}

			code, out, stderr := run(t, "model", "sync", "--account", "ds1", "--json")
			if code != 1 {
				t.Fatalf("空列表的退出码 = %d，期望 1，stdout = %s，stderr = %s", code, out, stderr)
			}
			if !strings.Contains(stderr, "1 个账号同步失败") {
				t.Fatalf("stderr 应说明这个账号同步失败: %s", stderr)
			}
			rows := mustSyncRows(t, out)
			if len(rows) != 1 {
				t.Fatalf("model sync 输出 %d 行，期望 1 行: %s", len(rows), out)
			}
			msg := rows[0].Error
			if !strings.Contains(msg, "上游没有返回模型") || !strings.Contains(msg, "base-url") {
				t.Fatalf("错误应说明上游返回了空列表并提示检查 base-url: %q", msg)
			}
			if strings.Contains(msg, "m1") {
				t.Fatalf("空列表不该用后面的形状兜底，错误里却提到了 m1: %q", msg)
			}
			if strings.Contains(msg, `{"data":[{"id":"…"}]}`) {
				t.Fatalf("空列表不是形状不对，错误里不该列三种形状: %q", msg)
			}

			// 失败不留行：account_models 里一个模型都没有。
			if statuses := mustModelRowStatuses(t); len(statuses) != 0 {
				t.Fatalf("同步失败却写进了模型: %v", statuses)
			}
		})
	}
}

// TestModelSyncUnknownShapeListsThreeShapes 三种形状都不匹配时报错，文案把三种形状都列出来，
// 并提示检查 base-url 或用 --models 手填。
func TestModelSyncUnknownShapeListsThreeShapes(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models": {status: http.StatusOK, body: `{"ok":true}`},
	})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "sync", "--account", "ds1", "--json")
	if code != 1 {
		t.Fatalf("形状不认识时退出码 = %d，期望 1，stdout = %s，stderr = %s", code, out, stderr)
	}
	rows := mustSyncRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("model sync 输出 %d 行，期望 1 行: %s", len(rows), out)
	}
	msg := rows[0].Error
	for _, want := range []string{
		`{"data":[{"id":"…"}]}`,
		`{"models":[{"name":"…"}]}`,
		`顶层数组 ["m1","m2"]`,
		"base-url",
		"--models",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案缺少 %q: %q", want, msg)
		}
	}
	if statuses := mustModelRowStatuses(t); len(statuses) != 0 {
		t.Fatalf("形状不对却写进了模型: %v", statuses)
	}
}

// TestModelSyncShapeDetectionKeepsModelsPathPriority 钉住形状识别不改地址优先级：
// 厂商级 models_path 是一条完整地址（返回 ollama 形状）时，同步只打那台、不打账号 base_url；
// 再加 `--endpoint` 时 `--endpoint` 压过厂商覆盖，打到账号 base_url 上的那条路径（返回 data 形状），
// 上一轮的名字留在库里、这一轮的名字也进库。
func TestModelSyncShapeDetectionKeepsModelsPathPriority(t *testing.T) {
	sandbox(t)
	base := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models":     {status: http.StatusOK, body: `{"data":[{"id":"m-base"}]}`},
		"GET /custom/models": {status: http.StatusOK, body: `{"data":[{"id":"m-endpoint"}]}`},
	})
	lists := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /tags": {status: http.StatusOK, body: `{"models":[{"name":"llama3:latest"}]}`},
	})

	mustInit(t)
	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.invalid",
		"--openai", "/v1/chat/completions",
		"--models-path", lists.srv.URL+"/tags"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "a1", "mine", base.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 第一轮：厂商级 models_path 是完整地址，形状是 ollama 的 models 数组。
	code, out, stderr := run(t, "model", "sync", "--account", "a1", "--json")
	if code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	rows := mustSyncRows(t, out)
	if len(rows) != 1 || rows[0].Models != 1 || rows[0].Added != 1 || rows[0].Error != "" {
		t.Fatalf("model sync = %+v，期望按完整地址拉到 1 个模型", rows)
	}
	if calls := lists.all(); len(calls) != 1 || calls[0].path != "/tags" {
		t.Fatalf("完整地址那台收到 %+v，期望一次 GET /tags", calls)
	}
	if calls := base.all(); len(calls) != 0 {
		t.Fatalf("账号 base_url 收到 %+v，完整地址的 models_path 不该拼它", calls)
	}
	if calls := lists.all(); calls[0].bearer != "Bearer sk-test" || calls[0].apiKey != "" {
		t.Fatalf("凭据头 = Bearer %q / x-api-key %q，期望按厂商默认协议发 Bearer", calls[0].bearer, calls[0].apiKey)
	}
	if statuses := mustModelRowStatuses(t); len(statuses) != 1 || statuses["llama3:latest"] != "allowed" {
		t.Fatalf("model ls = %v，期望只有 llama3:latest 且为 allowed", statuses)
	}

	// 第二轮：--endpoint 压过厂商覆盖，打到账号 base_url 上的 /custom/models（data 形状）。
	base.reset()
	lists.reset()
	code, out, stderr = run(t, "model", "sync", "--account", "a1", "--endpoint", "/custom/models", "--json")
	if code != 0 {
		t.Fatalf("model sync --endpoint 退出码 = %d，stderr = %s", code, stderr)
	}
	rows = mustSyncRows(t, out)
	if len(rows) != 1 || rows[0].Models != 1 || rows[0].Error != "" {
		t.Fatalf("model sync --endpoint = %+v，期望从 --endpoint 那条拿到 1 个模型", rows)
	}
	if calls := base.all(); len(calls) != 1 || calls[0].path != "/custom/models" {
		t.Fatalf("账号 base_url 收到 %+v，期望一次 GET /custom/models", calls)
	}
	if calls := lists.all(); len(calls) != 0 {
		t.Fatalf("厂商覆盖那台收到 %+v，--endpoint 生效时不该再打它", calls)
	}
	// 上一轮拉到的 llama3:latest 留着：同步只做加法，可达性只由探活结论改动，
	// 一次少给了几个名字不等于这些模型不可用。
	statuses := mustModelRowStatuses(t)
	if len(statuses) != 2 || statuses["m-endpoint"] != "allowed" || statuses["llama3:latest"] != "allowed" {
		t.Fatalf("model ls = %v，期望 m-endpoint 与上一轮的 llama3:latest 都在、都是 allowed", statuses)
	}
}
