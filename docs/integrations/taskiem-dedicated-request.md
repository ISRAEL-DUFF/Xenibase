# Taskiem production database: request for a dedicated Postgres project

> Received from Taskiem on 28 October 2026, stored as sent. Our assessment
> is in [taskiem-dedicated-assessment.md](taskiem-dedicated-assessment.md).

**From:** Taskiem
**To:** Xenibase team
**Date:** 28 October 2026

## Summary

We want Taskiem's production database to run on Xenibase. Our attempt on staging failed because a standard Xenibase project's database user cannot create roles (`CREATEROLE`), and Taskiem's schema needs to. We don't think you should grant that on a shared server, so we're asking for a **dedicated Postgres server for Taskiem**. On that server our owner user would have `CREATEROLE`, and nothing we do could touch other customers.

This is only about **Taskiem's own database**: its accounts, workflows and runs. Taskiem's existing use of Xenibase, for businesses' data tables, storefront catalogues and product photos, works and does not change.

## What happened on staging

On 20 October 2026 we pointed Taskiem's staging install at a Xenibase project database (Postgres 18).

- **The Postgres version is not the problem.** We ran Taskiem's full database test suite on Postgres 18.4, and it passed: every migration, the row-level security policies and the permission checks. CI runs on Postgres 16.
- **Creating Taskiem's roles is.** The first migration creates Taskiem's roles, and the project user was refused because it lacks `CREATEROLE`.

For now, staging runs on a Postgres 18 we host ourselves on the staging server. That's fine for test data, but not for production.

## Why Taskiem needs to create roles

Taskiem is multi-tenant, and each business's data is isolated with Postgres **row-level security**. That is enforced by the database, not just the application. The application never queries as the schema owner. Every connection switches (`SET ROLE`) to a role that has no `BYPASSRLS` and only the grants it needs.

The migrations create three group roles. None of them can log in:

| Role | Created by | Used for |
| --- | --- | --- |
| `taskiem_app` | migration 00001 | All application queries; row-level security applies |
| `taskiem_dispatch` | migration 00001 | Narrow, column-level access for authentication, queue claims and retention clean-up |
| `taskiem_public` | migration 00176 | The public host (storefronts, hosted forms, inbound mail): its own minimal grants and nothing else of the platform's |

Each is `NOLOGIN NOBYPASSRLS`. The migrations then grant table, column and function privileges to these roles. They also create `SECURITY DEFINER` functions owned by the schema owner.

The installer then creates two login roles that are members of those group roles:

| Login role | Member of |
| --- | --- |
| `<database>_app` | `taskiem_app` |
| `<database>_public` | `taskiem_public` (`NOINHERIT`) |

So the user that runs migrations must be able to:

1. **create roles** (`CREATEROLE`);
2. **grant membership** in the roles it created;
3. **own the schema** and create tables, partitioned tables, triggers and `SECURITY DEFINER` functions in it.

**Superuser is not needed.**

## Why we're not asking for `CREATEROLE` on a shared server

In Postgres, roles belong to the whole server, not to one database. On a shared Xenibase server:

- **Other customers' roles.** `CREATEROLE` on a project user would let it create roles alongside other customers' roles, and in some versions alter them. A multi-tenant platform shouldn't hand that out, and we assume that's why it was refused.
- **Name clashes.** Taskiem's role names are fixed. Two Taskiem environments, or any other product using the same names, could not share a server.

A dedicated server avoids both problems.

## What we need

### Required

| Need | Details |
| --- | --- |
| Dedicated Postgres server | A server (or cluster) for Taskiem alone, not a database on a shared server |
| Postgres version | 16 or newer; tested on 18 |
| Owner user | Owns an empty database, with `CREATEROLE`; superuser not needed |
| Direct session connections | Taskiem's workers hold a `LISTEN` connection for its task queue (`pg_notify`). A pooler in **transaction** mode breaks this; direct connections, or a pooler in **session** mode, work |
| Connections | About 20 per Taskiem process plus 5 for the public host. Initially 2 to 4 processes, so 100 to 150 connections leaves headroom |
| TLS | `sslmode=verify-full`, with the CA certificate we should trust |
| Network access | Reachable only from Taskiem's servers: private networking or an IP allowlist, never open to the internet |
| Backups and point-in-time recovery | Continuous WAL archiving, with daily base backups kept in another zone |

### Strongly preferred

| Need | Details |
| --- | --- |
| A synchronous standby in the same region, with automatic failover | Postgres is Taskiem's queue as well as its record, so a committed step should never be lost in a failover. Taskiem connects with several hosts and `target_session_attrs=read-write`, or through one address that always points at the primary |
| A self-service restore, to a new server, to a chosen time | Restores must not overwrite production. We test-restore monthly |

### Optional, later

| Need | Details |
| --- | --- |
| An asynchronous read replica | For the run list and dashboard, with `hot_standby_feedback = on` and `max_standby_streaming_delay` of about 30 s |

## Questions for the Xenibase team

1. Can you provide a dedicated Postgres server, with an owner user that has `CREATEROLE`? If so, on what plan, and at what cost?
2. Which regions are available, and is there one in or near Nigeria?
3. Which Postgres versions do you offer, and how are major and minor upgrades scheduled and announced?
4. Backups:
   - How long are backups and WAL kept, and can we choose?
   - Is point-in-time recovery self-service?
   - Can a restore go to a new server?
5. Is a synchronous standby with automatic failover available? What failover time should we expect?
6. How do we connect privately: private network, VPC peering or an IP allowlist? Is there a pooler in front, and if so, can it run in session mode or be bypassed?
7. What are the connection limit and maintenance windows, and how are maintenance windows announced?
8. Can we have a second, smaller dedicated server for staging, so staging matches production?

## Contact

Please reply to this message with answers or questions. We can do a call to go through the setup together.
