VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/morganseznec/hop/internal/cli.version=$(VERSION)
GOLANGCI_LINT_VERSION := v2.15.0

.PHONY: build install test cover lint vuln

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/hop ./cmd/hop

# Installs into $(go env GOBIN), or ~/go/bin by default.
install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/hop

test:
	go vet ./...
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
