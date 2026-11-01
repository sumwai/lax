package plugin

import (
	"fmt"
	"strings"
)

// 头名、凭据写法与插件请求头的规则都在这一个文件里。规则只有一份：manifest 的 auth 声明、
// provider 子命令的取值、沙箱对 Plan.Headers 的校验、网关最后叠加插件头，四者共用它。

// planHeaderLimit 是一次请求里插件能带的头上限。逐请求生成的头是几个（请求 ID、资源号、
// 序号），32 条足够，而数量上限是「插件不能拿它拼出一条自由报文」的第一道闸。
const (
	PlanHeaderMaxCount   = 32
	PlanHeaderMaxValue   = 4096
	planHeaderMaxTotal   = 16 << 10
	AuthHeaderMaxNameLen = 64
)

// planReservedHeaders 是插件不能设置的头。
//
// 两类：一是由 HTTP 帧本身决定的（host、content-length、transfer-encoding 等），插件改了
// 只会让报文畸形；二是凭据与会话（authorization、x-api-key、x-goog-api-key、cookie、
// x-lax-session）：凭据只能由宿主按 manifest 的 auth 声明注入，多开一个注入口等于让插件
// 自己选把哪串 key 发给谁。保留头在沙箱层直接拒绝，不静默丢弃——静默丢弃会让插件以为
// 它设的头发出去了。
var planReservedHeaders = map[string]bool{
	"host":                true,
	"content-length":      true,
	"connection":          true,
	"keep-alive":          true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"cookie":              true,
	"set-cookie":          true,
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
	"x-lax-session":       true,
}

// IsReservedPlanHeader 判断一个（已归一小写的）头名是不是插件不能设的保留头。
// proxy-* 前缀在这里单独判：那一族是代理链上的头，插件不该碰。
func IsReservedPlanHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if planReservedHeaders[name] {
		return true
	}
	return strings.HasPrefix(name, "proxy-")
}

// NormalizeHeaderName 规整并校验一个头名：去空白、转小写，必须是非空的 HTTP token。
// 落库与转发用的都是小写形态，回显与比对因此不受大小写写法影响。
func NormalizeHeaderName(field, raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return "", fmt.Errorf("%s 为空：写一个头名，例如 X-Api-Key", field)
	}
	if !isHTTPToken(name) {
		return "", fmt.Errorf("%s 是 %q：头名只能是 HTTP token 允许的字符（字母、数字与 !#$%%&'*+-.^_`|~）", field, raw)
	}
	return name, nil
}

// NormalizeAuthHeader 规整一个凭据头名，并按头的长度上限检查。
func NormalizeAuthHeader(raw string) (string, error) {
	name, err := NormalizeHeaderName("auth.header", raw)
	if err != nil {
		return "", err
	}
	if len(name) > AuthHeaderMaxNameLen {
		return "", fmt.Errorf("auth.header 长 %d 字节，上限 %d", len(name), AuthHeaderMaxNameLen)
	}
	return name, nil
}

// NormalizeAuthScheme 规整凭据写法。header 为空时 scheme 必须也为空：没有头就没有地方写值。
// header 非空而 scheme 为空时归一到 raw（最直白的写法：头值就是 key 本身）。
func NormalizeAuthScheme(scheme, header string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(scheme))
	if header == "" {
		if value != "" {
			return "", fmt.Errorf("auth.scheme 是 %q，auth.header 却是空的：没有头就没有地方写凭据，先把 header 补上", scheme)
		}
		return "", nil
	}
	switch value {
	case "":
		return AuthSchemeRaw, nil
	case AuthSchemeRaw, AuthSchemeBearer, AuthSchemeBearerSemicolon:
		return value, nil
	}
	return "", fmt.Errorf("auth.scheme 是 %q：只能是 %s、%s 或 %s", scheme,
		AuthSchemeRaw, AuthSchemeBearer, AuthSchemeBearerSemicolon)
}

// IsAuthScheme 判断一个取值是不是认得的凭据写法。命令面只靠它做预检，
// 「有写法没头」这类跨字段的判定在写入侧用 NormalizeAuthScheme 做。
func IsAuthScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case AuthSchemeRaw, AuthSchemeBearer, AuthSchemeBearerSemicolon:
		return true
	}
	return false
}

// AuthHeaderValue 按声明的写法拼出凭据头的值。scheme 已由 NormalizeAuthScheme 规整，
// 空串按 raw 处理。
func AuthHeaderValue(scheme, key string) string {
	switch scheme {
	case AuthSchemeBearer:
		return "Bearer " + key
	case AuthSchemeBearerSemicolon:
		return "Bearer;" + key
	default:
		return key
	}
}

// isHTTPToken 判一个取值是不是 RFC 7230 的 token：tchar 集合之外一个字符都不许有。
// 头名与头值里的空白、冒号与逗号都靠这一条挡住：它们能把一个头拆成两条。
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '!', c == '#', c == '$', c == '%', c == '&', c == '\'', c == '*',
			c == '+', c == '-', c == '.', c == '^', c == '_', c == '`', c == '|', c == '~':
		default:
			return false
		}
	}
	return true
}

// CheckPlanHeaderValue 校验一个插件请求头的值：不能为空串、不能超过单值上限，
// 也不能带 CR、LF 与 NUL（它们能把一个头拆成两条报文）。
func CheckPlanHeaderValue(name, value string) error {
	if len(value) > PlanHeaderMaxValue {
		return fmt.Errorf("头 %s 的值有 %d 字节，超过 %d 字节上限", name, len(value), PlanHeaderMaxValue)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("头 %s 的值里有 CR、LF 或 NUL：这些字符能把一个头拆成两条报文", name)
	}
	return nil
}
