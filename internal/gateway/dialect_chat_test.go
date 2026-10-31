package gateway_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// 非语音操作按方言分发：宿主先按有效出站协议编好报文，方言在它之上打字段补丁（请求侧）、
// 把上游响应改写成一条线协议形状的响应体（响应侧，ResultJSON 约定），流式则由方言逐块解。
// 这里用两条测试方言把三条路径都钉住。

const (
	chatDialectID          = "com.example.patch/chat@1"
	chatJSONOnlyDialectID  = "com.example.patch/json-only@1"
	chatDialectPluginID    = "com.example.patch"
	chatDialectTestModel   = "patch-model"
	chatDialectUpstreamURL = "/v1/chat/completions"
)

// chatPatchDialect 是实现了 Dialogue 与 StreamDialect 的测试方言。
// 请求侧加一个 enable_thinking 字段；响应侧把上游正文包成一条 chat 形状的响应体；
// 流式侧按 SSE 帧解 chat 的分片。
type chatPatchDialect struct {
	// path 非空时写进 Plan.Path，用来验证方言给地址这条能力。
	path string
	// buf 攒流式的半帧：块边界任意，方言自己处理半帧与多帧。
	buf bytes.Buffer
	// stop 是流式里记下的 finish_reason。
	stop string
	// textResult 为真时 Decode 返回纯文本结果，用来验证「对话入站不收纯文本」。
	textResult bool
	// streamErr 非空时第一次解流就报错，用来验证方言报错的收尾。
	streamErr error
}

func (d *chatPatchDialect) ID() string           { return chatDialectID }
func (d *chatPatchDialect) Operation() string    { return plugin.OpChatComplete }
func (d *chatPatchDialect) UpstreamWire() string { return plugin.WireOpenAIChat }

// Prepare 只动宿主报文里的一个字段：其余字节由宿主负责，方言不重编整包。
func (d *chatPatchDialect) Prepare(in plugin.Input) (plugin.Plan, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(in.Body, &fields); err != nil {
		return plugin.Plan{}, err
	}
	fields["enable_thinking"] = json.RawMessage(`true`)
	patched, err := json.Marshal(fields)
	if err != nil {
		return plugin.Plan{}, err
	}
	return plugin.Plan{Body: patched, Path: d.path}, nil
}

// Decode 把上游那条 chat 响应改写成另一条 chat 形状（ResultJSON 的约定：出站线协议形状），
// 正文前加「方言：」以证明客户端看到的字是方言给的，不是宿主直通回来的。
func (d *chatPatchDialect) Decode(in plugin.Input, resp plugin.Response) (plugin.Result, error) {
	var upstream struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &upstream); err != nil {
		return plugin.Result{}, err
	}
	text := ""
	if len(upstream.Choices) > 0 {
		text = upstream.Choices[0].Message.Content
	}
	if d.textResult {
		return plugin.Result{Kind: plugin.ResultText, Text: "纯文本"}, nil
	}
	body, err := json.Marshal(map[string]any{
		"id": "chatcmpl-dialect", "object": "chat.completion", "model": in.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "方言：" + text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 5, "total_tokens": 12},
	})
	if err != nil {
		return plugin.Result{}, err
	}
	return plugin.Result{
		Kind: plugin.ResultJSON, JSON: body,
		Usage: plugin.Usage{In: 7, Out: 5, Total: 12, Source: "upstream"},
	}, nil
}

// DecodeStream 按 SSE 空行切帧，把 chat 分片翻成语义事件。
func (d *chatPatchDialect) DecodeStream(in plugin.Input, chunk plugin.StreamChunk) ([]plugin.StreamEvent, error) {
	if d.streamErr != nil {
		return nil, d.streamErr
	}
	d.buf.Write(chunk.Data)
	var events []plugin.StreamEvent
	for {
		raw := d.buf.Bytes()
		i := bytes.Index(raw, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := string(raw[:i])
		d.buf.Next(i + 2)
		events = append(events, d.frameEvents(frame)...)
	}
	return events, nil
}

// frameEvents 把一帧 chat 分片解成事件。
func (d *chatPatchDialect) frameEvents(frame string) []plugin.StreamEvent {
	var events []plugin.StreamEvent
	for _, line := range strings.Split(frame, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			events = append(events, plugin.StreamEvent{Type: plugin.EventFinish, Reason: d.stop})
			continue
		}
		var env struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			continue
		}
		for _, ch := range env.Choices {
			if ch.Delta.Content != "" {
				events = append(events, plugin.StreamEvent{Type: plugin.EventTextDelta, Text: ch.Delta.Content})
			}
			if ch.FinishReason != "" {
				d.stop = ch.FinishReason
			}
		}
		if env.Usage != nil {
			events = append(events, plugin.StreamEvent{Type: plugin.EventUsage, Usage: &plugin.Usage{
				In: env.Usage.PromptTokens, Out: env.Usage.CompletionTokens,
			}})
		}
	}
	return events
}

// jsonOnlyDialect 只实现 Dialect，用来验证「客户端要流式但方言不会解流」这条拒绝。
type jsonOnlyDialect struct{}

func (jsonOnlyDialect) ID() string           { return chatJSONOnlyDialectID }
func (jsonOnlyDialect) Operation() string    { return plugin.OpChatComplete }
func (jsonOnlyDialect) UpstreamWire() string { return plugin.WireOpenAIChat }

func (jsonOnlyDialect) Prepare(in plugin.Input) (plugin.Plan, error) {
	return plugin.Plan{Body: in.Body}, nil
}

func (jsonOnlyDialect) Decode(in plugin.Input, resp plugin.Response) (plugin.Result, error) {
	return plugin.Result{Kind: plugin.ResultJSON, JSON: resp.Body}, nil
}

// dialectChatFixture 建一家 chat 厂商与一个账号，并把对话操作绑到给定方言上。
func dialectChatFixture(t *testing.T, baseURL, dialectID string) *fixture {
	t.Helper()
	f := newFixture(t)
	if err := f.st.AddProvider(store.Provider{
		Name: "patch", BaseURL: baseURL, DefaultProtocol: gateway.ProtocolChat,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: chatDialectUpstreamURL}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "patch1", "patch", "sk-patch-secret", []string{chatDialectTestModel}, 100)
	bindDialect(t, f, "patch", chatDialectTestModel, plugin.OpChatComplete, dialectID)
	return f
}

// dialectServer 用一张只装了测试方言的表起一个网关。
func dialectServer(t *testing.T, f *fixture, dialects ...plugin.Dialect) *httptest.Server {
	t.Helper()
	reg := plugin.DefaultRegistry()
	for _, d := range dialects {
		if err := reg.RegisterPluginDialect(chatDialectPluginID, d); err != nil {
			t.Fatalf("登记测试方言 %s: %v", d.ID(), err)
		}
	}
	return serverWithDialect(t, f, reg)
}

// captureDialectUpstream 记下上游收到的路径、请求头与请求体，并按 responseType 回一段正文。
type dialectUpstream struct {
	mu      sync.Mutex
	hits    int
	path    string
	accept  string
	body    []byte
	handler func(w http.ResponseWriter, r *http.Request)
}

func captureDialectUpstream(t *testing.T, responseType string, response []byte) (*httptest.Server, *dialectUpstream) {
	t.Helper()
	rec := &dialectUpstream{}
	rec.handler = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.hits++
		rec.path, rec.accept, rec.body = r.URL.Path, r.Header.Get("Accept"), raw
		rec.mu.Unlock()
		w.Header().Set("Content-Type", responseType)
		_, _ = w.Write(response)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (u *dialectUpstream) seen() (string, string, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.accept, u.body
}

func (u *dialectUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

// TestChatDialectPatchesRequestAndRewritesResponse 钉住非流式三段：
// 上游收到的是「宿主编码（chat 形状）+ 方言补丁」，客户端收到的是按入站协议编回的响应，
// 用量按中间表示入账。
func TestChatDialectPatchesRequestAndRewritesResponse(t *testing.T) {
	const upstreamBody = `{"id":"up","object":"chat.completion","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"上游原话"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	up, rec := captureDialectUpstream(t, "application/json", []byte(upstreamBody))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200，响应 = %s", status, body)
	}

	path, _, sent := rec.seen()
	if path != chatDialectUpstreamURL {
		t.Fatalf("上游路径 = %q，期望 %q", path, chatDialectUpstreamURL)
	}
	var patch map[string]any
	if err := json.Unmarshal(sent, &patch); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v，%s", err, sent)
	}
	if patch["enable_thinking"] != true {
		t.Fatalf("上游请求体没有方言补丁的 enable_thinking: %s", sent)
	}
	if _, ok := patch["messages"]; !ok {
		t.Fatalf("上游请求体不是宿主按 chat 编出的形状（缺 messages）: %s", sent)
	}
	if patch["model"] != chatDialectTestModel {
		t.Fatalf("上游模型名 = %v", patch["model"])
	}

	// 入站是 anthropic：响应必须是 anthropic 的形状，正文是方言改写过的。
	var reply struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("客户端响应不是 JSON: %v，%s", err, body)
	}
	if reply.Type != "message" || len(reply.Content) == 0 || reply.Content[0].Text != "方言：上游原话" {
		t.Fatalf("客户端响应不是按 anthropic 编回的方言结果: %s", body)
	}
	if strings.Contains(body, `"choices"`) {
		t.Fatalf("客户端拿到了上游 chat 的形状: %s", body)
	}

	recs := waitRequests(t, f.st, 1)
	if recs[0].InTokens != 7 || recs[0].OutTokens != 5 {
		t.Fatalf("用量 = in %d / out %d，期望中间表示里的 7 / 5", recs[0].InTokens, recs[0].OutTokens)
	}
	if recs[0].Inbound != gateway.ProtocolAnthropic || recs[0].Outbound != gateway.ProtocolChat {
		t.Fatalf("协议对 = %s→%s", recs[0].Inbound, recs[0].Outbound)
	}
}

// dialectSSE 是上游 chat 形状的三块文本、收尾与用量。
const dialectSSE = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"。\"},\"finish_reason\":\"stop\"}]," +
	"\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4}}\n\n" +
	"data: [DONE]\n\n"

// TestChatDialectStreamsToAnthropic 钉住流式：方言逐块解上游，客户端收到 anthropic 的
// content_block_delta 序列与最终 message_stop，用量在收尾帧里。
func TestChatDialectStreamsToAnthropic(t *testing.T) {
	up, rec := captureDialectUpstream(t, "text/event-stream", []byte(dialectSSE))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","max_stream":true,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200，响应 = %s", status, body)
	}
	if _, accept, _ := rec.seen(); accept != "text/event-stream" {
		t.Fatalf("上游 Accept = %q，期望 text/event-stream", accept)
	}
	for _, want := range []string{
		"event: message_start",
		`"text_delta"`, `"你"`, `"好"`, `"。"`,
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端流的缺 %s：\n%s", want, body)
		}
	}
	// 三块文本各自一条增量：三条 text_delta 而不是一整段。
	if n := strings.Count(body, `"text_delta"`); n != 3 {
		t.Fatalf("text_delta 帧数 = %d，期望 3：\n%s", n, body)
	}

	recs := waitRequests(t, f.st, 1)
	if recs[0].InTokens != 9 || recs[0].OutTokens != 4 {
		t.Fatalf("用量 = in %d / out %d，期望 9 / 4", recs[0].InTokens, recs[0].OutTokens)
	}
	if !recs[0].OK {
		t.Fatalf("流式请求记成了失败：%s", recs[0].Error)
	}
}

// TestChatDialectStreamsToChat 钉住同一个方言在 chat 入站上的流式形状：data: 帧加 [DONE]。
func TestChatDialectStreamsToChat(t *testing.T) {
	up, _ := captureDialectUpstream(t, "text/event-stream", []byte(dialectSSE))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{})

	status, body := postPath(t, srv, "/v1/chat/completions",
		`{"model":"patch-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200，响应 = %s", status, body)
	}
	for _, want := range []string{`"content":"你"`, `"content":"好"`, `"content":"。"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端流的缺 %s：\n%s", want, body)
		}
	}
}

// TestChatDialectWithoutStreamRejected 钉住：方言不会解流而客户端要流式，明确 400，
// 不静默退回非流式，也不发上游请求。
func TestChatDialectWithoutStreamRejected(t *testing.T) {
	up, rec := captureDialectUpstream(t, "application/json", []byte(`{}`))
	f := dialectChatFixture(t, up.URL, chatJSONOnlyDialectID)
	srv := dialectServer(t, f, jsonOnlyDialect{})

	status, body := postPath(t, srv, "/v1/chat/completions",
		`{"model":"patch-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 = %s", status, body)
	}
	if !strings.Contains(body, "不支持流式") {
		t.Fatalf("文案没说清方言不会解流：%s", body)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("上游被打了 %d 次，期望一次都不发", n)
	}
}

// TestChatDialectPathOverridesEndpoint 钉住 Plan.Path：地址变成厂商 base_url 加方言给的相对路径。
func TestChatDialectPathOverridesEndpoint(t *testing.T) {
	up, rec := captureDialectUpstream(t, "application/json", []byte(
		`{"id":"up","object":"chat.completion","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{path: "/v2/custom/chat"})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200，响应 = %s", status, body)
	}
	if path, _, _ := rec.seen(); path != "/v2/custom/chat" {
		t.Fatalf("上游路径 = %q，期望方言给的 /v2/custom/chat", path)
	}
}

// TestChatDialectRejectsAbsolutePath 钉住：方言给的整条地址被拒，请求不发出去。
// 客户端看到的 503 是「所有候选都失败」，这次尝试本身按本地构造失败记 502。
func TestChatDialectRejectsAbsolutePath(t *testing.T) {
	up, rec := captureDialectUpstream(t, "application/json", []byte(`{}`))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{path: "https://evil.example/steal"})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503，响应 = %s", status, body)
	}
	if !strings.Contains(body, "不是以 / 开头的相对路径") {
		t.Fatalf("文案没说清地址被拒的理由：%s", body)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("上游被打了 %d 次，期望一次都不发", n)
	}
	recs := waitRequests(t, f.st, 1)
	if recs[0].HTTPStatus != http.StatusBadGateway {
		t.Fatalf("这次尝试记的状态 = %d，期望 502", recs[0].HTTPStatus)
	}
}

// TestChatDialectTextResultRejected 钉住：对话入站不收纯文本结果，按方言实现错误报，
// 请求记录里是这次尝试的 502，客户端拿到的是「所有候选都失败」。
func TestChatDialectTextResultRejected(t *testing.T) {
	up, _ := captureDialectUpstream(t, "application/json", []byte(
		`{"id":"up","object":"chat.completion","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{textResult: true})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503，响应 = %s", status, body)
	}
	if !strings.Contains(body, "json 形状的上游响应体") {
		t.Fatalf("文案没说清对话操作要 json 结果：%s", body)
	}
	recs := waitRequests(t, f.st, 1)
	if recs[0].HTTPStatus != http.StatusBadGateway {
		t.Fatalf("这次尝试记的状态 = %d，期望 502", recs[0].HTTPStatus)
	}
}

// TestChatDialectStreamErrorEndsStream 钉住方言解流报错时客户端仍拿到收尾完整的流：
// 先一条错误帧，再按入站协议收尾；请求记录写明是方言的错。
func TestChatDialectStreamErrorEndsStream(t *testing.T) {
	up, _ := captureDialectUpstream(t, "text/event-stream", []byte(dialectSSE))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{streamErr: errors.New("上游分片读不懂")})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（流已经以 200 开头），响应 = %s", status, body)
	}
	for _, want := range []string{"event: error", "plugin_error", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端流的缺 %s：\n%s", want, body)
		}
	}
	recs := waitRequests(t, f.st, 1)
	if recs[0].OK || !strings.Contains(recs[0].Error, "plugin_error") {
		t.Fatalf("请求记录 = ok %v / error %q，期望记一次方言报错", recs[0].OK, recs[0].Error)
	}
}

// TestChatDialectStreamsAcrossChunks 钉住块边界任意：上游正文远超一次读的 4KB，
// 方言自己拼半帧，客户端拿到的仍是三条有序的文本增量与一个收尾。
func TestChatDialectStreamsAcrossChunks(t *testing.T) {
	// 注释帧不产生事件，只把正文撑到一块读不完。
	pad := ": " + strings.Repeat("x", 200) + "\n\n"
	var upstream strings.Builder
	upstream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n")
	for i := 0; i < 40; i++ {
		upstream.WriteString(pad)
	}
	upstream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n")
	upstream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"。\"},\"finish_reason\":\"stop\"}]}\n\n")
	upstream.WriteString("data: [DONE]\n\n")
	if upstream.Len() < 8<<10 {
		t.Fatalf("上游正文只有 %d 字节，撑不开多块读", upstream.Len())
	}

	up, _ := captureDialectUpstream(t, "text/event-stream", []byte(upstream.String()))
	f := dialectChatFixture(t, up.URL, chatDialectID)
	srv := dialectServer(t, f, &chatPatchDialect{})

	status, body := postPath(t, srv, "/v1/messages",
		`{"model":"patch-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200，响应 = %s", status, body)
	}
	if n := strings.Count(body, `"text_delta"`); n != 3 {
		t.Fatalf("text_delta 帧数 = %d，期望 3：\n%s", n, body)
	}
	// 顺序也要对：拼帧错位时正文会串。
	last := -1
	for _, want := range []string{`"你"`, `"好"`, `"。"`} {
		at := strings.Index(body, want)
		if at < 0 || at < last {
			t.Fatalf("文本增量顺序不对（%s 在 %d）：\n%s", want, at, body)
		}
		last = at
	}
}
