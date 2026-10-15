MISE ?= mise
GO := GOTOOLCHAIN=local $(MISE) exec -- go
GOFMT := $(MISE) exec -- gofmt
BUF := $(MISE) exec -- buf
PKL := $(MISE) exec -- pkl

VERSION ?= 0.1.0-dev
BUILD_ID ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo development)
LDFLAGS := -s -w -X change.codes/torsten/better-swarm/internal/buildinfo.Version=$(VERSION) -X change.codes/torsten/better-swarm/internal/buildinfo.BuildID=$(BUILD_ID)

.PHONY: all build check clean containerd-e2e release coverage fmt fmt-check generate generate-check network-e2e pkl-test test test-race vet web web-check

all: check

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/orch ./cmd/orch

# Release binaries are static Linux amd64 builds with a SHA-256 checksum file.
release:
	rm -rf dist && mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/orch-linux-amd64 ./cmd/orch
	cd dist && sha256sum orch-linux-amd64 > SHA256SUMS

generate:
	$(BUF) generate

generate-check:
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		$(BUF) format -d --exit-code; \
		$(BUF) lint; \
		$(BUF) generate --output "$$tmp"; \
		diff -ru gen "$$tmp/gen" || \
		(echo "Generated protobuf files are stale; run make generate" && exit 1)

fmt:
	$(GOFMT) -w $$(find cmd internal packaging pkl test -name '*.go' -type f)
	$(PKL) format -w pkl examples

fmt-check:
	@test -z "$$($(GOFMT) -l $$(find cmd internal packaging pkl test -name '*.go' -type f))" || \
		(echo "Go files need formatting; run make fmt" && exit 1)
	$(PKL) format --diff-name-only pkl examples

pkl-test:
	$(PKL) test --no-project --root-dir . pkl/tests/*.pkl

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

BUN := $(MISE) exec -- bun
WEB_UI := internal/web/ui

# The orch web dashboard is built into internal/web/ui/dist and embedded in the binary.
web:
	cd $(WEB_UI) && $(BUN) install --frozen-lockfile && $(BUN) run check && $(BUN) run build

web-check: web
	@git diff --exit-code -- $(WEB_UI)/dist || \
		(echo "Embedded dashboard is stale; run make web and commit internal/web/ui/dist" && exit 1)
	@test -z "$$(git status --porcelain -- $(WEB_UI)/dist)" || \
		(echo "Embedded dashboard has uncommitted files; run make web and commit internal/web/ui/dist" && exit 1)

coverage:
	$(GO) test -coverprofile=coverage.out ./...

check: generate-check fmt-check pkl-test vet test test-race build

containerd-e2e:
	./scripts/containerd-e2e.sh

network-e2e:
	./scripts/network-e2e.sh

clean:
	rm -rf bin/orch coverage.out dist
