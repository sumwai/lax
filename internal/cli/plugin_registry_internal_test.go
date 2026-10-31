package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/config"
	"lax/internal/plugin"
	"lax/internal/store/storetest"
)

// TestLoadRegistryHookPlugin 钉住网关启动时的两条路径：带代码的插件经沙箱登记 hook 方言，
// 沙箱打不开时只记 notes（serve 照常起来）并把状态写成 failed。
func TestLoadRegistryHookPlugin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "home"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}
	src := filepath.Join("..", "..", "internal", "plugin", "testdata", "plugin-hook")
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"plugin", "install", src}, &out, &errOut); code != 0 {
		t.Fatalf("安装 hook 插件失败（退出码 %d）: %s", code, errOut.String())
	}

	st := storetest.Open(t)
	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析数据路径: %v", err)
	}
	dataDir := filepath.Dir(paths.DB)

	reg, notes, err := loadRegistry(st.Store, dataDir, nil)
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("干净的 hook 插件不该有警告: %v", notes)
	}
	dialect, ok := reg.Dialect("com.example.hook/speech@1")
	if !ok {
		t.Fatal("hook 方言没登记进启动时的注册表")
	}
	plan, err := dialect.Prepare(plugin.Input{Model: "hook-tts-v1", Text: "hi"})
	if err != nil || plan.JSON["input"] != "hi" {
		t.Fatalf("登记进来的方言跑不起来: plan=%+v err=%v", plan, err)
	}
	if _, ok := reg.Dialect(plugin.DialectHostOpenAIChat); !ok {
		t.Fatal("内置方言被插件覆盖掉了")
	}

	// 把入口改坏：加载要退化成 notes + failed，服务本身不受影响。
	entry := filepath.Join(dataDir, "plugins", "installed", "com.example.hook", "1.0.0", "index.js")
	if err := os.WriteFile(entry, []byte("export default {"), 0o600); err != nil {
		t.Fatalf("改坏入口: %v", err)
	}
	reg, notes, err = loadRegistry(st.Store, dataDir, nil)
	if err != nil {
		t.Fatalf("插件坏掉时加载注册表不该报错: %v", err)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "com.example.hook") {
		t.Fatalf("坏插件应留下警告: %v", notes)
	}
	if _, ok := reg.Dialect("com.example.hook/speech@1"); ok {
		t.Fatal("打不开沙箱的方言不该进注册表")
	}
	p, ok, err := st.PluginByID("com.example.hook")
	if err != nil || !ok {
		t.Fatalf("读插件行: ok=%v err=%v", ok, err)
	}
	if p.State != pluginStateFailed || p.LastError == "" {
		t.Fatalf("插件行 = %+v，期望状态 failed 且带原因", p)
	}
}
