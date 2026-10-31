package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// 语音两条协议：openai-speech（TTS）与 openai-transcriptions（ASR）。两者都只做同协议直通，
// 判定的关键在字节：TTS 只换 model、音频与 Content-Type 原样；ASR 只重写 model 那个 part、
// 响应原样。探活发的是结构错的请求，靠错误码判可达性。

// voiceProvider 建一家同时声明两条语音路由的厂商，再挂一个账号。
func voiceProvider(t *testing.T, f *fixture, baseURL string, models []string) store.Account {
	t.Helper()
	routes := []store.ProviderRoute{
		{Protocol: gateway.ProtocolSpeech, Path: "/v1/audio/speech"},
		{Protocol: gateway.ProtocolTranscriptions, Path: "/v1/audio/transcriptions"},
	}
	if err := f.st.AddProvider(store.Provider{
		Name: "voice", BaseURL: baseURL, DefaultProtocol: gateway.ProtocolSpeech,
	}, routes); err != nil {
		t.Fatalf("建厂商 voice: %v", err)
	}
	acc := f.createAccount(t, "voice1", "voice", "sk-voice-secret", models, 100)
	// 语音入站只打声明过能力的模型（见 5.16）：测试里的模型都得声明，
	// 否则会在选号之前就以 400 退回来。
	for _, model := range models {
		if _, err := f.st.AddProviderModelCapability("voice", model, store.CapabilityPatch{
			Capabilities: []string{store.CapabilitySpeech, store.CapabilityTranscription},
		}); err != nil {
			t.Fatalf("声明 %s 的能力: %v", model, err)
		}
		bindDialect(t, f, "voice", model, plugin.OpSpeechSynthesize, plugin.DialectOpenAISpeech)
		bindDialect(t, f, "voice", model, plugin.OpSpeechTranscribe, plugin.DialectOpenAITranscriptions)
	}
	return acc
}

// bindDialect 把一个模型的一个操作绑到方言上：语音请求按绑定决定上游形状。
func bindDialect(t *testing.T, f *fixture, provider, model, operation, dialect string) {
	t.Helper()
	if err := f.st.SetModelOperation(store.ModelOperation{
		Provider: provider, Model: model, Operation: operation, DialectID: dialect, Source: "manual",
	}); err != nil {
		t.Fatalf("绑定方言 %s 到 %s/%s: %v", dialect, provider, model, err)
	}
}

// captureVoiceUpstream 记下最近一次请求的路径、Content-Type 与请求体，
// 并按 contentType 回一段固定响应（默认是音频字节）。
func captureVoiceUpstream(t *testing.T, responseType string, response []byte) (*httptest.Server, func() (string, string, []byte)) {
	t.Helper()
	var path, contentType string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		path, contentType, body = r.URL.Path, r.Header.Get("Content-Type"), raw
		w.Header().Set("Content-Type", responseType)
		_, _ = w.Write(response)
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, string, []byte) { return path, contentType, body }
}

// TestForwardSpeechPassthrough 钉住 TTS 的直通：只换 model，其余字段与音频字节原样，
// 响应头里的 Content-Type 原样带出，字符数入账。
func TestForwardSpeechPassthrough(t *testing.T) {
	f := newFixture(t)
	audio := []byte{0xFF, 0xFB, 0x90, 0x64, 0x00, 0x00}
	up, seen := captureVoiceUpstream(t, "audio/mpeg", audio)
	voiceProvider(t, f, up.URL, []string{"tts-1"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"tts-1","input":"你好，世界","voice":"alloy","response_format":"mp3","speed":1.25}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if !bytes.Equal([]byte(body), audio) {
		t.Fatalf("音频字节被改写过：%v", []byte(body))
	}

	path, contentType, got := seen()
	if path != "/v1/audio/speech" {
		t.Fatalf("上游路径 = %q", path)
	}
	if contentType != "application/json" {
		t.Fatalf("上游 Content-Type = %q，期望 application/json", contentType)
	}
	for _, want := range []string{`"model":"tts-1"`, `"voice":"alloy"`, `"speed":1.25`, `"input":"你好，世界"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("上游请求体缺 %s：%s", want, got)
		}
	}

	recs := waitRequests(t, f.st, 1)
	if recs[0].TTSChars != int64(len([]rune("你好，世界"))) {
		t.Fatalf("tts_chars = %d，期望 5", recs[0].TTSChars)
	}
	if recs[0].Inbound != gateway.ProtocolSpeech || recs[0].Outbound != gateway.ProtocolSpeech {
		t.Fatalf("协议对 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
}

// TestSpeechRejectsSSEStreamFormat 钉住本版不支持 stream_format: sse：
// 把上游 chat 的音频分片重新包成 OpenAI 事件需要另一套编码，等有客户端要再做。
func TestSpeechRejectsSSEStreamFormat(t *testing.T) {
	f := newFixture(t)
	voiceProvider(t, f, "http://127.0.0.1:1", []string{"tts-1"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"tts-1","input":"hi","voice":"alloy","stream_format":"sse"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "sse") {
		t.Fatalf("文案没说清 sse 不支持：%s", body)
	}
}

// TestSpeechAliasIsResolved 钉住别名在请求方向解析一次：发给上游的是真名。
func TestSpeechAliasIsResolved(t *testing.T) {
	f := newFixture(t)
	up, seen := captureVoiceUpstream(t, "audio/mpeg", []byte("ID3"))
	voiceProvider(t, f, up.URL, []string{"tts-1"})
	if err := f.st.SetModelAlias("fast", "tts-1"); err != nil {
		t.Fatalf("设别名: %v", err)
	}
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech", `{"model":"fast","input":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	_, _, got := seen()
	if !strings.Contains(string(got), `"model":"tts-1"`) {
		t.Fatalf("上游收到的模型名不是真名：%s", got)
	}
}

// TestSpeechWithoutRouteIsRejected 钉住「既没语音路由、也不能适配」时的 400 与零上游调用：
// 厂商只声明了 anthropic 路由，有效协议不是 openai-chat，语音请求进不了选号。
func TestSpeechWithoutRouteIsRejected(t *testing.T) {
	f := newFixture(t)
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	if err := f.st.AddProvider(store.Provider{Name: "claudeonly", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolAnthropic},
		[]store.ProviderRoute{{Protocol: gateway.ProtocolAnthropic, Path: "/v1/messages"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "claude1", "claudeonly", "sk-claude", []string{"tts-1"}, 100)
	// 声明能力但厂商没声明语音路由：这一用例验的是「缺路由」而不是「缺能力」，
	// 绑一条 chat 方言，让报文因为路由缺失而卡住。
	if _, err := f.st.AddProviderModelCapability("claudeonly", "tts-1", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech},
	}); err != nil {
		t.Fatalf("声明能力: %v", err)
	}
	bindDialect(t, f, "claudeonly", "tts-1", plugin.OpSpeechSynthesize, plugin.DialectMiMoChatSpeech)
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech", `{"model":"tts-1","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "openai-chat") {
		t.Fatalf("文案没说清只能经 chat 适配：%s", body)
	}
	if calls != 0 {
		t.Fatalf("上游收到 %d 次调用，期望 0", calls)
	}
}

// TestTranscriptionsRewritesOnlyModelField 钉住 ASR 的 multipart 重写：只换 model 那个 part，
// 音频字节与其它字段原样，boundary 重生成。
func TestTranscriptionsRewritesOnlyModelField(t *testing.T) {
	f := newFixture(t)
	up, seen := captureVoiceUpstream(t, "application/json", []byte(`{"text":"你好","usage":{"duration":2.6}}`))
	voiceProvider(t, f, up.URL, []string{"whisper-1"})
	srv := f.server(t)

	audio := []byte("RIFF....WAVEfmt ")
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "whisper-1")
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
	if string(body) != `{"text":"你好","usage":{"duration":2.6}}` {
		t.Fatalf("响应被改写过：%s", body)
	}

	path, contentType, got := seen()
	if path != "/v1/audio/transcriptions" {
		t.Fatalf("上游路径 = %q", path)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("上游 Content-Type = %q", contentType)
	}
	if params["boundary"] == w.Boundary() {
		t.Fatalf("boundary 没有重生成：%q", params["boundary"])
	}

	reader := multipart.NewReader(bytes.NewReader(got), params["boundary"])
	fields := map[string]string{}
	var fileBytes []byte
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析上游 multipart: %v", err)
		}
		data, _ := io.ReadAll(p)
		if p.FormName() == "file" {
			fileBytes = data
			continue
		}
		fields[p.FormName()] = string(data)
	}
	if fields["model"] != "whisper-1" || fields["language"] != "zh" {
		t.Fatalf("字段 = %v", fields)
	}
	if !bytes.Equal(fileBytes, audio) {
		t.Fatalf("音频字节变了：%q", fileBytes)
	}

	recs := waitRequests(t, f.st, 1)
	if recs[0].AudioSeconds != 3 {
		t.Fatalf("audio_seconds = %d，期望 3（2.6 四舍五入）", recs[0].AudioSeconds)
	}
	if recs[0].Inbound != gateway.ProtocolTranscriptions {
		t.Fatalf("入站协议 = %s", recs[0].Inbound)
	}
}

// TestTranscriptionsRejectsNonMultipart 钉住 Content-Type 校验：JSON 请求体在这里没有落点。
func TestTranscriptionsRejectsNonMultipart(t *testing.T) {
	f := newFixture(t)
	voiceProvider(t, f, "http://127.0.0.1:1", []string{"whisper-1"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/transcriptions", `{"model":"whisper-1"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "multipart/form-data") {
		t.Fatalf("文案没说清要 multipart：%s", body)
	}
}

// TestTranscriptionsMissingFile 钉住「只给 model 不给 file」在本地就拦下：
// 这种请求上游只会回 400，本机能说清缺什么。
func TestTranscriptionsMissingFile(t *testing.T) {
	f := newFixture(t)
	voiceProvider(t, f, "http://127.0.0.1:1", []string{"whisper-1"})
	srv := f.server(t)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "whisper-1")
	_ = w.Close()

	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "file") {
		t.Fatalf("文案没提 file：%s", body)
	}
}

// TestSpeechErrorShapeIsOpenAI 钉住缺 model 时的错误体是 openai 形状。
func TestSpeechErrorShapeIsOpenAI(t *testing.T) {
	f := newFixture(t)
	voiceProvider(t, f, "http://127.0.0.1:1", []string{"tts-1"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech", `{"input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "invalid_request_error") {
		t.Fatalf("错误体不是 openai 形状：%s", body)
	}
}

// TestProbeSpeechWrongShape 钉住 TTS 探活：发的是结构错的请求（input 为 null），
// 400 判 allowed 并在备注里写「凭据未验证」，404 判 missing，401 停用账号。
func TestProbeSpeechWrongShape(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		access   string
		disabled bool
		note     string
	}{
		{"400 判可达", http.StatusBadRequest, store.AccessAllowed, false, "凭据未验证"},
		{"422 判可达", http.StatusUnprocessableEntity, store.AccessAllowed, false, "凭据未验证"},
		{"404 判不存在", http.StatusNotFound, store.AccessMissing, false, ""},
		{"401 停用账号", http.StatusUnauthorized, "", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			var gotBody string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotBody = string(raw)
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, `{"error":{"message":"probe"}}`)
			}))
			t.Cleanup(up.Close)
			acc := voiceProvider(t, f, up.URL, []string{"tts-1"})

			gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Logf: func(string, ...any) {}})
			if err != nil {
				t.Fatalf("构造网关: %v", err)
			}
			res, err := gs.ProbeModelProtocol(context.Background(), acc, "tts-1", gateway.ProtocolSpeech)
			if err != nil {
				t.Fatalf("探活: %v", err)
			}
			if res.Access != c.access {
				t.Fatalf("access = %q（%s），期望 %q", res.Access, res.Message, c.access)
			}
			if !strings.Contains(gotBody, `"input":null`) {
				t.Fatalf("探活请求体不是应错结构：%s", gotBody)
			}
			if c.note != "" && !strings.Contains(res.Message, c.note) {
				t.Fatalf("备注 = %q，期望包含 %q", res.Message, c.note)
			}
			if c.disabled {
				got, err := f.st.GetAccount("voice1")
				if err != nil {
					t.Fatalf("读账号: %v", err)
				}
				if got.Enabled {
					t.Fatalf("401 之后账号仍然是启用状态")
				}
			}
		})
	}
}

// TestProbeTranscriptionsMissingFile 钉住 ASR 探活：multipart 里只有 model、没有 file，
// 上游的 400 判可达，且 Content-Type 是 multipart 而不是 JSON。
func TestProbeTranscriptionsMissingFile(t *testing.T) {
	f := newFixture(t)
	var gotType, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotType, gotBody = r.Header.Get("Content-Type"), string(raw)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"file is required"}}`)
	}))
	t.Cleanup(up.Close)
	acc := voiceProvider(t, f, up.URL, []string{"whisper-1"})

	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	res, err := gs.ProbeModelProtocol(context.Background(), acc, "whisper-1", gateway.ProtocolTranscriptions)
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Access != store.AccessAllowed {
		t.Fatalf("access = %q（%s），期望 allowed", res.Access, res.Message)
	}
	if !strings.HasPrefix(gotType, "multipart/form-data") {
		t.Fatalf("探活 Content-Type = %q，期望 multipart/form-data", gotType)
	}
	if !strings.Contains(gotBody, "whisper-1") {
		t.Fatalf("探活报文里没有模型名：%s", gotBody)
	}
}

// chatAdapterProvider 建一家只有 chat 路由的厂商，并给模型声明语音能力：
// 模拟 MiMo 那种「语音做成 chat 方言」的上游。
func chatAdapterProvider(t *testing.T, f *fixture, baseURL, model string,
	capabilities, voices, formats []string) store.Account {
	t.Helper()
	if err := f.st.AddProvider(store.Provider{
		Name: "mimo", BaseURL: baseURL, DefaultProtocol: gateway.ProtocolChat,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商 mimo: %v", err)
	}
	acc := f.createAccount(t, "mimo1", "mimo", "sk-mimo", []string{model}, 100)
	if _, err := f.st.AddProviderModelCapability("mimo", model, store.CapabilityPatch{
		Capabilities: capabilities, Voices: voices, AudioFormats: formats,
	}); err != nil {
		t.Fatalf("声明能力: %v", err)
	}
	for _, capability := range capabilities {
		switch capability {
		case store.CapabilitySpeech:
			bindDialect(t, f, "mimo", model, plugin.OpSpeechSynthesize, plugin.DialectMiMoChatSpeech)
		case store.CapabilityTranscription:
			bindDialect(t, f, "mimo", model, plugin.OpSpeechTranscribe, plugin.DialectMiMoChatASR)
		}
	}
	return acc
}

// TestSpeechChatAdapter 钉住 TTS 的 chat 适配：入站是 OpenAI audio 形状，
// 出站是聊天的 messages + audio 对象，响应里的 base64 音频解成字节给客户端。
func TestSpeechChatAdapter(t *testing.T) {
	f := newFixture(t)
	audio := []byte("ID3-fake-audio")
	chatBody := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"","audio":{"data":"` +
		base64.StdEncoding.EncodeToString(audio) + `"}},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":125,"completion_tokens":7,"total_tokens":132,` +
		`"prompt_tokens_details":{"cached_tokens":121}}}`
	up, seen := captureVoiceUpstream(t, "application/json", []byte(chatBody))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts",
		[]string{store.CapabilitySpeech}, []string{"Mia", "Chloe"}, []string{"mp3", "wav", "pcm16"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"你好世界","voice":"Mia","response_format":"mp3"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != string(audio) {
		t.Fatalf("音频字节 = %q，期望 %q", body, audio)
	}

	path, contentType, got := seen()
	if path != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q", path)
	}
	if contentType != "application/json" {
		t.Fatalf("上游 Content-Type = %q", contentType)
	}
	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Audio struct {
			Format string `json:"format"`
			Voice  string `json:"voice"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v，%s", err, got)
	}
	if sent.Model != "mimo-v2.5-tts" {
		t.Fatalf("上游模型名 = %q", sent.Model)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "assistant" || sent.Messages[0].Content != "你好世界" {
		t.Fatalf("messages = %+v，期望只有一条 assistant 带正文", sent.Messages)
	}
	if sent.Audio.Format != "mp3" || sent.Audio.Voice != "Mia" {
		t.Fatalf("audio = %+v", sent.Audio)
	}

	recs := waitRequests(t, f.st, 1)
	rec := recs[0]
	if rec.Inbound != gateway.ProtocolSpeech || rec.Outbound != gateway.ProtocolChat {
		t.Fatalf("协议对 = %s→%s，期望 openai-speech→openai-chat", rec.Inbound, rec.Outbound)
	}
	if rec.TTSChars != int64(len([]rune("你好世界"))) {
		t.Fatalf("tts_chars = %d，期望 4", rec.TTSChars)
	}
	if rec.InTokens != 125 || rec.OutTokens != 7 || rec.CacheRead != 121 {
		t.Fatalf("用量 = in %d / out %d / cache %d，期望 125 / 7 / 121", rec.InTokens, rec.OutTokens, rec.CacheRead)
	}
}

// TestSpeechChatAdapterPCM 钉住格式名两侧不一的情况：入站写 OpenAI 的 pcm，
// 上游与能力声明写 pcm16，本地校验要按适配后的名字放行。
func TestSpeechChatAdapterPCM(t *testing.T) {
	f := newFixture(t)
	chatBody := `{"choices":[{"message":{"audio":{"data":"` +
		base64.StdEncoding.EncodeToString([]byte("pcm-bytes")) + `"}}}]}`
	up, seen := captureVoiceUpstream(t, "application/json", []byte(chatBody))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts",
		[]string{store.CapabilitySpeech}, nil, []string{"mp3", "wav", "pcm16"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"hi","response_format":"pcm"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	_, _, got := seen()
	var sent struct {
		Audio struct {
			Format string `json:"format"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("解析上游请求体: %v", err)
	}
	if sent.Audio.Format != "pcm16" {
		t.Fatalf("audio.format = %q，期望 pcm16", sent.Audio.Format)
	}
}

// TestSpeechChatAdapterInstructions 钉住 instructions 映射到 user 消息（风格指令），
// 正文仍放在 assistant 消息里——MiMo 的示例就是这个形状。
func TestSpeechChatAdapterInstructions(t *testing.T) {
	f := newFixture(t)
	chatBody := `{"choices":[{"message":{"audio":{"data":"` +
		base64.StdEncoding.EncodeToString([]byte("wav-bytes")) + `"}}}]}`
	up, seen := captureVoiceUpstream(t, "application/json", []byte(chatBody))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts",
		[]string{store.CapabilitySpeech}, nil, nil)
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"Hello","instructions":"Speak slowly","response_format":"wav"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	_, _, got := seen()
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Audio struct {
			Format string `json:"format"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("解析上游请求体: %v", err)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "user" || sent.Messages[0].Content != "Speak slowly" {
		t.Fatalf("messages = %+v，期望 user 风格指令 + assistant 正文", sent.Messages)
	}
	if sent.Messages[1].Role != "assistant" || sent.Messages[1].Content != "Hello" {
		t.Fatalf("assistant 消息 = %+v", sent.Messages[1])
	}
	if sent.Audio.Format != "wav" {
		t.Fatalf("audio.format = %q，期望 wav", sent.Audio.Format)
	}
}

// TestSpeechWithoutCapabilityRejected 钉住第 1 条要求：没有声明 speech 能力的模型，
// 语音请求在本地就被拒，一条上游调用都不发。
func TestSpeechWithoutCapabilityRejected(t *testing.T) {
	f := newFixture(t)
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	if err := f.st.AddProvider(store.Provider{Name: "plain", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolChat},
		[]store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "plain1", "plain", "sk-plain", []string{"deepseek-v4.1-flash"}, 100)
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"deepseek-v4.1-flash","input":"你好","voice":"alloy"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "没有声明 speech 能力") {
		t.Fatalf("文案没说清缺能力：%s", body)
	}
	if calls != 0 {
		t.Fatalf("上游收到 %d 次调用，期望 0", calls)
	}
}

// TestSpeechVoiceRejected 钉住音色白名单：声明了音色之后，未声明的取值在本地被拒。
func TestSpeechVoiceRejected(t *testing.T) {
	f := newFixture(t)
	up, _ := captureVoiceUpstream(t, "application/json", []byte(`{}`))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-tts",
		[]string{store.CapabilitySpeech}, []string{"Mia", "Chloe"}, nil)
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"hi","voice":"alloy"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "Mia") || !strings.Contains(body, "Chloe") {
		t.Fatalf("文案没列出可用音色：%s", body)
	}
}

// TestTranscriptionsChatAdapter 钉住 ASR 的 chat 适配：multipart 音频编成
// input_audio 的 data URL，响应里的正文包成 {"text":…}，秒数与 token 入账。
func TestTranscriptionsChatAdapter(t *testing.T) {
	f := newFixture(t)
	chatBody := `{"choices":[{"message":{"content":"你好"}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"seconds":3}}`
	up, seen := captureVoiceUpstream(t, "application/json", []byte(chatBody))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-asr",
		[]string{store.CapabilityTranscription}, nil, []string{"wav", "mp3"})
	srv := f.server(t)

	wav := append([]byte("RIFF$\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0}, 16)...)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "mimo-v2.5-asr")
	_ = w.WriteField("language", "zh")
	part, _ := w.CreateFormFile("file", "a.wav")
	_, _ = part.Write(wav)
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
	if string(body) != `{"text":"你好"}` {
		t.Fatalf("响应 = %s，期望 {\"text\":\"你好\"}", body)
	}

	_, _, got := seen()
	var sent struct {
		Messages []struct {
			Content []struct {
				Type       string `json:"type"`
				InputAudio struct {
					Data string `json:"data"`
				} `json:"input_audio"`
			} `json:"content"`
		} `json:"messages"`
		ASROptions struct {
			Language string `json:"language"`
		} `json:"asr_options"`
	}
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("解析上游请求体: %v", err)
	}
	if len(sent.Messages) != 1 || len(sent.Messages[0].Content) != 1 {
		t.Fatalf("上游 messages = %+v，期望只有一条 input_audio", sent.Messages)
	}
	part0 := sent.Messages[0].Content[0]
	if part0.Type != "input_audio" || !strings.HasPrefix(part0.InputAudio.Data, "data:audio/wav;base64,") {
		t.Fatalf("input_audio = %+v", part0)
	}
	if sent.ASROptions.Language != "zh" {
		t.Fatalf("asr_options.language = %q", sent.ASROptions.Language)
	}

	recs := waitRequests(t, f.st, 1)
	rec := recs[0]
	if rec.Inbound != gateway.ProtocolTranscriptions || rec.Outbound != gateway.ProtocolChat {
		t.Fatalf("协议对 = %s→%s", rec.Inbound, rec.Outbound)
	}
	if rec.AudioSeconds != 3 {
		t.Fatalf("audio_seconds = %d，期望 3", rec.AudioSeconds)
	}
	if rec.InTokens != 10 || rec.OutTokens != 2 {
		t.Fatalf("用量 = in %d / out %d", rec.InTokens, rec.OutTokens)
	}
}

// TestTranscriptionsRejectsOGG 钉住本版不转码：ogg/opus 在入站就被拒，文案说清怎么绕开。
func TestTranscriptionsRejectsOGG(t *testing.T) {
	f := newFixture(t)
	up, _ := captureVoiceUpstream(t, "application/json", []byte(`{}`))
	chatAdapterProvider(t, f, up.URL, "mimo-v2.5-asr",
		[]string{store.CapabilityTranscription}, nil, []string{"wav", "mp3"})
	srv := f.server(t)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "mimo-v2.5-asr")
	part, _ := w.CreateFormFile("file", "voice.ogg")
	_, _ = part.Write([]byte("OggS\x00\x02"))
	_ = w.Close()

	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "转码") {
		t.Fatalf("文案没提转码：%s", body)
	}
}
