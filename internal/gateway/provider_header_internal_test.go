package gateway

import (
	"strings"
	"testing"
)

// TestExpandProviderHeaderValue 钉住四种写法的展开边界：字面量原样、
// {env:NAME} 取进程环境、{session} 取会话键、{model} 取模型名；展开后为空一律不发，
// 原因只提到变量名或占位符，不带值。
func TestExpandProviderHeaderValue(t *testing.T) {
	env := map[string]string{
		"TOKEN": "tok-123",
		"EMPTY": "",
		"WRAP":  "line1\nline2",
	}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}

	cases := []struct {
		name      string
		tmpl      string
		session   string
		model     string
		want      string
		wantCause string // 空串表示这个头要发
	}{
		{name: "字面量", tmpl: "2024-10-21", want: "2024-10-21"},
		{name: "字面量里的花括号不当占位符", tmpl: "Bearer {env:TOKEN}", want: "Bearer {env:TOKEN}"},
		{name: "环境变量有值", tmpl: "{env:TOKEN}", want: "tok-123"},
		{name: "环境变量是空串", tmpl: "{env:EMPTY}", wantCause: "EMPTY 是空串"},
		{name: "环境变量没设置", tmpl: "{env:MISSING}", wantCause: "环境变量 MISSING 未设置"},
		{name: "环境变量没写名字", tmpl: "{env:}", wantCause: "没有写环境变量名"},
		{name: "会话键", tmpl: "{session}", session: "sess-1", want: "sess-1"},
		{name: "没有会话键", tmpl: "{session}", wantCause: "没有会话键"},
		{name: "模型名", tmpl: "{model}", model: "gpt-x", want: "gpt-x"},
		{name: "没有模型名", tmpl: "{model}", wantCause: "没有模型名"},
		{name: "字面量是空串", tmpl: "", wantCause: "空串"},
		{name: "环境变量里带折行", tmpl: "{env:WRAP}", wantCause: "换行"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, reason := expandProviderHeaderValue(tc.tmpl, providerHeaderInput{
				Session: tc.session, Model: tc.model, LookupEnv: lookup})
			if tc.wantCause == "" {
				if value != tc.want || reason != "" {
					t.Fatalf("展开 = %q（原因 %q），期望 %q", value, reason, tc.want)
				}
				return
			}
			if value != "" {
				t.Fatalf("展开 = %q，期望不发这个头", value)
			}
			if !strings.Contains(reason, tc.wantCause) {
				t.Fatalf("原因 = %q，期望含 %q", reason, tc.wantCause)
			}
		})
	}
}

// TestExpandProviderHeaderValueNeverLeaks 确认没发出去时给日志用的原因里不带值：
// 环境变量的值可能就是密钥，写进日志等于白存。
func TestExpandProviderHeaderValueNeverLeaks(t *testing.T) {
	const secretValue = "super-secret-token-42"
	lookup := func(string) (string, bool) { return secretValue + "\n", true }
	value, reason := expandProviderHeaderValue("{env:LEAKY}", providerHeaderInput{LookupEnv: lookup})
	if value != "" || reason == "" {
		t.Fatalf("带折行的值不该发，实际 value=%q reason=%q", value, reason)
	}
	if strings.Contains(reason, secretValue) {
		t.Fatalf("原因里出现了值：%q", reason)
	}
}
