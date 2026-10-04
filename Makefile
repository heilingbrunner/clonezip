VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Prefer tools installed via `go install` over stale version-manager shims.
# GOBIN may point into a version-specific Go install (e.g. mise), so also
# include GOPATH/bin where tools survive Go upgrades.
GO_TOOL_DIRS := $(strip $(shell go env GOBIN) $(shell go env GOPATH)/bin)
export PATH := $(subst $() ,:,$(GO_TOOL_DIRS)):$(PATH)

.PHONY: all tidy verify generate fmt lint vet winres build

all: tidy verify generate fmt lint vet winres build

tidy:
	go mod tidy

verify:
	go mod verify

generate:
	go generate ./...

fmt:
	go fmt ./...

lint:
	golangci-lint run --fix

vet:
	go vet ./...

winres:
	go-winres make --arch amd64 \
		--product-version "$(VERSION)" \
		--file-version "$(VERSION)" \
		--in winres/winres.json

build:
	go build -ldflags "-X main.version=$(VERSION)" ./...
