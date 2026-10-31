package store_test

import (
	"strings"
	"testing"

	"lax/internal/store"
)

// 能力声明的写入口：set 替换给过的字段、add 合并、unset 清空、能力位按固定顺序落库。
// 语音入站只认这里写下的能力位（见 DESIGN 5.16），所以写入语义要钉住。

// TestSetProviderModelCapability 钉住 set 的语义：只替换给过的字段，没给的保持不动。
func TestSetProviderModelCapability(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")

	row, err := st.SetProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech, store.CapabilityTranscription},
		Voices:       []string{"Mia", "Chloe"},
		AudioFormats: []string{"mp3", "wav"},
	})
	if err != nil {
		t.Fatalf("set 能力: %v", err)
	}
	if got := strings.Join(row.Capabilities, ","); got != "speech,transcription" {
		t.Fatalf("能力位 = %q", got)
	}
	if strings.Join(row.Voices, ",") != "Mia,Chloe" || strings.Join(row.AudioFormats, ",") != "mp3,wav" {
		t.Fatalf("音色/格式 = %v / %v", row.Voices, row.AudioFormats)
	}

	// 只给 voices：能力与格式不动。
	row, err = st.SetProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Voices: []string{"冰糖"},
	})
	if err != nil {
		t.Fatalf("第二次 set: %v", err)
	}
	if strings.Join(row.Voices, ",") != "冰糖" {
		t.Fatalf("音色没被替换: %v", row.Voices)
	}
	if strings.Join(row.Capabilities, ",") != "speech,transcription" {
		t.Fatalf("没给的能力位被改动了: %v", row.Capabilities)
	}
	if strings.Join(row.AudioFormats, ",") != "mp3,wav" {
		t.Fatalf("没给的格式被改动了: %v", row.AudioFormats)
	}
}

// TestAddProviderModelCapability 钉住 add 的合并语义：并入、去重、重复执行结果不变。
func TestAddProviderModelCapability(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")

	if _, err := st.AddProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech},
		Voices:       []string{"Mia"},
	}); err != nil {
		t.Fatalf("第一次 add: %v", err)
	}
	row, err := st.AddProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Capabilities: []string{store.CapabilityTranscription, store.CapabilitySpeech},
		Voices:       []string{"Chloe", "Mia"},
	})
	if err != nil {
		t.Fatalf("第二次 add: %v", err)
	}
	if got := strings.Join(row.Capabilities, ","); got != "speech,transcription" {
		t.Fatalf("能力位 = %q，期望按固定顺序且去重", got)
	}
	if got := strings.Join(row.Voices, ","); got != "Mia,Chloe" {
		t.Fatalf("音色 = %q，期望 Mia,Chloe", got)
	}

	// 重复执行同一份声明：结果不变。
	again, err := st.AddProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Voices: []string{"Mia", "Chloe"},
	})
	if err != nil {
		t.Fatalf("第三次 add: %v", err)
	}
	if strings.Join(again.Voices, ",") != "Mia,Chloe" {
		t.Fatalf("重复 add 改了音色: %v", again.Voices)
	}
}

// TestModelCapabilityRejectsUnknown 钉住校验：未知能力位与控制字符都只能是写错了。
func TestModelCapabilityRejectsUnknown(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")

	if _, err := st.SetProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Capabilities: []string{"voice"},
	}); err == nil || !strings.Contains(err.Error(), "不认识") {
		t.Fatalf("未知能力位应报错，实际 %v", err)
	}
	if _, err := st.SetProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Voices: []string{"a\nb"},
	}); err == nil || !strings.Contains(err.Error(), "控制字符") {
		t.Fatalf("带控制字符的音色应报错，实际 %v", err)
	}
	if _, err := st.SetProviderModelCapability("acme", "missing", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech},
	}); err == nil {
		t.Fatalf("模型不在目录里时应报错")
	}
}

// TestClearProviderModelCapability 钉住 unset 只清指定列，其余保留。
func TestClearProviderModelCapability(t *testing.T) {
	st := openTemp(t)
	catalogAccountForPrice(t, st, "a1")

	if _, err := st.SetProviderModelCapability("acme", "m1", store.CapabilityPatch{
		Capabilities: []string{store.CapabilitySpeech},
		Voices:       []string{"Mia"},
		AudioFormats: []string{"mp3"},
	}); err != nil {
		t.Fatalf("先 set: %v", err)
	}
	row, err := st.ClearProviderModelCapability("acme", "m1", []string{"voices"})
	if err != nil {
		t.Fatalf("清音色: %v", err)
	}
	if len(row.Voices) != 0 {
		t.Fatalf("音色没清掉: %v", row.Voices)
	}
	if len(row.Capabilities) != 1 || len(row.AudioFormats) != 1 {
		t.Fatalf("清一项时动了别的列: %+v", row)
	}

	// 字段名写错：报错并列出可用字段。
	if _, err := st.ClearProviderModelCapability("acme", "m1", []string{"voice"}); err == nil ||
		!strings.Contains(err.Error(), "voices") {
		t.Fatalf("未知字段应报错并列出可用字段，实际 %v", err)
	}
}

// TestModelCapabilityOrder 钉住能力位的固定顺序：chat → embedding → speech → transcription。
func TestModelCapabilityOrder(t *testing.T) {
	want := "chat,embedding,speech,transcription"
	if got := strings.Join(store.CapabilityValues(), ","); got != want {
		t.Fatalf("CapabilityValues() = %q，期望 %q", got, want)
	}
}
