# AutoMigrate cannot take over a schema written by hand-rolled DDL

## What was observed

Introducing GORM `AutoMigrate` on top of the existing eight tables looked safe: on an empty
database it reproduced all eight tables, and a second `AutoMigrate` on the result wrote nothing
(idempotent). Adding two columns produced exactly two `ALTER TABLE ... ADD COLUMN` statements.

On a copy of a real database it was destructive. Five scenarios were measured, each against a
copy of a database produced by the 18 deleted `migrations/*.sql` files:

| Scenario | Write statements | Outcome |
| --- | --- | --- |
| Empty database | 0 after first run | all eight tables reproduced |
| Second `AutoMigrate` | 0 | idempotent |
| Add two columns | 2 | clean `ALTER TABLE ADD COLUMN` |
| Take over the real database copy | 13 | three tables rebuilt, `provider_models.context_window` data lost |
| Take over the same database with SQL comments stripped | 41 | `providers` rebuilt three times |

The data loss is visible in the rebuild's `INSERT ... SELECT` column list: `context_window` is
missing from both sides, so that column's values are dropped. The repeated rebuild is
`AutoMigrate` disagreeing with its own output: it writes `DEFAULT ""` and then fails to parse that
back as equal to the declared default on the next pass.

## Why it was not caught earlier

The empty-database path is the one exercised by every unit test, and it is flawless. The failure
only appears when the existing DDL was written by hand: the old files carry inline `--` comments
inside `CREATE TABLE`, use upper-case column types without backticks, and declare `UNIQUE` inline
rather than as a separate index. GORM's SQLite migrator decides column equality by comparing the
text of `sqlite_master.sql`, so any formatting difference from its own output reads as a mismatch
and triggers a table rebuild.

## Decision and cost

`AutoMigrate` stays, but only for a database it created. Legacy databases are migrated by
`internal/store/legacy.go`, which never compares DDL:

1. read the old tables by column name (`PRAGMA table_info` + `SELECT *`), skipping tables that do
   not exist and columns that were added later;
2. create a fresh database at `<path>.new`, run `AutoMigrate` plus the seed on it;
3. write the rows back in batches, applying the semantic conversions the old migrations did
   (status word list to `access`, orphan accounts to provider rows, `default_protocol` fill-in,
   `accounts.models` split into catalog and reachability rows, account-level prices into the
   per-account override columns, `in_tokens_uncached` backfilled from `in_tokens`);
4. rename the original to `<path>.bak` and `<path>.new` into place.

The cost is a second schema path that has to be maintained as long as legacy databases exist, plus
a 600-line file that duplicates conversion logic already expressed in the deleted migrations.
The alternative — letting `AutoMigrate` rebuild tables in place — was rejected because the
rebuild loses column data silently and because a `DROP TABLE` in the middle of the sequence is
unrecoverable.

Seeds are now written only when the database has no `providers` table. Running them on every open
resurrected providers and routes the user had deleted, which two CLI tests caught
(`TestProviderUnsetDefaultRouteRefused`, `TestProviderRmForceDisablesAccounts`).

## How it is verified

`TestMigrationChainFrom0001ToLatest` builds a database from `migrations/0001_init.sql` and runs it
forward through the eight files up to `0008`, then opens it and asserts the migrated state: seed
providers and routes present, `account_models.protocol` added, the `agents` table gone, old
accounts and request rows unchanged. Fifteen more upgrade cases cover the individual conversions
(orphan provider rows, price overrides, `in_tokens_uncached` backfill, reasoning columns left at
zero, gemini seed preserved when the user already defined that provider).

## Fix

`fix in progress` — the commits that introduce `internal/store/model.go`, `internal/store/legacy.go`
and the rewritten migration tests.
