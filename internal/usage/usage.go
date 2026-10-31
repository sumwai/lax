// Package usage 负责用量的时间范围解析与格式化。
package usage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseSince 把 7d、24h、3w 这类写法换算成起始时间戳。
func ParseSince(s string, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "7d"
	}
	if len(s) < 2 {
		return 0, fmt.Errorf("时间范围写法不对：%q，用 24h、7d、3w 这样的形式", s)
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("时间范围写法不对：%q，用 24h、7d、3w 这样的形式", s)
	}

	var d time.Duration
	switch unit {
	case 'h':
		d = time.Duration(n) * time.Hour
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	case 'w':
		d = time.Duration(n) * 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("不认识的时间单位 %q，可用 h、d、w", string(unit))
	}
	return now.Add(-d).Unix(), nil
}

// Yuan 把金额格式化成自适应精度的字符串：0 写两位小数，小于 0.01 的写四位小数，其余写两位。
//
// 固定两位不够用：决策请求的一次调用常见价是万分之几，固定两位会把 0.0021 显示成 0.00，
// 看上去与免费、与缺价都没区别。
func Yuan(v float64) string {
	switch {
	case v == 0:
		return strconv.FormatFloat(0, 'f', 2, 64)
	case math.Abs(v) < 0.01:
		return strconv.FormatFloat(v, 'f', 4, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

// YuanOrUnpriced 渲染可空金额：nil 是「未定价」，写成「未定价」；有值时按 Yuan 的精度规则写。
//
// 它对应 store.UsageRow.Cost 的 nil：一组里所有计价项都缺价时那里是 nil，JSON 里是 null。
// 「未定价」与 0.00 必须分开说：0.00 是这些请求确实不花钱（免费或打包），未定价是不知道多少钱。
func YuanOrUnpriced(v *float64) string {
	if v == nil {
		return "未定价"
	}
	return Yuan(*v)
}
