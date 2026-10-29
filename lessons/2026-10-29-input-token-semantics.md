# "Input tokens" does not mean the same thing in every protocol

Found: 2026-10-29, while making the cache token counts visible in `usage`.
Fixed: `8700e1c`.

## Symptom

Cost was `in_tokens * price_in + out_tokens * price_out`. Cache tokens were recorded but never
priced, which looked like a deliberate simplification. It was not: the three upstream protocols use
different definitions of the input count, so the same formula was wrong in two opposite directions.

- anthropic reports `input_tokens` with cache reads and cache writes already excluded. Cache reads
  therefore cost nothing at all, although they are billed at a discount, not free.
- openai-chat reports `prompt_tokens` including the cached part (`prompt_tokens_details.cached_tokens`
  is a subset), and gemini does the same with `promptTokenCount` containing
  `cachedContentTokenCount`. A cache hit was charged at the full input price.

Both errors are silent: the numbers look plausible and the totals stay monotonic, so nobody
questions them until they compare a bill.

## Why it was missed

The usage field mapping was written per protocol and tested per protocol, but the cost formula was
written once, against one mental model of "input tokens". No test compared a cost against a
hand-computed number across two protocols with cache hits.

## Decision

Normalise at write time rather than at read time: `requests.in_tokens_uncached` stores the input
count that is actually billed at the input price — `in_tokens` for anthropic, `max(in_tokens -
cache_read, 0)` for the two openai protocols and gemini. `in_tokens` keeps the upstream number, so
what `log tail` and `usage` display does not change. Accounts gain `price_cache_read` and
`price_cache_write`, and cost becomes `uncached * price_in + cache_read * price_cache_read +
cache_write * price_cache_write + out * price_out`, with the cache prices falling back to the input
price when unset.

Cost: one more stored column and a migration that backfills it with the old `in_tokens`, which is the
best available guess for historical rows; a protocol whose definition changes in future needs the
normalisation rule revisited, and the wrong answer would now be written to disk instead of computed
on read.

## How to verify

Tests cover all three outbound protocols plus the degenerate case (`cache_read` larger than
`in_tokens`, which clamps to zero), assert that a history without cache prices produces the same
cost as the old formula, and clamp a live run: a chat response reporting `prompt_tokens 3000` with
`cached_tokens 1000` and prices 2 / 8 / 0.2 / 2.5 per million yields 0.0087.
