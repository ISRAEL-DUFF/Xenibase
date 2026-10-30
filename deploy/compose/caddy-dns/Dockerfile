# Caddy with a DNS provider plugin, for the wildcard certificate of backend
# services' API domain (*.api.example.com), which only a DNS challenge can get.
# Built by enable-services.sh with the plugin for the chosen provider; the
# stock caddy image does the rest of Xenibase's TLS and stays the default.
# The version is pinned, and the same as compose.yaml's default image and
# test/acmedns's Caddy module.
ARG CADDY_VERSION=2.10.0
FROM caddy:${CADDY_VERSION}-builder AS build
ARG CADDY_VERSION
ARG DNS_PLUGIN=github.com/caddy-dns/cloudflare@v0.2.4
RUN xcaddy build v${CADDY_VERSION} --with ${DNS_PLUGIN}

FROM caddy:${CADDY_VERSION}
COPY --from=build /usr/bin/caddy /usr/bin/caddy
