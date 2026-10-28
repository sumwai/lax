package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
)

// upstreamCall 是一次探活打到假上游的痕迹：路径与两种凭据头。
type upstreamCall struct {
	path   string
	apiKey string
	bearer string
	body   string
}

// callRecorder 记下假上游收到的每一次请求。探活是并发调用的，所以要加锁。
type callRecorder struct {
	mu    sync.Mutex
	calls []upstreamCall
}

func (r *callRecorder) add(path, apiKey, bearer, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, upstreamCall{path: path, apiKey: apiKey, bearer: bearer, body: body})
}

func (r *callRecorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.path)
	}
	return out
}

func (r *callRecorder) all() []upstreamCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]upstreamCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// probeAnthropicOK 是一条合法的 anthropic 响应，分开写是为了让断言能整段比对。
const probeAnthropicOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`

// TestProbeModelDiscoversProtocol 确认探活能发现模型该用的协议：
// 账号协议那一次失败（404、403、400 指向端点三种各一例）、换协议之后成功，
// 结论是 usable，协议记成成功那个，上游收到的路径与凭据头都对。
func TestProbeModelDiscoversProtocol(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"路径不存在 404", http.StatusNotFound, `{"error":{"message":"no route for this path"}}`},
		{"按路径拒 403", http.StatusForbidden, `{"error":{"message":"claude-* 只接受 /messages"}}`},
		{"端点不支持 400", http.StatusBadRequest, `{"error":{"message":"this endpoint does not support the requested protocol"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			const key = "sk-ant-discover"
			rec := &callRecorder{}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
				if r.URL.Path != "/v1/messages" {
					w.WriteHeader(c.status)
					_, _ = io.WriteString(w, c.body)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, probeAnthropicOK)
			}))
			defer up.Close()

			acc := f.add(t, "claude1", gateway.ProtocolChat, up.URL, key, []string{"claude-x"}, 100)
			gs := newGateway(t, f)

			res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
			if err != nil {
				t.Fatalf("探活: %v", err)
			}
			if res.Status != store.ModelUsable {
				t.Fatalf("状态 = %q，期望 usable（账号协议失败后换协议成功）", res.Status)
			}
			if res.Protocol != gateway.ProtocolAnthropic {
				t.Fatalf("结论里的协议 = %q，期望 %q", res.Protocol, gateway.ProtocolAnthropic)
			}
			if res.HTTPStatus != http.StatusOK {
				t.Fatalf("HTTP 状态 = %d，期望成功那次的 200", res.HTTPStatus)
			}

			// 两次调用：账号协议一次、发现到的协议一次，没有更多。
			calls := rec.all()
			if len(calls) != 2 {
				t.Fatalf("上游收到 %d 次调用，期望 2 次：%v", len(calls), rec.paths())
			}
			if calls[0].path != "/v1/chat/completions" {
				t.Fatalf("第一次应打账号协议 chat 的路径，实际 %q", calls[0].path)
			}
			if calls[0].bearer != "Bearer "+key {
				t.Fatalf("第一次的凭据头应是 Bearer，实际 %q", calls[0].bearer)
			}
			if calls[1].path != "/v1/messages" || calls[1].apiKey != key {
				t.Fatalf("第二次应打 anthropic 路径并用 x-api-key，实际路径 %q，x-api-key %q", calls[1].path, calls[1].apiKey)
			}

			// 发现结果落库。
			rows, err := f.st.AccountModels(acc.ID)
			if err != nil {
				t.Fatalf("读模型行: %v", err)
			}
			if len(rows) != 1 || rows[0].Model != "claude-x" || rows[0].Protocol != gateway.ProtocolAnthropic {
				t.Fatalf("模型行的协议 = %+v，期望 claude-x → anthropic", rows)
			}

			// 判定那次记一行请求记录，协议对都用实际用的协议。
			recs, err := f.st.RecentRequests(10)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("请求记录数 = %d，期望 1（只记得出判定的那次）", len(recs))
			}
			if recs[0].Inbound != gateway.ProtocolAnthropic || recs[0].Outbound != gateway.ProtocolAnthropic {
				t.Fatalf("探活行的协议对 = %s→%s，期望 anthropic→anthropic", recs[0].Inbound, recs[0].Outbound)
			}
			if recs[0].InTokens != 3 || recs[0].OutTokens != 1 {
				t.Fatalf("token = %d/%d，期望 3/1", recs[0].InTokens, recs[0].OutTokens)
			}
		})
	}
}

// TestProbeModelPrimaryProtocolSucceedsOnce 确认账号协议直接成功时只花一次调用，
// 并把这次用的协议记下来。
func TestProbeModelPrimaryProtocolSucceedsOnce(t *testing.T) {
	f := newFixture(t)
	const key = "sk-chat-ok"
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelUsable || res.Protocol != gateway.ProtocolChat {
		t.Fatalf("结论 = %+v，期望 usable/chat", res)
	}
	if paths := rec.paths(); len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %v，期望只打一次账号协议的路径", paths)
	}

	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != gateway.ProtocolChat {
		t.Fatalf("模型行 = %+v，期望协议记 chat", rows)
	}
}

// TestProbeModelUnauthorizedSkipsDiscovery 确认 401 不换协议：只打一次，
// 账号被停用，也不写协议。
func TestProbeModelUnauthorizedSkipsDiscovery(t *testing.T) {
	f := newFixture(t)
	const key = "sk-bad-cred"
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelAuthFailed {
		t.Fatalf("状态 = %q，期望 auth", res.Status)
	}
	if paths := rec.paths(); len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %v，401 之后不该再换协议", paths)
	}

	got, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if got.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("401 不该写入协议，实际 %+v", rows)
	}
}

// TestProbeModelAllProtocolsFailKeepsAccountVerdict 确认三种协议都失败时，
// 结论取账号协议那次，协议列保持空（继承），只记一行请求记录。
func TestProbeModelAllProtocolsFailKeepsAccountVerdict(t *testing.T) {
	f := newFixture(t)
	const key = "sk-nope"
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelNotFound || res.HTTPStatus != http.StatusNotFound {
		t.Fatalf("结论 = %+v，期望账号协议那次的 notfound/404", res)
	}
	if res.Protocol != gateway.ProtocolChat {
		t.Fatalf("结论里的协议 = %q，期望账号协议 chat", res.Protocol)
	}
	paths := rec.paths()
	if len(paths) != 3 {
		t.Fatalf("三种协议都应试一遍，实际 %v", paths)
	}
	want := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("第 %d 次调用路径 = %q，期望 %q（账号协议优先，其余按固定顺序）", i+1, paths[i], p)
		}
	}

	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("全失败时不该凭空写协议，实际 %+v", rows)
	}
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].HTTPStatus != http.StatusNotFound || recs[0].Outbound != gateway.ProtocolChat {
		t.Fatalf("请求记录 = %+v，期望一行 404 且出站是 chat", recs)
	}
}

// TestProbeModelCandidatesListIsInput 确认候选协议列表是入参：
// 只给两种时不会去打第三种协议的路径（以后按厂商声明收紧就靠这个）。
func TestProbeModelCandidatesListIsInput(t *testing.T) {
	f := newFixture(t)
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-two", []string{"m1"}, 100)
	gs := newGateway(t, f)

	if _, err := gs.ProbeModelWithProtocols(context.Background(), acc, "m1",
		[]string{gateway.ProtocolAnthropic, gateway.ProtocolChat}); err != nil {
		t.Fatalf("探活: %v", err)
	}
	paths := rec.paths()
	if len(paths) != 2 || paths[0] != "/v1/chat/completions" || paths[1] != "/v1/messages" {
		t.Fatalf("上游调用 = %v，期望只试候选里的两种协议", paths)
	}
}

// TestForwardUsesModelProtocol 确认转发按模型的有效协议走：
// 同一账号下 claude-x 用 anthropic 直通（字节不改），gpt-y 没有行、继承账号的 chat 走转换。
func TestForwardUsesModelProtocol(t *testing.T) {
	f := newFixture(t)
	const key = "sk-mixed"
	anthropicUpstream := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","unknown_field":"保留","usage":{"input_tokens":3,"output_tokens":1}}`
	chatUpstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`

	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			if r.Header.Get("x-api-key") != key {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"anthropic 路径要 x-api-key"}}`)
				return
			}
			_, _ = io.WriteString(w, anthropicUpstream)
		case "/v1/chat/completions":
			if r.Header.Get("Authorization") != "Bearer "+key {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"chat 路径要 Bearer"}}`)
				return
			}
			_, _ = io.WriteString(w, chatUpstream)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
		}
	}))
	defer up.Close()

	acc := f.add(t, "cc", gateway.ProtocolChat, up.URL, key, []string{"claude-x", "gpt-y"}, 100)
	// 只有 claude-x 有协议发现结果；gpt-y 没有行，按继承算。
	if err := f.st.SetModelProtocol(acc.ID, "claude-x", gateway.ProtocolAnthropic); err != nil {
		t.Fatalf("写协议发现结果: %v", err)
	}
	srv := f.server(t)

	// 入站 anthropic + 有效协议 anthropic：直通，响应字节原样，请求体只改 model。
	status, body := postMessages(t, srv, `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"unknown_field":1}`)
	if status != http.StatusOK {
		t.Fatalf("claude-x 状态 = %d，响应 %s", status, body)
	}
	if body != anthropicUpstream {
		t.Fatalf("直通响应被改动了:\n得到 %s\n期望 %s", body, anthropicUpstream)
	}

	// 入站 anthropic + 有效协议 chat：转换路径，响应编回 anthropic 形状。
	status, body = postMessages(t, srv, `{"model":"gpt-y","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("gpt-y 状态 = %d，响应 %s", status, body)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("gpt-y 的响应不是 anthropic 格式: %v，原文 %s", err, body)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "pong" {
		t.Fatalf("gpt-y 的 content = %+v", out.Content)
	}

	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("上游调用数 = %d，期望 2：%v", len(calls), rec.paths())
	}
	if calls[0].path != "/v1/messages" || calls[0].apiKey != key {
		t.Fatalf("claude-x 应打 /v1/messages 并带 x-api-key，实际路径 %q，x-api-key %q", calls[0].path, calls[0].apiKey)
	}
	if !strings.Contains(calls[0].body, `"unknown_field":1`) {
		t.Fatalf("直通应原样带陌生字段: %s", calls[0].body)
	}
	if calls[1].path != "/v1/chat/completions" || calls[1].bearer != "Bearer "+key {
		t.Fatalf("gpt-y 应打 chat 路径并带 Bearer，实际路径 %q，Authorization %q", calls[1].path, calls[1].bearer)
	}

	// 请求记录里的出站协议按模型算。
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	byModel := map[string]store.RequestRecord{}
	for _, r := range recs {
		byModel[r.ModelRequested] = r
	}
	if got := byModel["claude-x"]; got.Outbound != gateway.ProtocolAnthropic {
		t.Fatalf("claude-x 的出站协议 = %q，期望 anthropic", got.Outbound)
	}
	if got := byModel["gpt-y"]; got.Outbound != gateway.ProtocolChat {
		t.Fatalf("gpt-y 的出站协议 = %q，期望继承账号的 openai-chat", got.Outbound)
	}
}

// TestProbeNeverLeaksKeyInLogsAndRecords 确认探活换协议的过程中，凭据不进日志、
// 不进请求记录、也不进任何可序列化到 --json 的结构。
func TestProbeNeverLeaksKeyInLogsAndRecords(t *testing.T) {
	f := newFixture(t)
	const key = "sk-leak-canary-discover"

	var logMu sync.Mutex
	var logs []string
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Logf: func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}

	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.WriteHeader(http.StatusBadRequest)
		// 上游把凭据回显到错误里，还带上「端点不支持」的措辞触发换协议。
		_, _ = io.WriteString(w, `{"error":{"message":"endpoint does not support this protocol, key=`+key+`"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if strings.Contains(res.Message, key) {
		t.Fatalf("探活结论泄漏了凭据: %s", res.Message)
	}
	if !strings.Contains(res.Message, "***") {
		t.Fatalf("证据不足：错误文本里凭据应被替换成 ***，实际 %s", res.Message)
	}

	logMu.Lock()
	joined := strings.Join(logs, "\n")
	logMu.Unlock()
	if strings.Contains(joined, key) {
		t.Fatalf("日志泄漏了凭据:\n%s", joined)
	}

	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d，期望 1", len(recs))
	}
	if strings.Contains(recs[0].Error, key) {
		t.Fatalf("请求记录泄漏了凭据: %+v", recs[0])
	}

	// 库里只有密文：account 行解出来的字节里不该出现明文。
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if strings.Contains(string(stored.KeyCipher), key) {
		t.Fatalf("凭据以明文出现在数据库里的 key_cipher: %q", stored.KeyCipher)
	}

	// --json 的来源就是这些结构，序列化后同样不能出现明文。
	for _, v := range []any{rows, recs, res} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化: %v", err)
		}
		if strings.Contains(string(b), key) {
			t.Fatalf("序列化输出泄漏了凭据: %s", b)
		}
	}
}
