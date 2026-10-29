# PostgreSQL with WAL-G, the image for dedicated instances (spec §3.2,
# §6.4) and agent-run shared clusters, one per supported major (V3 §2.4).
#   deploy/images/postgres/build.sh 18    (and 17)
# which tags xenibase-postgres:18-walg3.0.9, the tag the agent uses, and
# xenibase-postgres:18.6-walg3.0.9, so the previous release stays tagged
# after a rebuild onto a newer one.
#
# Each major is built from a pinned minor release (PG17_RELEASE,
# PG18_RELEASE): the official postgres:18 tag moves with every release, so
# images built from it on different nodes or days would run different ones.
# Raise them for a minor upgrade (docs/upgrade.md).
#
# WAL-G is built from a pinned commit through the Go module proxy, without
# cgo: no system packages, and encryption via OpenPGP (pure Go).
#
# Patroni (HA instances, V3 §2.2) runs on the Python of the official
# python image of the same Debian release, copied in with a virtualenv of
# pinned wheels from PyPI: again no system packages.
#
# Two packages are installed from the Debian and PGDG repositories the official
# image already uses: hypopg (hypothetical indexes for query insights' estimates,
# V3 §8), and ca-certificates, because the official image ships no CA roots and
# WAL-G reaches S3 storage over HTTPS.
ARG REGISTRY=
ARG PG_MAJOR=18
ARG PG17_RELEASE=17.11
ARG PG18_RELEASE=18.6

FROM ${REGISTRY}golang:1.25-alpine AS walg
# wal-g v3.0.9
ARG WALG_COMMIT=3e493188db28335fb93a45bd680c6ff8fc3028c1
ENV CGO_ENABLED=0 GOEXPERIMENT=jsonv2 GOTOOLCHAIN=go1.25.13
RUN --mount=type=secret,id=ca_bundle,required=false \
    if [ -f /run/secrets/ca_bundle ]; then export SSL_CERT_FILE=/run/secrets/ca_bundle; fi; \
    go install -trimpath -ldflags "-s -w" github.com/wal-g/wal-g/main/pg@${WALG_COMMIT} \
 && install -m 755 /go/bin/pg /out-wal-g

FROM ${REGISTRY}python:3.13-slim-trixie AS patroni
ARG PATRONI_VERSION=4.1.5
RUN --mount=type=secret,id=ca_bundle,required=false \
    if [ -f /run/secrets/ca_bundle ]; then export PIP_CERT=/run/secrets/ca_bundle; fi; \
    python -m venv /opt/patroni \
 && /opt/patroni/bin/pip install --no-cache-dir --disable-pip-version-check \
      "patroni[etcd3,psycopg3]==${PATRONI_VERSION}" \
 && /opt/patroni/bin/patroni --version

# The pinned release of each major; only the one PG_MAJOR names is built.
FROM ${REGISTRY}postgres:${PG17_RELEASE}-trixie AS pg17
FROM ${REGISTRY}postgres:${PG18_RELEASE}-trixie AS pg18

FROM pg${PG_MAJOR}
ARG PG_MAJOR
RUN test "${PG_MAJOR}" = "$(echo "$PG_VERSION" | cut -d. -f1)" \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates "postgresql-${PG_MAJOR}-hypopg" \
 && rm -rf /var/lib/apt/lists/* \
 && test -s /etc/ssl/certs/ca-certificates.crt \
 && test -f "/usr/share/postgresql/${PG_MAJOR}/extension/hypopg.control"
COPY --from=walg /out-wal-g /usr/local/bin/wal-g
COPY --from=patroni /usr/local/lib/python3.13 /usr/local/lib/python3.13
COPY --from=patroni /usr/local/lib/libpython3.13.so.1.0 /usr/local/lib/
COPY --from=patroni /usr/local/bin/python3.13 /usr/local/bin/
COPY --from=patroni /opt/patroni /opt/patroni
RUN ln -s python3.13 /usr/local/bin/python3 && ln -s python3 /usr/local/bin/python \
 && ldconfig && /opt/patroni/bin/patroni --version
COPY --chmod=755 xenibase-patroni.sh /usr/local/bin/xenibase-patroni
COPY --chmod=755 xenibase-hba.sh /docker-entrypoint-initdb.d/10-xenibase-hba.sh
LABEL org.opencontainers.image.title="xenibase-postgres" \
      org.opencontainers.image.description="PostgreSQL ${PG_MAJOR} with WAL-G v3.0.9 and hypopg for Xenibase instances"
