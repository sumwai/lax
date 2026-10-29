# Request-side reasoning content breaks reasoning upstreams

Found: 2026-10-29, during the read-only review of `b4d690a` (thinking content carried across
protocols).
Fix: `8a54abd`, which stopped writing reasoning content back on the request side of both
`openai-chat` and `openai-responses` encoders.

## Symptom

A Claude Code session with thinking enabled sends signed `thinking` blocks back in the message
history. When that request is routed to an account whose outbound protocol is `openai-chat`, the
translator wrote those blocks into `assistant.reasoning_content`. Reasoning upstreams such as
DeepSeek reject a request that echoes `reasoning_content` and answer 400, so the whole request
failed even though the history was well formed.

## Why it was easy to miss

The response direction needs the field: reasoning models emit `reasoning_content` and clients
expect to see it. Treating the request direction symmetrically looked natural, and the existing
round-trip tests encoded thinking on both directions, so the tests passed while the behaviour was
wrong for at least one real vendor.

## Decision

Reasoning content travels in the response direction only. Decoding a request drops these blocks
instead of writing them back into `reasoning_content` or a reasoning item. The same rule applies
to the `openai-responses` input path.

The trade-off is recorded in `DESIGN.md` section 5.4: a conversation that started on one upstream
does not carry its reasoning traces into the next request, which is what upstreams expect anyway.

## How to verify

A conversion test decodes a request containing assistant thinking content, re-encodes it for
`openai-chat`, and asserts the upstream body has no `reasoning_content` on any assistant message,
while the response direction still maps reasoning content into the client protocol.
