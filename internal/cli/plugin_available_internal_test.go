package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/pluginfetch"
)

// TestPluginLsAvailable 钉住仓库列表：扫描 plugins/ 下的包，按目录名排序，
// 并与库里的插件按 manifest ID 对照出已装版本。
func TestPluginLsAvailable(t *testing.T) {
	work := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(work, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(work, "data"))
	t.Setenv("HOME", filepath.Join(work, "home"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}
	hookDir, err := filepath.Abs(filepath.Join("..", "plugin", "testdata", "plugin-hook"))
	if err != nil {
		t.Fatalf("解析 hook 插件夹具：%v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"plugin", "install", hookDir}, &out, &errOut); code != 0 {
		t.Fatalf("安装 hook 插件失败（退出码 %d）: %s", code, errOut.String())
	}

	repo := t.TempDir()
	copyFixture(t, filepath.Join("..", "plugin", "testdata", "plugin-echo"), filepath.Join(repo, "plugins", "echo"))
	copyFixture(t, hookDir, filepath.Join(repo, "plugins", "hook"))

	old := fetchPluginRepo
	t.Cleanup(func() { fetchPluginRepo = old })
	fetchPluginRepo = func(context.Context, string) (pluginfetch.Package, error) {
		return pluginfetch.Package{Dir: repo}, nil
	}

	out.Reset()
	errOut.Reset()
	code := Run([]string{"plugin", "ls", "--available", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("plugin ls --available 退出码 = %d，stderr = %s", code, errOut.String())
	}
	var rows []pluginAvailableJSON
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("输出不是 JSON: %v，%s", err, out.String())
	}
	if len(rows) != 2 || rows[0].Name != "echo" || rows[1].Name != "hook" {
		t.Fatalf("仓库列表 = %+v，期望按目录名排序的 echo、hook", rows)
	}
	if rows[0].Installed || rows[0].InstalledVersion != "" || rows[0].Path != "plugins/echo" {
		t.Fatalf("未安装的 echo = %+v", rows[0])
	}
	if !rows[1].Installed || rows[1].InstalledVersion != "1.0.0" || !rows[1].Enabled || rows[1].State != pluginStateOK {
		t.Fatalf("已安装的 hook = %+v", rows[1])
	}
	if rows[1].ID != "com.example.hook" || rows[1].Runtime != "moejs" || rows[1].DialectCount != 1 {
		t.Fatalf("hook 的 manifest 字段 = %+v", rows[1])
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"plugin", "ls", "--available"}, &out, &errOut); code != 0 {
		t.Fatalf("文本输出退出码 = %d，stderr = %s", code, errOut.String())
	}
	for _, want := range []string{"名称", "仓库版本", "已装版本", "未安装", "已启用"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("文本输出缺 %q: %s", want, out.String())
		}
	}
}

func TestPluginLsAvailableRejectsInvalidPackage(t *testing.T) {
	work := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(work, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(work, "data"))
	t.Setenv("HOME", filepath.Join(work, "home"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}

	repo := t.TempDir()
	broken := filepath.Join(repo, "plugins", "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatalf("建坏包目录：%v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "lax.plugin.json"), []byte(`{"schema":"lax.plugin/v1"}`), 0o600); err != nil {
		t.Fatalf("写坏 manifest：%v", err)
	}

	old := fetchPluginRepo
	t.Cleanup(func() { fetchPluginRepo = old })
	fetchPluginRepo = func(context.Context, string) (pluginfetch.Package, error) {
		return pluginfetch.Package{Dir: repo}, nil
	}

	code := Run([]string{"plugin", "ls", "--available"}, &out, &errOut)
	if code == 0 || !strings.Contains(errOut.String(), "不是可安装的包") || !strings.Contains(errOut.String(), "broken") {
		t.Fatalf("退出码 = %d，stderr = %s，期望指出 broken 不是可安装的包", code, errOut.String())
	}
}

func TestPluginLsAvailableRejectsEnabled(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"plugin", "ls", "--available", "--enabled"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--available") {
		t.Fatalf("退出码 = %d，stderr = %s，期望用法错误", code, errOut.String())
	}
}

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("复制插件夹具 %s: %v", src, err)
	}
}
