#!/bin/sh
# Tests deploy/compose/upgrade.sh without Docker: a throwaway git repo with two
# tagged releases (v1.0.0 with 3 migrations, v2.0.0 with 5), a fake `docker`
# that keeps its state in files, and a fake install.sh that "installs" the
# checked-out release (and can fail or come up unhealthy, as a scenario says).
#
#   make test-deploy
set -eu
here=$(cd "$(dirname "$0")" && pwd)
script=$here/../../deploy/compose/upgrade.sh
pass=0 fail=0
check() { # check <name> <condition command...>
	name=$1; shift
	if "$@" >/dev/null 2>&1; then pass=$((pass + 1)); else
		fail=$((fail + 1)); echo "  FAIL: $name"
		# DEBUG=1 shows the script's own output for the scenario that failed.
		if [ -n "${DEBUG:-}" ] && [ -f "${T:-/nonexistent}/out" ]; then sed 's/^/        | /' "$T/out" | head -40; fi
	fi
}
has() { grep -q -- "$2" "$1"; }
hasnt() { ! grep -q -- "$2" "$1"; }
# before <a> <b>: the first call to fake docker or install.sh matching a came
# before the first matching b.
line_of() { grep -n -- "$2" "$1" | head -1 | cut -d: -f1; }
before() { a=$(line_of "$FAKE/calls" "$1"); b=$(line_of "$FAKE/calls" "$2"); [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]; }
mode_is() { [ "$(ls -ld "$2" | cut -c1-10)" = "$1" ]; } # mode_is -rw------- <file>
umask 022 # the usual one, which the scripts must not rely on

# setup builds a fresh repo, fake docker and state in $T and enters deploy/compose.
setup() {
	T=$(mktemp -d)
	export FAKE=$T/fake T
	mkdir -p "$FAKE/bin" "$T/repo/deploy/compose" "$T/repo/internal/store/migrations"
	cd "$T/repo"
	git init -q . && git config user.email t@t && git config user.name t && git checkout -q -b main
	cp "$script" deploy/compose/upgrade.sh
	: > deploy/compose/.env
	cat > deploy/compose/install.sh <<'I'
#!/bin/sh
# Fake installer: "installs" whatever is checked out. Failure scenarios apply to v2.0.0 only.
ver=$(git describe --tags --always)
echo "fake install.sh at $ver $*"
mode=$(cat "$FAKE/mode" 2>/dev/null || echo ok)
# Building alone changes nothing that runs. (LEGACY=1 deletes these two lines
# from v2.0.0's copy: an install.sh from before the options.)
[ "${1:-}" = --build-only ] && { echo "INSTALL build-only $ver" >> "$FAKE/calls"; [ "$ver:$mode" = v2.0.0:build_fail ] && exit 1; exit 0; }
step=full; [ "${1:-}" = --no-build ] && step=start
echo "INSTALL ${step:-full} $ver" >> "$FAKE/calls"
echo running > "$FAKE/server"
n=$(ls "$(git rev-parse --show-toplevel)"/internal/store/migrations/*.sql | wc -l | tr -d ' ')
if [ "$ver" = v2.0.0 ]; then
	case "$mode" in
	build_fail) exit 1 ;;                                         # a full install whose build fails
	install_fail) echo 5 > "$FAKE/schema"; exit 1 ;;           # migrated, then died
	unhealthy) echo "$ver" > "$FAKE/version"; echo "$n" > "$FAKE/schema"
		printf 'xenibase-server|restarting||0\nmetadata-db|running|healthy|0\n' > "$FAKE/ps"; exit 0 ;;
	unhealthy_nomigrate) echo "$ver" > "$FAKE/version"
		printf 'xenibase-server|restarting||0\nmetadata-db|running|healthy|0\n' > "$FAKE/ps"; exit 0 ;;
	esac
fi
echo "$ver" > "$FAKE/version"; echo "$n" > "$FAKE/schema"
printf 'xenibase-server|running|healthy|0\nmetadata-db|running|healthy|0\npooler-tx|running|healthy|0\n' > "$FAKE/ps"
I
	chmod +x deploy/compose/install.sh deploy/compose/upgrade.sh
	for i in 1 2 3; do echo "-- $i" > internal/store/migrations/0000${i}_m.sql; done
	# BARE=1: the old release has no upgrade.sh (as v2.1.0 doesn't); v2.0.0 carries it.
	[ -z "${BARE:-}" ] || rm -f deploy/compose/upgrade.sh
	git add -A && git commit -q -m v1 && git tag v1.0.0
	cp "$script" deploy/compose/upgrade.sh && chmod +x deploy/compose/upgrade.sh
	for i in 4 5; do echo "-- $i" > internal/store/migrations/0000${i}_m.sql; done
	# LEGACY=1: v2.0.0's install.sh can't build without starting.
	if [ -n "${LEGACY:-}" ]; then
		grep -v -e '--build-only' -e '--no-build' deploy/compose/install.sh > "$T/install.sh" && cat "$T/install.sh" > deploy/compose/install.sh
	fi
	git add -A && git commit -q -m v2 && git tag v2.0.0
	git checkout -q v1.0.0 && git checkout -q -B main v1.0.0 # production is on v1.0.0, on main
	# the fake docker
	cat > "$FAKE/bin/docker" <<'D'
#!/bin/sh
echo "docker $*" >> "$FAKE/calls"
case "$*" in
"compose version"*) echo "Docker Compose version v2.29.0" ;;
"compose ps -a"*) cat "$FAKE/ps" ;;
"compose exec -T xenibase-server xenibase-server -version") echo "xenibase-server $(cat "$FAKE/version") (commit x)" ;;
"compose exec -T metadata-db psql"*goose_db_version*) cat "$FAKE/schema" ;;
"compose exec -T metadata-db psql"*"drop database"*) [ -f "$FAKE/restore_fails" ] && exit 1; echo "RESTORE-DROP" >> "$FAKE/events" ;;
"compose exec -T metadata-db psql"*operations*) echo 0 ;;
"compose exec -T metadata-db pg_dump"*) [ -f "$FAKE/dump_fails" ] && exit 1
	echo "DUMP server=$(cat "$FAKE/server")" >> "$FAKE/events"; echo "PGDMP goose_db_version schema=$(cat "$FAKE/schema")" ;;
"compose exec -T metadata-db pg_restore -l"*) cat > /dev/null; echo "; goose_db_version" ;;
"compose exec -T metadata-db pg_restore"*) [ -f "$FAKE/restore_fails" ] && exit 1; sed -n 's/.*schema=//p' > "$FAKE/schema"; echo "RESTORE-LOAD" >> "$FAKE/events" ;;
"compose stop xenibase-server xenibase-agent") echo "STOP" >> "$FAKE/events"; echo stopped > "$FAKE/server" ;;
*) echo "fake docker: unhandled: $*" >&2; exit 99 ;;
esac
D
	chmod +x "$FAKE/bin/docker"
	echo v1.0.0 > "$FAKE/version"; echo 3 > "$FAKE/schema"; : > "$FAKE/events"; echo running > "$FAKE/server"
	printf 'xenibase-server|running|healthy|0\nmetadata-db|running|healthy|0\npooler-tx|running|healthy|0\n' > "$FAKE/ps"
	export PATH="$FAKE/bin:$PATH" UPGRADE_WAIT=2 UPGRADE_POLL=1 UPGRADE_MIN_FREE_GB=0
	cd deploy/compose
}
run() { # run <args...>: output in $T/out, status in $rc
	set +e; sh ./upgrade.sh "$@" > "$T/out" 2>&1; rc=$?; set -e
}
head_is() { [ "$(git describe --tags --always)" = "$1" ]; }

echo "upgrade.sh"

# 1. The good upgrade.
setup; run v2.0.0 --yes
check "success: exit 0" [ "$rc" = 0 ]
check "success: now v2.0.0" head_is v2.0.0
check "success: version reported" [ "$(cat "$FAKE/version")" = v2.0.0 ]
check "success: a verified backup was kept" sh -c "ls upgrade-backups/metadata-*-before-v2.0.0.dump && ls upgrade-backups/*.meta"
check "success: backup has the old schema" has "$(ls upgrade-backups/metadata-*.dump)" "schema=3"
check "success: built the tag's images first" before "INSTALL build-only v2.0.0" "compose stop xenibase-server xenibase-agent"
check "success: stopped the server and agent before the backup" before "compose stop xenibase-server xenibase-agent" pg_dump
check "success: backed up before starting the new version" before pg_dump "INSTALL start v2.0.0"
check "success: nothing was serving during the dump" has "$FAKE/events" "DUMP server=stopped"
check "success: started without building again" hasnt "$FAKE/calls" "INSTALL full"
check "success: the dump is the owner's only" mode_is -rw------- "$(ls upgrade-backups/metadata-*.dump)"
check "success: so is its .meta" mode_is -rw------- "$(ls upgrade-backups/metadata-*.dump.meta)"
check "success: and the log" mode_is -rw------- "$(ls upgrade-backups/upgrade-*.log)"
check "success: and the folder" mode_is drwx------ upgrade-backups
check "success: no restore" hasnt "$FAKE/events" RESTORE
check "success: says UPGRADED" has "$T/out" "UPGRADED to v2.0.0"
check "success: a log was written" sh -c "ls upgrade-backups/upgrade-*.log"

# 2. Same tag again: nothing to do.
run v2.0.0 --yes
check "same tag: exit 0" [ "$rc" = 0 ]
check "same tag: says so" has "$T/out" "already at v2.0.0"

# 3. install.sh fails after migrating: restore and go back.
setup; echo install_fail > "$FAKE/mode"; run v2.0.0 --yes
check "install fails: exit 2" [ "$rc" = 2 ]
check "install fails: back on v1.0.0" head_is v1.0.0
check "install fails: back on the main branch" [ "$(git symbolic-ref --short HEAD)" = main ]
check "install fails: database restored" has "$FAKE/events" RESTORE-LOAD
check "install fails: schema is 3 again" [ "$(cat "$FAKE/schema")" = 3 ]
check "install fails: v1.0.0 running again" [ "$(cat "$FAKE/version")" = v1.0.0 ]
check "install fails: stopped the new server before restoring" sh -c "grep -B1 RESTORE-DROP '$FAKE/events' | head -1 | grep -q STOP"
check "install fails: says rolled back" has "$T/out" "rolled back"
check "install fails: says what is lost" has "$T/out" "what v2.0.0 wrote after it started is gone"
check "install fails: the old release's install, in full" has "$FAKE/calls" "INSTALL full v1.0.0"

# 4. Comes up unhealthy after migrating: same.
setup; echo unhealthy > "$FAKE/mode"; run v2.0.0 --yes
check "unhealthy: exit 2" [ "$rc" = 2 ]
check "unhealthy: back on v1.0.0" head_is v1.0.0
check "unhealthy: restored" has "$FAKE/events" RESTORE-LOAD
check "unhealthy: names the failing service" has "$T/out" "xenibase-server(restarting)"

# 5. Unhealthy but never migrated: roll back without touching the database.
setup; echo unhealthy_nomigrate > "$FAKE/mode"; run v2.0.0 --yes
check "no migration: exit 2" [ "$rc" = 2 ]
check "no migration: back on v1.0.0" head_is v1.0.0
check "no migration: database left alone" hasnt "$FAKE/events" RESTORE-LOAD
check "no migration: says no restore needed" has "$T/out" "no restore needed"

# 6. --no-rollback stops where it is.
setup; echo unhealthy > "$FAKE/mode"; run v2.0.0 --yes --no-rollback
check "no-rollback: exit 2" [ "$rc" = 2 ]
check "no-rollback: left on v2.0.0" head_is v2.0.0
check "no-rollback: no restore" hasnt "$FAKE/events" RESTORE
check "no-rollback: prints the way back" has "$T/out" "to go back"

# 7. The rollback itself fails.
setup; echo install_fail > "$FAKE/mode"; touch "$FAKE/restore_fails"; run v2.0.0 --yes
check "rollback fails: exit 3" [ "$rc" = 3 ]
check "rollback fails: says so loudly" has "$T/out" "THE ROLLBACK FAILED"
check "rollback fails: names the backup" has "$T/out" "metadata-.*before-v2.0.0.dump"

# 8. Dry run changes nothing.
setup; run v2.0.0 --dry-run
check "dry-run: exit 0" [ "$rc" = 0 ]
check "dry-run: still v1.0.0" head_is v1.0.0
check "dry-run: no backup" sh -c "! ls upgrade-backups/*.dump"
check "dry-run: no dump taken" hasnt "$FAKE/calls" pg_dump
check "dry-run: describes the upgrade" has "$T/out" "2 new migrations"

# 9. Refusals, all before anything changes.
setup; run v9.9.9 --yes
check "unknown tag: exit 1" [ "$rc" = 1 ]
check "unknown tag: says so" has "$T/out" "no such tag"
run 'v1.0.0;rm -rf x' --yes
check "bad tag text: exit 1" [ "$rc" = 1 ]
check "bad tag text: refused" has "$T/out" "not a release tag"
echo dirty >> deploy-note.txt 2>/dev/null || true; echo x >> upgrade.sh; run v2.0.0 --yes
check "local changes: exit 1" [ "$rc" = 1 ]
check "local changes: says so" has "$T/out" "local changes"
git checkout -q -- upgrade.sh
printf 'xenibase-server|restarting||0\n' > "$FAKE/ps"; run v2.0.0 --yes
check "unhealthy start: exit 1" [ "$rc" = 1 ]
check "unhealthy start: says so" has "$T/out" "isn't healthy"
check "unhealthy start: nothing changed" head_is v1.0.0
check "unhealthy start: no backup taken" hasnt "$FAKE/calls" pg_dump
git checkout -q v2.0.0 && git checkout -q -B main v2.0.0; echo 5 > "$FAKE/schema"
printf 'xenibase-server|running|healthy|0\n' > "$FAKE/ps"; echo v2.0.0 > "$FAKE/version"
run v1.0.0 --yes
check "downgrade: exit 1" [ "$rc" = 1 ]
check "downgrade: explains" has "$T/out" "downgrade"

# 10. A refusal to confirm.
setup; set +e; echo n | sh ./upgrade.sh v2.0.0 > "$T/out" 2>&1; rc=$?; set -e
check "declined: exit 1" [ "$rc" = 1 ]
check "declined: nothing changed" head_is v1.0.0

# 11. The forced command lets a deploy key do an upgrade or a status, and nothing else.
setup; cp "$here/../../deploy/compose/deploy-ssh.sh" . && chmod +x deploy-ssh.sh
ssh_run() { set +e; SSH_ORIGINAL_COMMAND="$1" sh ./deploy-ssh.sh > "$T/out" 2>&1; rc=$?; set -e; }
ssh_run "upgrade v2.0.0"
check "ssh upgrade: exit 0" [ "$rc" = 0 ]
check "ssh upgrade: upgraded without asking" head_is v2.0.0
setup; cp "$here/../../deploy/compose/deploy-ssh.sh" . && chmod +x deploy-ssh.sh
ssh_run "upgrade v2.0.0 --dry-run"
check "ssh dry-run: changes nothing" head_is v1.0.0
for bad in "" "bash" "upgrade" "upgrade v2.0.0;id" "upgrade v2.0.0 --force" "upgrade \$(id)" "cat /etc/passwd" "status; id"; do
	ssh_run "$bad"
	check "ssh refuses '$bad'" [ "$rc" = 64 ]
done
ssh_run "status"
check "ssh status: lists services" has "$T/out" "xenibase-server"
ssh_run "last"
check "ssh last: reports the earlier result" has "$T/out" "last upgrade"
# An upgrade through the wrapper that fails: its exit status and log survive.
setup; cp "$here/../../deploy/compose/deploy-ssh.sh" . && chmod +x deploy-ssh.sh; echo unhealthy > "$FAKE/mode"
ssh_run "upgrade v2.0.0"
check "ssh failed upgrade: exit 2 (rolled back)" [ "$rc" = 2 ]
check "ssh failed upgrade: back on v1.0.0" head_is v1.0.0
ssh_run "last"
check "ssh last: says rolled back, exit 2" has "$T/out" "exit status 2"
check "ssh last: shows the log" has "$T/out" "rolled back"

# 12. The first upgrade of an install that doesn't have the script: a copy kept
# elsewhere, pointed at the install with XENIBASE_DIR.
BARE=1 setup; mkdir -p "$T/elsewhere"; cp "$script" "$T/elsewhere/upgrade.sh"; unset BARE
check "external copy: the old release really has no upgrade.sh" test ! -f ./upgrade.sh
set +e; XENIBASE_DIR=$PWD sh "$T/elsewhere/upgrade.sh" v2.0.0 --yes > "$T/out" 2>&1; rc=$?; set -e
check "external copy: exit 0" [ "$rc" = 0 ]
check "external copy: upgraded" head_is v2.0.0
check "external copy: the tag's own upgrade.sh arrived with the checkout" test -f ./upgrade.sh
check "external copy: no private copies left in /tmp" sh -c "! ls ${TMPDIR:-/tmp}/xenibase-upgrade.* 2>/dev/null | grep -q ."

# 13. The build fails: the old version kept serving throughout, so the rollback
# is a checkout and an install, with nothing stopped and nothing restored.
setup; echo build_fail > "$FAKE/mode"; run v2.0.0 --yes
check "build fails: exit 2" [ "$rc" = 2 ]
check "build fails: back on v1.0.0" head_is v1.0.0
check "build fails: the old version was never stopped" hasnt "$FAKE/calls" "compose stop"
check "build fails: no backup was needed" hasnt "$FAKE/calls" pg_dump
check "build fails: no restore" hasnt "$FAKE/events" RESTORE
check "build fails: the old release reinstalled" has "$FAKE/calls" "INSTALL full v1.0.0"
check "build fails: v1.0.0 running" [ "$(cat "$FAKE/version")" = v1.0.0 ]
check "build fails: says nothing was lost" has "$T/out" "nothing written to it was lost"
setup; echo build_fail > "$FAKE/mode"; run v2.0.0 --yes --no-rollback
check "build fails, no-rollback: exit 2" [ "$rc" = 2 ]
check "build fails, no-rollback: says the old version still serves" has "$T/out" "v1.0.0 is still serving"

# 14. The backup fails once the server is stopped: start the old version again.
setup; touch "$FAKE/dump_fails"; run v2.0.0 --yes
check "backup fails: exit 2" [ "$rc" = 2 ]
check "backup fails: back on v1.0.0" head_is v1.0.0
check "backup fails: the new version never started" hasnt "$FAKE/calls" "INSTALL start v2.0.0"
check "backup fails: no restore" hasnt "$FAKE/events" RESTORE
check "backup fails: the old version started again" before pg_dump "INSTALL full v1.0.0"
check "backup fails: schema still 3" [ "$(cat "$FAKE/schema")" = 3 ]
check "backup fails: no half-written dump left" sh -c "! ls upgrade-backups/*.dump"
check "backup fails: says so" has "$T/out" "the backup failed"

# 15. A tag whose install.sh can't build without starting: back up first, as
# before, rather than after a build that would start the new version.
LEGACY=1 setup; unset LEGACY; run v2.0.0 --yes
check "old install.sh: exit 0" [ "$rc" = 0 ]
check "old install.sh: upgraded" head_is v2.0.0
check "old install.sh: says the backup comes first" has "$T/out" "the backup comes first"
check "old install.sh: backup before the install" before pg_dump "INSTALL full v2.0.0"
check "old install.sh: never asked to build alone" hasnt "$FAKE/calls" "build-only"
check "old install.sh: the dump is the owner's only" mode_is -rw------- "$(ls upgrade-backups/metadata-*.dump)"
LEGACY=1 setup; unset LEGACY; echo install_fail > "$FAKE/mode"; run v2.0.0 --yes
check "old install.sh, fails: exit 2" [ "$rc" = 2 ]
check "old install.sh, fails: restored" has "$FAKE/events" RESTORE-LOAD
check "old install.sh, fails: schema is 3 again" [ "$(cat "$FAKE/schema")" = 3 ]

# 16. upgrade-backups made by deploy-ssh.sh (or by hand) under the usual umask
# is made owner-only.
setup; mkdir -m 755 upgrade-backups; run v2.0.0 --yes
check "existing folder: exit 0" [ "$rc" = 0 ]
check "existing folder: now drwx------" mode_is drwx------ upgrade-backups

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
