package plugin

import (
	"fmt"
	"time"
)

// 沙箱契约。插件代码只在 Sandbox 里执行，宿主只经这几个方法进出。
//
// 一个 Sandbox 属于一个插件版本，可以被多次调用；实现内部自己串行化（同一次插件版本
// 不并发跑两个 hook），因为插件模块级变量在同一个 Runtime 里是共享的。

// Sandbox 是执行插件 hook 的运行时。
type Sandbox interface {
	// PrepareRequest 调插件的 prepare_request，返回上游请求。
	PrepareRequest(in Input) (Plan, error)
	// DecodeResponse 调插件的 decode_response，返回语义结果。
	DecodeResponse(in Input, resp Response) (Result, error)
	// DecodeStream 调插件的 decode_stream 解一块上游字节，返回这次产生的事件。
	DecodeStream(in Input, chunk StreamChunk) ([]StreamEvent, error)
	// Close 释放运行时与该插件的模块状态。
	Close() error
}

// HookTimeouts 是每次 hook 调用的上限。零值表示用默认值。
type HookTimeouts struct {
	Prepare time.Duration
	Decode  time.Duration
	Stream  time.Duration
}

// 默认与上限。超时用中断实现：超时的调用被丢弃，运行时不再复用。
const (
	DefaultPrepareTimeout = 100 * time.Millisecond
	MaxPrepareTimeout     = 2 * time.Second
	DefaultDecodeTimeout  = 2 * time.Second
	MaxDecodeTimeout      = 30 * time.Second
	DefaultStreamTimeout  = 100 * time.Millisecond
	MaxStreamTimeout      = 2 * time.Second
	// MaxHookOutputBytes 是一次 hook 返回值的上限。
	MaxHookOutputBytes = 8 << 20
)

// SandboxOptions 打开一个插件沙箱需要的输入。
type SandboxOptions struct {
	Manifest Manifest
	// Root 是插件根目录。静态 import 只允许落在它里面的相对文件。
	Root  string
	Entry string
	// Trust 是安装时确认的信任级别。只有 sandboxed 与 trusted-worker 会执行代码。
	Trust    string
	Timeouts HookTimeouts
	// Options 是 manifest 里声明的配置项取值。
	Options map[string]string
	Logf    func(format string, args ...any)
	// Now 供 lax.now() 用，nil 时取 time.Now。
	Now func() time.Time
}

// HookError 是插件抛出的语义错误，宿主按 Code 映射成客户端错误形状。
type HookError struct {
	Code    string
	Message string
	// Retryable 只是提示，最终是否重试由宿主决定。
	Retryable bool
	// Details 只进受控日志，不返回客户端。
	Details string
}

func (e *HookError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("插件报错 %s", e.Code)
	}
	return e.Message
}

// 插件错误码。未知或缺失的码归一成 ErrorPluginError。
const (
	ErrorInvalidInput      = "invalid_input"
	ErrorUnsupportedOp     = "unsupported_operation"
	ErrorUnsupportedDialct = "unsupported_dialect"
	ErrorModelNotFound     = "model_not_found"
	ErrorModelForbidden    = "model_forbidden"
	ErrorAuthInvalid       = "auth_invalid"
	ErrorAuthExpired       = "auth_expired"
	ErrorRateLimited       = "rate_limited"
	ErrorUpstreamUnavail   = "upstream_unavailable"
	ErrorUpstreamTimeout   = "upstream_timeout"
	ErrorUpstreamProtocol  = "upstream_protocol_error"
	ErrorPluginError       = "plugin_error"
	ErrorPluginTimeout     = "plugin_timeout"
	ErrorPlanInvalid       = "plugin_plan_invalid"
	ErrorCancelled         = "cancelled"
	ErrorInternal          = "internal"
)

// KnownErrorCode 判断插件给的错误码是不是第一版定义过的。
func KnownErrorCode(code string) bool {
	switch code {
	case ErrorInvalidInput, ErrorUnsupportedOp, ErrorUnsupportedDialct, ErrorModelNotFound,
		ErrorModelForbidden, ErrorAuthInvalid, ErrorAuthExpired, ErrorRateLimited,
		ErrorUpstreamUnavail, ErrorUpstreamTimeout, ErrorUpstreamProtocol,
		ErrorPluginError, ErrorPluginTimeout, ErrorPlanInvalid, ErrorCancelled, ErrorInternal:
		return true
	}
	return false
}

// HookDialect 把一个插件沙箱包成 Dialect：语音等整包操作走 Prepare/Decode，
// 对话类操作走 Patch 与流式解码。
type HookDialect struct {
	IDValue      string
	OperationVal string
	WireVal      string
	Sandbox      Sandbox
}

func (h HookDialect) ID() string           { return h.IDValue }
func (h HookDialect) Operation() string    { return h.OperationVal }
func (h HookDialect) UpstreamWire() string { return h.WireVal }

func (h HookDialect) Prepare(in Input) (Plan, error) {
	// 方言 ID 由包装层填：插件不自报自己在跑哪条方言，回显错只会让日志误人。
	in.Dialect = h.IDValue
	return h.Sandbox.PrepareRequest(in)
}

func (h HookDialect) Decode(in Input, resp Response) (Result, error) {
	in.Dialect = h.IDValue
	return h.Sandbox.DecodeResponse(in, resp)
}

// StreamDialect 是能逐块解码上游响应的方言。宿主读上游流时按块调用。
type StreamDialect interface {
	DecodeStream(in Input, chunk StreamChunk) ([]StreamEvent, error)
}

// DecodeStream 让 HookDialect 满足 StreamDialect。
func (h HookDialect) DecodeStream(in Input, chunk StreamChunk) ([]StreamEvent, error) {
	in.Dialect = h.IDValue
	return h.Sandbox.DecodeStream(in, chunk)
}
