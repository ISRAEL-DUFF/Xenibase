# User guide

For people who use a Xenibase someone else runs: organisations, members and
roles, your database logins, branches, your own backup bucket, and leaving.
The [CLI guide](cli.md) covers the same from a terminal and in CI;
[webhooks and jobs](webhooks.md) has its own page.

## Signing in

An account starts from an invitation (or a sign-up, if the platform allows
it). You confirm your email address and set a password; that is all it takes
to sign in.

**Two-factor sign-in is optional.** To add it, open **Account → Security →
Two-factor sign-in → Turn on**, confirm your password, scan the QR code with an
authenticator app (TOTP) and enter the first code. You are shown **10 recovery
codes** once: keep them somewhere safe. Each code works once, if you lose your
phone. From then on, signing in needs your password and a code. To go back,
**Turn off** asks for your password and a current code (or a recovery code).
Both changes are emailed to you. With no code and no recovery code left, the
platform admin can reset your two-factor after confirming who you are, and you
sign in with your password until you turn it on again. Platform admins and
support staff always have it on.

**Account** lists your sessions (revoke any), lets you change your password
and name, and shows new recovery codes (after confirming your password and
code). A password reset by email signs you out everywhere.

### Getting around

The bar at the top shows where you are (organisation, project, branch);
the icons on the left are the current project's or organisation's
sections. **⌘K** (Ctrl+K) searches projects and pages. Press **?** for
every keyboard shortcut: **g** then a letter jumps to a section (**g t**
Table Editor, **g s** SQL Editor, **g o** overview, **g p** projects…),
and **[** hides the section menu. On a phone, the menu button at the top
left opens the same navigation. The theme (dark, light or your system's)
is in your account menu.

## Organisations

Everything you create belongs to an **organisation**. You start with a
personal one ("<your name>'s projects"); you can create more, and be
invited into other people's. The switcher at the top changes which one you
are looking at. Its home page shows the projects as cards, each with its
branches listed inside; the toggle beside the filters switches to a list.

| Org role | Can |
| --- | --- |
| **Owner** | Everything an admin can, plus add or remove owners, transfer projects out, delete the organisation. An org always keeps at least one owner. |
| **Admin** | Manage members and invitations, settings, backup storage, tokens, every project (as its admin), usage and the audit log. |
| **Member** | See only the projects they're added to, with that project's role; create projects if the org allows it (Settings → *Members can create projects*, on by default), becoming their admin. |
| **Billing** | See and change billing (plan, invoices, business details, contacts, budget and spend cap) and nothing in the projects. For finance staff; only owners can give this role. |

Only owners and billing members see **Billing**: see [Billing](billing.md).

On a project, roles are:

| Project role | Can |
| --- | --- |
| **Admin** | Everything below, plus restore in place, rotate the app password, settings and extensions, members, storage target and backup key, promote and demote, delete. |
| **Developer** | Read and write data (console, table editor, schema changes), branches, webhooks and jobs, backups and restores into a new project. |
| **Read-only** | See the project, its metrics and operations; read-only console and personal credentials. |

Org owners and admins are admins of every project in the organisation.

### Members and invitations

**Organisation → Members** invites by email with an org role, and
optionally project roles; **Project Settings → Members** adds someone to one project
(they join the organisation as a member). Invitations last 7 days and can
be revoked until they're accepted. If the address already has an account,
accepting just adds the membership.

You can **leave** an organisation yourself (Organisation → Leave), unless
you are its last owner: transfer ownership first.

**Removing** someone takes effect at once: their open database connections
end, their database logins are dropped, their API tokens for the
organisation are revoked, and their next request in the UI is refused. A
console query they already started finishes, within its timeout.

## Your database login

The app's connection string (shown once, when the project is created) uses
the project's owner role. People shouldn't share it: each member gets
their own login per project, so removing someone never means changing the
app's password.

**Project Settings → Members → Get my credentials** creates yours,
`<database>_u_<id>`, and shows its password once; you can rotate it any
time. Admins and developers get read/write access (what they create is
owned by the project, so the app can use it); read-only members get read
access to every table, including new ones. Your login follows the project
through promotion, demotion, restores and branch resets.

## Branches

A branch is a copy of a project, on the shared tier, with its own
database, password and connection string: for a risky migration, a
feature, or one per pull request in CI ([CLI guide](cli.md#a-database-branch-per-pull-request)).

- **Source:** the latest backup (no load on the parent) or **live** (a
  fresh dump now). **Contents:** full, or schema only (the default for a
  project marked *contains sensitive data*). **Lifetime:** kept, or deleted
  after a TTL (1 hour to 30 days, 7 days by default); the creator is emailed
  a day before.
- **Reset** refills it from the parent, keeping its name, URL and
  passwords, so `.env` files and pipelines keep working.
- **Detach** makes it an ordinary project (which can then be promoted).
- **With backend services** on the parent, the branch gets an API of its
  own: a new URL (ref), new publishable and secret keys and a new signing
  key, so neither's keys or users' tokens work on the other. It keeps the
  parent's exposed schemas, public tables, CORS origins and auth settings
  and templates, but not its secrets (SMTP password, SMS and OAuth
  credentials, captcha and hook secrets): set those on the branch. Its
  users are the ones the copy brought (none for a schema-only branch);
  they sign in again there. The create response and
  `xenibase branch create --env` give `XENIBASE_API_URL`,
  `XENIBASE_PUBLISHABLE_KEY` and `XENIBASE_SECRET_KEY`. Stored files are not
  copied unless you ask (**Copy its stored files too**, or `--copy-files`):
  they are then copied in the background and count toward file storage.
- A branch can't have branches. Its parent's webhooks and jobs are not
  copied. Branches count against your organisation's branch and storage
  quotas, and have no nightly backups unless you turn them on.

## Backups and your own bucket

Projects are backed up nightly (dedicated ones also continuously, with
point-in-time recovery for 7 days). **Database → Backups** lists them,
backs up now, restores into a new project, or restores in place (after a
safety backup, for project admins, with your password and code).

Backups go to the platform's storage unless your organisation brings its
own bucket, which then doesn't count against your backup quota:

1. Create a bucket (AWS S3, Cloudflare R2, Backblaze B2, MinIO, …) and an
   access key that can only read, write, list and delete under one prefix,
   for example with this S3 policy:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       { "Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": "arn:aws:s3:::acme-backups",
         "Condition": { "StringLike": { "s3:prefix": ["xenibase/*"] } } },
       { "Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
         "Resource": "arn:aws:s3:::acme-backups/xenibase/*" }
     ]
   }
   ```

2. **Organisation → Backup storage → Add**: endpoint, region, bucket,
   prefix, key. Xenibase writes, reads, lists and deletes a test object
   before it saves the target; the key is encrypted and never shown again.
   Nobody outside your organisation, including the platform admin, sees
   it.
3. **Project Settings → Backup storage**: pick the target. New backups go there;
   existing ones stay restorable where they are, or can be copied over.
4. Optionally **enable a project backup key** and download it (with your
   password and code). New backups are then encrypted with a key you hold,
   and the download's README shows how to decrypt and restore one with
   `gpg` and `pg_restore`, without Xenibase.

Deleting your organisation never deletes what is in your own bucket.

## Webhooks, jobs and the API

- [Webhooks and scheduled jobs](webhooks.md): table changes POSTed to a
  URL, signed; SQL or HTTP on a cron schedule.
- [The CLI and API tokens](cli.md): `xenibase login`, tokens scoped to one
  organisation (read, write or admin, optionally to some projects), and
  branches in CI.
- [Connected apps](cli.md#connected-apps): connect an organisation to a
  third-party app with your consent, and disconnect it from Account.

## Query insights

On Pro and Team plans and dedicated projects, **Query insights** shows the
queries that take your database's time, refreshed every 5 minutes: open
one for its latency over time and its plan (EXPLAIN, which doesn't run
the query). The **Indexes** tab suggests indexes for slow lookups and
foreign keys, with the `CREATE INDEX CONCURRENTLY` statement to run or
save as a migration; enable the `hypopg` extension to see how much each
would help before creating it. It also lists unused and duplicate
indexes, table bloat, slow statements, and sessions waiting on locks. See
[query insights](query-insights.md).

## Limits

Your organisation's plan sets its limits: projects, branches, storage,
connections, backup storage, webhook deliveries and jobs, console queries,
and operations at once. **Organisation → Usage & quotas** shows where you
are. Going past a creation limit is refused with the limit named. On
storage:

- at **90%** of a project's limit you get an email and a banner;
- at **100%** it is read-only by default (apps can still delete inside
  `BEGIN READ WRITE`; the console works, scheduled jobs' writes fail);
- at **120%** apps can't connect, but the console still works so you can
  delete data and **Reclaim space**.

Statements running over 10 minutes, and transactions idle over 5, are
ended on the shared tier.

### Connections

A project's **connection limit** (Project Settings → Connections and timeouts → **Max
backend connections**) caps how many connections its logins hold open to
Postgres. It is not the number of clients your apps can open: those
connect to the pooler (up to 2,000 on each port). In transaction mode,
port 6543, many clients share a small pool of Postgres connections; in
session mode, port 5432 (needed for `LISTEN/NOTIFY` and migrations), each
client holds one.

- **Shared projects**: 20 by default, up to your plan's limit.
- **Dedicated projects**: 90 by default. Each Postgres connection is a
  process, so how many an instance holds depends on its memory:
  `max_connections` is 50 per GB, from 100 (2 GB or less) to 500 (10 GB
  or more), for example 200 on a 4 GB instance and 400 on 8 GB. The
  project can have all of it but 10, which Xenibase keeps for backups,
  replication, probes and backend services: up to 190 on 4 GB. The
  panel shows the most this instance allows. Resizing to less
  memory lowers the limit to fit.

Instances created before connections grew with memory keep 100 until they are resized.

## Free projects that sleep

On the Free plan, a project with no client connections for 7 days is
**paused**. You get an email a day before. Its data is kept. The next
connection wakes it: that first connection is refused with *"This project
was paused for inactivity and is resuming. Retry in about 30 seconds."*,
and the retry works. With the pooled URL, the message comes with the
first query. A project paused for 90 days is **archived** to a verified
backup. The next connection restores it, which takes minutes. Archived
projects are deleted after a year, with 30 and 7 days' notice. You can
also resume a project with **Resume** on its page, or with
`xenibase resume <project>`. Paid plans never sleep: see **Pricing** in the
sidebar. Details: [The Free tier](free-tier.md).

## Getting help

**Organisation → Support**: open a ticket, follow it and reply. You can also
email the support address and reply to Xenibase's emails, which thread into
the ticket. Pro and Team organisations can register WhatsApp numbers that
message support directly. Pro tickets are answered within a business day,
Team tickets within four business hours, and urgent issues on a paid plan
within an hour, any time. Details: [Support](support.md).

**Organisation → Legal**: the service level agreement, the data processing
agreement and your order form, if you have one. An owner accepts them for
the organisation, and can also accept them when changing plan. Details:
[Legal documents](legal.md).

## Your data

- **Export** any project as a `pg_dump` file at any time (Project →
  Backups → Download, or `xenibase backups download`).
- The platform admin can't open your databases, console or backups through
  Xenibase except with **break-glass** access: at most 4 hours, with a reason,
  emailed to every owner, shown as a banner while it lasts, and marked in
  your audit log. Any owner can end it. It is an audit control, not
  encryption: someone with root on the servers can read the data.
- **Deleting an organisation** (owners, Organisation → Delete) takes each
  project's final backup and gives a 7-day grace period in which an owner
  can cancel; then the projects go. Final backups on the platform's
  storage are kept 30 days.
- **Deleting your account** is refused while you are the last owner of an
  organisation that still has projects.
