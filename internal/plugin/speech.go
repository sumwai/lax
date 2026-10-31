package plugin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 内置方言。它们随二进制发布，不经 plugin install 安装，ID 用 lax/ 前缀。
//
// MiMo 的语音是 chat completions 的一种形状：文本放 messages、音频用 audio 对象描述、
// 响应里的音频是 base64。方言只描述这个形状，发请求、选账号、记账、错误形状仍归宿主。

// SystemDialects 返回内置方言，顺序固定。
func SystemDialects() []Dialect {
	out := []Dialect{
		passthroughDialect{id: DialectOpenAISpeech, op: OpSpeechSynthesize, wire: WireSpeech},
		passthroughDialect{id: DialectOpenAITranscriptions, op: OpSpeechTranscribe, wire: WireTranscriptions},
		mimoChatSpeech{},
		mimoChatASR{},
	}
	// 对话、向量化与决策协议的编解码在 internal/translate 里，绑定它们的方言只用来标记
	// 「这条操作由宿主处理」：插件目录里引用了它们，就说明不需要插件提供映射。
	for _, wire := range []string{WireAnthropic, WireOpenAIChat, WireOpenAIResponses, WireGemini, WireEmbeddings, WireSystemOne} {
		out = append(out, passthroughDialect{id: WireDialect(wire), op: OperationForInbound(wire), wire: wire})
	}
	return out
}

// passthroughDialect 是同协议直通：宿主把入站报文原样转发，只替换模型名。
type passthroughDialect struct {
	id   string
	op   string
	wire string
}

func (d passthroughDialect) ID() string           { return d.id }
func (d passthroughDialect) Operation() string    { return d.op }
func (d passthroughDialect) UpstreamWire() string { return d.wire }

// Prepare 与 Decode 不会被调用：同协议直通与宿主实现线协议的请求都由宿主自己处理。
func (d passthroughDialect) Prepare(Input) (Plan, error) {
	return Plan{}, fmt.Errorf("方言 %s 由宿主直接编解码，不该经方言构造", d.id)
}

func (d passthroughDialect) Decode(Input, Response) (Result, error) {
	return Result{}, fmt.Errorf("方言 %s 由宿主直接编解码，不该经方言解码", d.id)
}

// mimoChatSpeech 是 TTS 的 chat 方言。
type mimoChatSpeech struct{}

func (mimoChatSpeech) ID() string           { return DialectMiMoChatSpeech }
func (mimoChatSpeech) Operation() string    { return OpSpeechSynthesize }
func (mimoChatSpeech) UpstreamWire() string { return WireOpenAIChat }

// Prepare 拼 messages 与 audio 对象：instructions 落到 user 消息，正文落到 assistant 消息。
func (mimoChatSpeech) Prepare(in Input) (Plan, error) {
	messages := make([]map[string]any, 0, 2)
	if strings.TrimSpace(in.Instructions) != "" {
		messages = append(messages, map[string]any{"role": "user", "content": in.Instructions})
	}
	messages = append(messages, map[string]any{"role": "assistant", "content": in.Text})

	audio := map[string]any{"format": upstreamSpeechFormat(in.Format)}
	if voice := strings.TrimSpace(in.Voice); voice != "" {
		audio["voice"] = voice
	}
	return Plan{JSON: map[string]any{
		"model":    in.Model,
		"messages": messages,
		"audio":    audio,
	}}, nil
}

// Decode 取出 choices[0].message.audio.data 的 base64 音频。
func (mimoChatSpeech) Decode(in Input, resp Response) (Result, error) {
	env, err := decodeChatEnvelope(resp.Body)
	if err != nil {
		return Result{}, err
	}
	if env.audio == "" {
		return Result{}, errors.New("上游没有返回音频（choices[0].message.audio.data 为空）：" +
			"确认这个模型确实是语音合成模型，用 lax model ls 看它的能力声明")
	}
	audio, err := base64.StdEncoding.DecodeString(env.audio)
	if err != nil {
		return Result{}, fmt.Errorf("上游返回的音频不是合法 base64: %v", err)
	}
	if len(audio) == 0 {
		return Result{}, errors.New("上游返回了空的音频")
	}
	return Result{
		Kind:  ResultMedia,
		MIME:  speechMIME(in.Format),
		Media: audio,
		Usage: env.usage,
	}, nil
}

// mimoChatASR 是转写的 chat 方言。
type mimoChatASR struct{}

func (mimoChatASR) ID() string           { return DialectMiMoChatASR }
func (mimoChatASR) Operation() string    { return OpSpeechTranscribe }
func (mimoChatASR) UpstreamWire() string { return WireOpenAIChat }

// Prepare 把音频包成 input_audio 的 data URL，语言进 asr_options。
func (mimoChatASR) Prepare(in Input) (Plan, error) {
	if len(in.Audio) == 0 {
		return Plan{}, errors.New("转写需要音频字节：入站的 file 字段是空的")
	}
	mime := "audio/wav"
	if in.AudioContainer == "mp3" {
		mime = "audio/mpeg"
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(in.Audio)

	language := in.Language
	if language == "" {
		language = "auto"
	}
	return Plan{JSON: map[string]any{
		"model": in.Model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": dataURL},
			}},
		}},
		"asr_options": map[string]any{"language": language},
	}}, nil
}

// Decode 把 choices[0].message.content 包成客户端要的 response_format，秒数取 usage.seconds。
func (mimoChatASR) Decode(in Input, resp Response) (Result, error) {
	env, err := decodeChatEnvelope(resp.Body)
	if err != nil {
		return Result{}, err
	}
	if !env.hasChoice {
		return Result{}, errors.New("上游没有返回转写结果（choices 为空）：确认这个模型确实是语音识别模型")
	}

	result := Result{Usage: env.usage}
	switch in.Format {
	case "text":
		result.Kind = ResultText
		result.Text = env.content
	case "verbose_json":
		language := in.Language
		if language == "" {
			language = "auto"
		}
		payload, err := json.Marshal(map[string]any{
			"text":     env.content,
			"task":     "transcribe",
			"language": language,
			"duration": env.usage.AudioSeconds,
		})
		if err != nil {
			return Result{}, fmt.Errorf("序列化转写结果失败: %v", err)
		}
		result.Kind = ResultJSON
		result.JSON = payload
	default:
		payload, err := json.Marshal(map[string]string{"text": env.content})
		if err != nil {
			return Result{}, fmt.Errorf("序列化转写结果失败: %v", err)
		}
		result.Kind = ResultJSON
		result.JSON = payload
	}
	return result, nil
}

// chatEnvelope 是 chat completion 里方言要读的几项：正文、音频 base64 与用量。
type chatEnvelope struct {
	hasChoice bool
	content   string
	audio     string
	usage     Usage
}

func decodeChatEnvelope(body []byte) (chatEnvelope, error) {
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Audio   *struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			Seconds float64 `json:"seconds"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return chatEnvelope{}, fmt.Errorf("上游响应不是 JSON: %v", err)
	}
	out := chatEnvelope{
		hasChoice: len(env.Choices) > 0,
		usage: Usage{
			In:           env.Usage.PromptTokens,
			Out:          env.Usage.CompletionTokens,
			CacheRead:    env.Usage.PromptTokensDetails.CachedTokens,
			Total:        env.Usage.TotalTokens,
			AudioSeconds: int64(env.Usage.Seconds + 0.5),
			Source:       "upstream",
		},
	}
	if out.hasChoice {
		out.content = env.Choices[0].Message.Content
		if env.Choices[0].Message.Audio != nil {
			out.audio = env.Choices[0].Message.Audio.Data
		}
	}
	return out, nil
}

// upstreamSpeechFormat 把入站格式折成上游的 audio.format。
// pcm 是 OpenAI 的叫法，上游叫 pcm16；两者都是 24 kHz 单声道 16 位。
func upstreamSpeechFormat(format string) string {
	if format == "pcm" {
		return "pcm16"
	}
	return format
}

// speechMIME 给音频结果挑 Content-Type。入站只放行 mp3、wav、pcm 三种。
func speechMIME(format string) string {
	switch format {
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "audio/mpeg"
	}
}
