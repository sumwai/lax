package cli

import (
	"testing"

	"lax/internal/quota"
)

func mergeTestFloat(v float64) *float64 { return &v }
func mergeTestInt(v int64) *int64       { return &v }

// TestMergeQuotaFactsKeepsOldUsageWhenNewResponseIsEmpty 钉住「缺字段不是清零」：
// 订阅端点报错、usage 端点 200 但 summary 为空时，上一次已知的周期、已用与用量都要保留。
func TestMergeQuotaFactsKeepsOldUsageWhenNewResponseIsEmpty(t *testing.T) {
	prev := quota.Facts{
		Balance: 24.6301, BalanceKnown: true, Currency: "USD",
		Used: 45.33, UsedKnown: true,
		Usage:     quota.UpstreamUsage{TotalCredits: mergeTestFloat(45.33), Requests: mergeTestInt(123)},
		PeriodEnd: 1764547200000, PlanID: "pro", PlanStatus: "active",
		FiveHour: &quota.Window{Used: mergeTestFloat(2), Limit: mergeTestFloat(10), ResetAt: 1760000000000},
	}
	next := quota.Facts{
		Status:  quota.StatusPartial,
		Balance: 24.6301, BalanceKnown: true, Currency: "USD",
		SourceErrors: map[string]string{"subscriptions": "subscriptions 响应里没有已知字段"},
	}

	got := mergeQuotaFacts(prev, next, quota.DefaultSettings())
	if got.PeriodEnd != prev.PeriodEnd || got.PlanID != "pro" || got.PlanStatus != "active" {
		t.Fatalf("订阅失败后周期/套餐被清空: %+v", got)
	}
	if got.FiveHour == nil || got.FiveHour.ResetAt != 1760000000000 {
		t.Fatalf("订阅失败后限流窗口被清空: %+v", got.FiveHour)
	}
	if !got.UsedKnown || got.Used != 45.33 {
		t.Fatalf("空 usage 响应把旧已用清掉了: %+v", got)
	}
	if got.Usage.TotalCredits == nil || *got.Usage.TotalCredits != 45.33 {
		t.Fatalf("空 usage 响应把旧 totalCredits 清掉了: %+v", got.Usage)
	}
	if got.Usage.Requests == nil || *got.Usage.Requests != 123 {
		t.Fatalf("空 usage 响应把旧请求数清掉了: %+v", got.Usage)
	}
}
