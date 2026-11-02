package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lax/internal/gateway"
)

// closedURL 返回一个已经没有服务监听的地址：向它发请求是连接被拒，一条 HTTP 响应都没拿到。
// 端口由 httptest 分配，用完就释放，因此不依赖某个固定的空端口。
func closedURL(t *testing.T) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := up.URL
	up.Close()
	return url
}

// assertCooldown 检查账号-模型的冷却截止时间离 before 约等于 want。时间戳是毫秒级，
// 500 毫秒的档位也能直接比较；向后放宽两秒吸收测试本身的耗时。
func assertCooldown(t *testing.T, f *fixture, name, model string, want time.Duration, before time.Time) {
	t.Helper()
	acc, err := f.st.GetAccount(name)
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}
	until := acc.ModelFailures[model].CooldownUntilMS
	if until == 0 {
		t.Fatalf("账号 %s 的模型 %s 没有进冷却", name, model)
	}
	got := time.UnixMilli(until).Sub(before)
	slack := want / 4
	if slack < 100*time.Millisecond {
		slack = 100 * time.Millisecond
	}
	if got < want-slack || got > want+2*time.Second {
		t.Fatalf("账号 %s 的模型 %s 冷却 = %v，期望约 %v", name, model, got, want)
	}
}

// TestTransportFailureCoolsTwoSeconds 钉住两件事同时成立：连接被拒时 requests.http_status 仍是
// 502（表示网关没拿到上游响应），冷却却按连接层那一档从 2 秒起算，而不是上游 5xx 的 10 秒。
// 单账号网关在断网恢复后能否马上接活取决于这一档。
func TestTransportFailureCoolsTwoSeconds(t *testing.T) {
	f := newFixture(t)
	f.add(t, "dead", gateway.ProtocolChat, closedURL(t), "sk-test", []string{"m1"}, 100)
	srv := f.server(t)

	before := time.Now()
	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	// 可重试的失败会继续换号，唯一候选用完之后客户端拿到的是 503；502 记在 requests 那一行。
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503（唯一候选失败），响应 %s", status, body)
	}

	recs, err := f.st.RecentRequests(5)
	if err != nil {
		t.Fatalf("读请求记录: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("请求记录数 = %d，期望 1", len(recs))
	}
	if recs[0].HTTPStatus != http.StatusBadGateway || recs[0].OK {
		t.Fatalf("请求记录 = %+v，期望 http_status 502 且 ok=0", recs[0])
	}

	assertCooldown(t, f, "dead", "m1", 2*time.Second, before)
	if streak := f.modelFailStreak(t, "dead", "m1"); streak != 1 {
		t.Fatalf("fail_streak = %d，期望 1", streak)
	}
}

// TestStatusFailureCoolsByStatusCode 钉住拿到状态的那两档：上游 500 从 10 秒起，
// 429 从 500 毫秒起，与连接层那一档分开算。429 不在一次请求内重试同一个账号。
func TestStatusFailureCoolsByStatusCode(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   time.Duration
	}{
		{"上游 500", http.StatusInternalServerError, 10 * time.Second},
		{"限流 429", http.StatusTooManyRequests, 500 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, `{"error":{"message":"上游说不行"}}`)
			}))
			defer up.Close()
			f.add(t, "up", gateway.ProtocolChat, up.URL, "sk-test", []string{"m1"}, 100)
			srv := f.server(t)

			before := time.Now()
			status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("状态 = %d，期望 503（唯一候选失败），响应 %s", status, body)
			}

			recs, err := f.st.RecentRequests(5)
			if err != nil {
				t.Fatalf("读请求记录: %v", err)
			}
			if len(recs) != 1 {
				t.Fatalf("请求记录数 = %d，期望 1: %+v", len(recs), recs)
			}
			for _, rec := range recs {
				if rec.HTTPStatus != c.status || rec.OK {
					t.Fatalf("请求记录 = %+v，期望 http_status %d 且 ok=0", rec, c.status)
				}
			}

			assertCooldown(t, f, "up", "m1", c.want, before)
		})
	}
}

// TestTransportFailureDoublesByFailStreak 钉住连接层那一档同样按 fail_streak 翻倍：
// 已经有两次连续失败时，第三次失败的冷却从 2 秒翻到 8 秒，而不是每次重头算。
func TestTransportFailureDoublesByFailStreak(t *testing.T) {
	f := newFixture(t)
	acc := f.add(t, "flaky", gateway.ProtocolChat, closedURL(t), "sk-test", []string{"m1"}, 100)
	// 先记两次失败、冷冷却清零，账号仍在候选里：下一次连接层失败按 streak=2 翻两倍成 8 秒。
	for i := 0; i < 2; i++ {
		if err := f.st.RecordFailure(acc.ID, "m1", 0, "上一次失败"); err != nil {
			t.Fatalf("记失败: %v", err)
		}
	}
	srv := f.server(t)

	before := time.Now()
	status, body := postMessages(t, srv, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503（唯一候选失败），响应 %s", status, body)
	}

	assertCooldown(t, f, "flaky", "m1", 8*time.Second, before)
	if streak := f.modelFailStreak(t, "flaky", "m1"); streak != 3 {
		t.Fatalf("fail_streak = %d，期望 3", streak)
	}
}
