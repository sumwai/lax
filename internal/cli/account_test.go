package cli_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switchboard/internal/config"
	"switchboard/internal/secret"
	"switchboard/internal/store"
)

// accountAddJSON 是 account add --json 的输出里用例关心的字段。
type accountAddJSON struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Protocol string `json:"protocol"`
	BaseURL  string `json:"base_url"`
}

func mustAccountAdd(t *testing.T, out string) accountAddJSON {
	t.Helper()
	var got accountAddJSON
	mustJSON(t, out, &got)
	return got
}

// TestAccountAddUsesProviderTable 覆盖 account add 从厂商表补默认地址与默认协议，
// 以及 --base-url 与 --protocol 各自能覆盖它们。
func TestAccountAddUsesProviderTable(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	// 默认协议取库里的有效默认协议：deepseek 显式存了 openai-chat，
	// 单靠排序会得到 anthropic（anthropic 排在 openai-chat 前）。
	code, out, stderr := run(t, "account", "add", "ds1", "--provider", "deepseek", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds1 := mustAccountAdd(t, out)
	if ds1.BaseURL != "https://api.deepseek.com" || ds1.Protocol != "openai-chat" {
		t.Fatalf("deepseek 账号的默认值 = %+v，期望 base_url 取厂商表、协议取库里的 openai-chat", ds1)
	}

	// --protocol 覆盖默认协议，地址仍来自厂商表。
	code, out, stderr = run(t, "account", "add", "ds2",
		"--provider", "deepseek", "--protocol", "anthropic", "--key", "sk-2", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds2 := mustAccountAdd(t, out)
	if ds2.BaseURL != "https://api.deepseek.com" || ds2.Protocol != "anthropic" {
		t.Fatalf("--protocol 应覆盖默认协议 = %+v", ds2)
	}

	// --base-url 覆盖厂商地址，协议仍按厂商表补。
	code, out, stderr = run(t, "account", "add", "ds3",
		"--provider", "deepseek", "--base-url", "http://127.0.0.1:9", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds3 := mustAccountAdd(t, out)
	if ds3.BaseURL != "http://127.0.0.1:9" || ds3.Protocol != "openai-chat" {
		t.Fatalf("--base-url 应覆盖厂商地址 = %+v", ds3)
	}

	// 只声明了 openai-responses 的账号也能加：三种线协议都收。
	code, out, stderr = run(t, "account", "add", "oa1",
		"--provider", "openai", "--protocol", "openai-responses", "--key", "sk-4", "--json")
	if code != 0 {
		t.Fatalf("openai-responses 账号的退出码 = %d，stderr = %s", code, stderr)
	}
	oa1 := mustAccountAdd(t, out)
	if oa1.Protocol != "openai-responses" || oa1.BaseURL != "https://api.openai.com" {
		t.Fatalf("openai-responses 账号 = %+v", oa1)
	}

	// 不认识的协议仍是用法错误，文案要列出三种可选值。
	code, _, stderr = run(t, "account", "add", "bad", "--provider", "openai", "--protocol", "gemini", "--key", "sk-5")
	if code != 2 {
		t.Fatalf("不支持的协议退出码 = %d，期望 2", code)
	}
	for _, want := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("协议错误的文案应列出 %q: %s", want, stderr)
		}
	}
}

// TestAccountAddProviderWithoutRoutes 覆盖厂商行在、但一条路由都没声明的情况：
// 没给 --protocol 也没给 --base-url 时报错并给出三条下一步，给了任一个都能加。
func TestAccountAddProviderWithoutRoutes(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	// 命令行已经造不出零路由厂商（add 至少要求一条路由），老库里可能有，用 store 直接造。
	seedProviderWithoutRoutes(t, "noroute", "https://api.example.com")

	code, out, stderr := run(t, "account", "add", "n1", "--provider", "noroute", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("厂商无路由的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("报错时不该写 stdout: %s", out)
	}
	for _, want := range []string{"switchboard provider set noroute", "--protocol", "--base-url", "--openai"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	// 显式 --protocol 时照常能加，地址仍取厂商的 base_url。
	code, out, stderr = run(t, "account", "add", "n2",
		"--provider", "noroute", "--protocol", "anthropic", "--key", "sk-2", "--json")
	if code != 0 {
		t.Fatalf("带 --protocol 的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Protocol != "anthropic" || got.BaseURL != "https://api.example.com" {
		t.Fatalf("带 --protocol 的账号 = %+v", got)
	}

	// 只给 --base-url 时按自建地址加账号，协议回退 openai-chat（见 DESIGN 5.8）。
	code, out, stderr = run(t, "account", "add", "n3",
		"--provider", "noroute", "--base-url", "http://127.0.0.1:9", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("带 --base-url 的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Protocol != "openai-chat" || got.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("带 --base-url 的账号 = %+v", got)
	}
}

// TestAccountAddUnknownProvider 覆盖厂商不在表里时的文案：已知厂商清单、最接近的名字、
// 自建地址的走法。
func TestAccountAddUnknownProvider(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	// 拼错一个字：提示最接近的已知厂商。
	code, out, stderr := run(t, "account", "add", "x1", "--provider", "deepsek", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("报错时不该写 stdout: %s", out)
	}
	for _, want := range []string{"deepseek", "已知：", "最接近", "--base-url"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	// 差得远：只列已知厂商，不猜。
	code, _, stderr = run(t, "account", "add", "x2", "--provider", "zzzzzzzz", "--key", "sk-2")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "已知：") {
		t.Fatalf("stderr 应列出已知厂商: %s", stderr)
	}
	if strings.Contains(stderr, "最接近") {
		t.Fatalf("编辑距离超过 2 时不该给最接近的名字: %s", stderr)
	}

	// 自定义名加 --base-url 仍然可用。
	code, out, stderr = run(t, "account", "add", "x3",
		"--provider", "deepsek", "--base-url", "http://127.0.0.1:9", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("自建地址的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Provider != "deepsek" || got.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("自建地址的账号 = %+v", got)
	}
}

// TestAccountAddWithoutProviders 覆盖厂商表被删空后的行为：
// 带 --base-url 照常可用，不带时报错并指出两条下一步。
func TestAccountAddWithoutProviders(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for name := range mustProviderRows(t, out) {
		if code, _, stderr := run(t, "provider", "rm", name, "--force"); code != 0 {
			t.Fatalf("provider rm %s 退出码 = %d，stderr = %s", name, code, stderr)
		}
	}

	code, out, _ = run(t, "provider", "ls", "--json")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d", code)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("厂商表应已清空: %s", out)
	}

	code, out, stderr = run(t, "account", "add", "a1", "--provider", "deepseek", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("厂商表为空时的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("报错时不该写 stdout: %s", out)
	}
	for _, want := range []string{"switchboard provider ls", "--base-url"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	code, out, stderr = run(t, "account", "add", "a1",
		"--provider", "deepseek", "--base-url", "http://127.0.0.1:9", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("带 --base-url 的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("厂商表为空时 --base-url 应直接用上: %+v", got)
	}
}

// TestAccountAddBaseURLValidated 覆盖 account add --base-url 的校验：与 provider add/set 同一份
// store.NormalizeBaseURL（见 DESIGN 5.8）。只有 scheme 的写法是用法错误、退出码 2，文案保留
// store 的原话并附这个子命令的用法行；大写 scheme 与末尾斜杠被接受，落库存规整后的形态。
func TestAccountAddBaseURLValidated(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "account", "add", "mine1",
		"--provider", "custom", "--base-url", "https://", "--key", "sk-test")
	if code != 2 || out != "" {
		t.Fatalf("退出码 = %d，stdout = %q", code, out)
	}
	for _, want := range []string{"里没有主机名", "https://api.deepseek.com", "用法：switchboard account add"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应包含 %q: %s", want, stderr)
		}
	}

	// 校验失败不该留下半个账号：同名再加一次必须成功。
	code, out, stderr = run(t, "account", "add", "mine1",
		"--provider", "custom", "--base-url", "HTTPS://API.Example.com/", "--key", "sk-test", "--json")
	if code != 0 {
		t.Fatalf("大写 scheme 应被接受，退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.BaseURL != "https://API.Example.com" {
		t.Fatalf("base_url = %q，期望 https://API.Example.com", got.BaseURL)
	}
}

// mustStore 打开当前沙箱的库与密钥，供用例直接检查 store 里的状态：
// 冷却截止、连续失败次数与方向化后的字段不在 --json 输出里，只能从库里读。
func mustStore(t *testing.T) (*store.Store, []byte) {
	t.Helper()
	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析路径失败: %v", err)
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, err := secret.LoadOrCreateKey(filepath.Join(filepath.Dir(paths.DB), "secret.key"))
	if err != nil {
		t.Fatalf("读取密钥失败: %v", err)
	}
	return st, key
}

// TestAccountSetRotatesKey 覆盖换 key 的完整行为：同一条加密路径换掉 key_cipher，
// 旧凭据的结论作废（冷却、最近错误与连续失败次数清零，账号重新启用），
// 探活结果与选号列表一个字不动，新 key 能转发、旧 key 已经不在库里。
func TestAccountSetRotatesKey(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t) // 只接受 Bearer sk-test
	mustInit(t)

	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", "sk-old", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 造出旧凭据留下的状态：一次失败冷却、一条失败计数、一个停用标记。
	st, key := mustStore(t)
	acc, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号失败: %v", err)
	}
	oldCipher := append([]byte(nil), acc.KeyCipher...)
	if plain, err := secret.Decrypt(key, oldCipher); err != nil || string(plain) != "sk-old" {
		t.Fatalf("旧凭据解密 = %q, %v，期望 sk-old", plain, err)
	}
	if err := st.RecordFailure(acc.ID, time.Now().Add(time.Hour).Unix(), "上游 429"); err != nil {
		t.Fatalf("造失败状态: %v", err)
	}
	if err := st.SetEnabled(acc.ID, false, "手工停用"); err != nil {
		t.Fatalf("停用账号: %v", err)
	}

	code, out, stderr := run(t, "account", "set", "ds1", "--key", "sk-test")
	if code != 0 {
		t.Fatalf("account set 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已更新 ds1 的凭据", "sk-t…", "冷却与失败计数已清零", "账号已启用"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应包含 %q: %s", want, out)
		}
	}
	if strings.Contains(out, "sk-test") {
		t.Fatalf("输出不该回显凭据明文: %s", out)
	}

	// 库里换成了新凭据，旧密文已经不在：换 key 不是追加，旧 key 不再能解密使用。
	acc, err = st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号失败: %v", err)
	}
	if bytes.Equal(acc.KeyCipher, oldCipher) {
		t.Fatal("key_cipher 没换")
	}
	if plain, err := secret.Decrypt(key, acc.KeyCipher); err != nil || string(plain) != "sk-test" {
		t.Fatalf("新凭据解密 = %q, %v，期望 sk-test", plain, err)
	}
	if acc.FailStreak != 0 || acc.CooldownUntil != 0 || acc.LastError != "" || !acc.Enabled {
		t.Fatalf("换 key 后账号状态 = %+v，期望冷却、失败计数与最近错误清零且已启用", acc)
	}
	if len(acc.Models) != 1 || acc.Models[0] != "m1" {
		t.Fatalf("换 key 不该动选号列表: %v", acc.Models)
	}

	// 新 key 能真的用上：假上游只接受 Bearer sk-test，调通就说明换的是它。
	code, out, stderr = run(t, "call", "hi", "--model", "m1")
	if code != 0 {
		t.Fatalf("call 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("call 输出 = %s", out)
	}
}

// TestAccountSetKeyFromStdin 覆盖 --key - 从标准输入读，三种给法与 account add 一致。
func TestAccountSetKeyFromStdin(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)
	mustInit(t)
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", "sk-old", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	saved := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道: %v", err)
	}
	if _, err := w.WriteString("  sk-test\n\n"); err != nil {
		t.Fatalf("写管道: %v", err)
	}
	_ = w.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; _ = r.Close() })

	if code, _, stderr := run(t, "account", "set", "ds1", "--key", "-"); code != 0 {
		t.Fatalf("account set --key - 退出码 = %d，stderr = %s", code, stderr)
	}
	// 首尾空白被去掉、新 key 生效：假上游只认 Bearer sk-test。
	if code, out, stderr := run(t, "call", "hi", "--model", "m1"); code != 0 {
		t.Fatalf("call 退出码 = %d，stderr = %s", code, stderr)
	} else if !strings.Contains(out, "pong") {
		t.Fatalf("call 输出 = %s", out)
	}
}

// TestAccountSetErrors 覆盖换 key 的三种失败：缺凭据与缺账号名是用法错误（退出码 2），
// 账号不存在是运行失败（退出码 1）并指向 account add。
func TestAccountSetErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, out, stderr := run(t, "account", "set", "ds1")
	if code != 2 || out != "" {
		t.Fatalf("缺凭据的退出码 = %d，stdout = %q", code, out)
	}
	for _, want := range []string{"缺少凭据", "SWITCHBOARD_KEY", "用法：switchboard account set"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应包含 %q: %s", want, stderr)
		}
	}

	code, out, stderr = run(t, "account", "set", "--key", "sk-1")
	if code != 2 || out != "" {
		t.Fatalf("缺账号名的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "用法：switchboard account set") {
		t.Fatalf("stderr 应给出用法: %s", stderr)
	}

	code, out, stderr = run(t, "account", "set", "nope", "--key", "sk-1")
	if code != 1 || out != "" {
		t.Fatalf("账号不存在的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "switchboard account add nope") {
		t.Fatalf("stderr 应指向 account add: %s", stderr)
	}
}

// TestAccountSetJSONMasksKey 确认 --json 只回显掩码，明文不进输出。
func TestAccountSetJSONMasksKey(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)
	mustInit(t)
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", "sk-old", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "set", "ds1", "--key", "sk-plain-secret", "--json")
	if code != 0 {
		t.Fatalf("account set --json 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(out, "sk-plain-secret") {
		t.Fatalf("--json 不该含凭据明文: %s", out)
	}
	var got map[string]any
	mustJSON(t, out, &got)
	if got["name"] != "ds1" || got["key"] != "sk-p…" {
		t.Fatalf("--json = %+v，期望名称与掩码", got)
	}
	for _, field := range []string{"cooldown_cleared", "fail_streak_cleared", "enabled"} {
		if got[field] != true {
			t.Fatalf("--json 的 %s = %v，期望 true", field, got[field])
		}
	}
}

// TestAccountUpdateIsSynonymForSet 确认 update 与 set 等价：都换凭据，帮助里只列 set。
func TestAccountUpdateIsSynonymForSet(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)
	mustInit(t)
	if code, _, stderr := run(t, "account", "add", "ds1", "--provider", "custom",
		"--base-url", up.URL, "--key", "sk-old", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	if code, out, stderr := run(t, "account", "update", "ds1", "--key", "sk-test"); code != 0 {
		t.Fatalf("account update 退出码 = %d，stderr = %s", code, stderr)
	} else if !strings.Contains(out, "已更新 ds1 的凭据") {
		t.Fatalf("account update 的输出 = %s，期望与 set 同一段文案", out)
	}
	if code, _, stderr := run(t, "call", "hi", "--model", "m1"); code != 0 {
		t.Fatalf("call 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "--help")
	if code != 0 {
		t.Fatalf("account --help 退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "set") {
		t.Fatalf("组级帮助应列出 set: %s", out)
	}
	if strings.Contains(out, "update") {
		t.Fatalf("组级帮助不该列 update 同义词: %s", out)
	}
}

// TestAccountTestWithoutModels 确认刚建好、还没同步过模型的账号也能探活：
// 模型列表为空时回退到默认模型名，退出码 0，备注说明用的是哪个模型名，不再 panic。
func TestAccountTestWithoutModels(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatCompletionOK},
	})
	mustInit(t)

	if code, _, stderr := run(t, "account", "add", "fresh",
		"--provider", "custom", "--base-url", up.srv.URL, "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "test", "fresh")
	if code != 0 {
		t.Fatalf("模型列表为空的账号探活退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "正常") || !strings.Contains(out, "还没有模型") || !strings.Contains(out, "default") {
		t.Fatalf("探活输出 = %s，期望写明用默认模型名", out)
	}
	call := up.mustSingleCall(t)
	if want := up.srv.URL + "/v1/chat/completions"; call.url != want {
		t.Fatalf("探活地址 = %q，期望 %q", call.url, want)
	}
}
