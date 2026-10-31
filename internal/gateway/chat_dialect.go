package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"lax/internal/plugin"
	"lax/internal/translate"
)

// 非语音操作绑到插件方言时的两条路：请求侧在宿主编好的报文上打补丁，响应侧由方言把上游报文
// 解成语义结果，宿主再按入站协议编回去。
//
// 边界的理由：地址、凭据、超时、重试与记账只有宿主一份。方言只能改请求体（Plan.Body/Plan.JSON）
// 与给一条相对路径（Plan.Path），拿不到域名与凭据，因此一个写错的方言最多打错路径，
// 不会把账号的 key 带到别的主机上。

const (
	// dialectStreamChunk 是一次交给方言的上游字节上限。块边界任意，方言自己处理半帧与多帧。
	dialectStreamChunk = 4 << 10
	// chatMaxBody 是对话类请求与响应的报文上限，与 handleInbound/jsonSuccess 用的 32 MiB 一致。
	chatMaxBody = 32 << 20
	// dialectErrorMax 是方言报错写进事件、请求记录与错误体时的截断长度：插件给的文案没有上限，
	// 整段塞进 SSE 帧或数据库列都没有意义。
	dialectErrorMax = 300
)

// operationFor 按入站协议与动作给出操作名：gemini 的 :embedContent 落在 gemini 路径上，
// 做的是向量化，按动作改一次，否则会去查对话那条方言绑定。
func operationFor(inbound string, embedding bool) string {
	if embedding {
		return plugin.OpEmbeddingsCreate
	}
	return plugin.OperationForInbound(inbound)
}

// voiceInbound 判断入站协议是不是语音两条之一。语音入站不走对话方言这条路：
// 它的输入归一（正文、音频）与对话的报文补丁不是同一套。
func voiceInbound(inbound string) bool {
	return inbound == ProtocolSpeech || inbound == ProtocolTranscriptions
}

// chatDialectApplies 判断这次尝试要不要经插件方言改请求与解响应。
// d 非空只可能是语音路由或 checkOperationDialect 给的方言，后者只给非语音入站。
func chatDialectApplies(inbound string, d plugin.Dialect) bool {
	return d != nil && !voiceInbound(inbound)
}

// dialogueInbound 判断入站协议是不是四条能进中间表示的对话协议。
// 只有它们认得「方言返回 json 结果」这条约定：向量化的报文没有消息序列，
// 决策协议的形状由它自己定，都不进 translate 的编解码器。
func dialogueInbound(inbound string) bool {
	switch inbound {
	case ProtocolAnthropic, ProtocolChat, ProtocolResponses, ProtocolGemini:
		return true
	}
	return false
}

// dialectLimits 给出一次方言调用的上限：请求体与响应体沿用该入站协议的报文上限。
func dialectLimits(inbound string) plugin.Limits {
	max := int64(chatMaxBody)
	if inbound == ProtocolSystemOne {
		max = systemOneMaxBody
	}
	return plugin.Limits{MaxInputBytes: max, MaxOutputBytes: max}
}

// dialectRelativePath 判断方言给的路径能不能用：只认以 `/` 开头、不含 `..` 段的相对路径。
// 域名与 scheme 只能来自厂商 base_url，否则一个方言就能把账号的凭据带到任意主机上。
func dialectRelativePath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
		return false
	}
	// 查询串与 `#` 之后的内容不参与路径判定，先切掉再按段检查路径穿越。
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// applyChatDialect 让方言在宿主编好的上游报文上打补丁，返回新的请求体与方言要的路径
// （空串表示不改地址）。
//
// 报文优先级：Plan.Body 非空就用它（通常是给宿主报文打了字段补丁），否则 Plan.JSON 非 nil 时
// 序列化它（方言自己从零构建），两者都没有就是方言没改，保留宿主的报文。
//
// 请求头由宿主决定，不采用方言给的头（Plan 也没有这个字段）：白名单透传与凭据注入只有一份，
// 让插件另开一个注入口，凭据的落点就变成两处。
func (s *Server) applyChatDialect(d plugin.Dialect, operation, inbound, outbound string,
	req *translate.Request, body []byte) ([]byte, string, error) {

	plan, err := d.Prepare(plugin.Input{
		Operation: operation, Wire: outbound, Model: req.Model,
		Body: body, Stream: req.Stream, Limits: dialectLimits(inbound),
	})
	if err != nil {
		return nil, "", fmt.Errorf("方言 %s 编请求失败: %w", d.ID(), err)
	}
	switch {
	case len(plan.Body) > 0:
		body = plan.Body
	case plan.JSON != nil:
		packed, err := json.Marshal(plan.JSON)
		if err != nil {
			return nil, "", fmt.Errorf("方言 %s 编出的请求体不是 JSON: %w", d.ID(), err)
		}
		body = packed
	}
	return body, plan.Path, nil
}

// chatDialectSuccess 处理插件方言路径的成功响应。非 2xx 由调用方按既有规则处理：
// 上游错误体仍按 errorMessageFromBody 与 scrubSecret 走，不交给方言解。
func (s *Server) chatDialectSuccess(ctx context.Context, w http.ResponseWriter, resp *http.Response,
	inbound, outbound, operation string, req *translate.Request, d plugin.Dialect,
	secretValue string) attempt {

	if req.Stream {
		sd, ok := d.(plugin.StreamDialect)
		if !ok {
			// handleInbound 在发上游请求之前已经拦过一遍（见 handleInbound 里的流式判定），
			// 这里保留一次判定：别的调用方漏了那一步时，不能静默退回非流式。
			return attempt{status: http.StatusBadRequest,
				message: fmt.Sprintf("方言 %s 不支持流式响应", d.ID())}
		}
		return s.chatDialectStream(ctx, w, resp, inbound, outbound, operation, req, d, sd, secretValue)
	}

	limits := dialectLimits(inbound)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limits.MaxOutputBytes))
	if err != nil {
		// 状态码到了、响应体没读完：与 jsonSuccess 一样按连接层档记。
		return attempt{status: http.StatusBadGateway, transport: true, message: "读取上游响应失败: " + err.Error(), err: err}
	}
	result, err := d.Decode(plugin.Input{
		Operation: operation, Wire: outbound, Model: req.Model, Limits: limits,
	}, plugin.Response{
		Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body,
	})
	if err != nil {
		return attempt{status: http.StatusBadGateway,
			message: fmt.Sprintf("plugin_error：方言 %s 解上游响应失败：%v", d.ID(), err), err: err}
	}

	switch result.Kind {
	case plugin.ResultJSON:
		// 约定：ResultJSON 是**出站线协议形状**的整条响应体（不是入站形状）。方言只负责把
		// 上游那种奇怪的报文改写成一条本宿主认识的响应，转成客户端协议由 translate 做：
		// 入站协议有四种，让每个方言自己按入站协议编四种形状既重复又容易漏字段。
		//
		// 入站与出站同协议时跳过翻译：向量化与决策请求没有中间表示，而同一协议的响应
		// 形状就是客户端要读的那一份。
		if inbound == outbound {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(result.JSON)
			return attempt{
				status: http.StatusOK, wrote: true,
				inTok: result.Usage.In, outTok: result.Usage.Out,
				cacheRead: result.Usage.CacheRead, cacheWrite: result.Usage.CacheWrite,
				reasoning: result.Usage.Reasoning, total: result.Usage.Total,
			}
		}
		if !dialogueInbound(inbound) {
			return attempt{status: http.StatusBadGateway,
				message: fmt.Sprintf("plugin_error：方言 %s 返回了 json 结果，但入站协议 %s 不是对话协议："+
					"向量化与决策请求没有中间表示，一条 %s 的响应无从改编成客户端读的形状",
					d.ID(), inbound, outbound)}
		}
		parsed, derr := translate.DecodeResponse(outbound, result.JSON, req.Model)
		if derr != nil {
			return attempt{status: http.StatusBadGateway,
				message: fmt.Sprintf("plugin_error：方言 %s 返回的 json 不是 %s 协议的响应体：%v",
					d.ID(), outbound, derr), err: derr}
		}
		if parsed.StopNote != "" {
			s.logf("方言 %s 给的结束原因 %s 在中间表示里没有对应取值，按 %s 编给客户端",
				d.ID(), parsed.StopNote, parsed.StopReason)
		}
		payload, eerr := translate.EncodeResponse(inbound, parsed)
		if eerr != nil {
			return attempt{status: http.StatusInternalServerError, message: eerr.Error(), err: eerr}
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

	case plugin.ResultText:
		// 纯文本只给不按对话协议应答的入站（systemone）：对话客户端读的是结构化响应，
		// 一段纯文本在那边就是一次解析失败，按方言实现错误报。
		if inbound != ProtocolSystemOne {
			return attempt{status: http.StatusBadGateway,
				message: fmt.Sprintf("plugin_error：方言 %s 对 %s 操作返回了文本结果："+
					"对话操作要返回 json 形状的上游响应体（ResultJSON），否则客户端拿到的不是它认识的响应",
					d.ID(), operation)}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(result.Text))
		return attempt{
			status: http.StatusOK, wrote: true,
			inTok: result.Usage.In, outTok: result.Usage.Out,
			cacheRead: result.Usage.CacheRead, cacheWrite: result.Usage.CacheWrite,
			reasoning: result.Usage.Reasoning, total: result.Usage.Total,
		}

	default:
		return attempt{status: http.StatusBadGateway,
			message: fmt.Sprintf("plugin_error：方言 %s 返回了未知的结果种类 %q", d.ID(), result.Kind)}
	}
}

// chatDialectStream 用方言逐块解上游流，把语义事件翻成中间表示的增量再按入站协议下发。
//
// 断流判定与 streamSuccess 一致：收尾事件写出之后客户端断开不算失败，之前断开按没送完记，
// 上游读出错与方言报错都记失败并且已经写了字节（wrote 为真），因此都不换号、不落冷却。
func (s *Server) chatDialectStream(ctx context.Context, w http.ResponseWriter, resp *http.Response,
	inbound, outbound, operation string, req *translate.Request, d plugin.Dialect,
	sd plugin.StreamDialect, secretValue string) attempt {

	enc, err := translate.NewStreamEncoder(inbound, req.Model)
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}

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

	write := func(events []translate.SSEEvent) error {
		for _, e := range events {
			if _, werr := w.Write(e.Bytes()); werr != nil {
				return werr
			}
		}
		flush()
		return nil
	}

	limits := dialectLimits(inbound)
	input := plugin.Input{
		Operation: operation, Wire: outbound, Model: req.Model, Stream: true, Limits: limits,
	}

	var usage translate.Usage
	// upstreamErr 记方言自己发过来的错误事件正文：客户端已经收到那条错误，这里不再补一个，
	// 但它仍然说明这次请求失败了，请求记录要写它而不是一句泛泛的断流。
	var upstreamErr string
	// finished 记「收尾事件已经写到客户端了」：之后客户端断开是完成的请求，不是断流。
	var finished bool

	emit := func(deltas []translate.Delta) error {
		for _, delta := range deltas {
			if delta.Usage != nil {
				usage = *delta.Usage
			}
			if delta.Kind == translate.DeltaError {
				delta.Text = scrubSecret(delta.Text, secretValue)
				upstreamErr = delta.Text
			}
			if werr := write(enc.Emit(delta)); werr != nil {
				return werr
			}
			if delta.Kind == translate.DeltaStop {
				finished = true
			}
		}
		return nil
	}

	// events 把方言给的一批事件翻成增量写下去。用量与结束原因在这里收口：
	// 有的上游先报用量再报结束原因，分开写会让客户端读到一条没有用量的收尾。
	events := func(items []plugin.StreamEvent) error {
		var deltas []translate.Delta
		for _, ev := range items {
			switch ev.Type {
			case plugin.EventUsage:
				if ev.Usage != nil {
					usage = translateUsage(*ev.Usage)
				}
			case plugin.EventFinish:
				deltas = append(deltas, translate.Delta{
					Kind: translate.DeltaStop, StopReason: dialectStopReason(ev.Reason), Usage: &usage,
				})
			default:
				if delta, ok := dialectDelta(ev); ok {
					deltas = append(deltas, delta)
				}
			}
		}
		if len(deltas) == 0 {
			return nil
		}
		return emit(deltas)
	}

	if werr := write(enc.Start()); werr != nil {
		return attempt{status: http.StatusOK, wrote: true, message: clientGoneMessage, err: errClientGone}
	}

	reader := bufio.NewReaderSize(resp.Body, dialectStreamChunk)
	buf := make([]byte, dialectStreamChunk)
	seq := 0
	var scanErr, decodeErr, writeErr error
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			items, derr := sd.DecodeStream(input, plugin.StreamChunk{Seq: seq, Data: buf[:n]})
			seq++
			if derr != nil {
				decodeErr = derr
				break
			}
			if werr := events(items); werr != nil {
				writeErr = werr
				break
			}
			if finished {
				// 方言已经报过结束：上游剩下的只有收尾帧，再解一遍会发出第二条收尾。
				break
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				scanErr = rerr
			}
			break
		}
	}
	// 干净读完之后再交一块 Final：方言可以把攒着的半帧（最后一行没有换行）交出来。
	if scanErr == nil && decodeErr == nil && writeErr == nil && !finished {
		items, derr := sd.DecodeStream(input, plugin.StreamChunk{Seq: seq, Final: true})
		if derr != nil {
			decodeErr = derr
		} else if werr := events(items); werr != nil {
			writeErr = werr
		}
	}

	// 写客户端失败只可能是客户端不在了；上游读错误在客户端断开时是 context canceled。
	clientGone := writeErr != nil || ctx.Err() != nil || errors.Is(scanErr, context.Canceled)
	failure := ""
	switch {
	case clientGone && !finished:
		failure = clientGoneMessage
	case clientGone:
		// 收尾已写出，客户端随后挂断：不记失败。
	case decodeErr != nil:
		failure = fmt.Sprintf("plugin_error：方言 %s 解流失败：%v", d.ID(), decodeErr)
	case scanErr != nil:
		failure = "上游流式响应中断: " + scanErr.Error()
	case !finished:
		failure = truncatedMessage
	}
	if failure != "" && upstreamErr == "" && !clientGone {
		// 错误增量走编码器，不另写一帧：形状跟入站协议走，与非 2xx 响应是同一份映射。
		_ = emit([]translate.Delta{{Kind: translate.DeltaError, Text: failure}})
	}
	_ = write(enc.Finish())

	res := attempt{status: http.StatusOK, wrote: true,
		inTok: usage.In, outTok: usage.Out, cacheRead: usage.CacheRead, cacheWrite: usage.CacheWrite,
		reasoning: usage.Reasoning, total: usage.Total}
	if failure != "" {
		res.message = scrubSecret(failure, secretValue)
		res.err = errStreamTruncated
		if decodeErr != nil {
			res.err = decodeErr
		}
		if scanErr != nil {
			res.err = scanErr
		}
		if clientGone {
			res.err = errClientGone
		}
	}
	return res
}

// dialectDelta 把方言的一条语义事件翻成中间表示的增量。第二项为 false 表示这条事件不产生帧
// （tool_call.end：工具块由编码器在换块或收尾时关掉，没有单独的结束帧）。
//
// 块号统一给 0：方言的 StreamEvent 里没有块号，一条方言同时交叉给两个工具调用喂参数时
// 两者会落进同一个块。这一版支持的方言是一个流里只有一个工具调用的那种；
// 要多工具并行，得先在事件里加块号。
func dialectDelta(ev plugin.StreamEvent) (translate.Delta, bool) {
	switch ev.Type {
	case plugin.EventTextDelta:
		return translate.Delta{Kind: translate.DeltaText, Text: ev.Text}, true
	case plugin.EventReasoningDelta:
		return translate.Delta{Kind: translate.DeltaThinking, Text: ev.Text}, true
	case plugin.EventToolStart:
		return translate.Delta{Kind: translate.DeltaToolStart, Index: ev.Index, ToolID: ev.ID, ToolName: ev.Name}, true
	case plugin.EventToolDelta:
		return translate.Delta{Kind: translate.DeltaToolArgs, Index: ev.Index, ToolID: ev.ID, ArgsJSON: ev.Arguments}, true
	case plugin.EventError:
		return translate.Delta{Kind: translate.DeltaError, Text: truncateDialectError(ev.Error)}, true
	}
	return translate.Delta{}, false
}

// dialectStopReason 把方言的结束原因折成中间表示的四种取值。认不出的按正常结束：
// 编一个不存在的取值会让客户端拿到它不认识的枚举值。
func dialectStopReason(reason string) string {
	switch reason {
	case "length":
		return translate.StopMaxTokens
	case "tool_use":
		return translate.StopToolUse
	case "stop_sequence":
		return translate.StopSequence
	default:
		return translate.StopEndTurn
	}
}

// translateUsage 把方言报的用量换成中间表示的用量。方言没有金额概念，计费只按宿主的价格表。
func translateUsage(u plugin.Usage) translate.Usage {
	return translate.Usage{
		In: u.In, Out: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		Reasoning: u.Reasoning, Total: u.Total,
	}
}

// truncateDialectError 截断方言报错的文案。按 rune 截，避免把一个多字节字符切成两半。
func truncateDialectError(text string) string {
	runes := []rune(text)
	if len(runes) <= dialectErrorMax {
		return text
	}
	return string(runes[:dialectErrorMax]) + "…"
}
