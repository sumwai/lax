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

	"lax/internal/account"
	"lax/internal/secret"
	"lax/internal/store"
	"lax/internal/translate"
)

// 协议常量。入站按路径区分，出站按该模型的有效协议：先看目录里这个模型声明过的协议，
// 没有再退回账号所属厂商的有效默认协议（优先级只写在 store.EffectiveProtocol 一处）。
// gemini 有自己的原生入站路径，因此入站与出站都能用它。
const (
	ProtocolAnthropic = translate.ProtocolAnthropic
	ProtocolChat      = translate.ProtocolChat
	ProtocolResponses = translate.ProtocolResponses
	ProtocolGemini    = translate.ProtocolGemini
	// ProtocolSystemOne 是第五条线协议：一个 state 加一组 questions，不出文本、没有流式。
	// 它只与自己互通：入站是 systemone 才能出站 systemone，其它协议进不来也出不去，
	// 因为它没有对话消息序列，转不成中间表示。
	ProtocolSystemOne = translate.ProtocolSystemOne
	// ProtocolEmbeddings 是第六条线协议：只做同协议直通，没有流式，也不进中间表示。
	// 向量化报文只有输入与向量结果，没有对话消息序列，与任何对话协议都换不过去。
	ProtocolEmbeddings = translate.ProtocolEmbeddings
	// ProtocolSpeech 是第七条线协议（文本转语音）：入站是 JSON、响应是音频字节，
	// 只做同协议直通。
	ProtocolSpeech = translate.ProtocolSpeech
	// ProtocolTranscriptions 是第八条线协议（语音转文本）：入站是 multipart/form-data、
	// 响应是 JSON 或 SSE，同样只做同协议直通。
	ProtocolTranscriptions = translate.ProtocolTranscriptions

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
	// 次序排好的候选（nil 表示没有意见）。不注入时用该厂商在 provider_routes 里声明过的协议，
	// 厂商一个路由都没声明时退回 anthropic、openai-chat、openai-responses 三种全试。
	// 厂商默认协议与库里已发现的协议由 routeProtocolOrder 排在候选前面。
	// 取值必须是线协议，出现别的值直接报错：编码器遇见未知协议会报错，静默落进 chat 分支更糟，
	// 那会把一条配置错误变成对错误地址的上游调用。
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

// Handler 返回 http 处理器。五条固定入站路径（anthropic、openai-chat、openai-responses、
// systemone、embeddings）与 gemini 的模型名前缀都按路径定协议；另有两个只读的模型清单接口
// （GET /v1/models 与 GET /v1/models/{id}）给客户端发现模型名用。`/v1/models/{id}` 用前缀注册，
// 其后全部算模型名，含 `/` 的模型名也照取。其余路径由 mux 返回 404。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", systemOnePath, embeddingsPath, speechPath, transcriptionsPath} {
		mux.HandleFunc(path, s.handleInbound)
	}
	// gemini 的模型名在路径里（/v1beta/models/{model}:generateContent），模型名的长度与内容都
	// 不固定，没法逐条注册，所以按前缀注册，模型名与流式标志交给 translate.GeminiPathTarget 解析。
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
		// 协议清单只从 store.WireProtocols 取：新加一条线协议时这里自动跟上，
		// 不让一份手写清单变成第二个事实来源。
		"inbound":  store.WireProtocols(),
		"outbound": store.WireProtocols(),
		"passthru": "同协议直通，跨协议经中间表示转换（向量化、systemone 与语音只做同协议直通）",
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
	// 两个都只用于入账判定 store.ReasoningExtra：total 大于输入加输出时才说明思考是输出之外的
	// 一项，那一部分才再按输出价计一次。转发路径不读它们。
	reasoning int64
	total     int64
	wrote     bool
	message   string
	err       error
	// upstreamBody 是上游错误响应的原始正文，只在入站是 systemone 且正文里不含凭据时保留：
	// 这条协议的客户端直接读上游自己的错误形状（例如一条 422 的 detail 数组），
	// 包成我们的 detail 对象反而会让它读不懂：它读的是上游自己那一层 detail。
	upstreamBody []byte
	// ttsChars 与 audioSeconds 是语音两条协议的计量：TTS 的字符数在入站时数好，
	// ASR 的秒数从响应体读出。其余协议两者恒为 0。
	ttsChars     int64
	audioSeconds int64
	// transport 表示这一次没有拿到任何 HTTP 响应：连接没建立、请求没发出去、响应体没读完。
	// status 对这类失败仍记 502（表示「网关没能拿到上游响应」），冷却却按连接层那一档算
	// （2 秒起，上限同样是 30 分钟）：这类失败多半来自本机网络或中转抖动，按上游 5xx 的十秒
	// 起算的话，断网恢复后账号还会被多锁一会儿。已经向客户端写出字节之后的断流不在此列，
	// 见 handleInbound 的冷却分支。
	transport bool
}

// upstreamErrorBodyMax 是一次上游错误的正文读取上限。超过它的正文不原样透传：
// 半截 JSON 比一句摘要更难用，仍然按解出来的那一句话报错。
const upstreamErrorBodyMax = 64 << 10

// cooldownKind 把一次失败的来源折成冷却档位：没拿到 HTTP 响应的是连接层，拿到了状态的按
// 状态码分限流与服务端失败。转发与探活两个入口都只经这一个函数定档，不各自算：
// 两处定档不一致时，同一个失败会在探活与转发上折出两个冷却时长，账号可能在一边已解冻、
// 另一边还锁着。
func cooldownKind(status int, noResponse bool) account.FailureKind {
	if noResponse {
		return account.KindTransport
	}
	return account.KindFromStatus(status)
}

func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	inbound, ok := inboundProtocol(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found_error", "未知路径 "+r.URL.Path)
		return
	}
	// gemini 的报文里没有模型名（它在路径上），流式也由路径后缀表示而非请求体里的 stream
	// 字段，两个值都要按路径注入。用 EscapedPath 取值，模型名里的 %2F 因此仍是一个路径段，
	// PathUnescape 能还原成 org/gemini；用 Path 取值会先把 %2F 解开，模型名被拆成两段。
	// 路径后缀是 :embedContent 时这条请求是向量化而不是对话：动作决定往下走哪条分支，
	// 模型名与是否对话仍按路径注入。
	var pathModel string
	var pathStream bool
	var embedding bool
	voice := inbound == ProtocolSpeech || inbound == ProtocolTranscriptions
	switch {
	case inbound == ProtocolGemini:
		model, action, ok := translate.GeminiPathAction(r.URL.EscapedPath())
		if !ok {
			writeError(w, http.StatusNotFound, "not_found_error", "未知路径 "+r.URL.Path)
			return
		}
		pathModel, pathStream = model, action == translate.GeminiActionStream
		embedding = action == translate.GeminiActionEmbed
	case inbound == ProtocolEmbeddings:
		embedding = true
	}
	if r.Method != http.MethodPost {
		// 405 带上 `Allow: POST`，与两个只读清单接口的 `Allow: GET` 对称：
		// 客户端据此能判断是方法用错了，而不是路径不存在。不带这个头时 405 与 404 在客户端
		// 看来只差一个数字，排查时容易先怀疑路径拼错。
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
			"这条入站路径只接受 POST；对话请求用 POST 发到 "+r.URL.Path+"，查模型清单用 GET /v1/models")
		return
	}

	// 从这里开始入站协议已经定下来，错误体按它给形状（writeInboundError）。
	// 请求体上限按协议取：systemone 是 8 MiB（决策请求只带一个 state 与若干问题，比对话请求
	// 小得多），其余四条是 32 MiB。
	// 多读一个字节是为了在 systemone 上认出「超了」而不是静默截断成一条解析失败。
	limit := int64(32 << 20)
	if inbound == ProtocolSystemOne {
		limit = systemOneMaxBody
	}
	if inbound == ProtocolSpeech || inbound == ProtocolTranscriptions {
		// 语音的请求体上限跟对话一样是 32 MiB：音频文件按 OpenAI 自己的上限是 25 MB。
		limit = speechMaxBody
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}
	if inbound == ProtocolSystemOne && int64(len(raw)) > systemOneMaxBody {
		writeInboundError(w, inbound, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("请求体超过 %d 字节的上限：systemone 的请求体上限是 8 MiB（32 MiB 与 64 层嵌套这些规则在上游），"+
				"把请求体里的 assets 压小一些再发", systemOneMaxBody))
		return
	}

	var req *translate.Request
	// speechChars 是 TTS 的计费字符数：入站时数好，不靠响应体（这条端点不报 token）。
	var speechChars int64
	// ttsReq / asrReq 留着给能力校验与 chat 适配器用（同一份解析结果，不重复解）。
	var ttsReq *speechRequest
	var asrReq *transcriptionsRequest
	switch {
	case inbound == ProtocolSystemOne:
		// systemone 没有编解码器，也不进中间表示：它没有对话消息序列，转不成 IR。
		// 因此本机只做选号与拒绝不支持的写法所必需的三件事：
		//   - model 必须是非空字符串（按它选账号）；
		//   - questions 必须是非空对象（一个问题的决策请求没有意义）；
		//   - stream 不能是布尔 true（这条协议没有流式，静默当同步会让客户端等到超时）。
		// 其余字段（state、questions 的内容、assets）不解析、不改写，原样交给上游校验。
		req, err = decodeSystemOneRequest(raw)
		if err != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "systemone: "+err.Error())
			return
		}
	case inbound == ProtocolEmbeddings:
		// 向量化与 systemone 一样没有编解码器，也不进中间表示：向量化报文没有对话消息序列可转换。
		// 本机只做选号与拒绝空请求所必需的事：
		//   - model 必须是非空字符串（按它选账号）；
		//   - input 必须存在（没有输入就没有可向量化的内容）；
		//   - stream 不能是布尔 true（这条协议没有流式，静默当同步会让客户端等到超时）。
		// 其余字段不解析、不改写，原样交给上游校验。
		req, err = decodeEmbeddingsRequest(raw)
		if err != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "向量化: "+err.Error())
			return
		}
	case embedding:
		// gemini 的 :embedContent 读的是 content，不是对话的 contents，因此不并到下面那一支。
		req, err = decodeGeminiEmbedRequest(raw)
		if err != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "gemini 向量化: "+err.Error())
			return
		}
	case inbound == ProtocolSpeech:
		// 语音两条协议也没有编解码器：报文不是对话消息序列。本机只做选号与拒绝空请求所需的事。
		parsed, err2 := decodeSpeechRequest(raw)
		if err2 != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "语音合成: "+err2.Error())
			return
		}
		req = &translate.Request{Model: parsed.Model}
		speechChars = parsed.Chars
		ttsReq = parsed
	case inbound == ProtocolTranscriptions:
		parsed, err2 := decodeTranscriptionsRequest(raw, r.Header.Get("Content-Type"))
		if err2 != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "语音转写: "+err2.Error())
			return
		}
		req = &translate.Request{Model: parsed.Model}
		asrReq = parsed
	default:
		req, err = translate.DecodeRequest(inbound, raw)
		if err != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", inbound+": "+err.Error())
			return
		}
	}
	if inbound == ProtocolGemini {
		req.Model = pathModel
		req.Stream = pathStream
	}
	if strings.TrimSpace(req.Model) == "" {
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}

	// 别名解析只做一次：客户端给的名字原样记进 model_requested，此后选号、路由的 {model}
	// 占位符、请求体里的 model 与 gemini 路径全部用解析后的真实名字。响应里的模型名不改，
	// 别名只影响请求方向。走链、成环与超长的判定只有 store.ResolveModelAlias 一份，
	// 这里不自己按跳查表。
	requested := req.Model
	if resolved, isAlias, err := s.store.ResolveModelAlias(requested); err != nil {
		// 链成环或太长：这不是「清单里没有这个模型」，而是这个名字眼下解不出真实名字，
		// 换哪个账号都发不出去，按请求本身有问题报 400。
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error",
			aliasBrokenMessage(requested, err))
		return
	} else if isAlias {
		req.Model = resolved
		s.logf("模型名 %s 按别名解析为 %s", requested, req.Model)
	}

	session := requestSession(r, raw)

	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeInboundError(w, inbound, http.StatusInternalServerError, "api_error", "读取账号失败: "+err.Error())
		return
	}

	// 语音入站先查能力声明：没有声明的模型（例如一个普通对话模型）在这里就被拒，
	// 不发上游请求。这一步在选号之前：它回答的是「这个端点能不能用这个模型」，
	// 与账号冷却、优先权无关。
	if inbound == ProtocolSpeech || inbound == ProtocolTranscriptions {
		if err := s.checkSpeechInbound(inbound, accounts, req.Model, ttsReq, asrReq); err != nil {
			writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
	}
	candidates := account.Candidates(accounts, req.Model, s.now(), session)
	if len(candidates) == 0 {
		// 候选为空有两种成因，报错不能混在一起：这个模型没有落在任何账号的
		// `account_models.access='allowed'` 上（清单问题），或者有账号可达但全部在冷却或被停用
		// （可用性问题，仍按 503 走）。可达性是账号侧唯一的事实，账号行没有第二份清单可对。
		if len(account.Providers(accounts, req.Model)) == 0 {
			writeInboundError(w, inbound, http.StatusNotFound, modelNotFoundKind(inbound),
				modelNotFoundMessage(requested, req.Model))
			return
		}
		writeInboundError(w, inbound, http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("模型 %s 的账号都在冷却或被停用，先用 lax account ls 看状态", req.Model))
		return
	}

	lastErr := "没有可用账号"
	// interopRejected 记第一次「入站协议与有效出站协议不相通」的文案，onlyInterop 表示每个候选都停在这一条上：
	// 没有谁因为账号冷却、被停用或连接失败等别的理由被跳过，也没有一次上游调用真的发出去过。
	// 这两种情况给客户端的答案不同：请求本身与这条协议不相通是 400，换账号重试也没用；
	// 所有账号都失败才是 503，换个时间才有意义。混成 503 会让客户端反复重试一个永远失败的写法。
	interopRejected := ""
	onlyInterop := true
	for _, cand := range candidates {
		if r.Context().Err() != nil {
			return
		}

		// 有效出站协议只有一处算：store.EffectiveProtocol 按「账号覆盖 → 目录里这个模型支持的协议 →
		// 厂商的有效默认协议」定优先级。账号行不存协议，cand.Protocol 只是厂商默认协议的展示值。
		// 在这里另算一遍优先级，两处迟早对不上，转发会打到一条账号根本没用过的路由上。
		// 有效价格与上下文长度同理只从 store.EffectiveModels 取，网关不重算优先级。
		// 向量化是例外：它只能同协议直通，协议由厂商声明的那一条向量化路由决定，
		// 不按模型的有效协议猜——对话协议的有效协议与向量化的线协议不是同一回事。
		var outbound string
		switch {
		case embedding:
			var msg string
			outbound, msg = s.embeddingOutbound(cand, inbound)
			if outbound == "" {
				// 厂商没声明路由：这不是这个账号这一次打不通（不落冷却、不写请求记录），
				// 也换不了号解决，与协议不相通一样记下来当 400。
				interopRejected = msg
				lastErr = msg
				s.logf("账号 %s：%s", cand.Name, msg)
				continue
			}
		case voice:
			// 语音与向量化不同：它有两种出站。厂商声明了语音路由就直通，
			// 没声明但模型声明了能力且走 chat 的就经适配器（见 speechOutbound）。
			var msg string
			outbound, msg = s.speechOutbound(cand, inbound, req.Model)
			if outbound == "" {
				interopRejected = msg
				lastErr = msg
				s.logf("账号 %s：%s", cand.Name, msg)
				continue
			}
		default:
			var err error
			outbound, err = s.store.EffectiveProtocol(cand.ID, req.Model)
			if err != nil {
				// 读库失败不猜协议：猜出来的协议会打到这家没声明的路径上，报错比打错地址好。
				s.logf("读账号 %s 模型 %s 的有效协议失败: %v", cand.Name, req.Model, err)
				lastErr = fmt.Sprintf("账号 %s：读有效出站协议失败（%v），先运行 lax doctor 检查数据库",
					cand.Name, err)
				onlyInterop = false
				continue
			}
			if outbound == "" {
				// 算不出协议说明配置缺一条路由，不是这个账号这一次打不通：不落冷却，
				// 只换下一个候选继续试（别的账号可能配好了），文案给下一步命令。
				lastErr = s.noProtocolMessage(cand, req.Model)
				s.logf("账号 %s 的模型 %s 算不出有效出站协议", cand.Name, req.Model)
				onlyInterop = false
				continue
			}
		}

		if msg := protocolInteropError(inbound, outbound, req.Model); msg != "" {
			// 协议不相通与账号无关：不落冷却、不写请求记录（一次上游调用都没发），
			// 只记下文案并换下一个候选看一眼它算出的协议（不同账号可能算出不同协议）。
			interopRejected = msg
			s.logf("账号 %s 的模型 %s 走 %s 协议，与入站协议 %s 不相通", cand.Name, req.Model, outbound, inbound)
			continue
		}
		onlyInterop = false

		attemptStart := s.now()
		res := s.tryUpstream(r.Context(), w, cand, outbound, inbound, r.Header, raw, req, session, embedding)
		latency := s.now().Sub(attemptStart)

		// 两列正好分工：model_requested 记客户端给的名字（可能是别名），
		// model_used 记真正发给上游的真实名字。只用一列的话，聚合用量时要么看不出客户端用了
		// 哪个别名，要么看不出实际打的是哪个模型。
		rec := &store.RequestRecord{
			TS:             attemptStart.Unix(),
			Inbound:        inbound,
			Outbound:       outbound,
			ModelRequested: requested,
			ModelUsed:      req.Model,
			AccountID:      cand.ID,
			HTTPStatus:     res.status,
			OK:             res.err == nil && res.status >= 200 && res.status < 300,
			InTokens:       res.inTok,
			// 入账时按出站协议归一化输入 token：anthropic 的 input_tokens 已排除缓存读写，
			// chat、responses 与 gemini 的上游把命中缓存的部分也算在输入里，花费只能按归一化后的
			// 这一列折，否则同一笔输入会被输入价与缓存价各算一次。in_tokens 仍是上游原值，
			// 展示不变。
			InTokensUncached: store.UncachedInTokens(outbound, res.inTok, res.cacheRead),
			OutTokens:        res.outTok,
			CacheRead:        res.cacheRead,
			CacheWrite:       res.cacheWrite,
			// 思考 token 原值整列入账，是否再按输出价计一次交给 ReasoningExtra：
			// 判定只写在那一处，不在这里按协议猜：各家对「思考算不算输出」的做法不同，
			// 只能按上游上报的数字判。
			ReasoningTokens:      res.reasoning,
			ReasoningTokensExtra: store.ReasoningExtra(res.total, res.inTok, res.outTok, res.reasoning),
			TTSChars:             speechChars,
			AudioSeconds:         res.audioSeconds,
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
			if inbound == ProtocolSystemOne && len(res.upstreamBody) > 0 {
				// 上游自己的错误正文原样透传：状态码与正文都照旧，不重新包装，也不把 422 的
				// detail 数组转成对象——这条协议的客户端读的就是上游那一层。请求记录与日志里用的
				// 仍是解出来并脱敏过的那一句话，不是这份原文。
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(res.status)
				_, _ = w.Write(res.upstreamBody)
				return
			}
			writeInboundError(w, inbound, mapErrorStatus(res.status), errorKind(res.status), res.message)
			return
		}
	}

	if onlyInterop && interopRejected != "" {
		writeInboundError(w, inbound, http.StatusBadRequest, "invalid_request_error", interopRejected)
		return
	}
	writeInboundError(w, inbound, http.StatusServiceUnavailable, "api_error", "所有候选账号都失败："+lastErr)
}

// modelNotFoundKind 给出「没有账号提供该模型」在入站协议里的错误类型：anthropic 用它自己的
// not_found_error，两条 openai 路径用 invalid_request_error（都没有专门的「模型未配置」类型）。
func modelNotFoundKind(inbound string) string {
	// systemone 的错误类型取值与 anthropic 同名，「清单里没有这个模型」
	// 在那一套里就是 not_found_error。
	if inbound == ProtocolAnthropic || inbound == ProtocolSystemOne {
		return "not_found_error"
	}
	return "invalid_request_error"
}

// modelNotFoundMessage 说明找不到模型，并给出下一步的两条命令：先看现有清单，再探活。
// requested 是客户端给的名字，used 是解析后的真实名字（没有别名时两者相同）。别名命中时
// 必须把两个名字都写出来：只说「没有账号提供 X」会让人去清单里找一个永远不存在的名字。
func modelNotFoundMessage(requested, used string) string {
	if requested != used {
		return fmt.Sprintf("别名 %s 指向 %s，但没有账号提供 %s；用 lax model ls 看现有清单",
			requested, used, used)
	}
	return fmt.Sprintf("没有账号提供模型 %s；先运行 lax model ls 看现有清单，"+
		"再用 lax model probe 探活", used)
}

// aliasBrokenMessage 说明别名链解不出真实名字（成环或太长）。store 的原文里带着整条链，
// 这里只补上「下一步做什么」：改掉链上的一条，或先看一眼现有别名。
func aliasBrokenMessage(name string, err error) string {
	return fmt.Sprintf("模型名 %s 解析别名失败：%v；用 lax model alias ls 看别名链，"+
		"改掉或删掉其中一条，客户端也可以直接写真实模型名", name, err)
}

// requestSession 取本次入站请求的会话键：`x-lax-session` 头优先，
// 其次请求体里的 metadata.user_id。选号粘性与附加头的 {session} 都走这一个函数，不写第二份——
// 两处取到不同的值会让「同一个对话」在选号与上游看来不是同一个会话。
func requestSession(r *http.Request, raw []byte) string {
	if v := r.Header.Get("x-lax-session"); v != "" {
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
	inbound string, inboundHeaders http.Header, raw []byte, req *translate.Request, session string,
	embedding bool) attempt {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return attempt{status: http.StatusInternalServerError, message: "解密账号凭据失败"}
	}

	upReq, err := s.buildRequest(ctx, a, outbound, inbound, inboundHeaders, raw, req, string(key), session, embedding)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyMax+1))
		whole := len(body) <= upstreamErrorBodyMax
		if !whole {
			body = body[:upstreamErrorBodyMax]
		}
		res := attempt{status: resp.StatusCode, message: scrubSecret(errorMessageFromBody(body), string(key))}
		// 入站是 systemone 时把上游错误正文也留着原样透传，两个前提：正文读全了，且里面没有
		// 我们的凭据（原样转发不得把上游回显的 key 带给客户端；那种情况退回我们的 detail 对象，
		// 那份正文已经过 scrubSecret）。
		if inbound == ProtocolSystemOne && whole && len(body) > 0 &&
			!(len(key) >= 8 && bytes.Contains(body, key)) {
			res.upstreamBody = body
		}
		return res
	}

	if speechNeedsChatAdapter(inbound, outbound) {
		// 语音入站 + chat 出站：把 chat completion 解成音频字节或转写结果。
		return s.chatSpeechSuccess(w, resp, inbound, raw, inboundHeaders.Get("Content-Type"))
	}
	if outbound == ProtocolSpeech || outbound == ProtocolTranscriptions {
		// 语音的响应要么是音频字节、要么是 JSON 或 SSE，都不走 jsonSuccess/streamSuccess：
		// 那两个按对话协议解析用量与帧，读一条音频只会得到零值或报错。
		return s.speechSuccess(w, resp, outbound, raw)
	}
	if req.Stream {
		return s.streamSuccess(ctx, w, resp, inbound, outbound, req, string(key))
	}
	return s.jsonSuccess(w, resp, inbound, outbound, req)
}

// buildRequest 按有效出站协议构造上游请求：路径、凭据头、附加头与请求体编码都跟它走。
// 入站协议与它相同时直通（只改 model），不同时经中间表示转换。
// inboundHeaders 是入站请求头，只有转发路径才给（模型同步与探活没有客户端请求，传 nil），
// 白名单里的头原样透传：客户端自己带的 trace id、地域限制等参数由它传到上游，
// 网关不维护第二份意图表。
// session 是本次请求的会话键（必要时是空串），供附加头的 {session} 用；模型名取 req.Model。
func (s *Server) buildRequest(ctx context.Context, a store.Account, outbound, inbound string,
	inboundHeaders http.Header, raw []byte,
	req *translate.Request, key, session string, embedding bool) (*http.Request, error) {

	// 路由的 path 里可以有 {model} 占位符（gemini 的模型名在路径里）：先拼好地址再替换，
	// 替换后才是真正要访问的 URL。gemini 的流式另走 :streamGenerateContent 与 alt=sse
	// （见 geminiStreamEndpoint）。
	// 拼不出地址时直接报错：拿一个空 base_url 猜出来的路径只会换回「unsupported protocol scheme」。
	upstream, err := s.endpoint(a, outbound)
	if err != nil {
		return nil, err
	}
	endpoint := modelEndpoint(upstream, req.Model)
	if outbound == ProtocolGemini && req.Stream {
		endpoint = geminiStreamEndpoint(endpoint)
	}
	if embedding && outbound == ProtocolGemini {
		// gemini 的向量化是另一个接口动作（:embedContent），路由里存的是对话那个后缀，
		// 这里只换后缀、不动查询串与模型名：换掉整条路径会连路由里配好的版本前缀一起丢掉。
		endpoint = geminiEmbedEndpoint(endpoint)
	}

	var body []byte
	// contentType 默认 JSON；语音转写要改成带新 boundary 的 multipart。
	contentType := "application/json"
	switch {
	case outbound == ProtocolTranscriptions && inboundHeaders != nil:
		// 语音转写换 model 那个 part：不能走 patchModel（请求体不是 JSON），
		// 也不能直接发 raw（model 还是客户端的名字，别名没解析）。
		// 探活没有客户端请求（inboundHeaders 为 nil），正文已经是带着自己的 boundary 的探测
		// 报文，下面那一支原样发。
		body, contentType, err = rewriteTranscriptionsBody(raw, inboundHeaders.Get("Content-Type"), req.Model)
	case outbound == ProtocolTranscriptions:
		body = raw
	case speechNeedsChatAdapter(inbound, outbound):
		// 语音入站 + chat 出站：上游把语音做成 chat 方言，请求要重新编。
		// 能力校验已经在上游调用之前做过（见 checkSpeechInbound）。
		body, err = s.encodeSpeechAsChat(inbound, raw, req.Model, inboundHeaders.Get("Content-Type"))
	case outbound == ProtocolSpeech:
		// 语音合成是 JSON，与其余直通协议一样只换 model。
		body, err = patchModel(raw, req.Model)
	case inbound != outbound:
		body, err = translate.EncodeRequest(outbound, req, req.Model)
	case outbound == ProtocolGemini:
		// gemini 的报文里没有 model 字段（模型名在路径上），所以直通时请求体一个字节都不改：
		// 往里面塞一个 model 只会让上游以「未知字段」拒掉整条请求，而模型名已经由路径带过去了。
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
	httpReq.Header.Set("Content-Type", contentType)
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	setAuth(httpReq, outbound, key)
	// 客户端白名单头排在 setAuth 之后、附加头之前：头名冲突的优先级由这个顺序决定，不另写判断。
	// 优先级是 setAuth 给的默认凭据头 < 客户端头 < 厂商附加头声明。
	for _, d := range applyClientHeaders(httpReq, inboundHeaders, inbound, outbound) {
		s.logf("客户端头 %s 被丢弃：值有 %d 字节，超过 %d 字节上限，不往上游透传",
			d.Name, d.Bytes, clientHeaderMaxBytes)
	}
	// 附加头压后叠加：同名时厂商声明覆盖客户端头与 setAuth 给的默认凭据头。
	// 声明在内层循环之后应用，是因为厂商声明的头才代表厂商真正要求的值，
	// 客户端想覆盖它会印出上游 400，不如让声明赢。
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
		switch {
		case outbound == ProtocolSystemOne:
			// 决策协议没有编解码器（它不进中间表示），用量两个字段直读；响应体不改写。
			res.inTok, res.outTok = systemOneUsage(body)
		case outbound == ProtocolEmbeddings:
			// 向量化也没有编解码器，只解析用量：这条报文只有 prompt_tokens 一个计数，没有输出 token，
			// 按对话响应的形状去解只会拿到零值，这次调用的花费因此整块漏掉。
			res.inTok = embeddingsUsage(body)
		default:
			if parsed, err := translate.DecodeResponse(outbound, body, req.Model); err == nil {
				res.inTok, res.outTok = parsed.Usage.In, parsed.Usage.Out
				res.cacheRead, res.cacheWrite = parsed.Usage.CacheRead, parsed.Usage.CacheWrite
				res.reasoning, res.total = parsed.Usage.Reasoning, parsed.Usage.Total
			}
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
		// 客户端：编一个不存在的取值会让客户端拿到它不认识的枚举值，原值只进日志当备注。
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

func (s *Server) streamSuccess(ctx context.Context, w http.ResponseWriter, resp *http.Response,
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
		return s.passthroughStream(ctx, w, resp, outbound, flush, secretValue)
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
	// gemini 的 {"error":{…}} 是一条新增入口，上游把凭据回显在 message 里时不能落库、
	// 也不能再回给客户端。
	// 四个出站协议的错误增量都走这一个分支，脱敏只在这一处做。
	var upstreamErr string
	// finished 记「收尾事件已经写到客户端了」。客户端拿到收尾之后自己断开（codex 收到
	// response.completed 就关连接）与断流是两回事：这一次请求对客户端已经完成，
	// 按失败处理会把一次成功的调用记成错误，并给它落一份没人需要的冷却。
	var finished bool
	// gone 把「客户端不在了」翻译成记账结论：收尾前断开是没送完的请求，收尾后断开不算失败。
	gone := func() attempt {
		if finished {
			return attempt{status: http.StatusOK, wrote: true,
				inTok: usage.In, outTok: usage.Out, cacheRead: usage.CacheRead,
				cacheWrite: usage.CacheWrite, reasoning: usage.Reasoning, total: usage.Total}
		}
		return attempt{status: http.StatusOK, wrote: true, message: clientGoneMessage, err: errClientGone}
	}
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
			if d.Kind == translate.DeltaStop {
				finished = true
			}
		}
		return nil
	}

	if err := write(enc.Start()); err != nil {
		return gone()
	}

	// ended 记「上游把收尾帧发过来了」：收尾帧没到就是断流，判据与直通路径一致，
	// 不单靠解码器报错——干净 EOF 时解码器一声不响，只看到 EOF 的调用方会把半截流转成正常结束。
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

	// 客户端自己断开时，上游的读也会跟着醒：Go 把请求上下文的取消传到上游连接上，
	// 读错误就是 context canceled。这不是上游的问题：收尾已经发出就是一次完成的请求，
	// 没收尾就是没送完，两种都不写「上游流式响应中断」，否则客户端挂断会被算成上游故障，
	// 账号白白落一份冷却。
	clientGone := ctx.Err() != nil || errors.Is(scanErr, context.Canceled)
	failure := ""
	switch {
	case clientGone && !finished:
		failure = clientGoneMessage
	case clientGone:
		// 收尾已写出，客户端随后挂断：上游剩下的只有用量帧与收尾帧，不记失败。
	case scanErr != nil:
		failure = "上游流式响应中断: " + scanErr.Error()
	case !ended && upstreamErr != "":
		failure = "上游流式响应失败: " + upstreamErr
	case !ended:
		failure = truncatedMessage
	}
	if failure != "" && upstreamErr == "" && !clientGone {
		// 错误增量走编码器，不另写一帧：入站协议是 responses 时它就是 error 加
		// response.failed，是 anthropic 时是 event: error——形状跟入站协议走，与非 2xx 响应用的是
		// 同一份映射，客户端只需要一套错误处理。
		_ = emit([]translate.Delta{{Kind: translate.DeltaError, Text: failure}})
	}
	_ = emit(dec.Close())
	_ = write(enc.Finish())

	res := attempt{status: http.StatusOK, wrote: true, inTok: usage.In, outTok: usage.Out,
		cacheRead: usage.CacheRead, cacheWrite: usage.CacheWrite,
		reasoning: usage.Reasoning, total: usage.Total}
	if failure != "" {
		// 写出之后的断流：status 仍是 200、wrote 为真，handleInbound 因此不冷却也不换号。
		// 它虽然是「读流的时候断了」，但已经向客户端写出了字节，不算可以重试的连接层失败：
		// 换号重试会把客户端已经收到的半截内容再发一遍。
		// 记账用的摘要也过一遍脱敏：scanErr 的原文里可能带着上游回显的地址或凭据。
		res.message = scrubSecret(failure, secretValue)
		res.err = errStreamTruncated
		if scanErr != nil {
			res.err = scanErr
		}
		if clientGone {
			// 客户端不在了与上游断流不是一回事：记账用的错误也不蹭上游那一条。
			res.err = errClientGone
		}
	}
	return res
}

// passthroughStream 原样转发同协议上游的字节，同时读出用量。
//
// 上游没发到收尾帧就断开时，已写出的字节一个不动，只在末尾追加该协议的合法结尾
// （见 passthroughTail）：客户端总能拿到一个收尾完整的流，而不是半截报文。
// 不补收尾的代价是客户端按 SSE 约定把这条流当成连接断了，已经收到的内容可能整段丢弃，
// 用户看到一次“空回复”，而上游实际上已经回了大半。
func (s *Server) passthroughStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, protocol string,
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

	// gone 把「写不出去了」翻译成记账结论：收尾帧已经转发就是完成的请求，否则是没送完。
	gone := func() attempt {
		if ended {
			scanned := scanner.Usage()
			return attempt{status: http.StatusOK, wrote: true,
				inTok: scanned.In, outTok: scanned.Out, cacheRead: scanned.CacheRead,
				cacheWrite: scanned.CacheWrite, reasoning: scanned.Reasoning, total: scanned.Total}
		}
		return attempt{status: http.StatusOK, wrote: true, message: clientGoneMessage, err: errClientGone}
	}

	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return gone()
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
	// 客户端断开时上游的读会以 context canceled 醒来，与转换路径同一个判据：
	// 收尾帧已经转发就是完整的流，没收尾只是没送完，两者都不是上游失败。
	clientGone := ctx.Err() != nil || errors.Is(readErr, context.Canceled)
	switch {
	case readErr != nil && clientGone && !ended:
		res.message = clientGoneMessage
		res.err = errClientGone
	case readErr != nil && !clientGone:
		res.message = "上游流式响应中断: " + readErr.Error()
		res.err = readErr
	}
	if !ended && !clientGone {
		if res.err == nil {
			// 干净的 EOF 也可能断在半路：收尾帧没到就是断流，与转换路径同一个判据。
			res.message = truncatedMessage
			res.err = errStreamTruncated
		}
		// 错误帧的正文既发给客户端也写进 requests，与转换路径一样先过 scrubSecret：
		// 读错误里可能带上游回显的地址或凭据，这类内容一旦落库就再也取不掉。
		res.message = scrubSecret(res.message, secretValue)
		appendStreamTail(w, flush, tail, passthroughTail(protocol, res.message, in, out))
	}
	return res
}

// errStreamTruncated 表示上游在收尾帧之前就结束了连接（干净的 EOF，读不出别的错）。
var errStreamTruncated = errors.New("上游流式响应在收尾帧之前结束")

// errClientGone 表示客户端自己关了连接、上游的读跟着被撤销。它不是上游失败：
// 收尾前断开记成失败的请求，收尾后断开直接算完成。当成上游失败会给一个只是提前退出的
// 客户端落一份账号冷却，而那个账号什么错都没犯。
var errClientGone = errors.New("客户端提前断开")

// clientGoneMessage 是客户端提前断开时写进 requests.error 的说明。
const clientGoneMessage = "客户端提前断开"

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
// 错误帧的形状与四条入站路径的非流式错误一致：openai-responses 的错误体在
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
					"id":     "resp_lax",
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
	case ProtocolEmbeddings:
		// 向量化没有厂商路由时猜 /v1/embeddings：与 OpenAI 的路径一致，ollama 的兼容层同样认它。
		suffix = "/embeddings"
	case ProtocolSystemOne:
		// 决策协议的入站与出站是同一个 POST /v1/systemone，所以猜出来的后缀是 /systemone。
		// 漏了这一格会落进下面的对话路径，请求打到一个厂商没声明过的地址，只能换回 404。
		suffix = "/systemone"
	}
	if strings.HasSuffix(base, "/v1") {
		return base + suffix
	}
	return base + "/v1" + suffix
}

// endpoint 解析账号在某种协议上的上游地址：厂商在 provider_routes 里声明过这条协议就用它的取值
// （相对路径拼 base_url、整条地址直接用，见 joinEndpoint）；没有厂商行、或这家没有该协议的路由时
// 退回后缀猜测。
//
// 地址只有一份，在厂商行上（`a.BaseURL` 是 providers.base_url 的派生值）。后缀猜测要 base_url
// 才能拼，拿不到时不是猜出一个畸形地址去打，而是报错并给出下一步命令：历史孤儿行（厂商行已不存在）
// 得先 provider add 重建，厂商在但没有这条路由得先 provider set 声明它。
func (s *Server) endpoint(a store.Account, protocol string) (string, error) {
	route, ok, err := s.store.LookupProviderRoute(a.Provider, protocol)
	if err != nil {
		s.logf("读厂商 %s 的 %s 路由失败，按 base-url 猜路径: %v", a.Provider, protocol, err)
	} else if ok && strings.TrimSpace(route.Path) != "" {
		return joinEndpoint(a.BaseURL, route.Path), nil
	}
	if a.BaseURL == "" {
		return "", s.noAddressError(a, protocol)
	}
	return Endpoint(a.BaseURL, protocol), nil
}

// noAddressError 说明为什么拼不出上游地址，并给出下一步命令。
// 两种情况：账号指向的厂商行不存在（历史孤儿行，地址与路由都随厂商一起没了），
// 与厂商在但没有这条协议的路由、厂商又没有 base_url 可供后缀猜测（路由与 models_path 都是整条地址）。
func (s *Server) noAddressError(a store.Account, protocol string) error {
	if _, err := s.store.GetProvider(a.Provider); errors.Is(err, store.ErrProviderNotFound) {
		return fmt.Errorf("账号 %s 的厂商 %q 在 providers 里不存在，地址只能从厂商行取：%s",
			a.Name, a.Provider, providerAddHint(a, protocol))
	}
	return fmt.Errorf("厂商 %s 没有 %s 路由，也没有 base_url 可供后缀猜测：用 lax provider set %s %s 声明这条路由，或在同一条命令里补上 --base-url",
		a.Provider, protocol, a.Provider, providerPathHint(protocol))
}

// providerAddHint 是「厂商行不存在」时的下一步：地址与默认协议都只能从厂商行取，
// 所以要么重建这家厂商，要么把账号改指到已有厂商。两条都写出来，因为重建厂商要用原来的名字与地址，
// 而用户手上未必还有那个地址。
func providerAddHint(a store.Account, protocol string) string {
	return fmt.Sprintf("先运行 lax provider add %s --base-url <地址> %s 重建厂商（用 lax provider ls 看现有厂商），或用 lax account set %s --provider <厂商> 把账号指到已有厂商",
		a.Provider, providerPathHint(protocol), a.Name)
}

// providerPathHint 给出某条协议在 provider 子命令里的写法，用于错误文案里的下一步命令。
// 只是提示，不是第二条校验：真正的合法取值仍由 store 判定。
func providerPathHint(protocol string) string {
	switch protocol {
	case ProtocolAnthropic:
		return "--anthropic /v1/messages"
	case ProtocolResponses:
		return "--responses /v1/responses"
	case ProtocolGemini:
		return "--gemini /v1beta/models/{model}:generateContent"
	case ProtocolSystemOne:
		return "--systemone /v1/systemone"
	default:
		return "--openai /v1/chat/completions"
	}
}

// noProtocolMessage 说明这个账号为什么算不出有效出站协议，并给出下一步命令。
// 没有协议可用有三种成因，下一步各不相同：厂商行不存在（历史孤儿行）得先 provider add；
// 厂商在但没有路由（或默认协议落到空）得先 provider set 声明一条路由；
// 账号没有厂商名得先给账号选一家厂商。不说清是哪一种，用户只会去查模型名。
func (s *Server) noProtocolMessage(a store.Account, model string) string {
	switch {
	case a.Provider == "":
		return fmt.Sprintf("账号 %s 没有厂商，算不出出站协议：先运行 lax account set %s --provider <厂商>（用 lax provider ls 看现有厂商）",
			a.Name, a.Name)
	case s.providerMissing(a.Provider):
		return fmt.Sprintf("账号 %s 的厂商 %q 不存在（历史孤儿行），地址与默认协议都只能从厂商行取：%s",
			a.Name, a.Provider, providerAddHint(a, ProtocolChat))
	default:
		return fmt.Sprintf("账号 %s 在模型 %s 上没有可用的出站协议（厂商 %s 没有路由或默认协议为空）：先运行 lax provider set %s %s 声明一条路由，或 lax model probe --account %s 探出这个模型的协议",
			a.Name, model, a.Provider, a.Provider, providerPathHint(ProtocolChat), a.Name)
	}
}

// providerMissing 判断厂商行是否不存在。读库失败当作存在：那种情况下要报的是读库错误，
// 不是「厂商没了」——把一次数据库故障说成配置缺失，会把人引到错误的下一步。
func (s *Server) providerMissing(provider string) bool {
	if provider == "" {
		return true
	}
	_, err := s.store.GetProvider(provider)
	return errors.Is(err, store.ErrProviderNotFound)
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
// （如 /api/paas/v4）原样保留。规整只碰路径，不动授权部分、查询串与 `#` 之后的内容。
// 其余一律照抄，不推 /v1、不猜后缀：路由里写的路径就是上游的路径。
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
// 模型名在路径里是 gemini 的原生写法，因此路由的 path 支持 {model} 占位符：一条路由能覆盖
// 整家厂商的所有模型，不用每个模型各声明一条。
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
// 同一个模型的流式与非流式因此打到两个不同地址，后面那条会换回 404。
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
// 加 anthropic-version，两条 openai 协议与 systemone 用 Bearer（TypeSafe 本家与 commandcode 的
// 决策端点都读 Authorization）。凭据头按协议而不是按厂商选：同一家上游的不同协议
// 也可能用不同写法，按厂商选就让分类好像同一个厂商的路径全用同一种凭据头。
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

// writeInboundError 按入站协议写出错误体：五条入站路径的客户端读的字段不同，形状由入站协议
// 决定，不由上游协议决定。入站协议为空或认不出时退回统一形状。
//
//   - anthropic：规定形状，与 writeError 一致；
//   - openai-chat 与 openai-responses：顶层只有 error，type 用 OpenAI 的取值，param 与 code
//     显式给 null（SDK 会读这两个键在不在，缺键与 null 在它们眼里不是一回事）；
//   - gemini：code 是 HTTP 状态码的整数，status 是 google 的状态名，两者类型都不能换；
//   - systemone：顶层只有 detail，里面是 message 与 error_type（不是 type）。真实的客户端是
//     TypeSafe 的 SDK，它读这两个字段，用对象而不是纯字符串是为了留下类型又不改 SDK 的读法；
//     error_type 的取值与 anthropic 那套同名，同一种错误在两条协议上的说法因此能对上。
//
// 形状写错时客户端读不出错误：把 anthropic 形状的错误体回给 openai 客户端，SDK 拿到的是
// 一个空的 error 对象，用户看到「未知错误」而不是上游的原话。
//
// message 由调用方给：上游原话在 tryUpstream 里已经过 scrubSecret，凭据不会随错误体回到客户端。
func writeInboundError(w http.ResponseWriter, inbound string, status int, kind, message string) {
	switch inbound {
	case ProtocolChat, ProtocolResponses, ProtocolEmbeddings:
		// 向量化是 OpenAI 的报文，错误体也照那一套写，客户端用的是同一个 SDK。
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
	case ProtocolSystemOne:
		// 字段名是 error_type 而不是 type：TypeSafe SDK 读 detail.error_type 与 detail.message。
		// 用对象而不是纯字符串，好在留下类型的同时不改 SDK 的读法。
		writeJSON(w, status, map[string]any{"detail": map[string]any{
			"message":    message,
			"error_type": kind,
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

// googleErrorStatus 把 HTTP 状态码换成 google 的状态名：google 的客户端按这个字符串分类错误，
// 它不认识数字状态码。映射之外的码给 UNKNOWN：
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
