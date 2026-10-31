// Package plugin 是插件系统的静态半部：manifest、方言与模型目录的解析，声明式映射的执行，
// 以及方言注册表。
//
// 不碰数据库、不发网络、不读环境变量。依赖方向是 gateway → plugin，
// 线协议名以字符串常量出现，取值与 gateway 的 Protocol* 一致。
package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Schema 与 ABI 版本。
const (
	ManifestSchema = "lax.plugin/v1"
	DialectSchema  = "lax.dialect/v1"
	// ABIv1 是宿主支持的主版本，manifest 的 abi 必须与它精确相等。
	ABIv1 = "lax.plugin/v1"
)

// 插件类型。
const (
	KindProvider  = "provider"
	KindTransform = "transform"
	KindAuth      = "auth"
)

// 运行时与信任级别。moejs 跑在 goja 沙箱里，trusted-worker 本版未实现。
const (
	RuntimeDeclarative = "declarative"
	RuntimeMoejs       = "moejs"

	TrustDeclarative   = "declarative"
	TrustSandboxed     = "sandboxed"
	TrustTrustedWorker = "trusted-worker"
	// TrustSystem 是随二进制发布的内置插件，不通过 plugin install 安装。
	TrustSystem = "system"
)

// 方言的实现方式。
const (
	ImplHook        = "hook"
	ImplDeclarative = "declarative"
)

// 线协议名，取值与 internal/gateway 的 Protocol* 常量一致。
const (
	WireAnthropic       = "anthropic"
	WireOpenAIChat      = "openai-chat"
	WireOpenAIResponses = "openai-responses"
	WireGemini          = "gemini"
	WireEmbeddings      = "openai-embeddings"
	WireSpeech          = "openai-speech"
	WireTranscriptions  = "openai-transcriptions"
	WireSystemOne       = "systemone"
)

// 操作名，格式 <namespace>.<verb>@<major>，不含线协议名。
const (
	OpChatComplete     = "chat.complete@1"
	OpEmbeddingsCreate = "embeddings.create@1"
	OpSpeechSynthesize = "speech.synthesize@1"
	OpSpeechTranscribe = "speech.transcribe@1"
	OpSystemDecide     = "system.decide@1"
)

// 内置方言 ID。lax/ 前缀保留给随二进制发布的方言，插件不能冒用。
const (
	DialectOpenAISpeech         = "lax/openai-speech@1"
	DialectOpenAITranscriptions = "lax/openai-transcriptions@1"
	// DialectMiMoChatSpeech 是语音做成 chat completions 形状的上游方言（MiMo）。
	DialectMiMoChatSpeech = "lax/mimo-chat-speech@1"
	DialectMiMoChatASR    = "lax/mimo-chat-asr@1"

	// 宿主自己实现的线协议方言：对话、向量化、决策请求的请求与响应都由
	// internal/translate 的编解码器处理，绑定它们只是把「谁处理」写清楚，
	// 不需要插件提供映射。
	DialectHostAnthropic       = "lax/anthropic@1"
	DialectHostOpenAIChat      = "lax/openai-chat@1"
	DialectHostOpenAIResponses = "lax/openai-responses@1"
	DialectHostGemini          = "lax/gemini@1"
	DialectHostEmbeddings      = "lax/openai-embeddings@1"
	DialectHostSystemOne       = "lax/systemone@1"
)

// WireDialect 给出一条线协议的宿主方言 ID，认不出的线协议返回空串。
func WireDialect(wire string) string {
	switch wire {
	case WireAnthropic:
		return DialectHostAnthropic
	case WireOpenAIChat:
		return DialectHostOpenAIChat
	case WireOpenAIResponses:
		return DialectHostOpenAIResponses
	case WireGemini:
		return DialectHostGemini
	case WireEmbeddings:
		return DialectHostEmbeddings
	case WireSystemOne:
		return DialectHostSystemOne
	}
	return ""
}

// IsHostDialect 判断方言是不是宿主自己实现的那几条（含语音直通与内置的 Go 方言）。
// 插件方言一律返回 false。
func IsHostDialect(id string) bool {
	return strings.HasPrefix(id, "lax/")
}

// DefaultRoutePath 给出一条线协议在插件安装时的默认端点。
// 插件目录引用宿主方言时没有 dialects.json，路由只能从这张表推。
func DefaultRoutePath(wire string) string {
	switch wire {
	case WireAnthropic:
		return "/v1/messages"
	case WireOpenAIChat:
		return "/v1/chat/completions"
	case WireOpenAIResponses:
		return "/v1/responses"
	case WireGemini:
		return "/v1beta/models/{model}:generateContent"
	case WireEmbeddings:
		return "/v1/embeddings"
	case WireSpeech:
		return "/v1/audio/speech"
	case WireTranscriptions:
		return "/v1/audio/transcriptions"
	case WireSystemOne:
		return "/v1/systemone"
	}
	return ""
}

// OperationForInbound 按入站协议给出操作名，认不出时返回空串。
func OperationForInbound(wire string) string {
	switch wire {
	case WireAnthropic, WireOpenAIChat, WireOpenAIResponses, WireGemini:
		return OpChatComplete
	case WireEmbeddings:
		return OpEmbeddingsCreate
	case WireSpeech:
		return OpSpeechSynthesize
	case WireTranscriptions:
		return OpSpeechTranscribe
	case WireSystemOne:
		return OpSystemDecide
	}
	return ""
}

// KnownWire 判断线协议名是不是宿主认识的八个之一。
func KnownWire(wire string) bool {
	switch wire {
	case WireAnthropic, WireOpenAIChat, WireOpenAIResponses, WireGemini,
		WireEmbeddings, WireSpeech, WireTranscriptions, WireSystemOne:
		return true
	}
	return false
}

// KnownOperation 判断操作名是不是第一版 ABI 定义过的。
func KnownOperation(op string) bool {
	switch op {
	case OpChatComplete, OpEmbeddingsCreate, OpSpeechSynthesize, OpSpeechTranscribe, OpSystemDecide:
		return true
	}
	return false
}

// Limits 是这次调用的上限，方言不得越线。0 表示不限。
type Limits struct {
	DeadlineMS     int64
	MaxInputChars  int64
	MaxInputBytes  int64
	MaxOutputBytes int64
}

// Input 是一次语义操作的输入，由宿主从入站报文归一得到。
type Input struct {
	Operation string
	// Wire 是这次请求的出站线协议。
	Wire string
	// Dialect 是这次请求用的方言 ID，插件可以直接回显它。
	Dialect string
	// Model 是上游模型名，不是客户端写的 provider/model。
	Model string
	// Body 是宿主编好的上游请求体（对话、向量化、决策类操作）。
	// 语句类方言只做字段补丁，控件自己重写整包；语音类方言用 JSON 从零构建。
	Body []byte
	// Stream 表示客户端要流式响应。
	Stream bool
	// Messages 是规范化的对话消息（JSON 数组），供 hook 方言自己搬字段。
	Messages json.RawMessage
	// Text 是 TTS 正文。
	Text string
	// Voice 空串表示用方言默认音色。
	Voice string
	// Format 用 OpenAI 的叫法：mp3、wav、pcm。
	Format string
	// Instructions 是风格指令，上游不接受时方言可以丢掉。
	Instructions string
	// Language 为空表示自动识别。
	Language string
	// Speed 为 nil 表示客户端没给语速。
	Speed *float64
	Audio []byte
	// AudioContainer 是识别出的容器名：wav、mp3。
	AudioContainer string
	// Chars 是宿主数出的 TTS 字符数。
	Chars   int64
	Options map[string]string
	Limits  Limits
}

// Plan 是方言编出的上游请求。地址、凭据、超时、重试与记账都归宿主。
type Plan struct {
	// JSON 由语音类方言从零构建请求体。
	JSON map[string]any
	// Body 非空时直接当上游请求体（通常是给宿主编好的 Body 打了字段补丁）。
	Body []byte
	// Path 非空时覆盖厂商路由里的地址，必须是以 / 开头的相对路径。第一版不参与选地址。
	Path string
}

// StreamChunk 是宿主从上游读到的一块字节。块边界任意，方言自己处理半帧与多帧。
type StreamChunk struct {
	// Seq 从 0 开始递增。
	Seq   int
	Data  []byte
	Final bool
}

// 流事件的类型。宿主负责把它们编成入站协议的帧。
const (
	EventTextDelta      = "text.delta"
	EventReasoningDelta = "reasoning.delta"
	EventToolStart      = "tool_call.start"
	EventToolDelta      = "tool_call.delta"
	EventToolEnd        = "tool_call.end"
	EventUsage          = "usage"
	EventFinish         = "finish"
	EventError          = "error"
)

// StreamEvent 是一条语义事件。
type StreamEvent struct {
	Type string
	// Index 是工具块的序号，同一个流里的并行工具用它区分；不填时为 0。
	Index int
	// Text 用于 text.delta 与 reasoning.delta。
	Text string
	// ID 与 Name 用于 tool_call 三种事件。
	ID   string
	Name string
	// Arguments 用于 tool_call.start 与 tool_call.delta。
	Arguments string
	// Usage 仅在 EventUsage 时读。
	Usage *Usage
	// Reason 仅在 EventFinish 时读：stop、length、tool_use 或 stop_sequence。
	Reason string
	// Error 仅在 EventError 时读。
	Error string
}

// Response 是宿主读回的上游响应体，已按 MaxOutputBytes 限制。
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

// 结果种类。
const (
	ResultMedia = "media"
	ResultText  = "text"
	ResultJSON  = "json"
)

// Usage 是用量事实。方言给不了金额，计费只按宿主的价格表。
type Usage struct {
	In           int64
	Out          int64
	CacheRead    int64
	CacheWrite   int64
	Reasoning    int64
	Total        int64
	AudioSeconds int64
	TTSChars     int64
	// Source 取 upstream、estimated、partial 或 none。
	Source string
}

// Result 决定宿主写回客户端的形状：Kind 取 ResultMedia、ResultText 或 ResultJSON。
type Result struct {
	Kind string
	MIME string
	// Media 是音频或图片字节。
	Media []byte
	Text  string
	// JSON 是已按客户端入站协议包好的结构化结果。
	JSON  []byte
	Usage Usage
}

// Dialect 保证原样进、原样出：宿主把入站报文解成 Input，方言编出上游请求体，
// 宿主持有上游响应再交给方言解成 Result。
type Dialect interface {
	// ID 形如 lax/mimo-chat-speech@1。
	ID() string
	Operation() string
	// UpstreamWire 决定宿主找哪条厂商路由、用哪种凭据头与响应读法。
	UpstreamWire() string
	Prepare(in Input) (Plan, error)
	Decode(in Input, resp Response) (Result, error)
}

// ---------------------------------------------------------------- manifest

// Manifest 是 lax.plugin.json 的结构。
type Manifest struct {
	Schema      string                     `json:"schema"`
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	Kind        string                     `json:"kind"`
	Runtime     string                     `json:"runtime"`
	Entry       string                     `json:"entry"`
	ABI         string                     `json:"abi"`
	Requires    []string                   `json:"requires"`
	Permissions Permissions                `json:"permissions"`
	Providers   []ProviderDecl             `json:"providers"`
	Options     map[string]OptionDecl      `json:"options"`
	Extensions  map[string]json.RawMessage `json:"extensions"`
}

// Permissions 是 manifest 声明的权限：本版只实现网络白名单的静态校验，
// 文件、进程与环境变量一律 none。
type Permissions struct {
	Network    NetworkPerm `json:"network"`
	Secrets    SecretsPerm `json:"secrets"`
	Filesystem string      `json:"filesystem"`
	Process    string      `json:"process"`
	Env        string      `json:"env"`
	Media      MediaPerm   `json:"media"`
}

// NetworkPerm 只管控制面请求，数据面走厂商路由。
type NetworkPerm struct {
	Mode                string   `json:"mode"`
	Schemes             []string `json:"schemes"`
	Hosts               []string `json:"hosts"`
	Redirects           string   `json:"redirects"`
	MaxControlCalls     int      `json:"max_control_calls"`
	MaxControlBodyBytes int64    `json:"max_control_body_bytes"`
}

// SecretsPerm 本版只允许 host-inject：插件看不到账号 key。
type SecretsPerm struct {
	Plugin  []string `json:"plugin"`
	Account []string `json:"account"`
	Mode    string   `json:"mode"`
}

type MediaPerm struct {
	ReadBytes  int64 `json:"read_bytes"`
	WriteBytes int64 `json:"write_bytes"`
}

type ProviderDecl struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	Catalog  string `json:"catalog"`
	Dialects string `json:"dialects"`
}

type OptionDecl struct {
	Type    string          `json:"type"`
	Enum    []string        `json:"enum"`
	Default json.RawMessage `json:"default"`
}

// ---------------------------------------------------------------- dialect

type DialectFile struct {
	Schema   string        `json:"schema"`
	Dialects []DialectDecl `json:"dialects"`
}

type DialectDecl struct {
	Schema         string                     `json:"schema"`
	ID             string                     `json:"id"`
	Operation      string                     `json:"operation"`
	DisplayName    string                     `json:"display_name"`
	UpstreamWire   string                     `json:"upstream_wire"`
	Transport      TransportDecl              `json:"transport"`
	Implementation ImplementationDecl         `json:"implementation"`
	Defaults       map[string]json.RawMessage `json:"defaults"`
	Limits         LimitsDecl                 `json:"limits"`
	Request        *RequestDecl               `json:"request"`
	Response       *ResponseDecl              `json:"response"`
	Extensions     map[string]json.RawMessage `json:"extensions"`
}

type TransportDecl struct {
	Request   string `json:"request"`
	Response  string `json:"response"`
	Streaming string `json:"streaming"`
}

// ImplementationDecl 说明请求与响应各由谁处理。
type ImplementationDecl struct {
	Prepare string `json:"prepare"`
	Decode  string `json:"decode"`
}

// LimitsDecl 是方言自己声明的上限，宿主与它取交集。
type LimitsDecl struct {
	MaxInputChars    int64 `json:"max_input_chars"`
	MaxInputBytes    int64 `json:"max_input_bytes"`
	MaxResponseBytes int64 `json:"max_response_bytes"`
}

// RequestDecl 是声明式请求映射：body 每个键求值后组成 JSON 请求体。
type RequestDecl struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    map[string]Value  `json:"body"`
}

// ResponseDecl 的 Mode 只有两种：passthrough（响应体就是结果）与
// json_pointer（按 RFC 6901 取一个字段，按 Encoding 解释）。
type ResponseDecl struct {
	Mode     string     `json:"mode"`
	MIME     Value      `json:"mime"`
	Pointer  string     `json:"pointer"`
	Encoding string     `json:"encoding"`
	Usage    *UsageDecl `json:"usage"`
}

// UsageDecl 把上游 JSON 里的用量字段指到规范字段上，取不到记 0。
type UsageDecl struct {
	InputTokens       string `json:"input_tokens"`
	OutputTokens      string `json:"output_tokens"`
	CachedInputTokens string `json:"cached_input_tokens"`
	CacheWriteTokens  string `json:"cache_write_tokens"`
	ReasoningTokens   string `json:"reasoning_tokens"`
	TotalTokens       string `json:"total_tokens"`
	AudioSeconds      string `json:"audio_seconds"`
	TTSCharacters     string `json:"tts_characters"`
}

// ---------------------------------------------------------------- catalog

type CatalogFile struct {
	Models []ModelDecl `json:"models"`
}

type ModelDecl struct {
	Model      string                   `json:"model"`
	UpstreamID string                   `json:"upstream_id"`
	Operations map[string]OperationDecl `json:"operations"`
}

type OperationDecl struct {
	Dialect      string        `json:"dialect"`
	Capabilities *Capabilities `json:"capabilities"`
}

// Capabilities 的指针字段为 nil 表示这次没声明，与 false、unknown 都不是一回事。
type Capabilities struct {
	Supported *Tri     `json:"supported"`
	Streaming *Tri     `json:"streaming"`
	Tools     *Tri     `json:"tools"`
	Reasoning *Tri     `json:"reasoning"`
	JSONMode  *Tri     `json:"json_mode"`
	Vision    *Tri     `json:"vision"`
	Formats   *TriList `json:"formats"`
	Voices    *TriList `json:"voices"`
}

// Tri 是三态能力值，Value 取 true、false 或 unknown。
type Tri struct {
	Value string
}

// UnmarshalJSON 同时接受 JSON 布尔与字符串。
func (t *Tri) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case bool:
		if v {
			t.Value = "true"
		} else {
			t.Value = "false"
		}
	case string:
		switch v {
		case "true", "false", "unknown":
			t.Value = v
		default:
			return fmt.Errorf("能力值只能是 true、false 或 \"unknown\"，得到 %q", v)
		}
	default:
		return fmt.Errorf("能力值只能是 true、false 或 \"unknown\"")
	}
	return nil
}

// MarshalJSON 写回原始形状：unknown 是字符串，其余照 Value 写。
func (t Tri) MarshalJSON() ([]byte, error) {
	switch t.Value {
	case "true":
		return []byte("true"), nil
	case "false":
		return []byte("false"), nil
	}
	return json.Marshal(t.Value)
}

// Bool 把 true 与 false 翻成布尔；unknown 返回 false 与 false。
func (t Tri) Bool() (value bool, known bool) {
	switch t.Value {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// TriList 是数组或 unknown。空数组表示明确一个都没有，不是未知。
type TriList struct {
	Unknown bool
	Values  []string
}

func (l *TriList) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		if v != "unknown" {
			return fmt.Errorf("列表型能力只能是数组或 \"unknown\"，得到 %q", v)
		}
		l.Unknown = true
		l.Values = nil
	case []any:
		values := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("列表型能力的每一项必须是字符串")
			}
			values = append(values, s)
		}
		l.Values = values
	case nil:
		l.Values = []string{}
	default:
		return fmt.Errorf("列表型能力只能是数组或 \"unknown\"")
	}
	return nil
}

// ---------------------------------------------------------------- value

// Value 是声明式映射里的一个值：普通 JSON，或一个节点。节点的来源键（const、input、
// model、option）最多一个，修饰键是 map、default 与 omit_if_empty：
//
//	{"input": "text"}
//	{"input": "voice", "default": "alloy"}
//	{"input": "format", "map": {"pcm": "pcm16"}, "omit_if_empty": true}
//
// 没有表达式、正则、循环与条件分支：超出这个边界要写 hook 方言。
type Value struct {
	Scalar   any
	Object   map[string]Value
	Array    []Value
	Const    any
	HasConst bool
	Input    string
	Model    string
	Option   string
	// Map 的键是来源取值的字符串形式。
	Map         map[string]any
	Default     any
	HasDefault  bool
	OmitIfEmpty bool
}

// UnmarshalJSON 把 JSON 解成普通值或节点：对象里出现节点键就按节点解释，其余递归。
func (v *Value) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	return v.fromAny(raw)
}

func (v *Value) fromAny(raw any) error {
	*v = Value{}
	switch value := raw.(type) {
	case map[string]any:
		if isNodeShaped(value) {
			return v.fromNode(value)
		}
		object := make(map[string]Value, len(value))
		for key, item := range value {
			var child Value
			if err := child.fromAny(item); err != nil {
				return err
			}
			object[key] = child
		}
		v.Object = object
	case []any:
		array := make([]Value, 0, len(value))
		for _, item := range value {
			var child Value
			if err := child.fromAny(item); err != nil {
				return err
			}
			array = append(array, child)
		}
		v.Array = array
	default:
		v.Scalar = value
	}
	return nil
}

var nodeKeys = []string{"const", "input", "model", "option", "map", "default", "omit_if_empty"}

// sourceKeys 是来源键：map 是修饰键，靠来源取到的值查表，因此不在这里。
var sourceKeys = []string{"const", "input", "model", "option"}

// isNodeShaped 只看键名。普通对象正好用这些键名时会被误判成节点，
// 代价是那种对象要套一层 const 或 map 才能表达。
func isNodeShaped(object map[string]any) bool {
	for _, key := range nodeKeys {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func (v *Value) fromNode(object map[string]any) error {
	for key := range object {
		known := false
		for _, candidate := range nodeKeys {
			if key == candidate {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("声明式映射的节点里没有 %q 这个键，可用的是 const、input、model、option、map、default、omit_if_empty", key)
		}
	}

	sources := 0
	for _, key := range sourceKeys {
		if _, ok := object[key]; ok {
			sources++
		}
	}
	if sources > 1 {
		return fmt.Errorf("一个节点最多有一个来源键（const、input、model、option），这里给了多个")
	}
	if sources == 0 {
		return fmt.Errorf("节点至少要有来源键之一（const、input、model、option），或写成普通 JSON 值")
	}

	if raw, ok := object["const"]; ok {
		var child Value
		if err := child.fromAny(raw); err != nil {
			return err
		}
		if child.IsNode() {
			return fmt.Errorf("const 里不能再放节点：常量只能是普通 JSON")
		}
		v.Const = child.plain()
		v.HasConst = true
	}
	if raw, ok := object["input"]; ok {
		name, ok := raw.(string)
		if !ok || name == "" {
			return fmt.Errorf("input 必须是字段名")
		}
		v.Input = name
	}
	if raw, ok := object["model"]; ok {
		name, ok := raw.(string)
		if !ok || name == "" {
			return fmt.Errorf("model 必须是字段名")
		}
		v.Model = name
	}
	if raw, ok := object["option"]; ok {
		name, ok := raw.(string)
		if !ok || name == "" {
			return fmt.Errorf("option 必须是配置项名")
		}
		v.Option = name
	}
	if raw, ok := object["map"]; ok {
		table, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("map 必须是对象：键是输入取值，值是常量")
		}
		v.Map = table
	}
	if raw, ok := object["default"]; ok {
		v.Default = raw
		v.HasDefault = true
	}
	if raw, ok := object["omit_if_empty"]; ok {
		flag, ok := raw.(bool)
		if !ok {
			return fmt.Errorf("omit_if_empty 必须是布尔值")
		}
		v.OmitIfEmpty = flag
	}

	if v.Map != nil && v.Input == "" && v.Option == "" && v.Model == "" {
		return fmt.Errorf("map 要配合 input、option 或 model 使用：它按来源的取值查表")
	}
	return nil
}

// plain 把普通 Value 还原成任意 JSON 结构。
func (v Value) plain() any {
	switch {
	case v.Object != nil:
		object := make(map[string]any, len(v.Object))
		for key, child := range v.Object {
			object[key] = child.plain()
		}
		return object
	case v.Array != nil:
		array := make([]any, 0, len(v.Array))
		for _, child := range v.Array {
			array = append(array, child.plain())
		}
		return array
	}
	return v.Scalar
}

// IsNode 判断这个 Value 是不是节点。
func (v Value) IsNode() bool {
	return v.HasConst || v.Input != "" || v.Model != "" || v.Option != "" || v.Map != nil ||
		v.HasDefault || v.OmitIfEmpty
}
