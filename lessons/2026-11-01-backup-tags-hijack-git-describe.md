# Local backup tags hijack `git describe` when the version is derived without a tag pattern

Found: 2026-11-01, while setting up release versioning for the public repository.

Fix: 7105ba4. The build rule in `Makefile` uses
`git describe --tags --match 'v*'`, and the release workflow passes the pushed tag explicitly.

## Symptom

The repository had three local tags created as manual backups:
`backup-before-msg-rewrite`, `backup-before-unwrap-20261101`, and
`backup-before-unwrap-attic-20261101`. A plain `git describe --tags` resolves the nearest
reachable tag by commit distance, not by version shape. Because those backup tags point at
commits near the branches being built, a version derived from plain `git describe` would report
strings such as `backup-before-unwrap-20261101-12-gabc1234` instead of a SemVer release.

This would not fail the build. It would publish binaries and release metadata whose version
could not be ordered or parsed, and it would only be noticed after the artifacts were public.

## Why it was easy to miss

The backup tags were created for local safety during history rewrites and were never intended to
be releases. They live next to the release tags in the same flat namespace, and `git describe`
does not distinguish intent. The first versioning setup also had no tagged release yet, so there
was no positive example showing which tags were supposed to participate.

## The decision

Release tags are the only version source and must match `v*`. Local backup tags stay local and
are not pushed. `make build` derives the version with
`git describe --tags --match 'v*' --always --dirty`, strips the `v`, and falls back to
`0.1.0-dev` when Git metadata is unavailable. The release workflow passes `GITHUB_REF_NAME`
directly to `make dist`, so a tagged build cannot accidentally pick up another tag.

Cost: a developer who creates a release-like tag without the `v` prefix will not get it included
in the version string. That is intentional; the convention is enforced by the release workflow's
`v*` trigger and by the documented `git tag -a vX.Y.Z` step.

## Verification

Run `make build && ./lax version` with the backup tags present and confirm the output is either a
`v*`-derived version plus commit distance or a commit hash, never a `backup-*` name. Run the
release workflow on a `v*` tag and inspect the published archive with `./lax version`, or run
`make dist VERSION=vX.Y.Z` locally and compare `dist/checksums.txt`.
