# TTS fragments are independently base64-encoded, and resource IDs must match the voice family

Found: 2026-11-01, while building a TTS plugin and probing the real
`/api/tts/unidirectional` endpoint with a local upstream key.

Fix: the local plugin package under `plugins/example-tts/` (`index.js` and
`plugin_test.go`). The package is ignored by git; this note records the vendor
behavior rather than a repository commit.

## Symptom

The endpoint streams newline-delimited JSON. Each `data` field is one audio fragment in base64,
and the terminal frame is `{"code":20000000,"message":"OK"}`. Concatenating the `data` strings
before decoding produces audio with extra zero bits at every fragment boundary: each fragment was
base64-encoded independently, including its own `=` padding. The result looks superficially valid
but is corrupt.

The same account also exposed two resource errors:

| Resource ID | Speaker | Result |
| --- | --- | --- |
| `resource-2.0` | `voice-a` | HTTP 200, audio |
| `resource-2.0` | `voice-b` | HTTP 200, audio |
| `resource-2.0` | `voice-other-family` | HTTP 200, JSON `code=55000000 resource ID is mismatched with speaker related resource` |
| `resource-1.0` | any | HTTP 403 `45000030 requested resource not granted` |
| `resource-2.0-concurr` | any | HTTP 403 `45000030 requested resource not granted` |

## Why it was easy to miss

The OpenAI speech API returns one binary body, so a first plugin can return the vendor's body
unchanged or decode a single base64 value. A fake fixture with one fragment does not exercise the
boundary at all. With two or more fragments, joining the encoded strings succeeds syntactically;
the corruption only appears after decoding or listening to the audio.

Resource ID and speaker selection are separate inputs, so the plugin can produce a well-formed
request that the vendor rejects at the business layer with HTTP 200 and a non-zero JSON `code`.
Checking only the HTTP status would miss both failures.

## Decision

Decode each fragment to bytes and re-encode the concatenated stream, rather than concatenating
base64 text. The sandbox has no `atob`/`Buffer`, so the plugin includes a small byte accumulator;
the regression test uses fragment sizes that are not multiples of three and asserts the decoded
bytes, not the encoded string.

The plugin accepts only `resource-2.0` and declares the two verified voices. The resource
ID is an enum so a configuration typo cannot silently pair a voice with the wrong family.
Non-zero response codes and a missing terminal frame are both surfaced as
`upstream_protocol_error`; an HTTP 200 alone is not treated as success.

Cost: adding voices from another resource family requires updating both `models.json` and the
manifest enum, then reinstalling the plugin. That is deliberate until the pairing is verified.

## How it is verified

The decode test sends two independently padded fragments and
asserts the exact decoded bytes. The prepare test pins the resource header, request ID,
usage header, default voice and `speed: 1.5` → `speech_rate: 50` mapping. The error cases cover a
non-zero code, a missing terminal frame and an empty audio stream.

End to end, a temporary lax data directory accepted the plugin, loaded it through
`plugin doctor`, and `model speak` returned a 24 kHz mono MP3. A request through a temporary
`lax serve` with `response_format: "wav"` returned a valid 24 kHz mono PCM WAV. The real key is
read from a local credentials file through stdin and is not part of any test fixture
or command line.
