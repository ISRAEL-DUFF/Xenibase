# Database roles on dedicated projects

Some applications create their own Postgres roles in their migrations:
group roles for row-level security, and logins that use them. Taskiem's
first migration, for example, creates `taskiem_app` and `taskiem_dispatch`,
and its installer creates `<database>_app` as a login in `taskiem_app`.
A project's owner role can't do that by default (`NOCREATEROLE`).

On a **dedicated** project, an organisation owner can turn on **role
management**: Project Settings → Compute → **Database roles** → **Let the
owner create roles**, or

```http
PUT /api/v1/projects/{id}/manage-roles
{"enabled": true}
```

It needs a fresh step-up (password, and a code if two-factor sign-in is on),
and it's audited. The owner role then has `CREATEROLE`.

## What the owner can and can't do

Xenibase runs Postgres 17 and 18. Since Postgres 16, a role with
`CREATEROLE`:

- creates roles, and alters, drops and grants **only the roles it created**
  (it holds `ADMIN OPTION` on them);
- can't give anyone `SUPERUSER`, `REPLICATION` or `BYPASSRLS`, or membership
  in Postgres's predefined roles (`pg_read_server_files` and the like);
- can't touch Xenibase's own roles on the instance (the superuser, the
  project's read-only, console and backend-services roles, members' logins).

The instance is the project's alone, so this reaches nothing of another
customer's. Shared projects can't have it: roles are cluster-wide on a shared
cluster, so names would clash and show up in other tenants' `pg_roles`.

## Logins and the poolers

The poolers accept only logins Xenibase knows. Every 30 seconds Xenibase
reads the roles the owner administers and syncs the logins that:

- are named **`<database>_<name>`**: the project's database name, an
  underscore, then lowercase letters, digits and underscores (Taskiem's
  `<database>_app` and `<database>_public` are);
- aren't one of the project's own names (`_owner`, `_ro`, `_console`,
  `_console_ro`, `_restore`, `_sla`, `_edge`, `_anon`, `_user`, `_service`, `_auth_hook`,
  `_u_…`);
- have a SCRAM-SHA-256 password (the default).

Each synced login is allowed to connect to the project's database and gets
the project's **connection limit**; a higher or unlimited one is put back on
the next sweep. To connect at once after creating a login, sync:

```http
POST /api/v1/projects/{id}/manage-roles/sync
```

The answer lists the synced logins and, for the owner's other logins, why
each isn't synced. Use the **session** pooler (port 5432) for `LISTEN` and
other session state; the transaction pooler (6543) works for ordinary
queries.

While the organisation is suspended or the project's storage is
hard-locked, the poolers stop accepting these logins, as they do the
project's own.

## Moves, upgrades and backups

- **Moves and major upgrades** recreate the owner's roles on the new
  instance before the copy: the same attributes and password, the owner's
  admin option, and the memberships between them.
- **HA standbys and restores** are physical copies, and carry the roles.
- **Demotion to the shared tier** is refused while roles other than the
  project's own exist.
- Branches don't carry the owner's roles.

## Turning it off

`{"enabled": false}` removes `CREATEROLE` from the owner. Roles it already
created stay, and their logins keep connecting; drop them first if they
should go.
