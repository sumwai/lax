package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// ProbeResult 是一次探活的结果。
type ProbeResult struct {
	Account string
	Status  int
	Latency time.Duration
	Message string
	OK      bool
}

// Probe 用最小请求探活一个账号，并按结果更新冷却与启用状态。
func (s *Server) Probe(ctx context.Context, a store.Account) (ProbeResult, error) {
	res := ProbeResult{Account: a.Name}

	model := a.Models[0]
	if len(a.Models) == 0 {
		model = "default"
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

	key, err := secret.Decrypt(s.key, a.KeyCipher)
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
		res.Message = err.Error()
		return res, nil
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if res.OK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		// 探活成功说明凭据可用，把之前因 401 停用的账号恢复回来。
		if err := s.store.SetEnabled(a.ID, true, ""); err != nil {
			return res, err
		}
		if err := s.store.SetCooldown(a.ID, 0, ""); err != nil {
			return res, err
		}
		return res, nil
	}

	res.Message = readErrorBody(resp.Body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		err = s.store.SetEnabled(a.ID, false, res.Message)
	case account.Retryable(resp.StatusCode):
		until := s.now().Add(account.Cooldown(resp.StatusCode, 0)).Unix()
		err = s.store.SetCooldown(a.ID, until, res.Message)
	}
	return res, err
}
