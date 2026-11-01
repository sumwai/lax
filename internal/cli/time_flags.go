package cli

import (
	"strings"
	"time"

	"lax/internal/timerange"
)

// timeRangeFlags 是 usage、log tail 与 plugin logs 共用的三个时间参数。
// defaultSince 为空表示默认不设时间范围；非空时等价于把这个值写进 --since。
type timeRangeFlags struct {
	since        *string
	from         *string
	to           *string
	defaultSince string
}

type timeRange struct {
	Since int64
	Until int64
	Text  string
}

func addTimeRangeFlags(fs *flagSet, defaultSince string) *timeRangeFlags {
	return &timeRangeFlags{
		since:        fs.String("since", defaultSince, "相对时间范围：30s、15m、24h、7d、3w；也收 11:00、-1d 11:00 与绝对时间（与 --from/--to 互斥）"),
		from:         fs.String("from", "", "起始时间：11:00、-1d 11:00、RFC3339、YYYY-MM-DD HH:MM[:SS] 或 YYYY-MM-DD（本地时区）"),
		to:           fs.String("to", "", "结束时间（不含）：写法同 --from；只写日期或 -1d 时包含当天"),
		defaultSince: defaultSince,
	}
}

// parse 把三个参数解成一个 [Since, Until) 区间。since 显式出现时与 from/to 互斥；
// 只有 from 时上界不限，只有 to 时下界不限。解析失败与冲突都是用法错误。
func (f *timeRangeFlags) parse(fs *flagSet, now time.Time) (timeRange, error) {
	from := strings.TrimSpace(*f.from)
	to := strings.TrimSpace(*f.to)
	if fs.Changed("since") && (from != "" || to != "") {
		return timeRange{}, usagef("--since 与 --from/--to 不能一起用：相对范围写 --since，绝对范围写 --from 与 --to")
	}
	if from != "" || to != "" {
		var out timeRange
		if from != "" {
			ts, err := timerange.ParseFrom(from, now, time.Local)
			if err != nil {
				return timeRange{}, usagef("--from：%v", err)
			}
			out.Since = ts
		}
		if to != "" {
			ts, err := timerange.ParseTo(to, now, time.Local)
			if err != nil {
				return timeRange{}, usagef("--to：%v", err)
			}
			out.Until = ts
		}
		if from != "" && to != "" && out.Since >= out.Until {
			return timeRange{}, usagef("--from 必须早于 --to")
		}
		out.Text = rangeText(from, to)
		return out, nil
	}

	since := strings.TrimSpace(*f.since)
	if since == "" {
		since = f.defaultSince
	}
	if since == "" {
		return timeRange{}, nil
	}
	ts, err := timerange.ParseSince(since, now)
	if err != nil {
		return timeRange{}, usagef("--since：%v", err)
	}
	return timeRange{Since: ts, Text: "最近 " + since}, nil
}

// rangeText 生成空结果与提示里给人看的时间范围说明。
func rangeText(from, to string) string {
	switch {
	case from != "" && to != "":
		return from + " 至 " + to
	case from != "":
		return "从 " + from + " 起"
	default:
		return "截至 " + to
	}
}
