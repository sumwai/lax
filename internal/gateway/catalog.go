package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// 只读的模型清单接口：两个接口都只读 accounts 一份数据，
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

// modelCatalog 是清单接口要用的数据，一次读库算出来。两个接口共用它：列表要真名与别名，
// 单模型接口还要用别名解析结果写 404 文案。
type modelCatalog struct {
	// entries 是真实名字的条目，按名字升序。
	entries []ModelEntry
	// owners 是「真实名字 → 提供一个它的厂商名」，同名被多家支持时取厂商名字典序第一个。
	owners map[string]string
	// aliasTargets 是解得出真实名字的别名（别名 → 真实名字），不管那个真名眼下有没有
	// 账号能用。清单只用得上目标在 owners 里的那些，404 文案要说清目标是谁。
	aliasTargets map[string]string
	// brokenAliases 是链成环或太长的别名：解不出真实名字，请求会按 400 拒掉，
	// 清单与单模型接口都不列出它们，只在 404 文案里说清是哪条链坏了。
	brokenAliases map[string]error
}

// modelCatalog 汇总选号实际读的那份清单：`enabled=1` 账号的可达模型（`account_models.access='allowed'`，
// 由 store 的派生字段 `Account.Models` 一并读出来），按模型名去重、升序，再补上别名。
// 三条规则：
//
//  1. 只看 `enabled=1`：停用账号的模型不出现在清单里，客户端因此不会把请求发到一个
//     必然 503 的模型名上；冷却中的账号照样算——冷却影响的是「这一次能不能接」，
//     不是「有没有这个模型名」，把它藏起来会让客户端在冷却的几十秒里以为模型没了。
//  2. 按模型名去重：同一家（或多家）上游都能提供同一个名字时只出现一次，选号本来
//     也按名字匹配，列出多行只会让客户端以为要用哪个账号。
//  3. `owned_by` 取厂商名；同名模型被多家支持时取厂商名字典序第一个，不看 priority
//     与账号顺序：同一份库必须给出同一份响应，否则客户端缓存与测试断言都会抖。
//
// 数据来源与选号候选集合同一批行：`account_models.access='allowed'`，解析别名后按名字匹配。
// 同步与探活的过程值（`forbidden` / `missing` / `error`）不进这里，账号行也没有第二份清单列可读。
// 别名只在这份真实清单之上加条目：解析不到目标的别名不会凭空造出一个能接请求的名字。
func (s *Server) modelCatalog() (*modelCatalog, error) {
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

	aliases, err := s.store.ModelAliases()
	if err != nil {
		return nil, err
	}
	c := &modelCatalog{
		entries:       entries,
		owners:        owners,
		aliasTargets:  make(map[string]string, len(aliases)),
		brokenAliases: map[string]error{},
	}
	// 逐个别名问 store，而不是在这里按跳查表：走链、成环与超长的判定只有 store.ResolveModelAlias
	// 一份。别名是手工维护的、表很小，多几趟小查询换一条判定规则。
	for _, a := range aliases {
		target, _, err := s.store.ResolveModelAlias(a.Alias)
		if err != nil {
			c.brokenAliases[a.Alias] = err
			continue
		}
		c.aliasTargets[a.Alias] = target
	}
	return c, nil
}

// all 把真实名字与别名合成一份清单：真名在前并按名字升序重排，保证同一份库给出同一份响应。
// 别名与真名同名时只留真名那一条（写别名时已经挡住这种情况，库被手改过或事后新加账号仍可能
// 撞上，真名说了算）；目标眼下没有启用账号提供的别名不出现——清单反映「现在能接的名字」。
func (c *modelCatalog) all() []ModelEntry {
	entries := make([]ModelEntry, 0, len(c.entries)+len(c.aliasTargets))
	real := make(map[string]bool, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
		real[e.ID] = true
	}
	for alias, target := range c.aliasTargets {
		if real[alias] {
			continue
		}
		owner, ok := c.owners[target]
		if !ok {
			continue
		}
		entries = append(entries, ModelEntry{ID: alias, Object: modelObject, Created: 0, OwnedBy: owner})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

// find 取出清单里 id 这一项，返回的对象与列表里那一条完全相同。别名条目的 owned_by 取目标的厂商：
// 别名本身不属于哪家厂商，客户端拿它去填下拉框时看到的是真相。
func (c *modelCatalog) find(id string) (ModelEntry, bool) {
	for _, e := range c.entries {
		if e.ID == id {
			return e, true
		}
	}
	target, ok := c.aliasTargets[id]
	if !ok {
		return ModelEntry{}, false
	}
	owner, ok := c.owners[target]
	if !ok {
		return ModelEntry{}, false
	}
	return ModelEntry{ID: id, Object: modelObject, Created: 0, OwnedBy: owner}, true
}

// notFoundMessage 是 `GET /v1/models/{id}` 的 404 文案。别名要把目标写出来：只说「清单里没有 X」
// 会让人去清单里找一个永远不存在的名字；链坏了的别名要说清是哪条链，
// 不然用户只会看到一个怎么也查不到的名字。
func (c *modelCatalog) notFoundMessage(id string) string {
	if err, ok := c.brokenAliases[id]; ok {
		return fmt.Sprintf("清单里没有模型 %s（停用账号不计，冷却中的账号仍然计入）；"+
			"这是一个别名，但解不出真实模型名：%v；用 lax model alias ls 看别名链", id, err)
	}
	if target, ok := c.aliasTargets[id]; ok {
		return fmt.Sprintf("清单里没有模型 %s（停用账号不计，冷却中的账号仍然计入）；"+
			"这是一个别名，目标是 %s，但没有启用账号提供它；"+
			"用 lax model ls 看现有清单，或先运行 model probe", id, target)
	}
	return fmt.Sprintf("清单里没有模型 %s（停用账号不计，冷却中的账号仍然计入）；"+
		"用 lax model ls 看现有清单，或先运行 model probe", id)
}

// handleModelList 是 `GET /v1/models`：列出账号池里现在能用的模型名，含别名。
// 没有任何模型时返回 200 与空数组，不是 404——「清单是空的」与「这个接口不存在」
// 是两件事，客户端要能分开。
func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	c, err := s.modelCatalog()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error",
			"读取模型清单失败: "+err.Error()+"；先用 lax doctor 检查数据库")
		return
	}
	writeJSON(w, http.StatusOK, modelListBody{Object: listObject, Data: c.all()})
}

// handleModelByID 是 `GET /v1/models/{id}`：单个模型名，别名也查得到同一条目。未命中返回 404 与
// `not_found_error`，文案给出两条下一步命令（看清单、探活）。文案讲的是「清单里没有」
// 而不是「没有账号提供」：清单只算 `enabled=1` 的账号、冷却中的账号照样算，所以只由停用
// 账号提供的名字在这里是 404，转发时却是 503（账号存在但当前不可用），两者的分界就是
// 「清单只算现在能接请求的账号」与「转发看的是有没有账号持有这个名字」两件事。
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

	c, err := s.modelCatalog()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error",
			"读取模型清单失败: "+err.Error()+"；先用 lax doctor 检查数据库")
		return
	}
	if e, ok := c.find(id); ok {
		writeJSON(w, http.StatusOK, e)
		return
	}
	writeError(w, http.StatusNotFound, "not_found_error", c.notFoundMessage(id))
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
