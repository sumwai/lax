# Follow mode must page by cursor, and every filter must survive into the loop

## Found

2026-11-01, while adding shared time ranges to `usage`, `log tail`, and `plugin logs`.

## Symptom

`log tail --follow --from 11:00` filtered the initial query but printed every later insertion,
including rows older than 11:00. `plugin logs --follow` could skip rows when more than
`pluginFollowFetch` logs arrived between polls: each tick fetched the newest N rows in reverse
order, filtered them by ID, and advanced only to the newest one. The older new rows fell outside
the next window and were never returned.

## Why it was not noticed earlier

Tests covered one new follow row, and ordinary plugin traffic stays below the fetch window. The
initial range query also looked correct, so the missing lower bound only appeared when the follow
loop was exercised with a range. The burst loss needs more than one fetch window of new rows in a
single poll interval, which normal traffic does not produce.

## Decision

Follow reads by ascending ID after a cursor. `store.RequestsAfter` and `store.PluginLogsAfter` take
both `since` and `until` bounds; the CLI drains batches until a short batch and advances the cursor
to the last returned row. Initial reads still use the recent-first query because that is the order
humans expect. The cost is that one poll tick may issue several queries during a burst; correctness
wins over the extra reads, and `--to` still stops the loop.

## Verification

`internal/store` covers `RequestsAfter` ordering plus its lower bound and
`PluginLogsAfter` cursor paging. `internal/cli` covers lightweight `--from`/`--to` ranges for
`usage`, `log tail`, and `plugin logs`, the single-row follow case, and the `--since` conflict.

## Fix

Fixed in `ad03c2b`, which moved both follow loops to ascending cursor reads and carried the
`since` bound into every poll.
