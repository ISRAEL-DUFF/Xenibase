# Status page

`xenibase-status` is Xenibase's public status page (V3 §2.6). Run it **away from
Xenibase**, on another provider, so an outage can't take down the page that
announces it.

It does four things:
- **Probes Xenibase from outside every minute**: the dashboard, the edge
  pooler on both ports, and the shared tier.
- **Takes signed heartbeats** from xenibase-server for what it can't probe:
  dedicated instances, backups, and webhooks & jobs. A component whose
  heartbeat stops shows **No data**, never Operational.
- **Opens an incident by itself** after 3 failing checks in a row, and
  resolves it after 2 good ones. The incidents you post in Admin →
  Incidents appear there too.
- Keeps **90 days of history** per component, emails **subscribers**
  (double opt-in), and serves the page as plain HTML with no JavaScript, a
  JSON API (`/api/v1/status`, `/api/v1/incidents`) and an RSS feed
  (`/feed.rss`).

Its state is one SQLite file. It never connects to Xenibase's database.

## Deploy

On a small server outside Xenibase's provider (1 vCPU, 512 MB is enough):

1. Build the image in a Xenibase checkout and copy it over:

   ```sh
   make status-image
   docker save xenibase-status:local | ssh status-host docker load
   ```

2. Copy `deploy/status/compose.yaml` and
   `deploy/status/status.example.toml` (as `status.toml`) to the server. Put
   the secrets in a `secrets/` directory next to them:

   ```sh
   mkdir -p secrets
   openssl rand -hex 32 > secrets/xenibase-status-push   # shared with xenibase-server
   ```

3. Edit `status.toml`: set `public_url`, and the dashboard URL in the
   `dashboard` probe. If you want email subscriptions, fill in `[smtp]`;
   remove the section otherwise. Then check the file:

   ```sh
   docker run --rm -v "$PWD/status.toml:/s.toml:ro" -v "$PWD/secrets:/run/secrets:ro" \
     xenibase-status:local check-config --config /s.toml
   ```

4. Point the status domain's DNS at the server and start it:

   ```sh
   STATUS_DOMAIN=status.example.com docker compose up -d
   ```

   Caddy gets the certificate and forwards to xenibase-status.

## The probe database

The edge pooler and shared tier probes log in through the pooler like a
customer does. They need a database of their own with a low-privilege
role:

1. In Xenibase, create a project named `status-probe`, in your own
   organisation, on the shared tier.
2. In Connect, copy its session and transaction connection strings. Replace
   the host with the floating IP's hostname: the public database host
   clients use.
3. Save them as `secrets/xenibase-status-probe-session` and
   `secrets/xenibase-status-probe-transaction`.

The transaction-port probe runs `SELECT 1`. The shared tier probe runs
`SELECT txid_current()`, which needs a primary that accepts writes.

## Connect Xenibase

On xenibase-server, set these and restart it:

```sh
XENIBASE_STATUS_URL=https://status.example.com
XENIBASE_STATUS_PUSH_SECRET_FILE=/run/secrets/xenibase-status-push   # the same secret
XENIBASE_STATUS_REGION=eu-central                                  # optional default
# XENIBASE_STATUS_COMPONENTS=dashboard,edge-pooler,...  only if you renamed components
```

xenibase-server then:
- sends a heartbeat every minute;
- pushes each incident within seconds of a change. Admin → Incidents shows
  "on status page", or the error if a push fails; failed pushes are retried
  every 10 seconds.

Every push is signed in the `Xenibase-Signature` header and refused if it
is more than 5 minutes old. The status page checks its `v2` value: an
HMAC-SHA256 of `<t>.<METHOD> <path>\n<body>` with the push secret, so a
signed request is accepted only by the endpoint it was made for. The header
also carries the webhooks' body-only `v1`, which older status pages check;
this page ignores it. A heartbeat sent before the newest one accepted is
refused (409). Upgrade xenibase-server before xenibase-status: a status
page of this release refuses pushes from an older server.

A failing probe shows on the page as its error class only (connection
refused, timeout, authentication failed, query failed, HTTP 503, name not
resolved, TLS error, or check failed); the full error, which can name a
probe's user, database and address, is in xenibase-status's log.

## Incidents

- **Automatic:** a probed component that fails 3 checks in a row opens an
  incident on the status page by itself. A component that answers on one
  pooler port but not the other is degraded, and opens a minor incident.
  The incident resolves itself after 2 good checks. Xenibase can't edit
  these.
- **Posted in Xenibase:** Admin → Incidents → New incident. Pick the affected
  components, the severity, the status, and the first update:
  - minor and maintenance show the components as degraded;
  - major and critical show them as an outage.

  Post updates as you learn more. Posting one with status **resolved**
  closes the incident; posting any other status reopens it.

- **Scheduled maintenance:** Admin → Incidents → Scheduled maintenance.
  An announcement is shown as **upcoming** until its window starts, and
  leaves its components alone until then; it resolves itself when the
  window ends. See [HA → Announced maintenance](ha.md#announced-maintenance)
  for what it does to the SLA.

Confirmed subscribers get an email for every new update, automatic or
posted.

### Customers' emails (managed subscribers)

xenibase-server also subscribes customers itself, hourly
(`PUT /api/v1/subscribers/managed`): for every paying organisation (any
plan but Free, not suspended), its **owners** and its **billing contacts**
with *Status page emails* on (Org → Billing → Billing contacts), each
with the components and regions its projects use:

| Projects | Components |
| --- | --- |
| any | dashboard, edge-pooler, backups, billing, webhooks-jobs |
| shared tier | shared-tier |
| dedicated or HA | dedicated |
| backend services on | backend-services |

They skip double opt-in (the terms cover it) and get only incidents on one
of their components, in one of their regions or in none. Every email has
the unsubscribe link; an unsubscribe is remembered, so the next sync
doesn't add the address back (subscribing on the page again still works).
A contact turned off, or an organisation that goes back to Free, is
removed at the next sync.

### Billing and backend services

- **billing** (heartbeat): degraded while a payment provider's last
  automatic charge in the last half hour hit an outage, or when last
  month's invoices haven't issued by the 2nd (with automatic issue on).
- **backend services** (heartbeat, per region): every xenibase-edge reports
  at least every 30 seconds; a region is degraded when some of its edges
  have been silent for 2 minutes and down when all have. Edges silent for
  a day are taken as gone. Give the component a probe of the region's
  edge (`/healthz`) as well, and the worse of the two is shown.

A heartbeat component's `heartbeat_id` (default its `id`) is the ID
xenibase-server reports; a report for a region applies to the components
of that ID in that region, so one `backend-services` report per region
fills one line per region (see `status.example.toml`).

### In the dashboard

Members of an organisation see, at the top of every page:

- its **billing state** (overdue, restricted, suspended, a payment
  declined): owners and billing members with the pay link, everyone else
  with "ask an owner or a billing member", and never an amount;
- the **budget** reaching 80% or 100%, to owners and billing members;
- an **open incident affecting its projects**: one that names one of its
  projects or their nodes, or, unscoped, is in a project's region (or all
  regions) on a component the project uses (`GET
  /api/v1/orgs/{org}/incidents`), linked to the status page.

## Operate

- **Back up** `/var/lib/xenibase-status/status.db` (the `data` volume) if you
  want to keep the history and subscribers. If it is lost, the page starts
  over with no history.
- `xenibase-status` logs JSON to stdout, including every component state
  change and every incident it opens or resolves.
- The subscribe form allows 5 requests an hour per address. Behind Caddy,
  `trust_proxy = true` makes that per client, not per proxy.

## Limits

The page probes from one vantage point; xenibase-server is the second one
for the SLA's measurement of HA projects (each minute counts as
unavailable only when both fail; see [HA](ha.md)). Subscribers sign up on
the page themselves: paying organisations aren't subscribed automatically
yet (planned for V4.1-M6). Components aren't split by region unless you
configure one component per region in the TOML.
