package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// 这个文件钉住「模型别名接进转发与清单」（DESIGN 5.12）：别名只在请求方向生效，
// 解析只做一次，选号、路由 {model} 占位符、请求体里的 model 与 gemini 路径都用解析后的
// 真名，响应里的 model 仍是上游的真名；两个清单接口同时列出真名与别名。

// mustAlias 写一条别名，写不进去直接失败：用例的前提是库里已经有这条别名。
func mustAlias(t *testing.T, f *fixture, alias, target string) {
	t.Helper()
	if err := f.st.SetModelAlias(alias, target); err != nil {
		t.Fatalf("写别名 %s → %s: %v", alias, target, err)
	}
}

// aliasErrorBody 解出错误体的 type 与 message，用来断言 400/404 的文案。
func aliasErrorBody(t *testing.T, body string) (kind, message string) {
	t.Helper()
	_, errObj := decodeErrorBody(t, body)
	if err := json.Unmarshal(errObj["type"], &kind); err != nil {
		t.Fatalf("error.type 不是字符串: %v，原文 %s", err, body)
	}
	if err := json.Unmarshal(errObj["message"], &message); err != nil {
		t.Fatalf("error.message 不是字符串: %v，原文 %s", err, body)
	}
	return kind, message
}

// TestAliasOneHopUsesRealNameUpstream 钉住一跳命中：客户端用别名，上游收到真名，
// 选号按真名取账号，requests 两列一个记别名一个记真名。
func TestAliasOneHopUsesRealNameUpstream(t *testing.T) {
	f := newFixture(t)

	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatCompletion)
	}))
	t.Cleanup(up.Close)

	// 账号只声明真名：客户端写死的 claude-3-5-sonnet 由别名指过来。
	f.add(t, "glm1", gateway.ProtocolChat, up.URL, "sk-glm", []string{"glm-4.6"}, 100)
	mustAlias(t, f, "claude-3-5-sonnet", "glm-4.6")
	srv := f.server(t)

	status, body := postTo(t, srv, `{"model":"claude-3-5-sonnet","max_tokens":16,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"glm-4.6"`) {
		t.Fatalf("上游请求体里的模型名应是真名: %s", gotBody)
	}
	if strings.Contains(gotBody, "claude-3-5-sonnet") {
		t.Fatalf("别名不该发给上游: %s", gotBody)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d", len(recs))
	}
	if recs[0].ModelRequested != "claude-3-5-sonnet" || recs[0].ModelUsed != "glm-4.6" {
		t.Fatalf("请求记录 model_requested/model_used = %q/%q，期望别名与真名各一列",
			recs[0].ModelRequested, recs[0].ModelUsed)
	}
}

// TestAliasMultiHop 钉住多跳：别名 → 别名 → 真名，走的仍是同一条解析（最长 8 跳）。
func TestAliasMultiHop(t *testing.T) {
	f := newFixture(t)

	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	t.Cleanup(up.Close)

	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-ds", []string{"deepseek-v4"}, 100)
	mustAlias(t, f, "fast", "claude-3-5-sonnet")
	mustAlias(t, f, "claude-3-5-sonnet", "deepseek-v4")
	srv := f.server(t)

	status, body := postTo(t, srv, `{"model":"fast","max_tokens":16,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(gotBody, `"model":"deepseek-v4"`) {
		t.Fatalf("多跳之后应把真名发给上游: %s", gotBody)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].ModelRequested != "fast" || recs[0].ModelUsed != "deepseek-v4" {
		t.Fatalf("请求记录 = %+v", recs)
	}
}

// TestAliasCycleIsBadRequest 钉住别名链成环：这不是「清单里没有这个模型」，请求本身解不出
// 真名，按入站协议报 400，文案指出是哪条链坏了并给出下一步命令，且不写请求记录。
func TestAliasCycleIsBadRequest(t *testing.T) {
	f := newFixture(t)
	// 账号声明一个真名，两个别名互指成环：请求这两个名字都解不出真名。
	f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-ds", []string{"m1"}, 100)
	mustAlias(t, f, "a-alias", "b-alias")
	mustAlias(t, f, "b-alias", "a-alias")
	srv := f.server(t)

	status, body := postTo(t, srv, `{"model":"a-alias","max_tokens":16,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolAnthropic)
	if status != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400，响应 %s", status, body)
	}
	kind, msg := aliasErrorBody(t, body)
	if kind != "invalid_request_error" {
		t.Fatalf("error.type = %q，期望 invalid_request_error（按入站协议给形状）", kind)
	}
	if !strings.Contains(msg, "别名链成环") || !strings.Contains(msg, "a-alias → b-alias → a-alias") {
		t.Fatalf("文案应点出成环的那条链: %s", msg)
	}
	if !strings.Contains(msg, "switchboard model alias ls") {
		t.Fatalf("文案应给出下一步命令 switchboard model alias ls: %s", msg)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("解析失败的请求不该写请求记录: %+v", recs)
	}
}

// TestAliasTargetWithoutProvider 钉住 404 文案：别名解析得出的目标没有任何账号提供时，
// 消息要同时说出别名与目标，并提示 switchboard model ls，而不是只说「没有账号提供 X」。
func TestAliasTargetWithoutProvider(t *testing.T) {
	f := newFixture(t)
	f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-ds", []string{"m1"}, 100)
	// 目标可以暂时没有账号提供（DESIGN 5.12 的写入校验允许这样），此时别名接不了请求。
	mustAlias(t, f, "fast", "ghost-model")
	srv := f.server(t)

	status, body := postTo(t, srv, `{"model":"fast","max_tokens":16,`+
		`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolAnthropic)
	if status != http.StatusNotFound {
		t.Fatalf("状态 = %d，期望 404，响应 %s", status, body)
	}
	kind, msg := aliasErrorBody(t, body)
	if kind != "not_found_error" {
		t.Fatalf("error.type = %q，期望 not_found_error", kind)
	}
	const want = "别名 fast 指向 ghost-model，但没有账号提供 ghost-model；用 switchboard model ls 看现有清单"
	if msg != want {
		t.Fatalf("404 文案 = %q，期望 %q", msg, want)
	}
}

// TestAliasPassthroughKeepsRealModelInResponse 钉住响应不改名：同协议直通时上游响应按字节
// 转发，响应里的 model 是上游的真名；请求体里的 model 则是解析后的真名。
func TestAliasPassthroughKeepsRealModelInResponse(t *testing.T) {
	f := newFixture(t)

	upstreamBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":8,"output_tokens":2}}`
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(up.Close)

	f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL, "sk-ant", []string{"claude-x"}, 100)
	mustAlias(t, f, "claude-3-5-sonnet", "claude-x")
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"claude-3-5-sonnet","max_tokens":16,`+
		`"messages":[{"role":"user","content":"ping"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(gotBody, `"model":"claude-x"`) {
		t.Fatalf("直通请求体里的模型名应是真名: %s", gotBody)
	}
	if body != upstreamBody {
		t.Fatalf("直通响应被改动了（别名不该影响响应方向）:\n得到 %s\n期望 %s", body, upstreamBody)
	}
}

// TestAliasGeminiPath 钉住 gemini 的模型名在路径上：客户端用别名，上游路径里是真名。
func TestAliasGeminiPath(t *testing.T) {
	f := newFixture(t)

	var gotURI string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiReply)
	}))
	t.Cleanup(up.Close)

	if err := f.st.CreateProvider(store.Provider{Name: "gtest", BaseURL: up.URL},
		[]store.ProviderRoute{{Protocol: translate.ProtocolGemini, Path: geminiRoute}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	f.addAs(t, "g1", "gtest", translate.ProtocolGemini, up.URL, "sk-gemini", []string{"gemini-3.8-flash"})
	mustAlias(t, f, "gemini-pro", "gemini-3.8-flash")
	srv := f.server(t)

	status, body := postPathTo(t, srv, "/v1beta/models/gemini-pro:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if gotURI != "/v1beta/models/gemini-3.8-flash:generateContent" {
		t.Fatalf("上游 RequestURI = %q，期望路径里是解析后的真名", gotURI)
	}
}

// TestModelListIncludesAliases 钉住清单同时列出真名与别名：别名条目的 id 是别名、
// owned_by 是提供目标的厂商（多个时取厂商名字典序第一个）、created 仍是 0；
// 与真名重名只出现一次；解析不到目标的别名不出现。整份响应按字符串比对，
// 字段顺序、去重与升序都是契约的一部分。
func TestModelListIncludesAliases(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "z1", "zeta", gateway.ProtocolAnthropic, "http://127.0.0.1:1", "sk-z", []string{"shared"})
	f.addAs(t, "a1", "alpha", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-a", []string{"shared", "m-b"})
	mustAlias(t, f, "fast", "shared")
	// 目标没有任何启用账号提供：这个别名眼下接不了，不进清单。
	mustAlias(t, f, "ghost-alias", "ghost-model")
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	want := `{"object":"list","data":[` +
		`{"id":"fast","object":"model","created":0,"owned_by":"alpha"},` +
		`{"id":"m-b","object":"model","created":0,"owned_by":"alpha"},` +
		`{"id":"shared","object":"model","created":0,"owned_by":"alpha"}]}`
	if body != want {
		t.Fatalf("清单 = %s\n期望 %s", body, want)
	}
}

// TestModelByIDAlias 钉住单模型接口的两条路：id 是别名时返回同一条目（owned_by 取目标的厂商）；
// 别名解析不到目标时按 404 处理，文案说明这是一个别名、目标是谁。
func TestModelByIDAlias(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "a1", "alpha", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-a", []string{"shared"})
	mustAlias(t, f, "fast", "shared")
	mustAlias(t, f, "ghost-alias", "ghost-model")
	srv := f.server(t)

	t.Run("命中别名", func(t *testing.T) {
		status, body, _ := getJSON(t, srv.URL+"/v1/models/fast")
		if status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		want := `{"id":"fast","object":"model","created":0,"owned_by":"alpha"}`
		if body != want {
			t.Fatalf("条目 = %s，期望 %s", body, want)
		}
	})

	t.Run("别名解析不到目标", func(t *testing.T) {
		status, body, _ := getJSON(t, srv.URL+"/v1/models/ghost-alias")
		if status != http.StatusNotFound {
			t.Fatalf("状态 = %d，期望 404，响应 %s", status, body)
		}
		kind, msg := aliasErrorBody(t, body)
		if kind != "not_found_error" {
			t.Fatalf("error.type = %q，期望 not_found_error", kind)
		}
		if !strings.Contains(msg, "这是一个别名，目标是 ghost-model") {
			t.Fatalf("文案应说明这是一个别名与它的目标: %s", msg)
		}
		if !strings.Contains(msg, "switchboard model ls") {
			t.Fatalf("文案应给出下一步命令: %s", msg)
		}
	})
}
