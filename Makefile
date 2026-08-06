BINARY   := dkm
PKG      := github.com/IzE-PewPewPew/DK-AgentMemory
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

# CGO is off everywhere. The whole point of shipping Go is a binary that runs on
# a machine with no toolchain, and a cgo dependency quietly takes that away.
export CGO_ENABLED := 0

.PHONY: all build install test test-integration lint vet fmt tidy clean release-snapshot cross docker dev dev-down help

all: build

## build: compile ./bin/dkm for the host platform
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/dkm

## install: build and place dkm on GOPATH/bin
install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/dkm

## test: unit tests. Store and API tests need a database and skip without one.
test:
	go test ./... -count=1

## test-integration: everything, including tests that need Postgres.
## Set DKM_TEST_DATABASE_URL to a database you do not mind having truncated.
test-integration:
	@test -n "$(DKM_TEST_DATABASE_URL)" || { \
		echo "DKM_TEST_DATABASE_URL is not set."; \
		echo "  docker run -d --name dkm-test -e POSTGRES_PASSWORD=test -p 5433:5432 pgvector/pgvector:pg16"; \
		echo "  export DKM_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:5433/postgres?sslmode=disable'"; \
		exit 1; }
	go test ./... -count=1 -tags=integration

## dev: start a throwaway Postgres with pgvector for integration tests
dev:
	@docker rm -f dkm-dev >/dev/null 2>&1 || true
	docker run -d --name dkm-dev \
		-e POSTGRES_PASSWORD=test \
		-p 5433:5432 \
		pgvector/pgvector:pg16 >/dev/null
	@echo "Waiting for Postgres..."
	@until docker exec dkm-dev pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	@echo ""
	@echo "  export DKM_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:5433/postgres?sslmode=disable'"
	@echo "  make test-integration"
	@echo ""
	@echo "Stop it with: make dev-down"

## dev-down: remove the throwaway database
dev-down:
	@docker rm -f dkm-dev >/dev/null 2>&1 || true

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

## lint: vet plus a gofmt check that fails rather than rewrites
lint: vet
	@test -z "$$(gofmt -l . | grep -v '^vendor/')" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

## cross: build every released platform, to catch OS-specific compile errors
cross:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/$(BINARY)_$${os}_$${arch}$$ext ./cmd/dkm || exit 1; \
	done

release-snapshot:
	goreleaser release --snapshot --clean

docker:
	docker build -t dkm:$(VERSION) -f deploy/Dockerfile .

clean:
	rm -rf bin dist

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
