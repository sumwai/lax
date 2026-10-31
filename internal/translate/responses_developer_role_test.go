package translate

import "testing"

// TestDecodeResponsesDeveloperRole 确认 OpenAI 的 developer 角色能解开：
// o 系列与 codex 默认把系统级约束发成 developer，Responses 里它和 system 一样只有
// instructions 一个落点，解码时折进 Request.System，与 instructions 的正文按顺序拼起来。
func TestDecodeResponsesDeveloperRole(t *testing.T) {
	raw := `{
	  "model": "deepseek-flash",
	  "instructions": "底层说明",
	  "input": [
	    {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "系统级约束"}]},
	    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "现在几点"}]}
	  ]
	}`

	req, err := DecodeResponsesRequest([]byte(raw))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if len(req.System) != 2 {
		t.Fatalf("system 块数 = %d，期望 2（instructions 与 developer 各一段）", len(req.System))
	}
	if req.System[0].Text != "底层说明" || req.System[1].Text != "系统级约束" {
		t.Fatalf("system = %+v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
		t.Fatalf("消息 = %+v，期望只剩一条 user", req.Messages)
	}
}
