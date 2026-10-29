package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件实现 openai-responses 的编解码：入站 /v1/responses 与 IR 互转，
// 以及流式事件、直通流用量读取。全部是纯函数与纯状态机，不读环境、不发网络。

// responsesRequest 是 POST /v1/responses 的请求体。
// input 是字符串或 item 数组，tool_choice 是字符串或对象，两者都先留原文再按需解析。
type responsesRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             json.RawMessage `json:"input,omitempty"`
	Tools             []responsesTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	MaxOutputTokens   int             `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

// responsesTool 是请求里的工具定义。第一版只认 function，其余 type 丢弃。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// responsesInputItem 是 input 数组里的一项。content 可能是字符串或部件数组，
// output 可能是字符串或部件数组，reasoning item 的正文在 content 或 summary 里，都先留原文。
type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// responsesContentPart 是 message 内容数组里的一个部件。
// image_url 在 Responses 里是字符串，个别客户端发对象，取值时两种都认。
type responsesContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	Refusal  string          `json:"refusal"`
}

// responsesIncomplete 是未完成原因。
type responsesIncomplete struct {
	Reason string `json:"reason"`
}

// responsesTokenDetail 携带命中缓存的输入 token 数。
type responsesTokenDetail struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// responsesUsage 是 Responses 的用量字段。
type responsesUsage struct {
	InputTokens        int64                 `json:"input_tokens"`
	OutputTokens       int64                 `json:"output_tokens"`
	TotalTokens        int64                 `json:"total_tokens"`
	InputTokensDetails *responsesTokenDetail `json:"input_tokens_details,omitempty"`
}

// responsesOutputItem 是响应 output 数组里的一项。
type responsesOutputItem struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Role      string                 `json:"role"`
	Status    string                 `json:"status"`
	Content   []responsesContentPart `json:"content"`
	Summary   []responsesContentPart `json:"summary"`
	CallID    string                 `json:"call_id"`
	Name      string                 `json:"name"`
	Arguments string                 `json:"arguments"`
}

// responsesError 是上游错误对象。
type responsesError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responsesResponse 是 Responses 的响应体，流式事件的 response 字段也复用它。
type responsesResponse struct {
	ID                string                `json:"id"`
	Object            string                `json:"object"`
	Status            string                `json:"status"`
	Model             string                `json:"model"`
	Output            []responsesOutputItem `json:"output"`
	IncompleteDetails *responsesIncomplete  `json:"incomplete_details"`
	Usage             *responsesUsage       `json:"usage"`
	Error             *responsesError       `json:"error"`
}

// responsesUsageOut 是编码响应时写出的用量。
type responsesUsageOut struct {
	InputTokens        int64                 `json:"input_tokens"`
	OutputTokens       int64                 `json:"output_tokens"`
	TotalTokens        int64                 `json:"total_tokens"`
	InputTokensDetails *responsesTokenDetail `json:"input_tokens_details,omitempty"`
}

// responsesResponseOut 是编码响应时的报文结构。
type responsesResponseOut struct {
	ID                string               `json:"id"`
	Object            string               `json:"object"`
	Status            string               `json:"status"`
	Model             string               `json:"model"`
	Output            []any                `json:"output"`
	IncompleteDetails *responsesIncomplete `json:"incomplete_details,omitempty"`
	Usage             responsesUsageOut    `json:"usage"`
}

// responsesStreamEvent 是流式事件的公共信封，字段按事件名取用。
type responsesStreamEvent struct {
	Type         string               `json:"type"`
	OutputIndex  *int                 `json:"output_index"`
	ContentIndex *int                 `json:"content_index"`
	Delta        string               `json:"delta"`
	ItemID       string               `json:"item_id"`
	Item         *responsesStreamItem `json:"item"`
	Response     *responsesResponse   `json:"response"`
	Message      string               `json:"message"`
	Error        *responsesError      `json:"error"`
}

// responsesStreamItem 是流式事件里出现的 output item，只取用得到的字段。
type responsesStreamItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// DecodeResponsesRequest 把 POST /v1/responses 的请求体解析成 IR。
func DecodeResponsesRequest(raw []byte) (*Request, error) {
	var in responsesRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}

	req := &Request{
		Model:             in.Model,
		MaxTokens:         in.MaxOutputTokens,
		Temperature:       in.Temperature,
		TopP:              in.TopP,
		Stream:            in.Stream,
		ParallelToolCalls: in.ParallelToolCalls,
	}
	if s := strings.TrimSpace(in.Instructions); s != "" {
		req.System = append(req.System, Block{Kind: BlockText, Text: s})
	}

	input := bytes.TrimSpace(in.Input)
	switch {
	case len(input) == 0 || string(input) == "null":
		// 没有输入，保持空消息列表。
	case input[0] == '"':
		var s string
		if err := json.Unmarshal(input, &s); err != nil {
			return nil, fmt.Errorf("input 不是合法字符串: %w", err)
		}
		responsesAddBlock(req, RoleUser, Block{Kind: BlockText, Text: s})
	case input[0] == '[':
		var items []responsesInputItem
		if err := json.Unmarshal(input, &items); err != nil {
			return nil, fmt.Errorf("input 不是合法数组: %w", err)
		}
		for i, item := range items {
			if err := responsesAddInputItem(req, item); err != nil {
				return nil, fmt.Errorf("input 第 %d 项: %w", i+1, err)
			}
		}
	default:
		return nil, errors.New("input 必须是字符串或 item 数组")
	}

	for _, t := range in.Tools {
		if t.Type != "function" && t.Type != "" {
			// web_search、file_search 等第一版不支持，直接丢弃。
			continue
		}
		if t.Name == "" {
			continue
		}
		req.Tools = append(req.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Parameters,
		})
	}

	choice, err := responsesDecodeToolChoice(in.ToolChoice)
	if err != nil {
		return nil, err
	}
	req.ToolChoice = choice

	return req, nil
}

// responsesAddInputItem 把一条 input item 写进 IR。
func responsesAddInputItem(req *Request, item responsesInputItem) error {
	switch item.Type {
	case "", "message":
		role := Role(item.Role)
		if role == "" {
			role = RoleUser
		}
		switch role {
		case RoleSystem:
			text := responsesContentText(item.Content)
			if text != "" {
				req.System = append(req.System, Block{Kind: BlockText, Text: text})
			}
		case RoleUser, RoleAssistant:
			blocks, err := responsesContentBlocks(item.Content)
			if err != nil {
				return err
			}
			for _, b := range blocks {
				responsesAddBlock(req, role, b)
			}
		default:
			return fmt.Errorf("不支持的消息角色 %q", item.Role)
		}
		return nil

	case "function_call":
		args := strings.TrimSpace(item.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return errors.New("function_call 的 arguments 不是合法 JSON")
		}
		responsesAddBlock(req, RoleAssistant, Block{
			Kind: BlockToolUse,
			ToolUse: &ToolUse{
				ID:    item.CallID,
				Name:  item.Name,
				Input: json.RawMessage(args),
			},
		})
		return nil

	case "function_call_output":
		responsesAddBlock(req, RoleTool, Block{
			Kind: BlockToolResult,
			ToolResult: &ToolResult{
				ToolUseID: item.CallID,
				Text:      responsesOutputText(item.Output),
			},
		})
		return nil

	case "reasoning":
		// 思考内容先于正文进消息：报文里的先后已不可考，统一按「先想后说」排。
		if text := responsesReasoningTextRaw(item.Content, item.Summary); text != "" {
			responsesAddBlock(req, RoleAssistant, Block{Kind: BlockThinking, Thinking: &Thinking{Text: text}})
		}
		return nil

	default:
		// item_reference 等第一版不支持，丢弃。
		return nil
	}
}

// responsesAddBlock 追加一个块。与上一条消息同角色时并进去，
// 避免把一段连续的 assistant 文本与工具调用拆成多条消息。
func responsesAddBlock(req *Request, role Role, b Block) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == role {
		req.Messages[n-1].Blocks = append(req.Messages[n-1].Blocks, b)
		return
	}
	req.Messages = append(req.Messages, Message{Role: role, Blocks: []Block{b}})
}

// responsesContentBlocks 把 message 的 content 折成 IR 块。字符串视为一个 text 块。
func responsesContentBlocks(raw json.RawMessage) ([]Block, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("content 不是合法字符串: %w", err)
		}
		return []Block{{Kind: BlockText, Text: s}}, nil
	}

	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content 不是合法字符串或部件数组: %w", err)
	}

	var blocks []Block
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			blocks = append(blocks, Block{Kind: BlockText, Text: p.Text})
		case "input_image":
			if url := responsesImageURL(p.ImageURL); url != "" {
				blocks = append(blocks, Block{Kind: BlockImage, Image: &Image{URL: url}})
			}
		}
	}
	return blocks, nil
}

// responsesContentText 把 content 里的文本按顺序拼成一个字符串，忽略图片等其它部件。
func responsesContentText(raw json.RawMessage) string {
	blocks, err := responsesContentBlocks(raw)
	if err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Kind == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// responsesImageURL 取 image_url 字段，字符串与 {url:...} 对象都认。
func responsesImageURL(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return obj.URL
}

// responsesReasoningText 取 reasoning item 的正文：content 与 summary 里的文本都算，
// 两处都有时 content 在前、summary 在后，用换行拼起来（DESIGN 5.4）。
func responsesReasoningText(content, summary []responsesContentPart) string {
	var parts []string
	for _, group := range [][]responsesContentPart{content, summary} {
		for _, p := range group {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// responsesReasoningTextRaw 用于请求的 input item：content 与 summary 还是原始 JSON。
func responsesReasoningTextRaw(content, summary json.RawMessage) string {
	return responsesReasoningText(responsesParts(content), responsesParts(summary))
}

// responsesParts 解析部件数组，不是数组时返回 nil。
// reasoning item 的部件有 reasoning_text 与 summary_text 两种，正文都在 text 字段，
// 这里不看类型，其它字段一概不取。
func responsesParts(raw json.RawMessage) []responsesContentPart {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	return parts
}

// responsesOutputText 把 function_call_output 的 output 折成文本。
func responsesOutputText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// responsesDecodeToolChoice 解析 tool_choice：字符串或 {type:"function",name:...}。
// 认不出的写法返回 nil，按「未在表内一律丢弃」处理。
func responsesDecodeToolChoice(raw json.RawMessage) (*ToolChoice, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("tool_choice 不是合法字符串: %w", err)
		}
		switch s {
		case ToolChoiceAuto, ToolChoiceRequired, ToolChoiceNone:
			return &ToolChoice{Mode: s}, nil
		default:
			return nil, nil
		}
	}

	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("tool_choice 不是合法对象: %w", err)
	}
	if (obj.Type == "function" || obj.Type == "tool") && obj.Name != "" {
		return &ToolChoice{Mode: ToolChoiceNamed, Name: obj.Name}, nil
	}
	return nil, nil
}

// EncodeResponsesRequest 把 IR 编成 Responses 请求体。model 是上游实际使用的模型名。
func EncodeResponsesRequest(r *Request, model string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("请求为空")
	}
	if model == "" {
		model = r.Model
	}

	out := responsesRequest{
		Model:             model,
		MaxOutputTokens:   r.MaxTokens,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		Stream:            r.Stream,
		ParallelToolCalls: r.ParallelToolCalls,
	}

	var instructions []string
	for _, b := range r.System {
		if b.Kind == BlockText && b.Text != "" {
			instructions = append(instructions, b.Text)
		}
	}

	items := []any{}
	for _, m := range r.Messages {
		if m.Role == RoleSystem {
			// IR 里的 system 消息在 Responses 里没有对应位置，折进 instructions。
			for _, b := range m.Blocks {
				if b.Kind == BlockText && b.Text != "" {
					instructions = append(instructions, b.Text)
				}
			}
			continue
		}
		msgItems, err := responsesEncodeMessage(m)
		if err != nil {
			return nil, err
		}
		items = append(items, msgItems...)
	}
	out.Instructions = strings.Join(instructions, "\n")

	input, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("编码 input 失败: %w", err)
	}
	out.Input = input

	for _, t := range r.Tools {
		if !isClientTool(t) {
			// anthropic 的服务端内置工具在 responses 里没有对应形状，只认 function。
			continue
		}
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}

	choice, err := responsesEncodeToolChoice(r.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = choice

	return json.Marshal(out)
}

// responsesEncodeMessage 把一条 IR 消息展开成 input 数组里的若干项。
// 文本与图片合成一个 message item；tool_use 与 tool_result 各自独立成项，
// 出现位置决定它们在 input 数组里的先后。
func responsesEncodeMessage(m Message) ([]any, error) {
	role := "user"
	textType := "input_text"
	if m.Role == RoleAssistant {
		role = "assistant"
		textType = "output_text"
	}

	items := []any{}
	var parts []any
	flush := func() {
		if len(parts) == 0 {
			return
		}
		items = append(items, map[string]any{
			"type":    "message",
			"role":    role,
			"content": parts,
		})
		parts = nil
	}

	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText:
			parts = append(parts, map[string]any{"type": textType, "text": b.Text})
		case BlockThinking:
			if b.Thinking == nil || b.Thinking.Text == "" {
				continue
			}
			flush()
			items = append(items, responsesEncodeReasoning(*b.Thinking))
		case BlockImage:
			if b.Image == nil || b.Image.URL == "" {
				continue
			}
			parts = append(parts, map[string]any{"type": "input_image", "image_url": b.Image.URL})
		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			flush()
			items = append(items, responsesEncodeFunctionCall(*b.ToolUse))
		case BlockToolResult:
			if b.ToolResult == nil {
				continue
			}
			flush()
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": b.ToolResult.ToolUseID,
				"output":  b.ToolResult.Text,
			})
		}
	}
	flush()
	return items, nil
}

// responsesEncodeReasoning 把思考内容编成 reasoning item。
// 正文写在 summary 的 summary_text 里：content 里的 reasoning_text 要与 item id 和
// encrypted_content 配套上游才认，这三样不搬，因此只写得出 summary 这一种形状。
func responsesEncodeReasoning(t Thinking) map[string]any {
	return map[string]any{
		"type": "reasoning",
		"summary": []any{
			map[string]any{"type": "summary_text", "text": t.Text},
		},
	}
}

// responsesEncodeFunctionCall 把一个工具调用编成 function_call item。
func responsesEncodeFunctionCall(u ToolUse) map[string]any {
	args := strings.TrimSpace(string(u.Input))
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   u.ID,
		"name":      u.Name,
		"arguments": args,
	}
}

// responsesEncodeToolChoice 把 IR 的工具选择编成 Responses 的写法。
func responsesEncodeToolChoice(tc *ToolChoice) (json.RawMessage, error) {
	if tc == nil {
		return nil, nil
	}
	switch tc.Mode {
	case "", ToolChoiceAuto:
		return json.RawMessage(`"auto"`), nil
	case ToolChoiceRequired:
		return json.RawMessage(`"required"`), nil
	case ToolChoiceNone:
		return json.RawMessage(`"none"`), nil
	case ToolChoiceNamed, "function":
		if tc.Name == "" {
			return nil, errors.New("tool_choice 为命名工具时必须给出 name")
		}
		b, err := json.Marshal(map[string]string{"type": "function", "name": tc.Name})
		if err != nil {
			return nil, fmt.Errorf("编码 tool_choice 失败: %w", err)
		}
		return json.RawMessage(b), nil
	default:
		return nil, fmt.Errorf("不支持的 tool_choice 模式 %q", tc.Mode)
	}
}

// DecodeResponsesResponse 解析非流式响应。requestedModel 会写回给客户端。
func DecodeResponsesResponse(raw []byte, requestedModel string) (*Response, error) {
	var in responsesResponse
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON: %w", err)
	}

	out := &Response{
		ID:    responsesNormalizeID(in.ID),
		Model: requestedModel,
	}
	if out.Model == "" {
		out.Model = in.Model
	}

	hasToolUse := false
	for i, item := range in.Output {
		switch item.Type {
		case "reasoning":
			if text := responsesReasoningText(item.Content, item.Summary); text != "" {
				out.Blocks = append(out.Blocks, Block{Kind: BlockThinking, Thinking: &Thinking{Text: text}})
			}
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text", "text":
					if part.Text != "" {
						out.Blocks = append(out.Blocks, Block{Kind: BlockText, Text: part.Text})
					}
				case "refusal":
					if part.Refusal != "" {
						out.Blocks = append(out.Blocks, Block{Kind: BlockText, Text: part.Refusal})
					}
				}
			}
		case "function_call":
			args := strings.TrimSpace(item.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, fmt.Errorf("上游第 %d 个 function_call 的 arguments 不是合法 JSON", i+1)
			}
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			out.Blocks = append(out.Blocks, Block{
				Kind: BlockToolUse,
				ToolUse: &ToolUse{
					ID:    id,
					Name:  item.Name,
					Input: json.RawMessage(args),
				},
			})
			hasToolUse = true
		}
	}

	out.StopReason = responsesStopReason(in, hasToolUse)
	if u := in.Usage; u != nil {
		out.Usage = Usage{In: u.InputTokens, Out: u.OutputTokens}
		if u.InputTokensDetails != nil {
			out.Usage.CacheRead = u.InputTokensDetails.CachedTokens
		}
	}
	return out, nil
}

// responsesStopReason 按 DESIGN.md 第 5 节的规则映射结束原因：
// max_output_tokens 优先，其次有工具调用算 tool_use，其余算 end_turn。
func responsesStopReason(in responsesResponse, hasToolUse bool) string {
	if in.IncompleteDetails != nil {
		switch in.IncompleteDetails.Reason {
		case "max_output_tokens":
			return StopMaxTokens
		case "content_filter":
			return StopEndTurn
		}
	}
	if hasToolUse || responsesOutputHasToolUse(in.Output) {
		return StopToolUse
	}
	return StopEndTurn
}

// responsesOutputHasToolUse 判断 output 里有没有 function_call。
func responsesOutputHasToolUse(items []responsesOutputItem) bool {
	for _, item := range items {
		if item.Type == "function_call" {
			return true
		}
	}
	return false
}

// responsesNormalizeID 保证响应 id 以 resp_ 开头，客户端据此区分协议。
func responsesNormalizeID(id string) string {
	if id == "" {
		return "resp_switchboard"
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

// EncodeResponsesResponse 把 IR 响应编成 Responses 响应体。
func EncodeResponsesResponse(r *Response) ([]byte, error) {
	if r == nil {
		return nil, errors.New("响应为空")
	}

	status := "completed"
	var incomplete *responsesIncomplete
	if r.StopReason == StopMaxTokens {
		status = "incomplete"
		incomplete = &responsesIncomplete{Reason: "max_output_tokens"}
	}

	output := []any{}
	var parts []any
	flush := func() {
		if len(parts) == 0 {
			return
		}
		output = append(output, map[string]any{
			"type":    "message",
			"role":    "assistant",
			"status":  "completed",
			"content": parts,
		})
		parts = nil
	}

	toolCount := 0
	for _, b := range r.Blocks {
		switch b.Kind {
		case BlockText:
			parts = append(parts, map[string]any{"type": "output_text", "text": b.Text})
		case BlockThinking:
			if b.Thinking == nil || b.Thinking.Text == "" {
				continue
			}
			flush()
			output = append(output, responsesEncodeReasoning(*b.Thinking))
		case BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			flush()
			toolCount++
			args := strings.TrimSpace(string(b.ToolUse.Input))
			if args == "" {
				args = "{}"
			}
			id := b.ToolUse.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", toolCount)
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        "fc_" + strings.TrimPrefix(id, "fc_"),
				"call_id":   id,
				"name":      b.ToolUse.Name,
				"arguments": args,
				"status":    "completed",
			})
		}
	}
	flush()

	out := responsesResponseOut{
		ID:                responsesNormalizeID(r.ID),
		Object:            "response",
		Status:            status,
		Model:             r.Model,
		Output:            output,
		IncompleteDetails: incomplete,
		Usage: responsesUsageOut{
			InputTokens:  r.Usage.In,
			OutputTokens: r.Usage.Out,
			TotalTokens:  r.Usage.In + r.Usage.Out,
		},
	}
	if r.Usage.CacheRead > 0 {
		out.Usage.InputTokensDetails = &responsesTokenDetail{CachedTokens: r.Usage.CacheRead}
	}

	return json.Marshal(out)
}

// responsesStreamDecoder 把 Responses 的流式事件翻成 IR 增量。
// 只处理文本、工具调用与收尾，其余事件按设计丢弃。
type responsesStreamDecoder struct {
	next          int            // 自增的 output 序号，上游没给 output_index 时使用
	toolIndex     map[string]int // item id / call_id → output_index
	lastToolIndex int
	hasTool       bool
	finished      bool
	// 按 output 序号记「已经发过哪个增量」：只发 output_item.done 的上游需要在
	// 那里补 ToolStart / ToolArgs，已经发过的部分不能重复（DESIGN 5.4）。
	toolStarted  map[int]bool
	toolArgsSeen map[int]bool
}

// Decode 处理一帧 data。data 是 SSE 的 data 字段内容。
func (d *responsesStreamDecoder) Decode(data []byte) ([]Delta, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "[DONE]" {
		return nil, nil
	}

	var env responsesStreamEvent
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, fmt.Errorf("上游流式帧不是合法 JSON: %w", err)
	}
	if d.toolIndex == nil {
		d.toolIndex = map[string]int{}
	}
	if d.toolStarted == nil {
		d.toolStarted = map[int]bool{}
	}
	if d.toolArgsSeen == nil {
		d.toolArgsSeen = map[int]bool{}
	}

	switch env.Type {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if env.Delta == "" {
			return nil, nil
		}
		return []Delta{{Kind: DeltaThinking, Text: env.Delta}}, nil

	case "response.output_text.delta":
		if env.Delta == "" {
			return nil, nil
		}
		return []Delta{{
			Kind:  DeltaText,
			Text:  env.Delta,
			Index: d.indexAt(env.OutputIndex, 0),
		}}, nil

	case "response.output_item.added":
		if env.Item == nil || env.Item.Type != "function_call" {
			return nil, nil
		}
		idx := d.indexAt(env.OutputIndex, d.next)
		key := env.Item.ID
		if key == "" {
			key = env.Item.CallID
		}
		if key != "" {
			d.toolIndex[key] = idx
		}
		d.lastToolIndex = idx
		d.hasTool = true
		d.toolStarted[idx] = true
		id := env.Item.CallID
		if id == "" {
			id = env.Item.ID
		}
		return []Delta{{
			Kind:     DeltaToolStart,
			Index:    idx,
			ToolID:   id,
			ToolName: env.Item.Name,
		}}, nil

	case "response.output_item.done":
		// 只发 done、不发 added 与 arguments.delta 的上游：整条工具调用在这里补齐，
		// 已经发过的部分按 output 序号跳过，避免重复。
		if env.Item == nil || env.Item.Type != "function_call" {
			return nil, nil
		}
		key := firstNonEmpty(env.Item.ID, env.Item.CallID)
		idx, known := d.toolIndex[key]
		if !known {
			// 没见过这个 item：优先用上游给的 output_index，没给就新开一个序号。
			if env.OutputIndex != nil {
				idx = d.indexAt(env.OutputIndex, d.next)
			} else {
				idx = d.indexAt(nil, d.next)
			}
		}
		if key != "" {
			d.toolIndex[key] = idx
		}
		d.lastToolIndex = idx
		d.hasTool = true

		var out []Delta
		if !d.toolStarted[idx] {
			d.toolStarted[idx] = true
			out = append(out, Delta{
				Kind:     DeltaToolStart,
				Index:    idx,
				ToolID:   firstNonEmpty(env.Item.CallID, env.Item.ID),
				ToolName: env.Item.Name,
			})
		}
		if !d.toolArgsSeen[idx] && env.Item.Arguments != "" {
			d.toolArgsSeen[idx] = true
			out = append(out, Delta{Kind: DeltaToolArgs, Index: idx, ArgsJSON: env.Item.Arguments})
		}
		return out, nil

	case "response.function_call_arguments.delta":
		if env.Delta == "" {
			return nil, nil
		}
		idx := d.lastToolIndex
		if env.OutputIndex != nil {
			idx = *env.OutputIndex
		} else if v, ok := d.toolIndex[env.ItemID]; ok {
			idx = v
		}
		d.toolStarted[idx] = true
		d.toolArgsSeen[idx] = true
		return []Delta{{
			Kind:     DeltaToolArgs,
			Index:    idx,
			ArgsJSON: env.Delta,
		}}, nil

	case "response.completed", "response.incomplete":
		if d.finished {
			return nil, nil
		}
		d.finished = true

		hasTool := d.hasTool
		reason := StopEndTurn
		var usage *Usage
		if env.Response != nil {
			if !hasTool {
				hasTool = responsesOutputHasToolUse(env.Response.Output)
			}
			reason = responsesStopReason(*env.Response, hasTool)
			if u := env.Response.Usage; u != nil {
				usage = &Usage{In: u.InputTokens, Out: u.OutputTokens}
				if u.InputTokensDetails != nil {
					usage.CacheRead = u.InputTokensDetails.CachedTokens
				}
			}
		} else if hasTool {
			reason = StopToolUse
		}
		return []Delta{{Kind: DeltaStop, StopReason: reason, Usage: usage}}, nil

	case "response.failed":
		if d.finished {
			return nil, nil
		}
		d.finished = true
		return []Delta{{Kind: DeltaError, Text: responsesStreamErrorText(env.Response)}}, nil

	case "error":
		if d.finished {
			return nil, nil
		}
		d.finished = true
		text := env.Message
		if env.Error != nil && env.Error.Message != "" {
			text = env.Error.Message
		}
		return []Delta{{Kind: DeltaError, Text: text}}, nil

	default:
		// response.created、in_progress、content_part.*、output_item.added(thinking) 等忽略：
		// 思考正文由 reasoning_summary_text.delta 与 reasoning_text.delta 带过来。
		return nil, nil
	}
}

// Close 在上游没有发 completed 时补一个收尾，避免客户端一直等。
func (d *responsesStreamDecoder) Close() []Delta {
	if d.finished {
		return nil
	}
	d.finished = true
	return []Delta{{Kind: DeltaStop, StopReason: StopEndTurn}}
}

// indexAt 取 output 序号：上游给了就用它，没给就用 fallback，并推进自增计数。
func (d *responsesStreamDecoder) indexAt(given *int, fallback int) int {
	if given != nil {
		if *given >= d.next {
			d.next = *given + 1
		}
		return *given
	}
	d.next = fallback + 1
	return fallback
}

// responsesStreamErrorText 取失败响应里的错误文本。
func responsesStreamErrorText(resp *responsesResponse) string {
	if resp != nil && resp.Error != nil && resp.Error.Message != "" {
		return resp.Error.Message
	}
	return "上游流式响应失败"
}

// responsesStreamEncoder 把 IR 增量编成 Responses 的 SSE 事件。
type responsesStreamEncoder struct {
	model         string
	id            string
	seq           int
	next          int
	started       bool
	finished      bool
	sentStop      bool
	thinkingIndex int
	textIndex     int
	toolIndex     map[int]int
	// toolBuffers 按 output 序号缓存工具 item 的名称、call_id 与已转发的参数：
	// 收尾要补 function_call_arguments.done 与 output_item.done，并把 output 回填（DESIGN 5.4）。
	toolBuffers map[int]*responsesToolBuffer
	toolOrder   []int
	// toolOpen 记「有工具块开了还没收」，断流时用来补说明。
	toolOpen bool
}

// responsesToolBuffer 是一个正在流式下发的工具 item。
type responsesToolBuffer struct {
	itemID    string
	callID    string
	name      string
	arguments strings.Builder
}

// item 拼出事件里的 item 对象。没有 call_id 的 item 不造 id：上游没给就不编。
func (b *responsesToolBuffer) item(args string) map[string]any {
	item := map[string]any{
		"type":      "function_call",
		"call_id":   b.callID,
		"name":      b.name,
		"arguments": args,
	}
	if b.itemID != "" {
		item["id"] = b.itemID
	}
	return item
}

// doneItem 是带上 status=completed 的 item，用于 output_item.done 与收尾的 output 数组。
func (b *responsesToolBuffer) doneItem(args string) map[string]any {
	item := b.item(args)
	item["status"] = "completed"
	return item
}

// argumentsJSON 给出完整的参数：一路没有参数片段时按空对象，
// 与三条协议的非流式编码器一致，不写空串。
func (b *responsesToolBuffer) argumentsJSON() string {
	if b.arguments.Len() == 0 {
		return "{}"
	}
	return b.arguments.String()
}

// newResponsesStreamEncoder 构造编码器。model 是客户端请求的模型名，会写回事件。
func newResponsesStreamEncoder(model string) *responsesStreamEncoder {
	return &responsesStreamEncoder{
		model:         model,
		id:            "resp_switchboard",
		thinkingIndex: -1,
		textIndex:     -1,
		toolIndex:     map[int]int{},
		toolBuffers:   map[int]*responsesToolBuffer{},
	}
}

// Start 发 response.created。重复调用只发一次。
func (e *responsesStreamEncoder) Start() []SSEEvent {
	if e.started {
		return nil
	}
	e.started = true
	return []SSEEvent{e.event("response.created", map[string]any{
		"response": map[string]any{
			"id":     e.id,
			"object": "response",
			"status": "in_progress",
			"model":  e.model,
			"output": []any{},
		},
	})}
}

// Emit 把一个增量翻成若干 SSE 事件。
func (e *responsesStreamEncoder) Emit(d Delta) []SSEEvent {
	if e.finished || e.sentStop {
		return nil
	}

	events := e.Start()
	switch d.Kind {
	case DeltaThinking:
		// 思考正文单独占一个 output item，正文为空（只有签名）时没有可写的东西。
		if d.Text == "" {
			break
		}
		if e.thinkingIndex < 0 {
			e.thinkingIndex = e.allocIndex()
			events = append(events, e.event("response.output_item.added", map[string]any{
				"output_index": e.thinkingIndex,
				"item":         map[string]any{"type": "reasoning", "summary": []any{}},
			}))
		}
		events = append(events, e.event("response.reasoning_summary_text.delta", map[string]any{
			"output_index":  e.thinkingIndex,
			"summary_index": 0,
			"delta":         d.Text,
		}))

	case DeltaText:
		if e.textIndex < 0 {
			e.textIndex = e.allocIndex()
		}
		events = append(events, e.event("response.output_text.delta", map[string]any{
			"output_index":  e.textIndex,
			"content_index": 0,
			"delta":         d.Text,
		}))

	case DeltaToolStart:
		idx, buf := e.toolBuffer(d.Index)
		buf.callID = d.ToolID
		buf.name = d.ToolName
		if d.ToolID != "" {
			buf.itemID = "fc_" + strings.TrimPrefix(d.ToolID, "fc_")
		}
		e.toolOpen = true
		events = append(events, e.event("response.output_item.added", map[string]any{
			"output_index": idx,
			"item":         buf.item(""),
		}))

	case DeltaToolArgs:
		idx, buf := e.toolBuffer(d.Index)
		buf.arguments.WriteString(d.ArgsJSON)
		e.toolOpen = true
		events = append(events, e.event("response.function_call_arguments.delta", map[string]any{
			"output_index": idx,
			"delta":        d.ArgsJSON,
		}))

	case DeltaStop:
		e.toolOpen = false
		events = append(events, e.completed(d.StopReason, d.Usage)...)

	case DeltaError:
		// 先发一条 error 事件说明出了什么事，再发 response.failed 收尾：responses 的客户端
		// 认终态事件，把一次失败报成 completed 会与请求记录里的 ok=0 自相矛盾。
		text := withIncompleteToolArgs(d.Text, e.toolOpen)
		events = append(events, e.event("error", map[string]any{
			"message": text,
		}))
		events = append(events, e.failed(text)...)
	}
	return events
}

// Finish 补齐收尾。已经发过 completed 或 response.failed 时不再重复。
func (e *responsesStreamEncoder) Finish() []SSEEvent {
	if e.finished {
		return nil
	}
	e.finished = true

	events := e.Start()
	events = append(events, e.completed(StopEndTurn, nil)...)
	return events
}

// completed 发 response.completed，带上状态与用量。
// 发之前把每个工具 item 的 arguments.done 与 output_item.done 补齐，并把完整的
// function_call 回填进 output：只发 added 与 delta 的流会让客户端拿不到收尾。
func (e *responsesStreamEncoder) completed(reason string, usage *Usage) []SSEEvent {
	if e.sentStop {
		return nil
	}
	e.sentStop = true
	e.toolOpen = false

	events := e.toolDoneEvents()

	status := "completed"
	resp := map[string]any{
		"id":     e.id,
		"object": "response",
		"status": status,
		"model":  e.model,
		"output": e.toolOutputItems(),
		"usage":  responsesUsageOut{},
	}
	if reason == StopMaxTokens {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = &responsesIncomplete{Reason: "max_output_tokens"}
	}
	if usage != nil {
		resp["usage"] = responsesUsageOut{
			InputTokens:  usage.In,
			OutputTokens: usage.Out,
			TotalTokens:  usage.In + usage.Out,
		}
	}
	events = append(events, e.event("response.completed", map[string]any{"response": resp}))
	return events
}

// toolDoneEvents 给每个工具 item 补 arguments.done 与 output_item.done，按 output 序号顺序发。
func (e *responsesStreamEncoder) toolDoneEvents() []SSEEvent {
	var events []SSEEvent
	for _, idx := range e.toolOrder {
		buf := e.toolBuffers[idx]
		if buf == nil {
			continue
		}
		args := buf.argumentsJSON()
		events = append(events, e.event("response.function_call_arguments.done", map[string]any{
			"output_index": idx,
			"item_id":      buf.itemID,
			"name":         buf.name,
			"arguments":    args,
		}))
		events = append(events, e.event("response.output_item.done", map[string]any{
			"output_index": idx,
			"item":         buf.doneItem(args),
		}))
	}
	return events
}

// toolOutputItems 拼出收尾事件里的 output 数组，只含工具 item：文本与思考 item 的正文
// 已经按增量发过，这里不攒，输出数组因此只列 function_call（DESIGN 5.4 已知边界）。
func (e *responsesStreamEncoder) toolOutputItems() []any {
	out := []any{}
	for _, idx := range e.toolOrder {
		buf := e.toolBuffers[idx]
		if buf == nil {
			continue
		}
		out = append(out, buf.doneItem(buf.argumentsJSON()))
	}
	return out
}

// toolBuffer 取某个上游块号对应的工具缓冲；没有就新开一个（先来参数后到开始也算）。
func (e *responsesStreamEncoder) toolBuffer(upstream int) (int, *responsesToolBuffer) {
	if idx, ok := e.toolIndex[upstream]; ok {
		if buf := e.toolBuffers[idx]; buf != nil {
			return idx, buf
		}
	}
	idx := e.allocIndex()
	e.toolIndex[upstream] = idx
	buf := &responsesToolBuffer{}
	e.toolBuffers[idx] = buf
	e.toolOrder = append(e.toolOrder, idx)
	return idx, buf
}

// failed 发 response.failed：上游断流或上游自己报了错误时，客户端得到的终态是失败。
// 形状与直通路径补的那一帧一致（status=failed 加 server_error），两条路径对断流的结论因此相同。
func (e *responsesStreamEncoder) failed(message string) []SSEEvent {
	if e.sentStop {
		return nil
	}
	e.sentStop = true
	e.finished = true

	resp := map[string]any{
		"id":     e.id,
		"object": "response",
		"status": "failed",
		"model":  e.model,
		"output": []any{},
		"error":  map[string]string{"code": "server_error", "message": message},
	}
	return []SSEEvent{e.event("response.failed", map[string]any{"response": resp})}
}

// allocIndex 分配下一个 output 序号。
func (e *responsesStreamEncoder) allocIndex() int {
	idx := e.next
	e.next++
	return idx
}

// event 构造一个事件，data 里的 type 与事件名一致，并带上自增序号。
func (e *responsesStreamEncoder) event(name string, fields map[string]any) SSEEvent {
	body := map[string]any{
		"type":            name,
		"sequence_number": e.seq,
	}
	e.seq++
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		b = []byte(`{"type":"error"}`)
	}
	return SSEEvent{Event: name, Data: b}
}

// responsesUsageScanner 从 Responses 的响应里读出用量，不改动转发内容。
// 流式帧读收尾事件的嵌套 usage，非流式响应体读顶层 usage（探活走这条）。
type responsesUsageScanner struct {
	in, out, cacheRead int64
}

// Feed 送入一条 data 帧或一份完整响应体。
func (s *responsesUsageScanner) Feed(data []byte) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}

	var env struct {
		responsesStreamEvent
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return
	}
	switch env.Type {
	case "response.completed", "response.incomplete":
		if env.Response == nil || env.Response.Usage == nil {
			return
		}
		s.set(env.Response.Usage)
	case "":
		// 非流式响应体没有事件名，usage 在顶层。
		if env.Usage != nil {
			s.set(env.Usage)
		}
	}
}

func (s *responsesUsageScanner) set(u *responsesUsage) {
	s.in = u.InputTokens
	s.out = u.OutputTokens
	s.cacheRead = 0
	if u.InputTokensDetails != nil {
		s.cacheRead = u.InputTokensDetails.CachedTokens
	}
}

// Usage 返回读到的用量，读不到时为 0。
func (s *responsesUsageScanner) Usage() (in, out, cacheRead, cacheWrite int64) {
	return s.in, s.out, s.cacheRead, 0
}
