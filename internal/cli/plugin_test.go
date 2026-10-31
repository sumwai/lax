package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/config"
	"lax/internal/plugin"
	"lax/internal/store/storetest"
)

// 插件命令面：用法错误、安装流水线、启停、卸载、dry-run 与 doctor 的完整性检查。
// 用例只碰本地目录与沙箱里的库，不联网。

const pluginFixtureManifest = `{
  "schema": "lax.plugin/v1",
  "id": "com.example.echo",
  "name": "Echo",
  "version": "1.0.0",
  "kind": "provider",
  "runtime": "declarative",
  "abi": "lax.plugin/v1",
  "requires": ["operation:speech.synthesize@1"],
  "permissions": {
    "network": {"mode": "none", "schemes": [], "hosts": [], "redirects": "none"},
    "secrets": {"plugin": [], "account": ["api_key"], "mode": "host-inject"},
    "filesystem": "none",
    "process": "none",
    "env": "none"
  },
  "providers": [
    {
      "id": "echo",
      "name": "Echo",
      "base_url": "https://api.echo.test",
      "catalog": "./models.json",
      "dialects": "./dialects.json"
    }
  ]
}
`

const pluginFixtureDialects = `{
  "schema": "lax.dialect/v1",
  "dialects": [
    {
      "id": "com.example.echo/speech@1",
      "operation": "speech.synthesize@1",
      "display_name": "Echo TTS",
      "upstream_wire": "openai-speech",
      "transport": {"request": "json", "response": "binary", "streaming": "none"},
      "implementation": {"prepare": "declarative", "decode": "declarative"},
      "request": {
        "method": "POST",
        "path": "/v1/audio/speech",
        "body": {
          "model": {"model": "upstream_id"},
          "input": {"input": "text"},
          "voice": {"input": "voice", "default": "alloy", "omit_if_empty": true}
        }
      },
      "response": {
        "mode": "passthrough",
        "mime": {"input": "format", "map": {"mp3": "audio/mpeg", "wav": "audio/wav"}}
      }
    }
  ]
}
`

const pluginFixtureModels = `{
  "models": [
    {
      "model": "echo-tts",
      "upstream_id": "echo-tts",
      "operations": {
        "speech.synthesize@1": {
          "dialect": "com.example.echo/speech@1",
          "capabilities": {
            "supported": true,
            "streaming": false,
            "formats": ["mp3", "wav"],
            "voices": ["alloy"]
          }
        }
      }
    }
  ]
}
`

// writePluginFixture 在临时目录里写一个最小可用的声明式插件包。
func writePluginFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"lax.plugin.json": pluginFixtureManifest,
		"dialects.json":   pluginFixtureDialects,
		"models.json":     pluginFixtureModels,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写夹具 %s: %v", name, err)
		}
	}
	return dir
}

// bumpFixtureVersion 换掉夹具的版本号：同一版本不能覆盖，升级要换号。
func bumpFixtureVersion(t *testing.T, dir, version string) {
	t.Helper()
	path := filepath.Join(dir, "lax.plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读夹具 manifest: %v", err)
	}
	updated := strings.Replace(string(raw), `"version": "1.0.0"`, `"version": "`+version+`"`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("改夹具版本: %v", err)
	}
}

// pluginDataDir 是沙箱里的数据目录，插件包挂在它下面的 plugins/。
func pluginDataDir(t *testing.T) string {
	t.Helper()
	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析数据路径: %v", err)
	}
	return filepath.Dir(paths.DB)
}

// pluginInstalledDir 是一个已装版本的目录。
func pluginInstalledDir(t *testing.T, id, version string) string {
	t.Helper()
	return filepath.Join(pluginDataDir(t), "plugins", "installed", id, version)
}

// pluginLsRow 是 plugin ls --json 的一行。
type pluginLsRow struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Kind         string   `json:"kind"`
	Runtime      string   `json:"runtime"`
	Trust        string   `json:"trust"`
	State        string   `json:"state"`
	Enabled      bool     `json:"enabled"`
	Providers    []string `json:"providers"`
	DialectCount int      `json:"dialect_count"`
	LastError    string   `json:"last_error"`
}

func mustPluginLs(t *testing.T, out string) []pluginLsRow {
	t.Helper()
	var rows []pluginLsRow
	mustJSON(t, out, &rows)
	return rows
}

func TestPluginUsageErrors(t *testing.T) {
	sandbox(t)

	// 不带子命令与 --help 等价：列子命令、退出码 0；下面的用法错误从真给了一个子命令开始。
	code, out, stderr := run(t, "plugin")
	if code != 0 {
		t.Fatalf("缺子命令的退出码 = %d，期望 0（与 --help 一致）；stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "install") {
		t.Fatalf("stdout 应列出可用子命令: %s", out)
	}

	code, out, stderr = run(t, "plugin", "install")
	if code != 2 {
		t.Fatalf("install 缺参数的退出码 = %d，期望 2；stderr = %s", code, stderr)
	}
	if out != "" {
		t.Fatalf("用法错误不该写 stdout: %s", out)
	}
	if !strings.Contains(stderr, "lax.plugin.json") {
		t.Fatalf("stderr 应说明目录里要有什么: %s", stderr)
	}

	code, _, stderr = run(t, "plugin", "nope")
	if code != 2 {
		t.Fatalf("未知子命令的退出码 = %d，期望 2；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "lax plugin --help") {
		t.Fatalf("stderr 应指向组级帮助: %s", stderr)
	}
}

func TestPluginGroupHelp(t *testing.T) {
	sandbox(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		code, out, stderr := run(t, "plugin", arg)
		if code != 0 {
			t.Fatalf("plugin %s 的退出码 = %d，期望 0", arg, code)
		}
		if stderr != "" {
			t.Fatalf("plugin %s 的帮助不该写 stderr: %s", arg, stderr)
		}
		for _, want := range []string{"用法：", "子命令：", "install", "doctor"} {
			if !strings.Contains(out, want) {
				t.Fatalf("plugin %s 的输出缺 %q: %s", arg, want, out)
			}
		}
	}
}

func TestPluginLsEmptyHint(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "plugin", "ls")
	if code != 0 {
		t.Fatalf("plugin ls 的退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "lax plugin install") {
		t.Fatalf("空表应给出安装示例: %s", out)
	}
}

// TestPluginLifecycle 跑完整条命令面：install → ls → show → disable → enable → rm --purge，
// 每一步都断言输出里的关键字段与库里的行。
func TestPluginLifecycle(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginFixture(t)

	code, out, stderr := run(t, "plugin", "install", src)
	if code != 0 {
		t.Fatalf("install 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已安装插件 com.example.echo 1.0.0", "厂商 echo", "openai-speech /v1/audio/speech", "com.example.echo/speech@1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("install 输出缺 %q: %s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(pluginInstalledDir(t, "com.example.echo", "1.0.0"), "lax.plugin.json")); err != nil {
		t.Fatalf("包没落到 installed/ 下: %v", err)
	}
	info, err := os.Stat(pluginInstalledDir(t, "com.example.echo", "1.0.0"))
	if err != nil {
		t.Fatalf("读包目录: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("包目录权限 = %o，期望 700", perm)
	}
	if left, err := os.ReadDir(filepath.Join(pluginDataDir(t), "plugins", "staging")); err != nil || len(left) != 0 {
		t.Fatalf("staging 里留下了 %d 项，err = %v", len(left), err)
	}

	db := storetest.Open(t)
	p, ok, err := db.PluginByID("com.example.echo")
	if err != nil || !ok {
		t.Fatalf("库里没有插件行: ok=%v err=%v", ok, err)
	}
	if p.ActiveVersion != "1.0.0" || !p.Enabled || p.State != "ok" || p.Kind != "provider" || p.Trust != "declarative" {
		t.Fatalf("插件行 = %+v", p)
	}
	if _, err := db.GetProvider("echo"); err != nil {
		t.Fatalf("插件没登记厂商: %v", err)
	}
	routes, err := db.ProviderRoutes("echo")
	if err != nil || len(routes) != 1 || routes[0].Protocol != "openai-speech" || routes[0].Path != "/v1/audio/speech" {
		t.Fatalf("路由 = %+v，err = %v", routes, err)
	}
	op, ok, err := db.ModelOperation("echo", "echo-tts", "speech.synthesize@1")
	if err != nil || !ok {
		t.Fatalf("模型没有方言绑定: ok=%v err=%v", ok, err)
	}
	if op.DialectID != "com.example.echo/speech@1" || op.Source != "plugin" {
		t.Fatalf("方言绑定 = %+v", op)
	}
	if !strings.Contains(op.CapabilitiesJSON, `"supported":true`) {
		t.Fatalf("能力声明没入库: %s", op.CapabilitiesJSON)
	}

	code, out, stderr = run(t, "plugin", "ls")
	if code != 0 {
		t.Fatalf("ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"com.example.echo", "1.0.0", "declarative", "ok", "echo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls 输出缺 %q: %s", want, out)
		}
	}

	code, out, _ = run(t, "plugin", "ls", "--json")
	if code != 0 {
		t.Fatalf("ls --json 退出码 = %d", code)
	}
	rows := mustPluginLs(t, out)
	if len(rows) != 1 {
		t.Fatalf("ls --json = %+v", rows)
	}
	row := rows[0]
	if row.ID != "com.example.echo" || row.Version != "1.0.0" || row.Kind != "provider" ||
		row.Trust != "declarative" || row.State != "ok" || !row.Enabled || row.DialectCount != 1 ||
		row.LastError != "" || len(row.Providers) != 1 || row.Providers[0] != "echo" {
		t.Fatalf("ls --json 一行 = %+v", row)
	}

	code, out, stderr = run(t, "plugin", "show", "com.example.echo")
	if code != 0 {
		t.Fatalf("show 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"manifest", "com.example.echo/speech@1", "已装版本 1.0.0", "信任", "冲突 无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show 输出缺 %q: %s", want, out)
		}
	}

	code, out, _ = run(t, "plugin", "show", "com.example.echo", "--json")
	if code != 0 {
		t.Fatalf("show --json 退出码 = %d", code)
	}
	var shown struct {
		Manifest    json.RawMessage `json:"manifest"`
		Permissions struct {
			Filesystem string `json:"filesystem"`
			Secrets    struct {
				Account []string `json:"account"`
			} `json:"secrets"`
		} `json:"permissions"`
		Versions  []string `json:"versions"`
		Providers []struct {
			ID       string   `json:"id"`
			Dialects []string `json:"dialects"`
		} `json:"providers"`
		Dialects  []string `json:"dialects"`
		State     string   `json:"state"`
		LastError string   `json:"last_error"`
	}
	mustJSON(t, out, &shown)
	if len(shown.Manifest) == 0 || !strings.Contains(string(shown.Manifest), "com.example.echo") {
		t.Fatalf("show --json 的 manifest = %s", shown.Manifest)
	}
	if shown.Permissions.Filesystem != "none" || len(shown.Permissions.Secrets.Account) != 1 {
		t.Fatalf("show --json 的权限 = %+v", shown.Permissions)
	}
	if len(shown.Versions) != 1 || shown.Versions[0] != "1.0.0" {
		t.Fatalf("show --json 的版本 = %+v", shown.Versions)
	}
	if len(shown.Providers) != 1 || shown.Providers[0].ID != "echo" || len(shown.Providers[0].Dialects) != 1 {
		t.Fatalf("show --json 的厂商 = %+v", shown.Providers)
	}
	if len(shown.Dialects) != 1 || shown.Dialects[0] != "com.example.echo/speech@1" || shown.State != "ok" {
		t.Fatalf("show --json 的方言与状态 = %+v / %q", shown.Dialects, shown.State)
	}

	code, _, stderr = run(t, "plugin", "show", "com.example.nope")
	if code != 1 {
		t.Fatalf("show 未知 ID 的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "com.example.echo") {
		t.Fatalf("stderr 应列出已装的 ID: %s", stderr)
	}

	code, out, stderr = run(t, "plugin", "disable", "com.example.echo")
	if code != 0 {
		t.Fatalf("disable 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "已停用") {
		t.Fatalf("disable 输出 = %s", out)
	}
	p, _, _ = db.PluginByID("com.example.echo")
	if p.Enabled || p.State != "disabled" {
		t.Fatalf("停用后插件行 = %+v", p)
	}
	code, out, _ = run(t, "plugin", "ls", "--json", "--enabled")
	if code != 0 || strings.Contains(out, "com.example.echo") {
		t.Fatalf("ls --enabled 仍列出停用的插件: %s", out)
	}

	code, out, stderr = run(t, "plugin", "enable", "com.example.echo")
	if code != 0 {
		t.Fatalf("enable 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "已启用") {
		t.Fatalf("enable 输出 = %s", out)
	}
	p, _, _ = db.PluginByID("com.example.echo")
	if !p.Enabled || p.State != "ok" {
		t.Fatalf("启用后插件行 = %+v", p)
	}

	code, _, stderr = run(t, "plugin", "enable", "com.example.nope")
	if code != 1 || !strings.Contains(stderr, "没有安装") {
		t.Fatalf("enable 未知 ID: code=%d stderr=%s", code, stderr)
	}

	code, out, stderr = run(t, "plugin", "rm", "com.example.echo", "--purge")
	if code != 0 {
		t.Fatalf("rm 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已卸载插件 com.example.echo", "1 个版本", "1 条方言绑定"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rm 输出缺 %q: %s", want, out)
		}
	}
	if _, ok, err := db.PluginByID("com.example.echo"); err != nil || ok {
		t.Fatalf("卸载后插件行还在: ok=%v err=%v", ok, err)
	}
	if _, err := db.GetProvider("echo"); err == nil {
		t.Fatal("卸载后插件登记的厂商还在")
	}
	if _, err := os.Stat(filepath.Join(pluginDataDir(t), "plugins", "installed", "com.example.echo")); !os.IsNotExist(err) {
		t.Fatalf("--purge 之后包目录还在: %v", err)
	}
}

// TestPluginInstallDryRun 钉住「dry-run 什么都不写」：库、包目录都不动。
func TestPluginInstallDryRun(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginFixture(t)

	code, out, stderr := run(t, "plugin", "install", src, "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"将安装插件 com.example.echo 1.0.0", "路由 openai-speech /v1/audio/speech", "模型 echo-tts", "权限", "别名 无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run 输出缺 %q: %s", want, out)
		}
	}

	code, out, _ = run(t, "plugin", "install", src, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("dry-run --json 退出码 = %d", code)
	}
	var plan struct {
		ID        string `json:"id"`
		Version   string `json:"version"`
		DryRun    bool   `json:"dry_run"`
		Providers []struct {
			ID     string   `json:"id"`
			Routes []string `json:"routes"`
			Models []struct {
				Model      string            `json:"model"`
				Operations map[string]string `json:"operations"`
			} `json:"models"`
			Dialects []string `json:"dialects"`
			Aliases  []string `json:"aliases"`
		} `json:"providers"`
	}
	mustJSON(t, out, &plan)
	if plan.ID != "com.example.echo" || !plan.DryRun || len(plan.Providers) != 1 {
		t.Fatalf("dry-run --json = %+v", plan)
	}
	if len(plan.Providers[0].Routes) != 1 || len(plan.Providers[0].Dialects) != 1 || len(plan.Providers[0].Models) != 1 {
		t.Fatalf("dry-run 的厂商 = %+v", plan.Providers[0])
	}
	if plan.Providers[0].Models[0].Operations["speech.synthesize@1"] != "com.example.echo/speech@1" {
		t.Fatalf("dry-run 的模型 = %+v", plan.Providers[0].Models[0])
	}

	db := storetest.Open(t)
	if plugins, err := db.ListPlugins(); err != nil || len(plugins) != 0 {
		t.Fatalf("dry-run 之后库里有插件行: %+v err=%v", plugins, err)
	}
	if _, err := os.Stat(filepath.Join(pluginDataDir(t), "plugins")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 之后数据目录里建了 plugins/: %v", err)
	}
}

// TestPluginInstallVersionRules 钉住两条版本规则：同一版本不能重装，换版本要显式确认切换。
func TestPluginInstallVersionRules(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginFixture(t)

	if code, _, stderr := run(t, "plugin", "install", src); code != 0 {
		t.Fatalf("首次安装失败: %s", stderr)
	}

	code, _, stderr := run(t, "plugin", "install", src)
	if code != 1 {
		t.Fatalf("重复安装同一版本的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "已经装过") || !strings.Contains(stderr, "version") {
		t.Fatalf("stderr 应说明版本已存在并提示换号: %s", stderr)
	}

	bumpFixtureVersion(t, src, "1.1.0")
	code, _, stderr = run(t, "plugin", "install", src)
	if code != 1 {
		t.Fatalf("换版本不给 --yes 的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("stderr 应提示加 --yes: %s", stderr)
	}

	code, out, stderr := run(t, "plugin", "install", src, "--yes")
	if code != 0 {
		t.Fatalf("给 --yes 之后安装失败: %s", stderr)
	}
	if !strings.Contains(out, "1.1.0") {
		t.Fatalf("输出应写新版本: %s", out)
	}
	db := storetest.Open(t)
	p, _, _ := db.PluginByID("com.example.echo")
	if p.ActiveVersion != "1.1.0" {
		t.Fatalf("活动版本 = %q，期望 1.1.0", p.ActiveVersion)
	}
	versions, err := db.PluginVersions("com.example.echo")
	if err != nil || len(versions) != 2 {
		t.Fatalf("版本行 = %+v，err = %v", versions, err)
	}
}

// TestPluginInstallRejectsSymlink 钉住复制时的符号链接拒绝：链接能指到包外。
func TestPluginInstallRejectsSymlink(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginFixture(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "leak.txt")); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}

	code, _, stderr := run(t, "plugin", "install", src)
	if code != 1 {
		t.Fatalf("带符号链接的安装退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "符号链接") {
		t.Fatalf("stderr 应指出符号链接: %s", stderr)
	}
	db := storetest.Open(t)
	if plugins, err := db.ListPlugins(); err != nil || len(plugins) != 0 {
		t.Fatalf("失败的安装留下了插件行: %+v err=%v", plugins, err)
	}
	if _, err := os.Stat(pluginInstalledDir(t, "com.example.echo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("失败的安装留下了包目录: %v", err)
	}
}

// TestPluginInstallChatProvider 钉住对话上游能靠插件装起来：不带 dialects 文件，
// 路由、模型目录、宿主方言绑定与别名一次写完。
func TestPluginInstallChatProvider(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := filepath.Join("..", "..", "internal", "plugin", "testdata", "plugin-river")

	code, _, stderr := run(t, "plugin", "install", src)
	if code != 0 {
		t.Fatalf("安装对话插件失败（退出码 %d）: %s", code, stderr)
	}

	db := storetest.Open(t)
	provider, err := db.GetProvider("river")
	if err != nil {
		t.Fatalf("厂商没建起来: %v", err)
	}
	if provider.BaseURL != "https://api.river.test" {
		t.Fatalf("厂商地址 = %q", provider.BaseURL)
	}
	routes, err := db.ProviderRoutes("river")
	if err != nil || len(routes) != 1 || routes[0].Protocol != "openai-chat" || routes[0].Path != "/v1/chat/completions" {
		t.Fatalf("路由 = %+v err=%v，期望一条默认的 openai-chat 路由", routes, err)
	}
	models, err := db.ProviderModels("river")
	if err != nil || len(models) != 1 || models[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("模型目录 = %+v err=%v，期望按上游名落一行", models, err)
	}
	bound, ok, err := db.ModelOperation("river", "deepseek-v4.1-flash", plugin.OpChatComplete)
	if err != nil || !ok || bound.DialectID != plugin.DialectHostOpenAIChat {
		t.Fatalf("方言绑定 = %+v ok=%v err=%v，期望宿主方言", bound, ok, err)
	}
	aliases, err := db.ModelAliases()
	if err != nil || len(aliases) != 1 || aliases[0].Alias != "river-fast" || aliases[0].Target != "deepseek-v4.1-flash" {
		t.Fatalf("别名 = %+v err=%v，期望 river-fast → 上游名", aliases, err)
	}
}

// TestPluginInstallRejectsUnexecutableOperation 钉住「声明式方言不能承载对话操作」：
// 声明了对话操作的插件在安装时报错，而不是装进去后静默不生效。
func TestPluginInstallRejectsUnexecutableOperation(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginFixture(t)
	models := `{
  "models": [
    {
      "model": "echo-chat",
      "upstream_id": "echo-chat",
      "operations": {"chat.complete@1": {"dialect": "com.example.echo/speech@1"}}
    }
  ]
}
`
	if err := os.WriteFile(filepath.Join(src, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatalf("改写夹具 models.json: %v", err)
	}

	code, _, stderr := run(t, "plugin", "install", src)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "声明式方言") || !strings.Contains(stderr, "hook") {
		t.Fatalf("stderr 应说清为什么不装：%s", stderr)
	}
	db := storetest.Open(t)
	if plugins, err := db.ListPlugins(); err != nil || len(plugins) != 0 {
		t.Fatalf("失败的安装留下了插件行: %+v err=%v", plugins, err)
	}
}

// hookPluginDir 是带代码（moejs）的插件夹具：index.js 导出两个钩子，不发网络请求。
func hookPluginDir() string {
	return filepath.Join("..", "..", "internal", "plugin", "testdata", "plugin-hook")
}

// copyHookPlugin 把 hook 夹具复制到临时目录，方便改坏入口再装。
func copyHookPlugin(t *testing.T) string {
	t.Helper()
	src := hookPluginDir()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("读 hook 夹具: %v", err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("读夹具 %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); err != nil {
			t.Fatalf("写夹具 %s: %v", entry.Name(), err)
		}
	}
	return dst
}

// TestPluginInstallHookPlugin 跑通带代码插件的安装：信任按 runtime 推断，库与确认输出都对得上。
func TestPluginInstallHookPlugin(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := hookPluginDir()

	code, out, stderr := run(t, "plugin", "install", src)
	if code != 0 {
		t.Fatalf("安装 hook 插件失败（退出码 %d）: %s", code, stderr)
	}
	for _, want := range []string{"已安装插件 com.example.hook 1.0.0（provider，sandboxed）", "运行时 moejs（沙箱，钩子超时 100ms/2s）", "com.example.hook/speech@1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("install 输出缺 %q: %s", want, out)
		}
	}
	// index.js 是普通文件，跟着包一起复制过去。
	if _, err := os.Stat(filepath.Join(pluginInstalledDir(t, "com.example.hook", "1.0.0"), "index.js")); err != nil {
		t.Fatalf("入口文件没复制进包目录: %v", err)
	}

	db := storetest.Open(t)
	p, ok, err := db.PluginByID("com.example.hook")
	if err != nil || !ok {
		t.Fatalf("库里没有插件行: ok=%v err=%v", ok, err)
	}
	if p.Trust != "sandboxed" || p.Kind != "provider" || !p.Enabled {
		t.Fatalf("插件行 = %+v", p)
	}
	versions, err := db.PluginVersions("com.example.hook")
	if err != nil || len(versions) != 1 || versions[0].Runtime != "moejs" {
		t.Fatalf("版本行 = %+v，err = %v，期望 runtime 是 moejs", versions, err)
	}
	// hook 方言没有声明式 request，路由按线协议的默认端点建。
	routes, err := db.ProviderRoutes("hooktts")
	if err != nil || len(routes) != 1 || routes[0].Protocol != "openai-speech" || routes[0].Path != "/v1/audio/speech" {
		t.Fatalf("路由 = %+v，err = %v", routes, err)
	}

	code, out, _ = run(t, "plugin", "ls", "--json")
	if code != 0 {
		t.Fatalf("ls --json 退出码 = %d", code)
	}
	rows := mustPluginLs(t, out)
	if len(rows) != 1 || rows[0].Runtime != "moejs" || rows[0].Trust != "sandboxed" || rows[0].Kind != "provider" {
		t.Fatalf("ls --json = %+v", rows)
	}

	code, out, stderr = run(t, "plugin", "doctor")
	if code != 0 {
		t.Fatalf("干净 hook 包的 doctor 退出码 = %d，stderr = %s，输出 %s", code, stderr, out)
	}
	for _, want := range []string{"沙箱", "导出钩子", "prepare_request"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor 输出缺 %q: %s", want, out)
		}
	}
}

// TestPluginInstallTrustRules 钉住 --trust 与 runtime 的对应：只能按规定的那一种装。
func TestPluginInstallTrustRules(t *testing.T) {
	sandbox(t)
	mustInit(t)

	declarativeSrc := writePluginFixture(t)
	code, _, stderr := run(t, "plugin", "install", declarativeSrc, "--trust", "sandboxed")
	if code != 2 {
		t.Fatalf("声明式插件升 sandboxed 的退出码 = %d，期望 2；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "没有代码可跑") {
		t.Fatalf("stderr 应说清为什么升不上去: %s", stderr)
	}

	code, _, stderr = run(t, "plugin", "install", hookPluginDir(), "--trust", "declarative")
	if code != 2 {
		t.Fatalf("moejs 插件降 declarative 的退出码 = %d，期望 2；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "--trust declarative 不成立") {
		t.Fatalf("stderr 应说清信任级别不成立: %s", stderr)
	}

	code, _, stderr = run(t, "plugin", "install", hookPluginDir(), "--trust", "trusted-worker")
	if code != 2 {
		t.Fatalf("--trust trusted-worker 的退出码 = %d，期望 2；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "本版不支持") {
		t.Fatalf("stderr 应说明本版不支持 trusted-worker: %s", stderr)
	}

	code, _, stderr = run(t, "plugin", "install", hookPluginDir(), "--trust", "nope")
	if code != 2 || !strings.Contains(stderr, "只能是 declarative 或 sandboxed") {
		t.Fatalf("未知 --trust: code=%d stderr=%s", code, stderr)
	}
	// 失败的安装一条库记录也不该留。
	db := storetest.Open(t)
	if plugins, err := db.ListPlugins(); err != nil || len(plugins) != 0 {
		t.Fatalf("信任级别报错后留下了插件行: %+v err=%v", plugins, err)
	}
}

// TestPluginDoctorHookEntryBroken 钉住 doctor 能看出入口脚本坏了：装的时候不跑代码，复核时才露出来。
func TestPluginDoctorHookEntryBroken(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := copyHookPlugin(t)
	broken := "export default {\n  prepare_request(ctx) {\n    return {\n"
	if err := os.WriteFile(filepath.Join(src, "index.js"), []byte(broken), 0o644); err != nil {
		t.Fatalf("改坏入口: %v", err)
	}
	if code, _, stderr := run(t, "plugin", "install", src); code != 0 {
		t.Fatalf("入口坏掉不影响安装，却失败了（退出码 %d）: %s", code, stderr)
	}

	code, out, stderr := run(t, "plugin", "doctor", "com.example.hook")
	if code != 1 {
		t.Fatalf("坏入口的 doctor 退出码 = %d，期望 1；stderr = %s，输出 %s", code, stderr, out)
	}
	for _, want := range []string{"fail", "沙箱"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor 输出缺 %q: %s", want, out)
		}
	}
}

// 并把状态写成 tampered。
func TestPluginDoctorTampered(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginFixture(t)
	if code, _, stderr := run(t, "plugin", "install", src); code != 0 {
		t.Fatalf("安装失败: %s", stderr)
	}

	code, out, stderr := run(t, "plugin", "doctor")
	if code != 0 {
		t.Fatalf("干净包的 doctor 退出码 = %d，stderr = %s，输出 %s", code, stderr, out)
	}
	if strings.Contains(out, "fail") {
		t.Fatalf("干净包的 doctor 有失败项: %s", out)
	}

	db := storetest.Open(t)
	models := filepath.Join(pluginInstalledDir(t, "com.example.echo", "1.0.0"), "models.json")
	// 改成一个仍然合法的目录：包还读得动，只是哈希对不上，能单独验证哈希那一项。
	tampered := `{"models":[{"model":"echo-tts","upstream_id":"echo-tts","operations":{"speech.synthesize@1":{"dialect":"com.example.echo/speech@1"}}}]}`
	if err := os.WriteFile(models, []byte(tampered), 0o600); err != nil {
		t.Fatalf("改包内文件: %v", err)
	}

	code, out, stderr = run(t, "plugin", "doctor", "com.example.echo")
	if code != 1 {
		t.Fatalf("被改过的包 doctor 退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	for _, want := range []string{"fail", "包哈希", "tampered"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor 输出缺 %q: %s", want, out)
		}
	}
	p, _, _ := db.PluginByID("com.example.echo")
	if p.State != "tampered" || p.LastError == "" {
		t.Fatalf("doctor 之后插件行 = %+v", p)
	}

	code, out, _ = run(t, "plugin", "doctor", "--json")
	if code != 1 {
		t.Fatalf("doctor --json 的退出码 = %d，期望 1", code)
	}
	var results []struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	mustJSON(t, out, &results)
	if len(results) != 1 || results[0].Name != "com.example.echo" || results[0].OK {
		t.Fatalf("doctor --json = %+v", results)
	}
	if len(results[0].Checks) == 0 {
		t.Fatalf("doctor --json 没有逐项检查: %+v", results[0])
	}
}

// TestPluginInstallHookChatPlugin 钉住「hook 方言能承载对话操作」：
// 声明式方言会被拒，带代码的可以装，路由按 openai-chat 的默认端点建、绑定指向插件方言。
func TestPluginInstallHookChatPlugin(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := filepath.Join("..", "..", "internal", "plugin", "testdata", "plugin-hook-chat")

	code, _, stderr := run(t, "plugin", "install", src)
	if code != 0 {
		t.Fatalf("安装对话 hook 插件失败（退出码 %d）: %s", code, stderr)
	}

	db := storetest.Open(t)
	routes, err := db.ProviderRoutes("hookchat")
	if err != nil || len(routes) != 1 || routes[0].Protocol != "openai-chat" || routes[0].Path != "/v1/chat/completions" {
		t.Fatalf("路由 = %+v err=%v", routes, err)
	}
	bound, ok, err := db.ModelOperation("hookchat", "hook-chat-v1", "chat.complete@1")
	if err != nil || !ok || bound.DialectID != "com.example.hookchat/chat@1" {
		t.Fatalf("绑定 = %+v ok=%v err=%v", bound, ok, err)
	}
	if code, _, stderr := run(t, "plugin", "doctor", "com.example.hookchat"); code != 0 {
		t.Fatalf("doctor 退出码 = %d，stderr = %s", code, stderr)
	}
}
