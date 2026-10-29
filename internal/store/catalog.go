package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"switchboard/internal/translate"
)

// 模型事实与账号可达性分两张表存（迁移 0015，见 DESIGN 5.13）：
//
//   - provider_models 是目录：上下文长度、最大输出、支持的协议、能力位与厂商级价格。同一个模型
//     在多家账号下只有一份，改一次就够。
//   - account_models 是可达性与账号侧覆盖：这个账号能不能用这个模型（access），以及在这个账号上
//     与目录不同的协议、长度与价格（NULL 表示继承目录）。
//
// 选号读的「这个账号提供哪些模型」因此是 account_models 里 access='allowed' 的行集合，
// 不再是一列 JSON 数组。

// 目录行的来源。取值同时决定覆盖顺序：manual 最强，sync 次之，probe 最弱。
// 一次同步拿到的上游清单不该把用户手填的名字与元数据冲掉，而探活只是为了补一个目录里没有的名字，
// 所以 UpsertProviderModels 按这个顺序判断能不能写（见 providerModelUpsert 的 WHERE）。
const (
	// SourceSync 表示这一行是 `model sync` 从上游模型清单里拿到的。
	SourceSync = "sync"
	// SourceManual 表示这一行是用户手填的（`account add --models`、`model ... --models` 一类）。
	SourceManual = "manual"
	// SourceProbe 表示这一行是探活顺手补上的：模型能打过，但上游清单里没有它。
	SourceProbe = "probe"
)

// 账号对某个模型的可达性。只有不会变的结论才进这里：能用的、明确无权、明确不存在，
// 以及没能得出定论的临时错误。凭据被拒（旧词表里的 auth）不是模型层面的结论，落成 error。
const (
	// AccessAllowed 表示这个账号能用这个模型，选号只看这一类。
	AccessAllowed = "allowed"
	// AccessForbidden 表示上游说不含这个模型（套餐或权限），凭据本身还能用。
	AccessForbidden = "forbidden"
	// AccessMissing 表示上游不认识这个模型名。
	AccessMissing = "missing"
	// AccessError 表示没得出定论的失败，下一轮要重探。
	AccessError = "error"
)

// 迁移 0015 之前的探活状态词表。库里已经不再存这些值，只留给尚未改完的
// internal/gateway 与 internal/cli 编译：它们把探活结论放进 ModelProbe.Status 再写回来，
// 写入口 SetModelStatus 负责把旧词表映射成 access，读出来的 AccountModel.Status 再由 access 反推。
//
// Deprecated: 新代码用 Access* 那组常量与 AccountModel.Access。
const (
	// ModelSynced 表示刚从上游列表里拉下来，还没探活。
	ModelSynced = "synced"
	// ModelUsable 表示探活通过，账号能用这个模型。
	ModelUsable = "usable"
	// ModelForbidden 表示上游拒绝：套餐不含或无权使用。
	ModelForbidden = "forbidden"
	// ModelNotFound 表示上游不认识这个模型名。
	ModelNotFound = "notfound"
	// ModelAuthFailed 表示凭据在这个账号上不成立。
	ModelAuthFailed = "auth"
	// ModelError 表示其他临时错误，结论不可用。
	ModelError = "error"
)

// ProviderModel 是目录里的一行：某个厂商的一个模型有哪些事实。
// Protocols 与 Capabilities 在库里是逗号分隔的字符串，读出来已经拆好。
// 四个价格是厂商级默认价（每百万 token），空表示没填。
type ProviderModel struct {
	Provider        string
	Model           string
	Source          string
	ContextWindow   *int64
	MaxOutput       *int64
	Protocols       []string
	Capabilities    []string
	PriceIn         *float64
	PriceOut        *float64
	PriceCacheRead  *float64
	PriceCacheWrite *float64
	// UpdatedAt 是这一行最后一次写入的 Unix 秒。迁移回填的行是 0：旧表没有时间可用。
	UpdatedAt int64
}

// AccountModel 是一个账号在一个模型上的可达性与覆盖值。
//
// Access 是 allowed / forbidden / missing / error；Protocol 是探活发现的账号级协议覆盖，
// 空串表示按目录与厂商默认协议算（见 resolveModelProtocol）。ContextWindow、MaxOutput 与四个
// 价格是这一行上的覆盖值，nil 表示继承目录（provider_models）。
//
// Status 是迁移 0015 之前的状态词表，由 Access 与 CheckedAt 反推，只为尚未改完的
// internal/gateway 与 internal/cli 保留。
//
// Deprecated: 用 Access。
type AccountModel struct {
	AccountID       int64
	Model           string
	Access          string
	HTTPStatus      int
	Error           string
	CheckedAt       int64
	Protocol        string
	ContextWindow   *int64
	MaxOutput       *int64
	PriceIn         *float64
	PriceOut        *float64
	PriceCacheRead  *float64
	PriceCacheWrite *float64
	Status          string
}

// ModelInfo 是一个账号上一个模型的有效值：能不能用，加上合并了目录与覆盖之后的元数据。
// 取值优先级一律是「账号覆盖 → 目录 → 厂商默认」（见 DESIGN 5.13）。
type ModelInfo struct {
	Model           string
	Access          string
	Protocol        string
	ContextWindow   *int64
	MaxOutput       *int64
	PriceIn         *float64
	PriceOut        *float64
	PriceCacheRead  *float64
	PriceCacheWrite *float64
}

// accessValues 是 access 列的取值白名单，校验与错误文案都读它。
func accessValues() []string {
	return []string{AccessAllowed, AccessForbidden, AccessMissing, AccessError}
}

func validAccess(access string) bool {
	switch access {
	case AccessAllowed, AccessForbidden, AccessMissing, AccessError:
		return true
	}
	return false
}

// LegacyStatus 把 access 反推成迁移 0015 之前的状态词表，只给尚未改完的调用方看：
// allowed 且探过就是 usable、没探过就是 synced，其余一一对应（auth 已经无从区分，落成 error）。
func LegacyStatus(access string, checkedAt int64) string {
	switch access {
	case AccessAllowed:
		if checkedAt > 0 {
			return ModelUsable
		}
		return ModelSynced
	case AccessForbidden:
		return ModelForbidden
	case AccessMissing:
		return ModelNotFound
	default:
		return ModelError
	}
}

// accessFromStatus 是 LegacyStatus 的反方向，供旧写入口 SetModelStatus 用：
// usable 与 synced 都是 allowed（同步拿到清单就算可达，探活通过也是可达），
// auth 与 error 落成 error，另两个一一对应。
func accessFromStatus(status string) string {
	switch status {
	case AccessAllowed, ModelUsable, ModelSynced:
		return AccessAllowed
	case AccessForbidden:
		return AccessForbidden
	case AccessMissing, ModelNotFound:
		return AccessMissing
	default:
		return AccessError
	}
}

// splitList 把逗号分隔的列拆成切片：去空白、丢空项，保留原有顺序。
func splitList(joined string) []string {
	if strings.TrimSpace(joined) == "" {
		return nil
	}
	parts := strings.Split(joined, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// joinList 是 splitList 的反方向：去空白、丢空项、去重后按逗号拼起来。
// 落库的写法只有一份，读回来才不会因为多一个逗号多出一个空协议。
func joinList(values []string) string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return strings.Join(out, ",")
}

// normalizeModelNames 规整一组模型名：去首尾空白、丢空项、按输入顺序去重。
// 模型名会原样发给上游，不做大小写归一（`GLM-4.6` 与 `glm-4.6` 在上游看来是两个名字）。
func normalizeModelNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// providerModelUpsert 按 (provider, model) 写一行目录。
//
// 来源不降级，强弱顺序是 manual > sync > probe：库里已经是 manual 的行不会被 sync 或 probe 改写，
// manual 可以覆盖 sync 与 probe，sync 可以覆盖 probe（上游清单比「探活顺手补的名字」更权威）。
// 同等级重复写等于刷新那一行。等级判定只写在末尾的 WHERE 里，Go 侧不另写一份。
// 其余列只更新非零/非空的那几个：传 nil 的长度与价格、空的协议与能力位都表示「这一项不知道」，
// 保留库里已有的值，否则一次只带名字的同步会把探活补上的元数据抹掉。
// 末尾那句 WHERE 只判断传进来的来源够不够格改写现有行，够格时整行按上面的 COALESCE 更新。
const providerModelUpsert = `INSERT INTO provider_models
	(provider, model, source, context_window, max_output, protocols, capabilities,
	 price_in, price_out, price_cache_read, price_cache_write, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (provider, model) DO UPDATE SET
		source = excluded.source,
		context_window = COALESCE(excluded.context_window, provider_models.context_window),
		max_output = COALESCE(excluded.max_output, provider_models.max_output),
		protocols = CASE WHEN excluded.protocols <> '' THEN excluded.protocols ELSE provider_models.protocols END,
		capabilities = CASE WHEN excluded.capabilities <> '' THEN excluded.capabilities ELSE provider_models.capabilities END,
		price_in = COALESCE(excluded.price_in, provider_models.price_in),
		price_out = COALESCE(excluded.price_out, provider_models.price_out),
		price_cache_read = COALESCE(excluded.price_cache_read, provider_models.price_cache_read),
		price_cache_write = COALESCE(excluded.price_cache_write, provider_models.price_cache_write),
		updated_at = excluded.updated_at
	WHERE CASE excluded.source WHEN 'manual' THEN 2 WHEN 'sync' THEN 1 ELSE 0 END
	   >= CASE provider_models.source WHEN 'manual' THEN 2 WHEN 'sync' THEN 1 ELSE 0 END`

// UpsertProviderModels 按 (provider, model) 写入目录，来源规则见 providerModelUpsert。
// 全部行在同一个事务里写：一个厂商的一批模型要么都进目录，要么一行都不进。
// 传进来的 Provider 会被形参覆盖（一批行只属于一家厂商），UpdatedAt 为 0 时取当前时间。
func (s *Store) UpsertProviderModels(provider string, models []ProviderModel) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("写模型目录需要厂商名，空厂商名无法定位是哪家上游的模型事实")
	}
	return s.withTx(func(tx *sql.Tx) error {
		for _, m := range models {
			m.Provider = provider
			if err := upsertProviderModelTx(tx, m); err != nil {
				return err
			}
		}
		return nil
	})
}

func upsertProviderModelTx(tx *sql.Tx, m ProviderModel) error {
	m.Model = strings.TrimSpace(m.Model)
	if m.Model == "" {
		return fmt.Errorf("厂商 %q 的模型名不能为空：目录以 (厂商, 模型) 为主键", m.Provider)
	}
	switch m.Source {
	case SourceSync, SourceManual, SourceProbe:
	default:
		return fmt.Errorf("厂商 %q 的模型 %q 来源是 %q，只能是 sync、manual 或 probe",
			m.Provider, m.Model, m.Source)
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = time.Now().Unix()
	}
	_, err := tx.Exec(providerModelUpsert, m.Provider, m.Model, m.Source,
		m.ContextWindow, m.MaxOutput, joinList(m.Protocols), joinList(m.Capabilities),
		m.PriceIn, m.PriceOut, m.PriceCacheRead, m.PriceCacheWrite, m.UpdatedAt)
	return err
}

// declareModelsTx 把一组名字落成「这个账号声明了这些模型」：目录里补 source='manual' 的行，
// 可达性表里把该账号的这些名字标成 allowed。`account add --models` 与 ReplaceModels 共用它。
// 厂商名为空时什么都不写（见 CreateAccount 的说明）；目录里已有的行不会被这份手填清单改写事实。
func declareModelsTx(tx *sql.Tx, accountID int64, provider string, models []string) error {
	names := normalizeModelNames(models)
	if len(names) == 0 || provider == "" {
		return nil
	}
	for _, name := range names {
		if err := upsertProviderModelTx(tx, ProviderModel{
			Provider: provider,
			Model:    name,
			Source:   SourceManual,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO account_models (account_id, model, access, http_status, error, checked_at)
			VALUES (?, ?, ?, 0, '', 0)
			ON CONFLICT (account_id, model) DO UPDATE SET access = excluded.access`,
			accountID, name, AccessAllowed); err != nil {
			return err
		}
	}
	return nil
}

// ProviderModels 返回一家厂商的目录，按模型名升序。
func (s *Store) ProviderModels(provider string) ([]ProviderModel, error) {
	rows, err := s.db.Query(`SELECT provider, model, source, context_window, max_output,
		protocols, capabilities, price_in, price_out, price_cache_read, price_cache_write, updated_at
		FROM provider_models WHERE provider = ? ORDER BY model`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderModel
	for rows.Next() {
		var m ProviderModel
		var protocols, capabilities string
		if err := rows.Scan(&m.Provider, &m.Model, &m.Source, &m.ContextWindow, &m.MaxOutput,
			&protocols, &capabilities, &m.PriceIn, &m.PriceOut, &m.PriceCacheRead, &m.PriceCacheWrite,
			&m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Protocols = splitList(protocols)
		m.Capabilities = splitList(capabilities)
		out = append(out, m)
	}
	return out, rows.Err()
}

// providerModel 读目录里的一行。第二个返回值是「有没有这一行」，调用方据此决定要不要回退，
// 因此没有这一行不算错误。
func (s *Store) providerModel(provider, model string) (ProviderModel, bool, error) {
	if provider == "" {
		return ProviderModel{}, false, nil
	}
	var m ProviderModel
	var protocols, capabilities string
	err := s.db.QueryRow(`SELECT provider, model, source, context_window, max_output,
		protocols, capabilities, price_in, price_out, price_cache_read, price_cache_write, updated_at
		FROM provider_models WHERE provider = ? AND model = ?`, provider, model).Scan(
		&m.Provider, &m.Model, &m.Source, &m.ContextWindow, &m.MaxOutput,
		&protocols, &capabilities, &m.PriceIn, &m.PriceOut, &m.PriceCacheRead, &m.PriceCacheWrite,
		&m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderModel{}, false, nil
	}
	if err != nil {
		return ProviderModel{}, false, err
	}
	m.Protocols = splitList(protocols)
	m.Capabilities = splitList(capabilities)
	return m, true, nil
}

// accountModelSelect 是读可达性行的唯一列清单，几个读入口共用它。
const accountModelSelect = `SELECT account_id, model, access, http_status, error, checked_at, protocol,
	context_window, max_output, price_in, price_out, price_cache_read, price_cache_write
	FROM account_models`

// AccountModels 返回一个账号的模型行，按模型名升序。
func (s *Store) AccountModels(accountID int64) ([]AccountModel, error) {
	rows, err := s.db.Query(accountModelSelect+` WHERE account_id = ? ORDER BY model`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccountModels(rows)
}

// AllAccountModels 返回全部账号的模型行，按模型名升序。
func (s *Store) AllAccountModels() ([]AccountModel, error) {
	rows, err := s.db.Query(accountModelSelect + ` ORDER BY model, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccountModels(rows)
}

func scanAccountModels(rows *sql.Rows) ([]AccountModel, error) {
	var out []AccountModel
	for rows.Next() {
		var m AccountModel
		if err := rows.Scan(&m.AccountID, &m.Model, &m.Access, &m.HTTPStatus, &m.Error, &m.CheckedAt,
			&m.Protocol, &m.ContextWindow, &m.MaxOutput, &m.PriceIn, &m.PriceOut,
			&m.PriceCacheRead, &m.PriceCacheWrite); err != nil {
			return nil, err
		}
		m.Status = LegacyStatus(m.Access, m.CheckedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AllowedModels 返回一个账号能用的模型名，按名字升序：选号与 `/v1/models` 都读这一份。
func (s *Store) AllowedModels(accountID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT model FROM account_models
		WHERE account_id = ? AND access = ? ORDER BY model`, accountID, AccessAllowed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// fillDerivedModels 给账号列表填上派生字段 Models（access='allowed' 的名字集合）。
// 一次查询取回全部账号的 allowed 名字再按 account_id 归位：账号再多也只多一次查询，
// 不按账号逐个查。
func (s *Store) fillDerivedModels(list []Account) error {
	if len(list) == 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT account_id, model FROM account_models
		WHERE access = ? ORDER BY account_id, model`, AccessAllowed)
	if err != nil {
		return err
	}
	defer rows.Close()

	byID := make(map[int64][]string, len(list))
	for rows.Next() {
		var id int64
		var model string
		if err := rows.Scan(&id, &model); err != nil {
			return err
		}
		byID[id] = append(byID[id], model)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		list[i].Models = byID[list[i].ID]
	}
	return nil
}

// EffectiveModels 返回一个账号全部模型行的有效值，按模型名升序：可达性来自 account_models，
// 元数据按「账号覆盖 → 目录 → 厂商默认」合并（见 DESIGN 5.13）。
// 返回的行不按可达性过滤：调用方自己看 Access（`/v1/models` 只列 allowed）。
func (s *Store) EffectiveModels(accountID int64) ([]ModelInfo, error) {
	rows, err := s.AccountModels(accountID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	provider, err := s.accountProvider(accountID)
	if err != nil {
		return nil, err
	}
	catalog := make(map[string]ProviderModel, len(rows))
	if provider != "" {
		list, err := s.ProviderModels(provider)
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			catalog[m.Model] = m
		}
	}
	// 厂商的有效默认协议只算一次：同一家厂商的每个模型都拿它当兜底。
	var providerDefault string
	if provider != "" {
		if providerDefault, err = s.providerDefaultProtocol(provider); err != nil {
			return nil, err
		}
	}

	out := make([]ModelInfo, 0, len(rows))
	for _, row := range rows {
		pm := catalog[row.Model]
		out = append(out, ModelInfo{
			Model:           row.Model,
			Access:          row.Access,
			Protocol:        resolveModelProtocol(providerDefault, pm.Protocols, row.Protocol),
			ContextWindow:   firstOfInt(row.ContextWindow, pm.ContextWindow),
			MaxOutput:       firstOfInt(row.MaxOutput, pm.MaxOutput),
			PriceIn:         firstOfFloat(row.PriceIn, pm.PriceIn),
			PriceOut:        firstOfFloat(row.PriceOut, pm.PriceOut),
			PriceCacheRead:  firstOfFloat(row.PriceCacheRead, pm.PriceCacheRead),
			PriceCacheWrite: firstOfFloat(row.PriceCacheWrite, pm.PriceCacheWrite),
		})
	}
	return out, nil
}

// resolveModelProtocol 是有效协议的另一半：账号覆盖优先，其次是目录里这个模型支持的协议，
// 最后回退到厂商的有效默认协议（见 DESIGN 5.13）。
//
// 目录给出的是一组协议而不是一个，取值顺序固定，不受库里的行序影响：
// 厂商的有效默认协议在这组里就用它（按厂商配置打是首选），否则按固定顺序
// openai-chat → openai-responses → anthropic → gemini 取第一个，再没有就取字典序最小的那个。
func resolveModelProtocol(providerDefault string, modelProtocols []string, accountProtocol string) string {
	if accountProtocol != "" {
		return accountProtocol
	}
	if len(modelProtocols) > 0 {
		if providerDefault != "" && containsString(modelProtocols, providerDefault) {
			return providerDefault
		}
		for _, protocol := range append(append([]string{}, defaultProtocolOrder...), translate.ProtocolGemini) {
			if containsString(modelProtocols, protocol) {
				return protocol
			}
		}
		sorted := append([]string{}, modelProtocols...)
		sort.Strings(sorted)
		return sorted[0]
	}
	return providerDefault
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// firstOfInt 按「覆盖 → 目录」取值：前者非空就用它，否则用后者。两者都空时返回 nil。
func firstOfInt(override, catalog *int64) *int64 {
	if override != nil {
		return override
	}
	return catalog
}

// firstOfFloat 同 firstOfInt，只是值的类型是价格。
func firstOfFloat(override, catalog *float64) *float64 {
	if override != nil {
		return override
	}
	return catalog
}

// SetAccountModelAccess 记录一个模型在这个账号上的可达性，是 access 这一列唯一的写入口。
// 只动 access、http_status、error 与 checked_at，协议与覆盖值原样保留；
// checked_at 由调用方给（探活那一刻），不在这里取当前时间。
func (s *Store) SetAccountModelAccess(accountID int64, model, access string, httpStatus int, msg string, checkedAt int64) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("记录可达性需要模型名：账号 %d 的哪一行都定位不了", accountID)
	}
	if !validAccess(access) {
		return fmt.Errorf("模型 %q 的可达性是 %q，只能是 %s", model, access, strings.Join(accessValues(), "、"))
	}
	_, err := s.db.Exec(`INSERT INTO account_models (account_id, model, access, http_status, error, checked_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, model) DO UPDATE SET
			access = excluded.access, http_status = excluded.http_status,
			error = excluded.error, checked_at = excluded.checked_at`,
		accountID, model, access, httpStatus, msg, checkedAt)
	return err
}

// SetAccountModelProtocol 记录协议发现的结果，是 account_models.protocol 唯一的写入口。
// 模型行还不存在时先落一行 allowed 占位（能测出协议就说明上游认这个模型名），
// 已存在的行只改协议列：可达性与探活时间由 SetAccountModelAccess 写。
// 协议只能是空串（回到继承）或四条线协议之一。
func (s *Store) SetAccountModelProtocol(accountID int64, model, protocol string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("记录协议需要模型名：账号 %d 的哪一行都定位不了", accountID)
	}
	if protocol != "" && !IsWireProtocol(protocol) {
		return fmt.Errorf("模型 %q 的协议是 %q，只能是空串或 %s", model, protocol, wireProtocolNames())
	}
	_, err := s.db.Exec(`INSERT INTO account_models (account_id, model, access, http_status, error, checked_at, protocol)
		VALUES (?, ?, ?, 0, '', 0, ?)
		ON CONFLICT (account_id, model) DO UPDATE SET protocol = excluded.protocol`,
		accountID, model, AccessAllowed, protocol)
	return err
}

// AccountModelOverride 是账号侧的覆盖值：nil 表示这一项继承目录（库里存 NULL）。
// 它是整体覆盖，不是增量：SetAccountModelOverride 把这里给的值一次写完，
// 想保留某一项就把它从目录里读出来再传进来。
type AccountModelOverride struct {
	ContextWindow   *int64
	MaxOutput       *int64
	PriceIn         *float64
	PriceOut        *float64
	PriceCacheRead  *float64
	PriceCacheWrite *float64
}

// SetAccountModelOverride 写账号侧的覆盖值，nil 就是 NULL（继承目录），不是「不动」。
// 模型行还不存在时先落一行 allowed 占位，与 SetAccountModelProtocol 一致：
// 给一个模型写覆盖值本身就说明这个账号要用它。可达性与协议不由这里改。
func (s *Store) SetAccountModelOverride(accountID int64, model string, o AccountModelOverride) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("记录模型覆盖值需要模型名：账号 %d 的哪一行都定位不了", accountID)
	}
	_, err := s.db.Exec(`INSERT INTO account_models
		(account_id, model, access, http_status, error, checked_at,
		 context_window, max_output, price_in, price_out, price_cache_read, price_cache_write)
		VALUES (?, ?, ?, 0, '', 0, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, model) DO UPDATE SET
			context_window = excluded.context_window,
			max_output = excluded.max_output,
			price_in = excluded.price_in,
			price_out = excluded.price_out,
			price_cache_read = excluded.price_cache_read,
			price_cache_write = excluded.price_cache_write`,
		accountID, model, AccessAllowed,
		o.ContextWindow, o.MaxOutput, o.PriceIn, o.PriceOut, o.PriceCacheRead, o.PriceCacheWrite)
	return err
}

// ResolveProtocol 是有效出站协议的纯规则：模型行上有协议就用它，否则用厂商的有效默认协议
// （账号行不再存协议，见迁移 0014 与 DESIGN 5.8）。两部分都是空串时结果就是空串。
func ResolveProtocol(providerProtocol, modelProtocol string) string {
	if modelProtocol != "" {
		return modelProtocol
	}
	return providerProtocol
}

// ModelProtocol 读一个模型行的协议发现结果。没有这一行时返回空字符串（也就是继承），
// 不当作错误。
func (s *Store) ModelProtocol(accountID int64, model string) (string, error) {
	var protocol string
	err := s.db.QueryRow(`SELECT protocol FROM account_models WHERE account_id = ? AND model = ?`,
		accountID, model).Scan(&protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return protocol, nil
}

// EffectiveProtocol 返回账号在某个模型上的有效出站协议，转发与展示共用这一次读。
// 顺序按 DESIGN 5.13：账号覆盖（account_models.protocol）→ 目录里这个模型支持的协议
// （provider_models.protocols，见 resolveModelProtocol）→ 厂商的有效默认协议。
// 行不存在、列为空、厂商行不存在或这家没有路由时都回退到空串，不报错。
//
// Deprecated: 三参数写法 EffectiveProtocol(accountID, accountProtocol, model) 只为尚未改完的调用点
// 保留（internal/gateway、internal/cli），第二个参数是账号协议，现在已经无用且被忽略；
// 两个写法走的是同一条规则，改完调用点后就收回这个口子。
func (s *Store) EffectiveProtocol(accountID int64, model string, legacyAccountProtocol ...string) (string, error) {
	if len(legacyAccountProtocol) > 1 {
		return "", fmt.Errorf("EffectiveProtocol 只收 (accountID, model) 两个参数：账号不再存协议，多余的参数是 %v", legacyAccountProtocol[1:])
	}
	if len(legacyAccountProtocol) == 1 {
		// 旧写法：第二个参数是账号协议（忽略），真正的模型名落在第三个参数上。
		model = legacyAccountProtocol[0]
	}
	protocol, err := s.ModelProtocol(accountID, model)
	if err != nil {
		return "", err
	}
	if protocol != "" {
		return protocol, nil
	}
	provider, err := s.accountProvider(accountID)
	if err != nil {
		return "", err
	}
	var modelProtocols []string
	if pm, ok, err := s.providerModel(provider, model); err != nil {
		return "", err
	} else if ok {
		modelProtocols = pm.Protocols
	}
	fallback, err := s.providerDefaultProtocol(provider)
	if err != nil {
		return "", err
	}
	return resolveModelProtocol(fallback, modelProtocols, protocol), nil
}

// accountProvider 读账号的厂商名。账号不存在时返回空串、不报错：有效协议的契约是
// 「算不出来就是空串」，不因为一行不存在或读不到而中断转发。
func (s *Store) accountProvider(accountID int64) (string, error) {
	var provider string
	err := s.db.QueryRow(`SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return provider, nil
}

// accountProviderStrict 与 accountProvider 不同：账号不存在时报错。写目录行时不能拿空厂商名
// 凑数——那会造出一批谁也读不到的 provider_models 行。
func (s *Store) accountProviderStrict(accountID int64) (string, error) {
	var provider string
	err := s.db.QueryRow(`SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("账号 %d 不存在：读不到它的厂商，模型目录无从落库", accountID)
	}
	if err != nil {
		return "", err
	}
	return provider, nil
}

// ReplaceModels 把这些名字标成 allowed，其余模型行一行都不动，返回新增的行数。
//
// 这是 `model sync` 的写入口（DESIGN 5.13）：上游清单里出现的名字变成这个账号可达的模型，
// 目录里没有的补一行 source='manual'——手填清单与上游清单都只说明「这个名字存在」，
// 不提供长度、价格与能力位，因此只补来源、不改已有的事实。清单里没出现的行保持原状：
// 上游某一轮少给了几个名字不再等于「这些模型消失了」，可达性只由探活结论改动。
//
// 空列表等于「这次没有新名字」，不删任何行（迁移 0015 之前那种「清空」语义随 accounts.models
// 一起没了）。账号不存在时报错，因为读不到厂商就写不了目录。
func (s *Store) ReplaceModels(accountID int64, models []string) (int, error) {
	provider, err := s.accountProviderStrict(accountID)
	if err != nil {
		return 0, err
	}
	names := normalizeModelNames(models)
	added := 0
	err = s.withTx(func(tx *sql.Tx) error {
		for _, name := range names {
			if err := upsertProviderModelTx(tx, ProviderModel{
				Provider: provider,
				Model:    name,
				Source:   SourceManual,
			}); err != nil {
				return err
			}
			// 先按「不覆盖已有行」插一次，只为数清新增了几个名字；
			// 再把这一行标成 allowed。分两步是因为 upsert 的 RowsAffected 数不出新增与更新的区别。
			res, err := tx.Exec(`INSERT INTO account_models (account_id, model, access, http_status, error, checked_at)
				VALUES (?, ?, ?, 0, '', 0)
				ON CONFLICT (account_id, model) DO NOTHING`, accountID, name, AccessAllowed)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				added++
			}
			if _, err := tx.Exec(`UPDATE account_models SET access = ? WHERE account_id = ? AND model = ?`,
				AccessAllowed, accountID, name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// SetModelStatus 记录一个模型的探活结果。只动可达性、HTTP 状态、备注与时间，
// 不动 protocol 与覆盖值：协议发现的结果只由 SetAccountModelProtocol 写。
//
// Deprecated: 新代码用 SetAccountModelAccess。这里收的是迁移 0015 之前的状态词表
// （usable / synced / forbidden / notfound / auth / error），由 accessFromStatus 映射成 access，
// checked_at 取当前时间。
func (s *Store) SetModelStatus(accountID int64, model, status string, httpStatus int, msg string) error {
	return s.SetAccountModelAccess(accountID, model, accessFromStatus(status), httpStatus, msg, time.Now().Unix())
}

// SetModelProtocol 记录协议发现的结果。
//
// Deprecated: 换成了 SetAccountModelProtocol，只差一个名字。
func (s *Store) SetModelProtocol(accountID int64, model, protocol string) error {
	return s.SetAccountModelProtocol(accountID, model, protocol)
}

// SetModels 把这些名字标成 allowed。
//
// Deprecated: 换成了 ReplaceModels。旧语义是「整体改写账号的模型列表」，
// accounts.models 那一列随迁移 0015 一起没了，这里只保留加法（见 ReplaceModels）。
func (s *Store) SetModels(accountID int64, models []string) error {
	_, err := s.ReplaceModels(accountID, models)
	return err
}
