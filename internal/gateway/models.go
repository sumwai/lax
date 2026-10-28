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
type ModelProbe struct {
	Model      string
	Status     string
	HTTPStatus int
	Message    string
	Latency    time.Duration
}

// ProbeModel 用一条 max_tokens=1 的请求判断账号能不能用这个模型。
// 只有 2xx 记为可用；401 会让账号停用，避免继续拿错凭据试。
func (s *Server) ProbeModel(ctx context.Context, a store.Account, model string) (ModelProbe, error) {
	res := ModelProbe{Model: model}

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return res, err
	}

	ir := &translate.Request{
		Model:     model,
		MaxTokens: 1,
		Messages: []translate.Message{{
			Role:   translate.RoleUser,
			Blocks: []translate.Block{{Kind: translate.BlockText, Text: "ping"}},
		}},
	}
	raw, err := translate.EncodeRequest(a.Protocol, ir, model)
	if err != nil {
		return res, err
	}
	upReq, err := s.buildRequest(ctx, a, a.Protocol, raw, ir, string(key))
	if err != nil {
		return res, err
	}

	start := s.now()
	resp, err := s.client.Do(upReq)
	res.Latency = s.now().Sub(start)
	if err != nil {
		res.Status = store.ModelError
		res.Message = scrubSecret("上游请求失败: "+err.Error(), string(key))
		s.recordProbeRequest(a, model, start, res, probeUsage{})
		return res, nil
	}
	defer resp.Body.Close()

	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		usage := readProbeUsage(a.Protocol, body)
		res.Status = store.ModelUsable
		s.recordProbeRequest(a, model, start, res, usage)
		return res, nil
	}

	msg := scrubSecret(readErrorBody(resp.Body), string(key))
	res.Message = msg
	lower := strings.ToLower(msg)
	var storeErr error
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// 401 是凭据问题，先把结论定下来，再试着停用账号。
		// 停用失败不能改写结论：调用方靠 auth 决定「碰到 401 就停下」，
		// 改成 error 会让它继续拿同一份坏凭据试下一个模型、下一个账号。
		res.Status = store.ModelAuthFailed
		if err := s.store.SetEnabled(a.ID, false, msg); err != nil {
			storeErr = fmt.Errorf("停用账号 %s 失败，需手工处理: %w", a.Name, err)
		}
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
	s.recordProbeRequest(a, model, start, res, probeUsage{})
	return res, storeErr
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

// recordProbeRequest 把探活的这次上游调用记进 requests。探活是真实计费的调用，
// 不记就等于在用量里消失。记账失败只写日志，不改变探活结论。
func (s *Server) recordProbeRequest(a store.Account, model string, start time.Time, res ModelProbe, usage probeUsage) {
	rec := &store.RequestRecord{
		TS:             start.Unix(),
		Inbound:        a.Protocol,
		Outbound:       a.Protocol,
		ModelRequested: model,
		ModelUsed:      model,
		AccountID:      a.ID,
		HTTPStatus:     res.HTTPStatus,
		OK:             res.HTTPStatus >= 200 && res.HTTPStatus < 300,
		InTokens:       usage.in,
		OutTokens:      usage.out,
		CacheRead:      usage.cacheRead,
		CacheWrite:     usage.cacheWrite,
		LatencyMS:      res.Latency.Milliseconds(),
		Error:          res.Message,
	}
	if err := s.store.InsertRequest(rec); err != nil {
		s.logf("写探活请求记录失败: %v", err)
	}
}
