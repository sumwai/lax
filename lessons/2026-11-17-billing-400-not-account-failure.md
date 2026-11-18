# A billing 400 is a failed account, not a client error

## Found

2026-11-17, from a production incident on the `acct-a` account (id 7, provider
`acme`).

## Symptom

Between 22:07:03 and 22:07:41 the account received 16 requests and the upstream returned
HTTP 400 every time:

```
You have insufficient credits to make this request. Please purchase more credits to continue using the service.
```

The gateway treated 400 as a malformed client request. It did not retry, did not cool the
account or the account-model down, did not disable the account, and did not write a
`route_stats` sample. The same sticky session stayed pinned to `acct-a` and kept producing
400s, while `acct-b` was serving 200s at the same time. The quota snapshot for `acct-a` was
stale, but the stale snapshot was not the cause: the account really had no credits left.
The account was disabled manually after the incident.

## Why it was not noticed earlier

The failure-transfer rules were built around the status code alone. 401 disabled an
account; 429, 408, 5xx and transport failures moved to the next candidate; other 4xx were
returned to the client because they usually mean the request itself is wrong. A 400 whose
body names a billing problem is the exception, and nothing looked at the body.

Three details kept the incident quiet:

- 400 was deliberately excluded from `route_stats`, so the failure never lowered the
  account's success rate or appeared in route samples.
- Sticky sessions hash over account IDs, not health, so the session kept choosing `acct-a`
  even after another account had recovered.
- Speech probes treat 400 and 422 as "endpoint reachable, credentials unverified", so a
  billing 400 there would not have changed account state either.

The result was an account that failed every request while every dashboard looked normal.

## Decision

Failure handling became data: `lax route policy` stores rules matched on account, provider,
model, status and scrubbed error text. A match decides retry count, account or
account-model cooldown, permanent disable and whether the request moves to the next
candidate. Rules are read per request, so a change applies without a restart.

The old behavior is preserved as built-in fallbacks rather than hard-coded branches:
401 disables and returns, 402 disables and switches, and a 400/403 whose text contains a
narrow billing keyword (for example `insufficient credit`, `no credits`,
`payment required`, `insufficient_quota`, `余额不足`, `欠费`, `额度不足`) disables and
switches. User rules run first and override the built-ins. Bare `credit` and bare `quota`
are deliberately not keywords, because they match unrelated messages.

`account test` and speech expected-error probes run the same rules before turning a status
into a verdict. A billing 400 that matches a disable or cooldown rule can no longer mark
the account usable.

## Cost and tradeoffs

- Retrying a 5xx can duplicate an upstream-side effect. The rule is opt-in, retries are
  capped at 10, waits are capped at 8s each and 60s per candidate, and a response that
  already wrote bytes to the client is never retried.
- A 429 must not be retried immediately. The same exponential backoff applies, so a retry
  storm cannot replace the old fail-fast behavior.
- Account-wide cooldown is broader than the historical account-model cooldown. A rule with
  `--model` keeps the narrow scope; only a rule without `--model` cools the whole account.
- Built-ins are code, not rows. They still have to be maintained with the code, but they
  cannot be deleted by accident and they keep working when the policy table cannot be read.

## Verification

The feature's suite covers the layers separately: store policy CRUD, JSON round-trip,
ordering and validation; account matching, priority, built-in fallback, retry delay math,
account cooldown and `Rank` exclusion; gateway billing-400 disable/switch, account and
model cooldown, retry counts, 401/402/403 fallbacks and the already-written-bytes case;
probe billing 400 for chat and speech; and the `route policy` command surface including
patch semantics, `--all-errors` and JSON output. `make check` is the gate.

## Fix

Status: fixed. Commit hash: `db2b9b34c33f64a755e6588423717683b64657f9`.
