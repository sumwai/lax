package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// headerRow 是 provider header ls --json 的一项。
type headerRow struct {
	Provider    string `json:"provider"`
	Protocol    string `json:"protocol"`
	Name        string `json:"name"`
	Value       string `json:"value"`
	ValueMasked bool   `json:"value_masked"`
}

// mustHeaderRows 解析 provider header ls --json。
func mustHeaderRows(t *testing.T, args ...string) []headerRow {
	t.Helper()
	full := append([]string{"provider", "header", "ls"}, args...)
	full = append(full, "--json")
	code, out, stderr := run(t, full...)
	if code != 0 {
		t.Fatalf("%v 退出码 = %d，stderr = %s", full, code, stderr)
	}
	var rows []headerRow
	mustJSON(t, out, &rows)
	return rows
}

// headerCell 在 provider header ls 的人读表格里找一家厂商的一行，按列拆成字段。
func headerCell(t *testing.T, out, provider, name string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == provider && fields[2] == name {
			return fields
		}
	}
	t.Fatalf("provider header ls 文本输出里没有 %s 的 %s: %s", provider, name, out)
	return nil
}

// mustSeedOpencode 造一家 opencode 厂商，附加头的用例都在它上面跑。
func mustSeedOpencode(t *testing.T) {
	t.Helper()
	if code, _, stderr := run(t, "provider", "add", "opencode",
		"--base-url", "https://opencode.example.com",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add opencode 退出码 = %d，stderr = %s", code, stderr)
	}
}

// TestProviderHeaderSetTextAndJSON 覆盖 set 的两条输出：文本一行确认加脱敏值，
// --json 的 value 同样脱敏、value_masked 为 true（值可能是密钥，读输出的人不能以为拿到明文）。
func TestProviderHeaderSetTextAndJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustSeedOpencode(t)

	code, out, stderr := run(t, "provider", "header", "set", "opencode", "x-opencode-session", "{session}", "--protocol", "openai-chat")
	if code != 0 {
		t.Fatalf("provider header set 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"已声明 opencode 的附加头 x-opencode-session（openai-chat）",
		"值 {ses…", // secret.Mask 只露前四位
		"switchboard provider header ls opencode",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("set 的文本输出缺 %q:\n%s", want, out)
		}
	}

	code, out, stderr = run(t, "provider", "header", "set", "opencode", "api-version", "2024-10-21", "--json")
	if code != 0 {
		t.Fatalf("provider header set --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var got headerRow
	mustJSON(t, out, &got)
	if got.Provider != "opencode" || got.Protocol != "" || got.Name != "api-version" {
		t.Fatalf("--json 的项 = %+v，期望厂商级 api-version", got)
	}
	if got.Value != "2024…" || !got.ValueMasked {
		t.Fatalf("--json 的值必须脱敏且标出 value_masked: %+v", got)
	}
	if strings.Contains(out, "2024-10-21") {
		t.Fatalf("--json 不该出现明文: %s", out)
	}

	// 覆盖语义：同一个 (厂商, 协议, 头名) 再写一次就是改值，不新增一行。
	if code, _, stderr := run(t, "provider", "header", "set", "opencode", "X-Opencode-Session", "{env:OPENCODE_SESSION}", "--protocol", "openai-chat"); code != 0 {
		t.Fatalf("覆盖声明退出码 = %d，stderr = %s", code, stderr)
	}
	rows := mustHeaderRows(t, "opencode")
	if len(rows) != 2 {
		t.Fatalf("覆盖后应有 2 条声明（厂商级 + 协议级），实际 %+v", rows)
	}
	if rows[1].Value != "{env…" {
		t.Fatalf("覆盖后协议级那条的值 = %q，期望 {env…", rows[1].Value)
	}
}

// TestProviderHeaderLsTextAndJSON 覆盖 ls 的两条输出与脱敏：
// 文本四列（厂商 / 协议 / 头名 / 值（脱敏）），厂商级协议列显示 -；
// --json 每项都带 value_masked。
func TestProviderHeaderLsTextAndJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustSeedOpencode(t)

	if code, _, stderr := run(t, "provider", "header", "set", "opencode", "x-opencode-session", "session-abcdef", "--protocol", "openai-chat"); code != 0 {
		t.Fatalf("set 协议级退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "provider", "header", "set", "opencode", "api-version", "2024-10-21"); code != 0 {
		t.Fatalf("set 厂商级退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "header", "ls")
	if code != 0 {
		t.Fatalf("provider header ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"厂商", "协议", "头名", "值（脱敏）"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider header ls 的表头缺 %q:\n%s", want, out)
		}
	}
	// 厂商级在前（协议列 -），协议级在后：同一个头名两边都有时，读者能看出哪条压过哪条。
	if got := headerCell(t, out, "opencode", "api-version"); got[1] != "-" || got[3] != "2024…" {
		t.Fatalf("厂商级那一行 = %v，期望协议列 - 且值脱敏", got)
	}
	if got := headerCell(t, out, "opencode", "x-opencode-session"); got[1] != "openai-chat" || got[3] != "sess…" {
		t.Fatalf("协议级那一行 = %v，期望协议列 openai-chat 且值脱敏", got)
	}
	if strings.Contains(out, "session-abcdef") {
		t.Fatalf("文本输出不该出现明文:\n%s", out)
	}

	rows := mustHeaderRows(t)
	if len(rows) != 2 {
		t.Fatalf("全部厂商的头应有 2 条，实际 %+v", rows)
	}
	for _, r := range rows {
		if !r.ValueMasked {
			t.Fatalf("--json 每项都要标 value_masked: %+v", r)
		}
		if strings.Contains(r.Value, "2024-10-21") || strings.Contains(r.Value, "session-abcdef") {
			t.Fatalf("--json 的值没脱敏: %+v", r)
		}
	}

	// 只看一家：另一家的头不出现。
	if code, _, stderr := run(t, "provider", "add", "other", "--base-url", "https://other.example.com", "--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add other 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "provider", "header", "set", "other", "x-other", "value-1234"); code != 0 {
		t.Fatalf("set other 退出码 = %d，stderr = %s", code, stderr)
	}
	if rows = mustHeaderRows(t); len(rows) != 3 {
		t.Fatalf("不带厂商时应列全部厂商的 3 条，实际 %+v", rows)
	}
	if rows = mustHeaderRows(t, "opencode"); len(rows) != 2 {
		t.Fatalf("带厂商时应只列 opencode 的 2 条，实际 %+v", rows)
	}
}

// TestProviderHeaderProtocolOverridesProviderLevel 钉住协议级覆盖厂商级在 ls 里的呈现：
// 同一个头名两边各占一行（协议列区分），删掉协议级那行之后厂商级那条还在，转发回退到它。
func TestProviderHeaderProtocolOverridesProviderLevel(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustSeedOpencode(t)

	for _, args := range [][]string{
		{"provider", "header", "set", "opencode", "x-opencode-session", "provider-level-1"},
		{"provider", "header", "set", "opencode", "x-opencode-session", "protocol-level-1", "--protocol", "openai"},
	} {
		if code, _, stderr := run(t, args...); code != 0 {
			t.Fatalf("%v 退出码 = %d，stderr = %s", args, code, stderr)
		}
	}

	rows := mustHeaderRows(t, "opencode")
	if len(rows) != 2 {
		t.Fatalf("同名头两边各一行，实际 %+v", rows)
	}
	if rows[0].Protocol != "" || rows[0].Value != "prov…" {
		t.Fatalf("第一行应是厂商级，实际 %+v", rows[0])
	}
	if rows[1].Protocol != "openai-chat" || rows[1].Value != "prot…" {
		t.Fatalf("第二行应是协议级，实际 %+v", rows[1])
	}

	// 删掉协议级那行：厂商级还在，输出要说明转发会回退到它。
	code, out, stderr := run(t, "provider", "header", "unset", "opencode", "x-opencode-session", "--protocol", "openai")
	if code != 0 {
		t.Fatalf("unset 协议级退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"opencode 已删掉附加头 x-opencode-session（openai-chat）",
		"同名头还有厂商级的一条（值 prov…），转发时会用它",
		"现有 1 条附加头",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("unset 的输出缺 %q:\n%s", want, out)
		}
	}
	if rows = mustHeaderRows(t, "opencode"); len(rows) != 1 || rows[0].Protocol != "" {
		t.Fatalf("删掉协议级后应只剩厂商级那条，实际 %+v", rows)
	}
}

// TestProviderHeaderUnsetJSON 覆盖 unset 的 --json：说明删了哪一条、这家还剩几条。
func TestProviderHeaderUnsetJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustSeedOpencode(t)

	if code, _, stderr := run(t, "provider", "header", "set", "opencode", "x-a", "value-1234"); code != 0 {
		t.Fatalf("set 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr := run(t, "provider", "header", "unset", "opencode", "x-a", "--json")
	if code != 0 {
		t.Fatalf("unset --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var got struct {
		Provider  string `json:"provider"`
		Protocol  string `json:"protocol"`
		Name      string `json:"name"`
		Removed   bool   `json:"removed"`
		Remaining int    `json:"remaining"`
	}
	mustJSON(t, out, &got)
	if got.Provider != "opencode" || got.Name != "x-a" || !got.Removed || got.Protocol != "" {
		t.Fatalf("unset 的 --json = %+v", got)
	}
	if got.Remaining != 0 {
		t.Fatalf("删完应剩 0 条，实际 %d", got.Remaining)
	}
	// json.Number 与字段都在一个对象里：确认顶层就是对象而不是数组。
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("unset --json 应是对象: %v\n%s", err, out)
	}
}

// TestProviderHeaderUsageErrors 覆盖用法错误：非法头名、非法协议、空值、缺参数、未知子命令。
// 每条都要退出码 2、stdout 为空，并打出该子命令的用法行。
func TestProviderHeaderUsageErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustSeedOpencode(t)

	cases := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "头名带空格",
			args:  []string{"provider", "header", "set", "opencode", "x bad", "v"},
			wants: []string{"头名", "非法字符", "用法：switchboard provider header set"},
		},
		{
			name:  "头名带中文",
			args:  []string{"provider", "header", "set", "opencode", "x中文", "v"},
			wants: []string{"头名", "非法字符", "用法：switchboard provider header set"},
		},
		{
			name:  "不认识的协议",
			args:  []string{"provider", "header", "set", "opencode", "x-a", "v", "--protocol", "cohere"},
			wants: []string{"--protocol", "anthropic、openai、responses、gemini", "switchboard provider header set opencode x-opencode-session"},
		},
		{
			name:  "协议传线协议全名也认",
			args:  []string{"provider", "header", "set", "opencode", "x-a", "v", "--protocol", "openai-responses"},
			wants: nil, // 这条不是错误，单独在下面断言
		},
		{
			name:  "值为空",
			args:  []string{"provider", "header", "set", "opencode", "x-a", "  "},
			wants: []string{"值不能为空", "provider header unset", "用法：switchboard provider header set"},
		},
		{
			name:  "缺位置参数",
			args:  []string{"provider", "header", "set", "opencode", "x-a"},
			wants: []string{"用法：switchboard provider header set"},
		},
		{
			name:  "unset 缺头名",
			args:  []string{"provider", "header", "unset", "opencode"},
			wants: []string{"用法：switchboard provider header unset"},
		},
		{
			name:  "ls 多给一个厂商",
			args:  []string{"provider", "header", "ls", "a", "b"},
			wants: []string{"用法：switchboard provider header ls"},
		},
		{
			name:  "未知子命令",
			args:  []string{"provider", "header", "nope"},
			wants: []string{"未知子命令", "switchboard provider header --help"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, stderr := run(t, c.args...)
			if c.wants == nil {
				if code != 0 {
					t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
				}
				return
			}
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2，stderr = %s", code, stderr)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			for _, want := range c.wants {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr 应提到 %q: %s", want, stderr)
				}
			}
		})
	}
}

// TestProviderHeaderUnsetMissing 覆盖删不到：报错并列出这家现有的声明，退出码 1（运行失败，
// 不是用法错误——参数写得没错，是这条头没声明过），并且提示声明与查看两条下一步命令。
func TestProviderHeaderUnsetMissing(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustSeedOpencode(t)

	if code, _, stderr := run(t, "provider", "header", "set", "opencode", "x-declared", "value-1234"); code != 0 {
		t.Fatalf("set 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "header", "unset", "opencode", "x-missing")
	if code != 1 || out != "" {
		t.Fatalf("删不到时退出码 = %d，stdout = %q，期望 1 且不写 stdout", code, out)
	}
	for _, want := range []string{
		"没有声明附加头 x-missing（整个厂商）",
		"现有 x-declared（整个厂商）",
		"switchboard provider header ls opencode",
		"switchboard provider header set opencode x-missing",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	// 协议写错（厂商级那条在，但协议级那条不在）同样列出这家现有的声明。
	code, _, stderr = run(t, "provider", "header", "unset", "opencode", "x-declared", "--protocol", "openai")
	if code != 1 || !strings.Contains(stderr, "现有 x-declared（整个厂商）") {
		t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
	}
}

// TestProviderHeaderUnknownProvider 覆盖厂商不存在：set 与 ls 都报错并指向 provider ls，
// 不把「写错厂商名」显示成「一条头都没有」。
func TestProviderHeaderUnknownProvider(t *testing.T) {
	sandbox(t)
	mustInit(t)

	for _, args := range [][]string{
		{"provider", "header", "set", "nope", "x-a", "v"},
		{"provider", "header", "unset", "nope", "x-a"},
		{"provider", "header", "ls", "nope"},
	} {
		code, out, stderr := run(t, args...)
		if code != 1 || out != "" {
			t.Fatalf("%v 退出码 = %d，stdout = %q，期望 1 且不写 stdout", args, code, out)
		}
		if !strings.Contains(stderr, "switchboard provider ls") {
			t.Fatalf("%v 的报错应指向 provider ls: %s", args, stderr)
		}
	}

	// 一条头都没有时 ls 给一行提示与一条示例命令，而不是一张空表。
	code, out, stderr := run(t, "provider", "header", "ls")
	if code != 0 {
		t.Fatalf("空头表时 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"还没有声明任何附加头", "switchboard provider header set opencode x-opencode-session"} {
		if !strings.Contains(out, want) {
			t.Fatalf("空头表的输出缺 %q:\n%s", want, out)
		}
	}
	if rows := mustHeaderRows(t); len(rows) != 0 {
		t.Fatalf("空头表的 --json 应是空数组，实际 %+v", rows)
	}
}

// TestProviderHeaderHelp 覆盖 provider header --help：三条用法、一个示例，
// 以及三种占位符写法与「协议级覆盖厂商级」的说明；组级帮助与顶层帮助都要提到 header。
func TestProviderHeaderHelp(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "provider", "header", "--help")
	if code != 0 || stderr != "" {
		t.Fatalf("provider header --help 退出码 = %d，stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"switchboard provider header set <厂商> <头名> <值> [--protocol <协议>]",
		"switchboard provider header unset <厂商> <头名> [--protocol <协议>]",
		"switchboard provider header ls [<厂商>]",
		"switchboard provider header set opencode x-opencode-session '{session}' --protocol openai-chat",
		"{session}", "{env:NAME}", "{model}",
		"协议级覆盖厂商级",
		"脱敏",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider header --help 缺 %q:\n%s", want, out)
		}
	}

	// 子命令自己的 --help 仍打印参数清单，退出码 0。
	code, _, stderr = run(t, "provider", "header", "set", "--help")
	if code != 0 || !strings.Contains(stderr, "--protocol") {
		t.Fatalf("set --help 退出码 = %d，stderr = %s", code, stderr)
	}

	// 组级帮助与顶层帮助都要提到 header 这一组。
	for _, args := range [][]string{{"provider", "--help"}, {"--help"}} {
		code, out, stderr := run(t, args...)
		if code != 0 {
			t.Fatalf("%v 退出码 = %d，stderr = %s", args, code, stderr)
		}
		if !strings.Contains(out, "header") {
			t.Fatalf("%v 的输出应提到 header: %s", args, out)
		}
	}
}
