package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
	"lax/internal/translate"
)

// uriUpstream 起一个假上游，记下每一次请求的完整 RequestURI（含查询串）、Host 与两个凭据头。
// 只看 r.URL.Path 判不出「整条地址有没有被 base_url 带头」，所以要连 RequestURI 一起记。
type uriUpstream struct {
	url     string
	host    string
	hits    int
	uri     string
	bearer  string
	apiKey  string
	respond string
}

func newURIUpstream(t *testing.T, respond string) *uriUpstream {
	t.Helper()
	u := &uriUpstream{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		u.uri = r.URL.RequestURI()
		u.host = r.Host
		u.bearer = r.Header.Get("Authorization")
		u.apiKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, u.respond)
	}))
	t.Cleanup(srv.Close)
	u.url = srv.URL
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析假上游地址: %v", err)
	}
	u.host = parsed.Host
	return u
}

// TestForwardUsesFullAddressRoute 钉住整条地址路由：路由的取值是整条地址时直接访问它，
// 不再拼厂商的 base_url——同一厂商的不同协议因此可以落在不同域名上。
// 凭据头仍按协议选：anthropic 走 x-api-key，另两家走 Bearer。凭据不进日志。
func TestForwardUsesFullAddressRoute(t *testing.T) {
	cases := []struct {
		protocol   string
		routeValue string // 写进 provider_routes 的整条地址（故意带多余斜杠与查询串）
		wantURI    string
	}{
		{translate.ProtocolChat, "//v1//chat//completions/?api-version=2024-05-01", "/v1/chat/completions?api-version=2024-05-01"},
		{translate.ProtocolAnthropic, "//v7//messages/", "/v7/messages"},
	}
	for _, c := range cases {
		t.Run(c.protocol, func(t *testing.T) {
			f := newFixture(t)
			const key = "sk-full-address-route"
			up := newURIUpstream(t, upstreamBodies[c.protocol])
			// 厂商的 base_url 指向另一个域名：整条地址路由不该被它带跑。
			decoy := newURIUpstream(t, upstreamBodies[c.protocol])

			if err := f.st.CreateProvider(store.Provider{Name: "split", BaseURL: decoy.url},
				[]store.ProviderRoute{{Protocol: c.protocol, Path: up.url + c.routeValue}}); err != nil {
				t.Fatalf("加厂商: %v", err)
			}
			f.addAs(t, "a1", "split", c.protocol, decoy.url, key, []string{"m1"})

			gs, logs := newGatewayWith(t, f, gateway.Options{})
			srv := httptest.NewServer(gs.Handler())
			t.Cleanup(srv.Close)

			status, body := postTo(t, srv, requestBodies[c.protocol], c.protocol)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, body)
			}
			if up.hits != 1 {
				t.Fatalf("整条地址那个上游收到 %d 次请求，期望 1 次", up.hits)
			}
			if up.uri != c.wantURI {
				t.Fatalf("上游收到的 RequestURI = %q，期望 %q（地址不该被 base_url 前缀带跑）", up.uri, c.wantURI)
			}
			if up.host == "" || strings.Contains(up.url, up.host) == false {
				t.Fatalf("上游 Host = %q，与假上游地址 %q 对不上", up.host, up.url)
			}
			if decoy.hits != 0 {
				t.Fatalf("厂商 base_url 那个域名收到 %d 次请求，整条地址路由不该碰它", decoy.hits)
			}
			if c.protocol == translate.ProtocolAnthropic {
				if up.apiKey != key {
					t.Fatalf("x-api-key = %q，期望按 anthropic 协议发账号凭据", up.apiKey)
				}
				if up.bearer != "" {
					t.Fatalf("不该发 Authorization，实际 %q", up.bearer)
				}
			} else {
				if up.bearer != "Bearer "+key {
					t.Fatalf("Authorization = %q，期望按 openai 兼容协议发 Bearer", up.bearer)
				}
				if up.apiKey != "" {
					t.Fatalf("不该发 x-api-key，实际 %q", up.apiKey)
				}
			}
			// 凭据只到上游：日志里不该出现 key 明文。
			if leaked := logs(); strings.Contains(leaked, key) {
				t.Fatalf("日志里出现了 key 明文: %s", leaked)
			}
		})
	}
}

// TestForwardMixesFullAddressAndRelativeRoutes 钉住同厂商两条协议各写一种写法时互不影响：
// 整条地址那条落到自己的域名，相对路径那条照旧拼厂商的 base_url。
func TestForwardMixesFullAddressAndRelativeRoutes(t *testing.T) {
	// 一条协议用整条地址（另一个域名），另一条用相对路径（拼 base_url）。
	newSplit := func(t *testing.T) (*fixture, *uriUpstream, *uriUpstream) {
		t.Helper()
		f := newFixture(t)
		base := newURIUpstream(t, upstreamBodies[translate.ProtocolAnthropic])
		other := newURIUpstream(t, upstreamBodies[translate.ProtocolChat])
		if err := f.st.CreateProvider(store.Provider{Name: "split", BaseURL: base.url},
			[]store.ProviderRoute{
				{Protocol: translate.ProtocolChat, Path: other.url + "/v1/chat/completions"},
				{Protocol: translate.ProtocolAnthropic, Path: "/v1/messages"},
			}); err != nil {
			t.Fatalf("加厂商: %v", err)
		}
		return f, base, other
	}

	t.Run("整条地址那条不碰 base_url 域名", func(t *testing.T) {
		f, base, other := newSplit(t)
		const key = "sk-mixed-chat"
		f.addAs(t, "a1", "split", translate.ProtocolChat, base.url, key, []string{"m1"})

		gs, logs := newGatewayWith(t, f, gateway.Options{})
		srv := httptest.NewServer(gs.Handler())
		t.Cleanup(srv.Close)

		if status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat); status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		if other.hits != 1 || other.uri != "/v1/chat/completions" {
			t.Fatalf("整条地址上游收到 %d 次、URI %q，期望 1 次 /v1/chat/completions", other.hits, other.uri)
		}
		if base.hits != 0 {
			t.Fatalf("base_url 域名收到 %d 次请求，chat 路由是整条地址，不该拼它", base.hits)
		}
		if leaked := logs(); strings.Contains(leaked, key) {
			t.Fatalf("日志里出现了 key 明文: %s", leaked)
		}
	})

	t.Run("相对路径那条仍拼 base_url", func(t *testing.T) {
		f, base, other := newSplit(t)
		const key = "sk-mixed-anthropic"
		f.addAs(t, "a1", "split", translate.ProtocolAnthropic, base.url, key, []string{"m1"})

		gs, logs := newGatewayWith(t, f, gateway.Options{})
		srv := httptest.NewServer(gs.Handler())
		t.Cleanup(srv.Close)

		if status, body := postTo(t, srv, requestBodies[translate.ProtocolAnthropic], translate.ProtocolAnthropic); status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		if base.hits != 1 || base.uri != "/v1/messages" {
			t.Fatalf("base_url 上游收到 %d 次、URI %q，期望 1 次 /v1/messages", base.hits, base.uri)
		}
		if base.apiKey != key {
			t.Fatalf("x-api-key = %q，期望 anthropic 协议发账号凭据", base.apiKey)
		}
		if other.hits != 0 {
			t.Fatalf("整条地址那条不该被用到，实际收到 %d 次请求", other.hits)
		}
		if leaked := logs(); strings.Contains(leaked, key) {
			t.Fatalf("日志里出现了 key 明文: %s", leaked)
		}
	})
}

// TestSyncModelsFullAddressModelsPath 钉住整条地址的 models_path：
// 路由级与厂商级两种覆盖都能直接指到别的域名，使用时不再拼 base_url，查询串原样保留。
func TestSyncModelsFullAddressModelsPath(t *testing.T) {
	models := `{"object":"list","data":[{"id":"m-a"}]}`

	t.Run("路由级 models_path 是整条地址", func(t *testing.T) {
		f := newFixture(t)
		const key = "sk-models-route-address"
		base := newURIUpstream(t, models)
		lists := newURIUpstream(t, models)

		if err := f.st.CreateProvider(store.Provider{Name: "split", BaseURL: base.url},
			[]store.ProviderRoute{{
				Protocol:   translate.ProtocolChat,
				Path:       "/v1/chat/completions",
				ModelsPath: lists.url + "//v1//models/",
			}}); err != nil {
			t.Fatalf("加厂商: %v", err)
		}
		acc := f.addAs(t, "a1", "split", translate.ProtocolChat, base.url, key, nil)
		gs, logs := newGatewayWith(t, f, gateway.Options{})

		got, err := gs.SyncModels(context.Background(), acc)
		if err != nil {
			t.Fatalf("同步: %v", err)
		}
		if len(got) != 1 || got[0] != "m-a" {
			t.Fatalf("模型 = %v", got)
		}
		// 非 gemini 上游的列表地址原样使用，一个查询参数都不加（见 modelListPageURL），
		// 路径本身也不带多出来的段。
		if lists.hits != 1 || lists.uri != "/v1/models" {
			t.Fatalf("模型列表上游收到 %d 次、URI %q，期望 1 次 /v1/models", lists.hits, lists.uri)
		}
		if lists.bearer != "Bearer "+key || lists.apiKey != "" {
			t.Fatalf("凭据头 = Bearer %q / x-api-key %q，期望按 chat 路由发 Bearer", lists.bearer, lists.apiKey)
		}
		if base.hits != 0 {
			t.Fatalf("base_url 域名收到 %d 次请求，整条地址的 models_path 不该拼它", base.hits)
		}
		if leaked := logs(); strings.Contains(leaked, key) {
			t.Fatalf("日志里出现了 key 明文: %s", leaked)
		}
	})

	t.Run("厂商级 models_path 是带查询串的整条地址", func(t *testing.T) {
		f := newFixture(t)
		const key = "sk-models-provider-address"
		base := newURIUpstream(t, models)
		lists := newURIUpstream(t, models)

		if err := f.st.CreateProvider(store.Provider{Name: "split", BaseURL: base.url},
			[]store.ProviderRoute{{
				Protocol: translate.ProtocolChat,
				Path:     "/v1/chat/completions",
			}}); err != nil {
			t.Fatalf("加厂商: %v", err)
		}
		override := lists.url + "//v9//models/?api-version=2024-05-01"
		if err := f.st.SetProvider("split", store.ProviderPatch{ModelsPath: &override}); err != nil {
			t.Fatalf("写厂商级覆盖: %v", err)
		}
		acc := f.addAs(t, "a1", "split", translate.ProtocolChat, base.url, key, nil)
		gs, logs := newGatewayWith(t, f, gateway.Options{})

		if _, err := gs.SyncModels(context.Background(), acc); err != nil {
			t.Fatalf("同步: %v", err)
		}
		// 查询串原样保留：不重排、不重新转义、也不追加分页参数（非 gemini 上游不分页）。
		if want := "/v9/models?api-version=2024-05-01"; lists.uri != want {
			t.Fatalf("模型列表 RequestURI = %q，期望 %q（路径规整、查询串一个字节不改）", lists.uri, want)
		}
		if base.hits != 0 {
			t.Fatalf("base_url 域名收到 %d 次请求", base.hits)
		}
		if leaked := logs(); strings.Contains(leaked, key) {
			t.Fatalf("日志里出现了 key 明文: %s", leaked)
		}
	})
}

// TestSyncModelsWithEndpointFullAddress 钉住 `model sync --endpoint` 收整条地址：
// 给了就不再拼厂商 base_url，规整后直接访问；相对路径照旧拼 base_url，两种写法各管一次调用。
func TestSyncModelsWithEndpointFullAddress(t *testing.T) {
	f := newFixture(t)
	const key = "sk-endpoint-full-address"
	models := `{"object":"list","data":[{"id":"m-a"},{"id":"m-b"}]}`
	base := newURIUpstream(t, models)
	other := newURIUpstream(t, models)

	if err := f.st.CreateProvider(store.Provider{Name: "myco", BaseURL: base.url},
		[]store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v2/chat/completions"}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	acc := f.addAs(t, "a1", "myco", translate.ProtocolChat, base.url, key, nil)
	gs, logs := newGatewayWith(t, f, gateway.Options{})

	// 整条地址：打到另一个域名，路径规整、查询串保留。
	got, err := gs.SyncModelsWithEndpoint(context.Background(), acc,
		other.url+"//v1//model/?api-version=2024-05-01")
	if err != nil {
		t.Fatalf("同步: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("模型 = %v", got)
	}
	if want := "/v1/model?api-version=2024-05-01"; other.uri != want {
		t.Fatalf("RequestURI = %q，期望 %q", other.uri, want)
	}
	if other.bearer != "Bearer "+key {
		t.Fatalf("Authorization = %q，--endpoint 的凭据头按厂商默认协议发", other.bearer)
	}
	if base.hits != 0 {
		t.Fatalf("base_url 域名收到 %d 次请求，整条地址的 --endpoint 不该拼它", base.hits)
	}

	// 相对路径：仍拼厂商的 base_url，与升级前一致。
	if _, err := gs.SyncModelsWithEndpoint(context.Background(), acc, "v1/models"); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if base.uri != "/v1/models" || base.hits != 1 {
		t.Fatalf("相对路径的 RequestURI = %q（%d 次），期望 1 次 /v1/models", base.uri, base.hits)
	}
	if other.hits != 1 {
		t.Fatalf("相对路径那次不该碰另一个域名，实际总共 %d 次", other.hits)
	}
	if leaked := logs(); strings.Contains(leaked, key) {
		t.Fatalf("日志里出现了 key 明文: %s", leaked)
	}
}

// TestSyncModelsWithEndpointRejectsBadFullAddress 钉住读侧的拒绝边界：
// --endpoint 写成像样的整条地址才用，校验不过就报错，不退回 base_url 拼一个畸形地址，
// 也不悄悄改成相对路径。错误文案与日志都不带凭据明文。
func TestSyncModelsWithEndpointRejectsBadFullAddress(t *testing.T) {
	f := newFixture(t)
	const key = "sk-bad-endpoint-address"
	base := newURIUpstream(t, `{"data":[{"id":"m-a"}]}`)

	acc := f.addAs(t, "a1", "test", translate.ProtocolChat, base.url, key, nil)
	gs, logs := newGatewayWith(t, f, gateway.Options{})

	cases := []struct{ raw, want string }{
		{"https://", "主机名"},
		{"https:///v1/models", "主机名"},
		{"ftp://host/v1/models", "http:// 或 https://"},
	}
	for _, c := range cases {
		_, err := gs.SyncModelsWithEndpoint(context.Background(), acc, c.raw)
		if err == nil {
			t.Fatalf("--endpoint %q 应报错", c.raw)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("--endpoint %q 的错误 = %q，期望提到 %q", c.raw, err.Error(), c.want)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("错误信息里出现了 key 明文: %s", err.Error())
		}
	}
	if base.hits != 0 {
		t.Fatalf("取值为非法整条地址时不该发出上游请求，实际 %d 次", base.hits)
	}
	if leaked := logs(); strings.Contains(leaked, key) {
		t.Fatalf("日志里出现了 key 明文: %s", leaked)
	}
}

// TestSyncModelsNonGeminiKeepsModelsURL 钉住非 gemini 上游的模型列表地址一个字节都不改：
// 不追加 pageSize / pageToken，也不经 url.Parse 重编码查询串。带签名的 models_path
// （`?sig=…` 里的 %2F 这类转义）重排一次就失效，所以这里连转义写法一起断言。
func TestSyncModelsNonGeminiKeepsModelsURL(t *testing.T) {
	models := `{"object":"list","data":[{"id":"m-a"}]}`
	lists := newURIUpstream(t, models)

	f := newFixture(t)
	if err := f.st.CreateProvider(store.Provider{Name: "signed", BaseURL: lists.url},
		[]store.ProviderRoute{{
			Protocol:   translate.ProtocolChat,
			Path:       "/v1/chat/completions",
			ModelsPath: "/v1/models?api-version=2024-05-01&sig=a%2Fb%2Bc&expires=1700000000",
		}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	acc := f.addAs(t, "a1", "signed", translate.ProtocolChat, lists.url, shapeKey, nil)
	gs, logs := newGatewayWith(t, f, gateway.Options{})

	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步: %v", err)
	}
	want := "/v1/models?api-version=2024-05-01&sig=a%2Fb%2Bc&expires=1700000000"
	if lists.hits != 1 || lists.uri != want {
		t.Fatalf("模型列表上游收到 %d 次、RequestURI %q，期望 1 次 %q（原样，不加分页参数）",
			lists.hits, lists.uri, want)
	}
	mustNotLeak(t, shapeKey, nil, nil, logs())
}
