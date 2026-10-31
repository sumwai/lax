package plugin_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/plugin"
)

// manifestName 是用例里的文件标签：报错要带上它，命令行才能指出是哪个文件坏了。
const manifestName = "lax.plugin.json"

func fixture(name string) string { return filepath.Join("testdata", name) }

// requireContains 确认 err 非空且文案里出现每一段要找的文字。
func requireContains(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望报错，实际通过了；要找的是 %s", strings.Join(parts, " / "))
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("错误信息 %q 里没有 %q", err, part)
		}
	}
}

// at 按点号路径取嵌套对象，用例只改坏一处。
func at(t *testing.T, doc map[string]any, dotted string) map[string]any {
	t.Helper()
	cur := doc
	for _, key := range strings.Split(dotted, ".") {
		next, ok := cur[key].(map[string]any)
		if !ok {
			t.Fatalf("%s 不是对象", dotted)
		}
		cur = next
	}
	return cur
}

// providerAt 取 providers 里第 i 家的对象。
func providerAt(t *testing.T, doc map[string]any, i int) map[string]any {
	t.Helper()
	list, ok := doc["providers"].([]any)
	if !ok || i >= len(list) {
		t.Fatalf("providers 里没有第 %d 家", i)
	}
	provider, ok := list[i].(map[string]any)
	if !ok {
		t.Fatalf("providers[%d] 不是对象", i)
	}
	return provider
}

// baseManifestDoc 是能通过校验的声明式 provider，用例只改坏一处。
func baseManifestDoc() map[string]any {
	return map[string]any{
		"schema":  "lax.plugin/v1",
		"id":      "com.example.river",
		"name":    "River",
		"version": "1.0.0",
		"kind":    "provider",
		"runtime": "declarative",
		"abi":     "lax.plugin/v1",
		"requires": []any{
			"operation:chat.complete@1",
			"runtime:declarative",
			"permission:network",
		},
		"permissions": map[string]any{
			"network": map[string]any{
				"mode":      "host-proxy",
				"schemes":   []any{"https"},
				"hosts":     []any{"api.river.ai"},
				"redirects": "none",
			},
			"secrets":    map[string]any{"mode": "host-inject", "account": []any{"api_key"}},
			"filesystem": "none",
			"process":    "none",
			"env":        "none",
		},
		"providers": []any{map[string]any{
			"id":       "river",
			"name":     "River",
			"base_url": "https://api.river.ai/v1",
			"catalog":  "./models.json",
			"dialects": "./dialects.json",
		}},
		"options": map[string]any{
			"region": map[string]any{"type": "string", "enum": []any{"SGP", "RU"}, "default": "SGP"},
		},
	}
}

// TestLoadManifestOK 读一份完整的声明式 provider：字段按 JSON 落位，extensions 原样保留。
func TestLoadManifestOK(t *testing.T) {
	m, err := plugin.LoadManifest(fixture("manifest-ok.json"))
	if err != nil {
		t.Fatalf("读 manifest-ok.json: %v", err)
	}
	if m.ID != "com.example.river" || m.Kind != plugin.KindProvider || m.Runtime != plugin.RuntimeDeclarative {
		t.Fatalf("清单字段 = %+v，期望 river/provider/declarative", m)
	}
	if m.Entry != "" {
		t.Fatalf("entry = %q，声明式插件应没有入口文件", m.Entry)
	}
	if len(m.Requires) != 4 || m.Requires[0] != "operation:chat.complete@1" {
		t.Fatalf("requires = %v，期望四条且首条是 chat.complete@1", m.Requires)
	}
	net := m.Permissions.Network
	if net.Mode != "host-proxy" || strings.Join(net.Hosts, ",") != "api.river.ai,cdn.river.ai" {
		t.Fatalf("network = %+v，期望 host-proxy 与两个 host", net)
	}
	if net.Redirects != "same-host" || net.MaxControlCalls != 4 {
		t.Fatalf("network = %+v，redirects 与调用上限没落位", net)
	}
	if len(m.Providers) != 1 || m.Providers[0].ID != "river" || m.Providers[0].BaseURL != "https://api.river.ai/v1" {
		t.Fatalf("providers = %+v，期望一家 river", m.Providers)
	}
	if m.Providers[0].Catalog != "./models.json" || m.Providers[0].Dialects != "./dialects.json" {
		t.Fatalf("providers[0] = %+v，catalog/dialects 路径没落位", m.Providers[0])
	}
	if len(m.Options) != 3 {
		t.Fatalf("options 有 %d 项，期望 3 项", len(m.Options))
	}
	if got := string(m.Options["timeout"].Default); got != "30" {
		t.Fatalf("options.timeout.default = %s，期望 30", got)
	}
	if _, ok := m.Extensions["x-homepage"]; !ok {
		t.Fatalf("extensions = %v，期望保留 x-homepage", m.Extensions)
	}
}

// TestLoadManifestMoejs 钉住带代码的插件能通过 manifest 校验：moejs 要入口，其余规则不变。
func TestLoadManifestMoejs(t *testing.T) {
	m, err := plugin.LoadManifest(fixture("manifest-moejs.json"))
	if err != nil {
		t.Fatalf("读 manifest-moejs.json: %v", err)
	}
	if m.Runtime != plugin.RuntimeMoejs || m.Entry != "./index.js" {
		t.Fatalf("运行时与入口 = %q / %q", m.Runtime, m.Entry)
	}
	if m.Kind != plugin.KindProvider || m.Permissions.Filesystem != "none" {
		t.Fatalf("类型与权限 = %q / %q", m.Kind, m.Permissions.Filesystem)
	}
}

// TestLoadManifestFixturesReject 用 testdata 跑反例：文案要带文件路径，指出下一版该怎么改。
func TestLoadManifestFixturesReject(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"manifest-unknown-field.json", []string{"未知字段", "trust", "extensions"}},
		{"manifest-entry-escape.json", []string{"逃出插件根"}},
		{"manifest-host-star.json", []string{"permissions.network.hosts[1]", "裸通配符"}},
		{"manifest-bad-abi.json", []string{"abi 是", `宿主支持的是 "lax.plugin/v1"`}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			_, err := plugin.LoadManifest(fixture(c.file))
			requireContains(t, err, append([]string{fixture(c.file)}, c.want...)...)
		})
	}
}

// TestDecodeManifestRejects 逐条改坏一份合法清单，确认拦在字段路径上。
func TestDecodeManifestRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, doc map[string]any)
		want   []string
	}{
		{
			name:   "schema 不匹配",
			mutate: func(t *testing.T, doc map[string]any) { doc["schema"] = "lax.plugin/v2" },
			want:   []string{"schema 是", "lax.plugin/v1"},
		},
		{
			name:   "abi 主版本不匹配",
			mutate: func(t *testing.T, doc map[string]any) { doc["abi"] = "lax.plugin/v2" },
			want:   []string{"abi 是", `宿主支持的是 "lax.plugin/v1"`},
		},
		{
			name:   "顶层未知字段",
			mutate: func(t *testing.T, doc map[string]any) { doc["trust"] = "sandboxed" },
			want:   []string{"未知字段", "trust", "extensions"},
		},
		{
			name:   "id 为空",
			mutate: func(t *testing.T, doc map[string]any) { doc["id"] = "" },
			want:   []string{"id 为空"},
		},
		{
			name:   "id 带斜杠",
			mutate: func(t *testing.T, doc map[string]any) { doc["id"] = "com.example/river" },
			want:   []string{"不能有斜杠"},
		},
		{
			name:   "id 有大写",
			mutate: func(t *testing.T, doc map[string]any) { doc["id"] = "com.Example.river" },
			want:   []string{"只能用 a-z0-9._-"},
		},
		{
			name:   "id 过长",
			mutate: func(t *testing.T, doc map[string]any) { doc["id"] = strings.Repeat("a", 129) },
			want:   []string{"上限 128"},
		},
		{
			name:   "id 首尾是点",
			mutate: func(t *testing.T, doc map[string]any) { doc["id"] = ".river" },
			want:   []string{"首尾不能用"},
		},
		{
			name:   "version 只有两段",
			mutate: func(t *testing.T, doc map[string]any) { doc["version"] = "1.0" },
			want:   []string{"version 是"},
		},
		{
			name:   "kind 未知",
			mutate: func(t *testing.T, doc map[string]any) { doc["kind"] = "middleware" },
			want:   []string{"kind 是", "provider、transform 或 auth"},
		},
		{
			name:   "runtime 未知",
			mutate: func(t *testing.T, doc map[string]any) { doc["runtime"] = "wasm" },
			want:   []string{"runtime 是", "declarative 或 moejs"},
		},
		{
			name: "moejs 缺 entry",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["runtime"] = "moejs"
			},
			want: []string{"entry 为空", "moejs"},
		},
		{
			name: "moejs 的 kind 不是 provider",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["runtime"] = "moejs"
				doc["entry"] = "./index.js"
				doc["kind"] = "transform"
			},
			want: []string{"kind 是", "只能是 provider"},
		},
		{
			name: "moejs 带文件权限",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["runtime"] = "moejs"
				doc["entry"] = "./index.js"
				at(t, doc, "permissions")["filesystem"] = "read"
			},
			want: []string{"permissions.filesystem", "只能写"},
		},
		{
			name:   "声明式却写了 entry",
			mutate: func(t *testing.T, doc map[string]any) { doc["entry"] = "./index.js" },
			want:   []string{"没有入口文件", "删掉 entry"},
		},
		{
			name: "entry 逃出插件根",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["runtime"] = "moejs"
				doc["entry"] = "../../etc/evil.js"
			},
			want: []string{"逃出插件根"},
		},
		{
			name: "entry 是绝对路径",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["runtime"] = "moejs"
				doc["entry"] = "/etc/passwd"
			},
			want: []string{"相对路径"},
		},
		{
			name:   "requires 缺冒号",
			mutate: func(t *testing.T, doc map[string]any) { doc["requires"] = []any{"chat.complete@1"} },
			want:   []string{"requires[0]", "<类>:<名>"},
		},
		{
			name:   "requires 未知操作",
			mutate: func(t *testing.T, doc map[string]any) { doc["requires"] = []any{"operation:speech.translate@1"} },
			want:   []string{"requires[0] 要求", "宿主不认识", "speech.transcribe@1"},
		},
		{
			name:   "requires 未知类",
			mutate: func(t *testing.T, doc map[string]any) { doc["requires"] = []any{"module:fetch"} },
			want:   []string{"的类", "module", "operation、runtime、permission"},
		},
		{
			name:   "requires 的 runtime 未知",
			mutate: func(t *testing.T, doc map[string]any) { doc["requires"] = []any{"runtime:deno"} },
			want:   []string{"runtime \"deno\" 不认识", "declarative 或 moejs"},
		},
		{
			name:   "requires 未知权限",
			mutate: func(t *testing.T, doc map[string]any) { doc["requires"] = []any{"permission:audio"} },
			want:   []string{"的权限", "network、secrets"},
		},
		{
			name:   "filesystem 不是 none",
			mutate: func(t *testing.T, doc map[string]any) { at(t, doc, "permissions")["filesystem"] = "read" },
			want:   []string{"permissions.filesystem", "只能写"},
		},
		{
			name:   "process 不是 none",
			mutate: func(t *testing.T, doc map[string]any) { at(t, doc, "permissions")["process"] = "spawn" },
			want:   []string{"permissions.process"},
		},
		{
			name:   "env 不是 none",
			mutate: func(t *testing.T, doc map[string]any) { at(t, doc, "permissions")["env"] = "read" },
			want:   []string{"permissions.env"},
		},
		{
			name:   "network mode 未知",
			mutate: func(t *testing.T, doc map[string]any) { at(t, doc, "permissions.network")["mode"] = "direct" },
			want:   []string{"permissions.network.mode", "host-proxy"},
		},
		{
			name: "host-proxy 却没有 hosts",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["hosts"] = []any{}
			},
			want: []string{"hosts 却为空"},
		},
		{
			name: "hosts 裸通配符",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["hosts"] = []any{"*"}
			},
			want: []string{"permissions.network.hosts[0]", "裸通配符"},
		},
		{
			name: "host 带 scheme",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["hosts"] = []any{"https://api.river.ai"}
			},
			want: []string{"不带 scheme"},
		},
		{
			name: "host 带路径",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["hosts"] = []any{"api.river.ai/v1"}
			},
			want: []string{"只写主机名"},
		},
		{
			name: "host 带空白",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["hosts"] = []any{"api river.ai"}
			},
			want: []string{"不能有空白"},
		},
		{
			name: "schemes 不是 https",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["schemes"] = []any{"http"}
			},
			want: []string{"permissions.network.schemes[0]", "控制面只允许 https"},
		},
		{
			name: "redirects 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.network")["redirects"] = "follow"
			},
			want: []string{"redirects 是", "same-host"},
		},
		{
			name: "secrets mode 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.secrets")["mode"] = "reveal"
			},
			want: []string{"secrets.mode", "host-inject"},
		},
		{
			name: "secrets account 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "permissions.secrets")["account"] = []any{"oauth"}
			},
			want: []string{"secrets.account[0]", "api_key 与 bearer"},
		},
		{
			name:   "provider 插件没有 providers",
			mutate: func(t *testing.T, doc map[string]any) { doc["providers"] = []any{} },
			want:   []string{"providers 为空"},
		},
		{
			name:   "provider id 为空",
			mutate: func(t *testing.T, doc map[string]any) { providerAt(t, doc, 0)["id"] = "" },
			want:   []string{"providers[0].id 为空", "provider/model"},
		},
		{
			name:   "provider id 带斜杠",
			mutate: func(t *testing.T, doc map[string]any) { providerAt(t, doc, 0)["id"] = "river/ai" },
			want:   []string{"providers[0].id", "不能有斜杠"},
		},
		{
			name: "provider id 重复",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["providers"] = append(doc["providers"].([]any), map[string]any{"id": "river"})
			},
			want: []string{"providers[1].id 与 providers[0].id", "provider ID 在 lax 内唯一"},
		},
		{
			name: "catalog 逃出插件根",
			mutate: func(t *testing.T, doc map[string]any) {
				providerAt(t, doc, 0)["catalog"] = "../../models.json"
			},
			want: []string{"providers[0].catalog", "逃出插件根"},
		},
		{
			name: "dialects 是绝对路径",
			mutate: func(t *testing.T, doc map[string]any) {
				providerAt(t, doc, 0)["dialects"] = "/etc/dialects.json"
			},
			want: []string{"providers[0].dialects", "相对路径"},
		},
		{
			name:   "base_url 是 http",
			mutate: func(t *testing.T, doc map[string]any) { providerAt(t, doc, 0)["base_url"] = "http://api.river.ai" },
			want:   []string{"base_url", "只允许 https"},
		},
		{
			name:   "base_url 缺 scheme",
			mutate: func(t *testing.T, doc map[string]any) { providerAt(t, doc, 0)["base_url"] = "api.river.ai" },
			want:   []string{"只允许 https"},
		},
		{
			name: "base_url 带 userinfo",
			mutate: func(t *testing.T, doc map[string]any) {
				providerAt(t, doc, 0)["base_url"] = "https://user:pass@api.river.ai"
			},
			want: []string{"不能带 userinfo"},
		},
		{
			name: "base_url 带 fragment",
			mutate: func(t *testing.T, doc map[string]any) {
				providerAt(t, doc, 0)["base_url"] = "https://api.river.ai/v1#frag"
			},
			want: []string{"不能带 fragment"},
		},
		{
			name: "option type 未知",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "options.region")["type"] = "array"
			},
			want: []string{"options.region.type", "string、number 或 boolean"},
		},
		{
			name: "enum 配在 number 上",
			mutate: func(t *testing.T, doc map[string]any) {
				doc["options"] = map[string]any{"timeout": map[string]any{
					"type": "number", "enum": []any{"1"}, "default": 1,
				}}
			},
			want: []string{"枚举只配 string"},
		},
		{
			name: "default 与 type 不符",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "options.region")["default"] = 1
			},
			want: []string{"options.region.default", "与 type string 不符"},
		},
		{
			name: "default 不在 enum 里",
			mutate: func(t *testing.T, doc map[string]any) {
				at(t, doc, "options.region")["default"] = "EU"
			},
			want: []string{"不在 enum 里"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := baseManifestDoc()
			c.mutate(t, doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("序列化用例: %v", err)
			}
			_, err = plugin.DecodeManifest(manifestName, data)
			requireContains(t, err, append([]string{manifestName}, c.want...)...)
		})
	}
}

// TestDecodeManifestRejectsBrokenJSON 不是规则问题，而是文件本身读不成 manifest。
func TestDecodeManifestRejectsBrokenJSON(t *testing.T) {
	cases := []struct {
		name string
		data string
		want []string
	}{
		{"语法错误", `{"schema":,}`, []string{"JSON 语法错误"}},
		{"文件被截断", `{"schema":`, []string{"JSON 不完整"}},
		{"值之后还有内容", `{"schema":"lax.plugin/v1"} {"schema":"lax.plugin/v1"}`, []string{"之后还有内容"}},
		{"字段类型不对", `{"schema": 1}`, []string{"schema 类型不对"}},
		{"顶层是数组", `[]`, []string{manifestName}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := plugin.DecodeManifest(manifestName, []byte(c.data))
			requireContains(t, err, append([]string{manifestName}, c.want...)...)
		})
	}
}

// TestValidateManifestTransformWithoutProviders kind 是 transform 时不要求 providers。
func TestValidateManifestTransformWithoutProviders(t *testing.T) {
	m := plugin.Manifest{
		Schema:  plugin.ManifestSchema,
		ID:      "com.example.tidy",
		Version: "0.1.0",
		Kind:    plugin.KindTransform,
		Runtime: plugin.RuntimeDeclarative,
		ABI:     plugin.ABIv1,
	}
	if err := plugin.ValidateManifest(m); err != nil {
		t.Fatalf("没有 providers 的 transform 应通过，实际 %v", err)
	}
}
