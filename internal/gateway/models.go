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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lax/internal/secret"
	"lax/internal/store"
	"lax/internal/translate"
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

// modelsEndpoint 解析账号的模型列表地址，同时返回这条地址该用哪种协议发凭据头。
// 地址与协议必须成对给出，下面五条规则同时决定这两样各自从哪来：
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
		// 地址只存在厂商行上：孤儿行的 a.BaseURL 是空串，拼出来的
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
		return "", "", fmt.Errorf("厂商 %s 没有模型列表接口，用 --models 手填，或用 lax provider set %s --models-path <路径> 补上模型列表路径",
			a.Provider, a.Provider)
	}
	return joinEndpoint(a.BaseURL, path), routeProtocol, nil
}

// SyncModels 按厂商覆盖与路由的 models_path 拉取上游的模型列表，按名字排序去重。
// 响应形状三种都认（见 parseModelListPage）：`{data:[…]}`、`{models:[…]}`
// 与顶层数组，形状不同的上游（例如 ollama 的 /api/tags）不必再靠 --models 手填。
// gemini 的 `{models:[{name:"models/…"}]}` 复用第二种形状，另加两条后处理：条目名去掉一个
// `models/` 前缀、按 `supportedGenerationMethods` 折成能力位（不再按它过滤整条：能算向量的
// 模型也要进目录才选得到号）；分页按 `nextPageToken` 取完。
// 条目里能认出来的上下文长度、最大输出与协议集合跟着名字一起写进目录（`source='sync'`）。
func (s *Server) SyncModels(ctx context.Context, a store.Account) ([]string, error) {
	return s.SyncModelsWithEndpoint(ctx, a, "")
}

// modelListPageSize 是请求模型列表时带的页大小。gemini 的清单分页返回，
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
//
// 清单里的元数据（上下文长度、最大输出、支持的协议）顺手写进目录 `provider_models`
// （`source='sync'`，只写上游真的给出的字段）：路径与凭据头都在这里，
// 调用方拿到的只是名字，再让它去读一次 JSON 就多一份解析规则。写目录失败整次同步失败——
// 静默丢掉元数据会让下一次同步也补不回来，看起来却像成功。可达性仍由调用方写（`ReplaceModels`）。
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

	var models []listModel
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
		listed, next, isModelList := parseModelListPage(raw)
		if !isModelList {
			return nil, fmt.Errorf("上游返回的不是模型列表：认 %s 三种形状，检查 base-url %s 是否指向模型列表接口（三种形状都不匹配时用 --models 手填）",
				modelShapeList(), a.BaseURL)
		}
		for _, model := range listed {
			// 同名条目只留第一次出现的那一条：分页边界的重复与页内重复都按一个模型算。
			if seen[model.name] {
				continue
			}
			seen[model.name] = true
			models = append(models, model)
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
	sort.Slice(models, func(i, j int) bool { return models[i].name < models[j].name })
	// 目录按 (厂商, 模型) 写：来源固定 sync，四列元数据只带上游给出的那几个，
	// 没给的留空（UpsertProviderModels 只写非零/非空的值，不会把探活补的长度抹掉）。
	if err := s.store.UpsertProviderModels(a.Provider, catalogModels(models)); err != nil {
		return nil, fmt.Errorf("把上游清单写进厂商 %s 的模型目录失败: %w", a.Provider, err)
	}
	names := make([]string, len(models))
	for i, model := range models {
		names[i] = model.name
	}
	return names, nil
}

// catalogModels 把清单折成目录行：来源一律 `sync`。能力位只写上游真的声明过的那两类
// （chat / embedding，见 entryCapabilities）；supported_parameters 这类形状各家不同的字段仍不解析，
// 写进去只等于把一个猜出来的结论存成了事实。
func catalogModels(models []listModel) []store.ProviderModel {
	rows := make([]store.ProviderModel, 0, len(models))
	for _, model := range models {
		rows = append(rows, store.ProviderModel{
			Model:         model.name,
			Source:        store.SourceSync,
			ContextWindow: model.contextWindow,
			MaxOutput:     model.maxOutput,
			Protocols:     model.protocols,
			Capabilities:  model.capabilities,
		})
	}
	return rows
}

// modelListPageURL 给需要分页的上游追加分页参数：pageSize 每页都带，pageToken 从第二页起带。
// 只有 gemini 的清单按 pageSize / pageToken 翻页，paginated 为假时原样返回 target：地址里可能带
// 签名（`?sig=…`），经 url.Parse 再 Encode 会重排参数、改写转义，签名当场失效，请求直接换回 401。
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
// scrubSecret，明文不进错误信息与日志：拉列表失败的消息里常带上游回显的地址，
// 而地址可能带着查询串里的凭据。错误信息里写的是 target（配置里的地址），
// 不带这次追加的分页参数，便于照着命令里的取值排查。
// 模型列表请求与转发同一套头：先 setAuth 设凭据头，再叠厂商附加头。少叠一层附加头就会让
// 需要额外认证头的厂商在同步时换回 401，而同一个账号的转发却是通的。
func (s *Server) fetchModelPage(ctx context.Context, pageURL, target, protocol string,
	a store.Account, secretValue string) ([]byte, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("模型列表地址 %q 不合法，检查厂商 %s 的 base-url: %w", pageURL, a.Provider, err)
	}
	req.Header.Set("Accept", "application/json")
	// 凭据头跟着取到这条路径的协议走，不是厂商默认协议。
	setAuth(req, protocol, secretValue, a.AuthHeader, a.AuthScheme)
	// 模型列表请求没有模型名（{model} 展开为空时不发那条头），会话键也没有：
	// {session} 在这里每次请求现生成一个：模型同步没有客户端请求，没有真会话可沿，
	// 只能给一个本次请求专用的值。
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

// modelListShape 是模型列表响应的一种已知形状，按数组里的顺序依次尝试：先试的形状命中就不再往下看。
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

// listModel 是模型清单里的一条：名字，加上从条目对象里认出来的元数据。
//
// 字段缺失时留空（nil 表示上游没给），不猜默认值：清单里没写上下文长度不等于它是 0，
// 存成 0 会让这个模型在按长度过滤时被当成不可用。每列只从上游真的给出的字段里取。
type listModel struct {
	name          string
	contextWindow *int64
	maxOutput     *int64
	// protocols 是这条条目声明的线协议集合，按 wireProtocolOrder 的固定顺序排列。
	protocols []string
	// capabilities 是条目声明过的能力位（chat / embedding），按 capabilityOrder 排列。
	// 空集合是「上游没说」，不表示不支持：把它读成不支持，就会把一个其实能用的模型
	// 从候选里删掉。
	capabilities []string
}

// generateContentMethod 是 gemini 清单里「这个模型能做生成」的方法名，它折成 gemini 协议。
// 与它并列的 embedContent / batchEmbedContents 折成能力位 embedding（见 entryCapabilities）：
// 它们不是线协议，而是这个模型能干什么：向量化能力不进协议集合，模型同步与选号靠它区分
// 「这个模型能对话」与「这个模型只能算向量」。
const (
	generateContentMethod = "generateContent"
	embedContentMethod    = "embedContent"
	batchEmbedMethod      = "batchEmbedContents"
)

// 能力位取值与它们的固定顺序统一在 store：声明写入、/v1/models 输出与探活都读同一份，
// gateway 不另存一份名单。
var capabilityOrder = store.CapabilityValues()

// endpointProtocols 是「上游清单里声明的端点名 → 线协议」的映射，放在 gateway 而不是 translate：
// 它服务的是模型同步与探活这两件上游侧的事（同步时写进目录、探活时据此排候选），
// 与协议编解码无关，translate 那边仍然是纯编解码、不认识厂商清单的字段名。
//
// 只认表里列出的写法，未识别的不计：端点名各家写法不同，猜一个相近的名字等于给探活安排一条
// 这家未必声明过的路径，不如留空、由厂商路由决定。
var endpointProtocols = map[string]string{
	"/messages":             ProtocolAnthropic,
	"/chat/completions":     ProtocolChat,
	"/responses":            ProtocolResponses,
	"/systemone":            ProtocolSystemOne,
	"/embeddings":           ProtocolEmbeddings,
	"/audio/speech":         ProtocolSpeech,
	"/audio/transcriptions": ProtocolTranscriptions,
	"/generateContent":      ProtocolGemini,
	"generateContent":       ProtocolGemini,
}

// wireProtocolOrder 是协议集合落库与排候选的固定顺序：同一份上游数据多跑几次
// 得到同一串，与上游列出端点的先后无关。不固定顺序的话，同一份清单两次同步能存出两串不同的值，
// 对比数据库时看不出到底改了什么。向量化排在最后：它是唯一不会成为对话默认协议的线协议。
var wireProtocolOrder = []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses, ProtocolGemini, ProtocolSystemOne, ProtocolEmbeddings, ProtocolSpeech, ProtocolTranscriptions}

// endpointProtocol 查端点名对应的线协议。表里没有的写法返回 false，调用方直接不计这个取值。
func endpointProtocol(name string) (string, bool) {
	protocol, ok := endpointProtocols[strings.TrimSpace(name)]
	return protocol, ok
}

// orderedProtocols 把协议集合按 wireProtocolOrder 排好并去重，空集合返回 nil（落库成空串）。
func orderedProtocols(set map[string]bool) []string {
	var out []string
	for _, protocol := range wireProtocolOrder {
		if set[protocol] {
			out = append(out, protocol)
		}
	}
	return out
}

// parseModelListPage 按 modelListShapes 的顺序解析上游的模型列表响应，返回清单（名字加元数据，
// 按名字排序去重）与下一页的游标（`nextPageToken`）。第三个返回值为 false 表示三种形状的字段都不存在，
// 响应不是模型列表。只有字段不存在才试下一种形状：字段在但不是数组、或数组为空，都算「上游返回空列表」
// （返回空切片与 true），不再拿下一个形状兜底——`{"data":[]}` 与 `{"models":[]}` 都是空列表，不是形状不对。
// 游标只看顶层 `nextPageToken`：没有、不是字符串或为空白都当作「没有下一页」。
func parseModelListPage(raw []byte) (models []listModel, nextToken string, isModelList bool) {
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
		return modelEntries(entries), "", true
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
		return modelEntries(entries), nextPageToken(top), true
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

// modelEntries 从条目列表里折出清单：名字加元数据，按名字排序去重。缺名字的条目
// （一个字段都没有、或全是空白）跳过；同名的条目只留第一次出现的那一条，后面的元数据不合并。
//
// 不再按 supportedGenerationMethods 过滤：以前 embedding 一类没有 generateContent 的条目
// 整条不进清单，于是向量化模型连目录都进不去，也就无法选号。现在它们照旧进目录，
// 只是能力位记成 embedding，能不能用由可达性与路由判定说：一个只能算向量的模型被禁在
// 目录之外时，它连选号的候选都不是。
//
// 去前缀与形状无关：凡是以 `models/` 开头的名字都去一个前缀。名字里带前缀的不只 gemini，
// 而路由的 `{model}` 占位符在请求时只放回裸名，库里存带前缀的名字会让上游收到
// `models/models/…`：两层前缀不是模型名，上游会当成不存在的模型回 404。
func modelEntries(entries []json.RawMessage) []listModel {
	seen := map[string]bool{}
	var models []listModel
	for _, entry := range entries {
		name := modelEntryName(entry)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, listModel{
			name:          name,
			contextWindow: entryContextWindow(entry),
			maxOutput:     entryMaxOutput(entry),
			protocols:     entryProtocols(entry),
			capabilities:  entryCapabilities(entry),
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].name < models[j].name })
	return models
}

// entryCapabilities 把条目声明过的能力位折成 chat / embedding，按 capabilityOrder 排好去重。
// 两个来源、三种写法：
//
//   - supportedGenerationMethods 里的 generateContent 是 chat，embedContent 与
//     batchEmbedContents 是 embedding；
//   - supported_endpoints 里的 /chat/completions 是 chat，/embeddings 是 embedding，
//     /audio/speech 是 speech，/audio/transcriptions 是 transcription。
//
// 两个键都没有时返回 nil：上游没说它能干什么，能力位就留空，不硬塞 chat。
func entryCapabilities(entry json.RawMessage) []string {
	set := map[string]bool{}
	if methods, ok := generationMethods(entry); ok {
		for _, method := range methods {
			switch method {
			case generateContentMethod:
				set[store.CapabilityChat] = true
			case embedContentMethod, batchEmbedMethod:
				set[store.CapabilityEmbedding] = true
			}
		}
	}
	if endpoints, ok := entryEndpoints(entry); ok {
		for _, endpoint := range endpoints {
			switch endpoint {
			case "/chat/completions":
				set[store.CapabilityChat] = true
			case "/embeddings":
				set[store.CapabilityEmbedding] = true
			case "/audio/speech":
				set[store.CapabilitySpeech] = true
			case "/audio/transcriptions":
				set[store.CapabilityTranscription] = true
			}
		}
	}
	var out []string
	for _, capability := range store.CapabilityValues() {
		if set[capability] {
			out = append(out, capability)
		}
	}
	return out
}

// generationMethods 取条目的 supportedGenerationMethods，第二个返回值是字段在不在。
// 字段在但解不成字符串数组时返回空列表与 true：那种条目没有可用的方法列表。
func generationMethods(entry json.RawMessage) ([]string, bool) {
	obj := entryObject(entry)
	value, ok := obj["supportedGenerationMethods"]
	if !ok {
		return nil, false
	}
	var methods []string
	_ = json.Unmarshal(value, &methods)
	return methods, true
}

// entryEndpoints 取条目的 supported_endpoints，第二个返回值是字段在不在。
func entryEndpoints(entry json.RawMessage) ([]string, bool) {
	obj := entryObject(entry)
	value, ok := obj["supported_endpoints"]
	if !ok {
		return nil, false
	}
	var endpoints []string
	_ = json.Unmarshal(value, &endpoints)
	return endpoints, true
}

// entryContextWindow 取条目的上下文长度：commandcode 与 openrouter 写在 `context_length`，
// gemini 写在 `inputTokenLimit`。两个键都没有、或者不是正数时留空。
func entryContextWindow(entry json.RawMessage) *int64 {
	obj := entryObject(entry)
	if n := jsonPositiveInt(obj["context_length"]); n != nil {
		return n
	}
	return jsonPositiveInt(obj["inputTokenLimit"])
}

// entryMaxOutput 取条目的最大输出：gemini 写在 `outputTokenLimit`，openrouter 藏在
// `top_provider.max_completion_tokens` 里。其它上游没有这一项，两个键都缺失时留空。
func entryMaxOutput(entry json.RawMessage) *int64 {
	obj := entryObject(entry)
	if n := jsonPositiveInt(obj["outputTokenLimit"]); n != nil {
		return n
	}
	var topProvider map[string]json.RawMessage
	if err := json.Unmarshal(obj["top_provider"], &topProvider); err != nil {
		return nil
	}
	return jsonPositiveInt(topProvider["max_completion_tokens"])
}

// entryProtocols 把条目声明支持的协议折成线协议集合，按 wireProtocolOrder 的固定顺序返回：
//
//   - `supported_endpoints`（commandcode）里每个端点名按 endpointProtocols 折算，未识别的不计；
//   - `supportedGenerationMethods`（gemini）只有 generateContent 折成 gemini 协议，
//     countTokens、embedContent 这些是接口动作而不是协议，不算。
//
// 两个键都没有时返回 nil：上游没声明支持哪些协议，落库就是空串，探活按厂商默认协议与路由的顺序试。
func entryProtocols(entry json.RawMessage) []string {
	obj := entryObject(entry)
	set := map[string]bool{}
	var endpoints []string
	if err := json.Unmarshal(obj["supported_endpoints"], &endpoints); err == nil {
		for _, name := range endpoints {
			if protocol, ok := endpointProtocol(name); ok {
				set[protocol] = true
			}
		}
	}
	if methods, ok := generationMethods(entry); ok && slices.Contains(methods, generateContentMethod) {
		set[ProtocolGemini] = true
	}
	return orderedProtocols(set)
}

// entryObject 把条目解成键值表。条目本身是字符串（顶层数组的 `["m1"]` 写法）时没有对象可读，
// 返回 nil：名字照取，元数据一律留空。
func entryObject(entry json.RawMessage) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil {
		return nil
	}
	return obj
}

// jsonPositiveInt 取一个 JSON 数字。键缺失、解不成数字、或不是正数时返回 nil：
// 0 与负数在这几列上没有意义，当作「上游没给」，不拿 0 冒充上下文长度。
// 整数写法之外只收整数值的浮点（`200000.0` 收，`1.5` 不收）。
func jsonPositiveInt(value json.RawMessage) *int64 {
	// 只认裸数字：`json.Number` 连 `"200000"` 这种带引号的写法也收，而上游把长度写成字符串
	// 属于另一套形状，不当成同一个字段。
	if len(value) == 0 || value[0] == '"' {
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(value, &number); err != nil {
		return nil
	}
	if n, err := number.Int64(); err == nil {
		if n <= 0 {
			return nil
		}
		return &n
	}
	f, err := number.Float64()
	if err != nil || f <= 0 || f != float64(int64(f)) {
		return nil
	}
	n := int64(f)
	return &n
}

// modelEntryName 取一个条目的模型名：按 id → name → model 的顺序取第一个非空字符串。
// 条目本身是字符串时直接用它（顶层数组的 `["m1"]` 写法）。名字以 `models/` 开头时去掉一个前缀
// （gemini 的清单这么写，见 modelNames 的注释）。
func modelEntryName(entry json.RawMessage) string {
	var s string
	if err := json.Unmarshal(entry, &s); err == nil {
		return bareModelName(strings.TrimSpace(s))
	}
	obj := entryObject(entry)
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
// 名字里是否带前缀与上游形状无关，这一条对所有形状都生效——路由的 `{model}`
// 占位符在请求时把裸名放回路径，库里存带前缀的名字会让上游收到两层前缀。
func bareModelName(name string) string {
	if rest, ok := strings.CutPrefix(name, "models/"); ok {
		return rest
	}
	return name
}

// maxModelListBytes 是一次读模型列表响应的上限。`model sync` 与 `account test` 共用这一个值：
// 两条路径对同一份清单必须得出同一个结论。清单本身并不小——commandcode 的 94 个模型就有 18KB——
// 按更小的上限截断会让同步拿到完整清单、`account test` 却报「上游返回的不是模型列表」：
// 同一份清单在两条路径上得出两个结论，排查时无法分辨是上游变了还是网关截断了。
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
	listed, _, isModelList := parseModelListPage(raw)
	if !isModelList {
		return nil, false
	}
	models := make([]string, len(listed))
	for i, model := range listed {
		models[i] = model.name
	}
	return models, true
}

// ModelProbe 是单个模型在一次探活里的结论。
//
// Access 是结论的正文：`allowed` / `forbidden` / `missing`（取值定义在 store.Access*），
// 调用方拿它写 store.SetAccountModelAccess。**401 不是模型层面的结论**：凭据被拒时这一栏留空，
// 表示「这一次不要写可达性」；否则一次全局凭据失效会把每个模型都标成 forbidden，
// 换回凭据后那份清单仍全部不可用。连接层失败、429/5xx、其它 400 与本地编码或构造失败同样留空
// （见 Undecided）：这些不是模型层面的结论，写成 error 会把一次 TLS 超时变成「模型不可用」。
//
// Undecided 表示这次没拿到足以改可达性的 HTTP 判定（连接层失败、429/5xx、
// 其它 400、本地编码或构造失败）。调用方不写 access，保留上一行结论。
//
// Status 是迁移 0015 之前的状态词表，由 Access 推出，只为尚未改完的 internal/cli 保留：
// Access 非空时两栏一起有值；Access 留空时 `auth` 表示凭据被拒、`error` 表示没得出判定，
// 旧词表那边因此仍能显示一行，不会被当成可用。
//
// Protocol 是这次实际用到的协议：发现成功时是发现到的那个，其余情况下是厂商默认协议。
type ModelProbe struct {
	Model      string
	Access     string
	Undecided  bool
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

// setUndecided 记下「这次没拿到足以改可达性的判定」：可达性留空（调用方不写，保留上一行结论），
// 旧词表那一栏仍记 error 供尚未改完的调用方显示，Undecided 置真让调用方知道这一行不是结论。
func (res *ModelProbe) setUndecided() {
	res.Access = ""
	res.Undecided = true
	res.Status = store.ModelError
}

// probeTry 是一次连接层失败的上游调用：开始时间、耗时与那次调用的错误摘要。
// 连接层失败会重试，这几次调用各自要留一行账，用 probeTransportRetryNote 与判定行区分开。
// status 恒为 0：一条 HTTP 响应都没拿到，没有状态码可记。
type probeTry struct {
	start   time.Time
	latency time.Duration
	status  int
	message string
}

// probeAttempt 是一次协议尝试的完整结果：结论加记账需要的开始时间、用量与连接层重试。
type probeAttempt struct {
	res     ModelProbe
	start   time.Time
	usage   probeUsage
	retries []probeTry
}

// defaultProbeProtocols 是探活候选的兜底顺序：账号没有厂商行（或该厂商一个路由都没声明）
// 时三条入站协议全试。厂商声明过路由的只试那些（见 routeProtocolOrder），循环本身不认识厂商。
// 兜底里不含 gemini：没有厂商声明 gemini 路由时，模型名与地址都没有确定的形状，
// 不该拿它去打一个猜出来的地址。
var defaultProbeProtocols = []string{
	translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses,
}

// isWireProtocol 判断一个取值是不是线协议。判定只有 store.IsWireProtocol 一份：gemini
// 也是线协议（有原生入站路径，流式与非流式双向都支持），厂商声明了 gemini 路由时探活要能用它。
// 别的值不能进探活：编码器遇见未知协议会报错，静默落进 chat 分支更糟——那会把一条配置
// 错误变成对错误地址的上游调用。
func isWireProtocol(p string) bool {
	return store.IsWireProtocol(p)
}

// routeProtocolOrder 给出探活一个模型的完整有序候选，顺序与去重都在这里定：
//
//  1. 库里已经发现过这个模型的协议排最前，它是「按事实打」的那次，省掉每次 --refresh
//     白打的一次 404；
//  2. 目录里这个模型声明支持的协议（provider_models.protocols）紧接着：上游自己说了它支持哪些协议
//     （commandcode 的 supported_endpoints、gemini 的 supportedGenerationMethods），先按它打就少打几次
//     错协议的请求。声明不在第 4 步的候选范围之内也打：模型级的声明本身就是这条路径存在的证据
//     （systemone 的路径固定，gateway.Endpoint 也能按协议给出后缀），不必厂商在 provider_routes
//     里再声明一条同名路由。这个位置上的失败不构成结论（循环本来就在非 primary 位置继续试），
//     只有 2xx 才算数；与第 1 步冲突时第 1 步优先；
//  3. 厂商的有效默认协议紧接其后，它既是对厂商配置的验证，也是 401 判定与全失败结论的依据；
//  4. 其余按 resolver 给的顺序：Options.RouteProtocols 注入时用它，否则用该厂商在
//     provider_routes 里声明过的协议（按协议名排序），都没有则三种全试。
//
// 账号行不再存协议（迁移 0014），a.Protocol 是厂商默认协议的派生值：厂商没有路由时它是空串，
// 候选就只剩第 4 步的三种全试。override 非 nil 时顶替第 4 步的来源（探活自己指定候选时用）。
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

	// 目录里声明的协议排在厂商默认协议之前，且不再要求它在 candidates 之内：声明本身就是这条路径
	// 存在的证据（见函数注释第 2 步）。排在它前面的只有已发现的协议，后面的失败本来就不作结论。
	for _, p := range s.declaredProtocols(a, model) {
		if err := add(p, fmt.Sprintf("厂商 %s 的模型目录", a.Provider)); err != nil {
			return nil, err
		}
	}

	if err := add(a.Protocol, fmt.Sprintf("厂商 %s 的默认协议", a.Provider)); err != nil {
		return nil, err
	}
	for _, p := range candidates {
		if err := add(p, fmt.Sprintf("厂商 %s 的路由", a.Provider)); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// declaredProtocols 读目录里这个模型声明支持的协议（provider_models.protocols）。
// 读不出来就当没有：一次读库失败不该让探活停下，候选仍按厂商路由与默认协议排。
func (s *Server) declaredProtocols(a store.Account, model string) []string {
	protocols, err := s.store.ProviderModelProtocols(a.Provider, model)
	if err != nil {
		s.logf("读厂商 %s 的模型 %s 声明的协议失败，按未声明处理: %v", a.Provider, model, err)
		return nil
	}
	return protocols
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
//
// hasEmbeddings 表示候选里还有向量化那一项：厂商同时声明了 openai-embeddings 路由时，一个
// 400 也可能是「这个模型不是对话模型」——那种情况下直接判 error 会把一个能用向量化的模型
// 标成失败：那种情况下直接下结论，就把一个只能算向量的模型当成了不能用。有向量化候选时
// 所有 400 都值得再试一次。
func retryOtherProtocol(res ModelProbe, hasEmbeddings bool) bool {
	switch res.HTTPStatus {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		if hasEmbeddings {
			return true
		}
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
// 循环本身见 probeOrder；这里只排候选、把向量化挤到最后，并认出厂商默认协议那一次的位置。
func (s *Server) ProbeModelWithProtocols(ctx context.Context, a store.Account, model string,
	candidates []string) (ModelProbe, error) {

	order, err := s.routeProtocolOrder(a, model, candidates)
	if err != nil {
		res := ModelProbe{Model: model, Protocol: a.Protocol, Message: err.Error()}
		res.setAccess(store.AccessError)
		return res, err
	}

	if idx := slices.Index(order, ProtocolEmbeddings); idx >= 0 && idx != len(order)-1 {
		// 向量化排在最后：它只在对话候选都不成时才值得试，垫在中间会让每个模型探活都多打一次。
		// 排序只影响候选顺序，不影响已发现协议与目录声明那两步的优先级。
		rest := append([]string{}, order[idx+1:]...)
		order = append(order[:idx:idx], rest...)
		order = append(order, ProtocolEmbeddings)
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

	return s.probeOrder(ctx, a, model, order, primaryIdx)
}

// ProbeModelProtocol 只按指定协议探活一个模型，候选就是这一条。
//
// `model probe --protocol` 用它只验一条路：不读已发现协议、不读目录声明、不取厂商默认协议，
// 所以没有多协议顺序，也没有「换协议之后那几次不作数」这一回事——打到的就是这一条，
// 401 照样是凭据被拒、停用账号。protocol 不是线协议时是用法错误，不发上游请求，
// 带出的结论标 Undecided，调用方不拿它写可达性。
// 判定与 ProbeModelWithProtocols 是同一套（同一个 probeOrder），只打一条路时不另写一份。
func (s *Server) ProbeModelProtocol(ctx context.Context, a store.Account, model, protocol string) (ModelProbe, error) {
	if !isWireProtocol(protocol) {
		msg := fmt.Sprintf("探活协议 %q 不是线协议，可用 %s", protocol, strings.Join(store.WireProtocols(), "、"))
		return ModelProbe{Model: model, Access: "", Undecided: true, Message: msg}, errors.New(msg)
	}
	return s.probeOrder(ctx, a, model, []string{protocol}, 0)
}

// probeOrder 按给定的候选顺序探活一个模型，两个入口共用这一个循环。primaryIdx 是厂商默认协议
// （或单协议探活唯一那一条）在 order 里的位置，401 判定与全失败时的结论都取它。
//
// 先用厂商默认协议试一次：2xx 就是结论；401 是凭据问题，不换协议，停用账号并停下；
// 只有可能只是路径不对时才换下一个候选（见 retryOtherProtocol）。
// 换协议后的第一次 2xx 即结论，把该协议写进 account_models.protocol，剩下的候选不再试。
// 换协议后的失败都不是结论：那里的 401 只能说明这个协议不认这套凭据头（anthropic 用
// x-api-key，另两家用 Bearer），其它失败也只是这条路不通，所以接着试下一个候选；
// 全部试完仍失败时，结论取厂商默认协议那一次。
//
// 每次上游调用都在 requests 里记一行，换协议前的中间尝试与连接层失败的重试也记
// （见 recordProbeAttempts）：一次 403 触发三次调用就应当是三行，账实一致比行数好看重要。
//
// 结论的写法：可达性放在 ModelProbe.Access（`allowed` / `forbidden` / `missing`），
// 由调用方写进 store.SetAccountModelAccess；401、连接层失败、429/5xx 等不构成结论的情况
// 那一栏留空，后者另有 Undecided（见 ModelProbe）。
// 协议发现成功时这里自己写 store.SetAccountModelProtocol（账号级覆盖）。
func (s *Server) probeOrder(ctx context.Context, a store.Account, model string,
	order []string, primaryIdx int) (ModelProbe, error) {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return ModelProbe{Model: model, Protocol: a.Protocol}, err
	}
	secretValue := string(key)

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

		if i == primaryIdx && !retryOtherProtocol(att.res, slices.Contains(order, ProtocolEmbeddings)) {
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

	ir := &translate.Request{Model: model}
	var raw []byte
	var err error
	// probeContentType 非空时覆盖 buildRequest 给的 Content-Type：语音转写的探活报文是
	// multipart，默认的 application/json 只会让上游回一个「unexpected content type」。
	var probeContentType string
	// embedding 决定这次探活打哪个动作：向量化协议打 embeddings，gemini 的 embedding 模型打
	// :embedContent。它就是 buildRequest 用来改写路径的那一个开关。
	embedding := protocol == ProtocolEmbeddings ||
		(protocol == ProtocolGemini && s.geminiEmbeddingOnly(a, model))
	switch {
	case protocol == ProtocolSystemOne:
		// 决策模型的探活不是 max_tokens=1 的对话请求：决策端点不认 messages，发了只会换回 404，
		// 把「模型可用」误判成「模型不存在」，探活反而会把一个能用的模型标成失败。
		raw, err = systemOneProbeBody(model)
	case protocol == ProtocolEmbeddings:
		raw, err = embeddingsProbeBody(model)
	case protocol == ProtocolSpeech:
		// 语音合成发一条结构错的请求：input 是 null。探活不要真实合成（那要花字符费），
		// 只让上游在参数校验上回一句，按错误码判路由与凭据（见 speechProbeVerdict）。
		raw, err = speechProbeBody(model)
	case protocol == ProtocolTranscriptions:
		// 语音转写同理：multipart 里只有 model、没有 file。
		raw, probeContentType, err = transcriptionsProbeBody(model)
	case embedding:
		raw, err = geminiEmbedProbeBody()
	default:
		ir.MaxTokens = 1
		ir.Messages = []translate.Message{{
			Role:   translate.RoleUser,
			Blocks: []translate.Block{{Kind: translate.BlockText, Text: "ping"}},
		}}
		raw, err = translate.EncodeRequest(protocol, ir, model)
	}
	if err != nil {
		res.setUndecided()
		res.Message = "按协议 " + protocol + " 编码探活请求失败: " + err.Error()
		return att
	}
	// 模型同步与探活没有客户端请求可依，入站头传 nil：客户端头透传只作用于转发路径。
	upReq, err := s.buildRequest(ctx, a, protocol, nil, protocol, nil, raw, ir, key, "", embedding)
	if err != nil {
		res.setUndecided()
		res.Message = "构造探活请求失败: " + err.Error()
		return att
	}
	if probeContentType != "" {
		upReq.Header.Set("Content-Type", probeContentType)
	}

	// 连接层失败（一条 HTTP 响应都没拿到）在同一个协议内重试：DNS、连接被拒、TLS 握手失败与
	// 读超时多半是本机网络或中转抖动，按判定规则这类失败不改可达性，白丢一次调用只会让一个
	// 已确认可用的模型看起来「没结论」。重试之间不退避（单个模型的调用超时已经是 60 秒），
	// 每次重试前先看 ctx：已取消就停。请求体只构造一次，重试用 GetBody 重放；
	// GetBody 为 nil 时没有可重放的 body，不重试。
	var resp *http.Response
	for try := 0; ; try++ {
		tryStart := att.start
		resp, err = s.client.Do(upReq)
		latency := s.now().Sub(tryStart)
		if err == nil {
			res.Latency = latency
			break
		}
		tryMsg := scrubSecret("上游请求失败: "+err.Error(), key)
		if try >= probeTransportRetries || upReq.GetBody == nil || ctx.Err() != nil {
			res.Latency = latency
			res.setUndecided()
			res.Message = tryMsg
			return att
		}
		body, bodyErr := upReq.GetBody()
		if bodyErr != nil {
			// 连重放 body 都失败：不再往下试，按这一次失败落成没得出判定。
			res.Latency = latency
			res.setUndecided()
			res.Message = tryMsg
			return att
		}
		// 这一次调用要重试：它自己留一行账（见 recordProbeAttempts），不计入这次尝试的判定。
		att.retries = append(att.retries, probeTry{start: tryStart, latency: latency, message: tryMsg})
		next := upReq.Clone(ctx)
		next.Body = body
		upReq = next
		// 判定行的 ts 取真正给出结论那次调用的开始时间，不取重试之前的。
		att.start = s.now()
	}
	defer resp.Body.Close()

	res.HTTPStatus = resp.StatusCode
	// 语音两条协议的判定与对话相反：探测报文是故意写错的，400/422 正是预期，只证明
	// 路由存在、请求到了业务层；凭据有效与否这一条判定不出（见 speechProbeVerdict）。
	if protocol == ProtocolSpeech || protocol == ProtocolTranscriptions {
		msg := ""
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg = scrubSecret(readErrorBody(resp.Body), key)
		}
		res.Message = msg
		if resp.StatusCode == http.StatusUnauthorized {
			// 与对话探活一致：凭据问题不是这个模型的结论，可达性留空，旧词表记 auth
			// 让 probeOrder 停用账号并停下。
			res.Access = ""
			res.Status = store.ModelAuthFailed
			return att
		}
		access, note, ok := speechProbeVerdict(resp.StatusCode, msg)
		if ok {
			res.setAccess(access)
		} else {
			res.setUndecided()
		}
		if note != "" {
			res.Message = note
		}
		return att
	}
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
		// 其余（其它 400、402、405、429、5xx）都不是模型层面的结论：可达性留空，调用方保留上一行；
		// 旧词表那一栏仍记 error 供显示，Undecided 置真。
		res.setUndecided()
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
	if protocol == ProtocolSystemOne {
		// 决策协议没有 usage scanner（translate 的四套编解码器不认它）：两个字段直读，
		// 没有缓存与思考两项：决策端点的用量只有输入与输出。
		in, out := systemOneUsage(body)
		return probeUsage{in: in, out: out}
	}
	if protocol == ProtocolEmbeddings {
		// 向量化同样没有 scanner：只有一个 prompt_tokens，输出恒为 0，按对话形状去解只会拿到零值。
		return probeUsage{in: embeddingsUsage(body)}
	}
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

// probeTransportRetryNote 是连接层失败重试那几行请求记录的错误摘要前缀。它们与判定行在同一列上，
// 只能靠前缀区分：这一行调用没拿到任何 HTTP 响应，因此也不是这次探活的结论。
const probeTransportRetryNote = "连接层失败重试，未作结论"

// probeTransportRetries 是同一个协议内连接层失败最多再试几次（共 probeTransportRetries+1 次调用）。
// 不换协议：偶发失败多半出在本机网络或中转上，换一条路径只会多花调用。
const probeTransportRetries = 2

// recordProbeAttempts 把这次探活的每一次上游调用各记一行 requests（verdict 是得出判定的那次）。
// 探活是真实计费的调用，打了几次就记几行，中间的重试也记：45 个 403 模型打 135 次却只留
// 45 行的话，用量与花费都对不上。入站与出站都取那次实际用的协议；只有 2xx 那次才 ok。
// 连接层失败的重试每行先写、判定行后写：重试行没有状态码、没有用量，错误摘要以
// probeTransportRetryNote 开头。
// 没有开始时间的尝试不写（正常路径下 probeOnce 入口就取了时间）。记账失败只写日志，
// 不改变探活结论。
func (s *Server) recordProbeAttempts(a store.Account, model string, attempts []probeAttempt, verdict int) {
	for i, att := range attempts {
		protocol := att.res.Protocol
		if protocol == "" {
			protocol = a.Protocol
		}
		// 连接层失败的那几次各留一行账：它们与判定行同源（入站、出站、模型与账号）。
		for _, try := range att.retries {
			if try.start.IsZero() {
				s.logf("账号 %s 的模型 %s 有一次探活重试没有开始时间，跳过记账", a.Name, model)
				continue
			}
			msg := probeTransportRetryNote
			if try.message != "" {
				msg += "：" + try.message
			}
			rec := &store.RequestRecord{
				TS:             try.start.Unix(),
				Inbound:        protocol,
				Outbound:       protocol,
				ModelRequested: model,
				ModelUsed:      model,
				AccountID:      a.ID,
				HTTPStatus:     try.status,
				OK:             false,
				LatencyMS:      try.latency.Milliseconds(),
				Error:          msg,
			}
			if err := s.store.InsertRequest(rec); err != nil {
				s.logf("写探活重试请求记录失败: %v", err)
			}
		}
		if att.start.IsZero() {
			s.logf("账号 %s 的模型 %s 有一次探活调用没有开始时间，跳过记账", a.Name, model)
			continue
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
			// 探活行也进 usage 汇总，所以同样按这次所用协议归一化输入 token：这一列是花费里输入
			// 那一项的乘数，留默认值 0 会让探活输入的花费整块消失（归一化只写在 store.UncachedInTokens
			// 一处，网关不按协议重算）。
			InTokensUncached: store.UncachedInTokens(protocol, att.usage.in, att.usage.cacheRead),
			OutTokens:        att.usage.out,
			CacheRead:        att.usage.cacheRead,
			CacheWrite:       att.usage.cacheWrite,
			// 探活也算真实花费，所以思考 token 与转发路径走同一个判定 store.ReasoningExtra：原值整列入列，
			// 只有被判定成「输出之外的一项」的那部分再按输出价计一次——判定只写在那一处，
			// 这里另写一套的话同一次调用在两张记录上会折出两个价。
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
