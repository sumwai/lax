# Usage `--by day` tests fail in the first hour after local midnight

## Found

2026-11-20, from GitHub Actions `ci` run `44646756654` on `master` at commit
`361ec2322393628023c66462c1b20c480b268fa2` (running at `2026-11-20T00:34:07Z`).

## Symptom

Both `TestUsageCacheTotalsPerDimension` (`lax/internal/cli/observe_test.go`) and
`TestUsageReasoningTotalsPerDimension` (`lax/internal/cli/reasoning_test.go`) failed in CI
when asserting `--by day` output with `--since 1d`:

```
按日期应有 1 行，实际 2 行：map[2026-11-19:... 2026-11-20:...]
```

The tests seeded two request records at `now - 1h` (`t1`) and `now` (`t2`), expecting both
records to fall on the same day. Between `00:00:00` and `00:59:59` local time, `now - 1h`
lands on the previous calendar day, so `--by day` returned two buckets instead of one.

## Why it was not noticed earlier

`store.Store.Usage` groups `--by day` in local time via `dateExpr("r.ts")`. Local
development runs in `UTC+8`, where `00:00–01:00 UTC` is `08:00–09:00` local time, safely
inside the middle of the day. GitHub Actions runners run in `UTC`, so the `00:34:07Z`
run hit the first hour after midnight and split `now - 1h` and `now` across two dates.

## Decision

Keep both seeded timestamps in the same local calendar day while still remaining within
the `--since 1d` window:

```go
t1 := max(dayStart.Unix(), now.Add(-time.Hour).Unix())
t2 := now.Unix()
```

Here `dayStart` is local midnight (`time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())`).
During `00:00–00:59` local time, `t1` clamps to `dayStart.Unix()` (so both `t1` and `t2`
belong to today's local date, including at `00:00:00` where `t1 == t2 == dayStart.Unix()`
and on DST fallback days). At all other times of day, `t1` remains `now - 1h`.

## Cost and tradeoffs

- Choosing `max(dayStart.Unix(), now.Add(-time.Hour).Unix())` avoids mutating the process
  `TZ` environment variable or introducing a global `time.Now` mock across `internal/cli`
  and `internal/store`.
- When a test runs between `00:00` and `01:00` local time (especially right at `00:00:00`),
  `t1` and `t2` are closer together or equal, which is acceptable because these tests
  verify per-dimension aggregation totals rather than intra-day timestamp ordering.

## Verification

- `go test -v -run "TestUsageCacheTotalsPerDimension|TestUsageReasoningTotalsPerDimension" ./internal/cli/...` passed.
- `TZ=UTC` targeted run of `TestUsageCacheTotalsPerDimension` and `TestUsageReasoningTotalsPerDimension` passed.
- `go test ./internal/cli/...` passed.
- `make check` passed.

## Fix

Status: fix in progress (local working tree fix, not committed yet).
