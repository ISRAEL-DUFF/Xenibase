#!/bin/sh
# Tests deploy/compose/install.sh without Docker: a fake `docker` that records
# its calls, a stub Postgres image build, and an existing .env. What it checks:
# --build-only builds and starts nothing, --no-build starts and builds nothing,
# an .env open to other users is set to mode 600, and co-hosting
# (docs/cohosting.md): every compose call reads compose.cohost.yaml, the
# shared network is created once, Caddy is recreated only for a new
# Caddyfile, and another app's files in caddy.d are left alone.
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
	# Like docker compose, the fake reads COMPOSE_FILE from the environment or
	# .env, records the files each compose call would read, and fails when one
	# is missing. The networks it "has" are the lines of $NETS.
	cat > "$work/b/bin/docker" <<'D'
#!/bin/sh
echo "docker $*" >> "$CALLS"
if [ "$1" = compose ]; then
	files=${COMPOSE_FILE:-$(grep '^COMPOSE_FILE=' .env 2>/dev/null | tail -1 | cut -d= -f2-)}
	files=${files:-compose.yaml}
	echo "$files :: $*" >> "$CALLS.files"
	IFS=:
	for f in $files; do [ -f "$f" ] || { echo "open $f: no such file" >&2; exit 14; }; done
	unset IFS
fi
case "$*" in
"compose logs"*) echo 'level=INFO msg=ready setup_code=abc123' ;;
"compose exec -T caddy cat /etc/caddy/Caddyfile") [ ! -f "$CADDY_SEES" ] || cat "$CADDY_SEES" ;;
"network inspect "*) grep -qx "$3" "$NETS" || exit 1 ;;
"network create "*) echo "$3" >> "$NETS" ;;
esac
exit 0
D
	chmod +x "$work/b/bin/docker"
	printf 'XENIBASE_UI_DOMAIN=app.example.test\nXENIBASE_ACME_EMAIL=a@example.test\nXENIBASE_MASTER_KEY=k\n' > "$work/b/deploy/compose/.env"
	chmod 644 "$work/b/deploy/compose/.env"
	for f in compose.yaml compose.cohost.yaml Caddyfile; do cp "$here/../../deploy/compose/$f" "$work/b/deploy/compose/"; done
	export CALLS="$work/b/calls" NETS="$work/b/networks" CADDY_SEES="$work/b/caddy-sees" PATH="$work/b/bin:$PATH"
	: > "$CALLS"; : > "$CALLS.files"; : > "$NETS"; rm -f "$CADDY_SEES"
	cd "$work/b/deploy/compose"
}
# line_no <file> <pattern>: the line of the first match.
line_no() { grep -n -- "$2" "$1" | head -1 | cut -d: -f1; }
# only_files <files>: every compose call read exactly these compose files.
only_files() { ! grep -v "^$1 :: " "$CALLS.files" | grep -q . && grep -q . "$CALLS.files"; }
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

# --- Co-hosting off: no network is touched, and Caddy is recreated only when
# the Caddyfile it reads isn't this one (a checkout leaves it the old inode).
fresh; run --no-build
check "no co-hosting: no network calls" hasnt "$CALLS" "docker network"
check "no co-hosting: compose reads compose.yaml alone" only_files compose.yaml
check "stale Caddyfile: Caddy is recreated" has "$CALLS" "force-recreate --no-deps caddy"
fresh; cp Caddyfile "$CADDY_SEES"; run --no-build
check "current Caddyfile: Caddy is left running" hasnt "$CALLS" "force-recreate --no-deps caddy"

# --- Co-hosting on, in .env, with another app's files in caddy.d.
cohost() { echo 'COMPOSE_FILE=compose.yaml:compose.cohost.yaml' >> .env; }
fresh; cohost
mkdir -p caddy.d
printf 'app.taskiem.test {\n\treverse_proxy taskiem:8080\n}\n' > caddy.d/taskiem.caddy
printf 'on_demand_tls {\n\task http://taskiem-public:9090/tls/ask\n}\n' > caddy.d/taskiem.global
cp caddy.d/taskiem.caddy "$work/taskiem.caddy"; cp caddy.d/taskiem.global "$work/taskiem.global"
run
check "co-hosting: exit 0" [ "$rc" = 0 ]
check "co-hosting: creates the network" has "$CALLS" "docker network create cohost"
check "co-hosting: says so" has "$work/out" "Created the co-hosting network 'cohost'"
check "co-hosting: before the stack starts" [ "$(line_no "$CALLS" 'network create')" -lt "$(line_no "$CALLS" 'compose up')" ]
check "co-hosting: every compose call reads compose.cohost.yaml" only_files compose.yaml:compose.cohost.yaml
check "co-hosting: no compose call names its own files" sh -c "! grep 'docker compose' '$CALLS' | grep -q -e ' -f ' -e ' --file'"
: > "$CALLS"; run
check "co-hosting again: exit 0" [ "$rc" = 0 ]
check "co-hosting again: the network isn't created twice" hasnt "$CALLS" "network create"
check "co-hosting again: one network" [ "$(wc -l < "$NETS" | tr -d ' ')" = 1 ]
check "co-hosting: taskiem.caddy is untouched" cmp caddy.d/taskiem.caddy "$work/taskiem.caddy"
check "co-hosting: taskiem.global is untouched" cmp caddy.d/taskiem.global "$work/taskiem.global"

# The other app's installer made the network first.
fresh; cohost; echo cohost > "$NETS"; run --no-build
check "network exists: exit 0" [ "$rc" = 0 ]
check "network exists: not created again" hasnt "$CALLS" "network create"

# A network of another name, and COMPOSE_FILE from the environment.
fresh; echo 'XENIBASE_COHOST_NETWORK=shared-web' >> .env
set +e; COMPOSE_FILE=compose.yaml:compose.cohost.yaml sh ./install.sh --no-build > "$work/out" 2>&1; rc=$?; set -e
check "env COMPOSE_FILE: exit 0" [ "$rc" = 0 ]
check "env COMPOSE_FILE: creates the named network" has "$CALLS" "docker network create shared-web"
check "env COMPOSE_FILE: every compose call reads it" only_files compose.yaml:compose.cohost.yaml
fresh; echo 'XENIBASE_COHOST_NETWORK=bad;name' >> .env; cohost; run --no-build
check "a bad network name: refused" [ "$rc" = 1 ]
check "a bad network name: nothing started" hasnt "$CALLS" "compose up"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
