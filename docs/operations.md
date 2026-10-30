# Operating Xenibase

Day-to-day running of a Xenibase install. Commands run in `deploy/compose`.

## Health at a glance

- **Alerts** (sidebar) lists what is wrong now; the same alerts go to your
  webhook and email (Settings → Alerts). Conditions are evaluated every
  `XENIBASE_ALERTS_INTERVAL` (30 s). Each alert is sent when it fires and
  when it resolves:

  | Alert (kind) | Severity | Fires when |
  | --- | --- | --- |
  | Backup failed (`backup_failed`) | critical | A live project's latest backup or base backup failed, or the metadata database's nightly backup did |
  | Backup overdue (`backup_overdue`) | warning | A project older than 26 h has no successful backup in 26 h (paused and archived projects, branches without backups and suspended organisations, which the nightly schedule skips, are left out) |
  | Restore test failed (`restore_test_failed`) | critical | For a live project tested on demand (`POST /api/v1/restore-tests?project_id=`), its latest test failed; otherwise the latest scheduled (weekly) restore test failed. A deleted project's failed test does not alert |
  | WAL archiving (`wal_archive`) | critical | A dedicated project's WAL archiving failed in the last 15 minutes, or a finished WAL segment has waited over 15 minutes to be archived: recovery past it is impossible, and WAL fills the volume. Check the archive's storage target and credentials, and the instance's log for `wal-push` |
  | Isolation check (`isolation_check_failed`) | critical | A live shared cluster's latest tenant-isolation check (every 24 h) found a problem |
  | Minor upgrade failed (`minor_upgrade_failed`) | critical | A live instance's latest Postgres minor upgrade failed; it stays on its release until a newer one or **Run now** |
  | Project stuck (`project_stuck`) | critical, or warning for `error` | A live project is in a status other than `active` with no queued or running operation, 10 minutes after its last one ended. `moving`, `promoting`, `demoting` or `upgrading`: the rollback can't give its logins back until its instance answers, and Xenibase retries every minute. `provisioning`, `restoring` or `deleting`: the operation ended without finishing, and nothing repairs it (see [Projects stuck in a status](#projects-stuck-in-a-status)). `error`: a delete or restore gave up; delete the project once the cause is fixed |
  | Operations waiting (`queue_age`) | critical for interactive, warning otherwise | The oldest runnable operation of a class has waited over 15 minutes (interactive) or an hour (long, scheduled): every slot that may run it is busy, or no server runs its kind (see [The operations queue](#the-operations-queue)) |
  | Node unreachable (`node_unreachable`) | critical | A node's agent has not answered for 2+ minutes |
  | Node disk (`node_disk`) | warning | A reachable node's data disk is over 85% full, from its last health report |
  | Project disk (`project_disk`) | warning | A project is larger than its disk warning (Project Settings → Database), from a size sample of the last 30 minutes |
  | Pooler down (`pooler_down`) | critical | A local PgBouncer's admin console has not answered for 1 minute |
  | Pooler allowlist off (`pooler_allowlist_off`) | critical | A local PgBouncer is not on `auth_type = hba`: IP allowlists are stored but not enforced |
  | Pooler configuration skipped (`pooler_config_skipped`) | critical | The last pooler sync left rows out as invalid (often a node's private address), so those projects or logins can't connect through the poolers |
  | Pooler host not ready (`pooler_host_not_ready`) | critical | A healthy edge pooler host can't serve or is stale for 1 minute ([standby edge pooler](edge-poolers.md)) |
  | Pooler split brain (`pooler_split_brain`) | critical | Two edge pooler hosts in one region say keepalived made them MASTER, for 1 minute |
  | Waker down (`waker_down`) | critical | The Free tier's waker has refused connections for 1 minute: paused projects can't resume on connect, and pausing is on hold |
  | Agent client certificate (`agent_client_cert`) | critical | xenibase-server's certificate for talking to agents (30 days, renewed with 7 left) has under 3 days left: renewing it keeps failing (see the server's log) |
  | Failure domain (`failure_domain`) | warning | HA members, etcd members or pooler hosts that must be apart share a failure domain |
  | Files share backups (`files_share_backups`) | warning | A region with stored files has no files target of its own, so its edges hold credentials that can delete its backups |
  | Capacity proposal (`capacity_proposal`) | warning | A capacity proposal waits for approval (Platform → Capacity) |
  | Capacity failed (`capacity_failed`) | critical | Provisioning a server failed in the last 24 hours |

  An alert resolves after 3 evaluations in a row (from any server, about a
  minute) find its condition clear; if it holds again before that, it is
  the same alert, so a value hovering at a threshold does not fire and
  resolve at every crossing. If one of the sources cannot be read (a
  failing query), what the others report still fires, but nothing
  resolves that round. The grace periods above are shared by every server.
  Resolved alerts are deleted after 90 days.

  Delivery is recorded per channel: if the webhook or the SMTP account
  fails, only that channel is retried (after 1 minute, doubling to 30), for
  7 days from the event; the other channel is not sent it again. An alert
  that resolves before its firing could be delivered is announced by its
  `alert.resolved` notification alone, which carries `started_at` and
  `resolved_at`.

- **Operations** shows every long action with its step log.
- **Metrics** (per project and per node) charts size, connections, TPS,
  cache hit ratio, CPU, memory, disk, and I/O; `/metrics` serves them to
  Prometheus.
- `docker compose ps` and `docker compose logs <service>` for the
  containers themselves. Their logs are rotated: every service in the
  compose files (here, the status page's and the pooler hosts') uses
  Docker's `json-file` driver with `max-size: 50m` and `max-file: 5`, so a
  container keeps at most 250 MB and `docker compose logs` reaches back
  that far. Ship them elsewhere if you need longer. A container created
  before this setting picks it up the next time it is recreated
  (`./install.sh` or `docker compose up -d`).

## The operations queue

Every long action is an operation, run by the server's workers
(`XENIBASE_WORKERS`, 4 by default per server). Each kind has a class:

| Class | Kinds | Slots per server (of 4) |
| --- | --- | --- |
| Interactive | Create, delete, restore, rotate, settings, pause and resume, unarchive, branches, services, HA disable and switchover, etcd, capacity, and any kind not listed below | All of them; claimed first |
| Long | Moves, major upgrades, promotion and demotion, HA enable, creating and detaching replicas, resizes, imports, Supabase user and storage copies, branch file copies, Move all | At most half (2) |
| Scheduled | Backups and base backups, restore tests, the metadata backup, storage switches, isolation checks, minor upgrades, the Free tier's pause and archive, V1 renames and credential expiry | At most all but one (3) |

Long and scheduled operations together hold at most all but one slot, so
one is always free for interactive work, and a server claims interactive
operations before older ones of the other classes. With
`XENIBASE_WORKERS=8`: 4 long, 7 scheduled, 7 together; with 1, nothing is
kept back. More servers add slots: each applies the caps to its own.

`queue_age` fires when the oldest runnable operation of a class has
waited past its threshold. For interactive work, look at what the
servers run (Operations, filtered by `running`): a burst of creates, or
one server for many projects, calls for more workers or another server.
For scheduled work during the backup window, a backlog of backups is
expected for a while; one that lasts over an hour means the window needs
more workers. An operation of a kind no server runs (a server of another
release) waits too, and fails after a day.

### Projects stuck in a status

`project_stuck` names a live project left in a status other than `active`
that no operation will move on. Read its last operation's log
(Operations, or the project's own list) first.

- `moving`, `promoting`, `demoting`, `upgrading`: Xenibase gives the
  logins back, resumes the poolers and sets the project `active` once its
  instance answers. Bring the instance's node back.
- `error`: a delete gave up, or a restore could not put the safety
  backup back. Fix the cause the log names and delete the project again
  (a branch can also be reset).
- `provisioning`, `deleting`: the create or delete ended without setting
  the project's final status. There is no button for these: in the
  metadata database set the project's status to `error`, then delete it,
  which drops whatever the create left (a delete finds what is already
  gone).
- `restoring`: the restore ended without reopening the project. If the
  log shows the restore, or its rollback, finished and the database is as
  expected, set the status to `active` (and `RESUME` its database on both
  PgBouncers if clients are refused); otherwise restore one of its backups
  into a new project, then set it to `error` and delete it.

## Backups

- Logical backups of every project run nightly (02:00 UTC plus up to 2 h of
  jitter); dedicated instances also take daily base backups and archive WAL
  continuously (point-in-time recovery for 7 days, or 14 or 30 as an
  add-on).
- Retention: 7 daily backups and 4 weekly ones, or longer as an add-on
  (extended: 30 daily, 12 weekly; long: 30 daily, 52 weekly); final
  backups of deleted projects are kept 30 days, and a deleted project's
  earlier backups on platform targets go with its final backup (those in
  an organisation's own bucket are left alone). See
  [billing add-ons](billing.md#add-ons).
- Weekly, a random project's latest backup is restored into a scratch
  database and verified (Settings → Backup checks). Every other week a
  random dedicated project is instead recovered from its base backup and
  WAL to the present, into a scratch instance on its node (the smallest
  size, no project, nothing archived), every table is read, and the
  instance is removed; the node needs twice the instance's data free.
  `POST /api/v1/restore-tests?project_id=<id>` runs one now; for a
  dedicated project it tests the point-in-time recovery.
- The metadata database backs itself up nightly to `<prefix>/metadata/`.
- Restores: a project's Backups tab → **Restore**, into a new project or in
  place (in place takes a safety backup first and needs re-authentication).

### Storage targets and project keys

Backups go to a **storage target**, an S3 bucket and prefix. **Platform
targets** (Settings → Platform storage targets) are yours to run; exactly
one is the default, used by every project that hasn't chosen another, and
backup storage on them counts against each organisation's
`backup_storage_mb` quota (manual backups beyond it are refused; nightly
ones still run). **Org targets** (Organisation → Backup storage, owners
and admins) are buckets an organisation brings itself: its projects can
use them, the storage doesn't count against its quota, and neither other
organisations nor you see them (the admin organisation list only counts
the projects using one). Adding or editing a target writes, reads, lists
and deletes a test object under its prefix first; credentials are sealed
with the master key and never shown again. Two targets can't share an
endpoint, bucket and prefix (give each its own prefix): a copy between
them would be the original, and deleting the original would delete both.

A project admin picks the target under **Backups → Storage**. New backups
go there; existing ones stay where they are and stay restorable, or can
be copied over (verified by checksum, originals deleted only if asked).
For a dedicated project the switch restarts the instance with WAL-G
pointed at the new target and takes a base backup at once; the previous
archive stays restorable for the project's recovery window (7 days, or the
14 or 30 it pays for), then goes.

**Use a project key** gives a project its own backup key: new backups are
OpenPGP messages to it, and on the dedicated tier WAL-G uses it too.
**Download key…** needs re-authentication, is audited, and gives a file
with a README on restoring with gpg and pg_restore alone
([disaster recovery](disaster-recovery.md#restoring-a-project-without-xenibase)).
A target can't be deleted while a project uses it; one that still holds
backups is deleted only when you accept that they become unrestorable. A
project whose backups are on its organisation's target moves to platform
storage before it can be transferred to another organisation.

## Security checks

Every shared cluster is checked nightly against the tenant-isolation
checklist (Settings → Tenant isolation, **Check now** to run it at once):
cluster settings and `pg_hba.conf`, every project's roles (its backend
services' logins too), who may connect to its database, and its database
(including `SECURITY DEFINER` functions that run as anyone but the
tenant), and two throwaway tenants that try to reach each other, discover other
projects' names, get round their quotas, or log in while suspended. See
[security review](security-review.md).

## Users and organisations

Every user has a personal organisation and can be invited into others.
Organisation owners and admins manage members and see every project in
it; members see only the projects they are added to, as admin, developer,
or read-only. Each member gets their own database login per project
(Members → **Get my credentials**): `<db>_u_<id>`, read/write for admins
and developers, read-only for read-only members. Removing someone from a
project or organisation drops their logins and ends their connections at
once; the app's own `<db>_owner` password is never shared, so it never
needs rotating when people leave.

## Platform admins and recovery

The first account, made in the setup wizard, is the platform admin. Any
platform admin can make another account an admin (Admin → Users → **Make
admin**) or take the role away (**Remove admin**). Both ask for your
password and an authenticator code again. The account must be active,
approved, verified and have two-factor set up; it is signed out and emailed
when its role changes, and the last active admin can't be removed. Keep at
least two admins, so losing one phone never locks you out.

If nobody who is an admin can sign in, recover from the server, where you
already hold the metadata database and `.env`:

```sh
docker compose exec xenibase-server xenibase-server admin list
docker compose exec xenibase-server xenibase-server admin promote you@example.com
docker compose exec xenibase-server xenibase-server admin reset-2fa you@example.com
docker compose exec xenibase-server xenibase-server admin reset-password you@example.com
```

`promote` also approves, verifies and re-enables the account (it need not
have an authenticator yet; its next sign-in sets one up). `reset-2fa` removes
the authenticator and recovery codes, ends the user's sessions and revokes
their API tokens: a platform admin sets up a new one at the
next sign-in, and anyone else signs in with a password until they turn
two-factor on again (Account → Security).
`reset-password` prints a one-hour, single-use link for when email doesn't
work. `demote` makes an admin an ordinary user, and still refuses to remove
the last one. Each change is written to the platform audit log as done on the
server. Anyone who can run these commands already controls the database, so
guard that access like the master key.

As platform admin you manage accounts (Users: approve, disable, reset
two-factor after checking who they are), email, sign-up, and the terms.
You do not see into other people's organisations through the UI. The
audit logs are per project, per organisation (Audit log), and for the
platform (Platform audit).

Projects created before V2 are in the platform admin's personal
organisation, unchanged: same URLs, passwords, and backups.

## The audit log

Every mutating API request, and the downloads of backups and backup keys,
writes an audit row when the handler returns. The row is written after the
change, not in the same transaction, so what is guaranteed is this:

- If the metadata database refuses the insert (a failover, a full disk), the
  row is kept in xenibase-server's memory and retried every 5 seconds, in
  order, with the time of the request, until it is written.
- Up to 10,000 rows wait. Past that the oldest is given up on, and a row
  given up on (also any still waiting when the server stops) is logged at
  error level as `audit row not written` with the whole row as JSON, so it
  can be put back by hand.
- `/metrics` counts it: `xenibase_audit_write_failures_total`,
  `xenibase_audit_pending`, `xenibase_audit_retried_total` and
  `xenibase_audit_dropped_total`. These are the process's own and are served
  while the database is down. Alert on `xenibase_audit_dropped_total > 0`,
  and on `xenibase_audit_pending > 0` for more than a few minutes.
- A crash of xenibase-server (or the machine) loses the rows still waiting.

## What is kept

How long the metadata database keeps what it records:

| Data | Kept |
| --- | --- |
| Metrics | 1-minute points 24 hours, hourly points 30 days |
| Query insights (statistics, slow queries, query texts) | 30 days |
| Reaped statements (with the statement's text) | 30 days, and removed within 5 minutes of the project's deletion |
| Data API request logs, edge reports | 7 days |
| Webhook deliveries | 7 days; dead letters 30 |
| Scheduled job runs | 30 days, at most 1,000 per job |
| Operations | A finished operation's parameters and step log for 90 days; then the row stays as a summary (kind, project, status, error, who and when) |
| Audit log | For good (append-only) |
| Projects and organisations | For good, marked deleted |

The tenancy sweep (every 5 minutes) removes reaped statements and prunes up
to 500 operations at a time (about 144,000 a day), so the backlog an
upgraded install starts with clears gradually.

## Quotas, storage locks and usage

Each organisation has a plan (Organisations → the org → Plan), with
per-key overrides; Plans lists and edits them. Creating projects, backups,
restores and console queries past a limit fails with the limit named.
Owners and admins see their use on **Usage & quotas**, with hourly
storage and a CSV export; you see every org's on the admin usage API.

Shared-tier projects are measured every minute:

- **90%** of the project's storage limit: a warning email and banner.
- **100%**: read-only by default. Apps can still delete in a read-write
  transaction (`BEGIN READ WRITE`), and the SQL console works.
- **120%** (or 100% with the node's disk 95% full), or growth of 5% of
  the limit while read-only (writes in read-write transactions): apps
  cannot log in. The console and table browser still work; delete data
  there, then **Reclaim space** (Project Settings → Database) to give the
  space back.

Projects at 90% or over are measured on every check (every minute), not
only from the 5-minute size sample, so one that keeps writing is locked
within a minute. Locks lift at the next check once the project is under
the limit; a project whose logins are off stays so until then.
Statements running over 10 minutes are cancelled, a statement that keeps
running after its cancel (PL/pgSQL can catch it) is ended with its session
15 seconds later, and transactions idle over 5 minutes are ended; the
project's Metrics page lists them.

Dedicated instances beyond an org's allowance become requests (Dedicated
requests) for you to approve or reject. An org can be given its own
shared cluster (Organisations → the org → Shared cluster): its new
projects go only there, and nobody else's do.

## Suspension, break-glass and deletion

**Suspend** (Organisations → the org, with a reason) stops the org's
projects accepting connections and its backups, and makes the org
read-only for its members; **Reinstate** undoes it. **Outbound access**
can be turned off on its own, without suspending (see Outbound traffic).

To look inside an org (a support case), open a **break-glass** session
with a reason and a length (up to 4 hours). You act as an org admin; its
owners and admins are emailed, every action is in its audit log marked
break-glass, and they see a banner. End it when you are done. A
break-glass session can't invite anyone (you included), change a
member's organisation or project role, or transfer ownership: it grants
nothing that outlives it, so ask an owner for those.

Owners can delete an organisation (Organisation → Delete). It is
read-only for 7 days and can be cancelled; then each project gets a final
backup, kept for 30 days, and is deleted.

## Opaque names

New projects' databases and roles are named `p_<random>`, so other
tenants cannot learn them. Projects from before V2 are renamed on the
server side and keep their old name as an alias, so their URLs keep
working; their role name stays visible until an admin uses **Switch to
opaque credentials** (Project Settings → Database), which issues new URLs and keeps the old
ones working for a grace period. Members' personal database logins
(`<database>_u_<member>`) are renamed at once, with the same password: their
old user name stops working, so members copy the new connection string.

## The SQL Editor

The **SQL Editor** is laid out as Supabase Studio's. Queries run against
the live database as the project's owner role, at most 1,000 rows shown
per statement, with a 30-second timeout unless you pick a longer one; run
read-only from the role picker, and read-only members and projects whose
console is read-only always do. ⌘↵ runs the selection or everything,
⌘⇧F formats, and the editor completes schema, table and column names.
Long queries can be cancelled; errors are marked where Postgres found
them; results export as CSV or JSON.

Queries are saved as you type. Each member keeps their own (**Private**),
can share one with everyone who can use the project's console
(**Shared**), and marks favourites for themselves. Only a query's owner
edits it (others duplicate it to change it); project admins may also
delete shared ones. Saved queries are deleted with the project and move
with it to another organisation. The editor's run history stays in the
browser.

## The table editor

Project members edit data and schema in the **Table Editor**, laid out as
Supabase Studio's (developers and above; read-only members browse and
export). The sidebar lists the schema's tables, views and other objects,
each with a menu to edit, duplicate, copy the name of, export or delete
it; open tables stay as tabs. Rows: **Filter** and **Sort** (several
columns), pages of 100 to 1,000 with the record count (an estimate on big
unfiltered tables), and **Definition** shows the table's DDL.
Double-click a cell to edit it: the change saves at once, checked
against the column's type, and if someone changed the row since you
loaded it you get a "Someone changed this row" message instead of
overwriting their change. JSON cells open a larger editor; foreign-key
cells link to the row they reference. **Insert** adds a row or a column
from a side panel; select rows to delete them. Tables need a primary key
to be editable.

Schema changes (new table, edit table, add or edit a column, with
foreign keys, unique and check constraints) are made in side panels and
then reviewed: every change shows its SQL and risk notes first, such as
"Rewrites the table and blocks writes" for a type change, and runs with
a 5-second lock timeout, so it fails fast rather than queueing behind a
long transaction. Editing a table runs all its changes in one
transaction. Indexes build concurrently. Schema changes are in the
project's audit log with their SQL. **Save as migration** exports the
change as plain SQL, goose or dbmate, to apply to other environments.

## Branches

A branch is a throwaway copy of a project on the shared tier (on the
organisation's own shared cluster if it has one), from its latest backup
or live, schema only or with data. Developers and above create them under
**Database → Branches** or with `xenibase branch create`; they expire after
7 days unless given another TTL (1 hour to 30 days, or kept), the
creator is emailed a day before, and an hourly job deletes expired ones
without a final backup. **Reset** refills a branch from its parent while
keeping its database name, URL, password and members' logins;
**Detach** turns it into a standalone project (which can then be
promoted). A parent can't be deleted while it has branches, and a
branch can't have branches of its own.

Branches count toward the organisation's branch quota (10 on Personal,
25 on Team) but not its projects, and toward its shared storage; usage
records branch-hours and branch GB-hours. They take no nightly backups
unless a project admin turns them on (**Project Settings → General → Data**). Mark a
project **Contains sensitive data** (or make it the organisation's
default under **Organisation → Projects**) and its branches copy the
schema only, unless a project admin asks for the data. Webhooks and
scheduled jobs are never copied.

## API tokens and the CLI

Members use the `xenibase` CLI ([CLI guide](cli.md)) and API tokens for
scripts and CI. Tokens act in one organisation with read, write or admin
scope, optionally restricted to some projects, and expire within a year.
Set a lower maximum under **Settings → API tokens**. Organisation owners
and admins revoke their members' tokens under **Organisation → API
tokens**; suspending an organisation disables its tokens, and removing
someone revokes theirs. You can't see other people's tokens, and a
platform admin's own tokens can't manage the platform: use the browser
for that.

Third-party apps that connect organisations through OAuth
([OAuth for third-party apps](oauth-apps.md)) are registered by a platform
admin with `POST /api/v1/admin/oauth/clients` from a browser session (there
is no page for it yet); `GET` lists them and `DELETE …/{client_id}`
disables one, revoking every token it holds. A consent produces an ordinary
token, so the token rules above apply to it; users disconnect an app under
**Account → Connected apps**.

## Capacity

- The shared cluster's tuning is in `compose.yaml` (`SHARED_PG_SHARED_BUFFERS`,
  `SHARED_PG_EFFECTIVE_CACHE_SIZE` in `.env` for other host sizes). Each
  shared project gets a pool of 5 server connections in transaction mode and
  up to 20 backends; the load test ([load test](load-test.md)) ran 150
  projects with ten busy ones on 4 vCPU.
- When a project outgrows the shared tier (sustained load, a large
  database), **promote** it (Project Settings → Compute and tier): same URL,
  a short write freeze.
- When it no longer needs its own instance, **demote** it (Project Settings →
  Compute and tier → Move back to shared, or `xenibase demote`). The preflight checks the size
  against the organisation's shared storage limits, extensions against
  the shared allow-list, custom roles, peak connections, database
  settings that will reset, and a shared cluster with room for the
  database plus 20% (the organisation's own when it has one). The URL and
  every password stay the same; the guardrails go back to the shared
  defaults, the SQL console stays read-only unless asked, and
  point-in-time recovery ends: the project takes a logical backup at
  once and nightly after that, while the old base backups stay restorable
  for 7 days. The dedicated instance is stopped and kept for 48 hours as
  a rollback option (an operator can start it again from its volume),
  then destroyed by the hourly cleanup, which releases it from the
  organisation's dedicated allowance.
- Add nodes on the Nodes page; new shared projects go to the least loaded
  shared cluster, dedicated instances to the least loaded dedicated node.

## Pooler configuration

xenibase-server writes the poolers' routes, logins and `hba.conf` from the
metadata database on every change. An entry PgBouncer can't be given safely
(a node address with an underscore, a malformed name) is left out rather
than escaped, with an error in the log and the `pooler_config_skipped`
alert naming it; everything else is still written. That project (or login)
can't connect through the poolers until the row is fixed. A project whose
allowlist couldn't be written loses its routes too, so it is never opened
to everyone by mistake. Node addresses are checked when a node is added:
a host name of letters, digits, dots and dashes, or an IP address.

## Outbound traffic

Webhooks and HTTP jobs are the only way tenants make Xenibase send requests
([webhooks and scheduled jobs](webhooks.md)). Before each request
xenibase-server resolves the host and refuses private, loopback, link-local,
CGNAT and cloud-metadata addresses, then connects to the address it
checked, without following redirects. Add your nodes' network (and any
other internal range) with `XENIBASE_OUTBOUND_BLOCK=10.0.0.0/16,192.0.2.0/24`
if it isn't in a private range already.

On an organisation's admin page you can:

- **Allow-list internal hosts** for that organisation alone (a receiver on
  your own network, a local test server): listed hosts may resolve to
  private addresses and use plain `http://`. Link-local and metadata
  addresses can never be allowed. Tenants can't change the list.
- **Disable outbound traffic** without suspending the databases: webhook
  events queue and HTTP jobs are skipped until it is back on.
- See the organisation's requests per destination host over the last 30
  days (hosts and counts only, no payloads).

Only one xenibase-server delivers webhooks and runs the scheduler at a time
(advisory locks), so more servers can be added later without double
deliveries.

Imports and Supabase migrations connect to a database the tenant names, from
xenibase-server and an agent, whose addresses every cluster trusts. Their
source goes through the same rules, and stricter ones: the host must resolve
to public addresses (or be allow-listed for the organisation, as above), and
loopback, link-local, the nodes' and instances' own addresses,
`XENIBASE_OUTBOUND_BLOCK` and, with machines Xenibase creates,
`XENIBASE_CLOUD_PRIVATE_CIDR` are refused even when allow-listed. The server
and the agent's copy connect to the address that was checked, so the name
can't be rebound in between, and a connection string may name only one host
and port (no `host=` parameter). A preflight passes if any of the caller's
organisations allows the host.

## Secrets

- `.env` holds `XENIBASE_MASTER_KEY`; keep a copy off the server. Only its
  owner may read it: `install.sh` writes it mode 600, and on every run sets
  an existing one (for example copied by hand from `.env.example`) back to
  600, with a warning. `upgrade.sh` writes its metadata dumps mode 600 in a
  mode-700 `upgrade-backups/`.
- **Rotate the master key**: generate a new one, run the rotation, then
  switch over:

  ```sh
  new=$(docker compose exec -T xenibase-server xenibase-server -gen-master-key)
  old=$(grep '^XENIBASE_MASTER_KEY=' .env | cut -d= -f2-)
  docker compose stop xenibase-server
  docker compose run --rm -e XENIBASE_MASTER_KEY="$new" -e XENIBASE_MASTER_KEY_PREVIOUS="$old" \
    xenibase-server -rotate-master-key
  sed -i "s|^XENIBASE_MASTER_KEY=.*|XENIBASE_MASTER_KEY=$new|" .env
  docker compose up -d
  ```

  The rotation re-encrypts everything in one transaction and checks the new
  key alone opens it all; on any error nothing changes. Operators sign in
  again afterwards. Back up the new `.env`.
- The passwords Xenibase derives instead of storing (the backend services'
  edge and request logins, the console logins, the keys that sign file
  URLs, the pooler admin's salt) come from a **derive seed**, sealed under
  the master key in the `derive_seed` settings row. A rotation re-seals it,
  so they do not change and backend services keep working. The first
  server of this release to start makes the seed: on an install with
  projects it is the master key in use then, so nothing changes on upgrade
  either. If you
  rotate before any server of this release has started, the rotation makes
  the seed from the old key, and needs `XENIBASE_MASTER_KEY_PREVIOUS` to
  hold that one key only.
- Releases before this one skipped part of what is sealed (per-project
  backup keys, Patroni secrets, webhook secrets and headers, scheduled jobs,
  projects' auth SMTP settings and queued messages, device sign-in tokens,
  the etcd CA key) and still reported success. If you rotated with one of
  them, those values still need the old key: keep it in
  `XENIBASE_MASTER_KEY_PREVIOUS` (or put it back) and run the rotation again
  with this release before you drop it.
- The backup key never changes (old backups need it); keep it offline.
- `XENIBASE_CONSOLE_DISABLED=true` turns the SQL console and table browser
  off for every project.

## Upgrades and disaster recovery

- [Upgrading](upgrade.md)
- [Disaster recovery](disaster-recovery.md): rebuild the control node,
  restore the metadata, re-register agents, bring back project data.
