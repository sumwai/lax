package cli_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"switchboard/internal/config"
	"switchboard/internal/store"
)

// providerRouteRow 是 provider ls --json 里的一条路由。
type providerRouteRow struct {
	Protocol   string `json:"protocol"`
	Path       string `json:"path"`
	ModelsPath string `json:"models_path"`
}

// providerRow 是 provider ls --json 的一行。
type providerRow struct {
	Name            string             `json:"name"`
	BaseURL         string             `json:"base_url"`
	DefaultProtocol string             `json:"default_protocol"`
	ModelsPath      string             `json:"models_path"`
	Protocols       []providerRouteRow `json:"protocols"`
	Note            string             `json:"note"`
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

// providerLine 在 provider ls 的人读表格里找一家厂商那一行，按列拆成字段。
func providerLine(t *testing.T, out, name string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return fields
		}
	}
	t.Fatalf("provider ls 文本输出里没有厂商 %q: %s", name, out)
	return nil
}

// seedProviderWithoutRoutes 用 store 的过渡写入口造一家一条路由都没有的厂商。
// 命令行已经造不出这种行（`provider add` 至少要求一条路由），但升级上来的老库里可能有：
// `account add` 与 `provider ls` 在这种行上的行为仍要钉住（见 DESIGN 5.8）。
func seedProviderWithoutRoutes(t *testing.T, name, baseURL string) {
	t.Helper()
	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析路径失败: %v", err)
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.AddProvider(store.Provider{Name: name, BaseURL: baseURL}, nil); err != nil {
		t.Fatalf("造零路由厂商失败: %v", err)
	}
}

// mustInit 建库，失败即停。
func mustInit(t *testing.T) {
	t.Helper()
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
}

// mustProviderLs 读 provider ls --json。
func mustProviderLs(t *testing.T) map[string]providerRow {
	t.Helper()
	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	return mustProviderRows(t, out)
}

// TestProviderLsSeeded 覆盖种子厂商在 provider ls 里的形状：
// 路由按协议名排序并带完整路径与模型列表路径，默认协议取库里的有效默认协议。
func TestProviderLsSeeded(t *testing.T) {
	sandbox(t)
	mustInit(t)

	byName := mustProviderLs(t)
	if len(byName) != 8 {
		t.Fatalf("种子厂商应有 8 家，实际 %d 家", len(byName))
	}

	ds, ok := byName["deepseek"]
	if !ok {
		t.Fatalf("provider ls 缺 deepseek")
	}
	if ds.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("deepseek 的 base_url = %q", ds.BaseURL)
	}
	if ds.ModelsPath != "" {
		t.Fatalf("deepseek 不该有厂商级模型列表覆盖，实际 %q", ds.ModelsPath)
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
		t.Fatalf("provider ls 缺 commandcode")
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

	// 人读的表格要带上默认协议、路由与模型列表三列。
	code, out, stderr := run(t, "provider", "ls")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"默认协议", "路由与路径", "模型列表", "deepseek", "/anthropic/v1/messages"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider ls 文本输出缺 %q: %s", want, out)
		}
	}
}

// TestProviderLsDefaultProtocolFromStore 钉住默认协议读库：
// deepseek 与 commandcode 都同时声明了 anthropic 与 openai-chat 两条路由，单靠排序会把 anthropic
// 排在前面，库里的显式默认是 openai-chat（迁移 0007），两处必须一致。
func TestProviderLsDefaultProtocolFromStore(t *testing.T) {
	sandbox(t)
	mustInit(t)

	rows := mustProviderLs(t)
	want := map[string]string{
		"deepseek":    "openai-chat",
		"commandcode": "openai-chat",
		"anthropic":   "anthropic",
		"openai":      "openai-chat",
	}
	for name, w := range want {
		row, ok := rows[name]
		if !ok {
			t.Fatalf("provider ls 缺 %s", name)
		}
		if row.DefaultProtocol != w {
			t.Fatalf("%s 的 default_protocol = %q，期望 %q", name, row.DefaultProtocol, w)
		}
	}

	// 人读表格的默认协议列与 --json 同一个值，且 deepseek 的路由列列出两条路由。
	code, out, stderr := run(t, "provider", "ls")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for name, w := range want {
		fields := providerLine(t, out, name)
		if len(fields) < 3 || fields[2] != w {
			t.Fatalf("%s 的默认协议列 = %v，期望 %q", name, fields, w)
		}
	}
	if ds := providerLine(t, out, "deepseek"); !strings.Contains(strings.Join(ds, " "), "/anthropic/v1/messages") {
		t.Fatalf("deepseek 的路由列应列出两条路由: %v", ds)
	}
}

// TestProviderLsWithoutRoutes 钉住一条路由都没有的厂商（老库里的过渡形状）：
// 默认协议显示 -，--json 里是空串与空数组，模型列表没有可显示的值时是 -，整条命令仍退出码 0。
func TestProviderLsWithoutRoutes(t *testing.T) {
	sandbox(t)
	mustInit(t)
	seedProviderWithoutRoutes(t, "noroute", "https://api.example.com")

	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	// protocols 必须是 [] 而不是 null，脚本里才能直接当数组用。
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("provider ls --json 输出不是 JSON: %v，%s", err, out)
	}
	found := false
	for _, item := range raw {
		if string(item["name"]) != `"noroute"` {
			continue
		}
		found = true
		if got := string(item["default_protocol"]); got != `""` {
			t.Fatalf("无路由厂商的 default_protocol = %s，期望空串", got)
		}
		if got := string(item["protocols"]); got != "[]" {
			t.Fatalf("无路由厂商的 protocols = %s，期望 []", got)
		}
		if got := string(item["models_path"]); got != `""` {
			t.Fatalf("无路由厂商的 models_path = %s，期望空串", got)
		}
	}
	if !found {
		t.Fatalf("provider ls 缺 noroute: %s", out)
	}

	code, out, stderr = run(t, "provider", "ls")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	fields := providerLine(t, out, "noroute")
	if len(fields) < 3 || fields[2] != "-" {
		t.Fatalf("无路由厂商的默认协议列 = %v，期望 -", fields)
	}
	if fields[len(fields)-1] != "-" {
		t.Fatalf("无路由厂商的模型列表列 = %v，期望 -", fields)
	}
}

// TestProviderLsModelsColumn 覆盖「模型列表」列的三种来源：
// 厂商级覆盖优先，没有覆盖时显示各路由的 models_path，都没有时是 -。
func TestProviderLsModelsColumn(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "override",
		"--base-url", "https://api.example.com",
		"--openai", "/v1/chat/completions",
		"--models-path", "/v9/models"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "provider", "add", "routeonly",
		"--base-url", "https://api.example.org",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "provider", "add", "nomodels",
		"--base-url", "https://api.example.net",
		"--anthropic", "/v1/messages"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	rows := mustProviderLs(t)
	if got := rows["override"].ModelsPath; got != "/v9/models" {
		t.Fatalf("override 的 models_path = %q", got)
	}
	if got := rows["routeonly"].ModelsPath; got != "" {
		t.Fatalf("routeonly 不该有厂商级覆盖，实际 %q", got)
	}
	if got := routeMap(rows["routeonly"])["openai-chat"].ModelsPath; got != "/v1/models" {
		t.Fatalf("openai 路由的 models_path 应按路径推导成 /v1/models，实际 %q", got)
	}

	code, out, stderr := run(t, "provider", "ls")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if line := strings.Join(providerLine(t, out, "override"), " "); !strings.Contains(line, "/v9/models") {
		t.Fatalf("override 的模型列表列应显示厂商级覆盖: %s", line)
	}
	if line := strings.Join(providerLine(t, out, "routeonly"), " "); !strings.Contains(line, "/v1/models") {
		t.Fatalf("routeonly 的模型列表列应显示路由推导出来的 /v1/models: %s", line)
	}
	if fields := providerLine(t, out, "nomodels"); fields[len(fields)-1] != "-" {
		t.Fatalf("nomodels 的模型列表列 = %v，期望 -", fields)
	}
}

// TestProviderAddCreates 覆盖 add 新建一家厂商：三条协议 flag、默认协议与备注一次写进去，
// openai 路由的模型列表路径按路径推导，地址末尾斜杠去掉。
func TestProviderAddCreates(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com/",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--responses", "/v1/responses",
		"--note", "自建", "--json")
	if code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	var added providerRow
	mustJSON(t, out, &added)
	if added.Name != "mine" || added.BaseURL != "https://api.example.com" || added.Note != "自建" {
		t.Fatalf("provider add --json = %+v", added)
	}
	if added.DefaultProtocol != "openai-chat" {
		t.Fatalf("默认协议应回退到 openai-chat，实际 %q", added.DefaultProtocol)
	}
	if len(added.Protocols) != 3 {
		t.Fatalf("应写进三条路由: %+v", added.Protocols)
	}
	routes := routeMap(added)
	if got := routes["openai-chat"]; got.Path != "/v1/chat/completions" || got.ModelsPath != "/v1/models" {
		t.Fatalf("openai 路由 = %+v", got)
	}
	if got := routes["anthropic"]; got.Path != "/v1/messages" || got.ModelsPath != "" {
		t.Fatalf("anthropic 路由 = %+v", got)
	}

	// 人读输出回显地址、路由条数、默认协议与下一步。
	code, out, stderr = run(t, "provider", "add", "human",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages", "--default-protocol", "anthropic")
	if code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已添加厂商 human", "1 条路由", "默认协议 anthropic", "switchboard account add"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider add 输出缺 %q: %s", want, out)
		}
	}
}

// TestProviderPathWritesAreNormalized 钉住「路径只规整、不拒绝输入」：provider add 与
// provider set 收到的 `v1/chat/completions`、`//v2//chat//completions/`、`v1/models` 这些写法
// 一律退出码 0、落库成规整形态；转发与模型同步打的是规整后的完整地址（见 DESIGN 5.8）。
func TestProviderPathWritesAreNormalized(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v2/models":            {status: http.StatusOK, body: modelsListM1},
		"POST /v2/chat/completions": {status: http.StatusOK, body: chatCompletionOK},
	})
	mustInit(t)

	// add：少写前导斜杠、多写重复与末尾斜杠都不报错。
	code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", up.srv.URL,
		"--openai", "v1/chat/completions",
		"--anthropic", "//v1//messages/",
		"--models-path", "v1/models")
	if code != 0 {
		t.Fatalf("不规范的路径写法应被规整而不是报错，退出码 = %d，stderr = %s", code, stderr)
	}
	added := mustProviderLs(t)["mine"]
	if got := routeMap(added); got["openai-chat"].Path != "/v1/chat/completions" || got["anthropic"].Path != "/v1/messages" {
		t.Fatalf("路由应落成规整形态: %+v", added.Protocols)
	}
	if added.ModelsPath != "/v1/models" {
		t.Fatalf("厂商级模型列表路径应落成 /v1/models，实际 %q", added.ModelsPath)
	}

	// 账号地址指到假上游，下面两条命令各自打一次上游，看的是规整后的完整地址。
	if code, _, stderr := run(t, "account", "add", "mine1",
		"--provider", "mine", "--base-url", up.srv.URL, "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// set --models-path：重复与末尾斜杠都规整，model sync 打到 /v2/models。
	if code, _, stderr := run(t, "provider", "set", "mine", "--models-path", "//v2//models/"); code != 0 {
		t.Fatalf("provider set --models-path 退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustProviderLs(t)["mine"]; got.ModelsPath != "/v2/models" {
		t.Fatalf("provider set 后的厂商级模型列表路径 = %q，期望 /v2/models", got.ModelsPath)
	}
	if code, _, stderr := run(t, "model", "sync", "--account", "mine1"); code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	calls := up.all()
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/v2/models" {
		t.Fatalf("上游调用 = %+v，期望一次 GET /v2/models", calls)
	}

	// set --openai：改路径同样只规整不拒绝，转发打到 /v2/chat/completions。
	up.reset()
	if code, _, stderr := run(t, "provider", "set", "mine", "--openai", "//v2//chat//completions/"); code != 0 {
		t.Fatalf("provider set --openai 退出码 = %d，stderr = %s", code, stderr)
	}
	if got := routeMap(mustProviderLs(t)["mine"])["openai-chat"]; got.Path != "/v2/chat/completions" {
		t.Fatalf("provider set 后的 openai 路由 = %+v，期望 /v2/chat/completions", got)
	}
	if code, _, stderr := run(t, "call", "hi", "--model", "m1"); code != 0 {
		t.Fatalf("call 退出码 = %d，stderr = %s", code, stderr)
	}
	calls = up.all()
	if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].path != "/v2/chat/completions" {
		t.Fatalf("上游调用 = %+v，期望一次 POST /v2/chat/completions", calls)
	}
}

// TestProviderAddDuplicate 钉住「add 只新建」：重名时报错并把用户指到 provider set，
// 已有的行一个字段都不动。
func TestProviderAddDuplicate(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com", "--openai", "/v1/chat/completions",
		"--note", "原备注"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://other.example.com", "--anthropic", "/v1/messages")
	if code != 1 {
		t.Fatalf("重名 add 的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("重名 add 不该往 stdout 写结果: %s", out)
	}
	for _, want := range []string{"厂商 mine 已存在", "switchboard provider set mine"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	mine := mustProviderLs(t)["mine"]
	if mine.BaseURL != "https://api.example.com" || mine.Note != "原备注" || len(mine.Protocols) != 1 {
		t.Fatalf("重名 add 不该改动已有的行: %+v", mine)
	}
}

// TestProviderAddEmptyValues 钉住「空串不再表示删除」：add 里任何一项传空值都是用法错误，
// 文案把用户指到 provider unset。
func TestProviderAddEmptyValues(t *testing.T) {
	sandbox(t)
	mustInit(t)

	cases := []struct {
		name string
		args []string
		flag string
	}{
		{"协议路径", []string{"--openai", ""}, "--openai"},
		{"协议路径只有斜杠", []string{"--openai", "//"}, "--openai"},
		{"默认协议", []string{"--default-protocol", ""}, "--default-protocol"},
		{"模型列表路径", []string{"--models-path", ""}, "--models-path"},
		{"模型列表路径只有斜杠", []string{"--models-path", "/"}, "--models-path"},
		{"备注", []string{"--note", ""}, "--note"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"provider", "add", "mine", "--base-url", "https://api.example.com",
				"--anthropic", "/v1/messages"}, c.args...)
			code, out, stderr := run(t, args...)
			if code != 2 {
				t.Fatalf("空值的退出码 = %d，期望 2（用法错误）", code)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			for _, want := range []string{c.flag, "传了空值", "switchboard provider unset"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr 应提到 %q: %s", want, stderr)
				}
			}
		})
	}
	if _, ok := mustProviderLs(t)["mine"]; ok {
		t.Fatal("空值被拒时不该写入厂商")
	}
}

// TestProviderAddUsageErrors 覆盖 add 的用法错误：缺地址、地址形状不对、一条路由都没给、
// 默认协议不认识。每条都要退出码 2 并给出正确用法。
// 路径写法不在这里断言：拼接改成程序规整、不报错，`v1/chat/completions`、`//v1//x`、
// `v1/models` 这类取值会被规整成完整地址（见 TestProviderPathWritesAreNormalized），而不是用错误。
func TestProviderAddUsageErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	t.Run("缺地址", func(t *testing.T) {
		code, out, stderr := run(t, "provider", "add", "mine", "--openai", "/v1/chat/completions")
		if code != 2 || out != "" {
			t.Fatalf("退出码 = %d，stdout = %q", code, out)
		}
		for _, want := range []string{"--base-url", "switchboard provider add <名称>"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	})

	t.Run("地址不带协议头", func(t *testing.T) {
		code, _, stderr := run(t, "provider", "add", "mine", "--base-url", "api.example.com", "--openai", "/v1/chat/completions")
		if code != 2 || !strings.Contains(stderr, "http://") {
			t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
		}
	})

	t.Run("没有协议路由", func(t *testing.T) {
		code, _, stderr := run(t, "provider", "add", "mine", "--base-url", "https://api.example.com")
		if code != 2 {
			t.Fatalf("退出码 = %d，期望 2", code)
		}
		for _, want := range []string{"至少给一条", "--anthropic", "--openai", "--responses"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	})

	t.Run("不认识的默认协议", func(t *testing.T) {
		code, _, stderr := run(t, "provider", "add", "mine", "--base-url", "https://api.example.com",
			"--openai", "/v1/chat/completions", "--default-protocol", "gemini")
		if code != 2 {
			t.Fatalf("退出码 = %d，期望 2", code)
		}
		// 错误文案给的是命令行写法，不是线协议名。
		for _, want := range []string{"--default-protocol", "anthropic、openai、responses", "--default-protocol openai"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	})

	if _, ok := mustProviderLs(t)["mine"]; ok {
		t.Fatal("用法错误时不该写入厂商")
	}
}

// TestProviderAddDefaultProtocol 覆盖 --default-protocol 的两种写法：
// 命令行写法 openai 落到 openai-chat，线协议全名 openai-responses 也认；
// 指不到这次声明的路由时 store 整笔拒绝，一条路由都不落库。
func TestProviderAddDefaultProtocol(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "provider", "add", "cli-spelling",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages", "--openai", "/v1/chat/completions",
		"--default-protocol", "openai", "--json")
	if code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	var added providerRow
	mustJSON(t, out, &added)
	if added.DefaultProtocol != "openai-chat" {
		t.Fatalf("--default-protocol openai 应落到 openai-chat，实际 %q", added.DefaultProtocol)
	}

	code, out, stderr = run(t, "provider", "add", "wire-spelling",
		"--base-url", "https://api.example.com",
		"--responses", "/v1/responses", "--default-protocol", "openai-responses", "--json")
	if code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &added)
	if added.DefaultProtocol != "openai-responses" {
		t.Fatalf("--default-protocol openai-responses 应原样落库，实际 %q", added.DefaultProtocol)
	}

	// 默认协议不在这次声明的路由里：退出码 1，厂商行与路由都不落库。
	code, out, stderr = run(t, "provider", "add", "bad",
		"--base-url", "https://api.example.com",
		"--openai", "/v1/chat/completions", "--default-protocol", "responses")
	if code != 1 {
		t.Fatalf("默认协议指不到路由的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("拒绝写库时不该往 stdout 写结果: %s", out)
	}
	for _, want := range []string{"默认协议", "openai-responses", "openai-chat"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
	if _, ok := mustProviderLs(t)["bad"]; ok {
		t.Fatal("整笔拒绝后不该留下厂商行或路由")
	}
}

// TestProviderSetUpdatesOnlyGivenFields 钉住 set 只改传入的字段：
// 其它路由、备注、默认协议保持原样，--models-path 改成新覆盖。
func TestProviderSetUpdatesOnlyGivenFields(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--note", "原备注"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 只补一条 responses 路由。
	code, out, stderr := run(t, "provider", "set", "mine",
		"--responses", "/v1/responses", "--json")
	if code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}
	var got providerRow
	mustJSON(t, out, &got)
	if len(got.Protocols) != 3 {
		t.Fatalf("补一条路由后应有 3 条: %+v", got.Protocols)
	}
	if got.Note != "原备注" || got.DefaultProtocol != "openai-chat" || got.ModelsPath != "" {
		t.Fatalf("没传的字段应保持原样: %+v", got)
	}

	// 只加厂商级模型列表覆盖。
	code, out, stderr = run(t, "provider", "set", "mine", "--models-path", "/v9/models", "--json")
	if code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &got)
	if got.ModelsPath != "/v9/models" || len(got.Protocols) != 3 || got.Note != "原备注" {
		t.Fatalf("只改 --models-path 时 = %+v", got)
	}

	// 改默认协议，命令行写法与线协议全名都认。
	code, out, stderr = run(t, "provider", "set", "mine", "--default-protocol", "anthropic", "--json")
	if code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &got)
	if got.DefaultProtocol != "anthropic" {
		t.Fatalf("--default-protocol anthropic 应生效，实际 %q", got.DefaultProtocol)
	}
	code, out, stderr = run(t, "provider", "set", "mine", "--default-protocol", "openai-chat", "--json")
	if code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &got)
	if got.DefaultProtocol != "openai-chat" {
		t.Fatalf("--default-protocol openai-chat 应生效，实际 %q", got.DefaultProtocol)
	}

	// 人读输出回显改了几项、现有几条路由与默认协议。
	code, out, stderr = run(t, "provider", "set", "mine", "--openai", "/v2/chat", "--note", "改过")
	if code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已修改厂商 mine", "改动 2 项", "3 条路由", "默认协议 openai-chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider set 输出缺 %q: %s", want, out)
		}
	}
	if got := routeMap(mustProviderLs(t)["mine"]); got["openai-chat"].Path != "/v2/chat" {
		t.Fatalf("覆盖后的 openai 路由 = %+v", got["openai-chat"])
	}
}

// TestProviderSetErrors 覆盖 set 的用法错误与运行失败：
// 空值指向 provider unset，厂商不存在指向 provider add，默认协议指不到路由时整笔拒绝。
func TestProviderSetErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com", "--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	for _, c := range []struct {
		name string
		args []string
	}{
		{"协议路径空值", []string{"--openai", ""}},
		{"协议路径只有斜杠", []string{"--openai", "//"}},
		{"备注空值", []string{"--note", ""}},
		{"默认协议空值", []string{"--default-protocol", ""}},
		{"模型列表空值", []string{"--models-path", ""}},
		{"地址空值", []string{"--base-url", ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"provider", "set", "mine"}, c.args...)
			code, out, stderr := run(t, args...)
			if code != 2 || out != "" {
				t.Fatalf("退出码 = %d，stdout = %q", code, out)
			}
			if !strings.Contains(stderr, "传了空值") {
				t.Fatalf("stderr 应说明是空值: %s", stderr)
			}
			if c.name != "地址空值" && !strings.Contains(stderr, "switchboard provider unset") {
				t.Fatalf("stderr 应指向 provider unset: %s", stderr)
			}
		})
	}

	t.Run("没有参数", func(t *testing.T) {
		code, _, stderr := run(t, "provider", "set", "mine")
		if code != 2 || !strings.Contains(stderr, "switchboard provider set <名称>") {
			t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
		}
	})

	// 路由路径的写法不在这里断言：拼接改成程序规整、不报错（同上）。

	t.Run("厂商不存在", func(t *testing.T) {
		code, _, stderr := run(t, "provider", "set", "nope", "--note", "x")
		if code != 1 || !strings.Contains(stderr, "switchboard provider add") {
			t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
		}
	})

	t.Run("默认协议指不到路由", func(t *testing.T) {
		code, out, stderr := run(t, "provider", "set", "mine", "--default-protocol", "responses")
		if code != 1 || out != "" {
			t.Fatalf("退出码 = %d，stdout = %q", code, out)
		}
		if !strings.Contains(stderr, "openai-responses") {
			t.Fatalf("stderr 应说明缺这条路由: %s", stderr)
		}
		if got := mustProviderLs(t)["mine"]; got.DefaultProtocol != "openai-chat" || len(got.Protocols) != 1 {
			t.Fatalf("整笔拒绝后不该改动厂商: %+v", got)
		}
	})
}

// TestProviderUnsetRoutesAndSettings 覆盖单字段与多字段 unset：
// 删路由、清模型列表覆盖、清显式默认协议、清备注，以及一次给多个字段。
func TestProviderUnsetRoutesAndSettings(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--models-path", "/v9/models",
		"--default-protocol", "anthropic",
		"--note", "自建"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	// 默认协议是 anthropic 时删它那条路由会被 store 整笔拒绝（先加路由或改默认协议），
	// 所以先把默认协议换成 openai-chat，这正是上游用法：set 只设值，unset 只删除。
	if code, _, stderr := run(t, "provider", "set", "mine", "--default-protocol", "openai"); code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}

	// 单字段：删掉 anthropic 路由，其余都不动。
	code, out, stderr := run(t, "provider", "unset", "mine", "anthropic", "--json")
	if code != 0 {
		t.Fatalf("provider unset 退出码 = %d，stderr = %s", code, stderr)
	}
	var got providerRow
	mustJSON(t, out, &got)
	if _, ok := routeMap(got)["anthropic"]; ok {
		t.Fatalf("anthropic 路由应被删掉: %+v", got.Protocols)
	}
	if len(got.Protocols) != 1 || got.ModelsPath != "/v9/models" || got.Note != "自建" {
		t.Fatalf("只该删掉那条路由: %+v", got)
	}

	// 多字段：一次清掉模型列表覆盖与备注，人读输出逐项说明。
	code, out, stderr = run(t, "provider", "unset", "mine", "models-path", "note")
	if code != 0 {
		t.Fatalf("provider unset 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"mine 已清掉模型列表覆盖", "mine 已清掉备注", "现有 1 条路由", "默认协议 openai-chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("unset 输出缺 %q: %s", want, out)
		}
	}
	after := mustProviderLs(t)["mine"]
	if after.ModelsPath != "" || after.Note != "" || len(after.Protocols) != 1 {
		t.Fatalf("清掉的两项应回到空值: %+v", after)
	}

	// 清显式默认协议：回退到固定顺序，因此这里从 anthropic 变成 openai-chat。
	if code, _, stderr := run(t, "provider", "add", "second",
		"--base-url", "https://api.example.org",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--default-protocol", "anthropic"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "provider", "unset", "second", "default-protocol", "--json")
	if code != 0 {
		t.Fatalf("provider unset 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &got)
	if got.DefaultProtocol != "openai-chat" || len(got.Protocols) != 2 {
		t.Fatalf("清掉显式默认协议后应按固定顺序回退: %+v", got)
	}
}

// TestProviderUnsetIdempotent 钉住幂等：要清的项本来就不存在时不报错、不写库，
// 输出一行说明，退出码仍是 0。
func TestProviderUnsetIdempotent(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "unset", "mine", "anthropic")
	if code != 0 {
		t.Fatalf("删除不存在的路由应退出码 0，实际 %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"mine 本来就没有 anthropic 路由", "现有 1 条路由"} {
		if !strings.Contains(out, want) {
			t.Fatalf("unset 输出缺 %q: %s", want, out)
		}
	}
	if got := mustProviderLs(t)["mine"]; len(got.Protocols) != 1 {
		t.Fatalf("幂等 unset 不该改动路由: %+v", got.Protocols)
	}

	// 重复字段只处理一次，也不因为重复而报错。
	code, out, stderr = run(t, "provider", "unset", "mine", "note", "note")
	if code != 0 {
		t.Fatalf("重复字段的退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Count(out, "本来就没有备注") != 1 {
		t.Fatalf("重复字段应只说明一次: %s", out)
	}
}

// TestProviderUnsetErrors 覆盖字段名的每一类错误：未知字段、带 -- 前缀、大小写不符、
// 不能 unset 的 base-url 与厂商名、一个字段都没给。
func TestProviderUnsetErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"未知字段", []string{"gemini"}, []string{"gemini", "不认识", "anthropic、openai、responses、models-path、default-protocol、note"}},
		{"带 -- 前缀", []string{"--", "--openai"}, []string{"不要带 -- 前缀", "可用："}},
		{"大小写不符", []string{"OpenAI"}, []string{"大小写不符", "openai"}},
		{"base-url", []string{"base-url"}, []string{"不能 unset", "--base-url"}},
		{"厂商名", []string{"name"}, []string{"不能 unset", "switchboard provider rm"}},
		{"没有字段", nil, []string{"用法：switchboard provider unset <名称> <字段>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"provider", "unset", "deepseek"}, c.args...)
			code, out, stderr := run(t, args...)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2（用法错误）", code)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			for _, want := range c.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr 应提到 %q: %s", want, stderr)
				}
			}
		})
	}

	t.Run("厂商不存在", func(t *testing.T) {
		code, _, stderr := run(t, "provider", "unset", "nope", "openai")
		if code != 1 || !strings.Contains(stderr, "switchboard provider ls") {
			t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
		}
	})
}

// TestProviderUnsetDefaultRouteRefused 钉住「删掉的路由正好是默认协议」：
// 整笔拒绝，文案由 store 给，列出仍然存在的协议并给出两条能执行的下一步
// （换成已声明的协议，或先 unset default-protocol）；cli 不再自己算一份回退规则。
func TestProviderUnsetDefaultRouteRefused(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// 种子 deepseek 的默认协议是 openai-chat。
	code, out, stderr := run(t, "provider", "unset", "deepseek", "openai")
	if code != 1 {
		t.Fatalf("删默认协议那条路由的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("拒绝写库时不该往 stdout 写结果: %s", out)
	}
	for _, want := range []string{"对应的路由不存在", `"openai-chat"`, "现有：anthropic",
		"switchboard provider set deepseek --default-protocol", "switchboard provider unset deepseek default-protocol"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
	if strings.Contains(stderr, "传空串") {
		t.Fatalf("stderr 不该再出现「传空串」这条作废的写法: %s", stderr)
	}
	if got := mustProviderLs(t)["deepseek"]; len(got.Protocols) != 2 {
		t.Fatalf("拒绝后不该删掉路由: %+v", got.Protocols)
	}

	// 同一次调用里先清默认协议，再删那条路由就放行。
	code, _, stderr = run(t, "provider", "unset", "deepseek", "default-protocol", "openai")
	if code != 0 {
		t.Fatalf("一起清默认协议再删路由的退出码 = %d，stderr = %s", code, stderr)
	}
	got := mustProviderLs(t)["deepseek"]
	if len(got.Protocols) != 1 || got.DefaultProtocol != "anthropic" {
		t.Fatalf("删掉 chat 路由后只剩 anthropic，默认协议随之回退: %+v", got)
	}
}

// TestProviderUnsetLastRouteRefused 钉住「删到一条路由都不剩」时整笔拒绝，
// 并把用户指到 provider rm。
func TestProviderUnsetLastRouteRefused(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "unset", "mine", "openai")
	if code != 1 {
		t.Fatalf("删光路由的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("拒绝写库时不该往 stdout 写结果: %s", out)
	}
	for _, want := range []string{"至少要保留一条协议路由", "switchboard provider rm mine"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
	if got := mustProviderLs(t)["mine"]; len(got.Protocols) != 1 {
		t.Fatalf("整笔拒绝后不该删掉路由: %+v", got.Protocols)
	}
}

// TestProviderGroupListsUnset 覆盖组级帮助与未知子命令提示都带上 unset。
func TestProviderGroupListsUnset(t *testing.T) {
	sandbox(t)
	mustInit(t)

	for _, args := range [][]string{
		{"provider", "--help"},
		{"provider", "nope"},
	} {
		code, out, stderr := run(t, args...)
		text := out + stderr
		if !strings.Contains(text, "unset") {
			t.Fatalf("%v 的输出应列出 unset: %s", args, text)
		}
		if args[1] == "--help" && code != 0 {
			t.Fatalf("组级帮助的退出码 = %d，期望 0", code)
		}
		if args[1] == "nope" && code != 2 {
			t.Fatalf("未知子命令的退出码 = %d，期望 2", code)
		}
	}
}

// TestProviderRmRefusesWhileAccountsUse 覆盖「还有账号在用」的拒绝与 --force 的放行。
func TestProviderRmRefusesWhileAccountsUse(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "http://127.0.0.1:9",
		"--openai", "/v1/chat/completions"); code != 0 {
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

	if _, ok := mustProviderLs(t)["mine"]; !ok {
		t.Fatal("被拒绝的 rm 不该真删")
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

	if _, ok := mustProviderLs(t)["mine"]; ok {
		t.Fatal("删除后 provider ls 不该再有 mine")
	}
}

// TestProviderRmMissingAndJSON 覆盖删不存在的厂商与该命令的 JSON 形状。
func TestProviderRmMissingAndJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)

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
		"--openai", "/v1/chat/completions"); code != 0 {
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

// TestProviderRmForceKeepsAccountForwarding 确认 provider rm --force 删掉厂商行与它的全部路由之后，
// 原先用这家厂商的账号仍能转发：账号行固化了自己的 base_url，路由没了只是让出站地址回到
// base_url 后缀猜测，请求不该 404，也不该因为读不到厂商行就失败。
//
// 没有这条断言，删除厂商时顺手清掉账号地址（或让转发依赖厂商行存在）不会被测出来，
// 线上表现是删完厂商之后所有请求 404。
func TestProviderRmForceKeepsAccountForwarding(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatCompletionOK},
	})
	mustInit(t)

	// 用种子厂商 deepseek：它的默认协议是 openai-chat，地址用 --base-url 指到假上游。
	code, out, stderr := run(t, "account", "add", "ds1",
		"--provider", "deepseek", "--base-url", up.srv.URL, "--key", "sk-test",
		"--models", "m1", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	added := mustAccountAdd(t, out)
	if added.Provider != "deepseek" || added.Protocol != "openai-chat" || added.BaseURL != up.srv.URL {
		t.Fatalf("账号的默认值 = %+v，期望 deepseek / openai-chat / 假上游地址", added)
	}

	// 账号还在用 deepseek：不带 --force 拒绝，带了才删。
	if code, out, _ = run(t, "provider", "rm", "deepseek"); code != 1 {
		t.Fatalf("有账号在用时退出码 = %d，期望 1", code)
	}
	code, out, stderr = run(t, "provider", "rm", "deepseek", "--force")
	if code != 0 {
		t.Fatalf("--force 的退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已删除厂商 deepseek", "仍保留自己的 base_url", "ds1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--force 的输出应提到 %q: %s", want, out)
		}
	}
	if _, ok := mustProviderLs(t)["deepseek"]; ok {
		t.Fatal("删除后 provider ls 不该再有 deepseek")
	}

	// 账号行仍固化着自己的地址：删厂商不改账号。
	code, out, _ = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d", code)
	}
	var accounts []map[string]any
	mustJSON(t, out, &accounts)
	if len(accounts) != 1 || accounts[0]["base_url"] != up.srv.URL {
		t.Fatalf("账号的 base_url 应保持不变，实际 %+v", accounts)
	}

	// 转发照旧：厂商没了，地址按 base_url 后缀猜出 /v1/chat/completions，凭据头是 Bearer。
	code, out, stderr = run(t, "call", "hi", "--model", "m1")
	if code != 0 {
		t.Fatalf("删厂商后 call 的退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("删厂商后 call 的输出 = %q，期望包含 pong", out)
	}
	calls := up.all()
	if len(calls) != 1 || calls[0].path != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %+v，期望只打一次 /v1/chat/completions", calls)
	}
	if calls[0].bearer != "Bearer sk-test" {
		t.Fatalf("凭据头 = %q，期望 Bearer sk-test", calls[0].bearer)
	}
}

// TestProviderUnsetMultiFieldKeepsOneRoute 覆盖「一次删多条、删到只剩一条仍成功」：
// 三条路由里一次给两个字段，剩下一条；再删一条也放行，最后一条才被拒。
func TestProviderUnsetMultiFieldKeepsOneRoute(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--responses", "/v1/responses",
		"--default-protocol", "openai"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 一次删两条：3 → 1，退出码 0，剩的是默认协议那条。
	code, out, stderr := run(t, "provider", "unset", "mine", "anthropic", "responses")
	if code != 0 {
		t.Fatalf("多字段 unset 的退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"mine 已清掉 anthropic 路由", "mine 已清掉 openai-responses 路由", "现有 1 条路由，默认协议 openai-chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("unset 输出缺 %q: %s", want, out)
		}
	}
	got := mustProviderLs(t)["mine"]
	if len(got.Protocols) != 1 || got.Protocols[0].Protocol != "openai-chat" {
		t.Fatalf("一次删两条后应只剩 openai-chat: %+v", got.Protocols)
	}

	// 补回两条，再逐条删到只剩一条：每一步都必须成功，最后一步才被拒。
	if code, _, stderr := run(t, "provider", "set", "mine",
		"--anthropic", "/v1/messages", "--responses", "/v1/responses"); code != 0 {
		t.Fatalf("provider set 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "provider", "unset", "mine", "anthropic"); code != 0 {
		t.Fatalf("删到剩两条的退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "provider", "unset", "mine", "responses")
	if code != 0 {
		t.Fatalf("删到只剩一条的退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "现有 1 条路由") {
		t.Fatalf("删到只剩一条时应该成功，输出了 %s", out)
	}
	if got := mustProviderLs(t)["mine"]; len(got.Protocols) != 1 {
		t.Fatalf("删到只剩一条后不该再动路由: %+v", got.Protocols)
	}
}

// TestProviderUnsetSameFieldTwice 钉住幂等：同一个字段第二次 unset 输出「本来就没有」、
// 退出码 0，库里的值不变。
func TestProviderUnsetSameFieldTwice(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--default-protocol", "anthropic",
		"--note", "自建"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 第一次：确实清掉了备注。
	code, out, stderr := run(t, "provider", "unset", "mine", "note")
	if code != 0 || !strings.Contains(out, "mine 已清掉备注") {
		t.Fatalf("第一次 unset note 的退出码 = %d，输出 %s，stderr = %s", code, out, stderr)
	}

	// 第二次：同一字段不再有值，说明一句、退出码 0。
	code, out, stderr = run(t, "provider", "unset", "mine", "note")
	if code != 0 {
		t.Fatalf("第二次 unset note 的退出码 = %d，期望 0，stderr = %s", code, stderr)
	}
	if out != "mine 本来就没有备注\n现有 2 条路由，默认协议 anthropic\n" {
		t.Fatalf("第二次 unset note 的输出 = %q，期望只说一句本来就没有", out)
	}

	// 显式默认协议同样幂等：第二次清掉时已经不存在。
	if code, _, stderr := run(t, "provider", "unset", "mine", "default-protocol"); code != 0 {
		t.Fatalf("第一次 unset default-protocol 的退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "provider", "unset", "mine", "default-protocol")
	if code != 0 {
		t.Fatalf("第二次 unset default-protocol 的退出码 = %d，期望 0，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "mine 本来就没有显式默认协议") {
		t.Fatalf("第二次 unset default-protocol 的输出 = %q", out)
	}
}

// TestProviderUnsetRepeatedFieldAppliedOnce 钉住重复字段只处理一次：
// 有值可删时也只删一次、只输出一行，库里的结果与只给一个字段一样。
func TestProviderUnsetRepeatedFieldAppliedOnce(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "unset", "mine", "openai", "openai")
	if code != 0 {
		t.Fatalf("重复字段的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := strings.Count(out, "mine 已清掉 openai 路由"); got != 1 {
		t.Fatalf("重复字段应只输出一行「已清掉」，实际 %d 行: %s", got, out)
	}
	if strings.Contains(out, "本来就没有") {
		t.Fatalf("有值可删时不该出现「本来就没有」: %s", out)
	}
	if got := mustProviderLs(t)["mine"]; len(got.Protocols) != 1 {
		t.Fatalf("重复字段不该多删路由: %+v", got.Protocols)
	}
}

// TestProviderSetEmptyValueWritesNothing 覆盖 set 里传空值（--openai ""、--note ""）：
// 退出码 2、文案指向 provider unset，且同一条命令里其它合法字段也不落库——
// 不留「地址改了、路由没删」这种半截写入。
func TestProviderSetEmptyValueWritesNothing(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.com",
		"--anthropic", "/v1/messages",
		"--openai", "/v1/chat/completions",
		"--models-path", "/v9/models",
		"--default-protocol", "openai",
		"--note", "原备注"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	before := mustProviderLs(t)["mine"]

	cases := []struct {
		name string
		flag string
		args []string
	}{
		{"协议路径空值在前", "--openai", []string{"--openai", "", "--anthropic", "/v2/messages"}},
		{"协议路径空值在后", "--openai", []string{"--anthropic", "/v2/messages", "--openai", ""}},
		{"备注空值在前", "--note", []string{"--note", "", "--base-url", "https://api.other.com"}},
		{"备注空值在后", "--note", []string{"--base-url", "https://api.other.com", "--note", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, stderr := run(t, append([]string{"provider", "set", "mine"}, c.args...)...)
			if code != 2 {
				t.Fatalf("空值的退出码 = %d，期望 2（用法错误）", code)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			for _, want := range []string{c.flag, "传了空值", "switchboard provider unset"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr 应提到 %q: %s", want, stderr)
				}
			}
			after := mustProviderLs(t)["mine"]
			if after.BaseURL != before.BaseURL || after.Note != before.Note ||
				after.ModelsPath != before.ModelsPath || after.DefaultProtocol != before.DefaultProtocol {
				t.Fatalf("空值被拒后不该改动任何设置:\n前 %+v\n后 %+v", before, after)
			}
			if got, want := routeMap(after), routeMap(before); len(got) != len(want) {
				t.Fatalf("空值被拒后路由数变了: %d → %d", len(want), len(got))
			}
			if after := routeMap(mustProviderLs(t)["mine"]); after["anthropic"].Path != "/v1/messages" {
				t.Fatalf("同一条命令里合法的 --anthropic 也不该落库，实际 %+v", after["anthropic"])
			}
		})
	}
}

// TestDefaultProtocolSpellings 覆盖 --default-protocol 的两种写法在 add 与 set 上等价：
// openai / openai-chat 都落到 openai-chat，responses / openai-responses 都落到
// openai-responses，anthropic 两边同形。
func TestDefaultProtocolSpellings(t *testing.T) {
	cases := []struct{ spelling, wire string }{
		{"openai", "openai-chat"},
		{"openai-chat", "openai-chat"},
		{"responses", "openai-responses"},
		{"openai-responses", "openai-responses"},
		{"anthropic", "anthropic"},
	}
	for _, c := range cases {
		t.Run("add "+c.spelling, func(t *testing.T) {
			sandbox(t)
			mustInit(t)

			code, _, stderr := run(t, "provider", "add", "spelled",
				"--base-url", "https://api.example.com",
				"--anthropic", "/v1/messages",
				"--openai", "/v1/chat/completions",
				"--responses", "/v1/responses",
				"--default-protocol", c.spelling)
			if code != 0 {
				t.Fatalf("provider add --default-protocol %s 退出码 = %d，stderr = %s", c.spelling, code, stderr)
			}
			if got := mustProviderLs(t)["spelled"].DefaultProtocol; got != c.wire {
				t.Fatalf("provider add --default-protocol %s 落库 = %q，期望 %q", c.spelling, got, c.wire)
			}
		})

		t.Run("set "+c.spelling, func(t *testing.T) {
			sandbox(t)
			mustInit(t)

			if code, _, stderr := run(t, "provider", "add", "spelled",
				"--base-url", "https://api.example.com",
				"--anthropic", "/v1/messages",
				"--openai", "/v1/chat/completions",
				"--responses", "/v1/responses"); code != 0 {
				t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
			}
			code, _, stderr := run(t, "provider", "set", "spelled", "--default-protocol", c.spelling)
			if code != 0 {
				t.Fatalf("provider set --default-protocol %s 退出码 = %d，stderr = %s", c.spelling, code, stderr)
			}
			if got := mustProviderLs(t)["spelled"].DefaultProtocol; got != c.wire {
				t.Fatalf("provider set --default-protocol %s 落库 = %q，期望 %q", c.spelling, got, c.wire)
			}
		})
	}
}

// TestDefaultProtocolOutsideRoutesRejected 钉住「指不到路由就整笔拒绝、不落库」：
// add 时厂商行与路由都不写，set 时原有行一个字段都不动，两处都退出码 1。
func TestDefaultProtocolOutsideRoutesRejected(t *testing.T) {
	sandbox(t)
	mustInit(t)

	t.Run("add", func(t *testing.T) {
		code, out, stderr := run(t, "provider", "add", "nope",
			"--base-url", "https://api.example.com",
			"--openai", "/v1/chat/completions",
			"--default-protocol", "responses")
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1", code)
		}
		if out != "" {
			t.Fatalf("拒绝写库时不该往 stdout 写结果: %s", out)
		}
		if !strings.Contains(stderr, "openai-responses") {
			t.Fatalf("stderr 应说明缺 responses 路由: %s", stderr)
		}
		if _, ok := mustProviderLs(t)["nope"]; ok {
			t.Fatal("整笔拒绝后不该留下厂商行")
		}
	})

	t.Run("set", func(t *testing.T) {
		if code, _, stderr := run(t, "provider", "add", "mine",
			"--base-url", "https://api.example.com",
			"--openai", "/v1/chat/completions"); code != 0 {
			t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
		}
		before := mustProviderLs(t)["mine"]

		code, out, stderr := run(t, "provider", "set", "mine", "--default-protocol", "anthropic")
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1", code)
		}
		if out != "" {
			t.Fatalf("拒绝写库时不该往 stdout 写结果: %s", out)
		}
		for _, want := range []string{"anthropic", "openai-chat"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
		after := mustProviderLs(t)["mine"]
		if after.DefaultProtocol != before.DefaultProtocol || len(after.Protocols) != 1 {
			t.Fatalf("整笔拒绝后不该改动厂商: %+v", after)
		}
	})
}
