package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"switchboard/internal/gateway"
)

// probeCall 是一次打到假上游的探活请求：方法、路径与两种凭据头。
// 「列表接口一次 GET 就够」这类断言要看得到方法，只看路径分不出两阶段。
type probeCall struct {
	method string
	path   string
	apiKey string
	bearer string
}

// probeUpstream 是按方法加路径回固定响应的假上游，认不出的请求回 404，
// 同时按顺序记下每一次调用。凭据头的形状也跟着路由的协议走，断言要能看到它。
type probeUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []probeCall
}

// probeReply 是一条预设响应：状态码与响应体原文。
type probeReply struct {
	status int
	body   string
}

func newProbeUpstream(t *testing.T, routes map[string]probeReply) *probeUpstream {
	t.Helper()
	up := &probeUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.calls = append(up.calls, probeCall{
			method: r.Method,
			path:   r.URL.Path,
			apiKey: r.Header.Get("x-api-key"),
			bearer: r.Header.Get("Authorization"),
		})
		up.mu.Unlock()

		resp, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"没有这个路由"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(up.srv.Close)
	return up
}

func (u *probeUpstream) all() []probeCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]probeCall, len(u.calls))
	copy(out, u.calls)
	return out
}

// mustCalls 断言这次探活正好打了 expected 次上游请求，返回它们。
// 顺序也要对：「列表接口一次 GET 就够」与「列表 404 之后再 POST」是两种不同的账。
func (u *probeUpstream) mustCalls(t *testing.T, expected int) []probeCall {
	t.Helper()
	calls := u.all()
	if len(calls) != expected {
		t.Fatalf("上游调用 = %+v，期望正好 %d 次", calls, expected)
	}
	return calls
}

// exhausted 把账号打成「上一次撞了 401」的状态：停用、带错误、留着连续失败次数。
// 探活成功要把这三样一起清掉，所以从这种状态起重跑才有意义。
func (f *fixture) exhausted(t *testing.T, name string) {
	t.Helper()
	acc, err := f.st.GetAccount(name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if err := f.st.SetEnabled(acc.ID, false, "上一个 key 被拒"); err != nil {
		t.Fatalf("停用账号: %v", err)
	}
	if err := f.st.RecordFailure(acc.ID, time.Now().Add(time.Minute).Unix(), "上一个 key 被拒"); err != nil {
		t.Fatalf("记失败: %v", err)
	}
}

func (f *fixture) mustAccount(t *testing.T, name string) (enabled bool, failStreak int, cooldown int64, lastError string) {
	t.Helper()
	acc, err := f.st.GetAccount(name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	return acc.Enabled, acc.FailStreak, acc.CooldownUntil, acc.LastError
}

// TestProbeModelsListIsEnough 钉住第一步就算结论：模型列表 2xx 说明凭据与连通性正常，
// 这一条不花 token，所以上游只该收到一次 GET，一条模型调用请求都不该收到。
func TestProbeModelsListIsEnough(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"GET /v1/models": {status: http.StatusOK, body: `{"data":[{"id":"m-b"},{"id":"m-a"},{"id":"m-a"}]}`},
	})

	// 自建地址（provider 不在厂商表里）：列表地址按 base_url 后缀猜成 <base>/v1/models。
	acc := f.addAs(t, "list1", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})
	f.exhausted(t, "list1")

	gs := newGateway(t, f)
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}

	calls := up.mustCalls(t, 1)
	if calls[0].method != http.MethodGet || calls[0].path != "/v1/models" {
		t.Fatalf("上游调用 = %+v，期望只有一次 GET /v1/models（列表接口不该顺带花一次模型调用）", calls)
	}
	if calls[0].bearer != "Bearer sk-test" {
		t.Fatalf("列表接口的凭据头 = %q，期望按取到地址的协议发 Bearer", calls[0].bearer)
	}
	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("探活结果 = %+v，期望 200 且 OK", res)
	}
	if res.Stage != gateway.ProbeStageModels {
		t.Fatalf("stage = %q，期望 %q", res.Stage, gateway.ProbeStageModels)
	}
	if res.Models != 2 || !res.ModelsKnown {
		t.Fatalf("上游模型数 = %d（known=%v），期望去重后的 2", res.Models, res.ModelsKnown)
	}

	// 2xx 的三个副作用：启用、清冷却与 fail_streak、清 last_error。
	enabled, streak, cooldown, lastErr := f.mustAccount(t, "list1")
	if !enabled || streak != 0 || cooldown != 0 || lastErr != "" {
		t.Fatalf("账号状态 = enabled:%v fail_streak:%d cooldown:%d last_error:%q，期望全部清干净",
			enabled, streak, cooldown, lastErr)
	}
}

// TestProbeFallsBackToCall 钉住「列表接口不可用时退回最小请求」：404 与 405 是这家没有列表接口，
// 403 可能只是网关按路径拒。三种都再发一条 max_tokens=1 的请求，结论记为 stage=call。
func TestProbeFallsBackToCall(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"没有列表接口 404", http.StatusNotFound},
		{"方法不允许 405", http.StatusMethodNotAllowed},
		{"网关拦了 403", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := newProbeUpstream(t, map[string]probeReply{
				"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
			})
			acc := f.addAs(t, "fb", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})
			f.exhausted(t, "fb")

			gs := newGateway(t, f)
			res, err := gs.Probe(context.Background(), acc)
			if err != nil {
				t.Fatalf("探活失败: %v", err)
			}

			calls := up.mustCalls(t, 2)
			if calls[0].method != http.MethodGet || calls[1].method != http.MethodPost {
				t.Fatalf("上游调用 = %+v，期望先 GET 列表再 POST 最小请求", calls)
			}
			if calls[1].path != "/v1/chat/completions" {
				t.Fatalf("最小请求路径 = %q，期望 /v1/chat/completions", calls[1].path)
			}
			if !res.OK || res.Stage != gateway.ProbeStageCall {
				t.Fatalf("探活结果 = %+v，期望 OK 且 stage=%s", res, gateway.ProbeStageCall)
			}
			if res.Models != 0 || res.ModelsKnown {
				t.Fatalf("上游模型数 = %d（known=%v），列表接口没通时应当是未知的 0", res.Models, res.ModelsKnown)
			}
			// 备注要写清结论出自模型调用，以及列表接口为什么不采用。
			for _, want := range []string{"模型调用", "列表接口返回"} {
				if !strings.Contains(res.Message, want) {
					t.Fatalf("备注 = %q，期望包含 %q", res.Message, want)
				}
			}
			enabled, streak, cooldown, _ := f.mustAccount(t, "fb")
			if !enabled || streak != 0 || cooldown != 0 {
				t.Fatalf("账号状态 = enabled:%v fail_streak:%d cooldown:%d，期望启用且清零", enabled, streak, cooldown)
			}
		})
	}
}

// TestProbeEmptyModelsKeepsNoteOnFailure 确认退回最小请求时也留着「用的是默认模型名」这条说明：
// 上游的错误文本说明这次为什么失败，说明解释的是哪个模型名，两条都要在。
func TestProbeEmptyModelsUsesFallbackModel(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/messages": {status: http.StatusOK, body: anthropicProbeOK},
	})

	// 模型列表为空：用例显式给 nil，与 `account add` 不给 --models 时的状态一致。
	acc := f.add(t, "empty1", gateway.ProtocolAnthropic, up.srv.URL, "sk-test", nil, 100)
	gs := newGateway(t, f)

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("探活结果 = %+v，期望 200 且 OK", res)
	}
	if res.Stage != gateway.ProbeStageCall {
		t.Fatalf("stage = %q，期望 %q", res.Stage, gateway.ProbeStageCall)
	}
	if !strings.Contains(res.Message, "还没有模型") || !strings.Contains(res.Message, "default") {
		t.Fatalf("备注 = %q，期望说明这个账号还没有模型、探活用默认模型名", res.Message)
	}
	calls := up.mustCalls(t, 2)
	if calls[1].path != "/v1/messages" || calls[1].apiKey != "sk-test" {
		t.Fatalf("最小请求 = %+v，期望按账号协议打 /v1/messages 并带 x-api-key", calls[1])
	}
}

func TestProbeEmptyModelsKeepsNoteOnFailure(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/messages": {status: http.StatusForbidden, body: `{"error":{"message":"套餐不含该模型"}}`},
	})

	acc := f.add(t, "empty2", gateway.ProtocolAnthropic, up.srv.URL, "sk-test", nil, 100)
	gs := newGateway(t, f)

	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if res.OK || res.Status != http.StatusForbidden {
		t.Fatalf("探活结果 = %+v，期望 403 且不 OK", res)
	}
	for _, want := range []string{"套餐不含该模型", "还没有模型", "模型调用"} {
		if !strings.Contains(res.Message, want) {
			t.Fatalf("备注 = %q，期望包含 %q", res.Message, want)
		}
	}
}

// TestProbeModelsListUnauthorized 确认列表接口自己报 401 就是凭据问题：停用账号、
// 把错误写进 last_error，不再花那一次模型调用去确认同一件事。
func TestProbeModelsListUnauthorized(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"GET /v1/models": {status: http.StatusUnauthorized, body: `{"error":{"message":"invalid api key"}}`},
	})
	acc := f.addAs(t, "auth", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})

	gs := newGateway(t, f)
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}

	up.mustCalls(t, 1)
	if res.OK || res.Status != http.StatusUnauthorized {
		t.Fatalf("探活结果 = %+v，期望 401 且不 OK", res)
	}
	if res.Stage != gateway.ProbeStageModels {
		t.Fatalf("stage = %q，期望 %q", res.Stage, gateway.ProbeStageModels)
	}
	enabled, _, _, lastErr := f.mustAccount(t, "auth")
	if enabled {
		t.Fatalf("401 之后账号应停用")
	}
	if !strings.Contains(lastErr, "invalid api key") || !strings.Contains(lastErr, "列表接口") {
		t.Fatalf("last_error = %q，期望带上游原话与是哪一步判的", lastErr)
	}
}

// TestProbeModelsListServerErrorCooldowns 确认列表接口的 429/5xx 走冷却：
// 按连续失败次数算时长、把账号记成一次失败，不再打模型调用。
func TestProbeModelsListServerErrorCooldowns(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"GET /v1/models": {status: http.StatusInternalServerError, body: `{"error":{"message":"upstream boom"}}`},
	})
	acc := f.addAs(t, "boom", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})

	gs := newGateway(t, f)
	before := time.Now()
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}

	up.mustCalls(t, 1)
	if res.OK || res.Status != http.StatusInternalServerError {
		t.Fatalf("探活结果 = %+v，期望 500 且不 OK", res)
	}
	enabled, streak, cooldown, lastErr := f.mustAccount(t, "boom")
	if !enabled {
		t.Fatalf("5xx 不该停用账号")
	}
	if streak != 1 {
		t.Fatalf("fail_streak = %d，期望 1", streak)
	}
	if want := before.Add(10 * time.Second).Unix(); cooldown < want-1 || cooldown > want+1 {
		t.Fatalf("cooldown_until = %d，期望首次 5xx 的 10 秒冷却（约 %d）", cooldown, want)
	}
	if !strings.Contains(lastErr, "upstream boom") {
		t.Fatalf("last_error = %q，期望带上游原话", lastErr)
	}
}

// TestProbeCallStageDecidesWhenBothFail 确认两阶段都失败时备注能区分阶段：
// 结论取模型调用那次（stage=call），列表接口那次的失败只作为说明留着。
func TestProbeCallStageDecidesWhenBothFail(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/messages": {status: http.StatusForbidden, body: `{"error":{"message":"套餐不含该模型"}}`},
	})
	acc := f.add(t, "both", gateway.ProtocolAnthropic, up.srv.URL, "sk-test", []string{"m1"}, 100)

	gs := newGateway(t, f)
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}

	calls := up.mustCalls(t, 2)
	if calls[0].method != http.MethodGet {
		t.Fatalf("第一次调用 = %+v，期望先 GET 列表", calls[0])
	}
	if res.Stage != gateway.ProbeStageCall || res.Status != http.StatusForbidden {
		t.Fatalf("探活结果 = %+v，期望 stage=call 且 403", res)
	}
	if !strings.Contains(res.Message, "模型调用返回 403") || !strings.Contains(res.Message, "列表接口返回 404") {
		t.Fatalf("备注 = %q，期望写清两阶段各自的结果", res.Message)
	}
	// 403 只报原话，不动状态。
	enabled, streak, cooldown, _ := f.mustAccount(t, "both")
	if !enabled || streak != 0 || cooldown != 0 {
		t.Fatalf("账号状态 = enabled:%v fail_streak:%d cooldown:%d，403 不该改状态", enabled, streak, cooldown)
	}
}

// TestProbeModelsListShapeUnknown 确认 2xx 但形状不是模型列表时不判失败：
// 凭据与连通性已经从状态码上看出来了，只是模型个数未知。
func TestProbeModelsListShapeUnknown(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"GET /v1/models": {status: http.StatusOK, body: `{"ok":true}`},
	})
	acc := f.addAs(t, "shape", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"})

	gs := newGateway(t, f)
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}

	up.mustCalls(t, 1)
	if !res.OK || res.Stage != gateway.ProbeStageModels {
		t.Fatalf("探活结果 = %+v，期望 2xx 判定为正常、stage=models", res)
	}
	if res.Models != 0 || res.ModelsKnown {
		t.Fatalf("上游模型数 = %d（known=%v），形状不对时个数应当是未知的 0", res.Models, res.ModelsKnown)
	}
	if !strings.Contains(res.Message, "不是模型列表") {
		t.Fatalf("备注 = %q，期望说明上游返回的不是模型列表", res.Message)
	}
}

// TestProbeEmptyModelsListCountsZero 确认显式的空列表数得出来：0 个模型与「不知道几个」要能分开。
func TestProbeEmptyModelsListCountsZero(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"GET /v1/models": {status: http.StatusOK, body: `{"data":[]}`},
	})
	acc := f.addAs(t, "zero", "custom", gateway.ProtocolChat, up.srv.URL, "sk-test", nil)

	gs := newGateway(t, f)
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if !res.OK || !res.ModelsKnown || res.Models != 0 {
		t.Fatalf("探活结果 = %+v，期望 2xx、模型数已知且为 0", res)
	}
}

// TestProbeNetworkErrorCooldowns 确认两阶段都连不上时结论取模型调用那次，并按转发路径的记法记 502：
// 超时与连不上都要打冷却，不能只报一句错就结束。
func TestProbeNetworkErrorCooldowns(t *testing.T) {
	f := newFixture(t)
	// 127.0.0.1:1 上没有服务，两条请求都是连不上。
	acc := f.addAs(t, "dead", "custom", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-test", []string{"m1"})

	gs := newGateway(t, f)
	before := time.Now()
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if res.OK || res.Status != http.StatusBadGateway {
		t.Fatalf("探活结果 = %+v，期望连不上记成 502", res)
	}
	if res.Stage != gateway.ProbeStageCall {
		t.Fatalf("stage = %q，期望结论取自模型调用", res.Stage)
	}
	for _, want := range []string{"模型调用失败", "列表接口不可用"} {
		if !strings.Contains(res.Message, want) {
			t.Fatalf("备注 = %q，期望包含 %q", res.Message, want)
		}
	}
	_, streak, cooldown, _ := f.mustAccount(t, "dead")
	if streak != 1 {
		t.Fatalf("fail_streak = %d，期望 1", streak)
	}
	if want := before.Add(10 * time.Second).Unix(); cooldown < want-1 || cooldown > want+1 {
		t.Fatalf("cooldown_until = %d，期望首次网络失败的 10 秒冷却（约 %d）", cooldown, want)
	}
}

// TestProbeNeverLeaksKey 确认两个阶段都不把凭据带进结论：上游把 key 回显进错误体，
// 结论里只该出现 ***，日志与可序列化到 `--json` 的结构里都不该有明文。
func TestProbeNeverLeaksKey(t *testing.T) {
	f := newFixture(t)
	// 运行期拼出来，避免测试文件里出现像个真 key 的字符串。
	secretValue := "sk-" + strings.Repeat("p", 24)

	var logMu sync.Mutex
	var logs []string
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Logf: func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, format)
	}})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// 上游把凭据回显到错误里：这条错误会进 last_error 与命令行输出。
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key=`+secretValue+`"}}`)
	}))
	defer up.Close()

	acc := f.addAs(t, "leak", "custom", gateway.ProtocolChat, up.URL, secretValue, []string{"m1"})
	res, err := gs.Probe(context.Background(), acc)
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if strings.Contains(res.Message, secretValue) {
		t.Fatalf("探活结论泄漏了凭据: %s", res.Message)
	}
	if !strings.Contains(res.Message, "***") {
		t.Fatalf("证据不足：错误文本里凭据应被替换成 ***，实际 %s", res.Message)
	}

	logMu.Lock()
	joined := strings.Join(logs, "\n")
	logMu.Unlock()
	if strings.Contains(joined, secretValue) {
		t.Fatalf("日志泄漏了凭据:\n%s", joined)
	}

	// last_error 是 --json 与 account ls 的来源，同样不能有明文。
	stored, err := f.st.GetAccount("leak")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if strings.Contains(stored.LastError, secretValue) {
		t.Fatalf("last_error 泄漏了凭据: %s", stored.LastError)
	}
	if strings.Contains(string(stored.KeyCipher), secretValue) {
		t.Fatalf("凭据以明文出现在数据库里的 key_cipher: %q", stored.KeyCipher)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("序列化探活结果: %v", err)
	}
	if strings.Contains(string(b), secretValue) {
		t.Fatalf("序列化后的探活结果泄漏了凭据: %s", b)
	}
}

// anthropicProbeOK 是一条合法的 anthropic 响应，探活用它。
const anthropicProbeOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`

// chatOK 是一条合法的 openai-chat 响应。
const chatOK = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
