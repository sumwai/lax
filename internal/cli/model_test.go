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

	"switchboard/internal/config"
	"switchboard/internal/store"
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
	// 上一轮刚探过，结论在新鲜期内，这里用 --refresh 强制重探才有表格。
	code, out, stderr = run(t, "model", "probe", "--account", "ds1", "--refresh")
	if code != 0 {
		t.Fatalf("model probe 文本输出退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"账号", "模型", "本次探活 2 个", "跳过 0 个", "可用 1", "无权 1", "选号列表共 1 个"} {
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

// openTestStore 直接打开测试沙箱里的库，用来改 checked_at、读请求记录这类子命令不对外暴露的状态。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析数据路径: %v", err)
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// backdateCheckedAt 改一个模型行的 checked_at，模拟结论已经过时。
func backdateCheckedAt(t *testing.T, st *store.Store, model string, ts int64) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE account_models SET checked_at = ? WHERE model = ?`, ts, model); err != nil {
		t.Fatalf("改 %s 的 checked_at: %v", model, err)
	}
}

// addProbeAccount 初始化数据库，加一个指向假上游的账号。
func addProbeAccount(t *testing.T, up *probeUpstream, name string, extra ...string) {
	t.Helper()
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	args := append([]string{"account", "add", name, "--provider", "custom",
		"--base-url", up.srv.URL, "--key", "sk-test"}, extra...)
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

	st := openTestStore(t)
	backdateCheckedAt(t, st, "m1", time.Now().Add(-2*time.Hour).Unix())

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

// TestModelProbeDoesNotSkipUnsettledStatuses 确认待探活、凭据被拒、失败三种没有定论的状态照常探。
func TestModelProbeDoesNotSkipUnsettledStatuses(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, nil, nil)
	addProbeAccount(t, up, "ds1")

	st := openTestStore(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	// checked_at 都是刚写进去的，唯一让它们重探的理由是状态没有定论。
	for model, status := range map[string]string{
		"m-synced": store.ModelSynced,
		"m-auth":   store.ModelAuthFailed,
		"m-error":  store.ModelError,
	} {
		httpStatus := 0
		if status != store.ModelSynced {
			httpStatus = http.StatusInternalServerError
		}
		if err := st.SetModelStatus(acc.ID, model, status, httpStatus, "上一轮的结论"); err != nil {
			t.Fatalf("写 %s 状态: %v", model, err)
		}
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	if rows := mustProbeRows(t, out); len(rows) != 3 {
		t.Fatalf("synced / auth / error 都不该跳过，实际探了 %d 个: %s", len(rows), out)
	}
	if got := len(up.requestBodies()); got != 3 {
		t.Fatalf("假上游收到 %d 条请求，期望 3 条", got)
	}
}

// TestModelProbeAllSkippedWritesUsableModels 确认全部跳过时仍按库里 usable 回写并退出 0。
func TestModelProbeAllSkippedWritesUsableModels(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusForbidden})
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	st := openTestStore(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	// 把选号列表改坏，模拟「上一轮留下的旧值」，下一轮全部跳过时应当被库里 usable 纠正。
	if err := st.SetModels(acc.ID, []string{"stale-model"}); err != nil {
		t.Fatalf("改选号列表: %v", err)
	}

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json")
	if code != 0 {
		t.Fatalf("全部跳过时退出码 = %d，期望 0；stdout = %s，stderr = %s", code, out, stderr)
	}
	if strings.Contains(stderr, "没有账号探出可用模型") {
		t.Fatalf("全部跳过不该落到没有可用模型的错误分支: %s", stderr)
	}
	after, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if len(after.Models) != 1 || after.Models[0] != "m1" {
		t.Fatalf("全部跳过时 accounts.models 应按库里 usable 回写，实际 %v", after.Models)
	}
}

// TestModelProbeWritesRequestRows 确认探活写请求记录，跳过的模型不写，且进 usage 汇总。
func TestModelProbeWritesRequestRows(t *testing.T) {
	sandbox(t)
	up := newProbeUpstream(t, []string{"m1", "m2"}, map[string]int{"m2": http.StatusForbidden})
	addProbeAccount(t, up, "ds1")

	if code, _, stderr := run(t, "model", "probe", "--account", "ds1", "--json"); code != 0 {
		t.Fatalf("首次探活退出码 = %d，stderr = %s", code, stderr)
	}

	st := openTestStore(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	recs, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("探活应写 2 行请求记录，实际 %d 行", len(recs))
	}
	byModel := map[string]store.RequestRecord{}
	for _, r := range recs {
		byModel[r.ModelUsed] = r
	}
	ok := byModel["m1"]
	if ok.Inbound != "openai-chat" || ok.Outbound != "openai-chat" {
		t.Fatalf("探活行的入站与出站都应取账号协议，实际 %s→%s", ok.Inbound, ok.Outbound)
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
	bad := byModel["m2"]
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
	if len(recs) != 2 {
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
	if usageRows[0].Requests != 2 || usageRows[0].InTokens != 7 || usageRows[0].OutTokens != 1 {
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
	st := openTestStore(t)
	stale := time.Now().Add(-2 * time.Hour).Unix()
	backdateCheckedAt(t, st, "m3", stale)
	backdateCheckedAt(t, st, "m4", stale)

	code, out, stderr := run(t, "model", "probe", "--account", "ds1", "--json", "--max-age", "1h", "--limit", "1")
	if code != 0 {
		t.Fatalf("--limit 退出码 = %d，stdout = %s，stderr = %s", code, out, stderr)
	}
	rows := mustProbeRows(t, out)
	if len(rows) != 1 || rows["m3"].Status != "usable" {
		t.Fatalf("--limit 1 应只探待探的 m3，实际 %s", out)
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

	st := openTestStore(t)
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
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", key); code != 0 {
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

	st := openTestStore(t)
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
	if code, _, stderr := run(t, "account", "add", "ds9", "--provider", "custom",
		"--base-url", "http://127.0.0.1:1", "--key", "sk-test"); code != 0 {
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
