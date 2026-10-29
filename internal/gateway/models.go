package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// ModelsEndpoint 是 base_url 加 /v1/models 的兜底：账号没有厂商行、或厂商一条路由都没声明时用它，
// 末尾带不带 /v1 都行。正常路径上的地址走 Server.modelsEndpoint（按厂商覆盖与路由的 models_path 取）。
// base_url 先按 store.NormalizeBaseURL 规整，最终地址的路径部分不会出现 `//`。
func ModelsEndpoint(base string) string {
	base = normalizedBase(base)
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// modelsEndpoint 解析账号的模型列表地址，同时返回这条地址该用哪种协议发凭据头（见 DESIGN 5.7）：
//
//  1. override 非空（`model sync --endpoint`）时用它，压过下面全部，协议取厂商的有效默认协议；取值可以是
//     相对 base_url 的路径，也可以是整条地址（整条地址直接访问，不再拼 base_url）；
//  2. 厂商行不存在（历史孤儿行）、或厂商行在但既没有路由也没有 models_path 覆盖时，按 base_url 后缀猜，
//     与升级前一致；孤儿行的地址也是空串，拼不出任何可用地址，这里直接报错并给出下一步命令；
//  3. 厂商级 models_path 非空时用它，协议取厂商的有效默认协议（覆盖是厂商级设置，没有自己的协议）；
//  4. 否则从路由取：厂商默认协议那条的 models_path，为空再退到同厂商按协议名排序的第一条非空值；
//  5. 厂商声明了路由但一个非空 models_path 都没有时报错，提示手填或 `provider set --models-path`。
//
// 第 2 步之后的取值都可能是整条地址：地址来自路由时由 joinEndpoint 判断就不拼 base_url。
// 协议必须与路径一起回去：凭据头按协议选（anthropic 用 x-api-key，另两家用 Bearer），
// 地址来自哪条路由就用哪条路由的协议。deepseek 的 anthropic 账号取到的是 chat 的 /v1/models，
// 用厂商默认协议发 x-api-key 上去只会换回一个 401/403，却把错误指到「凭据无效」上。
func (s *Server) modelsEndpoint(a store.Account, override string) (endpoint, protocol string, err error) {
	// --endpoint 的取值与路由取值同一套解释（store.NormalizeEndpoint）：相对路径只规整、不拒绝，
	// 整条地址校验后直接使用。规整之后为空视为这次没给，回到厂商覆盖与路由。
	if override != "" {
		target, err := store.NormalizeEndpoint(override)
		if err != nil {
			return "", "", fmt.Errorf("model sync --endpoint：%w", err)
		}
		if target != "" {
			return joinEndpoint(a.BaseURL, target), a.Protocol, nil
		}
	}

	p, err := s.store.GetProvider(a.Provider)
	if errors.Is(err, store.ErrProviderNotFound) {
		// 地址只有厂商行上有（见 DESIGN 5.8）：孤儿行的 a.BaseURL 是空串，拼出来的
		// 只会是 /v1/models 这种没有协议的路径。报配置缺什么，不报成「列表地址不合法」。
		if a.BaseURL == "" {
			return "", "", s.noAddressError(a, ProtocolChat)
		}
		return ModelsEndpoint(a.BaseURL), a.Protocol, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("读厂商 %s 失败: %w", a.Provider, err)
	}
	routes, err := s.store.ProviderRoutes(a.Provider)
	if err != nil {
		return "", "", fmt.Errorf("读厂商 %s 的路由失败: %w", a.Provider, err)
	}
	// 厂商行在、但一条路由都没声明，也没有厂商级覆盖：按 base_url 兜底，与升级前一致。
	if len(routes) == 0 && strings.TrimSpace(p.ModelsPath) == "" {
		if a.BaseURL == "" {
			return "", "", s.noAddressError(a, ProtocolChat)
		}
		return ModelsEndpoint(a.BaseURL), a.Protocol, nil
	}

	path, routeProtocol := store.ResolveModelsPath(p.ModelsPath, routes, a.Protocol)
	if path == "" {
		return "", "", fmt.Errorf("厂商 %s 没有模型列表接口，用 --models 手填，或用 switchboard provider set %s --models-path <路径> 补上模型列表路径",
			a.Provider, a.Provider)
	}
	return joinEndpoint(a.BaseURL, path), routeProtocol, nil
}

// SyncModels 按厂商覆盖与路由的 models_path 拉取上游的模型列表，按名字排序去重。
// 响应形状三种都认（见 parseModelListPage 与 DESIGN 5.7）：`{data:[…]}`、`{models:[…]}`
// 与顶层数组，形状不同的上游（例如 ollama 的 /api/tags）不必再靠 --models 手填。
// gemini 的 `{models:[{name:"models/…"}]}` 复用第二种形状，另加两条后处理：按
// `supportedGenerationMethods` 过滤、条目名去掉一个 `models/` 前缀；分页按 `nextPageToken` 取完。
func (s *Server) SyncModels(ctx context.Context, a store.Account) ([]string, error) {
	return s.SyncModelsWithEndpoint(ctx, a, "")
}

// modelListPageSize 是请求模型列表时带的页大小（见 DESIGN 5.7）。gemini 的清单分页返回，
// 不带页大小时按服务端的默认值分页，模型多的账号要多打好几次；其它上游不认识这个参数，原样忽略。
const modelListPageSize = 1000

// maxModelListPages 是模型列表最多取几页。超过上限时报错而不是静默截断：截断会让同步看起来成功，
// 实际少掉一部分模型，那几个模型永远选不到号，症状是「上游明明有这个模型却一直 404」。
const maxModelListPages = 10

// SyncModelsWithEndpoint 是 `model sync --endpoint <路径>` 的入口：endpoint 非空时只影响这一次调用，
// 相对路径取厂商 base_url 拼上它、整条地址直接访问，忽略厂商的 models_path 覆盖与路由里的值。
//
// 模型列表可能分页：gemini 的清单按 pageSize / pageToken 翻页，响应里有 nextPageToken 就接着取下一页，
// 最多 maxModelListPages 页，取完再统一排序去重。页数超上限时报错，不静默截断。
// 其它上游的列表地址一个字节都不改（见 modelListPageURL）。
func (s *Server) SyncModelsWithEndpoint(ctx context.Context, a store.Account, endpoint string) ([]string, error) {
	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return nil, err
	}
	secretValue := string(key)

	target, protocol, err := s.modelsEndpoint(a, endpoint)
	if err != nil {
		return nil, err
	}

	// 只有 gemini 的清单按 pageSize / pageToken 翻页；其它上游的地址原样使用（见 modelListPageURL）。
	paginated := protocol == ProtocolGemini

	var models []string
	seen := map[string]bool{}
	// seenTokens 记上游已经给过的游标：上游反复返回同一个 nextPageToken 时立即报错，
	// 不白打满 maxModelListPages 页。
	seenTokens := map[string]bool{}
	pageToken := ""
	for page := 1; ; page++ {
		pageURL, err := modelListPageURL(target, pageToken, paginated)
		if err != nil {
			return nil, fmt.Errorf("模型列表地址 %q 不合法，检查厂商 %s 的 base-url: %w", target, a.Provider, err)
		}
		raw, err := s.fetchModelPage(ctx, pageURL, target, protocol, a, secretValue)
		if err != nil {
			return nil, err
		}
		names, next, isModelList := parseModelListPage(raw)
		if !isModelList {
			return nil, fmt.Errorf("上游返回的不是模型列表：认 %s 三种形状，检查 base-url %s 是否指向模型列表接口（三种形状都不匹配时用 --models 手填）",
				modelShapeList(), a.BaseURL)
		}
		for _, name := range names {
			if seen[name] {
				continue
			}
			seen[name] = true
			models = append(models, name)
		}
		if next == "" {
			break
		}
		if seenTokens[next] {
			return nil, fmt.Errorf("上游重复返回同一个分页游标（GET %s，pageToken=%s）：清单接口可能不认 pageToken，检查上游的分页写法、或改用 --models 手填",
				target, next)
		}
		seenTokens[next] = true
		if page >= maxModelListPages {
			return nil, fmt.Errorf("上游的模型清单超过 %d 页还没取完（GET %s，每页 pageSize=%d）：清单过大，检查上游是否按页返回、或改用 --models 手填",
				maxModelListPages, target, modelListPageSize)
		}
		pageToken = next
	}
	if len(models) == 0 {
		// 清单被方法过滤干净时也是这里报错（gemini 的账号只有一个 embedding 模型），
		// 文案里点一句过滤规则，否则用户会先去怀疑地址与凭据。
		return nil, fmt.Errorf("上游没有返回模型，检查 base-url %s 是否正确（含 supportedGenerationMethods 的清单只保留带 generateContent 的条目）",
			a.BaseURL)
	}
	sort.Strings(models)
	return models, nil
}

// modelListPageURL 给需要分页的上游追加分页参数：pageSize 每页都带，pageToken 从第二页起带。
// 只有 gemini 的清单按 pageSize / pageToken 翻页，paginated 为假时原样返回 target：地址里可能带
// 签名（`?sig=…`），经 url.Parse 再 Encode 会重排参数、改写转义，签名当场失效（见 DESIGN 5.7）。
// 追加时也只往原有查询串后面接一段，原有查询串的编码一个字节不改。
func modelListPageURL(target, pageToken string, paginated bool) (string, error) {
	if !paginated {
		return target, nil
	}
	if _, err := url.Parse(target); err != nil {
		return "", err
	}
	query := "pageSize=" + strconv.Itoa(modelListPageSize)
	if pageToken != "" {
		query += "&pageToken=" + url.QueryEscape(pageToken)
	}
	base, fragment := target, ""
	if i := strings.IndexByte(target, '#'); i >= 0 {
		base, fragment = target[:i], target[i:]
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + query + fragment, nil
}

// fetchModelPage 取一页模型列表的原始响应体。凭据只在这里作为参数出现，错误文本一并过
// scrubSecret，明文不进错误信息与日志（见 DESIGN 5.6）。错误信息里写的是 target（配置里的地址），
// 不带这次追加的分页参数，便于照着命令里的取值排查。
// 模型列表请求与转发同一套头：先 setAuth 设凭据头，再叠厂商附加头（见 DESIGN 5.7、5.10）。
func (s *Server) fetchModelPage(ctx context.Context, pageURL, target, protocol string,
	a store.Account, secretValue string) ([]byte, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("模型列表地址 %q 不合法，检查厂商 %s 的 base-url: %w", pageURL, a.Provider, err)
	}
	req.Header.Set("Accept", "application/json")
	// 凭据头跟着取到这条路径的协议走，不是厂商默认协议。
	setAuth(req, protocol, secretValue)
	// 模型列表请求没有模型名（{model} 展开为空时不发那条头），会话键也没有：
	// {session} 在这里每次请求现生成一个（见 DESIGN 5.10）。
	if err := s.applyProviderHeaders(req, a, protocol, "", ""); err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型列表失败（GET %s）：%s", target, scrubSecret(err.Error(), secretValue))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游返回 %d（GET %s，按 %s 发凭据）：%s；检查该账号所属厂商的 base-url 与凭据",
			resp.StatusCode, target, protocol, scrubSecret(readErrorBody(resp.Body), secretValue))
	}

	var raw json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelListBytes)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("模型列表不是合法 JSON: %w", err)
	}
	return raw, nil
}

// modelListShape 是模型列表响应的一种已知形状，按数组里的顺序依次尝试（见 DESIGN 5.7）。
// field 为空串表示顶层本身就是条目数组，desc 是报错文案里给用户看的写法。
type modelListShape struct {
	field string
	desc  string
}

// modelListShapes 是三种已知形状：OpenAI 兼容的 `{data:[…]}`、ollama 风格的 `{models:[…]}`、
// 顶层数组。data 排在 models 前面，两种字段同时出现时认 data。
var modelListShapes = []modelListShape{
	{field: "data", desc: `{"data":[{"id":"…"}]}`},
	{field: "models", desc: `{"models":[{"name":"…"}]}`},
	{field: "", desc: `顶层数组 ["m1","m2"]`},
}

// modelShapeList 把三种形状的写法拼成一句报错文案，形状只在 modelListShapes 里定义一份。
func modelShapeList() string {
	desc := make([]string, len(modelListShapes))
	for i, shape := range modelListShapes {
		desc[i] = shape.desc
	}
	return strings.Join(desc, "、")
}

// parseModelListPage 按 modelListShapes 的顺序解析上游的模型列表响应，返回按名字排序去重后的模型名
// 与下一页的游标（`nextPageToken`）。第三个返回值为 false 表示三种形状的字段都不存在，响应不是模型列表。
// 只有字段不存在才试下一种形状：字段在但不是数组、或数组为空，都算「上游返回空列表」（返回空
// 切片与 true），不再拿下一个形状兜底——`{"data":[]}` 与 `{"models":[]}` 都是空列表，不是形状不对。
// 游标只看顶层 `nextPageToken`：没有、不是字符串或为空白都当作「没有下一页」。
func parseModelListPage(raw []byte) (models []string, nextToken string, isModelList bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, "", false
	}
	// 顶层数组：整个响应就是条目列表，可以混着写字符串与对象。
	if raw[0] == '[' {
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, "", false
		}
		return modelNames(entries), "", true
	}
	if raw[0] != '{' {
		return nil, "", false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, "", false
	}
	for _, shape := range modelListShapes {
		if shape.field == "" {
			continue
		}
		value, ok := top[shape.field]
		if !ok {
			continue
		}
		// 字段存在但解不成数组（null、对象、字符串）按空列表处理：这种形状已经命中，不再往下试。
		var entries []json.RawMessage
		_ = json.Unmarshal(value, &entries)
		return modelNames(entries), nextPageToken(top), true
	}
	return nil, "", false
}

// nextPageToken 取响应顶层的分页游标（gemini 的清单分页返回它）。没有、不是字符串或为空白
// 都当作「没有下一页」。
func nextPageToken(top map[string]json.RawMessage) string {
	value, ok := top["nextPageToken"]
	if !ok {
		return ""
	}
	var token string
	if err := json.Unmarshal(value, &token); err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

// modelNames 从条目列表里取模型名，按名字排序去重。缺名字的条目（一个字段都没有、或全是空白）跳过。
//
// 响应里真的出现 supportedGenerationMethods 字段时（gemini）按它过滤：只保留该方法列表里含
// generateContent 的条目，embedding 一类没有这个方法的条目不进清单。没出现这个字段的响应
// （ollama 的 `/api/tags` 这类）不过滤，否则没有方法列表的上游会被过滤成空列表。
//
// 去前缀与形状无关：凡是以 `models/` 开头的名字都去一个前缀。名字里带前缀的不只 gemini，
// 而路由的 `{model}` 占位符在请求时只放回裸名，库里存带前缀的名字会让上游收到
// `models/models/…`（见 DESIGN 5.7）。
func modelNames(entries []json.RawMessage) []string {
	filterMethods := hasGenerationMethods(entries)
	seen := map[string]bool{}
	var models []string
	for _, entry := range entries {
		if filterMethods && !supportsGenerateContent(entry) {
			continue
		}
		name := modelEntryName(entry)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, name)
	}
	sort.Strings(models)
	return models
}

// hasGenerationMethods 判断这批条目里有没有 supportedGenerationMethods 字段：有就说明这是
// 「带方法列表」的形状（gemini），按它过滤；一个都没有时（ollama 风格的 `{models:[{name}]}`）
// 不过滤，否则一个字段都没有的上游会被过滤成空列表。
func hasGenerationMethods(entries []json.RawMessage) bool {
	for _, entry := range entries {
		if _, ok := generationMethods(entry); ok {
			return true
		}
	}
	return false
}

// generationMethods 取条目的 supportedGenerationMethods，第二个返回值是字段在不在。
// 字段在但解不成字符串数组时返回空列表与 true：那种条目没有可用的方法列表，过滤时落选。
func generationMethods(entry json.RawMessage) ([]string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil {
		return nil, false
	}
	value, ok := obj["supportedGenerationMethods"]
	if !ok {
		return nil, false
	}
	var methods []string
	_ = json.Unmarshal(value, &methods)
	return methods, true
}

// supportsGenerateContent 判断条目的方法列表里有没有 generateContent。没有这个字段的条目
// 在过滤生效时同样落选：上游既没说它支持生成，就不进清单，宁少不错。
func supportsGenerateContent(entry json.RawMessage) bool {
	methods, _ := generationMethods(entry)
	for _, m := range methods {
		if m == "generateContent" {
			return true
		}
	}
	return false
}

// modelEntryName 取一个条目的模型名：按 id → name → model 的顺序取第一个非空字符串。
// 条目本身是字符串时直接用它（顶层数组的 `["m1"]` 写法）。名字以 `models/` 开头时去掉一个前缀
// （gemini 的清单这么写，见 modelNames 的注释）。
func modelEntryName(entry json.RawMessage) string {
	var s string
	if err := json.Unmarshal(entry, &s); err == nil {
		return bareModelName(strings.TrimSpace(s))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"id", "name", "model"} {
		value, ok := obj[key]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(value, &v); err != nil {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			return bareModelName(v)
		}
	}
	return ""
}

// bareModelName 去掉名字开头的一个 `models/` 前缀，只去一个：`models/models/x` 落成 `models/x`。
// 名字里是否带前缀与上游形状无关，这一条对所有形状都生效（见 DESIGN 5.7）——路由的 `{model}`
// 占位符在请求时把裸名放回路径，库里存带前缀的名字会让上游收到两层前缀。
func bareModelName(name string) string {
	if rest, ok := strings.CutPrefix(name, "models/"); ok {
		return rest
	}
	return name
}

// maxModelListBytes 是一次读模型列表响应的上限。`model sync` 与 `account test` 共用这一个值：
// 两条路径对同一份清单必须得出同一个结论。清单本身并不小——commandcode 的 94 个模型就有 18KB——
// 按更小的上限截断会让同步拿到完整清单、`account test` 却报「上游返回的不是模型列表」（见 DESIGN 5.7）。
const maxModelListBytes = 8 << 20

// readModelList 读出一份模型清单的模型名（排序去重后）。形状判定复用 parseModelListPage，与
// `model sync` 是同一份代码、同一套取值规则（`{data:[…]}`、`{models:[…]}`、顶层数组，按名字去重、
// 去 `models/` 前缀），不再有第二套只认 `{data}` 的逻辑：一份清单在这两条路径上必须数出同一个数、
// 挑出同一个名字。
// 第二个返回值为 false 表示上游返回的不是模型列表：`account test` 只用它报个数与挑一个模型名打
// 最小请求，形状不对不判这一段失败。
func readModelList(r io.Reader) ([]string, bool) {
	raw, err := io.ReadAll(io.LimitReader(r, maxModelListBytes))
	if err != nil {
		return nil, false
	}
	models, _, isModelList := parseModelListPage(raw)
	if !isModelList {
		return nil, false
	}
	return models, true
}

// ModelProbe 是单个模型在一次探活里的结论。
//
// Access 是结论的正文：`allowed` / `forbidden` / `missing` / `error`（见 store.Access* 与
// DESIGN 5.13），调用方拿它写 store.SetAccountModelAccess。**401 不是模型层面的结论**，那一栏留空，
// 表示「这一次不要写可达性」（见 DESIGN 5.7 里遇到 401 就停下那条）。
//
// Status 是迁移 0015 之前的状态词表，由 Access 推出，只为尚未改完的 internal/cli 保留：
// 两栏永远同时有值或同时为空，读哪一栏都不会看到半个结论。
//
// Protocol 是这次实际用到的协议：发现成功时是发现到的那个，其余情况下是厂商默认协议。
type ModelProbe struct {
	Model      string
	Access     string
	Status     string
	Protocol   string
	HTTPStatus int
	Message    string
	Latency    time.Duration
}

// setAccess 记下这次探活的可达性结论，并把旧词表那一栏一起推出来。
// 映射只有 store.LegacyStatus 一份：探活的结论一定带着「刚探过」，所以 checkedAt 传 1，
// allowed 落成 usable 而不是 synced（synced 的意思是「同步拿到名字、还没探过」，探活结论不会是它）。
func (res *ModelProbe) setAccess(access string) {
	res.Access = access
	res.Status = store.LegacyStatus(access, 1)
}

// probeAttempt 是一次协议尝试的完整结果：结论加记账需要的开始时间与用量。
type probeAttempt struct {
	res   ModelProbe
	start time.Time
	usage probeUsage
}

// defaultProbeProtocols 是探活候选的兜底顺序：账号没有厂商行（或该厂商一个路由都没声明）
// 时三条入站协议全试。厂商声明过路由的只试那些（见 routeProtocolOrder），循环本身不认识厂商。
// 兜底里不含 gemini：没有厂商声明 gemini 路由时，模型名与地址都没有确定的形状，
// 不该拿它去打一个猜出来的地址。
var defaultProbeProtocols = []string{
	translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses,
}

// isWireProtocol 判断一个取值是不是线协议。判定只有 store.IsWireProtocol 一份：gemini
// 也是线协议（入站两条路径、流式双向都支持，见 DESIGN 5.9），厂商声明了 gemini 路由时探活要能用它。
// 别的值不能进探活：编码器遇见未知协议会报错，静默落进 chat 分支更糟——那会把一条配置
// 错误变成对错误地址的上游调用。
func isWireProtocol(p string) bool {
	return store.IsWireProtocol(p)
}

// routeProtocolOrder 给出探活一个模型的完整有序候选，顺序与去重都在这里定：
//
//  1. 库里已经发现过这个模型的协议排最前，它是「按事实打」的那次，省掉每次 --refresh
//     白打的一次 404；
//  2. 厂商的有效默认协议紧接其后，它既是对厂商配置的验证，也是 401 判定与全失败结论的依据；
//  3. 其余按 resolver 给的顺序：Options.RouteProtocols 注入时用它，否则用该厂商在
//     provider_routes 里声明过的协议（按协议名排序），都没有则三种全试。
//
// 账号行不再存协议（迁移 0014），a.Protocol 是厂商默认协议的派生值：厂商没有路由时它是空串，
// 候选就只剩第 3 步的三种全试。override 非 nil 时顶替第 3 步的来源（探活自己指定候选时用）。
// 出现线协议以外的值直接报错。
func (s *Server) routeProtocolOrder(a store.Account, model string, override []string) ([]string, error) {
	var order []string
	seen := map[string]bool{}
	add := func(protocol, from string) error {
		if protocol == "" || seen[protocol] {
			return nil
		}
		if !isWireProtocol(protocol) {
			return fmt.Errorf("%s 给出的协议 %q 不是线协议，可用 %s",
				from, protocol, strings.Join(store.WireProtocols(), "、"))
		}
		seen[protocol] = true
		order = append(order, protocol)
		return nil
	}

	// 已发现的协议只用来排序，读不出来就当没有：一次读库失败不该让探活停下。
	discovered, err := s.store.ModelProtocol(a.ID, model)
	if err != nil {
		s.logf("读账号 %s 模型 %s 的已发现协议失败，按未发现处理: %v", a.Name, model, err)
		discovered = ""
	}
	if err := add(discovered, "account_models.protocol"); err != nil {
		return nil, err
	}
	if err := add(a.Protocol, fmt.Sprintf("厂商 %s 的默认协议", a.Provider)); err != nil {
		return nil, err
	}

	candidates := override
	if candidates == nil {
		injected, err := s.providerProtocols(a, model)
		if err != nil {
			return nil, err
		}
		candidates = injected
	}
	if len(candidates) == 0 {
		candidates = defaultProbeProtocols
	}
	for _, p := range candidates {
		if err := add(p, fmt.Sprintf("厂商 %s 的路由", a.Provider)); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// providerProtocols 是 resolver 的默认实现：该厂商在 provider_routes 里声明过的协议。
// 注入 Options.RouteProtocols 时以注入的为准。读库失败时报错而不是返回 nil：返回 nil 会让
// 探活放宽成三条入站协议全试，等于去打这家没声明的路径，把一次读库故障变成对错误地址的上游调用。
func (s *Server) providerProtocols(a store.Account, model string) ([]string, error) {
	if s.routeProtocols != nil {
		return s.routeProtocols(a.Provider, model), nil
	}
	protocols, err := s.store.ProviderProtocols(a.Provider)
	if err != nil {
		return nil, fmt.Errorf("读厂商 %s 的协议路由失败，探活不知道该试哪些协议（不放宽成三种全试，那会打到这家没声明的路径）: %w",
			a.Provider, err)
	}
	return protocols, nil
}

// retryOtherProtocol 判断这次失败是否可能只是协议（路径）不对，值得换一种协议再试。
// 404 是路径不存在，405 是方法不允许，403 有的网关就是按路径拒（commandcode 对非 /messages
// 的 claude-* 就返回 403）；400 只在错误文本指向端点、路径或「不支持该协议」时才算。
// 401 是凭据问题，与协议无关，不在此列。
func retryOtherProtocol(res ModelProbe) bool {
	switch res.HTTPStatus {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		lower := strings.ToLower(res.Message)
		for _, hint := range []string{
			"endpoint", "path", "route", "not supported", "unsupported", "does not support",
			"路径", "端点", "不支持",
		} {
			if strings.Contains(lower, hint) {
				return true
			}
		}
	}
	return false
}

// ProbeModel 用一条 max_tokens=1 的请求判断账号能不能用这个模型，
// 顺手发现这个模型该用哪种协议（候选协议见 routeProtocolOrder）。
func (s *Server) ProbeModel(ctx context.Context, a store.Account, model string) (ModelProbe, error) {
	return s.ProbeModelWithProtocols(ctx, a, model, nil)
}

// ProbeModelWithProtocols 探活一个模型。candidates 顶替 resolver 给的中间一串协议（nil 表示
// 用 resolver，也就是该厂商声明过的那些），厂商默认协议与已发现的协议仍由 routeProtocolOrder 排好。
//
// 先用厂商默认协议试一次：2xx 就是结论；401 是凭据问题，不换协议，停用账号并停下；
// 只有可能只是路径不对时才换下一个候选（见 retryOtherProtocol）。
// 换协议后的第一次 2xx 即结论，把该协议写进 account_models.protocol，剩下的候选不再试。
// 换协议后的失败都不是结论：那里的 401 只能说明这个协议不认这套凭据头（anthropic 用
// x-api-key，另两家用 Bearer），其它失败也只是这条路不通，所以接着试下一个候选；
// 全部试完仍失败时，结论取厂商默认协议那一次。
//
// 每次上游调用都在 requests 里记一行，换协议前的中间尝试也记（见 recordProbeAttempts）：
// 一次 403 触发三次调用就应当是三行，账实一致比行数好看重要。
//
// 结论的写法：可达性放在 ModelProbe.Access（`allowed` / `forbidden` / `missing` / `error`），
// 由调用方写进 store.SetAccountModelAccess；401 不是模型层面的结论，那一栏留空。
// 协议发现成功时这里自己写 store.SetAccountModelProtocol（账号级覆盖，见 DESIGN 5.13）。
func (s *Server) ProbeModelWithProtocols(ctx context.Context, a store.Account, model string,
	candidates []string) (ModelProbe, error) {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return ModelProbe{Model: model, Protocol: a.Protocol}, err
	}
	secretValue := string(key)

	order, err := s.routeProtocolOrder(a, model, candidates)
	if err != nil {
		res := ModelProbe{Model: model, Protocol: a.Protocol, Message: err.Error()}
		res.setAccess(store.AccessError)
		return res, err
	}

	// 厂商默认协议那一次是「按厂商配置打」的真相：401 判定与全失败时的结论都取它。
	// 它不一定排在第一位（已发现的协议会排在前面），所以按下标记下来。
	primaryIdx := 0
	for i, protocol := range order {
		if protocol == a.Protocol {
			primaryIdx = i
			break
		}
	}

	var attempts []probeAttempt
	var storeErr error
	for i, protocol := range order {
		att := s.probeOnce(ctx, a, model, protocol, secretValue)
		attempts = append(attempts, att)

		if att.res.Access == store.AccessAllowed {
			// 2xx 就是结论：记下这次用的协议，剩下的候选不再试。
			s.recordProbeAttempts(a, model, attempts, i)
			if err := s.store.SetAccountModelProtocol(a.ID, model, protocol); err != nil {
				storeErr = fmt.Errorf("账号 %s 的模型 %s 探活成功但记录协议失败: %w", a.Name, model, err)
			}
			return att.res, storeErr
		}

		// 401 没有对应的可达性取值（凭据问题不是这个模型的结论），只有旧词表里那一栏认得出它。
		if i == primaryIdx && att.res.HTTPStatus == http.StatusUnauthorized {
			// 401 是凭据问题，与协议无关：停用账号并立即收工。
			// 停用失败不能改写结论：调用方靠 auth 决定「碰到 401 就停下」。
			s.recordProbeAttempts(a, model, attempts, i)
			if err := s.store.SetEnabled(a.ID, false, att.res.Message); err != nil {
				storeErr = fmt.Errorf("停用账号 %s 失败，需手工处理: %w", a.Name, err)
			}
			return att.res, storeErr
		}

		if i == primaryIdx && !retryOtherProtocol(att.res) {
			// 厂商默认协议那次的明确结论（模型不存在、5xx、网络错误）与协议无关，不必再花调用。
			s.recordProbeAttempts(a, model, attempts, i)
			return att.res, nil
		}

		// 其余情况一律接着试下一个候选：可能只是路径不对，也可能换协议后的这条不通。
	}

	// 候选协议都试过还是失败：厂商默认协议那次的判定就是结论，protocol 保持原值。
	s.recordProbeAttempts(a, model, attempts, primaryIdx)
	return attempts[primaryIdx].res, storeErr
}

// probeOnce 用指定协议发一条 max_tokens=1 的请求，只得出结论，不写库。
// 开始时间在这里入口就取好：任何分支（编码失败、构造失败）都有时间戳，
// 不会写出 1970 的行（见 recordProbeAttempts 的零值保护）。
func (s *Server) probeOnce(ctx context.Context, a store.Account, model, protocol, key string) probeAttempt {
	att := probeAttempt{res: ModelProbe{Model: model, Protocol: protocol}, start: s.now()}
	res := &att.res

	ir := &translate.Request{
		Model:     model,
		MaxTokens: 1,
		Messages: []translate.Message{{
			Role:   translate.RoleUser,
			Blocks: []translate.Block{{Kind: translate.BlockText, Text: "ping"}},
		}},
	}
	raw, err := translate.EncodeRequest(protocol, ir, model)
	if err != nil {
		res.setAccess(store.AccessError)
		res.Message = "按协议 " + protocol + " 编码探活请求失败: " + err.Error()
		return att
	}
	// 模型同步没有客户端请求可依，入站头传 nil：客户端头透传只作用于转发路径（DESIGN 5.11）。
	upReq, err := s.buildRequest(ctx, a, protocol, protocol, nil, raw, ir, key, "")
	if err != nil {
		res.setAccess(store.AccessError)
		res.Message = "构造探活请求失败: " + err.Error()
		return att
	}

	resp, err := s.client.Do(upReq)
	res.Latency = s.now().Sub(att.start)
	if err != nil {
		res.setAccess(store.AccessError)
		res.Message = scrubSecret("上游请求失败: "+err.Error(), key)
		return att
	}
	defer resp.Body.Close()

	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		att.usage = readProbeUsage(protocol, body)
		res.setAccess(store.AccessAllowed)
		return att
	}

	msg := scrubSecret(readErrorBody(resp.Body), key)
	res.Message = msg
	lower := strings.ToLower(msg)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// 凭据被拒不是这个模型的结论：可达性留空（调用方不写），旧词表那一栏记 auth
		// 只为「碰到 401 就停下整次探活」这条判定还认得出它。
		res.Access = ""
		res.Status = store.ModelAuthFailed
	case resp.StatusCode == http.StatusForbidden:
		res.setAccess(store.AccessForbidden)
	case resp.StatusCode == http.StatusNotFound:
		res.setAccess(store.AccessMissing)
	case resp.StatusCode == http.StatusBadRequest &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "model_not_found") ||
			strings.Contains(lower, "unknown model")):
		// 400 里只有明说模型不存在才算 missing，其余当临时失败，
		// 否则「no available providers match ...」这种会误判成模型名不存在。
		res.setAccess(store.AccessMissing)
	default:
		res.setAccess(store.AccessError)
	}
	return att
}

// probeUsage 是一次上游调用里读出来的用量。缓存读写同样要记账：
// anthropic 的 scanner 两个都读，chat 与 responses 只读缓存读，缓存写恒为 0。
// reasoning 与 total 是上游上报的思考 token 与总 token，探活行与转发行同一套判定入账。
type probeUsage struct {
	in         int64
	out        int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
	total      int64
}

// readProbeUsage 用该协议的 usage scanner 从响应体里读用量，读不到时为 0。
func readProbeUsage(protocol string, body []byte) probeUsage {
	scanner, err := translate.NewUsageScanner(protocol)
	if err != nil {
		return probeUsage{}
	}
	scanner.Feed(body)
	scanned := scanner.Usage()
	return probeUsage{
		in: scanned.In, out: scanned.Out, cacheRead: scanned.CacheRead, cacheWrite: scanned.CacheWrite,
		reasoning: scanned.Reasoning, total: scanned.Total,
	}
}

// probeRetryNote 是没得出判定的那几次上游调用的错误摘要前缀。换协议会多打一两次，
// 每一行都要能看出「这不是这次探活的结论」。
const probeRetryNote = "换协议前的尝试，未作结论"

// recordProbeAttempts 把这次探活的每一次上游调用各记一行 requests（verdict 是得出判定的那次）。
// 探活是真实计费的调用，打了几次就记几行，中间的重试也记：45 个 403 模型打 135 次却只留
// 45 行的话，用量与花费都对不上。入站与出站都取那次实际用的协议；只有 2xx 那次才 ok。
// 没有开始时间的尝试不写（正常路径下 probeOnce 入口就取了时间）。记账失败只写日志，
// 不改变探活结论。
func (s *Server) recordProbeAttempts(a store.Account, model string, attempts []probeAttempt, verdict int) {
	for i, att := range attempts {
		if att.start.IsZero() {
			s.logf("账号 %s 的模型 %s 有一次探活调用没有开始时间，跳过记账", a.Name, model)
			continue
		}
		protocol := att.res.Protocol
		if protocol == "" {
			protocol = a.Protocol
		}
		msg := att.res.Message
		if i != verdict {
			msg = probeRetryNote
			if att.res.Message != "" {
				msg += "：" + att.res.Message
			}
		}
		rec := &store.RequestRecord{
			TS:             att.start.Unix(),
			Inbound:        protocol,
			Outbound:       protocol,
			ModelRequested: model,
			ModelUsed:      model,
			AccountID:      a.ID,
			HTTPStatus:     att.res.HTTPStatus,
			OK:             i == verdict && att.res.HTTPStatus >= 200 && att.res.HTTPStatus < 300,
			InTokens:       att.usage.in,
			// 探活行也进 usage 汇总，所以同样按这次所用协议归一化输入 token：不归一化时
			// 新列的默认值 0 会让探活输入的花费整块消失（见 store.UncachedInTokens）。
			InTokensUncached: store.UncachedInTokens(protocol, att.usage.in, att.usage.cacheRead),
			OutTokens:        att.usage.out,
			CacheRead:        att.usage.cacheRead,
			CacheWrite:       att.usage.cacheWrite,
			// 探活也算真实花费，思考 token 与转发行走同一个判定：原值入列，
			// 只有「输出之外的一项」那份再按输出价计一次（见 DESIGN 5.5）。
			ReasoningTokens:      att.usage.reasoning,
			ReasoningTokensExtra: store.ReasoningExtra(att.usage.total, att.usage.in, att.usage.out, att.usage.reasoning),
			LatencyMS:            att.res.Latency.Milliseconds(),
			Error:                msg,
		}
		if err := s.store.InsertRequest(rec); err != nil {
			s.logf("写探活请求记录失败: %v", err)
		}
	}
}
