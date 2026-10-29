package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// 只读的模型清单接口（见 DESIGN 5.2.1）。两个接口都只读 accounts 一份数据，
// 不写 requests、不动冷却与选号：客户端问「有哪些模型名可用」不该改变网关的任何状态。
const (
	// modelsPath 是清单的列表入口，modelsPathPrefix 是单模型入口的前缀（其后全部算模型名，
	// 含 `/` 的模型名也照取）。
	modelsPath       = "/v1/models"
	modelsPathPrefix = modelsPath + "/"

	// listObject 与 modelObject 是这两个接口的 `object` 取值，与上游模型列表一致。
	listObject  = "list"
	modelObject = "model"
)

// ModelEntry 是清单里的一项。`created` 固定 0：这里是本机账号池的清单，
// 没有上游的创建时间可用，编一个时间戳只会让客户端误以为它有意义。
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelListBody 是 `GET /v1/models` 的响应体。空清单要给 `"data":[]` 而不是 `null`
// （Data 在 modelCatalog 里总是非 nil 的切片）。
type modelListBody struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// modelCatalog 汇总选号实际读的那份清单：`enabled=1` 账号的可达模型（`account_models.access='allowed'`，
// 由 store 的派生字段 `Account.Models` 一并读出来），按模型名去重、升序。三条规则：
//
//  1. 只看 `enabled=1`：停用账号的模型不出现在清单里，客户端因此不会把请求发到一个
//     必然 503 的模型名上；冷却中的账号照样算——冷却影响的是「这一次能不能接」，
//     不是「有没有这个模型名」，把它藏起来会让客户端在冷却的几十秒里以为模型没了。
//  2. 按模型名去重：同一家（或多家）上游都能提供同一个名字时只出现一次，选号本来
//     也按名字匹配，列出多行只会让客户端以为要用哪个账号。
//  3. `owned_by` 取厂商名；同名模型被多家支持时取厂商名字典序第一个，不看 priority
//     与账号顺序：同一份库必须给出同一份响应，否则客户端缓存与测试断言都会抖。
//
// 数据来源与 5.3 的候选集合是同一批行：`account_models.access='allowed'`（见 5.13）。
// 同步与探活的过程值（`forbidden` / `missing` / `error`）不进这里，账号行也没有第二份清单列可读。
func (s *Server) modelCatalog() ([]ModelEntry, error) {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		for _, m := range a.Models {
			// 空白名字不是一个模型：它不会匹配任何请求里的 model，列出来只添乱。
			if strings.TrimSpace(m) == "" {
				continue
			}
			if owner, ok := owners[m]; !ok || a.Provider < owner {
				owners[m] = a.Provider
			}
		}
	}

	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]ModelEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, ModelEntry{ID: name, Object: modelObject, Created: 0, OwnedBy: owners[name]})
	}
	return entries, nil
}

// handleModelList 是 `GET /v1/models`：列出账号池里现在能用的模型名。
// 没有任何模型时返回 200 与空数组，不是 404——「清单是空的」与「这个接口不存在」
// 是两件事，客户端要能分开。
func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	entries, err := s.modelCatalog()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error",
			"读取模型清单失败: "+err.Error()+"；先用 switchboard doctor 检查数据库")
		return
	}
	writeJSON(w, http.StatusOK, modelListBody{Object: listObject, Data: entries})
}

// handleModelByID 是 `GET /v1/models/{id}`：单个模型名。未命中返回 404 与
// `not_found_error`，文案给出两条下一步命令（看清单、探活）。文案讲的是「清单里没有」
// 而不是「没有账号提供」：清单只算 `enabled=1` 的账号、冷却中的账号照样算，所以只由停用
// 账号提供的名字在这里是 404，转发时却是 503（账号存在但当前不可用），两者的分界写在
// DESIGN 5.2.1。
func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, modelsPathPrefix)
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found_error",
			"URL 里没有模型名，/v1/models/{id} 要带上模型名；先运行 curl http://127.0.0.1:4567/v1/models 看现有清单")
		return
	}

	entries, err := s.modelCatalog()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error",
			"读取模型清单失败: "+err.Error()+"；先用 switchboard doctor 检查数据库")
		return
	}
	for _, e := range entries {
		if e.ID == id {
			writeJSON(w, http.StatusOK, e)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found_error",
		fmt.Sprintf("清单里没有模型 %s（停用账号不计，冷却中的账号仍然计入）；用 switchboard model ls 看现有清单，或先运行 model probe", id))
}

// allowGet 校验只读接口的方法。非 GET 一律 405 并带 `Allow: GET`：
// 405 说的是「这个地址不支持这个方法」，与 404「这个模型名不存在」分开，
// 客户端据此能判断是自己方法用错了还是清单里真的没有。
func allowGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", http.MethodGet)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
		fmt.Sprintf("%s %s 只接受 GET；这是只读的模型清单，发对话请求要发到 /v1/messages、/v1/chat/completions 或 /v1/responses，查清单用 curl http://127.0.0.1:4567/v1/models",
			r.Method, r.URL.Path))
	return false
}
