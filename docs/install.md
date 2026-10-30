# Install Xenibase on a VPS

This guide takes a fresh server to a running Xenibase with your first
database, in about 15 minutes. CI follows it word for word on a clean
machine (`test/docs/install-from-docs.sh` runs the commands marked below).

## 1. What you need

- **A server** with Ubuntu 24.04 or Debian 12 (other Linux distributions
  with Docker work too), 2 vCPU and 4 GB RAM at least; 8 GB matches the
  shared cluster's default tuning. A Hetzner CX32 or CPX31 is a good fit.
  Put the apps that use Xenibase in the same region.
- **Two DNS names** pointing at the server's public IP (A, and AAAA if you
  have IPv6):
  - the web UI, e.g. `xenibase.example.com`;
  - the database host your apps connect to, e.g. `db.example.com`.
- **An S3-compatible bucket** for backups, with an access key that can read,
  write, and delete objects in it. Cloudflare R2 (no egress fees) is
  recommended; AWS S3, Backblaze B2, and MinIO work.
- **An SMTP server** for Xenibase's email: verification links, password
  resets, invitations, and security notices. Any provider works (Postmark,
  SES, Mailgun, Resend, your own relay); have the host, port, username,
  password, and a From address ready.
- **An authenticator app** (1Password, Google Authenticator, Aegis, …) for the
  platform admin's two-factor sign-in (other users can turn it on if they want).

## 2. Open the firewall

Xenibase needs these inbound ports:

| Port | For |
| --- | --- |
| 80/tcp | Let's Encrypt HTTP challenges, and the redirect to HTTPS |
| 443/tcp | The web UI |
| 5432/tcp | Databases, session mode (PgBouncer) |
| 6543/tcp | Databases, transaction mode (PgBouncer) |

With `ufw`:

```sh
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 5432/tcp
sudo ufw allow 6543/tcp
sudo ufw enable
```

Leave every other port closed. PostgreSQL itself is never exposed: clients
only reach it through PgBouncer, with TLS required.

## 3. Install Docker

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"   # then log out and back in
```

Check it works:

<!-- docs-test -->
```sh
docker version
docker compose version
```

## 4. Get Xenibase

```sh
git clone https://github.com/israel-duff/xenibase.git
cd xenibase
git checkout v4.1.0
```

## Check the server first

Before the installer, run the pre-flight check from the checkout. It changes
nothing; it tells you what would stop the install:

```sh
cd deploy/compose
./preflight.sh xenibase.example.com db.example.com
```

It checks the machine (Linux, CPUs, RAM, disk, clock), Docker and Compose,
that both names resolve to this server's public address, that ports 80, 443,
5432 and 6543 are free, and that the server can reach Let's Encrypt, Docker
Hub and the package registries. To also test your backup bucket and mail
server from here:

```sh
XENIBASE_S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com \
XENIBASE_SMTP_HOST=smtp.example.com XENIBASE_SMTP_PORT=587 \
./preflight.sh xenibase.example.com db.example.com
```

Fix every **FAIL** and read each **WARN**. It can't see your cloud
provider's firewall from inside the server: once Xenibase is running, check
from your laptop with `nc -vz <server address> 80 443 5432 6543`.

**Small servers (4 GB RAM).** The shared cluster's defaults assume 8 GB.
Lower them in `.env` before the first real use, and add swap so the image
build can't run out of memory:

```sh
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
# in deploy/compose/.env:
SHARED_PG_SHARED_BUFFERS=768MB
SHARED_PG_EFFECTIVE_CACHE_SIZE=2GB
```

Run the installer inside `tmux` or `screen`: on one CPU the first build takes
10–25 minutes, and a dropped SSH session would stop it.

## 5. Run the installer

<!-- docs-test -->
```sh
cd deploy/compose
./install.sh
```

It asks for the web UI hostname and an email for Let's Encrypt notices
(or reads `XENIBASE_UI_DOMAIN` and `XENIBASE_ACME_EMAIL`), then:

1. writes `deploy/compose/.env` with freshly generated secrets, readable
   by its owner only (mode 600; an existing `.env` is set to 600 too);
2. builds the images (PostgreSQL 18 with WAL-G, xenibase-server, the agent);
3. starts Caddy, xenibase-server, the metadata database, the shared
   PostgreSQL 18 cluster, both poolers, and the agent;
4. prints the web UI's address and a **one-time setup code**.

Container logs are rotated (Docker's `json-file` driver, 5 files of 50 MB per
container), so they can't fill the disk.

The first run takes a few minutes, mostly building images.

**Back up `.env` now**, somewhere off this server (a password manager is
fine). `XENIBASE_MASTER_KEY` in it decrypts every secret Xenibase stores; the
disaster-recovery runbook needs it.

<!-- docs-test -->
```sh
docker compose ps
```

Every service should be `running` or `healthy`.

## 6. The setup wizard

Open `https://<your UI hostname>`. Caddy gets the UI's certificate on the
first visit, which can take a few seconds. Then:

1. **Setup code**: paste the code from the installer. It changes every time
   `xenibase-server` restarts (including when you recreate it after editing
   `.env`); the current one is always
   `docker compose logs xenibase-server | grep setup_code | tail -1`. To keep
   it fixed, set `XENIBASE_SETUP_CODE` in `.env`. (Before v2.1.1 the installer
   printed the code without its trailing `=`: use the one from the log.)
2. **Platform admin account**: your email and a long password, then scan
   the QR code with your authenticator app and enter a code. Every sign-in
   needs one.
3. **Recovery codes**: ten one-time codes that sign you in if you lose your
   authenticator. Save them in a password manager or on paper.
4. **Email**: your SMTP server. Xenibase sends a test message to your address
   before saving; without working email nobody can verify an account,
   reset a password, or accept an invitation.
5. **Database hostname**: the name your apps will use (`db.example.com`).
   Xenibase checks that it resolves to this server, then gets a Let's Encrypt
   certificate for the poolers.
6. **Backup storage**: the bucket's endpoint (for R2,
   `https://<account-id>.r2.cloudflarestorage.com`), bucket, region (`auto`
   for R2), access key, and secret. Xenibase writes, reads, and deletes a test
   object before saving.
   **Getting R2 credentials:** in Cloudflare, R2 Object Storage → API →
   *Manage API tokens* → *Create API token*, permission **Object Read &
   Write**, scoped to your backup bucket only. Copy the **Access Key ID** and
   **Secret Access Key** it shows once (not the long "token value"). The
   endpoint is the account's, `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`
   (with `.eu.` for an EU-jurisdiction bucket), with no bucket name on the end
   and not the public `r2.dev` address. A `403 AccessDenied` on "write" means
   the token can't write to that bucket (read-only, or scoped elsewhere). If
   you create a general API token instead, the Access Key ID is the token's ID
   and the Secret Access Key is the SHA-256 of the token value.
7. **Backup key**: Xenibase generates the key that encrypts every backup.
   **Download it and store it offline**, then paste it back to confirm.
   Without it no backup can be restored, by you or anyone else.
8. **Local node**: the bundled agent has already registered itself; the
   wizard shows it as healthy.

**Add a second platform admin.** The wizard makes exactly one. Once a
colleague has an account with two-factor set up (invite them under Admin →
Users), choose **Platform role** next to their name and pick platform
admin; it asks for your password and a code. With two admins, one lost phone doesn't lock you out. If you ever
are locked out, recover from the server:
`docker compose exec xenibase-server xenibase-server admin promote <email>` (see
[operations](operations.md#platform-admins-and-recovery)).

## 7. Your first database

Projects → **New project**, give it a name, and create it. Xenibase shows the
password and both connection strings **once**. Copy them, tick "I've saved
this", and you are done:

```text
postgres://<project>_owner:<password>@db.example.com:6543/<project>?sslmode=require   # pooled (transaction mode)
postgres://<project>_owner:<password>@db.example.com:5432/<project>?sslmode=require   # session mode
```

Use the pooled URL for web apps and serverless functions, and the session
URL for migrations, `LISTEN/NOTIFY`, and long-lived workers. The
**Connect** tab has snippets for common frameworks. Check it from your
laptop:

```sh
psql "postgres://<project>_owner:<password>@db.example.com:6543/<project>?sslmode=verify-full&sslrootcert=system"
```

## 8. Finish setting up

- **Who can sign up** (Platform settings → Sign-up): invite-only by
  default; you can allow sign-ups that you approve, or open sign-up
  (optionally limited to email domains). Invite people from Users →
  **Invite someone**, or into an organisation from its Members page.

- **Alerts** (Settings → Alerts): a webhook (Slack, Discord, ntfy, or your
  own endpoint) and optionally SMTP email, for failed or overdue backups,
  unreachable nodes, full disks, a pooler that is down, and failed
  isolation checks. Use **Send test** to check them.
- **Backups** run nightly between 02:00 and 04:00 UTC
  (`XENIBASE_BACKUP_HOUR`, `XENIBASE_BACKUP_JITTER` in `.env`), and a restore
  test runs weekly. The project's **Backups** tab has "Back up now".
- **Prometheus** (optional): set `XENIBASE_METRICS_TOKEN` in `.env` (24+
  characters), run `docker compose up -d`, and scrape
  `https://<UI hostname>/metrics` with that bearer token.
- **Practise a restore** once: [disaster recovery](disaster-recovery.md).

Day-to-day running is in [operations](operations.md); upgrading is in
[upgrades](upgrade.md).

## Troubleshooting

| Symptom | Check |
| --- | --- |
| The UI does not load | `docker compose logs caddy`: DNS for the UI name must point here and port 80 must be open for the certificate. |
| "Database hostname does not resolve to this server" | The DB name's A record, and `XENIBASE_PUBLIC_IPS` in `.env` if the server is behind NAT. |
| Clients get `certificate verify failed` | The poolers' certificate is issued after the wizard's hostname step; Settings → Pooler TLS shows its state. |
| Lost the setup code | `docker compose logs xenibase-server \| grep setup_code`. |
| Anything else | `docker compose logs xenibase-server`; the Operations page shows every step of every action. |
