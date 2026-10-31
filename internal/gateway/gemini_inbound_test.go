package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
	"lax/internal/translate"
)

// gemini 的入站路径：模型名在路径里，两条路径后缀各自表示非流式与流式。
const (
	geminiInboundPath   = "/v1beta/models/gemini-2.5-flash:generateContent"
	geminiInboundStream = "/v1beta/models/gemini-2.5-flash:streamGenerateContent"
)

// geminiStreamBody 是假 gemini 上游的流式响应：文本两帧，末尾一帧 finishReason 加用量。
const geminiStreamBody = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[]},"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}

`

// postPath 往指定入站路径发一条 POST，返回状态与响应体。
func postPath(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return resp.StatusCode, string(b)
}

// TestForwardGeminiInboundToChat 钉住 gemini 入站 → chat 出站的整条非流式链路：
// 模型名从路径取（请求体里的 model 字段不算数），请求体按 chat 形状编出去，
// 响应按 gemini 形状编回来，用量从 chat 的 usage 转成 usageMetadata。
func TestForwardGeminiInboundToChat(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-inbound"
	up, rec := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.addAs(t, "c1", "ctest", translate.ProtocolChat, up.URL, key, []string{"gemini-2.5-flash"})
	srv := f.server(t)

	// 请求体里故意塞一个 model 字段：gemini 的报文里没有这个字段，模型名只认路径。
	body := `{"model":"body-model","contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"maxOutputTokens":16}}`
	status, resp := postPath(t, srv, geminiInboundPath, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	if rec.path != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q", rec.path)
	}
	if !strings.Contains(rec.body, `"model":"gemini-2.5-flash"`) {
		t.Fatalf("上游请求体里的模型名应当来自路径: %s", rec.body)
	}
	if !strings.Contains(rec.body, `"role":"user"`) || !strings.Contains(rec.body, `"max_tokens":16`) {
		t.Fatalf("上游请求体 = %s", rec.body)
	}

	var out translate.GeminiResponse
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是 gemini 形状: %v，原文 %s", err, resp)
	}
	if len(out.Candidates) != 1 || len(out.Candidates[0].Content.Parts) != 1 ||
		out.Candidates[0].Content.Parts[0].Text != "pong" {
		t.Fatalf("候选 = %+v", out.Candidates)
	}
	if out.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("finishReason = %q", out.Candidates[0].FinishReason)
	}
	if out.UsageMetadata == nil || out.UsageMetadata.PromptTokenCount != 3 ||
		out.UsageMetadata.CandidatesTokenCount != 1 || out.UsageMetadata.TotalTokenCount != 4 {
		t.Fatalf("usageMetadata = %+v", out.UsageMetadata)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolGemini || recs[0].Outbound != translate.ProtocolChat {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 3 || recs[0].OutTokens != 1 {
		t.Fatalf("记录的用量 = %d/%d", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestForwardGeminiInboundStreamToAnthropic 钉住 gemini 入站 → anthropic 出站的流式：
// 上游的 anthropic 事件折成增量，再编成 gemini 的 data 帧，收尾帧带 finishReason 与用量。
func TestForwardGeminiInboundStreamToAnthropic(t *testing.T) {
	f := newFixture(t)
	up, _ := fakeStreamUpstream(t, translate.ProtocolAnthropic)
	f.addAs(t, "a1", "atest", translate.ProtocolAnthropic, up.URL, "sk-ant-inbound",
		[]string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, resp := postPath(t, srv, geminiInboundStream,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	frames := geminiDataFrames(t, resp)
	if len(frames) < 3 {
		t.Fatalf("帧数 = %d，期望首帧加两段文本加收尾：%v", len(frames), frames)
	}
	var text strings.Builder
	for _, frame := range frames {
		for _, part := range frame.Candidates[0].Content.Parts {
			text.WriteString(part.Text)
		}
	}
	if text.String() != "pong" {
		t.Fatalf("流里的正文 = %q，期望 pong", text.String())
	}
	last := frames[len(frames)-1]
	if last.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("收尾帧的 finishReason = %q", last.Candidates[0].FinishReason)
	}
	if last.UsageMetadata == nil || last.UsageMetadata.PromptTokenCount != 3 ||
		last.UsageMetadata.CandidatesTokenCount != 2 {
		t.Fatalf("收尾帧的用量 = %+v，期望 3/2", last.UsageMetadata)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolGemini || recs[0].Outbound != translate.ProtocolAnthropic {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 3 || recs[0].OutTokens != 2 {
		t.Fatalf("记录的用量 = %d/%d", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestForwardGeminiOutboundStream 钉住 gemini 出站的流式：客户端说 anthropic，
// 上游请求打到 :streamGenerateContent 并带 alt=sse、凭据是 x-goog-api-key，
// gemini 的流式帧折成增量后编成 anthropic 事件，用量从 usageMetadata 记进请求记录。
func TestForwardGeminiOutboundStream(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-stream-out"

	var uri, googKey, reqBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		googKey = r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, geminiStreamBody)
	}))
	t.Cleanup(up.Close)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.URL},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, key, []string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, resp := postMessages(t, srv, `{"model":"gemini-2.5-flash","max_tokens":16,"stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	if uri != "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Fatalf("上游 URI = %q，期望流式端点带 alt=sse", uri)
	}
	if googKey != key {
		t.Fatalf("上游 x-goog-api-key = %q", googKey)
	}
	if strings.Contains(reqBody, `"stream"`) {
		t.Fatalf("gemini 的请求体里不该有 stream 字段: %s", reqBody)
	}
	if !strings.Contains(resp, "po") || !strings.Contains(resp, "ng") {
		t.Fatalf("客户端没拿到正文片段:\n%s", resp)
	}
	if !strings.Contains(resp, "message_stop") {
		t.Fatalf("anthropic 客户端没拿到收尾事件:\n%s", resp)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolAnthropic || recs[0].Outbound != translate.ProtocolGemini {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 12 || recs[0].OutTokens != 8 {
		t.Fatalf("记录的用量 = %d/%d，期望 12/8", recs[0].InTokens, recs[0].OutTokens)
	}
}

// TestGeminiPassthroughKeepsBytes 钉住 gemini 直通：报文里没有 model 字段可改，
// 请求体一个字节都不动（往里塞 model 会被上游当成未知字段），响应也按字节转发。
func TestGeminiPassthroughKeepsBytes(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-passthrough"
	const reply = geminiReply

	var uri, reqBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(up.Close)

	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, key, []string{"gemini-2.5-flash"})
	srv := f.server(t)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"x_vendor_field":{"keep":true}}`
	status, resp := postPath(t, srv, geminiInboundPath, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if reqBody != body {
		t.Fatalf("直通的请求体被改动了:\n得到 %s\n期望 %s", reqBody, body)
	}
	if uri != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("上游 URI = %q", uri)
	}
	if resp != reply {
		t.Fatalf("直通的响应被改动了:\n得到 %s\n期望 %s", resp, reply)
	}
	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].Inbound != translate.ProtocolGemini || recs[0].InTokens != 12 {
		t.Fatalf("请求记录 = %+v", recs)
	}
}

// TestGeminiInboundErrorBodyUnwrapped 钉住 gemini 的错误体：{"error":{"code","message","status"}}
// 里的 message 由 readErrorBody 解出来，客户端看到的是那句话而不是整段 JSON。
func TestGeminiInboundErrorBodyUnwrapped(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w,
			`{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`)
	}))
	t.Cleanup(up.Close)

	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini-bad", []string{"gemini-2.5-flash"})
	srv := f.server(t)

	status, resp := postPath(t, srv, geminiInboundPath,
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 %s", status, resp)
	}
	var out struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("错误体不是 JSON: %v，原文 %s", err, resp)
	}
	// 上游那一段 JSON 只取 message，客户端拿到的是那句话；code 是本次 HTTP 状态码，
	// status 按它映射（400 → INVALID_ARGUMENT），不是上游正文里的字段原样带出。
	if out.Error.Message != "API key not valid. Please pass a valid API key." {
		t.Fatalf("错误文案 = %q，期望只有上游原话", out.Error.Message)
	}
	if out.Error.Code != http.StatusBadRequest || out.Error.Status != "INVALID_ARGUMENT" {
		t.Fatalf("错误体 = %+v，期望 code=400、status=INVALID_ARGUMENT", out.Error)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK || recs[0].HTTPStatus != http.StatusBadRequest {
		t.Fatalf("请求记录 = %+v", recs)
	}
}

// TestGeminiInboundUnknownPathAndMethod 钉住入站路径的边界：动作后缀不认识时是 404，
// 方法不对时是 405 并带 Allow: POST。
func TestGeminiInboundUnknownPathAndMethod(t *testing.T) {
	f := newFixture(t)
	srv := f.server(t)

	if status, resp := postPath(t, srv, "/v1beta/models/gemini-2.5-flash:embedContent", `{}`); status != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404，响应 %s", status, resp)
	}

	resp, err := http.Get(srv.URL + geminiInboundPath)
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("状态 = %d，期望 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q，期望 POST", got)
	}
}

// TestGeminiInboundUnknownModel 钉住路径里的模型名参与选号：清单里没有这个名字时是 404，
// 文案带上路径里的名字，而不是请求体里的。
func TestGeminiInboundUnknownModel(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "c1", "ctest", translate.ProtocolChat, "http://127.0.0.1:1", "sk-chat",
		[]string{"other-model"})
	srv := f.server(t)

	status, resp := postPath(t, srv, "/v1beta/models/gemini-2.5-flash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404，响应 %s", status, resp)
	}
	if !strings.Contains(resp, "gemini-2.5-flash") {
		t.Fatalf("错误文案应当带上路径里的模型名: %s", resp)
	}
}

// geminiDataFrames 把客户端收到的 SSE 字节解成 gemini 的带候选帧：gemini 的流没有事件名，
// 每帧只有 data 行；错误帧不在这里返回。
func geminiDataFrames(t *testing.T, body string) []translate.GeminiResponse {
	t.Helper()
	var frames []translate.GeminiResponse
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame translate.GeminiResponse
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("帧不是合法 JSON: %v，原文 %s", err, data)
		}
		// 错误帧（{"error":…}）没有候选，正文断言另有专门的检查。
		if len(frame.Candidates) != 1 {
			continue
		}
		frames = append(frames, frame)
	}
	if len(frames) == 0 {
		t.Fatalf("响应里没有一个 data 帧:\n%s", body)
	}
	return frames
}

// TestGeminiStreamTruncatedEndsWithErrorAndFinish 钉住断流的两种走向：转换路径的收尾错误帧
// 走 gemini 编码器，直通路径补 gemini 形状的错误帧加带 finishReason 的收尾帧。两条路径都记
// 成失败，客户端总能拿到一个结束了的流。
func TestGeminiStreamTruncatedEndsWithErrorAndFinish(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		payload  string
	}{
		{
			// 转换路径：上游 anthropic 只发了一段正文就断，没有 message_stop。
			name:     "转换路径",
			upstream: translate.ProtocolAnthropic,
			payload: `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}

`,
		},
		{
			// 直通路径：上游 gemini 只发了一段正文就断，没有 finishReason。
			name:     "直通路径",
			upstream: translate.ProtocolGemini,
			payload: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}

`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := fixedSSEUpstream(t, c.payload)
			f.addAs(t, "u1", "utest", c.upstream, up.URL, "sk-trunc",
				[]string{"gemini-2.5-flash"})
			srv := f.server(t)

			status, resp := postPath(t, srv, geminiInboundStream,
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, resp)
			}
			if !strings.Contains(resp, `"error"`) {
				t.Fatalf("断流应当补一个错误帧:\n%s", resp)
			}
			frames := geminiDataFrames(t, resp)
			last := frames[len(frames)-1]
			if last.Candidates[0].FinishReason == "" {
				t.Fatalf("末帧应当带 finishReason:\n%s", resp)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || recs[0].OK {
				t.Fatalf("断流应当记成失败: %+v", recs)
			}
		})
	}
}

// TestGeminiStopNoteLogged 钉住非正常 finishReason 的去向：客户端拿到的结束原因是正常的
// （end_turn / stop），原值只进日志当备注，不进客户端的响应，也不算请求失败。
func TestGeminiStopNoteLogged(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"我不能回答"}]},`+
			`"finishReason":"SAFETY","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`)
	}))
	t.Cleanup(up.Close)

	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini-safety",
		[]string{"gemini-2.5-flash"})
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, resp := postTo(t, srv, `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`,
		translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if !strings.Contains(resp, "我不能回答") {
		t.Fatalf("客户端应当拿到上游的正文: %s", resp)
	}
	if !strings.Contains(resp, `"finish_reason":"stop"`) {
		t.Fatalf("非正常 finishReason 应当映射成正常结束: %s", resp)
	}
	if !strings.Contains(logs(), "SAFETY") {
		t.Fatalf("日志里应当留下原值当备注: %s", logs())
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("被安全策略截断不算转发失败: %+v", recs)
	}
}

// TestGeminiInboundModelNameIsPathUnescaped 钉住入站路径里的模型名转义：模型名里带 `/` 的名字
// 按路径段转义成 org%2Fgemini 也能取到，还原后的名字参与选号与上游报文。
func TestGeminiInboundModelNameIsPathUnescaped(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.addAs(t, "c1", "ctest", translate.ProtocolChat, up.URL, "sk-chat-escaped", []string{"org/gemini"})
	srv := f.server(t)

	status, resp := postPath(t, srv, "/v1beta/models/org%2Fgemini:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if !strings.Contains(rec.body, `"model":"org/gemini"`) {
		t.Fatalf("上游请求体里的模型名应当是还原后的名字: %s", rec.body)
	}
}

// TestGeminiInboundStreamToGeminiPassthrough 钉住 gemini 入站直通到 gemini 上游的流式：
// 客户端报文字节不变地发上去，出站地址换成 :streamGenerateContent?alt=sse，上游帧原样转到客户端。
func TestGeminiInboundStreamToGeminiPassthrough(t *testing.T) {
	f := newFixture(t)
	const key = "sk-gemini-pass-stream"

	var uri, reqBody, googKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		googKey = r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, geminiStreamBody)
	}))
	t.Cleanup(up.Close)

	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, key, []string{"gemini-2.5-flash"})
	srv := f.server(t)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"x_vendor_field":{"keep":true}}`
	status, resp := postPath(t, srv, geminiInboundStream, body)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if uri != "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Fatalf("上游 URI = %q", uri)
	}
	if googKey != key {
		t.Fatalf("上游 x-goog-api-key = %q", googKey)
	}
	if reqBody != body {
		t.Fatalf("直通的请求体被改动了:\n得到 %s\n期望 %s", reqBody, body)
	}
	if resp != geminiStreamBody {
		t.Fatalf("直通的流字节被改动了:\n得到 %s\n期望 %s", resp, geminiStreamBody)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("请求记录 = %+v", recs)
	}
	if recs[0].Inbound != translate.ProtocolGemini || recs[0].Outbound != translate.ProtocolGemini {
		t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
	if recs[0].InTokens != 12 || recs[0].OutTokens != 8 {
		t.Fatalf("流式直通记录的用量 = %d/%d，期望 12/8", recs[0].InTokens, recs[0].OutTokens)
	}
}
