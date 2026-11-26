# High availability for dedicated projects

An HA dedicated project runs a **primary and a streaming standby on
different nodes**, managed by [Patroni](https://patroni.readthedocs.io)
with its state in a three-member **etcd** cluster (V3 §2.2). If the
primary's node fails, the standby takes over and xenibase-server moves the
project's pooler route to it: clients keep the same URL. In the tests,
writes come back about **17–20 seconds** after the primary's node dies
(the target is under 60). HA projects carry the **99.9% SLA**, measured
from two vantage points (§2.7).

## Setting up: the etcd cluster

Each region with HA projects has **its own** etcd cluster (V3.1-M2), so a
region's failover depends only on that region. Platform → Nodes → **etcd
cluster for HA**, choose the region, and pick three of its nodes with agents
in three different failure domains. Xenibase starts
an etcd member on each (`gcr.io/etcd-development/etcd:v3.6.5`, pulled by
the agent; `XENIBASE_AGENT_ETCD_IMAGE` overrides it). Clients and peers use
TLS with certificates from Xenibase's own etcd CA (separate from the agent
CA; its key is sealed with the master key). The panel shows each member's
health; HA needs a quorum (two of three).

Members are reached at the node's address (`XENIBASE_AGENT_PUBLISH`, on
ports 2379 and 2380) or, on a single Docker host, by name on the agent's
network. Open 2379–2380 between the three nodes on the private network.

API: `GET /api/v1/admin/etcd?region=<id>` (default the home region) and
`POST /api/v1/admin/etcd` (the region is the nodes'). Every region's
cluster uses the one platform etcd CA; clusters are kept apart by their
members and tokens.

### Replacing a member

**Replace…** on a member (or `POST
/api/v1/admin/etcd/members/{node_id}/replace`, optionally with
`{"node_id": …}` for where the new member goes) runs an operation. By
default the new member goes to the least loaded eligible node in the
region: a healthy agent, not a pooler host, in service, and in a failure
domain neither remaining member uses. The other two members must be
healthy, or it is refused.

- **A dead member** is removed from the cluster first, so the remaining
  two keep a quorum of two. Then the new member is added and started, and
  joins.
- **A live member** (a node being drained) is the other way round: the new
  member joins first, then the old one is removed, so the cluster never
  has fewer than three.

Writes and failover keep working throughout. Patroni learns the new member
from the cluster, and member containers created afterwards are given the
new list. **Draining** a node that holds a member starts this replacement
by itself, and the drain response names the operation. If no node can take
the member, the drain goes ahead and says so.

### Moving a project onto its region's cluster

A project whose Patroni state is in another region's cluster (Lagos
projects set up under V3 used the home cluster) shows **Move to the
region's etcd** on its HA card once its region has a ready cluster (`POST
/api/v1/projects/{id}/ha/etcd-move`). The operation:

1. removes the standby;
2. restarts the primary on the new cluster with the poolers holding
   clients (writes paused 5–7 seconds in the tests, with no client
   errors);
3. builds a new standby from the newest base backup.

The project has no standby until step 3 finishes.

For a whole region at once, Platform → Regions → the region shows its
pooler pair (with a warning when the two share a failure domain), its etcd
cluster's health (manage it on Nodes), and the HA projects still on
another region's cluster, with **Move all** (`POST
/api/v1/admin/regions/{id}/etcd-move-all`). That queues one operation that
runs the projects' moves one after the other, never two at once in the
region; a project that can't be moved (busy, deleted, moved meanwhile) is
noted in its log and skipped, and the operation fails at the end if any
were.

The standby always goes on a node in another [failure
domain](failure-domains.md) from the primary, and the etcd cluster's three
members must be in three different ones (V3.1-M1).

## Turning HA on

Project Settings → Compute → **High availability → Enable HA…**, `xenibase ha
enable <p> [--node <id>] [--sync]`, or `POST /api/v1/projects/{id}/ha`. It
needs a second node that takes dedicated instances. The operation:

1. creates a replication role and the project's Patroni credentials;
2. restarts the instance under Patroni on the same data. Clients on the
   pooled URL wait (about 4 seconds in the tests); session-mode clients
   reconnect;
3. starts a standby on the other node from the newest WAL-G base backup
   (falling back to `pg_basebackup`) and waits until it streams;
4. adds the SLA probe login and starts measuring availability.

If the standby can't be built, it is removed and the project keeps
running as before.

**Synchronous replication** (`--sync`, or the switch on the card) makes
every commit wait for the standby: nothing acknowledged is lost on
failover, at the cost of write latency. Without it, replication is
asynchronous, and Patroni won't promote a standby more than 1 MB behind.

## Failover

Patroni promotes the standby when the primary's leader lease (20 s)
expires. xenibase-server checks every HA project's members every second:

- when the members that answer agree there is no running leader (Patroni
  has dropped the primary's lease and is electing another), the poolers
  **pause** the project's clients, so they wait rather than fail. A primary
  that only xenibase-server can't reach, while the standby still lists it,
  is left alone, and so is a project none of whose members answer the
  server;
- when Patroni has promoted the standby, the pooler route points at it and
  the poolers **resume**;
- the old primary, when its node comes back, rejoins as the standby (with
  `pg_rewind`).

With several xenibase-servers on one metadata database, one of them at a
time does this, under a Postgres advisory lock; the others take over within
seconds of its connection ending. A server that loses the lock resumes the
poolers it paused.

Each change is a **failover event** with how long writes were out. The
project's HA card (and `xenibase ha status <p>`) shows each member's role,
state and lag, the history, and the month's availability.

Containers have no watchdog device, so fencing is Patroni's leader lease
alone: a primary that can't renew its lease demotes itself, and the
poolers only ever follow the member holding the lease.

### When etcd is in trouble

- **One member lost:** etcd keeps its quorum; nothing changes for projects.
  Writes, failover and switchover all keep working. The etcd panel shows
  the member as unhealthy; bring its node back (or replace it, below).
- **Quorum lost** (two of three members down, or the network between
  them): Patroni runs with **failsafe mode** (on for every cluster since
  M27; xenibase-server turns it on for older clusters the first time it sees
  their leader). A primary that can still reach every member of its
  cluster over their Patroni APIs keeps serving instead of demoting itself,
  so projects keep taking writes. Automatic failover can't happen until
  the quorum is back: a primary that dies during an etcd outage stays down
  until etcd recovers.
- When the members return, etcd and the clusters pick up where they were.

`TestChaosEtcdMemberLoss` covers all three, with a client writing
throughout: no client errors, and every acknowledged commit kept.
`TestEtcdPerRegionAndReplace` covers the rest, again with a writer running:
- the EU cluster is lost;
- a Lagos cluster is set up and the project moved onto it;
- a member is destroyed and replaced;
- a switchover is run;
- a member is moved off a drained node.

## Switchover

**Switch over** on the card, `xenibase ha switchover <p> [--to <member>]`, or
`POST /api/v1/projects/{id}/switchover`: the poolers pause, Patroni hands
the primary role to the standby (the old primary stops cleanly first, so
nothing is lost), the route moves, and the poolers resume. Writes paused
about 2 seconds in the tests. The maintenance window uses the same path for
HA projects: it restarts the standby onto a new minor release, switches
over to it, then restarts the old primary.

With synchronous replication Patroni only hands over to the synchronous
standby. A standby that has just joined (HA just turned on, or a member
just back) becomes it within seconds; the switchover waits up to 30
seconds for that, before pausing anything, and otherwise asks you to try
again.

## Backups

WAL-G archives from whichever member is primary: Postgres only runs the
archive command there, and both members use the project's archive, so
point-in-time recovery spans failovers (WAL-G follows the timeline
change). Base backups run on the current primary's node.

## Turning HA off

**Turn off** on the card, `xenibase ha disable <p>`, or
`DELETE /api/v1/projects/{id}/ha`: the standby is removed and synchronous
replication goes off. The primary stays under Patroni with one member, so
turning HA on again doesn't restart it.

While HA is on, moving the project to another node, demoting it, a major
upgrade, and stopping or restarting it from the Instance panel are refused
(turn HA off first, or use a switchover).

Read replicas (docs/read-replicas.md) are members of the same Patroni
cluster tagged never to be promoted: turning HA off leaves them, a
switchover never picks one, and they follow the new primary after a
failover.

## Availability (the SLA)

Each HA project has a probe login (`<database>_sla`) that can only connect
and run `SELECT 1`. Every minute xenibase-server connects through the
project's **pooled URL** and runs it. With a status page configured
(`XENIBASE_STATUS_URL`), xenibase-server also gives xenibase-status the probe
targets, which it probes from outside every interval, and collects its
per-minute results. Both directions are signed with the push secret.

A minute is **unavailable when every vantage point that probed in it
failed**. With no status page, that is xenibase-server's probe alone.
Minutes while the project is busy with an operation it asked for (a
restore, a move) or its organisation is suspended are excluded. The
project's HA card shows the month so far; `GET /api/v1/projects/{id}/ha`
has the minutes.

### Announced maintenance

Planned work is announced from Admin → Incidents → **Schedule
maintenance** (or `POST /api/v1/admin/maintenance/announcements`): a
window of at most 24 hours, a region (or all), and optionally the
projects it covers. Xenibase posts it to the status page as upcoming,
emails the owners and admins of every organisation with a project it
covers, and fixes the announcement time. A window can't be edited:
cancel it (or announce a new one with `replaces`) and announce again.

A minute of an HA project's record is **excluded** when it falls inside an
announcement that covers the project (it, a node one of its members is
on, or its region) and was made **at least 72 hours before that minute**.
An announcement made with less notice is still posted, but only its
minutes from 72 hours after the announcement are excluded: the form warns,
and the list shows **short notice**. A cancelled announcement stops
excluding minutes from when it was cancelled. Maintenance done outside any
announcement isn't excluded.

The SLA prober applies exclusions every minute over the last six hours, so
a minute probed by xenibase-status a little late is excluded too. The HA
card and `availability.exclusions` list the excluded minutes per
announcement, so customers can see what was left out and why.

With `XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT` on (the default), the
weekly window's minor-upgrade sweep only switches over an HA project
inside an announcement covering it, made 72 hours ahead. Without one, the
upgrade waits for a window that has one. Non-HA projects aren't covered by
the SLA and are upgraded in every window, as before.

#### Proposed announcements

So that the gate doesn't hold upgrades back forever, Xenibase **drafts** the
announcement they need: when an HA instance is behind on its minor
release, the sweep proposes a window, the first weekly window at least
**96 hours** away (72 for the SLA, plus a day for you to confirm), scoped
to the projects waiting. Drafts are listed under Scheduled maintenance as
**proposed** with:

- **Confirm…**: shows the exact email (rendered by the server, with how
  many organisations and addresses it reaches), then announces it: the
  notice counts from that moment, it goes to the status page as upcoming,
  and the email goes out.
- **Discard**: the work keeps waiting, and the same window isn't proposed
  again; the next week's is.

A draft never emails, never appears on the status page or in customers'
banners, and never excludes a minute: only confirming sets the
announcement time. A draft whose window starts unconfirmed lapses.

The schedule form shows the same preview for the window you are about to
announce (`POST /api/v1/admin/maintenance/announcements/preview`).

## Not yet

- Xenibase proposing an announcement for the next window when work is
  queued (V3.1 §4.1): the admin announces it.
- A preview of the announcement email in the form.
