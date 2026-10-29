package gateway_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"switchboard/internal/gateway"
)

// geminiPage 是假上游返回的一页模型列表：body 与请求里看到的查询串都记下来。
type geminiPage struct {
	body  string
	token string
}

// syncGemini 用 gemini 协议的账号跑一次 SyncModels：厂商取种子里的 google-gemini，
// 地址指向假上游。pages 按 nextPageToken 的顺序回放，第 n 次请求拿第 n 页（越界时重复最后一页）。
func syncGemini(t *testing.T, pages []geminiPage, secretValue string) (
	models []string, syncErr error, logs string, queries []string) {
	t.Helper()

	f := newFixture(t)
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := len(queries)
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		if idx >= len(pages) {
			idx = len(pages) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pages[idx].body)
	}))
	t.Cleanup(up.Close)

	acc := f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, secretValue, nil)
	gs, collected := newGatewayWith(t, f, gateway.Options{})
	models, syncErr = gs.SyncModels(context.Background(), acc)
	return models, syncErr, collected(), queries
}

// TestSyncModelsGeminiShape 钉住 gemini 的清单形状：`{models:[{name:"models/…"}]}` 复用第二种形状，
// 另加两条后处理——只留 supportedGenerationMethods 含 generateContent 的条目、名字去掉一个
// `models/` 前缀。地址与凭据头按种子的 gemini 路由走：`/v1beta/models` 加 `x-goog-api-key`。
func TestSyncModelsGeminiShape(t *testing.T) {
	body := `{"models":[
		{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent","countTokens"]},
		{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent","countTokens"]},
		{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]},
		{"name":"models/embedding-001","supportedGenerationMethods":[]},
		{"name":"models/gemini-1.5-pro"}
	]}`

	models, err, logs, queries := syncGemini(t, []geminiPage{{body: body}}, shapeKey)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	// 只留 generateContent：embedding 两条不要；没写 supportedGenerationMethods 的条目同样落选
	// （上游没说它支持生成，宁少不错）。名字都是裸名，不带 models/ 前缀。
	if got, want := strings.Join(models, ","), "gemini-2.5-flash,gemini-2.5-pro"; got != want {
		t.Fatalf("模型 = %q，期望 %q（按方法过滤、去前缀）", got, want)
	}
	if len(queries) != 1 {
		t.Fatalf("没有 nextPageToken 时只该请求一次，实际 %d 次: %v", len(queries), queries)
	}
	if want := "/v1beta/models?pageSize=1000"; queries[0] != want {
		t.Fatalf("模型列表请求 = %q，期望 %q（种子里的 gemini 路由，带页大小）", queries[0], want)
	}
	mustNotLeak(t, shapeKey, models, err, logs)
}

// TestSyncModelsGeminiAuthHeader 钉住 gemini 的列表请求发 x-goog-api-key：地址来自 gemini 路由，
// 凭据头就按 gemini 选，不是账号协议那一套之外的猜法（见 DESIGN 5.7）。
func TestSyncModelsGeminiAuthHeader(t *testing.T) {
	f := newFixture(t)
	var apiKey, bearer string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey, bearer = r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro",
			"supportedGenerationMethods":["generateContent"]}]}`)
	}))
	t.Cleanup(up.Close)

	acc := f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, shapeKey, nil)
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	models, err := gs.SyncModels(context.Background(), acc)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	if apiKey != shapeKey {
		t.Fatalf("x-goog-api-key = %q，期望解出凭据后放进这个头", apiKey)
	}
	if bearer != "" {
		t.Fatalf("gemini 不该发 Authorization，实际 %q", bearer)
	}
	mustNotLeak(t, shapeKey, models, err, logs())
}

// TestSyncModelsGeminiPaginates 钉住分页：第一页带 pageSize，响应里有 nextPageToken 时带 pageToken
// 取下一页，最后一页没有游标就停；两页的模型合并、去重、排序。
func TestSyncModelsGeminiPaginates(t *testing.T) {
	pages := []geminiPage{
		{body: `{"models":[
			{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},
			{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}],
			"nextPageToken":"page-2"}`},
		{body: `{"models":[
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]}],
			"nextPageToken":""}`},
	}

	models, err, logs, queries := syncGemini(t, pages, shapeKey)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	if got, want := strings.Join(models, ","), "gemini-2.5-flash,gemini-2.5-pro"; got != want {
		t.Fatalf("模型 = %q，期望 %q（两页合并、去重、排序）", got, want)
	}
	if len(queries) != 2 {
		t.Fatalf("两页应当请求两次，实际 %d 次: %v", len(queries), queries)
	}
	if want := "/v1beta/models?pageSize=1000"; queries[0] != want {
		t.Fatalf("第一页 = %q，期望 %q", queries[0], want)
	}
	if want := "/v1beta/models?pageSize=1000&pageToken=page-2"; queries[1] != want {
		t.Fatalf("第二页 = %q，期望 %q（带上游给的游标）", queries[1], want)
	}
	mustNotLeak(t, shapeKey, models, err, logs)
}

// TestSyncModelsGeminiPageLimit 钉住页数上限：上游一直在翻页时报错，不静默截断成前 10 页，
// 也不无限翻下去。错误文案说明清单过大，并指出页大小与地址。
func TestSyncModelsGeminiPageLimit(t *testing.T) {
	// 每一页都给下一个游标，永远取不完。
	var mu sync.Mutex
	hits := 0
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		token := fmt.Sprintf("page-%d", hits+1)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro",
			"supportedGenerationMethods":["generateContent"]}],"nextPageToken":"`+token+`"}`)
	}))
	t.Cleanup(up.Close)

	acc := f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, shapeKey, nil)
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	models, err := gs.SyncModels(context.Background(), acc)
	if err == nil {
		t.Fatalf("超过页数上限应报错，得到 %v", models)
	}
	if len(models) != 0 {
		t.Fatalf("出错时不应返回模型: %v", models)
	}
	msg := err.Error()
	for _, want := range []string{"10 页", "pageSize=1000", "超过"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案应提到 %q: %v", want, err)
		}
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 10 {
		t.Fatalf("页数上限是 10，上游应当收到 10 次请求，实际 %d 次", got)
	}
	mustNotLeak(t, shapeKey, models, err, logs())
}

// TestSyncModelsNoNextPageTokenStops 钉住「nextPageToken 空即停」：字段存在但为空串、
// 空白串或不是字符串时都不再多请求一页。
func TestSyncModelsNoNextPageTokenStops(t *testing.T) {
	cases := []struct{ name, body string }{
		{"nextPageToken 为空串", `{"models":[{"name":"models/gemini-2.5-pro",
			"supportedGenerationMethods":["generateContent"]}],"nextPageToken":""}`},
		{"nextPageToken 只有空白", `{"models":[{"name":"models/gemini-2.5-pro",
			"supportedGenerationMethods":["generateContent"]}],"nextPageToken":"  "}`},
		{"nextPageToken 不是字符串", `{"models":[{"name":"models/gemini-2.5-pro",
			"supportedGenerationMethods":["generateContent"]}],"nextPageToken":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			models, err, logs, queries := syncGemini(t, []geminiPage{{body: c.body}}, shapeKey)
			if err != nil {
				t.Fatalf("同步模型: %v", err)
			}
			if got, want := strings.Join(models, ","), "gemini-2.5-pro"; got != want {
				t.Fatalf("模型 = %q，期望 %q", got, want)
			}
			if len(queries) != 1 {
				t.Fatalf("没有可用的游标时只该请求一次，实际 %d 次: %v", len(queries), queries)
			}
			mustNotLeak(t, shapeKey, models, err, logs)
		})
	}
}

// TestSyncModelsWithoutGenerationMethodsUnfiltered 钉住过滤只在响应里真的出现
// supportedGenerationMethods 字段时才生效：ollama 那种 `{models:[{name}]}` 一条都不许少，
// 否则没有方法列表的上游会被过滤成空列表。
func TestSyncModelsWithoutGenerationMethodsUnfiltered(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			name: "ollama 的 models 没有方法列表",
			body: `{"models":[{"name":"llama3:latest","model":"llama3"},{"name":"qwen2.5","model":"qwen2.5"}]}`,
			want: "llama3:latest,qwen2.5",
		},
		{
			name: "data 形状没有方法列表",
			body: `{"data":[{"id":"m-b"},{"id":"m-a"}]}`,
			want: "m-a,m-b",
		},
		{
			// 去前缀与形状无关：任何形状里以 models/ 开头的名字都去一个前缀。
			name: "data 形状里带前缀的名字也去前缀",
			body: `{"data":[{"id":"models/m1"},{"id":"models/models/m2"}]}`,
			want: "m1,models/m2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			models, err, logs := syncBody(t, c.body, shapeKey)
			if err != nil {
				t.Fatalf("同步模型: %v", err)
			}
			if got := strings.Join(models, ","); got != c.want {
				t.Fatalf("模型 = %q，期望 %q", got, c.want)
			}
			mustNotLeak(t, shapeKey, models, err, logs)
		})
	}
}

// TestSyncModelsRepeatedNextPageToken 钉住「上游反复给同一个游标」：不白打满 10 页，
// 第二次拿到同一个 nextPageToken 就报错，文案说明接口可能不认 pageToken。
func TestSyncModelsRepeatedNextPageToken(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var queries []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro",
			"supportedGenerationMethods":["generateContent"]}],"nextPageToken":"page-2"}`)
	}))
	t.Cleanup(up.Close)

	acc := f.addAs(t, "gem", "google-gemini", gateway.ProtocolGemini, up.URL, shapeKey, nil)
	gs, logs := newGatewayWith(t, f, gateway.Options{})
	models, err := gs.SyncModels(context.Background(), acc)
	if err == nil {
		t.Fatalf("重复的游标应报错，得到 %v", models)
	}
	if len(models) != 0 {
		t.Fatalf("出错时不应返回模型: %v", models)
	}
	if !strings.Contains(err.Error(), "重复") || !strings.Contains(err.Error(), "page-2") {
		t.Fatalf("错误文案应说明游标重复并带上游标: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), queries...)
	mu.Unlock()
	want := []string{"pageSize=1000", "pageSize=1000&pageToken=page-2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("上游收到的查询串 = %v，期望 %v（第二个游标重复即停，不再往下打）", got, want)
	}
	mustNotLeak(t, shapeKey, models, err, logs())
}
