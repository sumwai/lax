package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/translate"
)

// fourProtocols 是四条线协议，矩阵按「入站 × 出站」全排列跑。
var fourProtocols = []string{
	translate.ProtocolAnthropic,
	translate.ProtocolChat,
	translate.ProtocolResponses,
	translate.ProtocolGemini,
}

// 四条协议各自的一个最小请求与最小响应，用于矩阵测试。
// gemini 的请求体里没有 model 字段（模型名在路径上），这里放一个陌生字段，
// 直通用例因此同时断言「字节不动」与「陌生字段保留」。
var (
	requestBodies = map[string]string{
		translate.ProtocolAnthropic: `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		translate.ProtocolChat:      `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		translate.ProtocolResponses: `{"model":"m1","max_tokens":16,"input":"hi"}`,
		translate.ProtocolGemini: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
			`"x_vendor_field":{"keep":true}}`,
	}

	upstreamPaths = map[string]string{
		translate.ProtocolAnthropic: "/v1/messages",
		translate.ProtocolChat:      "/v1/chat/completions",
		translate.ProtocolResponses: "/v1/responses",
		// gemini 的模型名在路径里，占位符要在发请求前换成 m1。
		translate.ProtocolGemini: "/v1beta/models/m1:generateContent",
	}

	// 上游各协议的响应体，文本都是 pong，用量都是 3/1。
	// gemini 的响应体带一个陌生字段 modelVersion，直通的字节相等断言因此也覆盖它。
	upstreamBodies = map[string]string{
		translate.ProtocolAnthropic: `{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`,
		translate.ProtocolChat:      `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		translate.ProtocolResponses: `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`,
		translate.ProtocolGemini: `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},` +
			`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,` +
			`"totalTokenCount":4},"modelVersion":"gemini-2.5-flash"}`,
	}
)

// fakeProtocolUpstream 起一个只说某一种协议的上游，记录收到的路径与报文。
func fakeProtocolUpstream(t *testing.T, protocol string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.set(r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), string(body))
		rec.uri = r.URL.RequestURI()
		rec.googKey = r.Header.Get("x-goog-api-key")
		if r.URL.Path != upstreamPaths[protocol] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"wrong path `+r.URL.Path+`"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBodies[protocol])
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// recordedRequest 是一次打到假上游的请求。uri 带查询串（gemini 流式要断言 alt=sse），
// googKey 是 gemini 的凭据头（前三个头都装不下它）。
type recordedRequest struct {
	path    string
	uri     string
	bearer  string
	apiKey  string
	googKey string
	body    string
}

func (r *recordedRequest) set(path, bearer, apiKey, body string) {
	r.path, r.bearer, r.apiKey, r.body = path, bearer, apiKey, body
}

// TestProtocolMatrix 逐一验证「入站协议 × 厂商默认协议」十六种组合，断言形状、凭据头、用量与请求记录。
func TestProtocolMatrix(t *testing.T) {
	for _, inbound := range fourProtocols {
		for _, outbound := range fourProtocols {
			t.Run(inbound+"_to_"+outbound, func(t *testing.T) {
				f := newFixture(t)
				up, rec := fakeProtocolUpstream(t, outbound)
				f.add(t, "up-"+outbound, outbound, up.URL, "sk-test", []string{"m1"}, 100)
				srv := f.server(t)

				status, body := postTo(t, srv, requestBodies[inbound], inbound)
				if status != http.StatusOK {
					t.Fatalf("状态 = %d，响应 %s", status, body)
				}

				// 客户端拿到的必须是入站协议的形状。
				if !strings.Contains(body, "pong") {
					t.Fatalf("响应里没有正文: %s", body)
				}
				res, err := translate.DecodeResponse(inbound, []byte(body), "m1")
				if err != nil {
					t.Fatalf("响应不是 %s 的形状: %v，原文 %s", inbound, err, body)
				}
				if len(res.Blocks) == 0 || res.Blocks[0].Text != "pong" {
					t.Fatalf("%s 响应解析结果 = %+v", inbound, res.Blocks)
				}
				if res.Usage.In != 3 || res.Usage.Out != 1 {
					t.Fatalf("用量 = %+v，期望 3/1", res.Usage)
				}

				// 上游必须收到对应协议的路径与凭据：凭据头按协议选，不是按厂商猜。
				if rec.path != upstreamPaths[outbound] {
					t.Fatalf("上游路径 = %q，期望 %q", rec.path, upstreamPaths[outbound])
				}
				switch outbound {
				case translate.ProtocolAnthropic:
					if rec.apiKey != "sk-test" || rec.bearer != "" || rec.googKey != "" {
						t.Fatalf("anthropic 凭据头 = x-api-key %q，Authorization %q，x-goog-api-key %q",
							rec.apiKey, rec.bearer, rec.googKey)
					}
				case translate.ProtocolGemini:
					if rec.googKey != "sk-test" || rec.bearer != "" || rec.apiKey != "" {
						t.Fatalf("gemini 凭据头 = x-goog-api-key %q，Authorization %q，x-api-key %q",
							rec.googKey, rec.bearer, rec.apiKey)
					}
				default:
					if rec.bearer != "Bearer sk-test" || rec.googKey != "" {
						t.Fatalf("openai 凭据头 = Authorization %q，x-goog-api-key %q", rec.bearer, rec.googKey)
					}
				}

				// gemini 的报文里没有 model 字段（模型名在路径上）：跨协议编出去的请求体
				// 里带一个 model 会被上游当成未知字段拒掉。
				if outbound == translate.ProtocolGemini && strings.Contains(rec.body, `"model"`) {
					t.Fatalf("gemini 请求体里不该有 model 字段: %s", rec.body)
				}

				// 同协议直通要保证字节不变（只换 model）。gemini 连 model 都不换。
				if inbound == outbound {
					if strings.TrimSpace(body) != strings.TrimSpace(upstreamBodies[outbound]) {
						t.Fatalf("直通改动了响应字节:\n得到 %s\n期望 %s", body, upstreamBodies[outbound])
					}
				}

				if inbound == translate.ProtocolGemini {
					if outbound == translate.ProtocolGemini {
						// 直通的请求体一个字节都不动，陌生字段随原样到上游。
						if rec.body != requestBodies[translate.ProtocolGemini] {
							t.Fatalf("gemini 直通的请求体被改动了:\n得到 %s\n期望 %s",
								rec.body, requestBodies[translate.ProtocolGemini])
						}
					} else if strings.Contains(rec.body, "x_vendor_field") {
						t.Fatalf("跨协议不该搬陌生字段: %s", rec.body)
					}
				}

				recs, err := f.st.RecentRequests(5)
				if err != nil {
					t.Fatalf("读请求记录: %v", err)
				}
				if len(recs) != 1 || !recs[0].OK {
					t.Fatalf("请求记录 = %+v", recs)
				}
				if recs[0].Inbound != inbound || recs[0].Outbound != outbound {
					t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
				}
				if recs[0].InTokens != 3 || recs[0].OutTokens != 1 {
					t.Fatalf("记录的用量 = %d/%d", recs[0].InTokens, recs[0].OutTokens)
				}
			})
		}
	}
}

// TestPassthroughKeepsUnknownFields 说明直通不改报文：入站报文里的陌生字段要原样到上游。
func TestPassthroughKeepsUnknownFields(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.add(t, "chat", translate.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"x_vendor_field":{"keep":true}}`
	status, resp := postTo(t, srv, body, translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if !strings.Contains(rec.body, `"x_vendor_field":{"keep":true}`) {
		t.Fatalf("直通丢了陌生字段: %s", rec.body)
	}
}

// TestCrossProtocolDropsUnknownFields 说明跨协议转换只搬映射表里的字段。
func TestCrossProtocolDropsUnknownFields(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.add(t, "chat", translate.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"x_vendor_field":{"keep":true}}`
	status, resp := postTo(t, srv, body, translate.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}
	if strings.Contains(rec.body, "x_vendor_field") {
		t.Fatalf("跨协议不该搬陌生字段: %s", rec.body)
	}
	if !strings.Contains(rec.body, `"role":"user"`) {
		t.Fatalf("跨协议生成的请求体 = %s", rec.body)
	}
}

// TestUnknownInboundPath 未知路径应当 404。
func TestUnknownInboundPath(t *testing.T) {
	f := newFixture(t)
	srv := f.server(t)

	resp, err := http.Post(srv.URL+"/v1/nope", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404", resp.StatusCode)
	}
}

// inboundPathFor 给出一条入站路径。gemini 的模型名与流式标志都在路径里，所以多一个 stream 参数：
// 同一份请求体发给 :generateContent 是同步响应，发给 :streamGenerateContent 才是流。
func inboundPathFor(protocol string, stream bool) string {
	switch protocol {
	case translate.ProtocolAnthropic:
		return "/v1/messages"
	case translate.ProtocolChat:
		return "/v1/chat/completions"
	case translate.ProtocolResponses:
		return "/v1/responses"
	case translate.ProtocolGemini:
		if stream {
			return "/v1beta/models/m1:streamGenerateContent"
		}
		return "/v1beta/models/m1:generateContent"
	}
	return ""
}

// postTo 按入站协议选路径发一条非流式请求。
func postTo(t *testing.T, srv *httptest.Server, body, protocol string) (int, string) {
	t.Helper()
	return postPathTo(t, srv, inboundPathFor(protocol, false), body, protocol)
}

// postStreamTo 按入站协议发一条流式请求。
func postStreamTo(t *testing.T, srv *httptest.Server, body, protocol string) (int, string) {
	t.Helper()
	return postPathTo(t, srv, inboundPathFor(protocol, true), body, protocol)
}

// postPathTo 往指定入站路径发一条 POST，路径为空说明协议没有入站入口。
func postPathTo(t *testing.T, srv *httptest.Server, path, body, protocol string) (int, string) {
	t.Helper()
	if path == "" {
		t.Fatalf("没有协议 %s 的入站路径", protocol)
	}
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
