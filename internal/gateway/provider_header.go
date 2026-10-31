package gateway

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"strings"

	"lax/internal/store"
)

// 厂商附加请求头的模板展开与注入。声明存在 provider_headers 里，值可能是字面量，
// 也可能是 {env:NAME}、{session}、{model} 三种占位符。展开只发生在发上游请求之前：入站响应、
// `/v1/models` 这类只读接口、以及 requests 记录都不受影响。
//
// 值可能是密钥（自定义鉴权头、会话 token），所以这一层写日志时只出头名与「已发送 / 未发送」，
// 不写值，也不把值拼进错误信息。占位符按整值匹配：值里出现别的文字一律当字面量原样发出去，
// 不做局部替换（局部替换会把「值里正好有一对花括号」这类情况变成猜谜）。

// 三种占位符的原文。{session} 在注入前要特判一次：没有会话键时得先现生成一个，
// 不能让展开函数自己去造随机数，否则展开就不再是纯函数、也就没法对拍。
const (
	headerSessionPlaceholder = "{session}"
	headerModelPlaceholder   = "{model}"
	headerEnvPrefix          = "{env:"
	headerEnvSuffix          = "}"
)

// providerHeaderInput 是一次模板展开的输入。把它做成参数而不是让展开函数直接读全局，
// 是为了让展开可单测：环境变量读取器由调用方注入，会话键与模型名由本次请求给出。
type providerHeaderInput struct {
	// Session 是本次请求的会话键，空串表示这次没有会话键。
	Session string
	// Model 是本次请求的模型名（裸名），空串表示这次请求没有模型名（模型列表请求就是这样）。
	Model string
	// LookupEnv 取环境变量，nil 时用 os.LookupEnv。用 LookupEnv 而不是 Getenv，
	// 是为了把「变量没设置」与「设置了但值是空串」分开报：两者都不发这个头，但原因不同。
	LookupEnv func(name string) (string, bool)
}

// expandProviderHeaderValue 展开一条声明的值。返回值非空表示要发这个头；
// 返回空串时 reason 说明为什么没发，供日志用——reason 只提到头名与原因，绝不带值。
// 函数是纯的：同样的输入永远算出同样的输出，不读环境、不取时间、不生成随机数。
func expandProviderHeaderValue(tmpl string, in providerHeaderInput) (value, reason string) {
	lookup := in.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	switch {
	case tmpl == headerSessionPlaceholder:
		if in.Session == "" {
			return "", "这次请求没有会话键，模板 {session} 展开为空"
		}
		value = in.Session
	case tmpl == headerModelPlaceholder:
		if in.Model == "" {
			return "", "这次请求没有模型名，模板 {model} 展开为空"
		}
		value = in.Model
	case strings.HasPrefix(tmpl, headerEnvPrefix) && strings.HasSuffix(tmpl, headerEnvSuffix):
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(tmpl, headerEnvPrefix), headerEnvSuffix))
		if name == "" {
			return "", "模板 {env:} 没有写环境变量名"
		}
		raw, ok := lookup(name)
		if !ok {
			return "", "环境变量 " + name + " 未设置"
		}
		if raw == "" {
			// 设置了但值是空串：与「没设置」分开报，两者都不发这个头。
			return "", "环境变量 " + name + " 是空串，不发空头"
		}
		value = raw
	default:
		value = tmpl
	}

	if value == "" {
		return "", "展开后是空串，不发空头"
	}
	// 环境变量的值里可能带折行：原样拼进请求头等于让配置自己拼出第二条头，
	// 所以这里拦下，只说第几个字节，不带值。
	if i := strings.IndexAny(value, "\r\n\x00"); i >= 0 {
		return "", fmt.Sprintf("展开后含回车、换行或 NUL（第 %d 个字节）", i+1)
	}
	return value, ""
}

// applyProviderHeaders 把 (厂商, 协议) 的有效附加头叠到已经配好凭据头的上游请求上。
// 顺序固定为「先 setAuth 设默认凭据头，再叠声明」：同名时声明覆盖默认。
// 冲突只在顺序上判一次，写入口不做判断。
//
// 读库失败时返回错误，不静默跳过：少一条附加头，上游直接回 400，静默跳过等于把一次配置故障
// 变成一句看不懂的协议错误。错误文案指出下一步跑 doctor。
func (s *Server) applyProviderHeaders(httpReq *http.Request, a store.Account,
	protocol, model, session string) error {

	headers, err := s.store.EffectiveProviderHeaders(a.Provider, protocol)
	if err != nil {
		return fmt.Errorf("读厂商 %s 的附加请求头失败: %w；先运行 lax doctor 检查数据库与配置",
			a.Provider, err)
	}

	for _, h := range headers {
		if h.Value == headerSessionPlaceholder && session == "" {
			// 没有会话键时给这次请求现生成一个：一条请求一个，不跨请求复用。
			session = newSessionID()
		}
		value, reason := expandProviderHeaderValue(h.Value, providerHeaderInput{
			Session:   session,
			Model:     model,
			LookupEnv: os.LookupEnv,
		})
		if value == "" {
			s.logf("附加头 %s 未发送：%s", h.Name, reason)
			continue
		}
		httpReq.Header.Set(h.Name, value)
	}
	return nil
}

// newSessionID 生成一个 UUID v4，给没有会话键的请求里的 {session} 用，每次调用都不同，
// 因此不能拿一个固定值或者会重复的计数器凑。
// 只用标准库：go.mod 里的 uuid 是别人的间接依赖，为一个 16 字节随机数把它转成直接依赖不值得。
// 熵源不可用时返回空串，调用方按「没发这个头」处理：宁可少一条头，也不拿时间或计数器凑一个
// 会重复的会话值。
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // 版本 4：时间高 4 位改成 0100，接收方据此知道要按 v4 解析
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 变体：高 2 位改成 10，不写就与旧版 UUID 撞形状
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
