# The Free tier: pause and archive

Free projects cost little to keep because idle ones stop using the shared
cluster (V3 §4). This page covers what happens to an idle Free project,
how it comes back, and how to set it up. Paid plans (Pro, Team) are never
paused or archived. The same goes for an organisation on the **Unlimited**
quota plan: the platform's own (the platform admin's organisation) is never
paused, even though the billing migration gives it a Free billing account.

## What happens

| When | What |
| --- | --- |
| 6 days without client connections | The org's owners and admins (and the project's admins) are told the project will be paused in 24 hours. |
| 7 days without client connections, and 24 hours since the warning | **Paused.** Its pooler route points at the waker and its database refuses connections. The data stays where it was. |
| Paused for 90 days | **Archived.** A logical backup is taken, verified by restoring it into a scratch database, and the database is dropped. Roles, credentials and the connection string are kept. |
| 30 days and 7 days before a year archived | A deletion notice. If the sweep runs late, only the most urgent notice goes out. |
| A year archived | **Deleted**, like any project. The archive backup is kept for 30 more days. |

"Client connections" are clients seen through the poolers. Xenibase samples
every minute: a project with clients connected, or whose pooler
transaction counter moved since the last sample, is active. Using the
SQL console or table editor also counts. Xenibase's own sessions don't go
through the poolers, so they don't keep a project awake: backups, metrics,
and webhook listeners.

While a project is paused or archived:

- webhook deliveries wait in its outbox;
- scheduled jobs are skipped and recorded as skipped;
- nightly backups are skipped, since nothing changes;
- the SQL console and table editor say it is paused.

All of them pick up again once it resumes.

## Waking up

The first connection wakes the project by itself:

- **Paused:** the client gets *"This project was paused for inactivity and
  is resuming. Retry in about 30 seconds."* The resume takes a few seconds,
  and the retry gets in.
- **Archived:** the client gets *"This project was archived after long
  inactivity and is being restored from its archive. Retry in a few
  minutes."* The restore takes minutes for a small database.

In transaction-mode pooling (the pooled URL) the pooler finishes the login
itself, so the message arrives with the first query rather than at
connect. Clients that retry on connection errors (SQLSTATE class 08) come
back by themselves.

From the dashboard, the project's banner has **Resume** (or **Restore**).
From the CLI, run `xenibase resume <project>`. From the API, call
`POST /api/v1/projects/{id}/resume`. Anyone who can see the project can
resume it; they could wake it by connecting anyway.

An organisation that moves to a paid plan has its sleeping projects woken
by the next sweep.

## The waker

The waker is part of xenibase-server. It listens on `XENIBASE_WAKER_LISTEN`
(default `:6435`) and speaks just enough of the Postgres wire protocol to
read the startup message, queue the resume, and refuse the connection with
the message above. It uses SQLSTATE `08004`, because PgBouncer passes that
on to the client; PgBouncer would hold the client on `57P03`. The poolers
have already authenticated the client against the project's credentials,
so the waker doesn't authenticate again. It must therefore only be
reachable from the poolers, and it checks: it accepts connections only from
loopback, the server machine's own addresses, its local poolers' addresses (`XENIBASE_POOLER_SESSION_ADDR` and
`_POOLED_ADDR`, names resolved), the pooler hosts' private addresses, and
any networks in `XENIBASE_WAKER_ALLOW`. Anything else is refused with
`28000` before its startup message is read, and wakes nothing.

| Variable | |
| --- | --- |
| `XENIBASE_WAKER_ADDR` | The waker as the poolers reach it, `host:port`. The install bundle sets `172.31.250.11:6435` (xenibase-server on the Compose network). With standby pooler hosts (V3 §2.1), use the server's private address and open the port to the pooler hosts only. Empty turns pausing off. |
| `XENIBASE_WAKER_LISTEN` | Where it listens (default `:6435`). |
| `XENIBASE_WAKER_ALLOW` | Further networks the waker accepts connections from, comma-separated CIDRs, for poolers that reach it from an address xenibase-server doesn't know (behind NAT, say). |
| `XENIBASE_FREE_PAUSE_AFTER` | Default `168h` (7 days). |
| `XENIBASE_FREE_ARCHIVE_AFTER` | Default `2160h` (90 days). |
| `XENIBASE_FREE_DELETE_AFTER` | Default `8760h` (a year). |

Archiving needs backup storage and a backup key, as backups do. Without
them, projects pause but are never archived.

### If the waker is down

xenibase-server restarts the waker if it stops accepting connections. While
it is unreachable at `XENIBASE_WAKER_ADDR` (checked from xenibase-server, so
that address must be reachable from the server too):

- the `waker_down` alert fires (critical);
- a client of a paused project is held by PgBouncer (it retries the
  waker every 2 seconds, up to PgBouncer's `query_wait_timeout`, 120 s by
  default) and nothing wakes; once the waker is back, a held client gets
  the "resuming" message and its retry gets in;
- **Resume** on the project page (or `xenibase resume`) still works: it
  doesn't need the waker;
- the sweep doesn't pause or archive anything, so no more projects end up
  behind it.

The chaos test `TestChaosWakerFailure` covers this.

## Open signup

Signup can be open (Admin → Settings → Signup), protected by:

- **Cloudflare Turnstile**, when `XENIBASE_TURNSTILE_SITE_KEY` and
  `XENIBASE_TURNSTILE_SECRET` (or `_FILE`) are set. The signup page shows the
  challenge, and the server verifies the token with Cloudflare.
- **A per-IP cap** of `XENIBASE_SIGNUPS_PER_IP` new accounts a day from one
  address (default 3; 0 turns it off). Behind a proxy, set
  `XENIBASE_TRUSTED_PROXIES` so the client's address is the one counted.
- **Invitation caps.** Every account owns an organisation, and an
  invitation emails any address, so an organisation may send
  `XENIBASE_INVITES_PER_ORG_DAY` invitations in any 24 hours (default 100)
  and an inviter `XENIBASE_INVITES_PER_USER_DAY` across their organisations
  (default 50); past either, inviting answers `429 rate_limited` with
  `Retry-After`. 0 turns a cap off. Organisation and inviter names in the
  email are never links.
- What V2 already required: a verified email address, TOTP for every
  account, and one personal (Free) organisation per user.

## Pricing

The dashboard's **Pricing** page (`/pricing`) shows the price book in
effect and a monthly estimate. The same data is public at
`GET /api/v1/pricing`, for a marketing site to show. That endpoint leaves
out the book's internal notes.
