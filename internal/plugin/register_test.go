package plugin_test

import (
	"strings"
	"testing"

	"lax/internal/plugin"
)

const hookDir = "testdata/plugin-hook"

// TestLoadHookPlugin 钉住带代码的插件能加载：hook 方言不需要 request/response 声明。
func TestLoadHookPlugin(t *testing.T) {
	p, err := plugin.LoadDir(hookDir)
	if err != nil {
		t.Fatalf("加载 hook 插件失败: %v", err)
	}
	if p.Manifest.Runtime != plugin.RuntimeMoejs || p.Manifest.Entry != "./index.js" {
		t.Fatalf("运行时与入口 = %q / %q", p.Manifest.Runtime, p.Manifest.Entry)
	}
	if len(p.Dialects) != 1 || p.Dialects[0].Implementation.Prepare != plugin.ImplHook {
		t.Fatalf("方言 = %+v", p.Dialects)
	}
	hooks, err := p.ExportedHooks()
	if err != nil {
		t.Fatalf("核对导出钩子失败: %v", err)
	}
	if strings.Join(hooks, ",") != "prepare_request,decode_response" {
		t.Fatalf("导出钩子 = %v，期望 prepare_request 与 decode_response", hooks)
	}
}

// TestRegisterHookDialect 钉住 hook 方言登记后真的能跑：Prepare 与 Decode 都落到沙箱。
func TestRegisterHookDialect(t *testing.T) {
	p, err := plugin.LoadDir(hookDir)
	if err != nil {
		t.Fatalf("加载 hook 插件失败: %v", err)
	}
	reg := plugin.DefaultRegistry()
	if err := p.Register(reg, plugin.SandboxOptions{Trust: plugin.TrustSandboxed}); err != nil {
		t.Fatalf("登记 hook 方言失败: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Fatalf("关沙箱失败: %v", err)
		}
	}()

	d, ok := reg.Dialect("com.example.hook/speech@1")
	if !ok {
		t.Fatal("登记后按 ID 取不到方言")
	}
	if d.Operation() != plugin.OpSpeechSynthesize || d.UpstreamWire() != plugin.WireSpeech {
		t.Fatalf("方言 operation/wire = %s/%s", d.Operation(), d.UpstreamWire())
	}
	plan, err := d.Prepare(plugin.Input{Model: "hook-tts-v1", Text: "hello", Voice: "alloy"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	if plan.JSON["model"] != "hook-tts-v1" || plan.JSON["input"] != "hello" || plan.Path != "/v1/audio/speech" {
		t.Fatalf("请求 = %+v", plan)
	}
	result, err := d.Decode(plugin.Input{Text: "hello"}, plugin.Response{Status: 200, ContentType: "audio/mpeg", Body: []byte("ID3")})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.Kind != plugin.ResultMedia || result.MIME != "audio/mpeg" || string(result.Media) != "ID3" {
		t.Fatalf("结果 = %+v", result)
	}
	if result.Usage.TTSChars != 5 {
		t.Fatalf("用量 = %+v，期望 tts_characters 是 5", result.Usage)
	}
	// hook 方言同样能承载流式解码，宿主按 StreamDialect 判。
	if _, ok := d.(plugin.StreamDialect); !ok {
		t.Fatal("hook 方言应该满足 StreamDialect")
	}
}

// declarativeDecl 造一条能编过的声明式方言，只给登记与冲突用例用。
func declarativeDecl(id string) plugin.DialectDecl {
	return plugin.DialectDecl{
		ID:             id,
		Operation:      plugin.OpSpeechSynthesize,
		UpstreamWire:   plugin.WireSpeech,
		Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplDeclarative, Decode: plugin.ImplDeclarative},
	}
}

// TestRegisterRejectsHostDialectPrefix 钉住 lax/ 前缀不能冒用：那是内置方言的命名空间。
func TestRegisterRejectsHostDialectPrefix(t *testing.T) {
	p := plugin.Plugin{
		Manifest: plugin.Manifest{ID: "com.example.hook", Runtime: plugin.RuntimeMoejs, Entry: "./index.js"},
		Dialects: []plugin.DialectDecl{{
			ID:             "lax/openai-speech@1",
			Operation:      plugin.OpSpeechSynthesize,
			UpstreamWire:   plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplHook, Decode: plugin.ImplHook},
		}},
	}
	err := p.Register(plugin.DefaultRegistry(), plugin.SandboxOptions{Trust: plugin.TrustSandboxed})
	if err == nil || !strings.Contains(err.Error(), "lax/") || !strings.Contains(err.Error(), "不能冒用") {
		t.Fatalf("冒用 lax/ 的错误 = %v", err)
	}
}

// TestRegisterHookNeedsMoejs 钉住实现方式与运行时配套：声明式运行时没有地方跑 hook。
func TestRegisterHookNeedsMoejs(t *testing.T) {
	p := plugin.Plugin{
		Manifest: plugin.Manifest{ID: "com.example.hook", Runtime: plugin.RuntimeDeclarative},
		Dialects: []plugin.DialectDecl{{
			ID:             "com.example.hook/speech@1",
			Operation:      plugin.OpSpeechSynthesize,
			UpstreamWire:   plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplHook, Decode: plugin.ImplHook},
		}},
	}
	err := p.Register(plugin.DefaultRegistry(), plugin.SandboxOptions{})
	if err == nil || !strings.Contains(err.Error(), "runtime") || !strings.Contains(err.Error(), "moejs") {
		t.Fatalf("声明式运行时配 hook 方言的错误 = %v", err)
	}
}

// TestRegisterMixedImplementationRejected 钉住一半声明式一半 hook 没有执行路径。
func TestRegisterMixedImplementationRejected(t *testing.T) {
	p := plugin.Plugin{
		Manifest: plugin.Manifest{ID: "com.example.hook", Runtime: plugin.RuntimeMoejs, Entry: "./index.js"},
		Dialects: []plugin.DialectDecl{{
			ID:             "com.example.hook/speech@1",
			Operation:      plugin.OpSpeechSynthesize,
			UpstreamWire:   plugin.WireSpeech,
			Implementation: plugin.ImplementationDecl{Prepare: plugin.ImplHook, Decode: plugin.ImplDeclarative},
		}},
	}
	err := p.Register(plugin.DefaultRegistry(), plugin.SandboxOptions{Trust: plugin.TrustSandboxed})
	if err == nil || !strings.Contains(err.Error(), "要都写") {
		t.Fatalf("混写实现的错误 = %v", err)
	}
}

// TestRegisterRollsBackWholePackage 钉住整包语义：一条方言撞车时，同包别的方言也不能留下。
func TestRegisterRollsBackWholePackage(t *testing.T) {
	first, second := declarativeDecl("com.example.dup/a@1"), declarativeDecl("com.example.dup/b@1")
	taken, err := plugin.CompileDialect(second)
	if err != nil {
		t.Fatalf("编方言失败: %v", err)
	}

	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPluginDialect("com.example.dup", taken); err != nil {
		t.Fatalf("预登记方言失败: %v", err)
	}

	p := plugin.Plugin{
		Manifest: plugin.Manifest{ID: "com.example.dup", Runtime: plugin.RuntimeDeclarative},
		Dialects: []plugin.DialectDecl{first, second},
	}
	err = p.Register(reg, plugin.SandboxOptions{})
	if err == nil || !strings.Contains(err.Error(), "com.example.dup/b@1") {
		t.Fatalf("撞车的错误 = %v", err)
	}
	if _, exists := reg.Dialect("com.example.dup/a@1"); exists {
		t.Fatal("整包登记失败，但同包的 a@1 留在了注册表里")
	}
}
