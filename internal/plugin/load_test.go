package plugin_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/plugin"
)

const echoDir = "testdata/plugin-echo"

func TestLoadDir(t *testing.T) {
	p, err := plugin.LoadDir(echoDir)
	if err != nil {
		t.Fatalf("加载插件目录失败: %v", err)
	}
	if p.Manifest.ID != "com.example.echo" {
		t.Fatalf("插件 ID = %q", p.Manifest.ID)
	}
	if len(p.Dialects) != 1 || p.Dialects[0].ID != "com.example.echo/speech@1" {
		t.Fatalf("方言 = %+v", p.Dialects)
	}
	catalog := p.Catalogs["echo"]
	if len(catalog.Models) != 1 || catalog.Models[0].UpstreamID != "echo-tts" {
		t.Fatalf("目录 = %+v", catalog)
	}
	if got := p.DialectsByProvider["echo"]; len(got) != 1 || got[0] != "com.example.echo/speech@1" {
		t.Fatalf("provider 的方言 = %v", got)
	}
}

// TestLoadProviderWithDeclaredRoutes 钉住 manifest 的 routes：声明了就原样留在 Manifest 里，
// 且每条方言的 upstream_wire 都能在其中找到一条；漏一条时安装当场报错（否则装上也只能 400）。
func TestLoadProviderWithDeclaredRoutes(t *testing.T) {
	p, err := plugin.LoadDir(fixture("plugin-voice-routes"))
	if err != nil {
		t.Fatalf("加载带 routes 的插件：%v", err)
	}
	routes := p.Manifest.Providers[0].Routes
	if len(routes) != 2 {
		t.Fatalf("routes = %+v，期望两条", routes)
	}
	if routes[0].Protocol != plugin.WireSpeech || routes[0].Path != "/api/v3/tts/unidirectional" {
		t.Fatalf("routes[0] = %+v", routes[0])
	}
	if routes[1].Protocol != plugin.WireTranscriptions {
		t.Fatalf("routes[1] = %+v", routes[1])
	}
	header, scheme, err := p.Manifest.Providers[0].NormalizedAuth()
	if err != nil {
		t.Fatalf("规整凭据头：%v", err)
	}
	if header != "x-api-key" || scheme != plugin.AuthSchemeRaw {
		t.Fatalf("凭据头 = %q/%q", header, scheme)
	}

	_, err = plugin.LoadDir(fixture("plugin-voice-routes-missing"))
	requireContains(t, err, "routes 里却没有这条协议", "openai-transcriptions")
}

func TestRegisterPluginDialect(t *testing.T) {
	p, err := plugin.LoadDir(echoDir)
	if err != nil {
		t.Fatalf("加载插件目录失败: %v", err)
	}
	r := plugin.DefaultRegistry()
	if err := r.RegisterPlugin(p); err != nil {
		t.Fatalf("登记插件方言失败: %v", err)
	}
	d, ok := r.Dialect("com.example.echo/speech@1")
	if !ok {
		t.Fatal("登记后按 ID 取不到方言")
	}
	if d.Operation() != plugin.OpSpeechSynthesize || d.UpstreamWire() != plugin.WireSpeech {
		t.Fatalf("方言 operation/wire = %s/%s", d.Operation(), d.UpstreamWire())
	}

	// 声明式方言能真的编出请求与解出结果。
	plan, err := d.Prepare(plugin.Input{Model: "echo-tts", Text: "你好", Format: "mp3"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	if plan.JSON["model"] != "echo-tts" || plan.JSON["input"] != "你好" || plan.JSON["voice"] != "alloy" {
		t.Fatalf("请求体 = %+v", plan.JSON)
	}
	if plan.JSON["response_format"] != "mp3" {
		t.Fatalf("response_format = %v", plan.JSON["response_format"])
	}

	result, err := d.Decode(plugin.Input{Format: "mp3"}, plugin.Response{Status: 200, Body: []byte("ID3")})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.Kind != plugin.ResultMedia || result.MIME != "audio/mpeg" || string(result.Media) != "ID3" {
		t.Fatalf("结果 = %+v", result)
	}
}

func TestRegisterPluginRejectsSecondLoad(t *testing.T) {
	p, err := plugin.LoadDir(echoDir)
	if err != nil {
		t.Fatalf("加载插件目录失败: %v", err)
	}
	r := plugin.DefaultRegistry()
	if err := r.RegisterPlugin(p); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	if err := r.RegisterPlugin(p); err == nil {
		t.Fatal("同一个方言登记两次应当报错")
	}
}

func TestLoadDirRejectsMissingManifest(t *testing.T) {
	_, err := plugin.LoadDir(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "lax.plugin.json") {
		t.Fatalf("缺 manifest 的错误 = %v", err)
	}
}

func TestPluginDirHash(t *testing.T) {
	first, err := plugin.PluginDirHash(echoDir)
	if err != nil {
		t.Fatalf("算目录哈希失败: %v", err)
	}
	second, err := plugin.PluginDirHash(echoDir)
	if err != nil {
		t.Fatalf("算目录哈希失败: %v", err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("目录哈希不稳定：%q / %q", first, second)
	}
	other, err := plugin.PluginDirHash(filepath.Join(echoDir, "..", "plugin-echo"))
	if err != nil || other != first {
		t.Fatalf("同一目录换个写法哈希就变了：%q / %q（%v）", other, first, err)
	}
	if plugin.ManifestHash([]byte("a")) == plugin.ManifestHash([]byte("b")) {
		t.Fatal("manifest 哈希对内容不敏感")
	}
}

// TestChatWireDeclarativeDialect 走完整链路：插件声明一条 chat 线协议的 TTS 方言，
// 加载、登记、编请求、解响应四步都由声明式映射完成，不需要任何 Go 代码。
func TestChatWireDeclarativeDialect(t *testing.T) {
	p, err := plugin.LoadDir("testdata/plugin-chatvoice")
	if err != nil {
		t.Fatalf("加载插件目录失败: %v", err)
	}
	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPlugin(p); err != nil {
		t.Fatalf("登记插件方言失败: %v", err)
	}
	d, ok := reg.Dialect("com.example.chatvoice/speech@1")
	if !ok {
		t.Fatal("登记后取不到方言")
	}
	if d.UpstreamWire() != plugin.WireOpenAIChat {
		t.Fatalf("upstream_wire = %q，期望 openai-chat", d.UpstreamWire())
	}

	plan, err := d.Prepare(plugin.Input{Model: "chatvoice-tts", Text: "你好", Format: "wav", Voice: "alloy"})
	if err != nil {
		t.Fatalf("编请求失败: %v", err)
	}
	messages, _ := plan.JSON["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %+v", plan.JSON["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "assistant" || first["content"] != "你好" {
		t.Fatalf("messages[0] = %+v", first)
	}
	audio, _ := plan.JSON["audio"].(map[string]any)
	if audio["format"] != "wav" || audio["voice"] != "alloy" {
		t.Fatalf("audio = %+v", audio)
	}

	body := []byte(`{"choices":[{"message":{"audio":{"data":"` +
		base64.StdEncoding.EncodeToString([]byte("RIFF-audio")) + `"}}}],` +
		`"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11,` +
		`"prompt_tokens_details":{"cached_tokens":4}}}`)
	result, err := d.Decode(plugin.Input{Format: "wav"}, plugin.Response{Status: 200, Body: body})
	if err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if result.Kind != plugin.ResultMedia || result.MIME != "audio/wav" || string(result.Media) != "RIFF-audio" {
		t.Fatalf("结果 = %+v", result)
	}
	if result.Usage.In != 9 || result.Usage.Out != 2 || result.Usage.CacheRead != 4 || result.Usage.Total != 11 {
		t.Fatalf("用量 = %+v", result.Usage)
	}
}

// TestLoadChatProviderWithHostDialect 钉住「对话上游可以用插件装」：
// 目录直接引用宿主方言 lax/openai-chat@1，插件不带 dialects 文件，加载与登记都不报错。
func TestLoadChatProviderWithHostDialect(t *testing.T) {
	p, err := plugin.LoadDir("testdata/plugin-river")
	if err != nil {
		t.Fatalf("加载插件目录失败: %v", err)
	}
	if len(p.Dialects) != 0 {
		t.Fatalf("对话插件不该带方言文件：%+v", p.Dialects)
	}
	model := p.Catalogs["river"].Models[0]
	if model.Operations[plugin.OpChatComplete].Dialect != plugin.DialectHostOpenAIChat {
		t.Fatalf("绑定 = %q，期望宿主方言", model.Operations[plugin.OpChatComplete].Dialect)
	}
	reg := plugin.DefaultRegistry()
	if err := reg.RegisterPlugin(p); err != nil {
		t.Fatalf("登记插件失败: %v", err)
	}
	if _, ok := reg.Dialect(plugin.DialectHostOpenAIChat); !ok {
		t.Fatal("宿主方言不该被插件覆盖掉")
	}
}

// TestLoadRejectsUnknownHostDialect 钉住目录引用不存在的 lax/ 方言时报错并列出可用的。
func TestLoadRejectsUnknownHostDialect(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"lax.plugin.json": `{"schema":"lax.plugin/v1","id":"com.example.bad","name":"Bad","version":"1.0.0",
			"kind":"provider","runtime":"declarative","abi":"lax.plugin/v1","requires":["operation:chat.complete@1"],
			"permissions":{"network":{"mode":"none"},"secrets":{"mode":"host-inject"}},
			"providers":[{"id":"bad","base_url":"https://api.bad.test","catalog":"./models.json"}]}`,
		"models.json": `{"models":[{"model":"m","upstream_id":"m","operations":{"chat.complete@1":{"dialect":"lax/nope@1"}}}]}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	_, err := plugin.LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "lax/nope@1") {
		t.Fatalf("未知宿主方言的错误 = %v", err)
	}
}
