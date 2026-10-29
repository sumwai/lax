package translate

import (
	"encoding/json"
	"fmt"
)

// 线协议名称。入站按路径区分，出站按账号的 protocol 字段区分。
// gemini 两边都算一条线协议：入站路径是 /v1beta/models/{model}:generateContent（见 5.9）。
const (
	ProtocolAnthropic = "anthropic"
	ProtocolChat      = "openai-chat"
	ProtocolResponses = "openai-responses"
	ProtocolGemini    = "gemini"
)

// 结束原因统一成 anthropic 的四种说法，各协议编码时再翻回自己的词。
const (
	StopEndTurn   = "end_turn"
	StopMaxTokens = "max_tokens"
	StopToolUse   = "tool_use"
	StopSequence  = "stop_sequence"
)

// Role 是消息角色。system 与 tool 只出现在 IR 里，各协议编码时落到自己的位置。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// BlockKind 是内容块的种类。
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockImage      BlockKind = "image"
	BlockToolUse    BlockKind = "tool_use"
	BlockToolResult BlockKind = "tool_result"
	BlockThinking   BlockKind = "thinking"
)

// Block 是一块内容。按 Kind 取对应字段，其它字段为零值。
type Block struct {
	Kind       BlockKind
	Text       string
	Image      *Image
	ToolUse    *ToolUse
	ToolResult *ToolResult
	Thinking   *Thinking
}

// Thinking 是一段思考内容（DESIGN 5.4）。
// Text 是正文；Signature 是 anthropic 给出的签名，只有 anthropic 用得上，没有签名时
// 不写进 anthropic 报文；Redacted 是 anthropic redacted_thinking 的不透明 data，
// 同理只有 anthropic 能原样写回，编到另两条协议时丢弃。
type Thinking struct {
	Text      string
	Signature string
	Redacted  string
}

// Image 里存的是可直接放进请求的地址，http(s) 或 data URI。
type Image struct {
	URL string
}

// ToolUse 是一次工具调用。
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult 是一次工具调用的结果。
type ToolResult struct {
	ToolUseID string
	Text      string
	IsError   bool
}

// Message 是一条消息。
type Message struct {
	Role   Role
	Blocks []Block
}

// Tool 是一个工具定义。
// Type 只有 anthropic 的服务端内置工具才有值（例如 "web_search_20250305"）；空串与
// "custom" 都表示客户端自己执行的函数，另两条协议的工具定义只认这种。
type Tool struct {
	Type        string
	Name        string
	Description string
	Schema      json.RawMessage
}

// isClientTool 判断工具定义是不是客户端自己执行的函数（DESIGN 5.4）：anthropic 的
// 服务端内置工具在 chat 与 responses 里没有对应形状，整条丢弃而不是伪造 function。
func isClientTool(t Tool) bool {
	return t.Type == "" || t.Type == "custom"
}

// emptyToolSchema 是工具参数结构缺失时补的空对象。三条协议都把参数结构当作必填：
// anthropic 的 input_schema、chat 的 function.parameters、responses 的 parameters
// 缺了会被上游拒收，补空对象只是让「没有参数」这件事表达得出来（DESIGN 5.4）。
var emptyToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// schemaOrEmpty 给缺失的参数结构补上空对象，已有内容一个字节都不改。
func schemaOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return emptyToolSchema
	}
	return raw
}

// 工具选择模式。
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceRequired = "required"
	ToolChoiceNone     = "none"
	ToolChoiceNamed    = "tool"
)

// ToolChoice 是工具选择。
type ToolChoice struct {
	Mode string
	Name string
}

// Request 是中间表示里的请求。各协议解码后都变成它，编码上游时再展开。
type Request struct {
	Model             string
	System            []Block
	Messages          []Message
	Tools             []Tool
	ToolChoice        *ToolChoice
	ParallelToolCalls *bool
	MaxTokens         int
	Temperature       *float64
	TopP              *float64
	Stop              []string
	Stream            bool
}

// Usage 是统一后的用量。
type Usage struct {
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
}

// Response 是中间表示里的响应。
// StopNote 装上游结束原因里四种取值装不下的那一档（gemini 的 SAFETY、RECITATION 等）：
// 编给客户端时丢弃，由网关写进日志当备注（DESIGN 5.9）。其余三条协议目前不填。
type Response struct {
	ID         string
	Model      string
	Blocks     []Block
	StopReason string
	StopNote   string
	Usage      Usage
}

// DeltaKind 是流式增量的种类。
type DeltaKind string

const (
	DeltaThinking  DeltaKind = "thinking"
	DeltaText      DeltaKind = "text"
	DeltaToolStart DeltaKind = "tool_start"
	DeltaToolArgs  DeltaKind = "tool_args"
	DeltaStop      DeltaKind = "stop"
	DeltaError     DeltaKind = "error"
)

// Delta 是流式过程中的一个增量。上游解码器产出它，客户端编码器消费它。
// DeltaThinking 用 Text 与 Signature 两个字段：正文与签名分别到达，可能各占一个增量；
// 只有 anthropic 给得出签名，其余两家的思考增量只有正文。StopNote 与 Response 上的同名字段
// 一个意思：装不下的结束原因，编给客户端时丢弃。
type Delta struct {
	Kind       DeltaKind
	Text       string
	Signature  string
	Index      int
	ToolID     string
	ToolName   string
	ArgsJSON   string
	StopReason string
	StopNote   string
	Usage      *Usage
}

// StreamDecoder 把上游的流式帧翻成增量。Close 在上游结束时补齐收尾。
type StreamDecoder interface {
	Decode(data []byte) ([]Delta, error)
	Close() []Delta
}

// StreamEncoder 把增量翻成下发给客户端的事件。Start 在流开始时调用一次。
type StreamEncoder interface {
	Start() []SSEEvent
	Emit(d Delta) []SSEEvent
	Finish() []SSEEvent
}

// RequestDecoder 解析客户端请求体。
type RequestDecoder func(raw []byte) (*Request, error)

// RequestEncoder 把请求编码成上游报文，model 是上游实际使用的模型名。
type RequestEncoder func(r *Request, model string) ([]byte, error)

// ResponseDecoder 解析上游响应体，requestedModel 会写回给客户端。
type ResponseDecoder func(raw []byte, requestedModel string) (*Response, error)

// ResponseEncoder 把响应编码成客户端报文。
type ResponseEncoder func(r *Response) ([]byte, error)

// DecodeRequest 按协议解析请求。
func DecodeRequest(protocol string, raw []byte) (*Request, error) {
	switch protocol {
	case ProtocolAnthropic:
		return DecodeAnthropicRequest(raw)
	case ProtocolChat:
		return DecodeChatRequest(raw)
	case ProtocolResponses:
		return DecodeResponsesRequest(raw)
	case ProtocolGemini:
		// gemini 的报文里没有模型名与 stream 标志：两者由网关按路径注入（DESIGN 5.9）。
		return DecodeGeminiRequest(raw)
	default:
		return nil, fmt.Errorf("不支持的入站协议 %q", protocol)
	}
}

// EncodeRequest 按协议编码上游请求。
func EncodeRequest(protocol string, r *Request, model string) ([]byte, error) {
	switch protocol {
	case ProtocolAnthropic:
		return EncodeAnthropicRequest(r, model)
	case ProtocolChat:
		return EncodeChatRequest(r, model)
	case ProtocolResponses:
		return EncodeResponsesRequest(r, model)
	case ProtocolGemini:
		return EncodeGeminiRequest(r, model)
	default:
		return nil, fmt.Errorf("不支持的出站协议 %q", protocol)
	}
}

// DecodeResponse 按协议解析上游响应。
func DecodeResponse(protocol string, raw []byte, requestedModel string) (*Response, error) {
	switch protocol {
	case ProtocolAnthropic:
		return DecodeAnthropicResponse(raw, requestedModel)
	case ProtocolChat:
		return DecodeChatResponse(raw, requestedModel)
	case ProtocolResponses:
		return DecodeResponsesResponse(raw, requestedModel)
	case ProtocolGemini:
		return DecodeGeminiResponse(raw, requestedModel)
	default:
		return nil, fmt.Errorf("不支持的出站协议 %q", protocol)
	}
}

// EncodeResponse 按协议编码返回给客户端的响应。四条入站协议都认，gemini 的响应体是
// candidates 加 usageMetadata（DESIGN 5.9）。
func EncodeResponse(protocol string, r *Response) ([]byte, error) {
	switch protocol {
	case ProtocolAnthropic:
		return EncodeAnthropicResponse(r)
	case ProtocolChat:
		return EncodeChatResponse(r)
	case ProtocolResponses:
		return EncodeResponsesResponse(r)
	case ProtocolGemini:
		return EncodeGeminiResponse(r)
	default:
		return nil, fmt.Errorf("不支持的入站协议 %q", protocol)
	}
}

// NewStreamDecoder 按上游协议造流式解码器。
func NewStreamDecoder(protocol string) (StreamDecoder, error) {
	switch protocol {
	case ProtocolAnthropic:
		return &anthropicStreamDecoder{}, nil
	case ProtocolChat:
		return &chatStreamDecoder{}, nil
	case ProtocolResponses:
		return &responsesStreamDecoder{}, nil
	case ProtocolGemini:
		return &geminiStreamDecoder{}, nil
	default:
		return nil, fmt.Errorf("不支持的出站协议 %q", protocol)
	}
}

// NewStreamEncoder 按入站协议造流式编码器。model 是客户端请求的模型名，会写回响应。
func NewStreamEncoder(protocol, model string) (StreamEncoder, error) {
	switch protocol {
	case ProtocolAnthropic:
		return newAnthropicStreamEncoder(model), nil
	case ProtocolChat:
		return newChatStreamEncoder(model), nil
	case ProtocolResponses:
		return newResponsesStreamEncoder(model), nil
	case ProtocolGemini:
		return newGeminiStreamEncoder(model), nil
	default:
		return nil, fmt.Errorf("不支持的入站协议 %q", protocol)
	}
}

// PathProtocol 把入站路径映射成协议。三条固定路径直接对；gemini 的模型名在路径里，
// 因此按前缀认：/v1beta/models/{model}:generateContent 与 :streamGenerateContent（DESIGN 5.9）。
// 只认路径不认方法：两者都只接 POST，方法检查在网关那边。
func PathProtocol(path string) (string, bool) {
	switch path {
	case "/v1/messages":
		return ProtocolAnthropic, true
	case "/v1/chat/completions":
		return ProtocolChat, true
	case "/v1/responses":
		return ProtocolResponses, true
	}
	if _, _, ok := GeminiPathTarget(path); ok {
		return ProtocolGemini, true
	}
	return "", false
}

// UsageScanner 从同协议直通的流式字节里读出用量，不改变转发内容。
type UsageScanner interface {
	Feed(data []byte)
	Usage() (in, out, cacheRead, cacheWrite int64)
}

// NewUsageScanner 按协议造一个直通用的用量读取器。
func NewUsageScanner(protocol string) (UsageScanner, error) {
	switch protocol {
	case ProtocolAnthropic:
		return &AnthropicUsageScanner{}, nil
	case ProtocolChat:
		return &chatUsageScanner{}, nil
	case ProtocolResponses:
		return &responsesUsageScanner{}, nil
	case ProtocolGemini:
		return &geminiUsageScanner{}, nil
	default:
		return nil, fmt.Errorf("不支持的协议 %q", protocol)
	}
}
