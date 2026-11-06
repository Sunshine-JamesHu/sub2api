.PHONY: build build-cgo build-no-playground build-windows-playground generate test test-unit test-integration test-e2e

VERSION ?= $(shell ./scripts/resolve-version.sh)
LDFLAGS ?= -s -w -X main.Version=$(VERSION)

build:
	CGO_ENABLED=1 go build -ldflags="$(LDFLAGS)" -trimpath -o bin/server ./cmd/server

build-cgo: build

build-no-playground:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -trimpath -o bin/server-no-playground ./cmd/server

build-windows-playground:
	pwsh -NoProfile -File ./scripts/build-windows.ps1

generate:
	go generate ./ent
	go generate ./cmd/server

test:
	go test ./...
	golangci-lint run ./...

test-unit:
	go test -tags=unit ./...

test-integration:
	go test -tags=integration ./...

test-e2e:
	./scripts/e2e-test.sh

test-e2e-local:
	go test -tags=e2e -v -timeout=300s ./internal/integration/...
