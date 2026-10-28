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
	"strings"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/secret"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// 协议常量。入站按路径区分，出站按该模型的有效协议（account_models.protocol 优先，
// 否则账号的 protocol）。
const (
	ProtocolAnthropic = translate.ProtocolAnthropic
	ProtocolChat      = translate.ProtocolChat
	ProtocolResponses = translate.ProtocolResponses

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

// Handler 返回 http 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		mux.HandleFunc(path, s.handleInbound)
	}
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
		"inbound":  []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses},
		"outbound": []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses},
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
	wrote      bool
	message    string
	err        error
}

func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	inbound, ok := translate.PathProtocol(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found_error", "未知路径 "+r.URL.Path)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "只接受 POST")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}

	req, err := translate.DecodeRequest(inbound, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", inbound+": "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}

	session := r.Header.Get("x-switchboard-session")
	if session == "" {
		session = sessionFromBody(raw)
	}

	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "读取账号失败: "+err.Error())
		return
	}
	candidates := account.Candidates(accounts, req.Model, s.now(), session)
	if len(candidates) == 0 {
		writeError(w, http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("没有可用于模型 %s 的账号，先运行 switchboard account add 与 model probe", req.Model))
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
		res := s.tryUpstream(r.Context(), w, cand, outbound, inbound, raw, req)
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
			OutTokens:      res.outTok,
			CacheRead:      res.cacheRead,
			CacheWrite:     res.cacheWrite,
			LatencyMS:      latency.Milliseconds(),
			Error:          res.message,
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
			state.Failure = true
			state.CooldownUntil = s.now().Add(account.Cooldown(res.status, cand.FailStreak)).Unix()
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
			writeError(w, http.StatusUnauthorized, "authentication_error",
				fmt.Sprintf("账号 %s 的凭据被上游拒绝，已停用。%s", cand.Name, res.message))
			return
		case res.status == http.StatusForbidden:
			// 403 多为套餐或模型权限不对，凭据本身还能用，不停用也不再换号。
			writeError(w, http.StatusForbidden, "permission_error",
				fmt.Sprintf("账号 %s 无权使用该模型：%s", cand.Name, res.message))
			return
		case state.Failure:
			lastErr = fmt.Sprintf("%s: %s", cand.Name, res.message)
			continue
		case res.wrote:
			return
		default:
			writeError(w, mapErrorStatus(res.status), errorKind(res.status), res.message)
			return
		}
	}

	writeError(w, http.StatusServiceUnavailable, "api_error", "所有候选账号都失败："+lastErr)
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
	inbound string, raw []byte, req *translate.Request) attempt {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return attempt{status: http.StatusInternalServerError, message: "解密账号凭据失败"}
	}

	upReq, err := s.buildRequest(ctx, a, outbound, inbound, raw, req, string(key))
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: "上游请求失败: " + err.Error(), err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := readErrorBody(resp.Body)
		return attempt{status: resp.StatusCode, message: scrubSecret(msg, string(key))}
	}

	if req.Stream {
		return s.streamSuccess(w, resp, inbound, outbound, req)
	}
	return s.jsonSuccess(w, resp, inbound, outbound, req)
}

// buildRequest 按有效出站协议构造上游请求：路径、凭据头与请求体编码都跟它走。
// 入站协议与它相同时直通（只改 model），不同时经中间表示转换。
func (s *Server) buildRequest(ctx context.Context, a store.Account, outbound, inbound string, raw []byte,
	req *translate.Request, key string) (*http.Request, error) {

	endpoint := s.endpoint(a, outbound)

	var body []byte
	var err error
	if inbound == outbound {
		// 同协议直通：只替换 model，其余字节原样保留。
		body, err = patchModel(raw, req.Model)
	} else {
		body, err = translate.EncodeRequest(outbound, req, req.Model)
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
		return attempt{status: http.StatusBadGateway, message: "读取上游响应失败: " + err.Error(), err: err}
	}

	// 直通：字节原样返回，只用对应协议解析一次用量。
	if inbound == outbound {
		res := attempt{status: http.StatusOK, wrote: true}
		if parsed, err := translate.DecodeResponse(outbound, body, req.Model); err == nil {
			res.inTok, res.outTok = parsed.Usage.In, parsed.Usage.Out
			res.cacheRead, res.cacheWrite = parsed.Usage.CacheRead, parsed.Usage.CacheWrite
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
	}
}

func (s *Server) streamSuccess(w http.ResponseWriter, resp *http.Response,
	inbound, outbound string, req *translate.Request) attempt {

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
		return s.passthroughStream(w, resp, outbound, flush)
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
	emit := func(deltas []translate.Delta) error {
		for _, d := range deltas {
			if d.Usage != nil {
				usage = *d.Usage
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

	scanErr := translate.ScanSSE(resp.Body, func(event string, data []byte) error {
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			return nil
		}
		deltas, err := dec.Decode(data)
		if err != nil {
			return err
		}
		return emit(deltas)
	})

	if scanErr != nil {
		_ = write([]translate.SSEEvent{translate.ErrorEvent("api_error", scanErr.Error())})
	}
	_ = emit(dec.Close())
	_ = write(enc.Finish())

	res := attempt{status: http.StatusOK, wrote: true, inTok: usage.In, outTok: usage.Out,
		cacheRead: usage.CacheRead, cacheWrite: usage.CacheWrite}
	if scanErr != nil {
		res.message = "上游流式响应中断: " + scanErr.Error()
		res.err = scanErr
	}
	return res
}

// passthroughStream 原样转发同协议上游的字节，同时读出用量。
//
// 上游没发到收尾帧就断开时，已写出的字节一个不动，只在末尾追加该协议的合法结尾
// （见 passthroughTail）：直通与转换两条路径对断流的处理一致，客户端总能拿到一个
// 结束了的流，而不是戛然而止的半截字节（DESIGN 5.2 第 6 条、5.4）。
func (s *Server) passthroughStream(w http.ResponseWriter, resp *http.Response, protocol string, flush func()) attempt {
	scanner, err := translate.NewUsageScanner(protocol)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err, wrote: true}
	}

	buf := bufio.NewReaderSize(resp.Body, 32*1024)
	var readErr error
	// ended 记「收尾帧已经转发给客户端了」：收尾帧之后再断开不算断流，不再补任何字节，
	// 完整流的字节因此与上游逐字节一致。
	var ended bool
	// tail 是最后写出的至多两个字节，用来判断已写出的内容是不是停在帧边界上。
	var tail []byte

	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return attempt{status: http.StatusOK, wrote: true, message: "客户端断开", err: werr}
			}
			tail = append(tail, line...)
			if len(tail) > 2 {
				tail = tail[len(tail)-2:]
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(trimmed[len("data:"):])
				scanner.Feed(data)
				if streamFrameEnded(protocol, data) {
					ended = true
				}
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

	in, out, cacheRead, cacheWrite := scanner.Usage()
	res := attempt{status: http.StatusOK, wrote: true, inTok: in, outTok: out,
		cacheRead: cacheRead, cacheWrite: cacheWrite}
	if readErr != nil {
		res.message = "上游流式响应中断: " + readErr.Error()
		res.err = readErr
	}
	if !ended {
		if res.err == nil {
			// 干净的 EOF 也可能断在半路：收尾帧没到就是断流，与转换路径上解码器报错的判据一致。
			res.message = "上游流式响应中断: 收尾帧之前连接就结束了"
			res.err = errStreamTruncated
		}
		appendStreamTail(w, flush, tail, passthroughTail(protocol, res.message, in, out))
	}
	return res
}

// errStreamTruncated 表示上游在收尾帧之前就结束了连接（干净的 EOF，读不出别的错）。
var errStreamTruncated = errors.New("上游流式响应在收尾帧之前结束")

// streamFrameEnded 判断这一帧是不是该协议的收尾帧：anthropic 的 message_stop、
// openai-chat 的 [DONE]、openai-responses 的三个终态事件。
// 只看帧里的类型字段，不看正文，正文里出现同名文字不会被当成收尾。
func streamFrameEnded(protocol string, data []byte) bool {
	switch protocol {
	case ProtocolAnthropic:
		return bytes.Contains(data, []byte(`"type":"message_stop"`))
	case ProtocolChat:
		return string(bytes.TrimSpace(data)) == "[DONE]"
	case ProtocolResponses:
		for _, marker := range [][]byte{
			[]byte(`"type":"response.completed"`),
			[]byte(`"type":"response.incomplete"`),
			[]byte(`"type":"response.failed"`),
		} {
			if bytes.Contains(data, marker) {
				return true
			}
		}
	}
	return false
}

// passthroughTail 给出直通路径断流之后要补的收尾帧，按入站（=出站）协议决定形状：
//
//   - anthropic：event: error，再补 message_delta（带已知 usage）与 message_stop；
//   - openai-chat：一帧 data: {"error":…}，再补 data: [DONE]；
//   - openai-responses：event: error，再补 response.failed。
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
	}
	return nil
}

// appendStreamTail 把收尾帧追加到已写出的字节后面，前面已经发出去的内容一个字节都不改。
// 已写出的内容停在半行或半帧（没以空行结尾）时先补一个空行：不补的话追加的第一行会被并进
// 上一个事件，客户端那边仍然不是一个结束了的流。
func appendStreamTail(w io.Writer, flush func(), written []byte, events []translate.SSEEvent) {
	if len(written) > 0 && !bytes.HasSuffix(written, []byte("\n\n")) {
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

// Endpoint 是 base_url 加协议的后缀猜测，作为没有厂商路由时的兜底：末尾带不带 /v1 都行。
// 正常路径上的地址走 Server.endpoint（按 (厂商, 协议) 查 provider_routes）。
func Endpoint(base, protocol string) string {
	base = normalizedBase(base)
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

// endpoint 解析账号在某种协议上的上游地址：厂商在 provider_routes 里声明过这条协议就用
// base_url 拼上它的 path；没有厂商行、或这家没有该协议的路由时退回后缀猜测（见 DESIGN 5.8）。
// 自建地址的账号（provider 不在表里）因此仍然可用。
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

// normalizedBase 规整 base_url：scheme 之后合并重复斜杠、去掉末尾斜杠。
// store.NormalizeBaseURL 还会校验协议头，这里只是拼地址，脏地址交给 http.NewRequest 报错。
func normalizedBase(base string) string {
	if u, err := store.NormalizeBaseURL(base); err == nil {
		return u
	}
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// joinEndpoint 把 base_url 与路由里的完整路径拼起来，两者都按 store 的同一套规则规整：
// base_url 去尾斜杠（scheme 之后合并重复斜杠），路径补一个前导斜杠、合并重复斜杠、去尾斜杠，
// 因此最终地址在 scheme 之外不出现 `//`，`base_url` 里真实存在的前缀（如 /api/paas/v4）原样保留。
// 其余一律照抄，不推 /v1、不猜后缀。
func joinEndpoint(base, path string) string {
	return normalizedBase(base) + store.NormalizePath(path)
}

// setAuth 按协议给上游请求配好凭据头。
func setAuth(req *http.Request, protocol, key string) {
	if protocol == ProtocolAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
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

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
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
