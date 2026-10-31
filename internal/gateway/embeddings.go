package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"lax/internal/store"
	"lax/internal/translate"
)

// 向量化。它没有消息序列：输入一段文本或一组文本，输出一组向量，没有角色、
// 没有工具、没有流式，因此不进中间表示，只做同协议直通。入站有两条：openai 的
// /v1/embeddings（第六条线协议 openai-embeddings）与 gemini 的 :embedContent 动作。
// 这个文件放这条路径上的纯函数：路径、最少校验、用量读取与出站地址改写，不读库、不发请求。

// embeddingsPath 是 openai 向量化的入站路径，也是入站协议 openai-embeddings 的唯一入口。
const embeddingsPath = "/v1/embeddings"

// embeddingsNoStreamMessage 是请求体带 `"stream": true` 时的拒绝文案。说清「向量化没有流式」
// 与下一步改成什么：静默当同步会让等 SSE 的客户端挂到超时，它等的是一个永不会来的事件流。
const embeddingsNoStreamMessage = "向量化没有流式：一个请求只回一个 JSON 响应，" +
	"把请求体里的 \"stream\" 去掉（或改成 false）再发一次"

// embeddingsProbeInput 是探活时发的最小输入：一句短文本，挑不出内容语义，只证明端点可用。
const embeddingsProbeInput = "lax probe"

// decodeEmbeddingsRequest 做最少校验，返回选号要用的模型名。规则只有三条：model 是非空字符串、
// input 存在、不要求流式。其余字段（encoding_format、dimensions、user 一类）不解析、不改写：
// 转发时原样带走。
func decodeEmbeddingsRequest(raw []byte) (*translate.Request, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %v", err)
	}
	if fields == nil {
		return nil, errors.New(`请求体是 null：向量化请求是 {"model":"…","input":"…"} 这样的对象`)
	}

	modelField, ok := fields["model"]
	if !ok || isJSONNull(modelField) {
		return nil, errors.New("缺少 model 字段：向量化请求按模型名选号，模型名写在请求体的 model 字段里")
	}
	var model string
	if err := json.Unmarshal(modelField, &model); err != nil {
		return nil, errors.New("model 必须是字符串：向量化请求按模型名选号，写对象或数组都选不出账号")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model 不能是空字符串：向量化请求按模型名选号，空名字匹配不上任何账号")
	}

	input, ok := fields["input"]
	if !ok || isJSONNull(input) {
		return nil, errors.New("缺少 input 字段：要向量化的内容写在 input 里，" +
			"可以是一个字符串，也可以是字符串（或 token）数组")
	}

	var stream bool
	if err := json.Unmarshal(fields["stream"], &stream); err == nil && stream {
		return nil, errors.New(embeddingsNoStreamMessage)
	}

	return &translate.Request{Model: model}, nil
}

// decodeGeminiEmbedRequest 是 gemini 的 :embedContent 入站的最少校验：请求体必须是 JSON 对象。
// 模型名在路径上，由入站路径注入；`content`、`taskType`、`outputDimensionality` 原样转发。
// 与对话的 generateContent 不共用解码器：那一个读的是 contents，向量化读的是 content，
// 拿对话的规则去解只会把一条合法请求判成 400。
func decodeGeminiEmbedRequest(raw []byte) (*translate.Request, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %v", err)
	}
	if fields == nil {
		return nil, errors.New(`请求体是 null：向量化请求是 {"content":{"parts":[{"text":"…"}]}} 这样的对象`)
	}
	return &translate.Request{}, nil
}

// embeddingsUsage 读 OpenAI 向量化响应的 usage.prompt_tokens。向量是输出，但上游不按它计费、
// 也不上报条数，所以输出与思考 token 恒为 0：按上报的数字记账，不按向量条数估一个值。没带 usage、或读不出来时记 0：
// 精确到 0 的用量比编一个估算值好，`usage` 的花费估算也不会因为编出来的数字失真。
func embeddingsUsage(body []byte) (in int64) {
	var payload struct {
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	if payload.Usage.PromptTokens > 0 {
		return payload.Usage.PromptTokens
	}
	return payload.Usage.TotalTokens
}

// geminiEmbedEndpoint 把 gemini 的出站地址改到 :embedContent。厂商路由里写的是对话动作
// （`:generateContent` 或 `:streamGenerateContent`），向量化要换掉动作后缀；
// 查询串原样保留（路由带 `alt=sse` 时也不动它，向量化本来就不流式）。
// 路径末尾没有这两个动作后缀（自定义路由）时原样返回：猜一个后缀等于去打一条没声明过的路径。
func geminiEmbedEndpoint(endpoint string) string {
	pathPart, query := endpoint, ""
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		pathPart, query = endpoint[:i], endpoint[i:]
	}
	for _, action := range []translate.GeminiAction{translate.GeminiActionStream, translate.GeminiActionGenerate} {
		suffix := translate.GeminiActionSuffix(action)
		if strings.HasSuffix(pathPart, suffix) {
			pathPart = strings.TrimSuffix(pathPart, suffix) +
				translate.GeminiActionSuffix(translate.GeminiActionEmbed)
			break
		}
	}
	return pathPart + query
}

// embeddingOutbound 给向量化请求选协议：入站是 openai-embeddings 时出站也只能是它，
// 入站是 gemini 的 :embedContent 时出站也只能是 gemini。协议在这里是路由事实：厂商声明了这条
// 路由才转发，不按模型的有效协议猜——猜出来的地址上游根本不提供。返回空串时第二个值是给客户端的文案。
func (s *Server) embeddingOutbound(a store.Account, inbound string) (string, string) {
	route, ok, err := s.store.LookupProviderRoute(a.Provider, inbound)
	if err != nil {
		s.logf("读厂商 %s 的 %s 路由失败: %v", a.Provider, inbound, err)
		return "", fmt.Sprintf("账号 %s：读厂商 %s 的 %s 路由失败（%v），先运行 lax doctor 检查数据库",
			a.Name, a.Provider, inbound, err)
	}
	if !ok || strings.TrimSpace(route.Path) == "" {
		return "", embeddingNoRouteMessage(a.Provider, inbound)
	}
	return inbound, ""
}

// embeddingsProbeBody 是探活用的最小向量化请求体：一句短文本，不关心向量结果，只证明端点可用。
func embeddingsProbeBody(model string) ([]byte, error) {
	return json.Marshal(map[string]any{"model": model, "input": embeddingsProbeInput})
}

// geminiEmbedProbeBody 是 gemini 向量化探活的请求体：模型名在路径上，请求体只有 content
// （与 OpenAI 的 input 不同名字，两条路径的请求体长得不一样，不能共用一个）。
func geminiEmbedProbeBody() ([]byte, error) {
	return json.Marshal(map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{"text": embeddingsProbeInput}}},
	})
}

// geminiEmbeddingOnly 判断这个 gemini 模型是不是「只能向量化」：目录里它的能力位有 embedding、
// 没有 chat。读不到目录、或上游没声明方法列表时返回 false：
// 没有依据就不改探活姿势，仍按对话打：反方向猜错会让一个能对话的模型探出 404。
func (s *Server) geminiEmbeddingOnly(a store.Account, model string) bool {
	catalog, ok, err := s.store.ProviderModel(a.Provider, model)
	if err != nil || !ok {
		return false
	}
	var embed, chat bool
	for _, capability := range catalog.Capabilities {
		switch capability {
		case capabilityEmbedding:
			embed = true
		case capabilityChat:
			chat = true
		}
	}
	return embed && !chat
}

// embeddingNoRouteMessage 说明这家厂商没有声明向量化要用的那条路由，并给出下一步命令。
// 协议在这里是路由事实而不是模型事实：端点在不在由厂商决定，不按模型的
// account_models.protocol 猜，猜出来的地址上游根本不提供。
func embeddingNoRouteMessage(provider, protocol string) string {
	return fmt.Sprintf("厂商 %s 没有声明 %s 路由，向量化请求发不出去：用 lax provider set %s %s=%s 声明这条路由，"+
		"或让这座网关连一家声明过它的上游",
		provider, protocol, provider, protocol, embeddingRouteHint(protocol))
}

// embeddingRouteHint 是两条向量化路由的示例路径，只用于错误文案里的下一步命令。
func embeddingRouteHint(protocol string) string {
	if protocol == ProtocolGemini {
		return "/v1beta/models/{model}:generateContent"
	}
	return "/v1/embeddings"
}

// embeddingsInteropMessage 说明对话模型与向量化请求不相通：向量化请求没有消息序列可以转换，
// 反过来一条 chat 入站也发不出向量化模型。
func embeddingsInteropMessage(inbound, outbound, model string) string {
	if outbound == ProtocolEmbeddings {
		return fmt.Sprintf("模型 %s 的有效出站协议是 %s（向量化），对话请求发不了它："+
			"向量化请求用 POST %s 发，或换一个对话模型",
			model, ProtocolEmbeddings, embeddingsPath)
	}
	return fmt.Sprintf("入站协议 %s 的向量化请求只与向量化协议互通，模型 %s 的有效出站协议是 %s："+
		"换成声明了向量化路由的厂商，或用那条协议自己的对话路径发",
		inbound, model, outbound)
}
