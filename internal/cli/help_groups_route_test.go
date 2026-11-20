package cli_test

import (
	"strings"
	"testing"
)

func TestHelpGroupsRoute(t *testing.T) {
	sandbox(t)

	cases := []struct {
		name    string
		args    []string
		headers []string
	}{
		{
			name:    "provider add",
			args:    []string{"provider", "add", "--help"},
			headers: []string{"基础与鉴权:", "协议路由:", "通用:"},
		},
		{
			name:    "provider set",
			args:    []string{"provider", "set", "--help"},
			headers: []string{"基础与鉴权:", "协议路由:", "通用:"},
		},
		{
			name:    "route policy add",
			args:    []string{"route", "policy", "add", "--help"},
			headers: []string{"匹配范围:", "错误条件:", "重试与状态动作:", "流转动作:", "规则信息:", "通用:"},
		},
		{
			name:    "route policy set",
			args:    []string{"route", "policy", "set", "--help"},
			headers: []string{"匹配范围:", "错误条件:", "重试与状态动作:", "流转动作:", "规则信息:", "通用:"},
		},
		{
			name:    "route policy unset",
			args:    []string{"route", "policy", "unset", "--help"},
			headers: []string{"清除匹配范围:", "清除错误条件:", "清除重试与状态动作:", "重置规则信息:", "通用:"},
		},
		{
			name:    "route policy test",
			args:    []string{"route", "policy", "test", "--help"},
			headers: []string{"模拟错误:", "匹配上下文:", "输出控制:", "通用:"},
		},
		{
			name:    "route set",
			args:    []string{"route", "set", "--help"},
			headers: []string{"目标与基础策略:", "Softmax 评分与探索:", "成功率统计窗口:", "额度调度:", "通用:"},
		},
		{
			name:    "model price test",
			args:    []string{"model", "price", "test", "--help"},
			headers: []string{"作用域:", "Token 用量:", "语音用量:", "时间模拟:", "通用:"},
		},
		{
			name:    "account add",
			args:    []string{"account", "add", "--help"},
			headers: []string{"必填与身份:", "模型与调度:", "通用:"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := run(t, tc.args...)
			if code != 0 {
				t.Fatalf("%s 退出码 = %d，stderr = %s", tc.name, code, stderr)
			}
			lastIdx := -1
			for _, h := range tc.headers {
				idx := strings.Index(stderr, h)
				if idx < 0 {
					t.Fatalf("%s --help 缺少分组 %q:\n%s", tc.name, h, stderr)
				}
				if idx <= lastIdx {
					t.Fatalf("%s --help 分组 %q 顺序错误:\n%s", tc.name, h, stderr)
				}
				lastIdx = idx
			}
			if !strings.Contains(stderr, "示例:") {
				t.Fatalf("%s --help 缺少示例区块:\n%s", tc.name, stderr)
			}
		})
	}
}
