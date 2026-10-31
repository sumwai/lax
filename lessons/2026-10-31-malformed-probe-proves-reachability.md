# A deliberately malformed probe request proves reachability, not credentials

Found: 2026-10-31, while adding the two speech protocols (`openai-speech` and
`openai-transcriptions`). Nothing has failed yet; the entry records the trap before a later reader
mistakes a `400` for a credential check.
Fix: the rule and its note live in `internal/gateway/speech.go` (`speechProbeVerdict`); the tests are
`TestProbeSpeechWrongShape` and `TestProbeTranscriptionsMissingFile`.

## Symptom

TTS and ASR have no cheap success call. A real probe would either synthesize text (billed per
character) or upload audio (billed per second), and both create work nobody asked for. The chosen
probe sends a deliberately invalid body instead: `{"model":…,"input":null}` for speech, and a
multipart body with `model` but no `file` for transcriptions.

The upstream then answers `400`. That `400` is treated as `allowed`. A reader who sees `allowed` in
`account_models` will reasonably conclude the key works, because that is what `allowed` means for
every conversational model.

## Why it is easy to miss

Most OpenAI-compatible endpoints validate the key before the body, so `400` does imply "the request
reached the business layer with a credential the upstream accepted". Some do not. A gateway that
routes without checking the key returns `400` for any caller, including one with an empty
`Authorization` header. In that case the same `400` means only "this path exists".

This is the same shape as the earlier note that a public model list proves nothing about
credentials: a request whose success does not depend on the credential cannot testify about it.
Conversational probes avoid it by using a `max_tokens=1` call, which must be authorized.

## Decision

- `401` keeps its existing meaning: the credential is rejected, reachability is not written, the
  account is disabled and the probe run stops.
- `400` and `422` write `allowed` and also write `端点可达，凭据未验证` into `account_models.error`.
  The row therefore states what it does and does not prove.
- `404` writes `missing`; `403` writes `forbidden`; `2xx` writes `allowed` (an upstream that accepts
  the malformed body is still reachable).
- Transport failures keep the old rule: no write, retry twice first.

The cost is accepted: after a speech probe the only evidence of a working credential remains
`account test`, which performs a real list request plus a real minimal call. A speech-only account
therefore needs both commands before its key is trusted.

## How it was verified

`TestProbeSpeechWrongShape` drives all four verdicts against a fake upstream and asserts the access
value, the `凭据未验证` note, the account disable on `401`, and that the outgoing body really is
`{"input":null}`. `TestProbeTranscriptionsMissingFile` asserts the multipart content type, the
absence of a `file` part, and the `allowed` verdict on `400`.
