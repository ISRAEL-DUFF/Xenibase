# Disaster recovery

What to do when the control node is lost (spec §11.6). Practise it once
before you need it.

## What you need, kept off the server

- **`.env`** from `deploy/compose` (above all `XENIBASE_MASTER_KEY`: it
  decrypts the secrets in the metadata DB).
- **The backup key file** you downloaded in the setup wizard
  (`xenibase-backup-key-<fingerprint>.txt`). Without it no backup can be
  decrypted, by you or anyone else.
- **Credentials for the backup bucket.**

## 1. Rebuild the control node

On the new host, clone the repository and put the saved `.env` in
`deploy/compose` **before** running `./install.sh`, so it keeps the same
master key. Start only the metadata database for now:

```sh
cd deploy/compose
docker compose up -d metadata-db
```

## 2. Restore the metadata

The metadata DB backs itself up every night to
`<prefix>/metadata/<time>.dump.enc` in the bucket. Fetch the newest one
with any S3 client, decrypt it with the backup key, and restore it:

```sh
aws s3 cp s3://<bucket>/<prefix>/metadata/<newest>.dump.enc .   # or rclone, mc, …
# --user 0: the key file is usually readable by root only (run this as root).
docker compose run --rm -T --no-deps --user 0 -v "$PWD:/work" xenibase-agent \
  decrypt --key-file /work/xenibase-backup-key-<fingerprint>.txt \
  --in /work/<newest>.dump.enc --out /work/metadata.dump &&
docker compose cp metadata.dump metadata-db:/tmp/metadata.dump &&
docker compose exec metadata-db pg_restore -U xenibase -d xenibase --clean --if-exists --no-owner /tmp/metadata.dump
```

`xenibase-agent decrypt` checks every chunk; with `--out`, a wrong key or a
damaged object fails without writing a partial file, and `&&` stops there.
Don't redirect its standard output to a file instead: a failure part-way
leaves what it wrote, which only the exit status reports.

## 3. Start everything

```sh
docker compose up -d --wait
```

xenibase-server runs its migrations, re-renders the pooler configuration
(routes and SCRAM verifiers come from the metadata), and reloads the
poolers, so existing connection strings and passwords keep working once
the databases are back.

## 4. Re-register agents

If an agent's state volume (`agent-state`) survived, it reconnects by
itself. Otherwise the metadata still pins the old certificate: open
**Nodes → Re-register…**, and run the printed command on the node (the
token works once, for 24 hours).

## 5. Bring back project data

If the shared cluster's volume survived, there is nothing to do. If it was
lost too, each project's database is gone while its record remains: open
the project's **Backups** tab and **Restore** its latest backup into a new
project (in-place restore needs the old database to exist). Point the app at
the new connection string. Final backups of deleted projects stay listed
for 30 days.

## Restoring a project without Xenibase

A project with its own backup key (**Backups → Storage → Use a project
key**) can be restored with standard tools alone, from the downloaded
key file and the bucket, even if every Xenibase server is gone. This is
the point of an organisation's own storage target: the data and the key
are both yours.

1. Get the object from the bucket with any S3 client. Logical backups
   are under `<prefix>/projects/<project id>/logical/` (and `final/`,
   `safety/`); the ones made with the project key end in `.dump.gpg`.
2. Decrypt it with the downloaded key file, in a throwaway GnuPG home:

   ```sh
   export GNUPGHOME=$(mktemp -d)
   gpg --batch --import xenibase-backup-key-<project id>.asc
   gpg --batch --decrypt backup.dump.gpg > backup.dump
   ```

   gpg checks the message's integrity only at its end: if it reports an
   error, `backup.dump` is incomplete, so don't restore it.

3. Restore the `pg_dump` custom-format archive into any PostgreSQL 18:

   ```sh
   createdb restored
   pg_restore --no-owner --no-acl -d restored backup.dump
   ```

Dedicated projects' WAL-G base backups and WAL under
`<prefix>/instances/<instance id>/wal-g-…/` use the same key: point
`wal-g backup-fetch` at that prefix with `WALG_PGP_KEY_PATH` set to the
key file. Backups made before the project key was enabled (`.dump.enc`)
need the instance backup key and `xenibase-agent decrypt` instead.
