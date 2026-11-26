# A Xenibase edge pooler host (V3 §2.1): xenibase-agent in pooler mode and the
# session and transaction PgBouncers, in one container. keepalived runs
# beside it (keepalived.Dockerfile), the only part that needs NET_ADMIN; this
# container renders its configuration into the shared /etc/xenibase-edge.
# Production runs both with host networking on each of the two pooler hosts
# (compose.yaml here); TestPoolerHostFailover runs two pairs on the dev
# Docker network.
#
#   docker build -f deploy/pooler-host/Dockerfile -t xenibase-pooler-host .
#
# Behind a TLS-intercepting proxy, pass its CA as a build secret
# (--secret id=ca_bundle,src=ca.pem); base images can come from a mirror
# (--build-arg REGISTRY=mirror.gcr.io/library/).

ARG REGISTRY=
FROM ${REGISTRY}golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=ca_bundle,required=false \
    if [ -f /run/secrets/ca_bundle ]; then export SSL_CERT_FILE=/run/secrets/ca_bundle; fi; \
    go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/israel-duff/xenibase/internal/version.Version=${VERSION} -X github.com/israel-duff/xenibase/internal/version.Commit=${COMMIT}" \
      -o /out/xenibase-agent ./cmd/agent

FROM edoburu/pgbouncer:v1.26.1-p0
# Everything runs as the image's postgres user (uid 70): the agent writes
# the PgBouncer files and signals the PgBouncers, so they share a user.
USER root
COPY --from=build /out/xenibase-agent /usr/local/bin/xenibase-agent
COPY deploy/pgbouncer/session.ini /etc/pgbouncer/session.ini
COPY deploy/pgbouncer/transaction.ini /etc/pgbouncer/transaction.ini
COPY deploy/pooler-host/entrypoint.sh /usr/local/bin/pooler-host
RUN chmod 755 /usr/local/bin/pooler-host \
 && install -d -o 70 -g 70 -m 700 /var/lib/xenibase-agent \
 && install -d -o 70 -g 70 -m 750 /etc/pgbouncer/xenibase \
 && install -d -o 70 -g 70 -m 755 /run/xenibase /etc/xenibase-edge
USER 70:70
ENV XENIBASE_AGENT_STATE_DIR=/var/lib/xenibase-agent \
    XENIBASE_AGENT_POOLER_DIR=/etc/pgbouncer/xenibase \
    XENIBASE_AGENT_POOLER_PIDFILES=/run/xenibase/session.pid,/run/xenibase/transaction.pid \
    XENIBASE_EDGE_KEEPALIVED_DIR=/etc/xenibase-edge \
    XENIBASE_AGENT_LOG_FORMAT=json
VOLUME ["/var/lib/xenibase-agent", "/etc/xenibase-edge"]
EXPOSE 5432 6543 7070
ENTRYPOINT ["/usr/local/bin/pooler-host"]
