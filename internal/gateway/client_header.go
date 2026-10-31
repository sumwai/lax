package gateway

import (
	"net/http"
	"strings"
)

// 客户端头透传。上游请求头不全量照抄入站头：照抄会把客户端自己的凭据送出去，
// 也会把 Host、Content-Length 这类逐跳头拼坏。但一个都不给也不行：客户端靠 anthropic-beta、
// openai-organization 一类头开关上游特性，全丢掉时客户端看到的是「特性没生效」而没有任何错误。
//
// 规则是白名单 + 只在同协议时透传。这一层是纯函数：给定入站头与两个协议，算出发往上游的头，
// 不读环境、不取时间、不发请求、不写日志。被丢掉的超长条目由返回值交给调用方记日志。

// clientHeaderMaxBytes 是单个客户端头值的上限（8KB）。异常客户端把一个头写爆不应该变成
// 上游的 431，丢弃比照着转发更安全。
const clientHeaderMaxBytes = 8 << 10

// clientHeaderRule 是一条透传规则：入站协议与有效出站协议相同时，按 names 的顺序透传这些头。
// 用有序切片而不是 map，是为了让同名多值的 Add 顺序与日志顺序都可预期。
type clientHeaderRule struct {
	protocol string
	names    []string
}

// clientHeaderRules 是白名单表。表里没有的协议（gemini 与 systemone）
// 不透传任何头：gemini 的特性开关在查询串与请求体里，不在头里；systemone 是一份决策请求，
// 没有靠头开关的特性，一个头都不透传。
var clientHeaderRules = []clientHeaderRule{
	{protocol: ProtocolAnthropic, names: []string{"anthropic-beta", "anthropic-version"}},
	{protocol: ProtocolChat, names: []string{"openai-beta", "openai-organization", "openai-project"}},
	{protocol: ProtocolResponses, names: []string{"openai-beta", "openai-organization", "openai-project"}},
}

// neverForwardHeaders 是永远不从入站请求带到上游的头。白名单本身已经把
// 这些头挡在外面，这张名单是第二道闸：以后往白名单里加名字时，误加同名头仍会被挡下。
//
//   - 凭据类：客户端的凭据不是我们的凭据，带上去只会让上游按客户端的身份鉴权，或者把
//     客户端自己的 key 泄漏给另一家厂商。
//   - 逐跳头：它们描述的是「这一段连接」，交给 http.Client 自己算。
//   - x-lax-session：本网关自己的会话键，只用于选号粘性，不进上游。
var neverForwardHeaders = []string{
	"Authorization", "x-api-key", "x-goog-api-key", "Cookie",
	"Host", "Connection", "Content-Length", "Transfer-Encoding", "Expect", "Upgrade",
	"x-lax-session",
}

// clientHeaderDropped 是一条因为超长被丢弃的客户端头：只带头名与长度，不带值。
type clientHeaderDropped struct {
	Name  string
	Bytes int
}

// isNeverForwardedHeader 判断头名是否在「永远不透传」名单里。头名一律小写比较：
// 白名单与名单本身都写成小写，http.Header 的键在 Add 时才规范化。
func isNeverForwardedHeader(name string) bool {
	for _, blocked := range neverForwardHeaders {
		if strings.EqualFold(blocked, name) {
			return true
		}
	}
	return false
}

// applyClientHeaders 把客户端白名单头叠到已经设好凭据头的上游请求上，返回被丢弃的超长条目。
// 调用顺序固定在 setAuth 之后、厂商附加头之前：顺序就是优先级，客户端头压过 setAuth 的默认值
// （例如 anthropic-version），厂商附加头再压过客户端头。
//
// 入站协议与有效出站协议不同时一个都不透传：anthropic-beta 发给 chat 上游只会换回 400。
// 同名多值按出现顺序全部 Add 上去（anthropic-beta 可以重复出现表示同时开多个 beta）；
// 取不到就不设，空值也不设，anthropic-version 因此保持 setAuth 写下的默认值。
func applyClientHeaders(httpReq *http.Request, inboundHeaders http.Header,
	inbound, outbound string) []clientHeaderDropped {

	if inbound != outbound {
		return nil
	}
	var dropped []clientHeaderDropped
	for _, rule := range clientHeaderRules {
		if rule.protocol != inbound {
			continue
		}
		for _, name := range rule.names {
			if isNeverForwardedHeader(name) {
				continue
			}
			values := inboundHeaders.Values(name)
			if len(values) == 0 {
				continue
			}
			// 只要有一个值超长就整条头都不发：半条头会让上游按缺省行为处理，
			// 比一个都不发更难排查。
			if n, ok := oversizedHeaderValue(values); ok {
				dropped = append(dropped, clientHeaderDropped{Name: name, Bytes: n})
				continue
			}
			// 同名多值按出现顺序全部 Add 上去；加之前先删掉已有同名头，否则 setAuth 写下的
			// anthropic-version 会与客户端的值并存，上游取到的是先写的那个默认值。
			kept := make([]string, 0, len(values))
			for _, v := range values {
				if v == "" {
					// 空值不设：设上去等于把「客户端给了空串」变成「客户端给了一个空头」。
					continue
				}
				kept = append(kept, v)
			}
			if len(kept) == 0 {
				continue
			}
			httpReq.Header.Del(name)
			for _, v := range kept {
				httpReq.Header.Add(name, v)
			}
		}
	}
	return dropped
}

// oversizedHeaderValue 返回第一个超过上限的值的长度。
func oversizedHeaderValue(values []string) (int, bool) {
	for _, v := range values {
		if len(v) > clientHeaderMaxBytes {
			return len(v), true
		}
	}
	return 0, false
}
