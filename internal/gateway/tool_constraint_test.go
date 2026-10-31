package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lax/internal/gateway"
)

// 这个文件从网关这一层钉住工具约束：tools[].strict 只在两条
// openai 协议之间有落点，跨到 anthropic 与 gemini 必须消失；tool_choice 的白名单在
// responses 保留、在 chat 降级成 auto、在 gemini 落成 ANY 加 allowedFunctionNames。
// 这里断言的是假上游真正收到的那份报文。

// allowedToolsRequest 是 anthropic 入站的一份请求：声明两个工具，白名单只准调这两个。
const allowedToolsRequest = `{"model":"m1","max_tokens":256,` +
	`"messages":[{"role":"user","content":"北京天气"}],` +
	`"tools":[` +
	`{"name":"weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}},` +
	`{"name":"search","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],` +
	`"tool_choice":{"type":"allowed_tools","tools":[{"type":"tool","name":"weather"},{"type":"tool","name":"search"}]}}`

// strictToolsRequest 是 chat 入站的一份请求：第一个工具带 strict:true，第二个没给 strict。
// 没给与给了 false 不是一回事，所以断言里要区分 nil 与 false。
const strictToolsRequest = `{"model":"m1","messages":[{"role":"user","content":"北京天气"}],` +
	`"tools":[` +
	`{"type":"function","function":{"name":"weather","description":"查天气","strict":true,` +
	`"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},` +
	`{"type":"function","function":{"name":"time","parameters":{"type":"object","properties":{}}}}]}`

// TestAnthropicAllowedToolsToResponsesUpstream 覆盖 anthropic 入站、responses 出站：
// tool_choice 编成 responses 的 allowed_tools，补 mode:"auto"，白名单名字与顺序原样搬。
func TestAnthropicAllowedToolsToResponsesUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolResponses)
	f.add(t, "rs1", gateway.ProtocolResponses, up.URL, "sk-allowed-responses", []string{"m1"}, 100)
	srv := f.server(t)

	status, resp := postTo(t, srv, allowedToolsRequest, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	assertJSONField(t, upstreamJSON(t, rec), "tool_choice",
		`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"weather"},{"type":"function","name":"search"}]}`)
}

// TestAnthropicAllowedToolsToChatUpstreamDegradesToAuto 覆盖 anthropic 入站、chat 出站：
// chat 没有白名单形状，降级成一个字符串 auto，报文里不能留下 allowed_tools 的痕迹。
func TestAnthropicAllowedToolsToChatUpstreamDegradesToAuto(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-allowed-chat", []string{"m1"}, 100)
	srv := f.server(t)

	status, resp := postTo(t, srv, allowedToolsRequest, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	got := upstreamJSON(t, rec)
	if choice, _ := got["tool_choice"].(string); choice != "auto" {
		t.Fatalf("tool_choice = %#v，期望字符串 auto，请求体 %s", got["tool_choice"], rec.body)
	}
	if strings.Contains(rec.body, "allowed_tools") {
		t.Fatalf("chat 出站不该留下白名单形状: %s", rec.body)
	}
}

// TestAnthropicAllowedToolsToGeminiUpstream 覆盖 anthropic 入站、gemini 出站：
// 白名单落成 functionCallingConfig 的 mode=ANY 加 allowedFunctionNames，名字全搬不改子集。
func TestAnthropicAllowedToolsToGeminiUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolGemini)
	f.add(t, "gem", gateway.ProtocolGemini, up.URL, "sk-allowed-gemini", []string{"m1"}, 100)
	srv := f.server(t)

	status, resp := postTo(t, srv, allowedToolsRequest, gateway.ProtocolAnthropic)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	var sent struct {
		ToolConfig struct {
			FunctionCallingConfig struct {
				Mode                 string   `json:"mode"`
				AllowedFunctionNames []string `json:"allowedFunctionNames"`
			} `json:"functionCallingConfig"`
		} `json:"toolConfig"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	cfg := sent.ToolConfig.FunctionCallingConfig
	if cfg.Mode != "ANY" {
		t.Fatalf("functionCallingConfig.mode = %q，期望 ANY，请求体 %s", cfg.Mode, rec.body)
	}
	if strings.Join(cfg.AllowedFunctionNames, ",") != "weather,search" {
		t.Fatalf("allowedFunctionNames = %v，期望 [weather search]", cfg.AllowedFunctionNames)
	}
}

// TestResponsesAllowedToolsToAnthropicUpstreamKeepsWhitelist 是同一个白名单的反方向：
// responses 入站、anthropic 出站时白名单写回 anthropic 的 allowed_tools 形状，
// 它自己的 mode 不搬（anthropic 的默认值就是 auto）。
func TestResponsesAllowedToolsToAnthropicUpstreamKeepsWhitelist(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-allowed-anthropic", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","input":"北京天气",` +
		`"tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{}}}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"weather"}]}}`
	status, resp := postTo(t, srv, body, gateway.ProtocolResponses)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	assertJSONField(t, upstreamJSON(t, rec), "tool_choice",
		`{"type":"allowed_tools","tools":[{"type":"tool","name":"weather"}]}`)
}

// TestChatToolStrictToResponsesUpstreamKept 覆盖 chat 入站、responses 出站：strict 从
// function 里搬到工具对象顶层；客户端没给 strict 的工具，上游报文里也不能凭空出现它。
func TestChatToolStrictToResponsesUpstreamKept(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolResponses)
	f.add(t, "rs1", gateway.ProtocolResponses, up.URL, "sk-strict-responses", []string{"m1"}, 100)
	srv := f.server(t)

	status, resp := postTo(t, srv, strictToolsRequest, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	var sent struct {
		Tools []struct {
			Name   string `json:"name"`
			Strict *bool  `json:"strict"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Tools) != 2 {
		t.Fatalf("工具数 = %d，期望 2: %s", len(sent.Tools), rec.body)
	}
	if sent.Tools[0].Strict == nil || !*sent.Tools[0].Strict {
		t.Fatalf("weather.strict = %v，期望 true", sent.Tools[0].Strict)
	}
	if sent.Tools[1].Strict != nil {
		t.Fatalf("time.strict = %v，期望字段缺失（客户端没给）", *sent.Tools[1].Strict)
	}
}

// TestResponsesToolStrictToChatUpstreamKept 覆盖 responses 入站、chat 出站：strict 从工具
// 对象顶层搬进 function；false 是客户端显式给的值，不能当成「没给」省掉。
func TestResponsesToolStrictToChatUpstreamKept(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolChat)
	f.add(t, "ds1", gateway.ProtocolChat, up.URL, "sk-strict-chat", []string{"m1"}, 100)
	srv := f.server(t)

	body := `{"model":"m1","input":"北京天气",` +
		`"tools":[{"type":"function","name":"weather","description":"查天气","strict":false,` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	status, resp := postTo(t, srv, body, gateway.ProtocolResponses)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	var sent struct {
		Tools []struct {
			Function struct {
				Name   string `json:"name"`
				Strict *bool  `json:"strict"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Tools) != 1 {
		t.Fatalf("工具数 = %d，期望 1: %s", len(sent.Tools), rec.body)
	}
	fn := sent.Tools[0].Function
	if fn.Name != "weather" {
		t.Fatalf("工具名 = %q，期望 weather", fn.Name)
	}
	if fn.Strict == nil || *fn.Strict {
		t.Fatalf("weather.strict = %v，期望 false（客户端显式给的 false 要保留）", fn.Strict)
	}
}

// TestChatToolStrictDroppedToAnthropicUpstream 覆盖 chat 入站、anthropic 出站：anthropic
// 的工具定义里没有 strict 这个字段，写出去会让上游以未知字段拒掉整条请求。
func TestChatToolStrictDroppedToAnthropicUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolAnthropic)
	f.add(t, "claude", gateway.ProtocolAnthropic, up.URL, "sk-strict-dropped-anthropic", []string{"m1"}, 100)
	srv := f.server(t)

	status, resp := postTo(t, srv, strictToolsRequest, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	if strings.Contains(rec.body, "strict") {
		t.Fatalf("anthropic 出站不该有 strict 字段: %s", rec.body)
	}
	var sent struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Tools) != 2 || sent.Tools[0].Name != "weather" || sent.Tools[1].Name != "time" {
		t.Fatalf("工具定义 = %+v，期望 weather 与 time 两条都还在", sent.Tools)
	}
}

// TestChatToolStrictDroppedToGeminiUpstream 覆盖 chat 入站、gemini 出站：functionDeclarations
// 里同样没有 strict，工具本身仍按声明搬过去。
func TestChatToolStrictDroppedToGeminiUpstream(t *testing.T) {
	f := newFixture(t)
	up, rec := fakeProtocolUpstream(t, gateway.ProtocolGemini)
	f.add(t, "gem", gateway.ProtocolGemini, up.URL, "sk-strict-dropped-gemini", []string{"m1"}, 100)
	srv := f.server(t)

	status, resp := postTo(t, srv, strictToolsRequest, gateway.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, resp)
	}

	if strings.Contains(rec.body, "strict") {
		t.Fatalf("gemini 出站不该有 strict 字段: %s", rec.body)
	}
	var sent struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Name string `json:"name"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v，%s", err, rec.body)
	}
	if len(sent.Tools) != 1 || len(sent.Tools[0].FunctionDeclarations) != 2 {
		t.Fatalf("functionDeclarations = %+v，期望 2 条声明", sent.Tools)
	}
	if got := sent.Tools[0].FunctionDeclarations[0].Name; got != "weather" {
		t.Fatalf("第一条声明 = %q，期望 weather", got)
	}
}
