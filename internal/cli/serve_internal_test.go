package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServeLogWriterRoutesJSONToStderr 钉住 serve 日志去向的选择规则。
func TestServeLogWriterRoutesJSONToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := serveLogWriter(&Env{Stdout: &stdout, Stderr: &stderr}); got != &stdout {
		t.Fatal("普通模式日志应写 stdout")
	}
	if got := serveLogWriter(&Env{Stdout: &stdout, Stderr: &stderr, JSON: true}); got != &stderr {
		t.Fatal("--json 模式日志应写 stderr")
	}
}

// TestServeJSONKeepsStdoutPure 起一次真实的 serve：--json 的启动状态写 stdout，
// 之后直到退出 stdout 不再多一个字节；运行时日志改走 stderr。
//
// 用 /healthz 确认 Serve 已经进入循环、signal.NotifyContext 已经注册，再发 SIGINT 收尾：
// 刚看到启动状态就发信号可能撞上进程的默认动作，把测试进程本身杀掉。
func TestServeJSONKeepsStdoutPure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	var initOut, initErr bytes.Buffer
	if code := Run([]string{"init"}, &initOut, &initErr); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, initErr.String())
	}

	out := &lockedBuffer{}
	errOut := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"serve", "--json", "--addr", "127.0.0.1:0"}, out, errOut)
	}()

	var status struct {
		Addr string `json:"addr"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := out.String()
		if strings.Contains(text, `"addr"`) {
			if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &status); err != nil {
				t.Fatalf("启动状态不是 JSON: %v，原文 %q", err, text)
			}
			break
		}
		select {
		case code := <-done:
			t.Fatalf("serve 提前退出：退出码 %d，stderr = %s", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve 没有打出启动状态：stdout = %q，stderr = %q", out.String(), errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	client := &http.Client{Timeout: time.Second}
	healthy := false
	for !healthy && time.Now().Before(deadline) {
		resp, err := client.Get("http://" + status.Addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			healthy = resp.StatusCode == http.StatusOK
		}
		if !healthy {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !healthy {
		t.Fatalf("/healthz 没有就绪：stderr = %s", errOut.String())
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("发 SIGINT: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("收到 SIGINT 后退出码 = %d，stderr = %s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGINT 之后 serve 没有退出")
	}

	// 启动状态之后只要多出一段文本，第二次 Unmarshal 就会报
	// invalid character after top-level value。
	text := strings.TrimSpace(out.String())
	var rest map[string]any
	if err := json.Unmarshal([]byte(text), &rest); err != nil {
		t.Fatalf("stdout 应当只有一个 JSON 对象，实际 %q: %v", text, err)
	}
	if got, _ := rest["addr"].(string); got != status.Addr {
		t.Fatalf("addr = %q，期望 %q", rest["addr"], status.Addr)
	}
}
