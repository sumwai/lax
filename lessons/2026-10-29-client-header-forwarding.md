# A gateway that rebuilds upstream headers must forward the client's feature headers

Found: 2026-10-29, answering whether client headers reach the upstream.
Fixed: `f86972e`.

## Symptom

Upstream requests were built from scratch: `Content-Type`, `Accept` for streaming, the credential
header chosen by the outbound protocol, and any declared provider headers. Nothing from the inbound
request was copied. That kept client credentials out of the upstream, but it also dropped
`anthropic-beta`, so a client that asked for the 1M context window, interleaved thinking or prompt
caching got a plain request and no error. The same request straight to the vendor worked, which made
the gateway look like it silently ignored the feature. `anthropic-version` was pinned to
`2023-06-01` regardless of what the client asked for, and OpenAI's `openai-organization` and
`openai-project` were dropped, so a request could be billed to the wrong organization.

## Why it was missed

The fake upstreams in the suite asserted paths, bodies and the credential header, never the absence
or presence of feature headers. Forwarding nothing looked like the safe default, and "safe default"
was never written down as a decision with its cost.

## Decision

Forward a whitelist, and only when the inbound and outbound protocol match: `anthropic-beta` and
`anthropic-version` for anthropic, `openai-beta`, `openai-organization` and `openai-project` for the
two OpenAI protocols, nothing for gemini because its switches live in the query string and body.
Credentials, cookies, hop-by-hop headers and the gateway's own `x-switchboard-session` are never
forwarded. Order is `setAuth` defaults, then the client's whitelisted headers, then declared
provider headers, so site configuration still overrides whatever the client sends.

Cost: a client can now turn on upstream behaviour that the gateway does not model, so a beta header
whose semantics change the response shape reaches the client unfiltered; and a header over 8KB is
dropped with a log line rather than rejected.

## How to verify

Tests assert both directions: a matching pair carries both `anthropic-beta` values in order plus the
client's `anthropic-version`, an account credential replaces any client-supplied `Authorization` or
`x-api-key`, cookies never appear upstream, and a cross-protocol request carries none of these
headers. A live check with a header-echoing upstream reproduces the same table.
