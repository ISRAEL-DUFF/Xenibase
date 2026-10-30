#!/bin/sh
# Upgrade Xenibase on this host to a release tag: check out the tag and build
# its images while the old version serves, stop xenibase-server and the agent,
# back up the metadata database, start the new version, check the result, and
# put the old version back if the checks fail. (docs/deploying.md)
#
#   cd deploy/compose
#   ./upgrade.sh v4.1.0              # asks before it starts
#   ./upgrade.sh v4.1.0 --yes        # for automation
#   ./upgrade.sh v4.1.0 --dry-run    # only the checks; changes nothing
#
# Options:
#   --yes          don't ask
#   --dry-run      run the pre-checks and say what would happen
#   --no-rollback  on a failed check, stop and leave things as they are
#   --force        go ahead although the working tree is modified or the
#                  current install isn't healthy
#
# The first time (an install that doesn't have this script yet), fetch it from
# the new tag and point it at the install:
#   git fetch --tags && git show v4.1.0:deploy/compose/upgrade.sh > /tmp/upgrade.sh
#   XENIBASE_DIR=$PWD sh /tmp/upgrade.sh v4.1.0
#
# Environment: UPGRADE_BACKUP_DIR (default ./upgrade-backups), UPGRADE_WAIT
# (seconds to wait for the new version to become healthy, default 300),
# UPGRADE_POLL (seconds between checks, default 5), UPGRADE_MIN_FREE_GB
# (default 10).
#
# It needs a few minutes of downtime for the dashboard and API, from the
# backup until the new version is up: databases stay up and clients reconnect
# through the poolers in a second or two. A rollback after a migration
# restores the backup, so what the new version wrote before it failed its
# checks is lost; nothing written before the backup is.
# Exit status: 0 upgraded (or already there), 1 refused before changing
# anything, 2 failed and rolled back, 3 failed and could not be rolled back.
set -eu

die() { echo "upgrade: $*" >&2; exit 1; }
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

tag=
yes=0 dry=0 rollback=1 force=0
for a in "$@"; do
	case "$a" in
	--yes) yes=1 ;;
	--dry-run) dry=1 ;;
	--no-rollback) rollback=0 ;;
	--force) force=1 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
	-*) die "unknown option $a" ;;
	*) [ -z "$tag" ] || die "one tag only"; tag=$a ;;
	esac
done
[ -n "$tag" ] || die "usage: $0 <tag> [--yes] [--dry-run] [--no-rollback] [--force]"
# A release tag: v1.2.3, optionally with a suffix. Nothing else reaches git.
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.]+)?$' || die "'$tag' is not a release tag like v4.1.0"

# git checkout replaces this very file when the tag carries a different one, and
# a shell reads its script as it runs. So run from a private copy, from the
# directory the script was started in (XENIBASE_DIR can name it, for a copy kept
# elsewhere: the first upgrade of an install that doesn't have this script yet).
if [ -z "${XENIBASE_UPGRADE_COPY:-}" ]; then
	dir=${XENIBASE_DIR:-$(cd "$(dirname "$0")" && pwd)}
	copy=$(mktemp "${TMPDIR:-/tmp}/xenibase-upgrade.XXXXXX")
	cp "$0" "$copy"
	XENIBASE_UPGRADE_COPY=$copy XENIBASE_DIR=$dir exec sh "$copy" "$@"
fi
trap 'rm -f "$XENIBASE_UPGRADE_COPY"' EXIT
cd "$XENIBASE_DIR"
[ -f .env ] || die "no .env here: run this in deploy/compose of an installed Xenibase"
command -v docker >/dev/null || die "docker is required"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"
command -v git >/dev/null || die "git is required"
root=$(git rev-parse --show-toplevel 2>/dev/null) || die "this isn't a git checkout"

backup_dir=${UPGRADE_BACKUP_DIR:-./upgrade-backups}
wait_s=${UPGRADE_WAIT:-300}
min_free=${UPGRADE_MIN_FREE_GB:-10}
stamp=$(date +%Y%m%dT%H%M%S)
# The dumps hold password hashes and every sealed secret: what this script
# writes there is for its owner only. (Only there: a umask for the whole script
# would reach the files git checkout writes, which containers read.)
private() { (umask 077 && : > "$1"); }
[ -d "$backup_dir" ] || (umask 077 && mkdir -p "$backup_dir")
chmod 700 "$backup_dir"
log_file="$backup_dir/upgrade-$stamp.log"

# --- helpers -----------------------------------------------------------------

# db runs a query on the metadata database.
db() { docker compose exec -T metadata-db psql -U xenibase -d xenibase -Atc "$1"; }

# schema_version is the database's latest applied migration.
schema_version() { db "select coalesce(max(version_id),0) from goose_db_version" 2>/dev/null | tr -d '[:space:]'; }

# server_version is what the running xenibase-server says it is ("v4.1.0").
server_version() { docker compose exec -T xenibase-server xenibase-server -version 2>/dev/null | awk '{print $2}'; }

# unhealthy lists the services that aren't running (and healthy where they
# have a check); empty means everything is up.
unhealthy() {
	docker compose ps -a --format '{{.Service}}|{{.State}}|{{.Health}}|{{.ExitCode}}' 2>/dev/null | awk -F'|' '
		$2 == "running" && ($3 == "" || $3 == "healthy") { next }
		$2 == "exited" && $4 == "0" { next }
		{ printf "%s(%s%s) ", $1, $2, ($3 != "" ? "/" $3 : "") }'
}

# gates checks the install against the version and schema it should be at; it
# waits up to $wait_s seconds for the services to come up.
gates() {
	want_version=$1 want_schema=$2
	end=$(($(date +%s) + wait_s))
	while :; do
		bad=$(unhealthy)
		sv=$(server_version || true)
		sc=$(schema_version || true)
		if [ -z "$bad" ] && [ "$sv" = "$want_version" ] && [ "$sc" = "$want_schema" ]; then
			log "healthy: xenibase-server $sv, schema $sc, every service running"
			return 0
		fi
		if [ "$(date +%s)" -ge "$end" ]; then
			log "FAILED after ${wait_s}s: services not up: ${bad:-none}; xenibase-server version '${sv:-?}' (want $want_version); schema '${sc:-?}' (want $want_schema)"
			return 1
		fi
		sleep "${UPGRADE_POLL:-5}"
	done
}

# --- the plan ----------------------------------------------------------------

log "fetching tags"
git fetch --tags --quiet 2>/dev/null || log "(couldn't fetch tags; using the ones this checkout has)"
git rev-parse -q --verify "refs/tags/$tag^{commit}" >/dev/null || die "no such tag: $tag"
target=$(git rev-parse "refs/tags/$tag^{commit}")
prev=$(git rev-parse HEAD)
prev_name=$(git describe --tags --always 2>/dev/null || echo "$prev")
prev_ref=$(git symbolic-ref -q --short HEAD 2>/dev/null || true) # the branch we were on, if any
if [ "$target" = "$prev" ]; then
	log "already at $tag; nothing to do"
	exit 0
fi

if [ "$force" -eq 0 ]; then
	git diff --quiet && git diff --cached --quiet || die "the checkout has local changes (git status); commit or stash them, or use --force"
	bad=$(unhealthy)
	[ -z "$bad" ] || die "the current install isn't healthy: $bad (fix it first, or use --force)"
fi
free_gb=$(df -Pk . | awk 'NR==2 {print int($4/1024/1024)}')
[ "${free_gb:-0}" -ge "$min_free" ] || die "only ${free_gb}GB free here; need at least ${min_free}GB to build the images (UPGRADE_MIN_FREE_GB)"

cur_version=$(server_version || true)
cur_schema=$(schema_version || true)
[ -n "$cur_schema" ] || die "can't read the metadata database (is the stack up?)"
want_schema=$(git -C "$root" ls-tree --name-only "$target" internal/store/migrations/ | sed -E 's#.*/0*([0-9]+)_.*#\1#' | sort -n | tail -1)
[ -n "$want_schema" ] || die "$tag has no migrations directory: is it a Xenibase release?"
if [ "$want_schema" -lt "$cur_schema" ]; then
	die "$tag's last migration is $want_schema but the database is at $cur_schema: that is a downgrade, which can't be done by checking out (restore a metadata backup instead; docs/upgrade.md, Rolling back)"
fi
# The tag's install.sh builds without starting (--build-only), so the backup
# can wait until the build is done and nothing writes. One from before that
# would start the new version as it builds: then back up first, as releases
# before this one did.
if git show "$target:./install.sh" 2>/dev/null | grep -q -- '--build-only'; then
	early_backup=0
	steps="build the images (the old version serves), stop xenibase-server and xenibase-agent, back up, start $tag"
else
	early_backup=1
	steps="back up, then $tag's install.sh builds and starts (it can't build alone, so the backup comes first: a rollback loses what is written during the build)"
fi

echo
echo "Upgrade Xenibase on this host"
echo "  from  ${cur_version:-unknown} ($prev_name)   schema $cur_schema"
echo "  to    $tag   schema $want_schema ($((want_schema - cur_schema)) new migrations)"
echo "  steps: $steps"
echo "  backup to  $backup_dir (a copy of the metadata database, kept)"
echo "  on failure: $( [ "$rollback" -eq 1 ] && echo 'restore it and start the previous version' || echo 'stop, leaving things as they are (--no-rollback)')"
echo
if [ "$dry" -eq 1 ]; then
	echo "--dry-run: nothing was changed."
	exit 0
fi
if [ "$yes" -eq 0 ]; then
	printf 'Continue? [y/N] '
	read -r ans
	case "$ans" in y | Y | yes) ;; *) echo "cancelled"; exit 1 ;; esac
fi

perform() {
log "log: $log_file"

dump="$backup_dir/metadata-$stamp-before-$tag.dump"
dumped=0      # $dump is taken and verified
old_stopped=0 # the old xenibase-server is no longer serving: stopped, or replaced
restored=0    # a rollback put the backup back

# --- 1. the backup of the metadata database -------------------------------------

# backup dumps the metadata database to $dump (owner-only, like everything in
# $backup_dir) and checks it; on a failure it says why and returns 1.
backup() {
	log "backing up the metadata database to $dump"
	{ private "$dump" && docker compose exec -T metadata-db pg_dump -U xenibase -Fc xenibase > "$dump"; } || { rm -f "$dump"; log "pg_dump failed"; return 1; }
	[ -s "$dump" ] || { log "the backup is empty"; return 1; }
	docker compose exec -T metadata-db pg_restore -l < "$dump" 2>/dev/null | grep -q 'goose_db_version' || { log "the backup doesn't look like a Xenibase metadata dump"; return 1; }
	private "$dump.meta"
	printf 'from=%s\ncommit=%s\nschema=%s\nversion=%s\nto=%s\n' "$prev_name" "$prev" "$cur_schema" "$cur_version" "$tag" > "$dump.meta"
	dumped=1
	log "backup verified ($(wc -c < "$dump" | tr -d ' ') bytes). Copy it off this server too."
}

# --- 2. rollback ----------------------------------------------------------------

# rollback_now puts the previous release back. While the old version still
# serves (the build failed), that is a checkout and an install, and the
# database is not touched. Once it was stopped: keep the server and agent
# stopped, restore the backup if a migration ran, then check out and install.
rollback_now() {
	log "ROLLING BACK to $prev_name"
	if [ "$old_stopped" -eq 1 ]; then
		docker compose stop xenibase-server xenibase-agent >/dev/null 2>&1 || true
	fi
	now_schema=$(schema_version || true)
	if [ -n "$now_schema" ] && [ "$now_schema" != "$cur_schema" ]; then
		[ "$dumped" -eq 1 ] || { log "the database was migrated ($cur_schema -> $now_schema) and there is no backup to restore"; return 1; }
		log "the database was migrated ($cur_schema -> $now_schema): restoring the backup"
		docker compose exec -T metadata-db psql -U xenibase -d postgres -Atc "drop database xenibase with (force)" -c "create database xenibase owner xenibase" >/dev/null || return 1
		docker compose exec -T metadata-db pg_restore -U xenibase -d xenibase --no-owner < "$dump" >/dev/null || return 1
		[ "$(schema_version)" = "$cur_schema" ] || { log "the restored schema isn't $cur_schema"; return 1; }
		restored=1
	else
		log "the database wasn't migrated: no restore needed"
	fi
	git checkout --quiet "${prev_ref:-$prev}" || return 1
	# The previous release's own install.sh, whatever options it knows: it
	# rebuilds that release's images (normally from Docker's cache) and starts it.
	./install.sh || return 1
	gates "${cur_version:-}" "$cur_schema" || return 1
	return 0
}

fail() {
	log "UPGRADE FAILED: $1"
	if [ "$rollback" -eq 0 ]; then
		if [ "$old_stopped" -eq 0 ]; then
			log "--no-rollback: stopping here. $prev_name is still serving and the database wasn't touched; to go back: git checkout $prev_name, ./install.sh"
		elif [ "$dumped" -eq 0 ]; then
			log "--no-rollback: stopping here, with xenibase-server and xenibase-agent stopped; the database wasn't touched. To go back: git checkout $prev_name, ./install.sh"
		else
			log "--no-rollback: stopping here. The backup is $dump; to go back: stop xenibase-server, restore it (docs/upgrade.md, Rolling back), git checkout $prev_name, ./install.sh"
		fi
		exit 2
	fi
	if rollback_now; then
		if [ "$restored" -eq 1 ]; then
			log "rolled back: Xenibase is running $prev_name again, with the metadata database as it was at the backup: what $tag wrote after it started is gone. See $log_file"
		else
			log "rolled back: Xenibase is running $prev_name again. The metadata database wasn't restored: nothing written to it was lost. See $log_file"
		fi
		exit 2
	fi
	if [ "$dumped" -eq 1 ]; then
		log "THE ROLLBACK FAILED. The metadata backup is $dump (schema $cur_schema, from $prev_name)."
		log "By hand: docker compose stop xenibase-server; restore the backup (docs/upgrade.md, Rolling back); git checkout $prev_name; ./install.sh"
	else
		log "THE ROLLBACK FAILED. No backup was taken: the upgrade failed before it, with the database at schema $cur_schema."
		log "By hand: git checkout $prev_name; ./install.sh"
	fi
	exit 3
}

# --- 3. upgrade --------------------------------------------------------------------

started=$(date +%s)
if [ "$early_backup" -eq 1 ]; then
	backup || die "the backup failed; nothing was changed"
fi
log "checking out $tag"
git checkout --quiet "$target" || fail "git checkout $tag failed"
if [ "$early_backup" -eq 1 ]; then
	log "running install.sh (builds the images; the old version keeps serving until it restarts)"
	old_stopped=1
	./install.sh || fail "install.sh failed"
else
	log "building the images (the old version keeps serving)"
	./install.sh --build-only || fail "building $tag's images failed"
	# From here until the new version is up, nothing writes to the metadata
	# database, so the backup holds everything up to the start of the new one.
	log "stopping xenibase-server and xenibase-agent for the backup (the dashboard and API are down from here)"
	old_stopped=1
	docker compose stop xenibase-server xenibase-agent >/dev/null || fail "couldn't stop xenibase-server and xenibase-agent"
	backup || fail "the backup failed"
	log "starting $tag (xenibase-server runs the migrations as it starts)"
	./install.sh --no-build || fail "install.sh failed"
fi

# --- 4. check ----------------------------------------------------------------------

log "checking the new version"
gates "$tag" "$want_schema" || fail "the new version didn't become healthy"
failed_ops=$(db "select count(*) from operations where status = 'failed' and finished_at > to_timestamp($started)" 2>/dev/null | tr -d '[:space:]' || true)
[ "${failed_ops:-0}" = "0" ] || log "note: $failed_ops operation(s) failed since the upgrade started: look at Operations in the dashboard"

echo
log "UPGRADED to $tag in $(($(date +%s) - started))s. The pre-upgrade backup is $dump (kept; copy it off this server)."
log "Next: open the dashboard, check Nodes, Operations and Alerts, and run a backup."
}

# Run it with its output also going to the log file, and keep its exit status.
rc_file=$(mktemp)
private "$log_file"
( trap 'echo $? > "$rc_file"' EXIT; perform ) 2>&1 | tee -a "$log_file"
rc=$(cat "$rc_file"); rm -f "$rc_file"
exit "${rc:-3}"
