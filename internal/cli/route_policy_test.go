package cli_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// routeFailurePolicyOut 是 route policy --json 的输出形状，字段与 store 的规则一一对应。
type routeFailurePolicyOut struct {
	ID              int64    `json:"id"`
	Source          string   `json:"source"`
	Enabled         bool     `json:"enabled"`
	Priority        int      `json:"priority"`
	AccountID       int64    `json:"account_id"`
	Account         string   `json:"account"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Statuses        []string `json:"statuses"`
	ErrorContains   []string `json:"error_contains"`
	ErrorRegex      string   `json:"error_regex"`
	Retry           int      `json:"retry"`
	RetryBaseMS     int64    `json:"retry_base_ms"`
	CooldownSeconds int64    `json:"cooldown_seconds"`
	Disable         bool     `json:"disable"`
	NextRoute       bool     `json:"next_route"`
	ReturnError     bool     `json:"return_error"`
	Note            string   `json:"note"`
	CreatedAt       int64    `json:"created_at"`
	UpdatedAt       int64    `json:"updated_at"`
}

type routeFailurePolicyListOut struct {
	Policies []routeFailurePolicyOut `json:"policies"`
	Builtins []routeFailurePolicyOut `json:"builtins"`
}

func mustRouteFailurePolicy(t *testing.T, out string) routeFailurePolicyOut {
	t.Helper()
	var p routeFailurePolicyOut
	mustJSON(t, out, &p)
	if p.ID <= 0 {
		t.Fatalf("策略 id = %d，期望正整数；输出 %s", p.ID, out)
	}
	return p
}

func mustRouteFailurePolicyList(t *testing.T, out string) routeFailurePolicyListOut {
	t.Helper()
	var list routeFailurePolicyListOut
	mustJSON(t, out, &list)
	if len(list.Builtins) != 3 {
		t.Fatalf("内置兜底 = %+v，期望 3 条", list.Builtins)
	}
	for _, p := range list.Builtins {
		if p.Source != "builtin" || p.ID != 0 {
			t.Fatalf("内置兜底来源/ID = %q / %d，期望 builtin / 0", p.Source, p.ID)
		}
	}
	return list
}

func findRouteFailurePolicy(t *testing.T, list []routeFailurePolicyOut, id int64) routeFailurePolicyOut {
	t.Helper()
	for _, p := range list {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("列表里没有策略 #%d：%+v", id, list)
	return routeFailurePolicyOut{}
}

func TestRoutePolicyAddLsShowJSON(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "elsa", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-elsa", "--models", "m1"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "alias", "set", "fast", "m1"); code != 0 {
		t.Fatalf("建别名退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "route", "policy", "add",
		"--account", "elsa",
		"--provider", "route-test",
		"--model", "fast",
		"--status", "5xx, 429", "--status", "400",
		"--error-contains", "Rate Limit", "--error-contains", "timeout",
		"--error-regex", "(?i)limit",
		"--retry", "2", "--retry-base", "1s",
		"--next_route", "--priority", "7", "--note", "计费重试", "--json")
	if code != 0 {
		t.Fatalf("route policy add 退出码 = %d，stderr = %s", code, stderr)
	}
	p := mustRouteFailurePolicy(t, out)
	if p.Source != "user" || !p.Enabled || p.Priority != 7 || p.Account != "elsa" || p.AccountID == 0 ||
		p.Provider != "route-test" || p.Model != "m1" {
		t.Fatalf("新建策略 = %+v", p)
	}
	if strings.Join(p.Statuses, ",") != "5xx,429,400" {
		t.Fatalf("状态解析 = %v，期望去空白并保持输入顺序", p.Statuses)
	}
	if strings.Join(p.ErrorContains, ",") != "Rate Limit,timeout" || p.ErrorRegex != "(?i)limit" {
		t.Fatalf("错误文本条件 = %v / %q", p.ErrorContains, p.ErrorRegex)
	}
	if p.Retry != 2 || p.RetryBaseMS != 1000 || !p.NextRoute || p.ReturnError || p.CooldownSeconds != 0 || p.Disable {
		t.Fatalf("动作为 = %+v", p)
	}
	if p.Note != "计费重试" || p.CreatedAt == 0 || p.UpdatedAt == 0 {
		t.Fatalf("备注/时间 = %+v", p)
	}

	code, out, stderr = run(t, "route", "policy", "show", fmt.Sprint(p.ID), "--json")
	if code != 0 {
		t.Fatalf("route policy show --json 退出码 = %d，stderr = %s", code, stderr)
	}
	shown := mustRouteFailurePolicy(t, out)
	if shown.ID != p.ID || shown.Note != p.Note || strings.Join(shown.Statuses, ",") != "5xx,429,400" {
		t.Fatalf("show = %+v，期望与 add 一致", shown)
	}

	code, out, stderr = run(t, "route", "policy", "show", fmt.Sprint(p.ID))
	if code != 0 {
		t.Fatalf("route policy show 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"账号", "elsa", "厂商", "route-test", "模型", "m1", "状态",
		"5xx", "错误包含", "Rate Limit", "(?i)limit", "重试", "额外 2 次", "重试起始等待", "1s",
		"流程", "换下一个候选账号", "计费重试", "创建时间"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show 输出缺 %q:\n%s", want, out)
		}
	}

	code, out, stderr = run(t, "route", "policy", "ls", "--json")
	if code != 0 {
		t.Fatalf("route policy ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	list := mustRouteFailurePolicyList(t, out)
	if len(list.Policies) != 1 || list.Policies[0].ID != p.ID {
		t.Fatalf("用户规则 = %+v，期望只有 #%d", list.Policies, p.ID)
	}
	if !strings.Contains(list.Builtins[0].Note, "401") || list.Builtins[0].Statuses[0] != "401" {
		t.Fatalf("第一条内置兜底 = %+v，期望 401", list.Builtins[0])
	}

	code, out, stderr = run(t, "route", "policy", "ls")
	if code != 0 {
		t.Fatalf("route policy ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"ID", "来源", "启用", "优先级", "条件", "动作", "备注",
		"用户", "内置", "计费重试", "重试 2 次", "换号", "内置：401 停用并返回"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls 输出缺 %q:\n%s", want, out)
		}
	}

	code, out, stderr = run(t, "route", "policy", "show", "0")
	if code != 2 || out != "" || !strings.Contains(stderr, "内置") {
		t.Fatalf("show 0 应退出 2、不写 stdout 并说明内置规则没有 id，实际 %d / %q / %q", code, out, stderr)
	}
	code, out, stderr = run(t, "route", "policy", "show", "999", "--json")
	if code != 1 || out != "" || !strings.Contains(stderr, "lax route policy ls") {
		t.Fatalf("show 不存在的 id = %d / %q / %q，期望 1 且提示 ls", code, out, stderr)
	}
}

func TestRoutePolicySetPatchSemantics(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "route", "policy", "add",
		"--all-errors", "--disable", "--return", "--priority", "100", "--note", "base", "--json")
	if code != 0 {
		t.Fatalf("建基础规则退出码 = %d，stderr = %s", code, stderr)
	}
	p := mustRouteFailurePolicy(t, out)
	id := fmt.Sprint(p.ID)

	code, out, stderr = run(t, "route", "policy", "set", id, "--retry", "3", "--json")
	if code != 0 {
		t.Fatalf("set --retry 退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if p.Retry != 3 || p.RetryBaseMS != 500 || !p.Disable || !p.ReturnError || p.Note != "base" {
		t.Fatalf("只改 retry 后 = %+v，其它字段应保持不变", p)
	}

	code, out, stderr = run(t, "route", "policy", "set", id, "--cooldown", "30m", "--json")
	if code != 0 {
		t.Fatalf("set --cooldown 退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if p.CooldownSeconds != 1800 || p.Disable || p.Retry != 3 || !p.ReturnError {
		t.Fatalf("设置冷却应清掉停用、保留 retry 与流程 = %+v", p)
	}

	code, out, stderr = run(t, "route", "policy", "set", id, "--disable", "--json")
	if code != 0 {
		t.Fatalf("set --disable 退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if !p.Disable || p.CooldownSeconds != 0 || p.Retry != 3 || !p.ReturnError {
		t.Fatalf("设置停用应清掉冷却 = %+v", p)
	}

	code, out, stderr = run(t, "route", "policy", "set", id, "--next_route", "--json")
	if code != 0 {
		t.Fatalf("set --next_route 退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if !p.NextRoute || p.ReturnError || !p.Disable || p.Retry != 3 {
		t.Fatalf("设置换号应清掉返回 = %+v", p)
	}

	code, out, stderr = run(t, "route", "policy", "set", id,
		"--status", "400,5xx", "--error-contains", "Rate Limit", "--error-contains", "timeout",
		"--error-regex", "(?i)limit", "--priority", "5", "--note", "patched", "--json")
	if code != 0 {
		t.Fatalf("set 条件退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if strings.Join(p.Statuses, ",") != "400,5xx" || len(p.ErrorContains) != 2 || p.ErrorRegex != "(?i)limit" ||
		p.Priority != 5 || p.Note != "patched" || p.Retry != 3 || !p.Disable || !p.NextRoute {
		t.Fatalf("补丁后的规则 = %+v", p)
	}

	code, out, stderr = run(t, "route", "policy", "set", id, "--all-errors", "--json")
	if code != 0 {
		t.Fatalf("set --all-errors 退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if len(p.Statuses) != 0 || len(p.ErrorContains) != 0 || p.ErrorRegex != "" ||
		p.Priority != 5 || p.Note != "patched" || p.Retry != 3 {
		t.Fatalf("--all-errors 应只清条件 = %+v", p)
	}

	// 边界值：10 次、10s 起始、720h 冷却都可以。
	code, out, stderr = run(t, "route", "policy", "set", id,
		"--retry", "10", "--retry-base", "10s", "--cooldown", "720h", "--next_route", "--json")
	if code != 0 {
		t.Fatalf("set 边界值退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if p.Retry != 10 || p.RetryBaseMS != 10000 || p.CooldownSeconds != 30*24*3600 ||
		p.Disable || !p.NextRoute || p.ReturnError {
		t.Fatalf("边界值规则 = %+v", p)
	}

	// 非法值与互斥：全部是用法错误，退出码 2 且不写 stdout。
	bad := [][]string{
		{"route", "policy", "set", id},
		{"route", "policy", "set", id, "--retry", "0"},
		{"route", "policy", "set", id, "--retry", "11"},
		{"route", "policy", "set", id, "--retry-base", "99ms"},
		{"route", "policy", "set", id, "--retry-base", "11s"},
		{"route", "policy", "set", id, "--cooldown", "999ms"},
		{"route", "policy", "set", id, "--cooldown", "721h"},
		{"route", "policy", "set", id, "--error-regex", "("},
		{"route", "policy", "set", id, "--priority", "-1"},
		{"route", "policy", "set", id, "--disable", "--cooldown", "1m"},
		{"route", "policy", "set", id, "--next_route", "--return"},
		{"route", "policy", "set", id, "--status", "400", "--all-errors"},
		{"route", "policy", "set", id, "--error-regex", ""},
	}
	for _, args := range bad {
		code, out, _ := run(t, args...)
		if code != 2 || out != "" {
			t.Fatalf("%v 应退出 2 且不写 stdout，实际 %d / %q", args, code, out)
		}
	}

	code, out, stderr = run(t, "route", "policy", "set", "999", "--note", "x")
	if code != 1 || out != "" || !strings.Contains(stderr, "lax route policy ls") {
		t.Fatalf("set 不存在的 id = %d / %q / %q，期望 1 且提示 ls", code, out, stderr)
	}
}

func TestRoutePolicyUnsetAndSafety(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "elsa", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-elsa", "--models", "m1"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "route", "policy", "add",
		"--account", "elsa", "--provider", "route-test", "--model", "m1",
		"--all-errors", "--retry", "2", "--retry-base", "1s",
		"--cooldown", "30m", "--next_route", "--priority", "7", "--note", "unset-me", "--json")
	if code != 0 {
		t.Fatalf("建规则退出码 = %d，stderr = %s", code, stderr)
	}
	p := mustRouteFailurePolicy(t, out)
	id := fmt.Sprint(p.ID)

	code, out, stderr = run(t, "route", "policy", "unset", id,
		"--account", "--provider", "--model", "--retry", "--retry-base", "--cooldown",
		"--priority", "--note", "--json")
	if code != 0 {
		t.Fatalf("unset 退出码 = %d，stderr = %s", code, stderr)
	}
	p = mustRouteFailurePolicy(t, out)
	if p.AccountID != 0 || p.Account != "" || p.Provider != "" || p.Model != "" ||
		p.Retry != 0 || p.RetryBaseMS != 500 || p.CooldownSeconds != 0 ||
		p.Priority != 100 || p.Note != "" || !p.NextRoute {
		t.Fatalf("unset 后 = %+v，期望字段清空、默认值回填、流程保留", p)
	}

	// 条件清空后没有条件时，必须用 --all-errors 确认。
	code, out, stderr = run(t, "route", "policy", "add",
		"--status", "400,5xx", "--error-contains", "billing", "--return", "--json")
	if code != 0 {
		t.Fatalf("建条件规则退出码 = %d，stderr = %s", code, stderr)
	}
	cond := mustRouteFailurePolicy(t, out)
	condID := fmt.Sprint(cond.ID)

	code, out, stderr = run(t, "route", "policy", "unset", condID, "--status", "--json")
	if code != 0 {
		t.Fatalf("只清状态退出码 = %d，stderr = %s", code, stderr)
	}
	cond = mustRouteFailurePolicy(t, out)
	if len(cond.Statuses) != 0 || len(cond.ErrorContains) != 1 || cond.ErrorContains[0] != "billing" {
		t.Fatalf("只清状态后 = %+v，错误文本条件应保留", cond)
	}

	if code, out, _ := run(t, "route", "policy", "unset", condID, "--error-contains"); code != 2 || out != "" {
		t.Fatalf("清掉最后一个条件不带 --all-errors 应退出 2，实际 %d / %q", code, out)
	}
	code, out, stderr = run(t, "route", "policy", "unset", condID, "--error-contains", "--all-errors", "--json")
	if code != 0 {
		t.Fatalf("带 --all-errors 清条件退出码 = %d，stderr = %s", code, stderr)
	}
	cond = mustRouteFailurePolicy(t, out)
	if len(cond.Statuses) != 0 || len(cond.ErrorContains) != 0 || cond.ErrorRegex != "" {
		t.Fatalf("清空条件后 = %+v，期望所有条件为空", cond)
	}

	if code, out, _ := run(t, "route", "policy", "unset", condID); code != 2 || out != "" {
		t.Fatalf("unset 不带字段应退出 2，实际 %d / %q", code, out)
	}
	if code, out, _ := run(t, "route", "policy", "unset", condID, "--all-errors"); code != 2 || out != "" {
		t.Fatalf("unset 只带 --all-errors 应退出 2，实际 %d / %q", code, out)
	}
	code, out, stderr = run(t, "route", "policy", "unset", "999", "--note")
	if code != 1 || out != "" || !strings.Contains(stderr, "lax route policy ls") {
		t.Fatalf("unset 不存在的 id = %d / %q / %q", code, out, stderr)
	}
}

func TestRoutePolicyValidationAndAllErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "elsa", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-elsa", "--models", "m1"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	bad := [][]string{
		{"route", "policy", "add", "--all-errors"},                                          // 没有动作
		{"route", "policy", "add", "--retry", "1"},                                          // 没有条件也没 --all-errors
		{"route", "policy", "add", "--retry-base", "1s", "--all-errors"},                    // 只有等待不算动作
		{"route", "policy", "add", "--all-errors", "--status", "400", "--retry", "1"},       // 条件与全错误互斥
		{"route", "policy", "add", "--all-errors", "--error-contains", "x", "--retry", "1"}, // 同上
		{"route", "policy", "add", "--all-errors", "--error-regex", "x", "--retry", "1"},    // 同上
		{"route", "policy", "add", "--all-errors", "--disable", "--cooldown", "1m"},         // 停用与冷却互斥
		{"route", "policy", "add", "--all-errors", "--next_route", "--return"},              // 流程互斥
		{"route", "policy", "add", "--all-errors", "--retry", "0"},
		{"route", "policy", "add", "--all-errors", "--retry", "11"},
		{"route", "policy", "add", "--all-errors", "--retry-base", "99ms"},
		{"route", "policy", "add", "--all-errors", "--retry-base", "11s"},
		{"route", "policy", "add", "--all-errors", "--cooldown", "999ms"},
		{"route", "policy", "add", "--all-errors", "--cooldown", "721h"},
		{"route", "policy", "add", "--all-errors", "--priority", "-1", "--retry", "1"},
		{"route", "policy", "add", "--status", "400", "--retry", "1", "--error-regex", "("},
		{"route", "policy", "add", "--status", "400", "--retry", "1", "--error-regex", strings.Repeat("a", 513)},
		{"route", "policy", "add", "--status", "400", "--retry", "1", "--error-contains", strings.Repeat("a", 201)},
		{"route", "policy", "add", "--status", "3xx", "--retry", "1"},
		{"route", "policy", "add", "--status", "600", "--retry", "1"},
		{"route", "policy", "add", "--status", "abc", "--retry", "1"},
		{"route", "policy", "add", "--status", "", "--retry", "1"},
		{"route", "policy", "add", "--status", "400", "--retry", "1", "--error-contains", ""},
		{"route", "policy", "add", "extra", "--status", "400", "--retry", "1"},
	}
	for _, args := range bad {
		code, out, _ := run(t, args...)
		if code != 2 || out != "" {
			t.Fatalf("%v 应退出 2 且不写 stdout，实际 %d / %q", args, code, out)
		}
	}

	// 状态 token 大小写归一与去重。
	code, out, stderr := run(t, "route", "policy", "add",
		"--status", "4XX, 429,429, 500", "--retry", "1", "--json")
	if code != 0 {
		t.Fatalf("大小写状态退出码 = %d，stderr = %s", code, stderr)
	}
	p := mustRouteFailurePolicy(t, out)
	if strings.Join(p.Statuses, ",") != "4xx,429,500" {
		t.Fatalf("状态 token = %v，期望 4xx,429,500", p.Statuses)
	}

	// 账号与厂商必须存在。
	for _, args := range [][]string{
		{"route", "policy", "add", "--account", "nope", "--status", "400", "--retry", "1"},
		{"route", "policy", "add", "--provider", "nope", "--status", "400", "--retry", "1"},
	} {
		code, out, she := run(t, args...)
		if code != 1 || out != "" || she == "" {
			t.Fatalf("%v 应因对象不存在退出 1 并有错误提示，实际 %d / %q / %q", args, code, out, she)
		}
	}

	// add/ls/test 的帮助与未知子命令。
	code, out, stderr = run(t, "route", "policy", "--help")
	if code != 0 {
		t.Fatalf("route policy --help 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"ls", "show", "add", "set", "unset", "rm", "enable", "disable", "test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("route policy 帮助缺 %q:\n%s", want, out)
		}
	}
	if code, out, _ := run(t, "route", "policy", "bogus"); code != 2 || out != "" {
		t.Fatalf("未知子命令应退出 2，实际 %d / %q", code, out)
	}
}

func TestRoutePolicyEnableDisableRm(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "route", "policy", "add",
		"--all-errors", "--return", "--note", "toggle-me", "--json")
	if code != 0 {
		t.Fatalf("建规则退出码 = %d，stderr = %s", code, stderr)
	}
	p := mustRouteFailurePolicy(t, out)
	id := fmt.Sprint(p.ID)

	code, out, stderr = run(t, "route", "policy", "disable", id, "--json")
	if code != 0 {
		t.Fatalf("disable 退出码 = %d，stderr = %s", code, stderr)
	}
	var toggled struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	mustJSON(t, out, &toggled)
	if toggled.ID != p.ID || toggled.Enabled {
		t.Fatalf("disable 输出 = %+v", toggled)
	}

	code, out, stderr = run(t, "route", "policy", "ls", "--json")
	if code != 0 {
		t.Fatalf("停用后 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if list := mustRouteFailurePolicyList(t, out); len(list.Policies) != 0 {
		t.Fatalf("默认 ls 不该列出停用规则: %+v", list.Policies)
	}
	code, out, stderr = run(t, "route", "policy", "ls", "--all", "--json")
	if code != 0 {
		t.Fatalf("ls --all 退出码 = %d，stderr = %s", code, stderr)
	}
	all := mustRouteFailurePolicyList(t, out)
	if len(all.Policies) != 1 || all.Policies[0].Enabled || all.Policies[0].Note != "toggle-me" {
		t.Fatalf("ls --all = %+v，期望列出停用规则", all.Policies)
	}
	code, out, stderr = run(t, "route", "policy", "ls")
	if code != 0 || strings.Contains(out, "toggle-me") {
		t.Fatalf("默认 ls 不该出现停用规则: %d / %s / %s", code, out, stderr)
	}
	code, out, stderr = run(t, "route", "policy", "ls", "--all")
	if code != 0 || !strings.Contains(out, "toggle-me") {
		t.Fatalf("ls --all 应出现停用规则: %d / %s / %s", code, out, stderr)
	}

	code, out, stderr = run(t, "route", "policy", "enable", id)
	if code != 0 || !strings.Contains(out, "已启用") {
		t.Fatalf("enable = %d / %q / %q", code, out, stderr)
	}

	code, out, stderr = run(t, "route", "policy", "rm", id, "--json")
	if code != 0 {
		t.Fatalf("rm 退出码 = %d，stderr = %s", code, stderr)
	}
	var removed struct {
		ID      int64 `json:"id"`
		Removed bool  `json:"removed"`
	}
	mustJSON(t, out, &removed)
	if removed.ID != p.ID || !removed.Removed {
		t.Fatalf("rm 输出 = %+v", removed)
	}
	if code, out, stderr = run(t, "route", "policy", "rm", id); code != 1 || out != "" || !strings.Contains(stderr, "lax route policy ls") {
		t.Fatalf("重复 rm = %d / %q / %q，期望 1 且提示 ls", code, out, stderr)
	}
	if code, out, stderr = run(t, "route", "policy", "enable", id); code != 1 || out != "" || !strings.Contains(stderr, "lax route policy ls") {
		t.Fatalf("enable 不存在 = %d / %q / %q", code, out, stderr)
	}
	if code, out, stderr = run(t, "route", "policy", "rm", "0"); code != 1 || out != "" || !strings.Contains(stderr, "内置") {
		t.Fatalf("rm 0 = %d / %q / %q，期望提示内置规则不能删", code, out, stderr)
	}
}

func TestRoutePolicyTestExplainDryRun(t *testing.T) {
	sandbox(t)
	mustInit(t)
	// 地址故意指向打不通的本机端口：test 若真发请求就会失败。
	if code, _, stderr := addAccount(t, "elsa", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-elsa", "--models", "m1"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "route", "policy", "add",
		"--account", "elsa", "--provider", "route-test",
		"--status", "400", "--error-contains", "insufficient credits",
		"--cooldown", "30m", "--next_route", "--priority", "1", "--note", "计费冷却", "--json")
	if code != 0 {
		t.Fatalf("建用户规则退出码 = %d，stderr = %s", code, stderr)
	}
	userRule := mustRouteFailurePolicy(t, out)

	code, out, stderr = run(t, "route", "policy", "add",
		"--status", "500", "--retry", "1", "--note", "停用规则", "--json")
	if code != 0 {
		t.Fatalf("建停用规则退出码 = %d，stderr = %s", code, stderr)
	}
	disabledRule := mustRouteFailurePolicy(t, out)
	if code, _, stderr := run(t, "route", "policy", "disable", fmt.Sprint(disabledRule.ID)); code != 0 {
		t.Fatalf("停用规则退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr = run(t, "route", "policy", "ls", "--all", "--json")
	if code != 0 {
		t.Fatalf("测试前 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	before := mustRouteFailurePolicyList(t, out)

	code, out, stderr = run(t, "route", "policy", "test",
		"--status", "400", "--message", "You have insufficient credits to continue",
		"--provider", "route-test", "--model", "m1", "--account", "elsa",
		"--explain", "--json")
	if code != 0 {
		t.Fatalf("test --explain 退出码 = %d，stderr = %s", code, stderr)
	}
	var got struct {
		Status    int    `json:"status"`
		Message   string `json:"message"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Account   string `json:"account"`
		AccountID int64  `json:"account_id"`
		Reason    string `json:"reason"`
		Matched   *struct {
			Source              string `json:"source"`
			ID                  int64  `json:"id"`
			Name                string `json:"name"`
			Retry               int    `json:"retry"`
			RetryBaseMS         int64  `json:"retry_base_ms"`
			CooldownSeconds     int64  `json:"cooldown_seconds"`
			Disable             bool   `json:"disable"`
			NextRoute           bool   `json:"next_route"`
			ReturnError         bool   `json:"return_error"`
			ModelScopedCooldown bool   `json:"model_scoped_cooldown"`
		} `json:"matched"`
		Explain []struct {
			Source   string `json:"source"`
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Enabled  bool   `json:"enabled"`
			Priority int    `json:"priority"`
			Matched  bool   `json:"matched"`
			Reason   string `json:"reason"`
		} `json:"explain"`
	}
	mustJSON(t, out, &got)
	if got.Status != 400 || got.Provider != "route-test" || got.Model != "m1" ||
		got.Account != "elsa" || got.AccountID == 0 || got.Reason != "命中" {
		t.Fatalf("test 事实/原因 = %+v", got)
	}
	if got.Matched == nil || got.Matched.Source != "user" || got.Matched.ID != userRule.ID ||
		got.Matched.Name != "计费冷却" || got.Matched.CooldownSeconds != 1800 ||
		got.Matched.NextRoute != true || got.Matched.ReturnError || got.Matched.ModelScopedCooldown {
		t.Fatalf("命中决策 = %+v，期望用户规则、账号级冷却 30m、换号", got.Matched)
	}
	if len(got.Explain) != 5 {
		t.Fatalf("解释列表 = %+v，期望 2 条用户规则 + 3 条内置", got.Explain)
	}
	var sawDisabled, sawBuiltinBilling bool
	for _, entry := range got.Explain {
		if entry.ID == disabledRule.ID && entry.Source == "user" {
			sawDisabled = true
			if entry.Enabled || entry.Matched || !strings.Contains(entry.Reason, "停用") {
				t.Fatalf("停用规则解释 = %+v，期望跳过且说明已停用", entry)
			}
		}
		if entry.Source == "builtin" && strings.Contains(entry.Name, "计费类") {
			sawBuiltinBilling = true
			if !entry.Matched {
				t.Fatalf("400 + insufficient credits 应命中计费内置兜底: %+v", entry)
			}
		}
	}
	if !sawDisabled || !sawBuiltinBilling {
		t.Fatalf("解释列表缺少停用规则或计费兜底: %+v", got.Explain)
	}

	// dry run：规则与账号状态都不变，也没有写请求记录。
	code, out, stderr = run(t, "route", "policy", "ls", "--all", "--json")
	if code != 0 {
		t.Fatalf("测试后 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	after := mustRouteFailurePolicyList(t, out)
	if len(after.Policies) != len(before.Policies) {
		t.Fatalf("test 改了规则数量：%d -> %d", len(before.Policies), len(after.Policies))
	}
	for i := range after.Policies {
		if after.Policies[i].ID != before.Policies[i].ID || after.Policies[i].UpdatedAt != before.Policies[i].UpdatedAt {
			t.Fatalf("test 改写了策略：%+v -> %+v", before.Policies[i], after.Policies[i])
		}
	}
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("测试后 account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var accounts []struct {
		Name            string `json:"name"`
		Enabled         bool   `json:"enabled"`
		CooldownUntilMS int64  `json:"cooldown_until_ms"`
		LastError       string `json:"last_error"`
	}
	mustJSON(t, out, &accounts)
	if len(accounts) != 1 || !accounts[0].Enabled || accounts[0].CooldownUntilMS != 0 || accounts[0].LastError != "" {
		t.Fatalf("test 写了账号状态: %+v", accounts)
	}

	// 人读输出：事实、逐条解释、命中规则与动作。
	code, out, stderr = run(t, "route", "policy", "test",
		"--status", "400", "--message", "You have insufficient credits to continue",
		"--provider", "route-test", "--model", "m1", "--account", "elsa", "--explain")
	if code != 0 {
		t.Fatalf("test --explain 人读退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"测试事实", "匹配过程", "内置：401 停用并返回",
		fmt.Sprintf("用户规则 #%d（计费冷却）：命中", userRule.ID),
		"命中：用户规则", "重试：不重试", "冷却：账号级 30m", "流程：换下一个候选账号"} {
		if !strings.Contains(out, want) {
			t.Fatalf("test --explain 输出缺 %q:\n%s", want, out)
		}
	}

	// 401 命中内置兜底。
	code, out, stderr = run(t, "route", "policy", "test",
		"--status", "401", "--message", "Invalid Authorization",
		"--provider", "route-test", "--model", "m1", "--account", "elsa", "--json")
	if code != 0 {
		t.Fatalf("test 401 退出码 = %d，stderr = %s", code, stderr)
	}
	var builtinHit struct {
		Matched *struct {
			Source      string `json:"source"`
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Disable     bool   `json:"disable"`
			NextRoute   bool   `json:"next_route"`
			ReturnError bool   `json:"return_error"`
		} `json:"matched"`
	}
	mustJSON(t, out, &builtinHit)
	if builtinHit.Matched == nil || builtinHit.Matched.Source != "builtin" || builtinHit.Matched.ID != 0 ||
		!strings.Contains(builtinHit.Matched.Name, "401") || !builtinHit.Matched.Disable ||
		builtinHit.Matched.NextRoute || !builtinHit.Matched.ReturnError {
		t.Fatalf("401 应命中内置兜底: %+v", builtinHit.Matched)
	}
	code, out, stderr = run(t, "route", "policy", "test",
		"--status", "401", "--message", "Invalid Authorization",
		"--provider", "route-test", "--model", "m1", "--account", "elsa")
	if code != 0 {
		t.Fatalf("test 401 人读退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"命中：内置：401 停用并返回", "停用：永久停用整个账号", "流程：返回上游错误"} {
		if !strings.Contains(out, want) {
			t.Fatalf("401 人读输出缺 %q:\n%s", want, out)
		}
	}

	// 没有规则命中：明确说按旧行为处理。
	code, out, stderr = run(t, "route", "policy", "test",
		"--status", "404", "--message", "not found",
		"--provider", "route-test", "--model", "m1", "--account", "elsa")
	if code != 0 {
		t.Fatalf("test 404 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "没有规则命中，按旧行为处理") {
		t.Fatalf("404 人读输出应说明没有规则命中:\n%s", out)
	}
	code, out, stderr = run(t, "route", "policy", "test",
		"--status", "404", "--message", "not found",
		"--provider", "route-test", "--model", "m1", "--account", "elsa", "--json")
	if code != 0 {
		t.Fatalf("test 404 JSON 退出码 = %d，stderr = %s", code, stderr)
	}
	var noMatch struct {
		Matched any    `json:"matched"`
		Reason  string `json:"reason"`
	}
	mustJSON(t, out, &noMatch)
	if noMatch.Matched != nil || noMatch.Reason != "没有规则命中，按旧行为处理" {
		t.Fatalf("404 JSON = %+v，期望 matched=null", noMatch)
	}

	// test 的参数校验。
	for _, args := range [][]string{
		{"route", "policy", "test"},
		{"route", "policy", "test", "--status", "5xx"},
		{"route", "policy", "test", "--status", "399"},
		{"route", "policy", "test", "--status", "600"},
		{"route", "policy", "test", "--status", "abc"},
		{"route", "policy", "test", "--status", "400", "--account", ""},
		{"route", "policy", "test", "--status", "400", "extra"},
	} {
		code, out, _ := run(t, args...)
		if code != 2 || out != "" {
			t.Fatalf("%v 应退出 2 且不写 stdout，实际 %d / %q", args, code, out)
		}
	}
	for _, args := range [][]string{
		{"route", "policy", "test", "--status", "400", "--account", "nope"},
		{"route", "policy", "test", "--status", "400", "--provider", "nope"},
	} {
		code, out, _ := run(t, args...)
		if code != 1 || out != "" {
			t.Fatalf("%v 应因对象不存在退出 1，实际 %d / %q", args, code, out)
		}
	}
}

func TestAccountLsAccountCooldown(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "cool", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-cool", "--models", "m1,m2"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	st, _ := mustStore(t)
	acc, err := st.GetAccount("cool")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	// 同时放一个模型冷却：账号级冷却的优先级更高。
	// RecordFailure 也会写 accounts.last_error，所以先写模型冷却，再写账号级冷却，
	// 否则最后看到的 last_error 会被模型那条覆盖。
	if err := st.RecordFailure(acc.ID, "m1", time.Now().Add(time.Hour).UnixMilli(), "m1 限流"); err != nil {
		t.Fatalf("写模型冷却: %v", err)
	}
	until := time.Now().Add(2 * time.Hour).UnixMilli()
	if err := st.SetAccountCooldown(acc.ID, until, "额度不足"); err != nil {
		t.Fatalf("写账号级冷却: %v", err)
	}

	code, out, stderr := run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls --json 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Name            string `json:"name"`
		Status          string `json:"status"`
		CooldownUntilMS int64  `json:"cooldown_until_ms"`
		LastError       string `json:"last_error"`
		CoolingModels   int    `json:"cooling_models"`
	}
	mustJSON(t, out, &rows)
	if len(rows) != 1 {
		t.Fatalf("账号行数 = %d，期望 1", len(rows))
	}
	if !strings.HasPrefix(rows[0].Status, "账号冷却中（还剩 ") ||
		rows[0].CooldownUntilMS != until || rows[0].LastError != "额度不足" || rows[0].CoolingModels != 1 {
		t.Fatalf("账号状态 = %+v", rows[0])
	}

	code, out, stderr = run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if line := accountLsLine(t, out, "cool"); !strings.Contains(line, "账号冷却中（还剩 ") {
		t.Fatalf("账号状态列应显示账号级冷却: %q", line)
	}

	// 停用优先级高于账号冷却。
	if code, _, stderr := run(t, "account", "disable", "cool"); code != 0 {
		t.Fatalf("停用账号退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("停用后 account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &rows)
	if rows[0].Status != "停用" {
		t.Fatalf("停用后状态 = %q，期望 停用", rows[0].Status)
	}

	// 启用会清掉账号级与模型级冷却，状态回到正常。
	if code, _, stderr := run(t, "account", "enable", "cool"); code != 0 {
		t.Fatalf("启用账号退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("启用后 account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &rows)
	if rows[0].Status != "正常" || rows[0].CooldownUntilMS != 0 || rows[0].LastError != "" || rows[0].CoolingModels != 0 {
		t.Fatalf("启用后账号状态 = %+v", rows[0])
	}
}

// accountTestPolicyJSON 在旧字段之外带上策略来源与动作。
type accountTestPolicyJSON struct {
	accountTestJSON
	Policy *struct {
		Source string `json:"source"`
		ID     int64  `json:"id"`
		Name   string `json:"name"`
	} `json:"policy"`
	PolicyAction string `json:"policy_action"`
}

func TestAccountTestPolicyOutput(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models": {status: http.StatusOK, body: modelsListM1},
		"POST /v1/chat/completions": {status: http.StatusUnauthorized,
			body: `{"error":{"message":"Invalid Authorization"}}`},
	})
	mustInit(t)
	if code, _, stderr := addAccount(t, "public", "custom", up.srv.URL, "", "--key", "sk-bad", "--models", "m1"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}

	// 没有用户规则时命中 401 内置兜底：JSON 带 policy/policy_action，旧字段保持不变。
	code, out, _ := run(t, "account", "test", "public", "--json")
	if code != 1 {
		t.Fatalf("401 探活退出码 = %d，期望 1；输出 %s", code, out)
	}
	var got accountTestPolicyJSON
	mustJSON(t, out, &got)
	if got.Policy == nil || got.Policy.Source != "builtin" || got.Policy.ID != 0 ||
		!strings.Contains(got.Policy.Name, "401") || !strings.Contains(got.PolicyAction, "永久停用账号") {
		t.Fatalf("401 命中策略 = %+v / %q", got.Policy, got.PolicyAction)
	}
	if got.Account != "public" || got.Stage != "call" || got.Status != http.StatusUnauthorized || got.OK {
		t.Fatalf("旧字段被破坏: %+v", got)
	}

	code, out, _ = run(t, "account", "test", "public")
	if code != 1 {
		t.Fatalf("401 人读探活退出码 = %d", code)
	}
	if !strings.Contains(out, "命中策略：内置：401 停用并返回（永久停用账号）") {
		t.Fatalf("人读输出应带命中策略: %s", out)
	}

	// 用户规则覆盖内置兜底：只返回、不改状态。
	if code, _, stderr := run(t, "account", "enable", "public"); code != 0 {
		t.Fatalf("重新启用账号退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr := run(t, "route", "policy", "add",
		"--account", "public", "--status", "401", "--return", "--priority", "1", "--note", "401 只返回", "--json")
	if code != 0 {
		t.Fatalf("建用户规则退出码 = %d，stderr = %s", code, stderr)
	}
	rule := mustRouteFailurePolicy(t, out)

	code, out, _ = run(t, "account", "test", "public", "--json")
	if code != 1 {
		t.Fatalf("用户规则 401 探活退出码 = %d", code)
	}
	mustJSON(t, out, &got)
	if got.Policy == nil || got.Policy.Source != "user" || got.Policy.ID != rule.ID ||
		got.Policy.Name != "401 只返回" || got.PolicyAction != "只返回，不改状态" {
		t.Fatalf("用户规则命中策略 = %+v / %q", got.Policy, got.PolicyAction)
	}
	st, _ := mustStore(t)
	acc, err := st.GetAccount("public")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	if !acc.Enabled || acc.CooldownUntilMS != 0 {
		t.Fatalf("只带返回的规则不该改账号状态: enabled=%v cooldown=%d", acc.Enabled, acc.CooldownUntilMS)
	}

	code, out, _ = run(t, "account", "test", "public")
	if code != 1 || !strings.Contains(out, fmt.Sprintf("用户规则 #%d（401 只返回）（只返回，不改状态）", rule.ID)) {
		t.Fatalf("用户规则人读输出 = %d / %s", code, out)
	}

	// 没有规则命中时保持旧输出形状：JSON 里不加 policy 与 policy_action。
	up2 := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models":            {status: http.StatusOK, body: modelsListM1},
		"POST /v1/chat/completions": {status: http.StatusBadRequest, body: `{"error":{"message":"bad parameter"}}`},
	})
	if code, _, stderr := addAccount(t, "generic", "custom2", up2.srv.URL, "", "--key", "sk-generic", "--models", "m1"); code != 0 {
		t.Fatalf("建普通账号退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, _ = run(t, "account", "test", "generic", "--json")
	if code != 1 {
		t.Fatalf("普通 400 探活退出码 = %d", code)
	}
	var raw map[string]any
	mustJSON(t, out, &raw)
	if _, ok := raw["policy"]; ok {
		t.Fatalf("没有命中时不该有 policy: %s", out)
	}
	if _, ok := raw["policy_action"]; ok {
		t.Fatalf("没有命中时不该有 policy_action: %s", out)
	}
	for _, key := range []string{"account", "stage", "status", "ok", "models", "latency", "message", "list"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("旧字段 %q 丢了: %s", key, out)
		}
	}
	code, out, _ = run(t, "account", "test", "generic")
	if code != 1 || strings.Contains(out, "命中策略：") {
		t.Fatalf("没有命中时人读输出不该有命中策略: %d / %s", code, out)
	}
}
