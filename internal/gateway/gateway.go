// Package gateway 是唯一发上游请求的地方：收 anthropic 入站请求，挑账号，转发，记账。
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

// 协议常量。第一版只实现 anthropic 入站。
const (
	ProtocolAnthropic = "anthropic"
	ProtocolChat      = "openai-chat"
	InboundAnthropic  = "anthropic"
)

// Options 是构造网关需要的依赖。
type Options struct {
	Store  *store.Store
	Key    []byte
	Client *http.Client
	Logf   func(format string, args ...any)
	Now    func() time.Time
}

// Server 是网关。
type Server struct {
	store  *store.Store
	key    []byte
	client *http.Client
	logf   func(format string, args ...any)
	now    func() time.Time
}

// New 构造网关。
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("缺少 store")
	}
	if len(o.Key) == 0 {
		return nil, errors.New("缺少解密密钥")
	}
	s := &Server{store: o.Store, key: o.Key, client: o.Client, logf: o.Logf, now: o.Now}
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
	mux.HandleFunc("/v1/messages", s.handleMessages)
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
	})
}

// attempt 是一次上游尝试的结果。
type attempt struct {
	status  int
	inTok   int64
	outTok  int64
	wrote   bool
	message string
	err     error
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "只接受 POST")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}

	var req translate.AnthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}

	session := r.Header.Get("x-switchboard-session")
	if session == "" {
		session = sessionFromMetadata(req.Metadata)
	}

	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "读取账号失败: "+err.Error())
		return
	}
	candidates := account.Candidates(accounts, req.Model, s.now(), session)
	if len(candidates) == 0 {
		writeError(w, http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("没有可用于模型 %s 的账号，先运行 switchboard account add", req.Model))
		return
	}

	lastErr := "没有可用账号"
	for _, cand := range candidates {
		if r.Context().Err() != nil {
			return
		}

		attemptStart := s.now()
		res := s.tryUpstream(r.Context(), w, cand, raw, &req)
		latency := s.now().Sub(attemptStart)

		rec := &store.RequestRecord{
			TS:             attemptStart.Unix(),
			Inbound:        InboundAnthropic,
			Outbound:       cand.Protocol,
			ModelRequested: req.Model,
			ModelUsed:      req.Model,
			AccountID:      cand.ID,
			HTTPStatus:     res.status,
			OK:             res.err == nil && res.status >= 200 && res.status < 300,
			InTokens:       res.inTok,
			OutTokens:      res.outTok,
			LatencyMS:      latency.Milliseconds(),
			Error:          res.message,
		}
		if err := s.store.InsertRequest(rec); err != nil {
			s.logf("写请求记录失败: %v", err)
		}
		s.logf("%s %s→%s %s %s %d/%d %s%s",
			cand.Name, InboundAnthropic, cand.Protocol, req.Model,
			fmt.Sprintf("%d", res.status), res.inTok, res.outTok,
			latency.Round(time.Millisecond), errorSuffix(res.message))

		if rec.OK {
			return
		}

		switch {
		case res.status == http.StatusUnauthorized:
			// 401 说明凭据本身不行，停用账号，避免同一条错误把整个候选池刷成不可用。
			if err := s.store.SetEnabled(cand.ID, false, res.message); err != nil {
				s.logf("停用账号 %s 失败: %v", cand.Name, err)
			}
			writeError(w, http.StatusUnauthorized, "authentication_error",
				fmt.Sprintf("账号 %s 的凭据被上游拒绝，已停用。%s", cand.Name, res.message))
			return
		case res.status == http.StatusForbidden:
			// 403 多为套餐或模型权限不对，凭据本身还能用，不停用也不再换号。
			writeError(w, http.StatusForbidden, "permission_error",
				fmt.Sprintf("账号 %s 无权使用该模型：%s", cand.Name, res.message))
			return
		case account.Retryable(res.status) && !res.wrote:
			until := s.now().Add(account.Cooldown(res.status, 0)).Unix()
			if err := s.store.SetCooldown(cand.ID, until, res.message); err != nil {
				s.logf("设置冷却失败: %v", err)
			}
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

func sessionFromMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	return meta.UserID
}

func (s *Server) tryUpstream(ctx context.Context, w http.ResponseWriter, a store.Account,
	raw []byte, req *translate.AnthropicRequest) attempt {

	key, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		return attempt{status: http.StatusInternalServerError, message: "解密账号凭据失败"}
	}

	upReq, err := s.buildRequest(ctx, a, raw, req, string(key))
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
		return s.streamSuccess(w, resp, req, a)
	}
	return s.jsonSuccess(w, resp, req, a)
}

func (s *Server) buildRequest(ctx context.Context, a store.Account, raw []byte,
	req *translate.AnthropicRequest, key string) (*http.Request, error) {

	endpoint := Endpoint(a.BaseURL, a.Protocol)

	var body []byte
	var err error
	if a.Protocol == ProtocolAnthropic {
		body, err = patchModel(raw, req.Model)
	} else {
		var chatReq *translate.ChatRequest
		chatReq, err = translate.ToChat(req, req.Model)
		if err == nil {
			body, err = json.Marshal(chatReq)
		}
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
	setAuth(httpReq, a.Protocol, key)
	return httpReq, nil
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
	req *translate.AnthropicRequest, a store.Account) attempt {

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: "读取上游响应失败: " + err.Error(), err: err}
	}

	if a.Protocol == ProtocolAnthropic {
		var out translate.AnthropicResponse
		if err := json.Unmarshal(body, &out); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return attempt{status: http.StatusOK, wrote: true, inTok: out.Usage.InputTokens, outTok: out.Usage.OutputTokens}
		}
		return attempt{status: http.StatusBadGateway, message: "上游响应不是合法 JSON"}
	}

	var chatResp translate.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return attempt{status: http.StatusBadGateway, message: "上游响应不是合法 JSON: " + err.Error(), err: err}
	}
	out, err := translate.FromChat(&chatResp, req.Model)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return attempt{status: http.StatusInternalServerError, message: err.Error(), err: err}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
	return attempt{status: http.StatusOK, wrote: true, inTok: out.Usage.InputTokens, outTok: out.Usage.OutputTokens}
}

func (s *Server) streamSuccess(w http.ResponseWriter, resp *http.Response,
	req *translate.AnthropicRequest, a store.Account) attempt {

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

	if a.Protocol == ProtocolAnthropic {
		return s.passthroughStream(w, resp, flush)
	}

	state := translate.NewChatStream(req.Model)
	scanErr := translate.ScanSSE(resp.Body, func(event string, data []byte) error {
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			return nil
		}
		events, err := state.Translate(data)
		if err != nil {
			return err
		}
		for _, e := range events {
			if _, err := w.Write(e.Bytes()); err != nil {
				return err
			}
		}
		flush()
		return nil
	})

	if scanErr != nil {
		_, _ = w.Write(translate.ErrorEvent("api_error", scanErr.Error()).Bytes())
		flush()
	}
	for _, e := range state.Finish() {
		if _, err := w.Write(e.Bytes()); err != nil {
			break
		}
	}
	flush()

	in, out := state.Usage()
	res := attempt{status: http.StatusOK, wrote: true, inTok: in, outTok: out}
	if scanErr != nil {
		res.message = "上游流式响应中断: " + scanErr.Error()
		res.err = scanErr
	}
	return res
}

// passthroughStream 原样转发同协议上游的字节，同时读出用量。
func (s *Server) passthroughStream(w http.ResponseWriter, resp *http.Response, flush func()) attempt {
	buf := bufio.NewReaderSize(resp.Body, 32*1024)
	var sc translate.AnthropicUsageScanner
	var readErr error

	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return attempt{status: http.StatusOK, wrote: true, message: "客户端断开", err: werr}
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				sc.Feed(bytes.TrimSpace(trimmed[len("data:"):]))
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

	in, out := sc.Usage()
	res := attempt{status: http.StatusOK, wrote: true, inTok: in, outTok: out}
	if readErr != nil {
		res.message = "上游流式响应中断: " + readErr.Error()
		res.err = readErr
	}
	return res
}

// Endpoint 由 base_url 与协议推出上游地址。base_url 末尾带不带 /v1 都行。
func Endpoint(base, protocol string) string {
	base = strings.TrimRight(base, "/")
	suffix := "/chat/completions"
	if protocol == ProtocolAnthropic {
		suffix = "/messages"
	}
	if strings.HasSuffix(base, "/v1") {
		return base + suffix
	}
	return base + "/v1" + suffix
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
