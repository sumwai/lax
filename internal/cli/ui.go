package cli

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// uiStyle 负责人读输出的终端样式。颜色只在真正的终端上开启，
// 且尊重 NO_COLOR 环境变量；管道、重定向与测试 buffer 均输出纯文本。
type uiStyle struct {
	color bool
}

func newUI(w io.Writer) uiStyle {
	if os.Getenv("NO_COLOR") != "" {
		return uiStyle{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return uiStyle{}
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return uiStyle{}
	}
	return uiStyle{color: true}
}

func (u uiStyle) paint(code, s string) string {
	if !u.color || code == "" || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u uiStyle) ok(s string) string   { return u.paint("32", s) }
func (u uiStyle) warn(s string) string { return u.paint("33", s) }
func (u uiStyle) err(s string) string  { return u.paint("31", s) }
func (u uiStyle) dim(s string) string  { return u.paint("90", s) }
func (u uiStyle) bold(s string) string { return u.paint("1", s) }
func (u uiStyle) info(s string) string { return u.paint("36", s) }

// isWide 判断字符在终端里占两列。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r >= 0x2e80 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6:
		return true
	}
	return false
}

// scanCSI 扫描从 s[i:] 开始的 ANSI CSI 转义序列（\x1b[...<final>），返回序列结束下标。
func scanCSI(s string, i int) (int, bool) {
	if i+1 >= len(s) || s[i] != '\x1b' || s[i+1] != '[' {
		return 0, false
	}
	j := i + 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j >= len(s) {
		return 0, false
	}
	return j + 1, true
}

// displayWidth 计算字符串在终端里的可见显示宽度，忽略 ANSI CSI 转义序列。
func displayWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if end, ok := scanCSI(s, i); ok {
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if isWide(r) {
			w += 2
		} else {
			w++
		}
		i += size
	}
	return w
}

func pad(s string, w int) string {
	d := w - displayWidth(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}

func padLeft(s string, w int) string {
	d := w - displayWidth(s)
	if d <= 0 {
		return s
	}
	return strings.Repeat(" ", d) + s
}

// truncate 按显示宽度截断，透传 ANSI CSI 序列并在发生截断时补齐重置序列。
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	hasColor := false
	for i := 0; i < len(s); {
		if end, ok := scanCSI(s, i); ok {
			seq := s[i:end]
			b.WriteString(seq)
			if seq == "\x1b[0m" || seq == "\x1b[m" {
				hasColor = false
			} else {
				hasColor = true
			}
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := 1
		if isWide(r) {
			rw = 2
		}
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
		i += size
	}
	b.WriteString("…")
	if hasColor {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

type tableAlign int

const (
	alignLeft tableAlign = iota
	alignRight
)

type tableSpec struct {
	Header    []string
	Align     []tableAlign
	Rows      [][]string
	Footer    [][]string
	Style     uiStyle
	HeaderSep bool
}

// renderTable 输出按列对齐的表格，第一行是表头。
func renderTable(header []string, rows [][]string) string {
	return renderTableWithSpec(tableSpec{
		Header: header,
		Rows:   rows,
	})
}

// renderTableWithSpec 按列规范输出对齐表格，支持右对齐、表头分隔与汇总行分隔。
func renderTableWithSpec(spec tableSpec) string {
	if len(spec.Header) == 0 {
		return ""
	}
	cols := len(spec.Header)
	widths := make([]int, cols)
	measure := func(row []string) {
		for i := 0; i < cols && i < len(row); i++ {
			if w := displayWidth(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(spec.Header)
	for _, row := range spec.Rows {
		measure(row)
	}
	for _, row := range spec.Footer {
		measure(row)
	}

	alignAt := func(i int) tableAlign {
		if i < len(spec.Align) {
			return spec.Align[i]
		}
		return alignLeft
	}

	writeRow := func(b *strings.Builder, row []string, style func(string) string) {
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			var formatted string
			if alignAt(i) == alignRight {
				formatted = padLeft(cell, widths[i])
			} else if i < cols-1 {
				formatted = pad(cell, widths[i])
			} else {
				formatted = cell
			}
			if style != nil {
				formatted = style(formatted)
			} else if spec.Style.color && isPlaceholderCell(cell) {
				formatted = spec.Style.dim(formatted)
			}
			b.WriteString(formatted)
			if i < cols-1 {
				b.WriteString("  ")
			}
		}
		b.WriteByte('\n')
	}

	writeSep := func(b *strings.Builder) {
		for i := 0; i < cols; i++ {
			sep := strings.Repeat("─", widths[i])
			b.WriteString(spec.Style.dim(sep))
			if i < cols-1 {
				b.WriteString("  ")
			}
		}
		b.WriteByte('\n')
	}

	var b strings.Builder
	writeRow(&b, spec.Header, spec.Style.bold)
	if spec.HeaderSep {
		writeSep(&b)
	}
	for _, row := range spec.Rows {
		writeRow(&b, row, nil)
	}
	if len(spec.Footer) > 0 {
		writeSep(&b)
		for _, row := range spec.Footer {
			writeRow(&b, row, spec.Style.bold)
		}
	}
	return b.String()
}

func isPlaceholderCell(s string) bool {
	switch strings.TrimSpace(s) {
	case "-", "未设置", "未配置", "无":
		return true
	}
	return false
}

type kvRow struct {
	label string
	value string
	color string
}

func renderKV(b *strings.Builder, u uiStyle, indent, labelWidth int, rows []kvRow) {
	prefix := strings.Repeat(" ", indent)
	for _, row := range rows {
		if row.value == "" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(u.dim(pad(row.label, labelWidth)))
		b.WriteString("  ")
		if row.color == "" && isPlaceholderCell(row.value) {
			b.WriteString(u.dim(row.value))
		} else {
			b.WriteString(u.paint(row.color, row.value))
		}
		b.WriteByte('\n')
	}
}

// hintError 将失败原因与建议操作分层展示，避免超长单行报错。
type hintError struct {
	reason    string
	nextSteps []string
}

func (e hintError) Error() string {
	var b strings.Builder
	b.WriteString(e.reason)
	if len(e.nextSteps) == 1 {
		b.WriteString("\n  建议: ")
		b.WriteString(e.nextSteps[0])
	} else if len(e.nextSteps) > 1 {
		b.WriteString("\n  建议:")
		for _, s := range e.nextSteps {
			b.WriteString("\n    - ")
			b.WriteString(s)
		}
	}
	return b.String()
}

func (e hintError) formatWithUI(u uiStyle) string {
	var b strings.Builder
	b.WriteString(e.reason)
	if len(e.nextSteps) == 1 {
		b.WriteString("\n")
		b.WriteString(u.dim("  建议: " + e.nextSteps[0]))
	} else if len(e.nextSteps) > 1 {
		b.WriteString("\n")
		b.WriteString(u.dim("  建议:"))
		for _, s := range e.nextSteps {
			b.WriteString("\n")
			b.WriteString(u.dim("    - " + s))
		}
	}
	return b.String()
}

func formatHintError(reason string, nextSteps ...string) error {
	return hintError{reason: reason, nextSteps: nextSteps}
}
