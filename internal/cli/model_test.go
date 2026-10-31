package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lax/internal/store"
	"lax/internal/store/storetest"
)

// probeUpstream 是假 openai-chat 上游：GET /v1/models 给模型列表，
// POST /v1/chat/completions 按请求体里的模型名决定返回 200 还是 failFor 里的状态码。
type probeUpstream struct {
	srv     *httptest.Server
	models  []string
	failFor map[string]int

	mu     sync.Mutex
	bodies []string
}

func newProbeUpstream(t *testing.T, models []string, failFor map[string]int) *probeUpstream {
	t.Helper()
	up := &probeUpstream{models: models, failFor: failFor}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"凭据不对"}}`)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			var payload struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			for _, m := range up.models {
				payload.Data = append(payload.Data, struct {
					ID string `json:"id"`
				}{ID: m})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			up.mu.Lock()
			up.bodies = append(up.bodies, string(body))
			up.mu.Unlock()

			var req struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			}
			_ = json.Unmarshal(body, &req)
			if code, ok := up.failFor[req.Model]; ok {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":{"message":"模型 `+req.Model+` 不可用"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`)

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"没有这个路由"}}`)
		}
	}))
	t.Cleanup(up.srv.Close)
	return up
}

// requestBodies 返回假上游收到的全部 POST 请求体。
func (u *probeUpstream) requestBodies() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.bodies))
	copy(out, u.bodies)
	return out
}

// probeRow 是 account probe --json 的一行。
type probeRow struct {
	Model      string `json:"model"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status"`
	LatencyMS  int64  `json:"latency_ms"`
	Message    string `json:"message"`
}

// accountRow 是 account ls --json 的一行，只取用例关心的字段。
type accountRow struct {
	Name         string   `json:"name"`
	Models       []string `json:"models"`
	Enabled      bool     `json:"enabled"`
	Status       string   `json:"status"`
	ModelsUsable int      `json:"models_usable"`
	ModelsSynced int      `json:"models_synced"`
}

// modelRow 是 model ls --json 的一行。
type modelRow struct {
	Model    string `json:"model"`
	Accounts []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"accounts"`
}

func mustJSON(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("输出不是 JSON: %v，%s", err, out)
	}
}

func mustProbeRows(t *testing.T, out string) map[string]probeRow {
	t.Helper()
	var rows []probeRow
	mustJSON(t, out, &rows)
	byModel := make(map[string]probeRow, len(rows))
	for _, r := range rows {
		byModel[r.Model] = r
	}
	return byModel
}

func mustAccountRow(t *testing.T, out string) accountRow {
	t.Helper()
	var rows []accountRow
	mustJSON(t, out, &rows)
	if len(rows) != 1 {
		t.Fatalf("account ls 应有 1 个账号，实际 %d 个: %s", len(rows), out)
	}
	return rows[0]
}

func mustModelRows(t *testing.T, out string) map[string]modelRow {
	t.Helper()
	var rows []modelRow
	mustJSON(t, out, &rows)
	byModel := make(map[string]modelRow, len(rows))
	for _, r := range rows {
		byModel[r.Model] = r
	}
	return byModel
}

// TestModelSyncEndpointOverride 覆盖 model sync --endpoint：
// 只影响这一次调用，地址取该厂商 base_url 拼上它，压过厂商级覆盖与路由里的 models_path；
// 下一次不带 --endpoint 时回到路由推导出来的地址。
// 路径写法不在断言之列：拼接改成程序规整，`custom/models`、`//custom//models` 都会被规整成
// 完整地址，不是用法错误。
func TestModelSyncEndpointOverride(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /custom/models": {status: http.StatusOK, body: `{"data":[{"id":"m1"},{"id":"m2"}]}`},
		"GET /v1/models":     {status: http.StatusOK, body: modelsListM1},
	})
	mustInit(t)
	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.invalid",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	// 账号地址指到假上游，厂商路由的 models_path 是 /v1/models。
	if code, _, stderr := addAccount(t, "mine1", "mine", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "sync", "--account", "mine1", "--endpoint", "/custom/models", "--json")
	if code != 0 {
		t.Fatalf("model sync --endpoint 退出码 = %d，stderr = %s", code, stderr)
	}
	var synced []struct {
		Account string `json:"account"`
		Models  int    `json:"models"`
		Added   int    `json:"added"`
	}
	mustJSON(t, out, &synced)
	if len(synced) != 1 || synced[0].Models != 2 || synced[0].Added != 2 {
		t.Fatalf("model sync --endpoint = %+v，期望拉到 2 个模型", synced)
	}
	calls := up.all()
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/custom/models" {
		t.Fatalf("上游调用 = %+v，期望只打一次 GET /custom/models", calls)
	}
	if calls[0].bearer != "Bearer sk-test" {
		t.Fatalf("凭据头 = %q，期望 Bearer sk-test", calls[0].bearer)
	}

	// 临时覆盖不外溢：下一次同步回到路由推导出来的 /v1/models。
	up.reset()
	if code, _, stderr := run(t, "model", "sync", "--account", "mine1"); code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	calls = up.all()
	if len(calls) != 1 || calls[0].path != "/v1/models" {
		t.Fatalf("不带 --endpoint 的调用 = %+v，期望回到 /v1/models", calls)
	}
}

// TestModelSyncEndpointNormalized 钉住 --endpoint 的写法只规整、不报错：
// `custom/models`、`//custom//models/`、`/custom/models/` 都打到同一个完整地址，退出码 0；
// 规整之后为空的取值（""、"//"）仍是用法错误，文案让人去掉这个参数。
func TestModelSyncEndpointNormalized(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /custom/models": {status: http.StatusOK, body: modelsListM1},
	})
	mustInit(t)
	if code, _, stderr := run(t, "provider", "add", "mine",
		"--base-url", "https://api.example.invalid",
		"--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "mine1", "mine", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	for _, endpoint := range []string{"custom/models", "//custom//models/", "/custom/models/"} {
		up.reset()
		code, _, stderr := run(t, "model", "sync", "--account", "mine1", "--endpoint", endpoint)
		if code != 0 {
			t.Fatalf("--endpoint %q 应被规整而不是报错，退出码 = %d，stderr = %s", endpoint, code, stderr)
		}
		calls := up.all()
		if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/custom/models" {
			t.Fatalf("--endpoint %q 的上游调用 = %+v，期望一次 GET /custom/models", endpoint, calls)
		}
	}

	for _, empty := range []string{"", "  ", "//"} {
		code, out, stderr := run(t, "model", "sync", "--account", "mine1", "--endpoint", empty)
		if code != 2 {
			t.Fatalf("--endpoint %q 的退出码 = %d，期望 2（用法错误）", empty, code)
		}
		if out != "" {
			t.Fatalf("用法错误不该写 stdout: %s", out)
		}
		for _, want := range []string{"--endpoint", "没有路径", "去掉这个参数"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr 应提到 %q: %s", want, stderr)
			}
		}
	}
}

// TestAccountSyncProbeFlow 覆盖 sync → probe → 可达性 → model ls 整条链路。
func TestAccountSyncProbeFlow(t *testing.T) {
	sandbox(t)
	// m1 可用，m2 上游返回 403。
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusForbidden})

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "sync", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	var synced []struct {
		Account  string `json:"account"`
		Provider string `json:"provider"`
		Models   int    `json:"models"`
		Added    int    `json:"added"`
		Error    string `json:"error"`
	}
	mustJSON(t, out, &synced)
	if len(synced) != 1 || synced[0].Account != "ds1" || synced[0].Models != 2 || synced[0].Added != 2 {
		t.Fatalf("model sync = %+v，期望一个账号 ds1 models=2 added=2", synced)
	}

	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("account probe 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	probes := mustProbeRows(t, out)
	if len(probes) != 2 {
		t.Fatalf("model probe 应探 2 个模型，实际 %d 个: %s", len(probes), out)
	}
	if r := probes["m1"]; r.Status != "usable" || r.HTTPStatus != http.StatusOK {
		t.Fatalf("m1 探活结果 = %+v，期望 usable/200", r)
	}
	if r := probes["m2"]; r.Status != "forbidden" || r.HTTPStatus != http.StatusForbidden {
		t.Fatalf("m2 探活结果 = %+v，期望 forbidden/403", r)
	}

	// 每个模型一条 max_tokens=1 的请求。
	bodies := up.requestBodies()
	if len(bodies) != len(probes) {
		t.Fatalf("假上游收到 %d 条探活请求，期望 %d 条", len(bodies), len(probes))
	}
	for _, body := range bodies {
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("探活请求体不是 JSON: %v，%s", err, body)
		}
		if req.MaxTokens != 1 {
			t.Fatalf("探活请求 max_tokens = %d，期望 1：%s", req.MaxTokens, body)
		}
		if req.Model != "m1" && req.Model != "m2" {
			t.Fatalf("探活请求 model = %q", req.Model)
		}
	}

	// 探活结论就是可达性：选号按 access='allowed' 走，m1 能用、m2 无权。
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	acc := mustAccountRow(t, out)
	if len(acc.Models) != 1 || acc.Models[0] != "m1" {
		t.Fatalf("account ls models = %v，期望只有 m1", acc.Models)
	}
	if acc.ModelsUsable != 1 || acc.ModelsSynced != 2 {
		t.Fatalf("account ls 模型计数 = 可用 %d / 已登记 %d，期望 1 / 2", acc.ModelsUsable, acc.ModelsSynced)
	}

	code, out, stderr = run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "1 可用 / 2 已登记") {
		t.Fatalf("account ls 文本输出缺少模型计数: %s", out)
	}

	// model ls 按模型汇总探活状态。
	code, out, stderr = run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	models := mustModelRows(t, out)
	if len(models) != 2 {
		t.Fatalf("model ls 应有 2 个模型，实际 %d 个: %s", len(models), out)
	}
	for model, want := range map[string]string{"m1": "allowed", "m2": "forbidden"} {
		row, ok := models[model]
		if !ok {
			t.Fatalf("model ls 缺模型 %s: %s", model, out)
		}
		if len(row.Accounts) != 1 || row.Accounts[0].Name != "ds1" || row.Accounts[0].Status != want {
			t.Fatalf("model ls 里 %s = %+v，期望 ds1(%s)", model, row.Accounts, want)
		}
	}

	// 人读输出：表格带账号与状态列，末尾给计数与写库结果。
	// 上一轮刚探过，结论在新鲜期内，这里用 --refresh 强制重探才有表格。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--refresh")
	if code != 0 {
		t.Fatalf("model probe 文本输出退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"账号", "模型", "本次探活 2 个", "跳过 0 个", "可用 1", "无权 1", "可达 1 个"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model probe 文本输出缺少 %q: %s", want, out)
		}
	}
}

// TestModelProbeNoProgressOutsideTerminal 钉住实时进度的开关：测试里 stderr 是
// bytes.Buffer，不是字符设备，进度行必须一个字节都不出；人读的汇总行与表格形状、
// --json 的 stdout 都不受影响。
func TestModelProbeNoProgressOutsideTerminal(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 人读路径：进度不开，汇总行与表格照旧。
	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--concurrency", "2")
	if code != 0 {
		t.Fatalf("model probe 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(stderr, "\r") || strings.Contains(stderr, "探活中") {
		t.Fatalf("非终端不该打实时进度，stderr = %q", stderr)
	}
	for _, want := range []string{"账号", "模型", "本次探活 2 个", "跳过 0 个", "可达 2 个"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model probe 文本输出缺少 %q: %s", want, out)
		}
	}

	// --json 路径：stdout 只有 JSON，stderr 同样没有进度。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--refresh", "--json")
	if code != 0 {
		t.Fatalf("model probe --json 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(stderr, "\r") || strings.Contains(stderr, "探活中") {
		t.Fatalf("--json 不该打实时进度，stderr = %q", stderr)
	}
	if probes := mustProbeRows(t, out); len(probes) != 2 {
		t.Fatalf("model probe --json 应给出 2 个模型，实际 %d 个: %s", len(probes), out)
	}

	// 待探模型全被跳过（结论仍在新鲜期内）时不画进度行，也不报错。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("全部跳过时退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(stderr, "\r") || strings.Contains(stderr, "探活中") {
		t.Fatalf("全部跳过的账号不该有进度行，stderr = %q", stderr)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("全部跳过时 --json = %q，期望空数组", got)
	}
}

// TestAccountProbeAllForbiddenRecordsAccess 确认零可用时退出码 1，且结论照样落库：
// 探不动的名字从 allowed 变成 forbidden，行不删、也不再进候选。
func TestAccountProbeAllForbiddenRecordsAccess(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, nil, map[string]int{"keep-me": http.StatusForbidden})

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test", "--models", "keep-me"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 1 {
		t.Fatalf("全部 403 时 model probe 退出码 = %d，期望 1；stdout = %s，stderr = %s", code, out, stderr)
	}
	for _, want := range []string{"探出可用模型", "可达性已记录", "forbidden / missing"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应说明结论落在可达性上、%q 不进候选: %s", want, stderr)
		}
	}
	probes := mustProbeRows(t, out)
	if len(probes) != 1 || probes["keep-me"].Status != "forbidden" {
		t.Fatalf("探活结果 = %+v，期望 keep-me 是 forbidden", probes)
	}

	// 行还在，可达性改成 forbidden 并记了时间：下一次不用重探就能跳过。
	st := storetest.Open(t)
	if row := st.ModelRow(t, "ds1", "keep-me"); row.Access != store.AccessForbidden || row.CheckedAt == 0 {
		t.Fatalf("可达性没落库：%+v，期望 forbidden 且记了探活时间", row)
	}

	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	acc := mustAccountRow(t, out)
	if acc.ModelsUsable != 0 || acc.ModelsSynced != 1 || len(acc.Models) != 0 {
		t.Fatalf("零可用的账号 = %+v，期望 models 为空、可用 0、已登记 1", acc)
	}
	if !acc.Enabled || acc.Status != "正常" {
		t.Fatalf("403 不该停用账号：enabled = %v，status = %s", acc.Enabled, acc.Status)
	}
}

// TestAccountProbeAuthFailureDisablesAccount 确认凭据被拒时账号停用、后续账号不再处理，
// 已经探完的那几个模型的可达性照样留库（auth 落成 error，下一轮重探）。
func TestAccountProbeAuthFailureDisablesAccount(t *testing.T) {
	sandbox(t)
	// m1 能通，m2 回 401：凭据不行了，整次探活就停在这里。
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusUnauthorized})

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 1 {
		t.Fatalf("凭据被拒时 model probe 退出码 = %d，期望 1；stdout = %s，stderr = %s", code, out, stderr)
	}
	if !strings.Contains(stderr, "凭据") || !strings.Contains(stderr, "已停用") {
		t.Fatalf("stderr 应说明凭据被拒且账号已停用: %s", stderr)
	}
	if probes := mustProbeRows(t, out); probes["m2"].Status != "auth" {
		t.Fatalf("m2 探活结果 = %+v，期望 auth", probes["m2"])
	}

	// 探完的那两条各自落库：m1 能用，m2 是没定论的 error（凭据问题不是模型层面的结论）。
	st := storetest.Open(t)
	if row := st.ModelRow(t, "ds1", "m1"); row.Access != store.AccessAllowed {
		t.Fatalf("m1 的可达性 = %+v，期望 allowed", row)
	}
	if row := st.ModelRow(t, "ds1", "m2"); row.Access != store.AccessError {
		t.Fatalf("m2 的可达性 = %+v，期望 error（下一轮重探）", row)
	}

	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	acc := mustAccountRow(t, out)
	if acc.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	if len(acc.Models) != 1 || acc.Models[0] != "m1" {
		t.Fatalf("可用模型 = %v，期望只剩探通的 m1", acc.Models)
	}
}

// modelRequests 数出假上游收到的探活请求里各模型各被探了几次。
// 只看输出行会漏掉「发了请求但结果被截断丢掉」这类问题，所以要数请求。
func modelRequests(t *testing.T, up *probeUpstream) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, body := range up.requestBodies() {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("探活请求体不是 JSON: %v，%s", err, body)
		}
		out[req.Model]++
	}
	return out
}

// probeVerdictRow 从探活写下的记录里挑出某个模型的判定行：错误摘要不带换协议前缀的那一行
// （换协议前的中间尝试也各记一行，见 gateway.recordProbeAttempts）。
func probeVerdictRow(t *testing.T, recs []store.RequestRecord, model string) store.RequestRecord {
	t.Helper()
	for _, r := range recs {
		if r.ModelUsed == model && !strings.Contains(r.Error, "换协议前的尝试") {
			return r
		}
	}
	t.Fatalf("请求记录里没有模型 %s 的判定行: %+v", model, recs)
	return store.RequestRecord{}
}

// addProbeAccount 初始化数据库，加一个指向假上游的账号。地址只在厂商行上，
// 所以先按账号名登记一家厂商，再建账号：每个账号一家厂商，两个账号指向两个假上游时
// 才不会被后写的地址一起改掉。
func addProbeAccount(t *testing.T, up *probeUpstream, name string, extra ...string) {
	t.Helper()
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	provider := name + "-co"
	// 三条非 gemini 路由都声明：探活的候选协议跟着路由收紧，
	// 只声明一条时 403 之后就没有别的协议可换，探活行数会跟着变。
	ensureProvider(t, provider, up.srv.URL,
		"--openai", "/v1/chat/completions",
		"--anthropic", "/v1/messages",
		"--responses", "/v1/responses")
	args := append([]string{"account", "add", name, "--provider", provider, "--key", "sk-test"}, extra...)
	if code, _, stderr := run(t, args...); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
}

// TestModelProbeSkipsFreshModels 确认有定论的结论在新鲜期内被跳过，且确实没发上游请求。
func TestModelProbeSkipsFreshModels(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2", "m3"}, map[string]int{
		"m2": http.StatusForbidden,
		"m3": http.StatusNotFound,
	})
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "sync", "--account", "ds1"); code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("首次探活退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	if rows := mustProbeRows(t, out); len(rows) != 3 {
		t.Fatalf("首次探活应有 3 行，实际 %d 行: %s", len(rows), out)
	}
	if got := len(up.requestBodies()); got != 3 {
		t.Fatalf("假上游收到 %d 条探活请求，期望 3 条", got)
	}

	// 可用、无权、不存在三种结论都在新鲜期内，第二轮全部跳过。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("全部跳过时退出码 = %d，期望 0；stdout = %s，stderr = %s", code, out, stderr)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("全部跳过时 --json 应是空数组，实际 %s", got)
	}
	if got := len(up.requestBodies()); got != 3 {
		t.Fatalf("跳过的模型不该发上游请求，累计请求数 %d，期望 3", got)
	}

	// 人读输出能看出本次探了几个、跳过几个。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1")
	if code != 0 {
		t.Fatalf("文本输出退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "本次探活 0 个，跳过 3 个") {
		t.Fatalf("文本输出应给出探活与跳过的数量: %s", out)
	}
}

// TestModelProbeRefreshForcesReprobe 确认 --refresh 与 --max-age 0 忽略新鲜度。
func TestModelProbeRefreshForcesReprobe(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}
	if got := len(up.requestBodies()); got != 2 {
		t.Fatalf("首次探活请求数 = %d，期望 2", got)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json", "--refresh")
	if code != 0 {
		t.Fatalf("--refresh 退出码 = %d，stderr = %s", code, stderr)
	}
	if rows := mustProbeRows(t, out); len(rows) != 2 {
		t.Fatalf("--refresh 应重探 2 个模型，实际 %d 个: %s", len(rows), out)
	}
	if got := len(up.requestBodies()); got != 4 {
		t.Fatalf("--refresh 后请求数 = %d，期望 4", got)
	}

	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--json", "--max-age", "0")
	if code != 0 {
		t.Fatalf("--max-age 0 退出码 = %d，stderr = %s", code, stderr)
	}
	if rows := mustProbeRows(t, out); len(rows) != 2 {
		t.Fatalf("--max-age 0 应等价于 --refresh，实际探了 %d 个: %s", len(rows), out)
	}
	if got := len(up.requestBodies()); got != 6 {
		t.Fatalf("--max-age 0 后请求数 = %d，期望 6", got)
	}
}

// TestModelProbeMaxAgeExpiry 确认超过 --max-age 的模型会重探，未超的仍跳过。
func TestModelProbeMaxAgeExpiry(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	st.BackdateModel(t, "ds1", "m1", time.Now().Add(-2*time.Hour).Unix())

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json", "--max-age", "1h")
	if code != 0 {
		t.Fatalf("超期重探退出码 = %d，stderr = %s", code, stderr)
	}
	rows := mustProbeRows(t, out)
	if len(rows) != 1 || rows["m1"].Status != "usable" {
		t.Fatalf("超过 --max-age 的 m1 应被重探，m2 仍新鲜应跳过，实际 %s", out)
	}
	if got := len(up.requestBodies()); got != 3 {
		t.Fatalf("请求数 = %d，期望 3（首次 2 加超期重探 1）", got)
	}
}

// TestModelProbeSkipDoesNotTouchCheckedAt 确认跳过那一轮真的不写库：
// 被跳过的行，checked_at 与 status 与跳过前完全一致。
// 没有这条断言，跳过路径若误调 SetModelStatus，结论会被刷新成「刚探过」，
// 下一轮接着跳过，一条本来该重探的结论就永远停在旧值上。
func TestModelProbeSkipDoesNotTouchCheckedAt(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	// 把两条结论拨到一个可辨认的过去时间点，仍在 24h 窗口内。
	// 这样「没写库」是可验证的：写库会把它改成当前时间。
	st := storetest.Open(t)
	pinned := time.Now().Add(-time.Hour).Unix()
	st.BackdateModel(t, "ds1", "m1", pinned)
	st.BackdateModel(t, "ds1", "m2", pinned)

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("跳过轮次退出码 = %d，期望 0；stdout = %s，stderr = %s", code, out, stderr)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("两个结论都新鲜，本轮应全部跳过，实际 %s", got)
	}

	for _, model := range []string{"m1", "m2"} {
		row := st.ModelRow(t, "ds1", model)
		if row.CheckedAt != pinned {
			t.Fatalf("%s 被跳过了，checked_at 却变了：%d，期望保持 %d", model, row.CheckedAt, pinned)
		}
		if row.Status != store.ModelUsable {
			t.Fatalf("%s 被跳过了，status 却变了：%q，期望保持 %q", model, row.Status, store.ModelUsable)
		}
	}
	if got := modelRequests(t, up); got["m1"] != 1 || got["m2"] != 1 || len(got) != 2 {
		t.Fatalf("跳过的模型不该发上游请求，本轮统计 %v，期望 m1、m2 各 1 次", got)
	}
}

// TestModelProbeReprobesUsableWithZeroCheckedAt 确认「有定论但没记时间」不算新鲜。
// checked_at 为 0 的行可能来自手工导入、account add --models 或旧库升级，没有时间就无法
// 判断结论是不是一天前的，必须照常重探。
func TestModelProbeReprobesUsableWithZeroCheckedAt(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	// 只把 m1 的时间戳归零，可达性仍是 allowed；m2 保持新鲜做对照。
	st := storetest.Open(t)
	st.BackdateModel(t, "ds1", "m1", 0)
	if row := st.ModelRow(t, "ds1", "m1"); row.Access != store.AccessAllowed || row.CheckedAt != 0 {
		t.Fatalf("前置条件不对（期望 allowed 且没记时间）：%+v", row)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	rows := mustProbeRows(t, out)
	if len(rows) != 1 || rows["m1"].Status != "usable" {
		t.Fatalf("checked_at 为 0 的 m1 必须重探，m2 仍新鲜应跳过，实际 %s", out)
	}
	if got := modelRequests(t, up); got["m1"] != 2 || got["m2"] != 1 || len(got) != 2 {
		t.Fatalf("上游请求统计 = %v，期望 m1 探 2 次、m2 探 1 次", got)
	}
	if row := st.ModelRow(t, "ds1", "m1"); row.CheckedAt == 0 {
		t.Fatal("重探之后应写回当前时间戳")
	}
}

// TestModelProbeDefaultMaxAgeWindow 确认默认 --max-age 24h 的两侧：
// 25 小时前的结论必须重探，23 小时前的必须跳过。只断言「默认值大于 0」
// 挡不住窗口被改成 1h 或 30 天这类回归。
func TestModelProbeDefaultMaxAgeWindow(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m-23h", "m-25h"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	now := time.Now()
	st.BackdateModel(t, "ds1", "m-23h", now.Add(-23*time.Hour).Unix())
	st.BackdateModel(t, "ds1", "m-25h", now.Add(-25*time.Hour).Unix())

	// 不给 --max-age，走的就是默认值。
	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	rows := mustProbeRows(t, out)
	if len(rows) != 1 || rows["m-25h"].Status != "usable" {
		t.Fatalf("只有 25 小时前的 m-25h 该重探，实际 %s", out)
	}
	if got := modelRequests(t, up); got["m-25h"] != 2 || got["m-23h"] != 1 || len(got) != 2 {
		t.Fatalf("上游请求统计 = %v，期望 m-25h 探 2 次、m-23h 探 1 次（默认窗口 24h）", got)
	}
}

// TestModelProbeSummaryMixedProbeAndSkip 确认人读汇总在「探一个、跳一个」
// 的混合情形下两组计数都对：只有 0/3 与 2/0 两个极端覆盖不到中间的取数错误。
func TestModelProbeSummaryMixedProbeAndSkip(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m-new", "m-old"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	now := time.Now()
	st.BackdateModel(t, "ds1", "m-old", now.Add(-25*time.Hour).Unix())
	st.BackdateModel(t, "ds1", "m-new", now.Add(-time.Hour).Unix())

	code, out, stderr := run(t, "model", "probe", "--account", "ds1")
	if code != 0 {
		t.Fatalf("退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	want := "ds1：本次探活 1 个，跳过 1 个，可用 1，无权 0，不存在 0，失败 0；可达 2 个"
	if !strings.Contains(out, want) {
		t.Fatalf("汇总行缺 %q: %s", want, out)
	}
	// 跳过的不出现在输出里，一行表格只该有 m-old。
	if !strings.Contains(out, "m-old") {
		t.Fatalf("重探的 m-old 应出现在表格里: %s", out)
	}
	if strings.Contains(out, "m-new") {
		t.Fatalf("跳过的 m-new 不该出现在输出里: %s", out)
	}
}

// TestModelProbeModelFlagRespectsFreshness 确认新鲜度对 --model 也生效，--refresh 可强制重探。
func TestModelProbeModelFlagRespectsFreshness(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--model", "m1", "--json")
	if code != 0 {
		t.Fatalf("--model 退出码 = %d，stderr = %s", code, stderr)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("--model 指定的模型刚探过，应跳过，实际 %s", got)
	}
	if got := len(up.requestBodies()); got != 2 {
		t.Fatalf("跳过的单模型不该发请求，请求数 = %d", got)
	}

	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--model", "m1", "--json", "--refresh")
	if code != 0 {
		t.Fatalf("--model --refresh 退出码 = %d，stderr = %s", code, stderr)
	}
	if rows := mustProbeRows(t, out); len(rows) != 1 || rows["m1"].Status != "usable" {
		t.Fatalf("--refresh 应重探 m1，实际 %s", out)
	}
	if got := len(up.requestBodies()); got != 3 {
		t.Fatalf("--refresh 后请求数 = %d，期望 3", got)
	}
}

// TestModelProbeDoesNotSkipUnsettledAccess 确认只有「有定论且记了时间」才跳过：
// access 为 error 的行、以及 allowed 但 checked_at 为 0 的行都要照常探。
func TestModelProbeDoesNotSkipUnsettledAccess(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, nil, nil)
	addProbeAccount(t, up, "ds1")

	st := storetest.Open(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	now := time.Now().Unix()
	// 三条里只有 m-settled 同时满足两个条件，它应当被跳过；另两条各缺一个条件。
	for model, row := range map[string]struct {
		access    string
		httpCode  int
		checkedAt int64
	}{
		"m-settled":  {store.AccessAllowed, http.StatusOK, now},
		"m-error":    {store.AccessError, http.StatusInternalServerError, now},
		"m-no-stamp": {store.AccessAllowed, http.StatusOK, 0},
	} {
		if err := st.SetAccountModelAccess(acc.ID, model, row.access, row.httpCode, "上一轮的结论", row.checkedAt); err != nil {
			t.Fatalf("写 %s 的可达性: %v", model, err)
		}
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	rows := mustProbeRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("error 与 allowed 但没记时间的两行都该重探，实际探了 %d 个: %s", len(rows), out)
	}
	if _, ok := rows["m-settled"]; ok {
		t.Fatalf("有定论且新鲜的 m-settled 不该被重探: %s", out)
	}
	if got := len(up.requestBodies()); got != 2 {
		t.Fatalf("假上游收到 %d 条请求，期望 2 条", got)
	}
}

// TestModelProbeAllSkippedKeepsAccess 确认全被跳过那一轮一条上游请求都不发、也不动可达性：
// 结论仍在 --max-age 窗口内，库里那份 access 就是选号依据，不必再回写一份清单。
func TestModelProbeAllSkippedKeepsAccess(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "sync", "--account", "ds1"); code != 0 {
		t.Fatalf("model sync 退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	// 两条结论都定好了、都落在 24h 窗口内：这一轮应当全部跳过。
	pinned := time.Now().Add(-time.Hour).Unix()
	want := map[string]string{"m1": store.AccessAllowed, "m2": store.AccessForbidden}
	for model, access := range want {
		if err := st.SetAccountModelAccess(acc.ID, model, access, http.StatusOK, "", pinned); err != nil {
			t.Fatalf("写 %s 的可达性: %v", model, err)
		}
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("全部跳过时退出码 = %d，期望 0；stdout = %s，stderr = %s", code, out, stderr)
	}
	if strings.Contains(stderr, "没有账号探出可用模型") {
		t.Fatalf("全部跳过不该落到没有可用模型的错误分支: %s", stderr)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("全部跳过时 --json 应是空数组，实际 %s", got)
	}
	if got := len(up.requestBodies()); got != 0 {
		t.Fatalf("跳过的一轮不该发探活请求，实际 %d 条", got)
	}
	for model, access := range want {
		row := st.ModelRow(t, "ds1", model)
		if row.Access != access || row.CheckedAt != pinned {
			t.Fatalf("%s 被跳过了，库里却变了：%+v，期望 access %q、checked_at %d", model, row, access, pinned)
		}
	}
	after, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if len(after.Models) != 1 || after.Models[0] != "m1" {
		t.Fatalf("选号集合应只有 allowed 的 m1，实际 %v", after.Models)
	}
}

// TestModelProbeMixedAccountsExitCode 盯多账号的退出码：报错与否只看终态，
// 整次命令退 0 的条件是库里至少一个账号有 usable，而不是每个账号都探出了东西。
func TestModelProbeMixedAccountsExitCode(t *testing.T) {
	t.Run("一号全跳过二号真实探活", func(t *testing.T) {
		sandbox(t)
		skippedUp := newProbeUpstream(t, []string{"m-a"}, nil)
		liveUp := newProbeUpstream(t, []string{"m-b"}, nil)
		addProbeAccount(t, skippedUp, "a-skip")
		addProbeAccount(t, liveUp, "b-live")

		if code, _, stderr := run(t, "model", "probe", "--json"); code != 0 {
			t.Fatalf("首轮探活退出码 = %d，stderr = %s", code, stderr)
		}
		if got := modelRequests(t, skippedUp); got["m-a"] != 1 || len(got) != 1 {
			t.Fatalf("首轮 a-skip 的请求统计 = %v", got)
		}

		// 只让二号的结论过期：一号继续全跳过，二号要真探一次。
		st := storetest.Open(t)
		st.BackdateModel(t, "b-live", "m-b", time.Now().Add(-25*time.Hour).Unix())

		code, out, stderr := run(t, "model", "probe", "--json")
		if code != 0 {
			t.Fatalf("一号全跳过、二号探出可用时退出码 = %d，期望 0；stdout = %s，stderr = %s", code, out, stderr)
		}
		if stderr != "" {
			t.Fatalf("这种情况不该有错误信息: %s", stderr)
		}
		rows := mustProbeRows(t, out)
		if len(rows) != 1 || rows["m-b"].Status != "usable" {
			t.Fatalf("本轮只该探 b-live 的 m-b，实际 %s", out)
		}
		if got := modelRequests(t, skippedUp); got["m-a"] != 1 || len(got) != 1 {
			t.Fatalf("a-skip 的结论新鲜，不该再发请求，实际统计 %v", got)
		}
		if got := modelRequests(t, liveUp); got["m-b"] != 2 || len(got) != 1 {
			t.Fatalf("b-live 的结论过期，应重探一次，实际统计 %v", got)
		}
	})

	t.Run("一号全跳过且库里零可用另一号有可用", func(t *testing.T) {
		sandbox(t)
		// 一号手填的那个名字固定回 403：有定论、新鲜，但库里零可用。
		noneUp := newProbeUpstream(t, nil, map[string]int{"keep-me": http.StatusForbidden})
		liveUp := newProbeUpstream(t, []string{"m-b"}, nil)
		addProbeAccount(t, noneUp, "a-none", "--models", "keep-me")
		addProbeAccount(t, liveUp, "b-live")

		if code, _, stderr := run(t, "model", "probe", "--json"); code != 0 {
			t.Fatalf("首轮探活退出码 = %d，期望 0（b-live 探出可用）；stderr = %s", code, stderr)
		}

		// 只让二号过期：一号全跳过且零可用，二号真探一次。
		st := storetest.Open(t)
		st.BackdateModel(t, "b-live", "m-b", time.Now().Add(-25*time.Hour).Unix())

		code, out, stderr := run(t, "model", "probe")
		if code != 0 {
			t.Fatalf("退出码 = %d，期望 0（终态至少一个账号有可用）；stdout = %s，stderr = %s", code, out, stderr)
		}
		if strings.Contains(stderr, "没有账号探出可用模型") {
			t.Fatalf("至少一个账号有可用，不该落到零可用的错误分支: %s", stderr)
		}
		// 一号零可用但不报错，输出里说清可达性已经记录、forbidden / missing 不进候选；
		// 二号是真探的。
		for _, want := range []string{
			"a-none：本次探活 0 个，跳过 1 个，库里没有可用模型；可达性已记录，access 为 forbidden / missing 的模型不会进候选",
			"b-live：本次探活 1 个，跳过 0 个，可用 1",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("人读输出缺 %q: %s", want, out)
			}
		}

		// 一号那条手填的名字留库、可达性停在 forbidden：行不删，但不再进候选。
		none, err := st.GetAccount("a-none")
		if err != nil {
			t.Fatalf("读账号: %v", err)
		}
		if len(none.Models) != 0 {
			t.Fatalf("零可用账号的选号集合应为空，实际 %v", none.Models)
		}
		if row := st.ModelRow(t, "a-none", "keep-me"); row.Access != store.AccessForbidden {
			t.Fatalf("keep-me 的可达性 = %+v，期望 forbidden", row)
		}
		live, err := st.GetAccount("b-live")
		if err != nil {
			t.Fatalf("读账号: %v", err)
		}
		if len(live.Models) != 1 || live.Models[0] != "m-b" {
			t.Fatalf("可用账号的选号集合应是 m-b，实际 %v", live.Models)
		}
	})
}

// TestModelProbeWritesRequestRows 确认探活写请求记录，跳过的模型不写，且进 usage 汇总。
func TestModelProbeWritesRequestRows(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusForbidden})
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	st := storetest.Open(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	recs, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	// 每次上游调用各记一行：m1 一次成功；m2 的 403 触发换协议，三次调用三行。
	if len(recs) != 4 {
		t.Fatalf("探活应写 4 行请求记录（打了几次就记几行），实际 %d 行", len(recs))
	}
	ok := probeVerdictRow(t, recs, "m1")
	if ok.Inbound != "openai-chat" || ok.Outbound != "openai-chat" {
		t.Fatalf("探活行的入站与出站都应取厂商默认协议，实际 %s→%s", ok.Inbound, ok.Outbound)
	}
	if ok.ModelRequested != "m1" || ok.AccountID != acc.ID {
		t.Fatalf("探活行的模型与账号不对: %+v", ok)
	}
	if ok.HTTPStatus != http.StatusOK || !ok.OK {
		t.Fatalf("m1 探活行应是 200 且 ok: %+v", ok)
	}
	if ok.InTokens != 7 || ok.OutTokens != 1 {
		t.Fatalf("m1 的 token 应来自响应体 usage，实际输入 %d 输出 %d", ok.InTokens, ok.OutTokens)
	}
	bad := probeVerdictRow(t, recs, "m2")
	if bad.HTTPStatus != http.StatusForbidden || bad.OK {
		t.Fatalf("m2 探活行应是 403 且不 ok: %+v", bad)
	}
	if bad.InTokens != 0 || bad.OutTokens != 0 {
		t.Fatalf("非 2xx 的 token 应记 0，实际输入 %d 输出 %d", bad.InTokens, bad.OutTokens)
	}
	if bad.Error == "" {
		t.Fatal("非 2xx 的探活行应记下错误摘要")
	}

	// 第二轮全部跳过，不该再多出请求记录。
	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("第二轮探活退出码 = %d，stderr = %s", code, stderr)
	}
	recs, err = st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("跳过的模型不该写请求记录，累计 %d 行", len(recs))
	}

	// 探活的 token 进 usage 汇总。
	code, out, stderr := run(t, "usage", "--since", "1d", "--json")
	if code != 0 {
		t.Fatalf("usage 退出码 = %d，stderr = %s", code, stderr)
	}
	var usageRows []struct {
		Key       string `json:"key"`
		Requests  int64  `json:"requests"`
		InTokens  int64  `json:"in_tokens"`
		OutTokens int64  `json:"out_tokens"`
	}
	mustJSON(t, out, &usageRows)
	if len(usageRows) != 1 || usageRows[0].Key != "ds1" {
		t.Fatalf("usage 应有账号 ds1 一行，实际 %s", out)
	}
	if usageRows[0].Requests != 4 || usageRows[0].InTokens != 7 || usageRows[0].OutTokens != 1 {
		t.Fatalf("usage 汇总未包含探活调用: %s", out)
	}
}

// TestModelProbeLimitCountsPendingOnly 确认 --limit 截断的是跳过之后剩下的待探模型。
func TestModelProbeLimitCountsPendingOnly(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2", "m3", "m4"}, nil)
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	// m1、m2 仍新鲜，m3、m4 超期。--limit 1 应当落在 m3 而不是被跳过的 m1。
	st := storetest.Open(t)
	stale := time.Now().Add(-2 * time.Hour).Unix()
	st.BackdateModel(t, "ds1", "m3", stale)
	st.BackdateModel(t, "ds1", "m4", stale)

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json", "--max-age", "1h", "--limit", "1")
	if code != 0 {
		t.Fatalf("--limit 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	rows := mustProbeRows(t, out)
	if len(rows) != 1 || rows["m3"].Status != "usable" {
		t.Fatalf("--limit 1 应只探待探的 m3，实际 %s", out)
	}
	// 输出行只能证明「没写结果」，数请求才能证明被截断的模型真的没花钱：
	// m1、m2 被跳过，m3 落在额度里，m4 被 --limit 截掉，一条上游请求都不该有。
	got := modelRequests(t, up)
	want := map[string]int{"m1": 1, "m2": 1, "m3": 2, "m4": 1}
	if len(got) != len(want) {
		t.Fatalf("探活请求按模型统计 = %v，期望 %v", got, want)
	}
	for model, n := range want {
		if got[model] != n {
			t.Fatalf("模型 %s 被探 %d 次，期望 %d 次；统计 %v", model, got[model], n, got)
		}
	}
}

// TestModelProbeStopsAtUnauthorized 确认一个账号碰到 401 就停下，后续账号一条请求都不发。
func TestModelProbeStopsAtUnauthorized(t *testing.T) {
	sandbox(t)
	bad := newProbeUpstream(t, []string{"m1"}, map[string]int{"m1": http.StatusUnauthorized})
	next := newProbeUpstream(t, []string{"m2"}, nil)

	addProbeAccount(t, bad, "a-bad", "--priority", "10")
	addProbeAccount(t, next, "b-next", "--priority", "20")

	code, out, stderr := run(t, "model", "probe", "--json")
	if code != 1 {
		t.Fatalf("碰到 401 时退出码 = %d，期望 1；stdout = %s，stderr = %s", code, out, stderr)
	}
	if !strings.Contains(stderr, "凭据") {
		t.Fatalf("stderr 应说明凭据被拒: %s", stderr)
	}
	probes := mustProbeRows(t, out)
	if len(probes) != 1 || probes["m1"].Status != "auth" {
		t.Fatalf("401 之后不该再探后续账号，实际探了 %d 个: %s", len(probes), out)
	}
	if got := len(next.requestBodies()); got != 0 {
		t.Fatalf("后续账号的假上游收到 %d 条请求，期望 0 条", got)
	}

	st := storetest.Open(t)
	acc, err := st.GetAccount("a-bad")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if acc.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	after, err := st.GetAccount("b-next")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	rows, err := st.AccountModels(after.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("后续账号不该被处理，实际写了 %d 行: %v", len(rows), rows)
	}
}

// TestModelProbeNeverPrintsKey 确认凭据不进任何输出：上游把 key 回显进错误体，
// 人读表格、--json、以及写进 requests 的错误摘要里都只能是 ***。
func TestModelProbeNeverPrintsKey(t *testing.T) {
	sandbox(t)
	const key = "sk-echo-check-4242"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key `+key+`"}}`)
	}))
	t.Cleanup(up.Close)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", up.URL, "", "--key", key); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1")
	if code != 1 {
		t.Fatalf("凭据被拒时退出码 = %d，期望 1；stdout = %s", code, out)
	}
	code, jsonOut, jsonErr := run(t, "model", "probe", "--account", "ds1", "--json", "--refresh")
	if code != 1 {
		t.Fatalf("凭据被拒时退出码 = %d，期望 1；stdout = %s", code, jsonOut)
	}
	for name, text := range map[string]string{
		"人读表格":       out,
		"人读 stderr":  stderr,
		"--json 输出":  jsonOut,
		"--json 错误流": jsonErr,
	} {
		if strings.Contains(text, key) {
			t.Fatalf("%s 泄漏了凭据: %s", name, text)
		}
	}

	st := storetest.Open(t)
	recs, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("探活应写下请求记录")
	}
	for _, rec := range recs {
		if strings.Contains(rec.Error, key) {
			t.Fatalf("请求记录的错误摘要泄漏了凭据: %s", rec.Error)
		}
		if rec.HTTPStatus != http.StatusUnauthorized || rec.OK {
			t.Fatalf("401 的探活行应是 401 且不 ok: %+v", rec)
		}
	}
}

// TestModelProbeZeroUsableTellsSkipCount 确认零可用时的错误文案带探活数与跳过数，
// 让「全被跳过」与「探了都不行」分得清。
func TestModelProbeZeroUsableTellsSkipCount(t *testing.T) {
	sandbox(t)
	// 两个模型都返回 403：结论有定论但都不算可用。
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{
		"m1": http.StatusForbidden,
		"m2": http.StatusForbidden,
	})
	addProbeAccount(t, up, "ds1")

	code, _, errOut := run(t, "model", "probe", "--account", "ds1")
	if code != 1 {
		t.Fatalf("零可用时退出码 = %d，期望 1", code)
	}
	if !strings.Contains(errOut, "本次探活 2 个") || !strings.Contains(errOut, "跳过 0 个") {
		t.Fatalf("首次探活的错误文案应带探活数与跳过数: %s", errOut)
	}
	if strings.Contains(errOut, "--refresh") {
		t.Fatalf("没有跳过时不该提示 --refresh: %s", errOut)
	}

	code, out, errOut := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 1 {
		t.Fatalf("全部跳过且零可用时退出码 = %d，期望 1", code)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("全部跳过时 --json 仍是空数组，实际 %s", out)
	}
	for _, want := range []string{"本次探活 0 个", "跳过 2 个", "--max-age", "--refresh"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("全部跳过时的错误文案缺 %q: %s", want, errOut)
		}
	}

	// 连模型列表都没拉到的账号：真实原因不能被「没有账号探出可用模型」盖住。
	if code, _, stderr := addAccount(t, "ds9", "custom", "http://127.0.0.1:1", "", "--key", "sk-test"); code != 0 {
		t.Fatalf("加不可达账号失败: %s", stderr)
	}
	code, _, errOut = run(t, "model", "probe", "--account", "ds9")
	if code != 1 {
		t.Fatalf("同步失败时退出码 = %d，期望 1", code)
	}
	if !strings.Contains(errOut, "同步失败") {
		t.Fatalf("同步失败时的错误文案应点出同步失败: %s", errOut)
	}
}

// TestModelProbeKeepsStderrClean 确认正常探活不往 stderr 写日志，--json 的 stdout 只有 JSON。
func TestModelProbeKeepsStderrClean(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1"}, nil)
	addProbeAccount(t, up, "ds1")

	code, out, errOut := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stderr = %s", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("正常探活不该往 stderr 写日志: %s", errOut)
	}
	var rows []probeRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("--json 的 stdout 应只有 JSON: %v，%s", err, out)
	}
	if len(rows) != 1 || rows[0].Status != "usable" {
		t.Fatalf("探活结果 = %+v", rows)
	}
}

// TestModelProbeRefreshStartsFromDiscoveredProtocol 确认 --refresh 重探已知模型时，
// 库里发现的协议排在厂商默认协议前面：默认协议 chat 只在首轮白打一次 404，
// 之后的每次重探都直接命中 anthropic 那条路由，上游只收到一次调用。
//
// 顺序反了的表现是每次 --refresh 都先多一条 404，模型多、重探频繁时全是白花的调用。
func TestModelProbeRefreshStartsFromDiscoveredProtocol(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models":              {status: http.StatusOK, body: modelsListM1},
		"POST /v1/chat/completions":   {status: http.StatusNotFound, body: `{"error":{"message":"没有这个路由"}}`},
		"POST /anthropic/v1/messages": {status: http.StatusOK, body: anthropicMessageOK},
	})
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	// 账号的默认协议取种子厂商 deepseek 的 openai-chat，地址指向假上游；
	// 不给 --models，探活会先同步一次模型列表。
	if code, _, stderr := addAccount(t, "ds1", "deepseek", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 首轮：默认协议 chat 打 404，换 anthropic 成功，发现结果落库。
	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首轮探活退出码 = %d，stderr = %s", code, stderr)
	}
	calls := up.postCalls()
	if len(calls) != 2 || calls[0].path != "/v1/chat/completions" || calls[1].path != "/anthropic/v1/messages" {
		t.Fatalf("首轮的上游调用 = %v，期望先默认协议 chat、再 anthropic", up.postPaths())
	}
	if calls[0].bearer != "Bearer sk-test" || calls[0].apiKey != "" {
		t.Fatalf("chat 的凭据头 = bearer %q / x-api-key %q，期望只有 Bearer sk-test", calls[0].bearer, calls[0].apiKey)
	}
	if calls[1].apiKey != "sk-test" || calls[1].bearer != "" {
		t.Fatalf("anthropic 的凭据头 = x-api-key %q / bearer %q，期望只有 x-api-key sk-test", calls[1].apiKey, calls[1].bearer)
	}
	st := storetest.Open(t)
	if row := st.ModelRow(t, "ds1", "m1"); row.Protocol != "anthropic" || row.Status != store.ModelUsable {
		t.Fatalf("首轮结论 = %+v，期望 usable 且协议记 anthropic", row)
	}

	// 第二轮 --refresh：已发现的协议排第一位，只打一次，也不再重新同步模型列表。
	up.reset()
	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json", "--refresh")
	if code != 0 {
		t.Fatalf("--refresh 退出码 = %d，stderr = %s", code, stderr)
	}
	all := up.all()
	if len(all) != 1 || all[0].path != "/anthropic/v1/messages" {
		t.Fatalf("--refresh 的上游调用 = %+v，期望只打一次已发现的 anthropic 路由", all)
	}
	if all[0].apiKey != "sk-test" {
		t.Fatalf("--refresh 的凭据头 = %q，期望 x-api-key sk-test", all[0].apiKey)
	}
	if rows := mustProbeRows(t, out); len(rows) != 1 || rows["m1"].Status != "usable" {
		t.Fatalf("--refresh 的结论 = %s，期望 m1 仍可用", out)
	}
	if row := st.ModelRow(t, "ds1", "m1"); row.Protocol != "anthropic" {
		t.Fatalf("重探后的协议 = %q，期望仍是 anthropic", row.Protocol)
	}
}

// TestModelSyncEndpointOnlyHitsSelectedProvider 覆盖 model sync --endpoint 与 --provider 一起用：
// 只同步被选中的账号，地址取指定路径，库里的 models_path（厂商级与路由级）一个字段都不改，
// 没被选中的账号连一次上游都不打。
func TestModelSyncEndpointOnlyHitsSelectedProvider(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /custom/models": {status: http.StatusOK, body: `{"data":[{"id":"m1"},{"id":"m2"}]}`},
		"GET /v1/models":     {status: http.StatusOK, body: modelsListM1},
	})
	mustInit(t)

	for _, name := range []string{"alpha", "beta"} {
		if code, _, stderr := run(t, "provider", "add", name,
			"--base-url", "https://api.example.invalid",
			"--openai", "/v1/chat/completions"); code != 0 {
			t.Fatalf("provider add %s 退出码 = %d，stderr = %s", name, code, stderr)
		}
	}
	for _, c := range []struct{ account, provider string }{{"alpha1", "alpha"}, {"beta1", "beta"}} {
		if code, _, stderr := addAccount(t, c.account, c.provider, up.srv.URL, "", "--key", "sk-test"); code != 0 {
			t.Fatalf("account add %s 退出码 = %d，stderr = %s", c.account, code, stderr)
		}
	}

	code, out, stderr := run(t, "model", "sync", "--provider", "alpha", "--endpoint", "/custom/models", "--json")
	if code != 0 {
		t.Fatalf("model sync --provider alpha --endpoint 退出码 = %d，stderr = %s", code, stderr)
	}
	var synced []struct {
		Account  string `json:"account"`
		Provider string `json:"provider"`
		Models   int    `json:"models"`
		Added    int    `json:"added"`
	}
	mustJSON(t, out, &synced)
	if len(synced) != 1 || synced[0].Account != "alpha1" || synced[0].Provider != "alpha" {
		t.Fatalf("--provider alpha 只该处理 alpha1，实际 %+v", synced)
	}
	if synced[0].Models != 2 || synced[0].Added != 2 {
		t.Fatalf("--endpoint 应拉到 2 个模型，实际 %+v", synced[0])
	}

	// 只有被选中的账号发了一次请求，且打的是 --endpoint 给的路径。
	calls := up.all()
	if len(calls) != 1 || calls[0].path != "/custom/models" {
		t.Fatalf("上游调用 = %+v，期望只打一次 GET /custom/models", calls)
	}

	// 库里的 models_path 不变：厂商级仍是空串，路由仍是推导值。
	providers := mustProviderLs(t)
	if got := providers["alpha"].ModelsPath; got != "" {
		t.Fatalf("--endpoint 不该写进厂商级 models_path，实际 %q", got)
	}
	if got := routeMap(providers["alpha"])["openai-chat"].ModelsPath; got != "/v1/models" {
		t.Fatalf("--endpoint 不该改路由的 models_path，实际 %q", got)
	}
	if got := routeMap(providers["beta"])["openai-chat"].ModelsPath; got != "/v1/models" {
		t.Fatalf("没被选中的厂商也不该被改动，实际 %q", got)
	}

	// beta1 一行模型都没同步到：model ls 里只有 alpha1 的两个模型。
	code, out, stderr = run(t, "model", "ls", "--json")
	if code != 0 {
		t.Fatalf("model ls 退出码 = %d，stderr = %s", code, stderr)
	}
	rows := mustModelRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("model ls 应只有 alpha1 的两个模型，实际 %+v", rows)
	}
	for _, model := range []string{"m1", "m2"} {
		if rows[model].Accounts[0].Name != "alpha1" {
			t.Fatalf("%s 的账号 = %+v，期望只有 alpha1", model, rows[model].Accounts)
		}
	}

	// 下一次不带 --endpoint 的同步回到路由推导出来的 /v1/models，beta1 也照常处理。
	up.reset()
	if code, _, stderr := run(t, "model", "sync"); code != 0 {
		t.Fatalf("不带 --endpoint 的 model sync 退出码 = %d，stderr = %s", code, stderr)
	}
	calls = up.all()
	if len(calls) != 2 {
		t.Fatalf("两个账号各同步一次，期望 2 次调用，实际 %+v", calls)
	}
	for _, c := range calls {
		if c.path != "/v1/models" {
			t.Fatalf("不带 --endpoint 时应回到 /v1/models，实际 %+v", calls)
		}
	}
}

// TestAccountAddModelsWritesCatalog 确认 `account add --models` 的名字按新结构落库：
// 目录里补一行（来源 manual，不带长度与价格），账号侧记一条 allowed，
// 因此这个账号立刻进候选集合，`--json` 的 models 就是这份 allowed 名字集合。
func TestAccountAddModelsWritesCatalog(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := addAccount(t, "ds1", "custom", "http://127.0.0.1:9", "", "--key", "sk-test", "--models", "m1,m2"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr := addAccount(t, "ds2", "custom", "http://127.0.0.1:9", "", "--key", "sk-test", "--models", "m3", "--json")
	if code != 0 {
		t.Fatalf("account add --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var added struct {
		Models []string `json:"models"`
	}
	mustJSON(t, out, &added)
	if len(added.Models) != 1 || added.Models[0] != "m3" {
		t.Fatalf("account add --json 的 models = %v，期望就是手填的 m3", added.Models)
	}

	st := storetest.Open(t)
	catalog, err := st.ProviderModels("custom")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	byName := make(map[string]store.ProviderModel, len(catalog))
	for _, pm := range catalog {
		byName[pm.Model] = pm
	}
	for _, want := range []string{"m1", "m2", "m3"} {
		pm, ok := byName[want]
		if !ok {
			t.Fatalf("目录里没有 %s：%+v", want, catalog)
		}
		if pm.Source != store.SourceManual {
			t.Fatalf("%s 的目录来源 = %q，期望 manual", want, pm.Source)
		}
		if pm.ContextWindow != nil || pm.MaxOutput != nil {
			t.Fatalf("%s 的长度类元数据 = %v / %v，手填不提供这些，应当留空",
				want, pm.ContextWindow, pm.MaxOutput)
		}
	}

	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	rows, err := st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读模型行: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ds1 的模型行 = %+v，期望 m1、m2 两行", rows)
	}
	for _, row := range rows {
		if row.Access != store.AccessAllowed {
			t.Fatalf("%s 的可达性 = %q，期望 allowed", row.Model, row.Access)
		}
	}
	if allowed, err := st.AllowedModels(acc.ID); err != nil {
		t.Fatalf("读选号集合: %v", err)
	} else if len(allowed) != 2 || allowed[0] != "m1" || allowed[1] != "m2" {
		t.Fatalf("选号集合 = %v，期望 [m1 m2]", allowed)
	}
}
