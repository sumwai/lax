package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"lax/internal/plugin"
	"lax/internal/store"
)

// model dialect 一组：看待模型绑了哪条方言、手工改绑、删掉绑定。
//
// 绑定是 model_operations 的一行（厂商 × 模型 × 操作）。语音请求按绑定选方言出站；
// 对话、向量化与决策请求只能绑宿主方言（lax/ 前缀），绑插件方言会被网关拒绝。

const (
	modelDialectLsUsage  = "用法：lax model dialect ls [<模型>] [--provider 厂商]"
	modelDialectSetUsage = "用法：lax model dialect set <模型> <操作> <方言 ID> [--provider 厂商]\n" +
		"操作取 chat.complete@1、embeddings.create@1、speech.synthesize@1、speech.transcribe@1、system.decide@1"
	modelDialectUnsetUsage = "用法：lax model dialect unset <模型> <操作> [--provider 厂商]"
)

var dialectSubs = []groupItem{
	{"ls", "列出模型绑定的方言"},
	{"set", "把模型的一个操作绑到一条方言"},
	{"unset", "删掉一条绑定"},
}

func cmdModelDialect(e *Env, args []string) error {
	if isHelpRequest(args) || len(args) == 0 {
		printGroupHelp(e.Stdout, "model dialect", "方言绑定：模型的上游形状由哪条方言描述", dialectSubs)
		return nil
	}
	switch args[0] {
	case "ls":
		return cmdModelDialectLs(e, args[1:])
	case "set":
		return cmdModelDialectSet(e, args[1:])
	case "unset":
		return cmdModelDialectUnset(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | set | unset\n用 lax model dialect --help 查看子命令", args[0])
	}
}

// dialectLsJSON 是 ls 的一行。
type dialectLsJSON struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Operation string `json:"operation"`
	Dialect   string `json:"dialect"`
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updated_at"`
}

func cmdModelDialectLs(e *Env, args []string) error {
	fs := newFlagSet(e, "model dialect ls", "列出模型绑定的方言")
	provider := fs.String("provider", "", "只看这家厂商")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("一次最多看一个模型，看全部就整个省掉模型名\n%s", modelDialectLsUsage)
	}
	want := ""
	if fs.NArg() == 1 {
		want = strings.TrimSpace(fs.Arg(0))
		if want == "" {
			return usagef("模型名不能为空\n%s", modelDialectLsUsage)
		}
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	rows, err := a.st.ListModelOperations(strings.TrimSpace(*provider), want)
	if err != nil {
		return err
	}
	out := make([]dialectLsJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, dialectLsJSON{
			Model: row.Model, Provider: row.Provider, Operation: row.Operation,
			Dialect: row.DialectID, Source: row.Source, UpdatedAt: row.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Operation < out[j].Operation
	})

	if e.JSON {
		return e.printJSON(out)
	}
	if len(out) == 0 {
		fmt.Fprintln(e.Stdout, "还没有方言绑定。语音模型先声明能力并绑一条方言，例如 lax model capability add mimo-v2.5-tts --capability speech --dialect lax/mimo-chat-speech@1")
		return nil
	}
	table := make([][]string, 0, len(out))
	for _, row := range out {
		table = append(table, []string{
			row.Model, row.Provider, row.Operation, row.Dialect, row.Source,
			time.Unix(row.UpdatedAt, 0).Format("2006-01-02 15:04:05"),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"模型", "厂商", "操作", "方言", "来源", "更新时间"}, table))
	return nil
}

func cmdModelDialectSet(e *Env, args []string) error {
	fs := newFlagSet(e, "model dialect set", "把模型的一个操作绑到一条方言")
	provider := fs.String("provider", "", "厂商；不给时按模型名唯一确定")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 3 {
		return usagef("%s", modelDialectSetUsage)
	}
	model := strings.TrimSpace(fs.Arg(0))
	operation := strings.TrimSpace(fs.Arg(1))
	dialectID := strings.TrimSpace(fs.Arg(2))
	if model == "" || dialectID == "" {
		return usagef("模型名与方言 ID 都不能为空\n%s", modelDialectSetUsage)
	}
	if !plugin.KnownOperation(operation) {
		return usagef("操作 %q 不是宿主认识的操作\n%s", operation, modelDialectSetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	prov, err := resolveCatalogProvider(a.st, model, *provider, modelDialectSetUsage, "model dialect set")
	if err != nil {
		return err
	}
	updatedAt := time.Now().Unix()
	if err := a.st.SetModelOperation(store.ModelOperation{
		Provider: prov, Model: model, Operation: operation,
		DialectID: dialectID, Source: "manual", UpdatedAt: updatedAt,
	}); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(dialectLsJSON{
			Model: model, Provider: prov, Operation: operation, Dialect: dialectID,
			Source: "manual", UpdatedAt: updatedAt,
		})
	}
	fmt.Fprintf(e.Stdout, "已把厂商 %s 的 %s 的 %s 绑到方言 %s\n", prov, model, operation, dialectID)
	if !plugin.IsHostDialect(dialectID) && operation != plugin.OpSpeechSynthesize && operation != plugin.OpSpeechTranscribe {
		fmt.Fprintln(e.Stdout, "注意：本版只执行语音两条操作的插件方言，这一条绑到插件方言后网关会拒绝请求；非语音操作请绑宿主方言（lax/ 前缀）")
	}
	return nil
}

func cmdModelDialectUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "model dialect unset", "删掉一条方言绑定")
	provider := fs.String("provider", "", "厂商；不给时按模型名唯一确定")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("%s", modelDialectUnsetUsage)
	}
	model := strings.TrimSpace(fs.Arg(0))
	operation := strings.TrimSpace(fs.Arg(1))
	if model == "" {
		return usagef("模型名不能为空\n%s", modelDialectUnsetUsage)
	}
	if !plugin.KnownOperation(operation) {
		return usagef("操作 %q 不是宿主认识的操作\n%s", operation, modelDialectUnsetUsage)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	prov, err := resolveCatalogProvider(a.st, model, *provider, modelDialectUnsetUsage, "model dialect unset")
	if err != nil {
		return err
	}
	// 先确认这一行在：删不到时报错，而不是把「本来就没有」当成功。
	if _, ok, err := a.st.ModelOperation(prov, model, operation); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("厂商 %s 的 %s 没有 %s 的绑定：用 lax model dialect ls %s 看现有绑定", prov, model, operation, model)
	}
	if err := a.st.DeleteModelOperation(prov, model, operation); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model": model, "provider": prov, "operation": operation, "removed": true,
		})
	}
	fmt.Fprintf(e.Stdout, "已删掉厂商 %s 的 %s 在 %s 上的绑定\n", prov, model, operation)
	if operation == plugin.OpSpeechSynthesize || operation == plugin.OpSpeechTranscribe {
		fmt.Fprintln(e.Stdout, "下一步：语音请求现在会因为没有绑定而被拒；重新绑一条，或重启 lax serve 让能力回填补上默认方言")
	}
	return nil
}
