package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
	"switchboard/internal/translate"
)

// TestForwardNormalizesDirtyAccountBaseURL 钉住读取侧的规整：账号行里的 base_url 是脏的
// （末尾多一个斜杠、前缀里重复斜杠——升级前落库的老数据就是这样）时，转发拼出来的仍是
// 一个干净地址。断言完整 URL：只看 r.URL.Path 看不出 base_url 那一半有没有多出斜杠。
func TestForwardNormalizesDirtyAccountBaseURL(t *testing.T) {
	cases := []struct {
		name  string
		dirty string // 拼在假上游地址后面的脏写法
		want  string // 期望打到的完整路径
	}{
		{"末尾多一个斜杠", "//", "/v1/chat/completions"},
		{"末尾没有斜杠", "", "/v1/chat/completions"},
		{"前缀里的重复斜杠", "//api//paas//v4//", "/api/paas/v4/v1/chat/completions"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			var gotURL string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURL = "http://" + r.Host + r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, upstreamBodies[translate.ProtocolChat])
			}))
			t.Cleanup(up.Close)

			// 厂商行声明 chat 路由，账号地址是脏的：地址来自账号行，规整发生在读取侧。
			if err := f.st.CreateProvider(store.Provider{Name: "acme", BaseURL: "https://unused.invalid"},
				[]store.ProviderRoute{{Protocol: translate.ProtocolChat, Path: "/v1/chat/completions"}}); err != nil {
				t.Fatalf("建厂商: %v", err)
			}
			f.addAs(t, "a1", "acme", gateway.ProtocolChat, up.URL+c.dirty, "sk-dirty-base", []string{"m1"})
			srv := f.server(t)

			status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat)
			if status != http.StatusOK {
				t.Fatalf("状态 = %d，响应 %s", status, body)
			}
			if want := up.URL + c.want; gotURL != want {
				t.Fatalf("上游收到的地址 = %q，期望 %q", gotURL, want)
			}
			// 带端口的 host:port 不被改写。
			parsed, err := url.Parse(gotURL)
			if err != nil {
				t.Fatalf("地址不是合法 URL: %v，%q", err, gotURL)
			}
			wantHost, err := url.Parse(up.URL)
			if err != nil {
				t.Fatalf("假上游地址解析失败: %v", err)
			}
			if parsed.Host != wantHost.Host {
				t.Fatalf("地址的 host:port = %q，期望 %q", parsed.Host, wantHost.Host)
			}
			if strings.Contains(gotURL[len(parsed.Scheme)+3:], "//") {
				t.Fatalf("完整地址在 scheme 之后出现了重复斜杠: %q", gotURL)
			}
		})
	}
}

// TestEndpointFallbackNormalizesBaseURL 钉住后缀猜测那条兜底路径的规整：
// 账号的 provider 不在厂商表里（自建地址）时走 Endpoint，脏 base_url 同样拼出干净地址。
func TestEndpointFallbackNormalizesBaseURL(t *testing.T) {
	f := newFixture(t)
	var gotURL string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = "http://" + r.Host + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBodies[translate.ProtocolChat])
	}))
	t.Cleanup(up.Close)

	// provider 不在厂商表里；base_url 末尾带 //v1//，兜底规则认 /v1 后补 /chat/completions。
	f.addAs(t, "self", "self-hosted", gateway.ProtocolChat, up.URL+"//v1//", "sk-self", []string{"m1"})
	srv := f.server(t)

	status, body := postTo(t, srv, requestBodies[translate.ProtocolChat], translate.ProtocolChat)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，响应 %s", status, body)
	}
	if want := up.URL + "/v1/chat/completions"; gotURL != want {
		t.Fatalf("兜底路径的上游地址 = %q，期望 %q", gotURL, want)
	}
}
