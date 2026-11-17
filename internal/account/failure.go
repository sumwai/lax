package account

import (
	"regexp"
	"strings"
	"time"

	"lax/internal/store"
)

const (
	// MaxRetryCount 是一条规则允许的同账号最大重试次数（额外调用次数）。
	MaxRetryCount = 10
	// MaxRetryWait 是单次重试等待的上限。
	MaxRetryWait = 8 * time.Second
	// MaxTotalRetryWait 是一个客户端请求在一个候选账号上累计等待的上限。
	MaxTotalRetryWait = 60 * time.Second
	// DefaultRetryBase 是规则没写 retry_base_ms 时的首次等待。
	DefaultRetryBase = 500 * time.Millisecond

	// FailureSourceUser 表示命中的是库里的用户规则。
	FailureSourceUser = "user"
	// FailureSourceBuiltin 表示命中的是代码里的内置兜底。
	FailureSourceBuiltin = "builtin"
)

// FailureFacts 是一次上游失败的全部匹配事实，由网关或探活侧填。
//
// Transport 表示一条 HTTP 响应都没拿到。它不是单独的条件：这类失败的状态码已按约定记
// 502，所以 --status 5xx 的规则能匹配它；这里保留字段只是让调用方与日志能区分来源。
type FailureFacts struct {
	Status    int
	Message   string
	Provider  string
	Model     string
	AccountID int64
	Transport bool
}

// FailureDecision 是命中规则后的完整动作。规则是整条替换语义，没写的动作不做。
type FailureDecision struct {
	PolicyID    int64
	Source      string // FailureSourceUser 或 FailureSourceBuiltin
	Name        string // 内置规则名或用户备注
	RetryCount  int
	RetryBase   time.Duration
	Cooldown    time.Duration
	Disable     bool
	NextRoute   bool
	ReturnError bool
	// ModelScopedCooldown 为真表示只冷却账号-模型（规则带了 --model），否则冷却整个账号。
	ModelScopedCooldown bool
	Note                string
}

// MatchFailure 按顺序匹配用户规则，未命中再看内置兜底；第一条命中即生效。
// policies 由 store.ListRouteFailurePolicies 提供，已按 priority ASC、id ASC 排好。
// 没命中返回 false，调用方继续走原有的默认处理。
func MatchFailure(facts FailureFacts, policies []store.RouteFailurePolicy) (FailureDecision, bool) {
	for _, p := range policies {
		if !p.Enabled || !failurePolicyMatches(p, facts) {
			continue
		}
		return decisionFromPolicy(p, FailureSourceUser), true
	}
	for _, p := range builtinFailurePolicies {
		if !failurePolicyMatches(p, facts) {
			continue
		}
		return decisionFromPolicy(p, FailureSourceBuiltin), true
	}
	return FailureDecision{}, false
}

// failurePolicyMatches 判断一条规则的条件是否全部满足。
// 不同条件之间是 AND；同一个条件里的多个状态或多个错误文本是 OR。
func failurePolicyMatches(p store.RouteFailurePolicy, facts FailureFacts) bool {
	if p.AccountID != 0 && p.AccountID != facts.AccountID {
		return false
	}
	if p.Provider != "" && p.Provider != facts.Provider {
		return false
	}
	if p.Model != "" && p.Model != facts.Model {
		return false
	}
	if !store.StatusTokenMatches(p.Statuses, facts.Status) {
		return false
	}
	if len(p.ErrorContains) > 0 {
		message := strings.ToLower(facts.Message)
		matched := false
		for _, want := range p.ErrorContains {
			if strings.Contains(message, strings.ToLower(want)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if p.ErrorRegex != "" {
		re, err := regexp.Compile(p.ErrorRegex)
		if err != nil || !re.MatchString(facts.Message) {
			return false
		}
	}
	return true
}

// decisionFromPolicy 把规则翻译成运行期动作。store 的写入口已经校验过范围，
// 这里再夹一次，是为了让手工构造或旧库里的异常行不会把重试次数与等待放大。
func decisionFromPolicy(p store.RouteFailurePolicy, source string) FailureDecision {
	retry := p.RetryCount
	if retry < 0 {
		retry = 0
	}
	if retry > MaxRetryCount {
		retry = MaxRetryCount
	}
	base := time.Duration(p.RetryBaseMS) * time.Millisecond
	if base <= 0 {
		base = DefaultRetryBase
	}
	cooldown := time.Duration(p.CooldownSeconds) * time.Second
	if cooldown < 0 {
		cooldown = 0
	}

	nextRoute, returnError := p.NextRoute, p.ReturnError
	// 两个流程位同时为真是无效行，校验与落库都会拒绝。真遇到时按「返回」处理：
	// 少打一次上游比多打一次安全。两者都没写时按 spec 的默认流程返回。
	if returnError {
		nextRoute = false
	}
	if !nextRoute && !returnError {
		returnError = true
	}

	return FailureDecision{
		PolicyID:            p.ID,
		Source:              source,
		Name:                p.Note,
		RetryCount:          retry,
		RetryBase:           base,
		Cooldown:            cooldown,
		Disable:             p.Disable,
		NextRoute:           nextRoute,
		ReturnError:         returnError,
		ModelScopedCooldown: cooldown > 0 && p.Model != "",
		Note:                p.Note,
	}
}

// RetryDelay 返回第 retryIndex 次重试前的等待（retryIndex 从 1 开始）：
// min(base × 2^(retryIndex-1), MaxRetryWait)。base 非正时用 DefaultRetryBase。
func RetryDelay(base time.Duration, retryIndex int) time.Duration {
	if retryIndex < 1 {
		retryIndex = 1
	}
	if base <= 0 {
		base = DefaultRetryBase
	}
	d := base
	for i := 1; i < retryIndex; i++ {
		if d >= MaxRetryWait {
			return MaxRetryWait
		}
		d *= 2
	}
	if d > MaxRetryWait {
		return MaxRetryWait
	}
	return d
}

// InAccountCooldown 判断账号是否处在账号级冷却中。截止时间等于此刻算已结束，
// 与 AvailableForModel 对模型冷却的判定一致。
func InAccountCooldown(a store.Account, now time.Time) bool {
	return a.CooldownUntilMS > 0 && now.UnixMilli() < a.CooldownUntilMS
}

// billingKeywords 是内置兜底用的窄计费关键词，全部小写。不要加裸 credit 或裸 quota：
// 那会把正常报错也当成计费问题。保持在一处，内置规则与测试都从这里取。
var billingKeywords = []string{
	"insufficient credit",
	"no credits",
	"out of credits",
	"credit balance",
	"purchase more credits",
	"payment required",
	"insufficient_quota",
	"余额不足",
	"欠费",
	"额度不足",
	"额度已用完",
	"充值",
}

// builtinFailurePolicies 是代码里的兜底规则，按切片顺序匹配。
// 条件与用户规则同形，CLI 的 ls 与 test --explain 可以直接把 Note 当规则名展示。
var builtinFailurePolicies = []store.RouteFailurePolicy{
	{
		Enabled:     true,
		Statuses:    []string{"401"},
		Disable:     true,
		ReturnError: true,
		RetryBaseMS: 500,
		Note:        "内置：401 停用并返回",
	},
	{
		Enabled:     true,
		Statuses:    []string{"402"},
		Disable:     true,
		NextRoute:   true,
		RetryBaseMS: 500,
		Note:        "内置：402 停用并换号",
	},
	{
		Enabled:       true,
		Statuses:      []string{"400", "403"},
		ErrorContains: billingKeywords,
		Disable:       true,
		NextRoute:     true,
		RetryBaseMS:   500,
		Note:          "内置：计费类 400/403 停用并换号",
	},
}

// BuiltinFailurePolicies 返回内置兜底规则的副本，供 CLI 的 ls 与 test 展示。
// 内置规则不在数据库里，rm 删不掉；切片顺序就是匹配顺序。
func BuiltinFailurePolicies() []store.RouteFailurePolicy {
	out := make([]store.RouteFailurePolicy, len(builtinFailurePolicies))
	for i, p := range builtinFailurePolicies {
		out[i] = p
		out[i].Statuses = append([]string(nil), p.Statuses...)
		out[i].ErrorContains = append([]string(nil), p.ErrorContains...)
	}
	return out
}
