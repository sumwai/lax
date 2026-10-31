package translate

import (
	"encoding/json"
	"testing"
)

// irTestRequest 返回一条最小可编码的请求：四种协议都编得出来（anthropic 与 gemini 都要
// 至少一条消息）。Thinking、Tools、ToolChoice 由调用方按用例补。
func irTestRequest() *Request {
	return &Request{
		Model:     "m1",
		MaxTokens: 256,
		Messages: []Message{{Role: RoleUser, Blocks: []Block{
			{Kind: BlockText, Text: "只答一句"},
		}}},
	}
}

// irEncodeJSON 把请求编成报文并按通用值返回，方便断言某个字段在不在。
func irEncodeJSON(t *testing.T, protocol string, r *Request) map[string]any {
	t.Helper()
	raw, err := EncodeRequest(protocol, r, "up-model")
	if err != nil {
		t.Fatalf("编码 %s 请求: %v", protocol, err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("编码结果不是合法 JSON: %v（%s）", err, raw)
	}
	return got
}

// irGenerationConfig 取 gemini 请求里的 generationConfig，没有时返回空 map。
func irGenerationConfig(got map[string]any) map[string]any {
	gen, _ := got["generationConfig"].(map[string]any)
	if gen == nil {
		return map[string]any{}
	}
	return gen
}

// TestThinkingNilWritesNothing 覆盖思考配置参数的第一条：客户端没给这类字段时
// 一个字节都不写，不凭协议默认值加。
func TestThinkingNilWritesNothing(t *testing.T) {
	for _, protocol := range []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses, ProtocolGemini} {
		t.Run(protocol, func(t *testing.T) {
			r := irTestRequest()
			if r.Thinking != nil {
				t.Fatal("Thinking 默认应该是 nil")
			}
			got := irEncodeJSON(t, protocol, r)
			for _, key := range []string{"thinking", "reasoning_effort", "reasoning"} {
				if _, ok := got[key]; ok {
					t.Fatalf("Thinking 为 nil 时不该写 %q: %v", key, got)
				}
			}
			if _, ok := irGenerationConfig(got)["thinkingConfig"]; ok {
				t.Fatalf("Thinking 为 nil 时不该写 thinkingConfig: %v", got)
			}
		})
	}
}

// TestThinkingDisabledShapes 覆盖 Enabled=false 的三种目标形状：anthropic 写 disabled，
// 两条 openai 协议不写（没有「不思考」的档位），gemini 写 thinkingBudget:0。
func TestThinkingDisabledShapes(t *testing.T) {
	r := irTestRequest()
	r.Thinking = &ThinkingConfig{}

	got := irEncodeJSON(t, ProtocolAnthropic, r)
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("anthropic 的 thinking = %v，期望 {type:disabled}", got["thinking"])
	}
	if _, ok := thinking["budget_tokens"]; ok {
		t.Fatalf("disabled 不该带 budget_tokens: %v", thinking)
	}

	for _, protocol := range []string{ProtocolChat, ProtocolResponses} {
		got := irEncodeJSON(t, protocol, r)
		if _, ok := got["reasoning_effort"]; ok {
			t.Fatalf("%s 不该写 reasoning_effort: %v", protocol, got)
		}
		if _, ok := got["reasoning"]; ok {
			t.Fatalf("%s 不该写 reasoning: %v", protocol, got)
		}
	}

	got = irEncodeJSON(t, ProtocolGemini, r)
	tc, ok := irGenerationConfig(got)["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("gemini 该写 thinkingConfig: %v", got)
	}
	if budget, ok := tc["thinkingBudget"].(float64); !ok || budget != 0 {
		t.Fatalf("gemini 的 thinkingBudget = %v，期望 0", tc["thinkingBudget"])
	}
}

// TestThinkingBudgetAndEffortBothGiven 覆盖「预算与档位都有时」的取舍：按目标协议能直接
// 表达的那一个写——anthropic 与 gemini 用预算，两条 openai 协议用档位。
func TestThinkingBudgetAndEffortBothGiven(t *testing.T) {
	r := irTestRequest()
	r.Thinking = &ThinkingConfig{Enabled: true, Budget: 4096, Effort: "high"}

	got := irEncodeJSON(t, ProtocolAnthropic, r)
	thinking, _ := got["thinking"].(map[string]any)
	if budget, ok := thinking["budget_tokens"].(float64); !ok || budget != 4096 {
		t.Fatalf("anthropic 的 budget_tokens = %v，期望原样传 4096（不是档位折出来的 8192）", thinking["budget_tokens"])
	}

	got = irEncodeJSON(t, ProtocolGemini, r)
	tc, _ := irGenerationConfig(got)["thinkingConfig"].(map[string]any)
	if budget, ok := tc["thinkingBudget"].(float64); !ok || budget != 4096 {
		t.Fatalf("gemini 的 thinkingBudget = %v，期望原样传 4096", tc["thinkingBudget"])
	}

	got = irEncodeJSON(t, ProtocolChat, r)
	if got["reasoning_effort"] != "high" {
		t.Fatalf("chat 的 reasoning_effort = %v，期望档位直传 high", got["reasoning_effort"])
	}

	got = irEncodeJSON(t, ProtocolResponses, r)
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("responses 的 reasoning = %v，期望 effort 直传 high", got["reasoning"])
	}
}

// TestThinkingFoldsBetweenUnits 覆盖预算与档位的双向折算：预算进 openai 折成档位，
// 档位进 anthropic 与 gemini 折成预算。
func TestThinkingFoldsBetweenUnits(t *testing.T) {
	budgets := []struct {
		budget int
		effort string
	}{
		{budget: 8192, effort: "high"},
		{budget: 2048, effort: "medium"},
		{budget: 1024, effort: "low"},
		// 4096 落在 medium 档（≥2048），与「4096 折回 4096」的档位值巧合但规则不同。
		{budget: 4096, effort: "medium"},
	}
	for _, c := range budgets {
		r := irTestRequest()
		r.Thinking = &ThinkingConfig{Enabled: true, Budget: c.budget}
		got := irEncodeJSON(t, ProtocolChat, r)
		if got["reasoning_effort"] != c.effort {
			t.Fatalf("预算 %d 折成 %v，期望 %s", c.budget, got["reasoning_effort"], c.effort)
		}
	}

	efforts := []struct {
		effort string
		budget int
	}{
		{effort: "minimal", budget: 1024},
		{effort: "low", budget: 2048},
		{effort: "medium", budget: 4096},
		{effort: "high", budget: 8192},
	}
	for _, c := range efforts {
		r := irTestRequest()
		r.Thinking = &ThinkingConfig{Enabled: true, Effort: c.effort}
		got := irEncodeJSON(t, ProtocolAnthropic, r)
		thinking, _ := got["thinking"].(map[string]any)
		if budget, ok := thinking["budget_tokens"].(float64); !ok || int(budget) != c.budget {
			t.Fatalf("档位 %s 折成 %v，期望 %d", c.effort, thinking["budget_tokens"], c.budget)
		}
	}
}

// TestThinkingUnknownEffortNotWritten 覆盖认不出的档位：anthropic 与 gemini 没有对应的预算
// 可写，宁可整块不写，也不编一个客户端没说过的深度。
func TestThinkingUnknownEffortNotWritten(t *testing.T) {
	r := irTestRequest()
	r.Thinking = &ThinkingConfig{Enabled: true, Effort: "turbo"}

	got := irEncodeJSON(t, ProtocolAnthropic, r)
	if _, ok := got["thinking"]; ok {
		t.Fatalf("认不出的档位不该写 anthropic 的 thinking: %v", got)
	}
	got = irEncodeJSON(t, ProtocolGemini, r)
	if _, ok := irGenerationConfig(got)["thinkingConfig"]; ok {
		t.Fatalf("认不出的档位不该写 gemini 的 thinkingConfig: %v", got)
	}

	// 两条 openai 协议上档位是本协议的原生值，认不认识都直传。
	got = irEncodeJSON(t, ProtocolChat, r)
	if got["reasoning_effort"] != "turbo" {
		t.Fatalf("chat 的 reasoning_effort = %v，期望直传 turbo", got["reasoning_effort"])
	}
}

// TestAllowedToolsWithoutNamesWritesNothing 覆盖空白名单：这一档说的是「只准调这些」，
// 一个名字都没有时编一个空数组只会换来上游 400 或语义漂移，因此整条 tool_choice 不写，
// 由上游按自己的默认值走。
func TestAllowedToolsWithoutNamesWritesNothing(t *testing.T) {
	for _, protocol := range []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses, ProtocolGemini} {
		t.Run(protocol, func(t *testing.T) {
			r := irTestRequest()
			r.Tools = []Tool{{Name: "weather", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}}
			r.ToolChoice = &ToolChoice{Mode: ToolChoiceAllowedTools}
			got := irEncodeJSON(t, protocol, r)
			if _, ok := got["tool_choice"]; ok {
				t.Fatalf("%s 不该编出空白名单: %v", protocol, got["tool_choice"])
			}
			if _, ok := got["toolConfig"]; ok {
				t.Fatalf("%s 不该编出空的 toolConfig: %v", protocol, got["toolConfig"])
			}
		})
	}
}

// TestToolStrictOnlyTravelsBetweenOpenAIProtocols 覆盖 strict 的落点：两种 openai 协议之间
// 原样搬运（nil 与 false 不是一回事），编到 anthropic 与 gemini 时丢弃。
func TestToolStrictOnlyTravelsBetweenOpenAIProtocols(t *testing.T) {
	no := false
	yes := true

	r := irTestRequest()
	r.Tools = []Tool{
		{Name: "a", Schema: json.RawMessage(`{"type":"object","properties":{}}`), Strict: &yes},
		{Name: "b", Schema: json.RawMessage(`{"type":"object","properties":{}}`), Strict: &no},
		{Name: "c", Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
	}

	got := irEncodeJSON(t, ProtocolChat, r)
	tools, _ := got["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("chat 的 tools 数量 = %d", len(tools))
	}
	wantStrict := []any{true, false, nil}
	for i, want := range wantStrict {
		fn, _ := tools[i].(map[string]any)["function"].(map[string]any)
		strict, ok := fn["strict"]
		if want == nil {
			if ok {
				t.Fatalf("tools[%d] 没给 strict，编出来却有: %v", i, strict)
			}
			continue
		}
		if !ok || strict != want {
			t.Fatalf("tools[%d] 的 strict = %v（存在 %v），期望 %v", i, strict, ok, want)
		}
	}

	got = irEncodeJSON(t, ProtocolResponses, r)
	tools, _ = got["tools"].([]any)
	for i, tool := range tools {
		obj, _ := tool.(map[string]any)
		if i == 2 {
			if _, ok := obj["strict"]; ok {
				t.Fatalf("tools[2] 没给 strict，编出来却有: %v", obj["strict"])
			}
			continue
		}
		if _, ok := obj["strict"]; !ok {
			t.Fatalf("tools[%d] 的 strict 丢了: %v", i, obj)
		}
	}

	for _, protocol := range []string{ProtocolAnthropic, ProtocolGemini} {
		got := irEncodeJSON(t, protocol, r)
		if _, ok := got["strict"]; ok {
			t.Fatalf("%s 不该有顶层 strict: %v", protocol, got)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if containsKey(got, "strict") {
			t.Fatalf("%s 的工具定义里不该出现 strict: %s", protocol, raw)
		}
	}
}

// containsKey 递归看通用值里有没有某个键。
func containsKey(v any, key string) bool {
	switch val := v.(type) {
	case map[string]any:
		if _, ok := val[key]; ok {
			return true
		}
		for _, inner := range val {
			if containsKey(inner, key) {
				return true
			}
		}
	case []any:
		for _, inner := range val {
			if containsKey(inner, key) {
				return true
			}
		}
	}
	return false
}
