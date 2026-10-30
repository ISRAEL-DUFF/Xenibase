# Capacity and costs

Xenibase can add database nodes by itself when a region fills up. It can
also move projects off nodes, delete servers that stay empty, and work out
what each organisation costs to run (V3 §5).

Everything here is on **Platform → Capacity** and **Platform → Costs &
margins**.

## Providers

The provider is where new servers come from. There are two.

- **manual** (the default) is for machines Xenibase can't create: the
  install's own node, and colocated servers. You register them under
  Nodes → Add node, as before. Proposals still say when to add a machine
  and what size it should be. Once you have registered it, choose
  **Mark added** on the proposal.
- **hetzner** creates servers in a Hetzner Cloud project. Each server
  boots with cloud-init, which does the following:
  - installs Docker, ufw, fail2ban and unattended upgrades;
  - turns off SSH passwords;
  - allows SSH from anywhere, and everything else only from the private
    network;
  - runs the agent with a one-time registration token.

  The agent registers by itself. A shared node then gets its shared
  cluster, and the node goes into service.

| Variable | What |
| --- | --- |
| `XENIBASE_CLOUD_PROVIDER` | `manual` or `hetzner` |
| `XENIBASE_REGION` | This installation's region name (default `eu-central`). New nodes are given it, and so are existing ones on upgrade. |
| `XENIBASE_HETZNER_TOKEN` (or `_FILE`) | A Hetzner Cloud API token with read and write access |
| `XENIBASE_HETZNER_LOCATION` | Where servers go (default `fsn1`) |
| `XENIBASE_HETZNER_NETWORK_ID` | The private network new servers join; use the network your other nodes and xenibase-server are on |
| `XENIBASE_HETZNER_PLACEMENT_GROUP_ID` | Optional: an existing spread placement group for the home region. Otherwise Xenibase creates `xenibase-<region>` (then `-2`, … as each fills at 10 servers) and puts every new server in its region's group, so it counts as its own [failure domain](failure-domains.md). |
| `XENIBASE_HETZNER_SSH_KEYS` | Comma-separated names of SSH keys in the Hetzner project, for root |
| `XENIBASE_CLOUD_AGENT_IMAGE`, `XENIBASE_CLOUD_PG_IMAGE` | Images new servers can pull: the agent, and `xenibase-postgres` (with `{major}` for the Postgres version). Push your build to a registry; the bundle builds them locally only. |
| `XENIBASE_CLOUD_SERVER_URL` | How agents reach xenibase-server (default `XENIBASE_PUBLIC_URL`) |
| `XENIBASE_CLOUD_SERVER_CA_FILE` | The PEM of a private CA, if xenibase-server's certificate isn't publicly trusted |
| `XENIBASE_CLOUD_PRIVATE_CIDR` | The private network, default `10.0.0.0/16`. Instances accept logins from it, and the agent advertises its address on it. |
| `XENIBASE_CLOUD_EDGE_IMAGE` | Optional: xenibase-edge's image, for the edge nodes Xenibase adds. Their cloud-init also gets `XENIBASE_EDGE_SECRET` and `XENIBASE_API_DOMAIN`. Without it, edge proposals are added by hand. |

Servers Xenibase creates carry the labels `xenibase=node`, `xenibase-node=<name>`
and `xenibase-region=<region>`. Don't delete them by hand while Xenibase still
lists them: drain the node instead (below).

## Proposals

Every hour, and when you choose **Check now**, Xenibase checks three
thresholds in each region.

- **Shared tier:** the region's shared disk is projected to pass 70%
  within 14 days. The projection is the disk now plus the past week's
  hourly growth, fitted with a line.
- **Dedicated tier:** no dedicated host has room for the largest instance
  size (vCPU, memory and disk). This check only runs in regions that
  already have a dedicated host.
- **Edge tier** (V4.1 §11): the region's xenibase-edge processes used more
  than 70% of their hosts' CPUs throughout the last hour. Each edge
  reports its process's share of its host's CPUs; the check averages a
  region's edges in 5-minute buckets and needs at least 11 of the hour's
  12 above the threshold. Edges that serve every region count towards the
  home region.

An **edge node** runs xenibase-edge beside its agent and never gets a
database: no shared cluster, no dedicated instance, and the empty-node
cleanup leaves it alone. Its cloud-init writes the edge's settings
(including the edge secret) to a root-only file and opens port 8443 for
the load balancer or TLS terminator in front of the edges. You can also
register one by hand with the role `edge` so its cost is attributed.

When a threshold trips, Xenibase records a **proposal**: the server type, the
location, the monthly price from the provider's catalog, and the reason. A
region and tier never have more than one open proposal at a time. A
proposal is provisioned by itself only when all of these hold:

- automatic provisioning is on;
- the provider can create servers;
- the nodes' monthly cost, plus the servers that started proposals are
  still setting up, plus the new server's, stays within the **monthly
  infrastructure budget**. Proposals are checked and started one at a
  time across the control-plane servers, so two at once can't both fit
  into the same headroom.

The budget is zero until you set one, so nothing is bought until then.
When the budget is in another currency than the servers, Xenibase converts
at the current exchange rate (below).

Otherwise the proposal waits, and an alert fires. **Provision** starts the
server; **Reject** closes the proposal. Both the settings and approving a
proposal ask for your password and a code first, because they spend
money.

Provisioning is an operation with a log, and it runs these steps:

1. It creates the node with a one-time token.
2. It creates the server, with that token in the server's cloud-init.
3. It waits up to 20 minutes for the agent to register and answer.
4. On a shared node, it creates the shared cluster.
5. It puts the node into service.

From then on, new projects go to the node with the fewest projects, which
is the new one.

### Dedicated hosts on demand

New dedicated instances are placed on the node in their region with room
for them (its reported vCPUs, memory and disk minus what its instances are
allocated) and the fewest instances. When none has room and the provider
can create servers, the creation itself asks for a host: it opens the
region's dedicated proposal (or joins the open one) with the reason. Within
the budget the server is created at once and the project shows **Waiting
for a host** in its operation's log while it joins (usually 5–10 minutes,
up to 25), then lands on it. Over the budget, or while a proposal waits
for approval, the creation is refused with `409 capacity_pending_approval`
and the alert above fires; the user can try again once it is in service.
With hosts added by hand, the creation is refused with `503 no_capacity`,
naming the region.

If any step fails, Xenibase deletes the server and removes the node, the
proposal is marked failed, and a critical alert fires. The server's
`/var/log/cloud-init-output.log` usually says why.

The settings (Platform → Capacity → Settings) are:

- per tier: whether to propose at all, the threshold, the horizon, and the
  server type to add (or the minimum vCPU, memory and disk, leaving Xenibase
  to pick the cheapest type that fits);
- the shared cluster's memory on a new shared node;
- the budget and its currency.

## Draining and rebalancing

- **Drain** (Capacity → Nodes) stops anything new from being placed on a
  node and moves its projects elsewhere in the region, one at a time, with
  zero-downtime moves. Each shared project goes to the shared cluster with
  the most free disk that runs its Postgres version and is open to its
  organisation. A dedicated project goes to the dedicated host with the
  most free vCPU that fits it.

  A drain skips three kinds of project, with the reason shown:
  - HA projects: switch the primary over, or turn HA off, first;
  - paused Free projects: resume them first;
  - archived Free projects: unarchive them first, since their roles live
    on that cluster.

  **Stop drain** puts the node back into service and drops the moves that
  haven't started.
- **Rebalancing** runs weekly, and when you choose **Plan rebalance**. If
  shared nodes in a region differ in disk use by more than 15%, it
  proposes moves from the fullest node to the emptiest, at most ten in a
  batch, choosing the largest projects that narrow the gap. Approve or
  reject the batch. With automatic rebalancing on, batches are approved by
  themselves during the maintenance window (Nodes → Maintenance window),
  and their moves run only inside it: one not started when the window
  ends waits for the next. Rebalancing moves shared projects only; HA
  projects are never among them.
- **Empty nodes:** a node with no projects, dedicated instances, HA
  members, etcd member, or move copies is marked empty. The copies a move
  keeps are kept for 48 hours. After 24 hours empty a provider's node is
  **retiring**: nothing new is placed on it. At least 10 minutes later the
  next sweep counts again; if something landed there after all, or the
  node is now marked keep, it goes back into service, and otherwise its
  clusters are retired, its server is deleted and the node is removed (a
  failed delete is tried again by the next sweep). Manual machines are
  only flagged: decommission them yourself. Mark a node **Keep** (Capacity
  → Cost) to leave it alone.

## Cost attribution

Every day, Xenibase divides the infrastructure's cost among the
organisations that used it (V3 §5.4):

| Cost | From | Divided by |
| --- | --- | --- |
| A node | Its monthly price ÷ the days in the month: the provider's catalog price for servers Xenibase created; for manual nodes, what you enter (Capacity → Cost) | Each dedicated instance takes its share of the node's vCPUs; an HA standby's share is booked as **ha**. The rest goes to the shared cluster's projects: half by their share of storage (GB-hours), half by their share of client connection-hours. What nothing uses is **idle**. |
| Backup storage | The object storage price per GB-month | Each organisation's backup GB-hours |
| Data transfer | The price per GB | Each organisation's pooler transfer |
| Floating IPs, fixed overheads | Their monthly prices (Costs & margins → Cost settings), ÷ days | Nobody: **unallocated** |
| An edge node (role `edge`) | Its monthly price ÷ days | Organisations' share of the region's backend services: half by `api_requests`, half by realtime connection-minutes (all by one when there is none of the other; every region's when the region has none). Booked as **edge**. |
| Edges on shared nodes | **Edges' share of shared nodes** (Cost settings, default 0%) of each shared node's shared pool | The same as an edge node, as **edge**; the rest of the pool is divided as above. |
| File storage | The object storage price per GB-month, and the transfer price per GB downloaded | Each organisation's `storage_gb_hours` and `storage_egress_gb`, as **files** |
| SMS and WhatsApp codes | What the provider charged for each platform-sent code, in the cost currency at the day's rate (naira without one) | The organisation that sent it, as **messages** |

Costs stay in the currency they're billed in. Hetzner bills in euros, and
so do the default storage, transfer and floating IP prices. Yesterday and
any earlier days of the month not yet attributed are attributed hourly.
**Recompute** attributes the whole month again. A node removed since is
left out of days recomputed after its removal.

## Margins and the FX view

**Costs & margins** shows a month at a time:

- **Cost by region and tier**, in the billing currency, in naira at
  today's rate, and in naira at the rate in effect on each day.
- **Margin by service** (V4.1 §11): what the price book earned for
  databases (plans, hosting, backups, replicas), the API (data API, auth
  and realtime), files and messages, the billing page's breakdown, against
  what each cost: databases the node, backup and transfer categories, the
  API the edges, files and messages their own. Unallocated costs are in
  the total only.
- **Margin by plan** and **by organisation**. Revenue is what the month
  earns: its usage, and its own plan fee, even though the invoice bills
  the next month's fee in advance. Margin is revenue minus the
  organisation's cost at today's rate.
- **Unit costs**: a shared GB-month, a dedicated vCPU-month and a backup
  GB-month, in the billing currency and in naira. They feed repricing:
  unit price = unit cost × (1 + target margin) × FX buffer (V3 §3.1).
- **Free tier cost**, and **FX erosion**: how much more the month's costs
  are at today's rate than at the rates when they were incurred. A rising
  figure means margins are slipping before the next repricing.
- A **CSV** for the accountant.

Record exchange rates (naira per unit) under **Exchange rates**. Costs in
a currency with no rate are left out of the naira totals, and the page
says so.
