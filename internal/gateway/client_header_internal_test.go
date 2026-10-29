package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// TestClientHeaderWhitelistDoesNotOverlapNeverForward 钉住两张表不相交：白名单里的名字永远不能出现在
// 「永远不透传」名单里，否则第二道闸就成了实际生效的那道，白名单读起来会骗人。
func TestClientHeaderWhitelistDoesNotOverlapNeverForward(t *testing.T) {
	for _, rule := range clientHeaderRules {
		for _, name := range rule.names {
			if isNeverForwardedHeader(name) {
				t.Fatalf("%s 的白名单里有 %s，但它在永远不透传名单里", rule.protocol, name)
			}
		}
	}
	// gemini 的白名单是空的（DESIGN 5.11）：表里不该出现它。
	for _, rule := range clientHeaderRules {
		if rule.protocol == ProtocolGemini {
			t.Fatalf("gemini 不该有透传规则，实际 %q", rule.names)
		}
	}
	// 必须被挡下的三个凭据头与逐跳头都得在名单里。
	for _, name := range []string{"Authorization", "x-api-key", "x-goog-api-key", "Cookie",
		"Host", "Connection", "Content-Length", "Transfer-Encoding", "Expect", "Upgrade",
		"x-switchboard-session"} {
		if !isNeverForwardedHeader(name) {
			t.Fatalf("%s 应该在永远不透传名单里", name)
		}
	}
}

// TestApplyClientHeadersAddOrderAndEmptyValues 钉住纯函数的行为：同名多值按顺序 Add；
// 空值不设；跨协议一个都不透传；超长整条丢弃并返回头名与长度。
func TestApplyClientHeadersAddOrderAndEmptyValues(t *testing.T) {
	inbound := http.Header{}
	inbound.Add("anthropic-beta", "beta-1")
	inbound.Add("anthropic-beta", "beta-2")
	inbound.Set("anthropic-version", "")
	inbound.Set("openai-beta", "chat-only")

	req, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("anthropic-version", "2023-06-01") // setAuth 的默认值
	dropped := applyClientHeaders(req, inbound, ProtocolAnthropic, ProtocolAnthropic)
	if len(dropped) != 0 {
		t.Fatalf("没有超长值时不该有丢弃，实际 %+v", dropped)
	}
	if got := req.Header.Values("anthropic-beta"); len(got) != 2 || got[0] != "beta-1" || got[1] != "beta-2" {
		t.Fatalf("anthropic-beta = %q，期望两个值按出现顺序", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q，空值不该覆盖默认值", got)
	}
	if got := req.Header.Get("openai-beta"); got != "" {
		t.Fatalf("anthropic 的白名单里没有 openai-beta，实际%q", got)
	}

	// 客户端给了 anthropic-version 时只留客户端那一个：默认值不能与它并存。
	withVersion := http.Header{}
	withVersion.Set("anthropic-version", "2024-10-22")
	versioned, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	versioned.Header.Set("anthropic-version", "2023-06-01")
	applyClientHeaders(versioned, withVersion, ProtocolAnthropic, ProtocolAnthropic)
	if got := versioned.Header.Values("anthropic-version"); len(got) != 1 || got[0] != "2024-10-22" {
		t.Fatalf("anthropic-version = %q，期望只剩客户端的 2024-10-22", got)
	}

	// 跨协议：一个都不透传，已有的头不动。
	cross, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	cross.Header.Set("Authorization", "Bearer account-key")
	if dropped := applyClientHeaders(cross, inbound, ProtocolChat, ProtocolAnthropic); len(dropped) != 0 {
		t.Fatalf("跨协议不该有丢弃记录，实际 %+v", dropped)
	}
	if got := cross.Header.Get("anthropic-beta"); got != "" {
		t.Fatalf("跨协议时 anthropic-beta 不该透传，实际 %q", got)
	}
	if got := cross.Header.Get("Authorization"); got != "Bearer account-key" {
		t.Fatalf("跨协议时不该动已有的头，Authorization = %q", got)
	}
}

// TestApplyClientHeadersDropsOversizedValue 钉住 8KB 上限：只要有一个值超长就整条不发，
// 返回值给出头名与那个值的长度，且不含值本身。
func TestApplyClientHeadersDropsOversizedValue(t *testing.T) {
	inbound := http.Header{}
	inbound.Add("openai-beta", "short")
	inbound.Add("openai-beta", strings.Repeat("B", clientHeaderMaxBytes+1))
	inbound.Set("openai-project", strings.Repeat("C", clientHeaderMaxBytes)) // 正好 8KB 不算超过

	req, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	dropped := applyClientHeaders(req, inbound, ProtocolChat, ProtocolChat)
	if len(dropped) != 1 {
		t.Fatalf("丢弃记录 = %+v，期望只有一条", dropped)
	}
	if dropped[0].Name != "openai-beta" || dropped[0].Bytes != clientHeaderMaxBytes+1 {
		t.Fatalf("丢弃记录 = %+v，期望 openai-beta 与 %d 字节", dropped[0], clientHeaderMaxBytes+1)
	}
	if got := req.Header.Values("openai-beta"); len(got) != 0 {
		t.Fatalf("超长时整条头都不该发，实际 %d 个值", len(got))
	}
	if got := req.Header.Get("openai-project"); len(got) != clientHeaderMaxBytes {
		t.Fatalf("正好 8KB 该照常透传，实际长度 %d", len(got))
	}
}

// TestApplyClientHeadersNilHeader 确认没有入站头时（探活与同步传 nil）什么也不做，不 panic。
func TestApplyClientHeadersNilHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/messages", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if dropped := applyClientHeaders(req, nil, ProtocolAnthropic, ProtocolAnthropic); len(dropped) != 0 {
		t.Fatalf("nil 入站头不该有丢弃记录，实际 %+v", dropped)
	}
	if len(req.Header) != 0 {
		t.Fatalf("nil 入站头不该加上任何头，实际 %v", req.Header)
	}
}
