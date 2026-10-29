package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"switchboard/internal/gateway"
)

// catalogEntry 是 /v1/models 里的一项。四个字段都要，created 固定 0 是契约的一部分。
type catalogEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// gatewayServer 按当前沙箱的库与密钥起一个网关处理器：/v1/models 与三条入站路径都走
// 产品代码，用例只喂子命令与假上游，因此能验证「子命令回写的库 → /v1/models 的清单」
// 这一整条链，而不只是 store 里的某一列。
func gatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, key := mustStore(t)
	gs, err := gateway.New(gateway.Options{Store: st, Key: key})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	srv := httptest.NewServer(gs.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// mustCatalog 读 /v1/models，返回响应原文与解析结果：原文用来钉字段顺序与空数组，
// 解析结果用来比较整份清单。
func mustCatalog(t *testing.T, srv *httptest.Server) (string, []catalogEntry) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("请求 /v1/models: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models 状态 = %d，响应 %s", resp.StatusCode, raw)
	}
	var body struct {
		Object string         `json:"object"`
		Data   []catalogEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("清单不是合法 JSON: %v，原文 %s", err, raw)
	}
	if body.Object != "list" {
		t.Fatalf("object = %q，期望 list", body.Object)
	}
	return string(raw), body.Data
}

// postGateway 往某条入站路径发一条请求，返回状态与响应原文。
func postGateway(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// TestModelsCatalogFollowsModelProbe 确认清单与选号同源：两者都读 account_models 里
// access='allowed' 的行。model sync 把清单里的名字都标成 allowed，因此同步之后整份清单都在；
// 探活把 403 的那个改成 forbidden 之后清单只剩探通的那条，没探通的模型也接不到请求。
func TestModelsCatalogFollowsModelProbe(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusForbidden})
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "sync", "--account", "ds1"); code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	srv := gatewayServer(t)

	// 同步写的是「目录 + 这个账号可达性」：两个名字都是 allowed，清单因此列出这两条。
	synced := []catalogEntry{
		{ID: "m1", Object: "model", Created: 0, OwnedBy: "ds1-co"},
		{ID: "m2", Object: "model", Created: 0, OwnedBy: "ds1-co"},
	}
	if raw, got := mustCatalog(t, srv); !reflect.DeepEqual(got, synced) {
		t.Fatalf("同步之后清单 = %s，期望 %+v", raw, synced)
	}

	if code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("model probe 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}

	// owned_by 是账号所属厂商的名字：探活用例里每个账号一家自己的厂商（见 addProbeAccount）。
	want := []catalogEntry{{ID: "m1", Object: "model", Created: 0, OwnedBy: "ds1-co"}}
	if raw, got := mustCatalog(t, srv); !reflect.DeepEqual(got, want) {
		t.Fatalf("探活后清单 = %s，期望 %+v：只有探通的 m1，403 的 m2 不该在清单里", raw, want)
	}

	// 同源的另一半：清单里的 m1 能转发，探活时 403 的 m2 打过来是 404 而不是 503。
	if status, body := postGateway(t, srv, "/v1/chat/completions",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("m1 转发状态 = %d，响应 %s", status, body)
	}
	status, body := postGateway(t, srv, "/v1/chat/completions",
		`{"model":"m2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("清单里没有的 m2 状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "没有账号提供模型 m2") {
		t.Fatalf("m2 的错误文案 = %s，期望点名 m2", body)
	}
}

// TestModelsCatalogFollowsAccountEnableDisable 确认 account disable 之后该账号的模型从清单消失、
// account enable 之后回来：客户端因此不会把请求发到一个必然 503 的名字上。
func TestModelsCatalogFollowsAccountEnableDisable(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "sync", "--account", "ds1"); code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("model probe 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	srv := gatewayServer(t)

	// owned_by 是账号所属厂商的名字：探活用例里每个账号一家自己的厂商（见 addProbeAccount）。
	want := []catalogEntry{{ID: "m1", Object: "model", Created: 0, OwnedBy: "ds1-co"}}
	if raw, got := mustCatalog(t, srv); !reflect.DeepEqual(got, want) {
		t.Fatalf("停用前清单 = %s，期望 %+v", raw, want)
	}

	if code, _, stderr := run(t, "account", "disable", "ds1"); code != 0 {
		t.Fatalf("account disable 退出码 = %d，stderr = %s", code, stderr)
	}
	if raw, got := mustCatalog(t, srv); raw != `{"object":"list","data":[]}` || len(got) != 0 {
		t.Fatalf("停用后清单 = %s，期望空数组", raw)
	}
	// 代价也说清楚：名字虽然从清单里消失，客户端若照旧发过来，得到的是 503 而不是 404
	// ——账号确实声明过这个模型，只是这一次全都停用了（见 5.2 第 8 步）。
	status, body := postGateway(t, srv, "/v1/chat/completions",
		`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("停用后 m1 状态 = %d，响应 %s，期望 503", status, body)
	}
	if !strings.Contains(body, "都在冷却或被停用") {
		t.Fatalf("停用后 m1 的错误文案 = %s，期望说明账号被停用", body)
	}

	if code, _, stderr := run(t, "account", "enable", "ds1"); code != 0 {
		t.Fatalf("account enable 退出码 = %d，stderr = %s", code, stderr)
	}
	if raw, got := mustCatalog(t, srv); !reflect.DeepEqual(got, want) {
		t.Fatalf("启用后清单 = %s，期望 %+v", raw, want)
	}
}
