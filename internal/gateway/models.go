package gateway

import (
	"context"
	"encoding/json"
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

// ModelsEndpoint 由 base_url 推出模型列表地址。末尾带不带 /v1 都行。
func ModelsEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// SyncModels 拉取上游的模型列表，按名字排序去重。
func (s *Server) SyncModels(ctx context.Context, a store.Account) ([]string, error) {
	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return nil, err
	}

	endpoint := ModelsEndpoint(a.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("模型列表地址 %q 不合法，检查账号 %s 的 base-url: %w", endpoint, a.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	setAuth(req, a.Protocol, string(key))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型列表失败（GET %s）：%s", endpoint,
			scrubSecret(err.Error(), string(key)))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游返回 %d（GET %s）：%s；检查该账号的 base-url 与凭据",
			resp.StatusCode, endpoint, scrubSecret(readErrorBody(resp.Body), string(key)))
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

// defaultProbeProtocols 是协议发现的默认候选顺序。候选列表是入参，默认三种全试；
// 以后按厂商声明收紧时只传该厂商声明过的协议，这里的循环不用改。
var defaultProbeProtocols = []string{
	translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses,
}

// protocolOrder 算出尝试顺序：accountProtocol 优先，其余按 candidates 的顺序补齐。
// 顺序固定是为了同一模型多次探活得同一个结论。candidates 为空时用默认的三种。
func protocolOrder(accountProtocol string, candidates []string) []string {
	if len(candidates) == 0 {
		candidates = defaultProbeProtocols
	}
	order := make([]string, 0, len(candidates)+1)
	order = append(order, accountProtocol)
	for _, p := range candidates {
		if p != accountProtocol {
			order = append(order, p)
		}
	}
	return order
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
// 顺手发现这个模型该用哪种协议（候选协议用默认的三种）。
func (s *Server) ProbeModel(ctx context.Context, a store.Account, model string) (ModelProbe, error) {
	return s.ProbeModelWithProtocols(ctx, a, model, nil)
}

// ProbeModelWithProtocols 按给定候选协议列表探活一个模型。candidates 为空表示默认三种全试。
//
// 先用账号协议试一次：2xx 就是结论；401 是凭据问题，不换协议，停用账号并停下；
// 只有可能只是路径不对时才换下一个候选（见 retryOtherProtocol）。
// 换协议后的第一次 2xx 即结论，把该协议写进 account_models.protocol，剩下的候选不再试。
// 换协议后的失败都不是结论：那里的 401 只能说明这个协议不认这套凭据头（anthropic 用
// x-api-key，另两家用 Bearer），其它失败也只是这条路不通，所以结论仍取账号协议那一次。
func (s *Server) ProbeModelWithProtocols(ctx context.Context, a store.Account, model string, candidates []string) (ModelProbe, error) {
	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return ModelProbe{Model: model, Protocol: a.Protocol}, err
	}
	secretValue := string(key)

	var primary probeAttempt
	var storeErr error
	for i, protocol := range protocolOrder(a.Protocol, candidates) {
		att := s.probeOnce(ctx, a, model, protocol, secretValue)
		if i == 0 {
			primary = att
		}

		if att.res.Status == store.ModelUsable {
			// 2xx 就是结论：记下这次用的协议，剩下的候选不再试。
			s.recordProbeRequest(a, model, att)
			if err := s.store.SetModelProtocol(a.ID, model, protocol); err != nil {
				storeErr = fmt.Errorf("账号 %s 的模型 %s 探活成功但记录协议失败: %w", a.Name, model, err)
			}
			return att.res, storeErr
		}

		if i == 0 && att.res.Status == store.ModelAuthFailed {
			// 401 是凭据问题，与协议无关：停用账号并立即收工。
			// 停用失败不能改写结论：调用方靠 auth 决定「碰到 401 就停下」。
			s.recordProbeRequest(a, model, att)
			if err := s.store.SetEnabled(a.ID, false, att.res.Message); err != nil {
				storeErr = fmt.Errorf("停用账号 %s 失败，需手工处理: %w", a.Name, err)
			}
			return att.res, storeErr
		}

		if retryOtherProtocol(att.res) {
			// 可能只是路径不对，按固定顺序试下一个候选。
			continue
		}

		// 明确结论（例如说是模型不存在，或者 5xx 与网络错误）：不换协议。
		// 换协议之后的这类失败也不作数，回账号协议那次的结论。
		if i == 0 {
			s.recordProbeRequest(a, model, att)
			return att.res, nil
		}
		break
	}

	// 候选协议都试过还是失败：账号协议那次的判定就是结论，protocol 保持原值。
	s.recordProbeRequest(a, model, primary)
	return primary.res, storeErr
}

// probeOnce 用指定协议发一条 max_tokens=1 的请求，只得出结论，不写库。
// 记账只记得出判定的那一次（见 ProbeModelWithProtocols）：中间的重试都没得出判定，
// 也没有产生 token，不单独记行。
func (s *Server) probeOnce(ctx context.Context, a store.Account, model, protocol, key string) probeAttempt {
	att := probeAttempt{res: ModelProbe{Model: model, Protocol: protocol}}
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

	start := s.now()
	resp, err := s.client.Do(upReq)
	att.start = start
	res.Latency = s.now().Sub(start)
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

// recordProbeRequest 把探活得出判定的那次上游调用记进 requests。探活是真实计费的调用，
// 不记就等于在用量里消失。入站与出站都取这次实际用的协议。
// 记账失败只写日志，不改变探活结论。
func (s *Server) recordProbeRequest(a store.Account, model string, att probeAttempt) {
	protocol := att.res.Protocol
	if protocol == "" {
		protocol = a.Protocol
	}
	rec := &store.RequestRecord{
		TS:             att.start.Unix(),
		Inbound:        protocol,
		Outbound:       protocol,
		ModelRequested: model,
		ModelUsed:      model,
		AccountID:      a.ID,
		HTTPStatus:     att.res.HTTPStatus,
		OK:             att.res.HTTPStatus >= 200 && att.res.HTTPStatus < 300,
		InTokens:       att.usage.in,
		OutTokens:      att.usage.out,
		CacheRead:      att.usage.cacheRead,
		CacheWrite:     att.usage.cacheWrite,
		LatencyMS:      att.res.Latency.Milliseconds(),
		Error:          att.res.Message,
	}
	if err := s.store.InsertRequest(rec); err != nil {
		s.logf("写探活请求记录失败: %v", err)
	}
}
