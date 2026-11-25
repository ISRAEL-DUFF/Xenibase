#!/bin/sh
# Tests deploy/compose/preflight.sh's memory check without touching the
# network: fakes stand in for curl, dig, docker and the other tools it asks,
# and PREFLIGHT_MEMINFO for /proc/meminfo. The memory budget is a WARN when
# the configured services (co-hosting among them) need more than the server
# has, never a FAIL.
#
#   make test-deploy
set -eu
here=$(cd "$(dirname "$0")" && pwd)
script=$here/../../deploy/compose/preflight.sh
pass=0 fail=0
check() { # check <name> <condition command...>
	name=$1; shift
	if "$@" >/dev/null 2>&1; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "  FAIL: $name"; [ -z "${DEBUG:-}" ] || sed 's/^/        | /' "$work/out"; fi
}
has() { grep -q -- "$2" "$1"; }
hasnt() { ! grep -q -- "$2" "$1"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/c"
# Every tool preflight asks about answers at once and finds nothing.
for t in curl dig getent host ss nc docker timedatectl ufw sudo; do
	printf '#!/bin/sh\nexit 1\n' > "$work/bin/$t"
	chmod +x "$work/bin/$t"
done
cp "$script" "$work/c/preflight.sh"
cd "$work/c"

# run <MemTotal in GB> [.env lines...]: preflight's output in $work/out.
run() {
	printf 'MemTotal:       %s kB\nSwapTotal:      0 kB\n' "$(($1 * 1024 * 1024))" > "$work/meminfo"
	shift
	if [ "$#" -gt 0 ]; then printf '%s\n' "$@" > .env; else rm -f .env; fi
	set +e
	PREFLIGHT_MEMINFO=$work/meminfo PATH="$work/bin:$PATH" sh ./preflight.sh ui.example.test db.example.test > "$work/out" 2>&1
	set -e
}

echo "preflight.sh (memory)"

run 8
check "8 GB, defaults: passes" sh -c "grep -q 'PASS  memory: the configured services need about 4096 MB' '$work/out'"

run 3
check "3 GB, defaults: warns" sh -c "grep -q 'WARN  memory: the configured services need about 4096 MB' '$work/out'"
check "3 GB, defaults: names the settings" has "$work/out" "SHARED_PG_SHARED_BUFFERS"
check "3 GB, defaults: effective_cache_size beyond RAM" has "$work/out" "SHARED_PG_EFFECTIVE_CACHE_SIZE (6GB) is more than"
check "3 GB: the memory check never FAILs" sh -c "! grep -q 'FAIL  memory' '$work/out'"
run 4 SHARED_PG_SHARED_BUFFERS=768MB SHARED_PG_EFFECTIVE_CACHE_SIZE=2GB
check "4 GB, small profile: passes" sh -c "grep -q 'PASS  memory: the configured services need about 2816 MB' '$work/out'"
check "4 GB, small profile: cache size fits" hasnt "$work/out" "is more than this server"

# Co-hosting sets aside 8 GB for the other app unless told otherwise.
run 11 COMPOSE_FILE=compose.yaml:compose.cohost.yaml
check "11 GB co-hosting, defaults: warns" sh -c "grep -q 'WARN  memory: the configured services need about 12288 MB' '$work/out'"
check "11 GB co-hosting: says what it counted" has "$work/out" "8192 MB for the co-hosted app"
run 12 COMPOSE_FILE=compose.yaml:compose.cohost.yaml SHARED_PG_SHARED_BUFFERS=1GB SHARED_PG_EFFECTIVE_CACHE_SIZE=2GB XENIBASE_COHOST_RESERVE_MB=6144
check "12 GB co-hosting, the 12 GB profile: passes" sh -c "grep -q 'PASS  memory: the configured services need about 9216 MB' '$work/out'"
run 32 COMPOSE_FILE=compose.yaml:compose.cohost.yaml SHARED_PG_SHARED_BUFFERS=4GB SHARED_PG_EFFECTIVE_CACHE_SIZE=12GB COMPOSE_PROFILES=services
check "32 GB co-hosting with services, the VDS M profile: passes" sh -c "grep -q 'PASS  memory: the configured services need about 15360 MB' '$work/out'"
check "32 GB: counts the edge" has "$work/out" "the edge 1g"
run 15 COMPOSE_FILE=compose.yaml:compose.cohost.yaml SHARED_PG_SHARED_BUFFERS=4GB SHARED_PG_EFFECTIVE_CACHE_SIZE=12GB COMPOSE_PROFILES=services XENIBASE_EDGE_MEMORY=2g
check "15 GB, the VDS M profile: warns" sh -c "grep -q 'WARN  memory: the configured services need about 16384 MB' '$work/out'"

# The environment wins over .env, as for docker compose.
printf 'MemTotal:       %s kB\n' "$((8 * 1024 * 1024))" > "$work/meminfo"
printf 'SHARED_PG_SHARED_BUFFERS=6GB\n' > .env
set +e; SHARED_PG_SHARED_BUFFERS=1GB PREFLIGHT_MEMINFO=$work/meminfo PATH="$work/bin:$PATH" sh ./preflight.sh ui.example.test db.example.test > "$work/out" 2>&1; set -e
check "the environment wins over .env" sh -c "grep -q 'need about 3072 MB' '$work/out'"
# Postgres's bare numbers are 8 kB pages.
run 8 SHARED_PG_SHARED_BUFFERS=131072
check "a bare number is 8 kB pages" sh -c "grep -q 'need about 3072 MB' '$work/out'"
check "one memory line per run" sh -c "[ \$(grep -c 'memory: the configured' '$work/out') = 1 ]"

echo "preflight: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
