package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
)

// getJSON 发一个 GET，返回状态、响应体与响应头。
func getJSON(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	return callWithMethod(t, http.MethodGet, url)
}

func callWithMethod(t *testing.T, method, url string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return resp.StatusCode, string(b), resp.Header
}

// TestModelsListDedupesSortsAndPicksOwner 钉住清单的三个规则：只列 enabled 账号的
// accounts.models、按模型名去重升序、同名模型被多家支持时 owned_by 取厂商名字典序
// 第一个。响应体按整个字符串比对：字段顺序与 created 固定 0 都是契约的一部分。
func TestModelsListDedupesSortsAndPicksOwner(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "z1", "zeta", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-z-1", []string{"m-b", "m-a"})
	f.addAs(t, "a1", "alpha", gateway.ProtocolAnthropic, "http://127.0.0.1:1", "sk-a-1", []string{"m-a"})
	srv := f.server(t)

	status, body, header := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := `{"object":"list","data":[` +
		`{"id":"m-a","object":"model","created":0,"owned_by":"alpha"},` +
		`{"id":"m-b","object":"model","created":0,"owned_by":"zeta"}]}`
	if body != want {
		t.Fatalf("响应 = %s\n期望 = %s", body, want)
	}

	// 客户端能直接用 jq 取名字。
	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int    `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if parsed.Object != "list" || len(parsed.Data) != 2 {
		t.Fatalf("解析结果 = %+v", parsed)
	}
}

// TestModelsListSkipsDisabledKeepsCooling 钉住两个方向：停用账号的模型不出现在清单里，
// 冷却中账号的模型照样出现——冷却影响的是这一次能不能接，不是有没有这个模型名。
func TestModelsListSkipsDisabledKeepsCooling(t *testing.T) {
	f := newFixture(t)
	off := f.addAs(t, "off", "alpha", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-off", []string{"m-off"})
	cool := f.addAs(t, "cool", "zeta", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-cool", []string{"m-cool"})

	if err := f.st.SetEnabled(off.ID, false, "手工停用"); err != nil {
		t.Fatalf("停用账号: %v", err)
	}
	if err := f.st.RecordFailure(cool.ID, time.Now().Add(time.Minute).Unix(), "上游 429"); err != nil {
		t.Fatalf("打冷却: %v", err)
	}
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if strings.Contains(body, "m-off") {
		t.Fatalf("停用账号的模型不该出现: %s", body)
	}
	if !strings.Contains(body, `{"id":"m-cool","object":"model","created":0,"owned_by":"zeta"}`) {
		t.Fatalf("冷却中账号的模型应当出现: %s", body)
	}
}

// TestModelsListEmpty 钉住空清单是 200 加空数组，不是 404：客户端要能区分
// 「接口在、现在没有模型」与「接口不存在」。
func TestModelsListEmpty(t *testing.T) {
	f := newFixture(t)
	off := f.addAs(t, "off", "alpha", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-off", []string{"m-off"})
	if err := f.st.SetEnabled(off.ID, false, "手工停用"); err != nil {
		t.Fatalf("停用账号: %v", err)
	}
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if body != `{"object":"list","data":[]}` {
		t.Fatalf("响应 = %s，期望空数组", body)
	}

	// 一个账号都没有时同样如此。
	empty := newFixture(t)
	emptySrv := empty.server(t)
	status, body, _ = getJSON(t, emptySrv.URL+"/v1/models")
	if status != http.StatusOK || body != `{"object":"list","data":[]}` {
		t.Fatalf("空库：状态 = %d，响应 = %s", status, body)
	}
}

// TestModelByIDHitAndMiss 钉住单模型接口：命中返回与清单里同形的一个对象，
// 未命中 404 且文案指向 model ls 与 model probe；含 `/` 的模型名照取。
func TestModelByIDHitAndMiss(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-ds", []string{"deepseek-v4-pro", "org/vendor-model"})
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/v1/models/deepseek-v4-pro")
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	want := `{"id":"deepseek-v4-pro","object":"model","created":0,"owned_by":"deepseek"}`
	if body != want {
		t.Fatalf("响应 = %s，期望 %s", body, want)
	}

	// 模型名里的 `/` 不当作路径分隔：前缀之后的全部字符都是模型名。
	status, body, _ = getJSON(t, srv.URL+"/v1/models/org/vendor-model")
	if status != http.StatusOK || !strings.Contains(body, `"id":"org/vendor-model"`) {
		t.Fatalf("带斜杠的模型名：状态 = %d，响应 = %s", status, body)
	}

	status, body, _ = getJSON(t, srv.URL+"/v1/models/no-such-model")
	if status != http.StatusNotFound {
		t.Fatalf("未命中状态 = %d，响应 %s", status, body)
	}
	var errBody struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v，原文 %s", err, body)
	}
	if errBody.Error.Type != "not_found_error" {
		t.Fatalf("错误类型 = %q", errBody.Error.Type)
	}
	for _, want := range []string{"没有账号提供模型 no-such-model", "switchboard model ls", "model probe"} {
		if !strings.Contains(errBody.Error.Message, want) {
			t.Fatalf("错误文案 %q 缺少 %q", errBody.Error.Message, want)
		}
	}

	// 只有前缀、没有模型名时也是 404，文案说明缺的是模型名，不是「模型不存在」。
	status, body, _ = getJSON(t, srv.URL+"/v1/models/")
	if status != http.StatusNotFound || !strings.Contains(body, "URL 里没有模型名") {
		t.Fatalf("空模型名：状态 = %d，响应 = %s", status, body)
	}
}

// TestModelsRejectNonGet 钉住两个接口都只收 GET：其余方法 405 加 `Allow: GET`，
// 与 404「这个模型名不存在」区分开。
func TestModelsRejectNonGet(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-ds", []string{"m1"})
	srv := f.server(t)

	for _, path := range []string{"/v1/models", "/v1/models/m1"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
			status, body, header := callWithMethod(t, method, srv.URL+path)
			if status != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s 状态 = %d，期望 405（响应 %s）", method, path, status, body)
			}
			if allow := header.Get("Allow"); allow != http.MethodGet {
				t.Fatalf("%s %s Allow = %q，期望 GET", method, path, allow)
			}
			// HEAD 的响应体由 net/http 按协议丢掉，只校验状态与 Allow。
			if method == http.MethodHead {
				continue
			}
			if !strings.Contains(body, "只接受 GET") {
				t.Fatalf("%s %s 响应 = %s，期望说明只接受 GET", method, path, body)
			}
		}
	}
}

// TestModelsEndpointsAreReadOnly 钉住这两个接口没有副作用：不写 requests 行，
// 不动冷却、连续失败次数、启用状态与 last_error。
func TestModelsEndpointsAreReadOnly(t *testing.T) {
	f := newFixture(t)
	cool := f.addAs(t, "cool", "zeta", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-cool", []string{"m-cool"})
	if err := f.st.RecordFailure(cool.ID, time.Now().Add(time.Minute).Unix(), "上游 429"); err != nil {
		t.Fatalf("打冷却: %v", err)
	}
	off := f.addAs(t, "off", "alpha", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-off", []string{"m-off"})
	if err := f.st.SetEnabled(off.ID, false, "手工停用"); err != nil {
		t.Fatalf("停用账号: %v", err)
	}
	before := map[string][4]any{}
	for _, name := range []string{"cool", "off"} {
		acc, err := f.st.GetAccount(name)
		if err != nil {
			t.Fatalf("读账号: %v", err)
		}
		before[name] = [4]any{acc.Enabled, acc.CooldownUntil, acc.FailStreak, acc.LastError}
	}
	// 库本来就有账：空库断言 0 行分不出「没写」与「写了又删」，所以在调接口之前先塞一行。
	if err := f.st.InsertRequest(&store.RequestRecord{
		TS: time.Now().Unix(), Inbound: gateway.ProtocolChat, Outbound: gateway.ProtocolChat,
		ModelRequested: "m-seed", ModelUsed: "m-seed", AccountID: cool.ID,
		HTTPStatus: http.StatusOK, OK: true, InTokens: 7, OutTokens: 2,
	}); err != nil {
		t.Fatalf("塞一行请求记录: %v", err)
	}
	srv := f.server(t)

	for i := 0; i < 3; i++ {
		if status, body, _ := getJSON(t, srv.URL+"/v1/models"); status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		if status, body, _ := getJSON(t, srv.URL+"/v1/models/m-cool"); status != http.StatusOK {
			t.Fatalf("状态 = %d，响应 %s", status, body)
		}
		if status, _, _ := getJSON(t, srv.URL+"/v1/models/nope"); status != http.StatusNotFound {
			t.Fatalf("未命中状态 = %d", status)
		}
	}

	recs, err := f.st.RecentRequests(10)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	// 行数不变：还是原先那一行，值与塞进去时一致。
	if len(recs) != 1 || recs[0].ModelRequested != "m-seed" || recs[0].InTokens != 7 {
		t.Fatalf("只读接口不该写 requests，实际 %d 行：%+v", len(recs), recs)
	}
	for name, want := range before {
		acc, err := f.st.GetAccount(name)
		if err != nil {
			t.Fatalf("读账号: %v", err)
		}
		got := [4]any{acc.Enabled, acc.CooldownUntil, acc.FailStreak, acc.LastError}
		if got != want {
			t.Fatalf("账号 %s 状态被改动：%v → %v", name, want, got)
		}
	}
	// 冷却与连续失败次数按具体值钉住：RecordFailure 那一次应当留下 fail_streak=1
	// 与一个未来的冷却截止时间，接口调用前后都是这两个值。
	cooled, err := f.st.GetAccount("cool")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if cooled.FailStreak != 1 || cooled.CooldownUntil <= time.Now().Unix() {
		t.Fatalf("冷却账号 fail_streak = %d、cooldown = %d，期望 1 与未来时间",
			cooled.FailStreak, cooled.CooldownUntil)
	}
	if cooled.LastError != "上游 429" {
		t.Fatalf("冷却账号 last_error = %q，期望保持原值", cooled.LastError)
	}
}

// TestModelsEndpointsLeaveExistingRoutesAlone 确认新路径没有挪动原有入口：
// `/healthz` 的计数照旧、三条入站路径照旧只收 POST、未知路径仍是 404。
func TestModelsEndpointsLeaveExistingRoutesAlone(t *testing.T) {
	f := newFixture(t)
	f.addAs(t, "ds1", "deepseek", gateway.ProtocolChat, "http://127.0.0.1:1", "sk-ds", []string{"m1", "m2"})
	srv := f.server(t)

	status, body, _ := getJSON(t, srv.URL+"/healthz")
	if status != http.StatusOK || !strings.Contains(body, `"models":2`) || !strings.Contains(body, `"accounts":1`) {
		t.Fatalf("/healthz 状态 = %d，响应 = %s", status, body)
	}

	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		status, body, _ := getJSON(t, srv.URL+path)
		if status != http.StatusMethodNotAllowed || !strings.Contains(body, "只接受 POST") {
			t.Fatalf("入站路径 %s 的 GET：状态 = %d，响应 = %s", path, status, body)
		}
	}

	// 认不出的模型名仍走 5.2 的 404，文案与清单接口的那句不同（一个讲清单、一个讲这次请求）。
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"nope","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求网关: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), "没有账号提供模型 nope") {
		t.Fatalf("入站 404：状态 = %d，响应 = %s", resp.StatusCode, string(b))
	}

	if status, _, _ := getJSON(t, srv.URL+"/v1/unknown"); status != http.StatusNotFound {
		t.Fatalf("未知路径状态 = %d，期望 404", status)
	}
}
