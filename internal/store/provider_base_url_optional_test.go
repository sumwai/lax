package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"switchboard/internal/secret"
	"switchboard/internal/store"
)

// bigmodelRoutes 是真实上游 bigmodel 的形状：三条协议分属不同 host 与路径，
// 模型列表在 chat 那条路径的同级（`/api/coding/paas/v4/models`），`base_url` 拼不进任何地址。
func bigmodelRoutes() []store.ProviderRoute {
	return []store.ProviderRoute{
		{Protocol: "anthropic", Path: "https://open.bigmodel.cn/api/anthropic/v1/messages"},
		{Protocol: "openai-chat", Path: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions"},
		{Protocol: "openai-responses", Path: "https://open.bigmodel.cn/api/v1/responses"},
	}
}

// TestCreateProviderAllowsEmptyBaseURLWithFullAddressRoutes 钉住 base_url 唯一可以省略的前提：
// 每一条路由与要落库的 models_path 都是整条地址。落库的 base_url 是空串，路由原样保留整条地址，
// chat 路由的列表地址在同一个 scheme + host 上推导，读取侧（LookupProviderRoute 与
// ResolveModelsPath）拿到的都是整条地址，因此转发与同步都不需要 base_url 参与拼地址。
func TestCreateProviderAllowsEmptyBaseURLWithFullAddressRoutes(t *testing.T) {
	st := openTemp(t)

	if err := st.CreateProvider(store.Provider{Name: "bigmodel", Note: "三套协议分属不同路径"}, bigmodelRoutes()); err != nil {
		t.Fatalf("三条整条地址路由、无 base_url 的新建应成功: %v", err)
	}

	p, err := st.GetProvider("bigmodel")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "" {
		t.Fatalf("base_url = %q，期望空串（没有地址可以拼）", p.BaseURL)
	}
	if p.ModelsPath != "" {
		t.Fatalf("厂商级 models_path = %q，期望留空按路由取", p.ModelsPath)
	}

	want := map[string]string{
		"anthropic":        "https://open.bigmodel.cn/api/anthropic/v1/messages",
		"openai-chat":      "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		"openai-responses": "https://open.bigmodel.cn/api/v1/responses",
	}
	for protocol, path := range want {
		route, ok, err := st.LookupProviderRoute("bigmodel", protocol)
		if err != nil || !ok {
			t.Fatalf("取 %s 路由: ok=%v err=%v", protocol, ok, err)
		}
		if route.Path != path {
			t.Fatalf("%s 路由的 path = %q，期望 %q", protocol, route.Path, path)
		}
		if route.Provider != "bigmodel" {
			t.Fatalf("%s 路由的厂商 = %q", protocol, route.Provider)
		}
	}

	// 模型列表从 chat 路由推导：同一个 scheme + host 上换尾段，不换域名、不拼 base_url。
	chat, ok, err := st.LookupProviderRoute("bigmodel", "openai-chat")
	if err != nil || !ok {
		t.Fatalf("取 chat 路由: ok=%v err=%v", ok, err)
	}
	const listURL = "https://open.bigmodel.cn/api/coding/paas/v4/models"
	if chat.ModelsPath != listURL {
		t.Fatalf("chat 路由的 models_path = %q，期望 %q", chat.ModelsPath, listURL)
	}

	// 读取侧：三条路由的 models_path 里只有 chat 那条非空，其它协议的账号都回退到它，
	// 拿到的仍是整条地址（gateway 拼地址时因此不碰 base_url）。
	routes, err := st.ProviderRoutes("bigmodel")
	if err != nil {
		t.Fatalf("取路由清单: %v", err)
	}
	for _, protocol := range []string{"anthropic", "openai-chat", "openai-responses"} {
		path, owner := store.ResolveModelsPath(p.ModelsPath, routes, protocol)
		if path != listURL {
			t.Fatalf("%s 账号取到的模型列表地址 = %q，期望 %q", protocol, path, listURL)
		}
		if owner != "openai-chat" {
			t.Fatalf("%s 账号取到的列表地址所属协议 = %q，期望 openai-chat", protocol, owner)
		}
	}

	// 默认协议仍按固定顺序取第一条：chat 在，所以是 chat。
	def, err := st.EffectiveProviderProtocol("bigmodel")
	if err != nil {
		t.Fatalf("默认协议: %v", err)
	}
	if def != "openai-chat" {
		t.Fatalf("默认协议 = %q，期望 openai-chat", def)
	}
}

// TestCreateProviderRejectsEmptyBaseURLWithRelativeRoute 钉住另一面：只要有一条路由是相对路径，
// 缺 base_url 就是用法错误（哨兵 ErrBaseURLRequired，cli 据此给退出码 2），文案说清是哪条协议、
// 两条出路（给 --base-url 或写成整条地址），并且一个字段都不落库。
func TestCreateProviderRejectsEmptyBaseURLWithRelativeRoute(t *testing.T) {
	st := openTemp(t)

	err := st.CreateProvider(store.Provider{Name: "mixed"}, []store.ProviderRoute{
		{Protocol: "openai-chat", Path: "https://openai.example.com/v1/chat/completions"},
		{Protocol: "anthropic", Path: "/v1/messages"},
	})
	if err == nil {
		t.Fatal("有一条相对路径时缺 base_url 应报错")
	}
	if !errors.Is(err, store.ErrBaseURLRequired) {
		t.Fatalf("错误应可用 errors.Is 判成 ErrBaseURLRequired: %v", err)
	}
	for _, want := range []string{"anthropic", "/v1/messages", "相对路径", "--base-url", "整条地址"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案 %q 缺少 %q", err.Error(), want)
		}
	}
	if _, err := st.GetProvider("mixed"); err == nil {
		t.Fatal("被拒的新建不该留下厂商行")
	}
	if routes, err := st.ProviderRoutes("mixed"); err != nil || len(routes) != 0 {
		t.Fatalf("被拒的新建不该留下路由: %+v err=%v", routes, err)
	}

	// 厂商级 models_path 是相对路径时同样整笔拒绝：它也要靠 base_url 拼。
	err = st.CreateProvider(store.Provider{Name: "lists", ModelsPath: "/v1/models"}, bigmodelRoutes())
	if err == nil || !errors.Is(err, store.ErrBaseURLRequired) {
		t.Fatalf("相对路径的厂商级 models_path 应报 ErrBaseURLRequired，实际 %v", err)
	}
	for _, want := range []string{"模型列表地址", "/v1/models", "整条地址"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案 %q 缺少 %q", err.Error(), want)
		}
	}
	if _, err := st.GetProvider("lists"); err == nil {
		t.Fatal("被拒的新建不该留下厂商行")
	}
}

// TestSetProviderKeepsBaseURLCoverage 钉住 `provider set` 的两种结果：库里没有 base_url 时，
// 把一条路由从整条地址改成相对路径整笔拒绝（同一事务回滚，原来那条整条地址还在）；
// 先补上 base_url，同一条改动就放行，落库的是相对路径、转发时拼这个 base_url——
// 老行为（给了 base_url 之后相对路径照旧拼它）一字不变。
func TestSetProviderKeepsBaseURLCoverage(t *testing.T) {
	st := openTemp(t)
	if err := st.CreateProvider(store.Provider{Name: "bigmodel"}, bigmodelRoutes()); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}

	err := st.SetProvider("bigmodel", store.ProviderPatch{
		Routes: map[string]string{"anthropic": "/api/anthropic/v1/messages"},
	})
	if err == nil {
		t.Fatal("库里没有 base_url 时把整条地址改成相对路径应整笔拒绝")
	}
	if !errors.Is(err, store.ErrBaseURLRequired) {
		t.Fatalf("错误应可用 errors.Is 判成 ErrBaseURLRequired: %v", err)
	}
	route, ok, err := st.LookupProviderRoute("bigmodel", "anthropic")
	if err != nil || !ok {
		t.Fatalf("取 anthropic 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "https://open.bigmodel.cn/api/anthropic/v1/messages" {
		t.Fatalf("被拒的改动不该落库，anthropic 路由 = %q", route.Path)
	}

	// 同一次调用里带上 base_url 就放行：改地址与改路由在一个事务里。
	base := "https://open.bigmodel.cn"
	if err := st.SetProvider("bigmodel", store.ProviderPatch{
		BaseURL: &base,
		Routes:  map[string]string{"anthropic": "/api/anthropic/v1/messages"},
	}); err != nil {
		t.Fatalf("带上 base_url 之后应放行: %v", err)
	}
	p, err := st.GetProvider("bigmodel")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != base {
		t.Fatalf("base_url = %q，期望 %q", p.BaseURL, base)
	}
	route, ok, err = st.LookupProviderRoute("bigmodel", "anthropic")
	if err != nil || !ok {
		t.Fatalf("取 anthropic 路由: ok=%v err=%v", ok, err)
	}
	if route.Path != "/api/anthropic/v1/messages" {
		t.Fatalf("anthropic 路由 = %q，期望相对路径原样落库", route.Path)
	}

	// 有 base_url 之后，整条地址改成相对路径不再需要 address 覆盖：一条条改都放行。
	if err := st.SetProvider("bigmodel", store.ProviderPatch{
		Routes: map[string]string{"openai-responses": "/api/v1/responses"},
	}); err != nil {
		t.Fatalf("有 base_url 时改成相对路径应放行: %v", err)
	}
}

// TestAddProviderRequiresBaseURLCoverage 确认第二个写入口同一规则：整条地址路由、零路由加空地址
// 与相对路径加空地址三种形状里，只有「每条路由都是整条地址」放行。
func TestAddProviderRequiresBaseURLCoverage(t *testing.T) {
	st := openTemp(t)

	if err := st.AddProvider(store.Provider{Name: "full"}, bigmodelRoutes()); err != nil {
		t.Fatalf("AddProvider 收三条整条地址路由、无 base_url 应成功: %v", err)
	}
	p, err := st.GetProvider("full")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	if p.BaseURL != "" {
		t.Fatalf("base_url = %q，期望空串", p.BaseURL)
	}

	// 零路由加空地址：无处转发，整笔拒绝。
	if err := st.AddProvider(store.Provider{Name: "nothing"}, nil); err == nil || !errors.Is(err, store.ErrBaseURLRequired) {
		t.Fatalf("零路由加空地址应报 ErrBaseURLRequired，实际 %v", err)
	}
	// 相对路径加空地址：同样整笔拒绝。
	if err := st.AddProvider(store.Provider{Name: "rel"},
		[]store.ProviderRoute{{Protocol: "openai-chat", Path: "/v1/chat/completions"}}); err == nil || !errors.Is(err, store.ErrBaseURLRequired) {
		t.Fatalf("相对路径加空地址应报 ErrBaseURLRequired，实际 %v", err)
	}
	for _, name := range []string{"nothing", "rel"} {
		if _, err := st.GetProvider(name); err == nil {
			t.Fatalf("被拒的 %s 不该留下厂商行", name)
		}
	}
}

// TestBaseURLLessProviderKeepsCredentialCiphertextOnly 钉住凭据只以密文出现：
// 无 base_url 的厂商行、它的路由行与引用它的账号行一起序列化时，明文 key 一次都不出现。
// 这条路径新增了「地址为空」的分支，凭据边界不该因此变松。
func TestBaseURLLessProviderKeepsCredentialCiphertextOnly(t *testing.T) {
	st := openTemp(t)
	const key = "sk-base-url-less-must-not-appear"

	if err := st.CreateProvider(store.Provider{Name: "bigmodel"}, bigmodelRoutes()); err != nil {
		t.Fatalf("新建厂商: %v", err)
	}

	// 加密用的密钥文件由 secret 自己生成，与 cli 的路径无关：这里只关心落库的形状。
	secretKey, err := secret.LoadOrCreateKey(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	cipher, err := secret.Encrypt(secretKey, []byte(key))
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	acc := store.Account{
		Name: "bm1", Provider: "bigmodel", Protocol: "openai-chat", BaseURL: "",
		KeyCipher: cipher, Models: []string{"glm-4"}, Enabled: true,
	}
	if err := st.CreateAccount(&acc); err != nil {
		t.Fatalf("新增账号: %v", err)
	}

	p, err := st.GetProvider("bigmodel")
	if err != nil {
		t.Fatalf("取厂商: %v", err)
	}
	routes, err := st.ProviderRoutes("bigmodel")
	if err != nil {
		t.Fatalf("取路由: %v", err)
	}
	got, err := st.GetAccount("bm1")
	if err != nil {
		t.Fatalf("取账号: %v", err)
	}
	for name, v := range map[string]any{"provider": p, "routes": routes, "account": got} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化 %s: %v", name, err)
		}
		if strings.Contains(string(b), key) {
			t.Fatalf("%s 的序列化结果里出现了 key 明文: %s", name, b)
		}
		if strings.Contains(string(b), "sk-") {
			t.Fatalf("%s 的序列化结果里出现了形如 key 的明文: %s", name, b)
		}
	}
	// 解密回来的仍是原文：加密路径没有被这次改动碰到。
	plain, err := secret.Decrypt(secretKey, got.KeyCipher)
	if err != nil {
		t.Fatalf("解密: %v", err)
	}
	if string(plain) != key {
		t.Fatalf("解密出来的值不对")
	}
}
