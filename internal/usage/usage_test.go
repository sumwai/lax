package usage_test

import (
	"testing"

	"lax/internal/usage"
)

// TestYuanAdaptivePrecision 钉住金额的三档精度：0 两位，小于 0.01 四位，其余两位。
// 固定两位会把决策请求的万分之几显示成 0.00，与免费、与缺价都看不出区别。
func TestYuanAdaptivePrecision(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00"},
		{0.0021, "0.0021"},
		{0.0001, "0.0001"},
		{0.00999, "0.0100"},
		{0.01, "0.01"},
		{0.5, "0.50"},
		{6, "6.00"},
		{1234.5678, "1234.57"},
		// 决策请求一次调用常见价：万分之几要能看出来。
		{-0.0021, "-0.0021"},
	}
	for _, c := range cases {
		if got := usage.Yuan(c.in); got != c.want {
			t.Fatalf("Yuan(%v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestYuanOrUnpriced 钉住「未定价」的渲染：nil 走文案，有值走 Yuan 的精度规则。
// nil 与 0 必须分开说：0.00 是确实不花钱，未定价是不知道多少钱。
func TestYuanOrUnpriced(t *testing.T) {
	zero := 0.0
	small := 0.0021
	plain := 6.0
	cases := []struct {
		name string
		in   *float64
		want string
	}{
		{"未定价", nil, "未定价"},
		{"免费", &zero, "0.00"},
		{"万分之几", &small, "0.0021"},
		{"普通金额", &plain, "6.00"},
	}
	for _, c := range cases {
		if got := usage.YuanOrUnpriced(c.in); got != c.want {
			t.Fatalf("%s：YuanOrUnpriced = %q，期望 %q", c.name, got, c.want)
		}
	}
}
