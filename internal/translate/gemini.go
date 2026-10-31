package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// 本文件负责 gemini（Generative Language API 的 generateContent）与中间表示之间的编解码，
// 入站与出站、非流式与流式都在这里。模型名在 URL 路径里，不在请求体里，所以编解码器都不写
// model 字段：入站时模型名与流式标志由路径注入，出站时模型名由网关填进路由的 {model} 占位符。
// 表里没有的字段一律丢弃，不原样透传：能带到另一条协议上的东西必须先落成 IR。

// gemini 的 toolConfig 模式。gemini 只有这三种，没有「必须调用某个具体函数」这一档，
// 指定函数时用 ANY 加白名单：没有「必须调用某个具体函数」这一档，ANY 加一个名字最接近，
// 降成 AUTO 就等于把强制调用变成可选。
const (
	geminiModeAuto = "AUTO"
	geminiModeAny  = "ANY"
	geminiModeNone = "NONE"
)

// gemini 入站路径的形状：模型名在路径里，非流式与流式由路径后缀区分，
// 后缀本身不算模型名的一部分。
const (
	// GeminiInboundPathPrefix 是 gemini 入站路径的前缀，网关按它注册路由。
	GeminiInboundPathPrefix = "/v1beta/models/"
	geminiGenerateAction    = ":generateContent"
	geminiStreamAction      = ":streamGenerateContent"
	geminiEmbedAction       = ":embedContent"
)

// GeminiAction 是 gemini 入站路径末尾的接口动作。前两个是对话，
// embedContent 是向量化：它与对话不互通，也不经中间表示。
// 动作名就是上游路径里的后缀，拼写与上游一致。
type GeminiAction string

const (
	GeminiActionGenerate GeminiAction = "generateContent"
	GeminiActionStream   GeminiAction = "streamGenerateContent"
	GeminiActionEmbed    GeminiAction = "embedContent"
)

// GeminiActionSuffix 把动作折回路径后缀，供拼接出站地址时替换用。
func GeminiActionSuffix(action GeminiAction) string { return ":" + string(action) }

// GeminiPathTarget 从 gemini 的入站路径里取出模型名与是否流式。
// 模型名是前缀与动作后缀之间的那一段，按 url.PathUnescape 还原（`org%2Fgemini` 落成
// `org/gemini`，不会被拆成两段路径）；后缀不是生成类动作（generateContent 与
// streamGenerateContent）、或模型名为空时返回 ok=false。向量化动作不算在这里：
// 它自己去 GeminiPathAction 取，取到之后就不是一条对话请求了。
// 转义串本身不合法时按原文返回：这样的名字匹配不上任何账号，404 比 400 更贴近事实。
func GeminiPathTarget(path string) (model string, stream, ok bool) {
	model, action, ok := GeminiPathAction(path)
	if !ok || action == GeminiActionEmbed {
		return "", false, false
	}
	return model, action == GeminiActionStream, true
}

// GeminiPathAction 与 GeminiPathTarget 走同一套前缀与后缀判定，但把三种动作都认全：
// generateContent 是同步对话、streamGenerateContent 是流式对话、embedContent 是向量化。
// 调用方拿到的 action 决定这条请求按哪条协议处理：向量化报文与对话报文完全不同，
// 认错成对话会在解码时失败；两条对话动作再按是否流式分成两种响应处理。
func GeminiPathAction(path string) (model string, action GeminiAction, ok bool) {
	rest, found := strings.CutPrefix(path, GeminiInboundPathPrefix)
	if !found {
		return "", "", false
	}
	switch {
	case strings.HasSuffix(rest, geminiStreamAction):
		rest = strings.TrimSuffix(rest, geminiStreamAction)
		action = GeminiActionStream
	case strings.HasSuffix(rest, geminiGenerateAction):
		rest = strings.TrimSuffix(rest, geminiGenerateAction)
		action = GeminiActionGenerate
	case strings.HasSuffix(rest, geminiEmbedAction):
		rest = strings.TrimSuffix(rest, geminiEmbedAction)
		action = GeminiActionEmbed
	default:
		return "", "", false
	}
	if unescaped, err := url.PathUnescape(rest); err == nil {
		rest = unescaped
	}
	if strings.TrimSpace(rest) == "" {
		return "", "", false
	}
	return rest, action, true
}

// GeminiRequest 是 POST {base}/v1beta/models/{model}:generateContent 的请求体。
// 模型名不在请求体里：它在路径上，由网关按路由的 {model} 占位符填进去。
type GeminiRequest struct {
	SystemInstruction *GeminiContent    `json:"systemInstruction,omitempty"`
	Contents          []GeminiContent   `json:"contents"`
	Tools             []GeminiTool      `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGeneration `json:"generationConfig,omitempty"`
}

// GeminiContent 是一条对话内容，role 只有 user 与 model 两种。
type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts,omitempty"`
}

// GeminiPart 是内容里的一块。五种形状互斥，按需填一个。
type GeminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *GeminiBlob             `json:"inlineData,omitempty"`
	FileData         *GeminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

// GeminiBlob 是内联的二进制内容（图片）。
type GeminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// GeminiFileData 是对上游文件的引用。只有 Files API 给出的 URI 才一定可用：
// 自带 Studio 上传页的地址无法从网关这一侧复现，写了只能换回 400。
type GeminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

// GeminiFunctionCall 是一次工具调用。gemini 不给调用 id，只给函数名与参数。
type GeminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// GeminiFunctionResponse 是一条工具结果。gemini 按函数名对回调用，不认 id。
type GeminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

// GeminiTool 是一组函数定义。gemini 把一组声明放在同一个 tools 元素里。
type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations"`
}

// GeminiFunctionDeclaration 是一个函数定义。
type GeminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// GeminiToolConfig 是工具选择。
type GeminiToolConfig struct {
	FunctionCallingConfig *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// GeminiFunctionCallingConfig 是工具选择的取值。
type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// GeminiGeneration 是采样参数。ThinkingConfig 是请求侧的思考配置参数。
type GeminiGeneration struct {
	MaxOutputTokens int                   `json:"maxOutputTokens,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	StopSequences   []string              `json:"stopSequences,omitempty"`
	ThinkingConfig  *GeminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// GeminiThinkingConfig 是 thinkingConfig。thinkingBudget 用指针：0 是「不思考」这一档的
// 写法，必须能写出去：nil 与 0 在报文里是两件事，用值类型会把「不思考」静默成「不写」。
type GeminiThinkingConfig struct {
	ThinkingBudget *int `json:"thinkingBudget,omitempty"`
}

// GeminiResponse 是 generateContent 的非流式响应。
type GeminiResponse struct {
	ResponseID     string                `json:"responseId,omitempty"`
	ModelVersion   string                `json:"modelVersion,omitempty"`
	Candidates     []GeminiCandidate     `json:"candidates"`
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *GeminiUsageMetadata  `json:"usageMetadata,omitempty"`
}

// GeminiCandidate 是响应里的一个候选。
type GeminiCandidate struct {
	Content      GeminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index"`
}

// GeminiPromptFeedback 是提示词自身被拦时的说明。被拦的请求没有候选。
type GeminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// GeminiUsageMetadata 是 gemini 的用量。cachedContentTokenCount 是 promptTokenCount 的子集，
// 与 chat 的 prompt_tokens_details.cached_tokens 同一套记法：两者都是「已含在输入里」的那部分。
// thoughtsTokenCount 是思考 token：真实 gemini 的 totalTokenCount 已经把它算进去，
// 是否另计一次由入账时按 total 判定（判定只写在 store.ReasoningExtra 一处）。
type GeminiUsageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount,omitempty"`
}

// EncodeGeminiRequest 把 IR 编成 generateContent 的请求体。
//
// model 不写进报文（gemini 的模型名在路径里），参数留着是为了与另外三个编码器同一形状：
// 网关不必为 gemini 分情况。
func EncodeGeminiRequest(r *Request, model string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("请求为空")
	}
	// 流式与非流式的请求体同一形状：gemini 的流式由路径后缀（:streamGenerateContent）与
	// alt=sse 表示，报文里没有 stream 字段，所以 r.Stream 不在这里落笔。
	_ = model

	out := GeminiRequest{}
	system := joinText(r.System)

	// gemini 的 functionResponse 只按函数名对回调用，而中间表示的工具结果只有 tool_use_id。
	// 先按历史里的 tool_use 块建一张 id → 函数名的表，查不到名字的结果整块丢弃：
	// 把 id 当成函数名写出去，上游会认为它对应一个没声明过的函数并拒掉整条请求。
	names := toolNameByUseID(r.Messages)

	for i, m := range r.Messages {
		if m.Role == RoleSystem {
			// 中间表示里出现 system 消息时并进 systemInstruction：gemini 只有这一个位置。
			if text := joinText(m.Blocks); text != "" {
				if system != "" {
					system += "\n"
				}
				system += text
			}
			continue
		}
		role, err := geminiRole(m.Role)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条消息: %w", i+1, err)
		}
		parts, err := geminiParts(m.Blocks, names)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条消息: %w", i+1, err)
		}
		appendGeminiContent(&out.Contents, role, parts)
	}

	// 消息全被丢弃时宁可自己报错：上游的 contents 至少要一条，写出空数组只会换回一个
	// 指不到原因上的 400（与 anthropic 编码器同一原则）。
	if len(out.Contents) == 0 {
		return nil, errors.New("请求里没有可发给 gemini 的消息：每条消息的内容块都被丢弃了")
	}
	if system != "" {
		out.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: system}}}
	}

	var decls []GeminiFunctionDeclaration
	for _, t := range r.Tools {
		// 服务端内置工具（anthropic 的 web_search 一类）在 gemini 里没有对应形状，整条丢弃。
		if !isClientTool(t) {
			continue
		}
		decls = append(decls, GeminiFunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schemaOrEmpty(t.Schema),
		})
	}
	if len(decls) > 0 {
		out.Tools = []GeminiTool{{FunctionDeclarations: decls}}
	}

	if r.ToolChoice != nil {
		cfg, err := geminiToolChoiceToWire(r.ToolChoice)
		if err != nil {
			return nil, err
		}
		// cfg 为空表示这一档在 gemini 里编不出来（空白名单），不写 toolConfig：写一个空对象
		// 与不写不是一回事。
		if cfg != nil {
			out.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: cfg}
		}
	}

	if gen := geminiGeneration(r); gen != nil {
		out.GenerationConfig = gen
	}

	return json.Marshal(out)
}

// DecodeGeminiRequest 把 generateContent 的请求体解析成 IR。报文里没有模型名（它在 URL
// 路径上）也没有 stream 字段（流式由路径后缀表示），两者都由网关按路径注入，这里留空。
func DecodeGeminiRequest(raw []byte) (*Request, error) {
	var wire GeminiRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("gemini 请求体不是合法 JSON: %w", err)
	}

	out := &Request{}
	if wire.SystemInstruction != nil {
		out.System = geminiTextBlocks(wire.SystemInstruction.Parts)
	}

	// 合成 id 按整段对话连续编号，函数名 → 待对回的 id 记一份：gemini 的 functionResponse 只有
	// 函数名，编给 anthropic 与 chat 时那一头的 tool_result / tool 消息必须引用同一轮 tool_use 的
	// id，否则上游会以「对不上任何调用」拒掉整条请求。
	toolIndex := 0
	callIDs := map[string][]string{}
	for i, c := range wire.Contents {
		msg, err := geminiMessage(c, &toolIndex, &callIDs)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条内容: %w", i+1, err)
		}
		if len(msg.Blocks) > 0 {
			out.Messages = append(out.Messages, msg)
		}
	}

	for _, t := range wire.Tools {
		for _, d := range t.FunctionDeclarations {
			out.Tools = append(out.Tools, Tool{
				Name:        d.Name,
				Description: d.Description,
				Schema:      schemaOrEmpty(d.Parameters),
			})
		}
	}

	if cfg := geminiCallingConfig(wire.ToolConfig); cfg != nil {
		out.ToolChoice = geminiToolChoiceFromWire(cfg)
	}

	if gc := wire.GenerationConfig; gc != nil {
		out.MaxTokens = gc.MaxOutputTokens
		out.Temperature = gc.Temperature
		out.TopP = gc.TopP
		out.Stop = gc.StopSequences
		// thinkingBudget 是请求侧的思考配置参数：0 视为关闭，其余（包括负数的「动态思考」）
		// 都算开着，预算原样保留：负数是上游的「动态思考」，当成关闭会把它变成不思考。
		if tc := gc.ThinkingConfig; tc != nil && tc.ThinkingBudget != nil {
			budget := *tc.ThinkingBudget
			out.Thinking = &ThinkingConfig{Enabled: budget != 0, Budget: budget}
		}
	}
	return out, nil
}

// DecodeGeminiResponse 把 generateContent 的非流式响应解析成 IR。
// requestedModel 会写回给客户端：gemini 的模型名在路径里，响应里的 modelVersion 可能带版本后缀。
func DecodeGeminiResponse(raw []byte, requestedModel string) (*Response, error) {
	var wire GeminiResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("gemini 响应体不是合法 JSON: %w", err)
	}
	if len(wire.Candidates) == 0 {
		// 提示词自己被拦时没有候选，只有 promptFeedback：这是上游给出的结论，
		// 与「响应形状不对」要分开说，否则用户会去查自己的请求格式。
		if wire.PromptFeedback != nil && wire.PromptFeedback.BlockReason != "" {
			return nil, fmt.Errorf("上游拒绝了这次请求（promptFeedback.blockReason=%s），没有候选",
				wire.PromptFeedback.BlockReason)
		}
		return nil, errors.New("上游响应没有 candidates")
	}
	cand := wire.Candidates[0]

	out := &Response{
		ID:     wire.ResponseID,
		Model:  requestedModel,
		Blocks: []Block{},
	}
	if out.Model == "" {
		out.Model = wire.ModelVersion
	}

	toolIndex := 0
	for _, part := range cand.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			// gemini 不给调用 id，但 anthropic 的 tool_use 必须有 id，chat 的 tool_calls 也要。
			// 按出现顺序补一个固定的合成 id，编回 gemini 时它本来就会被丢掉。
			toolIndex++
			out.Blocks = append(out.Blocks, Block{Kind: BlockToolUse, ToolUse: &ToolUse{
				ID:    geminiToolUseID(toolIndex),
				Name:  part.FunctionCall.Name,
				Input: geminiArgumentsJSON(part.FunctionCall.Args),
			}})
		case part.InlineData != nil:
			if url := geminiDataURI(part.InlineData); url != "" {
				out.Blocks = append(out.Blocks, Block{Kind: BlockImage, Image: &Image{URL: url}})
			}
		case part.FileData != nil:
			if part.FileData.FileURI != "" {
				out.Blocks = append(out.Blocks, Block{Kind: BlockImage, Image: &Image{URL: part.FileData.FileURI}})
			}
		case part.Text != "":
			out.Blocks = append(out.Blocks, Block{Kind: BlockText, Text: part.Text})
		}
	}

	out.StopReason = geminiStopToIR(cand.FinishReason, hasToolUse(out.Blocks))
	out.StopNote = geminiStopNote(cand.FinishReason)
	out.Usage = geminiUsageToIR(wire.UsageMetadata)
	return out, nil
}

// EncodeGeminiResponse 把 IR 编成 generateContent 的响应体，用于「客户端说 gemini」这一条入站。
// 只搬映射表里有的几类块：思考内容与工具结果在 gemini 的响应里没有落点，丢弃
// （工具结果只出现在请求的 functionResponse 里）。
func EncodeGeminiResponse(r *Response) ([]byte, error) {
	if r == nil {
		return nil, errors.New("响应为空")
	}
	wire := GeminiResponse{
		ResponseID:   r.ID,
		ModelVersion: r.Model,
		Candidates: []GeminiCandidate{{
			Content:      GeminiContent{Role: "model", Parts: geminiResponseParts(r.Blocks)},
			FinishReason: geminiFinishFromIR(r.StopReason),
		}},
		UsageMetadata: geminiUsageFromIR(r.Usage),
	}
	return json.Marshal(wire)
}

// geminiResponseParts 把中间表示的内容块翻成 gemini 响应的 parts。
func geminiResponseParts(blocks []Block) []GeminiPart {
	parts := make([]GeminiPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case BlockText:
			if b.Text == "" {
				continue
			}
			parts = append(parts, GeminiPart{Text: b.Text})

		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			parts = append(parts, GeminiPart{FunctionCall: &GeminiFunctionCall{
				Name: b.ToolUse.Name,
				Args: geminiObjectOrNil(b.ToolUse.Input),
			}})

		case BlockImage:
			if b.Image == nil {
				continue
			}
			if part, ok := geminiImagePart(b.Image.URL); ok {
				parts = append(parts, part)
			}
		}
	}
	return parts
}

// geminiUsageFromIR 把中间表示的用量翻成 usageMetadata。totalTokenCount 是两项相加：
// gemini 没有缓存写入字段，CacheWrite 无处可放，丢弃。
func geminiUsageFromIR(u Usage) *GeminiUsageMetadata {
	return &GeminiUsageMetadata{
		PromptTokenCount:        u.In,
		CandidatesTokenCount:    u.Out,
		TotalTokenCount:         u.In + u.Out,
		CachedContentTokenCount: u.CacheRead,
	}
}

// geminiStreamDecoder 把 gemini 的流式帧（只有 `data: {...}`，没有事件名）翻成增量。
// 一帧是一个 GenerateContentResponse：文本与 functionCall 按 parts 逐帧给出，finishReason
// 与 usageMetadata 在末尾。gemini 的 functionCall 不是增量的（参数一次给全），所以每个
// functionCall 在同一帧里产出 tool_start 与一整段 tool_args。
type geminiStreamDecoder struct {
	// toolIndex 是合成调用 id 的序号，与 DecodeGeminiResponse 同一个写法。
	toolIndex  int
	sawTool    bool
	stopReason string
	stopped    bool
	closed     bool
	usage      *Usage
}

func (d *geminiStreamDecoder) Decode(data []byte) ([]Delta, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}

	var frame struct {
		Candidates     []GeminiCandidate     `json:"candidates"`
		PromptFeedback *GeminiPromptFeedback `json:"promptFeedback"`
		UsageMetadata  *GeminiUsageMetadata  `json:"usageMetadata"`
		Error          *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, fmt.Errorf("上游流式帧不是合法 JSON: %w", err)
	}
	if frame.Error != nil {
		msg := frame.Error.Message
		if msg == "" {
			msg = "上游返回错误"
		}
		return []Delta{{Kind: DeltaError, Text: msg}}, nil
	}

	if frame.UsageMetadata != nil {
		u := geminiUsageToIR(frame.UsageMetadata)
		d.usage = &u
	}

	if len(frame.Candidates) == 0 {
		// 提示词自己被拦：上游给出了结论，当成错误增量，与 DecodeGeminiResponse 同一条规则。
		if frame.PromptFeedback != nil && frame.PromptFeedback.BlockReason != "" {
			return []Delta{{Kind: DeltaError, Text: fmt.Sprintf(
				"上游拒绝了这次请求（promptFeedback.blockReason=%s），没有候选",
				frame.PromptFeedback.BlockReason)}}, nil
		}
		return nil, nil
	}

	cand := frame.Candidates[0]
	var out []Delta
	for _, part := range cand.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			d.toolIndex++
			d.sawTool = true
			index := d.toolIndex
			out = append(out, Delta{
				Kind:     DeltaToolStart,
				Index:    index,
				ToolID:   geminiToolUseID(index),
				ToolName: part.FunctionCall.Name,
			})
			// 参数一次给全：非对象的参数照样省略成空对象，与请求方向同一个处理。
			out = append(out, Delta{
				Kind:     DeltaToolArgs,
				Index:    index,
				ArgsJSON: string(geminiArgumentsJSON(part.FunctionCall.Args)),
			})
		case part.Text != "":
			out = append(out, Delta{Kind: DeltaText, Text: part.Text})
		}
		// inlineData / fileData 与 thought 标记不搬：流式增量只有文本、思考、工具三样，
		// 而思考内容在 gemini 这一档没有落点。
	}

	if cand.FinishReason != "" && !d.stopped {
		d.stopped = true
		d.stopReason = geminiStopToIR(cand.FinishReason, d.sawTool)
		out = append(out, Delta{
			Kind:       DeltaStop,
			StopReason: d.stopReason,
			StopNote:   geminiStopNote(cand.FinishReason),
			Usage:      d.takeUsage(),
		})
	}
	return out, nil
}

// Close 在上游结束时补齐收尾。finishReason 先到、usageMetadata 后到时补一个只带 Usage 的
// DeltaStop，与 chat 解码器同一个写法。
func (d *geminiStreamDecoder) Close() []Delta {
	if d.closed {
		return nil
	}
	d.closed = true
	if !d.stopped {
		d.stopped = true
		reason := d.stopReason
		if reason == "" {
			reason = StopEndTurn
		}
		return []Delta{{Kind: DeltaStop, StopReason: reason, Usage: d.takeUsage()}}
	}
	if u := d.takeUsage(); u != nil {
		return []Delta{{Kind: DeltaStop, Usage: u}}
	}
	return nil
}

// takeUsage 取走待附加的用量，避免重复上报。
func (d *geminiStreamDecoder) takeUsage() *Usage {
	u := d.usage
	d.usage = nil
	return u
}

// geminiStreamEncoder 把增量编成下发给 gemini 客户端的 SSE 帧。gemini 的流式没有事件名，
// 一帧是一个 GenerateContentResponse；收尾没有单独的结束事件，最后一帧的 finishReason 就是。
//
// 工具调用要攒：gemini 的 functionCall 一次给整个参数对象，而增量体系里的 tool_args 是片段。
// 攒到收尾（DeltaStop 或 Finish）才一次发出；断在中间的参数整块丢弃（见 toolEvents）。
//
// 带 finishReason 的收尾帧留到 Finish 才发：用量可能在 stop 增量之后才到（chat 上游把用量单发
// 一帧），早发一帧结束帧就带不上它，客户端会看到两次结束或者丢掉用量。
type geminiStreamEncoder struct {
	model   string
	started bool
	// toolOrder 是上游工具块号的出现顺序，决定同一帧里 parts 的顺序。
	toolOrder  []int
	toolNames  map[int]string
	toolArgs   map[int]*strings.Builder
	toolOpen   bool
	stopReason string
	usage      *Usage
	finished   bool
}

func newGeminiStreamEncoder(model string) *geminiStreamEncoder {
	return &geminiStreamEncoder{
		model:     model,
		toolNames: map[int]string{},
		toolArgs:  map[int]*strings.Builder{},
	}
}

// Start 发首帧：gemini 没有「消息开始」事件，先给一个带角色、不带内容的候选，
// 客户端因此立刻拿到一帧，而不是等到上游第一个 token。
func (e *geminiStreamEncoder) Start() []SSEEvent {
	if e.started {
		return nil
	}
	e.started = true
	return []SSEEvent{e.frame(&GeminiResponse{
		ModelVersion: e.model,
		Candidates: []GeminiCandidate{{
			Content: GeminiContent{Role: "model"},
		}},
	})}
}

func (e *geminiStreamEncoder) Emit(d Delta) []SSEEvent {
	switch d.Kind {
	case DeltaThinking:
		// 思考内容在 gemini 的报文里没有落点，签名更无处可放。
		return nil

	case DeltaText:
		if d.Text == "" {
			return nil
		}
		return []SSEEvent{e.partsEvent([]GeminiPart{{Text: d.Text}})}

	case DeltaToolStart:
		e.noteTool(d.Index, d.ToolName)
		e.toolOpen = true
		return nil

	case DeltaToolArgs:
		// 没收到 start 就来了参数时补一条，避免丢内容。
		if _, ok := e.toolNames[d.Index]; !ok {
			e.noteTool(d.Index, d.ToolName)
		}
		e.toolOpen = true
		if b, ok := e.toolArgs[d.Index]; ok {
			b.WriteString(d.ArgsJSON)
		} else {
			buf := &strings.Builder{}
			buf.WriteString(d.ArgsJSON)
			e.toolArgs[d.Index] = buf
		}
		return nil

	case DeltaStop:
		e.toolOpen = false
		if d.Usage != nil {
			e.usage = d.Usage
		}
		if d.StopReason != "" {
			e.stopReason = d.StopReason
		}
		// 只把攒下的工具发出去，结束帧留给 Finish：顺序反了会让客户端先收到结束再收到工具调用，
		// 而结束帧之后的内容它不会再读。
		return e.toolEvents()

	case DeltaError:
		return []SSEEvent{e.errorEvent(withIncompleteToolArgs(d.Text, e.toolOpen))}
	}
	return nil
}

// Finish 收尾：发攒下的工具帧与带 finishReason 的那一帧；已经发过就不再发第二次。
func (e *geminiStreamEncoder) Finish() []SSEEvent {
	if !e.started || e.finished {
		return nil
	}
	events := e.toolEvents()
	return append(events, e.finishEvent())
}

// noteTool 记下一个工具块的名字。
func (e *geminiStreamEncoder) noteTool(index int, name string) {
	if _, ok := e.toolNames[index]; !ok {
		e.toolOrder = append(e.toolOrder, index)
		e.toolNames[index] = name
		return
	}
	if name != "" {
		e.toolNames[index] = name
	}
}

// toolEvents 把攒下来的工具调用编成一帧。
//
// 参数分三种：空表示这个函数本来就没有参数，照发不带 args 的 functionCall；合法 JSON 对象原样
// 写进 args；既非空又不是对象的（上游断在参数中间）整块丢弃——gemini 的 args 是 protobuf
// Struct，写不了半截 JSON，发一个不带参数的 functionCall 只会让客户端拿空参数调一次函数，
// 比丢这一次调用更糟。断流的错误帧已经说明了参数不完整。
func (e *geminiStreamEncoder) toolEvents() []SSEEvent {
	if len(e.toolOrder) == 0 {
		return nil
	}
	parts := make([]GeminiPart, 0, len(e.toolOrder))
	for _, index := range e.toolOrder {
		name := e.toolNames[index]
		if name == "" {
			continue
		}
		var raw json.RawMessage
		if b, ok := e.toolArgs[index]; ok {
			raw = json.RawMessage(b.String())
		}
		args := geminiObjectOrNil(raw)
		if args == nil && len(bytes.TrimSpace(raw)) > 0 {
			continue
		}
		parts = append(parts, GeminiPart{FunctionCall: &GeminiFunctionCall{Name: name, Args: args}})
	}
	e.toolOrder = nil
	e.toolNames = map[int]string{}
	e.toolArgs = map[int]*strings.Builder{}
	if len(parts) == 0 {
		return nil
	}
	return []SSEEvent{e.partsEvent(parts)}
}

// finishEvent 是最后一帧：finishReason 与 usageMetadata 一起发，客户端据此结束流。
func (e *geminiStreamEncoder) finishEvent() SSEEvent {
	e.finished = true
	reason := e.stopReason
	if reason == "" {
		reason = StopEndTurn
	}
	wire := GeminiResponse{
		ModelVersion: e.model,
		Candidates: []GeminiCandidate{{
			Content:      GeminiContent{Role: "model"},
			FinishReason: geminiFinishFromIR(reason),
		}},
	}
	if e.usage != nil {
		wire.UsageMetadata = geminiUsageFromIR(*e.usage)
	}
	return e.frame(&wire)
}

func (e *geminiStreamEncoder) partsEvent(parts []GeminiPart) SSEEvent {
	return e.frame(&GeminiResponse{
		ModelVersion: e.model,
		Candidates: []GeminiCandidate{{
			Content: GeminiContent{Role: "model", Parts: parts},
		}},
	})
}

// errorEvent 是下发给客户端的错误帧：gemini 的错误体是 {"error":{"code","message","status"}}，
// 流里的错误也照这个形状写，客户端不用为流式另写一套解析。
func (e *geminiStreamEncoder) errorEvent(message string) SSEEvent {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    500,
			"message": message,
			"status":  "INTERNAL",
		},
	})
	if err != nil {
		body = []byte(`{"error":{"code":500,"message":"内部错误"}}`)
	}
	return SSEEvent{Data: body}
}

// frame 把一帧序列化成 SSE：gemini 的流没有事件名，只有 data 行。
func (e *geminiStreamEncoder) frame(v any) SSEEvent {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{}`)
	}
	return SSEEvent{Data: b}
}

// geminiUsageScanner 从响应体里读 gemini 的用量，用于同协议直通、流式直通与探活记账。
// 非流式的响应体是整条 JSON，流式的每帧也是 JSON，两者都只有 usageMetadata 有用。
type geminiUsageScanner struct {
	in, out, cacheRead int64
	reasoning, total   int64
}

// Feed 送入一段响应体，只认 usageMetadata，其余字段忽略。
func (s *geminiUsageScanner) Feed(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var env struct {
		UsageMetadata *GeminiUsageMetadata `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.UsageMetadata == nil {
		return
	}
	if env.UsageMetadata.PromptTokenCount > 0 {
		s.in = env.UsageMetadata.PromptTokenCount
	}
	if env.UsageMetadata.CandidatesTokenCount > 0 {
		s.out = env.UsageMetadata.CandidatesTokenCount
	}
	if env.UsageMetadata.CachedContentTokenCount > 0 {
		s.cacheRead = env.UsageMetadata.CachedContentTokenCount
	}
	if env.UsageMetadata.TotalTokenCount > 0 {
		s.total = env.UsageMetadata.TotalTokenCount
	}
	if env.UsageMetadata.ThoughtsTokenCount > 0 {
		s.reasoning = env.UsageMetadata.ThoughtsTokenCount
	}
}

// Usage 返回读到的用量。gemini 没有缓存写入字段，CacheWrite 恒为 0。
func (s *geminiUsageScanner) Usage() Usage {
	return Usage{In: s.in, Out: s.out, CacheRead: s.cacheRead, Reasoning: s.reasoning, Total: s.total}
}

// appendGeminiContent 追加一条内容：相邻同角色的并成一条，空 parts 不写出去。
func appendGeminiContent(contents *[]GeminiContent, role string, parts []GeminiPart) {
	if len(parts) == 0 {
		return
	}
	if n := len(*contents); n > 0 && (*contents)[n-1].Role == role {
		(*contents)[n-1].Parts = append((*contents)[n-1].Parts, parts...)
		return
	}
	*contents = append(*contents, GeminiContent{Role: role, Parts: parts})
}

// geminiRole 把中间表示的角色翻成 gemini 的两种角色。工具结果走 user：gemini 没有 tool 角色，
// functionResponse 是 user 轮次里的一块。
func geminiRole(role Role) (string, error) {
	switch role {
	case RoleUser, RoleTool:
		return "user", nil
	case RoleAssistant:
		return "model", nil
	default:
		return "", fmt.Errorf("不支持的消息角色 %q", role)
	}
}

// geminiParts 把一条消息的内容块翻成 gemini 的 parts。names 是 tool_use_id → 函数名。
func geminiParts(blocks []Block, names map[string]string) ([]GeminiPart, error) {
	parts := make([]GeminiPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case BlockText:
			if b.Text == "" {
				continue
			}
			parts = append(parts, GeminiPart{Text: b.Text})

		case BlockThinking:
			// 思考内容只在响应方向搬运，请求方向整块丢弃（gemini 的请求里也没有放思考内容的位置）。
			continue

		case BlockImage:
			if b.Image == nil {
				continue
			}
			if part, ok := geminiImagePart(b.Image.URL); ok {
				parts = append(parts, part)
			}

		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			parts = append(parts, GeminiPart{FunctionCall: &GeminiFunctionCall{
				Name: b.ToolUse.Name,
				Args: geminiObjectOrNil(b.ToolUse.Input),
			}})

		case BlockToolResult:
			if b.ToolResult == nil {
				continue
			}
			name := names[b.ToolResult.ToolUseID]
			if name == "" {
				continue
			}
			parts = append(parts, GeminiPart{FunctionResponse: &GeminiFunctionResponse{
				Name:     name,
				Response: geminiToolResponse(b.ToolResult.Text),
			}})
		}
	}
	return parts, nil
}

// geminiImagePart 把图片地址翻成 gemini 的 part：data URI 走 inlineData（内联字节），
// 其它地址走 fileData（引用）。data URI 解不出 media type 或正文时整块丢弃，不猜。
func geminiImagePart(url string) (GeminiPart, bool) {
	if url == "" {
		return GeminiPart{}, false
	}
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		mediaType, data, found := strings.Cut(rest, ";base64,")
		if !found || mediaType == "" || data == "" {
			return GeminiPart{}, false
		}
		return GeminiPart{InlineData: &GeminiBlob{MimeType: mediaType, Data: data}}, true
	}
	return GeminiPart{FileData: &GeminiFileData{FileURI: url}}, true
}

// geminiObjectOrNil 取一个能写进 gemini 的 JSON 对象：gemini 的 args 与 response 都是对象
// （protobuf Struct），null、字符串、数组都写不进去。非对象整段省略，不让一条非对象的工具
// 参数把整条请求换成 400：这一条工具不能用，但其余对话还能走。
func geminiObjectOrNil(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}
	return trimmed
}

// geminiToolResponse 把工具结果的一段文本包成 gemini 要的对象。
// 中间表示的工具结果只有一段文本，gemini 的 response 是任意对象，因此固定写成
// {"result": 文本}：上游只要求它是对象，具体键由模型自行理解。
func geminiToolResponse(text string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"result": text})
	if err != nil {
		return nil
	}
	return b
}

// geminiArgumentsJSON 把上游给的函数参数变成中间表示的 input：空或不是合法 JSON 时按
// 「没有参数」处理成空对象，不猜内容。
func geminiArgumentsJSON(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return json.RawMessage(`{}`)
	}
	return trimmed
}

// geminiToolUseID 是给 gemini 的调用补的合成 id：上游不给 id，而 anthropic 的 tool_use 与
// chat 的 tool_calls 都要一个。按响应里的出现顺序编号，同一份响应多次解码
// 得到同一个值。
func geminiToolUseID(index int) string {
	return fmt.Sprintf("gemini_call_%d", index)
}

// geminiDataURI 把内联字节还原成 data URI，与另外两条协议搬运图片的写法一致。
func geminiDataURI(blob *GeminiBlob) string {
	if blob == nil || blob.Data == "" || blob.MimeType == "" {
		return ""
	}
	return "data:" + blob.MimeType + ";base64," + blob.Data
}

// toolNameByUseID 建一张 tool_use_id → 函数名的表，供工具结果查名字。
func toolNameByUseID(messages []Message) map[string]string {
	names := map[string]string{}
	for _, m := range messages {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolUse && b.ToolUse != nil && b.ToolUse.ID != "" {
				names[b.ToolUse.ID] = b.ToolUse.Name
			}
		}
	}
	return names
}

// geminiGeneration 把中间表示的采样参数编成 generationConfig，一项都没有时返回 nil。
func geminiGeneration(r *Request) *GeminiGeneration {
	gen := &GeminiGeneration{
		MaxOutputTokens: r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
	}
	if len(r.Stop) > 0 {
		gen.StopSequences = append([]string(nil), r.Stop...)
	}
	// 思考配置参数：gemini 用预算表达，所以预算优先、没预算时按档位折；Enabled 为假时
	// 写 thinkingBudget:0（这是 gemini 唯一能把「不思考」说清楚的写法）。
	if th := r.Thinking; th != nil {
		if !th.Enabled {
			zero := 0
			gen.ThinkingConfig = &GeminiThinkingConfig{ThinkingBudget: &zero}
		} else if budget, ok := th.budgetTokens(); ok {
			gen.ThinkingConfig = &GeminiThinkingConfig{ThinkingBudget: &budget}
		}
	}
	if gen.MaxOutputTokens == 0 && gen.Temperature == nil && gen.TopP == nil &&
		len(gen.StopSequences) == 0 && gen.ThinkingConfig == nil {
		return nil
	}
	return gen
}

// geminiToolChoiceToWire 把中间表示的工具选择编成 gemini 的 functionCallingConfig。
// gemini 只有 AUTO、ANY、NONE 三档，没有「只准调某一个函数」这一档：指定函数时写 ANY 加
// allowedFunctionNames，那是最接近的写法：降成 AUTO 就把「强制调用」变成了「可以调」；
// 白名单（allowed_tools）同理，名字原样进 allowedFunctionNames。
func geminiToolChoiceToWire(tc *ToolChoice) (*GeminiFunctionCallingConfig, error) {
	switch tc.Mode {
	case "", ToolChoiceAuto:
		return &GeminiFunctionCallingConfig{Mode: geminiModeAuto}, nil
	case ToolChoiceRequired:
		return &GeminiFunctionCallingConfig{Mode: geminiModeAny}, nil
	case ToolChoiceNone:
		return &GeminiFunctionCallingConfig{Mode: geminiModeNone}, nil
	case ToolChoiceNamed:
		if tc.Name == "" {
			return nil, errors.New("tool_choice 为指定工具时必须给出 name")
		}
		return &GeminiFunctionCallingConfig{Mode: geminiModeAny, AllowedFunctionNames: []string{tc.Name}}, nil
	case ToolChoiceAllowedTools:
		// 空白名单整条不写：ANY 不带 allowedFunctionNames 等于「随便调哪个已声明的函数」，
		// 那已经不再是白名单了。
		if len(tc.Allowed) == 0 {
			return nil, nil
		}
		names := append([]string(nil), tc.Allowed...)
		return &GeminiFunctionCallingConfig{Mode: geminiModeAny, AllowedFunctionNames: names}, nil
	default:
		return nil, fmt.Errorf("不支持的 tool_choice %q", tc.Mode)
	}
}

// geminiCallingConfig 取 toolConfig 里的配置，没有时返回 nil。
func geminiCallingConfig(cfg *GeminiToolConfig) *GeminiFunctionCallingConfig {
	if cfg == nil {
		return nil
	}
	return cfg.FunctionCallingConfig
}

// geminiToolChoiceFromWire 把 gemini 的工具选择折回中间表示。
// ANY 带白名单时：一个名字按指定工具处理，多个名字进 allowed_tools（中间表示能装下整个
// 白名单）；一个名字都没有的 ANY 退成 required：ANY 本身已经规定了必须调用。
func geminiToolChoiceFromWire(cfg *GeminiFunctionCallingConfig) *ToolChoice {
	switch strings.ToUpper(strings.TrimSpace(cfg.Mode)) {
	case geminiModeNone:
		return &ToolChoice{Mode: ToolChoiceNone}
	case geminiModeAny:
		names := make([]string, 0, len(cfg.AllowedFunctionNames))
		for _, name := range cfg.AllowedFunctionNames {
			if name != "" {
				names = append(names, name)
			}
		}
		switch len(names) {
		case 0:
			return &ToolChoice{Mode: ToolChoiceRequired}
		case 1:
			return &ToolChoice{Mode: ToolChoiceNamed, Name: names[0]}
		default:
			return &ToolChoice{Mode: ToolChoiceAllowedTools, Allowed: names}
		}
	default:
		return &ToolChoice{Mode: ToolChoiceAuto}
	}
}

// geminiMessage 把一条 gemini 内容折成 IR 消息。toolIndex 是整段对话里的调用序号，callIDs 记
// 函数名到合成 id 的待对回队列，两者都跨消息传递，工具结果才能对上前面那次调用：
// 对不上时上游会用 400 拒掉整条请求。
func geminiMessage(c GeminiContent, toolIndex *int, callIDs *map[string][]string) (Message, error) {
	role := RoleUser
	switch strings.ToLower(strings.TrimSpace(c.Role)) {
	case "", "user":
		role = RoleUser
	case "model":
		role = RoleAssistant
	default:
		return Message{}, fmt.Errorf("内容角色 %q 不受支持，gemini 只有 user 与 model", c.Role)
	}

	// 一轮新的工具调用开始，上一轮没被对回的 id 就过期了：gemini 的对话严格 user/model 交替，
	// 结果必须写在紧随其后的 user 消息里，留着上一轮的队列只会把结果对到更早的调用上。
	if role == RoleAssistant && geminiHasFunctionCall(c.Parts) {
		*callIDs = map[string][]string{}
	}

	msg := Message{Role: role}
	for _, part := range c.Parts {
		switch {
		case part.FunctionCall != nil:
			*toolIndex++
			id := geminiToolUseID(*toolIndex)
			if part.FunctionCall.Name != "" {
				(*callIDs)[part.FunctionCall.Name] = append((*callIDs)[part.FunctionCall.Name], id)
			}
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockToolUse, ToolUse: &ToolUse{
				ID:    id,
				Name:  part.FunctionCall.Name,
				Input: geminiArgumentsJSON(part.FunctionCall.Args),
			}})
		case part.FunctionResponse != nil:
			// gemini 按函数名对回调用，没有调用 id：同名调用排成队列按出现顺序取，与 gemini 要求
			// 「结果与调用同序」一致。查不到（历史被截断、跨请求续聊）时退回函数名——那种请求
			// 上游可能拒收，但丢掉整条结果更糟。
			id := ""
			if queue := (*callIDs)[part.FunctionResponse.Name]; len(queue) > 0 {
				id = queue[0]
				(*callIDs)[part.FunctionResponse.Name] = queue[1:]
			}
			if id == "" {
				id = part.FunctionResponse.Name
			}
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockToolResult, ToolResult: &ToolResult{
				ToolUseID: id,
				Text:      geminiResponseText(part.FunctionResponse.Response),
			}})
		case part.InlineData != nil:
			if url := geminiDataURI(part.InlineData); url != "" {
				msg.Blocks = append(msg.Blocks, Block{Kind: BlockImage, Image: &Image{URL: url}})
			}
		case part.FileData != nil:
			if part.FileData.FileURI != "" {
				msg.Blocks = append(msg.Blocks, Block{Kind: BlockImage, Image: &Image{URL: part.FileData.FileURI}})
			}
		case part.Text != "":
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockText, Text: part.Text})
		}
	}
	return msg, nil
}

// geminiHasFunctionCall 判断这一组 part 里有没有工具调用。
func geminiHasFunctionCall(parts []GeminiPart) bool {
	for _, part := range parts {
		if part.FunctionCall != nil {
			return true
		}
	}
	return false
}

// geminiTextBlocks 取一段内容里的文本块，用于 systemInstruction。
func geminiTextBlocks(parts []GeminiPart) []Block {
	var blocks []Block
	for _, part := range parts {
		if part.Text != "" {
			blocks = append(blocks, Block{Kind: BlockText, Text: part.Text})
		}
	}
	return blocks
}

// geminiResponseText 把工具结果的对象折成一段文本：中间表示只有一段文本，
// 对象按紧凑 JSON 写进去，不猜哪个键才是正文。
func geminiResponseText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return ""
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return s
	}
	return string(trimmed)
}

// geminiStopToIR 把 finishReason 折成中间表示的四种结束原因。gemini 没有「因为工具调用而停」
// 这一档（有 functionCall 时 finishReason 仍是 STOP），所以出现工具调用时按 tool_use 给出去：
// 客户端要靠它走工具调用分支，写 end_turn 会让 anthropic 与 chat 的客户端当普通回复结束。
// 其余取值（SAFETY、RECITATION、BLOCKLIST 等）在中间表示里没有对应项，按 end_turn 处理：
// 编一个不存在的取值会让客户端拿到它不认识的枚举值。withToolUse 由调用方先扫一遍 parts 得到。
func geminiStopToIR(reason string, withToolUse bool) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return StopMaxTokens
	case "", "STOP", "OTHER":
		if withToolUse {
			return StopToolUse
		}
		return StopEndTurn
	default:
		return StopEndTurn
	}
}

// geminiStopNote 给出 finishReason 里「中间表示装不下」的那一档，供网关写进日志备注。
// STOP、MAX_TOKENS 与空值都是已映射的正常取值，不产生备注；其余（SAFETY、RECITATION、
// BLOCKLIST、MALFORMED_FUNCTION_CALL 等）原样返回，让用户知道这次回答为什么被截断。
func geminiStopNote(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "", "STOP", "MAX_TOKENS", "OTHER":
		return ""
	default:
		return strings.ToUpper(strings.TrimSpace(reason))
	}
}

// geminiFinishFromIR 把中间表示的结束原因翻回 gemini 的 finishReason。gemini 只有 STOP 与
// MAX_TOKENS 两个正常值，`tool_use` 与 `stop_sequence` 都写成 STOP：前者在 gemini 里本来就是
// STOP 加 functionCall，后者没有对应取值，编一个假的会被客户端当成上游事实。
func geminiFinishFromIR(reason string) string {
	if reason == StopMaxTokens {
		return "MAX_TOKENS"
	}
	return "STOP"
}

// hasToolUse 判断响应里有没有工具调用块。
func hasToolUse(blocks []Block) bool {
	for _, b := range blocks {
		if b.Kind == BlockToolUse {
			return true
		}
	}
	return false
}

// geminiUsageToIR 把 gemini 的用量映射成 IR 用量。nil 视为零值。
// 思考 token 与总 token 都要带上：真实 gemini 的 totalTokenCount = prompt + candidates + thoughts，
// 入账时按它判定 thoughts 要不要单独计价。
func geminiUsageToIR(u *GeminiUsageMetadata) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		In:        u.PromptTokenCount,
		Out:       u.CandidatesTokenCount,
		CacheRead: u.CachedContentTokenCount,
		Reasoning: u.ThoughtsTokenCount,
		Total:     u.TotalTokenCount,
	}
}
