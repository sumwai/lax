package store

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// TierModeAllIn 表示命中档位后整笔输入与输出统一按该档位单价计费。
	TierModeAllIn = "all_in"
	// TierModeProgressive 表示未命中缓存的输入 token 按各档位区间分段累进计费。
	TierModeProgressive = "progressive"

	// TierBasisTotalInput 以 total_input（uncached + cache_read + cache_write）作为阶梯判定基数。
	TierBasisTotalInput = "total_input"
	// TierBasisUncachedInput 以 uncached_input 作为阶梯判定基数。
	TierBasisUncachedInput = "uncached_input"
)

// PricingRule 描述一个模型的阶梯定价与分时计费规则，序列化为 JSON 存在 pricing_rule 列中。
type PricingRule struct {
	Version  int                  `json:"version"`
	Tiers    *TieredPricing       `json:"tiers,omitempty"`
	Schedule *TimeSchedulePricing `json:"schedule,omitempty"`
}

// PriceSet 表示规则中可覆盖的四项 token 单价（每百万 token）。nil 表示不覆盖该项。
type PriceSet struct {
	In         *float64 `json:"in,omitempty"`
	Out        *float64 `json:"out,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

func (p *PriceSet) empty() bool {
	if p == nil {
		return true
	}
	return p.In == nil && p.Out == nil && p.CacheRead == nil && p.CacheWrite == nil
}

func (p PriceSet) validate(scope string) error {
	checks := []struct {
		name string
		val  *float64
	}{
		{"in", p.In},
		{"out", p.Out},
		{"cache_read", p.CacheRead},
		{"cache_write", p.CacheWrite},
	}
	for _, c := range checks {
		if c.val != nil {
			if math.IsNaN(*c.val) || math.IsInf(*c.val, 0) {
				return fmt.Errorf("%s 的 %s 单价无效（不能为 NaN 或 Inf）", scope, c.name)
			}
			if *c.val < 0 {
				return fmt.Errorf("%s 的 %s 单价是 %v：%w", scope, c.name, *c.val, ErrNegativePrice)
			}
		}
	}
	return nil
}

// TieredPricing 描述按 token 量的阶梯定价。
type TieredPricing struct {
	Mode     string      `json:"mode,omitempty"`
	Basis    string      `json:"basis,omitempty"`
	Brackets []PriceTier `json:"brackets"`
}

// PriceTier 描述阶梯中的单个档位。MaxTokens 为 nil 表示无上限（仅允许出现在最后一档）。
type PriceTier struct {
	MaxTokens *int64   `json:"max_tokens,omitempty"`
	Prices    PriceSet `json:"prices"`
}

// TimeSchedulePricing 描述按本地时间窗口的分时计费。
type TimeSchedulePricing struct {
	Timezone string       `json:"timezone,omitempty"`
	Windows  []TimeWindow `json:"windows"`
}

// TimeWindow 描述单个分时窗口。Days 取值 0(Sun)..6(Sat)，空表示每天生效；Start/End 为 "HH:MM"，
// End 支持 "24:00" 且允许 Start > End 的跨午夜窗口。
type TimeWindow struct {
	Name       string    `json:"name"`
	Days       []int     `json:"days,omitempty"`
	Start      string    `json:"start"`
	End        string    `json:"end"`
	Multiplier *float64  `json:"multiplier,omitempty"`
	Override   *PriceSet `json:"override,omitempty"`
}

// CostInput 是单次计费或试算的输入用量与时间戳。
type CostInput struct {
	TS                   int64
	InTokensUncached     int64
	CacheRead            int64
	CacheWrite           int64
	OutTokens            int64
	ReasoningTokensExtra int64
	TTSChars             int64
	AudioSeconds         int64
}

// CostItem 是计费明细中的单项记录。
type CostItem struct {
	Dimension string
	Quantity  int64
	UnitPrice *float64
	Cost      *float64
	TierIndex int
}

// CostBreakdown 是 CalculateCost 的完整输出，供落库快照与 CLI 试算展示使用。
type CostBreakdown struct {
	TotalCost       *float64
	TierIndex       int
	TierLabel       string
	BasisTokens     int64
	WindowName      string
	Multiplier      *float64
	EffectivePrices ModelPrices
	Items           []CostItem
}

// ValidatePricingRule 校验 PricingRule 的结构、枚举、阶梯单调性、时区、时间窗及非负价格。
func ValidatePricingRule(r *PricingRule) error {
	if r == nil {
		return fmt.Errorf("计费规则不能为空")
	}
	if r.Version < 0 {
		return fmt.Errorf("计费规则版本号 %d 不能为负数", r.Version)
	}
	if r.Tiers != nil {
		switch r.Tiers.Mode {
		case "", TierModeAllIn, TierModeProgressive:
		default:
			return fmt.Errorf("未知的阶梯模式 %q，可用 %s、%s", r.Tiers.Mode, TierModeAllIn, TierModeProgressive)
		}
		switch r.Tiers.Basis {
		case "", TierBasisTotalInput, TierBasisUncachedInput:
		default:
			return fmt.Errorf("未知的阶梯基数 %q，可用 %s、%s", r.Tiers.Basis, TierBasisTotalInput, TierBasisUncachedInput)
		}
		if len(r.Tiers.Brackets) == 0 {
			return fmt.Errorf("阶梯定价的 brackets 不能为空")
		}
		var prevMax *int64
		for i, b := range r.Tiers.Brackets {
			if b.MaxTokens == nil {
				if i != len(r.Tiers.Brackets)-1 {
					return fmt.Errorf("阶梯第 %d 档的 max_tokens 为空，只有最后一档允许无上限", i+1)
				}
			} else {
				if *b.MaxTokens <= 0 {
					return fmt.Errorf("阶梯第 %d 档的 max_tokens (%d) 必须大于 0", i+1, *b.MaxTokens)
				}
				if prevMax != nil && *b.MaxTokens <= *prevMax {
					return fmt.Errorf("阶梯第 %d 档的 max_tokens (%d) 必须严格大于前一档 (%d)", i+1, *b.MaxTokens, *prevMax)
				}
				prevMax = b.MaxTokens
			}
			if err := b.Prices.validate(fmt.Sprintf("阶梯第 %d 档", i+1)); err != nil {
				return err
			}
		}
	}
	if r.Schedule != nil {
		if r.Schedule.Timezone != "" {
			if _, err := time.LoadLocation(r.Schedule.Timezone); err != nil {
				return fmt.Errorf("无效的时区 %q：%w", r.Schedule.Timezone, err)
			}
		}
		seen := make(map[string]struct{}, len(r.Schedule.Windows))
		for i, w := range r.Schedule.Windows {
			name := strings.TrimSpace(w.Name)
			if name == "" {
				return fmt.Errorf("分时窗口第 %d 项名称不能为空", i+1)
			}
			if _, ok := seen[name]; ok {
				return fmt.Errorf("分时窗口名称 %q 重复", name)
			}
			seen[name] = struct{}{}

			for _, d := range w.Days {
				if d < 0 || d > 6 {
					return fmt.Errorf("分时窗口 %q 的星期值 %d 超出 0..6 范围", name, d)
				}
			}
			startMin, err := parseHHMM(w.Start, false)
			if err != nil {
				return fmt.Errorf("分时窗口 %q 的 start %q 无效：%w", name, w.Start, err)
			}
			endMin, err := parseHHMM(w.End, true)
			if err != nil {
				return fmt.Errorf("分时窗口 %q 的 end %q 无效：%w", name, w.End, err)
			}
			if startMin == endMin {
				return fmt.Errorf("分时窗口 %q 的起止时间不能相同（%s）", name, w.Start)
			}
			if w.Multiplier == nil && w.Override.empty() {
				return fmt.Errorf("分时窗口 %q 必须至少设置 multiplier 或 override", name)
			}
			if w.Multiplier != nil {
				if math.IsNaN(*w.Multiplier) || math.IsInf(*w.Multiplier, 0) {
					return fmt.Errorf("分时窗口 %q 的倍率无效（不能为 NaN 或 Inf）", name)
				}
				if *w.Multiplier < 0 {
					return fmt.Errorf("分时窗口 %q 的倍率是 %v：%w", name, *w.Multiplier, ErrNegativePrice)
				}
			}
			if w.Override != nil {
				if err := w.Override.validate(fmt.Sprintf("分时窗口 %q", name)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func parseHHMM(raw string, allow24 bool) (int, error) {
	s := strings.TrimSpace(raw)
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("格式必须为 HH:MM")
	}
	hh, err1 := strconv.Atoi(s[:2])
	mm, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("格式必须为 HH:MM")
	}
	if allow24 && hh == 24 && mm == 0 {
		return 24 * 60, nil
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("时间超出范围")
	}
	return hh*60 + mm, nil
}

type inputSegment struct {
	tierIndex int
	qty       int64
	unitPrice *float64
}

func applyPriceSet(eff *ModelPrices, cacheReadExplicit, cacheWriteExplicit *bool, ps PriceSet) {
	if ps.In != nil {
		eff.In = cloneFloatPtr(ps.In)
	}
	if ps.Out != nil {
		eff.Out = cloneFloatPtr(ps.Out)
	}
	if ps.CacheRead != nil {
		eff.CacheRead = cloneFloatPtr(ps.CacheRead)
		*cacheReadExplicit = true
	}
	if ps.CacheWrite != nil {
		eff.CacheWrite = cloneFloatPtr(ps.CacheWrite)
		*cacheWriteExplicit = true
	}
}

// CalculateCost 根据基础单价、计费规则与请求用量计算总花费及明细。
func CalculateCost(base ModelPrices, rule *PricingRule, in CostInput) (*CostBreakdown, error) {
	if rule != nil {
		if err := ValidatePricingRule(rule); err != nil {
			return nil, err
		}
	}

	eff := ModelPrices{
		In:         cloneFloatPtr(base.In),
		Out:        cloneFloatPtr(base.Out),
		CacheRead:  cloneFloatPtr(base.CacheRead),
		CacheWrite: cloneFloatPtr(base.CacheWrite),
		TTSChar:    cloneFloatPtr(base.TTSChar),
		AudioSec:   cloneFloatPtr(base.AudioSec),
	}
	cacheReadExplicit := eff.CacheRead != nil
	cacheWriteExplicit := eff.CacheWrite != nil

	bd := &CostBreakdown{
		TierIndex: -1,
	}

	var segments []inputSegment
	isProgressive := false

	if rule != nil && rule.Tiers != nil {
		mode := rule.Tiers.Mode
		if mode == "" {
			mode = TierModeAllIn
		}
		basis := rule.Tiers.Basis
		if basis == "" {
			basis = TierBasisTotalInput
		}
		if basis == TierBasisUncachedInput {
			bd.BasisTokens = in.InTokensUncached
		} else {
			bd.BasisTokens = in.InTokensUncached + in.CacheRead + in.CacheWrite
		}

		brackets := rule.Tiers.Brackets
		if mode == TierModeAllIn {
			for i, b := range brackets {
				if b.MaxTokens == nil || bd.BasisTokens <= *b.MaxTokens {
					bd.TierIndex = i
					bd.TierLabel = formatTierLabel(i, brackets)
					applyPriceSet(&eff, &cacheReadExplicit, &cacheWriteExplicit, b.Prices)
					break
				}
			}
		} else {
			isProgressive = true
			offset := int64(0)
			if basis == TierBasisTotalInput {
				offset = in.CacheRead + in.CacheWrite
			}
			end := offset + in.InTokensUncached
			prevMax := int64(0)
			for i, b := range brackets {
				curMax := int64(math.MaxInt64)
				if b.MaxTokens != nil {
					curMax = *b.MaxTokens
				}
				lo := offset
				if prevMax > lo {
					lo = prevMax
				}
				hi := end
				if curMax < hi {
					hi = curMax
				}
				if hi > lo {
					up := cloneFloatPtr(b.Prices.In)
					if up == nil {
						up = cloneFloatPtr(base.In)
					}
					segments = append(segments, inputSegment{
						tierIndex: i,
						qty:       hi - lo,
						unitPrice: up,
					})
				}
				prevMax = curMax
			}
			if end > prevMax {
				segments = append(segments, inputSegment{
					tierIndex: -1,
					qty:       end - prevMax,
					unitPrice: cloneFloatPtr(base.In),
				})
			}

			hitIdx := -1
			for i, b := range brackets {
				if b.MaxTokens == nil || bd.BasisTokens <= *b.MaxTokens {
					hitIdx = i
					break
				}
			}
			if hitIdx >= 0 {
				bd.TierIndex = hitIdx
				bd.TierLabel = formatTierLabel(hitIdx, brackets)
				applyPriceSet(&eff, &cacheReadExplicit, &cacheWriteExplicit, brackets[hitIdx].Prices)
			}
		}
	}

	if rule != nil && rule.Schedule != nil {
		if w := matchTimeWindow(rule.Schedule, in.TS); w != nil {
			bd.WindowName = w.Name
			if w.Override != nil {
				if w.Override.In != nil {
					eff.In = cloneFloatPtr(w.Override.In)
					for i := range segments {
						segments[i].unitPrice = cloneFloatPtr(w.Override.In)
					}
				}
				if w.Override.Out != nil {
					eff.Out = cloneFloatPtr(w.Override.Out)
				}
				if w.Override.CacheRead != nil {
					eff.CacheRead = cloneFloatPtr(w.Override.CacheRead)
					cacheReadExplicit = true
				}
				if w.Override.CacheWrite != nil {
					eff.CacheWrite = cloneFloatPtr(w.Override.CacheWrite)
					cacheWriteExplicit = true
				}
			}
			// 先补齐 CacheRead/CacheWrite 回退，再统一乘以倍率。
			if !cacheReadExplicit && eff.CacheRead == nil && eff.In != nil {
				eff.CacheRead = cloneFloatPtr(eff.In)
			}
			if !cacheWriteExplicit && eff.CacheWrite == nil && eff.In != nil {
				eff.CacheWrite = cloneFloatPtr(eff.In)
			}
			if w.Multiplier != nil {
				bd.Multiplier = cloneFloatPtr(w.Multiplier)
				m := *w.Multiplier
				scaleFloatPtr(&eff.In, m)
				scaleFloatPtr(&eff.Out, m)
				scaleFloatPtr(&eff.CacheRead, m)
				scaleFloatPtr(&eff.CacheWrite, m)
				scaleFloatPtr(&eff.TTSChar, m)
				scaleFloatPtr(&eff.AudioSec, m)
				for i := range segments {
					scaleFloatPtr(&segments[i].unitPrice, m)
				}
			}
		}
	}

	if !cacheReadExplicit && eff.CacheRead == nil && eff.In != nil {
		eff.CacheRead = cloneFloatPtr(eff.In)
	}
	if !cacheWriteExplicit && eff.CacheWrite == nil && eff.In != nil {
		eff.CacheWrite = cloneFloatPtr(eff.In)
	}

	bd.EffectivePrices = eff

	var total float64
	hasNonZeroUsage := false
	hasPricedNonZeroUsage := false
	hasAnyPrice := hasAnyModelPrice(eff)

	addItem := func(dim string, qty int64, unitPrice *float64, perMillion bool, tierIdx int) {
		var cost *float64
		if unitPrice != nil {
			hasAnyPrice = true
			var c float64
			if perMillion {
				c = float64(qty) * (*unitPrice) / 1e6
			} else {
				c = float64(qty) * (*unitPrice)
			}
			cost = &c
			if qty > 0 {
				hasPricedNonZeroUsage = true
				total += c
			}
		}
		if qty > 0 {
			hasNonZeroUsage = true
		}
		bd.Items = append(bd.Items, CostItem{
			Dimension: dim,
			Quantity:  qty,
			UnitPrice: cloneFloatPtr(unitPrice),
			Cost:      cost,
			TierIndex: tierIdx,
		})
	}

	if isProgressive && len(segments) > 0 {
		for _, seg := range segments {
			addItem("in", seg.qty, seg.unitPrice, true, seg.tierIndex)
		}
	} else {
		addItem("in", in.InTokensUncached, eff.In, true, bd.TierIndex)
	}
	addItem("cache_read", in.CacheRead, eff.CacheRead, true, -1)
	addItem("cache_write", in.CacheWrite, eff.CacheWrite, true, -1)
	addItem("out", in.OutTokens, eff.Out, true, bd.TierIndex)
	addItem("reasoning", in.ReasoningTokensExtra, eff.Out, true, bd.TierIndex)
	addItem("tts_char", in.TTSChars, eff.TTSChar, true, -1)
	addItem("audio_second", in.AudioSeconds, eff.AudioSec, false, -1)

	if hasNonZeroUsage {
		if hasPricedNonZeroUsage {
			bd.TotalCost = &total
		}
	} else if hasAnyPrice {
		zero := 0.0
		bd.TotalCost = &zero
	}

	return bd, nil
}

func formatTierLabel(idx int, brackets []PriceTier) string {
	if idx < 0 || idx >= len(brackets) {
		return ""
	}
	b := brackets[idx]
	if b.MaxTokens == nil {
		if idx == 0 {
			return "all"
		}
		return fmt.Sprintf(">%d", *brackets[idx-1].MaxTokens)
	}
	if idx == 0 {
		return fmt.Sprintf("0-%d", *b.MaxTokens)
	}
	return fmt.Sprintf("%d-%d", *brackets[idx-1].MaxTokens+1, *b.MaxTokens)
}

func matchTimeWindow(sched *TimeSchedulePricing, ts int64) *TimeWindow {
	if sched == nil || len(sched.Windows) == 0 {
		return nil
	}
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	tz := sched.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil
	}
	t := time.Unix(ts, 0).In(loc)
	weekday := int(t.Weekday())
	prevWeekday := (weekday + 6) % 7
	minuteOfDay := t.Hour()*60 + t.Minute()

	for i := range sched.Windows {
		w := &sched.Windows[i]
		startMin, err1 := parseHHMM(w.Start, false)
		endMin, err2 := parseHHMM(w.End, true)
		if err1 != nil || err2 != nil {
			continue
		}
		if startMin < endMin {
			if minuteOfDay >= startMin && minuteOfDay < endMin {
				if dayAllowed(w.Days, weekday) {
					return w
				}
			}
		} else {
			// 跨午夜窗口：前半段属于当天，后半段（00:00..End）属于前一天。
			if minuteOfDay >= startMin {
				if dayAllowed(w.Days, weekday) {
					return w
				}
			} else if minuteOfDay < endMin {
				if dayAllowed(w.Days, prevWeekday) {
					return w
				}
			}
		}
	}
	return nil
}

func dayAllowed(days []int, target int) bool {
	if len(days) == 0 {
		return true
	}
	for _, d := range days {
		if d == target {
			return true
		}
	}
	return false
}

func hasAnyModelPrice(p ModelPrices) bool {
	return p.In != nil || p.Out != nil || p.CacheRead != nil || p.CacheWrite != nil ||
		p.TTSChar != nil || p.AudioSec != nil
}

func cloneFloatPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func scaleFloatPtr(p **float64, factor float64) {
	if p == nil || *p == nil {
		return
	}
	v := (**p) * factor
	*p = &v
}

func marshalPricingRule(r *PricingRule) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalPricingRule(raw *string) *PricingRule {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var r PricingRule
	if err := json.Unmarshal([]byte(*raw), &r); err != nil {
		return nil
	}
	return &r
}
