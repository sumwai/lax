package store

import (
	"fmt"
	"strings"
)

// 插件审计：只增不改。安装、升级、回滚、启停、卸载、权限变化各记一条，
// 回答“谁在什么时候把哪个权限打开了”。
const (
	AuditInstall    = "install"
	AuditUpdate     = "update"
	AuditRollback   = "rollback"
	AuditRemove     = "remove"
	AuditEnable     = "enable"
	AuditDisable    = "disable"
	AuditReload     = "reload"
	AuditState      = "state"
	AuditPermission = "permissions"
)

// PluginAudit 是一行审计。
type PluginAudit struct {
	ID                    int64
	PluginID              string
	Version               string
	Action                string
	Actor                 string
	Result                string
	Error                 string
	PermissionsBeforeJSON string
	PermissionsAfterJSON  string
	At                    int64
}

// AppendPluginAudit 追加一条审计。At 为 0 时取当前秒。
func (s *Store) AppendPluginAudit(a PluginAudit) error {
	if strings.TrimSpace(a.PluginID) == "" || strings.TrimSpace(a.Action) == "" {
		return fmt.Errorf("审计需要插件 ID 与动作：两者决定这条记录属于谁、做了什么")
	}
	if a.At == 0 {
		a.At = unixNow()
	}
	if a.Result == "" {
		a.Result = "ok"
	}
	row := pluginAuditRow{
		PluginID: a.PluginID, Version: a.Version, Action: a.Action, Actor: a.Actor,
		Result: a.Result, Error: a.Error,
		PermissionsBeforeJSON: a.PermissionsBeforeJSON, PermissionsAfterJSON: a.PermissionsAfterJSON,
		At: a.At,
	}
	return s.gorm.Create(&row).Error
}

// PluginAuditLog 按时间倒序返回一个插件的审计；limit <= 0 时默认 20 条。
func (s *Store) PluginAuditLog(pluginID string, limit int) ([]PluginAudit, error) {
	if limit <= 0 {
		limit = 20
	}
	var rows []pluginAuditRow
	if err := s.gorm.Where("plugin_id = ?", pluginID).Order("at desc, id desc").
		Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PluginAudit, 0, len(rows))
	for _, r := range rows {
		out = append(out, PluginAudit{
			ID: r.ID, PluginID: r.PluginID, Version: r.Version, Action: r.Action,
			Actor: r.Actor, Result: r.Result, Error: r.Error,
			PermissionsBeforeJSON: r.PermissionsBeforeJSON, PermissionsAfterJSON: r.PermissionsAfterJSON,
			At: r.At,
		})
	}
	return out, nil
}
