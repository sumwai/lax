package gateway

import (
	"fmt"
	"strings"

	"lax/internal/account"
	"lax/internal/store"
)

// 语音的能力声明校验。它回答的是「这个模型能不能用于这个端点」，与上游形状无关：
// 上游形状由方言决定（见 speech_dialect.go）。
//
// 校验在任何上游调用之前：没声明能力的模型在这里就被拒，不会因为客户端打错一个端点
// 就产生一次计费调用。能力位与音色、格式都是手工声明的，目录同步拿不到，
// 因此错误文案给出 `lax model capability add` 的完整命令。

// checkSpeechInbound 校验语音入站请求：模型必须声明对应能力，音色与格式必须在声明里。
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
			"否则任何对话模型都会被语音请求打到上游；先运行 lax model capability add %s --capability %s --dialect <方言>",
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
		if len(formats) > 0 && !containsSpeechFormat(formats, tts.Format) {
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

// containsSpeechFormat 判断入站要的音频格式是否在声明里。声明写的是上游的叫法
// （例如 pcm16），入站用的是 OpenAI 的叫法（pcm），两边按方言转换后的名字比。
func containsSpeechFormat(formats []string, inbound string) bool {
	if contains(formats, inbound) {
		return true
	}
	upstream := upstreamSpeechFormat(inbound)
	return upstream != inbound && contains(formats, upstream)
}

// upstreamSpeechFormat 把入站格式折成上游的 audio.format：pcm 是 OpenAI 的叫法，
// 上游叫 pcm16，两者的采样规格相同。
func upstreamSpeechFormat(format string) string {
	if format == "pcm" {
		return "pcm16"
	}
	return format
}
