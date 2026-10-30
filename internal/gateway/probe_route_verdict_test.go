package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// 这个文件钉住「路由收紧之后」探活的两条结论：
// 401 是凭据问题，不换协议、停用账号；403 只是这条路径不收，按厂商声明过的路由接着试，
// 没有下一条时才按结论记录。厂商行、路由与厂商默认协议都取迁移 0006 种进去的真表，
// 厂商的 base_url 指到 httptest 假上游，因此断言的是「打了几次、打到哪条路由」。

// TestProbeModelUnauthorizedSingleRouteStops 覆盖只有一条 anthropic 路由的厂商上探活遇到 401：
// 只发一次上游请求（不去试这家没声明的 chat / responses 路径），账号被停用，
// 不写协议发现结果，请求记录里那一行是 401。
func TestProbeModelUnauthorizedSingleRouteStops(t *testing.T) {
	f := newFixture(t)
	const key = "sk-probe-401"
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), "")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"凭据无效"}}`))
	}))
	defer up.Close()

	// 内置 anthropic 厂商只有 /v1/messages 一条路由。
	acc := f.addAs(t, "a1", "anthropic", gateway.ProtocolAnthropic, up.URL, key, []string{"claude-x"})
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelAuthFailed || res.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("结论 = %+v，期望 auth/401", res)
	}

	// 只打了一次，打的是唯一声明过的那条路由，凭据头是 anthropic 的 x-api-key。
	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用 = %v，期望只打声明过的那一条路由", rec.paths())
	}
	if calls[0].path != "/v1/messages" || calls[0].apiKey != key || calls[0].bearer != "" {
		t.Fatalf("上游调用 = %+v，期望 POST /v1/messages 只带 x-api-key", calls[0])
	}

	// 401 停用账号，且不写协议发现结果：401 不是模型层面的结论，可达性也不由这次探活给出。
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if stored.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	if res.Access != "" {
		t.Fatalf("401 不该给出可达性结论，实际 %q", res.Access)
	}
	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "claude-x" || rows[0].Protocol != "" {
		t.Fatalf("401 不该写入协议发现结果: %+v", rows)
	}

	// 只有一次调用，因此只有一行请求记录，协议对是 anthropic→anthropic。
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d，期望 1（401 不换协议）", len(recs))
	}
	if recs[0].HTTPStatus != http.StatusUnauthorized || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望 401 且 ok=0", recs[0])
	}
	if recs[0].Inbound != gateway.ProtocolAnthropic || recs[0].Outbound != gateway.ProtocolAnthropic {
		t.Fatalf("请求记录的协议对 = %s→%s，期望 anthropic→anthropic", recs[0].Inbound, recs[0].Outbound)
	}
}

// TestProbeModelForbiddenFollowsDeclaredRoutes 覆盖 403 之后按厂商声明过的路由接着试：
// deepseek 声明了 anthropic 与 openai-chat 两条路由，anthropic 那次 403，chat 那次 2xx，
// 结论是 usable/openai-chat，发现结果写进 account_models.protocol，中间那次记为未作结论。
func TestProbeModelForbiddenFollowsDeclaredRoutes(t *testing.T) {
	f := newFixture(t)
	const key = "sk-probe-403"
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), "")
		switch r.URL.Path {
		case "/anthropic/v1/messages":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"claude-* 只接受 /messages"}}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"没有这个路由"}}`))
		}
	}))
	defer up.Close()

	// 这一家的默认协议是 anthropic（默认协议只有厂商行上这一份）：厂商默认协议那次先试，
	// 403 之后才轮到同样声明过的 chat 路由。
	def := gateway.ProtocolAnthropic
	if err := f.st.SetProvider("deepseek", store.ProviderPatch{DefaultProtocol: &def}); err != nil {
		t.Fatalf("改默认协议: %v", err)
	}
	acc := f.addAs(t, "ds1", "deepseek", gateway.ProtocolAnthropic, up.URL, key, []string{"claude-x"})
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelUsable || res.Protocol != gateway.ProtocolChat {
		t.Fatalf("结论 = %+v，期望 usable/openai-chat", res)
	}

	paths := rec.paths()
	want := []string{"/anthropic/v1/messages", "/v1/chat/completions"}
	if len(paths) != len(want) {
		t.Fatalf("上游调用 = %v，期望 %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("第 %d 次调用路径 = %q，期望 %q（全部 %v）", i+1, paths[i], want[i], paths)
		}
	}

	// 发现结果落在模型行上，转发从此按 chat 打。
	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != gateway.ProtocolChat {
		t.Fatalf("模型行 = %+v，期望 protocol=openai-chat", rows)
	}

	// 两次调用两行账：403 那行带「换协议前的尝试」，判定行是 2xx。
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("请求记录数 = %d，期望 2（403 一次 + 2xx 一次）", len(recs))
	}
	verdict := probeVerdict(t, recs)
	if !verdict.OK || verdict.HTTPStatus != http.StatusOK || verdict.Outbound != gateway.ProtocolChat {
		t.Fatalf("判定行 = %+v，期望 200/openai-chat 且 ok", verdict)
	}
	for _, r := range recs {
		if strings.Contains(r.Error, probeRetryNoteTest) {
			if r.HTTPStatus != http.StatusForbidden || r.OK || r.Outbound != gateway.ProtocolAnthropic {
				t.Fatalf("403 那次应记为未作结论的 anthropic 尝试: %+v", r)
			}
		}
	}
}

// TestProbeModelForbiddenLastRouteRecordsVerdict 覆盖 403 之后没有下一条路由的情况：
// 厂商只有 anthropic 一条路由，403 就是结论（forbidden/403），只发一次请求、只记一行账，
// 账号保持启用，协议列不写。
func TestProbeModelForbiddenLastRouteRecordsVerdict(t *testing.T) {
	f := newFixture(t)
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), "")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"MODEL_NOT_IN_PLAN"}}`))
	}))
	defer up.Close()

	acc := f.addAs(t, "a1", "anthropic", gateway.ProtocolAnthropic, up.URL, "sk-probe-403-last", []string{"claude-x"})
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelForbidden || res.HTTPStatus != http.StatusForbidden {
		t.Fatalf("结论 = %+v，期望 forbidden/403", res)
	}
	if res.Protocol != gateway.ProtocolAnthropic {
		t.Fatalf("没有下一次尝试时协议应保持厂商默认协议，实际 %q", res.Protocol)
	}

	if res.Access != store.AccessForbidden {
		t.Fatalf("结论的可达性取值 = %q，期望 %q", res.Access, store.AccessForbidden)
	}
	if paths := rec.paths(); len(paths) != 1 || paths[0] != "/v1/messages" {
		t.Fatalf("上游调用 = %v，期望只打已声明的那一条", paths)
	}

	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("403 不该停用账号，凭据本身还能用")
	}
	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "claude-x" || rows[0].Protocol != "" {
		t.Fatalf("没发现成功协议时不该写协议列: %+v", rows)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].HTTPStatus != http.StatusForbidden || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望一行 403 且 ok=0", recs)
	}
	if strings.Contains(recs[0].Error, probeRetryNoteTest) {
		t.Fatalf("唯一一次尝试就是判定行，不该带未作结论的前缀: %+v", recs[0])
	}
}
