# A client hanging up looks exactly like a cancelled upstream read

Found: 2026-10-31, from a codex 0.166 session against a chat upstream: the client worked, but
`lax log tail` showed rows alternating between clean successes and
`上游流式响应中断: context canceled`.
Fix: `fix(gateway)` that treats a cancelled request context as a client hang-up and records it
against whether the terminal event had already been sent.

## Symptom

Every second or third request of a codex session was recorded as a stream interruption with the
error text `上游流式响应中断: context canceled`, while the client showed a normal answer and kept
going. Input and output token counts were recorded for those rows too, so the upstream had clearly
produced a full response.

## Why it was easy to miss

Both causes surface as the same Go error. `http.NewRequestWithContext(r.Context(), …)` ties the
upstream request to the client request, so when the client closes its connection the request
context is cancelled and the next read of the upstream response body fails with `context canceled`.
The gateway recorded that read error verbatim, labelled it as an upstream interruption, and marked
the request failed.

The intermittency comes from a race. codex stops reading as soon as it sees `response.completed`,
and the gateway emits that event when the upstream sends its stop chunk. A chat upstream still has
two frames to send after that — the usage-only chunk (streaming with `include_usage`) and
`[DONE]` — so whether the cancel lands before or after those arrive decides whether the row turns
red. That is also why the token counts look complete on the failing rows: usage had already been
read, the remaining frames were the ones lost.

## Decision

Client cancellation is not an upstream failure, and the deciding fact is whether the terminal event
already went out, not what the read error says:

- terminal sent (conversion path: the `stop` delta was written, or `Finish` ran; passthrough path:
  the upstream terminal frame was forwarded) → record `ok=1` with an empty `error`;
- terminal not sent → record `ok=0` with `error` = `客户端提前断开`, and use `errClientGone`
  rather than the read error so nothing points at the upstream;
- neither case appends an error frame or a protocol tail (the client is gone) and neither cools the
  account down.

A write failure is the same signal and now goes through the same decision: a broken pipe after the
terminal event is a completed request, not a failed one.

## How to verify

Three gateway tests in `internal/gateway/client_gone_test.go` drive a fake upstream that writes the
stop chunk, waits on a channel, then writes the usage chunk and `[DONE]`, while the client cancels
right after reading the terminal event:

| Test | Cancelled after | Recorded |
| --- | --- | --- |
| `TestClientGoneAfterTerminalIsSuccess` | `response.completed` | `ok=1`, no error |
| `TestClientGoneBeforeTerminalIsClientError` | first text delta | `ok=0`, `客户端提前断开` |
| `TestPassthroughClientGoneAfterTerminalIsSuccess` | `[DONE]` forwarded | `ok=1`, no error |
