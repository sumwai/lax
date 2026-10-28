package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// providerRouteRow 是 provider ls --json 里的一条路由。
type providerRouteRow struct {
	Protocol   string `json:"protocol"`
	Path       string `json:"path"`
	ModelsPath string `json:"models_path"`
}

// providerRow 是 provider ls --json 的一行。
type providerRow struct {
	Name      string             `json:"name"`
	BaseURL   string             `json:"base_url"`
	Protocols []providerRouteRow `json:"protocols"`
	Note      string             `json:"note"`
}

// mustProviderRows 解析 provider ls --json，按厂商名索引。
func mustProviderRows(t *testing.T, out string) map[string]providerRow {
	t.Helper()
	var rows []providerRow
	mustJSON(t, out, &rows)
	byName := make(map[string]providerRow, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	return byName
}

// routeMap 把路由清单换成「协议 -> 路由」，方便按协议断言。
func routeMap(p providerRow) map[string]providerRouteRow {
	m := make(map[string]providerRouteRow, len(p.Protocols))
	for _, r := range p.Protocols {
		m[r.Protocol] = r
	}
	return m
}

// TestProviderLsSeeded 覆盖种子厂商在 provider ls 里的形状：
// 默认协议是按协议名排序的第一条，路由带完整路径与模型列表路径。
func TestProviderLsSeeded(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	byName := mustProviderRows(t, out)
	if len(byName) != 8 {
		t.Fatalf("种子厂商应有 8 家，实际 %d 家: %s", len(byName), out)
	}

	ds, ok := byName["deepseek"]
	if !ok {
		t.Fatalf("provider ls 缺 deepseek: %s", out)
	}
	if ds.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("deepseek 的 base_url = %q", ds.BaseURL)
	}
	if len(ds.Protocols) != 2 || ds.Protocols[0].Protocol != "anthropic" {
		t.Fatalf("deepseek 的路由应按协议名排序且有两条: %+v", ds.Protocols)
	}
	routes := routeMap(ds)
	if got := routes["anthropic"]; got.Path != "/anthropic/v1/messages" || got.ModelsPath != "" {
		t.Fatalf("deepseek 的 anthropic 路由 = %+v", got)
	}
	if got := routes["openai-chat"]; got.Path != "/v1/chat/completions" || got.ModelsPath != "/v1/models" {
		t.Fatalf("deepseek 的 openai-chat 路由 = %+v", got)
	}

	cc, ok := byName["commandcode"]
	if !ok {
		t.Fatalf("provider ls 缺 commandcode: %s", out)
	}
	if cc.BaseURL != "https://api.commandcode.ai/provider/v1" {
		t.Fatalf("commandcode 的 base_url = %q", cc.BaseURL)
	}
	ccRoutes := routeMap(cc)
	if got := ccRoutes["openai-chat"]; got.Path != "/chat/completions" || got.ModelsPath != "/models" {
		t.Fatalf("commandcode 的 openai-chat 路由 = %+v", got)
	}
	if got := ccRoutes["anthropic"]; got.Path != "/messages" {
		t.Fatalf("commandcode 的 anthropic 路由 = %+v", got)
	}

	// 人读的表格要带上默认协议与路径两列。
	code, out, stderr = run(t, "provider", "ls")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"默认协议", "路由与路径", "deepseek", "/anthropic/v1/messages"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider ls 文本输出缺 %q: %s", want, out)
		}
	}
}

// TestProviderAddKeepsOtherRoutesAndNote 钉住重复 add 的语义：
// 只覆盖传入的那几条路由，没传入的路由与备注都留着。
func TestProviderAddKeepsOtherRoutesAndNote(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com/",
		"--note", "自建",
		"--route", "openai-chat=/v1/chat/completions:/v1/models")
	if code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	mine := mustProviderRows(t, out)["mine"]
	// 末尾斜杠在写入时去掉：拼接地址时用的就是这个形式。
	if mine.BaseURL != "https://api.example.com" {
		t.Fatalf("base_url = %q，期望去掉末尾斜杠", mine.BaseURL)
	}
	if mine.Note != "自建" || len(mine.Protocols) != 1 {
		t.Fatalf("首次 add 后 = %+v", mine)
	}

	// 第二次只加另一条协议的路由，不给 --note。
	code, _, stderr = run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--route", "anthropic=/v1/messages")
	if code != 0 {
		t.Fatalf("第二次 provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr = run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	mine = mustProviderRows(t, out)["mine"]
	if len(mine.Protocols) != 2 {
		t.Fatalf("第二次 add 不应丢掉上一轮的路由: %+v", mine.Protocols)
	}
	if mine.Note != "自建" {
		t.Fatalf("没给 --note 时备注应保持原样，实际 %q", mine.Note)
	}

	// 第三次覆盖 chat 路由，换备注，anthropic 那条仍在。
	code, _, stderr = run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--note", "改过",
		"--route", "openai-chat=/v2/chat")
	if code != 0 {
		t.Fatalf("第三次 provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, _ = run(t, "provider", "ls", "--json")
	mine = mustProviderRows(t, out)["mine"]
	routes := routeMap(mine)
	if got := routes["openai-chat"]; got.Path != "/v2/chat" || got.ModelsPath != "" {
		t.Fatalf("同一协议重复 add 应覆盖那条路由: %+v", got)
	}
	if _, ok := routes["anthropic"]; !ok {
		t.Fatalf("覆盖一条路由时不该删掉另一条: %+v", mine.Protocols)
	}
	if mine.Note != "改过" {
		t.Fatalf("给了 --note 应更新备注，实际 %q", mine.Note)
	}
}

// TestProviderAddRouteErrors 覆盖 --route 表达式写错的每一类。
func TestProviderAddRouteErrors(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	cases := []struct {
		name string
		expr string
		want string
	}{
		{"缺 =", "openai-chat/v1/chat/completions", "缺少 ="},
		{"协议名不认识", "gemini=/v1/chat/completions", "协议名"},
		{"路径不以 / 开头", "openai-chat=v1/chat/completions", "路径要以 / 开头"},
		{"模型列表路径不以 / 开头", "openai-chat=/v1/chat/completions:models", "模型列表路径要以 / 开头"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, stderr := run(t, "provider", "add", "mine",
				"--base-url", "https://api.example.com", "--route", c.expr)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2（用法错误）", code)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("stderr 应说明 %q: %s", c.want, stderr)
			}
			if !strings.Contains(stderr, "openai-chat=/v1/chat/completions:/v1/models") {
				t.Fatalf("stderr 应给出正确示例: %s", stderr)
			}
		})
	}

	code, _, stderr := run(t, "provider", "add", "mine", "--route", "openai-chat=/v1/chat/completions")
	if code != 2 || !strings.Contains(stderr, "--base-url") {
		t.Fatalf("缺 --base-url 应是用法错误并提示补上: 退出码 %d，stderr %s", code, stderr)
	}

	code, _, _ = run(t, "provider", "add", "mine", "--base-url", "api.example.com")
	if code != 2 {
		t.Fatalf("--base-url 不带协议头的退出码 = %d，期望 2", code)
	}

	// 表达式写错时一行都不该落库。
	code, out, _ := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d", code)
	}
	if _, ok := mustProviderRows(t, out)["mine"]; ok {
		t.Fatalf("--route 写错时不该写入厂商: %s", out)
	}
}

// TestProviderRmRefusesWhileAccountsUse 覆盖「还有账号在用」的拒绝与 --force 的放行。
func TestProviderRmRefusesWhileAccountsUse(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "http://127.0.0.1:9",
		"--route", "openai-chat=/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "use1",
		"--provider", "mine", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "rm", "mine")
	if code != 1 {
		t.Fatalf("有账号在用时 provider rm 的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("拒绝删除时不该往 stdout 写结果: %s", out)
	}
	for _, want := range []string{"use1", "--force"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	code, out, _ = run(t, "provider", "ls", "--json")
	if _, ok := mustProviderRows(t, out)["mine"]; !ok {
		t.Fatalf("被拒绝的 rm 不该真删: %s", out)
	}

	code, out, stderr = run(t, "provider", "rm", "mine", "--force")
	if code != 0 {
		t.Fatalf("--force 的退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已删除厂商 mine", "仍保留自己的 base_url", "use1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--force 的输出应提到 %q: %s", want, out)
		}
	}

	code, out, _ = run(t, "provider", "ls", "--json")
	if _, ok := mustProviderRows(t, out)["mine"]; ok {
		t.Fatalf("删除后 provider ls 不该再有 mine: %s", out)
	}
}

// TestProviderRmMissingAndJSON 覆盖删不存在的厂商与该命令的 JSON 形状。
func TestProviderRmMissingAndJSON(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "rm", "nope")
	if code != 1 {
		t.Fatalf("删不存在的厂商退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("删除失败不该写 stdout: %s", out)
	}
	if !strings.Contains(stderr, "switchboard provider ls") {
		t.Fatalf("stderr 应给出下一步命令: %s", stderr)
	}

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--route", "openai-chat=/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr = run(t, "provider", "rm", "mine", "--json")
	if code != 0 {
		t.Fatalf("provider rm --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var got struct {
		Removed  string   `json:"removed"`
		Routes   int      `json:"routes"`
		Accounts []string `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("provider rm --json 输出不是 JSON: %v，%s", err, out)
	}
	if got.Removed != "mine" || got.Routes != 1 || len(got.Accounts) != 0 {
		t.Fatalf("provider rm --json = %+v", got)
	}
}
