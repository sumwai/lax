package cli_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lax/internal/config"
	"lax/internal/secret"
	"lax/internal/store"
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

// TestAccountAddUsesProviderTable 覆盖 account add 的地址与协议来源：两个值都从厂商行读，
// 账号不持有副本，因此也没有能覆盖它们的参数。
func TestAccountAddUsesProviderTable(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	// 默认协议取库里的有效默认协议：deepseek 显式存了 openai-chat，
	// 单靠排序会得到 anthropic（anthropic 排在 openai-chat 前）。
	code, out, stderr := addAccount(t, "ds1", "deepseek", "", "", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
	ds1 := mustAccountAdd(t, out)
	if ds1.BaseURL != "https://api.deepseek.com" || ds1.Protocol != "openai-chat" {
		t.Fatalf("deepseek 账号的默认值 = %+v，期望 base_url 取厂商表、协议取库里的 openai-chat", ds1)
	}

	// 只声明了 anthropic 路由的厂商，默认协议因此是 anthropic。
	code, out, stderr = addAccount(t, "an1", "anthco", "https://anth.example.com", "anthropic", "--key", "sk-2", "--json")
	if code != 0 {
		t.Fatalf("anthropic 账号的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Protocol != "anthropic" || got.BaseURL != "https://anth.example.com" {
		t.Fatalf("anthropic 账号 = %+v", got)
	}

	// 只声明了 openai-responses 的厂商同样算得出默认协议：三种线协议都收。
	code, out, stderr = addAccount(t, "oa1", "respco", "https://resp.example.com", "openai-responses", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("openai-responses 账号的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Protocol != "openai-responses" {
		t.Fatalf("openai-responses 账号 = %+v", got)
	}

	// gemini 入站与出站都可用：厂商把默认协议设成 gemini 时，账号也按它算。
	code, out, stderr = addAccount(t, "g1", "gemco", "https://gem.example.com", "gemini", "--key", "sk-4", "--json")
	if code != 0 {
		t.Fatalf("gemini 账号的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Protocol != "gemini" {
		t.Fatalf("gemini 账号 = %+v", got)
	}

	// 厂商的 base_url 为空是合法的（路由与 models_path 都是整条地址），
	// 账号的派生地址因此也是空串，--json 里是空串而不是占位。
	if code, _, stderr := run(t, "provider", "add", "splitco",
		"--openai", "https://openai.example.com/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add splitco 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = addAccount(t, "sp1", "splitco", "", "", "--key", "sk-5", "--json")
	if code != 0 {
		t.Fatalf("无 base_url 厂商的账号退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.BaseURL != "" || got.Protocol != "openai-chat" {
		t.Fatalf("无 base_url 厂商的账号 = %+v，期望地址为空串、协议 openai-chat", got)
	}
}

// TestAccountAddRejectsRetiredFlags 钉住删掉的两个参数：--base-url 与 --protocol 是用法错误
// （退出码 2），文案说清它们去了哪、下一步跑什么，而不是只报「参数不认识」。
func TestAccountAddRejectsRetiredFlags(t *testing.T) {
	sandbox(t)
	mustInit(t)

	cases := []struct {
		args []string
		hint string
	}{
		{[]string{"--provider", "deepseek", "--base-url", "https://api.example.com", "--key", "sk-1"},
			"lax provider add"},
		{[]string{"--provider", "deepseek", "--protocol", "anthropic", "--key", "sk-1"},
			"--default-protocol"},
		{[]string{"--provider", "deepseek", "--base-url=https://api.example.com", "--key", "sk-1"},
			"lax provider add"},
	}
	for _, c := range cases {
		code, out, stderr := run(t, append([]string{"account", "add", "a1"}, c.args...)...)
		if code != 2 {
			t.Fatalf("%v 的退出码 = %d，期望 2（用法错误）", c.args, code)
		}
		if out != "" {
			t.Fatalf("用法错误不该写 stdout: %s", out)
		}
		for _, want := range []string{c.hint, "用法：lax account add"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%v 的 stderr 应提到 %q: %s", c.args, want, stderr)
			}
		}
	}
}

// TestAccountAddHelpFollowsProviderTable 钉住 --help 与新模型一致：参数里没有地址与协议，
// 用法行指向先登记厂商。
func TestAccountAddHelpFollowsProviderTable(t *testing.T) {
	sandbox(t)

	code, _, errOut := run(t, "account", "add", "--help")
	if code != 0 {
		t.Fatalf("account add --help 的退出码 = %d", code)
	}
	for _, want := range []string{"--provider", "--key", "--models", "--priority"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("account add --help 应列出 %q: %s", want, errOut)
		}
	}
	for _, gone := range []string{"--base-url", "--protocol", "--price-in", "--price-out", "--price-cache-read", "--price-cache-write"} {
		if strings.Contains(errOut, gone) {
			t.Fatalf("account add --help 不该再列出 %q: %s", gone, errOut)
		}
	}
}

// TestAccountAddProviderWithoutRoutes 覆盖厂商行在、但一条路由都没声明的情况：默认协议
// 算不出来，但建账号本身不算错——转发时按厂商默认协议推导，推导为空由转发层报错
// （转发层会报错），所以这里不报错，地址照旧来自厂商行。
func TestAccountAddProviderWithoutRoutes(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}
	// 命令行已经造不出零路由厂商（add 至少要求一条路由），老库里可能有，用 store 直接造。
	seedProviderWithoutRoutes(t, "noroute", "https://api.example.com")

	code, out, stderr := addAccount(t, "n1", "noroute", "", "", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("厂商无路由时建账号的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Protocol != "" || got.BaseURL != "https://api.example.com" {
		t.Fatalf("厂商无路由的账号 = %+v，期望协议为空串、地址取厂商行", got)
	}

	// 人读输出里协议算不出来时写 -，不打出一个空括号。
	code, out, stderr = addAccount(t, "n2", "noroute", "", "", "--key", "sk-2")
	if code != 0 {
		t.Fatalf("厂商无路由时建账号的退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加 n2（noroute，-，凭据 "; !strings.Contains(out, want) {
		t.Fatalf("协议算不出来时应写 -: %s", out)
	}
}

// TestAccountAddUnknownProvider 覆盖厂商不在表里时的文案：已知厂商清单、最接近的名字，
// 以及先登记厂商的那条命令（账号已经没有自己的地址可填）。
func TestAccountAddUnknownProvider(t *testing.T) {
	sandbox(t)
	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 退出码 = %d，stderr = %s", code, stderr)
	}

	// 拼错一个字：提示最接近的已知厂商，并给登记厂商的命令。
	code, out, stderr := run(t, "account", "add", "x1", "--provider", "deepsek", "--key", "sk-1")
	if code != 1 {
		t.Fatalf("未知厂商的退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("报错时不该写 stdout: %s", out)
	}
	for _, want := range []string{"deepseek", "已知：", "最接近",
		"lax provider add deepsek --base-url <地址> --openai /v1/chat/completions"} {
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

	// 先把厂商登记进表，同一个名字就能建账号，地址与协议从厂商行读。
	ensureProvider(t, "deepsek", "http://127.0.0.1:9", routeFlagsForProtocol("")...)
	code, out, stderr = run(t, "account", "add", "x3", "--provider", "deepsek", "--key", "sk-3", "--json")
	if code != 0 {
		t.Fatalf("登记厂商后的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.Provider != "deepsek" || got.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("登记厂商后的账号 = %+v", got)
	}
}

// TestAccountAddWithoutProviders 覆盖厂商表被删空后的行为：没有厂商就既没有地址也没有协议，
// 报错并指出先登记厂商；登记之后同一条命令就能建账号。
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
	for _, want := range []string{"厂商表是空的",
		"lax provider add deepseek --base-url <地址> --openai /v1/chat/completions"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}

	if code, _, stderr := run(t, "provider", "add", "deepseek",
		"--base-url", "http://127.0.0.1:9", "--openai", "/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add deepseek 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "account", "add", "a1", "--provider", "deepseek", "--key", "sk-1", "--json")
	if code != 0 {
		t.Fatalf("登记厂商后的退出码 = %d，stderr = %s", code, stderr)
	}
	if got := mustAccountAdd(t, out); got.BaseURL != "http://127.0.0.1:9" || got.Protocol != "openai-chat" {
		t.Fatalf("登记厂商后的账号 = %+v", got)
	}
}

// TestAccountLsShowsProviderAddressAndProtocol 覆盖 account ls 的地址列与协议列：
// 两列都从厂商行取，base_url 为空的厂商地址列写 -。
func TestAccountLsShowsProviderAddressAndProtocol(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "splitco",
		"--openai", "https://openai.example.com/v1/chat/completions"); code != 0 {
		t.Fatalf("provider add splitco 退出码 = %d，stderr = %s", code, stderr)
	}
	addAccount(t, "sp1", "splitco", "", "", "--key", "sk-1")
	addAccount(t, "ds1", "deepseek", "", "", "--key", "sk-2")

	code, out, stderr := run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		Name     string `json:"name"`
		Protocol string `json:"protocol"`
		BaseURL  string `json:"base_url"`
	}
	mustJSON(t, out, &rows)
	byName := map[string]struct {
		protocol string
		baseURL  string
	}{}
	for _, r := range rows {
		byName[r.Name] = struct {
			protocol string
			baseURL  string
		}{r.Protocol, r.BaseURL}
	}
	if got := byName["sp1"]; got.baseURL != "" || got.protocol != "openai-chat" {
		t.Fatalf("splitco 账号 = %+v，期望地址为空串、协议 openai-chat", got)
	}
	if got := byName["ds1"]; got.baseURL != "https://api.deepseek.com" || got.protocol != "openai-chat" {
		t.Fatalf("deepseek 账号 = %+v", got)
	}

	// 厂商行在、但一条路由都没声明：地址照旧来自厂商行，默认协议算不出来写 -。
	seedProviderWithoutRoutes(t, "noroute", "https://api.noroute.example.com")
	addAccount(t, "nr1", "noroute", "", "", "--key", "sk-3")

	code, out, stderr = run(t, "account", "ls")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"地址", "协议", "https://api.deepseek.com", "openai-chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("account ls 文本输出缺 %q:\n%s", want, out)
		}
	}
	// base_url 与默认协议算不出来的两种空值都在列里写 -，与 provider ls 同一套写法。
	for _, c := range []struct{ name, wantBase, wantProtocol string }{
		{"sp1", "-", "openai-chat"},
		{"nr1", "https://api.noroute.example.com", "-"},
	} {
		fields := strings.Fields(accountLsLine(t, out, c.name))
		if fields[2] != c.wantBase || fields[3] != c.wantProtocol {
			t.Fatalf("%s 的地址列与协议列 = %q / %q，期望 %q / %q", c.name, fields[2], fields[3], c.wantBase, c.wantProtocol)
		}
	}
}

// accountLsLine 从 account ls 的表格里挑出某个账号那一行：用例要逐列看地址与协议，
// 只找子串分不清哪一列写了什么。
func accountLsLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return line
		}
	}
	t.Fatalf("account ls 的输出里没有账号 %s 那一行:\n%s", name, out)
	return ""
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

	if code, _, stderr := addAccount(t, "ds1", "custom", up.URL, "", "--key", "sk-old", "--models", "m1"); code != 0 {
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
	for _, want := range []string{"已更新 ds1（custom）的凭据", "sk-t…", "冷却与失败计数已清零", "账号已启用"} {
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
	code, out, stderr = run(t, "model", "call", "hi", "--model", "m1")
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
	if code, _, stderr := addAccount(t, "ds1", "custom", up.URL, "", "--key", "sk-old", "--models", "m1"); code != 0 {
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
	if code, out, stderr := run(t, "model", "call", "hi", "--model", "m1"); code != 0 {
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
	for _, want := range []string{"缺少凭据", "LAX_KEY", "用法：lax account set"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应包含 %q: %s", want, stderr)
		}
	}

	code, out, stderr = run(t, "account", "set", "--key", "sk-1")
	if code != 2 || out != "" {
		t.Fatalf("缺账号名的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "用法：lax account set") {
		t.Fatalf("stderr 应给出用法: %s", stderr)
	}

	code, out, stderr = run(t, "account", "set", "nope", "--key", "sk-1")
	if code != 1 || out != "" {
		t.Fatalf("账号不存在的退出码 = %d，stdout = %q", code, out)
	}
	if !strings.Contains(stderr, "lax account add nope") {
		t.Fatalf("stderr 应指向 account add: %s", stderr)
	}
}

// TestAccountSetJSONMasksKey 确认 --json 只回显掩码，明文不进输出。
func TestAccountSetJSONMasksKey(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "custom", up.URL, "", "--key", "sk-old", "--models", "m1"); code != 0 {
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
	// 价格不在账号上：这条命令的 --json 不再有四个价格字段。
	for _, gone := range []string{"price_in", "price_out", "price_cache_read", "price_cache_write"} {
		if _, ok := got[gone]; ok {
			t.Fatalf("account set --json 不该再有 %s: %s", gone, out)
		}
	}
}

// TestAccountUpdateIsSynonymForSet 确认 update 与 set 等价：都换凭据，帮助里只列 set。
func TestAccountUpdateIsSynonymForSet(t *testing.T) {
	sandbox(t)
	up := fakeUpstream(t)
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "custom", up.URL, "", "--key", "sk-old", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	if code, out, stderr := run(t, "account", "update", "ds1", "--key", "sk-test"); code != 0 {
		t.Fatalf("account update 退出码 = %d，stderr = %s", code, stderr)
	} else if !strings.Contains(out, "已更新 ds1（custom）的凭据") {
		t.Fatalf("account update 的输出 = %s，期望与 set 同一段文案", out)
	}
	if code, _, stderr := run(t, "model", "call", "hi", "--model", "m1"); code != 0 {
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
// 列表接口不可用时退回最小请求，模型列表为空时回退到默认模型名、备注说明用的是哪个，
// 两阶段的结果都记在 stage 与模型数列上，不再 panic。
func TestAccountTestWithoutModels(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatCompletionOK},
	})
	mustInit(t)

	if code, _, stderr := addAccount(t, "fresh", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "test", "fresh", "--json")
	if code != 0 {
		t.Fatalf("模型列表为空的账号探活退出码 = %d，stderr = %s", code, stderr)
	}
	var got accountTestJSON
	mustJSON(t, out, &got)
	if !got.OK || got.Stage != "call" || got.Models != 0 {
		t.Fatalf("探活结果 = %+v，期望退回模型调用、没有模型数", got)
	}
	for _, want := range []string{"还没有模型", "default", "模型调用"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("备注 = %q，期望包含 %q", got.Message, want)
		}
	}
	calls := up.all()
	if len(calls) != 2 || calls[0].path != "/v1/models" || calls[1].path != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %+v，期望先 GET 列表再打最小请求", calls)
	}

	// 人读输出：列表接口没通时模型数是 -，不是 0。
	up.reset()
	code, out, stderr = run(t, "account", "test", "fresh")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "正常") || !strings.Contains(out, "上游模型数") {
		t.Fatalf("探活输出 = %s，期望表格带状态与模型数列", out)
	}
	if !strings.Contains(out, " -") {
		t.Fatalf("探活输出 = %s，列表接口没通时模型数应显示 -", out)
	}
}

// accountTestJSON 是 account test --json 输出里用例关心的字段。
// 旧字段（account、status、ok、latency、message）必须还在，之后加了 stage、models 与 list。
type accountTestJSON struct {
	Account string              `json:"account"`
	Stage   string              `json:"stage"`
	Status  int                 `json:"status"`
	OK      bool                `json:"ok"`
	Models  int                 `json:"models"`
	Latency string              `json:"latency"`
	Message string              `json:"message"`
	List    accountTestListJSON `json:"list"`
}

// accountTestListJSON 是列表那一段自己的结论：地址与连通性、上游模型数。
type accountTestListJSON struct {
	Stage   string `json:"stage"`
	Status  int    `json:"status"`
	OK      bool   `json:"ok"`
	Models  int    `json:"models"`
	Latency string `json:"latency"`
	Message string `json:"message"`
}

// TestAccountTestListOKStillChecksKey 钉住两段都跑：列表 2xx 之后仍发一条最小调用证明凭据有效，
// 两段都通过才算正常，上游模型数取列表那一段。
func TestAccountTestListOKStillChecksKey(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models":            {status: http.StatusOK, body: modelsListM1},
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatCompletionOK},
	})
	mustInit(t)

	if code, _, stderr := addAccount(t, "free", "custom", up.srv.URL, "", "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "test", "free", "--json")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stderr = %s", code, stderr)
	}
	var got accountTestJSON
	mustJSON(t, out, &got)
	if got.Account != "free" || got.Stage != "call" || got.Status != http.StatusOK || !got.OK {
		t.Fatalf("探活结果 = %+v，期望两段都通过、stage=call、200 且 ok", got)
	}
	if got.Models != 1 || got.List.Models != 1 || !got.List.OK {
		t.Fatalf("探活结果 = %+v，期望上游模型数 1 且列表那一段通过", got)
	}
	if got.Latency == "" || !strings.Contains(got.Message, "列表接口") || !strings.Contains(got.Message, "模型调用") {
		t.Fatalf("探活结果 = %+v，期望备注写清两段", got)
	}

	calls := up.all()
	if len(calls) != 2 || calls[0].method != http.MethodGet || calls[0].path != "/v1/models" ||
		calls[1].method != http.MethodPost || calls[1].path != "/v1/chat/completions" {
		t.Fatalf("上游调用 = %+v，期望先 GET 列表再 POST 最小请求", calls)
	}

	// 人读输出：模型数是真数字，不是 -。
	up.reset()
	code, out, stderr = run(t, "account", "test", "free")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stderr = %s", code, stderr)
	}
	if !strings.Contains(out, " 1 ") {
		t.Fatalf("探活输出 = %s，期望模型数列写出 1", out)
	}
}

// TestAccountTestPublicListDoesNotProveKey 钉住缺陷现场：列表接口对任何 key 都回 200（commandcode 的
// GET /provider/v1/models 就是），凭据得看调用那段。调用 401 时状态是失败、退出码 1，
// 列表那一段的结论仍留在输出里，好让「地址通、key 不行」一眼分开。
func TestAccountTestPublicListDoesNotProveKey(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"GET /v1/models": {status: http.StatusOK, body: modelsListM1},
		"POST /v1/chat/completions": {status: http.StatusUnauthorized,
			body: `{"error":{"message":"Invalid 'Authorization' header or token"}}`},
	})
	mustInit(t)

	if code, _, stderr := addAccount(t, "public", "custom", up.srv.URL, "", "--key", "sk-bad", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, _ := run(t, "account", "test", "public", "--json")
	if code == 0 {
		t.Fatalf("调用 401 应当是失败，退出码 = %d，输出 = %s", code, out)
	}
	var got accountTestJSON
	mustJSON(t, out, &got)
	if got.OK || got.Stage != "call" || got.Status != http.StatusUnauthorized {
		t.Fatalf("探活结果 = %+v，期望失败、stage=call、401", got)
	}
	if !got.List.OK || got.List.Status != http.StatusOK || got.Models != 1 {
		t.Fatalf("探活结果 = %+v，列表那一段应当仍是通过的、模型数 1", got)
	}
	for _, want := range []string{"模型调用返回 401", "列表接口可用"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("备注 = %q，期望包含 %q", got.Message, want)
		}
	}
	calls := up.all()
	if len(calls) != 2 {
		t.Fatalf("上游调用 = %+v，期望列表 2xx 之后仍发一次调用", calls)
	}

	// 人读输出：状态列是失败，且两段都写得出来。
	up.reset()
	code, out, _ = run(t, "account", "test", "public")
	if code == 0 {
		t.Fatalf("调用 401 应当以非零退出码结束，输出 = %s", out)
	}
	if !strings.Contains(out, "失败") || !strings.Contains(out, "列表接口可用") ||
		!strings.Contains(out, "模型调用返回 401") {
		t.Fatalf("探活输出 = %s，期望状态是失败、备注写清两段", out)
	}
}

// TestAccountTestFallsBackToCallStage 钉住退回最小请求时 stage 记成 call，
// 两阶段都失败时备注能把「列表接口」与「模型调用」分开。
func TestAccountTestFallsBackToCallStage(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: http.StatusOK, body: chatCompletionOK},
	})
	mustInit(t)

	if code, _, stderr := addAccount(t, "fb", "custom", up.srv.URL, "", "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "test", "fb", "--json")
	if code != 0 {
		t.Fatalf("探活退出码 = %d，stderr = %s", code, stderr)
	}
	var got accountTestJSON
	mustJSON(t, out, &got)
	if !got.OK || got.Stage != "call" || got.Models != 0 {
		t.Fatalf("探活结果 = %+v，期望 stage=call、模型数未知", got)
	}
	if !strings.Contains(got.Message, "列表接口返回") || !strings.Contains(got.Message, "模型调用") {
		t.Fatalf("备注 = %q，期望写清两阶段各自的结果", got.Message)
	}
	calls := up.all()
	if len(calls) != 2 || calls[0].method != http.MethodGet || calls[1].method != http.MethodPost {
		t.Fatalf("上游调用 = %+v，期望先 GET 列表再 POST 最小请求", calls)
	}
}

// TestAccountTestNeverPrintsKey 确认上游把凭据回显到错误里时，输出里也没有明文：
// stderr、`--json` 的 message 与库里存的 last_error 三处都要干净。
func TestAccountTestNeverPrintsKey(t *testing.T) {
	sandbox(t)
	// 运行期拼出来，避免测试文件里出现像个真 key 的字符串。
	key := "sk-" + strings.Repeat("t", 20)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: http.StatusUnauthorized,
			body: `{"error":{"message":"invalid api key=` + key + `"}}`},
	})
	mustInit(t)

	if code, _, stderr := addAccount(t, "leak", "custom", up.srv.URL, "", "--key", key, "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "account", "test", "leak", "--json")
	if code == 0 {
		t.Fatalf("401 应当是失败，退出码 = %d", code)
	}
	if strings.Contains(out, key) || strings.Contains(stderr, key) {
		t.Fatalf("探活输出泄漏了凭据：stdout=%s stderr=%s", out, stderr)
	}
	var got accountTestJSON
	mustJSON(t, out, &got)
	if !strings.Contains(got.Message, "***") {
		t.Fatalf("证据不足：message 里的凭据应被替换成 ***，实际 %q", got.Message)
	}

	// last_error 会出现在 account ls --json 里，同样不能有明文。
	code, out, stderr = run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if strings.Contains(out, key) {
		t.Fatalf("account ls --json 泄漏了凭据: %s", out)
	}
}

// TestAccountAddRejectsPriceFlags 确认四个价格参数已经从 account add 上删掉：
// 单价搬到模型目录之后账号不再是价格的落点，误敲仍是用法错误（退出码 2），
// 文案说明价格在哪、并给出 model price set 的命令形状；一个账号都不落库。
func TestAccountAddRejectsPriceFlags(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// `--flag 值` 与 `--flag=值` 两种写法都要拦：先给 flag 包之前就认出来。
	for _, flag := range []string{"--price-in", "--price-out", "--price-cache-read", "--price-cache-write"} {
		for _, arg := range []string{flag, flag + "=-0.5"} {
			args := []string{"--key", "sk-1", "--models", "m1"}
			if arg == flag {
				args = append(args, flag, "2")
			} else {
				args = append(args, arg)
			}
			code, out, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", args...)
			if code != 2 || out != "" {
				t.Fatalf("%s 的退出码 = %d，stdout = %q，期望 2 与空 stdout", arg, code, out)
			}
			for _, want := range []string{flag, "lax model price set", "--in N --out N", "用法：lax account add"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("%s 的 stderr 应包含 %q: %s", arg, want, stderr)
				}
			}
		}
	}

	st, _ := mustStore(t)
	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("列账号: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("用法错误不该落库，实际 %d 个账号: %+v", len(accounts), accounts)
	}
}

// TestAccountSetRejectsPriceFlags 确认 account set 也不再收价格参数：单价在模型目录上，
// 改价走 model price set。误敲是用法错误（退出码 2），并把正确用法打出来；
// 账号的凭据与状态一个字节都不动。
func TestAccountSetRejectsPriceFlags(t *testing.T) {
	sandbox(t)
	mustInit(t)

	const key = "sk-old"
	if code, _, stderr := addAccount(t, "ds1", "mine", "http://127.0.0.1:9", "openai-chat", "--key", key, "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	st, cipherKey := mustStore(t)
	before, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("读账号: %v", err)
	}

	for _, flag := range []string{"--price-in", "--price-out", "--price-cache-read", "--price-cache-write"} {
		code, out, stderr := run(t, "account", "set", "ds1", flag, "1.5")
		if code != 2 || out != "" {
			t.Fatalf("%s 的退出码 = %d，stdout = %q，期望 2 与空 stdout", flag, code, out)
		}
		for _, want := range []string{flag, "lax model price set", "--in N --out N", "用法：lax account set"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%s 的 stderr 应包含 %q: %s", flag, want, stderr)
			}
		}
	}

	after, err := st.GetAccount("ds1")
	if err != nil {
		t.Fatalf("再读账号: %v", err)
	}
	if !bytes.Equal(before.KeyCipher, after.KeyCipher) {
		t.Fatal("用法错误却换了凭据密文")
	}
	if plain, err := secret.Decrypt(cipherKey, after.KeyCipher); err != nil || string(plain) != key {
		t.Fatalf("凭据被改动: %q, %v", plain, err)
	}
	// 账号上不再有价格：单价在目录里，account ls --json 也不再报价格字段。
	code, out, stderr := run(t, "account", "ls", "--json")
	if code != 0 {
		t.Fatalf("account ls 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, gone := range []string{"price_in", "price_out", "price_cache_read", "price_cache_write"} {
		if strings.Contains(out, gone) {
			t.Fatalf("account ls --json 不该再有 %s: %s", gone, out)
		}
	}
}
