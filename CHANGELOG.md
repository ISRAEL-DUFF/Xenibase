# Changelog

Xenibase follows semantic versioning; server, agent, UI, and the install
bundle share one version (spec §11.5).

## Unreleased

### Added
- **Connections grow with a dedicated instance's memory.** `max_connections`
  was 100 for every size; it is now 50 per GB, from 100 (2 GB or less) to 500
  (10 GB or more): 200 on a 4 GB instance, 400 on 8 GB. A dedicated project's
  connection limit can go up to that less 10 that Xenibase keeps for its own
  connections (190 on 4 GB); more is refused with the ceiling named, which the
  Connections and timeouts panel and `instance.connection_limit_max` show. A resize changes
  `max_connections` with the restart, in Patroni's cluster configuration for HA
  and replica clusters, and lowers the project's limit when the new size holds
  fewer. A point-in-time restore or a detached replica never gets fewer than
  its source, which Postgres needs to replay the source's WAL. See
  `docs/user-guide.md` ("Connections").
- **An alert when a dedicated project's WAL archiving fails or falls
  behind.** Nothing watched archiving: a failing `wal-push` (rotated
  credentials, a full bucket) let WAL pile up until the volume filled,
  while base backups still succeeded, and recovery past it was impossible.
  The metrics collector now samples each dedicated instance's archiver
  (`wal_archive_*` on `/metrics`), and the critical `wal_archive` alert
  fires when archiving failed in the last 15 minutes or a finished segment
  has waited longer; it turns the status page's Backups component to
  degraded. And every other week the restore test recovers a random
  dedicated project's base backup and WAL to the present into a scratch
  instance on its node, reads every table and removes it (no project is
  created); a restore test run for a dedicated project does the same.

### Fixed
- **A branch reset no longer copies a parent the caller has lost.** Reset
  checked only the branch, so someone removed from the parent could keep
  refilling a branch with its current data, and a full-data branch of a
  parent marked sensitive afterwards kept refilling for any developer. A
  reset now needs `project.branch` on the parent too, and its admin role
  for a full copy of sensitive data (403 otherwise); a token restricted to
  the parent still covers its branches. Removing someone from a project
  also removes them from its live branches and drops their logins there.
- **A transfer can no longer split a branch from its parent's
  organisation.** Transferring a branch, or a project with live branches,
  is refused with a 409; before, the other organisation's admins could
  reset a moved branch and keep receiving the parent's data. A branch whose
  parent is in another organisation is no longer filled or reset (detach
  or delete it), and a parent's branch count only counts its own
  organisation's branches.
- **Detaching a branch now gets a new project's checks.** It needed only
  the developer role on the branch, so create-then-detach went past the
  projects quota, and a developer could own a project in an organisation
  whose members may not create them. Detach now needs `org.create_project`
  and room in the projects quota (409 `quota_exceeded`), and runs under the
  branch's lock: it is refused (409) while a reset or anything else is in
  progress, or the branch isn't active.
- **A branch's file copy no longer succeeds with files left out.** Every
  copy error (throttling, the network, the operation's deadline) counted
  as "not found in the parent". Only a file the parent no longer has
  counts as missing now; other failures fail the attempt once the rest were
  tried, and the retry copies only what is left.
- **A branch reset now copies files again and removes the ones it
  orphaned.** A reset of a branch made with `copy_files` brought the
  parent's file rows but not their files, and the files already under the
  branch's prefix (earlier copies, its own uploads) were left unbilled with
  no row naming them. Branches now remember `copy_files` (**migration
  00077** adds `projects.branch_copy_files`, filled from the create
  operation while that is kept); after every reset the background
  `copy_branch_files` puts the files no refilled row names on the garbage
  list for the storage sweep, then copies the parent's files again.
- **The branch expiry sweep no longer deletes a branch saved at the last
  minute.** It deleted from a list read at the start of the pass, so a
  branch extended or detached in between was deleted anyway, without the
  final backup turned on meanwhile. Each delete now checks the locked row
  is still an expired branch, and takes the final-backup choice from it.
- **Branch resets count against operations in flight.** They were counted
  but never checked, so every branch could be reset at once, each a full
  dump of the parent with a live source. A reset beyond the plan's limit
  is refused with 409 `quota_exceeded`, as a new branch is.
- **A branch create that failed after it went live no longer drops it.**
  Backend services were turned on after the branch was published, so a
  failure there retried the whole create, dropping the live database under
  its clients, and a second failure left an active branch whose API keys
  never worked. They are now turned on before the branch is published, and
  a retry that finds the branch active only queues its file copy.
- **A Postgres version notice whose email failed was never resent:** its
  record was written before the send. It is now kept only when the mail
  goes, still once across servers, so the next hourly check sends it
  again; the deprecation notice is retried too, until the first reminder.
- **The instance image did not pin the Postgres minor release:** it was
  built from the moving `postgres:<major>` tag, so nodes ran whatever
  release they last pulled, and a rebuild left the previous release
  untagged. Each major is now built from a pinned release (17.11 and 18.6,
  `PG17_RELEASE`/`PG18_RELEASE` in `deploy/images/postgres/Dockerfile`)
  and tagged with it as well (`xenibase-postgres:18.6-walg3.0.9`); the
  `{major}` tag the agent uses is unchanged. `make pg-image` and
  `install.sh` build through `deploy/images/postgres/build.sh`. See
  `docs/upgrade.md` ("PostgreSQL minor versions").
- **A point-in-time recovery to before a major upgrade could not start:**
  it used the project's new major on data from the old one. The restore now
  starts on the major of the instance that took the base backup, so an
  owner whose app breaks on the new major can go back to before the
  upgrade (writes made since are not in it). See `docs/moves.md` ("Major
  upgrades").
- **A project on a retired or preview Postgres major could not be restored
  to a point in time, branched, or have a replica detached:** each made its
  new project as a new project would, which those majors refuse. They now
  take any major the server has an image for, so a retired project keeps
  its safety net.
- **Minor upgrades took no lock, weren't operations, and weren't
  crash-safe.** Two servers could restart the same instance twice, an
  owner could queue `ha_disable` or a resize during an HA upgrade, and a
  server stopped between pause and resume left PgBouncer paused. Each
  minor upgrade is now a `minor_upgrade` operation, exclusive with the
  project's other operations (a shared cluster's keeps its projects'
  exclusive operations off until it ends); its OnFail resumes the poolers,
  a crashed attempt is retried once and resumes them first, and the sweep
  runs on one server at a time under an advisory lock and finishes any run
  an older server left unfinished. **Upgrade now** queues the same
  operation and waits for it.
- **A failed minor upgrade left the instance down and then looked done.**
  The agent removed the old container before creating the new one, so a
  release that would not start left nothing running, the sweep retried the
  same instance every 5 minutes, and the next release check read the
  missing or crash-looping container as upgraded. The agent now keeps the
  old container, stopped, until the new one is ready and starts it again
  otherwise; a release is reported only for a running container. The
  failure is recorded, a **Minor upgrade failed** alert fires, and the
  sweep moves on to other instances, trying that one again only for a
  newer release or **Upgrade now**. See `docs/moves.md` ("Minor upgrades
  and the maintenance window").
- **The HA watcher paused healthy projects when xenibase-server lost sight
  of their primary,** and ran on every server without a lock. It now pauses
  the poolers only when the members that answer agree there is no running
  leader, and leaves them alone when none answers. One server at a time
  watches, under a Postgres advisory lock; a server that loses it resumes
  the poolers it paused, and the next one resumes any left paused. A
  switchover marks its instance (**migration 00073**) so the watcher on
  another server leaves it alone instead of recording a failover. See
  `docs/ha.md` ("Failover").
- **A read-only member could read as the project owner in the SQL console.**
  Read-only sessions, read-scope tokens and the table browser's raw filter
  now log in as `<db>_console_ro`, granted only `<db>_ro`. Existing projects
  get the login on their first read-only session.
- **Imports could connect to any address,** including the clusters and nodes
  that trust Xenibase's own addresses, which bypassed IP allowlists. Sources
  now pass the outbound rules (public addresses, or hosts the platform admin
  allow-listed for the organisation; never loopback or a node), pinned to
  the checked address for the agent's copy.
- Tenants on a shared cluster could read each other's activity counters
  (`pg_stat_database` and the `pg_stat_get_db_*`/`pg_stat_get_backend_*`
  functions). They are revoked; **migration 00066 queues `apply_settings`
  for every active shared project** to apply this to existing databases.
- Narrowing an IP allowlist now drops the project's pooled clients from
  addresses no longer on it; PgBouncer's admin console is limited to private
  networks (`XENIBASE_POOLER_ADMIN_ALLOW`); a pooler host whose PgBouncer is
  not on `auth_type = hba` is marked not ready; the waker admits only the
  poolers (`XENIBASE_WAKER_ALLOW`), and the edge wakes a paused project only
  for a request with one of its keys; one invalid route or node address is
  skipped and alerted (`pooler_config_skipped`) instead of blocking every
  region's pooler sync.
- **Master-key rotation re-encrypts everything sealed.** It skipped
  per-project backup keys, Patroni secrets, webhook secrets and headers,
  scheduled jobs, projects' auth SMTP settings and queued messages, device
  sign-in tokens and the etcd CA key, and still reported success. If you
  rotated before, run the rotation again with the old key in
  `XENIBASE_MASTER_KEY_PREVIOUS` (`docs/operations.md`, "Secrets").
- **xenibase-server renews its certificate for agents.** It was issued at
  start for 30 days and never renewed, so a server that ran longer lost
  every agent. It is now reissued with 7 days left, and the new
  `agent_client_cert` alert fires if under 3 days remain.
- **The agent bootstrap token registers the local node once.**
  `XENIBASE_AGENT_BOOTSTRAP_TOKEN` could claim any node without an agent by
  name, including one just added and a removed one. It now registers only
  the bundle's local node (`XENIBASE_SHARED_NODE_NAME`), never a removed
  node, and only once; afterwards use a token from Nodes → Re-register.
- **Each edge can have its own credential, bound to a region.** One
  platform-wide `XENIBASE_EDGE_SECRET` authenticated every edge, and the edge
  chose its region, so any edge (and every edge node's cloud-init) could
  read every region's projects. `xenibase-server -issue-edge-credential
  <name> -edge-region <region>` issues a credential the server binds to its
  region; edge nodes Xenibase creates get one. The platform-wide secret now
  serves only regions without their own API domain
  (`XENIBASE_EDGE_SECRET_ALL_REGIONS=true` restores the old scope,
  deprecated). See `docs/upgrade.md` before upgrading an install with a
  region API domain.
- **A region's pooler hostname gets its own certificate.** Pooler hosts of a
  region with its own hostname were sent the home region's certificate,
  which doesn't name theirs. xenibase-server now keeps one per region, in the
  `XENIBASE_POOLER_TLS` mode, in the region's directory. In `files` mode
  the certificate must name every region's hostname (`docs/regions.md`).
- **No Hetzner token on pooler hosts.** The agent on each internet-facing
  pooler host held a full-access Hetzner token to assign the floating IP.
  xenibase-server's arbiter now moves it to keepalived's MASTER and the
  agent only reports its state; `XENIBASE_AGENT_ASSIGN_FLOATING_IP=true`
  keeps agent-side assignment (faster failover while the control plane is
  down). See `docs/upgrade.md`.
- **Password rotations no longer keep SCRAM verifiers.** A rotation kept the
  owner's new and previous verifiers in its parameters forever (a credential
  switch the new owner's), and the API returned them to anyone who could
  view the project. They now travel with the operation's secrets, removed
  when it ends; migration 00062 removes those already stored.
- `TestHASwitchoverAndFailover` asked for a switchover before Patroni reported
  the new standby streaming, and failed on a busy host; it now waits.
- An audit row the metadata database refused was only logged. It is now
  retried until written (in order, with the request's time), and `/metrics`
  counts failures, the backlog and rows given up on, which are logged in full
  (`docs/operations.md`, "The audit log").
- The audit log's append-only triggers were said to hold "even against
  xenibase-server itself"; the server owns the table and could disable them.
  The migration's comment and `docs/security-review.md` now say what they
  guarantee.
- The tenant lint's table list missed every backend services table and more,
  and any mention of `org_id` passed. It now reads the tables from the
  migrations and requires a real `org_id` predicate.
- Transferring a project left its reaped sessions, editor preferences and
  dedicated requests under the old organisation. Every child that carries the
  project's `org_id` now follows it by a foreign key.
- Reaped statements (with the tenant's statement text) and finished
  operations' parameters and step logs were kept forever. Reaped statements
  now go after 30 days and with their project, and operations keep a summary
  after 90 days (`docs/operations.md`, "What is kept").
- A server already running when a newer release migrated the database kept
  taking operations. It now stops, and answers `/readyz` with 503, within 30
  seconds. `docs/migrations.md` sets the expand-then-contract rule.
- Each call to a node's agent opened a new TLS connection; calls now reuse one
  per node.
- **The orphan reaper could destroy a move's, promotion's or major
  upgrade's target mid-copy.** They insert the target when they are queued
  and the project points at it only at the cutover, so after five minutes
  the reaper took it for a failed create's leftover. It now skips an
  instance a queued or running operation names as its target, and an
  upgrade preflight's scratch instance in its first hour, and checks each
  instance again just before destroying it. A retry no longer starts a new
  container under an instance already marked deleted.
- **A move's rollback that could not reach the source left the project
  frozen for good.** It returned before resuming the poolers, and the
  project stayed `moving` (or `promoting`, `demoting`, `upgrading`) with
  its logins `NOLOGIN` until someone repaired it by hand. The poolers now
  resume whatever happens, and a sweep every minute gives the logins back
  and sets such a project active once its instance answers; one still
  frozen after 10 minutes raises a `project_stuck` alert.
- **A retry after a move's freeze copied everything again with the source
  still frozen.** An error, crash or deploy between the freeze and the
  cutover started the second attempt with the logins `NOLOGIN` and the
  poolers paused, so a dump-mode move or promotion was down for a second
  whole copy. A move, promotion, demotion or major upgrade now opens writes
  again at the start of any attempt after the first that finds the project
  not yet cut over.
- **A resized project could not be restored to a point in time or have a
  read replica detached.** Both copied the source's profile name, `custom`
  after a resize to a size no profile has, and the create refused it as an
  unknown profile. They now pass the source's CPUs and memory as the size,
  as a node move does; a node move's capacity check is now of the real size
  too.
- **A dedicated project's major upgrade skipped the capacity check and
  dropped the PITR add-on.** Its new instance went onto the project's node
  whether or not there was room for a second one, and with 7 days of
  point-in-time recovery (and billing) whatever the project paid for. The
  preflight and the upgrade now refuse a node without room for it, and the
  new instance keeps the project's window.
- **A region move left the project's WAL archive in the old region.** The
  new instance's archive was placed from the project before the cutover,
  so it went to the old region's storage. It now goes by the region of the
  new instance's node.
- **Replication objects a move's cleanup could not remove stayed for
  ever.** A slot with no consumer kept WAL until the disk filled (on a
  shared cluster, for every tenant there), and the DDL block kept refusing
  the tenant's schema changes. An hourly sweep now removes the
  `xenibase_move_*` subscriptions, publications, inactive slots, logins
  and DDL blocks in project databases that no queued or running operation
  owns, and instances cap slot WAL (`max_slot_wal_keep_size`: a quarter of
  a dedicated volume, 10 GB on an agent-run shared cluster) from their next
  restart.
- **A deleted project's nightly backups were kept forever.** Only its final
  backup was given an expiry, so up to 11 earlier backups (82 with the long
  add-on) stayed on platform storage, listed and restorable. A delete now
  dates its nightly and archive backups with the final backup (30 days), as
  does an organisation's deletion, and the hourly expiry removes them with
  their cross-region copies; backups in an organisation's own bucket are
  left alone. **Migration 00075** dates those of projects already deleted,
  at least a week from the upgrade.
- **Copying backups between two targets at one place deleted them.** Two
  storage targets could name the same endpoint, bucket and prefix
  (`backups` and `backups/` included); a switch between them with "delete
  the originals" copied each object onto itself and then deleted it, and
  turning on data residency could delete a backup as its own cross-region
  copy. Such a target is now refused, and copies, and deletes of an
  original or a copy, between two existing ones are refused too. If two of
  your targets already share a place, send new backups to one of them;
  don't change either's prefix, which leaves the backups on it unreachable.
- **One failed WAL-G listing forgot every base backup.** The agent read a
  failed `wal-g backup-list` as an empty archive, and the base backup that
  followed it marked all of the project's base backups deleted, so its
  point-in-time window restarted at the next night's backup. A failed
  listing is now an error, and a listing without the backup just taken
  forgets nothing.
- **A storage or key switch cut a paid 14- or 30-day recovery window to
  7.** The archive a switch retires (enabling or rotating a project key is
  one) expired after 7 days, so a dedicated project paying for 14 or 30
  could recover only to the switch from day 8. It now stays for the
  project's window.
- "Backup overdue" fired for projects the nightly schedule skips on
  purpose (paused and archived Free projects, branches without backups,
  suspended organisations), and kept the public status page's Backups
  component degraded. It now leaves out what the schedule leaves out.
- **With more than one server, imports and Supabase migration steps failed
  at random.** The source's connection string (and S3 key) was kept in the
  memory of the server that took the request, so a step another server
  claimed failed with "xenibase-server restarted?" and the import rolled
  back, as did any retry. They now travel with the operation, encrypted
  with the master key, and are removed when it ends.
- Nothing pinned the OpenPGP keys derived from the backup key and project
  keys, which are never stored: a crypto library update that derived them
  differently would have left every WAL-G archive and project-key backup
  unreadable, unnoticed until a restore. A test now fixes them.
- An import's verification counted the rows of every table in the source,
  so a source login allowed to read only the imported schemas failed it
  with "permission denied" and the import rolled back after the copy. It
  now reads only the chosen schemas.
- **A Supabase migration's source could be in the platform's private
  network.** Its address check, unlike an import's, did not refuse the
  cloud private network or `XENIBASE_OUTBOUND_BLOCK` for a host the
  platform admin allow-listed for the organisation, so a tenant who
  controls that host's DNS could point it at platform services. It now
  uses the import's check.
- **A backup download that failed part-way looked complete.** Once the
  first bytes were sent, a decryption error ended the response normally,
  so the browser or `curl` saved a truncated archive as a finished
  download. The connection is now cut instead, and the download fails. The
  disaster-recovery runbook decrypted the metadata backup through standard
  output and restored the result even after a failure; it now uses
  `--out`, which leaves no file on failure, and `&&`
  (`docs/disaster-recovery.md`).
- **A signed thumbnail URL opened the original.** A signed URL with a
  transform carried an ordinary download token, so changing `render/sign`
  to `object/sign` in it downloaded the full-size file. A token with a
  transform now serves only that render (`400 invalid_signature` on the
  download route).
- **A signed upload URL skipped the storage policies on what it wrote, and
  could be used again and again.** The upload's row was written with the
  edge's own login, so a policy on the type, size or metadata never
  applied, and an upsert URL could overwrite its path as often as its
  holder liked, for up to 7 days. A signed upload is now written as the
  user who signed it (their role and claims are kept with the URL in the
  new `xb_storage.upload_tokens`, project schema version 8), the URL
  uploads once (only an upload that commits uses it up, on any edge), and
  it lasts 2 hours at most, as documented.
- **A large upload's parts were neither bounded nor counted.** A part's
  presigned URL signed no length, so each took up to the store's part limit
  (5 GiB) whatever the upload declared, and uploads in progress were not in
  the quota check: a 50 MB upload could hold about 20 GB for a day, and
  uploads started together each passed against the same total. Each part's
  URL now signs its exact length, uploads in progress hold their declared
  size against the quota (direct and dashboard uploads count them too, and
  starting one takes its turn on the project's usage row), and a project
  may have 100 open at once (`429 too_many_uploads`).

### Upgrading
- Migration 00060 adds `instances.max_connections`. Existing instances keep
  100 until they are resized.
- Migrations 00068 and 00069 move children an earlier transfer left behind to
  their project's organisation and add the keys; 00070 builds two indexes
  concurrently for the retention sweep (`docs/upgrade.md`, "Upgrading from
  4.2.4").
- Migration 00061 adds `edge_credentials`; migration 00062 removes SCRAM
  verifiers from finished password rotations and credential switches.
- Migration 00075 gives the nightly and archive backups of projects deleted
  before the upgrade, on platform targets, their final backup's expiry, and
  at least seven days: they are deleted from a week after the upgrade.
- Signed upload URLs made before the upgrade stop working (`400
  invalid_signature`): they have no row in `xb_storage.upload_tokens`, so
  they can't be used once. Apps sign a new URL; they last 2 hours at most.
  A project's signed uploads fail until its schema reaches version 8, which
  the backend services reconciler applies within minutes of the upgrade.

## v4.2.4

### Added
- **A region's own API domain.** Platform → Regions → **API domain**
  (`api_domain` on `PUT /api/v1/admin/regions/{id}`) gives a region's projects
  `https://<ref>.<api-domain>` for their backend services, served by an edge in
  the region (`XENIBASE_EDGE_DOMAIN` set to it, `XENIBASE_EDGE_REGION` to the
  region), so a Lagos project's API traffic stays in Lagos instead of crossing
  to the home region. Edge nodes Xenibase adds in the region get it in their
  cloud-init. See `docs/regions.md`.
- **Role management on dedicated projects.** An organisation owner can let a
  dedicated project's owner role create roles (`CREATEROLE`, Project Settings →
  Compute → Database roles, or `PUT /api/v1/projects/{id}/manage-roles`), for
  applications whose migrations create their own roles. Postgres 16+ limits it
  to the roles the owner creates. Logins it creates named `<database>_<name>`
  connect through the poolers within 30 seconds (`POST …/manage-roles/sync` for
  at once), with the project's connection limit; moves and major upgrades take
  the roles along. See `docs/database-roles.md`.
- **An IP allowlist per project.** Project Settings → Network access (or
  `PUT /api/v1/projects/{id}/network` with `allowed_cidrs`, up to 50 addresses
  or CIDR ranges) makes the poolers accept the project's logins only from
  those addresses; Xenibase's own services (backend services, the dashboard's
  SQL editor, probes) still connect. `GET` returns the caller's address and
  whether it is allowed. Enforced by PgBouncer HBA rules in each region's
  pooler configuration. See `docs/network-access.md`.
- **Early access to hidden regions.** A hidden region takes new projects from
  the organisations in its **Early-access organisations** list
  (`early_access_org_ids`), for rehearsals before it opens or a first customer;
  `GET /api/v1/regions` lists it to their members. Before, a hidden region
  refused every project, so the Lagos launch rehearsals could not run until it
  was open.
- **Standby settings per read replica.** Hot standby feedback (off by
  default) and how long replay waits for a conflicting query before cancelling
  it (30 s by default, 0 to 900; `max_standby_streaming_delay` and
  `max_standby_archive_delay`), set when a replica is added
  (`hot_standby_feedback`, `max_standby_delay_seconds` on
  `POST /api/v1/projects/{id}/replicas`) or changed later
  (`PATCH /api/v1/projects/{id}/replicas/{replica_id}`, the replica's
  **Settings…** in the dashboard, `xenibase replicas set`). A change applies to
  that replica alone with a reload, without a restart; the primary and HA
  standbys keep Postgres's defaults. See `docs/read-replicas.md`.

### Security
- Built with Go 1.26.10, which distrusts compromised intermediate certificates
  in `crypto/x509` (GO-2026-6809); the agent's TLS connections reached it.

### Upgrading
- Migration 00056 adds `regions.api_domain` and `region_early_access`. Nothing
  changes until a region is given a domain or an early-access list.
- Migration 00057 adds `projects.manage_roles` and `project_owned_logins`.
  Nothing changes until an owner turns role management on.
- Migration 00058 adds `projects.allowed_cidrs`. The poolers now use
  `auth_type = hba` with an `hba.conf` xenibase-server writes next to
  `databases.ini` and `userlist.txt`. **Upgrade the pooler hosts first**
  (agent and PgBouncer configuration, `xenibase-pooler-host` image), then
  xenibase-server: an older pooler-host agent refuses bundles that carry
  `hba.conf`. A compose install with the poolers in the bundle gets both
  together from `./upgrade.sh`; for a dev checkout, run `make pooler-seed` and
  recreate the poolers.
- Migration 00059 adds the settings to `read_replicas`. Existing replicas keep
  Postgres's defaults (feedback off, 30 s), which they already ran with.

### Fixed
- **Importing a database through a PgBouncer failed.** The import (and the Supabase migration) opened its connection to the source with `default_transaction_read_only=on` as a startup parameter. A PgBouncer in front of the source, such as the pooler of a Xenibase V2 instance, refuses any startup parameter it doesn't ignore, so the preflight stopped with "FATAL: unsupported startup parameter: default_transaction_read_only". Both now connect plainly and make the session read-only with `SET` once connected, which works through a pooler in session mode and straight to Postgres. The copy itself is made by `pg_dump`, which reads in its own read-only snapshot. Tests run the connection against a fake pooler that refuses the parameter.
- **A source that doesn't answer is explained, not an "internal error".** When the import's preflight (or the import itself) can't read the source in time, for example because a pooler in front of it has no free connection for that login, or a query is refused, the answer is now a clear request error naming the likely cause (the password is redacted). It was a 500 after the preflight's one-minute limit.

## v4.2.3

- **Upgrades rebuild the edge.** `install.sh` built the server and agent images but not the edge, so a deployment with backend services on kept running the old edge after `upgrade.sh` (v4.2.2's browser preflight fix never reached it). The edge is rebuilt on every install and upgrade while the `services` profile is on. If you upgraded to v4.2.2 and use backend services, upgrade again.
- **Two-factor sign-in is off by default for ordinary accounts.** New accounts sign in with a password and turn on a TOTP second factor themselves, under Account → Security (password, QR code, first code, then recovery codes; turning it off asks for a password and a code, and both changes are emailed). Platform admins and support staff still must have it and cannot turn it off. Accounts that already enrolled keep it. Step-up confirmations (deleting a project, spending money) ask for the password alone when two-factor is off. A reset by an administrator now leaves an ordinary account on password sign-in instead of making it enrol again. API: `POST /auth/login` returns the session itself for an account without two-factor (`challenge_id` is then absent), `POST /auth/reauth` takes the code only when it is on, and `GET /me/totp`, `POST /me/totp/setup`, `/enable` and `/disable` are new.
- **Backups to S3 and R2 work again.** The agent and Postgres images, built on the official `postgres` image, had no CA certificates, so every upload failed with "x509: certificate signed by unknown authority" (it also affected WAL-G archiving from dedicated instances). Both images now install `ca-certificates`, and the build fails if they are missing. `./upgrade.sh` rebuilds them; recreate dedicated instances' containers to pick up the new Postgres image.
- The storefront guide says how a webhook behaves across environments (one public URL each, where the secret goes).

### Added
- **OAuth for third-party apps.** An app registered by a platform admin
  (`POST /api/v1/admin/oauth/clients`) can send a user to `/oauth/authorize`,
  where they consent per organisation: narrow the scopes, restrict projects,
  choose the expiry, approve or deny. The app exchanges the single-use code
  (10 minutes, PKCE S256 required of every client) at `POST /api/v1/oauth/token`
  for an ordinary API token, and can give it back at `POST /api/v1/oauth/revoke`.
  Tokens issued this way show under **Account → Connected apps** with the app's
  name (`oauth_client_name`, `created_via: oauth`) and **Disconnect** revokes
  them; a second use of a code revokes the token it issued; disabling the app
  revokes everything it holds. Consents, issues and revocations are audited.
  Taskiem is the first such app. See `docs/oauth-apps.md`.
- **Integrator roles on dedicated instances.** `POST /api/v1/admin/projects/{project_id}/roles`
  (platform admin, step-up auth) creates the named `NOLOGIN` roles on a
  dedicated project's instance and grants them to the owner role
  `WITH ADMIN OPTION`, for integrators whose migrations expect them (Taskiem's
  `taskiem_app` and `taskiem_dispatch`). The roles are recorded on the project,
  recreated by moves, named by the demotion check and expected by the
  isolation audit. Shared projects answer `409 conflict`.

## v4.2.2

### Fixed
- **Browser apps couldn't call the API with Xenibase's own clients.** Every client sends
  `X-Client-Info`, and the edge's answer to a browser's preflight didn't allow it, so
  the first request of every browser app (and the Next.js guide's sample) was blocked by
  CORS. For an allowed origin the edge now allows the request headers the browser lists
  in its preflight. A test sends a real preflight with the headers the clients send.
- Backend services through the install bundle didn't work at all: the edge exited at
  once (its trusted-proxy setting was a bare address, and the edge wants a network range),
  and Caddy couldn't get the wildcard certificate with acme-dns (the plugin only
  expands `{env.…}` in the password, but the script used it for the username, subdomain
  and server URL as well). `enable-services.sh` now writes those three out and reloads
  Caddy, the compose file uses `172.31.250.10/32`, and tests check both: the script's
  output for each provider, and the real plugin against a fake acme-dns server
  (`make test-deploy`, `make test-acmedns`).
- With acme-dns, Caddy no longer waits on its own check of the acme-dns nameserver
  (`propagation_timeout -1`): from the first real server that UDP lookup timed out and
  failed a certificate acme-dns had served correctly. The health check in the docs
  (`/healthz` through the public address) couldn't work behind the wildcard site; it
  now says how to check from inside, and what an unknown name answers.
- The app's text and layout were about 12% smaller than intended (the page's
  root font size was 14px, and Tailwind sizes are in rem): text-xs rendered at
  10.5px and text-sm at 12.25px. The root is the browser default now.
- The wallet's bank account is shown as bank details: the account number large,
  then the bank and the account name, each with a copy button, and *Copy all
  details*. The account name no longer carries the provider's instruction
  ("Please make a bank transfer to …"). The API's wallet lists every account
  (`accounts`), and the page shows each, though a wallet has one today.
- An upload through xenibase-edge whose object store failed (unreachable,
  bad credential) was answered `400 incomplete_body` ("the upload ended
  before its length"), blaming the client and logging nothing. Only a body
  that really ended early is now `incomplete_body`; a store failure is
  `503 storage_unavailable` and the edge logs the cause.

### SDK
- `@xenibase/client` 0.1.0 is on npm (`npm install @xenibase/client`), under the
  Apache-2.0 licence.
- `@xenibase/client` 0.1.1: a CommonJS build as well as ES modules (a `require` from a
  script or `tsx` in a package without `"type": "module"` used to fail with
  `ERR_PACKAGE_PATH_NOT_EXPORTED`), and a network failure says why (`fetch failed <-
  ECONNRESET: …`, on `error.cause` too). `X-Client-Info` now follows the package version.
  Publish it with `npm publish` from `sdk/js`.

### Docs
- The admin auth API in full: creating users (`email_confirm`), and `generate-link`
  (which types need an existing user, that `redirect_to` must be an allowed redirect URL,
  the answer's fields, and how to get a session from a code in a test).
- The team guide: policies written with `current_database()` so one migration works on
  every project, how to make fewer round trips (embedded reads, `Promise.all`, a function
  that returns a whole page's data), testing sign-in without an inbox, where the linked pages
  are when the guide is shared as one file, and the distance to the project's region.
- [Enabling backend services](docs/enabling-backend-services.md): the full method for
  a compose install, with the routes that were tried (Cloudflare, NS delegation,
  deSEC, a registrar's reseller API, acme-dns) and why production uses acme-dns.
- [Building on Xenibase: a team guide](docs/guides/storefront.md): a domain
  storefront from nothing to a safe backend, for people new to backend services
  (the model, keys, row-level security, auth, orders through a webhook, files,
  live updates, a launch checklist, how to report problems).


## v4.2.1

### Backend services in the install bundle
- `deploy/compose/enable-services.sh <api-domain>` turns on backend services
  on a compose install: it runs `xenibase-edge` behind Caddy, with the
  wildcard certificate for `*.<api-domain>` obtained and renewed through a
  DNS challenge (Cloudflare, deSEC, or acme-dns for a DNS host with no API; a CNAME can
  delegate just the challenge). Existing installs are unchanged until it is run.
  See [Running xenibase-edge](docs/backend-services.md#running-xenibase-edge).

## v4.2.0

### Fixed
- The wallet panel has a **Get the bank account** button when the provider
  didn't return the wallet's bank account as it was opened (it showed
  nothing before).

### Email
- Every email now goes out with an HTML part in one branded layout (the logo,
  a card, a button for the link, a footer), and the plain text as before.
  Light and dark, and on a phone.
- The messages are written in a warmer voice: sign-up, verification,
  password reset, approval, two-factor and role changes; invitations; API
  tokens; invoices, receipts, wallet and overdue notices, prepaid, budget and
  spend-cap alerts, price changes; branches, jobs and webhooks; the Free
  tier's pause, archive and delete notices. Subjects keep their key words.
- Your projects' own emails (confirmation, magic link, password reset,
  invitation, email change) are laid out under the project's name, with no
  Xenibase branding. Project → Auth has a **Clean email layout** switch to
  send a template's text exactly as written instead.
- `xenibase-server mail-preview <dir> [public-url]` writes sample emails
  as HTML and text files to look at in a browser.

### Legal documents
- The service level agreement, data processing agreement, order forms and the
  terms of use and privacy notice are shown as documents: headings, lists and
  tables in the app's type, with a contents list for a long one and a button
  to print it or save it as a PDF. They were the Markdown source. Raw HTML in a
  document is never shown, and only web, mail and phone links are linked.
- The editors have a Preview tab, so what readers will see is seen before a
  new version is published.

### Sign-in screens
- A new look for sign-in, sign-up, verification, password reset, invitation
  and the setup wizard: larger headings and a brand panel of receding
  doorways. The words on the screens are unchanged.

### New brand
- A new logo, the doorway, and a teal accent in both themes, replacing the
  purple. The browser tab icon was still PGDock's: it is Xenibase's now,
  with app icons and a sharing card for link previews. See [Brand](docs/brand.md).

## v4.1.0

### Renamed: Xenibase
- The product is now **Xenibase** (it was PGDock). Everything follows, as a clean
  break: the `xenibase-server`, `xenibase-agent`, `xenibase-edge` and
  `xenibase-status` binaries and the `xenibase` CLI; `XENIBASE_*` environment
  variables (the old `PGDOCK_*` names are not read); `xb_` API key prefixes
  (`xb_pub_…`, `xb_sec_…`), registration tokens (`xbreg_…`), and the `xb_auth`,
  `xb_storage` and `xb_realtime` schemas in project databases; the
  `xenibase_*` cookies and `X-Xenibase-*` headers; invoice numbers
  (`XB-YYYY-NNNNNN`, credit notes `XB-CN-…`); the Go module path, the SDKs
  (`@xenibase/*`, the Dart package) and the default domain (`xenibase.ng`).
  Format identifiers inside data (such as the backup key file's magic header)
  are unchanged. An install of an earlier release isn't upgraded in place:
  install fresh and import its projects.

### Billing: dollar prices, wallets
- Prices are in **US dollars** (cents) and an invoice is converted to naira
  when it is issued, at a rate frozen on it (the market rate, the spread, the
  effective rate, when and where it was priced). The invoice, its PDF and its
  email show both. A rate older than `fx_max_age_hours` (4) is refused: the
  invoice stays a draft. The ledger, payments and credit notes stay in kobo.
  A price book in naira still works, and a platform with no wallet provider
  starts with one.
- Each organisation has a **wallet** at the wallet provider (iswallet),
  funded by bank transfer into its own account number and debited for each
  invoice when it is issued, and when it is topped up, under a **consent** an
  owner gives (and can withdraw) and Xenibase records. An unanswered debit is
  looked up by its idempotency key and never repeated; a wallet too short for
  an invoice starts dunning and pays it when topped up. The nightly
  reconciliation compares every wallet's balance with the books. New endpoints
  under `/orgs/{org}/billing`: `wallet`, `debit-consent`, and `pay-from-wallet`.
  Configured by `XENIBASE_ISWALLET_*`. See docs/payments.md and docs/iswallet.md.
- **Removed: cards, saved cards, hosted checkout, iSpend ("Pay with iSpend")
  and wallet mandates, USDT top-ups, automatic top-up and card expiry
  reminders.** The Flutterwave provider and the old iSpend merchant provider
  are gone. An organisation pays from its wallet (debited with its consent, or
  by **Pay from wallet** on an invoice), or by a manual payment the platform
  admin records (bank transfer, cheque); an organisation without a wallet
  stays on prepaid, topped up by manual payments, with no automatic top-up.
  - Configuration removed: `XENIBASE_FLW_*`, `XENIBASE_ISPEND_*`,
    `XENIBASE_PAY_CARDS`, `XENIBASE_PAY_VA_PRIMARY`, `XENIBASE_PAY_VA_FALLBACK` and
    `XENIBASE_PAY_WALLET`. Kept: `XENIBASE_ISWALLET_BASE_URL`, `_API_KEY`,
    `_WEBHOOK_SECRET`, `_OWNER_PREFIX` and `_CLIENT_ID`. Unregister the old
    providers' webhooks.
  - API removed: `POST /api/v1/orgs/{org}/billing/checkout`,
    `GET`/`DELETE /billing/payment-methods...`,
    `POST /billing/payment-methods/{id}/default`,
    `PUT`/`DELETE /billing/auto-topup`, `BillingSettings.stablecoin`,
    `BillingAccount.auto_topup`, and `channels.card` and `channels.stablecoin`
    (`channels` is now `{wallet, transfer}`). The payment webhook route is
    `POST /api/v1/payments/webhooks/iswallet` only. The `card_failing_since`
    field and the dunning step names keep their names and now mean "automatic
    wallet charge failing since".
  - Refunds: only manual payments can be refunded (the refund completes at
    once and the admin returns the money outside Xenibase). Money in a wallet
    is refused; correct a wallet-paid invoice with a credit note.
  - Database: migration 00052 drops `payment_methods` and
    `billing_accounts.auto_topup`; its down migration recreates them empty.
    Old payments and payment intents, and their `channel` constraints, stay
    as history.
  - CLI: `xenibase billing pay <invoice>` now debits the wallet for that
    invoice (no checkout link; the `--wallet` flag is gone). New
    `xenibase billing wallet` prints the balance, the amount owed and the debit
    consent. `xenibase billing transfer` shows the wallet's bank account.
  - `make test-payments-sandbox` now runs a small check of the iswallet client
    (needs `XENIBASE_TEST_SANDBOX=1` and `XENIBASE_ISWALLET_*`).
  - Operator note: the default DPA now names "Our wallet provider" as the
    payment sub-processor instead of Flutterwave and iSpend. An install
    already running keeps the DPA it published; if you publish the new
    wording, tell customers 30 days ahead, as the DPA's sub-processor notice
    requires, that Flutterwave and iSpend are replaced by the wallet
    provider (docs/legal.md).

### Deploying
- `deploy/compose/upgrade.sh <tag>`: an upgrade with a verified metadata
  backup, health and version checks, and an automatic rollback if the new
  version doesn't become healthy (`--dry-run` to only check). A **Deploy**
  GitHub Actions workflow runs it over SSH behind an approval, refusing a tag
  whose CI isn't green; the deploy key is limited to `upgrade`, `status` and
  `last` by `deploy-ssh.sh`. See docs/deploying.md.
- Fixed: the compose bundle built `xenibase-server` and `xenibase-agent` with no
  version, so a bundle install reported `dev`: `xenibase-server -version`
  didn't show the release, the Nodes page showed `dev`, and the rule that an
  agent must be at least as new as the server never applied to the bundled
  agent. `install.sh` now passes the version (`git describe`) to both.

### Found rehearsing the V2 to V4 upgrade
- Fixed: the Free tier would have paused the platform admin's own projects.
  The billing migration gives every organisation a Free billing account,
  including the one on the **Unlimited** quota plan, and the sweep only
  looked at the billing plan. Organisations on the Unlimited plan are now
  never paused or archived, and a project of theirs that is already asleep is
  woken by the next sweep.
- Fixed: a server now refuses to start on a metadata database that a newer
  release migrated (`the metadata database was migrated by a newer Xenibase
  than this one`). Before, an older release started on the newer schema
  without a word and could write wrong data. `v2.1.0` and earlier don't have
  the check, so rolling back from V4 means restoring the metadata backup
  first.
- Docs: the rollback sections of docs/upgrade.md and
  docs/upgrade-v2-to-v4.md said an old server "refuses to start" on a newer
  schema; they now give the restore steps that actually work.

### Integrations (Taskiem's asks)
- Webhooks have a `description` and string `metadata` for the tool that
  made them; every change event carries `project_id` and, for tables with
  one, `primary_key`; `rotate-secret` takes `overlap_seconds` (up to a
  day) during which deliveries are signed with both secrets (two `v1=`
  values: verifiers must accept any match). New migration 00050.
- `Idempotency-Key` on the data API's writes, batches and function calls:
  a repeat within 24 hours gets the first answer
  (`Idempotent-Replayed: true`), a different request with the same key
  `422 idempotency_key_reused`. New project schema version 6, applied by
  the reconciler on upgrade.
- Management API calls that need a paused or archived Free project's
  database answer `503 project_resuming` / `project_restoring` with
  `Retry-After` and wake it (they answered `409 conflict`). Every `429`
  carries `Retry-After`.
- The OpenAPI file declares its security schemes (`bearerAuth`,
  `sessionCookie`).
- Docs: the error code catalogue with retryable codes (docs/errors.md),
  the deprecation policy, the webhook management contract, building an
  integration (docs/integrations), and signed sample deliveries.

### Upgrading from V2
- Fixed: the compose bundle didn't pass several settings the docs
  describe from `.env` to xenibase-server, so setting them there had no
  effect: the payment providers (`XENIBASE_FLW_*`, `XENIBASE_ISPEND_*`,
  `XENIBASE_PAY_*`), the Free tier's clocks (`XENIBASE_FREE_*`),
  `XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT`, `XENIBASE_PG_VERSIONS`, the
  status page (`XENIBASE_STATUS_*`), the CDN purge, `XENIBASE_MASTER_KEY_PREVIOUS`,
  `XENIBASE_FLOATING_IP_ID` and `XENIBASE_OUTBOUND_BLOCK`.
- Docs: a runbook for going from V2 to V4.1 in one step
  (docs/upgrade-v2-to-v4.md), with the settings to decide first, a
  rehearsal on an isolated copy of production's metadata, and rollback;
  docs/upgrade.md covers V4.1's migrations and V3's 00020 and 00021.

### V4.1 (on feature/xenibase4)
- The DPA template lists the SMS and WhatsApp providers used for apps'
  sign-in codes (Termii, Africa's Talking, Meta) and Cloudflare's CDN as
  sub-processors. Existing installs: see docs/legal.md before publishing
  it as a new version.
- Docs: the Lagos launch checklist uses the region's own etcd cluster
  (V3.1); the status page's limits are up to date.
- Per-plan limits for backend services (V4.1-M2): plans set monthly data
  API requests and monthly active users (Personal: 500,000 and 10,000),
  past which requests get `429 plan_limit_reached` and new users `429
  mau_limit_reached` until the month ends (sign-in keeps working), and
  ceilings on a project's statement timeout, rate limits and daily SMS
  codes. Owners are emailed at 80% and 100%. The API page and `xenibase
  services status` show what is in effect. On upgrading installs the
  limits start with the first full month. New migration 00044.
- Fixed: the data API couldn't call functions in a schema with no tables.
- Billing add-ons (V4.1-M3): 14- and 30-day point-in-time recovery for
  dedicated projects and extended (30 daily, 12 weekly) or long (30
  daily, 52 weekly) backup retention, on Pro and Team, billed by the hour
  (Backups page, `xenibase pitr window`, `xenibase backup retention`); a price
  book can set a per-region premium on dedicated, HA and read replica
  lines. New migration 00045. Price books published before this have no
  add-on prices until a new one is published.
- Fixed: a price book saved through the API dropped its message margin.
- Resizing dedicated projects (V4.1-M4): a new size or a bigger disk from
  Project Settings → Size and disk, `xenibase instance resize|disk` or
  `PATCH /projects/{id}/instance`, with a cost estimate and a dry run. The
  instance restarts in a few seconds under the poolers' pause, or with HA
  switches over to a resized standby; a size its node can't hold moves it
  to one that can. Dedicated placement now checks a node has room.
- Dedicated hosts on demand: with no room in the region and a provider
  that creates servers, a dedicated create provisions a host within the
  infrastructure budget and waits for it; over the budget it is refused
  with `409 capacity_pending_approval` while the proposal waits.
- Postgres version lifecycle (V4.1-M5): Admin → Platform → Postgres
  versions sets each major to preview, supported, deprecated (with a
  retirement date at least 180 days out) or retired. Deprecation emails
  the owners and admins of affected organisations, with reminders at 90,
  30 and 7 days, and puts a banner on their projects; retired majors take
  no new projects, while existing ones keep running. Preview majors can be
  chosen on the create form. The terms template has a Postgres versions
  section. New migration 00046.
- The major upgrade preflight now checks dedicated projects too, restoring
  the schema into a temporary instance of the new major, and warns about
  features the new major removed.
- Status page (V4.1-M6): paying organisations' owners and billing
  contacts are subscribed to the incidents on the components and regions
  their projects use, synced hourly (a per-contact *Status page emails*
  toggle; an unsubscribe is kept). New **billing** and per-region
  **backend services** components; xenibase-edge now reports at least every
  30 seconds. Upgrade xenibase-status with xenibase-server and add the new
  components to status.toml (see status.example.toml). New migration
  00047.
- Proposed maintenance (V4.1-M7): when an HA project's minor upgrade waits
  for an announced window, Xenibase drafts the announcement for the first
  window at least 96 hours away; confirm it (it shows the exact email,
  and how many it reaches) or discard it under Admin → Incidents →
  Scheduled maintenance. The schedule form previews its email too. New
  migration 00048.
- Region page: a region's pooler pair and failure domains, its etcd
  cluster, and its HA projects still on another region's etcd, with
  **Move all**, which moves them one at a time.
- With automatic rebalancing on, rebalance moves run only inside the
  maintenance window.
- Banners: every member of an organisation sees an overdue, restricted or
  suspended billing state or a declined payment (owners and billing
  members with the pay link, no amounts), owners and billing members see
  the budget reaching 80% and 100%, and everyone sees open incidents
  affecting the organisation's projects.
- API page (V4.1-M8): a **Quick start** with the project's URL and
  publishable key in TypeScript, Dart and Go; **API docs** for each
  exposed table, view and function (columns, row-level security, policies,
  who may do what, and examples in curl, TypeScript, Dart and Go, with
  **Try it** in the request explorer); and **Usage this month** against
  the plan, with the project's share of the month's charges for owners and
  billing members. New `GET /projects/{id}/services/catalog` and
  `/services/usage`.
- CLI: `xenibase policies list` and `xenibase policies lint` (exit 1 on a
  danger finding, for CI); `xenibase logs api` with `--status`, `--path` and
  `--follow`.
- Branches of a project with backend services get their own API: a new
  URL, keys and signing key, the parent's settings without its auth
  secrets, and the users the copy brought, signed out. The create
  response, the credentials panel, the .env download and
  `xenibase branch create --env` give `XENIBASE_API_URL`,
  `XENIBASE_PUBLISHABLE_KEY` and `XENIBASE_SECRET_KEY`; `copy_files` (or
  `--copy-files`) copies the parent's stored files in the background.
- Edge caching of anonymous reads (V4.1-M9): list tables and stable
  functions with a TTL (Settings → API → **Cache anonymous reads**, or
  `cache_ttl_seconds`, at most 3,600 seconds) and publishable-key GETs
  without a user's token are served from each edge's cache (`X-Cache`,
  `Age`, `Cache-Control: public`). Writes through the API drop what they
  change at once; other changes show within the TTL. Cached answers are
  still metered. Upgrade xenibase-edge with xenibase-server;
  `XENIBASE_EDGE_CACHE_MB` sizes the cache (64 by default).
- Cost attribution for backend services (V4.1-M10): new cost categories
  **edge** (edge nodes, and a configurable share of shared nodes, split by
  each organisation's requests and realtime minutes), **files** and
  **messages** (the provider's charge for each platform-sent code), and a
  **Margin by service** table on Platform → Costs & margins. Nodes can
  have the role `edge`; edges report their CPU, and an hour above 70% in
  a region proposes an edge node, which runs xenibase-edge only
  (`XENIBASE_CLOUD_EDGE_IMAGE`). Upgrade xenibase-edge with xenibase-server. New
  migration 00049.
- Tests and hardening (V4.1-M11): image transforms run in separate
  `xenibase-edge render-worker` processes, so a bad image costs one request
  (`500 transform_failed`) rather than the edge
  (`XENIBASE_EDGE_RENDER_MEMORY_MB`, 512 by default). Fixed: a sign-in code
  used up by five wrong tries no longer lets a new one be sent at once;
  changing a password now signs out the user's other sessions. Project
  auth was reviewed against ASVS 4.0 level 2 (docs/security-review.md),
  and new tests run three sample apps' RLS policies through the data API,
  storage and realtime. Upgrade xenibase-edge with xenibase-server.
- Launch gates (V4.1-M12): `TestBackendLoad` can run against a real
  install (`XENIBASE_LOAD_TARGET`), with a six-server rig in
  `deploy/loadtest/` for the 1,000-project, 2,000 requests a second gate;
  docs/backend-runbook.md lists every GA gate with its dated status.
  Fixed: the compose install now passes `XENIBASE_API_DOMAIN`,
  `XENIBASE_EDGE_SECRET` and `XENIBASE_CLOUD_EDGE_IMAGE` from `.env` to
  xenibase-server; set the first two there to serve edges.

### V4 (on feature/xenibase4)
- Backend services' edge foundation (V4-M28): a project can turn on backend
  services (Project → Settings → API, `xenibase services enable`) and gets an
  API hostname, `https://<ref>.<domain>`, with a publishable key for apps
  and a secret key for servers. `xenibase-edge`, a new binary run in each
  region, serves them: it checks the key (and a signed-in user's ES256
  token) against that project only, applies the allowed origins and rate
  limits, and runs each request in one transaction through the pooler as
  the project's anon, user or service role. Requests are metered
  (`api_requests`, `api_egress_gb`) and logged for 7 days.
  `GET /data/v1/health` is the first endpoint; the data API, auth, storage
  and realtime follow. See docs/backend-services.md.
- New migration 00036 (backend services).
- Data API reads (V4-M29): `GET https://<ref>.<domain>/data/v1/<table>`
  with `select` (columns, JSON paths, related rows through foreign keys),
  `where`/`or` filters, ordering, cursor pages and counts; one row by key;
  `POST …/query` for nested filters; a per-project OpenAPI document. Tables
  are readable with the publishable key only with row-level security (or
  listed as public), so a forgotten policy fails closed. Schema changes are
  picked up within seconds. Exposed schemas and public tables are in
  Project → Settings → API. Backups, restores and promotions of projects
  with backend services keep their `xb_*` schemas and grants.
- Data API writes, functions and types (V4-M30): insert and upsert
  (`POST /data/v1/<table>`, `on_conflict`), update and delete with a
  required filter and `max_affected`, all-or-nothing batches
  (`POST /data/v1/batch`), and function calls (`/data/v1/rpc/<fn>`, GET for
  stable functions). Typed clients for TypeScript, Dart and Go
  (`xenibase gen types`, or a download on the API page), a security advisor,
  a request explorer that runs as anon, a user or the service role, and a
  row-level security policy helper in the Table Editor.
- Auth core (V4-M31): users in the project's own database (`xb_auth`),
  signing up and in by email and password, confirmation by link or code,
  magic links and email codes, password recovery and email change; ES256
  access tokens verified with the project's JWKS, refresh tokens that
  rotate on every use and end the session when reused, sign-out of one
  session, the others or all; lockout and per-address limits; an admin API
  with the secret key. Project → Authentication lists, invites, bans,
  signs out and deletes users, and sets the sign-in rules, email templates,
  the project's own SMTP server and signing-key rotation; `xenibase auth`.
  Monthly active users are recorded (`auth_mau`).
- New migration 00037 (auth).
- Auth: phone, OAuth, MFA, hooks (V4-M32): sign-in with SMS and WhatsApp
  codes (Termii and Xenibase's WhatsApp number by default, or the project's
  own Termii, Africa's Talking, Twilio or WhatsApp Cloud API account), phone
  and password sign-up, phone changes; SMS-pumping limits (allowed
  countries, 5 codes an hour per number, a daily cap with alerts) and
  per-message metering with spend; OAuth with Google, Apple, GitHub,
  Facebook and Microsoft (PKCE), linking by verified email and identity
  linking; anonymous users; TOTP and phone second factors with an MFA
  policy; custom-claims and before-sign-up Postgres hooks (as the new
  `<db>_auth_hook` role), after-sign-up/in and send-message webhooks;
  Turnstile captcha. Project → Authentication gains Phone, Providers, MFA
  and captcha, and Hooks; `xenibase auth config|set|hooks`. A Flutter sample
  is in docs/examples/flutter-auth.
- New migration 00038 (auth messages, hooks and alerts); project schema
  version 3.
- Security: backend services' request roles are now logins of their own
  and xenibase-edge connects as them directly, so database code that runs
  `RESET ROLE` (an RPC function, an auth hook, or SQL injection in dynamic
  SQL) can no longer reach the edge's login, the auth tables or the service
  role. New migration 00039.
- Storage (V4-M33): buckets (public or private, size limits, allowed
  types) and files in the region's object store, with access decided by
  row-level security on `xb_storage.objects` and helpers such as
  `xb_storage.folder(path, 1)`; uploads up to 50 MB through the edge and
  up to the plan's limit (5 GB on Pro and Team) by presigned multipart
  URLs, resumable for 24 hours; downloads with ranges, public URLs and
  signed URLs; listing, move, copy and delete; MIME sniffing; image
  resizing and conversion to WebP, AVIF, JPEG and PNG, cached in the store;
  file storage, download and transform quotas and metering
  (`storage_gb_hours`, `storage_egress_gb`, `image_transforms`); a nightly
  reconciler; a CDN purge when a bucket goes private (Cloudflare). Project →
  Storage and `xenibase storage buckets|ls|cp|rm|sign`.
- New migration 00040 (storage); project schema version 4.
- Realtime (V4-M34): one WebSocket per client
  (`/realtime/v1/websocket`, the protocol Supabase's realtime clients
  speak) with database changes on tables with realtime on, each subscriber
  getting only the rows its policies let it read (deletes only for rows it
  was sent); rolled-back changes never delivered; filters; broadcast and
  presence across edge processes; private channels decided by policies on
  `xb_realtime.channel_access`; broadcast history for chosen topics; an
  HTTP broadcast for servers; heartbeats, resync signals, connection and
  message limits, and metering (`realtime_messages`,
  `realtime_connection_minutes`). Project → Realtime and
  `xenibase realtime status|enable|disable|history`.
- New migration 00041 (realtime limits); project schema version 5.
- Read replicas (V4-M35): up to two per dedicated project, on other nodes
  (another region if wanted; data-residency projects stay in theirs).
  Each is a Patroni member that is never promoted and never synchronous,
  so it follows a new primary after a failover. The pooler's read-only
  route `<db>_ro` balances reads across the replicas within 10 seconds of
  the primary, and falls back to the primary, read-only, when none is.
  Data API GETs go to replicas with `Read-Replica: allowed`, or by default
  for publishable-key reads when the project turns that on. A replica can
  be detached into a standalone project. Replicas bill like dedicated
  instances of the primary's size (`replica_hours` and resource metrics).
  Project → Settings → Read replicas, `xenibase replicas list|create|delete|detach`.
  See docs/read-replicas.md.
- New migration 00042 (read replicas). HA standby billing no longer counts
  removed members.
- Supabase migration helper (V4-M36), run after importing a Supabase
  database into a project with backend services:
  - **Policies:** imported policies and column defaults are rewritten to
    the project's request roles and `xb_auth` helpers.
  - **Users:** users and identities are copied with their ids. Supabase's
    bcrypt password hashes keep working and become argon2id at each user's
    next sign-in. OAuth identities keep their provider ids.
  - **Files:** buckets and files are copied over Supabase's S3 protocol,
    and storage policies are recreated on `xb_storage.objects`.
  - **Reports:** each step lists what it couldn't carry over, and can run
    again.
  - Run it from Project → API → Migrate from Supabase,
    `xenibase migrate supabase`, or `POST /api/v1/projects/{id}/migrate/supabase`.
    See docs/migrate-from-supabase.md and the supabase-js mapping
    (docs/supabase-client-mapping.md).
- Password sign-in accepts bcrypt hashes and re-hashes them with argon2id
  on success (`password_rehashed` in the auth log).
- SDKs: `@xenibase/client` for TypeScript (sdk/js), Dart and Flutter
  (sdk/dart, `xenibase` on pub.dev), and Go (sdk/go). Each covers data, auth,
  storage and realtime, with:
  - typed rows from `xenibase gen types`;
  - token refresh before expiry, one refresh at a time;
  - consistent errors;
  - live queries that refetch on changes, resyncs and reconnects.

  `TestSDKs` runs each one against a real edge.
- A documentation site: `make docs-site` renders docs/ with navigation,
  search and a link check. It adds guides for Next.js, React Native and
  Flutter, and SDK references.
- Built with Go 1.26.9 (1.25 is out of support; html/template and net/http
  fixes are only in 1.26), and linted with golangci-lint 2.14. ES256 keys
  are encoded and parsed through `ecdsa.PublicKey.Bytes` and
  `ParseUncompressedPublicKey` instead of the deprecated coordinates.
- The Usage page lists file storage, downloads and image transforms (they
  were recorded but not shown).
- xenibase-server's metadata connection pool defaults to 16 connections
  (it was pgx's 4 on a small box) unless `XENIBASE_DATABASE_URL` sets
  `pool_max_conns`; the auth message and hook senders no longer hold a
  connection while they send.
- Backend services billing, hardening and GA readiness (V4-M37):
  - **Rating:** invoices charge each plan's backend-services usage above
    its allowance: data API requests and transfer, monthly active users,
    file storage (GB-hours) and downloads, image transforms, realtime
    connection-minutes and messages. SMS and WhatsApp codes are charged at
    the provider's cost plus the price book's margin (20% by default).
    Published price books without these lines charge nothing for them.
  - **Billing page:** every invoice and forecast line names its service,
    and **This month so far** breaks the month down by service, so far
    and projected.
  - **Spend caps** slow backend services instead of stopping them: a
    quarter of the data and storage rate limits, new image transforms
    paused, new realtime connections refused; sign-in, token refresh and
    open connections carry on. New migration 00043.
  - **SMS failover:** with Africa's Talking configured
    (`XENIBASE_AFRICASTALKING_*`), platform SMS codes go by Termii and, when
    it fails, by Africa's Talking; a failed provider is tried last for a
    minute.
  - **Security review of the edge** with fuzz tests of its isolation
    (hosts, keys, tokens), query SQL safety and signed URLs
    (docs/security-review.md), and the scope of the external penetration
    test (docs/pentest-scope.md).
  - **Load and failure tests:** `TestBackendLoad` (projects with backend
    services, a data API rate, image transform bursts; docs/load-test.md)
    and failure injection: an edge killed mid-upload, an SMS provider
    outage, a realtime process lost.
  - **Runbook** for backend services (docs/backend-runbook.md).

### V3.1 (on feature/xenibase3)
- Failure domains (V3.1-M1): each node can record what fails with it (a
  rack, host or power feed); servers Xenibase creates on Hetzner go into a
  per-region spread placement group. An HA project's standby, the etcd
  members and a region's pooler hosts are kept in different domains:
  enabling HA and setting up etcd refuse otherwise, a second pooler host
  in the same rack is warned about, and a `failure_domain` alert reports
  existing groups that share one. Platform → Nodes. See
  docs/failure-domains.md.
- New migration 00033 (failure domains).
- etcd per region (V3.1-M2): each region gets its own etcd cluster, so a
  region's HA failover depends only on that region. Platform → Nodes → etcd
  cluster for HA has a region picker and **Replace…** on each member: a
  dead or live member moves to another node in the region while the
  cluster keeps its quorum, and draining a node moves its member
  automatically. An HA project whose state is in another region's
  cluster gets **Move to the region's etcd** on its HA card (one restart,
  writes paused a few seconds). See docs/ha.md.
- New migration 00034 (etcd per region).
- Announced maintenance (V3.1-M3): Admin → Incidents → **Schedule
  maintenance** announces a window for a region or some projects. It
  shows on the status page as upcoming, is emailed to the owners and
  admins of the organisations it covers, and resolves by itself. HA
  projects' availability minutes inside a window announced at least 72
  hours ahead are excluded from the SLA, listed per announcement on the
  HA card. The weekly minor-upgrade sweep waits for an announced window
  before switching over an HA project
  (`XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT`, on by default).
- New migration 00035 (announced maintenance).

### V3 (in progress, on feature/xenibase3)
- Standby edge pooler: two pooler hosts behind a floating IP with
  keepalived. xenibase-server pushes the PgBouncer configuration to both and
  moves the floating IP itself if keepalived doesn't. Admin → Nodes shows
  both hosts and recent pooler events. See docs/edge-poolers.md.
- `xenibase-status`, a status page to run on separate infrastructure. It
  probes Xenibase from outside and takes signed heartbeats for the rest. It
  opens and resolves incidents by itself, keeps 90 days of history, and
  emails subscribers. Admin → Incidents posts incidents to it. See
  docs/status-page.md.
- New migrations 00020 and 00021 (pooler hosts, incidents).
- Moves by logical replication: promotion, demotion, node moves (platform
  admins: Project Settings → Compute → Moves, `xenibase move`) and major
  upgrades copy the data while the project serves, and pause writes only
  for the switch (about 0.1 s in the tests, at 1 and 3 GB). They fall back
  to dump and restore when they must, and say why. Schema changes are
  refused during a move. See docs/moves.md.
- Postgres versions: `XENIBASE_PG_VERSIONS` (default 17 and 18), a version
  picker when creating a project or shared cluster, and major upgrades
  with a preflight that test-restores the schema on the new version
  (Project Settings → Compute → Postgres version, `xenibase upgrade`).
- Minor releases are applied automatically in a weekly maintenance window
  (Admin → Nodes; default Sunday 02:00–06:00 UTC), one instance at a time
  with the poolers holding clients.
- HA for dedicated projects: a standby on another node under Patroni,
  with a three-member etcd cluster. The pooler route follows the leader,
  so writes are back through the same URL about 20 seconds after the
  primary's node dies. Planned switchovers, optional synchronous
  replication, failover history, and the month's availability measured
  from xenibase-server and the status page (the SLA's two vantage points).
  Project Settings → Compute → High availability, `xenibase ha`, Admin →
  Nodes → etcd. See docs/ha.md.
- Billing core: price books (versioned prices, repricing at least 30
  days ahead, grandfathering, a preview of each org's invoice), Free, Pro
  and Team plans with prorated plan changes and annual terms, monthly
  invoices (usage in arrears, the plan fee in advance, VAT, expected WHT)
  numbered `XB-YYYY-NNNNNN` and downloadable as PDF, credit notes, a
  double-entry ledger the database keeps balanced and append-only, the
  month's forecast, budget alerts, a spend cap that pauses new billable
  resources, and cost estimates before promoting or enabling HA. A new
  `billing` org role for finance staff. Org → Billing, Admin → Billing,
  `xenibase billing`. Invoices aren't issued automatically until an admin
  turns it on. See docs/billing.md.
- Payments: cards through Flutterwave (saved and charged when an
  invoice is issued), bank transfers into each organisation's own virtual
  account (iSpend, falling back to Flutterwave), Pay with iSpend and
  wallet mandates, optional USDT top-ups, and manual payments with proof.
  Webhooks are verified with the provider before anything is posted, and
  re-queried hourly and nightly. Receipts, refunds from credit, prepaid
  balances with daily deduction, alerts and auto top-up, WHT matching and
  credit-note uploads, a dunning ladder (overdue, restricted, suspended,
  deletion only if enabled), and nightly reconciliation with each
  provider. Org → Billing, Admin → Billing → Payments / WHT / Events /
  Reconciliation, `xenibase billing pay|transfer|payments`. Admin →
  Organisations → an org → Billing sets its mode (postpaid or prepaid),
  payment terms and price book. Refunds can reopen the invoices a payment
  settled. See docs/payments.md.
- The Free tier: a Free project idle for 7 days (no client connections
  through the poolers) is paused after a day's warning, and one paused for
  90 days is archived to a verified backup. The next connection wakes it:
  the client gets a message saying it is resuming (or being restored),
  and a retry gets in. Resume from the dashboard, `xenibase resume`, or the
  API. Archived projects are deleted after a year, with 30 and 7 days'
  notice. Paid plans are never paused. See docs/free-tier.md.
- Open signup: Cloudflare Turnstile on the signup page and a cap on
  accounts per IP address a day. A Pricing page in the dashboard, and
  public prices at `GET /api/v1/pricing`.
- Support:
  - Tickets from the dashboard (Organisation → Support), email (an
    inbound webhook, threaded by reference and headers) and WhatsApp
    (Pro and Team, from registered numbers), all in one console
    (Platform → Support).
  - Each ticket shows the organisation's plan, billing standing,
    projects, recent operations and incidents beside it.
  - Response targets in Nigerian business hours. Replies go back by the
    same channel; staff can add internal notes.
  - A new `support` platform role sees the console and nothing else. See
    docs/support.md.
- Revenue dashboard (Platform → Revenue):
  - MRR and ARR, with new, expansion, contraction and churn.
  - Paying organisations, ARPA, and conversions from Free.
  - Metered revenue, invoiced and collected amounts, receivables by age,
    and outstanding WHT.
  - A CSV for the accountant.
- Legal documents:
  - An acceptable use policy accepted with the terms.
  - A versioned SLA and DPA, and per-organisation order forms, accepted
    by an owner (Organisation → Legal, or with a plan change) and listed
    with their acceptances for the platform admin. See docs/legal.md.
- New migration 00029 (tickets, support phones, legal documents and
  acceptances, MRR snapshots).
- Capacity automation:
  - Hetzner Cloud servers created by Xenibase, joining by themselves
    through cloud-init and a one-time token.
  - Proposals when a region's shared disk is projected past 70% within 14
    days, or no dedicated host fits the largest size. They are
    provisioned within a monthly budget, and wait for approval above it.
  - Drains with zero-downtime moves, weekly rebalancing, and deletion of
    servers that stay empty for a day.
  - Platform → Capacity. See docs/capacity.md.
- Cost attribution and margins:
  - Each day's node, storage, transfer, floating IP and overhead costs,
    divided among organisations.
  - Margin by plan and organisation, the cost of the Free tier, and unit
    costs for repricing.
  - An FX view with recorded naira rates and the erosion since costs were
    incurred, and a CSV for the accountant.
  - Platform → Costs & margins.
- New migration 00030 (node provider, region, cost and lifecycle;
  capacity proposals; drain and rebalance moves; exchange rates; daily
  cost allocations).
- Regions:
  - Projects choose a region at creation (New project → Region,
    `xenibase projects create --region`, `xenibase regions`). Each region
    has its own pooler hosts and hostname, backup target and copy target.
  - Moving a project to another region is a zero-downtime move; the old
    hostname keeps working for 30 days.
  - Data residency (owners, with step-up): backups only in the region, no
    cross-region copies, no moves out, branches stay. Exports stay
    available.
  - Backups on platform targets are copied to a second region, verified
    by checksum, and the weekly restore test alternates between primary
    and copy.
  - Platform → Regions. See docs/regions.md.
- New migration 00031 (regions, project region and residency, per-region
  pooler generations, backup copies).
- Query insights (Pro, Team and dedicated; Project → Query insights,
  `xenibase insights`):
  - Top queries over an hour to 30 days, each with its latency and calls
    over time and its EXPLAIN as a plan tree.
  - A slow-query log.
  - Index suggestions with their `CREATE INDEX CONCURRENTLY`, a hypopg
    estimate where it is enabled, and save as migration.
  - Unused and duplicate indexes, table bloat with reclaim space, and
    blocking chains.
  - See docs/query-insights.md.
- New migration 00032 (query statistics). The Postgres image includes
  hypopg.
- Hardening for the Lagos launch (M27):
  - A billing audit: the ledger check now ties every invoice, credit note
    and payment to its ledger entries and the receivable to what is owed
    (docs/billing-audit.md, with questions for the accountant).
  - Fixed: a credit note on a partly paid invoice made the receivable
    negative, and a fully credited invoice stayed open for dunning.
  - Fixed: a signed but forged provider event could redirect another
    organisation's transfer. Refunds, manual payments, credit notes,
    attributing events, publishing price books and changing an org's
    billing terms now ask admins to confirm with a password or code.
  - Payment provider outages: a charge that hits one is tried again when
    the provider is back, a card retry isn't used up by one, and neither
    counts as a decline or emails the customer.
  - Fixed: with pooler hosts in two regions, the split-brain alert fired
    for a MASTER in each. It is now per region.
  - The waker restarts if it stops; a `waker_down` alert fires while it is
    unreachable, and Free projects aren't paused or archived meanwhile.
  - HA: Patroni's failsafe mode is on, so losing etcd's quorum doesn't
    stop writes; existing clusters get it from xenibase-server. A
    switchover with synchronous replication waits for the standby to be
    synchronous instead of failing.
  - Chaos tests (provider outage, pooler split brain, waker failure, etcd
    member and quorum loss), a rating load test at 1,000 organisations,
    and the Lagos launch gate (docs/lagos-launch.md).
- Fixed: base backups of HA projects failed (WAL-G connected as
  `postgres`).
- Fixed: recreating a Postgres 17 instance lost its data (V3 only; V2 ran
  18), and recreating an agent-run shared cluster failed.
- New migrations 00022 and 00023 (moves, Postgres releases), 00026
  (billing), 00027 (payments), 00028 (the Free tier). Upgrade notes:
  docs/upgrade.md#upgrading-to-v3.

- Fixed: the first-run setup code printed by `install.sh` was missing its
  trailing `=`, so the wizard called it wrong. The installer now prints the
  whole code, new codes have no padding, and the server ignores spaces,
  `=` padding and letter case when comparing. (The code still changes every
  time the server restarts, unless `XENIBASE_SETUP_CODE` is set in `.env`.)
- `deploy/compose/preflight.sh <ui-host> <db-host>`: a read-only check to run
  on the server before `install.sh` (OS, resources, Docker, DNS, ports,
  outbound reach, optionally the backup bucket and mail server).
  See docs/install.md.
- docs/install.md: how to get Cloudflare R2 credentials for the backup
  storage step, what to do when the wizard says the setup code is wrong, and
  memory settings and swap for a 4 GB server.

## v2.1.0

UI fixes found by testing the redesign, a SQL filter bar for the Table
Editor, and a way to add and recover platform admins. No database
migrations: upgrade from v2.0.0 with `git checkout v2.1.0` and
`./install.sh` ([upgrades](docs/upgrade.md)).

### Platform admins
- A platform admin can make another account a platform admin, or an
  ordinary user again (Admin → Users → Make admin / Remove admin), with a
  fresh password and code. The account must be active, approved, verified
  and have two-factor set up; its sessions end and it is emailed; the last
  active admin can't be removed. Setup still makes exactly one admin, so
  add a second (docs/install.md, docs/admin-runbook.md).
- Recovery when no admin can sign in: `xenibase-server admin list | promote |
  demote | reset-2fa | reset-password <email>`, run on the server. Changes
  are audited as done on the server. See docs/operations.md.

### SQL filter bar
- The Table Editor has a filter bar above the grid: type a condition as
  you would after `WHERE` (`email = 'a@b.com' or phone like '081%'`) and
  press Enter. Suggestions (Ctrl+Space, or as you type) list the table's
  columns with their type and nullability, then operators and functions.
  Recent filters are remembered per table, errors show the database's own
  message at the character that caused it, and the grid keeps its last
  good view. It combines with the Filter popover, and the count and export
  follow it.
- Safety: the condition is checked (one expression; nothing that can end
  a `WHERE` clause, such as `;`, `UNION`, `ORDER BY`, `LIMIT`) and runs as
  the project's read-only role in a read-only transaction under the
  statement timeout, so it can read what the console can read and write
  nothing. The API takes it as the `where` parameter of the rows, count
  and export endpoints.

### UI fixes from QA
- Text fields show focus with a thin accent border and a soft glow, not the
  heavy 2px outline buttons and links get.
- Phones: the top bar's switchers no longer overlap (names and badges
  show from the `sm` breakpoint up), and the Table Editor and SQL Editor
  show their table or query list and the editor in turns, with a
  Tables/Queries button, instead of squeezing the grid. The grid's footer
  fits.
- Column default values: a bare word on a text column (`free`) now shows
  how to quote it (`'free'`) before the database refuses it.
- SQL Editor completion picks up tables and columns created by the script
  you just ran, without a reload. The shortcuts sheet names Ctrl+Space
  (⌘Space is Spotlight on macOS).
- The grid's row checkboxes are 14px (they were the browser's 20px, nearly
  as tall as the row) and follow the theme, with a tick and a dash for
  partial selection.
- A branch or project that fails no longer shows a "save the password"
  card, and the message says when cleanup was incomplete.
- Row save errors in a side panel show once, inline, rather than also as
  a toast over the Save button.
- The schema change preview wraps long statements; the Plans table
  names its units; the new project hint describes opaque names; the 404
  page for signed-in users no longer uses the sign-in layout.

## v2.0.0

Xenibase V2: users and organisations, quotas and usage, API tokens and the
CLI, visual table editing, your own backup storage, database branching,
demotion, webhooks and scheduled jobs, and a redesigned UI.

**Upgrading from v1.0.0:** read [docs/upgrade.md](docs/upgrade.md) first,
and upgrade agents before the server.

### A redesigned UI
- The whole UI follows Supabase Studio's layout: a top bar with
  organisation, project and branch switchers, an icon rail with section
  menus, and dark and light themes (dark by default).
- **Table Editor:** a spreadsheet grid with filters, multi-column sort,
  immediate cell edits (conflicts reported), rows and columns edited in
  side panels, CSV/JSON/SQL export, and every schema change shown as SQL
  before it runs. Columns can be created with UNIQUE, CHECK and foreign
  keys; tables can be duplicated and commented.
- **SQL Editor:** a code editor with schema-aware completion and
  formatting, saved queries (private or shared with the project, with
  favourites), history and templates.
- Project pages: Database (branches, backups, webhooks, jobs, extensions,
  migrations), Reports, Logs, and settings split into General, Database,
  Compute and tier, and Backup storage.
- Organisation, account and admin pages in the same style; projects shown
  as cards with their branches.
- Keyboard shortcuts (press `?`), loading skeletons, a navigation drawer
  on phones, and colours that meet WCAG AA in both themes.

### Fixes from QA
- Request bodies with unknown fields are refused (`400`) instead of
  ignored; a misspelled field could clear an organisation's outbound
  allow-list.
- `GET /me` with an API token returns the token's real `expires_at`.
- Approving or rejecting a dedicated request, and demoting, accept an
  empty body (every field is optional).
- A project-restricted API token can no longer list organisation members.
- Signing out forgets the organisation remembered in the browser.
- S3-compatible stores that are unreachable or stall now fail within
  seconds (connect, TLS and reply timeouts) instead of holding an
  operation, and its slot in the operations-in-flight quota, for 30 minutes.
- Starting a stopped dedicated instance refreshes its recorded published
  port (dev mode, where Docker picks a new one).
- Switching to opaque credentials: the UI and docs now say members'
  personal logins are renamed at once. Per-project plan defaults are
  512 MB (Personal) and 8 GB (Team); the spec now says so.
- Demoting a project whose instance is down returns `503` with a reason,
  not a bare `500`.
- Upgrade: shared projects created before V2 get `temp_file_limit` (a
  migration queues `apply_settings` for each); `install.sh` recreates the
  poolers so they read the new configuration; an agent on an older minor
  version than the server is refused work (upgrade agents first).

### Hardening (V2 M16)
- **Security:** scheduled SQL jobs no longer run on a superuser
  connection, and copying a tenant's data (branches, restores, imports,
  the restore test, promotion, demotion) no longer runs the tenant's
  functions as the superuser. NAT64 and 6to4 addresses can't reach
  internal or metadata addresses. Upgrade before inviting anyone else.
- Organisation owners can download any backup as a `pg_dump` file
  (Backups → Download, `xenibase backup download`).
- Background work survives being stopped half-way: storage locks, the
  reaper, webhook delivery, the scheduler (runs now end with a stopping
  server), a member removed mid-session, an org suspended mid-backup.
- Faster webhook delivery: kept-alive connections, one destination check
  per batch, batched outbound counters.
- A V2 load check (300 projects on two nodes, webhooks, jobs, branches),
  a user guide, a platform admin runbook, an incident process and a terms
  template.

### Webhooks and scheduled jobs (V2 M15)
- Database webhooks: inserts, updates and deletes of chosen tables POSTed
  to a URL, recorded in the same transaction (a rolled-back change never
  sends anything), in commit order per webhook, signed with HMAC-SHA256,
  retried with backoff for 24 hours, then kept as dead letters you can
  replay. Column filters for updates, static headers, test events, a
  7-day delivery log, auto-pause after 50 failures, and broken-trigger
  detection.
- Scheduled jobs: SQL as the project owner, or a signed HTTP call, on a
  cron schedule in your time zone, with timeouts, skip-or-queue overlap,
  history, run now, and an email after three failures in a row.
- Outbound safety: requests only to public addresses (resolved, checked,
  connected to the checked address, no redirects); the platform admin can
  allow-list internal hosts per organisation, turn its outbound traffic
  off, and see its requests by host. Per-organisation rate limits queue
  webhook deliveries rather than dropping them.
- `xenibase webhooks …` and `xenibase jobs …`; Project → Webhooks and Jobs.

### Demotion (V2 M14)
- Move a dedicated project back to the shared tier with its URL, app
  password and every member's personal login unchanged, after a write
  freeze while the data is copied and verified. A failure before the
  switch leaves it on its dedicated instance.
- A preflight checklist (also `xenibase demote --check`): size against the
  organisation's shared storage limits, extensions, custom roles, peak
  connections, database settings that reset, and a shared cluster with
  room, the organisation's own when it has one.
- Guardrails go back to the shared defaults; point-in-time recovery ends,
  with a logical backup at once, and the old base backups stay
  restorable for 7 days. The stopped dedicated instance is kept for 48
  hours, then destroyed, which releases it from the dedicated allowance.
- `xenibase demote` and Settings → Move back to shared.

### Database branching (V2 M13)
- Branches: throwaway copies of a project on the shared tier, from its
  latest backup or live, schema only or with data, that delete themselves
  after a TTL (7 days by default) with an email a day before.
- Reset a branch from its parent without changing its URL, password or
  members' logins; detach it to keep it as a standalone project.
- Organisation branch quotas (10 on Personal, 25 on Team), branch-hours
  and branch GB-hours usage, and "contains sensitive data" projects whose
  branches copy only the schema by default.
- `xenibase branch list|create|reset|extend|detach|delete`, with `--env`
  for `$GITHUB_ENV`, and an example GitHub Actions workflow that gives
  every pull request its own database.

### Backup storage targets (V2 M12)
- Platform storage targets (one is the default) and organisation targets:
  buckets an organisation brings itself, invisible to everyone else and
  not counted against its backup quota. Every target is live-tested
  (write, read, list, delete) before it's saved; credentials are never
  shown again.
- A project chooses where its new backups go; existing ones stay
  restorable where they are, or are copied over, verified by checksum.
  Dedicated projects move their WAL-G archive and take a fresh base
  backup at once.
- Per-project backup keys: backups become standard OpenPGP messages, and
  the downloaded key file (re-authentication, audited) restores them with
  gpg and pg_restore alone.
- Backup storage on platform targets counts toward the `backup_storage_mb`
  quota.
- Point-in-time recovery reads WAL with the source archive's own
  credentials and key.

### Visual table editing (V2 M11)
- The table browser filters (equals, contains, ranges, null, lists),
  sorts by any column, opens foreign-key rows in a side panel, and
  exports up to 100,000 rows as CSV or JSON.
- Row editing for tables with a primary key: staged inline edits, new
  rows and deletes saved in one transaction, with type-aware inputs.
  If someone changed a row since you loaded it, the save stops with a
  conflict showing their version instead of overwriting it.
- A schema editor for tables, columns, constraints, foreign keys,
  indexes, schemas and enums. Every change previews its SQL with risk
  notes (table rewrites, NOT NULL scans, volatile defaults), runs with a
  5-second lock timeout (indexes concurrently), is audited with its SQL,
  and exports as a plain SQL, goose or dbmate migration.

### API tokens and the CLI (V2 M10)
- API tokens for scripts and CI: one organisation each, read/write/admin
  scopes, an optional project restriction, and a required expiry (90
  days by default, at most a year). Shown once; revocable by their owner
  and by the organisation's owners and admins; disabled when the
  organisation is suspended and revoked when their user leaves it.
- The `xenibase` CLI for linux, macOS and Windows: device login in the
  browser, contexts for several servers and organisations,
  `XENIBASE_TOKEN` for CI, `--json` everywhere, and commands for
  organisations, projects, SQL, connecting, backups, promotion, members,
  tokens and operations. See [docs/cli.md](docs/cli.md).

### Tenancy hardening, quotas and usage (V2 M9)
- Opaque database and role names for new projects; existing projects are
  renamed behind an alias (same URLs) and can switch to opaque
  credentials with a grace period. `pg_stat_activity` no longer shows
  other tenants.
- Quota plans with per-org overrides, checked when creating projects,
  backups, restores, console queries and connections.
- Storage enforcement on the shared tier: warning at 90%, read-only at
  100%, no app logins at 120%, with the console still working and
  Reclaim space. Long statements (10 min) and idle transactions (5 min)
  are ended; `temp_file_limit` is 2 GB.
- Per-org shared clusters, dedicated allowances and dedicated requests.
- Org suspension, break-glass sessions, and an admin console for
  organisations, plans and requests.
- Hourly usage recording, a Usage & quotas page with CSV export, and
  organisation deletion with a 7-day grace period.
- The isolation check runs nightly and covers metadata leaks, quota
  bypass and suspended orgs.

### Users and organisations (V2 M8)
- Many users: sign-up (invite-only by default, approval, or open with
  email domains), email verification, password reset, recovery codes,
  and versioned terms. SMTP is now a setup step.
- Organisations with owner, admin, and member roles; invitations,
  ownership transfer, leaving, and project transfer between orgs.
- Project roles (admin, developer, read-only) with a personal database
  login per member; removing someone drops their login and ends their
  connections at once.
- Organisation, project, and platform audit logs; an org switcher and
  account pages.
- Upgrading: existing projects move into the platform admin's personal
  organisation unchanged. `/api/v1/audit` is now `/api/v1/admin/audit`.

## v1.0.0

The first release: self-hosted managed PostgreSQL 18 with a web UI, on one
VPS or several nodes.

**Install:** [docs/install.md](docs/install.md). **Run it:**
[docs/operations.md](docs/operations.md). **Upgrade:**
[docs/upgrade.md](docs/upgrade.md). **Recover:**
[docs/disaster-recovery.md](docs/disaster-recovery.md).

### Projects
- Shared-tier projects in seconds: a database and role on the shared
  cluster, reached through PgBouncer in session (`:5432`) and transaction
  (`:6543`) mode with TLS required, guardrails per project, password
  rotation, and deletion with a final backup.
- Dedicated projects: their own PostgreSQL 18 container on a node (sizes
  small, medium, large), with continuous WAL archiving and point-in-time
  recovery for 7 days.
- Promotion from shared to dedicated with the same URL and password and a
  short write freeze, verified row by row; the shared copy is kept
  read-only for 48 hours.
- Import from any PostgreSQL 15+ database, Supabase included.

### Backups
- Nightly logical backups (7 daily, 4 weekly), encrypted before upload to
  any S3-compatible bucket; on-demand backups; restore into a new project or
  in place with a safety backup; weekly automatic restore tests; nightly
  self-backups of the metadata database.

### Console, browser, metrics
- SQL console scoped to the project's role (timeout, cancel, 1,000-row cap,
  CSV export, read-only mode), read-only table browser, extension
  allow-list.
- Metrics for projects (size, connections, TPS, cache hit ratio, top
  queries) and nodes (CPU, memory, disk, I/O), 1 h / 24 h / 7 d charts, and
  a Prometheus endpoint.

### Operations and security
- Alerts by signed webhook and SMTP email: backup failed or overdue,
  restore test failed, node disk over 85%, node unreachable, project over
  its disk warning, pooler down, and a failed isolation check.
- The tenant-isolation checklist (spec §7.1) checked in CI and weekly
  against every live shared cluster; `pg_hba.conf` limited to the control
  plane and the poolers.
- One owner account with argon2id passwords and TOTP, server-side
  sessions, CSRF protection, re-authentication for destructive actions,
  rate limiting and lockout, an append-only audit log.
- Secrets encrypted with a master key, rotatable with
  `xenibase-server -rotate-master-key`; agents on mTLS with pinned
  certificates and a major-version check.
- Multi-node: add nodes from the UI, per-node roles, placement by load.

### Tested
- Unit, integration, isolation, failure-injection (agent killed mid-dump,
  pooler killed, disk full, S3 lost), promotion with live writers, a
  150-project load test ([results](docs/load-test.md)), a browser journey
  from a fresh install to every feature, and the install guide itself run
  on a clean machine.
