package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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

// TestAccountSyncProbeFlow 覆盖 sync → probe → accounts.models → model ls 整条链路。
func TestAccountSyncProbeFlow(t *testing.T) {
	sandbox(t)
	// m1 可用，m2 上游返回 403。
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusForbidden})

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test"); code != 0 {
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

	// 可用模型写进 accounts.models，选号按它走。
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	acc := mustAccountRow(t, out)
	if len(acc.Models) != 1 || acc.Models[0] != "m1" {
		t.Fatalf("account ls models = %v，期望只有 m1", acc.Models)
	}
	if acc.ModelsUsable != 1 || acc.ModelsSynced != 2 {
		t.Fatalf("account ls 模型计数 = 可用 %d / 同步 %d，期望 1 / 2", acc.ModelsUsable, acc.ModelsSynced)
	}

	code, out, stderr = run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "1 可用 / 2 同步") {
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
	for model, want := range map[string]string{"m1": "usable", "m2": "forbidden"} {
		row, ok := models[model]
		if !ok {
			t.Fatalf("model ls 缺模型 %s: %s", model, out)
		}
		if len(row.Accounts) != 1 || row.Accounts[0].Name != "ds1" || row.Accounts[0].Status != want {
			t.Fatalf("model ls 里 %s = %+v，期望 ds1(%s)", model, row.Accounts, want)
		}
	}

	// 人读输出：表格带账号与状态列，末尾给计数与写库结果。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1")
	if code != 0 {
		t.Fatalf("model probe 文本输出退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"账号", "模型", "可用 1", "无权 1", "选号列表共 1 个"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model probe 文本输出缺少 %q: %s", want, out)
		}
	}
}

// TestAccountProbeAllForbiddenKeepsModels 确认零可用模型时退出码 1 且不动 accounts.models。
func TestAccountProbeAllForbiddenKeepsModels(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{
		"m1": http.StatusForbidden,
		"m2": http.StatusForbidden,
	})

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test", "--models", "keep-me"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 1 {
		t.Fatalf("全部 403 时 model probe 退出码 = %d，期望 1；stdout = %s，stderr = %s", code, out, stderr)
	}
	if !strings.Contains(stderr, "探出可用模型") {
		t.Fatalf("stderr 应说明没有可用模型: %s", stderr)
	}
	probes := mustProbeRows(t, out)
	if len(probes) != 2 || probes["m1"].Status != "forbidden" || probes["m2"].Status != "forbidden" {
		t.Fatalf("探活结果 = %+v，期望两个模型都是 forbidden", probes)
	}

	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	acc := mustAccountRow(t, out)
	if len(acc.Models) != 1 || acc.Models[0] != "keep-me" {
		t.Fatalf("零可用模型时 accounts.models 被改动：%v", acc.Models)
	}
	if !acc.Enabled || acc.Status != "正常" {
		t.Fatalf("403 不该停用账号：enabled = %v，status = %s", acc.Enabled, acc.Status)
	}
}

// TestAccountProbeAuthFailureDisablesAccount 确认凭据被拒时账号停用、不写可用列表。
func TestAccountProbeAuthFailureDisablesAccount(t *testing.T) {
	sandbox(t)
	// m1 能通，m2 回 401：只要出现凭据被拒，整批结论都不作数。
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusUnauthorized})

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test", "--models", "keep-me"); code != 0 {
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

	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	acc := mustAccountRow(t, out)
	if acc.Enabled {
		t.Fatal("401 之后账号应被停用")
	}
	if len(acc.Models) != 1 || acc.Models[0] != "keep-me" {
		t.Fatalf("凭据被拒时 accounts.models 被改动：%v", acc.Models)
	}
}
