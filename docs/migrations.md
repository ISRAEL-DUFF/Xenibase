# Writing metadata migrations

The metadata database's schema lives in `internal/store/migrations/`, one
goose file per change (`000NN_name.sql`), embedded in xenibase-server and
applied when it starts. Queries are in `internal/store/queries/`; after
changing either, run `make generate` and commit the generated Go
(`make check-generated` fails CI otherwise).

## What a running server does

- At start, `store.Migrate` takes an advisory lock, refuses a database
  whose schema is newer than the newest migration it carries, and applies
  the rest, each file in its own transaction.
- While it runs, `store.SchemaWatcher` reads the schema version every 30
  seconds. If a newer release has migrated the database since (a second
  control-plane server, or a rolling restart that started the new one
  first), the old server logs `schema check: no longer taking operations`,
  stops claiming operations (the ones it is running finish), and answers
  `/readyz` with 503 so a load balancer stops sending it requests. It keeps
  serving whatever still reaches it, and its background loops keep running,
  until it is replaced. `/healthz` stays 200: nothing is wrong with the
  process, only its version.

So for the time between the new release's migrations and the old server's
next check (up to 30 seconds), and for every request the old server still
gets after that, **the previous release's code runs on the new schema**.

## Expand, then contract

Every migration must leave a schema the previous release can still use:

- **Expand freely.** Add tables, columns with a default (or nullable),
  indexes, and constraints existing rows and the old code's writes meet.
  sqlc expands `SELECT *` into named columns when it generates, so a new
  column never breaks an older binary's queries.
- **Contract a release later.** Don't drop, rename or retype a table or
  column, or add a constraint the old code's writes break, in the release
  that stops using it. Ship the code that no longer needs it first, then
  remove it in the next release's migration. (00025, 00038 and 00052
  dropped or renamed things the release before still used; with two
  servers, the older one's queries failed until it was replaced.)
- **Data changes** (backfills, moves between tables) must be idempotent and
  safe to run while the old code writes: put right what is there, then add
  the constraint that keeps it right (00068).

## Keeping a live install writing

The tables are small next to customers' data, but some (`operations`,
`audit_log`, `metric_points`, `query_stats`) grow with the install's age.

- Adding a column with a constant default doesn't rewrite the table. A
  volatile default (`now()`, `gen_random_uuid()`) does.
- Add a foreign key or check on a large table `NOT VALID`, and
  `VALIDATE CONSTRAINT` in a later migration: validating doesn't block
  writes (00068 and 00069).
- Build an index on a large table with `CREATE INDEX CONCURRENTLY` in a
  migration marked `-- +goose NO TRANSACTION`, with `IF NOT EXISTS` so a
  rerun after a failure works (00070).
- Every file has a Down section. Tests use it to load data at an older
  version (`store.MigrateTo`); production rolls back by restoring the
  metadata backup taken before the upgrade ([upgrading](upgrade.md#rolling-back)).

## Tenant tables

A table with an `org_id` or `project_id` column is a tenant table, and the
tenant lint (`TestTenantQueriesAreScoped` in `internal/store`) finds it from
the migrations: every query on it filters by `org_id` or carries
`-- tenant: system - <reason>`. A child that carries its project's `org_id`
gets a foreign key `(project_id, org_id) REFERENCES projects (id, org_id)
ON UPDATE CASCADE`, so a project transfer moves it.
