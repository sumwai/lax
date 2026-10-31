package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// 这个文件钉住 systemone 的命令面：
// `provider add/set --systemone`、`provider unset systemone`、`--default-protocol systemone`、
// `lax model decide` 的三种拼法与错误，以及 `model call` 撞上 systemone 模型时的拒绝。

// systemOneDecideOK 是假决策上游的正常响应：三个问题的答案加一行用量。答案按真实形状写：
// 结论平铺在顶层（noul / choice / score），不共用一个 answer 字段：三种问题的结论形状不同，
// 共用一个字段就得再套一层类型判断。
const systemOneDecideOK = `{"answers":{` +
	`"present":{"type":"noul","noul":0.99},` +
	`"mood":{"type":"choice","choice":"billing","confidence":0.87,"probabilities":{"billing":0.9,"other":0.1}},` +
	`"anger":{"type":"score","score":1.4,"confidence":0.61,"legend":{"0":"calm","1":"mild","2":"angry"},` +
	`"probabilities":{"0":0.05,"1":0.34,"2":0.61}}},` +
	`"usage":{"input_tokens":11,"output_tokens":4}}`

// decideUpstream 是只说 systemone 的假上游：POST /v1/systemone 回一条固定响应，别的路径回 404。
func decideUpstream(t *testing.T) *recorderUpstream {
	t.Helper()
	return newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/systemone": {status: 200, body: systemOneDecideOK},
	})
}

// systemOneAccount 建一个走 systemone 的账号：厂商指向假上游，默认协议显式给 systemone，
// 模型名带一个 /（TypeSafe 的决策模型长这样）。
func systemOneAccount(t *testing.T, up *recorderUpstream) {
	t.Helper()
	mustInit(t)
	if code, _, stderr := addAccount(t, "ts1", "typesafe", up.srv.URL, "systemone",
		"--key", "sk-test", "--models", "typesafe/jev"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}
}

// tableRow 在一条人读表格里找第一列等于 name 的那一行，按空白拆成字段。
// 单元格里没有空格（答案与数字都是单词），所以 strings.Fields 就是分列。
func tableRow(t *testing.T, out, name string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return fields
		}
	}
	t.Fatalf("输出里没有 %q 那一行: %s", name, out)
	return nil
}

// TestProviderSystemOneRoute 钉住 --systemone 这条路：相对路径与整条地址两种写法都收，
// provider ls 的 --json 与文本都带出它，--default-protocol systemone 与线协议全名同形。
func TestProviderSystemOneRoute(t *testing.T) {
	sandbox(t)
	mustInit(t)

	// 固定回退顺序只认 openai-chat → openai-responses → anthropic，所以只声明
	// systemone 一条路由时默认协议要显式给；不给时它就是空的（provider ls 显示 -）。
	code, out, stderr := run(t, "provider", "add", "typesafe",
		"--base-url", "https://api.typesafe.ai", "--systemone", "/v1/systemone")
	if code != 0 {
		t.Fatalf("provider add --systemone 退出码 = %d，stderr = %s", code, stderr)
	}
	if want := "已添加厂商 typesafe（https://api.typesafe.ai），1 条路由，默认协议 -"; !strings.Contains(out, want) {
		t.Fatalf("provider add --systemone 的输出 = %s，期望包含 %q", out, want)
	}

	// provider set 把默认协议补成 systemone：与线协议全名同形，一行就够。
	if code, _, stderr := run(t, "provider", "set", "typesafe", "--default-protocol", "systemone"); code != 0 {
		t.Fatalf("provider set --default-protocol systemone 退出码 = %d，stderr = %s", code, stderr)
	}

	added := mustProviderLs(t)["typesafe"]
	if added.DefaultProtocol != "systemone" {
		t.Fatalf("默认协议 = %q，期望 systemone", added.DefaultProtocol)
	}
	route, ok := routeMap(added)["systemone"]
	if !ok {
		t.Fatalf("provider ls --json 没带出 systemone 路由: %+v", added.Protocols)
	}
	// 相对路径由 store 规整成带前导斜杠的形态；systemone 没有自己的列表接口，推导不出 models_path。
	if route.Path != "/v1/systemone" || route.ModelsPath != "" {
		t.Fatalf("systemone 路由 = %+v，期望 path=/v1/systemone 且没有 models_path", route)
	}

	code, out, stderr = run(t, "provider", "ls")
	if code != 0 {
		t.Fatalf("provider ls 退出码 = %d，stderr = %s", code, stderr)
	}
	if got := tableRow(t, out, "typesafe"); !strings.Contains(strings.Join(got, " "), "systemone /v1/systemone") {
		t.Fatalf("provider ls 的路由列应显示 systemone 路由: %v", got)
	}

	// 整条地址的写法：与相对路径走同一套校验与规整。
	code, _, stderr = run(t, "provider", "add", "typesafe-full",
		"--systemone", "https://api.typesafe.ai/v1/systemone", "--default-protocol", "systemone")
	if code != 0 {
		t.Fatalf("provider add --systemone 整条地址退出码 = %d，stderr = %s", code, stderr)
	}
	if got := routeMap(mustProviderLs(t)["typesafe-full"])["systemone"]; got.Path != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("整条地址的 systemone 路由 = %+v", got)
	}

	// 给了 --systemone 但值是空串：改值走 set、删路由走 unset，空值是用法错误。
	code, _, stderr = run(t, "provider", "add", "empty-one", "--base-url", "https://api.example.com", "--systemone", " ")
	if code != 2 {
		t.Fatalf("空 --systemone 退出码 = %d，期望 2（用法错误）", code)
	}
	if !strings.Contains(stderr, "provider unset") {
		t.Fatalf("空值文案应指向 provider unset: %s", stderr)
	}

	// 整条地址缺主机名：scheme 与主机名的校验对第五条协议同样生效。
	code, _, stderr = run(t, "provider", "add", "bad-one", "--systemone", "https:///v1/systemone")
	if code != 2 {
		t.Fatalf("坏整条地址退出码 = %d，期望 2（用法错误）", code)
	}
	if !strings.Contains(stderr, "主机名") {
		t.Fatalf("坏地址文案应说明缺主机名: %s", stderr)
	}
}

// TestProviderSystemOneHelp 钉住 provider add --help 里的 systemone 示例与默认协议提示。
func TestProviderSystemOneHelp(t *testing.T) {
	sandbox(t)

	code, _, errOut := run(t, "provider", "add", "--help")
	if code != 0 {
		t.Fatalf("provider add --help 的退出码 = %d", code)
	}
	for _, want := range []string{
		"--systemone",
		"/v1/systemone",
		"lax provider add typesafe --base-url https://api.typesafe.ai --systemone /v1/systemone",
		"--default-protocol systemone",
		"anthropic、openai、responses、gemini、systemone",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("provider add --help 应提到 %q: %s", want, errOut)
		}
	}
}

// TestProviderSystemOneUnset 钉住 `provider unset systemone` 与默认协议的两条交叉规则：
// 默认协议不是它时删得掉；删掉的正好是默认协议时整笔拒绝。
func TestProviderSystemOneUnset(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "ts",
		"--base-url", "https://api.typesafe.ai",
		"--openai", "/v1/chat/completions", "--systemone", "/v1/systemone",
		"--default-protocol", "openai"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}

	// 默认协议是 openai-chat，删 systemone 不碰默认协议。
	code, out, stderr := run(t, "provider", "unset", "ts", "systemone")
	if code != 0 {
		t.Fatalf("provider unset systemone 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"ts 已清掉 systemone 路由", "现有 1 条路由，默认协议 openai-chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider unset systemone 的输出缺 %q: %s", want, out)
		}
	}
	if _, ok := routeMap(mustProviderLs(t)["ts"])["systemone"]; ok {
		t.Fatal("unset 之后 systemone 路由应该不在了")
	}

	// 幂等：再删一次不报错，只说明本来就没有。
	code, out, stderr = run(t, "provider", "unset", "ts", "systemone")
	if code != 0 || !strings.Contains(out, "ts 本来就没有 systemone 路由") {
		t.Fatalf("幂等 unset 退出码 = %d，输出 = %s，stderr = %s", code, out, stderr)
	}

	// 默认协议换成 systemone 之后再删它：整笔拒绝，提示先改默认协议。
	if code, _, stderr := run(t, "provider", "set", "ts", "--systemone", "/v1/systemone", "--default-protocol", "systemone"); code != 0 {
		t.Fatalf("provider set --systemone --default-protocol systemone 退出码 = %d，stderr = %s", code, stderr)
	}
	code, out, stderr = run(t, "provider", "unset", "ts", "systemone")
	if code != 1 {
		t.Fatalf("删默认协议那条路由的退出码 = %d，期望 1（运行失败，store 整笔拒绝）", code)
	}
	if out != "" {
		t.Fatalf("被拒绝时不该写 stdout: %s", out)
	}
	if !strings.Contains(stderr, "default-protocol") {
		t.Fatalf("拒绝文案应指向 --default-protocol: %s", stderr)
	}
	if _, ok := routeMap(mustProviderLs(t)["ts"])["systemone"]; !ok {
		t.Fatal("被拒绝之后 systemone 路由应该还在")
	}
}

// TestDecideNoulQuestions 钉住 decide 的人读输出：每个问题一行（名字、类型、答案），
// noul 给概率、choice 与 score 给置信度，最后一行是用量。
func TestDecideNoulQuestions(t *testing.T) {
	sandbox(t)
	up := decideUpstream(t)
	systemOneAccount(t, up)

	code, out, stderr := run(t, "model", "decide", "--model", "typesafe/jev", "--state", "hello there",
		"--noul", "present=Is this text present?",
		"--choice", "mood=Which topic is it?", "--choice", "mood:billing=Charges and payments,other",
		"--score", "anger=How angry is it?", "--score", "anger:calm,mild,angry")
	if code != 0 {
		t.Fatalf("decide 退出码 = %d，stderr = %s", code, stderr)
	}

	if !strings.Contains(out, "模型 typesafe/jev（账号 ts1，systemone，耗时 ") {
		t.Fatalf("decide 的首行应写模型、账号与耗时: %s", out)
	}
	for _, want := range [][]string{
		{"present", "noul", "-", "0.99", "-"},
		{"mood", "choice", "billing", "0.9", "0.87"},
		{"anger", "score", "1.4（mild）", "0.34", "0.61"},
	} {
		if got := tableRow(t, out, want[0]); !equalFields(got, want) {
			t.Fatalf("表格行 %v，期望 %v（整份输出：%s）", got, want, out)
		}
	}
	if !strings.Contains(out, "用量 输入 11 / 输出 4") {
		t.Fatalf("输出末尾应有用量行: %s", out)
	}

	// 上游收到的是一条 systemone 请求：路径、模型名、state 与三种问题的形状都按那条协议原样转发。
	call := up.mustSingleCall(t)
	if call.path != "/v1/systemone" || call.bearer != "Bearer sk-test" {
		t.Fatalf("上游调用 = %+v，期望 POST /v1/systemone 且带 Bearer 凭据", call)
	}
	var body struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(call.body), &body); err != nil {
		t.Fatalf("上游收到的请求体不是 JSON: %v，%s", err, call.body)
	}
	if body.Model != "typesafe/jev" || string(body.State) != `"hello there"` {
		t.Fatalf("请求体 = %+v，期望模型名与纯文本 state", body)
	}
	if len(body.Questions) != 3 {
		t.Fatalf("问题数 = %d，期望 3: %s", len(body.Questions), call.body)
	}
	if got := body.Questions["mood"]; got.Type != "choice" || string(got.Criteria) != `{"billing":"Charges and payments","other":null}` {
		t.Fatalf("choice 问题 = %+v，期望 criteria 是「选项 → 说明」对象", got)
	}
	if got := body.Questions["anger"]; got.Type != "score" || string(got.Criteria) != `["calm","mild","angry"]` {
		t.Fatalf("score 问题 = %+v，期望 criteria 是有序的级别数组", got)
	}
	if got := body.Questions["present"]; got.Type != "noul" || len(got.Criteria) != 0 {
		t.Fatalf("noul 问题 = %+v，期望没有 criteria", got)
	}
}

// TestDecideQuestionsJSON 钉住 --questions 这条路：整块对象原样转发，输出按名字升序
// （JSON 对象没有顺序，两次同样的命令要给出同样的行序），state 首字符是 { 时按 JSON 发。
func TestDecideQuestionsJSON(t *testing.T) {
	sandbox(t)
	up := decideUpstream(t)
	systemOneAccount(t, up)

	block := `{"mood":{"type":"choice","instructions":"Which topic?","criteria":{"billing":"Charges","other":null}},` +
		`"present":{"type":"noul","instructions":"Is this text present?"}}`
	code, out, stderr := run(t, "model", "decide", "--model", "typesafe/jev",
		"--state", `{"text":"hello","n":3}`, "--questions", block)
	if code != 0 {
		t.Fatalf("decide --questions 退出码 = %d，stderr = %s", code, stderr)
	}
	// 名字升序：mood 在 present 之前。
	if strings.Index(out, "mood") > strings.Index(out, "present") {
		t.Fatalf("--questions 的输出应按名字升序: %s", out)
	}
	if got := tableRow(t, out, "mood"); !equalFields(got, []string{"mood", "choice", "billing", "0.9", "0.87"}) {
		t.Fatalf("mood 行 = %v", got)
	}

	call := up.mustSingleCall(t)
	var body struct {
		State     json.RawMessage `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal([]byte(call.body), &body); err != nil {
		t.Fatalf("上游收到的请求体不是 JSON: %v", err)
	}
	if string(body.State) != `{"text":"hello","n":3}` {
		t.Fatalf("state = %s，期望首字符是 { 的原样 JSON", body.State)
	}
	if string(body.Questions) != block {
		t.Fatalf("questions = %s，期望整块原样转发", body.Questions)
	}
}

// TestDecideJSONOutput 钉住 --json 的字段：上游的 answers 与 usage 原样，另加 model、account、
// latency_ms。
func TestDecideJSONOutput(t *testing.T) {
	sandbox(t)
	up := decideUpstream(t)
	systemOneAccount(t, up)

	code, out, stderr := run(t, "model", "decide", "--model", "typesafe/jev", "--state", "hi",
		"--noul", "present=Is this text present?", "--json")
	if code != 0 {
		t.Fatalf("decide --json 退出码 = %d，stderr = %s", code, stderr)
	}

	var payload struct {
		Model     string                     `json:"model"`
		Account   string                     `json:"account"`
		LatencyMS *int64                     `json:"latency_ms"`
		Answers   map[string]map[string]any  `json:"answers"`
		Usage     map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json 输出不是合法 JSON: %v，%s", err, out)
	}
	if payload.Model != "typesafe/jev" || payload.Account != "ts1" {
		t.Fatalf("--json 的模型与账号 = %q / %q", payload.Model, payload.Account)
	}
	if payload.LatencyMS == nil || *payload.LatencyMS < 0 {
		t.Fatalf("--json 应带非负的 latency_ms: %s", out)
	}
	if got := payload.Answers["present"]["noul"]; got != 0.99 {
		t.Fatalf("answers.present.noul = %v，期望 0.99", got)
	}
	if got := payload.Answers["mood"]["choice"]; got != "billing" {
		t.Fatalf("answers 应原样带出每一个问题: %+v", payload.Answers)
	}
	if _, ok := payload.Usage["input_tokens"]; !ok {
		t.Fatalf("--json 应原样带出 usage: %s", out)
	}
}

// TestDecideUsageErrors 覆盖 decide 的用法错误（退出码 2）：来源互斥、一个问题都没有、
// 判据数量不足、缺参数、state 不是合法 JSON、多余的位置参数。
func TestDecideUsageErrors(t *testing.T) {
	sandbox(t)
	mustInit(t)

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"两种来源同时给", []string{"--model", "m", "--state", "s", "--questions", `{"q":{"type":"noul","instructions":"x"}}`, "--noul", "q=x"},
			[]string{"--questions", "--noul", "只留一种"}},
		{"一个问题都没有", []string{"--model", "m", "--state", "s"},
			[]string{"一个问题都没拼出来", "--noul 名=问句"}},
		{"score 只有一级", []string{"--model", "m", "--state", "s", "--score", "q=x", "--score", "q:low"},
			[]string{"打分题 q", "至少要两级", "--score q:级别1,级别2"}},
		{"choice 只有一个选项", []string{"--model", "m", "--state", "s", "--choice", "q=x", "--choice", "q:a"},
			[]string{"选择题 q", "至少要两个选项"}},
		{"score 的级别带说明", []string{"--model", "m", "--state", "s", "--score", "q=x", "--score", "q:low=温和"},
			[]string{"级别", "只有名字"}},
		{"缺 --model", []string{"--state", "s", "--noul", "q=x"}, []string{"缺少 --model"}},
		{"缺 --state", []string{"--model", "m", "--noul", "q=x"}, []string{"缺少 --state"}},
		{"state 不是合法 JSON", []string{"--model", "m", "--state", `{"a":`, "--noul", "q=x"},
			[]string{"--state", "JSON"}},
		{"问句为空", []string{"--model", "m", "--state", "s", "--noul", "q="}, []string{"少了问句"}},
		{"判据没声明", []string{"--model", "m", "--state", "s", "--choice", "q:a,b"}, []string{"还没声明"}},
		{"多余的位置参数", []string{"--model", "m", "--state", "s", "--noul", "q=x", "多余"},
			[]string{"多余的参数", "多余"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, stderr := run(t, append([]string{"model", "decide"}, c.args...)...)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2（用法错误），stderr = %s", code, stderr)
			}
			if out != "" {
				t.Fatalf("用法错误不该写 stdout: %s", out)
			}
			for _, want := range c.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr 应提到 %q: %s", want, stderr)
				}
			}
			// 用法错误要打印用法行，不能只给一句「参数错误」。
			if !strings.Contains(stderr, "用法：lax model decide") {
				t.Fatalf("用法错误应打印用法行: %s", stderr)
			}
		})
	}
}

// TestDecideRejectsNonSystemOneModel 钉住「有效出站协议不是 systemone」：退出码 1，
// 文案说明它以哪条协议出站并给出下一步。
func TestDecideRejectsNonSystemOneModel(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: 200, body: chatCompletionOK},
	})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test", "--models", "m1"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "decide", "--model", "m1", "--state", "s", "--noul", "q=x")
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（运行失败）", code)
	}
	if out != "" {
		t.Fatalf("失败时不该写 stdout: %s", out)
	}
	for _, want := range []string{"m1", "openai-chat", "不是 systemone", "--systemone /v1/systemone", "lax model call"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
	// 一个字节都没发出去：协议不对是本地就能判掉的事。
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("不该有上游调用: %+v", calls)
	}
}

// TestDecideModelNotFound 钉住找不到模型的文案：决策模型只能手工声明，下一步要指得出来。
func TestDecideModelNotFound(t *testing.T) {
	sandbox(t)
	mustInit(t)

	code, _, stderr := run(t, "model", "decide", "--model", "typesafe/jev", "--state", "s", "--noul", "q=x")
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（运行失败）", code)
	}
	for _, want := range []string{"不在任何启用账号的模型列表里", "lax account add", "lax model add typesafe/jev --account", "--protocol systemone", "lax model probe"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
}

// TestDecideMissingModelPointsToModelAdd 钉住「有账号但没有这个模型」时的下一步：
// 手写一条可达性走 model add（同时写目录与账号级协议），不再让人重跑 account add --models；
// 只启用一个账号时命令里直接写出账号名，照着拄就行。
func TestDecideMissingModelPointsToModelAdd(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/chat/completions": {status: 200, body: chatCompletionOK},
	})
	mustInit(t)
	if code, _, stderr := addAccount(t, "ds1", "custom", up.srv.URL, "", "--key", "sk-test"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, out, stderr := run(t, "model", "decide", "--model", "foo", "--state", "x", "--noul", "n=Is this present?")
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（运行失败）", code)
	}
	if out != "" {
		t.Fatalf("失败时不该写 stdout: %s", out)
	}
	for _, want := range []string{
		"不在任何启用账号的模型列表里",
		"lax model add foo --account ds1 --protocol systemone",
		"lax model probe --account ds1 --model foo --protocol systemone",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
	// 本地就能判掉的配置问题，一条上游请求都不发。
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("不该有上游调用: %+v", calls)
	}
}

// TestDecideHTTPErrorUsesDetailMessage 钉住错误体：非 2xx 时取 detail.message 作为文案，
// 退出码 1（这条协议的客户端直接读上游那一层错误，不做二次包装）。
func TestDecideHTTPErrorUsesDetailMessage(t *testing.T) {
	sandbox(t)
	up := newRecorderUpstream(t, map[string]fakeResponse{
		"POST /v1/systemone": {status: 401, body: `{"detail":{"message":"凭据不对","error_type":"authentication_error"}}`},
	})
	systemOneAccount(t, up)

	code, out, stderr := run(t, "model", "decide", "--model", "typesafe/jev", "--state", "s", "--noul", "q=x")
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（非 2xx 是运行失败）", code)
	}
	if out != "" {
		t.Fatalf("失败时不该写 stdout: %s", out)
	}
	for _, want := range []string{"401", "authentication_error", "凭据不对", "lax log tail"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
}

// TestCallRefusesSystemOne 钉住 model call 的两条拒绝：--protocol systemone 是这一条命令接不了的协议，
// 模型的出站协议是 systemone 时同样接不了。两者都是退出码 1，文案指向 lax model decide。
func TestCallRefusesSystemOne(t *testing.T) {
	sandbox(t)
	up := decideUpstream(t)
	systemOneAccount(t, up)

	code, out, stderr := run(t, "model", "call", "hi", "--model", "typesafe/jev", "--protocol", "systemone")
	if code != 1 {
		t.Fatalf("call --protocol systemone 退出码 = %d，期望 1", code)
	}
	if out != "" {
		t.Fatalf("失败时不该写 stdout: %s", out)
	}
	if !strings.Contains(stderr, "lax model decide") {
		t.Fatalf("文案应指向 lax model decide: %s", stderr)
	}

	// 模型的出站协议是 systemone：不提 --protocol 也一样拒绝。
	code, _, stderr = run(t, "model", "call", "hi", "--model", "typesafe/jev")
	if code != 1 {
		t.Fatalf("call 撞上 systemone 模型退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "systemone") || !strings.Contains(stderr, "lax model decide") {
		t.Fatalf("文案应说明出站协议是 systemone 并指向 decide: %s", stderr)
	}

	// 一次上游调用都没有：决策端点不该被打成对话请求。
	if calls := up.all(); len(calls) != 0 {
		t.Fatalf("不该有上游调用: %+v", calls)
	}
}

// equalFields 比较两个字段切片。
func equalFields(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestDecideWithoutDefaultProtocol 钉住「路由声明了但默认协议是空的」这一条：固定回退顺序只认
// openai-chat → openai-responses → anthropic，所以只声明 systemone 一条路由时
// 厂商算不出默认协议，decide 的文案要指出补 --default-protocol，而不是让人去重复声明路由。
func TestDecideWithoutDefaultProtocol(t *testing.T) {
	sandbox(t)
	mustInit(t)

	if code, _, stderr := run(t, "provider", "add", "ts2",
		"--base-url", "https://api.typesafe.ai", "--systemone", "/v1/systemone"); code != 0 {
		t.Fatalf("provider add 退出码 = %d，stderr = %s", code, stderr)
	}
	if code, _, stderr := run(t, "account", "add", "ts2a", "--provider", "ts2", "--key", "sk-1",
		"--models", "pages/jev"); code != 0 {
		t.Fatalf("account add 退出码 = %d，stderr = %s", code, stderr)
	}

	code, _, stderr := run(t, "model", "decide", "--model", "pages/jev", "--state", "s", "--noul", "q=x")
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（运行失败）", code)
	}
	for _, want := range []string{"算不出有效出站协议", "--default-protocol systemone"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr 应提到 %q: %s", want, stderr)
		}
	}
}
