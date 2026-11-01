# A speech dialect on its own wire protocol was silently downgraded to passthrough

Found: 2026-11-01, while preparing a Doubao voice plugin whose TTS upstream is
`openai-speech` inbound and `openai-speech` outbound, but with its own path and request body.

Fix: 7bba977. The dispatch predicate lives in `internal/gateway/speech_dialect.go`
(`speechUsesDialect`), the wire declaration in `internal/plugin/manifest.go`
(`providers[].routes`, `providers[].auth`), and the regression coverage in
`internal/gateway/provider_auth_test.go` (`TestSpeechHookDialectOwnWire`).

## Symptom

A model bound to a plugin dialect whose `upstream_wire` equals the inbound protocol
(`openai-speech` for TTS) was not run through the dialect at all. The gateway took the
passthrough branch: it patched only the `model` field of the client's OpenAI-shaped body and
posted it to the path from the provider route.

The upstream therefore received `{"model":…,"input":…,"voice":…,"response_format":…}` instead
of the vendor's own request body, and the response was written back verbatim instead of being
decoded by the dialect. No error was raised: the request was well-formed for the wire protocol,
it just carried the wrong shape. The failure only showed up as a 4xx from the vendor, and the
gateway logged a normal passthrough attempt.

## Why it was easy to miss

`speechUsesDialect` was written when the only shape a dialect could describe beyond passthrough
was "speech made to look like chat completions" (MiMo). Its condition therefore encoded that
one case literally:

```go
return d.UpstreamWire() != inbound && outbound == ProtocolChat
```

Both halves are about MiMo: `outbound == ProtocolChat` is only true for a chat-shaped speech
vendor, and `d.UpstreamWire() != inbound` assumed that a dialect on the same wire was by
definition a passthrough. Every test used a chat-shaped dialect, so the branch for
"same wire, different shape" was never constructed. The `lax/` prefix was also the wrong
discriminator in the other direction: MiMo's built-in dialects carry the `lax/` prefix and do
need `Prepare`/`Decode`.

## The decision

The predicate is now:

```go
return !plugin.IsHostDialect(d.ID()) || d.UpstreamWire() != inbound
```

- a host dialect whose wire equals the inbound protocol is the passthrough case
  (`lax/openai-speech@1`, `lax/openai-transcriptions@1`);
- every other dialect, including plugin dialects on the same wire and the built-in MiMo chat
  dialects, runs `Prepare`/`Decode`.

Cost: the predicate now depends on the dialect identity rather than the outbound protocol, so
`speechUsesDialect` no longer needs the `outbound` argument and can no longer be fooled by a
binding whose wire matches the inbound protocol. A vendor that only differs from the OpenAI
audio body by field names now has to ship a dialect instead of relying on passthrough — which
is the point, since passthrough cannot rename fields.

## Verification

`TestSpeechHookDialectOwnWire` binds a speech model to a test dialect that declares
`openai-speech` and returns a `Plan` with its own body, path-independent route
(`/api/v3/tts/unidirectional`) and headers. The upstream recorder asserts the vendor-shaped
body arrived, the plugin header was applied, and that the plugin could not override the
credential header. Before the fix the request body was the OpenAI-shaped passthrough body and
the plugin headers were absent.
