// Package usage 负责用量的时间范围解析与格式化。
package usage

import (
	"fmt"
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

// Yuan 把金额格式化成两位小数。
func Yuan(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
