package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// 响应模式。
const (
	respModePassthrough = "passthrough"
	respModeJSONPointer = "json_pointer"
)

// 求值上限。方言是用户数据，一条写坏的映射不该吃光宿主内存。
const (
	declMaxDepth = 32
	declMaxBytes = 1 << 20
)

// CompileDialect 把声明式方言编成 Dialect；内置 Go 方言由注册表直接登记，不走这里。
func CompileDialect(decl DialectDecl) (Dialect, error) {
	if decl.Implementation.Prepare != ImplDeclarative || decl.Implementation.Decode != ImplDeclarative {
		return nil, fmt.Errorf("方言 %q 的 implementation.prepare 与 implementation.decode 必须是 %q：Go 实现的内置方言由注册表登记", decl.ID, ImplDeclarative)
	}
	switch {
	case decl.ID == "":
		return nil, errors.New("方言缺少 id")
	case decl.Operation == "":
		return nil, fmt.Errorf("方言 %q 缺少 operation", decl.ID)
	case decl.UpstreamWire == "":
		return nil, fmt.Errorf("方言 %q 缺少 upstream_wire", decl.ID)
	}
	return &declarativeDialect{decl: decl}, nil
}

type declarativeDialect struct {
	decl DialectDecl
}

func (d *declarativeDialect) ID() string           { return d.decl.ID }
func (d *declarativeDialect) Operation() string    { return d.decl.Operation }
func (d *declarativeDialect) UpstreamWire() string { return d.decl.UpstreamWire }

// Prepare 把语义输入编成上游 JSON 请求体。
func (d *declarativeDialect) Prepare(in Input) (Plan, error) {
	plan := Plan{JSON: map[string]any{}}
	if d.decl.Request == nil {
		return plan, nil
	}
	body := make(map[string]any, len(d.decl.Request.Body))
	for _, key := range slices.Sorted(maps.Keys(d.decl.Request.Body)) {
		value, present, err := EvalValue(d.decl.Request.Body[key], in)
		if err != nil {
			return Plan{}, fmt.Errorf("方言 %s 的请求字段 %q：%w", d.decl.ID, key, err)
		}
		if present {
			body[key] = value
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Plan{}, fmt.Errorf("方言 %s 的请求体不能序列化成 JSON：%w", d.decl.ID, err)
	}
	if len(encoded) > declMaxBytes {
		return Plan{}, fmt.Errorf("方言 %s 求值后的请求体有 %d 字节，超过 %d 字节（1 MiB）上限", d.decl.ID, len(encoded), declMaxBytes)
	}
	plan.JSON = body
	plan.Path = d.decl.Request.Path
	return plan, nil
}

// EvalValue 求值一个声明式映射节点。present 为假表示 omit_if_empty 判定为空，该字段不写入请求体。
func EvalValue(v Value, in Input) (any, bool, error) {
	return evalValue(v, in, 0)
}

func evalValue(v Value, in Input, depth int) (any, bool, error) {
	if depth > declMaxDepth {
		return nil, false, fmt.Errorf("声明式映射的嵌套超过 %d 层", declMaxDepth)
	}
	switch {
	case v.IsNode():
		return evalNode(v, in, depth)
	case v.Object != nil:
		object := make(map[string]any, len(v.Object))
		for _, key := range slices.Sorted(maps.Keys(v.Object)) {
			value, present, err := evalValue(v.Object[key], in, depth+1)
			if err != nil {
				return nil, false, fmt.Errorf("字段 %q：%w", key, err)
			}
			if present {
				object[key] = value
			}
		}
		return object, true, nil
	case v.Array != nil:
		array := make([]any, 0, len(v.Array))
		for index, item := range v.Array {
			value, present, err := evalValue(item, in, depth+1)
			if err != nil {
				return nil, false, fmt.Errorf("下标 %d：%w", index, err)
			}
			if present {
				array = append(array, value)
			}
		}
		return array, true, nil
	}
	return v.Scalar, true, nil
}

func evalNode(v Value, in Input, depth int) (any, bool, error) {
	value, hasSource, err := nodeSource(v, in)
	if err != nil {
		return nil, false, err
	}
	if !hasSource {
		// 只有修饰键没有来源键：按普通值求值。解析层会拒绝这种声明，这里只是兜底。
		plain := Value{Scalar: v.Scalar, Object: v.Object, Array: v.Array}
		if value, _, err = evalValue(plain, in, depth); err != nil {
			return nil, false, err
		}
	}
	switch {
	case v.Map != nil:
		if value, err = lookupMap(v, value); err != nil {
			return nil, false, err
		}
	case v.HasDefault && declEmpty(value):
		value = v.Default
	}
	if v.OmitIfEmpty && declEmpty(value) {
		return nil, false, nil
	}
	return value, true, nil
}

// nodeSource 取节点的来源值。hasSource 为假表示这个 Value 没有任何来源键。
func nodeSource(v Value, in Input) (any, bool, error) {
	switch {
	case v.HasConst:
		return v.Const, true, nil
	case v.Input != "":
		value, ok := inputSource(in, v.Input)
		if !ok {
			return nil, false, fmt.Errorf("input 不认识字段 %q，可用的是 chars、format、instructions、language、model、speed、text、voice", v.Input)
		}
		return value, true, nil
	case v.Model != "":
		return in.Model, true, nil
	case v.Option != "":
		// 配置项不存在按空值处理，和空串一样交给 default 与 omit_if_empty。
		return in.Options[v.Option], true, nil
	}
	return nil, false, nil
}

func inputSource(in Input, name string) (any, bool) {
	switch name {
	case "text":
		return in.Text, true
	case "voice":
		return in.Voice, true
	case "format":
		return in.Format, true
	case "instructions":
		return in.Instructions, true
	case "language":
		return in.Language, true
	case "model":
		return in.Model, true
	case "chars":
		return in.Chars, true
	case "speed":
		if in.Speed == nil {
			return nil, true
		}
		return *in.Speed, true
	}
	return nil, false
}

// lookupMap 用来源取值的字符串形式查表。查不到就回落 default，都没有就报错：
// 把未知取值静默发给上游，故障会挪到更难定位的地方。
func lookupMap(v Value, source any) (any, error) {
	key, textable := declText(source)
	if textable {
		if mapped, hit := v.Map[key]; hit {
			return mapped, nil
		}
	}
	if v.HasDefault {
		return v.Default, nil
	}
	if !textable {
		return nil, errors.New("map 的来源取值不是标量，无法查表")
	}
	return nil, fmt.Errorf("取值 %q 不在方言声明的映射里，可用的是 %s", key, strings.Join(slices.Sorted(maps.Keys(v.Map)), "、"))
}

// declText 给出标量的字符串形式，供 map 查表用。
func declText(value any) (string, bool) {
	switch x := value.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

// declEmpty 只把 nil、空串、空数组、空对象算空；false 与 0 是有效取值。
func declEmpty(value any) bool {
	switch x := value.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// Decode 把上游响应当成语义结果。
func (d *declarativeDialect) Decode(in Input, resp Response) (Result, error) {
	decl := d.decl.Response
	if decl == nil {
		return Result{}, fmt.Errorf("方言 %s 没有 response 声明，无法解码", d.decl.ID)
	}
	if decl.Mode != respModePassthrough && decl.Mode != respModeJSONPointer {
		return Result{}, fmt.Errorf("方言 %s 的 response.mode %q 不认识，可用的是 passthrough 与 json_pointer", d.decl.ID, decl.Mode)
	}
	mime, err := d.decodeMIME(in)
	if err != nil {
		return Result{}, err
	}

	if decl.Mode == respModePassthrough && d.decl.Transport.Response == "binary" {
		if decl.Usage != nil {
			return Result{}, fmt.Errorf("方言 %s 的响应是二进制，usage 指针只能读 JSON 响应", d.decl.ID)
		}
		return Result{Kind: ResultMedia, Media: resp.Body, MIME: mime}, nil
	}

	var doc any
	if decl.Mode == respModeJSONPointer || decl.Usage != nil {
		if doc, err = decodeJSONBody(resp.Body); err != nil {
			return Result{}, err
		}
	}
	result := Result{Kind: ResultJSON, JSON: resp.Body}
	if decl.Mode == respModeJSONPointer {
		if result, err = decodePointerResult(decl, doc, mime); err != nil {
			return Result{}, err
		}
	}
	if decl.Usage != nil {
		usage, err := extractUsage(decl.Usage, doc)
		if err != nil {
			return Result{}, err
		}
		result.Usage = usage
	}
	return result, nil
}

func (d *declarativeDialect) decodeMIME(in Input) (string, error) {
	value, present, err := EvalValue(d.decl.Response.MIME, in)
	if err != nil {
		return "", fmt.Errorf("方言 %s 的 response.mime 求值失败：%w", d.decl.ID, err)
	}
	if !present || value == nil {
		return "", nil
	}
	text, ok := declText(value)
	if !ok {
		return "", fmt.Errorf("方言 %s 的 response.mime 求值结果不是字符串", d.decl.ID)
	}
	return text, nil
}

// decodeJSONBody 保留数字精度：json.Number 重新编码后不会变成 float64 的近似值。
func decodeJSONBody(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON，与方言声明的响应形状不符：%w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("上游响应在 JSON 之后还有多余内容，与方言声明的响应形状不符")
	}
	return doc, nil
}

func decodePointerResult(decl *ResponseDecl, doc any, mime string) (Result, error) {
	value, ok := jsonPointerValue(doc, decl.Pointer)
	if !ok {
		return Result{}, fmt.Errorf("上游响应里没有 %q 指向的字段，与方言声明的响应形状不符", decl.Pointer)
	}
	switch decl.Encoding {
	case "base64":
		text, ok := value.(string)
		if !ok {
			return Result{}, fmt.Errorf("上游响应 %q 处不是字符串，无法按 base64 解码", decl.Pointer)
		}
		media, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return Result{}, fmt.Errorf("上游响应 %q 处的 base64 不合法：%w", decl.Pointer, err)
		}
		return Result{Kind: ResultMedia, Media: media, MIME: mime}, nil
	case "text":
		text, ok := value.(string)
		if !ok {
			return Result{}, fmt.Errorf("上游响应 %q 处不是字符串，无法按 text 解码", decl.Pointer)
		}
		return Result{Kind: ResultText, Text: text}, nil
	case "json":
		encoded, err := json.Marshal(value)
		if err != nil {
			return Result{}, fmt.Errorf("上游响应 %q 处不能重新编码成 JSON：%w", decl.Pointer, err)
		}
		return Result{Kind: ResultJSON, JSON: encoded}, nil
	}
	return Result{}, fmt.Errorf("response.encoding %q 不认识，可用的是 base64、text、json", decl.Encoding)
}

// jsonPointerValue 按 RFC 6901 取字段。
func jsonPointerValue(doc any, pointer string) (any, bool) {
	if pointer == "" {
		return doc, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	current := doc
	for _, token := range strings.Split(pointer[1:], "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[token]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) || token != strconv.Itoa(index) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// extractUsage 按声明把上游用量字段填进规范字段；指针为空或取不到的字段记 0。
func extractUsage(decl *UsageDecl, doc any) (Usage, error) {
	usage := Usage{Source: "upstream"}
	fields := []struct {
		pointer string
		target  *int64
	}{
		{decl.InputTokens, &usage.In},
		{decl.OutputTokens, &usage.Out},
		{decl.CachedInputTokens, &usage.CacheRead},
		{decl.CacheWriteTokens, &usage.CacheWrite},
		{decl.ReasoningTokens, &usage.Reasoning},
		{decl.TotalTokens, &usage.Total},
		{decl.AudioSeconds, &usage.AudioSeconds},
		{decl.TTSCharacters, &usage.TTSChars},
	}
	for _, field := range fields {
		if field.pointer == "" {
			continue
		}
		value, ok := jsonPointerValue(doc, field.pointer)
		if !ok {
			continue
		}
		number, err := usageNumber(field.pointer, value)
		if err != nil {
			return Usage{}, err
		}
		*field.target = number
	}
	return usage, nil
}

// usageNumber 认数字与数字字符串，四舍五入到整数；负值、NaN、Inf 是上游数据错误。
func usageNumber(pointer string, value any) (int64, error) {
	var number float64
	switch x := value.(type) {
	case json.Number:
		parsed, err := x.Float64()
		if err != nil {
			return 0, fmt.Errorf("上游用量 %q 不是数字：%w", pointer, err)
		}
		number = parsed
	case float64:
		number = x
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("上游用量 %q 的取值 %q 不是数字", pointer, x)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("上游用量 %q 不是数字", pointer)
	}
	switch {
	case math.IsNaN(number), math.IsInf(number, 0):
		return 0, fmt.Errorf("上游用量 %q 不是有限数字", pointer)
	case number < 0:
		return 0, fmt.Errorf("上游用量 %q 是负数", pointer)
	case number >= math.MaxInt64:
		return 0, fmt.Errorf("上游用量 %q 超出整数范围", pointer)
	}
	return int64(number + 0.5), nil
}
