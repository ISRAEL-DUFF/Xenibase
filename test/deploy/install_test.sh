#!/bin/sh
# Tests deploy/compose/install.sh without Docker: a fake `docker` that records
# its calls, a stub Postgres image build, and an existing .env. What it checks:
# --build-only builds and starts nothing, --no-build starts and builds nothing,
# and an .env open to other users is set to mode 600.
#
#   make test-deploy
set -eu
here=$(cd "$(dirname "$0")" && pwd)
script=$here/../../deploy/compose/install.sh
pass=0 fail=0
check() { # check <name> <condition command...>
	name=$1; shift
	if "$@" >/dev/null 2>&1; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "  FAIL: $name"; fi
}
has() { grep -q -- "$2" "$1"; }
hasnt() { ! grep -q -- "$2" "$1"; }
mode_is() { [ "$(ls -ld "$2" | cut -c1-10)" = "$1" ]; }
umask 022

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# fresh makes a bundle with install.sh, a stub image build, a fake docker and
# a hand-made .env of mode 644 (as `cp .env.example .env` leaves it).
fresh() {
	rm -rf "$work/b" && mkdir -p "$work/b/deploy/compose" "$work/b/deploy/images/postgres" "$work/b/bin"
	cp "$script" "$work/b/deploy/compose/install.sh"
	echo 'echo "pg-image $1" >> "$CALLS"' > "$work/b/deploy/images/postgres/build.sh"
	cat > "$work/b/bin/docker" <<'D'
#!/bin/sh
echo "docker $*" >> "$CALLS"
case "$*" in
"compose logs"*) echo 'level=INFO msg=ready setup_code=abc123' ;;
esac
exit 0
D
	chmod +x "$work/b/bin/docker"
	printf 'XENIBASE_UI_DOMAIN=app.example.test\nXENIBASE_ACME_EMAIL=a@example.test\nXENIBASE_MASTER_KEY=k\n' > "$work/b/deploy/compose/.env"
	chmod 644 "$work/b/deploy/compose/.env"
	export CALLS="$work/b/calls" PATH="$work/b/bin:$PATH"
	: > "$CALLS"
	cd "$work/b/deploy/compose"
}
run() { set +e; sh ./install.sh "$@" > "$work/out" 2>&1; rc=$?; set -e; }

echo "install.sh"

fresh; run
check "full: exit 0" [ "$rc" = 0 ]
check "full: builds the Postgres images" has "$CALLS" "pg-image 18"
check "full: builds the server" has "$CALLS" "docker build .*-t xenibase:local"
check "full: starts the stack" has "$CALLS" "docker compose up -d --no-build --wait"
check "full: prints the setup code" has "$work/out" "setup code: abc123"
check "full: a 644 .env is now 600" mode_is -rw------- .env
check "full: and it says so" has "$work/out" "warning: .env was open to other users"
run
check "again: a 600 .env draws no warning" hasnt "$work/out" "warning"

fresh; run --build-only
check "build-only: exit 0" [ "$rc" = 0 ]
check "build-only: builds the server and the agent" sh -c "grep -c 'docker build' '$CALLS' | grep -qx 2"
check "build-only: starts nothing" hasnt "$CALLS" "compose up"
check "build-only: says the running version serves" has "$work/out" "Nothing was restarted"
check "build-only: still tightens .env" mode_is -rw------- .env

fresh; run --no-build
check "no-build: exit 0" [ "$rc" = 0 ]
check "no-build: builds nothing" sh -c "! grep -q -e 'docker build' -e pg-image '$CALLS'"
check "no-build: starts the stack" has "$CALLS" "docker compose up -d --no-build --wait"
check "no-build: recreates the poolers" has "$CALLS" "force-recreate --no-deps pooler-session pooler-tx"

fresh; run --build-only --no-build
check "both: refused" [ "$rc" = 64 ]
check "both: nothing ran" hasnt "$CALLS" "docker build"
run --bogus
check "unknown option: refused" [ "$rc" = 64 ]

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
