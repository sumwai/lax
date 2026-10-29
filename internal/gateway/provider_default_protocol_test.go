package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// protocolUpstream 起一个假上游，按请求路径回该协议的响应，并把每次调用的路径与凭据头记进 rec。
// 断言「打到哪条路由、用了哪套凭据头」是这几条用例的全部证据。
func protocolUpstream(t *testing.T, rec *callRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), string(body))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = io.WriteString(w, upstreamBodies[translate.ProtocolAnthropic])
		case strings.HasSuffix(r.URL.Path, "/responses"):
			_, _ = io.WriteString(w, upstreamBodies[translate.ProtocolResponses])
		default:
			_, _ = io.WriteString(w, upstreamBodies[translate.ProtocolChat])
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// twoRouteProvider 建一家声明 chat 与 anthropic 两条路由、默认协议由调用方定的厂商。
// 默认协议只有厂商行上一份（见 DESIGN 5.8），账号行不再存 protocol。
func (f *fixture) twoRouteProvider(t *testing.T, name, baseURL, defaultProtocol string) {
	t.Helper()
	if err := f.st.CreateProvider(store.Provider{Name: name, BaseURL: baseURL, DefaultProtocol: defaultProtocol},
		[]store.ProviderRoute{
			{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"},
			{Protocol: translate.ProtocolAnthropic, Path: "/v1/messages"},
		}); err != nil {
		t.Fatalf("建厂商 %s: %v", name, err)
	}
}

// TestForwardUsesProviderDefaultProtocol 钉住默认协议只有厂商行上一份：
// 账号行不再存 protocol，模型上也没有发现结果时，转发用厂商的有效默认协议，
// 上游路径与凭据头都跟它走（这里默认协议是 anthropic，入站 chat 因此要转过一道）。
func TestForwardUsesProviderDefaultProtocol(t *testing.T) {
	f := newFixture(t)
	const key = "sk-provider-default"
	rec := &callRecorder{}
	up := protocolUpstream(t, rec)
	f.twoRouteProvider(t, "pdef", up.URL, translate.ProtocolAnthropic)

	acc := f.createAccount(t, "a1", "pdef", key, []string{"m1"}, 100)
	// 账号的 Protocol 是派生值（厂商的有效默认协议），不是账号行上的列。
	if acc.Protocol != translate.ProtocolAnthropic {
		t.Fatalf("账号的派生协议 = %q，期望厂商的默认协议 %q", acc.Protocol, translate.ProtocolAnthropic)
	}
	if got, err := f.st.ModelProtocol(acc.ID, "m1"); err != nil || got != "" {
		t.Fatalf("模型上没有发现结果时 protocol 应为空，实际 %q（%v）", got, err)
	}
	// EffectiveProtocol 与转发送的是同一个值：两参数写法，账号不再有协议字段可传。
	if got, err := f.st.EffectiveProtocol(acc.ID, "m1"); err != nil || got != translate.ProtocolAnthropic {
		t.Fatalf("有效出站协议 = %q（%v），期望 %q", got, err, translate.ProtocolAnthropic)
	}
	srv := f.server(t)

	status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用数 = %d，期望 1：%v", len(calls), rec.paths())
	}
	if calls[0].path != "/v1/messages" || calls[0].apiKey != key {
		t.Fatalf("默认协议 anthropic 应打 /v1/messages 并带 x-api-key，实际路径 %q，x-api-key %q",
			calls[0].path, calls[0].apiKey)
	}
	if calls[0].bearer != "" {
		t.Fatalf("anthropic 路由不该带 Bearer，实际 %q", calls[0].bearer)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 || recs[0].Outbound != translate.ProtocolAnthropic {
		t.Fatalf("请求记录 = %+v，期望一行且出站是 anthropic", recs)
	}
}

// TestForwardModelProtocolBeatsProviderDefault 钉住优先级：模型上的发现结果压过厂商默认协议。
// 同一家厂商（默认 anthropic）下，发现该用 chat 的模型必须打到 chat 路由并带 Bearer。
func TestForwardModelProtocolBeatsProviderDefault(t *testing.T) {
	f := newFixture(t)
	const key = "sk-model-beats-provider"
	rec := &callRecorder{}
	up := protocolUpstream(t, rec)
	f.twoRouteProvider(t, "pdef", up.URL, translate.ProtocolAnthropic)

	acc := f.createAccount(t, "a1", "pdef", key, []string{"m1"}, 100)
	// 探活发现这个模型该用 chat，结论落在 account_models.protocol 上。
	if err := f.st.SetAccountModelProtocol(acc.ID, "m1", translate.ProtocolChat); err != nil {
		t.Fatalf("写协议发现结果: %v", err)
	}
	if got, err := f.st.EffectiveProtocol(acc.ID, "m1"); err != nil || got != translate.ProtocolChat {
		t.Fatalf("有效出站协议 = %q（%v），期望模型上的 %q", got, err, translate.ProtocolChat)
	}
	srv := f.server(t)

	status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("上游调用数 = %d，期望 1：%v", len(calls), rec.paths())
	}
	if calls[0].path != "/v1/chat/completions" || calls[0].bearer != "Bearer "+key {
		t.Fatalf("模型上的发现结果应压过厂商默认协议，实际路径 %q，Authorization %q",
			calls[0].path, calls[0].bearer)
	}
	if calls[0].apiKey != "" {
		t.Fatalf("chat 路由不该带 x-api-key，实际 %q", calls[0].apiKey)
	}
}

// TestNoOutboundProtocolHintsProviderSet 钉住「没有任何协议可用」时的错误：
// 厂商一行在但一条路由都没声明，默认协议与模型发现结果都是空串，转发返回 503、
// 错误体按入站协议给形状，文案给出下一步命令，而不是「不支持的出站协议 ""」。
func TestNoOutboundProtocolHintsProviderSet(t *testing.T) {
	f := newFixture(t)
	// 一行不做任何路由声明的厂商：地址在，默认协议算不出来。
	if err := f.st.AddProvider(store.Provider{Name: "noroute", BaseURL: "http://127.0.0.1:1"}, nil); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	acc := f.createAccount(t, "a1", "noroute", "sk-noroute", []string{"m1"}, 100)
	if acc.Protocol != "" {
		t.Fatalf("没有路由的厂商不该算出默认协议，实际 %q", acc.Protocol)
	}
	if got, err := f.st.EffectiveProtocol(acc.ID, "m1"); err != nil || got != "" {
		t.Fatalf("有效出站协议 = %q（%v），期望空串", got, err)
	}
	srv := f.server(t)

	status, body := postTo(t, srv, requestBodies[translate.ProtocolAnthropic], translate.ProtocolAnthropic)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503（不是 400 也不是 500）：%s", status, body)
	}
	// 错误体仍按入站协议给形状（anthropic 那条），类型是 api_error。
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"api_error"`) {
		t.Fatalf("错误体不是 anthropic 形状: %s", body)
	}
	for _, want := range []string{"switchboard provider set noroute", "--openai /v1/chat/completions"} {
		if !strings.Contains(body, want) {
			t.Fatalf("错误文案应含下一步命令 %q，实际 %s", want, body)
		}
	}
	if strings.Contains(body, `不支持的出站协议`) {
		t.Fatalf("不该把配置缺失报成编码器错误: %s", body)
	}
}

// TestAccountTestWithoutProtocolHintsProviderSet 钉住 account test 的第二段：
// 厂商算不出有效默认协议时，报错同样要说清下一步运行什么命令，不拿默认分支去打 chat。
func TestAccountTestWithoutProtocolHintsProviderSet(t *testing.T) {
	f := newFixture(t)
	if err := f.st.AddProvider(store.Provider{Name: "noroute", BaseURL: "http://127.0.0.1:1"}, nil); err != nil {
		t.Fatalf("建厂商: %v", err)
	}
	f.createAccount(t, "a1", "noroute", "sk-noroute", []string{"m1"}, 100)
	acc, err := f.st.GetAccount("a1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	gs := newGateway(t, f)

	if _, err := gs.Probe(context.Background(), acc); err == nil {
		t.Fatal("算不出出站协议时应报错")
	} else if !strings.Contains(err.Error(), "switchboard provider set noroute") {
		t.Fatalf("报错应给出下一步命令，实际 %v", err)
	}
}

// TestOrphanAccountHintsProviderAdd 钉住历史孤儿行（账号的 provider 在表里查不到）：
// 地址与默认协议都只从厂商行取，孤儿行两样都没有，因此拼不出任何地址；
// 报错要说清先重建厂商，而不是拿一个空 base_url 猜出一条畸形路径去打。
func TestOrphanAccountHintsProviderAdd(t *testing.T) {
	f := newFixture(t)
	f.addOrphan(t, "ghost1", "ghost", "sk-ghost", []string{"m1"})
	srv := f.server(t)

	status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503：%s", status, body)
	}
	for _, want := range []string{"switchboard provider add ghost", "--base-url", "--openai /v1/chat/completions"} {
		if !strings.Contains(body, want) {
			t.Fatalf("错误文案应含 %q，实际 %s", want, body)
		}
	}
}

// TestProviderAddressIsSingleSource 钉住地址只有一份的回归：`provider set --base-url` 之后，
// 改之前就存在的账号下一次请求立刻打到新地址，账号行一个字节都不用改
// （账号只存厂商名，派生地址跟着厂商行走）。
func TestProviderAddressIsSingleSource(t *testing.T) {
	f := newFixture(t)
	const key = "sk-single-source"
	firstRec, secondRec := &callRecorder{}, &callRecorder{}
	first := protocolUpstream(t, firstRec)
	second := protocolUpstream(t, secondRec)
	f.twoRouteProvider(t, "psrc", first.URL, translate.ProtocolChat)

	acc := f.createAccount(t, "a1", "psrc", key, []string{"m1"}, 100)
	srv := f.server(t)

	if status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("第一次状态 = %d，响应 %s", status, body)
	}
	if paths := firstRec.paths(); len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("第一次应打第一台上游的 chat 路由，实际 %v", paths)
	}

	// 换厂商地址：账号行不动，下一次请求按新地址拼路径。
	newBase := second.URL
	if err := f.st.SetProvider("psrc", store.ProviderPatch{BaseURL: &newBase}); err != nil {
		t.Fatalf("改厂商地址: %v", err)
	}
	if status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("第二次状态 = %d，响应 %s", status, body)
	}
	if paths := secondRec.paths(); len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("改地址后应打第二台上游，实际 %v", paths)
	}
	if hits := len(firstRec.all()); hits != 1 {
		t.Fatalf("旧地址应当不再收到请求，实际共 %d 次", hits)
	}

	// 账号行没被改写：同一个 id、同一个名字，派生地址跟着厂商行变了。
	again, err := f.st.GetAccount(acc.Name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if again.ID != acc.ID || again.Name != acc.Name {
		t.Fatalf("改厂商地址不该动账号行，改前 %+v，改后 %+v", acc, again)
	}
	if again.BaseURL != second.URL {
		t.Fatalf("账号的派生地址 = %q，期望跟着厂商行变成 %q", again.BaseURL, second.URL)
	}
	if again.Provider != "psrc" {
		t.Fatalf("账号的厂商 = %q，期望 psrc", again.Provider)
	}
}
