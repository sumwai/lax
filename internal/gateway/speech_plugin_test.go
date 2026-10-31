package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// 方言驱动的语音路径：请求体由方言编，响应由方言解，网关不认任何一家的字段名。
// 这里用一条测试方言（chat 线协议），证明换成插件方言之后链路仍然完整。

const testDialectID = "com.example.test/voice@1"

// testDialect 把正文放进一个自定义字段，响应解成一行文本。
type testDialect struct{}

func (testDialect) ID() string           { return testDialectID }
func (testDialect) Operation() string    { return plugin.OpSpeechSynthesize }
func (testDialect) UpstreamWire() string { return plugin.WireOpenAIChat }

func (testDialect) Prepare(in plugin.Input) (plugin.Plan, error) {
	return plugin.Plan{JSON: map[string]any{
		"model":  in.Model,
		"text":   in.Text,
		"voice":  in.Voice,
		"custom": true,
	}}, nil
}

func (testDialect) Decode(in plugin.Input, resp plugin.Response) (plugin.Result, error) {
	var body struct {
		Spoken string `json:"spoken"`
		Usage  struct {
			In  int64 `json:"in"`
			Out int64 `json:"out"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return plugin.Result{}, err
	}
	return plugin.Result{
		Kind: plugin.ResultText,
		Text: "spoken:" + body.Spoken,
		Usage: plugin.Usage{
			In: body.Usage.In, Out: body.Usage.Out, Source: "upstream",
		},
	}, nil
}

func serverWithDialect(t *testing.T, f *fixture, reg *plugin.Registry) *httptest.Server {
	t.Helper()
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Dialects: reg})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestSpeechDialectDrivesRequestAndResponse(t *testing.T) {
	f := newFixture(t)
	up, seen := captureVoiceUpstream(t, "application/json", []byte(`{"spoken":"你好","usage":{"in":11,"out":3}}`))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts", []string{store.CapabilitySpeech}, []string{"Mia"}, []string{"mp3"})
	bindDialect(t, f, "mimo", "mimo-v2.5-tts", plugin.OpSpeechSynthesize, testDialectID)

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.test", testDialect{}); err != nil {
		t.Fatalf("登记测试方言: %v", err)
	}
	srv := serverWithDialect(t, f, reg)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"你好","voice":"Mia","response_format":"mp3"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != "spoken:你好" {
		t.Fatalf("客户端拿到 %q，期望方言解出的文本", body)
	}

	path, _, sent := seen()
	if path != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q", path)
	}
	var payload map[string]any
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v，%s", err, sent)
	}
	if payload["custom"] != true || payload["text"] != "你好" || payload["voice"] != "Mia" {
		t.Fatalf("上游请求体 = %v，期望由方言编出", payload)
	}

	recs := waitRequests(t, f.st, 1)
	if rec := recs[0]; rec.InTokens != 11 || rec.OutTokens != 3 {
		t.Fatalf("用量 = in %d / out %d，期望方言报的 11 / 3", rec.InTokens, rec.OutTokens)
	}
}

func TestSpeechDialectNotRegistered(t *testing.T) {
	f := newFixture(t)
	up, _ := captureVoiceUpstream(t, "application/json", []byte(`{}`))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts", []string{store.CapabilitySpeech}, nil, []string{"mp3"})
	bindDialect(t, f, "mimo", "mimo-v2.5-tts", plugin.OpSpeechSynthesize, "com.example.gone/voice@1")

	srv := f.server(t)
	status, body := postPath(t, srv, "/v1/audio/speech", `{"model":"mimo-v2.5-tts","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "没有注册") {
		t.Fatalf("文案没说清方言缺失：%s", body)
	}
}

func TestSpeechWithoutDialectBinding(t *testing.T) {
	f := newFixture(t)
	up, _ := captureVoiceUpstream(t, "audio/mpeg", []byte("audio"))
	// 不写绑定：语音请求要报出「没有绑定方言」并给出下一步命令，所以这里不走 chatAdapterProvider
	// （它顺手会绑一条内置方言）。
	if err := f.st.AddProvider(store.Provider{
		Name: "mimo", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolChat,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "mimo1", "mimo", "sk-mimo", []string{"tts-1"}, 100)
	if _, err := f.st.AddProviderModelCapability("mimo", "tts-1", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech},
	}); err != nil {
		t.Fatalf("声明能力: %v", err)
	}
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech", `{"model":"tts-1","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "--dialect") {
		t.Fatalf("文案没给绑定方言的下一步：%s", body)
	}
}
