#!/bin/sh
# The only thing a deploy key may run on the server: an upgrade, or a status
# report. Put it in the deploy user's ~/.ssh/authorized_keys as a forced
# command (docs/deploying.md), so a stolen key can't open a shell:
#
#   restrict,command="/opt/xenibase/deploy/compose/deploy-ssh.sh" ssh-ed25519 AAAA… xenibase-deploy
#
# The caller asks with one of
#
#   ssh deploy@host upgrade v4.1.0 [--dry-run]
#   ssh deploy@host status       services and the running version
#   ssh deploy@host last         the result and log of the last upgrade
#
# sshd puts the request in SSH_ORIGINAL_COMMAND. An upgrade runs detached from
# the SSH session: if the connection drops half-way, the upgrade carries on (a
# killed upgrade would leave things in an unknown state), and `last` says how
# it ended.
set -eu
cd "$(dirname "$0")"
# shellcheck disable=SC2086 # the request is split into words on purpose
set -- ${SSH_ORIGINAL_COMMAND:-}
verb=${1:-}
dir=upgrade-backups
rc_file=$dir/last-deploy.rc
case "$verb" in
upgrade)
	tag=${2:-}
	opt=${3:-}
	printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.]+)?$' || { echo "deploy: not a release tag: '$tag'" >&2; exit 64; }
	case "$opt" in
	"") flags=--yes ;;
	--dry-run) flags=--dry-run ;;
	*) echo "deploy: unknown option '$opt'" >&2; exit 64 ;;
	esac
	# Owner only, like the dumps upgrade.sh keeps next to it.
	(umask 077 && mkdir -p "$dir")
	log=$dir/last-deploy.log
	rm -f "$rc_file"
	(umask 077 && : > "$log")
	# Detached: ignores the hangup of a dropped connection, writes to a file.
	RC_FILE=$rc_file nohup sh -c 'rc=0; ./upgrade.sh "$1" "$2" || rc=$?; echo "$rc" > "$RC_FILE"' deploy "$tag" "$flags" > "$log" 2>&1 < /dev/null &
	pid=$!
	tail -f "$log" &
	tail_pid=$!
	wait "$pid" 2>/dev/null || true
	sleep 1
	kill "$tail_pid" 2>/dev/null || true
	exit "$(cat "$rc_file" 2>/dev/null || echo 3)"
	;;
status)
	docker compose ps -a --format 'table {{.Service}}\t{{.State}}\t{{.Health}}'
	docker compose exec -T xenibase-server xenibase-server -version
	;;
last)
	if [ -f "$rc_file" ]; then echo "last upgrade: exit status $(cat "$rc_file") (0 upgraded, 2 rolled back, 3 rollback failed)"; else echo "last upgrade: no result recorded (still running, or never run)"; fi
	[ ! -f "$dir/last-deploy.log" ] || tail -n 40 "$dir/last-deploy.log"
	;;
*)
	echo "deploy: allowed: 'upgrade <tag> [--dry-run]', 'status' or 'last'" >&2
	exit 64
	;;
esac
