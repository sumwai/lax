package cli_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 这个文件钉住种子厂商 google-gemini 在空库里的整条链路：init 之后 provider ls 就有它
// （gemini 路由、默认协议 gemini、路由级 models_path），account add --provider google-gemini
// 不带 --protocol 也落成 gemini 账号，model sync 打到种子的 /v1beta/models 并按页拉完，
// 模型名去掉 models/ 前缀、按 supportedGenerationMethods 过滤。上游是 httptest 假服务。

// geminiListStub 是假 google 的模型清单接口：按 pageToken 分页回答，记下每次的路径、
// 查询串与两个凭据头。
type geminiListStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	paths   []string
	queries []string
	googKey []string
	bearer  []string
}

func newGeminiListStub(t *testing.T) *geminiListStub {
	t.Helper()
	s := &geminiListStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.queries = append(s.queries, r.URL.RawQuery)
		s.googKey = append(s.googKey, r.Header.Get("x-goog-api-key"))
		s.bearer = append(s.bearer, r.Header.Get("Authorization"))
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			// 第一页：一条能用、一条只能做 embedding，nextPageToken 指向第二页。
			_, _ = io.WriteString(w, `{"models":[
				{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent","countTokens"]},
				{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}],
				"nextPageToken":"page-2"}`)
			return
		}
		// 第二页：没有游标，同步到此为止。
		_, _ = io.WriteString(w, `{"models":[
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}]}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *geminiListStub) calls() (paths, queries, googKey, bearer []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...), append([]string(nil), s.queries...),
		append([]string(nil), s.googKey...), append([]string(nil), s.bearer...)
}

// TestSeedGeminiProviderSyncsModels 逐条钉住种子厂商 google-gemini 在空库里的可用性。
func TestSeedGeminiProviderSyncsModels(t *testing.T) {
	sandbox(t)
	mustInit(t)
	up := newGeminiListStub(t)

	// 1. 空库里就有这家：默认协议 gemini，路由写相对路径且带占位符，
	//    模型清单路径是路由级的 /v1beta/models（厂商级没有覆盖）。
	seed := mustProviderLs(t)["google-gemini"]
	if seed.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("google-gemini 的 base_url = %q", seed.BaseURL)
	}
	if seed.DefaultProtocol != "gemini" {
		t.Fatalf("google-gemini 的默认协议 = %q，期望 gemini", seed.DefaultProtocol)
	}
	if seed.ModelsPath != "" {
		t.Fatalf("google-gemini 不该有厂商级 models_path 覆盖，实际 %q", seed.ModelsPath)
	}
	if len(seed.Protocols) != 1 {
		t.Fatalf("google-gemini 的路由 = %+v，期望只有 gemini 一条", seed.Protocols)
	}
	route := routeMap(seed)["gemini"]
	if route.Path != "/v1beta/models/{model}:generateContent" {
		t.Fatalf("gemini 路由的 path = %q，占位符应原样落库", route.Path)
	}
	if route.ModelsPath != "/v1beta/models" {
		t.Fatalf("gemini 路由的 models_path = %q，期望 /v1beta/models", route.ModelsPath)
	}

	// 2. account add --provider google-gemini：协议与地址都取厂商行（账号不再存这两项，
	//    见 DESIGN 5.8），默认协议是这家的 gemini，地址在下面指到假上游。
	code, out, stderr := addAccount(t, "gem", "google-gemini", up.srv.URL, "", "--key", "sk-gemini-1")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加 gem（google-gemini，gemini，凭据 sk-g…）"; !strings.Contains(out, want) {
		t.Fatalf("account add 的输出 = %s，期望包含 %q", out, want)
	}

	// 3. model sync 打到 /v1beta/models，第一页带 pageSize，第二页带上游给的游标，
	//    凭据是 x-goog-api-key（不是 Bearer）。
	code, out, stderr = run(t, "model", "sync", "--account", "gem", "--json")
	if code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	var synced []struct {
		Account string `json:"account"`
		Models  int    `json:"models"`
		Added   int    `json:"added"`
	}
	mustJSON(t, out, &synced)
	if len(synced) != 1 || synced[0].Account != "gem" || synced[0].Models != 3 || synced[0].Added != 3 {
		t.Fatalf("model sync = %+v，期望拉到 3 个模型、新增 3 个（含向量化那条）", synced)
	}

	paths, queries, googKey, bearer := up.calls()
	if len(paths) != 2 {
		t.Fatalf("模型清单请求次数 = %d，期望 2 页: %v", len(paths), paths)
	}
	for _, path := range paths {
		if path != "/v1beta/models" {
			t.Fatalf("模型清单路径 = %q，期望 %q", path, "/v1beta/models")
		}
	}
	wantQueries := []string{"pageSize=1000", "pageSize=1000&pageToken=page-2"}
	for i, want := range wantQueries {
		if queries[i] != want {
			t.Fatalf("第 %d 页的查询串 = %q，期望 %q", i+1, queries[i], want)
		}
	}
	for i := range googKey {
		if googKey[i] != "sk-gemini-1" {
			t.Fatalf("第 %d 页的 x-goog-api-key = %q", i+1, googKey[i])
		}
		if bearer[i] != "" {
			t.Fatalf("gemini 的清单请求不该发 Authorization，实际 %q", bearer[i])
		}
	}

	// 4. 落库的模型名是裸名：models/ 前缀去掉；只能做 embedding 的那条也进清单（DESIGN 5.15）。
	code, out, stderr = run(t, "model", "ls", "--account", "gem", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []modelRow
	mustJSON(t, out, &rows)
	var names []string
	for _, row := range rows {
		names = append(names, row.Model)
		if len(row.Accounts) != 1 || row.Accounts[0].Name != "gem" {
			t.Fatalf("模型 %s 的账号 = %+v", row.Model, row.Accounts)
		}
	}
	if got, want := strings.Join(names, ","),
		"gemini-2.5-flash,gemini-2.5-pro,text-embedding-004"; got != want {
		t.Fatalf("model ls 的模型 = %q，期望 %q（去前缀、排序，含 embedding）", got, want)
	}
}
