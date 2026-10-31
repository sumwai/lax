package translate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// 线协议名称。入站按路径区分，出站按账号的 protocol 字段区分。
// gemini 两边都算一条线协议：入站路径是 /v1beta/models/{model}:generateContent（见 5.9）。
// systemone 是决策协议（TypeSafe System One / Jev）：入站与出站都是 POST /v1/systemone，
// 只与自己互通、不经中间表示转换，存储层只把它当成第五个协议取值（见 5.14）。
const (
	ProtocolAnthropic = "anthropic"
	ProtocolChat      = "openai-chat"
	ProtocolResponses = "openai-responses"
	ProtocolGemini    = "gemini"
	ProtocolSystemOne = "systemone"
	// ProtocolEmbeddings 是向量化的线协议（OpenAI 的 /v1/embeddings 报文，见 5.15）。
	// 它没有消息序列，编解码器也不认它：网关只做同协议直通，translate 只用这个常量。
	ProtocolEmbeddings = "openai-embeddings"
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
	// RoleDeveloper 是 OpenAI 对 system 那一层的另一种叫法（o 系列与 codex 默认发这个）。
	// 中间表示里不保留它：解码时与 RoleSystem 同等处理，编码时永远不写 developer。
	RoleDeveloper Role = "developer"
)

// replaySignaturePrefix 标记本网关自己盖的思考签名（DESIGN 5.4）。chat 与 responses 没
// 有签名这个字段，而 anthropic 客户端只回传带签名的 thinking 块；上游要求把上一轮 CoT
// 原样回传时（DeepSeek 的 thinking 模式带 tools 就是这样），网关靠这个签名让 CoT 经客户
// 端往返一圈。签名是正文的子迹：同一段正文得到同一个签名，客户端改过正文就对不上。
const replaySignaturePrefix = "lax-replay:"

func replaySignature(text string) string {
	sum := sha256.Sum256([]byte(text))
	return replaySignaturePrefix + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func isReplaySignature(sig string) bool {
	return strings.HasPrefix(sig, replaySignaturePrefix)
}

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
// Strict 是两种 openai 协议共有的严格模式开关（参数按 schema 校验），nil 表示客户端没给
// 这个字段，与给了 false 不是一回事：编回 openai 时只有非 nil 才写出去（DESIGN 5.4
// 「工具定义与选择」）。
type Tool struct {
	Type        string
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
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
	// ToolChoiceAllowedTools 是「只准调白名单里的这些工具」：anthropic 与 responses 有
	// 原生形状，chat 没有（降级为 auto），gemini 用 ANY 加 allowedFunctionNames 表达
	// （DESIGN 5.4「工具定义与选择」）。
	ToolChoiceAllowedTools = "allowed_tools"
)

// ToolChoice 是工具选择。Name 只在 Mode 为 tool 时有值，Allowed 只在 Mode 为
// allowed_tools 时有值。
type ToolChoice struct {
	Mode    string
	Name    string
	Allowed []string
}

// ThinkingConfig 是请求侧的思考配置（DESIGN 5.4「思考配置参数」）：客户端要不要模型思考、
// 允许它想多深。它与 BlockKind 为 thinking 的内容块是两件事——这里是请求参数，那边是
// 上游在响应里给出的思考正文（类型名与 Thinking 分开，避免两件事共用一个名字）。
// Budget 是 token 预算（anthropic 与 gemini 用），Effort 是档位（两条 openai 协议用），
// 两者二选一：解码时按来源协议原样保留，编码时优先用目标协议能直接表达的那一个。
type ThinkingConfig struct {
	Enabled bool
	Budget  int
	Effort  string
}

// 档位与预算的换算（DESIGN 5.4「思考配置参数」的映射表）。换算是近似的：预算折成档位再折
// 回来会丢精度，取舍是要让客户端设的值在另一条协议上起作用的代价。
const (
	thinkingBudgetMinimal = 1024
	thinkingBudgetLow     = 2048
	thinkingBudgetMedium  = 4096
	thinkingBudgetHigh    = 8192
)

// thinkingBudgetFromEffort 把 openai 的档位折成 token 预算，认不出的档位返回 0。
// 认不出的档位不猜：anthropic 与 gemini 那边宁可不写，也不编一个上游没说的深度。
func thinkingBudgetFromEffort(effort string) int {
	switch effort {
	case "minimal":
		return thinkingBudgetMinimal
	case "low":
		return thinkingBudgetLow
	case "medium":
		return thinkingBudgetMedium
	case "high":
		return thinkingBudgetHigh
	default:
		return 0
	}
}

// thinkingEffortFromBudget 把 token 预算折成 openai 的档位（只调用于正预算）。
func thinkingEffortFromBudget(budget int) string {
	switch {
	case budget >= thinkingBudgetHigh:
		return "high"
	case budget >= thinkingBudgetLow:
		return "medium"
	default:
		return "low"
	}
}

// budgetTokens 返回编给按预算表达思考深度的协议（anthropic、gemini）时的预算：预算优先
// （那是这两条协议原生的表达），没有预算时按档位折。ok=false 表示这次不写这个字段：
// Enabled 为假、或既没有预算也没有认得出的档位。
func (t *ThinkingConfig) budgetTokens() (int, bool) {
	if t == nil || !t.Enabled {
		return 0, false
	}
	if t.Budget > 0 {
		return t.Budget, true
	}
	if budget := thinkingBudgetFromEffort(t.Effort); budget > 0 {
		return budget, true
	}
	return 0, false
}

// openAIEffort 返回编给两条 openai 协议时的档位：档位优先（那是这两条协议原生的表达），
// 没有档位时按预算折。空串表示这次不写这个字段：Enabled 为假时这两条协议没有「不思考」
// 的档位可写（DESIGN 5.4）。
func (t *ThinkingConfig) openAIEffort() string {
	if t == nil || !t.Enabled {
		return ""
	}
	if t.Effort != "" {
		return t.Effort
	}
	if t.Budget > 0 {
		return thinkingEffortFromBudget(t.Budget)
	}
	return ""
}

// Request 是中间表示里的请求。各协议解码后都变成它，编码上游时再展开。
// Thinking 只在客户端显式给了思考配置参数时才有值（DESIGN 5.4）。
type Request struct {
	Model             string
	System            []Block
	Messages          []Message
	Tools             []Tool
	ToolChoice        *ToolChoice
	Thinking          *ThinkingConfig
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
	// Reasoning 是上游上报的思考 token：chat 读 completion_tokens_details.reasoning_tokens，
	// responses 读 output_tokens_details.reasoning_tokens（部分中转照 chat 的写法放在
	// completion_tokens_details 里，两处都读），gemini 读 usageMetadata.thoughtsTokenCount。
	// anthropic 没有独立字段，恒为 0（DESIGN 5.5）。
	Reasoning int64
	// Total 是上游上报的总 token，0 表示上游没报。思考算不算输出的一项只能按它判：
	// 各协议做法不同，OpenAI 官方把 reasoning 当作 completion 的子集，有的中转把它报成
	// 输出之外的一项、total 里才对得上（见 store.ReasoningExtra 与 DESIGN 5.5）。
	Total int64
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
	if _, _, ok := GeminiPathAction(path); ok {
		// 三个动作都算 gemini：向量化的 :embedContent 也落在这条入站协议上（DESIGN 5.15），
		// 它不进中间表示，但报文确实走 gemini 的形状。
		return ProtocolGemini, true
	}
	return "", false
}

// UsageScanner 从同协议直通的流式字节里读出用量，不改变转发内容。
// 读出的 Usage 带 Reasoning 与 Total：入账时判定思考 token 算不算输出的一项要用它们，
// 判定本身在 store.ReasoningExtra（DESIGN 5.5）。
type UsageScanner interface {
	Feed(data []byte)
	Usage() Usage
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
