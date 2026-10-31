# Declared upstream audio-format names differ from the inbound OpenAI names

Found: 2026-10-31, while configuring MiMo TTS (`mimo-v2.5-tts`) and running
`lax model speak --format pcm` against the real upstream.

Fix: `internal/gateway/speech_adapter.go` (`containsSpeechFormat`); regression test
`TestSpeechChatAdapterPCM` in `internal/gateway/speech_test.go`.

## Symptom

The capability declaration followed the vendor documentation and said
`--audio-formats mp3,wav,pcm16`. A TTS request with `"response_format": "pcm"` was rejected
locally with 400 before any upstream call:

```
response_format "pcm" 不在模型 mimo-v2.5-tts 声明的音频格式里，可用的是 mp3、wav、pcm16
```

`mp3` and `wav` worked. `lax model speak --format pcm` failed the same way, and any OpenAI client
using the standard `pcm` name would hit it too.

## Why it was easy to miss

The same stream has two names: OpenAI's inbound `response_format` calls it `pcm`, while the
upstream chat dialect calls it `pcm16`. The adapter already translated the value when building the
upstream request (`pcm` -> `pcm16`), but the local capability check compared the raw inbound name
against the declared upstream name. The earlier adapter test used `mp3`, so the mismatch only
appeared when a real client asked for PCM.

## Decision and cost

Keep `audio_formats` in the catalog as upstream facts, because `/v1/models` reports what the vendor
actually produces. At the local gate, accept either the raw inbound name or its normalized upstream
name. The change is one extra comparison; the wire format and the catalog stay unchanged.

## How it was verified

Against the real upstream, `lax model speak --model mimo-v2.5-tts --voice 冰糖 --format pcm` returned
76800 bytes with `Content-Type: audio/pcm`. `TestSpeechChatAdapterPCM` asserts that a request with
`response_format: pcm` succeeds and sends `audio.format: pcm16` upstream.

## Fix status

fixed in 65b191e. The comparison now lives in `containsSpeechFormat` in
`internal/gateway/speech_capability.go`, next to the capability gate that needs it.
