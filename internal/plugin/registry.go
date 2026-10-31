package plugin

import (
	"fmt"
	"slices"
	"strings"
)

// Registry 是宿主认识的方言集合：内置方言加已安装插件编出来的方言。
//
// 注册是整包的：一个插件的方言要么全进去，要么一个都不进，避免半个插件生效。
type Registry struct {
	dialects map[string]Dialect
}

// NewRegistry 返回空注册表。
func NewRegistry() *Registry {
	return &Registry{dialects: map[string]Dialect{}}
}

// DefaultRegistry 只装内置方言，供没有插件目录的场景（测试、单次命令）用。
func DefaultRegistry() *Registry {
	r := NewRegistry()
	for _, d := range SystemDialects() {
		// 内置方言的 ID 由常量给出，冲突只可能是代码写错，忽略错误即可。
		_ = r.RegisterSystem(d)
	}
	return r
}

// RegisterSystem 登记一个内置方言。ID 必须用 lax/ 前缀，重复登记报错。
func (r *Registry) RegisterSystem(d Dialect) error {
	if d == nil {
		return fmt.Errorf("内置方言不能是 nil")
	}
	if !strings.HasPrefix(d.ID(), "lax/") {
		return fmt.Errorf("内置方言 %q 必须用 lax/ 前缀：lax/ 保留给随二进制发布的方言", d.ID())
	}
	return r.register(d)
}

// RegisterPluginDialect 登记一个插件方言。ID 必须以 <插件 ID>/ 开头，
// 这样插件的方言归属能只看 ID 判断，删除插件时也就能精确找到要清掉的绑定。
func (r *Registry) RegisterPluginDialect(pluginID string, d Dialect) error {
	if d == nil {
		return fmt.Errorf("插件 %s 的方言不能是 nil", pluginID)
	}
	if err := checkPluginDialectID(pluginID, d.ID()); err != nil {
		return err
	}
	return r.register(d)
}

// checkPluginDialectID 判方言 ID 的归属：lax/ 是内置的，插件方言必须带 <插件 ID>/ 前缀。
// 登记与安装前的静态检查用同一份规则，两处结论才不会打架。
func checkPluginDialectID(pluginID, id string) error {
	// 先判 lax/：写错了命名空间的方言要直接把原因说出来。
	if strings.HasPrefix(id, "lax/") {
		return fmt.Errorf("插件方言 %q 不能冒用 lax/ 前缀：那是内置方言的命名空间", id)
	}
	if pluginID == "" {
		return fmt.Errorf("登记插件方言 %q 需要插件 ID", id)
	}
	if !strings.HasPrefix(id, pluginID+"/") {
		return fmt.Errorf("插件方言 %q 必须以插件 ID 加斜杠开头（%s/）：前缀对不上时，删除插件找不到要清掉的绑定",
			id, pluginID)
	}
	return nil
}

// Dialect 按 ID 取方言。
func (r *Registry) Dialect(id string) (Dialect, bool) {
	if r == nil {
		return nil, false
	}
	d, ok := r.dialects[id]
	return d, ok
}

// IDs 按字典序返回已登记的方言 ID。
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.dialects))
	for id := range r.dialects {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (r *Registry) register(d Dialect) error {
	if err := checkDialectShape(d); err != nil {
		return err
	}
	if _, exists := r.dialects[d.ID()]; exists {
		return fmt.Errorf("方言 %q 已经登记过：同一个 ID 不能有两个实现", d.ID())
	}
	r.dialects[d.ID()] = d
	return nil
}

// unregister 只给整包回滚用：一个插件的方言要么全进，要么一个都不进。
func (r *Registry) unregister(id string) {
	delete(r.dialects, id)
}

// checkDialectShape 只查注册表自己依赖的几件事：ID 非空、操作与线协议是宿主认识的。
func checkDialectShape(d Dialect) error {
	return checkDialectFields(d.ID(), d.Operation(), d.UpstreamWire())
}

// checkDialectFields 是 checkDialectShape 的字段版本：安装前的静态检查只拿到声明，
// 还没编出 Dialect，两处要判同一套规则。
func checkDialectFields(id, operation, wire string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return fmt.Errorf("方言缺少 ID")
	case !KnownOperation(operation):
		return fmt.Errorf("方言 %q 的 operation %q 不是宿主认识的操作", id, operation)
	case !KnownWire(wire):
		return fmt.Errorf("方言 %q 的 upstream_wire %q 不是宿主认识的线协议", id, wire)
	}
	return nil
}
