package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"lax/internal/store"
	"lax/internal/store/storetest"
)

// 沙箱日志与 --follow 的轮询要直接调内部函数：命令行那条路径要等信号，
// 在测试里没法干净地收尾。

// lockedBuffer 是并发安全的缓冲区：--follow 的循环在另一个 goroutine 里写。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pluginLogTestStore 建一个沙箱库并返回句柄。
func pluginLogTestStore(t *testing.T) *storetest.DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"init"}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败（退出码 %d）: %s", code, errOut.String())
	}
	return storetest.Open(t)
}

// TestPluginLogSink 钉住落库的四件事：级别从沙箱形状里取、取不到按 info、
// 正文压成一行、超长按字节截到上限且不切碎字符。
func TestPluginLogSink(t *testing.T) {
	st := pluginLogTestStore(t)
	var human bytes.Buffer
	sink := pluginLogSink(st.Store, "com.example.echo", &human)

	// lax.log 的形状：第二段是级别，第三段是正文。
	sink(pluginSandboxLogFormat, "com.example.echo", "warn", "上游超时")
	// 沙箱自己的告警没有级别，按 info 记。
	sink("插件 %s：读不到配置项\n下一行也该被压平", "com.example.echo")

	rows, err := st.PluginLogs("com.example.echo", 10, "", 0)
	if err != nil {
		t.Fatalf("读日志: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("日志条数 = %d，期望 2: %+v", len(rows), rows)
	}
	// 按时间倒序：最新的在前。
	if rows[0].Level != "info" || rows[0].Hook != "" {
		t.Fatalf("无级别的日志 = %+v，期望 info 且 hook 为空", rows[0])
	}
	if strings.ContainsAny(rows[0].Message, "\n\r") {
		t.Fatalf("日志正文没被压成一行: %q", rows[0].Message)
	}
	if rows[1].Level != "warn" || rows[1].Message != "上游超时" {
		t.Fatalf("lax.log 那一行 = %+v，期望级别 warn、正文不带前缀", rows[1])
	}
	if !strings.Contains(human.String(), "上游超时") {
		t.Fatalf("人看的输出里没有这一行: %s", human.String())
	}

	// 超长正文：落库的被截到 8KiB 以内，并且仍是合法 UTF-8。
	sink(pluginSandboxLogFormat, "com.example.echo", "info", strings.Repeat("字", 4000))
	rows, err = st.PluginLogs("com.example.echo", 1, "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读最新一条: %+v err=%v", rows, err)
	}
	if len(rows[0].Message) > pluginLogMaxBytes+len("…") {
		t.Fatalf("消息长度 = %d，超过上限 %d", len(rows[0].Message), pluginLogMaxBytes)
	}
	if !utf8.ValidString(rows[0].Message) {
		t.Fatalf("截断把字符切碎了: %q", rows[0].Message[max(0, len(rows[0].Message)-8):])
	}
}

// TestFollowPluginLogs 钉住 --follow 至少能读到一行新日志。
func TestFollowPluginLogs(t *testing.T) {
	st := pluginLogTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &lockedBuffer{}
	e := &Env{Stdout: out, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- followPluginLogs(ctx, e, st.Store, "com.example.echo", pluginLogQuery{Limit: 10}, 0)
	}()
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = st.AppendPluginLog(store.PluginLog{
			PluginID: "com.example.echo", Level: "info", Message: "跟随到的一行", At: time.Now().Unix(),
		})
	}()

	// 轮询间隔是 1 秒，给三轮的时间。
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "跟随到的一行") {
		if time.Now().After(deadline) {
			t.Fatalf("--follow 没读到新行，输出 = %q", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("--follow 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消之后 --follow 没有退出")
	}
}
