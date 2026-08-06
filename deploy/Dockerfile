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

RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 dkm

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
