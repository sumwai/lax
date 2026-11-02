// Package account 实现账号池的纯逻辑：可用性判断、候选排序、冷却时长。
package account

import (
	"time"

	"lax/internal/store"
)

const (
	// CooldownRateLimit 是首次撞上 429 后的冷却时长。
	CooldownRateLimit = 500 * time.Millisecond
	// CooldownServer 是上游明确回了 5xx 后首次的冷却时长。
	CooldownServer = 10 * time.Second
	// CooldownTransport 是没有拿到任何 HTTP 响应时首次的冷却时长：连不上、TLS 握手失败、
	// 读超时、unexpected EOF 都算。这类失败多半来自本机网络或中转抖动，按上游 5xx 的时长
	// 处理会在断网恢复后仍把单账号网关锁住。
	CooldownTransport = 2 * time.Second
	// MaxCooldown 是冷却上限，三档相同。
	MaxCooldown = 30 * time.Minute
)

// FailureKind 是一次失败落在哪一档冷却。档位由失败的来源定，不由状态码反推：没有拿到 HTTP
// 响应的失败在 requests 里同样记 502，但与上游明确回了 5xx 是两回事：前者说明这台机器到上游的
// 路不通，后者说明上游自己有问题，两者的冷却时长因此差了五倍。
// 零值表示没有给出档位，Cooldown 按 KindServer 算，与只认状态码时的兜底一致。
type FailureKind int

const (
	// KindRateLimit 是上游回了 429：等一分钟再试。
	KindRateLimit FailureKind = iota + 1
	// KindServer 是上游明确回了 5xx：上游那头自己有问题，等十秒。
	KindServer
	// KindTransport 是一条 HTTP 响应都没拿到：请求没发出去、读响应体或读流之前就断了。
	KindTransport
)

// KindFromStatus 给拿到了状态的失败定档：429 是限流，其余 5xx 算服务端失败。
// 没有拿到状态码的失败不经过这里，调用方直接给 KindTransport。
func KindFromStatus(status int) FailureKind {
	if status == 429 {
		return KindRateLimit
	}
	return KindServer
}

// Cooldown 按失败档位与连续失败次数算冷却时长，失败次数从 0 开始。
// 三档同样翻倍、同样上限：档位只决定起始时长，连续失败时的退避曲线一致，
// 因此不会出现「连接层失败永远不翻倍」这种越等越久的分叉。
func Cooldown(kind FailureKind, streak int) time.Duration {
	base := CooldownServer
	switch kind {
	case KindRateLimit:
		base = CooldownRateLimit
	case KindTransport:
		base = CooldownTransport
	}
	if streak < 0 {
		streak = 0
	}
	d := base
	for i := 0; i < streak && d < MaxCooldown; i++ {
		d *= 2
	}
	if d > MaxCooldown {
		d = MaxCooldown
	}
	return d
}

// Retryable 判断上游状态是否值得换账号重试。
func Retryable(status int) bool {
	if status == 429 || status == 408 {
		return true
	}
	return status >= 500
}

// Supports 判断账号的模型列表是否覆盖请求的模型。列表为空表示该账号不提供任何模型：
// 刚建号、或 `model probe` 没探出可用模型的账号因此不参与选号，
// 否则它会接下任何模型名，把请求打到一家根本不提供该模型的上游。
func Supports(a store.Account, model string) bool {
	if len(a.Models) == 0 {
		return false
	}
	for _, m := range a.Models {
		if m == model {
			return true
		}
	}
	return false
}

// SettledFresh 判断一条探活结论是否还在新鲜期内，决定这一轮能否跳过重新探活。
// 只有有定论的状态（usable / forbidden / notfound）才可能新鲜：synced 从未探过，
// auth 与 error 是临时结论，都得重探。checked_at 为 0 视为没探过。
// 差值为负说明库里存的是未来时间戳（时钟回拨或别的机器写过库），这类行同样不算新鲜：
// 未来时间戳恒小于 maxAge，不排除的话它们会永远被跳过。
func SettledFresh(status string, checkedAt int64, now time.Time, maxAge time.Duration) bool {
	switch status {
	case store.ModelUsable, store.ModelForbidden, store.ModelNotFound:
	default:
		return false
	}
	if checkedAt <= 0 {
		return false
	}
	age := now.Sub(time.Unix(checkedAt, 0))
	return age >= 0 && age <= maxAge
}

// AvailableForModel 判断账号此刻能否接这个模型的请求：账号停用或这个账号-模型在冷却中都不行。
// 冷却按账号-模型隔离，同账号的其它模型不受影响（见 store.ModelFailure）。
func AvailableForModel(a store.Account, model string, now time.Time) bool {
	if !a.Enabled {
		return false
	}
	f := a.ModelFailures[model]
	if f.CooldownUntilMS == 0 {
		return true
	}
	return now.UnixMilli() >= f.CooldownUntilMS
}

// FailStreak 返回这个账号在这个模型上的连续失败次数，用于冷却翻倍。
func FailStreak(a store.Account, model string) int {
	return a.ModelFailures[model].FailStreak
}

// Providers 列出模型列表覆盖该模型的账号，不看 enabled 与冷却。
// 选号之外的判定用它区分两件事：没有任何账号提供这个模型（该报找不到），
// 与有账号提供但此刻都在冷却或被停用（那是可用性问题，不是清单问题）。
func Providers(accounts []store.Account, model string) []store.Account {
	var out []store.Account
	for _, a := range accounts {
		if Supports(a, model) {
			out = append(out, a)
		}
	}
	return out
}

// Candidates 是默认策略下的选号结果：priority 升序，会话粘性为 prefer。
// 新代码要按 route 策略选号时用 Rank；保留这个包装器给只关心候选集合的旧调用方。
func Candidates(accounts []store.Account, model string, now time.Time, session string) []store.Account {
	return Rank(RouteInput{
		Accounts: accounts,
		Model:    model,
		Now:      now,
		Policy:   store.DefaultRoutePolicy(),
		Session:  session,
	}).Candidates
}
