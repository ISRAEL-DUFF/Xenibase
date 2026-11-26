#!/bin/sh
# Check a server before installing Xenibase on it (docs/install.md).
#
#   cd deploy/compose
#   ./preflight.sh xenibase.example.com db.example.com
#
# The two names are the web UI's hostname and the database hostname your apps
# will use; both must already point at this server. Optional environment:
#
#   XENIBASE_S3_ENDPOINT  the backup bucket's endpoint URL, to check it is reachable
#   XENIBASE_SMTP_HOST    your mail server, with XENIBASE_SMTP_PORT (default 587)
#
# The memory check reads the settings install.sh will use, from the
# environment or an .env here: SHARED_PG_SHARED_BUFFERS and
# SHARED_PG_EFFECTIVE_CACHE_SIZE, COMPOSE_PROFILES (the edge's
# XENIBASE_EDGE_MEMORY), and co-hosting (COMPOSE_FILE with
# compose.cohost.yaml), for which it sets aside XENIBASE_COHOST_RESERVE_MB
# (default 8192) for the other app (docs/cohosting.md).
#
# It changes nothing. Each line is PASS, WARN (works, but look at it) or FAIL
# (the install will not work until this is fixed); the exit status is 1 if
# anything FAILed.
set -u

ui=${1:-${XENIBASE_UI_DOMAIN:-}}
db=${2:-${XENIBASE_DB_DOMAIN:-}}
if [ -z "$ui" ] || [ -z "$db" ]; then
	echo "usage: $0 <ui-hostname> <database-hostname>" >&2
	echo "e.g.   $0 xenibase.example.com db.example.com" >&2
	exit 2
fi

fails=0
warns=0
pass() { printf '  PASS  %s\n' "$1"; }
warn() { printf '  WARN  %s\n' "$1"; warns=$((warns + 1)); }
fail() { printf '  FAIL  %s\n' "$1"; fails=$((fails + 1)); }
note() { printf '        %s\n' "$1"; }
section() { printf '\n%s\n' "$1"; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- The machine -----------------------------------------------------------
section "Server"
if [ "$(uname -s)" != "Linux" ]; then
	fail "this is $(uname -s); Xenibase's install needs Linux (Ubuntu 24.04 or Debian 12)"
else
	os=unknown
	id=
	ver=
	if [ -r /etc/os-release ]; then
		os=$(. /etc/os-release && echo "${PRETTY_NAME:-unknown}")
		id=$(. /etc/os-release && echo "${ID:-}")
		ver=$(. /etc/os-release && echo "${VERSION_ID:-0}")
	fi
	major=${ver%%.*}
	case "$id" in
	ubuntu) if [ "${major:-0}" -ge 24 ] 2>/dev/null; then pass "$os"; else warn "$os: tested on Ubuntu 24.04; older releases may not have a current Docker"; fi ;;
	debian) if [ "${major:-0}" -ge 12 ] 2>/dev/null; then pass "$os"; else warn "$os: tested on Debian 12"; fi ;;
	"") warn "can't tell which Linux this is" ;;
	*) warn "$os: tested on Ubuntu 24.04 and Debian 12; other distributions with Docker usually work" ;;
	esac
fi
case "$(uname -m)" in
x86_64 | amd64 | aarch64 | arm64) pass "architecture $(uname -m)" ;;
*) fail "architecture $(uname -m): only amd64 and arm64 are built" ;;
esac
if have nproc; then
	cpus=$(nproc)
	if [ "$cpus" -ge 2 ]; then pass "$cpus CPUs"; else warn "$cpus CPU: 2 or more recommended (builds the images, runs Postgres, PgBouncer and the control plane)"; fi
fi
meminfo=${PREFLIGHT_MEMINFO:-/proc/meminfo} # a test can point it elsewhere
mem_mb=
if [ -r "$meminfo" ]; then
	mem_mb=$(awk '/^MemTotal:/ {print int($2/1024)}' "$meminfo")
	swap_mb=$(awk '/^SwapTotal:/ {print int($2/1024)}' "$meminfo")
	if [ "$mem_mb" -ge 7000 ]; then
		pass "${mem_mb} MB RAM"
	elif [ "$mem_mb" -ge 3500 ]; then
		warn "${mem_mb} MB RAM: enough to run; 8 GB matches the shared cluster's default tuning"
	elif [ "$mem_mb" -ge 1800 ] && [ "$swap_mb" -ge 1000 ]; then
		warn "${mem_mb} MB RAM with ${swap_mb} MB swap: building the images will be slow, and Postgres tight"
	else
		fail "${mem_mb} MB RAM: at least 4 GB is needed (the image build alone can use 2 GB)"
	fi
fi

# setting <NAME>: from the environment, else from .env here (quotes dropped).
setting() {
	eval "v=\${$1:-}"
	if [ -z "$v" ] && [ -f .env ]; then
		v=$(grep "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- | tr -d "\"'")
	fi
	printf '%s' "$v"
}
# to_mb <postgres size>: 2GB, 768MB, 524288kB, or a bare number of 8 kB pages.
to_mb() {
	printf '%s' "$1" | awk '{
		n = $0 + 0; u = $0; sub(/^[0-9.]+ */, "", u); u = tolower(u)
		if (u == "tb") n *= 1048576; else if (u == "gb") n *= 1024; else if (u == "kb") n /= 1024
		else if (u == "") n = n * 8 / 1024; else if (u != "mb") { print ""; exit }
		print int(n) }'
}
# What the configured services take, roughly: the shared cluster's buffers
# plus 2 GB for its connections, the metadata database, xenibase-server, the
# agent, the poolers, Caddy and the OS; the edge's limit when backend services
# are on; and what a co-hosted app needs. Advisory only: it never FAILs.
if [ -n "$mem_mb" ]; then
	sb=$(setting SHARED_PG_SHARED_BUFFERS); sb_mb=$(to_mb "${sb:-2GB}")
	ec=$(setting SHARED_PG_EFFECTIVE_CACHE_SIZE); ec_mb=$(to_mb "${ec:-6GB}")
	need=$(( ${sb_mb:-2048} + 2048 ))
	parts="shared_buffers ${sb:-2GB} + 2 GB for the rest of Xenibase"
	case ",$(setting COMPOSE_PROFILES)," in
	*,services,*)
		em=$(setting XENIBASE_EDGE_MEMORY); em_mb=$(to_mb "$(printf '%s' "${em:-1g}" | sed 's/[gG]$/GB/; s/[mM]$/MB/')")
		need=$((need + ${em_mb:-1024})); parts="$parts + the edge ${em:-1g}" ;;
	esac
	case ":$(setting COMPOSE_FILE):" in
	*:compose.cohost.yaml:* | *:./compose.cohost.yaml:*)
		rs=$(setting XENIBASE_COHOST_RESERVE_MB); rs=${rs:-8192}
		case "$rs" in *[!0-9]* | "") rs=8192 ;; esac
		need=$((need + rs)); parts="$parts + ${rs} MB for the co-hosted app (XENIBASE_COHOST_RESERVE_MB)" ;;
	esac
	if [ "$mem_mb" -ge "$need" ]; then
		pass "memory: the configured services need about ${need} MB (${parts}); ${mem_mb} MB here"
	else
		warn "memory: the configured services need about ${need} MB (${parts}) but this server has ${mem_mb} MB: lower SHARED_PG_SHARED_BUFFERS and SHARED_PG_EFFECTIVE_CACHE_SIZE in .env (docs/cohosting.md has profiles)"
	fi
	if [ -n "$ec_mb" ] && [ "$ec_mb" -gt "$mem_mb" ]; then
		warn "memory: SHARED_PG_EFFECTIVE_CACHE_SIZE (${ec:-6GB}) is more than this server's RAM: set it to what the shared cluster can really expect to have cached"
	fi
	note "dedicated instances need their own size on top (1 GB for the smallest)"
fi
free_gb=$(df -Pk "${PWD}" 2>/dev/null | awk 'NR==2 {print int($4/1024/1024)}')
if [ -n "${free_gb:-}" ]; then
	if [ "$free_gb" -ge 30 ]; then
		pass "${free_gb} GB free on this disk"
	elif [ "$free_gb" -ge 15 ]; then
		warn "${free_gb} GB free: the images need about 5 GB, and databases and local backups grow from there"
	else
		fail "${free_gb} GB free: at least 15 GB is needed to build and start, far more to hold data"
	fi
fi
if have timedatectl; then
	case "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" in
	yes) pass "clock is synchronised" ;;
	no) warn "clock isn't synchronised (timedatectl set-ntp true): certificates and sign-in codes depend on correct time" ;;
	esac
fi

# --- Docker ----------------------------------------------------------------
section "Docker"
if ! have docker; then
	fail "docker isn't installed: curl -fsSL https://get.docker.com | sudo sh"
else
	if docker info >/dev/null 2>&1; then
		pass "docker is running and you can use it ($(docker version --format '{{.Server.Version}}' 2>/dev/null))"
	elif sudo -n docker info >/dev/null 2>&1; then
		warn "docker needs sudo for this user: sudo usermod -aG docker \"\$USER\", then log out and back in"
	else
		fail "can't talk to the docker daemon (is it running? is your user in the docker group?)"
	fi
	if docker compose version >/dev/null 2>&1; then
		pass "docker compose $(docker compose version --short 2>/dev/null)"
	else
		fail "Docker Compose v2 is missing (the 'docker compose' command): install docker-compose-plugin"
	fi
fi
if have git; then pass "git"; else warn "git isn't installed (needed to fetch a release: apt install git)"; fi
if have curl; then pass "curl"; else warn "curl isn't installed; some checks below are skipped (apt install curl)"; fi

# --- Names -----------------------------------------------------------------
section "DNS"
public_ip=
if have curl; then
	public_ip=$(curl -fsS --max-time 6 https://api.ipify.org 2>/dev/null || curl -fsS --max-time 6 https://ifconfig.me 2>/dev/null || true)
fi
if [ -n "$public_ip" ]; then
	pass "this server's public address is $public_ip"
else
	warn "couldn't find this server's public address (no outbound internet, or curl missing); DNS can't be compared"
fi

# resolve prints the A records of a name, one per line.
resolve() {
	if have dig; then
		dig +short +time=3 +tries=1 A "$1" 2>/dev/null | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'
	elif have getent; then
		getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u
	elif have host; then
		host -t A "$1" 2>/dev/null | awk '/has address/ {print $4}'
	fi
}
if ! have dig && ! have getent && ! have host; then
	warn "no dig, getent or host: can't check DNS (apt install dnsutils)"
else
	for name in "$ui" "$db"; do
		ips=$(resolve "$name" | sort -u | tr '\n' ' ')
		if [ -z "$ips" ]; then
			fail "$name doesn't resolve yet: add an A record to ${public_ip:-the address of this server} and wait for it to propagate"
		elif [ -z "$public_ip" ]; then
			warn "$name -> $ips (can't compare: public address unknown)"
		elif echo " $ips" | grep -q " $public_ip "; then
			pass "$name -> $public_ip"
		else
			fail "$name -> ${ips}but this server is $public_ip: Let's Encrypt must reach this machine at that name"
		fi
	done
	if [ "$ui" = "$db" ]; then
		fail "the UI and the database hostname are the same ($ui): use two names (the poolers get their own certificate)"
	fi
fi
for name in "$ui" "$db"; do
	if have dig && [ -n "$(dig +short +time=3 +tries=1 AAAA "$name" 2>/dev/null)" ]; then
		warn "$name has an AAAA (IPv6) record: make sure this server answers on that address too, or Let's Encrypt may fail"
	fi
done

# --- Ports -----------------------------------------------------------------
section "Ports"
listening() {
	if have ss; then
		ss -H -ltn 2>/dev/null | awk -v p=":$1\$" '$4 ~ p {f=1} END {exit !f}'
	elif have nc; then
		# Something accepts a connection on this port.
		nc -z -w 2 127.0.0.1 "$1" >/dev/null 2>&1
	else
		return 2
	fi
}
for p in 80 443 5432 6543; do
	listening "$p"
	case $? in
	0)
		who=
		have ss && who=$(ss -H -ltnp "sport = :$p" 2>/dev/null | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -1)
		fail "port $p is already in use${who:+ by $who}: stop it, or Xenibase's ${p} listener can't start"
		;;
	1) pass "port $p is free" ;;
	*) warn "can't tell whether port $p is free (no ss or nc)" ;;
	esac
done
if have ufw && ufw status 2>/dev/null | grep -q "Status: active"; then
	missing=
	for p in 80 443 5432 6543; do
		ufw status 2>/dev/null | grep -Eq "^$p(/tcp)? +ALLOW" || missing="$missing $p"
	done
	if [ -z "$missing" ]; then pass "ufw allows 80, 443, 5432 and 6543"; else warn "ufw is on and doesn't list${missing}: sudo ufw allow <port>/tcp for each (and allow OpenSSH first)"; fi
fi
note "From your own laptop, once Xenibase is running, these must connect (a cloud firewall can block them even when ufw doesn't):"
note "  nc -vz ${public_ip:-<server address>} 80   443   5432   6543"

# --- Outbound --------------------------------------------------------------
section "Outbound"
if have curl; then
	for url in https://acme-v02.api.letsencrypt.org/directory https://registry-1.docker.io/v2/ https://proxy.golang.org https://registry.npmjs.org; do
		code=$(curl -s -o /dev/null -m 8 -w '%{http_code}' "$url" 2>/dev/null)
		case "$code" in
		2* | 3* | 401 | 403) pass "reach ${url#https://} (HTTP $code)" ;;
		*) fail "can't reach ${url#https://}: the installer builds images and Caddy gets certificates over this" ;;
		esac
	done
	if [ -n "${XENIBASE_S3_ENDPOINT:-}" ]; then
		code=$(curl -s -o /dev/null -m 8 -w '%{http_code}' "$XENIBASE_S3_ENDPOINT" 2>/dev/null)
		case "$code" in
		000 | "") fail "can't reach the backup endpoint $XENIBASE_S3_ENDPOINT" ;;
		*) pass "reach the backup endpoint (HTTP $code)" ;;
		esac
	else
		note "set XENIBASE_S3_ENDPOINT=https://… to check your backup bucket is reachable from here"
	fi
fi
if [ -n "${XENIBASE_SMTP_HOST:-}" ]; then
	port=${XENIBASE_SMTP_PORT:-587}
	if have nc && nc -z -w 6 "$XENIBASE_SMTP_HOST" "$port" 2>/dev/null; then
		pass "reach $XENIBASE_SMTP_HOST:$port"
	elif have nc; then
		fail "can't reach $XENIBASE_SMTP_HOST:$port: many hosts block outbound mail ports (25, sometimes 465/587); use your provider's submission port, or ask them to unblock it"
	else
		warn "nc isn't installed: can't test $XENIBASE_SMTP_HOST:$port"
	fi
else
	note "set XENIBASE_SMTP_HOST (and XENIBASE_SMTP_PORT) to check your mail server is reachable from here"
fi

# --- A previous attempt ----------------------------------------------------
section "This directory"
if [ -f .env ]; then
	warn ".env already exists here: install.sh keeps it (and its secrets). Remove it only for a clean first install"
fi
if have docker && docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^xenibase\|compose-xenibase'; then
	warn "Xenibase containers are already running on this host"
fi
[ -f install.sh ] || warn "install.sh isn't in this directory: run this from deploy/compose in a Xenibase checkout"

printf '\n'
if [ "$fails" -gt 0 ]; then
	printf '%s problem(s) to fix before installing; %s warning(s).\n' "$fails" "$warns"
	exit 1
fi
printf 'Ready to install (%s warning(s)). Next: ./install.sh\n' "$warns"
