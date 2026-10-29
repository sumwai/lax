package cli_test

import "testing"

// 这个文件是建账号的测试入口。账号只存厂商名，地址与默认协议都在厂商行上（见 DESIGN 5.8），
// 所以每个要建账号的用例都得先把厂商登记好；把这些步骤收在一处，用例里就不会再冒出
// 已经删掉的 --base-url / --protocol。

// providerExists 读 provider ls --json 判断厂商在不在表里。
func providerExists(t *testing.T, name string) bool {
	t.Helper()
	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	_, ok := mustProviderRows(t, out)[name]
	return ok
}

// ensureProvider 保证厂商表里有这家厂商，地址指向 baseURL，路由按 routes 声明。
// 已经存在时只把地址改过去：种子厂商的地址要靠这一步指到假上游，而它的路由与默认协议
// 属于用例自己的语义，不该被覆盖。
func ensureProvider(t *testing.T, name, baseURL string, routes ...string) {
	t.Helper()
	if providerExists(t, name) {
		if baseURL == "" {
			return
		}
		if code, _, stderr := run(t, "provider", "set", name, "--base-url", baseURL); code != 0 {
			t.Fatalf("provider set %s --base-url 退出码 = %d，stderr = %s", name, code, stderr)
		}
		return
	}
	args := []string{"provider", "add", name}
	if baseURL != "" {
		args = append(args, "--base-url", baseURL)
	}
	args = append(args, routes...)
	if code, _, stderr := run(t, args...); code != 0 {
		t.Fatalf("provider add %s 退出码 = %d，stderr = %s", name, code, stderr)
	}
}

// routeFlagsForProtocol 把协议换成一家的路由声明：只声明这一条，默认协议因此就是它
// （gemini 要显式指定，固定顺序回退里没有它，见 DESIGN 5.8）。空串按 openai-chat 算。
func routeFlagsForProtocol(protocol string) []string {
	switch protocol {
	case "anthropic":
		return []string{"--anthropic", "/v1/messages"}
	case "openai-responses":
		return []string{"--responses", "/v1/responses"}
	case "gemini":
		return []string{"--gemini", "/v1beta/models/{model}:generateContent", "--default-protocol", "gemini"}
	default:
		return []string{"--openai", "/v1/chat/completions"}
	}
}

// addAccount 是测试里建账号的入口：先按 protocol 把厂商登记到 baseURL，再跑真正的 account add。
// 返回值与 run 一致，用例照旧断退出码、stdout 与 stderr；protocol 传空串表示按厂商表的默认协议。
// baseURL 传空串时不动厂商行的地址（用种子厂商又不需要假上游的用例）。
func addAccount(t *testing.T, name, provider, baseURL, protocol string, extra ...string) (int, string, string) {
	t.Helper()
	ensureProvider(t, provider, baseURL, routeFlagsForProtocol(protocol)...)
	return run(t, append([]string{"account", "add", name, "--provider", provider}, extra...)...)
}
