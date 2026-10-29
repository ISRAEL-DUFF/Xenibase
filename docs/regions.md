# Regions and data residency

Xenibase V3 runs projects in more than one region (V3 §6). The control
plane (xenibase-server and its metadata database) stays in one place, the
**home region** (`XENIBASE_REGION`, default `eu-central`). A region groups:

- **nodes** (`nodes.region`), on Hetzner or on machines you register by
  hand (colocation, or a local provider);
- a **pooler pair** with its own hostname, the one in its projects'
  connection strings;
- a **storage target** for its projects' backups, and a **copy target**
  for cross-region copies of them.

Regions are managed under **Platform → Regions** (or
`PUT /api/v1/admin/regions/{id}`).

## Adding a region

1. **Platform → Regions → Add region.** Give it an ID (`ng-lagos`), a
   name, its two-letter country, and the pooler hostname
   (`db.ng.example.com`). Pick **Manual** for colocated machines or a
   local provider.
2. **Storage.** Add an S3-compatible bucket in the region as a platform
   storage target (Settings → Platform storage targets) and choose it as the
   region's **backup target**. Choose a **copy target** in another region
   or provider for cross-region copies (below).
3. **Nodes.** Record each node's [failure domain](failure-domains.md)
   (its rack) as you add it. HA standbys, etcd members and pooler hosts
   are kept in different ones. Add the region's nodes (`POST /api/v1/nodes` with
   `"region": "ng-lagos"`, or Platform → Nodes) and start their agents.
   Give one a shared cluster if the region offers the shared tier.
4. **Poolers.** Add two pooler hosts in the region (see
   [edge poolers](edge-poolers.md)), and set the region's floating IP ID.
   Point the region's hostname at that floating IP. Until a region has
   pooler hosts of its own, the home region's poolers serve its projects
   (Platform → Regions says "served by the home poolers"). The region's
   pooler hosts serve a certificate for its hostname, obtained and renewed
   the same way as the home one (`XENIBASE_POOLER_TLS`: self-signed, or from
   the ACME CA over HTTP-01, which needs port 80 of the region's hostname to
   reach xenibase-server's `/.well-known/acme-challenge/`; until it does, a
   self-signed certificate for the hostname stands in). In `files` mode the one
   certificate you supply is served everywhere, so it must name every
   region's hostname (or be a wildcard that covers them).
5. **Data residency** (optional): tick **Offer data residency**. It needs
   the country and an in-country backup target.
6. **API domain** (optional, for backend services): set the region's own
   **API domain** (`ng.api.example.com`) and run an edge in the region
   for it (below), so its projects' API traffic stays in the region.

Hidden regions take no new projects except from their **early-access
organisations**; existing ones keep running. List your own organisation
there to rehearse on the region before it opens, or a first customer to
start on it before everyone can (Platform → Regions → the region →
**Early-access organisations**, one organisation ID per line, or
`early_access_org_ids` in the `PUT`). Those organisations see the region
in New project, marked "early access"; nobody else does. Left out of a
`PUT`, the list is kept; opening the region doesn't clear it.
A hidden region shows its **launch checks** in its panel (and at
`GET /api/v1/admin/regions/{id}/readiness`): healthy nodes, the pooler
pair and its failure domains, the floating IP, the hostname, backup and
copy targets, and, if the region has an etcd cluster, three members in
three failure domains. Turning **Hidden** off is refused (`409
region_not_ready`, the region stays hidden, the rest of the change is
saved) while a blocking check fails: no healthy node, an etcd cluster
that isn't three members in three domains, an HA pair or etcd cluster
sharing a domain, or a residency region without its backup target. The
others are warnings. A region with no etcd cluster can open; it just
can't offer HA.

## Projects and regions

- **At creation** a project chooses its region (New project → Region,
  `xenibase projects create --region ng-lagos`, or `region` in
  `POST /api/v1/projects`). The default is the home region. Its node, its
  HA standby, its backups and its branches are all in that region.
  Restores into a new project stay in the source's region.
- **Promotions, demotions, upgrades and HA** stay in the project's region.
- **Moving regions** is a zero-downtime move to a node in the other region
  (Project Settings → Compute → Moves, platform admins). At the cutover the
  project's region changes and so does its hostname. **The old hostname
  keeps routing for 30 days**: the old region's poolers keep a forward to
  it, and Project Settings shows when it ends. Tell the customer to update
  their connection strings within that time.

Each region's pooler hosts get their own configuration: their projects'
routes and logins, plus those forwarded to them, with their own
generation and floating IP in the arbiter.

## A region's API domain

A project's backend services (data API, auth, files, realtime) are at
`https://<ref>.<api-domain>`. With one platform API domain
(`XENIBASE_API_DOMAIN`), every project's API is reached through the same
wildcard DNS record, usually pointing at the home region, so a Lagos
project's API calls would cross to the home region and back. Give the
region its own API domain to keep them there:

1. **DNS and TLS.** A wildcard record `*.ng.api.example.com` pointing at
   the region's edge (a node in the region, or a floating IP in front of
   two), and a wildcard certificate for it, obtained the same way as the
   platform's ([Enabling backend services](enabling-backend-services.md)).
2. **The edge.** Run xenibase-edge in the region with
   `XENIBASE_EDGE_DOMAIN=ng.api.example.com` and
   `XENIBASE_EDGE_REGION=ng-lagos`, and as `XENIBASE_EDGE_SECRET` a
   credential for the region (`xenibase-server -issue-edge-credential
   edge-lagos-1 -edge-region ng-lagos`, see
   [Running xenibase-edge](backend-services.md#running-xenibase-edge)): it
   serves the region's projects, and only them, and the platform-wide edge
   secret no longer reads them. Edge nodes Xenibase adds in the region get
   the region's domain and their own credential in their cloud-init.
3. **Platform → Regions → the region → API domain:** `ng.api.example.com`.
   It must differ from the platform's domain and from every other
   region's.

From then on the dashboard, the API page, `xenibase services env`,
branches, auth emails and signed file URLs give the region's projects
their `https://<ref>.ng.api.example.com` URL. Set it **before** the
region's first project enables backend services: changing it later
changes those projects' API URL, and their apps, OAuth redirect URLs and
saved links need the new one. Once the region has its own API domain, the
platform domain's edges (which use the platform-wide edge secret) stop
serving its projects, so `<ref>.<platform domain>` no longer reaches them
(unless `XENIBASE_EDGE_SECRET_ALL_REGIONS=true`, deprecated). A project
moved to another region takes that region's API URL.

## Data residency

A project in a region that offers it can turn on **Data must stay in
<country>** (V3 §6.3), at creation or later under Project Settings →
General → Region. Turning it on or off is for **organisation owners**,
needs a fresh step-up (password and code), and is audited. With it on:

- **Backups go only to targets in the region**: the region's own target,
  or a target the platform admin marked as being there. Pointing the
  project at any other target is refused, and so is a backup when the
  region has no in-country target.
- **No cross-region copies.** Copies to a copy target outside the region
  are skipped; turning residency on deletes existing ones.
- **The project can't be moved out of the region**; turn residency off
  first.
- **Branches** are created in the parent's region.
- **Exports** (backup downloads by owners) stay available: the customer
  decides where their own copy goes.

What it guarantees: Xenibase stores the project's data, WAL archive and
backups only on machines and buckets in the region. Whether a regulator
requires in-country hosting is the customer's determination. A residency
region should have its own pooler hosts, or client connections pass
through the home region's poolers.

## Cross-region backup copies

Every backup on a platform storage target is copied to its region's copy
target, asynchronously (every 5 minutes, 20 at a time), and verified by
checksum on the way and read back (V3 §2.5). The copy keeps the object
key. Failures are retried after an hour. Retention deletes the copy with
the original.

- Org (bring-your-own) targets aren't copied; that's the organisation's
  responsibility.
- Residency projects are skipped unless the copy target is in their
  region.
- WAL-G base backups and WAL archives aren't copied object by object; use
  the copy bucket's own replication for them if you need it.
- **The weekly restore test alternates** between a primary backup and a
  copy. Its log says which ("from its cross-region copy").

Platform → Regions shows the last 7 days' copy status (copied, pending,
failed, skipped).
