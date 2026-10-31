package gateway_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lax/internal/gateway"
	"lax/internal/store"
)

// 客户端自己挂断与上游断流是两件事：Go 把请求上下文的取消透到上游连接上，
// 读响应体就以 context canceled 醒来。收尾事件已经发给客户端的请求因此要记成成功，
// 否则每一次「客户端读够就关连接」都会在账上留下一条上游错误（DESIGN 5.2 第 6 条）。

// waitRequests 轮询到请求记录落库，最多等 3 秒。
func waitRequests(t *testing.T, st *store.Store, n int) []store.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		recs, err := st.RecentRequests(n)
		if err != nil {
			t.Fatalf("读请求记录: %v", err)
		}
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("等了 3 秒仍然只有 %d 条请求记录", len(recs))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// streamUntil 发一条流式请求，读到 stop 返回真（或读超时）就取消请求上下文并关闭连接。
func streamUntil(t *testing.T, url, body, marker string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()

	seen := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), marker) {
				break
			}
		}
		close(seen)
	}()

	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatalf("5 秒内没有读到 %q", marker)
	}
	// 读到标记就挂断：与 codex 收到 response.completed 后关连接同一种动作。
	cancel()
}

// responsesStreamBody 是一条 responses 入站的流式请求体。
const responsesStreamBody = `{"model":"m1","max_output_tokens":16,"stream":true,` +
	`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

// TestClientGoneAfterTerminalIsSuccess 覆盖「收尾事件发出去之后客户端挂断」：
// 上游后面还有用量帧与收尾帧，读它们时上下文已经取消，但这次请求对客户端已经完成，
// 账上记 ok=1，错误列为空。
func TestClientGoneAfterTerminalIsSuccess(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, `data: {"id":"c1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	f.add(t, "up1", gateway.ProtocolChat, up.URL, "sk-gone", []string{"m1"}, 100)
	srv := f.server(t)

	streamUntil(t, srv.URL+"/v1/responses", responsesStreamBody, "response.completed")

	recs := waitRequests(t, f.st, 1)
	if !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望 ok=1（收尾已发出后客户端挂断）", recs[0])
	}
	if recs[0].Error != "" {
		t.Fatalf("error = %q，期望空", recs[0].Error)
	}
}

// TestClientGoneBeforeTerminalIsClientError 覆盖「收尾之前客户端挂断」：
// 这一次没送完，记 ok=0，但错误说明写客户端提前断开，不写成上游流式响应中断。
func TestClientGoneBeforeTerminalIsClientError(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	f.add(t, "up1", gateway.ProtocolChat, up.URL, "sk-gone", []string{"m1"}, 100)
	srv := f.server(t)

	streamUntil(t, srv.URL+"/v1/responses", responsesStreamBody, "response.output_text.delta")

	recs := waitRequests(t, f.st, 1)
	if recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望 ok=0（收尾之前就断开）", recs[0])
	}
	if recs[0].Error != "客户端提前断开" {
		t.Fatalf("error = %q，期望「客户端提前断开」", recs[0].Error)
	}
}

// TestPassthroughClientGoneAfterTerminalIsSuccess 覆盖直通路径的同一件事：
// 收尾帧已经转发出去，之后客户端挂断不算断流，账上记 ok=1。
func TestPassthroughClientGoneAfterTerminalIsSuccess(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	f.add(t, "up1", gateway.ProtocolChat, up.URL, "sk-gone", []string{"m1"}, 100)
	srv := f.server(t)

	streamUntil(t, srv.URL+"/v1/chat/completions", `{"model":"m1","max_tokens":16,"stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`, "[DONE]")

	recs := waitRequests(t, f.st, 1)
	if !recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望 ok=1（收尾帧已转发后客户端挂断）", recs[0])
	}
	if recs[0].Error != "" {
		t.Fatalf("error = %q，期望空", recs[0].Error)
	}
}
