package store

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// RouteStrategyPriority 是主备：按 priority、ID 升序，失败再换下一个。
	RouteStrategyPriority = "priority"
	// RouteStrategyRotate 是按模型请求序号循环轮转。
	RouteStrategyRotate = "rotate"
	// RouteStrategyWeighted 是按账号权重做比例轮转。
	RouteStrategyWeighted = "weighted"
	// RouteStrategySuccess 是按统计窗口内的成功率降序。
	RouteStrategySuccess = "success"
	// RouteStrategySoftmax 是按得分做概率抽样。
	RouteStrategySoftmax = "softmax"

	// RouteStickyPrefer 让同一会话优先沿用哈希到的账号。
	RouteStickyPrefer = "prefer"
	// RouteStickyOff 忽略会话键对选号的影响。
	RouteStickyOff = "off"
	// RouteStickyStrict 只允许哈希到的账号，不可用时直接报错。
	RouteStickyStrict = "strict"

	// RouteDefaultWindowSeconds 是成功率与延迟的默认统计窗口（24 小时）。
	RouteDefaultWindowSeconds = 24 * 60 * 60
	// RouteMaxWindowSeconds 是统计窗口上限（30 天）。
	RouteMaxWindowSeconds = 30 * 24 * 60 * 60
	// RouteStatsRetentionSeconds 是分钟统计表的保留时长（30 天）。
	RouteStatsRetentionSeconds = 30 * 24 * 60 * 60
)

// RoutePolicy 是一个选号目标最终生效的策略。Model 为空串表示默认行。
type RoutePolicy struct {
	Model          string
	Strategy       string
	Sticky         string
	Temperature    float64
	Explore        float64
	PriorityWeight float64
	SuccessWeight  float64
	LatencyWeight  float64
	WindowSeconds  int64
	MinSamples     int
	UpdatedAt      int64
}

// DefaultRoutePolicy 返回内置默认策略。
func DefaultRoutePolicy() RoutePolicy {
	return RoutePolicy{
		Strategy:       RouteStrategyPriority,
		Sticky:         RouteStickyPrefer,
		Temperature:    0.1,
		Explore:        0.02,
		PriorityWeight: 0.4,
		SuccessWeight:  0.5,
		LatencyWeight:  0.1,
		WindowSeconds:  RouteDefaultWindowSeconds,
		MinSamples:     20,
	}
}

// ValidRouteStrategy 判断策略名是否可用。
func ValidRouteStrategy(v string) bool {
	switch v {
	case RouteStrategyPriority, RouteStrategyRotate, RouteStrategyWeighted,
		RouteStrategySuccess, RouteStrategySoftmax:
		return true
	}
	return false
}

// ValidRouteSticky 判断会话粘性取值是否可用。
func ValidRouteSticky(v string) bool {
	switch v {
	case RouteStickyPrefer, RouteStickyOff, RouteStickyStrict:
		return true
	}
	return false
}

// ValidateRoutePolicy 校验整条策略。CLI 与 store 写入口共用它，避免两套范围。
func ValidateRoutePolicy(p RoutePolicy) error {
	if !ValidRouteStrategy(p.Strategy) {
		return fmt.Errorf("不支持的选号策略 %q，可用 priority、rotate、weighted、success、softmax", p.Strategy)
	}
	if !ValidRouteSticky(p.Sticky) {
		return fmt.Errorf("不支持的会话粘性 %q，可用 prefer、off、strict", p.Sticky)
	}
	if p.Temperature <= 0 || p.Temperature > 1 || math.IsNaN(p.Temperature) {
		return fmt.Errorf("温度必须在 (0, 1] 之间，当前是 %v", p.Temperature)
	}
	if p.Explore < 0 || p.Explore > 1 || math.IsNaN(p.Explore) {
		return fmt.Errorf("探索比例必须在 0 到 1 之间，当前是 %v", p.Explore)
	}
	for name, v := range map[string]float64{
		"优先级权重": p.PriorityWeight,
		"成功率权重": p.SuccessWeight,
		"延迟权重":  p.LatencyWeight,
	} {
		if v < 0 || v > 1 || math.IsNaN(v) {
			return fmt.Errorf("%s必须在 0 到 1 之间，当前是 %v", name, v)
		}
	}
	if p.PriorityWeight+p.SuccessWeight+p.LatencyWeight <= 0 {
		return fmt.Errorf("三项评分权重不能全为 0：softmax 至少要有一个打分维度")
	}
	if p.WindowSeconds <= 0 || p.WindowSeconds > RouteMaxWindowSeconds {
		return fmt.Errorf("统计窗口必须在 1 秒到 %d 秒之间，当前是 %d", RouteMaxWindowSeconds, p.WindowSeconds)
	}
	if p.MinSamples < 1 {
		return fmt.Errorf("最小样本至少是 1，当前是 %d", p.MinSamples)
	}
	return nil
}

func validateRouteModelName(model string) error {
	for _, r := range model {
		if unicode.IsControl(r) {
			return fmt.Errorf("模型名 %q 里有控制字符：模型名会原样进请求与表格，只能写可见字符", model)
		}
	}
	return nil
}

// ResolveRoutePolicy 返回模型最终生效的策略与来源：model、default 或 builtin。
func (s *Store) ResolveRoutePolicy(model string) (RoutePolicy, string, error) {
	model = strings.TrimSpace(model)
	if err := validateRouteModelName(model); err != nil {
		return RoutePolicy{}, "", err
	}
	var rows []routePolicyRow
	if err := s.gorm.Where("model = ? OR model = ?", model, "").Find(&rows).Error; err != nil {
		return RoutePolicy{}, "", err
	}
	var def, exact *routePolicyRow
	for i := range rows {
		row := &rows[i]
		if row.Model == "" {
			def = row
		} else if row.Model == model {
			exact = row
		}
	}
	if exact != nil {
		return routePolicyFromRow(*exact), "model", nil
	}
	if model == "" {
		if def != nil {
			return routePolicyFromRow(*def), "default", nil
		}
		return DefaultRoutePolicy(), "builtin", nil
	}
	if def != nil {
		return routePolicyFromRow(*def), "default", nil
	}
	return DefaultRoutePolicy(), "builtin", nil
}

// RouteSnapshot 是一次选号要读的全部库内事实：生效策略、账号权重与窗口统计。
// 三个读取集中在这里，网关、CLI 与 explain 不各拼一份取数顺序。
type RouteSnapshot struct {
	Policy       RoutePolicy
	PolicySource string
	Weights      map[int64]int
	Stats        map[int64]RouteStat
}

// RouteSnapshot 按模型读取选号快照。now 决定统计窗口的右端，测试因此不依赖真实时钟。
func (s *Store) RouteSnapshot(model string, now time.Time) (RouteSnapshot, error) {
	policy, source, err := s.ResolveRoutePolicy(model)
	if err != nil {
		return RouteSnapshot{}, err
	}
	weights, err := s.RouteWeights(model)
	if err != nil {
		return RouteSnapshot{}, err
	}
	since := now.Unix() - policy.WindowSeconds
	stats, err := s.RouteStats(model, since)
	if err != nil {
		return RouteSnapshot{}, err
	}
	return RouteSnapshot{Policy: policy, PolicySource: source, Weights: weights, Stats: stats}, nil
}

// ListRoutePolicies 返回落库的默认行与模型覆盖行；空列表表示只有内置默认。
func (s *Store) ListRoutePolicies() ([]RoutePolicy, error) {
	var rows []routePolicyRow
	if err := s.gorm.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RoutePolicy, 0, len(rows))
	for _, row := range rows {
		out = append(out, routePolicyFromRow(row))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Model == out[j].Model {
			return false
		}
		if out[i].Model == "" {
			return true
		}
		if out[j].Model == "" {
			return false
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// SetRoutePolicy 覆盖写一条策略；Model 为空串写默认行。
func (s *Store) SetRoutePolicy(p RoutePolicy) error {
	p.Model = strings.TrimSpace(p.Model)
	if err := validateRouteModelName(p.Model); err != nil {
		return err
	}
	if err := ValidateRoutePolicy(p); err != nil {
		return err
	}
	p.UpdatedAt = time.Now().Unix()
	row := routePolicyRowFrom(p)
	return s.gorm.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "model"}},
		DoUpdates: clause.AssignmentColumns([]string{"strategy", "sticky", "temperature", "explore", "priority_weight", "success_weight", "latency_weight", "window_seconds", "min_samples", "updated_at"}),
	}).Create(&row).Error
}

// DeleteRoutePolicy 删除一条策略，返回是否真的删到了行。
func (s *Store) DeleteRoutePolicy(model string) (bool, error) {
	model = strings.TrimSpace(model)
	if err := validateRouteModelName(model); err != nil {
		return false, err
	}
	res := s.gorm.Where("model = ?", model).Delete(&routePolicyRow{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func routePolicyFromRow(r routePolicyRow) RoutePolicy {
	return RoutePolicy{
		Model:          r.Model,
		Strategy:       r.Strategy,
		Sticky:         r.Sticky,
		Temperature:    r.Temperature,
		Explore:        r.Explore,
		PriorityWeight: r.PriorityWeight,
		SuccessWeight:  r.SuccessWeight,
		LatencyWeight:  r.LatencyWeight,
		WindowSeconds:  r.WindowSeconds,
		MinSamples:     r.MinSamples,
		UpdatedAt:      r.UpdatedAt,
	}
}

func routePolicyRowFrom(p RoutePolicy) routePolicyRow {
	return routePolicyRow{
		Model:          p.Model,
		Strategy:       p.Strategy,
		Sticky:         p.Sticky,
		Temperature:    p.Temperature,
		Explore:        p.Explore,
		PriorityWeight: p.PriorityWeight,
		SuccessWeight:  p.SuccessWeight,
		LatencyWeight:  p.LatencyWeight,
		WindowSeconds:  p.WindowSeconds,
		MinSamples:     p.MinSamples,
		UpdatedAt:      p.UpdatedAt,
	}
}

// RouteAccountWeight 是一行账号权重。Model 为空串表示账号默认。
type RouteAccountWeight struct {
	AccountID int64
	Model     string
	Weight    int
	UpdatedAt int64
}

// ValidateRouteWeight 校验账号权重范围。
func ValidateRouteWeight(weight int) error {
	if weight < 1 || weight > 1000 {
		return fmt.Errorf("账号权重必须在 1 到 1000 之间，当前是 %d；要停用账号请用 lax account disable", weight)
	}
	return nil
}

// RouteWeights 返回一个模型最终生效的账号权重：账号-模型覆盖优先，其次账号默认，最后 1。
func (s *Store) RouteWeights(model string) (map[int64]int, error) {
	model = strings.TrimSpace(model)
	if err := validateRouteModelName(model); err != nil {
		return nil, err
	}
	var rows []routeAccountWeightRow
	if err := s.gorm.Where("model = ? OR model = ?", "", model).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int64]int)
	for _, row := range rows {
		if row.Model == "" {
			out[row.AccountID] = row.Weight
		}
	}
	for _, row := range rows {
		if row.Model == model && model != "" {
			out[row.AccountID] = row.Weight
		}
	}
	return out, nil
}

// ListRouteAccountWeights 返回全部显式权重行，按账号 ID 与模型名排序。
func (s *Store) ListRouteAccountWeights() ([]RouteAccountWeight, error) {
	var rows []routeAccountWeightRow
	if err := s.gorm.Order("account_id ASC").Order("model ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RouteAccountWeight, 0, len(rows))
	for _, row := range rows {
		out = append(out, RouteAccountWeight{
			AccountID: row.AccountID,
			Model:     row.Model,
			Weight:    row.Weight,
			UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// SetRouteAccountWeight 覆盖写一个账号权重。
func (s *Store) SetRouteAccountWeight(accountID int64, model string, weight int) error {
	model = strings.TrimSpace(model)
	if err := validateRouteModelName(model); err != nil {
		return err
	}
	if err := ValidateRouteWeight(weight); err != nil {
		return err
	}
	row := routeAccountWeightRow{AccountID: accountID, Model: model, Weight: weight, UpdatedAt: time.Now().Unix()}
	return s.gorm.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "model"}},
		DoUpdates: clause.AssignmentColumns([]string{"weight", "updated_at"}),
	}).Create(&row).Error
}

// DeleteRouteAccountWeight 删除一个账号权重，返回是否真的删到了行。
func (s *Store) DeleteRouteAccountWeight(accountID int64, model string) (bool, error) {
	model = strings.TrimSpace(model)
	if err := validateRouteModelName(model); err != nil {
		return false, err
	}
	res := s.gorm.Where("account_id = ? AND model = ?", accountID, model).Delete(&routeAccountWeightRow{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// RouteStat 是一个账号在一个模型上的统计汇总。
type RouteStat struct {
	AccountID    int64
	Attempts     int64
	Successes    int64
	LatencySumMS int64
	LatencyCount int64
}

// RouteStats 汇总一个模型在 [since, now] 内的分钟桶。since 为 0 表示全部。
func (s *Store) RouteStats(model string, since int64) (map[int64]RouteStat, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return map[int64]RouteStat{}, nil
	}
	q := s.gorm.Table("route_stats").Select(`account_id,
		COALESCE(SUM(attempts), 0) AS attempts,
		COALESCE(SUM(successes), 0) AS successes,
		COALESCE(SUM(latency_sum_ms), 0) AS latency_sum_ms,
		COALESCE(SUM(latency_count), 0) AS latency_count`).
		Where("model = ?", model)
	if since > 0 {
		q = q.Where("bucket >= ?", since)
	}
	var rows []RouteStat
	if err := q.Group("account_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int64]RouteStat, len(rows))
	for _, row := range rows {
		out[row.AccountID] = row
	}
	return out, nil
}

// PruneRouteStats 删除 before 之前的分钟桶，返回删除行数。
func (s *Store) PruneRouteStats(before int64) (int64, error) {
	if before <= 0 {
		return 0, nil
	}
	res := s.gorm.Where("bucket < ?", before).Delete(&routeStatRow{})
	return res.RowsAffected, res.Error
}

// upsertRouteStatAtTx 用调用方的尝试时间定分钟桶，窗口统计与请求记录因此对齐。
func upsertRouteStatAtTx(tx *gorm.DB, accountID int64, model string, ts int64, latencyMS int64, success bool) error {
	if accountID == 0 || strings.TrimSpace(model) == "" {
		return nil
	}
	bucket := ts
	if bucket <= 0 {
		bucket = time.Now().Unix()
	}
	bucket -= bucket % 60
	updates := map[string]any{"attempts": gorm.Expr("attempts + 1")}
	row := routeStatRow{AccountID: accountID, Model: model, Bucket: bucket, Attempts: 1}
	if success {
		updates["successes"] = gorm.Expr("successes + 1")
		updates["latency_sum_ms"] = gorm.Expr("latency_sum_ms + ?", latencyMS)
		updates["latency_count"] = gorm.Expr("latency_count + 1")
		row.Successes = 1
		row.LatencySumMS = latencyMS
		row.LatencyCount = 1
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "model"}, {Name: "bucket"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error
}
