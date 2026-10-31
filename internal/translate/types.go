// Package translate 在入站协议（anthropic、openai-chat、openai-responses、gemini）与出站协议
// （同四条，见 gemini.go）之间转换报文。
// 这里全是纯函数：不读环境、不发请求、不碰时间。
package translate

import (
	"bytes"
	"encoding/json"
)

// AnthropicRequest 是入站 /v1/messages 的请求体。
// thinking 是请求侧的思考配置参数，与消息里的 thinking 块
// 不是一件事：那边是上游给出的思考正文。
type AnthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	System        json.RawMessage      `json:"system,omitempty"`
	Messages      []AnthropicMessage   `json:"messages"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking      *AnthropicThinking   `json:"thinking,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage      `json:"metadata,omitempty"`
}

// AnthropicThinking 是顶层的 thinking 字段：type 为 enabled 时带预算，为 disabled 时
// 只有 type，disabled 与 enabled 靠这一个字段区分。
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// AnthropicMessage 是一条消息，content 可能是字符串或块数组。
type AnthropicMessage struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// Content 表示 anthropic 的 content 字段：字符串或块数组。
type Content struct {
	Text   *string
	Blocks []ContentBlock
}

// ContentBlock 是 content 数组里的一块。
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
	// 思考内容：thinking 块用 Thinking 与 Signature，redacted_thinking 块用 Data。
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// ImageSource 是 image 块的来源。anthropic 把图片写成 base64（data + media_type）或 url，两种写法
// 都落在这里；编到别的协议时按目标支持的那一种输出，另一个字段留空。
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicTool 是入站工具定义。Type 是 anthropic 服务端内置工具的种类
// （例如 "web_search_20250305"），客户端自己执行的函数没有这个字段。
type AnthropicTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthropicToolChoice 是入站工具选择。
// allowed_tools 这一档用 Tools 装白名单（每项形如 {"type":"tool","name":…}）；anthropic
// 另有一个可选的 mode（auto 或 any），中间表示不搬它：编出去时一律按 auto 写。
type AnthropicToolChoice struct {
	Type                   string                   `json:"type"`
	Name                   string                   `json:"name,omitempty"`
	DisableParallelToolUse bool                     `json:"disable_parallel_tool_use,omitempty"`
	Tools                  []AnthropicToolChoiceRef `json:"tools,omitempty"`
}

// AnthropicToolChoiceRef 是 allowed_tools 白名单里的一项。
type AnthropicToolChoiceRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// AnthropicResponse 是返回给客户端 /v1/messages 的响应体。
type AnthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        AnthropicUsage `json:"usage"`
}

// AnthropicUsage 是 anthropic 的用量字段。
type AnthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// UnmarshalJSON 让 Content 同时接受字符串与块数组。
func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*c = Content{}
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content{Text: &s}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(b, &blocks); err != nil {
		return err
	}
	*c = Content{Blocks: blocks}
	return nil
}

// MarshalJSON 按原样写回字符串或块数组。
func (c Content) MarshalJSON() ([]byte, error) {
	if c.Text != nil {
		return json.Marshal(*c.Text)
	}
	blocks := c.Blocks
	if blocks == nil {
		blocks = []ContentBlock{}
	}
	return json.Marshal(blocks)
}

// All 返回块数组，字符串形式视为一个 text 块。
func (c Content) All() []ContentBlock {
	if c.Text != nil {
		return []ContentBlock{{Type: "text", Text: *c.Text}}
	}
	return c.Blocks
}

// Plain 把文本块按顺序拼成一个字符串，忽略其它类型的块。
func (c Content) Plain() string {
	var buf bytes.Buffer
	for _, b := range c.All() {
		if b.Type == "text" {
			buf.WriteString(b.Text)
		}
	}
	return buf.String()
}

// ChatRequest 是发往 openai-chat 上游的请求体（入站与出站同一形状）。
// ReasoningEffort 与 Reasoning 都是请求侧的思考配置参数：前者是主流写法，后者是少数实现
// 把同一个值包在对象里的写法，解码时两者都读，编码时只写 ReasoningEffort：
// 两条都写会让上游收到两个可能矛盾的档位。
type ChatRequest struct {
	Model             string             `json:"model"`
	Messages          []ChatMessage      `json:"messages"`
	MaxTokens         int                `json:"max_tokens,omitempty"`
	Temperature       *float64           `json:"temperature,omitempty"`
	TopP              *float64           `json:"top_p,omitempty"`
	Stop              []string           `json:"stop,omitempty"`
	Tools             []ChatTool         `json:"tools,omitempty"`
	ToolChoice        any                `json:"tool_choice,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
	Reasoning         *ChatReasoning     `json:"reasoning,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *ChatStreamOptions `json:"stream_options,omitempty"`
}

// ChatReasoning 是少数实现把档位包一层的写法：只读 effort，其余子字段没有落点，丢弃。
type ChatReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// ChatStreamOptions 让上游在流式响应里带上用量。
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage 是 openai-chat 的一条消息。
type ChatMessage struct {
	Role string `json:"role"`
	// ReasoningContent 是思考内容正文，主流实现在这里；编码时只写这个字段。
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Reasoning 只在解码时读：少数实现把同一段正文写在 reasoning 字段。
	Reasoning  string         `json:"reasoning,omitempty"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// ChatToolCall 是 assistant 发起的工具调用。
type ChatToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatFunctionCall `json:"function"`
}

// ChatFunctionCall 是工具调用的函数部分。
type ChatFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ChatTool 是 openai-chat 的工具定义。
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatToolFunc `json:"function"`
}

// ChatToolFunc 是工具定义的函数部分。
// Strict 是严格模式开关（参数按 parameters 校验）：nil 表示客户端没给，非 nil 时写出去
// 与搬进中间表示。两种情况不能合成一个 false，否则往返一次就把「没开」变成了「显式关」。
type ChatToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatResponse 是 openai-chat 的非流式响应。
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatChoice 是响应里的一个候选。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatUsage 是 openai-chat 的用量字段。
type ChatUsage struct {
	PromptTokens            int64                  `json:"prompt_tokens"`
	CompletionTokens        int64                  `json:"completion_tokens"`
	TotalTokens             int64                  `json:"total_tokens"`
	PromptTokensDetails     *PromptTokenDetail     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokenDetail `json:"completion_tokens_details,omitempty"`
}

// PromptTokenDetail 携带命中缓存的输入 token 数。
type PromptTokenDetail struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// CompletionTokenDetail 携带输出里的思考 token 数。各家口径不同：OpenAI 官方把这部分算在
// completion_tokens 里（子集），有的中转把它报成输出之外的一项、只相加到 total。入账时按
// total 判定要不要另算一遍，不按协议猜：同一个协议的不同中转也会给出不同口径。
type CompletionTokenDetail struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ChatStreamChunk 是流式响应的一帧。
type ChatStreamChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []ChatChunk `json:"choices"`
	Usage   *ChatUsage  `json:"usage,omitempty"`
}

// ChatChunk 是流式响应里的一个候选增量。
type ChatChunk struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

// ChatDelta 是增量内容。
type ChatDelta struct {
	Role             string         `json:"role,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Reasoning        string         `json:"reasoning,omitempty"`
	Content          string         `json:"content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}
