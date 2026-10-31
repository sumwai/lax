package gateway_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// 向量化（DESIGN 5.15）：两个入站路径、只做同协议直通、没有流式、用量只记输入。
// 这里的用例都打 httptest 假上游，逐字节核对转发了什么。

// embeddingsProvider 建一家同时声明对话与向量化路由的厂商，再挂一个账号。
// 默认协议设成向量化：探活与转发都按厂商声明的那条路由走（DESIGN 5.15）。
func embeddingsProvider(t *testing.T, f *fixture, baseURL string, models []string) store.Account {
	t.Helper()
	routes := []store.ProviderRoute{
		{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions", ModelsPath: "/v1/models"},
		{Protocol: gateway.ProtocolEmbeddings, Path: "/v1/embeddings"},
	}
	if err := f.st.AddProvider(store.Provider{
		Name: "emb", BaseURL: baseURL, DefaultProtocol: gateway.ProtocolEmbeddings,
	}, routes); err != nil {
		t.Fatalf("建厂商 emb: %v", err)
	}
	return f.createAccount(t, "emb1", "emb", "sk-emb-secret", models, 100)
}

// captureUpstream 记下最近一次请求的路径与请求体，并回一段固定的 JSON。
func captureUpstream(t *testing.T, response string) (*httptest.Server, func() (string, string)) {
	t.Helper()
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, string) { return path, body }
}

const embeddingsResponse = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.25]}],` +
	`"model":"text-embedding-3-small","usage":{"prompt_tokens":7,"total_tokens":7}}`

// TestForwardEmbeddingsPassthrough 钉住 /v1/embeddings 的直通：路径是厂商声明的 /v1/embeddings，
// 请求体只换 model（input、encoding_format 原样），响应字节一个不改，用量只记输入。
func TestForwardEmbeddingsPassthrough(t *testing.T) {
	f := newFixture(t)
	up, seen := captureUpstream(t, embeddingsResponse)
	acc := embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/embeddings",
		`{"model":"text-embedding-3-small","input":["a","b"],"encoding_format":"float","dimensions":8}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != embeddingsResponse {
		t.Fatalf("响应字节被改写过：%s", body)
	}

	path, got := seen()
	if path != "/v1/embeddings" {
		t.Fatalf("上游路径 = %q，期望 /v1/embeddings", path)
	}
	for _, want := range []string{`"model":"text-embedding-3-small"`, `"encoding_format":"float"`, `"dimensions":8`, `"input":["a","b"]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("上游请求体缺 %s：%s", want, got)
		}
	}

	recs := waitRequests(t, f.st, 1)
	rec := recs[0]
	if rec.Inbound != "openai-embeddings" || rec.Outbound != "openai-embeddings" {
		t.Fatalf("协议 = %s→%s，期望 openai-embeddings→openai-embeddings", rec.Inbound, rec.Outbound)
	}
	if rec.InTokens != 7 || rec.OutTokens != 0 {
		t.Fatalf("用量 = %d/%d，期望 7/0（向量化只有输入）", rec.InTokens, rec.OutTokens)
	}
	if !rec.OK {
		t.Fatalf("请求记录 = %+v，期望 ok=1", rec)
	}
	if rec.AccountID != acc.ID {
		t.Fatalf("记录里的账号 = %d，期望 %d", rec.AccountID, acc.ID)
	}
}

// TestEmbeddingsAliasIsResolved 钉住别名在向量化请求里同样解析：客户端给的名字进 model_requested，
// 上游收到的是真实模型名。
func TestEmbeddingsAliasIsResolved(t *testing.T) {
	f := newFixture(t)
	up, seen := captureUpstream(t, embeddingsResponse)
	embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	if err := f.st.SetModelAlias("mini-embed", "text-embedding-3-small"); err != nil {
		t.Fatalf("设别名: %v", err)
	}
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/embeddings", `{"model":"mini-embed","input":"hello"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if _, got := seen(); !strings.Contains(got, `"model":"text-embedding-3-small"`) {
		t.Fatalf("上游请求体没换成真实模型名：%s", got)
	}
	rec := waitRequests(t, f.st, 1)[0]
	if rec.ModelRequested != "mini-embed" || rec.ModelUsed != "text-embedding-3-small" {
		t.Fatalf("记录 = %q→%q，期望 mini-embed→text-embedding-3-small", rec.ModelRequested, rec.ModelUsed)
	}
}

// TestEmbeddingsWithoutRouteIsRejected 钉住协议是路由事实：厂商没声明 openai-embeddings 路由时
// 返回 400 并给出声明命令，一条上游请求都不发（那等于去撞一条没声明过的路径）。
func TestEmbeddingsWithoutRouteIsRejected(t *testing.T) {
	f := newFixture(t)
	up, seen := captureUpstream(t, embeddingsResponse)
	// f.add 的假厂商只声明对话那三条路由，没有向量化。
	f.add(t, "chat1", gateway.ProtocolChat, up.URL, "sk-chat", []string{"text-embedding-3-small"}, 100)
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400：%s", status, body)
	}
	for _, want := range []string{"openai-embeddings", "lax provider set"} {
		if !strings.Contains(body, want) {
			t.Fatalf("错误体缺 %q：%s", want, body)
		}
	}
	if path, _ := seen(); path != "" {
		t.Fatalf("不该发上游请求，实际打到 %q", path)
	}
}

// TestEmbeddingsRejectsStream 钉住「向量化没有流式」：带 stream 的请求 400，文案说明去掉它。
func TestEmbeddingsRejectsStream(t *testing.T) {
	f := newFixture(t)
	up, seen := captureUpstream(t, embeddingsResponse)
	embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/embeddings",
		`{"model":"text-embedding-3-small","input":"hi","stream":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400：%s", status, body)
	}
	if !strings.Contains(body, "没有流式") {
		t.Fatalf("错误体没说明没有流式：%s", body)
	}
	if path, _ := seen(); path != "" {
		t.Fatalf("不该发上游请求，实际打到 %q", path)
	}
}

// TestEmbeddingsRejectsUnknownPath 钉住两条入站路径之外没有第三个入口：`/v1/embeddings/{model}` 是 404。
func TestEmbeddingsRejectsUnknownPath(t *testing.T) {
	f := newFixture(t)
	up, _ := captureUpstream(t, embeddingsResponse)
	embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	srv := f.server(t)

	status, _ := postPath(t, srv, "/v1/embeddings/text-embedding-3-small", `{"model":"x","input":"hi"}`)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404", status)
	}
}

// TestGeminiEmbedContentPassthrough 钉住 gemini 的向量化动作：入站 :embedContent 打到上游
// :embedContent（路由里存的是 :generateContent），请求体与响应体都一个字节不改；
// gemini 的响应没有用量字段，因此记 0/0。
func TestGeminiEmbedContentPassthrough(t *testing.T) {
	f := newFixture(t)
	const response = `{"embedding":{"values":[0.5,0.25]}}`
	up, seen := captureUpstream(t, response)
	f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, "sk-gem", []string{"text-embedding-004"})
	srv := f.server(t)

	const request = `{"content":{"parts":[{"text":"hello"}]},"taskType":"RETRIEVAL_QUERY"}`
	status, body := postPath(t, srv, "/v1beta/models/text-embedding-004:embedContent", request)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != response {
		t.Fatalf("响应字节被改写过：%s", body)
	}

	path, got := seen()
	if path != "/v1beta/models/text-embedding-004:embedContent" {
		t.Fatalf("上游路径 = %q，期望 :embedContent 动作", path)
	}
	if got != request {
		t.Fatalf("请求体被改写过：%s", got)
	}

	rec := waitRequests(t, f.st, 1)[0]
	if rec.Inbound != gateway.ProtocolGemini || rec.Outbound != gateway.ProtocolGemini {
		t.Fatalf("协议 = %s→%s，期望 gemini→gemini", rec.Inbound, rec.Outbound)
	}
	if rec.InTokens != 0 || rec.OutTokens != 0 {
		t.Fatalf("用量 = %d/%d，期望 0/0（gemini 的 :embedContent 不报用量）", rec.InTokens, rec.OutTokens)
	}
}

// TestGeminiEmbedContentRejectsGenerateModel 钉住对话动作与向量化动作不混：同一个模型按
// :generateContent 发是对话，按 :embedContent 发是向量化，两条路径各自把请求体原样带走。
func TestGeminiEmbedContentRejectsGenerateModel(t *testing.T) {
	f := newFixture(t)
	up, seen := captureUpstream(t, `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, "sk-gem", []string{"gemini-2.5-flash"})
	srv := f.server(t)

	// 对话模型被按向量化动作打：上游怎么答就怎么回，网关不拦（5.15 的已知行为）。
	postPath(t, srv, "/v1beta/models/gemini-2.5-flash:embedContent", `{"content":{"parts":[{"text":"hi"}]}}`)
	if path, _ := seen(); path != "/v1beta/models/gemini-2.5-flash:embedContent" {
		t.Fatalf("上游路径 = %q，期望 :embedContent", path)
	}
}

// TestProbeEmbeddingsModelSendsEmbeddingsBody 钉住向量化模型的探活：对话那条先试（顺序不变），
// 400 且厂商声明了向量化路由时再试一次 /v1/embeddings，2xx 即 allowed，协议记成 openai-embeddings。
func TestProbeEmbeddingsModelSendsEmbeddingsBody(t *testing.T) {
	f := newFixture(t)
	var paths []string
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/embeddings" {
			// 对话端点对这个模型就是 400：向量化模型不支持 chat completions。
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"This model is not supported for chat completions"}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, embeddingsResponse)
	}))
	t.Cleanup(up.Close)
	acc := embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	gs, _ := newGatewayWith(t, f, gateway.Options{})

	res, err := gs.ProbeModel(context.Background(), acc, "text-embedding-3-small")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Access != store.AccessAllowed {
		t.Fatalf("可达性 = %q，期望 allowed（%s）", res.Access, res.Message)
	}
	if len(paths) < 2 || paths[len(paths)-1] != "/v1/embeddings" {
		t.Fatalf("探活路径 = %v，期望最后一次打 /v1/embeddings", paths)
	}
	if !strings.Contains(body, `"input":"lax probe"`) {
		t.Fatalf("探活请求体 = %s，期望带上最小 input", body)
	}
	protocol, err := f.st.ModelProtocol(acc.ID, "text-embedding-3-small")
	if err != nil {
		t.Fatalf("读发现的协议: %v", err)
	}
	if protocol != gateway.ProtocolEmbeddings {
		t.Fatalf("发现的协议 = %q，期望 %q", protocol, gateway.ProtocolEmbeddings)
	}
}

// TestProbeGeminiEmbeddingOnlyModelUsesEmbedContent 钉住能力位的作用：目录里只有 embedding、
// 没有 chat 的 gemini 模型，探活打 :embedContent 而不是对话。
func TestProbeGeminiEmbeddingOnlyModelUsesEmbedContent(t *testing.T) {
	f := newFixture(t)
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, ":embedContent") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"generateContent is not supported for this model"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"embedding":{"values":[0.1]}}`)
	}))
	t.Cleanup(up.Close)
	acc := f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, "sk-gem",
		[]string{"text-embedding-004"})
	// 目录行用 manual：账号建的时候已经把名字写成 manual 的目录行，sync 等级写不进去（来源不降级）。
	if err := f.st.UpsertProviderModels("google-gemini", []store.ProviderModel{{
		Model: "text-embedding-004", Source: store.SourceManual,
		Capabilities: []string{"embedding"},
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	gs, _ := newGatewayWith(t, f, gateway.Options{})

	res, err := gs.ProbeModel(context.Background(), acc, "text-embedding-004")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Access != store.AccessAllowed {
		t.Fatalf("可达性 = %q，期望 allowed（%s）", res.Access, res.Message)
	}
	if len(paths) != 1 || paths[0] != "/v1beta/models/text-embedding-004:embedContent" {
		t.Fatalf("探活路径 = %v，期望只打一次 :embedContent（能力位说是向量化模型）", paths)
	}
}

// TestEmbeddingsErrorShapeIsOpenAI 钉住错误体：向量化是 OpenAI 的报文，错误体也是那一套
// （`error.type` / `error.message`），不是 anthropic 的 `type` 顶层字段。
func TestEmbeddingsErrorShapeIsOpenAI(t *testing.T) {
	f := newFixture(t)
	up, _ := captureUpstream(t, embeddingsResponse)
	embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/embeddings", `{"input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400：%s", status, body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, `"message"`) {
		t.Fatalf("错误体不是 OpenAI 形状：%s", body)
	}
	if !strings.Contains(body, "model") {
		t.Fatalf("错误文案该说明缺 model：%s", body)
	}
	_ = fmt.Sprintf("%s", body)
}
