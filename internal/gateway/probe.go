package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// ProbeStageModels 与 ProbeStageCall 是 `account test` 的两段：models 是模型列表接口，
// 证明地址通、上游连得上；call 是最小请求，证明凭据有效。备注文字与 `--json` 的 stage 都取这里，
// 两段的结果因此能区分开。
const (
	ProbeStageModels = "models"
	ProbeStageCall   = "call"
)

// ProbeStep 是一段探活的结论。
type ProbeStep struct {
	// Stage 是这一段的名字：ProbeStageModels 或 ProbeStageCall。
	Stage  string
	Status int
	OK     bool
	// Models 与 ModelsKnown 只有列表那一段有意义：上游报出多少个模型、以及这个数是不是真数出来的。
	// 「上游有 0 个模型」与「不知道几个」要能分开：输出里前者显示 0，后者显示 -。
	Models      int
	ModelsKnown bool
	// Model 也只有列表那一段有意义：清单里的第一个模型名（与 model sync 同一份解析、排序去重后的
	// 第一项）。账号自己还没有模型时，最小调用拿它当模型名——列表已经给出了一个真实存在的名字，
	// 再用固定名字去打会把「模型名不对」误报成账号失败。空串表示这一步没拿到名字
	// （列表接口没成，或上游返回的不是模型列表）。
	Model   string
	Latency time.Duration
	Message string
}

// ProbeResult 是一次探活的结果。主结论（Stage、Status、OK、Latency、Message）取模型调用那一段：
// 列表接口 2xx 只证明地址通，公开的列表接口对任何 key 都回 200，凭据有效与否只能靠一次真实调用
// （见 DESIGN 4、5.7）。Models 与 ModelsKnown 仍取列表那一段——只有列表接口报得出上游模型数。
// List 留着列表那一段的完整结论，人读输出与 `--json` 靠它把两段分开说清楚。
type ProbeResult struct {
	Account     string
	Stage       string
	Status      int
	Models      int
	ModelsKnown bool
	Latency     time.Duration
	Message     string
	OK          bool
	List        ProbeStep
}

// probeFallbackModel 是探活最小调用的最后一个模型名候选：账号的 `accounts.models` 里没有名字、
// 列表那一步也没拿到名字（列表接口 404/405/403、连不上，或上游返回的不是模型列表）时用它。
// 刚建好、还没跑过 `model sync` / `model probe` 的账号就是这种状态：没有模型名也得打一条请求，
// 因为 `account test` 检验的是凭据与账号整体可用性，与具体模型无关。它是第三顺位，
// 前两档都是真实存在的名字（见 probeMinimalCall 与 ProbeStep.Model），走到这里只是没有名字可用。
const probeFallbackModel = "default"

// Probe 分两步探活一个账号：先 GET 模型列表，证明地址通并报出上游模型数；列表 2xx 之后仍然发一条
// max_tokens=1 的请求证明凭据有效——列表接口不需要凭据时，任何 key 都能换回 200，光凭它判不出凭据
// 好不好。列表接口不可用（404、405、403、取不到列表地址或连不上）时也走这条最小调用，只是备注里
// 写清列表那一步为什么没成。两段都通过才判正常，状态与退出码按调用那段算；列表接口自己回
// 401/429/5xx/其它 4xx 时已经是凭据或上游的结论，不再花一次调用。两个阶段的状态更新都只经
// applyProbeState（见 DESIGN 4、5.7）。
func (s *Server) Probe(ctx context.Context, a store.Account) (ProbeResult, error) {
	list, note, decided, err := s.probeModelsEndpoint(ctx, a)
	if err != nil {
		return withList(resultFromStep(a.Name, list), list), err
	}
	if decided {
		// 列表接口自己就给出了结论（401、429/5xx、其它 4xx）：结论取这一段。
		return withList(resultFromStep(a.Name, list), list), nil
	}
	call, err := s.probeMinimalCall(ctx, a, note, list.Model)
	return withList(resultFromStep(a.Name, call), list), err
}

// resultFromStep 把一段的结论摊成 ProbeResult 的主结论。
func resultFromStep(accountName string, step ProbeStep) ProbeResult {
	return ProbeResult{
		Account: accountName,
		Stage:   step.Stage,
		Status:  step.Status,
		OK:      step.OK,
		Latency: step.Latency,
		Message: step.Message,
	}
}

// withList 把列表那一段并进结果。上游模型数只有列表那一段报得出来，主结论的 Models 因此取它，
// 与决定成败的调用那一段无关：列表通了、调用 401 时上游确实有 94 个模型，这一列照旧写 94。
func withList(res ProbeResult, list ProbeStep) ProbeResult {
	res.List = list
	res.Models = list.Models
	res.ModelsKnown = list.ModelsKnown
	return res
}

// probeModelsEndpoint 是第一步：GET 模型列表，地址与凭据头跟 `model sync` 同一套规则
// （见 modelsEndpoint）。返回的 fallbackNote 非空表示这一步得不出结论，调用方应发一条最小请求；
// note 里写清为什么（列表接口 404、取不到地址、连不上），两段都失败时备注靠它区分是哪一步。
// decided 为真表示这一步已经是最终结论（状态已按 5.3 落库），不必再花一次调用。
func (s *Server) probeModelsEndpoint(ctx context.Context, a store.Account) (step ProbeStep, fallbackNote string, decided bool, err error) {
	step = ProbeStep{Stage: ProbeStageModels}

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return step, "", false, err
	}
	secretValue := string(key)

	target, protocol, err := s.modelsEndpoint(a, "")
	if err != nil {
		// 厂商声明了路由却一个模型列表路径都没有：这不算账号有问题，
		// 退回最小请求，凭据好不好用照样测得出来。
		step.Message = "模型列表地址取不到（" + err.Error() + "）"
		return step, step.Message + "，改用最小请求", false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		step.Message = "模型列表地址不合法（" + target + "）"
		return step, step.Message + "，改用最小请求", false, nil
	}
	req.Header.Set("Accept", "application/json")
	// 凭据头跟着取到这条地址的协议走，与 `model sync` 一致：deepseek 的 anthropic 账号
	// 取到的是 chat 路由上的 /v1/models，得发 Bearer，按账号协议发 x-api-key 只会换回 401。
	setAuth(req, protocol, secretValue)
	// 模型列表请求也带厂商附加头（见 DESIGN 5.7、5.10）。读库失败在这里算作错误而不是退回
	// 最小请求：那是数据库或配置的故障，不是这家没有列表接口。
	if err := s.applyProviderHeaders(req, a, protocol, "", ""); err != nil {
		return step, "", false, err
	}

	start := s.now()
	resp, err := s.client.Do(req)
	step.Latency = s.now().Sub(start)
	if err != nil {
		// 连不上不是「这家没有列表接口」，可能只是这个地址被网关挡住或者网络抖动：
		// 照样退回最小请求，两条路都试过再下结论，结论取模型调用那次。
		// 状态照转发路径的记法记 502（网络失败没有状态码），冷却因此按 5xx 那档算。
		step.Status = http.StatusBadGateway
		step.Message = scrubSecret("上游请求失败："+err.Error(), secretValue)
		return step, "模型列表接口不可用，改用最小请求（" + step.Message + "）", false, nil
	}
	defer resp.Body.Close()

	step.Status = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		models, known := readModelList(resp.Body)
		step.OK = true
		step.Models = len(models)
		step.ModelsKnown = known
		if len(models) > 0 {
			// 记下清单第一项：账号还没同步过模型时，最小调用用它打，而不是用一个几乎肯定
			// 不存在的固定名字（上游回 503「无法为模型 default 选择渠道」那种）。
			step.Model = models[0]
		}
		if known {
			step.Message = fmt.Sprintf("列表接口可用（GET %s，上游 %d 个模型）", target, len(models))
		} else {
			// 形状不对只说明看不到模型个数，不代表地址不通：这一段仍算通过。
			step.Message = fmt.Sprintf("列表接口可用（GET %s），但上游返回的不是模型列表", target)
		}
		// 列表 2xx 只证明地址通：公开的列表接口对任何 key 都回 200，凭据要另发一次调用才知道。
		return step, step.Message, false, nil
	case fallsBackToCall(resp.StatusCode):
		msg := scrubSecret(readErrorBody(resp.Body), secretValue)
		step.Message = fmt.Sprintf("列表接口返回 %d：%s", resp.StatusCode, msg)
		return step, fmt.Sprintf("列表接口返回 %d，改用最小请求：%s", resp.StatusCode, msg), false, nil
	}

	// 401 是凭据问题，429/5xx 要打冷却，其它 4xx 只报原话：这三种都不必再花一次调用。
	step.Message = fmt.Sprintf("列表接口返回 %d：%s", resp.StatusCode, scrubSecret(readErrorBody(resp.Body), secretValue))
	return step, "", true, s.applyProbeState(a, step)
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

// probeMinimalCall 是第二步：发一条 max_tokens=1 的请求。列表接口 2xx 之后也要走这一步——
// 那一步只证明地址通；列表接口不可用时同样走它，只是结论就是这一次调用。
//
// 模型名按三档取，前两档都是真实存在的名字，第三档只是没有名字可用时的兜底：
//
//  1. 账号 `accounts.models` 的第一个非空名字——账号自己声明过模型时按它打；
//  2. 列表那一步拿到的第一个模型名——账号还没同步过模型时，上游清单已经给出了一个真实的名字，
//     再用固定名字去打只会把「模型名不对」误报成账号失败；
//  3. probeFallbackModel——列表那一步没成或清单为空时只能用它，凭据好不好用仍要试出来。
//
// 备注写清这次用的是哪一个、来自哪一档（账号清单 / 上游清单 / 兜底默认名）：只说「模型调用返回 404」
// 看不出是名字不对还是模型真的不能用。listNote 说明列表那一步的结果（通了、上游几个模型，或为什么
// 没成），与模型名来源的说明一起并进备注。
func (s *Server) probeMinimalCall(ctx context.Context, a store.Account, listNote, listModel string) (ProbeStep, error) {
	step := ProbeStep{Stage: ProbeStageCall}

	// 逐项跳过空名字：模型列表为空的账号（刚建好、还没同步过）直接取下标会 panic，
	// 空白名字拿去请求上游也只会换回一个 400。
	model := firstAccountModel(a.Models)
	note := listNote
	switch {
	case model != "":
		note = withNote(note, "模型名取账号清单第一项 "+model)
	case listModel != "":
		model = listModel
		note = withNote(note, "账号还没有模型，模型名取上游清单第一项 "+model)
	default:
		model = probeFallbackModel
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
		return step, err
	}

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return step, err
	}
	secretValue := string(key)
	// 账号探活按账号自己的协议打：它检验的是凭据与账号整体可用性，
	// 不跟模型上的协议发现结果走（那个由 model probe 负责）。附加头由 buildRequest 叠上；
	// 探活没有客户端请求，入站头传 nil（客户端头透传只作用于转发路径，DESIGN 5.11）。
	upReq, err := s.buildRequest(ctx, a, a.Protocol, a.Protocol, nil, raw, ir, secretValue, "")
	if err != nil {
		return step, err
	}

	start := s.now()
	resp, err := s.client.Do(upReq)
	step.Latency = s.now().Sub(start)
	if err != nil {
		// 网络失败没有状态码，按转发路径的记法记 502：超时与连不上都该打冷却（见 DESIGN 5.2、5.3）。
		step.Status = http.StatusBadGateway
		step.Message = withNote("模型调用失败："+scrubSecret(err.Error(), secretValue), note)
		return step, s.applyProbeState(a, step)
	}
	defer resp.Body.Close()

	step.Status = resp.StatusCode
	step.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if step.OK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		// 探活成功说明凭据可用，把之前因 401 停用的账号恢复回来。
		step.Message = withNote("模型调用可用（模型 "+model+"）", note)
		return step, s.applyProbeState(a, step)
	}

	step.Message = withNote(fmt.Sprintf("模型调用返回 %d：%s",
		resp.StatusCode, scrubSecret(readErrorBody(resp.Body), secretValue)), note)
	return step, s.applyProbeState(a, step)
}

// applyProbeState 按结论更新账号状态，是 `account test` 唯一的写入路径：两段都只经它落库，
// 冷却、清零与停用不会长出第二套算法（见 DESIGN 5.3、6）。2xx 启用账号并清零（冷却、last_error
// 与 fail_streak 一起），401 停用并把错误写进 last_error，429/5xx 按连续失败次数打冷却，
// 其它 4xx 只报原话、不动状态。同一份结论永远算出同一个时长，冷却不加抖动。
func (s *Server) applyProbeState(a store.Account, step ProbeStep) error {
	switch {
	case step.OK:
		if err := s.store.SetEnabled(a.ID, true, ""); err != nil {
			return err
		}
		return s.store.ClearFailure(a.ID)
	case step.Status == http.StatusUnauthorized:
		// 401 才是凭据问题。403 多为套餐或模型权限，凭据本身可用，不停用账号。
		return s.store.SetEnabled(a.ID, false, step.Message)
	case account.Retryable(step.Status):
		until := s.now().Add(account.Cooldown(step.Status, a.FailStreak)).Unix()
		return s.store.RecordFailure(a.ID, until, step.Message)
	}
	return nil
}

// firstAccountModel 取账号自己声明的模型列表里的第一个非空名字，没有时返回空串。
// 空白名字不算模型（与 /v1/models 的口径一致，见 DESIGN 5.2.1）：它是 --models 手填或上游清单
// 留下的空档，拿它去请求上游只会换回一个 400，还把「模型名不对」记在凭据头上。
func firstAccountModel(models []string) string {
	for _, name := range models {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return ""
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
