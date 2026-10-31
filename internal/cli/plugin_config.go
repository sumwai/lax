package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"lax/internal/plugin"
)

// plugin config 一组：读写 manifest 声明的配置项（options）。
//
// 值必须是 JSON 字符串、数字或布尔，与 manifest 的 type/enum 对齐；没写过的键回落到
// manifest 的 default。secret 不走这里，配置项只放非敏感值。

const (
	pluginConfigGetUsage   = "用法：lax plugin config get <插件 ID> [键] [--json]"
	pluginConfigSetUsage   = "用法：lax plugin config set <插件 ID> <键> <值> [--stdin]"
	pluginConfigUnsetUsage = "用法：lax plugin config unset <插件 ID> <键>"
)

func cmdPluginConfig(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "plugin config", "插件配置项：manifest 里声明的 options", []groupItem{
			{"get", "读取配置项，没写过的显示 manifest 的默认值"},
			{"set", "写一个配置项"},
			{"unset", "删掉一个配置项，回到默认值"},
		})
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：get | set | unset\n用 lax plugin config --help 查看子命令")
	}
	switch args[0] {
	case "get":
		return cmdPluginConfigGet(e, args[1:])
	case "set":
		return cmdPluginConfigSet(e, args[1:])
	case "unset":
		return cmdPluginConfigUnset(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：get | set | unset", args[0])
	}
}

func cmdPluginConfigGet(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin config get", "读取插件的配置项")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return usagef("%s", pluginConfigGetUsage)
	}
	id := strings.TrimSpace(fs.Arg(0))
	key := ""
	if fs.NArg() == 2 {
		key = strings.TrimSpace(fs.Arg(1))
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	decls, err := pluginOptionDecls(a, id)
	if err != nil {
		return err
	}
	stored, err := a.st.PluginOptions(id)
	if err != nil {
		return fmt.Errorf("读插件 %s 的配置项失败：%v", id, err)
	}

	values := make(map[string]string, len(decls))
	for name, decl := range decls {
		if v, ok := stored[name]; ok {
			values[name] = v
			continue
		}
		if len(decl.Default) > 0 {
			values[name] = decodeOptionDefault(decl.Default)
		}
	}
	if key != "" {
		if _, ok := decls[key]; !ok {
			return fmt.Errorf("插件 %s 没有声明配置项 %q：可用的是 %s", id, key, strings.Join(sortedOptionKeys(decls), "、"))
		}
		if e.JSON {
			return e.printJSON(map[string]any{"plugin": id, "key": key, "value": values[key], "declared": true})
		}
		if v, ok := values[key]; ok {
			fmt.Fprintf(e.Stdout, "%s = %s\n", key, v)
		} else {
			fmt.Fprintf(e.Stdout, "%s 没有取值，也没有默认值\n", key)
		}
		return nil
	}

	if e.JSON {
		return e.printJSON(map[string]any{"plugin": id, "options": values})
	}
	if len(values) == 0 {
		fmt.Fprintf(e.Stdout, "插件 %s 没有声明配置项，也没有存过值\n", id)
		return nil
	}
	table := make([][]string, 0, len(values))
	for _, name := range sortedStringKeys(values) {
		table = append(table, []string{name, values[name]})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"配置项", "取值"}, table))
	return nil
}

func cmdPluginConfigSet(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin config set", "写一个插件配置项")
	stdin := fs.Bool("stdin", false, "从标准输入读取值")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 || fs.NArg() > 3 {
		return usagef("%s", pluginConfigSetUsage)
	}
	id := strings.TrimSpace(fs.Arg(0))
	key := strings.TrimSpace(fs.Arg(1))
	value := ""
	if fs.NArg() == 3 {
		value = fs.Arg(2)
	}
	if *stdin {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if err != nil {
			return fmt.Errorf("读标准输入失败：%v", err)
		}
		value = strings.TrimRight(string(raw), "\n")
	} else if fs.NArg() != 3 {
		return usagef("要写入的取值要给出来，或用 --stdin 从标准输入读\n%s", pluginConfigSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	decls, err := pluginOptionDecls(a, id)
	if err != nil {
		return err
	}
	decl, ok := decls[key]
	if !ok {
		return fmt.Errorf("插件 %s 没有声明配置项 %q：可用的是 %s", id, key, strings.Join(sortedOptionKeys(decls), "、"))
	}
	typed, err := coerceOptionValue(key, value, decl)
	if err != nil {
		return err
	}
	if err := a.st.SetPluginOption(id, key, typed); err != nil {
		return fmt.Errorf("写配置项 %s 失败：%v", key, err)
	}
	if e.JSON {
		return e.printJSON(map[string]any{"plugin": id, "key": key, "value": value, "stored": true})
	}
	fmt.Fprintf(e.Stdout, "插件 %s 的 %s = %s\n", id, key, value)
	fmt.Fprintln(e.Stdout, "下一步：对运行中的 serve 发 lax plugin reload 让新取值生效")
	return nil
}

func cmdPluginConfigUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "plugin config unset", "删掉一个插件配置项")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("%s", pluginConfigUnsetUsage)
	}
	id := strings.TrimSpace(fs.Arg(0))
	key := strings.TrimSpace(fs.Arg(1))

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()
	decls, err := pluginOptionDecls(a, id)
	if err != nil {
		return err
	}
	decl, ok := decls[key]
	if !ok {
		return fmt.Errorf("插件 %s 没有声明配置项 %q：可用的是 %s", id, key, strings.Join(sortedOptionKeys(decls), "、"))
	}
	stored, err := a.st.PluginOptions(id)
	if err != nil {
		return fmt.Errorf("读插件 %s 的配置项失败：%v", id, err)
	}
	if _, existed := stored[key]; !existed {
		// 本来就没有不是错误：删它是幂等的，但要把「现在生效的是默认值」说清楚。
		if len(decl.Default) > 0 {
			fmt.Fprintf(e.Stdout, "%s 本来就没有设置，现在生效的是默认值 %s\n", key, decodeOptionDefault(decl.Default))
		} else {
			fmt.Fprintf(e.Stdout, "%s 本来就没有设置\n", key)
		}
		return nil
	}
	if err := a.st.DeletePluginOption(id, key); err != nil {
		return fmt.Errorf("删配置项 %s 失败：%v", key, err)
	}
	if e.JSON {
		return e.printJSON(map[string]any{"plugin": id, "key": key, "removed": true})
	}
	fmt.Fprintf(e.Stdout, "已删掉插件 %s 的配置项 %s\n", id, key)
	if len(decl.Default) > 0 {
		fmt.Fprintf(e.Stdout, "现在生效的是默认值 %s\n", decodeOptionDefault(decl.Default))
	}
	return nil
}

// pluginOptionDecls 读活动版本 manifest 里声明的配置项。包不在了就报错：
// 配置项的取值只有在 manifest 声明过时才合法，没有声明表就无从校验。
func pluginOptionDecls(a *app, id string) (map[string]plugin.OptionDecl, error) {
	p, ok, err := a.st.PluginByID(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, unknownPluginError(a.st, id)
	}
	pkg, err := installedPlugin(pluginDataDirFor(a), p)
	if err != nil {
		return nil, fmt.Errorf("读插件 %s 的活动版本失败：%v；用 lax plugin doctor %s 定位", id, err, id)
	}
	return pkg.Manifest.Options, nil
}

// pluginDataDirFor 是插件包所在的数据目录，与 serve 用的是同一个。
func pluginDataDirFor(a *app) string {
	return pluginDataDirOf(a.paths)
}

// coerceOptionValue 按 manifest 声明的类型把命令行取值翻成要入库的类型，并校验 enum。
func coerceOptionValue(key, raw string, decl plugin.OptionDecl) (any, error) {
	switch decl.Type {
	case "string":
		if len(decl.Enum) > 0 {
			found := false
			for _, candidate := range decl.Enum {
				if candidate == raw {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("配置项 %s 只能取 %s，得到 %q", key, strings.Join(decl.Enum, "、"), raw)
			}
		}
		return raw, nil
	case "number":
		var number json.Number
		if err := json.Unmarshal([]byte(raw), &number); err != nil {
			return nil, fmt.Errorf("配置项 %s 是数字类型，%q 解析不了", key, raw)
		}
		return number, nil
	case "boolean":
		flag, err := parseOptionBool(raw)
		if err != nil {
			return nil, fmt.Errorf("配置项 %s 是布尔类型，只认 true 或 false，得到 %q", key, raw)
		}
		return flag, nil
	}
	return nil, fmt.Errorf("配置项 %s 的类型 %q 不是宿主认识的（string、number、boolean）", key, decl.Type)
}

func parseOptionBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("不是布尔值")
}

// decodeOptionDefault 把 manifest 的默认值翻成输出用的字符串：字符串去掉引号，其余保持 JSON。
func decodeOptionDefault(raw json.RawMessage) string {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	if text, ok := decoded.(string); ok {
		return text
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}

func sortedOptionKeys(decls map[string]plugin.OptionDecl) []string {
	keys := make([]string, 0, len(decls))
	for key := range decls {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
