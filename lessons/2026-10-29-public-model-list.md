# A public model list proves nothing about credentials

Found: 2026-10-29, testing against real upstreams with a deliberately invalid key.
Fixed: `2ed8c7c`.

## Symptom

`lax account test` judged an account by `GET /v1/models` first, on the reasoning that a
model list costs no tokens and exercises credentials. Against commandcode, the list endpoint is
public: it answers 200 with 94 models for any key, including a key that the completions endpoint
rejects with `401 Invalid 'Authorization' header or token`. The command therefore printed `正常`
for an account that could not serve a single request.

## Why it was missed

Every fake upstream in the test suite required a credential header before answering the list, so
"list returned 200" always implied "the key was accepted". The public-list case never appeared in
the fixtures, and the design note treated a successful list as evidence rather than as a check of
reachability.

## Decision

The list still runs first, but only as evidence about the address and about how many models the
upstream advertises. Credentials are proven by one minimal call afterwards, and the verdict is the
call's. Both stages are reported, so a public list is visible in the output instead of being
mistaken for a credential check.

Cost: every `account test` now spends one request of at most a token or two even when the list
already succeeded, and a provider whose list works but whose chat endpoint needs an extra header
is reported as failing rather than as healthy.

## How to verify

`TestProbe*` cases assert that a list-only success sends a follow-up call, that a 401 on that call
is a failure with both stages in the message, and that a working key still reports success. A live
run against commandcode with a revoked key shows `失败 401` with `列表接口可用` beside it.
