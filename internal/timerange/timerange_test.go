package timerange_test

import (
	"testing"
	"time"

	"lax/internal/timerange"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	cases := map[string]time.Duration{
		"30s":   30 * time.Second,
		"15m":   15 * time.Minute,
		"1h":    time.Hour,
		"1h30m": 90 * time.Minute,
		"24h":   24 * time.Hour,
		"7d":    7 * 24 * time.Hour,
		"2w":    14 * 24 * time.Hour,
	}
	for input, want := range cases {
		got, err := timerange.ParseSince(input, now)
		if err != nil {
			t.Fatalf("ParseSince(%q): %v", input, err)
		}
		if got != now.Add(-want).Unix() {
			t.Fatalf("ParseSince(%q) = %d，期望 %d", input, got, now.Add(-want).Unix())
		}
	}

	for _, bad := range []string{"7", "7y", "xd", "0m", "999999999999999999d"} {
		if _, err := timerange.ParseSince(bad, now); err == nil {
			t.Fatalf("ParseSince(%q) 应报错", bad)
		}
	}

	defaulted, err := timerange.ParseSince("", now)
	if err != nil {
		t.Fatalf("空值应走默认 7d: %v", err)
	}
	if defaulted != now.Add(-7*24*time.Hour).Unix() {
		t.Fatal("空值的默认范围应为 7 天")
	}
}

func TestParseAbsoluteRange(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 11, 1, 9, 30, 0, 0, loc)
	midnight := time.Date(2026, 10, 31, 0, 0, 0, 0, loc)

	fromCases := []struct {
		input string
		want  time.Time
	}{
		{"2026-10-31T12:00:00+08:00", midnight.Add(12 * time.Hour)},
		{"2026-10-31 12:00:00", midnight.Add(12 * time.Hour)},
		{"2026-10-31T12:00", midnight.Add(12 * time.Hour)},
		{"2026-10-31 11:00", midnight.Add(11 * time.Hour)},
		{"2026-10-31", midnight},
		{"11:00", time.Date(2026, 11, 1, 11, 0, 0, 0, loc)},
		{"11:00:30", time.Date(2026, 11, 1, 11, 0, 30, 0, loc)},
		{"-1d 11:00", midnight.Add(11 * time.Hour)},
		{"-1w 09:30", time.Date(2026, 10, 25, 9, 30, 0, 0, loc)},
		{"-1d", midnight},
	}
	for _, c := range fromCases {
		got, err := timerange.ParseFrom(c.input, now, loc)
		if err != nil {
			t.Fatalf("ParseFrom(%q): %v", c.input, err)
		}
		if got != c.want.Unix() {
			t.Fatalf("ParseFrom(%q) = %d，期望 %d", c.input, got, c.want.Unix())
		}
	}

	got, err := timerange.ParseTo("2026-10-31", now, loc)
	if err != nil {
		t.Fatalf("ParseTo 日期: %v", err)
	}
	if want := midnight.AddDate(0, 0, 1).Unix(); got != want {
		t.Fatalf("日期形式的 ParseTo = %d，期望次日零点 %d", got, want)
	}
	got, err = timerange.ParseTo("2026-10-31T12:00", now, loc)
	if err != nil {
		t.Fatalf("ParseTo 时间: %v", err)
	}
	if want := midnight.Add(12 * time.Hour).Unix(); got != want {
		t.Fatalf("时间形式的 ParseTo = %d，期望 %d", got, want)
	}
	got, err = timerange.ParseTo("11:00", now, loc)
	if err != nil {
		t.Fatalf("ParseTo 当天时刻: %v", err)
	}
	if want := time.Date(2026, 11, 1, 11, 0, 0, 0, loc).Unix(); got != want {
		t.Fatalf("当天时刻的 ParseTo = %d，期望当天 11:00 %d", got, want)
	}
	got, err = timerange.ParseTo("-1d", now, loc)
	if err != nil {
		t.Fatalf("ParseTo 相对日期: %v", err)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, loc).Unix(); got != want {
		t.Fatalf("相对日期的 ParseTo = %d，期望次日零点 %d", got, want)
	}

	for _, bad := range []string{"", "昨天", "2026-13-01", "2026-10-31 25:00", "-0d", "-d 11:00", "-1y 11:00"} {
		if _, err := timerange.ParseFrom(bad, now, loc); err == nil {
			t.Fatalf("ParseFrom(%q) 应报错", bad)
		}
		if _, err := timerange.ParseTo(bad, now, loc); err == nil {
			t.Fatalf("ParseTo(%q) 应报错", bad)
		}
	}
}

// TestParseSinceAbsoluteFallback 钉住 --since 对旧写法的兼容：
// plugin logs 原先收 RFC3339，复用共享解析后不能退化成只能写时长。
func TestParseSinceAbsoluteFallback(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 11, 1, 9, 30, 0, 0, loc)
	got, err := timerange.ParseSince("2026-10-31T12:00:00+08:00", now)
	if err != nil {
		t.Fatalf("ParseSince RFC3339: %v", err)
	}
	if want := time.Date(2026, 10, 31, 12, 0, 0, 0, loc).Unix(); got != want {
		t.Fatalf("ParseSince RFC3339 = %d，期望 %d", got, want)
	}
}
