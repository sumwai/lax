# Lessons

Index of non-obvious pitfalls found in this repository. Each entry links to a file under
`lessons/` with the full account: what was observed, how it was diagnosed, when it was found,
and which commit fixed it.

Rule: when a review or a bug hunt turns up something that would be hard to reconstruct later,
add an entry here in the same change that fixes it. Files under `lessons/` are written in English
and include the discovery date and the fixing commit, so a later reader can tell whether the
note still applies.

| Found | Topic | File | Status |
| --- | --- | --- | --- |
| 2026-10-29 | Echoing reasoning content back in requests makes reasoning upstreams reject the call | [lessons/2026-10-29-request-side-reasoning.md](lessons/2026-10-29-request-side-reasoning.md) | fixed in 8a54abd |
| 2026-10-29 | Tool results must fold into a `user` message for anthropic upstreams | [lessons/2026-10-29-tool-result-role.md](lessons/2026-10-29-tool-result-role.md) | fixed in e7d29e7 |
| 2026-10-29 | An empty model list must not mean "serves every model" | [lessons/2026-10-29-empty-model-list.md](lessons/2026-10-29-empty-model-list.md) | fixed in 30421bf |
