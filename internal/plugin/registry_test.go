package plugin_test

import (
	"slices"
	"testing"

	"lax/internal/plugin"
)

func TestDefaultRegistry(t *testing.T) {
	r := plugin.DefaultRegistry()
	want := []string{
		plugin.DialectHostAnthropic,
		plugin.DialectHostEmbeddings,
		plugin.DialectHostGemini,
		plugin.DialectHostOpenAIChat,
		plugin.DialectHostOpenAIResponses,
		plugin.DialectHostSystemOne,
		plugin.DialectMiMoChatASR,
		plugin.DialectMiMoChatSpeech,
		plugin.DialectOpenAISpeech,
		plugin.DialectOpenAITranscriptions,
	}
	slices.Sort(want)
	if got := r.IDs(); !slices.Equal(got, want) {
		t.Fatalf("内置方言 = %v，期望 %v", got, want)
	}
	if _, ok := r.Dialect(plugin.DialectMiMoChatSpeech); !ok {
		t.Fatal("按 ID 取不到内置方言")
	}
	if _, ok := r.Dialect("nope/whatever@1"); ok {
		t.Fatal("未登记的方言不该取到")
	}
}

func TestRegistryRejectsBadIDs(t *testing.T) {
	r := plugin.NewRegistry()
	cases := []struct {
		name     string
		register func() error
	}{
		{
			name: "内置方言不用 lax 前缀",
			register: func() error {
				return r.RegisterSystem(stubDialect{id: "com.example/x@1"})
			},
		},
		{
			name: "插件方言前缀对不上插件 ID",
			register: func() error {
				return r.RegisterPluginDialect("com.example.other", stubDialect{id: "com.example.mimo/x@1"})
			},
		},
		{
			name: "插件方言冒用 lax 前缀",
			register: func() error {
				return r.RegisterPluginDialect("lax", stubDialect{id: "lax/x@1"})
			},
		},
		{
			name: "操作名不认识",
			register: func() error {
				return r.RegisterSystem(stubDialect{id: "lax/x@1", op: "chat.frobnicate@1"})
			},
		},
		{
			name: "线协议不认识",
			register: func() error {
				return r.RegisterSystem(stubDialect{id: "lax/x@1", wire: "carrier-pigeon"})
			},
		},
	}
	for _, c := range cases {
		if err := c.register(); err == nil {
			t.Fatalf("%s：期望被拒绝，却登记成功", c.name)
		}
	}
	if ids := r.IDs(); len(ids) != 0 {
		t.Fatalf("被拒绝的登记留下了方言：%v", ids)
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	r := plugin.NewRegistry()
	first := stubDialect{id: "com.example.mimo/x@1"}
	if err := r.RegisterPluginDialect("com.example.mimo", first); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	if err := r.RegisterPluginDialect("com.example.mimo", first); err == nil {
		t.Fatal("同一个 ID 登记两次应当报错")
	}
}

// stubDialect 只满足接口，用来测登记规则。
type stubDialect struct {
	id   string
	op   string
	wire string
}

func (s stubDialect) ID() string {
	if s.id == "" {
		return "com.example.stub/x@1"
	}
	return s.id
}

func (s stubDialect) Operation() string {
	if s.op == "" {
		return plugin.OpChatComplete
	}
	return s.op
}

func (s stubDialect) UpstreamWire() string {
	if s.wire == "" {
		return plugin.WireOpenAIChat
	}
	return s.wire
}

func (s stubDialect) Prepare(plugin.Input) (plugin.Plan, error) { return plugin.Plan{}, nil }

func (s stubDialect) Decode(plugin.Input, plugin.Response) (plugin.Result, error) {
	return plugin.Result{}, nil
}
