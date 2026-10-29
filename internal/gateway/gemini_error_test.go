package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switchboard/internal/gateway"
	"switchboard/internal/translate"
)

// 这个文件钉住 gemini 账号在上游出错时的行为：429/5xx 进冷却并换下一个号，
// 401 不换号、停用账号并立即返回，全部候选失败给 503；被安全策略截断（SAFETY）不算转发失败。
// 入站一律是 gemini 客户端，出站也一律是 gemini 账号，断言的是客户端拿到的字节与 requests 记账。

// geminiErrorBody 是 gemini 的错误体形状：{"error":{"code","message","status"}}。
func geminiErrorBody(code int, status, message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": code, "message": message, "status": status,
	}})
	return string(b)
}

// geminiStub 是一台说 gemini 的假上游：按预设状态与正文回答，记下被调用的次数。
type geminiStub struct {
	url    string
	hits   int
	status int
	body   string
}

func newGeminiStub(t *testing.T, status int, body string) *geminiStub {
	t.Helper()
	s := &geminiStub{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.body)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// TestGeminiFailoverOnRetryableStatus 钉住 429/5xx 两条可重试路径：撞上的账号按状态进冷却
// （429 一分钟、5xx 十秒），请求换到下一个候选并成功，客户端拿到第二个账号的 gemini 响应；
// requests 记两行，第一行 ok=0 带上游原话，第二行 ok=1。
func TestGeminiFailoverOnRetryableStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		statusText string
		// 冷却时长按状态算：429 是一分钟，5xx 是十秒。
		minCooldown, maxCooldown time.Duration
	}{
		{"配额用完 429", http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", time.Minute, time.Minute},
		{"上游 500", http.StatusInternalServerError, "INTERNAL", 10 * time.Second, 10 * time.Second},
		{"上游 503", http.StatusServiceUnavailable, "UNAVAILABLE", 10 * time.Second, 10 * time.Second},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			limited := newGeminiStub(t, c.status, geminiErrorBody(c.status, c.statusText, "上游说慢一点"))
			healthy := newGeminiStub(t, http.StatusOK, geminiReply)

			first := f.add(t, "limited", translate.ProtocolGemini, limited.url, "sk-gemini-1",
				[]string{"gemini-2.5-flash"}, 100)
			f.add(t, "healthy", translate.ProtocolGemini, healthy.url, "sk-gemini-2",
				[]string{"gemini-2.5-flash"}, 200)
			srv := f.server(t)

			status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:generateContent",
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, body)
			}
			if body != geminiReply {
				t.Fatalf("客户端应当拿到第二个账号的 gemini 响应:\n得到 %s\n期望 %s", body, geminiReply)
			}
			if limited.hits != 1 || healthy.hits != 1 {
				t.Fatalf("上游调用次数 = %d/%d，期望各一次", limited.hits, healthy.hits)
			}

			// 撞上的账号进了冷却，冷却时长按状态算，失败计数记一次。
			got, err := f.st.GetAccount(first.Name)
			if err != nil {
				t.Fatalf("读账号: %v", err)
			}
			if got.CooldownUntil == 0 {
				t.Fatalf("被上游拒绝的账号没有进冷却: %+v", got)
			}
			remaining := time.Until(time.Unix(got.CooldownUntil, 0))
			if remaining > c.minCooldown+time.Second || remaining < c.minCooldown-5*time.Second {
				t.Fatalf("冷却剩余 = %v，期望约 %v", remaining, c.minCooldown)
			}
			if got.FailStreak != 1 {
				t.Fatalf("连续失败次数 = %d，期望 1", got.FailStreak)
			}
			if !strings.Contains(got.LastError, "上游说慢一点") {
				t.Fatalf("账号的失败说明 = %q，期望带上上游原话", got.LastError)
			}

			// 换到的账号不该留下任何失败痕迹。
			ok, err := f.st.GetAccount("healthy")
			if err != nil {
				t.Fatalf("读账号: %v", err)
			}
			if ok.CooldownUntil != 0 || ok.FailStreak != 0 || !ok.Enabled {
				t.Fatalf("成功的账号不该被惩罚: %+v", ok)
			}

			recs, err := f.st.RecentRequests(10)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 2 {
				t.Fatalf("请求记录数 = %d，期望 2（失败一次 + 成功一次）: %+v", len(recs), recs)
			}
			var failed, succeeded bool
			for _, rec := range recs {
				if rec.Inbound != translate.ProtocolGemini || rec.Outbound != translate.ProtocolGemini {
					t.Fatalf("记录的协议 = %s→%s", rec.Inbound, rec.Outbound)
				}
				switch {
				case rec.OK:
					succeeded = true
					if rec.InTokens != 12 || rec.OutTokens != 7 {
						t.Fatalf("成功那次的用量 = %d/%d，期望 12/7", rec.InTokens, rec.OutTokens)
					}
				default:
					failed = true
					if rec.HTTPStatus != c.status {
						t.Fatalf("失败那次的 http_status = %d，期望 %d", rec.HTTPStatus, c.status)
					}
					if !strings.Contains(rec.Error, "上游说慢一点") {
						t.Fatalf("失败那次的 error = %q，期望带上上游原话", rec.Error)
					}
				}
			}
			if !failed || !succeeded {
				t.Fatalf("期望一行失败一行成功，实际 %+v", recs)
			}
		})
	}
}

// TestGeminiUnauthorizedDisablesAccountWithoutFailover 钉住 401：凭据问题与路径、状态无关，
// 停用这个账号并立即返回 401，不拿同一份坏凭据去换下一个号；客户端拿到 authentication_error。
func TestGeminiUnauthorizedDisablesAccountWithoutFailover(t *testing.T) {
	f := newFixture(t)
	denied := newGeminiStub(t, http.StatusUnauthorized,
		geminiErrorBody(http.StatusUnauthorized, "UNAUTHENTICATED", "API key not valid. Please pass a valid API key."))
	healthy := newGeminiStub(t, http.StatusOK, geminiReply)

	bad := f.add(t, "bad", translate.ProtocolGemini, denied.url, "sk-gemini-bad",
		[]string{"gemini-2.5-flash"}, 100)
	f.add(t, "healthy", translate.ProtocolGemini, healthy.url, "sk-gemini-good",
		[]string{"gemini-2.5-flash"}, 200)
	srv := f.server(t)

	status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusUnauthorized {
		t.Fatalf("状态 = %d，期望 401，响应 %s", status, body)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("错误体不是 JSON: %v，原文 %s", err, body)
	}
	if out.Error.Type != "authentication_error" {
		t.Fatalf("错误类型 = %q，期望 authentication_error", out.Error.Type)
	}
	for _, want := range []string{"bad", "已停用", "API key not valid"} {
		if !strings.Contains(out.Error.Message, want) {
			t.Fatalf("错误文案缺 %q: %s", want, out.Error.Message)
		}
	}

	// 401 不换号：第二个账号一次都不该被调用。
	if healthy.hits != 0 {
		t.Fatalf("401 之后不该换号，第二个上游收到 %d 次请求", healthy.hits)
	}

	got, err := f.st.GetAccount(bad.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if got.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	if !strings.Contains(got.LastError, "API key not valid") {
		t.Fatalf("停用原因 = %q，期望带上上游原话", got.LastError)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].OK || recs[0].HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("请求记录 = %+v，期望一行 401 且 ok=0", recs)
	}
}

// TestGeminiAllCandidatesFailReturns503 钉住候选全部失败：每个可重试的候选都试过一遍，
// 客户端拿到 503 与最后一家的上游原话，requests 里每一家各留一行 ok=0。
func TestGeminiAllCandidatesFailReturns503(t *testing.T) {
	f := newFixture(t)
	first := newGeminiStub(t, http.StatusTooManyRequests,
		geminiErrorBody(http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "第一个账号配额用完"))
	second := newGeminiStub(t, http.StatusServiceUnavailable,
		geminiErrorBody(http.StatusServiceUnavailable, "UNAVAILABLE", "第二个账号在维护"))

	f.add(t, "a1", translate.ProtocolGemini, first.url, "sk-gemini-1",
		[]string{"gemini-2.5-flash"}, 100)
	f.add(t, "a2", translate.ProtocolGemini, second.url, "sk-gemini-2",
		[]string{"gemini-2.5-flash"}, 200)
	srv := f.server(t)

	status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503，响应 %s", status, body)
	}
	for _, want := range []string{"所有候选账号都失败", "第二个账号在维护"} {
		if !strings.Contains(body, want) {
			t.Fatalf("503 的文案缺 %q: %s", want, body)
		}
	}
	if first.hits != 1 || second.hits != 1 {
		t.Fatalf("上游调用次数 = %d/%d，期望两个候选各一次", first.hits, second.hits)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("请求记录数 = %d，期望 2: %+v", len(recs), recs)
	}
	for _, rec := range recs {
		if rec.OK {
			t.Fatalf("两家都失败，不该有 ok=1 的记录: %+v", rec)
		}
	}
}

// TestGeminiSafetyEndShape 钉住 finishReason=SAFETY 的两种去向：
// 直通给 gemini 客户端时字节与上游一致（结束原因是 SAFETY，直通不改报文）；
// 转换给 anthropic 客户端时结束原因是 end_turn、原值只进日志，两种都记 ok=1 且 error 为空。
func TestGeminiSafetyEndShape(t *testing.T) {
	const safetyReply = `{"candidates":[{"content":{"role":"model","parts":[{"text":"我不能回答"}]},` +
		`"finishReason":"SAFETY","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,` +
		`"totalTokenCount":8},"modelVersion":"gemini-2.5-flash"}`

	t.Run("直通给 gemini 客户端", func(t *testing.T) {
		f := newFixture(t)
		up := newGeminiStub(t, http.StatusOK, safetyReply)
		f.add(t, "g1", translate.ProtocolGemini, up.url, "sk-gemini-safety",
			[]string{"gemini-2.5-flash"}, 100)
		srv := f.server(t)

		status, body := postPathTo(t, srv, "/v1beta/models/gemini-2.5-flash:generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, translate.ProtocolGemini)
		if status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		// 直通不改字节：客户端看到的结束原因仍是 SAFETY，由它自己决定怎么显示。
		if body != safetyReply {
			t.Fatalf("直通改动了响应字节:\n得到 %s\n期望 %s", body, safetyReply)
		}

		recs, err := f.st.RecentRequests(5)
		if err != nil {
			t.Fatalf("读请求记录: %v", err)
		}
		if len(recs) != 1 || !recs[0].OK || recs[0].Error != "" {
			t.Fatalf("请求记录 = %+v，期望一行 ok=1 且没有错误摘要", recs)
		}
		if recs[0].InTokens != 5 || recs[0].OutTokens != 3 {
			t.Fatalf("请求记录的用量 = %d/%d，期望 5/3", recs[0].InTokens, recs[0].OutTokens)
		}
	})

	t.Run("转换给 anthropic 客户端", func(t *testing.T) {
		f := newFixture(t)
		up := newGeminiStub(t, http.StatusOK, safetyReply)
		f.add(t, "g1", translate.ProtocolGemini, up.url, "sk-gemini-safety2",
			[]string{"gemini-2.5-flash"}, 100)
		gs, logs := newGatewayWith(t, f, gateway.Options{})
		srv := httptest.NewServer(gs.Handler())
		t.Cleanup(srv.Close)

		status, body := postTo(t, srv, `{"model":"gemini-2.5-flash","max_tokens":16,`+
			`"messages":[{"role":"user","content":"hi"}]}`, translate.ProtocolAnthropic)
		if status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		var out translate.AnthropicResponse
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("响应不是 anthropic 形状: %v，原文 %s", err, body)
		}
		if out.StopReason != "end_turn" {
			t.Fatalf("stop_reason = %q，期望 end_turn（SAFETY 不冒充工具调用，也不编新类型）", out.StopReason)
		}
		if len(out.Content) != 1 || out.Content[0].Text != "我不能回答" {
			t.Fatalf("content = %+v，期望上游那段正文", out.Content)
		}
		if !strings.Contains(logs(), "SAFETY") {
			t.Fatalf("原值应当进日志当备注: %s", logs())
		}

		recs, err := f.st.RecentRequests(5)
		if err != nil {
			t.Fatalf("读请求记录: %v", err)
		}
		if len(recs) != 1 || !recs[0].OK || recs[0].Error != "" {
			t.Fatalf("请求记录 = %+v，期望一行 ok=1 且没有错误摘要（原值不进 error 列）", recs)
		}
		if recs[0].Inbound != translate.ProtocolAnthropic || recs[0].Outbound != translate.ProtocolGemini {
			t.Fatalf("记录的协议 = %s→%s", recs[0].Inbound, recs[0].Outbound)
		}
	})
}
