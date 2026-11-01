package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"lax/internal/plugin"
	"lax/internal/pluginfetch"
	"lax/internal/store"
)

// fetchPluginRepo 给测试替换官方仓库下载；生产路径就是 pluginfetch.Fetch。
var fetchPluginRepo = pluginfetch.Fetch

// pluginAvailableJSON 是 plugin ls --available 的一行。Name 是仓库里的目录名，
// 也就是 plugin install 接受的裸插件名；DisplayName 是 manifest 里给人看的名字。
type pluginAvailableJSON struct {
	Name             string `json:"name"`
	Path             string `json:"path"`
	DisplayName      string `json:"display_name"`
	ID               string `json:"id"`
	Version          string `json:"version"`
	Kind             string `json:"kind"`
	Runtime          string `json:"runtime"`
	DialectCount     int    `json:"dialect_count"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version"`
	Enabled          bool   `json:"enabled"`
	State            string `json:"state"`
}

// listAvailablePlugins 取官方仓库默认分支，扫描 plugins/ 下的一层目录。
// 每个目录都要能被 plugin.LoadDir 严格校验通过；坏包直接报出目录名，
// 不把半个目录当成可安装项。
func listAvailablePlugins(ctx context.Context) ([]pluginAvailableJSON, error) {
	pkg, err := fetchPluginRepo(ctx, pluginOfficialRepo)
	if err != nil {
		return nil, fmt.Errorf("读取官方插件仓库 %s 失败：%w", pluginOfficialRepo, err)
	}
	defer pkg.Close()

	root := filepath.Join(pkg.Dir, "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("官方插件仓库里没有 plugins/ 目录：%v", err)
	}
	rows := make([]pluginAvailableJSON, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		dir := filepath.Join(root, name)
		loaded, err := plugin.LoadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("官方仓库里的插件目录 %s 不是可安装的包：%w", name, err)
		}
		rows = append(rows, pluginAvailableJSON{
			Name:         name,
			Path:         "plugins/" + name,
			DisplayName:  loaded.Manifest.Name,
			ID:           loaded.Manifest.ID,
			Version:      loaded.Manifest.Version,
			Kind:         loaded.Manifest.Kind,
			Runtime:      loaded.Manifest.Runtime,
			DialectCount: len(loaded.Dialects),
		})
	}
	slices.SortFunc(rows, func(a, b pluginAvailableJSON) int { return strings.Compare(a.Name, b.Name) })
	return rows, nil
}

// markAvailableInstalled 把仓库目录与库里的插件按 manifest ID 对上。
func markAvailableInstalled(rows []pluginAvailableJSON, installed []store.Plugin) []pluginAvailableJSON {
	byID := make(map[string]store.Plugin, len(installed))
	for _, p := range installed {
		byID[p.ID] = p
	}
	for i := range rows {
		p, ok := byID[rows[i].ID]
		if !ok {
			continue
		}
		rows[i].Installed = true
		rows[i].InstalledVersion = p.ActiveVersion
		rows[i].Enabled = p.Enabled
		rows[i].State = p.State
	}
	return rows
}

func printAvailablePlugins(e *Env, rows []pluginAvailableJSON) error {
	if e.JSON {
		return e.printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "官方插件仓库里还没有插件：等仓库新增 plugins/<名称>/ 后再看")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		status := "未安装"
		if r.Installed {
			status = "已停用"
			if r.Enabled {
				status = "已启用"
			}
			if r.State != "" && r.State != pluginStateOK && r.State != pluginStateDisabled {
				status += "（" + r.State + "）"
			}
		}
		table = append(table, []string{
			r.Name, cellOrDash(r.Version), r.ID, r.Kind, cellOrDash(r.Runtime),
			cellOrDash(r.InstalledVersion), status,
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "仓库版本", "插件 ID", "类型", "运行时", "已装版本", "状态"}, table))
	return nil
}
