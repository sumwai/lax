package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lax/internal/quota"
)

// SaveQuotaSnapshot 按 account_id 覆盖写一个账号的额度快照。
// 一个账号只保留最近一次：额度是监控事实，不是审计记录，历史快照不落库。
func (s *Store) SaveQuotaSnapshot(accountID int64, f quota.Facts) error {
	row, err := quotaSnapshotRowFrom(accountID, f)
	if err != nil {
		return err
	}
	return s.gorm.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"source", "status", "display_name", "balance", "balance_known", "used", "used_known",
			"currency", "fx_rate", "balance_usd", "used_usd", "total_usd", "remaining_ratio",
			"period_end", "plan_id", "plan_status", "cancel_at_period_end",
			"five_hour_used", "five_hour_limit", "five_hour_reset_at",
			"weekly_used", "weekly_limit", "weekly_reset_at",
			"upstream_usage_json", "available", "checked_at", "last_error",
			"source_errors_json", "updated_at",
		}),
	}).Create(&row).Error
}

// QuotaSnapshot 读一个账号的额度快照。没有快照时 found 为 false。
func (s *Store) QuotaSnapshot(accountID int64) (quota.Facts, bool, error) {
	var row quotaSnapshotRow
	err := s.gorm.Where("account_id = ?", accountID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return quota.Facts{}, false, nil
	}
	if err != nil {
		return quota.Facts{}, false, err
	}
	facts, err := quotaFactsFromRow(row)
	if err != nil {
		return quota.Facts{}, false, err
	}
	return facts, true, nil
}

// QuotaEntries 返回所有有快照的账号的额度事实与设置，键是账号 ID。
// 没有设置行的账号用 DefaultSettings；USD 字段按当前设置重新折算一次，
// 用户改了汇率之后不必等下一次刷新才看到新的折合余额。
func (s *Store) QuotaEntries() (map[int64]quota.Entry, error) {
	var snapshots []quotaSnapshotRow
	if err := s.gorm.Find(&snapshots).Error; err != nil {
		return nil, err
	}
	settings, err := s.AllQuotaSettings()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]quota.Entry, len(snapshots))
	for _, row := range snapshots {
		facts, err := quotaFactsFromRow(row)
		if err != nil {
			return nil, err
		}
		cfg, ok := settings[row.AccountID]
		if !ok {
			cfg = quota.DefaultSettings()
		}
		// 用户明确关闭监控（source=off）时，旧快照不能再算额度数据：否则关掉监控后
		// route set --quota-min-usd 仍会按旧余额把账号排除，网关报「额度过滤后没有候选」。
		if cfg.Source == quota.SourceOff {
			continue
		}
		facts = quota.Apply(facts, cfg)
		out[row.AccountID] = quota.Entry{Facts: facts, Settings: cfg}
	}
	return out, nil
}

// QuotaSettings 读一个账号的监控设置。没有设置行时 found 为 false，调用方用 DefaultSettings。
func (s *Store) QuotaSettings(accountID int64) (quota.Settings, bool, error) {
	var row quotaSettingsRow
	err := s.gorm.Where("account_id = ?", accountID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return quota.DefaultSettings(), false, nil
	}
	if err != nil {
		return quota.Settings{}, false, err
	}
	return quotaSettingsFromRow(row), true, nil
}

// AllQuotaSettings 返回全部显式设置行，键是账号 ID。没有行表示默认设置，不在这里补。
func (s *Store) AllQuotaSettings() (map[int64]quota.Settings, error) {
	var rows []quotaSettingsRow
	if err := s.gorm.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int64]quota.Settings, len(rows))
	for _, row := range rows {
		out[row.AccountID] = quotaSettingsFromRow(row)
	}
	return out, nil
}

// SetQuotaSettings 校验后覆盖写一个账号的监控设置。
func (s *Store) SetQuotaSettings(accountID int64, cfg quota.Settings) error {
	if err := quota.ValidateSettings(cfg); err != nil {
		return err
	}
	row := quotaSettingsRowFrom(accountID, cfg)
	return s.gorm.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"source", "fx_rate", "auto_refresh", "expiry_mode", "expiry_days", "base_url", "updated_at",
		}),
	}).Create(&row).Error
}

// DeleteQuotaSettings 删除一个账号的监控设置，返回是否真的删到了行。
// 删掉之后这个账号回到默认设置（auto / 不自动刷新 / warn / 3 天）。
func (s *Store) DeleteQuotaSettings(accountID int64) (bool, error) {
	res := s.gorm.Where("account_id = ?", accountID).Delete(&quotaSettingsRow{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// DeleteQuotaSnapshot 删除一个账号的额度快照，返回是否真的删到了行。
// 监控来源改成 off、切到另一家来源或刷新返回 unsupported 时，旧快照要清掉，
// 不能让它跨来源继续参与选号。
func (s *Store) DeleteQuotaSnapshot(accountID int64) (bool, error) {
	res := s.gorm.Where("account_id = ?", accountID).Delete(&quotaSnapshotRow{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// quotaSnapshotRowFrom 把快照转成数据库行。JSON 列只存解析后的结构化数据，
// 上游原始报文一个字节都不落库。
func quotaSnapshotRowFrom(accountID int64, f quota.Facts) (quotaSnapshotRow, error) {
	usageJSON, err := marshalOptionalJSON(upstreamUsageEmpty(f.Usage), f.Usage)
	if err != nil {
		return quotaSnapshotRow{}, fmt.Errorf("序列化上游用量失败: %w", err)
	}
	errorsJSON, err := marshalOptionalJSON(len(f.SourceErrors) == 0, f.SourceErrors)
	if err != nil {
		return quotaSnapshotRow{}, fmt.Errorf("序列化额度端点错误失败: %w", err)
	}
	row := quotaSnapshotRow{
		AccountID:         accountID,
		Source:            f.Source,
		Status:            f.Status,
		DisplayName:       f.DisplayName,
		Balance:           f.Balance,
		BalanceKnown:      f.BalanceKnown,
		Used:              f.Used,
		UsedKnown:         f.UsedKnown,
		Currency:          f.Currency,
		FXRate:            f.FXRate,
		BalanceUSD:        f.BalanceUSD,
		UsedUSD:           f.UsedUSD,
		TotalUSD:          f.TotalUSD,
		RemainingRatio:    f.RemainingRatio,
		PeriodEnd:         f.PeriodEnd,
		PlanID:            f.PlanID,
		PlanStatus:        f.PlanStatus,
		CancelAtPeriodEnd: f.CancelAtPeriodEnd,
		UpstreamUsageJSON: usageJSON,
		Available:         f.Available,
		CheckedAt:         f.CheckedAt,
		LastError:         f.LastError,
		SourceErrorsJSON:  errorsJSON,
		UpdatedAt:         time.Now().Unix(),
	}
	if w := f.FiveHour; w != nil {
		row.FiveHourUsed, row.FiveHourLimit, row.FiveHourResetAt = w.Used, w.Limit, w.ResetAt
	}
	if w := f.Weekly; w != nil {
		row.WeeklyUsed, row.WeeklyLimit, row.WeeklyResetAt = w.Used, w.Limit, w.ResetAt
	}
	return row, nil
}

// quotaFactsFromRow 把数据库行转回快照。JSON 列损坏时返回错误，不静默丢数据。
func quotaFactsFromRow(row quotaSnapshotRow) (quota.Facts, error) {
	f := quota.Facts{
		Source:            row.Source,
		Status:            row.Status,
		DisplayName:       row.DisplayName,
		Balance:           row.Balance,
		BalanceKnown:      row.BalanceKnown,
		Used:              row.Used,
		UsedKnown:         row.UsedKnown,
		Currency:          row.Currency,
		FXRate:            row.FXRate,
		BalanceUSD:        row.BalanceUSD,
		UsedUSD:           row.UsedUSD,
		TotalUSD:          row.TotalUSD,
		RemainingRatio:    row.RemainingRatio,
		PeriodEnd:         row.PeriodEnd,
		PlanID:            row.PlanID,
		PlanStatus:        row.PlanStatus,
		CancelAtPeriodEnd: row.CancelAtPeriodEnd,
		Available:         row.Available,
		CheckedAt:         row.CheckedAt,
		LastError:         row.LastError,
	}
	if row.FiveHourUsed != nil || row.FiveHourLimit != nil || row.FiveHourResetAt > 0 {
		f.FiveHour = &quota.Window{Used: row.FiveHourUsed, Limit: row.FiveHourLimit, ResetAt: row.FiveHourResetAt}
	}
	if row.WeeklyUsed != nil || row.WeeklyLimit != nil || row.WeeklyResetAt > 0 {
		f.Weekly = &quota.Window{Used: row.WeeklyUsed, Limit: row.WeeklyLimit, ResetAt: row.WeeklyResetAt}
	}
	if row.UpstreamUsageJSON != "" {
		if err := json.Unmarshal([]byte(row.UpstreamUsageJSON), &f.Usage); err != nil {
			return quota.Facts{}, fmt.Errorf("账号 %d 的上游用量 JSON 损坏: %w", row.AccountID, err)
		}
	}
	if row.SourceErrorsJSON != "" {
		if err := json.Unmarshal([]byte(row.SourceErrorsJSON), &f.SourceErrors); err != nil {
			return quota.Facts{}, fmt.Errorf("账号 %d 的额度端点错误 JSON 损坏: %w", row.AccountID, err)
		}
	}
	return f, nil
}

func quotaSettingsFromRow(row quotaSettingsRow) quota.Settings {
	cfg := quota.DefaultSettings()
	if row.Source != "" {
		cfg.Source = row.Source
	}
	cfg.FXRate = row.FXRate
	cfg.AutoRefresh = row.AutoRefresh
	if row.ExpiryMode != "" {
		cfg.ExpiryMode = row.ExpiryMode
	}
	if row.ExpiryDays > 0 {
		cfg.ExpiryDays = row.ExpiryDays
	}
	cfg.BaseURL = row.BaseURL
	return cfg
}

func quotaSettingsRowFrom(accountID int64, cfg quota.Settings) quotaSettingsRow {
	return quotaSettingsRow{
		AccountID:   accountID,
		Source:      cfg.Source,
		FXRate:      cfg.FXRate,
		AutoRefresh: cfg.AutoRefresh,
		ExpiryMode:  cfg.ExpiryMode,
		ExpiryDays:  cfg.ExpiryDays,
		BaseURL:     cfg.BaseURL,
		UpdatedAt:   time.Now().Unix(),
	}
}

// upstreamUsageEmpty 判断上游用量是不是没有可存的内容。
func upstreamUsageEmpty(u quota.UpstreamUsage) bool {
	return u.TotalCredits == nil && u.Requests == nil && u.SuccessRate == nil
}

// marshalOptionalJSON 在 empty 为真时返回空串，否则返回 JSON 文本。
func marshalOptionalJSON(empty bool, v any) (string, error) {
	if empty {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
