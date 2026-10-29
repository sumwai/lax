package cli_test

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 这个文件把 README 与 DESIGN 第 3 节里出现的示例命令逐字搬进测试：
// 参数名、必填项与文档里印出来的输出一旦与实现漂移，这里会先红。
// 唯一的额外一步是把上游地址指到 httptest 假上游——账号的地址只在厂商行上（见 DESIGN 5.8），
// 所以走 addAccount（先按厂商名把厂商的地址指过去）而不是直接调 account add；
// 不这么做 model sync 会去连真实 deepseek，测试不许连真实厂商（见 AGENTS.md）。

// syncTableRow 匹配 model sync 人读表格的一行：账号、厂商、模型数、新增数。
var syncTableRow = regexp.MustCompile(`(?m)^(\S+)\s+(\S+)\s+(\d+)\s+(\d+)\s*$`)

// TestDesignSection3Examples 逐字跑 DESIGN 第 3 节的 provider 与 account 示例，
// 并断言文档里印出的输出行原样出现。
func TestDesignSection3Examples(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// DESIGN 2.1：每协议一条 flag 声明路由，输出一行确认。
	code, out, stderr := run(t, "provider", "add", "acme",
		"--base-url", "https://api.example.com",
		"--openai", "/v1/chat/completions", "--anthropic", "/v1/messages")
	if code != 0 {
		t.Fatalf("DESIGN 示例 provider add acme 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加厂商 acme（https://api.example.com），2 条路由，默认协议 openai-chat"; !strings.Contains(out, want) {
		t.Fatalf("provider add acme 的输出与 DESIGN 第 3 节不符：\n得到 %s\n期望包含 %q", out, want)
	}

	// DESIGN 2.1：删一条路由用 provider unset，输出两行。
	code, out, stderr = run(t, "provider", "unset", "acme", "anthropic")
	if code != 0 {
		t.Fatalf("DESIGN 示例 provider unset acme anthropic 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"acme 已清掉 anthropic 路由",
		"现有 1 条路由，默认协议 openai-chat",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider unset 的输出与 DESIGN 第 3 节不符：\n得到 %s\n期望包含 %q", out, want)
		}
	}

	// DESIGN 2：account add 的输出格式（厂商、协议、脱敏凭据）。
	code, out, stderr = addAccount(t, "deepseek-1", "deepseek", "", "", "--key", "sk-xxxxxxxx")
	if code != 0 {
		t.Fatalf("DESIGN 示例 account add deepseek-1 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加 deepseek-1（deepseek，openai-chat，凭据 sk-x…）"; !strings.Contains(out, want) {
		t.Fatalf("account add 的输出与 DESIGN 第 3 节不符：\n得到 %s\n期望包含 %q", out, want)
	}
}

// TestDesignSection3SyncExample 逐字跑 DESIGN 第 3 节第 3 步的 `model sync --account deepseek-1`：
// 表格五列、一行数据与下一步提示都要在，上游地址按 deepseek 的 chat 路由 models_path 取。
// 唯一的额外一步是把 deepseek 的地址指到假上游（`provider set --base-url`），参数名与文档一致。
func TestDesignSection3SyncExample(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models": {status: 200, body: `{"data":[{"id":"deepseek-v4-pro"},{"id":"deepseek-v4-fast"},{"id":"deepseek-r1"}]}`},
	})
	mustInit(t)

	code, out, stderr := addAccount(t, "deepseek-1", "deepseek", up.srv.URL, "", "--key", "sk-xxxxxxxx")
	if code != 0 {
		t.Fatalf("DESIGN 示例 account add deepseek-1 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加 deepseek-1（deepseek，openai-chat，凭据 sk-x…）"; !strings.Contains(out, want) {
		t.Fatalf("account add 的输出与 DESIGN 第 3 节不符：\n得到 %s\n期望包含 %q", out, want)
	}

	code, out, stderr = run(t, "model", "sync", "--account", "deepseek-1")
	if code != 0 {
		t.Fatalf("DESIGN 示例 model sync --account deepseek-1 退出码 = %d，stderr = %s", code, stderr)
	}
	assertSyncTable(t, out, "deepseek-1", "deepseek", 3, 3)

	// 地址来自 deepseek 的 chat 路由 models_path（/v1/models），凭据头跟那条路由走。
	calls := up.all()
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/v1/models" {
		t.Fatalf("上游调用 = %+v，期望一次 GET /v1/models", calls)
	}
	if calls[0].bearer != "Bearer sk-xxxxxxxx" {
		t.Fatalf("凭据头 = %q，期望 Bearer sk-xxxxxxxx", calls[0].bearer)
	}
}

// TestREADMEProviderExamples 逐字跑 README「自己加一家厂商」一节的命令序列：
// add 带 --default-protocol openai、set 补 responses 路由并换默认协议、unset 删路由、
// unset 一个本来就没有的 models-path（幂等文案）。
func TestREADMEProviderExamples(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// README 的 myco 示例：两条路由 + 命令行写法的默认协议。
	code, out, stderr := run(t, "provider", "add", "myco",
		"--base-url", "https://api.deepseek.com",
		"--openai", "/v1/chat/completions",
		"--anthropic", "/anthropic/v1/messages",
		"--default-protocol", "openai")
	if code != 0 {
		t.Fatalf("README 示例 provider add myco 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加厂商 myco（https://api.deepseek.com），2 条路由，默认协议 openai-chat"; !strings.Contains(out, want) {
		t.Fatalf("provider add myco 的输出与 README 不符：\n得到 %s\n期望包含 %q", out, want)
	}

	// README：补一条 responses 路由，并把默认协议换掉。
	if code, _, stderr = run(t, "provider", "set", "myco",
		"--responses", "/v1/responses", "--default-protocol", "responses"); code != 0 {
		t.Fatalf("README 示例 provider set myco 退出码 = %d，stderr = %s", code, stderr)
	}
	myco := mustProviderLs(t)["myco"]
	if len(myco.Protocols) != 3 || myco.DefaultProtocol != "openai-responses" {
		t.Fatalf("README 示例跑完后 myco = %+v，期望 3 条路由、默认协议 openai-responses", myco)
	}

	// README：删一条路由。
	if code, _, stderr = run(t, "provider", "unset", "myco", "anthropic"); code != 0 {
		t.Fatalf("README 示例 provider unset myco anthropic 退出码 = %d，stderr = %s", code, stderr)
	}
	if myco = mustProviderLs(t)["myco"]; len(myco.Protocols) != 2 {
		t.Fatalf("删掉 anthropic 路由后 myco = %+v，期望剩 2 条", myco)
	}

	// README：unset 一个本来就没有的厂商级模型列表覆盖，幂等地说明一句。
	code, out, stderr = run(t, "provider", "unset", "myco", "models-path")
	if code != 0 {
		t.Fatalf("README 示例 provider unset myco models-path 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "myco 本来就没有模型列表覆盖"; !strings.Contains(out, want) {
		t.Fatalf("幂等 unset 的输出与 README 描述不符：\n得到 %s\n期望包含 %q", out, want)
	}

	// README 原文举的例子是「myco 本来就没有 anthropic 路由」：删掉的 anthropic 路由
	// 已经不存在，第二次 unset 必须原样说明一句，退出码仍是 0。
	code, out, stderr = run(t, "provider", "unset", "myco", "anthropic")
	if code != 0 {
		t.Fatalf("README 示例第二次 provider unset myco anthropic 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "myco 本来就没有 anthropic 路由"; !strings.Contains(out, want) {
		t.Fatalf("幂等 unset 的输出与 README 描述不符：\n得到 %s\n期望包含 %q", out, want)
	}
}

// TestREADMEProviderGeminiExample 逐字跑 README「自己加一家厂商」一节的 gemini 示例：
// 只声明一条 gemini 路由、默认协议写 gemini，{model} 占位符原样落库；
// README 里说的「这家只有这一条路由时会被拒绝」也一并钉住。
func TestREADMEProviderGeminiExample(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// google-gemini 已经内置（迁移 0009），README 的示例改成指向自建代理的一家同名形状的厂商。
	code, out, stderr := run(t, "provider", "add", "gem-proxy",
		"--base-url", "https://gemini-proxy.example.com",
		"--gemini", "/v1beta/models/{model}:generateContent",
		"--default-protocol", "gemini")
	if code != 0 {
		t.Fatalf("README 示例 provider add gem-proxy 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加厂商 gem-proxy（https://gemini-proxy.example.com），1 条路由，默认协议 gemini"; !strings.Contains(out, want) {
		t.Fatalf("provider add gem-proxy 的输出与 README 不符：\n得到 %s\n期望包含 %q", out, want)
	}
	got := mustProviderLs(t)["gem-proxy"]
	if got.DefaultProtocol != "gemini" {
		t.Fatalf("README 示例的默认协议 = %q，期望 gemini", got.DefaultProtocol)
	}
	if route := routeMap(got)["gemini"]; route.Path != "/v1beta/models/{model}:generateContent" {
		t.Fatalf("README 示例的 gemini 路由 = %+v", route)
	}

	// README：这家只有这一条路由时 unset 会被拒绝，要撤就撤整个厂商。
	code, out, stderr = run(t, "provider", "unset", "gem-proxy", "gemini")
	if code != 1 || out != "" {
		t.Fatalf("删掉唯一一条路由的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "switchboard provider rm gem-proxy") {
		t.Fatalf("拒绝文案应指向 provider rm: %s", stderr)
	}
}

// TestREADMEProviderHeaderExamples 逐字跑 README「自己加一家厂商」一节末尾的附加请求头示例：
// set 一条协议级的 x-opencode-session、ls 看这家声明了什么（值脱敏）、unset 删掉它。
// README 里说 ls 只输出前四位，这里就按前四位断言，不只看「不是明文」。
func TestREADMEProviderHeaderExamples(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// README 的 opencode 示例以这家厂商已经登记过为前提。
	if code, _, stderr := run(t, "provider", "add", "opencode",
		"--base-url", "https://opencode.example.com",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("README 示例 provider add opencode 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "header", "set", "opencode", "x-opencode-session", "{session}", "--protocol", "openai-chat")
	if code != 0 {
		t.Fatalf("README 示例 provider header set 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已声明 opencode 的附加头 x-opencode-session（openai-chat）", "值 {ses…"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider header set 的输出与 README 不符：\n得到 %s\n期望包含 %q", out, want)
		}
	}

	code, out, stderr = run(t, "provider", "header", "ls", "opencode")
	if code != 0 {
		t.Fatalf("README 示例 provider header ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"厂商", "协议", "头名", "值（脱敏）", "x-opencode-session", "openai-chat", "{ses…"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider header ls 的输出与 README 不符：\n得到 %s\n期望包含 %q", out, want)
		}
	}

	code, out, stderr = run(t, "provider", "header", "unset", "opencode", "x-opencode-session", "--protocol", "openai-chat")
	if code != 0 {
		t.Fatalf("README 示例 provider header unset 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "opencode 已删掉附加头 x-opencode-session（openai-chat）"; !strings.Contains(out, want) {
		t.Fatalf("provider header unset 的输出与 README 不符：\n得到 %s\n期望包含 %q", out, want)
	}
	if rows := mustHeaderRows(t, "opencode"); len(rows) != 0 {
		t.Fatalf("删完应一条不剩，实际 %+v", rows)
	}
}

// TestREADMEModelFlowExamples 逐字跑 README 快速上手与「模型从上游拉」两节的
// account add / model sync / model probe 命令；唯一多出来的一步是把厂商的地址指到假上游。
func TestREADMEModelFlowExamples(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models":            {status: 200, body: `{"data":[{"id":"m-b"},{"id":"m-a"},{"id":"m-c"}]}`},
		"POST /v1/chat/completions": {status: 200, body: chatCompletionOK},
	})
	mustInit(t)

	if code, _, stderr := run(t, "provider", "ls"); code != 0 {
		t.Fatalf("README 快速上手里的 provider ls 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := addAccount(t, "ds", "deepseek", up.srv.URL, "", "--key", "sk-xxx")
	if code != 0 {
		t.Fatalf("README 示例 account add ds 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加 ds（deepseek，openai-chat，凭据 sk-x…）"; !strings.Contains(out, want) {
		t.Fatalf("account add ds 的输出 = %s，期望包含 %q", out, want)
	}

	// README：不带筛选，全部账号各拉一次模型列表。
	code, out, stderr = run(t, "model", "sync")
	if code != 0 {
		t.Fatalf("README 示例 model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	assertSyncTable(t, out, "ds", "deepseek", 3, 3)
	up.reset()

	// README：按厂商与账号一起筛。
	if code, _, stderr = run(t, "model", "sync", "--provider", "deepseek", "--account", "ds"); code != 0 {
		t.Fatalf("README 示例 model sync --provider deepseek --account ds 退出码 = %d，stderr = %s", code, stderr)
	}
	up.reset()

	// README：临时指定列表路径，只影响这一次调用。
	if code, _, stderr = run(t, "model", "sync", "--account", "ds", "--endpoint", "/v1/models"); code != 0 {
		t.Fatalf("README 示例 model sync --endpoint 退出码 = %d，stderr = %s", code, stderr)
	}
	calls := up.all()
	if len(calls) != 1 || calls[0].method != "GET" || calls[0].path != "/v1/models" {
		t.Fatalf("--endpoint 示例的上游调用 = %+v，期望一次 GET /v1/models", calls)
	}

	// README：--limit 5 限制探活数；三个模型都探通，可达性转成 allowed、选号从此按它走。
	code, out, stderr = run(t, "model", "probe", "--account", "ds", "--limit", "5")
	if code != 0 {
		t.Fatalf("README 示例 model probe --limit 5 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"ds：本次探活 3 个，跳过 0 个，可用 3", "可达 3 个"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model probe 输出缺 %q:\n%s", want, out)
		}
	}
	code, out, _ = run(t, "account", "ls")
	if !strings.Contains(out, "3 可用 / 3 已登记") {
		t.Fatalf("探活后 account ls 的模型列 = %s，期望 3 可用 / 3 已登记", out)
	}
}

// assertSyncTable 断言 model sync 的人读表格：表头五列、一行数据、下一步提示。
func assertSyncTable(t *testing.T, out, account, provider string, models, added int) {
	t.Helper()
	for _, want := range []string{"账号", "厂商", "模型", "新增", "备注", "下一步：switchboard model probe"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model sync 输出缺 %q:\n%s", want, out)
		}
	}
	rows := syncTableRow.FindAllStringSubmatch(out, -1)
	if len(rows) != 1 {
		t.Fatalf("model sync 表格应有恰好一行数据，实际 %d 行:\n%s", len(rows), out)
	}
	want := []string{account, provider, strconv.Itoa(models), strconv.Itoa(added)}
	got := rows[0][1:]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model sync 数据行 = %v，期望 %v", got, want)
		}
	}
}
