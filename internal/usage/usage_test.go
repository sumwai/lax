package usage_test

import (
	"testing"
	"time"

	"switchboard/internal/usage"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	cases := map[string]time.Duration{
		"1h":  time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
	}
	for input, want := range cases {
		got, err := usage.ParseSince(input, now)
		if err != nil {
			t.Fatalf("ParseSince(%q): %v", input, err)
		}
		if got != now.Add(-want).Unix() {
			t.Fatalf("ParseSince(%q) = %d，期望 %d", input, got, now.Add(-want).Unix())
		}
	}

	if _, err := usage.ParseSince("7", now); err == nil {
		t.Fatal("缺少单位应报错")
	}
	if _, err := usage.ParseSince("7y", now); err == nil {
		t.Fatal("不认识的时间单位应报错")
	}
	if _, err := usage.ParseSince("xd", now); err == nil {
		t.Fatal("非数字应报错")
	}

	defaulted, err := usage.ParseSince("", now)
	if err != nil {
		t.Fatalf("空值应走默认 7d: %v", err)
	}
	if defaulted != now.Add(-7*24*time.Hour).Unix() {
		t.Fatal("空值的默认范围应为 7 天")
	}
}
