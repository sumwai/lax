package account

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"lax/internal/quota"
	"lax/internal/store"
)

// quotaExclusionReasonPrefix 是所有额度排除原因的统一前缀。网关靠它区分
// 「额度过滤后没有候选」与「账号都在冷却/被停用」，两种 503 的下一步命令不同。
const quotaExclusionReasonPrefix = "额度："

// IsQuotaExclusionReason 判断一条排除原因是不是额度过滤造成的。
func IsQuotaExclusionReason(reason string) bool {
	return strings.HasPrefix(reason, quotaExclusionReasonPrefix)
}

// quotaEntryFor 返回账号的额度事实。监控来源为 off 时按没有数据处理：
// 用户已经关闭监控，旧的余额与到期时间不能再参与过滤或排序。
// store.QuotaEntries 已经过滤过一层，这里再做一次，防止其它调用方直接把原始条目传进来。
func quotaEntryFor(quotas map[int64]quota.Entry, accountID int64) (quota.Entry, bool) {
	entry, ok := quotas[accountID]
	if !ok || entry.Settings.Source == quota.SourceOff {
		return quota.Entry{}, false
	}
	return entry, true
}

// quotaFilter 在现有三条硬过滤之后按额度再做一层过滤。
//
// 只排除「已知」的坏情况：额度从未刷新、汇率未知、余额未知的账号一律保留，
// 否则默认关闭的刷新会让一次误开的过滤把所有账号都误伤。返回保留的账号与排除原因。
func quotaFilter(accounts []store.Account, quotas map[int64]quota.Entry,
	policy store.RoutePolicy, now time.Time) ([]store.Account, []RouteExclusion) {

	if len(accounts) == 0 {
		return accounts, nil
	}
	kept := make([]store.Account, 0, len(accounts))
	var excluded []RouteExclusion
	for _, a := range accounts {
		entry, ok := quotaEntryFor(quotas, a.ID)
		if !ok {
			kept = append(kept, a)
			continue
		}
		if policy.QuotaMinUSD > 0 {
			if usd := entry.Facts.BalanceUSD; usd != nil && *usd < policy.QuotaMinUSD {
				excluded = append(excluded, RouteExclusion{
					Account: a,
					Reason: fmt.Sprintf("%s折合余额 %s USD 低于最低阈值 %s USD",
						quotaExclusionReasonPrefix, quotaNumberText(*usd), quotaNumberText(policy.QuotaMinUSD)),
				})
				continue
			}
		}
		state, action := quota.ExpiryAt(entry.Facts.PeriodEnd, now, entry.Settings)
		if action == quota.ExpiryModeExclude && state != "" {
			stateText := "已到期"
			if state == quota.ExpiryNear {
				days := entry.Settings.ExpiryDays
				if days <= 0 {
					days = quota.DefaultExpiryDays
				}
				stateText = fmt.Sprintf("%d 天内到期", days)
			}
			excluded = append(excluded, RouteExclusion{
				Account: a,
				Reason:  quotaExclusionReasonPrefix + stateText + "，到期处理为 exclude",
			})
			continue
		}
		kept = append(kept, a)
	}
	return kept, excluded
}

// quotaOrder 按额度设置做最终稳定排序。
//
// order=off 且没有 deprioritize 账号时原样返回，空 Quotas（或所有账号都没有快照）
// 因此与旧版输出逐项一致。expiry_mode=deprioritize 的账号整体放到末尾；同一组内
// 按配置的额度键排序，键相同或未知时保持传入顺序（策略、粘性、softmax 的顺序）。
func quotaOrder(choices []RouteChoice, quotas map[int64]quota.Entry,
	policy store.RoutePolicy, now time.Time) []RouteChoice {

	if len(choices) < 2 {
		return choices
	}
	order := policy.QuotaOrder
	if order == "" {
		order = quota.OrderOff
	}
	deprioritized := false
	for _, c := range choices {
		if quotaDeprioritized(c, quotas, now) {
			deprioritized = true
			break
		}
	}
	if order == quota.OrderOff && !deprioritized {
		return choices
	}
	out := append([]RouteChoice(nil), choices...)
	sort.SliceStable(out, func(i, j int) bool {
		left, right := quotaDeprioritized(out[i], quotas, now), quotaDeprioritized(out[j], quotas, now)
		if left != right {
			return !left
		}
		if order == quota.OrderOff {
			return false
		}
		return quotaOrderLess(out[i], out[j], quotas, order)
	})
	return out
}

// quotaDeprioritized 判断一个候选是否因为临近/已到期被设置成 deprioritize。
func quotaDeprioritized(c RouteChoice, quotas map[int64]quota.Entry, now time.Time) bool {
	entry, ok := quotaEntryFor(quotas, c.Account.ID)
	if !ok {
		return false
	}
	state, action := quota.ExpiryAt(entry.Facts.PeriodEnd, now, entry.Settings)
	return state != "" && action == quota.ExpiryModeDeprioritize
}

// quotaOrderLess 返回 a 是否应排在 b 前面。未知值一律排在已知值后面；
// 两端都未知或键相同返回 false，由 SliceStable 保留原有顺序。
func quotaOrderLess(a, b RouteChoice, quotas map[int64]quota.Entry, order string) bool {
	entryA, hasA := quotaEntryFor(quotas, a.Account.ID)
	entryB, hasB := quotaEntryFor(quotas, b.Account.ID)
	switch order {
	case quota.OrderExpiryAsc, quota.OrderExpiryDesc:
		valueA, knownA := quotaPeriodEnd(entryA, hasA)
		valueB, knownB := quotaPeriodEnd(entryB, hasB)
		if knownA != knownB {
			return knownA
		}
		if !knownA {
			return false
		}
		if order == quota.OrderExpiryAsc {
			return valueA < valueB
		}
		return valueA > valueB
	case quota.OrderRatioDesc, quota.OrderRatioAsc:
		valueA, knownA := quotaRemainingRatio(entryA, hasA)
		valueB, knownB := quotaRemainingRatio(entryB, hasB)
		if knownA != knownB {
			return knownA
		}
		if !knownA {
			return false
		}
		if order == quota.OrderRatioDesc {
			return valueA > valueB
		}
		return valueA < valueB
	case quota.OrderUSDDesc, quota.OrderUSDAsc:
		valueA, knownA := quotaBalanceUSD(entryA, hasA)
		valueB, knownB := quotaBalanceUSD(entryB, hasB)
		if knownA != knownB {
			return knownA
		}
		if !knownA {
			return false
		}
		if order == quota.OrderUSDDesc {
			return valueA > valueB
		}
		return valueA < valueB
	}
	return false
}

func quotaPeriodEnd(entry quota.Entry, ok bool) (int64, bool) {
	if !ok || entry.Facts.PeriodEnd <= 0 {
		return 0, false
	}
	return entry.Facts.PeriodEnd, true
}

func quotaRemainingRatio(entry quota.Entry, ok bool) (float64, bool) {
	if !ok || entry.Facts.RemainingRatio == nil {
		return 0, false
	}
	return *entry.Facts.RemainingRatio, true
}

func quotaBalanceUSD(entry quota.Entry, ok bool) (float64, bool) {
	if !ok || entry.Facts.BalanceUSD == nil {
		return 0, false
	}
	return *entry.Facts.BalanceUSD, true
}

// quotaNumberText 把金额原样转成短文本，不补多余的小数位。
func quotaNumberText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
