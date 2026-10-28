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

	ping := "ping"
	req := &translate.AnthropicRequest{
		Model:     model,
		MaxTokens: 1,
		Messages:  []translate.AnthropicMessage{{Role: "user", Content: translate.Content{Text: &ping}}},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	upReq, err := s.buildRequest(ctx, a, raw, req, string(key))
	if err != nil {
		return res, err
	}

	start := s.now()
	resp, err := s.client.Do(upReq)
	res.Latency = s.now().Sub(start)
	if err != nil {
		res.Status = store.ModelError
		res.Message = scrubSecret("上游请求失败: "+err.Error(), string(key))
		return res, nil
	}
	defer resp.Body.Close()

	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		res.Status = store.ModelUsable
		return res, nil
	}

	msg := scrubSecret(readErrorBody(resp.Body), string(key))
	res.Message = msg
	lower := strings.ToLower(msg)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		res.Status = store.ModelAuthFailed
		if err := s.store.SetEnabled(a.ID, false, msg); err != nil {
			return res, err
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
	return res, nil
}
