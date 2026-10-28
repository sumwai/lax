// Package agentcfg 改写其它客户端的配置文件，把请求指向本网关。
package agentcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// 常量：本机网关不校验凭据，客户端要求非空，所以给一个固定值。
const LocalToken = "switchboard-local"

// Target 是一个客户端的位置与它用到的配置键。
type Target struct {
	Name       string
	ConfigPath string
	KeyPath    []string
}

// Change 是一处待写入的差异。
type Change struct {
	Key string
	Old string
	New string
}

// Known 返回支持的客户端。home 用于拼出配置路径。
func Known(home string) []Target {
	return []Target{
		{
			Name:       "claude-code",
			ConfigPath: filepath.Join(home, ".claude", "settings.json"),
			KeyPath:    []string{"env"},
		},
	}
}

// Lookup 按名字找客户端。
func Lookup(home, name string) (Target, error) {
	for _, t := range Known(home) {
		if t.Name == name {
			return t, nil
		}
	}
	names := make([]string, 0)
	for _, t := range Known(home) {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return Target{}, fmt.Errorf("不支持的客户端 %q，可用：%s", name, joinComma(names))
}

// Desired 算出该客户端应写入的键值。
func Desired(baseURL, model string) map[string]string {
	out := map[string]string{
		"ANTHROPIC_BASE_URL":   baseURL,
		"ANTHROPIC_AUTH_TOKEN": LocalToken,
	}
	if model != "" {
		out["ANTHROPIC_MODEL"] = model
	}
	return out
}

// Diff 读出当前配置，返回需要改动的键。
func Diff(t Target, desired map[string]string) ([]Change, error) {
	doc, err := readDoc(t.ConfigPath)
	if err != nil {
		return nil, err
	}
	section := sectionOf(doc, t.KeyPath)

	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var changes []Change
	for _, k := range keys {
		old, _ := section[k].(string)
		if old == desired[k] {
			continue
		}
		changes = append(changes, Change{Key: k, Old: old, New: desired[k]})
	}
	return changes, nil
}

// Apply 备份后写入。返回备份文件路径。
func Apply(t Target, desired map[string]string) (string, error) {
	doc, err := readDoc(t.ConfigPath)
	if err != nil {
		return "", err
	}
	section := sectionOf(doc, t.KeyPath)
	for k, v := range desired {
		section[k] = v
	}
	setSection(doc, t.KeyPath, section)

	backup := t.ConfigPath + ".switchboard.bak"
	if err := os.MkdirAll(filepath.Dir(t.ConfigPath), 0o700); err != nil {
		return "", err
	}
	orig, readErr := os.ReadFile(t.ConfigPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	if err := os.WriteFile(backup, orig, 0o600); err != nil {
		return "", err
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err := os.WriteFile(t.ConfigPath, b, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

// Revert 用备份覆盖配置文件。备份为空表示原来没有这个文件，此时删掉它。
func Revert(t Target, backup string) error {
	if backup == "" {
		return errors.New("没有记录备份路径，无法还原")
	}
	b, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("读取备份 %s 失败: %w", backup, err)
	}
	if len(b) == 0 {
		err := os.Remove(t.ConfigPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(t.ConfigPath, b, 0o600)
}

// PointsAt 判断配置里的 base url 是否已经指向给定地址。
func PointsAt(t Target, baseURL string) bool {
	doc, err := readDoc(t.ConfigPath)
	if err != nil {
		return false
	}
	section := sectionOf(doc, t.KeyPath)
	cur, _ := section["ANTHROPIC_BASE_URL"].(string)
	return cur == baseURL
}

func readDoc(path string) (map[string]any, error) {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doc, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	return doc, nil
}

func sectionOf(doc map[string]any, keyPath []string) map[string]any {
	cur := doc
	for _, key := range keyPath {
		next, ok := cur[key].(map[string]any)
		if !ok {
			next = map[string]any{}
		}
		cur[key] = next
		cur = next
	}
	return cur
}

func setSection(doc map[string]any, keyPath []string, section map[string]any) {
	if len(keyPath) == 0 {
		return
	}
	cur := doc
	for _, key := range keyPath[:len(keyPath)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[key] = next
		}
		cur = next
	}
	cur[keyPath[len(keyPath)-1]] = section
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "、"
		}
		out += s
	}
	return out
}
