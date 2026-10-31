package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// 模块边界。插件包里的 .js 按受限 ESM 写，转换器逐 token 改写 import/export：
// 不引入解析器，所以支持的写法就是下面列出的几种，其余一律带行号报错。
//
// 换算成执行模型是 CommonJS 式的：每个文件包进
// `(function(module, exports, __require){ … })`，__require 只认相对路径并落在插件根
// 目录之内。import 与 export 都是静态的，没有顶层 await，也没有动态加载。

const (
	// maxModuleBytes 是单个源文件的上限：插件是用户数据，读文件不能没有边界。
	maxModuleBytes = 4 << 20
	// maxModuleTotal 是一个插件所有源文件的编译上限。
	maxModuleTotal = 8 << 20
	// maxModules 是一次加载里最多能拉进来的文件数。
	maxModules = 64
)

// jsVM 是一次 goja Runtime 加它自己的模块表。模块表必须跟着 Runtime 走：
// 同一个文件在不同调用里各求值一次，模块级变量不跨调用。
type jsVM struct {
	rt      *goja.Runtime
	modules map[string]goja.Value
	exports *goja.Object
	// dead 表示这次 Runtime 被中断过，后续调用必须重建。
	dead bool
}

// newVM 建一个 Runtime，注入 lax 与 console，再执行入口模块。
func (s *jsSandbox) newVM(initTimeout time.Duration) (*jsVM, error) {
	// 模块顶层也可能调 lax.http.request，它不该继承上一个 hook 用剩的预算。
	s.net = netBudget{}
	rt := goja.New()
	vm := &jsVM{rt: rt, modules: map[string]goja.Value{}}
	api := &sandboxAPI{sandbox: s, vm: vm}
	if err := api.install(); err != nil {
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("初始化 JS 运行时失败：%v", err)}
	}
	timer := time.AfterFunc(initTimeout, func() { rt.Interrupt(interruptHookTimeout) })
	exported, err := s.loadModule(vm, s.entry)
	if !timer.Stop() {
		vm.dead = true
		rt.ClearInterrupt()
	}
	if err != nil {
		return nil, s.moduleError(s.entry, err)
	}
	if vm.dead {
		return nil, &HookError{Code: ErrorPluginTimeout, Message: fmt.Sprintf("%s 加载超时：模块顶层代码里有死循环或等不到结果的 await", s.relName(s.entry))}
	}
	object, ok := exported.(*goja.Object)
	if !ok {
		return nil, &HookError{Code: ErrorPluginError, Message: fmt.Sprintf("入口模块 module.exports 是%s：入口要导出带 hook 函数的对象，例如 export default { prepare_request(ctx) { … } }", gojaShape(exported))}
	}
	vm.exports = object
	return vm, nil
}

// loadModule 求值一个源文件并返回它的 module.exports。先登记再执行：两个文件互相
// import 时，后加载的那个拿到的是对方已经写好的半成品 exports，不会无限递归。
func (s *jsSandbox) loadModule(vm *jsVM, path string) (goja.Value, error) {
	if cached, ok := vm.modules[path]; ok {
		return cached, nil
	}
	if len(vm.modules) >= maxModules {
		return nil, fmt.Errorf("一次加载最多 import %d 个文件，%s 是第 %d 个：检查是不是有一串无用的 import", maxModules, s.relName(path), len(vm.modules)+1)
	}
	program, err := s.program(path)
	if err != nil {
		return nil, err
	}
	exports := vm.rt.NewObject()
	module := vm.rt.NewObject()
	if err := module.Set("exports", exports); err != nil {
		return nil, fmt.Errorf("准备 %s 的 module.exports 失败：%v", s.relName(path), err)
	}
	vm.modules[path] = exports

	require := func(call goja.FunctionCall) goja.Value {
		spec := call.Argument(0).String()
		loaded, err := s.requireFrom(vm, path, spec)
		if err != nil {
			// panic 一个普通 JS 对象才是 throw；panic Go error 会被 goja 重新抛到宿主。
			failure := vm.rt.NewObject()
			_ = failure.Set("name", "LaxError")
			_ = failure.Set("message", err.Error())
			panic(failure)
		}
		return loaded
	}
	value, err := vm.rt.RunProgram(program)
	if err != nil {
		return nil, err
	}
	body, ok := goja.AssertFunction(value)
	if !ok {
		return nil, fmt.Errorf("%s 编译后不是函数：这是转换器的内部错误", s.relName(path))
	}
	if _, err := body(goja.Undefined(), module, exports, vm.rt.ToValue(require)); err != nil {
		return nil, err
	}
	final := module.Get("exports")
	if final == nil || goja.IsUndefined(final) || goja.IsNull(final) {
		final = exports
	}
	vm.modules[path] = final
	return final, nil
}

// requireFrom 解析一条相对 import，并保证落点在插件根目录之内。
func (s *jsSandbox) requireFrom(vm *jsVM, importer, spec string) (goja.Value, error) {
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return nil, fmt.Errorf("%s 里 import 的 %q 不是相对路径：只能写 ./ 或 ../ 开头的文件路径", s.relName(importer), spec)
	}
	target := filepath.Join(filepath.Dir(importer), filepath.FromSlash(spec))
	if err := withinRoot(s.root, target); err != nil {
		return nil, fmt.Errorf("%s 里 import 的 %q 指到了插件目录之外：插件只能引用自己根目录里的文件", s.relName(importer), spec)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("%s 里 import 的 %q 读不到：%v；路径要带扩展名，例如 \"./lib/a.js\"", s.relName(importer), spec, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s 里 import 的 %q 是目录：写目录里的具体文件", s.relName(importer), spec)
	}
	return s.loadModule(vm, target)
}

// withinRoot 判断 path 是否落在 root 之内，用于拦住 ../ 逃逸。
func withinRoot(root, path string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(absPath, absRoot+string(os.PathSeparator)) {
		return fmt.Errorf("%q 不在插件目录 %q 之内", absPath, absRoot)
	}
	return nil
}

// program 读文件、改写 ESM、编译，结果按绝对路径缓存：同一个源文件在一个插件里只
// 编译一次，之后每次新建 Runtime 直接复用不可变的 Program。
func (s *jsSandbox) program(path string) (*goja.Program, error) {
	if cached, ok := s.programs[path]; ok {
		return cached, nil
	}
	name := s.relName(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读插件脚本 %s 失败：%v", name, err)
	}
	if info.Size() > maxModuleBytes {
		return nil, fmt.Errorf("插件脚本 %s 有 %d 字节，超过单文件 %d 字节（%d MiB）上限", name, info.Size(), maxModuleBytes, maxModuleBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读插件脚本 %s 失败：%v", name, err)
	}
	source, exported, err := transformModule(name, string(data))
	if err != nil {
		return nil, err
	}
	// 前缀不换行：源文件的行号与编译后的行号一一对应，报错行号才对得上。
	var body strings.Builder
	body.WriteString("(function(module, exports, __require){")
	body.WriteString(source)
	// 具名导出在模块末尾才落到 module.exports 上：这样 export default 先换掉
	// module.exports 也不影响具名导出，两边同时用的时候都拿得到。
	for _, ident := range exported {
		fmt.Fprintf(&body, "\nmodule.exports.%s = %s;", ident, ident)
	}
	body.WriteString("\n})")
	wrapped := body.String()
	if s.sourceBytes+len(wrapped) > maxModuleTotal {
		return nil, fmt.Errorf("插件脚本总量超过 %d 字节（%d MiB）上限：拆掉用不到的 import，或把大块数据放进模型目录", maxModuleTotal, maxModuleTotal>>20)
	}
	program, err := goja.Compile(name, wrapped, true)
	if err != nil {
		return nil, fmt.Errorf("编译 %s 失败：%v", name, err)
	}
	s.sourceBytes += len(wrapped)
	s.programs[path] = program
	return program, nil
}

// relName 把绝对路径缩成插件目录内的相对路径，报错与堆栈里更好认。
func (s *jsSandbox) relName(path string) string {
	if rel, err := filepath.Rel(s.root, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return path
}

// ------------------------------------------------------------------ ESM 转换

const (
	tokIdent = iota
	tokPunct
	tokString
	tokTemplate
	tokNumber
	tokRegex
)

type jsToken struct {
	kind       int
	text       string
	line       int
	start, end int
}

type jsScanner struct {
	src  string
	pos  int
	line int
}

func scanJS(src string) ([]jsToken, error) {
	s := &jsScanner{src: src, line: 1}
	out := make([]jsToken, 0, len(src)/4+1)
	var prev *jsToken
	for {
		tok, ok, err := s.next(prev)
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, tok)
		prev = &out[len(out)-1]
	}
}

func (s *jsScanner) next(prev *jsToken) (jsToken, bool, error) {
	if err := s.skipSpace(); err != nil {
		return jsToken{}, false, err
	}
	if s.pos >= len(s.src) {
		return jsToken{}, false, nil
	}
	start, line := s.pos, s.line
	c := s.src[s.pos]
	switch {
	case c == '"' || c == '\'':
		if err := s.scanString(c); err != nil {
			return jsToken{}, false, err
		}
		return jsToken{kind: tokString, text: s.src[start:s.pos], line: line, start: start, end: s.pos}, true, nil
	case c == '`':
		if err := s.scanTemplate(); err != nil {
			return jsToken{}, false, err
		}
		return jsToken{kind: tokTemplate, text: s.src[start:s.pos], line: line, start: start, end: s.pos}, true, nil
	case c == '/' && regexAllowed(prev):
		if err := s.scanRegex(); err != nil {
			return jsToken{}, false, err
		}
		return jsToken{kind: tokRegex, text: s.src[start:s.pos], line: line, start: start, end: s.pos}, true, nil
	case isIdentStart(c):
		for s.pos < len(s.src) && isIdentPart(s.src[s.pos]) {
			s.pos++
		}
		return jsToken{kind: tokIdent, text: s.src[start:s.pos], line: line, start: start, end: s.pos}, true, nil
	case c >= '0' && c <= '9':
		for s.pos < len(s.src) && (isIdentPart(s.src[s.pos]) || s.src[s.pos] == '.') {
			s.pos++
		}
		return jsToken{kind: tokNumber, text: s.src[start:s.pos], line: line, start: start, end: s.pos}, true, nil
	default:
		s.pos++
		return jsToken{kind: tokPunct, text: s.src[start:s.pos], line: line, start: start, end: s.pos}, true, nil
	}
}

func (s *jsScanner) skipSpace() error {
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\n':
			s.line++
			s.pos++
		case ' ', '\t', '\r', '\v', '\f':
			s.pos++
		case '/':
			if strings.HasPrefix(s.src[s.pos:], "//") {
				for s.pos < len(s.src) && s.src[s.pos] != '\n' {
					s.pos++
				}
				continue
			}
			if strings.HasPrefix(s.src[s.pos:], "/*") {
				open := s.line
				s.pos += 2
				for s.pos < len(s.src) && !strings.HasPrefix(s.src[s.pos:], "*/") {
					if s.src[s.pos] == '\n' {
						s.line++
					}
					s.pos++
				}
				if s.pos >= len(s.src) {
					return fmt.Errorf("第 %d 行的 /* 注释没有闭合", open)
				}
				s.pos += 2
				continue
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

func (s *jsScanner) scanString(quote byte) error {
	open := s.line
	s.pos++
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case '\n':
			return fmt.Errorf("第 %d 行的字符串没有闭合", open)
		default:
			if s.src[s.pos] == quote {
				s.pos++
				return nil
			}
			s.pos++
		}
	}
	return fmt.Errorf("第 %d 行的字符串没有闭合", open)
}

// scanTemplate 只求把模板字面量整块跳过：${…} 里按花括号配平，嵌进去的引号成对跳过。
func (s *jsScanner) scanTemplate() error {
	open := s.line
	s.pos++
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case '\n':
			s.line++
			s.pos++
		case '`':
			s.pos++
			return nil
		case '$':
			if s.pos+1 < len(s.src) && s.src[s.pos+1] == '{' {
				s.pos += 2
				depth := 1
				for s.pos < len(s.src) && depth > 0 {
					switch s.src[s.pos] {
					case '{':
						depth++
						s.pos++
					case '}':
						depth--
						s.pos++
					case '\n':
						s.line++
						s.pos++
					case '\\':
						s.pos += 2
					case '\'', '"', '`':
						q := s.src[s.pos]
						s.pos++
						for s.pos < len(s.src) && s.src[s.pos] != q {
							if s.src[s.pos] == '\\' {
								s.pos++
							} else if s.src[s.pos] == '\n' {
								s.line++
							}
							s.pos++
						}
						s.pos++
					default:
						s.pos++
					}
				}
				continue
			}
			s.pos++
		default:
			s.pos++
		}
	}
	return fmt.Errorf("第 %d 行的模板字面量没有闭合", open)
}

func (s *jsScanner) scanRegex() error {
	open := s.line
	s.pos++
	inClass := false
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case '\n':
			return fmt.Errorf("第 %d 行的正则字面量没有闭合", open)
		case '[':
			inClass = true
			s.pos++
		case ']':
			inClass = false
			s.pos++
		case '/':
			if inClass {
				s.pos++
				continue
			}
			s.pos++
			for s.pos < len(s.src) && isIdentPart(s.src[s.pos]) {
				s.pos++
			}
			return nil
		default:
			s.pos++
		}
	}
	return fmt.Errorf("第 %d 行的正则字面量没有闭合", open)
}

// regexAllowed 用前一个 token 判断 / 是正则开头还是除号。判错的代价只是多吞或少吞
// 一段字符：改写只认行首的 import/export，误判不会改错代码。
func regexAllowed(prev *jsToken) bool {
	if prev == nil {
		return true
	}
	switch prev.kind {
	case tokIdent:
		switch prev.text {
		case "return", "typeof", "instanceof", "case", "in", "of", "delete", "void", "new", "do", "else", "yield", "await":
			return true
		}
		return false
	case tokPunct:
		switch prev.text {
		case ")", "]", "}":
			return false
		}
		return true
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

type moduleEdit struct {
	start, end int
	text       string
}

type moduleRewriter struct {
	name  string
	src   string
	toks  []jsToken
	edits []moduleEdit
	seq   int
	// exported 是具名导出的标识符，模块末尾统一挂到 module.exports 上。
	exported []string
}

// transformModule 把受限 ESM 改写成可执行的函数体。支持的写法：
//
//	export async function name / export function name / export class name
//	export const name = … / export let / export var
//	export default …
//	import Def from "./a.js" / import {a, b} from "./a.js" / import Def, {a as b} from "./a.js"
//	import "./a.js"
func transformModule(name, src string) (string, []string, error) {
	toks, err := scanJS(src)
	if err != nil {
		return "", nil, fmt.Errorf("%s：%w", name, err)
	}
	r := &moduleRewriter{name: name, src: src, toks: toks}
	depth := 0
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch tok.kind {
		case tokPunct:
			switch tok.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth > 0 {
					depth--
				}
			}
		case tokIdent:
			if depth != 0 {
				continue
			}
			switch tok.text {
			case "import":
				if err := r.importStatement(i); err != nil {
					return "", nil, err
				}
			case "export":
				if err := r.exportStatement(i); err != nil {
					return "", nil, err
				}
			}
		}
	}
	source, err := r.apply()
	if err != nil {
		return "", nil, err
	}
	return source, r.exported, nil
}

func (r *moduleRewriter) errf(line int, format string, args ...any) error {
	return fmt.Errorf("%s:%d：%s", r.name, line, fmt.Sprintf(format, args...))
}

func (r *moduleRewriter) replace(from, to jsToken, text string) {
	// 保留被替换区间里的换行，后面的代码行号才不会漂移
	r.edits = append(r.edits, moduleEdit{start: from.start, end: to.end, text: text + strings.Repeat("\n", strings.Count(r.src[from.start:to.end], "\n"))})
}

func (r *moduleRewriter) apply() (string, error) {
	sort.Slice(r.edits, func(i, j int) bool { return r.edits[i].start < r.edits[j].start })
	var b strings.Builder
	last := 0
	for _, e := range r.edits {
		if e.start < last {
			return "", fmt.Errorf("%s：转换器改写区间重叠，这是转换器的内部错误", r.name)
		}
		b.WriteString(r.src[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(r.src[last:])
	return b.String(), nil
}

func (r *moduleRewriter) importStatement(i int) error {
	tok := r.toks[i]
	line := tok.line
	if i+1 >= len(r.toks) {
		return r.errf(line, "import 后面缺少模块路径")
	}
	next := r.toks[i+1]
	if next.kind == tokPunct {
		switch next.text {
		case "(":
			return r.errf(line, "不支持动态 import()：模块只在加载时求值一次，路径要写成文件顶部的静态 import")
		case ".":
			return r.errf(line, "不支持 import.meta")
		}
	}
	if next.kind == tokString {
		spec, ok := unquoteJS(next.text)
		if !ok {
			return r.errf(line, "import 的模块路径不是合法字符串")
		}
		if err := r.checkSpecifier(line, spec); err != nil {
			return err
		}
		r.replace(tok, next, fmt.Sprintf("__require(%s)", next.text))
		return nil
	}
	from := -1
	for k := i + 1; k < len(r.toks); k++ {
		tk := r.toks[k]
		if tk.kind == tokIdent && tk.text == "from" {
			from = k
			break
		}
		if tk.kind == tokPunct && tk.text == ";" {
			break
		}
	}
	if from < 0 || from+1 >= len(r.toks) || r.toks[from+1].kind != tokString {
		return r.errf(line, "import 要写成 import X from \"./a.js\" 或 import {a, b} from \"./a.js\"")
	}
	specTok := r.toks[from+1]
	spec, ok := unquoteJS(specTok.text)
	if !ok {
		return r.errf(line, "import 的模块路径不是合法字符串")
	}
	if err := r.checkSpecifier(line, spec); err != nil {
		return err
	}

	clause := r.toks[i+1 : from]
	for _, tk := range clause {
		switch {
		case tk.kind == tokPunct && tk.text == "*":
			return r.errf(line, "不支持 import * as：按名字导入，例如 import { a } from \"./a.js\"")
		case tk.kind == tokPunct && tk.text == "(":
			return r.errf(line, "不支持动态 import()")
		}
	}
	var defaultName string
	if len(clause) > 0 && clause[0].kind == tokIdent {
		defaultName = clause[0].text
		clause = clause[1:]
		if len(clause) > 0 {
			if !isPunct(clause[0], ",") {
				return r.errf(line, "import 的默认导入后面只能跟 { … }，遇到 %q", clause[0].text)
			}
			clause = clause[1:]
		}
	}
	named := ""
	if len(clause) > 0 {
		binding, err := r.namedBinding(clause, line)
		if err != nil {
			return err
		}
		named = binding
	}
	switch {
	case defaultName == "" && named == "":
		return r.errf(line, "import 没有绑定名字：只想求值副作用就写 import \"./a.js\"")
	case named == "":
		r.replace(tok, specTok, fmt.Sprintf("const %s = __require(%s)", defaultName, specTok.text))
	case defaultName == "":
		r.replace(tok, specTok, fmt.Sprintf("const %s = __require(%s)", named, specTok.text))
	default:
		r.seq++
		tmp := fmt.Sprintf("__lax_import_%d", r.seq)
		r.replace(tok, specTok, fmt.Sprintf("const %s = __require(%s); const %s = %s; const %s = %s",
			tmp, specTok.text, defaultName, tmp, named, tmp))
	}
	return nil
}

// namedBinding 把 {a, b as c} 重建成解构用的 { a, b: c }。
func (r *moduleRewriter) namedBinding(clause []jsToken, line int) (string, error) {
	if len(clause) < 2 || !isPunct(clause[0], "{") || !isPunct(clause[len(clause)-1], "}") {
		return "", r.errf(line, "import 的绑定只支持 import X from、import {a, b} from 与 import X, {a, b} from 三种写法")
	}
	inner := clause[1 : len(clause)-1]
	parts := make([]string, 0, len(inner))
	for k := 0; k < len(inner); k++ {
		tk := inner[k]
		if isPunct(tk, ",") {
			continue
		}
		if tk.kind != tokIdent {
			return "", r.errf(line, "import 的名字列表里只能有标识符，遇到 %q", tk.text)
		}
		binding := tk.text
		if k+1 < len(inner) && inner[k+1].kind == tokIdent && inner[k+1].text == "as" {
			if k+2 >= len(inner) || inner[k+2].kind != tokIdent {
				return "", r.errf(line, "import 的 as 后面要跟新名字")
			}
			binding = tk.text + ": " + inner[k+2].text
			k += 2
		}
		parts = append(parts, binding)
	}
	if len(parts) == 0 {
		return "", r.errf(line, "import 的 { } 里没有名字")
	}
	return "{ " + strings.Join(parts, ", ") + " }", nil
}

func (r *moduleRewriter) checkSpecifier(line int, spec string) error {
	switch {
	case spec == "":
		return r.errf(line, "import 的模块路径是空字符串")
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		return nil
	case strings.HasPrefix(spec, "/"):
		return r.errf(line, "import 的路径 %q 是绝对路径：只能写 ./ 或 ../ 开头的相对路径", spec)
	}
	return r.errf(line, "import 的路径 %q 不是相对路径：插件只能 import 自己目录里的文件（./ 或 ../ 开头），不支持裸模块名与 node: 内置模块", spec)
}

func (r *moduleRewriter) exportStatement(i int) error {
	tok := r.toks[i]
	line := tok.line
	if i+1 >= len(r.toks) {
		return r.errf(line, "export 后面缺少声明")
	}
	next := r.toks[i+1]
	if next.kind == tokPunct {
		switch next.text {
		case "{":
			return r.errf(line, "不支持 export { … }：逐个写 export function 名字 或 export const 名字 = …")
		case "*":
			return r.errf(line, "不支持 export *：要用的名字逐个导出")
		}
		return r.errf(line, "export 后面是不认识的写法 %q", next.text)
	}
	if next.kind != tokIdent {
		return r.errf(line, "export 后面要跟 function、async function、class、const 或 default")
	}
	switch next.text {
	case "default":
		r.replace(tok, next, "module.exports =")
		return nil
	case "async":
		if i+3 < len(r.toks) && r.toks[i+2].kind == tokIdent && r.toks[i+2].text == "function" {
			if err := r.nameExport(i+3, line); err != nil {
				return err
			}
			r.replace(tok, tok, "")
			return nil
		}
		return r.errf(line, "只支持 export async function 名字(…)：其它 async 写法先声明，再用 export default 或 module.exports 暴露")
	case "function", "class":
		if err := r.nameExport(i+2, line); err != nil {
			return err
		}
		r.replace(tok, tok, "")
		return nil
	case "const", "let", "var":
		if err := r.nameExport(i+2, line); err != nil {
			return err
		}
		r.replace(tok, tok, "")
		return nil
	}
	return r.errf(line, "不支持的 export 写法 %q：只支持 export function、export async function、export class、export const/let/var 与 export default", next.text)
}

// nameExport 记下具名导出的标识符，模块末尾统一挂到 module.exports 上。
func (r *moduleRewriter) nameExport(index, line int) error {
	if index >= len(r.toks) || r.toks[index].kind != tokIdent {
		return r.errf(line, "export 后面要跟一个名字")
	}
	r.exported = append(r.exported, r.toks[index].text)
	return nil
}

func isPunct(tok jsToken, text string) bool {
	return tok.kind == tokPunct && tok.text == text
}

// unquoteJS 取字符串字面量的内容。模块路径里不会有转义，这里只剥引号。
func unquoteJS(raw string) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	quote := raw[0]
	if quote != '"' && quote != '\'' {
		return "", false
	}
	if raw[len(raw)-1] != quote {
		return "", false
	}
	return raw[1 : len(raw)-1], true
}

// gojaShape 给错误文案描述一个 JS 值的形状。
func gojaShape(v goja.Value) string {
	if v == nil {
		return "空值"
	}
	if _, ok := goja.AssertFunction(v); ok {
		return "函数"
	}
	if object, ok := v.(*goja.Object); ok {
		return " " + object.ClassName()
	}
	return " " + v.ExportType().String()
}
