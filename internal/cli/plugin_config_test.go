package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plugin config 一组：manifest 声明的 options 的读写与默认值回落。

func writePluginWithOptions(t *testing.T) string {
	t.Helper()
	dir := writePluginFixture(t)
	path := filepath.Join(dir, "lax.plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读夹具 manifest: %v", err)
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasSuffix(text, "}") {
		t.Fatalf("夹具 manifest 形状变了: %s", text)
	}
	options := `,
  "options": {
    "region": {"type": "string", "enum": ["SGP", "RU"], "default": "SGP"},
    "retries": {"type": "number", "default": 2},
    "verbose": {"type": "boolean", "default": false}
  }
}`
	updated := text[:len(text)-1] + options
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("写夹具 manifest: %v", err)
	}
	return dir
}

func TestPluginConfig(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginWithOptions(t)
	if code, _, stderr := run(t, "plugin", "install", src); code != 0 {
		t.Fatalf("安装失败: %s", stderr)
	}

	code, out, stderr := run(t, "plugin", "config", "get", "com.example.echo", "--json")
	if code != 0 {
		t.Fatalf("get 退出码 = %d，stderr = %s", code, stderr)
	}
	var got struct {
		Plugin  string            `json:"plugin"`
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("get --json 不是 JSON: %v，%s", err, out)
	}
	if got.Options["region"] != "SGP" || got.Options["retries"] != "2" || got.Options["verbose"] != "false" {
		t.Fatalf("默认值 = %+v", got.Options)
	}

	if code, out, stderr := run(t, "plugin", "config", "set", "com.example.echo", "region", "RU"); code != 0 {
		t.Fatalf("set 退出码 = %d，stderr = %s", code, stderr)
	} else if !strings.Contains(out, "region = RU") {
		t.Fatalf("set 输出 = %s", out)
	}
	if code, out, _ := run(t, "plugin", "config", "get", "com.example.echo", "region"); code != 0 || !strings.Contains(out, "RU") {
		t.Fatalf("get region = %s（退出码 %d）", out, code)
	}

	// 枚举外取值、未知键、类型不符都拒绝，退出码 1，库里的值不动。
	for _, c := range [][]string{
		{"plugin", "config", "set", "com.example.echo", "region", "XX"},
		{"plugin", "config", "set", "com.example.echo", "nope", "1"},
		{"plugin", "config", "set", "com.example.echo", "retries", "many"},
		{"plugin", "config", "set", "com.example.echo", "verbose", "yes"},
	} {
		if code, _, stderr := run(t, c...); code != 1 {
			t.Fatalf("%v 退出码 = %d，期望 1；stderr = %s", c[4:], code, stderr)
		}
	}

	// --stdin 写值。
	if code, _, stderr := runWithStdin(t, strings.NewReader("true\n"),
		"plugin", "config", "set", "com.example.echo", "verbose", "--stdin"); code != 0 {
		t.Fatalf("--stdin 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, out, _ := run(t, "plugin", "config", "get", "com.example.echo", "verbose"); code != 0 || !strings.Contains(out, "true") {
		t.Fatalf("--stdin 之后 verbose = %s", out)
	}

	if code, out, stderr := run(t, "plugin", "config", "unset", "com.example.echo", "region"); code != 0 {
		t.Fatalf("unset 退出码 = %d，stderr = %s", code, stderr)
	} else if !strings.Contains(out, "默认值 SGP") {
		t.Fatalf("unset 输出 = %s", out)
	}
	if code, out, _ := run(t, "plugin", "config", "get", "com.example.echo", "region"); code != 0 || !strings.Contains(out, "SGP") {
		t.Fatalf("unset 后 region = %s", out)
	}
	if code, _, _ := run(t, "plugin", "config", "unset", "com.example.echo", "region"); code != 0 {
		t.Fatalf("重复 unset 应当是幂等的，退出码 = %d", code)
	}
}

// TestPluginConfigUnsetJSONWhenNotSet 钉住幂等删的 --json 形状：没设置过也返回
// 与真正删掉时同一个对象，只把 removed 写成 false，stdout 不混中文提示。
func TestPluginConfigUnsetJSONWhenNotSet(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginWithOptions(t)
	if code, _, stderr := run(t, "plugin", "install", src); code != 0 {
		t.Fatalf("安装失败: %s", stderr)
	}

	code, out, stderr := run(t, "plugin", "config", "unset", "com.example.echo", "region", "--json")
	if code != 0 {
		t.Fatalf("unset --json 退出码 = %d，stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("成功时不该写 stderr: %s", stderr)
	}
	var got struct {
		Plugin  string `json:"plugin"`
		Key     string `json:"key"`
		Removed *bool  `json:"removed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unset --json 不是 JSON: %v，%s", err, out)
	}
	if got.Plugin != "com.example.echo" || got.Key != "region" {
		t.Fatalf("unset --json = %+v", got)
	}
	if got.Removed == nil || *got.Removed {
		t.Fatalf("没设置过时 removed 应是 false: %s", out)
	}
}

func TestPluginConfigUsageErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// 不带子命令与 --help 等价：帮助写 stdout、退出码 0。
	if code, out, stderr := run(t, "plugin", "config"); code != 0 {
		t.Fatalf("不带子命令的退出码 = %d，期望 0；stderr = %s", code, stderr)
	} else if !strings.Contains(out, "unset") {
		t.Fatalf("不带子命令应打出子命令清单: %s", out)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"plugin", "config", "get"}, "用法："},
		{[]string{"plugin", "config", "set", "x"}, "用法："},
		{[]string{"plugin", "config", "set", "x", "k"}, "--stdin"},
		{[]string{"plugin", "config", "bogus"}, "未知子命令"},
	} {
		code, _, stderr := run(t, c.args...)
		if code != 2 || !strings.Contains(stderr, c.want) {
			t.Fatalf("%v 退出码 = %d，stderr = %s，期望 2 且提到 %q", c.args, code, stderr, c.want)
		}
	}
}
