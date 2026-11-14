// Package quota 是账号额度的纯逻辑：上游响应的解析、金额折算、到期判断与设置校验。
//
// 这个包不读数据库、不发请求、不读环境变量：调用方（gateway）负责取响应体，
// store 负责落库，CLI 负责输出。额度只是只读监控与选号参考，任何解析失败都只影响
// 这一次快照，不会改账号的启用状态。
package quota

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 监控来源。auto 按账号所属厂商映射，off 表示关闭监控。
const (
	SourceAuto        = "auto"
	SourceCommandCode = "commandcode"
	SourceDeepSeek    = "deepseek"
	SourceOff         = "off"
)

// 一次额度快照的状态。ok 表示全部端点成功，partial 表示部分端点失败但仍有可用数据，
// auth 表示凭据被拒（也只记状态，不因此停用账号），error 表示其它失败，
// unsupported 表示这家厂商/当前设置没有额度接口。
const (
	StatusOK          = "ok"
	StatusPartial     = "partial"
	StatusAuth        = "auth"
	StatusError       = "error"
	StatusUnsupported = "unsupported"
)

// 到期处理方式：warn 只展示，deprioritize 把账号排到后面，exclude 直接排除。
const (
	ExpiryModeWarn         = "warn"
	ExpiryModeDeprioritize = "deprioritize"
	ExpiryModeExclude      = "exclude"
)

// 到期状态。空串表示周期结束时间未知。
const (
	ExpiryNear    = "near"
	ExpiryExpired = "expired"
)

// 额度排序方式。
const (
	OrderOff        = "off"
	OrderExpiryAsc  = "expiry_asc"
	OrderExpiryDesc = "expiry_desc"
	OrderRatioDesc  = "ratio_desc"
	OrderRatioAsc   = "ratio_asc"
	OrderUSDDesc    = "usd_desc"
	OrderUSDAsc     = "usd_asc"
)

// DefaultExpiryDays 是「临近到期」的默认提前天数。
const DefaultExpiryDays = 3

// Window 是一个限流窗口（例如 5 小时、每周）。它不是月度总额，
// 只用于展示，不参与余额折算。
type Window struct {
	Used    *float64
	Limit   *float64
	ResetAt int64 // Unix 毫秒；0 表示未知
}

// UpstreamUsage 是上游自己的用量汇总，只供展示。数值单位以上游为准，不做换算。
type UpstreamUsage struct {
	TotalCredits *float64 `json:"total_credits,omitempty"`
	Requests     *int64   `json:"requests,omitempty"`
	SuccessRate  *float64 `json:"success_rate,omitempty"`
}

// Facts 是一个账号最近一次额度快照。金额原值与 USD 折算值分开存：
// 汇率未知时原值仍然有效，只是 USD 值、剩余比例与美元排序都算不出来。
type Facts struct {
	Source            string
	Status            string
	DisplayName       string
	Balance           float64
	BalanceKnown      bool
	Used              float64
	UsedKnown         bool
	Currency          string
	FXRate            *float64
	BalanceUSD        *float64
	UsedUSD           *float64
	TotalUSD          *float64
	RemainingRatio    *float64
	PeriodEnd         int64 // Unix 毫秒；0 表示未知
	PlanID            string
	PlanStatus        string
	CancelAtPeriodEnd bool
	FiveHour          *Window
	Weekly            *Window
	Usage             UpstreamUsage
	Available         *bool
	CheckedAt         int64
	LastError         string
	// SourceErrors 只存每个端点的错误文字，键是 whoami、credits、subscriptions、usage、balance。
	SourceErrors map[string]string
}

// Settings 是一个账号的额度监控设置。
type Settings struct {
	Source      string
	FXRate      *float64
	AutoRefresh bool
	ExpiryMode  string
	ExpiryDays  int
	BaseURL     string
}

// Entry 是选号侧看到的一份额度事实与它的设置。
type Entry struct {
	Facts    Facts
	Settings Settings
}

// DefaultSettings 返回默认监控设置：默认关闭自动刷新、只提示到期、提前 3 天。
func DefaultSettings() Settings {
	return Settings{
		Source:     SourceAuto,
		ExpiryMode: ExpiryModeWarn,
		ExpiryDays: DefaultExpiryDays,
	}
}

// Apply 计算 USD 折算值与剩余比例。
//
// CommandCode 的分母是「余额 + 本期已用」，两者都已知时才给总额与比例；
// DeepSeek 只报余额，已用与总额未知，比例因此也未知。
// 币种是 USD（或空）时汇率按 1；其它币种使用设置里的手工汇率，未设置时 USD 值一律未知。
func Apply(f Facts, s Settings) Facts {
	f.FXRate = nil
	f.BalanceUSD = nil
	f.UsedUSD = nil
	f.TotalUSD = nil
	f.RemainingRatio = nil

	rate, ok := usdRate(f.Currency, s)
	if !ok {
		return f
	}
	r := rate
	f.FXRate = &r
	if f.BalanceKnown {
		v := f.Balance * rate
		f.BalanceUSD = &v
	}
	if f.UsedKnown {
		v := f.Used * rate
		f.UsedUSD = &v
	}
	if f.BalanceKnown && f.UsedKnown {
		total := (f.Balance + f.Used) * rate
		f.TotalUSD = &total
		if denominator := f.Balance + f.Used; denominator > 0 {
			ratio := f.Balance / denominator
			f.RemainingRatio = &ratio
		}
	}
	return f
}

// usdRate 给出把原币种折成 USD 的乘数。USD 与空币种按 1，其它币种只认手工汇率。
func usdRate(currency string, s Settings) (float64, bool) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" || currency == "USD" || currency == "CREDITS" || currency == "CREDIT" {
		return 1, true
	}
	if s.FXRate == nil || !validPositive(*s.FXRate) {
		return 0, false
	}
	return *s.FXRate, true
}

// ExpiryAt 判断周期结束时间相对 now 的状态与应执行的动作。
// 返回的 state 是 near 或 expired（空串表示未知/未临近）；action 是设置里的到期处理方式，
// 状态为空时动作也为空。到期判断只看时间，不看 cancelAtPeriodEnd：
// 自动续费与否只影响展示，不改变用户配置的到期规则。
func ExpiryAt(periodEndMS int64, now time.Time, s Settings) (state, action string) {
	if periodEndMS <= 0 {
		return "", ""
	}
	nowMS := now.UnixMilli()
	switch {
	case nowMS >= periodEndMS:
		state = ExpiryExpired
	case s.ExpiryDays > 0 && periodEndMS-nowMS <= int64(s.ExpiryDays)*24*60*60*1000:
		state = ExpiryNear
	default:
		return "", ""
	}
	action = s.ExpiryMode
	if action == "" {
		action = ExpiryModeWarn
	}
	return state, action
}

// ValidateOrder 校验额度排序取值。
func ValidateOrder(order string) error {
	switch order {
	case OrderOff, OrderExpiryAsc, OrderExpiryDesc, OrderRatioDesc,
		OrderRatioAsc, OrderUSDDesc, OrderUSDAsc:
		return nil
	}
	return fmt.Errorf("不支持的额度排序 %q，可用 off、expiry_asc、expiry_desc、ratio_desc、ratio_asc、usd_desc、usd_asc", order)
}

// ValidateMinUSD 校验最低折合余额阈值。
func ValidateMinUSD(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fmt.Errorf("最低折合 USD 必须是不小于 0 的数，当前是 %v", v)
	}
	return nil
}

// ValidateSettings 校验监控设置。CLI 与 store 写入口共用它，避免两套范围。
func ValidateSettings(s Settings) error {
	switch s.Source {
	case SourceAuto, SourceCommandCode, SourceDeepSeek, SourceOff:
	default:
		return fmt.Errorf("不支持的监控来源 %q，可用 auto、commandcode、deepseek、off", s.Source)
	}
	if s.FXRate != nil && !validPositive(*s.FXRate) {
		return fmt.Errorf("汇率必须是大于 0 的数，当前是 %v", *s.FXRate)
	}
	switch s.ExpiryMode {
	case ExpiryModeWarn, ExpiryModeDeprioritize, ExpiryModeExclude:
	default:
		return fmt.Errorf("不支持的到期处理 %q，可用 warn、deprioritize、exclude", s.ExpiryMode)
	}
	if s.ExpiryDays < 1 || s.ExpiryDays > 3650 {
		return fmt.Errorf("提前天数必须在 1 到 3650 之间，当前是 %d", s.ExpiryDays)
	}
	if strings.TrimSpace(s.BaseURL) != "" {
		if _, err := originURL(s.BaseURL); err != nil {
			return fmt.Errorf("额度接口地址不合法：%v", err)
		}
	}
	return nil
}

func validPositive(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

// CommandCodeEndpoints 是 CommandCode 四个额度端点的完整地址。
type CommandCodeEndpoints struct {
	WhoAmI        string
	Credits       string
	Subscriptions string
	Usage         string
}

// CommandCodeURLs 从 base_url 取 origin，再拼四个 /alpha 端点。
// base_url 可能带 /provider/v1 一类前缀，额度接口在同一个 origin 的根路径下。
func CommandCodeURLs(baseURL string) (CommandCodeEndpoints, error) {
	origin, err := originURL(baseURL)
	if err != nil {
		return CommandCodeEndpoints{}, err
	}
	return CommandCodeEndpoints{
		WhoAmI:        origin + "/alpha/whoami",
		Credits:       origin + "/alpha/billing/credits",
		Subscriptions: origin + "/alpha/billing/subscriptions",
		Usage:         origin + "/alpha/usage/summary",
	}, nil
}

// DeepSeekBalanceURL 返回 DeepSeek 余额接口的完整地址。
func DeepSeekBalanceURL(baseURL string) (string, error) {
	origin, err := originURL(baseURL)
	if err != nil {
		return "", err
	}
	return origin + "/user/balance", nil
}

// originURL 校验 base_url 并取出 scheme://host。
func originURL(baseURL string) (string, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return "", fmt.Errorf("缺少 base_url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("base_url %q 解析失败：%v", baseURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("base_url %q 的协议必须是 http 或 https", baseURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url %q 缺少主机名", baseURL)
	}
	// 额度请求会带解密后的账号 key。明文 http 只允许本机（本地代理通常监听
	// 127.0.0.1 / ::1），其余地址必须 https，避免一次配置失误或重定向把 key 明文发出去。
	if scheme == "http" && !isLoopbackHost(u.Host) {
		return "", fmt.Errorf("base_url %q 是明文 http；额度请求会带账号凭据，非本机地址必须用 https", baseURL)
	}
	return scheme + "://" + u.Host, nil
}

// isLoopbackHost 判断 host:port 是不是本机回环地址。localhost 按约定也算本机。
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BrowserUserAgent 是查额度时使用的浏览器 UA。CommandCode 的 /alpha 接口缺 UA 会回 403。
func BrowserUserAgent() string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
}

// ParseCommandCodeWhoAmI 取显示名。字段缺失或为空时返回错误，由调用方当作非致命失败。
func ParseCommandCodeWhoAmI(data []byte) (string, error) {
	doc, err := decodeObject(data)
	if err != nil {
		return "", err
	}
	// 显示名可能直接在最外层，也可能在 user / account / profile / data 一层里。
	objects := []map[string]any{doc}
	for _, wrapper := range []string{"user", "account", "profile", "data"} {
		if inner, ok := doc[wrapper].(map[string]any); ok {
			objects = append(objects, inner)
		}
	}
	for _, obj := range objects {
		for _, key := range []string{"displayName", "display_name", "name", "username", "email"} {
			if v := strings.TrimSpace(stringValue(obj[key])); v != "" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("whoami 响应里没有显示名")
}

// ParseCommandCodeCredits 取剩余额度。monthlyCredits 是剩余，不是总额；
// 本期已用在 usage.summary.totalCredits 里。币种缺省按 USD。
func ParseCommandCodeCredits(data []byte) (balance float64, currency string, err error) {
	doc, err := decodeObject(data)
	if err != nil {
		return 0, "", err
	}
	// 常见的包装层：data、billing 或 credits 对象。credits 本身是数字时它不是 map，跳过。
	for _, wrapper := range []string{"data", "billing", "credits"} {
		if inner, ok := doc[wrapper].(map[string]any); ok {
			doc = inner
			break
		}
	}
	currency = "USD"
	if c := strings.TrimSpace(stringValue(firstValue(doc, "currency", "unit"))); c != "" {
		currency = strings.ToUpper(c)
	}
	for _, key := range []string{"monthlyCredits", "monthly_credits", "monthlyCredit", "balance", "credits", "availableCredits", "available_credits"} {
		if n, ok := numberFrom(doc[key]); ok {
			return n, currency, nil
		}
	}
	return 0, "", fmt.Errorf("credits 响应里没有余额字段")
}

// Subscription 是 CommandCode 订阅信息里与额度有关的部分。
type Subscription struct {
	PeriodEnd         int64
	PlanID            string
	PlanStatus        string
	CancelAtPeriodEnd bool
	FiveHour          *Window
	Weekly            *Window
}

// ParseCommandCodeSubscription 解析周期结束、套餐与两个限流窗口。
// currentPeriodEnd 同时接受 RFC3339、Unix 秒与 Unix 毫秒；窗口的 resetAt 统一存毫秒。
func ParseCommandCodeSubscription(data []byte) (Subscription, error) {
	doc, err := decodeObject(data)
	if err != nil {
		return Subscription{}, err
	}
	// 常见的包装层。
	for _, key := range []string{"subscription", "data"} {
		if inner, ok := doc[key].(map[string]any); ok {
			doc = inner
			break
		}
	}
	var sub Subscription
	known := false
	if ms, ok := parseTimeMS(firstValue(doc, "currentPeriodEnd", "current_period_end", "periodEnd", "period_end")); ok {
		sub.PeriodEnd = ms
		known = true
	}
	sub.PlanID = strings.TrimSpace(stringValue(firstValue(doc, "planId", "plan_id")))
	sub.PlanStatus = strings.TrimSpace(stringValue(firstValue(doc, "planStatus", "plan_status", "status")))
	// plan 也可能是对象：取里面的 id / slug / name，状态同理。
	if plan, ok := firstValue(doc, "plan", "subscriptionPlan", "subscription_plan").(map[string]any); ok {
		if sub.PlanID == "" {
			sub.PlanID = strings.TrimSpace(stringValue(firstValue(plan, "id", "slug", "name")))
		}
		if sub.PlanStatus == "" {
			sub.PlanStatus = strings.TrimSpace(stringValue(firstValue(plan, "status")))
		}
	} else if sub.PlanID == "" {
		sub.PlanID = strings.TrimSpace(stringValue(firstValue(doc, "plan")))
	}
	if sub.PlanID != "" {
		known = true
	}
	if b, ok := boolValue(firstValue(doc, "cancelAtPeriodEnd", "cancel_at_period_end")); ok {
		sub.CancelAtPeriodEnd = b
		known = true
	}
	if limits, ok := firstValue(doc, "windowLimits", "window_limits", "limits").(map[string]any); ok {
		sub.FiveHour = parseWindow(firstValue(limits, "fiveHour", "five_hour", "fiveHourWindow", "5h"))
		sub.Weekly = parseWindow(firstValue(limits, "weekly", "week", "weeklyWindow"))
		if sub.FiveHour != nil || sub.Weekly != nil {
			known = true
		}
	}
	// 200 也可能回 `{}`、`{"subscription":null}` 或字段名改过的空对象。一个已知字段都
	// 没命中时必须报错，让调用方把 subscriptions 记进 SourceErrors 并保留旧快照；
	// 否则零值会被当成成功，把上次已知的到期时间、套餐与窗口清空，expiry_mode=exclude 静默失效。
	if !known {
		return Subscription{}, fmt.Errorf("subscriptions 响应里没有已知字段")
	}
	return sub, nil
}

// parseWindow 解析一个限流窗口；一个已知字段都没有时返回 nil。
func parseWindow(v any) *Window {
	doc, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	w := &Window{}
	if n, ok := firstNumber(doc, "used", "creditsUsed", "usage", "consumed"); ok {
		w.Used = &n
	}
	if n, ok := firstNumber(doc, "limit", "total", "cap", "max"); ok {
		w.Limit = &n
	}
	if ms, ok := parseTimeMS(firstValue(doc, "resetAt", "reset_at", "resetsAt", "resets_at")); ok {
		w.ResetAt = ms
	}
	if w.Used == nil && w.Limit == nil && w.ResetAt == 0 {
		return nil
	}
	return w
}

// ParseCommandCodeUsage 解析上游用量汇总。summary 可能直接在最外层，也可能在 usage 里。
// 没有已知字段但 JSON 合法时返回零值：用量只是展示，不因缺字段判整个端点失败。
func ParseCommandCodeUsage(data []byte) (UpstreamUsage, error) {
	doc, err := decodeObject(data)
	if err != nil {
		return UpstreamUsage{}, err
	}
	if inner, ok := doc["usage"].(map[string]any); ok {
		doc = inner
	}
	if summary, ok := doc["summary"].(map[string]any); ok {
		doc = summary
	}
	var out UpstreamUsage
	if n, ok := firstNumber(doc, "totalCredits", "total_credits", "creditsUsed", "credits_used"); ok {
		out.TotalCredits = &n
	}
	if n, ok := firstNumber(doc, "requests", "requestCount", "request_count", "totalRequests"); ok {
		v := int64(n)
		out.Requests = &v
	}
	if n, ok := firstNumber(doc, "successRate", "success_rate"); ok {
		out.SuccessRate = &n
	}
	return out, nil
}

// ParseDeepSeekBalance 解析 DeepSeek 余额。多币种时优先 CNY，否则取第一条。
// total_balance 在响应里是字符串，这里按数字解析。
func ParseDeepSeekBalance(data []byte) (balance float64, currency string, available *bool, err error) {
	doc, err := decodeObject(data)
	if err != nil {
		return 0, "", nil, err
	}
	if b, ok := boolValue(firstValue(doc, "is_available", "isAvailable")); ok {
		available = &b
	}
	infos, _ := doc["balance_infos"].([]any)
	type info struct {
		balance  float64
		currency string
	}
	parsed := make([]info, 0, len(infos))
	for _, raw := range infos {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		n, ok := firstNumber(m, "total_balance", "totalBalance", "balance")
		if !ok {
			continue
		}
		cur := strings.ToUpper(strings.TrimSpace(stringValue(firstValue(m, "currency", "unit"))))
		parsed = append(parsed, info{balance: n, currency: cur})
	}
	if len(parsed) == 0 {
		return 0, "", available, fmt.Errorf("balance 响应里没有可用余额")
	}
	chosen := parsed[0]
	for _, p := range parsed {
		if p.currency == "CNY" {
			chosen = p
			break
		}
	}
	return chosen.balance, chosen.currency, available, nil
}

func decodeObject(data []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("不是合法的 JSON 对象: %v", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("响应是空对象")
	}
	return doc, nil
}

func firstValue(doc map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := doc[key]; ok && v != nil {
			return v
		}
	}
	return nil
}

func firstNumber(doc map[string]any, keys ...string) (float64, bool) {
	return numberFrom(firstValue(doc, keys...))
}

// numberFrom 把数字、数字字符串或带常见数值字段的小对象解成 float64。
func numberFrom(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		// 金额字符串可能带千分位或货币符号，按数字解之前先去掉。
		s := strings.TrimSpace(n)
		s = strings.Trim(s, "$¥€ ")
		s = strings.ReplaceAll(s, ",", "")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case map[string]any:
		for _, key := range []string{"credits", "amount", "remaining", "balance", "value", "total"} {
			if f, ok := numberFrom(n[key]); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func stringValue(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	}
	return ""
}

func boolValue(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// parseTimeMS 把 RFC3339 字符串、Unix 秒/毫秒数字统一成 Unix 毫秒。
// 秒与毫秒按数量级区分：1e11 以下的数字是秒（约公元 5138 年之前）。
func parseTimeMS(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return epochToMS(int64(t)), true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return epochToMS(n), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.UnixMilli(), true
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return epochToMS(n), true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return epochToMS(int64(f)), true
		}
	}
	return 0, false
}

func epochToMS(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > 100_000_000_000 {
		return n
	}
	return n * 1000
}
