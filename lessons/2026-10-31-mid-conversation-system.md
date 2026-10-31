# Clients send `role: "system"` turns mid-conversation

Found: 2026-10-31, from a reported failure where Claude Code on `deepseek-flash` returned
`400 anthropic: 不支持的消息角色 "system"`.
Fix: `43eb3a2`, an anthropic decode that accepts a mid-conversation `system` message as an
IR `RoleSystem` message in place.

## Symptom

A Claude Code session pointed at a gateway model name that Claude Code does not know
(`deepseek-flash`, routed to an `openai-chat` upstream) failed on the first turn with
`400 anthropic: 不支持的消息角色 "system"`. Nothing reached the upstream: the anthropic decoder
rejected the body before account selection, so no provider, key or route was involved.

## Why it was easy to miss

The Anthropic Messages API documents `user` and `assistant` only; a `system` prompt is a top-level
field. The decoder followed the documentation, and no fixture in `testdata/` carried a `system`
message, so every conversion test passed.

The client side is the part that is not documented anywhere. Claude Code 2.1.310 sends
mid-conversation `system` turns behind a `mid_conv_system` capability flag, and the lookup for a
model it has no record of returns "unknown" rather than "off":

```
return mS(r,"mid_conv_system",e) ?? !0
```

The `?? !0` is the whole problem: every third-party model name lands on "enabled", so the client
inserts `{"role":"system","content":[{"type":"text",…}]}` into the history of a conversation it
was only ever tested against Anthropic with, where such turns are accepted under a beta header.
The flag name and the fallback path are visible in the client binary as
`CLAUDE_CODE_FORCE_MID_CONVERSATION_SYSTEM` and `[mid-conv-system] server rejected role:"system"`.

## Diagnosis

The reported error text is lax's own, so the failure was in this repository rather than upstream.
A local instance started on a scratch `XDG_CONFIG_HOME` and the same body reproduced it byte for
byte, which ruled out the account, routing and model catalogue.

Identifying the sender took grepping the 291 MB client binary for `role:"system"`:

| Hit | What it established |
| --- | --- |
| `role:"system",content:[…cache_control…]` | The client really builds system turns with text blocks and cache breakpoints |
| `$9o=[…"mid_conv_system"…]` | It is a declared model capability |
| `mS(r,"mid_conv_system",e) ?? !0` | Unknown model names default to enabled |
| `[mid-conv-system] server rejected role:"system" … sticky-rejecting` | The client has a retry-without-system-turns fallback, which did not rescue this request |

## Decision and cost

The anthropic decoder accepts a mid-conversation `system` message and keeps it in the IR as a
`RoleSystem` message at its original position, text blocks only — the same rule the top-level
`system` field already followed. Every encoder already had a place for such a message:
`openai-chat` writes a `role=system` message, `openai-responses` and `gemini` fold it into their
single top-level instruction field.

The cost is positional fidelity across protocols: routing the same conversation to an
`openai-responses` or `gemini` upstream moves the reminder to the top of the system prompt, where
it is read earlier than the client intended. Rejecting the message instead costs the whole
request, which is strictly worse. Unknown roles (`tool`, `developer`, typos) still fail with the
same 400.

## How to verify

`internal/translate/anthropic_system_message_test.go` decodes a history with a `system` turn
between two user turns and asserts the role, the position and the text-only block list. Two golden
cases pin the outbound shapes: `anthropic_mid_conv_system_to_chat_kept` keeps the turn as its own
`role=system` message, `anthropic_mid_conv_system_to_gemini_folded` folds it into
`systemInstruction`. The end-to-end check is the scratch-server request that used to return 400;
it now reaches account selection, which is where an unconfigured instance is expected to stop.
