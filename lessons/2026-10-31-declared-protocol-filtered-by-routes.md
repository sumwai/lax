# A model-level protocol declaration outranks the provider route list

Found: 2026-10-31, during `model probe` on a model whose catalog row declared `systemone`. The probe
still sent an anthropic request and a chat request first, collected a misleading `400`, and only
then tried the declared protocol. Two billed calls per model were spent on paths the catalog had
already ruled out.
Fix: `f979d9d` (declaration write path), `f1f3c37` (candidate ordering and single-protocol
probe) and `c738ec4` (the `model add` and `model probe --protocol` commands).

## Symptom

`provider_models.protocols` for the model contained `systemone`, but the provider had no
`provider_routes` row for that protocol: the decision endpoint was reachable at the fixed
`/v1/systemone` path derived from `base_url`, not through a declared route.

`routeProtocolOrder` collected the catalog declaration but then filtered it against the candidate
list, which was built from the provider's declared routes. `systemone` was dropped before the loop,
so the probe started from the provider default (`openai-chat`) and its other route (`anthropic`).
The first two calls failed with protocol-shaped `400`s, and only the fallback attempt reached the
declared path.

## Why it was easy to miss

The filter had a reasonable premise: a protocol with no declared route has no known address, and
guessing one is how a probe ends up hammering an endpoint that does not exist. That premise holds
for arbitrary route paths, but not for protocols whose path `gateway.Endpoint` derives from the
protocol name, and not for a model-level declaration that already says "this model speaks this
protocol on this base URL".

The existing test suite encoded the filter as intended behavior:
`TestProbeDeclaredProtocolOutsideRoutesSkipped` asserted that a declared protocol outside the route
list was not tried. There was no case where the declaration was correct and the route table was
incomplete, which is exactly the decision-model setup.

## Decision

Catalog declarations are added to the candidate order regardless of `provider_routes`:

- discovered protocol (including the account-level override written by `model add --protocol`)
  stays first, then catalog declarations, then the provider's effective default protocol, then the
  route list;
- a declaration placed before the primary protocol only accepts `2xx` as a conclusion. Its failures
  fall through to the remaining candidates, and when every candidate fails the verdict is still the
  provider-default attempt, so a wrong declaration costs calls but cannot produce a wrong verdict.

Two command-level changes make the fast path the normal one:

- `lax model add <model> --account <account> --protocol <protocol>` writes the protocol into both
  `provider_models.protocols` and `account_models.protocol`, so the discovered slot already points
  at it and the probe starts there;
- `lax model probe --protocol <protocol>` narrows the candidates to that single protocol, for the
  case where only one path needs verification.

## How to verify

`internal/gateway/probe_declared_protocol_test.go`:

| Test | Setup | Asserted |
| --- | --- | --- |
| `TestProbeModelDeclaredProtocolOutsideRoutes` | provider has only a chat route, catalog declares `systemone` | first and only call is `POST /v1/systemone`, verdict `allowed/systemone` |
| `TestProbeModelDeclaredProtocolOutsideRoutesFailsKeepsDefault` | same, but `systemone` answers 404 | two calls, verdict comes from the provider default |
| `TestProbeModelExplicitProtocolOnly` | anthropic and chat routes declared | `--protocol systemone` calls only `/v1/systemone` and records the discovered protocol |
| `TestProbeModelExplicitProtocolRejectsUnknown` | unknown protocol | error, no upstream call, `Undecided` |

`TestModelAddWritesBothTables` and `TestModelProbeProtocolOnly` in `internal/cli` cover the command
side: the declaration reaches both tables and the probe with `--protocol` touches only the declared
path.
