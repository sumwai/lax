package store

import (
	"fmt"
	"strings"
)

// 插件日志与活动版本切换。
//
// 日志只用来排障：每个插件只保留最近 pluginLogRetention 条，写入时顺手清掉更老的，
// 不归档、不跨插件查询、不提供删除接口（删插件时一起清）。

// pluginLogRetention 是每个插件保留的日志行数。
const pluginLogRetention = 500

// PluginLog 是一行插件日志。
type PluginLog struct {
	ID       int64
	PluginID string
	Level    string
	Hook     string
	Message  string
	At       int64
}

// AppendPluginLog 写一行插件日志，并把该插件超出保留条数的老行删掉。
func (s *Store) AppendPluginLog(entry PluginLog) error {
	if strings.TrimSpace(entry.PluginID) == "" {
		return fmt.Errorf("写插件日志需要插件 ID")
	}
	if entry.At == 0 {
		entry.At = unixNow()
	}
	row := pluginLogRow{
		PluginID: entry.PluginID, Level: entry.Level, Hook: entry.Hook, Message: entry.Message, At: entry.At,
	}
	if err := s.gorm.Create(&row).Error; err != nil {
		return err
	}
	// 超出保留条数的老行删掉：删除条件用「第 500 条的时间」，同一秒内多条由 id 兜底排序。
	var cutoff int64
	if err := s.gorm.Model(&pluginLogRow{}).Where("plugin_id = ?", entry.PluginID).
		Order("at desc, id desc").Offset(pluginLogRetention).Limit(1).
		Pluck("at", &cutoff).Error; err != nil || cutoff == 0 {
		return err
	}
	return s.gorm.Where("plugin_id = ? AND at <= ?", entry.PluginID, cutoff).
		Delete(&pluginLogRow{}).Error
}

// PluginLogs 按时间倒序返回日志。level 非空时只取该级别，since 非 0 时只取之后的。
func (s *Store) PluginLogs(pluginID string, limit int, level string, since int64) ([]PluginLog, error) {
	if limit <= 0 {
		limit = 100
	}
	query := s.gorm.Where("plugin_id = ?", pluginID)
	if level != "" {
		query = query.Where("level = ?", level)
	}
	if since > 0 {
		query = query.Where("at >= ?", since)
	}
	var rows []pluginLogRow
	if err := query.Order("at desc, id desc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PluginLog, 0, len(rows))
	for _, r := range rows {
		out = append(out, PluginLog{
			ID: r.ID, PluginID: r.PluginID, Level: r.Level, Hook: r.Hook, Message: r.Message, At: r.At,
		})
	}
	return out, nil
}

// SetActivePluginVersion 把活动版本切到一个已安装的版本。版本行不存在时报错，
// 已安装版本不可变只作用于行本身，切换活动指针是允许的（回滚就靠它）。
func (s *Store) SetActivePluginVersion(pluginID, version string) error {
	pluginID = strings.TrimSpace(pluginID)
	version = strings.TrimSpace(version)
	if pluginID == "" || version == "" {
		return fmt.Errorf("切换活动版本需要插件 ID 与版本号")
	}
	var row pluginVersionRow
	if err := s.gorm.Where("plugin_id = ? AND version = ?", pluginID, version).
		Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.Version == "" {
		return fmt.Errorf("插件 %s 没有装过版本 %s：用 lax plugin show %s 看已装版本", pluginID, version, pluginID)
	}
	return s.gorm.Table("plugins").Where("id = ?", pluginID).
		Updates(map[string]any{"active_version": version, "updated_at": unixNow()}).Error
}
