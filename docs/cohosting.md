# Xenibase and Taskiem on one server

This guide puts Xenibase and Taskiem on one Linux server, from a fresh
Ubuntu 24.04 machine to both apps serving, then checks the result and
drills an upgrade. Taskiem's database is a Xenibase **dedicated project**
on the same server. It was written for a Whogohost **VDS M** in Nigeria
(KVM, 4 vCores, 32 GB RAM, 600 GB NVMe), and works the same on any host
of that size.

Follow it top to bottom on a throwaway VM first: it doubles as the script
for that rehearsal. Each step ends with what to check before going on.
Where the rehearsal differs from production, a **Rehearsal** note says so.

## How the two fit together

Both apps need ports 80 and 443, and only one program can listen on them.
That program is **Xenibase's Caddy**: Taskiem runs without a proxy of its
own, and Xenibase's Caddy serves Taskiem's sites as well as its own.

| | Xenibase | Taskiem |
| --- | --- | --- |
| Runs from | `/opt/xenibase/deploy/compose` (the compose bundle, [install](install.md)) | Taskiem's VM bundle (`deploy/vm` in Taskiem's repository): `taskiem`, `taskiem-public`, `taskiem-mail`, OpenBao, ClamAV |
| Ports | 80, 443 and 443/udp for HTTP/3 (Caddy), 5432 and 6543 (the poolers) | none for the web (mail only, if Taskiem receives it) |
| Database | its own metadata database and shared cluster | a dedicated project on Xenibase, Postgres 18, role management on |

They meet in three places, the **co-hosting contract**. Both sides
implement exactly this:

1. **A shared Docker network**, named by `XENIBASE_COHOST_NETWORK`
   (default `cohost`). Xenibase's `caddy` container joins it, in addition
   to Xenibase's own network; Taskiem's `taskiem` and `taskiem-public`
   containers join it too. Caddy then reaches `taskiem:8080`,
   `taskiem-public:8082` and `taskiem-public:9090` by name. Whichever
   installer runs first creates the network (`docker network create
   cohost` when it's missing); both are safe to run again.
2. **Files in Xenibase's `caddy.d`**, the directory
   `/opt/xenibase/deploy/compose/caddy.d` on the host (mounted read-only
   in the container at `/etc/caddy/caddy.d`). Taskiem writes two files and
   nothing else there:
   - `taskiem.caddy`, its sites: the app domain with `reverse_proxy
     taskiem:8080`, and a catch-all `https://` site with `tls { on_demand }`
     and `reverse_proxy taskiem-public:8082` (businesses' own domains and
     the preview wildcard domain);
   - `taskiem.global`, its global options: `on_demand_tls { ask
     http://taskiem-public:9090/tls/ask }`. Xenibase's Caddyfile imports
     `caddy.d/*.global` inside its global options block, and
     `caddy.d/*.caddy` among its sites.
3. **A reload.** After writing them, Taskiem reloads Xenibase's Caddy:

   ```sh
   docker exec xenibase-caddy-1 caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
   ```

   `xenibase-caddy-1` is the container's name (compose project
   `xenibase`, service `caddy`), unless Xenibase runs with another
   `COMPOSE_PROJECT_NAME`. A name-independent way to find it:
   `docker ps -q --filter label=com.docker.compose.project=xenibase --filter label=com.docker.compose.service=caddy`.

What each side promises beyond that:

- **Xenibase** never deletes or rewrites a file in `caddy.d` it didn't
  write (it writes only `api.caddy`, with backend services on); install,
  upgrade and rollback leave `taskiem.caddy` and `taskiem.global` alone,
  and Caddy reads them again whenever it restarts. `caddy.d/*.caddy` and
  `caddy.d/*.global` are git-ignored, so they don't count as local changes
  to `upgrade.sh`.
- **Taskiem** writes `taskiem.global` before `taskiem.caddy` (Caddy refuses
  on-demand TLS without the `ask` endpoint), and if the reload fails puts
  the previous files back: a failed reload leaves the running config in
  place, but the next restart of Caddy (an upgrade of Xenibase) would read
  the broken file and the web UI would stay down with it. It publishes no
  ports 80 or 443, and names nothing on the shared network after
  Xenibase's services (`caddy`, `xenibase-server`, …).

On port 80, Caddy answers the HTTP-01 challenges of the certificates it
gets itself (Taskiem's on-demand ones included) before anything else; the
remaining challenges go to xenibase-server, which gets the database
hostname's certificate for the poolers. Other plain-HTTP requests are
redirected to HTTPS on the same name.

## 1. What you need

- **The server**, Ubuntu 24.04, with SSH access as a sudo user.
- **DNS records**, all pointing at the server's public address (A; AAAA
  too only if the server answers on IPv6):

  | Name (example) | Record | For |
  | --- | --- | --- |
  | `xenibase.example.com` | A | Xenibase's web UI |
  | `db.example.com` | A | the database hostname apps connect to (the poolers) |
  | `app.taskiem.example` | A | Taskiem's app |
  | `*.preview.taskiem.example` | A (wildcard) | Taskiem's preview pages; each name gets its own certificate on demand |
  | `taskiem.example` | MX (optional) | mail to Taskiem, if `taskiem-mail` receives it (Taskiem's runbook) |

  Businesses point their own domains at the server later, as Taskiem's
  runbook says; Caddy gets each one's certificate on its first visit, once
  Taskiem's `ask` endpoint approves it.
- **Object storage abroad** for backups: Whogohost has none. An
  S3-compatible bucket at, for example, Contabo Object Storage, with an
  access key that can read, write, list and delete in it. And a **second
  bucket at another provider** (Cloudflare R2, Backblaze B2, another
  Contabo region) as the copy target. Backups then leave Nigeria: don't
  offer data residency with this setup.
- **An SMTP server** for Xenibase's email, as in [install](install.md).
- **An authenticator app** for the platform admin's two-factor sign-in.

**Rehearsal:** use throwaway names under a domain you control
(`xb-rehearsal.example.com`, `db-rehearsal.example.com`,
`app.tk-rehearsal.example.com`, `*.preview.tk-rehearsal.example.com`), a
bucket and prefix of their own (`rehearsal/`), and a TTL of 300 so the
records can be deleted afterwards.

Check the names resolve before going on:

```sh
for n in xenibase.example.com db.example.com app.taskiem.example x.preview.taskiem.example; do
  printf '%s -> %s\n' "$n" "$(dig +short "$n" | tail -1)"
done
```

## 2. Prepare the server

```sh
sudo apt update && sudo apt -y upgrade
sudo apt -y install git curl dnsutils
sudo timedatectl set-ntp true

sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp
sudo ufw allow 5432/tcp
sudo ufw allow 6543/tcp
sudo ufw enable
```

Open 25/tcp too if `taskiem-mail` receives mail. Ports that containers
publish bypass `ufw` (Docker writes its own rules), so the rule that
matters is that nothing but Xenibase's Caddy and poolers, and Taskiem's
mail, publishes a port.

Install Docker and get Xenibase:

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"     # then log out and back in
sudo mkdir -p /opt/xenibase && sudo chown "$USER" /opt/xenibase
git clone https://github.com/israel-duff/xenibase.git /opt/xenibase
cd /opt/xenibase
git checkout <newest release tag>
ls deploy/compose/compose.cohost.yaml   # co-hosting needs a release that has this file
```

## 3. Install Xenibase with co-hosting on

Co-hosting is one line in `deploy/compose/.env`:

```sh
COMPOSE_FILE=compose.yaml:compose.cohost.yaml
```

Docker Compose reads `COMPOSE_FILE` from `.env` for every command run in
`deploy/compose`, so `install.sh`, `upgrade.sh` (and its rollback),
`enable-services.sh`, `deploy-ssh.sh` and your own `docker compose …`
all read `compose.cohost.yaml` too. It joins `caddy` to the network
`XENIBASE_COHOST_NETWORK` (default `cohost`), and makes port 80 redirect
hosts without a site of their own to HTTPS on their own name (without it,
they go to Xenibase's UI). `install.sh` creates the network if it doesn't
exist.

### Sizing: sharing the server with another app

The shared cluster's defaults assume Xenibase has an 8 GB server to
itself. On a shared server, set its memory in `.env` so Xenibase,
Taskiem's containers, Taskiem's dedicated instance and the page cache
all fit:

| Setting (`.env`) | Default | VDS M, 32 GB, Taskiem about 8 GB | 12 GB host |
| --- | --- | --- | --- |
| `SHARED_PG_SHARED_BUFFERS` | `2GB` | `4GB` | `1GB` |
| `SHARED_PG_EFFECTIVE_CACHE_SIZE` | `6GB` | `12GB` | `2GB` |
| `SHARED_PG_MAX_CONNECTIONS` | `500` | `300` | `200` |
| `SHARED_PG_WORK_MEM` | `8MB` | `8MB` | `4MB` |
| `SHARED_PG_MAINTENANCE_WORK_MEM` | `256MB` | `512MB` | `128MB` |
| `SHARED_PG_AUTOVACUUM_MAX_WORKERS` | `5` | `3` | `2` |
| `XENIBASE_EDGE_MEMORY` (backend services only) | `1g` | `1g` | `512m` |
| `XENIBASE_COHOST_RESERVE_MB` (for `preflight.sh`) | `8192` | `8192` | `6144` |

How the VDS M adds up: the shared cluster's 4 GB of buffers plus about
2 GB for its connections, the metadata database, xenibase-server, the
agent, the poolers and Caddy; Taskiem's containers about 8 GB (ClamAV
alone holds 1 to 2 GB); Taskiem's dedicated instance 1 GB (the smallest
size); the edge 1 GB if backend services are on. That is about 16 GB,
leaving about 16 GB for the page cache, more dedicated projects, and
growth. On 12 GB it is about 9 GB plus Taskiem's 1 GB instance: it runs,
with little room for more dedicated projects. `max_connections` of 300
and 200 are far above what PgBouncer's pools hold open; lower it further
only with the shared projects' connection limits in mind.

### Install

Set the two names, build without starting (this writes `.env` with fresh
secrets), add the co-hosting line and the profile, then start:

```sh
cd /opt/xenibase/deploy/compose
./preflight.sh xenibase.example.com db.example.com

export XENIBASE_UI_DOMAIN=xenibase.example.com XENIBASE_ACME_EMAIL=ops@example.com
./install.sh --build-only

cat >> .env <<'ENV'
# Co-hosting with Taskiem (docs/cohosting.md).
COMPOSE_FILE=compose.yaml:compose.cohost.yaml
XENIBASE_COHOST_NETWORK=cohost
XENIBASE_COHOST_RESERVE_MB=8192
# Sharing the server: the VDS M profile.
SHARED_PG_SHARED_BUFFERS=4GB
SHARED_PG_EFFECTIVE_CACHE_SIZE=12GB
SHARED_PG_MAX_CONNECTIONS=300
SHARED_PG_WORK_MEM=8MB
SHARED_PG_MAINTENANCE_WORK_MEM=512MB
SHARED_PG_AUTOVACUUM_MAX_WORKERS=3
ENV
./preflight.sh xenibase.example.com db.example.com   # the memory line now counts Taskiem
./install.sh --no-build
```

Run the build inside `tmux` or `screen`. `preflight.sh` warns (never
fails) when the server has less memory than the configured services need,
with Taskiem's share (`XENIBASE_COHOST_RESERVE_MB`) included once
co-hosting is on. Its "port in use" lines are about a server before the
install: on a server where Xenibase already runs, ports 80 to 6543 are
Xenibase's.

To turn co-hosting on for an install that already runs, add the
`COMPOSE_FILE` line to `.env` and run `./install.sh --no-build`: it
creates the network and recreates Caddy on it.

Check:

```sh
docker compose ps                                   # every service running or healthy
docker network inspect cohost --format '{{range .Containers}}{{.Name}} {{end}}'
                                                    # xenibase-caddy-1 is listed
docker compose exec -T shared-pg psql -U xenibase_admin -d postgres -Atc 'show shared_buffers; show max_connections'
```

**Back up `.env` now**, off this server: `XENIBASE_MASTER_KEY` in it
decrypts every secret Xenibase stores.

## 4. First-run setup

Open `https://xenibase.example.com` and go through the setup wizard as in
[install, the setup wizard](install.md#6-the-setup-wizard): the setup code
the installer printed, the platform admin account with two-factor sign-in
and its recovery codes, email, the **database hostname**
(`db.example.com`), then:

- **Backup storage**: the bucket abroad. For Contabo Object Storage, the
  endpoint is the region's S3 URL shown with the bucket in Contabo's panel
  (without the bucket name), the bucket, and the S3 access key and secret
  from the panel's object storage credentials. Xenibase writes, reads and
  deletes a test object before saving.
- **Backup key**: download it and store it **offline**, apart from `.env`,
  then paste it back to confirm. Without it no backup can be restored.
- **Local node**: the bundled agent shows as healthy. Its role is `both`:
  it runs the shared cluster and dedicated instances.

Add a second platform admin once a colleague has an account
([install](install.md#6-the-setup-wizard)).

## 5. The copy target

Backups should exist in two places. Add the second bucket as a platform
storage target (Settings → Platform storage targets → Add), then make it
the home region's **copy target** (Platform → Regions → the home region →
Copy target). Succeeded backups are then copied there by checksum
([regions](regions.md), [operations](operations.md#storage-targets-and-project-keys)).

## 6. Taskiem's database: a dedicated project

1. **An organisation for your own apps.** Top bar → organisation
   switcher → **New organisation**, for example `Internal`. You are its
   owner. A dedicated instance beyond its allowance becomes a request
   that you approve as platform admin (Admin → Dedicated requests).
2. **The project.** In that organisation: Projects → **New project**,
   name `taskiem`, tier **Dedicated**, node *Least loaded* (the local
   node), size **small** (1 CPU, 1 GB), Postgres **18**. Or with the API
   and a token of yours ([CLI and API tokens](cli.md)):

   ```sh
   curl -sS https://xenibase.example.com/api/v1/projects \
     -H "Authorization: Bearer $XENIBASE_TOKEN" -H 'Content-Type: application/json' \
     -d '{"org_id": "<org id>", "name": "taskiem", "tier": "dedicated", "profile": "small", "pg_version": 18}'
   ```

   Save the password the answer (or the page) shows once.
3. **Role management.** Taskiem's migrations create their own roles, and
   its installer creates the logins `<database>_app` and
   `<database>_public`. Project Settings → Compute → **Database roles** →
   **Let the owner create roles** (it asks for your password and a code),
   or with the API from a signed-in session, after a step-up
   (`POST /api/v1/auth/reauth`):

   ```http
   PUT /api/v1/projects/{id}/manage-roles
   {"enabled": true}
   ```

   Details: [database roles](database-roles.md).
4. **The connection URL** for Taskiem: the owner login through the
   **session** pooler, port 5432 (Taskiem's workers hold `LISTEN`
   connections, which the transaction pooler doesn't keep), on the public
   database hostname, verifying the certificate:

   ```text
   postgres://<database>_owner:<password>@db.example.com:5432/<database>?sslmode=verify-full
   ```

   The database name and owner login are on the project's **Connect**
   tab. The poolers' certificate is from Let's Encrypt; with `psql` and
   other libpq tools add `&sslrootcert=system`, drivers that use the
   system's CA store need nothing more.

   Taskiem's containers reach `db.example.com` at the server's own
   public address; check it from a container on the shared network:

   ```sh
   docker run --rm --network cohost postgres:18 \
     psql "postgres://<database>_owner:<password>@db.example.com:5432/<database>?sslmode=verify-full&sslrootcert=system" -c 'select 1'
   ```

   Leave the project's IP allowlist ([network access](network-access.md))
   off: through Docker, the poolers don't see the server's public address
   as the source of these connections.

## 7. Install Taskiem

Hand over to Taskiem's runbook (`docs/vm.md` in Taskiem's repository,
sections "Sharing the server with Xenibase" and "A database on Xenibase
(dedicated project)") for `taskiem-vps setup` with its external proxy.
These are Taskiem's settings for it, with the values from this guide:

| Taskiem setting | Value | Xenibase's side |
| --- | --- | --- |
| `TASKIEM_VPS_PROXY` | `external` | Taskiem runs no proxy of its own |
| `TASKIEM_VPS_PROXY_DIR` | `/opt/xenibase/deploy/compose/caddy.d` | the host directory Caddy mounts at `/etc/caddy/caddy.d` |
| `TASKIEM_VPS_PROXY_CONTAINER` | `xenibase-caddy-1` (Taskiem's default) | Caddy's container |
| `TASKIEM_VPS_PROXY_NETWORK` | `cohost` (Taskiem's default) | must equal `XENIBASE_COHOST_NETWORK` |
| `TASKIEM_VPS_DATABASE_URL` | the owner URL from step 6 (session pooler, port 5432, `sslmode=verify-full`) | |
| `TASKIEM_VPS_XENIBASE_URL`, `TASKIEM_VPS_XENIBASE_TOKEN`, `TASKIEM_VPS_XENIBASE_PROJECT` (optional) | `https://xenibase.example.com`, an API token of a member of the project, the project's ID | setup then calls `POST /api/v1/projects/{id}/manage-roles/sync` once, so its new logins connect at once |

The app and preview domains are Taskiem's own settings. Setup waits up to
90 seconds (`TASKIEM_VPS_LOGIN_WAIT`) for the logins it creates to reach
the poolers; without the sync call above they arrive within 30 seconds.
Setup also runs `ufw allow 5432/tcp` and `ufw allow 6543/tcp` in this mode.

Taskiem's `taskiem.caddy` holds its app domain, the on-demand catch-all
`https://` site, and `http://*.<preview domain>` redirecting to HTTPS.

Taskiem's runbook documents its own steps. When it is done, check on the
Xenibase side:

```sh
ls -l /opt/xenibase/deploy/compose/caddy.d          # taskiem.caddy and taskiem.global
docker network inspect cohost --format '{{range .Containers}}{{.Name}} {{end}}'
                                                    # xenibase-caddy-1, taskiem, taskiem-public
docker exec xenibase-caddy-1 caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
```

## 8. Checks

**Certificates for every name.** Each should answer with a certificate
from Let's Encrypt (or ZeroSSL, Caddy's fallback):

```sh
for u in https://xenibase.example.com https://app.taskiem.example https://check.preview.taskiem.example; do
  curl -sS -o /dev/null -w "%{http_code} $u\n" "$u"
done
openssl s_client -connect db.example.com:5432 -starttls postgres -servername db.example.com </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -enddate
cd /opt/xenibase/deploy/compose && docker compose logs caddy | grep -i 'certificate obtained'
```

The preview name gets its certificate on its first visit, if Taskiem's
`ask` endpoint approves it (a name Taskiem doesn't know is refused, which
is the point of the endpoint). Plain HTTP goes to HTTPS on the same name:
`curl -sI http://app.taskiem.example/` answers with a redirect to
`https://app.taskiem.example/`.

**`LISTEN` through the session pooler.** In one `psql` session on port
5432, a notification arrives:

```sh
psql "postgres://<database>_owner:<password>@db.example.com:5432/<database>?sslmode=verify-full&sslrootcert=system" \
  -c 'LISTEN cohost_check' -c "NOTIFY cohost_check, 'ok'"
# Asynchronous notification "cohost_check" with payload "ok" received ...
```

Taskiem's own health check, from its runbook, then shows its workers
connected.

**A backup.** Project `taskiem` → Backups → **Back up now** (or `POST
/api/v1/projects/{id}/backups`). It succeeds, and once the copy has run
the backup shows its copy on the copy target.

**A point-in-time restore.** Note the time, write a row in Taskiem (or
`create table cohost_check(t timestamptz default now()); insert into
cohost_check default values;` as the owner), wait a minute, then
Backups → **Point-in-time recovery** → that time → **Restore to this
point**, or:

```http
POST /api/v1/projects/{id}/pitr
{"name": "taskiem-pitr-check", "target_time": "2026-11-21T10:15:00Z"}
```

It makes a new dedicated project on the same node and leaves Taskiem's
alone. Connect to it with the password the answer shows, check the row is
there, then delete the new project.

## 9. An upgrade drill

Upgrade Xenibase as usual ([upgrade](upgrade.md)), and check Taskiem
kept serving:

```sh
cd /opt/xenibase/deploy/compose
./upgrade.sh <next tag> --dry-run
./upgrade.sh <next tag>
ls caddy.d                                          # taskiem.caddy and taskiem.global are still there
docker network inspect cohost --format '{{range .Containers}}{{.Name}} {{end}}'
curl -sS -o /dev/null -w '%{http_code}\n' https://app.taskiem.example
docker exec xenibase-caddy-1 caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

`upgrade.sh` refuses a tag that lacks `compose.cohost.yaml` (a release
before co-hosting) before it changes anything, since compose would stop
at that file after the checkout. `install.sh` recreates Caddy when its
Caddyfile changed, so the dashboard and Taskiem's sites drop for a second
or two during an upgrade; Caddy keeps its certificates in its volume and
reads Taskiem's files again as it starts. Taskiem's own upgrades leave
Xenibase running: they rewrite their two files and reload.

**Rehearsal:** until there are two releases with co-hosting to go
between, drill the part an upgrade does after its build:
`./install.sh --no-build`, then the checks above. Finish the rehearsal by
restoring a backup into a new project once more, then delete the VM, the
DNS records and the rehearsal prefix in both buckets.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `network cohost declared as external, but could not be found` | Run `./install.sh --no-build` (it creates it), or `docker network create cohost`. |
| Taskiem's sites answer 502 | `docker network inspect cohost`: `taskiem` and `taskiem-public` must be on it. |
| `caddy reload` fails with "on-demand TLS cannot be enabled without a permission module" | `taskiem.global` is missing, or Caddy reads an older Caddyfile without the `*.global` import: `./install.sh --no-build` recreates it with this release's. |
| `caddy reload` fails otherwise | The running config is unchanged. Fix or put back Taskiem's previous files before Caddy restarts: `docker exec xenibase-caddy-1 caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile` names the line. |
| A business's domain gets no certificate | Its DNS must point here, and Taskiem's `ask` endpoint must approve it: `docker compose logs caddy \| grep -i on-demand`. |
| `http://` on a Taskiem domain goes to Xenibase's UI | Co-hosting isn't on: `COMPOSE_FILE` in `.env`, then `./install.sh --no-build`. |
| Taskiem can't connect to the database | The check in step 6 from a container on `cohost`; the project's IP allowlist must be off; role management must be on before Taskiem's installer creates its logins. |
