package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"switchboard/internal/translate"
)

var (
	// ErrProviderNotFound 表示厂商表里没有这一行。GetProvider 与 DeleteProvider 用 errors.Is 判断它；
	// LookupProviderRoute 不返回这个错误，那里用 ok=false 更好用。
	ErrProviderNotFound = errors.New("厂商不存在")

	// ErrProviderExists 表示厂商名已经被占用。`provider add` 只用它报错，不做隐式更新：
	// 改一家已有的厂商走 `provider set`。
	ErrProviderExists = errors.New("厂商已存在")

	// ErrProviderNoRoutes 表示厂商行在、但一条路由都没声明，因此算不出默认协议。
	// EffectiveProviderProtocol 返回它，具体文案交给调用方决定。
	ErrProviderNoRoutes = errors.New("厂商没有声明任何路由")
)

// defaultProtocolOrder 是默认协议的回退顺序：openai-chat 先，openai-responses 次之，anthropic 最后。
// 不用协议名字典序：字典序把 anthropic 排在 openai-chat 前面，deepseek 与 commandcode 都同时
// 声明了这两条路由，默认协议会从 chat 悄悄变成 anthropic，升级前后的账号默认协议对不上。
var defaultProtocolOrder = []string{
	translate.ProtocolChat, translate.ProtocolResponses, translate.ProtocolAnthropic,
}

// Provider 是一行厂商。行里只有默认地址、默认协议、模型列表覆盖与备注，
// 协议的路径细节都在 ProviderRoute 上。账号行仍固化自己的 base_url，改这张表只影响之后新加的账号。
// DefaultProtocol 为空字符串表示这家没有显式默认协议，按固定顺序取第一条路由；
// 有效默认协议的算法只有 ResolveDefaultProtocol 一处。
// ModelsPath 是厂商级的模型列表路径覆盖，空字符串表示不覆盖、按路由的 models_path 取（见 5.7）。
type Provider struct {
	Name            string
	BaseURL         string
	Note            string
	DefaultProtocol string
	ModelsPath      string
}

// ProviderRoute 是一行 (厂商, 协议) 的路由。完整地址是厂商的 base_url 去掉末尾斜杠后直接拼 Path，
// 不再做任何后缀猜测。
// ModelsPath 是这条路由的模型列表路径，空字符串表示这条协议没有自己的列表接口，
// 读取时回退到同厂商协议名排序的第一条非空值。写入侧见 prepareRoutes 与 upsertRoutes（DESIGN 5.7）。
type ProviderRoute struct {
	Provider   string
	Protocol   string
	Path       string
	ModelsPath string

	// modelsAuto 为真表示这次的 ModelsPath 是 prepareRoutes 按路径推出来的，不是调用方给的。
	// 推出来的值只跟路径走：库里原本是空或同样是推导值时换成新的推导值，手工填的值不动。
	modelsAuto bool
}

// ProviderPatch 是 `provider set` 的一次改动：字段为 nil 表示这一项不动，与「传了空串」区分开。
// Routes 按协议列出路由改动：非空值覆盖那条路由的路径，空串删掉这条路由。
type ProviderPatch struct {
	BaseURL         *string
	Note            *string
	ModelsPath      *string
	DefaultProtocol *string
	Routes          map[string]string
}

const providerColumns = `name, base_url, note, default_protocol, models_path`

// ListProviders 按名称升序返回全部厂商。表为空时返回空切片，不算错误。
func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT ` + providerColumns + ` FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Provider{}
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Name, &p.BaseURL, &p.Note, &p.DefaultProtocol, &p.ModelsPath); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider 按名称取厂商。
func (s *Store) GetProvider(name string) (Provider, error) {
	var p Provider
	err := s.db.QueryRow(`SELECT `+providerColumns+` FROM providers WHERE name = ?`, name).
		Scan(&p.Name, &p.BaseURL, &p.Note, &p.DefaultProtocol, &p.ModelsPath)
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, fmt.Errorf("%w：%q，用 switchboard provider ls 看已配置的厂商", ErrProviderNotFound, name)
	}
	if err != nil {
		return Provider{}, err
	}
	return p, nil
}

// ProviderRoutes 返回一个厂商的全部路由，按协议名升序（顺序固定，回退取第一条时结果稳定）。
// 厂商不存在时返回空切片：这是「没配过路由」，不是错误。
func (s *Store) ProviderRoutes(provider string) ([]ProviderRoute, error) {
	rows, err := s.db.Query(`SELECT provider, protocol, path, models_path
		FROM provider_routes WHERE provider = ? ORDER BY protocol`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProviderRoute{}
	for rows.Next() {
		var r ProviderRoute
		if err := rows.Scan(&r.Provider, &r.Protocol, &r.Path, &r.ModelsPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LookupProviderRoute 按 (厂商, 协议) 取一条路由，是读路由的唯一入口：转发、模型同步与探活都走它。
// 厂商不存在或这家没有该协议的路由时 ok=false，且不返回错误，调用方据此退回后缀猜测。
func (s *Store) LookupProviderRoute(provider, protocol string) (ProviderRoute, bool, error) {
	if provider == "" || protocol == "" {
		return ProviderRoute{}, false, nil
	}
	var r ProviderRoute
	err := s.db.QueryRow(`SELECT provider, protocol, path, models_path
		FROM provider_routes WHERE provider = ? AND protocol = ?`, provider, protocol).
		Scan(&r.Provider, &r.Protocol, &r.Path, &r.ModelsPath)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRoute{}, false, nil
	}
	if err != nil {
		return ProviderRoute{}, false, err
	}
	return r, true, nil
}

// ProviderProtocols 返回该厂商声明过路由的协议，按协议名升序。
// 没有厂商行时返回 nil，调用方据此退回「三种协议全试」。
func (s *Store) ProviderProtocols(provider string) ([]string, error) {
	routes, err := s.ProviderRoutes(provider)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routes {
		if r.Protocol != "" {
			out = append(out, r.Protocol)
		}
	}
	return out, nil
}

// wireProtocol 判断是不是三种线协议之一。gemini 还没实现，不在这个名单里：留一条永远用不上的
// 路由只会误导（见 DESIGN 的已知未做）。
func wireProtocol(protocol string) bool {
	switch protocol {
	case translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses:
		return true
	}
	return false
}

// normalizeBaseURL 校验并规范化厂商地址：必须是 http:// 或 https:// 开头，去掉末尾斜杠。
// 三个写入口（CreateProvider、SetProvider、AddProvider）都在这里过一道，cli 那层不再是唯一防线：
// 脏地址一旦落库，转发与模型同步都会拿它拼路径。
func normalizeBaseURL(raw string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return "", errors.New("缺少 base_url")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("base_url %q 要以 http:// 或 https:// 开头，例如 https://api.example.com", raw)
	}
	return u, nil
}

// normalizeModelsPath 校验厂商级的模型列表路径覆盖。空串表示不覆盖，或者是「回到按路由取」。
func normalizeModelsPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path != "" && !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("模型列表路径 %q 要以 / 开头，完整地址是 base_url 拼上它，例如 /v1/models", raw)
	}
	return path, nil
}

// DeriveModelsPath 是 models_path 的写入默认值：openai-chat 的路由把尾部 /chat/completions
// 换成 /models（/v1/chat/completions → /v1/models、/chat/completions → /models），其它协议返回空串。
// 空串表示「这条路由没有自己的列表接口」，读取时回退到同厂商其它协议的非空 models_path。
// 推导只发生在写入侧，读取侧不拿这一条重算：内置 anthropic 厂商只有 anthropic 一条路由，
// 没有 openai 路由可推导，读时推导会让 anthropic 型账号彻底失去模型列表来源。
func DeriveModelsPath(protocol, path string) string {
	if protocol != translate.ProtocolChat {
		return ""
	}
	if tail, ok := strings.CutSuffix(path, "/chat/completions"); ok {
		return tail + "/models"
	}
	return ""
}

// prepareRoutes 校验一次写入要落的路由，并补上 models_path 的默认值。
func prepareRoutes(provider string, in []ProviderRoute) ([]ProviderRoute, error) {
	out := make([]ProviderRoute, 0, len(in))
	for _, r := range in {
		protocol := strings.TrimSpace(r.Protocol)
		if !wireProtocol(protocol) {
			return nil, fmt.Errorf("厂商 %s 的路由协议 %q 不认识，只能是 %s、%s、%s",
				provider, r.Protocol, translate.ProtocolAnthropic, translate.ProtocolChat, translate.ProtocolResponses)
		}
		path := strings.TrimSpace(r.Path)
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("厂商 %s 的 %s 路由路径 %q 要以 / 开头，例如 /v1/chat/completions；完整地址是 base_url 拼上它",
				provider, protocol, r.Path)
		}
		modelsPath, err := normalizeModelsPath(r.ModelsPath)
		if err != nil {
			return nil, fmt.Errorf("厂商 %s 的 %s 路由：%w", provider, protocol, err)
		}
		auto := modelsPath == ""
		if auto {
			modelsPath = DeriveModelsPath(protocol, path)
		}
		out = append(out, ProviderRoute{
			Provider: provider, Protocol: protocol, Path: path,
			ModelsPath: modelsPath, modelsAuto: auto,
		})
	}
	return out, nil
}

// upsertRoutes 按 (厂商, 协议) 覆盖或新增路由。
// models_path 是本次推出来的推导值时，只有库里那行原本也是空的或同样是推导值才替换：
// 改一条路由的路径会顺带把推导出来的列表路径换成新路径推出来的那个，而手工填的值不动
// （内置 anthropic 厂商那条 /v1/models 因此不会因为重写 anthropic 路由而消失）。
// 要改厂商级的列表地址就用 ProviderPatch.ModelsPath 覆盖。
func upsertRoutes(tx *sql.Tx, routes []ProviderRoute) error {
	for _, r := range routes {
		modelsPath := r.ModelsPath
		if r.modelsAuto {
			var oldPath, oldModels string
			err := tx.QueryRow(`SELECT path, models_path FROM provider_routes WHERE provider = ? AND protocol = ?`,
				r.Provider, r.Protocol).Scan(&oldPath, &oldModels)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// 新行：就用这次推出来的值。
			case err != nil:
				return err
			case oldModels != "" && oldModels != DeriveModelsPath(r.Protocol, oldPath):
				// 库里是手工填的值：留着，不跟着路径改。
				modelsPath = oldModels
			}
		}
		if _, err := tx.Exec(`INSERT INTO provider_routes (provider, protocol, path, models_path)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(provider, protocol) DO UPDATE SET
				path = excluded.path, models_path = excluded.models_path`,
			r.Provider, r.Protocol, r.Path, modelsPath); err != nil {
			return err
		}
	}
	return nil
}

// hasRoute 判断协议是否落在这些路由里。
func hasRoute(routes []ProviderRoute, protocol string) bool {
	for _, r := range routes {
		if r.Protocol == protocol {
			return true
		}
	}
	return false
}

// routeProtocols 列出这些路由的协议，按协议名升序，用于错误文案。
func routeProtocols(routes []ProviderRoute) string {
	protocols := make([]string, 0, len(routes))
	for _, r := range routes {
		protocols = append(protocols, r.Protocol)
	}
	sort.Strings(protocols)
	return strings.Join(protocols, "、")
}

// CreateProvider 新建一家厂商，是 `provider add` 的唯一写入口。
// 名称已存在时报 ErrProviderExists（改已有的厂商用 SetProvider），至少要声明一条协议路由，
// 默认协议非空时必须落在这次声明的路由上，否则整笔拒绝，不留下半条记录。
// 写库时 models_path 为空的路由按 DeriveModelsPath 补默认值。
func (s *Store) CreateProvider(p Provider, routes []ProviderRoute) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return errors.New("厂商名不能为空")
	}
	base, err := normalizeBaseURL(p.BaseURL)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", name, err)
	}
	prepared, err := prepareRoutes(name, routes)
	if err != nil {
		return err
	}
	if len(prepared) == 0 {
		return fmt.Errorf("厂商 %s 至少要声明一条协议路由（%s、%s、%s 里任一条），否则账号只能按 base_url 后缀猜路径",
			name, translate.ProtocolChat, translate.ProtocolResponses, translate.ProtocolAnthropic)
	}
	modelsPath, err := normalizeModelsPath(p.ModelsPath)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", name, err)
	}
	def := strings.TrimSpace(p.DefaultProtocol)
	if def != "" && !hasRoute(prepared, def) {
		return fmt.Errorf("厂商 %s 的默认协议是 %q，但这次没有声明这个协议的路由（这次声明：%s）；把默认协议改成已声明的其中一个，或补上这条路由",
			name, def, routeProtocols(prepared))
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRow(`SELECT 1 FROM providers WHERE name = ?`, name).Scan(&one)
	switch {
	case err == nil:
		return fmt.Errorf("%w：%q，改这家厂商用 switchboard provider set %s", ErrProviderExists, name, name)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	if _, err := tx.Exec(`INSERT INTO providers (name, base_url, note, default_protocol, models_path)
		VALUES (?, ?, ?, ?, ?)`, name, base, strings.TrimSpace(p.Note), def, modelsPath); err != nil {
		return err
	}
	if err := upsertRoutes(tx, prepared); err != nil {
		return err
	}
	return tx.Commit()
}

// SetProvider 增量修改一家厂商，是 `provider set` 的唯一写入口：patch 里为 nil 的字段一律不动。
// 协议路由按 patch.Routes 覆盖或删掉（空串即删），删到一条不剩时报错，整体删厂商走 DeleteProvider。
// 这次动了路由或默认协议时，改完之后默认协议必须指向一条仍然存在的路由，否则整笔拒绝：
// 留着一条指不到路由的默认协议，账号加进来之后默认协议要么报错要么落到后缀猜测上，
// 都不如当场说清楚。只改备注或地址的调用不受历史悬空值影响，那类脏数据由读取侧按固定顺序回退。
func (s *Store) SetProvider(name string, patch ProviderPatch) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("厂商名不能为空")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var p Provider
	err = tx.QueryRow(`SELECT `+providerColumns+` FROM providers WHERE name = ?`, name).
		Scan(&p.Name, &p.BaseURL, &p.Note, &p.DefaultProtocol, &p.ModelsPath)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：%q，新建厂商用 switchboard provider add，用 switchboard provider ls 看已配置的厂商", ErrProviderNotFound, name)
	}
	if err != nil {
		return err
	}

	if patch.BaseURL != nil {
		base, err := normalizeBaseURL(*patch.BaseURL)
		if err != nil {
			return fmt.Errorf("厂商 %s：%w", name, err)
		}
		p.BaseURL = base
	}
	if patch.Note != nil {
		p.Note = strings.TrimSpace(*patch.Note)
	}
	if patch.ModelsPath != nil {
		modelsPath, err := normalizeModelsPath(*patch.ModelsPath)
		if err != nil {
			return fmt.Errorf("厂商 %s：%w", name, err)
		}
		p.ModelsPath = modelsPath
	}
	if patch.DefaultProtocol != nil {
		def := strings.TrimSpace(*patch.DefaultProtocol)
		if def != "" && !wireProtocol(def) {
			return fmt.Errorf("厂商 %s 的默认协议 %q 不认识，只能是 %s、%s、%s；传空串表示回到按固定顺序取第一条已声明的路由",
				name, def, translate.ProtocolChat, translate.ProtocolResponses, translate.ProtocolAnthropic)
		}
		p.DefaultProtocol = def
	}

	// 改之前的形状：用来区分「这次把路由删光了」与「本来就没有路由」。
	before, err := providerRoutesTx(tx, name)
	if err != nil {
		return err
	}

	// 路由改动：空串删掉这条，其余覆盖或新增。协议名排序后处理，报错顺序因此固定。
	protocols := make([]string, 0, len(patch.Routes))
	for protocol := range patch.Routes {
		protocols = append(protocols, protocol)
	}
	sort.Strings(protocols)
	for _, protocol := range protocols {
		path := strings.TrimSpace(patch.Routes[protocol])
		if path == "" {
			if _, err := tx.Exec(`DELETE FROM provider_routes WHERE provider = ? AND protocol = ?`,
				name, protocol); err != nil {
				return err
			}
			continue
		}
		prepared, err := prepareRoutes(name, []ProviderRoute{{Protocol: protocol, Path: path}})
		if err != nil {
			return err
		}
		if err := upsertRoutes(tx, prepared); err != nil {
			return err
		}
	}

	routes, err := providerRoutesTx(tx, name)
	if err != nil {
		return err
	}
	// 删到一条不剩要拒绝；本来就没有路由的厂商（过渡期 AddProvider 能造出来）不因此挡住改备注这类操作。
	if len(routes) == 0 && len(before) > 0 {
		return fmt.Errorf("厂商 %s 至少要保留一条协议路由；不想再要这家厂商就用 switchboard provider rm %s", name, name)
	}
	// 只有这次动了路由或默认协议时才校验指向：历史库里遗留的悬空默认协议由读取侧按固定顺序回退，
	// 不该挡住一次只改备注或地址的调用。
	touchedRouting := len(patch.Routes) > 0 || patch.DefaultProtocol != nil
	if touchedRouting && p.DefaultProtocol != "" && !hasRoute(routes, p.DefaultProtocol) {
		return fmt.Errorf("厂商 %s 的默认协议 %q 对应的路由不存在（现有：%s）；先加上这条路由（--%s <路径>），或把默认协议改成已声明的其中一个（--default-protocol），也可以传空串回到按固定顺序取第一条",
			name, p.DefaultProtocol, routeProtocols(routes), protocolFlagName(p.DefaultProtocol))
	}

	if _, err := tx.Exec(`UPDATE providers SET base_url = ?, note = ?, default_protocol = ?, models_path = ?
		WHERE name = ?`, p.BaseURL, p.Note, p.DefaultProtocol, p.ModelsPath, name); err != nil {
		return err
	}
	return tx.Commit()
}

// protocolFlagName 把协议名换成 `provider set` 上对应的那个 flag，错误文案直接给出下一步能敲的命令。
func protocolFlagName(protocol string) string {
	switch protocol {
	case translate.ProtocolChat:
		return "openai"
	case translate.ProtocolResponses:
		return "responses"
	case translate.ProtocolAnthropic:
		return "anthropic"
	}
	return protocol
}

// providerRoutesTx 是事务里读路由的版本，供 SetProvider 校验改完之后的形状。
func providerRoutesTx(tx *sql.Tx, provider string) ([]ProviderRoute, error) {
	rows, err := tx.Query(`SELECT provider, protocol, path, models_path
		FROM provider_routes WHERE provider = ? ORDER BY protocol`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProviderRoute{}
	for rows.Next() {
		var r ProviderRoute
		if err := rows.Scan(&r.Provider, &r.Protocol, &r.Path, &r.ModelsPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddProvider 是过渡期的 upsert 写入口：厂商已存在时更新 base_url 与备注，路由按 (厂商, 协议)
// 覆盖或新增，没传入的路由不动。它比 CreateProvider 宽松——允许一条路由都没有——因为还没改造的
// `provider add` 就是这么用的。新建走 CreateProvider、改走 SetProvider，这条在 cli 换完之后删掉。
// p.DefaultProtocol 非空时必须落在「这次传入的路由」或「此前已声明的路由」上，否则整笔拒绝，
// 不留下半条记录；传空字符串表示不改这一列（已有行保留原值）。
func (s *Store) AddProvider(p Provider, routes []ProviderRoute) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return errors.New("厂商名不能为空")
	}
	base, err := normalizeBaseURL(p.BaseURL)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", name, err)
	}
	prepared, err := prepareRoutes(name, routes)
	if err != nil {
		return err
	}
	modelsPath, err := normalizeModelsPath(p.ModelsPath)
	if err != nil {
		return fmt.Errorf("厂商 %s：%w", name, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// models_path 传空串时不覆盖已存的值：这条兼容入口的调用方还不知道有这个字段。
	if _, err := tx.Exec(`INSERT INTO providers (name, base_url, note, models_path) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			base_url = excluded.base_url, note = excluded.note,
			models_path = CASE WHEN excluded.models_path = '' THEN models_path ELSE excluded.models_path END`,
		name, base, p.Note, modelsPath); err != nil {
		return err
	}

	if err := upsertRoutes(tx, prepared); err != nil {
		return err
	}

	// 校验放在路由写完之后：「这次传入的路由」也算数，所以先落路由再查有没有这条。
	def := strings.TrimSpace(p.DefaultProtocol)
	if def != "" {
		if err := requireDeclaredProtocol(tx, name, def); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE providers SET default_protocol = ? WHERE name = ?`, def, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// requireDeclaredProtocol 确认默认协议能指向这家厂商的一条已声明路由。
// 指不到时返回可行动的错误：要么先把这条路由加上，要么把默认协议改成已声明的其中一个。
func requireDeclaredProtocol(tx *sql.Tx, provider, protocol string) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM provider_routes WHERE provider = ? AND protocol = ?`,
		provider, protocol).Scan(&one)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	declared, err := declaredProtocols(tx, provider)
	if err != nil {
		return err
	}
	if len(declared) == 0 {
		return fmt.Errorf("厂商 %s 的默认协议是 %q，但这家还没有任何路由；先运行 switchboard provider add %s --base-url <地址> --route %s=<路径> 把这条路由加上，或去掉默认协议改回按固定顺序取第一条",
			provider, protocol, provider, protocol)
	}
	return fmt.Errorf("厂商 %s 的默认协议是 %q，但这家没有这个协议的路由（已声明：%s）；先运行 switchboard provider add %s --base-url <地址> --route %s=<路径> 补上这条路由，或把默认协议改成已声明的其中一个：%s",
		provider, protocol, strings.Join(declared, "、"), provider, protocol, strings.Join(declared, "、"))
}

// declaredProtocols 是事务里读这家厂商已声明协议的版本，按协议名升序，与 ProviderRoutes 一致。
func declaredProtocols(tx *sql.Tx, provider string) ([]string, error) {
	rows, err := tx.Query(`SELECT protocol FROM provider_routes WHERE provider = ? ORDER BY protocol`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var proto string
		if err := rows.Scan(&proto); err != nil {
			return nil, err
		}
		out = append(out, proto)
	}
	return out, rows.Err()
}

// ResolveDefaultProtocol 是有效默认协议的纯规则：
//  1. 显式值落在这家已声明的路由上就用它；
//  2. 否则按固定顺序 openai-chat → openai-responses → anthropic 取第一条已声明的路由；
//  3. 一条路由都没有时返回空串。
//
// 不走协议名字典序兜底：字典序把 anthropic 排在 openai-chat 前面，同一家上游声明了两条路由时
// 默认协议会从 chat 变成 anthropic。显式值指不到路由时同样按固定顺序回退，历史库里留下的
// 悬空默认协议（例如旧版本删路由时没清默认协议）因此不会静默落到后缀猜测上。
func ResolveDefaultProtocol(explicit string, routes []ProviderRoute) string {
	if explicit != "" && hasRoute(routes, explicit) {
		return explicit
	}
	for _, protocol := range defaultProtocolOrder {
		if hasRoute(routes, protocol) {
			return protocol
		}
	}
	return ""
}

// ResolveModelsPath 是模型列表来源的纯规则，返回列表路径与这条路径所属的协议：
//
//  1. 厂商级覆盖非空就用它，协议取账号协议（覆盖是厂商级设置，没有自己的协议）；
//  2. 账号协议那条路由的 models_path 非空就用它；
//  3. 否则取按协议名排序的第一条非空 models_path——deepseek 的 anthropic 账号因此仍能拿
//     openai-chat 那份列表；
//  4. 都没有时返回空串与空串，调用方按 base_url 兜底或报错。
//
// 协议必须与路径一起返回：凭据头按协议选（anthropic 用 x-api-key，另两家用 Bearer），
// 地址来自哪条路由就用哪条路由的协议，否则 deepseek 的 anthropic 账号会打 chat 的
// /v1/models 却发 x-api-key，第一个真实账号就会在 /models 上收到 401/403。
func ResolveModelsPath(override string, routes []ProviderRoute, accountProtocol string) (path, protocol string) {
	if override != "" {
		return override, accountProtocol
	}
	for _, r := range routes {
		if r.Protocol == accountProtocol && strings.TrimSpace(r.ModelsPath) != "" {
			return r.ModelsPath, r.Protocol
		}
	}
	for _, r := range routes {
		if strings.TrimSpace(r.ModelsPath) != "" {
			return r.ModelsPath, r.Protocol
		}
	}
	return "", ""
}

// EffectiveProviderProtocol 返回厂商的有效默认协议，是读默认协议的唯一入口：
// `account add` 在没给 --protocol 时用它补，`provider ls` 展示也用它。
// 厂商不存在时返回 ErrProviderNotFound；厂商在但一条路由都没有时返回空串与 ErrProviderNoRoutes，
// 需要自己的提示文案的调用方用 errors.Is 判这个哨兵，自己写。
func (s *Store) EffectiveProviderProtocol(name string) (string, error) {
	p, err := s.GetProvider(name)
	if err != nil {
		return "", err
	}
	routes, err := s.ProviderRoutes(name)
	if err != nil {
		return "", err
	}
	proto := ResolveDefaultProtocol(p.DefaultProtocol, routes)
	if proto == "" {
		return "", fmt.Errorf("%w：%q 还没有任何路由，默认协议无从算起", ErrProviderNoRoutes, name)
	}
	return proto, nil
}

// DeleteProvider 在一个事务里删掉厂商行与它的全部路由行。两张表之间没有外键，
// 分两条语句删就必须同一个事务，否则中途失败会留下没有厂商的孤儿路由。
// 仍有账号使用这个厂商时这里不拦：那是调用方的事（`provider rm` 要求 --force），
// 账号表只存 provider 字符串，删厂商不影响已有账号的 base_url。
func (s *Store) DeleteProvider(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`DELETE FROM providers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w：%q", ErrProviderNotFound, name)
	}

	if _, err := tx.Exec(`DELETE FROM provider_routes WHERE provider = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAccountsByProvider 返回 provider 字段等于 name 的账号，按 priority、id 升序。
// `provider rm` 用它列出还在用的账号。
func (s *Store) ListAccountsByProvider(name string) ([]Account, error) {
	rows, err := s.db.Query(`SELECT id, name, provider, protocol, base_url, key_cipher, models,
		price_in, price_out, priority, enabled, COALESCE(cooldown_until, 0), COALESCE(last_error, ''), fail_streak, created_at
		FROM accounts WHERE provider = ? ORDER BY priority ASC, id ASC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}
