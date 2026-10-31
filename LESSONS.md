# Lessons

Index of non-obvious pitfalls found in this repository. Each row links to a file under
`lessons/` with the full account: what was observed, how it was diagnosed, when it was found, and
which commit fixed it. Add a row in the same change that fixes the pitfall, and keep the fixing
commit in the row so a later reader can tell whether the note still applies.

| Found | Topic | File | Status |
| --- | --- | --- | --- |
| 2026-10-29 | Echoing reasoning content back in requests makes reasoning upstreams reject the call | [lessons/2026-10-29-request-side-reasoning.md](lessons/2026-10-29-request-side-reasoning.md) | fixed in 8a54abd |
| 2026-10-29 | A message that loses every block must not be encoded with empty content | [lessons/2026-10-29-empty-content-message.md](lessons/2026-10-29-empty-content-message.md) | fixed in 0a034b8 |
| 2026-10-29 | A stream suffix rewrite must anchor to the end of the path | [lessons/2026-10-29-gemini-stream-suffix.md](lessons/2026-10-29-gemini-stream-suffix.md) | fixed in 323c522 |
| 2026-10-29 | Tool results must fold into a `user` message for anthropic upstreams | [lessons/2026-10-29-tool-result-role.md](lessons/2026-10-29-tool-result-role.md) | fixed in e7d29e7 |
| 2026-10-29 | An empty model list must not mean "serves every model" | [lessons/2026-10-29-empty-model-list.md](lessons/2026-10-29-empty-model-list.md) | fixed in 30421bf |
| 2026-10-29 | A public model list proves nothing about credentials | [lessons/2026-10-29-public-model-list.md](lessons/2026-10-29-public-model-list.md) | fixed in 2ed8c7c |
| 2026-10-29 | A gateway that rebuilds upstream headers must forward the client's feature headers | [lessons/2026-10-29-client-header-forwarding.md](lessons/2026-10-29-client-header-forwarding.md) | fixed in f86972e |
| 2026-10-29 | "Input tokens" does not mean the same thing in every protocol | [lessons/2026-10-29-input-token-semantics.md](lessons/2026-10-29-input-token-semantics.md) | fixed in 8700e1c |
| 2026-10-29 | A dropped column must not be the same change that introduces its replacement | [lessons/2026-10-29-drop-column-sequencing.md](lessons/2026-10-29-drop-column-sequencing.md) | 0014 / 0015 |
| 2026-10-31 | Clients send `role: "system"` turns mid-conversation, and unknown model names enable them | [lessons/2026-10-31-mid-conversation-system.md](lessons/2026-10-31-mid-conversation-system.md) | fixed in 43eb3a2 |
| 2026-10-31 | Reasoning upstreams require their own CoT back when the request carries tools | [lessons/2026-10-31-reasoning-replay.md](lessons/2026-10-31-reasoning-replay.md) | fixed in 992f421 |
| 2026-10-31 | A client hanging up looks exactly like a cancelled upstream read | [lessons/2026-10-31-client-hangup-looks-like-upstream-cancel.md](lessons/2026-10-31-client-hangup-looks-like-upstream-cancel.md) | fixed in d77792e |
| 2026-10-31 | A transport failure must not decide whether a model is reachable | [lessons/2026-10-31-probe-transport-is-not-a-verdict.md](lessons/2026-10-31-probe-transport-is-not-a-verdict.md) | fixed in f1f3c37 |
| 2026-10-31 | A model-level protocol declaration outranks the provider route list | [lessons/2026-10-31-declared-protocol-filtered-by-routes.md](lessons/2026-10-31-declared-protocol-filtered-by-routes.md) | fixed in f1f3c37 |
| 2026-10-31 | AutoMigrate cannot take over a schema written by hand-rolled DDL | [lessons/2026-10-31-automigrate-takes-over-legacy-ddl.md](lessons/2026-10-31-automigrate-takes-over-legacy-ddl.md) | fix in progress |
