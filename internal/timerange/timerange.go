// Package timerange 解析命令行上的时间范围：相对时长、当天时刻、相对日期与绝对时间。
package timerange

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseSince 把相对时长换算成起始时间戳。
//
// 支持 30s、15m、24h、7d、3w 与 Go 的复合时长（如 1h30m）；也接受
// 11:00、-1d 11:00、2025-01-02 15:04 这类绝对时间，供原本只收 RFC3339
// 与 Go 时长的命令继续复用。空串按 7d 处理。
func ParseSince(s string, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "7d"
	}
	if d, ok, err := parseRelativeDuration(s); err != nil {
		return 0, err
	} else if ok {
		return now.Add(-d).Unix(), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d).Unix(), nil
	}
	if ts, _, err := parseAbsolute(s, now, now.Location()); err == nil {
		return ts.Unix(), nil
	}
	return 0, fmt.Errorf("时间范围写法不对：%q，用 30s、15m、24h、7d、3w，或 11:00、-1d 11:00、2025-01-02 15:04[:05]、RFC3339", s)
}

// ParseFrom 把起始时间解析成 Unix 秒。不带时区的写法按 loc 解释；
// 只写日期时取当天零点。
func ParseFrom(s string, now time.Time, loc *time.Location) (int64, error) {
	t, _, err := parseAbsolute(s, now, loc)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// ParseTo 把结束时间解析成 Unix 秒。不带时区的写法按 loc 解释；
// 只写日期时取次日零点，这样 [from, to) 会把当天完整包含进来。
// 11:00 这类只写时刻的写法是当天 11:00，不补到次日。
func ParseTo(s string, now time.Time, loc *time.Location) (int64, error) {
	t, dateOnly, err := parseAbsolute(s, now, loc)
	if err != nil {
		return 0, err
	}
	if dateOnly {
		t = t.AddDate(0, 0, 1)
	}
	return t.Unix(), nil
}

// parseRelativeDuration 只解析单个单位的 Ns、Nm、Nh、Nd、Nw。
// 复合时长交给 time.ParseDuration；d 与 w 不在它的语法里，所以单独处理。
func parseRelativeDuration(s string) (time.Duration, bool, error) {
	if len(s) < 2 {
		return 0, false, nil
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false, nil
	}
	var unitDuration time.Duration
	switch unit {
	case 's':
		unitDuration = time.Second
	case 'm':
		unitDuration = time.Minute
	case 'h':
		unitDuration = time.Hour
	case 'd':
		unitDuration = 24 * time.Hour
	case 'w':
		unitDuration = 7 * 24 * time.Hour
	default:
		return 0, false, nil
	}
	if n > math.MaxInt64/int64(unitDuration) {
		return 0, true, fmt.Errorf("时间范围太长：%q", s)
	}
	return time.Duration(n) * unitDuration, true, nil
}

// parseAbsolute 依次尝试 RFC3339、本地日期时间、本地日期、当天时刻与
// 「-Nd / -Nw + 时刻」的相对日期写法。dateOnly 为真时调用方需要决定
// 「结束时间」是否补到次日零点。
func parseAbsolute(s string, now time.Time, loc *time.Location) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, errors.New("时间不能为空")
	}
	if loc == nil {
		loc = time.Local
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, false, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true, nil
	}
	if h, m, sec, ok := parseClock(s); ok {
		base := now.In(loc)
		return time.Date(base.Year(), base.Month(), base.Day(), h, m, sec, 0, loc), false, nil
	}

	fields := strings.Fields(s)
	shift, ok := parseRelativeDate(fields[0])
	if ok {
		base := now.In(loc).AddDate(0, 0, shift)
		switch len(fields) {
		case 1:
			return time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, loc), true, nil
		case 2:
			if h, m, sec, ok := parseClock(fields[1]); ok {
				return time.Date(base.Year(), base.Month(), base.Day(), h, m, sec, 0, loc), false, nil
			}
		}
	}
	return time.Time{}, false, fmt.Errorf("时间写法不对：%q，用 11:00、-1d 11:00、2025-01-02、2025-01-02 15:04[:05] 或 RFC3339", s)
}

// parseRelativeDate 解析 -1d、-2w 这类日期偏移，返回按天算的位移。
func parseRelativeDate(s string) (days int, ok bool) {
	if len(s) < 3 || s[0] != '-' {
		return 0, false
	}
	unit := s[len(s)-1]
	if unit != 'd' && unit != 'w' {
		return 0, false
	}
	n, err := strconv.Atoi(s[1 : len(s)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	if unit == 'w' {
		n *= 7
	}
	return -n, true
}

func parseClock(s string) (hour, minute, second int, ok bool) {
	for _, layout := range []string{"15:04:05", "15:04"} {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t.Hour(), t.Minute(), t.Second(), true
		}
	}
	return 0, 0, 0, false
}
