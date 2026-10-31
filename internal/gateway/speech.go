package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode/utf8"

	"lax/internal/store"
)

// 语音。两条线协议都按 OpenAI 的形状：openai-speech（文本转语音，POST /v1/audio/speech）
// 与 openai-transcriptions（语音转文本，POST /v1/audio/transcriptions）。它们没有消息序列，
// translate 的编解码器不认它们，只做同协议直通。这个文件放这条路径上的纯函数：
// 路径、最少校验、multipart 重写、音频响应与用量读取，不读库、不发请求。
//
// 两条协议与对话、systemone、向量化都不相通：音频字节与消息序列之间没有可定义的映射，
// 转发只是把 400 推后到上游。

const (
	// speechPath 是 TTS 的入站路径，也是 openai-speech 的唯一入口。
	speechPath = "/v1/audio/speech"
	// transcriptionsPath 是 ASR 的入站路径。
	transcriptionsPath = "/v1/audio/transcriptions"
	// speechMaxBody 是语音两条路径的请求体上限。音频文件按 OpenAI 自己的上限是 25 MB，
	// 留出 multipart 边框与其它字段的余量取 32 MiB，与对话路径一致。
	speechMaxBody = 32 << 20
)

// speechRequest 是 TTS 入站的解析结果：模型名用于选号，字符数用于计费。
// 字符数在入站时就数好，响应体不必留着解析（这条端点也不报 token）。
type speechRequest struct {
	Model  string
	Chars  int64
	Stream bool
}

// decodeSpeechRequest 做最少校验，返回选号要用的模型名与计费的字符数。
//
// 只判三件事：model 是非空字符串、input 存在且是字符串或非空字符串数组、stream_format
// 给了的话必须是已知取值之一。voice 交给上游：各家认的音色名不一样，本机维护一份名单只会
// 让新音色被本地拒掉。
func decodeSpeechRequest(raw []byte) (*speechRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %v", err)
	}
	if fields == nil {
		return nil, errors.New(`请求体是 null：语音合成请求是 {"model":"…","input":"…","voice":"…"} 这样的对象`)
	}

	modelField, ok := fields["model"]
	if !ok || isJSONNull(modelField) {
		return nil, errors.New("缺少 model 字段：语音合成请求按模型名选号，模型名写在请求体的 model 字段里")
	}
	var model string
	if err := json.Unmarshal(modelField, &model); err != nil {
		return nil, errors.New("model 必须是字符串：语音合成请求按模型名选号，写对象或数组都选不出账号")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model 不能是空字符串：语音合成请求按模型名选号，空名字匹配不上任何账号")
	}

	input, ok := fields["input"]
	if !ok || isJSONNull(input) {
		return nil, errors.New("缺少 input 字段：要合成的文本写在 input 里，可以是一个字符串，也可以是字符串数组")
	}
	chars, err := speechInputChars(input)
	if err != nil {
		return nil, err
	}
	if chars == 0 {
		return nil, errors.New("input 是空的：没有文本就没有可合成的内容，空 input 通常来自变量没填上")
	}

	stream := false
	if rawFormat, ok := fields["stream_format"]; ok && !isJSONNull(rawFormat) {
		var format string
		if err := json.Unmarshal(rawFormat, &format); err != nil {
			return nil, errors.New("stream_format 必须是字符串：这条字段选响应形态，写对象或数组都读不出形态")
		}
		switch format {
		case "", "audio", "sse":
		default:
			return nil, fmt.Errorf("stream_format 是 %q，只认 audio 或 sse："+
				"要逐帧返回就写 sse，要一段完整音频就写 audio 或干脆不写", format)
		}
		stream = format == "sse"
	}

	return &speechRequest{Model: model, Chars: chars, Stream: stream}, nil
}

// speechInputChars 数 input 的字符数：字符串按 rune 数，字符串数组按各元素之和。
// 校验与计数在同一次解析里做，调用方拿到非零值就说明 input 合法。
func speechInputChars(input json.RawMessage) (int64, error) {
	var text string
	if err := json.Unmarshal(input, &text); err == nil {
		return int64(utf8.RuneCountInString(text)), nil
	}
	var parts []string
	if err := json.Unmarshal(input, &parts); err == nil {
		var total int64
		for _, part := range parts {
			total += int64(utf8.RuneCountInString(part))
		}
		return total, nil
	}
	return 0, errors.New("input 必须是字符串或字符串数组：数字、对象与布尔值都合不出文本，" +
		"`\"input\": 123` 这类写法在上游也只会换回 400")
}

// decodeTranscriptionsRequest 做最少校验，只返回模型名；请求体本身留在 raw 里，
// 等别名解析完成、真名确定之后再重写 model 那个 part（见 rewriteTranscriptionsBody）。
//
// 判三件事：Content-Type 是 multipart/form-data 且带 boundary、有 model part、有非空的 file part。
// 其余字段（language、prompt、response_format、temperature、stream 一类）原样转发。
func decodeTranscriptionsRequest(raw []byte, contentType string) (string, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return "", err
	}
	model, hasFile, err := scanTranscriptionsParts(raw, boundary)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(model) == "" {
		return "", errors.New("缺少 model 字段：语音转写请求按模型名选号，" +
			"multipart 里要有一个名为 model 的表单字段")
	}
	if !hasFile {
		return "", errors.New("缺少 file 字段：要转写的音频写在名为 file 的文件字段里，" +
			"只写 model 不发音频的话上游只会换回 400")
	}
	return model, nil
}

// multipartBoundary 从 Content-Type 里取 boundary，并确认媒体类型就是 multipart/form-data。
func multipartBoundary(contentType string) (string, error) {
	if strings.TrimSpace(contentType) == "" {
		return "", errors.New("请求体不是 multipart/form-data：语音转写要上传音频文件，" +
			"Content-Type 必须带 boundary，字段与文件都放在 multipart 的 part 里")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("Content-Type 解析失败: %v", err)
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") {
		return "", fmt.Errorf("请求体是 %s，语音转写只收 multipart/form-data："+
			"音频文件要放进 multipart 的 file 字段，JSON 请求体在这里无法表达音频字节", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", errors.New("Content-Type 缺 boundary：multipart/form-data; boundary=… 里的 boundary 不能省，" +
			"没有它上游分不出各个 part 的边界")
	}
	return boundary, nil
}

// scanTranscriptionsParts 遍历一遍 multipart，取 model 字段的值并记下有没有非空的 file part。
//
// 只读到判定的部分就停：file 一般排在 model 后面，整段音频读完只为确认它非空是白读的，
// 读一个字节就够。遍历顺序不影响结论（model 在 file 之后也能取到）。
func scanTranscriptionsParts(raw []byte, boundary string) (model string, hasFile bool, err error) {
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return model, hasFile, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("读取 multipart 失败: %v", err)
		}
		switch part.FormName() {
		case "model":
			value, err := io.ReadAll(io.LimitReader(part, 4096))
			if err != nil {
				return "", false, fmt.Errorf("读取 model 字段失败: %v", err)
			}
			if model == "" {
				model = string(value)
			}
		case "file":
			var one [1]byte
			if n, _ := io.ReadFull(part, one[:]); n > 0 {
				hasFile = true
			}
		}
		_ = part.Close()
		if model != "" && hasFile {
			return model, true, nil
		}
	}
}

// rewriteTranscriptionsBody 重写 multipart：只换 model 那个 part 的值，其余 part 的头部与
// 内容原样拷贝，boundary 重新生成。
//
// 复用入站的 boundary 要把那串字符抄进新报文，抄错一个字符上游就只能报「找不到 boundary」；
// 重新生成是免费的。音频 part 用 io.Copy 搬，不再整段解析一次。
// 同名 model 出现多次时只写第一次那一个值：多出来的 model part 上游也会拒，本机不替它猜哪个算数。
func rewriteTranscriptionsBody(raw []byte, contentType, model string) ([]byte, string, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return nil, "", err
	}
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	modelWritten := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("读取 multipart 失败: %v", err)
		}
		if part.FormName() == "model" {
			if !modelWritten {
				if err := writer.WriteField("model", model); err != nil {
					return nil, "", fmt.Errorf("重写 model 字段失败: %v", err)
				}
				modelWritten = true
			}
			_ = part.Close()
			continue
		}
		dst, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, "", fmt.Errorf("重写 multipart 失败: %v", err)
		}
		if _, err := io.Copy(dst, part); err != nil {
			return nil, "", fmt.Errorf("搬动 multipart 内容失败: %v", err)
		}
		_ = part.Close()
	}
	if !modelWritten {
		return nil, "", errors.New("multipart 里没有 model 字段：重写时找不到要替换的那一项")
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("收尾 multipart 失败: %v", err)
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// speechUsage 读 ASR 响应里的音频秒数：优先 usage.duration（gpt-4o-transcribe 的形状），
// 没有就退回顶层 duration（verbose_json 才有）。秒数取整到最近一秒：
// requests.audio_seconds 是整数列，截断会把 0.9 秒记成 0，逐条累加会让短音频整批变成免费。
func speechUsage(body []byte) int64 {
	var payload struct {
		Duration float64 `json:"duration"`
		Usage    struct {
			Duration float64 `json:"duration"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	seconds := payload.Usage.Duration
	if seconds == 0 {
		seconds = payload.Duration
	}
	if seconds <= 0 {
		return 0
	}
	return int64(seconds + 0.5)
}

// speechContentType 给 TTS 的音频响应挑 Content-Type：上游给了就用它的（这是事实来源，
// 各家对 opus 写 audio/ogg 还是 audio/opus 并不统一）；没给时按入站请求的
// response_format 兜底，再不行写 application/octet-stream。
func speechContentType(upstream string, raw []byte) string {
	if ct := strings.TrimSpace(upstream); ct != "" {
		return ct
	}
	var fields struct {
		Format string `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &fields); err == nil {
		switch fields.Format {
		case "mp3", "":
			return "audio/mpeg"
		case "opus":
			return "audio/ogg"
		case "aac":
			return "audio/aac"
		case "flac":
			return "audio/flac"
		case "wav":
			return "audio/wav"
		case "pcm":
			return "audio/pcm"
		}
	}
	return "application/octet-stream"
}

// SpeechPath 返回 TTS 的入站路径。命令行不另写一份字面量：路径变了只改这里。
func SpeechPath() string { return speechPath }

// TranscriptionsPath 返回 ASR 的入站路径。
func TranscriptionsPath() string { return transcriptionsPath }

// speechOutbound 给语音请求选协议：入站是哪一条，出站就是哪一条。协议在这里是路由事实：
// 厂商声明了这条路由才转发，不按模型的有效出站协议猜——一条模型是 TTS 还是 ASR
// 由入站路径决定，与向量化同理（见 5.16）。返回空串时第二个值是给客户端的文案。
func (s *Server) speechOutbound(a store.Account, inbound string) (string, string) {
	route, ok, err := s.store.LookupProviderRoute(a.Provider, inbound)
	if err != nil {
		s.logf("读厂商 %s 的 %s 路由失败: %v", a.Provider, inbound, err)
		return "", fmt.Sprintf("账号 %s：读厂商 %s 的 %s 路由失败（%v），先运行 lax doctor 检查数据库",
			a.Name, a.Provider, inbound, err)
	}
	if !ok || strings.TrimSpace(route.Path) == "" {
		return "", speechNoRouteMessage(a.Provider, inbound)
	}
	return inbound, ""
}

// speechNoRouteMessage 说明这家厂商没有声明语音要用的那条路由，并给出下一步命令。
func speechNoRouteMessage(provider, protocol string) string {
	hint := speechPath
	if protocol == ProtocolTranscriptions {
		hint = transcriptionsPath
	}
	return fmt.Sprintf("厂商 %s 没有声明 %s 路由，语音请求发不出去：用 lax provider set %s %s=%s 声明这条路由，"+
		"或让这座网关连一家声明过它的上游",
		provider, protocol, provider, protocol, hint)
}

// speechInboundPath 给错误文案里的下一步命令用。
func speechInboundPath(protocol string) string {
	switch protocol {
	case ProtocolSpeech:
		return speechPath
	case ProtocolTranscriptions:
		return transcriptionsPath
	}
	return ""
}

// speechProbeBody 是 TTS 探活的应错结构：model 是真实模型名，input 是 null。
// 结构错在 input 上，上游要么在参数校验上回 400、要么先校验凭据回 401：两种都不是真实合成。
func speechProbeBody(model string) ([]byte, error) {
	return json.Marshal(map[string]any{"model": model, "input": nil})
}

// transcriptionsProbeBody 是 ASR 探活的应错结构：multipart 里只有 model、没有 file。
// 返回正文与 Content-Type（带 boundary）。
func transcriptionsProbeBody(model string) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// speechProbeVerdict 把语音探活的上游响应翻成可达性结论。
//
// 与对话的探活不同，这里没有 2xx 才叫可用：请求结构是故意错的，400 与 422 正是预期，
// 只证明这条路由存在、请求到了业务层。404 是路由或模型名不对，401/403 是凭据问题。
//
// 返回 access 与说明；ok 为假表示「不改可达性」（与对话探活「没拿到判定」同义）。
// note 写进 account_models.error，把「凭据未验证」这件事留在库里，别让 400 看起来像验过了。
func speechProbeVerdict(status int, upstreamMessage string) (access string, note string, ok bool) {
	switch {
	case status == http.StatusUnauthorized:
		return store.AccessForbidden, "", false
	case status == http.StatusForbidden:
		return store.AccessForbidden, upstreamMessage, true
	case status == http.StatusNotFound:
		return store.AccessMissing, upstreamMessage, true
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return store.AccessAllowed, "端点可达，凭据未验证", true
	case status >= 200 && status < 300:
		return store.AccessAllowed, "端点可达，凭据未验证", true
	}
	return "", upstreamMessage, false
}

// speechSuccess 是语音两条协议的成功响应：内容原样回给客户端，只在 ASR 的 JSON 响应上
// 读一次音频秒数。
//
// 音频不攒：TTS 的输出可能是几十兆，攒完再发只是把首包时间拖到整段合成结束；ASR 的 SSE
// 同理，逐块转发。ASR 的非流式 JSON 响应很小（一个 text 与几段 segments），读全才能取
// usage.duration，与音频那条路分开处理。
func (s *Server) speechSuccess(w http.ResponseWriter, resp *http.Response, outbound string, raw []byte) attempt {
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))

	if outbound == ProtocolTranscriptions && !strings.HasPrefix(contentType, "text/event-stream") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, speechMaxBody))
		if err != nil {
			return attempt{status: http.StatusBadGateway, transport: true,
				message: "读取上游响应失败: " + err.Error(), err: err}
		}
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return attempt{status: http.StatusOK, wrote: true, audioSeconds: speechUsage(body)}
	}

	if outbound == ProtocolSpeech {
		contentType = speechContentType(contentType, raw)
	} else if contentType == "" {
		contentType = "text/event-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// 客户端断开：与其余流式路径一样记成一次失败，但不换号（客户端已经走了）。
				return attempt{status: http.StatusOK, wrote: true, message: "客户端断开: " + writeErr.Error(), err: writeErr}
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return attempt{status: http.StatusOK, wrote: true}
			}
			return attempt{status: http.StatusOK, wrote: true,
				message: "上游响应在上游中断: " + readErr.Error(), err: readErr}
		}
	}
}

// speechInteropMessage 是语音协议与其它协议不相通时的 400 文案。
// 两个方向共用它：入站是语音、出站不是同一条语音协议，或反过来。
func speechInteropMessage(inbound, outbound, model string) string {
	if inbound == ProtocolSpeech || inbound == ProtocolTranscriptions {
		return fmt.Sprintf("模型 %s 的有效出站协议是 %s，与 %s 入站不相通："+
			"语音只做同协议直通，音频字节与消息序列之间没有可转换的映射；"+
			"语音请求用 POST %s 发，%s 的请求改用走其它协议的模型",
			model, outbound, inbound, speechInboundPath(inbound), inbound)
	}
	return fmt.Sprintf("模型 %s 的有效出站协议是 %s，与 %s 入站不相通："+
		"语音只做同协议直通，没有消息序列可以经中间表示转换；"+
		"对话请求用 POST %s 发，或改用走其它协议的模型",
		model, outbound, inbound, inboundPath(inbound))
}
