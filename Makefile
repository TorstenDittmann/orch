MISE ?= mise
GO := GOTOOLCHAIN=local $(MISE) exec -- go
GOFMT := $(MISE) exec -- gofmt
BUF := $(MISE) exec -- buf

.PHONY: all build check clean containerd-e2e coverage fmt fmt-check generate generate-check test test-race vet

all: check

build:
	mkdir -p bin
	$(GO) build -trimpath -o bin/orch ./cmd/orch

generate:
	$(BUF) generate
	$(GO) generate ./...

generate-check:
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		$(BUF) format -d --exit-code; \
		$(BUF) lint; \
		$(BUF) generate --output "$$tmp"; \
		$(GO) run ./cmd/schema-gen -out "$$tmp/orch.schema.json"; \
		diff -ru gen "$$tmp/gen" || \
		(echo "Generated protobuf files are stale; run make generate" && exit 1); \
		cmp schema/orch.schema.json "$$tmp/orch.schema.json" || \
		(echo "Generated files are stale; run make generate" && exit 1)

fmt:
	$(GOFMT) -w $$(find cmd internal test -name '*.go' -type f)

fmt-check:
	@test -z "$$($(GOFMT) -l $$(find cmd internal test -name '*.go' -type f))" || \
		(echo "Go files need formatting; run make fmt" && exit 1)

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...

check: generate-check fmt-check vet test test-race build

containerd-e2e:
	./scripts/containerd-e2e.sh

clean:
	rm -f bin/orch coverage.out
