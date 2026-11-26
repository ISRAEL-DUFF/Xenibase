#!/bin/sh
# pooler-host: runs one Xenibase edge pooler host (V3 §2.1): both PgBouncers
# and xenibase-agent in pooler mode. keepalived runs in its own container and
# reads the configuration this script renders. If any process here exits,
# the rest are stopped and the container exits: keepalived's check then
# fails, the other pooler host takes the floating IP, and the restart policy
# brings this one back.
#
# Configuration (environment):
#   XENIBASE_AGENT_SERVER, XENIBASE_AGENT_TOKEN   registration (first start)
#   XENIBASE_AGENT_ADVERTISE                    host:port xenibase-server dials
#   XENIBASE_EDGE_SELF_IP, XENIBASE_EDGE_PEER_IP  the two hosts' private IPs
#   XENIBASE_EDGE_VRRP_PASSWORD                 shared, at most 8 characters
#   XENIBASE_EDGE_INTERFACE (eth0), XENIBASE_EDGE_VIP, XENIBASE_EDGE_ROUTER_ID (51),
#   XENIBASE_EDGE_PRIORITY (100)
#   XENIBASE_AGENT_SERVER_ID                    this machine's Hetzner ID, the floating IP's target
#   XENIBASE_AGENT_ASSIGN_FLOATING_IP, XENIBASE_AGENT_HETZNER_TOKEN,
#   XENIBASE_AGENT_FLOATING_IP_ID, XENIBASE_AGENT_HETZNER_API   only to assign the
#   floating IP from this host too (by default xenibase-server does)
#   XENIBASE_POOLER_SERVER_TLS_SSLMODE (prefer), XENIBASE_POOLER_SERVER_TLS_CA_FILE
#                                               TLS to the backends (verify-full needs the CA)
# Without XENIBASE_EDGE_SELF_IP no keepalived configuration is written.
set -eu

dir=${XENIBASE_AGENT_POOLER_DIR:-/etc/pgbouncer/xenibase}
edge=${XENIBASE_EDGE_KEEPALIVED_DIR:-/etc/xenibase-edge}
run=/run/xenibase

# Placeholder routes, users and certificate, so the PgBouncers start before
# xenibase-server's first push (the host is not ready until it arrives).
xenibase-agent pooler-seed --dir "$dir"
if [ -n "${XENIBASE_EDGE_SELF_IP:-}" ]; then
  xenibase-agent keepalived-config > "$edge/keepalived.conf.tmp"
  mv "$edge/keepalived.conf.tmp" "$edge/keepalived.conf"
fi

# The PgBouncers run on copies with this host's backend TLS settings.
xenibase-agent pooler-ini --in /etc/pgbouncer/session.ini --out "$run/session.ini"
xenibase-agent pooler-ini --in /etc/pgbouncer/transaction.ini --out "$run/transaction.ini"

pgbouncer "$run/session.ini" &
echo $! > "$run/session.pid"
pgbouncer "$run/transaction.ini" &
echo $! > "$run/transaction.pid"
xenibase-agent run &
agent=$!

pids="$(cat "$run/session.pid") $(cat "$run/transaction.pid") $agent"
stop() {
  # The agent first: keepalived's check fails at once and it gives up the
  # address while the PgBouncers drain.
  kill "$agent" 2>/dev/null || true
  sleep 1
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
}
trap 'stop; exit 0' TERM INT

while :; do
  for p in $pids; do
    if ! kill -0 "$p" 2>/dev/null; then
      echo "pooler-host: process $p exited; stopping the pooler host" >&2
      stop
      exit 1
    fi
  done
  sleep 1
done
