package plugin_test

import (
	"encoding/json"
	"testing"

	"lax/internal/plugin"
)

// dialectAt 取 dialects 里第 i 条的对象，用例只改坏一处。
func dialectAt(t *testing.T, doc map[string]any, i int) map[string]any {
	t.Helper()
	list, ok := doc["dialects"].([]any)
	if !ok || i >= len(list) {
		t.Fatalf("dialects 里没有第 %d 条", i)
	}
	decl, ok := list[i].(map[string]any)
	if !ok {
		t.Fatalf("dialects[%d] 不是对象", i)
	}
	return decl
}

// baseDialectDoc 是一条能通过校验的声明式 TTS 方言。
func baseDialectDoc() map[string]any {
	return map[string]any{
		"schema": "lax.dialect/v1",
		"dialects": []any{map[string]any{
			"id":            "com.example.acme/speech@1",
			"operation":     "speech.synthesize@1",
			"upstream_wire": "openai-speech",
			"transport":     map[string]any{"request": "json", "response": "binary", "streaming": "bytes"},
			"implementation": map[string]any{
				"prepare": "declarative",
				"decode":  "declarative",
			},
			"limits": map[string]any{"max_input_chars": 4096},
			"request": map[string]any{
				"method": "POST",
				"path":   "/v1/audio/speech",
				"body": map[string]any{
					"model": map[string]any{"model": "upstream_id"},
					"input": map[string]any{"input": "text"},
					"voice": map[string]any{"input": "voice", "default": "alloy"},
					"speed": map[string]any{"input": "speed", "omit_if_empty": true},
					"meta":  map[string]any{"chars": map[string]any{"input": "chars"}},
				},
			},
			"response": map[string]any{
				"mode":     "json_pointer",
				"pointer":  "/data",
				"encoding": "base64",
				"usage":    map[string]any{"output_tokens": "/usage/output_tokens"},
			},
		}},
	}
}

// TestLoadDialectFileOK 一个文件两条方言：一条声明式 TTS，一条 hook 的 chat，互不干扰。
func TestLoadDialectFileOK(t *testing.T) {
	file, err := plugin.LoadDialectFile(fixture("dialect-ok.json"))
	if err != nil {
		t.Fatalf("读 dialect-ok.json: %v", err)
	}
	if len(file.Dialects) != 2 {
		t.Fatalf("方言数 = %d，期望 2", len(file.Dialects))
	}
	speech := file.Dialects[0]
	if speech.ID != "com.example.acme/speech@1" || speech.Operation != plugin.OpSpeechSynthesize {
		t.Fatalf("方言 = %+v，期望 acme/speech 承载 speech.synthesize@1", speech)
	}
	if speech.UpstreamWire != plugin.WireSpeech || speech.Transport.Response != "binary" {
		t.Fatalf("transport = %+v，期望 openai-speech 与 binary 响应", speech.Transport)
	}
	if speech.Request == nil || speech.Request.Path != "/v1/audio/speech" {
		t.Fatalf("request = %+v，期望 /v1/audio/speech", speech.Request)
	}
	voice := speech.Request.Body["voice"]
	if voice.Input != "voice" || voice.Default != "alloy" {
		t.Fatalf("voice 节点 = %+v，期望读 input.voice 并留 default", voice)
	}
	speed := speech.Request.Body["speed"]
	if !speed.OmitIfEmpty {
		t.Fatalf("speed 节点 = %+v，期望 omit_if_empty", speed)
	}
	if speech.Response == nil || speech.Response.Mode != "passthrough" {
		t.Fatalf("response = %+v，期望 passthrough", speech.Response)
	}
	if speech.Response.Pointer != "" || speech.Response.Encoding != "" {
		t.Fatalf("passthrough 不该带 pointer 或 encoding，实际 %+v", speech.Response)
	}
	if _, ok := speech.Response.MIME.Map["mp3"]; !ok {
		t.Fatalf("mime 映射 = %v，期望含 mp3", speech.Response.MIME.Map)
	}

	chat := file.Dialects[1]
	if chat.Implementation.Prepare != plugin.ImplHook || chat.Request != nil || chat.Response != nil {
		t.Fatalf("第二条方言 = %+v，期望 hook/hook 且没有声明式 request 与 response", chat)
	}
	if chat.Transport.Streaming != "sse" {
		t.Fatalf("streaming = %q，期望 sse", chat.Transport.Streaming)
	}
}

// TestLoadDialectFileFixturesReject 反例：每条只坏一处，文案指向字段路径。
func TestLoadDialectFileFixturesReject(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"dialect-lax-prefix.json", []string{"lax/ 前缀保留", "内置方言"}},
		{"dialect-bad-combo.json", []string{"不承载", "anthropic → " + plugin.OpChatComplete, plugin.OpSpeechTranscribe}},
		{"dialect-mixed-implementation.json", []string{"decode 是 hook", "response 却存在"}},
		{"dialect-bad-usage-pointer.json", []string{"response.usage.input_tokens", "必须以 / 开头"}},
		{"dialect-unknown-input.json", []string{"request.body.messages.input", "只能读"}},
		{"dialect-unknown-field.json", []string{"未知字段", "framing", "extensions"}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			_, err := plugin.LoadDialectFile(fixture(c.file))
			requireContains(t, err, append([]string{fixture(c.file)}, c.want...)...)
		})
	}
}

// TestDecodeDialectFileRejects 逐条改坏一份合法方言文件。
func TestDecodeDialectFileRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, doc map[string]any)
		want   []string
	}{
		{
			name:   "schema 不匹配",
			mutate: func(t *testing.T, doc map[string]any) { doc["schema"] = "lax.dialect/v2" },
			want:   []string{"schema 是", "lax.dialect/v1"},
		},
		{
			name:   "dialects 为空",
			mutate: func(t *testing.T, doc map[string]any) { doc["dialects"] = []any{} },
			want:   []string{"dialects 为空"},
		},
		{
			name: "同一文件内 id 重复",
			mutate: func(t *testing.T, doc map[string]any) {
				dup := baseDialectDoc()["dialects"].([]any)[0]
				doc["dialects"] = append(doc["dialects"].([]any), dup)
			},
			want: []string{"dialects[1].id 与 dialects[0].id", "不能重复"},
		},
		{
			name:   "id 没有斜杠",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["id"] = "speech@1" },
			want:   []string{"只含一个斜杠"},
		},
		{
			name:   "id 冒用 lax 前缀",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["id"] = "lax/speech@1" },
			want:   []string{"lax/ 前缀保留"},
		},
		{
			name:   "id 缺主版本",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["id"] = "com.example.acme/speech" },
			want:   []string{"@<主版本>"},
		},
		{
			name:   "id 主版本不是数字",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["id"] = "com.example.acme/speech@v1" },
			want:   []string{"十进制主版本号"},
		},
		{
			name:   "id 缺名称",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["id"] = "com.example.acme/@1" },
			want:   []string{"要有名称"},
		},
		{
			name:   "operation 未知",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["operation"] = "speech.translate@1" },
			want:   []string{"operation 是", "speech.synthesize@1"},
		},
		{
			name:   "upstream_wire 未知",
			mutate: func(t *testing.T, doc map[string]any) { dialectAt(t, doc, 0)["upstream_wire"] = "grpc" },
			want:   []string{"upstream_wire 是", "openai-speech"},
		},
		{
			name: "operation 与 wire 组合非法",
			mutate: func(t *testing.T, doc map[string]any) {
				dialectAt(t, doc, 0)["operation"] = plugin.OpSpeechTranscribe
			},
			want: []string{"openai-speech 不承载", "openai-transcriptions → " + plugin.OpSpeechTranscribe},
		},
		{
			name: "transport.request 不是 json",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "transport")["request"] = "form"
			},
			want: []string{"transport.request", "只有 json"},
		},
		{
			name: "transport.response 不是 json 或 binary",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "transport")["response"] = "sse"
			},
			want: []string{"transport.response", "json 或 binary"},
		},
		{
			name: "transport.streaming 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "transport")["streaming"] = "chunked"
			},
			want: []string{"transport.streaming", "none、bytes、sse 或 jsonl"},
		},
		{
			name: "prepare 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "implementation")["prepare"] = "native"
			},
			want: []string{"implementation.prepare", "hook 或 declarative"},
		},
		{
			name: "prepare declarative 缺 request",
			mutate: func(t *testing.T, doc map[string]any) {
				delete(dialectAt(t, doc, 0), "request")
			},
			want: []string{"却缺 request"},
		},
		{
			name: "prepare hook 却有 request",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "implementation")["prepare"] = plugin.ImplHook
			},
			want: []string{"request 却存在"},
		},
		{
			name: "decode declarative 缺 response",
			mutate: func(t *testing.T, doc map[string]any) {
				delete(dialectAt(t, doc, 0), "response")
			},
			want: []string{"却缺 response"},
		},
		{
			name: "decode hook 却有 response",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "implementation")["decode"] = plugin.ImplHook
			},
			want: []string{"response 却存在"},
		},
		{
			name: "request.method 不是 POST",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "request")["method"] = "GET"
			},
			want: []string{"request.method", "只发 POST"},
		},
		{
			name: "request.path 不以斜杠开头",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "request")["path"] = "v1/audio/speech"
			},
			want: []string{"request.path", "必须以 / 开头"},
		},
		{
			name: "request.path 含 ..",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "request")["path"] = "/v1/../admin"
			},
			want: []string{"按原样拼接"},
		},
		{
			name: "request.path 是相对协议引用",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "request")["path"] = "//evil.example.com/v1"
			},
			want: []string{"重复的斜杠"},
		},
		{
			name: "request.body 为空",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "request")["body"] = map[string]any{}
			},
			want: []string{"request.body 为空"},
		},
		{
			name: "request.body 有空键",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, at(t, dialectAt(t, doc, 0), "request"), "body")[""] = "x"
			},
			want: []string{"request.body 里有空键"},
		},
		{
			name: "input 字段不认识",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, at(t, dialectAt(t, doc, 0), "request"), "body")["input"] = map[string]any{"input": "messages"}
			},
			want: []string{"request.body.input.input", "只能读"},
		},
		{
			name: "嵌套对象里的 input 字段不认识",
			mutate: func(t *testing.T, doc map[string]any) {
				body := at(t, at(t, dialectAt(t, doc, 0), "request"), "body")
				body["meta"] = map[string]any{"tone": map[string]any{"input": "tone"}}
			},
			want: []string{"request.body.meta.tone.input", "只能读"},
		},
		{
			name: "数组里的 input 字段不认识",
			mutate: func(t *testing.T, doc map[string]any) {
				body := at(t, at(t, dialectAt(t, doc, 0), "request"), "body")
				body["parts"] = []any{map[string]any{"input": "tone"}}
			},
			want: []string{"request.body.parts[0].input"},
		},
		{
			name: "model 字段不认识",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, at(t, dialectAt(t, doc, 0), "request"), "body")["model"] = map[string]any{"model": "id"}
			},
			want: []string{"只能读 upstream_id"},
		},
		{
			name: "response.mode 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "response")["mode"] = "sse_json_pointer"
			},
			want: []string{"response.mode", "passthrough 或 json_pointer"},
		},
		{
			name: "passthrough 带了 pointer",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "response")["mode"] = "passthrough"
			},
			want: []string{"passthrough 不取字段"},
		},
		{
			name: "json_pointer 的 pointer 不以斜杠开头",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "response")["pointer"] = "data"
			},
			want: []string{"response.pointer", "必须以 / 开头"},
		},
		{
			name: "json_pointer 的转义非法",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "response")["pointer"] = "/a~2b"
			},
			want: []string{"RFC 6901"},
		},
		{
			name: "json_pointer 的 encoding 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "response")["encoding"] = "hex"
			},
			want: []string{"response.encoding", "base64、text 或 json"},
		},
		{
			name: "usage 指针非法",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, at(t, dialectAt(t, doc, 0), "response"), "usage")["total_tokens"] = "usage.total_tokens"
			},
			want: []string{"response.usage.total_tokens"},
		},
		{
			name: "limits 为负",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, dialectAt(t, doc, 0), "limits")["max_response_bytes"] = -1
			},
			want: []string{"limits.max_response_bytes", "不能为负"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := baseDialectDoc()
			c.mutate(t, doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("序列化用例: %v", err)
			}
			_, err = plugin.DecodeDialectFile("dialects.json", data)
			requireContains(t, err, append([]string{"dialects.json"}, c.want...)...)
		})
	}
}

// TestValidateDialectDeclForRegistry 注册表直接拼的方言也过同一套规则：
// hook 方言没有 request 与 response，声明式方言两者都要。
func TestValidateDialectDeclForRegistry(t *testing.T) {
	hook := plugin.DialectDecl{
		ID:           "com.example.mimo/mimo-chat-speech@1",
		Operation:    plugin.OpSpeechSynthesize,
		UpstreamWire: plugin.WireOpenAIChat,
		Transport:    plugin.TransportDecl{Request: "json", Response: "json", Streaming: "none"},
		Implementation: plugin.ImplementationDecl{
			Prepare: plugin.ImplHook,
			Decode:  plugin.ImplHook,
		},
		Limits: plugin.LimitsDecl{MaxInputChars: 8192},
	}
	if err := plugin.ValidateDialectDecl(hook); err != nil {
		t.Fatalf("speech.synthesize@1 走 openai-chat 应通过：MiMo 的方言就是这个组合，实际 %v", err)
	}

	hook.UpstreamWire = plugin.WireAnthropic
	if err := plugin.ValidateDialectDecl(hook); err == nil {
		t.Fatalf("speech.synthesize@1 走 anthropic 应被拒：它能通过校验就说明组合规则失效了")
	}

	hook.UpstreamWire = plugin.WireSpeech
	if err := plugin.ValidateDialectDecl(hook); err != nil {
		t.Fatalf("hook 方言 %s 应通过，实际 %v", hook.ID, err)
	}
}
