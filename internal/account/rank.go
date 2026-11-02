package account

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"time"

	"lax/internal/store"
)

// RouteInput 是一次选号需要的全部外部事实。account 包不读库，策略、权重与统计都由调用方传入。
type RouteInput struct {
	Accounts []store.Account
	Model    string
	Now      time.Time
	Policy   store.RoutePolicy
	Weights  map[int64]int
	Stats    map[int64]store.RouteStat
	Session  string
	// Sequence 是同一模型的请求序号，rotate 与 weighted 用它错开第一名。
	Sequence uint64
	// Seed 固定 softmax 的随机抽样，测试与 route explain 因此可复现。
	Seed int64
}

// RouteChoice 是一个候选账号的统计与得分。Choices 已按最终尝试顺序排列。
type RouteChoice struct {
	Account     store.Account
	Weight      int
	Attempts    int64
	SuccessRate float64
	// LatencySamples 是成功并带耗时的样本数；为 0 时 AvgLatencyMS 没有意义。
	LatencySamples int64
	AvgLatencyMS   float64
	Score          float64
	// Probability 只有 softmax 会填；其它策略是 nil。
	Probability *float64
}

// RouteExclusion 是一个被硬过滤挡掉的账号及原因。
type RouteExclusion struct {
	Account store.Account
	Reason  string
}

// RouteDecision 是一次选号的结果。
type RouteDecision struct {
	Candidates []store.Account
	Choices    []RouteChoice
	Excluded   []RouteExclusion
	// StickyAccountID 是会话哈希到的账号；会话为空或策略是 off 时为 0。
	StickyAccountID int64
	// StickyMiss 表示 strict 粘性命中的账号此刻不可用，调用方应直接拒绝而不换号。
	StickyMiss bool
}

// Rank 按策略返回候选顺序与解释数据。硬过滤只有三条：账号启用、声明了该模型、账号-模型未冷却。
func Rank(in RouteInput) RouteDecision {
	var decision RouteDecision
	available := make([]store.Account, 0, len(in.Accounts))
	for _, a := range in.Accounts {
		switch {
		case !a.Enabled:
			decision.Excluded = append(decision.Excluded, RouteExclusion{Account: a, Reason: "账号已停用"})
		case !Supports(a, in.Model):
			decision.Excluded = append(decision.Excluded, RouteExclusion{Account: a, Reason: "没有声明这个模型"})
		case !AvailableForModel(a, in.Model, in.Now):
			decision.Excluded = append(decision.Excluded, RouteExclusion{Account: a, Reason: "模型冷却中"})
		default:
			available = append(available, a)
		}
	}

	targetID, hasTarget := stickyTarget(in.Accounts, in.Model, in.Session)
	if in.Policy.Sticky != store.RouteStickyOff && hasTarget {
		decision.StickyAccountID = targetID
	}

	if in.Policy.Sticky == store.RouteStickyStrict && hasTarget {
		only := available[:0]
		for _, a := range available {
			if a.ID == targetID {
				only = append(only, a)
			}
		}
		if len(only) == 0 {
			decision.StickyMiss = true
			return decision
		}
		available = only
	}

	ordered := strategyOrder(available, in.Policy, in.Weights, in.Stats, in.Sequence)
	if in.Policy.Sticky == store.RouteStickyPrefer && hasTarget && len(ordered) > 1 {
		for i, a := range ordered {
			if a.ID == targetID {
				copy(ordered[1:i+1], ordered[:i])
				ordered[0] = a
				break
			}
		}
	}

	choices := scoreChoices(ordered, in.Policy, in.Weights, in.Stats)
	if in.Policy.Strategy == store.RouteStrategySoftmax && len(choices) > 0 {
		choices = softmaxOrder(choices, in.Policy.Temperature, in.Policy.Explore, in.Seed)
	}
	decision.Choices = choices
	decision.Candidates = make([]store.Account, len(choices))
	for i := range choices {
		decision.Candidates[i] = choices[i].Account
	}
	return decision
}

// stickyTarget 在「提供该模型的全部账号」上算哈希，不看启停与冷却。
// 候选集合变化时，同一会话仍指向同一个账号，不会无故换号。
func stickyTarget(accounts []store.Account, model, session string) (int64, bool) {
	if session == "" {
		return 0, false
	}
	providers := Providers(accounts, model)
	if len(providers) == 0 {
		return 0, false
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	h := fnv.New32a()
	_, _ = h.Write([]byte(session))
	return providers[int(h.Sum32()%uint32(len(providers)))].ID, true
}

func strategyOrder(accounts []store.Account, policy store.RoutePolicy,
	weights map[int64]int, stats map[int64]store.RouteStat, sequence uint64) []store.Account {

	out := append([]store.Account(nil), accounts...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	if len(out) < 2 {
		return out
	}

	switch policy.Strategy {
	case store.RouteStrategyRotate:
		shift := int(sequence % uint64(len(out)))
		return append(out[shift:], out[:shift]...)
	case store.RouteStrategyWeighted:
		expanded := make([]int64, 0, len(out))
		for _, a := range out {
			w := routeWeight(a.ID, weights)
			for i := 0; i < w; i++ {
				expanded = append(expanded, a.ID)
			}
		}
		if len(expanded) == 0 {
			return out
		}
		firstID := expanded[int(sequence%uint64(len(expanded)))]
		weighted := make([]store.Account, 0, len(out))
		for _, a := range out {
			if a.ID == firstID {
				weighted = append(weighted, a)
				break
			}
		}
		for _, a := range out {
			if a.ID != firstID {
				weighted = append(weighted, a)
			}
		}
		return weighted
	case store.RouteStrategySuccess:
		sort.SliceStable(out, func(i, j int) bool {
			left := successRate(stats[out[i].ID], policy.MinSamples)
			right := successRate(stats[out[j].ID], policy.MinSamples)
			if left != right {
				return left > right
			}
			if out[i].Priority != out[j].Priority {
				return out[i].Priority < out[j].Priority
			}
			return out[i].ID < out[j].ID
		})
	}
	return out
}

func scoreChoices(accounts []store.Account, policy store.RoutePolicy,
	weights map[int64]int, stats map[int64]store.RouteStat) []RouteChoice {

	if len(accounts) == 0 {
		return nil
	}
	minPriority, maxPriority := accounts[0].Priority, accounts[0].Priority
	for _, a := range accounts[1:] {
		if a.Priority < minPriority {
			minPriority = a.Priority
		}
		if a.Priority > maxPriority {
			maxPriority = a.Priority
		}
	}
	weightSum := policy.PriorityWeight + policy.SuccessWeight + policy.LatencyWeight
	if weightSum <= 0 {
		weightSum = 1
	}
	choices := make([]RouteChoice, 0, len(accounts))
	for _, a := range accounts {
		stat := stats[a.ID]
		rate := successRate(stat, policy.MinSamples)
		latencyMS, latencyPart := latencyScore(stat)
		priorityScore := 1.0
		if maxPriority > minPriority {
			priorityScore = float64(maxPriority-a.Priority) / float64(maxPriority-minPriority)
		}
		score := (priorityScore*policy.PriorityWeight +
			rate*policy.SuccessWeight +
			latencyPart*policy.LatencyWeight) / weightSum
		choices = append(choices, RouteChoice{
			Account:        a,
			Weight:         routeWeight(a.ID, weights),
			Attempts:       stat.Attempts,
			SuccessRate:    rate,
			LatencySamples: stat.LatencyCount,
			AvgLatencyMS:   latencyMS,
			Score:          score,
		})
	}
	return choices
}

func successRate(stat store.RouteStat, minSamples int) float64 {
	if minSamples < 1 {
		minSamples = 1
	}
	return (float64(stat.Successes) + 0.5*float64(minSamples)) /
		(float64(stat.Attempts) + float64(minSamples))
}

func latencyScore(stat store.RouteStat) (float64, float64) {
	if stat.LatencyCount == 0 {
		return 0, 0.5
	}
	avg := float64(stat.LatencySumMS) / float64(stat.LatencyCount)
	if avg >= 10000 {
		return avg, 0
	}
	if avg < 0 {
		return 0, 1
	}
	return avg, 1 - avg/10000
}

func routeWeight(id int64, weights map[int64]int) int {
	if w := weights[id]; w > 0 {
		return w
	}
	return 1
}

func softmaxOrder(choices []RouteChoice, temperature, explore float64, seed int64) []RouteChoice {
	if len(choices) == 0 {
		return choices
	}
	maxScore := choices[0].Score
	for _, c := range choices[1:] {
		if c.Score > maxScore {
			maxScore = c.Score
		}
	}
	if temperature <= 0 {
		temperature = 0.1
	}
	q := make([]float64, len(choices))
	sumQ := 0.0
	for i, c := range choices {
		q[i] = float64(c.Weight) * math.Exp((c.Score-maxScore)/temperature)
		sumQ += q[i]
	}
	explore = math.Max(0, math.Min(1, explore))
	probs := make([]float64, len(choices))
	for i := range choices {
		if sumQ > 0 {
			probs[i] = (1-explore)*q[i]/sumQ + explore/float64(len(choices))
		} else {
			probs[i] = 1 / float64(len(choices))
		}
		p := probs[i]
		choices[i].Probability = &p
	}

	rng := rand.New(rand.NewSource(seed))
	remaining := make([]int, len(choices))
	for i := range remaining {
		remaining[i] = i
	}
	out := make([]RouteChoice, 0, len(choices))
	for len(remaining) > 0 {
		total := 0.0
		for _, idx := range remaining {
			total += probs[idx]
		}
		pick := 0
		if total > 0 {
			r := rng.Float64() * total
			acc := 0.0
			for i, idx := range remaining {
				acc += probs[idx]
				if r < acc {
					pick = i
					break
				}
				pick = i
			}
		}
		chosen := remaining[pick]
		out = append(out, choices[chosen])
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

// ExplainReason 给 route explain 用：把一次选号结果压成一行说明。
func ExplainReason(d RouteDecision) string {
	if d.StickyMiss {
		return fmt.Sprintf("会话固定到账号 %d，但它此刻不可用；strict 粘性不会换号", d.StickyAccountID)
	}
	return ""
}
