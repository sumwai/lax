package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lax/internal/gateway"
	"lax/internal/plugin"
	"lax/internal/store"
)

// upstreamRecorder 记下每个请求的方法、路径与几个关心的头，并按路径回固定响应。
type upstreamRecorder struct {
	requests []authRequest
}

type authRequest struct {
	Method        string
	Path          string
	Authorization string
	Headers       http.Header
}

// recordUpstream 起一个假上游：GET 模型列表回一条空清单，其余请求回一段最简 chat 响应。
func recordUpstream(t *testing.T) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rec.requests = append(rec.requests, authRequest{
			Method: r.Method, Path: r.URL.Path,
			Authorization: r.Header.Get("Authorization"), Headers: r.Header.Clone(),
		})
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// authProvider 建一家声明了凭据头的厂商，返回值是它的账号。
func authProvider(t *testing.T, f *fixture, baseURL, header, scheme string) store.Account {
	t.Helper()
	if err := f.st.AddProvider(store.Provider{
		Name: "doubao", BaseURL: baseURL, AuthHeader: header, AuthScheme: scheme,
		DefaultProtocol: gateway.ProtocolChat,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	return f.createAccount(t, "d1", "doubao", "sk-secret", []string{"m1"}, 100)
}

// TestProviderAuthHeader 钉住 auth 声明的三件事：转发、模型列表同步与探活都发声明的那一个头，
// 并且不再发协议默认的 Authorization。三种写法各测一次。
func TestProviderAuthHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		scheme string
		want   string
	}{
		{"raw", "X-Api-Key", "raw", "sk-secret"},
		{"bearer", "X-Api-Key", "bearer", "Bearer sk-secret"},
		{"bearer-semicolon", "X-Api-Key", "bearer-semicolon", "Bearer;sk-secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up, rec := recordUpstream(t)
			authProvider(t, f, up.URL, c.header, c.scheme)
			srv := f.server(t)

			status, body := postPath(t, srv, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 = %s", status, body)
			}
			if len(rec.requests) != 1 {
				t.Fatalf("上游收到 %d 个请求，期望 1 个", len(rec.requests))
			}
			got := rec.requests[0]
			if v := got.Headers.Get(c.header); v != c.want {
				t.Fatalf("%s = %q，期望 %q", c.header, v, c.want)
			}
			if got.Authorization != "" {
				t.Fatalf("声明了 %s 之后不该再发 Authorization，实际 %q", c.header, got.Authorization)
			}
		})
	}

	t.Run("未声明的厂商仍按协议发 Bearer", func(t *testing.T) {
		f := newFixture(t)
		up, rec := recordUpstream(t)
		if err := f.st.AddProvider(store.Provider{
			Name: "plain", BaseURL: up.URL, DefaultProtocol: gateway.ProtocolChat,
		}, []store.ProviderRoute{{Protocol: gateway.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
			t.Fatalf("建厂商: %v", err)
		}
		f.createAccount(t, "p1", "plain", "sk-plain", []string{"m1"}, 100)
		srv := f.server(t)

		if status, body := postPath(t, srv, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 = %s", status, body)
		}
		if got := rec.requests[0].Authorization; got != "Bearer sk-plain" {
			t.Fatalf("Authorization = %q，期望按协议发 Bearer", got)
		}
	})

	t.Run("模型列表同步", func(t *testing.T) {
		f := newFixture(t)
		up, rec := recordUpstream(t)
		acc := authProvider(t, f, up.URL, "X-Api-Key", "raw")
		gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key})
		if err != nil {
			t.Fatalf("构造网关: %v", err)
		}
		if _, err := gs.SyncModels(context.Background(), acc); err != nil {
			t.Fatalf("同步模型: %v", err)
		}
		if len(rec.requests) == 0 {
			t.Fatal("上游没收到请求")
		}
		got := rec.requests[0]
		if got.Path != "/v1/models" {
			t.Fatalf("路径 = %q，期望 /v1/models", got.Path)
		}
		if v := got.Headers.Get("X-Api-Key"); v != "sk-secret" {
			t.Fatalf("模型列表请求的 X-Api-Key = %q，期望 sk-secret", v)
		}
		if got.Authorization != "" {
			t.Fatalf("模型列表请求不该发 Authorization，实际 %q", got.Authorization)
		}
	})

	t.Run("探活", func(t *testing.T) {
		f := newFixture(t)
		up, rec := recordUpstream(t)
		acc := authProvider(t, f, up.URL, "X-Api-Key", "raw")
		gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key})
		if err != nil {
			t.Fatalf("构造网关: %v", err)
		}
		if _, err := gs.Probe(context.Background(), acc); err != nil {
			t.Fatalf("探活: %v", err)
		}
		if len(rec.requests) == 0 {
			t.Fatal("上游没收到请求")
		}
		for _, got := range rec.requests {
			if v := got.Headers.Get("X-Api-Key"); v != "sk-secret" {
				t.Fatalf("%s 请求的 X-Api-Key = %q，期望 sk-secret", got.Path, v)
			}
			if got.Authorization != "" {
				t.Fatalf("%s 请求不该发 Authorization，实际 %q", got.Path, got.Authorization)
			}
		}
	})
}

// headerDialect 是「在自己的线协议上编请求」的插件方言：入站是 openai-speech，它也声明
// openai-speech，但路径、请求体与请求头都由它自己给。豆包的 TTS 就是这个形状。
type headerDialect struct{}

func (headerDialect) ID() string           { return "com.example.hdr/tts@1" }
func (headerDialect) Operation() string    { return plugin.OpSpeechSynthesize }
func (headerDialect) UpstreamWire() string { return plugin.WireSpeech }

func (headerDialect) Prepare(in plugin.Input) (plugin.Plan, error) {
	return plugin.Plan{
		JSON: map[string]any{"req_params": map[string]any{"text": in.Text}},
		Headers: map[string]string{
			"x-api-request-id": "req-1",
			// 保留头必须被网关丢掉：凭据只由 auth 声明注入。
			"Authorization": "Bearer stolen",
			"x-api-key":     "stolen",
		},
	}, nil
}

func (headerDialect) Decode(in plugin.Input, resp plugin.Response) (plugin.Result, error) {
	var payload struct {
		Audio string `json:"audio"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return plugin.Result{}, err
	}
	return plugin.Result{Kind: plugin.ResultJSON, JSON: []byte(`{"audio":"` + payload.Audio + `"}`)}, nil
}

// TestSpeechHookDialectOwnWire 钉住两件事：语音方言可以在自己的线协议上编请求（不再要求
// upstream_wire 必须是 openai-chat），以及 Plan.Headers 生效但覆盖不了凭据头。
func TestSpeechHookDialectOwnWire(t *testing.T) {
	f := newFixture(t)
	up, rec := recordUpstream(t)
	if err := f.st.AddProvider(store.Provider{
		Name: "doubao", BaseURL: up.URL, AuthHeader: "X-Api-Key", AuthScheme: "raw",
		DefaultProtocol: gateway.ProtocolSpeech,
	}, []store.ProviderRoute{{Protocol: gateway.ProtocolSpeech, Path: "/api/v3/tts/unidirectional"}}); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "d1", "doubao", "sk-secret", []string{"seed-tts"}, 100)
	if _, err := f.st.AddProviderModelCapability("doubao", "seed-tts", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech},
	}); err != nil {
		t.Fatalf("声明能力: %v", err)
	}
	bindDialect(t, f, "doubao", "seed-tts", plugin.OpSpeechSynthesize, headerDialect{}.ID())

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.hdr", headerDialect{}); err != nil {
		t.Fatalf("登记方言: %v", err)
	}
	gs, err := gateway.New(gateway.Options{Store: f.st, Key: f.key, Dialects: reg})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)

	status, body := postPath(t, srv, "/v1/audio/speech", `{"model":"seed-tts","input":"你好","voice":"v1"}`)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 = %s", status, body)
	}

	if len(rec.requests) != 1 {
		t.Fatalf("上游收到 %d 个请求，期望 1 个", len(rec.requests))
	}
	got := rec.requests[0]
	if got.Path != "/api/v3/tts/unidirectional" {
		t.Fatalf("上游路径 = %q，期望按厂商路由走插件自己的路径", got.Path)
	}
	if ct := got.Headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q，期望 application/json", ct)
	}
	if v := got.Headers.Get("X-Api-Request-Id"); v != "req-1" {
		t.Fatalf("插件给的 x-api-request-id = %q，期望生效", v)
	}
	if v := got.Headers.Get("X-Api-Key"); v != "sk-secret" {
		t.Fatalf("X-Api-Key = %q，期望宿主注入的 sk-secret（插件不能覆盖凭据）", v)
	}
	if got.Authorization != "" {
		t.Fatalf("插件给的 Authorization 被写了进去: %q", got.Authorization)
	}
}
