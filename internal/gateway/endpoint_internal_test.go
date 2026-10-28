package gateway

import (
	"strings"
	"testing"
)

// TestJoinEndpoint 钉住拼接规则：base_url 去尾斜杠（scheme 之后合并重复斜杠），
// 路径走 store.NormalizePath，最终地址在 scheme 之外不出现 //。
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
			t.Fatalf("joinEndpoint(%q, %q) 在 scheme 之外出现了 //: %q", c.base, c.path, got)
		}
	}
}
