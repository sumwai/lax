# A dropped column must not be the same change that introduces its replacement

Found: 2026-10-29, twice in one session: migration `0014` dropped `accounts.base_url` and
`accounts.protocol`, migration `0015` dropped `accounts.models` and renamed `account_models.status`
to `access`. Both shipped with the store layer green and the rest of the tree red.

## Symptom

After each store commit, `make check` failed with a long list of gateway and CLI tests. The failures
were not subtle: the CLI printed an empty outbound protocol and the gateway answered
`不支持的出站协议 ""`, because the code still read a field whose column no longer existed. Each time
two further batches were needed — one for the gateway, one for the CLI — before the tree was green
again.

## Why it happened

The store change is the one that cannot be skipped: the new table shape has to exist before anything
can read it. So it goes first, and every consumer of the old shape breaks in that same commit. The
commit message and the review both looked at the store package, where tests passed, and the red tree
was discovered only by running `make check` afterwards.

## Decision

When a change replaces where data lives, land it in three steps instead of one:

1. add the new storage and write both places (readers unchanged, tests green);
2. move readers to the new place and delete the dual write (tests green, old columns still present);
3. drop the old columns in a migration that touches nothing else.

The cost is more commits and, for a while, two sources of truth that can disagree. The benefit is
that every commit stays green and a broken consumer is a small, obvious diff rather than a rediscovery
of the whole data flow.

## How to verify

`make check` after each step, not only inside the package that changed. For step 3 the check is
`PRAGMA table_info` on the affected table plus the full test suite: if anything still references the
column, the build fails in the same commit that drops it.
