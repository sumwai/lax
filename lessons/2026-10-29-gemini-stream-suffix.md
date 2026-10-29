# A stream suffix rewrite must anchor to the end of the path

Found: 2026-10-29, in the read-only review of the gemini chain (`4ffa79a` through `ebc1e0c`).
Fixed: `323c522`.

## Symptom

To turn a non-streaming gemini request into a streaming one, the gateway rewrites
`:generateContent` to `:streamGenerateContent?alt=sse` in the resolved route path. The rewrite used
`strings.LastIndex` for `:generateContent`. When a route path did not end with that suffix — a
custom route such as `/v1beta/models/{model}` — a model name that itself contained
`:generateContent` became the match, and streaming requests were sent to
`/v1beta/models/foo:streamGenerateContent?alt=sse` while non-streaming requests went to
`/v1beta/models/foo:generateContent`. The same model therefore appeared to work in one mode and
fail in the other, and the upstream error named the path rather than the cause.

## Why it was easy to miss

`url.PathEscape` escapes spaces and slashes but leaves `:` alone, so a colon inside the model name
survives escaping and looks like the action suffix. The default route shape does end with
`:generateContent`, so every built-in provider was unaffected and the tests only covered that
shape.

## Decision

Match the suffix only at the end of the URL path, before any query string. If the route has no such
suffix, leave the path alone instead of inventing one; a route that wants streaming has to declare
the suffix, and the seeding for gemini does.

Cost: a custom gemini route without the suffix cannot stream through the rewrite path, and the
request fails with the upstream's own error instead of being silently redirected.

## How to verify

Tests cover three shapes: a route ending in the suffix, a route without it while the model name
contains `:generateContent`, and a model name containing the suffix on a route that has its own.
The first is rewritten, the other two are left untouched.
