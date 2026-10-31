package cli_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recorderCall 是一次打到假上游的请求：方法、完整地址、路径与两种凭据头。
// url 是「scheme://host + 路径」的完整地址，取值与 httptest 的 srv.URL 同形：
// 只看 path 看不出 base_url 那一半拼得对不对（多一个斜杠、前缀被吃掉都藏在前面），
// 规整相关的断言要比的是整条地址。
type recorderCall struct {
	method  string
	url     string
	path    string
	apiKey  string
	bearer  string
	googKey string // gemini 的凭据头（x-goog-api-key），与前两种分开记
	// body 是请求体原文：systemone 的 state 与 questions 原样转发，用例要能看见它。
	body string
}

// recorderUpstream 是按方法加路径回固定响应的假上游：用例只声明「这条请求该回什么」，
// 认不出的请求回 404，同时记下每一次调用。路径与凭据头的断言都读这份记录。
// 不用 cli_test.go 里的 fakeUpstream，是因为它只认一种凭据头、只回 chat 响应，
// 而且不记录路径：按厂商路由转发与探活顺序这两类断言都需要看到实际打的地址。
type recorderUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []recorderCall
}

// fakeResponse 是一条预设的响应：状态码与响应体原文。
type fakeResponse struct {
	status int
	body   string
}

// newRecorderUpstream 启动假上游，t.Cleanup 里关掉。
func newRecorderUpstream(t *testing.T, routes map[string]fakeResponse) *recorderUpstream {
	t.Helper()
	up := &recorderUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.calls = append(up.calls, recorderCall{
			method:  r.Method,
			url:     "http://" + r.Host + r.URL.Path,
			path:    r.URL.Path,
			apiKey:  r.Header.Get("x-api-key"),
			bearer:  r.Header.Get("Authorization"),
			googKey: r.Header.Get("x-goog-api-key"),
			body:    string(body),
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

// all 返回到目前为止的全部调用。
func (u *recorderUpstream) all() []recorderCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]recorderCall, len(u.calls))
	copy(out, u.calls)
	return out
}

// postCalls 只返回 POST 的调用，模型同步那几条 GET 不算进来：
// 同步是一次独立的上游请求，混进来会让「探活打了几次」的断言从 2 变成 3。
func (u *recorderUpstream) postCalls() []recorderCall {
	var out []recorderCall
	for _, c := range u.all() {
		if c.method == http.MethodPost {
			out = append(out, c)
		}
	}
	return out
}

// postPaths 是 postCalls 的路径视图。
func (u *recorderUpstream) postPaths() []string {
	calls := u.postCalls()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.path)
	}
	return out
}

// mustSingleCall 取这次唯一的一次上游调用：用例一次只发一条命令，多打或没打都是错。
// 断言顺序是「正好一次」而不是「至少一次」：多打一次就是把同一笔钱记了两次。
func (u *recorderUpstream) mustSingleCall(t *testing.T) recorderCall {
	t.Helper()
	calls := u.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用 = %+v，期望正好一次", calls)
	}
	return calls[0]
}

// reset 清空记录，用来把「首轮」与「重探」的调用分开断言。
func (u *recorderUpstream) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = nil
}

// chatCompletionOK 是一条合法的 openai-chat 响应。
const chatCompletionOK = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`

// anthropicMessageOK 是一条合法的 anthropic 响应，探活与转发都用它。
const anthropicMessageOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`

// modelsListM1 是只有一个模型的模型列表响应。
const modelsListM1 = `{"data":[{"id":"m1"}]}`
