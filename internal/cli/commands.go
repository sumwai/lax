package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"switchboard/internal/config"
	"switchboard/internal/gateway"
	"switchboard/internal/secret"
	"switchboard/internal/store"
)

var commandTable = map[string]command{
	"init":    {name: "init", summary: "初始化配置目录与数据库", run: cmdInit},
	"serve":   {name: "serve", summary: "启动网关，前台运行", run: cmdServe},
	"account": {name: "account", summary: "管理上游账号：add / ls / rm / test / enable / disable", run: cmdAccount},
	"model":   {name: "model", summary: "列出网关可提供的模型", run: cmdModel},
	"usage":   {name: "usage", summary: "按账号、模型或日期汇总用量", run: cmdUsage},
	"log":     {name: "log", summary: "查看请求记录：tail", run: cmdLog},
	"call":    {name: "call", summary: "手工打一条请求，打印选号过程", run: cmdCall},
	"doctor":  {name: "doctor", summary: "自检配置、目录与端口", run: cmdDoctor},
	"version": {name: "version", summary: "打印版本", run: cmdVersion},
}

var commandOrder = []string{"init", "serve", "account", "model", "usage", "log", "call", "doctor", "version"}

func cmdInit(e *Env, args []string) error {
	fs := newFlagSet(e, "init", "初始化配置目录与数据库")
	force := fs.Bool("force", false, "覆盖已有配置文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := config.Resolve(e.Config, e.DB)
	if err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(paths.Config), filepath.Dir(paths.DB)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	_, statErr := os.Stat(paths.Config)
	if errors.Is(statErr, os.ErrNotExist) || *force {
		if err := config.Save(paths, config.Default(paths)); err != nil {
			return err
		}
	}

	st, err := store.Open(paths.DB)
	if err != nil {
		return fmt.Errorf("创建数据库失败: %w", err)
	}
	_ = st.Close()

	keyPath := filepath.Join(filepath.Dir(paths.DB), "secret.key")
	if _, err := secret.LoadOrCreateKey(keyPath); err != nil {
		return fmt.Errorf("创建密钥失败: %w", err)
	}

	if e.JSON {
		return e.printJSON(map[string]string{
			"config": paths.Config,
			"db":     paths.DB,
			"key":    keyPath,
		})
	}
	fmt.Fprintf(e.Stdout, "配置 %s\n数据库 %s\n密钥 %s\n", paths.Config, paths.DB, keyPath)
	fmt.Fprintln(e.Stdout, "下一步：switchboard account add <名称> --provider <厂商> --key <key>")
	return nil
}

func cmdServe(e *Env, args []string) error {
	fs := newFlagSet(e, "serve", "启动网关，前台运行")
	addr := fs.String("addr", "", "监听地址，留空用配置里的值")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	listen := a.cfg.Addr
	if *addr != "" {
		listen = *addr
	}

	gs, err := gateway.New(gateway.Options{
		Store: a.st,
		Key:   a.key,
		Logf: func(format string, logArgs ...any) {
			fmt.Fprintf(e.Stdout, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, logArgs...))
		},
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", listen, err)
	}

	accounts, err := a.st.ListAccounts()
	if err != nil {
		return err
	}
	models := map[string]bool{}
	for _, acc := range accounts {
		for _, m := range acc.Models {
			models[m] = true
		}
	}

	if e.JSON {
		if err := e.printJSON(map[string]any{
			"addr":     ln.Addr().String(),
			"accounts": len(accounts),
			"models":   len(models),
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(e.Stdout, "监听 %s\n入站 /v1/messages（anthropic）\n上游 %d 个账号，%d 个模型\n",
			ln.Addr().String(), len(accounts), len(models))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return gs.Serve(ctx, ln)
}

func cmdVersion(e *Env, args []string) error {
	fs := newFlagSet(e, "version", "打印版本")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if e.JSON {
		return e.printJSON(map[string]string{"name": appName, "version": version})
	}
	fmt.Fprintf(e.Stdout, "%s %s\n", appName, version)
	return nil
}

// check 是 doctor 的一项检查结果。
type check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Note string `json:"note"`
}

func cmdDoctor(e *Env, args []string) error {
	fs := newFlagSet(e, "doctor", "自检配置、目录与端口")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := config.Resolve(e.Config, e.DB)
	if err != nil {
		return err
	}

	var checks []check
	add := func(name string, ok bool, note string) {
		checks = append(checks, check{Name: name, OK: ok, Note: note})
	}

	if _, statErr := os.Stat(paths.Config); statErr == nil {
		add("配置文件", true, paths.Config)
	} else {
		add("配置文件", false, "不存在，先运行 switchboard init")
	}

	dataDir := filepath.Dir(paths.DB)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		add("数据目录可写", false, err.Error())
	} else if f, err := os.CreateTemp(dataDir, ".doctor-*"); err != nil {
		add("数据目录可写", false, err.Error())
	} else {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		add("数据目录可写", true, dataDir)
	}

	keyPath := filepath.Join(dataDir, "secret.key")
	if _, statErr := os.Stat(keyPath); statErr == nil {
		add("加密密钥", true, "权限 0600")
	} else {
		add("加密密钥", false, "不存在，先运行 switchboard init")
	}

	if cfg, loadErr := config.Load(paths); loadErr != nil {
		add("配置解析", false, loadErr.Error())
	} else {
		add("配置解析", true, cfg.Addr)
		if ln, lerr := net.Listen("tcp", cfg.Addr); lerr != nil {
			add("端口", false, lerr.Error())
		} else {
			_ = ln.Close()
			add("端口", true, cfg.Addr)
		}
	}

	if st, openErr := store.Open(paths.DB); openErr != nil {
		add("数据库", false, openErr.Error())
	} else {
		accounts, listErr := st.ListAccounts()
		_ = st.Close()
		if listErr != nil {
			add("数据库", false, listErr.Error())
		} else {
			add("数据库", true, fmt.Sprintf("%d 个账号", len(accounts)))
		}
	}

	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}

	if e.JSON {
		if err := e.printJSON(checks); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "fail"
			}
			fmt.Fprintf(e.Stdout, "%s  %s %s\n", mark, pad(c.Name, 12), c.Note)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d 项检查未通过", failed)
	}
	return nil
}
