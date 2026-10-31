package gateway_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// 这个文件钉住「目录里声明的协议排在探活候选前面」这条排序：
// 上游清单自己说了这个模型支持哪些协议（commandcode 的 supported_endpoints、gemini 的
// supportedGenerationMethods），探活就先用它打，少打几次错协议的请求；声明不在厂商声明的路由之内
// 也打（模型级声明本身就是这条路径存在的证据），与已经发现的协议冲突时已发现的优先。

// declareProtocols 把一个模型声明的协议写进目录，模拟一次 model sync 的结果。
func declareProtocols(t *testing.T, f *fixture, provider, model string, protocols ...string) {
	t.Helper()
	if err := f.st.UpsertProviderModels(provider, []store.ProviderModel{{
		Model:     model,
		Source:    store.SourceSync,
		Protocols: protocols,
	}}); err != nil {
		t.Fatalf("写目录里声明的协议: %v", err)
	}
}

// TestProbeDeclaredProtocolGoesFirst 钉住目录声明的协议排在厂商默认协议之前：
// 厂商默认协议是 openai-chat，模型声明只支持 anthropic，第一次请求就必须打 /v1/messages，
// 而且一次就成——不再先打 chat 换回一个 403/404 再换协议。
func TestProbeDeclaredProtocolGoesFirst(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/messages":         {status: http.StatusOK, body: probeAnthropicOK},
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
		"POST /v1/responses":        {status: http.StatusOK, body: chatOK},
	})

	// 账号不带手填模型：手填行在目录里是 manual，比同步写进来的 sync 更强，会把声明挡掉。
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-probe", nil, 100)
	declareProtocols(t, f, acc.Provider, "claude-x", gateway.ProtocolAnthropic)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	calls := up.mustCalls(t, 1)
	if calls[0].path != "/v1/messages" {
		t.Fatalf("第一次请求 = %q，期望声明的 /v1/messages（厂商默认协议是 openai-chat）", calls[0].path)
	}
	if res.Access != store.AccessAllowed || res.Protocol != gateway.ProtocolAnthropic {
		t.Fatalf("结论 = %+v，期望 allowed/anthropic", res)
	}
}

// TestProbeModelDeclaredProtocolOutsideRoutes 钉住声明不在厂商路由之内也打：这家厂商只有 chat 路由，
// 模型目录里声明了 systemone，探活的第一次调用就必须是 /v1/systemone（systemone 的路径固定，
// 地址由 base_url 后缀给出），2xx 就是结论——不再先打厂商默认协议白花一次调用。
func TestProbeModelDeclaredProtocolOutsideRoutes(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/systemone":        {status: http.StatusOK, body: systemOneOKBody},
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-probe", nil, 100)
	declareProtocols(t, f, acc.Provider, "m1", gateway.ProtocolSystemOne)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	calls := up.mustCalls(t, 1)
	if calls[0].path != "/v1/systemone" {
		t.Fatalf("第一次请求 = %q，期望声明的 /v1/systemone（这家没有 systemone 路由）", calls[0].path)
	}
	if res.Access != store.AccessAllowed || res.Protocol != gateway.ProtocolSystemOne {
		t.Fatalf("结论 = %+v，期望 allowed/systemone", res)
	}
}

// TestProbeModelDeclaredProtocolOutsideRoutesFailsKeepsDefault 钉住这些位置上的失败不构成结论：
// 声明的 systemone 回 404 之后接着打厂商默认协议，结论取厂商默认协议那一次。
func TestProbeModelDeclaredProtocolOutsideRoutesFailsKeepsDefault(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-probe", nil, 100)
	declareProtocols(t, f, acc.Provider, "m1", gateway.ProtocolSystemOne)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	calls := up.mustCalls(t, 2)
	if calls[0].path != "/v1/systemone" || calls[1].path != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %q, %q，期望先试声明的 systemone 再回厂商默认协议",
			calls[0].path, calls[1].path)
	}
	if res.Access != store.AccessAllowed || res.Protocol != gateway.ProtocolChat {
		t.Fatalf("结论 = %+v，期望 allowed/openai-chat", res)
	}
}

// TestProbeModelExplicitProtocolOnly 钉住 `model probe --protocol`：只按指定的那条协议打，
// 不读已发现协议、不读目录声明、不取厂商默认协议，所以厂商声明过的 anthropic 与 chat 两条路径零调用。
func TestProbeModelExplicitProtocolOnly(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/systemone":        {status: http.StatusOK, body: systemOneOKBody},
		"POST /v1/messages":         {status: http.StatusOK, body: probeAnthropicOK},
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})

	// 这家厂商（deepseek 的种子行）只声明了 anthropic 与 chat 两条路由，默认协议是 chat：
	// 断言它们零调用，只打指定的 systemone（systemone 不在厂商路由里，地址靠 base_url 后缀）。
	acc := f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.srv.URL, "sk-probe", nil)
	gs := newGateway(t, f)

	res, err := gs.ProbeModelProtocol(context.Background(), acc, "m1", gateway.ProtocolSystemOne)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	calls := up.mustCalls(t, 1)
	if calls[0].path != "/v1/systemone" {
		t.Fatalf("唯一一次请求 = %q，期望只打指定的 /v1/systemone", calls[0].path)
	}
	if res.Access != store.AccessAllowed || res.Protocol != gateway.ProtocolSystemOne {
		t.Fatalf("结论 = %+v，期望 allowed/systemone", res)
	}

	// 协议发现结果照旧写进模型行（账号级覆盖）。
	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != gateway.ProtocolSystemOne {
		t.Fatalf("模型行 = %+v，期望 protocol=systemone", rows)
	}
}

// TestProbeModelExplicitProtocolRejectsUnknown 钉住 `--protocol` 的校验：不是线协议时报错、
// 不发上游请求，带出的结论标 Undecided（调用方不拿它写可达性）。
func TestProbeModelExplicitProtocolRejectsUnknown(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-probe", []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModelProtocol(context.Background(), acc, "m1", "cohere")
	if err == nil {
		t.Fatal("非法协议应报错，不能静默落进某条分支")
	}
	for _, want := range []string{"cohere", gateway.ProtocolSystemOne} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误 = %v，期望包含 %q（列出可用的线协议）", err, want)
		}
	}
	if !res.Undecided || res.Access != "" {
		t.Fatalf("结论 = %+v，期望 Access 留空且 Undecided 为真", res)
	}
	up.mustCalls(t, 0)
}

// TestProbeDiscoveredProtocolBeatsDeclared 钉住冲突时的优先级：已发现的协议（account_models.protocol）
// 排在目录声明的协议前面——发现结果说的是「这个账号上真的走通了这条」，比上游清单的声明更强。
func TestProbeDiscoveredProtocolBeatsDeclared(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/messages":  {status: http.StatusOK, body: probeAnthropicOK},
		"POST /v1/responses": {status: http.StatusOK, body: chatOK},
	})

	// 账号不带手填模型：手填行在目录里是 manual，比同步写进来的 sync 更强，会把声明挡掉。
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-probe", nil, 100)
	declareProtocols(t, f, acc.Provider, "claude-x", gateway.ProtocolAnthropic)
	if err := f.st.SetAccountModelProtocol(acc.ID, "claude-x", gateway.ProtocolResponses); err != nil {
		t.Fatalf("写已发现的协议: %v", err)
	}
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	calls := up.mustCalls(t, 1)
	if calls[0].path != "/v1/responses" {
		t.Fatalf("第一次请求 = %q，期望已发现的 /v1/responses（压过目录声明的 anthropic）", calls[0].path)
	}
	if res.Protocol != gateway.ProtocolResponses {
		t.Fatalf("结论里的协议 = %q，期望 openai-responses", res.Protocol)
	}
}
