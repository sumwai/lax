package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// ProbeStageModels 与 ProbeStageCall 是 `account test` 得出结论的那一步：models 是模型列表接口，
// call 是最小请求。备注文字与 `--json` 的 stage 都取这里，两阶段的结果因此能区分开。
const (
	ProbeStageModels = "models"
	ProbeStageCall   = "call"
)

// ProbeResult 是一次探活的结果。
type ProbeResult struct {
	Account string
	// Stage 是得出结论的那一步：ProbeStageModels 或 ProbeStageCall。
	Stage  string
	Status int
	// Models 是列表接口报出的上游模型数，没走列表接口或没有数出来时为 0。
	Models int
	// ModelsKnown 说明 Models 是真的数过（列表接口 2xx 且解析得了）。
	// 「上游有 0 个模型」与「不知道几个」要能分开：输出里前者显示 0，后者显示 -。
	ModelsKnown bool
	// Latency 是得出结论那一阶段的上游耗时。
	Latency time.Duration
	Message string
	OK      bool
}

// probeFallbackModel 是模型列表为空的账号探活时用的模型名。刚建好、还没跑过
// `model sync` / `model probe` 的账号就是这种状态：没有模型名也得打一条请求，
// 因为 `account test` 检验的是凭据与账号整体可用性，与具体模型无关。
const probeFallbackModel = "default"

// Probe 分两步探活一个账号：先 GET 模型列表，2xx 就判凭据与连通性正常，这一条不花 token，
// 也不依赖某个模型名能不能用；列表接口不可用（404、405、403，或这家取不到列表地址）时退回一条
// max_tokens=1 的请求（见 DESIGN 4、5.7）。两个阶段的状态更新都只经 applyProbeState。
func (s *Server) Probe(ctx context.Context, a store.Account) (ProbeResult, error) {
	res, fallbackNote, err := s.probeModelsEndpoint(ctx, a)
	if err != nil || fallbackNote == "" {
		return res, err
	}
	return s.probeMinimalCall(ctx, a, fallbackNote)
}

// probeModelsEndpoint 是第一步：GET 模型列表，地址与凭据头跟 `model sync` 同一套规则
// （见 modelsEndpoint）。返回的 fallbackNote 非空表示这一步得不出结论，调用方应退回最小请求；
// note 里写清为什么（列表接口 404、取不到地址、连不上），两阶段都失败时备注靠它区分是哪一步。
func (s *Server) probeModelsEndpoint(ctx context.Context, a store.Account) (ProbeResult, string, error) {
	res := ProbeResult{Account: a.Name, Stage: ProbeStageModels}

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return res, "", err
	}
	secretValue := string(key)

	target, protocol, err := s.modelsEndpoint(a, "")
	if err != nil {
		// 厂商声明了路由却一个模型列表路径都没有：这不算账号有问题，
		// 退回最小请求，凭据好不好用照样测得出来。
		return res, "模型列表地址取不到，改用最小请求（" + err.Error() + "）", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return res, "模型列表地址不合法，改用最小请求（" + target + "）", nil
	}
	req.Header.Set("Accept", "application/json")
	// 凭据头跟着取到这条地址的协议走，与 `model sync` 一致：deepseek 的 anthropic 账号
	// 取到的是 chat 路由上的 /v1/models，得发 Bearer，按账号协议发 x-api-key 只会换回 401。
	setAuth(req, protocol, secretValue)

	start := s.now()
	resp, err := s.client.Do(req)
	res.Latency = s.now().Sub(start)
	if err != nil {
		// 连不上不是「这家没有列表接口」，可能只是这个地址被网关挡住或者网络抖动：
		// 照样退回最小请求，两条路都试过再下结论，结论取模型调用那次。
		// 状态照转发路径的记法记 502（网络失败没有状态码），冷却因此按 5xx 那档算。
		res.Status = http.StatusBadGateway
		res.Message = scrubSecret("上游请求失败："+err.Error(), secretValue)
		return res, "模型列表接口不可用，改用最小请求（" + res.Message + "）", nil
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		count, known := countModels(resp.Body)
		res.OK = true
		res.Models = count
		res.ModelsKnown = known
		res.Message = "列表接口可用"
		if !known {
			// 形状不对只说明看不到模型个数，凭据与连通性仍然正常：不判失败。
			res.Message = "列表接口可用，但上游返回的不是模型列表"
		}
		return res, "", s.applyProbeState(a, res)
	case fallsBackToCall(resp.StatusCode):
		msg := scrubSecret(readErrorBody(resp.Body), secretValue)
		return res, fmt.Sprintf("列表接口返回 %d，改用最小请求：%s", resp.StatusCode, msg), nil
	}

	// 401 是凭据问题，429/5xx 要打冷却，其它 4xx 只报原话：这三种都不必再花一次调用。
	res.Message = fmt.Sprintf("列表接口返回 %d：%s", resp.StatusCode, scrubSecret(readErrorBody(resp.Body), secretValue))
	return res, "", s.applyProbeState(a, res)
}

// fallsBackToCall 判断列表接口的这个状态是否说明「这家没有列表接口，或网关把列表拦住了」：
// 404 与 405 是这家没有这个接口，403 有的网关就是按路径拒（与 model probe 换协议的判据同一组）。
// 这三种退回最小请求，不判账号有问题。
func fallsBackToCall(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		return true
	}
	return false
}

// probeMinimalCall 是第二步：发一条 max_tokens=1 的请求，模型取 accounts.models 第一项，
// 为空时用默认模型名并在备注里说明。只有列表接口得不出结论时才走这里，
// listNote 说明那一步为什么没成，与「还没有模型」的说明一起并进备注。
func (s *Server) probeMinimalCall(ctx context.Context, a store.Account, listNote string) (ProbeResult, error) {
	res := ProbeResult{Account: a.Name, Stage: ProbeStageCall}

	// 先判空再取：模型列表为空的账号（刚建好、还没同步过）在这里取下标会直接 panic。
	model := probeFallbackModel
	note := listNote
	if len(a.Models) > 0 && a.Models[0] != "" {
		model = a.Models[0]
	} else {
		note = withNote(note, "这个账号还没有模型，探活用默认模型名 "+probeFallbackModel)
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

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return res, err
	}
	secretValue := string(key)
	// 账号探活按账号自己的协议打：它检验的是凭据与账号整体可用性，
	// 不跟模型上的协议发现结果走（那个由 model probe 负责）。
	upReq, err := s.buildRequest(ctx, a, a.Protocol, a.Protocol, raw, ir, secretValue)
	if err != nil {
		return res, err
	}

	start := s.now()
	resp, err := s.client.Do(upReq)
	res.Latency = s.now().Sub(start)
	if err != nil {
		// 网络失败没有状态码，按转发路径的记法记 502：超时与连不上都该打冷却（见 DESIGN 5.2、5.3）。
		res.Status = http.StatusBadGateway
		res.Message = withNote("模型调用失败："+scrubSecret(err.Error(), secretValue), note)
		return res, s.applyProbeState(a, res)
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if res.OK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		// 探活成功说明凭据可用，把之前因 401 停用的账号恢复回来。
		res.Message = withNote("模型调用可用（模型 "+model+"）", note)
		return res, s.applyProbeState(a, res)
	}

	res.Message = withNote(fmt.Sprintf("模型调用返回 %d：%s",
		resp.StatusCode, scrubSecret(readErrorBody(resp.Body), secretValue)), note)
	return res, s.applyProbeState(a, res)
}

// applyProbeState 按结论更新账号状态，是 `account test` 唯一的写入路径：两个阶段都只经它落库，
// 冷却、清零与停用不会长出第二套算法（见 DESIGN 5.3、6）。2xx 启用账号并清零（冷却、last_error
// 与 fail_streak 一起），401 停用并把错误写进 last_error，429/5xx 按连续失败次数打冷却，
// 其它 4xx 只报原话、不动状态。冷却时长不加抖动：失败几次就是几次，同一状态永远算出同一个时长。
func (s *Server) applyProbeState(a store.Account, res ProbeResult) error {
	switch {
	case res.OK:
		if err := s.store.SetEnabled(a.ID, true, ""); err != nil {
			return err
		}
		return s.store.ClearFailure(a.ID)
	case res.Status == http.StatusUnauthorized:
		// 401 才是凭据问题。403 多为套餐或模型权限，凭据本身可用，不停用账号。
		return s.store.SetEnabled(a.ID, false, res.Message)
	case account.Retryable(res.Status):
		until := s.now().Add(account.Cooldown(res.Status, a.FailStreak)).Unix()
		return s.store.RecordFailure(a.ID, until, res.Message)
	}
	return nil
}

// withNote 把「账号还没有模型」「列表接口 404」这类说明并进备注。上游的错误文本与这些说明都要留着：
// 错误文本说明这次为什么失败，说明解释的是哪一步、用的哪个模型名，少哪一条都得再猜一次。
func withNote(msg, note string) string {
	switch {
	case note == "":
		return msg
	case msg == "":
		return note
	}
	return msg + "；" + note
}
