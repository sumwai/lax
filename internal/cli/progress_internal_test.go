package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestProbeProgressFrames 钉住进度行的画法：一帧一条 \r、帧内不换行，
// 短帧补空格擦掉旧尾巴，收尾只补一个换行且重复调用不再写字节。
func TestProbeProgressFrames(t *testing.T) {
	var buf bytes.Buffer
	p := &probeProgress{w: &buf, on: true}

	p.update("ds1", 1, 3, "m1")
	first := buf.String()
	if !strings.HasPrefix(first, "\r") || strings.Count(first, "\n") != 0 {
		t.Fatalf("第一帧 = %q，期望以 \\r 开头且不换行", first)
	}
	for _, want := range []string{"ds1", "探活中", "1/3", "m1"} {
		if !strings.Contains(first, want) {
			t.Fatalf("第一帧缺少 %q: %q", want, first)
		}
	}

	// 先画一帧长的，再换回短的名字：新帧后面要补空格，否则旧尾巴留在屏幕上。
	buf.Reset()
	p.update("ds1", 2, 3, "a-much-longer-model-name-0123456789")
	buf.Reset()
	p.update("ds1", 3, 3, "m1")
	if last := buf.String(); !strings.HasSuffix(last, " ") {
		t.Fatalf("短帧应补空格擦掉旧尾巴: %q", last)
	}

	// 收尾：一个换行，且只加一次。
	buf.Reset()
	p.finish()
	p.update("ds1", 3, 3, "m1")
	buf.Reset()
	p.finish()
	p.finish()
	if got := buf.String(); got != "\n" {
		t.Fatalf("收尾 = %q，期望正好一个换行", got)
	}

	// 没画过帧时收尾不写字节：待探模型全被跳过的账号不该留一个空行。
	var empty bytes.Buffer
	q := &probeProgress{w: &empty, on: true}
	q.finish()
	if empty.Len() != 0 {
		t.Fatalf("没画过进度时收尾不应写字节: %q", empty.String())
	}
}

// TestProbeProgressDisabledWritesNothing 确认进度关闭时（非终端、--json、空指针）
// 一个字节都不写。
func TestProbeProgressDisabledWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	for _, p := range []*probeProgress{
		newProbeProgress(&buf, true), // bytes.Buffer 不是字符设备
		newProbeProgress(&buf, false),
		nil,
	} {
		p.update("ds1", 1, 2, "m1")
		p.finish()
	}
	if buf.Len() != 0 {
		t.Fatalf("进度关闭时不该有输出: %q", buf.String())
	}
}

// TestProbeProgressConcurrent 让多个 worker 同时刷帧，配 -race 复核加锁是否够用，
// 并确认 16 个模型全部完成时有一帧是 16/16。
func TestProbeProgressConcurrent(t *testing.T) {
	var buf bytes.Buffer
	p := &probeProgress{w: &buf, on: true}

	var wg sync.WaitGroup
	for i := 1; i <= 16; i++ {
		wg.Add(1)
		go func(done int) {
			defer wg.Done()
			p.update("ds1", done, 16, "m1")
		}(i)
	}
	wg.Wait()
	p.finish()

	if !strings.Contains(buf.String(), "16/16") {
		t.Fatalf("应有一帧是 16/16: %q", buf.String())
	}
}
