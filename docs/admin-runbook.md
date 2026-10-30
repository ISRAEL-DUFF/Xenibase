# Platform admin runbook

The tasks of whoever runs a Xenibase installation for other people, in the
order they come up. [Operations](operations.md) explains each feature;
this page is what to do, and when. Incidents have their own page:
[incident process](incidents.md).

## Before inviting anyone

0. **Have two platform admins.** Setup makes one. Invite a second person you
   trust, then Users → **Platform role** → platform admin (your password and a code). Otherwise
   losing one phone and its recovery codes leaves nobody able to manage the
   platform (the server-side recovery below still works, but it needs a
   shell on the server).
1. **Install and harden** ([install](install.md)): TLS on the poolers,
   off-host backups, the backup key exported and stored apart from the
   server, SMTP working (sign-up, invitations and every alert need it).
2. **Check isolation** (Settings → Tenant isolation → Run now). Every check
   must pass; it then runs nightly and alerts on failure.
3. **Read the terms** (Platform → Terms) and replace the default text with
   your own ([terms template](terms-template.md)): who you are, how to reach
   you, how long you take to tell people about incidents. Publishing a new
   version makes everyone accept it again.
4. **Sign-up mode** (Platform → Sign-up): keep **invite-only** for the beta.
5. **Plans** (Platform → Plans): the defaults are *Personal* (new personal
   organisations), *Team*, and *Unlimited* (yours). Lower them if your
   servers are small: storage and connections per project matter most.
6. **Outbound traffic** (V2 §10.7): webhooks and HTTP jobs reach only
   public `https://` addresses. If your servers can reach private networks
   or a metadata service on unusual ranges, add them to
   `XENIBASE_OUTBOUND_BLOCK`. Allow-list internal hosts for an organisation
   only when you trust it (Organisations → the org → Outbound).
7. **An external review.** Before anyone outside your own projects relies
   on it, have someone else look at tenant isolation, or pay for a short
   penetration test ([security review](security-review.md)).

## The invite-only beta

Two or three friends' organisations for two weeks, before inviting more
people (V2 §14 M16):

- Invite people (Platform → Users → Invite); each gets a personal
  organisation on the *Personal* plan. Move a team to *Team* (Organisations
  → the org → Plan) when they ask for more.
- Watch **Alerts**, **Usage** (Platform → Usage) and the platform audit log
  daily. Storage locks, reaped statements and dead-lettered webhooks show
  where people hit limits.
- Ask the testers to try a restore of their own (Backups → Restore into a
  new project) and to download one backup as a file (owners: Backups →
  Download), so they know their data can leave.
- Note every question they ask: each is a missing sentence in the
  [user guide](user-guide.md).
- At the end: fix what broke, then tag the release.

## Daily

- **Alerts.** Each alert names the project or node; [operations](operations.md#health-at-a-glance)
  lists what fires when. A failed backup or restore test comes first.
- **Requests** (Platform → Dedicated requests): approve within the node
  capacity you have, or reject with a reason.
- **Payments** (Admin → Billing): attribute any transfer under **Events →
  Unmatched**, look at **Reconciliation** (it should say "No
  differences" for each provider), and record any payment to Xenibase's own
  bank account under **Payments**. See [Payments](payments.md).

- **Capacity** (Platform → Capacity): approve or reject proposals that
  wait (an alert fires). When one fails, read its operation log and the
  server's `/var/log/cloud-init-output.log`. See
  [capacity and costs](capacity.md).
- **Regions** (Platform → Regions): the copy status should show no
  *failed* copies; a failure says why on the backup (`copy_error`) and is
  retried after an hour. See [regions](regions.md).
- **Support** (Platform → Support): answer what's overdue first. The
  queue is ordered by when each ticket is due an answer. Set tickets that
  wait on the customer to *pending*. See [support](support.md).

## Weekly

- **WHT** (Admin → Billing → WHT): chase credit notes older than 30 days;
  send the CSV to the accountant monthly.
- **Overdue organisations:** dunning runs by itself; hold it for an org
  that has agreed to pay later (Admin → Organisations → the org →
  Billing).

- **Capacity** (Nodes, and the [capacity notes](operations.md#capacity)):
  add a shared node before one passes about 200 projects, its disk 70%,
  or its peak client backends 60% of `max_connections`. The
  [load check](load-test.md#v2-load-check) peaked at 311 of 500 backends
  with 150 busy projects on each of two nodes.
- **Restore test** results (Settings → Backup checks).
- **Dependencies:** `govulncheck` and `npm audit` run on every push in CI;
  upgrade when they report something reachable ([upgrades](upgrade.md)).

## Before charging anyone (the paid launch)

V3's launch gate for M23. None of it is code, and all of it comes before
the first real invoice:

1. **The company** is incorporated. Its legal name, address, TIN and VAT
   registration go in Admin → Billing → Settings, so invoices show them.
2. **The wallet provider** (iswallet): the account is approved for live
   use, with the live key and webhook secret in `XENIBASE_ISWALLET_*` and
   the webhook URL registered, and the sandbox rehearsal has been run. See
   [Payments](payments.md).
3. **Legal review**: a lawyer reviews the terms, privacy notice, AUP,
   SLA and DPA. Publish the company's versions (Platform settings →
   Terms; Platform → Legal documents). See [Legal documents](legal.md).
4. **Accounting**: an accountant confirms the VAT and WHT rates and the
   invoice layout, and agrees to take the monthly CSVs (revenue and WHT).
5. **A week of internal billing on real usage**: run your own
   organisations on paid plans for a week. Draft invoices, pay them
   from the wallet and by a recorded manual payment, and check that the reconciliation shows no
   differences and the ledger check is balanced.
6. Then open paid plans, in the EU region first.

## Monthly

- **Costs & margins**: record the month's exchange rate, check
  organisations with negative margins and the Free tier's cost, and send
  the CSV to the accountant with the revenue CSV. When FX erosion grows,
  plan a repricing (price books, 30 days' notice).
- **Rebalancing** (Platform → Capacity): approve the week's batch unless
  automatic rebalancing is on.
- **Revenue** (Platform → Revenue): send the CSV to the accountant with
  the WHT CSV, and look at churn and receivables over 60 days.
- **Legal**: when you publish a new SLA or DPA, check Platform → Legal
  documents a few weeks later for organisations that haven't accepted it.

## Accounts

| Situation | Do |
| --- | --- |
| Someone lost their authenticator and recovery codes | Confirm who they are out of band (a call, not the email that asks), then Users → the user → **Reset two-factor**. It ends their sessions and revokes their API tokens, is audited, and they are emailed. (Ordinary accounts then sign in with a password until they turn two-factor back on; platform staff set up a new authenticator at their next sign-in.) |
| A colleague should help run the platform | They need an active account with two-factor set up. Users → **Platform role** → platform admin. It asks for your password and a code, signs them out, emails them, and is audited. Choosing *user* reverses it; the last admin can't be removed. |
| A colleague should answer support tickets | Users → **Platform role** → *support staff*. They see Platform → Support and the metadata of the organisations that write in, and nothing else of the platform's. See [support](support.md). |
| Nobody can sign in as a platform admin | On the server: `docker compose exec xenibase-server xenibase-server admin list`, then `promote <email>`, `reset-2fa <email>` or `reset-password <email>` (prints a one-hour link when email is down). Audited as done on the server. See [operations](operations.md#platform-admins-and-recovery). |
| Someone leaves a team | The team's owners remove them; it's immediate. You don't need to do anything. |
| An account is abused or compromised | Users → **Disable**: sessions, tokens and database logins end at once. |
| A user wants their account deleted | They do it from Account; it is refused while they're the last owner of an organisation with projects. |

## Organisations

| Situation | Do |
| --- | --- |
| A team needs more | Plan, or a single override (Organisations → the org → Limits). |
| A team needs isolation from others | Give it its own shared cluster (Organisations → the org → Shared cluster). |
| Abuse through webhooks or jobs | **Outbound off** (Organisations → the org → Outbound): its databases keep working. The per-host counters show where the traffic went. |
| Abuse, non-payment, or a security incident | **Suspend** with a reason: apps can't connect, background work stops, a final backup is taken, owners are emailed. **Reinstate** undoes it. |
| A support case needs a look inside | **Break-glass** (Organisations → the org): a reason, at most 4 hours, step-up auth. Owners are emailed and see a banner; everything is in their audit log. End it as soon as you're done. |
| A partner app (Taskiem) wants customers to connect their organisations to it | Register it once: `POST /api/v1/admin/oauth/clients` with its name, `https://` redirect URIs and the most it may ask for (`read`, `write`); send its developers the `client_id` and the one-time `client_secret` out of band. Users consent per organisation and disconnect from Account; `DELETE /api/v1/admin/oauth/clients/{client_id}` disables the app and revokes every token it holds. See [OAuth for third-party apps](oauth-apps.md). |
| An integrator's migrations need their own Postgres roles (Taskiem's `taskiem_app` and `taskiem_dispatch`) | Only on a **dedicated** project (owners are `NOCREATEROLE`, and shared clusters' roles are the platform's). `POST /api/v1/admin/projects/{project_id}/roles` with `{"roles": ["taskiem_app", "taskiem_dispatch"]}` (step-up auth, audited) creates them as the instance's superuser, `NOLOGIN` with no attribute, and grants each to the project's owner role `WITH ADMIN OPTION`, so the owner grants and manages them from then on. They are recorded on the project: a move recreates them on the target, and the demotion check names them (a project with integrator roles can't be demoted). A shared project answers `409 conflict`. |

## Backups and keys

- The **backup key** (Settings → Backup key) decrypts every backup on
  platform targets. Keep its export offline; without it, a lost server
  means lost backups.
- The **master key** (`XENIBASE_MASTER_KEY`) seals credentials; rotate it with
  `xenibase-server -rotate-master-key` ([operations](operations.md#secrets)).
- A **server's agent certificate** may have been copied (a server's memory
  or a core dump exposed): `GET /api/v1/admin/agent-client-certs` lists each
  server's certificates (a day long, named after its host), and `POST
  /api/v1/admin/agent-client-certs/revoke` with `{"server": "<host>"}`, or
  `{"serial": "<hex>"}`, withdraws them. Every agent gets the new list at
  once (the answer names any that didn't; they get it at their next health
  check), and the server issues itself a new certificate. It does not help
  if the master key went too: rotate that as well.
- A full rebuild of the control node: [disaster recovery](disaster-recovery.md).

## Releases

Upgrade with [upgrades](upgrade.md): agents on other nodes first (an agent
on an older minor version than the server is refused work), then the server
and its bundled agent, one node at a time. Read the changelog's *Security* notes before the rest.
