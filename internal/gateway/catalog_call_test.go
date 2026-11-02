package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lax/internal/gateway"
	"lax/internal/translate"
)

// setPriority 直接把账号的 priority 改掉。addAs 固定写 100，而「owned_by 不看 priority」
// 这条要两个账号的优先级不同才验证得了；store 没有改 priority 的方法，测试直接改这一列。
func (f *fixture) setPriority(t *testing.T, name string, priority int) {
	t.Helper()
	res, err := f.st.DB().Exec(`UPDATE accounts SET priority = ? WHERE name = ?`, priority, name)
	if err != nil {
		t.Fatalf("改 %s 的 priority: %v", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("读影响行数: %v", err)
	}
	if n != 1 {
		t.Fatalf("改 %s 的 priority 影响了 %d 行，期望 1 行", name, n)
	}
}

// TestCatalogNamesServeEveryInboundPath 按客户端的真实顺序走一遍：先 GET /v1/models 拿到清单，
// 再用清单里的名字分别打三条入站路径，三条都要 200、都是入站协议的形状、都按这个名字记账。
func TestCatalogNamesServeEveryInboundPath(t *testing.T) {
	f := newFixture(t)
	up, _ := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.URL, "sk-ds", []string{"deepseek-v4-pro"})
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("清单不是合法 JSON: %v，原文 %s", err, body)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "deepseek-v4-pro" {
		t.Fatalf("清单 = %+v，期望一条 deepseek-v4-pro", list.Data)
	}
	name := list.Data[0].ID

	inbounds := []string{translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses}
	for _, inbound := range inbounds {
		reqBody := strings.Replace(requestBodies[inbound], `"model":"m1"`, `"model":"`+name+`"`, 1)
		status, resp := postTo(t, srv, reqBody, inbound)
		if status != http.StatusOK {
			t.Fatalf("用清单里的名字 %s 打 %s：状态 = %d，响应 %s", name, inbound, status, resp)
		}
		// 客户端拿到的必须是入站协议的形状，正文与用量都来自同一条上游响应。
		res, err := translate.DecodeResponse(inbound, []byte(resp), name)
		if err != nil {
			t.Fatalf("%s 响应不是 %s 的形状: %v，原文 %s", name, inbound, err, resp)
		}
		if len(res.Blocks) != 1 || res.Blocks[0].Text != "pong" {
			t.Fatalf("%s 响应正文 = %+v，期望一段 pong", inbound, res.Blocks)
		}
		if res.Usage.In != 3 || res.Usage.Out != 1 {
			t.Fatalf("%s 用量 = %d/%d，期望 3/1", inbound, res.Usage.In, res.Usage.Out)
		}
	}

	// 三条入站路径各写一行账，模型名是清单里的那一个。
	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("请求记录 %d 行，期望 3 行", len(recs))
	}
	seen := map[string]bool{}
	for _, rec := range recs {
		if !rec.OK || rec.ModelRequested != name || rec.ModelUsed != name {
			t.Fatalf("请求记录 = %+v，期望 ok 且模型名是 %s", rec, name)
		}
		if rec.InTokens != 3 || rec.OutTokens != 1 {
			t.Fatalf("请求记录的用量 = %d/%d，期望 3/1", rec.InTokens, rec.OutTokens)
		}
		seen[rec.Inbound] = true
	}
	for _, inbound := range inbounds {
		if !seen[inbound] {
			t.Fatalf("请求记录里少了 inbound=%s 那一行", inbound)
		}
	}
}

// TestCatalogAndInboundAgreeOnUnlistedName 确认清单里没有的名字两条路给同一个结论：
// 单模型接口 404，三条入站路径也是 404（不是 503，也不是拿别的名字硬打）。
func TestCatalogAndInboundAgreeOnUnlistedName(t *testing.T) {
	f := newFixture(t)
	up, _ := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.URL, "sk-ds", []string{"deepseek-v4-pro"})
	srv := f.server(t)

	// 名字只差一个后缀：账号池非空，但这个模型不在清单里。
	const missing = "deepseek-v4-pro-mini"
	status, body, _ := getJSON(t, srv.URL+"/v1/models/"+missing)
	if status != http.StatusNotFound {
		t.Fatalf("单模型接口状态 = %d，响应 %s", status, body)
	}
	// 单模型接口讲「清单里没有」（只算启用账号），三条入站路径讲「没有账号提供」。
	// 两者都点名同一个名字，但口径不同。
	if !strings.Contains(body, "清单里没有模型 "+missing) {
		t.Fatalf("单模型接口文案 = %s，期望点名 %s", body, missing)
	}

	cases := []struct{ inbound, wantType string }{
		{translate.ProtocolAnthropic, "not_found_error"},
		{translate.ProtocolChat, "invalid_request_error"},
		{translate.ProtocolResponses, "invalid_request_error"},
	}
	for _, tc := range cases {
		reqBody := strings.Replace(requestBodies[tc.inbound], `"model":"m1"`, `"model":"`+missing+`"`, 1)
		status, resp := postTo(t, srv, reqBody, tc.inbound)
		if status != http.StatusNotFound {
			t.Fatalf("%s 用清单里没有的名字：状态 = %d，响应 %s", tc.inbound, status, resp)
		}
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(resp), &env); err != nil {
			t.Fatalf("%s 错误响应不是合法 JSON: %v，原文 %s", tc.inbound, err, resp)
		}
		if env.Error.Type != tc.wantType {
			t.Fatalf("%s 错误类型 = %q，期望 %q", tc.inbound, env.Error.Type, tc.wantType)
		}
		if !strings.Contains(env.Error.Message, "没有账号提供模型 "+missing) {
			t.Fatalf("%s 错误文案 = %q，期望点名 %s", tc.inbound, env.Error.Message, missing)
		}
	}
}

// TestCatalogKeepsModelWhileAccountCools 用真实的 429 把账号打进冷却，清单仍列出它的模型：
// 冷却影响的是这一次能不能接，不是有没有这个名字。把冷却中的模型藏起来，客户端会在
// 冷却的几十秒里以为模型没了。
func TestCatalogKeepsModelWhileAccountCools(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer up.Close()

	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.URL, "sk-ds", []string{"deepseek-v4-pro"})
	srv := f.server(t)

	const reqBody = `{"model":"deepseek-v4-pro","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	status, body := postTo(t, srv, reqBody, translate.ProtocolChat)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("唯一候选被 429 后状态 = %d，响应 %s", status, body)
	}
	if !strings.Contains(body, "所有候选账号都失败") {
		t.Fatalf("响应 = %s，期望说明所有候选都失败", body)
	}
	// 冷却真的落了库（否则下面「冷却中仍在清单里」是同义反复）。
	acc, err := f.st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	failure := acc.ModelFailures["deepseek-v4-pro"]
	if failure.CooldownUntilMS <= time.Now().UnixMilli() || failure.FailStreak != 1 {
		t.Fatalf("账号没进冷却：cooldown = %d，fail_streak = %d", failure.CooldownUntilMS, failure.FailStreak)
	}

	status, listBody, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("清单状态 = %d，响应 %s", status, listBody)
	}
	want := `{"object":"list","data":[{"id":"deepseek-v4-pro","object":"model","created":0,"owned_by":"deepseek"}]}`
	if listBody != want {
		t.Fatalf("清单 = %s\n期望 = %s", listBody, want)
	}
}

// TestModelNameWithSlashesAndColonsIsTakenWhole 确认 `{id}` 是模型名原文：前缀之后的 `/` 与 `:`
// 都算名字的一部分，不会被切成路径段。单模型接口 200，选号也按同一条整名匹配。
func TestModelNameWithSlashesAndColonsIsTakenWhole(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, translate.ProtocolChat)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, up.URL, "sk-ds",
		[]string{"deepseek/deepseek-v4.1-flash", "vendor/team/model:free"})
	srv := f.server(t)

	names := []string{"deepseek/deepseek-v4.1-flash", "vendor/team/model:free"}
	for _, name := range names {
		status, body, _ := getJSON(t, srv.URL+"/v1/models/"+name)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/models/%s：状态 = %d，响应 %s", name, status, body)
		}
		want := `{"id":"` + name + `","object":"model","created":0,"owned_by":"deepseek"}`
		if body != want {
			t.Fatalf("响应 = %s\n期望 = %s", body, want)
		}
	}

	// 清单里同样是两条整名，前缀不会被当成厂商分组切掉。
	status, listBody, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("清单状态 = %d，响应 %s", status, listBody)
	}
	wantList := `{"object":"list","data":[` +
		`{"id":"deepseek/deepseek-v4.1-flash","object":"model","created":0,"owned_by":"deepseek"},` +
		`{"id":"vendor/team/model:free","object":"model","created":0,"owned_by":"deepseek"}]}`
	if listBody != wantList {
		t.Fatalf("清单 = %s\n期望 = %s", listBody, wantList)
	}

	// 带冒号的名字整条到上游：选号按整名匹配，上游收到的 model 与客户端发的一致。
	body := `{"model":"vendor/team/model:free","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	status, resp := postTo(t, srv, body, translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("转发带冒号的名字：状态 = %d，响应 %s", status, resp)
	}
	if !strings.Contains(rec.body, `"model":"vendor/team/model:free"`) {
		t.Fatalf("上游收到的请求体 = %s，期望 model 是整条名字", rec.body)
	}
}

// TestCatalogOwnerIgnoresPriority 确认同名模型被两家支持时 `owned_by` 取厂商名字典序第一个，
// 不看 priority：把字典序小的那家排到高优先级后面，响应仍然给 alpha。否则同一份库会因为
// 账号优先级变化给出不同响应，客户端缓存与断言都会抖。
func TestCatalogOwnerIgnoresPriority(t *testing.T) {
	f := newFixture(t)
	// zeta 先建（id 更小）且优先级更高，两个「第一个」都不是 alpha。
	f.addAs(t, "z1", "zeta", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-z", []string{"shared"})
	f.addAs(t, "a1", "alpha", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-a", []string{"shared"})
	f.setPriority(t, "z1", 10)
	f.setPriority(t, "a1", 900)
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	want := `{"object":"list","data":[{"id":"shared","object":"model","created":0,"owned_by":"alpha"}]}`
	if body != want {
		t.Fatalf("响应 = %s\n期望 = %s", body, want)
	}

	// 单模型接口与清单同源，给的是同一个 owned_by。
	status, body, _ = getJSON(t, srv.URL+"/v1/models/shared")
	if status != http.StatusOK {
		t.Fatalf("单模型状态 = %d，响应 %s", status, body)
	}
	wantOne := `{"id":"shared","object":"model","created":0,"owned_by":"alpha"}`
	if body != wantOne {
		t.Fatalf("响应 = %s\n期望 = %s", body, wantOne)
	}
}

// TestCatalogReadErrorIsReported 确认清单读库失败时两个接口都报 500 与 api_error，
// 文案说明是读清单失败并指向 doctor：这条路径容易漏，因为正常库里跑不出来。
func TestCatalogReadErrorIsReported(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-ds", []string{"m1"})
	srv := f.server(t)

	// 关掉库，之后每次读账号都会失败。t.Cleanup 里再关一次只返回错误，不影响用例。
	if err := f.st.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}

	for _, path := range []string{"/v1/models", "/v1/models/m1"} {
		status, body, _ := getJSON(t, srv.URL+path)
		if status != http.StatusInternalServerError {
			t.Fatalf("%s 状态 = %d，响应 %s，期望 500", path, status, body)
		}
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("%s 错误响应不是合法 JSON: %v，原文 %s", path, err, body)
		}
		if env.Error.Type != "api_error" {
			t.Fatalf("%s 错误类型 = %q，期望 api_error", path, env.Error.Type)
		}
		for _, want := range []string{"读取模型清单失败", "lax doctor"} {
			if !strings.Contains(env.Error.Message, want) {
				t.Fatalf("%s 错误文案 %q 缺少 %q", path, env.Error.Message, want)
			}
		}
	}
}
