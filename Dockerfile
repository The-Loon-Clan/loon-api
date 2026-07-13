# syntax=docker/dockerfile:1

# go.mod replaces loon + loon-plugins with sibling checkouts. The Docker build
# pulls them in via BuildKit named build-contexts (see docker-compose.yml ->
# api.build.additional_contexts):
#   --build-context loon=../loon  --build-context loonplugins=../loon-plugins
# The replace paths (../loon, ../loon-plugins) resolve to /loon, /loon-plugins
# from the /app workdir.
FROM golang:1.26 AS build
WORKDIR /app
COPY --from=loon . /loon/
COPY --from=loonplugins . /loon-plugins/
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /loonapi .

# Static binary (CGO off) — the runtime image needs nothing but the binary + CA
# certs (for TLS NNTP if a download path ever reaches out).
FROM gcr.io/distroless/static-debian12
COPY --from=build /loonapi /loonapi
EXPOSE 8091
ENTRYPOINT ["/loonapi"]
