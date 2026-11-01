package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// LoadManifest 读 lax.plugin.json 并校验。file 只用于报错。
func LoadManifest(file string) (Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Manifest{}, fmt.Errorf("读插件清单 %s 失败：%w", file, err)
	}
	return DecodeManifest(file, data)
}

// DecodeManifest 严格解析并校验 manifest。未知字段直接拒绝：插件不能依赖宿主忽略它，
// 需要自定义内容时放进 extensions。
func DecodeManifest(file string, data []byte) (Manifest, error) {
	var m Manifest
	if err := decodeStrictJSON(data, &m); err != nil {
		return Manifest{}, decodeFailure(file, "manifest 不接受未知字段，自定义内容请放进 extensions", err)
	}
	if err := ValidateManifest(m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", file, err)
	}
	return m, nil
}

// decodeStrictJSON 拒绝未知字段与 JSON 值之后的多余内容。重复键没有开关可关，
// encoding/json 取后一个，重复绑定这类问题只能靠语义校验拦。
func decodeStrictJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("JSON 值之后还有内容")
	}
	return nil
}

// decodeFailure 给 encoding/json 的报错补上文件与字段路径。
func decodeFailure(file, unknownHint string, err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		where := typeErr.Field
		if where == "" {
			where = "顶层"
		}
		return fmt.Errorf("%s: %s 类型不对，要 %s，得到 %s", file, where, typeErr.Type, typeErr.Value)
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("%s: JSON 语法错误，第 %d 字节：%w", file, syntaxErr.Offset, err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: JSON 不完整，文件在结尾处被截断", file)
	}
	if name, ok := unknownField(err); ok {
		return fmt.Errorf("%s: 未知字段 %q：%s", file, name, unknownHint)
	}
	return fmt.Errorf("%s: %w", file, err)
}

// unknownField 从 encoding/json 的 unknown field 报错里取字段名。
func unknownField(err error) (string, bool) {
	const marker = "unknown field "
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(msg[i+len(marker):]), `"`), true
}

// supportedOperationNames 是第一版实现的操作，按 ABI 顺序列出，报错文案直接用它。
var supportedOperationNames = []string{
	OpChatComplete,
	OpEmbeddingsCreate,
	OpSpeechSynthesize,
	OpSpeechTranscribe,
	OpSystemDecide,
}

// knownPermissions 是 requires 里 permission 类认得的权限名。
var knownPermissions = []string{"network", "secrets", "media", "filesystem", "process", "env"}

// versionPattern 匹配三段数字版本，允许 -rc.1 这类后缀。
var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// ValidateManifest 校验 manifest 的静态规则。路径只判是否落在插件根内：
// 是否存在、是否符号链接逃逸要等安装流水线能看到真实目录才能判。
func ValidateManifest(m Manifest) error {
	if m.Schema != ManifestSchema {
		return fmt.Errorf("schema 是 %q：manifest 必须是 %q", m.Schema, ManifestSchema)
	}
	if m.ABI != ABIv1 {
		return fmt.Errorf("abi 是 %q，宿主支持的是 %q：主版本不一致的插件装不上，请按宿主 ABI 重写 manifest", m.ABI, ABIv1)
	}
	if m.ID == "" {
		return errors.New("id 为空：每个插件要有稳定且全局唯一的 ID，建议用反向域名，例如 com.example.mimo")
	}
	if err := checkName("id", m.ID, 128); err != nil {
		return err
	}
	if !versionPattern.MatchString(m.Version) {
		return fmt.Errorf("version 是 %q：要写成三段数字，例如 1.0.0", m.Version)
	}
	switch m.Kind {
	case KindProvider, KindTransform, KindAuth:
	default:
		return fmt.Errorf("kind 是 %q：只能是 %s、%s 或 %s", m.Kind, KindProvider, KindTransform, KindAuth)
	}
	if err := checkRuntime(m); err != nil {
		return err
	}
	if err := checkRequires(m.Requires); err != nil {
		return err
	}
	if err := validatePermissions(m.Permissions); err != nil {
		return err
	}
	if err := validateProviders(m.Kind, m.Providers); err != nil {
		return err
	}
	return validateOptions(m.Options)
}

// checkRuntime 按运行时判入口：声明式没有入口，moejs 必须有入口。
// 入口路径的错误比 kind 更根本，先报。
func checkRuntime(m Manifest) error {
	switch m.Runtime {
	case RuntimeDeclarative:
	case RuntimeMoejs:
	default:
		return fmt.Errorf("runtime 是 %q：只能是 %s 或 %s", m.Runtime, RuntimeDeclarative, RuntimeMoejs)
	}

	if m.Runtime == RuntimeDeclarative {
		if m.Entry != "" {
			return fmt.Errorf("runtime 是 declarative，entry 却是 %q：声明式插件没有入口文件，删掉 entry", m.Entry)
		}
		return nil
	}

	if m.Entry == "" {
		return fmt.Errorf("entry 为空：runtime 是 %s 的插件要写入口文件，相对插件根，例如 ./index.js", m.Runtime)
	}
	if err := checkPluginPath("entry", m.Entry); err != nil {
		return err
	}
	// 沙箱只给厂商用：变换与鉴权插件的钩子在本版没有调用点，装进去等于静默不生效。
	if m.Kind != KindProvider {
		return fmt.Errorf("kind 是 %q：runtime 是 %s 的插件只能是 %s，变换与鉴权插件还没有调用点",
			m.Kind, RuntimeMoejs, KindProvider)
	}
	return nil
}

// checkName 校验 ID 类字段共用的字符集。不允许斜杠与大写：斜杠只出现在 provider/model
// 拼接出来的模型名里，大小写不敏感的文件系统上会撞名。
func checkName(field, value string, limit int) error {
	switch {
	case strings.Contains(value, "/"):
		return fmt.Errorf("%s 是 %q：ID 里不能有斜杠", field, value)
	case len(value) > limit:
		return fmt.Errorf("%s 长 %d 字节，上限 %d", field, len(value), limit)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		case r >= 'A' && r <= 'Z':
			return fmt.Errorf("%s 是 %q：只能用 a-z0-9._-，大写会和小写撞名", field, value)
		default:
			return fmt.Errorf("%s 是 %q：只能用 a-z0-9._-，%q 不在其中", field, value, r)
		}
	}
	if strings.HasPrefix(value, ".") || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, ".") || strings.HasSuffix(value, "-") {
		return fmt.Errorf("%s 是 %q：首尾不能用 . 或 -", field, value)
	}
	return nil
}

// checkPluginPath 只判路径是否落在插件根内。
func checkPluginPath(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s 为空：相对插件根写文件路径，例如 ./models.json", field)
	}
	if path.IsAbs(value) {
		return fmt.Errorf("%s 是 %q：只能写插件根内的相对路径", field, value)
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s 是 %q：路径逃出插件根，只能是根内的文件", field, value)
	}
	return nil
}

// checkBaseURL 拦下带 userinfo 与 fragment 的地址：凭据由账号池注入，
// fragment 对 HTTP 请求没有意义，却常被用来越过白名单。
func checkBaseURL(field, value string) error {
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s 是 %q：不是合法 URL（%v）", field, value, err)
	}
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("%s 是 %q：只允许 https，明文 http 会把凭据与正文暴露在链路上", field, value)
	case u.Host == "":
		return fmt.Errorf("%s 是 %q：缺主机名", field, value)
	case u.User != nil:
		return fmt.Errorf("%s 是 %q：不能带 userinfo，凭据由账号池注入", field, value)
	case u.Fragment != "" || strings.Contains(value, "#"):
		return fmt.Errorf("%s 是 %q：不能带 fragment", field, value)
	}
	return nil
}

// checkRequires 按 ABI 协商：宿主不认识的要求在安装时就失败，不能留到请求时才报。
func checkRequires(requires []string) error {
	for i, req := range requires {
		class, name, ok := strings.Cut(req, ":")
		if !ok || class == "" || name == "" {
			return fmt.Errorf("requires[%d] 是 %q：每项要写成 <类>:<名>，类是 operation、runtime、permission", i, req)
		}
		switch class {
		case "operation":
			if !KnownOperation(name) {
				return fmt.Errorf("requires[%d] 要求 %q，宿主不认识；第一版支持的操作是 %s", i, name, strings.Join(supportedOperationNames, "、"))
			}
		case "runtime":
			switch name {
			case RuntimeDeclarative, RuntimeMoejs:
			default:
				return fmt.Errorf("requires[%d] 的 runtime %q 不认识：只能是 declarative 或 moejs", i, name)
			}
		case "permission":
			if !slices.Contains(knownPermissions, name) {
				return fmt.Errorf("requires[%d] 的权限 %q 不认识：可用的是 %s", i, name, strings.Join(knownPermissions, "、"))
			}
		default:
			return fmt.Errorf("requires[%d] 的类 %q 不认识：只用 operation、runtime、permission", i, class)
		}
	}
	return nil
}

func validatePermissions(p Permissions) error {
	if err := checkNoneOnly("permissions.filesystem", p.Filesystem); err != nil {
		return err
	}
	if err := checkNoneOnly("permissions.process", p.Process); err != nil {
		return err
	}
	if err := checkNoneOnly("permissions.env", p.Env); err != nil {
		return err
	}
	if err := validateNetworkPerm(p.Network); err != nil {
		return err
	}
	return validateSecretsPerm(p.Secrets)
}

// checkNoneOnly：插件代码只做纯计算，没有文件、进程与环境变量访问面。
func checkNoneOnly(field, value string) error {
	if value == "" || value == "none" {
		return nil
	}
	return fmt.Errorf("%s 是 %q：插件读不到文件、进程与环境变量，只能写 %q", field, value, "none")
}

func validateNetworkPerm(n NetworkPerm) error {
	switch n.Mode {
	case "", "none":
	case "host-proxy":
		if len(n.Hosts) == 0 {
			return errors.New("permissions.network.mode 是 host-proxy，hosts 却为空：白名单是控制面唯一的出口，至少要写一个主机")
		}
	default:
		return fmt.Errorf("permissions.network.mode 是 %q：只能是空、none 或 host-proxy", n.Mode)
	}
	for i, scheme := range n.Schemes {
		if scheme != "https" {
			return fmt.Errorf("permissions.network.schemes[%d] 是 %q：控制面只允许 https", i, scheme)
		}
	}
	for i, host := range n.Hosts {
		if err := checkNetworkHost(fmt.Sprintf("permissions.network.hosts[%d]", i), host); err != nil {
			return err
		}
	}
	switch n.Redirects {
	case "", "none", "same-host":
	default:
		return fmt.Errorf("permissions.network.redirects 是 %q：只能是空、none 或 same-host", n.Redirects)
	}
	return nil
}

// checkNetworkHost 只接受具体主机名：裸通配符等于没有白名单，scheme 与路径在别处声明。
func checkNetworkHost(field, host string) error {
	switch {
	case host == "":
		return fmt.Errorf("%s 为空：删掉这一项，或写具体主机名", field)
	case host == "*":
		return fmt.Errorf("%s 是 *：裸通配符等于不设白名单，写具体主机，例如 api.example.com", field)
	case strings.Contains(host, "://"):
		return fmt.Errorf("%s 是 %q：这里只写主机名，不带 scheme", field, host)
	case strings.Contains(host, "/"):
		return fmt.Errorf("%s 是 %q：这里只写主机名，路径交给厂商的 base_url", field, host)
	case strings.IndexFunc(host, unicode.IsSpace) >= 0:
		return fmt.Errorf("%s 是 %q：主机名里不能有空白", field, host)
	}
	return nil
}

func validateSecretsPerm(s SecretsPerm) error {
	switch s.Mode {
	case "", "none", "host-inject":
	default:
		return fmt.Errorf("permissions.secrets.mode 是 %q：只能是空、none 或 host-inject，插件看不到凭据原文", s.Mode)
	}
	for i, item := range s.Account {
		switch item {
		case "api_key", "bearer":
		default:
			return fmt.Errorf("permissions.secrets.account[%d] 是 %q：本版只注入 api_key 与 bearer", i, item)
		}
	}
	return nil
}

func validateProviders(kind string, providers []ProviderDecl) error {
	if kind == KindProvider && len(providers) == 0 {
		return errors.New("providers 为空：kind 是 provider 的插件至少要声明一家厂商")
	}
	seen := make(map[string]int, len(providers))
	for i, p := range providers {
		field := fmt.Sprintf("providers[%d]", i)
		if p.ID == "" {
			return fmt.Errorf("%s.id 为空：每家在 lax 里要有唯一 ID，模型名按 provider/model 拼接", field)
		}
		if err := checkName(field+".id", p.ID, 128); err != nil {
			return err
		}
		if prev, dup := seen[p.ID]; dup {
			return fmt.Errorf("%s.id 与 providers[%d].id 都是 %q：provider ID 在 lax 内唯一", field, prev, p.ID)
		}
		seen[p.ID] = i
		for _, file := range []struct{ name, value string }{
			{"catalog", p.Catalog},
			{"dialects", p.Dialects},
		} {
			if file.value == "" {
				continue
			}
			if err := checkPluginPath(field+"."+file.name, file.value); err != nil {
				return err
			}
		}
		if p.BaseURL != "" {
			if err := checkBaseURL(field+".base_url", p.BaseURL); err != nil {
				return err
			}
		}
		if _, _, err := p.NormalizedAuth(); err != nil {
			return fmt.Errorf("%s.%w", field, err)
		}
		if err := validateRouteDecls(field+".routes", p.Routes); err != nil {
			return err
		}
	}
	return nil
}

// NormalizedAuth 返回规整后的凭据头声明（头名小写、scheme 归一）。
// 未声明 auth 时两项都是空串，表示按线协议推导：转发行为与没有这一项的老版本一致。
func (p ProviderDecl) NormalizedAuth() (header, scheme string, err error) {
	if p.Auth == nil {
		return "", "", nil
	}
	if strings.TrimSpace(p.Auth.Header) == "" {
		// 先报更具体的那一条：既没头又写了写法时，缺的是头。
		if strings.TrimSpace(p.Auth.Scheme) != "" {
			_, err = NormalizeAuthScheme(p.Auth.Scheme, "")
			return "", "", err
		}
		return "", "", errors.New("auth.header 为空：写一个头名，例如 X-Api-Key")
	}
	header, err = NormalizeAuthHeader(p.Auth.Header)
	if err != nil {
		return "", "", err
	}
	scheme, err = NormalizeAuthScheme(p.Auth.Scheme, header)
	if err != nil {
		return "", "", err
	}
	return header, scheme, nil
}

// validateRouteDecls 校验 manifest 里的 routes：协议必须是已知线协议且不重复，路径非空。
// 路径的规整与整条地址的校验不在这里做：它走 store.prepareRoutes 那一份（与 provider 子命令
// 同一个入口），否则同一串路径在插件安装与手工声明上会有两种命运。
func validateRouteDecls(field string, routes []RouteDecl) error {
	seen := make(map[string]int, len(routes))
	for i, r := range routes {
		item := fmt.Sprintf("%s[%d]", field, i)
		protocol := strings.TrimSpace(r.Protocol)
		if protocol == "" {
			return fmt.Errorf("%s.protocol 为空：写线协议全名，例如 openai-speech", item)
		}
		if !KnownWire(protocol) {
			return fmt.Errorf("%s.protocol 是 %q：只能是 %s", item, r.Protocol, strings.Join(knownWireNames(), "、"))
		}
		if prev, dup := seen[protocol]; dup {
			return fmt.Errorf("%s.protocol 与 %s[%d].protocol 都是 %q：一家厂商的每条协议只有一条路由（主键是厂商加协议）",
				item, field, prev, protocol)
		}
		seen[protocol] = i
		if strings.TrimSpace(r.Path) == "" {
			return fmt.Errorf("%s.path 为空：写相对 base_url 的路径（例如 /v1/audio/speech），或整条地址", item)
		}
	}
	return nil
}

func validateOptions(options map[string]OptionDecl) error {
	if _, ok := options[""]; ok {
		return errors.New("options 里有空名字的配置项：plugin config set 靠名字定位")
	}
	for _, name := range slices.Sorted(maps.Keys(options)) {
		decl := options[name]
		field := "options." + name
		switch decl.Type {
		case "string", "number", "boolean":
		default:
			return fmt.Errorf("%s.type 是 %q：只能是 string、number 或 boolean", field, decl.Type)
		}
		if len(decl.Enum) > 0 && decl.Type != "string" {
			return fmt.Errorf("%s.enum 配在 type %s 上：枚举只配 string", field, decl.Type)
		}
		if len(decl.Default) == 0 {
			continue
		}
		if err := checkOptionDefault(field, decl); err != nil {
			return err
		}
	}
	return nil
}

func checkOptionDefault(field string, decl OptionDecl) error {
	var raw any
	if err := json.Unmarshal(decl.Default, &raw); err != nil {
		return fmt.Errorf("%s.default 不是合法 JSON：%w", field, err)
	}
	matched := false
	switch decl.Type {
	case "string":
		_, matched = raw.(string)
	case "number":
		_, matched = raw.(float64)
	case "boolean":
		_, matched = raw.(bool)
	}
	if !matched {
		return fmt.Errorf("%s.default 是 %s，与 type %s 不符：默认值要从同一个类型来", field, jsonKind(raw), decl.Type)
	}
	if len(decl.Enum) == 0 {
		return nil
	}
	text, ok := raw.(string)
	if ok && !slices.Contains(decl.Enum, text) {
		return fmt.Errorf("%s.default 是 %q，不在 enum 里：默认值要是枚举中的一个", field, text)
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "字符串"
	case float64:
		return "数字"
	case bool:
		return "布尔"
	case nil:
		return "null"
	case []any:
		return "数组"
	case map[string]any:
		return "对象"
	}
	return "未知类型"
}
