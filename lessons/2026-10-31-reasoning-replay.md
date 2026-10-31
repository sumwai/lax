# Reasoning upstreams sometimes require their own CoT back

Found: 2026-10-31, from a reported failure where Claude Code on `deepseek-flash` answered a tool
call and then got `400 The reasoning_content in the thinking mode must be passed back to the API`.
Fix: `992f421`, which stamps a replay signature on thinking blocks coming from chat and responses
upstreams and writes those blocks back as `reasoning_content` when the request carries tools.

## Symptom

A Claude Code session with thinking enabled ran a tool, sent the tool result back, and the next
request failed upstream with that message. The same conversation without tools worked, and the
turn that produced the tool call worked. Only the follow-up carrying the tool result failed.

This contradicts `lessons/2026-10-29-request-side-reasoning.md`, which recorded that echoing
`reasoning_content` makes reasoning upstreams reject the request. Both observations are real; they
describe two different request shapes, and the earlier note generalised from one of them.

## What the upstream actually checks

DeepSeek's thinking-mode guide states the rule, and a scratch instance reproduces it:

> for requests carrying the `tools` parameter, the `reasoning_content` must be fully passed back to
> the API in all subsequent requests — even for turns where the model did not perform a tool call.

Probes against the real endpoint, all with `deepseek-flash`:

| Request shape | Result |
| --- | --- |
| tools present, history ends with a `user` text turn, tool-call turn has no `reasoning_content` | 200 |
| tools present, history ends with a `tool` result, tool-call turn has no `reasoning_content` | 400 |
| same, with the tool-call turn's `reasoning_content` filled in | 200 |
| same, with `reasoning_content: ""` | 200 |
| no tools, `reasoning_content` present | 200 (documented as ignored) |

The second row is the trap: the check only fires when the request ends on a tool result, which is
exactly the request that continues a tool loop, and never the one that starts it.

## Why the naive reproduction fails

Filling the same body with a tool-call id the endpoint issued moments earlier also returns 200,
with or without `reasoning_content`. The upstream recognises its own recent tool-call ids and
treats the reasoning as already known; any other id (a made-up `call_abc`, a DeepSeek-shaped
`call_00_…` it never issued, or an id from a different vendor after switching models mid-session)
falls back to the strict check and returns 400. Reproducing therefore has to use an id the
endpoint does not know, or wait out whatever window it keeps.

## Why the gateway had nothing to send

For an anthropic client on a chat upstream the CoT travelled one way only: the chat response
`reasoning_content` became an anthropic `thinking` block, and the encoder dropped it because it
had no `signature`, which anthropic clients require before they echo a thinking block. Whatever
the client did with the history, the gateway could not produce `reasoning_content` on the next
request, so the strict check always failed once its id cache no longer covered the turn.

## Decision

The gateway stamps its own signature on thinking blocks that come from a chat or responses
upstream, so the client keeps the CoT and hands it back, and the request encoder writes those
blocks back as `reasoning_content` when the request carries tools:

- the signature is `lax-replay:` plus the first 16 bytes of the SHA-256 of the text, base64url;
- blocks with that prefix are dropped when encoding to a real anthropic upstream, which only
  accepts its own signatures;
- blocks without it are still dropped on the way to chat and responses, so a client-supplied
  `reasoning_content` is never echoed back;
- `reasoning_content` is only written when the request carries `tools`, matching the documented
  condition and avoiding the field on upstreams that may reject it.

Costs, recorded in `DESIGN.md` section 7: the CoT is now visible to the client as a thinking
block, and a conversation that moves to a real anthropic account carries signatures anthropic will
not verify. Both are cheaper than failing the tool loop.

## How to verify

`internal/translate` golden cases pin all three directions: a chat response now yields a signed
thinking block (`chat_reasoning_response_to_anthropic_replay_signature`), an anthropic request
carrying that block yields `reasoning_content` when tools are present
(`anthropic_replay_thinking_request_to_chat_reasoning_content`) and drops it when they are not
(`anthropic_replay_thinking_request_to_chat_without_tools_dropped`), and the block is dropped when
the outbound protocol is anthropic (`anthropic_replay_thinking_request_to_anthropic_dropped`).
The gateway tests assert the streamed `signature_delta` carries the replay prefix.

End to end, a request whose last message is a tool result and whose tool-call id is foreign
returns 400 through the upstream, and 200 through the gateway when the client echoes the thinking
block the gateway gave it; the same body with the block removed still returns 400.
