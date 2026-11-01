package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"lax/internal/plugin"
	"lax/internal/translate"
)

// 插件安装的目录落库：一家厂商、它的路由、模型目录、按操作的方言绑定与别名，一次事务写完。
//
// 与手写入口的分界是不覆盖。插件带来的事实只在库里还没有的时候写进去，凡是与已有写法不一致
// 的地方一律整包失败并给下一步命令：静默改写会让「谁写的这一行」变得不可追溯，
// 用户下次 `provider ls` 看到的就是自己没写过的地址。归属记在 providers.plugin_id 上，
// DeletePlugin 据此连带清理。

// PluginCatalog 是一次插件安装要落库的目录事实：一家厂商、它的路由、模型目录与方言绑定。
type PluginCatalog struct {
	Provider   string          // 厂商 ID，插件内唯一
	Name       string          // 展示名，落库到 providers.note
	BaseURL    string          // 厂商地址，http(s) 绝对地址
	AuthHeader string          // 凭据头名，空表示按线协议推导
	AuthScheme string          // 凭据写法：raw、bearer 或 bearer-semicolon
	ModelsPath string          // 模型列表接口路径，可空
	Routes     []ProviderRoute // 插件声明的路由（Protocol + Path）
	Models     []PluginModelEntry
}

// PluginModelEntry 是插件目录里的一个模型。
type PluginModelEntry struct {
	Model      string           // 客户端可见名字
	UpstreamID string           // 真正发上游的名字
	Operations []ModelOperation // 每个操作一条：Operation、DialectID、CapabilitiesJSON
}

// ApplyPluginCatalog 在一个事务里建厂商、路由、模型目录、方言绑定与别名。
// pluginID 记进 providers.plugin_id，删除插件时据此连带清理。
//
// 冲突判定的完整清单（任何一条不满足都整笔回滚，库里不留半条）：
//   - 厂商已存在且 base_url 与插件声明不同；
//   - 厂商已存在且它的 plugin_id 非空、又不是这个插件；
//   - 同名 (厂商, 协议) 路由已存在且路径不同；
//   - 同 (厂商, 上游模型名, 操作) 的绑定已存在、方言不同，且那一行不是插件写的；
//   - 同名别名已存在且指向别的目标。
//
// 幂等：重复调用只补缺的部分，行数与第一次相同。
func (s *Store) ApplyPluginCatalog(pluginID string, catalog PluginCatalog, source string) error {
	pluginID = strings.TrimSpace(pluginID)
	source = strings.TrimSpace(source)
	if pluginID == "" {
		return errors.New("写入插件目录需要插件 ID：providers.plugin_id 靠它记归属，删除插件时据此连带清理")
	}
	if source == "" {
		return errors.New("写入插件目录需要来源标记（安装时传 plugin）：空来源写进 provider_models 与 model_operations 之后，这一行就分不清是插件带来的还是用户手写的")
	}
	provider := strings.TrimSpace(catalog.Provider)
	if provider == "" {
		return errors.New("插件目录需要厂商 ID：providers 以厂商 ID 为主键，空名字定位不到任何一家厂商")
	}
	base, err := NormalizeBaseURL(catalog.BaseURL)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", provider, err)
	}
	modelsPath, err := NormalizeEndpoint(catalog.ModelsPath)
	if err != nil {
		return fmt.Errorf("厂商 %s 的模型列表地址：%w", provider, err)
	}
	authHeader, authScheme, err := normalizeAuth(catalog.AuthHeader, catalog.AuthScheme)
	if err != nil {
		return fmt.Errorf("厂商 %s 的凭据头声明：%w", provider, err)
	}
	// 路由走与 CreateProvider / SetProvider 同一套规整：斜杠写法不拒绝，路径为空与协议名不认识当场报错。
	routes, err := prepareRoutes(provider, catalog.Routes)
	if err != nil {
		return err
	}
	entries, err := normalizePluginModels(provider, catalog.Models)
	if err != nil {
		return err
	}

	now := unixNow()
	return s.withTx(func(tx *gorm.DB) error {
		if err := applyCatalogProviderTx(tx, pluginID, provider, catalog.Name, base, authHeader, authScheme, modelsPath); err != nil {
			return err
		}
		if err := applyCatalogRoutesTx(tx, provider, routes); err != nil {
			return err
		}
		if err := applyCatalogModelsTx(tx, provider, entries, source, now); err != nil {
			return err
		}
		return applyCatalogAliasesTx(tx, entries, now)
	})
}

// normalizePluginModels 去空白并补齐每条绑定上的厂商与上游模型名，同时把缺字段的条目挡在动库之前。
func normalizePluginModels(provider string, in []PluginModelEntry) ([]PluginModelEntry, error) {
	out := make([]PluginModelEntry, 0, len(in))
	for _, e := range in {
		m := PluginModelEntry{
			Model:      strings.TrimSpace(e.Model),
			UpstreamID: strings.TrimSpace(e.UpstreamID),
		}
		if m.UpstreamID == "" {
			return nil, fmt.Errorf("厂商 %s 的模型 %q 缺少上游模型名：目录以 (厂商, 上游模型名) 为主键，空名字写不进去",
				provider, e.Model)
		}
		if m.Model == "" {
			return nil, fmt.Errorf("厂商 %s 的模型 %q 缺少客户端可见名字：客户端按这个名字请求，缺了它也没有别名可写",
				provider, m.UpstreamID)
		}
		for _, raw := range e.Operations {
			op := raw
			op.Provider, op.Model = provider, m.UpstreamID
			op.Operation = strings.TrimSpace(op.Operation)
			op.DialectID = strings.TrimSpace(op.DialectID)
			if op.Operation == "" || op.DialectID == "" {
				return nil, fmt.Errorf("厂商 %s 的模型 %s 有一条绑定缺少操作名或方言 ID：model_operations 以 (厂商, 模型, 操作) 为主键，方言 ID 是必填值",
					provider, m.UpstreamID)
			}
			m.Operations = append(m.Operations, op)
		}
		out = append(out, m)
	}
	return out, nil
}

// applyCatalogProviderTx 建厂商行，已存在时只补 plugin_id 与凭据头声明。
//
// base_url 与凭据头是厂商事实，插件与库里不一致就不写：改哪一边都是人的决定，
// 这里只说明两边各是什么、下一步能敲什么。插件没声明的项一律不动（空值不清掉已有设置）。
func applyCatalogProviderTx(tx *gorm.DB, pluginID, provider, name, base, authHeader, authScheme, modelsPath string) error {
	var row providerRow
	if err := tx.Where("name = ?", provider).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.Name == "" {
		return tx.Create(&providerRow{
			Name:       provider,
			BaseURL:    base,
			Note:       strings.TrimSpace(name),
			ModelsPath: modelsPath,
			AuthHeader: authHeader,
			AuthScheme: authScheme,
			PluginID:   pluginID,
		}).Error
	}
	if row.BaseURL != base {
		return fmt.Errorf("厂商 %s 已经存在，地址是 %s，插件声明的是 %s：改插件 manifest 里的 base_url 与它对齐，或先 lax provider rm %s 再把厂商交给插件管（删厂商会一并删掉它的路由与模型目录）",
			provider, row.BaseURL, base, provider)
	}
	// 凭据头只在插件声明了它的时候参与比对：没声明表示「按线协议推导」，不改已有设置。
	if authHeader != "" && row.AuthHeader != "" &&
		(row.AuthHeader != authHeader || row.AuthScheme != authScheme) {
		return fmt.Errorf("厂商 %s 的凭据头已经是 %s（写法 %s），插件声明的是 %s（写法 %s）：改插件 manifest 里的 auth 与它对齐，或改库里的声明（lax provider set %s --auth-header %s --auth-scheme %s）",
			provider, row.AuthHeader, row.AuthScheme, authHeader, authScheme, provider, row.AuthHeader, row.AuthScheme)
	}
	fillAuth := authHeader != "" && row.AuthHeader == ""
	if row.PluginID == pluginID {
		// 同一个插件：只补缺的凭据头，不动其它列。
		if !fillAuth {
			return nil
		}
		return updateProviderAuthTx(tx, provider, authHeader, authScheme)
	}
	if row.PluginID != "" {
		return fmt.Errorf("厂商 %s 已经属于插件 %s，不能再归给 %s：先 lax plugin rm %s 卸掉原来的插件，或改插件 manifest 里的厂商 ID",
			provider, row.PluginID, pluginID, row.PluginID)
	}
	// 只补归属与缺的凭据头：地址与插件声明的相同，其余列是用户或同步写的，插件安装不碰。
	updates := map[string]any{"plugin_id": pluginID}
	if fillAuth {
		updates["auth_header"] = authHeader
		updates["auth_scheme"] = authScheme
	}
	return tx.Model(&providerRow{}).Where("name = ?", provider).Updates(updates).Error
}

// updateProviderAuthTx 只改凭据头两列，其余列原样。
func updateProviderAuthTx(tx *gorm.DB, provider, authHeader, authScheme string) error {
	return tx.Model(&providerRow{}).Where("name = ?", provider).Updates(map[string]any{
		"auth_header": authHeader,
		"auth_scheme": authScheme,
	}).Error
}

// applyCatalogRoutesTx 按 (厂商, 协议) 补缺失的路由。已存在但路径不同就报错：
// 路由决定了请求发到哪个端点，改了会连带改掉所有走这条协议的账号的行为。
func applyCatalogRoutesTx(tx *gorm.DB, provider string, routes []ProviderRoute) error {
	for _, r := range routes {
		var row providerRouteRow
		if err := tx.Where("provider = ? AND protocol = ?", provider, r.Protocol).
			Limit(1).Find(&row).Error; err != nil {
			return err
		}
		if row.Provider == "" {
			if err := tx.Create(&providerRouteRow{
				Provider:   provider,
				Protocol:   r.Protocol,
				Path:       r.Path,
				ModelsPath: r.ModelsPath,
			}).Error; err != nil {
				return err
			}
			continue
		}
		if row.Path == r.Path {
			continue
		}
		return fmt.Errorf("厂商 %s 的 %s 路由指向 %s，插件声明的是 %s：把已有路由改成插件声明的路径（lax provider set %s --%s %s），或改插件 manifest 里的这条路由",
			provider, r.Protocol, row.Path, r.Path, provider, routeSetFlag(r.Protocol), r.Path)
	}
	return nil
}

// seedModelCapabilitiesTx 把目录里按操作声明的能力翻成 provider_models 的能力位。
//
// 能力位是语音入站的第一道闸（见 gateway.checkSpeechInbound）：不写它，插件声明了
// speech.synthesize 的模型仍然会被本地拒掉，还得用户手敲一次 model capability add。
// 只并进不清：已有声明可能是探活或用户写的，只追加 supported 为 true 的项。
func seedModelCapabilitiesTx(tx *gorm.DB, provider, model string, op ModelOperation, now int64) error {
	capability := capabilityForOperation(op.Operation)
	if capability == "" {
		return nil
	}
	var declared struct {
		Supported any `json:"supported"`
		Formats   any `json:"formats"`
		Voices    any `json:"voices"`
	}
	if strings.TrimSpace(op.CapabilitiesJSON) != "" {
		if err := json.Unmarshal([]byte(op.CapabilitiesJSON), &declared); err != nil {
			return fmt.Errorf("模型 %s 的 %s 能力声明不是 JSON: %v", model, op.Operation, err)
		}
	}
	if !declaredTrue(declared.Supported) {
		return nil
	}

	var row providerModelRow
	if err := tx.Where("provider = ? AND model = ?", provider, model).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.Model == "" {
		return nil
	}
	updates := map[string]any{
		"capabilities": joinList(OrderCapabilities(mergeList(splitList(row.Capabilities), []string{capability}))),
		"updated_at":   now,
	}
	if voices := declaredList(declared.Voices); voices != nil {
		updates["voices"] = joinList(mergeList(splitList(row.Voices), voices))
	}
	if formats := declaredList(declared.Formats); formats != nil {
		updates["audio_formats"] = joinList(mergeList(splitList(row.AudioFormats), formats))
	}
	return tx.Model(&providerModelRow{}).Where("provider = ? AND model = ?", provider, model).
		Updates(updates).Error
}

// capabilityForOperation 给出操作对应的能力位。对话与向量化由目录同步写，这里只补语音两条。
func capabilityForOperation(operation string) string {
	switch operation {
	case plugin.OpSpeechSynthesize:
		return CapabilitySpeech
	case plugin.OpSpeechTranscribe:
		return CapabilityTranscription
	}
	return ""
}

// declaredTrue 读三态能力值里的布尔；"unknown" 与缺失都按不支持处理。
func declaredTrue(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

// declaredList 读能力声明里的列表；"unknown" 与缺失都返回 nil（表示不改这一列）。
func declaredList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// applyCatalogModelsTx 写模型目录与方言绑定。
func applyCatalogModelsTx(tx *gorm.DB, provider string, entries []PluginModelEntry, source string, now int64) error {
	for _, e := range entries {
		// 只插不改：已有的目录行可能带着同步或探活写下的长度、价格与能力位，
		// 插件目录只声明「这个模型存在」，覆盖等于把那些列清空。
		if err := insertProviderModelTx(tx, provider, e.UpstreamID, source, now); err != nil {
			return err
		}
		for _, op := range e.Operations {
			if err := applyCatalogOperationTx(tx, op, source, now); err != nil {
				return err
			}
			if err := seedModelCapabilitiesTx(tx, provider, e.UpstreamID, op, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyCatalogOperationTx 按 (厂商, 模型, 操作) 写一条绑定，手写的行不许被插件改方言。
func applyCatalogOperationTx(tx *gorm.DB, op ModelOperation, source string, now int64) error {
	var row modelOperationRow
	if err := tx.Where("provider = ? AND model = ? AND operation = ?",
		op.Provider, op.Model, op.Operation).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.Operation != "" && row.DialectID != op.DialectID && row.Source != sourceOpPlugin {
		return fmt.Errorf("厂商 %s 的模型 %s 在 %s 上已经绑到方言 %s（来源 %s），插件要绑的是 %s：手写的绑定优先，先用 lax model capability set %s/%s --capability <能力> --dialect <方言> 改掉它，或把这条操作从插件目录里去掉",
			op.Provider, op.Model, op.Operation, row.DialectID, row.Source, op.DialectID, op.Provider, op.Model)
	}
	op.Source = source
	op.UpdatedAt = now
	return upsertModelOperationTx(tx, op)
}

// applyCatalogAliasesTx 把客户端可见名字写成别名。名字与上游名相同就不写：
// 那种情况下名字本身就是目录里的一行，再写一条指向自己的别名只会挡住以后给这个名字加别名。
func applyCatalogAliasesTx(tx *gorm.DB, entries []PluginModelEntry, now int64) error {
	for _, e := range entries {
		if e.Model == e.UpstreamID {
			continue
		}
		if err := writePluginAliasTx(tx, e.Model, e.UpstreamID, now); err != nil {
			return err
		}
	}
	return nil
}

// writePluginAliasTx 是事务里的别名写入：目标一致就幂等，指向别的目标就报错。
//
// 不复用 SetModelAlias：那个入口自己开事务并带遮蔽校验，SQLite 后端只有一个连接，
// 在外层事务里再开一个会自己等自己；而且它的语义是「覆盖目标」，与这里「冲突即失败」相反。
func writePluginAliasTx(tx *gorm.DB, alias, target string, now int64) error {
	var row modelAliasRow
	if err := tx.Where("alias = ?", alias).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.Alias == "" {
		return tx.Create(&modelAliasRow{Alias: alias, Target: target, CreatedAt: now}).Error
	}
	if row.Target == target {
		return nil
	}
	return fmt.Errorf("别名 %s 已经指向 %s，插件要让客户端用 %s 请求 %s：改掉这条别名（lax model alias set %s %s）或删掉它（lax model alias unset %s），也可以改插件目录里的模型名",
		alias, row.Target, alias, target, alias, target, alias)
}

// deletePluginProvidersTx 删掉归属这个插件的厂商，连同它们的路由、模型目录、方言绑定，
// 以及目标落在这些模型上的别名。
//
// 归属只认 providers.plugin_id：用户 `provider add` 建的同名厂商没有这个标记，
// 后来手工把 plugin_id 改掉的行也不再归插件，两者都留着。
func deletePluginProvidersTx(tx *gorm.DB, pluginID string) error {
	var names []string
	if err := tx.Model(&providerRow{}).Where("plugin_id = ?", pluginID).
		Order("name").Pluck("name", &names).Error; err != nil {
		return err
	}
	for _, name := range names {
		// 先读模型名再删目录行：别名按目标匹配，目录行删掉之后这个厂商有过哪些模型名就查不回来了。
		var models []string
		if err := tx.Model(&providerModelRow{}).Where("provider = ?", name).
			Order("model").Pluck("model", &models).Error; err != nil {
			return err
		}
		if len(models) > 0 {
			if err := tx.Where("target IN ?", models).Delete(&modelAliasRow{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("provider = ?", name).Delete(&providerModelRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("provider = ?", name).Delete(&modelOperationRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("provider = ?", name).Delete(&providerRouteRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("name = ?", name).Delete(&providerRow{}).Error; err != nil {
			return err
		}
		// 账号不删，但与 provider rm --force 一样停用：地址与默认协议都取自厂商行，
		// 行没了就只剩必然失败的请求。
		ids, err := accountIDsByProviderTx(tx, name)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := setEnabledTx(tx, id, false, pluginRemovedReason); err != nil {
				return err
			}
		}
	}
	return nil
}

// pluginRemovedReason 是插件删除时停用账号写进 last_error 的话。
const pluginRemovedReason = "插件已删除，厂商随之下线；重新安装插件后 account enable 即可恢复"

// routeSetFlag 把线协议换成 `provider set` 上对应的 flag 名，错误文案据此给出能直接敲的命令。
// 不复用 protocolFlagName：那份映射只覆盖算默认协议用得上的几条，语音两条在命令面上的 flag
// 是 --speech 与 --transcriptions，而不是 --openai-speech。
func routeSetFlag(protocol string) string {
	switch protocol {
	case translate.ProtocolChat:
		return "openai"
	case translate.ProtocolResponses:
		return "responses"
	case translate.ProtocolAnthropic:
		return "anthropic"
	case translate.ProtocolGemini:
		return "gemini"
	case translate.ProtocolSystemOne:
		return "systemone"
	case translate.ProtocolEmbeddings:
		return "embeddings"
	case translate.ProtocolSpeech:
		return "speech"
	case translate.ProtocolTranscriptions:
		return "transcriptions"
	}
	return protocol
}
