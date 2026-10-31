package gateway

import (
	"strings"
	"testing"
)

// TestEndpointProtocolMapping 逐项钉住「上游声明的端点名 → 线协议」那张表：
// 表里四个端点名各一条，加上两个写法（带不带前导斜杠的 generateContent），
// 其余写法一律不计——未识别的端点名不猜。
func TestEndpointProtocolMapping(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
		ok       bool
	}{
		{"commandcode 的 anthropic 端点", "/messages", ProtocolAnthropic, true},
		{"openai chat 端点", "/chat/completions", ProtocolChat, true},
		{"responses 端点", "/responses", ProtocolResponses, true},
		{"gemini 端点带斜杠", "/generateContent", ProtocolGemini, true},
		{"gemini 端点不带斜杠", "generateContent", ProtocolGemini, true},
		{"只认表里列出的写法，大小写不猜", "/Messages", "", false},
		{"向量化端点折成 openai-embeddings", "/embeddings", ProtocolEmbeddings, true},
		{"空串不计", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := endpointProtocol(c.endpoint)
			if ok != c.ok || got != c.want {
				t.Fatalf("endpointProtocol(%q) = %q, %v，期望 %q, %v", c.endpoint, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestEntryProtocols 钉住条目两处协议来源的折算口径：supported_endpoints 按表折算，
// supportedGenerationMethods 只有 generateContent 折成 gemini（countTokens、embedContent 是接口动作），
// 两个键都没有时留空。顺序固定，与条目里列出的先后无关。
func TestEntryProtocols(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{
			"端点名按固定顺序排列，重复与未识别的丢掉",
			`{"supported_endpoints":["/responses","/messages","/chat/completions","/nope","/messages"]}`,
			"anthropic,openai-chat,openai-responses",
		},
		{
			"generateContent 折成 gemini 协议",
			`{"supportedGenerationMethods":["generateContent","countTokens"]}`,
			"gemini",
		},
		{
			"countTokens 与 embedContent 不算协议",
			`{"supportedGenerationMethods":["countTokens","embedContent"]}`,
			"",
		},
		{"两个键都没有时留空", `{"id":"m1","context_length":1000}`, ""},
		{"条目是字符串时没有元数据", `"m1"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(entryProtocols(rawJSON(c.entry)), ",")
			if got != c.want {
				t.Fatalf("协议集合 = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestJSONPositiveInt 钉住长度类字段的取值口径：只收正数，0、负数、非整数与解不成数字的都算
// 「上游没给」，不拿 0 冒充上下文长度。
func TestJSONPositiveInt(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{"整数", `200000`, 200000, true},
		{"整数值的浮点", `200000.0`, 200000, true},
		{"0 当作没给", `0`, 0, false},
		{"负数当作没给", `-1`, 0, false},
		{"非整数不收", `1.5`, 0, false},
		{"字符串不收", `"200000"`, 0, false},
		{"null 当作没给", `null`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jsonPositiveInt(rawJSON(c.raw))
			if (got != nil) != c.ok {
				t.Fatalf("jsonPositiveInt(%s) = %v，期望有没有值 = %v", c.raw, got, c.ok)
			}
			if got != nil && *got != c.want {
				t.Fatalf("jsonPositiveInt(%s) = %d，期望 %d", c.raw, *got, c.want)
			}
		})
	}
}

// rawJSON 把测试里的 JSON 字面量换成 json.RawMessage，省掉每处重复的类型转换。
func rawJSON(s string) []byte { return []byte(s) }
