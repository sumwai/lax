package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"switchboard/internal/gateway"
	"switchboard/internal/store"
	"switchboard/internal/translate"
	"switchboard/internal/usage"
)

func cmdUsage(e *Env, args []string) error {
	fs := newFlagSet(e, "usage", "按账号、模型或日期汇总用量")
	since := fs.String("since", "7d", "时间范围，如 24h、7d、3w")
	by := fs.String("by", "account", "维度：account | model | day")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch *by {
	case "account", "model", "day":
	default:
		return usagef("不支持的维度 %q，可用 account、model、day", *by)
	}

	sinceTS, err := usage.ParseSince(*since, time.Now())
	if err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	rows, err := a.st.Usage(*by, sinceTS)
	if err != nil {
		return err
	}

	if e.JSON {
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"key":              r.Key,
				"requests":         r.Requests,
				"in_tokens":        r.InTokens,
				"out_tokens":       r.OutTokens,
				"reasoning_tokens": r.ReasoningTokens,
				"cache_read":       r.CacheRead,
				"cache_write":      r.CacheWrite,
				"cost":             r.Cost,
			})
		}
		return e.printJSON(out)
	}

	if len(rows) == 0 {
		fmt.Fprintf(e.Stdout, "最近 %s 没有请求记录\n", *since)
		return nil
	}

	headerName := map[string]string{"account": "账号", "model": "模型", "day": "日期"}[*by]
	table := make([][]string, 0, len(rows))
	var totalReq, totalIn, totalOut, totalReasoning, totalCacheRead, totalCacheWrite int64
	var totalCost float64
	for _, r := range rows {
		table = append(table, []string{
			r.Key,
			fmt.Sprintf("%d", r.Requests),
			fmt.Sprintf("%d", r.InTokens),
			fmt.Sprintf("%d", r.OutTokens),
			fmt.Sprintf("%d", r.ReasoningTokens),
			fmt.Sprintf("%d", r.CacheRead),
			fmt.Sprintf("%d", r.CacheWrite),
			usage.YuanOrUnpriced(r.Cost),
		})
		totalReq += r.Requests
		totalIn += r.InTokens
		totalOut += r.OutTokens
		totalReasoning += r.ReasoningTokens
		totalCacheRead += r.CacheRead
		totalCacheWrite += r.CacheWrite
		// Cost 为 nil 表示这一组未定价（缺价），合计里不当 0 累加。
		if r.Cost != nil {
			totalCost += *r.Cost
		}
	}
	table = append(table, []string{
		"合计",
		fmt.Sprintf("%d", totalReq),
		fmt.Sprintf("%d", totalIn),
		fmt.Sprintf("%d", totalOut),
		fmt.Sprintf("%d", totalReasoning),
		fmt.Sprintf("%d", totalCacheRead),
		fmt.Sprintf("%d", totalCacheWrite),
		usage.Yuan(totalCost),
	})
	fmt.Fprint(e.Stdout, renderTable(
		[]string{headerName, "请求", "输入 token", "输出 token", "思考 token", "缓存读 token", "缓存写 token", "估算花费"}, table))
	if totalCost == 0 {
		fmt.Fprintln(e.Stdout, "花费为 0：账号没定价，加账号时给 --price-in 与 --price-out（缓存单价可选，未填时回退输入价）")
	}
	if totalCacheRead+totalCacheWrite > 0 {
		// 缓存 token 已经折进花费，不说清就会被当成只显示；口径与 DESIGN 5.5 的公式一致。
		fmt.Fprintln(e.Stdout, "缓存读写已按账号的缓存单价计入估算花费，未填缓存单价时按输入价算")
	}
	return nil
}

// resolveInboundProtocol 把命令行上的简写换回协议名。四条线协议都能当入站用
// （gemini 的两条入站路径见 DESIGN 5.9）。
func resolveInboundProtocol(name string) (string, bool) {
	switch name {
	case "", translate.ProtocolAnthropic:
		return translate.ProtocolAnthropic, true
	case "chat", translate.ProtocolChat:
		return translate.ProtocolChat, true
	case "responses", translate.ProtocolResponses:
		return translate.ProtocolResponses, true
	case translate.ProtocolGemini:
		return translate.ProtocolGemini, true
	default:
		return "", false
	}
}

// logSubs 是 log 一组的子命令清单。
var logSubs = []groupItem{
	{"tail", "查看最近的请求记录，-n 改条数，--follow 跟随新记录"},
}

func cmdLog(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "log", "查看请求记录", logSubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：tail\n用 switchboard log --help 查看子命令")
	}
	if args[0] != "tail" {
		return usagef("未知子命令 %q，可用：tail\n用 switchboard log --help 查看子命令", args[0])
	}
	fs := newFlagSet(e, "log tail", "查看最近的请求记录")
	n := fs.Int("n", 20, "条数")
	follow := fs.Bool("follow", false, "持续输出新记录")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	names, err := accountNames(a.st)
	if err != nil {
		return err
	}

	records, err := a.st.RecentRequests(*n)
	if err != nil {
		return err
	}
	if err := printRecords(e, records, names); err != nil {
		return err
	}
	if !*follow {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lastID := int64(0)
	if len(records) > 0 {
		lastID = records[0].ID
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			fresh, err := a.st.RecentRequests(100)
			if err != nil {
				return err
			}
			var newOnes []store.RequestRecord
			for i := len(fresh) - 1; i >= 0; i-- {
				if fresh[i].ID > lastID {
					newOnes = append(newOnes, fresh[i])
				}
			}
			if len(newOnes) == 0 {
				continue
			}
			if err := printRecords(e, newOnes, names); err != nil {
				return err
			}
			for _, r := range newOnes {
				if r.ID > lastID {
					lastID = r.ID
				}
			}
		}
	}
}

func accountNames(st *store.Store) (map[int64]string, error) {
	accounts, err := st.ListAccounts()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(accounts))
	for _, acc := range accounts {
		out[acc.ID] = acc.Name
	}
	return out, nil
}

func printRecords(e *Env, records []store.RequestRecord, names map[int64]string) error {
	if e.JSON {
		out := make([]map[string]any, 0, len(records))
		for _, r := range records {
			out = append(out, map[string]any{
				"ts":               r.TS,
				"inbound":          r.Inbound,
				"outbound":         r.Outbound,
				"model":            r.ModelRequested,
				"model_used":       r.ModelUsed,
				"account":          names[r.AccountID],
				"http_status":      r.HTTPStatus,
				"ok":               r.OK,
				"in_tokens":        r.InTokens,
				"out_tokens":       r.OutTokens,
				"reasoning_tokens": r.ReasoningTokens,
				"cache_read":       r.CacheRead,
				"cache_write":      r.CacheWrite,
				"latency_ms":       r.LatencyMS,
				"error":            r.Error,
			})
		}
		return e.printJSON(out)
	}

	if len(records) == 0 {
		fmt.Fprintln(e.Stdout, "还没有请求记录")
		return nil
	}

	rows := make([][]string, 0, len(records))
	for _, r := range records {
		account := names[r.AccountID]
		if account == "" {
			account = "-"
		}
		status := fmt.Sprintf("%d", r.HTTPStatus)
		if !r.OK {
			status = fmt.Sprintf("%d ✗", r.HTTPStatus)
		}
		rows = append(rows, []string{
			time.Unix(r.TS, 0).Format("01-02 15:04:05"),
			r.Inbound + "→" + r.Outbound,
			modelCell(r.ModelRequested, r.ModelUsed),
			account,
			status,
			fmt.Sprintf("%d", r.InTokens),
			fmt.Sprintf("%d", r.OutTokens),
			fmt.Sprintf("%d", r.ReasoningTokens),
			fmt.Sprintf("%d", r.CacheRead),
			fmt.Sprintf("%d", r.CacheWrite),
			fmt.Sprintf("%dms", r.LatencyMS),
			truncate(r.Error, 30),
		})
	}
	fmt.Fprint(e.Stdout, renderTable(
		[]string{"时间", "协议", "模型", "账号", "状态", "输入", "输出", "思考", "缓存读", "缓存写", "耗时", "错误"}, rows))
	return nil
}

// modelCell 给出「模型」列的内容。客户端给的名字与真正打出去的名字不同时写成
// `别名 → 真名`，一次别名命中因此一眼能看出实际打了哪个模型（DESIGN 5.12）；
// 相同时只写一个名字，不让每一行都背上一次重复。两个名字各自按 24 个显示宽度截断，
// 与单名字时的同一上限，`→` 只占一列，宽度对齐照旧交给 renderTable。
func modelCell(requested, used string) string {
	if used == "" || used == requested {
		return truncate(requested, 24)
	}
	return truncate(requested, 24) + " → " + truncate(used, 24)
}

func cmdCall(e *Env, args []string) error {
	fs := newFlagSet(e, "call", "手工打一条请求，打印选号过程")
	model := fs.String("model", "", "模型名，留空取第一个账号的第一个模型")
	protocol := fs.String("protocol", "anthropic", "入站协议：anthropic | chat | responses | gemini，gemini 的模型名走路径")
	stream := fs.Bool("stream", false, "流式请求")
	maxTokens := fs.Int("max-tokens", 256, "最大输出 token")
	debug := fs.Bool("debug", false, "打印选号过程")
	system := fs.String("system", "", "system 提示词")
	if err := fs.Parse(args); err != nil {
		return err
	}
	inbound, ok := resolveInboundProtocol(*protocol)
	if !ok {
		// systemone 是第五条线协议，但它不是对话：一条决策请求由一个 state 加一组问题组成，
		// 没有提示词可发（DESIGN 5.14 的 CLI 一行）。这是「这个命令接不了它」，不是用法写歪了，
		// 所以走运行失败（退出码 1）并直接指向真正该用的命令。
		if strings.TrimSpace(*protocol) == gateway.ProtocolSystemOne {
			return fmt.Errorf("systemone 不是对话协议：一条决策请求由一个 state 加一组问题组成，没有提示词可发，call 发不了它；改用 switchboard decide --model <模型> --state <文本或 JSON> --noul <名>=<问句>（见 switchboard decide --help）")
		}
		return usagef("不支持的入站协议 %q，可用 anthropic、chat、responses、gemini", *protocol)
	}
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return usagef(`用法：switchboard call "提示词" [--model 模型] [--protocol 协议] [--stream] [--debug] [--json]
可用协议：anthropic、chat、responses、gemini`)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	usedModel := *model
	if usedModel == "" {
		accounts, err := a.st.ListAccounts()
		if err != nil {
			return err
		}
		for _, acc := range accounts {
			if len(acc.Models) > 0 {
				usedModel = acc.Models[0]
				break
			}
		}
		if usedModel == "" {
			return usagef("没有可用模型：先 switchboard account add 加账号，再 switchboard model sync 拉上游模型列表、switchboard model probe 探活（模型列表为空的账号不参与选号）")
		}
	}

	// 这个模型的候选账号全部以 systemone 出站时，这条对话请求在网关里只会换回一句
	// 「协议不相通」的 400（DESIGN 5.14 的协议互通）：在发出去之前就说清，并给出该用的命令。
	if only, err := systemOneOnlyModel(a.st, usedModel); err != nil {
		return err
	} else if only {
		return fmt.Errorf("模型 %s 的有效出站协议是 systemone：这条协议一个 state 加一组问题、不出文本，没有提示词可以发给它，所以 call 发不了；改用 switchboard decide --model %s --state <文本或 JSON> --noul <名>=<问句>，或换一个走对话协议的模型（switchboard model ls 看清单）", usedModel, usedModel)
	}

	logf := func(string, ...any) {}
	if *debug {
		logf = func(format string, logArgs ...any) {
			fmt.Fprintf(e.Stderr, "· %s\n", fmt.Sprintf(format, logArgs...))
		}
	}

	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: logf, DumpBodies: *debug})
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

	ir := &translate.Request{
		Model:     usedModel,
		MaxTokens: *maxTokens,
		Stream:    *stream,
		Messages: []translate.Message{{
			Role:   translate.RoleUser,
			Blocks: []translate.Block{{Kind: translate.BlockText, Text: prompt}},
		}},
	}
	if *system != "" {
		ir.System = []translate.Block{{Kind: translate.BlockText, Text: *system}}
	}
	body, err := translate.EncodeRequest(inbound, ir, usedModel)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ln.Addr().String()+inboundPath(inbound, usedModel, *stream), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if *stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("网关返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	if *stream {
		// gemini 的流式帧要解码后才能给人看（帧里没有 event 名，一次响应就是若干 data 帧），
		// 其余三条协议原样转出 SSE，与之前一致。
		if inbound == translate.ProtocolGemini {
			return printGeminiStream(e, resp.Body)
		}
		_, err := io.Copy(e.Stdout, resp.Body)
		return err
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if e.JSON {
		fmt.Fprintln(e.Stdout, string(raw))
		return nil
	}

	res, err := translate.DecodeResponse(inbound, raw, usedModel)
	if err != nil {
		return fmt.Errorf("响应不是合法 %s 报文: %w", inbound, err)
	}
	for _, block := range res.Blocks {
		switch block.Kind {
		case translate.BlockText:
			fmt.Fprintln(e.Stdout, block.Text)
		case translate.BlockToolUse:
			if block.ToolUse != nil {
				fmt.Fprintf(e.Stdout, "[tool_use] %s %s\n", block.ToolUse.Name, string(block.ToolUse.Input))
			}
		}
	}
	if *debug {
		fmt.Fprintf(e.Stderr, "· 用量 输入 %d / 输出 %d（缓存 %d），结束原因 %s，模型 %s\n",
			res.Usage.In, res.Usage.Out, res.Usage.CacheRead, res.StopReason, res.Model)
	}
	return nil
}

// inboundPath 给出这条请求要打的入站路径。gemini 的模型名在路径里、流式由后缀决定
// （DESIGN 5.9）：模型名按路径段转义后拼进去，其余三条路径固定。
func inboundPath(protocol, model string, stream bool) string {
	if protocol != translate.ProtocolGemini {
		return map[string]string{
			translate.ProtocolAnthropic: "/v1/messages",
			translate.ProtocolChat:      "/v1/chat/completions",
			translate.ProtocolResponses: "/v1/responses",
		}[protocol]
	}
	action := ":generateContent"
	if stream {
		action = ":streamGenerateContent"
	}
	return translate.GeminiInboundPathPrefix + url.PathEscape(model) + action
}

// printGeminiStream 读本进程下发的 gemini SSE 流并逐帧解码：文本增量逐段写到 stdout，
// 工具调用按非流式输出的同一形状打一行。gemini 的帧没有 event 名，只认 `data:` 这一种行。
// --json 时一个 data 帧打一行 JSON 原文：gemini 的帧里没有模型名，原文就是协议对客户端说的
// 全部内容，jq 可以逐行解析。
func printGeminiStream(e *Env, body io.Reader) error {
	dec, err := translate.NewStreamDecoder(translate.ProtocolGemini)
	if err != nil {
		return err
	}

	sc := bufio.NewScanner(body)
	// 一帧可能带整张图片的 inlineData，缓冲给到 8MB。
	sc.Buffer(make([]byte, 0, 8<<10), 8<<20)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		if e.JSON {
			fmt.Fprintln(e.Stdout, payload)
			continue
		}
		deltas, err := dec.Decode([]byte(payload))
		if err != nil {
			return fmt.Errorf("gemini 流式帧解析失败（%v）；上游回来的可能不是 SSE，用 --debug 重跑一次看上游原文，或 switchboard log tail 看这一次的记录", err)
		}
		for _, d := range deltas {
			switch d.Kind {
			case translate.DeltaText:
				fmt.Fprint(e.Stdout, d.Text)
			case translate.DeltaToolStart:
				fmt.Fprintf(e.Stdout, "[tool_use] %s ", d.ToolName)
			case translate.DeltaToolArgs:
				fmt.Fprint(e.Stdout, d.ArgsJSON)
			case translate.DeltaError:
				fmt.Fprintf(e.Stderr, "· 上游错误：%s\n", d.Text)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取 gemini 流式响应失败（%v）；连接可能中途断了，用 switchboard log tail 看这一次的记录", err)
	}
	if !e.JSON {
		fmt.Fprintln(e.Stdout)
	}
	return nil
}
