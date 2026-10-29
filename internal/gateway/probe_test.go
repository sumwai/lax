package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
)

// TestProbeEmptyModelsUsesFallbackModel 钉住「模型列表为空的账号也能探活」：
// 刚建好、还没跑过 model sync / model probe 的账号就是这种状态，取 `Models[0]` 会直接 panic。
// 探活回退到默认模型名，备注说明这件事，账号按探活结果照常处理。
func TestProbeEmptyModelsUsesFallbackModel(t *testing.T) {
	f := newFixture(t)

	var gotPath string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicProbeOK)
	}))
	defer up.Close()

	// 模型列表为空：用例显式给 nil，与 `account add` 不给 --models 时的状态一致。
	acc := f.add(t, "empty1", gateway.ProtocolAnthropic, up.URL, "sk-test", nil, 100)
	gs := newGateway(t, f)

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("探活结果 = %+v，期望 200 且 OK", res)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("上游路径 = %q，期望 /v1/messages", gotPath)
	}
	if got := gotBody["model"]; got != "default" {
		t.Fatalf("请求里的模型名 = %v，期望回退到 default", got)
	}
	if !strings.Contains(res.Message, "还没有模型") || !strings.Contains(res.Message, "default") {
		t.Fatalf("备注 = %q，期望说明这个账号还没有模型、探活用默认模型名", res.Message)
	}
}

// TestProbeEmptyModelsKeepsNoteOnFailure 确认失败路径也留着这条说明：
// 上游的错误文本说明这次为什么失败，说明解释用的是哪个模型名，两条都要在。
func TestProbeEmptyModelsKeepsNoteOnFailure(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"套餐不含该模型"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "empty2", gateway.ProtocolAnthropic, up.URL, "sk-test", nil, 100)
	gs := newGateway(t, f)

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if res.OK || res.Status != http.StatusForbidden {
		t.Fatalf("探活结果 = %+v，期望 403 且不 OK", res)
	}
	for _, want := range []string{"套餐不含该模型", "还没有模型"} {
		if !strings.Contains(res.Message, want) {
			t.Fatalf("备注 = %q，期望包含 %q", res.Message, want)
		}
	}
}

// anthropicProbeOK 是一条合法的 anthropic 响应，探活用它。
const anthropicProbeOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
