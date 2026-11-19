package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"lax/internal/config"
	"lax/internal/secret"
	"lax/internal/store"
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
			return nil, errors.New("尚未初始化，先运行 lax init")
		}
		return nil, fmt.Errorf("读取配置 %s 失败: %w", paths.Config, err)
	}
	if requireInit {
		if _, err := os.Stat(paths.DB); err != nil {
			return nil, errors.New("数据库不存在，先运行 lax init")
		}
	}
	// Path 用 paths.DB 而不是 cfg.DB：--db 覆盖的是 paths，配置里的 db 字段只在 init 时写入。
	st, err := store.OpenWith(store.Options{Driver: cfg.Driver, DSN: cfg.DSN, Path: paths.DB})
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
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
	u := newUI(e.Stderr)
	return func(format string, args ...any) {
		fmt.Fprintf(e.Stderr, "%s %s\n", u.warn("警告:"), fmt.Sprintf(format, args...))
	}
}

func lookupEnv(name string) string { return os.Getenv(name) }
