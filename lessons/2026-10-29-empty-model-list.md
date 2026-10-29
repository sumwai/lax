# An empty model list must not mean "serves every model"

Found: 2026-10-29, from a live failure while running `serve`.
Fixed: `30421bf`.

## Symptom

`switchboard serve` routed a request for `deepseek/deepseek-v4.1-flash` (a Command Code model
name) to an account on the official DeepSeek endpoint, which answered:

```
The supported API model names are deepseek-flash, deepseek-v4-pro, but you passed deepseek/deepseek-v4.1-flash.
```

Two accounts in the pool had a synced but not yet probed model list, so their `accounts.models`
was still empty.

## Why it was easy to miss

`account.Supports` treated an empty list as "no restriction", a leftover from the era when model
lists were typed by hand and an account without `--models` was meant to accept anything. Once
`model sync` and `model probe` became the normal path, an empty list means "nothing usable was
found", not "anything goes". The lenient rule silently widened the candidate set to every model
name in every protocol namespace.

## Decision

An empty model list means the account provides no models: `Supports` returns false. When no
account provides a requested model the gateway answers 404 with an actionable message, and 503 is
kept for the different case where accounts exist but are all disabled or cooling down. Accounts
with zero usable models can still be probed explicitly with `account test` and `model probe`.

## How to verify

`internal/account` tests pin that an empty list never matches a model. Gateway tests assert 404
for an unknown model name across all three inbound paths and 503 when the providers exist but no
account is currently available.
