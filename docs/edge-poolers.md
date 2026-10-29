# Standby edge pooler

Xenibase's PgBouncers (session mode on 5432, transaction mode on 6543) can run
on a **pair of pooler hosts** behind a floating IP, so losing one machine
doesn't take every database connection down (V3 §2.1). This page covers
setting the pair up on Hetzner Cloud, checking it, and what to do when it
reports a problem.

Without pooler hosts, Xenibase keeps running its PgBouncers next to
xenibase-server, as in V2. Nothing changes until you add them.

## How it works

- Each pooler host runs two containers. The first, **pooler-host**, holds
  both PgBouncers and `xenibase-agent` in pooler mode. The second is
  **keepalived**, the only one with `NET_ADMIN`.
- xenibase-server renders the routes, the auth file and the TLS pair once and
  pushes them to every pooler host's agent (mTLS, like any node). Each push
  carries a **generation** number and a SHA-256 hash. The agent writes the
  files, reloads its PgBouncers, and reports what it serves.
- keepalived runs VRRP in **unicast** (Hetzner networks have no multicast).
  Its check is the agent's `/ready`. That check fails unless both PgBouncers
  accept connections and the host serves the newest generation. A host that
  missed a push is **stale** and can't take the floating IP. The arbiter
  pushes again until it catches up.
- When keepalived makes a host MASTER, the agent reports it, and
  xenibase-server's **arbiter**, which checks both hosts every 3 seconds,
  asks Hetzner's API to route the floating IP to that server. This takes
  about 3–7 seconds after the active host dies. The pooler hosts hold no
  Hetzner token. (An agent started with `XENIBASE_AGENT_ASSIGN_FLOATING_IP=true`
  and a token also assigns it itself, which keeps failover working while
  xenibase-server is down.)
- If the floating IP points at an unhealthy host for two checks in a row
  while the other host is healthy, the arbiter moves it even without
  keepalived. With two MASTERs it leaves a healthy holder alone. Every move
  is a pooler event, shown in Admin → Nodes.
- A configuration change fails only when **every** pooler host refuses it.
  When just one host misses it, that host is marked stale and catches up.
  Commands like `PAUSE` or `KILL` go to the hosts that are reachable.

## What you need

- Two small servers (2 vCPU, 4 GB is plenty for thousands of clients) in
  the same Hetzner location and private network as your database nodes.
  Put them in a spread placement group so they don't share a physical host.
- A Hetzner **floating IP** in that location, configured on the public
  interface of **both** servers. This is Hetzner's "persistent" setup: add
  the address to the interface in netplan. The API decides which server
  receives its traffic.
- A Hetzner Cloud API token with read and write access, for xenibase-server,
  which assigns the floating IP. Keep it off the pooler hosts.
- The two images, built in a Xenibase checkout:

  ```sh
  make pooler-host-images        # xenibase-pooler-host:local, xenibase-keepalived:local
  docker save xenibase-pooler-host:local xenibase-keepalived:local | ssh edge-a docker load
  docker save xenibase-pooler-host:local xenibase-keepalived:local | ssh edge-b docker load
  ```

## Set up

1. **Register both hosts.** In Xenibase, go to Admin → Nodes → Add node, with
   role **pooler** and the host's private address. Do this once per host.
   Each gives you a one-time token, valid for 24 hours.

2. **Configure each host.** Copy `deploy/pooler-host/compose.yaml` to the
   host. Next to it, create `.env` with that host's values:

   ```sh
   XENIBASE_AGENT_SERVER=https://xenibase.example.com
   XENIBASE_AGENT_TOKEN=...            # from step 1, this host's
   XENIBASE_AGENT_ADVERTISE=10.0.0.11:7070
   XENIBASE_EDGE_SELF_IP=10.0.0.11     # this host's private IP
   XENIBASE_EDGE_PEER_IP=10.0.0.12     # the other host's
   XENIBASE_EDGE_INTERFACE=enp7s0      # the private network's interface
   XENIBASE_EDGE_VRRP_PASSWORD=...     # the same on both, at most 8 characters
   XENIBASE_EDGE_PRIORITY=110          # 110 on one host, 100 on the other
   ```

   The hosts hold no Hetzner token: xenibase-server moves the floating IP
   to the host keepalived makes MASTER (step 4). A Hetzner token can't be
   narrowed to one IP, so on these internet-facing hosts it would open the
   whole Hetzner project. If failover must also work while xenibase-server
   is down, you can let the agents assign it themselves, at that cost:
   add `XENIBASE_AGENT_ASSIGN_FLOATING_IP=true`,
   `XENIBASE_AGENT_FLOATING_IP_ID=...` and `XENIBASE_AGENT_HETZNER_TOKEN=...`.

   Leave `XENIBASE_EDGE_VIP` unset on Hetzner. The floating IP is already on
   both interfaces; keepalived only decides which host claims it.

   The server ID comes from Hetzner's metadata service. Set
   `XENIBASE_AGENT_SERVER_ID` only if that isn't reachable.

   The firewall must allow the following:

   | Traffic | Port | From |
   | --- | --- | --- |
   | VRRP (IP protocol 112) | — | the peer |
   | Agent | 7070 | xenibase-server |
   | PgBouncers | 5432, 6543 | xenibase-server and clients |

3. **Start them**, on each host:

   ```sh
   docker compose up -d
   ```

   The PgBouncers start with placeholder files. A host isn't ready, and
   can't take the floating IP, until xenibase-server's first push arrives.
   That push comes within seconds of the agent registering.

4. **Let xenibase-server manage the floating IP.** In its `.env`:

   ```sh
   XENIBASE_FLOATING_IP_ID=...
   XENIBASE_HETZNER_TOKEN_FILE=/run/secrets/hetzner-token   # or XENIBASE_HETZNER_TOKEN
   ```

   Then restart it. Its arbiter checks the hosts every 3 seconds and moves
   the floating IP to keepalived's MASTER, or away from a host that can't
   serve. Without these, nothing moves the floating IP (unless the agents
   do, above) and the arbiter only reports. While xenibase-server is down
   the floating IP stays where it is: a pooler host that fails then is not
   replaced until it is back.

5. **Point clients at the floating IP.** Change the database host's DNS
   record (Settings → General) to the floating IP. Check
   that connections work through it.

6. **Retire the local PgBouncers** once traffic flows through the pair. Set
   `XENIBASE_POOLER_LOCAL=false` on xenibase-server, restart it, and stop the
   `pooler-session` and `pooler-tx` services in the install bundle.

Put the two hosts in different racks (or Hetzner spread placement groups),
and record each one's [failure domain](failure-domains.md) when you add it.
Xenibase warns when the second host shares the first's domain.

## Check it

Admin → Nodes shows **Edge pooler hosts**:
- each host's state: ready, not ready (with the reason), or unreachable;
- keepalived's state on each host;
- the configuration generation each host serves, marked stale if it is behind;
- which host holds the floating IP;
- the last pooler events.

Alerts fire for a pooler host that isn't ready (`pooler_host_not_ready`) and
for split brain (`pooler_split_brain`): two hosts of the same region both
keepalived MASTER. A MASTER in each region is normal; each region's
pair has its own floating IP. During split brain the arbiter leaves the
floating IP on its holder while that host is healthy, keeps pushing the
configuration to both, and moves the IP to the other host if the holder
goes stale (`TestChaosPoolerSplitBrain`).

**Rehearse a failover** before relying on it, with a client running queries
through the floating IP:

```sh
ssh edge-a docker kill xenibase-edge-pooler-host-1
```

Queries should succeed again within 10 seconds. The pooler events then show
`took_ip` for edge-b. Start edge-a again with `docker compose up -d`. It
rejoins as BACKUP and doesn't take the address back (keepalived runs with
`nopreempt`).

## When something is wrong

| You see | Meaning | Do |
| --- | --- | --- |
| A host is **stale** | It missed a push (it was down or unreachable). | Nothing, usually: the arbiter pushes again every 15 s. If it stays stale, check the host's agent logs (`docker compose logs pooler-host`). |
| **Push failed** events | xenibase-server couldn't reach that host's agent. | Check the host is up and port 7070 is open from xenibase-server. |
| **Split brain** | Both hosts' keepalived say MASTER, usually because VRRP between them is blocked. | Open IP protocol 112 between the hosts. Meanwhile Xenibase keeps the floating IP on one healthy host. |
| **Floating IP moved by Xenibase** | keepalived didn't move it, so the arbiter did. | Check keepalived's logs on both hosts (`docker compose logs keepalived`). |
| **No healthy pooler host** | Neither host can serve. | Look at both hosts' reasons in Admin → Nodes. Every connection is failing. |

## Tested, and not yet

`TestPoolerHostFailover` runs two real pooler-host containers with
keepalived on a Docker network, against a fake Hetzner API. It kills the
active host and requires queries through the shared address to recover
within 10 seconds (about 4 s in practice). A xenibase-status container
outside that network then has to mark the edge pooler down, open an
incident, and resolve it once a host is back.

Not provable there:
- real Hetzner floating-IP reassignment times;
- placement groups;
- probing from another provider.

Before the paid launch, run the rehearsal on the two real servers with
`scripts/rehearse-pooler-failover.sh`, which times the outage from a
client's side and fails above 10 seconds:

```sh
DATABASE_URL='postgresql://<user>:<password>@db.eu.<domain>:6543/<db>?sslmode=require' \
KILL_CMD='ssh edge-a docker kill xenibase-edge-pooler-host-1' \
scripts/rehearse-pooler-failover.sh
```

Leave `KILL_CMD` empty to cut the host yourself (power it off from
Hetzner's console, or detach its network) when the script says so. Record
the result here:

| Date | Region | How the host was cut | Gap | By |
| --- | --- | --- | --- | --- |
| | | | | |
