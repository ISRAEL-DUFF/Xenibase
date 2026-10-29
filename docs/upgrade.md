# Upgrading

Xenibase is versioned as one unit: server, agent, UI, and the bundle's
configuration ship together under one tag (`v1.2.3`). Within a major
version an upgrade is "check out the new tag and run the installer".

## Before you start

- Read the release notes in [CHANGELOG.md](../CHANGELOG.md) for the tag.
- Check the last nightly metadata self-backup succeeded (Settings → Backup
  checks). It is your way back if anything goes wrong.
- Keep your copy of `deploy/compose/.env` and the backup key at hand.

## Upgrade the bundle (the control node)

> **Recommended:** run `./upgrade.sh <tag>` rather than the commands in this
> block. It backs up the metadata database, checks the result, and rolls back
> by itself if the new version doesn't become healthy
> ([deploying](deploying.md)). The list after the block says what an upgrade
> does either way.

```sh
cd xenibase
git fetch --tags
git checkout v1.2.3            # the new release
cd deploy/compose
./install.sh                   # keeps .env; rebuilds images; restarts what changed
```

What happens:

1. The images are rebuilt from the new tag.
2. `docker compose up` replaces the containers whose image or configuration
   changed. xenibase-server runs the metadata migrations (goose) on start,
   before it serves anything; a migration that fails leaves the old schema
   in place and the server refuses to start. (A migration that succeeded
   can't be undone by starting the old version: see
   [rolling back](#rolling-back).)
3. The installer recreates the poolers so they read this version's
   configuration (their config files are bind-mounted, and a checkout
   replaces them, which compose doesn't notice). Connected clients
   reconnect (a second or two).
4. The bundled agent restarts with the new version. Running operations are
   resumed by the new server: every step is idempotent.
5. Migrations may queue one-off `apply_settings` operations (for example,
   00018 gives older shared projects their `temp_file_limit`); they finish
   in the background and show on Operations.

Check afterwards: `docker compose exec xenibase-server
xenibase-server -version` shows the new version, Nodes shows every agent
healthy, and Operations shows nothing failed.

## Remote nodes

Agents on other nodes must run the same **major** version as xenibase-server
and a minor version at least as new: upgrade the agents first, then the
server. xenibase-server sends no work to an agent of another major version or
an older minor (an older agent doesn't know the new work) and shows it on
the Nodes page; a newer minor or any patch version is fine. Replace the agent binary (or image) on each node and restart it; its
state directory keeps its identity, so it needs no new registration.

## PostgreSQL minor versions

Rebuild the images (`./install.sh` does it; on remote nodes `make
pg-image`, or pull them). Each instance then shows "18.x available" on its
node's page, and Xenibase restarts the instances that are behind, one at a
time, in the weekly maintenance window (Admin → Nodes; default Sunday
02:00–06:00 UTC). The poolers hold clients for each restart. **Upgrade now**
on the same panel does one immediately. See [moves and Postgres
versions](moves.md#minor-upgrades-and-the-maintenance-window).

The shared cluster from `compose.yaml` has no agent, so it is not part of
the sweep. Recreate it yourself in a quiet moment; shared projects on it
are unavailable for the restart (usually under a minute):

```sh
docker compose pull shared-pg
docker compose up -d shared-pg
```

## PostgreSQL major versions

Xenibase offers the majors in `XENIBASE_PG_VERSIONS` (default 17 and 18).
Projects move to a newer one with a major upgrade (Project Settings →
Compute → Postgres version), a logical-replication move that pauses writes
for a few seconds. See [moves and Postgres versions](moves.md#major-upgrades).

## Moving from V2 (PGDock)

V4.1 renamed the product (PGDock became Xenibase) as a clean break, so an
install on `v2.0.0` or `v2.1.0` can't be upgraded in place. Install V4.1
on a new server and move the projects across one at a time:
[Moving from V2 to V4.1](upgrade-v2-to-v4.md) is the runbook. The sections
below say what each version changed.

## Upgrading to V3

- **Standby pooler and status page** (migrations 00020 and 00021): nothing
  changes until you add a second pooler host ([edge poolers](edge-poolers.md))
  or run `xenibase-status` ([status page](status-page.md)); the single
  pooler of the bundle carries on as before.
- **Shared clusters restart once.** Every Postgres instance now runs
  `wal_level=logical`, so moves can copy from it. Agent-run instances pick
  it up the next time they are recreated (a restart from the UI, or the
  maintenance window). Recreate the compose shared cluster after upgrading
  (`docker compose up -d shared-pg`) so that shared projects can move by
  logical replication. Until then they move by dump and restore.
- **pg_hba for moves.** The bundle's `shared-pg-hba.conf` gains one rule
  for move logins from its Docker network, and agents add a similar rule
  to every running instance when they start, and to each instance they
  start later (`XENIBASE_AGENT_MOVE_ALLOW`, default the private ranges).
  Upgrade and restart the agents on remote nodes before moving projects
  there. A move whose target can't log in fails at its first step and rolls
  back, changing nothing.
- **Moves pause webhooks and skip jobs.** While a project is moving or
  upgrading, webhook deliveries wait and scheduled job runs are skipped, as
  during a promotion in V2.
- New migrations 00022 (moves, the `moving` and `upgrading` statuses),
  00023 (Postgres releases, minor-upgrade history), 00024 and 00025 (HA:
  etcd members, HA members, failover history, SLA probes and
  availability).
- **HA** needs the new `xenibase-postgres` images (they now carry Patroni;
  `./install.sh` and `make pg-image` build them) on every node, the agents
  of V3, and the etcd cluster set up once (Admin → Nodes). Agents pull
  `gcr.io/etcd-development/etcd:v3.6.5` when asked to run a member; open
  ports 2379–2380 between the three etcd nodes. See [HA](ha.md).

- **Billing** (migration 00026) publishes a first price book with
  placeholder prices and gives every organisation a billing account on the
  Free plan. Nothing is invoiced for months before the upgrade, and
  invoices aren't issued until you turn on automatic issue (Admin →
  Billing → Settings), so you can set real prices and your company's
  details first. A **Pro** quota plan is added between Personal and Team
  (an existing quota plan named Pro is kept as it is). See
  [Billing](billing.md).
- **Payments** (migration 00027, reworked in V4.1) are off until you configure
  the wallet provider: `XENIBASE_ISWALLET_BASE_URL`, `XENIBASE_ISWALLET_API_KEY`,
  `XENIBASE_ISWALLET_WEBHOOK_SECRET` and `XENIBASE_ISWALLET_OWNER_PREFIX`. Set the
  provider's webhook to `https://<your Xenibase>/api/v1/payments/webhooks/iswallet`;
  it must be reachable from the internet. Dunning starts
  for invoices past their due date once the server runs, but nothing is
  deleted for non-payment unless you turn it on (Admin → Billing →
  Settings). See [Payments](payments.md) and [the wallet provider](iswallet.md).
- **The Free tier** (migration 00028): idle Free projects are paused after
  7 days without client connections (owners are warned a day before) and
  archived after 90 days paused. The activity clock of every existing
  project starts at the upgrade. The bundle sets `XENIBASE_WAKER_ADDR`, the
  waker that wakes paused projects; with pooler hosts elsewhere, point it
  at the server's private address. Archiving needs backup storage. Open
  signup can use Cloudflare Turnstile (`XENIBASE_TURNSTILE_SITE_KEY`,
  `XENIBASE_TURNSTILE_SECRET`) and caps accounts per IP a day
  (`XENIBASE_SIGNUPS_PER_IP`, default 3). See [The Free tier](free-tier.md).
- **Support, revenue and legal** (migration 00029):
  - It adds tickets, the `support` platform role, legal documents with
    acceptances, and daily MRR snapshots.
  - On first start, the server publishes template SLA and DPA documents
    as version 1, and the terms gain an acceptable use policy. Review and
    replace them before launch ([Legal documents](legal.md)).
  - Support email needs `XENIBASE_SUPPORT_EMAIL` and
    `XENIBASE_SUPPORT_INBOUND_SECRET`, with the provider's inbound webhook
    at `https://<your Xenibase>/api/v1/support/inbound/email`.
  - WhatsApp needs the `XENIBASE_WHATSAPP_*` variables and Meta's webhook
    at `https://<your Xenibase>/api/v1/support/whatsapp`.
  - See [Support](support.md).
- **Capacity and costs** (migration 00030):
  - Every existing node becomes `manual`, `active`, in the region named
    by `XENIBASE_REGION` (default `eu-central`), with no cost.
  - Enter what each node costs under Platform → Capacity → Cost, so cost
    attribution can divide it, and record an exchange rate (Costs &
    margins → Exchange rates).
  - Nothing is bought until you set `XENIBASE_CLOUD_PROVIDER=hetzner` (with
    its variables) and a monthly budget.
  - See [Capacity and costs](capacity.md).

- **Regions** (migration 00031):
  - A `regions` table is created from the nodes' regions plus
    `eu-central`; every project is placed in its node's region.
  - The pooler generation moves to a per-region table; pooler hosts in
    the home region carry on with the same generation.
  - Nothing changes for a single-region install. To add one, see
    [Regions and data residency](regions.md).
  - Upgrade the agents with the server: HA members get
    `XENIBASE_ADMIN_USER`, which base backups of HA projects need.

- **Query insights** (migration 00032):
  - New tables for statistics, texts, snapshots and slow queries. Insights
    start with a baseline at the first read after the upgrade.
  - Rebuild or pull the Xenibase Postgres image (it now includes hypopg);
    existing dedicated instances get it at their next recreation or minor
    upgrade.
  - Without billing, set `XENIBASE_INSIGHTS_PLANS=all` so shared projects
    get insights too. See [Query insights](query-insights.md).

- **Hardening (M27)** (no migration):
  - Existing HA clusters get Patroni's `failsafe_mode` from xenibase-server
    the first time it sees their leader after the upgrade; nothing to do.
  - `XENIBASE_WAKER_ADDR` must be reachable from xenibase-server itself as
    well as the poolers: the server checks it for the `waker_down` alert,
    and doesn't pause Free projects while it can't reach it.
  - Admins are asked to confirm refunds, manual payments, credit notes,
    event attribution, price book publishing and org billing terms with
    their password or code.
  - Before opening Lagos, work through [the Lagos launch](lagos-launch.md).

## Upgrading to V3.1

- **Failure domains** (migration 00033): nothing changes until you
  record domains. Then record each node's rack (Platform → Nodes → the
  node → Failure domain) and look for the banner on the Nodes page (or a
  `failure_domain` alert) listing HA pairs, etcd members or pooler hosts
  that share one. New Hetzner servers go into `xenibase-<region>` spread
  placement groups that Xenibase creates; the API token needs to be able to
  manage placement groups (a read-and-write token can). See
  [Failure domains](failure-domains.md).
- **etcd per region** (migration 00034): the existing etcd cluster becomes
  its nodes' region's cluster, and every HA instance is recorded as using
  it. Set up a cluster in each other region with HA projects (Platform →
  Nodes → etcd cluster for HA, pick the region), then use **Move to the
  region's etcd** on each of those projects' HA cards. Upgrade the agents
  with the server: member replacement uses a new agent endpoint.
- **Announced maintenance** (migration 00035): from this release the
  weekly window's minor upgrades **wait for an announcement** before
  switching over an HA project. Announce the next window (Admin →
  Incidents → Schedule maintenance) at least 72 hours ahead, or set
  `XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT=false` to keep upgrading HA
  projects in every window, as V3 did. Announcements email organisation
  owners and admins, so check the SMTP settings first.

## Upgrading to V4

- **Backend services' foundation** (migration 00036): nothing changes for
  existing projects; backend services are off until a project turns them
  on. To serve them, set `XENIBASE_API_DOMAIN` and `XENIBASE_EDGE_SECRET` on
  xenibase-server, point a wildcard DNS record and certificate for
  `*.<domain>` at each region's xenibase-edge, and run xenibase-edge there with
  the same secret ([Backend services](backend-services.md)). Rotating the
  master key also re-seals the projects' signing keys.
- **Auth core** (migration 00037, project schema version 2): projects with
  backend services get the `xb_auth` tables within a minute of the upgrade
  (the reconciler applies them; nothing to run). Auth emails go through the
  platform SMTP at 30 an hour per project until a project sets its own; set
  up the platform SMTP first if it isn't. xenibase-edge needs this release too:
  upgrade xenibase-server first, then the edges.
- **Auth: phone, OAuth, MFA, hooks** (migration 00038, project schema
  version 3): the auth email queue is renamed and widened to SMS and
  WhatsApp, and projects with backend services get a `<db>_auth_hook` role
  and the OAuth and MFA tables within a minute (nothing to run). To offer
  phone sign-in on the platform's accounts, set the Termii and WhatsApp
  template variables ([Backend services](backend-services.md#platform-sms-and-whatsapp-operators));
  without them only projects with their own provider can use it. Upgrade
  xenibase-server first, then the edges.
- **Request roles become logins** (migration 00039): each project's
  `<db>_anon`, `<db>_user`, `<db>_service` and `<db>_auth_hook` get
  passwords and pooler entries, and the edge login loses its memberships of
  them, within a minute of the upgrade. Edges still on the previous release
  switch roles from the edge login, which is no longer allowed: their data
  API and hook requests fail until they are upgraded. Upgrade the edges in
  the same window as xenibase-server (a new edge works with an older
  xenibase-server, so upgrading the edges first avoids the gap).
- **Storage** (migration 00040, project schema version 4): projects with
  backend services get the `xb_storage` schema within a minute (nothing to
  run). Files are kept in each region's backup storage target, so the
  region needs one (the in-country one for data-residency projects), and
  xenibase-edge needs network access to it, as do browsers and apps for large
  uploads (presigned URLs point at the store). Plans gain the limits
  `file_storage_mb`, `storage_egress_mb_per_month`,
  `image_transforms_per_month` and `upload_max_mb`. To purge a Cloudflare
  CDN when a bucket goes private, set `XENIBASE_CDN_CLOUDFLARE_ZONE_ID` and
  `XENIBASE_CDN_CLOUDFLARE_TOKEN` on xenibase-server. Upgrade xenibase-server
  first, then the edges.
- **Realtime** (migration 00041, project schema version 5): projects with
  backend services get the `xb_realtime` tables and functions within a
  minute (nothing to run). xenibase-edge needs to reach each region's
  session-mode pooler for realtime's `LISTEN` connections (the feed sends
  its address; `XENIBASE_EDGE_SESSION_ADDR` overrides it); without one it
  polls the outbox every second and broadcasts stay within one edge
  process. If a load balancer sits in front of the edges, let WebSocket
  upgrades through and give it an idle timeout over 25 seconds (the
  clients' heartbeat). Upgrade xenibase-server first, then the edges.
- **Read replicas** (migration 00042): nothing changes until a dedicated
  project adds one. HA standbys' billing stops counting members that were
  removed.
- **Spend caps for backend services** (migration 00043): an organisation
  at its spend cap gets slower backend services instead of none; nothing
  to do.

## Upgrading to V4.1

- **Plan limits for backend services** (migration 00044): plans gain
  monthly data API requests and monthly active users (Personal: 500,000
  and 10,000), and ceilings on statement timeout, rate limits and daily
  SMS codes. On an upgraded install the monthly limits start with the
  first full month after the upgrade. Review them under Admin → Plans
  ([quotas](operations.md#quotas-storage-locks-and-usage)) before turning
  backend services on for customers.
- **Billing add-ons** (migration 00045): longer point-in-time recovery and
  backup retention, and per-region premiums. Price books published before
  the upgrade have no add-on prices: publish a new book with them before
  selling the add-ons.
- **Postgres version lifecycle** (migration 00046): every major in
  `XENIBASE_PG_VERSIONS` starts as supported; nothing changes until you
  deprecate one (Admin → Platform → Postgres versions).
- **Status subscribers** (migration 00047): with `xenibase-status` running,
  paying organisations' owners and billing contacts are subscribed to
  their components' incidents within the hour. Upgrade xenibase-status with
  xenibase-server and add the **billing** and per-region **backend services**
  components to `status.toml` (see `deploy/status/status.example.toml`).
- **Proposed maintenance** (migration 00048): HA minor upgrades waiting for
  an announced window now get a drafted announcement to confirm under
  Admin → Incidents → Scheduled maintenance. Nothing is sent until you
  confirm it.
- **Costs of backend services** (migration 00049): new cost categories
  edge, files and messages. Nodes can have the role `edge`. Set the share
  of shared nodes that edges use (**Edges' share of shared nodes** in the
  cost settings, [capacity](capacity.md)) if edges run
  on shared nodes.
- **Integrations** (migration 00050, project schema version 6): webhooks
  gain a description, metadata and overlapping secret rotation, every
  event carries `project_id` and `primary_key`, and the data API takes
  `Idempotency-Key`. The reconciler applies schema version 6 to projects
  with backend services within a minute; until it has, a write with an
  `Idempotency-Key` answers `503 idempotency_unavailable`. Receivers that
  parse `Xenibase-Signature` must accept several `v1=` values
  ([webhooks](webhooks.md#verifying-the-signature)) before anyone uses an
  overlapping rotation.
- **Settings the compose bundle passes on**: from this release the
  bundle's `compose.yaml` passes the optional settings these docs describe
  (payments, the Free tier's clocks,
  `XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT`, the status page, the CDN purge,
  `XENIBASE_MASTER_KEY_PREVIOUS`, `XENIBASE_PG_VERSIONS`) from `.env` to
  xenibase-server. Before, setting them in `.env` had no effect.
- Upgrade xenibase-edge with xenibase-server: the cache, render workers and
  heartbeats need this release on both.

## Upgrading from 4.2.4

- **Projects' children keep their organisation** (migrations 00068 and
  00069): the tables that carry a project's `org_id` (members, their
  logins, saved queries, reaped sessions, editor preferences, dedicated
  requests) get a foreign key to `projects (id, org_id)` that follows a
  transfer. Rows an earlier transfer left under the old organisation are
  moved to the project's first. 00068 adds the keys without scanning;
  00069 checks the existing rows without blocking writes. Both are quick at
  the sizes these tables have. A 4.2.4 server still running during the
  upgrade can't transfer a project (the transfer fails and changes nothing)
  until it is replaced.

## Rolling back

Take the metadata self-backup before every upgrade; it is the way back.

Before the new server has started, check out the previous tag and run
`./install.sh` again. Once it has run a migration, do **not** just check out
the old tag: a release before V4.1 starts on a database a newer release
migrated without noticing, and runs on tables it doesn't know, which can
write wrong data without any error. Stop xenibase-server, restore the metadata
database from the self-backup (see [disaster recovery](disaster-recovery.md),
step 2), then check out the previous tag and run `./install.sh`. From V4.1
on, a server refuses to start on a database a newer release migrated, and
says to restore the backup.
