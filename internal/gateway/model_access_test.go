package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// 这个文件钉住「模型事实在目录、账号只有可达性」之后网关的三条边界（见 DESIGN 5.13）：
//
//   - 选号只认 `account_models.access='allowed'`，forbidden / missing 的行不进候选；
//   - 可达性只由探活结论改写，拉清单（SyncModels）本身一个字段都不写；
//   - 零可用是「可达性全不是 allowed」，不是「目录里没有这个模型」：目录行留着，
//     /v1/models 与选号一起变空，等下一轮探活或手工声明再放回来。

func floatPtr(v float64) *float64 { return &v }

func int64Ptr(v int64) *int64 { return &v }

// catalogIDs 取 /v1/models 响应里的 id 列表，按响应顺序。
func catalogIDs(t *testing.T, srv *httptest.Server) []string {
	t.Helper()
	status, body, _ := getJSON(t, srv.URL+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("/v1/models 状态 = %d，响应 %s", status, body)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析 /v1/models 响应: %v（%s）", err, body)
	}
	ids := make([]string, 0, len(out.Data))
	for _, e := range out.Data {
		ids = append(ids, e.ID)
	}
	return ids
}

// TestSelectSkipsForbiddenAndMissingModels 确认候选集合只按可达性算：
// 同一行改成 forbidden 或 missing 之后既不进候选（转发 404，不是 503），也不出现在 /v1/models 里；
// 改回 allowed 立刻回来。这一条是「选号读可达性，不读账号级清单」的直接证据。
func TestSelectSkipsForbiddenAndMissingModels(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	body := requestBodies[translate.ProtocolChat]
	if ids := catalogIDs(t, srv); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("声明之后清单 = %v，期望 [m1]", ids)
	}
	if status, resp := postTo(t, srv, body, translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("可达时转发状态 = %d，响应 %s", status, resp)
	}

	for _, verdict := range []string{store.AccessForbidden, store.AccessMissing} {
		if err := f.st.SetAccountModelAccess(acc.ID, "m1", verdict, http.StatusForbidden, "探活结论", 1); err != nil {
			t.Fatalf("写可达性 %s: %v", verdict, err)
		}
		if ids := catalogIDs(t, srv); len(ids) != 0 {
			t.Fatalf("%s 之后清单 = %v，期望空（客户端不该看到用不了的名字）", verdict, ids)
		}
		status, resp := postTo(t, srv, body, translate.ProtocolChat)
		// 没有任何账号提供 = 清单问题，报 404；503 的意思是「提供了但此刻不可用」。
		if status != http.StatusNotFound || !strings.Contains(resp, "没有账号提供模型 m1") {
			t.Fatalf("%s 之后转发状态 = %d，响应 %s，期望 404 且文案说没有账号提供", verdict, status, resp)
		}
	}

	if err := f.st.SetAccountModelAccess(acc.ID, "m1", store.AccessAllowed, http.StatusOK, "", 1); err != nil {
		t.Fatalf("写回 allowed: %v", err)
	}
	if ids := catalogIDs(t, srv); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("改回 allowed 之后清单 = %v，期望 [m1]", ids)
	}
	if status, resp := postTo(t, srv, body, translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("改回 allowed 之后转发状态 = %d，响应 %s", status, resp)
	}
}

// TestSyncModelsLeavesAccessAlone 确认拉清单这一步不写可达性：
// 上游清单里有 m1 与 m2，账号上 m1 已经被探活判成 forbidden、m2 根本没有行，
// SyncModels 只把名字交回调用方，account_models 一行都不改——同步不能把探活结论翻成 allowed。
//
// 目录由 gateway 自己写（`UpsertProviderModels(source='sync')`，带上清单里的元数据，见 DESIGN 5.7），
// 可达性归命令层（`ReplaceModels` 只给没有行的名字插 allowed）。
func TestSyncModelsLeavesAccessAlone(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"GET /v1/models": {status: http.StatusOK, body: `{"data":[{"id":"m1"},{"id":"m2"}]}`},
	})
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1"}, 100)
	if err := f.st.SetAccountModelAccess(acc.ID, "m1", store.AccessForbidden, http.StatusForbidden, "MODEL_NOT_IN_PLAN", 7); err != nil {
		t.Fatalf("写探活结论: %v", err)
	}
	gs := newGateway(t, f)

	models, err := gs.SyncModels(context.Background(), acc)
	if err != nil {
		t.Fatalf("同步: %v", err)
	}
	if len(models) != 2 || models[0] != "m1" || models[1] != "m2" {
		t.Fatalf("同步结果 = %v，期望 [m1 m2]", models)
	}

	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达性行: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "m1" {
		t.Fatalf("同步不该给清单里的名字落可达性行，实际 %+v", rows)
	}
	if rows[0].Access != store.AccessForbidden || rows[0].HTTPStatus != http.StatusForbidden ||
		rows[0].CheckedAt != 7 || rows[0].Error != "MODEL_NOT_IN_PLAN" {
		t.Fatalf("同步把探活结论覆盖了: %+v", rows[0])
	}
	allowed, err := f.st.AllowedModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达模型: %v", err)
	}
	if len(allowed) != 0 {
		t.Fatalf("可达模型 = %v，期望空（forbidden 不是 allowed，m2 还没有行）", allowed)
	}
}

// TestProbeZeroUsableKeepsCatalogRows 确认「零可用」不等于「目录里没有」：
// 两个模型都被探活判成 forbidden 之后，可达模型为空、清单与选号都是空，
// 但目录里的两行还在——下一轮探活或手工改可达性时不用重新建目录。
func TestProbeZeroUsableKeepsCatalogRows(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-test", []string{"m1", "m2"}, 100)
	srv := f.server(t)

	for _, model := range []string{"m1", "m2"} {
		if err := f.st.SetAccountModelAccess(acc.ID, model, store.AccessForbidden, http.StatusForbidden, "MODEL_NOT_IN_PLAN", 7); err != nil {
			t.Fatalf("写探活结论: %v", err)
		}
	}

	allowed, err := f.st.AllowedModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达模型: %v", err)
	}
	if len(allowed) != 0 {
		t.Fatalf("可达模型 = %v，期望空", allowed)
	}
	catalog, err := f.st.ProviderModels(acc.Provider)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(catalog) != 2 || catalog[0].Model != "m1" || catalog[1].Model != "m2" {
		t.Fatalf("目录 = %+v，期望 m1 与 m2 两行都还在", catalog)
	}

	if ids := catalogIDs(t, srv); len(ids) != 0 {
		t.Fatalf("零可用时清单 = %v，期望空", ids)
	}
	status, resp := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat)
	if status != http.StatusNotFound || !strings.Contains(resp, "没有账号提供模型 m1") {
		t.Fatalf("转发状态 = %d，响应 %s，期望 404", status, resp)
	}
}

// TestEffectiveModelValuesDriveAccounting 钉住记账该读的那份值：价格与上下文长度按
// 「账号覆盖 → 目录」合并（store.EffectiveModels），网关不自己重算优先级，也不回写这两处。
// 覆盖只给了输入价与上下文长度，输出价与最大输出没有覆盖值，因此分别继承目录与为空。
func TestEffectiveModelValuesDriveAccounting(t *testing.T) {
	f := newFixture(t)
	up := newProbeUpstream(t, map[string]probeReply{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatOK},
	})
	acc := f.add(t, "ds1", gateway.ProtocolChat, up.srv.URL, "sk-test", nil, 100)

	if err := f.st.UpsertProviderModels(acc.Provider, []store.ProviderModel{{
		Model:         "m1",
		Source:        store.SourceSync,
		ContextWindow: int64Ptr(1000),
		MaxOutput:     int64Ptr(800),
		PriceIn:       floatPtr(1),
		PriceOut:      floatPtr(2),
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if err := f.st.SetAccountModelAccess(acc.ID, "m1", store.AccessAllowed, http.StatusOK, "", 7); err != nil {
		t.Fatalf("写可达性: %v", err)
	}
	if err := f.st.SetAccountModelOverride(acc.ID, "m1", store.AccountModelOverride{
		ContextWindow: int64Ptr(2000),
		PriceIn:       floatPtr(0.5),
	}); err != nil {
		t.Fatalf("写账号覆盖值: %v", err)
	}

	srv := f.server(t)
	if status, resp := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat); status != http.StatusOK {
		t.Fatalf("转发状态 = %d，响应 %s", status, resp)
	}
	recs, err := f.st.RecentRequests(1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("请求记录 = %+v（%v），期望一行", recs, err)
	}
	if recs[0].AccountID != acc.ID || recs[0].ModelUsed != "m1" {
		t.Fatalf("请求记录 = %+v，期望落在这个账号的 m1 上", recs[0])
	}

	infos, err := f.st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("读有效值: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("有效值 = %+v，期望只有 m1 一行", infos)
	}
	info := infos[0]
	if info.PriceIn == nil || *info.PriceIn != 0.5 {
		t.Fatalf("有效输入价 = %v，期望账号覆盖的 0.5", info.PriceIn)
	}
	if info.PriceOut == nil || *info.PriceOut != 2 {
		t.Fatalf("有效输出价 = %v，期望继承目录的 2", info.PriceOut)
	}
	if info.ContextWindow == nil || *info.ContextWindow != 2000 {
		t.Fatalf("有效上下文长度 = %v，期望账号覆盖的 2000", info.ContextWindow)
	}
	if info.MaxOutput == nil || *info.MaxOutput != 800 {
		t.Fatalf("有效最大输出 = %v，期望继承目录的 800", info.MaxOutput)
	}

	// 转发一个字节都不写这两处：覆盖值只由 SetAccountModelOverride 改，网关不参与。
	rows, err := f.st.AccountModels(acc.ID)
	if err != nil {
		t.Fatalf("读可达性行: %v", err)
	}
	if len(rows) != 1 || rows[0].ContextWindow == nil || *rows[0].ContextWindow != 2000 ||
		rows[0].PriceIn == nil || *rows[0].PriceIn != 0.5 {
		t.Fatalf("转发改动了账号覆盖值: %+v", rows)
	}
	catalog, err := f.st.ProviderModels(acc.Provider)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(catalog) != 1 || catalog[0].PriceIn == nil || *catalog[0].PriceIn != 1 {
		t.Fatalf("转发改动了目录价: %+v", catalog)
	}
}
