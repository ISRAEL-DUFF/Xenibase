# Taskiem's dedicated-database request: what Xenibase can do

Assessment of [Taskiem's request of 28 October 2026](taskiem-dedicated-request.md)
against the code as of this date. Not a reply to Taskiem.

**Update:** role management, the change below, is built
([Database roles on dedicated projects](../database-roles.md)). Taskiem's
migrations and installer run unchanged on a dedicated project with it on,
as long as the installer names its logins `<database>_app` and
`<database>_public`, which it does.

**Update:** the per-project IP allowlist is built too
([Network access](../network-access.md)): Taskiem lists its servers'
addresses and the poolers refuse its logins from anywhere else.

## The short answer

Xenibase covers almost everything Taskiem asks for, for any business on a
dedicated project. The exception is the one that blocked them: **an owner
that creates its own roles, including logins**. That needs a change, and
the change works for every dedicated project, not just Taskiem's.

## Where each requirement stands

| Requirement | Today | Notes |
| --- | --- | --- |
| A server for Taskiem alone | Yes | A dedicated project is its own Postgres instance on a node (`docs/user-guide.md`, `docs/capacity.md`). |
| Postgres 16 or newer | Yes | 17 and 18 (`PG_VERSIONS`). Major and minor upgrades are scheduled, announced and preflighted (`docs/upgrade.md`, V4.1-M5). |
| Owner with `CREATEROLE` | **No** | Every owner is created `NOCREATEROLE` and the attribute is re-applied on every reconcile (`internal/provision/flows.go`). The isolation audit flags any project role with it (`internal/isocheck`). See below. |
| Group roles `taskiem_app`, `taskiem_dispatch`, `taskiem_public` | Partly | A platform admin can create `NOLOGIN` integrator roles on a dedicated instance, granted to the owner `WITH ADMIN OPTION` (`internal/dedicated/integrator.go`). Taskiem's migrations would still run `CREATE ROLE` and fail on "already exists" unless they skip existing roles. |
| Login roles `<database>_app`, `<database>_public` | **No** | Integrator roles are `NOLOGIN` only, and names starting with the project's database name are refused. A login made any other way isn't in the poolers' auth file, so it can't connect through them (`internal/pooler/render.go`). |
| `LISTEN` on a session connection | Yes | The session pooler (5432) keeps one server connection per client, so `LISTEN/NOTIFY` works; the transaction pooler (6543) doesn't, as Taskiem says. |
| 100 to 150 connections | Check | The dedicated default connection limit is 90 (`internal/store/settings.go`); it's a per-plan setting. Raise it for Taskiem's plan. |
| `sslmode=verify-full` | Yes | The pooler hostname has a certificate from a public CA by ACME (or a self-signed one we'd hand over). |
| Reachable only from Taskiem's servers | Yes (built) | A per-project IP allowlist at the poolers (`docs/network-access.md`). No private peering: traffic still crosses the internet, over TLS. |
| WAL archiving, daily base backups, another zone | Mostly | WAL-G archives continuously, with configurable retention and PITR window (V4.1-M3). Copies to another zone need a copy target; with Nigerian residency on, that has to be a second in-country bucket. |
| Synchronous standby, automatic failover | Yes | HA with synchronous replication (`docs/ha.md`): nothing acknowledged is lost; writes came back in 17 to 20 s in the tests. Clients keep one address (the pooler moves the route). Needs the region's etcd cluster: 3 nodes in 3 failure domains. |
| Self-service restore to a new server at a chosen time | Yes | Restores go to a new project, in the same region; the original is untouched. |
| Async read replica with `hot_standby_feedback`, ~30 s standby delay | Partly | Read replicas exist (`docs/read-replicas.md`), but those two settings aren't exposed; Postgres's defaults apply (`hot_standby_feedback` off). |
| A smaller dedicated project for staging | Yes | Another dedicated project in the same organisation, within its allowance. |

## The one change: role management on dedicated projects

Postgres 16 changed what `CREATEROLE` allows. A role with it can only
alter, drop or grant roles it created (or holds `ADMIN` on), and can't
give anyone `SUPERUSER`, `REPLICATION` or `BYPASSRLS` it doesn't have
itself. Xenibase runs only 17 and 18. On a dedicated instance the only
other roles are Xenibase's own, which the owner holds no `ADMIN` on, so
an owner with `CREATEROLE` can't touch them. On the shared tier it stays
off: roles are cluster-wide, so names would clash and would show up in
other tenants' `pg_roles`.

What it takes, for every dedicated project rather than per ticket:

1. **An opt-in setting**, "Manage roles", on a dedicated project (owner,
   step-up, audited). It turns on `CREATEROLE` for the owner; the
   reconciler keeps it on while the setting is on.
2. **The poolers authenticate the project's own logins**, through
   PgBouncer's `auth_query` against a lookup function on the instance that
   answers only for roles the owner administers, or through an API that
   registers them. Without this, logins the owner creates can't connect.
3. **Guardrails on what the owner creates.** A sweep sets a
   `CONNECTION LIMIT` and `temp_file_limit` on new logins. The isolation
   audit accepts roles the owner administers, but still flags any with
   `SUPERUSER`, `REPLICATION` or `BYPASSRLS`.
4. **They travel with the database.** HA standbys and physical restores
   carry roles already. Logical moves, major upgrades and branches recreate
   the owner's roles before the copy, as they do for integrator roles now.
   Demotion to shared stays refused while they exist.

With that, Taskiem's migrations and installer run unchanged, and so would
any other product whose migrations create roles. Integrator roles stay
for an operator who wants to pre-create roles without opening this up.

## Smaller gaps worth closing

- ~~**IP allowlist per project** on the poolers~~: built.
- **Replica settings**: `hot_standby_feedback` and
  `max_standby_streaming_delay` as read-replica options.
- **Connection limit** on the plan Taskiem buys: at least 150.
