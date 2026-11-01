# GitHub-hosted runners cannot reach Tailscale-only SSH addresses

## Found

2026-11-01, while adding automatic deployment of released `lax` binaries to `aliyun-hongkong`.

## Symptom

The local `~/.ssh/config` reaches `aliyun-hongkong` through its Tailscale address
`100.81.171.10`. A GitHub-hosted runner has no route to that address, so an SSH-based deployment
workflow using the local alias would hang or fail even though the same command works from the
maintainer workstation.

## Why it was not noticed earlier

Every previous deployment was run by hand from a machine already joined to the tailnet. The SSH
configuration made the tailnet address look like the server's only address, so the missing route
only appears when a hosted runner tries to connect.

## Decision

Deploy over the server's public SSH endpoint, `larkax.com:22`, with a dedicated ed25519 key and a
pinned `known_hosts` entry. The public hostname is supplied through the `DEPLOY_HOST` repository
secret, and the host key through `DEPLOY_KNOWN_HOSTS`; the workflow never disables host key
verification. The deployment still installs to the same paths and restarts the same
`lax.service`.

The cost is that the deploy key grants a shell on a publicly reachable host. It is kept only in
GitHub Actions secrets and can be rotated by replacing the key in the server's
`~/.ssh/authorized_keys` and the `DEPLOY_SSH_KEY` secret. The deploy job only runs for `v*` tags.

## Verification

Connected to `larkax.com:22` with the dedicated key and the pinned host key, then deployed the
published `v0.1.0` Linux amd64 archive. The remote script verified the archive version, restarted
`lax.service`, and the job passed the local `/healthz` check. The public gateway still returns
`401` without credentials, as expected.

## Fix

Fixed in `5015185`, which added the public-host deploy job and both deployment scripts.
