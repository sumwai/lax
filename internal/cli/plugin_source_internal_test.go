package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/pluginfetch"
	"lax/internal/store/storetest"
)

func TestResolvePluginSpec(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "local-plugin"), 0o700); err != nil {
		t.Fatalf("建本地插件目录：%v", err)
	}
	t.Chdir(work)

	tests := []struct {
		name          string
		spec          string
		want          string
		wantShorthand bool
		wantErr       string
	}{
		{name: "本地同名目录优先", spec: "local-plugin", want: "local-plugin"},
		{name: "裸名映射官方目录", spec: "doubao-seed-tts", want: "github:sumwai/lax-plugins/plugins/doubao-seed-tts", wantShorthand: true},
		{name: "固定版本映射插件 tag", spec: "doubao-seed-tts@1.2.1", want: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@doubao-seed-tts-v1.2.1", wantShorthand: true},
		{name: "v 前缀版本映射同一个 tag", spec: "doubao-seed-tts@v1.2.1", want: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@doubao-seed-tts-v1.2.1", wantShorthand: true},
		{name: "预发布版本", spec: "doubao-seed-tts@1.3.0-rc.1", want: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@doubao-seed-tts-v1.3.0-rc.1", wantShorthand: true},
		{name: "显式本地路径", spec: "./local-plugin", want: "./local-plugin"},
		{name: "带斜杠的路径", spec: "plugins/local-plugin", want: "plugins/local-plugin"},
		{name: "非版本 ref 原样使用", spec: "local-plugin@main", want: "github:sumwai/lax-plugins/plugins/local-plugin@main", wantShorthand: true},
		{name: "带 ref 时不看本地同名目录", spec: "doubao-seed-tts@main", want: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@main", wantShorthand: true},
		{name: "虚线开头", spec: ".local-plugin", want: ".local-plugin"},
		{name: "完整 GitHub 地址", spec: "https://github.com/sumwai/lax-plugins/tree/master/plugins/doubao-seed-tts", want: "https://github.com/sumwai/lax-plugins/tree/master/plugins/doubao-seed-tts"},
		{name: "GitHub 简写", spec: "github:sumwai/lax-plugins/plugins/doubao-seed-tts", want: "github:sumwai/lax-plugins/plugins/doubao-seed-tts"},
		{name: "空来源", spec: ""},
		{name: "缺少版本", spec: "doubao-seed-tts@", wantErr: "@ 后面没有版本"},
		{name: "版本里有越界段", spec: "doubao-seed-tts@../main", wantErr: "不合法"},
		{name: "名字里有多余 @", spec: "doubao@seed@1.2.1", wantErr: "不合法"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, shorthand, err := resolvePluginSpec(tt.spec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolvePluginSpec(%q) 的错误 = %v，期望包含 %q", tt.spec, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePluginSpec(%q)：%v", tt.spec, err)
			}
			if got != tt.want || shorthand != tt.wantShorthand {
				t.Fatalf("resolvePluginSpec(%q) = (%q, %v)，期望 (%q, %v)", tt.spec, got, shorthand, tt.want, tt.wantShorthand)
			}
		})
	}
}

// TestPluginInstallPinnedVersion 钉住 @版本：下载地址走插件 tag，落库来源也带 tag。
func TestPluginInstallPinnedVersion(t *testing.T) {
	hookDir, err := filepath.Abs(filepath.Join("..", "plugin", "testdata", "plugin-hook"))
	if err != nil {
		t.Fatalf("解析 hook 插件夹具：%v", err)
	}
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(work, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(work, "data"))
	t.Setenv("HOME", filepath.Join(work, "home"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}

	old := fetchPluginSource
	t.Cleanup(func() { fetchPluginSource = old })
	want := "github:sumwai/lax-plugins/plugins/doubao-seed-tts@doubao-seed-tts-v1.2.1"
	var gotSpec string
	fetchPluginSource = func(_ context.Context, spec string) (pluginfetch.Package, error) {
		gotSpec = spec
		return pluginfetch.Package{Dir: hookDir, Origin: spec}, nil
	}

	out.Reset()
	errOut.Reset()
	code := Run([]string{"plugin", "install", "doubao-seed-tts@1.2.1", "--trust", "sandboxed"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("固定版本安装失败（退出码 %d）: %s", code, errOut.String())
	}
	if gotSpec != want {
		t.Fatalf("下载来源 = %q，期望 %q", gotSpec, want)
	}
	versions, err := storetest.Open(t).PluginVersions("com.example.hook")
	if err != nil || len(versions) != 1 || versions[0].Source != want {
		t.Fatalf("版本行 = %+v，err = %v", versions, err)
	}
}

// TestPluginInstallBareNameUsesDefaultRegistry 钉住简写：裸名字在没有同名本地目录时
// 要变成官方插件仓库里的路径，来源也按远程记。
func TestPluginInstallBareNameUsesDefaultRegistry(t *testing.T) {
	hookDir, err := filepath.Abs(filepath.Join("..", "plugin", "testdata", "plugin-hook"))
	if err != nil {
		t.Fatalf("解析 hook 插件夹具：%v", err)
	}
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(work, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(work, "data"))
	t.Setenv("HOME", filepath.Join(work, "home"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}

	old := fetchPluginSource
	t.Cleanup(func() { fetchPluginSource = old })
	var gotSpec string
	fetchPluginSource = func(_ context.Context, spec string) (pluginfetch.Package, error) {
		gotSpec = spec
		return pluginfetch.Package{
			Dir:    hookDir,
			Origin: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@HEAD",
		}, nil
	}

	out.Reset()
	errOut.Reset()
	code := Run([]string{"plugin", "install", "doubao-seed-tts", "--trust", "sandboxed"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("简写安装失败（退出码 %d）: %s", code, errOut.String())
	}
	want := "github:sumwai/lax-plugins/plugins/doubao-seed-tts"
	if gotSpec != want {
		t.Fatalf("下载来源 = %q，期望 %q", gotSpec, want)
	}

	st := storetest.Open(t)
	versions, err := st.PluginVersions("com.example.hook")
	if err != nil || len(versions) != 1 {
		t.Fatalf("版本行 = %+v，err = %v", versions, err)
	}
	if versions[0].Source != want+"@HEAD" {
		t.Fatalf("来源 = %q，期望 %q", versions[0].Source, want+"@HEAD")
	}
}

// TestPluginInstallFromRemoteSource 用假下载器跑一遍 GitHub 来源：安装流水线、包目录
// 与 plugin_versions.source 都要和本地目录安装一致。
func TestPluginInstallFromRemoteSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "home"))

	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}

	old := fetchPluginSource
	t.Cleanup(func() { fetchPluginSource = old })
	fetchPluginSource = func(context.Context, string) (pluginfetch.Package, error) {
		return pluginfetch.Package{
			Dir:    filepath.Join("..", "plugin", "testdata", "plugin-hook"),
			Origin: "github:acme/plugins/plugin-hook@main",
		}, nil
	}

	out.Reset()
	errOut.Reset()
	code := Run([]string{
		"plugin", "install", "https://github.com/acme/plugins/tree/main/plugin-hook",
		"--trust", "sandboxed",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("远程安装失败（退出码 %d）: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "已安装插件 com.example.hook 1.0.0") {
		t.Fatalf("安装输出 = %s", out.String())
	}

	st := storetest.Open(t)
	versions, err := st.PluginVersions("com.example.hook")
	if err != nil || len(versions) != 1 {
		t.Fatalf("版本行 = %+v，err = %v", versions, err)
	}
	if versions[0].Source != "github:acme/plugins/plugin-hook@main" {
		t.Fatalf("来源 = %q，期望 github:acme/plugins/plugin-hook@main", versions[0].Source)
	}
}
