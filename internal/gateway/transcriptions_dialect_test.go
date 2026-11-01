package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// transcriptionsHookDialect 钉住「入站与出站同为 openai-transcriptions、
// 但上游请求不是 OpenAI multipart」的形状。它不包含任何厂商知识：网关必须先
// 让方言编请求，再考虑 multipart 直通。
type transcriptionsHookDialect struct{}

func (transcriptionsHookDialect) ID() string        { return "com.example.transcribe/dialect@1" }
func (transcriptionsHookDialect) Operation() string { return plugin.OpSpeechTranscribe }
func (transcriptionsHookDialect) UpstreamWire() string {
	return plugin.WireTranscriptions
}

func (transcriptionsHookDialect) Prepare(in plugin.Input) (plugin.Plan, error) {
	return plugin.Plan{
		JSON: map[string]any{
			"custom_audio": in.AudioContainer,
			"language":     in.Language,
			"model":        in.Model,
		},
		Headers: map[string]string{"x-dialect": "transcribe"},
	}, nil
}

func (transcriptionsHookDialect) Decode(plugin.Input, plugin.Response) (plugin.Result, error) {
	return plugin.Result{Kind: plugin.ResultJSON, JSON: []byte(`{"text":"decoded by dialect"}`)}, nil
}

// TestTranscriptionsHookDialectOwnWire 钉住转写方言优先于 multipart 直通。
// 豆包 ASR 是这条分支的真实触发者，但网关测试只用假方言，不加载任何厂商插件。
func TestTranscriptionsHookDialectOwnWire(t *testing.T) {
	f := newFixture(t)
	up, seen := captureVoiceUpstream(t, "application/json", []byte(`{"ok":true}`))
	if err := f.st.AddProvider(store.Provider{
		Name: "voiceup", BaseURL: up.URL, AuthHeader: "X-Api-Key", AuthScheme: "raw",
		DefaultProtocol: gateway.ProtocolTranscriptions,
	}, []store.ProviderRoute{{
		Protocol: gateway.ProtocolTranscriptions,
		Path:     "/v1/custom/transcribe",
	}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "voice1", "voiceup", "sk-secret", []string{"asr-model"}, 100)
	if _, err := f.st.AddProviderModelCapability("voiceup", "asr-model", store.CapabilityPatch{
		Capabilities: []string{store.CapabilityTranscription},
		AudioFormats: []string{"wav", "mp3"},
	}); err != nil {
		t.Fatalf("声明能力: %v", err)
	}
	bindDialect(t, f, "voiceup", "asr-model", plugin.OpSpeechTranscribe, transcriptionsHookDialect{}.ID())

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.transcribe", transcriptionsHookDialect{}); err != nil {
		t.Fatalf("登记方言: %v", err)
	}
	srv := serverWithDialect(t, f, reg)

	audio := []byte("RIFF....WAVEfmt ")
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "asr-model")
	_ = w.WriteField("language", "zh")
	part, _ := w.CreateFormFile("file", "a.wav")
	_, _ = part.Write(audio)
	_ = w.Close()

	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", resp.StatusCode, body)
	}
	if string(body) != `{"text":"decoded by dialect"}` {
		t.Fatalf("响应 = %s，期望方言解出的 JSON", body)
	}

	path, contentType, sent := seen()
	if path != "/v1/custom/transcribe" {
		t.Fatalf("上游路径 = %q", path)
	}
	if contentType != "application/json" {
		t.Fatalf("上游 Content-Type = %q，期望 JSON；multipart 直通抢在方言之前执行了", contentType)
	}
	if strings.Contains(string(sent), "multipart") {
		t.Fatalf("上游收到 multipart：%s", sent)
	}
	var payload map[string]any
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v，%s", err, sent)
	}
	if payload["custom_audio"] != "wav" || payload["language"] != "zh" || payload["model"] != "asr-model" {
		t.Fatalf("上游请求体 = %v，期望由方言编出", payload)
	}

	recs := waitRequests(t, f.st, 1)
	if rec := recs[0]; rec.Inbound != gateway.ProtocolTranscriptions ||
		rec.Outbound != gateway.ProtocolTranscriptions || rec.ModelUsed != "asr-model" {
		t.Fatalf("请求记录 = %+v", rec)
	}
}
