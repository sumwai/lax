package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"lax/internal/pluginfetch"
)

// pluginOfficialRepo 是官方插件仓库的根；plugin ls --available 扫它的 plugins/。
const pluginOfficialRepo = "github:sumwai/lax-plugins"

// pluginDefaultRegistry 是裸插件名默认查找的官方插件仓库。不带版本时映射到
// <registry>/<name>@HEAD；带版本时映射到 <registry>/<name>@<name>-v<版本>，
// 每个插件的发布 tag 因此互不撞车。现有同名目录优先，避免本地包被远程包顶掉。
const pluginDefaultRegistry = pluginOfficialRepo + "/plugins"

var (
	barePluginName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	pinnedVersion  = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	refChars       = regexp.MustCompile(`^[0-9A-Za-z._/+-]+$`)
)

// fetchPluginSource 给测试替换下载器；生产路径就是 pluginfetch.Fetch。
var fetchPluginSource = pluginfetch.Fetch

// pluginSource 是一次安装或升级的来源。本地目录的 Origin 是 dir；GitHub 来源
// 在命令结束前由 Close 删掉临时目录。
type pluginSource struct {
	Dir    string
	Origin string

	remote *pluginfetch.Package
}

// openPluginSource 把命令行给的来源落成一个可读目录。远程下载只在这里发生，
// 之后的 manifest 校验、哈希、复制与落库完全复用本地目录那条流水线。
func openPluginSource(spec string) (pluginSource, error) {
	resolved, shorthand, err := resolvePluginSpec(spec)
	if err != nil {
		return pluginSource{}, err
	}
	if !pluginfetch.IsRemote(resolved) {
		return pluginSource{Dir: resolved, Origin: pluginVersionSource}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pkg, err := fetchPluginSource(ctx, resolved)
	if err != nil {
		if shorthand {
			name, _, pinned := strings.Cut(strings.TrimSpace(spec), "@")
			hint := fmt.Sprintf("不带版本时装默认分支的最新版；固定版本写成 %s@<版本>（tag 是 %s-v<版本>）；本地目录请写成 ./%s", name, name, name)
			if pinned {
				hint = fmt.Sprintf("固定版本要对应仓库里的 tag：%s 映射到 %s；确认 tag 存在或改成不带 @ 装最新版；本地目录请写成 ./%s", spec, resolved, name)
			}
			return pluginSource{}, fmt.Errorf("把 %q 当成官方插件名，从 %s 安装失败：%w\n%s", spec, resolved, err, hint)
		}
		return pluginSource{}, err
	}
	return pluginSource{Dir: pkg.Dir, Origin: pkg.Origin, remote: &pkg}, nil
}

// resolvePluginSpec 把裸插件名映射到官方插件仓库。返回的第二个值表示是否做了
// 简写映射，用来在下载失败时补一条「本地目录怎么写」的提示。
//
// 不带 @ 的裸名字先看当前目录：有同名目录就按本地目录装，保持旧行为；没有就取
// 默认分支的最新版。带 @ 的名字一律走官方目录：@1.2.1 与 @v1.2.1 都映射到 tag
// <name>-v1.2.1，其它写法（@main、提交号）原样当 ref 用。
func resolvePluginSpec(spec string) (string, bool, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.HasPrefix(spec, ".") || pluginfetch.IsRemote(spec) {
		return spec, false, nil
	}

	name, ref, hasRef := strings.Cut(spec, "@")
	if hasRef && !strings.Contains(name, "/") {
		if name == "" || !barePluginName.MatchString(name) {
			return "", false, usagef("插件名 %q 不合法：@ 前面只能有字母、数字与 ._-，例如 doubao-seed-tts@1.2.1", spec)
		}
		if ref == "" {
			return "", false, usagef("插件名 %q 的 @ 后面没有版本：固定版本写 %s@1.2.1，不写 @ 就装最新版", spec, name)
		}
		if !refChars.MatchString(ref) || strings.Contains(ref, "..") ||
			strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
			return "", false, usagef("插件版本或 ref %q 不合法：只能有字母、数字与 ._-/，且不能含 ..", ref)
		}
		if pinnedVersion.MatchString(ref) {
			version := ref
			if !strings.HasPrefix(version, "v") {
				version = "v" + version
			}
			return pluginDefaultRegistry + "/" + name + "@" + name + "-" + version, true, nil
		}
		// 非版本号按分支、tag 或提交号原样取，例如 doubao-seed-tts@main。
		return pluginDefaultRegistry + "/" + name + "@" + ref, true, nil
	}

	if strings.Contains(name, "/") {
		return spec, false, nil
	}
	if !barePluginName.MatchString(name) {
		return spec, false, nil
	}
	if _, err := os.Stat(name); err == nil {
		// 同名文件也按本地来源交给 LoadDir：它的报错比「悄悄换成远程包」清楚。
		return spec, false, nil
	} else if !os.IsNotExist(err) {
		// 权限之类的错误交给后续 LoadDir 报，别悄悄换成远程包。
		return spec, false, nil
	}
	return pluginDefaultRegistry + "/" + name, true, nil
}

func (s pluginSource) Close() {
	if s.remote == nil {
		return
	}
	_ = s.remote.Close()
}
