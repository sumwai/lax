package gateway

import (
	"strings"
	"testing"
)

// TestJoinEndpoint 钉住拼接规则：base_url 去尾斜杠（scheme 之后合并重复斜杠），
// 路径走 store.NormalizePath，最终地址的路径部分不出现 //。
// base_url 里真实存在的前缀（如 /api/paas/v4）原样保留，不推 /v1、不猜后缀。
func TestJoinEndpoint(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://api.deepseek.com", "/v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/", "/v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com", "v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com", "//v1//chat//completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com", "/v1/chat/completions/", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.example.com//", "/api/paas/v4/chat/completions", "https://api.example.com/api/paas/v4/chat/completions"},
		{"https://api.example.com//api//paas//v4//", "chat/completions", "https://api.example.com/api/paas/v4/chat/completions"},
		{"https://api.example.com", "", "https://api.example.com"},
		{"https://api.example.com/", "/", "https://api.example.com"},
	}
	for _, c := range cases {
		got := joinEndpoint(c.base, c.path)
		if got != c.want {
			t.Fatalf("joinEndpoint(%q, %q) = %q，期望 %q", c.base, c.path, got, c.want)
		}
		if rest := strings.TrimPrefix(strings.TrimPrefix(got, "https://"), "http://"); strings.Contains(rest, "//") {
			t.Fatalf("joinEndpoint(%q, %q) 的路径里出现了 //: %q", c.base, c.path, got)
		}
	}
}

// TestModelEndpoint 钉住 {model} 占位符的替换规则：只认这一个字面量，
// 出现几次换几次，其余字节不动；模型名按路径段转义，含 `/` 的名字仍是一段路径。
func TestModelEndpoint(t *testing.T) {
	cases := []struct{ endpoint, model, want string }{
		{
			"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent",
			"gemini-2.5-flash",
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
		},
		{
			"https://host/v1beta/models/{model}:generateContent?alt=json&m={model}",
			"m1",
			"https://host/v1beta/models/m1:generateContent?alt=json&m=m1",
		},
		{"https://host/v1beta/models/org/gemini:generateContent", "org/gemini",
			"https://host/v1beta/models/org/gemini:generateContent"},
		{"https://host/{model}", "org/gemini", "https://host/org%2Fgemini"},
		{"https://host/v1/chat/completions", "m1", "https://host/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := modelEndpoint(c.endpoint, c.model); got != c.want {
			t.Fatalf("modelEndpoint(%q, %q) = %q，期望 %q", c.endpoint, c.model, got, c.want)
		}
	}
}
