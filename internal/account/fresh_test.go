package account_test

import (
	"testing"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/store"
)

// TestSettledFresh 钉住新鲜度判定的每条边界：只有 0 <= 差值 <= maxAge 才算新鲜。
func TestSettledFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	maxAge := 24 * time.Hour

	cases := []struct {
		name      string
		status    string
		checkedAt int64
		maxAge    time.Duration
		want      bool
	}{
		{"从未探过", store.ModelUsable, 0, maxAge, false},
		{"刚好在窗口边界", store.ModelUsable, now.Add(-maxAge).Unix(), maxAge, true},
		{"窗口内", store.ModelUsable, now.Add(-maxAge + time.Second).Unix(), maxAge, true},
		{"刚超过窗口", store.ModelUsable, now.Add(-maxAge - time.Second).Unix(), maxAge, false},
		{"未来时间戳", store.ModelUsable, now.Add(time.Hour).Unix(), maxAge, false},
		{"待探活没有定论", store.ModelSynced, now.Unix(), maxAge, false},
		{"凭据被拒没有定论", store.ModelAuthFailed, now.Unix(), maxAge, false},
		{"临时失败没有定论", store.ModelError, now.Unix(), maxAge, false},
		{"无权但有定论", store.ModelForbidden, now.Unix(), maxAge, true},
		{"不存在但有定论", store.ModelNotFound, now.Unix(), maxAge, true},
		{"maxAge 为 0 且同秒", store.ModelUsable, now.Unix(), 0, true},
		{"maxAge 为 0 时差一秒也算过期", store.ModelUsable, now.Add(-time.Second).Unix(), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := account.SettledFresh(c.status, c.checkedAt, now, c.maxAge); got != c.want {
				t.Fatalf("SettledFresh(%q, %d, now, %v) = %v，期望 %v",
					c.status, c.checkedAt, c.maxAge, got, c.want)
			}
		})
	}
}
