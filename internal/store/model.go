package store

// 这个文件是数据库形状的唯一来源：十二张表的 model 定义、种子数据与旧库搬迁要用的表清单。
//
// model 与业务结构体分开写，因为两者形状不同：
//
//   - ProviderModel.Protocols 与 Capabilities 在库里是逗号分隔的文本，业务侧是 []string；
//   - ProviderHeader 的业务结构体不带 Provider（它由查询条件决定），而库里 provider 是主键之一；
//   - Account.Models 与 AccountModel.Status 是运行期派生字段，不存在于任何列。
//
// 于是这里只有行形状（*Row 后缀），业务侧仍用 catalog.go、provider.go、model_alias.go 里
// 原来的结构体，转换在各自的读写入口里做。
//
// 建表与加列全部走 AutoMigrate：没有迁移文件，也没有版本号。AutoMigrate 是幂等的 schema
// diff，表不存在就建、缺列就 ALTER TABLE ADD COLUMN，不改列类型、不删列、不删索引。

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// accountRow 是 accounts 表的一行。
//
// Provider 只存名字：转发用的地址、默认协议与路由都按它在 providers / provider_routes 里查。
type accountRow struct {
	ID            int64   `gorm:"column:id;primaryKey;autoIncrement"`
	Name          string  `gorm:"column:name;size:191;not null;uniqueIndex"`
	Provider      string  `gorm:"column:provider;not null"`
	KeyCipher     []byte  `gorm:"column:key_cipher;not null"`
	Priority      int     `gorm:"column:priority;not null;default:100"`
	Enabled       bool    `gorm:"column:enabled;not null;default:true"`
	CooldownUntil *int64  `gorm:"column:cooldown_until"`
	LastError     *string `gorm:"column:last_error"`
	FailStreak    int     `gorm:"column:fail_streak;not null;default:0"`
	CreatedAt     int64   `gorm:"column:created_at;not null"`
}

func (accountRow) TableName() string { return "accounts" }

// requestRow 是 requests 表的一行，一次上游尝试一条。
type requestRow struct {
	ID                   int64   `gorm:"column:id;primaryKey;autoIncrement"`
	TS                   int64   `gorm:"column:ts;not null;index:requests_ts,sort:desc"`
	Inbound              string  `gorm:"column:inbound;not null"`
	Outbound             string  `gorm:"column:outbound;not null"`
	ModelRequested       string  `gorm:"column:model_requested;not null"`
	ModelUsed            string  `gorm:"column:model_used;not null"`
	AccountID            *int64  `gorm:"column:account_id"`
	HTTPStatus           int     `gorm:"column:http_status;not null"`
	OK                   bool    `gorm:"column:ok;not null"`
	InTokens             int64   `gorm:"column:in_tokens;not null;default:0"`
	InTokensUncached     int64   `gorm:"column:in_tokens_uncached;not null;default:0"`
	OutTokens            int64   `gorm:"column:out_tokens;not null;default:0"`
	CacheRead            int64   `gorm:"column:cache_read;not null;default:0"`
	CacheWrite           int64   `gorm:"column:cache_write;not null;default:0"`
	ReasoningTokens      int64   `gorm:"column:reasoning_tokens;not null;default:0"`
	ReasoningTokensExtra int64   `gorm:"column:reasoning_tokens_extra;not null;default:0"`
	TTSChars             int64   `gorm:"column:tts_chars;not null;default:0"`
	AudioSeconds         int64   `gorm:"column:audio_seconds;not null;default:0"`
	LatencyMS            int64   `gorm:"column:latency_ms;not null;default:0"`
	Error                *string `gorm:"column:error"`
}

func (requestRow) TableName() string { return "requests" }

// providerRow 是 providers 表的一行。行里没有凭据，只有地址与路径。
//
// PluginID 记这一行是哪次插件安装建起来的，空串表示用户自己配的厂商。它只服务插件生命周期的
// 连带清理（见 DeletePlugin），Provider 业务结构体上没有对应字段：读写厂商的既有入口不看它。
type providerRow struct {
	Name            string `gorm:"column:name;size:191;primaryKey"`
	BaseURL         string `gorm:"column:base_url;not null"`
	Note            string `gorm:"column:note;not null;default:''"`
	DefaultProtocol string `gorm:"column:default_protocol;not null;default:''"`
	ModelsPath      string `gorm:"column:models_path;not null;default:''"`
	PluginID        string `gorm:"column:plugin_id;not null;default:''"`
}

func (providerRow) TableName() string { return "providers" }

// providerRouteRow 是一行厂商路由：某个线协议落在哪个地址上。
type providerRouteRow struct {
	Provider   string `gorm:"column:provider;size:191;primaryKey"`
	Protocol   string `gorm:"column:protocol;size:191;primaryKey"`
	Path       string `gorm:"column:path;not null"`
	ModelsPath string `gorm:"column:models_path;not null;default:''"`
}

func (providerRouteRow) TableName() string { return "provider_routes" }

// providerHeaderRow 是一行厂商附加请求头声明。
//
// ProviderRow 只用来让 AutoMigrate 建出 ON DELETE CASCADE：GORM 的外键必须挂在关系字段上，
// 没有它就不会生成约束。这个字段不参与读写，除非显式 Preload。
type providerHeaderRow struct {
	Provider string `gorm:"column:provider;size:191;primaryKey"`
	Protocol string `gorm:"column:protocol;size:191;primaryKey"`
	Name     string `gorm:"column:name;size:191;primaryKey"`
	Value    string `gorm:"column:value;not null"`

	ProviderRow *providerRow `gorm:"foreignKey:Provider;references:Name;constraint:OnDelete:CASCADE"`
}

func (providerHeaderRow) TableName() string { return "provider_headers" }

// providerModelRow 是模型目录的一行，按厂商共享。
type providerModelRow struct {
	Provider        string   `gorm:"column:provider;size:191;primaryKey"`
	Model           string   `gorm:"column:model;size:191;primaryKey"`
	Source          string   `gorm:"column:source;not null"`
	ContextWindow   *int64   `gorm:"column:context_window"`
	MaxOutput       *int64   `gorm:"column:max_output"`
	Protocols       string   `gorm:"column:protocols;not null;default:''"`
	Capabilities    string   `gorm:"column:capabilities;not null;default:''"`
	Voices          string   `gorm:"column:voices;not null;default:''"`
	AudioFormats    string   `gorm:"column:audio_formats;not null;default:''"`
	PriceIn         *float64 `gorm:"column:price_in"`
	PriceOut        *float64 `gorm:"column:price_out"`
	PriceCacheRead  *float64 `gorm:"column:price_cache_read"`
	PriceCacheWrite *float64 `gorm:"column:price_cache_write"`
	PriceTTSChar    *float64 `gorm:"column:price_tts_char"`
	PriceAudioSec   *float64 `gorm:"column:price_audio_second"`
	UpdatedAt       int64    `gorm:"column:updated_at;not null;default:0"`
}

func (providerModelRow) TableName() string { return "provider_models" }

// accountModelRow 是一个账号在一个模型上的可达性与覆盖值。
type accountModelRow struct {
	AccountID       int64    `gorm:"column:account_id;primaryKey"`
	Model           string   `gorm:"column:model;size:191;primaryKey"`
	Access          string   `gorm:"column:access;not null"`
	HTTPStatus      int      `gorm:"column:http_status;not null;default:0"`
	Error           string   `gorm:"column:error;not null;default:''"`
	CheckedAt       int64    `gorm:"column:checked_at;not null;default:0"`
	Protocol        string   `gorm:"column:protocol;not null;default:''"`
	ContextWindow   *int64   `gorm:"column:context_window"`
	MaxOutput       *int64   `gorm:"column:max_output"`
	PriceIn         *float64 `gorm:"column:price_in"`
	PriceOut        *float64 `gorm:"column:price_out"`
	PriceCacheRead  *float64 `gorm:"column:price_cache_read"`
	PriceCacheWrite *float64 `gorm:"column:price_cache_write"`
	PriceTTSChar    *float64 `gorm:"column:price_tts_char"`
	PriceAudioSec   *float64 `gorm:"column:price_audio_second"`
}

func (accountModelRow) TableName() string { return "account_models" }

// modelAliasRow 是一行别名映射。
type modelAliasRow struct {
	Alias     string `gorm:"column:alias;size:191;primaryKey"`
	Target    string `gorm:"column:target;not null"`
	CreatedAt int64  `gorm:"column:created_at;not null"`
}

func (modelAliasRow) TableName() string { return "model_aliases" }

// pluginRow 是 plugins 表的一行：插件的当前状态与活动版本。
type pluginRow struct {
	ID            string `gorm:"column:id;size:191;primaryKey"`
	ActiveVersion string `gorm:"column:active_version;not null;default:''"`
	Kind          string `gorm:"column:kind;not null;default:''"`
	Trust         string `gorm:"column:trust;not null;default:''"`
	Enabled       bool   `gorm:"column:enabled;not null;default:true"`
	State         string `gorm:"column:state;not null;default:''"`
	Source        string `gorm:"column:source;not null;default:''"`
	LastError     string `gorm:"column:last_error;not null;default:''"`
	InstalledAt   int64  `gorm:"column:installed_at;not null;default:0"`
	UpdatedAt     int64  `gorm:"column:updated_at;not null;default:0"`
}

func (pluginRow) TableName() string { return "plugins" }

// pluginVersionRow 是 plugin_versions 表的一行：一个已安装版本的不可变元数据。
type pluginVersionRow struct {
	PluginID        string `gorm:"column:plugin_id;size:191;primaryKey"`
	Version         string `gorm:"column:version;size:191;primaryKey"`
	ABI             string `gorm:"column:abi;not null;default:''"`
	Runtime         string `gorm:"column:runtime;not null;default:''"`
	ManifestJSON    string `gorm:"column:manifest_json;not null;default:''"`
	PermissionsJSON string `gorm:"column:permissions_json;not null;default:''"`
	PackageSHA256   string `gorm:"column:package_sha256;not null;default:''"`
	ManifestSHA256  string `gorm:"column:manifest_sha256;not null;default:''"`
	Source          string `gorm:"column:source;not null;default:''"`
	SignatureStatus string `gorm:"column:signature_status;not null;default:''"`
	InstalledAt     int64  `gorm:"column:installed_at;not null;default:0"`
}

func (pluginVersionRow) TableName() string { return "plugin_versions" }

// pluginOptionRow 是一行插件配置项。key 在 MySQL 里是保留字，引用它要显式写列名。
type pluginOptionRow struct {
	PluginID  string `gorm:"column:plugin_id;size:191;primaryKey"`
	Key       string `gorm:"column:key;size:191;primaryKey"`
	ValueJSON string `gorm:"column:value_json;not null;default:''"`
	UpdatedAt int64  `gorm:"column:updated_at;not null;default:0"`
}

func (pluginOptionRow) TableName() string { return "plugin_options" }

// modelOperationRow 是一行模型与操作的绑定：用哪条方言、声明了哪些能力与上限。
type modelOperationRow struct {
	Provider         string `gorm:"column:provider;size:191;primaryKey"`
	Model            string `gorm:"column:model;size:191;primaryKey"`
	Operation        string `gorm:"column:operation;size:191;primaryKey"`
	DialectID        string `gorm:"column:dialect_id;not null;default:''"`
	CapabilitiesJSON string `gorm:"column:capabilities_json;not null;default:''"`
	LimitsJSON       string `gorm:"column:limits_json;not null;default:''"`
	Source           string `gorm:"column:source;not null;default:''"`
	UpdatedAt        int64  `gorm:"column:updated_at;not null;default:0"`
}

func (modelOperationRow) TableName() string { return "model_operations" }

// pluginAuditRow 是一行插件审计。只增不改：安装、升级、回滚、启停、卸载、权限变化都记一条。
type pluginAuditRow struct {
	ID                    int64  `gorm:"column:id;primaryKey;autoIncrement"`
	PluginID              string `gorm:"column:plugin_id;size:191;not null;index:plugin_audit_plugin,priority:1"`
	Version               string `gorm:"column:version;not null;default:''"`
	Action                string `gorm:"column:action;not null"`
	Actor                 string `gorm:"column:actor;not null;default:''"`
	Result                string `gorm:"column:result;not null;default:''"`
	Error                 string `gorm:"column:error;not null;default:''"`
	PermissionsBeforeJSON string `gorm:"column:permissions_before_json;not null;default:''"`
	PermissionsAfterJSON  string `gorm:"column:permissions_after_json;not null;default:''"`
	At                    int64  `gorm:"column:at;not null;index:plugin_audit_plugin,priority:2,sort:desc"`
}

func (pluginAuditRow) TableName() string { return "plugin_audit" }

// pluginLogRow 是一行插件日志。日志是排障用的，只按插件保留最近若干条，不归档。
type pluginLogRow struct {
	ID       int64  `gorm:"column:id;primaryKey;autoIncrement"`
	PluginID string `gorm:"column:plugin_id;size:191;not null;index:plugin_log_plugin,priority:1"`
	Level    string `gorm:"column:level;not null;default:''"`
	Hook     string `gorm:"column:hook;not null;default:''"`
	Message  string `gorm:"column:message;not null;default:''"`
	At       int64  `gorm:"column:at;not null;index:plugin_log_plugin,priority:2,sort:desc"`
}

func (pluginLogRow) TableName() string { return "plugin_logs" }

// tables 是 AutoMigrate 与旧库搬迁共用的表清单，顺序不影响结果（GORM 会按依赖重排）。
func tables() []any {
	return []any{
		&accountRow{},
		&requestRow{},
		&providerRow{},
		&providerRouteRow{},
		&providerHeaderRow{},
		&providerModelRow{},
		&accountModelRow{},
		&modelAliasRow{},
		&pluginRow{},
		&pluginVersionRow{},
		&pluginOptionRow{},
		&modelOperationRow{},
		&pluginAuditRow{},
		&pluginLogRow{},
	}
}

// seedProvider 是一行种子厂商。
type seedProvider struct {
	name            string
	baseURL         string
	defaultProtocol string
}

// seedRoute 是一行种子路由。modelsPath 空串表示这条协议没有自己的列表接口。
type seedRoute struct {
	provider   string
	protocol   string
	path       string
	modelsPath string
}

// seedProviders 是内置厂商。默认协议显式给出：deepseek 与 commandcode 各声明了
// openai-chat 与 anthropic 两条路由，按协议名排序会把 anthropic 排前面，默认协议会
// 从 openai-chat 变成 anthropic，这两个厂商的账号在升级前后默认协议就不一致了。
var seedProviders = []seedProvider{
	{"anthropic", "https://api.anthropic.com", "anthropic"},
	{"openai", "https://api.openai.com", "openai-chat"},
	{"deepseek", "https://api.deepseek.com", "openai-chat"},
	{"moonshot", "https://api.moonshot.cn", "openai-chat"},
	{"kimi", "https://api.moonshot.cn", "openai-chat"},
	{"openrouter", "https://openrouter.ai/api", "openai-chat"},
	{"ollama", "http://127.0.0.1:11434", "openai-chat"},
	{"commandcode", "https://api.commandcode.ai/provider/v1", "openai-chat"},
	{"google-gemini", "https://generativelanguage.googleapis.com", "gemini"},
}

// seedRoutes 是内置路由。路径照抄各家文档；models_path 为空表示这家在这个协议下没有模型
// 列表接口，同步时回退到同厂商里按协议名排序的第一条非空路径。
var seedRoutes = []seedRoute{
	{"anthropic", "anthropic", "/v1/messages", "/v1/models"},
	{"openai", "openai-chat", "/v1/chat/completions", "/v1/models"},
	{"openai", "openai-responses", "/v1/responses", "/v1/models"},
	{"deepseek", "openai-chat", "/v1/chat/completions", "/v1/models"},
	{"deepseek", "anthropic", "/anthropic/v1/messages", ""},
	{"moonshot", "openai-chat", "/v1/chat/completions", "/v1/models"},
	{"kimi", "openai-chat", "/v1/chat/completions", "/v1/models"},
	{"openrouter", "openai-chat", "/v1/chat/completions", "/v1/models"},
	{"ollama", "openai-chat", "/v1/chat/completions", "/v1/models"},
	{"commandcode", "openai-chat", "/chat/completions", "/models"},
	{"commandcode", "anthropic", "/messages", ""},
	{"google-gemini", "gemini", "/v1beta/models/{model}:generateContent", "/v1beta/models"},
	{"openai", "openai-embeddings", "/v1/embeddings", ""},
	{"ollama", "openai-embeddings", "/v1/embeddings", ""},
	{"openai", "openai-speech", "/v1/audio/speech", ""},
	{"openai", "openai-transcriptions", "/v1/audio/transcriptions", ""},
}

// seed 写入内置厂商与路由。
//
// 冲突一律跳过，不覆盖已有行：用户可能已经用 `provider add` 建过同名厂商（地址指向代理），
// 种子不该改他的写法，也不该因为主键冲突整笔失败。
func seed(db *gorm.DB) error {
	rows := make([]providerRow, 0, len(seedProviders))
	for _, p := range seedProviders {
		rows = append(rows, providerRow{
			Name:            p.name,
			BaseURL:         p.baseURL,
			DefaultProtocol: p.defaultProtocol,
		})
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return fmt.Errorf("写入内置厂商: %w", err)
	}

	routes := make([]providerRouteRow, 0, len(seedRoutes))
	for _, r := range seedRoutes {
		routes = append(routes, providerRouteRow{
			Provider:   r.provider,
			Protocol:   r.protocol,
			Path:       r.path,
			ModelsPath: r.modelsPath,
		})
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&routes).Error; err != nil {
		return fmt.Errorf("写入内置厂商路由: %w", err)
	}
	return nil
}

// migrate 建表、加列，并在首次建库时写种子。旧库的搬迁由 OpenWith 在连库之前做，不在这里。
//
// 种子只在库还没有 providers 表时写：它是一个「首次初始化」步骤，不是一个幂等的补丁。
// 每次打开都补种子会把用户删掉的内置厂商与路由复活——`provider rm anthropic` 之后重启
// 就又多回来一行，与「种子行可以被用户删光」的约定矛盾。
func migrate(db *gorm.DB) error {
	fresh := !db.Migrator().HasTable(&providerRow{})
	if err := db.AutoMigrate(tables()...); err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	return seed(db)
}

// unixNow 是仓储层唯一取时间的地方，便于测试替换。
var unixNow = func() int64 { return time.Now().Unix() }
