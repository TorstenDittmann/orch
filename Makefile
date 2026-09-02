.PHONY: all build check clean coverage fmt fmt-check generate test test-race vet

all: check

build:
	mkdir -p bin
	go build -trimpath -o bin/orch ./cmd/orch

generate:
	go generate ./...

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

check: generate fmt-check vet test test-race build
	git diff --exit-code -- schema/orch.schema.json

clean:
	rm -f bin/orch coverage.out
