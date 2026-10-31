package plugin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lax/internal/plugin"
)

// 沙箱测试只走公开入口：OpenSandbox 加 Sandbox 的方法，插件脚本放在 testdata/js。
// 网络一律走 httptest 假服务，不连真实站点。

func baseOptions(id, entry string) plugin.SandboxOptions {
	return plugin.SandboxOptions{
		Manifest: plugin.Manifest{
			Schema:  plugin.ManifestSchema,
			ID:      id,
			Name:    id,
			Version: "1.0.0",
			Kind:    plugin.KindProvider,
			Runtime: plugin.RuntimeMoejs,
			ABI:     plugin.ABIv1,
			Entry:   entry,
			Permissions: plugin.Permissions{
				Filesystem: "none",
				Process:    "none",
				Env:        "none",
			},
		},
		Root:  "testdata/js",
		Entry: entry,
		Trust: plugin.TrustSandboxed,
		Timeouts: plugin.HookTimeouts{
			Prepare: 500 * time.Millisecond,
			Decode:  time.Second,
			Stream:  time.Second,
		},
	}
}

func openSandbox(t *testing.T, options plugin.SandboxOptions) plugin.Sandbox {
	t.Helper()
	sandbox, err := plugin.OpenSandbox(options)
	if err != nil {
		t.Fatalf("打开沙箱 %s：%v", options.Entry, err)
	}
	t.Cleanup(func() { _ = sandbox.Close() })
	return sandbox
}

func openWithNetwork(t *testing.T, id, entry string, network plugin.NetworkPerm, options map[string]string) plugin.Sandbox {
	t.Helper()
	opts := baseOptions(id, entry)
	opts.Manifest.Permissions.Network = network
	opts.Options = options
	return openSandbox(t, opts)
}

func requireHookError(t *testing.T, err error, code, contains string) *plugin.HookError {
	t.Helper()
	var hookErr *plugin.HookError
	if !errors.As(err, &hookErr) {
		t.Fatalf("期望 *plugin.HookError，得到 %T：%v", err, err)
	}
	if hookErr.Code != code {
		t.Fatalf("错误码是 %q，期望 %q（文案：%s）", hookErr.Code, code, hookErr.Message)
	}
	if contains != "" && !strings.Contains(hookErr.Message, contains) {
		t.Fatalf("错误文案 %q 里没有 %q", hookErr.Message, contains)
	}
	return hookErr
}

func TestSandboxPrepareRequestShapes(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		sandbox := openSandbox(t, baseOptions("com.example.prepare-json", "prepare-json.js"))
		plan, err := sandbox.PrepareRequest(plugin.Input{
			Operation: plugin.OpSpeechSynthesize,
			Wire:      plugin.WireOpenAIChat,
			Model:     "upstream-model",
			Text:      "你好",
		})
		if err != nil {
			t.Fatalf("PrepareRequest：%v", err)
		}
		if plan.Path != "/v1/speech" {
			t.Fatalf("path = %q，期望 /v1/speech", plan.Path)
		}
		if len(plan.Body) != 0 {
			t.Fatalf("body 不该有原始字节，得到 %q", plan.Body)
		}
		if plan.JSON["model"] != "upstream-model" || plan.JSON["text"] != "你好" {
			t.Fatalf("JSON = %v，期望带上 model 与 text", plan.JSON)
		}
	})

	t.Run("raw", func(t *testing.T) {
		sandbox := openSandbox(t, baseOptions("com.example.prepare-raw", "prepare-raw.js"))
		plan, err := sandbox.PrepareRequest(plugin.Input{Text: "audio"})
		if err != nil {
			t.Fatalf("PrepareRequest：%v", err)
		}
		if plan.Path != "/v1/raw" {
			t.Fatalf("path = %q，期望 /v1/raw", plan.Path)
		}
		if string(plan.Body) != "raw:audio" {
			t.Fatalf("body = %q，期望 raw:audio", plan.Body)
		}
		if plan.JSON != nil {
			t.Fatalf("字符串 body 不该同时给 JSON，得到 %v", plan.JSON)
		}
	})
}

func TestSandboxDecodeResponse(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.decode", "decode.js"))

	t.Run("text", func(t *testing.T) {
		result, err := sandbox.DecodeResponse(plugin.Input{}, plugin.Response{
			Status: 200, ContentType: "text/plain", Body: []byte("纯文本"),
		})
		if err != nil {
			t.Fatalf("DecodeResponse：%v", err)
		}
		if result.Kind != plugin.ResultText || result.Text != "纯文本" {
			t.Fatalf("result = %+v，期望 text 结果", result)
		}
	})

	t.Run("media", func(t *testing.T) {
		audio := []byte{0x00, 0xff, 0x10}
		result, err := sandbox.DecodeResponse(plugin.Input{}, plugin.Response{
			Status: 200, ContentType: "audio/mpeg", Body: audio,
		})
		if err != nil {
			t.Fatalf("DecodeResponse：%v", err)
		}
		if result.Kind != plugin.ResultMedia || result.MIME != "audio/mpeg" {
			t.Fatalf("result = %+v，期望 media 结果", result)
		}
		if string(result.Media) != string(audio) {
			t.Fatalf("media = %v，期望 %v", result.Media, audio)
		}
	})

	t.Run("json", func(t *testing.T) {
		result, err := sandbox.DecodeResponse(plugin.Input{}, plugin.Response{
			Status: 200, ContentType: "application/json", Body: []byte(`{"text":"hi"}`),
		})
		if err != nil {
			t.Fatalf("DecodeResponse：%v", err)
		}
		if result.Kind != plugin.ResultJSON {
			t.Fatalf("kind = %q，期望 json", result.Kind)
		}
		var decoded map[string]any
		if err := json.Unmarshal(result.JSON, &decoded); err != nil {
			t.Fatalf("result.JSON 不是 JSON：%v", err)
		}
		if decoded["text"] != "hi" || decoded["mouth"] != "closed" {
			t.Fatalf("result.JSON = %v，期望带上 text 与 mouth", decoded)
		}
		usage := result.Usage
		if usage.In != 3 || usage.Out != 5 || usage.CacheRead != 1 || usage.Total != 8 || usage.Source != "upstream" {
			t.Fatalf("usage = %+v，与脚本里的数字不一致", usage)
		}
	})
}

func TestSandboxDecodeStreamState(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.stream", "stream.js"))
	input := plugin.Input{Wire: plugin.WireOpenAIChat}

	events, err := sandbox.DecodeStream(input, plugin.StreamChunk{Seq: 0, Data: []byte("a")})
	if err != nil {
		t.Fatalf("第一块：%v", err)
	}
	if len(events) != 1 || events[0].Text != "data=a state=null module=1" {
		t.Fatalf("第一块的事件 = %+v，期望 state 为空、模块变量从 1 开始", events)
	}

	events, err = sandbox.DecodeStream(input, plugin.StreamChunk{Seq: 1, Data: []byte("b"), Final: true})
	if err != nil {
		t.Fatalf("第二块：%v", err)
	}
	if len(events) != 2 {
		t.Fatalf("第二块的事件 = %+v，期望 text.delta 与 finish", events)
	}
	if events[0].Text != "data=b state=1 module=2" {
		t.Fatalf("第二块的 text = %q，期望看到上一块的 state 与同一个 Runtime", events[0].Text)
	}
	if events[1].Type != plugin.EventFinish || events[1].Reason != "stop" {
		t.Fatalf("finish 事件 = %+v", events[1])
	}

	// Final 之后 state 清空、Runtime 重建，下一块属于另一条流。
	events, err = sandbox.DecodeStream(input, plugin.StreamChunk{Seq: 0, Data: []byte("c")})
	if err != nil {
		t.Fatalf("新流的块：%v", err)
	}
	if events[0].Text != "data=c state=null module=1" {
		t.Fatalf("新流的 text = %q，期望 state 与模块变量都归零", events[0].Text)
	}
}

func TestSandboxESMDefaultAndImport(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.esm", "esm.js"))
	plan, err := sandbox.PrepareRequest(plugin.Input{Text: "世界"})
	if err != nil {
		t.Fatalf("PrepareRequest：%v", err)
	}
	if plan.JSON["greeting"] != "hello 世界!" {
		t.Fatalf("greeting = %v，期望 import 进来的函数与常量都可用", plan.JSON["greeting"])
	}
	if plan.JSON["marker"] != "esm" {
		t.Fatalf("marker = %v，期望 export const 具名导出可用", plan.JSON["marker"])
	}
	if plan.JSON["uuid"] != "string" {
		t.Fatalf("uuid 类型 = %v，期望 lax.randomUUID() 返回字符串", plan.JSON["uuid"])
	}
}

func TestSandboxESMCycle(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.cycle", "cycle-a.js"))
	plan, err := sandbox.PrepareRequest(plugin.Input{Text: "x"})
	if err != nil {
		t.Fatalf("PrepareRequest：%v", err)
	}
	if plan.JSON["a"] != "b(x)" {
		t.Fatalf("a = %v，期望互相 import 的两个文件都能求值", plan.JSON["a"])
	}
}

func TestSandboxImportRejected(t *testing.T) {
	cases := []struct {
		entry    string
		contains string
		// line 是改写阶段报错的源码行号；0 表示错误发生在加载期，没有行号。
		line int
	}{
		{"bad-node.js", `"node:fs" 不是相对路径`, 1},
		{"bad-bare.js", `"lodash" 不是相对路径`, 1},
		{"bad-star.js", "不支持 import * as", 1},
		{"bad-export.js", "不支持 export {", 1},
		{"bad-dynamic.js", "不支持动态 import()", 1},
		{"bad-escape.js", "插件目录之外", 0},
	}
	for _, item := range cases {
		t.Run(item.entry, func(t *testing.T) {
			_, err := plugin.OpenSandbox(baseOptions("com.example."+item.entry, item.entry))
			if err == nil {
				t.Fatalf("期望 %s 被拒绝", item.entry)
			}
			if !strings.Contains(err.Error(), item.contains) {
				t.Fatalf("错误 %q 里没有 %q", err, item.contains)
			}
			location := item.entry
			if item.line > 0 {
				location = fmt.Sprintf("%s:%d", item.entry, item.line)
			}
			if !strings.Contains(err.Error(), location) {
				t.Fatalf("错误 %q 里没有 %q", err, location)
			}
		})
	}
}

func TestSandboxLaxError(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.errors", "errors.js"))

	hookErr := requireHookError(t, prepare(t, sandbox, "lax"), plugin.ErrorRateLimited, "上游限流")
	if !hookErr.Retryable || hookErr.Details != "acct=7" {
		t.Fatalf("retryable = %v，details = %q，期望 true 与 acct=7", hookErr.Retryable, hookErr.Details)
	}

	requireHookError(t, prepare(t, sandbox, "unknown"), plugin.ErrorPluginError, "未知的错误码")
	requireHookError(t, prepare(t, sandbox, "plain"), plugin.ErrorPluginError, "普通错误")
	requireHookError(t, prepare(t, sandbox, "shape"), plugin.ErrorPluginError, "要返回对象")
	requireHookError(t, prepare(t, sandbox, "unknownfield"), plugin.ErrorPluginError, "不认识的字段")

	if _, err := sandbox.PrepareRequest(plugin.Input{Text: "ok"}); err != nil {
		t.Fatalf("正常路径失败：%v", err)
	}
}

func prepare(t *testing.T, sandbox plugin.Sandbox, text string) error {
	t.Helper()
	_, err := sandbox.PrepareRequest(plugin.Input{Text: text})
	return err
}

func TestSandboxTimeout(t *testing.T) {
	opts := baseOptions("com.example.loop", "loop.js")
	opts.Timeouts.Prepare = 50 * time.Millisecond
	sandbox := openSandbox(t, opts)

	requireHookError(t, prepare(t, sandbox, "loop"), plugin.ErrorPluginTimeout, "prepare_request")

	// 超时的 Runtime 被丢弃，下一次调用照常新建。
	plan, err := sandbox.PrepareRequest(plugin.Input{Text: "ok"})
	if err != nil {
		t.Fatalf("超时后的调用失败：%v", err)
	}
	if plan.JSON["ok"] != true {
		t.Fatalf("plan = %v，期望 ok 为 true", plan.JSON)
	}
}

func TestSandboxGlobals(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.globals", "globals.js"))
	plan, err := sandbox.PrepareRequest(plugin.Input{})
	if err != nil {
		t.Fatalf("PrepareRequest：%v", err)
	}
	for name, value := range plan.JSON {
		if name == "hasFetch" || name == "url" || name == "lax" || name == "console" {
			continue
		}
		if value != "undefined" {
			t.Fatalf("globalThis.%s 是 %v，期望 undefined", name, value)
		}
	}
	if plan.JSON["hasFetch"] != false {
		t.Fatalf("globalThis 里还有 fetch")
	}
	if plan.JSON["url"] != "function" || plan.JSON["lax"] != "object" || plan.JSON["console"] != "object" {
		t.Fatalf("注入的全局不对：%v", plan.JSON)
	}
}

func TestSandboxOutputTooLarge(t *testing.T) {
	opts := baseOptions("com.example.output", "output.js")
	// 造 9 MiB 返回值本身要时间，用 decode 的宽上限，量的只是超限判定。
	opts.Timeouts.Decode = plugin.MaxDecodeTimeout
	sandbox := openSandbox(t, opts)
	_, err := sandbox.DecodeResponse(plugin.Input{}, plugin.Response{ContentType: "application/json"})
	requireHookError(t, err, plugin.ErrorPluginError, "上限")
}

func TestSandboxProtoRejected(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.proto", "proto.js"))
	requireHookError(t, prepare(t, sandbox, ""), plugin.ErrorPluginError, "__proto__")
}

func TestSandboxHTTPRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/redirect":
			http.Redirect(w, r, "/echo", http.StatusFound)
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("x", 64))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	allowed := plugin.NetworkPerm{
		Mode:                "host-proxy",
		Schemes:             []string{"http"},
		Hosts:               []string{"127.0.0.1"},
		MaxControlBodyBytes: 32,
	}

	t.Run("命中白名单", func(t *testing.T) {
		sandbox := openWithNetwork(t, "com.example.http", "http.js", allowed, map[string]string{"url": server.URL + "/echo"})
		plan, err := sandbox.PrepareRequest(plugin.Input{})
		if err != nil {
			t.Fatalf("PrepareRequest：%v", err)
		}
		if plan.JSON["status"] != float64(http.StatusOK) {
			t.Fatalf("status = %v，期望 200", plan.JSON["status"])
		}
		if !strings.Contains(plan.JSON["body"].(string), `"ok":true`) {
			t.Fatalf("body = %v，期望带上上游响应体", plan.JSON["body"])
		}
	})

	t.Run("主机不在白名单", func(t *testing.T) {
		network := allowed
		network.Hosts = []string{"api.example.com"}
		sandbox := openWithNetwork(t, "com.example.http-deny", "http.js", network, map[string]string{"url": server.URL + "/echo"})
		requireHookError(t, prepare(t, sandbox, ""), plugin.ErrorPluginError, "白名单")
	})

	t.Run("mode 是 none", func(t *testing.T) {
		sandbox := openWithNetwork(t, "com.example.http-none", "http.js", plugin.NetworkPerm{Mode: "none"}, map[string]string{"url": server.URL + "/echo"})
		requireHookError(t, prepare(t, sandbox, ""), plugin.ErrorPluginError, "host-proxy")
	})

	t.Run("重定向被拒", func(t *testing.T) {
		sandbox := openWithNetwork(t, "com.example.http-redirect", "http.js", allowed, map[string]string{"url": server.URL + "/redirect"})
		requireHookError(t, prepare(t, sandbox, ""), plugin.ErrorPluginError, "不跟随重定向")
	})

	t.Run("响应体超限", func(t *testing.T) {
		sandbox := openWithNetwork(t, "com.example.http-big", "http.js", allowed, map[string]string{"url": server.URL + "/big"})
		requireHookError(t, prepare(t, sandbox, ""), plugin.ErrorPluginError, "响应体超过")
	})
}

type testClock struct {
	at time.Time
}

func (c *testClock) Now() time.Time { return c.at }

func (c *testClock) Advance(d time.Duration) { c.at = c.at.Add(d) }

func TestSandboxCache(t *testing.T) {
	clock := &testClock{at: time.Unix(1_700_000_000, 0)}

	first := baseOptions("com.example.cache-a", "cache.js")
	first.Now = clock.Now
	sandbox := openSandbox(t, first)

	value := prepareValue(t, sandbox, "k")
	if value["before"] != nil {
		t.Fatalf("第一次 before = %v，期望 null", value["before"])
	}
	if after := value["after"].(map[string]any); after["n"] != float64(1) {
		t.Fatalf("第一次 after = %v，期望 n 是 1", after)
	}

	value = prepareValue(t, sandbox, "k")
	if before := value["before"].(map[string]any); before["n"] != float64(1) {
		t.Fatalf("第二次 before = %v，期望读到上一次写的 n=1", before)
	}

	// 不同插件 ID 之间隔离。
	other := openSandbox(t, baseOptions("com.example.cache-b", "cache.js"))
	if value := prepareValue(t, other, "k"); value["before"] != nil {
		t.Fatalf("另一个插件的 before = %v，期望 null", value["before"])
	}

	// TTL：时钟推过 30ms 之后读不到。
	ttl := baseOptions("com.example.cache-ttl", "cache.js")
	ttl.Now = clock.Now
	ttl.Options = map[string]string{"ttl_ms": "30"}
	ttlSandbox := openSandbox(t, ttl)
	_ = prepareValue(t, ttlSandbox, "k")
	clock.Advance(100 * time.Millisecond)
	if value := prepareValue(t, ttlSandbox, "k"); value["before"] != nil {
		t.Fatalf("过期的 before = %v，期望 null", value["before"])
	}

	// delete：删掉之后读不到。用同一个插件 ID，顺便验证缓存跨 Sandbox 实例共享。
	deleteOptions := baseOptions("com.example.cache-a", "cache.js")
	deleteOptions.Now = clock.Now
	deleteOptions.Options = map[string]string{"action": "delete"}
	deleteSandbox := openSandbox(t, deleteOptions)
	value = prepareValue(t, deleteSandbox, "k")
	if value["deleted"] != true || value["after"] != nil {
		t.Fatalf("delete 的结果 = %v，期望 true 与 null", value)
	}
}

func prepareValue(t *testing.T, sandbox plugin.Sandbox, text string) map[string]any {
	t.Helper()
	plan, err := sandbox.PrepareRequest(plugin.Input{Text: text})
	if err != nil {
		t.Fatalf("PrepareRequest：%v", err)
	}
	return plan.JSON
}

func TestSandboxLog(t *testing.T) {
	var lines []string
	opts := baseOptions("com.example.log", "log.js")
	opts.Logf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	sandbox := openSandbox(t, opts)
	if _, err := sandbox.PrepareRequest(plugin.Input{Text: "第一行\n第二行"}); err != nil {
		t.Fatalf("PrepareRequest：%v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("日志行数 = %d，期望 2：%v", len(lines), lines)
	}
	if strings.Contains(strings.Join(lines, ""), "\n") {
		t.Fatalf("日志里有换行：%q", lines)
	}
	if !strings.Contains(lines[0], "warn") || !strings.Contains(lines[0], "第一行 第二行") {
		t.Fatalf("第一条日志 = %q，期望级别与合并后的正文", lines[0])
	}
	if !strings.Contains(lines[1], "console 多参数") {
		t.Fatalf("第二条日志 = %q，期望 console.error 的多参数拼成一行", lines[1])
	}
}

func TestSandboxEmptyModule(t *testing.T) {
	_, err := plugin.OpenSandbox(baseOptions("com.example.empty", "empty.js"))
	if err == nil || !strings.Contains(err.Error(), "没有导出") {
		t.Fatalf("期望拒绝没有导出的入口，得到 %v", err)
	}
}

func TestSandboxClose(t *testing.T) {
	sandbox := openSandbox(t, baseOptions("com.example.close", "prepare-raw.js"))
	if _, err := sandbox.PrepareRequest(plugin.Input{Text: "a"}); err != nil {
		t.Fatalf("关之前的调用失败：%v", err)
	}
	if err := sandbox.Close(); err != nil {
		t.Fatalf("第一次 Close：%v", err)
	}
	if err := sandbox.Close(); err != nil {
		t.Fatalf("第二次 Close 不幂等：%v", err)
	}
	requireHookError(t, prepare(t, sandbox, "b"), plugin.ErrorInternal, "已关闭")
}
