package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// probeProgress 是 model probe 的实时进度行。每帧用 \r 回到行首原地重画，
// 例如「ds1 探活中 37/94  deepseek-v4.1-flash」：账号名、已完成数 / 待探总数、
// 最近完成的那个模型。并发大于 1 时显示的就是刚回来的那个，人看到的是一条
// 一直在动的行，而不是几十秒空白之后突然冒出整张表。
//
// 只在 e.Stderr 是终端时开启（--json 也不开）：管道、CI、测试里的 stderr 都不是
// 字符设备，这时一个字节都不写，日志与脚本输出因此不受影响。
type probeProgress struct {
	mu sync.Mutex
	w  io.Writer
	on bool
	// used 是上一帧占的列数。新帧更短时补空格擦掉旧尾巴，
	// 否则模型名一变长再变短会留下一截残影。
	used int
}

// newProbeProgress 按输出目标与开关决定是否画进度。
func newProbeProgress(w io.Writer, enabled bool) *probeProgress {
	return &probeProgress{w: w, on: enabled && isTerminal(w)}
}

// isTerminal 判断 w 是不是终端。只有 *os.File 才可能，而且 Stat 要报字符设备；
// bytes.Buffer、管道与重定向到文件都落在 false，所以不引入额外依赖也能判准。
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// update 画一帧：done 是已完成数，total 是本次待探总数，model 是刚完成的模型。
// 可能从任意 worker goroutine 调进来，内部加锁，不会两帧交错。
func (p *probeProgress) update(account string, done, total int, model string) {
	if p == nil || !p.on {
		return
	}
	// 模型名截到 40 列：账号名加上计数通常不超过 70 列，不会在 80 列的终端里折行。
	line := fmt.Sprintf("%s 探活中 %d/%d  %s", account, done, total, truncate(model, 40))

	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	b.WriteString("\r")
	b.WriteString(line)
	if gap := p.used - displayWidth(line); gap > 0 {
		b.WriteString(strings.Repeat(" ", gap))
	}
	p.used = displayWidth(line)
	fmt.Fprint(p.w, b.String())
}

// finish 给这个账号的进度行收一次尾：补一个换行，后面的汇总行与表格从新行开始。
// 没画过任何一帧（账号的待探模型全被跳过、或者进度本来就没开）时不写字节；
// 重复调用安全，出错返回的路径上也靠它兜底，错误行不会接在进度行后面。
func (p *probeProgress) finish() {
	if p == nil || !p.on {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used == 0 {
		return
	}
	fmt.Fprint(p.w, "\n")
	p.used = 0
}
