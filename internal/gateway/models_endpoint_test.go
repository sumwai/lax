package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// modelsServer 起一个假上游，记下每一次模型列表请求的路径与凭据头。
type modelsServer struct {
	url     string
	hits    int
	path    string
	apiKey  string
	bearer  string
	respond string
}

func newModelsServer(t *testing.T) *modelsServer {
	t.Helper()
	m := &modelsServer{respond: `{"object":"list","data":[{"id":"m-a"},{"id":"m-b"}]}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits++
		m.path = r.URL.Path
		m.apiKey = r.Header.Get("x-api-key")
		m.bearer = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, m.respond)
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

// TestSyncModelsAuthFollowsModelsPathRoute 钉住凭据头跟「实际取到 models_path 的那条路由」的协议走：
// deepseek 的 anthropic 账号取到的是 openai-chat 路由上的 /v1/models，就必须发 Bearer，
// 而不是厂商默认协议的 x-api-key——否则第一个真实账号就会在 /models 上换回 401/403，
// 错误还会指到「凭据无效」上。凭据不进错误文本。
func TestSyncModelsAuthFollowsModelsPathRoute(t *testing.T) {
	t.Run("回退到 chat 路由时用 Bearer", func(t *testing.T) {
		f := newFixture(t)
		up := newModelsServer(t)
		const key = "sk-deepseek-anthropic"
		acc := f.addAs(t, "ds-anthropic", "deepseek", gateway.ProtocolAnthropic, up.url, key, nil)
		gs := newGateway(t, f)

		models, err := gs.SyncModels(context.Background(), acc)
		if err != nil {
			t.Fatalf("同步: %v", err)
		}
		if len(models) != 2 || models[0] != "m-a" {
			t.Fatalf("模型 = %v", models)
		}
		if up.path != "/v1/models" {
			t.Fatalf("模型列表路径 = %q，期望回退到 chat 路由的 /v1/models", up.path)
		}
		if up.bearer != "Bearer "+key {
			t.Fatalf("Authorization = %q，期望按 chat 路由发 Bearer", up.bearer)
		}
		if up.apiKey != "" {
			t.Fatalf("不该发 x-api-key，实际 %q（地址来自 chat 路由）", up.apiKey)
		}
		if strings.Contains(up.bearer, key) == false {
			t.Fatalf("假上游应收到解出来的凭据")
		}
	})

	t.Run("走自己那条路由时用 x-api-key", func(t *testing.T) {
		f := newFixture(t)
		up := newModelsServer(t)
		const key = "sk-anthropic-own"
		// anthropic 厂商的 anthropic 路由自带 /v1/models，不必回退。
		acc := f.addAs(t, "claude", "anthropic", gateway.ProtocolAnthropic, up.url, key, nil)
		gs := newGateway(t, f)

		if _, err := gs.SyncModels(context.Background(), acc); err != nil {
			t.Fatalf("同步: %v", err)
		}
		if up.path != "/v1/models" {
			t.Fatalf("模型列表路径 = %q，期望 /v1/models", up.path)
		}
		if up.apiKey != key {
			t.Fatalf("x-api-key = %q，期望按 anthropic 路由发账号凭据", up.apiKey)
		}
		if up.bearer != "" {
			t.Fatalf("不该发 Authorization，实际 %q", up.bearer)
		}
	})
}

// TestSyncModelsProviderModelsPathOverride 确认厂商级的 models_path 覆盖压过路由里的值，
// 两者都只在这一次同步里生效，不改账号自己的 base_url。
func TestSyncModelsProviderModelsPathOverride(t *testing.T) {
	f := newFixture(t)
	up := newModelsServer(t)

	if err := f.st.CreateProvider(store.Provider{Name: "myco", BaseURL: "https://my.example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v2/chat/completions"}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	modelsPath := "/v3/models"
	if err := f.st.SetProvider("myco", store.ProviderPatch{ModelsPath: &modelsPath}); err != nil {
		t.Fatalf("写模型列表覆盖: %v", err)
	}

	acc := f.addAs(t, "a1", "myco", gateway.ProtocolChat, up.url, "sk-override", nil)
	gs := newGateway(t, f)

	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if up.path != "/v3/models" {
		t.Fatalf("模型列表路径 = %q，期望厂商级覆盖的 /v3/models（路由推导出来的是 /v2/models）", up.path)
	}

	// 覆盖清掉之后回到路由里的值：写入侧按路径推出来的 /v2/models。
	empty := ""
	if err := f.st.SetProvider("myco", store.ProviderPatch{ModelsPath: &empty}); err != nil {
		t.Fatalf("清模型列表覆盖: %v", err)
	}
	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if up.path != "/v2/models" {
		t.Fatalf("清掉覆盖后路径 = %q，期望路由里的 /v2/models", up.path)
	}
}

// TestSyncModelsOverrideAppliesWithoutRoutes 确认厂商级的 models_path 覆盖在一条路由都没有的
// 厂商上也生效：覆盖是厂商级设置，不依赖某条路由存在。
func TestSyncModelsOverrideAppliesWithoutRoutes(t *testing.T) {
	f := newFixture(t)
	up := newModelsServer(t)

	if err := f.st.AddProvider(store.Provider{Name: "noroute", BaseURL: "https://nr.example.com"}, nil); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	modelsPath := "/v9/models"
	if err := f.st.SetProvider("noroute", store.ProviderPatch{ModelsPath: &modelsPath}); err != nil {
		t.Fatalf("写模型列表覆盖: %v", err)
	}

	acc := f.addAs(t, "a1", "noroute", gateway.ProtocolChat, up.url, "sk-noroute", nil)
	gs := newGateway(t, f)
	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if up.path != "/v9/models" {
		t.Fatalf("模型列表路径 = %q，期望厂商覆盖的 /v9/models", up.path)
	}
}

// TestSyncModelsWithEndpointOverride 钉住 `model sync --endpoint <路径>`：
// 只影响这一次调用，压过厂商覆盖与路由。
// 路径写法是规整、不是用法错误：`v1/model`、`//v1//model/` 都会被打到规整后的 /v1/model。
func TestSyncModelsWithEndpointOverride(t *testing.T) {
	f := newFixture(t)
	up := newModelsServer(t)
	const key = "sk-endpoint-override"

	if err := f.st.CreateProvider(store.Provider{Name: "myco", BaseURL: "https://my.example.com"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v2/chat/completions"}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	modelsPath := "/v3/models"
	if err := f.st.SetProvider("myco", store.ProviderPatch{ModelsPath: &modelsPath}); err != nil {
		t.Fatalf("写模型列表覆盖: %v", err)
	}

	acc := f.addAs(t, "a1", "myco", gateway.ProtocolChat, up.url, key, nil)
	gs := newGateway(t, f)

	models, err := gs.SyncModelsWithEndpoint(context.Background(), acc, "/v1/model")
	if err != nil {
		t.Fatalf("同步: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("模型 = %v", models)
	}
	if up.path != "/v1/model" {
		t.Fatalf("模型列表路径 = %q，期望 --endpoint 给的 /v1/model", up.path)
	}
	if up.bearer != "Bearer "+key {
		t.Fatalf("Authorization = %q", up.bearer)
	}

	// 不规范的写法同样可用：没有前导斜杠、重复斜杠、末尾多一个斜杠都规整成同一个地址。
	if _, err := gs.SyncModelsWithEndpoint(context.Background(), acc, "//v1//model/"); err != nil {
		t.Fatalf("不规范路径不该报错: %v", err)
	}
	if up.path != "/v1/model" {
		t.Fatalf("不规范路径规整后的模型列表路径 = %q，期望 /v1/model", up.path)
	}
	if up.bearer != "Bearer "+key {
		t.Fatalf("Authorization = %q", up.bearer)
	}

	// 再跑一次不带 endpoint：回到厂商覆盖，说明覆盖只作用于那次调用。
	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if up.path != "/v3/models" {
		t.Fatalf("不带 --endpoint 时路径 = %q，期望厂商覆盖的 /v3/models", up.path)
	}
}

// TestSyncModelsNoModelsPathAnywhere 确认厂商声明了路由但一个非空 models_path 都没有时报错，
// 文案里给出两条出路：--models 手填，或 provider set --models-path。
func TestSyncModelsNoModelsPathAnywhere(t *testing.T) {
	f := newFixture(t)
	up := newModelsServer(t)

	// anthropic 路由不参与推导，所以这条路由的 models_path 是空串。
	if err := f.st.CreateProvider(store.Provider{Name: "nollm", BaseURL: "https://nollm.example.com"},
		[]store.ProviderRoute{{Protocol: "anthropic", Path: "/v1/messages"}}); err != nil {
		t.Fatalf("加厂商: %v", err)
	}
	acc := f.addAs(t, "a1", "nollm", gateway.ProtocolAnthropic, up.url, "sk-nollm", nil)
	gs := newGateway(t, f)

	_, err := gs.SyncModels(context.Background(), acc)
	if err == nil {
		t.Fatal("没有模型列表接口时应报错")
	}
	for _, want := range []string{"没有模型列表接口", "--models", "--models-path"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案 %q 缺少 %q", err.Error(), want)
		}
	}
	if up.hits != 0 {
		t.Fatalf("没有列表接口就不该发请求，实际 %d 次", up.hits)
	}
}
