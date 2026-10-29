#!/bin/sh
# Builds the instance image for one Postgres major and tags it twice:
# xenibase-postgres:<major>-walg3.0.9, the tag the agent's {major} template
# names, and xenibase-postgres:<major>.<minor>-walg3.0.9, the release the
# Dockerfile pins, so the release it replaces stays tagged (and can be put
# back) after a minor upgrade.
#
#   deploy/images/postgres/build.sh 18 [docker build flags...]
set -eu
if [ $# -lt 1 ]; then
	echo "usage: $0 <major> [docker build flags...]" >&2
	exit 2
fi
major=$1
shift
dir=$(cd "$(dirname "$0")" && pwd)
walg=walg3.0.9
tag="xenibase-postgres:$major-$walg"
docker build "$@" --build-arg "PG_MAJOR=$major" -t "$tag" "$dir"
release=$(docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$tag" |
	sed -n 's/^PG_VERSION=\([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')
if [ -z "$release" ]; then
	echo "$tag: no PG_VERSION in the image" >&2
	exit 1
fi
docker tag "$tag" "xenibase-postgres:$release-$walg"
echo "$tag is xenibase-postgres:$release-$walg"
