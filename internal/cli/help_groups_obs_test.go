package cli_test

import (
	"strings"
	"testing"
)

func TestHelpGroupsObs(t *testing.T) {
	sandbox(t)

	cases := []struct {
		name    string
		args    []string
		headers []string
	}{
		{
			name:    "model probe",
			args:    []string{"model", "probe", "--help"},
			headers: []string{"筛选范围:", "同步与缓存:", "执行控制:", "通用:"},
		},
		{
			name:    "usage",
			args:    []string{"usage", "--help"},
			headers: []string{"聚合维度:", "过滤条件:", "时间范围:", "通用:"},
		},
		{
			name:    "log tail",
			args:    []string{"log", "tail", "--help"},
			headers: []string{"输出控制:", "时间范围:", "通用:"},
		},
		{
			name:    "model call",
			args:    []string{"model", "call", "--help"},
			headers: []string{"路由与协议:", "请求参数:", "通用:"},
		},
		{
			name:    "plugin logs",
			args:    []string{"plugin", "logs", "--help"},
			headers: []string{"过滤与时间范围:", "输出控制:", "通用:"},
		},
		{
			name:    "model decide",
			args:    []string{"model", "decide", "--help"},
			headers: []string{"基础输入:", "逐题拼接（可重复）:", "整块导入（与逐题互斥）:", "通用:"},
		},
		{
			name:    "model capability add",
			args:    []string{"model", "capability", "add", "--help"},
			headers: []string{"作用域与能力:", "语音扩展:", "通用:"},
		},
		{
			name:    "model capability set",
			args:    []string{"model", "capability", "set", "--help"},
			headers: []string{"作用域与能力:", "语音扩展:", "通用:"},
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
