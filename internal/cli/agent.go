package cli

import (
	"fmt"
	"os"
	"time"

	"switchboard/internal/agentcfg"
	"switchboard/internal/store"
)

func cmdAgent(e *Env, args []string) error {
	if len(args) == 0 {
		return usagef("需要子命令：ls | use | revert")
	}
	switch args[0] {
	case "ls":
		return cmdAgentLs(e, args[1:])
	case "use":
		return cmdAgentUse(e, args[1:])
	case "revert":
		return cmdAgentRevert(e, args[1:])
	default:
		return usagef("未知子命令 %q，可用：ls | use | revert", args[0])
	}
}

func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("取用户目录失败: %w", err)
	}
	return home, nil
}

func cmdAgentLs(e *Env, args []string) error {
	fs := newFlagSet(e, "agent ls", "列出支持的客户端与指向状态")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	home, err := homeDir()
	if err != nil {
		return err
	}

	states, err := a.st.ListAgents()
	if err != nil {
		return err
	}
	byName := map[string]store.AgentState{}
	for _, st := range states {
		byName[st.Name] = st
	}

	targets := agentcfg.Known(home)
	type row struct {
		Name   string `json:"name"`
		Config string `json:"config"`
		Points bool   `json:"points_at_switchboard"`
		Model  string `json:"model"`
		Backup string `json:"backup"`
	}
	out := make([]row, 0, len(targets))
	for _, t := range targets {
		st := byName[t.Name]
		out = append(out, row{
			Name:   t.Name,
			Config: t.ConfigPath,
			Points: agentcfg.PointsAt(t, a.baseURL()),
			Model:  st.Model,
			Backup: st.BackupPath,
		})
	}

	if e.JSON {
		return e.printJSON(out)
	}

	rows := make([][]string, 0, len(out))
	for _, r := range out {
		state := "未指向"
		if r.Points {
			state = "已指向"
		}
		model := r.Model
		if model == "" {
			model = "-"
		}
		rows = append(rows, []string{r.Name, state, model, truncate(r.Config, 46)})
	}
	fmt.Fprint(e.Stdout, renderTable([]string{"客户端", "状态", "模型", "配置文件"}, rows))
	return nil
}

func cmdAgentUse(e *Env, args []string) error {
	fs := newFlagSet(e, "agent use", "把客户端指向本网关")
	model := fs.String("model", "", "默认模型")
	apply := fs.Bool("apply", false, "写入配置文件，不加只打印差异")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：switchboard agent use <客户端> [--model 模型] [--apply]")
	}
	name := fs.Arg(0)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	home, err := homeDir()
	if err != nil {
		return err
	}
	target, err := agentcfg.Lookup(home, name)
	if err != nil {
		return err
	}

	desired := agentcfg.Desired(a.baseURL(), *model)
	changes, err := agentcfg.Diff(target, desired)
	if err != nil {
		return err
	}

	if e.JSON {
		payload := map[string]any{
			"agent":   name,
			"config":  target.ConfigPath,
			"changes": changes,
			"applied": false,
		}
		if *apply {
			backup, err := agentcfg.Apply(target, desired)
			if err != nil {
				return err
			}
			if err := a.st.PutAgent(store.AgentState{
				Name:       name,
				ConfigPath: target.ConfigPath,
				BackupPath: backup,
				Model:      *model,
				AppliedAt:  time.Now().Unix(),
			}); err != nil {
				return err
			}
			payload["applied"] = true
			payload["backup"] = backup
		}
		return e.printJSON(payload)
	}

	if !*apply {
		fmt.Fprintf(e.Stdout, "将修改 %s\n", target.ConfigPath)
		for _, c := range changes {
			fmt.Fprintf(e.Stdout, "  %s: %s → %s\n", c.Key, orPlaceholder(c.Old), c.New)
		}
		if len(changes) == 0 {
			fmt.Fprintln(e.Stdout, "  已经是目标值，无需改动")
		}
		fmt.Fprintln(e.Stdout, "加 --apply 写入，原文件备份为 <文件名>.switchboard.bak")
		return nil
	}

	if len(changes) == 0 {
		fmt.Fprintf(e.Stdout, "%s 已经是目标值，未改动\n", target.ConfigPath)
		return nil
	}
	backup, err := agentcfg.Apply(target, desired)
	if err != nil {
		return err
	}
	if err := a.st.PutAgent(store.AgentState{
		Name:       name,
		ConfigPath: target.ConfigPath,
		BackupPath: backup,
		Model:      *model,
		AppliedAt:  time.Now().Unix(),
	}); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "已写入 %s\n备份 %s\n", target.ConfigPath, backup)
	fmt.Fprintf(e.Stdout, "还原：switchboard agent revert %s\n", name)
	return nil
}

func orPlaceholder(s string) string {
	if s == "" {
		return "(空)"
	}
	return s
}

func cmdAgentRevert(e *Env, args []string) error {
	fs := newFlagSet(e, "agent revert", "从备份还原客户端配置")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("用法：switchboard agent revert <客户端>")
	}
	name := fs.Arg(0)

	a, err := openApp(e, true)
	if err != nil {
		return err
	}
	defer a.Close()

	home, err := homeDir()
	if err != nil {
		return err
	}
	target, err := agentcfg.Lookup(home, name)
	if err != nil {
		return err
	}

	st, err := a.st.GetAgent(name)
	if err != nil {
		return fmt.Errorf("没有 %s 的改写记录，无法还原", name)
	}
	if err := agentcfg.Revert(target, st.BackupPath); err != nil {
		return err
	}
	if err := a.st.DeleteAgent(name); err != nil {
		return err
	}

	if e.JSON {
		return e.printJSON(map[string]string{"agent": name, "restored_from": st.BackupPath})
	}
	fmt.Fprintf(e.Stdout, "已从 %s 还原 %s\n", st.BackupPath, target.ConfigPath)
	return nil
}
