# Xenibase

Self-hosted managed PostgreSQL with a web UI. See
[the V1 specification & build plan](./Xenibase%20—%20V1%20Specification%20&%20Build%20Plan.md).

## Install (single host)

```sh
git clone https://github.com/israel-duff/xenibase && cd xenibase && git checkout v2.1.0
cd deploy/compose && ./install.sh
```

The full guide, from a fresh VPS (DNS, firewall, Docker) through the setup
wizard to your first database, is **[docs/install.md](docs/install.md)**;
CI runs its commands on a clean machine (`make test-docs`).

The bundle runs Caddy (TLS for the UI), xenibase-server, the metadata
database, the shared PostgreSQL 18 cluster, the two PgBouncers on
`:5432` (session) and `:6543` (transaction), and xenibase-agent.

| Guide | |
| --- | --- |
| [Install](docs/install.md) | A VPS to a working database |
| [User guide](docs/user-guide.md) | Organisations, members and roles, your logins, branches, your own backup bucket |
| [Operations](docs/operations.md) | Alerts, backups, security checks, capacity, secrets |
| [Platform admin runbook](docs/admin-runbook.md) | Before inviting anyone, the beta, daily and weekly tasks, accounts and organisations |
| [Incident process](docs/incidents.md) | Detect, contain, notify, recover, follow up |
| [Terms template](docs/terms-template.md) | Terms of use and privacy notice to adapt |
| [CLI and API tokens](docs/cli.md) | `xenibase` from a terminal or CI |
| [Backend services](docs/backend-services.md) | A project's data API, auth, storage and realtime |
| SDKs: [JavaScript](docs/sdk/javascript.md), [Dart/Flutter](docs/sdk/dart.md), [Go](docs/sdk/go.md) | Clients for apps and servers; guides for [Next.js](docs/guides/nextjs.md), [React Native](docs/guides/react-native.md), [Flutter](docs/guides/flutter.md) |
| [Migrating from Supabase](docs/migrate-from-supabase.md) | Data, users with their passwords, files and policies; the [client mapping](docs/supabase-client-mapping.md) |
| [Webhooks and scheduled jobs](docs/webhooks.md) | Table changes to a URL, signed; SQL or HTTP on a schedule |
| [Standby edge pooler](docs/edge-poolers.md) | Two pooler hosts behind a floating IP, with keepalived |
| [Status page](docs/status-page.md) | xenibase-status on separate infrastructure, incidents, subscriptions |
| [Upgrades](docs/upgrade.md) | New releases, agents, PostgreSQL minor versions |
| [Disaster recovery](docs/disaster-recovery.md) | Rebuild the control node from backups |
| [Security review](docs/security-review.md) | Spec §7, item by item, with the tests that check it |
| [Load test](docs/load-test.md) | 150 projects; the V2 check with 300 projects on two nodes, webhooks, jobs and branches |
| [Decisions](docs/decisions.md) | Choices made while building, per milestone |
| [Changelog](CHANGELOG.md) | Releases |

## Requirements

- Go 1.26+
- Node.js 22+ (only for building the UI; backend-only work compiles without it)
- [`golangci-lint`](https://golangci-lint.run/) v2 for `make lint`
- [`air`](https://github.com/air-verse/air) for `make dev`
- Docker with Compose for the dev environment

## Common tasks

| Command | What it does |
| --- | --- |
| `make build` | Builds the UI, then `bin/xenibase-server` (UI embedded) and `bin/xenibase-agent`. |
| `make dev-up` / `make dev-down` | Start/stop the dev environment: metadata PG (`:5440`), shared PG 18 (`:5441`), PgBouncer session (`:6432`) and transaction (`:6543`) poolers. |
| `make dev` | Go server with live reload plus the Vite dev server (proxies `/api` to Go), wired to the dev environment. |
| `make generate` | Regenerates Go server interfaces and TypeScript types from `api/openapi.yaml`. |
| `make test` | Go unit tests, frontend type-check and unit tests. Postgres-backed tests skip unless `XENIBASE_TEST_DATABASE_URL` is set. |
| `make test-db` | Go tests including the Postgres-backed ones, against the dev environment. |
| `make test-integration` | Provisioning, backups, restores, imports, dedicated instances with point-in-time recovery, promotion with live writers (and its rollback), multi-node placement, the SQL console's role scoping and read-only mode, the table browser's keyset pages, extensions, and metrics end to end, plus the tenant-isolation suite, against real Postgres 18 and PgBouncer (agents run in containers and create instances through the host's Docker; S3 is faked; import sources are `supabase/postgres` and Postgres 15). |
| `make pg-image` | Builds `xenibase-postgres:18-walg3.0.9`, the image dedicated instances run. |
| `make test-acme` | Obtains a real certificate over HTTP-01 from Pebble (Let's Encrypt's test CA). |
| `make test-e2e` | Installs the compose bundle from scratch and drives a browser from a fresh install to a working database, then backs up, deletes data, restores and verifies, imports a Supabase-shaped project and serves its app, creates a dedicated project and restores it to a point in time, promotes a hobby project while a writer runs, checking the URL is unchanged and no commits are lost, and queries, browses, and charts a project from the SQL console, table browser, and metrics pages (Playwright). |
| `make test-sdk` | The SDKs' unit tests (TypeScript, Go, and Dart when installed); `TestSDKs` runs them live in `make test-integration`. |
| `make docs-site` | Renders docs/ into `dist/docs` (navigation, search) and fails on broken links. |
| `make test-docs` | Follows `docs/install.md` in a scratch clone (its marked commands, as written), then drives a browser from the fresh install to a working database. |
| `make test-load` | 150 shared projects, pgbench on 10: create latency, pooler overhead, noisy neighbour; writes `tmp/load-report.md` (needs `pgbench`). |
| `make release` | linux/amd64 and arm64 binaries, the source bundle, and checksums in `dist/` (CI does it on `v*` tags). |
| `make run-dev` | Runs the built `bin/xenibase-server` against the dev environment (`deploy/dev/server.env`). |
| `make lint` | `golangci-lint`. |
| `make release-check` | Fails if the server binary embeds only the placeholder UI. |

```sh
make dev-up build
make run-dev        # bin/xenibase-server with deploy/dev/server.env

curl http://127.0.0.1:8080/api/v1/version

# Create a shared-tier project. The response carries the password and
# connection URLs exactly once; they work when the operation succeeds.
curl -s -X POST localhost:8080/api/v1/projects -d '{"name": "My Blog"}' | jq
psql 'postgresql://my_blog_k2f9_owner:<password>@127.0.0.1:6543/my_blog_k2f9?sslmode=disable'

# Enqueue a dummy operation and stream its progress (Server-Sent Events).
ID=$(curl -s -X POST localhost:8080/api/v1/dev/operations \
  -d '{"steps": 5, "delay_ms": 500, "fail_attempts": 1}' | jq -r .id)
curl -N localhost:8080/api/v1/operations/$ID/stream
```

Migrations run automatically on start. The dev server prints the first-run
setup code in its log; `XENIBASE_SETUP_CODE` fixes it instead.

## Operator authentication

One owner account (spec §7.2): an argon2id password plus a TOTP second
factor that is enrolled before the account exists. Other accounts sign in with
a password and can turn on a TOTP second factor from Account → Security
(platform staff cannot turn it off). Sessions live in the
metadata DB (tokens hashed at rest) behind `HttpOnly`, `Secure`,
`SameSite=Strict` cookies with a 12-hour idle timeout. Every mutating API
call needs the double-submit CSRF token, and destructive ones (deleting a
project) need a re-authentication from the last 10 minutes. Logins are rate
limited per address and lock the account after 5 failures. Every mutating
request, including refused ones, lands in the audit log.

## Projects (shared tier)

`POST /api/v1/projects` records the project and queues a `create` operation
(spec §6.1). It creates the owner role and database on the shared cluster,
hardens them (§7.1), adds the pooler route and SCRAM auth entry, reloads
both PgBouncers, and smoke-tests a login through each before marking the
project active. Each step is idempotent; if the operation finally fails, a
rollback drops everything it created. `rotate-password` and `DELETE` work the
same way. Xenibase stores only SCRAM verifiers: the pooler authenticates to
the backend with SCRAM passthrough, so no plaintext password is kept.

The isolation suite (`test/isolation`) creates two projects and asserts that
neither can reach the other's database, objects, sessions, or the server's
files. Decisions that depart from the spec are in
[docs/decisions.md](docs/decisions.md).

## Agents, backups, and restores

`xenibase-agent` runs on each node, next to Postgres (spec §3.2). xenibase-server
reaches it over mutual TLS: the server runs its own CA (key sealed with the
master key), signs each agent's certificate at registration, and pins its
fingerprint. An agent registers once with a one-time token (Nodes →
Register agent) or, for the install bundle's own agent,
`XENIBASE_AGENT_BOOTSTRAP_TOKEN`. It reports health and host metrics, and
runs `pg_dump`/`pg_restore` for backups, restores, and imports, with
passwords passed in the environment, never on the command line.

Backups are encrypted on the agent before they leave the node (AES-256-GCM
in 64 KiB chunks, with a per-object key wrapped by the backup key) and
streamed to S3 as `projects/<id>/logical/<time>.dump.enc`. Every active
project is backed up nightly, spread over a window starting at
`XENIBASE_BACKUP_HOUR` UTC, and keeps 7 daily plus 4 weekly backups. Deleting
a project takes a final backup and restoring in place takes a safety
backup first; both are kept 30 days. A weekly job restores the latest
backup of a random project into a scratch database and counts every
table, and the metadata DB backs itself up nightly (see
[docs/disaster-recovery.md](docs/disaster-recovery.md)).

Restore goes into a new project by default, so nothing is overwritten. In
place needs the typed project name and a fresh re-authentication; clients
wait at the pooler while it runs, and a failure puts the safety backup back.

## Dedicated projects and nodes

A project can live on the **dedicated tier** instead (spec §4.2): its own
PostgreSQL 18 container on a node, with CPU and memory limits from a profile
(`small` 1 CPU / 1 GB, `medium` 2 / 4 GB, `large` 4 / 8 GB) and its own
volume. The agent runs it from the `xenibase-postgres` image
(`make pg-image`, or `deploy/images/postgres`), which adds WAL-G. Clients
connect through the same poolers with the same URL format as shared
projects. WAL is archived continuously to the backup bucket, encrypted with
a key derived from the backup key; a base backup is taken at creation and
daily, keeping 7. **Point-in-time recovery** (Backups tab, or
`POST /projects/{id}/pitr`) restores any moment in that window into a new
dedicated project; the source is not changed.

Nodes are added on the Nodes page: Xenibase records the node and prints the
`xenibase-agent register` command with a one-time token. A node's role
(`shared`, `dedicated`, `both`) decides where new projects go; a shared or
both node can run an extra shared cluster (Node → Create shared cluster),
and new shared projects go to the least loaded one. The install bundle's
agent manages instances on the local Docker host.

### Promotion

A shared project can be **promoted** to the dedicated tier (Settings →
Promote to dedicated, or `POST /projects/{id}/promote`; spec §6.6). The
wizard shows the database size and the expected write freeze. Xenibase creates
the dedicated instance with the same role and password, then freezes writes
(transaction-mode clients wait in the pooler; session-mode clients are
disconnected once and reconnect), copies the data, checks every table's row
count and sequence, and moves the pooler route. The connection strings do
not change. The shared copy stays read-only for 48 hours and is then
dropped. If anything fails before the switch, the project stays on the
shared tier, writable, and the new instance is removed.

## SQL console, table browser, and metrics

Each project has an **SQL** tab (spec §8.5): a CodeMirror editor where
Ctrl/Cmd+Enter runs the selection (or everything), with a statement timeout
(30 s by default), a **Cancel** button, at most 1,000 rows per result, CSV
export of the shown rows, and a query history kept in the browser only. The
server stores just an audit entry (`project.console`), never the SQL. Queries
run as the project's role: xenibase-server signs in as the project's
`<db>_console` role, which may `SET ROLE` to the owner but inherits nothing,
so `RESET ROLE` gains no privileges. With **SQL console is read-only** on
(Settings; the default for dedicated and promoted projects), each submission
is one statement inside `BEGIN READ ONLY … ROLLBACK`.

The **Tables** tab is a read-only browser (spec §8.6): schemas, tables and
views with columns, types, indexes, row estimate, and size, and the rows 50
at a time, keyset-paginated on the primary key (on `ctid` for tables
without one).

The **Metrics** tab charts database size, connections (active and idle
backends, pooler clients), transactions per second, and the cache hit ratio
over 1 hour, 24 hours, or 7 days, plus the top 10 queries by total time once
`pg_stat_statements` is enabled (spec §8.7). Node pages chart CPU, load,
memory, disk, and disk I/O. Points are sampled every minute into
`metric_points`, averaged into hourly points, and kept for 24 hours (1-minute)
and 30 days (hourly). `GET /metrics` serves the latest values in the
Prometheus text format to signed-in operators or with
`Authorization: Bearer $XENIBASE_METRICS_TOKEN`. Project series are labelled
`project_id`, `org_id`, `org`, and `tier`, and node series `node_id` and
`node`. Project and database names are left out on purpose: names inside an
organisation belong to its members, not to whoever runs the scraper.

Settings → **Extensions** enables extensions from the allow-list (spec
§7.4): `pgcrypto`, `uuid-ossp`, `citext`, `pg_trgm`, `hstore`, `unaccent`,
`btree_gin`, `btree_gist`, `pg_stat_statements`, and `vector` everywhere,
plus `postgis`, `pg_partman`, `timescaledb`, `postgres_fdw`, and `pg_cron` on
dedicated instances, when the server has them installed.

## Import from an existing database

Projects → Import copies a database (Supabase included) into a new project
(spec §6.8). The preflight reports the source's version, size, schemas,
extensions against the allow-list, and grants and RLS policies that name
Supabase roles; Supabase-managed schemas (`auth`, `storage`, …) are skipped
by default. The agent then streams `pg_dump --no-owner --no-acl -n …` into
`pg_restore --role=<project owner>` and the result is verified by per-table
row counts and sequence values. For Supabase sources Xenibase adds
`auth.uid()`, `auth.role()`, and `auth.jwt()` (reading `request.jwt.claims`
like Supabase) and NOLOGIN stand-ins for `anon`, `authenticated`, and
`service_role`, so policies restore as written. The source connection string
stays in memory for the operation only, and the source is never modified.

## Operations

Long actions run as rows in the `operations` table (spec §6). Workers claim
them with `FOR UPDATE SKIP LOCKED`, so any number of servers can share the
queue. Each attempt heartbeats its lease; an operation whose worker dies is
requeued after the lease expires (30s) and resumes elsewhere. Failures retry
with exponential backoff up to the kind's attempt limit; handlers return
`jobs.Permanent(err)` to fail immediately. A table trigger publishes changes
with `NOTIFY`, which wakes idle workers and drives the SSE stream at
`/api/v1/operations/{id}/stream`.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `XENIBASE_LISTEN_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `XENIBASE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `XENIBASE_LOG_FORMAT` | `json` | `json` or `text` |
| `XENIBASE_SHUTDOWN_TIMEOUT` | `15s` | Graceful shutdown limit |
| `XENIBASE_DATABASE_URL` | *required* | Metadata DB connection string |
| `XENIBASE_MASTER_KEY` / `XENIBASE_MASTER_KEY_FILE` | *required (one)* | Base64 32-byte key encrypting secrets at rest. Generate with `xenibase-server -gen-master-key`. |
| `XENIBASE_MASTER_KEY_PREVIOUS` | | Comma-separated old keys that stay readable during a rotation |
| `XENIBASE_WORKERS` | `4` | Operations run concurrently |
| `XENIBASE_DEV_ENDPOINTS` | `false` | Enables `/api/v1/dev/*`. Never in production. |
| `XENIBASE_SHARED_ADMIN_URL` | | Shared cluster superuser (`xenibase_admin`) URL; registers the cluster at startup |
| `XENIBASE_SHARED_NODE_NAME` | `local` | Name of that node |
| `XENIBASE_SHARED_POOLER_HOST` / `_PORT` | admin URL's | How the poolers reach the cluster |
| `XENIBASE_POOLER_CONFIG_DIR` | | Where `databases.ini` and `userlist.txt` are written; enables provisioning |
| `XENIBASE_POOLER_FILE_MODE` | `0640` | Mode of the generated files |
| `XENIBASE_POOLER_ADMIN_USER` | `xenibase` | PgBouncer admin console user |
| `XENIBASE_POOLER_ADMIN_PASSWORD` / `_FILE` | *required with the dir* | Its password |
| `XENIBASE_POOLER_SESSION_ADDR` / `_POOLED_ADDR` | public host and ports | The two poolers as xenibase-server reaches them |
| `XENIBASE_POOLER_SSLMODE` | `prefer` | For admin and smoke-test connections |
| `XENIBASE_DB_HOST` | `localhost` | Host in client connection strings, e.g. `db.example.com` |
| `XENIBASE_DB_SESSION_PORT` / `_POOLED_PORT` | `5432` / `6543` | Ports in client connection strings |
| `XENIBASE_DB_SSLMODE` | `require` | `sslmode` in client connection strings |
| `XENIBASE_COOKIE_SECURE` | `true` | Secure, `__Host-` cookies; `false` only for plain-HTTP dev |
| `XENIBASE_TRUSTED_PROXIES` | | CIDRs whose `X-Forwarded-For` is trusted (Caddy) |
| `XENIBASE_PUBLIC_IPS` | | This host's public IPs, for the DB hostname DNS check |
| `XENIBASE_SETUP_CODE` | random | Fixes the first-run setup code |
| `XENIBASE_POOLER_TLS` | `self-signed` | Pooler certificate: `self-signed`, `acme`, `files`, or `off` |
| `XENIBASE_DATA_DIR` | `/var/lib/xenibase` | ACME account and certificate storage |
| `XENIBASE_ACME_EMAIL` / `_CA` / `_CA_ROOTS` | Let's Encrypt | ACME account email, directory URL, extra trusted roots |
| `XENIBASE_POOLER_TLS_CERT` / `_KEY` | | Certificate files for mode `files` |
| `XENIBASE_AGENT_BOOTSTRAP_TOKEN` | | Lets the bundled agent register the local node without an operator (24+ characters) |
| `XENIBASE_BACKUP_HOUR` | `2` | UTC hour the nightly backup window opens |
| `XENIBASE_BACKUP_JITTER` | `2h` | How widely projects are spread over the window |
| `XENIBASE_METADATA_BACKUP_URL` | `XENIBASE_DATABASE_URL` | The metadata DB as the agent reaches it, for self-backups; `off` disables them |
| `XENIBASE_SHARED_NODE_ROLE` | `both` | Role of the node registered from `XENIBASE_SHARED_ADMIN_URL`, when first registered |
| `XENIBASE_DEDICATED_ADMIN_VIA` | `network` | How xenibase-server reaches dedicated instances: `network` (the agent's Docker network) or `published` (the port published on the node) |
| `XENIBASE_CONSOLE_DISABLED` | `false` | Turns the SQL console and table browser off for every project |
| `XENIBASE_METRICS_INTERVAL` | `1m` | How often project and node metrics are sampled |
| `XENIBASE_METRICS_TOKEN` | | Bearer token for scraping `/metrics` (24+ characters); without it only signed-in operators can read it |
| `XENIBASE_ALERTS_INTERVAL` | `30s` | How often alert conditions are checked |
| `XENIBASE_PUBLIC_URL` | | The web UI's address, for links in alerts (`install.sh` sets it) |

xenibase-agent reads `XENIBASE_AGENT_STATE_DIR` (`/var/lib/xenibase-agent`),
`_LISTEN` (`:7070`), `_SERVER`, `_TOKEN`, `_BOOTSTRAP_TOKEN` with `_NODE`,
`_ADVERTISE` (how the server reaches it), `_SERVER_CA`, `_PG_BIN`, and
`_DISK_PATH`, and for instances `_DOCKER` (daemon address), `_PG_IMAGE`,
`_NETWORK` (Docker network to join), `_PUBLISH` (address to publish
ports on), and `_DB_ALLOW` (CIDRs new instances accept logins from; default
the private ranges); see `xenibase-agent run -h`.


