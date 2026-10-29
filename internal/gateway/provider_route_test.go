package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
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

// addAs 建一个指定厂商的账号。账号行只存厂商名，地址与默认协议都在厂商行上（见 DESIGN 5.8），所以：
//
//   - 厂商行已存在（种子厂商或用例自己建的）：把它的 base_url 指到 baseURL；这家还没设默认协议、
//     而这次传进来的协议正好是一条已声明的路由时，也把默认协议设成它（用例的 protocol 现在只能落在
//     厂商行上）。种子厂商的默认协议已经在迁移 0007/0009 里写好，这里不会改动它们。
//   - 厂商行不存在：按 add 的规则建一家假厂商（三条入站协议各一条按后缀规整出来的路由）。
func (f *fixture) addAs(t *testing.T, name, provider, protocol, baseURL, secretValue string, models []string) store.Account {
	t.Helper()
	p, err := f.st.GetProvider(provider)
	switch {
	case err == nil:
		patch := store.ProviderPatch{}
		changed := false
		if p.BaseURL != baseURL {
			patch.BaseURL = &baseURL
			changed = true
		}
		if p.DefaultProtocol == "" && f.hasRoute(t, provider, protocol) {
			patch.DefaultProtocol = &protocol
			changed = true
		}
		if changed {
			if err := f.st.SetProvider(provider, patch); err != nil {
				t.Fatalf("把厂商 %s 的地址指到 %s: %v", provider, baseURL, err)
			}
		}
	case errors.Is(err, store.ErrProviderNotFound):
		if err := f.st.AddProvider(store.Provider{Name: provider, BaseURL: baseURL, DefaultProtocol: protocol},
			fakeRoutes(baseURL, protocol)); err != nil {
			t.Fatalf("建假厂商 %s: %v", provider, err)
		}
	default:
		t.Fatalf("读厂商 %s: %v", provider, err)
	}
	return f.createAccount(t, name, provider, secretValue, models, 100)
}

// hasRoute 判断厂商是否声明了这条协议的路由。默认协议只能指向已声明的路由（见 store.SetProvider），
// 所以设之前先查一次。
func (f *fixture) hasRoute(t *testing.T, provider, protocol string) bool {
	t.Helper()
	routes, err := f.st.ProviderRoutes(provider)
	if err != nil {
		t.Fatalf("读厂商 %s 的路由: %v", provider, err)
	}
	for _, r := range routes {
		if r.Protocol == protocol {
			return true
		}
	}
	return false
}

// newGatewayWith 构造可注入选项的网关，并收集它打出的日志。
func newGatewayWith(t *testing.T, f *fixture, o gateway.Options) (*gateway.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var logs []string
	o.Store = f.st
	o.Key = f.key
	o.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	gs, err := gateway.New(o)
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	return gs, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(logs, "\n")
	}
}

// TestForwardUsesProviderRoute 确认出站地址按 (厂商, 协议) 的路由拼：
// deepseek 账号下 anthropic 模型打到 /anthropic/v1/messages 并带 x-api-key，
// chat 模型打到 /v1/chat/completions 并带 Bearer。凭据不进日志。
func TestForwardUsesProviderRoute(t *testing.T) {
	f := newFixture(t)
	const key = "sk-provider-route"

	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/anthropic/v1/messages":
			_, _ = io.WriteString(w, probeAnthropicOK)
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"没有这个路由"}}`)
		}
	}))
	defer up.Close()

	acc := f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.URL, key, []string{"claude-x", "gpt-y"})
	// deepseek 的 claude-* 走 anthropic：这个结论来自探活，写进 account_models.protocol。
	if err := f.st.SetModelProtocol(acc.ID, "claude-x", gateway.ProtocolAnthropic); err != nil {
		t.Fatalf("写协议发现结果: %v", err)
	}

	gs, logs := newGatewayWith(t, f, gateway.Options{})
	srv := httptest.NewServer(gs.Handler())
	defer srv.Close()

	for _, model := range []string{"claude-x", "gpt-y"} {
		status, body := postMessages(t, srv, `{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if status != http.StatusOK {
			t.Fatalf("%s 状态 = %d，响应 %s", model, status, body)
		}
	}

	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("上游调用数 = %d，期望 2：%v", len(calls), rec.paths())
	}
	if calls[0].path != "/anthropic/v1/messages" || calls[0].apiKey != key {
		t.Fatalf("claude-x 应打 /anthropic/v1/messages 并带 x-api-key，实际路径 %q，x-api-key %q",
			calls[0].path, calls[0].apiKey)
	}
	if calls[0].bearer != "" {
		t.Fatalf("anthropic 路由不该带 Bearer，实际 %q", calls[0].bearer)
	}
	if calls[1].path != "/v1/chat/completions" || calls[1].bearer != "Bearer "+key {
		t.Fatalf("gpt-y 应打 /v1/chat/completions 并带 Bearer，实际路径 %q，Authorization %q",
			calls[1].path, calls[1].bearer)
	}

	// 凭据不出现在日志与请求记录里。
	if got := logs(); strings.Contains(got, key) {
		t.Fatalf("日志泄漏了凭据:\n%s", got)
	}
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), key) {
			t.Fatalf("请求记录泄漏了凭据: %s", b)
		}
	}
	// 数据库里只有密文。
	stored, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if strings.Contains(string(stored.KeyCipher), key) {
		t.Fatal("key_cipher 里出现了凭据明文")
	}
}

// TestEndpointFallsBackWithoutProviderRoute 确认兜底：厂商在表里但没有该协议的路由时按 base_url
// 后缀猜（模型上的发现结果压过厂商默认协议时才走到这里）。账号的 provider 不在表里是另一种情况：
// 那样的账号既没有地址也没有默认协议，拼不出任何可用地址，见 TestOrphanAccountHintsProviderAdd。
func TestEndpointFallsBackWithoutProviderRoute(t *testing.T) {
	f := newFixture(t)
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	// anthropic 厂商只声明了 anthropic 一条路由；这个模型探活发现该用 chat，路由里没有这条协议，
	// 于是按 base_url 后缀猜。
	acc := f.addAs(t, "a1", "anthropic", gateway.ProtocolAnthropic, up.URL, "sk-fallback", []string{"m1"})
	if err := f.st.SetModelProtocol(acc.ID, "m1", gateway.ProtocolChat); err != nil {
		t.Fatalf("写协议发现结果: %v", err)
	}
	srv := f.server(t)

	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	want := "/v1/chat/completions"
	if paths := rec.paths(); len(paths) != 1 || paths[0] != want {
		t.Fatalf("上游路径 = %v，期望按后缀猜的 %q", paths, want)
	}
}

// TestProbeModelNarrowsCandidatesByProvider 确认探活的候选协议按厂商声明的路由收紧：
// anthropic 只声明了一条路由，探活失败时只发一次请求；deepseek 声明了两条，会试两次。
func TestProbeModelNarrowsCandidatesByProvider(t *testing.T) {
	cases := []struct {
		provider string
		protocol string
		base     string
		want     []string
	}{
		{"anthropic", gateway.ProtocolAnthropic, "https://api.anthropic.com",
			[]string{"/v1/messages"}},
		{"deepseek", gateway.ProtocolChat, "https://api.deepseek.com",
			[]string{"/v1/chat/completions", "/anthropic/v1/messages"}},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			f := newFixture(t)
			rec := &callRecorder{}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
			}))
			defer up.Close()

			// 厂商的 base_url 指向假上游，厂商与协议走真表里的路由。
			acc := f.addAs(t, "a1", c.provider, c.protocol, up.URL, "sk-narrow", []string{"m1"})
			gs := newGateway(t, f)

			res, err := gs.ProbeModel(context.Background(), acc, "m1")
			if err != nil {
				t.Fatalf("探活: %v", err)
			}
			if res.Status != store.ModelNotFound {
				t.Fatalf("结论 = %+v，期望 notfound", res)
			}
			paths := rec.paths()
			if len(paths) != len(c.want) {
				t.Fatalf("上游收到 %d 次调用 %v，期望 %d 次 %v", len(paths), paths, len(c.want), c.want)
			}
			for i := range c.want {
				if paths[i] != c.want[i] {
					t.Fatalf("第 %d 次调用路径 = %q，期望 %q（全部调用 %v）", i+1, paths[i], c.want[i], paths)
				}
			}
		})
	}
}

// TestProbeModelStartsFromDiscoveredProtocol 确认已经发现过协议的模型不再每次都白打一次 404：
// 库里的结论排在厂商默认协议之前，直接命中就只花一次调用。
func TestProbeModelStartsFromDiscoveredProtocol(t *testing.T) {
	f := newFixture(t)
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		if r.URL.Path != "/anthropic/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, probeAnthropicOK)
	}))
	defer up.Close()

	// 厂商默认协议是 chat，但 claude-x 已经发现成 anthropic。
	acc := f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.URL, "sk-discovered", []string{"claude-x"})
	if err := f.st.SetModelProtocol(acc.ID, "claude-x", gateway.ProtocolAnthropic); err != nil {
		t.Fatalf("写协议发现结果: %v", err)
	}

	gs := newGateway(t, f)
	res, err := gs.ProbeModel(context.Background(), acc, "claude-x")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	if res.Status != store.ModelUsable || res.Protocol != gateway.ProtocolAnthropic {
		t.Fatalf("结论 = %+v，期望 usable/anthropic", res)
	}
	if paths := rec.paths(); len(paths) != 1 || paths[0] != "/anthropic/v1/messages" {
		t.Fatalf("上游调用 = %v，期望直接用已发现的协议，只打一次", paths)
	}
}

// TestProbeModelTriesEveryCandidate 确认换协议之后遇到非重试类失败也不停手：
// 第三个候选仍要试到（原来的 break 会让它永远试不到），结论仍取厂商默认协议那次。
func TestProbeModelTriesEveryCandidate(t *testing.T) {
	f := newFixture(t)
	rec := &callRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"claude-* 只接受 /messages"}}`)
		case "/v1/messages":
			// 换协议之后的 5xx 不是重试类失败，但仍然不该让循环停手。
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"上游炸了"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
		}
	}))
	defer up.Close()

	// 假厂商声明了三条入站协议的路由（厂商不在种子表里，由 addAs 建行）：三种协议全试。
	acc := f.addAs(t, "ds1", "custom", gateway.ProtocolChat, up.URL, "sk-all-candidates", []string{"m1"})
	gs := newGateway(t, f)

	res, err := gs.ProbeModel(context.Background(), acc, "m1")
	if err != nil {
		t.Fatalf("探活: %v", err)
	}
	paths := rec.paths()
	if len(paths) != 3 {
		t.Fatalf("上游收到 %d 次调用 %v，期望三个候选都试到", len(paths), paths)
	}
	if res.HTTPStatus != http.StatusForbidden || res.Status != store.ModelForbidden {
		t.Fatalf("结论 = %+v，期望取厂商默认协议那次的 403", res)
	}
}

// TestProbeModelRouteProtocolsOption 确认 resolver 可以注入：注入后以它给的候选为准，
// 出现线协议以外的值直接报错，不发上游请求也不写请求记录。
func TestProbeModelRouteProtocolsOption(t *testing.T) {
	t.Run("按注入的候选", func(t *testing.T) {
		f := newFixture(t)
		rec := &callRecorder{}
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
		}))
		defer up.Close()

		acc := f.addAs(t, "a1", "anthropic", gateway.ProtocolChat, up.URL, "sk-resolver", []string{"m1"})
		gs, _ := newGatewayWith(t, f, gateway.Options{
			RouteProtocols: func(provider, model string) []string {
				return []string{gateway.ProtocolResponses}
			},
		})

		if _, err := gs.ProbeModel(context.Background(), acc, "m1"); err != nil {
			t.Fatalf("探活: %v", err)
		}
		// 注入的候选只有 responses，加上这家厂商的默认协议 anthropic，正好两次（anthropic 在前）。
		want := []string{"/v1/messages", "/v1/responses"}
		paths := rec.paths()
		if len(paths) != len(want) {
			t.Fatalf("上游调用 = %v，期望 %v", paths, want)
		}
		for i := range want {
			if paths[i] != want[i] {
				t.Fatalf("上游调用 = %v，期望 %v", paths, want)
			}
		}
	})

	t.Run("非法协议报错", func(t *testing.T) {
		f := newFixture(t)
		rec := &callRecorder{}
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), "")
		}))
		defer up.Close()

		acc := f.addAs(t, "a1", "custom", gateway.ProtocolChat, up.URL, "sk-bad-protocol", []string{"m1"})
		gs, _ := newGatewayWith(t, f, gateway.Options{
			RouteProtocols: func(provider, model string) []string {
				return []string{"cohere"}
			},
		})

		if _, err := gs.ProbeModel(context.Background(), acc, "m1"); err == nil {
			t.Fatal("非法协议应报错，不能静默落进 chat 分支")
		} else if !strings.Contains(err.Error(), "cohere") {
			t.Fatalf("错误信息应带上非法取值，实际 %v", err)
		}
		if paths := rec.paths(); len(paths) != 0 {
			t.Fatalf("配置非法时不该发上游请求，实际 %v", paths)
		}
		recs, err := f.st.RecentRequests(10)
		if err != nil {
			t.Fatalf("读请求记录: %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("一次调用都没发，不该有请求记录: %+v", recs)
		}
	})
}

// TestSyncModelsUsesProviderModelsPath 确认模型列表地址按 models_path 取：
// commandcode 在 /models；deepseek 的 anthropic 账号没有列表接口，回退到同厂商第一条非空路径。
func TestSyncModelsUsesProviderModelsPath(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		protocol string
		want     string
	}{
		{"commandcode 的 /models", "commandcode", gateway.ProtocolChat, "/models"},
		{"deepseek 的 anthropic 账号回退到 chat 的列表", "deepseek", gateway.ProtocolAnthropic, "/v1/models"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			var gotPath string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m-b"},{"id":"m-a"}]}`)
			}))
			defer up.Close()

			acc := f.addAs(t, "a1", c.provider, c.protocol, up.URL, "sk-sync", nil)
			gs := newGateway(t, f)

			models, err := gs.SyncModels(context.Background(), acc)
			if err != nil {
				t.Fatalf("同步: %v", err)
			}
			if len(models) != 2 || models[0] != "m-a" || models[1] != "m-b" {
				t.Fatalf("模型 = %v，期望排序去重后的 [m-a m-b]", models)
			}
			if gotPath != c.want {
				t.Fatalf("模型列表路径 = %q，期望 %q", gotPath, c.want)
			}
		})
	}
}

// TestSyncModelsWithoutModelsPath 确认厂商有路由但一个非空 models_path 都没有时报错，
// 并提示手填；厂商只有地址、一条路由都没声明时仍按 base_url 后缀猜。
func TestSyncModelsWithoutModelsPath(t *testing.T) {
	t.Run("全部为空", func(t *testing.T) {
		f := newFixture(t)
		// 只有 anthropic 一条路由：写入侧的推导只给 openai-chat 补 models_path，
		// 所以这条路由的 models_path 仍是空串，同步时没有列表地址可用。
		if err := f.st.AddProvider(store.Provider{Name: "nollm", BaseURL: "https://nollm.example.com"},
			[]store.ProviderRoute{{Protocol: "anthropic", Path: "/v1/messages"}}); err != nil {
			t.Fatalf("加厂商: %v", err)
		}
		var hits int
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
		}))
		defer up.Close()

		acc := f.addAs(t, "a1", "nollm", gateway.ProtocolChat, up.URL, "sk-nollm", nil)
		gs := newGateway(t, f)

		_, err := gs.SyncModels(context.Background(), acc)
		if err == nil {
			t.Fatal("没有模型列表接口时应报错")
		}
		if !strings.Contains(err.Error(), "没有模型列表接口") || !strings.Contains(err.Error(), "--models") {
			t.Fatalf("错误文案应提示用 --models 手填，实际 %v", err)
		}
		if hits != 0 {
			t.Fatalf("没有列表接口就不该发请求，实际 %d 次", hits)
		}
	})

	t.Run("一条路由都没有时按后缀猜", func(t *testing.T) {
		f := newFixture(t)
		var gotPath string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
		}))
		defer up.Close()

		// 厂商只有地址：一条路由、一个 models_path 都没有，列表地址回到 base_url 后缀兜底。
		if err := f.st.AddProvider(store.Provider{Name: "addr-only", BaseURL: up.URL}, nil); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		acc := f.createAccount(t, "a1", "addr-only", "sk-custom", nil, 100)
		gs := newGateway(t, f)

		if _, err := gs.SyncModels(context.Background(), acc); err != nil {
			t.Fatalf("只有地址时仍该能同步: %v", err)
		}
		if gotPath != "/v1/models" {
			t.Fatalf("模型列表路径 = %q，期望按后缀猜的 /v1/models", gotPath)
		}
	})
}

// probeResponsesOK 是一条合法的 openai-responses 响应，探活只读状态与用量。
const probeResponsesOK = `{"id":"resp_1","object":"response","status":"completed","output":[],
	"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`

// TestProbeModelUsesProviderRoutesPerProtocol 确认三种协议的探活都按 (厂商, 协议) 的路由拼地址：
// deepseek 账号下 claude 模型打到 /anthropic/v1/messages 并带 x-api-key，chat 模型打到
// /v1/chat/completions 并带 Bearer，openai 的 responses 模型打到 /v1/responses；
// 三者都把成功那次的协议写进 account_models.protocol。
//
// 没有这条断言，路由拼错只会表现为某个模型始终探不活，而「探活打的是哪个地址」正是
// 厂商表落库之后最容易悄悄偏离的地方。
func TestProbeModelUsesProviderRoutesPerProtocol(t *testing.T) {
	cases := []struct {
		name            string
		provider        string
		accountProtocol string
		model           string
		okPath          string
		okBody          string
		wantCalls       []string
		wantProtocol    string
	}{
		{
			name: "deepseek 的 anthropic 模型", provider: "deepseek", accountProtocol: gateway.ProtocolChat,
			model: "claude-x", okPath: "/anthropic/v1/messages", okBody: probeAnthropicOK,
			wantCalls:    []string{"/v1/chat/completions", "/anthropic/v1/messages"},
			wantProtocol: gateway.ProtocolAnthropic,
		},
		{
			name: "deepseek 的 chat 模型", provider: "deepseek", accountProtocol: gateway.ProtocolChat,
			model: "gpt-y", okPath: "/v1/chat/completions",
			okBody:       `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
			wantCalls:    []string{"/v1/chat/completions"},
			wantProtocol: gateway.ProtocolChat,
		},
		{
			name: "openai 的 responses 模型", provider: "openai", accountProtocol: gateway.ProtocolChat,
			model: "o3-mini", okPath: "/v1/responses", okBody: probeResponsesOK,
			wantCalls:    []string{"/v1/chat/completions", "/v1/responses"},
			wantProtocol: gateway.ProtocolResponses,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			const key = "sk-probe-route"
			rec := &callRecorder{}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
				if r.URL.Path != c.okPath {
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"error":{"message":"no route"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, c.okBody)
			}))
			defer up.Close()

			// 账号的地址指向假上游，厂商与协议走真表里的路由。
			acc := f.addAs(t, "a1", c.provider, c.accountProtocol, up.URL, key, []string{c.model})
			gs := newGateway(t, f)

			res, err := gs.ProbeModel(context.Background(), acc, c.model)
			if err != nil {
				t.Fatalf("探活: %v", err)
			}
			if res.Status != store.ModelUsable || res.Protocol != c.wantProtocol {
				t.Fatalf("结论 = %+v，期望 usable/%s", res, c.wantProtocol)
			}

			calls := rec.all()
			if len(calls) != len(c.wantCalls) {
				t.Fatalf("上游调用 = %v，期望 %v", rec.paths(), c.wantCalls)
			}
			for i, want := range c.wantCalls {
				if calls[i].path != want {
					t.Fatalf("第 %d 次调用路径 = %q，期望 %q（全部调用 %v）", i+1, calls[i].path, want, rec.paths())
				}
			}

			// 凭据头按协议走：anthropic 用 x-api-key，另两种用 Bearer，且只有那一个。
			last := calls[len(calls)-1]
			if c.wantProtocol == gateway.ProtocolAnthropic {
				if last.apiKey != key || last.bearer != "" {
					t.Fatalf("anthropic 的凭据头 = x-api-key %q / bearer %q，期望只有 x-api-key", last.apiKey, last.bearer)
				}
			} else if last.bearer != "Bearer "+key || last.apiKey != "" {
				t.Fatalf("%s 的凭据头 = bearer %q / x-api-key %q，期望只有 Bearer", c.wantProtocol, last.bearer, last.apiKey)
			}

			// 发现结果落在模型行上。
			rows, err := f.st.AccountModels(acc.ID)
			if err != nil {
				t.Fatalf("读模型行: %v", err)
			}
			if len(rows) != 1 || rows[0].Model != c.model || rows[0].Protocol != c.wantProtocol {
				t.Fatalf("模型行 = %+v，期望 %s → %s", rows, c.model, c.wantProtocol)
			}
		})
	}
}
