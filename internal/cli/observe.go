package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
				"key":        r.Key,
				"requests":   r.Requests,
				"in_tokens":  r.InTokens,
				"out_tokens": r.OutTokens,
				"cost":       r.Cost,
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
	var totalReq, totalIn, totalOut int64
	var totalCost float64
	for _, r := range rows {
		table = append(table, []string{
			r.Key,
			fmt.Sprintf("%d", r.Requests),
			fmt.Sprintf("%d", r.InTokens),
			fmt.Sprintf("%d", r.OutTokens),
			usage.Yuan(r.Cost),
		})
		totalReq += r.Requests
		totalIn += r.InTokens
		totalOut += r.OutTokens
		totalCost += r.Cost
	}
	table = append(table, []string{
		"合计",
		fmt.Sprintf("%d", totalReq),
		fmt.Sprintf("%d", totalIn),
		fmt.Sprintf("%d", totalOut),
		usage.Yuan(totalCost),
	})
	fmt.Fprint(e.Stdout, renderTable([]string{headerName, "请求", "输入 token", "输出 token", "估算花费"}, table))
	if totalCost == 0 {
		fmt.Fprintln(e.Stdout, "花费为 0：账号没定价，加账号时给 --price-in 与 --price-out")
	}
	return nil
}

func cmdLog(e *Env, args []string) error {
	if len(args) == 0 || args[0] != "tail" {
		return usagef("需要子命令：tail")
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
				"ts":          r.TS,
				"inbound":     r.Inbound,
				"outbound":    r.Outbound,
				"model":       r.ModelRequested,
				"account":     names[r.AccountID],
				"http_status": r.HTTPStatus,
				"ok":          r.OK,
				"in_tokens":   r.InTokens,
				"out_tokens":  r.OutTokens,
				"latency_ms":  r.LatencyMS,
				"error":       r.Error,
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
			truncate(r.ModelRequested, 24),
			account,
			status,
			fmt.Sprintf("%d", r.InTokens),
			fmt.Sprintf("%d", r.OutTokens),
			fmt.Sprintf("%dms", r.LatencyMS),
			truncate(r.Error, 30),
		})
	}
	fmt.Fprint(e.Stdout, renderTable(
		[]string{"时间", "协议", "模型", "账号", "状态", "输入", "输出", "耗时", "错误"}, rows))
	return nil
}

func cmdCall(e *Env, args []string) error {
	fs := newFlagSet(e, "call", "手工打一条请求，打印选号过程")
	model := fs.String("model", "", "模型名，留空取第一个账号的第一个模型")
	protocol := fs.String("protocol", "anthropic", "入站协议，第一版只支持 anthropic")
	stream := fs.Bool("stream", false, "流式请求")
	maxTokens := fs.Int("max-tokens", 256, "最大输出 token")
	debug := fs.Bool("debug", false, "打印选号过程")
	system := fs.String("system", "", "system 提示词")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *protocol != gateway.InboundAnthropic {
		return usagef("第一版只支持 anthropic 入站，收到 %q", *protocol)
	}
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return usagef(`用法：switchboard call "提示词" [--model 模型] [--debug]`)
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
			return usagef("没有可用模型，先运行 switchboard account add 并给出 --models")
		}
	}

	logf := func(string, ...any) {}
	if *debug {
		logf = func(format string, logArgs ...any) {
			fmt.Fprintf(e.Stderr, "· %s\n", fmt.Sprintf(format, logArgs...))
		}
	}

	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: logf})
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

	text := prompt
	req := translate.AnthropicRequest{
		Model:     usedModel,
		MaxTokens: *maxTokens,
		Stream:    *stream,
		Messages: []translate.AnthropicMessage{
			{Role: "user", Content: translate.Content{Text: &text}},
		},
	}
	if *system != "" {
		sysRaw, err := json.Marshal(*system)
		if err != nil {
			return err
		}
		req.System = sysRaw
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ln.Addr().String()+"/v1/messages", bytes.NewReader(body))
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

	var out translate.AnthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	for _, block := range out.Content {
		switch block.Type {
		case "text":
			fmt.Fprintln(e.Stdout, block.Text)
		case "tool_use":
			fmt.Fprintf(e.Stdout, "[tool_use] %s %s\n", block.Name, string(block.Input))
		}
	}
	if *debug {
		fmt.Fprintf(e.Stderr, "· 用量 输入 %d / 输出 %d，结束原因 %s，模型 %s\n",
			out.Usage.InputTokens, out.Usage.OutputTokens, out.StopReason, out.Model)
	}
	return nil
}
