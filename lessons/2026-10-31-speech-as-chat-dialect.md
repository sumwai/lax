# Speech endpoints built on chat completions need an adapter and a capability declaration

Found: 2026-10-31, while wiring MiMo (`api.xiaomimimo.com`) as a speech upstream. Nothing was broken
in production; the entry records why the gateway cannot treat a voice model as an ordinary chat
model, and what it now refuses locally instead of forwarding.
Fix: `internal/gateway/speech_adapter.go` (the chat dialect codec) and `internal/store/model_capability.go`
(the declaration), with the routes in `internal/gateway/speech.go`.

## Symptom

MiMo has no `/v1/audio/speech` and no `/v1/audio/transcriptions`. Both models are called through
`POST /v1/chat/completions`:

```json
{"model":"mimo-v2.5-tts",
 "messages":[{"role":"user","content":"<style instruction>"},
             {"role":"assistant","content":"<text to read>"}],
 "audio":{"format":"wav","voice":"Chloe"}}
```

The audio comes back as base64 in `choices[0].message.audio.data`; ASR takes an `input_audio` part
and returns text in `choices[0].message.content`. Three probed facts were not what the OpenAI audio
shape implies:

- `voice: "alloy"` returns `400 Param Incorrect`. Only the vendor's own voice names work
  (`mimo_default`, `冰糖`, `Mia`, `Chloe`, …), and the error does not say which part was wrong.
- An ASR request with a text part returns
  `400 "ASR request must not include text parts; text prompt is injected by the gateway"`. The
  `prompt` parameter of `/v1/audio/transcriptions` therefore has nowhere to go.
- `opus`, `aac` and `flac` are rejected; only `mp3`, `wav` and `pcm16` are produced.

## Why it is easy to miss

The vendor documentation shows `client.chat.completions.create`, not `client.audio.speech.create`,
so a client written against the OpenAI audio SDK sends a request shape the upstream never sees.
`Param Incorrect` carries no field name, and every guess costs a real billed call: the first
integration attempt spent a request per wrong voice name.

Treating "the model exists in the catalog" as permission to accept an audio request is worse than a
wrong guess: it turns a client-side typo (`model: deepseek-v4.1-flash` posted to
`/v1/audio/speech`) into a forwarded chat request that the upstream bills and answers with text,
after which the gateway fails while looking for `audio.data`.

## Decision

- Capability is a model fact, declared by hand: `model capability set <model> --capability speech
  --voices … --audio-formats …`. A speech request for a model without the matching capability is
  rejected locally, with zero upstream calls; the same for a voice or format outside the declared
  lists. Capability is separate from the route: the route decides passthrough, the capability
  decides whether the endpoint may be used at all.
- Only audio-inbound to `openai-chat` is adapted. Chat inbound never becomes an audio request, and
  speech inbound never converts to anthropic or responses.
- `/v1/models` carries `capabilities`, `voices` and `audio_formats`, so a client can discover the
  accepted values instead of guessing them.
- Probe results for these models are not trustworthy yet: `model probe` still sends a
  `max_tokens=1` chat request, and the resulting 400 rewrites `allowed` as `forbidden`. Reachability
  is declared with `model add` until a dialect probe exists.

## How it was verified

Against the real upstream: a `POST /v1/audio/speech` with `mimo-v2.5-tts` returned 10104 bytes of
MP3 (`ff f3` frame sync), and posting that file as multipart to `/v1/audio/transcriptions` returned
`{"text":"今天风很大啊。"}`. A speech request for `mimo-v2.6-flash` (a chat model) returned 400 with
no upstream call, and `voice: "alloy"` returned 400 listing the nine declared voices.

The regression tests are `TestSpeechChatAdapter`, `TestSpeechChatAdapterInstructions`,
`TestSpeechWithoutCapabilityRejected`, `TestSpeechVoiceRejected`, `TestTranscriptionsChatAdapter`
and `TestTranscriptionsRejectsOGG`.
