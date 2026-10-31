package plugin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// 注入给插件的 lax API。它是插件唯一的出口：控制面网络、缓存、日志与工具函数，
// 没有文件、进程、环境变量与 fetch。所有限制都在这里判，插件代码自己改不了。

const (
	// 控制面请求的三层上限：单次、单次 hook 的调用次数与总时长。
	sandboxHTTPCallTimeout = 15 * time.Second
	sandboxHTTPCalls       = 4
	sandboxHTTPTotal       = 30 * time.Second
	// defaultControlBodyBytes 是 manifest 没写 max_control_body_bytes 时的响应体上限。
	defaultControlBodyBytes = 1 << 20
	// sandboxLogBytes 是 lax.log 单条消息的上限。
	sandboxLogBytes = 8 << 10
	// cacheMaxBytes 是一个插件所有命名空间加起来的上限。
	cacheMaxBytes = 1 << 20
)

// forbiddenGlobals 是不该存在的全局名。goja 本来就不提供其中大部分，这里显式删一遍，
// 免得以后注入别的运行时时悄悄带进来；`eval` 与 `Function` 是 goja 默认有而声明式沙箱不要的：
// 它们让代码可以绕过静态 import 检查临时拼代码，限制不了的东西不应当成能力存在。
var forbiddenGlobals = []string{
	"fetch", "process", "require", "module", "exports", "Bun", "child_process",
	"XMLHttpRequest", "WebSocket", "fs", "global", "Buffer", "setTimeout", "setInterval",
	"eval", "Function",
}

// netBudget 是一次 hook 调用的控制面预算，每次 invoke 归零。
type netBudget struct {
	calls int
	spent time.Duration
}

type sandboxAPI struct {
	sandbox *jsSandbox
	vm      *jsVM
}

func (a *sandboxAPI) install() error {
	rt := a.vm.rt
	lax := rt.NewObject()
	_ = lax.Set("log", a.log)
	_ = lax.Set("now", func(goja.FunctionCall) goja.Value { return rt.ToValue(a.sandbox.o.Now().UnixMilli()) })
	_ = lax.Set("randomUUID", func(goja.FunctionCall) goja.Value { return rt.ToValue(newUUID()) })
	_ = lax.Set("sha256", a.sha256Fn)
	_ = lax.Set("hmacSHA256", a.hmacFn)
	_ = lax.Set("error", a.errorFn)

	base64Object := rt.NewObject()
	_ = base64Object.Set("encode", func(call goja.FunctionCall) goja.Value {
		return rt.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Argument(0).String())))
	})
	_ = base64Object.Set("decode", func(call goja.FunctionCall) goja.Value {
		decoded, err := base64.StdEncoding.DecodeString(call.Argument(0).String())
		if err != nil {
			a.throwf(ErrorInvalidInput, "lax.base64.decode 的输入不是合法 base64：%v", err)
		}
		return rt.ToValue(string(decoded))
	})
	_ = lax.Set("base64", base64Object)

	httpObject := rt.NewObject()
	_ = httpObject.Set("request", a.httpRequest)
	_ = lax.Set("http", httpObject)

	cacheObject := rt.NewObject()
	_ = cacheObject.Set("get", a.cacheGet)
	_ = cacheObject.Set("set", a.cacheSet)
	_ = cacheObject.Set("delete", a.cacheDelete)
	_ = lax.Set("cache", cacheObject)

	// 本版没有媒体引用通道：音频进 ctx.input.audio_base64，出 ctx.response.text。
	mediaObject := rt.NewObject()
	unavailable := func(name string) func(goja.FunctionCall) goja.Value {
		return func(goja.FunctionCall) goja.Value {
			a.throwf(ErrorPluginError, "lax.media.%s 本版不可用：插件拿不到媒体引用，音频用 ctx.input.audio_base64，文本用 ctx.response.text", name)
			return goja.Undefined()
		}
	}
	for _, name := range []string{"read", "write", "list", "delete"} {
		_ = mediaObject.Set(name, unavailable(name))
	}
	_ = lax.Set("media", mediaObject)

	console := rt.NewObject()
	for _, name := range []string{"log", "info", "warn", "error"} {
		level := name
		if level == "log" {
			level = "info"
		}
		_ = console.Set(name, func(call goja.FunctionCall) goja.Value {
			a.writeLog(level, joinArguments(call))
			return goja.Undefined()
		})
	}
	_ = rt.Set("console", console)
	_ = rt.Set("URL", a.urlConstructor)

	for _, name := range forbiddenGlobals {
		_ = rt.GlobalObject().Delete(name)
	}
	return rt.Set("lax", lax)
}

// joinArguments 把 console 的多个参数拼成一行，与浏览器的观感一致。
func joinArguments(call goja.FunctionCall) string {
	parts := make([]string, 0, len(call.Arguments))
	for _, arg := range call.Arguments {
		if arg == nil {
			parts = append(parts, "undefined")
			continue
		}
		parts = append(parts, arg.String())
	}
	return strings.Join(parts, " ")
}

func (a *sandboxAPI) log(call goja.FunctionCall) goja.Value {
	level := "info"
	message := ""
	switch len(call.Arguments) {
	case 0:
	case 1:
		message = call.Argument(0).String()
	default:
		level = call.Argument(0).String()
		message = call.Argument(1).String()
	}
	a.writeLog(level, message)
	return goja.Undefined()
}

func (a *sandboxAPI) writeLog(level, message string) {
	switch level {
	case "debug", "info", "warn", "error":
	default:
		level = "info"
	}
	// 日志是一行一条：换行会把后续内容伪装成宿主自己的输出。
	message = strings.NewReplacer("\n", " ", "\r", " ").Replace(message)
	if len(message) > sandboxLogBytes {
		message = message[:sandboxLogBytes] + "…"
	}
	a.sandbox.logf("插件 %s %s: %s", a.sandbox.o.Manifest.ID, level, message)
}

// errorObject 造一个可以 throw 的错误对象。宿主的 readLaxError 只认这里的标记。
func (a *sandboxAPI) errorObject(code, message, details string, retryable bool) *goja.Object {
	if !KnownErrorCode(code) {
		code = ErrorPluginError
	}
	rt := a.vm.rt
	object := rt.NewObject()
	_ = object.Set(laxErrorMarker, true)
	_ = object.Set("name", "LaxError")
	_ = object.Set("code", code)
	_ = object.Set("message", message)
	_ = object.Set("retryable", retryable)
	_ = object.Set("details", details)
	return object
}

func (a *sandboxAPI) throwf(code, format string, args ...any) {
	panic(a.errorObject(code, fmt.Sprintf(format, args...), "", false))
}

// errorFn 是 lax.error(code, message, options?)：返回错误对象，由插件决定在哪 throw。
func (a *sandboxAPI) errorFn(call goja.FunctionCall) goja.Value {
	code := call.Argument(0).String()
	message := call.Argument(1).String()
	retryable := false
	details := ""
	if options, ok := call.Argument(2).(*goja.Object); ok {
		if raw := options.Get("retryable"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
			retryable = raw.ToBoolean()
		}
		if raw := options.Get("details"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
			details = raw.String()
		}
	}
	return a.errorObject(code, message, details, retryable)
}

func (a *sandboxAPI) sha256Fn(call goja.FunctionCall) goja.Value {
	sum := sha256.Sum256([]byte(call.Argument(0).String()))
	return a.vm.rt.ToValue(hex.EncodeToString(sum[:]))
}

func (a *sandboxAPI) hmacFn(call goja.FunctionCall) goja.Value {
	mac := hmac.New(sha256.New, []byte(call.Argument(1).String()))
	mac.Write([]byte(call.Argument(0).String()))
	return a.vm.rt.ToValue(hex.EncodeToString(mac.Sum(nil)))
}

// newUUID 走 crypto/rand，不用外部依赖；版本位按 v4 写。
func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// urlConstructor 只解析与格式化，不发请求：插件要联网只能走 lax.http.request。
func (a *sandboxAPI) urlConstructor(call goja.ConstructorCall) *goja.Object {
	rt := a.vm.rt
	raw := call.Argument(0).String()
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(rt.NewTypeError(fmt.Sprintf("URL 解析失败：%v", err)))
	}
	object := rt.NewObject()
	_ = object.Set("href", parsed.String())
	_ = object.Set("protocol", parsed.Scheme+":")
	_ = object.Set("host", parsed.Host)
	_ = object.Set("hostname", parsed.Hostname())
	_ = object.Set("pathname", parsed.Path)
	_ = object.Set("search", parsed.RawQuery)
	_ = object.Set("toString", func(goja.FunctionCall) goja.Value { return rt.ToValue(parsed.String()) })
	return object
}

// ------------------------------------------------------------------ 控制面网络

func (a *sandboxAPI) httpRequest(call goja.FunctionCall) goja.Value {
	s := a.sandbox
	perm := s.o.Manifest.Permissions.Network
	if perm.Mode != "host-proxy" {
		a.throwf(ErrorPluginError, "lax.http.request 需要 manifest 里声明 permissions.network.mode = \"host-proxy\" 与 hosts 白名单，当前 mode 是 %q", perm.Mode)
	}
	options, ok := call.Argument(0).(*goja.Object)
	if !ok {
		a.throwf(ErrorPluginError, "lax.http.request 的参数要是对象 {method, url, headers, body, timeout_ms}，得到%s", gojaShape(call.Argument(0)))
	}
	rawURL := optionString(options, "url")
	if rawURL == "" {
		a.throwf(ErrorPluginError, "lax.http.request 缺 url")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		a.throwf(ErrorPluginError, "lax.http.request 的 url 解析失败：%v", err)
	}
	if err := a.allowedURL(parsed, perm); err != nil {
		a.throwf(ErrorPluginError, "lax.http.request 拒绝 %s：%v", parsed.Redacted(), err)
	}

	method := strings.ToUpper(optionString(options, "method"))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		a.throwf(ErrorPluginError, "lax.http.request 的 method 只支持 GET、POST、PUT、PATCH、DELETE、HEAD，得到 %q", method)
	}

	var body io.Reader
	if raw := options.Get("body"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
		text, isString := raw.Export().(string)
		if !isString {
			a.throwf(ErrorPluginError, "lax.http.request 的 body 只能是字符串，得到%s", gojaShape(raw))
		}
		body = strings.NewReader(text)
	}

	headers := http.Header{}
	if raw := options.Get("headers"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
		object, isObject := raw.(*goja.Object)
		if !isObject {
			a.throwf(ErrorPluginError, "lax.http.request 的 headers 必须是对象，得到%s", gojaShape(raw))
		}
		for _, key := range object.Keys() {
			value := object.Get(key)
			if !goja.IsString(value) {
				a.throwf(ErrorPluginError, "lax.http.request 的 headers.%s 必须是字符串，得到%s", key, gojaShape(value))
			}
			headers.Set(key, value.String())
		}
	}

	// 预算是每次 hook 调用一份：调用次数与总时长都按 s.net 记。
	callLimit := sandboxHTTPCalls
	if perm.MaxControlCalls > 0 && perm.MaxControlCalls < callLimit {
		callLimit = perm.MaxControlCalls
	}
	if s.net.calls >= callLimit {
		a.throwf(ErrorPluginError, "一次 hook 里最多 %d 次 lax.http.request，已经用满", callLimit)
	}
	remaining := sandboxHTTPTotal - s.net.spent
	if remaining <= 0 {
		a.throwf(ErrorPluginError, "一次 hook 里的控制面请求总共只能用 %s，已经用满", sandboxHTTPTotal)
	}
	timeout := sandboxHTTPCallTimeout
	if raw := options.Get("timeout_ms"); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
		switch value := raw.Export().(type) {
		case int64:
			if value > 0 {
				timeout = time.Duration(value) * time.Millisecond
			}
		case float64:
			if value > 0 {
				timeout = time.Duration(value) * time.Millisecond
			}
		default:
			a.throwf(ErrorPluginError, "lax.http.request 的 timeout_ms 必须是数字，得到%s", gojaShape(raw))
		}
	}
	if timeout > sandboxHTTPCallTimeout {
		timeout = sandboxHTTPCallTimeout
	}
	if timeout > remaining {
		timeout = remaining
	}

	request, err := http.NewRequest(method, parsed.String(), body)
	if err != nil {
		a.throwf(ErrorPluginError, "lax.http.request 构造请求失败：%v", err)
	}
	request.Header = headers
	client := &http.Client{
		Timeout: timeout,
		// 重定向会把已验证的 host 换成 manifest 没声明过的 host，一律拒绝。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("控制面请求不跟随重定向")
		},
	}
	start := s.o.Now()
	s.net.calls++
	response, err := client.Do(request)
	if err != nil {
		s.net.spent += s.o.Now().Sub(start)
		a.throwf(ErrorPluginError, "lax.http.request 请求 %s 失败：%v", parsed.Redacted(), err)
	}
	defer response.Body.Close()

	limit := perm.MaxControlBodyBytes
	if limit <= 0 {
		limit = defaultControlBodyBytes
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	s.net.spent += s.o.Now().Sub(start)
	if readErr != nil {
		a.throwf(ErrorPluginError, "lax.http.request 读响应失败：%v", readErr)
	}
	if int64(len(data)) > limit {
		a.throwf(ErrorPluginError, "lax.http.request 的响应体超过 %d 字节（permissions.network.max_control_body_bytes）", limit)
	}

	rt := a.vm.rt
	out := rt.NewObject()
	_ = out.Set("status", int64(response.StatusCode))
	responseHeaders := rt.NewObject()
	for key := range response.Header {
		_ = responseHeaders.Set(key, strings.Join(response.Header[key], ", "))
	}
	_ = out.Set("headers", responseHeaders)
	_ = out.Set("body", string(data))
	return out
}

func optionString(object *goja.Object, name string) string {
	raw := object.Get(name)
	if raw == nil || goja.IsUndefined(raw) || goja.IsNull(raw) {
		return ""
	}
	return raw.String()
}

// allowedURL 判 scheme 与 host：https 直通；http 只在本机回环且 manifest 允许时放行。
func (a *sandboxAPI) allowedURL(target *url.URL, perm NetworkPerm) error {
	host := strings.ToLower(target.Hostname())
	switch strings.ToLower(target.Scheme) {
	case "https":
	case "http":
		loopback := host == "127.0.0.1" || host == "localhost"
		if !loopback || !containsString(perm.Schemes, "http") {
			return errors.New("只允许 https：http 仅在本机回环（127.0.0.1/localhost）且 permissions.network.schemes 里写了 http 时放行")
		}
	default:
		return fmt.Errorf("只允许 https，得到 scheme %q", target.Scheme)
	}
	if host == "" {
		return errors.New("url 里没有主机名")
	}
	for _, pattern := range perm.Hosts {
		if matchHost(pattern, host) {
			return nil
		}
	}
	return fmt.Errorf("主机 %q 不在 permissions.network.hosts 白名单里（可用的是：%s）", host, strings.Join(perm.Hosts, "、"))
}

// matchHost 支持精确主机与 *.example.com 前缀通配；裸 * 在 manifest 校验时已经被拒。
func matchHost(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	if pattern == host {
		return true
	}
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok && strings.HasPrefix(suffix, ".") {
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ 缓存

type cacheEntry struct {
	data   []byte
	expire time.Time
}

// cacheStore 是进程内缓存，按插件 ID 隔离。它不落盘：进程重启就没了，
// 插件因此不能把它当持久存储。
type cacheStore struct {
	mu      sync.Mutex
	plugins map[string]map[string]map[string]cacheEntry
	bytes   map[string]int64
}

var pluginCache = &cacheStore{plugins: map[string]map[string]map[string]cacheEntry{}, bytes: map[string]int64{}}

func (c *cacheStore) get(plugin, namespace, key string, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	namespaceMap := c.plugins[plugin]
	if namespaceMap == nil {
		return nil, false
	}
	entry, ok := namespaceMap[namespace][key]
	if !ok {
		return nil, false
	}
	if !entry.expire.IsZero() && now.After(entry.expire) {
		c.deleteLocked(plugin, namespace, key)
		return nil, false
	}
	return entry.data, true
}

func (c *cacheStore) set(plugin, namespace, key string, data []byte, ttl time.Duration, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	namespaceMap := c.plugins[plugin]
	if namespaceMap == nil {
		namespaceMap = map[string]map[string]cacheEntry{}
		c.plugins[plugin] = namespaceMap
	}
	if namespaceMap[namespace] == nil {
		namespaceMap[namespace] = map[string]cacheEntry{}
	}
	size := c.bytes[plugin] + int64(len(namespace)+len(key)+len(data))
	if old, ok := namespaceMap[namespace][key]; ok {
		size -= int64(len(namespace) + len(key) + len(old.data))
	}
	if size > cacheMaxBytes {
		return fmt.Errorf("lax.cache 一个插件最多存 %d 字节（%d KiB），这次写入后会到 %d 字节", cacheMaxBytes, cacheMaxBytes>>10, size)
	}
	expire := time.Time{}
	if ttl > 0 {
		expire = now.Add(ttl)
	}
	namespaceMap[namespace][key] = cacheEntry{data: data, expire: expire}
	c.bytes[plugin] = size
	return nil
}

func (c *cacheStore) delete(plugin, namespace, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.plugins[plugin][namespace][key]
	if ok {
		c.deleteLocked(plugin, namespace, key)
	}
	return ok
}

func (c *cacheStore) deleteLocked(plugin, namespace, key string) {
	namespaceMap := c.plugins[plugin]
	if namespaceMap == nil {
		return
	}
	entry, ok := namespaceMap[namespace][key]
	if !ok {
		return
	}
	c.bytes[plugin] -= int64(len(namespace) + len(key) + len(entry.data))
	delete(namespaceMap[namespace], key)
	if len(namespaceMap[namespace]) == 0 {
		delete(namespaceMap, namespace)
	}
	if len(namespaceMap) == 0 {
		delete(c.plugins, plugin)
		delete(c.bytes, plugin)
	}
}

// drop 清掉一个插件的全部缓存，Close 时调用。
func (c *cacheStore) drop(plugin string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.plugins, plugin)
	delete(c.bytes, plugin)
}

func (a *sandboxAPI) cacheGet(call goja.FunctionCall) goja.Value {
	rt := a.vm.rt
	namespace := call.Argument(0).String()
	key := call.Argument(1).String()
	data, ok := pluginCache.get(a.sandbox.o.Manifest.ID, namespace, key, a.sandbox.o.Now())
	if !ok {
		return goja.Null()
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return goja.Null()
	}
	return rt.ToValue(out)
}

func (a *sandboxAPI) cacheSet(call goja.FunctionCall) goja.Value {
	namespace := call.Argument(0).String()
	key := call.Argument(1).String()
	plain, err := a.sandbox.toPlain(call.Argument(2), 0, map[*goja.Object]bool{})
	if err != nil {
		a.throwf(ErrorPluginError, "lax.cache.set 的值不能转成 JSON：%v", err)
	}
	data, err := json.Marshal(plain)
	if err != nil {
		a.throwf(ErrorPluginError, "lax.cache.set 的值不能转成 JSON：%v", err)
	}
	var ttl time.Duration
	if raw := call.Argument(3); raw != nil && !goja.IsUndefined(raw) && !goja.IsNull(raw) {
		milliseconds := raw.ToInteger()
		if milliseconds < 0 {
			milliseconds = 0
		}
		ttl = time.Duration(milliseconds) * time.Millisecond
	}
	if err := pluginCache.set(a.sandbox.o.Manifest.ID, namespace, key, data, ttl, a.sandbox.o.Now()); err != nil {
		a.throwf(ErrorPluginError, "%v", err)
	}
	return goja.Undefined()
}

func (a *sandboxAPI) cacheDelete(call goja.FunctionCall) goja.Value {
	deleted := pluginCache.delete(a.sandbox.o.Manifest.ID, call.Argument(0).String(), call.Argument(1).String())
	return a.vm.rt.ToValue(deleted)
}
