package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"switchboard/internal/account"
	"switchboard/internal/gateway"
	"switchboard/internal/store"
)

// systemOnePath 是 systemone 的入站路径（DESIGN 5.14）：模型名在请求体里，不在路径上。
// cli 一直是这样写死入站路径的（见 observe.go 的 inboundPath），网关那边注册的也是这一串。
const systemOnePath = "/v1/systemone"

// decideUsage 是所有用法错误都会打出来的那一行：参数形状、三种拼问题的写法与整块 JSON 的写法。
// 用法错误只说「参数错误」等于让人去猜语法（见 AGENTS.md）。
const decideUsage = "用法：switchboard decide --model <模型> --state <文本或 JSON> " +
	"[--noul <名>=<问句>] [--choice <名>=<问句> --choice <名>:<选项>[,<选项>...]] " +
	"[--score <名>=<问句> --score <名>:<级别>[,<级别>...]] [--questions <整块 JSON>] [--json]\n" +
	"  --questions 与 --noul/--choice/--score 二选一：前者给整块 questions 对象，后者按问题逐条拼"

// repeatedFlag 收集同一个参数在命令行上出现的每一次取值，保持出现顺序。
// flag 包对同一个 Var 按顺序调 Set，重复写同一个 flag 因此不需要自己扫 argv。
type repeatedFlag struct{ values []string }

func (r *repeatedFlag) String() string { return strings.Join(r.values, "，") }

func (r *repeatedFlag) Set(v string) error {
	r.values = append(r.values, v)
	return nil
}

// decideItem 是 choice 的一个选项或 score 的一级级别：key 是判据本身，desc 是可选说明。
// 写法是逗号分隔的 `key` 或 `key=说明`（见 DESIGN 第 4 节的 decide 一行）。
type decideItem struct {
	key     string
	desc    string
	hasDesc bool
}

// decideQuestion 是一个拼出来的问题：名字、类型、问句，choice 与 score 另有判据。
type decideQuestion struct {
	name   string
	kind   string
	prompt string
	items  []decideItem
}

// decideQuestions 收集命令行里拼出来的问题。声明（`名=问句`）与判据（`名:...`）分成两种写法，
// 名字必须先声明再用判据补：先声明后追加与人的读法一致，顺序也定了输出的行序。
type decideQuestions struct {
	order  []string
	byName map[string]*decideQuestion
}

func newDecideQuestions() *decideQuestions {
	return &decideQuestions{byName: map[string]*decideQuestion{}}
}

func (q *decideQuestions) len() int { return len(q.order) }

// add 处理 `--noul/--choice/--score` 的一次出现。取值有两种形式：`名=问句` 是声明，
// `名:判据[,判据...]` 是给已声明的问题追加选项或级别。判定只看第一个 `=` 与第一个 `:`
// 谁在前：声明里问句可以带冒号，判据里说明可以带等号，两种写法因此都不必转义。
func (q *decideQuestions) add(kind, raw string) error {
	name, rest, isDecl := splitDecideFlag(raw)
	if name == "" {
		return usagef("--%s 的取值 %q 少了问题名：写法是 %s，问题名不能为空\n%s", kind, raw, decideFlagForm(kind), decideUsage)
	}
	if isDecl {
		return q.declare(kind, name, rest, raw)
	}
	return q.appendItems(kind, name, rest, raw)
}

// declare 记下一个问题。重名、问句为空、类型换了都是用法错误：同一个问题在一份请求里只能有一种
// 形状，静默取第一条会让用户以为后面那条生效了。
func (q *decideQuestions) declare(kind, name, prompt, raw string) error {
	if strings.ContainsAny(name, " \t:") {
		return usagef("--%s 的问题名 %q 不能带空格或冒号：名字是上游 answers 里的键，也是输出的第一列\n%s", kind, name, decideUsage)
	}
	if strings.TrimSpace(prompt) == "" {
		return usagef("--%s %q 少了问句：写法是 %s，等号之后要写这个问题的问句\n%s", kind, raw, decideFlagForm(kind), decideUsage)
	}
	if prev, ok := q.byName[name]; ok {
		return usagef("问题 %s 声明了两次（--%s 与 --%s）：一个问题只声明一次，再用 --%s %s:… 补它的选项或级别；要换问句就把第一次删掉\n%s",
			name, prev.kind, kind, kind, name, decideUsage)
	}
	q.byName[name] = &decideQuestion{name: name, kind: kind, prompt: prompt}
	q.order = append(q.order, name)
	return nil
}

// appendItems 给已声明的问题追加选项（choice）或级别（score）。还没声明、类型对不上、判据重复
// 都是用法错误：判据写歪了上游只会拒掉整条请求，在这里说清是哪一项更省一轮。
func (q *decideQuestions) appendItems(kind, name, rest, raw string) error {
	question, ok := q.byName[name]
	if !ok {
		return usagef("--%s %q 里的问题 %s 还没声明：先用 --%s %s=<问句> 声明它，再用 --%s %s:… 补选项或级别\n%s",
			kind, raw, name, kind, name, kind, name, decideUsage)
	}
	if question.kind != kind {
		return usagef("问题 %s 已经用 --%s 声明成 %s，不能再用 --%s 给它加判据：一个问题的类型只有一种，把这两条并成同一种写法\n%s",
			name, question.kind, question.kind, kind, decideUsage)
	}
	items, err := parseDecideItems(kind, name, rest)
	if err != nil {
		return err
	}
	for _, it := range items {
		for _, have := range question.items {
			if have.key == it.key {
				return usagef("问题 %s 的判据 %q 写了两遍：每个选项或级别只写一次\n%s", name, it.key, decideUsage)
			}
		}
	}
	question.items = append(question.items, items...)
	return nil
}

// list 按声明顺序返回问题，并判掉「一个问题都没拼出来」这种用法错误。
func (q *decideQuestions) list() ([]decideQuestion, error) {
	if q.len() == 0 {
		return nil, usagef("一个问题都没拼出来：用 --noul 名=问句 问一个能不能，用 --choice 名=问句 再 --choice 名:选项[,选项...] 问一个选择题，用 --score 名=问句 再 --score 名:级别[,级别...] 问一个打分题；也可以直接用 --questions 给整块 JSON\n%s", decideUsage)
	}
	out := make([]decideQuestion, 0, q.len())
	for _, name := range q.order {
		question := q.byName[name]
		switch question.kind {
		case "choice":
			if len(question.items) < 2 {
				return nil, usagef("选择题 %s 只有 %d 个选项：选择题至少要两个选项，用 --choice %s:选项1,选项2 补上（选项写 key 或 key=说明）\n%s",
					name, len(question.items), name, decideUsage)
			}
		case "score":
			if len(question.items) < 2 {
				return nil, usagef("打分题 %s 只有 %d 级：打分至少要两级级别，用 --score %s:级别1,级别2[,级别3...] 补上，级别按从低到高的顺序写\n%s",
					name, len(question.items), name, decideUsage)
			}
		}
		out = append(out, *question)
	}
	return out, nil
}

// splitDecideFlag 把一个 flag 取值拆成名字与其余部分，并判断它是声明还是追加判据。
func splitDecideFlag(raw string) (name, rest string, isDecl bool) {
	eq := strings.Index(raw, "=")
	colon := strings.Index(raw, ":")
	if eq >= 0 && (colon < 0 || eq < colon) {
		return strings.TrimSpace(raw[:eq]), raw[eq+1:], true
	}
	if colon >= 0 {
		return strings.TrimSpace(raw[:colon]), raw[colon+1:], false
	}
	return strings.TrimSpace(raw), "", true
}

// decideFlagForm 给出一个 flag 的两种写法，供错误文案里给下一步命令。
func decideFlagForm(flagName string) string {
	switch flagName {
	case "choice", "score":
		return "--" + flagName + " 名=问句（声明）或 --" + flagName + " 名:判据[,判据...]（追加）"
	}
	return "--" + flagName + " 名=问句"
}

// parseDecideItems 读逗号分隔的判据。choice 的每一项是 `key` 或 `key=说明`；
// score 的每一级只能是 `key`：上游收的是有序的字符串数组，说明没有落点，写了就是用法错误，
// 静默丢掉会让用户以为上游拿到了那句说明。
func parseDecideItems(kind, name, rest string) ([]decideItem, error) {
	parts := strings.Split(rest, ",")
	out := make([]decideItem, 0, len(parts))
	for _, part := range parts {
		raw := strings.TrimSpace(part)
		if raw == "" {
			return nil, usagef("问题 %s 的判据里有一个空项：用逗号分隔，也别留出连续两个逗号，例如 --%s %s:选项1,选项2\n%s",
				name, kind, name, decideUsage)
		}
		item := decideItem{key: raw}
		if kind == "choice" {
			if key, desc, ok := strings.Cut(raw, "="); ok {
				item.key = strings.TrimSpace(key)
				item.desc = strings.TrimSpace(desc)
				item.hasDesc = item.desc != ""
			}
		} else if strings.Contains(raw, "=") {
			return nil, usagef("打分题 %s 的级别 %q 带了说明：打分级别只有名字（上游收的是有序的级别数组，从低到高），把等号与说明删掉再试\n%s",
				name, raw, decideUsage)
		}
		if item.key == "" {
			return nil, usagef("问题 %s 的判据 %q 少了名字：choice 的选项与 score 的级别都要有一个非空的 key\n%s", name, raw, decideUsage)
		}
		out = append(out, item)
	}
	return out, nil
}

// questionBody 是请求体里的一个问题。criteria 对 noul 为空、对 choice 是「选项 → 说明」对象
// （没写说明的选项值是 null，与 TypeSafe SDK 读到的形状一致）、对 score 是有序的级别数组。
type questionBody struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// encodeQuestions 把拼好的问题编成 questions 对象。choice 的判据是 JSON 对象，键的顺序在报文里
// 没有意义（编码器按名字排序），score 的级别按命令行里写的顺序进数组。
func encodeQuestions(questions []decideQuestion) (json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(questions))
	for _, question := range questions {
		body := questionBody{Type: question.kind, Instructions: question.prompt}
		switch question.kind {
		case "choice":
			criteria := make(map[string]any, len(question.items))
			for _, item := range question.items {
				if item.hasDesc {
					criteria[item.key] = item.desc
				} else {
					criteria[item.key] = nil
				}
			}
			raw, err := json.Marshal(criteria)
			if err != nil {
				return nil, err
			}
			body.Criteria = raw
		case "score":
			levels := make([]string, 0, len(question.items))
			for _, item := range question.items {
				levels = append(levels, item.key)
			}
			raw, err := json.Marshal(levels)
			if err != nil {
				return nil, err
			}
			body.Criteria = raw
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		out[question.name] = raw
	}
	return json.Marshal(out)
}

// decodeQuestionsJSON 收下 --questions 的整块对象：必须是带至少一个问题的 JSON 对象。
// 内容不替上游校验（DESIGN 5.14 的最少校验），但形状与空的区别要与上游一致地在这里说清。
func decodeQuestionsJSON(raw string) (json.RawMessage, []decideMeta, error) {
	trimmed := strings.TrimSpace(raw)
	var questions map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &questions); err != nil {
		return nil, nil, usagef("--questions 不是合法的 JSON 对象：%v；形状是 {\"问题名\":{\"type\":\"noul\",\"instructions\":\"…\"}}\n%s", err, decideUsage)
	}
	if len(questions) == 0 {
		return nil, nil, usagef("--questions 是空对象：至少给一个问题，形状是 {\"问题名\":{\"type\":\"noul\",\"instructions\":\"…\"}}\n%s", decideUsage)
	}
	// 整块 JSON 是一张表，键本身没有顺序：输出按名字升序排，两次同样的命令给出同样的行序。
	names := make([]string, 0, len(questions))
	for name := range questions {
		names = append(names, name)
	}
	sort.Strings(names)

	metas := make([]decideMeta, 0, len(names))
	for _, name := range names {
		var q struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(questions[name], &q)
		metas = append(metas, decideMeta{name: name, kind: q.Type})
	}
	return json.RawMessage(trimmed), metas, nil
}

// encodeDecideState 把 --state 变成请求体里的一段 JSON（见 DESIGN 第 4 节的 decide 一行）：
// 去掉首尾空白后首字符是 `{`、`[` 或 `"` 时按 JSON 解析，其余按纯文本字符串发。
// 纯文本原样进 JSON（不裁剪内部的空白）：它就是模型要看的材料。
func encodeDecideState(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		switch trimmed[0] {
		case '{', '[', '"':
			var probe any
			if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
				return nil, usagef("--state 的首字符是 %q，按 JSON 解析失败：%v；要么给它一段合法 JSON（对象、数组或带引号的字符串），要么把首字符改成别的按纯文本发\n%s",
					string(trimmed[0]), err, decideUsage)
			}
			return json.RawMessage(trimmed), nil
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

// decideMeta 是输出侧要知道的一个问题的名字与类型。--questions 给的整块 JSON 里没写 type 时
// 类型显示成 -，不影响答案：答案的形状由上游回什么决定。
type decideMeta struct {
	name string
	kind string
}

// decideTarget 是这次决策请求会落到哪个账号上，以及别名解析之后的模型名。
type decideTarget struct {
	model   string
	account string
}

// pickDecideTarget 按选号顺序找第一个「有效出站协议是 systemone」的候选账号。规则与网关的选号
// 一致（启用、冷却已过、模型列表覆盖该模型、priority 升序），但不带会话粘性：decide 一次只发
// 一条请求，没有会话可沿。有效协议只问 store.EffectiveProtocol，cli 不另算一份（见 DESIGN 5.13）。
//
// 三条失败各写清发生了什么、为什么、下一步敲什么：决策请求没有对话可以退化成别的协议，
// 「不是 systemone」只能靠声明一条路由或换一个模型解决。
func pickDecideTarget(st *store.Store, model string) (decideTarget, error) {
	resolved, isAlias, err := st.ResolveModelAlias(model)
	if err != nil {
		return decideTarget{}, fmt.Errorf("模型名 %s 解析不了（%v）：别名链成环或超过 8 跳时换句话说不出真名，选号无从谈起；用 switchboard model alias ls 看这条链，改掉或删掉它，也可以直接把 --model 换成真实模型名", model, err)
	}
	if !isAlias {
		// 不是别名时 ResolveModelAlias 回一个空串，名字照旧用命令行给的那个。
		resolved = model
	}

	accounts, err := st.ListAccounts()
	if err != nil {
		return decideTarget{}, err
	}
	now := time.Now()
	candidates := account.Candidates(accounts, resolved, now, "")
	if len(candidates) == 0 {
		if len(account.Providers(accounts, resolved)) == 0 {
			return decideTarget{}, fmt.Errorf("模型 %s 不在任何启用账号的模型列表里：决策请求按模型名选号，名字对不上就一个账号也选不出来；用 switchboard model ls 看现有清单，用 switchboard account add <名称> --provider <厂商> --key <key> --models %s 手工声明这个模型（决策模型不在上游清单里，只能手填），再 switchboard model probe --account <账号> 探活", resolved, resolved)
		}
		return decideTarget{}, fmt.Errorf("模型 %s 的账号都在冷却或被停用：这一次选不出账号；用 switchboard account ls 看状态，冷却几十秒后重试，被停用的用 switchboard account enable <账号> 启用", resolved)
	}

	// 网关遇到算不出协议或协议不相通的候选会跳过它看下一个（DESIGN 5.14 的协议互通），
	// 这里照同一个顺序找第一个走 systemone 的候选；firstProtocol 记第一个候选的协议，供文案说明。
	firstProtocol := ""
	for _, cand := range candidates {
		protocol, err := st.EffectiveProtocol(cand.ID, resolved)
		if err != nil {
			return decideTarget{}, fmt.Errorf("读账号 %s 模型 %s 的有效出站协议失败（%v）：配置读不出来就不猜协议；用 switchboard doctor 检查数据库", cand.Name, resolved, err)
		}
		if protocol == gateway.ProtocolSystemOne {
			return decideTarget{model: resolved, account: cand.Name}, nil
		}
		if firstProtocol == "" && protocol != "" {
			firstProtocol = protocol
		}
	}

	if firstProtocol == "" {
		// 算不出协议的两种成因要分开说：路由没声明、或者声明了但默认协议空（回退顺序只认
		// openai-chat → openai-responses → anthropic，见 DESIGN 5.8），两种下一步不一样。
		provider := candidates[0].Provider
		declared, err := st.ProviderProtocols(provider)
		if err != nil {
			return decideTarget{}, err
		}
		if !containsProtocol(declared, gateway.ProtocolSystemOne) {
			return decideTarget{}, fmt.Errorf("模型 %s 算不出有效出站协议：厂商 %s 没声明 systemone 路由，模型上也没有探出协议；先运行 switchboard provider set %s --systemone /v1/systemone 声明这条路由，再 switchboard model probe --account %s 把这个模型探出来", resolved, provider, provider, candidates[0].Name)
		}
		return decideTarget{}, fmt.Errorf("模型 %s 算不出有效出站协议：厂商 %s 声明了 systemone 路由，但默认协议是空的（回退顺序只认 openai-chat → openai-responses → anthropic，见 DESIGN 5.8）；运行 switchboard provider set %s --default-protocol systemone 把默认协议补上，或 switchboard model probe --account %s 把这个模型的协议探出来", resolved, provider, provider, candidates[0].Name)
	}
	return decideTarget{}, fmt.Errorf("模型 %s 以 %s 协议出站，不是 systemone：decide 只发 POST %s 的决策请求，出站协议跟着账号与厂商路由走（见 DESIGN 5.13），这条协议不参与转换；要给这家厂商声明一条 systemone 路由就运行 switchboard provider set %s --systemone /v1/systemone 再 switchboard model probe --account %s，要发对话请求就改用 switchboard call \"提示词\" --model %s，也可以换一个走 systemone 的模型（switchboard model ls 看清单）",
		resolved, firstProtocol, systemOnePath, candidates[0].Provider, candidates[0].Name, resolved)
}

// systemOneOnlyModel 判断这个模型的候选账号是否全部以 systemone 出站。call 发的是对话请求，
// 这种模型对它只会回 400（DESIGN 5.14 的协议互通），所以在发出去之前就拒绝。
// 一个候选都没有、或某一个候选走别的协议时返回假：那些情况由网关照常给出 404/503 或者照常转发。
func systemOneOnlyModel(st *store.Store, model string) (bool, error) {
	resolved, isAlias, err := st.ResolveModelAlias(model)
	if err != nil {
		// 别名链坏了不是这里要报的错：网关对这种情况回 400，让 call 照常发出去并显示那句文案。
		return false, nil
	}
	if !isAlias {
		resolved = model
	}
	accounts, err := st.ListAccounts()
	if err != nil {
		return false, err
	}
	candidates := account.Candidates(accounts, resolved, time.Now(), "")
	if len(candidates) == 0 {
		return false, nil
	}
	for _, cand := range candidates {
		protocol, err := st.EffectiveProtocol(cand.ID, resolved)
		if err != nil {
			return false, err
		}
		if protocol != gateway.ProtocolSystemOne {
			return false, nil
		}
	}
	return true, nil
}

// containsProtocol 判断协议清单里有没有这一条。厂商路由清单来自 store，cli 不另算一份。
func containsProtocol(protocols []string, want string) bool {
	for _, p := range protocols {
		if p == want {
			return true
		}
	}
	return false
}

// decideResponse 是上游回包里本命令要用的两段：answers 与 usage 原样带走（--json 直接输出），
// 人读输出另外按问题拆开。
type decideResponse struct {
	Answers json.RawMessage `json:"answers"`
	Usage   json.RawMessage `json:"usage"`
}

// decideAnswer 是上游给一个问题的答案：noul 给 probability，choice 与 score 给 confidence
// （DESIGN 第 4 节的 decide 一行）。两个字段都按可选处理，上游给了哪个就显示哪个。
type decideAnswer struct {
	Answer      json.RawMessage `json:"answer"`
	Probability *float64        `json:"probability"`
	Confidence  *float64        `json:"confidence"`
}

// cmdDecide 是 systemone 决策请求的入口：把 state 与问题组发给本进程，打印每个问题的答案与概率。
// 起网关的方式与 call 一致（起一个只监听回环地址的临时网关），不另写一套连接方式。
func cmdDecide(e *Env, args []string) error {
	fs := newFlagSet(e, "decide", "发一条 systemone 决策请求：一个 state 加一组问题，打印每个问题的答案与概率")
	model := fs.String("model", "", "模型名：有效出站协议必须是 systemone（别名先按 model alias 解析）")
	state := fs.String("state", "", "决策要看的材料：纯文本按字符串发；首字符是 {、[ 或 \" 时按 JSON 解析")
	questionsJSON := fs.String("questions", "", "整块 questions 对象（JSON）：给了它就不再拼问题，与 --noul/--choice/--score 互斥")
	var noul, choice, score repeatedFlag
	fs.Var(&noul, "noul", "拼一个是非题：`名=问句`，可重复")
	fs.Var(&choice, "choice", "选择题：`名=问句` 声明，再用 名:选项[,选项...] 追加选项（每个选项写 key 或 key=说明，至少两个），可重复")
	fs.Var(&score, "score", "打分题：`名=问句` 声明，再用 名:级别[,级别...] 追加级别（至少两级，按从低到高写），可重复")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("多余的参数 %q：decide 的输入都走参数，--state 给材料、--noul/--choice/--score 或 --questions 给问题\n%s", fs.Arg(0), decideUsage)
	}
	if strings.TrimSpace(*model) == "" {
		return usagef("缺少 --model：决策请求按模型名选号，模型的有效出站协议必须是 systemone；用 switchboard model ls 看哪些模型可用\n%s", decideUsage)
	}
	if strings.TrimSpace(*state) == "" {
		return usagef("缺少 --state：决策要看的材料就是它，纯文本与 JSON 都收（首字符是 {、[ 或 \" 时按 JSON 解析）；内容为空时给一对引号 --state '\"\"'\n%s", decideUsage)
	}

	// 两种问题来源互斥：同时给时谁生效都说不清，宁可停下来让人删掉一组。
	flagsGiven := len(noul.values) + len(choice.values) + len(score.values)
	if flagWasSet(fs, "questions") && flagsGiven > 0 {
		return usagef("--questions 与 --noul/--choice/--score 同时给了：前者是整块 questions 对象、后者逐条拼问题，两种写法只留一种\n%s", decideUsage)
	}

	stateRaw, err := encodeDecideState(*state)
	if err != nil {
		return err
	}

	var questionsRaw json.RawMessage
	var metas []decideMeta
	if flagWasSet(fs, "questions") {
		if questionsRaw, metas, err = decodeQuestionsJSON(*questionsJSON); err != nil {
			return err
		}
	} else {
		builder := newDecideQuestions()
		for _, group := range []struct {
			kind   string
			values []string
		}{{"noul", noul.values}, {"choice", choice.values}, {"score", score.values}} {
			for _, raw := range group.values {
				if err := builder.add(group.kind, raw); err != nil {
					return err
				}
			}
		}
		questions, err := builder.list()
		if err != nil {
			return err
		}
		if questionsRaw, err = encodeQuestions(questions); err != nil {
			return err
		}
		metas = make([]decideMeta, 0, len(questions))
		for _, question := range questions {
			metas = append(metas, decideMeta{name: question.name, kind: question.kind})
		}
	}

	// 用法错误到这里判完：一条写歪的命令不该读库，更不该发上游。
	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	target, err := pickDecideTarget(a.st, *model)
	if err != nil {
		return err
	}

	body, err := json.Marshal(decideRequestBody{Model: target.model, State: stateRaw, Questions: questionsRaw})
	if err != nil {
		return err
	}

	// 日志一律不写：网关每转发一次就打一行「账号 协议 模型 …」，而 decide 自己已经把这几个事实
	// 印在首行上了；call 也是这么做的。真的是失败时（写请求记录失败一类）靠 log tail 查。
	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: func(string, ...any) {}})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gs.Serve(ctx, ln) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+systemOnePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("连本进程的网关失败：%v；网关起不来时先运行 switchboard doctor 看端口与数据库", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读网关响应失败：%v；用 switchboard log tail 看这一次的记录", err)
	}
	latency := time.Since(started)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decideHTTPError(resp.StatusCode, raw, target.account)
	}

	var env decideResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("响应不是合法 JSON：%v；上游回来的可能是 HTML 错误页，用 switchboard log tail 看这一次的记录，或 switchboard account test %s 核对地址与凭据", err, target.account)
	}

	if e.JSON {
		// answers 与 usage 原样输出（不重新编码，上游加了字段也不会被吞），另加三个这次请求的事实字段。
		return e.printJSON(map[string]any{
			"model":      target.model,
			"account":    target.account,
			"latency_ms": latency.Milliseconds(),
			"answers":    env.Answers,
			"usage":      env.Usage,
		})
	}

	var answers map[string]decideAnswer
	if len(env.Answers) > 0 {
		if err := json.Unmarshal(env.Answers, &answers); err != nil {
			return fmt.Errorf("响应里的 answers 不是对象：%v；用 switchboard log tail 看这一次的记录，或 switchboard account test %s 核对上游", err, target.account)
		}
	}

	fmt.Fprintf(e.Stdout, "模型 %s（账号 %s，systemone，耗时 %s）\n",
		target.model, target.account, latency.Round(time.Millisecond))

	rows := make([][]string, 0, len(metas))
	for _, meta := range metas {
		answer, ok := answers[meta.name]
		kind := meta.kind
		if kind == "" {
			kind = "-"
		}
		if !ok {
			// 上游没给这个问题的答案：照样占一行，缺的是哪一个一眼可见，不用回头数行。
			rows = append(rows, []string{meta.name, kind, "（上游没有给答案）", "-", "-"})
			continue
		}
		rows = append(rows, []string{
			meta.name, kind, decideAnswerCell(answer.Answer),
			decideMetricCell(answer.Probability), decideMetricCell(answer.Confidence),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"名称", "类型", "答案", "概率", "置信度"}, rows))
	fmt.Fprintln(e.Stdout, decideUsageLine(env.Usage))
	return nil
}

// decideRequestBody 是发往 POST /v1/systemone 的请求体：这里只放这一段协议的三个必有字段，
// assets 一类本命令产不出来的键不伪造（见 DESIGN 5.14 的请求体一行）。
type decideRequestBody struct {
	Model     string          `json:"model"`
	State     json.RawMessage `json:"state"`
	Questions json.RawMessage `json:"questions"`
}

// decideHTTPError 把非 2xx 的响应变成一条错误（退出码 1）。文案取 detail.message：
// 这条协议的错误体是 {"detail":{"error_type":…,"message":…}}（DESIGN 5.2 与 5.14），
// 两个字段名都认（`error_type` 与早期文档里的 `type`）；其它形状退回响应原文。
func decideHTTPError(status int, raw []byte, accountName string) error {
	var env struct {
		Detail struct {
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
			Type      string `json:"type"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(raw, &env)

	message := strings.TrimSpace(env.Detail.Message)
	kind := strings.TrimSpace(env.Detail.ErrorType)
	if kind == "" {
		kind = strings.TrimSpace(env.Detail.Type)
	}
	switch {
	case message == "":
		// 没有 detail.message 说明回来的不是这条协议的错误体，原文是唯一的事实。
		message = strings.TrimSpace(string(raw))
		if message == "" {
			message = "响应体是空的"
		}
		kind = "错误体里没有 detail.message"
	case kind == "":
		kind = "错误体里没有类型"
	}
	return fmt.Errorf("网关返回 %d（%s）：%s；这一次的请求记录在 switchboard log tail 里，地址与凭据用 switchboard account test %s 核对", status, kind, message, accountName)
}

// decideAnswerCell 把上游的 answer 原样写成文本：字符串去掉引号，布尔与数字原文，
// 对象与数组按 JSON 原文（这条协议的答案也可能是结构化的）。
func decideAnswerCell(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(bytes.TrimSpace(raw))
}

// decideMetricCell 是概率与置信度两列：没给就写 -，给了原样印出来（不补零、不四舍五入）。
func decideMetricCell(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

// decideUsageLine 是最后一行用量：这条协议只有输入与输出两个数（DESIGN 5.14），
// 上游没给全时说明白缺的是哪个，不留一行 0。
func decideUsageLine(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "用量 上游没有给出"
	}
	var usage struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil {
		return "用量 上游没有给出"
	}
	in, out := "-", "-"
	if usage.InputTokens != nil {
		in = strconv.FormatInt(*usage.InputTokens, 10)
	}
	if usage.OutputTokens != nil {
		out = strconv.FormatInt(*usage.OutputTokens, 10)
	}
	return fmt.Sprintf("用量 输入 %s / 输出 %s", in, out)
}
