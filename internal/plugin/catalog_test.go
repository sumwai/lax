package plugin_test

import (
	"encoding/json"
	"testing"

	"lax/internal/plugin"
)

// modelAt 取 models 里第 i 个模型的对象，用例只改坏一处。
func modelAt(t *testing.T, doc map[string]any, i int) map[string]any {
	t.Helper()
	list, ok := doc["models"].([]any)
	if !ok || i >= len(list) {
		t.Fatalf("models 里没有第 %d 个", i)
	}
	model, ok := list[i].(map[string]any)
	if !ok {
		t.Fatalf("models[%d] 不是对象", i)
	}
	return model
}

// modelOperation 取某个模型的一个 operation 声明。
func modelOperation(t *testing.T, doc map[string]any, i int, op string) map[string]any {
	t.Helper()
	decl, ok := at(t, modelAt(t, doc, i), "operations")[op].(map[string]any)
	if !ok {
		t.Fatalf("models[%d].operations 里没有 %s", i, op)
	}
	return decl
}

// baseCatalogDoc 是一个能通过校验的模型目录。
func baseCatalogDoc() map[string]any {
	return map[string]any{
		"models": []any{map[string]any{
			"model":       "acme/acme-tts",
			"upstream_id": "acme-tts-v1",
			"operations": map[string]any{
				"speech.synthesize@1": map[string]any{
					"dialect": "com.example.acme/speech@1",
					"capabilities": map[string]any{
						"supported": true,
						"streaming": false,
						"formats":   []any{"mp3", "wav"},
						"voices":    []any{},
					},
				},
			},
		}},
	}
}

// TestLoadCatalogOK 一个文件里两个模型：空数组是明确声明，false 与 "unknown" 都不是省略。
func TestLoadCatalogOK(t *testing.T) {
	file, err := plugin.LoadCatalog(fixture("catalog-ok.json"))
	if err != nil {
		t.Fatalf("读 catalog-ok.json: %v", err)
	}
	if len(file.Models) != 2 {
		t.Fatalf("模型数 = %d，期望 2", len(file.Models))
	}
	chat := file.Models[0]
	if chat.Model != "acme/acme-chat" || chat.UpstreamID != "acme-chat-2026" {
		t.Fatalf("第一个模型 = %+v，期望 acme/acme-chat", chat)
	}
	chatCap := chat.Operations[plugin.OpChatComplete].Capabilities
	if chatCap == nil || chatCap.Streaming == nil {
		t.Fatalf("chat 能力 = %+v，期望声明了 streaming", chatCap)
	}
	if stream := chatCap.Streaming.Value; stream != "true" {
		t.Fatalf("streaming = %q，期望 true", stream)
	}
	if tools := chatCap.Tools.Value; tools != "false" {
		t.Fatalf("tools = %q，期望 false", tools)
	}
	if vision := chatCap.Vision.Value; vision != "unknown" {
		t.Fatalf("vision = %q，期望 unknown", vision)
	}

	tts := file.Models[1]
	if len(tts.Operations) != 2 {
		t.Fatalf("第二个模型有 %d 个 operation，期望 2", len(tts.Operations))
	}
	ttsCap := tts.Operations[plugin.OpSpeechSynthesize].Capabilities
	if ttsCap.Voices == nil {
		t.Fatalf("voices 缺声明，期望是空数组")
	}
	if ttsCap.Voices.Unknown || len(ttsCap.Voices.Values) != 0 {
		t.Fatalf("voices = %+v，空数组应表示明确一个都没有", ttsCap.Voices)
	}
	if formats := ttsCap.Formats.Values; len(formats) != 2 || formats[0] != "mp3" {
		t.Fatalf("formats = %v，期望 mp3 与 wav", formats)
	}
	transcribe := tts.Operations[plugin.OpSpeechTranscribe]
	if transcribe.Capabilities.Supported.Value != "false" {
		t.Fatalf("transcribe 的 supported = %+v，期望 false", transcribe.Capabilities.Supported)
	}
}

// TestLoadCatalogFixturesReject 反例：重复绑定与未知 operation 都要在安装时就报出来。
func TestLoadCatalogFixturesReject(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"catalog-duplicate-binding.json", []string{"models[1].model 与 models[0].model", "两个绑定"}},
		{"catalog-unknown-operation.json", []string{"speech.translate@1", "宿主认识的操作"}},
		{"catalog-unknown-field.json", []string{"未知字段", "schema"}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			_, err := plugin.LoadCatalog(fixture(c.file))
			requireContains(t, err, append([]string{fixture(c.file)}, c.want...)...)
		})
	}
}

// TestDecodeCatalogRejects 逐条改坏一份合法目录。
func TestDecodeCatalogRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, doc map[string]any)
		want   []string
	}{
		{
			name:   "models 为空",
			mutate: func(t *testing.T, doc map[string]any) { doc["models"] = []any{} },
			want:   []string{"models 为空"},
		},
		{
			name:   "model 为空",
			mutate: func(t *testing.T, doc map[string]any) { modelAt(t, doc, 0)["model"] = "  " },
			want:   []string{"models[0].model 为空"},
		},
		{
			name:   "model 含空白",
			mutate: func(t *testing.T, doc map[string]any) { modelAt(t, doc, 0)["model"] = "acme/acme tts" },
			want:   []string{"model 是", "不能有空白"},
		},
		{
			name:   "upstream_id 为空",
			mutate: func(t *testing.T, doc map[string]any) { modelAt(t, doc, 0)["upstream_id"] = "" },
			want:   []string{"models[0].upstream_id 为空"},
		},
		{
			name:   "operations 为空",
			mutate: func(t *testing.T, doc map[string]any) { modelAt(t, doc, 0)["operations"] = map[string]any{} },
			want:   []string{"models[0].operations 为空"},
		},
		{
			name: "operation 是宿主不认识的",
			mutate: func(t *testing.T, doc map[string]any) {
				ops := at(t, modelAt(t, doc, 0), "operations")
				ops["image.generate@1"] = ops["speech.synthesize@1"]
			},
			want: []string{"image.generate@1", "宿主认识的操作"},
		},
		{
			name: "dialect 为空",
			mutate: func(t *testing.T, doc map[string]any) {
				modelOperation(t, doc, 0, plugin.OpSpeechSynthesize)["dialect"] = ""
			},
			want: []string{"operations[speech.synthesize@1].dialect 为空"},
		},
		{
			name: "同一模型重复声明",
			mutate: func(t *testing.T, doc map[string]any) {
				dup := baseCatalogDoc()["models"].([]any)[0]
				doc["models"] = append(doc["models"].([]any), dup)
			},
			want: []string{"models[1].model 与 models[0].model", "两个绑定"},
		},
		{
			name: "formats 里有空项",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, modelOperation(t, doc, 0, plugin.OpSpeechSynthesize), "capabilities")["formats"] = []any{"mp3", " "}
			},
			want: []string{"capabilities.formats[1]", "空数组才表示一个都没有"},
		},
		{
			name: "voices 里有空项",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, modelOperation(t, doc, 0, plugin.OpSpeechSynthesize), "capabilities")["voices"] = []any{""}
			},
			want: []string{"capabilities.voices[0]"},
		},
		{
			name: "能力值不是三态之一",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, modelOperation(t, doc, 0, plugin.OpSpeechSynthesize), "capabilities")["supported"] = "maybe"
			},
			want: []string{"能力值只能是"},
		},
		{
			name: "能力里有未知字段",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, modelOperation(t, doc, 0, plugin.OpSpeechSynthesize), "capabilities")["modalities"] = map[string]any{}
			},
			want: []string{"未知字段", "modalities"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := baseCatalogDoc()
			c.mutate(t, doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("序列化用例: %v", err)
			}
			_, err = plugin.DecodeCatalog("models.json", data)
			requireContains(t, err, append([]string{"models.json"}, c.want...)...)
		})
	}
}

// TestValidateCatalogUnsupportedOnly 只声明 supported=false 也是合法目录：
// 缺声明与声明不支持是两件事，不拿一项能力去推断另一项。
func TestValidateCatalogUnsupportedOnly(t *testing.T) {
	supported := &plugin.Tri{Value: "false"}
	file := plugin.CatalogFile{Models: []plugin.ModelDecl{{
		Model:      "acme/acme-tts",
		UpstreamID: "acme-tts-v1",
		Operations: map[string]plugin.OperationDecl{
			plugin.OpSpeechSynthesize: {
				Dialect:      "com.example.acme/speech@1",
				Capabilities: &plugin.Capabilities{Supported: supported},
			},
		},
	}}}
	if err := plugin.ValidateCatalog(file); err != nil {
		t.Fatalf("只声明 supported=false 应通过，实际 %v", err)
	}
}

// TestValidateCatalogRejectsHandBuiltTri 手写结构体绕过 UnmarshalJSON 时也要拦住非法三态。
func TestValidateCatalogRejectsHandBuiltTri(t *testing.T) {
	file := plugin.CatalogFile{Models: []plugin.ModelDecl{{
		Model:      "acme/acme-tts",
		UpstreamID: "acme-tts-v1",
		Operations: map[string]plugin.OperationDecl{
			plugin.OpSpeechSynthesize: {
				Dialect:      "com.example.acme/speech@1",
				Capabilities: &plugin.Capabilities{Streaming: &plugin.Tri{Value: "yes"}},
			},
		},
	}}}
	requireContains(t, plugin.ValidateCatalog(file), "capabilities.streaming", "能力值只能是")
}
