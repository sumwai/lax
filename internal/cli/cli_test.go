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

	"lax/internal/cli"
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
	// 默认端口 4567 是固定值：本机别的服务占着它时 doctor 会照实报 fail，
	// 而这条用例验的是整条链路，不是端口恰好空闲。改成一个由内核分配的端口再往下走。
	useFreePort(t)

	code, _, errOut = addAccount(t, "ds1", "custom", up.URL, "", "--key", "sk-test", "--models", "m1")
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

	code, out, errOut = run(t, "model", "call", "hi", "--model", "m1")
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

// useFreePort 把沙箱里的监听地址改成 127.0.0.1:0。doctor 的端口检查因此仍走同一条路径，
// 只是不再要求这台机器的 4567 恰好空着：本机可能有别的服务长期占用它。
func useFreePort(t *testing.T) {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "lax", "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析配置: %v", err)
	}
	cfg["addr"] = "127.0.0.1:0"
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("编码配置: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("写配置: %v", err)
	}
}

func TestAccountAddReadsKeyFromStdin(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}

	saved := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道: %v", err)
	}
	if _, err := w.WriteString("  sk-test\n\n"); err != nil {
		t.Fatalf("写管道: %v", err)
	}
	_ = w.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; _ = r.Close() })

	code, _, errOut := addAccount(t, "ds1", "custom", up.URL, "", "--key", "-", "--models", "m1")
	if code != 0 {
		t.Fatalf("account add --key - 退出码 = %d，stderr = %s", code, errOut)
	}

	// 假上游只接受 Bearer sk-test，能调通就说明首尾空白被去掉、凭据可用。
	code, out, errOut := run(t, "model", "call", "hi", "--model", "m1")
	if code != 0 {
		t.Fatalf("call 退出码 = %d，stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("call 输出 = %s", out)
	}
}

func TestAccountEnableDisable(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", up.URL, "", "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}

	if code, _, stderr := run(t, "account", "disable", "ds1"); code != 0 {
		t.Fatalf("disable 失败: %s", stderr)
	}
	code, out, _ := run(t, "account", "ls", "--json")
	if code != 0 || !strings.Contains(out, `"enabled": false`) || !strings.Contains(out, `"status": "停用"`) {
		t.Fatalf("停用后状态不对: %s", out)
	}

	if code, _, stderr := run(t, "account", "enable", "ds1"); code != 0 {
		t.Fatalf("enable 失败: %s", stderr)
	}
	code, out, _ = run(t, "account", "ls", "--json")
	if code != 0 || !strings.Contains(out, `"enabled": true`) || !strings.Contains(out, `"status": "正常"`) {
		t.Fatalf("启用后状态不对: %s", out)
	}
}

// TestManualRequestCommandsLiveUnderModel 钉住四条手工请求命令的位置：它们在 model 一组里，
// 不是一级子命令。文档与帮助都按这个位置写，分发器与帮助对不上时没人会注意到。
func TestManualRequestCommandsLiveUnderModel(t *testing.T) {
	sandbox(t)

	for _, name := range []string{"call", "decide", "speak", "transcribe"} {
		code, out, errOut := run(t, name)
		if code != 2 {
			t.Fatalf("一级子命令 %s 的退出码 = %d，期望 2（已移进 model 一组）", name, code)
		}
		if !strings.Contains(errOut, "未知子命令") {
			t.Fatalf("一级子命令 %s 应报未知子命令: %s", name, errOut)
		}
		if out != "" {
			t.Fatalf("一级子命令 %s 不该写 stdout: %s", name, out)
		}
	}

	code, out, errOut := run(t, "model", "--help")
	if code != 0 {
		t.Fatalf("model --help 退出码 = %d，stderr = %s", code, errOut)
	}
	for _, want := range []string{"call", "decide", "speak", "transcribe"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model --help 的子命令清单缺 %q:\n%s", want, out)
		}
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
	if !strings.Contains(errOut, "lax init") {
		t.Fatalf("stderr 应给出下一步命令: %s", errOut)
	}
}

func TestAccountAddRejectsMissingKey(t *testing.T) {
	sandbox(t)
	if code, _, _ := run(t, "init"); code != 0 {
		t.Fatal("init 失败")
	}

	code, _, errOut := addAccount(t, "ds1", "deepseek", "", "")
	if code != 2 {
		t.Fatalf("退出码 = %d，缺必填参数应算用法错误", code)
	}
	if !strings.Contains(errOut, "LAX_KEY") {
		t.Fatalf("stderr 应提示凭据来源: %s", errOut)
	}

	code, _, errOut = run(t, "account", "add", "ds1", "--provider", "不存在的厂商", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d", code)
	}
	if !strings.Contains(errOut, "lax provider add") {
		t.Fatalf("stderr 应提示先登记厂商: %s", errOut)
	}
}

// TestUnknownFlagIsUsageError 钉住「参数名写错」的退出码：2（用法错误），不是 1（运行失败），
// 并把该子命令的用法行与参数清单打到 stderr，stdout 一个字都不写。
func TestUnknownFlagIsUsageError(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "account", "ls", "--nope")
	if code != 2 {
		t.Fatalf("未知参数的退出码 = %d，期望 2（用法错误）", code)
	}
	if out != "" {
		t.Fatalf("用法错误不该写 stdout: %s", out)
	}
	for _, want := range []string{"-nope", "用法：", "--json"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
}

// TestGroupHelp 确认有子命令的组都支持 -h、--help 与 help：
// 清单打到 stdout，退出码 0，且不需要先 init。
func TestGroupHelp(t *testing.T) {
	sandbox(t)
	groups := []struct {
		name string
		subs []string
	}{
		{"account", []string{"add", "ls", "set", "rm", "test", "enable", "disable"}},
		{"model", []string{"ls", "sync", "probe"}},
		{"provider", []string{"ls", "add", "set", "unset", "rm"}},
		{"log", []string{"tail"}},
	}
	for _, g := range groups {
		for _, want := range []string{"-h", "--help", "help"} {
			code, out, errOut := run(t, g.name, want)
			if code != 0 {
				t.Fatalf("%s %s 的退出码 = %d，期望 0", g.name, want, code)
			}
			if errOut != "" {
				t.Fatalf("%s %s 的帮助不该写 stderr: %s", g.name, want, errOut)
			}
			if !strings.Contains(out, "用法：") || !strings.Contains(out, "子命令：") {
				t.Fatalf("%s %s 的输出缺少用法或子命令清单: %s", g.name, want, out)
			}
			for _, sub := range g.subs {
				if !strings.Contains(out, sub) {
					t.Fatalf("%s %s 的清单缺少 %s: %s", g.name, want, sub, out)
				}
			}
		}
	}
}

// TestTopLevelHelpListsProviderGroup 钉住顶层帮助里的命令清单：
// provider 必须出现在顶层帮助里，同时 provider 一组本身要能被分发到（见 TestProviderLsSeeded）。
func TestTopLevelHelpListsProviderGroup(t *testing.T) {
	sandbox(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		code, out, errOut := run(t, arg)
		if code != 0 {
			t.Fatalf("%s 的退出码 = %d，期望 0", arg, code)
		}
		if errOut != "" {
			t.Fatalf("%s 的帮助不该写 stderr: %s", arg, errOut)
		}
		for _, want := range []string{"provider", "厂商与路由"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s 的顶层帮助缺 %q: %s", arg, want, out)
			}
		}
	}
}

// TestGroupUnknownSubcommandHints 确认未知子命令仍退出码 2，并提示去看组级帮助；子命令自己的 --help 保持原样，退出码 0。
func TestGroupUnknownSubcommandHints(t *testing.T) {
	sandbox(t)
	for _, group := range []string{"account", "model", "provider", "log"} {
		code, out, errOut := run(t, group, "nope")
		if code != 2 {
			t.Fatalf("%s nope 的退出码 = %d，期望 2", group, code)
		}
		if out != "" {
			t.Fatalf("用法错误不该写 stdout: %s", out)
		}
		if !strings.Contains(errOut, "lax "+group+" --help") {
			t.Fatalf("%s nope 的提示应给出组级帮助命令: %s", group, errOut)
		}
	}

	code, _, errOut := run(t, "model", "probe", "--help")
	if code != 0 {
		t.Fatalf("model probe --help 的退出码 = %d，期望 0", code)
	}
	if !strings.Contains(errOut, "--max-age") {
		t.Fatalf("子命令帮助应列出自己的参数: %s", errOut)
	}
}
