package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"lax/internal/quota"
	"lax/internal/secret"
	"lax/internal/store"
)

// quotaBodyLimit 是一次额度响应的读取上限。额度响应都是很小的 JSON，
// 1 MiB 足够；读不完的响应按失败处理，不把整包内容攒进内存。
const quotaBodyLimit = 1 << 20

// quotaCall 是一次额度端点请求的结果。status 为 0 表示没拿到 HTTP 响应（连接层失败）。
type quotaCall struct {
	body   []byte
	status int
	err    error
}

// RefreshQuota 查一个账号的额度，返回一次快照。它绝不写库、绝不改账号状态：
// 额度刷新失败只体现在返回的 Facts.Status / LastError / SourceErrors 上，
// 由调用方决定要不要保存。401/403 也只记 auth，不会停用账号——只有真实的转发请求
// 401 才走现有的停用逻辑。
//
// 来源选择：settings.Source 为空或 auto 时按账号厂商映射（commandcode / deepseek）；
// off 或其它厂商返回 unsupported 与 nil error，调用方跳过即可，不算刷新失败。
// 地址优先 settings.BaseURL，否则用账号所属厂商的 base_url，并只取它的 origin。
func (s *Server) RefreshQuota(ctx context.Context, a store.Account, cfg quota.Settings) (quota.Facts, error) {
	facts := quota.Facts{
		Status:       quota.StatusError,
		CheckedAt:    s.now().Unix(),
		SourceErrors: map[string]string{},
	}
	source := resolveQuotaSource(a.Provider, cfg.Source)
	facts.Source = source

	if source == quota.SourceOff {
		facts.Status = quota.StatusUnsupported
		facts.LastError = "额度监控已关闭"
		return facts, nil
	}
	if source == "" {
		facts.Status = quota.StatusUnsupported
		facts.LastError = fmt.Sprintf("暂不支持厂商 %q 的额度查询（目前支持 commandcode、deepseek）", a.Provider)
		return facts, nil
	}

	plain, err := secret.Decrypt(s.key, a.KeyCipher)
	if err != nil {
		msg := "解密账号凭据失败，账号的 key 可能是在别的密钥下加密的"
		facts.SourceErrors["auth"] = msg
		facts.LastError = msg
		return facts, fmt.Errorf("%s", msg)
	}
	key := string(plain)

	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = strings.TrimSpace(a.BaseURL)
	}

	switch source {
	case quota.SourceCommandCode:
		return s.refreshCommandCodeQuota(ctx, facts, base, key, cfg)
	case quota.SourceDeepSeek:
		return s.refreshDeepSeekQuota(ctx, facts, base, key, cfg)
	default:
		facts.Status = quota.StatusUnsupported
		facts.LastError = fmt.Sprintf("暂不支持来源 %q 的额度查询", source)
		return facts, nil
	}
}

// resolveQuotaSource 把设置里的来源落到具体适配器。
// auto 按账号厂商映射；空串按 auto 处理（旧设置行或迁移后的默认列）；其余不认识的来源返回空串。
func resolveQuotaSource(provider, source string) string {
	switch source {
	case quota.SourceOff, quota.SourceCommandCode, quota.SourceDeepSeek:
		return source
	case "", quota.SourceAuto:
		switch provider {
		case quota.SourceCommandCode:
			return quota.SourceCommandCode
		case quota.SourceDeepSeek:
			return quota.SourceDeepSeek
		}
		return ""
	default:
		return ""
	}
}

// refreshCommandCodeQuota 打四个 /alpha 端点。whoami 只取显示名，失败不致命，
// 状态只看 credits / subscriptions / usage 三个：全成 ok，部分成 partial，
// 全失败时按有没有 401/403 分 auth 与 error。
func (s *Server) refreshCommandCodeQuota(ctx context.Context, facts quota.Facts,
	base, key string, cfg quota.Settings) (quota.Facts, error) {

	endpoints, err := quota.CommandCodeURLs(base)
	if err != nil {
		msg := fmt.Sprintf("拼 CommandCode 额度地址失败：%v", err)
		facts.SourceErrors["base_url"] = msg
		facts.LastError = msg
		return facts, fmt.Errorf("%s", msg)
	}
	headers := map[string]string{
		"x-api-key":  key,
		"User-Agent": quota.BrowserUserAgent(),
	}

	// whoami 只补显示名：失败只记在 SourceErrors 里，不参与状态判定，也不覆盖 LastError。
	if call := s.fetchQuotaEndpoint(ctx, endpoints.WhoAmI, key, headers); call.err != nil {
		facts.SourceErrors["whoami"] = call.err.Error()
	} else if name, perr := quota.ParseCommandCodeWhoAmI(call.body); perr != nil {
		facts.SourceErrors["whoami"] = perr.Error()
	} else {
		facts.DisplayName = name
	}

	okCount := 0
	authSeen := false
	apply := func(endpoint string, call quotaCall, parse func([]byte) error) {
		if call.err != nil {
			facts.SourceErrors[endpoint] = call.err.Error()
			if facts.LastError == "" {
				facts.LastError = call.err.Error()
			}
			if call.status == http.StatusUnauthorized || call.status == http.StatusForbidden {
				authSeen = true
			}
			return
		}
		if perr := parse(call.body); perr != nil {
			facts.SourceErrors[endpoint] = perr.Error()
			if facts.LastError == "" {
				facts.LastError = perr.Error()
			}
			return
		}
		okCount++
	}

	apply("credits", s.fetchQuotaEndpoint(ctx, endpoints.Credits, key, headers), func(body []byte) error {
		balance, currency, perr := quota.ParseCommandCodeCredits(body)
		if perr != nil {
			return perr
		}
		facts.Balance, facts.BalanceKnown, facts.Currency = balance, true, currency
		return nil
	})

	apply("subscriptions", s.fetchQuotaEndpoint(ctx, endpoints.Subscriptions, key, headers), func(body []byte) error {
		sub, perr := quota.ParseCommandCodeSubscription(body)
		if perr != nil {
			return perr
		}
		facts.PeriodEnd = sub.PeriodEnd
		facts.PlanID = sub.PlanID
		facts.PlanStatus = sub.PlanStatus
		facts.CancelAtPeriodEnd = sub.CancelAtPeriodEnd
		facts.FiveHour = sub.FiveHour
		facts.Weekly = sub.Weekly
		return nil
	})

	apply("usage", s.fetchQuotaEndpoint(ctx, endpoints.Usage, key, headers), func(body []byte) error {
		usage, perr := quota.ParseCommandCodeUsage(body)
		if perr != nil {
			return perr
		}
		facts.Usage = usage
		if usage.TotalCredits != nil {
			facts.Used, facts.UsedKnown = *usage.TotalCredits, true
		}
		return nil
	})

	switch {
	case okCount == 3:
		facts.Status = quota.StatusOK
		facts.LastError = ""
	case okCount == 0 && authSeen:
		facts.Status = quota.StatusAuth
	case okCount == 0:
		facts.Status = quota.StatusError
	default:
		facts.Status = quota.StatusPartial
	}

	facts = quota.Apply(facts, cfg)
	if facts.Status == quota.StatusAuth || facts.Status == quota.StatusError {
		return facts, quotaRefreshError(facts)
	}
	return facts, nil
}

// refreshDeepSeekQuota 只打 GET /user/balance。DeepSeek 不报总额与周期，
// 已用与剩余比例因此留空，只给余额与币种。
func (s *Server) refreshDeepSeekQuota(ctx context.Context, facts quota.Facts,
	base, key string, cfg quota.Settings) (quota.Facts, error) {

	target, err := quota.DeepSeekBalanceURL(base)
	if err != nil {
		msg := fmt.Sprintf("拼 DeepSeek 余额地址失败：%v", err)
		facts.SourceErrors["base_url"] = msg
		facts.LastError = msg
		return facts, fmt.Errorf("%s", msg)
	}
	headers := map[string]string{
		"Authorization": "Bearer " + key,
		"User-Agent":    quota.BrowserUserAgent(),
	}
	call := s.fetchQuotaEndpoint(ctx, target, key, headers)
	if call.err != nil {
		facts.SourceErrors["balance"] = call.err.Error()
		facts.LastError = call.err.Error()
		if call.status == http.StatusUnauthorized || call.status == http.StatusForbidden {
			facts.Status = quota.StatusAuth
		} else {
			facts.Status = quota.StatusError
		}
		return facts, quotaRefreshError(facts)
	}
	balance, currency, available, perr := quota.ParseDeepSeekBalance(call.body)
	if perr != nil {
		facts.SourceErrors["balance"] = perr.Error()
		facts.LastError = perr.Error()
		facts.Status = quota.StatusError
		return facts, quotaRefreshError(facts)
	}
	facts.Balance, facts.BalanceKnown, facts.Currency = balance, true, currency
	facts.Available = available
	facts.Status = quota.StatusOK
	facts = quota.Apply(facts, cfg)
	return facts, nil
}

// quotaMaxRedirects 是额度请求允许跟随的同 host 重定向次数。
const quotaMaxRedirects = 3

// quotaHTTPClient 返回额度请求专用的 client：复用网关的 Transport（测试与生产可能注入
// 自定义 TLS/代理），但单独收紧重定向策略。额度请求带的是解密后的账号 key，而 Go 默认
// 会跟随跨域重定向，且只剥离 Authorization/Cookie 等标准头，自定义的 x-api-key 会被
// 原样复制到新主机。这里拒绝跨 host、拒绝从 https 降级到 http，并限制跳转次数。
// 返回 http.ErrUseLastResponse 时 Do 会把 3xx 响应原样交回，fetchQuotaEndpoint 按非 2xx
// 记失败并关闭响应体，凭据不会发到第二个地址。
func (s *Server) quotaHTTPClient() *http.Client {
	base := s.client
	if base == nil {
		base = &http.Client{}
	}
	client := *base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if len(via) >= quotaMaxRedirects {
			return http.ErrUseLastResponse
		}
		first := via[0].URL
		if !strings.EqualFold(req.URL.Host, first.Host) {
			return http.ErrUseLastResponse
		}
		if strings.EqualFold(first.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &client
}

// fetchQuotaEndpoint 发一次 GET。非 2xx 也把状态码带回去：调用方要靠 401/403 判 auth。
// 错误文字统一过 scrubSecret，上游回显的 key 不会进状态、日志或库。
func (s *Server) fetchQuotaEndpoint(ctx context.Context, target, key string, headers map[string]string) quotaCall {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return quotaCall{err: fmt.Errorf("额度请求地址不合法：%s", scrubSecret(err.Error(), key))}
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := s.quotaHTTPClient().Do(req)
	if err != nil {
		return quotaCall{err: fmt.Errorf("请求上游失败：%s", scrubSecret(err.Error(), key))}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaBodyLimit))
	if err != nil {
		return quotaCall{status: resp.StatusCode, err: fmt.Errorf("读取上游响应失败：%s", scrubSecret(err.Error(), key))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return quotaCall{
			body:   body,
			status: resp.StatusCode,
			err:    fmt.Errorf("上游返回 %d：%s", resp.StatusCode, scrubSecret(errorMessageFromBody(body), key)),
		}
	}
	return quotaCall{body: body, status: resp.StatusCode}
}

// quotaRefreshError 把一次失败快照折成给 CLI 的错误。
func quotaRefreshError(facts quota.Facts) error {
	msg := facts.LastError
	if msg == "" {
		msg = "额度查询失败"
	}
	return fmt.Errorf("%s", msg)
}
