package plugin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// JS 沙箱的生命周期。一次 hook 调用用一个新的 goja Runtime，模块每次重新执行，
// 超时或抛错之后不会有残留状态；唯一例外是同一段流的 decode_stream：它在同一个
// Runtime 里连续调用，模块级变量只在一条流内延续，Final 之后丢弃。
//
// 中断用 Runtime.Interrupt 实现：它只在 JS 代码里生效，卡在原生 Go 调用（例如
// lax.http.request 等上游响应）时不会立刻返回，要等原生调用自己结束。

const (
	// sandboxInitTimeout 是加载入口模块与顶层代码的上限。
	sandboxInitTimeout = 2 * time.Second
	// sandboxInlineLimit 是注入 ctx 的 base64 字段上限：超过只给空串与 truncated。
	sandboxInlineLimit = 8 << 20
	// sandboxMaxDepth 是返回值 JSON 化的嵌套上限。
	sandboxMaxDepth = 32
)

// sandboxInterrupt 是中断载荷：Interrupt 的值原样交回，用来区分超时与普通异常。
type sandboxInterrupt string

const interruptHookTimeout sandboxInterrupt = "plugin_timeout"

// laxErrorMarker 标记 lax.error() 造出来的错误对象，!! 是唯一判据。
const laxErrorMarker = "__laxError"

// hook 名，与 manifest 里解的入口模块导出名一致。
const (
	hookPrepare = "prepare_request"
	hookDecode  = "decode_response"
	hookStream  = "decode_stream"
)

// jsSandbox 是一个插件的沙箱实例。调用串行化：同一次插件版本不并发跑两个 hook。
type jsSandbox struct {
	o     SandboxOptions
	root  string
	entry string
	hooks []string

	mu          sync.Mutex
	closed      bool
	programs    map[string]*goja.Program
	sourceBytes int
	// vm 只给同一段流的 decode_stream 复用。
	vm    *jsVM
	state any
	net   netBudget
}

// OpenSandbox 打开一个插件的 JS 沙箱：编译并执行一次入口模块，确认导出了哪些 hook。
func OpenSandbox(o SandboxOptions) (sandbox Sandbox, err error) {
	// 插件代码由 goja 解释，但注入的 Go 侧 API 仍是宿主代码：一次未预期的 panic
	// 不该带崩整个进程，收成 internal 让调用方按普通错误处理。
	defer func() {
		if r := recover(); r != nil {
			sandbox, err = nil, &HookError{Code: ErrorInternal, Message: fmt.Sprintf("打开插件沙箱内部错误：%v", r)}
		}
	}()
	switch o.Trust {
	case TrustSandboxed, TrustTrustedWorker:
	default:
		return nil, fmt.Errorf("插件 %s 的信任级别是 %q：只有 %s 与 %s 会执行插件代码", o.Manifest.ID, o.Trust, TrustSandboxed, TrustTrustedWorker)
	}
	if o.Entry == "" {
		return nil, fmt.Errorf("插件 %s 的 manifest 没有 entry：JS 插件要声明入口脚本", o.Manifest.ID)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return nil, fmt.Errorf("解析插件目录 %s 失败：%v", o.Root, err)
	}
	entry, err := pluginFile(root, o.Entry)
	if err != nil {
		return nil, err
	}
	s := &jsSandbox{o: o, root: root, entry: entry, programs: map[string]*goja.Program{}}
	vm, err := s.newVM(sandboxInitTimeout)
	if err != nil {
		return nil, fmt.Errorf("插件 %s 的入口 %s 加载失败：%w", o.Manifest.ID, o.Entry, err)
	}
	hooks, err := s.checkExports(vm)
	if err != nil {
		return nil, fmt.Errorf("插件 %s 的入口 %s：%w", o.Manifest.ID, o.Entry, err)
	}
	s.hooks = hooks
	return s, nil
}

// checkExports 记下导出了哪几个 hook。缺 hook 不算加载失败：插件可以只实现解码侧。
func (s *jsSandbox) checkExports(vm *jsVM) ([]string, error) {
	found := make([]string, 0, 3)
	for _, name := range []string{hookPrepare, hookDecode, hookStream} {
		value := vm.exports.Get(name)
		if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
			continue
		}
		if _, ok := goja.AssertFunction(value); !ok {
			return nil, fmt.Errorf("%s 不是函数（是%s）：hook 要写成 export function %s(ctx)", name, gojaShape(value), name)
		}
		found = append(found, name)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("没有导出 %s、%s 或 %s：入口模块要 export 至少一个 hook", hookPrepare, hookDecode, hookStream)
	}
	return found, nil
}

func (s *jsSandbox) logf(format string, args ...any) {
	if s.o.Logf != nil {
		s.o.Logf(format, args...)
	}
}

func (s *jsSandbox) warnf(format string, args ...any) {
	s.logf("插件 %s："+format, append([]any{s.o.Manifest.ID}, args...)...)
}

// usedTimeout 把配置给的上限收进 [默认值, 上限]。
func (s *jsSandbox) usedTimeout(got, def, max time.Duration) time.Duration {
	if got <= 0 {
		got = def
	}
	if got > max {
		got = max
	}
	return got
}

// ------------------------------------------------------------------ hook 调用

// invoke 在一个 Runtime 上跑一个 hook，把返回值变成经 JSON 往返的 Go 值。
// 超时用 AfterFunc 中断：中断只对 JS 代码生效，计时器已经触发时这次 Runtime 作废。
func (s *jsSandbox) invoke(vm *jsVM, name string, timeout time.Duration, ctx map[string]any) (any, error) {
	fnValue := vm.exports.Get(name)
	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		if !slices.Contains(s.hooks, name) {
			return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("插件没有导出 %s：入口要 export function %s(ctx)，当前导出的是 %s", name, name, strings.Join(s.hooks, "、"))}
		}
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("插件导出的 %s 不是函数", name)}
	}
	s.net = netBudget{}
	timer := time.AfterFunc(timeout, func() { vm.rt.Interrupt(interruptHookTimeout) })
	result, callErr := fn(goja.Undefined(), vm.rt.ToValue(ctx))
	if !timer.Stop() {
		vm.dead = true
		vm.rt.ClearInterrupt()
	}
	if callErr != nil {
		return nil, s.hookError(name, callErr, timeout)
	}
	if vm.dead {
		return nil, s.timeoutError(name, timeout)
	}
	result, err := s.awaitPromise(name, result)
	if err != nil {
		return nil, err
	}
	plain, err := s.toPlain(result, 0, map[*goja.Object]bool{})
	if err != nil {
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的返回值不能转成 JSON：%v", name, err)}
	}
	data, err := json.Marshal(plain)
	if err != nil {
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的返回值序列化失败：%v", name, err)}
	}
	if len(data) > MaxHookOutputBytes {
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的返回值有 %d 字节，超过 %d 字节上限", name, len(data), MaxHookOutputBytes)}
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的返回值不是合法 JSON：%v", name, err)}
	}
	return out, nil
}

// promiseType 用来认 async hook 的返回值。goja 的 Promise 对象 ClassName 还是
// Object，只能看 ExportType。
var promiseType = reflect.TypeOf((*goja.Promise)(nil))

// awaitPromise 等 async hook 的返回值。沙箱里没有事件循环，microtask 在调用返回前
// 已经跑完，所以 Promise 只可能是已完成；还挂着的就是永远等不到结果。
func (s *jsSandbox) awaitPromise(name string, result goja.Value) (goja.Value, error) {
	object, ok := result.(*goja.Object)
	if !ok || object.ExportType() != promiseType {
		return result, nil
	}
	promise, ok := object.Export().(*goja.Promise)
	if !ok {
		return result, nil
	}
	switch promise.State() {
	case goja.PromiseStateFulfilled:
		return promise.Result(), nil
	case goja.PromiseStateRejected:
		return nil, s.thrown(promise.Result())
	}
	return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 返回的 Promise 一直没有完成：沙箱里没有事件循环，await 只能等已经 resolve 的值", name)}
}

func (s *jsSandbox) timeoutError(name string, timeout time.Duration) error {
	return &HookError{Code: ErrorPluginTimeout, Message: fmt.Sprintf("%s 超过 %s：脚本里有死循环，或在等不会发生的事", name, timeout)}
}

// hookError 把 goja 的错误翻成宿主认识的 HookError。
func (s *jsSandbox) hookError(name string, err error, timeout time.Duration) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		if value, ok := interrupted.Value().(sandboxInterrupt); ok && value == interruptHookTimeout {
			return s.timeoutError(name, timeout)
		}
		return &HookError{Code: ErrorPluginTimeout, Message: fmt.Sprintf("%s 被中断", name)}
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return s.thrown(ex.Value())
	}
	return &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 执行失败：%v", name, err)}
}

// thrown 把 JS 里 throw 出来的值翻成 HookError。lax.error() 造的带 code，
// 其它值只能当 plugin_error。
func (s *jsSandbox) thrown(v goja.Value) error {
	if code, message, retryable, details, ok := s.readLaxError(v); ok {
		return &HookError{Code: code, Message: message, Retryable: retryable, Details: details}
	}
	message := ""
	if object, ok := v.(*goja.Object); ok {
		if raw := object.Get("message"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
			message = raw.String()
		}
	}
	if message == "" && v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		message = v.String()
	}
	if message == "" {
		message = "插件抛了一个没有内容的值"
	}
	return &HookError{Code: ErrorPluginError, Message: message}
}

func (s *jsSandbox) readLaxError(v goja.Value) (code, message string, retryable bool, details string, ok bool) {
	object, isObject := v.(*goja.Object)
	if !isObject {
		return "", "", false, "", false
	}
	marker := object.Get(laxErrorMarker)
	if marker == nil || !marker.ToBoolean() {
		return "", "", false, "", false
	}
	code = "plugin_error"
	if raw := object.Get("code"); raw != nil && !goja.IsUndefined(raw) {
		code = raw.String()
	}
	if !KnownErrorCode(code) {
		code = ErrorPluginError
	}
	if raw := object.Get("message"); raw != nil && !goja.IsUndefined(raw) {
		message = raw.String()
	}
	if raw := object.Get("retryable"); raw != nil && !goja.IsUndefined(raw) {
		retryable = raw.ToBoolean()
	}
	if raw := object.Get("details"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
		details = raw.String()
	}
	return code, message, retryable, details, true
}

// moduleError 把加载模块时的 goja 错误翻成 HookError。
func (s *jsSandbox) moduleError(path string, err error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return &HookError{Code: ErrorPluginTimeout, Message: fmt.Sprintf("%s 加载超时：模块顶层代码里有死循环或等不到结果的 await", s.relName(path))}
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return s.thrown(ex.Value())
	}
	return &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 加载失败：%v", s.relName(path), err)}
}

func errSandboxClosed() error {
	return &HookError{Code: ErrorInternal, Message: "插件沙箱已关闭"}
}

// recoverSandbox 把一次 hook 里未预期的 panic 收成 internal。
func recoverSandbox(where string, err *error) {
	if r := recover(); r != nil {
		*err = &HookError{Code: ErrorInternal, Message: fmt.Sprintf("%s 内部错误：%v", where, r)}
	}
}

// ------------------------------------------------------------------ 返回值

// toPlain 把 JS 值转成能交给 encoding/json 的 Go 值。函数、Symbol 与带
// __proto__/constructor/prototype 键的对象一律拒绝：后者会把键名注入原型。
func (s *jsSandbox) toPlain(v goja.Value, depth int, seen map[*goja.Object]bool) (any, error) {
	if depth > sandboxMaxDepth {
		return nil, fmt.Errorf("嵌套超过 %d 层", sandboxMaxDepth)
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	object, isObject := v.(*goja.Object)
	if !isObject {
		if _, ok := goja.AssertFunction(v); ok {
			return nil, errors.New("返回值里不能有函数")
		}
		exported := v.Export()
		switch exported.(type) {
		case bool, int64, float64, string:
			return exported, nil
		}
		return nil, fmt.Errorf("返回值里有不能转成 JSON 的值（%s）", v.String())
	}
	if _, ok := goja.AssertFunction(object); ok {
		return nil, errors.New("返回值里不能有函数")
	}
	if seen[object] {
		return nil, errors.New("返回值里有循环引用")
	}
	seen[object] = true
	defer delete(seen, object)

	if object.ClassName() == "Array" {
		length := object.Get("length").ToInteger()
		items := make([]any, 0, length)
		for i := int64(0); i < length; i++ {
			item, err := s.toPlain(object.GetIdx(i), depth+1, seen)
			if err != nil {
				return nil, fmt.Errorf("第 %d 项：%w", i, err)
			}
			items = append(items, item)
		}
		return items, nil
	}

	out := make(map[string]any, len(object.Keys()))
	for _, key := range object.Keys() {
		switch key {
		case "__proto__", "constructor", "prototype":
			return nil, fmt.Errorf("返回值里有 %q 键：这个键名会被当成原型注入，插件返回值不能带", key)
		}
		item, err := s.toPlain(object.Get(key), depth+1, seen)
		if err != nil {
			return nil, fmt.Errorf("字段 %q：%w", key, err)
		}
		out[key] = item
	}
	return out, nil
}

// describe 给错误文案描述一个 JSON 值的形状。
func describe(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return fmt.Sprintf("对象（键：%s）", strings.Join(slices.Sorted(maps.Keys(value)), "、"))
	case []any:
		return fmt.Sprintf("数组（%d 项）", len(value))
	case string:
		if len(value) > 60 {
			return fmt.Sprintf("字符串 %q…", value[:60])
		}
		return fmt.Sprintf("字符串 %q", value)
	case float64:
		return fmt.Sprintf("数字 %v", value)
	case bool:
		return fmt.Sprintf("布尔 %v", value)
	}
	return fmt.Sprintf("%T", v)
}

func checkKeys(where string, obj map[string]any, allowed ...string) error {
	for _, key := range slices.Sorted(maps.Keys(obj)) {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("%s 里有不认识的字段 %q，可用的是：%s", where, key, strings.Join(allowed, "、"))
		}
	}
	return nil
}

func fieldString(where string, obj map[string]any, key string) (string, error) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s 的 %s 必须是字符串，得到%s", where, key, describe(raw))
	}
	return value, nil
}

func fieldBool(where string, obj map[string]any, key string) (bool, error) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s 的 %s 必须是布尔值，得到%s", where, key, describe(raw))
	}
	return value, nil
}

func fieldInt(where string, obj map[string]any, key string) (int64, error) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return 0, nil
	}
	switch value := raw.(type) {
	case float64:
		if value != float64(int64(value)) {
			return 0, fmt.Errorf("%s 的 %s 必须是整数，得到%s", where, key, describe(raw))
		}
		return int64(value), nil
	}
	return 0, fmt.Errorf("%s 的 %s 必须是数字，得到%s", where, key, describe(raw))
}

// ------------------------------------------------------------------ ctx

func (s *jsSandbox) baseContext(in Input) map[string]any {
	options := make(map[string]any, len(s.o.Options))
	for _, key := range slices.Sorted(maps.Keys(s.o.Options)) {
		options[key] = s.o.Options[key]
	}
	return map[string]any{
		"abi":       ABIv1,
		"operation": in.Operation,
		"model":     in.Model,
		"wire":      in.Wire,
		"stream":    in.Stream,
		"dialect":   in.Dialect,
		"input":     inputContext(in),
		"options":   options,
		"limits": map[string]any{
			"deadline_ms":      in.Limits.DeadlineMS,
			"max_input_chars":  in.Limits.MaxInputChars,
			"max_input_bytes":  in.Limits.MaxInputBytes,
			"max_output_bytes": in.Limits.MaxOutputBytes,
		},
	}
}

func inputContext(in Input) map[string]any {
	out := map[string]any{
		"text":            in.Text,
		"voice":           in.Voice,
		"format":          in.Format,
		"instructions":    in.Instructions,
		"language":        in.Language,
		"model":           in.Model,
		"audio_container": in.AudioContainer,
		"chars":           in.Chars,
		"speed":           nil,
		"audio_base64":    "",
		"audio_truncated": false,
	}
	if in.Speed != nil {
		out["speed"] = *in.Speed
	}
	if len(in.Audio) > 0 {
		if len(in.Audio) > sandboxInlineLimit {
			out["audio_truncated"] = true
		} else {
			out["audio_base64"] = base64.StdEncoding.EncodeToString(in.Audio)
		}
	}
	return out
}

func (s *jsSandbox) prepareContext(in Input) map[string]any {
	ctx := s.baseContext(in)
	ctx["body"] = decodeJSONValue(in.Body)
	ctx["messages"] = decodeJSONValue(in.Messages)
	return ctx
}

func (s *jsSandbox) decodeContext(in Input, resp Response) map[string]any {
	ctx := s.prepareContext(in)
	ctx["response"] = responseContext(resp)
	return ctx
}

func (s *jsSandbox) streamContext(in Input, chunk StreamChunk) map[string]any {
	ctx := s.prepareContext(in)
	ctx["chunk"] = chunkContext(chunk)
	ctx["state"] = s.state
	return ctx
}

// decodeJSONValue 把宿主手里的字节注入 ctx：能解成 JSON 就给结构化值，
// 解不开就给原始字符串，两种都比丢掉更有用。
func decodeJSONValue(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}
	return out
}

// isTextualContent 判断响应体是文本还是二进制：决定注入 text 还是 media_base64。
func isTextualContent(contentType string) bool {
	lower := strings.ToLower(contentType)
	if lower == "" {
		return true
	}
	for _, marker := range []string{"json", "text/", "xml", "javascript", "x-www-form-urlencoded"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func responseContext(resp Response) map[string]any {
	out := map[string]any{
		"status":          int64(resp.Status),
		"content_type":    resp.ContentType,
		"text":            "",
		"body":            nil,
		"media_base64":    "",
		"media_truncated": false,
	}
	if isTextualContent(resp.ContentType) {
		out["text"] = string(resp.Body)
		out["body"] = decodeJSONValue(resp.Body)
		return out
	}
	if len(resp.Body) > sandboxInlineLimit {
		out["media_truncated"] = true
		return out
	}
	out["media_base64"] = base64.StdEncoding.EncodeToString(resp.Body)
	return out
}

func chunkContext(chunk StreamChunk) map[string]any {
	out := map[string]any{
		"seq":       int64(chunk.Seq),
		"data":      "",
		"final":     chunk.Final,
		"encoding":  "utf8",
		"truncated": false,
	}
	if len(chunk.Data) > sandboxInlineLimit {
		out["truncated"] = true
		return out
	}
	if utf8.Valid(chunk.Data) {
		out["data"] = string(chunk.Data)
		return out
	}
	out["encoding"] = "base64"
	out["data"] = base64.StdEncoding.EncodeToString(chunk.Data)
	return out
}

// ------------------------------------------------------------------ 三个 hook

func (s *jsSandbox) PrepareRequest(in Input) (plan Plan, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer recoverSandbox(hookPrepare, &err)
	if s.closed {
		return Plan{}, errSandboxClosed()
	}
	timeout := s.usedTimeout(s.o.Timeouts.Prepare, DefaultPrepareTimeout, MaxPrepareTimeout)
	vm, err := s.newVM(timeout)
	if err != nil {
		return Plan{}, err
	}
	value, err := s.invoke(vm, hookPrepare, timeout, s.prepareContext(in))
	if err != nil {
		return Plan{}, err
	}
	return s.plan(value)
}

func (s *jsSandbox) DecodeResponse(in Input, resp Response) (result Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer recoverSandbox(hookDecode, &err)
	if s.closed {
		return Result{}, errSandboxClosed()
	}
	timeout := s.usedTimeout(s.o.Timeouts.Decode, DefaultDecodeTimeout, MaxDecodeTimeout)
	vm, err := s.newVM(timeout)
	if err != nil {
		return Result{}, err
	}
	value, err := s.invoke(vm, hookDecode, timeout, s.decodeContext(in, resp))
	if err != nil {
		return Result{}, err
	}
	return s.result(value)
}

func (s *jsSandbox) DecodeStream(in Input, chunk StreamChunk) (events []StreamEvent, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer recoverSandbox(hookStream, &err)
	if s.closed {
		return nil, errSandboxClosed()
	}
	timeout := s.usedTimeout(s.o.Timeouts.Stream, DefaultStreamTimeout, MaxStreamTimeout)
	if s.vm == nil || s.vm.dead {
		vm, err := s.newVM(timeout)
		if err != nil {
			s.state = nil
			return nil, err
		}
		s.vm = vm
	}
	value, err := s.invoke(s.vm, hookStream, timeout, s.streamContext(in, chunk))
	if err != nil {
		// 出错的这一块不再往下游写，state 也跟着丢：半条流的状态留着只会错得更晚。
		s.state = nil
		if s.vm.dead {
			s.vm = nil
		}
		return nil, err
	}
	events, state, done, err := s.events(value)
	if err != nil {
		s.state = nil
		return nil, err
	}
	if chunk.Final || done {
		s.state = nil
		if chunk.Final {
			// Final 之后这条流结束了，下一个流从干净的 Runtime 开始。
			s.vm = nil
		}
	} else {
		s.state = state
	}
	return events, nil
}

func (s *jsSandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.vm = nil
	s.state = nil
	s.programs = nil
	s.sourceBytes = 0
	pluginCache.drop(s.o.Manifest.ID)
	return nil
}

// ------------------------------------------------------------------ 返回值映射

// plan 归一 prepare_request 的两种返回形状：
// {method?, path?, headers?, body?} 与 {body:{kind:"json", value:…}}。
func (s *jsSandbox) plan(value any) (Plan, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return Plan{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 要返回对象 {method?, path?, headers?, body?}，得到%s", hookPrepare, describe(value))}
	}
	if err := checkKeys(hookPrepare+" 的返回值", obj, "method", "path", "headers", "body"); err != nil {
		return Plan{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	path, err := fieldString(hookPrepare, obj, "path")
	if err != nil {
		return Plan{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		return Plan{}, &HookError{Code: ErrorPlanInvalid, Message: fmt.Sprintf("%s 的 path 是 %q：必须以 / 开头（Plan 只描述路径，不选地址）", hookPrepare, path)}
	}
	if method, err := fieldString(hookPrepare, obj, "method"); err != nil {
		return Plan{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	} else if method != "" {
		s.warnf("%s 返回了 method=%q，Plan 没有对应字段，已忽略", hookPrepare, method)
	}
	plan := Plan{Path: path}
	if raw, ok := obj["headers"]; ok && raw != nil {
		headers, isObject := raw.(map[string]any)
		if !isObject {
			return Plan{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 headers 必须是对象，得到%s", hookPrepare, describe(raw))}
		}
		plan.Headers = make(map[string]string, len(headers))
		total := 0
		for _, key := range slices.Sorted(maps.Keys(headers)) {
			value, isString := headers[key].(string)
			if !isString {
				return Plan{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 headers.%s 必须是字符串，得到%s", hookPrepare, key, describe(headers[key]))}
			}
			name, err := NormalizeHeaderName(hookPrepare+" 的 headers", key)
			if err != nil {
				return Plan{}, &HookError{Code: ErrorPlanInvalid, Message: err.Error()}
			}
			if IsReservedPlanHeader(name) {
				return Plan{}, &HookError{Code: ErrorPlanInvalid, Message: fmt.Sprintf("%s 的 headers.%s 是保留头：%s 由宿主自己写（凭据只能由厂商的 auth 声明注入），插件不能设", hookPrepare, key, name)}
			}
			if err := CheckPlanHeaderValue(name, value); err != nil {
				return Plan{}, &HookError{Code: ErrorPlanInvalid, Message: fmt.Sprintf("%s：%v", hookPrepare, err)}
			}
			plan.Headers[name] = value
			total += len(name) + len(value)
		}
		if len(plan.Headers) > PlanHeaderMaxCount {
			return Plan{}, &HookError{Code: ErrorPlanInvalid, Message: fmt.Sprintf("%s 返回了 %d 个 headers，上限 %d 条", hookPrepare, len(plan.Headers), PlanHeaderMaxCount)}
		}
		if total > planHeaderMaxTotal {
			return Plan{}, &HookError{Code: ErrorPlanInvalid, Message: fmt.Sprintf("%s 返回的 headers 共 %d 字节，上限 %d 字节", hookPrepare, total, planHeaderMaxTotal)}
		}
	}

	raw, ok := obj["body"]
	if !ok || raw == nil {
		return plan, nil
	}
	switch body := raw.(type) {
	case string:
		plan.Body = []byte(body)
	case map[string]any:
		kind, hasKind := body["kind"]
		if !hasKind {
			plan.JSON = body
			return plan, nil
		}
		kindName, isString := kind.(string)
		if !isString || kindName != "json" {
			return Plan{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 body.kind 只能是 \"json\"，得到%s", hookPrepare, describe(kind))}
		}
		if err := checkKeys(hookPrepare+" 的 body", body, "kind", "value"); err != nil {
			return Plan{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
		}
		inner, isObject := body["value"].(map[string]any)
		if !isObject {
			return Plan{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 body.value 必须是对象，得到%s", hookPrepare, describe(body["value"]))}
		}
		plan.JSON = inner
	default:
		return Plan{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 body 只能是字符串或 {kind:\"json\", value:{…}}，得到%s", hookPrepare, describe(raw))}
	}
	return plan, nil
}

// result 把 decode_response 的返回值映射成 Result。
func (s *jsSandbox) result(value any) (Result, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 要返回 {result:{…}, usage?, finish_reason?}，得到%s", hookDecode, describe(value))}
	}
	if err := checkKeys(hookDecode+" 的返回值", obj, "result", "usage", "finish_reason"); err != nil {
		return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	rawResult, ok := obj["result"].(map[string]any)
	if !ok {
		return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 result 必须是对象 {kind, mime?, text?, body_base64?, json?}，得到%s", hookDecode, describe(obj["result"]))}
	}
	if err := checkKeys(hookDecode+" 的 result", rawResult, "kind", "mime", "text", "body_base64", "json"); err != nil {
		return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	kind, err := fieldString(hookDecode+" 的 result", rawResult, "kind")
	if err != nil {
		return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	mime, err := fieldString(hookDecode+" 的 result", rawResult, "mime")
	if err != nil {
		return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	out := Result{Kind: kind, MIME: mime}
	switch kind {
	case ResultText:
		text, err := fieldString(hookDecode+" 的 result", rawResult, "text")
		if err != nil {
			return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
		}
		out.Text = text
	case ResultMedia:
		encoded, err := fieldString(hookDecode+" 的 result", rawResult, "body_base64")
		if err != nil {
			return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
		}
		if encoded == "" {
			return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 result.kind 是 media，但 body_base64 是空的", hookDecode)}
		}
		media, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 result.body_base64 不是合法 base64：%v", hookDecode, err)}
		}
		out.Media = media
	case ResultJSON:
		rawJSON, present := rawResult["json"]
		if !present {
			return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 result.kind 是 json，但缺 json 字段", hookDecode)}
		}
		encoded, err := json.Marshal(rawJSON)
		if err != nil {
			return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 result.json 不能序列化：%v", hookDecode, err)}
		}
		out.JSON = encoded
	default:
		return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 result.kind 只能是 media、text 或 json，得到 %q", hookDecode, kind)}
	}

	usage, err := usageFromJSON(obj["usage"])
	if err != nil {
		return Result{}, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 usage：%v", hookDecode, err)}
	}
	out.Usage = usage
	if reason, err := fieldString(hookDecode, obj, "finish_reason"); err != nil {
		return Result{}, &HookError{Code: ErrorPluginError, Message: err.Error()}
	} else if reason != "" {
		s.warnf("%s 返回了 finish_reason=%q，Result 没有对应字段，已忽略", hookDecode, reason)
	}
	return out, nil
}

func usageFromJSON(value any) (Usage, error) {
	if value == nil {
		return Usage{}, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return Usage{}, fmt.Errorf("必须是对象，得到%s", describe(value))
	}
	fields := []string{"input_tokens", "output_tokens", "cached_input_tokens", "cache_write_tokens",
		"reasoning_tokens", "total_tokens", "audio_seconds", "tts_characters", "source"}
	if err := checkKeys("usage", obj, fields...); err != nil {
		return Usage{}, err
	}
	var out Usage
	targets := []struct {
		key string
		dst *int64
	}{
		{"input_tokens", &out.In},
		{"output_tokens", &out.Out},
		{"cached_input_tokens", &out.CacheRead},
		{"cache_write_tokens", &out.CacheWrite},
		{"reasoning_tokens", &out.Reasoning},
		{"total_tokens", &out.Total},
		{"audio_seconds", &out.AudioSeconds},
		{"tts_characters", &out.TTSChars},
	}
	for _, target := range targets {
		number, err := fieldInt("usage", obj, target.key)
		if err != nil {
			return Usage{}, err
		}
		*target.dst = number
	}
	source, err := fieldString("usage", obj, "source")
	if err != nil {
		return Usage{}, err
	}
	out.Source = source
	return out, nil
}

// events 把 decode_stream 的返回值拆成事件、下一次的 state 与 done。
func (s *jsSandbox) events(value any) ([]StreamEvent, any, bool, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, nil, false, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 要返回 {events:[…], state?, done?}，得到%s", hookStream, describe(value))}
	}
	if err := checkKeys(hookStream+" 的返回值", obj, "events", "state", "done"); err != nil {
		return nil, nil, false, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	rawEvents, present := obj["events"]
	if !present {
		return nil, nil, false, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的返回值缺 events 字段", hookStream)}
	}
	list, ok := rawEvents.([]any)
	if !ok {
		return nil, nil, false, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 events 必须是数组，得到%s", hookStream, describe(rawEvents))}
	}
	events := make([]StreamEvent, 0, len(list))
	for i, item := range list {
		event, err := streamEvent(item)
		if err != nil {
			return nil, nil, false, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("%s 的 events[%d]：%v", hookStream, i, err)}
		}
		events = append(events, event)
	}
	done, err := fieldBool(hookStream, obj, "done")
	if err != nil {
		return nil, nil, false, &HookError{Code: ErrorPluginError, Message: err.Error()}
	}
	return events, obj["state"], done, nil
}

func streamEvent(value any) (StreamEvent, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return StreamEvent{}, fmt.Errorf("必须是对象，得到%s", describe(value))
	}
	kind, err := fieldString("事件", obj, "type")
	if err != nil {
		return StreamEvent{}, err
	}
	if kind == "" {
		return StreamEvent{}, errors.New("缺 type 字段")
	}
	allowed := []string{"type", "index"}
	switch kind {
	case EventTextDelta, EventReasoningDelta:
		allowed = append(allowed, "text")
	case EventToolStart:
		allowed = append(allowed, "id", "name", "arguments")
	case EventToolDelta, EventToolEnd:
		allowed = append(allowed, "id", "name", "arguments")
	case EventUsage:
		allowed = append(allowed, "usage")
	case EventFinish:
		allowed = append(allowed, "reason")
	case EventError:
		allowed = append(allowed, "error")
	default:
		return StreamEvent{}, fmt.Errorf("type %q 不是宿主认识的事件类型：可用的是 %s", kind,
			strings.Join([]string{EventTextDelta, EventReasoningDelta, EventToolStart, EventToolDelta, EventToolEnd, EventUsage, EventFinish, EventError}, "、"))
	}
	if err := checkKeys("事件", obj, allowed...); err != nil {
		return StreamEvent{}, err
	}
	event := StreamEvent{Type: kind}
	index, err := fieldInt("事件", obj, "index")
	if err != nil {
		return StreamEvent{}, err
	}
	if index < 0 {
		return StreamEvent{}, fmt.Errorf("事件 的 index 不能是负数，得到 %d", index)
	}
	event.Index = int(index)
	text, err := fieldString("事件", obj, "text")
	if err != nil {
		return StreamEvent{}, err
	}
	id, err := fieldString("事件", obj, "id")
	if err != nil {
		return StreamEvent{}, err
	}
	name, err := fieldString("事件", obj, "name")
	if err != nil {
		return StreamEvent{}, err
	}
	arguments, err := fieldString("事件", obj, "arguments")
	if err != nil {
		return StreamEvent{}, err
	}
	event.Text, event.ID, event.Name, event.Arguments = text, id, name, arguments

	switch kind {
	case EventTextDelta, EventReasoningDelta:
		if event.Text == "" {
			return StreamEvent{}, fmt.Errorf("type 是 %s，但 text 是空的", kind)
		}
	case EventUsage:
		usage, err := usageFromJSON(obj["usage"])
		if err != nil {
			return StreamEvent{}, err
		}
		event.Usage = &usage
	case EventFinish:
		reason, err := fieldString("事件", obj, "reason")
		if err != nil {
			return StreamEvent{}, err
		}
		switch reason {
		case "stop", "length", "tool_use", "stop_sequence":
		default:
			return StreamEvent{}, fmt.Errorf("finish 事件的 reason 只能是 stop、length、tool_use 或 stop_sequence，得到 %q", reason)
		}
		event.Reason = reason
	case EventError:
		message, err := fieldString("事件", obj, "error")
		if err != nil {
			return StreamEvent{}, err
		}
		if message == "" {
			return StreamEvent{}, errors.New("error 事件的 error 是空的")
		}
		event.Error = message
	}
	return event, nil
}
