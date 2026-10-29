package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"switchboard/internal/translate"
)

// systemone 决策协议（DESIGN 5.14）。它不是对话：输入一个 state 加一组带名字的 questions，
// 输出每个问题的答案与概率，不产文本。因此这条协议只与自己互通、没有流式，也没有消息序列
// 可以进中间表示（DESIGN 5.4 的中间表示只服务其余四条）。这个文件里的东西都是纯函数：
// 输入请求体或两个协议名，输出判定，不读环境、不发请求、不读文件、不碰时间。

// systemOnePath 是入站路径。模型名在请求体里，不在路径上（DESIGN 5.14 的「入站路径」）。
const systemOnePath = "/v1/systemone"

// systemOneMaxBody 是本条协议入站请求体的上限（8 MiB）。上游自己的 32 MiB、64 层嵌套与
// 有限数这些规则不在这里复制一份（DESIGN 5.14 的「最少校验」），但读入必须有上限：
// 不加一条上限时一个坏客户端能把网关的内存吃满。
const systemOneMaxBody = 8 << 20

// systemOneNoStreamMessage 是请求体带 `"stream": true` 时的拒绝文案。说清「这条协议没有流式」
// 与下一步改成什么：静默当同步会让等 SSE 的客户端挂到超时（DESIGN 5.14 的「流式」）。
// 不在这里再写协议名：调用方统一给解析错误加 "systemone: " 前缀。
const systemOneNoStreamMessage = "这条协议没有流式：一个请求只回一个 JSON 响应，" +
	"把请求体里的 \"stream\" 去掉（或改成 false）再发一次"

// inboundProtocol 按入站路径定协议（DESIGN 5.2）。与 translate.PathProtocol 只差 systemone
// 这一条：它的报文与其余四条不相通，translate 的编解码器也不认它（DESIGN 5.14），
// 所以这条路径在网关这一层认，不进 translate 的协议分发。
func inboundProtocol(path string) (string, bool) {
	if path == systemOnePath {
		return ProtocolSystemOne, true
	}
	return translate.PathProtocol(path)
}

// inboundPath 给出某条协议客户端该用的入站路径，只用于错误文案里的下一步命令。
// 认不出的协议返回空串，调用方据此不写半句话。
func inboundPath(protocol string) string {
	switch protocol {
	case ProtocolAnthropic:
		return "/v1/messages"
	case ProtocolChat:
		return "/v1/chat/completions"
	case ProtocolResponses:
		return "/v1/responses"
	case ProtocolGemini:
		return geminiGeneratePath
	case ProtocolSystemOne:
		return systemOnePath
	}
	return ""
}

// protocolInteropError 判断入站协议与这次算出的有效出站协议能不能互通，返回 400 的文案；
// 空串表示可以转发。判定只有这一处，两个方向共用它（DESIGN 5.14 的「协议互通」）：
//
//   - 入站是 systemone、有效出站协议是另外四种：这条协议只与自己互通；
//   - 入站是那四条、有效出站协议是 systemone：同样不相通。
//
// 不试图经中间表示转换：决策请求没有对话消息序列，转换规则无从定义，硬转只会把一条决策
// 请求发成一条错协议的请求（DESIGN 5.4 的中间表示服务的是其余四条）。
func protocolInteropError(inbound, outbound, model string) string {
	if inbound == outbound {
		return ""
	}
	if inbound != ProtocolSystemOne && outbound != ProtocolSystemOne {
		return ""
	}
	if outbound == ProtocolSystemOne {
		return fmt.Sprintf("模型 %s 的有效出站协议是 systemone，与 %s 入站不相通："+
			"决策请求只与自己互通，没有对话消息序列可以经中间表示转换；"+
			"决策请求用 POST %s 发，%s 的请求改用走其它协议的模型",
			model, inbound, systemOnePath, inbound)
	}
	return fmt.Sprintf("模型 %s 的有效出站协议是 %s，与 systemone 入站不相通："+
		"这条协议只与自己互通，决策请求没有对话消息序列可以转换；"+
		"对话请求用 POST %s 发，或给提供这个模型的厂商配一条 systemone 路由"+
		"（switchboard provider set <厂商> %s）",
		model, outbound, inboundPath(outbound), providerPathHint(ProtocolSystemOne))
}

// decodeSystemOneRequest 做最少校验，返回选号要用的模型名。规则只有三条（DESIGN 5.14 的
// 「最少校验」）：`model` 是非空字符串、`questions` 是非空对象、不要求流式。
// 其余字段（state、questions 的内容、assets）不解析、不改写：转发时原样带走。
func decodeSystemOneRequest(raw []byte) (*translate.Request, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %v", err)
	}
	if fields == nil {
		return nil, errors.New("请求体是 null：决策请求是 {\"model\":\"…\",\"state\":…,\"questions\":{…}} 这样的对象")
	}

	modelField, ok := fields["model"]
	if !ok || isJSONNull(modelField) {
		return nil, errors.New("缺少 model 字段：决策请求的模型名写在请求体的 model 字段里，不在路径上")
	}
	var model string
	if err := json.Unmarshal(modelField, &model); err != nil {
		return nil, errors.New("model 必须是字符串：决策请求按模型名选号，写对象或数组都选不出账号")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model 不能是空字符串：决策请求按模型名选号，空名字匹配不上任何账号")
	}

	questionsField, ok := fields["questions"]
	if !ok || isJSONNull(questionsField) {
		return nil, errors.New("缺少 questions 字段：至少给一个问题，形如 " +
			"{\"ok\":{\"type\":\"noul\",\"instructions\":\"Is this text present?\"}}")
	}
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(questionsField, &questions); err != nil {
		return nil, errors.New("questions 必须是对象：形状是 {\"问题名\":{\"type\":\"noul\",\"instructions\":\"…\"}}")
	}
	if len(questions) == 0 {
		return nil, errors.New("questions 不能是空对象：至少给一个问题，" +
			"形状是 {\"问题名\":{\"type\":\"noul\",\"instructions\":\"…\"}}")
	}

	// 只把布尔 true 当流式请求：别的取值（字符串 "true" 一类）不是这条协议的写法，
	// 交给上游按它自己的理由拒绝，不在这里替它判。
	if streamField, ok := fields["stream"]; ok && string(bytes.TrimSpace(streamField)) == "true" {
		return nil, errors.New(systemOneNoStreamMessage)
	}
	return &translate.Request{Model: model}, nil
}

// isJSONNull 判断一个原始 JSON 值是不是 null。缺字段与 null 在这里同义：两者都没有可用的值。
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// systemOneUsage 读响应体里的 usage.input_tokens 与 usage.output_tokens。响应体本身不做任何
// 改写（直通），这里只取这两个数进请求记录；这条协议没有缓存与思考字段（DESIGN 5.14），
// 输入 token 因此按不含缓存处理。
func systemOneUsage(body []byte) (in, out int64) {
	var env struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, 0
	}
	return env.Usage.InputTokens, env.Usage.OutputTokens
}

// systemOneProbeBody 是一条最小决策请求：state 是 ping，一个问题（noul 型）问一句话在不在
// （DESIGN 5.14 的「探活」）。探活对 systemone 模型不能发 max_tokens=1 的对话请求：
// 决策端点不认 messages，只会换回 404，把「模型可用」误判成「模型不存在」。
func systemOneProbeBody(model string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model,
		"state": "ping",
		"questions": map[string]any{
			"ok": map[string]any{
				"type":         "noul",
				"instructions": "Is this text present?",
			},
		},
	})
}
