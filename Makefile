.PHONY: all build check clean coverage fmt fmt-check generate generate-check test test-race vet

all: check

build:
	mkdir -p bin
	go build -trimpath -o bin/orch ./cmd/orch

generate:
	buf generate
	go generate ./...

generate-check:
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		buf format -d --exit-code; \
		buf lint; \
		buf generate --output "$$tmp"; \
		go run ./cmd/schema-gen -out "$$tmp/orch.schema.json"; \
		diff -ru gen "$$tmp/gen" || \
		(echo "Generated protobuf files are stale; run make generate" && exit 1); \
		cmp schema/orch.schema.json "$$tmp/orch.schema.json" || \
		(echo "Generated files are stale; run make generate" && exit 1)

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

fmt-check:
	@test -z "$$(gofmt -l $$(find cmd internal -name '*.go' -type f))" || \
		(echo "Go files need formatting; run make fmt" && exit 1)

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

coverage:
	go test -coverprofile=coverage.out ./...

check: generate-check fmt-check vet test test-race build

clean:
	rm -f bin/orch coverage.out
