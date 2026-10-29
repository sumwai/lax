// Package account 实现账号池的纯逻辑：可用性判断、候选排序、冷却时长。
package account

import (
	"hash/fnv"
	"sort"
	"time"

	"switchboard/internal/store"
)

const (
	// CooldownRateLimit 是首次撞上 429 后的冷却时长。
	CooldownRateLimit = time.Minute
	// CooldownServer 是首次撞上 5xx 或超时后的冷却时长。
	CooldownServer = 10 * time.Second
	// MaxCooldown 是冷却上限。
	MaxCooldown = 30 * time.Minute
)

// Cooldown 按连续失败次数算冷却时长，失败次数从 0 开始。
func Cooldown(status int, streak int) time.Duration {
	base := CooldownServer
	if status == 429 {
		base = CooldownRateLimit
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

// Available 判断账号此刻能否接请求。
func Available(a store.Account, now time.Time) bool {
	if !a.Enabled {
		return false
	}
	if a.CooldownUntil == 0 {
		return true
	}
	return now.Unix() >= a.CooldownUntil
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

// Candidates 过滤出可用于该模型的账号，按 priority 升序排好。
// session 非空时，把按会话哈希选中的账号提到最前，保证同一会话粘同一个账号。
func Candidates(accounts []store.Account, model string, now time.Time, session string) []store.Account {
	var out []store.Account
	for _, a := range accounts {
		if !Available(a, now) || !Supports(a, model) {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})

	if session != "" && len(out) > 1 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(session))
		idx := int(h.Sum32() % uint32(len(out)))
		out = append(out[idx:], out[:idx]...)
	}
	return out
}
