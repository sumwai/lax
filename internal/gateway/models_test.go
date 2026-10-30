package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// newGateway 构造直连上游的网关实例，模型同步与探活都走它。
func newGateway(t *testing.T, f *fixture) *gateway.Server {
	t.Helper()
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	return gs
}

func TestModelsEndpoint(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://api.deepseek.com", "https://api.deepseek.com/v1/models"},
		{"https://api.deepseek.com/", "https://api.deepseek.com/v1/models"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1/models"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1/models"},
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/models"},
	}
	for _, c := range cases {
		if got := gateway.ModelsEndpoint(c.base); got != c.want {
			t.Fatalf("ModelsEndpoint(%q) = %q，期望 %q", c.base, got, c.want)
		}
	}
}

func TestSyncModelsSortsAndDedupes(t *testing.T) {
	f := newFixture(t)

	var gotPath, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"m-b"},
			{"id":"m-a"},
			{"id":"m-a"},
			{"name":"m-c"},
			{"id":"  "},
			{"id":" m-d "}
		]}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", nil, 100)
	gs := newGateway(t, f)

	models, err := gs.SyncModels(context.Background(), acc)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	if got, want := strings.Join(models, ","), "m-a,m-b,m-c,m-d"; got != want {
		t.Fatalf("模型 = %q，期望 %q（排序、去重、去空白）", got, want)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("上游路径 = %q，期望 /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("上游 Authorization = %q，期望解出凭据后转发", gotAuth)
	}
}

func TestSyncModelsBaseWithV1IsNotDuplicated(t *testing.T) {
	f := newFixture(t)

	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-x"}]}`)
	}))
	defer up.Close()

	acc := f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL+"/v1", "sk-ant-test", nil, 100)
	gs := newGateway(t, f)

	models, err := gs.SyncModels(context.Background(), acc)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	if len(models) != 1 || models[0] != "claude-x" {
		t.Fatalf("模型 = %v", models)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("上游路径 = %q，base 带 /v1 时不应拼成 /v1/v1/models", gotPath)
	}
}

func TestSyncModelsUnauthorized(t *testing.T) {
	f := newFixture(t)

	const key = "sk-sync-denied"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key `+key+`"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, nil, 100)
	gs := newGateway(t, f)

	_, err := gs.SyncModels(context.Background(), acc)
	if err == nil {
		t.Fatal("上游 401 时应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Fatalf("错误应带上状态码: %v", err)
	}
	if strings.Contains(msg, key) {
		t.Fatalf("错误文本泄漏了凭据: %v", err)
	}
}

func TestSyncModelsEmptyListIsError(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", nil, 100)
	gs := newGateway(t, f)

	models, err := gs.SyncModels(context.Background(), acc)
	if err == nil {
		t.Fatalf("空列表应算错误，得到 %v", models)
	}
	if len(models) != 0 {
		t.Fatalf("出错时不应返回模型: %v", models)
	}
	if !strings.Contains(err.Error(), "base-url") {
		t.Fatalf("错误应提示检查 base-url: %v", err)
	}
}

// TestProbeModelStatuses 钉住探活结论的两栏：Access 是正文（写 store.SetAccountModelAccess
// 的那个取值），Status 是旧词表，两栏必须一起对——只对一栏就会出现「结论一个可用、写回另一个」。
func TestProbeModelStatuses(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       string
		wantAccess string
		want       string
	}{
		{"可用", http.StatusOK, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"p"}],"usage":{"input_tokens":1,"output_tokens":1}}`, store.AccessAllowed, store.ModelUsable},
		{"无权", http.StatusForbidden, `{"error":{"message":"MODEL_NOT_IN_PLAN"}}`, store.AccessForbidden, store.ModelForbidden},
		{"不存在", http.StatusNotFound, `{"error":{"message":"no such model"}}`, store.AccessMissing, store.ModelNotFound},
		{"四百且提到模型", http.StatusBadRequest, `{"error":{"message":"model not found"}}`, store.AccessMissing, store.ModelNotFound},
		{"四百但无关模型", http.StatusBadRequest, `{"error":{"message":"max_tokens 太小"}}`, store.AccessError, store.ModelError},
		{"四百但只是没有可用上游", http.StatusBadRequest, `{"error":{"message":"No available providers match the 'only' filter: fireworks"}}`, store.AccessError, store.ModelError},
		{"上游故障", http.StatusInternalServerError, `{"error":{"message":"上游挂了"}}`, store.AccessError, store.ModelError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.httpStatus)
				_, _ = io.WriteString(w, c.body)
			}))
			defer up.Close()

			acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
			gs := newGateway(t, f)

			res, err := gs.ProbeModel(context.Background(), acc, "m1")
			if err != nil {
				t.Fatalf("探活: %v", err)
			}
			if res.Access != c.wantAccess {
				t.Fatalf("可达性 = %q，期望 %q（HTTP %d）", res.Access, c.wantAccess, c.httpStatus)
			}
			if res.Status != c.want {
				t.Fatalf("状态 = %q，期望 %q（HTTP %d）", res.Status, c.want, c.httpStatus)
			}
			if res.Model != "m1" {
				t.Fatalf("模型 = %q", res.Model)
			}
			if res.HTTPStatus != c.httpStatus {
				t.Fatalf("HTTP 状态 = %d，期望 %d", res.HTTPStatus, c.httpStatus)
			}
			if c.httpStatus != http.StatusOK && res.Message == "" {
				t.Fatal("非 2xx 应带上游错误文本")
			}
		})
	}
}

func TestProbeModelUnauthorizedDisablesAccount(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-bad-key", []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelAuthFailed {
		t.Fatalf("状态 = %q，期望 %q", res.Status, store.ModelAuthFailed)
	}

	got, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if got.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	if got.LastError == "" {
		t.Fatal("停用时应记下上游错误，便于排查")
	}
}

// 探活的花费由请求体里的 max_tokens 决定，必须钉死为 1。
func TestProbeModelSendsMaxTokensOne(t *testing.T) {
	f := newFixture(t)

	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"p"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	gs := newGateway(t, f)

	if _, err := gs.ProbeModel(context.Background(), acc, "m1"); err != nil {
		t.Fatalf("探活: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("上游 Authorization = %q", gotAuth)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v，原文 %s", err, gotBody)
	}
	if v, ok := body["max_tokens"].(float64); !ok || v != 1 {
		t.Fatalf("max_tokens = %v，探活必须只花 1 个输出 token", body["max_tokens"])
	}
	if body["model"] != "m1" {
		t.Fatalf("model = %v", body["model"])
	}
}

// 同协议的探活也走 max_tokens=1，且用 x-api-key 而不是 Bearer。
func TestProbeModelAnthropicPassthroughPath(t *testing.T) {
	f := newFixture(t)

	var gotPath, gotAPIKey, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"p"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	acc := f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL+"/v1", "sk-ant-test", []string{"claude-x"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelUsable {
		t.Fatalf("状态 = %q", res.Status)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("上游路径 = %q", gotPath)
	}
	if gotAPIKey != "sk-ant-test" {
		t.Fatalf("上游 x-api-key = %q", gotAPIKey)
	}
	if !strings.Contains(gotBody, `"max_tokens":1`) {
		t.Fatalf("max_tokens 应为 1: %s", gotBody)
	}
}

// 传输层错误的原文会带上请求地址，里面若含凭据必须被抹掉。
func TestProbeModelScrubsSecretInTransportError(t *testing.T) {
	f := newFixture(t)

	const key = "sk-transport-secret"
	acc := f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1/"+key, key, []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelError {
		t.Fatalf("状态 = %q，期望 %q", res.Status, store.ModelError)
	}
	if strings.Contains(res.Message, key) {
		t.Fatalf("错误文本泄漏了凭据: %s", res.Message)
	}
	if !strings.Contains(res.Message, "***") {
		t.Fatalf("错误文本里凭据应被替换成 ***: %s", res.Message)
	}
}

// 有的网关把上游错误再包一层 JSON 塞进 error.message，备注要给人看的话，不是报文。
func TestProbeModelUnwrapsNestedErrorBody(t *testing.T) {
	f := newFixture(t)

	inner := `{"error":{"message":"Model 'alibaba/qwen-3.6-max-preview' not found","type":"model_not_found"}}`
	quoted, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("构造响应体: %v", err)
	}
	body := `{"error":{"message":` + string(quoted) + `,"type":"not_found_error"}}`

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelNotFound {
		t.Fatalf("状态 = %q，期望 %q", res.Status, store.ModelNotFound)
	}
	if !strings.Contains(res.Message, "not found") {
		t.Fatalf("备注应是最内层那句人话: %s", res.Message)
	}
	if strings.Contains(res.Message, "\"type\"") || strings.Contains(res.Message, "{") {
		t.Fatalf("备注里不应残留嵌套报文: %s", res.Message)
	}
}

// 探活是真实计费的调用，成功那次也要在 requests 里留一行，token 从响应体读。
func TestProbeModelRecordsRequestRow(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"p"}],"usage":{"input_tokens":13,"output_tokens":1}}`)
	}))
	defer up.Close()

	acc := f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL, "sk-ant-test", []string{"claude-x"}, 100)
	gs := newGateway(t, f)

	if _, err := gs.ProbeModel(context.Background(), acc, "claude-x"); err != nil {
		t.Fatalf("探活: %v", err)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("探活应写 1 行请求记录，实际 %d 行", len(recs))
	}
	rec := recs[0]
	if rec.Inbound != gateway.ProtocolAnthropic || rec.Outbound != gateway.ProtocolAnthropic {
		t.Fatalf("入站与出站都应取厂商默认协议，实际 %s→%s", rec.Inbound, rec.Outbound)
	}
	if rec.ModelRequested != "claude-x" || rec.ModelUsed != "claude-x" {
		t.Fatalf("模型字段 = %q / %q", rec.ModelRequested, rec.ModelUsed)
	}
	if rec.AccountID != acc.ID {
		t.Fatalf("账号 id = %d，期望 %d", rec.AccountID, acc.ID)
	}
	if rec.HTTPStatus != http.StatusOK || !rec.OK {
		t.Fatalf("状态 = %d，ok = %v", rec.HTTPStatus, rec.OK)
	}
	if rec.InTokens != 13 || rec.OutTokens != 1 {
		t.Fatalf("token = %d/%d，期望 13/1", rec.InTokens, rec.OutTokens)
	}
	if rec.TS == 0 {
		t.Fatal("请求记录应带开始时间")
	}
}

// 失败与网络错误同样各记一行，token 记 0，凭据不进库。
func TestProbeModelRecordsFailedRequest(t *testing.T) {
	t.Run("上游 403", func(t *testing.T) {
		f := newFixture(t)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"套餐不含该模型"}}`)
		}))
		defer up.Close()

		acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
		gs := newGateway(t, f)
		if _, err := gs.ProbeModel(context.Background(), acc, "m1"); err != nil {
			t.Fatalf("探活: %v", err)
		}

		recs, err := f.st.RecentRequests(10)
		if err != nil {
			t.Fatalf("读请求记录: %v", err)
		}
		// 403 会触发换协议，三种协议各打一次，因此是三行：判定行加两行换协议前的尝试。
		if len(recs) != 3 {
			t.Fatalf("应写 3 行请求记录（打了几次就记几行），实际 %d 行", len(recs))
		}
		rec := probeVerdict(t, recs)
		if rec.HTTPStatus != http.StatusForbidden || rec.OK {
			t.Fatalf("状态 = %d，ok = %v", rec.HTTPStatus, rec.OK)
		}
		if rec.Outbound != gateway.ProtocolChat {
			t.Fatalf("判定行应是厂商默认协议那次，实际出站 %q", rec.Outbound)
		}
		if rec.InTokens != 0 || rec.OutTokens != 0 {
			t.Fatalf("非 2xx 的 token 应记 0，实际 %d/%d", rec.InTokens, rec.OutTokens)
		}
		if !strings.Contains(rec.Error, "套餐") {
			t.Fatalf("应记下错误摘要，实际 %q", rec.Error)
		}
	})

	t.Run("网络错误", func(t *testing.T) {
		f := newFixture(t)
		acc := f.add(t, "ds1", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-test", []string{"m1"}, 100)
		gs := newGateway(t, f)
		if _, err := gs.ProbeModel(context.Background(), acc, "m1"); err != nil {
			t.Fatalf("探活: %v", err)
		}

		recs, err := f.st.RecentRequests(10)
		if err != nil {
			t.Fatalf("读请求记录: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("应写 1 行请求记录，实际 %d 行", len(recs))
		}
		rec := recs[0]
		if rec.HTTPStatus != 0 || rec.OK {
			t.Fatalf("网络错误时状态应记 0 且不 ok，实际 %d/%v", rec.HTTPStatus, rec.OK)
		}
		if rec.InTokens != 0 || rec.OutTokens != 0 {
			t.Fatalf("网络错误时 token 应记 0，实际 %d/%d", rec.InTokens, rec.OutTokens)
		}
		if rec.Error == "" {
			t.Fatal("网络错误也应记下摘要")
		}
		if strings.Contains(rec.Error, "sk-test") {
			t.Fatalf("错误摘要泄漏了凭据: %s", rec.Error)
		}
	})
}

// anthropic 的探活行要把缓存读与缓存写一起记下：5.5 说这两列由该协议的 scanner 读出。
func TestProbeModelRecordsCacheTokens(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"text","text":"p"}],
			"usage":{"input_tokens":13,"output_tokens":1,
				"cache_read_input_tokens":2,"cache_creation_input_tokens":5}}`)
	}))
	defer up.Close()

	acc := f.add(t, "claude1", gateway.ProtocolAnthropic, up.URL, "sk-ant-test", []string{"claude-x"}, 100)
	gs := newGateway(t, f)

	if _, err := gs.ProbeModel(context.Background(), acc, "claude-x"); err != nil {
		t.Fatalf("探活: %v", err)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("探活应写 1 行请求记录，实际 %d 行", len(recs))
	}
	rec := recs[0]
	if rec.InTokens != 13 || rec.OutTokens != 1 {
		t.Fatalf("token = %d/%d，期望 13/1", rec.InTokens, rec.OutTokens)
	}
	if rec.CacheRead != 2 || rec.CacheWrite != 5 {
		t.Fatalf("缓存 = %d/%d，期望 2/5（cache_read_input_tokens / cache_creation_input_tokens）",
			rec.CacheRead, rec.CacheWrite)
	}
}

// chat 探活行只带缓存读，缓存写保持既有约定记 0。
func TestProbeModelChatRowHasNoCacheWrite(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"p"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":3}}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
	gs := newGateway(t, f)

	if _, err := gs.ProbeModel(context.Background(), acc, "m1"); err != nil {
		t.Fatalf("探活: %v", err)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("探活应写 1 行请求记录，实际 %d 行", len(recs))
	}
	rec := recs[0]
	if rec.InTokens != 7 || rec.OutTokens != 1 || rec.CacheRead != 3 {
		t.Fatalf("token 与缓存读 = %d/%d/%d，期望 7/1/3", rec.InTokens, rec.OutTokens, rec.CacheRead)
	}
	if rec.CacheWrite != 0 {
		t.Fatalf("chat 没有缓存写入字段，cache_write 应记 0，实际 %d", rec.CacheWrite)
	}
}

// responses 探活的用量在非流式响应体的顶层 usage 里（流式才在 response.completed 的嵌套结构里）。
// 这条从探活到 requests 行整条链路上钉住它：scanner 单测过了、接线错了照样会让 token 记 0。
func TestProbeModelResponsesRowReadsTopLevelUsage(t *testing.T) {
	f := newFixture(t)

	var gotPath, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[],
			"usage":{"input_tokens":9,"output_tokens":1,"total_tokens":10,
			"input_tokens_details":{"cached_tokens":4}}}`)
	}))
	defer up.Close()

	acc := f.add(t, "oa1", gateway.ProtocolResponses, up.URL, "sk-test", []string{"gpt-x"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "gpt-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelUsable || res.HTTPStatus != http.StatusOK {
		t.Fatalf("探活结果 = %+v，期望 usable/200", res)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("上游路径 = %q，期望 /v1/responses", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("上游 Authorization = %q", gotAuth)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("探活应写 1 行请求记录，实际 %d 行", len(recs))
	}
	rec := recs[0]
	if rec.Inbound != gateway.ProtocolResponses || rec.Outbound != gateway.ProtocolResponses {
		t.Fatalf("入站与出站都应取厂商默认协议，实际 %s→%s", rec.Inbound, rec.Outbound)
	}
	if rec.InTokens != 9 || rec.OutTokens != 1 {
		t.Fatalf("token = %d/%d，期望 9/1（顶层 usage 的 input_tokens / output_tokens）",
			rec.InTokens, rec.OutTokens)
	}
	if rec.CacheRead != 4 {
		t.Fatalf("缓存读 = %d，期望 4（input_tokens_details.cached_tokens）", rec.CacheRead)
	}
	if rec.CacheWrite != 0 {
		t.Fatalf("responses 只带缓存读，cache_write 应记 0，实际 %d", rec.CacheWrite)
	}
	if !rec.OK || rec.HTTPStatus != http.StatusOK {
		t.Fatalf("探活行应为 200 且 ok: %+v", rec)
	}
}

// 401 那条真实计费的调用同样要留一行请求记录，供 usage 汇总。
func TestProbeModelUnauthorizedRecordsRequestRow(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer up.Close()

	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-bad-key", []string{"m1"}, 100)
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("停用成功时不该返回错误: %v", err)
	}
	if res.Status != store.ModelAuthFailed {
		t.Fatalf("状态 = %q，期望 %q", res.Status, store.ModelAuthFailed)
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("401 也应写 1 行请求记录，实际 %d 行", len(recs))
	}
	rec := recs[0]
	if rec.HTTPStatus != http.StatusUnauthorized || rec.OK {
		t.Fatalf("状态 = %d，ok = %v，期望 401 且不 ok", rec.HTTPStatus, rec.OK)
	}
	if rec.InTokens != 0 || rec.OutTokens != 0 {
		t.Fatalf("非 2xx 的 token 应记 0，实际 %d/%d", rec.InTokens, rec.OutTokens)
	}
	if rec.Error == "" {
		t.Fatal("401 的探活行应记下错误摘要")
	}
	if strings.Contains(rec.Error, "sk-bad-key") {
		t.Fatalf("错误摘要泄漏了凭据: %s", rec.Error)
	}
}

// 停用账号失败时结论仍是 auth：调用方靠它决定「碰到 401 就停下」，
// 改写成 error 会让整批探活继续拿同一份坏凭据试下去。
func TestProbeModelUnauthorizedKeepsAuthWhenDisableFails(t *testing.T) {
	f := newFixture(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer up.Close()

	const key = "sk-bad-key"
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.URL, key, []string{"m1"}, 100)
	gs := newGateway(t, f)

	// 把连接设成只读：读库照常（探活的候选协议与转发地址都要读库），写必然失败，
	// 于是能单独验「停用失败」这一条。直接关连接不行，那会让前面的读也失败，
	// 探活会因为读不出厂商路由而改判成 error。
	if _, err := f.st.DB().Exec("PRAGMA query_only = 1"); err != nil {
		t.Fatalf("把库设成只读: %v", err)
	}

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err == nil {
		t.Fatal("停用失败时应返回错误，否则运维看不到账号还开着")
	}
	if res.Status != store.ModelAuthFailed {
		t.Fatalf("停用失败不该改写结论，状态 = %q，期望 %q", res.Status, store.ModelAuthFailed)
	}
	if !strings.Contains(err.Error(), "停用") {
		t.Fatalf("错误应说明停用失败: %v", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("错误文本泄漏了凭据: %v", err)
	}
}
