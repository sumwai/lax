package plugin_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"lax/internal/plugin"
)

// 内置方言的行为对拍：这些形状来自改造前的网关实现，方言重构后必须逐条一致。

func systemDialect(t *testing.T, id string) plugin.Dialect {
	t.Helper()
	for _, d := range plugin.SystemDialects() {
		if d.ID() == id {
			return d
		}
	}
	t.Fatalf("内置方言里没有 %s", id)
	return nil
}

func TestSystemDialectsAreComplete(t *testing.T) {
	want := map[string]struct{ op, wire string }{
		plugin.DialectOpenAISpeech:         {plugin.OpSpeechSynthesize, plugin.WireSpeech},
		plugin.DialectOpenAITranscriptions: {plugin.OpSpeechTranscribe, plugin.WireTranscriptions},
		plugin.DialectMiMoChatSpeech:       {plugin.OpSpeechSynthesize, plugin.WireOpenAIChat},
		plugin.DialectMiMoChatASR:          {plugin.OpSpeechTranscribe, plugin.WireOpenAIChat},
		plugin.DialectHostAnthropic:        {plugin.OpChatComplete, plugin.WireAnthropic},
		plugin.DialectHostOpenAIChat:       {plugin.OpChatComplete, plugin.WireOpenAIChat},
		plugin.DialectHostOpenAIResponses:  {plugin.OpChatComplete, plugin.WireOpenAIResponses},
		plugin.DialectHostGemini:           {plugin.OpChatComplete, plugin.WireGemini},
		plugin.DialectHostEmbeddings:       {plugin.OpEmbeddingsCreate, plugin.WireEmbeddings},
		plugin.DialectHostSystemOne:        {plugin.OpSystemDecide, plugin.WireSystemOne},
	}
	for _, d := range plugin.SystemDialects() {
		expect, ok := want[d.ID()]
		if !ok {
			t.Fatalf("多了一个内置方言 %s", d.ID())
		}
		if d.Operation() != expect.op || d.UpstreamWire() != expect.wire {
			t.Fatalf("%s 的 operation/wire = %s/%s，期望 %s/%s",
				d.ID(), d.Operation(), d.UpstreamWire(), expect.op, expect.wire)
		}
		delete(want, d.ID())
	}
	for id := range want {
		t.Fatalf("缺少内置方言 %s", id)
	}
}

func TestMiMoSpeechPrepare(t *testing.T) {
	d := systemDialect(t, plugin.DialectMiMoChatSpeech)

	plan, err := d.Prepare(plugin.Input{Model: "mimo-v2.5-tts", Text: "你好世界", Voice: "Mia", Format: "mp3"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	messages, _ := plan.JSON["messages"].([]map[string]any)
	if len(messages) != 1 || messages[0]["role"] != "assistant" || messages[0]["content"] != "你好世界" {
		t.Fatalf("messages = %+v，期望只有一条 assistant 带正文", plan.JSON["messages"])
	}
	audio, _ := plan.JSON["audio"].(map[string]any)
	if audio["format"] != "mp3" || audio["voice"] != "Mia" {
		t.Fatalf("audio = %+v，期望 mp3 + Mia", audio)
	}
	if plan.JSON["model"] != "mimo-v2.5-tts" {
		t.Fatalf("model = %v", plan.JSON["model"])
	}

	// instructions 落在 user 消息里，正文仍是 assistant。
	plan, err = d.Prepare(plugin.Input{Model: "m", Text: "正文", Voice: "Mia", Format: "mp3", Instructions: "轻快上扬"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	messages, _ = plan.JSON["messages"].([]map[string]any)
	if len(messages) != 2 || messages[0]["role"] != "user" || messages[0]["content"] != "轻快上扬" ||
		messages[1]["role"] != "assistant" || messages[1]["content"] != "正文" {
		t.Fatalf("messages = %+v，期望 user 指令 + assistant 正文", messages)
	}

	// pcm 要翻成上游的 pcm16；没给音色时不写 voice 字段。
	plan, err = d.Prepare(plugin.Input{Model: "m", Text: "hi", Format: "pcm"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	audio, _ = plan.JSON["audio"].(map[string]any)
	if audio["format"] != "pcm16" {
		t.Fatalf("audio.format = %v，期望 pcm16", audio["format"])
	}
	if _, ok := audio["voice"]; ok {
		t.Fatalf("没给音色却写了 voice：%+v", audio)
	}
}

func TestMiMoSpeechDecode(t *testing.T) {
	d := systemDialect(t, plugin.DialectMiMoChatSpeech)
	audio := []byte("ID3-fake-audio")
	body := `{"choices":[{"message":{"audio":{"data":"` + base64.StdEncoding.EncodeToString(audio) + `"}}}],` +
		`"usage":{"prompt_tokens":125,"completion_tokens":7,"total_tokens":132,` +
		`"prompt_tokens_details":{"cached_tokens":121}}}`

	result, err := d.Decode(plugin.Input{Format: "mp3"}, plugin.Response{Status: 200, Body: []byte(body)})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.Kind != plugin.ResultMedia || result.MIME != "audio/mpeg" || string(result.Media) != string(audio) {
		t.Fatalf("结果 = %+v，期望 audio bytes 与 audio/mpeg", result)
	}
	if result.Usage.In != 125 || result.Usage.Out != 7 || result.Usage.CacheRead != 121 || result.Usage.Total != 132 {
		t.Fatalf("用量 = %+v，期望 125/7/121/132", result.Usage)
	}

	result, err = d.Decode(plugin.Input{Format: "pcm"}, plugin.Response{Status: 200, Body: []byte(body)})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.MIME != "audio/pcm" {
		t.Fatalf("pcm 的 MIME = %q，期望 audio/pcm", result.MIME)
	}
}

func TestMiMoSpeechDecodeErrors(t *testing.T) {
	d := systemDialect(t, plugin.DialectMiMoChatSpeech)

	_, err := d.Decode(plugin.Input{Format: "mp3"}, plugin.Response{Status: 200, Body: []byte(`{"choices":[{"message":{}}]}`)})
	if err == nil || !strings.Contains(err.Error(), "audio.data") {
		t.Fatalf("缺音频时的错误 = %v，期望提到 audio.data", err)
	}

	_, err = d.Decode(plugin.Input{Format: "mp3"}, plugin.Response{Status: 200, Body: []byte(`{"choices":[{"message":{"audio":{"data":"!!!"}}}]}`)})
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("非法 base64 的错误 = %v", err)
	}

	_, err = d.Decode(plugin.Input{Format: "mp3"}, plugin.Response{Status: 200, Body: []byte("not json")})
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("坏正文的错误 = %v", err)
	}
}

func TestMiMoTranscriptPrepare(t *testing.T) {
	d := systemDialect(t, plugin.DialectMiMoChatASR)

	plan, err := d.Prepare(plugin.Input{Model: "mimo-v2.5-asr", Audio: []byte("RIFF"), AudioContainer: "wav"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	messages, _ := plan.JSON["messages"].([]map[string]any)
	if len(messages) != 1 || messages[0]["role"] != "user" {
		t.Fatalf("messages = %+v，期望一条 user", messages)
	}
	content, _ := messages[0]["content"].([]map[string]any)
	if len(content) != 1 || content[0]["type"] != "input_audio" {
		t.Fatalf("content = %+v，期望一个 input_audio part", content)
	}
	inputAudio, _ := content[0]["input_audio"].(map[string]any)
	data, _ := inputAudio["data"].(string)
	if !strings.HasPrefix(data, "data:audio/wav;base64,") {
		t.Fatalf("data URL = %q，期望 wav 的 data URL", data)
	}
	options, _ := plan.JSON["asr_options"].(map[string]any)
	if options["language"] != "auto" {
		t.Fatalf("language = %v，没给语言时应当是 auto", options["language"])
	}

	plan, err = d.Prepare(plugin.Input{Model: "m", Audio: []byte("ID3"), AudioContainer: "mp3", Language: "zh"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	messages, _ = plan.JSON["messages"].([]map[string]any)
	content, _ = messages[0]["content"].([]map[string]any)
	inputAudio, _ = content[0]["input_audio"].(map[string]any)
	if data, _ := inputAudio["data"].(string); !strings.HasPrefix(data, "data:audio/mpeg;base64,") {
		t.Fatalf("mp3 的 data URL = %q", data)
	}
	options, _ = plan.JSON["asr_options"].(map[string]any)
	if options["language"] != "zh" {
		t.Fatalf("language = %v，期望 zh", options["language"])
	}

	if _, err := d.Prepare(plugin.Input{Model: "m"}); err == nil {
		t.Fatal("没有音频时应当报错")
	}
}

func TestMiMoTranscriptDecode(t *testing.T) {
	d := systemDialect(t, plugin.DialectMiMoChatASR)
	body := []byte(`{"choices":[{"message":{"content":"今天的天气不错"}}],"usage":{"seconds":3.6}}`)

	result, err := d.Decode(plugin.Input{Format: "json"}, plugin.Response{Status: 200, Body: body})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.Kind != plugin.ResultJSON {
		t.Fatalf("Kind = %q，期望 json", result.Kind)
	}
	var payload map[string]any
	if err := json.Unmarshal(result.JSON, &payload); err != nil {
		t.Fatalf("结果不是 JSON: %v", err)
	}
	if payload["text"] != "今天的天气不错" {
		t.Fatalf("text = %v", payload["text"])
	}
	if result.Usage.AudioSeconds != 4 {
		t.Fatalf("秒数 = %d，期望 4（3.6 四舍五入）", result.Usage.AudioSeconds)
	}

	result, err = d.Decode(plugin.Input{Format: "text"}, plugin.Response{Status: 200, Body: body})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.Kind != plugin.ResultText || result.Text != "今天的天气不错" {
		t.Fatalf("text 结果 = %+v", result)
	}

	result, err = d.Decode(plugin.Input{Format: "verbose_json", Language: "zh"},
		plugin.Response{Status: 200, Body: body})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	payload = nil
	if err := json.Unmarshal(result.JSON, &payload); err != nil {
		t.Fatalf("结果不是 JSON: %v", err)
	}
	if payload["task"] != "transcribe" || payload["language"] != "zh" || payload["duration"] != float64(4) {
		t.Fatalf("verbose_json = %+v", payload)
	}

	if _, err := d.Decode(plugin.Input{Format: "json"}, plugin.Response{Status: 200, Body: []byte(`{"choices":[]}`)}); err == nil {
		t.Fatal("choices 为空时应当报错")
	}
}

func TestPassthroughDialectsRefuseToBuild(t *testing.T) {
	for _, id := range []string{plugin.DialectOpenAISpeech, plugin.DialectOpenAITranscriptions} {
		d := systemDialect(t, id)
		if _, err := d.Prepare(plugin.Input{}); err == nil {
			t.Fatalf("%s 不该接受 Prepare：同协议直通由宿主原样转发", id)
		}
		if _, err := d.Decode(plugin.Input{}, plugin.Response{}); err == nil {
			t.Fatalf("%s 不该接受 Decode：同协议直通由宿主原样转发", id)
		}
	}
}
