# Deploying releases: `upgrade.sh` and the Deploy workflow

Upgrading the control server by hand (checkout, `./install.sh`, look around)
works, but it leaves the safety checks to whoever is typing. `upgrade.sh`
does them every time, and a GitHub Actions workflow can run it for you behind
an approval. This page covers both. What an upgrade changes in Xenibase is in
[Upgrading](upgrade.md); moving from V2 (PGDock) is a fresh install,
not an upgrade: [the V2 to V4 runbook](upgrade-v2-to-v4.md).

## What `upgrade.sh` does

Run it in `deploy/compose` on the server:

```sh
./upgrade.sh v4.1.0 --dry-run   # the checks only; changes nothing
./upgrade.sh v4.1.0             # asks before it starts
./upgrade.sh v4.1.0 --yes       # for automation
```

1. **Checks before changing anything.** The tag exists and its migrations
   don't go backwards (a downgrade is refused); the working tree is clean;
   the current install is healthy; there is room to build (10 GB by default).
   It says what will change: versions, schema, number of new migrations.
2. **Backs up the metadata database** to `upgrade-backups/` (a `pg_dump`,
   verified by listing it, kept afterwards). Copy it off the server too.
3. **Checks out the tag and runs `./install.sh`**, which builds the images
   while the old version keeps serving, then restarts what changed.
4. **Checks the result**, for up to five minutes: every service running (and
   healthy where it has a check), `xenibase-server -version` reporting the tag,
   and the database at the tag's last migration.
5. **If a check fails, rolls back by itself:** stops the new server, restores
   the metadata backup *if* a migration had run, checks out the previous
   version, runs `./install.sh`, and checks again. Customer databases are not
   touched; clients reconnect through the poolers in a second or two.

Every step is logged to `upgrade-backups/upgrade-<time>.log`.

| Exit status | Meaning |
| --- | --- |
| 0 | Upgraded (or already at that tag). |
| 1 | Refused before changing anything. |
| 2 | Failed, and rolled back (or stopped, with `--no-rollback`). |
| 3 | Failed, and the **rollback failed too**: it prints the backup and the manual steps. |

Options: `--no-rollback` stops at a failure and leaves things as they are (for
when you want to look); `--force` goes ahead although the tree has local
changes or the current install isn't healthy.

**The dashboard and API are down for a few minutes** while containers restart
and migrations run. With a single control server that can't be avoided.

### The first time

An install that doesn't have `upgrade.sh` yet (anything before the release
that added it) takes it from the new tag:

```sh
cd ~/xenibase/deploy/compose
git fetch --tags
git show v4.1.0:deploy/compose/upgrade.sh > /tmp/upgrade.sh
XENIBASE_DIR=$PWD sh /tmp/upgrade.sh v4.1.0
```

From then on it is in the checkout. The script runs from a private copy of
itself, because `git checkout` replaces the file while it runs.

## Where a release comes from

Pushing a tag like `v4.3.0` runs CI on it, and the `release` job at the end
of `.github/workflows/ci.yml` builds the binaries, source bundle and
checksums (`make release`) and publishes the GitHub release, with the tag's
section of `CHANGELOG.md` as its notes. It runs only after `ci`, `e2e`,
`docs-install`, `audit` and `upgrade` have all passed on the tag, so a tag
whose tests fail gets no release; fix the problem, and tag again with the
next version. Re-running the failed jobs from the Actions page also re-runs
the release job once they pass.

## Deploying from GitHub Actions

The **Deploy** workflow (Actions → Deploy → Run workflow) upgrades the server
over SSH:

1. It refuses a tag that doesn't exist or whose CI isn't green (`ci`, `e2e`,
   `docs-install` and `audit` must all have passed on that commit).
2. It waits for a reviewer to approve the `production` environment.
3. It prints the server's status, runs `upgrade.sh <tag> --yes` through the
   deploy key, and prints the status and the result again. `dry_run` runs the
   checks only.

### One-time setup

**On the server**, as root:

```sh
adduser --disabled-password --gecos '' deploy
usermod -aG docker deploy                 # it runs docker compose
chown -R deploy: /path/to/xenibase          # the checkout; deploy runs git in it
```

Make a key pair on your laptop (`ssh-keygen -t ed25519 -f xenibase-deploy`), and
put the public key in `/home/deploy/.ssh/authorized_keys` as a **forced
command**, so the key can do an upgrade and nothing else:

```text
restrict,command="/path/to/xenibase/deploy/compose/deploy-ssh.sh" ssh-ed25519 AAAA… xenibase-deploy
```

`deploy-ssh.sh` accepts exactly `upgrade <tag> [--dry-run]`, `status` and
`last`, and rejects everything else. An upgrade runs detached from the SSH
session, so a dropped connection doesn't kill it half-way; `last` says how it
ended. The server must be able to `git fetch` the repository (public, or give
the deploy user a read-only deploy key).

Try it from your laptop: `ssh -i xenibase-deploy deploy@host status`, and a shell
must be refused: `ssh -i xenibase-deploy deploy@host bash`.

**In GitHub**, Settings → Environments → **production**:

- add **required reviewers** (this is the approval step);
- secret `DEPLOY_SSH_KEY`: the private key;
- secret `DEPLOY_KNOWN_HOSTS`: the output of `ssh-keyscan -p 22 your-host`
  (checked once, by you, so the workflow can't be tricked into another host);
- variables `DEPLOY_HOST` and `DEPLOY_USER` (and `DEPLOY_PORT` if not 22).

## What this does not do

- **Other nodes.** `upgrade.sh` upgrades the control server and its bundled
  agent. Agents on other nodes must be upgraded first, by hand ([remote
  nodes](upgrade.md#remote-nodes)): an agent older than the server is refused
  work.
- **Zero downtime.** There is one control server.
- **Test your apps.** The checks say Xenibase is up and migrated, not that your
  customers' apps work. Open the dashboard, Nodes, Operations and Alerts, and
  run a backup.
- **Undo a rollback you didn't want.** A rollback discards what was done in
  the dashboard since the upgrade started (project data is unaffected).
- **Provision servers.** New nodes come from Platform → Nodes → Add node, or
  from the [Hetzner provider](capacity.md); `upgrade.sh` only upgrades.

## Keeping it safe

- The backups in `upgrade-backups/` hold the metadata database, which includes
  email addresses and secrets sealed with `XENIBASE_MASTER_KEY`. Keep the folder
  readable by root and the deploy user only, and copy dumps off the server
  encrypted.
- Anyone who can approve the `production` environment can change what runs on
  your server. Keep the reviewers list short.
- The forced command is the second lock: even with the key stolen, an attacker
  can only start an upgrade to a tag that exists in your repository.
