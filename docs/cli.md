# The xenibase CLI and API tokens

`xenibase` works with your Xenibase from a terminal, a script or a CI job. It
talks to the same API as the web UI, with an **API token** in place of a
browser session.

## Install

Download the binary for your system from the release
(`xenibase-cli-<version>-<os>-<arch>`, for linux, darwin and windows on
amd64 and arm64), check it against `SHA256SUMS`, and put it on your
`PATH` as `xenibase`. Or build it from source with `go build -o xenibase ./cmd/cli`.

## Log in

```sh
xenibase login --server https://xenibase.example.com
```

The CLI prints a code and opens your browser. Sign in if you aren't
already, check the code matches, choose the **organisation** the CLI
will act in and its **scopes**, and approve (confirming your password and
code if you haven't in the last 10 minutes). The CLI saves the token in
`~/.config/xenibase/config.toml` (readable only by you) as a **context**.
Logging in to another organisation, or another Xenibase, adds another
context:

```sh
xenibase context list
xenibase context use xenibase.example.com/acme
```

Without a browser on the machine, use `--no-browser` and open the link
elsewhere, or paste a token made under **Account → API tokens**:
`xenibase login --server … --token -` (reads it from stdin). For a server
whose certificate isn't publicly trusted, add `--ca-file ca.pem`.

`xenibase logout` revokes the context's token and forgets it.

## Tokens

A token belongs to you and acts in **one organisation**:

| Scope | Allows |
| --- | --- |
| `read` | Viewing everything you can see in the organisation; read-only SQL |
| `write` | Creating and changing: SQL that writes, backups, restoring into a new project, creating projects |
| `admin` | Destructive and settings actions: deleting, promoting and demoting, restoring in place, members, settings, downloading backups (needs `write`) |

A token can be **restricted to some projects**: it then can't see or
touch anything else in the organisation, even what you can. Every token
expires (90 days by default, at most a year, or less if the platform
admin set a lower maximum). It never does more than you can: when your
role changes, it changes; when you leave the organisation, or it is
suspended, the token stops working.

Destructive actions with a token need the `admin` scope **and** a typed
confirmation (`--confirm <name>` in the CLI, a `confirm` field in the
API), in place of the browser's password-and-code step.

Manage your tokens under **Account → API tokens** or with
`xenibase tokens list | create | revoke`. Creating one in the browser asks
for your password and code if you haven't confirmed them in the last 10
minutes. Organisation owners and admins
see and can revoke every token scoped to their organisation
(**Organisation → API tokens**). You get an email when a token is
created and a week before it expires.

A **password reset** by email and a **two-factor reset** by the platform
admin revoke every token you have, since they are how an account is taken
back; so does changing your password, unless you untick **Also revoke my
API tokens** (or send `"revoke_tokens": false`). Make new ones afterwards.

## Connected apps

A third-party app (Taskiem is the first) can ask you to connect one of
your organisations to it instead of you pasting a token into it. The app
sends you to Xenibase's consent page, where you choose the organisation,
see and narrow the scopes it asks for, can restrict it to some projects,
and pick how long the access lasts; approving sends you back to the app.
What the app gets is an ordinary token with exactly that: it never does
more than you can, and it stops when you leave the organisation.

Connected apps are listed under **Account → Connected apps** with the
app's name, organisation, scopes and last use. **Disconnect** revokes the
token and breaks the app's connection at once; organisation owners and
admins can do the same from **Organisation → API tokens**. App developers
start at [OAuth for third-party apps](oauth-apps.md).

## In CI

Make a token for the job: `write` scope, restricted to the project it
deploys, expiring when you'd want to rotate it.

```sh
xenibase tokens create --name "GitHub Actions — blog" --scopes read,write --project blog --expires 90d
```

Store it as a secret and set `XENIBASE_SERVER` and `XENIBASE_TOKEN`; they
take priority over any saved context:

```yaml
# .github/workflows/migrate.yml
jobs:
  migrate:
    runs-on: ubuntu-latest
    env:
      XENIBASE_SERVER: https://xenibase.example.com
      XENIBASE_TOKEN: ${{ secrets.XENIBASE_TOKEN }}
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL -o xenibase https://github.com/…/releases/download/v1.1.0/xenibase-cli-v1.1.0-linux-amd64 && chmod +x xenibase
      - run: ./xenibase sql blog -f migrations/latest.sql
      - run: ./xenibase backup create blog
```

### A database branch per pull request

[`examples/github-actions-branch.yml`](examples/github-actions-branch.yml)
gives every pull request its own copy of the project: each push replaces
the branch `pr-<number>` with a fresh copy of the latest backup and puts
its `DATABASE_URL` in the job's environment; the branch deletes itself
72 hours after the last push, and when the pull request closes. The
token needs only the `write` scope restricted to the project (it covers
the project's branches):

```yaml
- run: xenibase branch create my-app "pr-${{ github.event.number }}" --ttl 72h --replace --env >> "$GITHUB_ENV"
```

The example also masks the URLs in the job log. To keep one branch per
pull request with stable credentials instead, create it once and run
`xenibase branch reset <branch>` on each push: a reset keeps the
database name, URL and password.

## Commands

```
xenibase login | logout | context list | context use <name> | whoami
xenibase orgs list | create <name>
xenibase org members | invite <email> --role <r> | remove <email> | usage | quotas

xenibase projects list | info <p> | create <name> [--tier] [--pg-version 17] | delete <p> --confirm <name>
xenibase connect <p> [--pooled|--session] [--psql]
xenibase creds <p> [--rotate]
xenibase sql <p> -c "select …" | -f file.sql [--json|--csv]

xenibase branch list <p> | create <p> <name> [--from backup|live] [--schema-only|--with-data] [--ttl 72h] [--env] [--replace]
xenibase branch reset <branch> [--from backup|live] | extend <branch> [--ttl 7d] | detach <branch> | delete <branch> --confirm <name>

xenibase backup list <p> | create <p> | restore <p> --backup <id> [--into <name>]
xenibase backup download <p> [--backup <id>] [-o file]   # organisation owners: a pg_dump file (a fresh backup by default)
xenibase promote <p> [--node <id>] [--profile <size>]
xenibase demote <p> [--node <id>] [--check] [--accept-warnings] [--console-writable]
xenibase upgrade <p> --to <major> [--check]        # a newer Postgres major
xenibase moves <p>                                  # recent moves, with the pause each took
xenibase ha status <p> | enable <p> [--node <id>] [--sync] | disable <p>
xenibase ha switchover <p> [--to <member>] | sync <p> on|off
xenibase replicas list <p> | create <p> [--node <id>] [--region <r>] | delete <p> <replica>
xenibase replicas detach <p> <replica> --name <new project>
xenibase move <p> --node <id>                       # platform admins: put the project on another node

xenibase webhooks list <p> | create <p> <name> --tables orders --url https://… [--events INSERT,UPDATE] [--columns c] [--header K=V]
xenibase webhooks delete <p> <webhook> | deliveries <p> <webhook> [--dead] | replay <p> <webhook> --all | --ids 1,2
xenibase jobs list <p> | create <p> <name> --cron '0 3 * * *' [--tz Europe/Berlin] (--sql '…' | --sql @file.sql | --url https://…)
xenibase jobs pause|resume|run|history <p> <job>

xenibase resume <p>   # wake a paused Free project, or restore an archived one

xenibase services status <p> | enable <p> | disable <p>   # backend services: the project's API (docs/backend-services.md)
xenibase keys list <p> | create <p> --name … [--kind publishable|secret] | revoke <p> <key id>
xenibase gen types --lang ts|dart|go --project <p> [-o file] [--package name]   # types for the data API
xenibase auth users list <p> [--search …] | show <p> <user> | invite <p> <email>   # the app's users
xenibase auth users ban <p> <user> [--for 24h] | unban <p> <user> | signout <p> <user> | delete <p> <user>
xenibase auth rotate-key <p>   # sign access tokens with a new key
xenibase auth config <p>       # sign-in methods, phone caps and this month's spend
xenibase auth set <p> '{"settings":{"phone_channels":["whatsapp"],"mfa_policy":"optional"}}'   # any AuthConfigUpdate
xenibase auth hooks <p>        # recent auth webhook deliveries
xenibase migrate supabase <p> [--step all|policies|users|storage] [--source <url>] \
  [--s3-endpoint https://<ref>.supabase.co/storage/v1/s3 --s3-region <r> --s3-access-key <id> --s3-secret-key <secret>]
                             # after importing a Supabase database (docs/migrate-from-supabase.md);
                             # XENIBASE_SUPABASE_DB_URL and XENIBASE_SUPABASE_S3_SECRET keep secrets out of shell history

xenibase billing show | plan free|pro|team [--annual] [--now] [--dry-run]   # owners and billing members
xenibase billing invoices | invoice <number|id> [--pdf file.pdf]
xenibase billing pay <number|id> | wallet | transfer | payments   # pay the invoice from the wallet now, the wallet's balance / amount owed / debit consent, the bank account to transfer to, payments received

xenibase members list <p> | invite <p> <email> --role <r> | remove <p> <email>
xenibase tokens list | create --name … --scopes … [--project <p>] [--expires 90d] | revoke <id>
xenibase operations get <id> [--follow]
```

`demote` runs the eligibility checks first and prints them; `--check`
stops there. Blocked checks (an extension or a role the shared tier
doesn't allow, the size, no room) refuse the demotion; warnings (peak
connections, database settings that reset) need `--accept-warnings`.
`--node` picks the shared cluster by its node's id (by default the one
with the most free capacity, or the organisation's own).

`upgrade` prints its preflight first (including the trial restore of the
schema on the new version) and stops there with `--check`; a blocked check
refuses. See [moves and Postgres versions](moves.md).

`webhooks create` and an HTTP `jobs create` print the signing secret
once. See [webhooks and scheduled jobs](webhooks.md) for the payload, the
signature, and delivery.

`<p>` is a project's name or id; `<branch>` is a branch's id, its name,
or `<parent>/<name>`. `branch create --env` prints `DATABASE_URL=…`,
`DATABASE_URL_SESSION=…`, `XENIBASE_BRANCH_ID=…` and `XENIBASE_BRANCH=…`
lines for `$GITHUB_ENV` or a `.env` file, and, when the parent has
backend services, the branch's own `XENIBASE_API_URL=…`,
`XENIBASE_PUBLISHABLE_KEY=…` and `XENIBASE_SECRET_KEY=…`; `--copy-files`
copies the parent's stored files into the branch in the background, and
again after every `branch reset`;
`--replace` deletes a branch of the same name first.

`policies list <p> [--table t]` shows each table's row-level security
and policies, with the roles as `anon`, `user`, `service` or `everyone`.
`policies lint <p>` prints the security advisor's findings and exits 1
when one is marked danger, for CI (`--json` for the list). `logs api <p>
[--follow] [--status 5xx|404] [--path /data/v1/…]` prints the API's
request log; `--follow` keeps printing new requests. `connect` prints a URL with your
**personal** database login for the project (issuing one the first
time, and remembering it in `~/.config/xenibase/credentials.toml`); `creds
--rotate` issues a new password.

Every command takes `--json` for scripts. Long operations (create,
backup, restore, promote, demote, upgrade, move, delete) stream their progress; `--no-wait`
returns the operation at once.

Exit codes: `0` success, `1` error (including "not found"), `2` usage
error, `3` the operation failed, `4` permission denied.
