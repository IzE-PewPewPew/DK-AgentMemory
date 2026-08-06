# syntax=docker/dockerfile:1

# Build stage.
FROM golang:1.25-alpine AS build

ARG VERSION=dev
ARG COMMIT=none
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/IzE-PewPewPew/DK-AgentMemory/internal/version.Version=${VERSION} \
        -X github.com/IzE-PewPewPew/DK-AgentMemory/internal/version.Commit=${COMMIT} \
        -X github.com/IzE-PewPewPew/DK-AgentMemory/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/dkm ./cmd/dkm

# Runtime stage.
#
# Alpine rather than scratch, for one reason: when something is wrong at 2am,
# being able to `docker exec` a shell into the container and run `nc -z
# postgres 5432` is worth the ~8 MB. A distroless image saves space and costs
# an hour every time it is needed.
FROM alpine:3.21

# No package manager call here, deliberately.
#
# The runtime stage needs exactly one thing from outside the image: a CA bundle
# for outbound HTTPS to the embedding and LLM providers. `apk add
# ca-certificates` fetches it over HTTPS — which fails on any host behind a
# TLS-inspecting proxy, and fails while installing the certificates that would
# have made it work. Copying the bundle from the build stage removes the network
# from this stage entirely, so the image builds the same on a laptop behind a
# corporate proxy as it does in CI.
#
# Timezone data is compiled into the binary instead (see cmd/dkm/main.go), so
# there is no tzdata package to install either.
#
# adduser is busybox, already present in the base image.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN adduser -D -H -u 10001 dkm

COPY --from=build /out/dkm /usr/local/bin/dkm
COPY config.example.yaml /etc/dkm/config.example.yaml

USER dkm
EXPOSE 8090

# livez is unauthenticated precisely so a health check does not need a
# credential baked into the orchestrator.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8090/v1/livez || exit 1

ENTRYPOINT ["dkm"]
CMD ["serve", "--config", "/etc/dkm/config.yaml"]
