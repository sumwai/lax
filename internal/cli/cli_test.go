package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switchboard/internal/cli"
)

// run 执行一条子命令，返回退出码与合并后的输出。
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func sandbox(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
}

// fakeUpstream 是一个假的 openai-chat 上游，回一句固定文本。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"凭据不对"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWalkingSkeleton(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)

	code, out, errOut := run(t, "init")
	if code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "配置") || !strings.Contains(out, "下一步") {
		t.Fatalf("init 输出 = %s", out)
	}

	code, _, errOut = run(t, "account", "add", "ds1",
		"--provider", "custom", "--base-url", up.URL, "--key", "sk-test", "--models", "m1")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, errOut)
	}

	code, out, _ = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d", code)
	}
	if strings.Contains(out, "sk-test") {
		t.Fatal("account ls 不应输出凭据明文")
	}
	var accounts []map[string]any
	if err := json.Unmarshal([]byte(out), &accounts); err != nil {
		t.Fatalf("account ls 输出不是 JSON: %v，%s", err, out)
	}
	if len(accounts) != 1 || accounts[0]["name"] != "ds1" || accounts[0]["status"] != "正常" {
		t.Fatalf("account ls = %+v", accounts)
	}

	code, out, _ = run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d", code)
	}
	var models []map[string]any
	if err := json.Unmarshal([]byte(out), &models); err != nil {
		t.Fatalf("model ls 输出不是 JSON: %v，%s", err, out)
	}
	if len(models) != 1 || models[0]["model"] != "m1" {
		t.Fatalf("model ls = %+v", models)
	}

	code, out, errOut = run(t, "call", "hi", "--model", "m1")
	if code != 0 {
		t.Fatalf("call 退出码 = %d，stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("call 输出 = %s", out)
	}

	code, out, _ = run(t, "log", "tail", "-n", "5", "--json")
	if code != 0 {
		t.Fatalf("log tail 退出码 = %d", code)
	}
	var records []map[string]any
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("log tail 输出不是 JSON: %v，%s", err, out)
	}
	if len(records) != 1 || records[0]["ok"] != true {
		t.Fatalf("请求记录 = %+v", records)
	}
	if records[0]["in_tokens"].(float64) != 7 {
		t.Fatalf("输入 token 记录 = %v", records[0]["in_tokens"])
	}

	code, out, _ = run(t, "usage", "--since", "1h", "--by", "account", "--json")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d", code)
	}
	var usageRows []map[string]any
	if err := json.Unmarshal([]byte(out), &usageRows); err != nil {
		t.Fatalf("usage 输出不是 JSON: %v，%s", err, out)
	}
	if len(usageRows) != 1 || usageRows[0]["key"] != "ds1" || usageRows[0]["requests"].(float64) != 1 {
		t.Fatalf("usage = %+v", usageRows)
	}

	code, out, _ = run(t, "doctor")
	if code != 0 {
		t.Fatalf("doctor 退出码 = %d，输出 %s", code, out)
	}
	if strings.Contains(out, "fail") {
		t.Fatalf("doctor 有失败项: %s", out)
	}
}

func TestAgentConfigCycle(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}
	if code, _, stderr := run(t, "account", "add", "ds1",
		"--provider", "custom", "--base-url", up.URL, "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	settingsPath := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")

	code, out, _ := run(t, "agent", "ls", "--json")
	if code != 0 {
		t.Fatalf("agent ls 退出码 = %d", code)
	}
	if !strings.Contains(out, "claude-code") {
		t.Fatalf("agent ls = %s", out)
	}

	code, out, _ = run(t, "agent", "use", "claude-code", "--model", "m1")
	if code != 0 {
		t.Fatalf("agent use 退出码 = %d", code)
	}
	if !strings.Contains(out, "将修改") || !strings.Contains(out, "--apply") {
		t.Fatalf("未加 --apply 时应只打印差异: %s", out)
	}
	if _, err := os.Stat(settingsPath); err == nil {
		t.Fatal("未加 --apply 时不应写文件")
	}

	code, out, errOut := run(t, "agent", "use", "claude-code", "--model", "m1", "--apply")
	if code != 0 {
		t.Fatalf("agent use --apply 退出码 = %d，stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "已写入") {
		t.Fatalf("输出 = %s", out)
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("配置文件未写入: %v", err)
	}
	if !strings.Contains(string(b), "http://127.0.0.1:3425") {
		t.Fatalf("配置文件内容 = %s", b)
	}

	code, out, _ = run(t, "agent", "ls", "--json")
	if code != 0 || !strings.Contains(out, `"points_at_switchboard": true`) {
		t.Fatalf("agent ls 应显示已指向: %s", out)
	}

	code, _, errOut = run(t, "agent", "revert", "claude-code")
	if code != 0 {
		t.Fatalf("agent revert 退出码 = %d，stderr = %s", code, errOut)
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatal("原来不存在的配置文件，还原后应被删掉")
	}
}

func TestUsageErrorExitCode(t *testing.T) {
	sandbox(t)

	code, _, errOut := run(t, "account")
	if code != 2 {
		t.Fatalf("缺少子命令的退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errOut, "add") {
		t.Fatalf("stderr 应列出可用子命令: %s", errOut)
	}

	code, _, _ = run(t, "nope")
	if code != 2 {
		t.Fatalf("未知子命令的退出码 = %d，期望 2", code)
	}

	code, _, errOut = run(t, "account", "ls")
	if code != 1 {
		t.Fatalf("未初始化时 account ls 的退出码 = %d，期望 1", code)
	}
	if !strings.Contains(errOut, "switchboard init") {
		t.Fatalf("stderr 应给出下一步命令: %s", errOut)
	}
}

func TestAccountAddRejectsMissingKey(t *testing.T) {
	sandbox(t)
	if code, _, _ := run(t, "init"); code != 0 {
		t.Fatal("init 失败")
	}

	code, _, errOut := run(t, "account", "add", "ds1", "--provider", "deepseek")
	if code != 2 {
		t.Fatalf("退出码 = %d，缺必填参数应算用法错误", code)
	}
	if !strings.Contains(errOut, "SWITCHBOARD_KEY") {
		t.Fatalf("stderr 应提示凭据来源: %s", errOut)
	}

	code, _, errOut = run(t, "account", "add", "ds1", "--provider", "不存在的厂商", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d", code)
	}
	if !strings.Contains(errOut, "--base-url") {
		t.Fatalf("stderr 应提示补 --base-url: %s", errOut)
	}
}
