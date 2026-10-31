package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lax/internal/gateway"
	"lax/internal/store"
)

// dropConnection 把这次连接直接关掉，不写任何响应：客户端一条 HTTP 响应都拿不到，
// 于是 s.client.Do 返回错误——这正是「连接层失败」那一档。
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("假上游不支持 Hijack，构造不出连接层失败")
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("取连接失败: %v", err)
		return
	}
	_ = conn.Close()
}

// TestProbeModelTransportRetrySucceeds 钉住连接层失败的重试：同一个协议内最多再试两次
// （共 3 次调用）、不换协议，前两次断连、第三次 2xx 时结论就是 allowed。
// 三次调用各留一行账：前两行以「连接层失败重试，未作结论」开头，第三行是判定行。
func TestProbeModelTransportRetrySucceeds(t *testing.T) {
	f := newFixture(t)

	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			dropConnection(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-probe", []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 3 {
		t.Fatalf("上游调用 = %d 次，期望 3 次（两次连接层失败重试后成功）", got)
	}
	if res.Access != store.AccessAllowed || res.Undecided {
		t.Fatalf("结论 = %+v，期望 allowed 且不是未判定", res)
	}
	if res.Protocol != gateway.ProtocolChat {
		t.Fatalf("结论里的协议 = %q，期望 openai-chat（重试不换协议）", res.Protocol)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("请求记录 = %d 行，期望 3 行（打了几次就记几行）: %+v", len(recs), recs)
	}
	retries := 0
	var verdict store.RequestRecord
	for _, rec := range recs {
		if strings.HasPrefix(rec.Error, probeTransportRetryNoteTest) {
			retries++
			if rec.HTTPStatus != 0 || rec.OK {
				t.Fatalf("重试行应为 0 且不 ok: %+v", rec)
			}
			if rec.InTokens != 0 || rec.OutTokens != 0 {
				t.Fatalf("重试行不该带用量: %+v", rec)
			}
			continue
		}
		verdict = rec
	}
	if retries != 2 {
		t.Fatalf("重试行 = %d 行，期望 2 行: %+v", retries, recs)
	}
	if verdict.HTTPStatus != http.StatusOK || !verdict.OK {
		t.Fatalf("判定行 = %+v，期望 200 且 ok", verdict)
	}
	if verdict.Inbound != gateway.ProtocolChat || verdict.Outbound != gateway.ProtocolChat {
		t.Fatalf("判定行的协议对 = %s→%s，期望 openai-chat→openai-chat", verdict.Inbound, verdict.Outbound)
	}
	if verdict.TS == 0 {
		t.Fatal("判定行应带开始时间")
	}
}

// TestProbeModelTransportFailureKeepsAccess 钉住「连接层失败不改可达性」：假上游每次都断连，
// 库里已确认的 allowed 行照旧是 allowed，checked_at 也没被改成这次探活的时间；
// 结论带 Undecided、Access 留空，由调用方保留上一行。
func TestProbeModelTransportFailureKeepsAccess(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dropConnection(t, w)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-probe", []string{"m1"}, 100)
	// 先写一行「已确认可用」：checked_at 给一个明显的过去时间，探活不该把它改成现在。
	checkedAt := time.Now().Add(-3 * time.Hour).Unix()
	if err := f.st.SetAccountModelAccess(acc.ID, "m1", store.AccessAllowed, http.StatusOK, "", checkedAt); err != nil {
		t.Fatalf("写已确认可用的行: %v", err)
	}
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if !res.Undecided || res.Access != "" {
		t.Fatalf("结论 = %+v，期望 Access 留空且 Undecided 为真", res)
	}
	if res.Status != store.ModelError {
		t.Fatalf("状态 = %q，期望旧词表仍记 %q 供显示", res.Status, store.ModelError)
	}

	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "m1" {
		t.Fatalf("模型行 = %+v，期望只有 m1 一行", rows)
	}
	if rows[0].Access != store.AccessAllowed {
		t.Fatalf("可达性 = %q，期望保留 %q（连接层失败不是模型层面的结论）", rows[0].Access, store.AccessAllowed)
	}
	if rows[0].CheckedAt != checkedAt {
		t.Fatalf("checked_at = %d，期望保留 %d（这次没拿到判定就不动探活时间）", rows[0].CheckedAt, checkedAt)
	}
}
