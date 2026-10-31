# goja has no heap cap, and Interrupt cannot stop in-flight native calls

Found: 2026-10-31, while building the in-process `moejs` sandbox that runs plugin hooks.

Fix: in progress; the gates live in `internal/plugin/sandbox.go` (`sandboxInlineLimit`,
`MaxHookOutputBytes`, `sandboxMaxDepth`), `internal/plugin/sandbox_module.go` (`maxModuleBytes`,
`maxModuleTotal`, `maxModules`) and `internal/plugin/sandbox_api.go` (`sandboxHTTPCallTimeout`,
`sandboxHTTPCalls`, `sandboxHTTPTotal`, `cacheMaxBytes`). No commit yet.

## Symptom

Two limits that look like the same limit behave differently.

A hook that never returns from JavaScript is stopped. `prepare_request` with `while (true) {}`
hits `Runtime.Interrupt` after the configured budget and fails with `plugin_timeout`; the next
call builds a fresh Runtime and works. That is the case every timeout test exercises.

A hook that stays inside a native Go call is not stopped. `lax.http.request` against a host that
accepts the connection and then stalls keeps running after the 100 ms hook budget expires: the
interrupt flag is set, but the Go HTTP call owns the goroutine until its own timeout (15 s per
call, 30 s per hook in total). The hook returns `plugin_timeout` only after that call finishes,
and the request that triggered it stays parked for the same time.

Memory has no comparable stop at all. goja exposes no heap limit; the only checks are byte gates
at the boundary, and the return-value gate runs after the value already exists. A script that
allocates in a loop grows the whole process until the OS kills the gateway. The four byte gates
(source files, return value, injected audio and stream chunks, cache) bound what crosses the
host↔plugin boundary, not what lives inside the interpreter.

## Why it was easy to miss

- A per-hook timeout reads like resource isolation. In a worker-based runtime it is: the worker
  gets rlimits and a memory ceiling. In-process engines do not have that separation, and Go has no
  per-goroutine memory limit to set.
- goja's own documentation states the caveat in one sentence: `Interrupt` "only works while in
  JavaScript code, it does not interrupt native Go functions (which includes all built-ins)".
  The natural timeout test uses a JavaScript infinite loop, so it passes and gives the impression
  that the budget covers everything.
- The control-plane HTTP test uses `httptest`, which answers immediately. Nothing in the suite
  holds a response open past the hook budget, so the gap between "JS stopped" and "call finished"
  never shows up as a failing test.
- The byte gates were added one at a time, each for a concrete failure (a 9 MiB return value, a
  64-byte body limit, a base64 audio blob). None of them is a memory limit, and nothing in the
  code says so out loud.

## Decision

Do not claim a memory cap that does not exist. Keep the four byte gates, add a per-hook timeout,
and keep the native surface as small as possible:

- every host↔plugin crossing is counted in bytes before it is handed over;
- `lax.http.request` is the only network primitive, with its own host whitelist, no redirects,
  15 s per call, 4 calls per hook and 30 s per hook;
- the documentation states "no hard memory cap" next to the gates, so an operator does not read
  the timeout as an isolation boundary.

The alternative — writing a JSON expression language for request patches and stream framing
instead of an interpreter — does not remove the problem: it moves it into a smaller evaluator with
its own unbounded allocations and its own interrupt story.

## Cost

- The gates are byte counts at boundaries, not live heap. A script can allocate far more than any
  gate between two crossings, and deliberately malicious code can still take the process down.
  `moejs` is for code that can be reviewed, not for untrusted code that must be contained.
- The timeout is a budget, not a deadline for the whole call. A slow control-plane call makes a
  100 ms hook occupy seconds, and the reported latency includes the native time. The sandbox
  serializes hooks per plugin version, so that call also blocks the plugin's other hooks.
- Every future native API has to be audited for the same property: if it can block, the hook
  timeout does not cover it, and it needs its own cancellation or timeout. `lax.cache` and the
  hashing helpers are synchronous and bounded; a filesystem or subprocess API would not be.
- The 8 MiB return-value gate is checked after serialization, so the JSON round trip itself needs
  memory proportional to the value before the gate rejects it.

## How it is verified

- `TestSandboxTimeout` runs a JavaScript infinite loop with a 50 ms budget, gets
  `plugin_timeout`, then calls the same sandbox again and gets a valid plan from a fresh Runtime.
- `TestSandboxOutputTooLarge` returns a value above 8 MiB and asserts the error names the limit.
- `TestSandboxHTTPRequest` covers the host whitelist, `mode: "none"`, redirect refusal and the
  response-body limit (`max_control_body_bytes`).
- `TestSandboxImportRejected`, `TestSandboxProtoRejected` and `TestSandboxGlobals` pin the other
  borders: no bare or escaping imports, no `__proto__` keys, no `fetch`/`process`/`fs`/
  `setTimeout` in the global object.
- Manual check for the native-call gap: point `lax.http.request` at a server that sleeps longer
  than the hook budget and observe that the hook error arrives after the sleep, not at the
  budget. There is no automated test for it yet because it needs a deliberately slow upstream.

## Fix status

fix in progress; fixing commit pending. The byte gates, the per-hook timeouts, the control-plane
caps and the tests above live in the sandbox files; the heap cap is not something the engine can
provide, so the limit is documented instead of promised.
