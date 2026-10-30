package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// syncCatalog 用一个固定返回 body 的假上游跑一次 SyncModels，返回写库之后的目录行（按名字索引）。
// 断言的是「上游清单里的元数据有没有落到 provider_models 的四列上」，所以读的是库而不是返回值。
func syncCatalog(t *testing.T, protocol, body string) map[string]store.ProviderModel {
	t.Helper()
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)

	acc := f.add(t, "meta", protocol, up.URL, "sk-meta", nil, 100)
	gs := newGateway(t, f)
	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步模型: %v", err)
	}
	rows, err := f.st.ProviderModels(acc.Provider)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	out := make(map[string]store.ProviderModel, len(rows))
	for _, row := range rows {
		out[row.Model] = row
	}
	return out
}

// mustInt64 断言目录里的一列长度有值且等于 want；want 为 nil 时断言这一列是空的。
func mustInt64(t *testing.T, what string, got *int64, want *int64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("%s = %d，期望留空（上游没给）", what, *got)
	case want != nil && got == nil:
		t.Fatalf("%s 为空，期望 %d", what, *want)
	case want != nil && got != nil && *got != *want:
		t.Fatalf("%s = %d，期望 %d", what, *got, *want)
	}
}

func ptr(v int64) *int64 { return &v }

// TestSyncModelsMetadataByShape 钉住三种已知形状都能从条目对象里读出元数据（见 DESIGN 5.7 的来源表）：
// commandcode 的 context_length 与 supported_endpoints、openrouter 的 top_provider.max_completion_tokens、
// gemini 的 inputTokenLimit 与 outputTokenLimit 与 supportedGenerationMethods。
// 三种形状走的是同一个解析器，元数据跟着名字一起写进目录。
func TestSyncModelsMetadataByShape(t *testing.T) {
	t.Run("commandcode 的 data 形状", func(t *testing.T) {
		catalog := syncCatalog(t, gateway.ProtocolChat, `{"data":[
			{"id":"claude-sonnet-4","name":"Claude Sonnet 4","owned_by":"anthropic",
			 "context_length":200000,"supported_endpoints":["/messages"]},
			{"id":"gpt-5","context_length":400000,"supported_endpoints":["/chat/completions","/responses"]}
		]}`)

		claude := catalog["claude-sonnet-4"]
		mustInt64(t, "claude-sonnet-4 的上下文长度", claude.ContextWindow, ptr(200000))
		mustInt64(t, "claude-sonnet-4 的最大输出", claude.MaxOutput, nil)
		if got := strings.Join(claude.Protocols, ","); got != gateway.ProtocolAnthropic {
			t.Fatalf("claude-sonnet-4 的协议 = %q，期望 anthropic（supported_endpoints 里的 /messages）", got)
		}
		if claude.Source != store.SourceSync {
			t.Fatalf("来源 = %q，期望 sync", claude.Source)
		}

		gpt := catalog["gpt-5"]
		mustInt64(t, "gpt-5 的上下文长度", gpt.ContextWindow, ptr(400000))
		if got := strings.Join(gpt.Protocols, ","); got != "openai-chat,openai-responses" {
			t.Fatalf("gpt-5 的协议 = %q，期望 openai-chat,openai-responses（固定顺序）", got)
		}
	})

	t.Run("openrouter 的 data 形状", func(t *testing.T) {
		catalog := syncCatalog(t, gateway.ProtocolChat, `{"data":[
			{"id":"anthropic/claude-3.5-sonnet","context_length":200000,
			 "top_provider":{"context_length":200000,"max_completion_tokens":8192},
			 "supported_parameters":["tools","reasoning"]}
		]}`)

		row := catalog["anthropic/claude-3.5-sonnet"]
		mustInt64(t, "上下文长度", row.ContextWindow, ptr(200000))
		mustInt64(t, "最大输出（top_provider.max_completion_tokens）", row.MaxOutput, ptr(8192))
		// openrouter 没声明支持哪些协议：这一列留空，不能从 supported_parameters 猜。
		if len(row.Protocols) != 0 {
			t.Fatalf("协议 = %v，期望空", row.Protocols)
		}
		// 能力位暂不解析（supported_parameters 各家形状不同），留空表示未知。
		if len(row.Capabilities) != 0 {
			t.Fatalf("能力位 = %v，期望留空", row.Capabilities)
		}
	})

	t.Run("gemini 的 models 形状", func(t *testing.T) {
		catalog := syncCatalog(t, gateway.ProtocolGemini, `{"models":[
			{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro",
			 "inputTokenLimit":1048576,"outputTokenLimit":65536,
			 "supportedGenerationMethods":["generateContent","countTokens"]},
			{"name":"models/text-embedding-004","inputTokenLimit":2048,
			 "supportedGenerationMethods":["embedContent"]}
		]}`)

		row, ok := catalog["gemini-2.5-pro"]
		if !ok {
			t.Fatalf("目录里没有 gemini-2.5-pro：%+v", catalog)
		}
		mustInt64(t, "上下文长度（inputTokenLimit）", row.ContextWindow, ptr(1048576))
		mustInt64(t, "最大输出（outputTokenLimit）", row.MaxOutput, ptr(65536))
		if got := strings.Join(row.Protocols, ","); got != gateway.ProtocolGemini {
			t.Fatalf("协议 = %q，期望 gemini（generateContent，countTokens 不算）", got)
		}
		// 没有 generateContent 的条目按方法过滤掉，不会因为带了长度就进目录。
		if _, ok := catalog["text-embedding-004"]; ok {
			t.Fatalf("embedding 条目不该进目录：%+v", catalog)
		}
	})

	t.Run("顶层数组形状", func(t *testing.T) {
		catalog := syncCatalog(t, gateway.ProtocolChat,
			`[{"id":"m1","context_length":1000,"top_provider":{"max_completion_tokens":100},
			   "supported_endpoints":["/responses"]}]`)

		row := catalog["m1"]
		mustInt64(t, "上下文长度", row.ContextWindow, ptr(1000))
		mustInt64(t, "最大输出", row.MaxOutput, ptr(100))
		if got := strings.Join(row.Protocols, ","); got != gateway.ProtocolResponses {
			t.Fatalf("协议 = %q，期望 openai-responses", got)
		}
	})
}

// TestSyncModelsMetadataMissing 钉住「字段缺失就留空，不猜默认值」：只有名字的条目在目录里
// 四列全空，0 与未识别的端点名同样不算数据。
func TestSyncModelsMetadataMissing(t *testing.T) {
	catalog := syncCatalog(t, gateway.ProtocolChat, `{"data":[
		{"id":"plain"},
		{"id":"zero","context_length":0,"outputTokenLimit":0,
		 "top_provider":{"max_completion_tokens":0},"supported_endpoints":["/embeddings"]}
	]}`)

	for _, name := range []string{"plain", "zero"} {
		row, ok := catalog[name]
		if !ok {
			t.Fatalf("目录里没有 %s：%+v", name, catalog)
		}
		if row.ContextWindow != nil || row.MaxOutput != nil {
			t.Fatalf("%s 的长度类元数据 = %v / %v，期望留空", name, row.ContextWindow, row.MaxOutput)
		}
		if len(row.Protocols) != 0 || len(row.Capabilities) != 0 {
			t.Fatalf("%s 的协议/能力位 = %v / %v，期望留空", name, row.Protocols, row.Capabilities)
		}
		if row.Source != store.SourceSync {
			t.Fatalf("%s 的来源 = %q，期望 sync", name, row.Source)
		}
	}
}

// TestSyncModelsKeepsManualMetadata 钉住来源不降级：用户手填过的目录行不会被一次同步改写，
// 上游这次给的上下文长度与协议集合都写不进去（见 DESIGN 5.13）。
func TestSyncModelsKeepsManualMetadata(t *testing.T) {
	f := newFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m1","context_length":999999,
			"supported_endpoints":["/responses"]}]}`)
	}))
	t.Cleanup(up.Close)

	acc := f.add(t, "manual1", gateway.ProtocolChat, up.URL, "sk-meta", nil, 100)
	if err := f.st.UpsertProviderModels(acc.Provider, []store.ProviderModel{{
		Model:         "m1",
		Source:        store.SourceManual,
		ContextWindow: ptr(32000),
		Protocols:     []string{gateway.ProtocolAnthropic},
	}}); err != nil {
		t.Fatalf("写手填目录行: %v", err)
	}

	gs := newGateway(t, f)
	if _, err := gs.SyncModels(context.Background(), acc); err != nil {
		t.Fatalf("同步模型: %v", err)
	}

	rows, err := f.st.ProviderModels(acc.Provider)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("目录 = %+v，期望只有 m1 一行", rows)
	}
	row := rows[0]
	if row.Source != store.SourceManual {
		t.Fatalf("来源 = %q，期望 manual（同步不降级）", row.Source)
	}
	mustInt64(t, "手填的上下文长度", row.ContextWindow, ptr(32000))
	if got := strings.Join(row.Protocols, ","); got != gateway.ProtocolAnthropic {
		t.Fatalf("协议 = %q，期望手填的 anthropic 原样保留", got)
	}
}
