package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Plugin 是一个解析好的插件包：manifest，加它引用的方言与模型目录。
type Plugin struct {
	// Root 是插件根目录，所有被引用的文件都在它之内。
	Root     string
	Manifest Manifest
	// Dialects 是各 provider 声明的方言，按 manifest 里 providers 的顺序。
	Dialects []DialectDecl
	// Catalogs 按 provider ID 索引。
	Catalogs map[string]CatalogFile
	// DialectsByProvider 记录每家 provider 能用的方言 ID。
	DialectsByProvider map[string][]string
	// ManifestBytes 是 lax.plugin.json 原文，安装时用来算 manifest 哈希。
	ManifestBytes []byte
	// sandbox 是懒打开的沙箱，同一次加载只开一次：一个插件版本的模块状态归它一个。
	sandbox Sandbox
}

// LoadDir 读取并校验一个插件目录。任何一处不合规都只返回第一个错误，
// 错误里带文件名与字段路径。
func LoadDir(root string) (Plugin, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Plugin{}, fmt.Errorf("读插件目录 %s 失败: %v", root, err)
	}
	if !info.IsDir() {
		return Plugin{}, fmt.Errorf("%s 不是目录：插件源可以是目录，也可以是已解开的包根", root)
	}
	manifestPath := filepath.Join(root, "lax.plugin.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return Plugin{}, fmt.Errorf("读 %s 失败: %v；插件根目录里必须有 lax.plugin.json", manifestPath, err)
	}
	manifest, err := DecodeManifest(manifestPath, manifestBytes)
	if err != nil {
		return Plugin{}, err
	}

	out := Plugin{
		Root:               root,
		Manifest:           manifest,
		Catalogs:           map[string]CatalogFile{},
		DialectsByProvider: map[string][]string{},
		ManifestBytes:      manifestBytes,
	}

	for _, provider := range manifest.Providers {
		ids := make([]string, 0, 1)
		if provider.Dialects != "" {
			path, err := pluginFile(root, provider.Dialects)
			if err != nil {
				return Plugin{}, fmt.Errorf("provider %s.dialects：%w", provider.ID, err)
			}
			file, err := LoadDialectFile(path)
			if err != nil {
				return Plugin{}, err
			}
			for _, decl := range file.Dialects {
				out.Dialects = append(out.Dialects, decl)
				ids = append(ids, decl.ID)
			}
		}
		out.DialectsByProvider[provider.ID] = ids

		if provider.Catalog != "" {
			path, err := pluginFile(root, provider.Catalog)
			if err != nil {
				return Plugin{}, fmt.Errorf("provider %s.catalog：%w", provider.ID, err)
			}
			catalog, err := LoadCatalog(path)
			if err != nil {
				return Plugin{}, err
			}
			out.Catalogs[provider.ID] = catalog
		}
	}

	if err := checkCatalogBindings(manifest, out); err != nil {
		return Plugin{}, err
	}
	return out, nil
}

// CompileDialects 把声明式方言编成可执行的方言，并对每条再做一次注册表层面的检查。
func (p Plugin) CompileDialects() ([]Dialect, error) {
	out := make([]Dialect, 0, len(p.Dialects))
	for _, decl := range p.Dialects {
		dialect, err := CompileDeclaredDialect(decl)
		if err != nil {
			return nil, err
		}
		out = append(out, dialect)
	}
	return out, nil
}

// CompileDeclaredDialect 编一条方言：hook 实现留给沙箱，声明式走 CompileDialect。
func CompileDeclaredDialect(decl DialectDecl) (Dialect, error) {
	if isHookDialect(decl) {
		return nil, fmt.Errorf("方言 %s 用 hook 实现：hook 方言要经 Plugin.Register 打开沙箱才能登记", decl.ID)
	}
	return CompileDialect(decl)
}

// isHookDialect 判一条方言声明的实现方式：任一钩子写 hook 就归沙箱。
func isHookDialect(decl DialectDecl) bool {
	return decl.Implementation.Prepare == ImplHook || decl.Implementation.Decode == ImplHook
}

// RegisterPlugin 只登记声明式方言，是 Register 的旧口径：内置方言与声明式插件不需要沙箱。
// 带代码（hook 方言）的插件要经 Plugin.Register 打开沙箱后登记，这里会直接报错。
func (r *Registry) RegisterPlugin(p Plugin) error {
	compiled, err := p.CompileDialects()
	if err != nil {
		return err
	}
	for _, d := range compiled {
		if err := checkDialectShape(d); err != nil {
			return err
		}
		if _, exists := r.dialects[d.ID()]; exists {
			return fmt.Errorf("方言 %q 与已登记的方言冲突：安装被拒绝，同一个 ID 不能有两个实现", d.ID())
		}
	}
	for _, d := range compiled {
		if err := r.RegisterPluginDialect(p.Manifest.ID, d); err != nil {
			return err
		}
	}
	return nil
}

// CheckDialects 只做登记前的静态检查：ID 归属、形状、包内重复、与注册表撞车、实现方式与运行时是否配套。
// 不编方言也不开沙箱，安装与 doctor 因此能在不动插件代码的前提下判断这一包能不能进注册表。
func (p Plugin) CheckDialects(reg *Registry) error {
	seen := make(map[string]bool, len(p.Dialects))
	for _, decl := range p.Dialects {
		if err := checkPluginDialectID(p.Manifest.ID, decl.ID); err != nil {
			return err
		}
		if err := checkDialectFields(decl.ID, decl.Operation, decl.UpstreamWire); err != nil {
			return err
		}
		if seen[decl.ID] {
			return fmt.Errorf("方言 %q 在包里声明了两次：同一个 ID 只能有一个实现", decl.ID)
		}
		seen[decl.ID] = true
		if reg != nil {
			if _, exists := reg.Dialect(decl.ID); exists {
				return fmt.Errorf("方言 %q 与已登记的方言冲突：同一个 ID 不能有两个实现", decl.ID)
			}
		}
		if !isHookDialect(decl) {
			continue
		}
		// hook 方言只能由沙箱执行，声明式运行时没有可跑代码的地方。
		if p.Manifest.Runtime != RuntimeMoejs {
			return fmt.Errorf("方言 %s 用 hook 实现，插件的 runtime 却是 %q：把 runtime 改成 %s 并写 entry",
				decl.ID, p.Manifest.Runtime, RuntimeMoejs)
		}
		if decl.Implementation.Prepare != ImplHook || decl.Implementation.Decode != ImplHook {
			return fmt.Errorf("方言 %s 的 implementation.prepare 与 implementation.decode 要都写 %s：一半声明式一半 hook 的方言没有执行路径",
				decl.ID, ImplHook)
		}
	}
	return nil
}

// Register 把一个插件的方言全登记进注册表：声明式方言就地编译，hook 方言包进打开的沙箱。
// 整包要么全进要么一个都不进；打开沙箱失败时一个方言都不登记。
func (p *Plugin) Register(reg *Registry, o SandboxOptions) error {
	if err := p.CheckDialects(reg); err != nil {
		return err
	}
	if p.hasHookDialects() {
		if err := p.OpenSandbox(o); err != nil {
			return err
		}
	}
	dialects := make([]Dialect, 0, len(p.Dialects))
	for _, decl := range p.Dialects {
		if isHookDialect(decl) {
			dialects = append(dialects, HookDialect{
				IDValue:      decl.ID,
				OperationVal: decl.Operation,
				WireVal:      decl.UpstreamWire,
				Sandbox:      p.sandbox,
			})
			continue
		}
		d, err := CompileDialect(decl)
		if err != nil {
			return err
		}
		dialects = append(dialects, d)
	}

	registered := make([]string, 0, len(dialects))
	for _, d := range dialects {
		if err := reg.RegisterPluginDialect(p.Manifest.ID, d); err != nil {
			for _, id := range registered {
				reg.unregister(id)
			}
			return err
		}
		registered = append(registered, d.ID())
	}
	return nil
}

// hasHookDialects 判这一包有没有要靠沙箱执行的方言。
func (p Plugin) hasHookDialects() bool {
	for _, decl := range p.Dialects {
		if isHookDialect(decl) {
			return true
		}
	}
	return false
}

// OpenSandbox 打开插件的沙箱。同一次加载只开一次：重复调用直接返回，已打开实例不重建。
// 未填的 Root、Entry 与 Manifest 用包里的值补，调用方只需给信任级别与超时。
func (p *Plugin) OpenSandbox(o SandboxOptions) error {
	if p.sandbox != nil {
		return nil
	}
	if o.Root == "" {
		o.Root = p.Root
	}
	if o.Entry == "" {
		o.Entry = p.Manifest.Entry
	}
	if o.Manifest.ID == "" {
		o.Manifest = p.Manifest
	}
	sandbox, err := OpenSandbox(o)
	if err != nil {
		return err
	}
	p.sandbox = sandbox
	return nil
}

// Close 关掉已打开的沙箱。没开过时什么也不做。
func (p *Plugin) Close() error {
	if p.sandbox == nil {
		return nil
	}
	sandbox := p.sandbox
	p.sandbox = nil
	return sandbox.Close()
}

// ABI 定义的钩子名。沙箱按名字取导出，doctor 也按这几个名字核对入口。
const (
	HookPrepareRequest = "prepare_request"
	HookDecodeResponse = "decode_response"
	HookDecodeStream   = "decode_stream"
)

// abiHooks 是 ABI 定义的钩子名，按探测顺序排列。
var abiHooks = []string{HookPrepareRequest, HookDecodeResponse, HookDecodeStream}

// ExportedHooks 返回入口源码里导出的钩子名。只能静态核对文本：沙箱不向外吐导出表，
// 语法错误归沙箱的 OpenSandbox 报，这里只管「名字有没有导出」。
func (p Plugin) ExportedHooks() ([]string, error) {
	if p.Manifest.Entry == "" {
		return nil, fmt.Errorf("manifest 没有 entry：runtime 是 %s 的插件要有入口文件", RuntimeMoejs)
	}
	file, err := pluginFile(p.Root, p.Manifest.Entry)
	if err != nil {
		return nil, err
	}
	source, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("读入口文件 %s 失败: %v", file, err)
	}
	names := make([]string, 0, len(abiHooks))
	for _, name := range abiHooks {
		if exportsHook(string(source), name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// exportsHook 只判「名字在入口里以导出或字段的形式出现过」。导出的是不是函数、模块能不能
// 执行，由 OpenSandbox 负责：两处一起才算通过。因此宁可误放也不误拦。
func exportsHook(source, name string) bool {
	quoted := regexp.QuoteMeta(name)
	for _, pattern := range []string{
		`(?m)\bexport\s+(?:async\s+)?function\s*\*?\s*` + quoted + `\b`,
		`(?m)\bexport\s+(?:const|let|var)\s+` + quoted + `\b`,
		`(?m)\bexport\s*\{[^}]*\b` + quoted + `\b[^}]*\}`,
		// export default { name(…){…} }、name: fn、name = fn 都与具名声明一并算上。
		`(?m)\b` + quoted + `\s*[:(=]`,
	} {
		if regexp.MustCompile(pattern).MatchString(source) {
			return true
		}
	}
	return false
}

// PluginDirHash 算插件目录的内容哈希，安装时记进 plugin_versions.package_sha256。
// 符号链接不跟随也不计入：安装流水线会先拒绝它们。
func PluginDirHash(root string) (string, error) {
	hash := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("遍历插件目录 %s 失败: %v", root, err)
	}
	slices.Sort(paths)
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00", filepath.ToSlash(rel))
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("读 %s 失败: %v", path, err)
		}
		if _, err := io.Copy(hash, file); err != nil {
			file.Close()
			return "", fmt.Errorf("读 %s 失败: %v", path, err)
		}
		file.Close()
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ManifestHash 算 manifest 原文的哈希。
func ManifestHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pluginFile 把 manifest 里的相对路径落到插件根之下，拒绝逃逸。
func pluginFile(root, rel string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absPath, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q 指到了插件目录之外：插件只能引用自己根目录里的文件", rel)
	}
	return absPath, nil
}

// checkCatalogBindings 确认目录里引用的方言都由同一家 provider 声明过。
func checkCatalogBindings(manifest Manifest, p Plugin) error {
	for _, provider := range manifest.Providers {
		catalog, ok := p.Catalogs[provider.ID]
		if !ok {
			continue
		}
		available := p.DialectsByProvider[provider.ID]
		for _, model := range catalog.Models {
			for operation, decl := range model.Operations {
				// 目录可以直接引用宿主方言（例如 lax/openai-chat@1）：那表示这条操作由
				// 宿主自己的编解码器处理，不需要插件提供映射。
				if IsHostDialect(decl.Dialect) {
					if _, ok := p.hostDialects()[decl.Dialect]; !ok {
						return fmt.Errorf("模型 %s 的 %s 引用了宿主方言 %q，但宿主没有这条方言；可用的是 %s",
							model.Model, operation, decl.Dialect, strings.Join(hostDialectIDs(), "、"))
					}
					continue
				}
				if !slices.Contains(available, decl.Dialect) {
					return fmt.Errorf("模型 %s 的 %s 绑定了方言 %q，但它不在 provider %s 的 dialects 文件里；"+
						"可用的是：%s", model.Model, operation, decl.Dialect, provider.ID,
						strings.Join(available, "、"))
				}
			}
		}
	}
	return nil
}

// hostDialects 列出宿主方言 ID，供目录引用校验用。
func (p Plugin) hostDialects() map[string]struct{} {
	out := map[string]struct{}{}
	for _, d := range SystemDialects() {
		out[d.ID()] = struct{}{}
	}
	return out
}

// hostDialectIDs 按固定顺序返回宿主方言 ID，供错误文案列出可用值。
func hostDialectIDs() []string {
	out := make([]string, 0, len(SystemDialects()))
	for _, d := range SystemDialects() {
		out = append(out, d.ID())
	}
	return out
}
