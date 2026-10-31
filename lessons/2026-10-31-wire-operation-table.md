# Pinning `speech.*` to the speech wire protocols makes a chat-shaped vendor impossible to declare

Found: 2026-10-31, while moving MiMo's speech codec out of `internal/gateway` into `internal/plugin`
next to the declarative dialect compiler.

Fix: in progress; the pair check lives in `internal/plugin/dialect.go` (`wireOperations` and
`ValidateDialectDecl`), the regression coverage in `internal/plugin/dialect_test.go`,
`internal/plugin/speech_test.go` and `testdata/plugin-chatvoice/`. No commit yet.

## Symptom

A dialect file that declares a chat-shaped speech upstream is rejected at install time:

```
dialects.json: dialects[0]: upstream_wire openai-chat 只承载 chat.complete@1，这里写的是
speech.transcribe@1：允许的对应关系是 anthropic → chat.complete@1、…、openai-chat →
speech.synthesize@1、…
```

The same error lists `openai-chat → speech.synthesize@1` among the allowed pairs, so the message
contradicts itself: it names the pair it just refused. Meanwhile the two built-in dialects that do
exactly this are accepted, because built-ins are registered through `Registry.RegisterSystem`,
which only checks that the operation and the wire are names the host knows.

## Why it was easy to miss

Capability and wire protocol were two independent dimensions until this change, and nothing ever
cross-checked them in one table:

- whether a model may be used for a speech endpoint was answered by `provider_models.capabilities`
  (`speech` / `transcription`), set by `lax model capability`;
- which shape the request takes upstream was decided by a Go branch in
  `internal/gateway/speech_adapter.go` that only fired for `openai-chat`;
- the protocol a model goes out on was `store.EffectiveProtocol`, and speech never went through it.

So "speech operation" and "speech wire protocol" were never stated together anywhere. The
`wireOperations` slice looked like the authority on the pairing, but the check computed the
expected operation from the wire with `OperationForInbound` (a 1:1 map) and compared; the slice was
only used to build the error text (`wirePairNames`). A row could therefore be listed as allowed
without any check reading it.

## What exposed it

Two tests, each passing on its own, that cannot both be right:

- `TestSystemDialectsAreComplete` pins the four built-in dialects, two of which are
  `speech.synthesize@1` / `speech.transcribe@1` × `openai-chat`;
- `TestValidateDialectDeclForRegistry` and the `dialect-bad-combo.json` fixture assert that the
  same pair is rejected.

Whichever way the validation is written, one of them has to move. The install pipeline surfaced it
again from the other side: a plugin with a MiMo-shaped dialect cannot be installed at all, so the
migration that was supposed to take MiMo out of the gateway left it only in the Go built-ins.

## Decision

Allow `speech.synthesize@1` and `speech.transcribe@1` on `openai-chat` as well as on their own
speech wire protocol. The inbound wire of a speech request stays one of the two audio protocols:
clients keep posting to `/v1/audio/speech` or `/v1/audio/transcriptions`, and the operation name is
still derived from the inbound wire. What the pair table answers is a different question — which
upstream wires a dialect may name for that operation. A vendor that speaks chat completions is a
legitimate answer to it, and MiMo is the proof.

The check has to read the pair table, not `OperationForInbound`. `OperationForInbound` keeps its
meaning (inbound wire to operation) and must not be used to validate the upstream side.

## Cost

- "One wire, one operation" stops holding for `openai-chat`: it carries `chat.complete@1`,
  `speech.synthesize@1` and `speech.transcribe@1`. Any code that infers the operation from the
  upstream wire has to stop doing that, or it will misread every chat-shaped speech model.
  `OperationForInbound` therefore answers a different question (inbound wire to operation) and is
  no longer used to validate the upstream side.
- Before the fix, a plugin could not declare a chat-shaped speech dialect at all, and the
  declarative mapping had no reachable combination in the gateway: same-wire speech dialects are
  passthrough (their `request` and `response` declarations never execute), and chat-shaped
  declarative dialects were rejected.
- Probe and account selection still work per protocol (see 5.7 and 5.16): a dialect binding does
  not add or remove candidate protocols, so opening the pair does not change which accounts a
  request may reach.

## How it is verified

- `ValidateDialectDecl` accepts `speech.synthesize@1` × `openai-chat` and
  `speech.transcribe@1` × `openai-chat` (`TestValidateDialectDeclForRegistry`);
- it still rejects a pair that is not in the table, for example `speech.transcribe@1` ×
  `anthropic`, with a message that lists the pairs actually allowed;
- `dialect-bad-combo.json` points at a genuinely illegal pair instead of the chat-shaped one;
- `TestSystemDialectsAreComplete` keeps passing unchanged;
- `testdata/plugin-chatvoice/` declares a chat-shaped speech dialect in JSON only, and
  `TestChatWireDeclarativeDialect` loads it, registers it, builds a request and decodes a base64
  audio response with usage, without any Go codec.

## Fix status

fixed in 5063c32. The pair table now drives the check, `OperationForInbound` keeps its inbound
meaning, `dialect-bad-combo.json` points at `speech.transcribe@1` × `anthropic`, and
`testdata/plugin-chatvoice/` declares a chat-shaped speech dialect in JSON only.
