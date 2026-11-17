package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lax/internal/account"
	"lax/internal/store"
)

// sleepContext 是默认的重试等待：定时器可被 ctx 取消，客户端断开后不再白等。
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// loadFailurePolicies 读一次启用的用户规则。读失败只打日志并返回 nil：
// account.MatchFailure 仍会用内置兜底，不因为策略表打不开就让客户端请求失败。
func (s *Server) loadFailurePolicies() []store.RouteFailurePolicy {
	load := s.failurePolicies
	if load == nil {
		load = s.store.ListRouteFailurePolicies
	}
	policies, err := load()
	if err != nil {
		s.logf("读取上游失败处理策略失败，本次只用内置规则: %v", err)
		return nil
	}
	return policies
}

// policyAttemptState 把命中规则的动作翻成一次尝试要落库的状态。
//
//   - Disable 永远是账号级，与规则有没有带 --model 无关；
//   - Cooldown 带模型时走模型级（state.Failure），否则走账号级（state.AccountCooldown）；
//   - 只带重试/换号/返回的规则不写状态。
//
// 截止时间取 s.now()，测试因此不依赖真实时钟。
func (s *Server) policyAttemptState(d account.FailureDecision, model, message string) store.AttemptState {
	var state store.AttemptState
	if d.Disable {
		state.Disable = true
		state.LastError = message
		return state
	}
	if d.Cooldown > 0 {
		state.CooldownUntilMS = s.now().Add(d.Cooldown).UnixMilli()
		state.LastError = message
		if d.ModelScopedCooldown && strings.TrimSpace(model) != "" {
			state.Failure = true
		} else {
			state.AccountCooldown = true
		}
	}
	return state
}

// failurePolicyLabel 是写进客户端错误与日志的规则称呼：内置规则用它的名字，
// 用户规则带上 id 与备注，排查时能直接去 `route policy show` 看这一条。
func failurePolicyLabel(d account.FailureDecision) string {
	if d.Source == account.FailureSourceBuiltin {
		if d.Name != "" {
			return d.Name
		}
		return "内置失败处理规则"
	}
	if d.Name != "" {
		return fmt.Sprintf("用户规则 #%d（%s）", d.PolicyID, d.Name)
	}
	return fmt.Sprintf("用户规则 #%d", d.PolicyID)
}

// matchedFailureMessage 是命中规则且要返回上游错误时给客户端的文案：
// 账号、命中的规则与上游原话三样都要有，否则用户看不出为什么这次没有换号。
func matchedFailureMessage(accountName string, d account.FailureDecision, upstream string) string {
	msg := fmt.Sprintf("账号 %s 命中%s", accountName, failurePolicyLabel(d))
	// 停用/冷却要写出来：客户端才知道这一次不只是「上游说不行」，账号状态也变了。
	// 「已停用」这四个字同时是旧 401 文案的兼容点（见 TestUnauthorizedDisablesAccount 与
	// gemini 的同类用例）。
	switch {
	case d.Disable:
		msg += "，已停用"
	case d.Cooldown > 0:
		msg += "，已冷却"
	}
	if strings.TrimSpace(upstream) != "" {
		msg += "：" + upstream
	}
	return msg
}

// probePolicyRef 把命中规则摊成 account test 输出用的来源、id 与名称。
func probePolicyRef(d account.FailureDecision) *ProbePolicy {
	name := d.Name
	if name == "" {
		if d.Source == account.FailureSourceBuiltin {
			name = "内置失败处理规则"
		} else {
			name = fmt.Sprintf("用户规则 #%d", d.PolicyID)
		}
	}
	return &ProbePolicy{Source: d.Source, ID: d.PolicyID, Name: name}
}

// failureActionText 是 account test 输出里的动作文案，例如「永久停用账号」或「账号冷却 30m」。
// 探活不会真的换号，所以只带换号/返回的规则写「不改状态」，避免让人以为探活会切账号。
func failureActionText(d account.FailureDecision, modelScoped bool) string {
	var parts []string
	if d.Disable {
		parts = append(parts, "永久停用账号")
	}
	if d.Cooldown > 0 {
		scope := "账号"
		if modelScoped {
			scope = "模型"
		}
		parts = append(parts, scope+"冷却 "+formatPolicyDuration(d.Cooldown))
	}
	if len(parts) == 0 {
		if d.NextRoute {
			return "只换号，不改状态"
		}
		return "只返回，不改状态"
	}
	return strings.Join(parts, "，")
}

// formatPolicyDuration 把规则时长写成人读的 30m / 1h / 90s。
func formatPolicyDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int(d/time.Second))
	default:
		return d.String()
	}
}

// applyProbeDecision 把 account test 命中的规则落库。停用永远账号级；冷却按规则是否
// 限定模型决定范围；只带换号/返回的规则什么都不写。
func (s *Server) applyProbeDecision(a store.Account, step ProbeStep, d account.FailureDecision) error {
	state := s.policyAttemptState(d, step.Model, step.Message)
	switch {
	case state.Disable:
		return s.store.SetEnabled(a.ID, false, step.Message)
	case state.Failure:
		return s.store.RecordFailure(a.ID, step.Model, state.CooldownUntilMS, step.Message)
	case state.AccountCooldown:
		// 只写账号冷却，不写 requests：account test 的探活结论不是客户端请求。
		return s.store.SetAccountCooldown(a.ID, state.CooldownUntilMS, step.Message)
	}
	return nil
}
