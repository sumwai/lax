# A transport failure must not decide whether a model is reachable

Found: 2026-10-31, during `model probe` against a proxy that intermittently timed out during the TLS
handshake. Rows that had been `allowed` were rewritten as `error`, the next `decide` call found no
candidate account, and the only way back was editing `account_models` by hand.
Fix: `f1f3c37` (verdict rules and transport retry in `internal/gateway/models.go`) and `c738ec4` (CLI skips the write for undecided rows).

## Symptom

A probe run mixed two kinds of rows. Most models kept the verdict from earlier runs, but a few that
had been confirmed `allowed` came back as `error` with a message like `上游请求失败: net/http: TLS
handshake timeout`. No HTTP response had been received for those rows: the failure was on the
connection, not an upstream statement about the model.

The consequence was worse than one bad row. `store.AllowedModels` only returns `access='allowed'`,
so a single timeout removed the model from the candidate set. `lax model decide` then reported that the
model was in no account's list, which reads like a configuration problem rather than a network
hiccup.

## Why it was easy to miss

The verdict switch in `probeOnce` had a default branch that mapped every non-2xx outcome to
`store.AccessError`. That branch was shared by 5xx responses, rate limits, generic 400s and
connection failures, so a transport error was indistinguishable from an upstream verdict once it
left the gateway.

The tests did not catch it either. `TestProbeModelStatuses` fed HTTP statuses and asserted the
mapped access value; the only network-error test asserted that the request row recorded
`http_status=0` and no tokens, not what happened to the model row. Nothing asserted that the
previous `access` survived a probe with no verdict, and the CLI wrote whatever the gateway returned
without asking whether a verdict existed at all.

## Decision

Only a statement about the model changes reachability:

- `2xx` → `allowed`; `403` → `forbidden`; `404` and a `400` whose text says the model does not exist
  → `missing`;
- `401` → no write (account-level conclusion, account is disabled);
- connection failures, `429`, `5xx`, other `400`s and local encoding/build failures → no write at
  all. `ModelProbe.Undecided` is set so the caller can tell "no verdict" from "empty verdict", and
  `access`, `http_status`, `error` and `checked_at` keep their previous values.

Connection failures are retried twice within the same protocol (three calls total, no backoff)
before being treated as undecided, because TLS and read timeouts on a local proxy are usually
transient. Each retry writes its own `requests` row with `http_status=0` and an error prefix that
marks it as a non-verdict, so the accounting still matches the calls actually made.

The cost is deliberate: a model that is genuinely broken with a persistent 5xx stays selectable
until an explicit 4xx or a successful call produces a verdict. The request path already handles
that case through failover and the transport cooldown, and leaving `checked_at` untouched means the
next probe retries instead of skipping the row as fresh.

## How to verify

`internal/gateway/probe_transport_test.go` drives a fake upstream that either drops the connection
or answers:

| Test | Upstream | Asserted |
| --- | --- | --- |
| `TestProbeModelTransportRetrySucceeds` | closes the first two connections, then 200 | three calls, `allowed`, two retry rows plus one verdict row |
| `TestProbeModelTransportFailureKeepsAccess` | closes every connection | `Undecided`, empty `Access`, stored `allowed` and `checked_at` unchanged |

`TestProbeModelStatuses` pins the three-way mapping (`Access`, legacy `Status`, `Undecided`) for
2xx, 403, 404, model-not-found 400, generic 400 and 500. `TestModelProbeUndecidedKeepsAllowed` in
`internal/cli` checks the same guarantee through the command: after a 500 the row stays `allowed`
and the summary line reports `未判定 1`.
