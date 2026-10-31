package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"lax/internal/store/storetest"
)

// model dialect 一组：看待、改绑与删掉绑定。绑定是 model_operations 的一行。

func dialectFixture(t *testing.T) {
	t.Helper()
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "mimo1", "mimo", "https://api.mimo.test", "openai-chat",
		"--key", "sk-test", "--models", "mimo-v2.5-tts"); code != 0 {
		t.Fatalf("建账号失败: %s", stderr)
	}
}

func TestModelDialectUsageErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有子命令", []string{"model", "dialect"}, "ls | set | unset"},
		{"set 参数不够", []string{"model", "dialect", "set", "m1"}, "用法："},
		{"未知操作", []string{"model", "dialect", "set", "m1", "chat.frobnicate@1", "lax/openai-chat@1"}, "不是宿主认识的操作"},
		{"unset 参数不够", []string{"model", "dialect", "unset", "m1"}, "用法："},
		{"未知子命令", []string{"model", "dialect", "nope"}, "未知子命令"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := run(t, c.args...)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2；stderr = %s", code, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("stderr 缺 %q: %s", c.want, stderr)
			}
		})
	}
}

func TestModelDialectSetListUnset(t *testing.T) {
	dialectFixture(t)

	code, out, stderr := run(t, "model", "dialect", "ls", "mimo-v2.5-tts")
	if code != 0 {
		t.Fatalf("空表 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "还没有方言绑定") {
		t.Fatalf("空表提示 = %s", out)
	}

	code, out, stderr = run(t, "model", "dialect", "set", "mimo-v2.5-tts",
		"speech.synthesize@1", "lax/mimo-chat-speech@1")
	if code != 0 {
		t.Fatalf("set 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "lax/mimo-chat-speech@1") {
		t.Fatalf("set 输出 = %s", out)
	}

	code, out, _ = run(t, "model", "dialect", "ls", "mimo-v2.5-tts", "--json")
	if code != 0 {
		t.Fatalf("ls --json 退出码 = %d", code)
	}
	var rows []struct {
		Model     string `json:"model"`
		Provider  string `json:"provider"`
		Operation string `json:"operation"`
		Dialect   string `json:"dialect"`
		Source    string `json:"source"`
		UpdatedAt int64  `json:"updated_at"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("ls --json 不是 JSON: %v，%s", err, out)
	}
	if len(rows) != 1 || rows[0].Dialect != "lax/mimo-chat-speech@1" ||
		rows[0].Operation != "speech.synthesize@1" || rows[0].Source != "manual" || rows[0].UpdatedAt == 0 {
		t.Fatalf("绑定行 = %+v", rows)
	}

	db := storetest.Open(t)
	if bound, ok, err := db.ModelOperation("mimo", "mimo-v2.5-tts", "speech.synthesize@1"); err != nil || !ok ||
		bound.DialectID != "lax/mimo-chat-speech@1" {
		t.Fatalf("库里的绑定 = %+v ok=%v err=%v", bound, ok, err)
	}

	if code, out, stderr := run(t, "model", "dialect", "unset", "mimo-v2.5-tts", "speech.synthesize@1"); code != 0 {
		t.Fatalf("unset 退出码 = %d，stderr = %s", code, stderr)
	} else if !strings.Contains(out, "已删掉") {
		t.Fatalf("unset 输出 = %s", out)
	}
	if _, ok, _ := db.ModelOperation("mimo", "mimo-v2.5-tts", "speech.synthesize@1"); ok {
		t.Fatal("绑定没删掉")
	}
	if code, _, stderr := run(t, "model", "dialect", "unset", "mimo-v2.5-tts", "speech.synthesize@1"); code != 1 {
		t.Fatalf("重复 unset 退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
}

// TestModelDialectSetWarnsOnPluginChatDialect 钉住对话操作绑到插件方言时的提醒：
// 命令照样写进去，但必须当场说明网关会拒。
func TestModelDialectSetWarnsOnPluginChatDialect(t *testing.T) {
	dialectFixture(t)
	code, out, stderr := run(t, "model", "dialect", "set", "mimo-v2.5-tts",
		"chat.complete@1", "com.example.plugin/chat@1")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "网关会拒绝") {
		t.Fatalf("缺提醒：%s", out)
	}

	code, out, _ = run(t, "model", "dialect", "set", "mimo-v2.5-tts",
		"chat.complete@1", "lax/openai-chat@1")
	if code != 0 {
		t.Fatalf("宿主方言退出码 = %d", code)
	}
	if strings.Contains(out, "网关会拒绝") {
		t.Fatalf("宿主方言不该被提醒：%s", out)
	}
}
