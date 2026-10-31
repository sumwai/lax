package plugin

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode"
)

// LoadCatalog 读 models.json 并校验。file 只用于报错。
func LoadCatalog(file string) (CatalogFile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return CatalogFile{}, fmt.Errorf("读模型目录 %s 失败：%w", file, err)
	}
	return DecodeCatalog(file, data)
}

// DecodeCatalog 严格解析并校验模型目录。
func DecodeCatalog(file string, data []byte) (CatalogFile, error) {
	var f CatalogFile
	if err := decodeStrictJSON(data, &f); err != nil {
		return CatalogFile{}, decodeFailure(file, "模型目录不接受未知字段", err)
	}
	if err := ValidateCatalog(f); err != nil {
		return CatalogFile{}, fmt.Errorf("%s: %w", file, err)
	}
	return f, nil
}

// ValidateCatalog 校验模型目录。能力之间不做交叉推断：supported 为 false 时不再要求
// streaming、formats 之类，缺声明与声明不支持是两件事。
func ValidateCatalog(f CatalogFile) error {
	if len(f.Models) == 0 {
		return errors.New("models 为空：模型目录至少要声明一个模型，否则装上去也没有可用模型")
	}
	seen := make(map[string]int, len(f.Models))
	for i, m := range f.Models {
		field := fmt.Sprintf("models[%d]", i)
		if strings.TrimSpace(m.Model) == "" {
			return fmt.Errorf("%s.model 为空：模型名是客户端写的那一段，例如 mimo/mimo-v2.5-tts", field)
		}
		if strings.IndexFunc(m.Model, unicode.IsSpace) >= 0 {
			return fmt.Errorf("%s.model 是 %q：模型名里不能有空白", field, m.Model)
		}
		if prev, dup := seen[m.Model]; dup {
			return fmt.Errorf("%s.model 与 models[%d].model 都是 %q：同一模型同一 operation 出现两个绑定是安装错误，不能靠顺序覆盖",
				field, prev, m.Model)
		}
		seen[m.Model] = i
		if m.UpstreamID == "" {
			return fmt.Errorf("%s.upstream_id 为空：上游要它自己的模型名，例如 deepseek-ai/DeepSeek-V4", field)
		}
		if len(m.Operations) == 0 {
			return fmt.Errorf("%s.operations 为空：模型至少要绑定一个 operation 的方言", field)
		}
		for _, op := range slices.Sorted(maps.Keys(m.Operations)) {
			if !KnownOperation(op) {
				return fmt.Errorf("%s.operations 里有 %q：宿主认识的操作是 %s", field, op, strings.Join(supportedOperationNames, "、"))
			}
			decl := m.Operations[op]
			if decl.Dialect == "" {
				return fmt.Errorf("%s.operations[%s].dialect 为空：每个 operation 要指向一条方言", field, op)
			}
			if decl.Capabilities == nil {
				continue
			}
			if err := validateCapabilities(fmt.Sprintf("%s.operations[%s].capabilities", field, op), *decl.Capabilities); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCapabilities(field string, cap Capabilities) error {
	for _, item := range []struct {
		name  string
		value *Tri
	}{
		{"supported", cap.Supported},
		{"streaming", cap.Streaming},
		{"tools", cap.Tools},
		{"reasoning", cap.Reasoning},
		{"json_mode", cap.JSONMode},
		{"vision", cap.Vision},
	} {
		if err := validateTri(field+"."+item.name, item.value); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		name  string
		value *TriList
	}{
		{"formats", cap.Formats},
		{"voices", cap.Voices},
	} {
		if item.value == nil {
			continue
		}
		for i, text := range item.value.Values {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("%s.%s[%d] 是空的：删掉这一项，或写具体的取值；空数组才表示一个都没有",
					field, item.name, i)
			}
		}
	}
	return nil
}

// validateTri 兜住手写结构体绕过 UnmarshalJSON 的情形：注册表与导入器也会直接拼 Capabilities。
func validateTri(field string, t *Tri) error {
	if t == nil {
		return nil
	}
	switch t.Value {
	case "true", "false", "unknown":
		return nil
	}
	return fmt.Errorf("%s 是 %q：能力值只能是 true、false 或 \"unknown\"", field, t.Value)
}
