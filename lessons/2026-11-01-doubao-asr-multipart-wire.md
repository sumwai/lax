# A same-wire ASR dialect was shadowed by multipart passthrough

Found: 2026-11-01, while connecting the Doubao Seed ASR plugin to the local lax
gateway. The plugin was installed, its dialect was registered, and the model was
bound correctly.

Fix: `internal/gateway/gateway.go` (`buildRequest`) and the generic regression
test in `internal/gateway/transcriptions_dialect_test.go`. No commit hash yet;
status is `fix in progress`.

## Symptom

`lax model transcribe /tmp/plain.mp3 --model doubao-seed-asr --language zh
--format json` returned HTTP 400 from Doubao:

```text
{"header":{"reqid":"","code":45000000,"message":"invalid character '-' in numeric literal"}}
```

The same endpoint, key and JSON body sent directly with curl succeeded and
returned `今天天气真好，我们出去走走吧。`. The request record showed the expected
protocol pair (`openai-transcriptions → openai-transcriptions`), account
`doubao` and model `bigmodel`, so selection and alias resolution had worked.

## Root cause

Doubao ASR declares `upstream_wire: openai-transcriptions`, the same wire as the
inbound endpoint, but its actual body is JSON. `buildRequest` checked the
passthrough case before the dialect case:

```go
case outbound == ProtocolTranscriptions && inboundHeaders != nil:
    body, contentType, err = rewriteTranscriptionsBody(...)
case speechUsesDialect(inbound, dialect):
    body, planHeaders, err = s.encodeSpeechDialect(...)
```

Because both conditions were true, the gateway rewrote the inbound multipart
body and sent it to Doubao. Doubao parses the body as JSON; the first bytes of a
multipart body start with `--<boundary>`, which produced the
`invalid character '-' in numeric literal` error. The plugin's
`prepare_request` never ran.

## Why it was easy to miss

There was a regression test for a plugin TTS dialect on its own wire protocol
(`openai-speech`), and that path had no competing passthrough branch. The ASR
tests covered plain multipart passthrough and the MiMo chat dialect separately,
so every individual branch was tested but the combination "plugin dialect whose
wire equals `openai-transcriptions`" was not. The multipart rewrite had also
been added before hook dialects could own the ASR wire, and the branch order was
not revisited when that support landed.

## Decision

Move `speechUsesDialect` ahead of the `openai-transcriptions` passthrough
branches. Host passthrough (`lax/openai-transcriptions@1`) still rewrites only
the `model` part; a plugin or built-in dialect now always gets `Prepare` and
`Decode`, even when its upstream wire matches the inbound wire.

Cost: none for passthrough users. The ordering is now part of the contract, so a
future branch that rewrites ASR bytes must be placed after the dialect check.

## Verification

`TestTranscriptionsHookDialectOwnWire` binds a fake hook dialect whose upstream
wire is `openai-transcriptions`, posts an OpenAI multipart request through the
gateway to an `httptest` upstream, and asserts that the upstream receives JSON
produced by `Prepare`, not multipart. It also asserts the inbound protocol
remains `openai-transcriptions` and the resolved model is recorded. The test
deliberately contains no vendor or model names; protocol mapping belongs to the
plugin's own `plugin_test.go`.

Live verification through `127.0.0.1:6543`: `json`, `text` and `verbose_json`
all returned HTTP 200 with the sentence above; `verbose_json` reported duration
`2.447` and one segment from `0.12` to `2.28` seconds. `lax log tail` recorded
`openai-transcriptions → openai-transcriptions`, alias `doubao-seed-asr →
bigmodel`, account `doubao`, HTTP 200.
