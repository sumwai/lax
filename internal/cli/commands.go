package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lax/internal/config"
	"lax/internal/gateway"
	"lax/internal/secret"
	"lax/internal/store"
)

var commandTable = map[string]command{
	"init":       {name: "init", summary: "初始化配置目录与数据库", run: cmdInit},
	"serve":      {name: "serve", summary: "启动网关，前台运行", run: cmdServe},
	"account":    {name: "account", summary: "管理上游账号：add / ls / rm / test / enable / disable", run: cmdAccount},
	"model":      {name: "model", summary: "模型：登记、同步、探活、别名、单价与手工试打（call / decide / speak / transcribe）", run: cmdModel},
	"provider":   {name: "provider", summary: "厂商与路由：ls / add / set / unset / rm / header", run: cmdProvider},
	"plugin":     {name: "plugin", summary: "插件：install / ls / show / enable / disable / rm / doctor / reload", run: cmdPlugin},
	"usage":      {name: "usage", summary: "按账号、模型或日期汇总用量", run: cmdUsage},
	"log":        {name: "log", summary: "查看请求记录：tail", run: cmdLog},
	"doctor":     {name: "doctor", summary: "自检配置、目录与端口", run: cmdDoctor},
	"version":    {name: "version", summary: "打印版本", run: cmdVersion},
}

var commandOrder = []string{"init", "serve", "account", "model", "provider", "plugin", "usage", "log", "doctor", "version"}

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

	// 用配置里的后端建库：--force 之外可能指向一个外部数据库，那一步不在 init 里做。
	cfg, err := config.Load(paths)
	if err != nil {
		return fmt.Errorf("读取配置 %s 失败: %w", paths.Config, err)
	}
	st, err := store.OpenWith(store.Options{Driver: cfg.Driver, DSN: cfg.DSN, Path: paths.DB})
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
	fmt.Fprintln(e.Stdout, "下一步：lax account add <名称> --provider <厂商> --key <key>")
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
	dataDir := filepath.Dir(a.paths.DB)

	// 插件加载失败只提示一句：一个装坏的插件不该让网关起不来，别的厂商还在用。
	dialects, notes, err := loadRegistry(a.st, dataDir, e.Stdout)
	if err != nil {
		return fmt.Errorf("读插件表失败: %w", err)
	}
	for _, note := range notes {
		fmt.Fprintf(e.Stderr, "警告：%s\n", note)
	}
	// 语音能力位是手工声明的，启动时补一次方言绑定：漏绑的模型在语音入站会被挡下来。
	// 这一步失败只记一行，管理员仍能用 lax model capability 手工绑。
	if added, err := a.st.BackfillSpeechDialects(time.Now().Unix()); err != nil {
		fmt.Fprintf(e.Stderr, "警告：回填语音方言绑定失败：%v\n", err)
	} else if added > 0 && !e.JSON {
		fmt.Fprintf(e.Stdout, "补齐语音方言绑定 %d 条\n", added)
	}
	// route_stats 只服务选号，不是审计表：启动时丢掉 30 天以前的分钟桶，requests 仍保留完整历史。
	if _, err := a.st.PruneRouteStats(time.Now().Unix() - store.RouteStatsRetentionSeconds); err != nil {
		fmt.Fprintf(e.Stderr, "警告：清理选号统计失败：%v\n", err)
	}

	gs, err := gateway.New(gateway.Options{
		Store:    a.st,
		Key:      a.key,
		Dialects: dialects,
		Logf: func(format string, logArgs ...any) {
			fmt.Fprintf(e.Stdout, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, logArgs...))
		},
	})
	if err != nil {
		return err
	}

	// pid 文件写在监听之前，退出时删掉：`plugin reload` 凭它找进程。
	// 监听失败时这个 defer 也会把它清掉，不会留下指向已退出进程的文件。
	pidPath := servePidPath(dataDir)
	if err := writeServePid(pidPath); err != nil {
		return err
	}
	defer func() {
		// 只删自己写的那一个：同一个地址起第二个 serve 会先覆盖 pid 文件再在监听上失败，
		// 它退出时不该把第一个 serve 的文件删掉。
		if pid, err := readServePid(pidPath); err == nil && pid == os.Getpid() {
			_ = os.Remove(pidPath)
		}
	}()

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
		fmt.Fprintf(e.Stdout, "监听 %s\n入站 /v1/messages（anthropic） /v1/chat/completions（openai-chat） /v1/responses（openai-responses）\n上游 %d 个账号，%d 个模型\n",
			ln.Addr().String(), len(accounts), len(models))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP 热加载：重建注册表并整表换掉，在途请求手里拿的仍是换表前取到的方言。
	// 失败只打一行日志、当前注册表一个字不动：一次手滑不该让在跑的服务断掉。
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			reloaded, reloadNotes, err := loadRegistry(a.st, dataDir, e.Stdout)
			if err != nil {
				fmt.Fprintf(e.Stderr, "警告：重读插件表失败，继续用当前注册表：%v\n", err)
				continue
			}
			gs.SetDialects(reloaded)
			for _, note := range reloadNotes {
				fmt.Fprintf(e.Stderr, "警告：%s\n", note)
			}
			auditPluginReload(e, a.st, pluginActorSIGHUP, "")
		}
	}()

	return gs.Serve(ctx, ln)
}

// servePidName 是 serve 的 pid 文件名。它放在数据目录里：`plugin reload` 按同一个
// 数据目录找它，--config / --db 指到哪就跟到哪，不会发错进程。
const servePidName = "lax.pid"

func servePidPath(dataDir string) string { return filepath.Join(dataDir, servePidName) }

// writeServePid 写下当前进程号（带换行），权限 0600：同一台机器上的其他用户不该知道
// 哪条命令能敲一下就让网关换表。
//
// os.WriteFile 只在新建时用给定权限，已存在的文件保留旧权限，所以写完再 chmod 一次。
func writeServePid(path string) error {
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("写 pid 文件 %s 失败：%v；检查数据目录的权限", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("把 pid 文件 %s 的权限改成 0600 失败：%v", path, err)
	}
	return nil
}

// readServePid 读 pid 文件。文件不存在或内容不是进程号时都说清楚下一步该做什么：
// 这两种情况都只能靠人动手，猜一个进程号发信号更糟。
func readServePid(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("没有 pid 文件 %s：lax serve 没在跑，先在一个终端里 lax serve（后台跑用 nohup lax serve &）", path)
	}
	if err != nil {
		return 0, fmt.Errorf("读 pid 文件 %s 失败：%v", path, err)
	}
	text := strings.TrimSpace(string(raw))
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pid 文件 %s 里不是进程号（%q）：删掉它再启动 lax serve", path, text)
	}
	return pid, nil
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

// legacyDirName 是改名前的默认目录名，改名前的库文件名是它加 .db。
// 改名只换了默认目录，老用户的数据还留在旧路径下，doctor 用它判断要不要提示搬家：
// 运行时不读这个位置，别的地方也不做兼容分支。
const legacyDirName = "switchboard"

// legacyDB 按与 config 相同的 XDG 约定拼出改名前的默认库文件位置。
func legacyDB() string {
	base := ""
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		base = filepath.Join(d, legacyDirName)
	} else if home, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(home, ".local", "share", legacyDirName)
	} else {
		return ""
	}
	return filepath.Join(base, legacyDirName+".db")
}

// legacyConfig 按与 config 相同的 XDG 约定拼出改名前的配置文件位置。
// 只给 doctor 用：配置文件里存的是库地址，只搬库不搬它，新名字下仍指向旧路径。
func legacyConfig() string {
	base := ""
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		base = filepath.Join(d, legacyDirName)
	} else if home, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(home, ".config", legacyDirName)
	} else {
		return ""
	}
	return filepath.Join(base, "config.json")
}

// legacyMoveNote 在「新库是空的、旧目录还有数据」时给出一条可执行的搬家命令，否则返回空串。
//
// 命令写成逐个 mv 而不是 mv 多个文件到目录：旧目录里的文件未必齐全（有人只搬过库），
// 缺一个就整条 mv 失败，比列清楚哪一个该到哪更难看。配置也一并提：它里面存的是库地址，
// 只搬库不搬配置，新名字下仍指向旧路径。
func legacyMoveNote(paths config.Paths, dataDir string) string {
	oldDB := legacyDB()
	if oldDB == "" {
		return ""
	}
	if _, err := os.Stat(oldDB); err != nil {
		return ""
	}

	oldDir := filepath.Dir(oldDB)
	moves := []string{fmt.Sprintf("mv %s %s", oldDB, dataDir)}
	hasKey := true
	if _, err := os.Stat(filepath.Join(oldDir, "secret.key")); err != nil {
		hasKey = false
	} else {
		moves = append(moves, fmt.Sprintf("mv %s %s", filepath.Join(oldDir, "secret.key"), dataDir))
	}
	if oldConfig := legacyConfig(); oldConfig != "" {
		if _, err := os.Stat(oldConfig); err == nil {
			moves = append(moves, fmt.Sprintf("mv %s %s", oldConfig, filepath.Dir(paths.Config)))
		}
	}
	note := fmt.Sprintf("新库是空的，旧目录还有数据：%s（账号、单价与用量都还在）", strings.Join(moves, "；"))
	if !hasKey {
		note += fmt.Sprintf("；旧目录里没有 %s，密钥丢了旧凭据解不开，账号要重新录 key", filepath.Join(oldDir, "secret.key"))
	}
	return note
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
		add("配置文件", false, "不存在，先运行 lax init")
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
		add("加密密钥", false, "不存在，先运行 lax init")
	}

	// 新库还没建、旧目录里的库还在：数据其实没丢，只是留在改名前的路径下。
	// 这一项报 fail 而不是静默通过：现有检查项只有 ok/fail 两级（没有 warn），
	// 而新库为空时用户看到的是空账号、空用量，正是需要动手才能恢复的状态。
	// 判断放在打开数据库之后用「账号数为 0」而不是「新库文件不存在」：doctor 自己就会把
	// 新库建出来，按文件存在与否判，第一次运行之后提示就永久消失了，而用户看到的一直是空账号。

	cfg, cfgErr := config.Load(paths)
	if cfgErr != nil {
		add("配置解析", false, cfgErr.Error())
	} else {
		add("配置解析", true, cfg.Addr)
		if ln, lerr := net.Listen("tcp", cfg.Addr); lerr != nil {
			add("端口", false, lerr.Error())
		} else {
			_ = ln.Close()
			add("端口", true, cfg.Addr)
		}
	}

	if st, openErr := store.OpenWith(store.Options{Driver: cfg.Driver, DSN: cfg.DSN, Path: paths.DB}); openErr != nil {
		add("数据库", false, openErr.Error())
	} else {
		accounts, listErr := st.ListAccounts()
		_ = st.Close()
		if listErr != nil {
			add("数据库", false, listErr.Error())
		} else {
			add("数据库", true, fmt.Sprintf("%d 个账号", len(accounts)))
			if len(accounts) == 0 {
				if note := legacyMoveNote(paths, dataDir); note != "" {
					add("旧数据搬迁", false, note)
				}
			}
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
