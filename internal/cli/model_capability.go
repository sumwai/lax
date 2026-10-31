package cli

import (
	"fmt"
	"sort"
	"strings"

	"lax/internal/store"
)

// model capability 一组：声明一个模型能干什么、能用哪些音色与音频格式。
//
// 能力位决定入站端点能不能用这个模型（speech → /v1/audio/speech，transcription →
// /v1/audio/transcriptions），没声明时语音入站直接 400，不发上游请求。音色列表是客户端
// 传 voice 的依据，/v1/models 也会把它带出去，客户端不必猜上游认哪些名字。
//
// add 与 set 的区别只在合并方式：add 把取值并进现有集合（幂等），set 用整份新列表替换
// 命令行给过的那几项（没给的字段不动）。unset 清字段。

const (
	modelCapabilityAddUsage = "用法：lax model capability add <模型> [--provider 厂商] " +
		"[--capability <能力>]... [--voices <音色[,音色...]>] [--audio-formats <格式[,格式...]>]"
	modelCapabilitySetUsage = "用法：lax model capability set <模型> [--provider 厂商] " +
		"[--capability <能力>]... [--voices <音色[,音色...]>] [--audio-formats <格式[,格式...]>]"
	modelCapabilityUnsetUsage = "用法：lax model capability unset <模型> [--provider 厂商] <字段> [<字段>...]\n" +
		"可用字段：capability、voices、audio-formats"
	modelCapabilityLsUsage = "用法：lax model capability ls [<模型>] [--provider 厂商]"
)

// capabilitySubs 是 capability 一组的子命令清单。
var capabilitySubs = []groupItem{
	{"add", "把能力、音色或音频格式并进现有声明（幂等）"},
	{"set", "用新列表替换命令行给过的那几项，其余不动"},
	{"unset", "清掉一项声明"},
	{"ls", "列出已声明能力、音色与音频格式的模型"},
}

func cmdModelCapability(e *Env, args []string) error {
	if isHelpRequest(args) {
		printGroupHelp(e.Stdout, "model capability", "模型能力声明：入站端点能不能用这个模型、能用哪些音色", capabilitySubs)
		return nil
	}
	if len(args) == 0 {
		return usagef("需要子命令：add | set | unset | ls\n用 lax model capability --help 查看子命令")
	}
	switch args[0] {
	case "add":
		return cmdModelCapabilityWrite(e, args[1:], true)
	case "set":
		return cmdModelCapabilityWrite(e, args[1:], false)
	case "unset":
		return cmdModelCapabilityUnset(e, args[1:])
	case "ls":
		return cmdModelCapabilityLs(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：add | set | unset | ls\n用 lax model capability --help 查看子命令", args[0])
	}
}

// capabilityWriteJSON 是 add 与 set 的 --json 形状：报的是合并之后整行的事实。
type capabilityWriteJSON struct {
	Model        string   `json:"model"`
	Provider     string   `json:"provider"`
	Capabilities []string `json:"capabilities"`
	Voices       []string `json:"voices"`
	AudioFormats []string `json:"audio_formats"`
}

func cmdModelCapabilityWrite(e *Env, args []string, additive bool) error {
	name := "model capability set"
	usageLine := modelCapabilitySetUsage
	summary := "用新列表替换能力声明，命令行没给的字段不动"
	if additive {
		name = "model capability add"
		usageLine = modelCapabilityAddUsage
		summary = "把能力、音色或音频格式并进现有声明"
	}
	fs := newFlagSet(e, name, summary)
	provider := fs.String("provider", "", "厂商；不给时按模型名唯一确定")
	var capabilityFlags repeatedFlag
	fs.Var(&capabilityFlags, "capability", "能力位：chat | embedding | speech | transcription，可重复或逗号分隔")
	voices := fs.String("voices", "", "音色名，逗号分隔，例如 mimo_default,Mia,Chloe")
	formats := fs.String("audio-formats", "", "音频格式，逗号分隔，例如 mp3,wav,pcm16")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("%s\n一次改一个模型，模型名要写全\n%s", usageLine, usageLine)
	}
	model := strings.TrimSpace(fs.Arg(0))
	if model == "" {
		return usagef("模型名不能为空：能力声明挂在「厂商 × 模型」上，模型名要写全\n%s", usageLine)
	}

	patch := store.CapabilityPatch{
		Capabilities: splitCSV(capabilityFlags.values),
	}
	if flagWasSet(fs, "voices") {
		patch.Voices = splitCSV([]string{*voices})
	}
	if flagWasSet(fs, "audio-formats") {
		patch.AudioFormats = splitCSV([]string{*formats})
	}
	if patch.Capabilities == nil && patch.Voices == nil && patch.AudioFormats == nil {
		return usagef("一项都没给：至少给 --capability、--voices 或 --audio-formats 之一\n%s", usageLine)
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	prov, err := resolveCatalogProvider(a.st, model, *provider, usageLine, "model capability "+map[bool]string{true: "add", false: "set"}[additive])
	if err != nil {
		return err
	}

	var row store.ProviderModel
	if additive {
		row, err = a.st.AddProviderModelCapability(prov, model, patch)
	} else {
		row, err = a.st.SetProviderModelCapability(prov, model, patch)
	}
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(capabilityWriteJSON{
			Model:        row.Model,
			Provider:     row.Provider,
			Capabilities: row.Capabilities,
			Voices:       row.Voices,
			AudioFormats: row.AudioFormats,
		})
	}

	verb := "已替换"
	if additive {
		verb = "已合并"
	}
	fmt.Fprintf(e.Stdout, "%s厂商 %s 的 %s 能力声明：%s\n", verb, row.Provider, row.Model, capabilitySummary(row))
	fmt.Fprintln(e.Stdout, "下一步：lax model capability ls 查看全部声明，或 lax model ls 确认模型已可用")
	return nil
}

// capabilityUnsetJSON 是 unset 的 --json 形状。
type capabilityUnsetJSON struct {
	Model        string   `json:"model"`
	Provider     string   `json:"provider"`
	Cleared      []string `json:"cleared"`
	Capabilities []string `json:"capabilities"`
	Voices       []string `json:"voices"`
	AudioFormats []string `json:"audio_formats"`
}

func cmdModelCapabilityUnset(e *Env, args []string) error {
	fs := newFlagSet(e, "model capability unset", "清掉一项能力声明")
	provider := fs.String("provider", "", "厂商；不给时按模型名唯一确定")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return usagef("需要模型名与至少一个字段\n%s", modelCapabilityUnsetUsage)
	}
	model := strings.TrimSpace(fs.Arg(0))
	fields := fs.Args()[1:]
	// 先把字段名过一遍白名单：写错一个字段名时，不该走完选厂商与读库才报错。
	for _, f := range fields {
		if _, err := store.ParseCapabilityField(f); err != nil {
			return usagef("%v\n%s", err, modelCapabilityUnsetUsage)
		}
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	prov, err := resolveCatalogProvider(a.st, model, *provider, modelCapabilityUnsetUsage, "model capability unset")
	if err != nil {
		return err
	}
	row, err := a.st.ClearProviderModelCapability(prov, model, fields)
	if err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(capabilityUnsetJSON{
			Model:        row.Model,
			Provider:     row.Provider,
			Cleared:      dedupeStrings(fields),
			Capabilities: row.Capabilities,
			Voices:       row.Voices,
			AudioFormats: row.AudioFormats,
		})
	}
	for _, f := range dedupeStrings(fields) {
		fmt.Fprintf(e.Stdout, "%s 的 %s 已清空\n", model, f)
	}
	fmt.Fprintf(e.Stdout, "现有声明：%s\n", capabilitySummary(row))
	return nil
}

// capabilityLsJSON 是 ls 的一项。
type capabilityLsJSON struct {
	Model        string   `json:"model"`
	Provider     string   `json:"provider"`
	Capabilities []string `json:"capabilities"`
	Voices       []string `json:"voices"`
	AudioFormats []string `json:"audio_formats"`
}

func cmdModelCapabilityLs(e *Env, args []string) error {
	fs := newFlagSet(e, "model capability ls", "列出已声明能力、音色与音频格式的模型")
	provider := fs.String("provider", "", "只看这家厂商")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return usagef("一次最多看一个模型，看全部就整个省掉模型名\n%s", modelCapabilityLsUsage)
	}
	var want string
	if fs.NArg() == 1 {
		want = strings.TrimSpace(fs.Arg(0))
		if want == "" {
			return usagef("模型名不能为空\n%s", modelCapabilityLsUsage)
		}
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	prov := strings.TrimSpace(*provider)
	var names []string
	if prov != "" {
		if _, err := a.st.GetProvider(prov); err != nil {
			return err
		}
		names = []string{prov}
	} else {
		providers, err := a.st.ListProviders()
		if err != nil {
			return err
		}
		for _, p := range providers {
			names = append(names, p.Name)
		}
	}

	rows := make([]capabilityLsJSON, 0)
	for _, name := range names {
		models, err := a.st.ProviderModels(name)
		if err != nil {
			return err
		}
		for _, m := range models {
			if want != "" && m.Model != want {
				continue
			}
			// 没声明的行不列：这一组回答的是「声明了什么」，把全部目录行摆出来只会淹没答案。
			if len(m.Capabilities) == 0 && len(m.Voices) == 0 && len(m.AudioFormats) == 0 {
				continue
			}
			rows = append(rows, capabilityLsJSON{
				Model:        m.Model,
				Provider:     m.Provider,
				Capabilities: m.Capabilities,
				Voices:       m.Voices,
				AudioFormats: m.AudioFormats,
			})
		}
	}
	if want != "" && len(rows) == 0 {
		return fmt.Errorf("模型 %q 没有任何能力声明：先 lax model capability add %s --capability speech --voices <音色>", want, want)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Provider < rows[j].Provider
	})

	if e.JSON {
		return e.printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "还没有能力声明。语音模型先声明再使用，例如 lax model capability add mimo-v2.5-tts --capability speech --voices mimo_default,Mia,Chloe --audio-formats mp3,wav,pcm16")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.Model,
			r.Provider,
			orDash(r.Capabilities),
			orDash(r.Voices),
			orDash(r.AudioFormats),
		})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"模型", "厂商", "能力", "音色", "音频格式"}, table))
	return nil
}

// resolveCatalogProvider 决定一次目录写入落在哪家厂商上：--provider 给了就用它，
// 没给就按模型名唯一确定；多家目录里都有同名模型时报错并列出候选。
func resolveCatalogProvider(st *store.Store, model, flagValue, usageLine, command string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}
	names, err := providersWithModel(st, model)
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("模型 %q 不在任何厂商的目录里：先用 lax model sync 拉上游清单把它写进目录，"+
			"或 lax model add %s --account <账号>", model, model)
	case 1:
		return names[0], nil
	default:
		return "", usagef("模型 %q 同时出现在 %d 家厂商的目录里：%s\n用 --provider 指定其中一家（例如 lax %s %s --provider %s）\n%s",
			model, len(names), strings.Join(names, "、"), command, model, names[0], usageLine)
	}
}

// capabilitySummary 把一行声明写成一句人读的话。
func capabilitySummary(m store.ProviderModel) string {
	parts := make([]string, 0, 3)
	if len(m.Capabilities) > 0 {
		parts = append(parts, "能力 "+strings.Join(m.Capabilities, "、"))
	}
	if len(m.Voices) > 0 {
		parts = append(parts, fmt.Sprintf("音色 %d 个（%s）", len(m.Voices), strings.Join(m.Voices, "、")))
	}
	if len(m.AudioFormats) > 0 {
		parts = append(parts, "音频格式 "+strings.Join(m.AudioFormats, "、"))
	}
	if len(parts) == 0 {
		return "（没有声明）"
	}
	return strings.Join(parts, "，")
}

// splitCSV 把若干段逗号分隔的取值拆成切片：去空白、丢空项、按顺序去重。
// 命令行同时接受 `--capability speech --capability transcription` 与 `--capability speech,transcription`。
func splitCSV(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			v := strings.TrimSpace(part)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dedupeStrings 去重并保持出现顺序，供 unset 的确认行用。
func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// orDash 把空列表写成 -，非空列表用顿号连起来。
func orDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, "、")
}
