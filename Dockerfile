# xenibase-server (with the web UI embedded) and, as the agent target,
# xenibase-agent.
#   docker build -t xenibase .
#
# Behind a TLS-intercepting proxy, pass its CA as a build secret:
#   docker build --secret id=ca_bundle,src=/path/to/ca.pem -t xenibase .
# Base images can come from a mirror, e.g.
#   --build-arg REGISTRY=mirror.gcr.io/library/

ARG REGISTRY=
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12

FROM ${REGISTRY}node:22-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=secret,id=ca_bundle,required=false \
    if [ -f /run/secrets/ca_bundle ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/ca_bundle; fi; \
    npm ci
COPY web/ ./
RUN npm run build

FROM ${REGISTRY}golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=ca_bundle,required=false \
    if [ -f /run/secrets/ca_bundle ]; then export SSL_CERT_FILE=/run/secrets/ca_bundle; fi; \
    go mod download
COPY . .
COPY --from=ui /src/web/dist/ ./web/dist/
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/israel-duff/xenibase/internal/version.Version=${VERSION} -X github.com/israel-duff/xenibase/internal/version.Commit=${COMMIT} -X github.com/israel-duff/xenibase/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -tags nodynamic \
      -o /out/usr/local/bin/xenibase-server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/israel-duff/xenibase/internal/version.Version=${VERSION} -X github.com/israel-duff/xenibase/internal/version.Commit=${COMMIT} -X github.com/israel-duff/xenibase/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/usr/local/bin/xenibase-agent ./cmd/agent \
 && /out/usr/local/bin/xenibase-server -require-ui \
 && mkdir -p /out/var/lib/xenibase/pooler

# xenibase-agent runs next to Postgres and needs its client tools (spec §3.2):
#   docker build --target agent -t xenibase-agent .
FROM ${REGISTRY}postgres:18 AS agent
# The official image ships no CA roots, and the agent uploads backups to S3
# storage over HTTPS: without them every upload fails with "certificate signed by
# unknown authority".
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && test -s /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/usr/local/bin/xenibase-agent /usr/local/bin/xenibase-agent
RUN install -d -o postgres -g postgres -m 700 /var/lib/xenibase-agent
USER postgres
ENV XENIBASE_AGENT_STATE_DIR=/var/lib/xenibase-agent \
    XENIBASE_AGENT_DISK_PATH=/var/lib/xenibase-agent
EXPOSE 7070
ENTRYPOINT ["/usr/local/bin/xenibase-agent"]
CMD ["run"]

FROM ${RUNTIME_IMAGE}
# uid 70 matches the PgBouncer image, so the poolers can read the 0640
# config, auth file, and TLS key xenibase-server writes.
COPY --from=build --chown=70:70 /out/ /
USER 70:70
ENV XENIBASE_LISTEN_ADDR=0.0.0.0:8080 \
    XENIBASE_DATA_DIR=/var/lib/xenibase \
    XENIBASE_POOLER_CONFIG_DIR=/var/lib/xenibase/pooler
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/xenibase-server"]
