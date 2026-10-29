# Tool results must fold into a user message for anthropic upstreams

Found: 2026-10-29, in a read-only audit of the tool-call translation chain at commit `89f9c46`.
Fixed: the role folding landed in `e7d29e7`; merging results with a following user message landed
in `8a54abd`.

## Symptom

An inbound `openai-chat` or `openai-responses` request that carried tool results was decoded into
an internal message with role `tool`, and the anthropic encoder wrote that role through verbatim:

```json
{"role": "tool", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "..."}]}
```

Anthropic-compatible upstreams only accept `user` and `assistant` roles and answer 400. Every
"chat client with tools, anthropic account" request therefore failed, and the only visible signal
was an upstream error that pointed at nothing in particular.

## Why it was easy to miss

The internal representation has a `tool` role because both OpenAI protocols have one, and the
anthropic decoder produced `tool_result` blocks inside user messages. The gap was on the encode
side, where nothing forced the role to be translated rather than copied. The existing tool tests
covered `chat -> chat` and `anthropic -> anthropic`, not the cross-protocol direction.

## Decision

Encoding to anthropic folds tool results into a `user` message: consecutive results following one
assistant message merge into a single `user` message, and any following user content joins that
same message so two consecutive `user` messages cannot appear.

Cost: because tool results and the next user turn share one message, the two are no longer
distinguishable once encoded. That is the shape anthropic expects, and the alternative (two
consecutive user messages) is rejected outright, so the merge is the working choice rather than
the strictly lossless one.

## How to verify

`TestChatToolResultsToAnthropicUpstream` and `TestAnthropicToolUseResponseToChatClient` assert the
upstream body has no `"role":"tool"`, that both tool results of one assistant message land in one
user message in order, and that response-side tool calls keep their ids and arguments.
