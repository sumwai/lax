package store_test

import (
	"math"
	"testing"
	"time"

	"lax/internal/store"
	"lax/internal/translate"
)

// 语音两项计费的用例：TTS 按每百万字符、ASR 按每秒。两列与 token 四列互斥，
// 花费公式也分开：字符价要除 1e6，秒价不除。

// TestPriceTTSAndAudioFields 钉住两个单价列的写入与合并：厂商级标价写进去、
// 账号覆盖值合并到 EffectiveModels，清掉覆盖之后回到目录价。
func TestPriceTTSAndAudioFields(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")

	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		TTSChar: floatPtr(15), AudioSec: floatPtr(0.0001),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}
	m := oneModel(t, st, "acme", "m1")
	if m.PriceTTSChar == nil || *m.PriceTTSChar != 15 {
		t.Fatalf("合成价 = %v，期望 15", m.PriceTTSChar)
	}
	if m.PriceAudioSec == nil || *m.PriceAudioSec != 0.0001 {
		t.Fatalf("转写价 = %v，期望 0.0001", m.PriceAudioSec)
	}

	if err := st.SetAccountModelPrices("a1", "m1", store.ModelPrices{
		TTSChar: floatPtr(20), AudioSec: floatPtr(0.0002),
	}); err != nil {
		t.Fatalf("写账号覆盖价: %v", err)
	}
	infos, err := st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("读有效值: %v", err)
	}
	var got *store.ModelInfo
	for i := range infos {
		if infos[i].Model == "m1" {
			got = &infos[i]
		}
	}
	if got == nil {
		t.Fatalf("EffectiveModels 里没有 m1: %+v", infos)
	}
	if got.PriceTTSChar == nil || *got.PriceTTSChar != 20 {
		t.Fatalf("合并后的合成价 = %v，期望账号覆盖的 20", got.PriceTTSChar)
	}
	if got.PriceAudioSec == nil || *got.PriceAudioSec != 0.0002 {
		t.Fatalf("合并后的转写价 = %v，期望账号覆盖的 0.0002", got.PriceAudioSec)
	}

	if err := st.ClearAccountModelPrices("a1", "m1", []string{"tts-char", "audio-second"}); err != nil {
		t.Fatalf("清账号覆盖价: %v", err)
	}
	infos, err = st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("再读有效值: %v", err)
	}
	for _, info := range infos {
		if info.Model != "m1" {
			continue
		}
		if info.PriceTTSChar == nil || *info.PriceTTSChar != 15 {
			t.Fatalf("清掉覆盖之后合成价 = %v，期望回到目录的 15", info.PriceTTSChar)
		}
	}
}

// TestUsageTTSAndAudioCost 钉住语音花费的公式：
// 1M 合成字符 × 15 = 15 元；10 秒 × 0.006 = 0.06 元。两个协议各一条记录、分开汇总。
func TestUsageTTSAndAudioCost(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		TTSChar: floatPtr(15), AudioSec: floatPtr(0.006),
	}); err != nil {
		t.Fatalf("写厂商级价: %v", err)
	}

	speech := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: translate.ProtocolSpeech, Outbound: translate.ProtocolSpeech,
		ModelRequested: "tts-1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, TTSChars: 1_000_000,
	}
	if err := st.InsertRequest(&speech); err != nil {
		t.Fatalf("写 TTS 记录: %v", err)
	}
	asr := store.RequestRecord{
		TS: time.Now().Unix(), Inbound: translate.ProtocolTranscriptions, Outbound: translate.ProtocolTranscriptions,
		ModelRequested: "whisper-1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true, AudioSeconds: 10,
	}
	if err := st.InsertRequest(&asr); err != nil {
		t.Fatalf("写 ASR 记录: %v", err)
	}

	rows, err := st.Usage("model", time.Now().Add(-time.Hour).Unix(), 0)
	if err != nil {
		t.Fatalf("汇总用量: %v", err)
	}
	// 两条记录用的是同一个 model_used，按模型维度汇总成一行：这一行同时带字符与秒，
	// 正好验证两列都能穿过聚合，且两项花费各自按自己的单位算。
	if len(rows) != 1 {
		t.Fatalf("汇总出 %d 行，期望 1 行: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.TTSChars != 1_000_000 || row.AudioSeconds != 10 {
		t.Fatalf("汇总的字符/秒 = %d/%d，期望 1000000/10", row.TTSChars, row.AudioSeconds)
	}
	if row.Cost == nil || math.Abs(*row.Cost-15.06) > 1e-9 {
		t.Fatalf("花费 = %v，期望 15.06（1M 字符×15 + 10 秒×0.006）", row.Cost)
	}
}
