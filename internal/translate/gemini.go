package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件负责 gemini（Generative Language API 的 generateContent）与中间表示之间的编解码。
// 第一步只做非流式出站（DESIGN 5.9）：模型名在 URL 路径里，不在请求体里，因此编码器不写
// model 字段；入站路径与流式都还没实现，解码器只留一个能跑通最小请求的实现给将来用。
// 映射规则见 DESIGN 5.9，表里没有的字段一律丢弃。

// gemini 的 toolConfig 模式。gemini 只有这三种，没有「必须调用某个具体函数」这一档，
// 指定函数时用 ANY 加白名单（DESIGN 5.9）。
const (
	geminiModeAuto = "AUTO"
	geminiModeAny  = "ANY"
	geminiModeNone = "NONE"
)

// GeminiRequest 是 POST {base}/v1beta/models/{model}:generateContent 的请求体。
// 模型名不在请求体里：它在路径上，由网关按路由的 {model} 占位符填进去（DESIGN 5.9）。
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
	Parts []GeminiPart `json:"parts"`
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

// GeminiFileData 是对上游文件的引用。只有 Files API 给出的 URI 才一定可用（见 DESIGN 5.9）。
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

// GeminiGeneration 是采样参数。
type GeminiGeneration struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
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
// 与 chat 的 prompt_tokens_details.cached_tokens 同一套记法（DESIGN 5.9）。
type GeminiUsageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
}

// EncodeGeminiRequest 把 IR 编成 generateContent 的请求体。
//
// model 不写进报文（gemini 的模型名在路径里），参数留着是为了与另外三个编码器同一形状：
// 网关不必为 gemini 分情况。
func EncodeGeminiRequest(r *Request, model string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("请求为空")
	}
	// 流式不在第一步的范围里：gemini 的流式走 streamGenerateContent 与 ?alt=sse，
	// 请求体形状一样但响应形状另一套，写出一个非流式请求换回一整段 JSON，
	// 客户端拿到的就不是它要的流（DESIGN 5.9）。
	if r.Stream {
		return nil, errors.New("gemini 出站暂不支持流式请求：第一步只做非流式（见 DESIGN 5.9）")
	}
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
		out.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: cfg}
	}

	if gen := geminiGeneration(r); gen != nil {
		out.GenerationConfig = gen
	}

	return json.Marshal(out)
}

// DecodeGeminiRequest 把 generateContent 的请求体解析成 IR。入站 gemini 还没有路径
// （DESIGN 第 7 节已知未做），这里是留给将来用的最小实现：报文里没有模型名——它在 URL 路径上，
// 入站实现时由路径注入，因此 Model 留空，由调用方填。
func DecodeGeminiRequest(raw []byte) (*Request, error) {
	var wire GeminiRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("gemini 请求体不是合法 JSON: %w", err)
	}

	out := &Request{}
	if wire.SystemInstruction != nil {
		out.System = geminiTextBlocks(wire.SystemInstruction.Parts)
	}

	for i, c := range wire.Contents {
		msg, err := geminiMessage(c)
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
			// 按出现顺序补一个固定的合成 id（DESIGN 5.9），编回 gemini 时它本来就会被丢掉。
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

	out.StopReason = geminiStopToIR(cand.FinishReason, out.Blocks)
	out.Usage = geminiUsageToIR(wire.UsageMetadata)
	return out, nil
}

// geminiUsageScanner 从响应体里读 gemini 的用量，用于同协议直通与探活记账。
// 第一步没有入站 gemini 路径，它实际只服务探活的非流式响应体。
type geminiUsageScanner struct {
	in, out, cacheRead int64
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
}

// Usage 返回读到的用量。gemini 没有缓存写入字段，cacheWrite 恒为 0。
func (s *geminiUsageScanner) Usage() (in, out, cacheRead, cacheWrite int64) {
	return s.in, s.out, s.cacheRead, 0
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
// functionResponse 是 user 轮次里的一块（DESIGN 5.9）。
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
			// 思考内容只在响应方向搬运，请求方向整块丢弃（DESIGN 5.4 的规则对 gemini 同样成立，
			// gemini 的请求里也没有放思考内容的位置）。
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
// 参数把整条请求换成 400（DESIGN 5.9 的取舍）。
func geminiObjectOrNil(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}
	return trimmed
}

// geminiToolResponse 把工具结果的一段文本包成 gemini 要的对象。
// 中间表示的工具结果只有一段文本，gemini 的 response 是任意对象，因此固定写成
// {"result": 文本}（见 DESIGN 5.9；上游只要求它是对象，具体键由模型自行理解）。
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
// chat 的 tool_calls 都要一个（DESIGN 5.9）。按响应里的出现顺序编号，同一份响应多次解码
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
	if gen.MaxOutputTokens == 0 && gen.Temperature == nil && gen.TopP == nil && len(gen.StopSequences) == 0 {
		return nil
	}
	return gen
}

// geminiToolChoiceToWire 把中间表示的工具选择编成 gemini 的 functionCallingConfig。
// gemini 只有 AUTO、ANY、NONE 三档，没有「只准调某一个函数」这一档：指定函数时写 ANY 加
// allowedFunctionNames，那是最接近的写法（DESIGN 5.9 的取舍）。
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
// ANY 带白名单时按指定工具处理，白名单有多个名字时退成 required：中间表示只能表达一个名字。
func geminiToolChoiceFromWire(cfg *GeminiFunctionCallingConfig) *ToolChoice {
	switch strings.ToUpper(strings.TrimSpace(cfg.Mode)) {
	case geminiModeNone:
		return &ToolChoice{Mode: ToolChoiceNone}
	case geminiModeAny:
		if len(cfg.AllowedFunctionNames) == 1 && cfg.AllowedFunctionNames[0] != "" {
			return &ToolChoice{Mode: ToolChoiceNamed, Name: cfg.AllowedFunctionNames[0]}
		}
		return &ToolChoice{Mode: ToolChoiceRequired}
	default:
		return &ToolChoice{Mode: ToolChoiceAuto}
	}
}

// geminiMessage 把一条 gemini 内容折成 IR 消息。
func geminiMessage(c GeminiContent) (Message, error) {
	role := RoleUser
	switch strings.ToLower(strings.TrimSpace(c.Role)) {
	case "", "user":
		role = RoleUser
	case "model":
		role = RoleAssistant
	default:
		return Message{}, fmt.Errorf("内容角色 %q 不受支持，gemini 只有 user 与 model", c.Role)
	}

	msg := Message{Role: role}
	toolIndex := 0
	for _, part := range c.Parts {
		switch {
		case part.FunctionCall != nil:
			toolIndex++
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockToolUse, ToolUse: &ToolUse{
				ID:    geminiToolUseID(toolIndex),
				Name:  part.FunctionCall.Name,
				Input: geminiArgumentsJSON(part.FunctionCall.Args),
			}})
		case part.FunctionResponse != nil:
			msg.Blocks = append(msg.Blocks, Block{Kind: BlockToolResult, ToolResult: &ToolResult{
				// gemini 按函数名对回调用，没有调用 id：入站时把函数名写进 ToolUseID，
				// 编到别的协议时由那边的编码器决定怎么用（见 DESIGN 5.9）。
				ToolUseID: part.FunctionResponse.Name,
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
// 对象按紧凑 JSON 写进去，不猜哪个键才是正文（见 DESIGN 5.9）。
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
// 其余取值（SAFETY、RECITATION、BLOCKLIST 等）在中间表示里没有对应项，按 end_turn 处理。
func geminiStopToIR(reason string, blocks []Block) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return StopMaxTokens
	case "", "STOP", "OTHER":
		if hasToolUse(blocks) {
			return StopToolUse
		}
		return StopEndTurn
	default:
		return StopEndTurn
	}
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
func geminiUsageToIR(u *GeminiUsageMetadata) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{In: u.PromptTokenCount, Out: u.CandidatesTokenCount, CacheRead: u.CachedContentTokenCount}
}
