# Enabling backend services on a compose install

This is the full, step-by-step method for turning on backend services (the data
API, auth, storage and realtime) on a single server that was installed with the
compose bundle ([Install](install.md)). It is the method used on the Xenibase
production server, written down with the reasons for each choice and the
routes that were tried and set aside, so that the next person, or the next
server, doesn't have to rediscover them.

What the services are and how an app uses them is in
[Backend services](backend-services.md). What to do when one misbehaves is in
the [backend runbook](backend-runbook.md). This page is only about getting
them running.

## What you end up with

Every project that turns on backend services is served at its own address:

```text
https://<ref>.<api-domain>        for example https://k7f3m2q9.api.xenibase.ng
```

`<ref>` is a short random reference Xenibase gives the project. `<api-domain>`
is a name you choose once (here `api.xenibase.ng`); it must be a different name
from the web UI's. The edge reads the hostname to know which project a request
is for, so one project's key can't be used on another, and browsers keep each
project's cookies and storage apart.

This page sets up the platform's API domain, served here in the home
region. A region elsewhere (Lagos) can have its own, `<ref>.ng.<api-domain>`,
served by an edge in that region so its projects' API traffic stays there:
see [Regions](regions.md#a-regions-api-domain).

```text
app ──HTTPS──▶ <ref>.api.example.com
                  │  DNS: *.api.example.com  A  → this server
                  ▼
              Caddy (port 443)            wildcard certificate for *.api.example.com
                  │  plain HTTP, inside the Docker network
                  ▼
              xenibase-edge (:8443)       key, CORS and rate-limit checks
                  │
                  ▼
              transaction pooler ──▶ the project's database
```

Three things have to exist, and everything below is about them:

1. **A wildcard DNS record**, so every `<ref>` resolves to the server.
2. **A wildcard TLS certificate** for `*.<api-domain>`, so every `<ref>` is
   served over HTTPS. References are created on the fly, so a certificate per
   project isn't practical, and one wildcard certificate covers them all.
3. **The edge process** (`xenibase-edge`), running behind Caddy.

## Why the certificate needs a DNS challenge

Let's Encrypt only issues a wildcard certificate after you prove you control the
domain by publishing a temporary TXT record named
`_acme-challenge.<api-domain>`. (The ordinary HTTP challenge can't prove a
wildcard.) The certificate renews about every 60 days, and each renewal needs a
new TXT record, so the record has to be created and removed **by a program**,
not by hand. That means one of two things: your DNS host has an API Caddy can
use, or you delegate just that one record to something that does.

Where a domain's DNS is hosted decides which route you can take. The
[routes we considered](#the-routes-we-considered) section lists each, and why
the production server uses acme-dns.

## The method used in production: acme-dns and one CNAME

[acme-dns](https://github.com/joohoi/acme-dns) is a small DNS service built
for exactly this situation. It answers only `_acme-challenge` questions and has
a tiny API for setting the TXT value. You do three things:

1. **Register once** with an acme-dns server. It gives you a username, a
   password and a private name (`<uuid>.auth.acme-dns.io`).
2. **Add one CNAME** at your DNS host:
   `_acme-challenge.<api-domain>` → that private name. From then on, anyone
   asking for the challenge record is sent to acme-dns.
3. **Give Caddy the registration** (in `.env`). When it needs a certificate,
   Caddy writes the challenge value to acme-dns through its API; Let's Encrypt
   follows the CNAME, reads it there and issues the certificate.

Nothing else about your domain moves: the web UI, the database host and your
mail records stay at your existing DNS host, untouched.

### Trade-off to know about

Whoever runs the acme-dns server can answer the challenge for
`_acme-challenge.<api-domain>`, so in principle they could obtain a certificate
for names under the API domain. It cannot reach your web UI, database host or
mail, because those don't use that CNAME. This is fine for most setups. If it
matters to you, run your own acme-dns (it needs port 53 and its own NS records)
or move the API domain's DNS to a provider with an API (Cloudflare, deSEC).

## Before you start

- A compose install on **Xenibase 4.2.1 or later**. That release has the edge
  in the bundle; earlier ones don't (see [after an upgrade](#after-an-upgrade)).
- The server is reachable on ports 80 and 443 (the install already needs them).
  The edge itself is **not** published: only Caddy listens publicly.
- You can add records at your DNS host: one A record and one CNAME (wildcard
  names allowed in the A record).
- A recent backup of `deploy/compose/.env` kept off the server. The enable
  script adds to it.
- Plan for the server restarting its web UI for a minute or two while images
  rebuild. Databases stay up.

## Step by step

Examples use `app.xenibase.ng` for the web UI and `api.xenibase.ng` for the API
domain. Use your own names and your server's address.

### 1. Choose the API domain

Pick a name that is not the web UI's, under a domain you control. We use
`api.xenibase.ng`. Projects will live at `<ref>.api.xenibase.ng`.

### 2. Add the wildcard A record

At your DNS host:

| Name | Type | Value |
| --- | --- | --- |
| `*.api` | A | the server's public IPv4 address |

A short TTL (300 seconds) helps while you test. Add an AAAA record the same way
if the server has IPv6. Check it from your own computer:

```sh
dig +short test.api.xenibase.ng
# the server's address; nothing printed means it isn't live yet
```

If your DNS host's editor adds the domain for you, enter the name relative to
the zone (`*.api`), not the full `*.api.xenibase.ng`, or you will end up with
`*.api.xenibase.ng.xenibase.ng`.

### 3. Register with acme-dns

From any computer:

```sh
curl -s -X POST https://auth.acme-dns.io/register
```

It prints JSON like this (these are fake values):

```json
{"username":"…","password":"…","fulldomain":"9f48….auth.acme-dns.io","subdomain":"9f48…","allowfrom":[]}
```

- **`username` and `password` are secrets.** Put them in a password manager now.
  Don't paste them into chat, tickets or a repository.
- **`subdomain`** (a UUID) and **`fulldomain`** (that UUID plus
  `.auth.acme-dns.io`) are not secret.

You only register once. Re-registering gives a different private name, and the
CNAME would have to change with it.

### 4. Add the CNAME

At your DNS host:

| Name | Type | Value |
| --- | --- | --- |
| `_acme-challenge.api` | CNAME | the `fulldomain` from step 3 |

Check it:

```sh
dig +short CNAME _acme-challenge.api.xenibase.ng
# 9f48….auth.acme-dns.io.
```

### 5. Put the registration in `.env` on the server

On the server, edit the file yourself (the values never pass through anyone
else, and the script never asks for them):

```sh
cd ~/Xenibase/deploy/compose
nano .env
```

Add three lines (the `subdomain`, not the `fulldomain`):

```text
ACMEDNS_USERNAME=<username>
ACMEDNS_PASSWORD=<password>
ACMEDNS_SUBDOMAIN=<the subdomain UUID>
```

### 6. Run the enable script

```sh
./enable-services.sh api.xenibase.ng acmedns
```

It takes a few minutes (it builds Caddy with the acme-dns plugin, then
rebuilds and restarts what changed). Running it again is safe: it only adds
what is missing and never overwrites what you set. What it does is listed under
[what the script changes](#what-the-script-changes).

### 7. Check the certificate and the containers

```sh
docker compose ps
docker compose logs caddy | grep -i "certificate obtained"
```

You want `xenibase-edge` running and healthy, and a line saying a certificate
was obtained for `*.api.xenibase.ng`. The first certificate usually takes a
minute or two.

Then test the whole path (DNS, certificate, Caddy and the edge) from your own
computer, with a name no project has. Any single name under the API domain counts
as a project to the edge, so an unknown one gets the edge's own answer:

```sh
curl -s https://anything.api.xenibase.ng/
# {"error":{"code":"project_not_found","message":"no project with backend services at this hostname","request_id":"req_…"}}
```

That answer, **with a request id and no certificate error**, means the DNS record,
the wildcard certificate, Caddy and the edge are all working. A certificate error
means it isn't issued yet or the CNAME is wrong; no answer at all means DNS or the
firewall; `503 starting` means the edge hasn't loaded its configuration yet (wait a
few seconds).

The edge's own health check can't be reached through the public address (the
wildcard certificate covers one level of names, and every one-level name is a
project to the edge). Ask it from inside, using any host that isn't under the API
domain:

```sh
docker compose exec -T caddy wget -qO- --header='Host: health' http://xenibase-edge:8443/healthz
# {"status":"ok"}
```

### 8. Turn it on for a project and call it

Use a throwaway project first. In the app: the project's **Settings → API →
Enable backend services**. Xenibase shows the project's address and its keys
**once**; save them. Then, from your computer:

```sh
curl -H "apikey: xb_pub_…" https://<ref>.api.xenibase.ng/data/v1/health
# {"status":"ok","project":"<ref>","role":"anon","region":"…"}
```

How to use the data API, auth, storage and realtime from there is in
[Backend services](backend-services.md).

## What the script changes

`deploy/compose/enable-services.sh <api-domain> [cloudflare|desec|acmedns]`:

| Where | What |
| --- | --- |
| `.env` | Adds, only if missing: `XENIBASE_API_DOMAIN`, a generated `XENIBASE_EDGE_SECRET` (40 random characters; the server and the edge must share it), `DNS_PROVIDER`, `CADDY_IMAGE` (`xenibase-caddy-<provider>:local`), `COMPOSE_PROFILES=services`, and for acme-dns `ACMEDNS_SERVER_URL` (default `https://auth.acme-dns.io`). It never writes your tokens or passwords. |
| Docker images | Builds `xenibase-caddy-<provider>:local`: Caddy with the chosen DNS plugin (`deploy/compose/caddy-dns/Dockerfile`, built with `xcaddy`). The stock `caddy:2.10.0` image (the same version) stays the default for installs that don't use backend services. |
| `deploy/compose/caddy.d/api.caddy` | Written by the script, git-ignored. The wildcard site: `*.<api-domain>`, its DNS challenge settings, and `reverse_proxy xenibase-edge:8443` with buffering off (realtime and large downloads stream). The Caddyfile imports `caddy.d/*.caddy`; with no file there, nothing changes. |
| Containers | `docker compose up -d --build` starts `xenibase-edge` (a compose *profile*, off until `COMPOSE_PROFILES=services` is set) with a 1 GB memory limit, and recreates Caddy with the new image. |

The edge talks to xenibase-server at `http://xenibase-server:8080` and to the
poolers (`pooler-tx:6543`, `pooler-session:5432`) over the Docker network. Its
only listener, `:8443`, is not published to the host: Caddy is the only thing in
front of it (`XENIBASE_EDGE_TRUSTED_PROXIES` is Caddy's address).

It refuses to run if the API domain equals the web UI's name, if a needed
credential is missing from `.env`, or if the edge secret is shorter than 32
characters.

## Operating it

### Renewal

Caddy renews the certificate by itself, starting about 30 days
before it expires, using the same CNAME and the same acme-dns registration. It
keeps certificates and its account in the `caddy-data` volume. Check any time:

```sh
docker compose logs caddy | grep -iE "certificate|acme" | tail -20
```

If renewals fail, the existing certificate keeps working until it expires, and
Caddy keeps retrying. The common causes are the CNAME being removed, the
acme-dns credentials being wrong, or acme-dns being unreachable.

### After an upgrade

`upgrade.sh` checks out the release you give it and
rebuilds. Because the edge, the Caddyfile import and the script are in the
release, they carry over, and your `.env` and `caddy.d/api.caddy` are not
tracked by git and are left as they are. But `upgrade.sh`'s health check covers
the dashboard and the server, **not** the edge, so after every upgrade run:

```sh
docker compose ps
curl -s https://anything.api.xenibase.ng/      # project_not_found, with a request id
```

An install upgraded to a release **without** the edge in it (anything before
4.2.1) loses the wildcard site and the edge service quietly: the dashboard works
and project APIs stop answering. Don't upgrade across that boundary without
re-running the script on a release that has it.

### Turning it off

Remove `COMPOSE_PROFILES=services` from `.env`, delete
`deploy/compose/caddy.d/api.caddy`, and run
`docker compose up -d --remove-orphans`. Projects keep their keys' records but
their API addresses stop answering. (To remove one project's services, use
Settings → API → **Turn off**: that revokes its keys.)

### Rotating secrets


- *Edge secret:* change `XENIBASE_EDGE_SECRET` in `.env` and run
  `docker compose up -d`; the server and the edge restart together with the new
  value.
- *acme-dns credentials:* register again (step 3), change the three
  `ACMEDNS_*` values **and** the CNAME, then `docker compose up -d`.

### Resources

The edge is limited to 1 GB (`XENIBASE_EDGE_MEMORY`). Image
transforms run in workers of up to 512 MB each (`XENIBASE_EDGE_RENDER_MEMORY_MB`),
and the anonymous-read cache defaults to 64 MB (`XENIBASE_EDGE_CACHE_MB`). On an
8 GB server shared with the databases, watch memory after turning it on
(`docker stats`).

### What to back up

`.env` (it holds the master key and the settings above).
The certificate store is not precious: Caddy can get a new certificate in a
minute.

## The routes we considered

The production domain is `xenibase.ng`, a `.ng` name whose DNS is at a Nigerian
registrar's own DNS service. That ruled out the usual routes. In the order we
tried them:

| Route | What it needs | What happened |
| --- | --- | --- |
| **Cloudflare DNS**, the best-supported option: move the zone to Cloudflare and let Caddy use its API | The zone hosted on Cloudflare | The operator could not add the `.ng` zone to Cloudflare (it didn't accept the domain at the time). (If your domain can be on Cloudflare, `./enable-services.sh <api-domain> cloudflare` with a zone-limited `CLOUDFLARE_API_TOKEN` is the simplest route.) |
| **Delegate only the `api` subdomain** to another DNS host with an API, using NS records for `api` | NS records on a subdomain at the existing host | The registrar's DNS manager allowed NS records but capped them at two per zone, and the two `@` records for the domain's own nameservers used them up. |
| **deSEC with a free `dedyn.io` name**: a free zone to hold the challenge, reached by a CNAME (`_acme-challenge.api` → `_acme-challenge.<name>.dedyn.io`, plus `ACME_CHALLENGE_DOMAIN`) | A deSEC account and a zone | deSEC was not accepting new `dedyn.io` registrations (suspended, as it showed at the time). The script still supports this route (`./enable-services.sh <api-domain> desec`) for anyone with a deSEC zone, including a second domain you own, pointed at deSEC's nameservers. |
| **The registrar's reseller API** for DNS records | An API that adds a TXT record | See [the reseller API](#appendix-the-registrars-reseller-api). Its DNS calls answered *Function not supported* for `.ng` and `.africa`. |
| **acme-dns** with one CNAME | A CNAME at the existing DNS host | Works with any DNS host that allows a CNAME. **This is what production uses.** |
| *Per-project certificates on first visit* (no wildcard) | Only the `*.api` A record, plus a check that a name is a real project | Not built. Possible for testing, but Let's Encrypt limits how many certificates one domain can get per week, and Xenibase would need to answer "is this a real project?" so strangers can't make Caddy request certificates for made-up names. |
| *Running your own acme-dns* | Port 53 open, and NS records for a zone to host it | Not used: it needs the NS records we couldn't add. A good later step if the public service's trust trade-off is unwelcome. |

## Troubleshooting

These are the likely causes, in the order to check them. They come from how the
pieces work; if you hit something else, `docker compose logs caddy` and
`docker compose logs xenibase-edge` are where to look.

| Symptom | Check |
| --- | --- |
| The script stops with "set … in .env first" | The three `ACMEDNS_*` values (or the provider's token) must be in `.env` before running it. |
| `dig +short test.api.<domain>` prints nothing | The wildcard A record isn't live: check its name was entered relative to the zone (`*.api`), that it is an A record, and ask the DNS host's own nameserver directly: `dig +short test.api.<domain> @<its nameserver>`. |
| `dig CNAME _acme-challenge.api.<domain>` prints nothing or a different name | The CNAME is missing, misspelt, or points at an old registration. It must equal the `fulldomain` in `.env`'s registration. |
| The Caddy log says `unsupported protocol scheme ""` or `%7Benv.ACMEDNS_…%7D` | The acme-dns plugin was given an `{env.…}` placeholder where it only accepts the value: it expands them in the password only. `caddy.d/api.caddy` must have the username, subdomain and server URL written out. Re-run `./enable-services.sh` from release 4.2.2 or later, which writes them. |
| The Caddy log says `timed out waiting for record to fully propagate … last error: <nil>` | Caddy's own check of acme-dns's nameserver (a UDP lookup from this server) didn't see the record, though acme-dns has it: check with `dig TXT <subdomain>.auth.acme-dns.io @ns.auth.acme-dns.io`. Some networks drop those lookups. `caddy.d/api.caddy` needs `propagation_timeout -1` inside `tls { … }`, which release 4.2.2's script writes: Let's Encrypt checks the record itself. |
| No "certificate obtained" line after a few minutes | `docker compose logs caddy` shows why: an acme-dns authentication error means the username, password or subdomain in `.env` is wrong; a timeout or NXDOMAIN on the challenge name means the CNAME. Let's Encrypt limits repeated failures, so fix the cause before retrying. |
| The test `curl` fails with a certificate error | The certificate isn't issued yet, or Caddy is still serving the old configuration: `docker compose restart caddy`. |
| `503 starting` from the edge | It hasn't loaded its configuration from xenibase-server yet. It answers once it has; if it never does, the edge secret in `.env` doesn't match the server's, or the server is down. |
| `xenibase-edge` is `Restarting` | `docker compose logs xenibase-edge`: it prints why it exits. A setting it refuses (`XENIBASE_EDGE_TRUSTED_PROXIES` must be a network range such as `172.31.250.10/32`, not a bare address; `XENIBASE_EDGE_SECRET` at least 32 characters) exits at once. Out of memory during image transforms is the other usual one: raise `XENIBASE_EDGE_MEMORY`. |
| Project APIs stopped answering after an upgrade | See [after an upgrade](#after-an-upgrade): the release you moved to may not include the edge. |

## Security notes

- Tokens, passwords and the master key live in `.env` on the server, mode 600
  (the installer sets it). The enable script never asks for them and never
  writes them.
- A DNS provider token (Cloudflare, deSEC) should be limited to the one zone
  and, where the provider allows, to DNS edits only.
- The edge is reachable only through Caddy; the compose service publishes no
  port. Don't add one.
- The edge holds the credentials of the region's file storage. Give the
  region a **files target** of its own (Platform → Regions; see
  [regions](regions.md#files-target)), or the edge holds the backup
  target's, which can delete every backup in the region; the
  `files_share_backups` alert fires until you do.
- Backend services add a public surface: the data API, auth, storage and
  realtime answer on the internet. Read the
  [security review](security-review.md) and the [penetration test scope](pentest-scope.md)
  before putting customers' data behind them.

## Appendix: the registrar's reseller API

Recorded because it looked like the way to automate DNS, and may be worth
another look.

Some Nigerian registrars offer a domain *reseller* API built on the WHMCS
"Domains Reseller" add-on. Its shape, from the registrar module's code and
documentation:

- **Base URL:** `https://<registrar>/host/modules/addons/DomainsReseller/api/index.php`,
  and paths such as `GET /domains/{domain}/dns` (`GetDNS`) and
  `POST /domains/{domain}/dns` (`SaveDNS`).
- **Authentication:** request headers `username` (the account email), `token`,
  `timestamp` (unix time) and a `User-Agent`. The token is the base64 of the
  hex HMAC-SHA256 of the **API key**, keyed by `"<email>:<yy-mm-dd HH>"` in UTC,
  where the hour is the one in `timestamp`. It changes every hour.
- **`SaveDNS` takes the whole record list** (`domain` and `dnsrecords`, both
  required): there is no "add this record" call, so any program that uses it
  must read all records, add one, and write them all back. A mistake or a race
  would erase the zone's other records. Never point such a program at a live
  zone; keep it to a throwaway domain.
- **What we found:** for a `.ng` domain and for a `.africa` domain, `GetDNS`
  answered `HTTP 501`, `{"error":"Function not supported by the registrar module"}`.
  The reseller API passes each call to the registrar behind the domain's TLD,
  and those registrars didn't implement DNS. A `.com` was not tested. Ask the
  registrar which TLDs support DNS calls before building anything on it.

## Our production setup

For the record of what is running (no credentials are recorded here; they are
in `.env` on the server and in the password manager):

| | |
| --- | --- |
| Web UI | `app.xenibase.ng` |
| Database host | `db.xenibase.ng` |
| API domain | `api.xenibase.ng`: projects at `<ref>.api.xenibase.ng` |
| DNS host | The registrar's own DNS (no API) |
| Records for backend services | `*.api` A → the server; `_acme-challenge.api` CNAME → the acme-dns `fulldomain` |
| Certificate method | acme-dns (`auth.acme-dns.io`), through Caddy's acme-dns plugin |
| Release | 4.2.1, upgraded from 4.1.0 with `upgrade.sh`; backend services enabled on 15 October 2026 |
| Server | One host, 8 GB, running the whole bundle including `xenibase-edge` |
