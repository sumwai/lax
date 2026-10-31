package gateway_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// TestSandboxDialectEndToEnd 用真实沙箱跑一条语音请求：JS 拼上游报文，JS 解回音频字节。
// 这条用例把 internal/plugin 的沙箱、注册表与网关的分发串在一起。
func TestSandboxDialectEndToEnd(t *testing.T) {
	f := newFixture(t)
	audio := []byte("ID3-sandbox-audio")
	chatBody := `{"choices":[{"message":{"audio":{"data":"` +
		base64.StdEncoding.EncodeToString(audio) + `"}}}]}`
	up, seen := captureVoiceUpstream(t, "application/json", []byte(chatBody))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts",
		[]string{store.CapabilitySpeech}, nil, []string{"mp3"})

	const dialectID = "com.example.hook/voice@1"
	bindDialect(t, f, "mimo", "mimo-v2.5-tts", plugin.OpSpeechSynthesize, dialectID)

	sandbox, err := plugin.OpenSandbox(plugin.SandboxOptions{
		Manifest: plugin.Manifest{ID: "com.example.hook"},
		Root:     "testdata/hook",
		Entry:    "index.js",
		Trust:    plugin.TrustSandboxed,
	})
	if err != nil {
		t.Fatalf("打开沙箱失败: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Close() })

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.hook", plugin.HookDialect{
		IDValue:      dialectID,
		OperationVal: plugin.OpSpeechSynthesize,
		WireVal:      plugin.WireOpenAIChat,
		Sandbox:      sandbox,
	}); err != nil {
		t.Fatalf("登记沙箱方言失败: %v", err)
	}
	srv := serverWithDialect(t, f, reg)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"你好","response_format":"mp3"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != string(audio) {
		t.Fatalf("客户端拿到的音频 = %q，期望 %q", body, audio)
	}

	_, _, sent := seen()
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Audio struct {
			Format string `json:"format"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v，%s", err, sent)
	}
	if payload.Model != "mimo-v2.5-tts" || len(payload.Messages) != 1 ||
		payload.Messages[0].Role != "assistant" || payload.Messages[0].Content != "你好" ||
		payload.Audio.Format != "mp3" {
		t.Fatalf("上游报文 = %s，期望由 JS 拼出 messages + audio", sent)
	}

	recs := waitRequests(t, f.st, 1)
	if rec := recs[0]; rec.InTokens != 3 || rec.OutTokens != 1 || rec.CacheRead != 2 {
		t.Fatalf("用量 = in %d / out %d / cache %d，期望 JS 报的 3 / 1 / 2",
			rec.InTokens, rec.OutTokens, rec.CacheRead)
	}
	if !strings.Contains(recs[0].Outbound, string(gateway.ProtocolChat)) {
		t.Fatalf("请求记录的出站协议 = %q，期望 openai-chat", recs[0].Outbound)
	}
}
