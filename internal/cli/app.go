package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"switchboard/internal/config"
	"switchboard/internal/secret"
	"switchboard/internal/store"
)

// app 是一次命令执行会用到的基础依赖。
type app struct {
	paths config.Paths
	cfg   config.Config
	st    *store.Store
	key   []byte
}

// openApp 打开配置与数据库。requireInit 为真时数据库必须已存在。
func openApp(e *Env, requireInit bool) (*app, error) {
	paths, err := config.Resolve(e.Config, e.DB)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("尚未初始化，先运行 switchboard init")
		}
		return nil, fmt.Errorf("读取配置 %s 失败: %w", paths.Config, err)
	}
	if requireInit {
		if _, err := os.Stat(paths.DB); err != nil {
			return nil, errors.New("数据库不存在，先运行 switchboard init")
		}
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", paths.DB, err)
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(filepath.Dir(paths.DB), "secret.key"))
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("读取密钥失败: %w", err)
	}
	return &app{paths: paths, cfg: cfg, st: st, key: key}, nil
}

// Close 关闭数据库。
func (a *app) Close() { _ = a.st.Close() }

// baseURL 是本机网关的地址，供客户端配置使用。
func (a *app) baseURL() string { return "http://" + a.cfg.Addr }

func (a *app) keyPath() string {
	return filepath.Join(filepath.Dir(a.paths.DB), "secret.key")
}

// gatewayLogf 返回给 gateway 用的日志函数，写 stderr。gateway 只在真失败时调用它
// （写请求记录失败、停用账号失败、清冷却失败等），正常路径一行都不出，
// 所以既不会碍着人读，也不会污染 stdout 与 --json 的结果。
func (e *Env) gatewayLogf() func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(e.Stderr, "警告：%s\n", fmt.Sprintf(format, args...))
	}
}

func lookupEnv(name string) string { return os.Getenv(name) }

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

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
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

// renderTable 输出按列对齐的表格，第一行是表头。
func renderTable(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && displayWidth(cell) > widths[i] {
				widths[i] = displayWidth(cell)
			}
		}
	}

	var b strings.Builder
	for i, h := range header {
		b.WriteString(pad(h, widths[i]+2))
	}
	b.WriteString("\n")
	for _, row := range rows {
		for i, cell := range row {
			b.WriteString(pad(cell, widths[i]+2))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// truncate 按显示宽度截断，超出补省略号。
func truncate(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := 1
		if isWide(r) {
			rw = 2
		}
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}
