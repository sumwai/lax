// Package gateway 是唯一发上游请求的地方：收任意入站协议，挑账号，转成该账号该模型的有效协议，记账。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// 协议常量。入站按路径区分，出站按该模型的有效协议（account_models.protocol 优先，
// 否则账号的 protocol）。gemini 入站与出站都支持（DESIGN 5.9）。
const (
	ProtocolAnthropic = translate.ProtocolAnthropic
	ProtocolChat      = translate.ProtocolChat
	ProtocolResponses = translate.ProtocolResponses
	ProtocolGemini    = translate.ProtocolGemini

	// InboundAnthropic 保留给旧调用方，等价于 ProtocolAnthropic。
	InboundAnthropic = translate.ProtocolAnthropic
)

// Options 是构造网关需要的依赖。
type Options struct {
	Store  *store.Store
	Key    []byte
	Client *http.Client
	Logf   func(format string, args ...any)
	Now    func() time.Time
	// DumpBodies 为真时把发往上游的请求体也打出来，供排障用。
	DumpBodies bool
	// RouteProtocols 给出探活一个模型时该试哪些协议：入参是账号的厂商与模型名，返回按优先
	// 次序排好的候选（nil 表示没有意见）。不注入时用该厂商在 provider_routes 里声明过的协议
	// （见 DESIGN 5.8），厂商没声明就三种全试。账号协议与库里已发现的协议由网关排在候选前面
	// （见 routeProtocolOrder），取值只认三种线协议，出现别的值直接报错，不会静默落进 chat 分支。
	RouteProtocols func(provider, model string) []string
}

// Server 是网关。
type Server struct {
	store      *store.Store
	key        []byte
	client     *http.Client
	logf       func(format string, args ...any)
	now        func() time.Time
	dumpBodies bool
	// routeProtocols 是 Options.RouteProtocols 的原样保存，nil 表示用库里的厂商路由。
	routeProtocols func(provider, model string) []string
}

// New 构造网关。
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("缺少 store")
	}
	if len(o.Key) == 0 {
		return nil, errors.New("缺少解密密钥")
	}
	s := &Server{store: o.Store, key: o.Key, client: o.Client, logf: o.Logf, now: o.Now,
		dumpBodies: o.DumpBodies, routeProtocols: o.RouteProtocols}
	if s.client == nil {
		s.client = &http.Client{Timeout: 0}
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Handler 返回 http 处理器。三条固定入站路径与 gemini 的模型名前缀按路径定协议（见 5.2），
// 另有两个只读的模型清单接口给客户端发现模型名用（见 5.2.1）；`/v1/models/{id}` 用前缀注册，
// 其后全部算模型名，含 `/` 的模型名也照取。其余路径由 mux 返回 404。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		mux.HandleFunc(path, s.handleInbound)
	}
	// gemini 的模型名在路径里（/v1beta/models/{model}:generateContent），按前缀注册，
	// 模型名与流式标志由 translate.GeminiPathTarget 解析（DESIGN 5.9）。
	mux.HandleFunc(translate.GeminiInboundPathPrefix, s.handleInbound)
	mux.HandleFunc(modelsPath, s.handleModelList)
	mux.HandleFunc(modelsPathPrefix, s.handleModelByID)
	return mux
}

// Serve 在给定监听器上运行，直到 ctx 取消。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		case <-done:
		}
	}()
	err := srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "detail": err.Error()})
		return
	}
	models := map[string]bool{}
	enabled := 0
	for _, a := range accounts {
		if a.Enabled {
			enabled++
		}
		for _, m := range a.Models {
			models[m] = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": len(accounts),
		"enabled":  enabled,
		"models":   len(models),
		"inbound":  []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses, ProtocolGemini},
		"outbound": []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses, ProtocolGemini},
		"passthru": "同协议直通，跨协议经中间表示转换",
	})
}

// attempt 是一次上游尝试的结果。
type attempt struct {
	status     int
	inTok      int64
	outTok     int64
	cacheRead  int64
	cacheWrite int64
	// reasoning 是上游上报的思考 token，total 是上游上报的总 token（0 表示没报）。
	// 两个都只用于入账判定（store.ReasoningExtra），转发路径不读它们（见 DESIGN 5.5）。
	reasoning int64
	total     int64
	wrote     bool
	message   string
	err       error
	// transport 表示这一次没有拿到任何 HTTP 响应：连接没建立、请求没发出去、响应体没读完。
	// status 对这类失败仍记 502（表示「网关没能拿到上游响应」），冷却却按连接层那一档算
	// （见 DESIGN 5.3）。已经向客户端写出字节之后的断流不在此列，见 handleInbound 的冷却分支。
	transport bool
}

// cooldownKind 把一次失败的来源折成冷却档位：没拿到 HTTP 响应的是连接层，拿到了状态的按
// 状态码分限流与服务端失败。转发与探活两个入口都只经这一个函数定档，不各自算（见 DESIGN 5.3）。
func cooldownKind(status int, noResponse bool) account.FailureKind {
	if noResponse {
		return account.KindTransport
	}
	return account.KindFromStatus(status)
}

func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	inbound, ok := translate.PathProtocol(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found_error", "未知路径 "+r.URL.Path)
		return
	}
	// gemini 的报文里没有模型名（它在路径上），流式也由路径后缀表示而非请求体里的 stream
	// 字段：两个值都按路径注入（DESIGN 5.9）。用 EscapedPath 取值，模型名里的 %2F 因此仍是
	// 一个路径段，PathUnescape 能还原成 org/gemini。
	var pathModel string
	var pathStream bool
	if inbound == ProtocolGemini {
		model, stream, ok := translate.GeminiPathTarget(r.URL.EscapedPath())
		if !ok {
			writeError(w, http.StatusNotFound, "not_found_error", "未知路径 "+r.URL.Path)
			return
		}
		pathModel, pathStream = model, stream
	}
	if r.Method != http.MethodPost {
		// 405 带上 `Allow: POST`，与两个只读清单接口的 `Allow: GET` 对称：
		// 客户端据此能判断是方法用错了，而不是路径不存在（见 DESIGN 5.2.1）。
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
			"这条入站路径只接受 POST；对话请求用 POST 发到 "+r.URL.Path+"，查模型清单用 GET /v1/models")
		return
	}

	// 从这里开始入站协议已经定下来，错误体按它给形状（writeInboundError）。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}

	req, err := translate.DecodeRequest(inbound, raw)
	if err != nil {
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", inbound+": "+err.Error())
		return
	}
	if inbound == ProtocolGemini {
		req.Model = pathModel
		req.Stream = pathStream
	}
	if strings.TrimSpace(req.Model) == "" {
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}

	session := requestSession(r, raw)

	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeInboundError(w, inbound, http.StatusInternalServerError, "api_error", "读取账号失败: "+err.Error())
		return
	}
	candidates := account.Candidates(accounts, req.Model, s.now(), session)
	if len(candidates) == 0 {
		// 候选为空有两种成因，报错不能混在一起：没有任何账号声明提供这个模型，是清单问题；
		// 有账号提供但全部在冷却或被停用，是可用性问题，仍按 503 走。
		if len(account.Providers(accounts, req.Model)) == 0 {
			writeInboundError(w, inbound, http.StatusNotFound, modelNotFoundKind(inbound), modelNotFoundMessage(req.Model))
			return
		}
		writeInboundError(w, inbound, http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("模型 %s 的账号都在冷却或被停用，先用 switchboard account ls 看状态", req.Model))
		return
	}

	lastErr := "没有可用账号"
	for _, cand := range candidates {
		if r.Context().Err() != nil {
			return
		}

		// 协议归属在模型上：模型行有发现结果就用它，否则用账号的 protocol。
		// 读不出来时退回账号协议，不让一次读库失败把请求打死。
		outbound, err := s.store.EffectiveProtocol(cand.ID, cand.Protocol, req.Model)
		if err != nil {
			s.logf("读账号 %s 模型 %s 的有效协议失败，改按账号协议 %s: %v",
				cand.Name, req.Model, cand.Protocol, err)
			outbound = cand.Protocol
		}

		attemptStart := s.now()
		res := s.tryUpstream(r.Context(), w, cand, outbound, inbound, r.Header, raw, req, session)
		latency := s.now().Sub(attemptStart)

		rec := &store.RequestRecord{
			TS:             attemptStart.Unix(),
			Inbound:        inbound,
			Outbound:       outbound,
			ModelRequested: req.Model,
			ModelUsed:      req.Model,
			AccountID:      cand.ID,
			HTTPStatus:     res.status,
			OK:             res.err == nil && res.status >= 200 && res.status < 300,
			InTokens:       res.inTok,
			// 入账时按出站协议归一化输入 token：anthropic 的 input_tokens 已排除缓存读写，
			// chat 与 gemini 的上游把命中缓存的部分也算在输入里，花费只能按归一化后的这一列折
			// （见 store.UncachedInTokens 与 DESIGN 5.5）。in_tokens 仍是上游原值，展示不变。
			InTokensUncached: store.UncachedInTokens(outbound, res.inTok, res.cacheRead),
			OutTokens:        res.outTok,
			CacheRead:        res.cacheRead,
			CacheWrite:       res.cacheWrite,
			// 思考 token 原值整列入账，是否再按输出价计一次交给 ReasoningExtra：
			// 判定只写在那一处，不在这里按协议猜（见 DESIGN 5.5）。
			ReasoningTokens:      res.reasoning,
			ReasoningTokensExtra: store.ReasoningExtra(res.total, res.inTok, res.outTok, res.reasoning),
			LatencyMS:            latency.Milliseconds(),
			Error:                res.message,
		}
		// 一次尝试的账（requests 一行）与它的结论（冷却、清零、停用）在同一个事务里落库，
		// 要么都写、要么都不写（见 store.RecordAttempt）。
		state := store.AttemptState{}
		switch {
		case rec.OK:
			// 成功一次就把连续失败次数清零，冷却时长下次从头算。
			state.Clear = true
		case res.status == http.StatusUnauthorized:
			// 401 说明凭据本身不行，停用账号，避免同一条错误把整个候选池刷成不可用。
			state.Disable = true
			state.LastError = res.message
		case account.Retryable(res.status) && !res.wrote:
			// 冷却档位看失败的来源：一条 HTTP 响应都没拿到的是连接层（res.transport），
			// 拿到状态的按状态码分限流与服务端失败。
			// 已经向客户端写出字节的失败不进冷却也不换号：流式断流（wrote 为真）发生在输出途中，
			// 重试会把半截响应当成新请求再打一遍，而这一次的结局已经写在客户端那边了。
			// 这类失败因此也不算连接层失败，它与「有没有拿到响应」无关。
			state.Failure = true
			state.CooldownUntil = s.now().Add(account.Cooldown(cooldownKind(res.status, res.transport), cand.FailStreak)).Unix()
			state.LastError = res.message
		}
		if err := s.store.RecordAttempt(rec, state); err != nil {
			s.logf("写请求记录失败: %v", err)
		}
		s.logf("%s %s→%s %s %s %d/%d %s%s",
			cand.Name, inbound, outbound, req.Model,
			fmt.Sprintf("%d", res.status), res.inTok, res.outTok,
			latency.Round(time.Millisecond), errorSuffix(res.message))

		if rec.OK {
			return
		}

		switch {
		case res.status == http.StatusUnauthorized:
			writeInboundError(w, inbound, http.StatusUnauthorized, "authentication_error",
				fmt.Sprintf("账号 %s 的凭据被上游拒绝，已停用。%s", cand.Name, res.message))
			return
		case res.status == http.StatusForbidden:
			// 403 多为套餐或模型权限不对，凭据本身还能用，不停用也不再换号。
			writeInboundError(w, inbound, http.StatusForbidden, "permission_error",
				fmt.Sprintf("账号 %s 无权使用该模型：%s", cand.Name, res.message))
			return
		case state.Failure:
			lastErr = fmt.Sprintf("%s: %s", cand.Name, res.message)
			continue
		case res.wrote:
			return
		default:
			writeInboundError(w, inbound, mapErrorStatus(res.status), errorKind(res.status), res.message)
			return
		}
	}

	writeInboundError(w, inbound, http.StatusServiceUnavailable, "api_error", "所有候选账号都失败："+lastErr)
}

// modelNotFoundKind 给出「没有账号提供该模型」在入站协议里的错误类型：anthropic 用它自己的
// not_found_error，两条 openai 路径用 invalid_request_error（都没有专门的「模型未配置」类型）。
func modelNotFoundKind(inbound string) string {
	if inbound == ProtocolAnthropic {
		return "not_found_error"
	}
	return "invalid_request_error"
}

// modelNotFoundMessage 说明找不到模型，并给出下一步的两条命令：先看现有清单，再探活。
func modelNotFoundMessage(model string) string {
	return fmt.Sprintf("没有账号提供模型 %s；先运行 switchboard model ls 看现有清单，"+
		"再用 switchboard model probe 探活", model)
}

// requestSession 取本次入站请求的会话键（见 DESIGN 5.3）：`x-switchboard-session` 头优先，
// 其次请求体里的 metadata.user_id。选号粘性与附加头的 {session} 都走这一个函数，不写第二份——
// 两处取到不同的值会让「同一个对话」在选号与上游看来不是同一个会话。
func requestSession(r *http.Request, raw []byte) string {
	if v := r.Header.Get("x-switchboard-session"); v != "" {
		return v
	}
	return sessionFromBody(raw)
}

// sessionFromBody 从请求体里取会话标识，用于粘同一个账号。
func sessionFromBody(raw []byte) string {
	var env struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		User           string `json:"user"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	switch {
	case env.Metadata.UserID != "":
		return env.Metadata.UserID
	case env.User != "":
		return env.User
	default:
		return env.ConversationID
	}
}

func (s *Server) tryUpstream(ctx context.Context, w http.ResponseWriter, a store.Account, outbound string,
	inbound string, inboundHeaders http.Header, raw []byte, req *translate.Request, session string) attempt {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return attempt{status: http.StatusInternalServerError, message: "解密账号凭据失败"}
	}

	upReq, err := s.buildRequest(ctx, a, outbound, inbound, inboundHeaders, raw, req, string(key), session)
	if err != nil {
		// 本地构造请求失败（编码、拼地址），请求根本没发出去：与网络无关，不走连接层档，
		// 仍按 502 的 5xx 档冷却。
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		// 连接建立失败、请求发送失败、等响应头时超时：一条 HTTP 响应都没拿到。
		return attempt{status: http.StatusBadGateway, transport: true, message: "上游请求失败: " + err.Error(), err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := readErrorBody(resp.Body)
		return attempt{status: resp.StatusCode, message: scrubSecret(msg, string(key))}
	}

	if req.Stream {
		return s.streamSuccess(w, resp, inbound, outbound, req, string(key))
	}
	return s.jsonSuccess(w, resp, inbound, outbound, req)
}

// buildRequest 按有效出站协议构造上游请求：路径、凭据头、附加头与请求体编码都跟它走。
// 入站协议与它相同时直通（只改 model），不同时经中间表示转换。
// inboundHeaders 是入站请求头，只有转发路径才给（模型同步与探活没有客户端请求，传 nil），
// 白名单里的头按 DESIGN 5.11 透传。
// session 是本次请求的会话键（必要时是空串），供附加头的 {session} 用；模型名取 req.Model。
func (s *Server) buildRequest(ctx context.Context, a store.Account, outbound, inbound string,
	inboundHeaders http.Header, raw []byte,
	req *translate.Request, key, session string) (*http.Request, error) {

	// 路由的 path 里可以有 {model} 占位符（gemini 的模型名在路径里，见 DESIGN 5.9）：
	// 先拼好地址再替换，替换后才是真正要访问的 URL。gemini 的流式另走
	// :streamGenerateContent 与 alt=sse（见 geminiStreamEndpoint）。
	endpoint := modelEndpoint(s.endpoint(a, outbound), req.Model)
	if outbound == ProtocolGemini && req.Stream {
		endpoint = geminiStreamEndpoint(endpoint)
	}

	var body []byte
	var err error
	switch {
	case inbound != outbound:
		body, err = translate.EncodeRequest(outbound, req, req.Model)
	case outbound == ProtocolGemini:
		// gemini 的报文里没有 model 字段（模型名在路径上），所以直通时请求体一个字节都不改：
		// 往里面塞一个 model 只会让上游以「未知字段」拒掉整条请求（DESIGN 5.9）。
		body = raw
	default:
		// 同协议直通：只替换 model，其余字节原样保留。
		body, err = patchModel(raw, req.Model)
	}
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	setAuth(httpReq, outbound, key)
	// 客户端白名单头排在 setAuth 之后、附加头之前：头名冲突的优先级由顺序决定，不另写判断
	// （DESIGN 5.11）：setAuth 的默认值 < 客户端头 < 厂商附加头声明。
	for _, d := range applyClientHeaders(httpReq, inboundHeaders, inbound, outbound) {
		s.logf("客户端头 %s 被丢弃：值有 %d 字节，超过 %d 字节上限，不往上游透传",
			d.Name, d.Bytes, clientHeaderMaxBytes)
	}
	// 附加头压后叠加：同名时声明覆盖客户端头与 setAuth 给的默认凭据头（DESIGN 5.10 的「声明优先」）。
	if err := s.applyProviderHeaders(httpReq, a, outbound, req.Model, session); err != nil {
		return nil, err
	}
	if s.dumpBodies {
		s.logf("→ 上游 %s 请求体: %s", endpoint, truncateForLog(string(body)))
	}
	return httpReq, nil
}

// truncateForLog 截断过长的报文，避免调试输出刷屏。
func truncateForLog(s string) string {
	const max = 800
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// patchModel 只替换 model 字段，其余字节按原样保留。
func patchModel(raw []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = m
	return json.Marshal(fields)
}

func (s *Server) jsonSuccess(w http.ResponseWriter, resp *http.Response,
	inbound, outbound string, req *translate.Request) attempt {

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		// 状态码到了、响应体没读完：同样是一次没拿到完整响应，按连接层档冷却。
		return attempt{status: http.StatusBadGateway, transport: true, message: "读取上游响应失败: " + err.Error(), err: err}
	}

	// 直通：字节原样返回，只用对应协议解析一次用量。
	if inbound == outbound {
		res := attempt{status: http.StatusOK, wrote: true}
		if parsed, err := translate.DecodeResponse(outbound, body, req.Model); err == nil {
			res.inTok, res.outTok = parsed.Usage.In, parsed.Usage.Out
			res.cacheRead, res.cacheWrite = parsed.Usage.CacheRead, parsed.Usage.CacheWrite
			res.reasoning, res.total = parsed.Usage.Reasoning, parsed.Usage.Total
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return res
	}

	parsed, err := translate.DecodeResponse(outbound, body, req.Model)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}
	if parsed.StopNote != "" {
		// 上游的结束原因在中间表示里没有对应取值（gemini 的 SAFETY 一类），统一按正常结束编给
		// 客户端，原值只进日志当备注（DESIGN 5.9）。
		s.logf("上游 %s 的结束原因 %s 在中间表示里没有对应取值，按 %s 编给客户端", outbound, parsed.StopNote, parsed.StopReason)
	}
	payload, err := translate.EncodeResponse(inbound, parsed)
	if err != nil {
		return attempt{status: http.StatusInternalServerError, message: err.Error(), err: err}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
	return attempt{
		status: http.StatusOK, wrote: true,
		inTok: parsed.Usage.In, outTok: parsed.Usage.Out,
		cacheRead: parsed.Usage.CacheRead, cacheWrite: parsed.Usage.CacheWrite,
		reasoning: parsed.Usage.Reasoning, total: parsed.Usage.Total,
	}
}

func (s *Server) streamSuccess(w http.ResponseWriter, resp *http.Response,
	inbound, outbound string, req *translate.Request, secretValue string) attempt {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if inbound == outbound {
		return s.passthroughStream(w, resp, outbound, flush, secretValue)
	}

	dec, err := translate.NewStreamDecoder(outbound)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err, wrote: true}
	}
	enc, err := translate.NewStreamEncoder(inbound, req.Model)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err, wrote: true}
	}

	write := func(events []translate.SSEEvent) error {
		for _, e := range events {
			if _, err := w.Write(e.Bytes()); err != nil {
				return err
			}
		}
		flush()
		return nil
	}

	var usage translate.Usage
	// upstreamErr 记上游自己发过来的错误帧正文：客户端已经收到那条错误，这里不再补一个，
	// 但它仍然说明这次请求失败了，请求记录要写上游的原话而不是一句泛泛的断流。
	// 这条正文会同时写进 requests.error 与发给客户端的错误帧，与非 2xx 路径一样过 scrubSecret：
	// gemini 的 {"error":{…}} 是新增入口，上游把凭据回显在 message 里时不能落库（DESIGN 5.6）。
	// 四个出站协议的错误增量都走这一个分支，脱敏只在这一处做。
	var upstreamErr string
	emit := func(deltas []translate.Delta) error {
		for _, d := range deltas {
			if d.Usage != nil {
				usage = *d.Usage
			}
			if d.StopNote != "" {
				s.logf("上游 %s 的结束原因 %s 在中间表示里没有对应取值，按 %s 编给客户端", outbound, d.StopNote, d.StopReason)
			}
			if d.Kind == translate.DeltaError {
				d.Text = scrubSecret(d.Text, secretValue)
				upstreamErr = d.Text
			}
			if err := write(enc.Emit(d)); err != nil {
				return err
			}
		}
		return nil
	}

	if err := write(enc.Start()); err != nil {
		return attempt{status: http.StatusOK, wrote: true, message: "客户端断开", err: err}
	}

	// ended 记「上游把收尾帧发过来了」：收尾帧没到就是断流，判据与直通路径一致
	// （DESIGN 5.2 第 6 条、5.4），不单靠解码器报错——干净 EOF 时解码器一声不响。
	var ended bool
	scanErr := translate.ScanSSE(resp.Body, func(event string, data []byte) error {
		if streamFrameEnded(outbound, event, data) {
			ended = true
		}
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			return nil
		}
		deltas, err := dec.Decode(data)
		if err != nil {
			return err
		}
		return emit(deltas)
	})

	failure := ""
	switch {
	case scanErr != nil:
		failure = "上游流式响应中断: " + scanErr.Error()
	case !ended && upstreamErr != "":
		failure = "上游流式响应失败: " + upstreamErr
	case !ended:
		failure = truncatedMessage
	}
	if failure != "" && upstreamErr == "" {
		// 错误增量走编码器，不另写一帧：入站协议是 responses 时它就是 error 加
		// response.failed，是 anthropic 时是 event: error——形状跟入站协议走，与非 2xx 响应用的是
		// 同一份映射（DESIGN 5.2 的错误体一节）。
		_ = emit([]translate.Delta{{Kind: translate.DeltaError, Text: failure}})
	}
	_ = emit(dec.Close())
	_ = write(enc.Finish())

	res := attempt{status: http.StatusOK, wrote: true, inTok: usage.In, outTok: usage.Out,
		cacheRead: usage.CacheRead, cacheWrite: usage.CacheWrite,
		reasoning: usage.Reasoning, total: usage.Total}
	if failure != "" {
		// 写出之后的断流：status 仍是 200、wrote 为真，handleInbound 因此不冷却也不换号。
		// 它虽然是「读流的时候断了」，但已经向客户端写出了字节，不算可以重试的连接层失败，
		// 也不改这一类失败的处理方式（见 DESIGN 5.3）。
		// 记账用的摘要也过一遍脱敏：scanErr 的原文里可能带着上游回显的地址或凭据。
		res.message = scrubSecret(failure, secretValue)
		res.err = errStreamTruncated
		if scanErr != nil {
			res.err = scanErr
		}
	}
	return res
}

// passthroughStream 原样转发同协议上游的字节，同时读出用量。
//
// 上游没发到收尾帧就断开时，已写出的字节一个不动，只在末尾追加该协议的合法结尾
// （见 passthroughTail）：直通与转换两条路径对断流的处理一致，客户端总能拿到一个
// 结束了的流，而不是戛然而止的半截字节（DESIGN 5.2 第 6 条、5.4）。
func (s *Server) passthroughStream(w http.ResponseWriter, resp *http.Response, protocol string,
	flush func(), secretValue string) attempt {
	scanner, err := translate.NewUsageScanner(protocol)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err, wrote: true}
	}

	buf := bufio.NewReaderSize(resp.Body, 32*1024)
	var readErr error
	// ended 记「收尾帧已经转发给客户端了」：收尾帧之后再断开不算断流，不再补任何字节，
	// 完整流的字节因此与上游逐字节一致。
	var ended bool
	// tail 是最后写出的至多四个字节，用来判断已写出的内容是不是停在帧边界上（\n\n 或 \r\n\r\n）。
	var tail []byte
	// eventName 是当前帧的 event: 行，遇到空行就清掉：具名事件的收尾帧光凭事件名就能认。
	var eventName string
	// dataBuf 攒当前帧的 data 行：一帧可以有好几行 data，按 \n 拼起来才是完整 JSON，
	// 多行缩进的收尾帧因此也算收尾。
	var dataBuf []byte
	checkFrame := func() {
		if len(dataBuf) == 0 {
			return
		}
		scanner.Feed(dataBuf)
		if streamFrameEnded(protocol, eventName, dataBuf) {
			ended = true
		}
	}

	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return attempt{status: http.StatusOK, wrote: true, message: "客户端断开", err: werr}
			}
			tail = append(tail, line...)
			if len(tail) > 4 {
				tail = tail[len(tail)-4:]
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				// 帧边界：一帧收完再交给 scanner 与收尾判定。
				checkFrame()
				eventName, dataBuf = "", nil
			case bytes.HasPrefix(trimmed, []byte("event:")):
				eventName = strings.TrimSpace(string(trimmed[len("event:"):]))
			case bytes.HasPrefix(trimmed, []byte("data:")):
				if len(dataBuf) > 0 {
					dataBuf = append(dataBuf, '\n')
				}
				dataBuf = append(dataBuf, bytes.TrimSpace(trimmed[len("data:"):])...)
			}
			flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	// 最后一帧可能没有空行结尾：攒下的字节仍然算一帧，用量与收尾判定不能丢。
	checkFrame()

	scanned := scanner.Usage()
	in, out := scanned.In, scanned.Out
	res := attempt{status: http.StatusOK, wrote: true, inTok: in, outTok: out,
		cacheRead: scanned.CacheRead, cacheWrite: scanned.CacheWrite,
		reasoning: scanned.Reasoning, total: scanned.Total}
	if readErr != nil {
		res.message = "上游流式响应中断: " + readErr.Error()
		res.err = readErr
	}
	if !ended {
		if res.err == nil {
			// 干净的 EOF 也可能断在半路：收尾帧没到就是断流，与转换路径同一个判据。
			res.message = truncatedMessage
			res.err = errStreamTruncated
		}
		// 错误帧的正文既发给客户端也写进 requests，与转换路径一样先过 scrubSecret：
		// 读错误里可能带上游回显的地址或凭据（DESIGN 5.6）。
		res.message = scrubSecret(res.message, secretValue)
		appendStreamTail(w, flush, tail, passthroughTail(protocol, res.message, in, out))
	}
	return res
}

// errStreamTruncated 表示上游在收尾帧之前就结束了连接（干净的 EOF，读不出别的错）。
var errStreamTruncated = errors.New("上游流式响应在收尾帧之前结束")

// truncatedMessage 是断流记在 requests 里的说明，也是发给客户端的错误帧正文。
// 直通与转换两条路径用同一句话，客户端看到的与账上记的对得上。
const truncatedMessage = "上游流式响应中断: 收尾帧之前连接就结束了"

// streamFrameEnded 判断这一帧是不是该协议的收尾帧：anthropic 的 message_stop、
// openai-chat 的 [DONE]、openai-responses 的 response.completed / response.incomplete /
// response.failed、gemini 的 candidates[].finishReason。事件名与帧里的 type 字段都认：解析
// JSON 取字段，不按字节子串找，`{"type": "message_stop"}` 这种带空格或换行缩进的合法帧因此
// 也算收尾；SSE 的 event: 行直接给了名字（上游只写事件名、data 是别的形状）时同样算。
func streamFrameEnded(protocol, event string, data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	// chat 的 [DONE] 既没有事件名也不是 JSON，单独认。
	if protocol == ProtocolChat && string(trimmed) == "[DONE]" {
		return true
	}
	if protocol == ProtocolGemini {
		// gemini 没有单独的收尾帧：某个候选带上 finishReason 就是上游把这一轮说完了。
		// 只有 promptFeedback.blockReason 的帧不算收尾（那是上游拒了提示词，不能当成完成）。
		var env struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
		}
		if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &env) != nil {
			return false
		}
		for _, c := range env.Candidates {
			if c.FinishReason != "" {
				return true
			}
		}
		return false
	}

	name := strings.TrimSpace(event)
	if name == "" && len(trimmed) > 0 && trimmed[0] == '{' {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(trimmed, &env); err == nil {
			name = env.Type
		}
	}

	switch protocol {
	case ProtocolAnthropic:
		return name == "message_stop"
	case ProtocolResponses:
		switch name {
		case "response.completed", "response.incomplete", "response.failed":
			return true
		}
	}
	return false
}

// passthroughTail 给出直通路径断流之后要补的收尾帧，按入站（=出站）协议决定形状：
//
//   - anthropic：event: error，再补 message_delta（带已知 usage）与 message_stop；
//   - openai-chat：一帧 data: {"error":…}，再补 data: [DONE]；
//   - openai-responses：event: error，再补 response.failed；
//   - gemini：一帧 data: {"error":…}，再补一帧带 finishReason 与 usageMetadata 的候选（gemini
//     没有单独的结束事件，收尾就是这一帧）。
//
// 错误帧的形状与四条入站路径的非流式错误一致（DESIGN 5.2）：openai-responses 的错误体在
// response.failed 的 response.error 里，gemini 用 google 的 code 与 status——流已经以 200
// 开头，没有别的 HTTP 状态码可用，因此 code 固定 500、status 是 INTERNAL。
//
// 用量是 scanner 已经从已转发帧里读出来的那些，读不到就是 0：补的是一个诚实的结尾，不是重放。
func passthroughTail(protocol, message string, in, out int64) []translate.SSEEvent {
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			return []byte(`{}`)
		}
		return b
	}

	switch protocol {
	case ProtocolAnthropic:
		return []translate.SSEEvent{
			translate.ErrorEvent("api_error", message),
			{Event: "message_delta", Data: marshal(map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": translate.StopEndTurn, "stop_sequence": nil},
				"usage": translate.AnthropicUsage{InputTokens: in, OutputTokens: out},
			})},
			{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
		}
	case ProtocolChat:
		return []translate.SSEEvent{
			{Data: marshal(map[string]any{"error": map[string]string{
				"type": "api_error", "message": message,
			}})},
			{Data: []byte("[DONE]")},
		}
	case ProtocolResponses:
		return []translate.SSEEvent{
			{Event: "error", Data: marshal(map[string]any{"type": "error", "message": message})},
			{Event: "response.failed", Data: marshal(map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id":     "resp_switchboard",
					"object": "response",
					"status": "failed",
					"error":  map[string]string{"code": "server_error", "message": message},
				},
			})},
		}
	case ProtocolGemini:
		// 收尾帧就是带 finishReason 的那一帧；usageMetadata 用 scanner 已经从已转发帧里
		// 读到的值，读不到就是 0。
		return []translate.SSEEvent{
			{Data: marshal(map[string]any{"error": map[string]any{
				"code": 500, "message": message, "status": "INTERNAL",
			}})},
			{Data: marshal(translate.GeminiResponse{
				Candidates: []translate.GeminiCandidate{{
					Content:      translate.GeminiContent{Role: "model", Parts: []translate.GeminiPart{}},
					FinishReason: "STOP",
				}},
				UsageMetadata: &translate.GeminiUsageMetadata{
					PromptTokenCount: in, CandidatesTokenCount: out, TotalTokenCount: in + out,
				},
			})},
		}
	}
	return nil
}

// appendStreamTail 把收尾帧追加到已写出的字节后面，前面已经发出去的内容一个字节都不改。
// 已写出的内容停在半行或半帧（没以空行结尾）时先补一个空行：不补的话追加的第一行会被并进
// 上一个事件，客户端那边仍然不是一个结束了的流。行尾两种都认：`\n\n` 与 CRLF 流的 `\r\n\r\n`
// 都是帧边界，后者已经结束了一帧，再补 `\n\n` 只会多出一个空帧。
func appendStreamTail(w io.Writer, flush func(), written []byte, events []translate.SSEEvent) {
	if len(written) > 0 && !bytes.HasSuffix(written, []byte("\n\n")) &&
		!bytes.HasSuffix(written, []byte("\r\n\r\n")) {
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return
		}
	}
	for _, e := range events {
		if _, err := w.Write(e.Bytes()); err != nil {
			return
		}
	}
	flush()
}

// Endpoint 是 base_url 加协议后缀的兜底猜测，作为没有厂商路由时的兜底：末尾带不带 /v1 都行。
// 正常路径上的地址走 Server.endpoint（按 (厂商, 协议) 查 provider_routes，路由的值可以是
// 相对路径，也可以是整条地址，见 joinEndpoint）。
// gemini 的路径里带模型名，兜底同样留 {model} 占位符，由 modelEndpoint 在转发前替换。
func Endpoint(base, protocol string) string {
	base = normalizedBase(base)
	if protocol == ProtocolGemini {
		// gemini 的原生路径是 /v1beta/models/{model}:generateContent：没有 /v1 这种后缀猜想，
		// 写 base_url 时带上 /v1beta 也不会拼出两层。
		return strings.TrimSuffix(base, "/v1beta") + geminiGeneratePath
	}
	suffix := "/chat/completions"
	switch protocol {
	case ProtocolAnthropic:
		suffix = "/messages"
	case ProtocolResponses:
		suffix = "/responses"
	}
	if strings.HasSuffix(base, "/v1") {
		return base + suffix
	}
	return base + "/v1" + suffix
}

// endpoint 解析账号在某种协议上的上游地址：厂商在 provider_routes 里声明过这条协议就用它的取值——
// 相对路径拼 base_url、整条地址直接用（见 joinEndpoint）；没有厂商行、或这家没有该协议的路由时
// 退回后缀猜测（见 DESIGN 5.8）。自建地址的账号（provider 不在表里）因此仍然可用。
func (s *Server) endpoint(a store.Account, protocol string) string {
	route, ok, err := s.store.LookupProviderRoute(a.Provider, protocol)
	if err != nil {
		s.logf("读厂商 %s 的 %s 路由失败，按 base-url 猜路径: %v", a.Provider, protocol, err)
		return Endpoint(a.BaseURL, protocol)
	}
	if !ok || strings.TrimSpace(route.Path) == "" {
		return Endpoint(a.BaseURL, protocol)
	}
	return joinEndpoint(a.BaseURL, route.Path)
}

// normalizedBase 规整 base_url：只合并路径里的重复斜杠、去掉末尾斜杠（见 store.NormalizeBaseURL）。
// store.NormalizeBaseURL 还会校验协议头，这里只是拼地址，脏地址交给 http.NewRequest 报错。
func normalizedBase(base string) string {
	if u, err := store.NormalizeBaseURL(base); err == nil {
		return u
	}
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// joinEndpoint 把 base_url 与路由里的端点取值拼起来。取值是整条地址（含 `://`）时直接用它，
// 不再拼 base_url：同一厂商的不同协议可以落在不同域名上（openai 兼容端点在 openai.example.com、
// anthropic 端点在 claude.example.com），此时 base_url 只是其中一家的地址。整条地址按
// store.NormalizeEndpoint 规整：scheme 归一成小写、路径合并重复斜杠去尾斜杠，
// 授权部分、查询串与 `#` 之后的内容照抄。相对路径仍与 base_url 拼接：base_url 去尾斜杠、
// 路径补一个前导斜杠并去尾斜杠，因此最终地址的路径部分不出现 `//`，`base_url` 里真实存在的前缀
// （如 /api/paas/v4）原样保留。规整只碰路径（DESIGN 5.8）。其余一律照抄，不推 /v1、不猜后缀。
// 校验不过的脏取值原样返回，交给 http.NewRequest 报错：读侧不改写用户数据。
func joinEndpoint(base, path string) string {
	if store.IsFullAddress(path) {
		if endpoint, err := store.NormalizeEndpoint(path); err == nil {
			return endpoint
		}
		return strings.TrimSpace(path)
	}
	return normalizedBase(base) + store.NormalizePath(path)
}

// geminiGeneratePath 是 gemini 非流式生成的路径形状（相对 base_url，含模型名占位符）。
// 模型名在路径里是 gemini 的原生写法，因此路由的 path 支持 {model} 占位符（DESIGN 5.9）。
const geminiGeneratePath = "/v1beta/models/{model}:generateContent"

// gemini 的两种动作后缀。路由的 path 按非流式写法（:generateContent）给，流式时由
// geminiStreamEndpoint 换掉这一段。
const (
	geminiGenerateAction = ":generateContent"
	geminiStreamAction   = ":streamGenerateContent"
)

// modelPlaceholder 是路由 path 里代表本次请求模型名的占位符。
const modelPlaceholder = "{model}"

// modelEndpoint 把拼好的上游地址里的 {model} 换成模型名：只认这一个字面量，出现几次就换几次，
// 其余字节（包括路径里的 `:` 与查询串）一个字不改。模型名按路径段转义（url.PathEscape），
// 含 `/` 的名字因此不会被拆成两段路径。地址里没有占位符时原样返回，别的协议行为不变。
func modelEndpoint(endpoint, model string) string {
	if !strings.Contains(endpoint, modelPlaceholder) {
		return endpoint
	}
	return strings.ReplaceAll(endpoint, modelPlaceholder, url.PathEscape(model))
}

// geminiStreamEndpoint 把非流式的 gemini 地址改成流式形状：路径末尾的 :generateContent 换成
// :streamGenerateContent，并加一个 alt=sse（不加的话上游返回的是一段 JSON 数组，不是 SSE）。
// 路由如果本来就写的 :streamGenerateContent，只补 alt=sse；路径末尾没有这两个动作后缀
// （自定义路由）时路径不动，只加 alt=sse。字符串拼接而不经 url.Parse：路径里的 `:` 与模型名
// 的转义形式（org%2Fgemini）原样保留。
//
// 只认路径末尾（查询串之前）的后缀：模型名里可以带 `:`（url.PathEscape 不转义它），路由本身
// 没有这个后缀时模型名里的 :generateContent 就会留在路径中间；在整个地址里找子串会把它改掉，
// 同一个模型的流式与非流式因此打到两个不同地址（DESIGN 5.9）。
func geminiStreamEndpoint(endpoint string) string {
	pathPart, query := endpoint, ""
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		pathPart, query = endpoint[:i], endpoint[i:]
	}
	switch {
	case strings.HasSuffix(pathPart, geminiStreamAction):
		// 路由本来就写的流式动作：只补 alt=sse。
	case strings.HasSuffix(pathPart, geminiGenerateAction):
		pathPart = strings.TrimSuffix(pathPart, geminiGenerateAction) + geminiStreamAction
	}
	if strings.Contains(query, "alt=sse") {
		return pathPart + query
	}
	sep := "?"
	if query != "" {
		sep = "&"
	}
	return pathPart + query + sep + "alt=sse"
}

// setAuth 按协议给上游请求配好凭据头：gemini 用 x-goog-api-key，anthropic 用 x-api-key
// 加 anthropic-version，两条 openai 协议用 Bearer。凭据头按协议而不是按厂商选
// （同一家上游的不同协议也可能用不同写法，见 DESIGN 5.8）。
func setAuth(req *http.Request, protocol, key string) {
	switch protocol {
	case ProtocolAnthropic:
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case ProtocolGemini:
		req.Header.Set("x-goog-api-key", key)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func readErrorBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 8<<10))
	return errorMessageFromBody(b)
}

// errorMessageFromBody 从上游错误体里取出真正的人话。
// 有的网关把上游错误再包一层 JSON 塞进 error.message，这里解开最多三层。
func errorMessageFromBody(b []byte) string {
	raw := bytes.TrimSpace(b)
	if len(raw) == 0 {
		return "上游没有返回响应体"
	}

	msg := raw
	for depth := 0; depth < 3; depth++ {
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			break
		}
		inner := env.Error.Message
		if inner == "" {
			inner = env.Message
		}
		inner = strings.TrimSpace(inner)
		if inner == "" {
			break
		}
		msg = []byte(inner)
		if !strings.HasPrefix(inner, "{") {
			break
		}
	}

	out := string(bytes.TrimSpace(msg))
	if len(out) > 300 {
		out = out[:300]
	}
	return out
}

func errorKind(status int) string {
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	default:
		return "api_error"
	}
}

func mapErrorStatus(status int) int {
	switch {
	case status == http.StatusBadRequest:
		return http.StatusBadRequest
	case status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case status < 400 || status > 599:
		return http.StatusBadGateway
	default:
		return status
	}
}

// writeError 写出统一形状的错误体：顶层 type 是 error，error.type 与 error.message 给类型与
// 文案。anthropic 入站就是这条形状，没有入站协议可依的路径（/v1/models、/v1/models/{id}、
// 未知路径、405、写库失败一类的内部错误）也用它——那些请求没有「客户端在等哪种形状」可谈。
func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}

// writeInboundError 按入站协议写出错误体（DESIGN 5.2）：四条入站路径的客户端读的字段不同，
// 形状由入站协议决定，不由上游协议决定。入站协议为空或认不出时退回统一形状。
//
//   - anthropic：规定形状，与 writeError 一致；
//   - openai-chat 与 openai-responses：顶层只有 error，type 用 OpenAI 的取值，param 与 code
//     显式给 null（SDK 会读这两个键在不在）；
//   - gemini：code 是 HTTP 状态码的整数，status 是 google 的状态名。
//
// message 由调用方给：上游原话在 tryUpstream 里已经过 scrubSecret（DESIGN 5.6）。
func writeInboundError(w http.ResponseWriter, inbound string, status int, kind, message string) {
	switch inbound {
	case ProtocolChat, ProtocolResponses:
		writeJSON(w, status, map[string]any{"error": map[string]any{
			"message": message,
			"type":    openAIErrorType(kind),
			"param":   nil,
			"code":    nil,
		}})
	case ProtocolGemini:
		writeJSON(w, status, map[string]any{"error": map[string]any{
			"code":    status,
			"message": message,
			"status":  googleErrorStatus(status),
		}})
	default:
		writeError(w, status, kind, message)
	}
}

// openAIErrorType 把内部的语义 kind 换成两条 openai 入站用的取值：客户端按 type 分支处理，
// rate_limit_exceeded 与 server_error 是 OpenAI 的写法，anthropic 的 rate_limit_error 与
// api_error 不是。其余取值两边同名，原样带出。
func openAIErrorType(kind string) string {
	switch kind {
	case "rate_limit_error":
		return "rate_limit_exceeded"
	case "api_error":
		return "server_error"
	}
	return kind
}

// googleErrorStatus 把 HTTP 状态码换成 google 的状态名（DESIGN 5.2）。映射之外的码给 UNKNOWN：
// 编一个更具体却不对的名字会让客户端按错的类别重试或放弃。
func googleErrorStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusMethodNotAllowed:
		return "UNIMPLEMENTED"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusInternalServerError:
		return "INTERNAL"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	return "UNKNOWN"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"type":"error","error":{"type":"api_error","message":"内部错误"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// scrubSecret 把上游回显到错误里的凭据替换掉，避免写进请求记录与命令行输出。
func scrubSecret(msg, key string) string {
	if len(key) < 8 {
		return msg
	}
	return strings.ReplaceAll(msg, key, "***")
}

func errorSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return " " + msg
}
