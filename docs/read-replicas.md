# Read replicas

A dedicated project can have up to two read replicas (V4 §7). A replica is
a streaming copy of the database on another node. It serves reads, is never
promoted, and never holds up a commit. The pooler's read-only route spreads
reads across the replicas that keep up. When the primary changes (a
failover or a switchover), the replicas follow the new one.

## Adding one

Go to Project → Settings → Read replicas → Add replica, or run:

    xenibase replicas create my-app [--node <id>] [--region <region>]

The replica goes on the least loaded eligible node unless you pick one. An
eligible node is healthy, takes dedicated instances, and doesn't already
hold a copy of this database (the primary, its HA standby, or the other
replica). By default the replica stays in the project's region. With
`--region` you can put it in another region, closer to the readers there.
A project with data residency keeps its replicas in its own region.

If the project isn't under Patroni yet (it has never had HA), the database
restarts under Patroni first, as when HA is turned on. Clients on the
pooled URL wait a few seconds. The replica is then built from the newest
base backup and streams from the primary.

The operation is `create_replica`. A replica that never starts streaming is
removed, and the project carries on as before.

## Reading from replicas

The read-only route is the project's database name with `_ro` appended:

    postgresql://<owner>:<password>@db.<region>.<domain>:6543/<db>_ro

The dashboard and `xenibase replicas list` show the full URL. It takes the
project's usual logins.

- Each new server connection goes to the next replica in rotation.
- Every transaction is read-only. A write is refused with "cannot execute
  … in a read-only transaction".
- When no replica is in rotation (all lagging, or all down), the route goes
  to the primary, still read-only, so reads keep working.
- Replicas trail the primary. A client that must read its own writes uses
  the normal route.

### The data API

Data API GET requests (`/data/v1/…`) go to the replicas when either:

- the request sends `Read-Replica: allowed`, or
- the project sets **Read from replicas by default** (Settings → API,
  `replica_reads` in the services settings). This applies to
  publishable-key requests that send no header.

`Read-Replica: primary` keeps a request on the primary. Auth, storage and
realtime always use the primary: some of their GETs write (a verification
link, an OAuth callback). `GET /data/v1/health` reports `"replica": true`
when a request was served by a replica.

## Lag and rotation

Xenibase measures each replica's lag every second, against the primary. The
lag is how long ago the primary wrote the WAL the replica has not replayed
yet. So a replica that stops replaying shows a growing lag even though the
primary's own `replay_lag` would freeze.

- A replica more than 10 seconds behind, or not streaming, leaves rotation
  (status `lagging` or `down`).
- It comes back once it is within half the threshold. The gap between the
  two limits stops it flapping in and out.
- The dashboard shows each replica's lag in time and bytes, and when it
  last entered or left rotation.

Operators set the threshold with `ReplicaMaxLag` in the dedicated service
configuration (tests use 3 seconds).

## Long queries on a replica

A replica replays the primary's changes while it answers queries, and the
two can collide. For example, a report on the replica is still reading rows
that the primary has deleted and its vacuum has since removed. Replay then
either waits for the query or cancels it, and the application gets
`ERROR: canceling statement due to conflict with recovery`. Two settings,
set per replica, decide which:

| Setting | Default | What it does |
| --- | --- | --- |
| Hot standby feedback | off | The replica tells the primary which rows its running queries still read, and the primary's vacuum keeps them. Most conflicts never happen. The cost is on the primary: dead rows stay longer, so one very long query on the replica can bloat tables there. |
| Wait for conflicting queries | 30 s | How long replay waits for a conflicting query before cancelling it: 0 to 900 seconds. It sets `max_standby_streaming_delay` and `max_standby_archive_delay` together. Longer lets long queries finish, but the replica falls behind while it waits. |

Set them when you add a replica, or later with **Settings…** on its row
(Project Settings → Compute → Read replicas), or through the API:

```http
PATCH /api/v1/projects/{id}/replicas/{replica_id}
{"hot_standby_feedback": true, "max_standby_delay_seconds": 30}
```

From the CLI: `xenibase replicas set <project> <replica-id> --feedback on
--standby-delay 30`, or the same flags on `replicas create`.

A change applies to that replica alone, at once, with a configuration
reload: no restart, and open connections stay open. The primary and HA
standbys keep Postgres's defaults. If the replica can't be reached (it is
down or restarting), the settings are stored and applied when it is back.
`settings_error` on the replica says why it is waiting. Xenibase checks
every replica's settings every five minutes and puts back any that drifted,
for example after the replica is rebuilt from a backup.

A replica waiting for a long query falls behind, and once it is past the
rotation threshold (10 seconds) it leaves the read route until it catches
up. Reads then go to the other replica or the primary. With a wait longer
than the threshold, expect a replica that runs long reports to leave
rotation now and then while they run.

Detaching a replica resets both settings: the new project starts with
Postgres's defaults.

## Failover

Replicas are Patroni members tagged `nofailover` and `nosync`:

- Patroni never promotes a replica.
- A replica never becomes the synchronous standby.
- Switchovers skip replicas, and so does the maintenance window's minor
  upgrade, which restarts the replicas but switches over only to the HA
  standby.

When the primary changes, the replicas stream from the new one with no
action from you. Their pooler addresses don't change, so the read-only
route keeps working throughout. Each replica leaves rotation only for as
long as it lags.

## Detaching

Detaching promotes a replica into a new, standalone dedicated project, for
analytics or a migration:

    xenibase replicas detach my-app <replica-id> --name "my-app analytics"

1. The replica leaves the read route and the project's cluster.
2. It restarts as an ordinary instance on its own data and is promoted.
3. It takes the new project's database name, owner and password, as a
   point-in-time recovery does.

The new project's password is shown once. The source project is not
changed. The operation is `detach_replica`. If it fails, the replica is
removed; create a new one if you still need it.

## Removing one

    xenibase replicas delete my-app <replica-id>

The replica leaves the read route first, then its container and volume are
removed (`delete_replica`). Removing the last replica removes the `_ro`
route.

Before a node move, a major upgrade, a demotion, or a move of the
project's Patroni state to another region's etcd cluster, delete the
replicas. Those operations rebuild the instance, and they refuse to run
while replicas exist.

## Billing

Each replica bills like a dedicated instance of the primary's size, by the
hour it exists. The usage metrics are `replica_hours`, `replica_vcpu_hours`,
`replica_ram_gb_hours` and `replica_disk_gb_hours`. Creating a replica
checks the organisation's dedicated allowance, as creating an instance
does.

## Placement and ports

A host list in PgBouncer takes one port, so all of a route's hosts must
share it.

- Where agents publish members on the node's address (one node per host),
  an instance's replicas ask for the same port, derived from the
  instance. The rare replica that can't get it is reached on its own port
  and, if it is outnumbered, left out of the multi-host route.
- On a single Docker host (development) every member listens on 5432.

## Not yet

A replica in another region is reached through the project region's
pooler. Region-local read endpoints, with the logins carried to that
region's poolers, come later. More than two replicas per project are
planned for later as well.
