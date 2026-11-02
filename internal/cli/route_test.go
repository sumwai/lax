package cli_test

import (
	"math"
	"strings"
	"testing"
)

func TestRoutePolicyAccountWeightsAndExplain(t *testing.T) {
	sandbox(t)
	mustInit(t)
	for _, name := range []string{"a1", "b1"} {
		code, _, stderr := addAccount(t, name, "route-test", "http://127.0.0.1:1", "openai-chat",
			"--key", "sk-"+name, "--models", "glm-5.2")
		if code != 0 {
			t.Fatalf("建账号 %s 退出码 = %d，stderr = %s", name, code, stderr)
		}
	}

	code, out, stderr := run(t, "route", "set", "glm-5.2", "--strategy", "softmax",
		"--sticky", "off", "--temperature", "0.3", "--explore", "10",
		"--score-priority", "0", "--score-success", "1", "--score-latency", "0")
	if code != 0 {
		t.Fatalf("route set 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "已设置 glm-5.2") || !strings.Contains(out, "softmax") {
		t.Fatalf("route set 输出 = %s", out)
	}

	code, out, stderr = run(t, "route", "show", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("route show 退出码 = %d，stderr = %s", code, stderr)
	}
	var policy map[string]any
	mustJSON(t, out, &policy)
	if policy["target"] != "glm-5.2" || policy["source"] != "model" || policy["strategy"] != "softmax" ||
		policy["sticky"] != "off" || policy["temperature"].(float64) != 0.3 ||
		policy["explore"].(float64) != 0.1 || policy["score_priority"].(float64) != 0 ||
		policy["score_success"].(float64) != 1 || policy["score_latency"].(float64) != 0 {
		t.Fatalf("route show = %+v", policy)
	}

	code, out, stderr = run(t, "route", "account", "set", "a1", "--model", "glm-5.2", "--weight", "3")
	if code != 0 {
		t.Fatalf("account set 模型权重退出码 = %d，stderr = %s", code, stderr)
	}
	code, _, stderr = run(t, "route", "account", "set", "a1", "--weight", "2")
	if code != 0 {
		t.Fatalf("account set 默认权重退出码 = %d，stderr = %s", code, stderr)
	}
	code, _, stderr = run(t, "route", "account", "set", "b1", "--model", "glm-5.2", "--weight", "1")
	if code != 0 {
		t.Fatalf("account set b1 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr = run(t, "route", "account", "ls", "--model", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var weights []struct {
		Account string `json:"account"`
		Model   string `json:"model"`
		Weight  int    `json:"weight"`
		Source  string `json:"source"`
	}
	mustJSON(t, out, &weights)
	if len(weights) != 2 {
		t.Fatalf("账号权重 = %+v，期望两个账号", weights)
	}
	byName := map[string]int{}
	source := map[string]string{}
	for _, row := range weights {
		byName[row.Account] = row.Weight
		source[row.Account] = row.Source
	}
	if byName["a1"] != 3 || source["a1"] != "model" || byName["b1"] != 1 || source["b1"] != "model" {
		t.Fatalf("模型权重解析 = %+v", weights)
	}

	if code, _, stderr := run(t, "route", "account", "unset", "a1", "--model", "glm-5.2"); code != 0 {
		t.Fatalf("unset 模型权重退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "account", "ls", "--model", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("unset 后 ls 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &weights)
	for _, row := range weights {
		if row.Account == "a1" && (row.Weight != 2 || row.Source != "account") {
			t.Fatalf("删模型权重后应回落到账号默认 2: %+v", row)
		}
	}

	code, out, stderr = run(t, "route", "explain", "glm-5.2", "--seed", "42", "--json")
	if code != 0 {
		t.Fatalf("route explain 退出码 = %d，stderr = %s", code, stderr)
	}
	var explain struct {
		Model      string   `json:"model"`
		Order      []string `json:"order"`
		Candidates []struct {
			Account     string  `json:"account"`
			Weight      int     `json:"weight"`
			Probability float64 `json:"probability"`
		} `json:"candidates"`
		Excluded []any `json:"excluded"`
	}
	mustJSON(t, out, &explain)
	if explain.Model != "glm-5.2" || len(explain.Candidates) != 2 || len(explain.Order) != 2 {
		t.Fatalf("explain = %+v", explain)
	}
	sum := 0.0
	for _, c := range explain.Candidates {
		sum += c.Probability
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("explain 概率和 = %v", sum)
	}
	_, again, _ := run(t, "route", "explain", "glm-5.2", "--seed", "42", "--json")
	var replay struct {
		Order []string `json:"order"`
	}
	mustJSON(t, again, &replay)
	if strings.Join(explain.Order, ",") != strings.Join(replay.Order, ",") {
		t.Fatalf("同一 seed 的 explain 顺序不同: %v vs %v", explain.Order, replay.Order)
	}

	if code, _, stderr := run(t, "route", "unset", "glm-5.2"); code != 0 {
		t.Fatalf("route unset 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "show", "glm-5.2", "--json")
	if code != 0 {
		t.Fatalf("unset 后 show 退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &policy)
	if policy["source"] != "builtin" || policy["strategy"] != "priority" {
		t.Fatalf("删模型策略后应回到内置默认: %+v", policy)
	}
}

func TestRouteDefaultAliasAndUsage(t *testing.T) {
	sandbox(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "a1", "route-test", "http://127.0.0.1:1", "openai-chat",
		"--key", "sk-a", "--models", "glm-5.2"); code != 0 {
		t.Fatalf("建账号退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "model", "alias", "set", "fast", "glm-5.2"); code != 0 {
		t.Fatalf("建别名退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "route", "set", "--default", "--strategy", "rotate", "--window", "7d")
	if code != 0 {
		t.Fatalf("route set --default 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "lax route show --default") {
		t.Fatalf("--default 的下一步命令不对: %s", out)
	}
	code, out, stderr = run(t, "route", "show", "--default", "--json")
	if code != 0 {
		t.Fatalf("route show --default 退出码 = %d，stderr = %s", code, stderr)
	}
	var policy map[string]any
	mustJSON(t, out, &policy)
	if policy["source"] != "default" || policy["strategy"] != "rotate" || policy["window_seconds"].(float64) != 7*86400 {
		t.Fatalf("默认策略 = %+v", policy)
	}

	if code, _, stderr := run(t, "route", "set", "fast", "--strategy", "weighted"); code != 0 {
		t.Fatalf("按别名写策略退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "route", "show", "fast", "--json")
	if code != 0 {
		t.Fatalf("按别名看策略退出码 = %d，stderr = %s", code, stderr)
	}
	mustJSON(t, out, &policy)
	if policy["target"] != "glm-5.2" || policy["source"] != "model" || policy["strategy"] != "weighted" {
		t.Fatalf("别名应写到真实模型名: %+v", policy)
	}

	code, out, stderr = run(t, "route", "ls")
	if code != 0 {
		t.Fatalf("route ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"default", "glm-5.2", "默认", "模型"} {
		if !strings.Contains(out, want) {
			t.Fatalf("route ls 缺 %q:\n%s", want, out)
		}
	}

	if code, _, _ := run(t, "route", "unset", "--default"); code != 0 {
		t.Fatalf("route unset --default 退出码 = %d", code)
	}
	if code, _, _ := run(t, "route", "unset", "--default"); code != 0 {
		t.Fatalf("重复 unset --default 应幂等")
	}

	cases := [][]string{
		{"route", "set", "glm-5.2"},
		{"route", "set", "glm-5.2", "--strategy", "unknown"},
		{"route", "set", "--default", "glm-5.2", "--strategy", "rotate"},
		{"route", "show"},
		{"route", "show", "--default", "glm-5.2"},
		{"route", "set", "glm-5.2", "--strategy", "softmax", "--window", "31d"},
		{"route", "account", "set", "a1"},
		{"route", "account", "set", "a1", "--weight", "0"},
	}
	for _, args := range cases {
		code, out, _ := run(t, args...)
		if code != 2 || out != "" {
			t.Fatalf("%v 应退出 2 且不写 stdout，实际 %d / %q", args, code, out)
		}
	}
}
