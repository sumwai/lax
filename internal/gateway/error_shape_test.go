package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/translate"
)

// 这个文件钉住「错误体按入站协议给形状」：四条入站路径的客户端读的字段不同，
// 形状由入站协议决定，不由上游协议决定。anthropic 是统一形状加 error.type；两条 openai 入站
// 只有顶层 error，param 与 code 显式给 null；gemini 是 google 的 code（HTTP 状态码整数）与
// status（状态名）。没有入站协议可依的路径（/v1/models、/healthz、未知路径、405）仍是统一形状。

// sqlNull 与 rawJSON 是断言里常用的两个原值：缺失的键与 null 不是一个东西，
// 所以这里不先用结构体归一，直接对着原始 JSON 比。
const rawNull = "null"

// errorKeys 取一张解码后的 JSON 对象的键，报错文案用。
func errorKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// decodeErrorBody 把错误体解成两层原始 JSON：顶层与 error 里各有哪些键、值是什么。
func decodeErrorBody(t *testing.T, body string) (map[string]json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("错误体不是 JSON 对象: %v，原文 %s", err, body)
	}
	raw, ok := top["error"]
	if !ok {
		t.Fatalf("错误体里没有 error: %s", body)
	}
	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &errObj); err != nil {
		t.Fatalf("error 不是 JSON 对象: %v，原文 %s", err, raw)
	}
	return top, errObj
}

// requireKeys 核对一层 JSON 的键集合：多一个少一个都算形状不对，这是「按协议给形状」的核心。
func requireKeys(t *testing.T, where string, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 的键 = %v，期望 %v", where, errorKeys(got), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s 少了键 %q，实际 %v", where, k, errorKeys(got))
		}
	}
}

// errorShape 是一个用例对四条入站路径各自的期望：type 给两条 anthropic/openai 形状用，
// googleStatus 给 gemini 形状用；code 一律等于 HTTP 状态码。
type errorShape struct {
	inbound      string
	wantType     string
	googleStatus string
	wantMessage  []string
}

// assertInboundError 逐字段核对一次非流式错误响应。
func assertInboundError(t *testing.T, c errorShape, status, wantStatus int, body string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("%s 状态 = %d，期望 %d：%s", c.inbound, status, wantStatus, body)
	}
	top, errObj := decodeErrorBody(t, body)

	var message string
	if raw, ok := errObj["message"]; ok {
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("%s 的 error.message 不是字符串: %v，原文 %s", c.inbound, err, raw)
		}
	}

	switch c.inbound {
	case translate.ProtocolAnthropic:
		// anthropic 客户端读顶层 type 与 error.type，形状与没有入站协议的路径相同。
		requireKeys(t, "顶层", top, "type", "error")
		if string(top["type"]) != `"error"` {
			t.Fatalf("anthropic 顶层 type = %s，期望 \"error\"", top["type"])
		}
		requireKeys(t, "error", errObj, "type", "message")
		if got := string(errObj["type"]); got != strconv.Quote(c.wantType) {
			t.Fatalf("anthropic 的 error.type = %s，期望 %q", got, c.wantType)
		}
	case translate.ProtocolChat, translate.ProtocolResponses:
		// 两条 openai 入站同形：顶层没有 type，error 里 param 与 code 两个键都在、值都是 null
		// （SDK 会读这两个键在不在）。
		requireKeys(t, "顶层", top, "error")
		requireKeys(t, "error", errObj, "message", "type", "param", "code")
		if got := string(errObj["type"]); got != strconv.Quote(c.wantType) {
			t.Fatalf("%s 的 error.type = %s，期望 %q", c.inbound, got, c.wantType)
		}
		for _, k := range []string{"param", "code"} {
			if got := string(errObj[k]); got != rawNull {
				t.Fatalf("%s 的 error.%s = %s，期望 null", c.inbound, k, got)
			}
		}
	case translate.ProtocolGemini:
		// gemini 客户端读 error.code（HTTP 状态码整数）与 error.status（google 的状态名）。
		requireKeys(t, "顶层", top, "error")
		requireKeys(t, "error", errObj, "code", "message", "status")
		if got := string(errObj["code"]); got != strconv.Itoa(wantStatus) {
			t.Fatalf("gemini 的 error.code = %s，期望 %d", got, wantStatus)
		}
		if got := string(errObj["status"]); got != strconv.Quote(c.googleStatus) {
			t.Fatalf("gemini 的 error.status = %s，期望 %q", got, c.googleStatus)
		}
	}

	if message == "" {
		t.Fatalf("%s 的 error.message 是空的: %s", c.inbound, body)
	}
	for _, want := range c.wantMessage {
		if !strings.Contains(message, want) {
			t.Fatalf("%s 的错误文案 %q 缺少 %q", c.inbound, message, want)
		}
	}
}

// failingStatusUpstream 起一台总是回固定状态与正文的假上游：用来制造「所有候选账号都失败」。
func failingStatusUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestInboundErrorShapeWhenAllAccountsFail 钉住 503：只有一个账号、上游必失败，四条入站路径
// 各自拿到自己的形状，type 按入站协议取（anthropic 的 api_error、openai 的 server_error、
// google 的 code=503/status=UNAVAILABLE）。
func TestInboundErrorShapeWhenAllAccountsFail(t *testing.T) {
	cases := []errorShape{
		{inbound: translate.ProtocolAnthropic, wantType: "api_error",
			wantMessage: []string{"所有候选账号都失败", "上游挂了"}},
		{inbound: translate.ProtocolChat, wantType: "server_error",
			wantMessage: []string{"所有候选账号都失败", "上游挂了"}},
		{inbound: translate.ProtocolResponses, wantType: "server_error",
			wantMessage: []string{"所有候选账号都失败", "上游挂了"}},
		{inbound: translate.ProtocolGemini, googleStatus: "UNAVAILABLE",
			wantMessage: []string{"所有候选账号都失败", "上游挂了"}},
	}

	for _, c := range cases {
		t.Run(c.inbound, func(t *testing.T) {
			f := newFixture(t)
			up := failingStatusUpstream(t, http.StatusInternalServerError, `{"error":{"message":"上游挂了"}}`)
			f.add(t, "ds1", c.inbound, up.URL, "sk-test", []string{"m1"}, 100)
			srv := f.server(t)

			status, body := postTo(t, srv, requestBodies[c.inbound], c.inbound)
			assertInboundError(t, c, status, http.StatusServiceUnavailable, body)
		})
	}
}

// TestInboundErrorShapeWhenModelMissing 钉住 404：没有账号提供这个模型，四条入站路径各给自己的
// 形状。两条 openai 入站的类型与 anthropic 不同（invalid_request_error 而不是 not_found_error），
// google 的 code 是 404、status 是 NOT_FOUND。
func TestInboundErrorShapeWhenModelMissing(t *testing.T) {
	const model = "no-such-model"
	cases := []errorShape{
		{inbound: translate.ProtocolAnthropic, wantType: "not_found_error",
			wantMessage: []string{model, "lax model ls", "lax model probe"}},
		{inbound: translate.ProtocolChat, wantType: "invalid_request_error",
			wantMessage: []string{model, "lax model ls"}},
		{inbound: translate.ProtocolResponses, wantType: "invalid_request_error",
			wantMessage: []string{model, "lax model ls"}},
		{inbound: translate.ProtocolGemini, googleStatus: "NOT_FOUND",
			wantMessage: []string{model, "lax model ls"}},
	}

	for _, c := range cases {
		t.Run(c.inbound, func(t *testing.T) {
			f := newFixture(t)
			// 一个刚建号、模型列表为空的账号：它不提供任何模型，不会接住任意模型名。
			f.add(t, "fresh", translate.ProtocolChat, "http://127.0.0.1:1", "sk-1", nil, 100)
			srv := f.server(t)

			// gemini 的模型名在路径里，其余三条在请求体里：两边都要换成不存在的名字。
			body := requestBodies[c.inbound]
			path := inboundPathFor(c.inbound, false)
			if c.inbound == translate.ProtocolGemini {
				path = "/v1beta/models/" + model + ":generateContent"
			} else {
				body = strings.Replace(body, `"model":"m1"`, `"model":"`+model+`"`, 1)
			}
			status, got := postPathTo(t, srv, path, body, c.inbound)
			assertInboundError(t, c, status, http.StatusNotFound, got)
		})
	}
}

// TestInboundErrorShapeScrubsSecret 钉住四种形状都过 scrubSecret：上游把凭据回显到错误正文里，
// 客户端拿到的四种形状里都只能是 ***。
func TestInboundErrorShapeScrubsSecret(t *testing.T) {
	const key = "sk-echo-in-error-body"

	for _, c := range []errorShape{
		{inbound: translate.ProtocolAnthropic, wantType: "api_error"},
		{inbound: translate.ProtocolChat, wantType: "server_error"},
		{inbound: translate.ProtocolResponses, wantType: "server_error"},
		{inbound: translate.ProtocolGemini, googleStatus: "UNAVAILABLE"},
	} {
		t.Run(c.inbound, func(t *testing.T) {
			f := newFixture(t)
			up := failingStatusUpstream(t, http.StatusInternalServerError,
				`{"error":{"message":"invalid key `+key+`"}}`)
			f.add(t, "ds1", c.inbound, up.URL, key, []string{"m1"}, 100)
			srv := f.server(t)

			status, body := postTo(t, srv, requestBodies[c.inbound], c.inbound)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("状态 = %d，期望 503：%s", status, body)
			}
			if strings.Contains(body, key) {
				t.Fatalf("错误体泄漏了凭据: %s", body)
			}
			if !strings.Contains(body, "***") {
				t.Fatalf("错误体应当替换凭据而不是丢掉原话: %s", body)
			}
			c.wantMessage = []string{"***"}
			assertInboundError(t, c, status, http.StatusServiceUnavailable, body)
		})
	}
}

// TestReadOnlyAndUnknownPathsKeepUnifiedErrorShape 钉住没有入站协议可依的入口：未知路径与 405
// 仍是统一形状（顶层 type 加 error.type），/healthz 的响应形状不变。
func TestReadOnlyAndUnknownPathsKeepUnifiedErrorShape(t *testing.T) {
	f := newFixture(t)
	f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-1", []string{"m1"}, 100)
	srv := f.server(t)

	// 未知路径 404、四条入站路径的 405 都是统一形状。
	cases := []struct {
		name   string
		method string
		path   string
		status int
		kind   string
	}{
		// 认不出的 gemini 动作后缀也走统一形状（别的路径由 mux 直接回 404）。
		{"未知动作", http.MethodPost, "/v1beta/models/m1:noSuchAction", http.StatusNotFound, "not_found_error"},
		{"anthropic 405", http.MethodGet, "/v1/messages", http.StatusMethodNotAllowed, "invalid_request_error"},
		{"chat 405", http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed, "invalid_request_error"},
		{"responses 405", http.MethodGet, "/v1/responses", http.StatusMethodNotAllowed, "invalid_request_error"},
		{"gemini 405", http.MethodGet, "/v1beta/models/m1:generateContent", http.StatusMethodNotAllowed, "invalid_request_error"},
		{"模型清单 405", http.MethodPost, "/v1/models", http.StatusMethodNotAllowed, "invalid_request_error"},
		{"清单里没有这个模型", http.MethodGet, "/v1/models/nope", http.StatusNotFound, "not_found_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("构造请求: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", c.method, c.path, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("状态 = %d，期望 %d：%s", resp.StatusCode, c.status, string(b))
			}
			shape := errorShape{inbound: translate.ProtocolAnthropic, wantType: c.kind}
			assertInboundError(t, shape, resp.StatusCode, c.status, string(b))
		})
	}

	// 完全不认识的路径由 mux 的 404 接住，不经过入站处理器：状态仍必须是 404。
	if status, _ := postPathTo(t, srv, "/v1/unknown", `{}`, translate.ProtocolChat); status != http.StatusNotFound {
		t.Fatalf("完全不认识的路径状态 = %d，期望 404", status)
	}

	// /healthz 不是错误入口，形状不变：状态与计数字段都在。
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求 /healthz: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 状态 = %d：%s", resp.StatusCode, string(b))
	}
	var health map[string]json.RawMessage
	if err := json.Unmarshal(b, &health); err != nil {
		t.Fatalf("/healthz 不是 JSON 对象: %v，原文 %s", err, string(b))
	}
	requireKeys(t, "/healthz", health,
		"status", "accounts", "enabled", "models", "inbound", "outbound", "passthru")
}

// streamErrorCases 是四条入站路径的断流用例：上游协议与入站协议相同（直通），
// 收尾帧之前掉线，网关补的错误帧必须按入站协议给形状。
func streamErrorCases() []errorShape {
	return []errorShape{
		{inbound: translate.ProtocolAnthropic},
		{inbound: translate.ProtocolChat},
		{inbound: translate.ProtocolResponses},
		{inbound: translate.ProtocolGemini},
	}
}

// cutBeforeEndFrame 去掉完整流的最后一帧（收尾帧），留下前面几帧作为「上游只发到这里」。
func cutBeforeEndFrame(t *testing.T, protocol string) (prefix, full string) {
	t.Helper()
	frames := strings.Split(strings.TrimSuffix(streamBodies[protocol], "\n\n"), "\n\n")
	last := frames[len(frames)-1]
	if !strings.Contains(last, streamEndMarkers[protocol]) {
		t.Fatalf("%s 的最后一帧里没有收尾标记 %q：%q", protocol, streamEndMarkers[protocol], last)
	}
	prefix = strings.Join(frames[:len(frames)-1], "\n\n") + "\n\n"
	return prefix, prefix + last + "\n\n"
}

// streamData 取出客户端收到的所有 data 行（不含事件名）。
func streamData(body string) []string {
	var out []string
	for _, chunk := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(chunk, "\n") {
			if strings.HasPrefix(line, "data: ") {
				out = append(out, strings.TrimPrefix(line, "data: "))
			}
		}
	}
	return out
}

// TestInboundStreamErrorFrameShapePerProtocol 钉住流内错误帧按入站协议给形状：
// anthropic 是 event: error 加 {"type":"error",…}；openai-chat 是一帧 data: {"error":{…}}；
// openai-responses 是 error 事件加 response.failed（终态，没有 [DONE]）；
// gemini 是没有事件名的 data: {"error":{"code","message","status"}}。
func TestInboundStreamErrorFrameShapePerProtocol(t *testing.T) {
	for _, c := range streamErrorCases() {
		t.Run(c.inbound, func(t *testing.T) {
			prefix, full := cutBeforeEndFrame(t, c.inbound)
			up := truncatedSSEUpstream(t, full, prefix)
			f := newFixture(t)
			f.add(t, "ds1", c.inbound, up.URL, "sk-cut", []string{"m1"}, 100)
			gs, _ := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			body := requestBodies[c.inbound]
			if c.inbound != translate.ProtocolGemini {
				body = strings.Replace(body, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
			}
			status, got := postStreamTo(t, srv, body, c.inbound)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，期望 200（响应头已发出）：%s", status, got)
			}

			switch c.inbound {
			case translate.ProtocolAnthropic:
				kind, message := errorFrame(t, got)
				if kind != "api_error" {
					t.Fatalf("anthropic 错误帧的 error.type = %q，期望 api_error", kind)
				}
				if !strings.Contains(message, "上游流式响应中断") {
					t.Fatalf("anthropic 错误帧的文案 = %q，期望写明上游中断", message)
				}
				if !strings.Contains(got, "event: message_stop\n") {
					t.Fatalf("anthropic 流应当以 message_stop 收尾：%s", tail(got, 200))
				}
			case translate.ProtocolChat:
				// chat 的错误帧没有事件名，只有 data 行；错误体在顶层 error 下。
				var frame struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				found := false
				for _, data := range streamData(got) {
					if !strings.HasPrefix(data, `{"error":`) {
						continue
					}
					found = true
					if err := json.Unmarshal([]byte(data), &frame); err != nil {
						t.Fatalf("chat 错误帧不是合法 JSON: %v，%q", err, data)
					}
				}
				if !found {
					t.Fatalf("chat 流里没有 data: {\"error\":…} 帧：%s", tail(got, 200))
				}
				if frame.Error.Type == "" || !strings.Contains(frame.Error.Message, "上游流式响应中断") {
					t.Fatalf("chat 错误帧 = %+v，期望带 type 且写明上游中断", frame.Error)
				}
				if !strings.HasSuffix(got, "data: [DONE]\n\n") {
					t.Fatalf("chat 流应当以 [DONE] 收尾：%s", tail(got, 80))
				}
			case translate.ProtocolResponses:
				// responses 的终态是 response.failed，错误体在 response.error 里（code 与 message）。
				var failed struct {
					Type     string `json:"type"`
					Response struct {
						Status string `json:"status"`
						Error  struct {
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(lastEventData(t, got, "response.failed")), &failed); err != nil {
					t.Fatalf("response.failed 的 data 不是合法 JSON: %v", err)
				}
				if failed.Type != "response.failed" || failed.Response.Status != "failed" {
					t.Fatalf("response.failed 的内容 = %+v，期望 type=response.failed、status=failed", failed)
				}
				if failed.Response.Error.Code != "server_error" ||
					!strings.Contains(failed.Response.Error.Message, "上游流式响应中断") {
					t.Fatalf("response.error = %+v，期望 code=server_error 且写明上游中断", failed.Response.Error)
				}
				if !strings.Contains(got, "event: error\n") {
					t.Fatalf("responses 流里应有 error 事件：%s", tail(got, 200))
				}
				if strings.Contains(got, "[DONE]") {
					t.Fatalf("responses 没有 [DONE]，不该出现：%s", tail(got, 120))
				}
			case translate.ProtocolGemini:
				// gemini 的错误帧是 google 的形状，且没有事件名。
				if strings.Contains(got, "event:") {
					t.Fatalf("gemini 客户端的流不该有 event 行：%s", tail(got, 200))
				}
				var frame struct {
					Error struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
						Status  string `json:"status"`
					} `json:"error"`
				}
				found := false
				for _, data := range streamData(got) {
					var env map[string]json.RawMessage
					if err := json.Unmarshal([]byte(data), &env); err != nil {
						continue
					}
					if _, ok := env["error"]; !ok {
						continue
					}
					found = true
					if err := json.Unmarshal([]byte(data), &frame); err != nil {
						t.Fatalf("gemini 错误帧不是合法 JSON: %v，%q", err, data)
					}
				}
				if !found {
					t.Fatalf("gemini 流里没有 data: {\"error\":…} 帧：%s", tail(got, 200))
				}
				if frame.Error.Code != http.StatusInternalServerError || frame.Error.Status != "INTERNAL" {
					t.Fatalf("gemini 错误帧 = %+v，期望 code=500、status=INTERNAL", frame.Error)
				}
				if !strings.Contains(frame.Error.Message, "上游流式响应中断") {
					t.Fatalf("gemini 错误帧的文案 = %q，期望写明上游中断", frame.Error.Message)
				}
			}

			// 断流这一次记成失败，与客户端看到的错误帧对得上。
			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 || recs[0].OK {
				t.Fatalf("请求记录 = %+v，期望一行 ok=0", recs)
			}
			if !strings.Contains(recs[0].Error, "上游流式响应中断") {
				t.Fatalf("请求记录的 error = %q，期望写明上游中断", recs[0].Error)
			}
		})
	}
}

// TestInboundStreamErrorFrameScrubsSecretPerProtocol 钉住四种形状的流内错误帧都过 scrubSecret：
// 上游在流里发一帧带凭据的错误体（出站协议与入站不同，走转换路径），客户端拿到的错误帧里
// 只有 ***。gemini 入站配 chat 上游：gemini 上游配 gemini 入站是直通，字节不改也就谈不上脱敏。
func TestInboundStreamErrorFrameScrubsSecretPerProtocol(t *testing.T) {
	const key = "sk-leak-in-converted-stream"

	cases := []struct {
		inbound  string
		upstream string
	}{
		{translate.ProtocolAnthropic, translate.ProtocolGemini},
		{translate.ProtocolChat, translate.ProtocolGemini},
		{translate.ProtocolResponses, translate.ProtocolGemini},
		{translate.ProtocolGemini, translate.ProtocolChat},
	}
	for _, c := range cases {
		t.Run(c.inbound+"_from_"+c.upstream, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if c.upstream == translate.ProtocolChat {
					_, _ = io.WriteString(w, `data: {"error":{"type":"invalid_request_error","message":"API key `+key+` is invalid"}}`+"\n\n")
					return
				}
				_, _ = io.WriteString(w, `data: {"error":{"code":401,"message":"API key `+key+` is invalid"}}`+"\n\n")
			}))
			t.Cleanup(up.Close)

			f := newFixture(t)
			f.add(t, "up1", c.upstream, up.URL, key, []string{"m1"}, 100)
			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			body := requestBodies[c.inbound]
			if c.inbound != translate.ProtocolGemini {
				body = strings.Replace(body, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
			}
			status, got := postStreamTo(t, srv, body, c.inbound)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，期望 200（响应头已发出）：%s", status, got)
			}
			if strings.Contains(got, key) {
				t.Fatalf("%s 错误帧泄漏了凭据：%s", c.inbound, tail(got, 400))
			}
			if !strings.Contains(got, "***") {
				t.Fatalf("%s 错误帧应当保留上游原话并替换凭据：%s", c.inbound, tail(got, 400))
			}

			// 每种形状都把脱敏后的原话放在自己的落点上。
			var message string
			switch c.inbound {
			case translate.ProtocolAnthropic:
				_, message = errorFrame(t, got)
			case translate.ProtocolChat:
				for _, data := range streamData(got) {
					var env struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(data), &env); err == nil && env.Error.Message != "" {
						message = env.Error.Message
					}
				}
			case translate.ProtocolResponses:
				var env struct {
					Response struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(lastEventData(t, got, "response.failed")), &env); err != nil {
					t.Fatalf("response.failed 的 data 不是合法 JSON: %v", err)
				}
				message = env.Response.Error.Message
			case translate.ProtocolGemini:
				for _, data := range streamData(got) {
					var env struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(data), &env); err == nil && env.Error.Message != "" {
						message = env.Error.Message
					}
				}
			}
			if !strings.Contains(message, "***") {
				t.Fatalf("%s 的错误帧文案 = %q，期望带上脱敏后的上游原话", c.inbound, message)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("请求记录行数 = %d，期望 1", len(recs))
			}
			if recs[0].OK || strings.Contains(recs[0].Error, key) {
				t.Fatalf("请求记录 = %+v，期望 ok=0 且不含明文", recs[0])
			}
			if leaked := logs(); strings.Contains(leaked, key) {
				t.Fatalf("网关日志泄漏了凭据：%s", leaked)
			}
		})
	}
}
