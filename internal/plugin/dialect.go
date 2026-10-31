package plugin

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
)

// LoadDialectFile 读 dialects.json 并校验。file 只用于报错。
func LoadDialectFile(file string) (DialectFile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return DialectFile{}, fmt.Errorf("读方言文件 %s 失败：%w", file, err)
	}
	return DecodeDialectFile(file, data)
}

// DecodeDialectFile 严格解析并校验方言文件。
func DecodeDialectFile(file string, data []byte) (DialectFile, error) {
	var f DialectFile
	if err := decodeStrictJSON(data, &f); err != nil {
		return DialectFile{}, decodeFailure(file, "方言文件不接受未知字段，自定义内容请放进 extensions", err)
	}
	if err := ValidateDialectFile(f); err != nil {
		return DialectFile{}, fmt.Errorf("%s: %w", file, err)
	}
	return f, nil
}

// wireOperations 是第一版放行的 wire × operation 组合。
//
// speech.* 两条除自己的语音线协议外，也允许 openai-chat：有些上游把 TTS/ASR 做成 chat
// completions 的一种形状（MiMo），方言是这条形状的唯一描述，语音入站仍然只归 speech 操作。
var wireOperations = []struct{ wire, operation string }{
	{WireAnthropic, OpChatComplete},
	{WireOpenAIChat, OpChatComplete},
	{WireOpenAIResponses, OpChatComplete},
	{WireGemini, OpChatComplete},
	{WireEmbeddings, OpEmbeddingsCreate},
	{WireSpeech, OpSpeechSynthesize},
	{WireOpenAIChat, OpSpeechSynthesize},
	{WireTranscriptions, OpSpeechTranscribe},
	{WireOpenAIChat, OpSpeechTranscribe},
	{WireSystemOne, OpSystemDecide},
}

func knownWireNames() []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(wireOperations))
	for _, pair := range wireOperations {
		if seen[pair.wire] {
			continue
		}
		seen[pair.wire] = true
		names = append(names, pair.wire)
	}
	return names
}

// pairAllowed 查 wire × operation 是不是第一版放行的组合。
func pairAllowed(wire, operation string) bool {
	for _, pair := range wireOperations {
		if pair.wire == wire && pair.operation == operation {
			return true
		}
	}
	return false
}

func wirePairNames() []string {
	pairs := make([]string, 0, len(wireOperations))
	for _, pair := range wireOperations {
		pairs = append(pairs, pair.wire+" → "+pair.operation)
	}
	return pairs
}

// ValidateDialectFile 校验一个方言文件：schema、非空、同文件内 ID 唯一。
func ValidateDialectFile(f DialectFile) error {
	if f.Schema != DialectSchema {
		return fmt.Errorf("schema 是 %q：方言文件必须是 %q", f.Schema, DialectSchema)
	}
	if len(f.Dialects) == 0 {
		return errors.New("dialects 为空：方言文件至少要有一条方言，模型目录里的 dialect 指向它")
	}
	seen := make(map[string]int, len(f.Dialects))
	for i, decl := range f.Dialects {
		if err := ValidateDialectDecl(decl); err != nil {
			return fmt.Errorf("dialects[%d]: %w", i, err)
		}
		if prev, dup := seen[decl.ID]; dup {
			return fmt.Errorf("dialects[%d].id 与 dialects[%d].id 都是 %q：同一个文件里方言 ID 不能重复", i, prev, decl.ID)
		}
		seen[decl.ID] = i
	}
	return nil
}

// ValidateDialectDecl 校验一条方言。内置方言也过这里，避免 system 方言绕过插件方言的规则。
func ValidateDialectDecl(decl DialectDecl) error {
	if decl.Schema != "" && decl.Schema != DialectSchema {
		return fmt.Errorf("schema 是 %q：只能是空或 %q", decl.Schema, DialectSchema)
	}
	if err := validateDialectID(decl.ID); err != nil {
		return err
	}
	if !KnownOperation(decl.Operation) {
		return fmt.Errorf("operation 是 %q：宿主认识的操作是 %s", decl.Operation, strings.Join(supportedOperationNames, "、"))
	}
	if !KnownWire(decl.UpstreamWire) {
		return fmt.Errorf("upstream_wire 是 %q：宿主认识的线协议是 %s", decl.UpstreamWire, strings.Join(knownWireNames(), "、"))
	}
	if !pairAllowed(decl.UpstreamWire, decl.Operation) {
		return fmt.Errorf("upstream_wire %s 不承载 %s：允许的组合是 %s",
			decl.UpstreamWire, decl.Operation, strings.Join(wirePairNames(), "、"))
	}

	switch decl.Transport.Request {
	case "", "json":
	default:
		return fmt.Errorf("transport.request 是 %q：第一版只有 json 请求体", decl.Transport.Request)
	}
	switch decl.Transport.Response {
	case "", "json", "binary":
	default:
		return fmt.Errorf("transport.response 是 %q：只能是空、json 或 binary", decl.Transport.Response)
	}
	switch decl.Transport.Streaming {
	case "", "none", "bytes", "sse", "jsonl":
	default:
		return fmt.Errorf("transport.streaming 是 %q：只能是空、none、bytes、sse 或 jsonl", decl.Transport.Streaming)
	}

	// 一种准备方式、一种解码方式：同一个方言混写 hook 与声明式，出故障时不知道走的是哪条路。
	switch decl.Implementation.Prepare {
	case ImplHook:
		if decl.Request != nil {
			return errors.New("implementation.prepare 是 hook，request 却存在：删掉 request，或把 prepare 改成 declarative")
		}
	case ImplDeclarative:
		if decl.Request == nil {
			return errors.New("implementation.prepare 是 declarative，却缺 request：声明式要写 method、path 与 body")
		}
		if err := validateRequestDecl(*decl.Request); err != nil {
			return err
		}
	default:
		return fmt.Errorf("implementation.prepare 是 %q：只能是 %s 或 %s", decl.Implementation.Prepare, ImplHook, ImplDeclarative)
	}
	switch decl.Implementation.Decode {
	case ImplHook:
		if decl.Response != nil {
			return errors.New("implementation.decode 是 hook，response 却存在：删掉 response，或把 decode 改成 declarative")
		}
	case ImplDeclarative:
		if decl.Response == nil {
			return errors.New("implementation.decode 是 declarative，却缺 response：声明式要写 mode，json_pointer 模式还要 pointer 与 encoding")
		}
		if err := validateResponseDecl(*decl.Response); err != nil {
			return err
		}
	default:
		return fmt.Errorf("implementation.decode 是 %q：只能是 %s 或 %s", decl.Implementation.Decode, ImplHook, ImplDeclarative)
	}

	if err := checkLimit("limits.max_input_chars", decl.Limits.MaxInputChars); err != nil {
		return err
	}
	if err := checkLimit("limits.max_input_bytes", decl.Limits.MaxInputBytes); err != nil {
		return err
	}
	return checkLimit("limits.max_response_bytes", decl.Limits.MaxResponseBytes)
}

// validateDialectID 要求 <前缀>/<名称>@<主版本>。lax/ 前缀是内置方言的，插件冒用会让
// 内置行为被静默替换；主版本号写在 ID 里，升级就只能新建一条。
func validateDialectID(id string) error {
	if id == "" {
		return errors.New("id 为空：方言要有全局唯一 ID，形如 com.example.mimo/mimo-chat-speech@1")
	}
	if strings.Count(id, "/") != 1 {
		return fmt.Errorf("id 是 %q：必须是 <前缀>/<名称>@<主版本>，只含一个斜杠", id)
	}
	if strings.Count(id, "@") != 1 {
		return fmt.Errorf("id 是 %q：必须以 @<主版本> 结尾，例如 @1", id)
	}
	prefix, rest, _ := strings.Cut(id, "/")
	if prefix == "lax" {
		return fmt.Errorf("id 是 %q：lax/ 前缀保留给随二进制发布的内置方言，插件用自己的反向域名前缀", id)
	}
	if prefix == "" {
		return fmt.Errorf("id 是 %q：斜杠前面要有前缀，例如 com.example.mimo", id)
	}
	at := strings.Index(rest, "@")
	name, major := rest[:at], rest[at+1:]
	if name == "" {
		return fmt.Errorf("id 是 %q：斜杠与 @ 之间要有名称", id)
	}
	if strings.TrimLeft(major, "0123456789") != "" || major == "" {
		return fmt.Errorf("id 是 %q：@ 后面要是十进制主版本号，例如 @1", id)
	}
	return nil
}

func checkLimit(field string, value int64) error {
	if value < 0 {
		return fmt.Errorf("%s 是 %d：上限不能为负，0 表示不限", field, value)
	}
	return nil
}

func validateRequestDecl(req RequestDecl) error {
	switch req.Method {
	case "", "POST":
	default:
		return fmt.Errorf("request.method 是 %q：第一版只发 POST", req.Method)
	}
	if req.Path == "" {
		return errors.New("request.path 为空：写厂商的相对路径，例如 /v1/audio/speech")
	}
	if !strings.HasPrefix(req.Path, "/") {
		return fmt.Errorf("request.path 是 %q：必须以 / 开头", req.Path)
	}
	// path.Clean 保持原样才算干净：拦下 .. 与 //host 这类相对协议引用，
	// 宿主把它拼到 base_url 上，拼接处不能再做判断。
	if path.Clean(req.Path) != req.Path {
		return fmt.Errorf("request.path 是 %q：不能含 .、.. 或重复的斜杠，宿主按原样拼接厂商地址", req.Path)
	}
	if len(req.Body) == 0 {
		return errors.New("request.body 为空：声明式请求至少要有一个字段")
	}
	if _, ok := req.Body[""]; ok {
		return errors.New("request.body 里有空键：请求体字段要有名字")
	}
	for _, key := range slices.Sorted(maps.Keys(req.Body)) {
		if err := validateValue("request.body."+key, req.Body[key]); err != nil {
			return err
		}
	}
	return nil
}

func validateResponseDecl(resp ResponseDecl) error {
	switch resp.Mode {
	case respModePassthrough:
		if resp.Pointer != "" || resp.Encoding != "" {
			return errors.New("response.mode 是 passthrough，pointer 或 encoding 却非空：passthrough 不取字段，删掉它们")
		}
	case respModeJSONPointer:
		if err := checkJSONPointer("response.pointer", resp.Pointer); err != nil {
			return err
		}
		switch resp.Encoding {
		case "base64", "text", "json":
		default:
			return fmt.Errorf("response.encoding 是 %q：json_pointer 要写 base64、text 或 json", resp.Encoding)
		}
	default:
		return fmt.Errorf("response.mode 是 %q：只能是 %s 或 %s", resp.Mode, respModePassthrough, respModeJSONPointer)
	}
	if resp.Usage == nil {
		return nil
	}
	for _, field := range []struct{ name, pointer string }{
		{"input_tokens", resp.Usage.InputTokens},
		{"output_tokens", resp.Usage.OutputTokens},
		{"cached_input_tokens", resp.Usage.CachedInputTokens},
		{"cache_write_tokens", resp.Usage.CacheWriteTokens},
		{"reasoning_tokens", resp.Usage.ReasoningTokens},
		{"total_tokens", resp.Usage.TotalTokens},
		{"audio_seconds", resp.Usage.AudioSeconds},
		{"tts_characters", resp.Usage.TTSCharacters},
	} {
		if field.pointer == "" {
			continue
		}
		if err := checkJSONPointer("response.usage."+field.name, field.pointer); err != nil {
			return err
		}
	}
	return nil
}

// checkJSONPointer 按 RFC 6901 判指针：/ 开头，~ 只能写成 ~0 或 ~1。
func checkJSONPointer(field, pointer string) error {
	if pointer == "" {
		return fmt.Errorf("%s 为空：写一个 / 开头的路径，例如 /data", field)
	}
	if !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("%s 是 %q：必须以 / 开头", field, pointer)
	}
	for i := 0; i < len(pointer); i++ {
		if pointer[i] != '~' {
			continue
		}
		if i+1 >= len(pointer) || (pointer[i+1] != '0' && pointer[i+1] != '1') {
			return fmt.Errorf("%s 是 %q：RFC 6901 里 ~ 只能写成 ~0 或 ~1，第 %d 字节不合规", field, pointer, i+1)
		}
	}
	return nil
}

// declarativeInputFields 是声明式映射能读的输入字段，与 Input 的语义字段一一对应。
// 白名单而不是反射：映射能读到的每个字段都要有明确的语义与校验。
var declarativeInputFields = []string{"text", "voice", "format", "instructions", "language", "speed", "model", "chars"}

// validateValue 递归校验声明式值。节点选项的形状已由 Value 的解析拦过，这里只补语义：
// 输入字段名与 model 字段名必须属于第一版认识的那几个。
func validateValue(field string, v Value) error {
	if v.IsNode() {
		if v.Input != "" && !slices.Contains(declarativeInputFields, v.Input) {
			return fmt.Errorf("%s.input 是 %q：声明式映射只能读 %s", field, v.Input, strings.Join(declarativeInputFields, "、"))
		}
		if v.Model != "" && v.Model != "upstream_id" {
			return fmt.Errorf("%s.model 是 %q：第一版只能读 upstream_id", field, v.Model)
		}
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(v.Object)) {
		if key == "" {
			return fmt.Errorf("%s 里有空键：对象字段要有名字", field)
		}
		if err := validateValue(field+"."+key, v.Object[key]); err != nil {
			return err
		}
	}
	for i, child := range v.Array {
		if err := validateValue(fmt.Sprintf("%s[%d]", field, i), child); err != nil {
			return err
		}
	}
	return nil
}
