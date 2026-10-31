package gateway_test

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
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
	return f.createAccount(t, "voice1", "voice", "sk-voice-secret", models, 100)
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

// TestSpeechStreamFormatPassthrough 钉住 stream_format: sse 也走直通：上游回 SSE 帧时
// Content-Type 与字节都原样，网关不按对话协议的帧格式去解它。
func TestSpeechStreamFormatPassthrough(t *testing.T) {
	f := newFixture(t)
	sse := []byte("data: {\"type\":\"speech.audio.delta\",\"audio\":\"AAAA\"}\n\n")
	up, seen := captureVoiceUpstream(t, "text/event-stream", sse)
	voiceProvider(t, f, up.URL, []string{"tts-1"})
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech",
		`{"model":"tts-1","input":"hi","voice":"alloy","stream_format":"sse"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}
	if body != string(sse) {
		t.Fatalf("SSE 字节被改写过：%q", body)
	}
	_, contentType, _ := seen()
	if contentType != "application/json" {
		t.Fatalf("上游 Content-Type = %q", contentType)
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

// TestSpeechWithoutRouteIsRejected 钉住缺路由时的 400 与零上游调用：
// 厂商没有声明 openai-speech 路由时，请求不进选号。
func TestSpeechWithoutRouteIsRejected(t *testing.T) {
	f := newFixture(t)
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	if err := f.st.AddProvider(store.Provider{Name: "chatonly", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolChat},
		[]store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "chat1", "chatonly", "sk-chat", []string{"tts-1"}, 100)
	srv := f.server(t)

	status, body := postPath(t, srv, "/v1/audio/speech", `{"model":"tts-1","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "openai-speech") {
		t.Fatalf("文案没给出下一步：%s", body)
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
