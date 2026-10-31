package translate

import "testing"

// TestDecodeAnthropicMidConversationSystem 确认对话中间的 role=system 消息能解开：
// Claude Code 的 mid-conv system 就发这个形状（模型名不在它的能力表里时默认开启），
// 拒收会让整个客户端停在 400 上。位置要保留，非文本块按顶层 system 的规则丢弃。
func TestDecodeAnthropicMidConversationSystem(t *testing.T) {
	raw := `{
	  "model": "deepseek-flash",
	  "max_tokens": 64,
	  "system": "顶层指令",
	  "messages": [
	    {"role": "user", "content": "先问一句"},
	    {"role": "system", "content": [
	      {"type": "text", "text": "中途提醒"},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}}
	    ]},
	    {"role": "user", "content": "继续"}
	  ]
	}`

	req, err := DecodeAnthropicRequest([]byte(raw))
	if err != nil {
		t.Fatalf("解码请求: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3", len(req.Messages))
	}
	mid := req.Messages[1]
	if mid.Role != RoleSystem {
		t.Fatalf("第二条消息角色 = %q，期望 system", mid.Role)
	}
	if len(mid.Blocks) != 1 || mid.Blocks[0].Kind != BlockText || mid.Blocks[0].Text != "中途提醒" {
		t.Fatalf("system 消息的块 = %+v，期望只留文本块", mid.Blocks)
	}
	if req.Messages[2].Role != RoleUser {
		t.Fatalf("第三条消息角色 = %q，期望保留在 system 之后", req.Messages[2].Role)
	}
}
