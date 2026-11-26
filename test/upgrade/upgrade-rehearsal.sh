#!/bin/sh
# A real upgrade with the compose bundle, using deploy/compose/upgrade.sh:
#
#   1. install an older release (UPGRADE_FROM, default the latest release
#      before this commit) as a user would;
#   2. try a "release" whose migration is broken: upgrade.sh must fail, restore
#      the metadata backup and bring the older release back, healthy;
#   3. upgrade to this commit: it must come up healthy at the new version and
#      schema, and keep what was in the database.
#
# Like test/docs/install-from-docs.sh, Pebble and a DNS stub stand in for
# Let's Encrypt and public DNS, host ports are offset, and a fake S3 is the
# bucket. Slow: it builds the images three times.
#
#   make test-upgrade
set -eu
repo=$(cd "$(dirname "$0")/../.." && pwd)
# The release to upgrade from: by default the latest release before this
# commit, which is what a real install does next. That is the newest tag of
# the form vX.Y.Z (no -rc, -test or -broken) reachable from HEAD and not on
# HEAD itself (CI on a release tag upgrades from the one before), found at
# run time so it moves on with each release; v4.1.0 when the clone has no
# such tag. v2.1.0 and earlier are PGDock (PGDOCK_* settings, pgdock
# containers and database user): an upgrade across the rename is not
# supported (install fresh and move the projects).
previous_release() {
	head=$(git -C "$repo" rev-parse HEAD)
	git -C "$repo" tag --merged HEAD --list 'v[0-9]*' --sort=-v:refname | while read -r t; do
		case "$t" in v*[!0-9.]* | v*..* | v.* | v*.) continue ;; esac
		[ "$(git -C "$repo" rev-parse "$t^{commit}")" != "$head" ] || continue
		echo "$t"
		break
	done
}
from=${UPGRADE_FROM:-$(previous_release)}
from=${from:-v4.1.0}
echo "upgrading from $from"
work=$(mktemp -d)
cleanup() { cd "$work/deploy/compose" 2>/dev/null && docker compose down -v --remove-orphans >/dev/null 2>&1; cd /; rm -rf "$work"; }
trap cleanup EXIT
step() { printf '\n=== %s\n' "$*"; }
die() { echo "FAIL: $*" >&2; exit 1; }

head_commit=$(git -C "$repo" rev-parse HEAD)
git clone -q "$repo" "$work"
cd "$work"
git config user.email test@xenibase.test && git config user.name test
git checkout -q "$from"

# Test-only images for the overlay (Pebble, fake S3); the bundle builds the rest.
# shellcheck disable=SC2086
docker build ${XENIBASE_BUILD_FLAGS:-} -q -t xenibase-pebble:local -f "$repo/test/e2e/bundle/Dockerfile.pebble" "$repo/test/e2e/bundle" >/dev/null
# shellcheck disable=SC2086
docker build ${XENIBASE_BUILD_FLAGS:-} -q -t xenibase-fakes3:local -f "$repo/test/e2e/bundle/Dockerfile.fakes3" "$repo" >/dev/null

export XENIBASE_UI_DOMAIN=xenibase.test XENIBASE_ACME_EMAIL=ops@xenibase.test XENIBASE_PUBLIC_IPS=127.0.0.1
export COMPOSE_PROJECT_NAME=xenibase-upgrade COMPOSE_FILE=compose.yaml:../../test/e2e/bundle/compose.e2e.yaml
export XENIBASE_NETWORK=xenibase-upgrade XENIBASE_DB_SESSION_PORT=25432 XENIBASE_DB_POOLED_PORT=26543
export SHARED_PG_SHARED_BUFFERS=128MB SHARED_PG_EFFECTIVE_CACHE_SIZE=512MB
export UPGRADE_WAIT=240 UPGRADE_POLL=5 UPGRADE_MIN_FREE_GB=1

cd "$work/deploy/compose"
step "install $from"
./install.sh
sv() { docker compose exec -T xenibase-server xenibase-server -version | awk '{print $2}'; }
schema() { docker compose exec -T metadata-db psql -U xenibase -d xenibase -Atc "select max(version_id) from goose_db_version" | tr -d '[:space:]'; }
old_version=$(sv); old_schema=$(schema)
echo "installed: $old_version, schema $old_schema"
# Something in the database that must survive a failed upgrade and a good one.
docker compose exec -T metadata-db psql -U xenibase -d xenibase -Atc "create table upgrade_canary (note text); insert into upgrade_canary values ('before')" >/dev/null
canary() { docker compose exec -T metadata-db psql -U xenibase -d xenibase -Atc "select string_agg(note, ',') from upgrade_canary" | tr -d '[:space:]'; }

# --- a release whose migration is broken --------------------------------------
step "a release with a broken migration is rolled back"
cd "$work"
git checkout -q -b broken "$head_commit"
last=$(ls internal/store/migrations/*.sql | sed -E 's#.*/([0-9]+)_.*#\1#' | sort -n | tail -1)
next=$(printf '%05d' $(( $(echo "$last" | sed 's/^0*//') + 1 )))
printf -- '-- +goose Up\nSELECT this_is_not_valid_sql FROM nowhere;\n\n-- +goose Down\nSELECT 1;\n' > "internal/store/migrations/${next}_broken.sql"
git add -A && git commit -q -m "a release that cannot migrate" && git tag v9.9.9-broken
git checkout -q "$from"
cd deploy/compose
# The older release has no upgrade.sh yet: run this commit's, as the first real deploy does.
upgrade="$repo/deploy/compose/upgrade.sh"
set +e; XENIBASE_DIR=$PWD sh "$upgrade" v9.9.9-broken --yes; rc=$?; set -e
[ "$rc" = 2 ] || die "a broken upgrade should exit 2 (rolled back), got $rc"
[ "$(sv)" = "$old_version" ] || die "after the rollback the server is $(sv), want $old_version"
[ "$(schema)" = "$old_schema" ] || die "after the rollback the schema is $(schema), want $old_schema"
[ "$(canary)" = before ] || die "the database lost its data in the rollback: '$(canary)'"
[ "$(git rev-parse HEAD)" = "$(git rev-parse "$from^{commit}")" ] || die "the checkout isn't back at $from"
echo "rolled back cleanly to $old_version"

# --- the real upgrade ------------------------------------------------------------
step "upgrade to this commit"
cd "$work"
# Annotated, like a release tag: git describe prefers an annotated tag over a
# lightweight one, so on a commit that carries a real release tag (CI on a tag)
# a lightweight test tag would lose, and the server would report the release name.
git tag -a v9.9.9-test -m "rehearsal" "$head_commit"
cd deploy/compose
docker compose exec -T metadata-db psql -U xenibase -d xenibase -Atc "insert into upgrade_canary values ('between')" >/dev/null
XENIBASE_DIR=$PWD sh "$upgrade" v9.9.9-test --yes
[ "$(sv)" = v9.9.9-test ] || die "the server reports '$(sv)', want v9.9.9-test (the bundle must build with its version)"
want_schema=$(ls "$work"/internal/store/migrations/*.sql | sed -E 's#.*/0*([0-9]+)_.*#\1#' | sort -n | tail -1)
[ "$(schema)" = "$want_schema" ] || die "the schema is $(schema), want $want_schema"
[ "$(canary)" = before,between ] || die "the database lost its data in the upgrade: '$(canary)'"
ls upgrade-backups/metadata-*-before-v9.9.9-test.dump >/dev/null || die "no pre-upgrade backup was kept"
# And upgrade.sh is idempotent.
sh ./upgrade.sh v9.9.9-test --yes | grep -q "already at v9.9.9-test" || die "a repeat upgrade should do nothing"
step "PASS: rolled back a broken upgrade, then upgraded to v9.9.9-test (schema $want_schema)"
