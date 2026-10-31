package store_test

import (
	"testing"

	"lax/internal/store"
)

// 方言绑定与语音回填。回填的方言选择必须与网关 speechOutbound 等价：
// 厂商声明了同一条语音路由就同协议直通，否则落到 MiMo 的 chat 方言。

// TestSetModelOperationUpsert 钉住 upsert 语义：同主键覆盖，时间由调用方给。
func TestSetModelOperationUpsert(t *testing.T) {
	st := openTemp(t)

	op := store.ModelOperation{
		Provider: "acme", Model: "m1", Operation: "chat.complete@1",
		DialectID: "acme/chat@1", CapabilitiesJSON: `{"tools":true}`, LimitsJSON: "{}",
		Source: "plugin", UpdatedAt: 100,
	}
	if err := st.SetModelOperation(op); err != nil {
		t.Fatalf("写绑定: %v", err)
	}
	got, ok, err := st.ModelOperation("acme", "m1", "chat.complete@1")
	if err != nil || !ok {
		t.Fatalf("读回绑定: ok=%v err=%v", ok, err)
	}
	if got.DialectID != op.DialectID || got.CapabilitiesJSON != op.CapabilitiesJSON || got.Source != "plugin" {
		t.Fatalf("读回的绑定 = %+v", got)
	}
	if got.UpdatedAt != 100 {
		t.Fatalf("updated_at = %d，期望调用方给的 100", got.UpdatedAt)
	}

	op.DialectID = "acme/chat-v2@1"
	op.CapabilitiesJSON = "{}"
	op.UpdatedAt = 200
	if err := st.SetModelOperation(op); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	got, _, _ = st.ModelOperation("acme", "m1", "chat.complete@1")
	if got.DialectID != "acme/chat-v2@1" || got.CapabilitiesJSON != "{}" || got.UpdatedAt != 200 {
		t.Fatalf("覆盖之后的绑定 = %+v", got)
	}

	if _, ok, err := st.ModelOperation("acme", "m1", "speech.synthesize@1"); err != nil || ok {
		t.Fatalf("没写过的操作不该有绑定: ok=%v err=%v", ok, err)
	}
}

// TestDeleteAndListModelOperations 钉住删除幂等、列表的过滤与排序。
func TestDeleteAndListModelOperations(t *testing.T) {
	st := openTemp(t)

	rows := []store.ModelOperation{
		{Provider: "b", Model: "m2", Operation: "chat.complete@1", DialectID: "d1", Source: "manual"},
		{Provider: "a", Model: "m2", Operation: "chat.complete@1", DialectID: "d2", Source: "manual"},
		{Provider: "a", Model: "m1", Operation: "speech.synthesize@1", DialectID: "d3", Source: "manual"},
		{Provider: "a", Model: "m1", Operation: "chat.complete@1", DialectID: "d4", Source: "manual"},
	}
	for _, r := range rows {
		if err := st.SetModelOperation(r); err != nil {
			t.Fatalf("写绑定 %+v: %v", r, err)
		}
	}

	all, err := st.ListModelOperations("", "")
	if err != nil {
		t.Fatalf("列全部: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("全部绑定 = %d 行，期望 4 行", len(all))
	}
	want := []string{"a/m1/chat.complete@1", "a/m1/speech.synthesize@1", "a/m2/chat.complete@1", "b/m2/chat.complete@1"}
	for i, key := range want {
		if got := all[i].Provider + "/" + all[i].Model + "/" + all[i].Operation; got != key {
			t.Fatalf("第 %d 行 = %s，期望 %s（按厂商、模型、操作升序）", i, got, key)
		}
	}

	byProvider, err := st.ListModelOperations("a", "")
	if err != nil || len(byProvider) != 3 {
		t.Fatalf("按厂商过滤 = %d 行, err = %v，期望 3 行", len(byProvider), err)
	}
	byModel, err := st.ListModelOperations("", "m1")
	if err != nil || len(byModel) != 2 {
		t.Fatalf("按模型过滤 = %d 行, err = %v，期望 2 行", len(byModel), err)
	}
	both, err := st.ListModelOperations("b", "m2")
	if err != nil || len(both) != 1 {
		t.Fatalf("按厂商与模型过滤 = %d 行, err = %v，期望 1 行", len(both), err)
	}

	if err := st.DeleteModelOperation("a", "m1", "chat.complete@1"); err != nil {
		t.Fatalf("删绑定: %v", err)
	}
	if err := st.DeleteModelOperation("a", "m1", "chat.complete@1"); err != nil {
		t.Fatalf("重复删同一条绑定应幂等，得到 %v", err)
	}
	if _, ok, _ := st.ModelOperation("a", "m1", "chat.complete@1"); ok {
		t.Fatal("删过的绑定还在")
	}
	if left, _ := st.ListModelOperations("", ""); len(left) != 3 {
		t.Fatalf("删除之后还剩 %d 行，期望 3 行", len(left))
	}
}

// speechFixture 搭一份带语音能力的目录：两家厂商，一家只有 chat 路由（MiMo 形状），
// 一家声明了自己的两条语音路由（同协议直通）。
func speechFixture(t *testing.T, st *store.Store) {
	t.Helper()
	mustProvider(t, st, "mimo", "https://mimo.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	mustProvider(t, st, "voiced", "https://voiced.example.com",
		store.ProviderRoute{Protocol: "openai-speech", Path: "/v1/audio/speech"},
		store.ProviderRoute{Protocol: "openai-transcriptions", Path: "/v1/audio/transcriptions"})

	if err := st.UpsertProviderModels("mimo", []store.ProviderModel{
		{Model: "tts", Source: store.SourceManual, Capabilities: []string{store.CapabilitySpeech}},
		{Model: "dual", Source: store.SourceManual,
			Capabilities: []string{store.CapabilitySpeech, store.CapabilityTranscription}},
		{Model: "handwritten", Source: store.SourceManual, Capabilities: []string{store.CapabilitySpeech}},
	}); err != nil {
		t.Fatalf("写 mimo 目录: %v", err)
	}
	if err := st.UpsertProviderModels("voiced", []store.ProviderModel{
		{Model: "tts", Source: store.SourceManual, Capabilities: []string{store.CapabilitySpeech}},
		{Model: "asr", Source: store.SourceManual, Capabilities: []string{store.CapabilityTranscription}},
	}); err != nil {
		t.Fatalf("写 voiced 目录: %v", err)
	}
}

// TestBackfillSpeechDialects 钉住回填的方言选择、行数与幂等性。
func TestBackfillSpeechDialects(t *testing.T) {
	st := openTemp(t)
	speechFixture(t, st)

	// 已有绑定：回填不覆盖它，也不算进新增行数。
	if err := st.SetModelOperation(store.ModelOperation{
		Provider: "mimo", Model: "handwritten", Operation: "speech.synthesize@1",
		DialectID: "com.example.mimo/custom@1", Source: "manual", UpdatedAt: 7,
	}); err != nil {
		t.Fatalf("写已有绑定: %v", err)
	}

	const now = 1700000000
	n, err := st.BackfillSpeechDialects(now)
	if err != nil {
		t.Fatalf("回填: %v", err)
	}
	if n != 5 {
		t.Fatalf("新增 %d 行，期望 5 行", n)
	}

	cases := []struct {
		provider, model, operation, dialect string
	}{
		// 没有语音路由：落到 MiMo 的 chat 方言。
		{"mimo", "tts", "speech.synthesize@1", "lax/mimo-chat-speech@1"},
		// 一个模型两个能力：两行。
		{"mimo", "dual", "speech.synthesize@1", "lax/mimo-chat-speech@1"},
		{"mimo", "dual", "speech.transcribe@1", "lax/mimo-chat-asr@1"},
		// 声明了自己的语音路由：同协议直通。
		{"voiced", "tts", "speech.synthesize@1", "lax/openai-speech@1"},
		{"voiced", "asr", "speech.transcribe@1", "lax/openai-transcriptions@1"},
	}
	for _, c := range cases {
		got, ok, err := st.ModelOperation(c.provider, c.model, c.operation)
		if err != nil || !ok {
			t.Fatalf("%s/%s/%s 没有绑定: ok=%v err=%v", c.provider, c.model, c.operation, ok, err)
		}
		if got.DialectID != c.dialect {
			t.Fatalf("%s/%s/%s 的方言 = %s，期望 %s", c.provider, c.model, c.operation, got.DialectID, c.dialect)
		}
		if got.Source != "declared" || got.CapabilitiesJSON != "{}" || got.LimitsJSON != "{}" || got.UpdatedAt != now {
			t.Fatalf("%s/%s/%s 的回填行 = %+v", c.provider, c.model, c.operation, got)
		}
	}

	// 已有绑定一个字不动。
	hand, _, _ := st.ModelOperation("mimo", "handwritten", "speech.synthesize@1")
	if hand.DialectID != "com.example.mimo/custom@1" || hand.Source != "manual" || hand.UpdatedAt != 7 {
		t.Fatalf("回填覆盖了已有绑定: %+v", hand)
	}

	// 幂等：第二次一行都不新增。
	if n, err := st.BackfillSpeechDialects(now + 100); err != nil || n != 0 {
		t.Fatalf("第二次回填 = %d, err = %v，期望 0", n, err)
	}
	if all, _ := st.ListModelOperations("", ""); len(all) != 6 {
		t.Fatalf("绑定总数 = %d，期望 6（5 行回填 + 1 行手写）", len(all))
	}
}

// TestBackfillSpeechDialectsSkipsOtherCapabilities 确认没有语音能力的行不产生绑定。
func TestBackfillSpeechDialectsSkipsOtherCapabilities(t *testing.T) {
	st := openTemp(t)
	mustProvider(t, st, "acme", "https://api.example.com",
		store.ProviderRoute{Protocol: "openai-chat", Path: "/v1/chat/completions"})
	if err := st.UpsertProviderModels("acme", []store.ProviderModel{
		{Model: "chatonly", Source: store.SourceSync, Capabilities: []string{store.CapabilityChat}},
	}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	if n, err := st.BackfillSpeechDialects(100); err != nil || n != 0 {
		t.Fatalf("回填 = %d, err = %v，期望 0", n, err)
	}
}
