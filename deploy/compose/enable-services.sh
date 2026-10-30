#!/bin/sh
# Turn on backend services (docs/backend-services.md) on this install: run
# xenibase-edge, and serve every project's API at https://<ref>.<api-domain>
# under one wildcard certificate that Caddy gets and renews with a DNS
# challenge (the only way to get a wildcard).
#
#   cd deploy/compose
#   ./enable-services.sh api.example.com [cloudflare|desec|acmedns]
#
# Before it, put the DNS provider's credentials in .env by hand (this script
# never asks for them and never writes them):
#   cloudflare  CLOUDFLARE_API_TOKEN   a token limited to the zone, "Edit zone DNS"
#   desec       DESEC_TOKEN
#   acmedns     ACMEDNS_USERNAME, ACMEDNS_PASSWORD, ACMEDNS_SUBDOMAIN (and
#               ACMEDNS_SERVER_URL if it isn't https://auth.acme-dns.io): the
#               values acme-dns's /register call returned, for DNS hosts with no
#               API. Add a CNAME there: _acme-challenge.<api-domain> -> its
#               "fulldomain".
# If the domain's DNS can't be on the provider (cloudflare, desec), point only
# the challenge at a zone that is: add a CNAME
#   _acme-challenge.<api-domain>  ->  _acme-challenge.<zone you control there>
# and set ACME_CHALLENGE_DOMAIN=<that zone name> in .env.
# DNS also needs `*.<api-domain>` as an A record to this server.
# Safe to run again. Full method: docs/enabling-backend-services.md
set -eu
cd "$(dirname "$0")"

# --config-only writes caddy.d/api.caddy and stops: no build, no containers (tests).
config_only=0
if [ "${1:-}" = "--config-only" ]; then config_only=1; shift; fi

die() { echo "enable-services: $*" >&2; exit 1; }
rand_url() { head -c "$1" /dev/urandom | base64 | tr -d '\n' | tr '+/' '-_' | tr -d '='; }
env_get() { grep "^$1=" .env | head -1 | cut -d= -f2- || true; }

[ -f .env ] || die "no .env here: run ./install.sh first"
api=${1:-$(env_get XENIBASE_API_DOMAIN)}
[ -n "$api" ] || die "usage: ./enable-services.sh <api-domain> [cloudflare|desec|acmedns]"
case "$api" in
*[!A-Za-z0-9.-]* | .* | *. | *..*) die "invalid domain: '$api'" ;;
esac
ui=$(env_get XENIBASE_UI_DOMAIN)
[ "$api" != "$ui" ] || die "the API domain can't be the web UI's name ($ui): projects are at <ref>.$api"

provider=${2:-$(env_get DNS_PROVIDER)}
provider=${provider:-cloudflare}
# Each plugin is pinned to a version whose go.mod needs no newer Caddy than
# caddy-dns/Dockerfile's (2.10.0), so the build doesn't move Caddy;
# test/acmedns checks what acmedns v0.7.0 does with the config below.
case "$provider" in
cloudflare) plugin=github.com/caddy-dns/cloudflare@v0.2.4 tokenvar=CLOUDFLARE_API_TOKEN ;;
desec) plugin=github.com/caddy-dns/desec@v1.0.1 tokenvar=DESEC_TOKEN ;;
acmedns) plugin=github.com/caddy-dns/acmedns@v0.7.0 tokenvar=ACMEDNS_USERNAME ;;
*) die "unknown DNS provider '$provider': use cloudflare, desec or acmedns" ;;
esac
if [ "$provider" = acmedns ]; then
	for v in ACMEDNS_USERNAME ACMEDNS_PASSWORD ACMEDNS_SUBDOMAIN; do
		[ -n "$(env_get "$v")" ] || die "set $v in .env first (from acme-dns's /register call)"
	done
else
	[ -n "$(env_get "$tokenvar")" ] || die "set $tokenvar in .env first (the $provider API token, limited to the zone)"
fi

ad_user=$(env_get ACMEDNS_USERNAME)
ad_sub=$(env_get ACMEDNS_SUBDOMAIN)
ad_url=$(env_get ACMEDNS_SERVER_URL)
ad_url=${ad_url:-https://auth.acme-dns.io}
if [ "$provider" = acmedns ]; then
	# These go into a Caddyfile as they are: no spaces, quotes or braces.
	for v in "$ad_user" "$ad_sub" "$ad_url"; do
		case "$v" in
		"" | *[!A-Za-z0-9._:/-]*) die "an acme-dns username, subdomain or server URL has a character that can't go in the Caddyfile: '$v'" ;;
		esac
	done
fi

challenge=$(env_get ACME_CHALLENGE_DOMAIN)
case "$challenge" in
*[!A-Za-z0-9.-]*) die "invalid ACME_CHALLENGE_DOMAIN: '$challenge'" ;;
esac

# Settings: add what's missing, keep what the operator set.
set_env() { grep -q "^$1=" .env || echo "$1=$2" >> .env; }
set_env XENIBASE_API_DOMAIN "$api"
set_env XENIBASE_EDGE_SECRET "$(rand_url 40)"
set_env DNS_PROVIDER "$provider"
[ "$provider" != acmedns ] || set_env ACMEDNS_SERVER_URL https://auth.acme-dns.io
set_env CADDY_IMAGE "xenibase-caddy-$provider:local"
set_env COMPOSE_PROFILES services
secret=$(env_get XENIBASE_EDGE_SECRET)
[ "${#secret}" -ge 32 ] || die "XENIBASE_EDGE_SECRET in .env must be at least 32 characters"

if [ "$config_only" = 0 ]; then
	echo "Building Caddy with the $provider DNS plugin..."
	docker build -q --build-arg "DNS_PLUGIN=$plugin" -t "$(env_get CADDY_IMAGE)" caddy-dns >/dev/null
fi

# The wildcard site: Caddy terminates TLS and hands plain HTTP to the edge.
{
	echo "# Written by enable-services.sh: backend services' API domain."
	echo '*.{$XENIBASE_API_DOMAIN} {'
	echo '	tls {'
	# Each plugin has its own Caddyfile syntax.
	case "$provider" in
	desec)
		echo "		dns desec {"
		echo "			token {env.$tokenvar}"
		echo "		}"
		;;
	acmedns)
		# The plugin expands {env.X} only in the password: the username, the
		# subdomain and the server URL are used exactly as written, so they are
		# written out here (none of them is secret; the password stays in .env).
		echo "		dns acmedns {"
		echo "			username $ad_user"
		echo "			password {env.ACMEDNS_PASSWORD}"
		echo "			subdomain $ad_sub"
		echo "			server_url $ad_url"
		echo "		}"
		;;
	*) echo "		dns $provider {env.$tokenvar}" ;;
	esac
	[ -z "$challenge" ] || echo "		dns_challenge_override_domain $challenge"
	if [ "$provider" = acmedns ]; then
		# acme-dns serves the record the moment it is updated, and Let's Encrypt
		# checks it itself. Caddy's own check asks acme-dns's nameserver over UDP
		# from this server, which can time out on some networks (it did on the
		# first real server) and fail a certificate that is fine.
		echo "		propagation_timeout -1"
	fi
	echo '	}'
	echo '	# No compression or buffering: realtime and large downloads stream.'
	echo '	reverse_proxy xenibase-edge:8443 {'
	echo '		flush_interval -1'
	echo '	}'
	echo '}'
} > caddy.d/api.caddy
[ "$config_only" = 0 ] || exit 0

echo "Starting xenibase-edge and reloading Caddy..."
docker compose up -d --build
# A changed caddy.d/api.caddy doesn't recreate the container: make Caddy read it
# (and try the certificate again now, not at the end of its back-off).
if docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile --force >/dev/null 2>&1; then
	echo "Caddy reloaded."
else
	docker compose restart caddy >/dev/null
	echo "Caddy restarted."
fi
echo
echo "Backend services are on for *.$api. The first certificate takes a minute."
echo "Check:   docker compose logs caddy | grep -i 'certificate obtained'"
echo "Then:    Project -> Settings -> API -> Enable backend services"
