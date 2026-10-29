package gateway

import (
	"context"
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

// probeFallbackModel 是模型列表为空的账号探活时用的模型名。刚建好、还没跑过
// `model sync` / `model probe` 的账号就是这种状态：没有模型名也得打一条请求，
// 因为 `account test` 检验的是凭据与账号整体可用性，与具体模型无关。
const probeFallbackModel = "default"

// Probe 用最小请求探活一个账号，并按结果更新冷却与启用状态。
func (s *Server) Probe(ctx context.Context, a store.Account) (ProbeResult, error) {
	res := ProbeResult{Account: a.Name}

	// 先判空再取：模型列表为空的账号（刚建好、还没同步过）在这里取下标会直接 panic。
	note := ""
	model := probeFallbackModel
	if len(a.Models) > 0 && a.Models[0] != "" {
		model = a.Models[0]
	} else {
		note = "这个账号还没有模型，探活用默认模型名 " + probeFallbackModel
	}
	res.Message = note
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

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return res, err
	}
	// 账号探活按账号自己的协议打：它检验的是凭据与账号整体可用性，
	// 不跟模型上的协议发现结果走（那个由 model probe 负责）。
	upReq, err := s.buildRequest(ctx, a, a.Protocol, a.Protocol, raw, ir, string(key))
	if err != nil {
		return res, err
	}

	start := s.now()
	resp, err := s.client.Do(upReq)
	res.Latency = s.now().Sub(start)
	if err != nil {
		res.Message = withNote(err.Error(), note)
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
		if err := s.store.ClearFailure(a.ID); err != nil {
			return res, err
		}
		return res, nil
	}

	res.Message = withNote(readErrorBody(resp.Body), note)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// 401 才是凭据问题。403 多为套餐或模型权限，凭据本身可用，不停用账号。
		err = s.store.SetEnabled(a.ID, false, res.Message)
	case account.Retryable(resp.StatusCode):
		until := s.now().Add(account.Cooldown(resp.StatusCode, a.FailStreak)).Unix()
		err = s.store.RecordFailure(a.ID, until, res.Message)
	}
	return res, err
}

// withNote 把「账号还没有模型」这类说明并进备注。上游的错误文本与这条说明都要留着：
// 错误文本说明这次为什么失败，说明解释用的是哪个模型名，少哪一条都得再猜一次。
func withNote(msg, note string) string {
	switch {
	case note == "":
		return msg
	case msg == "":
		return note
	}
	return msg + "；" + note
}
