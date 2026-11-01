package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"lax/internal/plugin"
	"lax/internal/store"
)

// 语音请求按方言分发。
//
// 网关不再认识任何一家上游的语音形状：模型在 model_operations 里绑定一条方言，
// 方言给出出站线协议。绑的是宿主的两条直通方言就原样转发，绑其它方言（插件方言，
// 以及把语音做成 chat 形状的 MiMo 内置方言）就由方言双向编解码。
// 请求构成与响应解码都在 internal/plugin 的方言里，网关只管地址、凭据与记账。

// speechUsesDialect 判断这一次语音请求要不要经方言编解码（而不是同协议直通）。
// 只看语音两条入站：非语音入站的插件方言走 chatDialectApplies，两条路不能互相接上——
// 语音的输入归一（文本、音频）与对话的报文补丁不是同一套。
//
// 判定不看方言的线协议是否等于入站：插件方言一律由它自己编请求、解响应，
// 即使它与入站是同一条线协议（豆包的 TTS 就是 openai-speech 入站打自己的路径）。
// 宿主直接透传的只有两条语音直通方言（lax/openai-speech@1、lax/openai-transcriptions@1），
// 它们的特征是宿主方言且线协议等于入站；MiMo 那两条也是宿主方言，但它们靠 Prepare/Decode
// 把语音做成 chat 的形状，因此仍走方言路径。
func speechUsesDialect(inbound string, d plugin.Dialect) bool {
	if d == nil || !voiceInbound(inbound) {
		return false
	}
	return !plugin.IsHostDialect(d.ID()) || d.UpstreamWire() != inbound
}

// speechRoute 按库里的方言绑定给出这条语音请求的出站协议与方言。
// 失败时第二项是给客户端的文案，第一项为空。
func (s *Server) speechRoute(a store.Account, inbound, model string) (string, plugin.Dialect, string) {
	operation := plugin.OperationForInbound(inbound)
	if operation == "" {
		return "", nil, fmt.Sprintf("协议 %s 没有对应的操作，语音请求走不通", inbound)
	}
	row, ok, err := s.store.ModelOperation(a.Provider, model, operation)
	if err != nil {
		s.logf("读厂商 %s 模型 %s 的方言绑定失败: %v", a.Provider, model, err)
		return "", nil, fmt.Sprintf("账号 %s：读模型 %s 的方言绑定失败（%v），先运行 lax doctor 检查数据库",
			a.Name, model, err)
	}
	if !ok || strings.TrimSpace(row.DialectID) == "" {
		return "", nil, fmt.Sprintf("模型 %s 的 %s 没有绑定方言：语音请求按绑定决定上游形状，"+
			"先运行 lax model capability add %s --capability %s --dialect <方言> 绑一条；现有方言：%s",
			model, operation, model, capabilityFor(inbound), strings.Join(s.currentDialects().IDs(), "、"))
	}
	registry := s.currentDialects()
	dialect, known := registry.Dialect(row.DialectID)
	if !known {
		// 不回落到内置方言：静默回落会把一次配置删除变成一次上游形状变化。
		return "", nil, fmt.Sprintf("模型 %s 绑定的方言 %s 没有注册（插件被停用或删除了？）："+
			"不会回落到别的方言，用 lax plugin ls 看插件状态，或重新绑定一条现有方言（%s）",
			model, row.DialectID, strings.Join(registry.IDs(), "、"))
	}

	wire := dialect.UpstreamWire()
	if _, msg := s.speechRouteFor(a.Provider, wire); msg != "" {
		return "", nil, msg + fmt.Sprintf("（模型 %s 绑定的方言 %s 要打上游的 %s 路由）", model, dialect.ID(), wire)
	}
	return wire, dialect, ""
}

// speechRouteFor 确认厂商声明了这条线协议的路由，返回它的路径。
// 语音与 chat 两条出站都要求路由存在：地址与凭据头都按路由走。
func (s *Server) speechRouteFor(provider, wire string) (string, string) {
	route, ok, err := s.store.LookupProviderRoute(provider, wire)
	if err != nil {
		s.logf("读厂商 %s 的 %s 路由失败: %v", provider, wire, err)
		return "", fmt.Sprintf("读厂商 %s 的 %s 路由失败（%v），先运行 lax doctor 检查数据库", provider, wire, err)
	}
	if !ok || strings.TrimSpace(route.Path) == "" {
		return "", speechNoRouteMessage(provider, wire)
	}
	return route.Path, ""
}

// capabilityFor 给错误文案里的下一步命令用：入站协议对应的能力位。
func capabilityFor(inbound string) string {
	if inbound == ProtocolTranscriptions {
		return store.CapabilityTranscription
	}
	return store.CapabilitySpeech
}

// checkOperationDialect 读这次请求的操作绑定的方言。
//
// 没有绑定是正常状态：老库与手写厂商都没有 model_operations 行，请求仍按有效协议走。
// 绑了宿主方言（lax/ 前缀）也是正常状态：宿主自己就是那条方言的执行者。
// 绑了插件方言则按注册表取出来交给调用方执行；取不到就报错，绝不回落——
// 静默回落会把「插件声明的形状」变成「实际并没生效」。
//
// 第一项为 nil 表示这次仍由宿主按协议编解码，第二项非空是拒绝文案。
func (s *Server) checkOperationDialect(provider, model, operation string) (plugin.Dialect, string) {
	if operation == "" {
		return nil, ""
	}
	row, ok, err := s.store.ModelOperation(provider, model, operation)
	if err != nil {
		return nil, fmt.Sprintf("读模型 %s 的 %s 方言绑定失败（%v），先运行 lax doctor 检查数据库", model, operation, err)
	}
	if !ok || strings.TrimSpace(row.DialectID) == "" || plugin.IsHostDialect(row.DialectID) {
		return nil, ""
	}
	registry := s.currentDialects()
	dialect, known := registry.Dialect(row.DialectID)
	if !known {
		// 不回落到宿主编解码：静默回落会把一次插件删除变成一次上游形状变化。
		return nil, fmt.Sprintf("模型 %s 的 %s 绑定的方言 %s 没有注册（插件被停用或删除了？）："+
			"不会回落到内置编解码，用 lax plugin ls 看插件状态，或重新绑定一条现有方言（%s）",
			model, operation, row.DialectID, strings.Join(registry.IDs(), "、"))
	}
	return dialect, ""
}

// speechInputOf 把入站报文归一成方言的输入。
func (s *Server) speechInputOf(inbound, model string, raw []byte, contentType string) (plugin.Input, error) {
	limits := plugin.Limits{MaxInputBytes: speechMaxBody, MaxOutputBytes: speechMaxBody}
	switch inbound {
	case ProtocolSpeech:
		sp, err := decodeSpeechRequest(raw)
		if err != nil {
			return plugin.Input{}, err
		}
		return plugin.Input{
			Operation: plugin.OpSpeechSynthesize, Model: model,
			Text: sp.Input, Voice: sp.Voice, Format: sp.Format, Instructions: sp.Instructions,
			Chars: sp.Chars, Limits: limits,
		}, nil
	case ProtocolTranscriptions:
		asr, err := decodeTranscriptionsRequest(raw, contentType)
		if err != nil {
			return plugin.Input{}, err
		}
		return plugin.Input{
			Operation: plugin.OpSpeechTranscribe, Model: model,
			Format: asr.Format, Language: asr.Language,
			Audio: asr.Audio, AudioContainer: asr.Container, Limits: limits,
		}, nil
	}
	return plugin.Input{}, fmt.Errorf("协议 %s 没有语音方言", inbound)
}

// encodeSpeechDialect 让方言编出上游请求。返回请求体与逐请求头（Plan.Headers），
// 头的叠加顺序由 buildRequest 决定。
func (s *Server) encodeSpeechDialect(d plugin.Dialect, inbound string, raw []byte,
	model, contentType string) ([]byte, map[string]string, error) {

	input, err := s.speechInputOf(inbound, model, raw, contentType)
	if err != nil {
		return nil, nil, err
	}
	plan, err := d.Prepare(input)
	if err != nil {
		return nil, nil, fmt.Errorf("方言 %s 编请求失败: %w", d.ID(), err)
	}
	// Plan.Path 不参与地址选择：地址按厂商路由走，路由在安装或 provider 配置时已经建好。
	// 让方言覆写地址等于给插件一个开打任意路径的口子，而它的收益只是省一行配置。
	// 请求体与对话方言同一套优先级：方言给了字节就用它，否则序列化 JSON。
	body := plan.Body
	if len(body) == 0 {
		body, err = json.Marshal(plan.JSON)
		if err != nil {
			return nil, nil, fmt.Errorf("方言 %s 编出的请求体不是 JSON: %w", d.ID(), err)
		}
	}
	return body, plan.Headers, nil
}

// speechDialectSuccess 处理方言路径的成功响应：媒体写字节，文本与 JSON 按结果种类写。
// 非 2xx 由调用方按既有规则处理：上游错误体是 openai 的形状，与语音入站一致。
func (s *Server) speechDialectSuccess(w http.ResponseWriter, resp *http.Response, inbound, model string,
	raw []byte, contentType string, d plugin.Dialect) attempt {

	body, err := io.ReadAll(io.LimitReader(resp.Body, speechMaxBody))
	if err != nil {
		return attempt{status: http.StatusBadGateway, transport: true,
			message: "读取上游响应失败: " + err.Error(), err: err}
	}
	input, err := s.speechInputOf(inbound, model, raw, contentType)
	if err != nil {
		// 请求已经发出去过，走到这里说明报文在两次解析之间被改过：按 502 报，不编一个空结果。
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}
	result, err := d.Decode(input, plugin.Response{
		Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body,
	})
	if err != nil {
		return attempt{status: http.StatusBadGateway, message: err.Error(), err: err}
	}

	res := attempt{
		status: http.StatusOK, wrote: true,
		inTok: result.Usage.In, outTok: result.Usage.Out,
		cacheRead: result.Usage.CacheRead, cacheWrite: result.Usage.CacheWrite,
		reasoning: result.Usage.Reasoning, total: result.Usage.Total,
		audioSeconds: result.Usage.AudioSeconds,
	}
	if input.Operation == plugin.OpSpeechSynthesize {
		// 字符数由宿主从入站数出，方言只在上游报另一套计数时才覆盖。
		res.ttsChars = input.Chars
		if result.Usage.TTSChars > 0 {
			res.ttsChars = result.Usage.TTSChars
		}
	}

	switch result.Kind {
	case plugin.ResultMedia:
		if len(result.Media) == 0 {
			return attempt{status: http.StatusBadGateway, message: "方言解出了空的媒体结果"}
		}
		w.Header().Set("Content-Type", result.MIME)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Media)
	case plugin.ResultText:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(result.Text))
	case plugin.ResultJSON:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.JSON)
	default:
		return attempt{status: http.StatusBadGateway,
			message: fmt.Sprintf("方言 %s 返回了未知的结果种类 %q", d.ID(), result.Kind)}
	}
	return res
}
