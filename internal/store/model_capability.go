package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
)

// 模型能力的声明与读取。
//
// 能力位回答的是「这个模型能被哪种入站端点使用」，而不是「上游支持哪种协议」：
// 一个模型可以既走 openai-chat 出站又是语音模型（MiMo 的 mimo-v2.5-tts 就是这样），
// 所以能力与协议是两列事实。目录同步拿不到这两类信息（上游清单只给模型名），
// 语音模型靠 `model capability add/set` 手工声明，未声明时语音入站直接拒绝，不发上游请求。
//
// 音色与音频格式同样只由声明给出：没有音色列表时客户端不知道该传什么，
// 上游只会回一个 `400 Param Incorrect`；在本地拒绝能把可用取值一起说出来。

// 能力位取值。chat 与 embedding 已经由目录同步写过，speech 与 transcription 只能手工声明。
const (
	// CapabilityChat 表示这个模型能做对话。
	CapabilityChat = "chat"
	// CapabilityEmbedding 表示这个模型能做向量化。
	CapabilityEmbedding = "embedding"
	// CapabilitySpeech 表示这个模型能做文本转语音，可以被 /v1/audio/speech 使用。
	CapabilitySpeech = "speech"
	// CapabilityTranscription 表示这个模型能做语音转文本，可以被 /v1/audio/transcriptions 使用。
	CapabilityTranscription = "transcription"
)

// capabilityOrder 是能力位的固定顺序：落库、输出与命令面都按它排，
// 同一份声明多跑几次得到同一串，对比数据库时看不出无意义的差异。
var capabilityOrder = []string{
	CapabilityChat, CapabilityEmbedding, CapabilitySpeech, CapabilityTranscription,
}

// CapabilityValues 按固定顺序返回全部能力位，供错误文案与帮助文本用。
func CapabilityValues() []string {
	out := make([]string, len(capabilityOrder))
	copy(out, capabilityOrder)
	return out
}

// IsCapability 判断一个取值是不是能力位。
func IsCapability(value string) bool {
	for _, c := range capabilityOrder {
		if c == value {
			return true
		}
	}
	return false
}

// OrderCapabilities 按固定顺序排好并去重，认不出的取值保留在末尾按原顺序：
// 写入前已经校验过，这里只为读出历史数据时保持顺序稳定。
func OrderCapabilities(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, canonical := range capabilityOrder {
		for _, v := range values {
			if v == canonical && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// CapabilityPatch 是一次能力声明改动：nil 表示这一项不动，非 nil 表示这一项要写。
//
// 三个字段都是「整份列表」而不是增量：Add 与 Set 的区别在写入口，而不在这个结构。
// 空切片表示清空，与 nil（不动）是两件事。
type CapabilityPatch struct {
	Capabilities []string
	Voices       []string
	AudioFormats []string
}

// empty 表示三项都没给。那样的写入什么都不改，调用方该在命令层拦住。
func (p CapabilityPatch) empty() bool {
	return p.Capabilities == nil && p.Voices == nil && p.AudioFormats == nil
}

// validate 检查能力位与两项列表：未知能力位、控制字符都只能是写错了。
// 音色名可以是中文（MiMo 有「冰糖」这样的名字），所以不限制字符集，只拒绝会破坏输出与请求体的字符。
func (p CapabilityPatch) validate() error {
	for _, c := range p.Capabilities {
		if !IsCapability(c) {
			return fmt.Errorf("能力位 %q 不认识，可用的是 %s", c,
				strings.Join(CapabilityValues(), "、"))
		}
	}
	for _, group := range []struct {
		name   string
		values []string
	}{
		{"voices", p.Voices},
		{"audio-formats", p.AudioFormats},
	} {
		for _, v := range group.values {
			if v == "" {
				continue
			}
			if strings.IndexFunc(v, unicode.IsControl) >= 0 {
				return fmt.Errorf("%s 里的取值 %q 带控制字符：它会原样进输出与请求体，只能写可见字符", group.name, v)
			}
		}
	}
	return nil
}

// cleanList 去掉首尾空白与空项、按顺序去重。
func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// SetProviderModelCapability 替换传进来的那几项，其余不动。
//
// 行必须已经在目录里，否则报 ErrModelNotInCatalog：能力是模型事实的附属信息，
// 写入不顺手插一行没有名字以外任何事实的空壳（与价格写入同一条规则）。
// 三项都是整份列表：Set 的语义是「这一项就是这个值」，合并要用 Add。
func (s *Store) SetProviderModelCapability(provider, model string, patch CapabilityPatch) (ProviderModel, error) {
	return s.writeCapability(provider, model, patch, false)
}

// AddProviderModelCapability 把传进来的取值并进现有列表，已有的不重复添加。
//
// 语义与 Set 的区别只在合并方式：add 用于「再声明一个音色」「再声明一个能力」，
// 重复执行结果不变。传空切片等于不动这一项（没有可加的东西）。
func (s *Store) AddProviderModelCapability(provider, model string, patch CapabilityPatch) (ProviderModel, error) {
	return s.writeCapability(provider, model, patch, true)
}

func (s *Store) writeCapability(provider, model string, patch CapabilityPatch, additive bool) (ProviderModel, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return ProviderModel{}, fmt.Errorf("声明模型能力需要厂商名与模型名：目录以 (厂商, 模型) 为主键，两者都不能为空")
	}
	if patch.empty() {
		return ProviderModel{}, fmt.Errorf("厂商 %q 的模型 %q 一项都没给：至少给一个能力、音色或音频格式", provider, model)
	}
	if err := patch.validate(); err != nil {
		return ProviderModel{}, err
	}

	var out ProviderModel
	err := s.withTx(func(tx *gorm.DB) error {
		if err := requireProviderModelTx(tx, provider, model); err != nil {
			return err
		}
		var row providerModelRow
		if err := tx.Where("provider = ? AND model = ?", provider, model).Limit(1).Find(&row).Error; err != nil {
			return err
		}

		capabilities := patch.Capabilities
		voices := patch.Voices
		formats := patch.AudioFormats
		if additive {
			capabilities = mergeList(splitList(row.Capabilities), capabilities)
			voices = mergeList(splitList(row.Voices), voices)
			formats = mergeList(splitList(row.AudioFormats), formats)
		} else {
			// Set 只换给过的那几项：nil 保持「不动」，空切片才是清空。
			if capabilities != nil {
				capabilities = cleanList(capabilities)
			}
			if voices != nil {
				voices = cleanList(voices)
			}
			if formats != nil {
				formats = cleanList(formats)
			}
		}
		if capabilities != nil {
			capabilities = OrderCapabilities(cleanList(capabilities))
		}

		updates := map[string]any{"updated_at": time.Now().Unix()}
		if capabilities != nil {
			updates["capabilities"] = joinList(capabilities)
		}
		if voices != nil {
			updates["voices"] = joinList(voices)
		}
		if formats != nil {
			updates["audio_formats"] = joinList(formats)
		}
		if err := tx.Model(&providerModelRow{}).
			Where("provider = ? AND model = ?", provider, model).
			Updates(updates).Error; err != nil {
			return err
		}

		if err := tx.Where("provider = ? AND model = ?", provider, model).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		out = providerModelsFromRows([]providerModelRow{row})[0]
		return nil
	})
	return out, err
}

// mergeList 把 incoming 并进 base：按 base 的顺序保留，再追加 incoming 里没有的。
func mergeList(base, incoming []string) []string {
	out := make([]string, 0, len(base)+len(incoming))
	seen := make(map[string]bool, len(base)+len(incoming))
	for _, group := range [][]string{base, incoming} {
		for _, raw := range group {
			v := strings.TrimSpace(raw)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// CapabilityField 是 `model capability unset` 的字段名。
type CapabilityField string

const (
	// FieldCapability 对应 capabilities 列。
	FieldCapability CapabilityField = "capability"
	// FieldVoices 对应 voices 列。
	FieldVoices CapabilityField = "voices"
	// FieldAudioFormats 对应 audio_formats 列。
	FieldAudioFormats CapabilityField = "audio-formats"
)

// capabilityColumns 把字段名映射成列名。列名只从这里取，不拿用户输入拼 SQL。
var capabilityColumns = map[CapabilityField]string{
	FieldCapability:   "capabilities",
	FieldVoices:       "voices",
	FieldAudioFormats: "audio_formats",
}

// CapabilityFieldNames 按固定顺序返回三个字段名，供错误文案与帮助文本用。
func CapabilityFieldNames() []string {
	return []string{string(FieldCapability), string(FieldVoices), string(FieldAudioFormats)}
}

// ParseCapabilityField 把命令行给的字段名解析成 CapabilityField。
func ParseCapabilityField(name string) (CapabilityField, error) {
	trimmed := strings.TrimSpace(name)
	for _, f := range []CapabilityField{FieldCapability, FieldVoices, FieldAudioFormats} {
		if trimmed == string(f) {
			return f, nil
		}
	}
	hint := ""
	if strings.HasPrefix(trimmed, "-") {
		hint = "；字段是位置参数，不带 -- 前缀"
	}
	return "", fmt.Errorf("未知的能力字段 %q%s，可用的是 %s", name, hint,
		strings.Join(CapabilityFieldNames(), "、"))
}

// ClearProviderModelCapability 把指定字段置空（= 这一项没声明过）。
//
// 只能清已存在于目录里的行：行不存在时报 ErrModelNotInCatalog，与写入口一致。
// 要清的字段本来为空时不报错（幂等），返回值是清完之后的整行。
func (s *Store) ClearProviderModelCapability(provider, model string, fields []string) (ProviderModel, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return ProviderModel{}, fmt.Errorf("清模型能力需要厂商名与模型名：目录以 (厂商, 模型) 为主键，两者都不能为空")
	}
	if len(fields) == 0 {
		return ProviderModel{}, errors.New("清能力至少要给一个字段：capability、voices 或 audio-formats")
	}
	parsed := make([]CapabilityField, 0, len(fields))
	seen := map[CapabilityField]bool{}
	for _, raw := range fields {
		f, err := ParseCapabilityField(raw)
		if err != nil {
			return ProviderModel{}, err
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		parsed = append(parsed, f)
	}

	var out ProviderModel
	err := s.withTx(func(tx *gorm.DB) error {
		if err := requireProviderModelTx(tx, provider, model); err != nil {
			return err
		}
		sets := make([]string, 0, len(parsed))
		for _, f := range parsed {
			sets = append(sets, capabilityColumns[f]+" = ''")
		}
		sets = append(sets, "updated_at = "+fmt.Sprintf("%d", time.Now().Unix()))
		if err := execSQL(tx, `UPDATE provider_models SET `+strings.Join(sets, ", ")+`
			WHERE provider = ? AND model = ?`, provider, model); err != nil {
			return err
		}
		var row providerModelRow
		if err := tx.Where("provider = ? AND model = ?", provider, model).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		out = providerModelsFromRows([]providerModelRow{row})[0]
		return nil
	})
	return out, err
}

// ModelCapabilities 读一个 (厂商, 模型) 的能力声明。行不存在时返回空与 false。
func (s *Store) ModelCapabilities(provider, model string) (ProviderModel, bool, error) {
	row, ok, err := s.ProviderModel(provider, model)
	return row, ok, err
}

// HasCapability 判断一行目录是不是声明了某个能力位。
func HasCapability(m ProviderModel, capability string) bool {
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}
