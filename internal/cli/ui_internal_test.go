package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestUIStylePlainInBufferAndNoColor(t *testing.T) {
	var buf bytes.Buffer
	u := newUI(&buf)
	if u.color {
		t.Fatal("bytes.Buffer 不应启用 ANSI 颜色")
	}
	if got := u.ok("ok"); got != "ok" {
		t.Fatalf("纯文本模式不应带转义序列，实际 %q", got)
	}

	t.Setenv("NO_COLOR", "1")
	u2 := uiStyle{color: true}
	if u2.ok("ok") == "ok" {
		t.Fatal("uiStyle{color:true} 应正常着色")
	}
	if got := u2.dim("hint"); got != "\x1b[90mhint\x1b[0m" {
		t.Fatalf("u2.dim(\"hint\") = %q，期望 \\x1b[90mhint\\x1b[0m", got)
	}
	// displayWidth 应忽略 ANSI 转义序列，保证带色单元格对齐。
	colored := "\x1b[32mok\x1b[0m"
	if w := displayWidth(colored); w != 2 {
		t.Fatalf("displayWidth(%q) = %d，期望 2", colored, w)
	}
}

func TestRenderTableWithSpecAlignAndFooter(t *testing.T) {
	got := renderTableWithSpec(tableSpec{
		Header:    []string{"账号", "请求", "费用"},
		Align:     []tableAlign{alignLeft, alignRight, alignRight},
		Rows:      [][]string{{"ds1", "12", "1.50"}},
		Footer:    [][]string{{"合计", "12", "1.50"}},
		HeaderSep: true,
	})
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("期望 5 行（表头+分隔+数据+分隔+合计），实际 %d 行:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], "────") || !strings.Contains(lines[3], "────") {
		t.Fatalf("缺少表头或合计分隔线:\n%s", got)
	}
	if !strings.HasPrefix(lines[4], "合计") {
		t.Fatalf("末行应为合计: %q", lines[4])
	}
}

func TestFlagDefaultsUseChineseLabel(t *testing.T) {
	var out, errOut bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errOut}
	fs := newFlagSet(e, "log tail", "查看请求日志")
	fs.Int("n", 20, "条数")
	fs.Usage()
	if !strings.Contains(errOut.String(), "(默认: 20)") {
		t.Fatalf("flag 默认值应为 (默认: 20)，实际:\n%s", errOut.String())
	}
}

func TestFormatHintError(t *testing.T) {
	err := formatHintError("协议不支持", "改用 lax model decide", "或运行 lax model ls")
	msg := err.Error()
	if !strings.Contains(msg, "协议不支持\n  建议:\n    - 改用 lax model decide") {
		t.Fatalf("结构化错误格式不符合预期:\n%s", msg)
	}
}

func TestTruncateANSI(t *testing.T) {
	colored := "\x1b[31mabcdef\x1b[0m"
	if got := truncate(colored, 6); got != colored {
		t.Fatalf("未超宽时应原样保留，实际 %q", got)
	}
	got := truncate(colored, 4)
	want := "\x1b[31mabc…\x1b[0m"
	if got != want {
		t.Fatalf("truncate(%q, 4) = %q，期望 %q", colored, got, want)
	}
	if w := displayWidth(got); w != 4 {
		t.Fatalf("截断后可见宽度 = %d，期望 4", w)
	}
}

func TestDimPlaceholdersInTableAndKV(t *testing.T) {
	colored := renderTableWithSpec(tableSpec{
		Header: []string{"项", "值"},
		Rows:   [][]string{{"a", "-"}, {"b", "未设置"}},
		Style:  uiStyle{color: true},
	})
	if !strings.Contains(colored, "\x1b[90m-\x1b[0m") || !strings.Contains(colored, "\x1b[90m未设置\x1b[0m") {
		t.Fatalf("带色模式下占位符应使用灰色弱化:\n%q", colored)
	}

	plain := renderTableWithSpec(tableSpec{
		Header: []string{"项", "值"},
		Rows:   [][]string{{"a", "-"}, {"b", "未设置"}},
		Style:  uiStyle{color: false},
	})
	if strings.Contains(plain, "\x1b[") {
		t.Fatalf("非 TTY 模式不应包含 ANSI 转义序列:\n%q", plain)
	}
}
