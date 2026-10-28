package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"switchboard/internal/config"
)

var commandTable = map[string]command{
	"init":    {name: "init", summary: "初始化配置目录与数据库", run: cmdInit},
	"serve":   {name: "serve", summary: "启动网关，前台运行", run: cmdServe},
	"account": {name: "account", summary: "管理上游账号：add / ls / rm / test", run: cmdAccount},
	"model":   {name: "model", summary: "列出网关可提供的模型", run: cmdModel},
	"agent":   {name: "agent", summary: "把其它客户端指向本网关：ls / use / revert", run: cmdAgent},
	"usage":   {name: "usage", summary: "按账号、模型或日期汇总用量", run: cmdUsage},
	"log":     {name: "log", summary: "查看请求记录：tail", run: cmdLog},
	"call":    {name: "call", summary: "手工打一条请求，打印选号过程", run: cmdCall},
	"doctor":  {name: "doctor", summary: "自检配置、目录与端口", run: cmdDoctor},
	"version": {name: "version", summary: "打印版本", run: cmdVersion},
}

var commandOrder = []string{"init", "serve", "account", "model", "agent", "usage", "log", "call", "doctor", "version"}

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

	if e.JSON {
		return e.printJSON(map[string]string{"config": paths.Config, "db": paths.DB})
	}
	fmt.Fprintf(e.Stdout, "配置 %s\n数据库 %s\n", paths.Config, paths.DB)
	fmt.Fprintln(e.Stdout, "下一步：switchboard account add <名称> --provider <厂商> --key <key>")
	return nil
}

func cmdServe(e *Env, args []string) error {
	fs := newFlagSet(e, "serve", "启动网关，前台运行")
	addr := fs.String("addr", "", "监听地址，留空用配置里的值")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = addr
	return errNotImplemented
}

func cmdAccount(e *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("需要子命令：add | ls | rm | test")
	}
	switch args[0] {
	case "add":
		fs := newFlagSet(e, "account add", "新增一个上游账号")
		provider := fs.String("provider", "", "上游厂商标识，如 deepseek")
		key := fs.String("key", "", "上游 API key")
		protocol := fs.String("protocol", "openai-chat", "上游线协议：openai-chat | anthropic")
		baseURL := fs.String("base-url", "", "上游地址，留空用厂商默认")
		models := fs.String("models", "", "该账号可提供的模型，逗号分隔")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("用法：switchboard account add <名称> --provider <厂商> --key <key>")
		}
		if *provider == "" || *key == "" {
			return errors.New("--provider 与 --key 都是必填")
		}
		_, _, _ = protocol, baseURL, models
		return errNotImplemented
	case "ls":
		return errNotImplemented
	case "rm":
		return errNotImplemented
	case "test":
		return errNotImplemented
	default:
		return fmt.Errorf("未知子命令 %q，可用：add | ls | rm | test", args[0])
	}
}

func cmdModel(e *Env, args []string) error {
	if len(args) == 0 || args[0] != "ls" {
		return errors.New("需要子命令：ls")
	}
	return errNotImplemented
}

func cmdAgent(e *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("需要子命令：ls | use | revert")
	}
	switch args[0] {
	case "ls", "use", "revert":
		return errNotImplemented
	default:
		return fmt.Errorf("未知子命令 %q，可用：ls | use | revert", args[0])
	}
}

func cmdUsage(e *Env, args []string) error {
	return errNotImplemented
}

func cmdLog(e *Env, args []string) error {
	if len(args) == 0 || args[0] != "tail" {
		return errors.New("需要子命令：tail")
	}
	return errNotImplemented
}

func cmdCall(e *Env, args []string) error {
	return errNotImplemented
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
		f.Close()
		os.Remove(name)
		add("数据目录可写", true, dataDir)
	}

	if _, statErr := os.Stat(paths.DB); statErr == nil {
		add("数据库文件", true, paths.DB)
	} else {
		add("数据库文件", false, "尚未创建")
	}

	cfg, loadErr := config.Load(paths)
	if loadErr != nil {
		add("配置解析", false, loadErr.Error())
	} else {
		add("配置解析", true, cfg.Addr)
		if ln, lerr := net.Listen("tcp", cfg.Addr); lerr != nil {
			add("端口", false, lerr.Error())
		} else {
			ln.Close()
			add("端口", true, cfg.Addr)
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
			fmt.Fprintf(e.Stdout, "%s  %-14s %s\n", mark, c.Name, c.Note)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d 项检查未通过", failed)
	}
	return nil
}
