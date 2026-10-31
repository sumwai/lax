package gateway_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"lax/internal/plugin"
)

// 向量化的 hook 方言：入站与出站都是 openai-embeddings 时，方言解出的整条响应体
// 就是客户端要读的形状，宿主直接写回。

type embeddingsDialect struct{}

func (embeddingsDialect) ID() string           { return "com.example.emb/embed@1" }
func (embeddingsDialect) Operation() string    { return plugin.OpEmbeddingsCreate }
func (embeddingsDialect) UpstreamWire() string { return plugin.WireEmbeddings }

func (embeddingsDialect) Prepare(in plugin.Input) (plugin.Plan, error) {
	return plugin.Plan{Body: in.Body}, nil
}

func (embeddingsDialect) Decode(in plugin.Input, resp plugin.Response) (plugin.Result, error) {
	// 方言把上游多包的一层拆掉，剩下的就是 openai-embeddings 的响应体。
	var wrapped struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &wrapped); err != nil {
		return plugin.Result{}, err
	}
	return plugin.Result{
		Kind:  plugin.ResultJSON,
		JSON:  wrapped.Result,
		Usage: plugin.Usage{In: 7, Total: 7, Source: "upstream"},
	}, nil
}

func TestEmbeddingsHookDialect(t *testing.T) {
	f := newFixture(t)
	up, seen := captureUpstream(t, `{"result":`+embeddingsResponse+`}`)
	embeddingsProvider(t, f, up.URL, []string{"text-embedding-3-small"})
	bindDialect(t, f, "emb", "text-embedding-3-small", plugin.OpEmbeddingsCreate, "com.example.emb/embed@1")

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.emb", embeddingsDialect{}); err != nil {
		t.Fatalf("登记方言失败: %v", err)
	}
	srv := serverWithDialect(t, f, reg)

	status, body := postPath(t, srv, "/v1/embeddings",
		`{"model":"text-embedding-3-small","input":"hello"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != embeddingsResponse {
		t.Fatalf("客户端响应 = %s，期望方言解出的那条响应体", body)
	}
	path, sent := seen()
	if path != "/v1/embeddings" {
		t.Fatalf("上游路径 = %q", path)
	}
	if sent == "" || sent == `{}` {
		t.Fatalf("上游请求体 = %q，期望宿主编码后的报文经方言原样带过去", sent)
	}
	recs := waitRequests(t, f.st, 1)
	if rec := recs[0]; rec.InTokens != 7 || !rec.OK {
		t.Fatalf("用量/状态 = in %d / ok %v，期望 7 / true", rec.InTokens, rec.OK)
	}
}
