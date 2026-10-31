package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"lax/internal/account"
	"lax/internal/store"
)

// 语音到 chat 的适配层。
//
// 有些上游没有 OpenAI 的 audio 端点，而是把语音做成 chat completions 的一种方言：
// 文本放 messages、音频用 audio 对象描述、响应里的音频是 base64。MiMo 的
// mimo-v2.5-tts 与 mimo-v2.5-asr 就是这种形状（实测见 DESIGN 5.16）。
//
// 链路是两个方向都转：
//
//	用户 → POST /v1/audio/speech (audio) → 适配 → POST /v1/chat/completions (chat) → 上游
//	用户 ← 音频字节 (audio)            ← 适配 ← chat completion (chat)             ← 上游
//
// 适配只在两件事同时成立时发生：入站是语音协议、出站是 openai-chat，
// 且模型用 `model capability` 声明了对应能力（见 checkSpeechInbound）。
// 别的组合仍然 400：语音与对话之间没有通用的转换，这里转的是特定方言，
// 不是一个可以套到任意 chat 模型上的映射。

// speechNeedsChatAdapter 判断这次请求要不要走 chat 适配。
func speechNeedsChatAdapter(inbound, outbound string) bool {
	if outbound != ProtocolChat {
		return false
	}
	return inbound == ProtocolSpeech || inbound == ProtocolTranscriptions
}

// encodeSpeechAsChat 把语音入站的原始请求编成 chat completions 的请求体。
//
// TTS 的映射：
//   - input → messages 里 assistant 的正文（input 是数组时按换行拼接）；
//   - instructions → messages 里 user 的风格指令，没有就省略这条消息；
//   - voice → audio.voice 原样，空则省略（用上游默认音色）；
//   - response_format → audio.format：mp3 → mp3、wav → wav、pcm → pcm16。
//
// ASR 的映射：
//   - file → messages[0].content 里唯一的 input_audio part（data URL，base64）；
//   - language → asr_options.language（auto / zh / en）；
//   - prompt、temperature 丢弃：上游不接受 text part（实测 400）。
func (s *Server) encodeSpeechAsChat(inbound string, raw []byte, model, contentType string) ([]byte, error) {
	switch inbound {
	case ProtocolSpeech:
		sp, err := decodeSpeechRequest(raw)
		if err != nil {
			return nil, err
		}
		messages := make([]map[string]any, 0, 2)
		if strings.TrimSpace(sp.Instructions) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": sp.Instructions})
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": sp.Input})
		audio := map[string]any{"format": upstreamSpeechFormat(sp.Format)}
		if v := strings.TrimSpace(sp.Voice); v != "" {
			audio["voice"] = v
		}
		return json.Marshal(map[string]any{
			"model":    model,
			"messages": messages,
			"audio":    audio,
		})
	case ProtocolTranscriptions:
		asr, err := decodeTranscriptionsRequest(raw, contentType)
		if err != nil {
			return nil, err
		}
		mime := "audio/wav"
		if asr.Container == "mp3" {
			mime = "audio/mpeg"
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(asr.Audio)
		body := map[string]any{
			"model": model,
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{{
					"type":        "input_audio",
					"input_audio": map[string]any{"data": dataURL},
				}},
			}},
		}
		language := asr.Language
		if language == "" {
			language = "auto"
		}
		body["asr_options"] = map[string]any{"language": language}
		return json.Marshal(body)
	}
	return nil, fmt.Errorf("协议 %s 没有 chat 适配器", inbound)
}

// upstreamSpeechFormat 把入站 response_format 折成上游 audio.format。
// pcm 是 OpenAI 的叫法，上游叫 pcm16；两者的采样规格都是 24 kHz 单声道 16 位。
func upstreamSpeechFormat(format string) string {
	if format == "pcm" {
		return "pcm16"
	}
	return format
}

// chatSpeechSuccess 处理 chat 适配的成功响应：把 chat completion 解成语音入站要的形状。
//
// 非 2xx 由调用方按既有规则处理（上游错误体的形状是 openai 的，与语音入站一致）。
// 这里只处理 2xx；响应体读不全时按连接层失败处理。
func (s *Server) chatSpeechSuccess(w http.ResponseWriter, resp *http.Response, inbound string, raw []byte, contentType string) attempt {
	body, err := io.ReadAll(io.LimitReader(resp.Body, speechMaxBody))
	if err != nil {
		return attempt{status: http.StatusBadGateway, transport: true,
			message: "读取上游响应失败: " + err.Error(), err: err}
	}

	switch inbound {
	case ProtocolSpeech:
		sp, err := decodeSpeechRequest(raw)
		if err != nil {
			return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
		}
		audio, err := chatSpeechAudio(body)
		if err != nil {
			return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
		}
		contentType := speechContentTypeFor(sp.Format)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audio)
		usage := chatUsage(body)
		return attempt{
			status: http.StatusOK, wrote: true,
			inTok: usage.In, outTok: usage.Out, cacheRead: usage.CacheRead, total: usage.Total,
			ttsChars: sp.Chars,
		}

	case ProtocolTranscriptions:
		asr, err := decodeTranscriptionsRequest(raw, contentType)
		if err != nil {
			// 适配上一步已经校验过；走到这里说明请求在两次解析之间被改过，或者
			// 内部逻辑不一致。按 502 报，不编一个空结果。
			return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
		}
		payload, contentType, seconds, err := chatTranscriptionPayload(body, asr)
		if err != nil {
			return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
		usage := chatUsage(body)
		return attempt{
			status: http.StatusOK, wrote: true,
			inTok: usage.In, outTok: usage.Out, cacheRead: usage.CacheRead, total: usage.Total,
			audioSeconds: seconds,
		}
	}
	return attempt{status: http.StatusBadGateway, message: "协议 " + inbound + " 没有 chat 适配器"}
}

// speechContentTypeFor 给客户端要的容器挑 Content-Type。
func speechContentTypeFor(format string) string {
	switch format {
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "audio/mpeg"
	}
}

// chatSpeechAudio 从 chat completion 里取出 base64 音频并解码。
// 没有 audio.data 说明这个模型没有产出音频——多半是模型选错了（能力声明过但模型其实是别的）。
func chatSpeechAudio(body []byte) ([]byte, error) {
	var env struct {
		Choices []struct {
			Message struct {
				Audio *struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("上游响应不是 JSON: %v", err)
	}
	if len(env.Choices) == 0 || env.Choices[0].Message.Audio == nil || env.Choices[0].Message.Audio.Data == "" {
		return nil, fmt.Errorf("上游没有返回音频（choices[0].message.audio.data 为空）：" +
			"确认这个模型确实是语音合成模型，用 lax model ls 看它的能力声明")
	}
	audio, err := base64.StdEncoding.DecodeString(env.Choices[0].Message.Audio.Data)
	if err != nil {
		return nil, fmt.Errorf("上游返回的音频不是合法 base64: %v", err)
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("上游返回了空的音频")
	}
	return audio, nil
}

// chatTranscriptionPayload 把 chat completion 的正文包成客户端要的 response_format。
// seconds 取上游的 usage.seconds（实测有值），没有就记 0。
func chatTranscriptionPayload(body []byte, asr *transcriptionsRequest) ([]byte, string, int64, error) {
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Seconds float64 `json:"seconds"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", 0, fmt.Errorf("上游响应不是 JSON: %v", err)
	}
	if len(env.Choices) == 0 {
		return nil, "", 0, fmt.Errorf("上游没有返回转写结果（choices 为空）：确认这个模型确实是语音识别模型")
	}
	text := env.Choices[0].Message.Content
	seconds := int64(env.Usage.Seconds + 0.5)

	switch asr.Format {
	case "text":
		return []byte(text), "text/plain; charset=utf-8", seconds, nil
	case "verbose_json":
		language := asr.Language
		if language == "" || language == "auto" {
			language = "auto"
		}
		payload, err := json.Marshal(map[string]any{
			"text":     text,
			"task":     "transcribe",
			"language": language,
			"duration": seconds,
		})
		return payload, "application/json", seconds, err
	default:
		payload, err := json.Marshal(map[string]string{"text": text})
		return payload, "application/json", seconds, err
	}
}

// chatUsage 读 chat completion 的 token 用量。缓存读只认 prompt_tokens_details.cached_tokens，
// 与对话路径同一套读法；没有 total 时按 0 处理，调用方的 ReasoningExtra 会按缺省判。
type usageNumbers struct {
	In, Out, CacheRead, Total int64
}

func chatUsage(body []byte) usageNumbers {
	var env struct {
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return usageNumbers{}
	}
	return usageNumbers{
		In:        env.Usage.PromptTokens,
		Out:       env.Usage.CompletionTokens,
		CacheRead: env.Usage.PromptTokensDetails.CachedTokens,
		Total:     env.Usage.TotalTokens,
	}
}

// checkSpeechInbound 校验语音入站请求：模型必须声明对应能力，音色与格式必须在声明里。
//
// 这一步在任何上游调用之前：没声明能力的模型（例如 deepseek-v4.1-flash）在这里就被拒，
// 不会因为客户端打错一个端点就产生一次计费调用。能力位是手工声明的（目录同步拿不到），
// 因此错误文案给出 `lax model capability add` 的完整命令。
func (s *Server) checkSpeechInbound(inbound string, accounts []store.Account, model string,
	tts *speechRequest, asr *transcriptionsRequest) error {

	capability := store.CapabilitySpeech
	if inbound == ProtocolTranscriptions {
		capability = store.CapabilityTranscription
	}

	providers := account.Providers(accounts, model)
	if len(providers) == 0 {
		return fmt.Errorf("模型 %s 没有账号提供：语音请求也按模型名选号，先用 lax model ls 看现有清单，"+
			"或 lax model sync --account <账号> 把这个账号的模型拉进来", model)
	}
	rows := make([]store.ProviderModel, 0, len(providers))
	for _, a := range providers {
		pm, ok, err := s.store.ProviderModel(a.Provider, model)
		if err != nil {
			return fmt.Errorf("读模型 %s 的能力声明失败: %v；先运行 lax doctor 检查数据库", model, err)
		}
		if ok && store.HasCapability(pm, capability) {
			rows = append(rows, pm)
		}
	}
	if len(rows) == 0 {
		hint := "speech --voices <音色[,音色...]> --audio-formats mp3,wav,pcm"
		if capability == store.CapabilityTranscription {
			hint = "transcription --audio-formats wav,mp3"
		}
		return fmt.Errorf("模型 %s 没有声明 %s 能力：语音入站只打声明过这项能力的模型，"+
			"否则任何对话模型都会被语音请求打到上游；先运行 lax model capability add %s --capability %s",
			model, capability, model, hint)
	}

	if tts != nil {
		voices := voiceUnion(rows)
		if v := strings.TrimSpace(tts.Voice); v != "" && len(voices) > 0 && !contains(voices, v) {
			return fmt.Errorf("音色 %q 不在模型 %s 声明的音色列表里，可用的是 %s；"+
				"音色是上游的取值，要加新音色用 lax model capability add %s --voices %s",
				v, model, strings.Join(voices, "、"), model, v)
		}
		formats := formatUnion(rows)
		if len(formats) > 0 && !contains(formats, tts.Format) {
			return fmt.Errorf("response_format %q 不在模型 %s 声明的音频格式里，可用的是 %s；"+
				"要加新格式用 lax model capability add %s --audio-formats %s（本版不做转码，声明的必须是上游原样能给的）",
				tts.Format, model, strings.Join(formats, "、"), model, tts.Format)
		}
	}

	if asr != nil {
		formats := formatUnion(rows)
		if len(formats) > 0 && !contains(formats, asr.Container) {
			return fmt.Errorf("入站音频是 %s，不在模型 %s 声明的输入格式里（%s）；"+
				"本版不做转码，先在客户端转成声明过的格式再发",
				asr.Container, model, strings.Join(formats, "、"))
		}
	}
	return nil
}

// voiceUnion 把若干目录行的音色并起来，保持出现顺序。
func voiceUnion(rows []store.ProviderModel) []string {
	var out []string
	seen := map[string]bool{}
	for _, row := range rows {
		for _, v := range row.Voices {
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// formatUnion 把若干目录行的音频格式并起来。
func formatUnion(rows []store.ProviderModel) []string {
	var out []string
	seen := map[string]bool{}
	for _, row := range rows {
		for _, v := range row.AudioFormats {
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
