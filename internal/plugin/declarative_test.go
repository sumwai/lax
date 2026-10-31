package plugin_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lax/internal/plugin"
)

// 声明式映射的测试只走公开入口：方言声明从 JSON 来，求值与解码都按契约观察结果。

func mustCompile(t *testing.T, raw string) plugin.Dialect {
	t.Helper()
	var decl plugin.DialectDecl
	if err := json.Unmarshal([]byte(raw), &decl); err != nil {
		t.Fatalf("解析方言声明: %v", err)
	}
	dialect, err := plugin.CompileDialect(decl)
	if err != nil {
		t.Fatalf("编译方言声明: %v", err)
	}
	return dialect
}

// requestDialect 拼一条带请求映射的声明，响应部分与求值无关。
func requestDialect(t *testing.T, body string) plugin.Dialect {
	t.Helper()
	return mustCompile(t, `{
	  "schema": "lax.dialect/v1",
	  "id": "com.example.test/speech@1",
	  "operation": "speech.synthesize@1",
	  "upstream_wire": "openai-speech",
	  "transport": {"request": "json", "response": "binary"},
	  "implementation": {"prepare": "declarative", "decode": "declarative"},
	  "request": {"method": "POST", "path": "/v1/audio/speech", "body": `+body+`}
	}`)
}

// structDialect 用程序化构造的 Value 建声明。map 节点目前只能这样测：types.go 把 map
// 也算作来源键，JSON 里的 {input, map} 会在解析层被拒（见实现报告）。
func structDialect(t *testing.T, body map[string]plugin.Value, response *plugin.ResponseDecl, transportResponse string) plugin.Dialect {
	t.Helper()
	dialect, err := plugin.CompileDialect(plugin.DialectDecl{
		ID: "com.example.test/tts@1", Operation: plugin.OpSpeechSynthesize, UpstreamWire: plugin.WireSpeech,
		Transport:      plugin.TransportDecl{Request: "json", Response: transportResponse},
		Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
		Request:        &plugin.RequestDecl{Body: body},
		Response:       response,
	})
	if err != nil {
		t.Fatalf("编译方言: %v", err)
	}
	return dialect
}

// responseDialect 拼一条带响应映射的声明。
func responseDialect(t *testing.T, transportResponse, response string) plugin.Dialect {
	t.Helper()
	return mustCompile(t, `{
	  "schema": "lax.dialect/v1",
	  "id": "com.example.test/tts@1",
	  "operation": "speech.synthesize@1",
	  "upstream_wire": "openai-chat",
	  "transport": {"request": "json", "response": "`+transportResponse+`"},
	  "implementation": {"prepare": "declarative", "decode": "declarative"},
	  "response": `+response+`
	}`)
}

func sameJSON(t *testing.T, got any, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化求值结果: %v", err)
	}
	var gotAny, wantAny any
	if err := json.Unmarshal(encoded, &gotAny); err != nil {
		t.Fatalf("回读求值结果: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantAny); err != nil {
		t.Fatalf("解析期望 JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Errorf("请求体不一致\n得到: %s\n期望: %s", encoded, want)
	}
}

func TestCompileDialect(t *testing.T) {
	cases := []struct {
		name string
		decl plugin.DialectDecl
		err  string
	}{
		{"声明式", plugin.DialectDecl{
			ID: "com.example/a@1", Operation: plugin.OpSpeechSynthesize, UpstreamWire: plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
		}, ""},
		{"hook 方言", plugin.DialectDecl{
			ID: "lax/a@1", Operation: plugin.OpSpeechSynthesize, UpstreamWire: plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplHook, Decode: plugin.ImplHook},
		}, "必须是 \"declarative\""},
		{"只声明了请求", plugin.DialectDecl{
			ID: "com.example/a@1", Operation: plugin.OpSpeechSynthesize, UpstreamWire: plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplHook},
		}, "必须是 \"declarative\""},
		{"缺 id", plugin.DialectDecl{
			Operation: plugin.OpSpeechSynthesize, UpstreamWire: plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
		}, "缺少 id"},
		{"缺 operation", plugin.DialectDecl{
			ID: "com.example/a@1", UpstreamWire: plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
		}, "缺少 operation"},
		{"缺 upstream_wire", plugin.DialectDecl{
			ID: "com.example/a@1", Operation: plugin.OpSpeechSynthesize,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
		}, "缺少 upstream_wire"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialect, err := plugin.CompileDialect(tc.decl)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("期望错误含有 %q，得到 %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("编译方言: %v", err)
			}
			if dialect.ID() != tc.decl.ID || dialect.Operation() != tc.decl.Operation || dialect.UpstreamWire() != tc.decl.UpstreamWire {
				t.Errorf("元信息不一致: %s %s %s", dialect.ID(), dialect.Operation(), dialect.UpstreamWire())
			}
		})
	}
}

func TestPrepare(t *testing.T) {
	speed := 1.25
	cases := []struct {
		name string
		body string
		in   plugin.Input
		want string
		err  string
	}{
		{
			name: "常量",
			body: `{"tag": {"const": "tts"}}`,
			want: `{"tag": "tts"}`,
		},
		{
			name: "input 取正文",
			body: `{"input": {"input": "text"}}`,
			in:   plugin.Input{Text: "你好"},
			want: `{"input": "你好"}`,
		},
		{
			name: "input 未知字段",
			body: `{"temperature": {"input": "temperature"}}`,
			err:  `不认识字段 "temperature"`,
		},
		{
			name: "model 取上游模型名",
			body: `{"model": {"model": "upstream_id"}}`,
			in:   plugin.Input{Model: "mimo-v2.5-tts"},
			want: `{"model": "mimo-v2.5-tts"}`,
		},
		{
			name: "option 命中",
			body: `{"region": {"option": "region"}}`,
			in:   plugin.Input{Options: map[string]string{"region": "HKG"}},
			want: `{"region": "HKG"}`,
		},
		{
			name: "option 缺失用默认值",
			body: `{"region": {"option": "region", "default": "SGP"}}`,
			want: `{"region": "SGP"}`,
		},
		{
			name: "空串用默认值",
			body: `{"voice": {"input": "voice", "default": "alloy"}}`,
			want: `{"voice": "alloy"}`,
		},
		{
			name: "空串省略字段",
			body: `{"instructions": {"input": "instructions", "omit_if_empty": true}}`,
			want: `{}`,
		},
		{
			name: "false 与 0 不算空",
			body: `{"flag": {"const": false, "omit_if_empty": true}, "zero": {"const": 0, "omit_if_empty": true}}`,
			want: `{"flag": false, "zero": 0}`,
		},
		{
			name: "空数组省略字段",
			body: `{"voices": {"const": [], "omit_if_empty": true}}`,
			want: `{}`,
		},
		{
			name: "嵌套对象",
			body: `{"meta": {"kind": {"const": "tts"}, "lang": {"input": "language", "default": "zh"}}}`,
			in:   plugin.Input{Language: "yue"},
			want: `{"meta": {"kind": "tts", "lang": "yue"}}`,
		},
		{
			name: "数组里放节点",
			body: `{"langs": [{"input": "language"}, {"const": "zh"}]}`,
			in:   plugin.Input{Language: "en"},
			want: `{"langs": ["en", "zh"]}`,
		},
		{
			name: "speed 未给就省略",
			body: `{"speed": {"input": "speed", "omit_if_empty": true}}`,
			want: `{}`,
		},
		{
			name: "speed 给了就写入",
			body: `{"speed": {"input": "speed", "omit_if_empty": true}}`,
			in:   plugin.Input{Speed: &speed},
			want: `{"speed": 1.25}`,
		},
		{
			name: "chars 是整数",
			body: `{"chars": {"input": "chars"}}`,
			in:   plugin.Input{Chars: 42},
			want: `{"chars": 42}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialect := requestDialect(t, tc.body)
			plan, err := dialect.Prepare(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("期望错误含有 %q，得到 %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("求值请求体: %v", err)
			}
			sameJSON(t, plan.JSON, tc.want)
		})
	}
}

func TestPrepareMap(t *testing.T) {
	mapNode := func() map[string]plugin.Value {
		return map[string]plugin.Value{
			"response_format": {Input: "format", Map: map[string]any{"mp3": "mp3", "wav": "wav"}},
		}
	}
	cases := []struct {
		name string
		body map[string]plugin.Value
		in   plugin.Input
		want string
		err  string
	}{
		{"命中", mapNode(), plugin.Input{Format: "wav"}, `{"response_format": "wav"}`, ""},
		{
			"未命中用默认值",
			map[string]plugin.Value{"response_format": {
				Input: "format", Map: map[string]any{"mp3": "mp3"}, Default: "mp3", HasDefault: true,
			}},
			plugin.Input{Format: "pcm"}, `{"response_format": "mp3"}`, "",
		},
		{"未命中报错", mapNode(), plugin.Input{Format: "pcm"}, "", `取值 "pcm" 不在方言声明的映射里，可用的是 mp3、wav`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := structDialect(t, tc.body, nil, "binary").Prepare(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("期望错误含有 %q，得到 %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("求值请求体: %v", err)
			}
			sameJSON(t, plan.JSON, tc.want)
		})
	}
}

func TestPreparePathAndStability(t *testing.T) {
	dialect := requestDialect(t, `{"input": {"input": "text"}, "model": {"model": "upstream_id"}}`)
	in := plugin.Input{Text: "你好", Model: "m"}
	first, err := dialect.Prepare(in)
	if err != nil {
		t.Fatalf("求值请求体: %v", err)
	}
	if first.Path != "/v1/audio/speech" {
		t.Errorf("Path = %q，期望 /v1/audio/speech", first.Path)
	}
	if _, ok := first.JSON["path"]; ok {
		t.Error("Path 不应写进 JSON 请求体")
	}
	second, err := dialect.Prepare(in)
	if err != nil {
		t.Fatalf("第二次求值: %v", err)
	}
	if !reflect.DeepEqual(first.JSON, second.JSON) {
		t.Errorf("两次求值不一致: %v / %v", first.JSON, second.JSON)
	}
}

func TestPrepareLimits(t *testing.T) {
	compile := func(body map[string]plugin.Value) plugin.Dialect {
		t.Helper()
		dialect, err := plugin.CompileDialect(plugin.DialectDecl{
			ID: "com.example/limits@1", Operation: plugin.OpSpeechSynthesize, UpstreamWire: plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
			Request:        &plugin.RequestDecl{Body: body},
		})
		if err != nil {
			t.Fatalf("编译方言: %v", err)
		}
		return dialect
	}

	deep := plugin.Value{Scalar: "x"}
	for range 40 {
		deep = plugin.Value{Array: []plugin.Value{deep}}
	}
	if _, err := compile(map[string]plugin.Value{"deep": deep}).Prepare(plugin.Input{}); err == nil ||
		!strings.Contains(err.Error(), "嵌套超过 32 层") {
		t.Errorf("嵌套超限应报错，得到 %v", err)
	}

	big := map[string]plugin.Value{"big": {HasConst: true, Const: strings.Repeat("x", 2<<20)}}
	if _, err := compile(big).Prepare(plugin.Input{}); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("体积超限应报错，得到 %v", err)
	}
}

func TestEvalValue(t *testing.T) {
	var omitted plugin.Value
	if err := json.Unmarshal([]byte(`{"input": "text", "omit_if_empty": true}`), &omitted); err != nil {
		t.Fatalf("解析节点: %v", err)
	}
	value, present, err := plugin.EvalValue(omitted, plugin.Input{Text: ""})
	if err != nil || present || value != nil {
		t.Errorf("空正文应省略: value=%v present=%v err=%v", value, present, err)
	}
	value, present, err = plugin.EvalValue(omitted, plugin.Input{Text: "你好"})
	if err != nil || !present || value != "你好" {
		t.Errorf("非空正文应写入: value=%v present=%v err=%v", value, present, err)
	}

	var unknown plugin.Value
	if err := json.Unmarshal([]byte(`{"input": "temperature"}`), &unknown); err != nil {
		t.Fatalf("解析节点: %v", err)
	}
	if _, _, err := plugin.EvalValue(unknown, plugin.Input{}); err == nil ||
		!strings.Contains(err.Error(), `不认识字段 "temperature"`) {
		t.Errorf("未知 input 字段应报错，得到 %v", err)
	}
}

func TestDecodePassthrough(t *testing.T) {
	t.Run("二进制", func(t *testing.T) {
		dialect := structDialect(t, nil, &plugin.ResponseDecl{
			Mode: "passthrough",
			MIME: plugin.Value{Input: "format", Map: map[string]any{"mp3": "audio/mpeg", "wav": "audio/wav"}},
		}, "binary")
		result, err := dialect.Decode(plugin.Input{Format: "wav"}, plugin.Response{
			Status: 200, ContentType: "audio/wav", Body: []byte("RIFF"),
		})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Kind != plugin.ResultMedia || result.MIME != "audio/wav" || string(result.Media) != "RIFF" {
			t.Errorf("结果不对: kind=%s mime=%s media=%q", result.Kind, result.MIME, result.Media)
		}
	})
	t.Run("结构化", func(t *testing.T) {
		dialect := responseDialect(t, "json", `{"mode": "passthrough"}`)
		body := []byte(`{"ok": true}`)
		result, err := dialect.Decode(plugin.Input{}, plugin.Response{Status: 200, Body: body})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Kind != plugin.ResultJSON || string(result.JSON) != string(body) {
			t.Errorf("结果不对: kind=%s json=%s", result.Kind, result.JSON)
		}
	})
}

func TestDecodeJSONPointer(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		dialect := responseDialect(t, "json", `{"mode": "json_pointer", "pointer": "/choices/0/text", "encoding": "text"}`)
		result, err := dialect.Decode(plugin.Input{}, plugin.Response{
			Status: 200, Body: []byte(`{"choices": [{"text": "你好"}]}`),
		})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Kind != plugin.ResultText || result.Text != "你好" {
			t.Errorf("结果不对: kind=%s text=%q", result.Kind, result.Text)
		}
	})
	t.Run("base64", func(t *testing.T) {
		dialect := responseDialect(t, "json",
			`{"mode": "json_pointer", "pointer": "/audio", "encoding": "base64", "mime": {"const": "audio/mpeg"}}`)
		result, err := dialect.Decode(plugin.Input{}, plugin.Response{
			Status: 200, Body: []byte(`{"audio": "aGVsbG8="}`),
		})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Kind != plugin.ResultMedia || result.MIME != "audio/mpeg" || string(result.Media) != "hello" {
			t.Errorf("结果不对: kind=%s mime=%s media=%q", result.Kind, result.MIME, result.Media)
		}
	})
	t.Run("json 保留数字精度", func(t *testing.T) {
		dialect := responseDialect(t, "json", `{"mode": "json_pointer", "pointer": "/data", "encoding": "json"}`)
		result, err := dialect.Decode(plugin.Input{}, plugin.Response{
			Status: 200, Body: []byte(`{"data": {"n": 12345678901234567890}}`),
		})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Kind != plugin.ResultJSON || string(result.JSON) != `{"n":12345678901234567890}` {
			t.Errorf("结果不对: kind=%s json=%s", result.Kind, result.JSON)
		}
	})
}

func TestDecodeUsage(t *testing.T) {
	usageDialect := func(t *testing.T) plugin.Dialect {
		t.Helper()
		return responseDialect(t, "json", `{
		  "mode": "json_pointer", "pointer": "/text", "encoding": "text",
		  "usage": {
		    "input_tokens": "/usage/prompt",
		    "output_tokens": "/usage/completion",
		    "cached_input_tokens": "/usage/cached",
		    "audio_seconds": "/usage/seconds",
		    "tts_characters": "/usage/chars",
		    "total_tokens": ""
		  }
		}`)
	}

	t.Run("整数与数字字符串", func(t *testing.T) {
		result, err := usageDialect(t).Decode(plugin.Input{}, plugin.Response{
			Status: 200,
			Body:   []byte(`{"text": "hi", "usage": {"prompt": 12, "completion": "34", "cached": 5, "seconds": 3.6, "chars": 10.4}}`),
		})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		want := plugin.Usage{In: 12, Out: 34, CacheRead: 5, AudioSeconds: 4, TTSChars: 10, Source: "upstream"}
		if result.Usage != want {
			t.Errorf("用量不对\n得到: %+v\n期望: %+v", result.Usage, want)
		}
	})
	t.Run("指针缺失记 0", func(t *testing.T) {
		result, err := usageDialect(t).Decode(plugin.Input{}, plugin.Response{
			Status: 200, Body: []byte(`{"text": "hi"}`),
		})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Usage != (plugin.Usage{Source: "upstream"}) {
			t.Errorf("用量应全为 0，得到 %+v", result.Usage)
		}
	})
	t.Run("值不是数字", func(t *testing.T) {
		_, err := usageDialect(t).Decode(plugin.Input{}, plugin.Response{
			Status: 200, Body: []byte(`{"text": "hi", "usage": {"prompt": "很多"}}`),
		})
		if err == nil || !strings.Contains(err.Error(), "不是数字") {
			t.Errorf("非数字用量应报错，得到 %v", err)
		}
	})
	t.Run("没有 usage 声明", func(t *testing.T) {
		dialect := responseDialect(t, "json", `{"mode": "passthrough"}`)
		result, err := dialect.Decode(plugin.Input{}, plugin.Response{Status: 200, Body: []byte(`{}`)})
		if err != nil {
			t.Fatalf("解码响应: %v", err)
		}
		if result.Usage != (plugin.Usage{}) {
			t.Errorf("没有声明时用量应为空，得到 %+v", result.Usage)
		}
	})
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name      string
		response  string
		transport string
		body      string
		err       string
	}{
		{
			name:      "坏 JSON",
			transport: "json",
			response:  `{"mode": "json_pointer", "pointer": "/choices/0/text", "encoding": "text"}`,
			body:      `choices: nope`,
			err:       "不是合法 JSON",
		},
		{
			name:      "指针指向不存在的路径",
			transport: "json",
			response:  `{"mode": "json_pointer", "pointer": "/choices/0/text", "encoding": "text"}`,
			body:      `{"choices": []}`,
			err:       `没有 "/choices/0/text"`,
		},
		{
			name:      "JSON 之后有多余内容",
			transport: "json",
			response:  `{"mode": "json_pointer", "pointer": "/text", "encoding": "text"}`,
			body:      `{"text": "hi"} {"text": "ho"}`,
			err:       "多余内容",
		},
		{
			name:      "base64 非法",
			transport: "json",
			response:  `{"mode": "json_pointer", "pointer": "/audio", "encoding": "base64"}`,
			body:      `{"audio": "!!!"}`,
			err:       "base64 不合法",
		},
		{
			name:      "不认识的响应模式",
			transport: "json",
			response:  `{"mode": "sse_json_pointer"}`,
			body:      `{}`,
			err:       `response.mode "sse_json_pointer" 不认识`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialect := responseDialect(t, tc.transport, tc.response)
			_, err := dialect.Decode(plugin.Input{}, plugin.Response{Status: 200, Body: []byte(tc.body)})
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("期望错误含有 %q，得到 %v", tc.err, err)
			}
		})
	}
}
