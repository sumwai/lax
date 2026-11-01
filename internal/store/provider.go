package store

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lax/internal/plugin"
	"lax/internal/translate"
)

var (
	// ErrProviderNotFound 表示厂商表里没有这一行。GetProvider 与 DeleteProvider 用 errors.Is 判断它；
	// LookupProviderRoute 不返回这个错误，那里用 ok=false 更好用。
	ErrProviderNotFound = errors.New("厂商不存在")

	// ErrProviderExists 表示厂商名已经被占用。`provider add` 只用它报错，不做隐式更新：
	// 改一家已有的厂商走 `provider set`。
	ErrProviderExists = errors.New("厂商已存在")

	// ErrProviderNoRoutes 表示厂商行在、但一条路由都没声明，因此算不出默认协议。
	// EffectiveProviderProtocol 返回它，具体文案交给调用方决定。
	ErrProviderNoRoutes = errors.New("厂商没有声明任何路由")

	// ErrBaseURLRequired 表示 base_url 为空，而这次要落库的路由或模型列表地址里有相对路径。
	// 空的 base_url 只有一种形态是合法的：每一条路由的 path 与要落库的 models_path 都是整条地址，
	// 那时 base_url 拼不进任何地址，写一个只为过校验的地址只是无效配置：
	// 转发会拿着一个不存在的域名去请求，错误要到第一次调用才暴露。
	// cli 用 errors.Is 判它，把错误包成用法错误（退出码 2），文案保留这里的原话。
	ErrBaseURLRequired = errors.New("缺少 base_url")
)

// defaultProtocolOrder 是默认协议的回退顺序：openai-chat 先，openai-responses 次之，anthropic 最后。
// 不用协议名字典序：字典序把 anthropic 排在 openai-chat 前面，deepseek 与 commandcode 都同时
// 声明了这两条路由，默认协议会从 chat 悄悄变成 anthropic，升级前后的账号默认协议对不上。
var defaultProtocolOrder = []string{
	translate.ProtocolChat, translate.ProtocolResponses, translate.ProtocolAnthropic,
}

// modelProtocolOrder 是目录协议集合（provider_models.protocols）的取值顺序：先按默认协议那三条，
// 再 gemini、最后 systemone。后两条不进 defaultProtocolOrder：一条路由都没显式
// 给默认协议的厂商不能因此算出 gemini，否则「只声明了 gemini 一条路由」就不再是配置缺口
// （而是被当成一个合法的默认协议，问题留到转发时才爆）。systemone 排在最后，是因为决策协议
// 不该在厂商同时声明 chat 与 systemone 时被选成默认：对话客户端打到决策端点上只会拿到 400。
var modelProtocolOrder = append(append([]string{}, defaultProtocolOrder...),
	translate.ProtocolEmbeddings, translate.ProtocolGemini, translate.ProtocolSystemOne)

// Provider 是一行厂商。行里只有默认地址、默认协议、模型列表覆盖与备注，
// 协议的路径细节都在 ProviderRoute 上。账号行只存厂商名，地址与默认协议都从这里读
// （迁移 0014 把地址与协议从账号表里删了，账号行只存厂商名），因此改这一行会立即影响所有引用它的账号。
// DefaultProtocol 为空字符串表示这家没有显式默认协议，按固定顺序取第一条路由；
// 有效默认协议的算法只有 ResolveDefaultProtocol 一处，读库入口是 providerDefaultProtocol。
// ModelsPath 是厂商级的模型列表地址覆盖，空字符串表示不覆盖、按路由的 models_path 取。
// 取值可以是相对 base_url 的路径，也可以是整条地址，两种写法同一套校验（见 NormalizeEndpoint）。
type Provider struct {
	Name            string
	BaseURL         string
	Note            string
	DefaultProtocol string
	ModelsPath      string
	// Origin 是这一行的来源：builtin、plugin 或 manual。只用于展示与排查，不参与转发。
	Origin string
	// AuthHeader 与 AuthScheme 是凭据头的声明（见 DESIGN 5.8）：AuthHeader 是头名（小写），
	// AuthScheme 取 raw、bearer 或 bearer-semicolon。两项都为空表示按线协议推导，
	// 与没有这一项的老版本行为一致。凭据本身不在这里：key 在账号行上，加密存放。
	AuthHeader string
	AuthScheme string
}

// 厂商来源。builtin 是首次建库时从内嵌目录写入的行，plugin 由插件安装认领，
// manual 是 provider add 或旧库搬迁补建的行。它是溯源，不是权限：用户仍可改或删任何来源的厂商。
const (
	ProviderOriginBuiltin = "builtin"
	ProviderOriginPlugin  = "plugin"
	ProviderOriginManual  = "manual"
)

// ProviderRoute 是一行 (厂商, 协议) 的路由。Path 有两种写法（见 NormalizeEndpoint）：相对路径
// 是厂商 base_url 去掉末尾斜杠后直接拼它，整条地址（含 `://`）则直接使用、不再拼 base_url，
// 因此同一厂商的不同协议可以落在不同域名上；两种写法都不做任何后缀猜测。
// Path 与 ModelsPath 在写入时都经 NormalizeEndpoint 规整（相对路径补一个前导斜杠、合并重复斜杠、
// 去尾斜杠；整条地址再校验 scheme 与主机名），读取侧拼地址时用的是同一套规则。
// ModelsPath 是这条路由的模型列表地址，空字符串表示这条协议没有自己的列表接口，
// 读取时回退到同厂商协议名排序的第一条非空值。写入侧见 prepareRoutes 与 upsertRoutes。
type ProviderRoute struct {
	Provider   string
	Protocol   string
	Path       string
	ModelsPath string

	// modelsAuto 为真表示这次的 ModelsPath 是 prepareRoutes 按路径推出来的，不是调用方给的。
	// 推出来的值只跟路径走：库里原本是空或同样是推导值时换成新的推导值，手工填的值不动。
	modelsAuto bool
}

// ProviderPatch 是 `provider set` 的一次改动：字段为 nil 表示这一项不动，与「传了空串」区分开。
// Routes 按协议列出路由改动：非空值覆盖那条路由的路径，空串删掉这条路由。
type ProviderPatch struct {
	BaseURL         *string
	Note            *string
	ModelsPath      *string
	DefaultProtocol *string
	// AuthHeader 与 AuthScheme 一起构成凭据头声明：能写 AuthHeader 就能写 AuthScheme，
	// 但只有头、没有写法时按 raw 落库（见 plugin.NormalizeAuthScheme）。
	// 清掉头的那一刻写法一并清空：只有写法没有头的行写不出任何东西。
	AuthHeader *string
	AuthScheme *string
	Routes     map[string]string
}

const providerColumns = `name, base_url, note, default_protocol, models_path, auth_header, auth_scheme, origin`

// providersFromRows 把行形状转成对外的 Provider。
func providersFromRows(rows []providerRow) []Provider {
	out := make([]Provider, 0, len(rows))
	for _, r := range rows {
		out = append(out, Provider{
			Name:            r.Name,
			BaseURL:         r.BaseURL,
			Note:            r.Note,
			DefaultProtocol: r.DefaultProtocol,
			ModelsPath:      r.ModelsPath,
			Origin:          r.Origin,
			AuthHeader:      r.AuthHeader,
			AuthScheme:      r.AuthScheme,
		})
	}
	return out
}

// normalizeAuth 规整一次写入的凭据头声明。空头是合法的（表示按线协议推导），
// 但头为空时写法必须也为空，头非空而写法为空时归一到 raw。规则本身在 plugin 包里，
// manifest 的 auth 声明与 provider 子命令走的是同一份。
func normalizeAuth(header, scheme string) (string, string, error) {
	h := ""
	if strings.TrimSpace(header) != "" {
		var err error
		h, err = plugin.NormalizeAuthHeader(header)
		if err != nil {
			return "", "", err
		}
	}
	s, err := plugin.NormalizeAuthScheme(scheme, h)
	if err != nil {
		return "", "", err
	}
	return h, s, nil
}

// providerRoutesFromRows 把行形状转成对外的 ProviderRoute。
func providerRoutesFromRows(rows []providerRouteRow) []ProviderRoute {
	out := make([]ProviderRoute, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProviderRoute{
			Provider:   r.Provider,
			Protocol:   r.Protocol,
			Path:       r.Path,
			ModelsPath: r.ModelsPath,
		})
	}
	return out
}

// ListProviders 按名称升序返回全部厂商。表为空时返回空切片，不算错误。
func (s *Store) ListProviders() ([]Provider, error) {
	var rows []providerRow
	if err := s.gorm.Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	return providersFromRows(rows), nil
}

// GetProvider 按名称取厂商。
func (s *Store) GetProvider(name string) (Provider, error) {
	var row providerRow
	if err := s.gorm.Where("name = ?", name).Limit(1).Find(&row).Error; err != nil {
		return Provider{}, err
	}
	if row.Name == "" {
		return Provider{}, fmt.Errorf("%w：%q，用 lax provider ls 看已配置的厂商", ErrProviderNotFound, name)
	}
	return providersFromRows([]providerRow{row})[0], nil
}

// ProviderRoutes 返回一个厂商的全部路由，按协议名升序（顺序固定，回退取第一条时结果稳定）。
// 厂商不存在时返回空切片：这是「没配过路由」，不是错误。
func (s *Store) ProviderRoutes(provider string) ([]ProviderRoute, error) {
	var rows []providerRouteRow
	if err := s.gorm.Where("provider = ?", provider).Order("protocol").Find(&rows).Error; err != nil {
		return nil, err
	}
	return providerRoutesFromRows(rows), nil
}

// LookupProviderRoute 按 (厂商, 协议) 取一条路由，是读路由的唯一入口：转发、模型同步与探活都走它。
// 厂商不存在或这家没有该协议的路由时 ok=false，且不返回错误，调用方据此退回后缀猜测。
func (s *Store) LookupProviderRoute(provider, protocol string) (ProviderRoute, bool, error) {
	if provider == "" || protocol == "" {
		return ProviderRoute{}, false, nil
	}
	var row providerRouteRow
	err := s.gorm.Where("provider = ? AND protocol = ?", provider, protocol).
		Limit(1).Find(&row).Error
	if err != nil {
		return ProviderRoute{}, false, err
	}
	if row.Provider == "" {
		return ProviderRoute{}, false, nil
	}
	return providerRoutesFromRows([]providerRouteRow{row})[0], true, nil
}

// ProviderProtocols 返回该厂商声明过路由的协议，按协议名升序。
// 没有厂商行时返回 nil，调用方据此退回「入站协议全试」。
func (s *Store) ProviderProtocols(provider string) ([]string, error) {
	routes, err := s.ProviderRoutes(provider)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routes {
		if r.Protocol != "" {
			out = append(out, r.Protocol)
		}
	}
	return out, nil
}

// IsWireProtocol 判断一个取值是不是线协议。八个值都认：三条入站对话协议加 gemini，再加上
// systemone、openai-embeddings、openai-speech 与 openai-transcriptions。gemini 入站有两条路径、两个方向的流式都支持，
// systemone 的入站与出站是同一个 POST /v1/systemone，向量化与语音的入站与出站各是自己的协议路径；
// 它们同样是线上的协议取值，账号的 --protocol、厂商路由与默认协议、探活候选都用这一个判定：
// cli 不另写一份名单，两处名单迟早会对不上。
func IsWireProtocol(protocol string) bool {
	switch protocol {
	case translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses,
		translate.ProtocolGemini, translate.ProtocolSystemOne, translate.ProtocolEmbeddings,
		translate.ProtocolSpeech, translate.ProtocolTranscriptions:
		return true
	}
	return false
}

// WireProtocols 是全部线协议，按文档里列的顺序（入站三条在前，gemini、systemone、向量化与两条语音在后）。
// 错误文案只从这一份清单拼，不另写一串字面量。
func WireProtocols() []string {
	return []string{
		translate.ProtocolAnthropic,
		translate.ProtocolChat,
		translate.ProtocolResponses,
		translate.ProtocolGemini,
		translate.ProtocolSystemOne,
		translate.ProtocolEmbeddings,
		translate.ProtocolSpeech,
		translate.ProtocolTranscriptions,
	}
}

// wireProtocolNames 把线协议清单拼成一句「A、B、C、D」，供错误文案用。
func wireProtocolNames() string { return strings.Join(WireProtocols(), "、") }

// NormalizePath 是相对路径规整的唯一入口：写入侧（CreateProvider、SetProvider、AddProvider 的
// path 与 models_path）与读取侧（拼转发地址、拼模型列表地址、`model sync --endpoint`）都走它
// （整条地址的取值先由 NormalizeEndpoint 分流，见那里）：
// 去掉首尾空白、把连续斜杠合并成一个、补上恰好一个前导斜杠、去掉末尾斜杠。
// 空串、只有斜杠、只有空白的输入一律返回空串，由调用方按「这一项没给」处理。
//
// 它只规整、不拒绝：`v1/chat/completions`、`//v1//chat/completions`、`/v1/chat/completions/`
// 都当同一个地址。路径来自用户抄上游文档，多一个或少一个斜杠是写法差异，不是用法错误；
// 报错只会让人先改命令再重敲一遍，得到的仍然是同一个地址。真正的用法错误是取值为空
// （删除语义归 `provider unset`），那一条由调用方判断。
//
// 路径里的 `{model}` 是出站模型名占位符：它不是斜杠，规整不动它，
// 花括号与冒号原样保留，转发时由 gateway 替换。
func NormalizePath(raw string) string {
	path := collapseSlashes(strings.TrimSpace(raw))
	path = strings.TrimRight(path, "/")
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

// collapseSlashes 把连续斜杠合并成一个。调用方只把它用在路径部分：
// 查询串与 `#` 之后的片段里斜杠连写是值的一部分，合并会改掉语义。
func collapseSlashes(s string) string {
	if !strings.Contains(s, "//") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSlash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '/' {
			if prevSlash {
				continue
			}
			prevSlash = true
		} else {
			prevSlash = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

// IsFullAddress 判断一个端点取值是不是整条地址：含 `://` 的就是。
// 不含 `://` 的取值是相对 base_url 的路径。路由的 path、路由与厂商的 models_path、
// `model sync --endpoint` 三处用同一个判定（见 NormalizeEndpoint），cli 判断路径 flag 的取值
// 是不是整条地址时也用这一个函数，判定只有一份。
func IsFullAddress(value string) bool {
	return strings.Contains(value, "://")
}

// NormalizeEndpoint 是端点取值（路由的 path、路由与厂商的 models_path、`model sync --endpoint`）
// 的唯一解释入口：含 `://` 的按整条地址校验并规整，其余按相对 base_url 的路径走 NormalizePath。
// 空值（含只有空白、只有斜杠、`///`）返回空串，由调用方按「这一项没给」处理，不报错。
//
// 整条地址让同一厂商的不同协议可以落在不同域名上：openai 兼容端点在 openai.example.com、
// anthropic 端点在 claude.example.com 时，base_url 只是其中一家的地址，另一条协议直接写整条地址。
// 校验与 NormalizeBaseURL 同一套：scheme 只认 http 与 https（大小写不敏感，落库时归一成小写），
// 缺主机名是错误（`https://` 里没有地址）。规整只作用在路径部分：合并重复斜杠、去掉末尾斜杠；
// 授权部分、查询串与 `#` 之后的内容一个字不动。
func NormalizeEndpoint(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !IsFullAddress(trimmed) {
		return NormalizePath(trimmed), nil
	}
	return normalizeAddress(trimmed, "整条地址")
}

// NormalizeBaseURL 校验并规整厂商地址，规则与 NormalizeEndpoint 里的整条地址一致（见那里）。
// 三个写入口（CreateProvider、SetProvider、AddProvider）都在这里过一道，cli 那层不再是唯一防线：
// 脏地址一旦落库，转发与模型同步都会拿它拼路径。
// 读侧拼地址（gateway.joinEndpoint 等）也用它，保证最终地址的路径里不出现 `//`。
func NormalizeBaseURL(raw string) (string, error) {
	return normalizeAddress(raw, "base_url")
}

// optionalBaseURL 是写入口用的 base_url 解释：空值合法（返回空串，能不能接受由
// requireBaseURLCoverage 判），非空值仍走 NormalizeBaseURL 的校验与规整。
// 空值本身不在这里报错：缺地址只在一个前提下算错——有相对路径要拼它。
func optionalBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return NormalizeBaseURL(raw)
}

// requireBaseURLCoverage 校验 base_url 为空时的唯一合法形态：每一条路由的 path 与要落库的
// models_path 都是整条地址（含 `://`），一条相对路径都没有。有一条相对路径时缺 base_url 就拼不出
// 完整地址，整笔拒绝，文案说清是哪条路由或哪项设置，以及两条出路：给 --base-url，或把这一项
// 写成整条地址。base_url 非空时这个函数什么都不做：相对路径照旧拼它。
// 一条路由都没有时同样拒绝：既没有地址又没有路由的厂商转发不了任何请求。
func requireBaseURLCoverage(provider, base string, routes []ProviderRoute, modelsPath string) error {
	if base != "" {
		return nil
	}
	if len(routes) == 0 {
		return fmt.Errorf("%w：厂商 %s 既没有地址也没有任何路由；加一条整条地址的路由（例如 --openai https://api.example.com/v1/chat/completions），或给 --base-url <地址>",
			ErrBaseURLRequired, provider)
	}
	for _, r := range routes {
		if IsFullAddress(r.Path) {
			continue
		}
		return fmt.Errorf("%w：厂商 %s 的 %s 路由是相对路径（%s），它需要 --base-url 才能拼成完整地址；给 --base-url <地址>，或把这条路由写成整条地址（例如 https://api.example.com%s）",
			ErrBaseURLRequired, provider, r.Protocol, r.Path, r.Path)
	}
	if modelsPath != "" && !IsFullAddress(modelsPath) {
		return fmt.Errorf("%w：厂商 %s 的模型列表地址是相对路径（%s），它需要 --base-url 才能拼成完整地址；给 --base-url <地址>，或把它写成整条地址（例如 https://api.example.com%s），也可以用 lax provider unset %s models-path 回到按路由取",
			ErrBaseURLRequired, provider, modelsPath, modelsPath, provider)
	}
	return nil
}

// normalizeAddress 校验并规整一条带 scheme 的整条地址。
//
// 校验用 url.Parse 做：scheme 只认 http 与 https（大小写不敏感，落库时归一成小写），
// 缺主机名时报错——`https://` 里没有地址，用它拼出来的请求地址是畸形的，
// 不能当成可用地址落库。缺 scheme 与别的 scheme 同样是错误。
//
// 规整只作用在路径部分：合并重复斜杠、去掉末尾斜杠，因此
// `https://api.example.com//api/paas/v4/` 变成 `https://api.example.com/api/paas/v4`——
// 真实存在的前缀原样保留。授权部分（含端口与用户信息）、查询串与 `#` 之后的内容一个字不动：
// 那里合并斜杠会改掉语义（`?next=//a//b` 是值，不是路径）。`..` 与 `.` 段也原样保留，
// 静默改写等于把地址换成另一个地址。field 只用在错误文案里，说明是哪个字段不对。
func normalizeAddress(raw, field string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("缺少 %s", field)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%s %q 不是合法地址: %v", field, trimmed, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%s %q 要以 http:// 或 https:// 开头，例如 https://api.example.com", field, trimmed)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s %q 里没有主机名：需要完整地址，例如 https://api.deepseek.com", field, trimmed)
	}

	// 重建时只动路径：授权部分、查询串与 `#` 之后的内容照抄原文，
	// 避免规整顺手改掉它们的形状（百分号转义也因此原样保留）。
	rest := trimmed[strings.Index(trimmed, "://")+len("://"):]
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	suffix := ""
	if i := strings.IndexAny(tail, "?#"); i >= 0 {
		tail, suffix = tail[:i], tail[i:]
	}
	path := strings.TrimRight(collapseSlashes(tail), "/")
	return scheme + "://" + authority + path + suffix, nil
}

// DeriveModelsPath 是 models_path 的写入默认值：openai-chat 的路由把尾部 /chat/completions
// 换成 /models，其它协议（含 gemini 与 systemone）返回空串。两种写法都推：相对路径在同一条路径上换尾段
// （/v1/chat/completions → /v1/models、/chat/completions → /models）；整条地址在同一 scheme + host
// 上换尾段（https://api.x/v1/chat/completions → https://api.x/v1/models），查询串与 `#` 之后的内容
// 跟着原样带过去，不换域名。
// 空串表示「这条路由没有自己的列表接口」，读取时回退到同厂商其它协议的非空 models_path。
// systemone 是决策端点（POST /v1/systemone），它没有模型列表，与 gemini 一样留空，不推 /models：
// 推出来的地址上游根本不提供，模型同步只会拿它去 404。
// 推导只发生在写入侧，读取侧不拿这一条重算：内置 anthropic 厂商只有 anthropic 一条路由，
// 没有 openai 路由可推导，读时推导会让 anthropic 型账号彻底失去模型列表来源。
func DeriveModelsPath(protocol, path string) string {
	if protocol != translate.ProtocolChat {
		return ""
	}
	target, suffix := path, ""
	// 整条地址的查询串与 `#` 之后的内容不属于路径，换尾段时不能碰它们；
	// 相对路径里 `?` 与 `#` 没有特殊含义，整个取值都是路径。
	if IsFullAddress(path) {
		if i := strings.IndexAny(path, "?#"); i >= 0 {
			target, suffix = path[:i], path[i:]
		}
	}
	if tail, ok := strings.CutSuffix(target, "/chat/completions"); ok {
		return tail + "/models" + suffix
	}
	return ""
}

// prepareRoutes 解释一次写入要落的路由（路径与模型列表路径都走 NormalizeEndpoint：整条地址
// 直接使用、相对路径规整后拼 base_url），并补上 models_path 的默认值。
// 斜杠写法不拒绝，只有规整之后为空才算缺值；整条地址校验不过时整笔拒绝。
func prepareRoutes(provider string, in []ProviderRoute) ([]ProviderRoute, error) {
	out := make([]ProviderRoute, 0, len(in))
	for _, r := range in {
		protocol := strings.TrimSpace(r.Protocol)
		if !IsWireProtocol(protocol) {
			return nil, fmt.Errorf("厂商 %s 的路由协议 %q 不认识，只能是 %s",
				provider, r.Protocol, wireProtocolNames())
		}
		path, err := NormalizeEndpoint(r.Path)
		if err != nil {
			return nil, fmt.Errorf("厂商 %s 的 %s 路由：%w", provider, protocol, err)
		}
		if path == "" {
			return nil, fmt.Errorf("厂商 %s 的 %s 路由缺路径：给相对 base_url 的完整路径（例如 /v1/chat/completions），或整条地址（例如 https://api.example.com/v1/chat/completions）",
				provider, protocol)
		}
		modelsPath, err := NormalizeEndpoint(r.ModelsPath)
		if err != nil {
			return nil, fmt.Errorf("厂商 %s 的 %s 路由的模型列表地址：%w", provider, protocol, err)
		}
		auto := modelsPath == ""
		if auto {
			modelsPath = DeriveModelsPath(protocol, path)
		}
		out = append(out, ProviderRoute{
			Provider: provider, Protocol: protocol, Path: path,
			ModelsPath: modelsPath, modelsAuto: auto,
		})
	}
	return out, nil
}

// upsertRoutes 按 (厂商, 协议) 覆盖或新增路由。
// models_path 是本次推出来的推导值时，只有库里那行原本也是空的或同样是推导值才替换：
// 改一条路由的路径会顺带把推导出来的列表路径换成新路径推出来的那个，而手工填的值不动
// （内置 anthropic 厂商那条 /v1/models 因此不会因为重写 anthropic 路由而消失）。
// 要改厂商级的列表地址就用 ProviderPatch.ModelsPath 覆盖。
func upsertRoutes(tx *gorm.DB, routes []ProviderRoute) error {
	for _, r := range routes {
		modelsPath := r.ModelsPath
		if r.modelsAuto {
			var old providerRouteRow
			if err := tx.Where("provider = ? AND protocol = ?", r.Provider, r.Protocol).
				Limit(1).Find(&old).Error; err != nil {
				return err
			}
			switch {
			case old.Provider == "":
				// 新行：就用这次推出来的值。
			case old.ModelsPath != "" && old.ModelsPath != DeriveModelsPath(r.Protocol, old.Path):
				// 库里是手工填的值：留着，不跟着路径改。
				modelsPath = old.ModelsPath
			}
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "provider"}, {Name: "protocol"}},
			DoUpdates: clause.AssignmentColumns([]string{"path", "models_path"}),
		}).Create(&providerRouteRow{
			Provider:   r.Provider,
			Protocol:   r.Protocol,
			Path:       r.Path,
			ModelsPath: modelsPath,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

// hasRoute 判断协议是否落在这些路由里。
func hasRoute(routes []ProviderRoute, protocol string) bool {
	for _, r := range routes {
		if r.Protocol == protocol {
			return true
		}
	}
	return false
}

// routeProtocols 列出这些路由的协议，按协议名升序，用于错误文案。
func routeProtocols(routes []ProviderRoute) string {
	protocols := make([]string, 0, len(routes))
	for _, r := range routes {
		protocols = append(protocols, r.Protocol)
	}
	sort.Strings(protocols)
	return strings.Join(protocols, "、")
}

// CreateProvider 新建一家厂商，是 `provider add` 的唯一写入口。
// 名称已存在时报 ErrProviderExists（改已有的厂商用 SetProvider），至少要声明一条协议路由，
// 默认协议非空时必须落在这次声明的路由上，否则整笔拒绝，不留下半条记录。
// 写库时 models_path 为空的路由按 DeriveModelsPath 补默认值。
// base_url 可以不给，条件是这次声明的每一条路由与要落库的 models_path 都是整条地址；
// 有一条相对路径时缺 base_url 报 ErrBaseURLRequired（见 requireBaseURLCoverage）。
func (s *Store) CreateProvider(p Provider, routes []ProviderRoute) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return errors.New("厂商名不能为空")
	}
	base, err := optionalBaseURL(p.BaseURL)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", name, err)
	}
	prepared, err := prepareRoutes(name, routes)
	if err != nil {
		return err
	}
	if len(prepared) == 0 {
		return fmt.Errorf("厂商 %s 至少要声明一条协议路由（%s 里任一条），否则账号只能按 base_url 后缀猜路径",
			name, wireProtocolNames())
	}
	modelsPath, err := NormalizeEndpoint(p.ModelsPath)
	if err != nil {
		return fmt.Errorf("厂商 %s 的 models_path：%w", name, err)
	}
	authHeader, authScheme, err := normalizeAuth(p.AuthHeader, p.AuthScheme)
	if err != nil {
		return fmt.Errorf("厂商 %s 的凭据头：%w", name, err)
	}
	// 缺 base_url 只在一个前提下算错：有相对路径要拼它。拒绝时一个字段都不落库。
	if err := requireBaseURLCoverage(name, base, prepared, modelsPath); err != nil {
		return err
	}
	def := strings.TrimSpace(p.DefaultProtocol)
	if def != "" && !hasRoute(prepared, def) {
		return fmt.Errorf("厂商 %s 的默认协议是 %q，但这次没有声明这个协议的路由（这次声明：%s）；把默认协议改成已声明的其中一个，或补上这条路由",
			name, def, routeProtocols(prepared))
	}

	return s.gorm.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&providerRow{}).Where("name = ?", name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w：%q，改这家厂商用 lax provider set %s", ErrProviderExists, name, name)
		}

		if err := tx.Create(&providerRow{
			Name:            name,
			BaseURL:         base,
			Note:            strings.TrimSpace(p.Note),
			DefaultProtocol: def,
			ModelsPath:      modelsPath,
			AuthHeader:      authHeader,
			AuthScheme:      authScheme,
			Origin:          ProviderOriginManual,
		}).Error; err != nil {
			return err
		}
		return upsertRoutes(tx, prepared)
	})
}

// SetProvider 增量修改一家厂商，是 `provider set` 的唯一写入口：patch 里为 nil 的字段一律不动。
// 协议路由按 patch.Routes 覆盖或删掉（空串即删），删到一条不剩时报错，整体删厂商走 DeleteProvider。
// 这次动了路由或默认协议时，改完之后默认协议必须指向一条仍然存在的路由，否则整笔拒绝：
// 留着一条指不到路由的默认协议，账号加进来之后默认协议要么报错要么落到后缀猜测上，
// 都不如当场说清楚。只改备注或地址的调用不受历史悬空值影响，那类脏数据由读取侧按固定顺序回退。
// 地址为空时改完的路由与 models_path 必须全是整条地址，否则报 ErrBaseURLRequired 并整笔拒绝。
func (s *Store) SetProvider(name string, patch ProviderPatch) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("厂商名不能为空")
	}

	return s.gorm.Transaction(func(tx *gorm.DB) error {
		var row providerRow
		if err := tx.Where("name = ?", name).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		if row.Name == "" {
			return fmt.Errorf("%w：%q，新建厂商用 lax provider add，用 lax provider ls 看已配置的厂商", ErrProviderNotFound, name)
		}
		p := providersFromRows([]providerRow{row})[0]

		if patch.BaseURL != nil {
			base, err := NormalizeBaseURL(*patch.BaseURL)
			if err != nil {
				return fmt.Errorf("厂商 %s：%w", name, err)
			}
			p.BaseURL = base
		}
		if patch.Note != nil {
			p.Note = strings.TrimSpace(*patch.Note)
		}
		if patch.ModelsPath != nil {
			// 空值表示回到按路由取（`provider unset <名称> models-path` 就是这么调的）。
			// 取值可以是相对路径，也可以是整条地址（见 NormalizeEndpoint）。
			modelsPath, err := NormalizeEndpoint(*patch.ModelsPath)
			if err != nil {
				return fmt.Errorf("厂商 %s 的 models_path：%w", name, err)
			}
			p.ModelsPath = modelsPath
		}
		if patch.DefaultProtocol != nil {
			def := strings.TrimSpace(*patch.DefaultProtocol)
			if def != "" && !IsWireProtocol(def) {
				return fmt.Errorf("厂商 %s 的默认协议 %q 不认识，只能是 %s；清掉显式值、回到按固定顺序取第一条用 lax provider unset %s default-protocol",
					name, def, wireProtocolNames(), name)
			}
			p.DefaultProtocol = def
		}
		if patch.AuthHeader != nil {
			p.AuthHeader = strings.TrimSpace(*patch.AuthHeader)
			if p.AuthHeader == "" {
				// 头清掉的那一刻写法一并清掉：只有写法没有头的行写不出任何凭据，
				// 留着它只会在下一次改头时静默复活一个旧写法。
				p.AuthScheme = ""
			}
		}
		if patch.AuthScheme != nil {
			p.AuthScheme = strings.TrimSpace(*patch.AuthScheme)
		}
		authHeader, authScheme, err := normalizeAuth(p.AuthHeader, p.AuthScheme)
		if err != nil {
			return fmt.Errorf("厂商 %s 的凭据头：%w", name, err)
		}
		p.AuthHeader, p.AuthScheme = authHeader, authScheme

		// 改之前的形状：用来区分「这次把路由删光了」与「本来就没有路由」。
		before, err := providerRoutesTx(tx, name)
		if err != nil {
			return err
		}

		// 路由改动：空串删掉这条，其余覆盖或新增。协议名排序后处理，报错顺序因此固定。
		protocols := make([]string, 0, len(patch.Routes))
		for protocol := range patch.Routes {
			protocols = append(protocols, protocol)
		}
		sort.Strings(protocols)
		for _, protocol := range protocols {
			path := strings.TrimSpace(patch.Routes[protocol])
			if path == "" {
				if err := tx.Where("provider = ? AND protocol = ?", name, protocol).
					Delete(&providerRouteRow{}).Error; err != nil {
					return err
				}
				continue
			}
			prepared, err := prepareRoutes(name, []ProviderRoute{{Protocol: protocol, Path: path}})
			if err != nil {
				return err
			}
			if err := upsertRoutes(tx, prepared); err != nil {
				return err
			}
		}

		routes, err := providerRoutesTx(tx, name)
		if err != nil {
			return err
		}
		// 删到一条不剩要拒绝；本来就没有路由的厂商（过渡期 AddProvider 能造出来）不因此挡住改备注这类操作。
		if len(routes) == 0 && len(before) > 0 {
			return fmt.Errorf("厂商 %s 至少要保留一条协议路由；不想再要这家厂商就用 lax provider rm %s", name, name)
		}
		// 改完之后地址与路由必须自洽：库里地址为空时，改完的路由与 models_path 都得是整条地址。
		// 把一条路由从整条地址改成相对路径、又没给 base_url、库里也没有时，整笔拒绝：
		// 这种行落库之后拼出来的是一个没有 scheme 的相对地址，请求直接失败。
		if err := requireBaseURLCoverage(name, p.BaseURL, routes, p.ModelsPath); err != nil {
			return err
		}
		// 只有这次动了路由或默认协议时才校验指向：历史库里遗留的悬空默认协议由读取侧按固定顺序回退，
		// 不该挡住一次只改备注或地址的调用。
		touchedRouting := len(patch.Routes) > 0 || patch.DefaultProtocol != nil
		if touchedRouting && p.DefaultProtocol != "" && !hasRoute(routes, p.DefaultProtocol) {
			return fmt.Errorf("厂商 %s 的默认协议 %q 对应的路由不存在（现有：%s）；先补上这条路由（lax provider set %s --%s <路径>），或把默认协议改成已声明的其中一个（lax provider set %s --default-protocol <协议>），也可以清掉显式值回到按固定顺序取第一条（lax provider unset %s default-protocol）",
				name, p.DefaultProtocol, routeProtocols(routes), name, protocolFlagName(p.DefaultProtocol), name, name)
		}

		return tx.Model(&providerRow{}).Where("name = ?", name).Updates(map[string]any{
			"base_url":         p.BaseURL,
			"note":             p.Note,
			"default_protocol": p.DefaultProtocol,
			"models_path":      p.ModelsPath,
			"auth_header":      p.AuthHeader,
			"auth_scheme":      p.AuthScheme,
		}).Error
	})
}

// protocolFlagName 把协议名换成 `provider set` 上对应的那个 flag，错误文案直接给出下一步能敲的命令。
// systemone 与语音两条的 flag 不是线协议全名，这里逐个映射回命令行写法。
func protocolFlagName(protocol string) string {
	switch protocol {
	case translate.ProtocolChat:
		return "openai"
	case translate.ProtocolResponses:
		return "responses"
	case translate.ProtocolAnthropic:
		return "anthropic"
	case translate.ProtocolGemini:
		return "gemini"
	case translate.ProtocolSystemOne:
		return "systemone"
	case translate.ProtocolEmbeddings:
		return "embeddings"
	case translate.ProtocolSpeech:
		return "speech"
	case translate.ProtocolTranscriptions:
		return "transcriptions"
	}
	return protocol
}

// providerRoutesTx 是事务里读路由的版本，供 SetProvider 校验改完之后的形状。
func providerRoutesTx(tx *gorm.DB, provider string) ([]ProviderRoute, error) {
	var rows []providerRouteRow
	if err := tx.Where("provider = ?", provider).Order("protocol").Find(&rows).Error; err != nil {
		return nil, err
	}
	return providerRoutesFromRows(rows), nil
}

// AddProvider 是第二条 upsert 写入口：厂商已存在时更新 base_url 与备注，路由按 (厂商, 协议)
// 覆盖或新增，没传入的路由不动。它比 CreateProvider 宽松——允许一条路由都没有。
// 定位：`cli` 不走它（新建走 CreateProvider、改走 SetProvider），生产代码里也没有其它调用者，
// 现在只有 `store` 自己的用例在用它。留着是为了不把「upsert 且允许零路由」这个形状从一个
// 已经测过的入口里抹掉；真要收窄就整条删掉并同步删掉对应用例，不要把它当新建入口用。
// p.DefaultProtocol 非空时必须落在「这次传入的路由」或「此前已声明的路由」上，否则整笔拒绝，
// 不留下半条记录；传空字符串表示不改这一列（已有行保留原值）。
// base_url 与 CreateProvider 同一规则：可以为空，但改完之后每条路由与 models_path 都得是整条地址。
func (s *Store) AddProvider(p Provider, routes []ProviderRoute) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return errors.New("厂商名不能为空")
	}
	base, err := optionalBaseURL(p.BaseURL)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", name, err)
	}
	prepared, err := prepareRoutes(name, routes)
	if err != nil {
		return err
	}
	modelsPath, err := NormalizeEndpoint(p.ModelsPath)
	if err != nil {
		return fmt.Errorf("厂商 %s 的 models_path：%w", name, err)
	}
	authHeader, authScheme, err := normalizeAuth(p.AuthHeader, p.AuthScheme)
	if err != nil {
		return fmt.Errorf("厂商 %s 的凭据头：%w", name, err)
	}

	return s.gorm.Transaction(func(tx *gorm.DB) error {
		// models_path 传空串时不覆盖已存的值：这条兼容入口的调用方还不知道有这个字段。
		// 分两步写，不用 `CASE WHEN excluded…`：那是 SQLite/PostgreSQL 的写法，MySQL 不认。
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "name"}},
			DoUpdates: clause.AssignmentColumns([]string{"base_url", "note"}),
		}).Create(&providerRow{
			Name:       name,
			BaseURL:    base,
			Note:       p.Note,
			ModelsPath: modelsPath,
			AuthHeader: authHeader,
			AuthScheme: authScheme,
			Origin:     ProviderOriginManual,
		}).Error; err != nil {
			return err
		}
		if modelsPath != "" {
			if err := tx.Model(&providerRow{}).Where("name = ?", name).
				Update("models_path", modelsPath).Error; err != nil {
				return err
			}
		}

		if err := upsertRoutes(tx, prepared); err != nil {
			return err
		}

		// 校验放在路由写完之后：「这次传入的路由」也算数，所以先落路由再查有没有这条。
		def := strings.TrimSpace(p.DefaultProtocol)
		if def != "" {
			if err := requireDeclaredProtocol(tx, name, def); err != nil {
				return err
			}
			if err := tx.Model(&providerRow{}).Where("name = ?", name).
				Update("default_protocol", def).Error; err != nil {
				return err
			}
		}

		// 地址覆盖同样放在路由写完之后：已有的路由与这次传入的路由都要算进「是不是全是整条地址」。
		finalRoutes, err := providerRoutesTx(tx, name)
		if err != nil {
			return err
		}
		var finalModels string
		if err := tx.Model(&providerRow{}).Where("name = ?", name).
			Limit(1).Pluck("models_path", &finalModels).Error; err != nil {
			return err
		}
		return requireBaseURLCoverage(name, base, finalRoutes, finalModels)
	})
}

// requireDeclaredProtocol 确认默认协议能指向这家厂商的一条已声明路由。
// 指不到时返回可行动的错误：要么先把这条路由加上，要么把默认协议改成已声明的其中一个。
func requireDeclaredProtocol(tx *gorm.DB, provider, protocol string) error {
	var count int64
	if err := tx.Model(&providerRouteRow{}).
		Where("provider = ? AND protocol = ?", provider, protocol).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	declared, err := declaredProtocols(tx, provider)
	if err != nil {
		return err
	}
	if len(declared) == 0 {
		return fmt.Errorf("厂商 %s 的默认协议是 %q，但这家还没有任何路由；先运行 lax provider set %s --%s <路径> 把这条路由加上，或去掉默认协议改回按固定顺序取第一条",
			provider, protocol, provider, protocolFlagName(protocol))
	}
	return fmt.Errorf("厂商 %s 的默认协议是 %q，但这家没有这个协议的路由（已声明：%s）；先运行 lax provider set %s --%s <路径> 补上这条路由，或把默认协议改成已声明的其中一个：%s",
		provider, protocol, strings.Join(declared, "、"), provider, protocolFlagName(protocol), strings.Join(declared, "、"))
}

// declaredProtocols 是事务里读这家厂商已声明协议的版本，按协议名升序，与 ProviderRoutes 一致。
func declaredProtocols(tx *gorm.DB, provider string) ([]string, error) {
	var out []string
	err := tx.Model(&providerRouteRow{}).Where("provider = ?", provider).
		Order("protocol").Pluck("protocol", &out).Error
	return out, err
}

// ResolveDefaultProtocol 是有效默认协议的纯规则：
//  1. 显式值落在这家已声明的路由上就用它；
//  2. 否则按固定顺序 openai-chat → openai-responses → anthropic 取第一条已声明的路由；
//  3. 一条路由都没有时返回空串。
//
// 不走协议名字典序兜底：字典序把 anthropic 排在 openai-chat 前面，同一家上游声明了两条路由时
// 默认协议会从 chat 变成 anthropic。显式值指不到路由时同样按固定顺序回退，历史库里留下的
// 悬空默认协议（例如旧版本删路由时没清默认协议）因此不会静默落到后缀猜测上。
func ResolveDefaultProtocol(explicit string, routes []ProviderRoute) string {
	if explicit != "" && hasRoute(routes, explicit) {
		return explicit
	}
	for _, protocol := range defaultProtocolOrder {
		if hasRoute(routes, protocol) {
			return protocol
		}
	}
	return ""
}

// ResolveModelsPath 是模型列表来源的纯规则，返回列表路径与这条路径所属的协议：
//
//  1. 厂商级覆盖非空就用它，协议取账号协议（覆盖是厂商级设置，没有自己的协议）；
//  2. 账号协议那条路由的 models_path 非空就用它；
//  3. 否则取按协议名排序的第一条非空 models_path——deepseek 的 anthropic 账号因此仍能拿
//     openai-chat 那份列表；
//  4. 都没有时返回空串与空串，调用方按 base_url 兜底或报错。
//
// 协议必须与路径一起返回：凭据头按协议选（anthropic 用 x-api-key，gemini 用 x-goog-api-key，
// openai 的两条用 Bearer），
// 地址来自哪条路由就用哪条路由的协议，否则 deepseek 的 anthropic 账号会打 chat 的
// /v1/models 却发 x-api-key，第一个真实账号就会在 /models 上收到 401/403。
func ResolveModelsPath(override string, routes []ProviderRoute, accountProtocol string) (path, protocol string) {
	if override != "" {
		return override, accountProtocol
	}
	for _, r := range routes {
		if r.Protocol == accountProtocol && strings.TrimSpace(r.ModelsPath) != "" {
			return r.ModelsPath, r.Protocol
		}
	}
	for _, r := range routes {
		if strings.TrimSpace(r.ModelsPath) != "" {
			return r.ModelsPath, r.Protocol
		}
	}
	return "", ""
}

// EffectiveProviderProtocol 返回厂商的有效默认协议，是 `provider ls` 这类「要算不出来就报错」的
// 调用方的入口：厂商不存在返回 ErrProviderNotFound，厂商在但一条路由都没有时返回空串与
// ErrProviderNoRoutes，需要自己的提示文案的调用方用 errors.Is 判哨兵，自己写。
// 账号的派生 Protocol 与 EffectiveProtocol 的兜底不要用它：那两处算不出来就是空串，不是错误，
// 它们走 providerDefaultProtocol。
func (s *Store) EffectiveProviderProtocol(name string) (string, error) {
	p, err := s.GetProvider(name)
	if err != nil {
		return "", err
	}
	routes, err := s.ProviderRoutes(name)
	if err != nil {
		return "", err
	}
	proto := ResolveDefaultProtocol(p.DefaultProtocol, routes)
	if proto == "" {
		return "", fmt.Errorf("%w：%q 还没有任何路由，默认协议无从算起", ErrProviderNoRoutes, name)
	}
	return proto, nil
}

// providerDefaultProtocol 返回厂商的有效默认协议，是读单家厂商默认协议的轻入口：
// 账号的派生 Protocol 与 EffectiveProtocol 的兜底都走它，规则仍是 ResolveDefaultProtocol 一份。
// 厂商不存在（含 name 为空串）、或厂商行在但一条路由都没声明时返回空串，不报错：
// 调用方要的是「算出来是什么」，而算不出来就是空串，拿空串回退即可，不是故障。
func (s *Store) providerDefaultProtocol(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	var explicit string
	err := s.gorm.Model(&providerRow{}).Where("name = ?", name).
		Limit(1).Pluck("default_protocol", &explicit).Error
	if err != nil {
		return "", err
	}
	if explicit == "" {
		// 厂商不存在与显式值为空串在这里不必区分：两种都算不出来。
		var count int64
		if err := s.gorm.Model(&providerRow{}).Where("name = ?", name).
			Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return "", nil
		}
	}
	routes, err := s.ProviderRoutes(name)
	if err != nil {
		return "", err
	}
	return ResolveDefaultProtocol(explicit, routes), nil
}

// DeleteProvider 在一个事务里删掉厂商行与它的全部路由行。两张表之间没有外键，
// 分两条语句删就必须同一个事务，否则中途失败会留下没有厂商的孤儿路由。
// 指向它的账号不动：`provider rm` 不带 --force 时调用方已经先拒绝了，那是 DeleteProviderForce 的事。
func (s *Store) DeleteProvider(name string) error {
	return s.deleteProvider(name, false)
}

// DeleteProviderForce 是 `provider rm --force` 的入口：除了删厂商与路由，还在同一个事务里把指向
// 它的账号停用（enabled=0 并写明 last_error）。为什么要停用：账号行的地址与默认协议都从厂商行取，
// 厂商没了这两样就都没了，留它们在选号池里必然一个请求一个 502。
// 停用走的是 setEnabledTx，与 SetEnabled 同一条写路径；账号本身与它的 key、模型清单、探活结果都不删，
// 用 `provider add` 重建同名厂商或 `account set` 改厂商之后手动 `account enable` 就能接着用。
func (s *Store) DeleteProviderForce(name string) error {
	return s.deleteProvider(name, true)
}

func (s *Store) deleteProvider(name string, force bool) error {
	return s.gorm.Transaction(func(tx *gorm.DB) error {
		res := tx.Where("name = ?", name).Delete(&providerRow{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w：%q", ErrProviderNotFound, name)
		}

		if err := tx.Where("provider = ?", name).Delete(&providerRouteRow{}).Error; err != nil {
			return err
		}

		if force {
			ids, err := accountIDsByProviderTx(tx, name)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := setEnabledTx(tx, id, false, providerDeletedReason); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// providerDeletedReason 是 `provider rm --force` 停用账号时写进 last_error 的话：
// 说清发生了什么与下一步做什么。
const providerDeletedReason = "厂商已删除，请 provider add 重建或修改账号的厂商"

// accountIDsByProviderTx 是事务里列指向某家厂商的账号，按 id 升序，结果与行序无关。
func accountIDsByProviderTx(tx *gorm.DB, name string) ([]int64, error) {
	var out []int64
	err := tx.Model(&accountRow{}).Where("provider = ?", name).
		Order("id").Pluck("id", &out).Error
	return out, err
}

// ListAccountsByProvider 返回 provider 字段等于 name 的账号，按 priority、id 升序。
// `provider rm` 用它列出还在用的账号（不带 --force 时先列出再拒绝）。
func (s *Store) ListAccountsByProvider(name string) ([]Account, error) {
	return s.queryAccounts("a.provider = ?", name)
}
