# Messages that lose all their blocks must not be written as empty content

Found: 2026-10-29, in the read-only review of `8a54abd` (thinking content kept legal across
protocols), by tracing what happens to an assistant turn whose only block is thinking content
without a signature.
Fixed: `0a034b8`.

## Symptom

When an assistant turn consisted solely of thinking blocks that carried no signature — the shape
produced by a chat upstream's `reasoning_content` — the anthropic encoder dropped every block but
still emitted the message:

```json
{"role": "assistant", "content": []}
```

Anthropic-compatible upstreams reject an empty `content` array with 400, so a history that had
been perfectly usable on the chat side failed as soon as the same conversation was routed to an
anthropic account. Dropping the empty message also risks two consecutive messages with the same
role, which those upstreams reject for the same reason: the conversation must alternate.

## Why it was easy to miss

The rule "no signature means no thinking block" was implemented and tested on its own. The tests
asserted the block was absent from the encoded body, which passed while the enclosing message kept
living as an empty shell. Nothing exercised the case where dropping blocks empties the message.

## Decision

Encoding to anthropic skips messages that end up with zero blocks, and merges a skipped message's
neighbours when that would leave two messages with the same role, preserving block order. If every
message is dropped, the encoder returns an error instead of writing `messages: []`, because an
empty array only buys another 400 with no indication of the cause.

Cost: a turn with nothing but unsignable thinking disappears from the history. The alternative
would be to invent a signature or a placeholder block, and both change meaning rather than drop
detail.

## How to verify

Encoders tests assert no `content: []` appears, that the surrounding user messages merge with
their blocks in order, and that a fully dropped history returns an error. Signed thinking blocks
still round-trip unchanged.
