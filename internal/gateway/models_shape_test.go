package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
)

// syncBody 用一个固定返回 body 的假上游跑一次 SyncModels，同时收集网关日志。
// 凭据给一个容易在输出里认出来的值，调用方用它断言日志与错误都不含明文。
func syncBody(t *testing.T, body, secretValue string) (models []string, syncErr error, logs string) {
	t.Helper()
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)

	acc := f.add(t, "shape", gateway.ProtocolChat, up.URL, secretValue, nil, 100)
	gs, collected := newGatewayWith(t, f, gateway.Options{})
	models, syncErr = gs.SyncModels(context.Background(), acc)
	return models, syncErr, collected()
}

// mustNotLeak 断言凭据明文既没进日志、也没进错误文本、也没进序列化后的模型列表。
// 凭据只在内存里活到构造上游请求为止，模型同步这条路径也不例外。
func mustNotLeak(t *testing.T, key string, models []string, syncErr error, logs string) {
	t.Helper()
	if strings.Contains(logs, key) {
		t.Fatalf("日志泄漏了凭据：\n%s", logs)
	}
	if syncErr != nil && strings.Contains(syncErr.Error(), key) {
		t.Fatalf("错误文本泄漏了凭据: %v", syncErr)
	}
	b, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("序列化模型列表: %v", err)
	}
	if strings.Contains(string(b), key) {
		t.Fatalf("序列化输出泄漏了凭据: %s", b)
	}
}

const shapeKey = "sk-shape-plaintext-7f3a"

// TestSyncModelsShapes 三种已知形状各跑一遍：`{data:[…]}`、`{models:[…]}`、顶层数组。
// 条目按 id → name → model 取第一个非空名字，去空白后排序去重。
func TestSyncModelsShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			// OpenAI 兼容：主流形状，object=list 这种多余字段不影响。
			name: "data 里的 id 与 name",
			body: `{"object":"list","data":[{"id":"m-b"},{"name":"m-a"},{"id":"  "},{"name":" m-a "}]}`,
			want: "m-a,m-b",
		},
		{
			// ollama 的 /api/tags：只有 models，条目的名字在 name 上。
			name: "models 里的 name",
			body: `{"models":[{"name":"llama3:latest","model":"llama3","modified_at":"2025-01-01T00:00:00Z"},{"name":" qwen2.5 "}]}`,
			want: "llama3:latest,qwen2.5",
		},
		{
			name: "顶层字符串数组",
			body: `["m2"," m1 ","","m2"]`,
			want: "m1,m2",
		},
		{
			name: "顶层对象数组",
			body: `[{"id":"m3"},{"name":"m0"},{"model":"m1"}]`,
			want: "m0,m1,m3",
		},
		{
			// 三个名字字段同时出现时按 id → name → model 取第一个非空值。
			name: "id 到 name 到 model 的取值顺序",
			body: `{"models":[{"id":"m-id","name":"m-name"},{"model":"m-model"},{"name":"m-name2","model":"m-model2"}]}`,
			want: "m-id,m-model,m-name2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			models, err, logs := syncBody(t, c.body, shapeKey)
			if err != nil {
				t.Fatalf("同步模型: %v", err)
			}
			if got := strings.Join(models, ","); got != c.want {
				t.Fatalf("模型 = %q，期望 %q", got, c.want)
			}
			mustNotLeak(t, shapeKey, models, err, logs)
		})
	}
}

// TestSyncModelsShapePriority 确认 data 与 models 同时存在时认 data：形状按固定顺序试，
// 不按字段长度、不按内容多寡挑。
func TestSyncModelsShapePriority(t *testing.T) {
	models, err, logs := syncBody(t,
		`{"data":[{"id":"from-data"}],"models":[{"name":"from-models"}]}`, shapeKey)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	if got, want := strings.Join(models, ","), "from-data"; got != want {
		t.Fatalf("模型 = %q，期望 %q（data 优先于 models）", got, want)
	}
	mustNotLeak(t, shapeKey, models, err, logs)
}

// TestSyncModelsEmptyListShapes 确认字段存在但为空就是「上游返回空列表」的失败，
// 不因为第一种形状为空就继续试下一种：空列表与形状不对是两件事。
func TestSyncModelsEmptyListShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"data 为空、models 有内容也不接着试", `{"data":[],"models":[{"name":"m1"}]}`},
		{"只有 models 且为空", `{"models":[]}`},
		{"data 是 null 也算空列表", `{"data":null,"models":[{"name":"m1"}]}`},
		{"顶层空数组", `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			models, err, logs := syncBody(t, c.body, shapeKey)
			if err == nil {
				t.Fatalf("空列表应算错误，得到 %v", models)
			}
			if len(models) != 0 {
				t.Fatalf("出错时不应返回模型: %v", models)
			}
			msg := err.Error()
			if !strings.Contains(msg, "上游没有返回模型") || !strings.Contains(msg, "base-url") {
				t.Fatalf("错误应说明上游返回空列表并提示检查 base-url: %v", err)
			}
			if strings.Contains(msg, "m1") {
				t.Fatalf("第一种形状为空时不该用后面的形状兜底: %v", err)
			}
			mustNotLeak(t, shapeKey, models, err, logs)
		})
	}
}

// TestSyncModelsSkipsEntriesWithoutName 确认条目缺所有名字字段、或名字全是空白时整条跳过，
// 其余条目照常进列表。
func TestSyncModelsSkipsEntriesWithoutName(t *testing.T) {
	models, err, logs := syncBody(t,
		`{"data":[{"object":"model"},{"id":"m1"},{"name":"   "},{"model":""},{"id":"m2"},{"id":7}]}`, shapeKey)
	if err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	if got, want := strings.Join(models, ","), "m1,m2"; got != want {
		t.Fatalf("模型 = %q，期望 %q（缺名字的条目跳过）", got, want)
	}
	mustNotLeak(t, shapeKey, models, err, logs)
}

// TestSyncModelsUnknownShape 确认三种形状都不匹配时报错，文案列出三种形状，便于对着上游排查。
func TestSyncModelsUnknownShape(t *testing.T) {
	cases := []struct{ name, body string }{
		{"只有陌生字段", `{"ok":true}`},
		{"顶层不是对象也不是数组", `"done"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			models, err, logs := syncBody(t, c.body, shapeKey)
			if err == nil {
				t.Fatalf("形状不认识时应报错，得到 %v", models)
			}
			msg := err.Error()
			for _, want := range []string{`{"data":[{"id":"…"}]}`, `{"models":[{"name":"…"}]}`, `顶层数组 ["m1","m2"]`} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误文案应列出三种形状，缺少 %q: %v", want, err)
				}
			}
			if !strings.Contains(msg, "base-url") || !strings.Contains(msg, "--models") {
				t.Fatalf("错误应提示检查 base-url 与 --models 手填: %v", err)
			}
			mustNotLeak(t, shapeKey, models, err, logs)
		})
	}
}
