#!/bin/sh
# Tests deploy/compose/enable-services.sh without Docker (--config-only): the
# Caddy file it writes for each DNS provider, what it refuses, and that the
# compose file's edge settings are the kind the edge accepts.
#
#   make test-deploy
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$here/../..
pass=0 fail=0
check() { # check <name> <condition command...>
	name=$1; shift
	if "$@" >/dev/null 2>&1; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "  FAIL: $name"; fi
}
has() { grep -qF -- "$2" "$1"; }
hasnt() { ! grep -qF -- "$2" "$1"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fresh() { # fresh <env lines...>: a copy of the bundle with a new .env
	rm -rf "$work/c" && mkdir -p "$work/c" && cp -R "$repo/deploy/compose/." "$work/c/" && rm -f "$work/c/caddy.d/"*.caddy
	cd "$work/c"
	printf '%s\n' "XENIBASE_UI_DOMAIN=app.example.test" "$@" > .env
}

# --- acme-dns: the plugin expands {env.X} in the password only, so the other
# three values must be written out; the password must not be.
fresh ACMEDNS_USERNAME=user-1234 ACMEDNS_PASSWORD='s3cret/pass' ACMEDNS_SUBDOMAIN=9f486b61-aecc-47ac-b6d2-b6999c2f1e44
sh ./enable-services.sh --config-only api.example.test acmedns >/dev/null
f=caddy.d/api.caddy
check "acmedns: the username is written out" has $f "username user-1234"
check "acmedns: the subdomain is written out" has $f "subdomain 9f486b61-aecc-47ac-b6d2-b6999c2f1e44"
check "acmedns: the server URL is written out (the default)" has $f "server_url https://auth.acme-dns.io"
check "acmedns: the password is read from the environment" has $f "password {env.ACMEDNS_PASSWORD}"
check "acmedns: the password itself isn't in the file" hasnt $f "s3cret"
check "acmedns: no {env.} for the username" hasnt $f "{env.ACMEDNS_USERNAME}"
check "acmedns: no {env.} for the subdomain" hasnt $f "{env.ACMEDNS_SUBDOMAIN}"
check "acmedns: no {env.} for the server URL" hasnt $f "{env.ACMEDNS_SERVER_URL}"
check "acmedns: Caddy doesn't wait on its own check of acme-dns's nameserver" has $f "propagation_timeout -1"
check "acmedns: the site proxies to the edge" has $f "reverse_proxy xenibase-edge:8443"
check "acmedns: the edge secret is 32+ characters" sh -c '[ "$(grep ^XENIBASE_EDGE_SECRET= .env | cut -d= -f2- | wc -c)" -ge 33 ]'
check "acmedns: the API domain is in .env" has .env "XENIBASE_API_DOMAIN=api.example.test"

# A server URL of your own is used as given.
fresh ACMEDNS_USERNAME=u ACMEDNS_PASSWORD=p ACMEDNS_SUBDOMAIN=s ACMEDNS_SERVER_URL=https://acme.example.org
sh ./enable-services.sh --config-only api.example.test acmedns >/dev/null
check "acmedns: a custom server URL" has caddy.d/api.caddy "server_url https://acme.example.org"

# Values that would break the Caddyfile are refused, and nothing is written.
fresh ACMEDNS_USERNAME='u }' ACMEDNS_PASSWORD=p ACMEDNS_SUBDOMAIN=s
check "acmedns: a username with a brace is refused" sh -c '! sh ./enable-services.sh --config-only api.example.test acmedns'
check "acmedns: nothing was written for it" sh -c '! [ -e caddy.d/api.caddy ]'
fresh ACMEDNS_USERNAME=u ACMEDNS_PASSWORD=p
check "acmedns: a missing subdomain is refused" sh -c '! sh ./enable-services.sh --config-only api.example.test acmedns'

# --- the other providers keep their token in the environment.
fresh CLOUDFLARE_API_TOKEN=abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN
sh ./enable-services.sh --config-only api.example.test cloudflare >/dev/null
check "cloudflare: the token is read from the environment" has caddy.d/api.caddy "dns cloudflare {env.CLOUDFLARE_API_TOKEN}"
check "cloudflare: Caddy still checks propagation itself" hasnt caddy.d/api.caddy "propagation_timeout"
fresh DESEC_TOKEN=abcdefghijklmnopqrstuvwxyz12 ACME_CHALLENGE_DOMAIN=acme.example.org
sh ./enable-services.sh --config-only api.example.test desec >/dev/null
check "desec: the token block" has caddy.d/api.caddy "token {env.DESEC_TOKEN}"
check "desec: the challenge is delegated" has caddy.d/api.caddy "dns_challenge_override_domain acme.example.org"

# --- the script refuses what it should.
fresh
check "no provider credentials: refused" sh -c '! sh ./enable-services.sh --config-only api.example.test cloudflare'
fresh CLOUDFLARE_API_TOKEN=x
check "the API domain can't be the UI's name" sh -c '! sh ./enable-services.sh --config-only app.example.test cloudflare'
check "an unknown provider is refused" sh -c '! sh ./enable-services.sh --config-only api.example.test route53'

# --- the edge takes its trusted proxies as network ranges (a bare address makes
# it exit at startup, which is what a bare address in the compose file did).
cd "$repo"
bad=$(grep -E 'XENIBASE_EDGE_TRUSTED_PROXIES:' deploy/compose/compose.yaml | grep -vE ':[[:space:]]+[0-9a-fA-F:.]+/[0-9]+[[:space:]]*$' || true)
check "compose: every XENIBASE_EDGE_TRUSTED_PROXIES is a CIDR range ($bad)" sh -c '[ -z "$1" ]' _ "$bad"

# --- install.sh (so upgrade.sh) rebuilds the edge when services are on; its image
# is local, and without this an upgrade keeps running the old edge.
cd "$repo"
inst=$(mktemp -d); mkdir -p "$inst/bin" "$inst/deploy/compose"
cp deploy/compose/install.sh "$inst/deploy/compose/"
mkdir -p "$inst/deploy/images/postgres"
cp deploy/images/postgres/build.sh "$inst/deploy/images/postgres/"
# build.sh reads the release an image holds back from its PG_VERSION.
cat > "$inst/bin/docker" <<'D'
#!/bin/sh
echo "docker $*" >> "$CALLS"
if [ "$1 $2" = "image inspect" ]; then echo "PG_VERSION=18.6-1.pgdg13+1"; fi
D
chmod +x "$inst/bin/docker"
run_install() { # run_install <.env contents>
	printf '%s\n' "$1" > "$inst/deploy/compose/.env"; : > "$inst/calls"
	(cd "$inst/deploy/compose" && CALLS="$inst/calls" PATH="$inst/bin:$PATH" sh ./install.sh >/dev/null 2>&1)
}
run_install 'XENIBASE_UI_DOMAIN=app.example.test
COMPOSE_PROFILES=services'
check "install: builds the edge image when services are on" grep -q 'build.*-t xenibase-edge:local' "$inst/calls"
check "install: builds and tags the instance image by its release" grep -q 'tag xenibase-postgres:18-walg3.0.9 xenibase-postgres:18.6-walg3.0.9' "$inst/calls"
run_install 'XENIBASE_UI_DOMAIN=app.example.test'
check "install: leaves the edge alone when services are off" sh -c '! grep -q xenibase-edge "$1"' _ "$inst/calls"

echo "enable-services: $pass passed, $fail failed"
[ "$fail" = 0 ]
