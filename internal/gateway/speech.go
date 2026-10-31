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

// speechRequest 是 TTS 入站的解析结果：模型名用于选号与能力校验，字符数用于记账，
// 其余字段供 chat 适配器拼上游请求。
type speechRequest struct {
	Model string
	Chars int64
	// Stream 表示客户端要 SSE 事件流（stream_format: sse）。本版不做事件转换，
	// 解析时直接拒绝。
	Stream bool
	// Input 是要合成的正文：input 是数组时按换行拼接。
	Input string
	// Instructions 是 OpenAI 的风格指令，适配时进 chat 的 user message。
	Instructions string
	Voice        string
	// Format 是客户端要的音频容器（mp3 / wav / pcm），ResponseFormat 同值，
	// 供适配器据此挑上游格式与写 Content-Type。
	Format string
}

// decodeSpeechRequest 做入站校验并解析出适配要用的字段。
//
// 校验的是本机真能表达的几件事：model 非空、input 非空、response_format 是基础三种之一、
// stream_format 不是 sse。音频格式不在这里转码（本版不做转码），
// 所以 opus/aac/flac 直接拒，而不是静默换一个格式让客户端解错码。
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
	text, chars, err := speechInput(input)
	if err != nil {
		return nil, err
	}
	if chars == 0 {
		return nil, errors.New("input 是空的：没有文本就没有可合成的内容，空 input 通常来自变量没填上")
	}

	voice, err := optionalString(fields, "voice")
	if err != nil {
		return nil, err
	}
	instructions, err := optionalString(fields, "instructions")
	if err != nil {
		return nil, err
	}

	// OpenAI 的 response_format 默认 mp3。本版只支持上游原样能给的三种，
	// opus/aac/flac 需要转码，等转码落地再放开。
	format, err := optionalString(fields, "response_format")
	if err != nil {
		return nil, err
	}
	switch format {
	case "", "mp3", "wav", "pcm":
		if format == "" {
			format = "mp3"
		}
	case "opus", "aac", "flac":
		return nil, fmt.Errorf("response_format %q 需要把上游音频转码，本版不做转码："+
			"改成 mp3、wav 或 pcm 再发", format)
	default:
		return nil, fmt.Errorf("response_format 是 %q，可用的是 mp3、wav、pcm", format)
	}

	streamFormat, err := optionalString(fields, "stream_format")
	if err != nil {
		return nil, err
	}
	switch streamFormat {
	case "", "audio":
	case "sse":
		return nil, errors.New("stream_format: sse 需要把上游音频重新包成 OpenAI 的 SSE 事件，本版不做：" +
			"去掉 stream_format 拿完整音频，或写 audio")
	default:
		return nil, fmt.Errorf("stream_format 是 %q，只认 audio 或 sse", streamFormat)
	}

	return &speechRequest{
		Model: model, Chars: chars, Input: text, Instructions: instructions,
		Voice: voice, Format: format,
	}, nil
}

// optionalString 读一个可选的字符串字段：缺失与 null 都返回空串，类型不对则报错。
func optionalString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok || isJSONNull(raw) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s 必须是字符串：写对象或数组都读不出来", name)
	}
	return value, nil
}

// speechInput 把 input 取成一段正文并数出字符数：字符串原样，字符串数组按换行拼接。
// 校验与计数在同一次解析里做，调用方拿到非零字符数就说明 input 合法。
func speechInput(input json.RawMessage) (string, int64, error) {
	var text string
	if err := json.Unmarshal(input, &text); err == nil {
		return text, int64(utf8.RuneCountInString(text)), nil
	}
	var parts []string
	if err := json.Unmarshal(input, &parts); err == nil {
		var total int64
		for _, part := range parts {
			total += int64(utf8.RuneCountInString(part))
		}
		return strings.Join(parts, "\n"), total, nil
	}
	return "", 0, errors.New("input 必须是字符串或字符串数组：数字、对象与布尔值都合不出文本，" +
		"`\"input\": 123` 这类写法在上游也只会换回 400")
}

// decodeTranscriptionsRequest 做最少校验，只返回模型名；请求体本身留在 raw 里，
// 等别名解析完成、真名确定之后再重写 model 那个 part（见 rewriteTranscriptionsBody）。
//
// 判三件事：Content-Type 是 multipart/form-data 且带 boundary、有 model part、有非空的 file part。
// 其余字段（language、prompt、response_format、temperature、stream 一类）原样转发。
// transcriptionsRequest 是 ASR 入站的解析结果：模型名用于选号与能力校验，
// 音频字节与语言供 chat 适配器拼上游请求，Format 是客户端要的响应形状。
type transcriptionsRequest struct {
	Model    string
	Language string
	// Format 是 response_format 的取值：json、text 或 verbose_json。
	Format string
	// Container 是入站音频识别出的容器：wav 或 mp3（本版不做转码，别的直接拒）。
	Container string
	Audio     []byte
}

// decodeTranscriptionsRequest 做入站校验并取回音频字节。
//
// 本版不做转码，所以入站只收 wav 与 mp3：ogg/opus（Telegram）与 SILK（微信）
// 需要先解码再发，文案里说清楚，而不是静默送给上游换回一个 400。
// 流式转写本版不做：客户端要 SSE 时直接拒。
func decodeTranscriptionsRequest(raw []byte, contentType string) (*transcriptionsRequest, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return nil, err
	}

	out := &transcriptionsRequest{Format: "json"}
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	stream := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 multipart 失败: %v", err)
		}
		switch part.FormName() {
		case "model":
			value, err := io.ReadAll(io.LimitReader(part, 4096))
			if err != nil {
				return nil, fmt.Errorf("读取 model 字段失败: %v", err)
			}
			if out.Model == "" {
				out.Model = string(value)
			}
		case "file":
			data, err := io.ReadAll(io.LimitReader(part, speechMaxBody))
			if err != nil {
				return nil, fmt.Errorf("读取 file 字段失败: %v", err)
			}
			if out.Audio == nil {
				out.Audio = data
			}
		case "language":
			value, err := io.ReadAll(io.LimitReader(part, 4096))
			if err != nil {
				return nil, fmt.Errorf("读取 language 字段失败: %v", err)
			}
			if out.Language == "" {
				out.Language = strings.TrimSpace(string(value))
			}
		case "response_format":
			value, err := io.ReadAll(io.LimitReader(part, 4096))
			if err != nil {
				return nil, fmt.Errorf("读取 response_format 字段失败: %v", err)
			}
			if format := strings.TrimSpace(string(value)); format != "" {
				out.Format = format
			}
		case "stream":
			value, err := io.ReadAll(io.LimitReader(part, 4096))
			if err == nil && strings.TrimSpace(string(value)) == "true" {
				stream = true
			}
		}
		_ = part.Close()
	}

	if strings.TrimSpace(out.Model) == "" {
		return nil, errors.New("缺少 model 字段：语音转写请求按模型名选号，" +
			"multipart 里要有一个名为 model 的表单字段")
	}
	if len(out.Audio) == 0 {
		return nil, errors.New("缺少 file 字段：要转写的音频写在名为 file 的文件字段里，" +
			"只写 model 不发音频的话上游只会换回 400")
	}
	if stream {
		return nil, errors.New("stream: true 需要把上游的逐字输出重包成 OpenAI 的 SSE 事件，本版不做：" +
			"去掉 stream 拿一次完整结果")
	}

	out.Container = detectAudioContainer(out.Audio)
	switch out.Container {
	case "wav", "mp3":
	case "ogg":
		return nil, errors.New("入站音频是 ogg/opus（Telegram 语音常用），要转成 wav 或 mp3 才能交给上游：" +
			"本版不做转码，先在客户端转成 wav/mp3 再发")
	case "silk":
		return nil, errors.New("入站音频是 SILK（微信语音常用），要解码成 wav 或 mp3 才能交给上游：" +
			"本版不做转码、也不内置 SILK 解码器，先在客户端转成 wav/mp3 再发")
	default:
		return nil, errors.New("file 不是可识别的音频：只认 wav（RIFF/WAVE 头）与 mp3（ID3 或帧同步头）")
	}

	switch out.Format {
	case "json", "text", "verbose_json":
	case "srt", "vtt":
		return nil, fmt.Errorf("response_format %q 需要时间轴，上游不返回分段信息："+
			"改用 json、text 或 verbose_json，字幕文件在本地从文本生成", out.Format)
	default:
		return nil, fmt.Errorf("response_format 是 %q，可用的是 json、text、verbose_json", out.Format)
	}

	switch out.Language {
	case "", "auto", "zh", "en":
	default:
		return nil, fmt.Errorf("language %q 不受上游支持：只认 auto、zh、en，"+
			"其它语言先去掉这个字段让上游自动判断", out.Language)
	}

	return out, nil
}

// detectAudioContainer 按文件头认容器。只认本版真能送上游的两种，
// 另两种（ogg、silk）单独认出来是为了给一句能照着做的错误文案。
func detectAudioContainer(data []byte) string {
	switch {
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "wav"
	case len(data) >= 3 && string(data[0:3]) == "ID3":
		return "mp3"
	case len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return "mp3"
	case len(data) >= 4 && string(data[0:4]) == "OggS":
		return "ogg"
	case bytes.HasPrefix(data, []byte("\x02#!SILK")) || bytes.HasPrefix(data, []byte("#!SILK")):
		return "silk"
	}
	return ""
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

// speechOutbound 给语音请求选协议。三种情况，按优先级：
//
//  1. 厂商声明了这条语音路由：同协议直通（真正的 OpenAI audio 端点）；
//  2. 没声明语音路由、但模型声明了对应能力、且有效协议是 openai-chat：走 chat 适配
//     （MiMo 那种「语音做成 chat 方言」的上游）；
//  3. 其余情况不相通，返回给客户端的文案。
//
// 能力声明必须先有：它回答的是「这个模型能不能用于这个端点」，与路由是否存在是两件事。
func (s *Server) speechOutbound(a store.Account, inbound, model string) (string, string) {
	capability := store.CapabilitySpeech
	if inbound == ProtocolTranscriptions {
		capability = store.CapabilityTranscription
	}
	pm, ok, err := s.store.ProviderModel(a.Provider, model)
	if err != nil {
		s.logf("读厂商 %s 的模型 %s 能力声明失败: %v", a.Provider, model, err)
		return "", fmt.Sprintf("账号 %s：读厂商 %s 的能力声明失败（%v），先运行 lax doctor 检查数据库",
			a.Name, a.Provider, err)
	}
	if !ok || !store.HasCapability(pm, capability) {
		return "", fmt.Sprintf("账号 %s 的厂商 %s 没有声明模型 %s 的 %s 能力："+
			"用 lax model capability add %s --capability %s 声明后重试",
			a.Name, a.Provider, model, capability, model, capability)
	}

	route, ok, err := s.store.LookupProviderRoute(a.Provider, inbound)
	if err != nil {
		s.logf("读厂商 %s 的 %s 路由失败: %v", a.Provider, inbound, err)
		return "", fmt.Sprintf("账号 %s：读厂商 %s 的 %s 路由失败（%v），先运行 lax doctor 检查数据库",
			a.Name, a.Provider, inbound, err)
	}
	if ok && strings.TrimSpace(route.Path) != "" {
		return inbound, ""
	}

	proto, err := s.store.EffectiveProtocol(a.ID, model)
	if err != nil {
		s.logf("读账号 %s 模型 %s 的有效协议失败: %v", a.Name, model, err)
		return "", fmt.Sprintf("账号 %s：读有效出站协议失败（%v），先运行 lax doctor 检查数据库", a.Name, err)
	}
	if proto == ProtocolChat {
		return ProtocolChat, ""
	}
	return "", speechNoRouteMessage(a.Provider, inbound) + fmt.Sprintf("（模型 %s 的有效出站协议是 %s，只支持经 openai-chat 适配）", model, proto)
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
