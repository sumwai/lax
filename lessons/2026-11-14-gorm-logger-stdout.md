# GORM's default logger writes record-not-found to stdout

## Found

2026-11-14, while checking the deployed `lax quota show <account>` output after v0.2.2.

## Symptom

Every `quota show` on an account without a `quota_settings` row printed a colored `record not found`
line and the raw SELECT to stdout before the table:

```
2026/11/15 04:19:24 lax/internal/store/quota.go:89 record not found
[0.073ms] [rows:0] SELECT * FROM `quota_settings` WHERE account_id = 7 ORDER BY ...
```

GORM's default logger writes to `os.Stdout`, so the same line could prefix `quota show --json` and
break a parser. A missing settings row is a normal branch: `QuotaSettings` falls back to the default
settings and reports `found=false`.

## Why it was not noticed earlier

`internal/cli` tests pass buffers into `cli.Run`, but GORM's default logger is created at package
init bound to the process `os.Stdout`. Capturing or replacing stdout inside the test harness does not
redirect it, so output assertions never saw the line. The human-output tests checked substrings, and
no test parsed `quota show --json` for an account without a settings row.

## Decision

`store.OpenWith` installs a logger that writes to `os.Stderr`, uses `logger.Warn`, and sets
`IgnoreRecordNotFoundError: true`. A missing optional row is not an error; real SQL errors still
reach stderr and the journal. The cost is that database errors no longer land on stdout, which is
where they never belonged. `--json` output stays clean even if a future warning is emitted.

## Verification

`go build ./...`, `go vet ./...`, and the focused `internal/cli` / `internal/store` tests pass. A
manual `quota show` against a copy of the production database produces no SQL lines, and
`quota show --json` starts with `{`.

## Fix

Fixed in `07a662c`, which added `newDBLogger` and switched `gorm.Open` to it.
