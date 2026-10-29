package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
)

// 这个文件钉住「目录里声明的协议排在探活候选前面」这条排序（见 DESIGN 5.7）：
// 上游清单自己说了这个模型支持哪些协议（commandcode 的 supported_endpoints、gemini 的
// supportedGenerationMethods），探活就先用它打，少打几次错协议的请求；范围仍是厂商声明过的路由，
// 与已经发现的协议冲突时已发现的优先。

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

// TestProbeDeclaredProtocolOutsideRoutesSkipped 钉住声明不影响「只打厂商声明过的路由」：
// 这家没有 gemini 路由，清单里声明了 gemini 也不进候选，第一次请求仍是厂商默认协议的路径。
func TestProbeDeclaredProtocolOutsideRoutesSkipped(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-probe", nil, 100)
	declareProtocols(t, f, acc.Provider, "m1", gateway.ProtocolGemini)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	calls := up.mustCalls(t, 1)
	if calls[0].path != "/v1/chat/completions" {
		t.Fatalf("第一次请求 = %q，期望厂商默认协议那条（这家没有 gemini 路由）", calls[0].path)
	}
	if res.Protocol != gateway.ProtocolChat {
		t.Fatalf("结论里的协议 = %q，期望 openai-chat", res.Protocol)
	}
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
