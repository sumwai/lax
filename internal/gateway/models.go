package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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
//  1. override 非空（`model sync --endpoint`）时用它，压过下面全部，协议取账号协议；取值可以是
//     相对 base_url 的路径，也可以是整条地址（整条地址直接访问，不再拼 base_url）；
//  2. 厂商行不存在（自建地址）、或厂商行在但既没有路由也没有 models_path 覆盖时，按 base_url 后缀猜，
//     与升级前一致；
//  3. 厂商级 models_path 非空时用它，协议取账号协议（覆盖是厂商级设置，没有自己的协议）；
//  4. 否则从路由取：账号协议那条的 models_path，为空再退到同厂商按协议名排序的第一条非空值；
//  5. 厂商声明了路由但一个非空 models_path 都没有时报错，提示手填或 `provider set --models-path`。
//
// 第 2 步之后的取值都可能是整条地址：地址来自路由时由 joinEndpoint 判断就不拼 base_url。
// 协议必须与路径一起回去：凭据头按协议选（anthropic 用 x-api-key，另两家用 Bearer），
// 地址来自哪条路由就用哪条路由的协议。deepseek 的 anthropic 账号取到的是 chat 的 /v1/models，
// 用账号协议发 x-api-key 上去只会换回一个 401/403，却把错误指到「凭据无效」上。
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
func (s *Server) SyncModels(ctx context.Context, a store.Account) ([]string, error) {
	return s.SyncModelsWithEndpoint(ctx, a, "")
}

// SyncModelsWithEndpoint 是 `model sync --endpoint <路径>` 的入口：endpoint 非空时只影响这一次调用，
// 相对路径取厂商 base_url 拼上它、整条地址直接访问，忽略厂商的 models_path 覆盖与路由里的值。
func (s *Server) SyncModelsWithEndpoint(ctx context.Context, a store.Account, endpoint string) ([]string, error) {
	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return nil, err
	}

	target, protocol, err := s.modelsEndpoint(a, endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("模型列表地址 %q 不合法，检查账号 %s 的 base-url: %w", target, a.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	// 凭据头跟着取到这条路径的协议走，不是账号协议。
	setAuth(req, protocol, string(key))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型列表失败（GET %s）：%s", target,
			scrubSecret(err.Error(), string(key)))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游返回 %d（GET %s，按 %s 发凭据）：%s；检查该账号的 base-url 与凭据",
			resp.StatusCode, target, protocol, scrubSecret(readErrorBody(resp.Body), string(key)))
	}

	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("模型列表不是合法 JSON: %w", err)
	}

	seen := map[string]bool{}
	var models []string
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("上游没有返回模型，检查 base-url %s 是否正确", a.BaseURL)
	}
	sort.Strings(models)
	return models, nil
}

// countModels 数出模型列表响应里的上游模型个数，形状按 `{data:[{id}]}` 解析（id 为空时认 name），
// 与 SyncModels 同一套取值规则，按名字去重。第二个返回值为 false 表示上游返回的不是模型列表：
// `account test` 只用它报个数，形状不对不判失败——凭据与连通性已经从 2xx 上看出来了。
func countModels(r io.Reader) (int, bool) {
	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 8<<10)).Decode(&payload); err != nil {
		return 0, false
	}
	// 没有 data 字段的响应体（例如 {"ok":true}）不是模型列表；显式给空数组是「0 个模型」，算数得出来。
	if payload.Data == nil {
		return 0, false
	}
	seen := map[string]bool{}
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
	}
	return len(seen), true
}

// ModelProbe 是单个模型在一次探活里的结论。
// Protocol 是这次实际用到的协议：发现成功时是发现到的那个，其余情况下是账号协议。
type ModelProbe struct {
	Model      string
	Status     string
	Protocol   string
	HTTPStatus int
	Message    string
	Latency    time.Duration
}

// probeAttempt 是一次协议尝试的完整结果：结论加记账需要的开始时间与用量。
type probeAttempt struct {
	res   ModelProbe
	start time.Time
	usage probeUsage
}

// defaultProbeProtocols 是探活候选的兜底顺序：账号没有厂商行（或该厂商一个路由都没声明）
// 时三种全试。厂商声明过路由的只试那些（见 routeProtocolOrder），循环本身不认识厂商。
var defaultProbeProtocols = []string{
	translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses,
}

// isWireProtocol 判断一个取值是不是三种线协议之一。别的值不能进探活：编码器遇见未知协议
// 会报错，静默落进 chat 分支更糟——那会把一条配置错误变成对错误地址的上游调用。
func isWireProtocol(p string) bool {
	switch p {
	case translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses:
		return true
	}
	return false
}

// routeProtocolOrder 给出探活一个模型的完整有序候选，顺序与去重都在这里定：
//
//  1. 库里已经发现过这个模型的协议排最前，它是「按事实打」的那次，省掉每次 --refresh
//     白打的一次 404；
//  2. 账号协议紧接其后，它既是对账号配置的验证，也是 401 判定与全失败结论的依据；
//  3. 其余按 resolver 给的顺序：Options.RouteProtocols 注入时用它，否则用该厂商在
//     provider_routes 里声明过的协议（按协议名排序），都没有则三种全试。
//
// override 非 nil 时顶替第 3 步的来源（探活自己指定候选时用）。出现线协议以外的值直接报错。
func (s *Server) routeProtocolOrder(a store.Account, model string, override []string) ([]string, error) {
	var order []string
	seen := map[string]bool{}
	add := func(protocol, from string) error {
		if protocol == "" || seen[protocol] {
			return nil
		}
		if !isWireProtocol(protocol) {
			return fmt.Errorf("%s 给出的协议 %q 不是线协议，可用 %s、%s、%s",
				from, protocol, translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses)
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
	if err := add(a.Protocol, fmt.Sprintf("账号 %s 的 protocol", a.Name)); err != nil {
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
// 探活放宽成三种协议全试，等于去打这家没声明的路径，把一次读库故障变成对错误地址的上游调用。
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
// 用 resolver，也就是该厂商声明过的那些），账号协议与已发现的协议仍由 routeProtocolOrder 排好。
//
// 先用账号协议试一次：2xx 就是结论；401 是凭据问题，不换协议，停用账号并停下；
// 只有可能只是路径不对时才换下一个候选（见 retryOtherProtocol）。
// 换协议后的第一次 2xx 即结论，把该协议写进 account_models.protocol，剩下的候选不再试。
// 换协议后的失败都不是结论：那里的 401 只能说明这个协议不认这套凭据头（anthropic 用
// x-api-key，另两家用 Bearer），其它失败也只是这条路不通，所以接着试下一个候选；
// 全部试完仍失败时，结论取账号协议那一次。
//
// 每次上游调用都在 requests 里记一行，换协议前的中间尝试也记（见 recordProbeAttempts）：
// 一次 403 触发三次调用就应当是三行，账实一致比行数好看重要。
func (s *Server) ProbeModelWithProtocols(ctx context.Context, a store.Account, model string,
	candidates []string) (ModelProbe, error) {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return ModelProbe{Model: model, Protocol: a.Protocol}, err
	}
	secretValue := string(key)

	order, err := s.routeProtocolOrder(a, model, candidates)
	if err != nil {
		return ModelProbe{Model: model, Protocol: a.Protocol, Status: store.ModelError, Message: err.Error()}, err
	}

	// 账号协议那一次是「按账号配置打」的真相：401 判定与全失败时的结论都取它。
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

		if att.res.Status == store.ModelUsable {
			// 2xx 就是结论：记下这次用的协议，剩下的候选不再试。
			s.recordProbeAttempts(a, model, attempts, i)
			if err := s.store.SetModelProtocol(a.ID, model, protocol); err != nil {
				storeErr = fmt.Errorf("账号 %s 的模型 %s 探活成功但记录协议失败: %w", a.Name, model, err)
			}
			return att.res, storeErr
		}

		if i == primaryIdx && att.res.Status == store.ModelAuthFailed {
			// 401 是凭据问题，与协议无关：停用账号并立即收工。
			// 停用失败不能改写结论：调用方靠 auth 决定「碰到 401 就停下」。
			s.recordProbeAttempts(a, model, attempts, i)
			if err := s.store.SetEnabled(a.ID, false, att.res.Message); err != nil {
				storeErr = fmt.Errorf("停用账号 %s 失败，需手工处理: %w", a.Name, err)
			}
			return att.res, storeErr
		}

		if i == primaryIdx && !retryOtherProtocol(att.res) {
			// 账号协议那次的明确结论（模型不存在、5xx、网络错误）与协议无关，不必再花调用。
			s.recordProbeAttempts(a, model, attempts, i)
			return att.res, nil
		}

		// 其余情况一律接着试下一个候选：可能只是路径不对，也可能换协议后的这条不通。
	}

	// 候选协议都试过还是失败：账号协议那次的判定就是结论，protocol 保持原值。
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
		res.Status = store.ModelError
		res.Message = "按协议 " + protocol + " 编码探活请求失败: " + err.Error()
		return att
	}
	upReq, err := s.buildRequest(ctx, a, protocol, protocol, raw, ir, key)
	if err != nil {
		res.Status = store.ModelError
		res.Message = "构造探活请求失败: " + err.Error()
		return att
	}

	resp, err := s.client.Do(upReq)
	res.Latency = s.now().Sub(att.start)
	if err != nil {
		res.Status = store.ModelError
		res.Message = scrubSecret("上游请求失败: "+err.Error(), key)
		return att
	}
	defer resp.Body.Close()

	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		att.usage = readProbeUsage(protocol, body)
		res.Status = store.ModelUsable
		return att
	}

	msg := scrubSecret(readErrorBody(resp.Body), key)
	res.Message = msg
	lower := strings.ToLower(msg)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		res.Status = store.ModelAuthFailed
	case resp.StatusCode == http.StatusForbidden:
		res.Status = store.ModelForbidden
	case resp.StatusCode == http.StatusNotFound:
		res.Status = store.ModelNotFound
	case resp.StatusCode == http.StatusBadRequest &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "model_not_found") ||
			strings.Contains(lower, "unknown model")):
		// 400 里只有明说模型不存在才算 notfound，其余当临时失败，
		// 否则「no available providers match ...」这种会误判成模型名不存在。
		res.Status = store.ModelNotFound
	default:
		res.Status = store.ModelError
	}
	return att
}

// probeUsage 是一次上游调用里读出来的用量。缓存读写同样要记账：
// anthropic 的 scanner 两个都读，chat 与 responses 只读缓存读，缓存写恒为 0。
type probeUsage struct {
	in         int64
	out        int64
	cacheRead  int64
	cacheWrite int64
}

// readProbeUsage 用该协议的 usage scanner 从响应体里读用量，读不到时为 0。
func readProbeUsage(protocol string, body []byte) probeUsage {
	scanner, err := translate.NewUsageScanner(protocol)
	if err != nil {
		return probeUsage{}
	}
	scanner.Feed(body)
	var u probeUsage
	u.in, u.out, u.cacheRead, u.cacheWrite = scanner.Usage()
	return u
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
			OutTokens:      att.usage.out,
			CacheRead:      att.usage.cacheRead,
			CacheWrite:     att.usage.cacheWrite,
			LatencyMS:      att.res.Latency.Milliseconds(),
			Error:          msg,
		}
		if err := s.store.InsertRequest(rec); err != nil {
			s.logf("写探活请求记录失败: %v", err)
		}
	}
}
