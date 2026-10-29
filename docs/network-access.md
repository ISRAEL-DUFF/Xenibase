# Network access: a project's IP allowlist

By default a project's poolers accept connections from anywhere on the
internet, protected by TLS and SCRAM passwords. An **IP allowlist** narrows
that: the poolers accept the project's logins only from the addresses on the
list, and refuse them from everywhere else before checking the password.

Set it under Project Settings → **Network access** → **Allowed IP
addresses**, one address or CIDR range per line, or through the API:

```http
PUT /api/v1/projects/{id}/network
{"allowed_cidrs": ["203.0.113.10", "198.51.100.0/24", "2001:db8::/48"]}
```

`GET /api/v1/projects/{id}/network` returns the list, the address your
request came from (`your_ip`) and whether it is on the list
(`your_ip_allowed`), so you can check before you lock yourself out. Anyone
who can change the project's settings can change the list, and every change
is audited (`project.network`, with the list before and after).

## The rules

- Up to 50 entries, IPv4 or IPv6. A bare address means that address alone
  (`/32` or `/128`).
- A range must be written from its first address: `10.1.2.0/24`, not
  `10.1.2.3/24` (the API says which one you meant). `0.0.0.0/0` and `::/0`
  are refused: leave the list empty instead.
- An empty list accepts connections from anywhere, as before.
- A change reaches every pooler of the project's region within seconds.
  Connections already open from an address the new list doesn't admit are
  dropped when you save it (on both ports and every route of the project;
  Xenibase's own service logins are left alone), so removing a leaked
  network takes effect at once. Clients still on the list keep their
  connections. The audit entry records how many were dropped
  (`dropped_connections`).

## What it covers

The list applies to every login of the project at the poolers, on both the
session (5432) and the transaction (6543) ports and on the read-only
`<database>_ro` route: the owner, `<database>_ro`, members' personal
credentials, and logins the owner creates with
[role management](database-roles.md).

It doesn't apply to Xenibase itself:

- **Backend services** (the data API, auth, storage, realtime) connect as the
  project's service logins, which the poolers accept from anywhere. The API
  keys and the row-level security policies protect those.
- **The dashboard's SQL editor and table editor**, backups, moves, upgrades and
  HA connect to the instance directly, not through the poolers.
- **Availability probes** connect as the probe login.

So an allowlist keeps everyone else off the database, without stopping the
dashboard or the API.

Xenibase's own addresses can't be borrowed to get round it: an import or a
Supabase migration refuses a source on loopback, on the nodes' and instances'
addresses or on the platform's networks, even one the platform admin
allow-listed ([outbound traffic](operations.md#outbound-traffic)).

The poolers see the address a connection comes from. If clients reach them
through a NAT gateway, list the gateway's public address; from a Kubernetes
cluster or a serverless platform, list its egress addresses.

## Creating and rotating

A new project has no list. Once a project has one, provisioning and password
rotation skip their check that the owner can connect through the poolers
(that check comes from Xenibase's own address, which isn't on your list);
the operation log says so.

## For operators

The allowlist is enforced by PgBouncer's HBA rules (`auth_type = hba`,
`auth_hba_file`): xenibase-server renders `hba.conf` for each region with the
pooler's other files, and the pooler hosts' agents write it before reloading
PgBouncer. Upgrade the pooler hosts (their agent and PgBouncer configuration)
**before** xenibase-server: an older agent refuses a bundle with `hba.conf`
in it, and an older PgBouncer configuration ignores the file, so a list would
be stored but not enforced. See the changelog's upgrade notes for v4.2.4.

The same file keeps the poolers' admin console (database `pgbouncer`) off
the internet: it accepts logins only from loopback and the private ranges
(10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7), where xenibase-server
and the pooler hosts' agents reach it, and rejects them from everywhere
else before the password is checked. If xenibase-server reaches the poolers
from elsewhere (another private range such as 100.64.0.0/10, or a public
address), list its networks in `XENIBASE_POOLER_ADMIN_ALLOW`
(comma-separated CIDRs), which replaces the default.
