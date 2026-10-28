package agentcfg_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"switchboard/internal/agentcfg"
)

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
}

func readEnv(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	var doc struct {
		Env map[string]any `json:"env"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析: %v", err)
	}
	return doc.Env
}

func TestApplyPreservesOtherKeysAndReverts(t *testing.T) {
	home := t.TempDir()
	target, err := agentcfg.Lookup(home, "claude-code")
	if err != nil {
		t.Fatalf("查找客户端: %v", err)
	}

	original := `{
  "model": "claude-sonnet-4",
  "permissions": {"allow": ["Bash(ls)"]},
  "env": {"EXISTING": "1", "ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
}
`
	writeSettings(t, target.ConfigPath, original)
	before, err := os.ReadFile(target.ConfigPath)
	if err != nil {
		t.Fatalf("读原文件: %v", err)
	}

	desired := agentcfg.Desired("http://127.0.0.1:3425", "deepseek-v4-pro")
	changes, err := agentcfg.Diff(target, desired)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	keys := map[string]bool{}
	for _, c := range changes {
		keys[c.Key] = true
	}
	if len(changes) != 3 || !keys["ANTHROPIC_BASE_URL"] || !keys["ANTHROPIC_AUTH_TOKEN"] || !keys["ANTHROPIC_MODEL"] {
		t.Fatalf("差异 = %+v", changes)
	}

	backup, err := agentcfg.Apply(target, desired)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	env := readEnv(t, target.ConfigPath)
	if env["EXISTING"] != "1" {
		t.Fatal("其它 env 键被改动了")
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:3425" {
		t.Fatalf("BASE_URL = %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_MODEL"] != "deepseek-v4-pro" {
		t.Fatalf("MODEL = %v", env["ANTHROPIC_MODEL"])
	}

	var doc map[string]any
	b, _ := os.ReadFile(target.ConfigPath)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析写入结果: %v", err)
	}
	if doc["model"] != "claude-sonnet-4" {
		t.Fatal("顶层 model 键被改动了")
	}
	if !agentcfg.PointsAt(target, "http://127.0.0.1:3425") {
		t.Fatal("PointsAt 应返回 true")
	}
	if agentcfg.PointsAt(target, "http://127.0.0.1:9999") {
		t.Fatal("地址不同时 PointsAt 应返回 false")
	}

	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("备份不存在: %v", err)
	}

	changes, err = agentcfg.Diff(target, desired)
	if err != nil {
		t.Fatalf("二次 Diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("写入后应无差异，实际 %+v", changes)
	}

	if err := agentcfg.Revert(target, backup); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	after, err := os.ReadFile(target.ConfigPath)
	if err != nil {
		t.Fatalf("读还原结果: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("还原后内容不一致:\n得到 %s\n期望 %s", after, before)
	}
}

func TestApplyCreatesMissingFile(t *testing.T) {
	home := t.TempDir()
	target, err := agentcfg.Lookup(home, "claude-code")
	if err != nil {
		t.Fatalf("查找客户端: %v", err)
	}

	desired := agentcfg.Desired("http://127.0.0.1:3425", "")
	changes, err := agentcfg.Diff(target, desired)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("差异 = %+v，期望只有两个键（没给模型）", changes)
	}

	if _, err := agentcfg.Apply(target, desired); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	env := readEnv(t, target.ConfigPath)
	if env["ANTHROPIC_AUTH_TOKEN"] != agentcfg.LocalToken {
		t.Fatalf("AUTH_TOKEN = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Fatal("没给模型时不应写入 ANTHROPIC_MODEL")
	}
}

func TestLookupUnknownClient(t *testing.T) {
	if _, err := agentcfg.Lookup(t.TempDir(), "nope"); err == nil {
		t.Fatal("未知客户端应报错并列出可用项")
	}
}

func TestRevertWithoutBackup(t *testing.T) {
	target, err := agentcfg.Lookup(t.TempDir(), "claude-code")
	if err != nil {
		t.Fatalf("查找客户端: %v", err)
	}
	if err := agentcfg.Revert(target, ""); err == nil {
		t.Fatal("没有备份时应报错")
	}
}
