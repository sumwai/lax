package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lax/internal/plugin"
	"lax/internal/translate"
)

// 插件系统的存储层：插件状态、已安装版本、配置项，以及模型按 operation 的方言绑定。
//
// 三条边界：已安装的版本不可变（版本行只插不改）；审计、缓存与插件独立密钥不在这里；
// 删除插件只带走 source='plugin' 且方言 ID 属于它的绑定，manual 与 declared 的行留着。

var (
	// ErrPluginNotFound 表示 plugins 表里没有这一行。
	ErrPluginNotFound = errors.New("插件不存在")
	// ErrPluginVersionExists 表示 (插件, 版本) 已经装过。版本行不可变，更新只能换版本号。
	ErrPluginVersionExists = errors.New("插件版本已存在")
)

// model_operations 的来源。plugin 的行随插件删除，另外两种由用户手写与能力位回填产生，
// 不归插件生命周期管。
const (
	sourceOpPlugin   = "plugin"
	sourceOpDeclared = "declared"
)

// pluginStateOK 是装好之后的默认状态。
const pluginStateOK = "ok"

// Plugin 是一行插件状态。
type Plugin struct {
	ID            string
	ActiveVersion string
	Kind          string
	Trust         string
	State         string
	Source        string
	LastError     string
	Enabled       bool
	InstalledAt   int64
	UpdatedAt     int64
}

// PluginVersion 是一个已安装版本的不可变元数据。
// Kind 与 Trust 来自 manifest，写在 plugins 行上，不随版本历史保留。
type PluginVersion struct {
	PluginID        string
	Version         string
	Kind            string
	Trust           string
	ABI             string
	Runtime         string
	ManifestJSON    string
	PermissionsJSON string
	PackageSHA256   string
	ManifestSHA256  string
	Source          string
	SignatureStatus string
	InstalledAt     int64
}

// ModelOperation 是一个模型在一个操作上的方言绑定与能力声明。
type ModelOperation struct {
	Provider         string
	Model            string
	Operation        string
	DialectID        string
	CapabilitiesJSON string
	LimitsJSON       string
	Source           string
	UpdatedAt        int64
}

func pluginFromRow(r pluginRow) Plugin {
	return Plugin{
		ID:            r.ID,
		ActiveVersion: r.ActiveVersion,
		Kind:          r.Kind,
		Trust:         r.Trust,
		State:         r.State,
		Source:        r.Source,
		LastError:     r.LastError,
		Enabled:       r.Enabled,
		InstalledAt:   r.InstalledAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

func pluginVersionFromRow(r pluginVersionRow) PluginVersion {
	return PluginVersion{
		PluginID:        r.PluginID,
		Version:         r.Version,
		ABI:             r.ABI,
		Runtime:         r.Runtime,
		ManifestJSON:    r.ManifestJSON,
		PermissionsJSON: r.PermissionsJSON,
		PackageSHA256:   r.PackageSHA256,
		ManifestSHA256:  r.ManifestSHA256,
		Source:          r.Source,
		SignatureStatus: r.SignatureStatus,
		InstalledAt:     r.InstalledAt,
	}
}

func modelOperationFromRow(r modelOperationRow) ModelOperation {
	return ModelOperation{
		Provider:         r.Provider,
		Model:            r.Model,
		Operation:        r.Operation,
		DialectID:        r.DialectID,
		CapabilitiesJSON: r.CapabilitiesJSON,
		LimitsJSON:       r.LimitsJSON,
		Source:           r.Source,
		UpdatedAt:        r.UpdatedAt,
	}
}

// PutPluginVersion 在一个事务里插入版本行并维护 plugins 行。
//
// 版本行已经存在时拒绝而不是覆盖：升级要换版本号，改哈希会让已经校验过的包对不上号。
// active 只决定活动版本：为假时 plugins.active_version 一个字不动。
func (s *Store) PutPluginVersion(v PluginVersion, active bool) error {
	v.PluginID = strings.TrimSpace(v.PluginID)
	v.Version = strings.TrimSpace(v.Version)
	if v.PluginID == "" || v.Version == "" {
		return errors.New("写入插件版本需要插件 ID 与版本号：两者是 plugin_versions 的主键")
	}
	if v.InstalledAt == 0 {
		v.InstalledAt = unixNow()
	}
	now := unixNow()

	return s.withTx(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&pluginVersionRow{}).
			Where("plugin_id = ? AND version = ?", v.PluginID, v.Version).
			Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w：插件 %s 的版本 %s 已经装过，已安装的版本不可变，更新要换一个新的版本号",
				ErrPluginVersionExists, v.PluginID, v.Version)
		}

		// 用 map 而不是结构体：GORM 会把结构体上名为 UpdatedAt 的字段改成当前秒，落库时间就不由调用方决定了。
		// plugins 与 model_operations 的 updated_at 同理，本文件的写入一律按列名给值。
		if err := tx.Table("plugin_versions").Create(map[string]any{
			"plugin_id": v.PluginID, "version": v.Version, "abi": v.ABI, "runtime": v.Runtime,
			"manifest_json": v.ManifestJSON, "permissions_json": v.PermissionsJSON,
			"package_sha256": v.PackageSHA256, "manifest_sha256": v.ManifestSHA256,
			"source": v.Source, "signature_status": v.SignatureStatus,
			"installed_at": v.InstalledAt,
		}).Error; err != nil {
			return err
		}

		var row pluginRow
		if err := tx.Where("id = ?", v.PluginID).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		if row.ID == "" {
			activeVersion := ""
			if active {
				activeVersion = v.Version
			}
			return tx.Table("plugins").Create(map[string]any{
				"id": v.PluginID, "active_version": activeVersion,
				"kind": v.Kind, "trust": v.Trust,
				"enabled": true, "state": pluginStateOK, "source": v.Source, "last_error": "",
				"installed_at": now, "updated_at": now,
			}).Error
		}

		updates := map[string]any{"source": v.Source, "updated_at": now}
		if v.Kind != "" {
			updates["kind"] = v.Kind
		}
		if v.Trust != "" {
			updates["trust"] = v.Trust
		}
		if active {
			updates["active_version"] = v.Version
		}
		return tx.Table("plugins").Where("id = ?", row.ID).Updates(updates).Error
	})
}

// ListPlugins 按 ID 升序返回全部插件。
func (s *Store) ListPlugins() ([]Plugin, error) {
	var rows []pluginRow
	if err := s.gorm.Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Plugin, 0, len(rows))
	for _, r := range rows {
		out = append(out, pluginFromRow(r))
	}
	return out, nil
}

// PluginByID 按 ID 取一行插件。不存在时 ok=false。
func (s *Store) PluginByID(id string) (Plugin, bool, error) {
	var row pluginRow
	if err := s.gorm.Where("id = ?", id).Limit(1).Find(&row).Error; err != nil {
		return Plugin{}, false, err
	}
	if row.ID == "" {
		return Plugin{}, false, nil
	}
	return pluginFromRow(row), true, nil
}

// ActivePluginVersion 取活动版本的元数据。插件不存在、没有活动版本、或活动版本的版本行缺失时
// ok=false：三种情况调用方都只能按「没有可加载的版本」处理。
func (s *Store) ActivePluginVersion(id string) (PluginVersion, bool, error) {
	p, ok, err := s.PluginByID(id)
	if err != nil || !ok || p.ActiveVersion == "" {
		return PluginVersion{}, false, err
	}
	var row pluginVersionRow
	if err := s.gorm.Where("plugin_id = ? AND version = ?", id, p.ActiveVersion).
		Limit(1).Find(&row).Error; err != nil {
		return PluginVersion{}, false, err
	}
	if row.Version == "" {
		return PluginVersion{}, false, nil
	}
	return pluginVersionFromRow(row), true, nil
}

// PluginVersions 返回一个插件装过的全部版本，按安装时间、版本号升序。
func (s *Store) PluginVersions(id string) ([]PluginVersion, error) {
	var rows []pluginVersionRow
	if err := s.gorm.Where("plugin_id = ?", id).
		Order("installed_at").Order("version").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PluginVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, pluginVersionFromRow(r))
	}
	return out, nil
}

// SetPluginEnabled 启用或停用一个插件。行不存在时返回 ErrPluginNotFound。
func (s *Store) SetPluginEnabled(id string, enabled bool) error {
	return s.updatePluginRow(id, map[string]any{"enabled": enabled})
}

// SetPluginState 写插件状态与最近一次错误。行不存在时返回 ErrPluginNotFound。
func (s *Store) SetPluginState(id, state, lastError string) error {
	return s.updatePluginRow(id, map[string]any{"state": state, "last_error": lastError})
}

// updatePluginRow 是 plugins 行的更新入口，顺带刷新 updated_at。
//
// 存在性用一次单独的查询判：MySQL 的 RowsAffected 是「值真的变了的行数」，
// 把 enabled 重复设成同一个值会得到 0，「没这行」与「改了但值没变」就分不开。
func (s *Store) updatePluginRow(id string, updates map[string]any) error {
	updates["updated_at"] = unixNow()
	return s.withTx(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&pluginRow{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("%w：%q，用 lax plugin ls 看已安装的插件", ErrPluginNotFound, id)
		}
		return tx.Table("plugins").Where("id = ?", id).Updates(updates).Error
	})
}

// DeletePlugin 在一个事务里删掉插件行、全部版本、配置项、它带来的方言绑定，
// 以及归它管的厂商与这些厂商的路由、模型目录、方言绑定与别名。
// 插件不存在时返回 ErrPluginNotFound，别的表一行都不动。
func (s *Store) DeletePlugin(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("删除插件需要插件 ID")
	}
	return s.withTx(func(tx *gorm.DB) error {
		res := tx.Where("id = ?", id).Delete(&pluginRow{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w：%q，用 lax plugin ls 看已安装的插件", ErrPluginNotFound, id)
		}
		if err := tx.Where("plugin_id = ?", id).Delete(&pluginVersionRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("plugin_id = ?", id).Delete(&pluginOptionRow{}).Error; err != nil {
			return err
		}
		if err := deletePluginDialectsTx(tx, id); err != nil {
			return err
		}
		return deletePluginProvidersTx(tx, id)
	})
}

// deletePluginDialectsTx 删掉一个插件带来的方言绑定。
//
// 归属按方言 ID 的前缀判（`<plugin_id>/`），且只删 source='plugin' 的行：手写与能力位回填的
// 绑定是用户的事实，插件卸掉之后仍然成立。前缀在 Go 里比，不用 SQL LIKE：
// 插件 ID 里出现 `_` 或 `%` 时 LIKE 会把它们当通配符。
func deletePluginDialectsTx(tx *gorm.DB, pluginID string) error {
	var rows []modelOperationRow
	if err := tx.Select("provider", "model", "operation", "dialect_id").
		Where("source = ?", sourceOpPlugin).Find(&rows).Error; err != nil {
		return err
	}
	prefix := pluginID + "/"
	for _, r := range rows {
		if !strings.HasPrefix(r.DialectID, prefix) {
			continue
		}
		if err := tx.Where("provider = ? AND model = ? AND operation = ?",
			r.Provider, r.Model, r.Operation).Delete(&modelOperationRow{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// ModelOperation 读一个 (厂商, 模型, 操作) 的方言绑定。没有这一行时 ok=false。
func (s *Store) ModelOperation(provider, model, operation string) (ModelOperation, bool, error) {
	var row modelOperationRow
	err := s.gorm.Where("provider = ? AND model = ? AND operation = ?", provider, model, operation).
		Limit(1).Find(&row).Error
	if err != nil {
		return ModelOperation{}, false, err
	}
	if row.Operation == "" {
		return ModelOperation{}, false, nil
	}
	return modelOperationFromRow(row), true, nil
}

// SetModelOperation 写一个 (厂商, 模型, 操作) 的方言绑定，同一个主键覆盖。
// UpdatedAt 为 0 时取当前秒。
func (s *Store) SetModelOperation(op ModelOperation) error {
	op.Provider = strings.TrimSpace(op.Provider)
	op.Model = strings.TrimSpace(op.Model)
	op.Operation = strings.TrimSpace(op.Operation)
	if op.Provider == "" || op.Model == "" || op.Operation == "" {
		return errors.New("写入方言绑定需要厂商、模型与操作名：三者是 model_operations 的主键")
	}
	if op.UpdatedAt == 0 {
		op.UpdatedAt = unixNow()
	}
	return s.withTx(func(tx *gorm.DB) error { return upsertModelOperationTx(tx, op) })
}

// upsertModelOperationTx 按主键覆盖一行。列名写成 map 的理由同 PutPluginVersion：
// 结构体上的 UpdatedAt 会被 GORM 换成当前秒，覆盖掉调用方给的时间。
func upsertModelOperationTx(tx *gorm.DB, op ModelOperation) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider"}, {Name: "model"}, {Name: "operation"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"dialect_id", "capabilities_json", "limits_json", "source", "updated_at",
		}),
	}).Table("model_operations").Create(map[string]any{
		"provider": op.Provider, "model": op.Model, "operation": op.Operation,
		"dialect_id": op.DialectID, "capabilities_json": op.CapabilitiesJSON,
		"limits_json": op.LimitsJSON, "source": op.Source, "updated_at": op.UpdatedAt,
	}).Error
}

// DeleteModelOperation 删一行绑定，删不存在的行不算错误。
func (s *Store) DeleteModelOperation(provider, model, operation string) error {
	return s.withTx(func(tx *gorm.DB) error {
		return tx.Where("provider = ? AND model = ? AND operation = ?", provider, model, operation).
			Delete(&modelOperationRow{}).Error
	})
}

// ListModelOperations 列出绑定，按厂商、模型、操作升序。provider 与 model 任一为空表示不过滤。
func (s *Store) ListModelOperations(provider, model string) ([]ModelOperation, error) {
	q := s.gorm.Model(&modelOperationRow{})
	if provider != "" {
		q = q.Where("provider = ?", provider)
	}
	if model != "" {
		q = q.Where("model = ?", model)
	}
	var rows []modelOperationRow
	if err := q.Order("provider").Order("model").Order("operation").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ModelOperation, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelOperationFromRow(r))
	}
	return out, nil
}

// backfillTargets 是能触发方言绑定的能力位与对应的操作名。顺序固定，多跑几次结果一致。
var backfillTargets = []struct {
	capability string
	operation  string
}{
	{CapabilitySpeech, plugin.OpSpeechSynthesize},
	{CapabilityTranscription, plugin.OpSpeechTranscribe},
}

// BackfillSpeechDialects 把目录里已声明的语音能力补成方言绑定，返回新增的行数。
//
// 只补缺的、不覆盖已有绑定，所以可以重复调用：第二次返回 0。
// 方言选择与改造前的语音出站判定一致：厂商声明了同一条语音路由就同协议直通，
// 否则走 MiMo 的 chat 方言（路径为空的路由算没声明）。now 为 0 时取当前秒。
func (s *Store) BackfillSpeechDialects(now int64) (int, error) {
	if now == 0 {
		now = unixNow()
	}
	inserted := 0
	err := s.withTx(func(tx *gorm.DB) error {
		var models []providerModelRow
		if err := tx.Select("provider", "model", "capabilities").Find(&models).Error; err != nil {
			return err
		}
		var existing []modelOperationRow
		if err := tx.Select("provider", "model", "operation").Find(&existing).Error; err != nil {
			return err
		}
		have := make(map[string]bool, len(existing))
		for _, r := range existing {
			have[operationKey(r.Provider, r.Model, r.Operation)] = true
		}
		direct, err := directSpeechProvidersTx(tx)
		if err != nil {
			return err
		}

		for _, m := range models {
			caps := splitList(m.Capabilities)
			for _, target := range backfillTargets {
				if !containsString(caps, target.capability) {
					continue
				}
				key := operationKey(m.Provider, m.Model, target.operation)
				if have[key] {
					continue
				}
				dialect := backfillDialect(target.operation, direct[providerOperationKey(m.Provider, target.operation)])
				// 用 map 写：结构体上的 UpdatedAt 会被 GORM 换成当前秒，这里要落 now。
				if err := tx.Table("model_operations").Create(map[string]any{
					"provider": m.Provider, "model": m.Model, "operation": target.operation,
					"dialect_id": dialect, "capabilities_json": "{}", "limits_json": "{}",
					"source": sourceOpDeclared, "updated_at": now,
				}).Error; err != nil {
					return err
				}
				have[key] = true
				inserted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// directSpeechProvidersTx 读出哪些厂商声明了自己那条语音协议路由，返回「厂商/操作」集合。
// 路径为空的路由算没声明：拼不出地址的路由发不出请求，网关的同协议直通也要求路径非空。
func directSpeechProvidersTx(tx *gorm.DB) (map[string]bool, error) {
	var routes []providerRouteRow
	if err := tx.Select("provider", "protocol", "path").Find(&routes).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(routes))
	for _, r := range routes {
		if strings.TrimSpace(r.Path) == "" {
			continue
		}
		switch r.Protocol {
		case translate.ProtocolSpeech:
			out[providerOperationKey(r.Provider, plugin.OpSpeechSynthesize)] = true
		case translate.ProtocolTranscriptions:
			out[providerOperationKey(r.Provider, plugin.OpSpeechTranscribe)] = true
		}
	}
	return out, nil
}

// backfillDialect 按有没有同协议路由选方言：有就同协议直通，没有就走 MiMo 的 chat 方言。
func backfillDialect(operation string, direct bool) string {
	if operation == plugin.OpSpeechSynthesize {
		if direct {
			return plugin.DialectOpenAISpeech
		}
		return plugin.DialectMiMoChatSpeech
	}
	if direct {
		return plugin.DialectOpenAITranscriptions
	}
	return plugin.DialectMiMoChatASR
}

func providerOperationKey(provider, operation string) string {
	return provider + "\x00" + operation
}

func operationKey(provider, model, operation string) string {
	return provider + "\x00" + model + "\x00" + operation
}

// PluginOptions 读一个插件的配置项。没有的行不返回，宿主按 manifest 的 default 兜底。
// 字符串值去掉 JSON 引号，其余形状原样给出 JSON 文本。
func (s *Store) PluginOptions(pluginID string) (map[string]string, error) {
	var rows []pluginOptionRow
	if err := s.gorm.Where("plugin_id = ?", pluginID).Order("key").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		var decoded any
		if err := json.Unmarshal([]byte(r.ValueJSON), &decoded); err == nil {
			if text, ok := decoded.(string); ok {
				out[r.Key] = text
				continue
			}
		}
		out[r.Key] = r.ValueJSON
	}
	return out, nil
}

// SetPluginOption 写一个配置项，现有的键就覆盖。
func (s *Store) SetPluginOption(pluginID, key string, value any) error {
	pluginID = strings.TrimSpace(pluginID)
	key = strings.TrimSpace(key)
	if pluginID == "" || key == "" {
		return errors.New("配置项需要插件 ID 与键名")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("配置项 %s 不能序列化成 JSON: %v", key, err)
	}
	row := pluginOptionRow{PluginID: pluginID, Key: key, ValueJSON: string(encoded), UpdatedAt: unixNow()}
	return s.gorm.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin_id"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value_json", "updated_at"}),
	}).Create(&row).Error
}

// DeletePluginOption 删一个配置项，删不到不报错（回到 manifest 的默认值）。
func (s *Store) DeletePluginOption(pluginID, key string) error {
	return s.gorm.Where("plugin_id = ? AND key = ?", pluginID, key).Delete(&pluginOptionRow{}).Error
}
